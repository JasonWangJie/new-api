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
			assert.Equal(t, tc.want, view.FailReason)
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
	assert.True(t, strings.HasPrefix(view.FailReason, "Upstream rejected the reference image:"))
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
			assert.Equal(t, "Video generation failed", PublicTaskFailureReason(task))
			assert.Equal(t, before, []byte(task.Data))
			assert.Equal(t, 1250, task.Quota)
			assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), task.Status)
			assert.Equal(t, "Video generation failed", task.FailReason)
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
