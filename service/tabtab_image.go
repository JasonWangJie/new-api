package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/tidwall/gjson"
)

var tabtabImageModels = []string{
	"gemini-nano-banana-2.1", "gemini-3-pro-image", "gemini-3.1-flash-image", "gemini-3.1-flash-lite-image",
	"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "gpt-image-2-r", "gpt-image-2.5-r", "seedream-5-pro", "mj",
}

// TabtabImageCapability advertises the Task API contract, independently of the
// upstream's synchronous Gemini/OpenAI endpoints and their token usage.
func TabtabImageCapability(modelName string) dto.ImageCapability {
	known := modelName == "" || slices.Contains(tabtabImageModels, modelName)
	parameters := []string{"model", "prompt", "n", "response_format"}
	mask := strings.HasPrefix(modelName, "gpt-image-") && !strings.HasSuffix(modelName, "-r")
	switch {
	case modelName == "mj":
	case modelName == "gemini-3.1-flash-lite-image":
		parameters = append(parameters, "ratio", "aspect_ratio")
	case mask:
		parameters = append(parameters, "size", "quality", "background", "output_format", "width", "height", "mask")
	default:
		parameters = append(parameters, "size", "resolution", "ratio", "aspect_ratio")
		if strings.HasSuffix(modelName, "-r") {
			parameters = append(parameters, "quality")
		}
	}
	return dto.ImageCapability{
		Provider: "tabtab", Protocol: "openai_images", Models: slices.Clone(tabtabImageModels),
		Generate: known, Edit: known, ReferenceField: "images",
		Mask: mask, Parameters: parameters,
	}
}

// TabtabWorkbenchImageCapability intersects an administrator's catalog with
// the mapped model's executable Task API dimensions without changing policy.
func TabtabWorkbenchImageCapability(modelName string, capability ImageModelCapability) (ImageModelCapability, bool) {
	declared := TabtabImageCapability(modelName)
	if !declared.Generate || modelName == "" {
		return capability, false
	}
	capability.MaxOutputImages = 1
	capability.AllowHalfK = false
	resolutions := []string{"1K", "2K", "4K"}
	if modelName == "seedream-5-pro" || slices.Contains(declared.Parameters, "width") {
		resolutions = []string{"1K", "2K"}
	}
	if !slices.Contains(declared.Parameters, "size") {
		// AUTO is a display/routing sentinel. No size argument is sent for a
		// fixed-size upstream, and no resolution is invented for its price.
		capability.Resolutions = []string{"AUTO"}
	} else if len(capability.Resolutions) == 0 {
		capability.Resolutions = resolutions
	} else {
		capability.Resolutions = slices.DeleteFunc(slices.Clone(capability.Resolutions), func(value string) bool { return !slices.Contains(resolutions, value) })
		if len(capability.Resolutions) == 0 {
			return capability, false
		}
	}
	qualities := []string(nil)
	formats, backgrounds := []string(nil), []string(nil)
	if slices.Contains(declared.Parameters, "quality") {
		qualities = []string{"auto", "low", "medium", "high"}
		if modelName != "gpt-image-2" {
			qualities = append(qualities, "xhigh", "max")
		}
	}
	if slices.Contains(declared.Parameters, "output_format") {
		formats = []string{"png", "jpeg", "webp"}
		backgrounds = []string{"auto", "opaque", "transparent"}
	}
	for _, dimension := range []struct {
		current *[]string
		choices []string
	}{{&capability.Qualities, qualities}, {&capability.Formats, formats}, {&capability.Backgrounds, backgrounds}} {
		if len(dimension.choices) == 0 {
			*dimension.current = nil
			continue
		}
		if len(*dimension.current) == 0 {
			*dimension.current = slices.Clone(dimension.choices)
			continue
		}
		*dimension.current = slices.DeleteFunc(slices.Clone(*dimension.current), func(value string) bool { return !slices.Contains(dimension.choices, value) })
		if len(*dimension.current) == 0 {
			return capability, false
		}
	}
	return capability, true
}

// TabtabImageSubmission runs on the canonical JSON after parameter overrides.
// Task API has no count parameter: one accepted task represents one image.
func TabtabImageSubmission(body []byte, modelName string, count int) ([]byte, error) {
	if len(body) > 50<<20 {
		return nil, errors.New("TabTab image request exceeds 50 MiB")
	}
	if count != 1 {
		return nil, errors.New("TabTab Task API image requests require n=1")
	}
	if !slices.Contains(tabtabImageModels, modelName) {
		return nil, errors.New("unsupported TabTab image model")
	}
	var fields map[string]common.RawMessage
	if common.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, errors.New("TabTab image request must be a JSON object")
	}
	for name := range fields {
		if !slices.Contains([]string{"model", "prompt", "n", "size", "resolution", "ratio", "aspect_ratio", "quality", "background", "output_format", "width", "height", "response_format", "images", "mask"}, name) {
			return nil, fmt.Errorf("%s is not supported by TabTab image tasks", name)
		}
	}
	var submittedModel, prompt string
	if common.Unmarshal(fields["model"], &submittedModel) != nil || submittedModel != modelName {
		return nil, errors.New("TabTab image model conflicts with the selected upstream model")
	}
	if common.Unmarshal(fields["prompt"], &prompt) != nil || strings.TrimSpace(prompt) == "" {
		return nil, errors.New("TabTab image prompt is required")
	}
	if raw, exists := fields["n"]; exists && string(raw) != "1" {
		return nil, errors.New("TabTab Task API image requests require n=1")
	}
	args := map[string]any{"prompt": prompt}
	if raw, exists := fields["images"]; exists {
		var references []any
		if common.Unmarshal(raw, &references) != nil || len(references) > 128 {
			return nil, errors.New("TabTab image references must be a bounded array")
		}
		images := make([]string, 0, len(references))
		for _, reference := range references {
			image, err := tabtabImageReference(reference)
			if err != nil {
				return nil, err
			}
			images = append(images, image)
		}
		if len(images) > 0 {
			args["images"] = images
		}
	}
	stringsByName := make(map[string]string)
	for _, name := range []string{"size", "resolution", "ratio", "aspect_ratio", "quality", "background", "output_format"} {
		if raw, exists := fields[name]; exists {
			var value string
			if common.Unmarshal(raw, &value) != nil {
				return nil, fmt.Errorf("TabTab %s must be a string", name)
			}
			stringsByName[name] = value
		}
	}
	ratio, alternate := stringsByName["ratio"], stringsByName["aspect_ratio"]
	if ratio != "" && alternate != "" && ratio != alternate {
		return nil, errors.New("TabTab ratio and aspect_ratio must match")
	}
	if ratio == "" {
		ratio = alternate
	}
	size, resolution := stringsByName["size"], strings.ToUpper(stringsByName["resolution"])
	gpt := strings.HasPrefix(modelName, "gpt-image-") && !strings.HasSuffix(modelName, "-r")
	if gpt {
		if resolution != "" && resolution != "AUTO" {
			mapped, err := MapOpenAIImageDimensions(resolution, ratio)
			if err != nil {
				return nil, err
			}
			if size != "" && size != "auto" && size != mapped {
				return nil, errors.New("TabTab GPT image size conflicts with resolution and aspect ratio")
			}
			size = mapped
		} else if ratio != "" && ratio != "auto" {
			width, height, dimensions := imageDimensions(size)
			left, right, separated := strings.Cut(ratio, ":")
			ratioWidth, widthErr := strconv.Atoi(left)
			ratioHeight, heightErr := strconv.Atoi(right)
			if !dimensions || !separated || widthErr != nil || heightErr != nil || ratioWidth < 1 || ratioWidth > 128 || ratioHeight < 1 || ratioHeight > 128 || width > 3840 || height > 3840 || width*ratioHeight != height*ratioWidth {
				return nil, errors.New("TabTab GPT image aspect ratio conflicts with pixel size")
			}
		}
		width, height := 0, 0
		_, hasWidth := fields["width"]
		_, hasHeight := fields["height"]
		if hasWidth != hasHeight {
			return nil, errors.New("TabTab width and height must be supplied together")
		}
		if hasWidth {
			if common.Unmarshal(fields["width"], &width) != nil || common.Unmarshal(fields["height"], &height) != nil {
				return nil, errors.New("TabTab width and height must be integers")
			}
			if size != "" && size != "auto" && size != fmt.Sprintf("%dx%d", width, height) {
				return nil, errors.New("TabTab size conflicts with width and height")
			}
		} else if size != "" && size != "auto" {
			w, h, ok := strings.Cut(size, "x")
			var widthErr, heightErr error
			width, widthErr = strconv.Atoi(w)
			height, heightErr = strconv.Atoi(h)
			if !ok || widthErr != nil || heightErr != nil {
				return nil, errors.New("TabTab GPT image size must be auto or pixel dimensions")
			}
		}
		if width != 0 || height != 0 || hasWidth {
			if width < 16 || width > 3840 || height < 16 || height > 3840 || width%16 != 0 || height%16 != 0 || width > height*3 || height > width*3 || width*height < 655360 || width*height > 8294400 {
				return nil, errors.New("TabTab GPT image dimensions exceed the model limits")
			}
			args["size"] = fmt.Sprintf("%dx%d", width, height)
		} else if size != "" {
			args["size"] = size
		}
		qualities := []string{"auto", "low", "medium", "high"}
		if modelName != "gpt-image-2" {
			qualities = append(qualities, "xhigh", "max")
		}
		if value := stringsByName["quality"]; value != "" {
			if !slices.Contains(qualities, value) {
				return nil, errors.New("unsupported TabTab image quality")
			}
			args["quality"] = value
		}
		for name, choices := range map[string][]string{"background": {"auto", "opaque", "transparent"}, "output_format": {"png", "jpeg", "webp"}} {
			if value := stringsByName[name]; value != "" {
				if !slices.Contains(choices, value) {
					return nil, fmt.Errorf("unsupported TabTab image %s", name)
				}
				args[name] = value
			}
		}
		if raw, exists := fields["mask"]; exists {
			if _, ok := args["images"]; !ok {
				return nil, errors.New("TabTab mask requires a reference image")
			}
			var reference any
			if common.Unmarshal(raw, &reference) != nil {
				return nil, errors.New("invalid TabTab image mask")
			}
			mask, err := tabtabImageReference(reference)
			if err != nil {
				return nil, err
			}
			args["mask"] = mask
		}
	} else {
		for _, name := range []string{"mask", "width", "height", "background", "output_format"} {
			if _, exists := fields[name]; exists {
				return nil, fmt.Errorf("TabTab %s does not support %s", modelName, name)
			}
		}
		if modelName == "mj" {
			if size != "" && size != "auto" || resolution != "" && resolution != "AUTO" || ratio != "" && ratio != "auto" || stringsByName["quality"] != "" {
				return nil, errors.New("TabTab MJ image tasks support only prompt and reference images")
			}
			return common.Marshal(map[string]any{"model": modelName, "args": args})
		}
		if strings.Contains(size, "x") {
			w, h, _ := strings.Cut(size, "x")
			width, werr := strconv.Atoi(w)
			height, herr := strconv.Atoi(h)
			if werr != nil || herr != nil || width < 1 || height < 1 || width > 8192 || height > 8192 {
				return nil, errors.New("invalid TabTab image dimensions")
			}
			divisor, remainder := width, height
			for remainder != 0 {
				divisor, remainder = remainder, divisor%remainder
			}
			derivedRatio := fmt.Sprintf("%d:%d", width/divisor, height/divisor)
			if ratio == "" {
				ratio = derivedRatio
			}
			if resolution == "" {
				resolution = "1K"
				if max(width, height) > 2048 {
					resolution = "4K"
				} else if max(width, height) > 1024 {
					resolution = "2K"
				}
			}
			size = ""
		}
		if ratio == "" {
			ratio = "1:1"
		}
		ratios := []string{"auto", "21:9", "16:9", "5:4", "4:3", "3:2", "1:1", "2:3", "3:4", "4:5", "9:16"}
		if modelName == "gemini-nano-banana-2.1" || modelName == "gemini-3.1-flash-image" {
			ratios = append(ratios, "8:1", "4:1", "1:4", "1:8")
		}
		if strings.HasSuffix(modelName, "-r") {
			ratios = []string{"1:1", "16:9", "9:16", "4:3", "3:4", "2:3", "3:2"}
		}
		if modelName == "seedream-5-pro" {
			ratios = []string{"21:9", "2:1", "16:9", "3:2", "4:3", "1:1", "3:4", "2:3", "9:16", "1:2", "9:21"}
		}
		if !slices.Contains(ratios, ratio) {
			return nil, errors.New("unsupported TabTab image aspect ratio")
		}
		args["ratio"] = ratio
		if size != "" && size != "auto" && resolution != "" && resolution != "AUTO" && strings.ToUpper(size) != resolution {
			return nil, errors.New("TabTab size and resolution must match")
		}
		if resolution == "" || resolution == "AUTO" {
			resolution = strings.ToUpper(size)
		}
		if modelName == "gemini-3.1-flash-lite-image" {
			if resolution != "" && resolution != "AUTO" {
				return nil, errors.New("TabTab flash lite image has a fixed size")
			}
		} else {
			if resolution == "" || resolution == "AUTO" {
				resolution = "2K"
				if strings.HasSuffix(modelName, "-r") || modelName == "seedream-5-pro" {
					resolution = "1K"
				}
			}
			if !slices.Contains([]string{"1K", "2K", "4K"}, resolution) {
				return nil, errors.New("TabTab image size must be 1K, 2K or 4K")
			}
			if modelName == "seedream-5-pro" && resolution == "4K" {
				return nil, errors.New("TabTab Seedream image size must be 1K or 2K")
			}
			args["size"] = resolution
		}
		if value := stringsByName["quality"]; value != "" {
			if !strings.HasSuffix(modelName, "-r") || !slices.Contains([]string{"auto", "low", "medium", "high", "xhigh", "max"}, value) {
				return nil, errors.New("unsupported TabTab image quality")
			}
			args["quality"] = value
		}
	}
	return common.Marshal(map[string]any{"model": modelName, "args": args})
}

func tabtabImageReference(reference any) (string, error) {
	if object, ok := reference.(map[string]any); ok {
		if len(object) != 1 {
			return "", errors.New("invalid TabTab image reference object")
		}
		reference = object["image_url"]
		if reference == nil {
			reference = object["url"]
		}
	}
	value, ok := reference.(string)
	if !ok || value == "" {
		return "", errors.New("TabTab image references must be image URLs or data URIs")
	}
	if strings.HasPrefix(value, "data:image/") {
		metadata, payload, valid := strings.Cut(value, ",")
		if !valid || !strings.HasSuffix(metadata, ";base64") || len(payload) > base64.StdEncoding.EncodedLen(32<<20) {
			return "", errors.New("invalid or oversized TabTab image data URI")
		}
		if decoded, err := base64.StdEncoding.Strict().DecodeString(payload); err != nil || len(decoded) == 0 || len(decoded) > 32<<20 {
			return "", errors.New("invalid or oversized TabTab image data URI")
		}
		return value, nil
	}
	if _, err := ValidateImageReferenceURL(value); err != nil {
		return "", err
	}
	return value, nil
}

func TabtabAsyncImageProfile(modelName string) *dto.UpstreamAsyncProfile {
	return &dto.UpstreamAsyncProfile{
		ID: "tabtab-image", MediaType: dto.UpstreamAsyncMediaImage, Models: []string{modelName},
		Operations: []string{dto.UpstreamAsyncOperationGenerate, dto.UpstreamAsyncOperationEdit},
		Submit:     dto.UpstreamAsyncSubmit{TaskIDPath: "task_id", Request: &dto.UpstreamAsyncSubmitRequest{Method: "POST", Path: "/api/task/create", Headers: map[string]string{"Authorization": "Bearer {api_key}"}}},
		Poll: dto.UpstreamAsyncPoll{
			Request: dto.UpstreamAsyncPollRequest{Method: "GET", Path: "/api/task/status", Query: map[string]string{"id": "{task_id}"}, Headers: map[string]string{"Authorization": "Bearer {api_key}"}},
			Response: dto.UpstreamAsyncPollResponse{StatusPath: "status", ResultPath: "urls", FailureReasonPath: "reason", StatusValues: dto.UpstreamAsyncStatusValues{
				InProgress: []dto.UpstreamAsyncStatusValue{dto.UpstreamAsyncStatusValue(`"processing"`)},
				Succeeded:  []dto.UpstreamAsyncStatusValue{dto.UpstreamAsyncStatusValue(`"succeeded"`)}, Failed: []dto.UpstreamAsyncStatusValue{dto.UpstreamAsyncStatusValue(`"failed"`)},
			}, DownloadHeaders: map[string]string{"Authorization": "Bearer {api_key}"}},
		},
	}
}

// ClassifyTabtabImageHTTPFailure authorizes rejection handling only for the
// documented complete HTTP error envelope, without any receipt or output data.
func ClassifyTabtabImageHTTPFailure(ctx context.Context, response *http.Response, statusMapping string) *AsyncImageFailure {
	if response == nil {
		return &AsyncImageFailure{Code: 608, InternalCode: "execution_unknown", Message: "TabTab task acceptance could not be determined", ExecutionUnknown: true}
	}
	defer CloseResponseBodyGracefully(response)
	body, readErr := ReadUpstreamAsyncResponse(response, asyncImageProviderErrorBodyLimit)
	copyResponse := *response
	copyResponse.Body = io.NopCloser(bytes.NewReader(body))
	failure := ClassifyOpenAIAsyncImageHTTPFailure(ctx, &copyResponse, statusMapping)
	var fields map[string]common.RawMessage
	valid := readErr == nil && common.Unmarshal(body, &fields) == nil && fields != nil
	var code string
	if common.Unmarshal(fields["code"], &code) != nil {
		valid = false
	}
	expectedStatus := map[string]int{"AUTH_INVALID_TOKEN": 401, "FORBIDDEN": 403, "VALIDATION_FAILED": 400, "POINTS_NOT_ENOUGH": 402, "RATE_LIMITED": 429, "MODEL_UNAVAILABLE": 503, "FILE_TYPE_NOT_ALLOWED": 400}
	valid = valid && expectedStatus[code] == response.StatusCode
	seen := make(map[string]bool)
	gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool {
		if seen[key.Str] {
			valid = false
		}
		seen[key.Str] = true
		return true
	})
	for name, raw := range fields {
		switch name {
		case "code", "message", "request_id":
			var text string
			if common.Unmarshal(raw, &text) != nil {
				valid = false
			}
		case "data":
			var object map[string]common.RawMessage
			if common.Unmarshal(raw, &object) != nil || len(object) != 0 {
				valid = false
			}
		default:
			valid = false
		}
	}
	if valid {
		failure.ProviderCode = code
		failure.SubmissionRejected = true
		failure.RetryableHTTPResponse = response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable
		return failure
	}
	failure.Code, failure.InternalCode, failure.ExecutionUnknown = 608, "execution_unknown", true
	failure.SubmissionRejected, failure.RetryableHTTPResponse, failure.SynchronousRetryableResponse = false, false, false
	return failure
}

// NormalizeTabtabImageTask does not infer success from transport status or an
// unknown provider state. Relative output references stay on the channel origin.
func NormalizeTabtabImageTask(body []byte, baseURL, taskID string) ([]byte, error) {
	var envelope struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
		Data      struct {
			ID                string  `json:"id"`
			UsageRecordID     string  `json:"usage_record_id"`
			RequestID         string  `json:"request_id"`
			ProviderRequestID string  `json:"provider_request_id"`
			Model             string  `json:"model"`
			ModelCode         string  `json:"model_code"`
			CreatedAt         string  `json:"created_at"`
			UpdatedAt         string  `json:"updated_at"`
			ChargedPoints     float64 `json:"charged_points"`
			GenerationTimeMS  float64 `json:"generation_time_ms"`
			Status            string  `json:"status"`
			ErrorCode         string  `json:"error_code"`
			ErrorMessage      string  `json:"error_message"`
			Results           []struct {
				URL      string `json:"url"`
				MIMEType string `json:"mime_type"`
			} `json:"results"`
		} `json:"data"`
	}
	if common.Unmarshal(body, &envelope) != nil {
		return nil, errors.New("invalid TabTab task response")
	}
	for _, object := range []gjson.Result{gjson.ParseBytes(body), gjson.GetBytes(body, "data")} {
		seen, unique := make(map[string]bool), true
		object.ForEach(func(key, _ gjson.Result) bool {
			if seen[key.Str] {
				unique = false
				return false
			}
			seen[key.Str] = true
			return true
		})
		if !unique {
			return nil, errors.New("TabTab task response contains duplicate fields")
		}
	}
	if envelope.Code != "OK" {
		// This normalizer is called only after successful HTTP transport. A
		// 2xx body without a usable receipt cannot prove non-acceptance, even
		// when its error projection omits a contradictory or duplicate field.
		// HTTP failures follow the existing complete-response classifier.
		return nil, errors.New("TabTab task response did not report OK")
	}
	if envelope.Data.ID == "" || len(envelope.Data.ID) > 191 || strings.ContainsAny(envelope.Data.ID, "\r\n\x00") || strings.TrimSpace(envelope.Data.ID) != envelope.Data.ID {
		return nil, errors.New("TabTab task ID is invalid")
	}
	if taskID != "" && envelope.Data.ID != taskID {
		return nil, errors.New("TabTab task response ID does not match the accepted task")
	}
	if envelope.Data.Status != "succeeded" && len(envelope.Data.Results) > 0 {
		return nil, errors.New("TabTab non-success task contains contradictory output")
	}
	if envelope.Data.Status == "failed" {
		if envelope.Data.UsageRecordID != "" && envelope.Data.UsageRecordID != envelope.Data.ID || envelope.Data.ModelCode != "" && envelope.Data.ModelCode != envelope.Data.Model {
			return nil, errors.New("TabTab failed task metadata conflicts with its identity or model")
		}
		if requestID := envelope.Data.ProviderRequestID; len(requestID) > asyncImageProviderRequestIDLimit || strings.TrimSpace(requestID) != requestID || strings.ContainsFunc(requestID, unicode.IsControl) {
			return nil, errors.New("TabTab provider request ID is invalid")
		}
		for _, value := range []string{envelope.Data.CreatedAt, envelope.Data.UpdatedAt} {
			if value != "" {
				if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
					return nil, errors.New("TabTab failed task timestamp is invalid")
				}
			}
		}
		var root map[string]common.RawMessage
		_ = common.Unmarshal(body, &root)
		for name := range root {
			if !slices.Contains([]string{"code", "data", "request_id", "message"}, name) {
				return nil, errors.New("TabTab failed task contains unrecognized execution evidence")
			}
		}
		var raw struct {
			Data map[string]common.RawMessage `json:"data"`
		}
		if common.Unmarshal(body, &raw) != nil {
			return nil, errors.New("invalid TabTab failed task response")
		}
		for name := range raw.Data {
			if !slices.Contains([]string{"id", "usage_record_id", "request_id", "provider_request_id", "model", "model_code", "created_at", "updated_at", "status", "charged_points", "results", "generation_time_ms", "error_code", "error_message"}, name) {
				return nil, errors.New("TabTab failed task contains unrecognized execution evidence")
			}
		}
	}
	urls := make([]string, 0, len(envelope.Data.Results))
	if envelope.Data.Status == "succeeded" {
		if len(envelope.Data.Results) != 1 {
			return nil, errors.New("TabTab image task must return one result")
		}
		base, err := url.Parse(baseURL)
		if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil {
			return nil, errors.New("TabTab channel base URL is invalid")
		}
		for _, item := range envelope.Data.Results {
			result, err := url.Parse(item.URL)
			if err != nil || result.User != nil || result.Fragment != "" || strings.ContainsAny(item.URL, "\\\r\n\x00") {
				return nil, errors.New("TabTab image result URL is invalid")
			}
			if strings.HasPrefix(item.URL, "/files/") && result.Host == "" {
				result = base.ResolveReference(result)
			}
			if result.Host == "" || (result.Scheme != "http" && result.Scheme != "https") || item.MIMEType != "" && !strings.HasPrefix(item.MIMEType, "image/") {
				return nil, errors.New("TabTab image result URL or MIME type is invalid")
			}
			urls = append(urls, result.String())
		}
	}
	reason := envelope.Data.ErrorMessage
	if reason == "" {
		reason = envelope.Data.ErrorCode
	}
	requestID := envelope.Data.ProviderRequestID
	if requestID == "" {
		requestID = envelope.Data.RequestID
	}
	if requestID == "" {
		requestID = envelope.RequestID
	}
	return common.Marshal(map[string]any{
		"task_id": envelope.Data.ID, "status": envelope.Data.Status, "reason": reason, "urls": urls,
		"error_code": SanitizeAsyncImageDiagnostic(envelope.Data.ErrorCode, asyncImageProviderCodeLimit),
		"request_id": SanitizeAsyncImageDiagnostic(requestID, asyncImageProviderRequestIDLimit),
	})
}
