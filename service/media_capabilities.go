package service

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	gatewaydto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
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
	for _, meta := range slices.Concat(snapshot.Factory, snapshot.Override) {
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

// MediaChannelSupportsPlatform reports whether a channel can execute the
// selected image provider/protocol family or video task-plugin provider.
func MediaChannelSupportsPlatform(channel *model.Channel, modelName, platform string) bool {
	if channel == nil {
		return false
	}
	if ImageChannelSupportsPlatform(*channel, modelName, platform) {
		return true
	}
	for _, candidate := range mediaVideoCandidates(modelName) {
		if candidate.Plugin == nil || candidate.Protocol != "openai_video" || candidate.Plugin.Meta.Key != platform {
			continue
		}
		filter := gatewaydto.ChannelFilter{
			Kind:                   gatewaydto.FilterTaskPluginIdentity,
			TaskPluginKey:          platform,
			TaskPluginChannelTypes: candidate.Plugin.Meta.ChannelTypes,
		}
		if matches, _ := model.ChannelSatisfiesFilters(channel, modelName, []gatewaydto.ChannelFilter{filter}); matches {
			return true
		}
	}
	return false
}

// MediaPlatformSupportsVideoModel reports whether a registered task plugin
// exposes the model through the host-owned OpenAI video protocol.
func MediaPlatformSupportsVideoModel(modelName, platform string) bool {
	return slices.ContainsFunc(mediaVideoCandidates(modelName), func(candidate jsplugin.ProtocolBinding) bool {
		return candidate.Plugin != nil && candidate.Protocol == "openai_video" && candidate.Plugin.Meta.Key == platform
	})
}

func mediaVideoCandidates(modelName string) []jsplugin.ProtocolBinding {
	generation := jsplugin.DefaultRegistry.Generation()
	candidates := generation.LookupEndpointCandidates("POST", "/v1/videos", modelName)
	if len(candidates) == 0 {
		if alias, ok := model.ResolveTaskModelAlias(generation, modelName); ok {
			candidates = generation.LookupEndpointCandidates("POST", "/v1/videos", alias.Declared)
		}
	}
	return candidates
}

func GenericVideoProfile(modelName string) jsplugin.VideoProfile {
	textDuration := jsplugin.VideoDurationProfile{Min: 1, Max: relaycommon.MaxTaskDurationSeconds, Step: 1, Default: 5}
	imageDuration := textDuration
	resolutions := []string{"480p", "720p", "1080p", "4k"}
	return jsplugin.VideoProfile{
		Models: []string{modelName},
		Modes: []jsplugin.VideoModeProfile{
			{Name: "text_to_video", Duration: &textDuration, Resolutions: slices.Clone(resolutions), DefaultResolution: "720p"},
			{Name: "image_to_video", Duration: &imageDuration, Resolutions: slices.Clone(resolutions), DefaultResolution: "720p", Inputs: []jsplugin.VideoInputProfile{{Name: "image", Kind: "image", Sources: []string{"url", "asset", "data_uri", "file_id", "upload"}, MaxItems: 1}}},
		},
	}
}

// EffectiveVideoProfile returns the plugin declaration narrowed by an
// administrator policy. Plugins predating videoProfiles retain the generic
// OpenAI Video form.
func EffectiveVideoProfile(binding jsplugin.ProtocolBinding, modelName string, overrides VideoModelOverrides) (jsplugin.VideoProfile, error) {
	declaredModel := binding.Model
	if declaredModel == "" {
		declaredModel = modelName
	}
	profile, found := binding.Plugin.Meta.VideoProfileForModel(declaredModel)
	if !found {
		profile = GenericVideoProfile(modelName)
	}
	profile.Models = []string{modelName}
	return ApplyVideoModelOverrides(profile, overrides)
}

func ApplyVideoModelOverrides(profile jsplugin.VideoProfile, overrides VideoModelOverrides) (jsplugin.VideoProfile, error) {
	profile = jsplugin.CloneVideoProfile(profile)
	if overrides.Modes != nil {
		if len(overrides.Modes) == 0 {
			return profile, fmt.Errorf("video_overrides.modes must not be empty")
		}
		seen := make(map[string]struct{}, len(overrides.Modes))
		for _, name := range overrides.Modes {
			if _, duplicate := seen[name]; duplicate {
				return profile, fmt.Errorf("video_overrides.modes must be unique")
			}
			seen[name] = struct{}{}
			if !slices.ContainsFunc(profile.Modes, func(mode jsplugin.VideoModeProfile) bool { return mode.Name == name }) {
				return profile, fmt.Errorf("video_overrides mode %q expands the plugin capability", name)
			}
		}
		profile.Modes = slices.DeleteFunc(profile.Modes, func(mode jsplugin.VideoModeProfile) bool { return !slices.Contains(overrides.Modes, mode.Name) })
	}
	knownInputs := make(map[string]struct{})
	durationModes, resolutionModes, aspectRatioModes, directionModes := 0, 0, 0, 0
	for modeIndex := range profile.Modes {
		mode := &profile.Modes[modeIndex]
		if mode.Duration != nil {
			durationModes++
		}
		if overrides.Duration != nil && mode.Duration != nil {
			if err := narrowVideoDuration(mode.Duration, *overrides.Duration); err != nil {
				return profile, fmt.Errorf("video_overrides duration for %s: %w", mode.Name, err)
			}
		}
		if len(mode.Resolutions) > 0 {
			resolutionModes++
		}
		if overrides.Resolutions != nil && len(mode.Resolutions) > 0 {
			values, err := narrowVideoChoices(mode.Resolutions, overrides.Resolutions, "resolution")
			if err != nil {
				return profile, fmt.Errorf("video_overrides for %s: %w", mode.Name, err)
			}
			mode.Resolutions = values
		}
		if overrides.DefaultResolution != "" && len(mode.Resolutions) > 0 {
			if !slices.Contains(mode.Resolutions, overrides.DefaultResolution) {
				return profile, fmt.Errorf("video_overrides default_resolution is unavailable for %s", mode.Name)
			}
			mode.DefaultResolution = overrides.DefaultResolution
		} else if len(mode.Resolutions) > 0 && !slices.Contains(mode.Resolutions, mode.DefaultResolution) {
			mode.DefaultResolution = mode.Resolutions[0]
		}
		if len(mode.AspectRatios) > 0 {
			aspectRatioModes++
		}
		if overrides.AspectRatios != nil && len(mode.AspectRatios) > 0 {
			values, err := narrowVideoChoices(mode.AspectRatios, overrides.AspectRatios, "aspect ratio")
			if err != nil {
				return profile, fmt.Errorf("video_overrides for %s: %w", mode.Name, err)
			}
			mode.AspectRatios = values
		}
		if overrides.DefaultAspectRatio != "" && len(mode.AspectRatios) > 0 {
			if !slices.Contains(mode.AspectRatios, overrides.DefaultAspectRatio) {
				return profile, fmt.Errorf("video_overrides default_aspect_ratio is unavailable for %s", mode.Name)
			}
			mode.DefaultAspectRatio = overrides.DefaultAspectRatio
		} else if len(mode.AspectRatios) > 0 && !slices.Contains(mode.AspectRatios, mode.DefaultAspectRatio) {
			mode.DefaultAspectRatio = mode.AspectRatios[0]
		}
		if len(mode.ExtensionDirections) > 0 {
			directionModes++
		}
		if overrides.ExtensionDirections != nil && len(mode.ExtensionDirections) > 0 {
			values, err := narrowVideoChoices(mode.ExtensionDirections, overrides.ExtensionDirections, "extension direction")
			if err != nil {
				return profile, fmt.Errorf("video_overrides for %s: %w", mode.Name, err)
			}
			mode.ExtensionDirections = values
		}
		if overrides.DefaultExtensionDirection != "" && len(mode.ExtensionDirections) > 0 {
			if !slices.Contains(mode.ExtensionDirections, overrides.DefaultExtensionDirection) {
				return profile, fmt.Errorf("video_overrides default_extension_direction is unavailable for %s", mode.Name)
			}
			mode.DefaultExtensionDirection = overrides.DefaultExtensionDirection
		} else if len(mode.ExtensionDirections) > 0 && !slices.Contains(mode.ExtensionDirections, mode.DefaultExtensionDirection) {
			mode.DefaultExtensionDirection = mode.ExtensionDirections[0]
		}
		for inputIndex := range mode.Inputs {
			input := &mode.Inputs[inputIndex]
			knownInputs[input.Name] = struct{}{}
			if limit, exists := overrides.MaxInputItems[input.Name]; exists {
				if limit < 1 || limit > input.MaxItems {
					return profile, fmt.Errorf("video_overrides max_input_items.%s expands or disables the plugin capability", input.Name)
				}
				input.MaxItems = limit
			}
		}
	}
	if overrides.Duration != nil && durationModes == 0 {
		return profile, fmt.Errorf("video_overrides duration is unavailable for the selected modes")
	}
	if (overrides.Resolutions != nil || overrides.DefaultResolution != "") && resolutionModes == 0 {
		return profile, fmt.Errorf("video_overrides resolutions are unavailable for the selected modes")
	}
	if (overrides.AspectRatios != nil || overrides.DefaultAspectRatio != "") && aspectRatioModes == 0 {
		return profile, fmt.Errorf("video_overrides aspect ratios are unavailable for the selected modes")
	}
	if (overrides.ExtensionDirections != nil || overrides.DefaultExtensionDirection != "") && directionModes == 0 {
		return profile, fmt.Errorf("video_overrides extension directions are unavailable for the selected modes")
	}
	for name := range overrides.MaxInputItems {
		if _, exists := knownInputs[name]; !exists {
			return profile, fmt.Errorf("video_overrides max_input_items.%s is not declared by the selected modes", name)
		}
	}
	return profile, nil
}

func narrowVideoChoices(upstream, requested []string, label string) ([]string, error) {
	if len(requested) == 0 {
		return nil, fmt.Errorf("%ss must not be empty", label)
	}
	seen := make(map[string]struct{}, len(requested))
	for _, value := range requested {
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("%ss must be unique", label)
		}
		if !slices.Contains(upstream, value) {
			return nil, fmt.Errorf("%s %q expands the plugin capability", label, value)
		}
		seen[value] = struct{}{}
	}
	return slices.Clone(requested), nil
}

func narrowVideoDuration(duration *jsplugin.VideoDurationProfile, override VideoDurationOverrides) error {
	allows := func(value int) bool {
		if len(duration.Values) > 0 {
			return slices.Contains(duration.Values, value)
		}
		return value >= duration.Min && value <= duration.Max && (value-duration.Min)%duration.Step == 0
	}
	if override.Values != nil {
		if len(override.Values) == 0 {
			return fmt.Errorf("values must not be empty")
		}
		seen := make(map[int]struct{}, len(override.Values))
		for _, value := range override.Values {
			if !allows(value) {
				return fmt.Errorf("value %d expands the plugin capability", value)
			}
			if _, duplicate := seen[value]; duplicate {
				return fmt.Errorf("values must be unique")
			}
			seen[value] = struct{}{}
		}
		duration.Min, duration.Max, duration.Step = 0, 0, 0
		duration.Values = slices.Clone(override.Values)
	} else if len(duration.Values) > 0 {
		minimum, maximum := 1, relaycommon.MaxTaskDurationSeconds
		if override.Min != nil {
			minimum = *override.Min
		}
		if override.Max != nil {
			maximum = *override.Max
		}
		duration.Values = slices.DeleteFunc(duration.Values, func(value int) bool { return value < minimum || value > maximum })
		if len(duration.Values) == 0 {
			return fmt.Errorf("range excludes every plugin duration")
		}
	} else {
		minimum, maximum := duration.Min, duration.Max
		if override.Min != nil {
			minimum = *override.Min
		}
		if override.Max != nil {
			maximum = *override.Max
		}
		if minimum > maximum || !allows(minimum) || !allows(maximum) {
			return fmt.Errorf("range expands or misaligns with the plugin capability")
		}
		duration.Min, duration.Max = minimum, maximum
	}
	if override.Default != nil {
		duration.Default = *override.Default
	}
	if len(duration.Values) > 0 {
		if !slices.Contains(duration.Values, duration.Default) {
			if override.Default != nil {
				return fmt.Errorf("default is outside the narrowed duration")
			}
			duration.Default = duration.Values[0]
		}
		return nil
	}
	if duration.Default < duration.Min || duration.Default > duration.Max || (duration.Default-duration.Min)%duration.Step != 0 {
		if override.Default != nil {
			return fmt.Errorf("default is outside the narrowed duration")
		}
		duration.Default = duration.Min
	}
	return nil
}

func VideoPolicyCapability(catalog []ImageModelCapability, modelName string) (ImageModelCapability, bool) {
	index := slices.IndexFunc(catalog, func(capability ImageModelCapability) bool { return capability.MatchesMedia(modelName, "video") })
	if index < 0 {
		return ImageModelCapability{}, false
	}
	return catalog[index], true
}

func ValidateVideoModelOverrides(provider, modelName string, overrides VideoModelOverrides) error {
	for _, binding := range mediaVideoCandidates(modelName) {
		if binding.Plugin == nil || binding.Plugin.Meta.Key != provider {
			continue
		}
		_, err := EffectiveVideoProfile(binding, modelName, overrides)
		return err
	}
	return fmt.Errorf("model %q is not declared by video provider %q", modelName, provider)
}

func ValidateVideoProfileRequest(profile jsplugin.VideoProfile, action string, request any, files []map[string]any) error {
	modeIndex := slices.IndexFunc(profile.Modes, func(mode jsplugin.VideoModeProfile) bool { return mode.Name == action })
	if modeIndex < 0 {
		return fmt.Errorf("video mode %q is not enabled", action)
	}
	mode := profile.Modes[modeIndex]
	body, ok := request.(map[string]any)
	if !ok {
		return fmt.Errorf("video request is unavailable")
	}
	metadata, _ := body["metadata"].(map[string]any)
	hasField := func(names ...string) bool {
		for _, name := range names {
			if _, exists := body[name]; exists {
				return true
			}
			if _, exists := metadata[name]; exists {
				return true
			}
		}
		return false
	}
	if action == "edit_video" || action == "extend_video" {
		prompt, _ := body["prompt"].(string)
		if strings.TrimSpace(prompt) == "" {
			return fmt.Errorf("video prompt is required")
		}
		if hasField("source_task_id") {
			return fmt.Errorf("video source_task_id is not supported")
		}
	}
	if action == "extend_video" && hasField("duration") && hasField("seconds") {
		return fmt.Errorf("video duration and seconds cannot both be provided")
	}
	durationValue := firstVideoValue(body, metadata, "duration", "seconds")
	if mode.Duration == nil {
		if hasField("duration", "seconds") {
			return fmt.Errorf("video duration is fixed by the provider or source")
		}
	} else {
		duration := mode.Duration.Default
		if durationValue != nil {
			number, valid := videoNumber(durationValue)
			if !valid || number != math.Trunc(number) {
				return fmt.Errorf("video duration must be an integer")
			}
			duration = int(number)
		}
		if len(mode.Duration.Values) > 0 {
			if !slices.Contains(mode.Duration.Values, duration) {
				return fmt.Errorf("video duration is outside the enabled capability")
			}
		} else if duration < mode.Duration.Min || duration > mode.Duration.Max || (duration-mode.Duration.Min)%mode.Duration.Step != 0 {
			return fmt.Errorf("video duration is outside the enabled capability")
		}
	}
	if len(mode.Resolutions) == 0 {
		if hasField("resolution", "size") {
			return fmt.Errorf("video resolution is fixed by the provider or source")
		}
	} else {
		resolution := videoStringValue(body, metadata, "resolution")
		if resolution == "" {
			resolution = NormalizeVideoResolution(videoStringValue(body, metadata, "size"))
		}
		if resolution == "" {
			resolution = mode.DefaultResolution
		}
		resolution = strings.ToLower(resolution)
		if !slices.Contains(mode.Resolutions, resolution) {
			return fmt.Errorf("video resolution %q is not enabled", resolution)
		}
	}
	if len(mode.AspectRatios) > 0 {
		aspectRatio := videoStringValue(body, metadata, "aspect_ratio")
		if aspectRatio == "" {
			aspectRatio = videoStringValue(body, metadata, "ratio")
		}
		if aspectRatio == "" {
			aspectRatio = mode.DefaultAspectRatio
		}
		if !slices.Contains(mode.AspectRatios, aspectRatio) {
			return fmt.Errorf("video aspect ratio %q is not enabled", aspectRatio)
		}
	} else if hasField("aspect_ratio", "ratio") {
		return fmt.Errorf("video aspect ratio is fixed by the provider or source")
	}
	if action == "extend_video" {
		direction := videoStringValue(body, metadata, "extension_direction")
		if direction == "" {
			direction = mode.DefaultExtensionDirection
		}
		if !slices.Contains(mode.ExtensionDirections, direction) {
			return fmt.Errorf("video extension direction %q is not enabled", direction)
		}
	} else if hasField("extension_direction") {
		return fmt.Errorf("video extension direction is only valid for extension requests")
	}
	counts := videoInputCounts(body, metadata, files)
	declaredInputs := make(map[string]jsplugin.VideoInputProfile, len(mode.Inputs))
	for _, input := range mode.Inputs {
		declaredInputs[input.Name] = input
		sources := videoInputSources(input.Name, body, metadata, files)
		count, knownInput := counts[input.Name]
		if !knownInput {
			count = len(sources)
			counts[input.Name] = count
		}
		if input.Required && count == 0 {
			return fmt.Errorf("video input %q is required", input.Name)
		}
		if count > input.MaxItems {
			return fmt.Errorf("video input %q exceeds the enabled limit", input.Name)
		}
		for _, source := range sources {
			if !slices.Contains(input.Sources, source) {
				return fmt.Errorf("video input %q does not allow source %q", input.Name, source)
			}
		}
	}
	for name, count := range counts {
		if count > 0 {
			if _, declared := declaredInputs[name]; !declared {
				return fmt.Errorf("video input %q is not enabled", name)
			}
		}
	}
	for field, supported := range map[string]bool{"generate_audio": mode.Options.GenerateAudio, "watermark": mode.Options.Watermark, "return_last_frame": mode.Options.ReturnLastFrame} {
		value := firstVideoValue(body, metadata, field)
		if enabled, exists := value.(bool); exists && enabled && !supported {
			return fmt.Errorf("video option %q is not enabled", field)
		}
	}
	return nil
}

func videoInputSources(name string, body, metadata map[string]any, files []map[string]any) []string {
	values := make([]any, 0)
	appendValues := func(value any) {
		switch items := value.(type) {
		case []any:
			values = append(values, items...)
		case []string:
			for _, item := range items {
				values = append(values, item)
			}
		case nil:
		default:
			values = append(values, value)
		}
	}
	switch name {
	case "image":
		if body["image"] != nil {
			appendValues(body["image"])
		} else {
			appendValues(body["input_reference"])
		}
		if images, ok := body["images"].([]any); ok && len(images) > 0 && body["image"] == nil && body["input_reference"] == nil {
			appendValues(images[0])
		}
	case "reference_images":
		appendValues(body[name])
		if images, ok := body["images"].([]any); ok {
			start := 0
			if body["image"] == nil && body["input_reference"] == nil && len(images) > 0 {
				start = 1
			}
			for _, image := range images[start:] {
				appendValues(image)
			}
		}
	default:
		appendValues(body[name])
	}
	if content, ok := metadata["content"].([]any); ok {
		firstImageAssigned := videoValueCount(body["image"]) > 0 || videoValueCount(body["input_reference"]) > 0 || videoValueCount(body["images"]) > 0
		for _, raw := range content {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := item["role"].(string)
			typeName, _ := item["type"].(string)
			target := ""
			switch {
			case role == "first_frame":
				target, firstImageAssigned = "image", true
			case role == "last_frame":
				target = "last_frame"
			case role == "" && typeName == "image_url" && !firstImageAssigned:
				target, firstImageAssigned = "image", true
			case role == "reference_image" || typeName == "image_url":
				target = "reference_images"
			case role == "reference_video" || typeName == "video_url":
				target = "reference_videos"
			case role == "reference_audio" || typeName == "audio_url":
				target = "reference_audios"
			}
			matches := name == target
			if !matches {
				continue
			}
			value := item["url"]
			for _, field := range []string{"image_url", "video_url", "audio_url"} {
				if item[field] != nil {
					value = item[field]
					break
				}
			}
			appendValues(value)
		}
	}
	for _, file := range files {
		field, _ := file["field"].(string)
		if field == name || name == "image" && (field == "image" || field == "input_reference") {
			values = append(values, videoUploadedFileSource{})
		}
	}
	sources := make([]string, 0, len(values))
	for _, value := range values {
		sources = append(sources, classifyVideoInputSource(value, name))
	}
	return sources
}

type videoUploadedFileSource struct{}

func classifyVideoInputSource(value any, inputName string) string {
	if _, uploaded := value.(videoUploadedFileSource); uploaded {
		return "upload"
	}
	if object, ok := value.(map[string]any); ok {
		if text, _ := object["file_id"].(string); strings.TrimSpace(text) != "" {
			return "file_id"
		}
		if text, _ := object["voice_id"].(string); strings.TrimSpace(text) != "" {
			return "voice_id"
		}
		for _, field := range []string{"url", "image_url", "video_url", "audio_url"} {
			if nested := object[field]; nested != nil {
				return classifyVideoInputSource(nested, inputName)
			}
		}
	}
	text, _ := value.(string)
	text = strings.ToLower(strings.TrimSpace(text))
	switch {
	case strings.HasPrefix(text, "data:"):
		return "data_uri"
	case strings.HasPrefix(text, "asset://"):
		return "asset"
	case strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://"):
		return "url"
	case inputName == "reference_audios" && text != "":
		return "voice_id"
	default:
		return "unknown"
	}
}

func firstVideoValue(body, metadata map[string]any, names ...string) any {
	for _, name := range names {
		if value, exists := body[name]; exists {
			return value
		}
		if value, exists := metadata[name]; exists {
			return value
		}
	}
	return nil
}

func videoNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, !math.IsNaN(number) && !math.IsInf(number, 0)
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case string:
		parsed, err := strconv.ParseFloat(number, 64)
		return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	default:
		return 0, false
	}
}

func videoStringValue(body, metadata map[string]any, name string) string {
	value := firstVideoValue(body, metadata, name)
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func videoValueCount(value any) int {
	switch items := value.(type) {
	case []any:
		return len(items)
	case []string:
		return len(items)
	case nil:
		return 0
	case string:
		if strings.TrimSpace(items) == "" {
			return 0
		}
		return 1
	default:
		return 1
	}
}

func videoInputCounts(body, metadata map[string]any, files []map[string]any) map[string]int {
	counts := map[string]int{
		"video":            videoValueCount(body["video"]),
		"reference_images": videoValueCount(body["reference_images"]),
		"reference_videos": videoValueCount(body["reference_videos"]),
		"reference_audios": videoValueCount(body["reference_audios"]),
		"last_frame":       videoValueCount(body["last_frame"]),
	}
	counts["image"] = max(videoValueCount(body["image"]), videoValueCount(body["input_reference"]))
	if images := videoValueCount(body["images"]); images > 0 {
		if counts["image"] == 0 {
			counts["image"] = 1
			images--
		}
		counts["reference_images"] += images
	}
	if content, ok := metadata["content"].([]any); ok {
		firstImageAssigned := counts["image"] > 0
		for _, raw := range content {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := item["role"].(string)
			typeName, _ := item["type"].(string)
			switch {
			case role == "first_frame":
				counts["image"]++
				firstImageAssigned = true
			case role == "last_frame":
				counts["last_frame"]++
			case role == "" && typeName == "image_url" && !firstImageAssigned:
				counts["image"]++
				firstImageAssigned = true
			case role == "reference_video" || typeName == "video_url":
				counts["reference_videos"]++
			case role == "reference_audio" || typeName == "audio_url":
				counts["reference_audios"]++
			case role == "reference_image" || typeName == "image_url":
				counts["reference_images"]++
			}
		}
	}
	for _, file := range files {
		field, _ := file["field"].(string)
		switch field {
		case "image", "input_reference":
			counts["image"]++
		case "last_frame", "reference_images", "reference_videos", "reference_audios":
			counts[field]++
		}
	}
	return counts
}

func NormalizeVideoResolution(value string) string {
	raw := strings.ToLower(strings.TrimSpace(value))
	if raw == "480p" || raw == "720p" || raw == "1080p" || raw == "2k" || raw == "4k" {
		return raw
	}
	left, right, ok := strings.Cut(strings.ReplaceAll(raw, "*", "x"), "x")
	width, widthErr := strconv.ParseUint(left, 10, 32)
	height, heightErr := strconv.ParseUint(right, 10, 32)
	if !ok || widthErr != nil || heightErr != nil || width == 0 || height == 0 {
		return ""
	}
	edge := max(width, height)
	if edge >= 3840 {
		return "4k"
	}
	if edge >= 1920 {
		return "1080p"
	}
	if edge >= 1280 {
		return "720p"
	}
	return "480p"
}

func VideoPoolResolution(size string) string {
	switch NormalizeVideoResolution(size) {
	case "1080p", "2k":
		return "2K"
	case "4k":
		return "4K"
	case "480p", "720p":
		return "1K"
	}
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
