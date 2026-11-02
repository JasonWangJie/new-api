package service

import (
	"cmp"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// SelectSyncImageChannel applies the configured image pool before the ordinary
// synchronous relay bills or submits. It does not enqueue or retry requests.
func SelectSyncImageChannel(c *gin.Context, modelName string, selected *model.Channel) (*model.Channel, *types.NewAPIError) {
	return selectSyncImageChannel(c, modelName, selected, 0)
}

// SelectSyncImageChannelAfterFallbackFailure selects an ordinary candidate when
// the preferred fallback cannot be prepared. The exclusion lasts only for this
// selection and does not create or consume an upstream retry.
func SelectSyncImageChannelAfterFallbackFailure(c *gin.Context, modelName string, failed *model.Channel) (*model.Channel, *types.NewAPIError) {
	if c == nil || failed == nil {
		return nil, nil
	}
	preferred, exists := c.Get("sync_image_preferred_fallback_channel")
	if !exists || preferred != failed {
		return nil, nil
	}
	return selectSyncImageChannel(c, modelName, nil, failed.Id)
}

func selectSyncImageChannel(c *gin.Context, modelName string, selected *model.Channel, excludedChannelID int) (*model.Channel, *types.NewAPIError) {
	if c == nil || c.Request == nil {
		return selected, nil
	}
	path := c.Request.URL.Path
	if c.Request.Method != http.MethodPost || path != "/v1/images/generations" && path != "/v1/images/edits" {
		return selected, nil
	}
	group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	if autoGroup := common.GetContextKeyString(c, constant.ContextKeyAutoGroup); autoGroup != "" {
		group = autoGroup
	}
	if group == "auto" {
		return selected, nil // The ordinary selector must first authorize a concrete group.
	}
	if routed, exists := c.Get("sync_image_selected_channel"); selected != nil && exists && routed == selected {
		return selected, nil
	}
	token := model.Token{Id: c.GetInt("token_id"), UserId: c.GetInt("id"), Group: group}
	policy, catalog, err := ResolveImagePolicy(c.Request.Context(), token, "openai")
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if policy.Id == 0 {
		return selected, nil
	}
	if !policy.Enabled || !IsUserSelectableGroup(common.GetContextKeyString(c, constant.ContextKeyUserGroup), policy.Group) {
		return nil, types.NewErrorWithStatusCode(errors.New("image platform group is unavailable"), types.ErrorCodeGetChannelFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	var input struct {
		Size       string `json:"size"`
		Resolution string `json:"resolution"`
	}
	if err := common.UnmarshalBodyReusable(c, &input); err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	request := AsyncImageRequest{Model: modelName, Platform: "openai", Size: input.Size, Resolution: input.Resolution, Kind: "text_to_image"}
	if strings.HasSuffix(path, "/edits") {
		request.Kind = "image_to_image"
	}
	index := slices.IndexFunc(catalog, func(capability ImageModelCapability) bool { return capability.MatchesMedia(modelName, "image") })
	if index < 0 {
		return nil, types.NewErrorWithStatusCode(errors.New("image model is not available in this platform catalog"), types.ErrorCodeModelNotFound, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	resolution := AsyncImageRoutingResolution(request)
	capability := catalog[index]
	if resolution == "0.5K" && !capability.AllowHalfK || resolution != "" && resolution != "AUTO" && !slices.Contains(capability.Resolutions, resolution) && !(resolution == "0.5K" && capability.AllowHalfK) {
		return nil, types.NewErrorWithStatusCode(errors.New("unsupported_image_dimensions: resolution is not supported by this model"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	channels, err := ImageCandidateChannels(c.Request.Context(), policy, modelName, resolution)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	constraints := GetChannelConstraints(c)
	pin, pinned, _ := constraints.ResolvedPin()
	eligible := make([]model.Channel, 0, len(channels))
	for _, channel := range channels {
		if channel.Id == excludedChannelID {
			continue
		}
		// Resolution mappings also apply when this group's async service is off.
		if !policy.AsyncEnabled {
			var available bool
			channel, available, err = imageChannelForResolution(channel, modelName, resolution)
			if err != nil {
				return nil, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
			}
			if !available {
				continue
			}
		}
		if pinned && pin.ChannelId != channel.Id || !ImageCapabilitySupportsRequest(ImageChannelCapability(channel, modelName), request) || len(ImageChannelAccounts(channel, ImageAccountRouting{})) == 0 || !SynchronousImageChannelSupported(channel, modelName, path) {
			continue
		}
		if allowed, _ := model.ChannelSatisfiesFilters(&channel, modelName, constraints.Filters); allowed {
			eligible = append(eligible, channel)
		}
	}
	if len(eligible) == 0 {
		return nil, types.NewErrorWithStatusCode(errors.New("no eligible synchronous image channel in the selected pool"), types.ErrorCodeGetChannelFailed, http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
	}
	chosen := -1
	if used := c.GetStringSlice("use_channel"); len(used) > 0 && !pinned {
		fallbackID, err := GetMediaPoolFallbackChannelID(c.Request.Context(), ImagePoolBindingKey(policy, modelName, resolution))
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
		}
		if fallbackID > 0 && strconv.Itoa(fallbackID) != used[len(used)-1] {
			chosen = slices.IndexFunc(eligible, func(channel model.Channel) bool { return channel.Id == fallbackID })
		}
	}
	preferredFallback := chosen >= 0
	if value, exists := c.Get(ginKeyChannelAffinityLogInfo); chosen < 0 && exists && selected != nil {
		if info, ok := value.(map[string]any); ok && info["channel_id"] == selected.Id {
			chosen = slices.IndexFunc(eligible, func(channel model.Channel) bool { return channel.Id == selected.Id })
		}
	}
	if chosen < 0 {
		slices.SortFunc(eligible, func(a, b model.Channel) int { return cmp.Compare(b.GetPriority(), a.GetPriority()) })
		priority := eligible[0].GetPriority()
		eligible = slices.DeleteFunc(eligible, func(channel model.Channel) bool { return channel.GetPriority() != priority })
		weights := make([]int, len(eligible))
		for i := range eligible {
			weights[i] = eligible[i].GetWeight()
		}
		chosen = PickMediaChannelByWeight(weights, rand.Float64())
	}
	if chosen < 0 {
		return nil, types.NewError(errors.New("image channel selection failed"), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	common.SetContextKey(c, constant.ContextKeyUsingGroup, policy.Group)
	common.SetContextKey(c, constant.ContextKeyAutoGroup, policy.Group)
	selected = &eligible[chosen]
	c.Set("sync_image_selected_channel", selected)
	var fallback *model.Channel
	if preferredFallback {
		fallback = selected
	}
	c.Set("sync_image_preferred_fallback_channel", fallback)
	return selected, nil
}
