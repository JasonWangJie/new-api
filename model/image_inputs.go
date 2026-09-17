package model

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrImageUploadRateLimited = errors.New("async_image_upload_rate_limited")
	ErrImageUploadByteQuota   = errors.New("async_image_upload_byte_quota")
	ErrImageUploadInProgress  = errors.New("async_image_upload_in_progress")
	ErrImageUploadUnavailable = errors.New("async_image_upload_result_unavailable")
	ErrImageUploadAliasLimit  = errors.New("async_image_upload_alias_limit")
)

func RecordImageUploadAttempt(ctx context.Context, tokenId, limit int) (string, error) {
	ticket := ImageUploadAdmissionTicket{TicketId: common.GetUUID(), TokenId: tokenId, CreatedAt: time.Now().Unix()}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx).Where("id = ?", tokenId).Take(&token).Error; err != nil {
			return err
		}
		var admission ImageInputAdmission
		err := tx.Where("token_id = ?", tokenId).Take(&admission).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			admission = ImageInputAdmission{TokenId: tokenId, AttemptTimes: "[]"}
			if err := tx.Create(&admission).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		var timestamps []int64
		if err := common.UnmarshalJsonStr(admission.AttemptTimes, &timestamps); err != nil {
			return err
		}
		retained := make([]int64, 0, len(timestamps)+1)
		for _, timestamp := range timestamps {
			if timestamp > ticket.CreatedAt-60 {
				retained = append(retained, timestamp)
			}
		}
		if len(retained) >= limit {
			return ErrImageUploadRateLimited
		}
		retained = append(retained, ticket.CreatedAt)
		encoded, err := common.Marshal(retained)
		if err != nil {
			return err
		}
		if err := tx.Model(&admission).Updates(map[string]any{"attempt_times": string(encoded), "version": admission.Version + 1}).Error; err != nil {
			return err
		}
		return tx.Create(&ticket).Error
	})
	return ticket.TicketId, err
}

func ReserveImageInput(ctx context.Context, ticketId string, input ImageInputObject, quota int64) (ImageInputObject, bool, error) {
	reused := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx).Where("id = ?", input.TokenId).Take(&token).Error; err != nil {
			return err
		}
		now := time.Now().Unix()
		var ticket ImageUploadAdmissionTicket
		if err := tx.Where("ticket_id = ? AND token_id = ? AND consumed_at = 0 AND created_at >= ?", ticketId, input.TokenId, now-600).Take(&ticket).Error; err != nil {
			return err
		}
		if err := tx.Model(&ticket).Update("consumed_at", now).Error; err != nil {
			return err
		}
		var existing ImageInputObject
		err := tx.Where("token_id = ? AND key_hash = ?", input.TokenId, input.KeyHash).Take(&existing).Error
		if err == nil {
			if existing.Fingerprint != input.Fingerprint {
				return ErrImageConflict
			}
			if existing.Status == "reserved" || existing.Status == "uploading" {
				return ErrImageUploadInProgress
			}
			if existing.Status != "active" || existing.ExpiresAt <= now {
				return ErrImageUploadUnavailable
			}
			input = existing
			reused = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var held int64
		if err := tx.Model(&ImageInputObject{}).Select("COALESCE(SUM(byte_size),0)").Where("token_id = ? AND status IN ?", input.TokenId, []string{"reserved", "uploading", "active", "failed", "deleting"}).Scan(&held).Error; err != nil {
			return err
		}
		if held < 0 || input.ByteSize <= 0 || held > quota || input.ByteSize > quota-held {
			return ErrImageUploadByteQuota
		}
		return tx.Create(&input).Error
	})
	return input, reused, err
}

func ConfirmImageInput(ctx context.Context, input ImageInputObject, object ImageStorageObject, urlHash string) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current ImageInputObject
		if err := lockForUpdate(tx).Where("input_id = ? AND token_id = ?", input.InputId, input.TokenId).Take(&current).Error; err != nil {
			return err
		}
		if current.Fingerprint != input.Fingerprint || current.Status != "reserved" && current.Status != "active" || current.Status == "reserved" && current.LeaseExpiresAt <= time.Now().Unix() {
			return ErrImageConflict
		}
		if current.Status == "active" && current.ObjectId != object.ObjectId {
			return ErrImageConflict
		}
		var stored ImageStorageObject
		if err := lockForUpdate(tx).Where("object_id = ? AND status = ?", object.ObjectId, "active").Take(&stored).Error; err != nil {
			return err
		}
		if stored.Checksum != object.Checksum || stored.ByteSize != current.ByteSize {
			return ErrImageConflict
		}
		var alias ImageInputAlias
		err := tx.Where("url_hash = ?", urlHash).Take(&alias).Error
		if err == nil {
			if alias.InputId != input.InputId || alias.TokenId != input.TokenId {
				return ErrImageConflict
			}
		} else {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			var count int64
			if err := tx.Model(&ImageInputAlias{}).Where("input_id = ?", input.InputId).Count(&count).Error; err != nil {
				return err
			}
			if count >= 128 {
				return ErrImageUploadAliasLimit
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&ImageInputAlias{UrlHash: urlHash, InputId: input.InputId, TokenId: input.TokenId}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&current).Updates(map[string]any{"status": "active", "object_id": object.ObjectId, "lease_token": "", "lease_expires_at": 0}).Error
	})
}

// ClaimImageInputDeletion shares the input row lock with task admission, so
// no new task can bind an input after cleanup starts.
func ClaimImageInputDeletion(ctx context.Context, input ImageInputObject, lease string, seconds int) (ImageUploadIntent, error) {
	var intent ImageUploadIntent
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().Unix()
		var current ImageInputObject
		if err := lockForUpdate(tx).Where("id = ? AND status IN ? AND lease_expires_at <= ?", input.Id, []string{"reserved", "failed", "active", "deleting"}, now).Take(&current).Error; err != nil {
			return err
		}
		if current.Status == "active" && current.ExpiresAt > now {
			return ErrImageConflict
		}
		var references int64
		if err := tx.Model(&ImageInputTaskReference{}).Joins("JOIN async_image_tasks ON async_image_tasks.task_id = image_input_task_references.task_id").Where("image_input_task_references.input_id = ? AND async_image_tasks.status NOT IN ?", current.InputId, []string{ImageTaskSucceeded, ImageTaskFailed, ImageTaskExpired, ImageTaskExecutionUnknown}).Count(&references).Error; err != nil {
			return err
		}
		if references > 0 {
			return ErrImageConflict
		}
		if err := lockForUpdate(tx).Where("intent_key = ? AND lease_expires_at <= ?", current.IntentKey, now).Take(&intent).Error; err != nil {
			return err
		}
		if intent.FirstDeleteAt > 0 && now-intent.FirstDeleteAt < 600 {
			return ErrImageConflict
		}
		if err := tx.Model(&current).Update("status", "deleting").Error; err != nil {
			return err
		}
		return tx.Model(&intent).Updates(map[string]any{"status": "deleting", "lease_token": lease, "lease_expires_at": now + int64(seconds)}).Error
	})
	return intent, err
}
