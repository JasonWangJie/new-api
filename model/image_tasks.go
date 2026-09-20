package model

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EraseExpiredAsyncImageTaskContent preserves accounting and idempotency
// tombstones while removing expired prompt, payload and reference content.
func EraseExpiredAsyncImageTaskContent(ctx context.Context) error {
	return DB.WithContext(ctx).Model(&AsyncImageTask{}).
		Where("expires_at > 0 AND expires_at <= ? AND status IN ? AND (prompt_summary <> '' OR request_cipher IS NOT NULL OR reference_urls <> '')", time.Now().Unix(), []string{ImageTaskSucceeded, ImageTaskFailed, ImageTaskExpired, ImageTaskExecutionUnknown}).
		Updates(map[string]any{"prompt_summary": "", "request_cipher": nil, "reference_urls": ""}).Error
}

// AcceptAsyncImageTask serializes admission on its Token and commits the task,
// idempotency binding, input references, event and queue command together.
func AcceptAsyncImageTask(ctx context.Context, task AsyncImageTask, keyHash string, inputIds []string) (AsyncImageTask, bool, error) {
	if keyHash != "" {
		var prior AsyncImageIdempotency
		err := DB.WithContext(ctx).Where("token_id = ? AND key_hash = ?", task.TokenId, keyHash).Take(&prior).Error
		if err == nil {
			if prior.RequestHash != task.RequestHash {
				return task, false, ErrImageConflict
			}
			err = DB.WithContext(ctx).Where("task_id = ?", prior.TaskId).Take(&task).Error
			return task, true, err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return task, false, err
		}
	}
	now := time.Now().Unix()
	task.Status = ImageTaskQueued
	task.BillingStatus = "pending"
	task.Version = 1
	task.CreatedAt = now
	task.UpdatedAt = now
	task.NextAttemptAt = now
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", task.TokenId, task.UserId).Take(&token).Error; err != nil {
			return err
		}
		if keyHash != "" {
			if err := tx.Create(&AsyncImageIdempotency{TokenId: task.TokenId, KeyHash: keyHash, RequestHash: task.RequestHash, TaskId: task.TaskId}).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&task).Error; err != nil {
			return err
		}
		for _, id := range inputIds {
			var input ImageInputObject
			if err := lockForUpdate(tx).Where("input_id = ? AND token_id = ? AND status = ? AND expires_at > ?", id, task.TokenId, "active", now).Take(&input).Error; err != nil {
				return err
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&ImageInputTaskReference{TaskId: task.TaskId, InputId: id}).Error; err != nil {
				return err
			}
		}
		event := AsyncImageEvent{TaskId: task.TaskId, EventKey: common.GetUUID(), EventType: "accepted", Status: task.Status, CreatedAt: now}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}
		return tx.Create(&ImageOutbox{EventKey: event.EventKey, Kind: "execute", AggregateId: task.TaskId, Status: "pending", CreatedAt: now, NextAttemptAt: now}).Error
	})
	if err != nil && keyHash != "" {
		var prior AsyncImageIdempotency
		if lookupErr := DB.WithContext(ctx).Where("token_id = ? AND key_hash = ?", task.TokenId, keyHash).Take(&prior).Error; lookupErr == nil {
			if prior.RequestHash != task.RequestHash {
				return task, false, ErrImageConflict
			}
			lookupErr = DB.WithContext(ctx).Where("task_id = ?", prior.TaskId).Take(&task).Error
			return task, true, lookupErr
		}
	}
	return task, false, err
}

func ClaimAsyncImageTask(ctx context.Context, id, lease string, seconds int) (AsyncImageTask, error) {
	var task AsyncImageTask
	if err := DB.WithContext(ctx).Where("task_id = ?", id).Take(&task).Error; err != nil {
		return task, err
	}
	now := time.Now().Unix()
	if task.Terminal() || (task.Status == ImageTaskInvoking && task.UpstreamTaskId == "") || task.NextAttemptAt <= 0 || task.NextAttemptAt > now || task.LeaseExpiresAt > now {
		return task, ErrImageConflict
	}
	updates := map[string]any{"lease_token": lease, "lease_expires_at": now + int64(seconds), "heartbeat_at": now}
	if task.Status == ImageTaskQueued {
		updates["status"] = ImageTaskInvoking
		if task.UpstreamTaskId == "" {
			updates["channel_id"] = 0
			updates["dispatched_at"] = 0
		}
		if task.StartedAt == 0 {
			updates["started_at"] = now
		}
	}
	if err := TransitionImageTask(ctx, task, updates, "claimed", "", ""); err != nil {
		return task, err
	}
	return task, DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", id, lease).Take(&task).Error
}

// ScheduleAsyncImagePoll releases the current execution lease after an
// upstream asynchronous job has been accepted and durably schedules its next
// status poll. The upstream channel and dispatch marker stay pinned so the
// task cannot be mistaken for a fresh generation request.
func ScheduleAsyncImagePoll(ctx context.Context, task AsyncImageTask, nextAttemptAt int64, eventType, message string) error {
	if task.UpstreamTaskId == "" || nextAttemptAt <= 0 {
		return ErrImageConflict
	}
	return TransitionImageTask(ctx, task, map[string]any{
		"status":           ImageTaskInvoking,
		"lease_token":      "",
		"lease_expires_at": 0,
		"next_attempt_at":  nextAttemptAt,
	}, eventType, message, "execute")
}

func HeartbeatAsyncImageTask(ctx context.Context, id, lease string, seconds int) error {
	now := time.Now().Unix()
	result := DB.WithContext(ctx).Model(&AsyncImageTask{}).Where("task_id = ? AND lease_token = ? AND lease_expires_at > ?", id, lease, now).Updates(map[string]any{"heartbeat_at": now, "lease_expires_at": now + int64(seconds)})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImageConflict
	}
	return nil
}

func MarkAsyncImageDispatched(ctx context.Context, task AsyncImageTask, channelId int) error {
	now := time.Now().Unix()
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&AsyncImageTask{}).Where("task_id = ? AND status = ? AND version = ? AND lease_token = ? AND lease_expires_at > ?", task.TaskId, ImageTaskInvoking, task.Version, task.LeaseToken, now).Updates(map[string]any{"channel_id": channelId, "dispatched_at": now, "progress": 20, "version": task.Version + 1, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrImageConflict
		}
		return tx.Create(&AsyncImageEvent{TaskId: task.TaskId, EventKey: common.GetUUID(), EventType: "upstream_dispatched", Status: ImageTaskInvoking, CreatedAt: now}).Error
	})
}

func RegisterAsyncImageResults(ctx context.Context, task AsyncImageTask, results []AsyncImageResult) error {
	if task.Status != ImageTaskUploading || len(results) != task.ImageCount || len(results) == 0 {
		return ErrImageConflict
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&AsyncImageTask{}).Where("task_id = ? AND version = ? AND status = ? AND lease_token = ? AND lease_expires_at > ?", task.TaskId, task.Version, ImageTaskUploading, task.LeaseToken, time.Now().Unix())
		updated := query.Updates(map[string]any{"status": ImageTaskBillingPending, "result_count": len(results), "version": task.Version + 1, "progress": 85, "updated_at": time.Now().Unix()})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrImageConflict
		}
		for i, result := range results {
			if result.TaskId != task.TaskId || result.ImageIndex != i {
				return ErrImageConflict
			}
			var object ImageStorageObject
			if err := lockForUpdate(tx).Where("object_id = ? AND status = ?", result.ObjectId, "active").Take(&object).Error; err != nil {
				return err
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&result).Error; err != nil {
				return err
			}
			var existing AsyncImageResult
			if err := tx.Where("task_id = ? AND image_index = ?", task.TaskId, i).Take(&existing).Error; err != nil {
				return err
			}
			if existing.ObjectId != result.ObjectId {
				return ErrImageConflict
			}
		}
		return tx.Create(&AsyncImageEvent{TaskId: task.TaskId, EventKey: common.GetUUID(), EventType: "storage_confirmed", Status: ImageTaskBillingPending, CreatedAt: time.Now().Unix()}).Error
	})
}

func CompleteAsyncImageTask(ctx context.Context, task AsyncImageTask, autoArchive bool) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var bill AsyncImageBill
		if err := tx.Where("task_id = ? AND status = ?", task.TaskId, "applied").Take(&bill).Error; err != nil {
			return err
		}
		if bill.LogStatus != "done" && bill.LogStatus != "disabled" {
			return ErrImageConflict
		}
		billingStatus := "succeeded"
		if bill.Quota == 0 {
			billingStatus = "not_billable"
		}
		now := time.Now().Unix()
		updated := tx.Model(&AsyncImageTask{}).Where("task_id = ? AND version = ? AND status = ? AND lease_token = ? AND lease_expires_at > ? AND result_count = image_count AND result_count > 0", task.TaskId, task.Version, ImageTaskBillingPending, task.LeaseToken, now).Updates(map[string]any{"status": ImageTaskSucceeded, "billing_status": billingStatus, "request_cipher": nil, "lease_token": "", "lease_expires_at": 0, "version": task.Version + 1, "progress": 100, "finished_at": now, "updated_at": now})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrImageConflict
		}
		if err := tx.Where("task_id = ?", task.TaskId).Delete(&AsyncImageStagingObject{}).Error; err != nil {
			return err
		}
		event := AsyncImageEvent{TaskId: task.TaskId, EventKey: common.GetUUID(), EventType: "completed", Status: ImageTaskSucceeded, CreatedAt: now}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}
		if autoArchive {
			return tx.Create(&ImageOutbox{EventKey: event.EventKey, Kind: "library_archive", AggregateId: task.TaskId, Status: "pending", CreatedAt: now, NextAttemptAt: now}).Error
		}
		return nil
	})
}

func ResumeAsyncImageTask(ctx context.Context, task AsyncImageTask) error {
	if task.Status != ImageTaskStorageFailed && task.Status != ImageTaskBillingFailed {
		return ErrImageConflict
	}
	var bill AsyncImageBill
	if err := DB.WithContext(ctx).Where("task_id = ?", task.TaskId).Take(&bill).Error; err != nil {
		return err
	}
	status := ImageTaskUpstreamSucceeded
	if task.ResultCount == task.ImageCount && task.ResultCount > 0 {
		status = ImageTaskBillingPending
	}
	return TransitionImageTask(ctx, task, map[string]any{"status": status, "error_code": "", "error_message": "", "public_error_code": 0, "storage_retry_count": 0, "billing_retry_count": 0, "next_attempt_at": time.Now().Unix(), "lease_token": "", "lease_expires_at": 0}, "resumed", "Post-processing recovery; upstream generation is not repeated", "postprocess")
}

func TerminateAsyncImageTask(ctx context.Context, task AsyncImageTask) error {
	if task.Terminal() {
		return ErrImageConflict
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Settlement locks the task before the bill. Keep the same lock order so
		// the terminal state reflects whether the financial transaction committed.
		var current AsyncImageTask
		if err := lockForUpdate(tx).Where("task_id = ?", task.TaskId).Take(&current).Error; err != nil {
			return err
		}
		if current.Terminal() || current.Version != task.Version || current.Status != task.Status {
			return ErrImageConflict
		}
		updates := map[string]any{"status": ImageTaskFailed, "request_cipher": nil, "lease_token": "", "lease_expires_at": 0, "next_attempt_at": 0, "error_code": "admin_terminated", "public_error_code": 608, "error_message": "Task terminated by administrator", "reconciliation_status": "not_charged", "finished_at": time.Now().Unix()}
		var bill AsyncImageBill
		if err := lockForUpdate(tx).Where("task_id = ?", task.TaskId).Take(&bill).Error; err == nil && bill.Status == "applied" {
			updates["reconciliation_status"] = "settled"
			updates["billing_status"] = "succeeded"
			if bill.Quota == 0 {
				updates["billing_status"] = "not_billable"
			}
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return transitionImageTaskTx(tx, current, updates, "terminated", "Administrator terminated task", "")
	})
}

func RecoverAsyncImageTasks(ctx context.Context, limit, timeout int) error {
	now := time.Now().Unix()
	var tasks []AsyncImageTask
	if err := DB.WithContext(ctx).Where("status = ? AND ((lease_token <> '' AND lease_expires_at <= ?) OR (started_at > 0 AND started_at <= ?))", ImageTaskInvoking, now, now-int64(timeout)).Order("id").Limit(limit).Find(&tasks).Error; err != nil {
		return err
	}
	for _, task := range tasks {
		updates := map[string]any{"status": ImageTaskQueued, "lease_token": "", "lease_expires_at": 0, "next_attempt_at": now}
		event, kind := "recovered", "execute"
		if task.UpstreamTaskId != "" {
			updates["status"] = ImageTaskInvoking
		} else if task.DispatchedAt > 0 {
			updates["status"] = ImageTaskExecutionUnknown
			updates["request_cipher"] = nil
			updates["public_error_code"] = 608
			updates["error_code"] = "execution_unknown"
			updates["error_message"] = "Upstream execution may have completed; manual reconciliation is required"
			updates["finished_at"] = now
			event, kind = "execution_unknown", ""
		}
		if err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			updates["version"] = task.Version + 1
			updates["updated_at"] = now
			changed := tx.Model(&AsyncImageTask{}).Where("task_id = ? AND version = ? AND status = ? AND lease_token = ? AND (lease_expires_at <= ? OR (started_at > 0 AND started_at <= ?))", task.TaskId, task.Version, ImageTaskInvoking, task.LeaseToken, now, now-int64(timeout)).Updates(updates)
			if changed.Error != nil {
				return changed.Error
			}
			if changed.RowsAffected != 1 {
				return ErrImageConflict
			}
			record := AsyncImageEvent{TaskId: task.TaskId, EventKey: common.GetUUID(), EventType: event, Status: updates["status"].(string), CreatedAt: now}
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
			if kind == "" {
				return nil
			}
			return tx.Create(&ImageOutbox{EventKey: record.EventKey, Kind: kind, AggregateId: task.TaskId, Status: "pending", CreatedAt: now, NextAttemptAt: now}).Error
		}); err != nil && !errors.Is(err, ErrImageConflict) {
			return err
		}
	}
	return nil
}
