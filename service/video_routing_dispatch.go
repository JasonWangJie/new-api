package service

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// RefreshConditionalVideoAdmission can move an unsent, uncharged job when its
// frozen target is revoked while queued. Once dispatch or reservation starts,
// only the existing evidence-based recovery policy may select another target.
func RefreshConditionalVideoAdmission(ctx context.Context, job *model.AsyncMediaJob, request *AsyncVideoRequest) error {
	snapshot := request.ConditionalRouting
	if snapshot.ActiveTarget() == nil || job.DispatchedAt != 0 || job.Quota != 0 || job.AttemptGeneration != 0 || job.BillingStatus != "pending" || request.PendingDecision != nil {
		return nil
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", job.TokenId, job.UserId).Take(&token).Error; err != nil {
		return err
	}
	policy, catalog, err := MixedVideoPolicy(ctx, token, request.MixedRouting)
	if err != nil {
		return err
	}
	current, _, _, currentErr := DecodeFrozenVideoRoutingTarget(ctx, snapshot, snapshot.TargetIndex, job.Group, request.Operation, job.UserId)
	if currentErr == nil {
		keyIndex := slices.Index(current.Channel.GetKeys(), request.Channel.Key)
		if keyIndex >= 0 && (current.Channel.ChannelInfo.MultiKeyStatusList[keyIndex] == 0 || current.Channel.ChannelInfo.MultiKeyStatusList[keyIndex] == common.ChannelStatusEnabled) {
			return nil
		}
		currentErr = errors.New("frozen video account is no longer enabled")
	}
	snapshot.Targets[snapshot.TargetIndex].Reason = SanitizeAsyncImageDiagnostic(currentErr.Error(), 512)
	var selected *MixedVideoCandidate
	var body []byte
	contentType := request.ContentType
	for index := snapshot.TargetIndex + 1; index < len(snapshot.Targets); index++ {
		candidate, raw, ct, err := DecodeFrozenVideoRoutingTarget(ctx, snapshot, index, job.Group, request.Operation, job.UserId)
		if err != nil {
			snapshot.Targets[index].Reason = SanitizeAsyncImageDiagnostic(err.Error(), 512)
			continue
		}
		selected, body, contentType, snapshot.TargetIndex = &candidate, raw, ct, index
		break
	}
	if selected == nil && snapshot.Rule.OnExhausted == "original" {
		channels, err := MixedVideoPoolChannels(ctx, policy, job.Model, request.MixedRouting.Resolution)
		if err != nil {
			return err
		}
		capability, _ := MixedVideoPolicyCapability(catalog, job.Model)
		candidates := make([]MixedVideoCandidate, 0)
		weights := make([]int, 0)
		for _, channel := range channels {
			if snapshot.PinnedChannelID > 0 && channel.Id != snapshot.PinnedChannelID {
				continue
			}
			if ok, _ := model.ChannelSatisfiesFilters(&channel, job.Model, snapshot.ChannelFilters()); !ok {
				continue
			}
			candidate, err := DecodeMixedVideoCandidate(ctx, channel, job.Model, request.Operation, snapshot.SourceBody, capability.VideoOverrides, job.UserId, snapshot.SourceContentType)
			if err != nil || len(ImageChannelAccounts(channel, ImageAccountRouting{})) == 0 {
				continue
			}
			candidates = append(candidates, candidate)
			weights = append(weights, channel.GetWeight())
		}
		if len(candidates) > 0 {
			// Preserve the original pool's configured weights on fallback.
			index := PickMediaChannelByWeight(weights, rand.Float64())
			if index >= 0 {
				selected = &candidates[index]
				body, contentType = slices.Clone(snapshot.SourceBody), snapshot.SourceContentType
				snapshot.Fallback = true
			}
		}
	}
	if selected == nil {
		return errors.New("no frozen video routing target is available before dispatch")
	}
	key, _, keyErr := selected.Channel.GetNextEnabledKey()
	if keyErr != nil || key == "" {
		return errors.New("conditional video target account is unavailable")
	}
	request.Body, request.ContentType, request.OriginalBody = body, contentType, nil
	if err := RebindMixedVideoRequest(request, *selected, job.ExpiresAt); err != nil {
		return err
	}
	request.Channel.Key, request.Channel.Keys, request.Channel.ChannelInfo = key, nil, model.ChannelInfo{}
	encoded, err := common.Marshal(request)
	if err != nil {
		return err
	}
	cipher, err := EncryptImagePayload(encoded, "media-request:"+job.TaskId)
	if err != nil {
		return err
	}
	channelData, err := common.Marshal(request.Channel)
	if err != nil {
		return err
	}
	channelCipher, err := EncryptImagePayload(channelData, "media-channel:"+job.TaskId)
	if err != nil {
		return err
	}
	provider := selected.Binding.Plugin.Meta.Key
	query := model.DB.WithContext(ctx).Model(&model.AsyncMediaJob{}).Where("id = ? AND lease_token = ? AND attempt_generation = ? AND COALESCE(dispatched_at, 0) = ? AND quota = ? AND billing_status = ?", job.ID, job.LeaseToken, 0, 0, 0, "pending")
	if err := model.ChangeAsyncMediaJob(query, map[string]any{"channel_id": selected.Channel.Id, "provider": provider, "request_cipher": cipher, "channel_cipher": channelCipher}, ""); err != nil {
		return err
	}
	job.ChannelId, job.Provider, job.RequestCipher, job.ChannelCipher = selected.Channel.Id, provider, cipher, channelCipher
	return nil
}
