package model

import (
	"context"
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const imageStorageRevisionKey = "image_storage_revision"

var ErrActiveImageStorageChannel = errors.New("Active storage channels cannot be deleted. Enable another channel for the same purpose first.")

// RemoveImageStorageChannel hides a disabled channel from configuration while
// preserving its location and credentials for existing objects and upload intents.
func RemoveImageStorageChannel(ctx context.Context, profileID string, expected int64) (int64, error) {
	return WithImageStorageRevision(ctx, &expected, func(tx *gorm.DB) error {
		var profile ImageStorageProfile
		if err := tx.Where("profile_id = ?", profileID).Take(&profile).Error; err != nil {
			return err
		}
		if !profile.CurrentRevision {
			return ErrOptionRevisionConflict
		}
		if profile.Active {
			return ErrActiveImageStorageChannel
		}
		result := tx.Model(&ImageStorageProfile{}).Where("id = ? AND current_revision = ? AND active = ?", profile.Id, true, false).Update("current_revision", false)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrOptionRevisionConflict
		}
		return nil
	})
}

// ImagePublicationLeaseSeconds protects the handoff from durable bytes to their
// task, input or library reference, including retries of old upload intents.
const ImagePublicationLeaseSeconds = 720

func ReserveConfirmedImagePublication(ctx context.Context, intent ImageUploadIntent, identityHash string, now int64) (ImageStorageObject, error) {
	var object ImageStorageObject
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Acquire SQLite's write lock before reading. Other engines also lock
		// this row, consistently with ClaimImageObjectDeletion below.
		if err := tx.Model(&ImageStorageObject{}).Where("identity_hash = ? AND status = ?", identityHash, "active").UpdateColumn("status", gorm.Expr("status")).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where("identity_hash = ? AND status = ?", identityHash, "active").Take(&object).Error; err != nil {
			return err
		}
		if object.ProfileId != intent.ProfileId || object.ObjectKey != intent.ObjectKey || object.Checksum != intent.Checksum || object.ByteSize != intent.ByteSize {
			return ErrImageConflict
		}
		var current ImageUploadIntent
		if err := lockForUpdate(tx).Where("id = ? AND status = ?", intent.Id, "confirmed").Take(&current).Error; err != nil {
			return err
		}
		if current.ProfileId != object.ProfileId || current.ObjectKey != object.ObjectKey || current.Checksum != object.Checksum || current.ByteSize != object.ByteSize {
			return ErrImageConflict
		}
		return tx.Model(&current).Update("lease_expires_at", max(current.LeaseExpiresAt, now+ImagePublicationLeaseSeconds)).Error
	})
	return object, err
}

// MigrateImageStorageChannels preserves every historical object location and ID.
func MigrateImageStorageChannels(db *gorm.DB) error {
	for {
		var profiles []ImageStorageProfile
		if err := db.Where("channel_id IS NULL OR channel_id = ?", "").Order("id").Limit(100).Find(&profiles).Error; err != nil {
			return err
		}
		if len(profiles) == 0 {
			break
		}
		for _, profile := range profiles {
			if err := db.Model(&ImageStorageProfile{}).Where("id = ? AND (channel_id IS NULL OR channel_id = ?)", profile.Id, "").Updates(map[string]any{"channel_id": profile.ProfileId, "name": strings.TrimSpace(profile.Provider + " " + profile.Class), "current_revision": true}).Error; err != nil {
				return err
			}
		}
	}
	// Share the writer guard so migration cannot undo a concurrent activation.
	return db.Transaction(func(tx *gorm.DB) error {
		guard := Option{Key: imageStorageRevisionKey, Value: `{"revision":0}`}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"key"})}).Create(&guard).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where(map[string]any{"key": imageStorageRevisionKey}).Take(&guard).Error; err != nil {
			return err
		}
		// Older writers could race; preserve the latest row selected by the old reader.
		for _, class := range []string{"temporary", "durable"} {
			var active ImageStorageProfile
			err := tx.Where("class = ? AND active = ?", class, true).Order("id DESC").Take(&active).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if err := tx.Model(&ImageStorageProfile{}).Where("class = ? AND active = ? AND id <> ?", class, true, active.Id).Update("active", false).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func ImageStorageChannelProfiles(ctx context.Context, channelID string) ([]ImageStorageProfile, error) {
	var profiles []ImageStorageProfile
	err := DB.WithContext(ctx).Where("channel_id = ?", channelID).Order("id").Find(&profiles).Error
	if err == nil && len(profiles) == 0 {
		err = gorm.ErrRecordNotFound
	}
	return profiles, err
}

func GetImageStorageRevision(ctx context.Context) (int64, error) {
	var option Option
	err := DB.WithContext(ctx).Where(map[string]any{"key": imageStorageRevisionKey}).Take(&option).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var value struct {
		Revision int64 `json:"revision"`
	}
	if err := common.UnmarshalJsonStr(option.Value, &value); err != nil {
		return 0, err
	}
	return value.Revision, nil
}

// GetImageStorageChannelSnapshot pairs channel rows with the revision actually
// observed, so a concurrent activation cannot give a stale form a fresh guard.
func GetImageStorageChannelSnapshot(ctx context.Context) ([]ImageStorageProfile, int64, error) {
	var profiles []ImageStorageProfile
	var value struct {
		Revision int64 `json:"revision"`
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var guard Option
		err := lockForUpdate(tx).Where(map[string]any{"key": imageStorageRevisionKey}).Take(&guard).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			if err := common.UnmarshalJsonStr(guard.Value, &value); err != nil {
				return err
			}
		}
		return tx.Order("id DESC").Find(&profiles).Error
	})
	return profiles, value.Revision, err
}

// WithImageStorageRevision serializes all profile writers, including legacy saves.
// A no-op upsert obtains SQLite's write lock before reading the guard revision.
func WithImageStorageRevision(ctx context.Context, expected *int64, write func(*gorm.DB) error) (int64, error) {
	if expected != nil && (*expected < 0 || *expected >= MaxOptionRevision) {
		return 0, ErrOptionRevisionInvalid
	}
	var next int64
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		guard := Option{Key: imageStorageRevisionKey, Value: `{"revision":0}`}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"key"})}).Create(&guard).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where(map[string]any{"key": imageStorageRevisionKey}).Take(&guard).Error; err != nil {
			return err
		}
		var value struct {
			Revision int64 `json:"revision"`
		}
		if err := common.UnmarshalJsonStr(guard.Value, &value); err != nil {
			return err
		}
		if value.Revision < 0 || value.Revision >= MaxOptionRevision {
			return ErrOptionRevisionInvalid
		}
		if expected != nil && value.Revision != *expected {
			return ErrOptionRevisionConflict
		}
		if err := write(tx); err != nil {
			return err
		}
		next = value.Revision + 1
		value.Revision = next
		encoded, err := common.Marshal(value)
		if err != nil {
			return err
		}
		return tx.Model(&Option{}).Where(map[string]any{"key": imageStorageRevisionKey}).Update("value", string(encoded)).Error
	})
	return next, err
}
