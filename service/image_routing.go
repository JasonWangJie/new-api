package service

import (
	"cmp"
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/go-redis/redis/v8"
)

func ImageChannelSupportsPlatform(channel model.Channel, modelName, platform string) bool {
	capability := ImageChannelCapability(channel, modelName)
	if platform == "gemini" {
		return capability.Generate && capability.Protocol == "gemini_native"
	}
	return capability.Generate && (platform == "openai" && capability.Protocol == "openai_images" || capability.Provider == platform)
}

func ImagePoolBindingKey(policy model.ImageGroupPolicy, modelName, resolution string) string {
	if policy.PoolMode == "resolution" {
		modelName = ""
	} else if policy.PoolMode == "model" {
		resolution = ""
	}
	return ImageIdentityHash(policy.Group, policy.Platform, policy.PoolMode, modelName, resolution)
}

func ImageCandidateChannels(ctx context.Context, policy model.ImageGroupPolicy, modelName, resolution string) ([]model.Channel, error) {
	var abilities []model.Ability
	if err := model.DB.WithContext(ctx).Where(map[string]any{"group": policy.Group, "enabled": true}).Find(&abilities).Error; err != nil {
		return nil, err
	}
	ids := make([]int, 0)
	for _, ability := range abilities {
		if ability.Group == policy.Group && ability.Model == modelName {
			ids = append(ids, ability.ChannelId)
		}
	}
	if len(ids) == 0 {
		normalized := ratio_setting.RoutingMatchModelName(modelName)
		for _, ability := range abilities {
			if ability.Group == policy.Group && ability.Model == normalized {
				ids = append(ids, ability.ChannelId)
			}
		}
	}
	var bindings []model.ImageChannelPool
	if err := model.DB.WithContext(ctx).Where("binding_key = ?", ImagePoolBindingKey(policy, modelName, resolution)).Find(&bindings).Error; err != nil {
		return nil, err
	}
	var channels []model.Channel
	if len(ids) == 0 {
		return channels, nil
	}
	if err := model.DB.WithContext(ctx).Where("id IN ? AND status = ?", ids, common.ChannelStatusEnabled).Find(&channels).Error; err != nil {
		return nil, err
	}
	eligible := make([]model.Channel, 0, len(channels))
	for _, channel := range channels {
		capability := ImageChannelCapability(channel, modelName)
		if !capability.Generate || !ImageChannelSupportsPlatform(channel, modelName, policy.Platform) {
			continue
		}
		if len(bindings) > 0 {
			index := slices.IndexFunc(bindings, func(binding model.ImageChannelPool) bool { return binding.ChannelId == channel.Id })
			if index < 0 {
				continue
			}
			priority := int64(bindings[index].Priority)
			channel.Priority = &priority
		}
		eligible = append(eligible, channel)
	}
	return eligible, nil
}

type ImageAccount struct {
	Fingerprint string
	Index       int
	Key         string `json:"-"`
}

type ImageAccountRouting struct {
	Pin              string
	Excluded         []string
	PinChannel       int
	ExcludedChannels []int
	Stop             bool
}

// Account identity survives key reordering and never exposes a usable key.
func ImageChannelAccounts(channel model.Channel, routing ImageAccountRouting) []ImageAccount {
	if routing.PinChannel != 0 && routing.PinChannel != channel.Id || slices.Contains(routing.ExcludedChannels, channel.Id) {
		return nil
	}
	keys := []string{channel.Key}
	if channel.ChannelInfo.IsMultiKey {
		keys = channel.GetKeys()
	}
	accounts := make([]ImageAccount, 0, len(keys))
	for index, key := range keys {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if status, exists := channel.ChannelInfo.MultiKeyStatusList[index]; channel.ChannelInfo.IsMultiKey && exists && status != common.ChannelStatusEnabled {
			continue
		}
		fingerprint := ImageIdentityHash("image-account", strconv.Itoa(channel.Id), key)
		if routing.Pin != "" && routing.Pin != fingerprint || slices.Contains(routing.Excluded, fingerprint) {
			continue
		}
		accounts = append(accounts, ImageAccount{Fingerprint: fingerprint, Index: index, Key: key})
	}
	return accounts
}

func PickImageChannel(ctx context.Context, policy model.ImageGroupPolicy, request AsyncImageRequest, routing ImageAccountRouting) (*model.Channel, ImageAccount, error) {
	if request.Provider == "" {
		request.Provider = policy.AsyncProvider
	}
	channels, err := ImageCandidateChannels(ctx, policy, request.Model, request.Resolution)
	if err != nil {
		return nil, ImageAccount{}, err
	}
	channels = slices.DeleteFunc(channels, func(channel model.Channel) bool { return len(ImageChannelAccounts(channel, routing)) == 0 })
	channels = slices.DeleteFunc(channels, func(channel model.Channel) bool {
		capability := ImageChannelCapability(channel, request.Model)
		return request.Provider != "" && capability.Provider != request.Provider || !ImageCapabilitySupportsRequest(capability, request)
	})
	cfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return nil, ImageAccount{}, err
	}
	if cfg.CircuitBreaker {
		available := channels[:0]
		for _, channel := range channels {
			allowed, err := ImageCircuitAllows(ctx, "async", channel.Id, cfg)
			if err != nil {
				return nil, ImageAccount{}, err
			}
			if allowed {
				available = append(available, channel)
			}
		}
		channels = available
	}
	if len(channels) == 0 {
		return nil, ImageAccount{}, errors.New("no eligible image account in the selected pool")
	}
	slices.SortFunc(channels, func(a, b model.Channel) int { return cmp.Compare(b.GetPriority(), a.GetPriority()) })
	priority := channels[0].GetPriority()
	channels = slices.DeleteFunc(channels, func(channel model.Channel) bool { return channel.GetPriority() != priority })
	var sum float64
	for _, channel := range channels {
		sum += float64(max(0, min(channel.GetWeight(), 1_000_000_000)))
	}
	adjustment := 0.0
	if sum == 0 {
		sum = float64(len(channels))
		adjustment = 1
	}
	weight := rand.Float64() * sum
	for i := range channels {
		weight -= float64(max(0, min(channels[i].GetWeight(), 1_000_000_000))) + adjustment
		if weight < 0 {
			accounts := ImageChannelAccounts(channels[i], routing)
			account := accounts[rand.IntN(len(accounts))]
			// Only this invocation uses the selected key. The stored channel and
			// its key status remain unchanged; relay middleware cannot repick it.
			channels[i].Key = account.Key
			channels[i].Keys = []string{account.Key}
			channels[i].ChannelInfo.IsMultiKey = false
			return &channels[i], account, nil
		}
	}
	return nil, ImageAccount{}, errors.New("image channel selection failed")
}

var imageCircuitRecord = redis.NewScript(`if ARGV[1] == 'success' then redis.call('DEL',KEYS[1],KEYS[2]); return 0 end; local count=redis.call('INCR',KEYS[1]); redis.call('EXPIRE',KEYS[1],ARGV[3]); if count >= tonumber(ARGV[2]) then redis.call('SET',KEYS[2],'open','EX',ARGV[3]); end; return count`)

func ImageCircuitAllows(ctx context.Context, scope string, channelId int, cfg ImageRuntimeConfig) (bool, error) {
	if !cfg.CircuitBreaker {
		return true, nil
	}
	if common.RDB == nil {
		return false, errors.New("Redis is required for image circuit state")
	}
	open, err := common.RDB.Exists(ctx, ImageRedisKey("breaker", scope, strconv.Itoa(channelId), "open")).Result()
	return open == 0, err
}

func RecordImageCircuit(ctx context.Context, scope string, channelId int, success bool, cfg ImageRuntimeConfig) error {
	if !cfg.CircuitBreaker {
		return nil
	}
	if common.RDB == nil {
		return errors.New("Redis is required for image circuit state")
	}
	state := "failure"
	if success {
		state = "success"
	}
	prefix := ImageRedisKey("breaker", scope, strconv.Itoa(channelId))
	return imageCircuitRecord.Run(ctx, common.RDB, []string{prefix + ":count", prefix + ":open"}, state, cfg.FailureThreshold, cfg.Cooldown).Err()
}

type ImageChannelAttempt struct {
	ChannelId         int    `json:"channel_id"`
	KeyFingerprint    string `json:"key_fingerprint,omitempty"`
	KeyIndex          int    `json:"key_index"`
	StartedAt         int64  `json:"started_at"`
	FinishedAt        int64  `json:"finished_at,omitempty"`
	Dispatched        bool   `json:"dispatched"`
	ReferenceMode     string `json:"reference_mode"`
	Code              int    `json:"error_code,omitempty"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	ErrorMessage      string `json:"error_message,omitempty"`
	ProviderCode      string `json:"provider_code,omitempty"`
	ProviderStatus    string `json:"provider_status,omitempty"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	ReferenceFailure  bool   `json:"upstream_reference_failure,omitempty"`
}

func ImageAccountAttemptRouting(attempts []ImageChannelAttempt, retries int, platform string, maxSwitches int) ImageAccountRouting {
	routing := ImageAccountRouting{}
	var reference []ImageChannelAttempt
	var last string
	switches := 0
	for _, attempt := range attempts {
		if !attempt.Dispatched {
			continue
		}
		if attempt.KeyFingerprint == "" {
			routing.PinChannel, routing.ExcludedChannels, routing.Stop = ImageAttemptRouting(attempts, retries)
			return routing
		}
		if last != "" && last != attempt.KeyFingerprint {
			switches++
		}
		last = attempt.KeyFingerprint
		if attempt.ReferenceFailure {
			reference = append(reference, attempt)
		}
	}
	if len(reference) > 0 {
		first := reference[0].KeyFingerprint
		for _, attempt := range reference {
			if attempt.KeyFingerprint != first {
				routing.Stop = true
				return routing
			}
		}
		if len(reference) <= retries {
			routing.Pin = first
		} else {
			routing.Excluded = []string{first}
		}
	}
	if platform == "gemini" && last != "" && switches >= maxSwitches {
		if slices.Contains(routing.Excluded, last) || routing.Pin != "" && routing.Pin != last {
			routing.Stop = true
			return routing
		}
		routing.Pin = last
	}
	return routing
}

// ImageAttemptRouting derives the durable A/A/A/B rule from completed upstream
// fetch failures. Server download failures do not enter that account history.
func ImageAttemptRouting(attempts []ImageChannelAttempt, retries int) (pin int, excluded []int, stop bool) {
	var reference []ImageChannelAttempt
	for _, attempt := range attempts {
		if attempt.Dispatched && attempt.ReferenceFailure {
			reference = append(reference, attempt)
		}
	}
	if len(reference) == 0 {
		return 0, nil, false
	}
	first := reference[0].ChannelId
	for _, attempt := range reference {
		if attempt.ChannelId != first {
			return 0, []int{first}, true
		}
	}
	if len(reference) <= retries {
		return first, nil, false
	}
	return 0, []int{first}, false
}

func ImageRetryDelay(base, ceiling, count, jitter int, retryAfter time.Duration, retryAfterMax int) int {
	delay := min(ceiling, base*(1<<min(count, 10)))
	spread := delay * min(jitter, 100) / 100
	if spread > 0 {
		delay = max(1, delay+rand.IntN(2*spread+1)-spread)
	}
	return max(delay, min(retryAfterMax, max(0, int(retryAfter.Seconds()))))
}

func ValidateAsyncImageEligibility(ctx context.Context, token model.Token, request AsyncImageRequest, cfg ImageRuntimeConfig) (model.ImageGroupPolicy, model.ImageFundingSelection, error) {
	if !cfg.AsyncEnabled || !common.RedisEnabled || common.RDB == nil {
		return model.ImageGroupPolicy{}, model.ImageFundingSelection{}, errors.New("async image service is disabled or Redis is unavailable")
	}
	if token.Status != common.TokenStatusEnabled || token.ExpiredTime != -1 && token.ExpiredTime <= common.GetTimestamp() {
		return model.ImageGroupPolicy{}, model.ImageFundingSelection{}, errors.New("Token is unavailable")
	}
	if token.ModelLimitsEnabled && !token.GetModelLimitsMap()[request.Model] {
		return model.ImageGroupPolicy{}, model.ImageFundingSelection{}, errors.New("image model is outside Token permissions")
	}
	policy, catalog, err := ResolveAsyncImagePolicy(ctx, token, request)
	if err != nil {
		return policy, model.ImageFundingSelection{}, err
	}
	if !policy.Enabled || !policy.AsyncEnabled {
		return policy, model.ImageFundingSelection{}, errors.New("async images are disabled for the selected platform group")
	}
	var user model.User
	if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", token.UserId, common.UserStatusEnabled).Take(&user).Error; err != nil {
		return policy, model.ImageFundingSelection{}, err
	}
	if _, allowed := GetUserUsableGroups(user.Group)[policy.Group]; !allowed {
		return policy, model.ImageFundingSelection{}, errors.New("image platform group is outside user permissions")
	}
	if !ratio_setting.ContainsGroupRatio(policy.Group) {
		return policy, model.ImageFundingSelection{}, errors.New("image platform group is unavailable")
	}
	index := slices.IndexFunc(catalog, func(capability ImageModelCapability) bool { return capability.Id == request.Model })
	if index < 0 {
		return policy, model.ImageFundingSelection{}, errors.New("image model is not available in this platform catalog")
	}
	capability := catalog[index]
	invalid := func(message string) (model.ImageGroupPolicy, model.ImageFundingSelection, error) {
		return policy, model.ImageFundingSelection{}, &AsyncImageFailure{Code: 604, InternalCode: "invalid_request", Message: message, HTTPStatus: 400}
	}
	if request.Resolution == "0.5K" && !capability.AllowHalfK {
		return invalid("unsupported_image_dimensions: model does not support 0.5K")
	}
	if request.Resolution != "" && request.Resolution != "AUTO" && !slices.Contains(capability.Resolutions, request.Resolution) && !(request.Resolution == "0.5K" && capability.AllowHalfK) {
		return invalid("unsupported_image_dimensions: resolution is not supported by this model")
	}
	if capability.MaxOutputImages > 0 && request.Count > capability.MaxOutputImages {
		return invalid("requested image count exceeds model limit")
	}
	for _, parameter := range []struct {
		name   string
		values []string
	}{{"quality", capability.Qualities}, {"output_format", capability.Formats}, {"background", capability.Backgrounds}} {
		if raw, exists := request.Native[parameter.name]; exists {
			var value string
			if err := common.Unmarshal(raw, &value); err != nil || (len(parameter.values) > 0 && !slices.Contains(parameter.values, value)) {
				return invalid("unsupported model parameter: " + parameter.name)
			}
		}
	}
	references := 0
	for _, part := range request.Parts {
		if part.Type == "image_url" {
			references++
		}
	}
	if references > capability.MaxReferenceImages {
		return invalid("too_many_reference_images_for_model")
	}
	channels, err := ImageCandidateChannels(ctx, policy, request.Model, request.Resolution)
	if err != nil {
		return policy, model.ImageFundingSelection{}, err
	}
	channels = slices.DeleteFunc(channels, func(ch model.Channel) bool {
		capability := ImageChannelCapability(ch, request.Model)
		return request.Provider != "" && capability.Provider != request.Provider || !ImageCapabilitySupportsRequest(capability, request)
	})
	if len(channels) == 0 {
		return policy, model.ImageFundingSelection{}, errors.New("no eligible image channel in the selected pool")
	}
	if _, err := GetImageStorage(ctx, "temporary"); err != nil {
		return policy, model.ImageFundingSelection{}, errors.New("temporary image storage is unavailable")
	}
	if EstimateAsyncImageQuotaFunc == nil {
		return policy, model.ImageFundingSelection{}, errors.New("async image pricing is unavailable")
	}
	amount, err := EstimateAsyncImageQuotaFunc(ctx, token, request, policy.Group)
	if err != nil || amount < 0 || amount > common.MaxQuota {
		return policy, model.ImageFundingSelection{}, errors.New("async image pricing could not be evaluated")
	}
	if !token.UnlimitedQuota && token.RemainQuota < amount {
		return policy, model.ImageFundingSelection{}, model.ErrImageInsufficientQuota
	}
	funding, err := model.SelectImageFunding(ctx, user.Id, amount, user.GetSetting().BillingPreference)
	return policy, funding, err
}

func ImageRedisKey(parts ...string) string {
	prefix := strings.TrimSpace(common.GetEnvOrDefaultString("ASYNC_IMAGE_REDIS_PREFIX", "new-api:images"))
	return prefix + ":" + strings.Join(parts, ":")
}

func ImageChannelConcurrencyKey(channelId int) string {
	return ImageRedisKey("concurrency", strconv.Itoa(channelId))
}
