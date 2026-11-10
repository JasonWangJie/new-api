package model

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// lockTaskVideoModelBindings serializes catalog writers across processes, even
// when no channel declares a model yet. Acquire it before reading any bindings
// so MySQL's repeatable-read snapshot includes the preceding writer's commit.
func lockTaskVideoModelBindings(tx *gorm.DB) error {
	guard := Option{Key: "task_video_model_bindings_guard", Value: "{}"}
	if err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"key"}),
	}).Create(&guard).Error; err != nil {
		return err
	}
	return lockForUpdate(tx).Where(map[string]any{"key": guard.Key}).Take(&Option{}).Error
}

func (channel *Channel) TaskVideoModelConfig(publicModel string) (string, dto.TaskVideoModelConfig, error) {
	upstream, _, err := channel.taskVideoOrdinaryUpstream(publicModel)
	if err != nil {
		return "", dto.TaskVideoModelConfig{}, err
	}
	config, err := ResolveTaskVideoModelConfig(channel.GetSetting(), upstream)
	return upstream, config, err
}

// TaskVideoResolutionModelConfig resolves a mixed video mapping directly to the
// upstream model, or retains ordinary model resolution when the alias has no map.
func (channel *Channel) TaskVideoResolutionModelConfig(publicModel, resolution string) (string, dto.TaskVideoModelConfig, error) {
	settings := channel.GetSetting()
	targets, mapped := settings.VideoResolutionModelMapping[publicModel]
	if !mapped {
		return channel.TaskVideoModelConfig(publicModel)
	}
	if channel.Type != constant.ChannelTypeTaskPlugin {
		return "", dto.TaskVideoModelConfig{}, fmt.Errorf("video resolution model mapping requires a task plugin channel")
	}
	if resolution == "" {
		return "", dto.TaskVideoModelConfig{}, fmt.Errorf("resolution is required for video model %s", publicModel)
	}
	upstream := targets[resolution]
	if upstream == "" {
		return "", dto.TaskVideoModelConfig{}, fmt.Errorf("video model %s has no mapping for %s", publicModel, resolution)
	}
	binding, err := resolveTaskVideoResolutionBinding(settings, upstream, resolution, nil)
	return binding.upstream, binding.native, err
}

// IsMiniMaxH3VideoModel scopes the public 720p and native 768p equivalence to
// the known model identities on each video protocol.
func IsMiniMaxH3VideoModel(pluginKey, upstream string) bool {
	switch pluginKey {
	case "yali":
		return slices.Contains([]string{"minimax-h3-720p", "MiniMaxH3-720p-realistic", "MiniMaxH3-480p", "MiniMaxH3-480p-sec", "MiniMaxH3-2k-pro-sec"}, upstream)
	case "drama":
		return upstream == "minimax-h3" || upstream == "minimax-h3-2k"
	case "yuxibox":
		return slices.Contains([]string{"minimax-h3-quality", "minimax-h3-fast", "minimax-h3-15s"}, upstream)
	case "wolong":
		return upstream == "czniu-minimax-h3-15s" || upstream == "czniu-minimax-h3-30s"
	case "huahuo-video":
		return upstream == "inmind/video/minimax-h3-2"
	case "hailuo":
		return upstream == "MiniMax-H3"
	default:
		return false
	}
}

type taskVideoModelBinding struct {
	upstream   string
	resolution string
	native     dto.TaskVideoModelConfig
	public     dto.TaskVideoModelConfig
}

var errTaskVideoCapabilityRequired = errors.New("requires a saved capability configuration")

func taskVideoModelHasProtocol(meta jsplugin.Meta, upstream string) bool {
	return slices.ContainsFunc(meta.Protocols, func(claim jsplugin.ProtocolClaim) bool {
		return claim.Name == "openai_video" && (len(claim.Models) == 0 || slices.Contains(claim.Models, upstream))
	})
}

func resolveTaskVideoResolutionBinding(settings dto.ChannelSettings, upstream, resolution string, source *jsplugin.Meta) (taskVideoModelBinding, error) {
	var config dto.TaskVideoModelConfig
	var meta jsplugin.Meta
	var hasMeta bool
	if source != nil {
		meta, hasMeta = *source, true
	} else {
		meta, hasMeta = jsplugin.DefaultRegistry.PresetTaskVideoMeta(settings.TaskPluginKey)
	}
	configured := dto.ChannelConfiguredTaskVideoPlugin(settings.TaskPluginKey)
	if configured {
		var err error
		if hasMeta && dto.PresetTaskVideoPlugin(settings.TaskPluginKey) {
			source = &meta
		}
		config, err = resolveTaskVideoModelConfig(settings, upstream, source)
		if err != nil {
			return taskVideoModelBinding{}, err
		}
	} else {
		if !hasMeta || !taskVideoModelHasProtocol(meta, upstream) {
			return taskVideoModelBinding{}, fmt.Errorf("model %s requires an openai_video plugin binding", upstream)
		}
		var found bool
		config, found = jsplugin.TaskVideoModelConfigFromMeta(meta, upstream)
		if !found {
			return taskVideoModelBinding{}, fmt.Errorf("model %s requires declared video capabilities", upstream)
		}
	}
	h3 := IsMiniMaxH3VideoModel(settings.TaskPluginKey, upstream)
	resolutionMatches := func(native string) bool {
		return strings.EqualFold(native, resolution) || h3 && resolution == "720p" && strings.EqualFold(native, "768p")
	}
	nativeResolution := ""
	var presetDuration *jsplugin.VideoDurationProfile
	if (config.PresetModel != "" || !configured) && hasMeta {
		profile, _ := meta.VideoProfileForModel(upstream)
		for _, mode := range profile.Modes {
			if mode.Name == "edit_video" || mode.Name == "extend_video" {
				continue
			}
			if index := slices.IndexFunc(mode.Resolutions, resolutionMatches); index >= 0 {
				nativeResolution = mode.Resolutions[index]
				presetDuration = mode.DurationForResolution(nativeResolution)
				break
			}
		}
	} else if index := slices.IndexFunc(config.Resolutions, resolutionMatches); index >= 0 {
		nativeResolution = config.Resolutions[index]
	}
	if nativeResolution == "" || config.BillingMode == "" {
		return taskVideoModelBinding{}, fmt.Errorf("model %s does not support mapped video resolution %s", upstream, resolution)
	}
	public := config
	public.PresetModel = ""
	public.Resolutions = []string{resolution}
	if presetDuration != nil {
		public.Duration = dto.TaskVideoDuration{Min: presetDuration.Min, Max: presetDuration.Max, Step: presetDuration.Step, Values: presetDuration.Values, Default: presetDuration.Default}
	} else if duration, ok := config.DurationsByResolution[strings.ToLower(nativeResolution)]; ok {
		public.Duration = duration
	}
	// Keep the per-specification duration when discovery merges several targets.
	public.DurationsByResolution = map[string]dto.TaskVideoDuration{resolution: public.Duration}
	return taskVideoModelBinding{upstream: upstream, resolution: resolution, native: config, public: public}, nil
}

func (channel *Channel) taskVideoModelBindings(settings dto.ChannelSettings, publicModel string, source *jsplugin.Meta) ([]taskVideoModelBinding, error) {
	targets, mapped := settings.VideoResolutionModelMapping[publicModel]
	var bindings []taskVideoModelBinding
	if mapped {
		bindings = make([]taskVideoModelBinding, 0, len(targets)+1)
		for _, resolution := range dto.VideoResolutionMappingTiers() {
			upstream, exists := targets[resolution]
			if !exists {
				continue
			}
			binding, err := resolveTaskVideoResolutionBinding(settings, upstream, resolution, source)
			if err != nil {
				return nil, err
			}
			bindings = append(bindings, binding)
		}
	}
	configured := dto.ChannelConfiguredTaskVideoPlugin(settings.TaskPluginKey)
	if !configured && !mapped {
		return nil, nil
	}
	upstream, explicit, err := channel.taskVideoOrdinaryUpstream(publicModel)
	if err != nil {
		return nil, err
	}
	if !configured {
		// Other task plugins resolve ordinary models from their own declarations.
		if explicit {
			meta, found := jsplugin.DefaultRegistry.PresetTaskVideoMeta(settings.TaskPluginKey)
			if !found || !slices.Contains(meta.Models, upstream) || !taskVideoModelHasProtocol(meta, upstream) {
				return nil, fmt.Errorf("ordinary model %s requires a declared openai_video plugin binding", upstream)
			}
		}
		return bindings, nil
	}
	config, err := resolveTaskVideoModelConfig(settings, upstream, source)
	if mapped && !explicit && errors.Is(err, errTaskVideoCapabilityRequired) {
		// A resolution-only alias does not need an ordinary binding. Keep any
		// configured or source-declared ordinary binding when it does have one.
		return bindings, nil
	}
	if err != nil {
		return nil, err
	}
	return append(bindings, taskVideoModelBinding{upstream: upstream, native: config, public: config}), nil
}

func (channel *Channel) taskVideoOrdinaryUpstream(publicModel string) (string, bool, error) {
	upstream := publicModel
	explicit := false
	if raw := channel.GetModelMapping(); raw != "" {
		mapping := map[string]string{}
		if err := common.UnmarshalJsonStr(raw, &mapping); err != nil {
			return "", false, err
		}
		explicit = mapping[publicModel] != ""
		var cyclic bool
		upstream, cyclic = followChannelModelMapping(mapping, publicModel)
		if cyclic {
			return "", false, fmt.Errorf("cyclic model mapping")
		}
	}
	return upstream, explicit, nil
}

// ResolveTaskVideoModelConfig keeps source presets authoritative, including
// when a frozen configuration has lost its non-JSON provenance marker.
func ResolveTaskVideoModelConfig(settings dto.ChannelSettings, upstream string) (dto.TaskVideoModelConfig, error) {
	return resolveTaskVideoModelConfig(settings, upstream, nil)
}

func resolveTaskVideoModelConfig(settings dto.ChannelSettings, upstream string, source *jsplugin.Meta) (dto.TaskVideoModelConfig, error) {
	config, exists := settings.TaskPluginModelConfigs[upstream]
	config.PresetModel = ""
	config.PresetModels = nil
	config.CustomModelUnion = false
	if dto.PresetTaskVideoPlugin(settings.TaskPluginKey) {
		meta, found := jsplugin.Meta{}, source != nil
		if source != nil {
			meta = *source
		} else {
			meta, found = jsplugin.DefaultRegistry.PresetTaskVideoMeta(settings.TaskPluginKey)
		}
		if found {
			if canonical, legacy := jsplugin.LegacyTaskVideoModel(meta, upstream); legacy {
				if exists && !jsplugin.TaskVideoModelConfigsEqual(config, dto.TaskVideoModelConfig{}) {
					return config, fmt.Errorf("model %s must retain its source-declared legacy capabilities", upstream)
				}
				return dto.TaskVideoModelConfig{PresetModel: canonical}, nil
			}
			defaults, known := jsplugin.TaskVideoModelConfigFromMeta(meta, upstream)
			if known {
				if exists && config.BillingMode != defaults.BillingMode {
					return config, fmt.Errorf("model %s must retain its built-in billing unit", upstream)
				}
				if exists && meta.Key != "wolong" && !jsplugin.TaskVideoModelConfigsEqual(config, defaults) {
					return config, fmt.Errorf("model %s must retain its built-in capabilities", upstream)
				}
				if !exists || jsplugin.TaskVideoModelConfigsEqual(config, defaults) {
					defaults.PresetModel = upstream
					return defaults, nil
				}
			}
		}
	}
	if !exists {
		return config, fmt.Errorf("model %s %w", upstream, errTaskVideoCapabilityRequired)
	}
	return config, config.ValidateForPlugin(settings.TaskPluginKey, relaycommon.MaxTaskDurationSeconds)
}

// IsConfiguredTaskVideoModel identifies a genuinely new channel-owned model;
// an explicit copy of a built-in profile does not change its billing contract.
func IsConfiguredTaskVideoModel(settings dto.ChannelSettings, upstream string) bool {
	if !dto.ChannelConfiguredTaskVideoPlugin(settings.TaskPluginKey) {
		return false
	}
	if _, exists := settings.TaskPluginModelConfigs[upstream]; !exists {
		return false
	}
	if dto.PresetTaskVideoPlugin(settings.TaskPluginKey) {
		if meta, found := jsplugin.DefaultRegistry.PresetTaskVideoMeta(settings.TaskPluginKey); found {
			if _, legacy := jsplugin.LegacyTaskVideoModel(meta, upstream); legacy {
				return false
			}
			_, known := meta.VideoProfileForModel(upstream)
			return !known
		}
	}
	return true
}

// ValidateTaskVideoModelBindings prevents one retail model from changing units
// with channel selection. Different public aliases may use different units.
func (channel *Channel) ValidateTaskVideoModelBindings() error {
	return channel.validateTaskVideoModelBindings(DB)
}

func (channel *Channel) hasTaskVideoModelBindings() bool {
	settings := channel.GetSetting()
	return dto.ChannelConfiguredTaskVideoPlugin(settings.TaskPluginKey) || len(settings.VideoResolutionModelMapping) > 0
}

func (channel *Channel) validateTaskVideoModelBindings(db *gorm.DB) error {
	settings := channel.GetSetting()
	pluginKey := settings.TaskPluginKey
	if err := settings.VideoResolutionModelMapping.Validate(); err != nil {
		return err
	}
	if err := channel.validateTaskVideoResolutionSources(settings.VideoResolutionModelMapping); err != nil {
		return err
	}
	if channel.Type != constant.ChannelTypeTaskPlugin || !dto.ChannelConfiguredTaskVideoPlugin(pluginKey) && len(settings.VideoResolutionModelMapping) == 0 {
		return nil
	}
	units := map[string]string{}
	for _, name := range channel.GetModels() {
		if name == "" {
			continue
		}
		bindings, err := channel.taskVideoModelBindings(settings, name, nil)
		if err != nil {
			return err
		}
		folded := strings.ToLower(name)
		for _, binding := range bindings {
			unit := binding.native.BillingMode
			if unit == "" {
				continue
			}
			if previous, exists := units[folded]; exists && previous != unit {
				return fmt.Errorf("model %s has incompatible billing units; use different public model aliases", name)
			}
			units[folded] = unit
		}
	}
	var others []Channel
	if err := db.Where("type = ? AND id <> ?", constant.ChannelTypeTaskPlugin, channel.Id).Select("id", "setting", "models", "model_mapping").Find(&others).Error; err != nil {
		return err
	}
	for _, other := range others {
		otherSettings := other.GetSetting()
		if otherSettings.TaskPluginKey != pluginKey {
			continue
		}
		for name := range strings.SplitSeq(other.Models, ",") {
			unit, exists := units[strings.ToLower(name)]
			if !exists {
				continue
			}
			bindings, err := other.taskVideoModelBindings(otherSettings, name, nil)
			if err == nil {
				for _, binding := range bindings {
					if otherUnit := binding.native.BillingMode; otherUnit != "" && unit != otherUnit {
						return fmt.Errorf("model %s has incompatible billing units; use different public model aliases", name)
					}
				}
			}
		}
	}
	return nil
}

func (channel *Channel) validateTaskVideoResolutionSources(mapping dto.VideoResolutionModelMapping) error {
	if len(mapping) == 0 {
		return nil
	}
	if channel.Type != constant.ChannelTypeTaskPlugin {
		return fmt.Errorf("video resolution model mapping requires a task plugin channel")
	}
	models := channel.GetModels()
	for name := range mapping {
		if !slices.Contains(models, name) {
			return fmt.Errorf("video resolution mapping model %s must be included in the channel models", name)
		}
	}
	return nil
}

func taskVideoModelCatalog(channels []Channel, pluginKey string, sources ...jsplugin.Meta) (map[string]dto.TaskVideoModelConfig, error) {
	var source *jsplugin.Meta
	if len(sources) > 0 {
		source = &sources[0]
	}
	catalog := map[string]dto.TaskVideoModelConfig{}
	publicCatalog := map[string]dto.TaskVideoModelConfig{}
	mappedCatalog := map[string]dto.TaskVideoModelConfig{}
	units := map[string]string{}
	for _, channel := range channels {
		settings := channel.GetSetting()
		if channel.Status != common.ChannelStatusEnabled || settings.TaskPluginKey != pluginKey {
			continue
		}
		for name := range strings.SplitSeq(channel.Models, ",") {
			if name == "" {
				continue
			}
			bindings, err := channel.taskVideoModelBindings(settings, name, source)
			if err != nil {
				return nil, fmt.Errorf("channel %d: %w", channel.Id, err)
			}
			for _, binding := range bindings {
				if unit := binding.native.BillingMode; unit != "" {
					folded := strings.ToLower(name)
					if previous, exists := units[folded]; exists && previous != unit {
						return nil, fmt.Errorf("model %s has incompatible billing units; use different public model aliases", name)
					}
					units[folded] = unit
				}
				aliasCatalog := publicCatalog
				if binding.resolution != "" {
					aliasCatalog = mappedCatalog
				}
				for _, entry := range []struct {
					target map[string]dto.TaskVideoModelConfig
					key    string
					config dto.TaskVideoModelConfig
				}{{catalog, binding.upstream, binding.native}, {aliasCatalog, name, binding.public}} {
					if previous, exists := entry.target[entry.key]; exists {
						entry.config = jsplugin.MergeTaskVideoModel(previous, entry.config, relaycommon.MaxTaskDurationSeconds)
					}
					entry.target[entry.key] = entry.config
				}
			}
		}
	}
	maps.Copy(catalog, publicCatalog)
	for name, config := range mappedCatalog {
		if _, native := catalog[name]; !native {
			catalog[name] = config
		}
	}
	return catalog, nil
}
