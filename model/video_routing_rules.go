package model

import (
	"context"
	"errors"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const VideoRoutingConfigOptionKey = "video_routing_rules"

type VideoRoutingConfig struct {
	Version  int                `json:"version"`
	Revision int64              `json:"revision"`
	Enabled  bool               `json:"enabled"`
	Rules    []VideoRoutingRule `json:"rules"`
}

type VideoRoutingRule struct {
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	Enabled     bool                 `json:"enabled"`
	Match       VideoRoutingMatch    `json:"match"`
	Targets     []VideoRoutingTarget `json:"targets"`
	OnExhausted string               `json:"on_exhausted"`
}

type VideoRoutingMatch struct {
	Groups      []string            `json:"groups"`
	Models      []string            `json:"models"`
	Resolutions []string            `json:"resolutions"`
	Modes       []string            `json:"modes"`
	VideoInputs []string            `json:"video_inputs"`
	Seconds     VideoRoutingSeconds `json:"seconds"`
}

type VideoRoutingSeconds struct {
	Min          *int `json:"min,omitempty"`
	Max          *int `json:"max,omitempty"`
	MinExclusive bool `json:"min_exclusive"`
	MaxExclusive bool `json:"max_exclusive"`
}

// Model names the alias advertised by this channel, before its model mapping.
type VideoRoutingTarget struct {
	ChannelID  int    `json:"channel_id"`
	Model      string `json:"model"`
	Resolution string `json:"resolution"`
}

func GetVideoRoutingConfig(ctx context.Context) (VideoRoutingConfig, error) {
	config := VideoRoutingConfig{Version: 1, Rules: []VideoRoutingRule{}}
	var option Option
	err := DB.WithContext(ctx).Where(map[string]any{"key": VideoRoutingConfigOptionKey}).Take(&option).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	// Decode into a zero value so a missing version cannot masquerade as v1.
	config = VideoRoutingConfig{}
	if err := common.UnmarshalJsonStr(option.Value, &config); err != nil {
		return config, err
	}
	if config.Version != 1 || config.Revision < 0 || config.Revision > MaxOptionRevision {
		return config, errors.New("Unsupported or invalid video routing configuration")
	}
	return normalizeVideoRoutingCollections(config), nil
}

// SaveVideoRoutingConfig requires the revision read by the administrator. The
// service validates rule capabilities before this persistence boundary.
func SaveVideoRoutingConfig(ctx context.Context, config VideoRoutingConfig, revision int64) (VideoRoutingConfig, error) {
	if config.Version != 1 {
		return config, errors.New("Unsupported video routing configuration version")
	}
	if revision < 0 || revision >= MaxOptionRevision {
		return config, ErrOptionRevisionInvalid
	}
	config = normalizeVideoRoutingCollections(config)
	encoded, err := common.Marshal(config)
	if err != nil {
		return config, err
	}
	config.Revision, err = SaveVersionedOption(ctx, VideoRoutingConfigOptionKey, string(encoded), &revision)
	return config, err
}

// Keep the configuration JSON arrays stable for clients, including drafts sent
// without optional match selectors. Do not mutate the caller's rule slice.
func normalizeVideoRoutingCollections(config VideoRoutingConfig) VideoRoutingConfig {
	config.Rules = slices.Clone(config.Rules)
	if config.Rules == nil {
		config.Rules = []VideoRoutingRule{}
	}
	for i := range config.Rules {
		match := &config.Rules[i].Match
		for _, values := range []*[]string{&match.Groups, &match.Models, &match.Resolutions, &match.Modes, &match.VideoInputs} {
			if *values == nil {
				*values = []string{}
			}
		}
		if config.Rules[i].Targets == nil {
			config.Rules[i].Targets = []VideoRoutingTarget{}
		}
	}
	return config
}
