package model

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// Finished jobs expire one day after completion, even when the job itself ran
// for several days. Legacy previews and failures without a finish time use
// their creation time. Uncertain storage deletes retain their recovery ledger.
const imageCleanupExpiredHistory = `COALESCE(image_cleanup_jobs.status, '') IN ('completed', 'completed_with_errors', 'cancelled', 'failed', 'preview_ready')
 AND ((COALESCE(image_cleanup_jobs.finished_at, 0) > 0 AND image_cleanup_jobs.finished_at <= ?)
 OR (COALESCE(image_cleanup_jobs.finished_at, 0) = 0 AND COALESCE(image_cleanup_jobs.created_at, 0) > 0 AND image_cleanup_jobs.created_at <= ?))
 AND COALESCE(image_cleanup_jobs.lease_expires_at, 0) <= ?
 AND NOT EXISTS (SELECT 1 FROM image_cleanup_retries AS history_retry
 JOIN image_storage_objects AS history_object ON history_object.object_id = history_retry.object_id
 WHERE history_retry.job_id = image_cleanup_jobs.job_id AND history_object.status = 'deleting')`

// RecentImageCleanupJobs is a fixed recent-history window, not a paginated
// archive. In-progress, paused and uncertain jobs remain eligible for display.
func RecentImageCleanupJobs(ctx context.Context, now int64) ([]ImageCleanupJob, error) {
	var jobs []ImageCleanupJob
	cutoff := now - 86400
	err := DB.WithContext(ctx).Where("NOT ("+imageCleanupExpiredHistory+")", cutoff, cutoff, now).Order("created_at DESC, id DESC").Limit(5).Find(&jobs).Error
	return jobs, err
}

// PruneImageCleanupHistory removes at most 200 detail rows and 20 summaries per
// pass. The parent row lock fences confirmation/control and late worker writes;
// each transaction rechecks age, state, leases and uncertain object deletion.
func PruneImageCleanupHistory(ctx context.Context, now int64) error {
	cutoff := now - 86400
	var ids []int
	if err := DB.WithContext(ctx).Model(&ImageCleanupJob{}).Where(imageCleanupExpiredHistory, cutoff, cutoff, now).Order("id").Limit(20).Pluck("id", &ids).Error; err != nil {
		return err
	}
	remaining := 200
	for _, id := range ids {
		if err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var job ImageCleanupJob
			err := lockForUpdate(tx).Where("id = ?", id).Where(imageCleanupExpiredHistory, cutoff, cutoff, now).Take(&job).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			var details []int
			if remaining > 0 {
				if err := tx.Model(&ImageCleanupRetry{}).Where("job_id = ?", job.JobId).Order("id").Limit(remaining).Pluck("id", &details).Error; err != nil {
					return err
				}
			}
			if len(details) > 0 {
				if err := tx.Where("id IN ? AND job_id = ?", details, job.JobId).Delete(&ImageCleanupRetry{}).Error; err != nil {
					return err
				}
				remaining -= len(details)
			}
			var pending []int
			if err := tx.Model(&ImageCleanupRetry{}).Where("job_id = ?", job.JobId).Limit(1).Pluck("id", &pending).Error; err != nil {
				return err
			}
			if len(pending) > 0 {
				return nil
			}
			return tx.Where("id = ? AND status = ?", job.Id, job.Status).Where(imageCleanupExpiredHistory, cutoff, cutoff, now).Delete(&ImageCleanupJob{}).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// ImageCleanupRetry keeps failed objects discoverable after the scan cursor moves.
type ImageCleanupRetry struct {
	Id            int
	JobId         string `gorm:"size:64;uniqueIndex:idx_image_cleanup_retry,priority:1"`
	ObjectId      string `gorm:"size:64;uniqueIndex:idx_image_cleanup_retry,priority:2"`
	Attempts      int
	NextAttemptAt int64  `gorm:"type:bigint;index"`
	LastError     string `gorm:"type:text"`
	Outcome       string `gorm:"size:16"`
}

// ImageCleanupSchedule deduplicates a policy's time slot across application nodes.
type ImageCleanupSchedule struct {
	PolicyId string `gorm:"size:64;primaryKey"`
	Slot     int64  `gorm:"type:bigint"`
}

func MigrateImageCleanupModels(db *gorm.DB) error {
	if err := db.AutoMigrate(&ImageCleanupRetry{}, &ImageCleanupSchedule{}); err != nil {
		return err
	}
	// AutoMigrate adds nullable columns to existing rows on all supported
	// dialects. Explicit zeros keep old queued jobs visible to due/control CAS.
	for _, column := range []string{"cutoff_at", "max_object_id", "scanned", "matched", "matched_bytes", "protected", "reference_skipped", "retention_skipped", "location_skipped", "deleted", "skipped", "next_attempt_at"} {
		if err := db.Model(&ImageCleanupJob{}).Where(column+" IS NULL").Update(column, 0).Error; err != nil {
			return err
		}
	}
	for _, column := range []string{"source", "preview_token", "control", "policy_id"} {
		if err := db.Model(&ImageCleanupJob{}).Where(column+" IS NULL").Update(column, "").Error; err != nil {
			return err
		}
	}
	return nil
}

// WithImageCleanupJobLease fences late storage responses from an old runner.
// Empty leases are accepted only for reconciliation of stopped jobs.
func WithImageCleanupJobLease(ctx context.Context, id int, lease string, update func(*gorm.DB) error) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job ImageCleanupJob
		if err := lockForUpdate(tx).Where("id = ?", id).Take(&job).Error; err != nil {
			return err
		}
		if job.LeaseToken != lease || lease != "" && job.LeaseExpiresAt <= time.Now().Unix() {
			return ErrImageConflict
		}
		if lease == "" && job.Status != "paused" && job.Status != "cancelled" && job.Status != "completed_with_errors" {
			return ErrImageConflict
		}
		return update(tx)
	})
}
