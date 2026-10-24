package service

import (
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// ImageRetryDecision is a bounded explanation of policy, not provider content.
// It is safe to show to the task owner without exposing accounts or credentials.
type ImageRetryDecision = model.ImageRetryDecision

// ImageFailureRetryDecision describes the actual committed transition so the
// displayed decision cannot disagree with retry budgets or execution barriers.
func ImageFailureRetryDecision(task model.AsyncImageTask, failure *AsyncImageFailure, cfg ImageRuntimeConfig, updates map[string]any) ImageRetryDecision {
	decision := ImageRetryDecision{Version: 1, Stage: "submit", Action: "stop", Reason: "unknown_error", Rule: failure.InternalCode, TotalUsed: task.RetryCount, TotalLimit: cfg.TotalRetries, RecordedAt: time.Now().Unix()}
	if task.StartedAt > 0 {
		decision.Deadline = task.StartedAt + int64(cfg.ExecutionTimeout)
	}
	if task.DispatchedAt == 0 {
		decision.Stage = "prepare"
	}
	if task.ChannelId == 0 {
		decision.Stage = "routing"
	}
	if task.UpstreamTaskId != "" && !failure.UpstreamTerminal {
		decision.Stage = "poll"
	}
	if task.UpstreamSucceededAt > 0 {
		decision.Stage = "download"
		decision.Budget, decision.Used, decision.Limit = "storage", task.StorageRetryCount, cfg.StorageRetries
		if failure.InternalCode == "bill_preparation_failed" || failure.InternalCode == "billing_usage_missing" {
			decision.Stage = "billing"
			decision.Budget, decision.Used, decision.Limit = "billing", task.BillingRetryCount, cfg.BillingRetries
		}
	} else {
		switch failure.Code {
		case 601:
			decision.Reason = "content_policy"
		case 604, 611, 612, 613:
			decision.Reason = "invalid_input"
		case 602:
			decision.Budget, decision.Used, decision.Limit = "reference", task.ReferenceRetryCount, cfg.ReferenceRetries
			decision.Reason = "reference_retry_budget_exhausted"
		case 603:
			decision.Budget, decision.Used, decision.Limit = "capacity", task.CapacityRetryCount, cfg.CapacityRetries
			decision.Reason = "capacity_retry_budget_exhausted"
			if failure.InternalCode == "upstream_image_route_incompatible" || failure.InternalCode == "upstream_image_reference_incompatible" || failure.InternalCode == "upstream_image_submission_rejected" {
				decision.Reason = "compatibility_retry_budget_exhausted"
			}
		case 605, 606:
			decision.Budget, decision.Used, decision.Limit = "transient", task.TransientRetryCount, cfg.TransientRetries
			decision.Reason = "transient_retry_budget_exhausted"
		}
	}
	if task.UpstreamSucceededAt == 0 && task.UpstreamTaskId == "" && (failure.InternalCode == "upstream_transport_timeout" || failure.InternalCode == "timeout_accounts_exhausted") {
		decision.Budget, decision.Used, decision.Limit = "timeout", 0, cfg.TimeoutRetries
		var attempts []ImageChannelAttempt
		history := task.Attempts
		if updated, ok := updates["attempts"].(string); ok {
			history = updated
		}
		if common.UnmarshalJsonStr(history, &attempts) == nil {
			for _, attempt := range attempts {
				if attempt.Decision != nil && attempt.Decision.Budget == "timeout" && attempt.Decision.Action == "switch_account" {
					decision.Used++
				}
			}
		}
		if updates["status"] == model.ImageTaskQueued {
			decision.Used++
		}
	}
	localFallback := failure.ReferenceTransportFallback
	if failure.ReferenceTransportFallback {
		decision.Budget, decision.Used, decision.Limit = "reference", task.ReferenceRetryCount, 1
	}
	if failure.Code == 602 {
		localFallback = localFallback || IsImageReferenceDownloadTimeout(failure)
		if !failure.ReferenceTransportFallback && task.DispatchedAt > 0 && (failure.InternalCode == "upstream_failed" || failure.UpstreamTerminal) {
			decision.Limit++
		}
		var attempts []ImageChannelAttempt
		if task.ReferenceRetryCount == 0 && common.UnmarshalJsonStr(task.Attempts, &attempts) == nil && len(attempts) > 0 {
			localFallback = localFallback || attempts[len(attempts)-1].ReferenceMode == "passthrough_fallback_local"
		}
	}
	status, _ := updates["status"].(string)
	if count, ok := updates["retry_count"].(int); ok {
		decision.TotalUsed = count
	}
	for _, key := range []string{decision.Budget + "_retry_count", "storage_retry_count", "billing_retry_count"} {
		if count, ok := updates[key].(int); ok {
			decision.Used = count
		}
	}
	decision.NextAttemptAt, _ = updates["next_attempt_at"].(int64)
	switch status {
	case model.ImageTaskQueued, model.ImageTaskInvoking:
		decision.Action, decision.Reason = "retry_submission", "temporary_failure"
		if decision.Budget == "timeout" {
			decision.Action = "switch_account"
		} else if task.UpstreamSucceededAt > 0 {
			decision.Action = "resume_output"
			if failure.InternalCode == "billing_usage_missing" && task.UpstreamTaskId != "" {
				decision.Action = "poll_same_job"
			}
		} else if task.UpstreamTaskId != "" && !failure.UpstreamTerminal {
			decision.Action, decision.Reason, decision.Budget = "poll_same_job", "accepted_job", "poll"
			decision.Limit = 0 // Polling is bounded by the execution deadline.
		} else if localFallback {
			decision.Action = "local_reference_fallback"
		} else if failure.InternalCode == "upstream_image_route_incompatible" {
			decision.Action, decision.Reason = "switch_account", "incompatible_image_route"
		} else if failure.InternalCode == "upstream_image_reference_incompatible" {
			decision.Action, decision.Reason = "switch_account", "incompatible_image_reference"
		} else if failure.InternalCode == "upstream_image_submission_rejected" {
			decision.Action, decision.Reason = "switch_account", "submission_rejected_without_diagnostic"
		} else if failure.Code == 603 {
			decision.Action, decision.Reason = "wait_capacity", "no_available_capacity"
		}
	case model.ImageTaskStorageFailed, model.ImageTaskBillingFailed:
		decision.Action, decision.Reason = "manual_recovery", decision.Budget+"_retry_budget_exhausted"
		if failure.Code == 607 || failure.Code == 604 || failure.Code == 601 {
			decision.Reason = "invalid_output"
		}
	case model.ImageTaskExecutionUnknown:
		decision.Action, decision.Reason = "reconcile_unknown", "execution_unknown"
	case model.ImageTaskExpired:
		decision.Reason = "execution_deadline"
	default:
		if task.RetryCount >= cfg.TotalRetries && (failure.Code == 602 || failure.Code == 603 || failure.Code == 605 || failure.Code == 606 || failure.ReferenceTransportFallback) {
			decision.Reason = "total_retry_budget_exhausted"
		}
	}
	if failure.InternalCode == "reference_accounts_exhausted" || failure.InternalCode == "account_switches_exhausted" {
		decision.Reason = "account_switch_limit"
	}
	if failure.InternalCode == "image_compatible_accounts_exhausted" || failure.InternalCode == "image_reference_accounts_exhausted" {
		decision.Reason = "no_compatible_account"
	}
	if decision.Budget == "timeout" && status == model.ImageTaskFailed {
		decision.Reason = "timeout_retry_budget_exhausted"
		if failure.InternalCode == "timeout_accounts_exhausted" {
			decision.Reason = "account_switch_limit"
		} else if task.StartedAt > 0 && decision.RecordedAt >= decision.Deadline {
			decision.Reason = "execution_deadline"
		} else if task.RetryCount >= cfg.TotalRetries {
			decision.Reason = "total_retry_budget_exhausted"
		}
	}
	return decision
}
