package service

import (
	"cmp"
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	hostreasoning "github.com/QuantumNous/new-api/setting/reasoning"
	"github.com/go-redis/redis/v8"
)

func ImageChannelSupportsPlatform(channel model.Channel, modelName, platform string) bool {
	capability := ImageChannelCapability(channel, modelName)
	if platform == "gemini" {
		return capability.Generate && capability.Protocol == "gemini_native"
	}
	if platform == "mixed" {
		_, registered := constant.ChannelTypeNames[channel.Type]
		return registered && channel.Type != constant.ChannelTypeUnknown && capability.Generate && capability.Protocol == "openai_images"
	}
	return capability.Generate && (platform == "openai" && capability.Protocol == "openai_images" || capability.Provider == platform)
}

type ImagePoolChannelOption struct {
	Id   int    `json:"id"`
	Name string `json:"name"`
}

type ImagePoolModelOption struct {
	Id         string   `json:"id"`
	Label      string   `json:"label"`
	MediaTypes []string `json:"media_types"`
	ChannelIds []int    `json:"channel_ids"`
}

type ImagePoolSelectionOptions struct {
	Models   []ImagePoolModelOption   `json:"models"`
	Channels []ImagePoolChannelOption `json:"channels"`
}

type MediaRoutingPreviewRequest struct {
	ExecutionMode string `json:"execution_mode"`
	Group         string `json:"group"`
	Platform      string `json:"platform"`
	MediaType     string `json:"media_type"`
	Model         string `json:"model"`
	Resolution    string `json:"resolution"`
	Operation     string `json:"operation"`
	Seconds       *int   `json:"seconds,omitempty"`
	TokenID       int    `json:"token_id,omitempty"`
}

type MediaRoutingPreviewChannel struct {
	Provider          string   `json:"provider"`
	Protocol          string   `json:"protocol"`
	ChannelID         int      `json:"channel_id"`
	Group             string   `json:"group"`
	BindingKey        string   `json:"binding_key"`
	BindingFound      bool     `json:"binding_found"`
	Name              string   `json:"name"`
	UpstreamModel     string   `json:"upstream_model"`
	Priority          int64    `json:"priority"`
	Weight            int      `json:"weight"`
	AvailableAccounts int      `json:"available_accounts"`
	Reasons           []string `json:"reasons"`
}

type MediaRoutingPreview struct {
	MediaRoutingPreviewRequest
	BindingKey    string                       `json:"binding_key"`
	BindingFound  bool                         `json:"binding_found"`
	PoolMode      string                       `json:"pool_mode"`
	PolicyEnabled bool                         `json:"policy_enabled"`
	AsyncEnabled  bool                         `json:"async_enabled"`
	Eligible      []MediaRoutingPreviewChannel `json:"eligible"`
	Excluded      []MediaRoutingPreviewChannel `json:"excluded"`
	Warnings      []string                     `json:"warnings"`
	Reasons       []string                     `json:"reasons"`
	RoutingGroups []string                     `json:"routing_groups"`
}

// PreviewMediaRouting reads configuration only. It deliberately avoids the
// random picker, quota reservation, circuit probes and concurrency acquisition.
func PreviewMediaRouting(ctx context.Context, input MediaRoutingPreviewRequest) (MediaRoutingPreview, error) {
	if input.ExecutionMode == "" {
		input.ExecutionMode = "async"
	}
	synchronous := input.ExecutionMode == "sync"
	preview := MediaRoutingPreview{
		MediaRoutingPreviewRequest: input,
		Eligible:                   make([]MediaRoutingPreviewChannel, 0), Excluded: make([]MediaRoutingPreviewChannel, 0),
		Warnings: []string{"configuration_preview_only"},
	}
	var policy model.ImageGroupPolicy
	var catalog []ImageModelCapability
	policyReasons := make([]string, 0)
	var routingGroups []string
	imageRequest := AsyncImageRequest{
		Provider: input.Platform, Platform: "openai", Dialect: "async", Model: input.Model,
		Kind: input.Operation, Resolution: input.Resolution, Count: 1,
		// The unified generation endpoint also accepts provider reference fields.
		SourcePath: "/v1/images/generations_async",
	}
	if input.Platform == "gemini" {
		imageRequest.Platform = "gemini"
	}
	if input.Platform == "mixed" {
		imageRequest.Provider = ""
	}
	if input.Operation == "image_to_image" {
		imageRequest.Parts = []AsyncImageInputPart{{Type: "image_url", URL: "https://preview.invalid/reference.png"}}
	}
	if synchronous {
		imageRequest.Dialect, imageRequest.SourcePath = "", "/v1/images/generations"
		if input.Operation == "image_to_image" {
			imageRequest.SourcePath = "/v1/images/edits"
		}
	}
	if input.TokenID > 0 {
		var token model.Token
		if err := model.DB.WithContext(ctx).First(&token, input.TokenID).Error; err != nil {
			return preview, err
		}
		var owner model.User
		if err := model.DB.WithContext(ctx).First(&owner, token.UserId).Error; err != nil {
			return preview, err
		}
		var err error
		if synchronous {
			policy, catalog, err = ResolveImagePolicy(ctx, token, "openai")
			if input.Platform == "gemini" {
				group := token.Group
				if group == "" {
					group = owner.Group
				}
				policy, catalog = model.ImageGroupPolicy{Group: group, Platform: "gemini"}, nil
			}
		} else if input.Platform == "mixed" {
			policy, catalog, err = ResolveImagePolicy(ctx, token, "mixed")
		} else if input.MediaType == "image" {
			policy, catalog, err = ResolveAsyncImagePolicy(ctx, token, imageRequest)
		} else {
			policy, catalog, err = ResolveImagePolicy(ctx, token, input.Platform)
		}
		if err != nil {
			return preview, err
		}
		preview.Group = policy.Group
		if token.Status != common.TokenStatusEnabled && token.Status != common.TokenStatusExpired && token.Status != common.TokenStatusExhausted {
			policyReasons = append(policyReasons, "token_disabled")
		}
		if token.Status == common.TokenStatusExpired || token.ExpiredTime != -1 && token.ExpiredTime <= time.Now().Unix() {
			policyReasons = append(policyReasons, "token_expired")
		}
		if token.Status == common.TokenStatusExhausted || !token.UnlimitedQuota && token.RemainQuota <= 0 {
			policyReasons = append(policyReasons, "token_exhausted")
		}
		if token.ModelLimitsEnabled && !token.GetModelLimitsMap()[input.Model] {
			policyReasons = append(policyReasons, "token_model_not_allowed")
		}
		if owner.Status != common.UserStatusEnabled {
			policyReasons = append(policyReasons, "user_disabled")
		}
		if (input.MediaType == "video" || synchronous) && policy.Group == "auto" {
			// Match SetupContextForToken/GetRequestAutoGroups: an empty stored
			// list inherits global groups; malformed or revoked lists fail closed.
			groups, parseErr := token.GetAutoGroups()
			if parseErr != nil {
				routingGroups = []string{}
			} else if len(groups) > 0 {
				routingGroups = FilterUserTokenAutoGroups(owner.Group, groups)
			} else {
				routingGroups = GetUserAutoGroup(owner.Group)
			}
			if len(routingGroups) == 0 {
				policyReasons = append(policyReasons, "token_group_not_allowed")
			}
		} else if !GroupInUserUsableGroups(owner.Group, policy.Group) || input.MediaType == "image" && !IsUserSelectableGroup(owner.Group, policy.Group) {
			policyReasons = append(policyReasons, "token_group_not_allowed")
		}
	} else if synchronous {
		var err error
		policy, catalog, err = ResolveImagePolicy(ctx, model.Token{Group: input.Group}, "openai")
		if err != nil {
			return preview, err
		}
		if input.Platform == "gemini" {
			policy, catalog = model.ImageGroupPolicy{Group: input.Group, Platform: "gemini"}, nil
		}
	} else if input.Platform == "mixed" {
		var err error
		policy, catalog, err = ResolveImagePolicy(ctx, model.Token{Group: input.Group}, "mixed")
		if err != nil {
			return preview, err
		}
	} else if input.MediaType == "image" {
		// An explicit group has no owner permission context. Resolve the same
		// provider/family policies and channel priorities within that group.
		token := model.Token{Group: input.Group}
		var err error
		policy, catalog, err = resolveInheritedImagePolicy(ctx, token, imageRequest)
		if err != nil {
			return preview, err
		}
		var priority int64
		found := false
		for _, family := range []string{"openai", "gemini"} {
			request := imageRequest
			request.Platform = family
			candidatePolicy, candidateCatalog, err := resolveInheritedImagePolicy(ctx, token, request)
			if err != nil {
				return preview, err
			}
			if !candidatePolicy.Enabled || !candidatePolicy.AsyncEnabled || !slices.ContainsFunc(candidateCatalog, func(capability ImageModelCapability) bool { return capability.MatchesMedia(input.Model, "image") }) {
				continue
			}
			channels, err := ImageCandidateChannels(ctx, candidatePolicy, input.Model, input.Resolution)
			if err != nil {
				return preview, err
			}
			for _, channel := range channels {
				capability := ImageChannelCapability(channel, input.Model)
				if capability.Provider != input.Platform || !ImageCapabilitySupportsRequest(capability, imageRequest) {
					continue
				}
				if !found || channel.GetPriority() > priority {
					policy, catalog, priority, found = candidatePolicy, candidateCatalog, channel.GetPriority(), true
				}
			}
		}
	} else {
		var policies []model.ImageGroupPolicy
		if err := model.DB.WithContext(ctx).Where("policy_key = ?", ImageIdentityHash(input.Group, input.Platform)).Limit(1).Find(&policies).Error; err != nil {
			return preview, err
		}
		policy = model.ImageGroupPolicy{Group: input.Group, Platform: input.Platform, PoolMode: "resolution"}
		if len(policies) > 0 {
			policy = policies[0]
			if err := common.UnmarshalJsonStr(policy.Models, &catalog); err != nil {
				return preview, err
			}
		}
	}
	preview.PoolMode, preview.PolicyEnabled, preview.AsyncEnabled = policy.PoolMode, policy.Enabled, policy.AsyncEnabled
	if synchronous && policy.Group == "auto" && input.TokenID > 0 {
		preview.RoutingGroups, preview.Reasons = routingGroups, policyReasons
		for _, group := range routingGroups {
			request := input
			request.TokenID, request.Group = 0, group
			part, err := PreviewMediaRouting(ctx, request)
			if err != nil {
				return preview, err
			}
			for _, candidate := range append(part.Eligible, part.Excluded...) {
				candidate.Reasons = append(candidate.Reasons, policyReasons...)
				if len(candidate.Reasons) == 0 {
					preview.Eligible = append(preview.Eligible, candidate)
				} else {
					preview.Excluded = append(preview.Excluded, candidate)
				}
			}
		}
		return preview, nil
	}
	if policy.Id == 0 && input.MediaType == "image" && !synchronous {
		policyReasons = append(policyReasons, "policy_missing")
	}
	if policy.Id > 0 {
		if !policy.Enabled {
			policyReasons = append(policyReasons, "policy_disabled")
		}
		if !policy.AsyncEnabled && !synchronous {
			policyReasons = append(policyReasons, "async_disabled")
		}
	}
	capabilityIndex := slices.IndexFunc(catalog, func(capability ImageModelCapability) bool {
		return capability.MatchesMedia(input.Model, input.MediaType)
	})
	if policy.Id > 0 && capabilityIndex < 0 {
		policyReasons = append(policyReasons, "model_not_in_policy")
	}
	if input.MediaType == "image" && capabilityIndex >= 0 {
		capability := catalog[capabilityIndex]
		if input.Resolution != "AUTO" && len(capability.Resolutions) > 0 && !slices.Contains(capability.Resolutions, input.Resolution) && !(input.Resolution == "0.5K" && capability.AllowHalfK) {
			policyReasons = append(policyReasons, "resolution_not_in_policy")
		}
		if input.Operation == "image_to_image" && capability.MaxReferenceImages == 0 && !synchronous {
			policyReasons = append(policyReasons, "operation_not_supported")
		}
	}
	if routingGroups == nil {
		if policy.Group == "auto" && input.TokenID == 0 {
			routingGroups = []string{}
			policyReasons = append(policyReasons, "token_required_for_auto_group")
		} else {
			routingGroups = []string{policy.Group}
		}
	}
	preview.Reasons, preview.RoutingGroups = policyReasons, routingGroups
	for _, group := range routingGroups {
		poolPolicy := policy
		poolPolicy.Group = group
		resolution := input.Resolution
		if input.MediaType == "video" {
			resolution = VideoPoolResolution(resolution)
		}
		bindingKey := ImagePoolBindingKey(poolPolicy, input.Model, resolution)
		if len(routingGroups) == 1 {
			preview.BindingKey = bindingKey
		}
		var bindings []model.ImageChannelPool
		if err := model.DB.WithContext(ctx).Where("binding_key = ? AND channel_id >= ?", bindingKey, 0).Find(&bindings).Error; err != nil {
			return preview, err
		}
		if synchronous && (policy.Id == 0 || input.Platform == "gemini") {
			bindings = nil
		}
		if input.Platform == "mixed" && len(bindings) == 0 {
			policyReasons = append(policyReasons, "mixed_pool_missing")
			preview.Reasons = policyReasons
		}
		preview.BindingFound = preview.BindingFound || len(bindings) > 0
		var abilities []model.Ability
		if err := model.DB.WithContext(ctx).Where(map[string]any{"group": group}).Find(&abilities).Error; err != nil {
			return preview, err
		}
		routingModel := ratio_setting.RoutingMatchModelName(input.Model)
		exactAbility := slices.ContainsFunc(abilities, func(ability model.Ability) bool { return ability.Enabled && ability.Model == input.Model })
		ids := make([]int, 0, len(abilities)+len(bindings))
		for _, ability := range abilities {
			ids = append(ids, ability.ChannelId)
		}
		for _, binding := range bindings {
			if binding.ChannelId > 0 {
				ids = append(ids, binding.ChannelId)
			}
		}
		if len(ids) == 0 {
			continue
		}
		var channels []model.Channel
		if err := model.DB.WithContext(ctx).Where("id IN ?", ids).Order("id").Find(&channels).Error; err != nil {
			return preview, err
		}
		imageCandidates := make(map[int]model.Channel)
		if input.MediaType == "image" {
			candidates, err := ImageCandidateChannels(ctx, poolPolicy, input.Model, resolution)
			if err != nil {
				return preview, err
			}
			for _, channel := range candidates {
				imageCandidates[channel.Id] = channel
			}
		}
		for _, channel := range channels {
			candidate := MediaRoutingPreviewChannel{ChannelID: channel.Id, Group: group, BindingKey: bindingKey, BindingFound: len(bindings) > 0, Name: channel.Name, UpstreamModel: input.Model, Reasons: slices.Clone(policyReasons)}
			if channel.Status != common.ChannelStatusEnabled {
				candidate.Reasons = append(candidate.Reasons, "channel_disabled")
			}
			enabled := slices.ContainsFunc(abilities, func(ability model.Ability) bool {
				return ability.ChannelId == channel.Id && ability.Enabled && (ability.Model == input.Model || ability.Model == routingModel && (input.MediaType == "video" || !exactAbility))
			})
			if !enabled {
				candidate.Reasons = append(candidate.Reasons, "model_not_enabled")
			}
			if input.MediaType == "image" {
				if mapped, found := imageCandidates[channel.Id]; found {
					channel = mapped
				}
				if _, found := imageCandidates[channel.Id]; !found && policy.AsyncEnabled || synchronous && policy.Id > 0 && !policy.AsyncEnabled {
					mapped, available, err := imageChannelForResolution(channel, input.Model, resolution)
					if err != nil {
						return preview, err
					}
					if !available {
						candidate.Reasons = append(candidate.Reasons, "resolution_mapping_missing")
					}
					channel = mapped
				}
				capability := ImageChannelCapability(channel, input.Model)
				candidate.Provider, candidate.Protocol = capability.Provider, capability.Protocol
				if input.Platform != "mixed" && !(synchronous && input.Platform == "openai") && capability.Provider != input.Platform || !ImageChannelSupportsPlatform(channel, input.Model, policy.Platform) {
					candidate.Reasons = append(candidate.Reasons, "platform_mismatch")
				}
				if !ImageCapabilitySupportsRequest(capability, imageRequest) {
					candidate.Reasons = append(candidate.Reasons, "operation_not_supported")
				}
				if input.Platform == "mixed" && !MixedImageChannelRequestSupported(channel, imageRequest) {
					candidate.Reasons = append(candidate.Reasons, "image_parameters_not_supported")
				}
				if synchronous {
					path := imageRequest.SourcePath
					if input.Platform == "gemini" {
						path = "/v1beta/models/" + input.Model + ":generateContent"
					}
					if !SynchronousImageChannelSupported(channel, input.Model, path) {
						candidate.Reasons = append(candidate.Reasons, "synchronous_protocol_not_supported")
					}
				}
			} else {
				if !mediaChannelSupportsVideoPlatform(&channel, input.Model, input.Platform) {
					candidate.Reasons = append(candidate.Reasons, "platform_mismatch")
				}
				for _, binding := range mediaVideoCandidates(input.Model) {
					if binding.Plugin == nil || binding.Plugin.Meta.Key != input.Platform {
						continue
					}
					var overrides VideoModelOverrides
					if capabilityIndex >= 0 {
						overrides = catalog[capabilityIndex].VideoOverrides
					}
					profile, err := EffectiveVideoProfile(binding, input.Model, overrides)
					if err != nil {
						return preview, err
					}
					modeIndex := slices.IndexFunc(profile.Modes, func(mode jsplugin.VideoModeProfile) bool { return mode.Name == input.Operation })
					if modeIndex < 0 {
						candidate.Reasons = append(candidate.Reasons, "operation_not_supported")
						break
					}
					mode := profile.Modes[modeIndex]
					// Validate scalar routing dimensions with the same profile validator;
					// reference material is intentionally outside this configuration view.
					mode.Inputs = nil
					profile.Modes = []jsplugin.VideoModeProfile{mode}
					parameters := map[string]any{"prompt": "routing preview"}
					if input.Resolution != "" {
						parameters["resolution"] = input.Resolution
					}
					if input.Seconds != nil {
						parameters["seconds"] = *input.Seconds
					}
					if err := ValidateVideoProfileRequest(profile, input.Operation, parameters, nil); err != nil {
						candidate.Reasons = append(candidate.Reasons, "video_parameters_not_supported")
					}
					break
				}
			}
			if len(bindings) > 0 {
				index := slices.IndexFunc(bindings, func(binding model.ImageChannelPool) bool { return binding.ChannelId == channel.Id })
				if index < 0 {
					candidate.Reasons = append(candidate.Reasons, "not_in_pool")
				} else {
					ApplyMediaChannelPoolWeight(&channel, bindings[index].Priority)
				}
			} else if input.MediaType == "video" {
				weight := uint(max(1, min(channel.GetWeight(), 1_000_000_000)))
				channel.Weight = &weight
			}
			candidate.Priority, candidate.Weight = channel.GetPriority(), channel.GetWeight()
			candidate.AvailableAccounts = len(ImageChannelAccounts(channel, ImageAccountRouting{}))
			if candidate.AvailableAccounts == 0 {
				candidate.Reasons = append(candidate.Reasons, "no_enabled_account")
			}
			var mappingErr error
			candidate.UpstreamModel, mappingErr = ResolveImageChannelUpstreamModel(channel, input.Model)
			if mappingErr != nil {
				candidate.Reasons = append(candidate.Reasons, "invalid_model_mapping")
			}
			if len(candidate.Reasons) > 0 {
				preview.Excluded = append(preview.Excluded, candidate)
			} else {
				preview.Eligible = append(preview.Eligible, candidate)
			}
		}
	}
	if len(preview.Eligible) > 0 {
		priority := preview.Eligible[0].Priority
		for _, channel := range preview.Eligible {
			priority = max(priority, channel.Priority)
		}
		positiveWeight := slices.ContainsFunc(preview.Eligible, func(channel MediaRoutingPreviewChannel) bool {
			return channel.Priority == priority && channel.Weight > 0
		})
		eligible := make([]MediaRoutingPreviewChannel, 0, len(preview.Eligible))
		for _, channel := range preview.Eligible {
			if channel.Priority < priority {
				channel.Reasons = append(channel.Reasons, "lower_priority")
			} else if positiveWeight && channel.Weight == 0 {
				channel.Reasons = append(channel.Reasons, "zero_weight")
			}
			if len(channel.Reasons) > 0 {
				preview.Excluded = append(preview.Excluded, channel)
			} else {
				eligible = append(eligible, channel)
			}
		}
		preview.Eligible = eligible
	}
	return preview, nil
}

// GetImagePoolSelectionOptions returns only executable model/channel choices
// for a configured group and media platform. A saved policy catalog limits the
// selectable models; without one, enabled channel abilities form the catalog.
func GetImagePoolSelectionOptions(ctx context.Context, group, platform string) (ImagePoolSelectionOptions, error) {
	options := ImagePoolSelectionOptions{
		Models:   make([]ImagePoolModelOption, 0),
		Channels: make([]ImagePoolChannelOption, 0),
	}
	var abilities []model.Ability
	if err := model.DB.WithContext(ctx).Where(map[string]any{"group": group, "enabled": true}).Find(&abilities).Error; err != nil {
		return options, err
	}

	labels := make(map[string]string)
	mediaTypes := make(map[string][]string)
	configuredModels := make([]string, 0)
	configuredModelSet := make(map[string]bool)
	var policies []model.ImageGroupPolicy
	if err := model.DB.WithContext(ctx).Where("policy_key = ?", ImageIdentityHash(group, platform)).Limit(1).Find(&policies).Error; err != nil {
		return options, err
	}
	if len(policies) > 0 {
		var catalog []ImageModelCapability
		if err := common.UnmarshalJsonStr(policies[0].Models, &catalog); err != nil {
			return options, err
		}
		for _, capability := range catalog {
			if !configuredModelSet[capability.Id] {
				configuredModels = append(configuredModels, capability.Id)
				configuredModelSet[capability.Id] = true
			}
			labels[capability.Id] = capability.Label
			kinds := []string{capability.MediaType}
			if capability.MediaType == "" {
				kinds = []string{"image"}
				if MediaPlatformSupportsVideoModel(capability.Id, platform) {
					kinds = append(kinds, "video")
				}
			}
			for _, kind := range kinds {
				if kind != "" && !slices.Contains(mediaTypes[capability.Id], kind) {
					mediaTypes[capability.Id] = append(mediaTypes[capability.Id], kind)
				}
			}
		}
	}

	abilityChannels := make(map[string][]int)
	channelIdSet := make(map[int]bool)
	for _, ability := range abilities {
		abilityChannels[ability.Model] = append(abilityChannels[ability.Model], ability.ChannelId)
		channelIdSet[ability.ChannelId] = true
	}
	if len(policies) == 0 {
		for modelName := range abilityChannels {
			configuredModels = append(configuredModels, modelName)
			labels[modelName] = modelName
			if MediaPlatformSupportsVideoModel(modelName, platform) {
				mediaTypes[modelName] = append(mediaTypes[modelName], "video")
			}
		}
	}

	channelIds := make([]int, 0, len(channelIdSet))
	for channelId := range channelIdSet {
		channelIds = append(channelIds, channelId)
	}
	var channels []model.Channel
	if len(channelIds) > 0 {
		if err := model.DB.WithContext(ctx).Where("id IN ? AND status = ?", channelIds, common.ChannelStatusEnabled).Find(&channels).Error; err != nil {
			return options, err
		}
	}
	channelsById := make(map[int]*model.Channel, len(channels))
	for i := range channels {
		channelsById[channels[i].Id] = &channels[i]
	}

	slices.Sort(configuredModels)
	selectedChannels := make(map[int]bool)
	for _, modelName := range configuredModels {
		ids := slices.Clone(abilityChannels[modelName])
		routingModel := ratio_setting.RoutingMatchModelName(modelName)
		videoModel := MediaPlatformSupportsVideoModel(modelName, platform)
		if videoModel {
			ids = append(ids, abilityChannels[routingModel]...)
		} else if len(ids) == 0 {
			ids = abilityChannels[routingModel]
		}
		modelChannels := make([]int, 0, len(ids))
		hasImageChannel := false
		seen := make(map[int]bool)
		for _, channelId := range ids {
			channel := channelsById[channelId]
			if seen[channelId] || channel == nil {
				continue
			}
			imageSupported := ImageChannelSupportsPlatform(*channel, modelName, platform)
			if (len(policies) == 0 || policies[0].AsyncEnabled) && len(channel.GetSetting().ImageResolutionModelMapping[modelName]) > 0 {
				imageSupported = false
				for _, resolution := range []string{"1K", "2K", "4K"} {
					candidate, available, err := imageChannelForResolution(*channel, modelName, resolution)
					if err != nil {
						return options, err
					}
					if available && ImageChannelSupportsPlatform(candidate, modelName, platform) {
						imageSupported = true
						break
					}
				}
			}
			if !imageSupported && (platform == "mixed" || !mediaChannelSupportsVideoPlatform(channel, modelName, platform)) {
				continue
			}
			seen[channelId] = true
			selectedChannels[channelId] = true
			modelChannels = append(modelChannels, channelId)
			hasImageChannel = hasImageChannel || imageSupported
		}
		if len(modelChannels) == 0 {
			continue
		}
		if len(policies) == 0 && hasImageChannel && !slices.Contains(mediaTypes[modelName], "image") {
			mediaTypes[modelName] = append(mediaTypes[modelName], "image")
		}
		slices.Sort(modelChannels)
		label := labels[modelName]
		if label == "" {
			label = modelName
		}
		kinds := slices.Clone(mediaTypes[modelName])
		slices.Sort(kinds)
		options.Models = append(options.Models, ImagePoolModelOption{Id: modelName, Label: label, MediaTypes: kinds, ChannelIds: modelChannels})
	}
	for channelId := range selectedChannels {
		channel := channelsById[channelId]
		options.Channels = append(options.Channels, ImagePoolChannelOption{Id: channel.Id, Name: channel.Name})
	}
	slices.SortFunc(options.Channels, func(a, b ImagePoolChannelOption) int { return cmp.Compare(a.Id, b.Id) })
	return options, nil
}

func ImagePoolBindingKey(policy model.ImageGroupPolicy, modelName, resolution string) string {
	if policy.PoolMode == "resolution" {
		modelName = ""
	} else if policy.PoolMode == "model" {
		resolution = ""
	}
	return ImageIdentityHash(policy.Group, policy.Platform, policy.PoolMode, modelName, resolution)
}

func ResolveImageChannelUpstreamModel(channel model.Channel, modelName string) (string, error) {
	if channel.GetModelMapping() == "" {
		return modelName, nil
	}
	var mapping map[string]string
	if err := common.UnmarshalJsonStr(channel.GetModelMapping(), &mapping); err != nil {
		return modelName, err
	}
	seen := make(map[string]bool)
	for {
		next, found := mapping[modelName]
		if base := hostreasoning.BaseModelName(modelName); (!found || next == "") && base != modelName {
			next, found = mapping[base]
		}
		if !found || next == "" || next == modelName {
			return modelName, nil
		}
		if seen[modelName] {
			return modelName, errors.New("cyclic image model mapping")
		}
		seen[modelName] = true
		modelName = next
	}
}

// ValidateMixedImagePoolMembers prevents duplicate supplier accounts from
// receiving multiple weights through separate channel/provider declarations.
func ValidateMixedImagePoolMembers(ctx context.Context, ids []int) error {
	if len(ids) < 2 {
		return nil
	}
	var channels []model.Channel
	if err := model.DB.WithContext(ctx).Where("id IN ?", ids).Find(&channels).Error; err != nil {
		return err
	}
	seen := make(map[string]int)
	for _, channel := range channels {
		base := channel.GetBaseURL()
		if base == "" {
			base = constant.GetChannelBaseURL(channel.Type)
		}
		parsed, err := url.Parse(base)
		if err != nil || parsed.Hostname() == "" {
			return errors.New("mixed pool channel requires a valid upstream origin")
		}
		port := parsed.Port()
		if port == "" {
			port = "443"
			if strings.EqualFold(parsed.Scheme, "http") {
				port = "80"
			}
		}
		origin := strings.ToLower(parsed.Scheme + "://" + net.JoinHostPort(strings.TrimSuffix(parsed.Hostname(), "."), port))
		keys := []string{channel.Key}
		if channel.ChannelInfo.IsMultiKey {
			keys = channel.GetKeys()
		}
		for _, key := range keys {
			if strings.TrimSpace(key) == "" {
				continue
			}
			identity := ImageIdentityHash("mixed-supplier-account", origin, key)
			if previous, exists := seen[identity]; exists && previous != channel.Id {
				return errors.New("mixed pool contains duplicate upstream origin and credential members")
			}
			seen[identity] = channel.Id
		}
	}
	return nil
}

func ImageCandidateChannels(ctx context.Context, policy model.ImageGroupPolicy, modelName, resolution string) ([]model.Channel, error) {
	if policy.AsyncEnabled {
		resolution = strings.ToUpper(strings.TrimSpace(resolution))
	}
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
	if err := model.DB.WithContext(ctx).Where("binding_key = ? AND channel_id >= ?", ImagePoolBindingKey(policy, modelName, resolution), 0).Find(&bindings).Error; err != nil {
		return nil, err
	}
	var channels []model.Channel
	if policy.Platform == "mixed" && len(bindings) == 0 {
		return channels, nil
	}
	if len(ids) == 0 {
		return channels, nil
	}
	if err := model.DB.WithContext(ctx).Where("id IN ? AND status = ?", ids, common.ChannelStatusEnabled).Find(&channels).Error; err != nil {
		return nil, err
	}
	eligible := make([]model.Channel, 0, len(channels))
	for _, channel := range channels {
		if policy.AsyncEnabled {
			var available bool
			var err error
			channel, available, err = imageChannelForResolution(channel, modelName, resolution)
			if err != nil {
				return nil, err
			}
			if !available {
				continue
			}
		}
		capability := ImageChannelCapability(channel, modelName)
		if !capability.Generate || !ImageChannelSupportsPlatform(channel, modelName, policy.Platform) {
			continue
		}
		if len(bindings) > 0 {
			index := slices.IndexFunc(bindings, func(binding model.ImageChannelPool) bool { return binding.ChannelId == channel.Id })
			if index < 0 {
				continue
			}
			ApplyMediaChannelPoolWeight(&channel, bindings[index].Priority)
		}
		eligible = append(eligible, channel)
	}
	return eligible, nil
}

// imageChannelForResolution creates a request-local mapping that is also kept
// in the frozen channel snapshot after the upstream accepts an image task.
func imageChannelForResolution(channel model.Channel, modelName, resolution string) (model.Channel, bool, error) {
	rules := channel.GetSetting().ImageResolutionModelMapping[modelName]
	if len(rules) == 0 {
		return channel, true, nil
	}
	target := rules[resolution]
	if target == "" {
		return channel, false, nil
	}
	// Targets are final upstream names, not another link in the ordinary
	// mapping chain. Keep this override isolated from the stored channel.
	mapping := map[string]string{modelName: target, target: target}
	encoded, err := common.Marshal(mapping)
	if err != nil {
		return channel, false, err
	}
	channel.ModelMapping = common.GetPointer(string(encoded))
	return channel, true, nil
}

// ApplyMediaChannelPoolWeight uses the legacy pool priority field as a relative
// weight. Pool candidates share one priority and ignore global channel weights.
// Historical negative priorities behave as zero weights without a migration.
func ApplyMediaChannelPoolWeight(channel *model.Channel, poolWeight int) {
	priority := int64(0)
	weight := uint(max(0, poolWeight))
	channel.Priority = &priority
	channel.Weight = &weight
}

// PickMediaChannelByWeight selects the interval containing position in [0, 1).
// Zero weights are excluded unless all weights are zero, which shares traffic
// equally. Callers pass a random position in production.
func PickMediaChannelByWeight(weights []int, position float64) int {
	var sum float64
	for _, weight := range weights {
		sum += float64(max(0, min(weight, 1_000_000_000)))
	}
	adjustment := 0.0
	if sum == 0 {
		sum = float64(len(weights))
		adjustment = 1
	}
	pick := position * sum
	for i, weight := range weights {
		pick -= float64(max(0, min(weight, 1_000_000_000))) + adjustment
		if pick < 0 {
			return i
		}
	}
	return -1
}

type ImageAccount struct {
	Fingerprint           string
	CredentialFingerprint string `json:"-"`
	Index                 int
	Key                   string `json:"-"`
}

var ErrImageCompatibleAccountsExhausted = errors.New("no other eligible image account supports the request")

type ImageAccountRouting struct {
	Pin                     string
	Excluded                []string
	Avoid                   string
	PinChannel              int
	ExcludedChannels        []int
	AvoidChannel            int
	Cooling                 map[string]bool
	WaitForCooldown         bool
	TimedOutAccounts        []string
	TimedOutCredentials     []string
	IncompatibleAccounts    []string
	IncompatibleCredentials []string
	IncompatibleChannels    []int
	Stop                    bool
}

// Account identity survives key reordering and never exposes a usable key.
func ImageChannelAccounts(channel model.Channel, routing ImageAccountRouting) []ImageAccount {
	if routing.PinChannel != 0 && routing.PinChannel != channel.Id || slices.Contains(routing.ExcludedChannels, channel.Id) || slices.Contains(routing.IncompatibleChannels, channel.Id) {
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
		credentialFingerprint := ImageIdentityHash("image-credential", key)
		if slices.Contains(routing.TimedOutAccounts, fingerprint) || slices.Contains(routing.TimedOutCredentials, credentialFingerprint) {
			continue
		}
		if slices.Contains(routing.IncompatibleAccounts, fingerprint) || slices.Contains(routing.IncompatibleCredentials, credentialFingerprint) {
			continue
		}
		if routing.Cooling[fingerprint] {
			continue
		}
		if routing.Pin != "" && routing.Pin != fingerprint || slices.Contains(routing.Excluded, fingerprint) {
			continue
		}
		accounts = append(accounts, ImageAccount{Fingerprint: fingerprint, CredentialFingerprint: credentialFingerprint, Index: index, Key: key})
	}
	return accounts
}

func PickImageChannel(ctx context.Context, policy model.ImageGroupPolicy, request AsyncImageRequest, routing ImageAccountRouting) (*model.Channel, ImageAccount, error) {
	if policy.Platform == "mixed" && (request.MixedRouting == nil || request.Provider != "") {
		return nil, ImageAccount{}, errors.New("mixed image selection requires providerless admitted routing state")
	}
	if request.MixedRouting != nil && (policy.Platform != "mixed" || policy.Group != request.MixedRouting.Group || ImagePoolBindingKey(policy, request.Model, AsyncImageRoutingResolution(request)) != request.MixedRouting.BindingKey) {
		return nil, ImageAccount{}, errors.New("mixed image routing identity changed before selection")
	}
	if request.Provider == "" && request.MixedRouting == nil {
		request.Provider = policy.AsyncProvider
	}
	channels, err := ImageCandidateChannels(ctx, policy, request.Model, AsyncImageRoutingResolution(request))
	if err != nil {
		return nil, ImageAccount{}, err
	}
	channels = slices.DeleteFunc(channels, func(channel model.Channel) bool {
		capability := ImageChannelCapability(channel, request.Model)
		if request.Provider != "" && capability.Provider != request.Provider {
			return true
		}
		if policy.Platform == "mixed" || channel.Type == constant.ChannelTypeTabTab || channel.Type == constant.ChannelTypeNewAPI && channel.GetSetting().TianyueMediaCompatibility {
			return !MixedImageChannelRequestSupported(channel, request)
		}
		return !ImageCapabilitySupportsRequest(capability, request)
	})
	// Rejected compatibility and timeout exclusions belong to this task and
	// survive cooldown fallback, including copied credentials in other channels.
	timeoutExcluded := false
	channels = slices.DeleteFunc(channels, func(channel model.Channel) bool {
		if len(ImageChannelAccounts(channel, routing)) > 0 {
			return false
		}
		withoutTimeouts := routing
		withoutTimeouts.TimedOutAccounts, withoutTimeouts.TimedOutCredentials = nil, nil
		timeoutExcluded = timeoutExcluded || len(ImageChannelAccounts(channel, withoutTimeouts)) > 0
		return true
	})
	if len(channels) == 0 && timeoutExcluded {
		return nil, ImageAccount{}, ErrImageTimeoutAccountsExhausted
	}
	if len(channels) == 0 && (len(routing.IncompatibleAccounts) > 0 || len(routing.IncompatibleCredentials) > 0 || len(routing.IncompatibleChannels) > 0) {
		return nil, ImageAccount{}, ErrImageCompatibleAccountsExhausted
	}
	cfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return nil, ImageAccount{}, err
	}
	if (cfg.CircuitBreaker || cfg.AccountCooldownSeconds > 0) && common.RDB == nil {
		return nil, ImageAccount{}, ErrImageLocalControlsUnavailable
	}
	if cfg.CircuitBreaker {
		available := channels[:0]
		for _, channel := range channels {
			allowed, err := ImageCircuitAllows(ctx, "async", channel.Id, cfg)
			if err != nil {
				return nil, ImageAccount{}, errors.Join(ErrImageLocalControlsUnavailable, err)
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
	// Prefer a channel with room before applying priority and weight. The worker
	// still acquires its slot atomically, so this snapshot never authorizes a
	// submission or bypasses the final concurrency check.
	if common.RDB != nil {
		counts := make([]*redis.IntCmd, 0, len(channels))
		_, err := common.RDB.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, channel := range channels {
				counts = append(counts, pipe.ZCount(ctx, ImageChannelConcurrencyKey(channel.Id), strconv.FormatInt(time.Now().Unix()+1, 10), "+inf"))
			}
			return nil
		})
		if err != nil {
			return nil, ImageAccount{}, errors.Join(ErrImageLocalControlsUnavailable, err)
		}
		available := make([]model.Channel, 0, len(channels))
		for i, channel := range channels {
			limit := cfg.ImageConcurrency
			settings := channel.GetSetting()
			if settings.ImageConcurrencyLimit != nil && *settings.ImageConcurrencyLimit > 0 {
				limit = min(limit, *settings.ImageConcurrencyLimit)
			}
			if counts[i].Val() < int64(limit) {
				available = append(available, channel)
			}
		}
		if len(available) == 0 {
			return nil, ImageAccount{}, ErrImageLocalCapacityBusy
		}
		channels = available
	}
	if cfg.AccountCooldownSeconds > 0 {
		fingerprints := make([]string, 0)
		cooldownKeys := make([]string, 0)
		for _, channel := range channels {
			for _, account := range ImageChannelAccounts(channel, routing) {
				fingerprints = append(fingerprints, account.Fingerprint)
				cooldownKeys = append(cooldownKeys, ImageAccountCooldownKey(account.Fingerprint))
			}
		}
		states, err := common.RDB.MGet(ctx, cooldownKeys...).Result()
		if err != nil {
			return nil, ImageAccount{}, errors.Join(ErrImageLocalControlsUnavailable, err)
		}
		routing.Cooling = make(map[string]bool, len(fingerprints))
		allCooling := true
		for i, state := range states {
			routing.Cooling[fingerprints[i]] = state != nil
			allCooling = allCooling && state != nil
		}
		if allCooling {
			var ttlCommands []*redis.DurationCmd
			_, err := common.RDB.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, key := range cooldownKeys {
					ttlCommands = append(ttlCommands, pipe.PTTL(ctx, key))
				}
				return nil
			})
			if err != nil {
				return nil, ImageAccount{}, errors.Join(ErrImageLocalControlsUnavailable, err)
			}
			// Only accounts passing eligibility, breaker and capacity checks
			// enter this fallback. Keep all other exclusions intact.
			earliest, selected := time.Duration(1<<63-1), 0
			for i, command := range ttlCommands {
				remaining := command.Val()
				if remaining < 0 {
					if remaining == -1 {
						remaining = time.Duration(1<<63 - 1)
					} else {
						remaining = 0
					}
				}
				if remaining < earliest {
					earliest, selected = remaining, i
				}
			}
			if routing.WaitForCooldown && earliest > 0 {
				return nil, ImageAccount{}, &ImageAccountCooling{DelaySeconds: int(min(earliest/time.Second+1, time.Duration(max(1, cfg.AccountCooldownSeconds))))}
			}
			routing.Cooling[fingerprints[selected]] = false
		}
		channels = slices.DeleteFunc(channels, func(channel model.Channel) bool { return len(ImageChannelAccounts(channel, routing)) == 0 })
	}
	// Prefer another healthy compatible account after a transient rejection.
	// A pool with no alternative retains its bounded same-account retry path.
	if routing.Avoid != "" || routing.AvoidChannel != 0 {
		if routing.Avoid != "" && routing.AvoidChannel != 0 && !slices.ContainsFunc(channels, func(channel model.Channel) bool {
			return channel.Id != routing.AvoidChannel && len(ImageChannelAccounts(channel, routing)) > 0
		}) {
			// A multi-key channel can still offer another healthy account when
			// no other channel in the binding is available.
			routing.AvoidChannel = 0
		}
		alternative := false
		for _, channel := range channels {
			if channel.Id == routing.AvoidChannel {
				continue
			}
			for _, account := range ImageChannelAccounts(channel, routing) {
				if account.Fingerprint != routing.Avoid {
					alternative = true
					break
				}
			}
			if alternative {
				break
			}
		}
		if alternative {
			if routing.Avoid != "" {
				routing.Excluded = append(routing.Excluded, routing.Avoid)
			}
			if routing.AvoidChannel != 0 {
				routing.ExcludedChannels = append(routing.ExcludedChannels, routing.AvoidChannel)
			}
			channels = slices.DeleteFunc(channels, func(channel model.Channel) bool { return len(ImageChannelAccounts(channel, routing)) == 0 })
		}
	}
	channels = preferImageChannelsForReferenceCount(channels, request.ReferenceImageCount())
	slices.SortFunc(channels, func(a, b model.Channel) int { return cmp.Compare(b.GetPriority(), a.GetPriority()) })
	priority := channels[0].GetPriority()
	channels = slices.DeleteFunc(channels, func(channel model.Channel) bool { return channel.GetPriority() != priority })
	weights := make([]int, len(channels))
	for i := range channels {
		weights[i] = channels[i].GetWeight()
	}
	index := PickMediaChannelByWeight(weights, rand.Float64())
	if index < 0 {
		return nil, ImageAccount{}, errors.New("image channel selection failed")
	}
	accounts := ImageChannelAccounts(channels[index], routing)
	account := accounts[rand.IntN(len(accounts))]
	// Only this invocation uses the selected key. The stored channel and
	// its key status remain unchanged; relay middleware cannot repick it.
	channels[index].Key = account.Key
	channels[index].Keys = []string{account.Key}
	channels[index].ChannelInfo.IsMultiKey = false
	return &channels[index], account, nil
}

func preferImageChannelsForReferenceCount(channels []model.Channel, referenceCount int) []model.Channel {
	if referenceCount <= model.DefaultImageChannelMaxReferenceImages {
		return channels
	}
	preferred := slices.DeleteFunc(slices.Clone(channels), func(channel model.Channel) bool {
		return !imageChannelSupportsReferenceCount(channel, referenceCount)
	})
	if len(preferred) == 0 {
		return channels
	}
	return preferred
}

func imageChannelSupportsReferenceCount(channel model.Channel, referenceCount int) bool {
	return referenceCount <= model.DefaultImageChannelMaxReferenceImages ||
		channel.GetImageMaxReferenceImages() >= referenceCount
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
	Provider                     string              `json:"provider,omitempty"`
	Protocol                     string              `json:"protocol,omitempty"`
	UpstreamModel                string              `json:"upstream_model,omitempty"`
	AuditVersion                 int                 `json:"audit_version,omitempty"`
	Decision                     *ImageRetryDecision `json:"decision,omitempty"`
	ChannelId                    int                 `json:"channel_id"`
	KeyFingerprint               string              `json:"key_fingerprint,omitempty"`
	CredentialFingerprint        string              `json:"credential_fingerprint,omitempty"`
	KeyIndex                     int                 `json:"key_index"`
	StartedAt                    int64               `json:"started_at"`
	FinishedAt                   int64               `json:"finished_at,omitempty"`
	Dispatched                   bool                `json:"dispatched"`
	TimedOut                     bool                `json:"timed_out,omitempty"`
	RetryOnTimeout               bool                `json:"retry_on_timeout,omitempty"`
	RetryReferenceTimeoutLocally bool                `json:"retry_reference_timeout_locally,omitempty"`
	SynchronousRetryableResponse bool                `json:"synchronous_retryable_response,omitempty"`
	SubmissionRejected           bool                `json:"submission_rejected,omitempty"`
	RetrySameChannel             bool                `json:"retry_same_channel,omitempty"`
	ReferenceMode                string              `json:"reference_mode"`
	TransportUsed                string              `json:"reference_transport_used,omitempty"`
	ReferenceFallback            bool                `json:"reference_fallback,omitempty"`
	Code                         int                 `json:"error_code,omitempty"`
	HTTPStatus                   int                 `json:"http_status,omitempty"`
	ErrorMessage                 string              `json:"error_message,omitempty"`
	ProviderCode                 string              `json:"provider_code,omitempty"`
	ProviderStatus               string              `json:"provider_status,omitempty"`
	UpstreamRequestID            string              `json:"upstream_request_id,omitempty"`
	UpstreamTaskID               string              `json:"upstream_task_id,omitempty"`
	ReferenceFailure             bool                `json:"upstream_reference_failure,omitempty"`
}

func isCompletedImageReferenceFetchFailure(attempt ImageChannelAttempt) bool {
	return attempt.Dispatched && attempt.FinishedAt > 0 && attempt.ReferenceFailure && attempt.Code == 602
}

func ImageAccountAttemptRouting(attempts []ImageChannelAttempt, retries int, platform string, maxSwitches int) ImageAccountRouting {
	routing := ImageAccountRouting{}
	effectiveRetries := retries
	for _, attempt := range attempts {
		if attempt.ReferenceFailure && attempt.ReferenceMode == "passthrough_fallback_local" && (attempt.Code == 601 || attempt.Code == 602 || attempt.Code == 610 || attempt.Code == 613 || attempt.Code == 603 && attempt.TransportUsed == "remote" && imageReferenceTransportConstraint(attempt.ErrorMessage) == "youtube_url") {
			effectiveRetries = max(effectiveRetries, 1)
			break
		}
	}
	var reference []ImageChannelAttempt
	var last string
	var lastAttempt ImageChannelAttempt
	switches := 0
	legacy, lastChannel, channelSwitches := false, 0, 0
	for _, attempt := range attempts {
		if !attempt.Dispatched {
			continue
		}
		if attempt.TimedOut && attempt.FinishedAt > 0 {
			if attempt.KeyFingerprint != "" && !slices.Contains(routing.TimedOutAccounts, attempt.KeyFingerprint) {
				routing.TimedOutAccounts = append(routing.TimedOutAccounts, attempt.KeyFingerprint)
			}
			if attempt.CredentialFingerprint != "" && !slices.Contains(routing.TimedOutCredentials, attempt.CredentialFingerprint) {
				routing.TimedOutCredentials = append(routing.TimedOutCredentials, attempt.CredentialFingerprint)
			}
		}
		if imageAttemptCompatibilityCode(attempt) != "" {
			if attempt.KeyFingerprint != "" && !slices.Contains(routing.IncompatibleAccounts, attempt.KeyFingerprint) {
				routing.IncompatibleAccounts = append(routing.IncompatibleAccounts, attempt.KeyFingerprint)
			}
			if attempt.CredentialFingerprint != "" && !slices.Contains(routing.IncompatibleCredentials, attempt.CredentialFingerprint) {
				routing.IncompatibleCredentials = append(routing.IncompatibleCredentials, attempt.CredentialFingerprint)
			}
			// Legacy attempts cannot identify a reordered key safely. Exclude
			// their entire channel instead of guessing an account identity.
			if attempt.KeyFingerprint == "" && attempt.CredentialFingerprint == "" && !slices.Contains(routing.IncompatibleChannels, attempt.ChannelId) {
				routing.IncompatibleChannels = append(routing.IncompatibleChannels, attempt.ChannelId)
			}
		}
		if attempt.KeyFingerprint == "" {
			legacy = true
		}
		if lastChannel != 0 && lastChannel != attempt.ChannelId {
			channelSwitches++
		}
		lastChannel = attempt.ChannelId
		if last != "" && last != attempt.KeyFingerprint {
			switches++
		}
		last = attempt.KeyFingerprint
		lastAttempt = attempt
		referenceFetchFailure := isCompletedImageReferenceFetchFailure(attempt)
		routing.WaitForCooldown = routing.WaitForCooldown || referenceFetchFailure || (attempt.SynchronousRetryableResponse || attempt.SubmissionRejected) && attempt.FinishedAt > 0
		if attempt.ReferenceFailure && !referenceFetchFailure {
			reference = append(reference, attempt)
		}
	}
	lastTimedOut := lastAttempt.TimedOut && lastAttempt.FinishedAt > 0
	lastReferenceFetchFailure := isCompletedImageReferenceFetchFailure(lastAttempt)
	lastSubmissionResponse := (lastAttempt.SynchronousRetryableResponse || lastAttempt.SubmissionRejected) && lastAttempt.FinishedAt > 0
	lastCompatibilityCode := imageAttemptCompatibilityCode(lastAttempt)
	lastIncompatible := lastCompatibilityCode != ""
	lastReferenceSizeRejection := lastAttempt.Code == 603 && lastAttempt.HTTPStatus == http.StatusBadRequest && lastCompatibilityCode == "upstream_image_reference_incompatible" && imageReferenceSizeRejectionPattern.MatchString(strings.TrimSpace(lastAttempt.ErrorMessage))
	lastReferenceTransportRejection := lastCompatibilityCode == "upstream_image_reference_incompatible" && imageReferenceTransportConstraint(lastAttempt.ErrorMessage) == "file_data"
	if lastAttempt.Dispatched && lastAttempt.FinishedAt > 0 && lastAttempt.RetrySameChannel && lastAttempt.Code == 606 {
		// A verified terminal connection failure retries within its original
		// channel, while retaining credential exclusions and cooldowns.
		routing.PinChannel, routing.Avoid = lastAttempt.ChannelId, lastAttempt.KeyFingerprint
		routing.WaitForCooldown = true
		return routing
	}
	localReferenceRecovery := lastAttempt.RetrySameChannel && lastAttempt.ReferenceMode == "passthrough_fallback_local" ||
		lastAttempt.RetryReferenceTimeoutLocally && IsImageReferenceDownloadTimeout(&AsyncImageFailure{Code: lastAttempt.Code, Message: lastAttempt.ErrorMessage})
	if lastReferenceFetchFailure && localReferenceRecovery && !lastAttempt.ReferenceFallback && lastAttempt.ReferenceMode != "local" &&
		(lastAttempt.TransportUsed == "remote" || lastAttempt.TransportUsed == "mixed") {
		localAttempted := slices.ContainsFunc(attempts, func(attempt ImageChannelAttempt) bool {
			return attempt.ChannelId == lastAttempt.ChannelId && attempt.Dispatched && attempt.ReferenceFallback && attempt.ReferenceMode == "local"
		})
		if !localAttempted {
			// This channel opted into one local recovery before pool rotation.
			// Keep credential exclusions and cooldowns; pinning the channel must
			// not authorize another dispatch while its only account is cooling.
			routing.Pin, routing.AvoidChannel = "", 0
			routing.PinChannel, routing.Avoid = lastAttempt.ChannelId, lastAttempt.KeyFingerprint
			return routing
		}
	}
	if legacy {
		if !routing.WaitForCooldown {
			routing.PinChannel, routing.ExcludedChannels, routing.Stop = ImageAttemptRouting(attempts, effectiveRetries)
		}
		for _, attempt := range attempts {
			if attempt.Dispatched && attempt.FinishedAt > 0 && routing.PinChannel == attempt.ChannelId && (attempt.TimedOut || imageAttemptCompatibilityCode(attempt) != "") {
				routing.PinChannel = 0
				break
			}
		}
		if lastAttempt.FinishedAt > 0 && (lastAttempt.Code == 605 || lastAttempt.Code == 606 || lastAttempt.HTTPStatus == 429) {
			routing.AvoidChannel = lastAttempt.ChannelId
		}
		if lastReferenceFetchFailure || lastSubmissionResponse || lastReferenceSizeRejection || lastReferenceTransportRejection {
			routing.AvoidChannel = lastAttempt.ChannelId
		}
		if platform == "gemini" && lastChannel != 0 && channelSwitches >= maxSwitches && !lastTimedOut && !lastReferenceFetchFailure {
			if lastIncompatible || slices.Contains(routing.ExcludedChannels, lastChannel) || routing.PinChannel != 0 && routing.PinChannel != lastChannel {
				routing.Stop = true
				return routing
			}
			routing.PinChannel, routing.AvoidChannel = lastChannel, 0
		}
		return routing
	}
	if len(reference) > 0 && !routing.WaitForCooldown {
		first := reference[0].KeyFingerprint
		for _, attempt := range reference {
			if attempt.KeyFingerprint != first {
				routing.Stop = true
				return routing
			}
		}
		firstTimedOut := slices.Contains(routing.TimedOutAccounts, first) || reference[0].CredentialFingerprint != "" && slices.Contains(routing.TimedOutCredentials, reference[0].CredentialFingerprint)
		firstIncompatible := slices.Contains(routing.IncompatibleAccounts, first) || reference[0].CredentialFingerprint != "" && slices.Contains(routing.IncompatibleCredentials, reference[0].CredentialFingerprint)
		if !firstTimedOut && !firstIncompatible {
			if len(reference) <= effectiveRetries {
				routing.Pin = first
			} else {
				routing.Excluded = []string{first}
			}
		}
	}
	// Prefer another account only after a completed transient rejection.
	if lastAttempt.FinishedAt > 0 && (lastAttempt.Code == 605 || lastAttempt.Code == 606 || lastAttempt.HTTPStatus == 429) {
		routing.Avoid = last
	}
	if lastReferenceFetchFailure || lastSubmissionResponse || lastReferenceSizeRejection || lastReferenceTransportRejection {
		routing.Avoid, routing.AvoidChannel = last, lastAttempt.ChannelId
	}
	if platform == "gemini" && last != "" && switches >= maxSwitches && !lastTimedOut && !lastReferenceFetchFailure {
		if lastIncompatible || slices.Contains(routing.Excluded, last) || routing.Pin != "" && routing.Pin != last {
			routing.Stop = true
			return routing
		}
		routing.Pin, routing.Avoid = last, ""
	}
	return routing
}

// ImageAttemptRouting retains the legacy bounded account fallback for ambiguous
// reference failures. Confirmed fetch failures use account cooldown and rotation.
func ImageAttemptRouting(attempts []ImageChannelAttempt, retries int) (pin int, excluded []int, stop bool) {
	var reference []ImageChannelAttempt
	for _, attempt := range attempts {
		if !attempt.Dispatched {
			continue
		}
		if attempt.ReferenceFailure && !isCompletedImageReferenceFetchFailure(attempt) {
			reference = append(reference, attempt)
		}
	}
	if len(reference) > 0 {
		first := reference[0].ChannelId
		for _, attempt := range reference {
			if attempt.ChannelId != first {
				return 0, []int{first}, true
			}
		}
		if len(reference) <= retries {
			pin = first
		} else {
			excluded = []int{first}
		}
	}
	return pin, excluded, false
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
	index := slices.IndexFunc(catalog, func(capability ImageModelCapability) bool { return capability.MatchesMedia(request.Model, "image") })
	if index < 0 {
		return policy, model.ImageFundingSelection{}, errors.New("image model is not available in this platform catalog")
	}
	capability := catalog[index]
	invalid := func(message string) (model.ImageGroupPolicy, model.ImageFundingSelection, error) {
		return policy, model.ImageFundingSelection{}, &AsyncImageFailure{Code: 604, InternalCode: "invalid_request", Message: message, HTTPStatus: 400}
	}
	resolution := AsyncImageRoutingResolution(request)
	if resolution == "0.5K" && !capability.AllowHalfK {
		return invalid("unsupported_image_dimensions: model does not support 0.5K")
	}
	if resolution != "" && resolution != "AUTO" && !slices.Contains(capability.Resolutions, resolution) && !(resolution == "0.5K" && capability.AllowHalfK) {
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
	references := request.ReferenceImageCount()
	if references > capability.MaxReferenceImages {
		return invalid("too_many_reference_images_for_model")
	}
	channels, err := ImageCandidateChannels(ctx, policy, request.Model, resolution)
	if err != nil {
		return policy, model.ImageFundingSelection{}, err
	}
	channels = slices.DeleteFunc(channels, func(ch model.Channel) bool {
		capability := ImageChannelCapability(ch, request.Model)
		if request.Provider != "" && capability.Provider != request.Provider {
			return true
		}
		if policy.Platform == "mixed" || ch.Type == constant.ChannelTypeTabTab || ch.Type == constant.ChannelTypeNewAPI && ch.GetSetting().TianyueMediaCompatibility {
			return !MixedImageChannelRequestSupported(ch, request)
		}
		return !ImageCapabilitySupportsRequest(capability, request)
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
	if request.Reservation != nil {
		// The worker loaded this server-only field from the durable bill. Its
		// own hold must not make polling or a safe retry look unaffordable.
		return policy, model.ImageFundingSelection{Source: request.Reservation.FundingSource, SubscriptionId: request.Reservation.SubscriptionId}, nil
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
