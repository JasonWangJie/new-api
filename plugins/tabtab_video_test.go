package plugins_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay/channel"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadTabtabVideoPlugin(t *testing.T) *jsplugin.LoadedPlugin {
	t.Helper()
	source, err := builtinplugins.Source("tabtab")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "tabtab"})
	require.NoError(t, err)
	return plugin
}

func TestTabtabVideoRequestsAndFrozenUsage(t *testing.T) {
	plugin := loadTabtabVideoPlugin(t)
	assert.Empty(t, plugin.Meta.ChannelTypes)
	assert.Equal(t, "https://api.tabtabapi.com", plugin.Meta.BaseURL)
	for _, tc := range []struct {
		name, model, action, ratio, resolution string
		seconds                                int
		request                                map[string]any
		args                                   map[string]any
		referenceVideo                         bool
	}{
		{name: "seedance default", model: "seedance-2.0", action: "text_to_video", ratio: "16:9", resolution: "720p", seconds: 5},
		{name: "seedance mixed references", model: "seedance-2.0", action: "reference_to_video", ratio: "9:16", resolution: "4k", seconds: 15,
			request: map[string]any{"duration": 15, "size": "4k", "ratio": "9:16", "images": []any{"https://cdn.example/image.png"}, "videos": []any{"https://cdn.example/ref.mp4"}, "audios": []any{"https://cdn.example/ref.mp3"}},
			args:    map[string]any{"images": []any{"https://cdn.example/image.png"}, "videos": []any{"https://cdn.example/ref.mp4"}, "audios": []any{"https://cdn.example/ref.mp3"}}, referenceVideo: true},
		{name: "seedance 25 audio only", model: "seedance-2.5", action: "reference_to_video", ratio: "16:9", resolution: "1080p", seconds: 30,
			request: map[string]any{"seconds": 30, "resolution": "1080p", "reference_audios": []any{"https://cdn.example/ref.mp3"}}, args: map[string]any{"audios": []any{"https://cdn.example/ref.mp3"}}},
		{name: "seedance first last", model: "seedance-2.0", action: "first_last_frame", ratio: "16:9", resolution: "720p", seconds: 5,
			request: map[string]any{"start_frame": "https://cdn.example/start.png", "end_frame": "https://cdn.example/end.png"}, args: map[string]any{"start_frame": "https://cdn.example/start.png", "end_frame": "https://cdn.example/end.png"}},
		{name: "kling reference defaults", model: "kling-4.0", action: "reference_to_video", ratio: "auto", resolution: "1080p", seconds: 5,
			request: map[string]any{"reference_images": []any{"https://cdn.example/image.png"}}, args: map[string]any{"images": []any{"https://cdn.example/image.png"}}},
		{name: "kling first image", model: "kling-4.0", action: "image_to_video", ratio: "auto", resolution: "1080p", seconds: 5,
			request: map[string]any{"input_reference": "https://cdn.example/start.png"}, args: map[string]any{"start_frame": "https://cdn.example/start.png"}},
		{name: "gemini single video", model: "gemini-omni-1.1-flash", action: "reference_to_video", ratio: "16:9", resolution: "360p", seconds: 10,
			request: map[string]any{"seconds": 10, "resolution": "360p", "video": "https://cdn.example/ref.mp4"}, args: map[string]any{"video": "https://cdn.example/ref.mp4"}, referenceVideo: true},
		{name: "gemini image", model: "gemini-omni-1.1-flash", action: "image_to_video", ratio: "16:9", resolution: "720p", seconds: 6,
			request: map[string]any{"image": "https://cdn.example/image.png"}, args: map[string]any{"images": []any{"https://cdn.example/image.png"}}},
		{name: "gemini mixed image video", model: "gemini-omni-1.1-flash", action: "reference_to_video", ratio: "16:9", resolution: "720p", seconds: 6,
			request: map[string]any{"image": "https://cdn.example/image.png", "video": "https://cdn.example/ref.mp4"}, args: map[string]any{"images": []any{"https://cdn.example/image.png"}, "video": "https://cdn.example/ref.mp4"}, referenceVideo: true},
		{name: "wan audio", model: "wan3.0-video", action: "reference_to_video", ratio: "auto", resolution: "720p", seconds: 5,
			request: map[string]any{"audios": []any{"https://cdn.example/ref.mp3"}}, args: map[string]any{"audios": []any{"https://cdn.example/ref.mp3"}}},
		{name: "wan first last", model: "wan3.0-video", action: "first_last_frame", ratio: "auto", resolution: "1080p", seconds: 2,
			request: map[string]any{"seconds": 2, "resolution": "1080p", "image": "https://cdn.example/start.png", "last_frame": "https://cdn.example/end.png"}, args: map[string]any{"start_frame": "https://cdn.example/start.png", "end_frame": "https://cdn.example/end.png"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"model": "public-alias", "prompt": " a mountain "}
			for key, value := range tc.request {
				request[key] = value
			}
			value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": "public-alias", "upstreamModel": tc.model, "operation": "create", "body": map[string]any{"kind": "json", "value": request},
			})
			require.NoError(t, err)
			intent := value.(map[string]any)
			assert.Equal(t, "public-alias", intent["model"])
			assert.Equal(t, tc.action, intent["action"])
			repeated, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": "public-alias", "upstreamModel": tc.model, "operation": "create", "body": map[string]any{"kind": "json", "value": intent["requestBody"]},
			})
			require.NoError(t, err)
			assert.Equal(t, intent, repeated, "host candidate and final decoders must preserve canonical requests")
			assert.Equal(t, " a mountain ", request["prompt"], "decoding must not mutate host input")
			profile, found := plugin.Meta.VideoProfileForModel(tc.model)
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, tc.action, intent["requestBody"], nil))
			ctx := map[string]any{"model": "public-alias", "upstreamModel": tc.model, "baseUrl": "https://channel.example/prefix/", "apiKey": "test-key", "requestBody": intent["requestBody"]}
			value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, err)
			descriptor := value.(map[string]any)
			assert.Equal(t, "POST", descriptor["method"])
			assert.Equal(t, "https://channel.example/prefix/api/task/create", descriptor["url"])
			assert.Equal(t, "Bearer test-key", descriptor["headers"].(map[string]any)["Authorization"])
			body := descriptor["body"].(map[string]any)
			assert.Equal(t, tc.model, body["model"])
			args := body["args"].(map[string]any)
			assert.EqualValues(t, tc.seconds, args["duration"])
			assert.Equal(t, "a mountain", args["prompt"])
			assert.Equal(t, tc.ratio, args["ratio"])
			assert.Equal(t, tc.resolution, args["resolution"])
			assert.Len(t, args, 4+len(tc.args))
			for name, expected := range tc.args {
				assert.Equal(t, expected, args[name], name)
			}
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"seconds": int64(tc.seconds), "resolution": tc.resolution, "reference_video": tc.referenceVideo}, value)
			ctx["usagePurpose"] = "billing_ratios"
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"seconds": int64(tc.seconds)}, value)
			value, err = plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": map[string]any{"code": "OK", "data": map[string]any{"id": "USG_private", "status": "processing", "charged_points": 999999999}}})
			require.NoError(t, err)
			receipt := value.(map[string]any)
			assert.Equal(t, "USG_private", receipt["taskId"])
			assert.Equal(t, map[string]any{"seconds": int64(tc.seconds), "resolution": tc.resolution, "reference_video": tc.referenceVideo}, receipt["state"])
		})
	}
}

func TestTabtabVideoValidationBeforeSubmission(t *testing.T) {
	plugin := loadTabtabVideoPlugin(t)
	for _, tc := range []struct {
		name, model string
		fields      map[string]any
	}{
		{name: "duration overflow", fields: map[string]any{"seconds": "18446744073686646784"}},
		{name: "fractional duration", fields: map[string]any{"seconds": 4.5}},
		{name: "zero duration", fields: map[string]any{"seconds": 0}},
		{name: "conflicting durations", fields: map[string]any{"seconds": 5, "duration": 6}},
		{name: "boolean duration", fields: map[string]any{"seconds": false}},
		{name: "metadata bypass", fields: map[string]any{"metadata": map[string]any{"duration": 999999}}},
		{name: "raw args bypass", fields: map[string]any{"args": map[string]any{"duration": 999999}}},
		{name: "unsupported resolution", fields: map[string]any{"resolution": "8k"}},
		{name: "private reference", fields: map[string]any{"image": "http://127.0.0.1/ref.png"}},
		{name: "credentialed reference", fields: map[string]any{"image": "https://user:key@cdn.example/ref.png"}},
		{name: "inline reference", fields: map[string]any{"image": "data:image/png;base64,YQ=="}},
		{name: "audio without visual", fields: map[string]any{"audios": []any{"https://cdn.example/a.mp3"}}},
		{name: "end without start", fields: map[string]any{"end_frame": "https://cdn.example/end.png"}},
		{name: "frames plus references", fields: map[string]any{"image": "https://cdn.example/start.png", "images": []any{"https://cdn.example/ref.png"}}},
		{name: "reference cap", fields: map[string]any{"images": make([]any, 10)}},
		{name: "aggregate cap", fields: map[string]any{"images": []any{"https://cdn.example/1.png", "https://cdn.example/2.png", "https://cdn.example/3.png", "https://cdn.example/4.png", "https://cdn.example/5.png", "https://cdn.example/6.png", "https://cdn.example/7.png", "https://cdn.example/8.png", "https://cdn.example/9.png"}, "videos": []any{"https://cdn.example/1.mp4", "https://cdn.example/2.mp4", "https://cdn.example/3.mp4"}, "audios": []any{"https://cdn.example/a.mp3"}}},
		{name: "kling audio", model: "kling-4.0", fields: map[string]any{"audios": []any{"https://cdn.example/a.mp3"}}},
		{name: "kling frame ratio", model: "kling-4.0", fields: map[string]any{"image": "https://cdn.example/start.png", "ratio": "16:9"}},
		{name: "gemini duration steps", model: "gemini-omni-1.1-flash", fields: map[string]any{"seconds": 5}},
		{name: "gemini frames", model: "gemini-omni-1.1-flash", fields: map[string]any{"start_frame": "https://cdn.example/start.png"}},
		{name: "gemini multiple videos", model: "gemini-omni-1.1-flash", fields: map[string]any{"videos": []any{"https://cdn.example/1.mp4", "https://cdn.example/2.mp4"}}},
		{name: "wan automatic billing", model: "wan3.0-video", fields: map[string]any{"duration": -1}},
		{name: "wan reference billing", model: "wan3.0-video", fields: map[string]any{"videos": []any{"https://cdn.example/ref.mp4"}}},
		{name: "wan reference role billing", model: "wan3.0-video", fields: map[string]any{"references": []any{map[string]any{"type": "video", "source": "https://cdn.example/ref.mp4"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			if model == "" {
				model = "seedance-2.0"
			}
			request := map[string]any{"prompt": "a mountain"}
			for key, value := range tc.fields {
				request[key] = value
			}
			ctx := map[string]any{"model": model, "operation": "create", "body": map[string]any{"kind": "json", "value": request}}
			_, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, ctx)
			require.Error(t, err)
			_, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": model, "requestBody": request})
			require.Error(t, err, "the outbound boundary must validate again")
		})
	}
}

func TestTabtabVideoPollingAndArtifactContract(t *testing.T) {
	plugin := loadTabtabVideoPlugin(t)
	ctx := map[string]any{"baseUrl": "https://channel.example/prefix", "taskId": "USG_private/id?x=1", "apiKey": "channel-secret"}
	value, err := plugin.Engine.Call(t.Context(), "buildQueryRequest", ctx)
	require.NoError(t, err)
	assert.Equal(t, "https://channel.example/prefix/api/task/status?id=USG_private%2Fid%3Fx%3D1", value.(map[string]any)["url"])
	for _, tc := range []struct{ status, want string }{{"processing", "IN_PROGRESS"}, {"succeeded", "SUCCESS"}, {"failed", "FAILURE"}, {"unknown", "UNKNOWN"}, {"toString", "UNKNOWN"}} {
		t.Run(tc.status, func(t *testing.T) {
			body := map[string]any{"code": "OK", "data": map[string]any{"id": ctx["taskId"], "status": tc.status, "error_message": "Provider rejected generation", "charged_points": 999999999, "results": []any{map[string]any{"url": "/files/video.mp4", "mime_type": "video/mp4"}}}}
			if tc.want != "SUCCESS" {
				delete(body["data"].(map[string]any), "results")
			}
			value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
			require.NoError(t, err)
			result := value.(map[string]any)
			assert.Equal(t, tc.want, result["status"])
			if tc.want == "SUCCESS" {
				assert.Equal(t, "https://channel.example/files/video.mp4", result["url"], "relative results resolve against the channel origin")
			}
			if tc.want == "FAILURE" {
				assert.Equal(t, "Provider rejected generation", result["reason"])
			}
			value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, result, body)
			require.NoError(t, err)
			assert.Empty(t, value, "provider points never replace frozen submitted usage")
		})
	}
	for _, body := range []any{
		map[string]any{"code": "OK", "data": map[string]any{"id": "different-task", "status": "processing"}},
		map[string]any{"code": "RATE_LIMITED", "data": map[string]any{"status": "processing"}},
		map[string]any{"code": "OK", "data": map[string]any{"status": "succeeded"}},
		map[string]any{"code": "OK", "data": map[string]any{"status": "succeeded", "results": []any{map[string]any{"url": "https://user:secret@cdn.example/video.mp4"}}}},
	} {
		_, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
		require.Error(t, err)
	}
	for _, url := range []string{"/files/video.mp4", "https://cdn.example/video.mp4"} {
		for _, method := range []string{"GET", "HEAD"} {
			value, err = plugin.Engine.Call(t.Context(), "buildContentRequest", map[string]any{
				"artifactKey": "video", "baseUrl": "https://channel.example/prefix", "apiKey": "must-not-leak", "clientRequest": map[string]any{"method": method},
				"data": map[string]any{"code": "OK", "data": map[string]any{"results": []any{map[string]any{"url": url}}}},
			})
			require.NoError(t, err)
			descriptor := value.(map[string]any)
			assert.Equal(t, method, descriptor["method"])
			assert.True(t, descriptor["credentialless"].(bool))
			assert.NotContains(t, descriptor, "headers")
		}
	}
	value, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "render"}, map[string]any{}, map[string]any{"data": map[string]any{"id": "USG_private", "charged_points": 99, "results": []any{map[string]any{"url": "https://cdn.example/private.mp4"}}}})
	require.NoError(t, err)
	assert.Empty(t, value, "public presentation must not expose the upstream id, URLs or points ledger")
}

func TestTabtabVideoHostAdaptorLifecycle(t *testing.T) {
	plugin := loadTabtabVideoPlugin(t)
	service.InitHttpClient()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer channel-secret", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			assert.Equal(t, "/api/task/create", r.URL.Path)
			var body map[string]any
			if !assert.NoError(t, common.DecodeJson(r.Body, &body)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Equal(t, "seedance-2.0", body["model"])
			assert.EqualValues(t, 5, body["args"].(map[string]any)["duration"])
			_, _ = io.WriteString(w, `{"code":"OK","data":{"id":"USG_private/id?x=1","status":"processing","charged_points":99999}}`)
			return
		}
		assert.Equal(t, "/api/task/status", r.URL.Path)
		assert.Equal(t, "USG_private/id?x=1", r.URL.Query().Get("id"))
		_, _ = io.WriteString(w, `{"code":"OK","data":{"id":"USG_private/id?x=1","status":"succeeded","charged_points":99999,"duration":30,"resolution":"4k","results":[{"url":"/files/video.mp4","mime_type":"video/mp4"}]}}`)
	}))
	defer upstream.Close()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"public-alias","prompt":"a mountain"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(jsplugin.ContextKeyPinnedEndpoint, jsplugin.PinnedEndpoint{Plugin: plugin, Protocol: "openai_video", Model: "public-alias"})
	c.Set(jsplugin.ContextKeyProtocolRequest, jsplugin.ProtocolRequestContext{
		RouteRequestContext: jsplugin.RouteRequestContext{Body: map[string]any{"kind": "json", "value": map[string]any{"model": "public-alias", "prompt": "a mountain"}}},
		Protocol:            "openai_video", Operation: "create", Model: "public-alias", UpstreamModel: "seedance-2.0",
	})
	info := &relaycommon.RelayInfo{OriginModelName: "public-alias", TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_public"}, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "seedance-2.0", ChannelBaseUrl: upstream.URL, ApiKey: "channel-secret"}}
	adaptor := taskplugin.New(plugin)
	adaptor.Init(info)
	require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
	assert.Equal(t, "text_to_video", info.Action)
	profile, found := plugin.Meta.VideoProfileForModel("seedance-2.0")
	require.True(t, found)
	request, exists := c.Get("task_request")
	require.True(t, exists)
	require.NoError(t, service.ValidateVideoProfileRequest(profile, info.Action, request, nil))
	facts, err := adaptor.ExtractUsageFactsValidated(c, info)
	require.NoError(t, err)
	assert.EqualValues(t, 5, facts["seconds"])
	assert.Equal(t, "720p", facts["resolution"])
	body, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	response, err := adaptor.DoRequest(c, info, body)
	require.NoError(t, err)
	defer response.Body.Close()
	receipt, taskErr := adaptor.ParseResponse(c, response, info)
	require.Nil(t, taskErr)
	require.NotNil(t, receipt)
	assert.Equal(t, "USG_private/id?x=1", receipt.UpstreamTaskID)
	task := &model.Task{TaskID: "task_public", Platform: "tabtab", Status: model.TaskStatusInProgress, Action: info.Action, Data: receipt.TaskData,
		Properties: model.Properties{OriginModelName: "public-alias", UpstreamModelName: "seedance-2.0"}, PrivateData: model.TaskPrivateData{UpstreamTaskID: receipt.UpstreamTaskID, PluginState: receipt.PluginState}}
	frozenState := string(task.PrivateData.PluginState)
	response, err = adaptor.FetchTaskContext(t.Context(), upstream.URL, "channel-secret", task, "")
	require.NoError(t, err)
	defer response.Body.Close()
	pollBody, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	result, err := adaptor.ParseTaskResult(task, response, pollBody)
	require.NoError(t, err)
	assert.Equal(t, string(model.TaskStatusSuccess), result.Status)
	assert.Equal(t, upstream.URL+"/files/video.mp4", result.Url)
	assert.Empty(t, result.UsageFacts, "completion retains the host's frozen submitted usage")
	assert.Equal(t, frozenState, string(task.PrivateData.PluginState))
	var frozenFacts map[string]any
	require.NoError(t, common.Unmarshal(task.PrivateData.PluginState, &frozenFacts))
	assert.Equal(t, map[string]any{"seconds": float64(5), "resolution": "720p", "reference_video": false}, frozenFacts)
	task.Status, task.Data = model.TaskStatusSuccess, pollBody
	artifacts, err := adaptor.ListArtifacts(task)
	require.NoError(t, err)
	assert.Equal(t, []channel.TaskArtifact{{Key: "video", Type: "video", MimeType: "video/mp4"}}, artifacts)
	content, err := adaptor.BuildContentRequest(task, "video", channel.TaskArtifactClientRequest{Method: http.MethodGet})
	require.NoError(t, err)
	assert.Equal(t, upstream.URL+"/files/video.mp4", content.URL)
	assert.True(t, content.Credentialless)
	assert.Empty(t, content.Headers)
	public, err := adaptor.ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	assert.NotContains(t, string(public), "USG_private")
	assert.NotContains(t, string(public), "charged_points")
	assert.NotContains(t, string(public), upstream.URL)
	for _, status := range []string{"failed", "new-status", ""} {
		data, err := common.Marshal(map[string]any{"code": "OK", "data": map[string]any{"id": "accepted-id", "status": status, "error_code": "GENERATION_FAILED", "error_message": "Provider rejected generation"}})
		require.NoError(t, err)
		receipt, taskErr = adaptor.ParseResponse(c, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data)))}, info)
		require.Nil(t, taskErr)
		assert.Equal(t, "accepted-id", receipt.UpstreamTaskID)
		if status == "failed" {
			require.NotNil(t, receipt.Immediate)
			assert.Equal(t, string(model.TaskStatusFailure), receipt.Immediate.Status)
			assert.Equal(t, "GENERATION_FAILED: Provider rejected generation", receipt.Immediate.Reason)
		} else {
			assert.Nil(t, receipt.Immediate, "a valid accepted id remains available for polling")
		}
	}
	for _, input := range []map[string]any{
		{"code": "RATE_LIMITED", "error_message": "Please retry later"},
		{"code": "OK", "data": map[string]any{"status": "failed", "error_code": "AUTH_ERROR", "error_message": "Bearer channel-secret sk_untrusted-token"}},
	} {
		value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{"apiKey": "channel-secret"}, input)
		if input["code"] == "RATE_LIMITED" {
			require.ErrorContains(t, err, "RATE_LIMITED: Please retry later")
			continue
		}
		require.NoError(t, err)
		reason := value.(map[string]any)["reason"].(string)
		assert.Contains(t, reason, "AUTH_ERROR")
		assert.NotContains(t, reason, "channel-secret")
		assert.NotContains(t, reason, "sk_untrusted-token")
	}
}

func TestTabtabVideoPublishedModelBoundaries(t *testing.T) {
	plugin := loadTabtabVideoPlugin(t)
	for _, tc := range []struct {
		model       string
		min, max    int
		resolutions []string
	}{
		{"seedance-2.0", 4, 15, []string{"480p", "720p", "1080p", "4k"}},
		{"seedance-2.5", 4, 30, []string{"480p", "720p", "1080p"}},
		{"kling-4.0", 3, 30, []string{"720p", "1080p"}},
		{"gemini-omni-1.1-flash", 4, 10, []string{"360p", "720p", "1080p"}},
		{"wan3.0-video", 2, 30, []string{"480p", "720p", "1080p"}},
	} {
		t.Run(tc.model, func(t *testing.T) {
			for _, duration := range []int{tc.min - 1, tc.min, tc.max, tc.max + 1} {
				t.Run(fmt.Sprintf("duration_%d", duration), func(t *testing.T) {
					ctx := map[string]any{"model": "mapped-alias", "upstreamModel": tc.model, "operation": "create", "body": map[string]any{"kind": "json", "value": map[string]any{"prompt": "draw", "seconds": duration}}}
					value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, ctx)
					if duration < tc.min || duration > tc.max {
						require.Error(t, err)
						return
					}
					require.NoError(t, err)
					intent := value.(map[string]any)
					profile, found := plugin.Meta.VideoProfileForModel(tc.model)
					require.True(t, found)
					require.NoError(t, service.ValidateVideoProfileRequest(profile, "text_to_video", intent["requestBody"], nil))
					value, err = plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{"model": "mapped-alias", "upstreamModel": tc.model, "requestBody": intent["requestBody"]})
					require.NoError(t, err)
					assert.EqualValues(t, duration, value.(map[string]any)["seconds"])
				})
			}
			for _, resolution := range append(tc.resolutions, "8k") {
				t.Run("resolution_"+resolution, func(t *testing.T) {
					value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
						"model": tc.model, "body": map[string]any{"kind": "json", "value": map[string]any{"prompt": "draw", "resolution": resolution}},
					})
					if resolution == "8k" {
						require.Error(t, err)
						return
					}
					require.NoError(t, err)
					intent := value.(map[string]any)
					profile, found := plugin.Meta.VideoProfileForModel(tc.model)
					require.True(t, found)
					require.NoError(t, service.ValidateVideoProfileRequest(profile, "text_to_video", intent["requestBody"], nil))
				})
			}
		})
	}
}

func TestTabtabVideoReferenceMergingAndHostProfiles(t *testing.T) {
	plugin := loadTabtabVideoPlugin(t)
	for _, tc := range []struct {
		model                  string
		images, videos, audios int
	}{
		{"seedance-2.0", 9, 3, 0}, {"seedance-2.5", 30, 10, 10}, {"kling-4.0", 10, 5, 0}, {"gemini-omni-1.1-flash", 10, 1, 0}, {"wan3.0-video", 10, 0, 5},
	} {
		t.Run(tc.model, func(t *testing.T) {
			request := map[string]any{"prompt": "use the references"}
			for _, media := range []struct {
				field string
				count int
			}{{"reference_images", tc.images}, {"reference_videos", tc.videos}, {"reference_audios", tc.audios}} {
				if media.count == 0 {
					continue
				}
				urls := make([]any, media.count)
				for i := range urls {
					urls[i] = fmt.Sprintf("https://cdn.example/%s/%d", media.field, i)
				}
				request[media.field] = urls
			}
			value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{"model": tc.model, "body": map[string]any{"kind": "json", "value": request}})
			require.NoError(t, err)
			intent := value.(map[string]any)
			assert.Equal(t, "reference_to_video", intent["action"])
			profile, found := plugin.Meta.VideoProfileForModel(tc.model)
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, "reference_to_video", intent["requestBody"], nil))
			request["image_urls"] = []any{"https://cdn.example/over-limit.png"}
			_, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{"model": tc.model, "body": map[string]any{"kind": "json", "value": request}})
			require.Error(t, err, "different aliases must count toward one aggregate limit")
		})
	}
	for _, tc := range []struct {
		name, action string
		fields       map[string]any
		invalid      bool
	}{
		{name: "object and string frame aliases agree", action: "image_to_video", fields: map[string]any{"image": map[string]any{"url": "https://cdn.example/start.png"}, "start_frame": "https://cdn.example/start.png"}},
		{name: "conflicting frame objects", invalid: true, fields: map[string]any{"image": map[string]any{"url": "https://cdn.example/one.png"}, "input_reference": map[string]any{"url": "https://cdn.example/two.png"}}},
		{name: "typed first last roles", action: "first_last_frame", fields: map[string]any{"references": []any{map[string]any{"type": "image", "role": "last_frame", "source": "https://cdn.example/end.png"}, map[string]any{"type": "image", "role": "first_frame", "source": "https://cdn.example/start.png"}}}},
		{name: "typed mixed reference roles", action: "reference_to_video", fields: map[string]any{"images": []any{"https://cdn.example/first.png"}, "references": []any{map[string]any{"type": "image", "role": "reference_image", "source": "https://cdn.example/second.png"}, map[string]any{"type": "video", "role": "reference_video", "source": "https://cdn.example/clip.mp4"}, map[string]any{"type": "audio", "role": "reference_audio", "source": "https://cdn.example/voice.mp3"}}}},
		{name: "duplicate frame role", invalid: true, fields: map[string]any{"image": "https://cdn.example/first.png", "references": []any{map[string]any{"type": "image", "role": "first_frame", "source": "https://cdn.example/other.png"}}}},
		{name: "mismatched typed role", invalid: true, fields: map[string]any{"references": []any{map[string]any{"type": "image", "role": "reference_video", "source": "https://cdn.example/ref.png"}}}},
		{name: "reference metadata bypass", invalid: true, fields: map[string]any{"references": []any{map[string]any{"type": "image", "source": "https://cdn.example/ref.png", "duration": 100000}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"prompt": "use references"}
			for name, value := range tc.fields {
				request[name] = value
			}
			value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{"model": "seedance-2.0", "body": map[string]any{"kind": "json", "value": request}})
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			intent := value.(map[string]any)
			assert.Equal(t, tc.action, intent["action"])
			profile, found := plugin.Meta.VideoProfileForModel("seedance-2.0")
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, tc.action, intent["requestBody"], nil))
		})
	}
}

func TestTabtabVideoHostRejectsAmbiguousReceiptsAndUnsafeURLs(t *testing.T) {
	plugin := loadTabtabVideoPlugin(t)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Set("task_request", map[string]any{"prompt": "draw"})
	info := &relaycommon.RelayInfo{OriginModelName: "seedance-2.0", TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "public-id"}, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "seedance-2.0", ChannelBaseUrl: "https://channel.example", ApiKey: "channel-secret"}}
	adaptor := taskplugin.New(plugin)
	adaptor.Init(info)
	for _, tc := range []struct {
		name, json string
		unknown    bool
	}{
		{"non-OK accepted id", `{"code":"UPSTREAM_ERROR","data":{"id":"known","status":"failed"}}`, false},
		{"failed result", `{"code":"OK","data":{"id":"known","status":"failed","results":[{"url":"https://cdn.example/output.mp4"}]}}`, false},
		{"failed malformed results", `{"code":"OK","data":{"id":"known","status":"failed","results":"https://cdn.example/output.mp4"}}`, false},
		{"failed nested output", `{"code":"OK","data":{"id":"known","status":"failed","output":{"url":"https://cdn.example/output.mp4"}}}`, false},
		{"failed outer output", `{"code":"OK","url":"https://cdn.example/output.mp4","data":{"id":"known","status":"failed"}}`, false},
		{"status array", `{"code":"OK","data":{"id":"known","status":["failed"]}}`, true},
		{"status object", `{"code":"OK","data":{"id":"known","status":{"value":"succeeded"}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt, taskErr := adaptor.ParseResponse(c, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.json))}, info)
			require.Nil(t, taskErr)
			assert.Equal(t, "known", receipt.UpstreamTaskID)
			assert.Nil(t, receipt.Immediate, "ambiguous accepted work must not immediately fail and refund")
			task := &model.Task{TaskID: "public-id", Properties: model.Properties{OriginModelName: "seedance-2.0"}, PrivateData: model.TaskPrivateData{UpstreamTaskID: "known"}}
			result, err := adaptor.ParseTaskResult(task, &http.Response{StatusCode: http.StatusOK}, []byte(tc.json))
			if tc.unknown {
				require.NoError(t, err)
				assert.Equal(t, "UNKNOWN", result.Status)
				return
			}
			require.Error(t, err)
		})
	}
	for _, raw := range []string{
		`{"code":"UPSTREAM_ERROR","data":{"results":[{"url":"https://cdn.example/output.mp4"}]}}`,
		`{"code":"OK","data":{"id":"bad\u0001id","status":"processing"}}`,
		`{"code":"OK","data":{"id":123,"status":"processing"}}`,
	} {
		receipt, taskErr := adaptor.ParseResponse(c, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}, info)
		require.NotNil(t, taskErr)
		assert.Nil(t, receipt, "invalid or absent identifiers must never be invented")
	}
	for _, tc := range []struct{ name, url string }{
		{"hex loopback", "http://0x7f.0.0.1/video.mp4"}, {"private IPv4", "http://192.168.1.3/video.mp4"},
		{"credentials", "https://user:secret@cdn.example/video.mp4"}, {"scheme relative", "//cdn.example/video.mp4"},
		{"fragment", "/files/video.mp4#private"}, {"wrong scheme", "file:///tmp/video.mp4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := common.Marshal(map[string]any{"code": "OK", "data": map[string]any{"id": "known", "status": "succeeded", "results": []any{map[string]any{"url": tc.url}}}})
			require.NoError(t, err)
			task := &model.Task{TaskID: "public-id", Status: model.TaskStatusSuccess, Data: data, PrivateData: model.TaskPrivateData{UpstreamTaskID: "known"}}
			_, err = adaptor.BuildContentRequest(task, "video", channel.TaskArtifactClientRequest{Method: http.MethodGet})
			require.Error(t, err)
		})
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		data := []byte(`{"code":"OK","data":{"id":"known","status":"succeeded","results":[{"url":"https://cdn.example/video.mp4"}]}}`)
		content, err := adaptor.BuildContentRequest(&model.Task{TaskID: "public-id", Status: model.TaskStatusSuccess, Data: data}, "video", channel.TaskArtifactClientRequest{Method: method})
		require.NoError(t, err)
		assert.Equal(t, method, content.Method)
		assert.True(t, content.Credentialless)
		assert.Empty(t, content.Headers)
	}
	_, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{"model": "seedance-2.0", "body": map[string]any{"kind": "json", "value": map[string]any{"prompt": "draw", "image": "http://0x7f.0.0.1/image.png"}}})
	require.Error(t, err, "alternate private IP spellings must not bypass reference validation")
	data := []byte(`{"code":"OK","data":{"id":"known","status":"failed","error_code":"AUTH_ERROR","error_message":"API key=other-secret password=other-password https://user:pass@secret.example/path"}}`)
	result, err := adaptor.ParseTaskResult(&model.Task{TaskID: "public-id", PrivateData: model.TaskPrivateData{UpstreamTaskID: "known"}}, &http.Response{StatusCode: http.StatusOK}, data)
	require.NoError(t, err)
	assert.Contains(t, result.Reason, "AUTH_ERROR")
	for _, secret := range []string{"other-secret", "other-password", "user:pass", "secret.example"} {
		assert.NotContains(t, result.Reason, secret)
	}
}
