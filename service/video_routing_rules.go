package service

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// ValidateVideoRoutingConfig validates disabled drafts structurally, but only
// enabled rules in an enabled configuration depend on live groups, channels
// and plugin capabilities.
func ValidateVideoRoutingConfig(ctx context.Context, config model.VideoRoutingConfig) error {
	if config.Version != 1 {
		return fmt.Errorf("Unsupported video routing configuration version")
	}
	if config.Revision < 0 || config.Revision >= model.MaxOptionRevision {
		return model.ErrOptionRevisionInvalid
	}
	if len(config.Rules) > 100 {
		return fmt.Errorf("Video routing supports at most 100 rules")
	}
	ids := make(map[string]bool, len(config.Rules))
	channels := make(map[int]model.Channel)
	for _, rule := range config.Rules {
		live := config.Enabled && rule.Enabled
		if rule.ID == "" || len(rule.ID) > 64 || strings.TrimSpace(rule.ID) != rule.ID || strings.ContainsFunc(rule.ID, unicode.IsControl) || ids[rule.ID] {
			return fmt.Errorf("Video routing rule IDs must be unique, nonempty and at most 64 bytes")
		}
		ids[rule.ID] = true
		if strings.TrimSpace(rule.Name) == "" || utf8.RuneCountInString(rule.Name) > 128 || strings.ContainsFunc(rule.Name, unicode.IsControl) {
			return fmt.Errorf("Rule %q requires a name of at most 128 characters", rule.ID)
		}
		if rule.OnExhausted != "original" && rule.OnExhausted != "fail" {
			return fmt.Errorf("Rule %q must choose original or fail when targets are exhausted", rule.ID)
		}
		if len(rule.Targets) == 0 || len(rule.Targets) > 20 {
			return fmt.Errorf("Rule %q requires between 1 and 20 targets", rule.ID)
		}
		for key, values := range map[string][]string{"groups": rule.Match.Groups, "models": rule.Match.Models, "resolutions": rule.Match.Resolutions, "modes": rule.Match.Modes, "video_inputs": rule.Match.VideoInputs} {
			if len(values) > 100 {
				return fmt.Errorf("Rule %q has too many %s", rule.ID, key)
			}
			seen := make(map[string]bool, len(values))
			for _, value := range values {
				if value == "" || len(value) > 255 || value != strings.TrimSpace(value) || strings.ContainsFunc(value, unicode.IsControl) || seen[value] {
					return fmt.Errorf("Rule %q has invalid or duplicate %s", rule.ID, key)
				}
				seen[value] = true
				switch key {
				case "groups":
					if live && !ratio_setting.ContainsGroupRatio(value) {
						return fmt.Errorf("Rule %q references unavailable group %q", rule.ID, value)
					}
				case "resolutions":
					if NormalizeVideoResolution(value) != value {
						return fmt.Errorf("Rule %q contains an invalid resolution", rule.ID)
					}
				case "modes":
					if !slices.Contains([]string{"text_to_video", "image_to_video", "reference_to_video", "first_last_frame"}, value) {
						return fmt.Errorf("Rule %q contains an unsupported generation mode", rule.ID)
					}
				case "video_inputs":
					if value != "none" && value != "video" {
						return fmt.Errorf("Rule %q contains an invalid reference video condition", rule.ID)
					}
				}
			}
		}
		seconds := rule.Match.Seconds
		minimum, maximum := 1, relaycommon.MaxTaskDurationSeconds
		if seconds.MinExclusive && seconds.Min == nil || seconds.MaxExclusive && seconds.Max == nil {
			return fmt.Errorf("Rule %q has an exclusive duration bound without a value", rule.ID)
		}
		if seconds.Min != nil {
			if *seconds.Min < 1 || *seconds.Min > relaycommon.MaxTaskDurationSeconds {
				return fmt.Errorf("Rule %q has an invalid minimum duration", rule.ID)
			}
			minimum = *seconds.Min
			if seconds.MinExclusive {
				minimum++
			}
		}
		if seconds.Max != nil {
			if *seconds.Max < 1 || *seconds.Max > relaycommon.MaxTaskDurationSeconds {
				return fmt.Errorf("Rule %q has an invalid maximum duration", rule.ID)
			}
			maximum = *seconds.Max
			if seconds.MaxExclusive {
				maximum--
			}
		}
		if minimum > maximum {
			return fmt.Errorf("Rule %q has an empty duration range", rule.ID)
		}
		targets := make(map[int]bool, len(rule.Targets))
		for _, target := range rule.Targets {
			if target.ChannelID <= 0 || target.Model == "" || len(target.Model) > 255 || strings.TrimSpace(target.Model) != target.Model || strings.ContainsFunc(target.Model, unicode.IsControl) || target.Resolution != "" && NormalizeVideoResolution(target.Resolution) != target.Resolution {
				return fmt.Errorf("Rule %q has an invalid target", rule.ID)
			}
			if targets[target.ChannelID] {
				return fmt.Errorf("Rule %q repeats a target channel; channel retries require distinct channels", rule.ID)
			}
			targets[target.ChannelID] = true
			if !live {
				continue
			}
			channel, found := channels[target.ChannelID]
			if !found {
				if err := model.DB.WithContext(ctx).Omit("key").First(&channel, target.ChannelID).Error; err != nil {
					return fmt.Errorf("Rule %q target channel %d is unavailable: %w", rule.ID, target.ChannelID, err)
				}
				channels[target.ChannelID] = channel
			}
			if channel.Status != common.ChannelStatusEnabled || !slices.Contains(channel.GetModels(), target.Model) {
				return fmt.Errorf("Rule %q target channel %d does not enable model %q", rule.ID, target.ChannelID, target.Model)
			}
			groupAllowed := false
			for _, group := range channel.GetGroups() {
				if ratio_setting.ContainsGroupRatio(group) && (len(rule.Match.Groups) == 0 || slices.Contains(rule.Match.Groups, group)) {
					groupAllowed = true
					break
				}
			}
			if !groupAllowed {
				return fmt.Errorf("Rule %q target channel %d has no matching enabled group", rule.ID, target.ChannelID)
			}
			compatible := false
			tiers := MixedVideoResolutionChoices(channel, target.Model, "create")
			for _, tier := range tiers {
				binding, upstream, err := MixedVideoChannelBinding(channel, target.Model, "create", tier)
				if err != nil {
					if len(tiers) == 1 && tier == "" {
						return fmt.Errorf("Rule %q target channel %d: %w", rule.ID, target.ChannelID, err)
					}
					continue
				}
				profile, err := EffectiveMixedVideoExecutionProfile(binding, target.Model, upstream, tier, VideoModelOverrides{})
				if err == nil && videoRoutingTargetCanMatch(rule.Match, target, profile, minimum, maximum) {
					compatible = true
					break
				}
			}
			if !compatible {
				return fmt.Errorf("Rule %q target channel %d has no compatible mode, resolution and duration", rule.ID, target.ChannelID)
			}
		}
	}
	return nil
}

// videoRoutingTargetCanMatch checks whole capability combinations. Runtime
// decoding still validates the actual references, options and plugin contract.
func videoRoutingTargetCanMatch(match model.VideoRoutingMatch, target model.VideoRoutingTarget, profile jsplugin.VideoProfile, minimum, maximum int) bool {
	for _, mode := range profile.Modes {
		if mode.Name == "edit_video" || mode.Name == "extend_video" || len(match.Modes) > 0 && !slices.Contains(match.Modes, mode.Name) {
			continue
		}
		resolutions := mode.Resolutions
		if len(resolutions) == 0 {
			resolutions = []string{""}
		}
		for _, raw := range resolutions {
			resolution := NormalizeVideoResolution(raw)
			if raw == "720px" {
				resolution = "720p"
			}
			if target.Resolution != "" && resolution != target.Resolution || target.Resolution == "" && len(match.Resolutions) > 0 && !slices.Contains(match.Resolutions, resolution) {
				continue
			}
			duration := mode.DurationForResolution(raw)
			if duration == nil {
				if match.Seconds.Min == nil && match.Seconds.Max == nil {
					return true
				}
				continue
			}
			if len(duration.Values) > 0 {
				if slices.ContainsFunc(duration.Values, func(value int) bool { return value >= minimum && value <= maximum }) {
					return true
				}
				continue
			}
			lower, upper := max(minimum, duration.Min), min(maximum, duration.Max)
			if lower <= upper && duration.Step > 0 {
				offset := (lower - duration.Min) % duration.Step
				if offset == 0 || duration.Step-offset <= upper-lower {
					return true
				}
			}
		}
	}
	return false
}

// MatchVideoRoutingRules preserves configuration order for execution and preview.
// Empty selectors are wildcards; a missing fact never satisfies a constraint.
func MatchVideoRoutingRules(config model.VideoRoutingConfig, group, publicModel, mode string, facts map[string]any) []model.VideoRoutingRule {
	matched := make([]model.VideoRoutingRule, 0)
	if !config.Enabled || config.Version != 1 {
		return matched
	}
	resolution, _ := facts["resolution"].(string)
	videoInput, _ := facts["video_input"].(string)
	seconds, hasSeconds := videoNumber(facts["seconds"])
	for _, rule := range config.Rules {
		if !rule.Enabled || len(rule.Match.Groups) > 0 && !slices.Contains(rule.Match.Groups, group) || len(rule.Match.Models) > 0 && !slices.Contains(rule.Match.Models, publicModel) || len(rule.Match.Modes) > 0 && !slices.Contains(rule.Match.Modes, mode) || len(rule.Match.Resolutions) > 0 && !slices.Contains(rule.Match.Resolutions, resolution) || len(rule.Match.VideoInputs) > 0 && !slices.Contains(rule.Match.VideoInputs, videoInput) {
			continue
		}
		bounds := rule.Match.Seconds
		if bounds.Min != nil || bounds.Max != nil {
			if !hasSeconds || seconds < 1 || seconds > relaycommon.MaxTaskDurationSeconds || seconds != math.Trunc(seconds) {
				continue
			}
			if bounds.Min != nil && (seconds < float64(*bounds.Min) || bounds.MinExclusive && seconds == float64(*bounds.Min)) || bounds.Max != nil && (seconds > float64(*bounds.Max) || bounds.MaxExclusive && seconds == float64(*bounds.Max)) {
				continue
			}
		}
		matched = append(matched, rule)
	}
	return matched
}
