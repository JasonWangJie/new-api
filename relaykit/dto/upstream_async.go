package dto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strings"

	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/tidwall/gjson"
)

const MaxUpstreamAsyncProfiles = 32

const (
	UpstreamAsyncMediaImage = "image"
	UpstreamAsyncMediaVideo = "video"

	UpstreamAsyncOperationGenerate = "generate"
	UpstreamAsyncOperationEdit     = "edit"
	UpstreamAsyncOperationExtend   = "extend"
)

var upstreamAsyncPathPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.(?:[A-Za-z0-9_-]+|#|[0-9]+))*$`)
var upstreamAsyncHeaderNamePattern = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

var upstreamAsyncUsageTargets = map[string]struct{}{
	"prompt_tokens":                       {},
	"completion_tokens":                   {},
	"total_tokens":                        {},
	"prompt_cache_hit_tokens":             {},
	"input_tokens":                        {},
	"output_tokens":                       {},
	"claude_cache_creation_5_m_tokens":    {},
	"claude_cache_creation_1_h_tokens":    {},
	"prompt_tokens_details.cached_tokens": {},
	"prompt_tokens_details.cached_tokens_details.text_tokens":  {},
	"prompt_tokens_details.cached_tokens_details.audio_tokens": {},
	"prompt_tokens_details.cached_tokens_details.image_tokens": {},
	"prompt_tokens_details.cached_creation_tokens":             {},
	"prompt_tokens_details.cache_write_tokens":                 {},
	"prompt_tokens_details.text_tokens":                        {},
	"prompt_tokens_details.audio_tokens":                       {},
	"prompt_tokens_details.image_tokens":                       {},
	"completion_tokens_details.reasoning_tokens":               {},
	"completion_tokens_details.text_tokens":                    {},
	"completion_tokens_details.audio_tokens":                   {},
	"completion_tokens_details.image_tokens":                   {},
	"input_tokens_details.cached_tokens":                       {},
	"input_tokens_details.cached_tokens_details.text_tokens":   {},
	"input_tokens_details.cached_tokens_details.audio_tokens":  {},
	"input_tokens_details.cached_tokens_details.image_tokens":  {},
	"input_tokens_details.cached_creation_tokens":              {},
	"input_tokens_details.cache_write_tokens":                  {},
	"input_tokens_details.text_tokens":                         {},
	"input_tokens_details.audio_tokens":                        {},
	"input_tokens_details.image_tokens":                        {},
	"output_tokens_details.reasoning_tokens":                   {},
	"output_tokens_details.text_tokens":                        {},
	"output_tokens_details.audio_tokens":                       {},
	"output_tokens_details.image_tokens":                       {},
}

type UpstreamAsyncConfig struct {
	Profiles []UpstreamAsyncProfile `json:"profiles"`
}

type UpstreamAsyncProfile struct {
	ID         string              `json:"id"`
	MediaType  string              `json:"media_type"`
	Models     []string            `json:"models"`
	Operations []string            `json:"operations"`
	Submit     UpstreamAsyncSubmit `json:"submit"`
	Poll       UpstreamAsyncPoll   `json:"poll"`
}

type UpstreamAsyncSubmit struct {
	TaskIDPath string                      `json:"task_id_path"`
	Request    *UpstreamAsyncSubmitRequest `json:"request,omitempty"`
}

type UpstreamAsyncSubmitRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    any               `json:"body,omitempty"`
}

type UpstreamAsyncPoll struct {
	Request  UpstreamAsyncPollRequest  `json:"request"`
	Response UpstreamAsyncPollResponse `json:"response"`
}

type UpstreamAsyncPollRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    any               `json:"body,omitempty"`
}

type UpstreamAsyncPollResponse struct {
	StatusPath        string                    `json:"status_path"`
	StatusValues      UpstreamAsyncStatusValues `json:"status_values"`
	ProgressPath      string                    `json:"progress_path,omitempty"`
	FailureReasonPath string                    `json:"failure_reason_path,omitempty"`
	ResultPath        string                    `json:"result_path"`
	UsagePaths        map[string]string         `json:"usage_paths,omitempty"`
	DownloadHeaders   map[string]string         `json:"download_headers,omitempty"`
}

type UpstreamAsyncStatusValues struct {
	Queued     []UpstreamAsyncStatusValue `json:"queued,omitempty"`
	InProgress []UpstreamAsyncStatusValue `json:"in_progress,omitempty"`
	Succeeded  []UpstreamAsyncStatusValue `json:"succeeded"`
	Failed     []UpstreamAsyncStatusValue `json:"failed"`
}

// UpstreamAsyncStatusValue preserves the configured JSON scalar so strings,
// numbers and booleans remain distinct when a poll response is classified.
type UpstreamAsyncStatusValue json.RawMessage

func (value *UpstreamAsyncStatusValue) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if !kitutil.Valid(trimmed) || len(trimmed) == 0 {
		return fmt.Errorf("status value must be valid JSON")
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := kitutil.Unmarshal(trimmed, &text); err != nil || strings.TrimSpace(text) == "" {
			return fmt.Errorf("status string must not be empty")
		}
	case 't', 'f':
		var flag bool
		if err := kitutil.Unmarshal(trimmed, &flag); err != nil {
			return fmt.Errorf("status value must be a string, number, or boolean")
		}
	default:
		if _, ok := new(big.Rat).SetString(string(trimmed)); !ok {
			return fmt.Errorf("status value must be a string, number, or boolean")
		}
	}
	*value = append((*value)[:0], trimmed...)
	return nil
}

func (value UpstreamAsyncStatusValue) MarshalJSON() ([]byte, error) {
	if len(value) == 0 {
		return nil, fmt.Errorf("status value is empty")
	}
	return append([]byte(nil), value...), nil
}

func (value UpstreamAsyncStatusValue) key() (string, error) {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		return "", fmt.Errorf("status value is empty")
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := kitutil.Unmarshal(trimmed, &text); err != nil {
			return "", err
		}
		return "string:" + text, nil
	case 't', 'f':
		return "boolean:" + string(trimmed), nil
	default:
		number, ok := new(big.Rat).SetString(string(trimmed))
		if !ok {
			return "", fmt.Errorf("invalid numeric status value")
		}
		return "number:" + number.RatString(), nil
	}
}

func (value UpstreamAsyncStatusValue) Matches(result gjson.Result) bool {
	configured, err := value.key()
	if err != nil {
		return false
	}
	var actual string
	switch result.Type {
	case gjson.String:
		actual = "string:" + result.Str
	case gjson.True:
		actual = "boolean:true"
	case gjson.False:
		actual = "boolean:false"
	case gjson.Number:
		raw := result.Raw
		if raw == "" {
			raw = fmt.Sprint(result.Num)
		}
		number, ok := new(big.Rat).SetString(raw)
		if !ok {
			return false
		}
		actual = "number:" + number.RatString()
	default:
		return false
	}
	return configured == actual
}

func (config *UpstreamAsyncConfig) Validate() error {
	if config == nil {
		return nil
	}
	if len(config.Profiles) == 0 {
		return fmt.Errorf("upstream_async.profiles must not be empty")
	}
	if len(config.Profiles) > MaxUpstreamAsyncProfiles {
		return fmt.Errorf("upstream_async.profiles must contain at most %d entries", MaxUpstreamAsyncProfiles)
	}
	ids := make(map[string]struct{}, len(config.Profiles))
	for index := range config.Profiles {
		if err := config.Profiles[index].validate(index); err != nil {
			return err
		}
		id := strings.TrimSpace(config.Profiles[index].ID)
		if _, exists := ids[id]; exists {
			return fmt.Errorf("upstream_async.profiles[%d].id duplicates %q", index, id)
		}
		ids[id] = struct{}{}
		for previous := range index {
			if upstreamAsyncProfilesOverlap(config.Profiles[previous], config.Profiles[index]) {
				return fmt.Errorf("upstream_async.profiles[%d] overlaps profiles[%d]", index, previous)
			}
		}
	}
	return nil
}

func (profile UpstreamAsyncProfile) validate(index int) error {
	prefix := fmt.Sprintf("upstream_async.profiles[%d]", index)
	if id := strings.TrimSpace(profile.ID); id == "" || len(id) > 64 || id != profile.ID {
		return fmt.Errorf("%s.id must contain 1 to 64 characters", prefix)
	}
	mediaType := strings.ToLower(strings.TrimSpace(profile.MediaType))
	if profile.MediaType != mediaType || mediaType != UpstreamAsyncMediaImage && mediaType != UpstreamAsyncMediaVideo {
		return fmt.Errorf("%s.media_type must be image or video", prefix)
	}
	if len(profile.Models) > 128 {
		return fmt.Errorf("%s.models contains too many entries", prefix)
	}
	seenModels := make(map[string]struct{}, len(profile.Models))
	for _, model := range profile.Models {
		trimmedModel := strings.TrimSpace(model)
		if model != trimmedModel || trimmedModel == "" || len(trimmedModel) > 191 {
			return fmt.Errorf("%s.models contains an invalid model", prefix)
		}
		if _, exists := seenModels[trimmedModel]; exists {
			return fmt.Errorf("%s.models contains duplicate model %q", prefix, trimmedModel)
		}
		seenModels[trimmedModel] = struct{}{}
	}
	if len(profile.Operations) == 0 || len(profile.Operations) > 3 {
		return fmt.Errorf("%s.operations must contain between 1 and 3 entries", prefix)
	}
	seenOperations := make(map[string]struct{}, len(profile.Operations))
	for _, operation := range profile.Operations {
		normalizedOperation := strings.ToLower(strings.TrimSpace(operation))
		if operation != normalizedOperation || normalizedOperation != UpstreamAsyncOperationGenerate && normalizedOperation != UpstreamAsyncOperationEdit && normalizedOperation != UpstreamAsyncOperationExtend {
			return fmt.Errorf("%s.operations contains invalid operation %q", prefix, operation)
		}
		if mediaType == UpstreamAsyncMediaImage && normalizedOperation == UpstreamAsyncOperationExtend {
			return fmt.Errorf("%s.operations does not allow extend for images", prefix)
		}
		if _, exists := seenOperations[normalizedOperation]; exists {
			return fmt.Errorf("%s.operations contains duplicate operation %q", prefix, normalizedOperation)
		}
		seenOperations[normalizedOperation] = struct{}{}
	}
	if err := validateUpstreamAsyncPath(profile.Submit.TaskIDPath, prefix+".submit.task_id_path", true); err != nil {
		return err
	}
	if request := profile.Submit.Request; request != nil {
		if request.Method != http.MethodPost {
			return fmt.Errorf("%s.submit.request.method must be POST", prefix)
		}
		if err := validateUpstreamAsyncRequestURLPath(request.Path, prefix+".submit.request.path"); err != nil {
			return err
		}
		if strings.Contains(request.Path, "{task_id}") {
			return fmt.Errorf("%s.submit.request.path cannot use task_id before submission", prefix)
		}
		if len(request.Query) > 64 {
			return fmt.Errorf("%s.submit.request.query contains too many entries", prefix)
		}
		for name, value := range request.Query {
			if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) || strings.ContainsAny(name, "&=\r\n") {
				return fmt.Errorf("%s.submit.request.query contains an invalid name", prefix)
			}
			if strings.Contains(value, "{task_id}") {
				return fmt.Errorf("%s.submit.request.query cannot use task_id before submission", prefix)
			}
			if err := validateUpstreamAsyncTemplates(value, prefix+".submit.request.query."+name); err != nil {
				return err
			}
		}
		if len(request.Headers) > 64 {
			return fmt.Errorf("%s.submit.request.headers contains too many entries", prefix)
		}
		if err := validateUpstreamAsyncHeaders(request.Headers, prefix+".submit.request.headers"); err != nil {
			return err
		}
		for _, value := range request.Headers {
			if strings.Contains(value, "{task_id}") {
				return fmt.Errorf("%s.submit.request.headers cannot use task_id before submission", prefix)
			}
		}
		if request.Body != nil {
			if _, ok := request.Body.(map[string]any); !ok {
				return fmt.Errorf("%s.submit.request.body must be a JSON object", prefix)
			}
			if err := validateUpstreamAsyncSubmitBody(request.Body, prefix+".submit.request.body", 0); err != nil {
				return err
			}
			if encoded, err := kitutil.Marshal(request.Body); err != nil || len(encoded) > 64<<10 {
				return fmt.Errorf("%s.submit.request.body must be no larger than 64 KiB", prefix)
			}
		}
	}
	method := strings.ToUpper(strings.TrimSpace(profile.Poll.Request.Method))
	if profile.Poll.Request.Method != method || method != http.MethodGet && method != http.MethodPost {
		return fmt.Errorf("%s.poll.request.method must be GET or POST", prefix)
	}
	if err := validateUpstreamAsyncRequestURLPath(profile.Poll.Request.Path, prefix+".poll.request.path"); err != nil {
		return err
	}
	if method == http.MethodGet && profile.Poll.Request.Body != nil {
		return fmt.Errorf("%s.poll.request.body is not allowed for GET", prefix)
	}
	if err := validateUpstreamAsyncTemplates(profile.Poll.Request.Path, prefix+".poll.request.path"); err != nil {
		return err
	}
	if len(profile.Poll.Request.Query) > 64 || len(profile.Poll.Request.Headers) > 64 || len(profile.Poll.Response.DownloadHeaders) > 64 {
		return fmt.Errorf("%s poll request contains too many query or header templates", prefix)
	}
	for name, value := range profile.Poll.Request.Query {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) || strings.ContainsAny(name, "&=\r\n") {
			return fmt.Errorf("%s.poll.request.query contains an invalid name", prefix)
		}
		if err := validateUpstreamAsyncTemplates(value, prefix+".poll.request.query."+name); err != nil {
			return err
		}
	}
	if err := validateUpstreamAsyncHeaders(profile.Poll.Request.Headers, prefix+".poll.request.headers"); err != nil {
		return err
	}
	if err := validateUpstreamAsyncHeaders(profile.Poll.Response.DownloadHeaders, prefix+".poll.response.download_headers"); err != nil {
		return err
	}
	if err := validateUpstreamAsyncTemplateValue(profile.Poll.Request.Body, prefix+".poll.request.body", 0); err != nil {
		return err
	}
	if encoded, err := kitutil.Marshal(profile.Poll.Request.Body); err != nil || len(encoded) > 64<<10 {
		return fmt.Errorf("%s.poll.request.body must be valid JSON no larger than 64 KiB", prefix)
	}
	response := profile.Poll.Response
	for pathName, path := range map[string]string{
		"status_path":         response.StatusPath,
		"progress_path":       response.ProgressPath,
		"failure_reason_path": response.FailureReasonPath,
		"result_path":         response.ResultPath,
	} {
		required := pathName == "status_path" || pathName == "result_path"
		if err := validateUpstreamAsyncPath(path, prefix+".poll.response."+pathName, required); err != nil {
			return err
		}
	}
	if len(response.StatusValues.Succeeded) == 0 || len(response.StatusValues.Failed) == 0 {
		return fmt.Errorf("%s.poll.response.status_values.succeeded and failed are required", prefix)
	}
	statusSets := []struct {
		name   string
		values []UpstreamAsyncStatusValue
	}{
		{"queued", response.StatusValues.Queued},
		{"in_progress", response.StatusValues.InProgress},
		{"succeeded", response.StatusValues.Succeeded},
		{"failed", response.StatusValues.Failed},
	}
	seenStatuses := make(map[string]string)
	for _, set := range statusSets {
		if len(set.values) > 64 {
			return fmt.Errorf("%s.poll.response.status_values.%s contains too many values", prefix, set.name)
		}
		for _, value := range set.values {
			key, err := value.key()
			if err != nil {
				return fmt.Errorf("%s.poll.response.status_values.%s: %w", prefix, set.name, err)
			}
			if previous, exists := seenStatuses[key]; exists {
				return fmt.Errorf("%s.poll.response.status_values.%s overlaps %s", prefix, set.name, previous)
			}
			seenStatuses[key] = set.name
		}
	}
	if len(response.UsagePaths) > len(upstreamAsyncUsageTargets) {
		return fmt.Errorf("%s.poll.response.usage_paths contains too many entries", prefix)
	}
	for target, path := range response.UsagePaths {
		if _, ok := upstreamAsyncUsageTargets[target]; !ok {
			return fmt.Errorf("%s.poll.response.usage_paths contains unsupported target %q", prefix, target)
		}
		if err := validateUpstreamAsyncPath(path, prefix+".poll.response.usage_paths."+target, true); err != nil {
			return err
		}
	}
	return nil
}

func validateUpstreamAsyncPath(path string, field string, required bool) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" && !required {
		return nil
	}
	if path != trimmed || trimmed == "" || len(trimmed) > 256 || !upstreamAsyncPathPattern.MatchString(trimmed) {
		return fmt.Errorf("%s must be a restricted GJSON path", field)
	}
	return nil
}

func validateUpstreamAsyncRequestURLPath(path, field string) error {
	trimmed := strings.TrimSpace(path)
	parsed, err := url.Parse(trimmed)
	if path != trimmed || err != nil || parsed.Path == "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || strings.ContainsAny(path, "?#") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("%s must be a relative absolute-path without query or fragment", field)
	}
	return validateUpstreamAsyncTemplates(path, field)
}

func validateUpstreamAsyncSubmitBody(value any, field string, depth int) error {
	if depth > 16 {
		return fmt.Errorf("%s exceeds the nesting limit", field)
	}
	switch typed := value.(type) {
	case nil, bool, float64, json.Number:
		return nil
	case string:
		if strings.Contains(typed, "{task_id}") {
			return fmt.Errorf("%s cannot use task_id before submission", field)
		}
		if path, ok := strings.CutPrefix(typed, "{request."); ok {
			if path, ok = strings.CutSuffix(path, "}"); ok {
				return validateUpstreamAsyncPath(path, field, true)
			}
		}
		return validateUpstreamAsyncTemplates(typed, field)
	case []any:
		for index, child := range typed {
			if err := validateUpstreamAsyncSubmitBody(child, fmt.Sprintf("%s[%d]", field, index), depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for key, child := range typed {
			if err := validateUpstreamAsyncSubmitBody(child, field+"."+key, depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%s contains an unsupported JSON value", field)
	}
}

func validateUpstreamAsyncHeaders(headers map[string]string, field string) error {
	for rawName, value := range headers {
		name := strings.TrimSpace(rawName)
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if rawName != name || !upstreamAsyncHeaderNamePattern.MatchString(name) || slices.Contains([]string{"Host", "Content-Length", "Connection", "Transfer-Encoding", "Upgrade", "Proxy-Authorization"}, canonical) {
			return fmt.Errorf("%s contains an unsafe header name", field)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s.%s contains a line break", field, canonical)
		}
		if err := validateUpstreamAsyncTemplates(value, field+"."+canonical); err != nil {
			return err
		}
	}
	return nil
}

func validateUpstreamAsyncTemplateValue(value any, field string, depth int) error {
	if depth > 16 {
		return fmt.Errorf("%s exceeds the nesting limit", field)
	}
	switch typed := value.(type) {
	case nil, bool, float64, json.Number:
		return nil
	case string:
		return validateUpstreamAsyncTemplates(typed, field)
	case []any:
		for index, child := range typed {
			if err := validateUpstreamAsyncTemplateValue(child, fmt.Sprintf("%s[%d]", field, index), depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for key, child := range typed {
			if err := validateUpstreamAsyncTemplateValue(child, field+"."+key, depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%s contains an unsupported JSON value", field)
	}
}

func validateUpstreamAsyncTemplates(value string, field string) error {
	if len(value) > 16<<10 || strings.ContainsAny(value, "\x00") {
		return fmt.Errorf("%s contains an invalid template", field)
	}
	remainder := value
	for _, placeholder := range []string{"{task_id}", "{model}", "{upstream_model}", "{api_key}"} {
		remainder = strings.ReplaceAll(remainder, placeholder, "")
	}
	if strings.ContainsAny(remainder, "{}") {
		return fmt.Errorf("%s contains an unsupported placeholder", field)
	}
	return nil
}

func upstreamAsyncProfilesOverlap(left, right UpstreamAsyncProfile) bool {
	if !strings.EqualFold(strings.TrimSpace(left.MediaType), strings.TrimSpace(right.MediaType)) {
		return false
	}
	operationOverlap := slices.ContainsFunc(left.Operations, func(operation string) bool {
		return slices.ContainsFunc(right.Operations, func(candidate string) bool {
			return strings.EqualFold(strings.TrimSpace(operation), strings.TrimSpace(candidate))
		})
	})
	if !operationOverlap {
		return false
	}
	if len(left.Models) == 0 || len(right.Models) == 0 {
		return true
	}
	return slices.ContainsFunc(left.Models, func(model string) bool {
		return slices.Contains(right.Models, model)
	})
}

func (config *UpstreamAsyncConfig) Match(mediaType, model, operation string) (*UpstreamAsyncProfile, bool) {
	if config == nil {
		return nil, false
	}
	for index := range config.Profiles {
		profile := &config.Profiles[index]
		if !profile.matches(mediaType, model, operation) {
			continue
		}
		clone, err := profile.Clone()
		if err != nil {
			return nil, false
		}
		return clone, true
	}
	return nil, false
}

func (config *UpstreamAsyncConfig) HasMatch(mediaType, model, operation string) bool {
	if config == nil {
		return false
	}
	return slices.ContainsFunc(config.Profiles, func(profile UpstreamAsyncProfile) bool {
		return profile.matches(mediaType, model, operation)
	})
}

func (profile UpstreamAsyncProfile) matches(mediaType, model, operation string) bool {
	if !strings.EqualFold(strings.TrimSpace(profile.MediaType), strings.TrimSpace(mediaType)) ||
		!slices.ContainsFunc(profile.Operations, func(candidate string) bool {
			return strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(operation))
		}) {
		return false
	}
	return len(profile.Models) == 0 || slices.Contains(profile.Models, strings.TrimSpace(model))
}

func (profile UpstreamAsyncProfile) Clone() (*UpstreamAsyncProfile, error) {
	encoded, err := kitutil.Marshal(UpstreamAsyncConfig{Profiles: []UpstreamAsyncProfile{profile}})
	if err != nil {
		return nil, err
	}
	var cloned UpstreamAsyncConfig
	if err := kitutil.Unmarshal(encoded, &cloned); err != nil {
		return nil, err
	}
	if len(cloned.Profiles) != 1 {
		return nil, fmt.Errorf("failed to clone upstream async profile")
	}
	return &cloned.Profiles[0], nil
}

func (config *UpstreamAsyncConfig) Clone() (*UpstreamAsyncConfig, error) {
	if config == nil {
		return nil, nil
	}
	encoded, err := kitutil.Marshal(config)
	if err != nil {
		return nil, err
	}
	var cloned UpstreamAsyncConfig
	if err := kitutil.Unmarshal(encoded, &cloned); err != nil {
		return nil, err
	}
	return &cloned, nil
}

func (config *UpstreamAsyncConfig) UnmarshalJSON(data []byte) error {
	var root map[string]json.RawMessage
	if err := kitutil.Unmarshal(data, &root); err != nil {
		return err
	}
	if err := rejectUpstreamAsyncUnknown(root, "upstream_async", "profiles"); err != nil {
		return err
	}
	var rawProfiles []json.RawMessage
	if err := kitutil.Unmarshal(root["profiles"], &rawProfiles); err != nil {
		return fmt.Errorf("upstream_async.profiles must be an array")
	}
	profiles := make([]UpstreamAsyncProfile, len(rawProfiles))
	for index, raw := range rawProfiles {
		profile, err := decodeUpstreamAsyncProfile(raw, index)
		if err != nil {
			return err
		}
		profiles[index] = profile
	}
	config.Profiles = profiles
	return nil
}

func decodeUpstreamAsyncProfile(data []byte, index int) (UpstreamAsyncProfile, error) {
	prefix := fmt.Sprintf("upstream_async.profiles[%d]", index)
	var raw map[string]json.RawMessage
	if err := kitutil.Unmarshal(data, &raw); err != nil {
		return UpstreamAsyncProfile{}, fmt.Errorf("%s must be an object", prefix)
	}
	if err := rejectUpstreamAsyncUnknown(raw, prefix, "id", "media_type", "models", "operations", "submit", "poll"); err != nil {
		return UpstreamAsyncProfile{}, err
	}
	var profile UpstreamAsyncProfile
	if err := decodeUpstreamAsyncFields(raw, map[string]any{"id": &profile.ID, "media_type": &profile.MediaType, "models": &profile.Models, "operations": &profile.Operations}); err != nil {
		return UpstreamAsyncProfile{}, fmt.Errorf("%s: %w", prefix, err)
	}
	var submit map[string]json.RawMessage
	if err := kitutil.Unmarshal(raw["submit"], &submit); err != nil {
		return UpstreamAsyncProfile{}, fmt.Errorf("%s.submit must be an object", prefix)
	}
	if err := rejectUpstreamAsyncUnknown(submit, prefix+".submit", "task_id_path", "request"); err != nil {
		return UpstreamAsyncProfile{}, err
	}
	if err := decodeUpstreamAsyncFields(submit, map[string]any{"task_id_path": &profile.Submit.TaskIDPath}); err != nil {
		return UpstreamAsyncProfile{}, fmt.Errorf("%s.submit: %w", prefix, err)
	}
	if rawRequest, exists := submit["request"]; exists {
		var request map[string]json.RawMessage
		if err := kitutil.Unmarshal(rawRequest, &request); err != nil || request == nil {
			return UpstreamAsyncProfile{}, fmt.Errorf("%s.submit.request must be an object", prefix)
		}
		if err := rejectUpstreamAsyncUnknown(request, prefix+".submit.request", "method", "path", "query", "headers", "body"); err != nil {
			return UpstreamAsyncProfile{}, err
		}
		profile.Submit.Request = &UpstreamAsyncSubmitRequest{}
		if err := decodeUpstreamAsyncFields(request, map[string]any{"method": &profile.Submit.Request.Method, "path": &profile.Submit.Request.Path, "query": &profile.Submit.Request.Query, "headers": &profile.Submit.Request.Headers, "body": &profile.Submit.Request.Body}); err != nil {
			return UpstreamAsyncProfile{}, fmt.Errorf("%s.submit.request: %w", prefix, err)
		}
	}
	var poll map[string]json.RawMessage
	if err := kitutil.Unmarshal(raw["poll"], &poll); err != nil {
		return UpstreamAsyncProfile{}, fmt.Errorf("%s.poll must be an object", prefix)
	}
	if err := rejectUpstreamAsyncUnknown(poll, prefix+".poll", "request", "response"); err != nil {
		return UpstreamAsyncProfile{}, err
	}
	var request map[string]json.RawMessage
	if err := kitutil.Unmarshal(poll["request"], &request); err != nil {
		return UpstreamAsyncProfile{}, fmt.Errorf("%s.poll.request must be an object", prefix)
	}
	if err := rejectUpstreamAsyncUnknown(request, prefix+".poll.request", "method", "path", "query", "headers", "body"); err != nil {
		return UpstreamAsyncProfile{}, err
	}
	if err := decodeUpstreamAsyncFields(request, map[string]any{"method": &profile.Poll.Request.Method, "path": &profile.Poll.Request.Path, "query": &profile.Poll.Request.Query, "headers": &profile.Poll.Request.Headers, "body": &profile.Poll.Request.Body}); err != nil {
		return UpstreamAsyncProfile{}, fmt.Errorf("%s.poll.request: %w", prefix, err)
	}
	var response map[string]json.RawMessage
	if err := kitutil.Unmarshal(poll["response"], &response); err != nil {
		return UpstreamAsyncProfile{}, fmt.Errorf("%s.poll.response must be an object", prefix)
	}
	if err := rejectUpstreamAsyncUnknown(response, prefix+".poll.response", "status_path", "status_values", "progress_path", "failure_reason_path", "result_path", "usage_paths", "download_headers"); err != nil {
		return UpstreamAsyncProfile{}, err
	}
	if err := decodeUpstreamAsyncFields(response, map[string]any{"status_path": &profile.Poll.Response.StatusPath, "progress_path": &profile.Poll.Response.ProgressPath, "failure_reason_path": &profile.Poll.Response.FailureReasonPath, "result_path": &profile.Poll.Response.ResultPath, "usage_paths": &profile.Poll.Response.UsagePaths, "download_headers": &profile.Poll.Response.DownloadHeaders}); err != nil {
		return UpstreamAsyncProfile{}, fmt.Errorf("%s.poll.response: %w", prefix, err)
	}
	var statuses map[string]json.RawMessage
	if err := kitutil.Unmarshal(response["status_values"], &statuses); err != nil {
		return UpstreamAsyncProfile{}, fmt.Errorf("%s.poll.response.status_values must be an object", prefix)
	}
	if err := rejectUpstreamAsyncUnknown(statuses, prefix+".poll.response.status_values", "queued", "in_progress", "succeeded", "failed"); err != nil {
		return UpstreamAsyncProfile{}, err
	}
	if err := decodeUpstreamAsyncFields(statuses, map[string]any{"queued": &profile.Poll.Response.StatusValues.Queued, "in_progress": &profile.Poll.Response.StatusValues.InProgress, "succeeded": &profile.Poll.Response.StatusValues.Succeeded, "failed": &profile.Poll.Response.StatusValues.Failed}); err != nil {
		return UpstreamAsyncProfile{}, fmt.Errorf("%s.poll.response.status_values: %w", prefix, err)
	}
	return profile, nil
}

func rejectUpstreamAsyncUnknown(raw map[string]json.RawMessage, field string, allowed ...string) error {
	for key := range raw {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("%s contains unknown field %q", field, key)
		}
	}
	return nil
}

func decodeUpstreamAsyncFields(raw map[string]json.RawMessage, targets map[string]any) error {
	for name, target := range targets {
		value, exists := raw[name]
		if !exists {
			continue
		}
		if err := kitutil.Unmarshal(value, target); err != nil {
			return fmt.Errorf("field %s has an invalid value", name)
		}
	}
	return nil
}

func UpstreamAsyncUsageTargetAllowed(target string) bool {
	_, ok := upstreamAsyncUsageTargets[target]
	return ok
}
