package controller

import (
	"io"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// routeConditionalVideo returns true when it handled or rejected admission.
// Original-route fallback leaves a frozen rule in context and uses the existing
// selector, restricted to the group that matched that rule.
func routeConditionalVideo(c *gin.Context, token model.Token, policy model.ImageGroupPolicy, groups []string, route *service.MixedVideoRouting, raw []byte, contentType string, facts map[string]any) bool {
	if route.Operation != "" && route.Operation != "create" {
		return false
	}
	value, _ := c.Get("conditional_video_rules_config")
	config, ok := value.(model.VideoRoutingConfig)
	if !ok {
		AsyncImagePublicError(c, 503, "routing_unavailable", "Video routing configuration is unavailable")
		c.Abort()
		return true
	}
	if !service.VideoRoutingAppliesToSource(config, route.Model, groups) {
		return false
	}
	sourceModes := make(map[string]string)
	for _, group := range groups {
		check, groupPolicy := *route, policy
		groupPolicy.Group = group
		check.Group, check.BindingKey = group, service.MediaPoolBindingKey(groupPolicy, route.Model, route.Resolution, "video")
		_, catalog, policyErr := service.MixedVideoPolicy(c.Request.Context(), token, &check)
		if policyErr != nil {
			continue
		}
		capability, _ := service.MixedVideoPolicyCapability(catalog, route.Model)
		channels, channelErr := service.MixedVideoPoolChannels(c.Request.Context(), groupPolicy, route.Model, route.Resolution)
		if channelErr != nil {
			AsyncImagePublicError(c, 503, "routing_unavailable", "Video source routing is unavailable")
			c.Abort()
			return true
		}
		for _, channel := range channels {
			candidate, decodeErr := service.DecodeMixedVideoCandidate(c.Request.Context(), channel, route.Model, route.Operation, raw, capability.VideoOverrides, token.UserId, contentType)
			if decodeErr != nil {
				continue
			}
			mode, _ := candidate.Intent["action"].(string)
			if mode == "" {
				mode = "text_to_video"
			}
			sourceModes[group] = mode
			break
		}
	}
	constraints := service.GetChannelConstraints(c)
	pin, pinned, _ := constraints.ResolvedPin()
	pinnedID := 0
	if pinned {
		pinnedID = pin.ChannelId
	}
	for _, rule := range config.Rules {
		one := config
		one.Rules = []model.VideoRoutingRule{rule}
		for _, group := range groups {
			mode, validSource := sourceModes[group]
			if !validSource || len(service.MatchVideoRoutingRules(one, group, route.Model, mode, facts)) == 0 {
				continue
			}
			check, groupPolicy := *route, policy
			groupPolicy.Group = group
			check.Group, check.BindingKey = group, service.MediaPoolBindingKey(groupPolicy, route.Model, route.Resolution, "video")
			if _, _, err := service.MixedVideoPolicy(c.Request.Context(), token, &check); err != nil {
				continue
			}
			if token.ModelLimitsEnabled && !token.GetModelLimitsMap()[route.Model] {
				AsyncImagePublicError(c, 403, "model_not_allowed", "Requested video model is not allowed")
				c.Abort()
				return true
			}
			snapshot, candidate, dispatch, dispatchType, err := service.PlanVideoRouting(c.Request.Context(), config, rule, group, route.Model, route.Operation, raw, contentType, facts, token.UserId, pinnedID, constraints.Filters)
			if err != nil {
				AsyncImagePublicError(c, 400, "provider_unavailable", err.Error())
				c.Abort()
				return true
			}
			c.Set(service.VideoRoutingContextKey, snapshot)
			*route = check
			if candidate == nil {
				c.Set("conditional_video_fallback_group", group)
				return false
			}
			common.CleanupBodyStorage(c)
			storage, err := common.CreateBodyStorage(dispatch)
			if err != nil {
				c.AbortWithStatus(503)
				return true
			}
			c.Set(common.KeyBodyStorage, storage)
			c.Request.Body, c.Request.ContentLength = io.NopCloser(storage), int64(len(dispatch))
			c.Request.Header.Set("Content-Type", dispatchType)
			pinMixedVideoCandidate(c, *candidate, route, 0)
			c.Set("mixed_video_billing_facts", facts)
			constraints.AddPin(dto.ChannelPin{ChannelId: candidate.Channel.Id, Source: "media_policy", Rank: 20, RetryMode: dto.PinRetrySingleAttempt})
			c.Next()
			return true
		}
	}
	return false
}

func pinConditionalVideoWorker(c *gin.Context, job model.AsyncMediaJob, request service.AsyncVideoRequest) {
	var channel model.Channel
	if model.DB.WithContext(c.Request.Context()).Where("id = ? AND status = ?", job.ChannelId, common.ChannelStatusEnabled).Take(&channel).Error != nil {
		c.AbortWithStatus(403)
		return
	}
	index := slices.Index(channel.GetKeys(), request.Channel.Key)
	if index < 0 || channel.ChannelInfo.MultiKeyStatusList[index] != 0 && channel.ChannelInfo.MultiKeyStatusList[index] != common.ChannelStatusEnabled {
		AsyncImagePublicError(c, 403, "provider_unavailable", "Conditional video account permission changed")
		c.Abort()
		return
	}
	candidate, err := service.DecodeVideoExecution(c.Request.Context(), request, channel, job.Model, job.Group, service.VideoModelOverrides{}, job.UserId)
	if err != nil || candidate.Binding.Plugin.Meta.Key != job.Provider || candidate.Binding.Plugin.Engine.SourceFingerprint() != request.PluginHash {
		AsyncImagePublicError(c, 400, "provider_unavailable", "Conditional video execution is no longer available")
		c.Abort()
		return
	}
	c.Set(service.VideoRoutingContextKey, request.ConditionalRouting)
	pinMixedVideoCandidate(c, candidate, request.MixedRouting, job.AttemptGeneration)
	c.Next()
}
