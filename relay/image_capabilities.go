package relay

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/volcengine"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func init() {
	service.ImageChannelCapabilityFunc = imageChannelCapability
	service.ValidateMixedImageChannelRequestFunc = validateMixedImageChannelRequest
}

// validateMixedImageChannelRequest performs no I/O. It checks provider limits
// before a mixed pool assigns weight to a channel that cannot execute the input.
func validateMixedImageChannelRequest(ch model.Channel, request service.AsyncImageRequest) error {
	settings := dto.ChannelSettings{}
	if ch.Setting != nil && *ch.Setting != "" {
		if err := common.UnmarshalJsonStr(*ch.Setting, &settings); err != nil {
			return fmt.Errorf("invalid image channel settings")
		}
	}
	otherSettings := dto.ChannelOtherSettings{}
	if ch.OtherSettings != "" {
		if err := common.UnmarshalJsonStr(ch.OtherSettings, &otherSettings); err != nil {
			return fmt.Errorf("invalid image channel other settings")
		}
	}
	if preset := common.GetAdvancedCustomPreset(ch.Type); preset != nil {
		otherSettings.AdvancedCustom = preset
	}
	var overrides map[string]any
	if ch.ParamOverride != nil && *ch.ParamOverride != "" {
		if err := common.UnmarshalJsonStr(*ch.ParamOverride, &overrides); err != nil {
			return fmt.Errorf("invalid image channel parameter override")
		}
	}
	capability := imageChannelCapability(ch, request.Model)
	if !capability.Generate || capability.Protocol != "openai_images" {
		return fmt.Errorf("channel does not declare the requested image protocol and model")
	}
	for name := range request.Native {
		if slices.Contains([]string{"model", "prompt", "n", "size", "resolution", "aspect_ratio", "ratio", "image", "images", "image_urls", "mask", "user", "response_format", "stream"}, name) {
			continue
		}
		// Native containers already accepted by the public parser still need
		// the matching adaptor; another provider must not silently ignore them.
		if name == "parameters" && (capability.Provider == "ali" || capability.Provider == "volcengine") ||
			name == "input" && (capability.Provider == "ali" || capability.Provider == "replicate") ||
			name == "extra_fields" && (capability.Provider == "replicate" || capability.Provider == "jimeng") ||
			name == "watermark" && capability.Provider == "ali" || name == "deferred" && capability.Provider == "xai" {
			continue
		}
		if !slices.Contains(capability.Parameters, name) {
			return fmt.Errorf("image channel does not support parameter %s", name)
		}
	}
	c, _ := gin.CreateTestContext(&asyncImageResponseWriter{Headers: make(http.Header), Limit: 1 << 20})
	// Capability previews must not rotate credentials or change account state.
	c.Set("model_mapping", ch.GetModelMapping())
	c.Set("async_image_execution", true)
	info := &relaycommon.RelayInfo{OriginModelName: request.Model, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: request.Model, ChannelType: ch.Type, ChannelId: ch.Id, ParamOverride: overrides, ChannelSetting: settings, ChannelOtherSettings: otherSettings}}
	info.ImageMixedBilling = true
	if err := helper.ModelMappedHelper(c, info, nil); err != nil {
		return err
	}
	c.Set(asyncImageResolutionModelContextKey, info.UpstreamModelName)
	operation := dto.UpstreamAsyncOperationGenerate
	if request.Kind == "image_to_image" && !(ch.Type == constant.ChannelTypeNewAPI && settings.TianyueMediaCompatibility) {
		operation = dto.UpstreamAsyncOperationEdit
	}
	profile, hasProfile := otherSettings.UpstreamAsync.Match(dto.UpstreamAsyncMediaImage, info.UpstreamModelName, operation)
	if ch.Type == constant.ChannelTypeTabTab {
		profile, hasProfile = service.TabtabAsyncImageProfile(info.UpstreamModelName), true
	}
	if !hasProfile && ch.Type == constant.ChannelTypeNewAPI && settings.TianyueMediaCompatibility {
		profile = service.TianyueAsyncImageProfile(info.UpstreamModelName)
		hasProfile = profile != nil
	}
	if err := validateMixedImageTokenUsage(request.Billing, capability, profile); err != nil {
		return err
	}
	passThrough := settings.PassThroughBodyEnabled || model_setting.GetGlobalSettings().PassThroughRequestEnabled
	if passThrough && !capability.OpenAICompatibleRequest {
		return fmt.Errorf("image channel pass-through cannot preserve the normalized image request; request conversion is required")
	}
	fields := make(map[string]common.RawMessage)
	maps.Copy(fields, request.Native)
	fields["model"], _ = common.Marshal(info.UpstreamModelName)
	fields["prompt"], _ = common.Marshal(request.Prompt)
	if request.Prompt == "" {
		// Model discovery and routing preview have no user prompt.
		fields["prompt"] = common.RawMessage(`"image routing preview"`)
	}
	fields["n"], _ = common.Marshal(request.Count)
	if request.Size != "" {
		fields["size"], _ = common.Marshal(request.Size)
	}
	if ch.Type == constant.ChannelTypeTabTab {
		for name, value := range map[string]string{"resolution": request.Resolution, "aspect_ratio": request.AspectRatio} {
			if value != "" {
				fields[name], _ = common.Marshal(value)
			}
		}
	}
	delete(fields, "images")
	delete(fields, "image_urls")
	delete(fields, "image")
	references := make([]string, 0, request.ReferenceImageCount())
	for _, part := range request.Parts {
		if part.Type == "mask" {
			if !capability.Mask {
				return fmt.Errorf("image channel does not support mask edits")
			}
			fields["mask"], _ = common.Marshal(map[string]string{"image_url": part.URL})
		}
		if part.Type == "image_url" {
			reference := part.URL
			if ch.Type == constant.ChannelTypeNewAPI && settings.TianyueMediaCompatibility && (strings.HasPrefix(strings.ToLower(reference), "data:") || service.IsGatewayImageReference(reference)) {
				// Dispatch resolves local input and replaces it with a signed
				// gateway URL. Probe that URL shape without downloading, signing
				// or changing the persisted reference during mixed admission.
				reference = fmt.Sprintf("https://image-admission.invalid/references/%d", len(references))
			}
			references = append(references, reference)
		}
	}
	if len(references) > 0 {
		if capability.ReferenceField != "" {
			fields[capability.ReferenceField], _ = common.Marshal(references)
		} else {
			images := make([]map[string]string, 0, len(references))
			for _, reference := range references {
				images = append(images, map[string]string{"image_url": reference})
			}
			fields["images"], _ = common.Marshal(images)
		}
	}
	if ch.Type == constant.ChannelTypeNewAPI && settings.TianyueMediaCompatibility {
		var err error
		fields, err = service.TianyueImageRequest(request, references)
		if err != nil {
			return err
		}
		fields["model"], _ = common.Marshal(info.UpstreamModelName)
	}
	data, err := common.Marshal(fields)
	if err != nil {
		return err
	}
	imageRequest := dto.ImageRequest{}
	if err := common.Unmarshal(data, &imageRequest); err != nil {
		return err
	}
	if raw, exists := fields["parameters"]; exists {
		imageRequest.BillingParameters = &dto.ImageBillingParameters{}
		if err := common.Unmarshal(raw, imageRequest.BillingParameters); err != nil {
			return err
		}
	}
	count, err := imageRequest.ImageCount(ch.Type == constant.ChannelTypeAli)
	if err != nil {
		return err
	}
	if count != request.Count {
		return fmt.Errorf("provider image count conflicts with the validated image count")
	}
	info.RelayMode = relayconstant.RelayModeImagesGenerations
	path := "/v1/images/generations"
	if request.Kind == "image_to_image" && capability.ReferenceField == "" && capability.EditFormat != "multipart" {
		info.RelayMode = relayconstant.RelayModeImagesEdits
		path = "/v1/images/edits"
	}
	// Multipart adaptors with a pure edit preparer share their projection with
	// dispatch. Other multipart adaptors probe scalars without reading files.
	c.Request, _ = http.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	c.Request.Header.Set("Content-Type", "application/json")
	info.ApiType, _ = common.ChannelType2APIType(ch.Type)
	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return fmt.Errorf("image channel has no image adaptor")
	}
	adaptor.Init(info)
	if !passThrough {
		var converted any
		if preparer, ok := adaptor.(channel.ImageEditRequestPreparer); ok && request.Kind == "image_to_image" && capability.EditFormat == "multipart" {
			converted, err = preparer.PrepareImageEditRequest(info, imageRequest, references)
		} else {
			converted, err = adaptor.ConvertImageRequest(c, info, imageRequest)
		}
		if err != nil {
			return err
		}
		prepared, prepareErr := prepareImageJSONSubmission(c, info, &imageRequest, converted)
		if prepareErr != nil {
			return prepareErr
		}
		data = prepared
	}

	if ch.Type == constant.ChannelTypeVolcEngine {
		data, err = volcengine.PrepareImageSubmission(data, info.UpstreamModelName, request.Count, true)
		if err != nil {
			return err
		}
	}
	if ch.Type == constant.ChannelTypeTabTab {
		if _, err := service.TabtabImageSubmission(data, info.UpstreamModelName, request.Count); err != nil {
			return err
		}
	}
	probe, err := http.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if hasProfile && profile.Submit.Request != nil {
		encoded, err := common.Marshal(fields)
		if err != nil {
			return err
		}
		source, err := asyncImageSubmitSource(request, encoded, references)
		if err != nil {
			return err
		}
		baseURL := ch.GetBaseURL()
		if baseURL == "" {
			baseURL = "https://image-admission.invalid"
		}
		probe, err = service.BuildUpstreamAsyncSubmitRequest(c.Request.Context(), baseURL, profile, upstreamAsyncImageTemplateContext(info, ""), source, bytes.NewReader(data), "application/json")
		if err != nil {
			return err
		}
		if profile.Submit.Request.Body != nil {
			if err := validateAsyncImageResolutionSubmitModel(c, probe); err != nil {
				return err
			}
		}
	}
	if err := service.ValidateMixedImageSubmitCount(probe, request.Count); err != nil {
		return err
	}
	if len(request.Parts) > 0 && slices.ContainsFunc(request.Parts, func(part service.AsyncImageInputPart) bool { return part.Type == "mask" }) {
		// A channel override or submit template must not silently discard a
		// mask after the adaptor has declared support for it.
		if probe.GetBody == nil {
			return fmt.Errorf("image submit body cannot preserve the requested mask")
		}
		body, err := probe.GetBody()
		if err != nil {
			return err
		}
		defer body.Close()
		encoded, err := io.ReadAll(io.LimitReader(body, 64<<20))
		mask := gjson.GetBytes(encoded, "mask")
		if err != nil || !mask.Exists() || mask.Type == gjson.Null || mask.Type == gjson.String && strings.TrimSpace(mask.String()) == "" {
			return fmt.Errorf("image submit body cannot preserve the requested mask")
		}
	}
	return nil
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
	if ch.Type == constant.ChannelTypeTabTab {
		var err error
		modelName, err = service.ResolveImageChannelUpstreamModel(ch, modelName)
		if err != nil {
			return dto.ImageCapability{}
		}
	} else if ch.ModelMapping != nil && *ch.ModelMapping != "" {
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
	if ch.Type == constant.ChannelTypeTabTab {
		capability = service.TabtabImageCapability(modelName)
	}
	if ch.Type == constant.ChannelTypeNewAPI && ch.GetSetting().TianyueMediaCompatibility {
		capability.Mask = false
		capability.ReferenceField = "images"
		capability.Parameters = append(slices.Clone(capability.Parameters), "aspect_ratio", "output_resolution")
	}
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

// A directory probe has no frozen price. At admission, token expressions must
// have an executable usage contract; a total-only mapping cannot price p/c.
func validateMixedImageTokenUsage(snapshot *service.MediaBillingSnapshot, capability dto.ImageCapability, profile *dto.UpstreamAsyncProfile) error {
	if snapshot == nil {
		return nil
	}
	used := map[string]bool{}
	if snapshot.Tiered != nil && snapshot.Tiered.BillingMode == "tiered_expr" {
		maps.Copy(used, billingexpr.UsedVars(snapshot.Tiered.ExprString))
	} else if !snapshot.Price.UsePrice && snapshot.Price.QuotaToPreConsume > 0 {
		used["p"], used["c"] = true, true
	}
	if profile != nil {
		// Subcategories must also supply their aggregate axes for canonical
		// usage validation and the same token normalization used at settlement.
		used["p"] = used["p"] || used["cr"] || used["cc"] || used["img"] || used["img_cr"] || used["ai"]
		used["c"] = used["c"] || used["img_o"] || used["ao"]
		used["cr"] = used["cr"] || used["img_cr"]
		used["img"] = used["img"] || used["img_cr"]
		if used["cc1h"] {
			return fmt.Errorf("image async profile cannot supply the Anthropic token usage semantics required by cc1h")
		}
	}
	requirements := map[string][]string{
		"p":      {"prompt_tokens", "input_tokens"},
		"len":    {"prompt_tokens", "input_tokens"},
		"c":      {"completion_tokens", "output_tokens"},
		"cr":     {"prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens"},
		"cc":     {"prompt_tokens_details.cached_creation_tokens", "prompt_tokens_details.cache_write_tokens", "input_tokens_details.cached_creation_tokens", "input_tokens_details.cache_write_tokens"},
		"cc1h":   {"claude_cache_creation_1_h_tokens"},
		"img":    {"prompt_tokens_details.image_tokens", "input_tokens_details.image_tokens"},
		"img_cr": {"prompt_tokens_details.cached_tokens_details.image_tokens", "input_tokens_details.cached_tokens_details.image_tokens"},
		"img_o":  {"completion_tokens_details.image_tokens", "output_tokens_details.image_tokens"},
		"ai":     {"prompt_tokens_details.audio_tokens", "input_tokens_details.audio_tokens"},
		"ao":     {"completion_tokens_details.audio_tokens", "output_tokens_details.audio_tokens"},
	}
	for variable, alternatives := range requirements {
		if !used[variable] {
			continue
		}
		if profile == nil {
			if !capability.TokenUsage {
				return fmt.Errorf("image channel does not provide actual token usage for the captured price")
			}
			continue
		}
		if !slices.ContainsFunc(alternatives, func(target string) bool { return strings.TrimSpace(profile.Poll.Response.UsagePaths[target]) != "" }) {
			return fmt.Errorf("image async profile does not map actual token usage required by %s", variable)
		}
	}
	return nil
}
