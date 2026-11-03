package service

import (
	"context"
	"fmt"

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
	plugin, found := jsplugin.DefaultRegistry.Get("sudashui")
	if !exists || !valid || !found || request.Protocol != "openai_video" {
		return filter
	}
	decoded, err := plugin.Engine.CallPath(context.WithoutCancel(c.Request.Context()), "protocols", []string{"openai_video", "decodeRequest"}, request.JSValue())
	body, ok := decoded.(map[string]any)
	filter.TaskVideoValidator = func(settings kitdto.ChannelSettings, upstream string) bool {
		if err != nil || !ok {
			return false
		}
		config, exists := settings.TaskPluginModelConfigs[upstream]
		if !exists {
			return false
		}
		selected := jsplugin.WithTaskVideoModels(plugin, map[string]kitdto.TaskVideoModelConfig{upstream: config})
		profile, exists := selected.Meta.VideoProfileForModel(upstream)
		action, _ := body["action"].(string)
		return exists && ValidateVideoProfileRequest(profile, action, body["requestBody"], nil) == nil
	}
	return filter
}

func ChannelTaskVideoPlugin(plugin *jsplugin.LoadedPlugin, channel *model.Channel, publicModel string) (*jsplugin.LoadedPlugin, error) {
	if plugin == nil || plugin.Meta.Key != "sudashui" {
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
	plugin, found := generation.Get("sudashui")
	if !found {
		return jsplugin.ProtocolBinding{}, fmt.Errorf("Sudashui plugin is unavailable")
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
