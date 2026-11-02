package plugins_test

import (
	"testing"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
				"body":    map[string]any{"kind": "multipart", "fields": map[string]any{"prompt": []string{"animate"}, "seconds": []string{"8"}, "size": []string{"1280x720"}}, "files": files},
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
