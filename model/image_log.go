package model

import (
	"context"
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

func MigrateImageLogs(db *gorm.DB) error {
	if !common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return db.AutoMigrate(&AsyncImageLogReceipt{})
	}
	query := strings.Replace(clickHouseLogCreateTableSQL(clickHouseLogTTLDays()), "CREATE TABLE IF NOT EXISTS logs (", "CREATE TABLE IF NOT EXISTS async_image_consume_logs (\n event_id String, fingerprint String,", 1)
	query = strings.Replace(query, "ENGINE = MergeTree()", "ENGINE = ReplacingMergeTree()", 1)
	query = strings.Replace(query, "ORDER BY (created_at, request_id)", "ORDER BY (created_at, event_id)", 1)
	return db.Exec(query).Error
}

func imageConsumeLogsQuery(db *gorm.DB) *gorm.DB {
	if !common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return db.Model(&Log{})
	}
	columns := "id,user_id,created_at,type,content,username,token_name,model_name,quota,prompt_tokens,completion_tokens,use_time,is_stream,channel_id,token_id,`group`,ip,request_id,upstream_request_id,other"
	return db.Table("(SELECT " + columns + " FROM logs UNION ALL SELECT " + columns + " FROM async_image_consume_logs FINAL) AS logs")
}

func imageUsageQuery(db *gorm.DB) *gorm.DB {
	columns := "id,user_id,username,model_name,created_at,use_group,token_id,channel_id,node_name,token_used,count,quota"
	return db.Table("(SELECT " + columns + " FROM quota_data UNION ALL SELECT " + columns + " FROM async_image_usage_projections) AS quota_data")
}

// ConfirmAsyncImageLog uses a durable main-DB command and a log-store receipt.
// Ambiguous writes may be retried; financial effects are never repeated here.
func ConfirmAsyncImageLog(ctx context.Context, taskId string) error {
	var bill AsyncImageBill
	if err := DB.WithContext(ctx).Where("task_id = ? AND status = ?", taskId, "applied").First(&bill).Error; err != nil {
		return err
	}
	if bill.LogStatus == "done" || bill.LogStatus == "disabled" {
		return nil
	}
	if !common.LogConsumeEnabled {
		return DB.WithContext(ctx).Model(&AsyncImageBill{}).Where("id = ? AND status = ?", bill.Id, "applied").Update("log_status", "disabled").Error
	}
	var log Log
	if err := common.UnmarshalJsonStr(bill.LogPayload, &log); err != nil {
		return err
	}
	if log.RequestId != bill.BillingRequestId || log.UserId != bill.UserId || log.TokenId != bill.TokenId || log.Quota != bill.Quota {
		return ErrImageConflict
	}
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		row := struct {
			Log
			EventId     string `gorm:"column:event_id"`
			Fingerprint string
		}{Log: log, EventId: bill.BillingRequestId, Fingerprint: bill.Fingerprint}
		if err := LOG_DB.WithContext(ctx).Table("async_image_consume_logs").Create(&row).Error; err != nil {
			return err
		}
		var actual struct{ Fingerprint string }
		if err := LOG_DB.WithContext(ctx).Table("async_image_consume_logs FINAL").Select("fingerprint").Where("created_at = ? AND event_id = ?", log.CreatedAt, bill.BillingRequestId).Take(&actual).Error; err != nil {
			return err
		}
		if actual.Fingerprint != bill.Fingerprint {
			return ErrImageConflict
		}
	} else {
		if err := LOG_DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			receipt := AsyncImageLogReceipt{EventId: bill.BillingRequestId, Fingerprint: bill.Fingerprint}
			if err := tx.Create(&receipt).Error; err != nil {
				return err
			}
			return tx.Create(&log).Error
		}); err != nil {
			// A receipt exists only if its log committed in the same transaction.
			// Do not infer duplicate insertion from dialect-specific RowsAffected.
			var existing AsyncImageLogReceipt
			if lookupErr := LOG_DB.WithContext(ctx).Where("event_id = ?", bill.BillingRequestId).Take(&existing).Error; lookupErr != nil {
				return err
			}
			if existing.Fingerprint != bill.Fingerprint {
				return ErrImageConflict
			}
		}
	}
	return DB.WithContext(ctx).Model(&AsyncImageBill{}).Where("id = ? AND status = ? AND fingerprint = ?", bill.Id, "applied", bill.Fingerprint).Update("log_status", "done").Error
}

func RefreshAsyncImageBillingCache(ctx context.Context, taskId string) error {
	var bill AsyncImageBill
	if err := DB.WithContext(ctx).Where("task_id = ? AND status = ?", taskId, "applied").First(&bill).Error; err != nil {
		return err
	}
	if err := invalidateUserCache(bill.UserId); err != nil {
		return err
	}
	var token Token
	if err := DB.WithContext(ctx).Unscoped().Where("id = ?", bill.TokenId).First(&token).Error; err != nil {
		return err
	}
	if token.Key == "" {
		return errors.New("image settlement token cache identity missing")
	}
	return invalidateTokenCacheForMutation(token.Key)
}
