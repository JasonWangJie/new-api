package service

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/model"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

const MixedVideoResolutionMappingContextKey = "mixed_video_resolution_mapping"

// MixedVideoResolutionMapping freezes the execution identity of one mapped
// mixed-video attempt. Resolution is the exact public tier, not the pool tier.
type MixedVideoResolutionMapping struct {
	ChannelID           int    `json:"channel_id"`
	Resolution          string `json:"resolution"`
	UpstreamModel       string `json:"upstream_model"`
	channelSettingsHash string
}

func newMixedVideoResolutionMapping(channel model.Channel, tier, upstream string) *MixedVideoResolutionMapping {
	return &MixedVideoResolutionMapping{ChannelID: channel.Id, Resolution: tier, UpstreamModel: upstream, channelSettingsHash: videoMappingChannelSettingsHash(channel)}
}

func (mapping MixedVideoResolutionMapping) MatchesChannel(channel model.Channel) bool {
	return channel.Id == mapping.ChannelID && mapping.channelSettingsHash == videoMappingChannelSettingsHash(channel)
}

func videoMappingChannelSettingsHash(channel model.Channel) string {
	setting, headers := "", ""
	if channel.Setting != nil {
		setting = *channel.Setting
	}
	if channel.HeaderOverride != nil {
		headers = *channel.HeaderOverride
	}
	return ImageIdentityHash(channel.GetBaseURL(), setting, headers, channel.OtherSettings)
}

func PinnedMixedVideoResolutionMapping(c *gin.Context) *MixedVideoResolutionMapping {
	value, _ := c.Get(MixedVideoResolutionMappingContextKey)
	mapping, _ := value.(*MixedVideoResolutionMapping)
	return mapping
}

// MixedVideoResolutionChoices enumerates bindings for capability discovery.
// The empty tier represents a regular, resolution-independent model mapping.
func MixedVideoResolutionChoices(channel model.Channel, modelName, operation string) []string {
	if operation == "" || operation == "create" {
		if targets := channel.GetSetting().VideoResolutionModelMapping[modelName]; len(targets) > 0 {
			return slices.DeleteFunc(kitdto.VideoResolutionMappingTiers(), func(tier string) bool { return targets[tier] == "" })
		}
	}
	return []string{""}
}

// MixedVideoValidationDimensions projects an H3 decoder's native 768p onto the
// equivalent public tier for host validation. The plugin still sees its own
// decoded request and retains its existing provider-specific usage facts.
func MixedVideoValidationDimensions(body map[string]any, plugin, upstream string) map[string]any {
	if !model.IsMiniMaxH3VideoModel(plugin, upstream) {
		return body
	}
	project := func(source map[string]any) map[string]any {
		var projected map[string]any
		for _, key := range []string{"resolution", "size"} {
			if value, ok := source[key].(string); ok && strings.EqualFold(strings.TrimSpace(value), "768p") {
				if projected == nil {
					projected = maps.Clone(source)
				}
				projected[key] = "720p"
			}
		}
		return projected
	}
	projected := project(body)
	if metadata, ok := body["metadata"].(map[string]any); ok {
		if projectedMetadata := project(metadata); projectedMetadata != nil {
			if projected == nil {
				projected = maps.Clone(body)
			}
			projected["metadata"] = projectedMetadata
		}
	}
	if projected != nil {
		return projected
	}
	return body
}

func (request AsyncVideoRequest) decodeMixedVideoRetryCandidate(ctx context.Context, channel model.Channel, publicModel string, raw []byte, contentType string, overrides VideoModelOverrides, userID int) (MixedVideoCandidate, error) {
	sameMapping := request.ResolutionMapping != nil && request.Channel.Id == channel.Id
	if sameMapping {
		if !sameVideoMappingOrigin(channel, request.Channel) {
			return MixedVideoCandidate{}, errors.New("mixed video retry channel origin changed")
		}
		// Keep the accepted configuration and the retry policy's live account.
		frozen := request.Channel
		frozen.Key, frozen.Keys, frozen.ChannelInfo = channel.Key, channel.Keys, channel.ChannelInfo
		channel = frozen
	}
	candidate, err := DecodeMixedVideoCandidate(ctx, channel, publicModel, request.Operation, raw, overrides, userID, contentType)
	if err == nil && sameMapping && (!request.ValidateMixedVideoMapping(candidate) || candidate.Binding.Plugin.Engine.SourceFingerprint() != request.PluginHash) {
		return MixedVideoCandidate{}, errors.New("mixed video retry mapping or plugin changed")
	}
	return candidate, err
}

func sameVideoMappingOrigin(live, frozen model.Channel) bool {
	return live.Type == frozen.Type && live.GetBaseURL() == frozen.GetBaseURL() &&
		live.GetSetting().TaskPluginKey == frozen.GetSetting().TaskPluginKey
}

func (request AsyncVideoRequest) frozenVideoMappingChannel() *model.Channel {
	if request.ResolutionMapping == nil {
		return nil
	}
	return &request.Channel
}

func (request AsyncVideoRequest) ValidateMixedVideoMapping(candidate MixedVideoCandidate) bool {
	if request.ResolutionMapping == nil {
		return candidate.ResolutionMapping == nil
	}
	selected := candidate.ResolutionMapping
	return selected != nil && request.ResolutionMapping.ChannelID == selected.ChannelID &&
		request.ResolutionMapping.Resolution == selected.Resolution && request.ResolutionMapping.UpstreamModel == selected.UpstreamModel
}

func publicH3VideoResolution(resolution string) string {
	if strings.EqualFold(resolution, "768p") {
		return "720p"
	}
	return resolution
}

// MixedVideoMappingTier uses the same normalization as public price facts.
// A client-facing model with a mapping requires an explicit resolution.
func mixedVideoMappingTier(body map[string]any) string {
	metadata, _ := body["metadata"].(map[string]any)
	top, _ := body["resolution"].(string)
	nested, _ := metadata["resolution"].(string)
	return NormalizeVideoResolution(cmp.Or(top, nested))
}

func MixedVideoRequiresExplicitResolution(ctx context.Context, policy model.ImageGroupPolicy, groups []string, publicModel string, body map[string]any) (bool, error) {
	if mixedVideoMappingTier(body) != "" {
		return false, nil
	}
	for _, group := range groups {
		policy.Group = group
		tiers := []string{"1K", "2K", "4K"}
		if policy.PoolMode == "model" {
			tiers = tiers[:1]
		}
		for _, tier := range tiers {
			channels, err := MixedVideoPoolChannels(ctx, policy, publicModel, tier)
			if err != nil {
				return false, err
			}
			for _, channel := range channels {
				if len(channel.GetSetting().VideoResolutionModelMapping[publicModel]) > 0 {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
