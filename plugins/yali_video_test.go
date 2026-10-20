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

func loadYaliVideoPlugin(t *testing.T) *jsplugin.LoadedPlugin {
	t.Helper()
	source, err := builtinplugins.Source("yali")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "yali"})
	require.NoError(t, err)
	return plugin
}

func decodeYaliVideoRequest(t *testing.T, plugin *jsplugin.LoadedPlugin, request map[string]any) (map[string]any, error) {
	t.Helper()
	value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"model": "minimax-h3-720p", "operation": "create", "body": map[string]any{"kind": "json", "value": request},
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

func TestYaliVideoRequestMappingAndBillingIdentity(t *testing.T) {
	plugin := loadYaliVideoPlugin(t)
	request := map[string]any{
		"model": "minimax-h3-720p", "prompt": " keep the subject ", "duration": "15", "size": "720P", "ratio": "9:16",
		"images":   []any{"https://cdn.example/scene.png"},
		"metadata": map[string]any{"reference_audio": []any{"https://cdn.example/reference.mp3"}, "customer_note": "preserved by the client"},
	}
	intent, err := decodeYaliVideoRequest(t, plugin, request)
	require.NoError(t, err)
	assert.Equal(t, "minimax-h3-720p", intent["model"])
	assert.Equal(t, "reference_to_video", intent["action"])
	normalized := intent["requestBody"].(map[string]any)
	assert.Equal(t, []any{"https://cdn.example/scene.png"}, normalized["reference_images"])
	assert.Equal(t, []any{"https://cdn.example/reference.mp3"}, normalized["reference_audios"])
	assert.NotContains(t, normalized, "metadata")
	assert.Equal(t, " keep the subject ", request["prompt"], "decoding must not mutate the original request")
	profile, found := plugin.Meta.VideoProfileForModel("minimax-h3-720p")
	require.True(t, found)
	require.NoError(t, service.ValidateVideoProfileRequest(profile, intent["action"].(string), normalized, nil))

	ctx := map[string]any{
		"model": "minimax-h3-720p", "upstreamModel": "MiniMaxH3-720p-realistic", "requestBody": normalized,
		"baseUrl": "https://api.yaliai.example", "apiKey": "test-key",
	}
	value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
	require.NoError(t, err)
	descriptor := value.(map[string]any)
	assert.Equal(t, "https://api.yaliai.example/v1/videos", descriptor["url"])
	assert.Equal(t, "Bearer test-key", descriptor["headers"].(map[string]any)["Authorization"])
	body := descriptor["body"].(map[string]any)
	assert.Equal(t, "MiniMaxH3-720p-realistic", body["model"])
	assert.Equal(t, "768P", body["resolution"], "the provider uses 768P for its medium-resolution protocol enum")
	assert.EqualValues(t, 15, body["seconds"])
	assert.Equal(t, "9:16", body["aspect_ratio"])
	assert.Equal(t, []any{"https://cdn.example/scene.png"}, body["images"])
	assert.Equal(t, map[string]any{"reference_audio": []any{"https://cdn.example/reference.mp3"}}, body["metadata"])
	assert.NotContains(t, descriptor, "model", "the descriptor must not overwrite the public model identity")
	value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
	require.NoError(t, err)
	facts := value.(map[string]any)
	assert.EqualValues(t, 1, facts["requests"])
	assert.Equal(t, "720p", facts["resolution"], "the provider enum must not change the public billing resolution")
	schema, _ := plugin.Meta.UsageForModel("MiniMaxH3-720p-realistic")
	assert.Contains(t, schema, "requests")
	assert.Contains(t, schema["resolution"].Enum, "720p")
	assert.NotContains(t, plugin.Meta.Models, "minimax-h3-2k", "the lower-resolution provider must not join the existing 2K model")
}

func TestYaliVideoRequestValidation(t *testing.T) {
	plugin := loadYaliVideoPlugin(t)
	for _, tc := range []struct {
		name    string
		fields  map[string]any
		wantErr string
	}{
		{name: "text only", fields: map[string]any{}},
		{name: "structured references", fields: map[string]any{"references": []any{map[string]any{"type": "image", "source": "https://cdn.example/image.png"}, map[string]any{"type": "audio", "role": "reference_audio", "source": "https://cdn.example/audio.mp3"}}}},
		{name: "duration zero", fields: map[string]any{"seconds": 0}, wantErr: "seconds must be 15"},
		{name: "duration overflow", fields: map[string]any{"seconds": "18446744073709551615"}, wantErr: "seconds must be 15"},
		{name: "duration boolean", fields: map[string]any{"seconds": true}, wantErr: "seconds must be 15"},
		{name: "conflicting duration", fields: map[string]any{"seconds": 15, "duration": 14}, wantErr: "must match"},
		{name: "reject 2K quality", fields: map[string]any{"resolution": "2K"}, wantErr: "resolution must be 720p"},
		{name: "conflicting resolution", fields: map[string]any{"resolution": "2K", "size": "720p"}, wantErr: "must match"},
		{name: "unsupported video", fields: map[string]any{"reference_videos": []any{"https://cdn.example/video.mp4"}}, wantErr: "not supported"},
		{name: "metadata video bypass", fields: map[string]any{"metadata": map[string]any{"reference_video": []any{"https://cdn.example/video.mp4"}}}, wantErr: "not supported"},
		{name: "official content bypass", fields: map[string]any{"metadata": map[string]any{"content": []any{}}}, wantErr: "not supported"},
		{name: "unsupported face effect", fields: map[string]any{"face": map[string]any{"enabled": true, "mode": "heavy"}}, wantErr: "face is not supported"},
		{name: "unsupported metadata face effect", fields: map[string]any{"metadata": map[string]any{"face": map[string]any{"enabled": true, "mode": "heavy"}}}, wantErr: "metadata.face is not supported"},
		{name: "unsupported task mode", fields: map[string]any{"task_mode": "references"}, wantErr: "task_mode is not supported"},
		{name: "unknown business field", fields: map[string]any{"vendor_effect": "cinematic"}, wantErr: "vendor_effect is not supported"},
		{name: "metadata image alias", fields: map[string]any{"metadata": map[string]any{"image_urls": []any{"https://cdn.example/image.png"}}}, wantErr: "metadata.image_urls is not supported"},
		{name: "metadata duration override", fields: map[string]any{"metadata": map[string]any{"seconds": 5}}, wantErr: "metadata.seconds is not supported"},
		{name: "malformed image list", fields: map[string]any{"images": "https://cdn.example/image.png"}, wantErr: "media limit"},
		{name: "aggregate image bound", fields: map[string]any{"images": []any{"https://cdn.example/1", "https://cdn.example/2", "https://cdn.example/3", "https://cdn.example/4", "https://cdn.example/5"}, "reference_images": []any{"https://cdn.example/6", "https://cdn.example/7", "https://cdn.example/8", "https://cdn.example/9", "https://cdn.example/10"}}, wantErr: "at most 9"},
		{name: "audio bound", fields: map[string]any{"reference_audios": []any{"https://cdn.example/1", "https://cdn.example/2", "https://cdn.example/3", "https://cdn.example/4"}}, wantErr: "media limit"},
		{name: "private image", fields: map[string]any{"images": []any{"http://127.0.0.1/image"}}, wantErr: "public hosts"},
		{name: "credential image", fields: map[string]any{"images": []any{"https://user:password@cdn.example/image"}}, wantErr: "without credentials"},
		{name: "non URL image", fields: map[string]any{"images": []any{"data:image/png;base64,aaaa"}}, wantErr: "HTTP(S)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"prompt": "waves"}
			for key, value := range tc.fields {
				request[key] = value
			}
			intent, err := decodeYaliVideoRequest(t, plugin, request)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			profile, found := plugin.Meta.VideoProfileForModel("minimax-h3-720p")
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, intent["action"].(string), intent["requestBody"], nil))
		})
	}
}

func TestYaliVideoMediaAliasesKeepReferences(t *testing.T) {
	plugin := loadYaliVideoPlugin(t)
	for _, tc := range []struct {
		name string
		kind string
	}{
		{"reference_images", "image"}, {"images", "image"}, {"image_urls", "image"}, {"referenceImages", "image"},
		{"reference_audios", "audio"}, {"audios", "audio"}, {"audio_urls", "audio"}, {"referenceAudios", "audio"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mediaURL := "https://cdn.example/" + tc.kind + ".media"
			intent, err := decodeYaliVideoRequest(t, plugin, map[string]any{"prompt": "keep the references", tc.name: []any{mediaURL}})
			require.NoError(t, err)
			assert.Equal(t, "reference_to_video", intent["action"])
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"model": "minimax-h3-720p", "upstreamModel": "MiniMaxH3-720p-realistic", "requestBody": intent["requestBody"],
				"baseUrl": "https://api.yaliai.example", "apiKey": "test-key",
			})
			require.NoError(t, err)
			body := value.(map[string]any)["body"].(map[string]any)
			if tc.kind == "image" {
				assert.Equal(t, []any{mediaURL}, body["images"])
				return
			}
			assert.Equal(t, []any{mediaURL}, body["metadata"].(map[string]any)["reference_audio"])
		})
	}
	intent, err := decodeYaliVideoRequest(t, plugin, map[string]any{
		"prompt":           "preserve Drama's material order",
		"reference_images": []any{"https://cdn.example/1.png"}, "images": []any{"https://cdn.example/2.png"},
		"image_urls": []any{"https://cdn.example/3.png"}, "referenceImages": []any{"https://cdn.example/4.png"}, "image": "https://cdn.example/5.png",
	})
	require.NoError(t, err)
	assert.Equal(t, []any{"https://cdn.example/1.png", "https://cdn.example/2.png", "https://cdn.example/3.png", "https://cdn.example/4.png", "https://cdn.example/5.png"}, intent["requestBody"].(map[string]any)["reference_images"])
}

func TestYaliVideoTaskLifecycleAndAuthenticatedContent(t *testing.T) {
	plugin := loadYaliVideoPlugin(t)
	for _, tc := range []struct {
		name    string
		body    map[string]any
		wantErr string
	}{
		{name: "accepted", body: map[string]any{"id": "yali-private", "status": "queued"}},
		{name: "accepted with empty error object", body: map[string]any{"id": "yali-private", "status": "queued", "error": map[string]any{}}},
		{name: "missing id", body: map[string]any{"task_id": "wrong-field"}, wantErr: "task id is missing"},
		{name: "non string id", body: map[string]any{"id": 123}, wantErr: "task id is missing"},
		{name: "rejected", body: map[string]any{"id": "rejected", "status": "failed", "error": map[string]any{"message": "quota exhausted"}}, wantErr: "quota exhausted"},
		{name: "coded error", body: map[string]any{"id": "rejected", "error": map[string]any{"code": "quota_exceeded"}}, wantErr: "quota_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", map[string]any{}, map[string]any{"body": tc.body})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "yali-private", value.(map[string]any)["taskId"])
		})
	}
	for _, tc := range []struct {
		status string
		want   string
	}{
		{"queued", "QUEUED"}, {"in_progress", "IN_PROGRESS"}, {"completed", "SUCCESS"}, {"failed", "FAILURE"}, {"surprise", "UNKNOWN"}, {"toString", "UNKNOWN"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{}, map[string]any{"status": tc.status})
			require.NoError(t, err)
			assert.Equal(t, tc.want, value.(map[string]any)["status"])
		})
	}
	value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{}, map[string]any{"error": map[string]any{"message": "provider rejected the task"}})
	require.NoError(t, err)
	assert.Equal(t, "FAILURE", value.(map[string]any)["status"])
	assert.Equal(t, "provider rejected the task", value.(map[string]any)["reason"])
	for _, emptyError := range []any{map[string]any{}, nil, "", map[string]any{"message": "", "code": 0}} {
		value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{}, map[string]any{"status": "completed", "error": emptyError})
		require.NoError(t, err)
		assert.Equal(t, "SUCCESS", value.(map[string]any)["status"], "empty upstream errors are present in real successful responses")
	}
	value, err = plugin.Engine.Call(t.Context(), "buildQueryRequest", map[string]any{"taskId": "private/id?query", "baseUrl": "https://api.yaliai.example", "apiKey": "test-key"})
	require.NoError(t, err)
	assert.Equal(t, "https://api.yaliai.example/v1/videos/private%2Fid%3Fquery", value.(map[string]any)["url"])
	assert.Equal(t, "Bearer test-key", value.(map[string]any)["headers"].(map[string]any)["Authorization"])

	adaptor := taskplugin.New(plugin)
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "test-key", ChannelBaseUrl: "https://api.yaliai.example"}})
	data, err := common.Marshal(map[string]any{"id": "private/id?query", "status": "completed", "video": map[string]any{"url": "/v1/videos/private/content"}})
	require.NoError(t, err)
	task := &model.Task{TaskID: "public-id", Status: model.TaskStatusSuccess, Data: data, PrivateData: model.TaskPrivateData{UpstreamTaskID: "private/id?query"}}
	artifacts, err := adaptor.ListArtifacts(task)
	require.NoError(t, err)
	assert.Equal(t, []channel.TaskArtifact{{Key: "video", Type: "video", MimeType: "video/mp4"}}, artifacts)
	descriptor, err := adaptor.BuildContentRequest(task, "video", channel.TaskArtifactClientRequest{Method: http.MethodHead})
	require.NoError(t, err)
	require.NotNil(t, descriptor)
	assert.Equal(t, "https://api.yaliai.example/v1/videos/private%2Fid%3Fquery/content", descriptor.URL)
	assert.Equal(t, http.MethodHead, descriptor.Method)
	assert.Equal(t, "Bearer test-key", descriptor.Headers["Authorization"])
	assert.False(t, descriptor.Credentialless)
}
