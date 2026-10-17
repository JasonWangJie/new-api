package service

import (
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
)

// PublicTaskFailureReason recovers details from historical Drama snapshots whose
// driver stored only a placeholder. It is presentation-only compatibility, not
// driver evaluation: it does not call JavaScript, poll upstream, or change tasks.
func PublicTaskFailureReason(task *model.Task) string {
	if task == nil {
		return ""
	}
	if task.Platform != "drama" || task.Status != model.TaskStatusFailure {
		return task.FailReason
	}
	if reason := strings.TrimSpace(task.FailReason); reason != "" && reason != "Video generation failed" {
		return task.FailReason
	}
	var snapshot map[string]any
	if common.Unmarshal(task.Data, &snapshot) != nil || snapshot["status"] != "failed" {
		return task.FailReason
	}
	var candidates []string
	switch failure := snapshot["error"].(type) {
	case string:
		candidates = append(candidates, failure)
	case map[string]any:
		if message, ok := failure["message"].(string); ok {
			candidates = append(candidates, message)
		}
	}
	if metadata, ok := snapshot["metadata"].(map[string]any); ok {
		if message, ok := metadata["url"].(string); ok {
			candidates = append(candidates, message)
		}
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || strings.HasPrefix(candidate, "/") {
			continue
		}
		if parsed, err := url.Parse(candidate); err == nil {
			if parsed.Host != "" {
				continue
			}
			switch strings.ToLower(parsed.Scheme) {
			case "http", "https", "data", "blob", "file", "ftp", "s3", "gs":
				continue
			}
		}
		if reason := SanitizeAsyncImageDiagnostic(candidate, common.LocalLogContentLimit); reason != "" {
			return reason
		}
	}
	return task.FailReason
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
