package model

import (
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var errMeasuredTaskCASLost = errors.New("measured task terminal transition was already claimed")

// RecordMeasuredTaskSubmitAccounting atomically persists the submit-side usage
// counters and a task marker. A repeated call after an ambiguous response is a
// no-op, so neither request count nor usage can be applied twice.
func RecordMeasuredTaskSubmitAccounting(ctx context.Context, task *Task) (recorded bool, err error) {
	if task == nil || task.ID <= 0 || task.UserId <= 0 || task.ChannelId <= 0 || task.Quota < 0 {
		return false, errors.New("invalid measured task accounting inputs")
	}
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		if current.AttemptGeneration != task.AttemptGeneration {
			return ErrTaskAttemptConflict
		}
		if current.PrivateData.SubmitAccountingRecorded {
			return nil
		}
		if current.UserId != task.UserId || current.ChannelId != task.ChannelId || current.BillingChannelID() != task.BillingChannelID() || current.Quota != task.Quota {
			return errors.New("measured task accounting inputs do not match persisted task")
		}
		subject, err := CanvasBillingSubject(tx, current.PrivateData.TokenId, task.UserId)
		if err != nil {
			return err
		}
		userQuery := tx.Unscoped().Model(&User{}).Where("id = ?", task.UserId)
		if subject != "" {
			userQuery = userQuery.Where("canvas_subject = ?", subject)
		}
		userResult := userQuery.Updates(map[string]any{
			"used_quota":    gorm.Expr("used_quota + ?", task.Quota),
			"request_count": gorm.Expr("request_count + 1"),
		})
		if userResult.Error != nil {
			return userResult.Error
		}
		if userResult.RowsAffected != 1 {
			return errors.New("measured task user accounting could not be applied")
		}
		channelResult := tx.Model(&Channel{}).Where("id = ?", current.BillingChannelID()).
			Update("used_quota", gorm.Expr("used_quota + ?", task.Quota))
		if channelResult.Error != nil {
			return channelResult.Error
		}
		if channelResult.RowsAffected != 1 && task.Quota != 0 {
			var count int64
			if err := tx.Model(&Channel{}).Where("id = ?", current.BillingChannelID()).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return errors.New("measured task channel accounting could not be applied")
			}
		}
		current.PrivateData.SubmitAccountingRecorded = true
		if current.PrivateData.BillingChannelID == 0 {
			current.PrivateData.BillingChannelID = current.ChannelId
		}
		if err := tx.Model(&Task{}).Where("id = ? AND attempt_generation = ?", task.ID, task.AttemptGeneration).Update("private_data", current.PrivateData).Error; err != nil {
			return err
		}
		recorded = true
		return nil
	})
	if err == nil {
		task.PrivateData.SubmitAccountingRecorded = true
		if task.PrivateData.BillingChannelID == 0 {
			task.PrivateData.BillingChannelID = task.ChannelId
		}
	}
	return recorded, err
}

// SettleTaskBilling commits a terminal task and its balance changes together.
// An unaffordable completion becomes a failure with a full reservation refund;
// it never publishes a successful result that has not been paid for.
func SettleTaskBilling(ctx context.Context, task *Task, fromStatus TaskStatus, actualQuota int, usageFacts map[string]any, matchedTier string) (won bool, reservedQuota int, err error) {
	return settleTaskBilling(ctx, task, fromStatus, actualQuota, usageFacts, matchedTier, nil)
}

type asyncMediaTermination struct {
	job   AsyncMediaJob
	other *LogOther
}

func settleTaskBilling(ctx context.Context, task *Task, fromStatus TaskStatus, actualQuota int, usageFacts map[string]any, matchedTier string, termination *asyncMediaTermination) (won bool, reservedQuota int, err error) {
	if task == nil || task.ID <= 0 || task.Quota < 0 || actualQuota < 0 {
		return false, 0, errors.New("invalid task settlement inputs")
	}
	if termination == nil && (fromStatus == TaskStatusSuccess || fromStatus == TaskStatusFailure) ||
		(task.Status != TaskStatusSuccess && task.Status != TaskStatusFailure) {
		return false, 0, errors.New("task settlement must transition from non-terminal to terminal")
	}
	reservedQuota = task.Quota
	if task.Status == TaskStatusFailure && actualQuota != 0 {
		return false, reservedQuota, errors.New("failed task must refund its full reservation")
	}
	updated := *task
	updated.Quota = actualQuota
	if billingContext := task.PrivateData.BillingContext; billingContext != nil {
		copied := *billingContext
		if billingContext.TieredSnapshot != nil {
			snapshot := *billingContext.TieredSnapshot
			snapshot.UsageFacts = maps.Clone(snapshot.UsageFacts)
			if len(usageFacts) > 0 {
				if snapshot.UsageFacts == nil {
					snapshot.UsageFacts = make(map[string]any)
				}
				maps.Copy(snapshot.UsageFacts, usageFacts)
			}
			if task.Status == TaskStatusSuccess && matchedTier != "" {
				snapshot.EstimatedTier = matchedTier
			}
			copied.TieredSnapshot = &snapshot
		}
		updated.PrivateData.BillingContext = &copied
	}
	var tokenKey, subject, generation string
	// Cache reservations are made while holding the task row lock. Track them
	// separately so a database rollback or token rejection compensates exactly
	// the cache changes made by this attempt.
	userCacheReserved, tokenCacheReserved := 0, 0
	delta := actualQuota - reservedQuota
	transactionBodySucceeded := false
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		if current.Status != fromStatus || current.Quota != reservedQuota || current.AttemptGeneration != task.AttemptGeneration || current.PrivateData.UpstreamTaskID != task.PrivateData.UpstreamTaskID {
			return errMeasuredTaskCASLost
		}
		var terminatedJob AsyncMediaJob
		if termination != nil {
			if err := lockForUpdate(tx).Where("id = ? AND task_id = ? AND user_id = ?", termination.job.ID, current.TaskID, current.UserId).Take(&terminatedJob).Error; err != nil {
				return err
			}
			if terminatedJob.ReferenceRecoveryStage == AsyncMediaAdminTerminatedStage {
				return errMeasuredTaskCASLost
			}
			if terminatedJob.AttemptGeneration != current.AttemptGeneration ||
				terminatedJob.ChannelId != current.ChannelId || terminatedJob.TokenId != current.PrivateData.TokenId || !current.PrivateData.AsyncMedia ||
				current.Quota > 0 && (!current.PrivateData.DurableBilling || !current.PrivateData.SubmitAccountingRecorded) || terminatedJob.RefundLogPayload != "" {
				return ErrTaskAttemptConflict
			}
			if current.AttemptGeneration == math.MaxInt64 {
				return ErrTaskAttemptConflict
			}
			updated.AttemptGeneration = current.AttemptGeneration + 1
			updated.PrivateData = current.PrivateData
			updated.PrivateData.ResultURL, updated.PrivateData.PluginState, updated.PrivateData.UpstreamAsyncResponse = "", nil, nil
			updated.PrivateData.VideoReconciliation, updated.PrivateData.MediaRetryFact = nil, nil
			updated.PrivateData.VideoTerminalConfirmed = false
			updated.Data = nil
		}
		if termination == nil && current.PrivateData.AsyncMedia && current.PrivateData.MediaRetryFact != nil && current.PrivateData.MediaRetryFact.HasOutput && task.Status == TaskStatusFailure {
			return errors.New("confirmed video output requires billing reconciliation")
		}
		if termination == nil && current.PrivateData.AsyncMedia && task.Status == TaskStatusFailure && slices.ContainsFunc(current.PrivateData.VideoAttempts, func(attempt VideoTaskAttempt) bool { return attempt.Status == TaskStatusUnknown }) {
			var job AsyncMediaJob
			if err := lockForUpdate(tx).Where("task_id = ? AND user_id = ? AND attempt_generation = ?", current.TaskID, current.UserId, current.AttemptGeneration).Take(&job).Error; err != nil {
				return err
			}
			if job.ReferenceRecoveryStage != "age_limit_refund" {
				return errors.New("unconfirmed video attempt requires billing reconciliation")
			}
		}
		if current.PrivateData.DurableBilling && !current.PrivateData.SubmitAccountingRecorded && (termination == nil || current.Quota > 0) {
			return errors.New("task submission accounting is not yet recorded")
		}
		if current.UserId != task.UserId || current.ChannelId != task.ChannelId {
			return errors.New("task settlement identity mismatch")
		}
		updated.PrivateData.BillingChannelID = current.BillingChannelID()
		if termination == nil {
			updated.PrivateData.MediaRetryFact = current.PrivateData.MediaRetryFact
		}
		updated.PrivateData.MediaRetryFailedAt = current.PrivateData.MediaRetryFailedAt
		updated.PrivateData.MediaRetryState = current.PrivateData.MediaRetryState
		updated.PrivateData.MediaRetryDecision = current.PrivateData.MediaRetryDecision
		privateData := current.PrivateData
		var subjectErr error
		subject, subjectErr = CanvasBillingSubject(tx, privateData.TokenId, task.UserId)
		if subjectErr != nil {
			return subjectErr
		}
		if err := LockCanvasBillingOwner(tx, task.UserId, subject); err != nil {
			return err
		}
		var subscription UserSubscription
		samePeriod := true
		if privateData.BillingSource == "subscription" {
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", privateData.SubscriptionId, task.UserId).Take(&subscription).Error; err != nil {
				return err
			}
			samePeriod = SubscriptionReservationMatchesPeriod(&subscription, privateData.SubscriptionPeriodStart, current.SubmitTime)
		}
		var user User
		if err := lockForUpdate(tx.Unscoped()).Where("id = ?", task.UserId).Take(&user).Error; err != nil {
			return err
		}
		var token Token
		if privateData.TokenId > 0 {
			if err := lockForUpdate(tx.Unscoped()).Where("id = ?", privateData.TokenId).Take(&token).Error; err != nil {
				return err
			}
			tokenKey = token.Key
		}
		var ownerErr error
		subject, ownerErr = ValidateCanvasBillingOwner(tx, token, user)
		if ownerErr != nil {
			return ownerErr
		}
		generation, ownerErr = canvasQuotaCacheGeneration(user.Id, subject)
		if ownerErr != nil || common.RedisEnabled && subject != "" && generation == "" && delta > 0 && privateData.BillingSource != "subscription" {
			return ErrImageQuotaCacheUnavailable
		}
		affordable := true
		if delta > 0 {
			if privateData.BillingSource == "subscription" {
				affordable = samePeriod && subscription.Status == "active" && subscription.EndTime > common.GetTimestamp() &&
					(subscription.AmountTotal <= 0 || int64(delta) <= subscription.AmountTotal-subscription.AmountUsed)
			} else if user.Quota < delta {
				affordable = false
			} else if common.RedisEnabled {
				result, cacheErr := cacheTryReserveUserQuota(task.UserId, int64(delta), subject, generation)
				if cacheErr != nil || result == cacheQuotaMiss {
					if common.BatchUpdateEnabled {
						return errors.New("task settlement wallet cache unavailable during batch updates")
					}
					affordable = user.Quota >= delta
				} else {
					affordable = result == cacheQuotaOK
					if affordable {
						userCacheReserved = delta
					}
				}
			} else {
				affordable = user.Quota >= delta
			}
			if affordable && privateData.TokenId > 0 && !token.UnlimitedQuota {
				if token.RemainQuota < delta {
					affordable = false
				} else if common.RedisEnabled {
					result, cacheErr := cacheTryReserveTokenQuota(token.Id, token.Key, int64(delta))
					if cacheErr != nil || result == cacheQuotaMiss {
						if common.BatchUpdateEnabled {
							return errors.New("task settlement token cache unavailable during batch updates")
						}
						affordable = token.RemainQuota >= delta
					} else {
						affordable = result == cacheQuotaOK
						if affordable {
							tokenCacheReserved = delta
						}
					}
				} else {
					affordable = token.RemainQuota >= delta
				}
			}
		}
		if !affordable {
			updated.Status = TaskStatusFailure
			updated.Progress = "100%"
			updated.FailReason = "insufficient quota to settle completed task"
			updated.Quota = 0
			updated.Data = nil
			updated.PrivateData.ResultURL = ""
			updated.PrivateData.PluginState = nil
			updated.PrivateData.UpstreamAsyncResponse = nil
			delta = -reservedQuota
		}
		if current.AttemptGeneration > 0 && current.PrivateData.AsyncMedia {
			updated.PrivateData.VideoAttempts = append([]VideoTaskAttempt(nil), current.PrivateData.VideoAttempts...)
			attemptIndex := -1
			for i := range updated.PrivateData.VideoAttempts {
				if updated.PrivateData.VideoAttempts[i].Generation == current.AttemptGeneration {
					attemptIndex = i
					break
				}
			}
			if attemptIndex < 0 {
				startedAt := current.StartTime
				if startedAt <= 0 {
					startedAt = current.SubmitTime
				}
				updated.PrivateData.VideoAttempts = append(updated.PrivateData.VideoAttempts, VideoTaskAttempt{ChannelID: current.ChannelId, Decision: current.PrivateData.MediaRetryDecision, Generation: current.AttemptGeneration, UpstreamTaskID: current.PrivateData.UpstreamTaskID, StartedAt: startedAt})
				attemptIndex = len(updated.PrivateData.VideoAttempts) - 1
			}
			attempt := &updated.PrivateData.VideoAttempts[attemptIndex]
			attempt.Status, attempt.Reason, attempt.FinishedAt = string(updated.Status), updated.FailReason, time.Now().Unix()
		}
		if delta != 0 {
			credit := -delta
			if privateData.BillingSource == "subscription" && samePeriod {
				// A reset expires the old period's reservation. Refund only within
				// that period, never against usage accrued after a reset.
				used := max(subscription.AmountUsed+int64(delta), 0)
				if err := tx.Model(&UserSubscription{}).Where("id = ?", subscription.Id).Update("amount_used", used).Error; err != nil {
					return err
				}
			}
			userUpdates := map[string]any{"used_quota": gorm.Expr("used_quota + ?", delta)}
			userQuery := tx.Unscoped().Model(&User{}).Where("id = ?", task.UserId)
			if subject != "" {
				userQuery = userQuery.Where("canvas_subject = ?", subject)
			}
			if current.PrivateData.SubmitAccountingRecorded && credit > 0 {
				userQuery = userQuery.Where("used_quota >= ?", credit)
			}
			if privateData.BillingSource != "subscription" {
				userUpdates["quota"] = gorm.Expr("quota - ?", delta)
				if credit > 0 {
					userQuery = userQuery.Where("quota <= ?", common.MaxWalletQuota-credit)
				}
			}
			result := userQuery.Updates(userUpdates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("task user refund or charge could not be applied")
			}
			if privateData.TokenId > 0 {
				query := tx.Unscoped().Model(&Token{}).Where("id = ?", privateData.TokenId)
				if credit > 0 {
					query = query.Where("remain_quota <= ?", common.MaxWalletQuota-credit)
					if current.PrivateData.SubmitAccountingRecorded {
						query = query.Where("used_quota >= ?", credit)
					}
				}
				result := query.Updates(map[string]any{
					"remain_quota":  gorm.Expr("remain_quota - ?", delta),
					"used_quota":    gorm.Expr("used_quota + ?", delta),
					"accessed_time": common.GetTimestamp(),
				})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return errors.New("task token refund could not be applied")
				}
			}
			query := tx.Model(&Channel{}).Where("id = ?", current.BillingChannelID())
			if current.PrivateData.SubmitAccountingRecorded && credit > 0 {
				query = query.Where("used_quota >= ?", credit)
			}
			result = query.Update("used_quota", gorm.Expr("used_quota + ?", delta))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				var count int64
				if err := tx.Model(&Channel{}).Where("id = ?", current.BillingChannelID()).Count(&count).Error; err != nil {
					return err
				}
				if count != 0 {
					return errors.New("task channel accounting could not be applied")
				}
			}
		}
		if updated.Status == TaskStatusFailure && updated.Quota == 0 && (termination == nil || reservedQuota > 0) {
			updated.PrivateData.RefundedQuota = common.GetPointer(reservedQuota)
		}
		claim := tx.Model(&Task{}).Where("id = ? AND status = ? AND quota = ? AND attempt_generation = ?", task.ID, fromStatus, reservedQuota, task.AttemptGeneration).Select("*").Updates(&updated)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			return errMeasuredTaskCASLost
		}
		if termination != nil {
			billing := "not_billable"
			if reservedQuota > 0 || terminatedJob.BillingStatus == "refunded" || updated.PrivateData.RefundedQuota != nil && *updated.PrivateData.RefundedQuota > 0 {
				billing = "refunded"
			}
			changes := map[string]any{"status": "failed", "billing_status": billing, "quota": 0,
				"attempt_generation": updated.AttemptGeneration, "reference_recovery_stage": AsyncMediaAdminTerminatedStage,
				"lease_token": "", "lease_expires_at": 0, "next_attempt_at": 0, "request_cipher": nil, "channel_cipher": nil,
				"error_message": updated.FailReason}
			if reservedQuota > 0 {
				other := termination.other
				if other == nil {
					other = NewLogOther()
				}
				other.SetPublic("is_task", true)
				other.SetPublic("task_id", current.TaskID)
				other.SetPublic("billing_phase", "administrator_termination")
				other.SetPublic("pre_consumed_quota", reservedQuota)
				other.SetPublic("actual_quota", 0)
				other.SetPublic("reason", updated.FailReason)
				modelName := current.Properties.OriginModelName
				if bc := current.PrivateData.BillingContext; bc != nil && bc.OriginModelName != "" {
					modelName = bc.OriginModelName
				}
				if modelName == "" {
					modelName = terminatedJob.Model
				}
				refund := &Log{UserId: current.UserId, Username: user.Username, CreatedAt: common.GetTimestamp(),
					Type: LogTypeRefund, Content: updated.FailReason, TokenName: token.Name, ModelName: modelName,
					Quota: reservedQuota, ChannelId: current.BillingChannelID(), TokenId: current.PrivateData.TokenId,
					Group: current.Group, Other: other.JSONString()}
				ensureLogRequestId(refund)
				payload, err := common.Marshal(refund)
				if err != nil {
					return err
				}
				changes["refund_log_payload"] = string(payload)
			}
			if err := tx.Model(&AsyncMediaJob{}).Where("id = ?", terminatedJob.ID).Updates(changes).Error; err != nil {
				return err
			}
			var after AsyncMediaJob
			if err := tx.Where("id = ?", terminatedJob.ID).Take(&after).Error; err != nil {
				return err
			}
			if err := recordAsyncMediaEventTx(tx, after, "terminated", "", nil, ""); err != nil {
				return err
			}
			if err := recordAsyncMediaTransitionTx(tx, terminatedJob, after, nil, ""); err != nil {
				return err
			}
		} else if err := recordAsyncMediaNativeTaskTx(tx, *task, false); err != nil {
			return err
		}
		if current.PrivateData.MediaRetryState != nil || current.PrivateData.MediaRetryDecision != nil {
			outcome := "failed"
			if updated.Status == TaskStatusSuccess {
				outcome = "succeeded"
			}
			if err := MarkMediaRetryTaskOutcomeTx(tx, current.TaskID, outcome); err != nil {
				return err
			}
		}
		transactionBodySucceeded = true
		return nil
	})
	if err != nil && transactionBodySucceeded {
		// Commit may have succeeded before its acknowledgement was lost. Keep
		// the cache hold until the durable task state can be observed again;
		// adding it back here could make already spent quota available twice.
		return false, reservedQuota, err
	}
	cacheDelta := 0
	if err == nil {
		cacheDelta = -delta
		*task = updated
	}
	if task.PrivateData.BillingSource != "subscription" {
		syncDurableUserQuotaCache(task.UserId, cacheDelta+userCacheReserved, subject, generation)
	}
	if tokenKey != "" {
		syncDurableTokenQuotaCache(task.PrivateData.TokenId, tokenKey, cacheDelta+tokenCacheReserved)
	}
	if errors.Is(err, errMeasuredTaskCASLost) {
		return false, reservedQuota, nil
	}
	return err == nil, reservedQuota, err
}
