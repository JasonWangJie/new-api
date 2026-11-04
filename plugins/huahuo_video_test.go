package plugins_test

import (
	"bytes"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	adaptor "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHuahuoVideoWireContract(t *testing.T) {
	source, err := builtinplugins.Source(constant.TaskPlatformHuahuoVideo)
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: constant.TaskPlatformHuahuoVideo})
	require.NoError(t, err)
	assert.Empty(t, plugin.Meta.Models, "saved channel configuration supplies model IDs")
	const baseURL = "https://huahuo.ccwu.cc"
	const image = "https://cdn.example/ref.png"
	const audio = "https://cdn.example/ref.mp3"
	const video = "https://cdn.example/ref.mp4"
	const last = "https://cdn.example/end.png"
	request := map[string]any{"model": "retail-video", "prompt": "walk", "seconds": 5, "resolution": "720p", "aspect_ratio": "16:9"}
	for _, tc := range []struct {
		name, action string
		extra        map[string]any
		wire         map[string]any
	}{
		{name: "text", action: "text_to_video"},
		{name: "image", action: "image_to_video", extra: map[string]any{"image": map[string]any{"url": image}}, wire: map[string]any{"input_reference": []any{image}}},
		{name: "references", action: "reference_to_video", extra: map[string]any{"input_images": `["` + image + `"]`, "reference_audios": []any{audio}, "video_reference": video}, wire: map[string]any{"input_reference": []any{image}, "audio_references": []any{audio}, "video_reference": []any{video}}},
		{name: "frames", action: "first_last_frame", extra: map[string]any{"image": image, "last_frame": last}, wire: map[string]any{"start_frame": image, "end_frame": last}},
		{name: "start only", action: "first_last_frame", extra: map[string]any{"start_frame": image}, wire: map[string]any{"start_frame": image}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := maps.Clone(request)
			maps.Copy(body, tc.extra)
			value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": "retail-video", "upstreamModel": "vendor/path/model", "operation": "create", "body": map[string]any{"kind": "json", "value": body},
			})
			require.NoError(t, err)
			intent := value.(map[string]any)
			assert.Equal(t, "retail-video", intent["model"])
			assert.Equal(t, tc.action, intent["action"])
			value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"model": "retail-video", "upstreamModel": "vendor/path/model", "publicTaskId": "task_public", "baseUrl": baseURL, "apiKey": "test-key", "action": intent["action"], "requestBody": intent["requestBody"],
			})
			require.NoError(t, err)
			wire := value.(map[string]any)
			assert.Equal(t, baseURL+"/v1/videos", wire["url"])
			assert.Equal(t, map[string]any{"Authorization": "Bearer test-key", "Idempotency-Key": "task_public", "Content-Type": "application/json"}, wire["headers"])
			expected := map[string]any{"model": "vendor/path/model", "prompt": "walk", "seconds": "5", "resolution": "720p", "size": "16:9"}
			maps.Copy(expected, tc.wire)
			assert.Equal(t, expected, wire["body"])
		})
	}
	for _, bad := range []map[string]any{
		{"seconds": 3601}, {"reference_videos": "http://cdn.example/ref.mp4"}, {"input_images": "https://127.0.0.1/ref.png"}, {"input_images": "https://0x7f.0x0.0x0.0x1/ref.png"},
		{"end_frame": last}, {"start_frame": image, "audio_references": audio}, {"input_reference": image, "input_images": image},
	} {
		body := maps.Clone(request)
		maps.Copy(body, bad)
		_, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "retail-video", "body": map[string]any{"kind": "json", "value": body},
		})
		assert.Error(t, err, "%v", bad)
	}
	for upstream, expected := range map[string]string{
		"queued": "QUEUED", "submitting": "SUBMITTED", "reserved": "SUBMITTED", "review_required": "IN_PROGRESS", "processing": "IN_PROGRESS", "in_progress": "IN_PROGRESS", "completed": "SUCCESS", "failed": "FAILURE", "unrecognized": "UNKNOWN",
	} {
		result, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{"taskId": "task_private"}, map[string]any{"id": "task_private", "status": upstream, "progress": 100})
		require.NoError(t, err)
		assert.Equal(t, expected, result.(map[string]any)["status"])
		if expected == "IN_PROGRESS" || expected == "QUEUED" {
			assert.Equal(t, "99%", result.(map[string]any)["progress"])
		}
	}
	for _, tc := range []struct {
		state, expected string
		amount          any
	}{
		{state: "charged", amount: 0, expected: "UNKNOWN"},
		{state: "settled", amount: 5, expected: "UNKNOWN"},
		{state: "reserved", expected: "UNKNOWN"},
		{state: "unrecognized", amount: "5", expected: "UNKNOWN"},
		{amount: 5, expected: "UNKNOWN"},
		{state: "released", amount: 5, expected: "FAILURE"},
		{state: "refunded", amount: "5", expected: "FAILURE"},
	} {
		value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{"taskId": "task_private"}, map[string]any{
			"id": "task_private", "status": "failed", "credit_state": tc.state, "credit_amount": tc.amount,
		})
		require.NoError(t, err)
		assert.Equal(t, tc.expected, value.(map[string]any)["status"], "credit state %q amount %v", tc.state, tc.amount)
	}
	value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{"taskId": "task_private"}, map[string]any{"id": "other", "status": "completed"})
	require.NoError(t, err)
	assert.Equal(t, "UNKNOWN", value.(map[string]any)["status"])
	for _, body := range []map[string]any{
		{"id": "task_private", "task_id": "other", "status": "completed"},
		{"id": "task_private", "status": "failed", "progress": 100, "progress_source": "confirmed", "credit_state": "refunded"},
		{"id": "task_private", "status": "completed", "success": false},
	} {
		value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{"taskId": "task_private"}, body)
		require.NoError(t, err)
		assert.Equal(t, "UNKNOWN", value.(map[string]any)["status"], "%v", body)
	}
	_, err = plugin.Engine.Call(t.Context(), "parseSubmitResponse", map[string]any{}, map[string]any{
		"statusCode": 503, "body": map[string]any{"id": "task_private", "task_id": "other", "status": "review_required"},
	})
	require.Error(t, err)
	value, err = plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{"taskId": "task_private", "apiKey": "fixture-private-key"}, map[string]any{
		"id": "task_private", "status": "failed", "error": map[string]any{"message": "task_private failed with fixture-private-key at https://provider.example/private"},
	})
	require.NoError(t, err)
	assert.Equal(t, "[redacted] failed with [redacted] at [url]", value.(map[string]any)["reason"])
	value, err = plugin.Engine.Call(t.Context(), "parseSubmitResponse", map[string]any{}, map[string]any{"statusCode": 503, "body": map[string]any{"id": "task_private", "status": "review_required"}})
	require.NoError(t, err)
	assert.Equal(t, "task_private", value.(map[string]any)["taskId"])
	assert.Empty(t, value.(map[string]any)["taskData"])
	value, err = plugin.Engine.Call(t.Context(), "buildContentRequest", map[string]any{"baseUrl": baseURL, "apiKey": "test-key", "upstreamTaskId": "task_private", "artifactKey": "video", "clientRequest": map[string]any{"method": "GET"}})
	require.NoError(t, err)
	assert.Equal(t, baseURL+"/v1/videos/task_private/content", value.(map[string]any)["url"])
	assert.Equal(t, map[string]any{"Authorization": "Bearer test-key"}, value.(map[string]any)["headers"])
}

func TestHuahuoVideoMultipartPreservesReferenceOrderAndBytes(t *testing.T) {
	plugin, ok := jsplugin.DefaultRegistry.Get(constant.TaskPlatformHuahuoVideo)
	require.True(t, ok)
	files := []string{"first-image-bytes", "second-image-bytes"}
	var raw bytes.Buffer
	incoming := multipart.NewWriter(&raw)
	fields := map[string]string{"model": "retail-video", "prompt": "walk", "seconds": "5", "resolution": "720p", "size": "16:9"}
	for name, value := range fields {
		require.NoError(t, incoming.WriteField(name, value))
	}
	for _, data := range files {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="input_reference[]"; filename="ref.png"`)
		header.Set("Content-Type", "image/png")
		part, err := incoming.CreatePart(header)
		require.NoError(t, err)
		_, err = io.WriteString(part, data)
		require.NoError(t, err)
	}
	require.NoError(t, incoming.Close())
	baseURL, mapping := "https://provider.example", `{"retail-video":"vendor/model"}`
	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, BaseURL: &baseURL, Models: "retail-video", ModelMapping: &mapping}
	images, zero := 2, 0
	channel.SetSetting(kitdto.ChannelSettings{TaskPluginKey: constant.TaskPlatformHuahuoVideo, TaskPluginModelConfigs: map[string]kitdto.TaskVideoModelConfig{
		"vendor/model": {Duration: kitdto.TaskVideoDuration{Values: []int{5}}, Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"}, Modes: []string{"reference_to_video"},
			References: kitdto.TaskVideoReferences{Images: &images, Videos: &zero, Audios: &zero, ImageUpload: true}, BillingMode: "per_request"},
	}})
	candidate, err := service.DecodeMixedVideoCandidate(t.Context(), channel, "retail-video", "create", raw.Bytes(), service.VideoModelOverrides{}, 0, incoming.FormDataContentType())
	require.NoError(t, err)
	assert.Equal(t, "reference_to_video", candidate.Intent["action"])
	assert.Equal(t, "vendor/model", candidate.UpstreamModel)
	view, _, err := service.MixedVideoRequestContext(raw.Bytes(), incoming.FormDataContentType(), "create")
	require.NoError(t, err)
	value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{"model": "retail-video", "body": view.Body})
	require.NoError(t, err)
	intent := value.(map[string]any)
	assert.Equal(t, "reference_to_video", intent["action"])
	profile := jsplugin.VideoProfile{Models: []string{"retail-video"}, Modes: []jsplugin.VideoModeProfile{{
		Name: "reference_to_video", Duration: &jsplugin.VideoDurationProfile{Values: []int{5}, Default: 5}, Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"},
		Inputs: []jsplugin.VideoInputProfile{{Name: "reference_images", Kind: "image", Sources: []string{"upload"}, MaxItems: 2, Required: true}},
	}}}
	require.NoError(t, service.ValidateVideoProfileRequest(profile, "reference_to_video", intent["requestBody"], view.Files))
	requestAdaptor := adaptor.New(plugin)
	info := &relaycommon.RelayInfo{OriginModelName: "retail-video",
		TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_public", Action: "reference_to_video"},
		ChannelMeta:   &relaycommon.ChannelMeta{ChannelBaseUrl: "https://provider.example", UpstreamModelName: "vendor/model", ApiKey: "test-key"},
	}
	requestAdaptor.Init(info)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader(raw.Bytes()))
	c.Request.Header.Set("Content-Type", incoming.FormDataContentType())
	c.Set("task_request", intent["requestBody"])
	body, err := requestAdaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://provider.example/v1/videos", body)
	require.NoError(t, err)
	require.NotNil(t, request.GetBody)
	require.NoError(t, requestAdaptor.BuildRequestHeader(c, request, info))
	assert.Nil(t, request.GetBody, "the transport must not replay a possibly billable POST")
	contentType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	require.NoError(t, err)
	assert.Equal(t, "multipart/form-data", contentType)
	assert.NotEqual(t, incoming.Boundary(), params["boundary"])
	assert.Equal(t, "task_public", request.Header.Get("Idempotency-Key"))
	form, err := multipart.NewReader(request.Body, params["boundary"]).ReadForm(1024)
	require.NoError(t, err)
	t.Cleanup(func() { _ = form.RemoveAll() })
	assert.Equal(t, []string{"vendor/model"}, form.Value["model"])
	assert.Equal(t, []string{"5"}, form.Value["seconds"])
	require.Len(t, form.File["input_reference"], 2)
	for index, file := range form.File["input_reference"] {
		opened, err := file.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(opened)
		_ = opened.Close()
		require.NoError(t, err)
		assert.Equal(t, files[index], string(data))
		assert.Equal(t, "image/png", file.Header.Get("Content-Type"))
	}
}

func TestHuahuoVideoRetryRulesKeepOriginalOperation(t *testing.T) {
	policy := mediaretry.Policy{Rules: []mediaretry.Rule{{
		ID: "retry", Name: "Retry", Enabled: true, Scope: mediaretry.Scope{MediaTypes: []string{"video"}},
		Match: mediaretry.Match{ContainsAny: []string{"fixture failure"}}, Steps: []mediaretry.Step{{ID: "retry", Action: mediaretry.SameChannel, MaxAttempts: 1}},
	}}}
	state := mediaretry.State{Now: 10, TotalLimit: 5}
	fact := mediaretry.Fact{MediaType: "video", Provider: constant.TaskPlatformHuahuoVideo, Message: "fixture failure", Deadline: 60, Stage: "submit", Unknown: true}
	decision, err := service.PlanInitialVideoFailure(policy, fact, state)
	require.NoError(t, err)
	assert.True(t, decision.Matched)
	assert.Equal(t, mediaretry.ManualReview, decision.Action)
	fact.Stage, fact.HasUpstreamTask, fact.Accepted = "poll", true, true
	decision, err = service.PlanTerminalVideoFailure(policy, fact, state)
	require.NoError(t, err)
	assert.True(t, decision.Matched)
	assert.Equal(t, mediaretry.PollSameJob, decision.Action)
	fact.Stage, fact.Unknown, fact.Terminal = "terminal", false, true
	decision, err = service.PlanTerminalVideoFailure(policy, fact, state)
	require.NoError(t, err)
	assert.True(t, decision.Matched)
	assert.Equal(t, mediaretry.Stop, decision.Action)
}
