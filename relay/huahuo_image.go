package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
)

const huahuoImageMaxReceiptBytes = 1 << 20

type huahuoImageResult struct {
	Status string `json:"status"`
	URL    string `json:"url"`
	State  struct {
		Code      string `json:"code"`
		RequestID string `json:"requestId"`
		Review    bool   `json:"review"`
	} `json:"state"`
}

func buildHuahuoImageSubmit(ctx context.Context, plugin *jsplugin.LoadedPlugin, channel model.Channel, request service.AsyncImageRequest, references []string, key, apiKey string) (*http.Request, error) {
	if request.Dialect != "async" || request.Count != 1 {
		return nil, errors.New("Huahuo images require an asynchronous request with n=1")
	}
	if request.ReferenceImageCount() > channel.GetImageMaxReferenceImages() {
		return nil, errors.New("Huahuo image reference count exceeds the channel limit")
	}
	if channel.ParamOverride != nil && *channel.ParamOverride != "" && *channel.ParamOverride != "{}" {
		return nil, errors.New("Huahuo image requests use normalized parameters and cannot apply a channel parameter override")
	}
	var quality string
	for name, value := range request.Native {
		switch name {
		case "model", "prompt", "n", "size", "resolution", "aspect_ratio", "ratio", "images", "user", "stream":
		case "quality":
			if err := common.Unmarshal(value, &quality); err != nil {
				return nil, errors.New("quality must be a string")
			}
		case "response_format":
			var format string
			if err := common.Unmarshal(value, &format); err != nil || format != "" && format != "url" {
				return nil, errors.New("Huahuo images require response_format=url")
			}
		default:
			return nil, fmt.Errorf("Huahuo image requests do not support %s", name)
		}
	}
	if references == nil {
		// Admission has no I/O and does not resolve stored input aliases.
		references = make([]string, 0, request.ReferenceImageCount())
		for _, part := range request.Parts {
			if part.Type == "mask" {
				return nil, errors.New("Huahuo image requests do not support masks")
			}
			if part.Type == "image_url" {
				if service.IsGatewayImageReference(part.URL) || strings.HasPrefix(strings.ToLower(part.URL), "data:") {
					references = append(references, "data:image/png;base64,AA==")
				} else {
					references = append(references, part.URL)
				}
			}
		}
	}
	if request.ReferenceImageCount() != len(references) {
		return nil, errors.New("Huahuo image reference preparation is incomplete")
	}
	for _, reference := range references {
		if strings.HasPrefix(strings.ToLower(reference), "data:") {
			continue
		}
		if _, err := service.ValidateImageReferenceURL(reference); err != nil {
			return nil, errors.New("Huahuo requires public HTTPS image references")
		}
	}
	upstream, err := service.ResolveImageChannelUpstreamModel(channel, request.Model)
	if err != nil {
		return nil, err
	}
	size, resolution := strings.TrimSpace(request.Size), strings.ToUpper(strings.TrimSpace(request.Resolution))
	if resolution == "AUTO" || resolution == "自动" {
		resolution = ""
	}
	switch tier := strings.ToUpper(size); tier {
	case "0.5K", "1K", "2K", "4K":
		if resolution != "" && resolution != tier {
			return nil, errors.New("Huahuo image size conflicts with resolution")
		}
		size, resolution = "", tier
	}
	if strings.EqualFold(size, "auto") || size == "自动" || request.ExplicitTier {
		size = ""
	}
	ratio := strings.TrimSpace(request.AspectRatio)
	if strings.EqualFold(ratio, "auto") || ratio == "自动" {
		ratio = ""
	}
	values := huahuoImageDriverContext(channel.GetBaseURL(), apiKey, request.Model, upstream)
	values["requestHeaders"] = map[string]string{"Idempotency-Key": key}
	values["requestBody"] = map[string]any{"prompt": request.Prompt, "n": request.Count, "size": size, "aspectRatio": ratio, "resolution": resolution, "quality": quality, "images": references}
	return huahuoImageHTTPRequest(ctx, plugin, "buildSubmitRequest", values, http.MethodPost, key)
}

func huahuoImageDriverContext(baseURL, apiKey, model, upstream string) map[string]any {
	return map[string]any{
		"baseUrl": strings.TrimRight(baseURL, "/"), "apiKey": apiKey, "authHeader": "Bearer " + apiKey,
		"model": model, "upstreamModel": upstream,
	}
}

func huahuoImageHTTPRequest(ctx context.Context, plugin *jsplugin.LoadedPlugin, hook string, values map[string]any, method, key string) (*http.Request, error) {
	value, err := plugin.Engine.Call(ctx, hook, values)
	if err != nil {
		return nil, err
	}
	var descriptor struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
		Body    common.RawMessage `json:"body"`
	}
	encoded, err := common.Marshal(value)
	if err != nil || len(encoded) > 64<<20 || common.Unmarshal(encoded, &descriptor) != nil || descriptor.Method != method {
		return nil, errors.New("Huahuo image request descriptor is invalid")
	}
	base, _ := url.Parse(values["baseUrl"].(string))
	requestURL, err := url.Parse(descriptor.URL)
	if err != nil || base == nil || requestURL.Scheme != base.Scheme || (requestURL.Scheme != "https" && requestURL.Scheme != "http") || requestURL.User != nil || requestURL.RawQuery != "" || requestURL.Fragment != "" || jsplugin.ValidateRequestURL(descriptor.URL, base.String(), nil) != nil {
		return nil, errors.New("Huahuo image request must use the channel origin")
	}
	var body io.Reader
	if method == http.MethodPost {
		if key == "" || descriptor.Headers["Idempotency-Key"] != key {
			return nil, errors.New("Huahuo image submit request has an invalid recovery key")
		}
		body = bytes.NewReader(descriptor.Body)
	}
	request, err := http.NewRequestWithContext(ctx, method, descriptor.URL, body)
	if err != nil {
		return nil, errors.New("Huahuo image request URL is invalid")
	}
	for name, value := range descriptor.Headers {
		request.Header.Set(name, value)
	}
	if method == http.MethodPost {
		if err := service.ValidateUpstreamAsyncImageSubmitCount(request, 1); err != nil {
			return nil, err
		}
		// Disable automatic net/http replay on a stale pooled connection even
		// though the outgoing operation has a valid idempotency key.
		request.GetBody = nil
	}
	return request, nil
}

func executeHuahuoImageRequest(ctx context.Context, task model.AsyncImageTask, request service.AsyncImageRequest, channel model.Channel, info *relaycommon.RelayInfo, references []string, beforeInvoke func() error) (string, error) {
	key := task.UpstreamTaskId
	query := key != ""
	plugin, available := jsplugin.DefaultRegistry.Get(constant.HuahuoImagePluginKey)
	if !available {
		if query {
			return "", &service.AsyncImagePending{TaskId: key}
		}
		return "", &service.AsyncImageFailure{Code: 604, InternalCode: "invalid_request", Message: "Huahuo image plugin is unavailable", HTTPStatus: http.StatusBadRequest}
	}
	client, err := service.GetHttpClientWithProxySettings(info.ChannelSetting.Proxy, info.ChannelSetting)
	if err != nil {
		if query {
			return "", &service.AsyncImagePending{TaskId: key}
		}
		return "", err
	}
	transport := *client
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	values := huahuoImageDriverContext(info.ChannelBaseUrl, info.ApiKey, info.OriginModelName, info.UpstreamModelName)
	var upstream *http.Request
	if query {
		values["taskId"] = key
		upstream, err = huahuoImageHTTPRequest(ctx, plugin, "buildQueryRequest", values, http.MethodGet, "")
	} else {
		key = task.HuahuoImageRequestKey()
		values["requestHeaders"] = map[string]string{"Idempotency-Key": key}
		upstream, err = buildHuahuoImageSubmit(ctx, plugin, channel, request, references, key, info.ApiKey)
	}
	if err != nil {
		if query {
			return "", &service.AsyncImagePending{TaskId: key}
		}
		return "", &service.AsyncImageFailure{Code: 604, InternalCode: "invalid_request", Message: err.Error(), HTTPStatus: http.StatusBadRequest}
	}
	if !query {
		if err := beforeInvoke(); err != nil {
			return "", err
		}
	}
	response, err := transport.Do(upstream)
	if err != nil {
		return "", &service.AsyncImagePending{TaskId: key}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, huahuoImageMaxReceiptBytes+1))
	var decoded any
	if err != nil || len(body) > huahuoImageMaxReceiptBytes || common.Unmarshal(body, &decoded) != nil {
		return "", &service.AsyncImagePending{TaskId: key}
	}
	var parsed huahuoImageResult
	var output any
	if query {
		output, err = plugin.Engine.Call(ctx, "parseTaskResult", values, decoded, map[string]any{"status": response.StatusCode})
	} else {
		output, err = plugin.Engine.Call(ctx, "parseSubmitResponse", values, map[string]any{"statusCode": response.StatusCode, "body": decoded})
	}
	if err != nil {
		return "", &service.AsyncImagePending{TaskId: key}
	}
	encoded, err := common.Marshal(output)
	if err != nil || len(encoded) > huahuoImageMaxReceiptBytes {
		return "", &service.AsyncImagePending{TaskId: key}
	}
	if query {
		err = common.Unmarshal(encoded, &parsed)
	} else {
		var submit struct {
			TaskID    string            `json:"taskId"`
			Immediate huahuoImageResult `json:"immediate"`
			State     common.RawMessage `json:"state"`
		}
		err = common.Unmarshal(encoded, &submit)
		if submit.TaskID != key || err != nil || common.Unmarshal(submit.State, &submit.Immediate.State) != nil {
			return "", &service.AsyncImagePending{TaskId: key}
		}
		parsed = submit.Immediate
	}
	if err != nil {
		return "", &service.AsyncImagePending{TaskId: key}
	}
	if parsed.State.Code == "delivery_timeout" {
		return "", &service.AsyncImageFailure{Code: 608, InternalCode: "huahuo_delivery_timeout", ProviderCode: parsed.State.Code, UpstreamRequestID: parsed.State.RequestID, Message: "Huahuo reported a delivery timeout and its charge requires reconciliation", HTTPStatus: response.StatusCode, ExecutionUnknown: true}
	}
	if parsed.State.Review {
		return "", &service.AsyncImageFailure{Code: 608, InternalCode: "huahuo_charge_unconfirmed", ProviderCode: parsed.State.Code, UpstreamRequestID: parsed.State.RequestID, Message: "Huahuo reported conflicting failure and charge information", HTTPStatus: response.StatusCode, ExecutionUnknown: true}
	}
	if parsed.State.RequestID != "" && parsed.State.RequestID != key {
		return "", &service.AsyncImagePending{TaskId: key}
	}
	if parsed.Status == "SUCCESS" && response.StatusCode == http.StatusOK && strings.HasPrefix(strings.ToLower(parsed.URL), "https://") && service.PublicUpstreamAsyncResultURL(parsed.URL) {
		return parsed.URL, nil
	}
	if parsed.Status == "FAILURE" {
		code := 606
		if parsed.State.Code == "image_concurrency_limit" || parsed.State.Code == "storage_capacity_exceeded" || parsed.State.Code == "storage_check_unavailable" {
			code = 603
		}
		return "", &service.AsyncImageFailure{Code: code, InternalCode: "upstream_job_failed", ProviderCode: parsed.State.Code, UpstreamRequestID: parsed.State.RequestID, Message: "Huahuo image generation failed", HTTPStatus: response.StatusCode, UpstreamTerminal: true}
	}
	return "", &service.AsyncImagePending{TaskId: key}
}

var huahuoImageDownloadProfile = dto.UpstreamAsyncProfile{ID: constant.HuahuoImagePluginKey, MediaType: dto.UpstreamAsyncMediaImage}
