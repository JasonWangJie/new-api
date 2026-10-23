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
	SummaryZh     string
	SuggestionZh  string
	Retryable     bool
	Stage         string
}

var imageFailureNoticeZh = map[string]string{
	"A reference image is required": "需要参考图",
	"Add an eligible account for this model and resolution, or check why the existing account timed out.":                                                    "为该模型和分辨率添加可用的备用账号，或检查现有账号超时的原因。",
	"Adjust both image dimensions to multiples of 16 before submitting again.":                                                                               "请将图片宽度和高度都调整为 16 的整数倍后再提交。",
	"Ask an administrator to reconcile the quota reservation before trying again.":                                                                           "请先由管理员核对额度预留，再尝试提交。",
	"Ask the administrator to add or enable a compatible account, or explicitly choose a supported size or resolution before submitting a new task.":         "请管理员添加或启用支持该尺寸或分辨率的账号；也可自行选择受支持的尺寸或分辨率后新建任务。",
	"Ask the service provider to check for missing or invalid image results; the actual image count is unknown. Do not repeat generation.":                   "上游可能没有可用图片或数量不符合要求，请联系服务方核查完整响应；实际图片数量尚未确认，请勿重复生成。",
	"Ask the service provider to check the request format, model compatibility, and upstream request ID.":                                                    "请联系服务提供方核查请求格式、模型兼容性和上游请求 ID。",
	"Ask the service provider to return a secure HTTPS image URL without embedded credentials. Keep URL safety checks enabled and do not repeat generation.": "请联系服务方修正结果地址，使用不含内嵌账号密码的安全 HTTPS 图片链接；保留地址安全校验，请勿重复生成。",
	"Automatic retries stopped because the configured retry limit was reached. Check compatible routes and retry settings before creating a new task.":       "自动重试次数已达到配置的预算上限，已停止继续提交。请管理员检查兼容线路和重试设置后再新建任务。",
	"Check storage or billing recovery; do not repeat generation.":                                                                                           "请检查存储或计费恢复状态，不要重复生成。",
	"Check that the reference image can be downloaded by the upstream provider.":                                                                             "请确认上游供应商可以下载参考图。",
	"Check the supported reference link format with the service provider, or use a model that accepts your reference material.":                              "请联系服务方核对支持的参考链接格式，或使用支持当前素材的模型。",
	"Check the upstream limits shown in the error, then reduce the input size or adjust the parameters.":                                                     "请核对错误中说明的上游限制，再缩小输入或调整参数。",
	"Check upstream availability and account timeout settings before submitting again.":                                                                      "再次提交前，请检查上游服务可用性及账号超时设置。",
	"Compress each reference image below the stated limit before submitting again.":                                                                          "请压缩每张参考图，确保小于错误中说明的单张大小限制后再提交。",
	"Contact the service provider to check local image request preparation.":                                                                                 "请联系服务提供方核查本地图片请求准备。",
	"Do not submit again until an administrator has checked the upstream result and billing.":                                                                "管理员核查上游结果和计费前，请勿再次提交。",
	"Image capacity is temporarily unavailable":                                                                                                              "图片处理容量暂时不可用",
	"Image execution deadline expired after request timeouts":                                                                                                "图片请求超时后已达到任务总时限",
	"Image execution deadline expired":                    "图片执行期限已到",
	"Image post-processing needs attention":               "图片后处理需要检查",
	"Image quota checks are temporarily unavailable":      "图片额度校验暂时不可用",
	"Image quota reservation could not be confirmed":      "无法确认图片额度预留结果",
	"Image request preparation failed":                    "图片请求准备失败",
	"Image request timeout retry limit reached":           "图片请求超时换号次数已达上限",
	"Image width and height must both be multiples of 16": "图片宽度和高度都必须是 16 的整数倍",
	"No account switch is currently scheduled. Check the task status and compatible route configuration before submitting again.":                                     "当前没有安排换号重试。请先核对任务状态及兼容线路配置，再决定是否重新提交。",
	"No other eligible image account supports the requested size or resolution":                                                                                       "当前没有其他可用上游账号支持本次请求的图片尺寸或分辨率，已停止自动重试。",
	"No unused image account remains after request timeouts":                                                                                                          "请求超时后，没有剩余账号可切换",
	"Reference image could not be fetched":                                                                                                                            "无法获取参考图",
	"Reference image exceeds the upstream limit of {{limit}}":                                                                                                         "参考图超过上游 {{limit}} 限制",
	"Remove the unsupported thinking level parameter or choose a model that supports it.":                                                                             "请移除不受支持的思考级别参数，或选择支持该参数的模型。",
	"Review the prompt and reference material against the upstream content rules before submitting an adjusted request.":                                              "请检查提示词和参考素材是否符合上游内容规则，调整后再提交。",
	"Review the upstream message; contact the service provider if the review failed unexpectedly.":                                                                    "请查看上游说明；若审核失败不符合预期，请联系服务提供方。",
	"Temporary upstream image service failure":                                                                                                                        "上游图片服务暂时异常",
	"The existing output will be processed again; generation will not be repeated.":                                                                                   "将重新处理已有产物，不会重复生成。",
	"The existing upstream job will be checked again; no new generation will be submitted.":                                                                           "将继续查询原上游任务，不会提交新的生成请求。",
	"The generation attempt timed out after {{seconds}} seconds, and no alternative account is available":                                                             "本次生图尝试耗时 {{seconds}} 秒后超时，且没有备用账号可切换",
	"The image format declared by the upstream does not match the image data":                                                                                         "上游标注的图片格式与实际图片数据不一致",
	"The reference image could not be downloaded locally":                                                                                                             "本站下载参考图失败",
	"The selected model does not support the requested thinking level":                                                                                                "当前模型不支持请求中的思考级别参数",
	"The selected upstream route does not support the requested image size or resolution":                                                                             "当前上游线路不支持本次请求的图片尺寸或分辨率档位。",
	"The task execution deadline was reached, so no further submission is scheduled. Check compatible routes before creating a new task.":                             "任务已达到执行截止时间，已停止继续提交。请管理员检查兼容线路，确认后再新建任务。",
	"The task expired before upstream dispatch; submit a new request if it is still needed.":                                                                          "任务在派发上游前已过期，如仍有需要请提交新请求。",
	"The task is scheduled for another submission within its retry limits.":                                                                                           "任务已安排在重试限制内再次提交。",
	"The task is scheduled to try another eligible account within its retry limits, skipping incompatible accounts and preserving the requested size and resolution.": "本任务已安排在重试限额内换号重试，并跳过不兼容账号；保留原图片尺寸和分辨率，不自动降低画质。",
	"The task is waiting for a local dependency to recover before dispatch.":                                                                                          "任务正在等待本地依赖恢复，尚未派发。",
	"The task is waiting for capacity before another upstream submission.":                                                                                            "任务正在等待可用容量后再次提交上游。",
	"The task was manually terminated":                                                                                                                                "任务已被手动终止",
	"The upstream connection ended before returning a complete result (unexpected EOF)":                                                                               "上游结果尚未完整返回，连接就意外结束（unexpected EOF）",
	"The upstream connection ended before returning a result (EOF)":                                                                                                   "上游尚未返回生成结果，连接就已结束（EOF）",
	"The upstream could not fetch the reference image, and the local download fallback also failed":                                                                   "上游抓取参考图失败，本地回退下载也失败",
	"The upstream could not fetch the reference image":                                                                                                                "上游抓取参考图失败",
	"The upstream did not return a usable image":                                                                                                                      "上游没有返回可用图片",
	"The upstream did not return valid complete image results":                                                                                                        "上游未返回符合要求的完整图片结果",
	"The upstream image container is invalid or contains excessive trailing data":                                                                                     "上游返回的图片容器校验失败：结构无效或尾部附加数据过多",
	"The upstream image URL does not meet secure HTTPS requirements":                                                                                                  "上游返回的图片地址不符合安全 HTTPS 要求",
	"The upstream only supports a specific YouTube video URL format":                                                                                                  "上游只支持指定格式的 YouTube 视频链接",
	"The upstream rejected the request under its content rules":                                                                                                       "上游因内容规则拒绝了本次请求",
	"The upstream returned text instead of an image":                                                                                                                  "上游只返回了文字，没有返回图片",
	"This task stopped after the upstream rejection; no account switch is scheduled. Ask the administrator to enable a compatible route before submitting again.":     "本任务在上游拒绝后已停止，没有安排自动换号。请管理员检查支持该尺寸或分辨率的线路及账号，确认可用后再提交。",
	"Too many reference images":                                                           "参考图数量超限",
	"Upstream connection closed before the result was confirmed":                          "上游连接提前断开，生成结果未确认",
	"Upstream could not process the prompt or reference images":                           "上游无法处理提示词或参考图",
	"Upstream execution could not be confirmed":                                           "无法确认上游执行结果",
	"Upstream rate limit exceeded":                                                        "上游请求频率受限",
	"Upstream rejected image input or parameters":                                         "上游拒绝了图片输入或参数",
	"Upstream rejected the request; the exact cause is not confirmed":                     "上游拒绝了请求，具体原因尚未确认",
	"Upstream request timed out":                                                          "上游请求超时",
	"Upstream request was interrupted":                                                    "上游请求被中断",
	"Upstream response could not be read or parsed":                                       "上游响应读取或解析失败",
	"Upstream safety review did not pass":                                                 "上游安全审核未通过",
	"Wait for local quota checks to recover; no upstream generation has been authorized.": "请等待本地额度校验恢复，尚未授权向上游生成。",
}

var (
	imageReferenceLimitNoticePattern = regexp.MustCompile(`(?i)(?:image_url|reference image|参考图|图片)[^\r\n]{0,100}?(?:exceeds?|上限|不得超过|不能超过|超过|(?:size\s+)?limit(?:\s+of)?|maximum(?:\s+size)?)\s*[:：]?\s*(?:the\s+)?(?:limit(?:\s+of)?\s*[:：]?\s*)?([0-9]{1,12}(?:\.[0-9]{1,6})?\s*(?:GB|MB|KB|B))(?:\b|$)`)
	imageInlineLimitNoticePattern    = regexp.MustCompile(`(?i)\bper-image limit is [0-9]{1,12} bytes\s*\(([0-9]{1,12}(?:\.[0-9]{1,6})?\s*[KMG]iB)\)`)
	imageSizeNoticeParamPattern      = regexp.MustCompile(`^[0-9]{1,12}(?:\.[0-9]{1,6})?(?:[KMG]i?B|B)$`)
	imageTextOnlyNoticePattern       = regexp.MustCompile(`(?i)^(?:(?:很)?抱歉[，,：:\s]*|(?:i['’]m sorry|i am sorry|sorry)[,\s]*(?:but\s+)?)?(?:我(?:无法|不能)直接(?:生成或编辑|生成|编辑)图片|i cannot directly (?:generate|edit) images)`)
	imageContentPolicyNoticePattern  = regexp.MustCompile(`(?i)^(?:the request violates content policies\b|we['’]re so sorry, but the (?:image we created|prompt) may violate our content policies\.)`)
	imageOutputCountNoticePattern    = regexp.MustCompile(`^upstream must return between 1 and [0-9]{1,4} complete images$`)
	imageDimensionNoticePattern      = regexp.MustCompile(`(?i)^(?:the requested image size is invalid\.\s*)?width and height must both be divisible by 16\.?$`)
	imageYouTubeNoticePattern        = regexp.MustCompile(`(?i)^(?:provider api error:\s*)?only a specific youtube video url is supported(?: \(request id: [a-z0-9_-]{1,128}\))?\.?$`)
)

// SummaryText keeps API summaries readable for clients that do not implement
// translated summary keys. Only the recorded, bounded display parameters are used.
func (notice ImageFailureNotice) SummaryText() string {
	text := notice.SummaryZh
	if text == "" {
		text = imageFailureNoticeZh[notice.Summary]
	}
	if text == "" {
		if notice.Summary == "" {
			return ""
		}
		text = imageFailureNoticeZh["Upstream rejected the request; the exact cause is not confirmed"]
	}
	if limit, ok := notice.SummaryParams["limit"].(string); ok && imageSizeNoticeParamPattern.MatchString(limit) {
		text = strings.ReplaceAll(text, "{{limit}}", limit)
	}
	if seconds, ok := notice.SummaryParams["seconds"].(int64); ok && seconds > 0 && seconds <= 86400 {
		text = strings.ReplaceAll(text, "{{seconds}}", strconv.FormatInt(seconds, 10))
	}
	if strings.Contains(text, "{{") || strings.Contains(text, "}}") {
		return imageFailureNoticeZh["Upstream rejected the request; the exact cause is not confirmed"]
	}
	return text
}

func (notice ImageFailureNotice) SuggestionText() string {
	if notice.SuggestionZh != "" {
		return notice.SuggestionZh
	}
	if text := imageFailureNoticeZh[notice.Suggestion]; text != "" {
		return text
	}
	if notice.Suggestion == "" {
		return ""
	}
	return imageFailureNoticeZh["Ask the service provider to check the request format, model compatibility, and upstream request ID."]
}

// MessageText explains failures to API clients while preserving the sanitized
// upstream diagnostic separately from the reason and recovery advice.
func (notice ImageFailureNotice) MessageText(raw string) string {
	detail := SanitizeAsyncImageDiagnostic(raw, common.LocalLogContentLimit)
	if notice.SummaryZh == "" {
		return detail
	}
	message := notice.SummaryText()
	if suggestion := notice.SuggestionText(); suggestion != "" {
		message += "\n处理建议：" + suggestion
	}
	if detail != "" {
		message += "\n原始错误：" + detail
	}
	return SanitizeAsyncImageText(message, common.LocalLogContentLimit)
}

// ImageTaskFailureNotice describes the committed recovery action. A transient
// error category alone never promises another generation or an automatic retry.
func ImageTaskFailureNotice(task model.AsyncImageTask) (notice ImageFailureNotice) {
	defer func() {
		if notice.SummaryZh == "" {
			notice.SummaryZh = imageFailureNoticeZh[notice.Summary]
			if notice.Summary != "" && notice.SummaryZh == "" {
				notice.SummaryZh = imageFailureNoticeZh["Upstream rejected the request; the exact cause is not confirmed"]
			}
		}
		if notice.SuggestionZh == "" {
			notice.SuggestionZh = imageFailureNoticeZh[notice.Suggestion]
		}
	}()
	var decision ImageRetryDecision
	validDecision := common.UnmarshalJsonStr(task.RetryDecision, &decision) == nil && decision.Version == 1
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
	specificAdvice := ""
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
		case "image reference requires an HTTPS URL without credentials":
			notice.Summary = "The upstream image URL does not meet secure HTTPS requirements"
			specificAdvice = "Ask the service provider to return a secure HTTPS image URL without embedded credentials. Keep URL safety checks enabled and do not repeat generation."
			specificOutputCause = true
		default:
			if imageOutputCountNoticePattern.MatchString(message) {
				notice.Summary = "The upstream did not return valid complete image results"
				specificAdvice = "Ask the service provider to check for missing or invalid image results; the actual image count is unknown. Do not repeat generation."
				specificOutputCause = true
			}
		}
	case "admin_terminated":
		notice.Summary = "The task was manually terminated"
		specificCause = true
	case "image_reference_accounts_exhausted", "account_switches_exhausted":
		if task.ErrorCode == "image_reference_accounts_exhausted" || imageAttemptCompatibilityCode(lastAttempt) == "upstream_image_reference_incompatible" {
			notice.Summary = "The upstream only supports a specific YouTube video URL format"
			specificAdvice = "Check the supported reference link format with the service provider, or use a model that accepts your reference material."
		}
	case "upstream_failed", "upstream_job_failed", "invalid_reference_image", "upstream_image_reference_incompatible":
		limitMatch := imageReferenceLimitNoticePattern.FindStringSubmatch(message)
		if len(limitMatch) != 2 && strings.Contains(strings.ToLower(message), "inline image") {
			limitMatch = imageInlineLimitNoticePattern.FindStringSubmatch(message)
		}
		lowered := strings.ToLower(message)
		switch {
		case task.PublicErrorCode != 601 && len(limitMatch) == 2:
			limit := strings.ToUpper(strings.Join(strings.Fields(limitMatch[1]), ""))
			limit = strings.ReplaceAll(limit, "IB", "iB")
			notice.Summary = "Reference image exceeds the upstream limit of {{limit}}"
			notice.SummaryParams = map[string]any{"limit": limit}
			specificAdvice = "Compress each reference image below the stated limit before submitting again."
		case task.PublicErrorCode != 601 && imageDimensionNoticePattern.MatchString(message):
			notice.Summary = "Image width and height must both be multiples of 16"
			specificAdvice = "Adjust both image dimensions to multiples of 16 before submitting again."
		case task.PublicErrorCode != 601 && imageYouTubeNoticePattern.MatchString(message):
			notice.Summary = "The upstream only supports a specific YouTube video URL format"
			specificAdvice = "Check the supported reference link format with the service provider, or use a model that accepts your reference material."
		case task.PublicErrorCode != 601 && (lowered == "this model does not support thinking level" || message == "模型不支持 thinking level" || message == "AI Studio 请求参数无效: 模型不支持 thinking level" || message == "AI Studio 请求参数无效：模型不支持 thinking level"):
			notice.Summary = "The selected model does not support the requested thinking level"
			specificAdvice = "Remove the unsupported thinking level parameter or choose a model that supports it."
		case task.PublicErrorCode != 601 && imageContentPolicyNoticePattern.MatchString(message):
			notice.Summary = "The upstream rejected the request under its content rules"
			specificAdvice = "Review the prompt and reference material against the upstream content rules before submitting an adjusted request."
		case task.PublicErrorCode == 602 && lastAttempt.Dispatched && lastAttempt.ReferenceFailure && (lastAttempt.TransportUsed == "remote" || lastAttempt.TransportUsed == "mixed"):
			notice.Summary = "The upstream could not fetch the reference image"
		case task.PublicErrorCode == 610 || task.PublicErrorCode == 605:
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
	if specificAdvice != "" {
		notice.Suggestion = specificAdvice
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
		if specificAdvice == "" {
			notice.Suggestion = "Check storage or billing recovery; do not repeat generation."
		}
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
	compatibilityFailure := task.ErrorCode == "upstream_image_route_incompatible" || task.ErrorCode == "image_compatible_accounts_exhausted"
	switch task.ErrorCode {
	case "upstream_failed", "upstream_job_failed", "account_switches_exhausted", "execution_deadline":
		compatibilityFailure = isImageRouteCompatibilityRejection(lastAttempt.HTTPStatus, lastAttempt.ProviderCode, message) || isImageRouteCompatibilityRejection(lastAttempt.HTTPStatus, lastAttempt.ProviderCode, lastAttempt.ErrorMessage)
	}
	if compatibilityFailure && task.PublicErrorCode != 601 && task.UpstreamSucceededAt == 0 && task.ErrorCode != "admin_terminated" && task.Status != model.ImageTaskStorageFailed && task.Status != model.ImageTaskBillingFailed && task.Status != model.ImageTaskBillingPending {
		notice.Summary = "The selected upstream route does not support the requested image size or resolution"
		notice.SummaryZh = "当前上游线路不支持本次请求的图片尺寸或分辨率档位。"
		notice.Suggestion = "No account switch is currently scheduled. Check the task status and compatible route configuration before submitting again."
		notice.SuggestionZh = "当前没有安排换号重试。请先核对任务状态及兼容线路配置，再决定是否重新提交。"
		switch {
		case task.UpstreamTaskId != "":
			notice.Suggestion = "Do not submit again until an administrator has checked the upstream result and billing."
			notice.SuggestionZh = "管理员核查上游结果和计费前，请勿再次提交。"
			if !task.Terminal() && task.NextAttemptAt > 0 && validDecision && decision.Action == "poll_same_job" {
				notice.Suggestion = "The existing upstream job will be checked again; no new generation will be submitted."
				notice.SuggestionZh = "将继续查询原上游任务，不会提交新的生成请求。"
			}
		case task.ErrorCode == "image_compatible_accounts_exhausted":
			notice.Summary = "No other eligible image account supports the requested size or resolution"
			notice.SummaryZh = "当前没有其他可用上游账号支持本次请求的图片尺寸或分辨率，已停止自动重试。"
			notice.Suggestion = "Ask the administrator to add or enable a compatible account, or explicitly choose a supported size or resolution before submitting a new task."
			notice.SuggestionZh = "请管理员添加或启用支持该尺寸或分辨率的账号；也可自行选择受支持的尺寸或分辨率后新建任务。"
		case task.Terminal() && (task.Status == model.ImageTaskExpired || validDecision && decision.Reason == "execution_deadline"):
			notice.Suggestion = "The task execution deadline was reached, so no further submission is scheduled. Check compatible routes before creating a new task."
			notice.SuggestionZh = "任务已达到执行截止时间，已停止继续提交。请管理员检查兼容线路，确认后再新建任务。"
		case task.Terminal() && validDecision && (decision.Reason == "compatibility_retry_budget_exhausted" || decision.Reason == "total_retry_budget_exhausted" || decision.Reason == "account_switch_limit"):
			notice.Suggestion = "Automatic retries stopped because the configured retry limit was reached. Check compatible routes and retry settings before creating a new task."
			notice.SuggestionZh = "自动重试次数已达到配置的预算上限，已停止继续提交。请管理员检查兼容线路和重试设置后再新建任务。"
		case task.Terminal():
			notice.Suggestion = "This task stopped after the upstream rejection; no account switch is scheduled. Ask the administrator to enable a compatible route before submitting again."
			notice.SuggestionZh = "本任务在上游拒绝后已停止，没有安排自动换号。请管理员检查支持该尺寸或分辨率的线路及账号，确认可用后再提交。"
		case task.NextAttemptAt > 0 && validDecision && decision.Action == "switch_account":
			notice.Suggestion = "The task is scheduled to try another eligible account within its retry limits, skipping incompatible accounts and preserving the requested size and resolution."
			notice.SuggestionZh = "本任务已安排在重试限额内换号重试，并跳过不兼容账号；保留原图片尺寸和分辨率，不自动降低画质。"
		}
	}
	if task.Terminal() || task.NextAttemptAt <= 0 || !validDecision {
		return notice
	}
	switch decision.Action {
	case "retry_submission", "switch_account", "same_account_retry", "local_reference_fallback":
		if task.UpstreamTaskId == "" && task.UpstreamSucceededAt == 0 {
			notice.Retryable = true
			if notice.SuggestionZh == "" {
				notice.Suggestion = "The task is scheduled for another submission within its retry limits."
			}
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
