package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
)

// PreviewImageFailureDecision runs the worker's read-only failure planner with
// durable protocol evidence and current budgets. Older samples without that
// evidence remain text matches; diagnostics never reconstruct acceptance.
func PreviewImageFailureDecision(ctx context.Context, taskID string, fact mediaretry.Fact, state mediaretry.State, policy mediaretry.Policy) (mediaretry.Decision, error) {
	if fact.ImageEvidence == nil {
		return mediaretry.Evaluate(policy, fact, state)
	}
	var task model.AsyncImageTask
	if err := model.DB.WithContext(ctx).Where("task_id = ?", taskID).Take(&task).Error; err != nil {
		return mediaretry.Decision{}, err
	}
	cfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return mediaretry.Decision{}, err
	}
	evidence := fact.ImageEvidence
	var pending model.ImageRetryPolicyState
	if task.RetryPolicyState != "" {
		if err := common.UnmarshalJsonStr(task.RetryPolicyState, &pending); err != nil {
			return mediaretry.Decision{}, err
		}
	}
	if evidence.FailureAttempt > 0 && evidence.FailureAttempt == pending.FailureAttempt && (pending.Pending || pending.BuiltinPending) {
		// Scheduling reserves the next category attempt. Previewing that same
		// failure must not reserve it twice; already-started actions clear these
		// flags and retain all their spent counters.
		budget := ""
		if pending.BuiltinPending {
			var audit ImageRetryDecision
			if err := common.UnmarshalJsonStr(task.RetryDecision, &audit); err != nil {
				return mediaretry.Decision{}, err
			}
			budget = audit.Budget
			var attempts []ImageChannelAttempt
			if task.Attempts != "" {
				if err := common.UnmarshalJsonStr(task.Attempts, &attempts); err != nil {
					return mediaretry.Decision{}, err
				}
				if len(attempts) > 0 && attempts[len(attempts)-1].ChannelId == fact.ChannelID {
					attempts[len(attempts)-1].FinishedAt = 0
					encoded, err := common.Marshal(attempts)
					if err != nil {
						return mediaretry.Decision{}, err
					}
					task.Attempts = string(encoded)
				}
			}
		} else if pending.Decision.Action == mediaretry.ResumeDownload {
			budget = "storage"
		} else if pending.Decision.Action == mediaretry.ResumeBilling {
			budget = "billing"
		}
		switch budget {
		case "reference":
			task.ReferenceRetryCount = max(0, task.ReferenceRetryCount-1)
		case "capacity":
			task.CapacityRetryCount = max(0, task.CapacityRetryCount-1)
		case "transient", "poll":
			task.TransientRetryCount = max(0, task.TransientRetryCount-1)
		case "storage":
			task.StorageRetryCount = max(0, task.StorageRetryCount-1)
		case "billing":
			task.BillingRetryCount = max(0, task.BillingRetryCount-1)
		}
	}
	task.ChannelId, task.Provider, task.DispatchedAt = fact.ChannelID, fact.Provider, evidence.DispatchedAt
	task.UpstreamTaskId = evidence.UpstreamTaskID
	task.RetryCount = state.TotalUsed
	checkpoint, err := common.Marshal(model.ImageRetryPolicyState{State: state})
	if err != nil {
		return mediaretry.Decision{}, err
	}
	task.RetryPolicyState = string(checkpoint)
	failure := &AsyncImageFailure{Code: evidence.Code, InternalCode: evidence.InternalCode, Message: fact.Message,
		HTTPStatus: fact.HTTPStatus, ProviderCode: fact.ErrorCode, ExecutionUnknown: fact.Unknown,
		RetryAfter:       time.Duration(min(max(evidence.RetryAfterSeconds, 0), 86400)) * time.Second,
		UpstreamTerminal: evidence.UpstreamTerminal, RetrySameChannel: evidence.RetrySameChannel,
		RetryableHTTPResponse: evidence.RetryableHTTPResponse, SynchronousRetryableResponse: evidence.SynchronousRetryableResponse,
		SubmissionRejected: evidence.SubmissionRejected, ReferenceRejectionVerified: evidence.ReferenceRejectionVerified,
		ReferenceTransportFallback: evidence.ReferenceTransportFallback}
	var plan imageFailurePlan
	if evidence.Postprocess {
		cause := errors.New(fact.Message)
		if evidence.QuotaUncertain {
			cause = model.ErrImageQuotaCacheUncertain
		}
		plan, err = buildImagePostprocessPlan(ctx, task, cfg, cause, fact.Stage != "billing", &policy, fact.Message)
	} else {
		plan, err = buildImageFailurePlan(ctx, task, failure, cfg, &policy)
	}
	if err != nil {
		return mediaretry.Decision{}, err
	}
	decision := plan.Decision
	if !decision.Matched {
		decision.TotalUsed, decision.TotalLimit = state.TotalUsed, state.TotalLimit
		decision.RuleUsed, decision.RuleLimit = plan.Audit.Used, plan.Audit.Limit
		decision.RequestedAction = decision.Action
		if plan.Audit.NextAttemptAt > 0 {
			decision.DelaySeconds = int(max(0, plan.Audit.NextAttemptAt-time.Now().Unix()))
		}
		if plan.OutboxKind == "" {
			decision.BlockedReason = plan.Audit.Reason
		}
	}
	return decision, nil
}

// PreviewImageRetryEligibility uses the worker's permission, reservation and
// candidate filters. It never acquires a slot, downloads input, or consumes quota.
func PreviewImageRetryEligibility(ctx context.Context, taskID string, fact mediaretry.Fact, decision mediaretry.Decision) (MediaRetryEligibility, error) {
	result := MediaRetryEligibility{Checked: true, CandidateChannelIDs: []int{}, Warnings: []string{"dispatch_checks_repeated_before_send"}}
	var task model.AsyncImageTask
	if err := model.DB.WithContext(ctx).Where("task_id = ?", taskID).Take(&task).Error; err != nil {
		return result, err
	}
	if task.Terminal() {
		result.Reason = "historical_task_not_restarted"
		return result, nil
	}
	if decision.Action == mediaretry.ManualReview || decision.Action == mediaretry.Stop {
		result.Eligible, result.Reason = decision.BlockedReason == "", decision.BlockedReason
		return result, nil
	}
	if decision.Action == "" {
		result.Checked, result.Reason = false, "builtin_decision_not_simulated"
		return result, nil
	}
	builtinGeneration := !decision.Matched && slices.Contains([]string{"retry_submission", "switch_account", "local_reference_fallback", "wait_capacity"}, decision.Action)
	if !mediaretry.IsResubmission(decision.Action) && !builtinGeneration {
		switch decision.Action {
		case mediaretry.PollSameJob:
			result.Eligible = task.UpstreamTaskId != "" && len(task.SelectedChannelCipher) > 0
		case mediaretry.ResumeDownload, mediaretry.ResumeBilling:
			result.Eligible = task.UpstreamSucceededAt > 0 || task.ImageCount > 0 || task.ResultCount > 0
		case "resume_output":
			result.Eligible = task.UpstreamSucceededAt > 0 || task.ImageCount > 0 || task.ResultCount > 0
		case "manual_recovery", "reconcile_unknown":
			result.Reason = decision.BlockedReason
			return result, nil
		}
		if !result.Eligible {
			result.Reason = "recovery_evidence_unavailable"
		}
		return result, nil
	}
	cfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return result, err
	}
	if task.UpstreamSucceededAt > 0 || task.ImageCount > 0 || task.ResultCount > 0 {
		result.Reason = "output_already_available"
		return result, nil
	}
	if model.AsyncImageExecutionDeadline(task, cfg.ExecutionTimeout) <= time.Now().Unix() {
		result.Reason = "execution_deadline_reached"
		return result, nil
	}
	data, err := DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
	if err != nil {
		result.Reason = "request_replay_unavailable"
		return result, nil
	}
	var request AsyncImageRequest
	if err := common.Unmarshal(data, &request); err != nil {
		result.Reason = "request_replay_unavailable"
		return result, nil
	}
	request.Reservation, err = model.GetAsyncImageReservation(ctx, task)
	if err != nil {
		return result, err
	}
	if request.Reservation == nil || request.Reservation.Status != "reserved" {
		result.Reason = "reservation_unavailable"
		return result, nil
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", task.TokenId, task.UserId).Take(&token).Error; err != nil {
		result.Reason = "token_unavailable"
		return result, nil
	}
	policy, _, err := ValidateAsyncImageEligibility(ctx, token, request, cfg)
	if err != nil || policy.Group != task.Group {
		result.Reason = "eligibility_changed"
		return result, nil
	}
	for _, part := range request.Parts {
		if part.Type != "image_url" && part.Type != "mask" {
			continue
		}
		bound, err := IsBoundImageReference(ctx, task.TokenId, part.URL)
		if err != nil || part.LocalBound && !bound || part.LocalURL != "" && part.LocalExpiresAt <= time.Now().Unix() {
			result.Reason = "reference_unavailable"
			return result, nil
		}
		if !bound && part.LocalURL == "" && !strings.HasPrefix(strings.ToLower(part.URL), "data:") {
			if _, err := ValidateImageReferenceURL(part.URL); err != nil {
				result.Reason = "reference_unavailable"
				return result, nil
			}
			result.Warnings = append(result.Warnings, "remote_reference_download_not_performed")
		}
	}
	var attempts []ImageChannelAttempt
	if task.Attempts != "" {
		if err := common.UnmarshalJsonStr(task.Attempts, &attempts); err != nil {
			return result, err
		}
	}
	routing := ImageAccountAttemptRouting(attempts, cfg.ReferenceRetries, request.Platform, cfg.GeminiAccountSwitches)
	if !builtinGeneration {
		routing = ImagePolicyRouting(routing, model.ImageRetryPolicyState{Pending: true, Fact: fact, Decision: decision})
	}
	channels, _, err := eligibleImageChannels(ctx, policy, request, routing)
	if err != nil {
		result.Reason = "no_compatible_channel"
		var cooling *ImageAccountCooling
		if errors.As(err, &cooling) {
			result.Reason = "account_cooldown"
		} else if errors.Is(err, ErrImageLocalCapacityBusy) {
			result.Reason = "local_capacity_busy"
		} else if errors.Is(err, ErrImageLocalControlsUnavailable) {
			result.Reason = "routing_controls_unavailable"
		}
		return result, nil
	}
	for _, channel := range channels {
		result.CandidateChannelIDs = append(result.CandidateChannelIDs, channel.Id)
	}
	result.Eligible = len(channels) > 0
	return result, nil
}
