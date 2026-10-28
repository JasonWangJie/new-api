package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
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
	Body                      []byte
	ContentType               string
	ClientIP                  string
	Operation                 string
	Channel                   model.Channel
	Execution                 *model.TaskExecutionSnapshot
	Billing                   MediaBillingSnapshot
	PluginHash                string
	Details                   *AsyncVideoRequestDetails  `json:"request_details,omitempty"`
	ReferenceInputs           []AsyncVideoReferenceInput `json:"reference_inputs,omitempty"`
	ReferenceRecoveryVersion  int                        `json:"reference_recovery_version,omitempty"`
	ReferenceRecoveryDeadline int64                      `json:"reference_recovery_deadline,omitempty"`
	ReferenceURLExpiresAt     int64                      `json:"reference_url_expires_at,omitempty"`
}

var SubmitAsyncVideoFunc func(context.Context, model.AsyncMediaJob, AsyncVideoRequest) error
var PersistAsyncVideoFunc func(context.Context, model.AsyncMediaJob, *model.Task) error

// recordAsyncVideoSubmissionRefund records only confirmed reservation rollbacks.
// Missing consumption and its gross usage projection share a durable outbox,
// without changing financial counters. Submit failures have not accumulated
// settled usage, so RefundTaskQuota cannot be used.
func recordAsyncVideoSubmissionRefund(c *gin.Context, info *relaycommon.RelayInfo, quota int, refundErr error) {
	if info.TaskRelayInfo == nil || info.TaskRelayInfo.PublicTaskID == "" {
		return
	}
	taskID := info.TaskRelayInfo.PublicTaskID
	updates := map[string]any{"billing_status": "refunded", "quota": 0}
	var refund *model.RecordTaskBillingLogParams
	if refundErr != nil {
		updates = map[string]any{"billing_status": "failed"}
	} else if quota == 0 {
		updates["billing_status"] = "not_billable"
	} else {
		other := model.NewLogOther()
		other.SetPublic("is_task", true)
		other.SetPublic("task_id", taskID)
		other.SetPublic("reason", "Task submission failed; reserved quota returned")
		other.SetPublic("billing_source", info.BillingSource)
		other.SetPublic("request_path", c.Request.URL.Path)
		if info.SubscriptionId > 0 {
			other.SetPublic("subscription_id", info.SubscriptionId)
		}
		attachQuotaSaturation(c, info, other)
		refund = &model.RecordTaskBillingLogParams{
			UserId: info.UserId, LogType: model.LogTypeRefund, ChannelId: info.ChannelId,
			ModelName: info.OriginModelName, Quota: quota, TokenId: info.TokenId,
			Group: info.UsingGroup, Other: other, RequestId: c.GetString(common.RequestIdKey),
		}
	}
	ctx := context.WithoutCancel(c.Request.Context())
	if err := model.SaveAsyncMediaSubmissionRefund(ctx, taskID, info.UserId, updates, refund); err != nil {
		logger.LogError(c, "video submission refund audit "+taskID+": "+err.Error())
	}
}

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
			refundLogCtx, refundLogCancel := context.WithTimeout(ctx, 2*time.Second)
			refundLogErr := model.ReplayAsyncMediaRefundLogs(refundLogCtx, 100)
			refundLogCancel()
			if refundLogErr != nil {
				common.SysError("media refund log delivery: " + refundLogErr.Error())
			}
			cfg, err := GetMediaRuntimeConfig(ctx)
			if err != nil {
				common.SysError("media configuration: " + err.Error())
				continue
			}
			if err := CleanupMediaObjects(ctx); err != nil {
				common.SysError("media cleanup: " + err.Error())
			}
			if err := CleanupMediaInputs(ctx, time.Now().Unix()); err != nil {
				common.SysError("media input cleanup: " + err.Error())
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
	if job.Status == "failed" || job.Status == "expired" || job.Status == "execution_unknown" {
		if job.Status == "execution_unknown" {
			if job.ReferenceRecoveryStage == "unknown" {
				return nil
			}
			return model.UpdateAsyncMediaJob(parent, job, map[string]any{"status": "failed", "next_attempt_at": 0})
		}
		return nil
	}
	if job.ReferenceRecoveryStage == "prepare" || job.ReferenceRecoveryStage == "submit" || job.ReferenceRecoveryStage == "dispatched" || job.ReferenceRecoveryStage == "unknown" || job.ReferenceRecoveryStage == "refund" {
		return ProcessAsyncVideoReferenceRecovery(parent, job, cfg.withWorkerDefaults())
	}
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
		if updates["billing_status"] == "unknown" {
			// A previous lease holder may confirm its refund after this worker's
			// read. Preserve that evidence atomically when releasing this lease.
			updates["billing_status"] = gorm.Expr("CASE WHEN billing_status IN ('refunded','not_billable','failed') THEN billing_status ELSE ? END", "unknown")
		}
		if err := model.UpdateAsyncMediaJob(context.WithoutCancel(parent), job, updates); err != nil {
			common.SysError("media lease confirmation: " + err.Error())
			return
		}
		if updates["status"] == "succeeded" || updates["status"] == "failed" || updates["status"] == "submitted" {
			var task model.Task
			if err := model.DB.WithContext(context.WithoutCancel(parent)).Where("task_id = ? AND attempt_generation = ?", job.TaskId, job.AttemptGeneration).Take(&task).Error; err == nil && (task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure) {
				if err := RetireMediaInputObjects(context.WithoutCancel(parent), &task); err != nil {
					common.SysError("media input retirement: " + err.Error())
				}
			}
		}
		if updates["status"] == "succeeded" || updates["status"] == "expired" {
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
		retentionJob := job
		if task.Status == model.TaskStatusFailure || task.Status == model.TaskStatusSuccess {
			// Confirmed generation completion releases the replay envelope even
			// while generated output still waits for the storage worker.
			retentionJob.Status = "failed"
		}
		updates["status"], updates["quota"], updates["request_cipher"] = "submitted", task.Quota, retainAsyncVideoRequestDetails(retentionJob)
		if job.BillingStatus != "failed" {
			updates["billing_status"] = "reserved"
			if task.Status == model.TaskStatusSuccess {
				updates["billing_status"] = "settled"
			}
		}
		if task.Status == model.TaskStatusFailure {
			if job.BillingStatus != "failed" {
				updates["billing_status"] = "failed"
				if task.Quota == 0 {
					updates["billing_status"] = "refunded"
				}
			}
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
					updated := model.DB.WithContext(ctx).Model(&model.Task{}).Where("id = ? AND status = ? AND attempt_generation = ?", task.ID, model.TaskStatusSuccess, task.AttemptGeneration).Update("private_data", task.PrivateData)
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
	if job.Status == "submitted" || job.DispatchedAt > 0 || job.BillingStatus == "reserving" || job.BillingStatus == "reserved" {
		reason := "Upstream submission could not be confirmed; the video operation will not be repeated"
		if detail := VideoTaskFailureNotice(job.ErrorMessage).Detail; detail != "" {
			if strings.Contains(detail, reason) {
				reason = detail
			} else {
				reason += "\n" + detail
			}
		}
		billingStatus := job.BillingStatus
		if billingStatus != "refunded" && billingStatus != "not_billable" && billingStatus != "failed" {
			billingStatus = "unknown"
		}
		updates["status"], updates["billing_status"], updates["error_message"], updates["next_attempt_at"] = "failed", billingStatus, VideoTaskFailureReason(reason), 0
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
	updates["status"], updates["billing_status"], updates["next_attempt_at"] = "failed", current.BillingStatus, 0
	if current.BillingStatus != "refunded" && current.BillingStatus != "not_billable" && current.BillingStatus != "failed" {
		updates["billing_status"] = "not_billable"
		if current.DispatchedAt > 0 || current.BillingStatus == "reserving" || current.BillingStatus == "reserved" {
			updates["billing_status"] = "unknown"
		}
	}
	if current.DispatchedAt > 0 && updates["billing_status"] == "unknown" {
		reason = "Upstream submission could not be confirmed; the video operation will not be repeated\n" + reason
	}
	updates["error_message"] = VideoTaskFailureReason(reason)
	return submitErr
}
