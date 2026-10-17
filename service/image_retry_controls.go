package service

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/go-redis/redis/v8"
)

// Redis TIME makes the supplier budget consistent across gateway instances.
// Initial submissions and accepted-job polling never consume these tokens.
var imageSupplierRetryAcquire = redis.NewScript(`
local stamp=redis.call('TIME')
local now=tonumber(stamp[1])+tonumber(stamp[2])/1000000
local capacity=tonumber(ARGV[1])
local window=tonumber(ARGV[2])
local state=redis.call('HMGET',KEYS[1],'tokens','updated')
local tokens=tonumber(state[1]) or capacity
local updated=tonumber(state[2]) or now
tokens=math.min(capacity,tokens+math.max(0,now-updated)*capacity/window)
local wait=0
if tokens>=1 then tokens=tokens-1 else wait=math.ceil((1-tokens)*window/capacity) end
redis.call('HSET',KEYS[1],'tokens',tokens,'updated',now)
redis.call('EXPIRE',KEYS[1],math.ceil(window*2))
return wait
`)

// ImageSupplierRetryBudgetKey groups channels by provider and origin. Different
// API keys on the same supplier share a budget; paths and signed query values
// never enter the persisted Redis key.
func ImageSupplierRetryBudgetKey(channel model.Channel, modelName string) string {
	base := channel.GetBaseURL()
	origin := "channel-type:" + strconv.Itoa(channel.Type)
	if parsed, err := url.Parse(base); err == nil && parsed.Hostname() != "" {
		port := parsed.Port()
		if port == "" {
			port = "443"
			if parsed.Scheme == "http" {
				port = "80"
			}
		}
		origin = strings.ToLower(parsed.Scheme + "://" + net.JoinHostPort(strings.TrimSuffix(parsed.Hostname(), "."), port))
	}
	provider := ImageChannelCapability(channel, modelName).Provider
	return ImageRedisKey("supplier-retry", ImageIdentityHash("image-supplier", provider, origin))
}

func AcquireImageSupplierRetry(ctx context.Context, channel model.Channel, modelName string, cfg ImageRuntimeConfig) (int, error) {
	if cfg.SupplierRetryCapacity == 0 {
		return 0, nil
	}
	if common.RDB == nil {
		return 0, errors.New("Redis is required for supplier retry budgets")
	}
	return imageSupplierRetryAcquire.Run(ctx, common.RDB, []string{ImageSupplierRetryBudgetKey(channel, modelName)}, cfg.SupplierRetryCapacity, cfg.SupplierRetryWindowSeconds).Int()
}

// ReturnImageSupplierRetry releases a permit when preparation never crossed
// the submission barrier. Expired buckets are not recreated.
func ReturnImageSupplierRetry(ctx context.Context, channel model.Channel, modelName string, cfg ImageRuntimeConfig) error {
	if cfg.SupplierRetryCapacity == 0 || common.RDB == nil {
		return nil
	}
	return imageSupplierRetryReturn.Run(ctx, common.RDB, []string{ImageSupplierRetryBudgetKey(channel, modelName)}, cfg.SupplierRetryCapacity).Err()
}

var imageSupplierRetryReturn = redis.NewScript(`
local tokens=redis.call('HGET',KEYS[1],'tokens')
if tokens then redis.call('HSET',KEYS[1],'tokens',math.min(tonumber(ARGV[1]),tonumber(tokens)+1)) end
return 1
`)

func ImageAccountCooldownKey(fingerprint string) string {
	return ImageRedisKey("account-cooldown", fingerprint)
}

func RecordImageAccountCooldown(ctx context.Context, fingerprint string, failure *AsyncImageFailure, cfg ImageRuntimeConfig) error {
	if fingerprint == "" || cfg.AccountCooldownSeconds == 0 || failure == nil {
		return nil
	}
	if failure.Code != 605 && failure.Code != 606 && failure.HTTPStatus != 429 && failure.InternalCode != "upstream_transport_timeout" {
		return nil
	}
	if common.RDB == nil {
		return errors.New("Redis is required for image account cooldowns")
	}
	retryAfter := min(max(failure.RetryAfter, 0), time.Duration(min(cfg.RetryAfterMax, 86400))*time.Second)
	seconds := max(cfg.AccountCooldownSeconds, int((retryAfter+time.Second-1)/time.Second))
	key := ImageAccountCooldownKey(fingerprint)
	// A shorter concurrent failure must not shorten a provider's Retry-After.
	return imageAccountCooldownRecord.Run(ctx, common.RDB, []string{key}, seconds).Err()
}

var imageAccountCooldownRecord = redis.NewScript(`
local remaining=redis.call('TTL',KEYS[1])
local seconds=tonumber(ARGV[1])
if remaining<seconds then redis.call('SET',KEYS[1],'cooling','EX',seconds) end
return 1
`)

type ImageAccountCooling struct{ DelaySeconds int }

func (wait *ImageAccountCooling) Error() string { return "Image accounts are temporarily cooling down" }

var (
	ErrImageLocalCapacityBusy        = errors.New("local image execution capacity is busy")
	ErrImageLocalControlsUnavailable = errors.New("local image routing controls are temporarily unavailable")
	ErrImageTimeoutAccountsExhausted = errors.New("all eligible image accounts have timed out for this task")
)

// DeferAsyncImageSubmission waits for shared routing controls without spending
// another generation attempt or discarding the original request and hold.
func DeferAsyncImageSubmission(ctx context.Context, task model.AsyncImageTask, cfg ImageRuntimeConfig, reason string, delay, used, limit int) error {
	if task.UpstreamTaskId != "" || task.UpstreamSucceededAt > 0 || task.DispatchedAt > 0 {
		return model.ErrImageConflict
	}
	now := time.Now().Unix()
	if task.StartedAt > 0 && now >= task.StartedAt+int64(cfg.ExecutionTimeout) {
		return failAsyncImageInvocation(ctx, task, &AsyncImageFailure{Code: 608, InternalCode: "execution_deadline", Message: "Image execution deadline expired"}, cfg)
	}
	decision := ImageRetryDecision{Version: 1, Stage: "routing", Action: "wait_account_cooldown", Reason: reason, Rule: reason, TotalUsed: task.RetryCount, TotalLimit: cfg.TotalRetries, NextAttemptAt: now + int64(max(1, delay)), Deadline: task.StartedAt + int64(cfg.ExecutionTimeout), RecordedAt: now}
	if task.StartedAt > 0 {
		decision.NextAttemptAt = min(decision.NextAttemptAt, decision.Deadline)
	}
	message, publicCode := "Waiting for image retry controls", 603
	switch reason {
	case "supplier_retry_budget_exhausted":
		decision.Action, decision.Budget, decision.Used, decision.Limit = "wait_supplier_budget", "supplier", used, limit
	case "supplier_retry_budget_unavailable":
		decision.Action, decision.Budget = "wait_local_dependency", "supplier"
		message = "Supplier retry control is temporarily unavailable"
	case "routing_controls_unavailable":
		decision.Action = "wait_local_dependency"
		message = "Image routing controls are temporarily unavailable"
	case "local_capacity_busy":
		decision.Action = "wait_capacity"
		message = "Waiting for local image execution capacity"
	case "quota_cache_unavailable":
		decision.Stage, decision.Action = "prepare", "wait_local_dependency"
		message, publicCode = "The quota service is temporarily unavailable; generation has not started", 606
	}
	encoded, err := common.Marshal(decision)
	if err != nil {
		return err
	}
	updates := map[string]any{"status": model.ImageTaskQueued, "channel_id": 0, "selected_channel_cipher": nil, "lease_token": "", "lease_expires_at": 0, "next_attempt_at": decision.NextAttemptAt, "retry_decision": string(encoded), "error_code": reason, "error_message": message, "public_error_code": publicCode}
	if task.Attempts != "" {
		var attempts []ImageChannelAttempt
		if err := common.UnmarshalJsonStr(task.Attempts, &attempts); err != nil {
			return err
		}
		if len(attempts) > 0 {
			last := &attempts[len(attempts)-1]
			if !last.Dispatched && last.FinishedAt == 0 && last.ChannelId == task.ChannelId {
				last.FinishedAt, last.Code, last.ErrorMessage, last.Decision = now, publicCode, message, &decision
				encodedAttempts, err := common.Marshal(attempts)
				if err != nil {
					return err
				}
				updates["attempts"] = string(encodedAttempts)
			}
		}
	}
	return model.TransitionImageTask(ctx, task, updates, "retry_control_wait", message, "execute")
}
