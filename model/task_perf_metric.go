package model

import (
	"math"
	"time"

	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TaskPerfSample is the current delivery outcome of one accepted customer task.
// It is written in the task's state transaction, so retries and reconciliation
// replace one observation instead of incrementing an in-memory counter again.
// It contains no user, credential, provider payload, or result URL information.
type TaskPerfSample struct {
	ID           int64  `gorm:"primaryKey"`
	TaskId       string `gorm:"size:64;uniqueIndex:idx_task_perf_identity,priority:2"`
	Kind         string `gorm:"size:8;uniqueIndex:idx_task_perf_identity,priority:1"`
	ModelName    string `gorm:"size:255"`
	Group        string `gorm:"column:group;size:255"`
	Outcome      string `gorm:"size:8"`
	CompletedAt  int64  `gorm:"index:idx_task_perf_completed"`
	BucketTs     int64
	LatencyMs    int64
	LatencyCount int64
	OutputCount  int64
}

type TaskPerfMetricBucket struct {
	ModelName      string
	Group          string
	Kind           string
	BucketTs       int64
	RequestCount   int64
	SuccessCount   int64
	FailureCount   int64
	UnknownCount   int64
	LatencyCount   int64
	TotalLatencyMs int64
	OutputCount    int64
}

// GetTaskPerfMetricBuckets aggregates in the database; public requests never
// load individual task observations. CompletedAt bounds the window exactly,
// while BucketTs keeps hourly series independent of SQL-dialect date functions.
func GetTaskPerfMetricBuckets(modelName, group string, startTs, endTs int64, groups []string) ([]TaskPerfMetricBucket, error) {
	rows := make([]TaskPerfMetricBucket, 0)
	if groups != nil && len(groups) == 0 {
		return rows, nil
	}
	modelCol, groupCol := perfIdentityColumns()
	query := DB.Model(&TaskPerfSample{}).
		Select(modelCol+" AS model_name, "+groupCol+" AS "+commonGroupCol+", kind, bucket_ts, COUNT(*) AS request_count, "+
			"SUM(CASE WHEN outcome = 'success' THEN 1 ELSE 0 END) AS success_count, "+
			"SUM(CASE WHEN outcome = 'failure' THEN 1 ELSE 0 END) AS failure_count, "+
			"SUM(CASE WHEN outcome = 'unknown' THEN 1 ELSE 0 END) AS unknown_count, "+
			"SUM(latency_count) AS latency_count, SUM(latency_ms) AS total_latency_ms, SUM(output_count) AS output_count").
		Where("completed_at >= ? AND completed_at <= ?", startTs, endTs)
	if modelName != "" {
		query = query.Where(modelCol+" = ?", modelName)
	}
	if group != "" {
		query = query.Where(groupCol+" = ?", group)
	}
	if groups != nil {
		query = query.Where(groupCol+" IN ?", groups)
	}
	err := query.Group(modelCol + ", " + groupCol + ", kind, bucket_ts").Order("bucket_ts ASC").Scan(&rows).Error
	return rows, err
}

func recordImageTaskPerfSampleTx(tx *gorm.DB, taskID, beforeStatus string) error {
	var task AsyncImageTask
	if err := tx.Session(&gorm.Session{NewDB: true}).Where("task_id = ?", taskID).Take(&task).Error; err != nil {
		return err
	}
	if !task.Terminal() {
		if beforeStatus == ImageTaskSucceeded || beforeStatus == ImageTaskFailed || beforeStatus == ImageTaskExpired || beforeStatus == ImageTaskExecutionUnknown {
			return tx.Session(&gorm.Session{NewDB: true}).Where("kind = ? AND task_id = ? AND outcome <> ?", "image", taskID, "success").Delete(&TaskPerfSample{}).Error
		}
		return nil
	}
	if !perf_metrics_setting.GetSetting().Enabled || task.Model == "" {
		return nil
	}
	outcome := "failure"
	if task.Status == ImageTaskSucceeded {
		outcome = "success"
	} else if task.Status == ImageTaskExecutionUnknown || task.ReconciliationStatus == "upstream_unconfirmed" || task.ReconciliationStatus == ImagePolicyUnconfirmed || task.ReconciliationStatus == "quota_unconfirmed" {
		outcome = "unknown"
	}
	completed := task.FinishedAt
	if completed <= 0 {
		completed = time.Now().Unix()
	}
	sample := TaskPerfSample{TaskId: task.TaskId, Kind: "image", ModelName: task.Model, Group: task.Group, Outcome: outcome, CompletedAt: completed}
	if outcome == "success" {
		sample.OutputCount = int64(max(0, task.ResultCount))
	}
	wasTerminal := beforeStatus == ImageTaskSucceeded || beforeStatus == ImageTaskFailed || beforeStatus == ImageTaskExpired || beforeStatus == ImageTaskExecutionUnknown
	return saveTaskPerfSampleTx(tx, sample, task.CreatedAt, wasTerminal)
}

func recordVideoTaskPerfSampleTx(tx *gorm.DB, before, after AsyncMediaJob) error {
	terminal := after.Status == "succeeded" || after.Status == "failed" || after.Status == "expired" || after.Status == "execution_unknown"
	if !terminal {
		if before.Status == "succeeded" || before.Status == "failed" || before.Status == "expired" || before.Status == "execution_unknown" {
			return tx.Session(&gorm.Session{NewDB: true}).Where("kind = ? AND task_id = ? AND outcome <> ?", "video", after.TaskId, "success").Delete(&TaskPerfSample{}).Error
		}
		return nil
	}
	if !perf_metrics_setting.GetSetting().Enabled || after.Model == "" {
		return nil
	}
	// Retention cleanup of an already delivered result does not turn its original
	// delivery into a failure or move the observation into today's window.
	wasTerminal := before.Status == "succeeded" || before.Status == "failed" || before.Status == "expired" || before.Status == "execution_unknown"
	if wasTerminal && after.Status == "expired" {
		return nil
	}
	outcome := "failure"
	if after.Status == "succeeded" {
		outcome = "success"
	} else if after.Status == "execution_unknown" || after.BillingStatus == "unknown" || after.ReferenceRecoveryStage == "age_limit_unknown" || after.ReferenceRecoveryStage == "policy_stopped_unknown" {
		outcome = "unknown"
	}
	sample := TaskPerfSample{TaskId: after.TaskId, Kind: "video", ModelName: after.Model, Group: after.Group, Outcome: outcome, CompletedAt: time.Now().Unix()}
	if outcome == "success" {
		sample.OutputCount = 1
	}
	return saveTaskPerfSampleTx(tx, sample, after.CreatedAt, wasTerminal)
}

func saveTaskPerfSampleTx(tx *gorm.DB, sample TaskPerfSample, createdAt int64, preserveCompletion bool) error {
	tx = tx.Session(&gorm.Session{NewDB: true})
	var existing TaskPerfSample
	if err := tx.Where("kind = ? AND task_id = ?", sample.Kind, sample.TaskId).Limit(1).Find(&existing).Error; err != nil {
		return err
	}
	// A delivered upstream fallback can later be resumed to improve local
	// storage. That maintenance cannot erase or postpone its first delivery.
	if existing.Outcome == "success" {
		return nil
	}
	if preserveCompletion {
		if existing.ID == 0 {
			// Metadata edits and retention cleanup must not backfill old tasks
			// or invent a fresh delivery timestamp after collection was disabled.
			return nil
		}
		if existing.Outcome == sample.Outcome {
			sample.CompletedAt = existing.CompletedAt
		} else {
			// A reconciled outcome replaces the task's previous observation.
			// Successful delivery is measured at this confirmation, not at an
			// earlier timeout/unknown outcome.
			sample.CompletedAt = time.Now().Unix()
		}
	}
	if sample.Group == "" {
		sample.Group = "default"
	}
	sample.BucketTs = sample.CompletedAt - sample.CompletedAt%3600
	if sample.Outcome == "success" && createdAt > 0 && sample.CompletedAt >= createdAt && sample.CompletedAt-createdAt <= math.MaxInt64/1000 {
		sample.LatencyMs = (sample.CompletedAt - createdAt) * 1000
		sample.LatencyCount = 1
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "kind"}, {Name: "task_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"model_name", "group", "outcome", "completed_at", "bucket_ts", "latency_ms", "latency_count", "output_count"}),
	}).Create(&sample).Error
}
