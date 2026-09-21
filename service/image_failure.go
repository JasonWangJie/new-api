package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const (
	asyncImageProviderErrorBodyLimit = 64 << 10
	asyncImageProviderMessageLimit   = common.LocalLogContentLimit
	asyncImageProviderCodeLimit      = 128
	asyncImageProviderRequestIDLimit = 191
)

var (
	asyncImageBearerCredentialPattern = regexp.MustCompile(`(?i)(\bbearer\s+)[^\s,;"']+`)
	asyncImageNamedCredentialPattern  = regexp.MustCompile(`(?i)(\b(?:api[_ -]?key|authorization|access[_ -]?token|secret|password)\b["']?\s*[:=]\s*["']?)[^\s,;"'}]+`)
	asyncImageGoogleAPIKeyPattern     = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}\b`)
)

type AsyncImageFailure struct {
	Code              int
	InternalCode      string
	Message           string
	HTTPStatus        int
	ProviderCode      string
	ProviderStatus    string
	UpstreamRequestID string
	RetryAfter        time.Duration
	ExecutionUnknown  bool
}

type AsyncImagePending struct{ TaskId string }

func (pending *AsyncImagePending) Error() string { return "upstream image job is pending" }

func (failure *AsyncImageFailure) Error() string { return failure.Message }

func NewImageReferenceFailure(index int, err error) *AsyncImageFailure {
	diagnostic := sanitizeAsyncImageProviderDiagnostic(err.Error(), asyncImageProviderMessageLimit)
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

func ImageRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32); err == nil {
		return time.Duration(max(0, min(seconds, 86400))) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		return min(24*time.Hour, max(0, deadline.Sub(now)))
	}
	return 0
}

func sanitizeAsyncImageProviderDiagnostic(value string, limit int) string {
	value = strings.TrimSpace(common.MaskSensitiveInfo(value))
	value = asyncImageBearerCredentialPattern.ReplaceAllString(value, "${1}***")
	value = asyncImageNamedCredentialPattern.ReplaceAllString(value, "${1}***")
	value = asyncImageGoogleAPIKeyPattern.ReplaceAllString(value, "***")
	return limitAsyncImageProviderDiagnostic(value, limit)
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

func findAsyncImageProviderRequestID(value any) string {
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
		for _, child := range typed {
			if requestID := findAsyncImageProviderRequestID(child); requestID != "" {
				return requestID
			}
		}
	case []any:
		for _, child := range typed {
			if requestID := findAsyncImageProviderRequestID(child); requestID != "" {
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
	if response == nil {
		return ClassifyAsyncImageFailure(errors.New("upstream Gemini response is missing"), 0)
	}
	status := response.StatusCode
	var body []byte
	var readErr error
	if response.Body != nil {
		body, readErr = io.ReadAll(io.LimitReader(response.Body, asyncImageProviderErrorBodyLimit+1))
		CloseResponseBodyGracefully(response)
	}
	if len(body) > asyncImageProviderErrorBodyLimit {
		body = body[:asyncImageProviderErrorBodyLimit]
	}

	var envelope struct {
		Error common.RawMessage `json:"error"`
	}
	var providerError struct {
		Message string            `json:"message"`
		Code    common.RawMessage `json:"code"`
		Status  string            `json:"status"`
		Type    string            `json:"type"`
	}
	if common.Unmarshal(body, &envelope) == nil && common.GetJsonType(envelope.Error) == "object" {
		_ = common.Unmarshal(envelope.Error, &providerError)
	}
	providerMessage := sanitizeAsyncImageProviderDiagnostic(providerError.Message, asyncImageProviderMessageLimit)
	providerCode := ""
	switch common.GetJsonType(providerError.Code) {
	case "string", "number":
		providerCode = common.JsonRawMessageToString(providerError.Code)
	}
	if providerCode == "" {
		providerCode = providerError.Type
	}
	providerCode = limitAsyncImageProviderDiagnostic(providerCode, asyncImageProviderCodeLimit)
	providerStatus := limitAsyncImageProviderDiagnostic(providerError.Status, asyncImageProviderCodeLimit)
	if providerCode == "" {
		providerCode = providerStatus
	}

	requestID := ""
	var decoded any
	if common.Unmarshal(body, &decoded) == nil {
		requestID = findAsyncImageProviderRequestID(decoded)
	}
	if requestID == "" {
		for _, header := range []string{"X-Goog-Request-Id", common.UpstreamRequestIdKey, common.RequestIdKey, "X-Request-Id", "Request-Id"} {
			if requestID = response.Header.Get(header); requestID != "" {
				break
			}
		}
	}
	requestID = limitAsyncImageProviderDiagnostic(requestID, asyncImageProviderRequestIDLimit)

	var normalizedErr error
	if readErr != nil {
		normalizedErr = fmt.Errorf("read upstream Gemini error response: %w", readErr)
	} else {
		responseCopy := *response
		responseCopy.Body = io.NopCloser(bytes.NewReader(body))
		normalizedErr = RelayErrorHandler(ctx, &responseCopy, false)
	}
	classificationMessage := providerMessage
	if classificationMessage == "" {
		classificationMessage = normalizedErr.Error()
	}
	classificationSignals := strings.TrimSpace(strings.Join([]string{classificationMessage, providerCode, providerStatus}, " "))
	failure := ClassifyAsyncImageFailure(errors.New(classificationSignals), status)
	if providerMessage != "" {
		failure.Message = providerMessage
	}
	failure.Message = sanitizeAsyncImageProviderDiagnostic(failure.Message, asyncImageProviderMessageLimit)
	failure.ProviderCode = providerCode
	failure.ProviderStatus = providerStatus
	failure.UpstreamRequestID = requestID
	failure.RetryAfter = ImageRetryAfter(response.Header.Get("Retry-After"), time.Now())
	return failure
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
	switch {
	case strings.Contains(lowered, "content_policy") || strings.Contains(lowered, "safety") || strings.Contains(lowered, "policy violation") || strings.Contains(lowered, "内容安全") || strings.Contains(lowered, "政策") || strings.Contains(lowered, "third-party"):
		code = 601
	case strings.Contains(lowered, "image_url fetch") || strings.Contains(lowered, "image reference download") || strings.Contains(lowered, "reference dns") || strings.Contains(lowered, "fetch image") || strings.Contains(lowered, "参考图拉取"):
		code = 602
	case strings.Contains(lowered, "at most") && strings.Contains(lowered, "images") || strings.Contains(lowered, "too_many_reference_images") || strings.Contains(lowered, "too_many_images"):
		code = 611
	case strings.Contains(lowered, "prompt or input images could not be processed") || strings.Contains(lowered, "prompt_or_input_images_could_not_be_processed") || strings.Contains(lowered, "input_image_processing_failed") || strings.Contains(lowered, "no_image") || strings.Contains(lowered, "输入图片无法"):
		code = 613
	case strings.Contains(lowered, "请上传") || strings.Contains(lowered, "未检测到参考图") || strings.Contains(lowered, "reference images are required") || strings.Contains(lowered, "missing reference image") || strings.Contains(lowered, "reference_image_required") || strings.Contains(lowered, "no_reference_image"):
		code = 612
	case strings.Contains(lowered, "capacity") || strings.Contains(lowered, "quota exhausted") || strings.Contains(lowered, "no eligible image channel"):
		code = 603
	case strings.Contains(lowered, "unsupported_image_dimensions") || strings.Contains(lowered, "invalid_request") || strings.Contains(lowered, "eligibility"):
		code = 604
	case status == 429:
		code = 605
	case status >= 500:
		code = 606
	}
	// Upstream error text may echo credentials, signed URLs or private prompts.
	// Persist a stable public explanation instead of that untrusted payload.
	reasons := map[int]string{601: "Upstream content or safety policy rejected the request", 602: "Upstream reference-image fetch failed", 603: "Image execution capacity is unavailable", 604: "Upstream rejected image parameters or dimensions", 605: "Upstream image rate limit exceeded", 606: "Temporary upstream image service failure", 610: "Upstream image request failed", 611: "Upstream reference-image count limit exceeded", 612: "Upstream requires a reference image", 613: "Upstream could not process the prompt or input images"}
	return &AsyncImageFailure{Code: code, InternalCode: "upstream_failed", Message: reasons[code], HTTPStatus: status}
}
