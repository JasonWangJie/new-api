package service

import (
	"context"
	"errors"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

type ImageCleanupPolicy struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Enabled         bool   `json:"enabled"`
	ChannelID       string `json:"channel_id"`
	OlderThan       int    `json:"older_than"`
	AgeUnit         string `json:"age_unit"`
	ScheduleType    string `json:"schedule_type"`
	IntervalMinutes int    `json:"interval_minutes"`
	DailyTime       string `json:"daily_time"`
	Timezone        string `json:"timezone"`
	NextRunAt       int64  `json:"next_run_at"`
}

type ImageCleanupConfig struct {
	Revision              int64                `json:"revision"`
	Enabled               bool                 `json:"enabled"`
	Concurrency           int                  `json:"concurrency"`
	RequestsPerSecond     int                  `json:"requests_per_second"`
	BatchSize             int                  `json:"batch_size"`
	ScanPagesPerSecond    int                  `json:"scan_pages_per_second"`
	RequestTimeoutSeconds int                  `json:"request_timeout_seconds"`
	CPUPausePercent       int                  `json:"cpu_pause_percent"`
	MemoryPausePercent    int                  `json:"memory_pause_percent"`
	Policies              []ImageCleanupPolicy `json:"policies"`
}

func DefaultImageCleanupConfig() ImageCleanupConfig {
	return ImageCleanupConfig{Concurrency: 1, RequestsPerSecond: 2, BatchSize: 50, ScanPagesPerSecond: 1, RequestTimeoutSeconds: 15, CPUPausePercent: 70, MemoryPausePercent: 80, Policies: []ImageCleanupPolicy{}}
}

func GetImageCleanupConfig(ctx context.Context) (ImageCleanupConfig, error) {
	cfg := DefaultImageCleanupConfig()
	var option model.Option
	err := model.DB.WithContext(ctx).Where(map[string]any{"key": "image_cleanup_config"}).Take(&option).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := common.UnmarshalJsonStr(option.Value, &cfg); err != nil {
		return cfg, err
	}
	for i := range cfg.Policies {
		if cfg.Policies[i].Timezone == "" {
			cfg.Policies[i].Timezone = "Asia/Shanghai"
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	var schedules []model.ImageCleanupSchedule
	if len(cfg.Policies) > 0 {
		ids := make([]string, 0, len(cfg.Policies))
		for _, policy := range cfg.Policies {
			ids = append(ids, policy.ID)
		}
		if err := model.DB.WithContext(ctx).Where("policy_id IN ?", ids).Find(&schedules).Error; err != nil {
			return cfg, err
		}
	}
	slots := make(map[string]int64, len(schedules))
	for _, schedule := range schedules {
		slots[schedule.PolicyId] = schedule.Slot
	}
	now := time.Now()
	for i, policy := range cfg.Policies {
		cfg.Policies[i].NextRunAt = 0
		if !cfg.Enabled || !policy.Enabled {
			continue
		}
		slot, next := imageCleanupPolicyWindow(policy, now)
		if slot > slots[policy.ID] {
			next = now.Unix()
		}
		cfg.Policies[i].NextRunAt = next
	}
	return cfg, nil
}

func (cfg ImageCleanupConfig) Validate() error {
	if cfg.Revision < 0 || cfg.Revision > model.MaxOptionRevision || cfg.Concurrency < 1 || cfg.Concurrency > 8 || cfg.RequestsPerSecond < 1 || cfg.RequestsPerSecond > 20 || cfg.BatchSize < 20 || cfg.BatchSize > 200 || cfg.ScanPagesPerSecond < 1 || cfg.ScanPagesPerSecond > 5 || cfg.RequestTimeoutSeconds < 5 || cfg.RequestTimeoutSeconds > 60 || cfg.CPUPausePercent < 30 || cfg.CPUPausePercent > 90 || cfg.MemoryPausePercent < 40 || cfg.MemoryPausePercent > 90 || len(cfg.Policies) > 50 {
		return errors.New("invalid image cleanup resource limits")
	}
	ids := map[string]bool{}
	channels := map[string]bool{}
	for _, policy := range cfg.Policies {
		if policy.ID == "" || len(policy.ID) > 64 || ids[policy.ID] || strings.TrimSpace(policy.Name) == "" || len(policy.Name) > 200 || policy.ChannelID == "" || len(policy.ChannelID) > 64 {
			return errors.New("invalid image cleanup policy identity")
		}
		ids[policy.ID] = true
		if channels[policy.ChannelID] {
			return errors.New("only one cleanup policy is allowed per storage channel")
		}
		channels[policy.ChannelID] = true
		if _, err := (ImageCleanupFilters{OlderThan: policy.OlderThan, AgeUnit: policy.AgeUnit}).AgeSeconds(); err != nil || policy.OlderThan == 0 {
			return errors.New("invalid image cleanup retention age")
		}
		if _, err := time.LoadLocation(policy.Timezone); err != nil {
			return errors.New("invalid image cleanup timezone")
		}
		switch policy.ScheduleType {
		case "interval":
			if policy.IntervalMinutes < 5 || policy.IntervalMinutes > 43200 {
				return errors.New("image cleanup interval must be between 5 and 43200 minutes")
			}
		case "daily":
			if _, err := time.Parse("15:04", policy.DailyTime); err != nil {
				return errors.New("invalid image cleanup daily time")
			}
		default:
			return errors.New("invalid image cleanup schedule")
		}
	}
	return nil
}

func SaveImageCleanupConfig(ctx context.Context, cfg ImageCleanupConfig) error {
	for i := range cfg.Policies {
		cfg.Policies[i].NextRunAt = 0
		if cfg.Policies[i].Timezone == "" {
			cfg.Policies[i].Timezone = "Asia/Shanghai"
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	for _, policy := range cfg.Policies {
		if _, err := model.ImageStorageChannelProfiles(ctx, policy.ChannelID); err != nil {
			return err
		}
	}
	data, err := common.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = model.SaveVersionedOption(ctx, "image_cleanup_config", string(data), &cfg.Revision)
	return err
}

// imageCleanupPolicyWindow shares timezone and interval boundaries between
// scheduler execution and the administrator's next-run preview.
func imageCleanupPolicyWindow(policy ImageCleanupPolicy, now time.Time) (int64, int64) {
	if policy.ScheduleType == "interval" {
		interval := int64(policy.IntervalMinutes) * 60
		slot := now.Unix() / interval * interval
		return slot, slot + interval
	}
	location, _ := time.LoadLocation(policy.Timezone)
	local := now.In(location)
	clock, _ := time.Parse("15:04", policy.DailyTime)
	due := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, location)
	if now.Before(due) {
		return 0, due.Unix()
	}
	return due.Unix(), due.AddDate(0, 0, 1).Unix()
}
