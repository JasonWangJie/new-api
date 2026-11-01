package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	"gorm.io/gorm"
)

// CurrentMediaRetryPreviewState keeps the selected error's evidence while
// refreshing durable budgets. A historical sample can never reset live usage.
func CurrentMediaRetryPreviewState(ctx context.Context, taskID, mediaType string, recorded mediaretry.State) (mediaretry.State, error) {
	cfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return recorded, err
	}
	current := mediaretry.State{TotalLimit: cfg.TotalRetries, Now: time.Now().Unix()}
	switch mediaType {
	case "image":
		var task model.AsyncImageTask
		if err := model.DB.WithContext(ctx).Where("task_id = ?", taskID).Take(&task).Error; err != nil {
			return recorded, err
		}
		var checkpoint model.ImageRetryPolicyState
		if task.RetryPolicyState != "" {
			if err := common.UnmarshalJsonStr(task.RetryPolicyState, &checkpoint); err != nil {
				return recorded, err
			}
			current = checkpoint.State
		}
		current.TotalUsed = task.RetryCount
		if checkpoint.ReservedRetry {
			current.TotalUsed = max(0, current.TotalUsed-1)
		}
		current.TotalLimit = cfg.TotalRetries
	case "video":
		var job model.AsyncMediaJob
		if err := model.DB.WithContext(ctx).Where("task_id = ?", taskID).Take(&job).Error; err != nil {
			return recorded, err
		}
		var task model.Task
		err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", taskID, job.UserId).Take(&task).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return recorded, err
		}
		if task.ID > 0 {
			current = videoRetryState(task, job)
		} else {
			plain, err := DecryptImagePayload(job.RequestCipher, "media-request:"+taskID)
			if err != nil {
				return recorded, err
			}
			var request AsyncVideoRequest
			if err := common.Unmarshal(plain, &request); err != nil {
				return recorded, err
			}
			if request.InitialRetryState != nil {
				current = *request.InitialRetryState
			}
		}
		current.TotalLimit = min(current.TotalLimit, cfg.TotalRetries)
	default:
		return recorded, errors.New("unsupported media recovery context")
	}
	rules := make(map[string]int, len(current.RuleUses)+len(recorded.RuleUses))
	steps := make(map[string]int, len(current.StepUses)+len(recorded.StepUses))
	for key, value := range current.RuleUses {
		rules[key] = max(0, value)
	}
	for key, value := range recorded.RuleUses {
		rules[key] = max(rules[key], value)
	}
	for key, value := range current.StepUses {
		steps[key] = max(0, value)
	}
	for key, value := range recorded.StepUses {
		steps[key] = max(steps[key], value)
	}
	current.RuleUses, current.StepUses = rules, steps
	current.TotalUsed = max(recorded.TotalUsed, current.TotalUsed)
	current.Now = time.Now().Unix()
	return current, nil
}
