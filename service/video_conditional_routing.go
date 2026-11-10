package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"mime"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	"github.com/gin-gonic/gin"
)

const VideoRoutingContextKey = "async_video_conditional_routing"

func VideoRoutingAppliesToSource(config model.VideoRoutingConfig, publicModel string, groups []string) bool {
	if !config.Enabled {
		return false
	}
	for _, rule := range config.Rules {
		if !rule.Enabled || len(rule.Match.Models) > 0 && !slices.Contains(rule.Match.Models, publicModel) {
			continue
		}
		if len(rule.Match.Groups) == 0 || slices.ContainsFunc(groups, func(group string) bool { return slices.Contains(rule.Match.Groups, group) }) {
			return true
		}
	}
	return false
}

// VideoRoutingSnapshot is server-owned and encrypted with the durable request.
// SourceBody is independent of OriginalBody, which belongs to media localization.
type VideoRoutingSnapshot struct {
	Revision          int64                      `json:"revision"`
	Rule              model.VideoRoutingRule     `json:"rule"`
	SourceBody        []byte                     `json:"source_body"`
	SourceContentType string                     `json:"source_content_type"`
	SourceModel       string                     `json:"source_model"`
	SourceFacts       map[string]any             `json:"source_facts"`
	Targets           []VideoRoutingFrozenTarget `json:"targets"`
	TargetIndex       int                        `json:"target_index"`
	Fallback          bool                       `json:"fallback"`
	PinnedChannelID   int                        `json:"pinned_channel_id,omitempty"`
	Filters           []VideoRoutingFilter       `json:"filters,omitempty"`
}

type VideoRoutingFilter struct {
	Kind                   dto.ChannelFilterKind
	RequestPath            string
	TaskPluginKey          string
	TaskPluginKeys         []string
	TaskPluginChannelTypes []int
}

func (snapshot *VideoRoutingSnapshot) ChannelFilters() []dto.ChannelFilter {
	filters := make([]dto.ChannelFilter, 0, len(snapshot.Filters))
	for _, filter := range snapshot.Filters {
		filters = append(filters, dto.ChannelFilter{Kind: filter.Kind, RequestPath: filter.RequestPath, TaskPluginKey: filter.TaskPluginKey, TaskPluginKeys: filter.TaskPluginKeys, TaskPluginChannelTypes: filter.TaskPluginChannelTypes})
	}
	return filters
}

type VideoRoutingFrozenTarget struct {
	Target        model.VideoRoutingTarget `json:"target"`
	UpstreamModel string                   `json:"upstream_model"`
	PluginKey     string                   `json:"plugin_key"`
	PluginHash    string                   `json:"plugin_hash"`
	Reason        string                   `json:"reason,omitempty"`
}

func (snapshot *VideoRoutingSnapshot) ActiveTarget() *VideoRoutingFrozenTarget {
	if snapshot == nil || snapshot.Fallback || snapshot.TargetIndex < 0 || snapshot.TargetIndex >= len(snapshot.Targets) {
		return nil
	}
	return &snapshot.Targets[snapshot.TargetIndex]
}

func VideoRoutingExecutionModel(c *gin.Context, publicModel string) string {
	value, _ := c.Get(VideoRoutingContextKey)
	snapshot, _ := value.(*VideoRoutingSnapshot)
	if target := snapshot.ActiveTarget(); target != nil && snapshot.SourceModel == publicModel {
		return target.Target.Model
	}
	return publicModel
}

func (request AsyncVideoRequest) ExecutionModel(publicModel string) string {
	if target := request.ConditionalRouting.ActiveTarget(); target != nil {
		return target.Target.Model
	}
	return publicModel
}

// PlanVideoRouting freezes all targets without selecting credentials or making
// network calls. The public request and public price never use target defaults.
func PlanVideoRouting(ctx context.Context, config model.VideoRoutingConfig, rule model.VideoRoutingRule, group, publicModel, operation string, raw []byte, contentType string, facts map[string]any, userID, pinnedChannelID int, filters []dto.ChannelFilter) (*VideoRoutingSnapshot, *MixedVideoCandidate, []byte, string, error) {
	encodedRule, err := common.Marshal(rule)
	if err != nil {
		return nil, nil, nil, "", err
	}
	var frozenRule model.VideoRoutingRule
	if err := common.Unmarshal(encodedRule, &frozenRule); err != nil {
		return nil, nil, nil, "", err
	}
	snapshot := &VideoRoutingSnapshot{Revision: config.Revision, Rule: frozenRule, SourceBody: slices.Clone(raw), SourceContentType: contentType, SourceModel: publicModel, SourceFacts: maps.Clone(facts), TargetIndex: -1, PinnedChannelID: pinnedChannelID}
	for _, filter := range filters {
		snapshot.Filters = append(snapshot.Filters, VideoRoutingFilter{Kind: filter.Kind, RequestPath: filter.RequestPath, TaskPluginKey: filter.TaskPluginKey, TaskPluginKeys: slices.Clone(filter.TaskPluginKeys), TaskPluginChannelTypes: slices.Clone(filter.TaskPluginChannelTypes)})
	}
	var selected *MixedVideoCandidate
	var dispatch []byte
	dispatchType := contentType
	for index, target := range rule.Targets {
		frozen := VideoRoutingFrozenTarget{Target: target}
		candidate, body, ct, err := DecodeVideoRoutingTarget(ctx, target, group, operation, raw, contentType, userID)
		if err == nil && pinnedChannelID > 0 && target.ChannelID != pinnedChannelID {
			err = errors.New("target conflicts with the pinned channel")
		}
		if err == nil {
			if ok, _ := model.ChannelSatisfiesFilters(&candidate.Channel, target.Model, filters); !ok {
				err = errors.New("target does not satisfy channel constraints")
			}
		}
		if err == nil {
			frozen.UpstreamModel, frozen.PluginKey, frozen.PluginHash = candidate.UpstreamModel, candidate.Binding.Plugin.Meta.Key, candidate.Binding.Plugin.Engine.SourceFingerprint()
			// The host protocol keeps the client's public identity. Its separate
			// upstreamModel carries the actual execution model to the plugin.
			body, ct, err = RewriteVideoRoutingRequest(body, ct, operation, publicModel, "")
		}
		if err != nil {
			frozen.Reason = SanitizeAsyncImageDiagnostic(err.Error(), 512)
		} else if selected == nil {
			selected, dispatch, dispatchType, snapshot.TargetIndex = &candidate, body, ct, index
		}
		snapshot.Targets = append(snapshot.Targets, frozen)
	}
	if selected == nil {
		snapshot.Fallback = rule.OnExhausted == "original"
		if !snapshot.Fallback {
			return snapshot, nil, nil, "", errors.New("no compatible conditional video target is available")
		}
	}
	return snapshot, selected, dispatch, dispatchType, nil
}

// DecodeFrozenVideoRoutingTarget checks the live authority against the frozen
// target identity. Parameter rewriting always starts from the source request.
func DecodeFrozenVideoRoutingTarget(ctx context.Context, snapshot *VideoRoutingSnapshot, index int, group, operation string, userID int, frozenChannel ...*model.Channel) (MixedVideoCandidate, []byte, string, error) {
	if snapshot == nil || index < 0 || index >= len(snapshot.Targets) {
		return MixedVideoCandidate{}, nil, "", errors.New("conditional video target snapshot is unavailable")
	}
	target := snapshot.Targets[index]
	if target.Reason != "" || target.UpstreamModel == "" {
		return MixedVideoCandidate{}, nil, "", errors.New("conditional video target was unavailable at admission")
	}
	candidate, body, ct, err := DecodeVideoRoutingTarget(ctx, target.Target, group, operation, snapshot.SourceBody, snapshot.SourceContentType, userID, frozenChannel...)
	if err != nil {
		return candidate, nil, "", err
	}
	if candidate.UpstreamModel != target.UpstreamModel || candidate.Binding.Plugin.Meta.Key != target.PluginKey || candidate.Binding.Plugin.Engine.SourceFingerprint() != target.PluginHash {
		return candidate, nil, "", errors.New("conditional video target mapping or plugin changed")
	}
	if snapshot.PinnedChannelID > 0 && candidate.Channel.Id != snapshot.PinnedChannelID {
		return candidate, nil, "", errors.New("conditional video channel pin changed")
	}
	if ok, _ := model.ChannelSatisfiesFilters(&candidate.Channel, target.Target.Model, snapshot.ChannelFilters()); !ok {
		return candidate, nil, "", errors.New("conditional video target constraints changed")
	}
	body, ct, err = RewriteVideoRoutingRequest(body, ct, operation, snapshot.SourceModel, "")
	return candidate, body, ct, err
}

// DecodeVideoExecution uses the current body so localized references survive
// revalidation. Live mapping checks precede the frozen request's decoder.
func DecodeVideoExecution(ctx context.Context, request AsyncVideoRequest, channel model.Channel, publicModel, group string, overrides VideoModelOverrides, userID int) (MixedVideoCandidate, error) {
	target := request.ConditionalRouting.ActiveTarget()
	if target == nil {
		return DecodeMixedVideoCandidate(ctx, channel, publicModel, request.Operation, request.Body, overrides, userID, request.ContentType)
	}
	candidate, _, _, err := DecodeFrozenVideoRoutingTarget(ctx, request.ConditionalRouting, request.ConditionalRouting.TargetIndex, group, request.Operation, userID, request.frozenVideoMappingChannel())
	if err != nil {
		return candidate, err
	}
	if channel.Id != target.Target.ChannelID {
		return candidate, errors.New("conditional video channel does not match the frozen target")
	}
	body, ct, err := RewriteVideoRoutingRequest(request.Body, request.ContentType, request.Operation, target.Target.Model, "")
	if err != nil {
		return candidate, err
	}
	candidate, err = request.decodeMixedVideoRetryCandidate(ctx, channel, target.Target.Model, body, ct, VideoModelOverrides{}, userID)
	if err == nil && (candidate.UpstreamModel != target.UpstreamModel || candidate.Binding.Plugin.Engine.SourceFingerprint() != target.PluginHash || !request.ValidateMixedVideoMapping(candidate)) {
		err = errors.New("conditional video execution identity changed")
	}
	return candidate, err
}

// ConditionalVideoRetryCandidates preserves rule order and intersects it with
// the existing error policy. Unknown results require an explicit matching rule.
func ConditionalVideoRetryCandidates(ctx context.Context, job model.AsyncMediaJob, request AsyncVideoRequest, policy model.ImageGroupPolicy, catalog []ImageModelCapability) ([]model.Channel, bool, error) {
	snapshot, decision := request.ConditionalRouting, request.PendingDecision
	if snapshot == nil || decision == nil || request.RetryFact == nil {
		return nil, false, errors.New("conditional retry context is unavailable")
	}
	if request.RetryFact.HasOutput || decision.BlockedReason != "" || !decision.Matched && (request.RetryFact.Unknown || request.RetryFact.Accepted && !request.RetryFact.Terminal) {
		return nil, false, errors.New("unknown conditional video execution cannot be resubmitted")
	}
	other := decision.Action == mediaretry.OtherChannel || decision.Action == mediaretry.LocalOtherChannel
	channels := make([]model.Channel, 0)
	if !snapshot.Fallback {
		start, end := snapshot.TargetIndex, snapshot.TargetIndex+1
		if other {
			end = len(snapshot.Targets)
			// After rebinding, dispatch rechecks the selected destination
			// against the same failure; that does not advance the target again.
			if active := snapshot.ActiveTarget(); active != nil && active.Target.ChannelID == request.RetryFact.ChannelID {
				start++
			}
		}
		for index := max(0, start); index < end; index++ {
			candidate, _, _, err := DecodeFrozenVideoRoutingTarget(ctx, snapshot, index, job.Group, request.Operation, job.UserId, request.frozenVideoMappingChannel())
			if err != nil || !videoRoutingRetryAllowed(candidate.Channel, request, candidate) {
				continue
			}
			channels = append(channels, candidate.Channel)
		}
		if len(channels) > 0 || !other || snapshot.Rule.OnExhausted != "original" {
			return channels, false, nil
		}
	}
	ordinary, err := MixedVideoPoolChannels(ctx, policy, job.Model, request.MixedRouting.Resolution)
	if err != nil {
		return nil, true, err
	}
	capability, _ := MixedVideoPolicyCapability(catalog, job.Model)
	for _, channel := range ordinary {
		if snapshot.PinnedChannelID > 0 && channel.Id != snapshot.PinnedChannelID {
			continue
		}
		if ok, _ := model.ChannelSatisfiesFilters(&channel, job.Model, snapshot.ChannelFilters()); !ok {
			continue
		}
		candidate, err := request.decodeMixedVideoRetryCandidate(ctx, channel, job.Model, snapshot.SourceBody, snapshot.SourceContentType, capability.VideoOverrides, job.UserId)
		if err == nil && videoRoutingRetryAllowed(channel, request, candidate) {
			channels = append(channels, channel)
		}
	}
	return channels, true, nil
}

func videoRoutingRetryAllowed(channel model.Channel, request AsyncVideoRequest, candidate MixedVideoCandidate) bool {
	decision := request.PendingDecision
	other := decision.Action == mediaretry.OtherChannel || decision.Action == mediaretry.LocalOtherChannel
	if other && channel.Id == request.RetryFact.ChannelID || !other && channel.Id != request.RetryFact.ChannelID || slices.Contains(decision.ExcludedChannelIDs, channel.Id) || len(decision.AllowedChannelIDs) > 0 && !slices.Contains(decision.AllowedChannelIDs, channel.Id) {
		return false
	}
	if len(ImageChannelAccounts(channel, ImageAccountRouting{})) == 0 {
		return false
	}
	if decision.Action != mediaretry.LocalSameChannel && decision.Action != mediaretry.LocalOtherChannel {
		return true
	}
	selectors, _ := videoRetryInputSelectors(candidate.Binding.Plugin)
	return len(selectors) > 0
}

// RebindConditionalVideoRequest is called only after the retry policy selected
// a permissible channel. Restoring source parameters precedes fallback decode.
func RebindConditionalVideoRequest(ctx context.Context, job model.AsyncMediaJob, request *AsyncVideoRequest, channel model.Channel, overrides VideoModelOverrides) (MixedVideoCandidate, error) {
	snapshot := request.ConditionalRouting
	if snapshot == nil {
		return request.decodeMixedVideoRetryCandidate(ctx, channel, job.Model, request.Body, request.ContentType, overrides, job.UserId)
	}
	if !snapshot.Fallback {
		start, end := snapshot.TargetIndex, snapshot.TargetIndex+1
		if request.PendingDecision.Action == mediaretry.OtherChannel || request.PendingDecision.Action == mediaretry.LocalOtherChannel {
			// Repeated preparation for the same failure revalidates the already
			// selected destination instead of advancing the frozen target list.
			active := snapshot.ActiveTarget()
			if request.RetryFact == nil || active == nil || active.Target.ChannelID == request.RetryFact.ChannelID {
				start++
			}
			end = len(snapshot.Targets)
		}
		for index := max(0, start); index < end; index++ {
			if snapshot.Targets[index].Target.ChannelID != channel.Id {
				continue
			}
			candidate, body, ct, err := DecodeFrozenVideoRoutingTarget(ctx, snapshot, index, job.Group, request.Operation, job.UserId, request.frozenVideoMappingChannel())
			if err != nil {
				continue
			}
			keyIndex := slices.Index(candidate.Channel.GetKeys(), channel.Key)
			if !sameVideoMappingOrigin(candidate.Channel, channel) || keyIndex < 0 ||
				candidate.Channel.ChannelInfo.MultiKeyStatusList[keyIndex] != 0 && candidate.Channel.ChannelInfo.MultiKeyStatusList[keyIndex] != common.ChannelStatusEnabled {
				return candidate, errors.New("conditional video retry account or origin changed")
			}
			if err := applyVideoRoutingBody(request, body, ct, snapshot.Targets[index].Target.Resolution); err != nil {
				return candidate, err
			}
			snapshot.TargetIndex = index
			candidate.Channel.Key, candidate.Channel.Keys, candidate.Channel.ChannelInfo = channel.Key, channel.Keys, channel.ChannelInfo
			return candidate, nil
		}
		if snapshot.Rule.OnExhausted != "original" {
			return MixedVideoCandidate{}, errors.New("conditional video routing forbids original-route fallback")
		}
	}
	candidate, err := request.decodeMixedVideoRetryCandidate(ctx, channel, job.Model, snapshot.SourceBody, snapshot.SourceContentType, overrides, job.UserId)
	if err != nil {
		return candidate, fmt.Errorf("original video route: %w", err)
	}
	resolution, _ := snapshot.SourceFacts["resolution"].(string)
	if err := applyVideoRoutingBody(request, slices.Clone(snapshot.SourceBody), snapshot.SourceContentType, resolution); err != nil {
		return candidate, err
	}
	snapshot.Fallback = true
	return candidate, nil
}

// applyVideoRoutingBody preserves already localized media when the durable
// recovery worker selects its target after the download phase.
func applyVideoRoutingBody(request *AsyncVideoRequest, body []byte, contentType, resolution string) error {
	if !request.LocalReferencesReady || request.PendingDecision == nil || (request.PendingDecision.Action != mediaretry.LocalSameChannel && request.PendingDecision.Action != mediaretry.LocalOtherChannel) {
		request.LocalReferencesReady = false
		request.Body, request.ContentType, request.OriginalBody = body, contentType, nil
		return nil
	}
	if resolution == "" {
		resolution, _ = request.ConditionalRouting.SourceFacts["resolution"].(string)
	}
	localized, localizedType, err := RewriteVideoRoutingRequest(request.Body, request.ContentType, request.Operation, request.ConditionalRouting.SourceModel, resolution)
	if err != nil {
		return err
	}
	// Multipart rewrites retain the original boundary, but keep each replay
	// paired with its own type rather than mixing two independently built forms.
	if localizedType != contentType {
		return errors.New("localized conditional video content type changed")
	}
	request.Body, request.ContentType, request.OriginalBody = localized, localizedType, body
	return nil
}

// NormalizeVideoRoutingAdmissionDefaults preserves ordinary routing when no
// compatible source variant could match. If any variant can match, all source
// defaults must agree before a rule is allowed to choose an execution target.
// knownVideoInput is only used by parameter-only administrative previews;
// admission leaves it empty and derives the fact from actual request materials.
func NormalizeVideoRoutingAdmissionDefaults(config model.VideoRoutingConfig, publicModel string, groups []string, raw []byte, contentType, operation string, profilesByGroup map[string][]jsplugin.VideoProfile, knownVideoInput string) ([]byte, string, map[string]any, error) {
	profiles := make([]jsplugin.VideoProfile, 0)
	for _, group := range groups {
		profiles = append(profiles, profilesByGroup[group]...)
	}
	if VideoRoutingAppliesToSource(config, publicModel, groups) {
		view, _, err := MixedVideoRequestContext(raw, contentType, operation)
		if err != nil {
			return nil, "", nil, err
		}
		for _, group := range groups {
			for _, profile := range profilesByGroup[group] {
				for _, mode := range profile.Modes {
					variant := profile
					variant.Modes = []jsplugin.VideoModeProfile{mode}
					_, _, body, err := NormalizeMixedVideoDefaults(raw, contentType, operation, []jsplugin.VideoProfile{variant})
					if err != nil {
						continue
					}
					facts, err := MixedVideoPublicFacts(body, operation, view.Files, variant)
					if err != nil {
						continue
					}
					if knownVideoInput != "" {
						facts["video_input"] = knownVideoInput
					}
					if len(MatchVideoRoutingRules(config, group, publicModel, mode.Name, facts)) > 0 {
						return NormalizeVideoRoutingSourceDefaults(raw, contentType, operation, profiles)
					}
				}
			}
		}
	}
	return NormalizeMixedVideoDefaults(raw, contentType, operation, profiles)
}

// NormalizeVideoRoutingSourceDefaults refuses ambiguous source defaults and
// preserves supplied JSON scalar encodings while adding canonical defaults.
func NormalizeVideoRoutingSourceDefaults(raw []byte, contentType, operation string, profiles []jsplugin.VideoProfile) ([]byte, string, map[string]any, error) {
	var chosen map[string]any
	for _, profile := range profiles {
		_, _, body, err := NormalizeMixedVideoDefaults(raw, contentType, operation, []jsplugin.VideoProfile{profile})
		if err != nil {
			continue
		}
		facts, err := MixedVideoPublicFacts(body, operation, nil, profile)
		if err != nil {
			continue
		}
		if chosen != nil {
			for _, key := range []string{"seconds", "resolution"} {
				if chosen[key] != facts[key] {
					return nil, "", nil, fmt.Errorf("source video %s default is ambiguous; supply it explicitly", key)
				}
			}
		}
		chosen = facts
	}
	normalized, ct, body, err := NormalizeMixedVideoDefaults(raw, contentType, operation, profiles)
	if err != nil {
		return nil, "", nil, err
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "application/json" {
		var originalFields, normalizedFields map[string]common.RawMessage
		if err := common.Unmarshal(raw, &originalFields); err != nil {
			return nil, "", nil, err
		}
		if err := common.Unmarshal(normalized, &normalizedFields); err != nil {
			return nil, "", nil, err
		}
		for key, value := range normalizedFields {
			if _, exists := originalFields[key]; !exists {
				originalFields[key] = value
			}
		}
		normalized, err = common.Marshal(originalFields)
	}
	return normalized, ct, body, err
}

func PublicVideoRoutingBody(request AsyncVideoRequest) AsyncVideoRequest {
	if request.ConditionalRouting != nil {
		request.Body, request.ContentType = request.ConditionalRouting.SourceBody, request.ConditionalRouting.SourceContentType
	}
	return request
}

func (snapshot *VideoRoutingSnapshot) Audit(executionModel string) *dto.VideoRoutingAudit {
	if snapshot == nil {
		return nil
	}
	resolution, _ := snapshot.SourceFacts["resolution"].(string)
	audit := &dto.VideoRoutingAudit{Revision: snapshot.Revision, RuleID: snapshot.Rule.ID, RuleName: snapshot.Rule.Name, SourceModel: snapshot.SourceModel, SourceResolution: resolution, ExecutionModel: executionModel, ExecutionResolution: resolution, TargetIndex: snapshot.TargetIndex, IsFallback: snapshot.Fallback}
	if target := snapshot.ActiveTarget(); target != nil {
		audit.ExecutionModel = target.UpstreamModel
		if target.Target.Resolution != "" {
			audit.ExecutionResolution = target.Target.Resolution
		}
	}
	for _, target := range snapshot.Targets {
		audit.Targets = append(audit.Targets, dto.VideoRoutingTargetAudit{ChannelID: target.Target.ChannelID, Model: target.Target.Model, UpstreamModel: target.UpstreamModel, Resolution: target.Target.Resolution, Reason: target.Reason})
	}
	return audit
}

// PublicVideoRoutingFailureNotice keeps useful host explanations without
// exposing execution identities from a conditional route's upstream diagnostic.
// Persisted diagnostics and administrator projections remain unchanged.
func PublicVideoRoutingFailureNotice(raw string) VideoFailureNotice {
	if strings.TrimSpace(raw) == "" {
		return VideoFailureNotice{}
	}
	if index := strings.LastIndex(raw, "\n原始错误："); index >= 0 {
		raw = raw[index+len("\n原始错误："):]
	}
	// Reclassify text instead of trusting a provider-supplied formatted notice.
	raw = strings.ReplaceAll(raw, "\n处理建议：", " ")
	notice := VideoTaskFailureNotice(raw)
	notice.Detail = ""
	return notice
}

func PublicVideoRoutingFailureReason(raw string) string {
	return PublicVideoRoutingFailureNotice(raw).Message()
}
