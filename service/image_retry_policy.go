package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
)

var errImageRetryPolicyChanged = errors.New("image retry policy changed before dispatch")
var errImageRetryPolicyUnavailable = errors.New("image retry policy is unavailable")

// validateAsyncImageDispatch repeats read-only authorization after potentially
// slow reference preparation. It checks the selected account, never acquires a
// second execution slot or re-reserves the original hold.
func validateAsyncImageDispatch(ctx context.Context, task model.AsyncImageTask, request AsyncImageRequest, selected model.Channel, account ImageAccount, cfg ImageRuntimeConfig) error {
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", task.TokenId, task.UserId).Take(&token).Error; err != nil {
		return err
	}
	var err error
	request.Reservation, err = model.GetAsyncImageReservation(ctx, task)
	if err != nil {
		return err
	}
	if request.Reservation != nil && request.Reservation.Status != "reserved" {
		return errors.New("image reservation changed before dispatch")
	}
	policy, _, err := ValidateAsyncImageEligibility(ctx, token, request, cfg)
	if err != nil {
		return err
	}
	if policy.Group != task.Group || request.MixedRouting != nil && (policy.Platform != "mixed" || policy.Group != request.MixedRouting.Group || ImagePoolBindingKey(policy, request.Model, AsyncImageRoutingResolution(request)) != request.MixedRouting.BindingKey) {
		return errors.New("image routing identity changed before dispatch")
	}
	channels, err := ImageCandidateChannels(ctx, policy, request.Model, AsyncImageRoutingResolution(request))
	if err != nil {
		return err
	}
	index := slices.IndexFunc(channels, func(channel model.Channel) bool { return channel.Id == selected.Id })
	if index < 0 {
		return errors.New("selected image channel is no longer available")
	}
	channel := channels[index]
	capability, prior := ImageChannelCapability(channel, request.Model), ImageChannelCapability(selected, request.Model)
	if channel.Type != selected.Type || channel.GetBaseURL() != selected.GetBaseURL() || capability.Provider != prior.Provider || capability.Protocol != prior.Protocol || request.Provider != "" && capability.Provider != request.Provider {
		return errors.New("selected image channel changed before dispatch")
	}
	if policy.Platform == "mixed" || channel.Type == constant.ChannelTypeTabTab || channel.Type == constant.ChannelTypeNewAPI && channel.GetSetting().TianyueMediaCompatibility {
		if !MixedImageChannelRequestSupported(channel, request) {
			return errors.New("selected image channel no longer supports this request")
		}
	} else if !ImageCapabilitySupportsRequest(capability, request) {
		return errors.New("selected image channel no longer supports this request")
	}
	currentModel, err := ResolveImageChannelUpstreamModel(channel, request.Model)
	if err != nil {
		return err
	}
	priorModel, err := ResolveImageChannelUpstreamModel(selected, request.Model)
	if err != nil || currentModel != priorModel {
		return errors.New("selected image model mapping changed before dispatch")
	}
	if !slices.ContainsFunc(ImageChannelAccounts(channel, ImageAccountRouting{}), func(candidate ImageAccount) bool { return candidate.Fingerprint == account.Fingerprint }) {
		return errors.New("selected image account is no longer enabled")
	}
	deadline := model.AsyncImageExecutionDeadline(task, cfg.ExecutionTimeout)
	if deadline > 0 && time.Now().Unix() >= deadline {
		return errors.New("image execution deadline reached before dispatch")
	}
	if task.RetryCount > cfg.TotalRetries {
		return errors.New("image retry budget changed before dispatch")
	}
	return nil
}

// ImageRetryFailureContext separates protocol evidence from editable labels.
// A keyword match cannot turn a dispatched, unconfirmed request into a refusal.
func ImageRetryFailureContext(task model.AsyncImageTask, failure *AsyncImageFailure, request *AsyncImageRequest, cfg ImageRuntimeConfig) (model.ImageRetryPolicyState, error) {
	checkpoint := model.ImageRetryPolicyState{}
	preparationFailed := false
	if task.RetryPolicyState != "" {
		if err := common.UnmarshalJsonStr(task.RetryPolicyState, &checkpoint); err != nil {
			return checkpoint, err
		}
		// A selected local action can fail while fetching its inputs, before
		// generation dispatch. That attempted step still uses its own budget.
		if checkpoint.Pending && checkpoint.ReservedRetry && mediaretry.IsResubmission(checkpoint.Decision.Action) && task.ChannelId > 0 && task.DispatchedAt == 0 && task.UpstreamTaskId == "" && task.UpstreamSucceededAt == 0 && task.ImageCount == 0 && task.ResultCount == 0 {
			checkpoint.State = mediaretry.Consume(checkpoint.State, checkpoint.Decision)
			preparationFailed = true
		}
	}
	fact := mediaretry.Fact{MediaType: "image", Provider: task.Provider, Model: task.Model, ChannelID: task.ChannelId,
		Stage: "submit", HTTPStatus: failure.HTTPStatus, ErrorCode: failure.ProviderCode, Message: failure.Message,
		Unknown: failure.ExecutionUnknown, HasUpstreamTask: task.UpstreamTaskId != "", HasOutput: task.UpstreamSucceededAt > 0 || task.ImageCount > 0 || task.ResultCount > 0,
		Terminal: failure.UpstreamTerminal || failure.SubmissionRejected || failure.SynchronousRetryableResponse,
		Deadline: model.AsyncImageExecutionDeadline(task, cfg.ExecutionTimeout)}
	fact.ImageEvidence = &mediaretry.ImageFailureEvidence{FailureAttempt: task.Version, Code: failure.Code, InternalCode: failure.InternalCode,
		RetryAfterSeconds: int64(failure.RetryAfter / time.Second),
		UpstreamTaskID:    task.UpstreamTaskId, DispatchedAt: task.DispatchedAt, UpstreamTerminal: failure.UpstreamTerminal,
		RetrySameChannel: failure.RetrySameChannel, RetryableHTTPResponse: failure.RetryableHTTPResponse,
		SynchronousRetryableResponse: failure.SynchronousRetryableResponse, SubmissionRejected: failure.SubmissionRejected,
		ReferenceRejectionVerified: failure.ReferenceRejectionVerified, ReferenceTransportFallback: failure.ReferenceTransportFallback}
	if fact.ErrorCode == "" {
		fact.ErrorCode = failure.InternalCode
	}
	if task.ChannelId == 0 {
		fact.Stage = "routing"
	} else if task.DispatchedAt == 0 {
		fact.Stage = "prepare"
	}
	fact.Accepted = fact.HasUpstreamTask || task.DispatchedAt > 0 && !fact.Terminal
	if fact.HasUpstreamTask && !fact.Terminal {
		fact.Stage = "poll"
	}
	if request != nil {
		fact.HasReferences, fact.LocalSupported = request.ReferenceImageCount() > 0, true
		fact.HasOutput = fact.HasOutput || request.Completion != nil
	}
	if fact.HasOutput {
		fact.Stage = "download"
		if failure.InternalCode == "bill_preparation_failed" || failure.InternalCode == "billing_usage_missing" {
			fact.Stage = "billing"
		}
	}
	switch failure.Code {
	case 601:
		fact.Category = "content_policy"
	case 602:
		fact.Category = "reference_download"
	case 603:
		fact.Category = "capacity"
	case 604, 611, 612, 613:
		fact.Category = "invalid_input"
	case 605, 606:
		fact.Category = "transient"
	case 608:
		fact.Category = "execution_unknown"
	default:
		fact.Category = "unclassified"
	}
	if fact.HasOutput {
		fact.Category = "storage"
		if fact.Stage == "billing" {
			fact.Category = "billing"
		}
	}
	checkpoint.Fact = NormalizeMediaRetryFact(fact)
	checkpoint.FailureAttempt, checkpoint.SourceDispatchedAt, checkpoint.SourceUpstreamTaskID = task.Version, task.DispatchedAt, task.UpstreamTaskId
	checkpoint.RecordedAt, checkpoint.FailureCode, checkpoint.FailureInternalCode = time.Now().Unix(), failure.Code, failure.InternalCode
	checkpoint.State.TotalUsed, checkpoint.State.TotalLimit, checkpoint.State.Now = task.RetryCount, cfg.TotalRetries, time.Now().Unix()
	if preparationFailed {
		// Preparing a selected step consumes its rule budget, but the reserved
		// generation attempt was never sent and can be used by the next step.
		checkpoint.State.TotalUsed = max(0, task.RetryCount-1)
	}
	checkpoint.Pending = false
	checkpoint.BuiltinPending = false
	checkpoint.ReservedRetry = false
	return checkpoint, nil
}

// imageFailurePlan contains a read-only decision and its atomic state changes.
// Preview builds the same plan; only workers commit it under the task lease.
type imageFailurePlan struct {
	Updates                             map[string]any
	Event                               *model.MediaRetryEvent
	EventType, EventMessage, OutboxKind string
	Audit                               ImageRetryDecision
	Decision                            mediaretry.Decision
}

func commitImagePolicyDecision(ctx context.Context, task model.AsyncImageTask, failure *AsyncImageFailure, cfg ImageRuntimeConfig, checkpoint model.ImageRetryPolicyState, updates map[string]any) error {
	plan, err := buildImagePolicyDecision(ctx, task, failure, cfg, checkpoint, updates)
	if err != nil {
		return err
	}
	return model.TransitionImageTask(ctx, task, plan.Updates, plan.EventType, plan.EventMessage, plan.OutboxKind, plan.Event)
}

func buildImagePolicyDecision(ctx context.Context, task model.AsyncImageTask, failure *AsyncImageFailure, cfg ImageRuntimeConfig, checkpoint model.ImageRetryPolicyState, updates map[string]any) (imageFailurePlan, error) {
	decision := checkpoint.Decision
	now := time.Now().Unix()
	status, outbox := model.ImageTaskExecutionUnknown, ""
	if checkpoint.Fact.HasOutput {
		status = model.ImageTaskStorageFailed
		if checkpoint.Fact.Stage == "billing" {
			status = model.ImageTaskBillingFailed
		}
	}
	updates["request_cipher"], updates["finished_at"] = task.RequestCipher, now
	updates["next_attempt_at"], updates["lease_token"], updates["lease_expires_at"] = int64(0), "", 0
	checkpoint.Pending = false
	switch decision.Action {
	case mediaretry.SameChannel, mediaretry.OtherChannel, mediaretry.LocalSameChannel, mediaretry.LocalOtherChannel:
		if checkpoint.Fact.Accepted && !checkpoint.Fact.Terminal || checkpoint.Fact.Unknown || checkpoint.Fact.HasOutput {
			return imageFailurePlan{}, errors.New("media retry decision cannot replay an unconfirmed or completed image request")
		}
		if task.DispatchedAt > 0 {
			canRetry, err := canRetryAsyncImageSubmission(ctx, task, cfg)
			if err != nil {
				return imageFailurePlan{}, err
			}
			if !canRetry {
				checkpoint.Decision.Action, checkpoint.Decision.BlockedReason = mediaretry.ManualReview, "request_replay_unavailable"
				return buildImagePolicyDecision(ctx, task, failure, cfg, checkpoint, updates)
			}
		}
		status, outbox, checkpoint.Pending = model.ImageTaskQueued, "execute", true
		if !checkpoint.ReservedRetry {
			updates["retry_count"] = task.RetryCount + 1
		}
		checkpoint.ReservedRetry = true
		updates["channel_id"], updates["upstream_task_id"], updates["dispatched_at"], updates["selected_channel_cipher"] = 0, "", 0, nil
	case mediaretry.PollSameJob:
		if task.UpstreamTaskId == "" {
			checkpoint.Decision.Action, checkpoint.Decision.BlockedReason = mediaretry.ManualReview, "upstream_task_unavailable"
			return buildImagePolicyDecision(ctx, task, failure, cfg, checkpoint, updates)
		}
		if task.ChannelId <= 0 || len(task.SelectedChannelCipher) == 0 {
			checkpoint.Decision.Action, checkpoint.Decision.BlockedReason = mediaretry.ManualReview, "frozen_channel_unavailable"
			return buildImagePolicyDecision(ctx, task, failure, cfg, checkpoint, updates)
		}
		status, outbox = model.ImageTaskInvoking, "execute"
		checkpoint.Pending = true
	case mediaretry.ResumeDownload, mediaretry.ResumeBilling:
		if decision.Action == mediaretry.ResumeDownload && task.StorageRetryCount >= cfg.StorageRetries || decision.Action == mediaretry.ResumeBilling && task.BillingRetryCount >= cfg.BillingRetries {
			checkpoint.Decision.Action, checkpoint.Decision.BlockedReason = mediaretry.ManualReview, "postprocessing_retry_budget_exhausted"
			return buildImagePolicyDecision(ctx, task, failure, cfg, checkpoint, updates)
		}
		status, outbox = model.ImageTaskInvoking, "execute"
		if task.ImageCount > 0 {
			status = model.ImageTaskStorageFailed
			if decision.Action == mediaretry.ResumeBilling {
				status = model.ImageTaskBillingFailed
			}
		}
		checkpoint.Pending = true
		if decision.Action == mediaretry.ResumeDownload {
			updates["storage_retry_count"] = task.StorageRetryCount + 1
		} else {
			updates["billing_retry_count"] = task.BillingRetryCount + 1
		}
	case mediaretry.Stop:
		status = model.ImageTaskFailed
	}
	if decision.BlockedReason == "resubmissions_paused" {
		status, outbox, checkpoint.Pending = model.ImageTaskQueued, "execute", true
		decision.DelaySeconds = max(30, decision.DelaySeconds)
	}
	if checkpoint.Pending {
		updates["finished_at"] = 0
		updates["next_attempt_at"] = now + int64(max(1, decision.DelaySeconds))
	} else if cfg.FailedRequestRetentionHours > 0 {
		updates["request_retain_until"] = now + int64(cfg.FailedRequestRetentionHours)*3600
	} else if status == model.ImageTaskFailed || status == model.ImageTaskExpired || status == model.ImageTaskExecutionUnknown {
		updates["request_cipher"], updates["request_retain_until"] = nil, 0
	}
	updates["status"] = status
	checkpoint.Decision = decision
	encoded, err := common.Marshal(checkpoint)
	if err != nil {
		return imageFailurePlan{}, err
	}
	updates["retry_policy_state"] = string(encoded)
	audit := ImageRetryDecision{Version: 1, Stage: checkpoint.Fact.Stage, Action: decision.Action, Reason: decision.BlockedReason,
		Rule: decision.RuleID, TotalUsed: checkpoint.State.TotalUsed, TotalLimit: checkpoint.State.TotalLimit, RecordedAt: now, Policy: &decision}
	if next, ok := updates["next_attempt_at"].(int64); ok {
		audit.NextAttemptAt = next
	}
	if audit.Reason == "" {
		audit.Reason = decision.Category
	}
	encoded, err = common.Marshal(audit)
	if err != nil {
		return imageFailurePlan{}, err
	}
	updates["retry_decision"] = string(encoded)
	if raw, ok := updates["attempts"].(string); ok {
		var attempts []ImageChannelAttempt
		if err := common.UnmarshalJsonStr(raw, &attempts); err != nil {
			return imageFailurePlan{}, err
		}
		if len(attempts) > 0 {
			attempts[len(attempts)-1].Decision = &audit
			encoded, err := common.Marshal(attempts)
			if err != nil {
				return imageFailurePlan{}, err
			}
			updates["attempts"] = string(encoded)
		}
	}
	event, err := model.NewMediaRetryEvent(task.TaskId, checkpoint.FailureAttempt, checkpoint.Fact, decision, checkpoint.State)
	if err != nil {
		return imageFailurePlan{}, err
	}
	return imageFailurePlan{Updates: updates, Event: event, EventType: "retry_policy_decision", EventMessage: failure.Message, OutboxKind: outbox, Audit: audit, Decision: decision}, nil
}

// ImagePolicyRouting retains credential exclusions but replaces implicit retry
// preferences with the administrator's explicit channel action.
func ImagePolicyRouting(routing ImageAccountRouting, checkpoint model.ImageRetryPolicyState) ImageAccountRouting {
	decision := checkpoint.Decision
	if !checkpoint.Pending || !mediaretry.IsResubmission(decision.Action) {
		return routing
	}
	routing.Stop, routing.Pin, routing.PinChannel, routing.Avoid, routing.AvoidChannel = false, "", 0, "", 0
	routing.Excluded = nil
	routing.ExcludedChannels = slices.Clone(decision.ExcludedChannelIDs)
	routing.AllowedChannels = slices.Clone(decision.AllowedChannelIDs)
	routing.WaitForCooldown = true
	if decision.Action == mediaretry.SameChannel || decision.Action == mediaretry.LocalSameChannel {
		routing.PinChannel = checkpoint.Fact.ChannelID
	} else {
		routing.ExcludedChannels = append(routing.ExcludedChannels, checkpoint.Fact.ChannelID)
	}
	return routing
}

// refreshImageRetryDecision reads the live policy before choosing a replacement
// channel. Rule changes never clear the lifetime use counters.
func refreshImageRetryDecision(ctx context.Context, task *model.AsyncImageTask, cfg ImageRuntimeConfig) (model.ImageRetryPolicyState, bool, error) {
	var checkpoint model.ImageRetryPolicyState
	if task.RetryPolicyState == "" {
		return checkpoint, false, nil
	}
	if err := common.UnmarshalJsonStr(task.RetryPolicyState, &checkpoint); err != nil {
		return checkpoint, false, err
	}
	if !checkpoint.Pending && !checkpoint.BuiltinPending {
		return checkpoint, false, nil
	}
	checkpoint.State.Now, checkpoint.State.TotalLimit = time.Now().Unix(), cfg.TotalRetries
	checkpoint.State.TotalUsed = task.RetryCount
	if checkpoint.ReservedRetry {
		checkpoint.State.TotalUsed = max(0, task.RetryCount-1)
	}
	decision, err := EvaluateMediaRetry(ctx, checkpoint.Fact, checkpoint.State)
	if err != nil {
		return checkpoint, false, fmt.Errorf("%w: %v", errImageRetryPolicyUnavailable, err)
	}
	if !decision.Matched {
		if checkpoint.BuiltinPending {
			return checkpoint, false, nil
		}
		// Re-enter the normal failure policy instead of silently executing a
		// step from a disabled/deleted rule. The caller replays only the error.
		checkpoint.Pending = false
		checkpoint.Decision = decision
		encoded, err := common.Marshal(checkpoint)
		if err != nil {
			return checkpoint, false, err
		}
		updates := map[string]any{"retry_policy_state": string(encoded)}
		if checkpoint.ReservedRetry {
			updates["retry_count"] = max(0, task.RetryCount-1)
		}
		if err := model.TransitionImageTask(ctx, *task, updates, "retry_policy_replaced", "", ""); err != nil {
			return checkpoint, false, err
		}
		if err := model.DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error; err != nil {
			return checkpoint, false, err
		}
		return checkpoint, true, nil
	}
	checkpoint.Pending, checkpoint.BuiltinPending = true, false
	checkpoint.Decision = decision
	encoded, err := common.Marshal(checkpoint)
	if err != nil {
		return checkpoint, false, err
	}
	updates := map[string]any{"retry_policy_state": string(encoded)}
	if mediaretry.IsResubmission(decision.Action) && !checkpoint.ReservedRetry {
		checkpoint.ReservedRetry = true
		encoded, err = common.Marshal(checkpoint)
		if err != nil {
			return checkpoint, false, err
		}
		updates["retry_policy_state"], updates["retry_count"] = string(encoded), task.RetryCount+1
	}
	event, err := model.NewMediaRetryEvent(task.TaskId, checkpoint.FailureAttempt, checkpoint.Fact, decision, checkpoint.State)
	if err != nil {
		return checkpoint, false, err
	}
	if err := model.TransitionImageTask(ctx, *task, updates, "retry_policy_checked", "", "", event); err != nil {
		return checkpoint, false, err
	}
	err = model.DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error
	return checkpoint, false, err
}
