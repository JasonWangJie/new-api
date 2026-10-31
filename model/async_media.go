package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AsyncMediaJob owns submission and storage leases. Upstream task accounting
// remains in Task; retrying storage never enters the submission path.
type AsyncMediaJob struct {
	ID                     int    `json:"-"`
	TaskId                 string `json:"task_id" gorm:"size:64;uniqueIndex"`
	IdentityHash           string `json:"-" gorm:"size:64;uniqueIndex"`
	RequestHash            string `json:"-" gorm:"size:64"`
	UserId                 int    `json:"user_id" gorm:"index"`
	TokenId                int    `json:"api_key_id" gorm:"index"`
	ChannelId              int    `json:"-"`
	Provider               string `json:"provider" gorm:"size:32"`
	Model                  string `json:"model" gorm:"size:255"`
	Group                  string `json:"group" gorm:"size:64"`
	RequestCipher          []byte `json:"-"`
	ChannelCipher          []byte `json:"-"`
	OutputRootPath         string `json:"-" gorm:"type:text"`
	OutputObjectKey        string `json:"-" gorm:"size:128"`
	ArtifactKeys           string `json:"-" gorm:"type:text"`
	Status                 string `json:"status" gorm:"size:32;index"`
	StorageStatus          string `json:"storage_status" gorm:"size:32"`
	BillingStatus          string `json:"billing_status" gorm:"size:32"`
	RefundLogPayload       string `json:"-" gorm:"type:text"`
	Quota                  int    `json:"quota"`
	DispatchedAt           int64  `json:"-"`
	LeaseToken             string `json:"-" gorm:"size:64"`
	LeaseExpiresAt         int64  `json:"-"`
	NextAttemptAt          int64  `json:"-" gorm:"index"`
	StorageRetries         int    `json:"storage_retry_count"`
	AttemptGeneration      int64  `json:"-" gorm:"not null;default:0"`
	ReferenceRetryCount    int    `json:"-" gorm:"not null;default:0"`
	ReferenceRecoveryStage string `json:"-" gorm:"size:32"`
	ErrorMessage           string `json:"error_message,omitempty" gorm:"type:text"`
	CreatedAt              int64  `json:"created_at" gorm:"index"`
	ExpiresAt              int64  `json:"expires_at"`
}

type MediaArtifactObject struct {
	ID           int    `json:"-"`
	ObjectId     string `json:"object_id" gorm:"size:64;uniqueIndex"`
	IdentityHash string `json:"-" gorm:"size:64;uniqueIndex"`
	TaskId       string `json:"task_id" gorm:"size:64;index"`
	ArtifactKey  string `json:"key" gorm:"size:128"`
	UserId       int    `json:"-" gorm:"index"`
	TokenId      int    `json:"-"`
	RootPath     string `json:"-" gorm:"type:text"`
	ObjectKey    string `json:"-" gorm:"size:255"`
	MimeType     string `json:"mime_type" gorm:"size:64"`
	ByteSize     int64  `json:"byte_size"`
	Checksum     string `json:"checksum" gorm:"size:64"`
	CreatedAt    int64  `json:"created_at"`
	ExpiresAt    int64  `json:"expires_at" gorm:"index"`
	Status       string `json:"-" gorm:"size:16"`
}

// GetDueAsyncMediaJobs rotates pending work by its next eligible attempt. A
// long-running upstream job must not monopolize every worker slot while newer
// submissions or completed results wait behind it.
func GetDueAsyncMediaJobs(ctx context.Context, now int64, limit int) ([]AsyncMediaJob, error) {
	return GetDueAsyncMediaJobsForPhase(ctx, now, limit, "")
}

// GetDueAsyncMediaJobsForPhase gives each independently bounded worker pool
// its own queue, so a blocked download cannot hide later submissions.
func GetDueAsyncMediaJobsForPhase(ctx context.Context, now int64, limit int, phase string) ([]AsyncMediaJob, error) {
	var jobs []AsyncMediaJob
	query := DB.WithContext(ctx).Where("status IN ? AND lease_expires_at <= ? AND next_attempt_at <= ?", []string{"queued", "submitting", "submitted", "reconciling"}, now, now)
	switch phase {
	case "submit":
		query = query.Where("status IN ? AND COALESCE(reference_recovery_stage, '') <> ?", []string{"queued", "submitting"}, "prepare")
	case "status":
		query = query.Where("status = ? OR (status = ? AND storage_status NOT IN ?)", "reconciling", "submitted", []string{"ready", "saving", "failed"})
	case "download":
		query = query.Where("((status = ? AND reference_recovery_stage = ?) OR (status = ? AND storage_status IN ?))", "queued", "prepare", "submitted", []string{"ready", "saving", "failed"})
	case "":
	default:
		return nil, errors.New("invalid async media worker phase")
	}
	err := query.
		Order("CASE WHEN next_attempt_at = 0 THEN created_at ELSE next_attempt_at END").Order("created_at").Order("id").Limit(limit).Find(&jobs).Error
	return jobs, err
}

func AcceptAsyncMediaJob(ctx context.Context, job AsyncMediaJob) (AsyncMediaJob, bool, error) {
	reused := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).Select("id").Where("id = ?", job.UserId).Take(&user).Error; err != nil {
			return err
		}
		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", job.TokenId, job.UserId).Take(&token).Error; err != nil {
			return err
		}
		var existing AsyncMediaJob
		err := tx.Where("identity_hash = ?", job.IdentityHash).Take(&existing).Error
		if err == nil {
			if existing.RequestHash != job.RequestHash {
				return ErrImageConflict
			}
			job, reused = existing, true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := enforceAsyncAdmission(tx, &AsyncMediaJob{}, job.UserId, job.TokenId, len(job.RequestCipher)); err != nil {
			return err
		}
		job.Status, job.StorageStatus, job.BillingStatus = "queued", "pending", "pending"
		job.CreatedAt = time.Now().Unix()
		if err := tx.Create(&job).Error; err != nil {
			return err
		}
		return recordAsyncMediaEventTx(tx, job, "accepted", "", nil, "")
	})
	return job, reused, err
}

func ClaimAsyncMediaJob(ctx context.Context, job *AsyncMediaJob, lease string, seconds int) (bool, error) {
	now := time.Now().Unix()
	result := DB.WithContext(ctx).Model(&AsyncMediaJob{}).Where("id = ? AND lease_expires_at <= ? AND next_attempt_at <= ? AND status IN ?", job.ID, now, now, []string{"queued", "submitting", "submitted", "reconciling"}).
		Where("status = ? AND storage_status = ? AND attempt_generation = ? AND COALESCE(reference_recovery_stage, '') = ?", job.Status, job.StorageStatus, job.AttemptGeneration, job.ReferenceRecoveryStage).
		Updates(map[string]any{"lease_token": lease, "lease_expires_at": now + int64(seconds)})
	if result.Error != nil || result.RowsAffected == 0 {
		return false, result.Error
	}
	job.LeaseToken, job.LeaseExpiresAt = lease, now+int64(seconds)
	return true, nil
}

func UpdateAsyncMediaJob(ctx context.Context, job AsyncMediaJob, updates map[string]any) error {
	query := DB.WithContext(ctx).Model(&AsyncMediaJob{}).Where("id = ? AND lease_token = ? AND attempt_generation = ?", job.ID, job.LeaseToken, job.AttemptGeneration)
	if job.Status != "reconciling" {
		// A status/submit worker that read before reconciliation began must not
		// erase its projection or expire the retained credential envelope.
		query = query.Where("status <> ?", "reconciling")
	}
	return ChangeAsyncMediaJob(query, updates, "")
}

// DispatchAsyncMediaJob records the selected live account slot only when the
// current lease actually crosses the provider-dispatch barrier.
func DispatchAsyncMediaJob(ctx context.Context, job AsyncMediaJob, updates map[string]any, keyIndex int, referenceMode string) error {
	if keyIndex < -1 || referenceMode != "" && referenceMode != "original" && referenceMode != "local_download" {
		return ErrImageConflict
	}
	var slot *int
	if keyIndex >= 0 {
		slot = &keyIndex
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var before AsyncMediaJob
		if err := lockForUpdate(tx).Where("id = ? AND lease_token = ? AND attempt_generation = ?", job.ID, job.LeaseToken, job.AttemptGeneration).Take(&before).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrImageConflict
			}
			return err
		}
		if err := tx.Model(&AsyncMediaJob{}).Where("id = ? AND lease_token = ? AND attempt_generation = ?", job.ID, job.LeaseToken, job.AttemptGeneration).Updates(updates).Error; err != nil {
			return err
		}
		var after AsyncMediaJob
		if err := tx.Where("id = ?", job.ID).Take(&after).Error; err != nil {
			return err
		}
		return recordAsyncMediaTransitionTx(tx, before, after, slot, referenceMode)
	})
}

type asyncMediaSubmissionRefundLogs struct {
	Version int                        `json:"version"`
	Consume *Log                       `json:"consume,omitempty"`
	Refund  *Log                       `json:"refund"`
	Usage   *AsyncImageUsageProjection `json:"usage,omitempty"`
}

func parseAsyncMediaSubmissionRefundLogs(payload string) (asyncMediaSubmissionRefundLogs, error) {
	var logs asyncMediaSubmissionRefundLogs
	if err := common.UnmarshalJsonStr(payload, &logs); err != nil {
		return logs, err
	}
	if logs.Version == 0 && logs.Consume == nil && logs.Refund == nil && logs.Usage == nil {
		// Existing commands contain only the refund. Deliver them unchanged;
		// replay must not backfill historical consumption implicitly.
		logs.Refund = &Log{}
		if err := common.UnmarshalJsonStr(payload, logs.Refund); err != nil {
			return logs, err
		}
	} else if logs.Version != 1 || logs.Consume == nil || logs.Refund == nil {
		return logs, ErrImageConflict
	}
	return logs, nil
}

// SaveAsyncMediaSubmissionRefund persists confirmed rollback logs together.
// Retrying their delivery must never repeat financial adjustments.
func SaveAsyncMediaSubmissionRefund(ctx context.Context, taskID string, userID int, updates map[string]any, refund *RecordTaskBillingLogParams, expectedGeneration ...int64) error {
	generation := int64(0)
	if len(expectedGeneration) > 1 || len(expectedGeneration) == 1 && expectedGeneration[0] < 0 {
		return ErrImageConflict
	}
	if len(expectedGeneration) == 1 {
		generation = expectedGeneration[0]
	}
	var payload string
	var refundLog *Log
	if refund != nil {
		if refund.LogType != LogTypeRefund || refund.UserId != userID || refund.TokenId <= 0 || refund.Quota <= 0 || updates["billing_status"] != "refunded" {
			return ErrImageConflict
		}
		refundLog = newTaskBillingLog(*refund)
		ensureLogRequestId(refundLog)
		data, err := common.Marshal(refundLog)
		if err != nil {
			return err
		}
		payload = string(data)
	}
	if err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job AsyncMediaJob
		if err := lockForUpdate(tx).
			Where("task_id = ? AND user_id = ? AND attempt_generation = ?", taskID, userID, generation).Take(&job).Error; err != nil {
			return err
		}
		changes := maps.Clone(updates)
		if changes == nil {
			changes = make(map[string]any)
		}
		delete(changes, "refund_log_payload")
		if refund != nil {
			if refund.TokenId != job.TokenId {
				return ErrImageConflict
			}
			if job.RefundLogPayload != "" {
				existingLogs, err := parseAsyncMediaSubmissionRefundLogs(job.RefundLogPayload)
				if err != nil {
					return err
				}
				existing := existingLogs.Refund
				if existing.Type != refund.LogType || existing.UserId != refund.UserId || existing.TokenId != refund.TokenId || existing.ChannelId != refund.ChannelId || existing.ModelName != refund.ModelName || existing.Quota != refund.Quota {
					return ErrImageConflict
				}
			} else if job.BillingStatus != "refunded" {
				if common.LogConsumeEnabled && job.AttemptGeneration == 0 {
					var acceptedTasks int64
					if err := tx.Model(&Task{}).Where("task_id = ? AND user_id = ?", taskID, userID).Count(&acceptedTasks).Error; err != nil {
						return err
					}
					if acceptedTasks == 0 {
						var other map[string]any
						if err := common.UnmarshalJsonStr(refundLog.Other, &other); err != nil {
							return err
						}
						if other == nil {
							other = make(map[string]any)
						}
						other["is_task"], other["task_id"], other["billing_phase"] = true, taskID, "submission_preconsume"
						other["reason"] = "Task submission failed; recording its reserved quota"
						encodedOther, err := common.Marshal(other)
						if err != nil {
							return err
						}
						consume := *refundLog
						consume.Type, consume.Content, consume.Other = LogTypeConsume, "视频提交失败，补记预扣消费", string(encodedOther)
						consume.PromptTokens, consume.CompletionTokens, consume.UseTime = 0, 0, 0
						command := asyncMediaSubmissionRefundLogs{Version: 1, Consume: &consume, Refund: refundLog}
						if common.DataExportEnabled {
							nodeName := refund.NodeName
							if nodeName == "" {
								nodeName = common.NodeName
							}
							// Reuse the durable per-event usage table. This audit adds
							// its gross quota without counting a generated request.
							command.Usage = &AsyncImageUsageProjection{
								EventId: "video-submission-preconsume:" + taskID,
								UserID:  consume.UserId, Username: consume.Username, ModelName: consume.ModelName,
								CreatedAt: consume.CreatedAt - consume.CreatedAt%3600, UseGroup: consume.Group,
								TokenID: consume.TokenId, ChannelID: consume.ChannelId, NodeName: nodeName, Quota: consume.Quota,
							}
						}
						data, err := common.Marshal(command)
						if err != nil {
							return err
						}
						payload = string(data)
					}
				}
				changes["refund_log_payload"] = payload
			}
		}
		// The locked read proves ownership even when an identical MySQL update
		// reports zero changed rows. Keep an already-pending log immutable.
		if err := tx.Model(&AsyncMediaJob{}).Where("id = ? AND task_id = ? AND user_id = ? AND attempt_generation = ?", job.ID, taskID, userID, generation).Updates(changes).Error; err != nil {
			return err
		}
		var after AsyncMediaJob
		if err := tx.Where("id = ?", job.ID).Take(&after).Error; err != nil {
			return err
		}
		return recordAsyncMediaTransitionTx(tx, job, after, nil, "")
	}); err != nil {
		return err
	}
	if refund != nil {
		return ConfirmAsyncMediaRefundLog(ctx, taskID)
	}
	return nil
}

// ConfirmAsyncMediaRefundLog delivers rollback logs and their gross usage
// projection without changing balances or settled usage counters. Captured
// logging and export decisions survive subsequent settings changes.
func ConfirmAsyncMediaRefundLog(ctx context.Context, taskID string) error {
	var job AsyncMediaJob
	if err := DB.WithContext(ctx).Select("id", "task_id", "user_id", "token_id", "refund_log_payload").Where("task_id = ?", taskID).Take(&job).Error; err != nil {
		return err
	}
	if job.RefundLogPayload == "" {
		return nil
	}
	logs, err := parseAsyncMediaSubmissionRefundLogs(job.RefundLogPayload)
	if err != nil {
		return err
	}
	log := logs.Refund
	if log.Type != LogTypeRefund || log.UserId != job.UserId || log.TokenId != job.TokenId || log.Quota <= 0 || log.RequestId == "" || log.Id != 0 {
		return ErrImageConflict
	}
	projectUsage := false
	if logs.Consume != nil {
		consume := logs.Consume
		if consume.Id != 0 || consume.Type != LogTypeConsume || consume.UserId != log.UserId || consume.TokenId != log.TokenId || consume.ChannelId != log.ChannelId || consume.ModelName != log.ModelName || consume.Group != log.Group || consume.Quota != log.Quota || consume.RequestId != log.RequestId || consume.PromptTokens != 0 || consume.CompletionTokens != 0 || consume.UseTime != 0 {
			return ErrImageConflict
		}
		var other map[string]any
		if common.UnmarshalJsonStr(consume.Other, &other) != nil || other["task_id"] != job.TaskId || other["billing_phase"] != "submission_preconsume" {
			return ErrImageConflict
		}
		if usage := logs.Usage; usage != nil {
			if usage.Id != 0 || usage.EventId != "video-submission-preconsume:"+job.TaskId || usage.UserID != consume.UserId || usage.Username != consume.Username || usage.ModelName != consume.ModelName || usage.CreatedAt != consume.CreatedAt-consume.CreatedAt%3600 || usage.UseGroup != consume.Group || usage.TokenID != consume.TokenId || usage.ChannelID != consume.ChannelId || usage.Quota != consume.Quota || usage.Count != 0 || usage.TokenUsed != 0 {
				return ErrImageConflict
			}
		}
		if err := recordAsyncMediaSubmissionRefundPair(ctx, job.TaskId, *consume, *log); err != nil {
			return err
		}
		if logs.Usage != nil {
			var existing []Log
			if err := imageConsumeLogsQuery(LOG_DB.WithContext(ctx)).Where("request_id = ? AND user_id = ? AND token_id = ? AND type = ?", consume.RequestId, consume.UserId, consume.TokenId, LogTypeConsume).Limit(2).Find(&existing).Error; err != nil {
				return err
			}
			if len(existing) != 1 || !isAsyncMediaSubmissionConsumption(existing[0], *consume, job.TaskId) {
				return ErrImageConflict
			}
			// An adopted older consumption already owns its normal usage
			// export. Only the exact log captured by this command needs this
			// projection, including when delivery committed before a crash.
			existing[0].Id = 0
			projectUsage = existing[0] == *consume
		}
	} else if err := recordTaskBillingLogOnce(ctx, log, "video-refund:"+job.TaskId, fmt.Sprintf("%x", sha256.Sum256([]byte(job.RefundLogPayload)))); err != nil {
		return err
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current AsyncMediaJob
		if err := lockForUpdate(tx).Select("refund_log_payload").Where("id = ? AND user_id = ?", job.ID, job.UserId).Take(&current).Error; err != nil {
			return err
		}
		if current.RefundLogPayload == "" {
			return nil
		}
		if current.RefundLogPayload != job.RefundLogPayload {
			return ErrImageConflict
		}
		if projectUsage {
			projection := *logs.Usage
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "event_id"}}, DoNothing: true}).Create(&projection).Error; err != nil {
				return err
			}
			var existing AsyncImageUsageProjection
			if err := tx.Where("event_id = ?", logs.Usage.EventId).Take(&existing).Error; err != nil {
				return err
			}
			existing.Id = 0
			if existing != *logs.Usage {
				return ErrImageConflict
			}
		}
		// The projection and acknowledgement commit together. A failure at
		// either point leaves the immutable command available for replay.
		return tx.Model(&AsyncMediaJob{}).
			Where("id = ? AND user_id = ? AND refund_log_payload = ?", job.ID, job.UserId, job.RefundLogPayload).
			Update("refund_log_payload", "").Error
	})
}

// recordAsyncMediaSubmissionRefundPair commits relational log entries together.
// ClickHouse uses the existing per-event delivery and the persisted command to
// finish a partially delivered pair. Neither path changes financial state.
func recordAsyncMediaSubmissionRefundPair(ctx context.Context, taskID string, consume, refund Log) error {
	logs := []Log{consume, refund}
	eventIDs := []string{"video-submission-preconsume:" + taskID, "video-refund:" + taskID}
	fingerprints := make([]string, len(logs))
	for index := range logs {
		encoded, err := common.Marshal(logs[index])
		if err != nil {
			return err
		}
		fingerprints[index] = fmt.Sprintf("%x", sha256.Sum256(encoded))
	}
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		for index := range logs {
			if index == 0 {
				var existing []Log
				if err := imageConsumeLogsQuery(LOG_DB.WithContext(ctx)).Where("request_id = ? AND user_id = ? AND token_id = ? AND type = ?", consume.RequestId, consume.UserId, consume.TokenId, LogTypeConsume).Limit(2).Find(&existing).Error; err != nil {
					return err
				}
				if len(existing) > 0 {
					if len(existing) != 1 || !isAsyncMediaSubmissionConsumption(existing[0], consume, taskID) {
						return ErrImageConflict
					}
					continue
				}
			}
			if err := recordTaskBillingLogOnce(ctx, &logs[index], eventIDs[index], fingerprints[index]); err != nil {
				return err
			}
		}
		return nil
	}
	err := LOG_DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for index := range logs {
			var receipt AsyncImageLogReceipt
			err := tx.Where("event_id = ?", eventIDs[index]).Take(&receipt).Error
			if err == nil {
				if receipt.Fingerprint != fingerprints[index] {
					return ErrImageConflict
				}
				continue
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err := tx.Create(&AsyncImageLogReceipt{EventId: eventIDs[index], Fingerprint: fingerprints[index]}).Error; err != nil {
				return err
			}
			if index == 0 {
				var existing []Log
				if err := tx.Where("request_id = ? AND user_id = ? AND token_id = ? AND type = ?", consume.RequestId, consume.UserId, consume.TokenId, LogTypeConsume).Limit(2).Find(&existing).Error; err != nil {
					return err
				}
				if len(existing) > 0 {
					if len(existing) != 1 || !isAsyncMediaSubmissionConsumption(existing[0], consume, taskID) {
						return ErrImageConflict
					}
					continue
				}
			}
			if err := tx.Create(&logs[index]).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return nil
	}
	// A concurrent delivery can win the unique event IDs. After rollback, both
	// matching receipts prove that its complete transaction already committed.
	var receipts []AsyncImageLogReceipt
	if lookupErr := LOG_DB.WithContext(ctx).Where("event_id IN ?", eventIDs).Find(&receipts).Error; lookupErr != nil || len(receipts) != len(eventIDs) {
		return err
	}
	for _, receipt := range receipts {
		if receipt.EventId == eventIDs[0] && receipt.Fingerprint == fingerprints[0] || receipt.EventId == eventIDs[1] && receipt.Fingerprint == fingerprints[1] {
			continue
		}
		return err
	}
	return nil
}

func isAsyncMediaSubmissionConsumption(existing, expected Log, taskID string) bool {
	if existing.Quota != expected.Quota || existing.ChannelId != expected.ChannelId || existing.ModelName != expected.ModelName || existing.Group != expected.Group {
		return false
	}
	var other map[string]any
	return common.UnmarshalJsonStr(existing.Other, &other) == nil && other["task_id"] == taskID
}

// ReplayAsyncMediaRefundLogs repairs missing usage logs, including terminal and
// expired tasks. It never resumes generation or performs financial adjustments.
func ReplayAsyncMediaRefundLogs(ctx context.Context, limit int) error {
	if limit <= 0 {
		return nil
	}
	var jobs []AsyncMediaJob
	if err := DB.WithContext(ctx).Select("task_id").Where("COALESCE(refund_log_payload, '') <> ''").Order("id").Limit(limit).Find(&jobs).Error; err != nil {
		return err
	}
	var replayErrors []error
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(replayErrors, err)...)
		}
		if err := ConfirmAsyncMediaRefundLog(ctx, job.TaskId); err != nil {
			replayErrors = append(replayErrors, fmt.Errorf("media refund log %s: %w", job.TaskId, err))
		}
	}
	return errors.Join(replayErrors...)
}
