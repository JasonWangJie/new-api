package service

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/tidwall/gjson"
)

// PublicTaskUpstreamProgress exposes only a valid Drama provider percentage,
// independently of the host's lifecycle and settlement progress.
func PublicTaskUpstreamProgress(task *model.Task) (float64, bool) {
	if task == nil || task.Platform != "drama" {
		return 0, false
	}
	snapshot := task.Data
	path := "progress"
	configured := task.PrivateData.UpstreamAsync != nil
	if configured {
		path = strings.TrimSpace(task.PrivateData.UpstreamAsync.Poll.Response.ProgressPath)
		if path == "" {
			return 0, false
		}
		snapshot = task.PrivateData.UpstreamAsyncResponse
	}
	if !gjson.ValidBytes(snapshot) {
		return 0, false
	}
	value := gjson.GetBytes(snapshot, path)
	var progress float64
	switch {
	case value.Type == gjson.Number:
		progress = value.Num
	case configured && value.Type == gjson.String:
		text := strings.TrimSpace(value.Str)
		text, _ = strings.CutSuffix(text, "%")
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return 0, false
		}
		progress = parsed
	default:
		return 0, false
	}
	if math.IsNaN(progress) || math.IsInf(progress, 0) || progress < 0 || progress > 100 {
		return 0, false
	}
	return progress, true
}

// PublicTaskFailureReason explains video failures and recovers details from
// historical Drama snapshots whose driver stored only a placeholder. It does
// not call JavaScript, poll upstream, or change persisted tasks.
func PublicTaskFailureReason(task *model.Task) string {
	if task == nil {
		return ""
	}
	if task.Status != model.TaskStatusFailure {
		return task.FailReason
	}
	video := task.Platform == "drama" || task.PrivateData.AsyncMedia
	if !video {
		plugin, found := jsplugin.DefaultRegistry.Get(string(task.Platform))
		if !found {
			if channelType, err := strconv.Atoi(string(task.Platform)); err == nil {
				plugin, found = jsplugin.DefaultRegistry.GetByChannelType(channelType)
			}
		}
		if found {
			video = slices.ContainsFunc(plugin.Meta.Protocols, func(claim jsplugin.ProtocolClaim) bool { return claim.Name == "openai_video" })
		}
	}
	if !video {
		return task.FailReason
	}
	if task.Platform != "drama" || !isMissingVideoFailureReason(task.FailReason) {
		return VideoTaskFailureReason(task.FailReason)
	}
	var snapshot map[string]any
	if common.Unmarshal(task.Data, &snapshot) != nil || snapshot["status"] != "failed" {
		return VideoTaskFailureReason(task.FailReason)
	}
	var candidates []string
	switch failure := snapshot["error"].(type) {
	case string:
		candidates = append(candidates, failure)
	case map[string]any, []any:
		if encoded, err := common.Marshal(failure); err == nil {
			candidates = append(candidates, string(encoded))
		}
	}
	if metadata, ok := snapshot["metadata"].(map[string]any); ok {
		if message, ok := metadata["url"].(string); ok {
			candidates = append(candidates, message)
		}
	}
	for _, candidate := range candidates {
		candidate = sanitizeVideoFailureDiagnostic(candidate, -1)
		if isMissingVideoFailureReason(candidate) {
			continue
		}
		return VideoTaskFailureReason(candidate)
	}
	return VideoTaskFailureReason(task.FailReason)
}

// BuildTaskPluginView converts a persisted task into the deliberately narrow
// public shape permitted at JavaScript plugin boundaries.
func BuildTaskPluginView(task *model.Task) (dto.TaskView, error) {
	createdAt := task.CreatedAt
	if createdAt == 0 {
		createdAt = task.SubmitTime
	}
	view := dto.TaskView{
		TaskID:     task.TaskID,
		Platform:   string(task.Platform),
		Status:     string(task.Status),
		Progress:   task.Progress,
		FailReason: PublicTaskFailureReason(task),
		CreatedAt:  createdAt,
		UpdatedAt:  task.UpdatedAt,
		FinishedAt: task.FinishTime,
	}
	if len(task.Data) > 0 {
		if err := common.Unmarshal(task.Data, &view.Data); err != nil {
			return dto.TaskView{}, err
		}
		view.Data = replacePrivateTaskID(view.Data, task.PrivateData.UpstreamTaskID, task.TaskID)
	}
	return view, nil
}

// replacePrivateTaskID rewrites exact private IDs only in known task-ID fields.
// Map keys and opaque strings, including URLs containing the ID, are preserved.
func replacePrivateTaskID(value any, privateTaskID, publicTaskID string) any {
	if privateTaskID == "" || privateTaskID == publicTaskID {
		return value
	}
	switch typed := value.(type) {
	case []any:
		replaced := make([]any, len(typed))
		for index, item := range typed {
			replaced[index] = replacePrivateTaskID(item, privateTaskID, publicTaskID)
		}
		return replaced
	case map[string]any:
		replaced := make(map[string]any, len(typed))
		for key, item := range typed {
			if (key == "id" || key == "task_id" || key == "taskId") && item == privateTaskID {
				replaced[key] = publicTaskID
				continue
			}
			replaced[key] = replacePrivateTaskID(item, privateTaskID, publicTaskID)
		}
		return replaced
	default:
		return value
	}
}
