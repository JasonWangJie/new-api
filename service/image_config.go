package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ImageRuntimeConfig struct {
	AsyncEnabled          bool   `json:"async_enabled"`
	AutoArchive           bool   `json:"auto_archive_to_library"`
	Workers               int    `json:"worker_concurrency"`
	WorkerLease           int    `json:"worker_lease_seconds"`
	RecoveryInterval      int    `json:"recovery_interval_seconds"`
	ExecutionTimeout      int    `json:"execution_timeout_seconds"`
	AttemptTimeout        int    `json:"account_attempt_timeout_seconds"`
	ImageConcurrency      int    `json:"image_concurrency"`
	StorageRetries        int    `json:"storage_retry_attempts"`
	BillingRetries        int    `json:"billing_retry_attempts"`
	RetryBackoff          int    `json:"retry_backoff_seconds"`
	OpenAIReferenceMode   string `json:"openai_reference_transport_mode"`
	GeminiReferenceMode   string `json:"gemini_reference_transport_mode"`
	GeminiAccountSwitches int    `json:"gemini_async_max_account_switches"`
	CircuitBreaker        bool   `json:"image_circuit_breaker_enabled"`
	FailureThreshold      int    `json:"failure_threshold"`
	Cooldown              int    `json:"cooldown_seconds"`
	ReferenceRetries      int    `json:"reference_fetch_max_retries"`
	ReferenceRetryBase    int    `json:"reference_retry_base_seconds"`
	ReferenceRetryMax     int    `json:"reference_retry_max_seconds"`
	TransientRetries      int    `json:"upstream_transient_max_retries"`
	TransientRetryBase    int    `json:"upstream_transient_retry_base_seconds"`
	TransientRetryMax     int    `json:"upstream_transient_retry_max_seconds"`
	CapacityRetries       int    `json:"capacity_max_retries"`
	CapacityRetryBase     int    `json:"capacity_retry_base_seconds"`
	CapacityRetryMax      int    `json:"capacity_retry_max_seconds"`
	TotalRetries          int    `json:"total_max_retries"`
	RetryJitter           int    `json:"retry_jitter_percent"`
	RetryAfterMax         int    `json:"retry_after_max_seconds"`
	DownloadMaxBytes      int64  `json:"download_max_bytes"`
	DownloadMaxPixels     int64  `json:"download_max_pixels"`
	MaxReferences         int    `json:"max_reference_images"`
	ReferenceTotalBytes   int64  `json:"max_reference_total_bytes"`
	ReferenceTotalPixels  int64  `json:"max_reference_total_pixels"`
	DownloadTimeout       int    `json:"download_timeout_seconds"`
	DownloadRedirects     int    `json:"download_max_redirects"`
	ReferenceConcurrency  int    `json:"reference_fetch_concurrency"`
	ReferenceCacheTTL     int    `json:"reference_cache_ttl_seconds"`
	ReferenceCacheBytes   int64  `json:"reference_cache_max_bytes"`
	UploadTimeout         int    `json:"upload_timeout_seconds"`
	UploadsPerMinute      int    `json:"upload_per_minute"`
	InputBytesPerKey      int64  `json:"max_input_bytes_per_key"`
	SingleUploadBytes     int64  `json:"max_upload_bytes"`
	SignedURLExpiry       int    `json:"signed_url_expiry_seconds"`
	InputRetentionHours   int    `json:"input_retention_hours"`
	TaskRetentionDays     int    `json:"task_retention_days"`
	ResultRetentionDays   int    `json:"result_retention_days"`
	PromptPreview         bool   `json:"prompt_preview_enabled"`
	PromptPreviewChars    int    `json:"prompt_preview_max_chars"`
	LibraryRetentionDays  int    `json:"library_retention_days"`
	LibraryItems          int    `json:"library_max_items_per_user"`
	LibraryBytes          int64  `json:"library_max_bytes_per_user"`
	LibraryImageBytes     int64  `json:"library_max_image_bytes"`
	LibraryImagePixels    int64  `json:"library_max_image_pixels"`
	ImportsPerMinute      int    `json:"library_import_per_minute"`
	SubmissionsPerMinute  int    `json:"library_submission_per_minute"`
}

func DefaultImageRuntimeConfig() ImageRuntimeConfig {
	return ImageRuntimeConfig{Workers: 4, WorkerLease: 120, RecoveryInterval: 30, ExecutionTimeout: 1200, AttemptTimeout: 300, ImageConcurrency: 4, StorageRetries: 5, BillingRetries: 10, RetryBackoff: 30, OpenAIReferenceMode: "passthrough_fallback_local", GeminiReferenceMode: "passthrough", GeminiAccountSwitches: 3, FailureThreshold: 5, Cooldown: 300, ReferenceRetries: 2, ReferenceRetryBase: 15, ReferenceRetryMax: 60, TransientRetries: 3, TransientRetryBase: 15, TransientRetryMax: 60, CapacityRetries: 5, CapacityRetryBase: 30, CapacityRetryMax: 300, TotalRetries: 16, RetryJitter: 20, RetryAfterMax: 900, DownloadMaxBytes: 32 << 20, DownloadMaxPixels: 80_000_000, MaxReferences: 8, ReferenceTotalBytes: 64 << 20, ReferenceTotalPixels: 80_000_000, DownloadTimeout: 30, DownloadRedirects: 3, ReferenceConcurrency: 8, ReferenceCacheTTL: 60, ReferenceCacheBytes: 128 << 20, UploadTimeout: 300, UploadsPerMinute: 20, InputBytesPerKey: 1 << 30, SingleUploadBytes: 32 << 20, SignedURLExpiry: 3600, InputRetentionHours: 24, TaskRetentionDays: 90, ResultRetentionDays: 90, PromptPreview: true, PromptPreviewChars: 160, LibraryRetentionDays: 90, LibraryItems: 1000, LibraryBytes: 5 << 30, LibraryImageBytes: 20 << 20, LibraryImagePixels: 40_000_000, ImportsPerMinute: 20, SubmissionsPerMinute: 10}
}

func GetImageRuntimeConfig(ctx context.Context) (ImageRuntimeConfig, error) {
	cfg := DefaultImageRuntimeConfig()
	var option model.Option
	err := model.DB.WithContext(ctx).Where(map[string]any{"key": "image_runtime_config"}).Take(&option).Error
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

func (cfg ImageRuntimeConfig) Validate() error {
	positive := []int{cfg.Workers, cfg.WorkerLease, cfg.RecoveryInterval, cfg.ExecutionTimeout, cfg.AttemptTimeout, cfg.ImageConcurrency, cfg.RetryBackoff, cfg.ReferenceRetryBase, cfg.ReferenceRetryMax, cfg.TransientRetryBase, cfg.TransientRetryMax, cfg.CapacityRetryBase, cfg.CapacityRetryMax, cfg.RetryAfterMax, cfg.FailureThreshold, cfg.Cooldown, cfg.MaxReferences, cfg.DownloadTimeout, cfg.ReferenceConcurrency, cfg.UploadTimeout, cfg.UploadsPerMinute, cfg.SignedURLExpiry, cfg.InputRetentionHours, cfg.TaskRetentionDays, cfg.ResultRetentionDays, cfg.PromptPreviewChars, cfg.LibraryRetentionDays, cfg.LibraryItems, cfg.ImportsPerMinute, cfg.SubmissionsPerMinute}
	for _, value := range positive {
		if value <= 0 {
			return errors.New("image runtime limits must be positive")
		}
	}
	if cfg.StorageRetries < 0 || cfg.BillingRetries < 0 || cfg.ReferenceRetries < 0 || cfg.TransientRetries < 0 || cfg.CapacityRetries < 0 || cfg.TotalRetries < 0 || cfg.GeminiAccountSwitches < 0 || cfg.DownloadRedirects < 0 || cfg.ReferenceCacheTTL < 0 || cfg.ReferenceCacheBytes < 0 || cfg.RetryJitter < 0 || cfg.RetryJitter > 100 {
		return errors.New("invalid image retry/cache limits")
	}
	if cfg.UploadsPerMinute > 1000 || cfg.InputBytesPerKey <= 0 || cfg.InputBytesPerKey > 100<<30 || cfg.SingleUploadBytes <= 0 || cfg.SingleUploadBytes > 64<<20 || cfg.UploadTimeout > 600 || cfg.InputRetentionHours > 720 {
		return errors.New("SC upload limits exceed supported bounds")
	}
	if cfg.DownloadMaxBytes <= 0 || cfg.DownloadMaxPixels <= 0 || cfg.ReferenceTotalBytes <= 0 || cfg.ReferenceTotalPixels <= 0 || cfg.LibraryBytes <= 0 || cfg.LibraryImageBytes <= 0 || cfg.LibraryImagePixels <= 0 || cfg.MaxReferences > 128 {
		return errors.New("invalid image storage limits")
	}
	// Bound administrator values before duration, exponential backoff and
	// response-size arithmetic; configurations can also arrive from the DB.
	if cfg.Workers > 10000 || cfg.ImageConcurrency > 10000 || cfg.ReferenceConcurrency > 10000 || cfg.WorkerLease > 86400 || cfg.ExecutionTimeout > 86400 || cfg.AttemptTimeout > cfg.ExecutionTimeout || cfg.RecoveryInterval > 86400 || cfg.RetryBackoff > 86400 || cfg.Cooldown > 86400 || cfg.DownloadTimeout > 600 || cfg.DownloadRedirects > 10 || cfg.SignedURLExpiry > 604800 || cfg.TaskRetentionDays > 36500 || cfg.ResultRetentionDays > 36500 || cfg.LibraryRetentionDays > 36500 || cfg.ReferenceCacheTTL > 86400 || cfg.ReferenceCacheBytes > 1<<30 || cfg.DownloadMaxBytes > 64<<20 || cfg.DownloadMaxPixels > 320_000_000 || cfg.ReferenceTotalBytes > 1<<30 || cfg.ReferenceTotalPixels > 1_000_000_000 || cfg.LibraryImageBytes > 64<<20 || cfg.LibraryImagePixels > 320_000_000 || cfg.LibraryBytes > 1<<40 {
		return errors.New("image runtime limits exceed supported arithmetic bounds")
	}
	for _, value := range []int{cfg.StorageRetries, cfg.BillingRetries, cfg.ReferenceRetries, cfg.TransientRetries, cfg.CapacityRetries, cfg.TotalRetries, cfg.GeminiAccountSwitches, cfg.FailureThreshold} {
		if value > 1000 {
			return errors.New("image retry budget exceeds supported bounds")
		}
	}
	for _, value := range []int{cfg.ReferenceRetryBase, cfg.ReferenceRetryMax, cfg.TransientRetryBase, cfg.TransientRetryMax, cfg.CapacityRetryBase, cfg.CapacityRetryMax, cfg.RetryAfterMax} {
		if value > 86400 {
			return errors.New("image retry delay exceeds supported bounds")
		}
	}
	for _, mode := range []string{cfg.OpenAIReferenceMode, cfg.GeminiReferenceMode} {
		if mode != "passthrough" && mode != "local" && mode != "passthrough_fallback_local" {
			return errors.New("unsupported reference transport mode")
		}
	}
	return nil
}

func SaveImageRuntimeConfig(ctx context.Context, cfg ImageRuntimeConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := common.Marshal(cfg)
	if err != nil {
		return err
	}
	return model.DB.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value"})}).Create(&model.Option{Key: "image_runtime_config", Value: string(data)}).Error
}

type ImageModelCapability struct {
	Id                 string   `json:"id"`
	Label              string   `json:"label"`
	Qualities          []string `json:"qualities,omitempty"`
	Resolutions        []string `json:"resolutions,omitempty"`
	Formats            []string `json:"formats,omitempty"`
	Backgrounds        []string `json:"backgrounds,omitempty"`
	MaxOutputImages    int      `json:"max_output_images"`
	MaxReferenceImages int      `json:"max_reference_images"`
	AllowHalfK         bool     `json:"allow_half_k"`
}

func ValidateImageModelName(name string) error {
	if name != strings.TrimSpace(name) || name == "" || len(name) > 255 || !utf8.ValidString(name) || strings.Contains(name, "*") {
		return errors.New("invalid image model name")
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return errors.New("invalid image model name")
		}
	}
	return nil
}

func ImageIdentityHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ResolveImagePolicy(ctx context.Context, token model.Token, platform string) (model.ImageGroupPolicy, []ImageModelCapability, error) {
	group := token.Group
	if group == "" {
		var user model.User
		if err := model.DB.WithContext(ctx).Where("id = ?", token.UserId).First(&user).Error; err != nil {
			return model.ImageGroupPolicy{}, nil, err
		}
		group = user.Group
	}
	var mapping model.TokenImagePlatformMapping
	err := model.DB.WithContext(ctx).Where("token_id = ? AND platform = ?", token.Id, platform).Take(&mapping).Error
	if err == nil {
		group = mapping.Group
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return model.ImageGroupPolicy{}, nil, err
	}
	var policy model.ImageGroupPolicy
	err = model.DB.WithContext(ctx).Where("policy_key = ?", ImageIdentityHash(group, platform)).Take(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		policy = model.ImageGroupPolicy{Group: group, Platform: platform, PoolMode: "resolution"}
		return policy, nil, nil
	}
	if err != nil {
		return policy, nil, err
	}
	var models []ImageModelCapability
	if err := common.UnmarshalJsonStr(policy.Models, &models); err != nil {
		return policy, nil, fmt.Errorf("invalid image model catalog: %w", err)
	}
	for i := range models {
		if models[i].Label == "" {
			models[i].Label = models[i].Id
		}
		if models[i].MaxOutputImages == 0 {
			models[i].MaxOutputImages = dto.MaxImageN
		}
		if len(models[i].Resolutions) == 0 {
			models[i].Resolutions = []string{"1K", "2K", "4K"}
		}
	}
	return policy, models, nil
}
