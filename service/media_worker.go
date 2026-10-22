package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
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
	Operation   string
	Channel     model.Channel
	Execution   *model.TaskExecutionSnapshot
	Billing     MediaBillingSnapshot
	PluginHash  string
	Details     *AsyncVideoRequestDetails `json:"request_details,omitempty"`
}

var SubmitAsyncVideoFunc func(context.Context, model.AsyncMediaJob, AsyncVideoRequest) error
var PersistAsyncVideoFunc func(context.Context, model.AsyncMediaJob, *model.Task) error

// ErrAsyncMediaDownloadTransport marks download failures for which the owner's
// browser may still reach the public result. Invalid media and rejected URLs
// must never use this fallback.
var ErrAsyncMediaDownloadTransport = errors.New("async media download transport failed")

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
			if _, err := DispatchAsyncMediaJobs(ctx, cfg); err != nil {
				common.SysError("media queue: " + err.Error())
			}
		}
	}()
	return cancel
}

// DispatchAsyncMediaJobs starts one bounded pass over each stage. The returned
// group allows shutdown callers and regression tests to await this pass.
func DispatchAsyncMediaJobs(ctx context.Context, cfg MediaRuntimeConfig) (*sync.WaitGroup, error) {
	var workers sync.WaitGroup
	if common.RDB == nil || !common.RedisEnabled {
		return &workers, nil
	}
	cfg = cfg.withWorkerDefaults()
	pools := []struct {
		phase       string
		concurrency int
		timeout     int
	}{
		{phase: "submit", concurrency: cfg.SubmitConcurrency, timeout: cfg.SubmitTimeout},
		{phase: "status", concurrency: cfg.StatusConcurrency, timeout: 30},
		{phase: "download", concurrency: cfg.DownloadConcurrency, timeout: cfg.DownloadTimeout},
	}
	redis := common.RDB
	for _, pool := range pools {
		jobs, err := model.GetDueAsyncMediaJobsForPhase(ctx, time.Now().Unix(), pool.concurrency, pool.phase)
		if err != nil {
			return &workers, err
		}
		slotKey := ImageRedisKey("video-media-" + pool.phase + "-slots")
		if pool.phase == "download" {
			// Preserve the existing download budget during rolling upgrades.
			slotKey = ImageRedisKey("video-media-slots")
		}
		for _, job := range jobs {
			lease := common.GetUUID()
			leaseSeconds := pool.timeout + 120
			slot, err := imageInvocationAcquire.Run(ctx, redis, []string{slotKey}, time.Now().Unix(), time.Now().Unix()+int64(leaseSeconds), pool.concurrency, lease, leaseSeconds).Int()
			if err != nil {
				return &workers, err
			}
			if slot != 1 {
				continue
			}
			claimed, err := model.ClaimAsyncMediaJob(ctx, &job, lease, leaseSeconds)
			if err != nil || !claimed {
				_ = redis.ZRem(ctx, slotKey, lease).Err()
				if err != nil {
					return &workers, err
				}
				continue
			}
			workers.Go(func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						common.SysError("media worker panic; lease recovery will reconcile the job")
					}
				}()
				defer redis.ZRem(context.WithoutCancel(ctx), slotKey, lease)
				workerCtx, cancel := context.WithTimeout(ctx, time.Duration(pool.timeout)*time.Second)
				defer cancel()
				if err := ProcessAsyncMediaJob(workerCtx, job, cfg); err != nil {
					common.SysError("media task " + job.TaskId + ": " + err.Error())
				}
			})
		}
	}
	return &workers, nil
}

func ProcessAsyncMediaJob(parent context.Context, job model.AsyncMediaJob, cfg MediaRuntimeConfig) error {
	cfg = cfg.withWorkerDefaults()
	timeout := cfg.DownloadTimeout
	if job.Status == "queued" || job.Status == "submitting" {
		timeout = cfg.SubmitTimeout
	} else if job.StorageStatus != "ready" && job.StorageStatus != "saving" && job.StorageStatus != "failed" {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Second)
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
		updates["status"], updates["quota"], updates["request_cipher"] = "submitted", task.Quota, retainAsyncVideoRequestDetails(job)
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
		if job.Status != "submitted" || (job.StorageStatus != "ready" && job.StorageStatus != "saving" && job.StorageStatus != "failed") {
			// Release the submit/status lease before entering the download pool.
			updates["storage_status"], updates["next_attempt_at"] = "ready", 0
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
		previousResultURL := task.PrivateData.ResultURL
		if err := PersistAsyncVideoFunc(ctx, job, &task); err != nil {
			var storageErr *os.PathError
			fallbackAllowed := errors.Is(err, ErrAsyncMediaDownloadTransport) || errors.As(err, &storageErr)
			if fallbackAllowed && job.StorageRetries >= cfg.StorageRetries && PublicUpstreamAsyncVideoResultURL(&task) != "" {
				if task.PrivateData.ResultURL != previousResultURL {
					updated := model.DB.WithContext(ctx).Model(&model.Task{}).Where("id = ? AND status = ?", task.ID, model.TaskStatusSuccess).Update("private_data", task.PrivateData)
					if updated.Error != nil || updated.RowsAffected != 1 {
						return errors.New("updated upstream video result could not be saved")
					}
				}
				updates["storage_status"], updates["status"], updates["storage_retries"] = "upstream", "succeeded", job.StorageRetries+1
				updates["error_message"], updates["next_attempt_at"] = "", 0
				return nil
			}
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
	if job.Status == "submitted" || job.DispatchedAt > 0 || job.BillingStatus == "reserving" {
		reason := "Upstream submission could not be confirmed; the video operation will not be repeated"
		if detail := VideoTaskFailureNotice(job.ErrorMessage).Detail; detail != "" {
			if strings.Contains(detail, reason) {
				reason = detail
			} else {
				reason += "\n" + detail
			}
		}
		updates["status"], updates["billing_status"], updates["error_message"], updates["next_attempt_at"] = "execution_unknown", "unknown", VideoTaskFailureReason(reason), 0
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
		updates["status"], updates["request_cipher"] = "submitted", retainAsyncVideoRequestDetails(job)
		return nil
	}
	// The durable Task barrier may have been crossed before a billing error.
	var accepted model.Task
	if err := model.DB.WithContext(context.WithoutCancel(parent)).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&accepted).Error; err == nil {
		updates["status"], updates["billing_status"], updates["quota"], updates["request_cipher"], updates["error_message"] = "submitted", "failed", accepted.Quota, retainAsyncVideoRequestDetails(job), "Task billing confirmation requires reconciliation"
		return submitErr
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	reason := submitErr.Error()
	updates["status"], updates["billing_status"], updates["next_attempt_at"] = "failed", "refunded", 0
	if current.DispatchedAt > 0 {
		updates["status"], updates["billing_status"] = "execution_unknown", "unknown"
		reason = "Upstream submission could not be confirmed; the video operation will not be repeated\n" + reason
	}
	updates["error_message"] = VideoTaskFailureReason(reason)
	return submitErr
}
