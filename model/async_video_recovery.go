package model

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrTaskAttemptConflict = errors.New("task attempt generation was superseded")

// ScheduleAsyncVideoReferenceRecovery fences the failed native operation and
// queues one local-reference recovery without releasing or repeating its hold.
func ScheduleAsyncVideoReferenceRecovery(ctx context.Context, task Task, job AsyncMediaJob, reason string) (bool, error) {
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
		if current.Status != task.Status || current.Status == TaskStatusFailure || current.Status == TaskStatusSuccess || current.AttemptGeneration != task.AttemptGeneration || current.AttemptGeneration < 0 || current.AttemptGeneration == math.MaxInt64 ||
			currentJob.AttemptGeneration != job.AttemptGeneration || currentJob.AttemptGeneration != current.AttemptGeneration || currentJob.ReferenceRetryCount != 0 || currentJob.Status != "submitted" || currentJob.BillingStatus != "reserved" || currentJob.RefundLogPayload != "" || currentJob.OutputObjectKey != "" || currentJob.ExpiresAt <= now ||
			current.TaskID != currentJob.TaskId || current.UserId != currentJob.UserId || current.ChannelId != currentJob.ChannelId || current.PrivateData.TokenId != currentJob.TokenId || current.Platform != "drama" || currentJob.Provider != "drama" || len(currentJob.RequestCipher) == 0 ||
			current.Quota <= 0 || current.Quota != currentJob.Quota || !current.PrivateData.AsyncMedia || !current.PrivateData.DurableBilling || !current.PrivateData.SubmitAccountingRecorded || current.PrivateData.UpstreamTaskID == "" || current.PrivateData.ResultURL != "" ||
			current.TaskID != task.TaskID || current.UserId != task.UserId || current.ChannelId != task.ChannelId || current.Quota != task.Quota || current.Properties.OriginModelName != task.Properties.OriginModelName || current.Properties.UpstreamModelName != task.Properties.UpstreamModelName || current.PrivateData.UpstreamTaskID != task.PrivateData.UpstreamTaskID || currentJob.TaskId != job.TaskId || currentJob.UserId != job.UserId || currentJob.TokenId != job.TokenId || currentJob.ChannelId != job.ChannelId || currentJob.Model != job.Model {
			return ErrTaskAttemptConflict
		}
		generation := current.AttemptGeneration + 1
		startedAt := current.StartTime
		if startedAt <= 0 {
			startedAt = current.SubmitTime
		}
		diagnostic := []rune(strings.TrimSpace(reason))
		current.PrivateData.VideoAttempts = append(current.PrivateData.VideoAttempts, VideoTaskAttempt{Generation: current.AttemptGeneration, UpstreamTaskID: current.PrivateData.UpstreamTaskID, Status: string(TaskStatusFailure), Reason: string(diagnostic[:min(len(diagnostic), common.LocalLogContentLimit)]), StartedAt: startedAt, FinishedAt: now})
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
		jobChanges := map[string]any{"attempt_generation": generation, "reference_retry_count": 1, "reference_recovery_stage": "prepare", "status": "queued", "storage_status": "pending", "dispatched_at": 0, "lease_token": "", "lease_expires_at": 0, "next_attempt_at": now, "error_message": ""}
		result = tx.Model(&AsyncMediaJob{}).Where("id = ? AND attempt_generation = ? AND reference_retry_count = ? AND status = ? AND billing_status = ?", currentJob.ID, job.AttemptGeneration, 0, "submitted", "reserved").Updates(jobChanges)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskAttemptConflict
		}
		return nil
	})
	if errors.Is(err, ErrTaskAttemptConflict) {
		return false, nil
	}
	return err == nil, err
}

// CommitAsyncVideoReferenceSubmission publishes a replacement native receipt
// only while both the recovery lease and task generation still belong to it.
func CommitAsyncVideoReferenceSubmission(ctx context.Context, job AsyncMediaJob, task Task, upstreamID string, data, state []byte) (bool, error) {
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
			currentJob.LeaseToken != job.LeaseToken || currentJob.LeaseExpiresAt <= time.Now().Unix() || currentJob.ReferenceRetryCount != 1 || currentJob.ReferenceRecoveryStage != "dispatched" || currentJob.DispatchedAt <= 0 || currentJob.BillingStatus != "reserved" || currentJob.Provider != "drama" || currentJob.ExpiresAt <= time.Now().Unix() ||
			current.Status == TaskStatusSuccess || current.Status == TaskStatusFailure || current.PrivateData.UpstreamTaskID != "" || current.PrivateData.ResultURL != "" ||
			current.TaskID != currentJob.TaskId || current.UserId != currentJob.UserId || current.ChannelId != currentJob.ChannelId || current.PrivateData.TokenId != currentJob.TokenId || current.Quota <= 0 || current.Quota != currentJob.Quota ||
			current.TaskID != task.TaskID || current.UserId != task.UserId || current.ChannelId != task.ChannelId || currentJob.TaskId != job.TaskId || currentJob.UserId != job.UserId || currentJob.TokenId != job.TokenId || currentJob.ChannelId != job.ChannelId {
			return ErrTaskAttemptConflict
		}
		current.PrivateData.UpstreamTaskID = upstreamID
		current.PrivateData.PluginState = append([]byte(nil), state...)
		current.PrivateData.PollFailures = 0
		current.PrivateData.VideoTerminalConfirmed = false
		current.PrivateData.VideoAttempts = append(current.PrivateData.VideoAttempts, VideoTaskAttempt{Generation: current.AttemptGeneration, UpstreamTaskID: upstreamID, Status: string(TaskStatusSubmitted), StartedAt: currentJob.DispatchedAt})
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
		return nil
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
			currentJob.LeaseToken != job.LeaseToken || currentJob.LeaseExpiresAt <= now || currentJob.ReferenceRetryCount != 1 || currentJob.ReferenceRecoveryStage != "dispatched" || currentJob.DispatchedAt <= 0 || currentJob.BillingStatus != "reserved" || currentJob.Provider != "drama" ||
			current.PrivateData.UpstreamTaskID != "" || current.PrivateData.ResultURL != "" || current.Quota <= 0 || current.Quota != currentJob.Quota || current.TaskID != currentJob.TaskId || current.UserId != currentJob.UserId || current.ChannelId != currentJob.ChannelId || current.PrivateData.TokenId != currentJob.TokenId {
			return ErrTaskAttemptConflict
		}
		diagnostic := []rune(strings.TrimSpace(reason))
		reason = string(diagnostic[:min(len(diagnostic), common.LocalLogContentLimit)])
		current.PrivateData.VideoTerminalConfirmed = false
		current.PrivateData.VideoAttempts = append(current.PrivateData.VideoAttempts, VideoTaskAttempt{Generation: current.AttemptGeneration, Status: string(TaskStatusUnknown), Reason: reason, StartedAt: currentJob.DispatchedAt, FinishedAt: now})
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
		return nil
	})
	if errors.Is(err, ErrTaskAttemptConflict) {
		return false, nil
	}
	return err == nil, err
}
