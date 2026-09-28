package plugins_test

import (
	"maps"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoResponsesProtocol(t *testing.T) {
	testVideoResponsesProtocol(t, videoResponsesTestCase{
		pluginKey: "doubao",
		model:     "doubao-seedance-2-0-260128",
		requestBody: map[string]any{
			"model": "doubao-seedance-2-0-260128",
			"input": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "a running fox"},
				map[string]any{"type": "input_image", "image_url": "https://cdn.example/frame.png"},
			}}},
			"seconds": 6,
			"size":    "1920x1080",
		},
		wantAction: "image_to_video",
		wantRequest: map[string]any{
			"model":   "doubao-seedance-2-0-260128",
			"prompt":  "a running fox",
			"images":  []any{"https://cdn.example/frame.png"},
			"seconds": float64(6),
			"metadata": map[string]any{
				"resolution": "1080p",
			},
		},
		wantUsageKeys:  []string{"resolution", "seconds", "tokens", "video_input"},
		wantVendorName: "doubao",
	})
}

func TestDoubaoVideoUsageBilling(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	registry := jsplugin.NewRegistry()
	plugin, err := registry.RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)

	usage := func(t *testing.T, hook string, args ...any) map[string]any {
		t.Helper()
		value, callErr := plugin.Engine.Call(t.Context(), hook, args...)
		require.NoError(t, callErr)
		encoded, marshalErr := common.Marshal(value)
		require.NoError(t, marshalErr)
		var facts map[string]any
		require.NoError(t, common.Unmarshal(encoded, &facts))
		return facts
	}

	for _, tc := range []struct {
		name, model, action string
		request             map[string]any
		seconds, tokens     float64
	}{
		{name: "omitted duration keeps five second default", request: map[string]any{}, seconds: 5, tokens: 108000},
		{name: "request seconds expose output duration", request: map[string]any{"seconds": 10}, seconds: 10, tokens: 216000},
		{name: "native metadata duration exposes output duration", request: map[string]any{"metadata": map[string]any{"duration": 8}}, seconds: 8, tokens: 172800},
		{name: "native frames keep fractional output duration and legacy token estimate", model: "doubao-seedance-1-0-pro-250528", request: map[string]any{"metadata": map[string]any{"frames": 57}}, seconds: 2.375, tokens: 43200},
		{name: "native frames take precedence for second pricing", model: "doubao-seedance-1-0-pro-250528", request: map[string]any{"seconds": 5, "metadata": map[string]any{"frames": 241}}, seconds: 241.0 / 24, tokens: 108000},
		{name: "editing excludes input reservation from output seconds", action: "edit_video", request: map[string]any{"duration": 8, "video": "https://cdn.example/input.mp4"}, seconds: 8, tokens: 496800},
		{name: "extension excludes input reservation from output seconds", action: "extend_video", request: map[string]any{"duration": 5, "video": "https://cdn.example/input.mp4"}, seconds: 5, tokens: 432000},
		{name: "mapped Seedance 2.5 editing reserves thirty output seconds", model: "doubao-seedance-2-5-260628", action: "edit_video", request: map[string]any{"video": "asset://input-video"}, seconds: 30, tokens: 1296000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			if model == "" {
				model = "doubao-seedance-2-0-260128"
			}
			facts := usage(t, "extractUsage", map[string]any{
				"model": "client-alias", "upstreamModel": model, "action": tc.action, "requestBody": tc.request,
			})
			assert.Equal(t, tc.seconds, facts["seconds"])
			assert.Equal(t, tc.tokens, facts["tokens"])
		})
	}

	for _, tc := range []struct {
		name    string
		body    map[string]any
		seconds float64
	}{
		{name: "successful duration is measured in seconds", body: map[string]any{"duration": 6}, seconds: 6},
		{name: "numeric duration string preserves fractional seconds", body: map[string]any{"duration": "6.5"}, seconds: 6.5},
		{name: "frames and frame rate preserve fractional seconds", body: map[string]any{"frames": 57, "framespersecond": 24}, seconds: 2.375},
		{name: "missing duration retains estimate", body: map[string]any{}},
		{name: "negative duration retains estimate", body: map[string]any{"duration": -1}},
		{name: "zero duration retains estimate", body: map[string]any{"duration": 0}},
		{name: "nonfinite duration retains estimate", body: map[string]any{"duration": "Infinity"}},
		{name: "invalid numeric duration retains estimate", body: map[string]any{"duration": "NaN"}},
		{name: "over limit duration retains estimate", body: map[string]any{"duration": 3601}},
		{name: "boolean duration does not become one second", body: map[string]any{"duration": true}},
		{name: "empty duration does not become zero seconds", body: map[string]any{"duration": ""}},
		{name: "zero frame rate retains estimate", body: map[string]any{"frames": 120, "framespersecond": 0}},
		{name: "over limit frame duration retains estimate", body: map[string]any{"frames": 86401, "framespersecond": 24}},
		{name: "nonterminal response contributes no measured duration", body: map[string]any{"status": "running", "duration": 6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"status": "succeeded", "usage": map[string]any{"completion_tokens": 216000}}
			maps.Copy(body, tc.body)
			facts := usage(t, "extractUsageOnComplete", map[string]any{}, map[string]any{}, body)
			if tc.seconds == 0 {
				assert.NotContains(t, facts, "seconds")
			} else {
				assert.Equal(t, tc.seconds, facts["seconds"])
			}
			if body["status"] == "succeeded" {
				assert.Equal(t, float64(216000), facts["tokens"])
			}
		})
	}

	submitted := usage(t, "extractUsage", map[string]any{
		"model": "doubao-seedance-2-0-260128", "requestBody": map[string]any{"seconds": 5},
	})
	completed := usage(t, "extractUsageOnComplete", map[string]any{}, map[string]any{}, map[string]any{
		"status": "succeeded", "duration": 6, "usage": map[string]any{"completion_tokens": 216000},
	})
	for _, tc := range []struct {
		name, expression string
		estimated, final int
	}{
		{name: "second pricing settles actual duration", expression: `tier("base", u("seconds") * 0.10)`, estimated: 1000, final: 1200},
		{name: "per task pricing ignores duration and tokens", expression: `tier("base", 0.50)`, estimated: 1000, final: 1000},
		{name: "existing token pricing settles actual tokens", expression: `tier("base", u("tokens") * 10 / 1000000)`, estimated: 2160, final: 4320},
		{name: "explicit zero remains free", expression: `tier("base", u("seconds") * 0)`, estimated: 0, final: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, billing_setting.SmokeTestTaskExpr(tc.expression, plugin.Meta.UsageSchema))
			snap := &billingexpr.BillingSnapshot{
				ExprString: tc.expression, ExprHash: billingexpr.ExprHashString(tc.expression),
				ExprVersion: billingexpr.ExprVersion(tc.expression), TaskUsageBilling: true,
				QuotaPerUnit: 1000, GroupRatio: 2, UsageFacts: submitted,
			}
			estimate, err := billingexpr.ComputeTieredQuotaWithRequest(snap, billingexpr.TokenParams{}, billingexpr.RequestInput{Usage: submitted})
			require.NoError(t, err)
			assert.Equal(t, tc.estimated, estimate.ActualQuotaAfterGroup)
			settled, facts, err := service.EvaluateTaskCompletionUsage(snap, completed)
			require.NoError(t, err)
			assert.Equal(t, tc.final, settled.ActualQuotaAfterGroup)
			assert.Equal(t, float64(6), facts["seconds"])
			assert.Equal(t, float64(5), snap.UsageFacts["seconds"])
			retained, _, err := service.EvaluateTaskCompletionUsage(snap, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.estimated, retained.ActualQuotaAfterGroup)
		})
	}
}

func TestDoubaoVideoReferencesAndProfiles(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	registry := jsplugin.NewRegistry()
	plugin, err := registry.RegisterFactory(source, jsplugin.Options{Key: "doubao"})
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
			"model": model, "upstreamModel": model, "body": map[string]any{"kind": "json", "value": request},
		})
		if callErr != nil {
			return nil, callErr
		}
		return object(t, value), nil
	}
	build := func(t *testing.T, model string, resolved map[string]any) (map[string]any, error) {
		t.Helper()
		value, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
			"model": model, "upstreamModel": model, "baseUrl": "https://ark.example", "apiKey": "secret",
			"action": resolved["action"], "requestBody": resolved["requestBody"],
		})
		if callErr != nil {
			return nil, callErr
		}
		return object(t, value), nil
	}
	decodeOperation := func(t *testing.T, operation, model string, request map[string]any) (map[string]any, error) {
		t.Helper()
		value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"operation": operation, "model": model, "upstreamModel": model, "body": map[string]any{"kind": "json", "value": request},
		})
		if callErr != nil {
			return nil, callErr
		}
		return object(t, value), nil
	}

	t.Run("top-level input reference reaches Ark content", func(t *testing.T) {
		resolved, callErr := decode(t, "doubao-seedance-2-0-260128", map[string]any{
			"prompt": "animate", "input_reference": "https://cdn.example/first.png", "duration": 5, "resolution": "720p",
		})
		require.NoError(t, callErr)
		assert.Equal(t, "image_to_video", resolved["action"])
		descriptor, callErr := build(t, "doubao-seedance-2-0-260128", resolved)
		require.NoError(t, callErr)
		content := descriptor["body"].(map[string]any)["content"].([]any)
		require.Len(t, content, 2)
		image := content[0].(map[string]any)
		assert.Equal(t, "first_frame", image["role"])
		assert.Equal(t, "https://cdn.example/first.png", image["image_url"].(map[string]any)["url"])
	})

	t.Run("legacy native image content remains image to video", func(t *testing.T) {
		resolved, callErr := decode(t, "doubao-seedance-1-0-pro-250528", map[string]any{
			"prompt": "animate", "duration": 5, "resolution": "720p",
			"metadata": map[string]any{"content": []any{map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": "https://cdn.example/legacy.png"},
			}}},
		})
		require.NoError(t, callErr)
		assert.Equal(t, "image_to_video", resolved["action"])
		descriptor, callErr := build(t, "doubao-seedance-1-0-pro-250528", resolved)
		require.NoError(t, callErr)
		content := descriptor["body"].(map[string]any)["content"].([]any)
		require.Len(t, content, 2)
		assert.Equal(t, "https://cdn.example/legacy.png", content[0].(map[string]any)["image_url"].(map[string]any)["url"])
	})

	t.Run("multimodal arrays and native content are combined", func(t *testing.T) {
		resolved, callErr := decode(t, "doubao-seedance-2-5-260628", map[string]any{
			"prompt": "combine references", "duration": 30,
			"reference_images": []any{"https://cdn.example/ref.png"},
			"reference_videos": []any{"asset://video-id"},
			"reference_audios": []any{"https://cdn.example/audio.mp3"},
			"metadata": map[string]any{"content": []any{map[string]any{
				"type": "image_url", "role": "reference_image", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}, "tag": "native",
			}}},
		})
		require.NoError(t, callErr)
		assert.Equal(t, "reference_to_video", resolved["action"])
		descriptor, callErr := build(t, "doubao-seedance-2-5-260628", resolved)
		require.NoError(t, callErr)
		content := descriptor["body"].(map[string]any)["content"].([]any)
		require.Len(t, content, 5)
		assert.Equal(t, "native", content[3].(map[string]any)["tag"])
	})

	t.Run("editing and extension preserve the main video ordering", func(t *testing.T) {
		for _, model := range []string{"doubao-seedance-2-0-260128", "doubao-seedance-2-0-fast-260128", "doubao-seedance-2-0-mini-260615", "doubao-seedance-2-5-260628"} {
			for _, path := range []string{"/v1/videos/edits", "/v1/videos/extensions"} {
				binding, found := registry.Generation().LookupEndpoint("POST", path, model)
				require.True(t, found, model+path)
				assert.Same(t, plugin, binding.Plugin)
			}
		}
		_, found := registry.Generation().LookupEndpoint("POST", "/v1/videos/edits", "doubao-seedance-1-5-pro-251215")
		assert.False(t, found)

		resolved, callErr := decodeOperation(t, "edit", "doubao-seedance-2-0-260128", map[string]any{
			"prompt": "change the sky", "video": "https://cdn.example/main.mp4", "duration": 8, "resolution": "720p",
			"reference_images": []any{"asset://image-one"}, "reference_videos": []any{"asset://video-two"}, "reference_audios": []any{"https://cdn.example/audio.mp3"},
		})
		require.NoError(t, callErr)
		assert.Equal(t, "edit_video", resolved["action"])
		descriptor, callErr := build(t, "doubao-seedance-2-0-260128", resolved)
		require.NoError(t, callErr)
		body := descriptor["body"].(map[string]any)
		assert.Equal(t, "adaptive", body["ratio"])
		content := body["content"].([]any)
		require.Len(t, content, 5)
		mainVideo := content[0].(map[string]any)
		assert.Equal(t, "reference_video", mainVideo["role"])
		assert.Equal(t, "https://cdn.example/main.mp4", mainVideo["video_url"].(map[string]any)["url"])
		assert.Contains(t, content[4].(map[string]any)["text"], "@视频1")
		assert.Contains(t, content[4].(map[string]any)["text"], "change the sky")

		resolved, callErr = decodeOperation(t, "edit", "doubao-seedance-2-5-260628", map[string]any{
			"prompt": "restyle", "video": "asset://main-video", "resolution": "720p",
		})
		require.NoError(t, callErr)
		descriptor, callErr = build(t, "doubao-seedance-2-5-260628", resolved)
		require.NoError(t, callErr)
		assert.Equal(t, float64(-1), descriptor["body"].(map[string]any)["duration"])

		resolved, callErr = decodeOperation(t, "extend", "doubao-seedance-2-5-260628", map[string]any{
			"prompt": "continue walking", "video": "asset://main-video", "duration": 30, "resolution": "720p", "extension_direction": "forward",
		})
		require.NoError(t, callErr)
		descriptor, callErr = build(t, "doubao-seedance-2-5-260628", resolved)
		require.NoError(t, callErr)
		body = descriptor["body"].(map[string]any)
		assert.NotContains(t, body, "extension_direction")
		assert.Contains(t, body["content"].([]any)[1].(map[string]any)["text"], "向前")
		resolved, callErr = decodeOperation(t, "extend", "doubao-seedance-2-0-260128", map[string]any{
			"prompt": "continue walking", "video": "https://cdn.example/main.mp4", "duration": 5, "resolution": "720p",
		})
		require.NoError(t, callErr)
		descriptor, callErr = build(t, "doubao-seedance-2-0-260128", resolved)
		require.NoError(t, callErr)
		assert.Contains(t, descriptor["body"].(map[string]any)["content"].([]any)[1].(map[string]any)["text"], "向后")

		_, callErr = decodeOperation(t, "edit", "doubao-seedance-1-5-pro-251215", map[string]any{"prompt": "edit", "video": "https://cdn.example/main.mp4"})
		require.ErrorContains(t, callErr, "not supported")
		_, callErr = decodeOperation(t, "edit", "doubao-seedance-2-0-260128", map[string]any{"prompt": "edit", "video": "  "})
		require.ErrorContains(t, callErr, "video is required")
		_, callErr = decodeOperation(t, "edit", "doubao-seedance-2-0-260128", map[string]any{"prompt": "edit", "video": map[string]any{"file_id": "file-one"}})
		require.ErrorContains(t, callErr, "public http(s) URL or asset://")
		_, callErr = decodeOperation(t, "edit", "doubao-seedance-2-0-260128", map[string]any{
			"prompt": "edit", "video": map[string]any{"url": "https://cdn.example/main.mp4", "file_id": "file-one"},
		})
		require.ErrorContains(t, callErr, "does not support file_id")
		_, callErr = decodeOperation(t, "edit", "doubao-seedance-2-0-260128", map[string]any{
			"prompt": "edit", "video": "https://cdn.example/main.mp4", "metadata": map[string]any{"content": []any{
				map[string]any{"type": "video_url", "role": "reference_video", "video_url": map[string]any{"file_id": "file-two"}},
			}},
		})
		require.ErrorContains(t, callErr, "does not support file_id")
		_, callErr = decodeOperation(t, "edit", "doubao-seedance-2-0-260128", map[string]any{
			"prompt": "edit", "video": "https://cdn.example/main.mp4", "resolution": "1080p", "reference_images": []any{"https://cdn.example/ref.png"},
		})
		require.ErrorContains(t, callErr, "at most 720p")
		_, callErr = decodeOperation(t, "extend", "doubao-seedance-2-0-260128", map[string]any{
			"prompt": "extend", "video": "https://cdn.example/main.mp4", "reference_videos": []any{"asset://two", "asset://three", "asset://four"},
		})
		require.ErrorContains(t, callErr, "at most 2")
		_, callErr = decodeOperation(t, "edit", "doubao-seedance-2-0-260128", map[string]any{
			"prompt": "edit", "video": "https://cdn.example/main.mp4", "metadata": map[string]any{"operation": "extend"},
		})
		require.ErrorContains(t, callErr, "cannot override")
		_, callErr = decodeOperation(t, "edit", "doubao-seedance-2-0-260128", map[string]any{
			"prompt": "edit", "video": "https://cdn.example/main.mp4", "metadata": map[string]any{"content": []any{
				map[string]any{"type": "image_url", "role": "first_frame", "image_url": map[string]any{"url": "https://cdn.example/first.png"}},
			}},
		})
		require.ErrorContains(t, callErr, "image is not supported")
		_, callErr = decodeOperation(t, "edit", "doubao-seedance-2-0-260128", map[string]any{
			"prompt": "edit", "video": "https://cdn.example/main.mp4", "source_task_id": "old-task",
		})
		require.ErrorContains(t, callErr, "source_task_id cannot override")
		_, callErr = decodeOperation(t, "extend", "doubao-seedance-2-0-260128", map[string]any{
			"prompt": "extend", "video": "https://cdn.example/main.mp4", "metadata": map[string]any{"extension_direction": "forward"},
		})
		require.ErrorContains(t, callErr, "metadata.extension_direction cannot override")

		value, callErr := plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{
			"action": "extend_video", "model": "doubao-seedance-2-0-260128", "upstreamModel": "doubao-seedance-2-0-260128",
			"requestBody": map[string]any{"prompt": "extend", "video": "https://cdn.example/main.mp4", "duration": 5, "resolution": "720p"},
		})
		require.NoError(t, callErr)
		assert.Equal(t, "video", object(t, value)["video_input"])
		assert.Greater(t, object(t, value)["tokens"].(float64), float64(108000))
	})

	t.Run("profile boundaries reject unsupported combinations", func(t *testing.T) {
		_, callErr := decode(t, "doubao-seedance-2-0-260128", map[string]any{"prompt": "too long", "duration": 16})
		require.ErrorContains(t, callErr, "supported range")
		_, callErr = decode(t, "doubao-seedance-2-0-fast-260128", map[string]any{"prompt": "too large", "resolution": "1080p"})
		require.ErrorContains(t, callErr, "resolution")
		_, callErr = decode(t, "doubao-seedance-2-0-fast-260128", map[string]any{"prompt": "invalid size", "size": "ultra"})
		require.ErrorContains(t, callErr, "size must")
		_, callErr = decode(t, "doubao-seedance-1-0-lite-t2v", map[string]any{"image": "https://cdn.example/first.png"})
		require.ErrorContains(t, callErr, "not supported")
		_, callErr = decode(t, "doubao-seedance-2-0-260128", map[string]any{"image": "data:image/png;base64,AA=="})
		require.ErrorContains(t, callErr, "public http(s) URL or asset://")
		references := make([]any, 10)
		for index := range references {
			references[index] = "https://cdn.example/ref.png"
		}
		_, callErr = decode(t, "doubao-seedance-2-0-260128", map[string]any{"reference_images": references})
		require.ErrorContains(t, callErr, "at most 9")
	})

	t.Run("multipart upload gives an actionable URL hint", func(t *testing.T) {
		_, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "doubao-seedance-2-0-260128",
			"body": map[string]any{"kind": "multipart", "fields": map[string]any{"prompt": []any{"animate"}}, "files": []any{
				map[string]any{"field": "input_reference", "ref": "request_file:input_reference"},
			}},
		})
		require.ErrorContains(t, callErr, "public http(s) URL or asset:// ID")
	})
}
