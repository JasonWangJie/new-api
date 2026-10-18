package service

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

type ImageFailureNotice struct {
	Summary       string
	SummaryParams map[string]any
	Suggestion    string
	Retryable     bool
	Stage         string
}

var (
	imageReferenceLimitNoticePattern = regexp.MustCompile(`(?i)(?:image_url|reference image|参考图|图片)[^\r\n]{0,100}?(?:exceeds?|上限|不得超过|不能超过|超过|(?:size\s+)?limit(?:\s+of)?|maximum(?:\s+size)?)\s*[:：]?\s*(?:the\s+)?(?:limit(?:\s+of)?\s*)?([0-9]{1,12}(?:\.[0-9]{1,6})?\s*(?:GB|MB|KB|B))(?:\b|$)`)
	imageSizeNoticeParamPattern      = regexp.MustCompile(`^[0-9]{1,12}(?:\.[0-9]{1,6})?(?:GB|MB|KB|B)$`)
	imageTextOnlyNoticePattern       = regexp.MustCompile(`(?i)^(?:(?:很)?抱歉[，,：:\s]*|(?:i['’]m sorry|i am sorry|sorry)[,\s]*(?:but\s+)?)?(?:我(?:无法|不能)直接(?:生成或编辑|生成|编辑)图片|i cannot directly (?:generate|edit) images)`)
)

// SummaryText keeps API summaries readable for clients that do not implement
// translated summary keys. Only the recorded, bounded display parameters are used.
func (notice ImageFailureNotice) SummaryText() string {
	text := notice.Summary
	if limit, ok := notice.SummaryParams["limit"].(string); ok && imageSizeNoticeParamPattern.MatchString(limit) {
		text = strings.ReplaceAll(text, "{{limit}}", limit)
	}
	if seconds, ok := notice.SummaryParams["seconds"].(int64); ok && seconds > 0 && seconds <= 86400 {
		text = strings.ReplaceAll(text, "{{seconds}}", strconv.FormatInt(seconds, 10))
	}
	if strings.Contains(text, "{{") || strings.Contains(text, "}}") {
		return "Upstream rejected the request; the exact cause is not confirmed"
	}
	return text
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
	internalSummaries := map[string]string{
		"upstream_transport_disconnected": "Upstream connection closed before the result was confirmed",
		"upstream_transport_timeout":      "Upstream request timed out",
		"upstream_transport_canceled":     "Upstream request was interrupted",
		"upstream_response_invalid":       "Upstream response could not be read or parsed",
		"timeout_accounts_exhausted":      "No unused image account remains after request timeouts",
		"timeout_retries_exhausted":       "Image request timeout retry limit reached",
		"timeout_deadline_exhausted":      "Image execution deadline expired after request timeouts",
	}
	if summary, ok := internalSummaries[task.ErrorCode]; ok {
		notice.Summary = summary
	}
	message := SanitizeAsyncImageDiagnostic(task.ErrorMessage, asyncImageProviderMessageLimit)
	var attempts []ImageChannelAttempt
	if err := common.UnmarshalJsonStr(task.Attempts, &attempts); err != nil {
		attempts = nil
	}
	var lastAttempt ImageChannelAttempt
	if len(attempts) > 0 {
		lastAttempt = SanitizeAsyncImageAttempt(attempts[len(attempts)-1])
	}
	specificCause, specificOutputCause := false, false
	switch task.ErrorCode {
	case "upstream_transport_disconnected":
		diagnostic, _ := strings.CutSuffix(message, "; only the existing upstream job may be polled again")
		switch diagnostic {
		case "Upstream connection was interrupted after dispatch; execution could not be confirmed (EOF)":
			notice.Summary = "The upstream connection ended before returning a result (EOF)"
		case "Upstream connection was interrupted after dispatch; execution could not be confirmed (unexpected EOF)":
			notice.Summary = "The upstream connection ended before returning a complete result (unexpected EOF)"
		}
	case "timeout_accounts_exhausted":
		for i := len(attempts) - 1; i >= 0; i-- {
			attempt := attempts[i]
			if !attempt.Dispatched || !attempt.TimedOut || attempt.StartedAt <= 0 || attempt.FinishedAt <= attempt.StartedAt {
				continue
			}
			// StartedAt includes preparation. Describe the generation attempt's
			// recorded elapsed time, never infer upstream wait or a historical limit.
			seconds := attempt.FinishedAt - attempt.StartedAt
			if seconds <= 86400 {
				notice.Summary = "The generation attempt timed out after {{seconds}} seconds, and no alternative account is available"
				notice.SummaryParams = map[string]any{"seconds": seconds}
			}
			break
		}
	case "reference_download_failed":
		notice.Summary = "The reference image could not be downloaded locally"
		if lastAttempt.ReferenceMode == "local" && lastAttempt.ReferenceFallback && !lastAttempt.Dispatched && lastAttempt.FinishedAt > 0 && lastAttempt.Code == 602 {
			for _, attempt := range attempts[:len(attempts)-1] {
				if attempt.Dispatched && attempt.FinishedAt > 0 && attempt.ReferenceFailure && (attempt.TransportUsed == "remote" || attempt.TransportUsed == "mixed") {
					notice.Summary = "The upstream could not fetch the reference image, and the local download fallback also failed"
					break
				}
			}
		}
	case "output_parse_failed":
		switch message {
		case "declared image MIME does not match its bytes":
			notice.Summary = "The image format declared by the upstream does not match the image data"
			specificOutputCause = true
		case "invalid image container or excessive trailing data":
			notice.Summary = "The upstream image container is invalid or contains excessive trailing data"
			specificOutputCause = true
		case "Upstream image output is missing":
			notice.Summary = "The upstream did not return a usable image"
			specificOutputCause = true
		}
	case "admin_terminated":
		notice.Summary = "The task was manually terminated"
		specificCause = true
	case "upstream_failed", "upstream_job_failed", "invalid_reference_image":
		switch {
		case task.PublicErrorCode == 604:
			if match := imageReferenceLimitNoticePattern.FindStringSubmatch(message); len(match) == 2 {
				limit := strings.ToUpper(strings.Join(strings.Fields(match[1]), ""))
				notice.Summary = "Reference image exceeds the upstream limit of {{limit}}"
				notice.SummaryParams = map[string]any{"limit": limit}
			}
		case task.PublicErrorCode == 602 && lastAttempt.Dispatched && lastAttempt.ReferenceFailure && (lastAttempt.TransportUsed == "remote" || lastAttempt.TransportUsed == "mixed"):
			notice.Summary = "The upstream could not fetch the reference image"
		case task.PublicErrorCode == 610 || task.PublicErrorCode == 605:
			lowered := strings.ToLower(message)
			rateLimited := strings.Contains(lowered, "请求过于频繁") || strings.Contains(lowered, "请求太频繁") || strings.Contains(lowered, "rate limit exceeded") || strings.Contains(lowered, "too many requests")
			switch strings.ToLower(lastAttempt.ProviderCode) {
			case "429", "rate_limit_exceeded", "too_many_requests":
				rateLimited = true
			}
			if rateLimited {
				notice.Summary = "Upstream rate limit exceeded"
				break
			}
			if imageTextOnlyNoticePattern.MatchString(message) {
				notice.Summary = "The upstream returned text instead of an image"
			}
		}
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
	case task.ErrorCode == "timeout_accounts_exhausted":
		notice.Suggestion = "Add an eligible account for this model and resolution, or check why the existing account timed out."
	case task.ErrorCode == "timeout_retries_exhausted" || task.ErrorCode == "timeout_deadline_exhausted":
		notice.Suggestion = "Check upstream availability and account timeout settings before submitting again."
	case task.Status == model.ImageTaskExpired && task.DispatchedAt == 0 && task.UpstreamTaskId == "" && task.UpstreamSucceededAt == 0:
		notice.Summary = "Image execution deadline expired"
		notice.Suggestion = "The task expired before upstream dispatch; submit a new request if it is still needed."
	case task.UpstreamSucceededAt > 0 || task.Status == model.ImageTaskStorageFailed || task.Status == model.ImageTaskBillingFailed:
		if !specificOutputCause {
			notice.Summary = "Image post-processing needs attention"
			notice.SummaryParams = nil
		}
		notice.Suggestion = "Check storage or billing recovery; do not repeat generation."
	}
	if task.ErrorCode == "admin_terminated" {
		notice.Suggestion = ""
	}
	if task.Status == model.ImageTaskExecutionUnknown || task.PublicErrorCode == 608 && task.ErrorCode != "execution_deadline" && task.ErrorCode != "admin_terminated" && !(validDecision && decision.Action == "poll_same_job" && task.UpstreamTaskId != "" && !task.Terminal()) {
		if _, ok := internalSummaries[task.ErrorCode]; !ok && !specificCause {
			notice.Summary = "Upstream execution could not be confirmed"
			notice.SummaryParams = nil
		}
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
