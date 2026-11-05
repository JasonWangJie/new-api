package jsplugin

import (
	"maps"
	"reflect"
	"slices"
	"strings"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

// WithTaskVideoModels creates immutable channel-specific metadata while sharing
// the compiled engine. Existing pins continue to own their previous metadata.
func WithTaskVideoModels(plugin *LoadedPlugin, configs map[string]dto.TaskVideoModelConfig) *LoadedPlugin {
	if plugin == nil || !dto.ChannelConfiguredTaskVideoPlugin(plugin.Meta.Key) {
		return plugin
	}
	copy := *plugin
	meta := plugin.Meta
	keepsPresets := dto.PresetTaskVideoPlugin(meta.Key)
	if keepsPresets && copy.presetVideoMeta == nil {
		original := cloneMeta(meta)
		copy.presetVideoMeta = &original
	}
	copy.presetVideoAliases = maps.Clone(plugin.presetVideoAliases)
	if copy.presetVideoAliases == nil {
		copy.presetVideoAliases = map[string]bool{}
	}
	if !keepsPresets {
		meta.Models, meta.VideoProfiles, meta.UsageProfiles = nil, nil, nil
	}
	copy.Meta = cloneMeta(meta)
	names := make([]string, 0, len(configs))
	presets := make(map[string]string)
	legacyPresets := make(map[string]string)
	for name := range configs {
		if keepsPresets && len(configs[name].PresetModels) == 0 {
			preset := configs[name].PresetModel
			if preset == "" {
				preset = name
			}
			if canonical, legacy := LegacyTaskVideoModel(*copy.presetVideoMeta, preset); legacy && configs[name].PresetModel != "" {
				legacyPresets[name] = canonical
				if name == canonical {
					_, profiled := plugin.Meta.VideoProfileForModel(name)
					originalUsage, originalExamples := copy.presetVideoMeta.UsageForModel(canonical)
					currentUsage, currentExamples := plugin.Meta.UsageForModel(name)
					if !profiled && reflect.DeepEqual(originalUsage, currentUsage) && reflect.DeepEqual(originalExamples, currentExamples) {
						continue
					}
				}
			}
			if defaults, found := TaskVideoModelConfigFromMeta(*copy.presetVideoMeta, preset); found && TaskVideoModelConfigsEqual(defaults, configs[name]) {
				presets[name] = preset
				original, _ := copy.presetVideoMeta.VideoProfileForModel(preset)
				current, _ := plugin.Meta.VideoProfileForModel(name)
				originalUsage, originalExamples := copy.presetVideoMeta.UsageForModel(preset)
				currentUsage, currentExamples := plugin.Meta.UsageForModel(name)
				original.Models = []string{name}
				if reflect.DeepEqual(original, current) && reflect.DeepEqual(originalUsage, currentUsage) && reflect.DeepEqual(originalExamples, currentExamples) {
					copy.presetVideoAliases[name] = true
					continue
				}
			}
		}
		names = append(names, name)
	}
	if keepsPresets {
		for i := range copy.Meta.VideoProfiles {
			copy.Meta.VideoProfiles[i].Models = slices.DeleteFunc(copy.Meta.VideoProfiles[i].Models, func(name string) bool { return slices.Contains(names, name) })
		}
		copy.Meta.VideoProfiles = slices.DeleteFunc(copy.Meta.VideoProfiles, func(profile VideoProfile) bool { return len(profile.Models) == 0 })
		for i := range copy.Meta.UsageProfiles {
			copy.Meta.UsageProfiles[i].Models = slices.DeleteFunc(copy.Meta.UsageProfiles[i].Models, func(name string) bool { return slices.Contains(names, name) })
		}
		copy.Meta.UsageProfiles = slices.DeleteFunc(copy.Meta.UsageProfiles, func(profile UsageProfile) bool { return len(profile.Models) == 0 })
	}
	slices.Sort(names)
	for _, name := range names {
		if !slices.Contains(copy.Meta.Models, name) {
			copy.Meta.Models = append(copy.Meta.Models, name)
		}
		config := configs[name]
		delete(copy.presetVideoAliases, name)
		if preset, legacy := legacyPresets[name]; legacy {
			schema, examples := copy.presetVideoMeta.UsageForModel(preset)
			copy.Meta.UsageProfiles = append(copy.Meta.UsageProfiles, UsageProfile{Models: []string{name}, Schema: schema, Examples: examples})
			copy.presetVideoAliases[name] = true
			continue
		}
		var sourceProfiles []VideoProfile
		var sourceUsage []UsageProfile
		if keepsPresets && len(config.PresetModels) > 0 {
			legacySource := false
			for _, preset := range config.PresetModels {
				profile, found := copy.presetVideoMeta.VideoProfileForModel(preset)
				if !found {
					if _, legacy := LegacyTaskVideoModel(*copy.presetVideoMeta, preset); !legacy {
						continue
					}
					legacySource = true
				}
				schema, examples := copy.presetVideoMeta.UsageForModel(preset)
				sourceProfiles = append(sourceProfiles, profile)
				sourceUsage = append(sourceUsage, UsageProfile{Schema: schema, Examples: examples})
			}
			if legacySource {
				_, usage := unionTaskVideoProfiles(sourceProfiles, sourceUsage, name)
				copy.Meta.UsageProfiles = append(copy.Meta.UsageProfiles, usage)
				copy.presetVideoAliases[name] = true
				continue
			}
			if !config.CustomModelUnion {
				profile, usage := unionTaskVideoProfiles(sourceProfiles, sourceUsage, name)
				copy.Meta.VideoProfiles = append(copy.Meta.VideoProfiles, profile)
				copy.Meta.UsageProfiles = append(copy.Meta.UsageProfiles, usage)
				copy.presetVideoAliases[name] = true
				continue
			}
		}
		if preset, found := presets[name]; found {
			profile, _ := copy.presetVideoMeta.VideoProfileForModel(preset)
			profile.Models = []string{name}
			copy.Meta.VideoProfiles = append(copy.Meta.VideoProfiles, profile)
			schema, examples := copy.presetVideoMeta.UsageForModel(preset)
			copy.Meta.UsageProfiles = append(copy.Meta.UsageProfiles, UsageProfile{Models: []string{name}, Schema: schema, Examples: examples})
			copy.presetVideoAliases[name] = true
			continue
		}
		resolutions := make([]string, 0, len(config.Resolutions))
		for _, resolution := range config.Resolutions {
			resolution = strings.ToLower(resolution)
			if !slices.Contains(resolutions, resolution) {
				resolutions = append(resolutions, resolution)
			}
		}
		profile := VideoProfile{Models: []string{name}}
		duration := taskVideoDurationProfile(config.Duration)
		var durationsByResolution map[string]VideoDurationProfile
		if len(config.DurationsByResolution) > 0 {
			durationsByResolution = make(map[string]VideoDurationProfile, len(config.DurationsByResolution))
			for resolution, capability := range config.DurationsByResolution {
				durationsByResolution[resolution] = taskVideoDurationProfile(capability)
			}
		}
		imageSources := []string{"url"}
		if config.References.ImageDataURI {
			imageSources = append(imageSources, "data_uri")
		}
		if config.References.ImageUpload {
			imageSources = append(imageSources, "upload")
		}
		for _, name := range config.Modes {
			mode := VideoModeProfile{Name: name, Duration: &duration, Resolutions: slices.Clone(resolutions), AspectRatios: slices.Clone(config.AspectRatios), DurationsByResolution: durationsByResolution}
			if len(mode.Resolutions) > 0 {
				mode.DefaultResolution = mode.Resolutions[0]
			}
			if len(mode.AspectRatios) > 0 {
				mode.DefaultAspectRatio = mode.AspectRatios[0]
			}
			switch name {
			case "image_to_video":
				mode.Inputs = []VideoInputProfile{{Name: "image", Kind: "image", Sources: imageSources, MaxItems: 1, Required: true}}
			case "first_last_frame":
				// Huahuo enables inline frames separately from ordinary image references.
				frameSources := []string{"url"}
				mode.Inputs = []VideoInputProfile{{Name: "image", Kind: "image", Sources: frameSources, MaxItems: 1, Required: true}}
				if plugin.Meta.Key == "sudashui" || config.References.Images != nil && *config.References.Images >= 2 {
					mode.Inputs = append(mode.Inputs, VideoInputProfile{Name: "last_frame", Kind: "image", Sources: frameSources, MaxItems: 1, Required: plugin.Meta.Key != "huahuo-video"})
				}
			case "reference_to_video":
				for _, input := range []struct {
					name, kind string
					count      *int
				}{{"reference_images", "image", config.References.Images}, {"reference_videos", "video", config.References.Videos}, {"reference_audios", "audio", config.References.Audios}} {
					if input.count != nil && *input.count > 0 {
						sources := []string{"url"}
						if input.kind == "image" {
							sources = imageSources
						}
						mode.Inputs = append(mode.Inputs, VideoInputProfile{Name: input.name, Kind: input.kind, Sources: sources, MaxItems: *input.count})
					}
				}
			}
			profile.Modes = append(profile.Modes, mode)
		}
		schema := map[string]UsageFieldSchema{}
		if config.BillingMode != "per_request" {
			schema["seconds"] = UsageFieldSchema{Type: "number", Unit: "second", Description: LocalizedText{"en": "Video generation unit price", "zh": "视频生成单价"}}
		}
		if config.BillingMode != "per_second" {
			key := "requests"
			if plugin.Meta.Key == "wolong" {
				key = "video_count"
			}
			schema[key] = UsageFieldSchema{Type: "number", Unit: "count", UnitLabel: LocalizedText{"en": "video", "zh": "条"}, Description: LocalizedText{"en": "Video generation unit price", "zh": "视频生成单价"}}
		}
		schema["resolution"] = UsageFieldSchema{Enum: resolutions, Description: LocalizedText{"en": "Output video resolution", "zh": "输出视频分辨率"}}
		if plugin.Meta.Key == "drama" {
			schema["video_input"] = plugin.Meta.UsageSchema["video_input"]
		}
		if plugin.Meta.Key == "tabtab" {
			schema["reference_video"] = plugin.Meta.UsageSchema["reference_video"]
		}
		usage := UsageProfile{Models: []string{name}, Schema: schema}
		if len(sourceProfiles) > 0 {
			profile, usage = unionTaskVideoProfiles(append(sourceProfiles, profile), append(sourceUsage, usage), name)
			copy.presetVideoAliases[name] = true
		}
		copy.Meta.VideoProfiles = append(copy.Meta.VideoProfiles, profile)
		copy.Meta.UsageProfiles = append(copy.Meta.UsageProfiles, usage)
	}
	return &copy
}

// LegacyTaskVideoModel identifies source-declared models that predate video
// capability profiles. New channel-owned models and Wolong still need profiles.
func LegacyTaskVideoModel(meta Meta, name string) (string, bool) {
	if meta.Key == "wolong" || !dto.PresetTaskVideoPlugin(meta.Key) {
		return "", false
	}
	canonical, declared := meta.declaredModel(name)
	if !declared {
		return "", false
	}
	_, profiled := meta.VideoProfileForModel(canonical)
	return canonical, !profiled
}

// HasPresetTaskVideoModel reads immutable source declarations, never the model
// list enlarged by channel configuration.
func HasPresetTaskVideoModel(plugin *LoadedPlugin, name string) bool {
	if plugin == nil || !dto.PresetTaskVideoPlugin(plugin.Meta.Key) {
		return false
	}
	meta := PresetTaskVideoMeta(plugin)
	if plugin.presetVideoAliases[name] {
		return true
	}
	_, declared := meta.declaredModel(name)
	return declared
}

// PresetTaskVideoMeta returns the active source contract before channel-owned
// models were added. Uploaded overrides retain their own original declarations.
func PresetTaskVideoMeta(plugin *LoadedPlugin) Meta {
	if plugin == nil {
		return Meta{}
	}
	if plugin.presetVideoMeta != nil {
		return cloneMeta(*plugin.presetVideoMeta)
	}
	return cloneMeta(plugin.Meta)
}

// PresetTaskVideoMeta keeps saved channel validation independent of execution
// enablement. Reading a disabled source here never publishes its endpoints.
func (r *Registry) PresetTaskVideoMeta(key string) (Meta, bool) {
	if plugin, found := r.Get(key); found {
		return PresetTaskVideoMeta(plugin), true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if plugin, found := r.override[key]; found {
		return PresetTaskVideoMeta(plugin), true
	}
	if plugin, found := r.factory[key]; found {
		return PresetTaskVideoMeta(plugin), true
	}
	return Meta{}, false
}

func IsCustomTaskVideoModel(plugin *LoadedPlugin, name string) bool {
	return plugin != nil && dto.ChannelConfiguredTaskVideoPlugin(plugin.Meta.Key) && !HasPresetTaskVideoModel(plugin, name)
}

func TaskVideoModelConfigsEqual(a, b dto.TaskVideoModelConfig) bool {
	a.PresetModel, b.PresetModel = "", ""
	a.PresetModels, b.PresetModels = nil, nil
	a.CustomModelUnion, b.CustomModelUnion = false, false
	return reflect.DeepEqual(a, b)
}

// unionTaskVideoProfiles is discovery metadata for a public alias served by
// several channels. Channel selection replaces it with one exact contract.
func unionTaskVideoProfiles(profiles []VideoProfile, usages []UsageProfile, name string) (VideoProfile, UsageProfile) {
	profile := VideoProfile{Models: []string{name}}
	usage := UsageProfile{Models: []string{name}, Schema: map[string]UsageFieldSchema{}}
	for candidateIndex, candidate := range profiles {
		for _, next := range candidate.Modes {
			index := slices.IndexFunc(profile.Modes, func(mode VideoModeProfile) bool { return mode.Name == next.Name })
			if index < 0 {
				profile.Modes = append(profile.Modes, next)
				continue
			}
			mode := &profile.Modes[index]
			if mode.Duration == nil {
				mode.Duration = next.Duration
			} else if next.Duration != nil {
				left := dto.TaskVideoDuration{Min: mode.Duration.Min, Max: mode.Duration.Max, Step: mode.Duration.Step, Values: mode.Duration.Values, Default: mode.Duration.Default}
				right := dto.TaskVideoDuration{Min: next.Duration.Min, Max: next.Duration.Max, Step: next.Duration.Step, Values: next.Duration.Values, Default: next.Duration.Default}
				merged := MergeTaskVideoModel(dto.TaskVideoModelConfig{Duration: left}, dto.TaskVideoModelConfig{Duration: right}, relaycommon.MaxTaskDurationSeconds)
				duration := taskVideoDurationProfile(merged.Duration)
				mode.Duration = &duration
			}
			for _, choice := range []struct {
				target *[]string
				values []string
			}{{&mode.Resolutions, next.Resolutions}, {&mode.AspectRatios, next.AspectRatios}, {&mode.ExtensionDirections, next.ExtensionDirections}} {
				for _, value := range choice.values {
					if !slices.Contains(*choice.target, value) {
						*choice.target = append(*choice.target, value)
					}
				}
			}
			mode.Options.GenerateAudio = mode.Options.GenerateAudio || next.Options.GenerateAudio
			mode.Options.Watermark = mode.Options.Watermark || next.Options.Watermark
			mode.Options.ReturnLastFrame = mode.Options.ReturnLastFrame || next.Options.ReturnLastFrame
			for i := range mode.Inputs {
				if !slices.ContainsFunc(next.Inputs, func(input VideoInputProfile) bool {
					return input.Name == mode.Inputs[i].Name && input.Kind == mode.Inputs[i].Kind
				}) {
					mode.Inputs[i].Required = false
				}
			}
			for _, input := range next.Inputs {
				index := slices.IndexFunc(mode.Inputs, func(existing VideoInputProfile) bool {
					return existing.Name == input.Name && existing.Kind == input.Kind
				})
				if index < 0 {
					input.Required = false
					mode.Inputs = append(mode.Inputs, input)
					continue
				}
				current := &mode.Inputs[index]
				current.Required = current.Required && input.Required
				current.MaxItems = max(current.MaxItems, input.MaxItems)
				for _, source := range input.Sources {
					if !slices.Contains(current.Sources, source) {
						current.Sources = append(current.Sources, source)
					}
				}
			}
		}
		schema, examples := usages[candidateIndex].Schema, usages[candidateIndex].Examples
		for key, field := range schema {
			if previous, exists := usage.Schema[key]; exists {
				for _, value := range previous.Enum {
					if !slices.Contains(field.Enum, value) {
						field.Enum = append(slices.Clone(field.Enum), value)
					}
				}
			}
			usage.Schema[key] = field
		}
		if len(examples) > 0 && len(usage.Examples) == 0 {
			usage.Examples = CloneUsageExamples(examples)
		}
	}
	return profile, usage
}

// TaskVideoModelConfigFromMeta projects the factory capability contract without
// duplicating provider model tables in host code.
func TaskVideoModelConfigFromMeta(meta Meta, name string) (dto.TaskVideoModelConfig, bool) {
	profile, found := meta.VideoProfileForModel(name)
	if !found || len(profile.Modes) == 0 || profile.Modes[0].Duration == nil {
		return dto.TaskVideoModelConfig{}, false
	}
	mode := profile.Modes[0]
	duration := mode.Duration
	config := dto.TaskVideoModelConfig{
		Duration:    dto.TaskVideoDuration{Min: duration.Min, Max: duration.Max, Step: duration.Step, Values: slices.Clone(duration.Values), Default: duration.Default},
		Resolutions: slices.Clone(mode.Resolutions), AspectRatios: slices.Clone(mode.AspectRatios), BillingMode: "per_request",
	}
	for _, dimension := range []struct {
		values    *[]string
		preferred string
	}{{&config.Resolutions, mode.DefaultResolution}, {&config.AspectRatios, mode.DefaultAspectRatio}} {
		if i := slices.Index(*dimension.values, dimension.preferred); i > 0 {
			values := *dimension.values
			copy(values[1:i+1], values[:i])
			values[0] = dimension.preferred
		}
	}
	images, videos, audios := 0, 0, 0
	for _, mode := range profile.Modes {
		if !slices.Contains([]string{"text_to_video", "image_to_video", "reference_to_video", "first_last_frame"}, mode.Name) {
			continue
		}
		config.Modes = append(config.Modes, mode.Name)
		counts := map[string]int{}
		for _, input := range mode.Inputs {
			counts[input.Kind] += input.MaxItems
		}
		images, videos, audios = max(images, counts["image"]), max(videos, counts["video"]), max(audios, counts["audio"])
	}
	config.References = dto.TaskVideoReferences{Images: &images, Videos: &videos, Audios: &audios}
	schema, _ := meta.UsageForModel(name)
	if _, perSecond := schema["seconds"]; perSecond {
		config.BillingMode = "per_second"
	}
	return config, true
}

func taskVideoDurationProfile(config dto.TaskVideoDuration) VideoDurationProfile {
	duration := VideoDurationProfile{Min: config.Min, Max: config.Max, Step: config.Step, Values: slices.Clone(config.Values), Default: config.Default}
	if duration.Default == 0 {
		duration.Default = duration.Min
		if len(duration.Values) > 0 {
			duration.Default = duration.Values[0]
		}
	}
	return duration
}

// MergeTaskVideoModel describes the union for discovery. Admission always uses
// the selected channel's unmerged config, never this permissive catalog union.
func MergeTaskVideoModel(a, b dto.TaskVideoModelConfig, maxSeconds int) dto.TaskVideoModelConfig {
	if a.PresetModel != "" && a.PresetModel == b.PresetModel && TaskVideoModelConfigsEqual(a, b) {
		return a
	}
	left, right := slices.Clone(a.PresetModels), slices.Clone(b.PresetModels)
	if a.PresetModel != "" {
		left = append(left, a.PresetModel)
	}
	if b.PresetModel != "" {
		right = append(right, b.PresetModel)
	}
	a.PresetModel = ""
	a.PresetModels = nil
	if len(left)+len(right) > 0 {
		a.CustomModelUnion = a.CustomModelUnion || b.CustomModelUnion || len(left) == 0 || len(right) == 0
		a.PresetModels = left
		for _, source := range right {
			if !slices.Contains(a.PresetModels, source) {
				a.PresetModels = append(a.PresetModels, source)
			}
		}
	}
	if len(a.DurationsByResolution)+len(b.DurationsByResolution) > 0 {
		durations := make(map[string]dto.TaskVideoDuration)
		for _, resolution := range slices.Concat(a.Resolutions, b.Resolutions) {
			key := strings.ToLower(resolution)
			if _, exists := durations[key]; exists {
				continue
			}
			durations[key] = mergeResolutionDuration(a, b, key, maxSeconds)
		}
		a.DurationsByResolution = durations
	}
	values := []int{}
	for i := 1; i <= maxSeconds; i++ {
		if a.AllowsDuration(i) || b.AllowsDuration(i) {
			values = append(values, i)
		}
	}
	a.Duration = dto.TaskVideoDuration{Values: values, Default: a.Duration.Default}
	for _, v := range b.Resolutions {
		if !slices.Contains(a.Resolutions, v) {
			a.Resolutions = append(slices.Clone(a.Resolutions), v)
		}
	}
	for _, v := range b.AspectRatios {
		if !slices.Contains(a.AspectRatios, v) {
			a.AspectRatios = append(slices.Clone(a.AspectRatios), v)
		}
	}
	for _, v := range b.Modes {
		if !slices.Contains(a.Modes, v) {
			a.Modes = append(slices.Clone(a.Modes), v)
		}
	}
	for _, limit := range []struct {
		target **int
		value  *int
	}{{&a.References.Images, b.References.Images}, {&a.References.Videos, b.References.Videos}, {&a.References.Audios, b.References.Audios}} {
		if limit.value != nil && (*limit.target == nil || *limit.value > **limit.target) {
			value := *limit.value
			*limit.target = &value
		}
	}
	a.References.ImageUpload = a.References.ImageUpload || b.References.ImageUpload
	a.References.ImageDataURI = a.References.ImageDataURI || b.References.ImageDataURI
	if a.BillingMode == "" {
		a.BillingMode = b.BillingMode
	} else if b.BillingMode != "" && a.BillingMode != b.BillingMode {
		a.BillingMode = "mixed"
	}
	return a
}

func mergeResolutionDuration(a, b dto.TaskVideoModelConfig, resolution string, maxSeconds int) dto.TaskVideoDuration {
	var duration dto.TaskVideoDuration
	seen := make([]bool, maxSeconds+1)
	for _, config := range []dto.TaskVideoModelConfig{a, b} {
		if !slices.ContainsFunc(config.Resolutions, func(value string) bool { return strings.EqualFold(value, resolution) }) {
			continue
		}
		options, exists := config.DurationsByResolution[resolution]
		if !exists {
			options = config.Duration
		}
		if duration.Default == 0 {
			duration.Default = taskVideoDurationProfile(options).Default
		}
		if len(options.Values) > 0 {
			for _, seconds := range options.Values {
				if seconds > 0 && seconds <= maxSeconds {
					seen[seconds] = true
				}
			}
			continue
		}
		if options.Step <= 0 {
			continue
		}
		for seconds := max(1, options.Min); seconds <= min(maxSeconds, options.Max); seconds += options.Step {
			seen[seconds] = true
		}
	}
	for seconds := 1; seconds <= maxSeconds; seconds++ {
		if seen[seconds] {
			duration.Values = append(duration.Values, seconds)
		}
	}
	return duration
}

func configuredVideoPlugins(plugins map[string]*LoadedPlugin, configs map[string]map[string]dto.TaskVideoModelConfig) map[string]*LoadedPlugin {
	if configs == nil {
		return plugins
	}
	configured := maps.Clone(plugins)
	for key, plugin := range plugins {
		if dto.ChannelConfiguredTaskVideoPlugin(key) {
			configured[key] = WithTaskVideoModels(plugin, configs[key])
		}
	}
	return configured
}
