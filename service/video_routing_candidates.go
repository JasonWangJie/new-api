package service

import (
	"context"
	"errors"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// DecodeVideoRoutingTarget validates an explicitly configured execution target.
// Its model is a configured channel alias, independently of the public model's
// ordinary pool; authorization of that public model remains with admission.
func DecodeVideoRoutingTarget(ctx context.Context, target model.VideoRoutingTarget, group, operation string, sourceRaw []byte, sourceContentType string, userID int, frozenChannel ...*model.Channel) (MixedVideoCandidate, []byte, string, error) {
	var empty MixedVideoCandidate
	var channel model.Channel
	if target.ChannelID <= 0 || target.Model == "" || group == "" {
		return empty, nil, "", errors.New("video routing target is incomplete")
	}
	if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", target.ChannelID, common.ChannelStatusEnabled).Take(&channel).Error; err != nil {
		return empty, nil, "", errors.New("video routing target channel is unavailable")
	}
	if !slices.Contains(channel.GetGroups(), group) || !slices.Contains(channel.GetModels(), target.Model) {
		return empty, nil, "", errors.New("video routing target model or group is unavailable")
	}
	var ability model.Ability
	if err := model.DB.WithContext(ctx).Where(map[string]any{"group": group, "model": target.Model, "channel_id": channel.Id, "enabled": true}).Take(&ability).Error; err != nil {
		return empty, nil, "", errors.New("video routing target model ability is unavailable")
	}
	if len(ImageChannelAccounts(channel, ImageAccountRouting{})) == 0 {
		return empty, nil, "", errors.New("video routing target has no available account")
	}
	if len(frozenChannel) > 0 && frozenChannel[0] != nil && frozenChannel[0].Id == channel.Id {
		frozen := *frozenChannel[0]
		if !sameVideoMappingOrigin(channel, frozen) {
			return empty, nil, "", errors.New("conditional video target origin changed")
		}
		frozen.Key, frozen.Keys, frozen.ChannelInfo = channel.Key, channel.Keys, channel.ChannelInfo
		channel = frozen
	}
	raw, contentType, err := RewriteVideoRoutingRequest(sourceRaw, sourceContentType, operation, target.Model, target.Resolution)
	if err != nil {
		return empty, nil, "", err
	}
	candidate, err := DecodeMixedVideoCandidate(ctx, channel, target.Model, operation, raw, VideoModelOverrides{}, userID, contentType)
	if err != nil {
		return empty, nil, "", err
	}
	return candidate, raw, contentType, nil
}
