package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AsyncImageOutput struct {
	Images       []ImageBytes
	UpstreamURLs []string
}

type AsyncImageExecutor func(context.Context, model.AsyncImageTask, AsyncImageRequest, *model.Channel, ImageRuntimeConfig, func() error) (*AsyncImageOutput, model.AsyncImageBill, error)

var ExecuteAsyncImageFunc AsyncImageExecutor

// The host supplies the existing model-price evaluator without executing an
// upstream request or reserving funds at admission.
var EstimateAsyncImageQuotaFunc func(context.Context, model.Token, AsyncImageRequest, string) (int, error)

var imageLeaseRenew = redis.NewScript(`if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('EXPIRE',KEYS[1],ARGV[2]) else return 0 end`)
var imageLeaseRelease = redis.NewScript(`if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) else return 0 end`)
var imageQueuePop = redis.NewScript(`local item=redis.call('ZRANGE',KEYS[1],0,0,'WITHSCORES'); if #item == 0 or tonumber(item[2]) > tonumber(ARGV[1]) then return '' end; redis.call('ZREM',KEYS[1],item[1]); return item[1]`)
var imageInvocationAcquire = redis.NewScript(`redis.call('ZREMRANGEBYSCORE',KEYS[1],'-inf',ARGV[1]); if redis.call('ZCARD',KEYS[1]) >= tonumber(ARGV[3]) then return 0 end; redis.call('ZADD',KEYS[1],ARGV[2],ARGV[4]); redis.call('EXPIRE',KEYS[1],ARGV[5]); return 1`)
var imageInvocationRenew = redis.NewScript(`for _, key in ipairs(KEYS) do if not redis.call('ZSCORE',key,ARGV[1]) then return 0 end end; for _, key in ipairs(KEYS) do redis.call('ZADD',key,'XX',ARGV[2],ARGV[1]); redis.call('EXPIRE',key,ARGV[3]) end; return 1`)

func StartAsyncImageWorkers(parent context.Context) context.CancelFunc {
	ctx, cancel := context.WithCancel(parent)
	if !common.RedisEnabled || common.RDB == nil || ExecuteAsyncImageFunc == nil {
		return cancel
	}
	cfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		common.SysError("image workers configuration unavailable")
		return cancel
	}
	var workers sync.WaitGroup
	for range cfg.Workers {
		workers.Go(func() {
			defer func() {
				if recover() != nil {
					common.SysError("image worker stopped after an unexpected failure")
				}
			}()
			asyncImageWorkerLoop(ctx)
		})
	}
	workers.Go(func() {
		defer func() {
			if recover() != nil {
				common.SysError("image outbox dispatcher stopped after an unexpected failure")
			}
		}()
		asyncImageOutboxLoop(ctx)
	})
	workers.Go(func() {
		defer func() {
			if recover() != nil {
				common.SysError("image recovery stopped after an unexpected failure")
			}
		}()
		asyncImageRecoveryLoop(ctx, cfg.RecoveryInterval)
	})
	return func() {
		cancel()
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			common.SysError("image workers are stopping; durable leases will recover unfinished work")
		}
	}
}

func asyncImageOutboxLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	failing := false
	for {
		if err := deliverAsyncImageOutbox(ctx); err != nil {
			if !failing && ctx.Err() == nil {
				common.SysError("image outbox dispatcher is waiting for database or Redis availability")
			}
			failing = true
		} else {
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func asyncImageWorkerLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			id, err := imageQueuePop.Run(ctx, common.RDB, []string{ImageRedisKey("queue")}, time.Now().Unix()).Text()
			if err != nil || id == "" {
				continue
			}
			if err := RunAsyncImageTask(ctx, id); err != nil && !errors.Is(err, model.ErrImageConflict) && !errors.Is(err, context.Canceled) {
				common.SysError("image worker task " + id + ": " + err.Error())
			}
		}
	}
}

func asyncImageRecoveryLoop(ctx context.Context, interval int) {
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	for {
		if err := RebuildAsyncImageQueue(ctx); err != nil && ctx.Err() == nil {
			common.SysError("image recovery is waiting for database or Redis availability")
		}
		if cfg, err := GetImageRuntimeConfig(ctx); err == nil {
			if err := MaintainImageStorage(ctx, cfg); err != nil && ctx.Err() == nil {
				common.SysError("image storage maintenance is waiting for database or storage availability")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func RebuildAsyncImageQueue(ctx context.Context) error {
	if common.RDB == nil {
		return errors.New("Redis is required for image scheduling")
	}
	cfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	if err := model.RecoverAsyncImageTasks(ctx, 100, cfg.ExecutionTimeout, cfg.FailedRequestRetentionHours); err != nil {
		return err
	}
	if err := deliverAsyncImageOutbox(ctx); err != nil {
		return err
	}
	var tasks []model.AsyncImageTask
	statuses := []string{model.ImageTaskQueued, model.ImageTaskInvoking, model.ImageTaskUpstreamSucceeded, model.ImageTaskUploading, model.ImageTaskBillingPending, model.ImageTaskStorageFailed, model.ImageTaskBillingFailed}
	if err := model.DB.WithContext(ctx).Where("status IN ? AND lease_expires_at <= ? AND next_attempt_at > 0", statuses, time.Now().Unix()).Order("next_attempt_at,id").Limit(100).Find(&tasks).Error; err != nil {
		return err
	}
	for _, task := range tasks {
		if err := common.RDB.ZAdd(ctx, ImageRedisKey("queue"), &redis.Z{Score: float64(task.NextAttemptAt), Member: task.TaskId}).Err(); err != nil {
			return err
		}
	}
	return nil
}

func deliverAsyncImageOutbox(ctx context.Context) error {
	if common.RDB == nil {
		return errors.New("Redis is required for image scheduling")
	}
	var commands []model.ImageOutbox
	if err := model.DB.WithContext(ctx).Where("status = ? AND kind IN ? AND next_attempt_at <= ?", "pending", []string{"execute", "postprocess"}, time.Now().Unix()).Order("id").Limit(100).Find(&commands).Error; err != nil {
		return err
	}
	for _, command := range commands {
		if err := common.RDB.ZAdd(ctx, ImageRedisKey("queue"), &redis.Z{Score: float64(command.NextAttemptAt), Member: command.AggregateId}).Err(); err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Model(&model.ImageOutbox{}).Where("id = ? AND status = ?", command.Id, "pending").Update("status", "delivered").Error; err != nil {
			return err
		}
	}
	return nil
}

func RunAsyncImageTask(parent context.Context, id string) (runError error) {
	defer func() {
		if recover() != nil {
			runError = errors.New("image invocation interrupted; durable recovery will reconcile its dispatch state")
		}
	}()
	cfg, err := GetImageRuntimeConfig(parent)
	if err != nil {
		return err
	}
	lease := common.GetUUID()
	leaseKey := ImageRedisKey("lease", id)
	acquired, err := common.RDB.SetNX(parent, leaseKey, lease, time.Duration(cfg.WorkerLease)*time.Second).Result()
	if err != nil {
		return err
	}
	if !acquired {
		return model.ErrImageConflict
	}
	defer imageLeaseRelease.Run(context.WithoutCancel(parent), common.RDB, []string{leaseKey}, lease)
	task, err := model.ClaimAsyncImageTask(parent, id, lease, cfg.WorkerLease)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(time.Duration(max(1, cfg.WorkerLease/3)) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				value, err := imageLeaseRenew.Run(ctx, common.RDB, []string{leaseKey}, lease, cfg.WorkerLease).Int()
				if err != nil || value != 1 {
					cancel()
					return
				}
				if err := model.HeartbeatAsyncImageTask(ctx, id, lease, cfg.WorkerLease); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	if task.UpstreamSucceededAt > 0 && len(task.RequestCipher) > 0 && task.ImageCount == 0 && (task.Status == model.ImageTaskStorageFailed || task.Status == model.ImageTaskBillingFailed) {
		if err := model.TransitionImageTask(ctx, task, map[string]any{"status": model.ImageTaskInvoking}, "completion_resumed", "", ""); err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", id, lease).Take(&task).Error; err != nil {
			return err
		}
	}
	if task.Status == model.ImageTaskInvoking {
		if err := invokeAsyncImageTask(ctx, &task, cfg); err != nil {
			return err
		}
	}
	if err := model.DB.WithContext(ctx).Where("task_id = ?", id).Take(&task).Error; err != nil {
		return err
	}
	if task.LeaseToken != lease {
		return nil
	}
	if task.Status == model.ImageTaskUpstreamSucceeded || task.Status == model.ImageTaskUploading || task.Status == model.ImageTaskStorageFailed {
		if err := UploadAsyncImageTask(ctx, &task, cfg); err != nil {
			return scheduleImagePostprocessRetry(context.WithoutCancel(parent), task, cfg, err, true)
		}
	}
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", id, lease).Take(&task).Error; err != nil {
		return err
	}
	if task.Status == model.ImageTaskBillingFailed {
		if err := model.TransitionImageTask(ctx, task, map[string]any{"status": model.ImageTaskBillingPending}, "billing_retry", "", ""); err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Where("task_id = ?", id).Take(&task).Error; err != nil {
			return err
		}
	}
	if task.Status != model.ImageTaskBillingPending {
		return model.ErrImageConflict
	}
	var bill model.AsyncImageBill
	if err := model.DB.WithContext(ctx).Where("task_id = ?", id).Take(&bill).Error; err != nil {
		return err
	}
	if err := model.ApplyAsyncImageBill(ctx, id, bill.Fingerprint); err != nil {
		return scheduleImagePostprocessRetry(context.WithoutCancel(parent), task, cfg, err, false)
	}
	if err := model.RefreshAsyncImageBillingCache(ctx, id); err != nil {
		return scheduleImagePostprocessRetry(context.WithoutCancel(parent), task, cfg, err, false)
	}
	if err := model.ConfirmAsyncImageLog(ctx, id); err != nil {
		return scheduleImagePostprocessRetry(context.WithoutCancel(parent), task, cfg, err, false)
	}
	return model.CompleteAsyncImageTask(ctx, task, cfg.AutoArchive)
}

func invokeAsyncImageTask(ctx context.Context, task *model.AsyncImageTask, cfg ImageRuntimeConfig) error {
	knownUpstreamTask := task.UpstreamTaskId != ""
	acceptedOutput := task.UpstreamSucceededAt > 0
	data, err := DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
	if err != nil {
		if knownUpstreamTask || acceptedOutput {
			return fmt.Errorf("accepted upstream image task request could not be decrypted: %w", err)
		}
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 604, InternalCode: "request_decryption_failed", Message: "Encrypted request is unavailable"}, cfg)
	}
	var request AsyncImageRequest
	if err := common.Unmarshal(data, &request); err != nil {
		if knownUpstreamTask || acceptedOutput {
			return fmt.Errorf("accepted upstream image task request is invalid: %w", err)
		}
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 604, Message: "Persisted request is invalid"}, cfg)
	}
	acceptedOutput = acceptedOutput || request.Completion != nil
	if request.MixedRouting != nil && task.DispatchedAt > 0 && !knownUpstreamTask && !acceptedOutput {
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 608, InternalCode: "execution_unknown", Message: "Mixed image submission was interrupted after dispatch; administrator reconciliation is required", ExecutionUnknown: true}, cfg)
	}
	if acceptedOutput && request.Completion == nil && !knownUpstreamTask {
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 607, InternalCode: "completion_checkpoint_invalid", Message: "Accepted image output is missing its durable completion evidence"}, cfg)
	}
	request.Reservation, err = model.GetAsyncImageReservation(ctx, *task)
	if err != nil {
		return err
	}
	var attempts []ImageChannelAttempt
	if task.Attempts != "" {
		if err := common.UnmarshalJsonStr(task.Attempts, &attempts); err != nil {
			return err
		}
	}
	var channel *model.Channel
	var account ImageAccount
	if knownUpstreamTask || acceptedOutput {
		if request.Reservation == nil {
			return errors.New("accepted upstream image task is missing its billing reservation")
		}
		if acceptedOutput && request.Reservation.Status != "reserved" {
			return errors.New("accepted image output requires an unsettled billing reservation")
		}
		// Poll an accepted job with the account and provider configuration that
		// created it, even when administrators disable new submissions.
		if len(task.SelectedChannelCipher) == 0 {
			return errors.New("accepted upstream image task is missing its frozen channel configuration")
		}
		data, decodeErr := DecryptImagePayload(task.SelectedChannelCipher, "image-channel:"+task.TaskId)
		if decodeErr != nil {
			return decodeErr
		}
		channel = &model.Channel{}
		if decodeErr := common.Unmarshal(data, channel); decodeErr != nil {
			return decodeErr
		}
		account = ImageAccount{Fingerprint: ImageIdentityHash("image-account", strconv.Itoa(channel.Id), channel.Key)}
	} else {
		var token model.Token
		if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", task.TokenId, task.UserId).Take(&token).Error; err != nil {
			return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 604, Message: "Token is unavailable"}, cfg)
		}
		policy, _, err := ValidateAsyncImageEligibility(ctx, token, request, cfg)
		if err != nil || policy.Group != task.Group {
			return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 604, InternalCode: "eligibility_changed", Message: "Image eligibility or platform group changed before execution"}, cfg)
		}
		routing := ImageAccountAttemptRouting(attempts, cfg.ReferenceRetries, request.Platform, cfg.GeminiAccountSwitches)
		if routing.Stop {
			failure := &AsyncImageFailure{Code: 603, InternalCode: "account_switches_exhausted", Message: "Image account switching budget is exhausted"}
			for _, attempt := range attempts {
				if attempt.ReferenceFailure {
					failure.Code, failure.InternalCode, failure.Message = 602, "reference_accounts_exhausted", "Upstream reference-image fetch retries are exhausted"
					break
				}
			}
			return failAsyncImageInvocation(ctx, *task, failure, cfg)
		}
		// A safe retry has no accepted upstream job. Select from the current
		// enabled keys, retry exclusions and circuit state instead of restoring
		// the prior attempt's channel snapshot.
		channel, account, err = PickImageChannel(ctx, policy, request, routing)
		if err != nil {
			if errors.Is(err, ErrImageCompatibleAccountsExhausted) {
				failure := &AsyncImageFailure{Code: 603, InternalCode: "image_compatible_accounts_exhausted", Message: "No other eligible image account supports the requested size or resolution"}
				for i := len(attempts) - 1; i >= 0; i-- {
					compatibilityCode := imageAttemptCompatibilityCode(attempts[i])
					if compatibilityCode == "" {
						continue
					}
					if compatibilityCode == "upstream_image_reference_incompatible" {
						failure.InternalCode, failure.Message = "image_reference_accounts_exhausted", "No other eligible image account supports the reference input"
					}
					break
				}
				return failAsyncImageInvocation(ctx, *task, failure, cfg)
			}
			if errors.Is(err, ErrImageTimeoutAccountsExhausted) {
				return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 606, InternalCode: "timeout_accounts_exhausted", Message: "No unused image account remains after request timeouts"}, cfg)
			}
			if errors.Is(err, ErrImageLocalControlsUnavailable) {
				return DeferAsyncImageSubmission(ctx, *task, cfg, "routing_controls_unavailable", 10, 0, 0)
			}
			if errors.Is(err, ErrImageLocalCapacityBusy) {
				return DeferAsyncImageSubmission(ctx, *task, cfg, "local_capacity_busy", 10, 0, 0)
			}
			var cooling *ImageAccountCooling
			if errors.As(err, &cooling) {
				return DeferAsyncImageSubmission(ctx, *task, cfg, "account_cooldown", cooling.DelaySeconds, 0, 0)
			}
			return failAsyncImageInvocation(ctx, *task, ClassifyAsyncImageFailure(err, 0), cfg)
		}
	}
	now := time.Now().Unix()
	remaining := int64(cfg.ExecutionTimeout) - (now - task.StartedAt)
	if !acceptedOutput && remaining <= 0 {
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 608, InternalCode: "execution_deadline", Message: "Image execution deadline expired", ExecutionUnknown: task.DispatchedAt > 0 || knownUpstreamTask}, cfg)
	}
	ctx, cancelInvocation := context.WithCancel(ctx)
	var renewers sync.WaitGroup
	defer func() {
		cancelInvocation()
		renewers.Wait()
	}()
	if request.Completion == nil {
		slotKey := ImageRedisKey("invoke-slots")
		channelSlotKey := ImageChannelConcurrencyKey(channel.Id)
		slotTTL := cfg.AttemptTimeout + cfg.WorkerLease
		settings := channel.GetSetting()
		if settings.ImageAttemptTimeoutSeconds != nil && *settings.ImageAttemptTimeoutSeconds > 0 {
			slotTTL = min(*settings.ImageAttemptTimeoutSeconds, 86400) + cfg.WorkerLease
		}
		channelConcurrency := cfg.ImageConcurrency
		if settings.ImageConcurrencyLimit != nil && *settings.ImageConcurrencyLimit > 0 {
			channelConcurrency = min(channelConcurrency, *settings.ImageConcurrencyLimit)
		}
		slot, err := imageInvocationAcquire.Run(ctx, common.RDB, []string{slotKey}, now, now+int64(slotTTL), cfg.ImageConcurrency, task.LeaseToken, slotTTL).Int()
		if err != nil {
			return err
		}
		if slot != 1 {
			if knownUpstreamTask {
				return model.ScheduleAsyncImagePoll(context.WithoutCancel(ctx), *task, now+10, "capacity_wait", "Image execution capacity is busy")
			}
			return DeferAsyncImageSubmission(ctx, *task, cfg, "local_capacity_busy", 10, 0, 0)
		}
		defer common.RDB.ZRem(context.WithoutCancel(ctx), slotKey, task.LeaseToken)
		channelSlot, err := imageInvocationAcquire.Run(ctx, common.RDB, []string{channelSlotKey}, now, now+int64(slotTTL), channelConcurrency, task.LeaseToken, slotTTL).Int()
		if err != nil {
			return err
		}
		if channelSlot != 1 {
			if knownUpstreamTask {
				return model.ScheduleAsyncImagePoll(context.WithoutCancel(ctx), *task, now+10, "capacity_wait", "Channel image execution capacity is busy")
			}
			return DeferAsyncImageSubmission(ctx, *task, cfg, "local_capacity_busy", 10, 0, 0)
		}
		defer common.RDB.ZRem(context.WithoutCancel(ctx), channelSlotKey, task.LeaseToken)
		leaseToken := task.LeaseToken
		renewers.Go(func() {
			defer func() {
				if recover() != nil {
					cancelInvocation()
				}
			}()
			ticker := time.NewTicker(time.Duration(max(1, cfg.WorkerLease/3)) * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					retained, err := imageInvocationRenew.Run(ctx, common.RDB, []string{slotKey, channelSlotKey}, leaseToken, time.Now().Unix()+int64(slotTTL), slotTTL).Int()
					if err != nil || retained != 1 {
						cancelInvocation()
						return
					}
				}
			}
		})
	}
	task.ChannelId = channel.Id
	capability := ImageChannelCapability(*channel, request.Model)
	upstreamAsyncConfigured := channel.GetOtherSettings().UpstreamAsync != nil
	if !knownUpstreamTask && !acceptedOutput {
		encodedChannel, err := common.Marshal(channel)
		if err != nil {
			return err
		}
		cipher, err := EncryptImagePayload(encodedChannel, "image-channel:"+task.TaskId)
		if err != nil {
			return err
		}
		provider := capability.Provider
		if provider == "" && upstreamAsyncConfigured {
			provider = "upstream_async"
		}
		if err := model.TransitionImageTask(ctx, *task, map[string]any{"channel_id": channel.Id, "provider": provider, "execution_protocol": capability.Protocol, "selected_channel_cipher": cipher}, "provider_selected", "", ""); err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error; err != nil {
			return err
		}
	}
	mode := ImageChannelReferenceTransport(*channel, request.Platform, task.ReferenceRetryCount, cfg)
	if !knownUpstreamTask && !acceptedOutput {
		previousDispatch := false
		for _, attempt := range attempts {
			previousDispatch = previousDispatch || attempt.Dispatched
		}
		if previousDispatch && cfg.SupplierRetryCapacity > 0 {
			wait, budgetErr := AcquireImageSupplierRetry(ctx, *channel, request.Model, cfg)
			if budgetErr != nil {
				return DeferAsyncImageSubmission(ctx, *task, cfg, "supplier_retry_budget_unavailable", 10, 0, 0)
			}
			if wait > 0 {
				return DeferAsyncImageSubmission(ctx, *task, cfg, "supplier_retry_budget_exhausted", wait, cfg.SupplierRetryCapacity, cfg.SupplierRetryCapacity)
			}
			defer func() {
				if task.DispatchedAt == 0 {
					_ = ReturnImageSupplierRetry(context.WithoutCancel(ctx), *channel, request.Model, cfg)
				}
			}()
		}
		selection := ImageRetryDecision{Version: 1, Stage: "submit", Action: "initial_submission", Reason: "eligible_account", Rule: "account_selection", TotalUsed: task.RetryCount, TotalLimit: cfg.TotalRetries, RecordedAt: now, Deadline: task.StartedAt + int64(cfg.ExecutionTimeout)}
		if len(attempts) > 0 {
			last := attempts[len(attempts)-1]
			selection.Action = "same_account_retry"
			if last.KeyFingerprint != "" && last.KeyFingerprint != account.Fingerprint || last.KeyFingerprint == "" && last.ChannelId != channel.Id {
				selection.Action = "switch_account"
			}
		}
		referenceFallback := false
		if mode == "local" {
			for _, previous := range attempts {
				referenceFallback = referenceFallback || previous.Dispatched && previous.ReferenceFailure && (previous.TransportUsed == "remote" || previous.TransportUsed == "mixed")
			}
		}
		if referenceFallback {
			selection.Action, selection.Reason = "local_reference_fallback", "reference_transport_fallback"
		}
		upstreamModel, err := ResolveImageChannelUpstreamModel(*channel, request.Model)
		if err != nil {
			return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 604, InternalCode: "invalid_model_mapping", Message: "Image channel model mapping is invalid"}, cfg)
		}
		attempts = append(attempts, ImageChannelAttempt{AuditVersion: 1, Decision: &selection, Provider: capability.Provider, Protocol: capability.Protocol, UpstreamModel: upstreamModel, ChannelId: channel.Id, KeyFingerprint: account.Fingerprint, CredentialFingerprint: account.CredentialFingerprint, RetryOnTimeout: request.MixedRouting == nil, KeyIndex: account.Index, StartedAt: now, ReferenceMode: mode, ReferenceFallback: referenceFallback})
		encoded, err := common.Marshal(attempts)
		if err != nil {
			return err
		}
		decisionJSON, err := common.Marshal(selection)
		if err != nil {
			return err
		}
		if err := model.TransitionImageTask(ctx, *task, map[string]any{"channel_id": channel.Id, "attempts": string(encoded), "retry_decision": string(decisionJSON)}, "channel_selected", "", ""); err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error; err != nil {
			return err
		}
	}
	attemptCtx := ctx
	attemptTimeout := int64(cfg.AttemptTimeout)
	if request.Completion == nil {
		var cancel context.CancelFunc
		settings := channel.GetSetting()
		if settings.ImageAttemptTimeoutSeconds != nil && *settings.ImageAttemptTimeoutSeconds > 0 {
			attemptTimeout = int64(min(*settings.ImageAttemptTimeoutSeconds, 86400))
		}
		if !acceptedOutput {
			attemptTimeout = min(attemptTimeout, remaining)
		}
		attemptCtx, cancel = context.WithTimeout(ctx, time.Duration(attemptTimeout)*time.Second)
		defer cancel()
	}
	request.PostprocessContext = ctx
	transportUsed := ""
	request.ReferenceTransportUsed = &transportUsed
	beforeInvoke := func() error {
		if acceptedOutput || knownUpstreamTask {
			return model.ErrImageConflict
		}
		if attemptCtx.Err() != nil {
			return attemptCtx.Err()
		}
		if len(attempts) > 0 {
			attempts[len(attempts)-1].TransportUsed = transportUsed
			attempts[len(attempts)-1].Dispatched = true
			encoded, err := common.Marshal(attempts)
			if err != nil {
				return err
			}
			task.Attempts = string(encoded)
		}
		if err := model.MarkAsyncImageDispatched(attemptCtx, *task, channel.Id); err != nil {
			return err
		}
		return model.DB.WithContext(attemptCtx).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error
	}
	output, bill, err := ExecuteAsyncImageFunc(attemptCtx, *task, request, channel, cfg, beforeInvoke)
	if reloadErr := model.DB.WithContext(context.WithoutCancel(ctx)).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error; reloadErr != nil {
		return reloadErr
	}
	if err != nil {
		var pending *AsyncImagePending
		if errors.As(err, &pending) {
			if err := model.DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error; err != nil {
				return err
			}
			return model.ScheduleAsyncImagePoll(context.WithoutCancel(ctx), *task, time.Now().Unix()+10, "upstream_pending", "")
		}
		if task.DispatchedAt == 0 && task.UpstreamTaskId == "" && task.UpstreamSucceededAt == 0 {
			if errors.Is(err, model.ErrImageQuotaCacheUnavailable) {
				return DeferAsyncImageSubmission(context.WithoutCancel(ctx), *task, cfg, "quota_cache_unavailable", 10, 0, 0)
			}
			if errors.Is(err, model.ErrImageQuotaCacheUncertain) {
				return failAsyncImageInvocation(context.WithoutCancel(ctx), *task, &AsyncImageFailure{Code: 608, InternalCode: "quota_cache_uncertain", Message: "The quota reservation could not be confirmed; generation has not started and administrator reconciliation is required", ExecutionUnknown: true}, cfg)
			}
		}
		failure := ClassifyAsyncImageFailure(err, 0)
		var networkError net.Error
		if ctx.Err() != nil && failure.InternalCode == "upstream_transport_timeout" {
			failure.InternalCode = "upstream_transport_canceled"
			failure.Message = "Image execution was interrupted; upstream execution could not be confirmed"
			failure.ExecutionUnknown = true
		}
		// A single-attempt timeout authorizes the configured account switch,
		// while parent cancellation (shutdown or lost leases) still requires
		// reconciliation. Preserve success milestones and accepted upstream IDs.
		if task.UpstreamSucceededAt == 0 && task.UpstreamTaskId == "" && task.DispatchedAt > 0 && ctx.Err() == nil && (errors.Is(attemptCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() || failure.InternalCode == "upstream_transport_timeout") {
			failure.Code, failure.InternalCode = 608, "upstream_transport_timeout"
			failure.ExecutionUnknown = task.UpstreamTaskId == ""
			failure.Message = "Upstream request timed out after dispatch; execution could not be confirmed"
		}
		if failure.InternalCode == "upstream_transport_timeout" && errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			failure.Message = fmt.Sprintf("%s (local request timeout: %d seconds)", failure.Message, attemptTimeout)
		}
		if task.DispatchedAt == 0 {
			diagnostic := SanitizeAsyncImageDiagnostic(err.Error(), asyncImageProviderMessageLimit)
			common.SysError(fmt.Sprintf("async image local preparation failed: task=%s channel=%d cause=%s", task.TaskId, task.ChannelId, diagnostic))
			if failure.Code == 610 {
				failure.InternalCode = "request_preparation_failed"
				failure.Message = "Image request preparation failed: " + diagnostic
			} else if failure.Code == 606 && failure.InternalCode == "upstream_failed" {
				failure.InternalCode = "request_preparation_timeout"
				failure.Message = "Temporary image request preparation failure: " + diagnostic
			}
		}
		if task.UpstreamSucceededAt == 0 && task.DispatchedAt > 0 && (ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			failure.ExecutionUnknown = true
		}
		if task.UpstreamSucceededAt == 0 && task.UpstreamTaskId != "" && failure.ExecutionUnknown {
			return model.ScheduleAsyncImagePoll(context.WithoutCancel(ctx), *task, time.Now().Unix()+10, "recovered", "Upstream image task polling will continue")
		}
		if task.UpstreamSucceededAt == 0 && task.DispatchedAt > 0 && (failure.Code == 605 || failure.Code == 606 || failure.HTTPStatus == 429 || failure.ExecutionUnknown) {
			_ = RecordImageCircuit(context.WithoutCancel(ctx), "async", channel.Id, false, cfg)
			if task.UpstreamTaskId == "" || failure.UpstreamTerminal {
				_ = RecordImageAccountCooldown(context.WithoutCancel(ctx), account.Fingerprint, failure, cfg)
			}
		}
		return failAsyncImageInvocation(context.WithoutCancel(ctx), *task, failure, cfg)
	}
	if !acceptedOutput {
		_ = RecordImageCircuit(context.WithoutCancel(ctx), "async", channel.Id, true, cfg)
	}
	if len(attempts) > 0 {
		attempts[len(attempts)-1].Dispatched = task.DispatchedAt > 0
		attempts[len(attempts)-1].FinishedAt = time.Now().Unix()
		attempts[len(attempts)-1].UpstreamTaskID = task.UpstreamTaskId
	}
	encoded, err := common.Marshal(attempts)
	if err != nil {
		return err
	}
	task.Attempts = string(encoded)
	if output == nil {
		return failAsyncImageInvocation(context.WithoutCancel(ctx), *task, &AsyncImageFailure{Code: 607, InternalCode: "output_parse_failed", Message: "Upstream image output is missing"}, cfg)
	}
	var persistErr error
	if len(output.UpstreamURLs) > 0 {
		if len(output.Images) > 0 {
			return model.ErrImageConflict
		}
		for _, resultURL := range output.UpstreamURLs {
			if !PublicUpstreamAsyncResultURL(resultURL) {
				return model.ErrImageConflict
			}
		}
		persistErr = model.StageAsyncImageUpstreamOutput(context.WithoutCancel(ctx), *task, output.UpstreamURLs, bill)
	} else {
		staging := make([]model.AsyncImageStagingObject, len(output.Images))
		for i, image := range output.Images {
			staging[i] = model.AsyncImageStagingObject{Data: image.Data, ContentType: image.ContentType, Checksum: image.Checksum, Width: image.Width, Height: image.Height}
		}
		persistErr = model.StageAsyncImageOutput(ctx, *task, staging, bill)
	}
	if persistErr != nil {
		if errors.Is(persistErr, model.ErrImageConflict) {
			return persistErr
		}
		if task.UpstreamTaskId != "" || task.UpstreamSucceededAt > 0 {
			return failAsyncImageInvocation(context.WithoutCancel(ctx), *task, &AsyncImageFailure{Code: 606, InternalCode: "output_persist_failed", Message: "Upstream output could not be durably persisted"}, cfg)
		}
		return failAsyncImageInvocation(context.WithoutCancel(ctx), *task, &AsyncImageFailure{Code: 608, InternalCode: "execution_unknown", Message: "Upstream output could not be durably persisted", ExecutionUnknown: true}, cfg)
	}
	return nil
}

func failAsyncImageInvocation(ctx context.Context, task model.AsyncImageTask, failure *AsyncImageFailure, cfg ImageRuntimeConfig) error {
	failure.Message = SanitizeAsyncImageDiagnostic(failure.Message, asyncImageProviderMessageLimit)
	failure.ProviderCode = limitAsyncImageProviderDiagnostic(failure.ProviderCode, asyncImageProviderCodeLimit)
	failure.ProviderStatus = limitAsyncImageProviderDiagnostic(failure.ProviderStatus, asyncImageProviderCodeLimit)
	failure.UpstreamRequestID = limitAsyncImageProviderDiagnostic(failure.UpstreamRequestID, asyncImageProviderRequestIDLimit)
	if failure.Message == "" {
		failure.Message = "Upstream image request failed"
	}
	mixedRouting := false
	if len(task.RequestCipher) > 0 {
		data, err := DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
		if err == nil {
			var request AsyncImageRequest
			if common.Unmarshal(data, &request) == nil {
				mixedRouting = request.MixedRouting != nil
			}
		}
	}
	if mixedRouting && task.UpstreamSucceededAt == 0 && task.UpstreamTaskId == "" && task.DispatchedAt > 0 && !failure.UpstreamTerminal && (failure.HTTPStatus < 400 || failure.HTTPStatus == http.StatusRequestTimeout || failure.HTTPStatus >= 500) {
		// A transport interruption or server error cannot prove non-acceptance.
		// Keep the hold and require reconciliation instead of crossing providers.
		failure.ExecutionUnknown = true
	}
	status, kind := model.ImageTaskFailed, ""
	updates := map[string]any{"public_error_code": failure.Code, "error_code": failure.InternalCode, "error_message": failure.Message, "lease_token": "", "lease_expires_at": 0, "request_cipher": nil, "finished_at": time.Now().Unix(), "next_attempt_at": 0}
	submissionTransientRetries := 0
	timeoutRetries := 0
	timeoutAttempt := task.UpstreamSucceededAt == 0 && task.UpstreamTaskId == "" && task.DispatchedAt > 0 && failure.InternalCode == "upstream_transport_timeout"
	compatibilityFailure := failure.InternalCode == "upstream_image_route_incompatible" || failure.InternalCode == "upstream_image_reference_incompatible"
	if task.Attempts != "" {
		var attempts []ImageChannelAttempt
		if err := common.UnmarshalJsonStr(task.Attempts, &attempts); err != nil {
			return err
		}
		for _, attempt := range attempts {
			if attempt.Dispatched && attempt.FinishedAt > 0 && attempt.TimedOut {
				timeoutRetries++
			}
			if attempt.Dispatched && attempt.FinishedAt > 0 && (attempt.Code == 605 || attempt.Code == 606) {
				submissionTransientRetries++
			}
		}
		if len(attempts) > 0 {
			last := &attempts[len(attempts)-1]
			// Selection and capacity failures can occur before a new attempt is
			// recorded. Never overwrite an earlier completed account failure.
			if last.FinishedAt == 0 && last.ChannelId == task.ChannelId {
				if task.UpstreamTaskId == "" || failure.UpstreamTerminal || task.UpstreamSucceededAt > 0 {
					last.FinishedAt = time.Now().Unix()
				}
				last.Dispatched = task.DispatchedAt > 0
				last.TimedOut = timeoutAttempt
				last.Code = failure.Code
				last.HTTPStatus = failure.HTTPStatus
				last.ErrorMessage = failure.Message
				last.ProviderCode = failure.ProviderCode
				last.ProviderStatus = failure.ProviderStatus
				last.UpstreamRequestID = failure.UpstreamRequestID
				last.UpstreamTaskID = task.UpstreamTaskId
				last.ReferenceFailure = last.Dispatched && (failure.ReferenceTransportFallback || failure.Code == 602 && (failure.InternalCode == "upstream_failed" || failure.UpstreamTerminal))
				encoded, err := common.Marshal(attempts)
				if err != nil {
					return err
				}
				updates["attempts"] = string(encoded)
			}
		}
	}
	if task.UpstreamSucceededAt > 0 {
		// The encrypted completion is authoritative. Retry only its local
		// download or billing phase, including after the generation deadline.
		key, count, maximum, paused := "storage_retry_count", task.StorageRetryCount, cfg.StorageRetries, model.ImageTaskStorageFailed
		if failure.InternalCode == "bill_preparation_failed" || failure.InternalCode == "billing_usage_missing" {
			key, count, maximum, paused = "billing_retry_count", task.BillingRetryCount, cfg.BillingRetries, model.ImageTaskBillingFailed
		}
		status = paused
		updates["request_cipher"] = task.RequestCipher
		updates["finished_at"] = 0
		updates["public_error_code"] = 609
		if count < maximum && failure.Code != 607 && failure.Code != 604 && failure.Code != 601 {
			status, kind = model.ImageTaskInvoking, "execute"
			updates[key] = count + 1
			updates["next_attempt_at"] = time.Now().Unix() + int64(cfg.RetryBackoff)*(1<<min(count, 10))
			if failure.InternalCode == "billing_usage_missing" && task.UpstreamTaskId != "" {
				// Re-query the same completed job for corrected usage. Retain the
				// success milestone so this can never cross the submit barrier.
				data, err := DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
				if err != nil {
					return err
				}
				var request AsyncImageRequest
				if err := common.Unmarshal(data, &request); err != nil {
					return err
				}
				request.Completion = nil
				data, err = common.Marshal(request)
				if err != nil {
					return err
				}
				cipher, err := EncryptImagePayload(data, "task:"+task.TaskId)
				if err != nil {
					return err
				}
				updates["request_cipher"] = cipher
			}
		}
	} else if timeoutAttempt && !mixedRouting {
		// Unknown execution belongs to the attempt. The task may continue on
		// another credential under the administrator's bounded retry policy.
		updates["public_error_code"] = 606
		updates["reconciliation_status"] = "upstream_unconfirmed"
		now := time.Now().Unix()
		deadline := task.StartedAt + int64(cfg.ExecutionTimeout)
		reservation, err := model.GetAsyncImageReservation(ctx, task)
		if err != nil {
			return err
		}
		if reservation == nil || reservation.Status != "reserved" {
			status = model.ImageTaskExecutionUnknown
			updates["public_error_code"] = 608
			updates["error_code"] = "quota_cache_uncertain"
			updates["error_message"] = "The quota reservation could not be confirmed; administrator reconciliation is required"
		} else if task.StartedAt > 0 && now >= deadline {
			updates["error_code"] = "timeout_deadline_exhausted"
			updates["error_message"] = "Image execution deadline expired after request timeouts"
		} else if timeoutRetries >= cfg.TimeoutRetries || task.RetryCount >= cfg.TotalRetries {
			updates["error_code"] = "timeout_retries_exhausted"
			updates["error_message"] = "Image request timeout retry budget is exhausted"
		} else {
			delay := ImageRetryDelay(cfg.TimeoutRetryBase, cfg.TimeoutRetryMax, timeoutRetries, cfg.RetryJitter, 0, cfg.RetryAfterMax)
			delay = min(max(delay, cfg.TimeoutRetryBase), cfg.TimeoutRetryMax)
			status, kind = model.ImageTaskQueued, "execute"
			updates["request_cipher"] = task.RequestCipher
			updates["finished_at"] = 0
			updates["retry_count"] = task.RetryCount + 1
			updates["next_attempt_at"] = now + int64(delay)
			if task.StartedAt > 0 {
				updates["next_attempt_at"] = min(updates["next_attempt_at"].(int64), deadline)
			}
			updates["channel_id"], updates["dispatched_at"] = 0, 0
			updates["selected_channel_cipher"] = nil
		}
	} else if failure.ExecutionUnknown {
		status = model.ImageTaskExecutionUnknown
		updates["public_error_code"] = 608
	} else if task.UpstreamTaskId != "" && !failure.UpstreamTerminal && (failure.InternalCode == "execution_deadline" || task.StartedAt > 0 && time.Now().Unix()-task.StartedAt >= int64(cfg.ExecutionTimeout) || failure.Code != 605 && failure.Code != 606) {
		// An expired or unreadable poll cannot prove that the accepted job
		// stopped. Keep its financial hold for administrator reconciliation.
		status = model.ImageTaskExecutionUnknown
		updates["public_error_code"] = 608
	} else if failure.InternalCode == "execution_deadline" {
		status = model.ImageTaskExpired
	} else {
		resubmit := task.UpstreamTaskId == ""
		if failure.UpstreamTerminal {
			resubmit = false
			if !mixedRouting && !compatibilityFailure && task.ImageCount == 0 && task.ResultCount == 0 && (failure.Code == 602 || failure.Code == 603 || failure.Code == 605 || failure.Code == 606) {
				reservation, err := model.GetAsyncImageReservation(ctx, task)
				if err != nil {
					return err
				}
				var resultMilestones int64
				if err := model.DB.WithContext(ctx).Model(&model.AsyncImageEvent{}).Where("task_id = ? AND event_type = ?", task.TaskId, "upstream_result_received").Count(&resultMilestones).Error; err != nil {
					return err
				}
				resubmit = reservation != nil && reservation.Status == "reserved" && resultMilestones == 0
			}
		}
		count, maximum, base, maxDelay, key := 0, 0, 0, 0, ""
		pollRetry := task.UpstreamTaskId != "" && !failure.UpstreamTerminal && (failure.Code == 605 || failure.Code == 606)
		if pollRetry {
			count, maximum, base, maxDelay, key = min(task.TransientRetryCount, cfg.TransientRetries+1), cfg.TransientRetries, cfg.TransientRetryBase, cfg.TransientRetryMax, "transient_retry_count"
		} else if resubmit && task.RetryCount < cfg.TotalRetries && failure.ReferenceTransportFallback {
			count, maximum, base, maxDelay, key = task.ReferenceRetryCount, 1, cfg.ReferenceRetryBase, cfg.ReferenceRetryMax, "reference_retry_count"
		} else if resubmit && task.RetryCount < cfg.TotalRetries {
			switch failure.Code {
			case 602:
				count, maximum, base, maxDelay, key = task.ReferenceRetryCount, cfg.ReferenceRetries, cfg.ReferenceRetryBase, cfg.ReferenceRetryMax, "reference_retry_count"
				if task.DispatchedAt > 0 && (failure.InternalCode == "upstream_failed" || failure.UpstreamTerminal) {
					maximum++
				}
				if failure.InternalCode == "reference_accounts_exhausted" {
					maximum = 0
				}
			case 603:
				count, maximum, base, maxDelay, key = task.CapacityRetryCount, cfg.CapacityRetries, cfg.CapacityRetryBase, cfg.CapacityRetryMax, "capacity_retry_count"
				if failure.InternalCode == "account_switches_exhausted" || failure.InternalCode == "image_compatible_accounts_exhausted" || failure.InternalCode == "image_reference_accounts_exhausted" {
					maximum = 0
				}
			case 605, 606:
				count, maximum, base, maxDelay, key = submissionTransientRetries, cfg.TransientRetries, cfg.TransientRetryBase, cfg.TransientRetryMax, "transient_retry_count"
				if task.UpstreamTaskId == "" {
					count = max(count, task.TransientRetryCount)
				}
				if failure.InternalCode == "timeout_accounts_exhausted" {
					maximum = 0
				}
			}
		}
		if key != "" && (pollRetry || count < maximum) {
			delay := ImageRetryDelay(base, maxDelay, count, cfg.RetryJitter, failure.RetryAfter, cfg.RetryAfterMax)
			status, kind = model.ImageTaskQueued, "execute"
			if pollRetry {
				status = model.ImageTaskInvoking
			}
			updates["request_cipher"] = task.RequestCipher
			updates["finished_at"] = 0
			updates[key] = count + 1
			if pollRetry {
				updates[key] = min(count+1, cfg.TransientRetries+1)
			}
			if !pollRetry {
				updates["retry_count"] = task.RetryCount + 1
			}
			updates["next_attempt_at"] = time.Now().Unix() + int64(delay)
			if task.StartedAt > 0 {
				updates["next_attempt_at"] = min(updates["next_attempt_at"].(int64), task.StartedAt+int64(cfg.ExecutionTimeout))
			}
			if !pollRetry {
				updates["upstream_task_id"] = ""
				updates["channel_id"] = 0
				updates["dispatched_at"] = 0
				updates["selected_channel_cipher"] = nil
			}
		}
	}
	updates["status"] = status
	decision := ImageFailureRetryDecision(task, failure, cfg, updates)
	encodedDecision, err := common.Marshal(decision)
	if err != nil {
		return err
	}
	updates["retry_decision"] = string(encodedDecision)
	if encodedAttempts, ok := updates["attempts"].(string); ok {
		var history []ImageChannelAttempt
		if err := common.UnmarshalJsonStr(encodedAttempts, &history); err != nil {
			return err
		}
		if len(history) > 0 {
			history[len(history)-1].Decision = &decision
			encoded, err := common.Marshal(history)
			if err != nil {
				return err
			}
			updates["attempts"] = string(encoded)
		}
	}
	if (status == model.ImageTaskFailed || status == model.ImageTaskExpired || status == model.ImageTaskExecutionUnknown) && cfg.FailedRequestRetentionHours > 0 && len(task.RequestCipher) > 0 {
		updates["request_cipher"] = task.RequestCipher
		updates["request_retain_until"] = time.Now().Unix() + int64(cfg.FailedRequestRetentionHours)*3600
	}
	return model.TransitionImageTask(ctx, task, updates, "invocation_failed", failure.Message, kind)
}

func UploadAsyncImageTask(ctx context.Context, task *model.AsyncImageTask, cfg ImageRuntimeConfig) error {
	if task.Status != model.ImageTaskUploading {
		if err := model.TransitionImageTask(ctx, *task, map[string]any{"status": model.ImageTaskUploading, "progress": 70}, "uploading", "", ""); err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Where("task_id = ?", task.TaskId).Take(task).Error; err != nil {
			return err
		}
	}
	var staging []model.AsyncImageStagingObject
	if err := model.DB.WithContext(ctx).Where("task_id = ?", task.TaskId).Order("image_index").Find(&staging).Error; err != nil {
		return err
	}
	if len(staging) != task.ImageCount || len(staging) == 0 {
		return model.ErrImageConflict
	}
	var intents []model.ImageUploadIntent
	if err := model.DB.WithContext(ctx).Where("task_id = ?", task.TaskId).Order("image_index").Find(&intents).Error; err != nil {
		return err
	}
	if len(intents) == 0 {
		store, err := GetImageStorage(ctx, "temporary")
		if err != nil {
			return err
		}
		for i, image := range staging {
			intents = append(intents, model.ImageUploadIntent{IntentKey: ImageIdentityHash("async-result", task.TaskId, strconv.Itoa(i)), TaskId: task.TaskId, UserId: task.UserId, TokenId: task.TokenId, ImageIndex: i, Class: "temporary", ProfileId: store.Profile.ProfileId, ObjectKey: ImageResultObjectKey(*task, i, image.ContentType), ContentType: image.ContentType, ByteSize: int64(len(image.Data)), Checksum: image.Checksum, Status: "pending", CreatedAt: time.Now().Unix()})
		}
		if err := model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			for _, intent := range intents {
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&intent).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Where("task_id = ?", task.TaskId).Order("image_index").Find(&intents).Error; err != nil {
			return err
		}
	}
	if len(intents) != len(staging) {
		return model.ErrImageConflict
	}
	results := make([]model.AsyncImageResult, len(staging))
	for i, saved := range staging {
		image, err := ValidateImageBytes(saved.Data, saved.ContentType, cfg.DownloadMaxBytes, cfg.DownloadMaxPixels)
		if err != nil {
			return err
		}
		if image.Checksum != saved.Checksum {
			return model.ErrImageConflict
		}
		object, err := StoreImageIntent(ctx, intents[i], image, task.CreatedAt+int64(cfg.ResultRetentionDays)*86400)
		if err != nil {
			return err
		}
		results[i] = model.AsyncImageResult{TaskId: task.TaskId, ImageIndex: i, ObjectId: object.ObjectId, CreatedAt: time.Now().Unix()}
	}
	return model.RegisterAsyncImageResults(ctx, *task, results)
}

func scheduleImagePostprocessRetry(ctx context.Context, task model.AsyncImageTask, cfg ImageRuntimeConfig, cause error, storage bool) error {
	if errors.Is(cause, model.ErrImageConflict) {
		return cause
	}
	status, key, count, limit := model.ImageTaskBillingFailed, "billing_retry_count", task.BillingRetryCount, cfg.BillingRetries
	if storage {
		status, key, count, limit = model.ImageTaskStorageFailed, "storage_retry_count", task.StorageRetryCount, cfg.StorageRetries
	}
	quotaUncertain := !storage && errors.Is(cause, model.ErrImageQuotaCacheUncertain)
	next := int64(0)
	kind := ""
	if count < limit && !quotaUncertain {
		next = time.Now().Unix() + int64(cfg.RetryBackoff)*(1<<min(count, 10))
		kind = "postprocess"
	}
	budget, stage := "billing", "billing"
	if storage {
		budget, stage = "storage", "upload"
	}
	decision := ImageRetryDecision{Version: 1, Stage: stage, Action: "manual_recovery", Reason: budget + "_retry_budget_exhausted", Rule: status, Budget: budget, Used: min(count+1, limit), Limit: limit, TotalUsed: task.RetryCount, TotalLimit: cfg.TotalRetries, NextAttemptAt: next, RecordedAt: time.Now().Unix()}
	if next > 0 {
		decision.Action, decision.Reason = "resume_output", "temporary_failure"
	}
	errorCode := status
	if quotaUncertain {
		errorCode, decision.Reason = "billing_quota_uncertain", "quota_confirmation_unknown"
	}
	encoded, err := common.Marshal(decision)
	if err != nil {
		return err
	}
	message := fmt.Sprintf("%s: confirmation is unavailable; existing images are retained", status)
	if diagnostic := SanitizeAsyncImageDiagnostic(cause.Error(), asyncImageProviderMessageLimit); diagnostic != "" {
		message = status + ": " + diagnostic
	}
	return model.TransitionImageTask(ctx, task, map[string]any{"status": status, key: count + 1, "next_attempt_at": next, "retry_decision": string(encoded), "lease_token": "", "lease_expires_at": 0, "public_error_code": 609, "error_code": errorCode, "error_message": message}, "postprocessing_failed", "", kind)
}
