package service

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
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
