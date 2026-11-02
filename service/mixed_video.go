package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// MixedVideoRouting is server-owned admission state. Provider continues to name
// the actual executing plugin, while this identity controls pool permissions.
type MixedVideoRouting struct {
	Group      string `json:"group"`
	BindingKey string `json:"binding_key"`
	PoolMode   string `json:"pool_mode"`
	Model      string `json:"model"`
	Operation  string `json:"operation"`
	Resolution string `json:"resolution"`
}

type MixedVideoCandidate struct {
	Channel       model.Channel
	Binding       jsplugin.ProtocolBinding
	UpstreamModel string
	Intent        map[string]any
}

func MixedVideoPolicyCapability(catalog []ImageModelCapability, publicModel string) (ImageModelCapability, bool) {
	index := slices.IndexFunc(catalog, func(capability ImageModelCapability) bool {
		return capability.Id == publicModel && capability.MediaType == "video"
	})
	if index < 0 {
		return ImageModelCapability{}, false
	}
	return catalog[index], true
}

// MixedVideoChannelBinding resolves aliases inside one explicit pool member;
// global aliases deliberately remain unambiguous for all other relay paths.
func MixedVideoChannelBinding(channel model.Channel, publicModel, operation string) (jsplugin.ProtocolBinding, string, error) {
	upstream, err := ResolveImageChannelUpstreamModel(channel, publicModel)
	if err != nil {
		return jsplugin.ProtocolBinding{}, "", err
	}
	path := "/v1/videos"
	switch operation {
	case "", "create":
	case "edit":
		path += "/edits"
	case "extend":
		path += "/extensions"
	default:
		return jsplugin.ProtocolBinding{}, "", errors.New("invalid mixed video operation")
	}
	generation := jsplugin.DefaultRegistry.Generation()
	if generation == nil {
		return jsplugin.ProtocolBinding{}, "", errors.New("video plugins are unavailable")
	}
	for _, binding := range generation.LookupEndpointCandidates(http.MethodPost, path, upstream) {
		if binding.Plugin == nil || binding.Protocol != "openai_video" {
			continue
		}
		filter := dto.ChannelFilter{Kind: dto.FilterTaskPluginIdentity, TaskPluginKey: binding.Plugin.Meta.Key, TaskPluginChannelTypes: binding.Plugin.Meta.ChannelTypes}
		if matches, _ := model.ChannelSatisfiesFilters(&channel, publicModel, []dto.ChannelFilter{filter}); matches {
			return binding, upstream, nil
		}
	}
	return jsplugin.ProtocolBinding{}, "", fmt.Errorf("channel %d has no video protocol for mapped model %q", channel.Id, upstream)
}

// EffectiveMixedVideoProfile intersects a member with the public allowlist.
// Unlike provider overrides, a mixed allowlist may include other members' modes.
func EffectiveMixedVideoProfile(binding jsplugin.ProtocolBinding, publicModel string, overrides VideoModelOverrides) (jsplugin.VideoProfile, error) {
	profile, err := EffectiveVideoProfile(binding, publicModel, VideoModelOverrides{})
	if err != nil {
		return profile, err
	}
	modes := make([]jsplugin.VideoModeProfile, 0, len(profile.Modes))
	for _, mode := range profile.Modes {
		if overrides.Modes != nil && !slices.Contains(overrides.Modes, mode.Name) {
			continue
		}
		if overrides.Resolutions != nil && len(mode.Resolutions) > 0 {
			mode.Resolutions = slices.DeleteFunc(mode.Resolutions, func(value string) bool { return !slices.Contains(overrides.Resolutions, value) })
			if len(mode.Resolutions) == 0 {
				continue
			}
		}
		if overrides.AspectRatios != nil && len(mode.AspectRatios) > 0 {
			mode.AspectRatios = slices.DeleteFunc(mode.AspectRatios, func(value string) bool { return !slices.Contains(overrides.AspectRatios, value) })
			if len(mode.AspectRatios) == 0 {
				continue
			}
		}
		if overrides.ExtensionDirections != nil && len(mode.ExtensionDirections) > 0 {
			mode.ExtensionDirections = slices.DeleteFunc(mode.ExtensionDirections, func(value string) bool { return !slices.Contains(overrides.ExtensionDirections, value) })
			if len(mode.ExtensionDirections) == 0 {
				continue
			}
		}
		if overrides.Duration != nil && mode.Duration != nil {
			values := make([]int, 0)
			for value := 1; value <= relaycommon.MaxTaskDurationSeconds; value++ {
				allowed := slices.Contains(mode.Duration.Values, value)
				if len(mode.Duration.Values) == 0 {
					allowed = value >= mode.Duration.Min && value <= mode.Duration.Max && mode.Duration.Step > 0 && (value-mode.Duration.Min)%mode.Duration.Step == 0
				}
				if !allowed || overrides.Duration.Min != nil && value < *overrides.Duration.Min || overrides.Duration.Max != nil && value > *overrides.Duration.Max || overrides.Duration.Values != nil && !slices.Contains(overrides.Duration.Values, value) {
					continue
				}
				values = append(values, value)
			}
			if len(values) == 0 {
				continue
			}
			mode.Duration.Values, mode.Duration.Min, mode.Duration.Max, mode.Duration.Step = values, 0, 0, 0
			if overrides.Duration.Default != nil && slices.Contains(values, *overrides.Duration.Default) {
				mode.Duration.Default = *overrides.Duration.Default
			}
			if !slices.Contains(values, mode.Duration.Default) {
				mode.Duration.Default = values[0]
			}
		}
		if len(mode.Resolutions) > 0 {
			if slices.Contains(mode.Resolutions, overrides.DefaultResolution) {
				mode.DefaultResolution = overrides.DefaultResolution
			}
			if !slices.Contains(mode.Resolutions, mode.DefaultResolution) {
				mode.DefaultResolution = mode.Resolutions[0]
			}
		}
		if len(mode.AspectRatios) > 0 {
			if slices.Contains(mode.AspectRatios, overrides.DefaultAspectRatio) {
				mode.DefaultAspectRatio = overrides.DefaultAspectRatio
			}
			if !slices.Contains(mode.AspectRatios, mode.DefaultAspectRatio) {
				mode.DefaultAspectRatio = mode.AspectRatios[0]
			}
		}
		if len(mode.ExtensionDirections) > 0 {
			if slices.Contains(mode.ExtensionDirections, overrides.DefaultExtensionDirection) {
				mode.DefaultExtensionDirection = overrides.DefaultExtensionDirection
			}
			if !slices.Contains(mode.ExtensionDirections, mode.DefaultExtensionDirection) {
				mode.DefaultExtensionDirection = mode.ExtensionDirections[0]
			}
		}
		for index := range mode.Inputs {
			if limit, ok := overrides.MaxInputItems[mode.Inputs[index].Name]; ok {
				mode.Inputs[index].MaxItems = min(mode.Inputs[index].MaxItems, limit)
			}
		}
		modes = append(modes, mode)
	}
	profile.Modes = modes
	if len(modes) == 0 {
		return profile, errors.New("channel has no mode in the mixed video allowlist")
	}
	return profile, nil
}

// MixedVideoPublicFacts is deliberately strict: defaults must be public and
// independent of the eventual provider. No provider usage becomes retail usage.
func MixedVideoPublicFacts(body map[string]any, operation string, profiles ...jsplugin.VideoProfile) (map[string]any, error) {
	action := operation
	if action == "" || action == "create" {
		action = "generate"
	}
	facts := map[string]any{"requests": float64(1), "action": action}
	metadata, _ := body["metadata"].(map[string]any)
	if _, _, err := mixedVideoAspectRatio(body, metadata, profiles...); err != nil {
		return nil, err
	}
	for index, source := range []map[string]any{body, metadata} {
		for _, key := range []string{"seconds", "duration", "durationSeconds", "video_duration"} {
			if raw, supplied := source[key]; supplied {
				seconds, ok := videoNumber(raw)
				if !ok || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds != math.Trunc(seconds) || seconds < 1 || seconds > relaycommon.MaxTaskDurationSeconds {
					return nil, errors.New("mixed video duration must be a bounded positive whole number")
				}
				if index == 0 && key == "duration" && mixedVideoDurationUnitControl(body) {
					continue
				}
				if previous, present := facts["seconds"]; present && previous != seconds {
					return nil, errors.New("conflicting mixed video durations")
				}
				facts["seconds"] = seconds
			}
		}
		for _, key := range []string{"resolution", "size"} {
			if raw, supplied := source[key]; supplied {
				value, ok := raw.(string)
				resolution := NormalizeVideoResolution(value)
				if ok && resolution == "" && strings.EqualFold(strings.TrimSpace(value), "720px") {
					// 720px is a model-specific wire value. Only a profile from an
					// actual bound candidate can give it a public billing meaning.
					for _, profile := range profiles {
						if slices.ContainsFunc(profile.Modes, func(mode jsplugin.VideoModeProfile) bool {
							return slices.Contains(mode.Resolutions, "720px")
						}) {
							resolution = "720p"
							break
						}
					}
				}
				if !ok || resolution == "" {
					return nil, errors.New("mixed video output resolution is invalid")
				}
				if previous, present := facts["resolution"]; present && previous != resolution {
					return nil, errors.New("conflicting mixed video resolutions")
				}
				facts["resolution"] = resolution
			}
		}
	}
	return facts, nil
}

// Tianyue's native per-operation request uses duration=1 as a billing unit;
// video_duration remains the output seconds. Candidate admission separately
// restricts this native combination to the matching Tianyue billing mode.
func mixedVideoDurationUnitControl(body map[string]any) bool {
	unit, hasUnit := videoNumber(body["duration"])
	seconds, hasSeconds := videoNumber(body["video_duration"])
	return hasUnit && hasSeconds && unit == 1 && seconds > 1
}

// mixedVideoValidationBody changes only the validation view, keeping the
// provider's original request intact. Final admission requires the decoder to
// demonstrate the same media identity in the projected role. A nil decoded
// body is used only to discover a possible public variant before decoding.
func mixedVideoValidationBody(mode jsplugin.VideoModeProfile, body, decoded map[string]any) map[string]any {
	view := maps.Clone(body)
	hasPrimary := slices.ContainsFunc(mode.Inputs, func(input jsplugin.VideoInputProfile) bool { return input.Name == "image" })
	hasReferences := slices.ContainsFunc(mode.Inputs, func(input jsplugin.VideoInputProfile) bool { return input.Name == "reference_images" })
	primary := body["image"]
	if primary == nil {
		primary = body["input_reference"]
	}
	if hasReferences && !hasPrimary && primary != nil {
		identity := videoMediaIdentity(primary)
		if decoded == nil || identity != "" && slices.ContainsFunc(videoMediaValues("reference_images", decoded), func(value any) bool { return videoMediaIdentity(value) == identity }) {
			references := videoInputValues("reference_images", map[string]any{"reference_images": body["reference_images"]}, nil)
			view["reference_images"] = append(references, primary)
			delete(view, "image")
			delete(view, "input_reference")
		}
	}
	if hasPrimary && !hasReferences && len(videoMediaValues("image", body)) == 0 {
		references := videoInputValues("reference_images", map[string]any{"reference_images": body["reference_images"]}, nil)
		if len(references) == 1 {
			identity := videoMediaIdentity(references[0])
			if decoded == nil || identity != "" && slices.ContainsFunc(videoMediaValues("image", decoded), func(value any) bool { return videoMediaIdentity(value) == identity }) {
				view["image"] = references[0]
				delete(view, "reference_images")
			}
		}
	}
	if mixedVideoDurationUnitControl(body) {
		view["seconds"], _ = videoNumber(body["video_duration"])
		delete(view, "duration")
	}
	return view
}

func mixedVideoAspectRatio(body, metadata map[string]any, profiles ...jsplugin.VideoProfile) (string, bool, error) {
	ratio, supplied := "", false
	for _, source := range []map[string]any{body, metadata} {
		for _, name := range []string{"aspect_ratio", "ratio", "aspectRatio"} {
			value, present := source[name]
			if !present {
				continue
			}
			text, valid := value.(string)
			if !valid || text == "" || text != strings.TrimSpace(text) {
				return "", false, errors.New("mixed video aspect ratio must be a non-empty string")
			}
			if supplied && ratio != text {
				return "", false, errors.New("conflicting mixed video aspect ratios")
			}
			ratio, supplied = text, true
		}
	}
	pixelRatio := ""
	for _, source := range []map[string]any{body, metadata} {
		size, _ := source["size"].(string)
		left, right, pixels := strings.Cut(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(size)), "*", "x"), "x")
		width, widthErr := strconv.ParseUint(left, 10, 32)
		height, heightErr := strconv.ParseUint(right, 10, 32)
		if !pixels || widthErr != nil || heightErr != nil || width == 0 || height == 0 {
			continue
		}
		inferred := "16:9"
		if height > width {
			inferred = "9:16"
		}
		// Keep exact shapes supported by a member (for example xAI's square),
		// otherwise preserve Google's established landscape/portrait size map.
		divisor, remainder := width, height
		for remainder != 0 {
			divisor, remainder = remainder, divisor%remainder
		}
		exact := strconv.FormatUint(width/divisor, 10) + ":" + strconv.FormatUint(height/divisor, 10)
		if supplied && ratio == exact || slices.ContainsFunc(profiles, func(profile jsplugin.VideoProfile) bool {
			return slices.ContainsFunc(profile.Modes, func(mode jsplugin.VideoModeProfile) bool { return slices.Contains(mode.AspectRatios, exact) })
		}) {
			inferred = exact
		}
		if pixelRatio != "" && pixelRatio != inferred || supplied && ratio != inferred {
			return "", false, errors.New("conflicting mixed video size aspect ratios")
		}
		pixelRatio = inferred
	}
	if !supplied && pixelRatio != "" {
		return pixelRatio, true, nil
	}
	return ratio, supplied, nil
}

// DecodeMixedVideoCandidate runs each member's decoder against its own mapping.
// Returned objects belong to this call and are never reused by another plugin.
func DecodeMixedVideoCandidate(ctx context.Context, channel model.Channel, publicModel, operation string, raw []byte, overrides VideoModelOverrides, userID int, contentTypes ...string) (MixedVideoCandidate, error) {
	var candidate MixedVideoCandidate
	binding, upstream, err := MixedVideoChannelBinding(channel, publicModel, operation)
	if err != nil {
		return candidate, err
	}
	contentType := "application/json"
	if len(contentTypes) > 0 {
		contentType = contentTypes[0]
	}
	view, body, err := MixedVideoRequestContext(raw, contentType, operation)
	if err != nil {
		return candidate, err
	}
	if body["model"] != publicModel {
		return candidate, errors.New("mixed video public model changed")
	}
	profile, err := EffectiveMixedVideoProfile(binding, publicModel, overrides)
	if err != nil {
		return candidate, err
	}
	if _, err := MixedVideoPublicFacts(body, operation, profile); err != nil {
		return candidate, err
	}
	if mixedVideoDurationUnitControl(body) && (binding.Plugin.Meta.Key != "tianyue" || strings.HasSuffix(upstream, "-se")) {
		return candidate, errors.New("native duration unit requires a Tianyue per-operation channel")
	}
	if mixedVideoAccountReference(body) {
		return candidate, errors.New("account-bound video assets require an explicit provider and channel")
	}
	request := jsplugin.ProtocolRequestContext{RouteRequestContext: view, Protocol: binding.Protocol, Operation: binding.Operation.Name, Model: publicModel, UpstreamModel: upstream}
	value, err := binding.Plugin.Engine.CallPathWithAdmissionTimeout(ctx, jsplugin.DefaultCallTimeout, "protocols", []string{binding.Protocol, "decodeRequest"}, request.JSValue())
	if err != nil {
		return candidate, err
	}
	intent, ok := value.(map[string]any)
	if !ok || intent["kind"] != "submit" || intent["model"] != publicModel {
		return candidate, errors.New("video decoder rejected the public model")
	}
	if _, forbidden := intent["renderer"]; forbidden {
		return candidate, errors.New("video decoder returned an invalid intent")
	}
	action, _ := intent["action"].(string)
	if action == "" {
		action = "text_to_video"
	}
	modeIndex := slices.IndexFunc(profile.Modes, func(mode jsplugin.VideoModeProfile) bool { return mode.Name == action })
	if modeIndex < 0 {
		return candidate, fmt.Errorf("video mode %q is not enabled", action)
	}
	decoded, _ := intent["requestBody"].(map[string]any)
	if err := ValidateVideoProfileRequest(profile, action, mixedVideoValidationBody(profile.Modes[modeIndex], body, decoded), view.Files); err != nil {
		return candidate, err
	}
	if normalized, present := intent["requestBody"]; present {
		if err := ValidateVideoProfileRequest(profile, action, normalized, view.Files); err != nil {
			return candidate, err
		}
		if normalizedBody, ok := normalized.(map[string]any); ok {
			if binding.Plugin.Meta.Key == "sora" {
				// Sora's larger native sizes have no equivalent public resolution;
				// generic pixel bucketing would incorrectly price them as 720p.
				size, _ := normalizedBody["size"].(string)
				if size != "1280x720" && size != "720x1280" {
					return candidate, errors.New("Sora mixed routing requires a 1280x720 or 720x1280 output size")
				}
			}
			if !maps.Equal(videoMediaInventory(body, view.Files), videoMediaInventory(normalizedBody, view.Files)) {
				return candidate, errors.New("video decoder changed the input media identities or counts")
			}
			publicFacts, err := MixedVideoPublicFacts(body, operation, profile)
			if err != nil {
				return candidate, err
			}
			decodedFacts, err := MixedVideoPublicFacts(normalizedBody, operation, profile)
			if err != nil {
				return candidate, err
			}
			for _, field := range []string{"seconds", "resolution"} {
				if expected, present := publicFacts[field]; present {
					if actual, decoded := decodedFacts[field]; decoded && actual != expected {
						return candidate, fmt.Errorf("video decoder changed the public %s", field)
					}
				}
			}
		}
	}
	if ids, present := intent["originTaskIds"]; present {
		list, ok := ids.([]any)
		if !ok {
			return candidate, errors.New("invalid video origin tasks")
		}
		for _, value := range list {
			id, ok := value.(string)
			if !ok || id == "" {
				return candidate, errors.New("invalid video origin task")
			}
			var origin model.Task
			if userID <= 0 || model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ? AND channel_id = ? AND platform = ?", id, userID, channel.Id, binding.Plugin.Meta.Key).Take(&origin).Error != nil {
				return candidate, errors.New("origin video is not available on this channel")
			}
		}
	}
	return MixedVideoCandidate{Channel: channel, Binding: binding, UpstreamModel: upstream, Intent: intent}, nil
}

// MixedVideoRequestContext parses the existing public JSON/multipart formats
// without storing file bytes in plugin objects. The same parser is used again
// for frozen retries so a multipart job can move between compatible plugins.
func MixedVideoRequestContext(raw []byte, contentType, operation string) (jsplugin.RouteRequestContext, map[string]any, error) {
	view := jsplugin.RouteRequestContext{Method: http.MethodPost, Path: "/v1/videos"}
	if operation == "edit" {
		view.Path += "/edits"
	}
	if operation == "extend" {
		view.Path += "/extensions"
	}
	if len(raw) > 64<<20 {
		return view, nil, errors.New("video request exceeds the byte limit")
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return view, nil, err
	}
	body := make(map[string]any)
	if mediaType == "application/json" {
		if err := common.Unmarshal(raw, &body); err != nil {
			return view, nil, err
		}
		view.Body = map[string]any{"kind": "json", "value": body}
		return view, body, nil
	}
	if mediaType != "multipart/form-data" || operation != "" && operation != "create" {
		return view, nil, errors.New("unsupported mixed video request body")
	}
	fields := make(map[string][]string)
	reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	fileCounts := make(map[string]int)
	for count := 0; ; count++ {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return view, nil, err
		}
		if count >= 256 {
			part.Close()
			return view, nil, errors.New("video multipart has too many parts")
		}
		name := part.FormName()
		if name == "" || len(name) > 256 {
			part.Close()
			return view, nil, errors.New("invalid video multipart field")
		}
		if part.FileName() == "" {
			data, err := io.ReadAll(io.LimitReader(part, (1<<20)+1))
			part.Close()
			if err != nil || len(data) > 1<<20 || len(fields[name]) > 0 {
				return view, nil, errors.New("invalid or duplicate video multipart field")
			}
			fields[name] = []string{string(data)}
			var value any
			if (strings.HasPrefix(strings.TrimSpace(string(data)), "{") || strings.HasPrefix(strings.TrimSpace(string(data)), "[")) && common.Unmarshal(data, &value) == nil {
				body[name] = value
			} else {
				body[name] = string(data)
			}
			continue
		}
		size, err := io.Copy(io.Discard, part)
		part.Close()
		if err != nil || len(view.Files) >= 32 {
			return view, nil, errors.New("invalid video multipart file")
		}
		ref := "request_file:" + name
		if fileCounts[name] > 0 {
			ref += ":" + strconv.Itoa(fileCounts[name])
		}
		fileCounts[name]++
		view.Files = append(view.Files, map[string]any{"ref": ref, "field": name, "filename": part.FileName(), "mimeType": part.Header.Get("Content-Type"), "size": size})
	}
	view.Body = map[string]any{"kind": "multipart", "fields": fields, "files": view.Files}
	return view, body, nil
}

// NormalizeMixedVideoDefaults chooses one complete public capability variant,
// materializes its defaults, then gives the identical request to every member.
func NormalizeMixedVideoDefaults(raw []byte, contentType, operation string, profiles []jsplugin.VideoProfile) ([]byte, string, map[string]any, error) {
	view, original, err := MixedVideoRequestContext(raw, contentType, operation)
	if err != nil {
		return nil, "", nil, err
	}
	facts, err := MixedVideoPublicFacts(original, operation, profiles...)
	if err != nil {
		return nil, "", nil, err
	}
	metadata, _ := original["metadata"].(map[string]any)
	aspectRatio, hasAspectRatio, err := mixedVideoAspectRatio(original, metadata, profiles...)
	if err != nil {
		return nil, "", nil, err
	}
	var normalized map[string]any
	for _, profile := range profiles {
		for _, mode := range profile.Modes {
			if operation == "edit" && mode.Name != "edit_video" || operation == "extend" && mode.Name != "extend_video" || (operation == "create" || operation == "") && (mode.Name == "edit_video" || mode.Name == "extend_video") {
				continue
			}
			data, err := common.Marshal(original)
			if err != nil {
				return nil, "", nil, err
			}
			var body map[string]any
			if err := common.Unmarshal(data, &body); err != nil {
				return nil, "", nil, err
			}
			metadata, _ := body["metadata"].(map[string]any)
			if seconds, present := facts["seconds"]; present {
				if _, supplied := body["seconds"]; !supplied {
					if _, supplied := body["duration"]; !supplied || mixedVideoDurationUnitControl(body) {
						body["seconds"] = seconds
					}
				}
			} else if mode.Duration != nil {
				body["seconds"] = float64(mode.Duration.Default)
			}
			if _, present := facts["resolution"]; !present && len(mode.Resolutions) > 0 {
				body["resolution"] = mode.DefaultResolution
			}
			if hasAspectRatio && (len(mode.AspectRatios) > 0 || firstVideoValue(original, metadata, "aspect_ratio", "ratio", "aspectRatio") != nil) {
				body["aspect_ratio"] = aspectRatio
			} else if len(mode.AspectRatios) > 0 {
				body["aspect_ratio"] = mode.DefaultAspectRatio
			}
			if mode.Name == "extend_video" && firstVideoValue(body, metadata, "extension_direction") == nil && mode.DefaultExtensionDirection != "" {
				body["extension_direction"] = mode.DefaultExtensionDirection
			}
			if err := ValidateVideoProfileRequest(profile, mode.Name, mixedVideoValidationBody(mode, body, nil), view.Files); err != nil {
				continue
			}
			normalized = body
			break
		}
		if normalized != nil {
			break
		}
	}
	if normalized == nil {
		return nil, "", nil, errors.New("video request has no compatible public capability variant")
	}
	mediaType, params, _ := mime.ParseMediaType(contentType)
	if mediaType == "application/json" {
		encoded, err := common.Marshal(normalized)
		return encoded, contentType, normalized, err
	}
	var output bytes.Buffer
	writer := multipart.NewWriter(&output)
	reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", nil, err
		}
		destination, err := writer.CreatePart(part.Header)
		if err == nil {
			_, err = io.Copy(destination, part)
		}
		part.Close()
		if err != nil {
			return nil, "", nil, err
		}
	}
	for _, field := range []string{"seconds", "resolution", "aspect_ratio", "extension_direction"} {
		if value, present := normalized[field]; present {
			if _, existed := original[field]; !existed {
				if err := writer.WriteField(field, fmt.Sprint(value)); err != nil {
					return nil, "", nil, err
				}
			}
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", nil, err
	}
	return output.Bytes(), writer.FormDataContentType(), normalized, nil
}

func mixedVideoAccountReference(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.HasPrefix(typed, "asset://")
	case []any:
		return slices.ContainsFunc(typed, mixedVideoAccountReference)
	case map[string]any:
		for key, item := range typed {
			if (key == "file_id" || key == "asset_id" || key == "voice_id") && item != nil && item != "" {
				return true
			}
			if mixedVideoAccountReference(item) {
				return true
			}
		}
	}
	return false
}

// MixedVideoPoolChannels is shared by admission and recovery. Missing and empty
// bindings fail closed; no global channel weight can escape an explicit pool.
func MixedVideoPoolChannels(ctx context.Context, policy model.ImageGroupPolicy, publicModel, resolution string) ([]model.Channel, error) {
	var pools []model.ImageChannelPool
	if err := model.DB.WithContext(ctx).Where("binding_key = ? AND channel_id > ?", MediaPoolBindingKey(policy, publicModel, resolution, "video"), 0).Find(&pools).Error; err != nil {
		return nil, err
	}
	if len(pools) == 0 {
		return nil, nil
	}
	var abilities []model.Ability
	if err := model.DB.WithContext(ctx).Where(map[string]any{"group": policy.Group, "model": publicModel, "enabled": true}).Find(&abilities).Error; err != nil {
		return nil, err
	}
	ids := make([]int, 0)
	for _, pool := range pools {
		if slices.ContainsFunc(abilities, func(ability model.Ability) bool { return ability.ChannelId == pool.ChannelId }) {
			ids = append(ids, pool.ChannelId)
		}
	}
	var channels []model.Channel
	if len(ids) > 0 {
		if err := model.DB.WithContext(ctx).Where("id IN ? AND status = ?", ids, common.ChannelStatusEnabled).Order("id").Find(&channels).Error; err != nil {
			return nil, err
		}
	}
	for index := range channels {
		poolIndex := slices.IndexFunc(pools, func(pool model.ImageChannelPool) bool { return pool.ChannelId == channels[index].Id })
		ApplyMediaChannelPoolWeight(&channels[index], pools[poolIndex].Priority)
	}
	return channels, nil
}

func MixedVideoAttempt(candidate MixedVideoCandidate, generation int64) *model.VideoTaskAttempt {
	return &model.VideoTaskAttempt{Generation: generation, ChannelID: candidate.Channel.Id, PluginKey: candidate.Binding.Plugin.Meta.Key, PluginVersion: candidate.Binding.Plugin.Meta.Version, PluginHash: candidate.Binding.Plugin.Engine.SourceFingerprint(), UpstreamModel: candidate.UpstreamModel, MixedVideo: true}
}

// RebindMixedVideoRequest changes execution metadata without changing the
// original request, public routing identity or frozen retail reservation.
func RebindMixedVideoRequest(request *AsyncVideoRequest, candidate MixedVideoCandidate, expiresAt int64) error {
	request.Channel = candidate.Channel
	request.PluginHash = candidate.Binding.Plugin.Engine.SourceFingerprint()
	execution := &model.TaskExecutionSnapshot{}
	if request.Execution != nil {
		*execution = *request.Execution
	}
	meta := candidate.Binding.Plugin.Meta
	execution.TaskPlugin = &model.TaskPluginSnapshot{Key: meta.Key, Name: meta.Name, Version: meta.Version, Author: &model.TaskPluginAuthorSnapshot{Name: meta.Author.Name, URL: meta.Author.URL}, APIVersion: meta.APIVersion, Generation: jsplugin.DefaultRegistry.Generation().Number}
	request.Execution = execution
	replay := *request
	if len(request.OriginalBody) > 0 {
		replay.Body = request.OriginalBody
	}
	replay.ReferenceInputs, replay.ReferenceLocalizationUnsupported = nil, false
	if err := freezeVideoRetryInputs(&replay, expiresAt); err != nil {
		return err
	}
	request.ReferenceInputs, request.ReferenceLocalizationUnsupported = replay.ReferenceInputs, replay.ReferenceLocalizationUnsupported
	request.ReferenceRecoveryVersion = replay.ReferenceRecoveryVersion
	if request.ReferenceRecoveryDeadline == 0 {
		request.ReferenceRecoveryDeadline = replay.ReferenceRecoveryDeadline
	} else {
		request.ReferenceRecoveryDeadline = min(request.ReferenceRecoveryDeadline, replay.ReferenceRecoveryDeadline)
	}
	if request.ReferenceURLExpiresAt == 0 {
		request.ReferenceURLExpiresAt = replay.ReferenceURLExpiresAt
	} else {
		request.ReferenceURLExpiresAt = min(request.ReferenceURLExpiresAt, replay.ReferenceURLExpiresAt)
	}
	return nil
}

// MixedVideoPolicy verifies the frozen routing authority; newly enabled mixed
// configuration never takes over a historical provider-specific task.
func MixedVideoPolicy(ctx context.Context, token model.Token, route *MixedVideoRouting) (model.ImageGroupPolicy, []ImageModelCapability, error) {
	policy, catalog, err := ResolveImagePolicy(ctx, token, "mixed")
	if err != nil {
		return policy, catalog, err
	}
	var owner model.User
	if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", token.UserId, common.UserStatusEnabled).Take(&owner).Error; err != nil {
		return policy, catalog, err
	}
	_, selected := MixedVideoPolicyCapability(catalog, route.Model)
	if policy.Group == "auto" {
		groups, err := token.GetAutoGroups()
		if err != nil {
			return policy, catalog, err
		}
		if len(groups) > 0 {
			groups = FilterUserTokenAutoGroups(owner.Group, groups)
		} else {
			groups = GetUserAutoGroup(owner.Group)
		}
		if !slices.Contains(groups, route.Group) {
			return policy, catalog, errors.New("mixed video automatic group permission changed")
		}
		policy.Group = route.Group
	}
	if !selected || !policy.Enabled || !policy.AsyncEnabled || policy.Group != route.Group || !GroupInUserUsableGroups(owner.Group, route.Group) || MediaPoolBindingKey(policy, route.Model, route.Resolution, "video") != route.BindingKey {
		return policy, catalog, errors.New("mixed video routing permission changed")
	}
	return policy, catalog, nil
}
