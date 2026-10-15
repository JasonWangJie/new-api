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
	t.Run("current reference and keyframe contracts", func(t *testing.T) {
		references := make([]any, 14)
		for i := range references {
			references[i] = "https://cdn.example/reference.png"
		}
		for _, model := range []string{"grok-imagine-video", "grok-imagine-video-1.5"} {
			limit, seconds := 7, 10
			if model == "grok-imagine-video-1.5" {
				limit, seconds = 14, 15
			}
			resolved, callErr := decode(t, model, map[string]any{"reference_images": references[:limit], "duration": seconds, "aspect_ratio": "21:9"})
			require.NoError(t, callErr)
			assert.Equal(t, "reference_to_video", resolved["action"])
			assert.Len(t, resolved["requestBody"].(map[string]any)["reference_images"], limit)
			_, callErr = decode(t, model, map[string]any{"reference_images": append(append([]any{}, references[:limit]...), references[0])})
			require.ErrorContains(t, callErr, "reference_images supports at most")
		}
		keyframes := []any{
			map[string]any{"image": map[string]any{"url": "https://cdn.example/key.png"}, "timestamp_s": 1.1},
			map[string]any{"image": map[string]any{"file_id": "file-key"}, "timestamp_s": 2},
			map[string]any{"image": "data:image/png;base64,AA==", "timestamp_s": 3},
			map[string]any{"image": "https://cdn.example/key.png", "timestamp_s": 4},
		}
		resolved, callErr := decode(t, "grok-imagine-video-1.5", map[string]any{
			"image": "https://cdn.example/first.png", "last_frame": "https://cdn.example/last.png", "keyframes": keyframes,
			"reference_images": references, "reference_audios": []any{"eve", "leo", "ara"}, "duration": 5, "resolution": "720p", "aspect_ratio": "5:2",
		})
		require.NoError(t, callErr)
		assert.Equal(t, "reference_to_video", resolved["action"])
		body := resolved["requestBody"].(map[string]any)
		assert.Equal(t, 1.1, body["keyframes"].([]any)[0].(map[string]any)["timestamp_s"])
		value, callErr := plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{"action": resolved["action"], "requestBody": body})
		require.NoError(t, callErr)
		assert.Equal(t, float64(20), object(t, value)["input_images"], "14 references, four keyframes and two pinned endpoints all count")
		resolved, callErr = decode(t, "grok-imagine-video-1.5", map[string]any{"keyframes": keyframes[:1]})
		require.NoError(t, callErr)
		assert.Equal(t, "reference_to_video", resolved["action"], "a pinned interior frame does not require a prompt or endpoint frame")
		for _, tc := range []struct {
			name, model, errorText string
			request                map[string]any
		}{
			{"classic reference duration", "grok-imagine-video", "at most 10", map[string]any{"reference_images": references[:1], "duration": 11}},
			{"classic references with first frame", "grok-imagine-video", "cannot combine", map[string]any{"reference_images": references[:1], "image": "https://cdn.example/first.png"}},
			{"classic keyframes", "grok-imagine-video", "require grok-imagine-video-1.5", map[string]any{"keyframes": keyframes[:1]}},
			{"too many keyframes", "grok-imagine-video-1.5", "at most 4", map[string]any{"keyframes": append(append([]any{}, keyframes...), keyframes[0])}},
			{"keyframe resolution", "grok-imagine-video-1.5", "at most 720p", map[string]any{"keyframes": keyframes[:1], "resolution": "1080p"}},
			{"keyframe shape", "grok-imagine-video-1.5", "must be an array", map[string]any{"keyframes": keyframes[0]}},
			{"missing timestamp", "grok-imagine-video-1.5", "timestamp_s", map[string]any{"keyframes": []any{map[string]any{"image": references[0]}}}},
			{"string timestamp", "grok-imagine-video-1.5", "timestamp_s", map[string]any{"keyframes": []any{map[string]any{"image": references[0], "timestamp_s": "2"}}}},
			{"zero timestamp", "grok-imagine-video-1.5", "inside", map[string]any{"keyframes": []any{map[string]any{"image": references[0], "timestamp_s": 0}}}},
			{"negative timestamp", "grok-imagine-video-1.5", "inside", map[string]any{"keyframes": []any{map[string]any{"image": references[0], "timestamp_s": -1}}}},
			{"end timestamp", "grok-imagine-video-1.5", "inside", map[string]any{"keyframes": []any{map[string]any{"image": references[0], "timestamp_s": 5}}}},
			{"oversized timestamp", "grok-imagine-video-1.5", "inside", map[string]any{"keyframes": []any{map[string]any{"image": references[0], "timestamp_s": 999999999}}}},
			{"same grid slot", "grok-imagine-video-1.5", "same", map[string]any{"keyframes": []any{
				map[string]any{"image": references[0], "timestamp_s": 1.01}, map[string]any{"image": references[0], "timestamp_s": 1.1},
			}}},
			{"missing keyframe image", "grok-imagine-video-1.5", "image", map[string]any{"keyframes": []any{map[string]any{"timestamp_s": 1}}}},
			{"conflicting duration", "grok-imagine-video-1.5", "cannot both", map[string]any{"prompt": "test", "duration": 1, "seconds": 15}},
			{"metadata duration", "grok-imagine-video-1.5", "metadata.duration cannot override", map[string]any{"prompt": "test", "metadata": map[string]any{"duration": 999999999}}},
			{"metadata keyframes", "grok-imagine-video-1.5", "metadata.keyframes cannot override", map[string]any{"prompt": "test", "metadata": map[string]any{"keyframes": keyframes}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, callErr := decode(t, tc.model, tc.request)
				require.ErrorContains(t, callErr, tc.errorText)
			})
		}
	})
	t.Run("synchronous and asynchronous receipts share duration settlement", func(t *testing.T) {
		ctx := map[string]any{"publicTaskId": "task-local", "action": "text_to_video", "requestBody": map[string]any{"duration": 5, "resolution": "720p"}}
		for _, tc := range []struct {
			name      string
			body      map[string]any
			immediate bool
			source    string
		}{
			{"pending receipt", map[string]any{"request_id": "upstream-id"}, false, ""},
			{"completed receipt", map[string]any{"status": "done", "video": map[string]any{"url": "https://cdn.example/out.mp4", "duration": 6}}, true, "upstream"},
			{"missing duration", map[string]any{"video": map[string]any{"url": "https://cdn.example/out.mp4"}}, true, "request"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				value, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": tc.body})
				require.NoError(t, err)
				parsed := object(t, value)
				if tc.immediate {
					assert.Equal(t, "task-local", parsed["taskId"])
					assert.Equal(t, "SUCCESS", parsed["immediate"].(map[string]any)["status"])
					assert.Equal(t, tc.source, parsed["state"].(map[string]any)["duration_source"])
				} else {
					assert.Equal(t, "upstream-id", parsed["taskId"])
					assert.NotContains(t, parsed, "immediate")
				}
			})
		}
		for _, seconds := range []any{6, nil, 0, -1, 999999999} {
			body := map[string]any{"status": "done", "video": map[string]any{"url": "https://cdn.example/out.mp4", "duration": seconds}}
			value, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{}, body)
			require.NoError(t, err)
			facts := object(t, value)
			if seconds == 6 {
				assert.Equal(t, float64(6), facts["seconds"])
			} else {
				assert.NotContains(t, facts, "seconds", "missing or invalid measurements must preserve frozen request seconds")
			}
		}
		for _, resolution := range []string{"4k", "unexpected"} {
			value, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{}, map[string]any{"video": map[string]any{"resolution": resolution}})
			require.NoError(t, err)
			assert.NotContains(t, object(t, value), "resolution", "invalid upstream resolution must preserve the priced request tier")
		}
	})

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
			"https://gateway.example/v1/media/objects/video-id?signature=one",
			"https://api.x.ai/v1/videos/video-id/content",
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
		_, callErr = callOperation("edit", map[string]any{"prompt": "restyle", "video": "http://cdn.example/source.mp4"})
		require.ErrorContains(t, callErr, "HTTPS")
		_, callErr = callOperation("edit", map[string]any{"prompt": "restyle", "video": "data:video/quicktime;base64,AA=="})
		require.ErrorContains(t, callErr, "video/mp4")
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
		value, callErr = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", map[string]any{"action": "edit_video"}, map[string]any{}, map[string]any{
			"video": map[string]any{"duration": 0.5, "resolution": "480p"},
		})
		require.NoError(t, callErr)
		assert.Equal(t, 0.5, object(t, value)["seconds"], "video editing has no one-second minimum in the provider contract")
		assert.Equal(t, "480p", object(t, value)["resolution"])
	})

	for _, testCase := range []struct {
		name, model, errorText string
		request                map[string]any
	}{
		{name: "duration boundary", model: "grok-imagine-video-1.5", request: map[string]any{"prompt": "bad", "duration": 16}, errorText: "between 1 and 15"},
		{name: "invalid resolution", model: "grok-imagine-video-1.5", request: map[string]any{"prompt": "bad", "resolution": "ultra"}, errorText: "resolution must"},
		{name: "reference count", model: "grok-imagine-video-1.5", request: map[string]any{"prompt": "bad", "reference_images": make([]any, 15)}, errorText: "at most 14"},
		{name: "reference resolution", model: "grok-imagine-video-1.5", request: map[string]any{"reference_images": []any{"https://cdn.example/1.png"}, "resolution": "1080p"}, errorText: "at most 720p"},
		{name: "classic last frame", model: "grok-imagine-video", request: map[string]any{"last_frame": "https://cdn.example/last.png"}, errorText: "require grok-imagine-video-1.5"},
		{name: "classic image and references conflict", model: "grok-imagine-video", request: map[string]any{"image": "https://cdn.example/first.png", "reference_images": []any{"https://cdn.example/ref.png"}}, errorText: "cannot combine"},
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
		for _, contentURL := range []string{"/v1/videos/request-id/content", "https://api.x.ai/v1/videos/request-id/content"} {
			value, callErr = plugin.Engine.Call(t.Context(), "buildContentRequest", map[string]any{
				"baseUrl": "https://api.x.ai", "apiKey": "test-only-key", "artifactKey": "video", "clientRequest": map[string]any{"method": "GET"},
				"data": map[string]any{"video": map[string]any{"url": contentURL}},
			})
			require.NoError(t, callErr)
			descriptor = object(t, value)
			assert.Equal(t, "https://api.x.ai/v1/videos/request-id/content", descriptor["url"])
			assert.Equal(t, "Bearer test-only-key", descriptor["headers"].(map[string]any)["Authorization"])
			assert.NotEqual(t, true, descriptor["credentialless"])
		}
		value, callErr = plugin.Engine.Call(t.Context(), "buildContentRequest", map[string]any{
			"baseUrl": "https://api.x.ai", "apiKey": "test-only-key", "artifactKey": "video", "clientRequest": map[string]any{"method": "GET"},
			"data": map[string]any{"video": map[string]any{"url": "https://api.x.ai.attacker.example/v1/videos/request-id/content"}},
		})
		require.NoError(t, callErr)
		assert.NotContains(t, object(t, value), "headers", "external artifacts must never receive the channel credential")
		for _, contentURL := range []string{"//attacker.example/v1/videos/request-id/content", "/v1/videos/../content"} {
			_, callErr = plugin.Engine.Call(t.Context(), "buildContentRequest", map[string]any{
				"baseUrl": "https://api.x.ai", "apiKey": "test-only-key", "artifactKey": "video", "clientRequest": map[string]any{"method": "GET"},
				"data": map[string]any{"video": map[string]any{"url": contentURL}},
			})
			require.Error(t, callErr)
		}
		_, callErr = plugin.Engine.Call(t.Context(), "buildContentRequest", map[string]any{
			"baseUrl": "http://api.example", "apiKey": "test-only-key", "artifactKey": "video", "clientRequest": map[string]any{"method": "GET"},
			"data": map[string]any{"video": map[string]any{"url": "/v1/videos/request-id/content"}},
		})
		require.ErrorContains(t, callErr, "requires HTTPS")

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
