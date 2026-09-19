package relay

import (
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
)

func init() {
	service.ImageChannelCapabilityFunc = imageChannelCapability
}

func imageChannelCapability(ch model.Channel, modelName string) dto.ImageCapability {
	if ch.Type <= constant.ChannelTypeUnknown || ch.Type >= constant.ChannelTypeDummy || ch.Type == constant.ChannelTypeMidjourney || ch.Type == constant.ChannelTypeMidjourneyPlus {
		return dto.ImageCapability{}
	}
	if ch.Type == constant.ChannelTypeTaskPlugin {
		return dto.ImageCapability{}
	}
	// Independent task protocols do not become image adaptors merely because
	// their historic channel types fall back to the OpenAI transport.
	apiType, known := common.ChannelType2APIType(ch.Type)
	if !known {
		snapshot := pluginruntime.DefaultRegistry.Snapshot()
		for _, meta := range append(snapshot.Factory, snapshot.Override...) {
			if slices.Contains(meta.ChannelTypes, ch.Type) {
				return dto.ImageCapability{}
			}
		}
	}
	provider, ok := GetAdaptor(apiType).(channel.ImageCapabilityProvider)
	if !ok {
		return dto.ImageCapability{}
	}
	// Model mappings determine the upstream protocol, while the original name
	// continues to identify permissions and billing.
	if ch.ModelMapping != nil && *ch.ModelMapping != "" {
		var mapping map[string]string
		if common.UnmarshalJsonStr(*ch.ModelMapping, &mapping) != nil {
			return dto.ImageCapability{}
		}
		if mapped := mapping[modelName]; mapped != "" {
			modelName = mapped
		}
	}
	if constant.IsAdvancedCustomChannel(ch.Type) && modelName != "" {
		var settings dto.ChannelOtherSettings
		if ch.OtherSettings != "" && common.UnmarshalJsonStr(ch.OtherSettings, &settings) != nil {
			return dto.ImageCapability{}
		}
		cfg := settings.AdvancedCustom
		if preset := common.GetAdvancedCustomPreset(ch.Type); preset != nil {
			cfg = preset
		}
		if cfg == nil || !slices.Contains(cfg.SupportedEndpointTypesForModel(modelName), constant.EndpointTypeImageGeneration) {
			return dto.ImageCapability{}
		}
	}
	capability := provider.ImageCapability(modelName)
	if modelName != "" && capability.Provider != "openai" && capability.Provider != "newapi" && capability.Provider != "moonshot" && capability.Provider != "advanced_custom" && !capability.DeclaresModel(modelName) {
		capability.Generate = false
		capability.Edit = false
	}
	switch ch.Type {
	case constant.ChannelTypeAzure:
		capability.Provider = "azure"
	case constant.ChannelTypeOpenRouter:
		capability.Provider = "openrouter"
	case constant.ChannelTypeXinference:
		capability.Provider = "xinference"
	case constant.ChannelTypeSub2API:
		capability.Provider = "sub2api"
	}
	if capability.Protocol == "gemini_native" && strings.HasPrefix(strings.ToLower(modelName), "imagen") {
		capability.Protocol, capability.Edit = "openai_images", false
	}
	return capability
}
