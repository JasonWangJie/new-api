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
)

func (channel *Channel) TaskVideoModelConfig(publicModel string) (string, dto.TaskVideoModelConfig, error) {
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
	config, exists := settings.TaskPluginModelConfigs[upstream]
	if !exists {
		return "", config, fmt.Errorf("model %s requires a saved capability configuration", upstream)
	}
	return upstream, config, config.Validate(relaycommon.MaxTaskDurationSeconds)
}

// ValidateTaskVideoModelBindings prevents one retail model from changing units
// with channel selection. Different public aliases may use different units.
func (channel *Channel) ValidateTaskVideoModelBindings() error {
	return channel.validateTaskVideoModelBindings(DB)
}

func (channel *Channel) validateTaskVideoModelBindings(db *gorm.DB) error {
	if channel.Type != constant.ChannelTypeTaskPlugin || channel.GetSetting().TaskPluginKey != "sudashui" {
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
		if other.GetSetting().TaskPluginKey != "sudashui" {
			continue
		}
		for name := range strings.SplitSeq(other.Models, ",") {
			unit, exists := units[strings.ToLower(name)]
			if !exists {
				continue
			}
			_, config, err := other.TaskVideoModelConfig(name)
			if err == nil && unit != config.BillingMode {
				return fmt.Errorf("model %s has incompatible billing units; use different public model aliases", name)
			}
		}
	}
	return nil
}

func taskVideoModelCatalog(channels []Channel) (map[string]dto.TaskVideoModelConfig, error) {
	catalog := map[string]dto.TaskVideoModelConfig{}
	publicCatalog := map[string]dto.TaskVideoModelConfig{}
	units := map[string]string{}
	for _, channel := range channels {
		if channel.Status != common.ChannelStatusEnabled || channel.GetSetting().TaskPluginKey != "sudashui" {
			continue
		}
		for name := range strings.SplitSeq(channel.Models, ",") {
			if name == "" {
				continue
			}
			upstream, config, err := channel.TaskVideoModelConfig(name)
			if err != nil {
				return nil, fmt.Errorf("channel %d: %w", channel.Id, err)
			}
			folded := strings.ToLower(name)
			if previous, exists := units[folded]; exists && previous != config.BillingMode {
				return nil, fmt.Errorf("model %s has incompatible billing units; use different public model aliases", name)
			}
			units[folded] = config.BillingMode
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
