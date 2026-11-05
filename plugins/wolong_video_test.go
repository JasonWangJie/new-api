package plugins_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
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

func loadWolongVideoPlugin(t *testing.T) *jsplugin.LoadedPlugin {
	t.Helper()
	source, err := builtinplugins.Source("wolong")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "wolong"})
	require.NoError(t, err)
	return plugin
}

func decodeWolongVideoRequest(t *testing.T, plugin *jsplugin.LoadedPlugin, modelName string, request map[string]any) (map[string]any, error) {
	t.Helper()
	value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"model": "public-alias", "upstreamModel": modelName, "operation": "create",
		"body": map[string]any{"kind": "json", "value": request},
	})
	if err != nil {
		return nil, err
	}
	return value.(map[string]any), nil
}

func TestWolongVideoModelsBillingAndIdempotency(t *testing.T) {
	plugin := loadWolongVideoPlugin(t)
	assert.Empty(t, plugin.Meta.ChannelTypes)
	assert.Equal(t, "https://wolongvideo.com", plugin.Meta.BaseURL)
	models := []string{"haya-mo-seedance-25", "mili-bd-seedance-25", "ljvideo-seedance-25-deal-y", "meaicc-sd-2-c5", "meaicc-sd-2-c7", "aigc-seedance-20-manju", "czniu-seedance-20", "czniu-seedance-20-fast", "czniu-seedance-20-mini", "czniu-seedance-20-stable"}
	assert.ElementsMatch(t, models, plugin.Meta.Models)
	for _, modelName := range models {
		t.Run(modelName, func(t *testing.T) {
			request := map[string]any{"prompt": "a blue glass cube"}
			if modelName == "czniu-seedance-20-fast" || modelName == "czniu-seedance-20-mini" {
				request["reference_images"] = "https://cdn.example/reference.png"
			}
			intent, err := decodeWolongVideoRequest(t, plugin, modelName, request)
			require.NoError(t, err)
			canonical := intent["requestBody"].(map[string]any)
			assert.Equal(t, "public-alias", intent["model"])
			assert.EqualValues(t, 5, canonical["seconds"])
			assert.Equal(t, "16:9", canonical["aspect_ratio"])
			resolution := "720p"
			if modelName == "ljvideo-seedance-25-deal-y" {
				resolution = "1080p"
			}
			assert.Equal(t, resolution, canonical["resolution"])
			profile, found := plugin.Meta.VideoProfileForModel(modelName)
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, intent["action"].(string), canonical, nil))
			again, err := decodeWolongVideoRequest(t, plugin, modelName, canonical)
			require.NoError(t, err)
			assert.Equal(t, intent, again, "the host decodes canonical requests again before dispatch")
			ctx := map[string]any{"model": "public-alias", "upstreamModel": modelName, "requestBody": canonical, "baseUrl": "https://upstream.example/", "apiKey": "test-key", "publicTaskId": "video_public-task-1"}
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, err)
			descriptor := value.(map[string]any)
			assert.Equal(t, "https://upstream.example/v1/videos/generations", descriptor["url"])
			assert.Equal(t, http.MethodPost, descriptor["method"])
			assert.NotContains(t, descriptor, "model")
			assert.Equal(t, "Bearer test-key", descriptor["headers"].(map[string]any)["Authorization"])
			assert.Equal(t, "video_public-task-1", descriptor["headers"].(map[string]any)["Idempotency-Key"])
			wire := descriptor["body"].(map[string]any)
			assert.Equal(t, modelName, wire["model"])
			assert.EqualValues(t, 5, wire["duration"])
			assert.Equal(t, resolution, wire["quality"])
			assert.NotContains(t, wire, "wait")
			repeated, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, err)
			assert.Equal(t, descriptor, repeated, "rebuilding one dispatch must preserve its idempotency key and payload")
			facts, err := plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			want := map[string]any{"video_count": int64(1), "resolution": resolution}
			if modelName == "czniu-seedance-20-stable" {
				want = map[string]any{"seconds": int64(5), "resolution": resolution}
			}
			assert.EqualValues(t, want, facts)
			ctx["usagePurpose"] = "billing_ratios"
			facts, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			assert.Nil(t, facts)
			completed, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, map[string]any{"status": "SUCCESS"}, map[string]any{"cost": 9999, "duration": 100000})
			require.NoError(t, err)
			assert.Empty(t, completed, "completion retains the frozen quantity and ignores upstream prices")
		})
	}
}

func TestWolongVideoReferencesAndValidation(t *testing.T) {
	plugin := loadWolongVideoPlugin(t)
	image := "https://cdn.example/image.png?signature=A%2FB&expires=2000000000"
	video := "https://cdn.example/video.mp4"
	for _, tc := range []struct {
		name, modelName, action string
		request                 map[string]any
		images                  []any
	}{
		{"single reference", "haya-mo-seedance-25", "reference_to_video", map[string]any{"reference_images": image}, []any{image}},
		{"duplicate ordered references", "haya-mo-seedance-25", "reference_to_video", map[string]any{"images": []any{image, image}, "videos": []any{video}, "audios": "https://cdn.example/sound.mp3"}, []any{image, image}},
		{"mili four seconds with image", "mili-bd-seedance-25", "reference_to_video", map[string]any{"image": image, "seconds": 4}, []any{image}},
		{"ratio size alias", "haya-mo-seedance-25", "reference_to_video", map[string]any{"reference_images": image, "size": "9:16", "ratio": "9:16"}, []any{image}},
		{"mili auto ratio", "mili-bd-seedance-25", "reference_to_video", map[string]any{"reference_images": image, "aspect_ratio": "auto"}, []any{image}},
		{"manju single image", "aigc-seedance-20-manju", "image_to_video", map[string]any{"image_urls": []any{image}}, []any{image}},
		{"manju implicit frames", "aigc-seedance-20-manju", "first_last_frame", map[string]any{"image_urls": []any{image, image}}, []any{image, image}},
		{"manju explicit frames", "aigc-seedance-20-manju", "first_last_frame", map[string]any{"first_frame": image, "last_frame": image}, []any{image, image}},
		{"manju canonical roles", "aigc-seedance-20-manju", "first_last_frame", map[string]any{"references": []any{map[string]any{"type": "image", "role": "first_frame", "source": image}, map[string]any{"type": "image", "role": "last_frame", "source": image}}}, []any{image, image}},
		{"manju ordinary references", "aigc-seedance-20-manju", "reference_to_video", map[string]any{"reference_images": []any{image, image, image}}, []any{image, image, image}},
		{"fast reference video", "czniu-seedance-20-fast", "reference_to_video", map[string]any{"reference_videos": video}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent, err := decodeWolongVideoRequest(t, plugin, tc.modelName, tc.request)
			require.NoError(t, err)
			assert.Equal(t, tc.action, intent["action"])
			profile, found := plugin.Meta.VideoProfileForModel(tc.modelName)
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, tc.action, intent["requestBody"], nil))
			again, err := decodeWolongVideoRequest(t, plugin, tc.modelName, intent["requestBody"].(map[string]any))
			require.NoError(t, err)
			assert.Equal(t, intent, again)
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"upstreamModel": tc.modelName, "requestBody": intent["requestBody"], "publicTaskId": "video_public-task-1", "baseUrl": "https://upstream.example", "apiKey": "key"})
			require.NoError(t, err)
			wire := value.(map[string]any)["body"].(map[string]any)
			if tc.images != nil {
				assert.Equal(t, tc.images, wire["image_urls"])
			}
			assert.NotContains(t, wire, "reference_images")
			assert.NotContains(t, wire, "references")
		})
	}
	for _, tc := range []struct {
		name, modelName string
		request         map[string]any
	}{
		{"missing prompt", "haya-mo-seedance-25", map[string]any{}},
		{"long prompt", "haya-mo-seedance-25", map[string]any{"prompt": strings.Repeat("中", 10001)}},
		{"haya too short", "haya-mo-seedance-25", map[string]any{"seconds": 4}},
		{"czniu no ten seconds", "czniu-seedance-20", map[string]any{"seconds": 10}},
		{"manju discrete duration", "aigc-seedance-20-manju", map[string]any{"seconds": 6}},
		{"huge duration", "haya-mo-seedance-25", map[string]any{"duration": "18446744073686646784"}},
		{"zero duration", "haya-mo-seedance-25", map[string]any{"seconds": 0}},
		{"fractional duration", "haya-mo-seedance-25", map[string]any{"seconds": 5.5}},
		{"boolean duration", "haya-mo-seedance-25", map[string]any{"seconds": false}},
		{"duration conflict", "haya-mo-seedance-25", map[string]any{"seconds": 5, "duration": 6}},
		{"resolution conflict", "haya-mo-seedance-25", map[string]any{"resolution": "720p", "quality": "1080p"}},
		{"size ratio conflict", "haya-mo-seedance-25", map[string]any{"size": "16:9", "ratio": "9:16"}},
		{"adaptive is not auto", "mili-bd-seedance-25", map[string]any{"size": "adaptive"}},
		{"ljvideo resolution", "ljvideo-seedance-25-deal-y", map[string]any{"resolution": "720p"}},
		{"image type", "haya-mo-seedance-25", map[string]any{"image_urls": []any{false}}},
		{"private media", "haya-mo-seedance-25", map[string]any{"image": "http://127.0.0.1/file.png"}},
		{"credentials", "haya-mo-seedance-25", map[string]any{"image": "https://user:password@cdn.example/file.png"}},
		{"asset ids unsupported", "haya-mo-seedance-25", map[string]any{"asset_ids": []any{"ast_123"}}},
		{"data URI unsupported", "haya-mo-seedance-25", map[string]any{"image": "data:image/png;base64,AAAA"}},
		{"metadata bypass", "haya-mo-seedance-25", map[string]any{"metadata": map[string]any{"duration": 999999}}},
		{"last frame alone", "aigc-seedance-20-manju", map[string]any{"last_frame": image}},
		{"unsupported frames", "haya-mo-seedance-25", map[string]any{"first_frame": image, "last_frame": image}},
		{"manju video", "aigc-seedance-20-manju", map[string]any{"videos": []any{video}}},
		{"fast pure text", "czniu-seedance-20-fast", map[string]any{}},
		{"mini audio only", "czniu-seedance-20-mini", map[string]any{"reference_audios": "https://cdn.example/sound.mp3"}},
		{"mili four second video", "mili-bd-seedance-25", map[string]any{"seconds": 4, "videos": []any{video}}},
		{"unknown model", "czniu-wan-30", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name != "missing prompt" && tc.request["prompt"] == nil {
				tc.request["prompt"] = "a video"
			}
			_, err := decodeWolongVideoRequest(t, plugin, tc.modelName, tc.request)
			require.Error(t, err)
		})
	}
	images := make([]any, 30)
	for i := range images {
		images[i] = image
	}
	_, err := decodeWolongVideoRequest(t, plugin, "haya-mo-seedance-25", map[string]any{"image_urls": images})
	require.NoError(t, err)
	_, err = decodeWolongVideoRequest(t, plugin, "mili-bd-seedance-25", map[string]any{"image_urls": images, "video_urls": []any{video}})
	require.Error(t, err, "Mili caps the aggregate reference count at 30")
	_, err = decodeWolongVideoRequest(t, plugin, "mili-bd-seedance-25", map[string]any{"image_urls": images[:10], "seconds": 4})
	require.Error(t, err, "Mili requires five seconds above nine images")
}

func TestWolongVideoConfiguredModelDecoderAndUsage(t *testing.T) {
	plugin := loadWolongVideoPlugin(t)
	const modelName = "vendor/future-seedance-version"
	const image = "https://cdn.example/reference.png"
	for _, tc := range []struct {
		name, action string
		fields       map[string]any
	}{
		{"text", "text_to_video", map[string]any{}},
		{"image", "image_to_video", map[string]any{"input_reference": image}},
		{"references", "reference_to_video", map[string]any{"image_urls": []any{image, image}, "video_urls": []any{"https://cdn.example/reference.mp4"}}},
		{"primary with ordinary references", "reference_to_video", map[string]any{"input_reference": image, "reference_images": []any{image}}},
		{"frames", "first_last_frame", map[string]any{"first_frame": image, "last_frame": image}},
		{"explicit reference mode", "reference_to_video", map[string]any{"mode": "reference_to_video", "image": image}},
		{"explicit image mode", "image_to_video", map[string]any{"mode": "image_to_video", "image_urls": []any{image}}},
		{"explicit frame mode", "first_last_frame", map[string]any{"mode": "first_last_frame", "image_urls": []any{image, image}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"prompt": "a video", "seconds": 7, "resolution": "480p", "aspect_ratio": "21:9"}
			for key, value := range tc.fields {
				request[key] = value
			}
			intent, err := decodeWolongVideoRequest(t, plugin, modelName, request)
			require.NoError(t, err)
			assert.Equal(t, tc.action, intent["action"])
			canonical := intent["requestBody"].(map[string]any)
			again, err := decodeWolongVideoRequest(t, plugin, modelName, canonical)
			require.NoError(t, err)
			assert.Equal(t, intent, again)
			ctx := map[string]any{"model": "public-alias", "upstreamModel": modelName, "requestBody": canonical, "publicTaskId": "video_public-task-1", "baseUrl": "https://upstream.example", "apiKey": "key"}
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, err)
			wire := value.(map[string]any)["body"].(map[string]any)
			assert.Equal(t, modelName, wire["model"])
			assert.EqualValues(t, 7, wire["duration"])
			assert.Equal(t, "480p", wire["quality"])
			assert.Equal(t, "21:9", wire["ratio"])
			assert.NotContains(t, wire, "mode", "mode is host admission state, not an undocumented upstream field")
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			assert.EqualValues(t, map[string]any{"seconds": int64(7), "video_count": int64(1), "resolution": "480p"}, value,
				"the channel's selected billing profile decides which quantity to use")
		})
	}
	for _, missing := range []string{"seconds", "resolution", "aspect_ratio"} {
		request := map[string]any{"prompt": "a video", "seconds": 7, "resolution": "480p", "aspect_ratio": "16:9"}
		delete(request, missing)
		_, err := decodeWolongVideoRequest(t, plugin, modelName, request)
		require.Error(t, err, "unknown models must not invent a default %s", missing)
	}
	t.Run("unresolved alias defers dimensions without inventing defaults", func(t *testing.T) {
		value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "public-alias", "operation": "create", "body": map[string]any{"kind": "json", "value": map[string]any{"prompt": "a video"}},
		})
		require.NoError(t, err)
		canonical := value.(map[string]any)["requestBody"].(map[string]any)
		for _, field := range []string{"seconds", "resolution", "aspect_ratio"} {
			assert.NotContains(t, canonical, field)
		}
		assert.NotContains(t, value, "action", "an unresolved alias must not choose provider-specific media semantics")
		_, err = decodeWolongVideoRequest(t, plugin, modelName, canonical)
		require.Error(t, err, "final decoding of an unknown upstream model remains strict")
		resolved, err := decodeWolongVideoRequest(t, plugin, "haya-mo-seedance-25", canonical)
		require.NoError(t, err)
		assert.EqualValues(t, 5, resolved["requestBody"].(map[string]any)["seconds"])
		ctx := map[string]any{"model": "public-alias", "requestBody": canonical, "publicTaskId": "video_public-task-1", "baseUrl": "https://upstream.example", "apiKey": "key"}
		for _, hook := range []string{"buildSubmitRequest", "extractUsage"} {
			_, err := plugin.Engine.Call(t.Context(), hook, ctx)
			require.Error(t, err, "%s must not dispatch or bill an unresolved alias", hook)
		}
	})
	t.Run("unresolved alias preserves reference roles for the actual model", func(t *testing.T) {
		request := map[string]any{"prompt": "a video", "input_reference": image, "mode": "reference_to_video"}
		value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "public/alias", "operation": "create", "body": map[string]any{"kind": "json", "value": request},
		})
		require.NoError(t, err)
		canonical := value.(map[string]any)["requestBody"].(map[string]any)
		assert.Equal(t, request, canonical)
		resolved, err := decodeWolongVideoRequest(t, plugin, "haya-mo-seedance-25", canonical)
		require.NoError(t, err)
		assert.Equal(t, "reference_to_video", resolved["action"])
		assert.Equal(t, []any{image}, resolved["requestBody"].(map[string]any)["reference_images"])
	})
	t.Run("built-in public name can map to a different upstream", func(t *testing.T) {
		request := map[string]any{"prompt": "a video", "seconds": 10}
		value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "czniu-seedance-20", "operation": "create", "body": map[string]any{"kind": "json", "value": request},
		})
		require.NoError(t, err)
		canonical := value.(map[string]any)["requestBody"].(map[string]any)
		assert.EqualValues(t, request, canonical)
		resolved, err := decodeWolongVideoRequest(t, plugin, "haya-mo-seedance-25", canonical)
		require.NoError(t, err)
		assert.EqualValues(t, 10, resolved["requestBody"].(map[string]any)["seconds"])
		_, err = decodeWolongVideoRequest(t, plugin, "czniu-seedance-20", canonical)
		require.Error(t, err, "the actual selected model still enforces its discrete durations")
	})
	for _, name := range []string{"vendor/new-video", strings.Repeat("a", 255), strings.Repeat("新", 85)} {
		_, err := decodeWolongVideoRequest(t, plugin, name, map[string]any{"prompt": "a video", "seconds": 5, "resolution": "720p", "aspect_ratio": "16:9"})
		require.NoError(t, err)
	}
	for _, name := range []string{" leading", "trailing ", "invalid,model", "wild*card", "control\x01model", strings.Repeat("新", 86)} {
		_, err := decodeWolongVideoRequest(t, plugin, name, map[string]any{"prompt": "a video", "seconds": 5, "resolution": "720p", "aspect_ratio": "16:9"})
		require.Error(t, err)
	}
	_, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": "", "requestBody": map[string]any{"prompt": "a video", "seconds": 5, "resolution": "720p", "aspect_ratio": "16:9"}})
	require.Error(t, err, "an empty machine model cannot be submitted")
	for _, tc := range []struct {
		name   string
		fields map[string]any
	}{
		{"too long", map[string]any{"seconds": 3601}},
		{"wrapped unsigned duration", map[string]any{"seconds": "18446744073686646784"}},
		{"text with references", map[string]any{"mode": "text_to_video", "reference_images": image}},
		{"image with video", map[string]any{"mode": "image_to_video", "image": image, "video_urls": []any{"https://cdn.example/reference.mp4"}}},
		{"frames require two", map[string]any{"mode": "first_last_frame", "image": image}},
		{"reference requires media", map[string]any{"mode": "reference_to_video"}},
		{"asset not supported", map[string]any{"asset_ids": []any{"asset_private"}}},
		{"invalid resolution", map[string]any{"resolution": "bad value"}},
		{"invalid ratio", map[string]any{"aspect_ratio": "0:9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"prompt": "a video", "seconds": 7, "resolution": "480p", "aspect_ratio": "16:9"}
			for key, value := range tc.fields {
				request[key] = value
			}
			_, err := decodeWolongVideoRequest(t, plugin, modelName, request)
			require.Error(t, err)
		})
	}
	_, err = decodeWolongVideoRequest(t, plugin, modelName, map[string]any{"prompt": "a video", "duration": 3600, "quality": "4k", "ratio": "auto"})
	require.NoError(t, err, "generic protocol limits do not replace channel capability validation")
	images := make([]any, 64)
	for i := range images {
		images[i] = image
	}
	request := map[string]any{"seconds": 5, "resolution": "720p", "aspect_ratio": "16:9", "image_urls": images}
	_, err = decodeWolongVideoRequest(t, plugin, modelName, request)
	require.NoError(t, err)
	request["video_urls"] = []any{"https://cdn.example/reference.mp4"}
	_, err = decodeWolongVideoRequest(t, plugin, modelName, request)
	require.Error(t, err, "the generic limit applies across all media kinds")
}

func TestWolongVideoMixedCandidatePreservesInputsAndDimensions(t *testing.T) {
	setting := `{"task_plugin_key":"wolong"}`
	mapping := `{"public-alias":"haya-mo-seedance-25"}`
	channel := model.Channel{Id: 42, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled,
		Key: "test-key", Models: "public-alias", Group: "default", Setting: &setting, ModelMapping: &mapping}
	raw := []byte(`{"model":"public-alias","prompt":"a video","seconds":10,"resolution":"720p","size":"16:9","input_reference":"https://cdn.example/image.png?sig=A%2FB","video_urls":["https://cdn.example/video.mp4"]}`)
	candidate, err := service.DecodeMixedVideoCandidate(t.Context(), channel, "public-alias", "create", raw, service.VideoModelOverrides{}, 0)
	require.NoError(t, err)
	assert.Equal(t, "wolong", candidate.Binding.Plugin.Meta.Key)
	assert.Equal(t, "haya-mo-seedance-25", candidate.UpstreamModel)
	assert.Equal(t, "reference_to_video", candidate.Intent["action"], "a generic input_reference has no undocumented first-frame role")
	canonical := candidate.Intent["requestBody"].(map[string]any)
	assert.Equal(t, []any{"https://cdn.example/image.png?sig=A%2FB"}, canonical["reference_images"])
	assert.Equal(t, []any{"https://cdn.example/video.mp4"}, canonical["reference_videos"])
	assert.EqualValues(t, 10, canonical["seconds"])
	assert.Equal(t, "720p", canonical["resolution"])
	assert.Equal(t, "16:9", canonical["aspect_ratio"])
	facts, err := service.MixedVideoPublicFacts(canonical, "create", nil)
	require.NoError(t, err)
	assert.Equal(t, "video", facts["video_input"])
}

func TestWolongVideoReceiptProgressRefundAndContent(t *testing.T) {
	plugin := loadWolongVideoPlugin(t)
	ctx := map[string]any{"taskId": "task_upstream-1", "baseUrl": "https://upstream.example/subpath", "apiKey": "key"}
	for _, status := range []string{"running", "succeeded", "failed", "needs_review"} {
		envelope := map[string]any{"ok": false, "task": map[string]any{"id": "task_upstream-1", "status": status, "progress": 51.9, "stage": "生成中"}}
		value, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"statusCode": 200, "body": envelope})
		require.NoError(t, err)
		result := value.(map[string]any)
		assert.Equal(t, "task_upstream-1", result["taskId"])
		assert.EqualValues(t, envelope, result["taskData"])
		assert.NotContains(t, result, "immediate")
	}
	for _, tc := range []struct {
		name, status, want, progress string
		fields                       map[string]any
	}{
		{"queued zero", "queued", "QUEUED", "0%", map[string]any{"progress": 0}},
		{"running decimal", "running", "IN_PROGRESS", "51%", map[string]any{"progress": 51.9}},
		{"review never completes", "needs_review", "IN_PROGRESS", "99%", map[string]any{"progress": 100, "error": "execution unknown"}},
		{"missing progress", "running", "IN_PROGRESS", "", nil},
		{"null progress", "running", "IN_PROGRESS", "", map[string]any{"progress": nil}},
		{"string progress", "running", "IN_PROGRESS", "", map[string]any{"progress": "40"}},
		{"out of bounds progress", "running", "IN_PROGRESS", "", map[string]any{"progress": 101}},
		{"refunded boolean", "failed", "FAILURE", "", map[string]any{"refunded": true}},
		{"refunded amount", "failed", "FAILURE", "", map[string]any{"charged_amount": 60, "refunded_amount": 60}},
		{"refund covers charge", "failed", "FAILURE", "", map[string]any{"charged_amount": 60, "refunded_amount": 61}},
		{"explicit zero charge", "canceled", "FAILURE", "", map[string]any{"charged_amount": 0}},
		{"refund missing", "failed", "UNKNOWN", "", nil},
		{"refund partial", "failed", "UNKNOWN", "", map[string]any{"charged_amount": 60, "refunded_amount": 30}},
		{"refund contradictory", "canceled", "UNKNOWN", "", map[string]any{"refunded": true, "charged_amount": 60, "refunded_amount": 0}},
		{"refund false conflicts", "failed", "UNKNOWN", "", map[string]any{"refunded": false, "charged_amount": 60, "refunded_amount": 60}},
		{"refund malformed", "failed", "UNKNOWN", "", map[string]any{"refunded": true, "charged_amount": "0"}},
		{"failure has output", "failed", "UNKNOWN", "", map[string]any{"refunded": true, "data": []any{map[string]any{"url": "https://cdn.example/video.mp4", "mime": "video/mp4"}}}},
		{"failure malformed outputs", "failed", "UNKNOWN", "", map[string]any{"refunded": true, "data": map[string]any{"url": "https://cdn.example/video.mp4"}}},
		{"success output", "succeeded", "SUCCESS", "100%", map[string]any{"data": []any{map[string]any{"url": "/media/outputs/movie.mp4?e=123&t=signed", "mime": "video/mp4"}}}},
		{"success no output", "succeeded", "UNKNOWN", "", nil},
		{"success image only", "succeeded", "UNKNOWN", "", map[string]any{"data": []any{map[string]any{"url": "https://cdn.example/image.png", "mime": "image/png"}}}},
		{"success conflicting error", "succeeded", "UNKNOWN", "", map[string]any{"error": "failed", "data": []any{map[string]any{"url": "https://cdn.example/video.mp4", "mime": "video/mp4"}}}},
		{"success private result", "succeeded", "UNKNOWN", "", map[string]any{"data": []any{map[string]any{"url": "http://127.0.0.1/movie.mp4", "mime": "video/mp4"}}}},
		{"new status", "new-status", "UNKNOWN", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := map[string]any{"id": "task_upstream-1", "status": tc.status}
			for name, value := range tc.fields {
				task[name] = value
			}
			value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, map[string]any{"ok": true, "task": task})
			require.NoError(t, err)
			result := value.(map[string]any)
			assert.Equal(t, tc.want, result["status"])
			if tc.progress == "" {
				assert.NotContains(t, result, "progress")
			} else {
				assert.Equal(t, tc.progress, result["progress"])
			}
			if tc.want == "SUCCESS" {
				assert.Equal(t, "https://upstream.example/media/outputs/movie.mp4?e=123&t=signed", result["url"])
			}
		})
	}
	value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, map[string]any{"ok": true, "task": map[string]any{"id": "other-task", "status": "failed", "refunded": true}})
	require.NoError(t, err)
	assert.Equal(t, "UNKNOWN", value.(map[string]any)["status"])
	value, err = plugin.Engine.Call(t.Context(), "buildQueryRequest", ctx)
	require.NoError(t, err)
	assert.Equal(t, "https://upstream.example/subpath/v1/tasks/task_upstream-1", value.(map[string]any)["url"])
	adaptor := taskplugin.New(plugin)
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: "https://upstream.example/subpath", ApiKey: "secret-key"}})
	data, err := common.Marshal(map[string]any{"ok": true, "task": map[string]any{"id": "task_upstream-1", "status": "succeeded", "data": []any{map[string]any{"url": "/media/video.mp4?token=preserved", "mime": "video/mp4"}}}})
	require.NoError(t, err)
	task := &model.Task{TaskID: "video_public-task-1", Status: model.TaskStatusSuccess, Data: data, PrivateData: model.TaskPrivateData{UpstreamTaskID: "task_upstream-1"}}
	artifacts, err := adaptor.ListArtifacts(task)
	require.NoError(t, err)
	assert.Equal(t, []channel.TaskArtifact{{Key: "video", Type: "video", MimeType: "video/mp4"}}, artifacts)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		descriptor, err := adaptor.BuildContentRequest(task, "video", channel.TaskArtifactClientRequest{Method: method, Headers: map[string]string{"Range": "bytes=0-1023"}})
		require.NoError(t, err)
		assert.Equal(t, "https://upstream.example/media/video.mp4?token=preserved", descriptor.URL)
		assert.Equal(t, method, descriptor.Method)
		assert.True(t, descriptor.Credentialless)
		assert.NotContains(t, descriptor.Headers, "Authorization")
	}
}
