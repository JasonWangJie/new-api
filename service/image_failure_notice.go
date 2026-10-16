package service

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

type ImageFailureNotice struct {
	Summary    string
	Suggestion string
	Retryable  bool
	Stage      string
}

// ImageTaskFailureNotice describes the committed recovery action. A transient
// error category alone never promises another generation or an automatic retry.
func ImageTaskFailureNotice(task model.AsyncImageTask) ImageFailureNotice {
	var decision ImageRetryDecision
	validDecision := common.UnmarshalJsonStr(task.RetryDecision, &decision) == nil && decision.Version == 1
	var notice ImageFailureNotice
	if validDecision {
		notice.Stage = decision.Stage
	}
	if notice.Stage == "" {
		switch {
		case task.Status == model.ImageTaskBillingFailed || task.Status == model.ImageTaskBillingPending:
			notice.Stage = "billing"
		case task.UpstreamSucceededAt > 0 || task.Status == model.ImageTaskStorageFailed:
			notice.Stage = "postprocess"
		case task.UpstreamTaskId != "":
			notice.Stage = "poll"
		case task.DispatchedAt > 0:
			notice.Stage = "submit"
		case task.ChannelId == 0:
			notice.Stage = "routing"
		default:
			notice.Stage = "prepare"
		}
	}
	if task.Status == model.ImageTaskSucceeded || task.ErrorCode == "" && task.ErrorMessage == "" && task.PublicErrorCode == 0 {
		return notice
	}
	if !task.Terminal() && task.UpstreamTaskId != "" && validDecision && decision.Action == "poll_same_job" && (decision.Rule == "upstream_pending" || decision.Rule == "recovered" || decision.Rule == "capacity_wait") {
		return notice
	}
	summaries := map[int]string{
		601: "Upstream safety review did not pass",
		602: "Reference image could not be fetched",
		603: "Image capacity is temporarily unavailable",
		604: "Upstream rejected image input or parameters",
		605: "Upstream rate limit exceeded",
		606: "Temporary upstream image service failure",
		608: "Upstream execution could not be confirmed",
		609: "Image post-processing needs attention",
		610: "Upstream rejected the request; the exact cause is not confirmed",
		611: "Too many reference images",
		612: "A reference image is required",
		613: "Upstream could not process the prompt or reference images",
	}
	notice.Summary = summaries[task.PublicErrorCode]
	if notice.Summary == "" {
		notice.Summary = "Upstream rejected the request; the exact cause is not confirmed"
	}
	notice.Suggestion = "Ask the service provider to check the request format, model compatibility, and upstream request ID."
	switch task.PublicErrorCode {
	case 601:
		notice.Suggestion = "Review the upstream message; contact the service provider if the review failed unexpectedly."
	case 602:
		notice.Suggestion = "Check that the reference image can be downloaded by the upstream provider."
	case 604, 611, 612, 613:
		notice.Suggestion = "Check the upstream limits shown in the error, then reduce the input size or adjust the parameters."
	case 609:
		notice.Suggestion = "Check storage or billing recovery; do not repeat generation."
	}
	switch {
	case task.ErrorCode == "billing_quota_uncertain":
		notice.Summary = "Image post-processing needs attention"
		notice.Suggestion = "Ask an administrator to reconcile the quota reservation before trying again."
		return notice
	case task.ErrorCode == "quota_cache_uncertain":
		notice.Summary = "Image quota reservation could not be confirmed"
		notice.Suggestion = "Ask an administrator to reconcile the quota reservation before trying again."
		return notice
	case task.ErrorCode == "quota_cache_unavailable" || task.ErrorCode == "request_preparation_failed" && strings.Contains(strings.ToLower(task.ErrorMessage), "cache"):
		notice.Summary = "Image quota checks are temporarily unavailable"
		notice.Suggestion = "Wait for local quota checks to recover; no upstream generation has been authorized."
	case task.ErrorCode == "request_preparation_failed" || task.ErrorCode == "request_preparation_timeout":
		notice.Summary = "Image request preparation failed"
		notice.Suggestion = "Contact the service provider to check local image request preparation."
	case task.Status == model.ImageTaskExpired && task.DispatchedAt == 0 && task.UpstreamTaskId == "" && task.UpstreamSucceededAt == 0:
		notice.Summary = "Image execution deadline expired"
		notice.Suggestion = "The task expired before upstream dispatch; submit a new request if it is still needed."
	case task.UpstreamSucceededAt > 0 || task.Status == model.ImageTaskStorageFailed || task.Status == model.ImageTaskBillingFailed:
		notice.Summary = "Image post-processing needs attention"
		notice.Suggestion = "Check storage or billing recovery; do not repeat generation."
	}
	if task.Status == model.ImageTaskExecutionUnknown || task.PublicErrorCode == 608 && task.ErrorCode != "execution_deadline" && !(validDecision && decision.Action == "poll_same_job" && task.UpstreamTaskId != "" && !task.Terminal()) {
		notice.Summary = "Upstream execution could not be confirmed"
		notice.Suggestion = "Do not submit again until an administrator has checked the upstream result and billing."
		return notice
	}
	if task.Terminal() || task.NextAttemptAt <= 0 || !validDecision {
		return notice
	}
	switch decision.Action {
	case "retry_submission", "switch_account", "same_account_retry", "local_reference_fallback":
		if task.UpstreamTaskId == "" && task.UpstreamSucceededAt == 0 {
			notice.Retryable = true
			notice.Suggestion = "The task is scheduled for another submission within its retry limits."
		}
	case "poll_same_job":
		if task.UpstreamTaskId != "" {
			notice.Retryable = true
			notice.Suggestion = "The existing upstream job will be checked again; no new generation will be submitted."
		}
	case "resume_output":
		if task.UpstreamSucceededAt > 0 {
			notice.Retryable = true
			notice.Suggestion = "The existing output will be processed again; generation will not be repeated."
		}
	case "wait_local_dependency":
		notice.Retryable = true
		notice.Suggestion = "The task is waiting for a local dependency to recover before dispatch."
	case "wait_capacity", "wait_account_cooldown", "wait_supplier_budget":
		notice.Retryable = true
		notice.Suggestion = "The task is waiting for capacity before another upstream submission."
	}
	return notice
}
