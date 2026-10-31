package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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

func ExpireAsyncMediaJob(ctx context.Context, job AsyncMediaJob, now int64) error {
	// Keep output pointers until the caller confirms file deletion, so a failed
	// filesystem cleanup remains discoverable on the next sweep.
	changes := map[string]any{"storage_status": "expired", "request_cipher": nil, "channel_cipher": nil}
	if job.ReferenceRecoveryStage != "refund" {
		changes["status"] = "expired"
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
		return ChangeAsyncMediaJob(query, changes, "")
	})
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
	err := tx.Session(&gorm.Session{NewDB: true}).Where("task_id = ? AND user_id = ? AND channel_id = ? AND token_id = ? AND attempt_generation = ?",
		task.TaskID, task.UserId, task.ChannelId, task.PrivateData.TokenId, task.AttemptGeneration).Take(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
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
