package model

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	"gorm.io/gorm"
)

var ErrTaskAttemptConflict = errors.New("task attempt generation was superseded")

// SelectAsyncVideoRecoveryChannel changes execution identity while the original
// billing identity and reservation stay pinned. Both task projections move in
// one transaction before the dispatch barrier.
func SelectAsyncVideoRecoveryChannel(ctx context.Context, job AsyncMediaJob, task Task, selected Task, requestCipher, channelCipher []byte) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		var queued AsyncMediaJob
		if err := lockForUpdate(tx).Where("id = ?", job.ID).Take(&queued).Error; err != nil {
			return err
		}
		if current.Status != TaskStatusQueued || current.AttemptGeneration != task.AttemptGeneration || queued.AttemptGeneration != task.AttemptGeneration || queued.LeaseToken != job.LeaseToken || queued.LeaseExpiresAt <= time.Now().Unix() || queued.DispatchedAt != 0 || queued.ReferenceRecoveryStage != "submit" || current.PrivateData.UpstreamTaskID != "" || queued.BillingStatus != "reserved" || current.Quota != task.Quota || current.BillingChannelID() != selected.BillingChannelID() || selected.ChannelId <= 0 || current.TaskID != queued.TaskId || current.UserId != queued.UserId || current.PrivateData.TokenId != queued.TokenId || string(current.Platform) != queued.Provider || selected.TaskID != current.TaskID || selected.UserId != current.UserId || selected.Platform != current.Platform || selected.Quota != current.Quota || selected.AttemptGeneration != current.AttemptGeneration {
			return ErrTaskAttemptConflict
		}
		if err := tx.Model(&current).Updates(map[string]any{"channel_id": selected.ChannelId, "private_data": selected.PrivateData}).Error; err != nil {
			return err
		}
		return tx.Model(&queued).Updates(map[string]any{"channel_id": selected.ChannelId, "request_cipher": requestCipher, "channel_cipher": channelCipher}).Error
	})
}

// DispatchAsyncVideoRecovery counts only committed sends. A restart after this
// transaction is uncertain execution, never another opportunity to submit.
func DispatchAsyncVideoRecovery(ctx context.Context, job AsyncMediaJob, task Task, decision *mediaretry.Decision, keyIndex int, referenceMode string) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		var before AsyncMediaJob
		if err := lockForUpdate(tx).Where("id = ?", job.ID).Take(&before).Error; err != nil {
			return err
		}
		if current.Status != TaskStatusQueued || current.AttemptGeneration != job.AttemptGeneration || before.AttemptGeneration != job.AttemptGeneration || before.LeaseToken != job.LeaseToken || before.LeaseExpiresAt <= time.Now().Unix() || before.ReferenceRecoveryStage != "submit" || before.DispatchedAt != 0 || before.BillingStatus != "reserved" || current.ChannelId != job.ChannelId || current.PrivateData.UpstreamTaskID != "" || current.Quota != before.Quota || !current.PrivateData.SubmitAccountingRecorded {
			return ErrTaskAttemptConflict
		}
		if decision != nil {
			state := mediaretry.State{TotalUsed: max(before.ReferenceRetryCount-1, 0), TotalLimit: 16}
			if current.PrivateData.MediaRetryState != nil {
				state = *current.PrivateData.MediaRetryState
			}
			if !decision.Matched || !mediaretry.IsResubmission(decision.Action) || decision.BlockedReason != "" || state.TotalUsed >= state.TotalLimit {
				return ErrTaskAttemptConflict
			}
			state = mediaretry.Consume(state, *decision)
			current.PrivateData.MediaRetryState = &state
			current.PrivateData.MediaRetryDecision = decision
			if err := tx.Model(&current).Update("private_data", current.PrivateData).Error; err != nil {
				return err
			}
		}
		if decision != nil {
			if err := MarkMediaRetryEventExecutedTx(tx, current.TaskID, *decision); err != nil {
				return err
			}
		}
		if err := tx.Model(&before).Updates(map[string]any{"reference_recovery_stage": "dispatched", "dispatched_at": time.Now().Unix()}).Error; err != nil {
			return err
		}
		var after AsyncMediaJob
		if err := tx.First(&after, before.ID).Error; err != nil {
			return err
		}
		return recordAsyncMediaTransitionTx(tx, before, after, &keyIndex, referenceMode)
	})
}

// ScheduleAsyncVideoReferenceRecovery fences the failed native operation and
// queues one recovery without releasing or repeating its hold. Capacity failures
// reuse the original inputs; reference fetch failures first prepare local inputs.
type AsyncVideoRecoveryPlan struct {
	Decision           mediaretry.Decision
	Fact               mediaretry.Fact
	State              mediaretry.State
	RejectedSubmission bool
	PreparationFailed  bool
	RequestCipher      []byte
}

func ScheduleAsyncVideoReferenceRecovery(ctx context.Context, task Task, job AsyncMediaJob, reason string, localReferences bool, plans ...AsyncVideoRecoveryPlan) (bool, error) {
	policyRecovery := len(plans) == 1 && plans[0].Decision.Matched && (mediaretry.IsResubmission(plans[0].Decision.Action) || plans[0].Decision.BlockedReason == "resubmissions_paused" && mediaretry.IsResubmission(plans[0].Decision.RequestedAction))
	if len(plans) > 1 || len(plans) == 1 && !policyRecovery {
		return false, ErrImageConflict
	}
	if task.ID <= 0 || job.ID <= 0 || task.TaskID == "" || task.TaskID != job.TaskId || task.UserId != job.UserId || task.ChannelId != job.ChannelId || task.PrivateData.TokenId != job.TokenId || strings.TrimSpace(reason) == "" {
		return false, ErrImageConflict
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		var currentJob AsyncMediaJob
		if err := lockForUpdate(tx).Where("id = ?", job.ID).Take(&currentJob).Error; err != nil {
			return err
		}
		now := time.Now().Unix()
		rejected := policyRecovery && plans[0].RejectedSubmission && currentJob.ReferenceRecoveryStage == "dispatched" && currentJob.DispatchedAt > 0 && current.Status == TaskStatusQueued && current.PrivateData.UpstreamTaskID == ""
		preparationFailed := policyRecovery && plans[0].PreparationFailed && currentJob.ReferenceRecoveryStage == "prepare" && currentJob.DispatchedAt == 0 && current.Status == TaskStatusQueued && current.PrivateData.UpstreamTaskID == ""
		if current.Status != task.Status || current.Status == TaskStatusFailure || current.Status == TaskStatusSuccess || current.PrivateData.VideoReconciliation != nil || current.AttemptGeneration != task.AttemptGeneration || current.AttemptGeneration < 0 || current.AttemptGeneration == math.MaxInt64 ||
			currentJob.AttemptGeneration != job.AttemptGeneration || currentJob.AttemptGeneration != current.AttemptGeneration || (!policyRecovery && currentJob.ReferenceRetryCount != 0 || currentJob.ReferenceRetryCount >= 1000) || (!rejected && !preparationFailed && currentJob.Status != "submitted") || currentJob.BillingStatus != "reserved" || currentJob.RefundLogPayload != "" || currentJob.OutputObjectKey != "" || currentJob.ExpiresAt <= now ||
			current.TaskID != currentJob.TaskId || current.UserId != currentJob.UserId || current.ChannelId != currentJob.ChannelId || current.PrivateData.TokenId != currentJob.TokenId || (string(current.Platform) != currentJob.Provider || !policyRecovery && current.Platform != "drama") || len(currentJob.RequestCipher) == 0 ||
			current.Quota <= 0 || current.Quota != currentJob.Quota || !current.PrivateData.AsyncMedia || !current.PrivateData.DurableBilling || !current.PrivateData.SubmitAccountingRecorded || (!rejected && !preparationFailed && current.PrivateData.UpstreamTaskID == "") || current.PrivateData.ResultURL != "" || current.PrivateData.MediaRetryFact != nil && current.PrivateData.MediaRetryFact.HasOutput ||
			current.TaskID != task.TaskID || current.UserId != task.UserId || current.ChannelId != task.ChannelId || current.Quota != task.Quota || current.Properties.OriginModelName != task.Properties.OriginModelName || current.Properties.UpstreamModelName != task.Properties.UpstreamModelName || current.PrivateData.UpstreamTaskID != task.PrivateData.UpstreamTaskID || currentJob.TaskId != job.TaskId || currentJob.UserId != job.UserId || currentJob.TokenId != job.TokenId || currentJob.ChannelId != job.ChannelId || currentJob.Model != job.Model {
			return ErrTaskAttemptConflict
		}
		generation := current.AttemptGeneration + 1
		if current.PrivateData.BillingChannelID == 0 {
			current.PrivateData.BillingChannelID = current.ChannelId
		}
		if policyRecovery {
			decision := plans[0].Decision
			current.PrivateData.MediaRetryDecision = &decision
			fact, state := plans[0].Fact, plans[0].State
			current.PrivateData.MediaRetryFact, current.PrivateData.MediaRetryState = &fact, &state
			current.PrivateData.MediaRetryFailedAt = now
			event, err := NewMediaRetryEvent(current.TaskID, current.AttemptGeneration, plans[0].Fact, decision, plans[0].State)
			if err != nil {
				return err
			}
			if err := RecordMediaRetryEventTx(tx, event); err != nil {
				return err
			}
		}
		startedAt := current.StartTime
		if startedAt <= 0 {
			startedAt = current.SubmitTime
		}
		diagnostic := []rune(strings.TrimSpace(reason))
		current.PrivateData.VideoAttempts = append(current.PrivateData.VideoAttempts, VideoTaskAttempt{ChannelID: current.ChannelId, Decision: current.PrivateData.MediaRetryDecision, Generation: current.AttemptGeneration, UpstreamTaskID: current.PrivateData.UpstreamTaskID, Status: string(TaskStatusFailure), Reason: string(diagnostic[:min(len(diagnostic), common.LocalLogContentLimit)]), StartedAt: startedAt, FinishedAt: now})
		current.PrivateData.UpstreamTaskID, current.PrivateData.ResultURL = "", ""
		current.PrivateData.PollFailures = 0
		current.PrivateData.VideoTerminalConfirmed = false
		current.PrivateData.PluginState, current.PrivateData.UpstreamAsyncResponse = nil, nil
		taskChanges := map[string]any{"attempt_generation": generation, "private_data": current.PrivateData, "status": TaskStatusQueued, "data": nil, "fail_reason": "", "finish_time": 0, "progress": "0%", "updated_at": now}
		result := tx.Model(&Task{}).Where("id = ? AND status = ? AND attempt_generation = ? AND quota = ?", current.ID, task.Status, task.AttemptGeneration, task.Quota).Updates(taskChanges)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskAttemptConflict
		}
		stage, nextAttempt := "prepare", now
		if !localReferences {
			stage, nextAttempt = "submit", now+5
		}
		jobChanges := map[string]any{"attempt_generation": generation, "reference_retry_count": currentJob.ReferenceRetryCount + 1, "reference_recovery_stage": stage, "status": "queued", "storage_status": "pending", "dispatched_at": 0, "lease_token": "", "lease_expires_at": 0, "next_attempt_at": nextAttempt, "error_message": ""}
		if policyRecovery {
			jobChanges["request_cipher"] = plans[0].RequestCipher
			jobChanges["next_attempt_at"] = now + int64(plans[0].Decision.DelaySeconds)
		}
		result = tx.Model(&AsyncMediaJob{}).Where("id = ? AND attempt_generation = ? AND reference_retry_count = ? AND status = ? AND billing_status = ?", currentJob.ID, job.AttemptGeneration, currentJob.ReferenceRetryCount, currentJob.Status, "reserved").Updates(jobChanges)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskAttemptConflict
		}
		if err := recordAsyncMediaEventTx(tx, currentJob, "upstream_failed", "", nil, ""); err != nil {
			return err
		}
		var after AsyncMediaJob
		if err := tx.Where("id = ?", currentJob.ID).Take(&after).Error; err != nil {
			return err
		}
		return recordAsyncMediaTransitionTx(tx, currentJob, after, nil, "")
	})
	if errors.Is(err, ErrTaskAttemptConflict) {
		return false, nil
	}
	return err == nil, err
}

// CommitAsyncVideoReferenceSubmission publishes a replacement native receipt
// only while both the recovery lease and task generation still belong to it.
func CommitAsyncVideoReferenceSubmission(ctx context.Context, job AsyncMediaJob, task Task, upstreamID string, data, state []byte, confirmedOutput ...bool) (bool, error) {
	if task.ID <= 0 || job.ID <= 0 || job.LeaseToken == "" || upstreamID == "" || task.TaskID != job.TaskId || task.UserId != job.UserId || task.ChannelId != job.ChannelId || task.PrivateData.TokenId != job.TokenId {
		return false, ErrImageConflict
	}
	for _, payload := range [][]byte{data, state} {
		if len(payload) == 0 {
			continue
		}
		var decoded any
		if err := common.Unmarshal(payload, &decoded); err != nil {
			return false, err
		}
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		var currentJob AsyncMediaJob
		if err := lockForUpdate(tx).Where("id = ?", job.ID).Take(&currentJob).Error; err != nil {
			return err
		}
		if current.AttemptGeneration != task.AttemptGeneration || currentJob.AttemptGeneration != job.AttemptGeneration || current.AttemptGeneration != currentJob.AttemptGeneration ||
			currentJob.LeaseToken != job.LeaseToken || currentJob.LeaseExpiresAt <= time.Now().Unix() || currentJob.ReferenceRetryCount < 1 || currentJob.ReferenceRecoveryStage != "dispatched" || currentJob.DispatchedAt <= 0 || currentJob.BillingStatus != "reserved" || string(current.Platform) != currentJob.Provider || currentJob.ExpiresAt <= time.Now().Unix() ||
			current.Status == TaskStatusSuccess || current.Status == TaskStatusFailure || current.PrivateData.UpstreamTaskID != "" || current.PrivateData.ResultURL != "" ||
			current.TaskID != currentJob.TaskId || current.UserId != currentJob.UserId || current.ChannelId != currentJob.ChannelId || current.PrivateData.TokenId != currentJob.TokenId || current.Quota <= 0 || current.Quota != currentJob.Quota ||
			current.TaskID != task.TaskID || current.UserId != task.UserId || current.ChannelId != task.ChannelId || currentJob.TaskId != job.TaskId || currentJob.UserId != job.UserId || currentJob.TokenId != job.TokenId || currentJob.ChannelId != job.ChannelId {
			return ErrTaskAttemptConflict
		}
		current.PrivateData.UpstreamTaskID = upstreamID
		if len(confirmedOutput) == 1 && confirmedOutput[0] {
			current.PrivateData.MediaRetryFact = &mediaretry.Fact{MediaType: "video", Provider: currentJob.Provider, Model: currentJob.Model, ChannelID: current.ChannelId, Stage: "billing", ErrorCode: "upstream_output_confirmed", Accepted: true, Terminal: true, HasOutput: true, HasUpstreamTask: true}
		}
		current.PrivateData.PluginState = append([]byte(nil), state...)
		current.PrivateData.PollFailures = 0
		if current.PrivateData.MediaRetryState != nil {
			if err := MarkMediaRetryTaskOutcomeTx(tx, current.TaskID, "unknown"); err != nil {
				return err
			}
		}
		current.PrivateData.VideoTerminalConfirmed = false
		current.PrivateData.VideoAttempts = append(current.PrivateData.VideoAttempts, VideoTaskAttempt{ChannelID: current.ChannelId, Decision: current.PrivateData.MediaRetryDecision, Generation: current.AttemptGeneration, UpstreamTaskID: upstreamID, Status: string(TaskStatusSubmitted), StartedAt: currentJob.DispatchedAt})
		var taskData any
		if current.PrivateData.UpstreamAsync != nil {
			current.PrivateData.UpstreamAsyncResponse = append([]byte(nil), data...)
		} else if len(data) > 0 {
			taskData = string(data)
			// SQLite must return bytes to Task.Data's RawMessage scanner;
			// PostgreSQL's simple protocol must receive JSON text, not bytea.
			if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
				taskData = append([]byte(nil), data...)
			}
		}
		result := tx.Model(&Task{}).Where("id = ? AND attempt_generation = ? AND status = ?", current.ID, task.AttemptGeneration, current.Status).Updates(map[string]any{"private_data": current.PrivateData, "data": taskData, "status": TaskStatusSubmitted, "fail_reason": "", "progress": "0%", "finish_time": 0, "updated_at": time.Now().Unix()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskAttemptConflict
		}
		result = tx.Model(&AsyncMediaJob{}).Where("id = ? AND lease_token = ? AND attempt_generation = ? AND reference_recovery_stage = ?", currentJob.ID, job.LeaseToken, job.AttemptGeneration, "dispatched").Updates(map[string]any{"reference_recovery_stage": "waiting", "status": "submitted", "error_message": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskAttemptConflict
		}
		var after AsyncMediaJob
		if err := tx.Where("id = ?", currentJob.ID).Take(&after).Error; err != nil {
			return err
		}
		if err := recordAsyncMediaEventTx(tx, after, "submitted", "", nil, ""); err != nil {
			return err
		}
		return recordAsyncMediaTransitionTx(tx, currentJob, after, nil, "")
	})
	if errors.Is(err, ErrTaskAttemptConflict) {
		return false, nil
	}
	return err == nil, err
}

// MarkAsyncVideoReferenceSubmissionUnknown retains the original hold when the
// replacement operation may have started without returning a durable receipt.
func MarkAsyncVideoReferenceSubmissionUnknown(ctx context.Context, job AsyncMediaJob, task Task, reason string) (bool, error) {
	if task.ID <= 0 || job.ID <= 0 || job.LeaseToken == "" || strings.TrimSpace(reason) == "" || task.TaskID != job.TaskId || task.UserId != job.UserId || task.ChannelId != job.ChannelId || task.PrivateData.TokenId != job.TokenId {
		return false, ErrImageConflict
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		var currentJob AsyncMediaJob
		if err := lockForUpdate(tx).Where("id = ?", job.ID).Take(&currentJob).Error; err != nil {
			return err
		}
		now := time.Now().Unix()
		if current.AttemptGeneration <= 0 || current.AttemptGeneration != task.AttemptGeneration || currentJob.AttemptGeneration != job.AttemptGeneration || current.AttemptGeneration != currentJob.AttemptGeneration || current.Status != task.Status || current.Status == TaskStatusSuccess || current.Status == TaskStatusFailure ||
			currentJob.LeaseToken != job.LeaseToken || currentJob.LeaseExpiresAt <= now || currentJob.ReferenceRetryCount < 1 || currentJob.ReferenceRecoveryStage != "dispatched" || currentJob.DispatchedAt <= 0 || currentJob.BillingStatus != "reserved" || string(current.Platform) != currentJob.Provider ||
			current.PrivateData.UpstreamTaskID != "" || current.PrivateData.ResultURL != "" || current.Quota <= 0 || current.Quota != currentJob.Quota || current.TaskID != currentJob.TaskId || current.UserId != currentJob.UserId || current.ChannelId != currentJob.ChannelId || current.PrivateData.TokenId != currentJob.TokenId {
			return ErrTaskAttemptConflict
		}
		diagnostic := []rune(strings.TrimSpace(reason))
		reason = string(diagnostic[:min(len(diagnostic), common.LocalLogContentLimit)])
		if current.PrivateData.MediaRetryState != nil {
			if err := MarkMediaRetryTaskOutcomeTx(tx, current.TaskID, "unknown"); err != nil {
				return err
			}
		}
		current.PrivateData.VideoTerminalConfirmed = false
		current.PrivateData.VideoAttempts = append(current.PrivateData.VideoAttempts, VideoTaskAttempt{ChannelID: current.ChannelId, Decision: current.PrivateData.MediaRetryDecision, Generation: current.AttemptGeneration, Status: string(TaskStatusUnknown), Reason: reason, StartedAt: currentJob.DispatchedAt, FinishedAt: now})
		result := tx.Model(&Task{}).Where("id = ? AND status = ? AND attempt_generation = ?", current.ID, task.Status, task.AttemptGeneration).Updates(map[string]any{"status": TaskStatusUnknown, "fail_reason": reason, "progress": "100%", "private_data": current.PrivateData, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskAttemptConflict
		}
		result = tx.Model(&AsyncMediaJob{}).Where("id = ? AND lease_token = ? AND attempt_generation = ? AND reference_recovery_stage = ?", currentJob.ID, job.LeaseToken, job.AttemptGeneration, "dispatched").Updates(map[string]any{"status": "execution_unknown", "billing_status": "unknown", "reference_recovery_stage": "unknown", "error_message": reason, "next_attempt_at": 0})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskAttemptConflict
		}
		var after AsyncMediaJob
		if err := tx.Where("id = ?", currentJob.ID).Take(&after).Error; err != nil {
			return err
		}
		return recordAsyncMediaTransitionTx(tx, currentJob, after, nil, "")
	})
	if errors.Is(err, ErrTaskAttemptConflict) {
		return false, nil
	}
	return err == nil, err
}

// RecordAsyncVideoRetryDecision keeps diagnostic samples and counters within the
// task generation; a stale poller cannot overwrite a later dispatch decision.
func RecordAsyncVideoRetryDecision(ctx context.Context, task Task, job AsyncMediaJob, fact mediaretry.Fact, decision mediaretry.Decision, state mediaretry.State, consume bool, preparationOnly ...bool) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		var queued AsyncMediaJob
		if err := lockForUpdate(tx).Where("id = ?", job.ID).Take(&queued).Error; err != nil {
			return err
		}
		if current.TaskID != queued.TaskId || current.UserId != queued.UserId || current.AttemptGeneration != task.AttemptGeneration || queued.AttemptGeneration != job.AttemptGeneration || current.Status != task.Status || current.PrivateData.UpstreamTaskID != task.PrivateData.UpstreamTaskID || job.LeaseToken != "" && queued.LeaseToken != job.LeaseToken {
			return ErrTaskAttemptConflict
		}
		if previous := current.PrivateData.MediaRetryFact; previous != nil && previous.HasOutput {
			fact.HasOutput = true
		}
		event, err := NewMediaRetryEvent(task.TaskID, task.AttemptGeneration, fact, decision, state)
		if err != nil {
			return err
		}
		if err := RecordMediaRetryEventTx(tx, event); err != nil {
			return err
		}
		current.PrivateData.MediaRetryDecision = &decision
		current.PrivateData.MediaRetryFact = &fact
		if current.PrivateData.MediaRetryState == nil {
			current.PrivateData.MediaRetryState = &state
		}
		if !consume {
			current.PrivateData.MediaRetryFailedAt = time.Now().Unix()
		}
		if consume {
			previousTotal := state.TotalUsed
			state = mediaretry.Consume(state, decision)
			if len(preparationOnly) == 1 && preparationOnly[0] {
				state.TotalUsed = previousTotal
			}
			current.PrivateData.MediaRetryState = &state
			if err := MarkMediaRetryEventExecutedTx(tx, task.TaskID, decision); err != nil {
				return err
			}
		}
		return tx.Model(&current).Update("private_data", current.PrivateData).Error
	})
}

// HoldAsyncVideoRecovery leaves a confirmed failed operation's existing hold
// available for an administrator. It never manufactures a new upstream receipt.
func HoldAsyncVideoRecovery(ctx context.Context, job AsyncMediaJob, task Task, reason string, confirmedRejection ...bool) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		var queued AsyncMediaJob
		if err := lockForUpdate(tx).Where("id = ?", job.ID).Take(&queued).Error; err != nil {
			return err
		}
		if current.AttemptGeneration != task.AttemptGeneration || queued.AttemptGeneration != job.AttemptGeneration || current.TaskID != queued.TaskId || current.Status != TaskStatusQueued || queued.DispatchedAt != 0 && !(len(confirmedRejection) == 1 && confirmedRejection[0] && queued.ReferenceRecoveryStage == "dispatched") || queued.BillingStatus != "reserved" || queued.LeaseToken != job.LeaseToken || queued.LeaseExpiresAt <= time.Now().Unix() {
			return ErrTaskAttemptConflict
		}
		if err := tx.Model(&current).Updates(map[string]any{"status": TaskStatusUnknown, "fail_reason": reason}).Error; err != nil {
			return err
		}
		if err := tx.Model(&queued).Updates(map[string]any{"status": "execution_unknown", "reference_recovery_stage": "manual", "next_attempt_at": 0, "error_message": reason}).Error; err != nil {
			return err
		}
		return nil
	})
}
