package service

import (
	"context"
	"path/filepath"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	hostdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// ImageExecutionCapability describes configured routes, not a guarantee for a
// particular size, edit input, balance, live capacity or upstream availability.
type ImageExecutionCapability struct {
	Sync         bool   `json:"sync"`
	Async        bool   `json:"async"`
	SyncEndpoint string `json:"sync_endpoint,omitempty"`
}

type imageExecutionSnapshot struct {
	abilities []model.Ability
	channels  map[int]model.Channel
	policies  map[string]model.ImageGroupPolicy
	catalogs  map[string][]ImageModelCapability
	pools     map[string][]model.ImageChannelPool
	mappings  map[string]string
}

func loadImageExecutionSnapshot(ctx context.Context, tokenID int, groups, models []string) (imageExecutionSnapshot, error) {
	snapshot := imageExecutionSnapshot{channels: map[int]model.Channel{}, policies: map[string]model.ImageGroupPolicy{}, catalogs: map[string][]ImageModelCapability{}, pools: map[string][]model.ImageChannelPool{}, mappings: map[string]string{}}
	var mappings []model.TokenImagePlatformMapping
	if tokenID > 0 {
		if err := model.DB.WithContext(ctx).Where("token_id = ?", tokenID).Find(&mappings).Error; err != nil {
			return snapshot, err
		}
	}
	groups = slices.Clone(groups)
	for _, mapping := range mappings {
		snapshot.mappings[mapping.Platform] = mapping.Group
		if !slices.Contains(groups, mapping.Group) {
			groups = append(groups, mapping.Group)
		}
	}
	if len(groups) == 0 || len(models) == 0 {
		return snapshot, nil
	}
	routingModels := slices.Clone(models)
	for _, name := range models {
		routingModels = append(routingModels, ratio_setting.RoutingMatchModelName(name))
	}
	if err := model.DB.WithContext(ctx).Where(map[string]any{"group": groups, "enabled": true}).Where("model IN ?", routingModels).Find(&snapshot.abilities).Error; err != nil {
		return snapshot, err
	}
	ids := make([]int, 0, len(snapshot.abilities))
	for _, ability := range snapshot.abilities {
		ids = append(ids, ability.ChannelId)
	}
	var channels []model.Channel
	if len(ids) > 0 {
		if err := model.DB.WithContext(ctx).Where("id IN ?", ids).Find(&channels).Error; err != nil {
			return snapshot, err
		}
	}
	for _, channel := range channels {
		snapshot.channels[channel.Id] = channel
	}
	var policies []model.ImageGroupPolicy
	if err := model.DB.WithContext(ctx).Where(map[string]any{"group": groups}).Find(&policies).Error; err != nil {
		return snapshot, err
	}
	for _, policy := range policies {
		var catalog []ImageModelCapability
		if err := common.UnmarshalJsonStr(policy.Models, &catalog); err != nil {
			return snapshot, err
		}
		for i := range catalog {
			if len(catalog[i].Resolutions) == 0 {
				catalog[i].Resolutions = []string{"1K", "2K", "4K"}
			}
		}
		snapshot.policies[policy.PolicyKey], snapshot.catalogs[policy.PolicyKey] = policy, catalog
	}
	var pools []model.ImageChannelPool
	if err := model.DB.WithContext(ctx).Where(map[string]any{"group": groups}).Where("channel_id >= ?", 0).Find(&pools).Error; err != nil {
		return snapshot, err
	}
	for _, pool := range pools {
		snapshot.pools[pool.BindingKey] = append(snapshot.pools[pool.BindingKey], pool)
	}
	return snapshot, nil
}

func (snapshot imageExecutionSnapshot) policy(group, provider, family string) (model.ImageGroupPolicy, []ImageModelCapability, bool) {
	providerGroup := group
	if mapped := snapshot.mappings[provider]; mapped != "" {
		providerGroup = mapped
	}
	key := ImageIdentityHash(providerGroup, provider)
	if policy, found := snapshot.policies[key]; found {
		return policy, snapshot.catalogs[key], true
	}
	if mapped := snapshot.mappings[family]; mapped != "" {
		group = mapped
	}
	if snapshot.mappings[provider] != "" {
		group = providerGroup
	}
	key = ImageIdentityHash(group, family)
	policy, found := snapshot.policies[key]
	return policy, snapshot.catalogs[key], found
}

func (snapshot imageExecutionSnapshot) channelsFor(group, name string) []model.Channel {
	result := make([]model.Channel, 0)
	exact := slices.ContainsFunc(snapshot.abilities, func(ability model.Ability) bool { return ability.Group == group && ability.Model == name })
	for _, ability := range snapshot.abilities {
		if ability.Group != group || ability.Model != name && (exact || ability.Model != ratio_setting.RoutingMatchModelName(name)) {
			continue
		}
		channel, exists := snapshot.channels[ability.ChannelId]
		if exists && channel.Status == common.ChannelStatusEnabled && len(ImageChannelAccounts(channel, ImageAccountRouting{})) > 0 {
			result = append(result, channel)
		}
	}
	return result
}

// SynchronousImageChannelSupported matches both explicit and built-in async
// profiles, without rotating a key or sending a probe upstream.
func SynchronousImageChannelSupported(channel model.Channel, name, path string) bool {
	if channel.Type == constant.ChannelTypeTabTab || channel.Type == constant.ChannelTypeNewAPI && channel.GetSetting().TianyueMediaCompatibility {
		return false
	}
	allowed, _ := model.ChannelSatisfiesFilters(&channel, name, []hostdto.ChannelFilter{{Kind: hostdto.FilterRequestPath, RequestPath: path}})
	return allowed
}

func (snapshot imageExecutionSnapshot) candidates(policy model.ImageGroupPolicy, name, resolution string, synchronous bool) []model.Channel {
	bindings := snapshot.pools[ImagePoolBindingKey(policy, name, resolution)]
	if policy.Platform == "mixed" && len(bindings) == 0 {
		return nil
	}
	result := make([]model.Channel, 0)
	for _, channel := range snapshot.channelsFor(policy.Group, name) {
		if len(bindings) > 0 && !slices.ContainsFunc(bindings, func(binding model.ImageChannelPool) bool { return binding.ChannelId == channel.Id }) {
			continue
		}
		mapped, available, err := imageChannelForResolution(channel, name, resolution)
		if err != nil || !available || !ImageChannelSupportsPlatform(mapped, name, policy.Platform) {
			continue
		}
		capability := ImageChannelCapability(mapped, name)
		if !capability.Generate || synchronous && !SynchronousImageChannelSupported(mapped, name, "/v1/images/generations") {
			continue
		}
		if !synchronous && (policy.Platform == "mixed" || mapped.Type == constant.ChannelTypeTabTab) {
			size, err := MapOpenAIImageDimensions(resolution, "")
			if err != nil || !MixedImageChannelRequestSupported(mapped, AsyncImageRequest{Platform: "openai", Dialect: "async", SourcePath: "/v1/images/generations_async", Model: name, Kind: "text_to_image", Count: 1, Size: size, Resolution: resolution, ExplicitTier: true}) {
				continue
			}
		}
		result = append(result, mapped)
	}
	return result
}

func imagePolicyAllows(catalog []ImageModelCapability, name, resolution string) bool {
	return slices.ContainsFunc(catalog, func(capability ImageModelCapability) bool {
		return capability.MatchesMedia(name, "image") && (resolution == "AUTO" || slices.Contains(capability.Resolutions, resolution) || resolution == "0.5K" && capability.AllowHalfK)
	})
}

// ImageExecutionCapabilities batches configuration reads for the authorized
// model list. It never reserves quota, picks an account or submits a task.
func ImageExecutionCapabilities(ctx context.Context, tokenID int, userGroup string, groups, names []string) (map[string]ImageExecutionCapability, error) {
	result := make(map[string]ImageExecutionCapability)
	if len(names) == 0 {
		return result, nil
	}
	snapshot, err := loadImageExecutionSnapshot(ctx, tokenID, groups, names)
	if err != nil || len(names) == 0 {
		return result, err
	}
	cfg, err := GetImageRuntimeConfig(ctx)
	asyncAvailable := err == nil && cfg.AsyncEnabled && common.RedisEnabled && common.RDB != nil && ImageEncryptionAvailable()
	if asyncAvailable {
		var storage model.ImageStorageProfile
		err = model.DB.WithContext(ctx).Where("class = ? AND active = ?", "temporary", true).Order("id DESC").Take(&storage).Error
		asyncAvailable = err == nil
		if asyncAvailable && storage.Backend == "local" {
			asyncAvailable = filepath.IsAbs(storage.Root)
		} else if asyncAvailable {
			_, err = OpenImageStorage(ctx, storage)
			asyncAvailable = err == nil
		}
	}
	for _, name := range names {
		declared := common.IsImageGenerationModel(name)
		resolutions := []string{"1K", "2K", "4K", "AUTO"}
		for _, catalog := range snapshot.catalogs {
			for _, capability := range catalog {
				if capability.MatchesMedia(name, "image") {
					declared = true
					resolutions = append(resolutions, capability.Resolutions...)
					if capability.AllowHalfK {
						resolutions = append(resolutions, "0.5K")
					}
				}
			}
		}
		for _, channel := range snapshot.channels {
			upstream, err := ResolveImageChannelUpstreamModel(channel, name)
			capability := ImageChannelCapability(channel, name)
			declared = declared || err == nil && capability.Generate && capability.DeclaresModel(upstream)
		}
		if !declared {
			continue
		}
		slices.Sort(resolutions)
		resolutions = slices.Compact(resolutions)
		providers := make([]string, 0)
		for _, channel := range snapshot.channels {
			providers = append(providers, ImageChannelCapability(channel, name).Provider)
		}
		slices.Sort(providers)
		providers = slices.Compact(providers)
		capabilities := ImageExecutionCapability{}
		for _, group := range groups {
			policy, catalog, configured := snapshot.policy(group, "openai", "openai")
			if !configured {
				policy = model.ImageGroupPolicy{Group: group, Platform: "openai"}
			}
			for _, resolution := range resolutions {
				if (!configured || policy.Enabled && IsUserSelectableGroup(userGroup, policy.Group) && imagePolicyAllows(catalog, name, resolution)) && len(snapshot.candidates(policy, name, resolution, true)) > 0 {
					capabilities.Sync, capabilities.SyncEndpoint = true, "image-generation"
				}
				if !asyncAvailable {
					continue
				}
				mixed, mixedCatalog, hasMixed := snapshot.policy(group, "mixed", "mixed")
				if hasMixed && mixed.Enabled && mixed.AsyncEnabled && slices.ContainsFunc(mixedCatalog, func(capability ImageModelCapability) bool { return capability.MatchesMedia(name, "image") }) {
					if !IsUserSelectableGroup(userGroup, mixed.Group) {
						continue
					}
					if len(snapshot.pools[ImagePoolBindingKey(mixed, name, resolution)]) > 0 {
						capabilities.Async = capabilities.Async || imagePolicyAllows(mixedCatalog, name, resolution) && len(snapshot.candidates(mixed, name, resolution, false)) > 0
						continue
					}
				}
				for _, provider := range providers {
					for _, family := range []string{"openai", "gemini"} {
						candidate, catalog, found := snapshot.policy(group, provider, family)
						if !found || !candidate.Enabled || !candidate.AsyncEnabled || !IsUserSelectableGroup(userGroup, candidate.Group) || !imagePolicyAllows(catalog, name, resolution) {
							continue
						}
						capabilities.Async = capabilities.Async || slices.ContainsFunc(snapshot.candidates(candidate, name, resolution, false), func(channel model.Channel) bool { return ImageChannelCapability(channel, name).Provider == provider })
					}
				}
			}
			if !capabilities.Sync {
				for _, channel := range snapshot.channelsFor(group, name) {
					capability := ImageChannelCapability(channel, name)
					if capability.Generate && capability.Protocol == "gemini_native" && SynchronousImageChannelSupported(channel, name, "/v1beta/models/"+name+":generateContent") {
						capabilities.Sync, capabilities.SyncEndpoint = true, "gemini"
					}
				}
			}
		}
		result[name] = capabilities
	}
	return result, nil
}
