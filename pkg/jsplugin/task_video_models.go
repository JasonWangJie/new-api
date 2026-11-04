package jsplugin

import (
	"maps"
	"slices"
	"strings"

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
	meta.Models, meta.VideoProfiles, meta.UsageProfiles = nil, nil, nil
	copy.Meta = cloneMeta(meta)
	copy.Meta.Models = make([]string, 0, len(configs))
	for name := range configs {
		copy.Meta.Models = append(copy.Meta.Models, name)
	}
	slices.Sort(copy.Meta.Models)
	for _, name := range copy.Meta.Models {
		config := configs[name]
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
					mode.Inputs = append(mode.Inputs, VideoInputProfile{Name: "last_frame", Kind: "image", Sources: frameSources, MaxItems: 1, Required: plugin.Meta.Key == "sudashui"})
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
		copy.Meta.VideoProfiles = append(copy.Meta.VideoProfiles, profile)
		schema := map[string]UsageFieldSchema{}
		if config.BillingMode != "per_request" {
			schema["seconds"] = UsageFieldSchema{Type: "number", Unit: "second", Description: LocalizedText{"en": "Video generation unit price", "zh": "视频生成单价"}}
		}
		if config.BillingMode != "per_second" {
			schema["requests"] = UsageFieldSchema{Type: "number", Unit: "count", UnitLabel: LocalizedText{"en": "video", "zh": "条"}, Description: LocalizedText{"en": "Video generation unit price", "zh": "视频生成单价"}}
		}
		schema["resolution"] = UsageFieldSchema{Enum: resolutions, Description: LocalizedText{"en": "Output video resolution", "zh": "输出视频分辨率"}}
		copy.Meta.UsageProfiles = append(copy.Meta.UsageProfiles, UsageProfile{Models: []string{name}, Schema: schema})
	}
	return &copy
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
	if a.BillingMode != b.BillingMode {
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
