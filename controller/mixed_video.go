package controller

import (
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	gatewaydto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// Serialize local management writes that validate policy and price together.
var mixedVideoConfigurationMu sync.Mutex

// PinAsyncVideoEndpoint keeps ordinary provider routing unchanged. Mixed aliases
// are resolved per explicit channel before the global alias resolver can run.
func PinAsyncVideoEndpoint() gin.HandlerFunc {
	ordinary := middleware.PinTaskPluginEndpoint()
	return func(c *gin.Context) {
		if c.GetString("async_media_provider") != "" {
			ordinary(c)
			return
		}
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			AsyncImagePublicError(c, 400, "invalid_request", "Video body is unavailable")
			c.Abort()
			return
		}
		raw, err := storage.Bytes()
		_, body, parseErr := service.MixedVideoRequestContext(raw, c.GetHeader("Content-Type"), c.GetString("async_media_operation"))
		if err != nil || parseErr != nil {
			ordinary(c)
			return
		}
		publicModel, _ := body["model"].(string)
		var token model.Token
		if err := model.DB.WithContext(c.Request.Context()).Where("id = ? AND user_id = ?", c.GetInt("token_id"), c.GetInt("id")).Take(&token).Error; err != nil {
			c.AbortWithStatus(503)
			return
		}
		policy, catalog, err := service.ResolveImagePolicy(c.Request.Context(), token, "mixed")
		if err != nil {
			c.AbortWithStatus(503)
			return
		}
		capability, matched := service.MixedVideoPolicyCapability(catalog, publicModel)
		if !matched {
			ordinary(c)
			return
		}
		if !policy.Enabled || !policy.AsyncEnabled {
			AsyncImagePublicError(c, 403, "provider_unavailable", "Mixed video policy is disabled")
			c.Abort()
			return
		}
		operation := c.GetString("async_media_operation")
		groups := []string{policy.Group}
		if policy.Group == "auto" {
			var owner model.User
			if err := model.DB.WithContext(c.Request.Context()).First(&owner, token.UserId).Error; err != nil {
				c.AbortWithStatus(503)
				return
			}
			groups = service.GetRequestAutoGroups(c, owner.Group)
		}
		profiles := make([]jsplugin.VideoProfile, 0)
		for _, group := range groups {
			poolPolicy := policy
			poolPolicy.Group = group
			variants, err := service.MixedVideoModelProfiles(c.Request.Context(), poolPolicy, publicModel, capability.VideoOverrides, true)
			if err == nil {
				profiles = append(profiles, variants...)
			}
		}
		raw, contentType, body, err := service.NormalizeMixedVideoDefaults(raw, c.GetHeader("Content-Type"), operation, profiles)
		if err != nil {
			AsyncImagePublicError(c, 400, "invalid_request", err.Error())
			c.Abort()
			return
		}
		common.CleanupBodyStorage(c)
		normalizedStorage, err := common.CreateBodyStorage(raw)
		if err != nil {
			c.AbortWithStatus(503)
			return
		}
		c.Set(common.KeyBodyStorage, normalizedStorage)
		c.Request.Body, c.Request.ContentLength = io.NopCloser(normalizedStorage), int64(len(raw))
		c.Request.Header.Set("Content-Type", contentType)
		facts, err := service.MixedVideoPublicFacts(body, operation, profiles...)
		if err != nil {
			AsyncImagePublicError(c, 400, "invalid_request", err.Error())
			c.Abort()
			return
		}
		resolution, _ := facts["resolution"].(string)
		route := &service.MixedVideoRouting{Group: policy.Group, BindingKey: service.MediaPoolBindingKey(policy, publicModel, service.VideoPoolResolution(resolution), "video"), PoolMode: policy.PoolMode, Model: publicModel, Operation: operation, Resolution: service.VideoPoolResolution(resolution)}
		constraints := service.GetChannelConstraints(c)
		forced, forcedOK, _ := constraints.ResolvedPin()
		candidates := make([]service.MixedVideoCandidate, 0)
		candidateGroups := make([]string, 0)
		weights := make([]int, 0)
		var rejection error
		for _, group := range groups {
			poolPolicy := policy
			poolPolicy.Group = group
			check := *route
			check.Group, check.BindingKey = group, service.MediaPoolBindingKey(poolPolicy, publicModel, route.Resolution, "video")
			if _, _, err := service.MixedVideoPolicy(c.Request.Context(), token, &check); err != nil {
				rejection = err
				continue
			}
			channels, err := service.MixedVideoPoolChannels(c.Request.Context(), poolPolicy, publicModel, route.Resolution)
			if err != nil {
				c.AbortWithStatus(503)
				return
			}
			for _, channel := range channels {
				if forcedOK && channel.Id != forced.ChannelId {
					continue
				}
				if matches, _ := model.ChannelSatisfiesFilters(&channel, publicModel, constraints.Filters); !matches {
					continue
				}
				candidate, err := service.DecodeMixedVideoCandidate(c.Request.Context(), channel, publicModel, operation, raw, capability.VideoOverrides, token.UserId, c.GetHeader("Content-Type"))
				if err != nil {
					rejection = err
					continue
				}
				if len(service.ImageChannelAccounts(channel, service.ImageAccountRouting{})) == 0 {
					continue
				}
				candidates = append(candidates, candidate)
				candidateGroups = append(candidateGroups, group)
				weights = append(weights, channel.GetWeight())
			}
		}
		if len(candidates) == 0 {
			message := "No compatible video channel is available in the mixed pool"
			if rejection != nil {
				message = service.SanitizeAsyncImageDiagnostic(rejection.Error(), 512)
			}
			AsyncImagePublicError(c, 400, "provider_unavailable", message)
			c.Abort()
			return
		}
		selectedIndex := service.PickMediaChannelByWeight(weights, rand.Float64())
		if selectedIndex < 0 || selectedIndex >= len(candidates) {
			c.AbortWithStatus(503)
			return
		}
		selected := candidates[selectedIndex]
		route.Group = candidateGroups[selectedIndex]
		policy.Group = route.Group
		route.BindingKey = service.MediaPoolBindingKey(policy, publicModel, route.Resolution, "video")
		pinMixedVideoCandidate(c, selected, route, 0)
		c.Set("mixed_video_billing_facts", facts)
		constraints.AddPin(gatewaydto.ChannelPin{ChannelId: selected.Channel.Id, Source: "media_policy", Rank: 20, RetryMode: gatewaydto.PinRetrySingleAttempt})
		c.Next()
	}
}

func pinMixedVideoCandidate(c *gin.Context, candidate service.MixedVideoCandidate, route *service.MixedVideoRouting, generation int64) {
	binding := candidate.Binding
	routing := jsplugin.DefaultRegistry.Generation()
	c.Set(jsplugin.ContextKeyPinnedEndpoint, jsplugin.PinnedEndpoint{Generation: routing, Plugin: binding.Plugin, Protocol: binding.Protocol, Operation: binding.Operation, Model: route.Model, MappedModel: candidate.UpstreamModel, Candidates: []jsplugin.ProtocolBinding{binding}})
	c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{Generation: routing, Plugin: binding.Plugin})
	c.Set("async_media_mixed_routing", route)
	c.Set("async_media_mixed_video", true)
	c.Set("mixed_video_billing", true)
	c.Set("async_media_current_attempt", service.MixedVideoAttempt(candidate, generation))
	common.SetContextKey(c, constant.ContextKeyUsingGroup, route.Group)
	common.SetContextKey(c, constant.ContextKeyAutoGroup, route.Group)
}

// A worker revalidates the exact frozen channel. It never reruns admission's
// random picker or lets a newly configured pool take over an old task.
func pinAsyncVideoWorker(c *gin.Context, job model.AsyncMediaJob, request service.AsyncVideoRequest) {
	if request.MixedRouting == nil {
		middleware.PinTaskPluginEndpoint()(c)
		return
	}
	var token model.Token
	if err := model.DB.WithContext(c.Request.Context()).Where("id = ? AND user_id = ?", job.TokenId, job.UserId).Take(&token).Error; err != nil {
		c.AbortWithStatus(503)
		return
	}
	policy, catalog, err := service.MixedVideoPolicy(c.Request.Context(), token, request.MixedRouting)
	if err != nil {
		AsyncImagePublicError(c, 403, "provider_unavailable", err.Error())
		c.Abort()
		return
	}
	capability, _ := service.MixedVideoPolicyCapability(catalog, job.Model)
	channels, err := service.MixedVideoPoolChannels(c.Request.Context(), policy, job.Model, request.MixedRouting.Resolution)
	if err != nil {
		c.AbortWithStatus(503)
		return
	}
	var selected *model.Channel
	for index := range channels {
		if channels[index].Id == job.ChannelId {
			selected = &channels[index]
			break
		}
	}
	if selected == nil {
		AsyncImagePublicError(c, 403, "provider_unavailable", "Mixed video channel permission changed")
		c.Abort()
		return
	}
	keyIndex := slices.Index(selected.GetKeys(), request.Channel.Key)
	if keyIndex < 0 || selected.ChannelInfo.MultiKeyStatusList[keyIndex] != 0 && selected.ChannelInfo.MultiKeyStatusList[keyIndex] != common.ChannelStatusEnabled {
		AsyncImagePublicError(c, 403, "provider_unavailable", "Mixed video account permission changed")
		c.Abort()
		return
	}
	candidate, err := service.DecodeMixedVideoCandidate(c.Request.Context(), *selected, job.Model, request.Operation, request.Body, capability.VideoOverrides, job.UserId, request.ContentType)
	if err == nil {
		frozenModel, mappingErr := service.ResolveImageChannelUpstreamModel(request.Channel, job.Model)
		if mappingErr != nil || candidate.UpstreamModel != frozenModel {
			err = errors.New("mixed video model mapping changed before dispatch")
		}
	}
	if err == nil && (candidate.Binding.Plugin.Meta.Key != job.Provider || candidate.Binding.Plugin.Engine.SourceFingerprint() != request.PluginHash) {
		err = errors.New("mixed video provider changed")
	}
	if err != nil {
		AsyncImagePublicError(c, 400, "provider_unavailable", err.Error())
		c.Abort()
		return
	}
	pinMixedVideoCandidate(c, candidate, request.MixedRouting, job.AttemptGeneration)
	c.Next()
}
