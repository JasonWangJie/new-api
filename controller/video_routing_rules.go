package controller

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func GetVideoRoutingConfig(c *gin.Context) {
	config, err := model.GetVideoRoutingConfig(c.Request.Context())
	if err != nil {
		imageManagementError(c, http.StatusServiceUnavailable, errors.New("Video routing rules are unavailable"))
		return
	}
	imageManagementData(c, config)
}

func UpdateVideoRoutingConfig(c *gin.Context) {
	input := struct {
		model.VideoRoutingConfig
		Revision *int64 `json:"revision"`
	}{}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &input); err != nil {
		imageManagementError(c, http.StatusBadRequest, err)
		return
	}
	if input.Revision == nil {
		imageManagementError(c, http.StatusPreconditionRequired, errors.New("Configuration revision is required. Reload it before saving"))
		return
	}
	input.VideoRoutingConfig.Revision = *input.Revision
	if err := service.ValidateVideoRoutingConfig(c.Request.Context(), input.VideoRoutingConfig); err != nil {
		imageManagementError(c, http.StatusBadRequest, err)
		return
	}
	config, err := model.SaveVideoRoutingConfig(c.Request.Context(), input.VideoRoutingConfig, *input.Revision)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrOptionRevisionConflict):
			imageManagementError(c, http.StatusConflict, err)
		case errors.Is(err, model.ErrOptionRevisionInvalid):
			imageManagementError(c, http.StatusBadRequest, err)
		default:
			imageManagementError(c, http.StatusServiceUnavailable, errors.New("Video routing rules could not be saved"))
		}
		return
	}
	recordManageAudit(c, "video_routing_rules.update", map[string]any{"revision": config.Revision, "enabled": config.Enabled, "rule_count": len(config.Rules)})
	imageManagementData(c, config)
}

// GetVideoRoutingOptions exposes channel aliases rather than credentials or
// provider settings. An omitted group lists all currently enabled candidates.
func GetVideoRoutingOptions(c *gin.Context) {
	group := strings.TrimSpace(c.Query("group"))
	var channels []model.Channel
	if err := model.DB.WithContext(c.Request.Context()).Omit("key").Where("status = ?", common.ChannelStatusEnabled).Order("id").Find(&channels).Error; err != nil {
		imageManagementError(c, http.StatusServiceUnavailable, errors.New("Video routing channel options are unavailable"))
		return
	}
	type modelOption struct {
		Model         string `json:"model"`
		UpstreamModel string `json:"upstream_model"`
	}
	type channelOption struct {
		ID     int           `json:"id"`
		Name   string        `json:"name"`
		Models []modelOption `json:"models"`
	}
	options := make([]channelOption, 0)
	for _, channel := range channels {
		if group != "" && !slices.Contains(channel.GetGroups(), group) {
			continue
		}
		option := channelOption{ID: channel.Id, Name: channel.Name, Models: []modelOption{}}
		for _, alias := range channel.GetModels() {
			binding, upstream, err := service.MixedVideoChannelBinding(channel, alias, "create")
			if err != nil {
				continue
			}
			profile, err := service.EffectiveMixedVideoProfile(binding, alias, service.VideoModelOverrides{})
			if err != nil || !slices.ContainsFunc(profile.Modes, func(mode jsplugin.VideoModeProfile) bool {
				return mode.Name != "edit_video" && mode.Name != "extend_video"
			}) {
				continue
			}
			option.Models = append(option.Models, modelOption{Model: alias, UpstreamModel: upstream})
		}
		if len(option.Models) > 0 {
			options = append(options, option)
		}
	}
	imageManagementData(c, gin.H{"channels": options})
}
