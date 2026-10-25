package model

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// MediaInputObject binds a validated local reference to one canonical input
// slot. Filesystem locations remain private; signed reads also verify ownership.
type MediaInputObject struct {
	ID         int    `json:"-"`
	ObjectId   string `json:"object_id" gorm:"size:64;uniqueIndex"`
	TaskId     string `json:"task_id" gorm:"size:64;index;uniqueIndex:idx_media_input_task_slot,priority:1"`
	Slot       string `json:"slot" gorm:"size:96;uniqueIndex:idx_media_input_task_slot,priority:2"`
	Kind       string `json:"kind" gorm:"size:16"`
	SourceHash string `json:"-" gorm:"size:64"`
	UserId     int    `json:"-" gorm:"index"`
	TokenId    int    `json:"-" gorm:"index"`
	RootPath   string `json:"-" gorm:"type:text"`
	ObjectKey  string `json:"-" gorm:"size:255"`
	MimeType   string `json:"mime_type" gorm:"size:128"`
	ByteSize   int64  `json:"byte_size"`
	Checksum   string `json:"checksum" gorm:"size:64"`
	Status     string `json:"-" gorm:"size:16;index"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at" gorm:"index"`
}

// SaveMediaInputObject publishes only fully validated, atomically written
// files. A repeated slot returns its original object without extending expiry.
func SaveMediaInputObject(ctx context.Context, object MediaInputObject) (MediaInputObject, error) {
	now := time.Now().Unix()
	if object.ObjectId == "" || len(object.ObjectId) > 64 || object.TaskId == "" || len(object.TaskId) > 64 || object.Slot == "" || len(object.Slot) > 96 || object.UserId <= 0 || object.TokenId <= 0 ||
		(object.Kind != "image" && object.Kind != "video" && object.Kind != "audio") || len(object.SourceHash) != 64 || len(object.Checksum) != 64 ||
		strings.TrimSpace(object.RootPath) == "" || object.ObjectKey == "" || len(object.ObjectKey) > 255 || object.MimeType == "" || len(object.MimeType) > 128 || object.ByteSize <= 0 || object.Status != "active" || object.ExpiresAt <= now {
		return MediaInputObject{}, ErrImageConflict
	}
	if object.CreatedAt == 0 {
		object.CreatedAt = now
	}
	if object.CreatedAt <= 0 || object.ExpiresAt <= object.CreatedAt {
		return MediaInputObject{}, ErrImageConflict
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job AsyncMediaJob
		if err := lockForUpdate(tx).Where("task_id = ? AND user_id = ? AND token_id = ?", object.TaskId, object.UserId, object.TokenId).Take(&job).Error; err != nil {
			return err
		}
		if job.ExpiresAt <= now || object.ExpiresAt > job.ExpiresAt || job.Status != "queued" && job.Status != "submitting" && job.Status != "submitted" {
			return ErrImageConflict
		}
		var existing MediaInputObject
		err := tx.Where("task_id = ? AND slot = ?", object.TaskId, object.Slot).Take(&existing).Error
		if err == nil {
			if existing.UserId != object.UserId || existing.TokenId != object.TokenId || existing.SourceHash != object.SourceHash || existing.Kind != object.Kind || existing.ByteSize != object.ByteSize || existing.Checksum != object.Checksum || existing.Status != "active" || existing.ExpiresAt <= now {
				return ErrImageConflict
			}
			object = existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(&object).Error
	})
	return object, err
}

func GetMediaInputObject(ctx context.Context, objectID string) (MediaInputObject, error) {
	var object MediaInputObject
	err := DB.WithContext(ctx).Where("object_id = ?", objectID).Take(&object).Error
	return object, err
}

func ListMediaInputObjects(ctx context.Context, taskID string, userID, tokenID int) ([]MediaInputObject, error) {
	var objects []MediaInputObject
	err := DB.WithContext(ctx).Where("task_id = ? AND user_id = ? AND token_id = ?", taskID, userID, tokenID).Order("id").Find(&objects).Error
	return objects, err
}

func GetExpiredMediaInputObjects(ctx context.Context, now int64, limit int) ([]MediaInputObject, error) {
	if limit <= 0 {
		return nil, nil
	}
	var objects []MediaInputObject
	err := DB.WithContext(ctx).Where("expires_at <= ?", now).Order("expires_at").Order("id").Limit(limit).Find(&objects).Error
	return objects, err
}

// DeleteMediaInputObject removes only the object whose file was just cleaned
// up, so a stale cleanup pass cannot delete another slot's replacement.
func DeleteMediaInputObject(ctx context.Context, object MediaInputObject) error {
	return DB.WithContext(ctx).Where("id = ? AND object_id = ? AND task_id = ? AND user_id = ? AND token_id = ? AND object_key = ? AND checksum = ?", object.ID, object.ObjectId, object.TaskId, object.UserId, object.TokenId, object.ObjectKey, object.Checksum).Delete(&MediaInputObject{}).Error
}
