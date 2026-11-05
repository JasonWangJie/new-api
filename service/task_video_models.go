package service

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

func ConfiguredTaskVideoFilter(c *gin.Context) dto.ChannelFilter {
	filter := dto.ChannelFilter{Kind: dto.FilterTaskVideoCapability}
	value, exists := c.Get(jsplugin.ContextKeyProtocolRequest)
	request, valid := value.(jsplugin.ProtocolRequestContext)
	if !exists || !valid || request.Protocol != "openai_video" {
		return filter
	}
	type decodedVideo struct {
		plugin *jsplugin.LoadedPlugin
		intent map[string]any
	}
	decoded := make(map[string]decodedVideo)
	for _, key := range kitdto.ChannelConfiguredTaskVideoPlugins() {
		plugin, found := jsplugin.DefaultRegistry.Get(key)
		if !found || !slices.ContainsFunc(plugin.Meta.Protocols, func(claim jsplugin.ProtocolClaim) bool { return claim.Name == "openai_video" }) {
			continue
		}
		if kitdto.PresetTaskVideoPlugin(key) {
			decoded[key] = decodedVideo{plugin: plugin}
			continue
		}
		result, err := plugin.Engine.CallPath(context.WithoutCancel(c.Request.Context()), "protocols", []string{"openai_video", "decodeRequest"}, request.JSValue())
		intent, ok := result.(map[string]any)
		if err == nil && ok && intent["kind"] == "submit" && intent["model"] == request.Model {
			decoded[key] = decodedVideo{plugin, intent}
		}
	}
	filter.TaskVideoValidator = func(settings kitdto.ChannelSettings, upstream string) bool {
		body, ok := decoded[settings.TaskPluginKey]
		if !ok {
			return false
		}
		if kitdto.PresetTaskVideoPlugin(settings.TaskPluginKey) {
			candidateRequest := request
			candidateRequest.UpstreamModel = upstream
			result, err := body.plugin.Engine.CallPath(context.WithoutCancel(c.Request.Context()), "protocols", []string{"openai_video", "decodeRequest"}, candidateRequest.JSValue())
			intent, valid := result.(map[string]any)
			if err != nil || !valid || intent["kind"] != "submit" || intent["model"] != request.Model {
				return false
			}
			body.intent = intent
		}
		config, err := model.ResolveTaskVideoModelConfig(settings, upstream)
		if err != nil {
			return false
		}
		if config.PresetModel != "" && config.BillingMode == "" {
			// Older source-declared models retain their decoder's original
			// contract without inventing a video capability or billing unit.
			return true
		}
		selected := jsplugin.WithTaskVideoModels(body.plugin, map[string]kitdto.TaskVideoModelConfig{upstream: config})
		profile, exists := selected.Meta.VideoProfileForModel(upstream)
		action, _ := body.intent["action"].(string)
		return exists && ValidateVideoProfileRequest(profile, action, body.intent["requestBody"], request.Files) == nil
	}
	return filter
}

func ChannelTaskVideoPlugin(plugin *jsplugin.LoadedPlugin, channel *model.Channel, publicModel string) (*jsplugin.LoadedPlugin, error) {
	if plugin == nil || !kitdto.ChannelConfiguredTaskVideoPlugin(plugin.Meta.Key) {
		return plugin, nil
	}
	upstream, config, err := channel.TaskVideoModelConfig(publicModel)
	if err != nil {
		return nil, err
	}
	return jsplugin.WithTaskVideoModels(plugin, map[string]kitdto.TaskVideoModelConfig{upstream: config, publicModel: config}), nil
}

// FrozenTaskVideoBinding resolves an accepted request using its encrypted channel
// snapshot even if that model was subsequently removed from live discovery.
func FrozenTaskVideoBinding(channel *model.Channel, publicModel string) (jsplugin.ProtocolBinding, error) {
	generation := jsplugin.DefaultRegistry.Generation()
	key := channel.GetSetting().TaskPluginKey
	if generation == nil || !kitdto.ChannelConfiguredTaskVideoPlugin(key) {
		return jsplugin.ProtocolBinding{}, errors.New("configured video plugin is unavailable")
	}
	plugin, found := generation.Get(key)
	if !found {
		return jsplugin.ProtocolBinding{}, fmt.Errorf("video plugin %s is unavailable", key)
	}
	selected, err := ChannelTaskVideoPlugin(plugin, channel, publicModel)
	if err != nil {
		return jsplugin.ProtocolBinding{}, err
	}
	frozen, err := generation.RebuildWithPlugins([]*jsplugin.LoadedPlugin{selected})
	if err != nil {
		return jsplugin.ProtocolBinding{}, err
	}
	binding, found := frozen.LookupEndpoint("POST", "/v1/videos", publicModel)
	if !found {
		return jsplugin.ProtocolBinding{}, fmt.Errorf("saved video model binding is unavailable")
	}
	return binding, nil
}
