package model

import (
	"context"

	"gorm.io/gorm"
)

const (
	MediaReplayImage            = "image"
	MediaReplayVideo            = "video"
	MediaReplayRetentionSeconds = 24 * 60 * 60
)

// MediaReplaySnapshot is an encrypted copy of the accepted client request. A
// pending snapshot has no failure deadline and is not available to readers.
type MediaReplaySnapshot struct {
	Id          int64    `json:"-"`
	TaskId      string   `json:"-" gorm:"size:64;not null;uniqueIndex:idx_media_replay_task,priority:1"`
	MediaType   string   `json:"-" gorm:"size:8;not null;uniqueIndex:idx_media_replay_task,priority:2"`
	Payload     []byte   `json:"-" gorm:"not null"`
	FailedAt    int64    `json:"-"`
	RetainUntil int64    `json:"-" gorm:"index:idx_media_replay_expiry"`
	InputIDs    []string `json:"-" gorm:"-"`
}

func createMediaReplaySnapshot(tx *gorm.DB, mediaType, taskID string, snapshot *MediaReplaySnapshot) error {
	if len(snapshot.Payload) == 0 {
		return ErrImageConflict
	}
	return tx.Create(&MediaReplaySnapshot{TaskId: taskID, MediaType: mediaType, Payload: snapshot.Payload}).Error
}

func GetRetainedMediaReplaySnapshot(ctx context.Context, mediaType, taskID string, now int64) (MediaReplaySnapshot, error) {
	var snapshot MediaReplaySnapshot
	err := DB.WithContext(ctx).Where("task_id = ? AND media_type = ? AND failed_at > 0 AND retain_until > ?", taskID, mediaType, now).Take(&snapshot).Error
	return snapshot, err
}

func FindRetainedMediaReplayExpiry(ctx context.Context, mediaType, taskID string, now int64) (int64, error) {
	var snapshot MediaReplaySnapshot
	err := DB.WithContext(ctx).Select("retain_until").Where("task_id = ? AND media_type = ? AND failed_at > 0 AND retain_until > ?", taskID, mediaType, now).Take(&snapshot).Error
	return snapshot.RetainUntil, err
}

func updateMediaReplaySnapshot(tx *gorm.DB, mediaType, taskID, status string, now int64) error {
	switch status {
	case ImageTaskSucceeded:
		if err := tx.Where("task_id = ? AND media_type = ?", taskID, mediaType).Delete(&MediaReplaySnapshot{}).Error; err != nil {
			return err
		}
		if mediaType == MediaReplayVideo {
			return tx.Where("task_id = ?", taskID).Delete(&ImageInputTaskReference{}).Error
		}
		return nil
	case ImageTaskFailed, ImageTaskExpired, ImageTaskExecutionUnknown, ImageTaskStorageFailed, ImageTaskBillingFailed:
		return tx.Model(&MediaReplaySnapshot{}).Where("task_id = ? AND media_type = ? AND failed_at = 0", taskID, mediaType).
			Updates(map[string]any{"failed_at": now, "retain_until": now + MediaReplayRetentionSeconds}).Error
	default:
		return nil
	}
}

// PruneExpiredMediaReplaySnapshots releases only the bounded batch of expired
// snapshots selected for this sweep. Active tasks never have a deadline here.
func PruneExpiredMediaReplaySnapshots(ctx context.Context, now int64, limit int) error {
	if limit <= 0 {
		return nil
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var snapshots []MediaReplaySnapshot
		if err := lockForUpdate(tx).Select("id", "task_id", "media_type").Where("retain_until > 0 AND retain_until <= ?", now).
			Order("retain_until, id").Limit(limit).Find(&snapshots).Error; err != nil || len(snapshots) == 0 {
			return err
		}
		ids := make([]int64, 0, len(snapshots))
		var videoTaskIDs []string
		for _, snapshot := range snapshots {
			ids = append(ids, snapshot.Id)
			if snapshot.MediaType == MediaReplayVideo {
				videoTaskIDs = append(videoTaskIDs, snapshot.TaskId)
			}
		}
		if len(videoTaskIDs) > 0 {
			if err := tx.Where("task_id IN ?", videoTaskIDs).Delete(&ImageInputTaskReference{}).Error; err != nil {
				return err
			}
		}
		return tx.Where("id IN ?", ids).Delete(&MediaReplaySnapshot{}).Error
	})
}
