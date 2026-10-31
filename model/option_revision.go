package model

import (
	"context"
	"errors"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const MaxOptionRevision int64 = 1<<53 - 1

var ErrOptionRevisionConflict = errors.New("Configuration changed. Reload it before saving")
var ErrOptionRevisionInvalid = errors.New("Invalid configuration revision")

// SaveVersionedOption serializes writers even before the option exists. A nil
// expected revision is reserved for trusted internal writers; it still advances
// the stored revision instead of trusting a revision supplied in value.
func SaveVersionedOption(ctx context.Context, key, value string, expectedRevision *int64) (int64, error) {
	if expectedRevision != nil && (*expectedRevision < 0 || *expectedRevision >= MaxOptionRevision) {
		return 0, ErrOptionRevisionInvalid
	}
	var payload map[string]common.RawMessage
	if err := common.UnmarshalJsonStr(value, &payload); err != nil {
		return 0, err
	}
	if payload == nil {
		return 0, errors.New("Configuration must be a JSON object")
	}
	var next int64
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// A no-op primary-key upsert takes SQLite's write lock before reading;
		// MySQL and PostgreSQL additionally use the shared row-lock helper.
		guard := Option{Key: key, Value: "{}"}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"key"}),
		}).Create(&guard).Error; err != nil {
			return err
		}
		var current Option
		if err := lockForUpdate(tx).Where(map[string]any{"key": key}).Take(&current).Error; err != nil {
			return err
		}
		var metadata struct {
			Revision int64 `json:"revision"`
		}
		if err := common.UnmarshalJsonStr(current.Value, &metadata); err != nil {
			return err
		}
		if metadata.Revision < 0 || metadata.Revision > MaxOptionRevision {
			return ErrOptionRevisionInvalid
		}
		if expectedRevision != nil && metadata.Revision != *expectedRevision {
			return ErrOptionRevisionConflict
		}
		if metadata.Revision == MaxOptionRevision {
			return ErrOptionRevisionInvalid
		}
		next = metadata.Revision + 1
		encodedRevision, err := common.Marshal(next)
		if err != nil {
			return err
		}
		payload["revision"] = encodedRevision
		encoded, err := common.Marshal(payload)
		if err != nil {
			return err
		}
		return tx.Model(&Option{}).Where(map[string]any{"key": key}).Update("value", string(encoded)).Error
	})
	return next, err
}
