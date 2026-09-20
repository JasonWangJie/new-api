package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AsyncImageExecutor func(context.Context, model.AsyncImageTask, AsyncImageRequest, *model.Channel, ImageRuntimeConfig, func() error) ([]ImageBytes, model.AsyncImageBill, error)

var ExecuteAsyncImageFunc AsyncImageExecutor

// The host supplies the existing model-price evaluator without executing an
// upstream request or reserving funds at admission.
var EstimateAsyncImageQuotaFunc func(context.Context, model.Token, AsyncImageRequest, string) (int, error)

var imageLeaseRenew = redis.NewScript(`if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('EXPIRE',KEYS[1],ARGV[2]) else return 0 end`)
var imageLeaseRelease = redis.NewScript(`if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) else return 0 end`)
var imageQueuePop = redis.NewScript(`local item=redis.call('ZRANGE',KEYS[1],0,0,'WITHSCORES'); if #item == 0 or tonumber(item[2]) > tonumber(ARGV[1]) then return '' end; redis.call('ZREM',KEYS[1],item[1]); return item[1]`)
var imageInvocationAcquire = redis.NewScript(`redis.call('ZREMRANGEBYSCORE',KEYS[1],'-inf',ARGV[1]); if redis.call('ZCARD',KEYS[1]) >= tonumber(ARGV[3]) then return 0 end; redis.call('ZADD',KEYS[1],ARGV[2],ARGV[4]); redis.call('EXPIRE',KEYS[1],ARGV[5]); return 1`)

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
	if err := model.RecoverAsyncImageTasks(ctx, 100, cfg.ExecutionTimeout); err != nil {
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
	data, err := DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
	if err != nil {
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 604, InternalCode: "request_decryption_failed", Message: "Encrypted request is unavailable"}, cfg)
	}
	var request AsyncImageRequest
	if err := common.Unmarshal(data, &request); err != nil {
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 604, Message: "Persisted request is invalid"}, cfg)
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", task.TokenId, task.UserId).Take(&token).Error; err != nil {
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 604, Message: "Token is unavailable"}, cfg)
	}
	policy, _, err := ValidateAsyncImageEligibility(ctx, token, request, cfg)
	if err != nil || policy.Group != task.Group {
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 604, InternalCode: "eligibility_changed", Message: "Image eligibility or platform group changed before execution"}, cfg)
	}
	var attempts []ImageChannelAttempt
	if task.Attempts != "" {
		if err := common.UnmarshalJsonStr(task.Attempts, &attempts); err != nil {
			return err
		}
	}
	routing := ImageAccountAttemptRouting(attempts, cfg.ReferenceRetries, request.Platform, cfg.GeminiAccountSwitches)
	if routing.Stop {
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 602, InternalCode: "reference_accounts_exhausted", Message: "Upstream reference-image fetch retries are exhausted"}, cfg)
	}
	channel, account, err := PickImageChannel(ctx, policy, request, routing)
	if len(task.SelectedChannelCipher) > 0 {
		data, decodeErr := DecryptImagePayload(task.SelectedChannelCipher, "image-channel:"+task.TaskId)
		if decodeErr != nil {
			return decodeErr
		}
		channel = &model.Channel{}
		if decodeErr := common.Unmarshal(data, channel); decodeErr != nil {
			return decodeErr
		}
		account = ImageAccount{Fingerprint: ImageIdentityHash("image-account", strconv.Itoa(channel.Id), channel.Key)}
		err = nil
	}
	if err != nil {
		return failAsyncImageInvocation(ctx, *task, ClassifyAsyncImageFailure(err, 0), cfg)
	}
	if task.DispatchedAt == 0 {
		eligible, err := ImageCandidateChannels(ctx, policy, request.Model, request.Resolution)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(eligible, func(candidate model.Channel) bool {
			return candidate.Id == channel.Id && ImageCapabilitySupportsRequest(ImageChannelCapability(candidate, request.Model), request)
		}) {
			return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 604, Message: "Selected image channel is no longer eligible"}, cfg)
		}
	}
	now := time.Now().Unix()
	slotKey := ImageRedisKey("invoke-slots")
	slot, err := imageInvocationAcquire.Run(ctx, common.RDB, []string{slotKey}, now, now+int64(cfg.AttemptTimeout+cfg.WorkerLease), cfg.ImageConcurrency, task.LeaseToken, cfg.AttemptTimeout+cfg.WorkerLease).Int()
	if err != nil {
		return err
	}
	if slot != 1 {
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 603, InternalCode: "capacity_wait", Message: "Image execution capacity is busy"}, cfg)
	}
	defer common.RDB.ZRem(context.WithoutCancel(ctx), slotKey, task.LeaseToken)
	channelSlotKey := ImageChannelConcurrencyKey(channel.Id)
	channelSlot, err := imageInvocationAcquire.Run(ctx, common.RDB, []string{channelSlotKey}, now, now+int64(cfg.AttemptTimeout+cfg.WorkerLease), cfg.ImageConcurrency, task.LeaseToken, cfg.AttemptTimeout+cfg.WorkerLease).Int()
	if err != nil {
		return err
	}
	if channelSlot != 1 {
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 603, Message: "Channel image execution capacity is busy"}, cfg)
	}
	defer common.RDB.ZRem(context.WithoutCancel(ctx), channelSlotKey, task.LeaseToken)
	task.ChannelId = channel.Id
	capability := ImageChannelCapability(*channel, request.Model)
	if task.Provider == "" && (task.Dialect == "async" || capability.Provider == "ali" || capability.Provider == "replicate") {
		encodedChannel, err := common.Marshal(channel)
		if err != nil {
			return err
		}
		cipher, err := EncryptImagePayload(encodedChannel, "image-channel:"+task.TaskId)
		if err != nil {
			return err
		}
		if err := model.TransitionImageTask(ctx, *task, map[string]any{"provider": capability.Provider, "execution_protocol": capability.Protocol, "selected_channel_cipher": cipher}, "provider_selected", "", ""); err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error; err != nil {
			return err
		}
	}
	mode := cfg.OpenAIReferenceMode
	if request.Platform == "gemini" {
		mode = cfg.GeminiReferenceMode
	}
	if task.ReferenceRetryCount > 0 && mode == "passthrough_fallback_local" {
		mode = "local"
	}
	attempts = append(attempts, ImageChannelAttempt{ChannelId: channel.Id, KeyFingerprint: account.Fingerprint, KeyIndex: account.Index, StartedAt: now, ReferenceMode: mode})
	encoded, err := common.Marshal(attempts)
	if err != nil {
		return err
	}
	if err := model.TransitionImageTask(ctx, *task, map[string]any{"channel_id": channel.Id, "attempts": string(encoded)}, "channel_selected", "", ""); err != nil {
		return err
	}
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error; err != nil {
		return err
	}
	remaining := int64(cfg.ExecutionTimeout) - (time.Now().Unix() - task.StartedAt)
	if remaining <= 0 {
		return failAsyncImageInvocation(ctx, *task, &AsyncImageFailure{Code: 608, InternalCode: "execution_deadline", Message: "Image execution deadline expired", ExecutionUnknown: task.DispatchedAt > 0 && task.UpstreamTaskId == ""}, cfg)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, time.Duration(min(int64(cfg.AttemptTimeout), remaining))*time.Second)
	defer cancel()
	beforeInvoke := func() error {
		if attemptCtx.Err() != nil {
			return attemptCtx.Err()
		}
		if err := model.MarkAsyncImageDispatched(attemptCtx, *task, channel.Id); err != nil {
			return err
		}
		return model.DB.WithContext(attemptCtx).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error
	}
	images, bill, err := ExecuteAsyncImageFunc(attemptCtx, *task, request, channel, cfg, beforeInvoke)
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
		failure := ClassifyAsyncImageFailure(err, 0)
		if task.DispatchedAt > 0 && (ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			failure.ExecutionUnknown = true
		}
		if task.UpstreamTaskId != "" && (failure.ExecutionUnknown || failure.Code == 605 || failure.Code == 606) {
			return model.ScheduleAsyncImagePoll(context.WithoutCancel(ctx), *task, time.Now().Unix()+10, "recovered", "Upstream image task polling will continue")
		}
		if task.DispatchedAt > 0 && (failure.Code == 605 || failure.Code == 606 || failure.ExecutionUnknown) {
			_ = RecordImageCircuit(context.WithoutCancel(ctx), "async", channel.Id, false, cfg)
		}
		return failAsyncImageInvocation(context.WithoutCancel(ctx), *task, failure, cfg)
	}
	_ = RecordImageCircuit(context.WithoutCancel(ctx), "async", channel.Id, true, cfg)
	attempts[len(attempts)-1].Dispatched = task.DispatchedAt > 0
	attempts[len(attempts)-1].FinishedAt = time.Now().Unix()
	encoded, err = common.Marshal(attempts)
	if err != nil {
		return err
	}
	task.Attempts = string(encoded)
	staging := make([]model.AsyncImageStagingObject, len(images))
	for i, image := range images {
		staging[i] = model.AsyncImageStagingObject{Data: image.Data, ContentType: image.ContentType, Checksum: image.Checksum, Width: image.Width, Height: image.Height}
	}
	if err := model.StageAsyncImageOutput(ctx, *task, staging, bill); err != nil {
		if errors.Is(err, model.ErrImageConflict) {
			return err
		}
		return failAsyncImageInvocation(context.WithoutCancel(ctx), *task, &AsyncImageFailure{Code: 608, InternalCode: "execution_unknown", Message: "Upstream output could not be durably persisted", ExecutionUnknown: true}, cfg)
	}
	return nil
}

func failAsyncImageInvocation(ctx context.Context, task model.AsyncImageTask, failure *AsyncImageFailure, cfg ImageRuntimeConfig) error {
	status, kind := model.ImageTaskFailed, ""
	updates := map[string]any{"public_error_code": failure.Code, "error_code": failure.InternalCode, "error_message": failure.Message, "lease_token": "", "lease_expires_at": 0, "request_cipher": nil, "finished_at": time.Now().Unix(), "next_attempt_at": 0}
	if task.Attempts != "" {
		var attempts []ImageChannelAttempt
		if err := common.UnmarshalJsonStr(task.Attempts, &attempts); err != nil {
			return err
		}
		if len(attempts) > 0 {
			last := &attempts[len(attempts)-1]
			last.FinishedAt = time.Now().Unix()
			last.Dispatched = task.DispatchedAt > 0
			last.Code = failure.Code
			last.ReferenceFailure = last.Dispatched && failure.Code == 602 && failure.InternalCode == "upstream_failed"
			encoded, err := common.Marshal(attempts)
			if err != nil {
				return err
			}
			updates["attempts"] = string(encoded)
		}
	}
	if failure.ExecutionUnknown {
		status = model.ImageTaskExecutionUnknown
		updates["public_error_code"] = 608
	} else if failure.InternalCode == "execution_deadline" {
		status = model.ImageTaskExpired
	} else if task.RetryCount < cfg.TotalRetries {
		count, maximum, base, maxDelay, key := 0, 0, 0, 0, ""
		switch failure.Code {
		case 602:
			count, maximum, base, maxDelay, key = task.ReferenceRetryCount, cfg.ReferenceRetries, cfg.ReferenceRetryBase, cfg.ReferenceRetryMax, "reference_retry_count"
			if task.DispatchedAt > 0 && failure.InternalCode == "upstream_failed" {
				maximum++
			}
			if failure.InternalCode == "reference_accounts_exhausted" {
				maximum = 0
			}
		case 603:
			count, maximum, base, maxDelay, key = task.CapacityRetryCount, cfg.CapacityRetries, cfg.CapacityRetryBase, cfg.CapacityRetryMax, "capacity_retry_count"
		case 605, 606:
			count, maximum, base, maxDelay, key = task.TransientRetryCount, cfg.TransientRetries, cfg.TransientRetryBase, cfg.TransientRetryMax, "transient_retry_count"
		}
		if key != "" && count < maximum {
			delay := ImageRetryDelay(base, maxDelay, count, cfg.RetryJitter, failure.RetryAfter, cfg.RetryAfterMax)
			status, kind = model.ImageTaskQueued, "execute"
			if task.UpstreamTaskId != "" {
				status = model.ImageTaskInvoking
			}
			updates["request_cipher"] = task.RequestCipher
			updates["finished_at"] = 0
			updates[key] = count + 1
			updates["retry_count"] = task.RetryCount + 1
			updates["next_attempt_at"] = time.Now().Unix() + int64(delay)
			if task.UpstreamTaskId == "" {
				updates["channel_id"] = 0
				updates["dispatched_at"] = 0
			}
		}
	}
	updates["status"] = status
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
	next := int64(0)
	kind := ""
	if count < limit {
		next = time.Now().Unix() + int64(cfg.RetryBackoff)*(1<<min(count, 10))
		kind = "postprocess"
	}
	return model.TransitionImageTask(ctx, task, map[string]any{"status": status, key: count + 1, "next_attempt_at": next, "lease_token": "", "lease_expires_at": 0, "public_error_code": 609, "error_code": status, "error_message": fmt.Sprintf("%s: confirmation is unavailable; existing images are retained", status)}, "postprocessing_failed", "", kind)
}
