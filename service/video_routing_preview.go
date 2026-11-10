package service

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
)

type VideoRoutingPreview struct {
	MatchedRules        []VideoRoutingRuleMatch     `json:"matched_rules"`
	RuleID              string                      `json:"rule_id"`
	RuleName            string                      `json:"rule_name"`
	Source              map[string]any              `json:"source"`
	Targets             []VideoRoutingPreviewTarget `json:"targets"`
	OnExhausted         string                      `json:"on_exhausted"`
	SelectedTargetIndex *int                        `json:"selected_target_index,omitempty"`
	IsFallback          bool                        `json:"is_fallback"`
}

type VideoRoutingRuleMatch struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type VideoRoutingPreviewTarget struct {
	ChannelID     int      `json:"channel_id"`
	Model         string   `json:"model"`
	UpstreamModel string   `json:"upstream_model"`
	Resolution    string   `json:"resolution"`
	Eligible      bool     `json:"eligible"`
	Reasons       []string `json:"reasons"`
}

// videoRoutingPreviewProfileForInputs checks known media kinds before removing
// content requirements that a parameter-only preview cannot verify.
func videoRoutingPreviewProfileForInputs(profile jsplugin.VideoProfile, mode, videoInput string) (jsplugin.VideoProfile, error) {
	index := slices.IndexFunc(profile.Modes, func(candidate jsplugin.VideoModeProfile) bool { return candidate.Name == mode })
	if index < 0 {
		return jsplugin.VideoProfile{}, fmt.Errorf("video mode %q is not enabled", mode)
	}
	selected := profile.Modes[index]
	if videoInput == "video" && !slices.ContainsFunc(selected.Inputs, func(input jsplugin.VideoInputProfile) bool {
		return input.Kind == "video" && input.MaxItems > 0 && len(input.Sources) > 0
	}) {
		return jsplugin.VideoProfile{}, errors.New("video reference input is not supported by the selected mode")
	}
	if videoInput == "none" && slices.ContainsFunc(selected.Inputs, func(input jsplugin.VideoInputProfile) bool {
		return input.Kind == "video" && input.Required
	}) {
		return jsplugin.VideoProfile{}, errors.New("video reference input is required by the selected mode")
	}
	selected.Inputs = nil
	profile.Modes = []jsplugin.VideoModeProfile{selected}
	return profile, nil
}

// PreviewConditionalVideoRouting shares the matcher and the request rewriter.
// It validates dimensions only: material contents are deliberately unavailable.
func PreviewConditionalVideoRouting(ctx context.Context, input MediaRoutingPreviewRequest, preview *MediaRoutingPreview, groups []string, parameters map[string]any) (bool, error) {
	if input.ExecutionMode == "sync" || input.Platform != "mixed" || input.MediaType != "video" || input.Provider != "" || input.Operation == "edit_video" || input.Operation == "extend_video" || parameters == nil {
		return false, nil
	}
	config, err := model.GetVideoRoutingConfig(ctx)
	if input.VideoRoutingConfig != nil {
		config, err = *input.VideoRoutingConfig, nil
	}
	if err != nil {
		return false, err
	}
	if !config.Enabled {
		return false, nil
	}
	if input.VideoRoutingConfig != nil {
		structural := config
		structural.Rules = slices.Clone(config.Rules)
		for i := range structural.Rules {
			structural.Rules[i].Enabled = false
		}
		if err := ValidateVideoRoutingConfig(ctx, structural); err != nil {
			return false, err
		}
	}
	facts, err := MixedVideoPublicFacts(parameters, "create", nil)
	if err != nil {
		return false, err
	}
	if input.VideoInput != "" {
		facts["video_input"] = input.VideoInput
	}
	mode := input.Operation
	if input.Mode != "" {
		mode = input.Mode
	}
	// Admission only matches groups whose original public pool accepts the
	// source request. Aggregated profiles from another automatic group do not
	// grant that authority to a group containing only a conditional target.
	token := model.Token{Group: input.Group}
	if input.TokenID > 0 {
		if err := model.DB.WithContext(ctx).First(&token, input.TokenID).Error; err != nil {
			return false, err
		}
	}
	policy, catalog, err := ResolveImagePolicy(ctx, token, "mixed")
	if err != nil {
		return false, err
	}
	capability, declared := MixedVideoPolicyCapability(catalog, input.Model)
	if !declared {
		return false, nil
	}
	sourceGroups := make([]string, 0, len(groups))
	for _, group := range groups {
		poolPolicy := policy
		poolPolicy.Group = group
		profiles, err := MixedVideoModelProfiles(ctx, poolPolicy, input.Model, capability.VideoOverrides, true)
		if errors.Is(err, errNoCompatibleMixedVideoChannel) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, profile := range profiles {
			profile, err := videoRoutingPreviewProfileForInputs(profile, mode, input.VideoInput)
			if err != nil {
				continue
			}
			if ValidateVideoProfileRequest(profile, mode, parameters, nil) == nil {
				sourceGroups = append(sourceGroups, group)
				break
			}
		}
	}
	result := &VideoRoutingPreview{MatchedRules: []VideoRoutingRuleMatch{}, Source: map[string]any{"model": input.Model, "resolution": facts["resolution"], "seconds": facts["seconds"]}, Targets: []VideoRoutingPreviewTarget{}}
	var matched *model.VideoRoutingRule
	selectedGroup := ""
	for _, rule := range config.Rules {
		one := config
		one.Rules = []model.VideoRoutingRule{rule}
		for _, group := range sourceGroups {
			if len(MatchVideoRoutingRules(one, group, input.Model, mode, facts)) == 0 {
				continue
			}
			result.MatchedRules = append(result.MatchedRules, VideoRoutingRuleMatch{ID: rule.ID, Name: rule.Name})
			if matched == nil {
				copy := rule
				matched, selectedGroup = &copy, group
			}
			break
		}
	}
	if matched == nil {
		return false, nil
	}
	result.RuleID, result.RuleName, result.OnExhausted = matched.ID, matched.Name, matched.OnExhausted
	preview.ConditionalRouting = result
	preview.Warnings = append(preview.Warnings, "conditional_preview_dimensions_only_materials_not_validated")
	if len(result.MatchedRules) > 1 {
		preview.Warnings = append(preview.Warnings, "conditional_rules_overlap_first_match_wins")
	}
	source := make(map[string]any, len(parameters)+1)
	for key, value := range parameters {
		source[key] = value
	}
	source["model"] = input.Model
	raw, err := common.Marshal(source)
	if err != nil {
		return false, err
	}
	for index, target := range matched.Targets {
		entry := VideoRoutingPreviewTarget{ChannelID: target.ChannelID, Model: target.Model, Resolution: target.Resolution, Reasons: slices.Clone(preview.Reasons)}
		if entry.Resolution == "" {
			entry.Resolution, _ = facts["resolution"].(string)
		}
		var channel model.Channel
		if err := model.DB.WithContext(ctx).First(&channel, target.ChannelID).Error; err != nil {
			entry.Reasons = append(entry.Reasons, "target_channel_unavailable")
		} else {
			if channel.Status != common.ChannelStatusEnabled {
				entry.Reasons = append(entry.Reasons, "channel_disabled")
			}
			var ability model.Ability
			abilityErr := model.DB.WithContext(ctx).Where(map[string]any{"group": selectedGroup, "model": target.Model, "channel_id": channel.Id, "enabled": true}).Take(&ability).Error
			if !slices.Contains(channel.GetGroups(), selectedGroup) || !slices.Contains(channel.GetModels(), target.Model) || abilityErr != nil {
				entry.Reasons = append(entry.Reasons, "target_model_or_group_unavailable")
			}
			if len(ImageChannelAccounts(channel, ImageAccountRouting{})) == 0 {
				entry.Reasons = append(entry.Reasons, "no_available_accounts")
			}
			tier := ""
			if choices := MixedVideoResolutionChoices(channel, target.Model, "create"); len(choices) > 0 && choices[0] != "" {
				tier = entry.Resolution
			}
			binding, upstream, bindErr := MixedVideoChannelBinding(channel, target.Model, "create", tier)
			entry.UpstreamModel = upstream
			if bindErr != nil {
				entry.Reasons = append(entry.Reasons, SanitizeAsyncImageDiagnostic(bindErr.Error(), 512))
			} else {
				profile, validationErr := EffectiveMixedVideoExecutionProfile(binding, target.Model, upstream, tier, VideoModelOverrides{})
				if validationErr == nil {
					profile, validationErr = videoRoutingPreviewProfileForInputs(profile, mode, input.VideoInput)
				}
				if validationErr == nil {
					var rewritten []byte
					var ct string
					rewritten, ct, validationErr = RewriteVideoRoutingRequest(raw, "application/json", "create", target.Model, target.Resolution)
					if validationErr == nil {
						_, body, parseErr := MixedVideoRequestContext(rewritten, ct, "create")
						validationErr = parseErr
						if validationErr == nil {
							validationErr = ValidateVideoProfileRequest(profile, mode, body, nil)
						}
					}
				}
				if validationErr != nil {
					entry.Reasons = append(entry.Reasons, SanitizeAsyncImageDiagnostic(validationErr.Error(), 512))
				}
				if len(entry.Reasons) == 0 && result.SelectedTargetIndex == nil {
					result.SelectedTargetIndex = common.GetPointer(index)
					preview.Eligible = []MediaRoutingPreviewChannel{{Provider: binding.Plugin.Meta.Key, Protocol: binding.Protocol, ChannelID: channel.Id, Group: selectedGroup, Name: channel.Name, UpstreamModel: upstream, AvailableAccounts: len(ImageChannelAccounts(channel, ImageAccountRouting{})), Reasons: []string{}}}
				}
			}
		}
		entry.Eligible = len(entry.Reasons) == 0
		result.Targets = append(result.Targets, entry)
	}
	result.IsFallback = result.SelectedTargetIndex == nil && matched.OnExhausted == "original"
	if result.IsFallback {
		preview.RoutingGroups = []string{selectedGroup}
	}
	return !result.IsFallback, nil
}
