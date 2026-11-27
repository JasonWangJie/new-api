package model

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	MediaRetestInputTaskPrefix       = "retest_"
	MediaRetestInputRetentionSeconds = AsyncMediaMaxTaskAgeSeconds + MediaReplayRetentionSeconds + 10*60
)

// SaveMediaRetestInput publishes a temporary copy for an operator's token. The
// copy exists before normal task admission, so its replay snapshot is the guard.
func SaveMediaRetestInput(ctx context.Context, object MediaInputObject, source MediaReplaySnapshot, byteLimit int64) (MediaInputObject, error) {
	now := time.Now().Unix()
	if !strings.HasPrefix(object.TaskId, MediaRetestInputTaskPrefix) || object.ByteSize <= 0 || object.ExpiresAt <= now || object.ExpiresAt > object.CreatedAt+MediaRetestInputRetentionSeconds || object.UserId <= 0 || object.TokenId <= 0 {
		return MediaInputObject{}, ErrImageConflict
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var target Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND status = ? AND (expired_time = -1 OR expired_time > ?)", object.TokenId, object.UserId, common.TokenStatusEnabled, now).Take(&target).Error; err != nil {
			return err
		}
		if IsCanvasTokenKey(target.Key) {
			return ErrImageConflict
		}
		var snapshot MediaReplaySnapshot
		if err := lockForUpdate(tx).Where("media_type = ? AND task_id = ? AND failed_at > 0 AND retain_until > ?", source.MediaType, source.TaskId, now).Take(&snapshot).Error; err != nil {
			return err
		}
		var existing MediaInputObject
		err := tx.Where("object_id = ?", object.ObjectId).Take(&existing).Error
		if err == nil {
			if existing.TaskId != object.TaskId || existing.TokenId != object.TokenId || existing.UserId != object.UserId || existing.Kind != object.Kind || existing.SourceHash != object.SourceHash || existing.MimeType != object.MimeType || existing.Checksum != object.Checksum || existing.ByteSize != object.ByteSize || existing.Status != "active" || existing.ExpiresAt <= now {
				return ErrImageConflict
			}
			object = existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var heldBytes int64
		if err := tx.Model(&MediaInputObject{}).Where("token_id = ? AND expires_at > ? AND status = ?", object.TokenId, now, "active").Select("COALESCE(SUM(byte_size), 0)").Scan(&heldBytes).Error; err != nil {
			return err
		}
		if heldBytes < 0 || heldBytes > byteLimit || object.ByteSize > byteLimit-heldBytes {
			return ErrImageUploadByteQuota
		}
		return tx.Create(&object).Error
	})
	return object, err
}
