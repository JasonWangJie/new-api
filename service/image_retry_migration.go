package service

import (
	"context"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// BootstrapPendingImageRetry restores the evidence needed to apply a new rule
// to a retry queued before policy checkpoints existed. Diagnostic words alone
// cannot prove that a previous generation was rejected.
func BootstrapPendingImageRetry(ctx context.Context, task *model.AsyncImageTask, request AsyncImageRequest, cfg ImageRuntimeConfig) error {
	if task == nil || task.RetryPolicyState != "" || task.RetryCount <= 0 || task.DispatchedAt != 0 || task.UpstreamTaskId != "" || task.UpstreamSucceededAt > 0 || task.ImageCount > 0 || task.ResultCount > 0 || request.Completion != nil || task.Status != model.ImageTaskQueued && task.Status != model.ImageTaskInvoking {
		return nil
	}
	var attempts []ImageChannelAttempt
	if task.Attempts != "" {
		if err := common.UnmarshalJsonStr(task.Attempts, &attempts); err != nil {
			return err
		}
	}
	source := *task
	failure := &AsyncImageFailure{Code: task.PublicErrorCode, InternalCode: task.ErrorCode, Message: task.ErrorMessage}
	recordedAt := task.UpdatedAt
	found := false
	for index := len(attempts) - 1; index >= 0; index-- {
		attempt := attempts[index]
		if attempt.FinishedAt <= 0 {
			continue
		}
		found = true
		source.ChannelId = attempt.ChannelId
		if attempt.Provider != "" {
			source.Provider = attempt.Provider
		}
		source.UpstreamTaskId = attempt.UpstreamTaskID
		if attempt.Dispatched {
			source.DispatchedAt = max(1, attempt.StartedAt)
		}
		failure.HTTPStatus, failure.ProviderCode = attempt.HTTPStatus, attempt.ProviderCode
		failure.SubmissionRejected, failure.SynchronousRetryableResponse = attempt.SubmissionRejected, attempt.SynchronousRetryableResponse
		failure.ExecutionUnknown = attempt.Dispatched && (attempt.TimedOut || !attempt.SubmissionRejected && !attempt.SynchronousRetryableResponse)
		if failure.Code == 0 {
			failure.Code = attempt.Code
		}
		if failure.Message == "" {
			failure.Message = attempt.ErrorMessage
		}
		recordedAt = attempt.FinishedAt
		break
	}
	if !found {
		// A historical counter without evidence is insufficient to authorize a
		// new submission rule. Its existing built-in behavior remains bounded.
		failure.ExecutionUnknown = true
	}
	checkpoint, err := ImageRetryFailureContext(source, failure, &request, cfg)
	if err != nil {
		return err
	}
	checkpoint.ReservedRetry, checkpoint.BuiltinPending = true, true
	checkpoint.State.TotalUsed = max(0, task.RetryCount-1)
	if checkpoint.Fact.Unknown {
		// The current task has no live receipt. Historical IDs remain in the
		// source audit; a rule must not pretend they are an active poll target.
		checkpoint.Fact.HasUpstreamTask = false
	}
	checkpoint.RecordedAt = recordedAt
	if checkpoint.RecordedAt <= 0 {
		checkpoint.RecordedAt = time.Now().Unix()
	}
	encoded, err := common.Marshal(checkpoint)
	if err != nil {
		return err
	}
	if err := model.TransitionImageTask(ctx, *task, map[string]any{"retry_policy_state": string(encoded)}, "retry_policy_bootstrapped", "", ""); err != nil {
		return err
	}
	return model.DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error
}
