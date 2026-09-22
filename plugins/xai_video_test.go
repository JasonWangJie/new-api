package plugins_test

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXAIVideoPlugin(t *testing.T) {
	source, err := builtinplugins.Source("xai")
	require.NoError(t, err)
	registry := jsplugin.NewRegistry()
	plugin, err := registry.RegisterFactory(source, jsplugin.Options{Key: "xai"})
	require.NoError(t, err)

	object := func(t *testing.T, value any) map[string]any {
		t.Helper()
		encoded, marshalErr := common.Marshal(value)
		require.NoError(t, marshalErr)
		var decoded map[string]any
		require.NoError(t, common.Unmarshal(encoded, &decoded))
		return decoded
	}
	decode := func(t *testing.T, model string, request map[string]any) (map[string]any, error) {
		t.Helper()
		value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": model,
			"body":  map[string]any{"kind": "json", "value": request},
		})
		if callErr != nil {
			return nil, callErr
		}
		return object(t, value), nil
	}

	for _, model := range []string{"grok-imagine-video-1.5", "grok-imagine-video-1.5-preview", "grok-imagine-video-1.5-2026-05-30", "grok-imagine-video"} {
		profile, found := plugin.Meta.VideoProfileForModel(model)
		require.True(t, found, model)
		assert.NotEmpty(t, profile.Modes)
		binding, claimed := registry.Generation().LookupEndpoint("POST", "/v1/videos", model)
		require.True(t, claimed, model)
		assert.Same(t, plugin, binding.Plugin)
	}
	for _, path := range []string{"/v1/videos/edits", "/v1/videos/extensions"} {
		binding, claimed := registry.Generation().LookupEndpoint("POST", path, "grok-imagine-video")
		require.True(t, claimed, path)
		assert.Same(t, plugin, binding.Plugin)
		_, claimed = registry.Generation().LookupEndpoint("POST", path, "grok-imagine-video-1.5")
		assert.False(t, claimed, path)
	}

	for _, testCase := range []struct {
		name, action string
		request      map[string]any
	}{
		{name: "text", action: "text_to_video", request: map[string]any{"prompt": "sunrise", "duration": 15, "resolution": "1080p"}},
		{name: "image", action: "image_to_video", request: map[string]any{"image": map[string]any{"file_id": "file-first"}, "duration": 5}},
		{name: "references", action: "reference_to_video", request: map[string]any{
			"prompt": "use the references", "resolution": "720p",
			"reference_images": []any{map[string]any{"url": "https://cdn.example/1.png"}, "data:image/png;base64,AA=="},
			"reference_audios": []any{"eve", map[string]any{"voice_id": "leo"}},
		}},
		{name: "first and last", action: "first_last_frame", request: map[string]any{
			"image": "https://cdn.example/first.png", "last_frame": map[string]any{"file_id": "file-last"}, "resolution": "720p",
		}},
		{name: "last frame only", action: "first_last_frame", request: map[string]any{
			"last_frame": "https://cdn.example/last.png", "resolution": "720p",
		}},
		{name: "pinned reference combination", action: "reference_to_video", request: map[string]any{
			"image": "https://cdn.example/first.png", "last_frame": "https://cdn.example/last.png",
			"reference_images": []any{"https://cdn.example/ref.png"}, "reference_audios": []any{"eve"}, "resolution": "720p",
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resolved, callErr := decode(t, "grok-imagine-video-1.5", testCase.request)
			require.NoError(t, callErr)
			assert.Equal(t, testCase.action, resolved["action"])
			requestBody := resolved["requestBody"].(map[string]any)
			assert.Equal(t, "grok-imagine-video-1.5", requestBody["model"])
			assert.Contains(t, []any{float64(5), float64(15)}, requestBody["duration"])
		})
	}

	t.Run("editing and extension use dedicated upstream operations", func(t *testing.T) {
		callOperation := func(operation string, request map[string]any) (map[string]any, error) {
			value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"operation": operation, "model": "grok-imagine-video", "body": map[string]any{"kind": "json", "value": request},
			})
			if callErr != nil {
				return nil, callErr
			}
			return object(t, value), nil
		}
		for _, video := range []any{
			"https://cdn.example/source.mp4?signature=one",
			"data:video/mp4;base64,AA==",
			map[string]any{"file_id": "file-video"},
		} {
			resolved, callErr := callOperation("edit", map[string]any{"prompt": "restyle", "video": video})
			require.NoError(t, callErr)
			assert.Equal(t, "edit_video", resolved["action"])
			value, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"model": "grok-imagine-video", "upstreamModel": "grok-imagine-video", "baseUrl": "https://api.x.ai", "apiKey": "secret",
				"action": resolved["action"], "requestBody": resolved["requestBody"],
			})
			require.NoError(t, callErr)
			assert.Equal(t, "https://api.x.ai/v1/videos/edits", object(t, value)["url"])
		}

		resolved, callErr := callOperation("extend", map[string]any{
			"prompt": "continue", "video": map[string]any{"file_id": "file-video"}, "seconds": 10,
		})
		require.NoError(t, callErr)
		assert.Equal(t, "extend_video", resolved["action"])
		requestBody := resolved["requestBody"].(map[string]any)
		assert.Equal(t, float64(10), requestBody["duration"])
		assert.NotContains(t, requestBody, "extension_direction")
		value, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
			"model": "grok-imagine-video", "upstreamModel": "grok-imagine-video", "baseUrl": "https://api.x.ai", "apiKey": "secret",
			"action": resolved["action"], "requestBody": requestBody,
		})
		require.NoError(t, callErr)
		assert.Equal(t, "https://api.x.ai/v1/videos/extensions", object(t, value)["url"])

		_, callErr = callOperation("extend", map[string]any{"prompt": "continue", "video": "https://cdn.example/source.mp4", "duration": 1})
		require.ErrorContains(t, callErr, "between 2 and 10")
		_, callErr = callOperation("extend", map[string]any{"prompt": "continue", "video": "https://cdn.example/source.mp4", "duration": 2})
		require.NoError(t, callErr)
		_, callErr = callOperation("extend", map[string]any{"prompt": "continue", "video": "https://cdn.example/source.mp4", "extension_direction": "forward"})
		require.ErrorContains(t, callErr, "only backward")
		_, callErr = callOperation("edit", map[string]any{"prompt": "restyle", "video": "https://cdn.example/source.mov"})
		require.ErrorContains(t, callErr, "MP4")
		_, callErr = callOperation("edit", map[string]any{"prompt": "restyle", "video": "https://cdn.example/source.mp4", "duration": 5})
		require.ErrorContains(t, callErr, "not supported")
		_, callErr = callOperation("edit", map[string]any{"prompt": "restyle", "video": "https://cdn.example/source.mp4", "action": "extend_video"})
		require.ErrorContains(t, callErr, "action is not supported")
		_, callErr = callOperation("edit", map[string]any{"prompt": "restyle", "video": "https://cdn.example/source.mp4", "source_task_id": "old-task"})
		require.ErrorContains(t, callErr, "source_task_id is not supported")
		_, callErr = callOperation("edit", map[string]any{
			"prompt": "restyle", "video": "https://cdn.example/source.mp4",
			"metadata": map[string]any{"video": "https://cdn.example/override.mp4"},
		})
		require.ErrorContains(t, callErr, "metadata.video cannot override")
		_, callErr = callOperation("extend", map[string]any{
			"prompt": "continue", "video": "https://cdn.example/source.mp4",
			"metadata": map[string]any{"extension_direction": "forward"},
		})
		require.ErrorContains(t, callErr, "metadata.extension_direction cannot override")

		_, callErr = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"operation": "edit", "model": "grok-imagine-video-1.5", "body": map[string]any{"kind": "json", "value": map[string]any{"prompt": "restyle", "video": "https://cdn.example/source.mp4"}},
		})
		require.ErrorContains(t, callErr, "require grok-imagine-video")

		value, callErr = plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{"action": "edit_video", "requestBody": map[string]any{}})
		require.NoError(t, callErr)
		assert.Equal(t, float64(8.7), object(t, value)["input_video_seconds"])
		value, callErr = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", map[string]any{}, map[string]any{}, map[string]any{
			"usage": map[string]any{"cost_in_usd_ticks": 12300000000},
		})
		require.NoError(t, callErr)
		facts := object(t, value)
		assert.Equal(t, "upstream", facts["billing_source"])
		assert.InDelta(t, 1.23, facts["upstream_cost_credit"], 1e-12)
		value, callErr = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", map[string]any{"action": "extend_video"}, map[string]any{}, map[string]any{
			"video": map[string]any{"duration": 12},
		})
		require.NoError(t, callErr)
		assert.NotContains(t, object(t, value), "seconds", "the final extension duration includes the source clip and must not replace the requested extension seconds")
		value, callErr = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", map[string]any{"action": "edit_video"}, map[string]any{}, map[string]any{
			"video": map[string]any{"duration": 8.7},
		})
		require.NoError(t, callErr)
		assert.Equal(t, float64(8.7), object(t, value)["seconds"])
	})

	for _, testCase := range []struct {
		name, model, errorText string
		request                map[string]any
	}{
		{name: "duration boundary", model: "grok-imagine-video-1.5", request: map[string]any{"prompt": "bad", "duration": 16}, errorText: "between 1 and 15"},
		{name: "invalid resolution", model: "grok-imagine-video-1.5", request: map[string]any{"prompt": "bad", "resolution": "ultra"}, errorText: "resolution must"},
		{name: "reference count", model: "grok-imagine-video-1.5", request: map[string]any{"prompt": "bad", "reference_images": []any{1, 2, 3, 4, 5, 6, 7, 8}}, errorText: "at most 7"},
		{name: "reference resolution", model: "grok-imagine-video-1.5", request: map[string]any{"reference_images": []any{"https://cdn.example/1.png"}, "resolution": "1080p"}, errorText: "at most 720p"},
		{name: "classic last frame", model: "grok-imagine-video", request: map[string]any{"last_frame": "https://cdn.example/last.png"}, errorText: "require grok-imagine-video-1.5"},
		{name: "classic image and references conflict", model: "grok-imagine-video", request: map[string]any{"image": "https://cdn.example/first.png", "reference_images": []any{"https://cdn.example/ref.png"}}, errorText: "require grok-imagine-video-1.5"},
		{name: "unsupported videos", model: "grok-imagine-video-1.5", request: map[string]any{"prompt": "bad", "reference_videos": []any{"https://cdn.example/video.mp4"}}, errorText: "does not support reference_videos"},
		{name: "unsupported source video", model: "grok-imagine-video-1.5", request: map[string]any{"prompt": "bad", "video": "https://cdn.example/video.mp4"}, errorText: "video is not supported"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, callErr := decode(t, testCase.model, testCase.request)
			require.ErrorContains(t, callErr, testCase.errorText)
		})
	}

	t.Run("multipart images become distinct data URI placeholders", func(t *testing.T) {
		files := []any{
			map[string]any{"field": "reference_images", "ref": "request_file:reference_images", "mimeType": "image/png"},
			map[string]any{"field": "reference_images", "ref": "request_file:reference_images:1", "mimeType": "image/jpeg"},
		}
		value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "grok-imagine-video-1.5",
			"body": map[string]any{"kind": "multipart", "fields": map[string]any{
				"prompt": []any{"use both"}, "resolution": []any{"720p"},
			}, "files": files},
		})
		require.NoError(t, callErr)
		resolved := object(t, value)
		assert.Equal(t, "reference_to_video", resolved["action"])
		requestBody := resolved["requestBody"].(map[string]any)
		value, callErr = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
			"model": "grok-imagine-video-1.5", "upstreamModel": "grok-imagine-video-1.5", "baseUrl": "https://api.x.ai", "apiKey": "secret",
			"action": resolved["action"], "requestBody": requestBody, "files": files,
		})
		require.NoError(t, callErr)
		descriptor := object(t, value)
		body := descriptor["body"].(map[string]any)
		references := body["reference_images"].([]any)
		require.Len(t, references, 2)
		first := references[0].(map[string]any)["url"].(map[string]any)
		second := references[1].(map[string]any)["url"].(map[string]any)
		assert.Equal(t, "request_file:reference_images", first["__fileRef"])
		assert.Equal(t, "request_file:reference_images:1", second["__fileRef"])
	})

	t.Run("polling artifacts and usage facts", func(t *testing.T) {
		for _, testCase := range []struct {
			status, expected string
		}{
			{status: "pending", expected: "QUEUED"},
			{status: "done", expected: "SUCCESS"},
			{status: "failed", expected: "FAILURE"},
			{status: "expired", expected: "FAILURE"},
		} {
			value, callErr := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{}, map[string]any{
				"status": testCase.status, "video": map[string]any{"url": "https://vidgen.x.ai/video.mp4", "duration": 8}, "progress": 100,
			})
			require.NoError(t, callErr)
			assert.Equal(t, testCase.expected, object(t, value)["status"])
		}

		value, callErr := plugin.Engine.Call(t.Context(), "listArtifacts", map[string]any{
			"status": "SUCCESS", "data": map[string]any{"status": "done", "video": map[string]any{"url": "https://vidgen.x.ai/video.mp4"}},
		})
		require.NoError(t, callErr)
		artifacts, ok := value.([]any)
		require.True(t, ok)
		require.Len(t, artifacts, 1)

		value, callErr = plugin.Engine.Call(t.Context(), "buildContentRequest", map[string]any{
			"artifactKey": "video", "clientRequest": map[string]any{"method": "GET"},
			"data": map[string]any{"status": "done", "video": map[string]any{"url": "https://vidgen.x.ai/video.mp4"}},
		})
		require.NoError(t, callErr)
		descriptor := object(t, value)
		assert.Equal(t, "https://vidgen.x.ai/video.mp4", descriptor["url"])
		assert.Equal(t, true, descriptor["credentialless"])

		value, callErr = plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{"requestBody": map[string]any{
			"duration": 8, "resolution": "720p", "image": map[string]any{"url": "https://cdn.example/first.png"},
			"last_frame": map[string]any{"url": "https://cdn.example/last.png"}, "reference_images": []any{map[string]any{"url": "https://cdn.example/ref.png"}},
		}})
		require.NoError(t, callErr)
		assert.Equal(t, map[string]any{
			"seconds": float64(8), "resolution": "720p", "input_images": float64(3),
			"input_video_seconds": float64(0), "billing_source": "estimate", "upstream_cost_credit": float64(0),
		}, object(t, value))
	})
}
