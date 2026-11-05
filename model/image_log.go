package model

import (
	"context"
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// AsyncMediaRefundQuotas recovers one confirmed full refund per task from the
// historical log store. Callers pass only tasks already known to be refunded.
// Partial settlements, conflicting refunds and missing records remain unknown.
func AsyncMediaRefundQuotas(ctx context.Context, jobs []AsyncMediaJob) (map[string]int, error) {
	amounts := make(map[string]int)
	if len(jobs) == 0 || LOG_DB == nil {
		return amounts, nil
	}
	if len(jobs) > 100 {
		return nil, errors.New("refund lookup exceeds the media task page limit")
	}
	byID := make(map[string]AsyncMediaJob, len(jobs))
	userIDs := make([]int, 0, len(jobs))
	since := jobs[0].CreatedAt
	conditions := LOG_DB.Session(&gorm.Session{NewDB: true})
	for _, job := range jobs {
		byID[job.TaskId] = job
		userIDs = append(userIDs, job.UserId)
		since = min(since, job.CreatedAt)
		pattern := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(job.TaskId) + "%"
		predicate := "other LIKE ? ESCAPE '!'"
		if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
			pattern = "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(job.TaskId) + "%"
			predicate = "other LIKE ?"
		}
		conditions = conditions.Or(predicate, pattern)
	}
	// Bound result processing even for malformed or unusually verbose history.
	// Reaching the limit leaves the amount unknown rather than selecting a
	// possibly incomplete subset of conflicting records.
	const maxRefundLogs = 1000
	var logs []Log
	if err := imageConsumeLogsQuery(LOG_DB.WithContext(ctx)).
		Select("user_id", "token_id", "quota", "other", "content", "request_id", "created_at").
		Where("type = ? AND user_id IN ? AND created_at >= ?", LogTypeRefund, userIDs, since).
		Where(conditions).Limit(maxRefundLogs + 1).Find(&logs).Error; err != nil {
		return nil, err
	}
	if len(logs) > maxRefundLogs {
		return nil, errors.New("refund log history exceeds the lookup limit")
	}
	requests := make(map[string]string)
	ambiguous := make(map[string]bool)
	for _, log := range logs {
		var other struct {
			TaskID           string `json:"task_id"`
			Reason           string `json:"reason"`
			ActualQuota      *int   `json:"actual_quota"`
			PreConsumedQuota *int   `json:"pre_consumed_quota"`
		}
		if common.UnmarshalJsonStr(log.Other, &other) != nil || log.Quota <= 0 || log.RequestId == "" {
			continue
		}
		job, found := byID[other.TaskID]
		if !found || job.UserId != log.UserId || job.TokenId != log.TokenId || log.CreatedAt < job.CreatedAt {
			continue
		}
		if other.ActualQuota != nil {
			if *other.ActualQuota != 0 || other.PreConsumedQuota == nil || *other.PreConsumedQuota != log.Quota || other.Reason == "" {
				continue
			}
		} else if other.Reason != "Task submission failed; reserved quota returned" &&
			(other.Reason == "" || other.PreConsumedQuota != nil || log.Content != "") {
			// Legacy full-failure refunds have a reason and no settlement
			// quantities. Other unclassified records cannot prove a full refund.
			continue
		}
		if previous, exists := amounts[job.TaskId]; exists && (previous != log.Quota || requests[job.TaskId] != log.RequestId) {
			ambiguous[job.TaskId] = true
		}
		amounts[job.TaskId], requests[job.TaskId] = log.Quota, log.RequestId
	}
	for id := range ambiguous {
		delete(amounts, id)
	}
	return amounts, nil
}

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
	if err := recordTaskBillingLogOnce(ctx, &log, bill.BillingRequestId, bill.Fingerprint); err != nil {
		return err
	}
	return DB.WithContext(ctx).Model(&AsyncImageBill{}).Where("id = ? AND status = ? AND fingerprint = ?", bill.Id, "applied", bill.Fingerprint).Update("log_status", "done").Error
}

// recordTaskBillingLogOnce deduplicates durable billing events in the log store.
// It never performs financial writes or acknowledges the main-DB command.
func recordTaskBillingLogOnce(ctx context.Context, log *Log, eventID, fingerprint string) error {
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		row := struct {
			Log
			EventId     string `gorm:"column:event_id"`
			Fingerprint string
		}{Log: *log, EventId: eventID, Fingerprint: fingerprint}
		if err := LOG_DB.WithContext(ctx).Table("async_image_consume_logs").Create(&row).Error; err != nil {
			return err
		}
		var actual struct{ Fingerprint string }
		if err := LOG_DB.WithContext(ctx).Table("async_image_consume_logs FINAL").Select("fingerprint").Where("created_at = ? AND event_id = ?", log.CreatedAt, eventID).Take(&actual).Error; err != nil {
			return err
		}
		if actual.Fingerprint != fingerprint {
			return ErrImageConflict
		}
	} else {
		if err := LOG_DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			receipt := AsyncImageLogReceipt{EventId: eventID, Fingerprint: fingerprint}
			if err := tx.Create(&receipt).Error; err != nil {
				return err
			}
			return tx.Create(log).Error
		}); err != nil {
			// A receipt exists only if its log committed in the same transaction.
			// Do not infer duplicate insertion from dialect-specific RowsAffected.
			var existing AsyncImageLogReceipt
			if lookupErr := LOG_DB.WithContext(ctx).Where("event_id = ?", eventID).Take(&existing).Error; lookupErr != nil {
				return err
			}
			if existing.Fingerprint != fingerprint {
				return ErrImageConflict
			}
		}
	}
	return nil
}

func RefreshAsyncImageBillingCache(ctx context.Context, taskId string) error {
	var bill AsyncImageBill
	if err := DB.WithContext(ctx).Where("task_id = ? AND status = ?", taskId, "applied").First(&bill).Error; err != nil {
		return err
	}
	// ApplyAsyncImageBill projects only its own deltas. Invalidating a live
	// balance here would discard unflushed deductions from ordinary relays.
	return nil
}
