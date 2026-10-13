package service

import (
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

type ImageRecoveryRate struct {
	Succeeded int64    `json:"succeeded"`
	Resolved  int64    `json:"resolved"`
	Percent   *float64 `json:"percent"`
}

func (rate *ImageRecoveryRate) observe(succeeded bool) {
	rate.Resolved++
	if succeeded {
		rate.Succeeded++
	}
	percent := float64(rate.Succeeded) * 100 / float64(rate.Resolved)
	rate.Percent = &percent
}

type ImageReliabilityStats struct {
	Total           int64             `json:"total"`
	Succeeded       int64             `json:"succeeded"`
	Failed          int64             `json:"failed"`
	Expired         int64             `json:"expired"`
	Unknown         int64             `json:"execution_unknown"`
	Pending         int64             `json:"pending"`
	Postprocess     int64             `json:"postprocess_pending"`
	Terminated      int64             `json:"admin_terminated"`
	Audited         int64             `json:"audited_tasks"`
	Final           ImageRecoveryRate `json:"final_success"`
	FirstSubmission ImageRecoveryRate `json:"first_submission_success"`
	Retry           ImageRecoveryRate `json:"retry_recovery"`
	Switch          ImageRecoveryRate `json:"account_switch_recovery"`
	LocalFallback   ImageRecoveryRate `json:"local_fallback_recovery"`
}

// Observe counts resolved tasks once. Unknown execution, pending work, and
// manual termination cannot be treated as completed generation failures.
func (stats *ImageReliabilityStats) Observe(task model.AsyncImageTask, attempts []ImageChannelAttempt) {
	stats.Total++
	if task.ErrorCode == "admin_terminated" {
		stats.Terminated++
		return
	}
	resolved, succeeded := false, task.Status == model.ImageTaskSucceeded
	switch task.Status {
	case model.ImageTaskSucceeded:
		stats.Succeeded++
		resolved = true
	case model.ImageTaskFailed:
		stats.Failed++
		resolved = true
	case model.ImageTaskExpired:
		stats.Expired++
		resolved = true
	case model.ImageTaskExecutionUnknown:
		stats.Unknown++
	case model.ImageTaskStorageFailed, model.ImageTaskBillingFailed, model.ImageTaskUpstreamSucceeded, model.ImageTaskUploading, model.ImageTaskBillingPending:
		stats.Postprocess++
	default:
		stats.Pending++
	}
	if resolved {
		stats.Final.observe(succeeded)
	}
	audited := len(attempts) > 0
	dispatches, switched, fallback := 0, false, false
	previousAccount := ""
	for _, attempt := range attempts {
		audited = audited && attempt.AuditVersion == 1 && attempt.KeyFingerprint != ""
		if !attempt.Dispatched {
			continue
		}
		dispatches++
		if previousAccount != "" && previousAccount != attempt.KeyFingerprint {
			switched = true
		}
		previousAccount = attempt.KeyFingerprint
		fallback = fallback || attempt.ReferenceFallback && attempt.TransportUsed == "local"
	}
	if !audited {
		return // Legacy configured modes cannot prove actual local fallback.
	}
	stats.Audited++
	if !resolved || dispatches == 0 {
		return
	}
	stats.FirstSubmission.observe(succeeded && dispatches == 1)
	if dispatches > 1 {
		stats.Retry.observe(succeeded)
	}
	if switched {
		stats.Switch.observe(succeeded)
	}
	if fallback {
		stats.LocalFallback.observe(succeeded)
	}
}

type ImageReliabilityGroup struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	ChannelID   int    `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	ImageReliabilityStats
}

type ImageReliabilityReport struct {
	Totals ImageReliabilityStats   `json:"totals"`
	Groups []ImageReliabilityGroup `json:"groups"`
}

// ImageReliability reads the entire filtered scope in bounded batches. It never
// reads encrypted requests or provider bodies and is independent of list pages.
func ImageReliability(query *gorm.DB) (ImageReliabilityReport, error) {
	report := ImageReliabilityReport{Groups: make([]ImageReliabilityGroup, 0)}
	groups := make(map[string]*ImageReliabilityGroup)
	var batch []model.AsyncImageTask
	err := query.Select("id", "task_id", "status", "error_code", "provider", "platform", "model", "channel_id", "attempts").FindInBatches(&batch, 500, func(_ *gorm.DB, _ int) error {
		for _, task := range batch {
			var attempts []ImageChannelAttempt
			if common.UnmarshalJsonStr(task.Attempts, &attempts) != nil {
				attempts = nil
			}
			channelID := task.ChannelId
			for _, attempt := range attempts {
				if attempt.Dispatched {
					channelID = attempt.ChannelId
				}
			}
			provider := task.Provider
			if provider == "" {
				provider = task.Platform
			}
			key := provider + "\x00" + task.Model + "\x00" + strconv.Itoa(channelID)
			group := groups[key]
			if group == nil {
				group = &ImageReliabilityGroup{Provider: provider, Model: task.Model, ChannelID: channelID}
				groups[key] = group
			}
			report.Totals.Observe(task, attempts)
			group.Observe(task, attempts)
		}
		return nil
	}).Error
	if err != nil {
		return report, err
	}
	for _, group := range groups {
		report.Groups = append(report.Groups, *group)
	}
	slices.SortFunc(report.Groups, func(a, b ImageReliabilityGroup) int {
		if order := strings.Compare(a.Provider, b.Provider); order != 0 {
			return order
		}
		if order := strings.Compare(a.Model, b.Model); order != 0 {
			return order
		}
		return a.ChannelID - b.ChannelID
	})
	return report, nil
}
