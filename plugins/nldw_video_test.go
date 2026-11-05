package plugins_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadNLDWVideoPlugin(t *testing.T) *jsplugin.LoadedPlugin {
	t.Helper()
	source, err := builtinplugins.Source("nldw")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "nldw"})
	require.NoError(t, err)
	return plugin
}

func TestNLDWVideoRequiresDurableAdmissionBeforeBilling(t *testing.T) {
	plugin := loadNLDWVideoPlugin(t)
	for _, path := range []string{"/v1/videos", "/v1/videos/generations_async"} {
		t.Run(path, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"sd2.5pp","prompt":"a video"}`))
			c.Request.Header.Set("X-Async-Media-Execution", "true")
			c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{Plugin: plugin})
			info := &relaycommon.RelayInfo{OriginModelName: "sd2.5pp", TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
			result, taskErr := relay.RelayTaskSubmit(c, info)
			require.NotNil(t, taskErr)
			assert.Equal(t, http.StatusBadRequest, taskErr.StatusCode)
			assert.Equal(t, "async_video_required", taskErr.Code)
			assert.Contains(t, taskErr.Error.Error(), "/v1/videos/generations_async")
			assert.Nil(t, result)
			assert.Nil(t, info.Billing, "rejection must precede any charge or upstream dispatch")
			assert.Empty(t, info.PublicTaskID)
		})
	}
	t.Run("durable admission validates without dispatch or charge", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
		c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{Plugin: plugin})
		c.Set("async_media_prepare", true)
		c.Set("task_request", map[string]any{"model": "sd2.5pp", "prompt": "a video", "seconds": 5, "resolution": "720p"})
		common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://api.nldw.example")
		frozen := service.MediaBillingSnapshot{}
		frozen.Price.Quota = 100
		c.Set("async_media_billing", frozen)
		info := &relaycommon.RelayInfo{OriginModelName: "sd2.5pp", ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "sd2.5pp"}, TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: "text_to_video"}}
		result, taskErr := relay.RelayTaskSubmit(c, info)
		require.Nil(t, taskErr)
		require.NotNil(t, result)
		assert.Equal(t, 100, result.Quota)
		assert.Empty(t, result.UpstreamTaskID)
		assert.Nil(t, info.Billing)
	})
}

func decodeNLDWVideoRequest(t *testing.T, plugin *jsplugin.LoadedPlugin, upstream string, request map[string]any) (map[string]any, error) {
	t.Helper()
	value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"model": "public-alias", "upstreamModel": upstream, "operation": "create", "body": map[string]any{"kind": "json", "value": request},
	})
	if err != nil {
		return nil, err
	}
	return value.(map[string]any), nil
}

func TestLegacyVideoConfiguredModels(t *testing.T) {
	for _, tc := range []struct {
		key, known string
		load       func(*testing.T) *jsplugin.LoadedPlugin
	}{
		{key: "nldw", known: "sd2.5pp", load: loadNLDWVideoPlugin},
		{key: "yuxibox", known: "minimax-h3-15s", load: func(t *testing.T) *jsplugin.LoadedPlugin { return loadYuxiboxVideoPlugin(t, nil) }},
		{key: "tianyue", known: "L-sd2-video", load: loadTianyueVideoPlugin},
	} {
		t.Run(tc.key, func(t *testing.T) {
			plugin := tc.load(t)
			raw := map[string]any{"prompt": " animate ", "seconds": 6, "resolution": "720p"}
			preliminary, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": tc.known, "operation": "create", "body": map[string]any{"kind": "json", "value": raw},
			})
			require.NoError(t, err)
			assert.Equal(t, raw, preliminary.(map[string]any)["requestBody"], "public names must not select upstream defaults or capability rules")
			assert.NotContains(t, preliminary.(map[string]any), "action")

			images, videos := make([]any, 32), make([]any, 32)
			for i := range images {
				images[i], videos[i] = "https://cdn.example/image.png", "https://cdn.example/video.mp4"
			}
			request := map[string]any{"prompt": "animate", "seconds": 3600, "resolution": "1080p", "aspect_ratio": "1:1", "reference_images": images, "reference_videos": videos}
			upstream := "vendor/自定义视频"
			value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": tc.known, "upstreamModel": upstream, "operation": "create", "body": map[string]any{"kind": "json", "value": request},
			})
			require.NoError(t, err)
			intent := value.(map[string]any)
			assert.Equal(t, tc.known, intent["model"])
			assert.Equal(t, "reference_to_video", intent["action"])
			ctx := map[string]any{"model": tc.known, "upstreamModel": upstream, "requestBody": intent["requestBody"], "baseUrl": "https://api.example", "apiKey": "test-key", "publicTaskId": "public-id"}
			value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, err)
			descriptor := value.(map[string]any)
			assert.Equal(t, "https://api.example/v1/videos", descriptor["url"])
			assert.Equal(t, upstream, descriptor["body"].(map[string]any)["model"])
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			facts := value.(map[string]any)
			assert.Len(t, facts, 3)
			assert.EqualValues(t, 3600, facts["seconds"])
			assert.EqualValues(t, 1, facts["requests"])
			assert.Equal(t, "1080p", facts["resolution"])
			ctx["usagePurpose"] = "billing_ratios"
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			assert.Nil(t, value, "the host chooses the administrator's billing mode")
			value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{"status": "SUCCESS"}, map[string]any{"status": "completed", "seconds": 1})
			require.NoError(t, err)
			assert.Empty(t, value, "custom model settlement retains the frozen submission usage")
			value, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": "public-alias", "upstreamModel": strings.Repeat("字", 85), "operation": "create",
				"body": map[string]any{"kind": "json", "value": map[string]any{
					"prompt": "animate", "seconds": 1, "resolution": "Future.8K", "aspect_ratio": "32:9", "reference_images": "https://cdn.example/image.png",
				}},
			})
			require.NoError(t, err, "255 UTF-8 byte models and single-input workbench controls are accepted")
			assert.Equal(t, "reference_to_video", value.(map[string]any)["action"])
			customBody := value.(map[string]any)["requestBody"].(map[string]any)
			assert.Equal(t, "Future.8K", customBody["resolution"], "unknown specifications must not inherit a preset's resolution conversion")
			assert.Equal(t, "32:9", customBody["aspect_ratio"])
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{"model": "public-alias", "upstreamModel": upstream, "requestBody": customBody})
			require.NoError(t, err)
			assert.Equal(t, "future.8k", value.(map[string]any)["resolution"], "usage follows the host's canonical resolution enum")

			for _, invalid := range []struct {
				name, model, omit string
				fields            map[string]any
			}{
				{name: "missing duration", omit: "seconds"},
				{name: "missing resolution", omit: "resolution"},
				{name: "missing ratio", omit: "aspect_ratio"},
				{name: "zero duration", fields: map[string]any{"seconds": 0}},
				{name: "host duration ceiling", fields: map[string]any{"seconds": 3601}},
				{name: "unsafe resolution token", fields: map[string]any{"resolution": "8k/unsafe"}},
				{name: "resolution token ceiling", fields: map[string]any{"resolution": strings.Repeat("a", 33)}},
				{name: "invalid ratio", fields: map[string]any{"aspect_ratio": "0:9"}},
				{name: "ratio bound", fields: map[string]any{"aspect_ratio": "1000:9"}},
				{name: "combined media ceiling", fields: map[string]any{"reference_audios": []any{"https://cdn.example/audio.mp3"}}},
				{name: "custom references require URLs", fields: map[string]any{"reference_images": []any{"data:image/png;base64,YQ=="}}},
				{name: "untrimmed model", model: " future"},
				{name: "wildcard model", model: "future*"},
				{name: "comma model", model: "future,other"},
				{name: "control model", model: "future\u0085"},
				{name: "utf8 model ceiling", model: strings.Repeat("字", 86)},
			} {
				t.Run(invalid.name, func(t *testing.T) {
					body := map[string]any{}
					for name, value := range request {
						body[name] = value
					}
					delete(body, invalid.omit)
					for name, value := range invalid.fields {
						body[name] = value
					}
					modelName := upstream
					if invalid.model != "" {
						modelName = invalid.model
					}
					_, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
						"model": tc.known, "upstreamModel": modelName, "body": map[string]any{"kind": "json", "value": body},
					})
					require.Error(t, err)
					_, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": tc.known, "upstreamModel": modelName, "requestBody": body, "publicTaskId": "public-id"})
					require.Error(t, err, "the driver must independently enforce the same boundary")
				})
			}
		})
	}
}

func TestConfiguredVideoSingleImageReferenceProtocol(t *testing.T) {
	const upstream = "vendor/future-image-video"
	const imageURL = "https://cdn.example/image.png"
	for _, key := range []string{"yali", "nldw", "yuxibox"} {
		t.Run(key, func(t *testing.T) {
			source, err := builtinplugins.Source(key)
			require.NoError(t, err)
			plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: key})
			require.NoError(t, err)
			images, videos, audios := 2, 0, 1
			config := kitdto.TaskVideoModelConfig{Duration: kitdto.TaskVideoDuration{Values: []int{5}}, Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"}, Modes: []string{"text_to_video", "reference_to_video", "image_to_video"}, References: kitdto.TaskVideoReferences{Images: &images, Videos: &videos, Audios: &audios}, BillingMode: "per_request"}
			require.NoError(t, config.ValidateForPlugin(key, relaycommon.MaxTaskDurationSeconds))
			plugin = jsplugin.WithTaskVideoModels(plugin, map[string]kitdto.TaskVideoModelConfig{upstream: config})
			profile, found := plugin.Meta.VideoProfileForModel(upstream)
			require.True(t, found)
			for _, tc := range []struct {
				name, action string
				fields       map[string]any
			}{
				{name: "text", action: "text_to_video", fields: map[string]any{"mode": "text_to_video"}},
				{name: "singular image", action: "image_to_video", fields: map[string]any{"image": imageURL}},
				{name: "explicit image alias", action: "image_to_video", fields: map[string]any{"input_reference": imageURL, "mode": "image_to_video"}},
				{name: "single reference remains reference", action: "reference_to_video", fields: map[string]any{"reference_images": []any{imageURL}}},
				{name: "explicit reference", action: "reference_to_video", fields: map[string]any{"reference_images": []any{imageURL}, "mode": "reference_to_video"}},
				{name: "image plus reference", action: "reference_to_video", fields: map[string]any{"image": imageURL, "reference_images": []any{"https://cdn.example/other.png"}}},
				{name: "image plus audio", action: "reference_to_video", fields: map[string]any{"image": imageURL, "reference_audios": []any{"https://cdn.example/audio.mp3"}}},
				{name: "image mode rejects missing image", fields: map[string]any{"mode": "image_to_video"}},
				{name: "image mode rejects references", fields: map[string]any{"image": imageURL, "reference_images": []any{"https://cdn.example/other.png"}, "mode": "image_to_video"}},
				{name: "image mode rejects audio", fields: map[string]any{"image": imageURL, "reference_audios": []any{"https://cdn.example/audio.mp3"}, "mode": "image_to_video"}},
				{name: "image mode rejects a last frame", fields: map[string]any{"image": imageURL, "last_frame": "https://cdn.example/last.png", "mode": "image_to_video"}},
				{name: "text mode rejects image", fields: map[string]any{"image": imageURL, "mode": "text_to_video"}},
				{name: "reference mode requires references", fields: map[string]any{"mode": "reference_to_video"}},
				{name: "reference collection cannot become image implicitly", fields: map[string]any{"reference_images": []any{imageURL}, "mode": "image_to_video"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					request := map[string]any{"prompt": "animate the scene", "seconds": 5, "resolution": "720p", "aspect_ratio": "16:9"}
					for name, value := range tc.fields {
						request[name] = value
					}
					intent, err := decodeNLDWVideoRequest(t, plugin, upstream, request)
					if tc.action == "" {
						require.Error(t, err)
						_, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": "public-alias", "upstreamModel": upstream, "requestBody": request, "publicTaskId": "public-image-id"})
						require.Error(t, err, "the outbound boundary must reject contradictory modes too")
						return
					}
					require.NoError(t, err)
					assert.Equal(t, tc.action, intent["action"])
					body := intent["requestBody"].(map[string]any)
					require.NoError(t, service.ValidateVideoProfileRequest(profile, tc.action, body, nil), "canonical input must match the channel's configured mode")
					repeated, err := decodeNLDWVideoRequest(t, plugin, upstream, body)
					require.NoError(t, err)
					assert.Equal(t, intent, repeated, "candidate and final decoders must preserve image versus reference semantics")
					if tc.action == "image_to_video" {
						assert.Equal(t, imageURL, body["image"])
						assert.NotContains(t, body, "references")
						assert.NotContains(t, body, "reference_images")
					} else {
						assert.NotContains(t, body, "image")
					}
					value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": "public-alias", "upstreamModel": upstream, "requestBody": body, "publicTaskId": "public-image-id", "baseUrl": "https://api.example", "apiKey": "fixture-key"})
					require.NoError(t, err)
					wire := value.(map[string]any)["body"].(map[string]any)
					assert.Equal(t, upstream, wire["model"])
					for _, field := range []string{"image", "input_reference", "first_frame", "start_frame", "last_frame"} {
						assert.NotContains(t, wire, field, "single-image generation must use the existing ordinary-reference protocol")
					}
					if tc.action == "image_to_video" {
						switch key {
						case "yali":
							assert.Equal(t, []any{imageURL}, wire["images"])
						case "nldw":
							assert.Equal(t, "omni_reference", wire["mode"])
							assert.Equal(t, []any{map[string]any{"kind": "image", "data": imageURL}}, wire["references"])
						case "yuxibox":
							assert.Equal(t, []any{imageURL}, wire["reference_images"])
						}
					}
				})
			}
		})
	}
}

func TestNLDWVideoModelsReferencesAndBilling(t *testing.T) {
	plugin := loadNLDWVideoPlugin(t)
	assert.ElementsMatch(t, []string{"sd2.5pp", "sd2.5", "sd2.5de"}, plugin.Meta.Models)
	assert.Empty(t, plugin.Meta.ChannelTypes, "the provider must be bound through its own task-plugin channel")
	for _, tc := range []struct {
		name, upstream, action, resolution, quantity string
		seconds                                      int
		fields                                       map[string]any
		wantReferences                               []any
		scalarField                                  string
	}{
		{name: "pp text is per video", upstream: "sd2.5pp", action: "text_to_video", resolution: "1080p", quantity: "requests", seconds: 15},
		{name: "native interleaved references", upstream: "sd2.5", action: "reference_to_video", resolution: "720p", quantity: "seconds", seconds: 30,
			fields: map[string]any{"references": []any{
				map[string]any{"kind": "audio", "data": "data:audio/mpeg;base64,YQ=="}, map[string]any{"kind": "image", "data": "https://cdn.example/image.png"},
				map[string]any{"kind": "video", "data": "https://cdn.example/video.mp4"}, map[string]any{"kind": "image", "data": "https://cdn.example/other.png"},
			}},
			wantReferences: []any{
				map[string]any{"kind": "audio", "data": "data:audio/mpeg;base64,YQ=="}, map[string]any{"kind": "image", "data": "https://cdn.example/image.png"},
				map[string]any{"kind": "video", "data": "https://cdn.example/video.mp4"}, map[string]any{"kind": "image", "data": "https://cdn.example/other.png"},
			}},
		{name: "standard video reference", upstream: "sd2.5", action: "reference_to_video", resolution: "480p", quantity: "seconds", seconds: 5,
			fields:         map[string]any{"reference_videos": []any{"https://cdn.example/reference.mp4"}},
			wantReferences: []any{map[string]any{"kind": "video", "data": "https://cdn.example/reference.mp4"}}},
		{name: "de image", upstream: "sd2.5de", action: "reference_to_video", resolution: "480p", quantity: "seconds", seconds: 4,
			fields:         map[string]any{"input_reference": "data:image/png;base64,YQ=="},
			wantReferences: []any{map[string]any{"kind": "image", "data": "data:image/png;base64,YQ=="}}},
		{name: "de audio only", upstream: "sd2.5de", action: "reference_to_video", resolution: "720p", quantity: "seconds", seconds: 30,
			fields:         map[string]any{"reference_audios": []any{"https://cdn.example/audio.wav"}, "aspect_ratio": "adaptive"},
			wantReferences: []any{map[string]any{"kind": "audio", "data": "https://cdn.example/audio.wav"}}},
		{name: "pp single image control", upstream: "sd2.5pp", action: "reference_to_video", resolution: "720p", quantity: "requests", seconds: 5,
			fields: map[string]any{"reference_images": "https://cdn.example/image.png"}, scalarField: "reference_images",
			wantReferences: []any{map[string]any{"kind": "image", "data": "https://cdn.example/image.png"}}},
		{name: "sd single video control", upstream: "sd2.5", action: "reference_to_video", resolution: "720p", quantity: "seconds", seconds: 5,
			fields: map[string]any{"reference_videos": "https://cdn.example/video.mp4"}, scalarField: "reference_videos",
			wantReferences: []any{map[string]any{"kind": "video", "data": "https://cdn.example/video.mp4"}}},
		{name: "de single audio control", upstream: "sd2.5de", action: "reference_to_video", resolution: "480p", quantity: "seconds", seconds: 4,
			fields: map[string]any{"reference_audios": "https://cdn.example/audio.wav"}, scalarField: "reference_audios",
			wantReferences: []any{map[string]any{"kind": "audio", "data": "https://cdn.example/audio.wav"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"model": "public-alias", "prompt": " keep the references ", "seconds": tc.seconds, "resolution": tc.resolution, "aspect_ratio": "9:16", "mode": "omni_reference"}
			for key, value := range tc.fields {
				request[key] = value
			}
			intent, err := decodeNLDWVideoRequest(t, plugin, tc.upstream, request)
			require.NoError(t, err)
			assert.Equal(t, "public-alias", intent["model"])
			assert.Equal(t, tc.action, intent["action"])
			profile, found := plugin.Meta.VideoProfileForModel(tc.upstream)
			require.True(t, found)
			if tc.scalarField != "" {
				arrayRequest := make(map[string]any, len(request))
				for name, value := range request {
					arrayRequest[name] = value
				}
				arrayRequest[tc.scalarField] = []any{request[tc.scalarField]}
				arrayIntent, err := decodeNLDWVideoRequest(t, plugin, tc.upstream, arrayRequest)
				require.NoError(t, err)
				assert.Equal(t, arrayIntent, intent, "a single-item workbench control is equivalent to its array representation")
				for _, mode := range profile.Modes {
					if mode.Name != tc.action {
						continue
					}
					mode.Inputs = append([]jsplugin.VideoInputProfile(nil), mode.Inputs...)
					for i := range mode.Inputs {
						mode.Inputs[i].MaxItems = 1
					}
					profile.Modes = []jsplugin.VideoModeProfile{mode}
					break
				}
			}
			require.NoError(t, service.ValidateVideoProfileRequest(profile, tc.action, intent["requestBody"], nil))
			assert.Equal(t, " keep the references ", request["prompt"], "repeated candidate decoding must leave the client request intact")
			again, err := decodeNLDWVideoRequest(t, plugin, tc.upstream, intent["requestBody"].(map[string]any))
			require.NoError(t, err)
			assert.Equal(t, intent, again)
			ctx := map[string]any{"model": "public-alias", "upstreamModel": tc.upstream, "requestBody": intent["requestBody"], "baseUrl": "https://api.nldw.example", "apiKey": "test-key", "publicTaskId": "gateway-task-1", "requestHeaders": map[string]string{"Idempotency-Key": "client-key"}}
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, err)
			descriptor := value.(map[string]any)
			assert.Equal(t, "https://api.nldw.example/v1/videos", descriptor["url"])
			assert.Equal(t, http.MethodPost, descriptor["method"])
			assert.NotContains(t, descriptor, "model", "the public model identity stays pinned to the alias")
			headers := descriptor["headers"].(map[string]any)
			assert.Equal(t, "Bearer test-key", headers["Authorization"])
			assert.Equal(t, "gateway-task-1", headers["Idempotency-Key"])
			body := descriptor["body"].(map[string]any)
			assert.Equal(t, tc.upstream, body["model"])
			assert.EqualValues(t, tc.seconds, body["seconds"])
			assert.Equal(t, tc.resolution, body["resolution"])
			assert.Equal(t, "omni_reference", body["mode"])
			assert.Equal(t, request["aspect_ratio"], body["aspect_ratio"])
			if tc.wantReferences != nil {
				assert.Equal(t, tc.wantReferences, body["references"])
			} else {
				assert.Empty(t, body["references"])
			}
			for _, field := range []string{"reference_images", "reference_videos", "reference_audios", "input_reference"} {
				assert.NotContains(t, body, field)
			}
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			facts := value.(map[string]any)
			wantQuantity := tc.seconds
			if tc.quantity == "requests" {
				wantQuantity = 1
			}
			assert.EqualValues(t, wantQuantity, facts[tc.quantity])
			assert.Equal(t, tc.resolution, facts["resolution"])
			schema, _ := plugin.Meta.UsageForModel(tc.upstream)
			assert.Contains(t, schema, tc.quantity)
			assert.Contains(t, schema["resolution"].Enum, tc.resolution)
			assert.Len(t, facts, len(schema))
			ctx["usagePurpose"] = "billing_ratios"
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			if tc.quantity == "requests" {
				assert.Nil(t, value, "per-video billing must not multiply by seconds")
			} else {
				assert.EqualValues(t, tc.seconds, value.(map[string]any)["seconds"])
			}
			value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, err)
			assert.Equal(t, descriptor, value, "repeating one dispatch keeps identical JSON and upstream idempotency identity")
		})
	}
}

func TestNLDWVideoAdaptorUsesDurableIdentityAndModelMapping(t *testing.T) {
	plugin := loadNLDWVideoPlugin(t)
	intent, err := decodeNLDWVideoRequest(t, plugin, "sd2.5de", map[string]any{
		"prompt": "a video", "duration": 4, "size": "480p", "reference_audios": []any{"https://cdn.example/audio.mp3"},
	})
	require.NoError(t, err)
	var previousBody map[string]any
	for _, publicID := range []string{"gateway-task-1", "gateway-task-1", "gateway-task-2"} {
		info := &relaycommon.RelayInfo{
			OriginModelName: "public-alias",
			ChannelMeta:     &relaycommon.ChannelMeta{ApiKey: "test-key", ChannelBaseUrl: "https://api.nldw.example", UpstreamModelName: "sd2.5de"},
			TaskRelayInfo:   &relaycommon.TaskRelayInfo{PublicTaskID: publicID, Action: "reference_to_video"},
		}
		adaptor := taskplugin.New(plugin)
		adaptor.Init(info)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Header.Set("Idempotency-Key", "one-client-key")
		c.Set("task_request", intent["requestBody"])
		reader, err := adaptor.BuildRequestBody(c, info)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, common.DecodeJson(reader, &body))
		assert.Equal(t, "sd2.5de", body["model"])
		assert.EqualValues(t, 4, body["seconds"])
		assert.Equal(t, []any{map[string]any{"kind": "audio", "data": "https://cdn.example/audio.mp3"}}, body["references"])
		if previousBody != nil {
			assert.Equal(t, previousBody, body)
		}
		previousBody = body
		url, err := adaptor.BuildRequestURL(info)
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, url, nil)
		require.NoError(t, err)
		require.NoError(t, adaptor.BuildRequestHeader(c, req, info))
		assert.Equal(t, publicID, req.Header.Get("Idempotency-Key"), "retries share one identity and separate gateway tasks stay distinct")
		assert.Equal(t, "Bearer test-key", req.Header.Get("Authorization"))
		assert.Equal(t, "public-alias", info.OriginModelName)
	}
}

func TestNLDWVideoRejectsInvalidRequestsBeforeBilling(t *testing.T) {
	plugin := loadNLDWVideoPlugin(t)
	thirtyImages := make([]any, 30)
	for i := range thirtyImages {
		thirtyImages[i] = "https://cdn.example/image.png"
	}
	for _, tc := range []struct {
		name, model string
		fields      map[string]any
	}{
		{name: "custom model requires explicit ratio", model: "sd2.5-pro"},
		{name: "empty prompt", fields: map[string]any{"prompt": " "}},
		{name: "zero seconds", fields: map[string]any{"seconds": 0}},
		{name: "negative seconds", fields: map[string]any{"seconds": -1}},
		{name: "fractional seconds", fields: map[string]any{"seconds": 5.5}},
		{name: "boolean seconds", fields: map[string]any{"seconds": true}},
		{name: "overflow seconds", fields: map[string]any{"seconds": "18446744073686646784"}},
		{name: "pp upper bound", fields: map[string]any{"seconds": 16}},
		{name: "sd lower bound", model: "sd2.5", fields: map[string]any{"seconds": 4}},
		{name: "sd upper bound", model: "sd2.5", fields: map[string]any{"seconds": 31}},
		{name: "de lower bound", model: "sd2.5de", fields: map[string]any{"seconds": 3}},
		{name: "conflicting duration alias", fields: map[string]any{"duration": 9}},
		{name: "pp resolution", fields: map[string]any{"resolution": "480p"}},
		{name: "de resolution", model: "sd2.5de", fields: map[string]any{"resolution": "1080p"}},
		{name: "pp adaptive ratio", fields: map[string]any{"aspect_ratio": "adaptive"}},
		{name: "unsupported mode", fields: map[string]any{"mode": "first_last_frame"}},
		{name: "metadata multiplier bypass", fields: map[string]any{"metadata": map[string]any{"seconds": 9000}}},
		{name: "unsupported audio generation switch", fields: map[string]any{"generate_audio": false}},
		{name: "unsupported quantity", fields: map[string]any{"n": 2}},
		{name: "malformed array", fields: map[string]any{"reference_images": map[string]any{"url": "https://cdn.example/image.png"}}},
		{name: "aggregate image aliases", fields: map[string]any{"reference_images": thirtyImages, "image": "https://cdn.example/extra.png"}},
		{name: "aggregate native aliases", fields: map[string]any{"reference_images": thirtyImages, "references": []any{map[string]any{"kind": "image", "data": "https://cdn.example/extra.png"}}}},
		{name: "too many audio references", fields: map[string]any{"reference_audios": []any{"https://cdn.example/1", "https://cdn.example/2", "https://cdn.example/3", "https://cdn.example/4", "https://cdn.example/5", "https://cdn.example/6", "https://cdn.example/7", "https://cdn.example/8", "https://cdn.example/9", "https://cdn.example/10", "https://cdn.example/11"}}},
		{name: "unsupported reference kind", fields: map[string]any{"references": []any{map[string]any{"kind": "file", "data": "https://cdn.example/file"}}}},
		{name: "reference extra field", fields: map[string]any{"references": []any{map[string]any{"kind": "image", "data": "https://cdn.example/file", "seconds": 9000}}}},
		{name: "http URL", fields: map[string]any{"image": "http://cdn.example/image.png"}},
		{name: "private IP", fields: map[string]any{"image": "https://127.0.0.1/image.png"}},
		{name: "credential URL", fields: map[string]any{"image": "https://user:password@cdn.example/image.png"}},
		{name: "wrong inline MIME", fields: map[string]any{"image": "data:audio/mpeg;base64,YQ=="}},
		{name: "invalid inline base64", fields: map[string]any{"image": "data:image/png;base64,???"}},
		{name: "sd text only", model: "sd2.5", fields: map[string]any{"image": nil}},
		{name: "sd audio only", model: "sd2.5", fields: map[string]any{"image": nil, "reference_audios": []any{"https://cdn.example/audio.mp3"}}},
		{name: "de text only", model: "sd2.5de", fields: map[string]any{"image": nil}},
		{name: "de video", model: "sd2.5de", fields: map[string]any{"reference_videos": []any{"https://cdn.example/video.mp4"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := tc.model
			if upstream == "" {
				upstream = "sd2.5pp"
			}
			request := map[string]any{"prompt": "a video", "seconds": 5, "resolution": "720p", "image": "https://cdn.example/image.png"}
			for key, value := range tc.fields {
				if value == nil {
					delete(request, key)
				} else {
					request[key] = value
				}
			}
			_, err := decodeNLDWVideoRequest(t, plugin, upstream, request)
			require.Error(t, err)
			_, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": upstream, "requestBody": request, "publicTaskId": "public-id"})
			require.Error(t, err, "direct driver execution must retain the validation boundary")
		})
	}
	_, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": "sd2.5pp", "requestBody": map[string]any{"prompt": "a video", "seconds": 5, "resolution": "720p"}})
	require.Error(t, err, "a create without a durable gateway task identity must not be sent")
}

func TestNLDWVideoTaskLifecyclePreservesUncertainCharges(t *testing.T) {
	plugin := loadNLDWVideoPlugin(t)
	ctx := map[string]any{"model": "sd2.5", "taskId": "private/id", "requestBody": map[string]any{"prompt": "a video", "seconds": 10, "resolution": "720p", "image": "https://cdn.example/image.png"}}
	for _, status := range []string{"queued", "in_progress", "submission_unknown", "completed", "failed"} {
		t.Run("submit "+status, func(t *testing.T) {
			body := map[string]any{"id": "private/id", "status": status}
			if status == "submission_unknown" {
				body["error"] = map[string]any{"message": "submission outcome unknown"}
			}
			value, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": body})
			require.NoError(t, err)
			receipt := value.(map[string]any)
			assert.Equal(t, "private/id", receipt["taskId"])
			assert.EqualValues(t, 10, receipt["state"].(map[string]any)["requestedSeconds"])
			assert.NotContains(t, receipt, "immediate", "an accepted task is reconciled by polling before settlement")
		})
	}
	for _, tc := range []struct {
		name string
		id   any
	}{
		{name: "null", id: nil}, {name: "numeric", id: 123}, {name: "empty", id: ""},
		{name: "leading whitespace", id: " private-id"}, {name: "control", id: "private\n-id"}, {name: "oversized", id: strings.Repeat("x", 192)},
		{name: "current path segment", id: "."}, {name: "parent path segment", id: ".."},
	} {
		t.Run("invalid task ID "+tc.name, func(t *testing.T) {
			_, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": map[string]any{"id": tc.id, "status": "queued"}})
			require.Error(t, err)
			_, err = plugin.Engine.Call(t.Context(), "buildQueryRequest", map[string]any{"taskId": tc.id, "baseUrl": "https://api.nldw.example"})
			require.Error(t, err, "an invalid task ID must not change the query endpoint")
			_, err = plugin.Engine.Call(t.Context(), "buildContentRequest", map[string]any{"upstreamTaskId": tc.id, "artifactKey": "video", "baseUrl": "https://api.nldw.example", "clientRequest": map[string]any{"method": http.MethodGet}})
			require.Error(t, err, "an invalid task ID must not change the authenticated content endpoint")
		})
	}
	ctx["state"] = map[string]any{"requestedSeconds": 10}
	adaptor := taskplugin.New(plugin)
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: "https://api.nldw.example"}})
	pluginState, err := common.Marshal(ctx["state"])
	require.NoError(t, err)
	task := &model.Task{TaskID: "public-id", Status: model.TaskStatusInProgress,
		Properties:  model.Properties{OriginModelName: "sd2.5", UpstreamModelName: "sd2.5"},
		PrivateData: model.TaskPrivateData{UpstreamTaskID: "private/id", PluginState: pluginState}}
	for _, tc := range []struct {
		name   string
		status any
	}{
		{name: "failure array", status: []any{"failed"}},
		{name: "completion array", status: []any{"completed"}},
		{name: "object", status: map[string]any{"toString": "failed"}},
		{name: "null", status: nil},
		{name: "number", status: 400},
		{name: "boolean", status: false},
	} {
		t.Run("invalid status type "+tc.name, func(t *testing.T) {
			body := map[string]any{"id": "private/id", "status": tc.status, "seconds": 8}
			value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
			require.NoError(t, err)
			assert.Equal(t, "UNKNOWN", value.(map[string]any)["status"], "a non-string status cannot confirm completion or authorize a refund")
			value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{"status": "SUCCESS"}, body)
			require.NoError(t, err)
			assert.Empty(t, value, "malformed completion statuses cannot replace frozen usage")
			encoded, err := common.Marshal(body)
			require.NoError(t, err)
			result, err := adaptor.ParseTaskResult(task, &http.Response{StatusCode: http.StatusOK}, encoded)
			require.NoError(t, err)
			assert.Equal(t, "UNKNOWN", result.Status)
			assert.Empty(t, result.UsageFacts)
		})
	}
	for _, tc := range []struct {
		name, field string
		value       any
		diagnostic  bool
	}{
		{name: "video URL", field: "video_url", value: "https://cdn.example/result.mp4"},
		{name: "URL", field: "url", value: "https://cdn.example/result.mp4"},
		{name: "nested video", field: "video", value: map[string]any{"url": "https://cdn.example/result.mp4"}},
		{name: "metadata URL", field: "metadata", value: map[string]any{"url": "https://cdn.example/result.mp4"}},
		{name: "result collection", field: "results", value: []any{map[string]any{"url": "https://cdn.example/result.mp4"}}},
		{name: "output collection", field: "outputs", value: []any{map[string]any{"video_url": "https://cdn.example/result.mp4"}}},
		{name: "artifact collection", field: "artifacts", value: []any{map[string]any{"url": "https://cdn.example/result.mp4"}}},
		{name: "nested completion", field: "metadata", value: map[string]any{"status": "completed"}},
		{name: "echoed input diagnostics", field: "request", value: map[string]any{"url": "https://cdn.example/input.png", "references": []any{map[string]any{"kind": "image", "data": "https://cdn.example/input.png"}}}, diagnostic: true},
	} {
		t.Run("failed evidence "+tc.name, func(t *testing.T) {
			body := map[string]any{"id": "private/id", "status": "failed", tc.field: tc.value}
			value, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": body})
			require.NoError(t, err)
			receipt := value.(map[string]any)
			assert.Equal(t, "private/id", receipt["taskId"])
			assert.NotContains(t, receipt, "immediate", "a contradictory receipt must retain the accepted task and reservation")
			value, err = plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
			if tc.diagnostic {
				require.NoError(t, err)
				assert.Equal(t, "FAILURE", value.(map[string]any)["status"], "echoed reference URLs do not prevent a confirmed failure from refunding")
				return
			}
			require.Error(t, err, "output or completion evidence contradicts failure and must prevent a refund")
		})
	}
	for _, tc := range []struct{ status, want string }{
		{"queued", "QUEUED"}, {"in_progress", "IN_PROGRESS"}, {"submission_unknown", "IN_PROGRESS"}, {"completed", "SUCCESS"}, {"failed", "FAILURE"}, {"unrecognized", "UNKNOWN"}, {"toString", "UNKNOWN"},
	} {
		t.Run("poll "+tc.status, func(t *testing.T) {
			body := map[string]any{"id": "private/id", "status": tc.status, "progress": 50}
			if tc.status == "submission_unknown" {
				body["error"] = map[string]any{"message": "submission outcome unknown"}
			}
			value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
			require.NoError(t, err)
			assert.Equal(t, tc.want, value.(map[string]any)["status"])
		})
	}
	for _, tc := range []struct {
		name    string
		error   any
		wantErr bool
	}{
		{name: "true error", error: true, wantErr: true},
		{name: "numeric error", error: 503, wantErr: true},
		{name: "array error", error: []any{"failed"}, wantErr: true},
		{name: "unrecognized error object", error: map[string]any{"detail": "failed"}, wantErr: true},
		{name: "null error", error: nil}, {name: "empty error object", error: map[string]any{}},
		{name: "empty error text", error: ""}, {name: "false error", error: false}, {name: "zero error", error: 0},
		{name: "empty error fields", error: map[string]any{"message": "", "code": 0}},
	} {
		t.Run("completion error shape "+tc.name, func(t *testing.T) {
			body := map[string]any{"id": "private/id", "status": "completed", "seconds": 8, "error": tc.error}
			value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
			if tc.wantErr {
				require.Error(t, err, "a malformed nonempty error cannot confirm success")
				_, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{"status": "SUCCESS"}, body)
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "SUCCESS", value.(map[string]any)["status"])
			value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{"status": "SUCCESS"}, body)
			require.NoError(t, err)
			assert.EqualValues(t, 8, value.(map[string]any)["seconds"])
		})
	}
	for _, body := range []map[string]any{
		{"id": "different-task", "status": "completed"},
		{"id": "private/id", "status": "completed", "error": map[string]any{"message": "generation failed"}},
		{"id": "private/id", "status": "completed", "seconds": 11},
		{"id": "private/id", "status": "completed", "seconds": "18446744073686646784"},
	} {
		_, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
		require.Error(t, err, "contradictory or unbounded results must not settle or refund the reservation")
	}
	value, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{"status": "SUCCESS"}, map[string]any{"id": "private/id", "status": "completed", "seconds": 8})
	require.NoError(t, err)
	assert.EqualValues(t, 8, value.(map[string]any)["seconds"])
	value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{"status": "SUCCESS"}, map[string]any{"id": "private/id", "status": "completed"})
	require.NoError(t, err)
	assert.Empty(t, value, "missing completion usage retains the frozen requested seconds")
	_, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{"status": "SUCCESS"}, map[string]any{"id": "private/id", "status": "completed", "seconds": 11})
	require.Error(t, err)
	ctx["model"] = "sd2.5pp"
	value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{"status": "SUCCESS"}, map[string]any{"id": "private/id", "status": "completed", "seconds": 8})
	require.NoError(t, err)
	assert.EqualValues(t, 1, value.(map[string]any)["requests"])
}

func TestNLDWVideoQueriesAndDownloadsUseOriginalAuthenticatedTask(t *testing.T) {
	plugin := loadNLDWVideoPlugin(t)
	value, err := plugin.Engine.Call(t.Context(), "buildQueryRequest", map[string]any{"taskId": "private/id?query", "baseUrl": "https://api.nldw.example", "apiKey": "test-key"})
	require.NoError(t, err)
	assert.Equal(t, "https://api.nldw.example/v1/videos/private%2Fid%3Fquery", value.(map[string]any)["url"])
	assert.Equal(t, "Bearer test-key", value.(map[string]any)["headers"].(map[string]any)["Authorization"])
	adaptor := taskplugin.New(plugin)
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "test-key", ChannelBaseUrl: "https://api.nldw.example"}})
	data, err := common.Marshal(map[string]any{"id": "private/id?query", "status": "completed", "video_url": "https://other.example/do-not-use.mp4"})
	require.NoError(t, err)
	task := &model.Task{TaskID: "public-id", Status: model.TaskStatusSuccess, Data: data, PrivateData: model.TaskPrivateData{UpstreamTaskID: "private/id?query"}}
	artifacts, err := adaptor.ListArtifacts(task)
	require.NoError(t, err)
	assert.Equal(t, []channel.TaskArtifact{{Key: "video", Type: "video", MimeType: "video/mp4"}}, artifacts)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		descriptor, err := adaptor.BuildContentRequest(task, "video", channel.TaskArtifactClientRequest{Method: method})
		require.NoError(t, err)
		require.NotNil(t, descriptor)
		assert.Equal(t, "https://api.nldw.example/v1/videos/private%2Fid%3Fquery/content", descriptor.URL)
		assert.Equal(t, method, descriptor.Method)
		assert.Equal(t, "Bearer test-key", descriptor.Headers["Authorization"])
		assert.False(t, descriptor.Credentialless)
	}
	value, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "render"}, map[string]any{}, map[string]any{"data": map[string]any{"id": "private/id", "video_url": "https://other.example/do-not-leak.mp4"}})
	require.NoError(t, err)
	assert.Empty(t, value, "the public response is rendered by the host without the provider's private task or output URL")
}
