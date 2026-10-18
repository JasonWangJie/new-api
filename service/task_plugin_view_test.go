package service

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
)

func TestBuildTaskPluginViewRestoresLegacyDramaFailureReason(t *testing.T) {
	const placeholder = "Video generation failed"
	const dimensions = "Error while downloading image, error: expected the width to be at most 6000px, but received a 6060x3476px image instead"
	for _, tc := range []struct {
		name, platform, status, reason, data, want string
	}{
		{name: "saved image dimensions", reason: placeholder, data: `{"status":"failed","metadata":{"url":"` + dimensions + `"}}`, want: dimensions},
		{name: "saved upstream busy", reason: placeholder, data: `{"status":"failed","metadata":{"url":"系统繁忙，请稍后重试"}}`, want: "系统繁忙，请稍后重试"},
		{name: "empty saved reason", data: `{"status":"failed","metadata":{"url":"系统繁忙，请稍后重试"}}`, want: "系统繁忙，请稍后重试"},
		{name: "error string has priority", reason: placeholder, data: `{"status":"failed","error":" primary error ","metadata":{"url":"secondary error"}}`, want: "primary error"},
		{name: "error message has priority", reason: placeholder, data: `{"status":"failed","error":{"message":" primary message "},"metadata":{"url":"secondary error"}}`, want: "primary message"},
		{name: "empty error falls back to metadata", reason: placeholder, data: `{"status":"failed","error":{"message":" "},"metadata":{"url":"系统繁忙，请稍后重试"}}`, want: "系统繁忙，请稍后重试"},
		{name: "specific saved reason is authoritative", reason: " Local billing settlement failed ", data: `{"status":"failed","error":"different upstream reason"}`, want: " Local billing settlement failed "},
		{name: "another provider remains unchanged", platform: "sora", reason: placeholder, data: `{"status":"failed","error":"not a Drama reason"}`, want: placeholder},
		{name: "active task remains unchanged", status: "IN_PROGRESS", reason: placeholder, data: `{"status":"failed","error":"stale snapshot"}`, want: placeholder},
		{name: "successful task remains unchanged", status: "SUCCESS", reason: placeholder, data: `{"status":"failed","error":"stale snapshot"}`, want: placeholder},
		{name: "nonfailed snapshot remains unchanged", reason: placeholder, data: `{"status":"queued","error":"stale error"}`, want: placeholder},
		{name: "signed result URL is not a reason", reason: placeholder, data: `{"status":"failed","metadata":{"url":" HTTPS://cdn.example/private.mp4?signature=private-token "}}`, want: placeholder},
		{name: "relative result URL is not a reason", reason: placeholder, data: `{"status":"failed","metadata":{"url":"/v1/videos/private/content"}}`, want: placeholder},
		{name: "inline result URL is not a reason", reason: placeholder, data: `{"status":"failed","metadata":{"url":"data:video/mp4;base64,cHJpdmF0ZQ=="}}`, want: placeholder},
		{name: "malformed error falls back to metadata", reason: placeholder, data: `{"status":"failed","error":{"message":{"prompt":"private"}},"metadata":{"url":"系统繁忙，请稍后重试"}}`, want: "系统繁忙，请稍后重试"},
		{name: "nonstring metadata is ignored", reason: placeholder, data: `{"status":"failed","metadata":{"url":["private"]}}`, want: placeholder},
		{name: "missing reason remains empty", data: `{"status":"failed","metadata":{"url":" "}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &model.Task{TaskID: "task_legacy_drama", Platform: "drama", Status: model.TaskStatusFailure, FailReason: tc.reason, Quota: 1250, Progress: "100%", Data: []byte(tc.data)}
			if tc.platform != "" {
				task.Platform = constant.TaskPlatform(tc.platform)
			}
			if tc.status != "" {
				task.Status = model.TaskStatus(tc.status)
			}
			before, err := common.Marshal(task)
			require.NoError(t, err)
			snapshot, quota := task.Snapshot(), task.Quota
			view, err := BuildTaskPluginView(task)
			require.NoError(t, err)
			want := tc.want
			if task.Platform == "drama" && task.Status == model.TaskStatusFailure {
				want = VideoTaskFailureReason(want)
			}
			assert.Equal(t, want, view.FailReason)
			assert.True(t, snapshot.Equal(task.Snapshot()), "presentation must not change task lifecycle or billing state")
			assert.Equal(t, quota, task.Quota)
			after, err := common.Marshal(task)
			require.NoError(t, err)
			assert.Equal(t, before, after, "presentation must not rewrite the stored row or raw snapshot")
		})
	}
}

func TestBuildTaskPluginViewSanitizesRecoveredDramaDiagnostics(t *testing.T) {
	const diagnostic = "Image download denied; Bearer private-bearer; api_key=private-key; https://signed.example/private?token=private-token"
	task := &model.Task{Platform: "drama", Status: model.TaskStatusFailure, FailReason: "Video generation failed", Data: []byte(`{"status":"failed","metadata":{"url":"` + diagnostic + `"}}`)}
	view, err := BuildTaskPluginView(task)
	require.NoError(t, err)
	assert.Contains(t, view.FailReason, "Image download denied")
	for _, secret := range []string{"private-bearer", "private-key", "private-token", "signed.example"} {
		assert.NotContains(t, view.FailReason, secret)
	}

	longDiagnostic := "Upstream rejected the reference image: " + strings.Repeat("长", common.LocalLogContentLimit)
	task.Data, err = common.Marshal(map[string]any{"status": "failed", "error": longDiagnostic})
	require.NoError(t, err)
	view, err = BuildTaskPluginView(task)
	require.NoError(t, err)
	assert.Contains(t, view.FailReason, "Upstream rejected the reference image:")
	assert.LessOrEqual(t, len([]rune(view.FailReason)), common.LocalLogContentLimit)
	assert.True(t, bytes.Contains(task.Data, []byte(longDiagnostic)), "stored diagnostic must remain intact")
}

func TestPublicTaskFailureReasonKeepsUnavailableSnapshotsUnchanged(t *testing.T) {
	assert.Empty(t, PublicTaskFailureReason(nil))
	for _, tc := range []struct{ name, data string }{
		{name: "missing snapshot"},
		{name: "malformed snapshot", data: `{"status":"failed",`},
		{name: "null snapshot", data: `null`},
		{name: "array snapshot", data: `[]`},
		{name: "null metadata", data: `{"status":"failed","metadata":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &model.Task{Platform: "drama", Status: model.TaskStatusFailure, FailReason: "Video generation failed", Quota: 1250, Data: []byte(tc.data)}
			before := bytes.Clone(task.Data)
			assert.Equal(t, VideoTaskFailureReason("Video generation failed"), PublicTaskFailureReason(task))
			assert.Equal(t, before, []byte(task.Data))
			assert.Equal(t, 1250, task.Quota)
			assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), task.Status)
			assert.Equal(t, "Video generation failed", task.FailReason)
		})
	}
}

func TestVideoTaskFailureNoticeExplainsProviderReasons(t *testing.T) {
	for _, tc := range []struct {
		name, raw, summary, suggestion string
	}{
		{name: "input image person", raw: "The request failed because the input image 'content[3]' may contain real person.", summary: "输入图片", suggestion: "图片"},
		{name: "input video person", raw: "The request failed because the input video 'content[2]' may contain real person.", summary: "输入视频", suggestion: "视频"},
		{name: "output video safety", raw: "The request failed because the output video may contain sensitive information.", summary: "生成的视频可能包含敏感内容", suggestion: "提示词"},
		{name: "output audio copyright", raw: "The request failed because the output audio may be related to copyright restrictions.", summary: "生成的音频可能涉及版权限制", suggestion: "音频"},
		{name: "safety without category", raw: "内容安全审核拦截", summary: "内容安全审核未通过", suggestion: "提示词"},
		{name: "aspect ratio", raw: `The aspect ratio of "2_image_example.png" is 2.8458498023715415. Required 0.4–2.5.`, summary: "宽高比为 2.846", suggestion: "0.4–2.5"},
		{name: "video aspect ratio", raw: `The aspect ratio of input video 'content[2]' is 3.0. Required 0.4-2.5.`, summary: "输入视频的宽高比", suggestion: "视频"},
		{name: "unknown material aspect ratio", raw: `The aspect ratio of 'content[2]' is 3.0. Required 0.4-2.5.`, summary: "输入素材的宽高比", suggestion: "素材"},
		{name: "image dimensions", raw: "Error while downloading image, error: expected the width to be at most 6000px, but received a 6060x3476px image instead", summary: "6060×3476", suggestion: "6000"},
		{name: "image inaccessible", raw: "Image download denied: HTTP 403", summary: "无法获取输入图片", suggestion: "访问"},
		{name: "image invalid", raw: "Invalid image format", summary: "图片格式", suggestion: "图片"},
		{name: "unsupported parameter", raw: "Invalid parameter: duration must be 5 or 10", summary: "参数不符合上游要求", suggestion: "参数"},
		{name: "rate limited", raw: "Rate limit exceeded", summary: "请求频率或并发数", suggestion: "并发"},
		{name: "busy", raw: "系统繁忙，请稍后重试", summary: "服务当前繁忙", suggestion: "稍后"},
		{name: "timeout", raw: "Upstream request timed out", summary: "超时", suggestion: "任务号"},
		{name: "local settlement", raw: "Local billing settlement failed", summary: "结算失败", suggestion: "管理员"},
		{name: "local storage", raw: "Generated video could not be saved locally: storage timeout", summary: "视频结果未能保存到本地", suggestion: "不要重复发起视频生成"},
		{name: "local storage status", raw: "storage_failed", summary: "视频结果未能保存到本地", suggestion: "不要重复发起视频生成"},
		{name: "unknown", raw: "Unrecognized provider error Q102", summary: "未提供可识别的进一步原因", suggestion: "管理员"},
		{name: "generic", raw: "Video generation failed", summary: "未提供具体错误原因", suggestion: "管理员"},
		{name: "empty", summary: "未提供具体错误原因", suggestion: "管理员"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notice := VideoTaskFailureNotice(tc.raw)
			assert.Contains(t, notice.Summary, tc.summary)
			assert.Contains(t, notice.Suggestion, tc.suggestion)
			message := VideoTaskFailureReason(tc.raw)
			assert.Contains(t, message, notice.Summary)
			assert.Contains(t, message, notice.Suggestion)
			assert.Equal(t, message, VideoTaskFailureReason(message), "repeated presentation must not stack explanations")
			assert.Equal(t, notice, VideoTaskFailureNotice(message))
			if strings.Contains(tc.name, "person") {
				assert.Contains(t, notice.Summary, "可能包含真人")
				assert.Contains(t, notice.Summary, "content[")
				assert.Contains(t, notice.Summary, "不等同于参考素材序号")
			}
			if tc.name == "output video safety" || tc.name == "safety without category" {
				assert.Contains(t, notice.Summary, "未提供具体")
			}
		})
	}
}

func TestVideoTaskFailureNoticeSanitizesDetails(t *testing.T) {
	raw := "The request failed because the input image 'content[3]' may contain real person. Bearer private-bearer; api_key=private-key; token=private-token; HTTPS://signed.example/file?signature=private-signature"
	notice := VideoTaskFailureNotice(raw)
	assert.Contains(t, notice.Detail, "input image 'content[3]' may contain real person")
	for _, secret := range []string{"private-bearer", "private-key", "private-token", "private-signature", "signed.example"} {
		assert.NotContains(t, VideoTaskFailureReason(raw), secret)
	}
	longMessage := VideoTaskFailureReason("Unknown failure " + strings.Repeat("长", common.LocalLogContentLimit))
	assert.LessOrEqual(t, len([]rune(longMessage)), common.LocalLogContentLimit)
	assert.Equal(t, longMessage, VideoTaskFailureReason(longMessage))
	lateRaw := strings.Repeat("长", 1950) + " The request failed because the output video may contain sensitive information."
	lateNotice := VideoTaskFailureNotice(lateRaw)
	assert.Contains(t, lateNotice.Summary, "敏感内容", "recognizable reasons after a long prefix must still be explained")
	lateReason := VideoTaskFailureReason(lateRaw)
	assert.Equal(t, lateReason, VideoTaskFailureReason(lateReason), "detail truncation must not change the explanation on another query surface")
	assert.Equal(t, lateNotice, VideoTaskFailureNotice(lateReason))
	restored := VideoTaskFailureNotice("上游审核说明 Bearer private-summary " + strings.Repeat("长", 300) + "\n处理建议：请联系管理员 token=private-suggestion " + strings.Repeat("长", 300) + "\n原始错误：HTTPS://signed.example/file?token=private-detail")
	assert.LessOrEqual(t, len([]rune(restored.Summary)), 256)
	assert.LessOrEqual(t, len([]rune(restored.Suggestion)), 256)
	for _, field := range []string{restored.Summary, restored.Suggestion, restored.Detail} {
		for _, secret := range []string{"private-summary", "private-suggestion", "private-detail", "signed.example"} {
			assert.NotContains(t, field, secret, "restored explanation fields must be sanitized too")
		}
	}
	for _, url := range []string{"https://signed.example/private?token=secret", "/v1/videos/private/content", "data:video/mp4;base64,cHJpdmF0ZQ=="} {
		assert.Empty(t, VideoTaskFailureNotice(url).Detail)
	}
	assert.Empty(t, VideoTaskFailureNotice(`{"request":{"prompt":"private prompt"}}`).Detail)
}

func TestPublicTaskFailureReasonAppliesOnlyToFailedVideoTasks(t *testing.T) {
	const raw = "The request failed because the output video may contain sensitive information."
	originalRegistry := jsplugin.DefaultRegistry
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	t.Cleanup(func() { jsplugin.DefaultRegistry = originalRegistry })
	_, err := jsplugin.DefaultRegistry.Register(`
		export const meta = {apiVersion:1,key:"test-video-notice",name:"Test",version:"1.0.0",author:{name:"Test"},channelTypes:[55],models:["video"],fetchMode:"per_task",protocols:["openai_video"]};
		export function buildSubmitRequest() { return {}; }
		export function parseSubmitResponse() { return {}; }
		export function buildQueryRequest() { return {}; }
		export function parseTaskResult() { return {}; }
		export const protocols = {openai_video:{decodeRequest() { return {}; },render() { return {}; }}};
		export function listArtifacts() { return []; }
		export function buildContentRequest() { return {}; }
	`, jsplugin.Options{})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, platform string
		asyncMedia     bool
		status         model.TaskStatus
		translated     bool
	}{
		{name: "drama legacy", platform: "drama", status: model.TaskStatusFailure, translated: true},
		{name: "async video", platform: "custom-video", asyncMedia: true, status: model.TaskStatusFailure, translated: true},
		{name: "registered video protocol", platform: "test-video-notice", status: model.TaskStatusFailure, translated: true},
		{name: "legacy numeric video platform", platform: "55", status: model.TaskStatusFailure, translated: true},
		{name: "other media", platform: "music", status: model.TaskStatusFailure},
		{name: "ongoing", platform: "drama", status: model.TaskStatusInProgress},
		{name: "success", platform: "drama", status: model.TaskStatusSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &model.Task{Platform: constant.TaskPlatform(tc.platform), Status: tc.status, FailReason: raw, PrivateData: model.TaskPrivateData{AsyncMedia: tc.asyncMedia}}
			want := raw
			if tc.translated {
				want = VideoTaskFailureReason(raw)
			}
			assert.Equal(t, want, PublicTaskFailureReason(task))
			assert.Equal(t, raw, task.FailReason)
		})
	}
}

func TestBuildTaskPluginViewRewritesOnlyStructuredTaskIDFields(t *testing.T) {
	const (
		privateTaskID = "upstream-task-123"
		publicTaskID  = "task_public_123"
		resultURL     = "https://cdn.example.com/results/upstream-task-123/video.mp4"
	)

	taskData, err := common.Marshal(map[string]any{
		"task_id": privateTaskID,
		"id":      privateTaskID,
		"taskId":  privateTaskID,
		"url":     resultURL,
		"message": "completed upstream-task-123",
		"nested": []any{
			map[string]any{
				"task_id": privateTaskID,
				"url":     resultURL,
			},
			privateTaskID,
		},
		privateTaskID: "opaque map key",
	})
	require.NoError(t, err)
	task := &model.Task{
		TaskID: publicTaskID,
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID: privateTaskID,
		},
		Data: taskData,
	}

	view, err := BuildTaskPluginView(task)
	require.NoError(t, err)

	data, ok := view.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, publicTaskID, data["task_id"])
	assert.Equal(t, publicTaskID, data["id"])
	assert.Equal(t, publicTaskID, data["taskId"])
	assert.Equal(t, resultURL, data["url"])
	assert.Equal(t, "completed upstream-task-123", data["message"])
	assert.Equal(t, "opaque map key", data[privateTaskID])

	nested, ok := data["nested"].([]any)
	require.True(t, ok)
	nestedData, ok := nested[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, publicTaskID, nestedData["task_id"])
	assert.Equal(t, resultURL, nestedData["url"])
	assert.Equal(t, privateTaskID, nested[1])

}

func TestBuildTaskPluginViewOmitsPrivatePollState(t *testing.T) {
	task := &model.Task{
		TaskID: "task_public_view",
		Data:   []byte(`{"ok":true}`),
		PrivateData: model.TaskPrivateData{
			PluginState:  []byte(`{"req_key":"secret"}`),
			PollFailures: 7,
		},
	}

	view, err := BuildTaskPluginView(task)
	require.NoError(t, err)
	encoded, err := common.Marshal(view)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, common.Unmarshal(encoded, &payload))
	assert.NotContains(t, payload, "plugin_state")
	assert.NotContains(t, payload, "poll_failures")
	assert.NotContains(t, payload, "private_data")
}
