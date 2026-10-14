package plugins_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	pluginadaptor "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadDramaVideoPlugin(t *testing.T) *jsplugin.LoadedPlugin {
	t.Helper()
	source, err := builtinplugins.Source("drama")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "drama"})
	require.NoError(t, err)
	return plugin
}

func decodeDramaVideoRequest(t *testing.T, plugin *jsplugin.LoadedPlugin, modelName, operation string, body map[string]any) (map[string]any, error) {
	t.Helper()
	value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"model": modelName, "operation": operation, "body": map[string]any{"kind": "json", "value": body},
	})
	if err != nil {
		return nil, err
	}
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var intent map[string]any
	require.NoError(t, common.Unmarshal(encoded, &intent))
	return intent, nil
}

func TestDramaVideoModelsAndUpstreamRoutes(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	for _, tc := range []struct {
		model, resolution, path string
	}{
		{"seedance2.0-E-720p", "720p", "/v1/video/generations"},
		{"seedance2.0-E-1080p", "1080p", "/v1/video/generations"},
		{"seedance2.0-F-720p", "720p", "/v1/video/generations"},
		{"seedance2.0-fast-F-720p", "720p", "/v1/video/generations"},
		{"seedance2.0-G-720p", "720p", "/v1/video/generations"},
		{"seedance2.0-E", "480p", "/v1/videos"},
		{"seedance2.0-fast-E", "720p", "/v1/videos"},
		{"seedance2.0-Mini-E", "480p", "/v1/videos"},
		{"seedance2.5-E", "1080p", "/v1/videos"},
		{"seedance2.0-F", "720px", "/v1/videos"},
		{"seedance2.5-F", "720p", "/v1/videos"},
		{"seedance2.0-G", "4k", "/v1/videos"},
		{"seedance2.0-fast-G", "720p", "/v1/videos"},
		{"seedance2.0-mini-G", "480p", "/v1/videos"},
		{"seedance2.5-G", "1080p", "/v1/videos"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			binding, found := jsplugin.DefaultRegistry.Generation().LookupEndpoint(http.MethodPost, "/v1/videos", tc.model)
			require.True(t, found)
			assert.Equal(t, "drama", binding.Plugin.Meta.Key)
			profile, found := plugin.Meta.VideoProfileForModel(tc.model)
			require.True(t, found)
			request := map[string]any{"prompt": "waves at sunset", "seconds": 5, "resolution": tc.resolution, "aspect_ratio": "16:9"}
			intent, err := decodeDramaVideoRequest(t, plugin, tc.model, "create", request)
			require.NoError(t, err)
			assert.Equal(t, "text_to_video", intent["action"])
			require.NoError(t, service.ValidateVideoProfileRequest(profile, "text_to_video", intent["requestBody"], nil))
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"model": tc.model, "requestBody": intent["requestBody"], "action": intent["action"], "baseUrl": "https://upstream.example", "apiKey": "upstream-test-key",
			})
			require.NoError(t, err)
			descriptor := value.(map[string]any)
			assert.Equal(t, "https://upstream.example"+tc.path, descriptor["url"])
			assert.Equal(t, "POST", descriptor["method"])
			assert.Equal(t, "Bearer upstream-test-key", descriptor["headers"].(map[string]any)["Authorization"])
			body := descriptor["body"].(map[string]any)
			assert.Equal(t, tc.model, body["model"])
			assert.Equal(t, tc.resolution, body["resolution"])
			assert.EqualValues(t, 5, body["seconds"])
			value, err = plugin.Engine.Call(t.Context(), "buildQueryRequest", map[string]any{
				"model": tc.model, "baseUrl": "https://upstream.example", "apiKey": "upstream-test-key", "taskId": "task/private?one",
			})
			require.NoError(t, err)
			assert.Equal(t, "https://upstream.example"+tc.path+"/task%2Fprivate%3Fone", value.(map[string]any)["url"])
		})
	}
	t.Run("mapping keeps client identity", func(t *testing.T) {
		value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "my-video", "upstreamModel": "seedance2.5-G", "operation": "create", "body": map[string]any{"kind": "json", "value": map[string]any{"prompt": "waves", "seconds": 8}},
		})
		require.NoError(t, err)
		intent := value.(map[string]any)
		assert.Equal(t, "my-video", intent["model"])
		value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
			"model": "my-video", "upstreamModel": "seedance2.5-G", "baseUrl": "https://upstream.example", "apiKey": "test-key", "requestBody": intent["requestBody"],
		})
		require.NoError(t, err)
		assert.Equal(t, "seedance2.5-G", value.(map[string]any)["body"].(map[string]any)["model"])
		assert.NotContains(t, value.(map[string]any), "model")
	})
}

func TestDramaVideoMediaAndOperations(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	for _, tc := range []struct {
		name, model, operation, action string
		request                        map[string]any
		wantReferences                 []any
	}{
		{"E image is an ordinary reference", "seedance2.0-E", "create", "reference_to_video", map[string]any{"image": "https://cdn.example/image.png"}, []any{map[string]any{"type": "image", "role": "reference", "source": "https://cdn.example/image.png"}}},
		{"F typed references and false audio", "seedance2.5-F", "create", "reference_to_video", map[string]any{"generate_audio": false, "references": []any{map[string]any{"type": "video", "role": "reference", "source": "https://cdn.example/motion.mp4"}}}, []any{map[string]any{"type": "video", "role": "reference", "source": "https://cdn.example/motion.mp4"}}},
		{"G image and video references", "seedance2.5-G", "create", "reference_to_video", map[string]any{"reference_images": []any{"asset://asset-one"}, "reference_videos": []any{"https://cdn.example/motion.mp4"}}, []any{map[string]any{"type": "image", "role": "reference_image", "source": "asset://asset-one"}, map[string]any{"type": "video", "role": "reference_video", "source": "https://cdn.example/motion.mp4"}}},
		{"G first and last frame", "seedance2.5-G", "create", "first_last_frame", map[string]any{"image": "https://cdn.example/first.png", "last_frame": "https://cdn.example/last.png"}, []any{map[string]any{"type": "image", "role": "first_frame", "source": "https://cdn.example/first.png"}, map[string]any{"type": "image", "role": "last_frame", "source": "https://cdn.example/last.png"}}},
		{"G editing auto duration", "seedance2.5-G", "edit", "edit_video", map[string]any{"video": "https://cdn.example/source.mp4"}, []any{map[string]any{"type": "video", "role": "reference_video", "source": "https://cdn.example/source.mp4"}}},
		{"G extension", "seedance2.5-G", "extend", "extend_video", map[string]any{"video": "https://cdn.example/source.mp4", "extension_direction": "backward"}, []any{map[string]any{"type": "video", "role": "reference_video", "source": "https://cdn.example/source.mp4"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"prompt": "use the references"}
			if tc.operation != "edit" {
				request["seconds"] = 8
			}
			for key, value := range tc.request {
				request[key] = value
			}
			intent, err := decodeDramaVideoRequest(t, plugin, tc.model, tc.operation, request)
			require.NoError(t, err)
			assert.Equal(t, tc.action, intent["action"])
			profile, found := plugin.Meta.VideoProfileForModel(tc.model)
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, tc.action, intent["requestBody"], nil))
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"model": tc.model, "baseUrl": "https://upstream.example", "apiKey": "key", "action": intent["action"], "requestBody": intent["requestBody"],
			})
			require.NoError(t, err)
			body := value.(map[string]any)["body"].(map[string]any)
			encoded, err := common.Marshal(body["references"])
			require.NoError(t, err)
			var references []any
			require.NoError(t, common.Unmarshal(encoded, &references))
			assert.Equal(t, tc.wantReferences, references)
			if _, exists := tc.request["generate_audio"]; exists {
				assert.Equal(t, false, body["generate_audio"])
			}
			if tc.operation == "edit" {
				assert.EqualValues(t, -1, body["seconds"])
				assert.Equal(t, "edit", body["omni_reference_task_type"])
			}
		})
	}
	tooManyImages := make([]any, 10)
	for i := range tooManyImages {
		tooManyImages[i] = fmt.Sprintf("https://cdn.example/%d.png", i)
	}
	for _, tc := range []struct {
		name, model, errorText string
		request                map[string]any
	}{
		{"unsigned duration overflow", "seedance2.5-G", "seconds must be between", map[string]any{"seconds": "18446744073686646784"}},
		{"boolean duration", "seedance2.5-G", "seconds must be an integer", map[string]any{"seconds": true}},
		{"negative generation duration", "seedance2.5-G", "seconds must be between", map[string]any{"seconds": -1}},
		{"duration aliases differ", "seedance2.5-G", "must match", map[string]any{"seconds": 5, "duration": 8}},
		{"F needs 720px", "seedance2.0-F", "resolution is not supported", map[string]any{"resolution": "720p"}},
		{"fast resolution", "seedance2.0-fast-G", "resolution is not supported", map[string]any{"resolution": "1080p"}},
		{"E adaptive ratio", "seedance2.0-E", "aspect_ratio is not supported", map[string]any{"aspect_ratio": "adaptive"}},
		{"E first frame", "seedance2.0-E", "ordinary references", map[string]any{"references": []any{map[string]any{"type": "image", "role": "first_frame", "source": "https://cdn.example/first.png"}}}},
		{"legacy F video", "seedance2.0-F-720p", "media exceeds", map[string]any{"reference_videos": []any{"https://cdn.example/video.mp4"}}},
		{"F combined audio video limit", "seedance2.5-F", "media exceeds", map[string]any{"reference_videos": []any{"https://cdn.example/1.mp4", "https://cdn.example/2.mp4"}, "reference_audios": []any{"https://cdn.example/1.mp3", "https://cdn.example/2.mp3"}}},
		{"too many images", "seedance2.0-G", "media exceeds", map[string]any{"reference_images": tooManyImages}},
		{"duplicate media", "seedance2.0-E", "Duplicate reference", map[string]any{"image": "https://cdn.example/a.png", "reference_images": []any{"https://cdn.example/a.png"}}},
		{"missing G role", "seedance2.0-G", "explicit matching role", map[string]any{"references": []any{map[string]any{"type": "image", "source": "https://cdn.example/a.png"}}}},
		{"E HTTP media", "seedance2.0-E", "not supported", map[string]any{"image": "http://cdn.example/a.png"}},
		{"E asset reference", "seedance2.0-E", "not supported", map[string]any{"image": "asset://asset-one"}},
		{"last frame alone", "seedance2.5-G", "requires first_frame", map[string]any{"last_frame": "https://cdn.example/last.png"}},
		{"pinned and reference images", "seedance2.5-G", "cannot be mixed", map[string]any{"image": "https://cdn.example/first.png", "reference_images": []any{"https://cdn.example/other.png"}}},
		{"G 2.0 audio alone", "seedance2.0-G", "require an image or video", map[string]any{"reference_audios": []any{"https://cdn.example/a.mp3"}}},
		{"metadata duration bypass", "seedance2.5-G", "metadata.seconds is not supported", map[string]any{"metadata": map[string]any{"seconds": 99999999999}}},
		{"unknown billing multiplier", "seedance2.5-G", "n is not supported", map[string]any{"n": 99999999999}},
		{"E unsupported audio flag", "seedance2.0-E", "generate_audio is not supported", map[string]any{"generate_audio": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"prompt": "a cat", "seconds": 5}
			for key, value := range tc.request {
				request[key] = value
			}
			_, err := decodeDramaVideoRequest(t, plugin, tc.model, "create", request)
			require.ErrorContains(t, err, tc.errorText)
		})
	}
}

func TestDramaVideoPollingAndActualUsage(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	for _, tc := range []struct {
		name, model, action, wantStatus, errorText string
		state, body                                map[string]any
		wantFacts                                  map[string]any
	}{
		{name: "queued receipt", model: "seedance2.5-G", body: map[string]any{"status": "queued", "progress": 0}, wantStatus: "QUEUED", wantFacts: map[string]any{}},
		{name: "unknown status cannot resolve inherited properties", model: "seedance2.5-G", body: map[string]any{"status": "constructor"}, wantStatus: "UNKNOWN", wantFacts: map[string]any{}},
		{name: "failed task", model: "seedance2.5-G", body: map[string]any{"status": "failed", "error": "generation rejected"}, wantStatus: "FAILURE", wantFacts: map[string]any{}},
		{name: "E needs no seconds or tokens", model: "seedance2.0-E", body: map[string]any{"status": "completed", "metadata": map[string]any{"url": "/v1/videos/upstream/content"}}, wantStatus: "SUCCESS", wantFacts: map[string]any{}},
		{name: "F seconds from bill", model: "seedance2.5-F", body: map[string]any{"status": "completed", "metadata": map[string]any{"billing": map[string]any{"seconds": 8}}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"seconds": float64(8)}},
		{name: "F reference video actual tokens", model: "seedance2.5-F", state: map[string]any{"video_input": "video"}, body: map[string]any{"status": "completed", "seconds": "8", "metadata": map[string]any{"usage": map[string]any{"total_tokens": 600037, "output_tokens": 590000}, "billing": map[string]any{"settle_state": "settled"}}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"seconds": float64(8), "tokens": float64(600037)}},
		{name: "F final bill pending", model: "seedance2.5-F", state: map[string]any{"video_input": "video"}, body: map[string]any{"status": "completed", "metadata": map[string]any{"usage": map[string]any{"total_tokens": 42}, "billing": map[string]any{"settle_state": "pending"}}}, wantStatus: "IN_PROGRESS", wantFacts: map[string]any{}},
		{name: "F missing final tokens", model: "seedance2.5-F", state: map[string]any{"video_input": "video"}, body: map[string]any{"status": "completed"}, wantStatus: "IN_PROGRESS", wantFacts: map[string]any{}},
		{name: "G actual total tokens", model: "seedance2.0-G", body: map[string]any{"status": "completed", "seconds": 5, "usage": map[string]any{"total_tokens": 432900, "completion_tokens": 400000}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"seconds": float64(5), "tokens": float64(432900)}},
		{name: "G missing final tokens", model: "seedance2.5-G", body: map[string]any{"status": "completed", "progress": 100}, wantStatus: "IN_PROGRESS", wantFacts: map[string]any{}},
		{name: "G explicit zero usage", model: "seedance2.5-G", body: map[string]any{"status": "completed", "usage": map[string]any{"total_tokens": 0}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"tokens": float64(0)}},
		{name: "G edit echoes auto duration", model: "seedance2.5-G", action: "edit_video", body: map[string]any{"status": "completed", "seconds": "-1", "usage": map[string]any{"total_tokens": 12345}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"tokens": float64(12345)}},
		{name: "G extension retains requested seconds", model: "seedance2.5-G", action: "extend_video", body: map[string]any{"status": "completed", "seconds": 35, "usage": map[string]any{"total_tokens": 12345}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"tokens": float64(12345)}},
		{name: "token overflow", model: "seedance2.5-G", body: map[string]any{"status": "completed", "usage": map[string]any{"total_tokens": 2147483648}}, errorText: "Invalid upstream video token usage"},
		{name: "boolean tokens", model: "seedance2.5-G", body: map[string]any{"status": "completed", "usage": map[string]any{"total_tokens": true}}, errorText: "Invalid upstream video token usage"},
		{name: "duration overflow", model: "seedance2.5-F", body: map[string]any{"status": "completed", "seconds": "18446744073686646784"}, errorText: "Invalid upstream video duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := map[string]any{"model": tc.model, "state": tc.state, "action": tc.action}
			value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, tc.body)
			if tc.errorText != "" {
				require.ErrorContains(t, err, tc.errorText)
				return
			}
			require.NoError(t, err)
			result := value.(map[string]any)
			assert.Equal(t, tc.wantStatus, result["status"])
			if tc.name == "failed task" {
				assert.Equal(t, "generation rejected", result["reason"])
			}
			value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, result, tc.body)
			require.NoError(t, err)
			encoded, err := common.Marshal(value)
			require.NoError(t, err)
			var facts map[string]any
			require.NoError(t, common.Unmarshal(encoded, &facts))
			assert.Equal(t, tc.wantFacts, facts)
		})
	}
}

func TestDramaVideoHostDownloadAndFrozenBilling(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	adaptor := pluginadaptor.New(plugin)
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: "https://upstream.example", ApiKey: "upstream-test-key"}})
	task := &model.Task{
		TaskID: "task_public", Status: model.TaskStatusSuccess, Progress: "100%", SubmitTime: 10, FinishTime: 20,
		Properties:  model.Properties{OriginModelName: "my-video", UpstreamModelName: "seedance2.5-G"},
		PrivateData: model.TaskPrivateData{UpstreamTaskID: "task/private?one"},
		Data:        []byte(`{"id":"task/private?one","metadata":{"url":"https://untrusted.example/private.mp4","billing":{"quota":12345}}}`),
	}
	artifacts, err := adaptor.ListArtifacts(task)
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		descriptor, err := adaptor.BuildContentRequest(task, "video", relaychannel.TaskArtifactClientRequest{Method: method})
		require.NoError(t, err)
		assert.Equal(t, "https://upstream.example/v1/videos/task%2Fprivate%3Fone/content", descriptor.URL)
		assert.Equal(t, method, descriptor.Method)
		assert.Equal(t, "Bearer upstream-test-key", descriptor.Headers["Authorization"])
		assert.False(t, descriptor.Credentialless)
	}
	public, err := adaptor.ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	var rendered map[string]any
	require.NoError(t, common.Unmarshal(public, &rendered))
	assert.Equal(t, "task_public", rendered["id"])
	assert.Equal(t, "my-video", rendered["model"])
	assert.Equal(t, "completed", rendered["status"])
	assert.NotContains(t, string(public), "untrusted.example")
	assert.NotContains(t, string(public), "upstream-test-key")
	assert.NotContains(t, rendered, "metadata")
	task.Data = []byte(`{"status":"completed","last_frame_url":"https://cdn.example/last-frame.png"}`)
	artifacts, err = adaptor.ListArtifacts(task)
	require.NoError(t, err)
	require.Len(t, artifacts, 2)
	lastFrame, err := adaptor.BuildContentRequest(task, "last_frame", relaychannel.TaskArtifactClientRequest{Method: http.MethodGet})
	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example/last-frame.png", lastFrame.URL)
	assert.True(t, lastFrame.Credentialless)
	assert.Empty(t, lastFrame.Headers)

	for _, tc := range []struct {
		model, expression string
		request           map[string]any
		response          map[string]any
		wantQuota         int
	}{
		{"seedance2.0-E", `tier("video", u("requests") * 0.5)`, map[string]any{"prompt": "a cat", "seconds": 5}, map[string]any{"status": "completed"}, 250000},
		{"seedance2.0-E", `tier("video", u("requests") * 0.5)`, map[string]any{"prompt": "a cat", "seconds": 15}, map[string]any{"status": "completed"}, 250000},
		{"seedance2.5-F", `u("video_input") == "video" ? tier("tokens", u("tokens") * 10 / 1000000) : tier("seconds", u("seconds") * 0.1)`, map[string]any{"prompt": "a cat", "seconds": 8}, map[string]any{"status": "completed", "seconds": "8"}, 400000},
		{"seedance2.5-F", `u("video_input") == "video" ? tier("tokens", u("tokens") * 10 / 1000000) : tier("seconds", u("seconds") * 0.1)`, map[string]any{"prompt": "a cat", "seconds": 8, "reference_videos": []any{"https://cdn.example/motion.mp4"}}, map[string]any{"status": "completed", "metadata": map[string]any{"usage": map[string]any{"total_tokens": 600037}}}, 3000185},
		{"seedance2.5-G", `tier("tokens", u("tokens") * 10 / 1000000)`, map[string]any{"prompt": "a cat", "seconds": 5}, map[string]any{"status": "completed", "usage": map[string]any{"total_tokens": 432900}}, 2164500},
	} {
		t.Run(tc.model, func(t *testing.T) {
			intent, err := decodeDramaVideoRequest(t, plugin, tc.model, "create", tc.request)
			require.NoError(t, err)
			value, err := plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{"model": tc.model, "requestBody": intent["requestBody"], "action": intent["action"], "usagePurpose": "facts"})
			require.NoError(t, err)
			encoded, err := common.Marshal(value)
			require.NoError(t, err)
			var estimate map[string]any
			require.NoError(t, common.Unmarshal(encoded, &estimate))
			snapshot := &billingexpr.BillingSnapshot{ExprString: tc.expression, ExprHash: billingexpr.ExprHashString(tc.expression), ExprVersion: 1, TaskUsageBilling: true, GroupRatio: 1, QuotaPerUnit: 500000, UsageFacts: estimate}
			value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", map[string]any{"model": tc.model, "state": estimate}, map[string]any{}, tc.response)
			require.NoError(t, err)
			encoded, err = common.Marshal(value)
			require.NoError(t, err)
			var actual map[string]any
			require.NoError(t, common.Unmarshal(encoded, &actual))
			settled, _, err := service.EvaluateTaskCompletionUsage(snapshot, actual)
			require.NoError(t, err)
			assert.Equal(t, tc.wantQuota, settled.ActualQuotaAfterGroup)
			assert.Nil(t, settled.Clamp)
		})
	}
}

func TestDramaVideoHTTPTaskLifecycle(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	for _, modelName := range []string{"seedance2.0-E-720p", "seedance2.5-F", "seedance2.5-G"} {
		t.Run(modelName, func(t *testing.T) {
			var submits, polls, downloads atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(t, "Bearer upstream-key", request.Header.Get("Authorization"))
				if request.Method == http.MethodPost {
					submits.Add(1)
					var body map[string]any
					if !assert.NoError(t, common.DecodeJson(request.Body, &body)) {
						writer.WriteHeader(http.StatusBadRequest)
						return
					}
					assert.Equal(t, modelName, body["model"])
					assert.Equal(t, float64(5), body["seconds"])
					writer.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(writer, `{"id":"remote-task","status":"queued"}`)
					return
				}
				if request.URL.Path == "/v1/videos/remote-task/content" {
					downloads.Add(1)
					writer.Header().Set("Content-Type", "video/mp4")
					_, _ = io.WriteString(writer, "upstream-video-bytes")
					return
				}
				poll := polls.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				if poll == 1 {
					_, _ = io.WriteString(writer, `{"id":"remote-task","status":"in_progress","progress":40}`)
					return
				}
				_, _ = io.WriteString(writer, `{"id":"remote-task","status":"completed","progress":100,"seconds":5,"usage":{"total_tokens":12345},"metadata":{"url":"/v1/videos/remote-task/content"}}`)
			}))
			t.Cleanup(upstream.Close)
			intent, err := decodeDramaVideoRequest(t, plugin, modelName, "create", map[string]any{"prompt": "a cat", "seconds": 5})
			require.NoError(t, err)
			info := &relaycommon.RelayInfo{OriginModelName: modelName, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: modelName, ChannelBaseUrl: upstream.URL, ApiKey: "upstream-key"}, TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: intent["action"].(string)}}
			adaptor := pluginadaptor.New(plugin)
			adaptor.Init(info)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{}`))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Request.Header.Set("Authorization", "Bearer client-key-must-not-be-forwarded")
			ctx.Set("task_request", intent["requestBody"])
			require.Nil(t, adaptor.ValidateRequestAndSetAction(ctx, info))
			requestBody, err := adaptor.BuildRequestBody(ctx, info)
			require.NoError(t, err)
			response, err := adaptor.DoRequest(ctx, info, requestBody)
			require.NoError(t, err)
			receipt, taskErr := adaptor.ParseResponse(ctx, response, info)
			require.NoError(t, response.Body.Close())
			require.Nil(t, taskErr)
			require.NotNil(t, receipt)
			assert.Equal(t, "remote-task", receipt.UpstreamTaskID)
			require.NotEmpty(t, receipt.PluginState)
			task := &model.Task{TaskID: "public-task", Action: info.Action, Status: model.TaskStatusSubmitted, Properties: model.Properties{OriginModelName: modelName, UpstreamModelName: modelName}, Data: receipt.TaskData, PrivateData: model.TaskPrivateData{UpstreamTaskID: receipt.UpstreamTaskID, PluginState: receipt.PluginState}}
			for _, wantStatus := range []string{"IN_PROGRESS", "SUCCESS"} {
				response, err = adaptor.FetchTaskContext(t.Context(), upstream.URL, "upstream-key", task, "")
				require.NoError(t, err)
				body, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				result, err := adaptor.ParseTaskResult(task, response, body)
				require.NoError(t, err)
				assert.Equal(t, wantStatus, result.Status)
				task.Status = model.TaskStatus(result.Status)
				task.Data = body
				if wantStatus == "SUCCESS" && modelName == "seedance2.5-G" {
					assert.Equal(t, float64(12345), result.UsageFacts["tokens"])
				}
			}
			descriptor, err := adaptor.BuildContentRequest(task, "video", relaychannel.TaskArtifactClientRequest{Method: http.MethodGet})
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), descriptor.Method, descriptor.URL, nil)
			require.NoError(t, err)
			for key, value := range descriptor.Headers {
				request.Header.Set(key, value)
			}
			response, err = upstream.Client().Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			assert.Equal(t, "upstream-video-bytes", string(body))
			assert.EqualValues(t, 1, submits.Load())
			assert.EqualValues(t, 2, polls.Load())
			assert.EqualValues(t, 1, downloads.Load())
		})
	}
}
