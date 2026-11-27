package controller

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type mediaRetestModelOption struct {
	Model         string `json:"model"`
	UpstreamModel string `json:"upstream_model,omitempty"`
}

type mediaRetestChannelOption struct {
	ID     int                      `json:"id"`
	Name   string                   `json:"name"`
	Models []mediaRetestModelOption `json:"models"`
}

type mediaRetestOptions struct {
	OriginalChannelID         int                        `json:"original_channel_id"`
	OriginalChannelName       string                     `json:"original_channel_name,omitempty"`
	OriginalModel             string                     `json:"original_model"`
	OriginalUnavailableCode   string                     `json:"original_unavailable_code,omitempty"`
	OriginalUnavailableReason string                     `json:"original_unavailable_reason,omitempty"`
	Channels                  []mediaRetestChannelOption `json:"channels"`
	AutoModels                []mediaRetestModelOption   `json:"auto_models"`
}

func GetMediaRetestOptions(admission http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "private, no-store")
		tokenID, err := strconv.Atoi(c.Query("token_id"))
		if err != nil || tokenID <= 0 {
			mediaRetestError(c, http.StatusBadRequest, "invalid_retest_input", "A valid API key ID is required")
			return
		}
		token, clientIP, ok := mediaRetestToken(c, tokenID)
		if !ok {
			return
		}
		ctx := c.Request.Context()
		taskID := c.Param("task_id")
		mediaType := mediaRetestType(taskID)
		source, header, snapshot, ok := retainedMediaRetest(c, mediaType, taskID)
		if !ok {
			return
		}
		options := mediaRetestOptions{
			OriginalModel: source.Model, Channels: []mediaRetestChannelOption{}, AutoModels: []mediaRetestModelOption{},
		}
		if header.ImageDetails != nil && header.ImageDetails.Model != "" {
			options.OriginalModel = header.ImageDetails.Model
		}
		original, err := originalMediaRetestChannel(ctx, source)
		if err != nil {
			mediaRetestError(c, http.StatusServiceUnavailable, "retest_unavailable", "The task dispatch history is unavailable")
			return
		}
		options.OriginalChannelID, options.OriginalChannelName = original.Id, original.Name
		if err := service.ValidateMediaRetestReferences(ctx, source, token, header.MediaRetestRequest); err != nil {
			options.OriginalUnavailableCode, options.OriginalUnavailableReason = mediaRetestReferenceError(err)
			imageManagementData(c, options)
			return
		}
		candidates, err := mediaRetestCandidates(ctx, source.MediaType, token, options.OriginalModel, original.Id)
		if err != nil {
			mediaRetestError(c, http.StatusServiceUnavailable, "retest_unavailable", "The current channel and model catalog is unavailable")
			return
		}
		limits := token.GetModelLimitsMap()
		switch {
		case original.Id == 0:
			options.OriginalUnavailableCode, options.OriginalUnavailableReason = "channel_not_recorded", "The original task has no recorded channel"
		case original.Status != common.ChannelStatusEnabled:
			options.OriginalUnavailableCode, options.OriginalUnavailableReason = "channel_disabled", "The original channel is disabled or unavailable"
		case token.ModelLimitsEnabled && !limits[options.OriginalModel]:
			options.OriginalUnavailableCode, options.OriginalUnavailableReason = "model_not_allowed", "The original public model is outside this API key's permissions"
		}
		operator := mediaRetestOperator{userID: token.UserId, tokenID: token.Id}
		probeContext := context.WithValue(ctx, mediaRetestOperatorContext{}, operator)
		channels := make(map[int]mediaRetestChannelOption)
		for _, name := range slices.Sorted(maps.Keys(candidates)) {
			if ctx.Err() != nil {
				return
			}
			if token.ModelLimitsEnabled && !limits[name] {
				continue
			}
			request, err := service.SelectMediaRetestModel(header.MediaRetestRequest, name)
			if err != nil {
				mediaRetestError(c, http.StatusConflict, "request_unavailable", "The original request cannot be replayed")
				return
			}
			for _, id := range slices.Sorted(maps.Keys(candidates[name])) {
				if ctx.Err() != nil {
					return
				}
				probe := runMediaRetestAdmission(WithMediaRetestRouting(probeContext, id, true), admission, token, clientIP, request, "")
				upstreamModel, available := parseMediaRetestProbe(probe, id)
				if !available {
					if name == options.OriginalModel && id == original.Id && options.OriginalUnavailableCode == "" {
						_, rejection := parseMediaRetestAdmission(probe)
						options.OriginalUnavailableCode = "combination_unavailable"
						options.OriginalUnavailableReason = rejection.Message
					}
					continue
				}
				option := mediaRetestModelOption{Model: name, UpstreamModel: upstreamModel}
				if id == 0 {
					// A later automatic dispatch may resolve to another upstream model.
					option.UpstreamModel = ""
					options.AutoModels = append(options.AutoModels, option)
					continue
				}
				channel := channels[id]
				channel.ID, channel.Name = id, candidates[name][id]
				channel.Models = append(channel.Models, option)
				channels[id] = channel
			}
		}
		for _, id := range slices.Sorted(maps.Keys(channels)) {
			options.Channels = append(options.Channels, channels[id])
		}
		if time.Now().Unix() >= snapshot.RetainUntil {
			mediaRetestError(c, http.StatusGone, "request_expired", "The original request has expired")
			return
		}
		imageManagementData(c, options)
	}
}

func parseMediaRetestProbe(probe *httptest.ResponseRecorder, channelID int) (string, bool) {
	var result struct {
		ChannelID     int    `json:"channel_id"`
		UpstreamModel string `json:"upstream_model"`
	}
	valid := probe.Code == http.StatusOK && common.Unmarshal(probe.Body.Bytes(), &result) == nil &&
		result.ChannelID > 0 && (channelID == 0 || result.ChannelID == channelID)
	return result.UpstreamModel, valid
}

func originalMediaRetestChannel(ctx context.Context, source service.MediaRetestSource) (model.Channel, error) {
	channelID := source.ChannelID
	if source.MediaType == model.MediaReplayImage {
		var task model.AsyncImageTask
		if err := model.DB.WithContext(ctx).Select("channel_id", "attempts").Where("task_id = ?", source.TaskID).Take(&task).Error; err != nil {
			return model.Channel{}, err
		}
		channelID = task.ChannelId
		var attempts []service.ImageChannelAttempt
		if task.Attempts != "" {
			if err := common.UnmarshalJsonStr(task.Attempts, &attempts); err != nil {
				return model.Channel{}, err
			}
		}
		for _, attempt := range slices.Backward(attempts) {
			if attempt.ChannelId > 0 && (attempt.Dispatched || channelID == 0) {
				channelID = attempt.ChannelId
			}
			if attempt.ChannelId > 0 && attempt.Dispatched {
				break
			}
		}
	} else {
		var event model.AsyncMediaEvent
		err := model.DB.WithContext(ctx).Select("channel_id").Where("task_id = ? AND event_type = ? AND channel_id > ?", source.TaskID, "dispatched", 0).Order("id DESC").Take(&event).Error
		if err == nil {
			channelID = event.ChannelId
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return model.Channel{}, err
		}
	}
	channel := model.Channel{Id: channelID}
	if channelID <= 0 {
		return channel, nil
	}
	err := model.DB.WithContext(ctx).Select("id", "name", "status").Where("id = ?", channelID).Take(&channel).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = nil
	}
	return channel, err
}

// The catalog narrows the probes to advertised group/model pairs. Ordinary
// admission applies current token, pool, protocol, and request constraints.
func mediaRetestCandidates(ctx context.Context, mediaType string, token model.Token, originalModel string, originalChannel int) (map[string]map[int]string, error) {
	var owner model.User
	if err := model.DB.WithContext(ctx).Select("group").Where("id = ? AND status = ?", token.UserId, common.UserStatusEnabled).Take(&owner).Error; err != nil {
		return nil, err
	}
	groups := slices.Collect(maps.Keys(service.GetUserUsableGroups(owner.Group)))
	var abilities []model.Ability
	if err := model.DB.WithContext(ctx).Select("channel_id", "model").Where(map[string]any{"group": groups, "enabled": true}).Find(&abilities).Error; err != nil {
		return nil, err
	}
	var routes model.VideoRoutingConfig
	if mediaType == model.MediaReplayVideo {
		var err error
		routes, err = model.GetVideoRoutingConfig(ctx)
		if err != nil {
			return nil, err
		}
	}
	ids := make(map[int]bool)
	for _, ability := range abilities {
		ids[ability.ChannelId] = true
	}
	if originalChannel > 0 {
		ids[originalChannel] = true
	}
	var channelRows []model.Channel
	if len(ids) > 0 {
		if err := model.DB.WithContext(ctx).Select("id", "name").Where("id IN ? AND status = ?", slices.Collect(maps.Keys(ids)), common.ChannelStatusEnabled).Find(&channelRows).Error; err != nil {
			return nil, err
		}
	}
	names := make(map[int]string, len(channelRows))
	for _, channel := range channelRows {
		names[channel.Id] = channel.Name
	}
	candidates := make(map[string]map[int]string)
	add := func(name string, id int) {
		if name == "" {
			return
		}
		if candidates[name] == nil {
			candidates[name] = map[int]string{0: ""}
		}
		if channelName, active := names[id]; active {
			candidates[name][id] = channelName
		}
	}
	add(originalModel, originalChannel)
	channelsByModel := make(map[string][]int)
	for _, ability := range abilities {
		add(ability.Model, ability.ChannelId)
		channelsByModel[ability.Model] = append(channelsByModel[ability.Model], ability.ChannelId)
	}
	publicNames := map[string]bool{originalModel: true}
	for name := range token.GetModelLimitsMap() {
		publicNames[name] = true
	}
	if routes.Enabled {
		for _, rule := range routes.Rules {
			if !rule.Enabled {
				continue
			}
			for _, target := range rule.Targets {
				if len(rule.Match.Models) == 0 {
					for name := range candidates {
						add(name, target.ChannelID)
					}
				}
				for _, name := range rule.Match.Models {
					publicNames[name] = true
					add(name, target.ChannelID)
				}
			}
		}
	}
	for name := range publicNames {
		if normalized := ratio_setting.RoutingMatchModelName(name); normalized != name {
			for _, id := range channelsByModel[normalized] {
				add(name, id)
			}
		}
	}
	return candidates, nil
}
