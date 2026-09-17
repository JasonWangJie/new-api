package controller

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
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
	for _, platform := range []string{"openai", "gemini"} {
		policy, catalog, err := service.ResolveImagePolicy(ctx, token, platform)
		if err != nil {
			imageManagementError(c, 503, err)
			return
		}
		// Missing policies preserve executable synchronous image capabilities.
		var existing model.ImageGroupPolicy
		if err := model.DB.WithContext(ctx).Where("policy_key = ?", service.ImageIdentityHash(policy.Group, platform)).Take(&existing).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			policy.Enabled = true
			var abilities []model.Ability
			if err := model.DB.WithContext(ctx).Where(map[string]any{"group": policy.Group, "enabled": true}).Find(&abilities).Error; err != nil {
				imageManagementError(c, 503, err)
				return
			}
			seen := make(map[string]bool)
			for _, ability := range abilities {
				lower := strings.ToLower(ability.Model)
				if ability.Group != policy.Group || seen[ability.Model] || !strings.Contains(lower, "image") && !strings.Contains(lower, "dall-e") {
					continue
				}
				seen[ability.Model] = true
				catalog = append(catalog, service.ImageModelCapability{Id: ability.Model, Label: ability.Model, MaxOutputImages: dto.MaxImageN, MaxReferenceImages: cfg.MaxReferences, Resolutions: []string{"1K", "2K", "4K"}})
			}
		}
		mode, protocol := "realtime", "openai_images"
		if platform == "gemini" {
			protocol = "gemini_native"
		}
		if policy.AsyncEnabled {
			mode = "async"
			protocol = "openai_async"
			if platform == "gemini" {
				protocol = "gemini_sc"
			}
		}
		available := valid && policy.Enabled
		if _, allowed := usableGroups[policy.Group]; !allowed {
			available = false
		}
		reason := ""
		if !available {
			reason = "Token or image platform is unavailable"
		}
		if mode == "async" {
			if !cfg.AsyncEnabled || !common.RedisEnabled || common.RDB == nil || !service.ImageEncryptionAvailable() {
				available = false
				reason = "Async image service is disabled"
			} else if store, err := service.GetImageStorage(ctx, "temporary"); err != nil {
				available = false
				reason = "Temporary image storage is unavailable"
			} else {
				versionFacts = append(versionFacts, store.Profile.ProfileId)
			}
		}
		eligibleCount := 0
		for _, capability := range catalog {
			if token.ModelLimitsEnabled && !token.GetModelLimitsMap()[capability.Id] {
				continue
			}
			resolutions := make([]string, 0, len(capability.Resolutions))
			for _, resolution := range capability.Resolutions {
				channels, err := service.ImageCandidateChannels(ctx, policy, capability.Id, resolution)
				if err != nil {
					imageManagementError(c, 503, err)
					return
				}
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
			if len(channels) == 0 {
				continue
			}
			eligibleCount++
			models = append(models, gin.H{"id": capability.Id, "label": capability.Label, "platform": platform, "mode": mode, "protocol": protocol, "capability": capability, "available": available})
			for _, channel := range channels {
				versionFacts = append(versionFacts, []any{channel.Id, channel.Status, channel.Models, channel.GetPriority(), channel.GetWeight(), channel.ModelMapping, channel.Setting})
			}
		}
		if eligibleCount == 0 {
			available = false
			reason = "No executable image model is available"
		}
		if !available {
			mode = "unavailable"
		}
		platforms = append(platforms, gin.H{"platform": platform, "mode": mode, "protocol": protocol, "available": available, "reason": reason, "group": policy.Group})
		versionFacts = append(versionFacts, policy)
	}
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
	imageManagementData(c, gin.H{"api_key_id": token.Id, "platforms": platforms, "models": models, "capability_version": service.ImageIdentityHash(string(encoded)), "gateway_base_url": AsyncImageGatewayBase(), "local_library_defaults": gin.H{"retention_days": 30, "max_items": 100, "max_bytes": 200 << 20}, "poll_interval_seconds": 3})
}
