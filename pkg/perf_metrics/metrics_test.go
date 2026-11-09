package perfmetrics

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestModelPerformanceWeightedSummary(t *testing.T) {
	result := buildQueryResult("model-a", map[bucketKey]counters{
		{model: "model-a", group: "small", bucketTs: 3600}: {requestCount: 1, successCount: 1, latencyCount: 1, totalLatencyMs: 1000, outputCount: 2, workloads: imageWorkload},
		{model: "model-a", group: "large", bucketTs: 3600}: {requestCount: 99, unknownCount: 9, workloads: imageWorkload},
	})
	assert.Equal(t, int64(100), result.Summary.RequestCount)
	assert.Equal(t, float64(1), result.Summary.SuccessRate)
	assert.Equal(t, int64(90), result.Summary.FailureCount)
	assert.Equal(t, int64(9), result.Summary.UnknownCount)
	assert.Equal(t, int64(1000), result.Summary.AvgLatencyMs, "unsuccessful delivery must not dilute successful latency")
	assert.Equal(t, int64(2), result.Summary.OutputCount)
	assert.Equal(t, "image", result.Summary.Workload)
	assert.Len(t, result.Groups, 2)
}

// Every external run must target this feature's disposable local database.
func performanceDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dialect := os.Getenv("PERF_METRICS_TEST_DIALECT")
	dsn := os.Getenv("PERF_METRICS_TEST_DSN")
	previousDB, previousType, previousLogType := model.DB, common.MainDatabaseType(), common.LogDatabaseType()
	previousPath, previousMaster := common.SQLitePath, common.IsMasterNode
	switch dialect {
	case "", "sqlite":
		common.SQLitePath = filepath.Join(t.TempDir(), "performance.db") + "?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_txlock=immediate"
		dsn = ""
	case "mysql":
		require.Contains(t, dsn, "@tcp(127.0.0.1:")
		require.Contains(t, dsn, ")/new_api_image_test_perf_")
	case "postgres":
		parsed, err := url.Parse(dsn)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1", parsed.Hostname())
		require.True(t, strings.HasPrefix(parsed.Path, "/new_api_image_test_perf_"))
	default:
		t.Fatalf("unsupported test dialect %q", dialect)
	}
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("LOG_SQL_DSN", "")
	common.IsMasterNode = false
	require.NoError(t, model.InitDB())
	db := model.DB
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	hotBuckets.Clear()
	t.Cleanup(func() {
		hotBuckets.Clear()
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
		common.SetLogDatabaseType(previousLogType)
		common.SQLitePath, common.IsMasterNode = previousPath, previousMaster
		assert.NoError(t, connection.Close())
	})
	return db
}

func TestModelPerformanceQueryWindowAndGroupIsolation(t *testing.T) {
	db := performanceDatabase(t)
	require.NoError(t, db.Migrator().DropTable(&model.PerfMetric{}, &model.TaskPerfSample{}))
	require.NoError(t, db.AutoMigrate(&model.PerfMetric{}, &model.TaskPerfSample{}))
	now := time.Now().Unix()
	bucket := now - now%3600
	require.NoError(t, db.Create(&model.PerfMetric{ModelName: "chat", Group: "default", BucketTs: bucket, RequestCount: 100, SuccessCount: 90, TotalLatencyMs: 100000, OutputTokens: 5000, GenerationMs: 100000, TtftSumMs: 1000, TtftCount: 10}).Error)
	require.NoError(t, db.Create(&model.PerfMetric{ModelName: "chat", Group: "removed", BucketTs: bucket, RequestCount: 1000, SuccessCount: 1000, TotalLatencyMs: 1000000}).Error)
	Record(Sample{Model: "chat", Group: "default", Success: true, LatencyMs: 1000, OutputTokens: 50, GenerationMs: 1000})
	result, err := Query(QueryParams{Model: "chat", Hours: 24, Groups: []string{"default"}})
	require.NoError(t, err)
	assert.Equal(t, int64(101), result.Summary.RequestCount)
	assert.InDelta(t, 100.0*91/101, result.Summary.SuccessRate, 0.00001)
	assert.Equal(t, float64(50), result.Summary.AvgTps)
	assert.Equal(t, int64(100), result.Summary.AvgTtftMs)
	require.Len(t, result.Groups, 1)
	assert.Equal(t, "default", result.Groups[0].Group)
	summary, err := QuerySummaryAll(24, []string{"default"})
	require.NoError(t, err)
	require.Len(t, summary.Models, 1)
	assert.Equal(t, result.Summary.RequestCount, summary.Models[0].RequestCount)
	assert.Equal(t, result.Summary.AvgTtftMs, summary.Models[0].AvgTtftMs)
	assert.Equal(t, 24, summary.WindowHours)
	assert.InDelta(t, now, summary.UpdatedAt, 5)
	result, err = Query(QueryParams{Model: "chat", Groups: []string{}})
	require.NoError(t, err)
	assert.Empty(t, result.Groups)
	assert.Zero(t, result.Summary.RequestCount)
	for _, sample := range []model.TaskPerfSample{
		{TaskId: "upper-case", Kind: "image", ModelName: "CaseModel", Group: "default", Outcome: "success", CompletedAt: now, BucketTs: bucket, OutputCount: 2},
		{TaskId: "lower-case", Kind: "image", ModelName: "casemodel", Group: "default", Outcome: "failure", CompletedAt: now, BucketTs: bucket},
		{TaskId: "non-active-case-group", Kind: "image", ModelName: "CaseModel", Group: "Default", Outcome: "failure", CompletedAt: now, BucketTs: bucket},
	} {
		require.NoError(t, db.Create(&sample).Error)
	}
	result, err = Query(QueryParams{Model: "CaseModel", Groups: []string{"default"}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Summary.RequestCount)
	assert.Equal(t, float64(100), result.Summary.SuccessRate)
	summary, err = QuerySummaryAll(24, []string{"default"})
	require.NoError(t, err)
	require.Len(t, summary.Models, 3, "case-sensitive public model identities must remain separate")
	byModel := make(map[string]ModelSummary)
	for _, row := range summary.Models {
		byModel[row.ModelName] = row
	}
	assert.Equal(t, int64(1), byModel["CaseModel"].SuccessCount)
	assert.Equal(t, int64(1), byModel["casemodel"].FailureCount)
}

func TestModelPerformanceTaskDelivery(t *testing.T) {
	db := performanceDatabase(t)
	entities := []any{&model.TaskPerfSample{}, &model.PerfMetric{}, &model.AsyncImageTask{}, &model.AsyncImageBill{}, &model.AsyncImageStagingObject{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}}
	require.NoError(t, db.Migrator().DropTable(entities...))
	require.NoError(t, db.AutoMigrate(entities...))
	previousEnabled := perf_metrics_setting.GetSetting().Enabled
	t.Cleanup(func() {
		assert.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": strconv.FormatBool(previousEnabled)}))
	})
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": "true"}))
	now := time.Now().Unix()
	task := model.AsyncImageTask{TaskId: "image-delivery", Model: "public-image", Group: "default", CreatedAt: now - 120, Status: model.ImageTaskBillingPending, Version: 1, LeaseToken: "delivery", LeaseExpiresAt: now + 300, ImageCount: 2, ResultCount: 2}
	require.NoError(t, db.Create(&task).Error)
	require.NoError(t, db.Create(&model.AsyncImageBill{TaskId: task.TaskId, BillingRequestId: "image-delivery", Status: "applied", LogStatus: "done"}).Error)
	require.NoError(t, model.CompleteAsyncImageTask(t.Context(), task, false))
	require.ErrorIs(t, model.CompleteAsyncImageTask(t.Context(), task, false), model.ErrImageConflict)
	hotBuckets.Clear() // A fresh process must still see the same durable observation.
	result, err := Query(QueryParams{Model: "public-image", Hours: 24})
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Summary.RequestCount)
	assert.Equal(t, int64(2), result.Summary.OutputCount)
	assert.InDelta(t, 120000, result.Summary.AvgLatencyMs, 5000)
	assert.Equal(t, "image", result.Summary.Workload)
	assert.Equal(t, float64(100), result.Summary.SuccessRate)

	unknown := model.AsyncImageTask{TaskId: "image-unknown", Model: "public-image", Group: "default", CreatedAt: now - 60, Status: model.ImageTaskInvoking, Version: 1}
	require.NoError(t, db.Create(&unknown).Error)
	require.NoError(t, model.TransitionImageTask(t.Context(), unknown, map[string]any{"status": model.ImageTaskExecutionUnknown, "finished_at": now}, "execution_unknown", "", ""))
	result, err = Query(QueryParams{Model: "public-image", Hours: 24})
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.Summary.RequestCount)
	assert.Equal(t, int64(1), result.Summary.UnknownCount)
	assert.Equal(t, float64(50), result.Summary.SuccessRate)
	require.NoError(t, db.Where("task_id = ?", unknown.TaskId).Take(&unknown).Error)
	require.NoError(t, model.TransitionImageTask(t.Context(), unknown, map[string]any{"status": model.ImageTaskInvoking, "finished_at": 0}, "resumed", "", ""))
	result, err = Query(QueryParams{Model: "public-image", Hours: 24})
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Summary.RequestCount, "resumed work is pending, not another failed sample")
	require.NoError(t, db.Where("task_id = ?", unknown.TaskId).Take(&unknown).Error)
	require.NoError(t, model.TransitionImageTask(t.Context(), unknown, map[string]any{"status": model.ImageTaskFailed, "finished_at": now}, "failed", "", ""))
	result, err = Query(QueryParams{Model: "public-image", Hours: 24})
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.Summary.RequestCount)
	assert.Equal(t, int64(1), result.Summary.FailureCount)
	assert.Zero(t, result.Summary.UnknownCount)

	job := model.AsyncMediaJob{TaskId: "video-delivery", IdentityHash: "video-delivery", Model: "public-video", Group: "default", CreatedAt: now - 300, Status: "submitted", StorageStatus: "ready"}
	require.NoError(t, db.Create(&job).Error)
	// If the lifecycle event cannot commit, neither the outcome nor its sample may commit.
	require.NoError(t, db.Migrator().DropTable(&model.AsyncMediaEvent{}))
	err = model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID), map[string]any{"status": "succeeded", "storage_status": "succeeded"}, "")
	require.Error(t, err)
	var persisted model.AsyncMediaJob
	require.NoError(t, db.First(&persisted, job.ID).Error)
	assert.Equal(t, "submitted", persisted.Status)
	var count int64
	require.NoError(t, db.Model(&model.TaskPerfSample{}).Where("task_id = ?", job.TaskId).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, db.AutoMigrate(&model.AsyncMediaEvent{}))
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(4)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			results <- model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ? AND status = ?", job.ID, "submitted"), map[string]any{"status": "succeeded", "storage_status": "succeeded"}, "")
		})
	}
	workers.Wait()
	close(results)
	won := 0
	for err := range results {
		if err == nil {
			won++
		} else {
			assert.ErrorIs(t, err, model.ErrImageConflict)
		}
	}
	assert.Equal(t, 1, won)
	var sample model.TaskPerfSample
	require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&sample).Error)
	completed := sample.CompletedAt
	require.NoError(t, model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID), map[string]any{"billing_status": "settled"}, ""))
	require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&sample).Error)
	assert.Equal(t, completed, sample.CompletedAt, "metadata updates cannot move the sample's window")
	require.NoError(t, model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID), map[string]any{"status": "submitted", "storage_status": "pending"}, "resume_requested"))
	result, err = Query(QueryParams{Model: "public-video", Hours: 24})
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Summary.SuccessCount, "storage-only recovery cannot erase an already delivered fallback")
	require.NoError(t, model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID), map[string]any{"status": "failed"}, ""))
	require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&sample).Error)
	assert.Equal(t, completed, sample.CompletedAt)
	assert.Equal(t, "success", sample.Outcome)
	require.NoError(t, model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID), map[string]any{"status": "expired"}, ""))
	result, err = Query(QueryParams{Model: "public-video", Hours: 24})
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Summary.RequestCount)
	assert.Equal(t, int64(1), result.Summary.SuccessCount, "result retention expiry cannot rewrite successful delivery")
	assert.Equal(t, int64(1), result.Summary.OutputCount)
	assert.InDelta(t, 300000, result.Summary.AvgLatencyMs, 5000)
	assert.Equal(t, "video", result.Summary.Workload)
	legacy := model.AsyncMediaJob{TaskId: "legacy-terminal", IdentityHash: "legacy-terminal", Model: "old-video", Group: "default", CreatedAt: now - 86400, Status: "succeeded"}
	require.NoError(t, db.Create(&legacy).Error)
	require.NoError(t, model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ?", legacy.ID), map[string]any{"billing_status": "settled"}, ""))
	result, err = Query(QueryParams{Model: "old-video", Hours: 24})
	require.NoError(t, err)
	assert.Zero(t, result.Summary.RequestCount, "editing a legacy terminal task cannot invent today's delivery")
	unknownJob := model.AsyncMediaJob{TaskId: "video-unknown", IdentityHash: "video-unknown", Model: "unknown-video", Group: "default", CreatedAt: now - 4000, Status: "submitted"}
	require.NoError(t, db.Create(&unknownJob).Error)
	require.NoError(t, model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ?", unknownJob.ID), map[string]any{"status": "execution_unknown"}, ""))
	require.NoError(t, db.Model(&model.TaskPerfSample{}).Where("task_id = ?", unknownJob.TaskId).Update("completed_at", now-1800).Error)
	require.NoError(t, model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ?", unknownJob.ID), map[string]any{"status": "succeeded"}, ""))
	sample = model.TaskPerfSample{}
	require.NoError(t, db.Where("task_id = ?", unknownJob.TaskId).Take(&sample).Error)
	assert.InDelta(t, now, sample.CompletedAt, 5)
	assert.InDelta(t, 4000000, sample.LatencyMs, 5000, "delivery after unknown must use actual confirmation time")

	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": "false"}))
	disabled := model.AsyncMediaJob{TaskId: "disabled-terminal", IdentityHash: "disabled-terminal", Model: "disabled-video", Group: "default", CreatedAt: now - 300, Status: "submitted"}
	require.NoError(t, db.Create(&disabled).Error)
	require.NoError(t, model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ?", disabled.ID), map[string]any{"status": "succeeded"}, ""))
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": "true"}))
	require.NoError(t, model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ?", disabled.ID), map[string]any{"billing_status": "settled"}, ""))
	result, err = Query(QueryParams{Model: "disabled-video", Hours: 24})
	require.NoError(t, err)
	assert.Zero(t, result.Summary.RequestCount)

	// Window bounds use the actual terminal timestamp, not the start of its hour.
	require.NoError(t, db.Model(&model.TaskPerfSample{}).Where("task_id = ?", job.TaskId).Update("completed_at", now-25*3600).Error)
	result, err = Query(QueryParams{Model: "public-video", Hours: 24})
	require.NoError(t, err)
	assert.Empty(t, result.Groups)
	result, err = Query(QueryParams{Model: "public-video", Hours: 48})
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Summary.RequestCount)
	require.NoError(t, model.DeletePerfMetricsBefore(now-24*3600))
	result, err = Query(QueryParams{Model: "public-video", Hours: 48})
	require.NoError(t, err)
	assert.Zero(t, result.Summary.RequestCount, "configured retention also removes durable task observations")
	result, err = Query(QueryParams{Model: "public-image", Hours: 24})
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.Summary.RequestCount, "retention preserves samples inside the boundary")
}

func TestModelPerformanceVideoErrorExclusions(t *testing.T) {
	db := performanceDatabase(t)
	entities := []any{&model.TaskPerfSample{}, &model.PerfMetric{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.Option{}}
	require.NoError(t, db.Migrator().DropTable(entities...))
	require.NoError(t, db.AutoMigrate(entities...))
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	previousEnabled := perf_metrics_setting.GetSetting().Enabled
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": strconv.FormatBool(previousEnabled)}))
	})
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": "true"}))

	now := time.Now().Unix()
	bucket := now - now%3600
	for _, fixture := range []struct {
		id, model, group, status, reason string
	}{
		{"delivered", "mixed-video", "default", "succeeded", "Code_100%"},
		{"matched", "mixed-video", "default", "failed", "资源错误 Code_100% provider-only-detail"},
		{"different-case", "mixed-video", "vip", "failed", "code_100%"},
		{"wildcards-are-literal", "mixed-video", "vip", "failed", "CodeX100Z"},
		{"unknown", "mixed-video", "default", "execution_unknown", "Code_100%"},
		{"expired", "mixed-video", "default", "expired", "Code_100%"},
		{"pending", "mixed-video", "default", "submitted", "Code_100%"},
		{"all-excluded", "failed-video", "default", "failed", "Code_100%"},
		{"Other-Task", "old-video", "default", "failed", "Code_100%"},
	} {
		job := model.AsyncMediaJob{TaskId: fixture.id, IdentityHash: fixture.id, Model: fixture.model, Group: fixture.group, Status: "submitted", CreatedAt: now - 300}
		require.NoError(t, db.Create(&job).Error)
		require.NoError(t, model.ChangeAsyncMediaJob(db.Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID), map[string]any{"status": fixture.status, "error_message": fixture.reason}, ""))
	}
	// A missing terminal job (or one with a different task ID) never excludes a
	// historical observation. Identical image task IDs are a separate workload.
	require.NoError(t, db.Model(&model.TaskPerfSample{}).Where("task_id = ?", "Other-Task").Update("task_id", "other-task").Error)
	require.NoError(t, db.Create(&model.TaskPerfSample{TaskId: "matched", Kind: "image", ModelName: "mixed-video", Group: "default", Outcome: "failure", CompletedAt: now, BucketTs: bucket}).Error)
	require.NoError(t, db.Create(&model.PerfMetric{ModelName: "mixed-video", Group: "default", BucketTs: bucket, RequestCount: 1, SuccessCount: 1, TotalLatencyMs: 1000}).Error)
	params := QueryParams{Model: "mixed-video", Groups: []string{"default", "vip"}}
	raw, err := Query(params)
	require.NoError(t, err)
	require.Equal(t, int64(8), raw.Summary.RequestCount)
	assert.Nil(t, raw.Summary.VideoFilteredSuccessRate)

	key := perf_metrics_setting.VideoErrorExclusionsOptionKey
	rules := "  Code_100% \r\n资源错误\nCode_100%\n"
	require.NoError(t, model.UpdateOption(key, rules))
	require.Error(t, model.UpdateOption(key, strings.Repeat("x", 257)))
	require.Error(t, model.UpdateOption(key, "Code\x00error"))
	var saved model.Option
	require.NoError(t, db.Take(&saved).Error)
	assert.Equal(t, rules, saved.Value, "invalid rules must not replace the persisted setting")
	result, err := Query(params)
	require.NoError(t, err)
	assert.Equal(t, []string{"Code_100%", "资源错误"}, result.VideoErrorExclusions)
	filtered := result.Summary.VideoFilteredSuccessRate
	require.NotNil(t, filtered)
	assert.Equal(t, int64(2), filtered.ExcludedCount, "overlapping literal rules and the expired job each count once")
	assert.Equal(t, int64(4), filtered.SampleCount, "unknown remains in the denominator, image and relay are outside this metric")
	require.NotNil(t, filtered.Rate)
	assert.Equal(t, float64(25), *filtered.Rate)
	result.Summary.VideoFilteredSuccessRate = nil
	assert.Equal(t, raw.Summary, result.Summary, "raw metrics and average latency retain their original scope")

	params.Group = "default"
	defaultGroup, err := Query(params)
	require.NoError(t, err)
	require.Len(t, defaultGroup.Groups, 1)
	require.NotNil(t, defaultGroup.Summary.VideoFilteredSuccessRate)
	require.NotNil(t, defaultGroup.Summary.VideoFilteredSuccessRate.Rate)
	assert.Equal(t, float64(50), *defaultGroup.Summary.VideoFilteredSuccessRate.Rate)
	assert.Equal(t, defaultGroup.Summary.VideoFilteredSuccessRate, defaultGroup.Groups[0].VideoFilteredSuccessRate)

	summary, err := QuerySummaryAll(24, params.Groups)
	require.NoError(t, err)
	assert.Equal(t, result.VideoErrorExclusions, summary.VideoErrorExclusions)
	models := make(map[string]ModelSummary, len(summary.Models))
	for _, entry := range summary.Models {
		models[entry.ModelName] = entry
	}
	assert.Equal(t, filtered, models["mixed-video"].VideoFilteredSuccessRate)
	allExcluded := models["failed-video"].VideoFilteredSuccessRate
	require.NotNil(t, allExcluded)
	assert.Equal(t, int64(1), allExcluded.ExcludedCount)
	assert.Zero(t, allExcluded.SampleCount)
	assert.Nil(t, allExcluded.Rate)
	old := models["old-video"].VideoFilteredSuccessRate
	require.NotNil(t, old)
	assert.Zero(t, old.ExcludedCount, "matching only a case-insensitive task ID cannot exclude the observation")
	encoded, err := common.Marshal(summary)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "provider-only-detail")
	assert.Contains(t, string(encoded), `"rate":null`)

	require.NoError(t, model.UpdateOption(key, "CodeX100Z"))
	params.Group = ""
	updated, err := Query(params)
	require.NoError(t, err)
	require.NotNil(t, updated.Summary.VideoFilteredSuccessRate)
	assert.Equal(t, int64(1), updated.Summary.VideoFilteredSuccessRate.ExcludedCount, "rule edits apply to existing observations")
	require.NoError(t, model.UpdateOption(key, ""))
	cleared, err := Query(params)
	require.NoError(t, err)
	assert.Empty(t, cleared.VideoErrorExclusions)
	assert.Equal(t, raw.Summary, cleared.Summary)
}
