package service

import (
	"context"
	"errors"
	"path/filepath"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type MediaRuntimeConfig struct {
	VideoAsyncEnabled   bool   `json:"video_async_enabled"`
	LocalPath           string `json:"local_path"`
	RetentionDays       int    `json:"retention_days"`
	SignedURLExpiry     int    `json:"signed_url_expiry_seconds"`
	MaxFileBytes        int64  `json:"max_file_bytes"`
	DownloadTimeout     int    `json:"download_timeout_seconds"`
	DownloadConcurrency int    `json:"download_concurrency"`
	StorageRetries      int    `json:"storage_retry_attempts"`
}

type VideoReadinessCheck struct {
	Key    string `json:"key"`
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

type VideoReadiness struct {
	Ready  bool                  `json:"ready"`
	Checks []VideoReadinessCheck `json:"checks"`
}

func DefaultMediaRuntimeConfig() MediaRuntimeConfig {
	return MediaRuntimeConfig{LocalPath: "data/media", RetentionDays: 90, SignedURLExpiry: 3600, MaxFileBytes: 1 << 30, DownloadTimeout: 900, DownloadConcurrency: 2, StorageRetries: 5}
}

func (cfg MediaRuntimeConfig) Validate() error {
	if cfg.LocalPath == "" || cfg.RetentionDays < 1 || cfg.RetentionDays > 36500 || cfg.SignedURLExpiry < 1 || cfg.SignedURLExpiry > 604800 || cfg.MaxFileBytes < 1 || cfg.MaxFileBytes > 16<<30 || cfg.DownloadTimeout < 1 || cfg.DownloadTimeout > 86400 || cfg.DownloadConcurrency < 1 || cfg.DownloadConcurrency > 32 || cfg.StorageRetries < 0 || cfg.StorageRetries > 100 {
		return errors.New("invalid media storage limits")
	}
	_, err := filepath.Abs(cfg.LocalPath)
	return err
}

func GetMediaRuntimeConfig(ctx context.Context) (MediaRuntimeConfig, error) {
	cfg := DefaultMediaRuntimeConfig()
	var option model.Option
	err := model.DB.WithContext(ctx).Where(map[string]any{"key": "media_runtime_config"}).Take(&option).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := common.UnmarshalJsonStr(option.Value, &cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

func SaveMediaRuntimeConfig(ctx context.Context, cfg MediaRuntimeConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := common.Marshal(cfg)
	if err != nil {
		return err
	}
	return model.DB.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value"})}).Create(&model.Option{Key: "media_runtime_config", Value: string(data)}).Error
}

// GetVideoReadiness reports only operational state. It deliberately excludes
// key material, Redis addresses, and other secret configuration values.
func GetVideoReadiness(ctx context.Context) (VideoReadiness, error) {
	cfg, err := GetMediaRuntimeConfig(ctx)
	if err != nil {
		return VideoReadiness{}, err
	}
	checks := []VideoReadinessCheck{
		{Key: "video_async_enabled", Ready: cfg.VideoAsyncEnabled, Reason: "Global asynchronous video generation is disabled"},
		{Key: "task_plugins", Ready: constant.TaskPluginEnabled, Reason: "Task plugins are disabled"},
		{Key: "redis", Ready: common.RedisEnabled && common.RDB != nil, Reason: "Redis is disabled or unavailable"},
		{Key: "payload_encryption", Ready: ImageEncryptionAvailable(), Reason: "Asynchronous media payload encryption keys are unavailable"},
		{Key: "url_signing", Ready: common.CryptoSecret != "", Reason: "Media URL signing key is unavailable"},
	}
	hasVideoPlugin := false
	if checks[1].Ready {
		snapshot := jsplugin.DefaultRegistry.Snapshot()
		for _, meta := range slices.Concat(snapshot.Override, snapshot.Factory) {
			plugin, found := jsplugin.DefaultRegistry.Get(meta.Key)
			if found && slices.ContainsFunc(plugin.Meta.Protocols, func(claim jsplugin.ProtocolClaim) bool { return claim.Name == "openai_video" }) {
				hasVideoPlugin = true
				break
			}
		}
	}
	if checks[1].Ready && !hasVideoPlugin {
		checks[1].Ready = false
		checks[1].Reason = "No active video task plugin is registered"
	}
	if checks[2].Ready {
		if pingErr := common.RDB.Ping(ctx).Err(); pingErr != nil {
			checks[2].Ready = false
			checks[2].Reason = "Redis health check failed"
		}
	}
	readiness := VideoReadiness{Ready: true, Checks: checks}
	for i := range readiness.Checks {
		if readiness.Checks[i].Ready {
			readiness.Checks[i].Reason = ""
		} else {
			readiness.Ready = false
		}
	}
	return readiness, nil
}
