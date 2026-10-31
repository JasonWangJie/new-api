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
)

type MediaRuntimeConfig struct {
	Revision                   int64  `json:"revision"`
	VideoAsyncEnabled          bool   `json:"video_async_enabled"`
	LocalPath                  string `json:"local_path"`
	RetentionDays              int    `json:"retention_days"`
	SignedURLExpiry            int    `json:"signed_url_expiry_seconds"`
	MaxFileBytes               int64  `json:"max_file_bytes"`
	DownloadTimeout            int    `json:"download_timeout_seconds"`
	DownloadConcurrency        int    `json:"download_concurrency"`
	SubmitConcurrency          int    `json:"submit_concurrency"`
	StatusConcurrency          int    `json:"status_concurrency"`
	SubmitTimeout              int    `json:"submit_timeout_seconds"`
	PollTimeout                int    `json:"poll_timeout_seconds"`
	ReconciliationTimeout      int    `json:"reconciliation_timeout_minutes"`
	ReconciliationPollInterval int    `json:"reconciliation_poll_interval_seconds"`
	StorageRetries             int    `json:"storage_retry_attempts"`
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
	return MediaRuntimeConfig{LocalPath: "data/media", RetentionDays: 90, SignedURLExpiry: 3600, MaxFileBytes: 1 << 30, DownloadTimeout: 900, DownloadConcurrency: 2, SubmitConcurrency: 2, StatusConcurrency: 8, SubmitTimeout: 120, PollTimeout: 30, ReconciliationTimeout: 1440, ReconciliationPollInterval: 60, StorageRetries: 5}
}

// Older configuration clients omit the added worker settings. Preserve their
// former submission limits while separating the download budget.
func (cfg MediaRuntimeConfig) withWorkerDefaults() MediaRuntimeConfig {
	defaults := DefaultMediaRuntimeConfig()
	if cfg.SubmitConcurrency == 0 {
		cfg.SubmitConcurrency = cfg.DownloadConcurrency
		if cfg.SubmitConcurrency == 0 {
			cfg.SubmitConcurrency = defaults.SubmitConcurrency
		}
	}
	if cfg.StatusConcurrency == 0 {
		cfg.StatusConcurrency = defaults.StatusConcurrency
	}
	if cfg.SubmitTimeout == 0 {
		cfg.SubmitTimeout = cfg.DownloadTimeout
		if cfg.SubmitTimeout == 0 {
			cfg.SubmitTimeout = defaults.SubmitTimeout
		}
	}
	return cfg
}

func (cfg MediaRuntimeConfig) Validate() error {
	cfg = cfg.withWorkerDefaults()
	if cfg.Revision < 0 || cfg.Revision > model.MaxOptionRevision {
		return model.ErrOptionRevisionInvalid
	}
	if cfg.PollTimeout < 1 || cfg.PollTimeout > 300 {
		return errors.New("video polling timeout must be between 1 and 300 seconds")
	}
	if cfg.ReconciliationTimeout < 1 || cfg.ReconciliationTimeout > 10080 {
		return errors.New("video reconciliation timeout must be between 1 and 10080 minutes")
	}
	if cfg.ReconciliationPollInterval < 5 || cfg.ReconciliationPollInterval > 3600 {
		return errors.New("video reconciliation polling interval must be between 5 and 3600 seconds")
	}
	if cfg.SubmitConcurrency < 1 || cfg.SubmitConcurrency > 32 || cfg.StatusConcurrency < 1 || cfg.StatusConcurrency > 64 || cfg.SubmitTimeout < 1 || cfg.SubmitTimeout > 86400 {
		return errors.New("invalid media worker limits")
	}
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
	// Existing saved configs used the download limits for submission as well.
	// New installations keep the independently chosen defaults above.
	cfg.SubmitConcurrency, cfg.SubmitTimeout = 0, 0
	if err := common.UnmarshalJsonStr(option.Value, &cfg); err != nil {
		return cfg, err
	}
	cfg = cfg.withWorkerDefaults()
	return cfg, cfg.Validate()
}

func SaveMediaRuntimeConfig(ctx context.Context, cfg MediaRuntimeConfig, expectedRevision ...int64) error {
	cfg = cfg.withWorkerDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := common.Marshal(cfg)
	if err != nil {
		return err
	}
	var expected *int64
	if len(expectedRevision) > 0 {
		expected = &expectedRevision[0]
	}
	_, err = model.SaveVersionedOption(ctx, "media_runtime_config", string(data), expected)
	return err
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
