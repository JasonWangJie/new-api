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
		name, size, ratio, errorText string
		fields                       map[string]any
	}{
		{"public defaults", "720x1280", "9:16", "", defaults},
		{"native defaults", "720x1280", "9:16", "", nil},
		{"public landscape", "1280x720", "16:9", "", map[string]any{"resolution": "720p", "aspect_ratio": "16:9"}},
		{"aspect ratio alias", "720x1280", "9:16", "", map[string]any{"aspectRatio": "9:16"}},
		{"ratio alias", "1280x720", "16:9", "", map[string]any{"ratio": "16:9"}},
		{"metadata dimensions", "1280x720", "16:9", "", map[string]any{"metadata": map[string]any{"resolution": "720p", "aspectRatio": "16:9"}}},
		{"native size", "1280x720", "16:9", "", map[string]any{"size": "1280x720"}},
		{"native portrait size", "720x1280", "9:16", "", map[string]any{"size": "720x1280"}},
		{"native pro size is preserved", "1792x1024", "16:9", "", map[string]any{"size": "1792x1024"}},
		{"native pro portrait size is preserved", "1024x1792", "9:16", "", map[string]any{"size": "1024x1792"}},
		{"native metadata size", "1024x1792", "9:16", "", map[string]any{"metadata": map[string]any{"size": "1024x1792"}}},
		{"unsupported native size", "", "", "supported native Sora size", map[string]any{"size": "1920x1080"}},
		{"unsupported public resolution", "", "", "public resolution must be 720p", map[string]any{"resolution": "1080p"}},
		{"unsupported metadata resolution", "", "", "public resolution must be 720p", map[string]any{"metadata": map[string]any{"resolution": "4k"}}},
		{"native size is not public 720p", "", "", "conflict with the native Sora size", map[string]any{"size": "1792x1024", "resolution": "720p"}},
		{"conflicting aspect ratio", "", "", "conflicts with the native Sora size", map[string]any{"size": "720x1280", "aspect_ratio": "16:9"}},
		{"conflicting duration aliases", "", "", "seconds and duration must match", map[string]any{"seconds": 4, "duration": 8}},
		{"invalid generic duration", "", "", "seconds must be one of", map[string]any{"seconds": 5}},
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
				assert.Equal(t, tc.ratio, intent["requestBody"].(map[string]any)["aspect_ratio"])
				require.NoError(t, service.ValidateVideoProfileRequest(profile, "text_to_video", intent["requestBody"], nil))
				for _, allowedRatio := range []string{"16:9", "9:16"} {
					restricted, err := service.ApplyVideoModelOverrides(profile, service.VideoModelOverrides{AspectRatios: []string{allowedRatio}})
					require.NoError(t, err)
					validationErr := service.ValidateVideoProfileRequest(restricted, "text_to_video", intent["requestBody"], nil)
					if allowedRatio == tc.ratio {
						assert.NoError(t, validationErr)
					} else {
						assert.ErrorContains(t, validationErr, "aspect ratio", "normalization must retain the actual orientation for host policy checks")
					}
				}
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
			"model":        "sora-2-pro",
			"prompt":       "waves at sunset",
			"seconds":      float64(8),
			"size":         "1792x1024",
			"aspect_ratio": "16:9",
		},
		wantUsageKeys:  []string{"seconds", "size"},
		wantVendorName: "sora",
	})
}

func TestSoraVideoDurationAliases(t *testing.T) {
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, fields, errorText string
		seconds                 int
	}{
		{name: "native default", seconds: 4},
		{name: "top seconds", fields: `,"seconds":4`, seconds: 4},
		{name: "top duration string", fields: `,"duration":"8"`, seconds: 8},
		{name: "metadata seconds", fields: `,"metadata":{"seconds":12,"label":"kept"}`, seconds: 12},
		{name: "metadata duration string", fields: `,"metadata":{"duration":"12"}`, seconds: 12},
		{name: "all matching aliases", fields: `,"seconds":8,"duration":"8","metadata":{"seconds":"8","duration":8}`, seconds: 8},
		{name: "metadata masks top seconds", fields: `,"seconds":4,"metadata":{"duration":12}`, errorText: "must match"},
		{name: "top duration masks metadata seconds", fields: `,"duration":12,"metadata":{"seconds":4}`, errorText: "must match"},
		{name: "metadata conflicts", fields: `,"metadata":{"seconds":4,"duration":12}`, errorText: "must match"},
		{name: "top aliases conflict", fields: `,"seconds":4,"duration":12`, errorText: "must match"},
		{name: "explicit zero", fields: `,"seconds":0,"metadata":{"duration":12}`, errorText: "must be one of"},
		{name: "explicit metadata zero", fields: `,"metadata":{"duration":0}`, errorText: "must be one of"},
		{name: "null", fields: `,"metadata":{"seconds":null}`, errorText: "must be one of"},
		{name: "boolean", fields: `,"seconds":false`, errorText: "must be one of"},
		{name: "array coercion", fields: `,"metadata":{"seconds":[4]}`, errorText: "must be one of"},
		{name: "empty string", fields: `,"duration":""`, errorText: "must be one of"},
		{name: "unsupported duration", fields: `,"metadata":{"duration":5}`, errorText: "must be one of"},
		{name: "oversized duration", fields: `,"seconds":18446744073686646784`, errorText: "must be one of"},
	} {
		for _, surface := range []string{"json", "multipart", "responses"} {
			t.Run(tc.name+"/"+surface, func(t *testing.T) {
				var request map[string]any
				require.NoError(t, common.Unmarshal([]byte(`{"model":"sora-2-pro","prompt":"animate","input":"animate"`+tc.fields+`}`), &request))
				body := map[string]any{"kind": "json", "value": request}
				protocol := "openai_video"
				if surface == "multipart" {
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
					body = map[string]any{"kind": "multipart", "fields": fields}
				} else if surface == "responses" {
					protocol = "openai_responses"
				}
				ctx := map[string]any{"model": "sora-2-pro", "upstreamModel": "sora-2-pro", "body": body, "baseUrl": "https://sora.example"}
				value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{protocol, "decodeRequest"}, ctx)
				if tc.errorText != "" {
					require.ErrorContains(t, err, tc.errorText)
					return
				}
				require.NoError(t, err)
				intent := value.(map[string]any)
				canonical := intent["requestBody"].(map[string]any)
				assert.EqualValues(t, tc.seconds, canonical["seconds"])
				assert.NotContains(t, canonical, "duration")
				if metadata, ok := canonical["metadata"].(map[string]any); ok {
					assert.NotContains(t, metadata, "seconds")
					assert.NotContains(t, metadata, "duration")
					assert.Equal(t, request["metadata"].(map[string]any)["label"], metadata["label"])
				}
				ctx["requestBody"], ctx["action"] = canonical, intent["action"]
				value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
				require.NoError(t, err)
				wire := value.(map[string]any)["body"].(map[string]any)
				assert.EqualValues(t, tc.seconds, wire["seconds"])
				usage, err := plugin.Engine.Call(t.Context(), "extractUsage", ctx)
				require.NoError(t, err)
				assert.Equal(t, wire["seconds"], usage.(map[string]any)["seconds"])
			})
		}
	}
}

func TestSoraRemixInheritsOriginDimensions(t *testing.T) {
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	ctx := map[string]any{
		"model": "sora-2-pro", "upstreamModel": "sora-2-pro", "action": "remix", "originTaskId": "origin-video",
		"baseUrl":     "https://sora.example",
		"requestBody": map[string]any{"prompt": "add more stars"},
		"originTasks": []any{map[string]any{"data": map[string]any{"seconds": 12, "size": "1792x1024"}}},
	}
	value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
	require.NoError(t, err)
	descriptor := value.(map[string]any)
	assert.Equal(t, "https://sora.example/v1/videos/origin-video/remix", descriptor["url"])
	wire := descriptor["body"].(map[string]any)
	assert.Equal(t, "add more stars", wire["prompt"])
	assert.NotContains(t, wire, "size", "remix must inherit the origin dimensions instead of selecting new-video defaults")
	assert.NotContains(t, wire, "seconds", "remix must inherit the origin duration instead of selecting the new-video default")
	assert.NotContains(t, wire, "aspect_ratio")
	usage, err := plugin.Engine.Call(t.Context(), "extractUsage", ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 12, usage.(map[string]any)["seconds"])
	assert.Equal(t, "1792x1024", usage.(map[string]any)["size"])
	for _, fields := range []map[string]any{
		{"seconds": 8},
		{"duration": 12},
		{"metadata": map[string]any{"seconds": 8}},
		{"metadata": map[string]any{"duration": 12}},
	} {
		ctx["requestBody"] = fields
		fields["prompt"] = "add more stars"
		_, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
		require.ErrorContains(t, err, "remix duration is inherited", "remix must not send a duration that differs from origin-based billing")
	}
}

func TestSoraMultipartPrimaryImageAliases(t *testing.T) {
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	profile, found := plugin.Meta.VideoProfileForModel("sora-2")
	require.True(t, found)
	for _, field := range []string{"image", "input_reference"} {
		t.Run(field, func(t *testing.T) {
			files := []map[string]any{{"field": field, "ref": "request_file:" + field + ":2", "filename": "frame.png", "mimeType": "image/png"}}
			ctx := map[string]any{
				"model": "public-video", "upstreamModel": "sora-2", "files": files,
				"body":    map[string]any{"kind": "multipart", "fields": map[string]any{"prompt": []string{"animate"}, "metadata": []string{`{"duration":8}`}, "resolution": []string{"720p"}, "aspect_ratio": []string{"16:9"}}, "files": files},
				"baseUrl": "https://sora.example", "apiKey": "test-only",
			}
			value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, ctx)
			require.NoError(t, callErr)
			intent := value.(map[string]any)
			assert.Equal(t, "public-video", intent["model"])
			assert.Equal(t, "image_to_video", intent["action"])
			require.NoError(t, service.ValidateVideoProfileRequest(profile, "image_to_video", intent["requestBody"], files))
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
			assert.EqualValues(t, 8, fields["seconds"])
			assert.NotContains(t, fields, "duration")
			assert.JSONEq(t, `{}`, fields["metadata"].(string))
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
