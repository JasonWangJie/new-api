package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
)

const (
	asyncImageProviderErrorBodyLimit = 64 << 10
	asyncImageProviderMessageLimit   = common.LocalLogContentLimit
	asyncImageProviderCodeLimit      = 128
	asyncImageProviderRequestIDLimit = 191
)

var (
	asyncImageBearerCredentialPattern = regexp.MustCompile(`(?i)(\bbearer\s+)[^\s,;"']+`)
	asyncImageNamedCredentialPattern  = regexp.MustCompile(`(?i)(\b(?:api[_ -]?key|authorization|access[_ -]?token|secret|password)\b(?:\s+(?:provided|supplied))?["']?\s*[:=]\s*["']?)[^\s,;"'}]+`)
	asyncImageGoogleAPIKeyPattern     = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}\b`)
	asyncImageOpenAIAPIKeyPattern     = regexp.MustCompile(`\bsk-[0-9A-Za-z_-]+`)
)

type AsyncImageFailure struct {
	Code                         int
	InternalCode                 string
	Message                      string
	HTTPStatus                   int
	ProviderCode                 string
	ProviderStatus               string
	UpstreamRequestID            string
	RetryAfter                   time.Duration
	ExecutionUnknown             bool
	UpstreamTerminal             bool `json:"-"`
	RetryableHTTPResponse        bool `json:"-"`
	SynchronousRetryableResponse bool `json:"-"`
	ReferenceTransportFallback   bool
}

type AsyncImagePending struct{ TaskId string }

func (pending *AsyncImagePending) Error() string { return "upstream image job is pending" }

func (failure *AsyncImageFailure) Error() string { return failure.Message }

func NewImageReferenceFailure(index int, err error) *AsyncImageFailure {
	diagnostic := SanitizeAsyncImageDiagnostic(err.Error(), asyncImageProviderMessageLimit)
	message := fmt.Sprintf("Reference image %d: %s", max(1, index), diagnostic)
	var existing *AsyncImageFailure
	if errors.As(err, &existing) {
		failure := *existing
		failure.Message = message
		return &failure
	}
	if IsImageValidationError(err) {
		return &AsyncImageFailure{Code: 604, InternalCode: "invalid_reference_image", Message: message}
	}
	return &AsyncImageFailure{Code: 602, InternalCode: "reference_download_failed", Message: message}
}

func IsImageReferenceDownloadTimeout(failure *AsyncImageFailure) bool {
	if failure == nil || failure.Code != 602 {
		return false
	}
	message := strings.ToLower(failure.Message)
	return strings.Contains(message, "timeout") || strings.Contains(message, "timed out") || strings.Contains(message, "deadline exceeded") || strings.Contains(message, "exceeded its total time budget") || strings.Contains(message, "超时")
}

func ImageRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32); err == nil {
		return time.Duration(max(0, min(seconds, 86400))) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		return min(24*time.Hour, max(0, deadline.Sub(now)))
	}
	return 0
}

// SanitizeAsyncImageDiagnostic bounds diagnostic text and masks credentials and URLs.
func SanitizeAsyncImageDiagnostic(value string, limit int) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[") {
		var decoded any
		if common.UnmarshalJsonStr(value, &decoded) != nil {
			return ""
		}
		value = asyncImageErrorMessage(decoded, 0)
	}
	return SanitizeAsyncImageText(value, limit)
}

// SanitizeAsyncImageText preserves explicitly permitted user text, including
// JSON-shaped prompts, while bounding it and masking credentials and URLs.
func SanitizeAsyncImageText(value string, limit int) string {
	value = strings.TrimSpace(common.MaskSensitiveInfo(value))
	value = asyncImageBearerCredentialPattern.ReplaceAllString(value, "${1}***")
	value = asyncImageNamedCredentialPattern.ReplaceAllString(value, "${1}***")
	value = asyncImageGoogleAPIKeyPattern.ReplaceAllString(value, "***")
	value = asyncImageOpenAIAPIKeyPattern.ReplaceAllString(value, "***")
	return limitAsyncImageProviderDiagnostic(value, limit)
}

// SanitizeAsyncImageAttempt also protects legacy attempts read from storage;
// newly persisted diagnostics already pass through the same field limits.
func SanitizeAsyncImageAttempt(attempt ImageChannelAttempt) ImageChannelAttempt {
	attempt.ErrorMessage = SanitizeAsyncImageDiagnostic(attempt.ErrorMessage, asyncImageProviderMessageLimit)
	attempt.ProviderCode = SanitizeAsyncImageDiagnostic(attempt.ProviderCode, asyncImageProviderCodeLimit)
	attempt.ProviderStatus = SanitizeAsyncImageDiagnostic(attempt.ProviderStatus, asyncImageProviderCodeLimit)
	attempt.UpstreamRequestID = SanitizeAsyncImageDiagnostic(attempt.UpstreamRequestID, asyncImageProviderRequestIDLimit)
	return attempt
}

// asyncImageErrorMessage only reads diagnostic fields, never echoed request
// input, prompts, credentials or a serialized copy of the response object.
func asyncImageErrorMessage(value any, depth int) string {
	if depth >= 6 {
		return ""
	}
	switch typed := value.(type) {
	case string:
		message := strings.TrimSpace(typed)
		if strings.HasPrefix(message, "{") || strings.HasPrefix(message, "[") {
			var decoded any
			if common.UnmarshalJsonStr(message, &decoded) != nil {
				return ""
			}
			return asyncImageErrorMessage(decoded, depth+1)
		}
		return message
	case map[string]any:
		for _, key := range []string{"error", "detail", "message", "msg", "reason"} {
			if message := asyncImageErrorMessage(typed[key], depth+1); message != "" {
				if location, ok := typed["loc"].([]any); ok {
					var path strings.Builder
					for _, part := range location {
						switch part.(type) {
						case string, float64:
							path.WriteByte('/')
							path.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(fmt.Sprint(part)))
						}
					}
					if path.Len() > 0 {
						return path.String() + ": " + message
					}
				}
				return message
			}
		}
	case []any:
		messages := make([]string, 0, min(len(typed), 20))
		for _, item := range typed[:min(len(typed), 20)] {
			if message := asyncImageErrorMessage(item, depth+1); message != "" {
				messages = append(messages, message)
			}
		}
		return strings.Join(messages, "; ")
	}
	return ""
}

// AsyncImageTransportFailure preserves the dispatch barrier while separating
// timeout, connection loss and accepted-response parsing diagnostics.
func AsyncImageTransportFailure(err error, task model.AsyncImageTask, responseInvalid bool, headers http.Header) *AsyncImageFailure {
	code, internal, message := 608, "upstream_transport_disconnected", "Upstream connection was interrupted after dispatch; execution could not be confirmed"
	var networkError net.Error
	switch {
	case responseInvalid:
		internal, message = "upstream_response_invalid", "Upstream response could not be read or parsed after dispatch; execution could not be confirmed"
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout():
		internal, message = "upstream_transport_timeout", "Upstream request timed out after dispatch; execution could not be confirmed"
	case errors.Is(err, context.Canceled):
		internal, message = "upstream_transport_canceled", "Upstream request was interrupted after dispatch; execution could not be confirmed"
	}
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		message += " (unexpected EOF)"
	case errors.Is(err, io.EOF):
		message += " (EOF)"
	}
	unknown := task.UpstreamTaskId == ""
	if !unknown {
		code = 606
		message += "; only the existing upstream job may be polled again"
	}
	return &AsyncImageFailure{Code: code, InternalCode: internal, Message: message, ExecutionUnknown: unknown, UpstreamRequestID: AsyncImageResponseRequestID(headers)}
}

// AsyncImageResponseRequestID accepts provider response headers only. Local
// correlation IDs and request payload fields are not inferred as provider IDs.
func AsyncImageResponseRequestID(headers http.Header) string {
	for _, key := range []string{"X-Goog-Request-Id", common.UpstreamRequestIdKey, common.RequestIdKey, "X-Request-Id", "Request-Id"} {
		if requestID := headers.Get(key); requestID != "" {
			return SanitizeAsyncImageDiagnostic(requestID, asyncImageProviderRequestIDLimit)
		}
	}
	return ""
}

func limitAsyncImageProviderDiagnostic(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}

func findAsyncImageProviderRequestID(value any, depth int) string {
	if depth >= 6 {
		return ""
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", ""))
			if normalized != "requestid" && normalized != "xrequestid" && normalized != "xgoogrequestid" {
				continue
			}
			if requestID, ok := child.(string); ok && strings.TrimSpace(requestID) != "" {
				return requestID
			}
		}
		for _, key := range []string{"error", "detail", "details"} {
			if requestID := findAsyncImageProviderRequestID(typed[key], depth+1); requestID != "" {
				return requestID
			}
		}
	case []any:
		for _, child := range typed[:min(len(typed), 20)] {
			if requestID := findAsyncImageProviderRequestID(child, depth+1); requestID != "" {
				return requestID
			}
		}
	}
	return ""
}

// ClassifyGeminiAsyncImageHTTPFailure reads the provider response before the
// compatibility error layer can replace its diagnostic message. Only bounded,
// sanitized fields are retained on the durable image attempt.
func ClassifyGeminiAsyncImageHTTPFailure(ctx context.Context, response *http.Response) *AsyncImageFailure {
	return classifyAsyncImageHTTPFailure(ctx, response, "", true)
}

// ClassifyOpenAIAsyncImageHTTPFailure preserves provider diagnostics while
// retaining channel status mappings for retry classification. HTTPStatus is
// always the actual response status, rather than the mapped status.
func ClassifyOpenAIAsyncImageHTTPFailure(ctx context.Context, response *http.Response, statusCodeMapping string) *AsyncImageFailure {
	return classifyAsyncImageHTTPFailure(ctx, response, statusCodeMapping, false)
}

func isImageRouteCompatibilityRejection(status int, providerCode, message string) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(providerCode), "image_size_not_supported") && !strings.Contains(strings.ToLower(message), "no eligible upstream image provider supports the requested image size or resolution tier") {
		return false
	}
	// A policy rejection must retain its terminal semantics even if the
	// upstream response also mentions an unavailable size or route.
	return ClassifyAsyncImageFailure(errors.New(providerCode+" "+message), status).Code != 601
}

// imageAccountCompatibilityCode only permits a new account after an explicit
// input rejection. Authentication, policy and ambiguous server failures keep
// their existing execution and retry semantics.
func imageAccountCompatibilityCode(status int, providerCode, message string) string {
	if isImageRouteCompatibilityRejection(status, providerCode, message) {
		return "upstream_image_route_incompatible"
	}
	if status == http.StatusBadRequest && isGenericImageInputRejectionCode(providerCode) && imageYouTubeNoticePattern.MatchString(strings.TrimSpace(message)) {
		return "upstream_image_reference_incompatible"
	}
	return ""
}

func isGenericImageInputRejectionCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "", "400", "invalid_argument", "invalid_request", "invalid_request_error":
		return true
	}
	return false
}

func isUndiagnosedImageSubmissionResponse(body []byte) bool {
	if strings.TrimSpace(string(body)) == "" {
		return true
	}
	var fields map[string]any
	if common.Unmarshal(body, &fields) != nil || fields == nil {
		return false
	}
	scopes := []map[string]any{fields}
	switch providerError := fields["error"].(type) {
	case nil:
	case string:
		if strings.TrimSpace(providerError) != "" {
			return false
		}
	case map[string]any:
		scopes = append(scopes, providerError)
	default:
		return false
	}
	for index, scope := range scopes {
		for field, value := range scope {
			switch field {
			case "error":
				if index != 0 {
					return false
				}
			case "code", "type", "status":
				if value == nil {
					continue
				}
				if _, numeric := value.(float64); numeric && field != "code" {
					return false
				}
				if !isGenericImageInputRejectionCode(fmt.Sprint(value)) {
					return false
				}
			case "message", "msg", "detail", "reason":
				if value != nil && value != "" {
					return false
				}
			case "request_id", "requestId", "requestID":
				if _, ok := value.(string); value != nil && !ok {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

// isRetryableImageGatewayResponse recognizes the bounded gateway error response
// allowed by the retry policy. The relay must confirm synchronous execution before the
// worker may use it to submit another generation.
func isRetryableImageGatewayResponse(failure *AsyncImageFailure, decoded any) bool {
	if failure == nil || failure.HTTPStatus != http.StatusBadGateway || failure.Code != 606 || failure.InternalCode != "upstream_failed" || failure.ProviderCode != "upstream_request_failed" {
		return false
	}
	envelope, ok := decoded.(map[string]any)
	if !ok {
		return false
	}
	providerError, ok := envelope["error"].(map[string]any)
	if !ok || providerError["code"] != "upstream_request_failed" {
		return false
	}
	for index, scope := range []map[string]any{envelope, providerError} {
		for field, value := range scope {
			if index == 0 && field == "error" {
				continue
			}
			if field == "request_id" || field == "requestId" || field == "requestID" {
				if _, ok := value.(string); value != nil && !ok {
					return false
				}
				continue
			}
			if field == "param" {
				if value != nil && value != "" {
					return false
				}
				continue
			}
			if index == 0 {
				return false
			}
			text, isString := value.(string)
			if value != nil && !isString {
				return false
			}
			switch field {
			case "code":
				if text != "upstream_request_failed" {
					return false
				}
			case "type":
				switch text {
				case "", "upstream_request_failed", "upstream_error", "server_error", "api_error", "new_api_error", "one_api_error", "invalid_request_error", "bad_gateway":
				default:
					return false
				}
			case "status":
				switch strings.ToLower(text) {
				case "", "error", "failed", "failure", "bad_gateway", "502":
				default:
					return false
				}
			case "message", "msg", "detail", "reason":
				if ClassifyAsyncImageFailure(errors.New(text), http.StatusBadRequest).Code != 610 {
					return false
				}
				lowered := strings.ToLower(text)
				for _, signal := range []string{"unauthorized", "unauthenticated", "authentication", "authorization", "permission", "forbidden", "api key", "api_key", "access token", "access_token", "invalid", "unsupported", "required", "must be", "bad request", "validation", "task_id", "taskid", "job_id", "jobid", "accepted", "success", "succeeded", "completed", "result", "image_url", "b64_json", "base64", "output_url", "参数", "鉴权", "认证", "授权", "无权限", "密钥", "不支持", "格式错误", "尺寸无效", "已受理", "已接收", "生成成功", "完成生成"} {
					if strings.Contains(lowered, signal) {
						return false
					}
				}
			default:
				return false
			}
		}
	}
	return true
}

func imageAttemptCompatibilityCode(attempt ImageChannelAttempt) string {
	if !attempt.Dispatched || attempt.FinishedAt <= 0 || attempt.UpstreamTaskID != "" || attempt.Code == 601 {
		return ""
	}
	if ClassifyAsyncImageFailure(errors.New(attempt.ProviderStatus), attempt.HTTPStatus).Code == 601 {
		return ""
	}
	if attempt.Code == 603 && attempt.HTTPStatus == http.StatusBadRequest && attempt.ErrorMessage == "Upstream image request failed" && isGenericImageInputRejectionCode(attempt.ProviderCode) && isGenericImageInputRejectionCode(attempt.ProviderStatus) {
		return "upstream_image_submission_rejected"
	}
	compatibilityCode := imageAccountCompatibilityCode(attempt.HTTPStatus, attempt.ProviderCode, attempt.ErrorMessage)
	if compatibilityCode == "upstream_image_reference_incompatible" && !isGenericImageInputRejectionCode(attempt.ProviderStatus) {
		return ""
	}
	return compatibilityCode
}

func classifyAsyncImageHTTPFailure(ctx context.Context, response *http.Response, statusCodeMapping string, classifyErrorType bool) *AsyncImageFailure {
	if response == nil {
		return ClassifyAsyncImageFailure(errors.New("upstream image response is missing"), 0)
	}
	status := response.StatusCode
	var body []byte
	var readErr error
	if response.Body != nil {
		body, readErr = io.ReadAll(io.LimitReader(response.Body, asyncImageProviderErrorBodyLimit+1))
		CloseResponseBodyGracefully(response)
	}
	bodyTruncated := len(body) > asyncImageProviderErrorBodyLimit
	if bodyTruncated {
		body = body[:asyncImageProviderErrorBodyLimit]
	}

	var envelope struct {
		Error common.RawMessage `json:"error"`
	}
	var providerError struct {
		Code   common.RawMessage `json:"code"`
		Status string            `json:"status"`
		Type   string            `json:"type"`
	}
	if common.Unmarshal(body, &envelope) == nil && common.GetJsonType(envelope.Error) == "object" {
		_ = common.Unmarshal(envelope.Error, &providerError)
	}
	var decoded any
	_ = common.Unmarshal(body, &decoded)
	providerMessage := SanitizeAsyncImageDiagnostic(asyncImageErrorMessage(decoded, 0), asyncImageProviderMessageLimit)
	providerCode := ""
	switch common.GetJsonType(providerError.Code) {
	case "string", "number":
		providerCode = common.JsonRawMessageToString(providerError.Code)
	}
	classificationCode := SanitizeAsyncImageDiagnostic(providerCode, asyncImageProviderCodeLimit)
	if providerCode == "" {
		providerCode = providerError.Type
	}
	providerCode = SanitizeAsyncImageDiagnostic(providerCode, asyncImageProviderCodeLimit)
	providerStatus := SanitizeAsyncImageDiagnostic(providerError.Status, asyncImageProviderCodeLimit)
	if providerCode == "" {
		providerCode = providerStatus
	}
	// OpenAI-compatible gateways may use invalid_request_error as a generic
	// error type even for 5xx responses. Keep it for diagnostics without
	// overriding the HTTP retry policy; explicit provider codes still apply.
	if classifyErrorType && providerError.Type != "invalid_request_error" {
		classificationCode = providerCode
	}

	requestID := ""
	if decoded != nil {
		requestID = findAsyncImageProviderRequestID(decoded, 0)
	}
	if requestID == "" {
		requestID = AsyncImageResponseRequestID(response.Header)
	}
	requestID = SanitizeAsyncImageDiagnostic(requestID, asyncImageProviderRequestIDLimit)

	classificationMessage := providerMessage
	if classificationMessage == "" {
		classificationMessage = "Upstream image request failed"
		if readErr != nil {
			classificationMessage = "Upstream image error response could not be read"
		}
	}
	classificationSignals := strings.TrimSpace(strings.Join([]string{classificationMessage, classificationCode, providerStatus}, " "))
	mappedStatus := types.InitOpenAIError(types.ErrorCodeBadResponseStatusCode, status)
	ResetStatusCode(mappedStatus, statusCodeMapping)
	failure := ClassifyAsyncImageFailure(errors.New(classificationSignals), mappedStatus.StatusCode)
	compatibilityCode := imageAccountCompatibilityCode(status, classificationCode, providerMessage)
	if compatibilityCode == "upstream_image_reference_incompatible" && (!isGenericImageInputRejectionCode(providerCode) || !isGenericImageInputRejectionCode(providerError.Type) || !isGenericImageInputRejectionCode(providerStatus)) {
		compatibilityCode = ""
	}
	// OpenAI error types are normally diagnostic-only, but a policy type must
	// still prevent an explicit compatibility message from authorizing failover.
	if ClassifyAsyncImageFailure(errors.New(providerError.Type), status).Code == 601 {
		compatibilityCode = ""
	}
	if failure.Code != 601 && compatibilityCode != "" {
		failure.Code, failure.InternalCode = 603, compatibilityCode
	}
	// A complete, explicit submission rejection without any diagnostic may use
	// another account. Do not infer this from a read failure or a provider's
	// authentication, policy or input error fields.
	if status == http.StatusBadRequest && readErr == nil && !bodyTruncated && providerMessage == "" && failure.Code == 610 && isGenericImageInputRejectionCode(providerCode) && isGenericImageInputRejectionCode(providerError.Type) && isGenericImageInputRejectionCode(providerStatus) && isUndiagnosedImageSubmissionResponse(body) {
		failure.Code, failure.InternalCode = 603, "upstream_image_submission_rejected"
		failure.Message = "Upstream image request failed"
	}
	failure.HTTPStatus = status
	if providerMessage != "" {
		failure.Message = providerMessage
	}
	failure.Message = SanitizeAsyncImageDiagnostic(failure.Message, asyncImageProviderMessageLimit)
	failure.ProviderCode = providerCode
	failure.ProviderStatus = providerStatus
	failure.UpstreamRequestID = requestID
	failure.RetryAfter = ImageRetryAfter(response.Header.Get("Retry-After"), time.Now())
	if !classifyErrorType && readErr == nil && !bodyTruncated {
		failure.RetryableHTTPResponse = isRetryableImageGatewayResponse(failure, decoded)
	}
	return failure
}

// ShouldFallbackGeminiReferenceTransport accepts explicit reference fetch
// failures without requiring Google's optional error envelope fields. Ambiguous
// errors still require the narrow compatibility response. The caller must also
// verify that fallback is configured and a remote fileData reference was sent.
func ShouldFallbackGeminiReferenceTransport(failure *AsyncImageFailure) bool {
	if failure == nil || failure.HTTPStatus != http.StatusBadRequest || failure.ExecutionUnknown {
		return false
	}
	if failure.Code == 602 {
		return true
	}
	if !strings.EqualFold(failure.ProviderStatus, "INVALID_ARGUMENT") || failure.ProviderCode != "400" {
		return false
	}
	return failure.Code == 601 || failure.Code == 610 || failure.Code == 613
}

// ClassifyAsyncImageFailure gives policy and reference-network semantics
// precedence over generic input wording. Unknown upstream 400s stay unclassified.
func ClassifyAsyncImageFailure(err error, status int) *AsyncImageFailure {
	var existing *AsyncImageFailure
	if errors.As(err, &existing) {
		return existing
	}
	message := err.Error()
	lowered := strings.ToLower(message)
	code := 610
	var networkError net.Error
	switch {
	case strings.Contains(lowered, "content_policy") || strings.Contains(lowered, "safety") || strings.Contains(lowered, "guardrail") || strings.Contains(lowered, "refusal") || strings.Contains(lowered, "policy violation") || strings.Contains(lowered, "内容安全") || strings.Contains(lowered, "内容审核") || strings.Contains(lowered, "安全审核") || strings.Contains(lowered, "图片审核失败") || strings.Contains(lowered, "政策") || strings.Contains(lowered, "third-party policy"):
		code = 601
	case strings.Contains(lowered, "at most") && strings.Contains(lowered, "images") || strings.Contains(lowered, "too_many_reference_images") || strings.Contains(lowered, "too_many_images"):
		code = 611
	case strings.Contains(lowered, "prompt or input images could not be processed") || strings.Contains(lowered, "prompt_or_input_images_could_not_be_processed") || strings.Contains(lowered, "input_image_processing_failed") || strings.Contains(lowered, "no_image") || strings.Contains(lowered, "输入图片无法"):
		code = 613
	case strings.Contains(lowered, "请上传") || strings.Contains(lowered, "未检测到参考图") || strings.Contains(lowered, "reference images are required") || strings.Contains(lowered, "missing reference image") || strings.Contains(lowered, "reference_image_required") || strings.Contains(lowered, "no_reference_image"):
		code = 612
	case status == http.StatusRequestEntityTooLarge || strings.Contains(lowered, "unsupported_image_dimensions") || strings.Contains(lowered, "invalid_image") || strings.Contains(lowered, "image_url exceeds") || strings.Contains(lowered, "image byte limit exceeded") || strings.Contains(lowered, "image reference byte limit exceeded") || strings.Contains(lowered, "参考图过大") || strings.Contains(lowered, "image too large") || strings.Contains(lowered, "image is too large") || strings.Contains(lowered, "image size exceeds") || strings.Contains(lowered, "prompt cannot exceed") || strings.Contains(lowered, "prompt exceeds") || strings.Contains(lowered, "prompt is too long") || strings.Contains(lowered, "prompt_too_long") || strings.Contains(lowered, "maximum prompt length") || strings.Contains(lowered, "提示词过长") || strings.Contains(lowered, "提示词长度超限"):
		code = 604
	case strings.Contains(lowered, "image_url fetch") || strings.Contains(lowered, "image reference download") || strings.Contains(lowered, "reference image fetch") || strings.Contains(lowered, "reference image download") || strings.Contains(lowered, "reference material download") || strings.Contains(lowered, "reference material fetch") || strings.Contains(lowered, "reference dns") || strings.Contains(lowered, "fetch image") || strings.Contains(lowered, "参考图拉取") || strings.Contains(lowered, "参考素材下载") || strings.Contains(lowered, "参考素材拉取") || strings.Contains(lowered, "参考素材链接无法访问") || strings.Contains(lowered, "参考图链接无法访问") || strings.Contains(lowered, "国内下载中转失败"):
		code = 602
	case strings.Contains(lowered, "capacity") || strings.Contains(lowered, "quota exhausted") || strings.Contains(lowered, "no eligible image channel") || strings.Contains(lowered, "no eligible image account"):
		code = 603
	case strings.Contains(lowered, "invalid_request") || strings.Contains(lowered, "eligibility"):
		code = 604
	case (status == 0 || status == http.StatusBadRequest) && (strings.Contains(lowered, "服务繁忙,请稍后重试") || strings.Contains(lowered, "服务繁忙，请稍后重试") || strings.Contains(lowered, "asset name service busy, please try again later") || strings.Contains(lowered, "internal error encountered.")):
		code = 606
	case (status == 0 || status == http.StatusBadRequest) && (strings.Contains(lowered, "service timeout, please try again later") || strings.Contains(lowered, "生成超时，请重试") || strings.Contains(lowered, "生成超时,请重试")):
		code = 606
	case status == 0 && (errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout()):
		code = 606
	case status == 429:
		code = 605
	case status >= 500:
		code = 606
	}
	// Upstream error text may echo credentials, signed URLs or private prompts.
	// Persist a stable public explanation instead of that untrusted payload.
	reasons := map[int]string{601: "Upstream content or image review did not pass", 602: "Upstream reference-image fetch failed", 603: "Image execution capacity is unavailable", 604: "Upstream rejected image parameters or dimensions", 605: "Upstream image rate limit exceeded", 606: "Temporary upstream image service failure", 610: "Upstream image request failed", 611: "Upstream reference-image count limit exceeded", 612: "Upstream requires a reference image", 613: "Upstream could not process the prompt or input images"}
	return &AsyncImageFailure{Code: code, InternalCode: "upstream_failed", Message: reasons[code], HTTPStatus: status}
}
