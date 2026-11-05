package model

import (
	"fmt"
	"maps"
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
	return channel.taskVideoModelConfig(publicModel, nil)
}

func (channel *Channel) taskVideoModelConfig(publicModel string, source *jsplugin.Meta) (string, dto.TaskVideoModelConfig, error) {
	settings := channel.GetSetting()
	upstream := publicModel
	if raw := channel.GetModelMapping(); raw != "" {
		mapping := map[string]string{}
		if err := common.UnmarshalJsonStr(raw, &mapping); err != nil {
			return "", dto.TaskVideoModelConfig{}, err
		}
		var cyclic bool
		upstream, cyclic = followChannelModelMapping(mapping, publicModel)
		if cyclic {
			return "", dto.TaskVideoModelConfig{}, fmt.Errorf("cyclic model mapping")
		}
	}
	config, err := resolveTaskVideoModelConfig(settings, upstream, source)
	return upstream, config, err
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
		return config, fmt.Errorf("model %s requires a saved capability configuration", upstream)
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

func (channel *Channel) validateTaskVideoModelBindings(db *gorm.DB) error {
	pluginKey := channel.GetSetting().TaskPluginKey
	if channel.Type != constant.ChannelTypeTaskPlugin || !dto.ChannelConfiguredTaskVideoPlugin(pluginKey) {
		return nil
	}
	units := map[string]string{}
	for name := range strings.SplitSeq(channel.Models, ",") {
		if name == "" {
			continue
		}
		_, config, err := channel.TaskVideoModelConfig(name)
		if err != nil {
			return err
		}
		if config.BillingMode == "" {
			continue
		}
		folded := strings.ToLower(name)
		if previous, exists := units[folded]; exists && previous != config.BillingMode {
			return fmt.Errorf("model %s has incompatible billing units; use different public model aliases", name)
		}
		units[folded] = config.BillingMode
	}
	var others []Channel
	if err := db.Where("type = ? AND id <> ?", constant.ChannelTypeTaskPlugin, channel.Id).Select("id", "setting", "models", "model_mapping").Find(&others).Error; err != nil {
		return err
	}
	for _, other := range others {
		if other.GetSetting().TaskPluginKey != pluginKey {
			continue
		}
		for name := range strings.SplitSeq(other.Models, ",") {
			unit, exists := units[strings.ToLower(name)]
			if !exists {
				continue
			}
			_, config, err := other.TaskVideoModelConfig(name)
			if err == nil && config.BillingMode != "" && unit != config.BillingMode {
				return fmt.Errorf("model %s has incompatible billing units; use different public model aliases", name)
			}
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
	units := map[string]string{}
	for _, channel := range channels {
		if channel.Status != common.ChannelStatusEnabled || channel.GetSetting().TaskPluginKey != pluginKey {
			continue
		}
		for name := range strings.SplitSeq(channel.Models, ",") {
			if name == "" {
				continue
			}
			upstream, config, err := channel.taskVideoModelConfig(name, source)
			if err != nil {
				return nil, fmt.Errorf("channel %d: %w", channel.Id, err)
			}
			if config.BillingMode != "" {
				folded := strings.ToLower(name)
				if previous, exists := units[folded]; exists && previous != config.BillingMode {
					return nil, fmt.Errorf("model %s has incompatible billing units; use different public model aliases", name)
				}
				units[folded] = config.BillingMode
			}
			for _, entry := range []struct {
				target map[string]dto.TaskVideoModelConfig
				key    string
			}{{catalog, upstream}, {publicCatalog, name}} {
				merged := config
				if previous, exists := entry.target[entry.key]; exists {
					merged = jsplugin.MergeTaskVideoModel(previous, config, relaycommon.MaxTaskDurationSeconds)
				}
				entry.target[entry.key] = merged
			}
		}
	}
	maps.Copy(catalog, publicCatalog)
	return catalog, nil
}
