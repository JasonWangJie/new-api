package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AsyncMediaEvent records confirmed lifecycle changes, never provider payloads,
// credentials, material URLs, or transient polling/lease activity.
type AsyncMediaEvent struct {
	Id                int    `json:"id"`
	TaskId            string `json:"-" gorm:"size:64;index"`
	EventKey          string `json:"-" gorm:"size:64;uniqueIndex"`
	EventType         string `json:"event_type" gorm:"size:64"`
	Status            string `json:"status" gorm:"size:32"`
	Message           string `json:"message" gorm:"type:text"`
	CreatedAt         int64  `json:"created_at" gorm:"type:bigint"`
	AttemptGeneration int64  `json:"-"`
	ChannelId         int    `json:"-"`
	KeyIndex          *int   `json:"-"`
	ReferenceMode     string `json:"-" gorm:"size:32"`
}

func ListAsyncMediaEvents(ctx context.Context, taskID string) ([]AsyncMediaEvent, error) {
	events := make([]AsyncMediaEvent, 0)
	err := DB.WithContext(ctx).Where("task_id = ?", taskID).Order("id").Find(&events).Error
	return events, err
}

// ChangeAsyncMediaJob preserves the caller's eligibility/ownership predicates.
// A matched locked row also distinguishes a MySQL no-op from a lost CAS.
func ChangeAsyncMediaJob(query *gorm.DB, updates map[string]any, eventType string) error {
	switch eventType {
	case "", "resume_requested", "terminated", "reconciliation":
	default:
		return ErrImageConflict
	}
	return query.Transaction(func(tx *gorm.DB) error {
		var before AsyncMediaJob
		if err := lockForUpdate(tx.Session(&gorm.Session{})).Take(&before).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrImageConflict
			}
			return err
		}
		if err := tx.Session(&gorm.Session{}).Where("id = ?", before.ID).Updates(updates).Error; err != nil {
			return err
		}
		var after AsyncMediaJob
		fresh := tx.Session(&gorm.Session{NewDB: true})
		if err := fresh.Where("id = ?", before.ID).Take(&after).Error; err != nil {
			return err
		}
		operationChanged := before.Status != after.Status || before.StorageStatus != after.StorageStatus || before.BillingStatus != after.BillingStatus ||
			before.ReferenceRecoveryStage != after.ReferenceRecoveryStage || before.DispatchedAt != after.DispatchedAt ||
			before.StorageRetries != after.StorageRetries || before.NextAttemptAt != after.NextAttemptAt
		if eventType != "" && operationChanged {
			if err := recordAsyncMediaEventTx(fresh, after, eventType, "", nil, ""); err != nil {
				return err
			}
		}
		return recordAsyncMediaTransitionTx(fresh, before, after, nil, "")
	})
}

// HasPendingSubmissionReconciliation preserves the evidence of a dispatched
// operation whose acceptance and reserved funds still require manual review.
func (job AsyncMediaJob) HasPendingSubmissionReconciliation() bool {
	return job.Status == "execution_unknown" && job.DispatchedAt > 0 && job.BillingStatus == "unknown"
}

// ExcludePendingSubmissionReconciliation filters before pagination so retained
// unknown operations cannot starve cleanup of financially completed jobs.
func ExcludePendingSubmissionReconciliation(query *gorm.DB) *gorm.DB {
	return query.Where("NOT (COALESCE(status, '') = ? AND COALESCE(dispatched_at, 0) > ? AND COALESCE(billing_status, '') = ?)", "execution_unknown", 0, "unknown")
}

func ExpireAsyncMediaJob(ctx context.Context, job AsyncMediaJob, now int64) error {
	if job.HasPendingSubmissionReconciliation() {
		return ErrImageConflict
	}
	// Keep output pointers until the caller confirms file deletion, so a failed
	// filesystem cleanup remains discoverable on the next sweep.
	changes := map[string]any{"storage_status": "expired", "request_cipher": nil, "channel_cipher": nil}
	if job.ReferenceRecoveryStage != "refund" {
		changes["status"] = gorm.Expr("CASE WHEN status IN ? THEN status ELSE ? END", []string{"failed", "succeeded"}, "expired")
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Match settlement's task-before-job lock order. Retention expiry must
		// not destroy credentials needed to reconcile an accepted operation.
		var task Task
		err := lockForUpdate(tx).Where("task_id = ? AND user_id = ? AND attempt_generation = ?", job.TaskId, job.UserId, job.AttemptGeneration).Take(&task).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && task.PrivateData.AsyncMedia && (task.PrivateData.VideoReconciliation != nil || task.PrivateData.UpstreamTaskID != "" && task.Quota > 0) && task.Status != TaskStatusSuccess && task.Status != TaskStatusFailure && job.ReferenceRecoveryStage != "refund" && job.BillingStatus != "refunded" && job.BillingStatus != "not_billable" {
			return ErrImageConflict
		}
		query := tx.Model(&AsyncMediaJob{}).
			Where("id = ? AND expires_at <= ? AND lease_expires_at <= ? AND attempt_generation = ? AND COALESCE(reference_recovery_stage, '') = ? AND billing_status = ?", job.ID, now, now, job.AttemptGeneration, job.ReferenceRecoveryStage, job.BillingStatus)
		query = ExcludePendingSubmissionReconciliation(query)
		return ChangeAsyncMediaJob(query, changes, "")
	})
}

// FailAgedAsyncMediaJob fences every prior worker before local timeout policy
// ends the operation. Unknown accounting remains held for reconciliation.
func FailAgedAsyncMediaJob(ctx context.Context, job AsyncMediaJob, now int64, automatic bool) (AsyncMediaJob, error) {
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task Task
		taskErr := lockForUpdate(tx).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&task).Error
		if taskErr != nil && !errors.Is(taskErr, gorm.ErrRecordNotFound) {
			return taskErr
		}
		var current AsyncMediaJob
		if err := lockForUpdate(tx).Where("id = ? AND task_id = ? AND user_id = ?", job.ID, job.TaskId, job.UserId).Take(&current).Error; err != nil {
			return err
		}
		if current.HasPendingSubmissionReconciliation() || current.Status == "failed" || current.Status == "succeeded" || current.CreatedAt <= 0 ||
			automatic && current.CreatedAt > now-AsyncMediaMaxTaskAgeSeconds || !automatic && current.Status != "expired" ||
			current.Status == "expired" && (taskErr == nil && task.Status == TaskStatusSuccess || current.BillingStatus == "settled") {
			return ErrImageConflict
		}
		if taskErr == nil && task.PrivateData.AsyncMedia && task.PrivateData.VideoReconciliation != nil && task.Status != TaskStatusSuccess && task.Status != TaskStatusFailure {
			return ErrImageConflict
		}
		if current.AttemptGeneration == math.MaxInt64 {
			return ErrTaskAttemptConflict
		}
		generation := current.AttemptGeneration + 1
		reason, event := AsyncMediaTaskAgeExceededReason, "age_limit_failed"
		if !automatic {
			reason, event = AsyncMediaExpiredTerminatedReason, "terminated"
		}
		billing, stage := current.BillingStatus, "age_limit_complete"
		dispatched := current.DispatchedAt > 0 || current.Status == "submitted" || current.ReferenceRecoveryStage == "dispatched" || current.ReferenceRecoveryStage == "waiting" ||
			task.PrivateData.UpstreamTaskID != "" || task.Status == TaskStatusSubmitted || task.Status == TaskStatusInProgress
		confirmedFailure := task.Status == TaskStatusFailure && task.PrivateData.VideoTerminalConfirmed
		if billing != "settled" && billing != "refunded" && billing != "not_billable" {
			billing, stage = "unknown", "age_limit_unknown"
			if taskErr == nil && task.AttemptGeneration == current.AttemptGeneration && task.ChannelId == current.ChannelId && task.PrivateData.TokenId == current.TokenId {
				switch {
				case task.Status == TaskStatusSuccess:
					billing, stage = "settled", "age_limit_complete"
				case task.Status == TaskStatusFailure && task.Quota == 0:
					billing, stage = "refunded", "age_limit_complete"
				case task.Status != TaskStatusUnknown && current.Status != "execution_unknown" && current.ReferenceRecoveryStage != "unknown" && current.BillingStatus == "reserved" && task.PrivateData.DurableBilling && task.PrivateData.SubmitAccountingRecorded && !slices.ContainsFunc(task.PrivateData.VideoAttempts, func(attempt VideoTaskAttempt) bool { return attempt.Status == TaskStatusUnknown }) && (confirmedFailure || !dispatched && task.Status != TaskStatusFailure):
					billing, stage = "reserved", "age_limit_refund"
				}
			} else if errors.Is(taskErr, gorm.ErrRecordNotFound) && current.DispatchedAt == 0 && current.Quota == 0 && current.BillingStatus == "pending" {
				billing, stage = "not_billable", "age_limit_complete"
			}
		}
		if taskErr == nil && task.AttemptGeneration == current.AttemptGeneration {
			task.AttemptGeneration = generation
			if task.Status != TaskStatusSuccess {
				task.Status, task.FailReason, task.Progress, task.FinishTime = TaskStatusFailure, reason, "100%", now
				if stage == "age_limit_refund" {
					task.Status = TaskStatusUnknown
				}
				task.PrivateData.VideoTerminalConfirmed = false
				task.PrivateData.ResultURL, task.PrivateData.PluginState, task.PrivateData.UpstreamAsyncResponse = "", nil, nil
				task.Data = nil
			}
			if err := tx.Model(&Task{}).Where("id = ? AND attempt_generation = ?", task.ID, current.AttemptGeneration).
				Updates(map[string]any{"attempt_generation": generation, "status": task.Status, "fail_reason": task.FailReason, "progress": task.Progress, "finish_time": task.FinishTime, "updated_at": now, "private_data": task.PrivateData, "data": task.Data}).Error; err != nil {
				return err
			}
		}
		updates := map[string]any{"status": "failed", "billing_status": billing, "reference_recovery_stage": stage, "attempt_generation": generation,
			"lease_token": "", "lease_expires_at": 0, "next_attempt_at": 0, "request_cipher": nil, "channel_cipher": nil, "error_message": reason}
		if billing == "refunded" || billing == "not_billable" {
			updates["quota"] = 0
		}
		if err := tx.Model(&AsyncMediaJob{}).Where("id = ?", current.ID).Updates(updates).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", current.ID).Take(&job).Error; err != nil {
			return err
		}
		if err := recordAsyncMediaEventTx(tx, job, event, "", nil, ""); err != nil {
			return err
		}
		return recordAsyncMediaTransitionTx(tx, current, job, nil, "")
	})
	return job, err
}

// CanTerminateVideoReconciliation checks whether an administrator can release
// a retained video reservation after explicitly checking the upstream task.
// ReconcileAgedAsyncMediaReservation repeats this check under row locks.
func (job AsyncMediaJob) CanTerminateVideoReconciliation(task Task, now int64) bool {
	if job.Status != "reconciling" || task.Status == TaskStatusSuccess || task.Status == TaskStatusFailure ||
		task.PrivateData.VideoReconciliation == nil || task.PrivateData.UpstreamTaskID == "" || job.LeaseExpiresAt > now {
		return false
	}
	if job.BillingStatus != "reserved" && job.BillingStatus != "unknown" ||
		job.TaskId != task.TaskID || job.UserId != task.UserId || job.ChannelId != task.ChannelId || job.TokenId != task.PrivateData.TokenId ||
		job.AttemptGeneration != task.AttemptGeneration || job.Quota != task.Quota || !task.VideoRecoveryQuotaValid() ||
		!task.PrivateData.AsyncMedia || !task.PrivateData.DurableBilling || !task.PrivateData.SubmitAccountingRecorded {
		return false
	}
	return task.PrivateData.ResultURL == "" && (task.PrivateData.MediaRetryFact == nil || !task.PrivateData.MediaRetryFact.HasOutput) &&
		job.OutputObjectKey == "" && job.ArtifactKeys == "" && !slices.Contains([]string{"ready", "saving", "succeeded", "upstream"}, job.StorageStatus)
}

// ReconcileAgedAsyncMediaReservation authorizes one refund only after the
// administrator explicitly verifies the unknown upstream operation. Initial
// and replacement submissions share the same durable accounting boundary.
func ReconcileAgedAsyncMediaReservation(ctx context.Context, job AsyncMediaJob) (AsyncMediaJob, error) {
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task Task
		if err := lockForUpdate(tx).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&task).Error; err != nil {
			return err
		}
		var current AsyncMediaJob
		if err := lockForUpdate(tx).Where("id = ?", job.ID).Take(&current).Error; err != nil {
			return err
		}
		policyStopped := current.ReferenceRecoveryStage == "policy_stopped_unknown"
		unknownExecution := current.Status == "execution_unknown" && (current.ReferenceRecoveryStage == "unknown" || current.ReferenceRecoveryStage == "manual")
		waitingReconciliation := current.Status == "reconciling"
		if waitingReconciliation {
			if !current.CanTerminateVideoReconciliation(task, time.Now().Unix()) {
				return ErrImageConflict
			}
		} else if unknownExecution {
			if task.Status != TaskStatusUnknown || current.LeaseExpiresAt > time.Now().Unix() {
				return ErrImageConflict
			}
		} else if current.Status != "failed" || current.ReferenceRecoveryStage != "age_limit_unknown" && !policyStopped || !policyStopped && current.ReferenceRetryCount != 1 || task.Status != TaskStatusFailure || !policyStopped && task.PrivateData.UpstreamTaskID != "" {
			return ErrImageConflict
		}
		if current.BillingStatus != "unknown" && !((unknownExecution || waitingReconciliation) && current.BillingStatus == "reserved") ||
			current.AttemptGeneration != job.AttemptGeneration || current.AttemptGeneration != task.AttemptGeneration || current.TaskId != job.TaskId || current.UserId != job.UserId || current.UserId != task.UserId || current.ChannelId != task.ChannelId || current.TokenId != task.PrivateData.TokenId ||
			!task.VideoRecoveryQuotaValid() || task.Quota != current.Quota || !task.PrivateData.AsyncMedia || !task.PrivateData.DurableBilling || !task.PrivateData.SubmitAccountingRecorded ||
			task.PrivateData.ResultURL != "" || task.PrivateData.MediaRetryFact != nil && task.PrivateData.MediaRetryFact.HasOutput || current.OutputObjectKey != "" || current.ArtifactKeys != "" {
			return ErrImageConflict
		}
		generation := task.AttemptGeneration
		if unknownExecution || waitingReconciliation {
			if generation == math.MaxInt64 {
				return ErrTaskAttemptConflict
			}
			generation++
		}
		if err := tx.Model(&Task{}).Where("id = ? AND status = ? AND attempt_generation = ?", task.ID, task.Status, task.AttemptGeneration).Updates(map[string]any{"status": TaskStatusUnknown, "attempt_generation": generation}).Error; err != nil {
			return err
		}
		changes := map[string]any{"status": "failed", "attempt_generation": generation, "billing_status": "reserved", "reference_recovery_stage": "age_limit_refund", "next_attempt_at": 0, "lease_token": "", "lease_expires_at": 0}
		if unknownExecution || waitingReconciliation {
			changes["error_message"] = job.ErrorMessage
		}
		if err := tx.Model(&AsyncMediaJob{}).Where("id = ?", current.ID).Updates(changes).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", current.ID).Take(&job).Error; err != nil {
			return err
		}
		if err := recordAsyncMediaEventTx(tx, job, "reconciliation", "", nil, ""); err != nil {
			return err
		}
		return recordAsyncMediaTransitionTx(tx, current, job, nil, "")
	})
	return job, err
}

func recordAsyncMediaEventTx(tx *gorm.DB, job AsyncMediaJob, eventType, discriminator string, keyIndex *int, referenceMode string) error {
	if eventType == "resume_requested" || strings.HasPrefix(eventType, "storage_") {
		// Storage retries can restart their counter after an explicit resume.
		// Keep each real recovery cycle while repeated polls remain silent.
		var latest AsyncMediaEvent
		err := tx.Session(&gorm.Session{NewDB: true}).Select("id").Where("task_id = ? AND event_type = ?", job.TaskId, "resume_requested").Order("id DESC").Take(&latest).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		discriminator = fmt.Sprintf("%s:%d", discriminator, latest.Id)
	}
	key := fmt.Sprintf("%s:%d:%s:%s", job.TaskId, job.AttemptGeneration, eventType, discriminator)
	event := AsyncMediaEvent{TaskId: job.TaskId, EventKey: fmt.Sprintf("%x", sha256.Sum256([]byte(key))), EventType: eventType,
		Status: job.Status, Message: "", CreatedAt: time.Now().Unix(), AttemptGeneration: job.AttemptGeneration,
		ChannelId: job.ChannelId, KeyIndex: keyIndex, ReferenceMode: referenceMode}
	return tx.Session(&gorm.Session{NewDB: true}).Clauses(clause.OnConflict{DoNothing: true}).Create(&event).Error
}

func recordAsyncMediaTransitionTx(tx *gorm.DB, before, after AsyncMediaJob, keyIndex *int, referenceMode string) error {
	if before.Status != after.Status || before.BillingStatus != after.BillingStatus || before.ReferenceRecoveryStage != after.ReferenceRecoveryStage {
		if err := recordVideoTaskPerfSampleTx(tx, before, after); err != nil {
			return err
		}
	}
	if before.Status != after.Status {
		eventType := after.Status
		switch after.Status {
		case "submitting":
			eventType = "attempt_started"
		case "queued":
			eventType = ""
		case "submitted", "succeeded", "failed", "expired", "execution_unknown":
		default:
			eventType = ""
		}
		if eventType != "" {
			if err := recordAsyncMediaEventTx(tx, after, eventType, "", nil, ""); err != nil {
				return err
			}
		}
	}
	if after.DispatchedAt > 0 && before.DispatchedAt != after.DispatchedAt {
		if err := recordAsyncMediaEventTx(tx, after, "dispatched", "", keyIndex, referenceMode); err != nil {
			return err
		}
	}
	if before.ReferenceRecoveryStage != after.ReferenceRecoveryStage {
		eventType := ""
		switch after.ReferenceRecoveryStage {
		case "prepare":
			eventType = "retry_scheduled"
		case "submit":
			eventType = "reference_ready"
		}
		if eventType != "" {
			if err := recordAsyncMediaEventTx(tx, after, eventType, "", nil, "local_download"); err != nil {
				return err
			}
		}
	}
	if before.StorageStatus != after.StorageStatus {
		if after.StorageStatus == "ready" {
			if err := recordAsyncMediaEventTx(tx, after, "upstream_succeeded", "", nil, ""); err != nil {
				return err
			}
		}
		switch after.StorageStatus {
		case "ready", "saving", "succeeded", "stored", "failed", "upstream", "expired":
			if err := recordAsyncMediaEventTx(tx, after, "storage_"+after.StorageStatus, fmt.Sprint(after.StorageRetries), nil, ""); err != nil {
				return err
			}
		}
	}
	if before.BillingStatus != after.BillingStatus {
		switch after.BillingStatus {
		case "reserving", "reserved", "settled", "refunded", "unknown", "not_billable", "failed":
			if err := recordAsyncMediaEventTx(tx, after, "billing_"+after.BillingStatus, "", nil, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// recordAsyncMediaNativeTaskTx runs after the native receipt/terminal write.
// A local timeout or reconciliation is not proof of an upstream failure.
func recordAsyncMediaNativeTaskTx(tx *gorm.DB, task Task, receipt bool) error {
	if !task.PrivateData.AsyncMedia {
		return nil
	}
	var job AsyncMediaJob
	err := lockForUpdate(tx.Session(&gorm.Session{NewDB: true})).Where("task_id = ? AND user_id = ?", task.TaskID, task.UserId).Take(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.HasPrefix(job.ReferenceRecoveryStage, "age_limit_") &&
		(job.AttemptGeneration != task.AttemptGeneration || receipt || task.Status == TaskStatusSuccess) {
		return ErrTaskAttemptConflict
	}
	if job.ChannelId != task.ChannelId || job.TokenId != task.PrivateData.TokenId || job.AttemptGeneration != task.AttemptGeneration {
		return nil
	}
	if receipt {
		if err := recordAsyncMediaEventTx(tx, job, "submitted", "", nil, ""); err != nil {
			return err
		}
	}
	if task.Status != TaskStatusSuccess && task.Status != TaskStatusFailure {
		return nil
	}
	eventType := "upstream_succeeded"
	if task.Status == TaskStatusFailure {
		eventType = "failed"
		if receipt || task.PrivateData.VideoTerminalConfirmed {
			eventType = "upstream_failed"
		}
	}
	return recordAsyncMediaEventTx(tx, job, eventType, "", nil, "")
}
