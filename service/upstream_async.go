package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/tidwall/gjson"
)

const maxUpstreamAsyncResponseBytes = 4 << 20

var ErrUpstreamAsyncBillingUsage = errors.New("upstream async billing usage is invalid")

type UpstreamAsyncTemplateContext struct {
	TaskID        string
	Model         string
	UpstreamModel string
	APIKey        string
}

type UpstreamAsyncPollResult struct {
	Status   model.TaskStatus
	Progress string
	Reason   string
	URLs     []string
	Usage    *relaydto.Usage
}

// BuildUpstreamAsyncSubmitRequest replaces only the outbound HTTP request.
// Admission, billing, and the dispatch barrier remain owned by the caller.
func BuildUpstreamAsyncSubmitRequest(ctx context.Context, baseURL string, profile *relaydto.UpstreamAsyncProfile, values UpstreamAsyncTemplateContext, source any, defaultBody io.Reader, defaultContentType string) (*http.Request, error) {
	if profile == nil || profile.Submit.Request == nil {
		return nil, errors.New("upstream async submit request is not configured")
	}
	configured := profile.Submit.Request
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("channel base URL is invalid")
	}
	requestURL, err := url.Parse(strings.TrimRight(base.String(), "/") + expandUpstreamAsyncTemplate(configured.Path, values, true))
	if err != nil || !sameUpstreamAsyncOrigin(base, requestURL) {
		return nil, errors.New("upstream async submit URL is invalid")
	}
	query := requestURL.Query()
	for name, template := range configured.Query {
		query.Set(name, expandUpstreamAsyncTemplate(template, values, false))
	}
	requestURL.RawQuery = query.Encode()
	body := defaultBody
	contentType := defaultContentType
	if configured.Body != nil {
		if strings.HasPrefix(strings.ToLower(defaultContentType), "multipart/") {
			return nil, errors.New("upstream async submit JSON template cannot replace multipart input")
		}
		encodedSource, err := common.Marshal(source)
		if err != nil {
			return nil, errors.New("upstream async submit source is invalid")
		}
		expanded, present, err := expandUpstreamAsyncSubmitJSON(configured.Body, values, encodedSource, 0)
		if err != nil || !present {
			return nil, errors.New("upstream async submit body could not be rendered")
		}
		encoded, err := common.Marshal(expanded)
		if err != nil || len(encoded) > 64<<20 {
			return nil, errors.New("upstream async submit body exceeds the byte limit")
		}
		body = bytes.NewReader(encoded)
		contentType = "application/json"
	}
	request, err := http.NewRequestWithContext(ctx, configured.Method, requestURL.String(), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for name, template := range configured.Headers {
		value := expandUpstreamAsyncTemplate(template, values, false)
		if strings.ContainsAny(name+value, "\r\n") {
			return nil, errors.New("upstream async submit header is invalid")
		}
		request.Header.Set(name, value)
	}
	return request, nil
}

// A missing request field omits its containing object property. Array elements
// cannot be omitted, so a missing field there rejects the submit before dispatch.
func expandUpstreamAsyncSubmitJSON(value any, values UpstreamAsyncTemplateContext, source []byte, depth int) (any, bool, error) {
	if depth > 16 {
		return nil, false, errors.New("upstream async submit body exceeds the nesting limit")
	}
	switch typed := value.(type) {
	case nil, bool, float64, json.Number:
		return typed, true, nil
	case string:
		if path, ok := strings.CutPrefix(typed, "{request."); ok {
			if path, ok = strings.CutSuffix(path, "}"); ok {
				result := gjson.GetBytes(source, path)
				if !result.Exists() {
					return nil, false, nil
				}
				var decoded any
				if err := common.Unmarshal([]byte(result.Raw), &decoded); err != nil {
					return nil, false, errors.New("upstream async submit source field is invalid")
				}
				return decoded, true, nil
			}
		}
		return expandUpstreamAsyncTemplate(typed, values, false), true, nil
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			expanded, present, err := expandUpstreamAsyncSubmitJSON(child, values, source, depth+1)
			if err != nil || !present {
				return nil, false, errors.New("upstream async submit array field is missing or invalid")
			}
			result[index] = expanded
		}
		return result, true, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			expanded, present, err := expandUpstreamAsyncSubmitJSON(child, values, source, depth+1)
			if err != nil {
				return nil, false, err
			}
			if present {
				result[key] = expanded
			}
		}
		return result, true, nil
	default:
		return nil, false, fmt.Errorf("unsupported upstream async submit value %T", value)
	}
}

func ValidateUpstreamAsyncImageSubmitCount(request *http.Request, expected int) error {
	if request == nil || request.GetBody == nil {
		return errors.New("upstream async submit body is unavailable")
	}
	body, err := request.GetBody()
	if err != nil {
		return err
	}
	defer body.Close()
	encoded, err := io.ReadAll(io.LimitReader(body, (64<<20)+1))
	if err != nil || len(encoded) > 64<<20 {
		return errors.New("upstream async submit body exceeds the byte limit")
	}
	for _, path := range []string{"n", "create_count", "batch_size", "num_outputs", "input.num_outputs", "parameters.n", "sequential_image_generation_options.max_images"} {
		value := gjson.GetBytes(encoded, path)
		if !value.Exists() {
			continue
		}
		count, err := strconv.ParseUint(value.Raw, 10, 64)
		if err != nil || count != uint64(expected) {
			return fmt.Errorf("upstream async submit %s conflicts with the validated image count", path)
		}
	}
	return nil
}

func ValidateUpstreamAsyncVideoSubmitDuration(request *http.Request, source any, maxSeconds int) error {
	if request == nil || request.GetBody == nil {
		return errors.New("upstream async submit body is unavailable")
	}
	sourceJSON, err := common.Marshal(source)
	if err != nil {
		return errors.New("upstream async submit source is invalid")
	}
	var original gjson.Result
	for _, path := range []string{"seconds", "duration", "metadata.seconds", "metadata.duration"} {
		if candidate := gjson.GetBytes(sourceJSON, path); candidate.Exists() {
			original = candidate
			break
		}
	}
	body, err := request.GetBody()
	if err != nil {
		return err
	}
	defer body.Close()
	encoded, err := io.ReadAll(io.LimitReader(body, (64<<20)+1))
	if err != nil || len(encoded) > 64<<20 {
		return errors.New("upstream async submit body exceeds the byte limit")
	}
	for _, path := range []string{"seconds", "duration", "metadata.seconds", "metadata.duration", "input.seconds", "input.duration", "parameters.seconds", "parameters.duration"} {
		value := gjson.GetBytes(encoded, path)
		if !value.Exists() {
			continue
		}
		if value.Type != gjson.Number || value.Num <= 0 || value.Num > float64(maxSeconds) || original.Type != gjson.Number || value.Num != original.Num {
			return fmt.Errorf("upstream async submit %s conflicts with the validated video duration", path)
		}
	}
	return nil
}

func ParseUpstreamAsyncTaskID(profile *relaydto.UpstreamAsyncProfile, body []byte) (string, error) {
	if profile == nil {
		return "", errors.New("upstream async profile is required")
	}
	result := gjson.GetBytes(body, strings.TrimSpace(profile.Submit.TaskIDPath))
	if !result.Exists() {
		return "", errors.New("upstream async task ID is missing")
	}
	var taskID string
	switch result.Type {
	case gjson.String:
		taskID = strings.TrimSpace(result.Str)
	case gjson.Number:
		taskID = strings.TrimSpace(result.Raw)
	}
	if taskID == "" || len(taskID) > 191 || strings.ContainsAny(taskID, "\r\n\x00") {
		return "", errors.New("upstream async task ID is invalid")
	}
	return taskID, nil
}

func BuildUpstreamAsyncPollRequest(ctx context.Context, baseURL string, profile *relaydto.UpstreamAsyncProfile, values UpstreamAsyncTemplateContext) (*http.Request, error) {
	if profile == nil {
		return nil, errors.New("upstream async profile is required")
	}
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("channel base URL is invalid")
	}
	path := expandUpstreamAsyncTemplate(profile.Poll.Request.Path, values, true)
	requestURL, err := url.Parse(strings.TrimRight(base.String(), "/") + path)
	if err != nil || !sameUpstreamAsyncOrigin(base, requestURL) {
		return nil, errors.New("upstream async poll URL is invalid")
	}
	query := requestURL.Query()
	for name, template := range profile.Poll.Request.Query {
		query.Set(name, expandUpstreamAsyncTemplate(template, values, false))
	}
	requestURL.RawQuery = query.Encode()

	var body io.Reader
	if profile.Poll.Request.Body != nil {
		expanded, err := expandUpstreamAsyncJSON(profile.Poll.Request.Body, values, 0)
		if err != nil {
			return nil, err
		}
		encoded, err := common.Marshal(expanded)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	method := strings.ToUpper(strings.TrimSpace(profile.Poll.Request.Method))
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return nil, err
	}
	for name, template := range profile.Poll.Request.Headers {
		expanded := expandUpstreamAsyncTemplate(template, values, false)
		if strings.ContainsAny(name+expanded, "\r\n") {
			return nil, errors.New("upstream async request header is invalid")
		}
		request.Header.Set(name, expanded)
	}
	if body != nil && request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func PollUpstreamAsync(ctx context.Context, baseURL, proxy string, profile *relaydto.UpstreamAsyncProfile, values UpstreamAsyncTemplateContext) (*http.Response, error) {
	request, err := BuildUpstreamAsyncPollRequest(ctx, baseURL, profile, values)
	if err != nil {
		return nil, err
	}
	client, err := GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, err
	}
	return client.Do(request)
}

func ReadUpstreamAsyncResponse(response *http.Response, limit int64) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, errors.New("upstream async response is missing")
	}
	if limit <= 0 || limit > maxUpstreamAsyncResponseBytes {
		limit = maxUpstreamAsyncResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("upstream async response exceeds the byte limit")
	}
	if !gjson.ValidBytes(body) {
		return nil, errors.New("upstream async response must be JSON")
	}
	return body, nil
}

func ParseUpstreamAsyncPollResponse(profile *relaydto.UpstreamAsyncProfile, body []byte) (*UpstreamAsyncPollResult, error) {
	if profile == nil {
		return nil, errors.New("upstream async profile is required")
	}
	if !gjson.ValidBytes(body) {
		return nil, errors.New("upstream async response must be JSON")
	}
	statusValue := gjson.GetBytes(body, profile.Poll.Response.StatusPath)
	result := &UpstreamAsyncPollResult{Status: model.TaskStatusUnknown}
	for status, values := range map[model.TaskStatus][]relaydto.UpstreamAsyncStatusValue{
		model.TaskStatusQueued:     profile.Poll.Response.StatusValues.Queued,
		model.TaskStatusInProgress: profile.Poll.Response.StatusValues.InProgress,
		model.TaskStatusSuccess:    profile.Poll.Response.StatusValues.Succeeded,
		model.TaskStatusFailure:    profile.Poll.Response.StatusValues.Failed,
	} {
		if slicesContainsUpstreamAsyncStatus(values, statusValue) {
			result.Status = status
			break
		}
	}
	if path := strings.TrimSpace(profile.Poll.Response.ProgressPath); path != "" {
		progress := gjson.GetBytes(body, path)
		switch progress.Type {
		case gjson.String:
			result.Progress = limitUpstreamAsyncText(progress.Str, 20)
		case gjson.Number:
			if progress.Num >= 0 && progress.Num <= 100 && !math.IsNaN(progress.Num) && !math.IsInf(progress.Num, 0) {
				result.Progress = strconv.FormatFloat(progress.Num, 'f', -1, 64) + "%"
			}
		}
	}
	if path := strings.TrimSpace(profile.Poll.Response.FailureReasonPath); path != "" {
		reason := gjson.GetBytes(body, path)
		if reason.Type == gjson.String {
			result.Reason = sanitizeAsyncImageProviderDiagnostic(reason.Str, common.LocalLogContentLimit)
		}
	}
	if result.Status == model.TaskStatusSuccess {
		urls, err := parseUpstreamAsyncResultURLs(gjson.GetBytes(body, profile.Poll.Response.ResultPath), profile.MediaType)
		if err != nil {
			return nil, err
		}
		result.URLs = urls
	}
	usage, err := parseUpstreamAsyncUsage(body, profile.Poll.Response.UsagePaths)
	if err != nil {
		return nil, err
	}
	result.Usage = usage
	return result, nil
}

func BuildUpstreamAsyncDownloadHeaders(profile *relaydto.UpstreamAsyncProfile, rawURL, baseURL string, values UpstreamAsyncTemplateContext) (map[string]string, bool, error) {
	if profile == nil || len(profile.Poll.Response.DownloadHeaders) == 0 {
		return nil, true, nil
	}
	resultURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || resultURL.Host == "" || (resultURL.Scheme != "http" && resultURL.Scheme != "https") || resultURL.User != nil || resultURL.Fragment != "" {
		return nil, false, errors.New("upstream async result URL is invalid")
	}
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || !sameUpstreamAsyncOrigin(base, resultURL) {
		return nil, false, errors.New("credentialed upstream async result URL must use the channel origin")
	}
	headers := make(map[string]string, len(profile.Poll.Response.DownloadHeaders))
	for name, template := range profile.Poll.Response.DownloadHeaders {
		expanded := expandUpstreamAsyncTemplate(template, values, false)
		if strings.ContainsAny(name+expanded, "\r\n") {
			return nil, false, errors.New("upstream async download header is invalid")
		}
		headers[name] = expanded
	}
	return headers, false, nil
}

func PublicUpstreamAsyncVideoResultURL(task *model.Task) string {
	if task == nil || task.Status != model.TaskStatusSuccess || task.PrivateData.UpstreamAsync == nil ||
		len(task.PrivateData.UpstreamAsync.Poll.Response.DownloadHeaders) != 0 {
		return ""
	}
	resultURL := strings.TrimSpace(task.PrivateData.ResultURL)
	if !PublicUpstreamAsyncResultURL(resultURL) {
		return ""
	}
	return resultURL
}

func UpstreamAsyncOperation(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "edit", "edit_video":
		return relaydto.UpstreamAsyncOperationEdit
	case "extend", "extend_video":
		return relaydto.UpstreamAsyncOperationExtend
	default:
		return relaydto.UpstreamAsyncOperationGenerate
	}
}

func expandUpstreamAsyncTemplate(template string, values UpstreamAsyncTemplateContext, escapePath bool) string {
	replacements := map[string]string{
		"{task_id}":        values.TaskID,
		"{model}":          values.Model,
		"{upstream_model}": values.UpstreamModel,
		"{api_key}":        values.APIKey,
	}
	for placeholder, value := range replacements {
		if escapePath {
			value = url.PathEscape(value)
		}
		template = strings.ReplaceAll(template, placeholder, value)
	}
	return template
}

func expandUpstreamAsyncJSON(value any, values UpstreamAsyncTemplateContext, depth int) (any, error) {
	if depth > 16 {
		return nil, errors.New("upstream async request body exceeds the nesting limit")
	}
	switch typed := value.(type) {
	case nil, bool, float64, common.RawMessage:
		return typed, nil
	case string:
		return expandUpstreamAsyncTemplate(typed, values, false), nil
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			expanded, err := expandUpstreamAsyncJSON(child, values, depth+1)
			if err != nil {
				return nil, err
			}
			result[index] = expanded
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			expanded, err := expandUpstreamAsyncJSON(child, values, depth+1)
			if err != nil {
				return nil, err
			}
			result[key] = expanded
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported upstream async request body value %T", value)
	}
}

func sameUpstreamAsyncOrigin(left, right *url.URL) bool {
	if left == nil || right == nil || !strings.EqualFold(left.Scheme, right.Scheme) || !strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	port := func(value *url.URL) string {
		if value.Port() != "" {
			return value.Port()
		}
		if strings.EqualFold(value.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return port(left) == port(right)
}

func slicesContainsUpstreamAsyncStatus(values []relaydto.UpstreamAsyncStatusValue, result gjson.Result) bool {
	if !result.Exists() {
		return false
	}
	for _, value := range values {
		if value.Matches(result) {
			return true
		}
	}
	return false
}

func parseUpstreamAsyncResultURLs(result gjson.Result, mediaType string) ([]string, error) {
	urls := make([]string, 0, 1)
	if result.Type == gjson.String {
		urls = append(urls, result.Str)
	} else if result.IsArray() {
		for _, item := range result.Array() {
			if item.Type != gjson.String {
				return nil, errors.New("upstream async result array must contain only URLs")
			}
			urls = append(urls, item.Str)
		}
	}
	maximum := 1
	if strings.EqualFold(mediaType, relaydto.UpstreamAsyncMediaImage) {
		maximum = relaydto.MaxImageN
	}
	if len(urls) == 0 || len(urls) > maximum {
		return nil, fmt.Errorf("upstream async result must contain between 1 and %d URLs", maximum)
	}
	for index := range urls {
		urls[index] = strings.TrimSpace(urls[index])
		parsed, err := url.Parse(urls[index])
		if err != nil || len(urls[index]) > 64<<10 || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
			return nil, errors.New("upstream async result contains an invalid HTTP(S) URL")
		}
	}
	return urls, nil
}

func parseUpstreamAsyncUsage(body []byte, paths map[string]string) (*relaydto.Usage, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	usage := &relaydto.Usage{UsageSource: "upstream_async"}
	found := false
	for target, path := range paths {
		value := gjson.GetBytes(body, path)
		if !value.Exists() {
			continue
		}
		if value.Type != gjson.Number {
			return nil, fmt.Errorf("%w: %s must be a number", ErrUpstreamAsyncBillingUsage, target)
		}
		raw := value.Raw
		if raw == "" {
			raw = strconv.FormatFloat(value.Num, 'g', -1, 64)
		}
		number, ok := new(big.Rat).SetString(raw)
		if !ok || number.Sign() < 0 || !number.IsInt() || !number.Num().IsInt64() {
			return nil, fmt.Errorf("%w: %s must be a non-negative integer", ErrUpstreamAsyncBillingUsage, target)
		}
		integer := number.Num().Int64()
		if integer > math.MaxInt32 {
			return nil, fmt.Errorf("%w: %s exceeds the billing limit", ErrUpstreamAsyncBillingUsage, target)
		}
		setUpstreamAsyncUsage(usage, target, int(integer))
		found = true
	}
	if !found {
		return nil, nil
	}
	if total := int64(usage.PromptTokens) + int64(usage.CompletionTokens); total > math.MaxInt32 {
		return nil, fmt.Errorf("%w: prompt and completion tokens exceed the billing limit", ErrUpstreamAsyncBillingUsage)
	} else if int(total) > usage.TotalTokens {
		usage.TotalTokens = int(total)
	}
	if total := int64(usage.InputTokens) + int64(usage.OutputTokens); total > math.MaxInt32 {
		return nil, fmt.Errorf("%w: input and output tokens exceed the billing limit", ErrUpstreamAsyncBillingUsage)
	} else if int(total) > usage.TotalTokens {
		usage.TotalTokens = int(total)
	}
	return usage, nil
}

func setUpstreamAsyncUsage(usage *relaydto.Usage, target string, value int) {
	switch target {
	case "prompt_tokens":
		usage.PromptTokens = value
	case "completion_tokens":
		usage.CompletionTokens = value
	case "total_tokens":
		usage.TotalTokens = value
	case "prompt_cache_hit_tokens":
		usage.PromptCacheHitTokens = value
	case "input_tokens":
		usage.InputTokens = value
	case "output_tokens":
		usage.OutputTokens = value
	case "claude_cache_creation_5_m_tokens":
		usage.ClaudeCacheCreation5mTokens = value
	case "claude_cache_creation_1_h_tokens":
		usage.ClaudeCacheCreation1hTokens = value
	case "prompt_tokens_details.cached_tokens":
		usage.PromptTokensDetails.CachedTokens = value
	case "prompt_tokens_details.cached_tokens_details.text_tokens":
		ensureUpstreamAsyncCachedDetails(&usage.PromptTokensDetails).TextTokens = common.GetPointer(value)
	case "prompt_tokens_details.cached_tokens_details.audio_tokens":
		ensureUpstreamAsyncCachedDetails(&usage.PromptTokensDetails).AudioTokens = common.GetPointer(value)
	case "prompt_tokens_details.cached_tokens_details.image_tokens":
		ensureUpstreamAsyncCachedDetails(&usage.PromptTokensDetails).ImageTokens = common.GetPointer(value)
	case "prompt_tokens_details.cached_creation_tokens":
		usage.PromptTokensDetails.CachedCreationTokens = value
	case "prompt_tokens_details.cache_write_tokens":
		usage.PromptTokensDetails.CacheWriteTokens = value
	case "prompt_tokens_details.text_tokens":
		usage.PromptTokensDetails.TextTokens = value
	case "prompt_tokens_details.audio_tokens":
		usage.PromptTokensDetails.AudioTokens = value
	case "prompt_tokens_details.image_tokens":
		usage.PromptTokensDetails.ImageTokens = value
	case "completion_tokens_details.reasoning_tokens":
		usage.CompletionTokenDetails.ReasoningTokens = value
	case "completion_tokens_details.text_tokens":
		usage.CompletionTokenDetails.TextTokens = value
	case "completion_tokens_details.audio_tokens":
		usage.CompletionTokenDetails.AudioTokens = value
	case "completion_tokens_details.image_tokens":
		usage.CompletionTokenDetails.ImageTokens = value
	case "input_tokens_details.cached_tokens":
		ensureUpstreamAsyncInputDetails(usage).CachedTokens = value
	case "input_tokens_details.cached_tokens_details.text_tokens":
		ensureUpstreamAsyncCachedDetails(ensureUpstreamAsyncInputDetails(usage)).TextTokens = common.GetPointer(value)
	case "input_tokens_details.cached_tokens_details.audio_tokens":
		ensureUpstreamAsyncCachedDetails(ensureUpstreamAsyncInputDetails(usage)).AudioTokens = common.GetPointer(value)
	case "input_tokens_details.cached_tokens_details.image_tokens":
		ensureUpstreamAsyncCachedDetails(ensureUpstreamAsyncInputDetails(usage)).ImageTokens = common.GetPointer(value)
	case "input_tokens_details.cached_creation_tokens":
		ensureUpstreamAsyncInputDetails(usage).CachedCreationTokens = value
	case "input_tokens_details.cache_write_tokens":
		ensureUpstreamAsyncInputDetails(usage).CacheWriteTokens = value
	case "input_tokens_details.text_tokens":
		ensureUpstreamAsyncInputDetails(usage).TextTokens = value
	case "input_tokens_details.audio_tokens":
		ensureUpstreamAsyncInputDetails(usage).AudioTokens = value
	case "input_tokens_details.image_tokens":
		ensureUpstreamAsyncInputDetails(usage).ImageTokens = value
	case "output_tokens_details.reasoning_tokens":
		ensureUpstreamAsyncOutputDetails(usage).ReasoningTokens = value
	case "output_tokens_details.text_tokens":
		ensureUpstreamAsyncOutputDetails(usage).TextTokens = value
	case "output_tokens_details.audio_tokens":
		ensureUpstreamAsyncOutputDetails(usage).AudioTokens = value
	case "output_tokens_details.image_tokens":
		ensureUpstreamAsyncOutputDetails(usage).ImageTokens = value
	}
}

func ensureUpstreamAsyncInputDetails(usage *relaydto.Usage) *relaydto.InputTokenDetails {
	if usage.InputTokensDetails == nil {
		usage.InputTokensDetails = &relaydto.InputTokenDetails{}
	}
	return usage.InputTokensDetails
}

func ensureUpstreamAsyncOutputDetails(usage *relaydto.Usage) *relaydto.OutputTokenDetails {
	if usage.OutputTokensDetails == nil {
		usage.OutputTokensDetails = &relaydto.OutputTokenDetails{}
	}
	return usage.OutputTokensDetails
}

func ensureUpstreamAsyncCachedDetails(details *relaydto.InputTokenDetails) *relaydto.CachedTokenDetails {
	if details.CachedTokensDetails == nil {
		details.CachedTokensDetails = &relaydto.CachedTokenDetails{}
	}
	return details.CachedTokensDetails
}

func limitUpstreamAsyncText(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if maximum <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) > maximum {
		return string(runes[:maximum])
	}
	return value
}
