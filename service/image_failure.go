package service

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type AsyncImageFailure struct {
	Code             int
	InternalCode     string
	Message          string
	HTTPStatus       int
	RetryAfter       time.Duration
	ExecutionUnknown bool
}

func (failure *AsyncImageFailure) Error() string { return failure.Message }

func ImageRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32); err == nil {
		return time.Duration(max(0, min(seconds, 86400))) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		return min(24*time.Hour, max(0, deadline.Sub(now)))
	}
	return 0
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
	case strings.Contains(lowered, "at most") && strings.Contains(lowered, "images") || strings.Contains(lowered, "too_many_reference_images"):
		code = 611
	case strings.Contains(lowered, "prompt or input images could not be processed") || strings.Contains(lowered, "输入图片无法"):
		code = 613
	case strings.Contains(lowered, "请上传") || strings.Contains(lowered, "未检测到参考图") || strings.Contains(lowered, "reference images are required") || strings.Contains(lowered, "missing reference image"):
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
