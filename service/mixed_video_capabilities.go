package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
)

var errNoCompatibleMixedVideoChannel = errors.New("No compatible video channel is available for the mixed model")

// ValidateMixedVideoOverridesStatic validates a draft without requiring a live
// provider. Enabling the policy additionally checks the current channel profiles.
func ValidateMixedVideoOverridesStatic(overrides VideoModelOverrides) error {
	for name, limit := range overrides.MaxInputItems {
		if name == "" || name != strings.TrimSpace(name) || limit < 1 || limit > 64 {
			return fmt.Errorf("invalid mixed video input limit for %s", name)
		}
	}
	if duration := overrides.Duration; duration != nil {
		for _, bound := range []*int{duration.Min, duration.Max, duration.Default} {
			if bound != nil && (*bound < 1 || *bound > relaycommon.MaxTaskDurationSeconds) {
				return errors.New("invalid mixed video duration bound")
			}
		}
		if duration.Min != nil && duration.Max != nil && *duration.Min > *duration.Max {
			return errors.New("invalid mixed video duration range")
		}
		seen := make(map[int]bool, len(duration.Values))
		for _, value := range duration.Values {
			if value < 1 || value > relaycommon.MaxTaskDurationSeconds || duration.Min != nil && value < *duration.Min || duration.Max != nil && value > *duration.Max || seen[value] {
				return errors.New("invalid or duplicate mixed video duration choice")
			}
			seen[value] = true
		}
		if duration.Default != nil && (duration.Min != nil && *duration.Default < *duration.Min || duration.Max != nil && *duration.Default > *duration.Max || duration.Values != nil && !slices.Contains(duration.Values, *duration.Default)) {
			return errors.New("mixed video default duration is outside the allowed choices")
		}
	}
	for _, dimension := range []struct {
		name     string
		values   []string
		fallback string
	}{
		{"mode", overrides.Modes, ""},
		{"resolution", overrides.Resolutions, overrides.DefaultResolution},
		{"aspect_ratio", overrides.AspectRatios, overrides.DefaultAspectRatio},
		{"extension_direction", overrides.ExtensionDirections, overrides.DefaultExtensionDirection},
	} {
		seen := make(map[string]bool, len(dimension.values))
		for _, value := range dimension.values {
			if value == "" || value != strings.TrimSpace(value) || seen[value] {
				return fmt.Errorf("invalid or duplicate mixed video %s", dimension.name)
			}
			if dimension.name == "mode" && !slices.Contains([]string{"text_to_video", "image_to_video", "reference_to_video", "first_last_frame", "edit_video", "extend_video"}, value) || dimension.name == "extension_direction" && value != "forward" && value != "backward" {
				return fmt.Errorf("unknown mixed video %s %q", dimension.name, value)
			}
			seen[value] = true
		}
		if dimension.fallback != "" && (dimension.fallback != strings.TrimSpace(dimension.fallback) || dimension.values != nil && !slices.Contains(dimension.values, dimension.fallback)) {
			return fmt.Errorf("mixed video default %s is outside the allowed choices", dimension.name)
		}
	}
	if overrides.DefaultExtensionDirection != "" && overrides.DefaultExtensionDirection != "forward" && overrides.DefaultExtensionDirection != "backward" {
		return errors.New("unknown mixed video default extension direction")
	}
	return nil
}

// MixedVideoModelProfiles preserves complete provider variants, including the
// relationships between duration, resolution and inputs. Pool filtering is used
// for public capabilities, while configuration discovery also permits new pools.
func MixedVideoModelProfiles(ctx context.Context, policy model.ImageGroupPolicy, publicModel string, overrides VideoModelOverrides, requirePool bool) ([]jsplugin.VideoProfile, error) {
	return mixedVideoModelProfilesDB(ctx, model.DB, policy, publicModel, overrides, requirePool)
}

func mixedVideoModelProfilesDB(ctx context.Context, db *gorm.DB, policy model.ImageGroupPolicy, publicModel string, overrides VideoModelOverrides, requirePool bool) ([]jsplugin.VideoProfile, error) {
	if err := ValidateMixedVideoOverridesStatic(overrides); err != nil {
		return nil, err
	}
	group := policy.Group
	var abilities []model.Ability
	query := db.WithContext(ctx).Where("enabled = ?", true).Where("model IN ?", []string{publicModel, ratio_setting.RoutingMatchModelName(publicModel)})
	if group != "auto" || requirePool {
		query = query.Where(map[string]any{"group": group})
	}
	if err := query.Find(&abilities).Error; err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(abilities))
	for _, ability := range abilities {
		ids = append(ids, ability.ChannelId)
	}
	var channels []model.Channel
	if len(ids) > 0 {
		if err := db.WithContext(ctx).Where("id IN ? AND status = ?", ids, common.ChannelStatusEnabled).Order("id").Find(&channels).Error; err != nil {
			return nil, err
		}
	}
	var pools []model.ImageChannelPool
	if requirePool {
		if err := db.WithContext(ctx).Where(map[string]any{"group": group, "platform": "mixed"}).Where("channel_id > ?", 0).Find(&pools).Error; err != nil {
			return nil, err
		}
	}
	profiles := make([]jsplugin.VideoProfile, 0)
	seen := make(map[string]bool)
	for _, channel := range channels {
		if requirePool && len(ImageChannelAccounts(channel, ImageAccountRouting{})) == 0 {
			continue
		}
		for _, operation := range []string{"create", "edit", "extend"} {
			for _, tier := range MixedVideoResolutionChoices(channel, publicModel, operation) {
				binding, upstream, err := MixedVideoChannelBinding(channel, publicModel, operation, tier)
				if err != nil {
					continue
				}
				profile, err := EffectiveMixedVideoExecutionProfile(binding, publicModel, upstream, tier, overrides)
				if err != nil {
					continue
				}
				if requirePool {
					modes := make([]jsplugin.VideoModeProfile, 0, len(profile.Modes))
					for _, mode := range profile.Modes {
						portable := true
						inputs := make([]jsplugin.VideoInputProfile, 0, len(mode.Inputs))
						for _, input := range mode.Inputs {
							input.Sources = slices.DeleteFunc(input.Sources, func(source string) bool {
								return source == "asset" || source == "file_id" || source == "voice_id"
							})
							if len(input.Sources) == 0 {
								portable = portable && !input.Required
								continue
							}
							inputs = append(inputs, input)
						}
						if !portable {
							continue
						}
						mode.Inputs = inputs
						resolutions := mode.Resolutions
						if len(resolutions) == 0 {
							resolutions = []string{""}
						}
						allowed := make([]string, 0, len(resolutions))
						for _, resolution := range resolutions {
							key := MediaPoolBindingKey(policy, publicModel, VideoPoolResolution(resolution), "video")
							if slices.ContainsFunc(pools, func(pool model.ImageChannelPool) bool { return pool.BindingKey == key && pool.ChannelId == channel.Id }) {
								allowed = append(allowed, resolution)
							}
						}
						if len(allowed) == 0 {
							continue
						}
						if len(mode.Resolutions) > 0 {
							mode.Resolutions = allowed
							if !slices.Contains(allowed, mode.DefaultResolution) {
								mode.DefaultResolution = allowed[0]
							}
						}
						modes = append(modes, mode)
					}
					profile.Modes = modes
				}
				if len(profile.Modes) == 0 {
					continue
				}
				encoded, err := common.Marshal(profile)
				if err != nil {
					return nil, err
				}
				if !seen[string(encoded)] {
					seen[string(encoded)] = true
					profiles = append(profiles, profile)
				}
			}
		}
	}
	if len(profiles) == 0 {
		return profiles, errNoCompatibleMixedVideoChannel
	}
	if !requirePool {
		for dimension, value := range map[string]string{"resolution": overrides.DefaultResolution, "aspect_ratio": overrides.DefaultAspectRatio, "extension_direction": overrides.DefaultExtensionDirection} {
			if value == "" {
				continue
			}
			found := false
			for _, profile := range profiles {
				for _, mode := range profile.Modes {
					switch dimension {
					case "resolution":
						found = found || mode.DefaultResolution == value
					case "aspect_ratio":
						found = found || mode.DefaultAspectRatio == value
					case "extension_direction":
						found = found || mode.DefaultExtensionDirection == value
					}
				}
			}
			if !found {
				return nil, fmt.Errorf("mixed video default %s has no supporting channel", dimension)
			}
		}
		for dimension, values := range map[string][]string{"mode": overrides.Modes, "resolution": overrides.Resolutions, "aspect_ratio": overrides.AspectRatios, "extension_direction": overrides.ExtensionDirections} {
			seenValues := make(map[string]bool)
			for _, value := range values {
				if seenValues[value] {
					return nil, fmt.Errorf("duplicate mixed video %s", dimension)
				}
				seenValues[value] = true
				found := false
				for _, profile := range profiles {
					for _, mode := range profile.Modes {
						switch dimension {
						case "mode":
							found = found || mode.Name == value
						case "resolution":
							found = found || slices.Contains(mode.Resolutions, value)
						case "aspect_ratio":
							found = found || slices.Contains(mode.AspectRatios, value)
						case "extension_direction":
							found = found || slices.Contains(mode.ExtensionDirections, value)
						}
					}
				}
				if !found {
					return nil, fmt.Errorf("mixed video %s %q has no supporting channel", dimension, value)
				}
			}
		}
	}
	return profiles, nil
}
