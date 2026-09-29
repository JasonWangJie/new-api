package model

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
)

var ErrAsyncTaskStatsUnavailable = errors.New("async task statistics are unavailable")

type AsyncTaskStats struct {
	TodayRequests int64
	SuccessCount  int64
	FailureCount  int64
}

// GetAsyncTaskStats counts the owner's persisted jobs created on now's local
// calendar day. Related upstream Task rows contribute status, never request count.
func GetAsyncTaskStats(ctx context.Context, userID int, mediaType string, now time.Time) (AsyncTaskStats, error) {
	var stats AsyncTaskStats
	if DB == nil {
		return stats, ErrAsyncTaskStatsUnavailable
	}
	if mediaType != "image" && mediaType != "video" && mediaType != "media" {
		return stats, ErrAsyncTaskStatsUnavailable
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := start.AddDate(0, 0, 1)

	if mediaType == "image" || mediaType == "media" {
		err := DB.WithContext(ctx).Model(&AsyncImageTask{}).
			Where("user_id = ? AND created_at >= ? AND created_at < ?", userID, start.Unix(), end.Unix()).
			Select(`COUNT(*) AS today_requests,
				COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS success_count,
				COALESCE(SUM(CASE WHEN status IN ? THEN 1 ELSE 0 END), 0) AS failure_count`,
				ImageTaskSucceeded, []string{ImageTaskFailed, ImageTaskExpired, ImageTaskExecutionUnknown, ImageTaskStorageFailed, ImageTaskBillingFailed}).
			Scan(&stats).Error
		if err != nil {
			return AsyncTaskStats{}, err
		}
	}
	if mediaType == "video" || mediaType == "media" {
		// A saved result can coexist with unresolved billing. Failure wins until
		// reconciliation or retry clears it; upstream success alone is not delivery.
		jobs := DB.WithContext(ctx).Model(&AsyncMediaJob{}).
			Where("user_id = ? AND created_at >= ? AND created_at < ?", userID, start.Unix(), end.Unix()).
			Select(`CASE
				WHEN status IN ? OR storage_status = ? OR billing_status = ?
					OR EXISTS (SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status = ?)
					THEN 'failure'
				WHEN status = ? THEN 'success'
				ELSE 'pending' END AS outcome`,
				[]string{"failed", "expired", "execution_unknown", "storage_failed", "billing_failed"}, "failed", "failed", TaskStatusFailure, "succeeded")
		var videos AsyncTaskStats
		err := DB.WithContext(ctx).Table("(?) AS daily_video_jobs", jobs).
			Select(`COUNT(*) AS today_requests,
				COALESCE(SUM(CASE WHEN outcome = 'success' THEN 1 ELSE 0 END), 0) AS success_count,
				COALESCE(SUM(CASE WHEN outcome = 'failure' THEN 1 ELSE 0 END), 0) AS failure_count`).
			Scan(&videos).Error
		if err != nil {
			return AsyncTaskStats{}, err
		}
		stats.TodayRequests += videos.TodayRequests
		stats.SuccessCount += videos.SuccessCount
		stats.FailureCount += videos.FailureCount
	}
	return stats, nil
}

// GetAsyncTaskStatsQuota reads the live balance without hydrating or changing
// quota caches. During Redis-backed batch accounting, only an existing valid
// cache can include reservations that have not reached the database yet.
func GetAsyncTaskStatsQuota(ctx context.Context, userID int) (int, error) {
	if common.RedisEnabled {
		var userCache *UserBase
		var cacheErr error
		if common.RDB == nil {
			cacheErr = errors.New("async task statistics quota cache is unavailable")
		} else {
			userCache, cacheErr = cacheGetUserBase(userID)
		}
		if cacheErr == nil {
			return userCache.Quota, nil
		}
		if common.BatchUpdateEnabled {
			return 0, cacheErr
		}
	}
	if DB == nil {
		return 0, ErrAsyncTaskStatsUnavailable
	}
	var user User
	if err := DB.WithContext(ctx).Select("quota").Where("id = ?", userID).Take(&user).Error; err != nil {
		return 0, err
	}
	return user.Quota, nil
}
