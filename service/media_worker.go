package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/types"
	"gorm.io/gorm"
)

type MediaBillingSnapshot struct {
	ImageQuotaBeforeGroup float64
	Price                 types.PriceData
	Ratios                map[string]float64
	Tiered                *billingexpr.BillingSnapshot
	Clamp                 *common.QuotaClamp
}

type AsyncVideoRequest struct {
	Body        []byte
	ContentType string
	ClientIP    string
	Channel     model.Channel
	Execution   *model.TaskExecutionSnapshot
	Billing     MediaBillingSnapshot
	PluginHash  string
}

var SubmitAsyncVideoFunc func(context.Context, model.AsyncMediaJob, AsyncVideoRequest) error
var PersistAsyncVideoFunc func(context.Context, model.AsyncMediaJob, *model.Task) error

func StartAsyncMediaWorkers(parent context.Context) context.CancelFunc {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		timer := time.NewTicker(3 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			cfg, err := GetMediaRuntimeConfig(ctx)
			if err != nil {
				common.SysError("media configuration: " + err.Error())
				continue
			}
			if err := CleanupMediaObjects(ctx); err != nil {
				common.SysError("media cleanup: " + err.Error())
			}
			var jobs []model.AsyncMediaJob
			if err := model.DB.WithContext(ctx).Where("status IN ? AND lease_expires_at <= ? AND next_attempt_at <= ?", []string{"queued", "submitting", "submitted"}, time.Now().Unix(), time.Now().Unix()).Order("created_at").Limit(cfg.DownloadConcurrency).Find(&jobs).Error; err != nil {
				common.SysError("media queue: " + err.Error())
				continue
			}
			for _, job := range jobs {
				lease := common.GetUUID()
				// Redis bounds download concurrency across gateway processes.
				if common.RDB == nil || !common.RedisEnabled {
					continue
				}
				slot, err := imageInvocationAcquire.Run(ctx, common.RDB, []string{ImageRedisKey("video-media-slots")}, time.Now().Unix(), time.Now().Unix()+int64(cfg.DownloadTimeout+120), cfg.DownloadConcurrency, lease, cfg.DownloadTimeout+120).Int()
				if err != nil || slot != 1 {
					continue
				}
				claimed, err := model.ClaimAsyncMediaJob(ctx, &job, lease, cfg.DownloadTimeout+120)
				if err != nil || !claimed {
					_ = common.RDB.ZRem(ctx, ImageRedisKey("video-media-slots"), lease).Err()
					continue
				}
				go func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							common.SysError("media worker panic; lease recovery will reconcile the job")
						}
					}()
					defer common.RDB.ZRem(context.WithoutCancel(ctx), ImageRedisKey("video-media-slots"), lease)
					if err := ProcessAsyncMediaJob(ctx, job, cfg); err != nil {
						common.SysError("media task " + job.TaskId + ": " + err.Error())
					}
				}()
			}
		}
	}()
	return cancel
}

func ProcessAsyncMediaJob(parent context.Context, job model.AsyncMediaJob, cfg MediaRuntimeConfig) error {
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.DownloadTimeout)*time.Second)
	defer cancel()
	updates := map[string]any{"lease_token": "", "lease_expires_at": 0, "next_attempt_at": time.Now().Unix() + 3}
	defer func() {
		if err := model.UpdateAsyncMediaJob(context.WithoutCancel(parent), job, updates); err != nil {
			common.SysError("media lease confirmation: " + err.Error())
		} else if updates["status"] == "succeeded" || updates["status"] == "expired" {
			var current model.AsyncMediaJob
			cleanupCtx := context.WithoutCancel(parent)
			if err := model.DB.WithContext(cleanupCtx).Where("id = ?", job.ID).Take(&current).Error; err == nil {
				if err := RemoveAsyncMediaOutput(current); err != nil {
					common.SysError("media output cleanup: " + err.Error())
				} else {
					_ = model.DB.WithContext(cleanupCtx).Model(&current).Updates(map[string]any{"output_root_path": "", "output_object_key": ""}).Error
				}
			}
		}
	}()
	if job.ExpiresAt <= time.Now().Unix() {
		updates["status"], updates["storage_status"], updates["request_cipher"], updates["next_attempt_at"] = "expired", "expired", nil, 0
		return nil
	}
	var task model.Task
	err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&task).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil {
		updates["status"], updates["quota"], updates["request_cipher"] = "submitted", task.Quota, nil
		if job.BillingStatus != "failed" {
			updates["billing_status"] = "reserved"
			if task.Status == model.TaskStatusSuccess {
				updates["billing_status"] = "settled"
			}
			if task.Status == model.TaskStatusFailure {
				updates["billing_status"] = "refunded"
			}
		}
		if task.Status == model.TaskStatusFailure {
			updates["status"], updates["error_message"], updates["next_attempt_at"] = "failed", task.FailReason, 0
			return nil
		}
		if task.Status != model.TaskStatusSuccess {
			return nil
		}
		if PersistAsyncVideoFunc == nil {
			return errors.New("media persistence is unavailable")
		}
		if err := model.DB.WithContext(ctx).Where("id = ? AND lease_token = ?", job.ID, job.LeaseToken).Take(&job).Error; err != nil {
			return err
		}
		if err := model.UpdateAsyncMediaJob(ctx, job, map[string]any{"storage_status": "saving"}); err != nil {
			return err
		}
		if err := PersistAsyncVideoFunc(ctx, job, &task); err != nil {
			updates["storage_status"], updates["storage_retries"], updates["error_message"] = "failed", job.StorageRetries+1, "Generated video could not be saved locally"
			if job.StorageRetries >= cfg.StorageRetries {
				updates["next_attempt_at"] = int64(1 << 62)
			} else {
				updates["next_attempt_at"] = time.Now().Unix() + 30*(1<<min(job.StorageRetries, 8))
			}
			return err
		}
		updates["storage_status"], updates["status"], updates["error_message"], updates["next_attempt_at"] = "succeeded", "succeeded", "", 0
		return nil
	}
	if job.DispatchedAt > 0 || job.BillingStatus == "reserving" {
		updates["status"], updates["billing_status"], updates["error_message"], updates["next_attempt_at"] = "execution_unknown", "unknown", "Upstream submission could not be confirmed; generation will not be repeated", 0
		return nil
	}
	if SubmitAsyncVideoFunc == nil {
		return errors.New("async video submission is unavailable")
	}
	data, err := DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
	if err != nil {
		updates["status"], updates["error_message"], updates["next_attempt_at"] = "failed", "Encrypted video request is unavailable", 0
		return err
	}
	var request AsyncVideoRequest
	if err := common.Unmarshal(data, &request); err != nil {
		return err
	}
	if err := model.UpdateAsyncMediaJob(ctx, job, map[string]any{"status": "submitting"}); err != nil {
		return err
	}
	submitErr := SubmitAsyncVideoFunc(ctx, job, request)
	var current model.AsyncMediaJob
	if err := model.DB.WithContext(context.WithoutCancel(parent)).Where("id = ? AND lease_token = ?", job.ID, job.LeaseToken).Take(&current).Error; err != nil {
		return err
	}
	if submitErr == nil {
		updates["status"], updates["request_cipher"] = "submitted", nil
		return nil
	}
	// The durable Task barrier may have been crossed before a billing error.
	var accepted model.Task
	if err := model.DB.WithContext(context.WithoutCancel(parent)).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&accepted).Error; err == nil {
		updates["status"], updates["billing_status"], updates["quota"], updates["request_cipher"], updates["error_message"] = "submitted", "failed", accepted.Quota, nil, "Task billing confirmation requires reconciliation"
		return submitErr
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	updates["status"], updates["billing_status"], updates["error_message"], updates["next_attempt_at"] = "failed", "refunded", "Video submission failed", 0
	if current.DispatchedAt > 0 {
		updates["status"], updates["billing_status"], updates["error_message"] = "execution_unknown", "unknown", "Upstream submission could not be confirmed; generation will not be repeated"
	}
	return submitErr
}
