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
	if plugin == nil || plugin.Meta.Key != "sudashui" {
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
		duration := &VideoDurationProfile{Min: config.Duration.Min, Max: config.Duration.Max, Step: config.Duration.Step, Values: slices.Clone(config.Duration.Values), Default: config.Duration.Default}
		if duration.Default == 0 {
			duration.Default = duration.Min
			if len(duration.Values) > 0 {
				duration.Default = duration.Values[0]
			}
		}
		for _, name := range config.Modes {
			mode := VideoModeProfile{Name: name, Duration: duration, Resolutions: slices.Clone(resolutions), AspectRatios: slices.Clone(config.AspectRatios)}
			if len(mode.Resolutions) > 0 {
				mode.DefaultResolution = mode.Resolutions[0]
			}
			if len(mode.AspectRatios) > 0 {
				mode.DefaultAspectRatio = mode.AspectRatios[0]
			}
			switch name {
			case "image_to_video":
				mode.Inputs = []VideoInputProfile{{Name: "image", Kind: "image", Sources: []string{"url"}, MaxItems: 1, Required: true}}
			case "first_last_frame":
				mode.Inputs = []VideoInputProfile{{Name: "image", Kind: "image", Sources: []string{"url"}, MaxItems: 1, Required: true}, {Name: "last_frame", Kind: "image", Sources: []string{"url"}, MaxItems: 1, Required: true}}
			case "reference_to_video":
				for _, input := range []struct {
					name, kind string
					count      *int
				}{{"reference_images", "image", config.References.Images}, {"reference_videos", "video", config.References.Videos}, {"reference_audios", "audio", config.References.Audios}} {
					if input.count != nil && *input.count > 0 {
						mode.Inputs = append(mode.Inputs, VideoInputProfile{Name: input.name, Kind: input.kind, Sources: []string{"url"}, MaxItems: *input.count})
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

// MergeTaskVideoModel describes the union for discovery. Admission always uses
// the selected channel's unmerged config, never this permissive catalog union.
func MergeTaskVideoModel(a, b dto.TaskVideoModelConfig, maxSeconds int) dto.TaskVideoModelConfig {
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
	if a.BillingMode != b.BillingMode {
		a.BillingMode = "mixed"
	}
	return a
}

func configuredVideoPlugins(plugins map[string]*LoadedPlugin, configs map[string]dto.TaskVideoModelConfig) map[string]*LoadedPlugin {
	if configs == nil {
		return plugins
	}
	if plugin := plugins["sudashui"]; plugin != nil {
		plugins = maps.Clone(plugins)
		plugins["sudashui"] = WithTaskVideoModels(plugin, configs)
	}
	return plugins
}
