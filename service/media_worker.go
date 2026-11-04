package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
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
	ConditionalRouting               *VideoRoutingSnapshot `json:"conditional_routing,omitempty"`
	MixedRouting                     *MixedVideoRouting    `json:"mixed_routing,omitempty"`
	Body                             []byte
	ContentType                      string
	ClientIP                         string
	Operation                        string
	Channel                          model.Channel
	Execution                        *model.TaskExecutionSnapshot
	InitialExecution                 *model.TaskExecutionSnapshot `json:"initial_execution,omitempty"`
	Billing                          MediaBillingSnapshot
	InitialBillingChannelID          int                        `json:"initial_billing_channel_id,omitempty"`
	ReservationFailure               *BillingReservationFailure `json:"billing_reservation_failure,omitempty"`
	InitialRetryState                *mediaretry.State          `json:"initial_retry_state,omitempty"`
	InitialUnknownAttempts           []model.VideoTaskAttempt   `json:"initial_unknown_attempts,omitempty"`
	RetryFailedAt                    int64                      `json:"retry_failed_at,omitempty"`
	RetryFact                        *mediaretry.Fact           `json:"retry_fact,omitempty"`
	PendingDecision                  *mediaretry.Decision       `json:"pending_retry_decision,omitempty"`
	ReferenceLocalizationUnsupported bool                       `json:"reference_localization_unsupported,omitempty"`
	LocalReferencesReady             bool                       `json:"local_references_ready,omitempty"`
	VideoReferenceTransportMode      string                     `json:"video_reference_transport_mode,omitempty"`
	InlineReferenceInputs            []AsyncVideoReferenceInput `json:"inline_reference_inputs,omitempty"`
	OriginalBody                     []byte                     `json:"original_body,omitempty"`
	PluginHash                       string
	Details                          *AsyncVideoRequestDetails  `json:"request_details,omitempty"`
	ReferenceInputs                  []AsyncVideoReferenceInput `json:"reference_inputs,omitempty"`
	ReferenceRecoveryVersion         int                        `json:"reference_recovery_version,omitempty"`
	ReferenceRecoveryDeadline        int64                      `json:"reference_recovery_deadline,omitempty"`
	ReferenceURLExpiresAt            int64                      `json:"reference_url_expires_at,omitempty"`
}

func (request AsyncVideoRequest) HasReferenceInputs() bool {
	return len(request.ReferenceInputs)+len(request.InlineReferenceInputs) > 0
}

var SubmitAsyncVideoFunc func(context.Context, model.AsyncMediaJob, AsyncVideoRequest) error
var PersistAsyncVideoFunc func(context.Context, model.AsyncMediaJob, *model.Task) error

const asyncMediaAgeRetryLeasePrefix = "age-limit-retry:"

// A failed pre-consumption has no BillingSession for the relay's refund defer.
// Persist only proven outcomes; an ambiguous database write retains reserving
// so worker recovery continues to require reconciliation.
func recordAsyncVideoPreConsumeFailure(c *gin.Context, info *relaycommon.RelayInfo, outcome billingPreConsumeOutcome) {
	if c == nil || info == nil || info.TaskRelayInfo == nil || info.PublicTaskID == "" || !c.GetBool("async_media_execution") || outcome == billingPreConsumeUnknown {
		return
	}
	leaseToken := c.GetString("async_media_execution_lease_token")
	if leaseToken == "" {
		return
	}
	if c.GetBool("async_media_policy_unknown_ended") || c.GetBool("async_media_reconciliation_dispatched") && !c.GetBool("async_media_submission_rejected") {
		return
	}
	if value, exists := c.Get("async_media_initial_retry_request"); exists {
		if request, ok := value.(*AsyncVideoRequest); ok && request != nil && len(request.InitialUnknownAttempts) > 0 {
			return
		}
	}
	status := "not_billable"
	if outcome == billingPreConsumeRollbackFailed {
		status = "failed"
	}
	err := model.DB.WithContext(context.WithoutCancel(c.Request.Context())).Model(&model.AsyncMediaJob{}).
		Where("task_id = ? AND user_id = ? AND token_id = ? AND lease_token = ? AND attempt_generation = ? AND COALESCE(dispatched_at, 0) = ? AND quota = ? AND billing_status = ?", info.PublicTaskID, info.UserId, info.TokenId, leaseToken, 0, 0, 0, "reserving").
		Update("billing_status", status).Error
	if err != nil {
		logger.LogError(c, "video pre-consumption outcome "+info.PublicTaskID+": "+err.Error())
	}
}

// RecordAsyncVideoReservationFailure preserves unequal or uncertain holds when
// a dispatched operation cannot safely enter the ordinary refund path.
func RecordAsyncVideoReservationFailure(c *gin.Context, info *relaycommon.RelayInfo) {
	if c == nil || info == nil || info.TaskRelayInfo == nil || info.PublicTaskID == "" || !c.GetBool("async_media_execution") {
		return
	}
	session, ok := info.Billing.(*BillingSession)
	if !ok {
		return
	}
	failure := session.ReservationFailure()
	leaseToken := c.GetString("async_media_execution_lease_token")
	if failure == nil || leaseToken == "" {
		return
	}
	updates := map[string]any{"billing_status": "failed", "error_message": "Video billing adjustment requires reconciliation"}
	if value, exists := c.Get("async_media_initial_retry_request"); exists {
		if request, valid := value.(*AsyncVideoRequest); valid && request != nil {
			request.ReservationFailure = failure
			encoded, err := common.Marshal(request)
			if err == nil {
				var cipher []byte
				cipher, err = EncryptImagePayload(encoded, "media-request:"+info.PublicTaskID)
				if err == nil {
					updates["request_cipher"] = cipher
				}
			}
			if err != nil {
				logger.LogError(c, "video reservation failure snapshot "+info.PublicTaskID+": "+err.Error())
			}
		}
	}
	logger.LogWarn(c, fmt.Sprintf("video reservation requires reconciliation task=%s funding=%s funding_quota=%d token_quota=%d paired_quota=%d attempted_quota=%d uncertain=%t stage=%s",
		info.PublicTaskID, failure.FundingSource, failure.ConfirmedFundingQuota, failure.ConfirmedTokenQuota,
		failure.PairedQuota, failure.AttemptedQuota, failure.Uncertain, failure.Stage))
	err := model.DB.WithContext(context.WithoutCancel(c.Request.Context())).Model(&model.AsyncMediaJob{}).
		Where("task_id = ? AND user_id = ? AND token_id = ? AND lease_token = ? AND attempt_generation = ?", info.PublicTaskID, info.UserId, info.TokenId, leaseToken, 0).
		Where("billing_status IN ?", []string{"reserving", "reserved", "unknown", "failed"}).
		Updates(updates).Error
	if err != nil {
		logger.LogError(c, "video reservation reconciliation "+info.PublicTaskID+": "+err.Error())
	}
}

// recordAsyncVideoSubmissionRefund records only confirmed reservation rollbacks.
// Missing consumption and its gross usage projection share a durable outbox,
// without changing financial counters. Submit failures have not accumulated
// settled usage, so RefundTaskQuota cannot be used.
func recordAsyncVideoSubmissionRefund(c *gin.Context, info *relaycommon.RelayInfo, quota int, refundErr error) {
	if info.TaskRelayInfo == nil || info.TaskRelayInfo.PublicTaskID == "" {
		return
	}
	taskID := info.TaskRelayInfo.PublicTaskID
	billingChannelID := c.GetInt("async_media_initial_billing_channel_id")
	if billingChannelID <= 0 {
		billingChannelID = info.ChannelId
	}
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
			UserId: info.UserId, LogType: model.LogTypeRefund, ChannelId: billingChannelID,
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
		nextRetryEventCleanup := time.Time{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if err := FailOverdueAsyncMediaJobs(ctx, 100); err != nil {
				common.SysError("media maximum age cleanup: " + err.Error())
			}
			refundLogCtx, refundLogCancel := context.WithTimeout(ctx, 2*time.Second)
			refundLogErr := model.ReplayAsyncMediaRefundLogs(refundLogCtx, 100)
			refundLogCancel()
			if refundLogErr != nil {
				common.SysError("media refund log delivery: " + refundLogErr.Error())
			}
			if now := time.Now(); !now.Before(nextRetryEventCleanup) {
				cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 2*time.Second)
				cleanupErr := model.PruneExpiredMediaRetryEvents(cleanupCtx, now, 1000)
				cleanupCancel()
				nextRetryEventCleanup = now.Add(time.Minute)
				if cleanupErr != nil {
					common.SysError("media recovery event cleanup: " + cleanupErr.Error())
				}
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
	if job.CreatedAt > 0 && job.CreatedAt <= time.Now().Unix()-model.AsyncMediaMaxTaskAgeSeconds && job.Status != "succeeded" {
		if job.ReferenceRecoveryStage == "age_limit_refund" {
			return completeAsyncMediaAgeRefund(parent, job)
		}
		stopped, err := model.FailAgedAsyncMediaJob(parent, job, time.Now().Unix(), true)
		if errors.Is(err, model.ErrImageConflict) {
			return nil
		}
		if err != nil {
			return err
		}
		return completeAsyncMediaAgeRefund(parent, stopped)
	}
	if job.CreatedAt > 0 {
		var cancelAge context.CancelFunc
		parent, cancelAge = context.WithDeadline(parent, time.Unix(job.CreatedAt+model.AsyncMediaMaxTaskAgeSeconds, 0))
		defer cancelAge()
	}
	if job.Status == "failed" || job.Status == "expired" || job.Status == "execution_unknown" {
		if job.Status == "execution_unknown" {
			var task model.Task
			if err := model.DB.WithContext(parent).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&task).Error; err == nil {
				if applied, err := tryUnknownVideoRecovery(parent, task, job, job.ErrorMessage); err != nil || applied {
					return err
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if job.ReferenceRecoveryStage == "unknown" || VideoJobRequiresReconciliation(job) && job.DispatchedAt > 0 && job.BillingStatus == "unknown" {
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
	var task model.Task
	err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&task).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if job.ExpiresAt <= time.Now().Unix() {
		if err == nil && task.PrivateData.AsyncMedia && task.PrivateData.VideoReconciliation == nil && task.PrivateData.UpstreamTaskID != "" && task.Quota > 0 && task.Status != model.TaskStatusFailure && task.Status != model.TaskStatusSuccess && job.BillingStatus != "refunded" && job.BillingStatus != "not_billable" {
			if err := reconcileAsyncVideoTask(ctx, &task, task.Snapshot(), "retention_expired", 0); err != nil {
				return err
			}
			// The task transaction also changes the job projection. Keep this
			// lease's current status so its deferred release cannot undo that write.
			if err := model.DB.WithContext(ctx).Where("id = ? AND lease_token = ? AND attempt_generation = ?", job.ID, job.LeaseToken, job.AttemptGeneration).Take(&job).Error; err != nil {
				return err
			}
			if err := model.DB.WithContext(ctx).First(&task, task.ID).Error; err != nil {
				return err
			}
		}
		if err != nil || !task.PrivateData.AsyncMedia || task.PrivateData.VideoReconciliation == nil || task.Status == model.TaskStatusFailure || task.Status == model.TaskStatusSuccess {
			updates["status"], updates["storage_status"], updates["request_cipher"], updates["next_attempt_at"] = "expired", "expired", nil, 0
			return nil
		}
	}
	if err == nil {
		retentionJob := job
		if task.Status == model.TaskStatusFailure || task.Status == model.TaskStatusSuccess {
			// Confirmed generation completion releases the replay envelope even
			// while generated output still waits for the storage worker.
			retentionJob.Status = "failed"
		}
		updates["status"], updates["quota"], updates["request_cipher"] = "submitted", task.Quota, retainAsyncVideoRequestDetails(retentionJob)
		if task.PrivateData.VideoReconciliation != nil && task.Status != model.TaskStatusFailure && task.Status != model.TaskStatusSuccess {
			updates["status"] = "reconciling"
			updates["request_cipher"] = job.RequestCipher
		}
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
		if job.StorageRetries > 0 && task.PrivateData.MediaRetryFact != nil && task.PrivateData.MediaRetryFact.Stage == "download" {
			decision, policyErr := VideoPostprocessDecision(ctx, job, &task, "download", "", true)
			if policyErr != nil {
				return policyErr
			}
			if decision.Matched && (decision.Action != mediaretry.ResumeDownload || job.StorageRetries > cfg.StorageRetries) {
				updates["storage_status"], updates["next_attempt_at"] = "failed", int64(1<<62)
				return nil
			}
		}
		if err := model.UpdateAsyncMediaJob(ctx, job, map[string]any{"storage_status": "saving"}); err != nil {
			return err
		}
		previousResultURL := task.PrivateData.ResultURL
		if err := PersistAsyncVideoFunc(ctx, job, &task); err != nil {
			decision, policyErr := VideoPostprocessDecision(context.WithoutCancel(ctx), job, &task, "download", VideoTaskFailureReason(err.Error()), false)
			if policyErr != nil {
				return policyErr
			}
			if decision.Matched {
				updates["storage_status"], updates["storage_retries"], updates["error_message"] = "failed", job.StorageRetries+1, "Generated video could not be saved locally"
				updates["next_attempt_at"] = int64(1 << 62)
				if decision.Action == mediaretry.ResumeDownload && job.StorageRetries < cfg.StorageRetries {
					updates["next_attempt_at"] = time.Now().Unix() + int64(max(1, decision.DelaySeconds))
				}
				return err
			}
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
	if job.Status == "submitted" || job.Status == "reconciling" || job.DispatchedAt > 0 || job.BillingStatus == "reserving" || job.BillingStatus == "reserved" {
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
		if VideoJobRequiresReconciliation(job) && job.DispatchedAt > 0 && billingStatus == "unknown" {
			updates["status"] = "execution_unknown"
		}
		return nil
	}
	switch job.BillingStatus {
	case "failed", "unknown", "refunded", "not_billable":
		// Billing can finish before the submit worker persists its terminal
		// status. Reclaiming that lease must not reserve again or erase an
		// outstanding debit left by an unsuccessful compensation.
		reason := job.ErrorMessage
		if reason == "" {
			reason = "Video submission ended before completion; the video operation will not be repeated"
		}
		updates["status"], updates["error_message"], updates["next_attempt_at"] = "failed", VideoTaskFailureReason(reason), 0
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
		updates["status"], updates["request_cipher"] = "submitted", retainAsyncVideoRequestDetails(current)
		return nil
	}
	// The durable Task barrier may have been crossed before a billing error.
	var accepted model.Task
	if err := model.DB.WithContext(context.WithoutCancel(parent)).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&accepted).Error; err == nil {
		updates["status"], updates["billing_status"], updates["quota"], updates["request_cipher"], updates["error_message"] = "submitted", "failed", accepted.Quota, retainAsyncVideoRequestDetails(current), "Task billing confirmation requires reconciliation"
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
		if VideoJobRequiresReconciliation(current) {
			updates["status"] = "execution_unknown"
		}
		reason = "Upstream submission could not be confirmed; the video operation will not be repeated\n" + reason
	}
	updates["error_message"] = VideoTaskFailureReason(reason)
	return submitErr
}

// FailOverdueAsyncMediaJobs also retries durable timeout refunds. It runs
// independently of submission enablement, Redis worker slots, and retention.
func FailOverdueAsyncMediaJobs(ctx context.Context, limit int) error {
	if limit <= 0 {
		return nil
	}
	now := time.Now().Unix()
	var jobs []model.AsyncMediaJob
	query := model.ExcludePendingSubmissionReconciliation(model.DB.WithContext(ctx)).
		Where("created_at > 0 AND (created_at <= ? OR (status = ? AND reference_recovery_stage = ?))", now-model.AsyncMediaMaxTaskAgeSeconds, "failed", "age_limit_refund").
		Where("NOT (status = ? AND EXISTS (SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status NOT IN ?))", "reconciling", []model.TaskStatus{model.TaskStatusSuccess, model.TaskStatusFailure}).
		Where("COALESCE(lease_token, '') NOT LIKE ? OR COALESCE(lease_expires_at, 0) <= ?", asyncMediaAgeRetryLeasePrefix+"%", now).
		Where("status NOT IN ? OR (status = ? AND reference_recovery_stage = ? AND next_attempt_at <= ?)", []string{"failed", "succeeded"}, "failed", "age_limit_refund", now).
		Where("status <> ? OR (billing_status <> ? AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status = ?))", "expired", "settled", model.TaskStatusSuccess).
		Order("CASE WHEN COALESCE(lease_token, '') LIKE '" + asyncMediaAgeRetryLeasePrefix + "%' THEN 1 ELSE 0 END").
		Order("next_attempt_at").Order("id").Limit(limit)
	err := query.Find(&jobs).Error
	if err != nil {
		return err
	}
	var failures []error
	for _, job := range jobs {
		candidate := job
		err = nil
		if job.ReferenceRecoveryStage != "age_limit_refund" {
			var stopped model.AsyncMediaJob
			stopped, err = model.FailAgedAsyncMediaJob(ctx, job, now, true)
			if err == nil {
				job, candidate = stopped, stopped
			}
		}
		if err == nil {
			err = completeAsyncMediaAgeRefund(ctx, job)
		}
		if err == nil {
			continue
		}
		// Rotate every failed phase, including native-read and final-refund
		// errors. Retried rows remain behind untouched paused operations even
		// after this lease expires; ordinary active leases remain revocable.
		rotationErr := model.DB.WithContext(context.WithoutCancel(ctx)).Model(&model.AsyncMediaJob{}).
			Where("id = ? AND status = ? AND attempt_generation = ? AND COALESCE(reference_recovery_stage, '') = ? AND COALESCE(lease_token, '') = ?", candidate.ID, candidate.Status, candidate.AttemptGeneration, candidate.ReferenceRecoveryStage, candidate.LeaseToken).
			Updates(map[string]any{"lease_token": asyncMediaAgeRetryLeasePrefix + common.GetUUID(), "lease_expires_at": now + 60}).Error
		if !errors.Is(err, model.ErrImageConflict) {
			failures = append(failures, err)
		}
		if rotationErr != nil {
			failures = append(failures, rotationErr)
		}
	}
	return errors.Join(failures...)
}

// TerminateExpiredAsyncMediaJob uses the same local end-of-life policy for
// historical unsuccessful records, without asserting upstream verification.
func TerminateExpiredAsyncMediaJob(ctx context.Context, job model.AsyncMediaJob) error {
	automatic := job.CreatedAt > 0 && job.CreatedAt <= time.Now().Unix()-model.AsyncMediaMaxTaskAgeSeconds
	stopped, err := model.FailAgedAsyncMediaJob(ctx, job, time.Now().Unix(), automatic)
	if err != nil {
		return err
	}
	return completeAsyncMediaAgeRefund(ctx, stopped)
}

// The controller must require the existing explicit upstream-check confirmation.
func ReconcileTerminatedAsyncMediaReservation(ctx context.Context, job model.AsyncMediaJob) error {
	reconciled, err := model.ReconcileAgedAsyncMediaReservation(ctx, job)
	if err != nil {
		return err
	}
	return completeAsyncMediaAgeRefund(ctx, reconciled)
}

func completeAsyncMediaAgeRefund(ctx context.Context, job model.AsyncMediaJob) error {
	if job.ReferenceRecoveryStage != "age_limit_refund" {
		return nil
	}
	var task model.Task
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ? AND attempt_generation = ?", job.TaskId, job.UserId, job.AttemptGeneration).Take(&task).Error; err != nil {
		return err
	}
	if task.Status != model.TaskStatusFailure || task.Quota != 0 {
		if task.Status != model.TaskStatusUnknown || !task.PrivateData.DurableBilling || !task.PrivateData.SubmitAccountingRecorded {
			return model.ErrTaskAttemptConflict
		}
		task.Status, task.FailReason, task.Progress, task.FinishTime = model.TaskStatusFailure, job.ErrorMessage, "100%", time.Now().Unix()
		won, err := SettleTaskBillingOnComplete(ctx, nil, &task, model.TaskStatusUnknown, relaycommon.FailTaskInfo(task.FailReason))
		if err != nil {
			_ = model.DB.WithContext(context.WithoutCancel(ctx)).Model(&model.AsyncMediaJob{}).
				Where("id = ? AND status = ? AND attempt_generation = ? AND reference_recovery_stage = ?", job.ID, "failed", job.AttemptGeneration, "age_limit_refund").
				Update("next_attempt_at", time.Now().Unix()+60).Error
			return err
		}
		if !won {
			return model.ErrTaskAttemptConflict
		}
	}
	query := model.DB.WithContext(ctx).Model(&model.AsyncMediaJob{}).
		Where("id = ? AND status = ? AND attempt_generation = ? AND reference_recovery_stage = ?", job.ID, "failed", job.AttemptGeneration, "age_limit_refund")
	return model.ChangeAsyncMediaJob(query, map[string]any{"billing_status": "refunded", "quota": 0, "reference_recovery_stage": "age_limit_complete", "next_attempt_at": 0, "lease_token": "", "lease_expires_at": 0, "request_cipher": nil, "channel_cipher": nil}, "")
}
