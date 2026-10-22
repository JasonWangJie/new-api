package model

import (
	"errors"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Setup struct {
	ID            uint   `json:"id" gorm:"primaryKey"`
	Version       string `json:"version" gorm:"type:varchar(50);not null"`
	InitializedAt int64  `json:"initialized_at" gorm:"type:bigint;not null"`
}

func GetSetup() *Setup {
	var setup Setup
	err := DB.First(&setup).Error
	if err != nil {
		return nil
	}
	return &setup
}

// CompleteSetup commits the administrator, operation modes and setup marker
// together before publishing the new settings to this process.
func CompleteSetup(rootUser *User, selfUseMode, demoSite bool) error {
	options := []Option{
		{Key: "SelfUseModeEnabled", Value: strconv.FormatBool(selfUseMode)},
		{Key: "DemoSiteEnabled", Value: strconv.FormatBool(demoSite)},
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		var existing Setup
		if err := tx.First(&existing).Error; err == nil {
			return errors.New("system is already initialized")
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if rootUser != nil {
			if err := tx.Create(rootUser).Error; err != nil {
				return err
			}
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value"}),
		}).Create(&options).Error; err != nil {
			return err
		}
		// The fixed primary key also prevents simultaneous setup requests on
		// different nodes from committing more than one administrator.
		return tx.Create(&Setup{ID: 1, Version: common.Version, InitializedAt: time.Now().Unix()}).Error
	})
	if err != nil {
		return err
	}
	for _, option := range options {
		if err := updateOptionMap(option.Key, option.Value); err != nil {
			return err
		}
	}
	constant.Setup = true
	return nil
}
