package plugins_test

import (
	"fmt"
	"maps"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSoraVideoDimensionsAndDefaults(t *testing.T) {
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	profile, found := plugin.Meta.VideoProfileForModel("sora-2-pro")
	require.True(t, found)
	_, _, defaults, err := service.NormalizeMixedVideoDefaults([]byte(`{"model":"public-video","prompt":"a cat"}`), "application/json", "create", []jsplugin.VideoProfile{profile})
	require.NoError(t, err)
	assert.EqualValues(t, 4, defaults["seconds"], "the public default must match Sora's native duration")
	assert.Equal(t, "720p", defaults["resolution"])
	assert.Equal(t, "9:16", defaults["aspect_ratio"])
	for _, tc := range []struct {
		name, size, errorText string
		fields                map[string]any
	}{
		{"public defaults", "720x1280", "", defaults},
		{"public landscape", "1280x720", "", map[string]any{"resolution": "720p", "aspect_ratio": "16:9"}},
		{"metadata dimensions", "1280x720", "", map[string]any{"metadata": map[string]any{"resolution": "720p", "aspectRatio": "16:9"}}},
		{"native size", "1280x720", "", map[string]any{"size": "1280x720"}},
		{"native pro size is preserved", "1792x1024", "", map[string]any{"size": "1792x1024"}},
		{"unsupported public resolution", "", "public resolution must be 720p", map[string]any{"resolution": "1080p"}},
		{"unsupported metadata resolution", "", "public resolution must be 720p", map[string]any{"metadata": map[string]any{"resolution": "4k"}}},
		{"native size is not public 720p", "", "conflict with the native Sora size", map[string]any{"size": "1792x1024", "resolution": "720p"}},
		{"conflicting aspect ratio", "", "conflicts with the native Sora size", map[string]any{"size": "720x1280", "aspect_ratio": "16:9"}},
		{"conflicting duration aliases", "", "seconds and duration must match", map[string]any{"seconds": 4, "duration": 8}},
		{"invalid generic duration", "", "seconds must be one of", map[string]any{"seconds": 5}},
	} {
		for _, kind := range []string{"json", "multipart"} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				request := map[string]any{"model": "public-video", "prompt": "a cat", "seconds": 8}
				maps.Copy(request, tc.fields)
				body := map[string]any{"kind": kind, "value": request}
				if kind == "multipart" {
					fields := make(map[string][]string)
					for key, value := range request {
						text := fmt.Sprint(value)
						if key == "metadata" {
							encoded, err := common.Marshal(value)
							require.NoError(t, err)
							text = string(encoded)
						}
						fields[key] = []string{text}
					}
					body = map[string]any{"kind": kind, "fields": fields}
				}
				ctx := map[string]any{"model": "public-video", "upstreamModel": "sora-2-pro", "body": body, "baseUrl": "https://sora.example"}
				value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, ctx)
				if tc.errorText != "" {
					require.ErrorContains(t, err, tc.errorText)
					return
				}
				require.NoError(t, err)
				intent := value.(map[string]any)
				ctx["requestBody"], ctx["action"] = intent["requestBody"], intent["action"]
				value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
				require.NoError(t, err)
				wire := value.(map[string]any)["body"].(map[string]any)
				assert.Equal(t, tc.size, wire["size"])
				assert.NotContains(t, wire, "resolution")
				assert.NotContains(t, wire, "aspect_ratio")
				usage, err := plugin.Engine.Call(t.Context(), "extractUsage", ctx)
				require.NoError(t, err)
				assert.Equal(t, tc.size, usage.(map[string]any)["size"], "billing and wire requests must use the same native dimensions")
				assert.Equal(t, wire["seconds"], usage.(map[string]any)["seconds"])
			})
		}
	}
}

func TestSoraResponsesProtocol(t *testing.T) {
	testVideoResponsesProtocol(t, videoResponsesTestCase{
		pluginKey: "sora",
		model:     "sora-2-pro",
		requestBody: map[string]any{
			"model":   "sora-2-pro",
			"input":   "waves at sunset",
			"seconds": 8,
			"size":    "1792x1024",
		},
		wantAction: "text_to_video",
		wantRequest: map[string]any{
			"model":   "sora-2-pro",
			"prompt":  "waves at sunset",
			"seconds": float64(8),
			"size":    "1792x1024",
		},
		wantUsageKeys:  []string{"seconds", "size"},
		wantVendorName: "sora",
	})
}

func TestSoraMultipartPrimaryImageAliases(t *testing.T) {
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	for _, field := range []string{"image", "input_reference"} {
		t.Run(field, func(t *testing.T) {
			files := []map[string]any{{"field": field, "ref": "request_file:" + field + ":2", "filename": "frame.png", "mimeType": "image/png"}}
			ctx := map[string]any{
				"model": "public-video", "upstreamModel": "sora-2", "files": files,
				"body":    map[string]any{"kind": "multipart", "fields": map[string]any{"prompt": []string{"animate"}, "seconds": []string{"8"}, "resolution": []string{"720p"}, "aspect_ratio": []string{"16:9"}}, "files": files},
				"baseUrl": "https://sora.example", "apiKey": "test-only",
			}
			value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, ctx)
			require.NoError(t, callErr)
			intent := value.(map[string]any)
			assert.Equal(t, "public-video", intent["model"])
			assert.Equal(t, "image_to_video", intent["action"])
			require.NoError(t, service.ValidateVideoProfileRequest(service.GenericVideoProfile("sora-2"), "image_to_video", intent["requestBody"], files))
			ctx["requestBody"], ctx["action"] = intent["requestBody"], intent["action"]
			value, callErr = plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, callErr)
			descriptor := value.(map[string]any)
			assert.Equal(t, "multipart", descriptor["bodyType"])
			parts := descriptor["parts"].([]any)
			fields := make(map[string]any)
			for _, value := range parts[:len(parts)-1] {
				part := value.(map[string]any)
				fields[part["name"].(string)] = part["value"]
			}
			assert.Equal(t, "1280x720", fields["size"])
			assert.NotContains(t, fields, "resolution")
			assert.NotContains(t, fields, "aspect_ratio")
			file := parts[len(parts)-1].(map[string]any)
			assert.Equal(t, "input_reference", file["name"], "both public aliases use the upstream's native file field")
			assert.Equal(t, files[0]["ref"], file["fileRef"], "the opaque upload identity must survive field canonicalization")
		})
	}
	_, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"model": "sora-2", "body": map[string]any{"kind": "multipart", "fields": map[string]any{}, "files": []map[string]any{{"field": "image", "ref": "request_file:image"}, {"field": "input_reference", "ref": "request_file:input_reference"}}},
	})
	require.ErrorContains(t, err, "must be provided once")
}
