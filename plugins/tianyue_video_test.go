package plugins_test

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay/channel"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadTianyueVideoPlugin(t *testing.T) *jsplugin.LoadedPlugin {
	t.Helper()
	source, err := builtinplugins.Source("tianyue")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "tianyue"})
	require.NoError(t, err)
	return plugin
}

func TestTianyueVideoRequestBillingAndReferences(t *testing.T) {
	plugin := loadTianyueVideoPlugin(t)
	assert.Equal(t, []string{"L-sd2-video"}, plugin.Meta.Models)
	assert.Empty(t, plugin.Meta.ChannelTypes, "Tianyue must not claim all NewAPI channels")
	for _, tc := range []struct {
		name         string
		upstream     string
		request      map[string]any
		wantDuration int
		wantAction   string
		wantUsage    map[string]any
	}{
		{name: "per call native fields", upstream: "L-sd2-video", request: map[string]any{"duration": 1, "video_duration": 6}, wantDuration: 1, wantAction: "text_to_video", wantUsage: map[string]any{"requests": float64(1), "resolution": "720p"}},
		{name: "per call public duration", upstream: "L-sd2-video", request: map[string]any{"duration": 6, "input_reference": "https://cdn.example/image.png"}, wantDuration: 1, wantAction: "image_to_video", wantUsage: map[string]any{"requests": float64(1), "resolution": "720p"}},
		// The -se name is a protocol fixture, not a claim that this model exists.
		{name: "mapped per second model", upstream: "protocol-fixture-se", request: map[string]any{"seconds": 6, "image_urls": []any{"https://cdn.example/1.png", "https://cdn.example/2.png"}, "audio_urls": []any{"https://cdn.example/reference.mp3"}, "video_urls": []any{"https://cdn.example/reference.mp4"}}, wantDuration: 6, wantAction: "reference_to_video", wantUsage: map[string]any{"seconds": float64(6), "resolution": "720p"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"model": "public-alias", "prompt": " retain reference identity ", "resolution": "720p", "aspect_ratio": "9:16"}
			for key, value := range tc.request {
				request[key] = value
			}
			value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": "public-alias", "upstreamModel": tc.upstream, "operation": "create", "body": map[string]any{"kind": "json", "value": request},
			})
			require.NoError(t, err)
			intent := value.(map[string]any)
			assert.Equal(t, "public-alias", intent["model"])
			assert.Equal(t, tc.wantAction, intent["action"])
			profile, found := plugin.Meta.VideoProfileForModel("L-sd2-video")
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, tc.wantAction, intent["requestBody"], nil))
			ctx := map[string]any{"model": "public-alias", "upstreamModel": tc.upstream, "baseUrl": "https://api.tianyue.example", "apiKey": "test-key", "requestBody": intent["requestBody"]}
			value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, err)
			descriptor := value.(map[string]any)
			assert.Equal(t, "https://api.tianyue.example/v1/videos", descriptor["url"])
			assert.Equal(t, "Bearer test-key", descriptor["headers"].(map[string]any)["Authorization"])
			body := descriptor["body"].(map[string]any)
			assert.Equal(t, tc.upstream, body["model"])
			assert.EqualValues(t, tc.wantDuration, body["duration"])
			assert.EqualValues(t, 6, body["video_duration"])
			assert.Equal(t, "9:16", body["aspect_ratio"])
			assert.NotContains(t, body, "seconds")
			if tc.wantAction == "image_to_video" {
				assert.Equal(t, []any{"https://cdn.example/image.png"}, body["image_urls"])
			}
			if tc.wantAction == "reference_to_video" {
				assert.Equal(t, request["image_urls"], body["image_urls"])
				assert.Equal(t, request["audio_urls"], body["audio_urls"])
				assert.Equal(t, request["video_urls"], body["video_urls"])
			}
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			facts := value.(map[string]any)
			assert.Len(t, facts, len(tc.wantUsage))
			for key, expected := range tc.wantUsage {
				assert.EqualValues(t, expected, facts[key], key)
			}
			ctx["usagePurpose"] = "billing_ratios"
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			if tc.wantDuration == 1 {
				assert.Nil(t, value, "a per-call price must not be multiplied by video seconds")
			} else {
				assert.EqualValues(t, 6, value.(map[string]any)["seconds"])
			}
		})
	}
}

func TestTianyueVideoRejectsUnsafeInputsBeforeSubmission(t *testing.T) {
	plugin := loadTianyueVideoPlugin(t)
	for _, tc := range []struct {
		name   string
		fields map[string]any
	}{
		{name: "missing actual duration", fields: map[string]any{"seconds": nil}},
		{name: "zero duration", fields: map[string]any{"seconds": 0}},
		{name: "negative duration", fields: map[string]any{"seconds": -1}},
		{name: "fractional duration", fields: map[string]any{"seconds": 1.5}},
		{name: "duration overflow", fields: map[string]any{"seconds": "18446744073686646784"}},
		{name: "host duration limit", fields: map[string]any{"seconds": relaycommon.MaxTaskDurationSeconds + 1}},
		{name: "conflicting aliases", fields: map[string]any{"duration": 7}},
		{name: "per call overcharge", fields: map[string]any{"duration": 6, "video_duration": 6}},
		{name: "metadata multiplier bypass", fields: map[string]any{"metadata": map[string]any{"duration": 9000}}},
		{name: "unsupported resolution", fields: map[string]any{"resolution": "4k"}},
		{name: "inline reference", fields: map[string]any{"image": "data:image/png;base64,YQ=="}},
		{name: "private reference", fields: map[string]any{"image_urls": []any{"http://127.0.0.1/image.png"}}},
		{name: "credentialed reference", fields: map[string]any{"audio_urls": []any{"https://user:secret@cdn.example/a.mp3"}}},
		{name: "array type", fields: map[string]any{"video_urls": "https://cdn.example/a.mp4"}},
		{name: "aggregate reference cap", fields: map[string]any{"image_urls": make([]any, 65)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"prompt": "a video", "seconds": 6, "resolution": "720p"}
			for key, value := range tc.fields {
				request[key] = value
			}
			_, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": "L-sd2-video", "operation": "create", "body": map[string]any{"kind": "json", "value": request},
			})
			require.Error(t, err)
		})
	}
}

func TestTianyueVideoLifecycleUsesPublicResultAndBoundedUsage(t *testing.T) {
	plugin := loadTianyueVideoPlugin(t)
	ctx := map[string]any{"model": "L-sd2-video", "requestBody": map[string]any{"prompt": "a video", "seconds": 6, "resolution": "720p"}}
	value, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": map[string]any{"id": "private/id", "status": "queued"}})
	require.NoError(t, err)
	assert.Equal(t, "private/id", value.(map[string]any)["taskId"])
	assert.EqualValues(t, 6, value.(map[string]any)["state"].(map[string]any)["requestedSeconds"])
	value, err = plugin.Engine.Call(t.Context(), "buildQueryRequest", map[string]any{"taskId": "private/id", "baseUrl": "https://api.tianyue.example", "apiKey": "test-key"})
	require.NoError(t, err)
	assert.Equal(t, "https://api.tianyue.example/v1/videos/private%2Fid", value.(map[string]any)["url"])
	for _, tc := range []struct{ status, want string }{{"queued", "QUEUED"}, {"in_progress", "IN_PROGRESS"}, {"failed", "FAILURE"}, {"unknown", "UNKNOWN"}, {"toString", "UNKNOWN"}} {
		value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, map[string]any{"status": tc.status, "progress": 50})
		require.NoError(t, err)
		assert.Equal(t, tc.want, value.(map[string]any)["status"])
	}
	for _, data := range []map[string]any{
		{"status": "completed", "url": "https://cdn.example/result.mp4"},
		{"status": "completed", "video_url": "https://cdn.example/result.mp4"},
		{"status": "completed", "metadata": map[string]any{"url": "https://cdn.example/result.mp4"}},
	} {
		value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, data)
		require.NoError(t, err)
		assert.Equal(t, "SUCCESS", value.(map[string]any)["status"])
		assert.Equal(t, "https://cdn.example/result.mp4", value.(map[string]any)["url"])
	}
	_, err = plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, map[string]any{"status": "completed"})
	require.ErrorContains(t, err, "result URL is missing")
	ctx["upstreamModel"], ctx["state"] = "protocol-fixture-se", map[string]any{"requestedSeconds": 6}
	for _, tc := range []struct {
		seconds any
		want    string
	}{
		{seconds: "6", want: "SUCCESS"}, {seconds: "7", want: "FAILURE"},
	} {
		value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, map[string]any{"status": "completed", "seconds": tc.seconds, "url": "https://cdn.example/result.mp4"})
		require.NoError(t, err)
		assert.Equal(t, tc.want, value.(map[string]any)["status"])
	}
	value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{"status": "SUCCESS"}, map[string]any{"status": "completed", "seconds": "6"})
	require.NoError(t, err)
	assert.EqualValues(t, 6, value.(map[string]any)["seconds"])
	_, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{"status": "SUCCESS"}, map[string]any{"status": "completed", "seconds": "18446744073686646784"})
	require.Error(t, err)

	adaptor := taskplugin.New(plugin)
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "must-not-be-forwarded", ChannelBaseUrl: "https://api.tianyue.example"}})
	data, err := common.Marshal(map[string]any{"status": "completed", "video_url": "https://cdn.example/result.mp4"})
	require.NoError(t, err)
	task := &model.Task{TaskID: "public-id", Status: model.TaskStatusSuccess, Data: data, PrivateData: model.TaskPrivateData{UpstreamTaskID: "private-id"}}
	artifacts, err := adaptor.ListArtifacts(task)
	require.NoError(t, err)
	assert.Equal(t, []channel.TaskArtifact{{Key: "video", Type: "video", MimeType: "video/mp4"}}, artifacts)
	descriptor, err := adaptor.BuildContentRequest(task, "video", channel.TaskArtifactClientRequest{Method: http.MethodGet})
	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example/result.mp4", descriptor.URL)
	assert.True(t, descriptor.Credentialless)
	assert.Empty(t, descriptor.Headers)
	value, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "render"}, map[string]any{}, map[string]any{"data": map[string]any{"video_url": "must-not-leak"}})
	require.NoError(t, err)
	assert.Empty(t, value)
}
