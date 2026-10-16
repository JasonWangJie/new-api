package service

import (
	"bytes"
	"io"
	"maps"
	"math"
	"mime"
	"mime/multipart"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// AsyncVideoRequestDetails contains only client-supplied display data. The
// channel, credentials, execution and billing snapshots never belong here.
type AsyncVideoRequestDetails struct {
	RequestedSeconds  *float64                   `json:"requested_seconds,omitempty"`
	ReferenceMedia    []AsyncVideoReferenceMedia `json:"reference_media,omitempty"`
	RequestParameters map[string]any             `json:"request_parameters,omitempty"`
}

type AsyncVideoReferenceMedia struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// RequestDetails reads legacy queued envelopes as well as the small display
// snapshots retained after submission. Uploaded bytes are never included.
func (request AsyncVideoRequest) RequestDetails() *AsyncVideoRequestDetails {
	if request.Details != nil {
		return request.Details
	}
	var parameters map[string]any
	mediaType, params, err := mime.ParseMediaType(request.ContentType)
	if err != nil {
		return nil
	}
	switch mediaType {
	case "application/json":
		if common.Unmarshal(request.Body, &parameters) != nil {
			return nil
		}
	case "multipart/form-data":
		parameters = make(map[string]any)
		reader := multipart.NewReader(bytes.NewReader(request.Body), params["boundary"])
		for range 256 {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil
			}
			name := part.FormName()
			if part.FileName() != "" {
				parameters[name] = map[string]any{"filename": part.FileName(), "content_type": part.Header.Get("Content-Type"), "content": "[binary omitted]"}
				continue
			}
			value, err := io.ReadAll(io.LimitReader(part, (64<<10)+1))
			if err != nil {
				return nil
			}
			if len(value) > 64<<10 {
				parameters[name] = "[omitted]"
				continue
			}
			var structured any
			if strings.HasPrefix(strings.TrimSpace(string(value)), "{") || strings.HasPrefix(strings.TrimSpace(string(value)), "[") {
				if common.Unmarshal(value, &structured) == nil {
					parameters[name] = structured
					continue
				}
			}
			parameters[name] = string(value)
		}
	default:
		return nil
	}
	if parameters == nil {
		return nil
	}
	sanitizer := asyncVideoRequestSanitizer{remaining: 256 << 10, nodes: 4096}
	parameters = sanitizer.sanitize(parameters, 0).(map[string]any)
	details := &AsyncVideoRequestDetails{RequestParameters: parameters}
	// Check only generation settings, never durations of reference material or
	// inferred billing ratios, which need not equal the user's requested length.
	settings := []map[string]any{parameters}
	for _, key := range []string{"parameters", "metadata", "input"} {
		if nested, ok := parameters[key].(map[string]any); ok {
			settings = append(settings, nested)
			if nestedParameters, ok := nested["parameters"].(map[string]any); ok {
				settings = append(settings, nestedParameters)
			}
		}
	}
	for _, setting := range settings {
		for _, key := range []string{"seconds", "duration"} {
			value, exists := setting[key]
			if !exists {
				continue
			}
			var seconds float64
			switch typed := value.(type) {
			case float64:
				seconds = typed
			case string:
				seconds, _ = strconv.ParseFloat(typed, 64)
			}
			if !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds > 0 && seconds <= relaycommon.MaxTaskDurationSeconds {
				details.RequestedSeconds = &seconds
				break
			}
		}
		if details.RequestedSeconds != nil {
			break
		}
	}
	details.collectReferenceMedia(parameters, "", make(map[string]bool))
	return details
}

type asyncVideoRequestSanitizer struct {
	remaining int
	nodes     int
}

func (sanitizer *asyncVideoRequestSanitizer) sanitize(value any, depth int) any {
	if depth > 16 || sanitizer.remaining <= 0 || sanitizer.nodes <= 0 {
		return "[omitted]"
	}
	sanitizer.nodes--
	switch typed := value.(type) {
	case map[string]any:
		output := make(map[string]any)
		for _, key := range slices.Sorted(maps.Keys(typed)) {
			if len(output) >= 256 || sanitizer.remaining <= 0 || sanitizer.nodes <= 0 {
				break
			}
			if len(key) > 256 {
				continue
			}
			sanitizer.remaining -= len(key)
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(key))
			switch normalized {
			case "key", "apikey", "authorization", "accesstoken", "refreshtoken", "token", "password", "secret", "secretkey", "privatekey", "cookie", "cookies", "credential", "credentials", "session", "sessionid", "bearer", "channel", "channelcipher", "requestcipher", "billing", "execution", "clientip", "pluginhash":
				output[key] = "[redacted]"
				continue
			}
			if strings.Contains(normalized, "base64") || normalized == "b64json" {
				output[key] = "[binary omitted]"
				continue
			}
			if strings.Contains(normalized, "password") || strings.HasSuffix(normalized, "apikey") || strings.HasSuffix(normalized, "accesskey") || strings.HasSuffix(normalized, "privatekey") || strings.HasSuffix(normalized, "secretkey") || strings.HasSuffix(normalized, "secret") || strings.HasSuffix(normalized, "authorization") || strings.HasSuffix(normalized, "token") || normalized == "awsaccesskeyid" {
				output[key] = "[redacted]"
				continue
			}
			output[key] = sanitizer.sanitize(typed[key], depth+1)
		}
		return output
	case []any:
		output := make([]any, 0, min(len(typed), 256))
		for _, child := range typed {
			if len(output) >= 256 || sanitizer.remaining <= 0 || sanitizer.nodes <= 0 {
				break
			}
			output = append(output, sanitizer.sanitize(child, depth+1))
		}
		return output
	case string:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(typed)), "data:") || len(typed) > 64<<10 {
			return "[binary or oversized value omitted]"
		}
		lower := strings.ToLower(typed)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			parsed, err := url.Parse(typed)
			if err != nil || parsed.User != nil || parsed.Hostname() == "" || strings.ContainsAny(typed, "\\") || strings.ContainsFunc(typed, unicode.IsControl) {
				return "[unsafe URL omitted]"
			}
			if len(typed) > sanitizer.remaining {
				return "[omitted]"
			}
			sanitizer.remaining -= len(typed)
			return typed
		}
		// Keep caller-owned signed media URLs usable, while masking credential
		// patterns embedded in ordinary text. No request headers are read here.
		typed = asyncImageBearerCredentialPattern.ReplaceAllString(typed, "${1}***")
		typed = asyncImageNamedCredentialPattern.ReplaceAllString(typed, "${1}***")
		typed = asyncImageGoogleAPIKeyPattern.ReplaceAllString(typed, "***")
		typed = asyncImageOpenAIAPIKeyPattern.ReplaceAllString(typed, "***")
		if len(typed) > sanitizer.remaining {
			return "[omitted]"
		}
		sanitizer.remaining -= len(typed)
		return typed
	default:
		sanitizer.remaining -= 16
		return value
	}
}

func asyncVideoReferenceType(name string) string {
	switch strings.ToLower(name) {
	case "image", "images", "image_url", "img_url", "input_image", "input_reference", "image_tail", "first_frame", "last_frame", "first_frame_url", "last_frame_url", "reference_image", "reference_images":
		return "image"
	case "video", "videos", "video_url", "input_video", "reference_video", "reference_videos":
		return "video"
	case "audio", "audios", "audio_url", "input_audio", "reference_audio", "reference_audios", "driving_audio":
		return "audio"
	default:
		return ""
	}
}

func (details *AsyncVideoRequestDetails) collectReferenceMedia(value any, mediaType string, seen map[string]bool) {
	if len(details.ReferenceMedia) >= 64 {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		if kind, ok := typed["type"].(string); ok {
			if detected := asyncVideoReferenceType(kind); detected != "" {
				mediaType = detected
			}
		}
		for _, key := range slices.Sorted(maps.Keys(typed)) {
			childType := asyncVideoReferenceType(key)
			if childType == "" && (key == "url" || key == "uri") {
				childType = mediaType
			}
			details.collectReferenceMedia(typed[key], childType, seen)
		}
	case []any:
		for _, child := range typed {
			details.collectReferenceMedia(child, mediaType, seen)
		}
	case string:
		if mediaType == "" || len(typed) > 8192 {
			return
		}
		parsed, err := url.Parse(typed)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || strings.ContainsAny(typed, "\\") || strings.ContainsFunc(typed, unicode.IsSpace) || strings.ContainsFunc(typed, unicode.IsControl) || seen[mediaType+":"+typed] {
			return
		}
		seen[mediaType+":"+typed] = true
		details.ReferenceMedia = append(details.ReferenceMedia, AsyncVideoReferenceMedia{Type: mediaType, URL: typed})
	}
}

// retainAsyncVideoRequestDetails discards the replay envelope once a durable
// upstream task exists. Only an encrypted, bounded display snapshot survives
// until the existing task-expiry cleanup clears RequestCipher.
func retainAsyncVideoRequestDetails(job model.AsyncMediaJob) []byte {
	if len(job.RequestCipher) == 0 {
		return nil
	}
	plain, err := DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
	var request AsyncVideoRequest
	if err == nil {
		err = common.Unmarshal(plain, &request)
	}
	if err != nil {
		common.SysError("media task " + job.TaskId + ": request details could not be retained")
		return nil
	}
	if request.Details != nil && len(request.Body) == 0 {
		var envelope map[string]common.RawMessage
		if common.Unmarshal(plain, &envelope) == nil && len(envelope) == 1 && envelope["request_details"] != nil {
			return job.RequestCipher
		}
	}
	details := request.RequestDetails()
	if details == nil {
		return nil
	}
	plain, err = common.Marshal(map[string]any{"request_details": details})
	if err == nil {
		plain, err = EncryptImagePayload(plain, "media-request:"+job.TaskId)
	}
	if err != nil {
		common.SysError("media task " + job.TaskId + ": request details could not be encrypted")
		return nil
	}
	return plain
}
