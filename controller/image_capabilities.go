package controller

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	gatewaydto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func GetImageWorkbenchCapabilities(c *gin.Context) {
	ctx := c.Request.Context()
	tokenId, err := strconv.Atoi(c.Param("token_id"))
	if err != nil {
		imageManagementError(c, 400, errors.New("Invalid Token ID"))
		return
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", tokenId, c.GetInt("id")).Take(&token).Error; err != nil {
		imageManagementError(c, 404, errors.New("Token was not found"))
		return
	}
	cfg, err := service.GetImageRuntimeConfig(ctx)
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	platforms := make([]gin.H, 0, 2)
	models := make([]gin.H, 0)
	versionFacts := make([]any, 0)
	valid := token.Status == common.TokenStatusEnabled && (token.ExpiredTime == -1 || token.ExpiredTime > time.Now().Unix())
	var owner model.User
	if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", token.UserId, common.UserStatusEnabled).Take(&owner).Error; err != nil {
		imageManagementError(c, 403, errors.New("Image owner is unavailable"))
		return
	}
	usableGroups := service.GetUserUsableGroups(owner.Group)
	for _, platform := range service.RegisteredImageProviders() {
		type scopedCapability struct {
			policy     model.ImageGroupPolicy
			capability service.ImageModelCapability
		}
		families := []string{"openai"}
		if platform == "gemini" || platform == "vertex" {
			families = append(families, "gemini")
		}
		scoped := make([]scopedCapability, 0)
		for _, family := range families {
			policy, catalog, err := service.ResolveAsyncImagePolicy(ctx, token, service.AsyncImageRequest{Provider: platform, Platform: family})
			if err != nil {
				imageManagementError(c, 503, err)
				return
			}
			// Missing provider policies retain the executable synchronous catalog.
			// The adaptor declaration and configured abilities determine membership.
			if policy.Id == 0 {
				policy.Enabled = true
				var abilities []model.Ability
				if err := model.DB.WithContext(ctx).Where(map[string]any{"group": policy.Group, "enabled": true}).Find(&abilities).Error; err != nil {
					imageManagementError(c, 503, err)
					return
				}
				seen := make(map[string]bool)
				for _, ability := range abilities {
					if seen[ability.Model] {
						continue
					}
					channel, err := model.CacheGetChannel(ability.ChannelId)
					if err != nil {
						continue
					}
					declared := service.ImageChannelCapability(*channel, ability.Model)
					expectedProtocol := "openai_images"
					if family == "gemini" {
						expectedProtocol = "gemini_native"
					}
					if declared.Provider != platform || !declared.Generate || declared.Protocol != expectedProtocol {
						continue
					}
					seen[ability.Model] = true
					catalog = append(catalog, service.ImageModelCapability{Id: ability.Model, Label: ability.Model, MaxOutputImages: dto.MaxImageN, MaxReferenceImages: cfg.MaxReferences, Resolutions: []string{"1K", "2K", "4K"}})
				}
			}
			for _, capability := range catalog {
				scoped = append(scoped, scopedCapability{policy: policy, capability: capability})
			}
			versionFacts = append(versionFacts, policy)
		}

		eligibleCount := 0
		providerAvailable := false
		providerMode, providerProtocol, providerGroup := "unavailable", "", ""
		providerReason := "Token or image platform is unavailable"
		seenModels := make(map[string]bool)
		for _, entry := range scoped {
			capability, policy := entry.capability, entry.policy
			if seenModels[capability.Id] || token.ModelLimitsEnabled && !token.GetModelLimitsMap()[capability.Id] {
				continue
			}
			mode := "realtime"
			if policy.AsyncEnabled {
				mode = "async"
			}
			available := valid && policy.Enabled
			if _, allowed := usableGroups[policy.Group]; !allowed {
				available = false
			}
			if mode == "async" {
				if !cfg.AsyncEnabled || !common.RedisEnabled || common.RDB == nil || !service.ImageEncryptionAvailable() {
					available = false
					providerReason = "Async image service is disabled"
				} else if store, err := service.GetImageStorage(ctx, "temporary"); err != nil {
					available = false
					providerReason = "Temporary image storage is unavailable"
				} else {
					versionFacts = append(versionFacts, store.Profile.ProfileId)
				}
			}
			resolutions := make([]string, 0, len(capability.Resolutions))
			for _, resolution := range capability.Resolutions {
				channels, err := service.ImageCandidateChannels(ctx, policy, capability.Id, resolution)
				if err != nil {
					imageManagementError(c, 503, err)
					return
				}
				channels = slices.DeleteFunc(channels, func(channel model.Channel) bool {
					return service.ImageChannelCapability(channel, capability.Id).Provider != platform
				})
				if len(channels) > 0 {
					resolutions = append(resolutions, resolution)
				}
			}
			capability.Resolutions = resolutions
			if len(resolutions) == 0 {
				continue
			}
			channels, err := service.ImageCandidateChannels(ctx, policy, capability.Id, "")
			if err != nil {
				imageManagementError(c, 503, err)
				return
			}
			channels = slices.DeleteFunc(channels, func(channel model.Channel) bool {
				return service.ImageChannelCapability(channel, capability.Id).Provider != platform
			})
			if len(channels) == 0 {
				continue
			}
			declared := service.ImageChannelCapability(channels[0], capability.Id)
			if !declared.Edit && declared.ReferenceField == "" {
				capability.MaxReferenceImages = 0
			}
			if declared.Protocol == "gemini_native" {
				capability.MaxOutputImages = 1
			}
			seenModels[capability.Id] = true
			eligibleCount++
			models = append(models, gin.H{"provider": platform, "media_type": "image", "supports_generation": declared.Generate, "supports_edit": declared.Edit, "supports_reference_generation": declared.ReferenceField != "", "supported_parameters": declared.Parameters, "id": capability.Id, "label": capability.Label, "platform": platform, "mode": mode, "protocol": declared.Protocol, "capability": capability, "available": available})
			for _, channel := range channels {
				versionFacts = append(versionFacts, []any{channel.Id, channel.Status, channel.Models, channel.GetPriority(), channel.GetWeight(), channel.ModelMapping, channel.Setting})
			}
			if providerGroup == "" {
				providerGroup = policy.Group
			} else if providerGroup != policy.Group {
				providerGroup = "auto"
			}
			if providerProtocol == "" {
				providerProtocol = declared.Protocol
			} else if providerProtocol != declared.Protocol {
				providerProtocol = "mixed"
			}
			if mode == "async" || providerMode == "unavailable" {
				providerMode = mode
			}
			if available {
				providerAvailable, providerReason = true, ""
			}
		}
		if eligibleCount == 0 {
			providerReason = "No executable image model is available"
		}
		if !providerAvailable {
			providerMode = "unavailable"
		}
		platforms = append(platforms, gin.H{"provider": platform, "media_type": "image", "platform": platform, "mode": providerMode, "protocol": providerProtocol, "available": providerAvailable, "reason": providerReason, "group": providerGroup})
	}
	videoCfg, err := service.GetMediaRuntimeConfig(ctx)
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	videoModels := make([]gin.H, 0)
	generation := pluginruntime.DefaultRegistry.Generation()
	if generation != nil {
		var abilities []model.Ability
		if err := model.DB.WithContext(ctx).Where(map[string]any{"enabled": true}).Find(&abilities).Error; err != nil {
			imageManagementError(c, 503, err)
			return
		}
		seen := make(map[string]bool)
		for _, ability := range abilities {
			if token.ModelLimitsEnabled && !token.GetModelLimitsMap()[ability.Model] {
				continue
			}
			channel, err := model.CacheGetChannel(ability.ChannelId)
			if err != nil || channel.Status != common.ChannelStatusEnabled {
				continue
			}
			candidates := generation.LookupEndpointCandidates("POST", "/v1/videos", ability.Model)
			if len(candidates) == 0 {
				if alias, ok := model.ResolveTaskModelAlias(generation, ability.Model); ok {
					candidates = generation.LookupEndpointCandidates("POST", "/v1/videos", alias.Declared)
				}
			}
			for _, candidate := range candidates {
				if candidate.Plugin == nil || candidate.Protocol != "openai_video" {
					continue
				}
				provider := candidate.Plugin.Meta.Key
				key := provider + ":" + ability.Model
				policy, catalog, err := service.ResolveImagePolicy(ctx, token, provider)
				if err != nil {
					imageManagementError(c, 503, err)
					return
				}
				allowedGroup := policy.Group == ability.Group || policy.Group == "auto" && slices.Contains(service.GetUserAutoGroup(owner.Group), ability.Group)
				if !allowedGroup || !service.GroupInUserUsableGroups(owner.Group, ability.Group) || policy.Id != 0 && (!policy.Enabled || !policy.AsyncEnabled || !slices.ContainsFunc(catalog, func(capability service.ImageModelCapability) bool { return capability.Id == ability.Model })) {
					continue
				}
				matches, _ := model.ChannelSatisfiesFilters(channel, ability.Model, []gatewaydto.ChannelFilter{{Kind: gatewaydto.FilterTaskPluginIdentity, TaskPluginKey: provider, TaskPluginChannelTypes: candidate.Plugin.Meta.ChannelTypes}})
				if seen[key] || !matches {
					continue
				}
				seen[key] = true
				available := valid && videoCfg.VideoAsyncEnabled && service.ImageEncryptionAvailable() && common.RedisEnabled && common.RDB != nil
				videoModels = append(videoModels, gin.H{"id": ability.Model, "label": ability.Model, "provider": provider, "media_type": "video", "protocol": "openai_video", "mode": "async", "available": available, "supported_parameters": []string{"model", "prompt", "seconds", "size", "input_reference", "provider_extensions"}, "max_duration_seconds": relaycommon.MaxTaskDurationSeconds})
			}
		}
	}
	versionFacts = append(versionFacts, videoCfg, videoModels)
	var pools []model.ImageChannelPool
	if err := model.DB.WithContext(ctx).Find(&pools).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	versionFacts = append(versionFacts, []any{token.Id, token.Status, token.ExpiredTime, token.Group, token.ModelLimitsEnabled, token.ModelLimits}, pools, cfg)
	encoded, err := common.Marshal(versionFacts)
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	slices.SortFunc(models, func(a, b gin.H) int { return strings.Compare(a["id"].(string), b["id"].(string)) })
	imageManagementData(c, gin.H{"api_key_id": token.Id, "platforms": platforms, "models": models, "video_models": videoModels, "image_providers": service.RegisteredImageProviders(), "capability_version": service.ImageIdentityHash(string(encoded)), "gateway_base_url": AsyncImageGatewayBase(), "local_library_defaults": gin.H{"retention_days": 30, "max_items": 100, "max_bytes": 200 << 20}, "poll_interval_seconds": 3})
}
