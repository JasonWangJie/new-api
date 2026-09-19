package service

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

// The relay supplies capability declarations without coupling services to
// transport adaptors or creating a package import cycle.
var ImageChannelCapabilityFunc func(model.Channel, string) dto.ImageCapability

func ImageChannelCapability(channel model.Channel, modelName string) dto.ImageCapability {
	if ImageChannelCapabilityFunc == nil {
		return dto.ImageCapability{}
	}
	return ImageChannelCapabilityFunc(channel, modelName)
}

func RegisteredImageProvider(provider string) bool {
	if provider == "openai" || provider == "gemini" {
		return true
	}
	for channelType := range constant.ChannelTypeDummy {
		capability := ImageChannelCapability(model.Channel{Type: channelType}, "")
		if capability.Provider == provider && capability.Generate {
			return true
		}
	}
	return false
}

var FreezeAsyncImageBillingFunc func(context.Context, model.Token, AsyncImageRequest, string, *model.Channel) (MediaBillingSnapshot, error)

func RegisteredImageProviders() []string {
	providers := make([]string, 0)
	seen := make(map[string]bool)
	for channelType := range constant.ChannelTypeDummy {
		capability := ImageChannelCapability(model.Channel{Type: channelType}, "")
		if capability.Generate && !seen[capability.Provider] {
			providers = append(providers, capability.Provider)
			seen[capability.Provider] = true
		}
	}
	return providers
}

func ImageCapabilitySupportsRequest(capability dto.ImageCapability, request AsyncImageRequest) bool {
	if !capability.Generate {
		return false
	}
	if request.Kind != "image_to_image" {
		return true
	}
	return capability.Edit || capability.ReferenceField != "" && request.Dialect == "async" && strings.HasSuffix(request.SourcePath, "generations_async")
}

func RegisteredMediaProviders() []string {
	providers := RegisteredImageProviders()
	snapshot := jsplugin.DefaultRegistry.Snapshot()
	for _, meta := range append(snapshot.Factory, snapshot.Override...) {
		plugin, found := jsplugin.DefaultRegistry.Get(meta.Key)
		if !found || slices.Contains(providers, meta.Key) {
			continue
		}
		if slices.ContainsFunc(plugin.Meta.Protocols, func(claim jsplugin.ProtocolClaim) bool { return claim.Name == "openai_video" }) {
			providers = append(providers, meta.Key)
		}
	}
	return providers
}

func RegisteredMediaProvider(provider string) bool {
	return slices.Contains(RegisteredMediaProviders(), provider)
}

func VideoPoolResolution(size string) string {
	left, right, ok := strings.Cut(strings.ToLower(size), "x")
	width, widthErr := strconv.ParseUint(left, 10, 32)
	height, heightErr := strconv.ParseUint(right, 10, 32)
	if !ok || widthErr != nil || heightErr != nil || width == 0 || height == 0 {
		return "1K"
	}
	if min(width, height) > 2048 {
		return "4K"
	}
	if min(width, height) > 1024 {
		return "2K"
	}
	return "1K"
}
