package plugins_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay/channel"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadYuxiboxVideoPlugin(t *testing.T, now func() time.Time) *jsplugin.LoadedPlugin {
	t.Helper()
	source, err := builtinplugins.Source("yuxibox")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "yuxibox", Now: now})
	require.NoError(t, err)
	return plugin
}

func decodeYuxiboxVideoRequest(t *testing.T, plugin *jsplugin.LoadedPlugin, modelName string, request map[string]any) (map[string]any, error) {
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

func TestYuxiboxVideoModelsDefaultsAndBilling(t *testing.T) {
	plugin := loadYuxiboxVideoPlugin(t, nil)
	assert.Empty(t, plugin.Meta.ChannelTypes)
	assert.Equal(t, "https://yuxibox.xyz", plugin.Meta.BaseURL)
	assert.Equal(t, "1.0.2", plugin.Meta.Version)
	models := []struct {
		name, resolution, quantity string
		seconds                    int
		imageRequired              bool
	}{
		{"seedance-2.0-mini-yx-g", "720p", "video_count", 15, false},
		{"seedance-2.0-mini-yx-g-no-face", "720p", "video_count", 15, false},
		{"seedance-2.0-fast-yx-g", "720p", "video_count", 15, false},
		{"seedance-2.0-fast-yx-g-no-face", "720p", "video_count", 15, false},
		{"seedance-2.0-yx-g", "720p", "video_count", 15, false},
		{"seedance-2.0-yx-g-no-face", "720p", "video_count", 15, false},
		{"seedance-2.5-yx-g", "720p", "video_count", 30, false},
		{"seedance-2.5-yx-g-no-face", "720p", "video_count", 30, false},
		{"seedance-2.5-yx-sz", "720p", "seconds", 5, false},
		{"seedance2.5-yx-count", "720p", "video_count", 30, false},
		{"minimax-h3-quality", "768p", "seconds", 5, true},
		{"minimax-h3-fast", "768p", "seconds", 5, true},
		{"minimax-h3-15s", "768p", "seconds", 5, false},
	}
	wantModels := make([]string, 0, len(models))
	for _, tc := range models {
		wantModels = append(wantModels, tc.name)
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"prompt": "a blue glass cube"}
			if tc.imageRequired {
				request["reference_images"] = []any{"https://cdn.example/image.png"}
			}
			intent, err := decodeYuxiboxVideoRequest(t, plugin, tc.name, request)
			require.NoError(t, err)
			assert.Equal(t, "public-alias", intent["model"])
			canonical := intent["requestBody"].(map[string]any)
			assert.EqualValues(t, tc.seconds, canonical["seconds"])
			assert.Equal(t, tc.resolution, canonical["resolution"])
			profile, found := plugin.Meta.VideoProfileForModel(tc.name)
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, intent["action"].(string), canonical, nil))
			again, err := decodeYuxiboxVideoRequest(t, plugin, tc.name, canonical)
			require.NoError(t, err)
			assert.Equal(t, intent, again, "decoding the canonical request must remain stable")
			ctx := map[string]any{"model": "public-alias", "upstreamModel": tc.name, "requestBody": canonical, "baseUrl": "https://upstream.example", "apiKey": "provider-key"}
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, err)
			descriptor := value.(map[string]any)
			assert.Equal(t, "https://upstream.example/v1/videos", descriptor["url"])
			assert.Equal(t, http.MethodPost, descriptor["method"])
			assert.NotContains(t, descriptor, "model")
			assert.Equal(t, "Bearer provider-key", descriptor["headers"].(map[string]any)["Authorization"])
			body := descriptor["body"].(map[string]any)
			assert.Equal(t, tc.name, body["model"])
			assert.EqualValues(t, tc.seconds, body["seconds"])
			if tc.name == "seedance2.5-yx-count" {
				assert.NotContains(t, body, "resolution")
			} else {
				assert.Equal(t, tc.resolution, body["resolution"])
			}
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			facts := value.(map[string]any)
			quantity := 1
			if tc.quantity == "seconds" {
				quantity = tc.seconds
			}
			assert.EqualValues(t, quantity, facts[tc.quantity])
			schema, _ := plugin.Meta.UsageForModel(tc.name)
			assert.Len(t, facts, len(schema))
			ctx["usagePurpose"] = "billing_ratios"
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			assert.Nil(t, value, "usage expressions own the quantity without a second legacy multiplier")
		})
	}
	assert.ElementsMatch(t, wantModels, plugin.Meta.Models)
}

func TestYuxiboxVideoReferencesAndSizeAliases(t *testing.T) {
	plugin := loadYuxiboxVideoPlugin(t, nil)
	signedImage := "https://cdn.example/image.png?signature=A%2FB&expires=2000000000"
	for _, tc := range []struct {
		name, modelName, action, resolution, ratio string
		request, wire                              map[string]any
	}{
		{name: "ordered duplicate references", modelName: "seedance-2.5-yx-sz", action: "reference_to_video", resolution: "720p", ratio: "16:9",
			request: map[string]any{"reference_images": []any{signedImage, signedImage}, "reference_videos": []any{"https://cdn.example/video.mp4"}, "reference_audios": []any{"https://cdn.example/audio.mp3"}, "watermark": false, "prompt_extend": false, "generate_audio": true},
			wire:    map[string]any{"reference_images": []any{signedImage, signedImage}, "videos": []any{"https://cdn.example/video.mp4"}, "audios": []any{"https://cdn.example/audio.mp3"}, "watermark": false, "prompt_extend": false}},
		{name: "empty ordinary reference role", modelName: "seedance-2.5-yx-sz", action: "reference_to_video", resolution: "720p", ratio: "16:9",
			request: map[string]any{"references": []any{map[string]any{"type": "image", "role": "", "source": signedImage}}},
			wire:    map[string]any{"reference_images": []any{signedImage}}},
		{name: "paired frames", modelName: "seedance-2.0-yx-g", action: "first_last_frame", resolution: "720p", ratio: "16:9",
			request: map[string]any{"input_reference": signedImage, "last_frame": "https://cdn.example/last.png"},
			wire:    map[string]any{"first_frame": signedImage, "last_frame": "https://cdn.example/last.png"}},
		{name: "count pixel dimensions", modelName: "seedance2.5-yx-count", action: "reference_to_video", resolution: "720p", ratio: "9:16",
			request: map[string]any{"input_reference": signedImage, "size": "720x1280"}, wire: map[string]any{"reference_images": []any{signedImage}}},
		{name: "h3 alias and explicit zero seed", modelName: "minimax-h3-fast", action: "reference_to_video", resolution: "768p", ratio: "9:16",
			request: map[string]any{"input_reference": signedImage, "resolution": "720p", "seed": 0}, wire: map[string]any{"reference_images": []any{signedImage}, "seed": 0}},
		{name: "4k pixel dimensions", modelName: "seedance-2.0-yx-g", action: "text_to_video", resolution: "4k", ratio: "16:9", request: map[string]any{"size": "3840x2160"}},
		{name: "adaptive ratio", modelName: "seedance-2.5-yx-g", action: "text_to_video", resolution: "720p", ratio: "adaptive", request: map[string]any{"ratio": "adaptive"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"prompt": "preserve image identity"}
			for key, value := range tc.request {
				request[key] = value
			}
			intent, err := decodeYuxiboxVideoRequest(t, plugin, tc.modelName, request)
			require.NoError(t, err)
			assert.Equal(t, tc.action, intent["action"])
			profile, found := plugin.Meta.VideoProfileForModel(tc.modelName)
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, tc.action, intent["requestBody"], nil))
			if tc.request["generate_audio"] == true {
				for _, mode := range profile.Modes {
					assert.False(t, mode.Options.GenerateAudio, "a fixed enabled setting must not make the workbench submit a default false toggle")
				}
				assert.NotContains(t, intent["requestBody"], "generate_audio")
			}
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": "public-alias", "upstreamModel": tc.modelName, "requestBody": intent["requestBody"], "baseUrl": "https://upstream.example", "apiKey": "key"})
			require.NoError(t, err)
			body := value.(map[string]any)["body"].(map[string]any)
			assert.Equal(t, tc.ratio, body["ratio"])
			if tc.modelName != "seedance2.5-yx-count" {
				assert.Equal(t, tc.resolution, body["resolution"])
			}
			for key, want := range tc.wire {
				assert.EqualValues(t, want, body[key], key)
			}
			assert.NotContains(t, body, "reference_videos")
			assert.NotContains(t, body, "reference_audios")
		})
	}
}

func TestYuxiboxVideoWorkbenchSingleReferenceControls(t *testing.T) {
	plugin := loadYuxiboxVideoPlugin(t, nil)
	for _, sample := range []struct{ input, wire, alias, url string }{
		{"reference_images", "reference_images", "images", "https://cdn.example/image.png?signature=unchanged"},
		{"reference_videos", "videos", "videos", "https://cdn.example/video.mp4"},
		{"reference_audios", "audios", "audios", "https://cdn.example/audio.mp3"},
	} {
		t.Run(sample.input, func(t *testing.T) {
			// The shared workbench emits a string when an administrator narrows
			// an input to maxItems=1. Decode it before applying that capability.
			intent, err := decodeYuxiboxVideoRequest(t, plugin, "seedance-2.5-yx-sz", map[string]any{"prompt": "a video", sample.input: sample.url})
			require.NoError(t, err)
			canonical := intent["requestBody"].(map[string]any)
			assert.Equal(t, []any{sample.url}, canonical[sample.input])
			profile, found := plugin.Meta.VideoProfileForModel("seedance-2.5-yx-sz")
			require.True(t, found)
			for _, mode := range profile.Modes {
				if mode.Name != "reference_to_video" {
					continue
				}
				mode.Inputs = append([]jsplugin.VideoInputProfile(nil), mode.Inputs...)
				for i := range mode.Inputs {
					if mode.Inputs[i].Name == sample.input {
						mode.Inputs[i].MaxItems = 1
					}
				}
				profile.Modes = []jsplugin.VideoModeProfile{mode}
				break
			}
			require.NoError(t, service.ValidateVideoProfileRequest(profile, "reference_to_video", canonical, nil))
			again, err := decodeYuxiboxVideoRequest(t, plugin, "seedance-2.5-yx-sz", canonical)
			require.NoError(t, err)
			assert.Equal(t, intent, again)
			equivalent, err := decodeYuxiboxVideoRequest(t, plugin, "seedance-2.5-yx-sz", map[string]any{"prompt": "a video", sample.input: sample.url, sample.alias: []any{sample.url}})
			require.NoError(t, err)
			assert.Equal(t, intent, equivalent, "equivalent aliases must normalize before comparison")
			_, err = decodeYuxiboxVideoRequest(t, plugin, "seedance-2.5-yx-sz", map[string]any{"prompt": "a video", sample.input: sample.url, sample.alias: []any{sample.url + "&different=true"}})
			require.Error(t, err, "conflicting media aliases remain invalid")
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": "public-alias", "upstreamModel": "seedance-2.5-yx-sz", "requestBody": canonical, "baseUrl": "https://upstream.example", "apiKey": "key"})
			require.NoError(t, err)
			assert.Equal(t, []any{sample.url}, value.(map[string]any)["body"].(map[string]any)[sample.wire])
		})
	}
}

func TestYuxiboxVideoRejectsInvalidInputs(t *testing.T) {
	plugin := loadYuxiboxVideoPlugin(t, nil)
	images := make([]any, 31)
	for i := range images {
		images[i] = "https://cdn.example/reference.png"
	}
	for _, tc := range []struct {
		name, modelName string
		fields          map[string]any
	}{
		{name: "custom model requires explicit dimensions", modelName: "seedance-unlisted"},
		{name: "blank prompt", fields: map[string]any{"prompt": " "}},
		{name: "long prompt", fields: map[string]any{"prompt": strings.Repeat("字", 16001)}},
		{name: "zero duration", fields: map[string]any{"seconds": 0}},
		{name: "negative duration", fields: map[string]any{"seconds": -1}},
		{name: "fractional duration", fields: map[string]any{"seconds": 5.5}},
		{name: "boolean duration", fields: map[string]any{"seconds": true}},
		{name: "wrapped uint duration", fields: map[string]any{"seconds": "18446744073686646784"}},
		{name: "duration over model bound", fields: map[string]any{"seconds": 31}},
		{name: "conflicting seconds", fields: map[string]any{"seconds": 5, "duration": 6}},
		{name: "conflicting ratio", fields: map[string]any{"ratio": "16:9", "aspect_ratio": "9:16"}},
		{name: "size ratio conflict", fields: map[string]any{"size": "1280x720", "ratio": "9:16"}},
		{name: "size resolution conflict", fields: map[string]any{"size": "1280x720", "resolution": "1080p"}},
		{name: "unsupported mini resolution", modelName: "seedance-2.0-mini-yx-g", fields: map[string]any{"resolution": "1080p"}},
		{name: "sz adaptive", fields: map[string]any{"ratio": "adaptive"}},
		{name: "metadata bypass", fields: map[string]any{"metadata": map[string]any{"seconds": 99999}}},
		{name: "batch bypass", fields: map[string]any{"n": 2}},
		{name: "audio disabled", fields: map[string]any{"generate_audio": false}},
		{name: "watermark enabled", fields: map[string]any{"watermark": true}},
		{name: "seedance seed", fields: map[string]any{"seed": 0}},
		{name: "unpaired first frame", fields: map[string]any{"image": "https://cdn.example/first.png"}},
		{name: "unpaired last frame", fields: map[string]any{"last_frame": "https://cdn.example/last.png"}},
		{name: "mixed frames and references", fields: map[string]any{"first_frame": "https://cdn.example/first.png", "last_frame": "https://cdn.example/last.png", "reference_audios": []any{"https://cdn.example/audio.mp3"}}},
		{name: "too many references", fields: map[string]any{"reference_images": images}},
		{name: "count aggregate limit", modelName: "seedance2.5-yx-count", fields: map[string]any{"reference_images": images[:30], "audios": []any{"https://cdn.example/audio.mp3"}}},
		{name: "count lower duration", modelName: "seedance2.5-yx-count", fields: map[string]any{"seconds": 4}},
		{name: "h3 quality requires image", modelName: "minimax-h3-quality"},
		{name: "h3 fast requires image", modelName: "minimax-h3-fast"},
		{name: "h3 video", modelName: "minimax-h3-15s", fields: map[string]any{"videos": []any{"https://cdn.example/video.mp4"}}},
		{name: "h3 unsupported ratio", modelName: "minimax-h3-15s", fields: map[string]any{"ratio": "1:1"}},
		{name: "h3 fractional seed", modelName: "minimax-h3-15s", fields: map[string]any{"seed": 0.5}},
		{name: "http reference", fields: map[string]any{"reference_images": []any{"http://cdn.example/image.png"}}},
		{name: "private reference", fields: map[string]any{"reference_images": []any{"https://127.0.0.1/image.png"}}},
		{name: "private ipv6", fields: map[string]any{"reference_images": []any{"https://[::1]/image.png"}}},
		{name: "credential reference", fields: map[string]any{"reference_images": []any{"https://user:secret@cdn.example/image.png"}}},
		{name: "data reference", fields: map[string]any{"reference_images": []any{"data:image/png;base64,YQ=="}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modelName := tc.modelName
			if modelName == "" {
				modelName = "seedance-2.5-yx-sz"
			}
			request := map[string]any{"prompt": "a video"}
			for key, value := range tc.fields {
				request[key] = value
			}
			_, err := decodeYuxiboxVideoRequest(t, plugin, modelName, request)
			require.Error(t, err)
		})
	}
	for _, kind := range []string{"multipart", "form"} {
		_, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{"model": "seedance-2.5-yx-sz", "operation": "create", "body": map[string]any{"kind": kind}})
		require.Error(t, err)
	}
	for _, operation := range []string{"edit", "extend"} {
		_, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{"model": "seedance-2.5-yx-sz", "operation": operation, "body": map[string]any{"kind": "json", "value": map[string]any{"prompt": "a video"}}})
		require.Error(t, err)
	}
}

func TestYuxiboxVideoBillingPeriodStaysFrozen(t *testing.T) {
	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	for _, tc := range []struct {
		hour, minute, second int
		want                 string
	}{
		{0, 0, 0, "off_peak"}, {7, 59, 59, "off_peak"}, {8, 0, 0, "peak"}, {23, 59, 59, "peak"},
	} {
		t.Run(tc.want+time.Date(2026, 11, 1, tc.hour, tc.minute, tc.second, 0, shanghai).Format("15:04:05"), func(t *testing.T) {
			now := time.Date(2026, 11, 1, tc.hour, tc.minute, tc.second, 0, shanghai)
			plugin := loadYuxiboxVideoPlugin(t, func() time.Time { return now })
			intent, err := decodeYuxiboxVideoRequest(t, plugin, "minimax-h3-15s", map[string]any{"prompt": "a video"})
			require.NoError(t, err)
			value, err := plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{"model": "minimax-h3-15s", "requestBody": intent["requestBody"]})
			require.NoError(t, err)
			facts := value.(map[string]any)
			assert.Equal(t, tc.want, facts["billing_period"])
			now = now.Add(12 * time.Hour)
			completion, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", map[string]any{"model": "minimax-h3-15s"}, map[string]any{"status": "SUCCESS"}, map[string]any{"status": "completed", "seconds": 999999})
			require.NoError(t, err)
			assert.Empty(t, completion, "completion must preserve the frozen requested quantity and submission period")
			const expression = `u("billing_period") == "off_peak" ? tier("off_peak", u("seconds") * 0.1) : tier("peak", u("seconds") * 0.2)`
			snapshot := &billingexpr.BillingSnapshot{ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), ExprVersion: 1, TaskUsageBilling: true, UsageFacts: facts, GroupRatio: 1, QuotaPerUnit: 500000}
			settled, usage, err := service.EvaluateTaskCompletionUsage(snapshot, completion.(map[string]any))
			require.NoError(t, err)
			wantQuota := 500000
			if tc.want == "off_peak" {
				wantQuota = 250000
			}
			assert.Equal(t, wantQuota, settled.ActualQuotaAfterGroup)
			assert.Equal(t, tc.want, usage["billing_period"])
		})
	}
}

func TestYuxiboxVideoReceiptPollingAndAuthenticatedContent(t *testing.T) {
	plugin := loadYuxiboxVideoPlugin(t, nil)
	ctx := map[string]any{"model": "seedance-2.5-yx-sz", "taskId": "upstream-id", "baseUrl": "https://upstream.example", "apiKey": "original-key"}
	for _, status := range []int{200, 202, 502} {
		value, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"status": status, "body": map[string]any{"id": "upstream-id", "status": "queued", "error": map[string]any{"message": "receipt incomplete"}}})
		require.NoError(t, err)
		assert.Equal(t, "upstream-id", value.(map[string]any)["taskId"])
		assert.NotContains(t, value, "immediate")
	}
	for _, tc := range []struct{ status, stage, want string }{
		{"queued", "", "QUEUED"}, {"in_progress", "", "IN_PROGRESS"},
		{"in_progress", "submission_pending", "IN_PROGRESS"}, {"in_progress", "artifact_importing", "IN_PROGRESS"},
		{"completed", "", "SUCCESS"}, {"failed", "", "FAILURE"}, {"new-status", "", "UNKNOWN"}, {"toString", "", "UNKNOWN"},
	} {
		t.Run(tc.status+tc.stage, func(t *testing.T) {
			value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, map[string]any{"id": "upstream-id", "status": tc.status, "stage": tc.stage, "progress": 50})
			require.NoError(t, err)
			assert.Equal(t, tc.want, value.(map[string]any)["status"])
		})
	}
	for _, body := range []map[string]any{
		{"status": "completed"},
		{"status": "failed"},
		{"id": "other-task", "status": "completed"},
		{"id": "upstream-id", "status": []any{"failed"}},
		{"id": "upstream-id", "status": "completed", "stage": "artifact_importing"},
		{"id": "upstream-id", "status": "completed", "error": map[string]any{"message": "generation failed"}},
	} {
		value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
		if err == nil {
			assert.Equal(t, "UNKNOWN", value.(map[string]any)["status"])
		}
	}
	for _, field := range []string{"url", "video_url", "download_url", "result_url", "volcesVideoUrl"} {
		value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, map[string]any{"id": "upstream-id", "status": "failed", field: "https://cdn.example/completed.mp4"})
		require.NoError(t, err)
		assert.Equal(t, "UNKNOWN", value.(map[string]any)["status"], "an upstream output cannot authorize an automatic refund")
	}
	value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, map[string]any{"id": "upstream-id", "status": "failed", "data": map[string]any{"creations": []any{map[string]any{"url": "https://cdn.example/completed.mp4"}}}})
	require.NoError(t, err)
	assert.Equal(t, "UNKNOWN", value.(map[string]any)["status"])
	for _, id := range []any{"", ".", "..", " leading", "bad\nid", strings.Repeat("x", 192), 1, nil} {
		_, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": map[string]any{"id": id}})
		require.Error(t, err)
	}
	value, err = plugin.Engine.Call(t.Context(), "buildQueryRequest", ctx)
	require.NoError(t, err)
	assert.Equal(t, "https://upstream.example/v1/videos/upstream-id", value.(map[string]any)["url"])
	adaptor := taskplugin.New(plugin)
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: "https://upstream.example", ApiKey: "new-key"}})
	data, err := common.Marshal(map[string]any{"id": "upstream-id", "status": "completed", "video_url": "https://untrusted.example/file.mp4"})
	require.NoError(t, err)
	task := &model.Task{TaskID: "public-task", Status: model.TaskStatusSuccess, Data: data, Properties: model.Properties{OriginModelName: "seedance-2.5-yx-sz"}, PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-id", Key: "original-key"}}
	artifacts, err := adaptor.ListArtifacts(task)
	require.NoError(t, err)
	assert.Equal(t, []channel.TaskArtifact{{Key: "video", Type: "video", MimeType: "video/mp4"}}, artifacts)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		descriptor, err := adaptor.BuildContentRequest(task, "video", channel.TaskArtifactClientRequest{Method: method, Headers: map[string]string{"Range": "bytes=0-1023"}})
		require.NoError(t, err)
		assert.Equal(t, "https://upstream.example/v1/videos/upstream-id/content", descriptor.URL)
		assert.Equal(t, method, descriptor.Method)
		assert.Equal(t, "Bearer original-key", descriptor.Headers["Authorization"])
		assert.False(t, descriptor.Credentialless)
	}
}
