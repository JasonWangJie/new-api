package plugins_test

import (
	"maps"
	"testing"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoogleResponsesProtocol(t *testing.T) {
	testVideoResponsesProtocol(t, videoResponsesTestCase{
		pluginKey: "google",
		model:     "veo-3.1-fast-generate-preview",
		requestBody: map[string]any{
			"model": "veo-3.1-fast-generate-preview",
			"input": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "animate this frame"},
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="},
			}}},
			"seconds": 8,
			"size":    "1280x720",
		},
		wantAction: "image_to_video",
		wantRequest: map[string]any{
			"model":    "veo-3.1-fast-generate-preview",
			"prompt":   "animate this frame",
			"images":   []any{"data:image/png;base64,aGVsbG8="},
			"duration": float64(8),
			"size":     "1280x720",
			"metadata": map[string]any{},
		},
		wantUsageKeys:  []string{"resolution", "seconds"},
		wantVendorName: "gemini",
	})
}

func TestGoogleVideoProfilesDefaultsAndWireParameters(t *testing.T) {
	source, err := builtinplugins.Source("google")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "google"})
	require.NoError(t, err)
	for _, model := range plugin.Meta.Models {
		profile, found := plugin.Meta.VideoProfileForModel(model)
		require.True(t, found, model)
		for _, mode := range profile.Modes {
			require.NotNil(t, mode.Duration)
			assert.Equal(t, 8, mode.Duration.Default)
			assert.NotContains(t, mode.Duration.Values, 5, "the generic five-second host default is not a Veo duration")
		}
	}
	for _, tc := range []struct {
		name, protocol, model, upstream, resolution, ratio string
		request                                            map[string]any
		seconds                                            int
	}{
		{"native 3.0 default", "openai_video", "veo-3.0-generate-001", "", "720p", "16:9", nil, 8},
		{"native 3.1 default", "openai_video", "veo-3.1-generate-preview", "", "720p", "16:9", nil, 8},
		{"mapped default", "openai_video", "cinema-public", "veo-3.1-fast-generate-preview", "720p", "16:9", nil, 8},
		{"mapped portrait", "openai_video", "cinema-public", "veo-3.1-fast-generate-preview", "720p", "9:16", map[string]any{"seconds": 6, "resolution": "720p", "aspect_ratio": "9:16"}, 6},
		{"mapped 1080p", "openai_video", "cinema-public", "veo-3.1-generate-preview", "1080p", "9:16", map[string]any{"seconds": 8, "resolution": "1080p", "aspect_ratio": "9:16"}, 8},
		{"mapped 4k", "openai_video", "cinema-public", "veo-3.1-generate-preview", "4k", "16:9", map[string]any{"seconds": 8, "resolution": "4k"}, 8},
		{"legacy metadata", "openai_video", "veo-3.1-generate-preview", "", "1080p", "9:16", map[string]any{"metadata": map[string]any{"durationSeconds": 8, "resolution": "1080p", "aspectRatio": "9:16"}}, 8},
		{"native short duration", "openai_video", "veo-3.1-generate-preview", "", "720p", "16:9", map[string]any{"metadata": map[string]any{"durationSeconds": 4}}, 4},
		{"top native duration", "openai_video", "veo-3.1-generate-preview", "", "720p", "16:9", map[string]any{"durationSeconds": 6}, 6},
		{"matching native duration", "openai_video", "veo-3.1-generate-preview", "", "720p", "16:9", map[string]any{"seconds": 4, "metadata": map[string]any{"durationSeconds": 4}}, 4},
		{"pixel dimensions", "openai_video", "veo-3.1-generate-preview", "", "1080p", "9:16", map[string]any{"size": "1080x1920"}, 8},
		{"Responses standard parameters", "openai_responses", "cinema-public", "veo-3.1-fast-generate-preview", "720p", "9:16", map[string]any{"seconds": 4, "resolution": "720p", "aspect_ratio": "9:16"}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"model": tc.model, "prompt": "a walking cat", "input": "a walking cat"}
			maps.Copy(request, tc.request)
			ctx := map[string]any{
				"model": tc.model, "upstreamModel": tc.upstream, "operation": "create",
				"body":    map[string]any{"kind": "json", "value": request},
				"baseUrl": "https://generativelanguage.googleapis.example", "apiKey": "test-only-key",
			}
			value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{tc.protocol, "decodeRequest"}, ctx)
			require.NoError(t, callErr)
			intent := value.(map[string]any)
			assert.Equal(t, tc.model, intent["model"])
			ctx["requestBody"], ctx["action"] = intent["requestBody"], intent["action"]
			upstream := tc.upstream
			if upstream == "" {
				upstream = tc.model
			}
			profile, found := plugin.Meta.VideoProfileForModel(upstream)
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, intent["action"].(string), intent["requestBody"], nil))
			value, callErr = plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, callErr)
			descriptor := value.(map[string]any)
			assert.NotContains(t, descriptor, "model")
			assert.Equal(t, "https://generativelanguage.googleapis.example/v1beta/models/"+upstream+":predictLongRunning", descriptor["url"])
			body := descriptor["body"].(map[string]any)
			params := body["parameters"].(map[string]any)
			assert.EqualValues(t, tc.seconds, params["durationSeconds"])
			assert.Equal(t, tc.resolution, params["resolution"])
			assert.Equal(t, tc.ratio, params["aspectRatio"])
			assert.EqualValues(t, 1, params["numberOfVideos"])
			value, callErr = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, callErr)
			facts := value.(map[string]any)
			assert.EqualValues(t, tc.seconds, facts["seconds"])
			assert.Equal(t, tc.resolution, facts["resolution"])
		})
	}
	t.Run("image reference and standard options survive conversion", func(t *testing.T) {
		value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "cinema-public", "upstreamModel": "veo-3.1-generate-preview", "body": map[string]any{"kind": "json", "value": map[string]any{
				"prompt": "animate this", "input_reference": "data:image/png;base64,aGVsbG8=", "seconds": 8, "resolution": "1080p", "aspect_ratio": "9:16",
				"metadata": map[string]any{"seed": 42},
			}},
		})
		require.NoError(t, callErr)
		intent := value.(map[string]any)
		assert.Equal(t, "image_to_video", intent["action"])
		value, callErr = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
			"model": "cinema-public", "upstreamModel": "veo-3.1-generate-preview", "requestBody": intent["requestBody"], "baseUrl": "https://google.example",
		})
		require.NoError(t, callErr)
		body := value.(map[string]any)["body"].(map[string]any)
		instance := body["instances"].([]any)[0].(map[string]any)
		assert.Equal(t, "aGVsbG8=", instance["image"].(map[string]any)["inlineData"].(map[string]any)["data"])
		params := body["parameters"].(map[string]any)
		assert.EqualValues(t, 42, params["seed"])
		assert.Equal(t, "1080p", params["resolution"])
		assert.Equal(t, "9:16", params["aspectRatio"])
	})
	for _, field := range []string{"image", "input_reference"} {
		t.Run("native multipart "+field, func(t *testing.T) {
			files := []map[string]any{{"field": field, "ref": "request_file:" + field + ":2", "mimeType": "image/png"}}
			ctx := map[string]any{
				"model": "cinema-public", "upstreamModel": "veo-3.1-generate-preview", "files": files,
				"body":    map[string]any{"kind": "multipart", "fields": map[string]any{"prompt": []string{"animate this"}}, "files": files},
				"baseUrl": "https://google.example",
			}
			value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, ctx)
			require.NoError(t, callErr)
			intent := value.(map[string]any)
			assert.Equal(t, "cinema-public", intent["model"])
			assert.Equal(t, "image_to_video", intent["action"])
			profile, found := plugin.Meta.VideoProfileForModel("veo-3.1-generate-preview")
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, "image_to_video", intent["requestBody"], files))
			ctx["requestBody"] = intent["requestBody"]
			value, callErr = plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, callErr)
			wire := value.(map[string]any)["body"].(map[string]any)
			image := wire["instances"].([]any)[0].(map[string]any)["image"].(map[string]any)
			inline := image["inlineData"].(map[string]any)
			assert.Equal(t, "image/png", inline["mimeType"])
			assert.Equal(t, files[0]["ref"], inline["data"].(map[string]any)["__fileRef"])
		})
	}
	for _, tc := range []struct {
		name, model, errorText string
		request                map[string]any
	}{
		{"generic five seconds", "veo-3.1-generate-preview", "seconds must be one of", map[string]any{"seconds": 5}},
		{"3.0 duration", "veo-3.0-generate-001", "seconds must be 8", map[string]any{"seconds": 4}},
		{"3.0 4k", "veo-3.0-generate-001", "resolution must be 720p or 1080p", map[string]any{"resolution": "4k"}},
		{"high resolution short clip", "veo-3.1-generate-preview", "require 8 seconds", map[string]any{"seconds": 6, "resolution": "1080p"}},
		{"3.0 portrait 1080p", "veo-3.0-generate-001", "requires aspect_ratio 16:9", map[string]any{"resolution": "1080p", "aspect_ratio": "9:16"}},
		{"invalid aspect ratio", "veo-3.1-generate-preview", "aspect_ratio must be", map[string]any{"aspect_ratio": "1:1"}},
		{"conflicting duration", "veo-3.1-generate-preview", "seconds and duration must match", map[string]any{"seconds": 4, "duration": 8}},
		{"conflicting native duration", "veo-3.1-generate-preview", "video duration aliases must match", map[string]any{"seconds": 4, "metadata": map[string]any{"durationSeconds": 8}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": "cinema-public", "upstreamModel": tc.model, "body": map[string]any{"kind": "json", "value": tc.request},
			})
			require.ErrorContains(t, callErr, tc.errorText)
		})
	}
}
