package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestMain(m *testing.M) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic("failed to open test db: " + err.Error())
	}
	sqlDB, err := db.DB()
	if err != nil {
		panic("failed to get sql.DB: " + err.Error())
	}
	sqlDB.SetMaxOpenConns(1)

	model.DB = db
	model.LOG_DB = db

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = true

	if err := db.AutoMigrate(
		&model.Task{},
		&model.User{},
		&model.Token{},
		&model.Log{},
		&model.Channel{},
		&model.Midjourney{},
		&model.TopUp{},
		&model.UserSubscription{},
		&model.SystemTask{},
		&model.SystemTaskLock{},
		&model.TaskPerfSample{},
		&model.AsyncMediaJob{},
		&model.AsyncMediaEvent{},
	); err != nil {
		panic("failed to migrate: " + err.Error())
	}

	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

func truncate(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		model.DB.Exec("DELETE FROM tasks")
		model.DB.Exec("DELETE FROM users")
		model.DB.Exec("DELETE FROM tokens")
		model.DB.Exec("DELETE FROM logs")
		model.DB.Exec("DELETE FROM channels")
		model.DB.Exec("DELETE FROM midjourneys")
		model.DB.Exec("DELETE FROM top_ups")
		model.DB.Exec("DELETE FROM user_subscriptions")
		model.DB.Exec("DELETE FROM system_task_locks")
		model.DB.Exec("DELETE FROM system_tasks")
	})
}

// DSNs must point at isolated test databases. Measured task tests keep logs in
// a separate database so transaction success cannot depend on log DB placement.
func setupMeasuredTaskBillingDB(t *testing.T) {
	t.Helper()
	dbType := common.DatabaseTypeSQLite
	var driver gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "billing.db"))
	var logDriver gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "logs.db"))
	logType := common.DatabaseTypeSQLite
	if dsn := os.Getenv("TEST_MYSQL_DSN"); dsn != "" {
		dbType = common.DatabaseTypeMySQL
		driver = mysql.Open(dsn)
		if logDSN := os.Getenv("TEST_MYSQL_LOG_DSN"); logDSN != "" {
			logDriver = mysql.Open(logDSN)
			logType = dbType
		}
	} else if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		dbType = common.DatabaseTypePostgreSQL
		driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
		if logDSN := os.Getenv("TEST_POSTGRES_LOG_DSN"); logDSN != "" {
			logDriver = postgres.New(postgres.Config{DSN: logDSN, PreferSimpleProtocol: true})
			logType = dbType
		}
	}
	db, err := gorm.Open(driver, &gorm.Config{})
	require.NoError(t, err)
	logDB, err := gorm.Open(logDriver, &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	if dbType == common.DatabaseTypeSQLite {
		sqlDB.SetMaxOpenConns(1)
	} else {
		sqlDB.SetMaxOpenConns(4)
	}
	sqlLogDB, err := logDB.DB()
	require.NoError(t, err)
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
	model.DB, model.LOG_DB = db, logDB
	common.SetDatabaseTypes(dbType, logType)
	rows := []any{&model.Task{}, &model.User{}, &model.Token{}, &model.Channel{}, &model.UserSubscription{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.MediaRetryEvent{}}
	t.Cleanup(func() {
		for _, row := range rows {
			require.NoError(t, db.Unscoped().Where("1 = 1").Delete(row).Error)
		}
		require.NoError(t, logDB.Where("1 = 1").Delete(&model.Log{}).Error)
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.SetDatabaseTypes(oldMainType, oldLogType)
		require.NoError(t, sqlLogDB.Close())
		require.NoError(t, sqlDB.Close())
	})
	for range 2 {
		require.NoError(t, db.AutoMigrate(rows...))
		require.NoError(t, logDB.AutoMigrate(&model.Log{}))
	}
	for _, row := range rows {
		require.NoError(t, db.Unscoped().Where("1 = 1").Delete(row).Error)
	}
	require.NoError(t, logDB.Where("1 = 1").Delete(&model.Log{}).Error)
}

func seedUser(t *testing.T, id int, quota int) {
	t.Helper()
	user := &model.User{Id: id, Username: "test_user", Quota: quota, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(user).Error)
}

func seedToken(t *testing.T, id int, userId int, key string, remainQuota int) {
	t.Helper()
	token := &model.Token{
		Id:          id,
		UserId:      userId,
		Key:         key,
		Name:        "test_token",
		Status:      common.TokenStatusEnabled,
		RemainQuota: remainQuota,
		UsedQuota:   0,
	}
	require.NoError(t, model.DB.Create(token).Error)
}

func seedSubscription(t *testing.T, id int, userId int, amountTotal int64, amountUsed int64) {
	t.Helper()
	sub := &model.UserSubscription{
		Id:          id,
		UserId:      userId,
		AmountTotal: amountTotal,
		AmountUsed:  amountUsed,
		Status:      "active",
		StartTime:   time.Now().Unix(),
		EndTime:     time.Now().Add(30 * 24 * time.Hour).Unix(),
	}
	require.NoError(t, model.DB.Create(sub).Error)
}

func seedChannel(t *testing.T, id int) {
	t.Helper()
	ch := &model.Channel{Id: id, Name: "test_channel", Key: "sk-test", Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(ch).Error)
}

func seedChargedAccounting(t *testing.T, userID, channelID, tokenID, quota, requestCount int) {
	t.Helper()
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]any{
		"used_quota":    quota,
		"request_count": requestCount,
	}).Error)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channelID).
		Update("used_quota", quota).Error)
	if tokenID > 0 {
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", tokenID).
			Update("used_quota", quota).Error)
	}
}

func makeTask(userId, channelId, quota, tokenId int, billingSource string, subscriptionId int) *model.Task {
	return &model.Task{
		TaskID:    "task_" + time.Now().Format("150405.000"),
		UserId:    userId,
		ChannelId: channelId,
		Quota:     quota,
		Status:    model.TaskStatus(model.TaskStatusInProgress),
		Group:     "default",
		Data:      json.RawMessage(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		Properties: model.Properties{
			OriginModelName: "test-model",
		},
		PrivateData: model.TaskPrivateData{
			BillingSource:  billingSource,
			SubscriptionId: subscriptionId,
			TokenId:        tokenId,
			BillingContext: &model.TaskBillingContext{
				ModelPrice:      0.02,
				GroupRatio:      1.0,
				OriginModelName: "test-model",
			},
		},
	}
}

func TestAsyncMediaMaximumAgeTermination(t *testing.T) {
	for _, tc := range []struct {
		name, status, billing, storage                 string
		provider                                       string
		recoveryStage                                  string
		native                                         model.TaskStatus
		recent, manual, refundFailure                  bool
		reconcile                                      bool
		undispatched, recentDispatch, confirmedFailure bool
		wantStatus, wantBilling                        string
		wantQuota                                      int
	}{
		{name: "recent queued operation remains pending", status: "queued", billing: "pending", recent: true, wantStatus: "queued", wantBilling: "pending", wantQuota: 900},
		{name: "queued timeout terminates before dispatch", status: "queued", billing: "pending", wantStatus: "failed", wantBilling: "not_billable", wantQuota: 900},
		{name: "durable undispatched reservation refunded once", status: "queued", recoveryStage: "prepare", billing: "reserved", native: model.TaskStatusQueued, undispatched: true, wantStatus: "failed", wantBilling: "refunded", wantQuota: 1000},
		{name: "undispatched refund failure recovers without generation", status: "queued", recoveryStage: "prepare", billing: "reserved", native: model.TaskStatusQueued, undispatched: true, refundFailure: true, wantStatus: "failed", wantBilling: "refunded", wantQuota: 1000},
		{name: "recently dispatched accepted operation retains its hold", status: "submitted", billing: "reserved", native: model.TaskStatusInProgress, recentDispatch: true, wantStatus: "failed", wantBilling: "unknown", wantQuota: 900},
		{name: "authoritative failed operation can refund its reservation", status: "submitted", billing: "reserved", native: model.TaskStatusFailure, confirmedFailure: true, wantStatus: "failed", wantBilling: "refunded", wantQuota: 1000},
		{name: "unknown accounting retains its hold", status: "execution_unknown", billing: "unknown", native: model.TaskStatusUnknown, wantStatus: "failed", wantBilling: "unknown", wantQuota: 900},
		{name: "nldw unconfirmed dispatch retains reconciliation evidence past age limit", provider: "nldw", status: "execution_unknown", billing: "unknown", recentDispatch: true, wantStatus: "execution_unknown", wantBilling: "unknown", wantQuota: 900},
		{name: "yuxibox unconfirmed dispatch retains reconciliation evidence past age limit", provider: "yuxibox", status: "execution_unknown", billing: "unknown", native: model.TaskStatusUnknown, recentDispatch: true, wantStatus: "execution_unknown", wantBilling: "unknown", wantQuota: 900},
		{name: "unknown job cannot refund a pending native task automatically", status: "execution_unknown", billing: "reserved", native: model.TaskStatusInProgress, wantStatus: "failed", wantBilling: "unknown", wantQuota: 900},
		{name: "unknown reference recovery retains funds until verification", status: "submitted", billing: "reserved", recoveryStage: "unknown", native: model.TaskStatusInProgress, wantStatus: "failed", wantBilling: "unknown", wantQuota: 900},
		{name: "confirmed replacement reservation retains its reconciliation entry", status: "execution_unknown", billing: "unknown", recoveryStage: "unknown", native: model.TaskStatusUnknown, reconcile: true, wantStatus: "failed", wantBilling: "unknown", wantQuota: 900},
		{name: "submitted without native receipt retains unknown funds", status: "submitted", billing: "reserved", wantStatus: "failed", wantBilling: "unknown", wantQuota: 900},
		{name: "already succeeded output is protected", status: "succeeded", billing: "settled", native: model.TaskStatusSuccess, wantStatus: "succeeded", wantBilling: "settled", wantQuota: 900},
		{name: "expired successful output is protected", status: "expired", billing: "settled", native: model.TaskStatusSuccess, wantStatus: "expired", wantBilling: "settled", wantQuota: 900},
		{name: "expired success without duplicated billing marker protected", status: "expired", billing: "reserved", native: model.TaskStatusSuccess, wantStatus: "expired", wantBilling: "reserved", wantQuota: 900},
		{name: "expired settled evidence without native row protected", status: "expired", billing: "settled", wantStatus: "expired", wantBilling: "settled", wantQuota: 900},
		{name: "successful generation stalled in storage stops without refund", status: "submitted", storage: "failed", billing: "settled", native: model.TaskStatusSuccess, wantStatus: "failed", wantBilling: "settled", wantQuota: 900},
		{name: "expired refunded failure becomes failed", status: "expired", billing: "refunded", native: model.TaskStatusFailure, wantStatus: "failed", wantBilling: "refunded", wantQuota: 900},
		{name: "administrator can end recent unsuccessful expired record", status: "expired", billing: "refunded", native: model.TaskStatusFailure, recent: true, manual: true, wantStatus: "failed", wantBilling: "refunded", wantQuota: 900},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupMeasuredTaskBillingDB(t)
			seedUser(t, 950, 900)
			seedToken(t, 950, 950, "age-limit-token", 900)
			seedChannel(t, 950)
			seedChargedAccounting(t, 950, 950, 950, 100, 1)
			now := time.Now().Unix()
			created := now - model.AsyncMediaMaxTaskAgeSeconds
			if tc.recent {
				created = now
			}
			job := model.AsyncMediaJob{TaskId: "task_age_limit", IdentityHash: "age-limit", UserId: 950, TokenId: 950, ChannelId: 950, Provider: "drama", Model: "test-model", Status: tc.status, BillingStatus: tc.billing, StorageStatus: tc.storage, CreatedAt: created, ExpiresAt: now + 86400, Quota: 100, RequestCipher: []byte("private request"), LeaseToken: "stale-worker", LeaseExpiresAt: now + 60}
			if tc.provider != "" {
				job.Provider = tc.provider
			}
			job.ReferenceRecoveryStage = tc.recoveryStage
			if tc.recentDispatch {
				job.DispatchedAt = now - 10
			}
			if tc.undispatched {
				job.AttemptGeneration, job.ReferenceRetryCount = 1, 1
			}
			if tc.reconcile {
				job.ReferenceRetryCount = 1
			}
			if tc.billing == "pending" {
				job.Quota = 0
			}
			if tc.status == "expired" {
				job.ExpiresAt = now - 1
			}
			require.NoError(t, model.DB.Create(&job).Error)
			var native *model.Task
			if tc.native != "" {
				native = makeTask(950, 950, 100, 950, BillingSourceWallet, 0)
				native.TaskID, native.Status, native.SubmitTime, native.Platform = job.TaskId, tc.native, created, "drama"
				native.AttemptGeneration = job.AttemptGeneration
				native.PrivateData.AsyncMedia, native.PrivateData.DurableBilling, native.PrivateData.SubmitAccountingRecorded = true, true, true
				native.PrivateData.UpstreamTaskID = "known-provider-task"
				if tc.reconcile {
					native.PrivateData.UpstreamTaskID = ""
				}
				if tc.undispatched {
					native.PrivateData.UpstreamTaskID = ""
				}
				if tc.confirmedFailure {
					native.PrivateData.VideoTerminalConfirmed = true
				}
				if tc.native == model.TaskStatusFailure && !tc.confirmedFailure {
					native.Quota = 0
				}
				require.NoError(t, model.DB.Create(native).Error)
			}
			var late *model.Task
			if native != nil {
				copy := *native
				late = &copy
			}
			if tc.refundFailure {
				require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 950).Update("used_quota", 0).Error)
				require.Error(t, FailOverdueAsyncMediaJobs(t.Context(), 100))
				var paused model.AsyncMediaJob
				require.NoError(t, model.DB.First(&paused, job.ID).Error)
				assert.Equal(t, "failed", paused.Status)
				assert.Equal(t, "age_limit_refund", paused.ReferenceRecoveryStage)
				assert.False(t, model.HasUnfinishedSyncTasks(), "local termination must stop the native poller before its refund recovers")
				require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 950).Update("used_quota", 100).Error)
				require.NoError(t, model.DB.Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID).Updates(map[string]any{"next_attempt_at": 0, "lease_expires_at": now - 1}).Error)
			}
			if job.HasPendingSubmissionReconciliation() {
				_, err := model.FailAgedAsyncMediaJob(t.Context(), job, now, true)
				require.ErrorIs(t, err, model.ErrImageConflict)
				sibling := model.AsyncMediaJob{TaskId: "age_cleanup_sibling", IdentityHash: "age-cleanup-sibling", UserId: job.UserId, TokenId: job.TokenId, ChannelId: job.ChannelId, Provider: job.Provider, Status: "queued", BillingStatus: "pending", CreatedAt: created, ExpiresAt: now + 86400}
				require.NoError(t, model.DB.Create(&sibling).Error)
				require.NoError(t, FailOverdueAsyncMediaJobs(t.Context(), 1))
				require.NoError(t, model.DB.First(&sibling, sibling.ID).Error)
				assert.Equal(t, "failed", sibling.Status, "retained reconciliation must not starve other overdue jobs")
			}
			if tc.manual {
				require.NoError(t, TerminateExpiredAsyncMediaJob(t.Context(), job))
			} else {
				require.NoError(t, FailOverdueAsyncMediaJobs(t.Context(), 100))
			}
			require.NoError(t, FailOverdueAsyncMediaJobs(t.Context(), 100))
			var got model.AsyncMediaJob
			require.NoError(t, model.DB.First(&got, job.ID).Error)
			assert.Equal(t, tc.wantStatus, got.Status)
			assert.Equal(t, tc.wantBilling, got.BillingStatus)
			assert.Equal(t, tc.wantQuota, getUserQuota(t, 950))
			if job.HasPendingSubmissionReconciliation() {
				assert.Equal(t, job.RequestCipher, got.RequestCipher)
				assert.Equal(t, job.AttemptGeneration, got.AttemptGeneration)
				assert.Equal(t, job.LeaseToken, got.LeaseToken)
			}
			if got.Status == "failed" {
				assert.Empty(t, got.LeaseToken)
				assert.Empty(t, got.RequestCipher)
				assert.ErrorIs(t, model.UpdateAsyncMediaJob(t.Context(), job, map[string]any{"status": "succeeded"}), model.ErrImageConflict)
				if late != nil && late.Status != model.TaskStatusSuccess && late.Status != model.TaskStatusFailure {
					from := late.Status
					late.Status = model.TaskStatusSuccess
					won, _, err := model.SettleTaskBilling(t.Context(), late, from, 100, nil, "")
					require.NoError(t, err)
					assert.False(t, won, "old generation cannot settle after the policy fence")
				}
			}
			if tc.native == "" && got.Status == "failed" {
				lateTask := makeTask(950, 950, 100, 950, BillingSourceWallet, 0)
				lateTask.TaskID = job.TaskId
				lateTask.PrivateData.AsyncMedia = true
				assert.ErrorIs(t, lateTask.InsertWithContext(t.Context()), model.ErrTaskAttemptConflict, "a late submit receipt cannot recreate a terminated operation")
			}
			if got.Status == "failed" && !tc.manual {
				events, err := model.ListAsyncMediaEvents(t.Context(), job.TaskId)
				require.NoError(t, err)
				assert.True(t, slices.ContainsFunc(events, func(event model.AsyncMediaEvent) bool { return event.EventType == "age_limit_failed" }))
				assert.False(t, slices.ContainsFunc(events, func(event model.AsyncMediaEvent) bool {
					return event.EventType == "upstream_failed" || event.EventType == "reconciliation"
				}))
			}
			if tc.reconcile {
				require.NoError(t, ReconcileTerminatedAsyncMediaReservation(t.Context(), got))
				require.NoError(t, model.DB.First(&got, job.ID).Error)
				assert.Equal(t, "failed", got.Status)
				assert.Equal(t, "refunded", got.BillingStatus)
				assert.Equal(t, 1000, getUserQuota(t, 950))
				assert.ErrorIs(t, ReconcileTerminatedAsyncMediaReservation(t.Context(), got), model.ErrImageConflict)
			} else if got.ReferenceRecoveryStage == "age_limit_unknown" {
				assert.Error(t, ReconcileTerminatedAsyncMediaReservation(t.Context(), got), "an administrator confirmation cannot manufacture a reservation without the existing durable recovery proof")
				assert.Equal(t, 900, getUserQuota(t, 950))
			}
		})
	}
}

func TestAsyncMediaMaximumAgeDispatchAndPollingBarriers(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	now := time.Now().Unix()
	job := model.AsyncMediaJob{TaskId: "age-dispatch", IdentityHash: "age-dispatch", Status: "queued", BillingStatus: "pending", CreatedAt: now - model.AsyncMediaMaxTaskAgeSeconds, ExpiresAt: now + 86400, LeaseToken: "old", LeaseExpiresAt: now + 60}
	require.NoError(t, model.DB.Create(&job).Error)
	assert.ErrorIs(t, model.DispatchAsyncMediaJob(t.Context(), job, map[string]any{"dispatched_at": now}, 0, "original"), model.ErrImageConflict)
	claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "new", 120)
	require.NoError(t, err)
	assert.False(t, claimed)
	var current model.AsyncMediaJob
	require.NoError(t, model.DB.First(&current, job.ID).Error)
	assert.Equal(t, "failed", current.Status)
	ordinary := model.Task{TaskID: "ordinary-unknown", Status: model.TaskStatusUnknown, FailReason: model.AsyncMediaTaskAgeExceededReason, SubmitTime: now - 60}
	require.NoError(t, model.DB.Create(&ordinary).Error)
	page, err := model.GetUnfinishedSyncTaskPage(t.Context(), 0, 0, 10)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, ordinary.TaskID, page[0].TaskID, "provider wording alone cannot block an unrelated UNKNOWN task")
	for index := range 2 {
		id := fmt.Sprintf("protected-expired-%d", index)
		completed := model.AsyncMediaJob{TaskId: id, IdentityHash: id, Status: "expired", BillingStatus: "reserved", CreatedAt: now - model.AsyncMediaMaxTaskAgeSeconds, ExpiresAt: now - 1}
		require.NoError(t, model.DB.Create(&completed).Error)
		require.NoError(t, model.DB.Create(&model.Task{TaskID: id, Status: model.TaskStatusSuccess}).Error)
	}
	waiting := model.AsyncMediaJob{TaskId: "waiting-behind-success", IdentityHash: "waiting-behind-success", Status: "queued", BillingStatus: "pending", CreatedAt: now - model.AsyncMediaMaxTaskAgeSeconds, ExpiresAt: now + 86400}
	require.NoError(t, model.DB.Create(&waiting).Error)
	require.NoError(t, FailOverdueAsyncMediaJobs(t.Context(), 1))
	require.NoError(t, model.DB.First(&waiting, waiting.ID).Error)
	assert.Equal(t, "failed", waiting.Status, "protected successful rows must be excluded before applying the batch limit")
	broken := model.AsyncMediaJob{TaskId: "broken-age-row", IdentityHash: "broken-age-row", Status: "submitted", BillingStatus: "unknown", AttemptGeneration: math.MaxInt64, CreatedAt: now - model.AsyncMediaMaxTaskAgeSeconds, ExpiresAt: now + 86400}
	require.NoError(t, model.DB.Create(&broken).Error)
	later := model.AsyncMediaJob{TaskId: "after-broken-age-row", IdentityHash: "after-broken-age-row", Status: "submitted", StorageStatus: "failed", BillingStatus: "pending", NextAttemptAt: 1 << 62, CreatedAt: now - model.AsyncMediaMaxTaskAgeSeconds, ExpiresAt: now + 86400}
	require.NoError(t, model.DB.Create(&later).Error)
	require.Error(t, FailOverdueAsyncMediaJobs(t.Context(), 1))
	require.NoError(t, model.DB.First(&broken, broken.ID).Error)
	assert.Contains(t, broken.LeaseToken, asyncMediaAgeRetryLeasePrefix)
	assert.Greater(t, broken.LeaseExpiresAt, now)
	assert.Zero(t, broken.NextAttemptAt, "cleanup retry must not rewrite the submission schedule")
	require.NoError(t, FailOverdueAsyncMediaJobs(t.Context(), 1))
	require.NoError(t, model.DB.First(&later, later.ID).Error)
	assert.Equal(t, "failed", later.Status, "one damaged operation must not monopolize the cleanup batch")
	require.NoError(t, model.DB.Model(&broken).Update("lease_expires_at", now-1).Error)
	refundBroken := model.AsyncMediaJob{TaskId: "missing-native-refund", IdentityHash: "missing-native-refund", Status: "failed", BillingStatus: "reserved", ReferenceRecoveryStage: "age_limit_refund", CreatedAt: now - model.AsyncMediaMaxTaskAgeSeconds, ExpiresAt: now + 86400}
	require.NoError(t, model.DB.Create(&refundBroken).Error)
	paused := model.AsyncMediaJob{TaskId: "paused-after-refund-error", IdentityHash: "paused-after-refund-error", Status: "submitted", StorageStatus: "failed", BillingStatus: "pending", NextAttemptAt: 1 << 62, CreatedAt: now - model.AsyncMediaMaxTaskAgeSeconds, ExpiresAt: now + 86400}
	require.NoError(t, model.DB.Create(&paused).Error)
	require.Error(t, FailOverdueAsyncMediaJobs(t.Context(), 1))
	require.NoError(t, model.DB.First(&refundBroken, refundBroken.ID).Error)
	assert.Contains(t, refundBroken.LeaseToken, asyncMediaAgeRetryLeasePrefix)
	assert.Equal(t, "reserved", refundBroken.BillingStatus, "a failed native lookup cannot invent a refund")
	require.NoError(t, FailOverdueAsyncMediaJobs(t.Context(), 1))
	require.NoError(t, model.DB.First(&paused, paused.ID).Error)
	assert.Equal(t, "failed", paused.Status, "expired retry markers must remain behind unattempted paused operations")
}

func TestAsyncMediaMaximumAgeReferenceRecoveryDeadline(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}))
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "age-test:"+base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "age-test")
	seedUser(t, 951, 900)
	seedToken(t, 951, 951, "reference-age-token", 900)
	seedChannel(t, 951)
	seedChargedAccounting(t, 951, 951, 951, 100, 1)
	now := time.Now().Unix()
	created := now - model.AsyncMediaMaxTaskAgeSeconds + 600
	request := AsyncVideoRequest{ReferenceRecoveryVersion: 1, ReferenceRecoveryDeadline: now + 86400}
	plain, err := common.Marshal(request)
	require.NoError(t, err)
	cipher, err := EncryptImagePayload(plain, "media-request:reference-age")
	require.NoError(t, err)
	job := model.AsyncMediaJob{TaskId: "reference-age", IdentityHash: "reference-age", UserId: 951, TokenId: 951, ChannelId: 951, Provider: "drama", Status: "queued", BillingStatus: "reserved", ReferenceRecoveryStage: "submit", ReferenceRetryCount: 1, AttemptGeneration: 1, Quota: 100, CreatedAt: created, ExpiresAt: now + 86400, RequestCipher: cipher, LeaseToken: "reference-age-worker", LeaseExpiresAt: now + 120}
	require.NoError(t, model.DB.Create(&job).Error)
	task := makeTask(951, 951, 100, 951, BillingSourceWallet, 0)
	task.TaskID, task.Status, task.AttemptGeneration = job.TaskId, model.TaskStatusQueued, job.AttemptGeneration
	task.PrivateData.AsyncMedia, task.PrivateData.DurableBilling, task.PrivateData.SubmitAccountingRecorded = true, true, true
	require.NoError(t, model.DB.Create(task).Error)
	previous := SubmitAsyncVideoReferenceRetryFunc
	t.Cleanup(func() { SubmitAsyncVideoReferenceRetryFunc = previous })
	calls := 0
	SubmitAsyncVideoReferenceRetryFunc = func(ctx context.Context, current model.AsyncMediaJob, _ model.Task, _ AsyncVideoRequest) error {
		calls++
		deadline, present := ctx.Deadline()
		require.True(t, present)
		assert.Equal(t, created+model.AsyncMediaMaxTaskAgeSeconds, deadline.Unix(), "reference recovery must inherit the acceptance-age deadline instead of its longer provider deadline")
		return model.UpdateAsyncMediaJob(ctx, current, map[string]any{"reference_recovery_stage": "waiting"})
	}
	require.NoError(t, ProcessAsyncMediaJob(t.Context(), job, DefaultMediaRuntimeConfig()))
	assert.Equal(t, 1, calls)
	assert.Equal(t, 900, getUserQuota(t, 951))
}

func TestPriceDataOtherRatiosFilterAndSnapshot(t *testing.T) {
	priceData := types.PriceData{}

	priceData.AddOtherRatio("zero", 0)
	priceData.AddOtherRatio("negative", -0.5)
	priceData.AddOtherRatio("nan", math.NaN())
	priceData.AddOtherRatio("inf", math.Inf(1))
	priceData.AddOtherRatio("one", 1)
	priceData.AddOtherRatio("positive", 2.5)

	ratios := priceData.OtherRatios()
	require.Len(t, ratios, 2)
	assert.Equal(t, 1.0, ratios["one"])
	assert.Equal(t, 2.5, ratios["positive"])
	assert.True(t, priceData.HasOtherRatio("one"))
	assert.False(t, priceData.HasOtherRatio("zero"))

	ratios["positive"] = 99
	ratios["new"] = 3
	nextSnapshot := priceData.OtherRatios()
	assert.Equal(t, 2.5, nextSnapshot["positive"])
	assert.NotContains(t, nextSnapshot, "new")
}

func TestPriceDataReplaceAndApplyOtherRatios(t *testing.T) {
	priceData := types.PriceData{}

	replaced := priceData.ReplaceOtherRatios(map[string]float64{
		"zero":     0,
		"negative": -3,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
		"one":      1,
		"duration": 2,
		"size":     1.5,
	})

	require.True(t, replaced)
	assert.Equal(t, 3.0, priceData.OtherRatioMultiplier())
	assert.Equal(t, 30.0, priceData.ApplyOtherRatiosToFloat(10))
	assert.Equal(t, 10.0, priceData.RemoveOtherRatiosFromFloat(30))
	assert.True(t, decimal.NewFromInt(30).Equal(priceData.ApplyOtherRatiosToDecimal(decimal.NewFromInt(10))))

	replaced = priceData.ReplaceOtherRatios(map[string]float64{
		"zero": 0,
		"nan":  math.NaN(),
	})

	require.False(t, replaced)
	assert.Nil(t, priceData.OtherRatios())
	assert.Equal(t, 1.0, priceData.OtherRatioMultiplier())
}

func TestTaskBillingOtherFiltersHistoricalOtherRatios(t *testing.T) {
	task := makeTask(1, 1, 100, 0, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.OtherRatios = map[string]float64{
		"seconds":  2,
		"identity": 1,
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	}

	other := taskBillingOther(task).Snapshot()

	assert.Equal(t, 2.0, other["seconds"])
	assert.Equal(t, 1.0, other["identity"])
	assert.NotContains(t, other, "zero")
	assert.NotContains(t, other, "negative")
	assert.NotContains(t, other, "nan")
	assert.NotContains(t, other, "inf")
	assert.NotContains(t, other, "billing_mode")
	assert.NotContains(t, other, "expr_b64")
	assert.NotContains(t, other, "matched_tier")
	assert.NotContains(t, other, "usage_facts")
}

func TestTaskBillingOtherIncludesTieredSnapshotAndKeepsUsageFactsNested(t *testing.T) {
	task := makeTask(1, 1, 100, 0, BillingSourceWallet, 0)
	expression := `tier("720P", u("seconds") * 5)`
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString:    expression,
		EstimatedTier: "720P",
		UsageFacts: map[string]any{
			"resolution": "720P",
			"seconds":    5,
		},
	}

	other := taskBillingOther(task).Snapshot()

	assert.Equal(t, "tiered_expr", other["billing_mode"])
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(expression)), other["expr_b64"])
	assert.Equal(t, "720P", other["matched_tier"])
	facts, ok := other["usage_facts"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{
		"resolution": "720P",
		"seconds":    5,
	}, facts)
	assert.NotContains(t, other, "resolution")
	assert.NotContains(t, other, "seconds")
}

func TestTaskBillingOtherOmitsEmptyUsageFacts(t *testing.T) {
	task := makeTask(1, 1, 100, 0, BillingSourceWallet, 0)
	expression := `tier("base", 1)`
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString:    expression,
		EstimatedTier: "base",
		UsageFacts:    map[string]any{},
	}

	other := taskBillingOther(task).Snapshot()

	assert.Equal(t, "tiered_expr", other["billing_mode"])
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(expression)), other["expr_b64"])
	assert.Equal(t, "base", other["matched_tier"])
	assert.NotContains(t, other, "usage_facts")
}

func callLogTaskConsumption(t *testing.T, info *relaycommon.RelayInfo, task *model.Task) *model.Log {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	ctx.Set("token_name", "test_token")
	LogTaskConsumption(ctx, info, task)
	log := getLastLog(t)
	require.NotNil(t, log)
	return log
}

func TestLogTaskConsumptionIncludesTieredSnapshotUsageFacts(t *testing.T) {
	truncate(t)
	const userID, channelID = 40, 40
	seedUser(t, userID, 10_000)
	seedChannel(t, channelID)

	expression := `tier("720P", u("seconds") * 5)`
	task := makeTask(userID, channelID, 100, 0, BillingSourceWallet, 0)
	info := &relaycommon.RelayInfo{
		UserId:          userID,
		TokenId:         0,
		OriginModelName: "wan2.5-i2v-preview",
		UsingGroup:      "default",
		ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: channelID},
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{Action: "GENERATE"},
		PriceData: types.PriceData{
			ModelPrice:     0.02,
			Quota:          100,
			GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
		},
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			ExprString:    expression,
			EstimatedTier: "720P",
			UsageFacts: map[string]any{
				"resolution": "720P",
				"seconds":    5,
			},
		},
	}

	log := callLogTaskConsumption(t, info, task)

	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
	assert.Equal(t, "tiered_expr", other["billing_mode"])
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(expression)), other["expr_b64"])
	assert.Equal(t, "720P", other["matched_tier"])
	facts, ok := other["usage_facts"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "720P", facts["resolution"])
	assert.Equal(t, float64(5), facts["seconds"])
	assert.NotContains(t, other, "resolution")
	assert.NotContains(t, other, "seconds")
	assert.Contains(t, log.Content, "计算参数：")
	assert.Contains(t, log.Content, "resolution: 720P")
	assert.Contains(t, log.Content, "seconds: 5")
}

func TestLogTaskConsumptionWithoutSnapshotKeepsRatioMode(t *testing.T) {
	truncate(t)
	const userID, channelID = 41, 41
	seedUser(t, userID, 10_000)
	seedChannel(t, channelID)

	priceData := types.PriceData{
		ModelPrice:     0.02,
		Quota:          100,
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}
	priceData.AddOtherRatio("size", 2)
	task := makeTask(userID, channelID, 100, 0, BillingSourceWallet, 0)
	info := &relaycommon.RelayInfo{
		UserId:          userID,
		TokenId:         0,
		OriginModelName: "test-model",
		UsingGroup:      "default",
		ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: channelID},
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{Action: "GENERATE"},
		PriceData:       priceData,
	}

	log := callLogTaskConsumption(t, info, task)

	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
	assert.Equal(t, true, other["is_task"])
	assert.Equal(t, "/v1/videos", other["request_path"])
	assert.NotContains(t, other, "billing_mode")
	assert.NotContains(t, other, "expr_b64")
	assert.NotContains(t, other, "matched_tier")
	assert.NotContains(t, other, "usage_facts")
	assert.Contains(t, log.Content, "计算参数：")
	assert.Contains(t, log.Content, "size: 2.00")
}

func TestTaskBillingOtherSeparatesPluginAndRootDiagnostics(t *testing.T) {
	task := makeTask(1, 1, 100, 0, BillingSourceWallet, 0)
	task.TaskID = "task_public"
	task.PrivateData.UpstreamTaskID = "upstream-private"
	task.PrivateData.NodeName = "node-a"
	task.PrivateData.CurrentAttempt = &model.VideoTaskAttempt{
		PluginKey: "current-video", PluginVersion: "2.0.0", UpstreamModel: "provider-video", Generation: 3, ChannelID: 99,
		UpstreamTaskID: "private-replacement-id", PluginHash: "private-source-hash", Reason: "private-provider-diagnostic",
	}
	task.PrivateData.Execution = &model.TaskExecutionSnapshot{
		TaskPlugin: &model.TaskPluginSnapshot{
			Key:     "document-parser",
			Name:    "Document Parser",
			Version: "1.2.3",
			Author: &model.TaskPluginAuthorSnapshot{
				Name: "Community Author",
				URL:  "https://plugins.example/author",
			},
			APIVersion: 1,
			Generation: 42,
		},
	}

	other := taskBillingOther(task).Snapshot()

	assert.Equal(t, "task_public", other["task_id"])
	adminInfo, ok := other["admin_info"].(map[string]any)
	require.True(t, ok)
	pluginInfo, ok := adminInfo["task_plugin"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "document-parser", pluginInfo["key"])
	assert.Equal(t, "1.2.3", pluginInfo["version"])
	assert.Equal(t, map[string]any{
		"plugin_key": "current-video", "plugin_version": "2.0.0", "upstream_model": "provider-video", "generation": int64(3), "channel_id": 99,
	}, adminInfo["current_video_attempt"])
	assert.Equal(t, map[string]any{
		"name": "Community Author",
		"url":  "https://plugins.example/author",
	}, pluginInfo["author"])

	rootInfo, ok := other["root_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream-private", rootInfo["upstream_task_id"])
	assert.Equal(t, "node-a", rootInfo["node_name"])
	runtimeInfo, ok := rootInfo["task_plugin"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, uint64(42), runtimeInfo["generation"])
	assert.NotContains(t, runtimeInfo, "author")
}

func TestTaskBillingContextPriceDataFiltersMultiplier(t *testing.T) {
	priceData := taskBillingContextPriceData(&model.TaskBillingContext{
		OtherRatios: map[string]float64{
			"seconds":  2,
			"size":     3,
			"identity": 1,
			"zero":     0,
			"negative": -1,
			"nan":      math.NaN(),
			"inf":      math.Inf(1),
		},
	})

	require.NotNil(t, priceData)
	assert.Equal(t, 6.0, priceData.OtherRatioMultiplier())
	assert.Equal(t, map[string]float64{
		"seconds":  2,
		"size":     3,
		"identity": 1,
	}, priceData.OtherRatios())
}

// ---------------------------------------------------------------------------
// Read-back helpers
// ---------------------------------------------------------------------------

func getUserQuota(t *testing.T, id int) int {
	t.Helper()
	var user model.User
	require.NoError(t, model.DB.Select("quota").Where("id = ?", id).First(&user).Error)
	return user.Quota
}

func getUserUsageAccounting(t *testing.T, id int) (int, int) {
	t.Helper()
	var user model.User
	require.NoError(t, model.DB.Select("used_quota", "request_count").Where("id = ?", id).First(&user).Error)
	return user.UsedQuota, user.RequestCount
}

func getChannelUsedQuota(t *testing.T, id int) int64 {
	t.Helper()
	var channel model.Channel
	require.NoError(t, model.DB.Select("used_quota").Where("id = ?", id).First(&channel).Error)
	return channel.UsedQuota
}

func getTokenRemainQuota(t *testing.T, id int) int {
	t.Helper()
	var token model.Token
	require.NoError(t, model.DB.Select("remain_quota").Where("id = ?", id).First(&token).Error)
	return token.RemainQuota
}

func getTokenUsedQuota(t *testing.T, id int) int {
	t.Helper()
	var token model.Token
	require.NoError(t, model.DB.Select("used_quota").Where("id = ?", id).First(&token).Error)
	return token.UsedQuota
}

func getSubscriptionUsed(t *testing.T, id int) int64 {
	t.Helper()
	var sub model.UserSubscription
	require.NoError(t, model.DB.Select("amount_used").Where("id = ?", id).First(&sub).Error)
	return sub.AmountUsed
}

func getTaskQuota(t *testing.T, id int64) int {
	t.Helper()
	var task model.Task
	require.NoError(t, model.DB.Select("quota").Where("id = ?", id).First(&task).Error)
	return task.Quota
}

func getMidjourneyTask(t *testing.T, id int) model.Midjourney {
	t.Helper()
	var task model.Midjourney
	require.NoError(t, model.DB.First(&task, id).Error)
	return task
}

func getLastLog(t *testing.T) *model.Log {
	t.Helper()
	var log model.Log
	err := model.LOG_DB.Order("id desc").First(&log).Error
	if err != nil {
		return nil
	}
	return &log
}

func countLogs(t *testing.T) int64 {
	t.Helper()
	var count int64
	model.LOG_DB.Model(&model.Log{}).Count(&count)
	return count
}

// ===========================================================================
// Legacy Midjourney billing tests
// ===========================================================================

func TestPrepareMidjourneyTaskBillingKeepsUnbilledMarkerClear(t *testing.T) {
	task := &model.Midjourney{Quota: 900, TokenId: 7, BillingChannelId: 8}

	prepared, err := PrepareMidjourneyTaskBilling(&relaycommon.RelayInfo{}, task, 900, false)

	require.NoError(t, err)
	assert.False(t, prepared)
	assert.Zero(t, task.Quota)
	assert.Zero(t, task.TokenId)
	assert.Zero(t, task.BillingChannelId)
}

func TestSettleMidjourneyTaskBillingRequiresPersistedTask(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 49, 49, 49
	const initialUserQuota, initialTokenQuota, chargedQuota = 10000, 5000, 3000
	seedUser(t, userID, initialUserQuota)
	seedToken(t, tokenID, userID, "sk-midjourney-unpersisted", initialTokenQuota)
	seedChannel(t, channelID)

	relayInfo := &relaycommon.RelayInfo{
		UserId:    userID,
		TokenId:   tokenID,
		TokenKey:  "sk-midjourney-unpersisted",
		UserQuota: initialUserQuota,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId: channelID,
		},
	}
	task := &model.Midjourney{UserId: userID, ChannelId: channelID}
	prepared, err := PrepareMidjourneyTaskBilling(relayInfo, task, chargedQuota, true)
	require.NoError(t, err)
	require.True(t, prepared)

	billed, err := SettleMidjourneyTaskBilling(relayInfo, task, prepared)

	require.Error(t, err)
	assert.False(t, billed)
	assert.Equal(t, initialUserQuota, getUserQuota(t, userID))
	assert.Equal(t, initialTokenQuota, getTokenRemainQuota(t, tokenID))
}

func TestMidjourneyRefundRestoresEveryAccountingElementOnBillingChannel(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, billingChannelID, executionChannelID = 50, 50, 50, 51
	const initialUserQuota, initialTokenQuota, chargedQuota = 10000, 5000, 3000
	seedUser(t, userID, initialUserQuota)
	seedToken(t, tokenID, userID, "sk-midjourney", initialTokenQuota)
	seedChannel(t, billingChannelID)
	seedChannel(t, executionChannelID)

	relayInfo := &relaycommon.RelayInfo{
		UserId:     userID,
		TokenId:    tokenID,
		TokenKey:   "sk-midjourney",
		UserQuota:  initialUserQuota,
		UsingGroup: "default",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId: billingChannelID,
		},
	}
	task := &model.Midjourney{
		UserId:    userID,
		Action:    "IMAGINE",
		MjId:      "mj-accounting-refund",
		ChannelId: executionChannelID,
		Progress:  "0%",
	}

	prepared, err := PrepareMidjourneyTaskBilling(relayInfo, task, chargedQuota, true)
	require.NoError(t, err)
	require.True(t, prepared)
	assert.Equal(t, chargedQuota, task.Quota)
	assert.Zero(t, task.TokenId)
	assert.Equal(t, billingChannelID, task.BillingChannelId)
	require.NoError(t, task.Insert())

	billed, err := SettleMidjourneyTaskBilling(relayInfo, task, prepared)
	require.NoError(t, err)
	require.True(t, billed)
	assert.Equal(t, initialUserQuota-chargedQuota, getUserQuota(t, userID))
	assert.Equal(t, initialTokenQuota-chargedQuota, getTokenRemainQuota(t, tokenID))
	persisted := getMidjourneyTask(t, task.Id)
	assert.Equal(t, chargedQuota, persisted.Quota)
	assert.Equal(t, tokenID, persisted.TokenId)
	assert.Equal(t, billingChannelID, persisted.BillingChannelId)

	seedChargedAccounting(t, userID, billingChannelID, tokenID, chargedQuota, 1)

	assert.True(t, RefundMidjourneyQuota(ctx, task, "构图失败"))
	assert.Equal(t, initialUserQuota, getUserQuota(t, userID))
	assert.Equal(t, initialTokenQuota, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Zero(t, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Zero(t, getChannelUsedQuota(t, billingChannelID))
	assert.Zero(t, getChannelUsedQuota(t, executionChannelID))

	persisted = getMidjourneyTask(t, task.Id)
	assert.Zero(t, persisted.Quota)
	assert.Equal(t, tokenID, persisted.TokenId)
	assert.Equal(t, billingChannelID, persisted.BillingChannelId)
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Equal(t, chargedQuota, log.Quota)
	assert.Equal(t, tokenID, log.TokenId)
	assert.Equal(t, billingChannelID, log.ChannelId)

	assert.True(t, RefundMidjourneyQuota(ctx, task, "duplicate poll"))
	assert.Equal(t, int64(1), countLogs(t))
}

func TestSettleMidjourneyTaskBillingFundingFailureClearsMarkers(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 52, 52, 52
	const initialUserQuota, initialTokenQuota, chargedQuota = 10000, 5000, 3000
	seedUser(t, userID, initialUserQuota)
	seedToken(t, tokenID, userID, "sk-midjourney-funding-failure", initialTokenQuota)
	seedChannel(t, channelID)

	relayInfo := &relaycommon.RelayInfo{
		UserId:    userID,
		TokenId:   tokenID,
		TokenKey:  "sk-midjourney-funding-failure",
		UserQuota: initialUserQuota,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId: channelID,
		},
	}
	task := &model.Midjourney{UserId: userID, MjId: "mj-funding-failure", ChannelId: channelID}
	prepared, err := PrepareMidjourneyTaskBilling(relayInfo, task, chargedQuota, true)
	require.NoError(t, err)
	require.True(t, prepared)
	require.NoError(t, task.Insert())

	require.NoError(t, model.DB.Exec(`
		CREATE TRIGGER fail_midjourney_user_update
		BEFORE UPDATE ON users
		WHEN OLD.id = 52
		BEGIN
			SELECT RAISE(ABORT, 'forced user quota failure');
		END;
	`).Error)
	t.Cleanup(func() {
		model.DB.Exec("DROP TRIGGER IF EXISTS fail_midjourney_user_update")
	})

	billed, err := SettleMidjourneyTaskBilling(relayInfo, task, prepared)

	require.Error(t, err)
	assert.False(t, billed)
	assert.Equal(t, initialUserQuota, getUserQuota(t, userID))
	assert.Equal(t, initialTokenQuota, getTokenRemainQuota(t, tokenID))
	persisted := getMidjourneyTask(t, task.Id)
	assert.Zero(t, persisted.Quota)
	assert.Zero(t, persisted.TokenId)
	assert.Zero(t, persisted.BillingChannelId)
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Zero(t, usedQuota)
	assert.Zero(t, requestCount)
	assert.Zero(t, getChannelUsedQuota(t, channelID))
	assert.Zero(t, countLogs(t))
}

func TestSettleMidjourneyTaskBillingTokenFailureKeepsFundingRefundable(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 53, 53, 53
	const initialUserQuota, initialTokenQuota, chargedQuota = 10000, 5000, 3000
	seedUser(t, userID, initialUserQuota)
	seedToken(t, tokenID, userID, "sk-midjourney-token-failure", initialTokenQuota)
	seedChannel(t, channelID)

	relayInfo := &relaycommon.RelayInfo{
		UserId:    userID,
		TokenId:   tokenID,
		TokenKey:  "sk-midjourney-token-failure",
		UserQuota: initialUserQuota,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId: channelID,
		},
	}
	task := &model.Midjourney{UserId: userID, MjId: "mj-token-failure", ChannelId: channelID}
	prepared, err := PrepareMidjourneyTaskBilling(relayInfo, task, chargedQuota, true)
	require.NoError(t, err)
	require.True(t, prepared)
	require.NoError(t, task.Insert())

	require.NoError(t, model.DB.Exec(`
		CREATE TRIGGER fail_midjourney_token_update
		BEFORE UPDATE ON tokens
		WHEN OLD.id = 53
		BEGIN
			SELECT RAISE(ABORT, 'forced token quota failure');
		END;
	`).Error)
	t.Cleanup(func() {
		model.DB.Exec("DROP TRIGGER IF EXISTS fail_midjourney_token_update")
	})

	billed, err := SettleMidjourneyTaskBilling(relayInfo, task, prepared)

	require.Error(t, err)
	require.True(t, billed)
	assert.Equal(t, initialUserQuota-chargedQuota, getUserQuota(t, userID))
	assert.Equal(t, initialTokenQuota, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))
	persisted := getMidjourneyTask(t, task.Id)
	assert.Equal(t, chargedQuota, persisted.Quota)
	assert.Zero(t, persisted.TokenId)
	assert.Equal(t, channelID, persisted.BillingChannelId)

	seedChargedAccounting(t, userID, channelID, 0, chargedQuota, 1)
	assert.True(t, RefundMidjourneyQuota(ctx, task, "token settlement failed"))
	assert.Equal(t, initialUserQuota, getUserQuota(t, userID))
	assert.Equal(t, initialTokenQuota, getTokenRemainQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Zero(t, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Zero(t, getChannelUsedQuota(t, channelID))
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Zero(t, log.TokenId)
}

func TestPrepareMidjourneyTaskBillingRejectsSubscriptionBeforeCharge(t *testing.T) {
	task := &model.Midjourney{Quota: 900, TokenId: 7, BillingChannelId: 8}
	relayInfo := &relaycommon.RelayInfo{BillingSource: BillingSourceSubscription, SubscriptionId: 1}

	prepared, err := PrepareMidjourneyTaskBilling(relayInfo, task, 900, true)

	require.Error(t, err)
	assert.False(t, prepared)
	assert.Zero(t, task.Quota)
	assert.Zero(t, task.TokenId)
	assert.Zero(t, task.BillingChannelId)
}

func TestRefundMidjourneyQuotaUsesLegacyChannelFallbackWithoutTokenAdjustment(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 54, 54, 54
	const walletAfterCharge, tokenQuota, chargedQuota = 7000, 5000, 3000
	seedUser(t, userID, walletAfterCharge)
	seedToken(t, tokenID, userID, "sk-midjourney-legacy", tokenQuota)
	seedChannel(t, channelID)
	seedChargedAccounting(t, userID, channelID, 0, chargedQuota, 1)
	task := &model.Midjourney{
		UserId:    userID,
		MjId:      "mj-legacy-fallback",
		Action:    "IMAGINE",
		ChannelId: channelID,
		Quota:     chargedQuota,
		TokenId:   0,
		Progress:  "0%",
	}
	require.NoError(t, task.Insert())

	assert.True(t, RefundMidjourneyQuota(ctx, task, "legacy failure"))

	assert.Equal(t, walletAfterCharge+chargedQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenQuota, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Zero(t, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Zero(t, getChannelUsedQuota(t, channelID))
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, channelID, log.ChannelId)
	assert.Zero(t, log.TokenId)
}

// ===========================================================================
// RefundTaskQuota tests
// ===========================================================================

func TestRefundTaskQuota_Wallet(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 1, 1, 1
	const initQuota, preConsumed = 10000, 3000
	const tokenRemain = 5000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-test-key", tokenRemain)
	seedChannel(t, channelID)
	seedChargedAccounting(t, userID, channelID, tokenID, preConsumed, 1)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, RefundTaskQuota(ctx, task, "task failed: upstream error"))

	// User quota should increase by preConsumed
	assert.Equal(t, initQuota+preConsumed, getUserQuota(t, userID))

	// Token remain_quota should increase, used_quota should decrease
	assert.Equal(t, tokenRemain+preConsumed, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Zero(t, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Zero(t, getChannelUsedQuota(t, channelID))

	// A refund log should be created
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Equal(t, preConsumed, log.Quota)
	assert.Equal(t, "test-model", log.ModelName)
	assert.Zero(t, task.Quota)
	assert.Zero(t, getTaskQuota(t, task.ID))
}

func TestRefundTaskQuota_Subscription(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID, subID = 2, 2, 2, 1
	const preConsumed = 2000
	const subTotal, subUsed int64 = 100000, 50000
	const tokenRemain = 8000

	seedUser(t, userID, 0)
	seedToken(t, tokenID, userID, "sk-sub-key", tokenRemain)
	seedChannel(t, channelID)
	seedSubscription(t, subID, userID, subTotal, subUsed)
	seedChargedAccounting(t, userID, channelID, tokenID, preConsumed, 1)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceSubscription, subID)
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, RefundTaskQuota(ctx, task, "subscription task failed"))

	// Subscription used should decrease by preConsumed
	assert.Equal(t, subUsed-int64(preConsumed), getSubscriptionUsed(t, subID))

	// Token should also be refunded
	assert.Equal(t, tokenRemain+preConsumed, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Zero(t, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Zero(t, getChannelUsedQuota(t, channelID))

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Zero(t, getTaskQuota(t, task.ID))
}

func TestRefundTaskQuota_ZeroQuota(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID = 3
	seedUser(t, userID, 5000)

	task := makeTask(userID, 0, 0, 0, BillingSourceWallet, 0)

	assert.True(t, RefundTaskQuota(ctx, task, "zero quota task"))

	// No change to user quota
	assert.Equal(t, 5000, getUserQuota(t, userID))

	// No log created
	assert.Equal(t, int64(0), countLogs(t))
}

func TestRefundTaskQuota_NoToken(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, channelID = 4, 4
	const initQuota, preConsumed = 10000, 1500

	seedUser(t, userID, initQuota)
	seedChannel(t, channelID)
	seedChargedAccounting(t, userID, channelID, 0, preConsumed, 1)

	task := makeTask(userID, channelID, preConsumed, 0, BillingSourceWallet, 0) // TokenId=0
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, RefundTaskQuota(ctx, task, "no token task failed"))

	// User quota refunded
	assert.Equal(t, initQuota+preConsumed, getUserQuota(t, userID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Zero(t, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Zero(t, getChannelUsedQuota(t, channelID))

	// Log created
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Zero(t, getTaskQuota(t, task.ID))
}

func TestRefundTaskQuota_FundingFailureKeepsAccountingAndPendingMarker(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, channelID, preConsumed = 5, 5, 1200
	seedUser(t, userID, 5000)
	seedChannel(t, channelID)
	seedChargedAccounting(t, userID, channelID, 0, preConsumed, 1)
	task := makeTask(userID, channelID, preConsumed, 0, BillingSourceSubscription, 9999)
	task.Status = model.TaskStatusFailure
	require.NoError(t, model.DB.Create(task).Error)

	assert.False(t, RefundTaskQuota(ctx, task, "subscription missing"))
	assert.Equal(t, 5000, getUserQuota(t, userID))
	assert.Equal(t, preConsumed, task.Quota)
	assert.Equal(t, preConsumed, getTaskQuota(t, task.ID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Equal(t, preConsumed, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Equal(t, int64(preConsumed), getChannelUsedQuota(t, channelID))
	assert.Equal(t, int64(0), countLogs(t))
}

// ===========================================================================
// RecalculateTaskQuota tests
// ===========================================================================

func TestRecalculate_PositiveDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 10, 10, 10
	const initQuota, preConsumed = 10000, 2000
	const actualQuota = 3000 // under-charged by 1000
	const tokenRemain = 5000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-recalc-pos", tokenRemain)
	seedChannel(t, channelID)
	seedChargedAccounting(t, userID, channelID, tokenID, preConsumed, 1)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)

	RecalculateTaskQuota(ctx, task, actualQuota, "adaptor adjustment")

	// User quota should decrease by the delta (1000 additional charge)
	assert.Equal(t, initQuota-(actualQuota-preConsumed), getUserQuota(t, userID))

	// Token should also be charged the delta
	assert.Equal(t, tokenRemain-(actualQuota-preConsumed), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, actualQuota, getTokenUsedQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Equal(t, actualQuota, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Equal(t, int64(actualQuota), getChannelUsedQuota(t, channelID))

	// task.Quota should be updated to actualQuota
	assert.Equal(t, actualQuota, task.Quota)

	// Log type should be Consume (additional charge)
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeConsume, log.Type)
	assert.Equal(t, actualQuota-preConsumed, log.Quota)
}

func TestRecalculate_NegativeDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 11, 11, 11
	const initQuota, preConsumed = 10000, 5000
	const actualQuota = 3000 // over-charged by 2000
	const tokenRemain = 5000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-recalc-neg", tokenRemain)
	seedChannel(t, channelID)
	seedChargedAccounting(t, userID, channelID, tokenID, preConsumed, 1)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)

	RecalculateTaskQuota(ctx, task, actualQuota, "adaptor adjustment")

	// User quota should increase by abs(delta) = 2000 (refund overpayment)
	assert.Equal(t, initQuota+(preConsumed-actualQuota), getUserQuota(t, userID))

	// Token should be refunded the difference
	assert.Equal(t, tokenRemain+(preConsumed-actualQuota), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, actualQuota, getTokenUsedQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Equal(t, actualQuota, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Equal(t, int64(actualQuota), getChannelUsedQuota(t, channelID))

	// task.Quota updated
	assert.Equal(t, actualQuota, task.Quota)

	// Log type should be Refund
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Equal(t, preConsumed-actualQuota, log.Quota)
}

func TestRecalculate_ZeroDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID = 12
	const initQuota, preConsumed = 10000, 3000

	seedUser(t, userID, initQuota)

	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)

	RecalculateTaskQuota(ctx, task, preConsumed, "exact match")

	// No change to user quota
	assert.Equal(t, initQuota, getUserQuota(t, userID))

	// No log created (delta is zero)
	assert.Equal(t, int64(0), countLogs(t))
}

func TestRecalculate_ActualQuotaZero(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, preConsumed = 13, 5000
	const initQuota = 10000

	seedUser(t, userID, initQuota)

	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)
	require.NoError(t, model.DB.Create(task).Error)

	RecalculateTaskQuota(ctx, task, 0, "zero actual")

	assert.Equal(t, initQuota+preConsumed, getUserQuota(t, userID))
	assert.Zero(t, task.Quota)
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Equal(t, preConsumed, log.Quota)
}

func TestRecalculate_RejectsNegativeActualQuota(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, preConsumed = 34, 5000
	const initQuota = 10000
	seedUser(t, userID, initQuota)
	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)

	RecalculateTaskQuota(ctx, task, -1, "invalid negative actual")

	assert.Equal(t, initQuota, getUserQuota(t, userID))
	assert.Equal(t, preConsumed, task.Quota)
	assert.Equal(t, int64(0), countLogs(t))
}

func TestRecalculate_Subscription_NegativeDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID, subID = 14, 14, 14, 2
	const preConsumed = 5000
	const actualQuota = 2000 // over-charged by 3000
	const subTotal, subUsed int64 = 100000, 50000
	const tokenRemain = 8000

	seedUser(t, userID, 0)
	seedToken(t, tokenID, userID, "sk-sub-recalc", tokenRemain)
	seedChannel(t, channelID)
	seedSubscription(t, subID, userID, subTotal, subUsed)
	seedChargedAccounting(t, userID, channelID, tokenID, preConsumed, 1)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceSubscription, subID)

	RecalculateTaskQuota(ctx, task, actualQuota, "subscription over-charge")

	// Subscription used should decrease by delta (refund 3000)
	assert.Equal(t, subUsed-int64(preConsumed-actualQuota), getSubscriptionUsed(t, subID))

	// Token refunded
	assert.Equal(t, tokenRemain+(preConsumed-actualQuota), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, actualQuota, getTokenUsedQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Equal(t, actualQuota, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Equal(t, int64(actualQuota), getChannelUsedQuota(t, channelID))

	assert.Equal(t, actualQuota, task.Quota)

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
}

// ===========================================================================
// CAS + Billing integration tests
// Simulates the flow in updateVideoSingleTask (service/task_polling.go)
// ===========================================================================

// simulatePollBilling reproduces the CAS + billing logic from updateVideoSingleTask.
// It takes a persisted task (already in DB), applies the new status, and performs
// the conditional update + billing exactly as the polling loop does.
func simulatePollBilling(ctx context.Context, task *model.Task, newStatus model.TaskStatus, actualQuota int) {
	snap := task.Snapshot()

	shouldRefund := false
	shouldSettle := false
	quota := task.Quota

	task.Status = newStatus
	switch string(newStatus) {
	case model.TaskStatusSuccess:
		task.Progress = "100%"
		task.FinishTime = 9999
		shouldSettle = true
	case model.TaskStatusFailure:
		task.Progress = "100%"
		task.FinishTime = 9999
		task.FailReason = "upstream error"
		if quota != 0 {
			shouldRefund = true
		}
	default:
		task.Progress = "50%"
	}

	isDone := task.Status == model.TaskStatus(model.TaskStatusSuccess) || task.Status == model.TaskStatus(model.TaskStatusFailure)
	if isDone && snap.Status != task.Status {
		won, err := task.UpdateWithStatus(snap.Status)
		if err != nil {
			shouldRefund = false
			shouldSettle = false
		} else if !won {
			shouldRefund = false
			shouldSettle = false
		}
	} else if !snap.Equal(task.Snapshot()) {
		_, _ = task.UpdateWithStatus(snap.Status)
	}

	if shouldSettle && actualQuota > 0 {
		RecalculateTaskQuota(ctx, task, actualQuota, "test settle")
	}
	if shouldRefund {
		RefundTaskQuota(ctx, task, task.FailReason)
	}
}

func TestCASGuardedRefund_Win(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 20, 20, 20
	const initQuota, preConsumed = 10000, 4000
	const tokenRemain = 6000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-cas-refund-win", tokenRemain)
	seedChannel(t, channelID)
	seedChargedAccounting(t, userID, channelID, tokenID, preConsumed, 1)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	require.NoError(t, model.DB.Create(task).Error)

	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusFailure), 0)

	// CAS wins: task in DB should now be FAILURE
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloaded.Status)
	assert.Zero(t, reloaded.Quota)

	// Refund should have happened
	assert.Equal(t, initQuota+preConsumed, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain+preConsumed, getTokenRemainQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Zero(t, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Zero(t, getChannelUsedQuota(t, channelID))

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
}

func TestCASGuardedRefund_Lose(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 21, 21, 21
	const initQuota, preConsumed = 10000, 4000
	const tokenRemain = 6000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-cas-refund-lose", tokenRemain)
	seedChannel(t, channelID)
	seedChargedAccounting(t, userID, channelID, tokenID, preConsumed, 1)

	// Create task with IN_PROGRESS in DB
	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	require.NoError(t, model.DB.Create(task).Error)

	// Simulate another process already transitioning to FAILURE
	model.DB.Model(&model.Task{}).Where("id = ?", task.ID).Update("status", model.TaskStatusFailure)

	// Our process still has the old in-memory state (IN_PROGRESS) and tries to transition
	// task.Status is still IN_PROGRESS in the snapshot
	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusFailure), 0)

	// CAS lost: user quota should NOT change (no double refund)
	assert.Equal(t, initQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain, getTokenRemainQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Equal(t, preConsumed, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Equal(t, int64(preConsumed), getChannelUsedQuota(t, channelID))

	// No billing log should be created
	assert.Equal(t, int64(0), countLogs(t))
}

func TestMeasuredVideoSubscriptionFailureRefundsOnceAndKeepsActualSeconds(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	const userID, tokenID, channelID, subscriptionID = 71, 71, 71, 71
	const reservedQuota = 4_000
	seedUser(t, userID, 10_000)
	seedToken(t, tokenID, userID, "sk-measured-subscription", 6_000)
	seedChannel(t, channelID)
	seedSubscription(t, subscriptionID, userID, 10_000, reservedQuota)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", tokenID).Update("used_quota", reservedQuota).Error)

	task := makeTask(userID, channelID, reservedQuota, tokenID, BillingSourceSubscription, subscriptionID)
	expression := `tier("seconds", u("seconds"))`
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString:       expression,
		ExprHash:         billingexpr.ExprHashString(expression),
		GroupRatio:       1,
		QuotaPerUnit:     1_000,
		ExprVersion:      1,
		TaskUsageBilling: true,
		UsageFacts:       map[string]any{"seconds": float64(4)},
		EstimatedTier:    "seconds",
	}
	require.NoError(t, model.DB.Create(task).Error)
	recorded, err := model.RecordMeasuredTaskSubmitAccounting(t.Context(), task)
	require.NoError(t, err)
	assert.True(t, recorded)
	recorded, err = model.RecordMeasuredTaskSubmitAccounting(t.Context(), task)
	require.NoError(t, err)
	assert.False(t, recorded)
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Equal(t, reservedQuota, usedQuota)
	assert.Equal(t, 1, requestCount)

	stale := *task
	task.Status = model.TaskStatusFailure
	task.FailReason = "upstream duration exceeds reserved limit"
	won, err := SettleMeasuredVideoTask(t.Context(), task, model.TaskStatusInProgress, 0, map[string]any{"seconds": float64(6)}, nil)
	require.NoError(t, err)
	assert.True(t, won)
	stale.Status = model.TaskStatusFailure
	won, err = SettleMeasuredVideoTask(t.Context(), &stale, model.TaskStatusInProgress, 0, nil, nil)
	require.NoError(t, err)
	assert.False(t, won)

	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), persisted.Status)
	assert.Zero(t, persisted.Quota)
	assert.Equal(t, float64(6), persisted.PrivateData.BillingContext.TieredSnapshot.UsageFacts["seconds"])
	assert.Equal(t, 10_000, getUserQuota(t, userID))
	assert.Equal(t, 10_000, getTokenRemainQuota(t, tokenID))
	usedQuota, requestCount = getUserUsageAccounting(t, userID)
	assert.Zero(t, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Zero(t, getChannelUsedQuota(t, channelID))
	var subscription model.UserSubscription
	require.NoError(t, model.DB.First(&subscription, subscriptionID).Error)
	assert.Zero(t, subscription.AmountUsed)
	assert.Equal(t, int64(1), countLogs(t))
}

func TestMeasuredVideoConcurrentSettlementChargesOnce(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	const userID, tokenID, channelID = 74, 74, 74
	const reservedQuota = 5_000
	seedUser(t, userID, 10_000)
	seedToken(t, tokenID, userID, "sk-measured-concurrent", 8_000)
	seedChannel(t, channelID)
	seedChargedAccounting(t, userID, channelID, tokenID, reservedQuota, 1)

	expression := `tier("seconds", u("seconds"))`
	task := makeTask(userID, channelID, reservedQuota, tokenID, BillingSourceWallet, 0)
	task.PrivateData.SubmitAccountingRecorded = true
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString: expression, ExprHash: billingexpr.ExprHashString(expression),
		GroupRatio: 1, QuotaPerUnit: 1_000, ExprVersion: 1, TaskUsageBilling: true,
		UsageFacts: map[string]any{"seconds": float64(5)},
	}
	require.NoError(t, model.DB.Create(task).Error)
	var saved model.Task
	require.NoError(t, model.DB.First(&saved, task.ID).Error)

	type result struct {
		won bool
		err error
	}
	ready := make(chan struct{})
	outcomes := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-ready
			candidate := saved
			candidate.Status = model.TaskStatusSuccess
			won, err := SettleMeasuredVideoTask(t.Context(), &candidate, model.TaskStatusInProgress, 3_250, map[string]any{"seconds": 3.25}, nil)
			outcomes <- result{won: won, err: err}
		})
	}
	close(ready)
	workers.Wait()
	close(outcomes)
	winners := 0
	for outcome := range outcomes {
		require.NoError(t, outcome.err)
		if outcome.won {
			winners++
		}
	}
	assert.Equal(t, 1, winners)
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), persisted.Status)
	assert.Equal(t, 3_250, persisted.Quota)
	assert.Equal(t, 3.25, persisted.PrivateData.BillingContext.TieredSnapshot.UsageFacts["seconds"])
	assert.Equal(t, 11_750, getUserQuota(t, userID))
	assert.Equal(t, 9_750, getTokenRemainQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Equal(t, 3_250, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Equal(t, int64(3_250), getChannelUsedQuota(t, channelID))
	assert.Equal(t, int64(1), countLogs(t))
}

func TestDurableTaskReservationRequiresFullPersistedUpperBound(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	oldRedis, oldEnabled, oldBatch := common.RDB, common.RedisEnabled, common.BatchUpdateEnabled
	common.RDB, common.RedisEnabled, common.BatchUpdateEnabled = client, true, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled, common.BatchUpdateEnabled = oldRedis, oldEnabled, oldBatch
		require.NoError(t, client.Close())
	})

	const userID, tokenID = 72, 72
	initialQuota := common.GetTrustQuota() + 100
	seedUser(t, userID, initialQuota)
	seedToken(t, tokenID, userID, "sk-durable-task-reservation", initialQuota)
	// Service's SQLite TestMain uses AutoMigrate directly and does not run
	// model.InitDB's reserved-column setup. Prime the token cache just as the
	// request authentication layer would before the relay billing session.
	require.NoError(t, client.HSet(context.Background(), "token:"+common.GenerateHMAC("sk-durable-task-reservation"), map[string]any{
		"Id": tokenID, "UserId": userID, "RemainQuota": initialQuota, "UsedQuota": 0,
		"Status": common.TokenStatusEnabled,
	}).Err())
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Set("token_quota", initialQuota)
	info := &relaycommon.RelayInfo{
		UserId:      userID,
		TokenId:     tokenID,
		TokenKey:    "sk-durable-task-reservation",
		UserSetting: dto.UserSetting{BillingPreference: "wallet_only"},
	}
	c.Set("async_media_prepare", true)
	require.Nil(t, PreConsumeDurableTaskBilling(c, 40, info))
	assert.Nil(t, info.Billing)
	assert.Equal(t, initialQuota, getUserQuota(t, userID))
	c.Set("async_media_prepare", false)
	require.Nil(t, PreConsumeDurableTaskBilling(c, 40, info))
	require.NotNil(t, info.Billing)
	assert.True(t, HasDurableTaskReservation(info, 40))
	assert.False(t, HasDurableTaskReservation(info, 41))
	assert.Equal(t, initialQuota-40, getUserQuota(t, userID))
	assert.Equal(t, initialQuota-40, getTokenRemainQuota(t, tokenID))
	cachedUserQuota, err := model.GetUserQuota(userID, false)
	require.NoError(t, err)
	assert.Equal(t, initialQuota-40, cachedUserQuota)

	require.NoError(t, info.Billing.Reserve(60))
	assert.True(t, HasDurableTaskReservation(info, 60))
	assert.Equal(t, initialQuota-60, getUserQuota(t, userID))
	assert.Equal(t, initialQuota-60, getTokenRemainQuota(t, tokenID))
	require.Error(t, info.Billing.Reserve(initialQuota+1))
	assert.False(t, HasDurableTaskReservation(info, initialQuota+1))
	assert.Equal(t, initialQuota-60, getUserQuota(t, userID))
	require.Error(t, info.Billing.Settle(61), "durable charges cannot exceed the reservation")
	assert.Equal(t, initialQuota-60, getUserQuota(t, userID))

	assert.False(t, HasDurableTaskReservation(&relaycommon.RelayInfo{Billing: &BillingSession{preConsumedQuota: 60}}, 0))
	assert.False(t, HasDurableTaskReservation(&relaycommon.RelayInfo{Billing: &BillingSession{durable: true, trusted: true, preConsumedQuota: 60}}, 0))
}

func TestMeasuredVideoRefundSurvivesDeletedChannelAndCredentials(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	const userID, tokenID, channelID = 73, 73, 73
	const reservedQuota = 3_000
	seedUser(t, userID, 7_000)
	seedToken(t, tokenID, userID, "sk-measured-deleted", 7_000)
	seedChannel(t, channelID)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", tokenID).Update("used_quota", reservedQuota).Error)

	task := makeTask(userID, channelID, reservedQuota, tokenID, BillingSourceWallet, 0)
	expression := `tier("seconds", u("seconds"))`
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString: expression, ExprHash: billingexpr.ExprHashString(expression),
		GroupRatio: 1, QuotaPerUnit: 1_000, ExprVersion: 1, TaskUsageBilling: true,
		UsageFacts: map[string]any{"seconds": float64(3)},
	}
	require.NoError(t, model.DB.Create(task).Error)
	_, err := model.RecordMeasuredTaskSubmitAccounting(t.Context(), task)
	require.NoError(t, err)
	require.NoError(t, model.DB.Delete(&model.Token{Id: tokenID}).Error)
	require.NoError(t, model.DB.Delete(&model.User{Id: userID}).Error)
	require.NoError(t, model.DB.Delete(&model.Channel{Id: channelID}).Error)

	task.Status = model.TaskStatusFailure
	won, err := SettleMeasuredVideoTask(t.Context(), task, model.TaskStatusInProgress, 0, nil, nil)
	require.NoError(t, err)
	assert.True(t, won)
	var user model.User
	require.NoError(t, model.DB.Unscoped().First(&user, userID).Error)
	assert.Equal(t, 10_000, user.Quota)
	assert.Zero(t, user.UsedQuota)
	var token model.Token
	require.NoError(t, model.DB.Unscoped().First(&token, tokenID).Error)
	assert.Equal(t, 10_000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), persisted.Status)
	assert.Zero(t, persisted.Quota)
}

func TestMeasuredVideoZeroPriceStillRecordsAndSettlesOnce(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	const userID, channelID = 75, 75
	seedUser(t, userID, 10_000)
	seedChannel(t, channelID)
	expression := `tier("free", u("seconds") * 0)`
	task := makeTask(userID, channelID, 0, 0, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString: expression, ExprHash: billingexpr.ExprHashString(expression),
		GroupRatio: 1, QuotaPerUnit: 1_000, ExprVersion: 1, TaskUsageBilling: true,
		UsageFacts: map[string]any{"seconds": float64(5)},
	}
	require.NoError(t, model.DB.Create(task).Error)
	recorded, err := model.RecordMeasuredTaskSubmitAccounting(t.Context(), task)
	require.NoError(t, err)
	assert.True(t, recorded)
	recorded, err = model.RecordMeasuredTaskSubmitAccounting(t.Context(), task)
	require.NoError(t, err)
	assert.False(t, recorded)
	task.Status = model.TaskStatusSuccess
	won, err := SettleMeasuredVideoTask(t.Context(), task, model.TaskStatusInProgress, 0, map[string]any{"seconds": 3.25}, nil)
	require.NoError(t, err)
	assert.True(t, won)
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), persisted.Status)
	assert.Equal(t, 3.25, persisted.PrivateData.BillingContext.TieredSnapshot.UsageFacts["seconds"])
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Zero(t, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Equal(t, 10_000, getUserQuota(t, userID))
	assert.Zero(t, getChannelUsedQuota(t, channelID))
	assert.Zero(t, countLogs(t))
}

func TestMeasuredVideoRefundFailureRollsBackTerminalStateAndBalances(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	const userID, tokenID, channelID = 76, 76, 76
	const reservedQuota = 3_000
	seedUser(t, userID, 7_000)
	seedToken(t, tokenID, userID, "sk-measured-rollback", 7_000)
	seedChannel(t, channelID)
	expression := `tier("seconds", u("seconds"))`
	task := makeTask(userID, channelID, reservedQuota, tokenID, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString: expression, ExprHash: billingexpr.ExprHashString(expression),
		GroupRatio: 1, QuotaPerUnit: 1_000, ExprVersion: 1, TaskUsageBilling: true,
		UsageFacts: map[string]any{"seconds": float64(3)},
	}
	require.NoError(t, model.DB.Create(task).Error)
	_, err := model.RecordMeasuredTaskSubmitAccounting(t.Context(), task)
	require.NoError(t, err)
	// The token has no recorded debit: its refund must abort the entire
	// transaction, including the earlier wallet refund and terminal CAS.
	task.Status = model.TaskStatusFailure
	won, err := SettleMeasuredVideoTask(t.Context(), task, model.TaskStatusInProgress, 0, nil, nil)
	require.ErrorContains(t, err, "token refund could not be applied")
	assert.False(t, won)
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), persisted.Status)
	assert.Equal(t, reservedQuota, persisted.Quota)
	assert.Equal(t, 7_000, getUserQuota(t, userID))
	assert.Equal(t, 7_000, getTokenRemainQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Equal(t, reservedQuota, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Equal(t, int64(reservedQuota), getChannelUsedQuota(t, channelID))
	assert.Zero(t, countLogs(t))

	// After repairing the missing debit, the persisted task is still eligible
	// for one complete refund on the next poll.
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", tokenID).Update("used_quota", reservedQuota).Error)
	won, err = SettleMeasuredVideoTask(t.Context(), task, model.TaskStatusInProgress, 0, nil, nil)
	require.NoError(t, err)
	assert.True(t, won)
	assert.Equal(t, 10_000, getUserQuota(t, userID))
	assert.Equal(t, 10_000, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, int64(1), countLogs(t))
}

func TestCASGuardedSettle_Win(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 22, 22, 22
	const initQuota, preConsumed = 10000, 5000
	const actualQuota = 3000 // over-charged, should get partial refund
	const tokenRemain = 8000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-cas-settle-win", tokenRemain)
	seedChannel(t, channelID)
	seedChargedAccounting(t, userID, channelID, tokenID, preConsumed, 1)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	require.NoError(t, model.DB.Create(task).Error)

	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusSuccess), actualQuota)

	// CAS wins: task should be SUCCESS
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusSuccess, reloaded.Status)

	// Settlement should refund the over-charge (5000 - 3000 = 2000 back to user)
	assert.Equal(t, initQuota+(preConsumed-actualQuota), getUserQuota(t, userID))
	assert.Equal(t, tokenRemain+(preConsumed-actualQuota), getTokenRemainQuota(t, tokenID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Equal(t, actualQuota, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Equal(t, int64(actualQuota), getChannelUsedQuota(t, channelID))

	// task.Quota should be updated to actualQuota
	assert.Equal(t, actualQuota, task.Quota)
}

func TestNonTerminalUpdate_NoBilling(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, channelID = 23, 23
	const initQuota, preConsumed = 10000, 3000

	seedUser(t, userID, initQuota)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, 0, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	task.Progress = "20%"
	require.NoError(t, model.DB.Create(task).Error)

	// Simulate a non-terminal poll update (still IN_PROGRESS, progress changed)
	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusInProgress), 0)

	// User quota should NOT change
	assert.Equal(t, initQuota, getUserQuota(t, userID))

	// No billing log
	assert.Equal(t, int64(0), countLogs(t))

	// Task progress should be updated in DB
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, "50%", reloaded.Progress)
}

// ===========================================================================
// Mock adaptor for settleTaskBillingOnComplete tests
// ===========================================================================

type mockAdaptor struct {
	adjustReturn int
}

func (m *mockAdaptor) Init(_ *relaycommon.RelayInfo) {}
func (m *mockAdaptor) FetchTask(string, string, *model.Task, string) (*http.Response, error) {
	return nil, nil
}
func (m *mockAdaptor) ParseTaskResult(*model.Task, *http.Response, []byte) (*relaycommon.TaskInfo, error) {
	return nil, nil
}
func (m *mockAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int {
	return m.adjustReturn
}

// ===========================================================================
// PerCallBilling tests — settleTaskBillingOnComplete
// ===========================================================================

func TestTaskBillingTerminalSettlement(t *testing.T) {
	oldRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString("{\"test-model\":1}"))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(oldRatios)) })
	for _, tc := range []struct {
		name                                                                      string
		expression                                                                string
		facts                                                                     map[string]any
		reserved, wantQuota                                                       int
		perCall, subscription, insufficient, failed, legacy, removePrice, expired bool
		adjust, totalTokens, completionTokens                                     int
	}{
		{name: "seconds increase", expression: "tier(\"base\", u(\"seconds\") * 10)", facts: map[string]any{"seconds": float64(8)}, reserved: 50, wantQuota: 80},
		{name: "seconds decrease", expression: "tier(\"base\", u(\"seconds\") * 10)", facts: map[string]any{"seconds": float64(3)}, reserved: 50, wantQuota: 30},
		{name: "constant per task ignores measured usage", expression: "tier(\"request\", 50)", facts: map[string]any{"seconds": float64(80)}, reserved: 50, wantQuota: 50},
		{name: "explicit zero usage refunds", expression: "tier(\"base\", u(\"seconds\") * 10)", facts: map[string]any{"seconds": float64(0)}, reserved: 50},
		{name: "zero price task", expression: "tier(\"free\", 0)", reserved: 0},
		{name: "legacy per call skips adaptor and tokens", perCall: true, adjust: 1, totalTokens: 80, reserved: 50, wantQuota: 50},
		{name: "adaptor adjustment", adjust: 30, reserved: 50, wantQuota: 30},
		{name: "invalid expression retains reservation", expression: "tier(\"broken\",", reserved: 50, wantQuota: 50},
		{name: "missing facts retain submission", expression: "tier(\"base\", u(\"seconds\") + u(\"clips\") * 10)", reserved: 50, wantQuota: 25},
		{name: "partial facts preserve other usage", expression: "tier(\"base\", u(\"seconds\") + u(\"clips\") * 10)", facts: map[string]any{"seconds": float64(8)}, reserved: 50, wantQuota: 28},
		{name: "full facts override submission", expression: "tier(\"base\", u(\"seconds\") + u(\"clips\") * 10)", facts: map[string]any{"seconds": float64(8), "clips": float64(3)}, reserved: 50, wantQuota: 38},
		{name: "completion changes tier", expression: "u(\"resolution\") == \"1080P\" ? tier(\"1080P\", u(\"seconds\") * 10) : tier(\"720P\", u(\"seconds\") * 5)", facts: map[string]any{"resolution": "1080P"}, reserved: 25, wantQuota: 50},
		{name: "total tokens take priority", totalTokens: 80, completionTokens: 20, reserved: 50, wantQuota: 80},
		{name: "completion tokens fallback", completionTokens: 80, reserved: 50, wantQuota: 80},
		{name: "frozen token price survives deleted live configuration", totalTokens: 80, reserved: 50, wantQuota: 80, removePrice: true},
		{name: "missing tokens keep reservation", reserved: 50, wantQuota: 50},
		{name: "subscription shortfall fails and refunds", subscription: true, insufficient: true, expression: "tier(\"base\", u(\"seconds\") * 10)", facts: map[string]any{"seconds": float64(8)}, reserved: 50},
		{name: "expired subscription cannot fund extra usage", subscription: true, expired: true, expression: "tier(\"base\", u(\"seconds\") * 10)", facts: map[string]any{"seconds": float64(8)}, reserved: 50},
		{name: "wallet shortfall fails and refunds", insufficient: true, expression: "tier(\"base\", u(\"seconds\") * 10)", facts: map[string]any{"seconds": float64(8)}, reserved: 50},
		{name: "upstream failure refunds", failed: true, expression: "tier(\"base\", u(\"seconds\") * 10)", reserved: 50},
		{name: "legacy task retains existing submit accounting", legacy: true, expression: "tier(\"base\", u(\"seconds\") * 10)", facts: map[string]any{"seconds": float64(3)}, reserved: 50, wantQuota: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupMeasuredTaskBillingDB(t)
			const id = 981
			initialWallet := 10000
			if tc.insufficient && !tc.subscription {
				initialWallet = 10
			}
			seedUser(t, id, initialWallet)
			seedToken(t, id, id, "sk-terminal-settlement", 8000)
			seedChannel(t, id)
			seedChargedAccounting(t, id, id, id, tc.reserved, 1)
			source := BillingSourceWallet
			if tc.subscription {
				source = BillingSourceSubscription
				limit := int64(100)
				if tc.insufficient {
					limit = 60
				}
				seedSubscription(t, id, id, limit, int64(tc.reserved))
				if tc.expired {
					require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", id).Update("end_time", time.Now().Unix()-1).Error)
				}
			}
			task := makeTask(id, id, tc.reserved, id, source, id)
			task.PrivateData.DurableBilling = !tc.legacy
			task.PrivateData.SubmitAccountingRecorded = !tc.legacy
			task.PrivateData.BillingContext.PerCallBilling = tc.perCall
			task.PrivateData.BillingContext.ModelRatio = 1
			task.PrivateData.BillingContext.GroupRatio = 1
			if tc.removePrice {
				previous := ratio_setting.ModelRatio2JSONString()
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString("{}"))
				t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previous)) })
				task.Group = ""
			}
			if tc.expression != "" {
				task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
					ExprString: tc.expression, ExprHash: billingexpr.ExprHashString(tc.expression), GroupRatio: 1, QuotaPerUnit: 1, ExprVersion: 1, TaskUsageBilling: true,
					UsageFacts: map[string]any{"seconds": float64(5), "clips": float64(2), "resolution": "720P"}, EstimatedTier: "base",
				}
			}
			require.NoError(t, model.DB.Create(task).Error)
			require.NoError(t, ensureMeasuredTaskSubmitAccounting(t.Context(), task))
			stale := *task
			task.Status = model.TaskStatusSuccess
			task.Progress = "100%"
			task.PrivateData.ResultURL = "https://cdn.example/unpaid.mp4"
			task.Data = []byte("{\"url\":\"https://cdn.example/unpaid.mp4\"}")
			if tc.failed {
				task.Status = model.TaskStatusFailure
			}
			won, err := SettleTaskBillingOnComplete(t.Context(), &mockAdaptor{adjustReturn: tc.adjust}, task, model.TaskStatusInProgress, &relaycommon.TaskInfo{UsageFacts: tc.facts, TotalTokens: tc.totalTokens, CompletionTokens: tc.completionTokens})
			require.NoError(t, err)
			require.True(t, won)
			var saved model.Task
			require.NoError(t, model.DB.First(&saved, task.ID).Error)
			assert.Equal(t, tc.wantQuota, saved.Quota)
			if tc.insufficient || tc.expired {
				assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), saved.Status)
				assert.Contains(t, saved.FailReason, "insufficient quota")
				assert.Empty(t, saved.Data)
				assert.Empty(t, saved.PrivateData.ResultURL)
			} else if tc.failed {
				assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), saved.Status)
			} else {
				assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), saved.Status)
			}
			wantWallet := initialWallet + tc.reserved - tc.wantQuota
			if tc.subscription {
				wantWallet = initialWallet
				assert.Equal(t, int64(tc.wantQuota), getSubscriptionUsed(t, id))
			}
			assert.Equal(t, wantWallet, getUserQuota(t, id))
			assert.Equal(t, 8000+tc.reserved-tc.wantQuota, getTokenRemainQuota(t, id))
			used, requests := getUserUsageAccounting(t, id)
			assert.Equal(t, tc.wantQuota, used)
			assert.Equal(t, 1, requests)
			assert.Equal(t, int64(tc.wantQuota), getChannelUsedQuota(t, id))
			if tc.name == "completion changes tier" {
				assert.Equal(t, "1080P", saved.PrivateData.BillingContext.TieredSnapshot.EstimatedTier)
			}
			stale.Status = model.TaskStatusSuccess
			won, err = SettleTaskBillingOnComplete(t.Context(), &mockAdaptor{adjustReturn: tc.adjust}, &stale, model.TaskStatusInProgress, &relaycommon.TaskInfo{UsageFacts: tc.facts, TotalTokens: tc.totalTokens, CompletionTokens: tc.completionTokens})
			require.NoError(t, err)
			assert.False(t, won)
			assert.Equal(t, wantWallet, getUserQuota(t, id))
		})
	}
}

func TestTaskRequestBillingDatabaseSettlement(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	savedConfig := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		savedConfig[key] = value
		return nil
	}))
	t.Cleanup(func() { require.NoError(t, config.GlobalConfig.LoadFromDB(savedConfig)) })
	const id, initialQuota = 984, 10000
	const tokenKey = "sk-frozen-request-billing"
	const expression = `tier("seconds", u("seconds") * 10) * (param("priority") == true ? 2 : 1) * (header("X-Video-Tier") == "fast" ? 3 : 1) * (hour("UTC") == 3 ? 4 : 1)`
	rawExprs, err := common.Marshal(map[string]string{"test-video::test-model": expression})
	require.NoError(t, err)
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{billing_setting.PluginBillingExprOption: string(rawExprs)}))
	resolvedExpr, configured := billing_setting.ResolveTaskBillingExpr("test-video", "test-model", "test-model")
	require.True(t, configured)
	request := billingexpr.RequestInput{
		Body:    []byte(`{"priority":true,"prompt":"private-video-prompt"}`),
		Headers: map[string]string{"X-Video-Tier": "fast", "Authorization": "Bearer private-token"},
		Usage:   map[string]any{"seconds": float64(5)},
	}
	frozenRequest, err := billingexpr.FreezeTaskRequestInput(resolvedExpr, request, time.Date(2000, time.January, 2, 3, 4, 0, 0, time.UTC))
	require.NoError(t, err)
	request.Frozen = frozenRequest
	cost, trace, err := billingexpr.RunExprWithRequest(resolvedExpr, billingexpr.TokenParams{}, request)
	require.NoError(t, err)
	reservedQuota := common.QuotaRound(cost)
	require.Equal(t, 1200, reservedQuota)
	seedUser(t, id, initialQuota)
	seedToken(t, id, id, tokenKey, initialQuota)
	seedChannel(t, id)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Set("token_quota", initialQuota)
	info := &relaycommon.RelayInfo{
		UserId: id, TokenId: id, TokenKey: tokenKey, UserGroup: "default", UsingGroup: "default", OriginModelName: "test-model",
		TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: "text_to_video"},
		ChannelMeta:   &relaycommon.ChannelMeta{ChannelId: id}, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"},
		PriceData: types.PriceData{Quota: reservedQuota, QuotaToPreConsume: reservedQuota, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}},
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode: "tiered_expr", ExprString: resolvedExpr, ExprHash: billingexpr.ExprHashString(resolvedExpr),
			GroupRatio: 1, QuotaPerUnit: 1, ExprVersion: 1, TaskUsageBilling: true, UsageFacts: request.Usage,
			EstimatedTier: trace.MatchedTier, EstimatedQuotaAfterGroup: reservedQuota, TaskRequest: frozenRequest,
		},
	}
	require.Nil(t, PreConsumeDurableTaskBilling(c, reservedQuota, info))
	assert.Equal(t, initialQuota-reservedQuota, getUserQuota(t, id))
	assert.Equal(t, initialQuota-reservedQuota, getTokenRemainQuota(t, id))
	task := makeTask(id, id, reservedQuota, id, BillingSourceWallet, 0)
	task.PrivateData.DurableBilling = true
	task.PrivateData.BillingContext.TieredSnapshot = info.TieredBillingSnapshot
	require.NoError(t, model.DB.Create(task).Error)
	require.NoError(t, LogTaskConsumption(c, info, task))
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	require.NotNil(t, persisted.PrivateData.BillingContext.TieredSnapshot.TaskRequest)
	assert.Equal(t, frozenRequest, persisted.PrivateData.BillingContext.TieredSnapshot.TaskRequest)

	request.Body = []byte(`{"priority":false}`)
	request.Headers["X-Video-Tier"] = "standard"
	rawExprs, err = common.Marshal(map[string]string{"test-video::test-model": `tier("changed", 9999)`})
	require.NoError(t, err)
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{billing_setting.PluginBillingExprOption: string(rawExprs)}))
	persisted.Status = model.TaskStatusSuccess
	won, err := SettleTaskBillingOnComplete(t.Context(), &mockAdaptor{}, &persisted, model.TaskStatusInProgress, &relaycommon.TaskInfo{UsageFacts: map[string]any{"seconds": float64(3)}})
	require.NoError(t, err)
	require.True(t, won)
	var completed model.Task
	require.NoError(t, model.DB.First(&completed, task.ID).Error)
	assert.Equal(t, 720, completed.Quota)
	assert.Equal(t, initialQuota-720, getUserQuota(t, id))
	assert.Equal(t, initialQuota-720, getTokenRemainQuota(t, id))
	used, requests := getUserUsageAccounting(t, id)
	assert.Equal(t, 720, used)
	assert.Equal(t, 1, requests)
	assert.Equal(t, int64(720), getChannelUsedQuota(t, id))
	assert.Equal(t, frozenRequest, completed.PrivateData.BillingContext.TieredSnapshot.TaskRequest)
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Find(&logs).Error)
	require.Len(t, logs, 2)
	for _, log := range logs {
		encoded, err := common.Marshal(log)
		require.NoError(t, err)
		for _, private := range []string{"private-video-prompt", "private-token", "task_request", "x-video-tier"} {
			assert.NotContains(t, string(encoded), private)
		}
	}
}

// The real database commits, but the caller loses the acknowledgement. This
// protects the financial boundary without relying on a network timing race.
type taskBillingCommitErrorPool struct{ gorm.ConnPool }

func (pool taskBillingCommitErrorPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := pool.ConnPool.(gorm.TxBeginner).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &taskBillingCommitErrorTx{tx}, nil
}

type taskBillingCommitErrorTx struct{ *sql.Tx }

func (tx taskBillingCommitErrorTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	return assert.AnError
}

func TestTaskBillingCacheReservationRollback(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		wallet, token                       int
		databaseFailure                     bool
		walletDBShortfall, tokenDBShortfall bool
		commitUncertain                     bool
		reserveOnly                         string
	}{
		{name: "pending wallet batch is respected", wallet: 20, token: 100},
		{name: "token rejection compensates wallet reservation", wallet: 100, token: 20},
		{name: "database rollback compensates both caches", wallet: 100, token: 100, databaseFailure: true},
		{name: "stale wallet cache cannot overdraw database", wallet: 100, token: 100, walletDBShortfall: true},
		{name: "stale token cache cannot overdraw database", wallet: 100, token: 100, tokenDBShortfall: true},
		{name: "uncertain settlement commit retains cache deductions", wallet: 100, token: 100, commitUncertain: true},
		{name: "uncertain durable wallet reservation retains cache deduction", wallet: 100, token: 100, commitUncertain: true, reserveOnly: "wallet"},
		{name: "uncertain durable token reservation retains cache deduction", wallet: 100, token: 100, commitUncertain: true, reserveOnly: "token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupMeasuredTaskBillingDB(t)
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			previousRedis, previousEnabled, previousBatch := common.RDB, common.RedisEnabled, common.BatchUpdateEnabled
			common.RDB, common.RedisEnabled, common.BatchUpdateEnabled = client, true, true
			t.Cleanup(func() {
				common.RDB, common.RedisEnabled, common.BatchUpdateEnabled = previousRedis, previousEnabled, previousBatch
				require.NoError(t, client.Close())
			})
			const id = 983
			initialWallet, initialToken := 100, 100
			if tc.walletDBShortfall {
				initialWallet = 20
			}
			if tc.tokenDBShortfall {
				initialToken = 20
			}
			seedUser(t, id, initialWallet)
			seedToken(t, id, id, "sk-cache-settlement", initialToken)
			seedChannel(t, id)
			seedChargedAccounting(t, id, id, id, 50, 1)
			_, err := model.GetUserCache(id)
			require.NoError(t, err)
			require.NoError(t, client.HSet(t.Context(), "user:983", "Quota", tc.wallet).Err())
			tokenCacheKey := "token:" + common.GenerateHMAC("sk-cache-settlement")
			require.NoError(t, client.HSet(t.Context(), tokenCacheKey, map[string]any{"Id": id, "UserId": id, "RemainQuota": tc.token, "UsedQuota": 50, "Status": common.TokenStatusEnabled}).Err())
			task := makeTask(id, id, 50, id, BillingSourceWallet, 0)
			task.PrivateData.DurableBilling = true
			task.PrivateData.SubmitAccountingRecorded = true
			expression := "tier(\"seconds\", u(\"seconds\") * 10)"
			task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), GroupRatio: 1, QuotaPerUnit: 1, ExprVersion: 1, TaskUsageBilling: true, UsageFacts: map[string]any{"seconds": float64(5)}}
			require.NoError(t, model.DB.Create(task).Error)
			originalDB := model.DB
			if tc.commitUncertain {
				injectedDB := originalDB.Session(&gorm.Session{Context: t.Context()})
				pool := &taskBillingCommitErrorPool{originalDB.ConnPool}
				injectedDB.ConnPool, injectedDB.Statement.ConnPool = pool, pool
				model.DB = injectedDB
				t.Cleanup(func() { model.DB = originalDB })
			}
			if tc.reserveOnly != "" {
				var reserved bool
				var reserveErr error
				wantWallet, wantToken := 100, 100
				if tc.reserveOnly == "wallet" {
					reserved, reserveErr = model.TryReserveUserQuotaDurable(id, 30)
					wantWallet = 70
				} else {
					reserved, reserveErr = model.TryReserveTokenQuotaDurable(id, "sk-cache-settlement", 30, false)
					wantToken = 70
				}
				model.DB = originalDB
				require.ErrorIs(t, reserveErr, assert.AnError)
				assert.False(t, reserved, "uncertain reservation cannot authorize upstream dispatch")
				assert.Equal(t, wantWallet, getUserQuota(t, id))
				assert.Equal(t, wantToken, getTokenRemainQuota(t, id))
				wallet, err := client.HGet(t.Context(), "user:983", "Quota").Int()
				require.NoError(t, err)
				assert.Equal(t, wantWallet, wallet)
				token, err := client.HGet(t.Context(), tokenCacheKey, "RemainQuota").Int()
				require.NoError(t, err)
				assert.Equal(t, wantToken, token)
				return
			}
			if tc.databaseFailure {
				require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register("test:billing_claim_failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "tasks" {
						tx.AddError(assert.AnError)
					}
				}))
			}
			task.Status = model.TaskStatusSuccess
			won, err := SettleTaskBillingOnComplete(t.Context(), &mockAdaptor{}, task, model.TaskStatusInProgress, &relaycommon.TaskInfo{UsageFacts: map[string]any{"seconds": float64(8)}})
			model.DB = originalDB
			if tc.commitUncertain {
				require.ErrorIs(t, err, assert.AnError)
				assert.False(t, won)
				var saved model.Task
				require.NoError(t, model.DB.First(&saved, task.ID).Error)
				assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), saved.Status)
				assert.Equal(t, 80, saved.Quota)
				won, err = SettleTaskBillingOnComplete(t.Context(), &mockAdaptor{}, task, model.TaskStatusInProgress, &relaycommon.TaskInfo{UsageFacts: map[string]any{"seconds": float64(8)}})
				require.NoError(t, err)
				assert.False(t, won)
				assert.Equal(t, 70, getUserQuota(t, id))
				assert.Equal(t, 70, getTokenRemainQuota(t, id))
				assert.Equal(t, "70", client.HGet(t.Context(), "user:983", "Quota").Val())
				assert.Equal(t, "70", client.HGet(t.Context(), tokenCacheKey, "RemainQuota").Val())
			} else if tc.databaseFailure {
				require.ErrorIs(t, err, assert.AnError)
				assert.False(t, won)
				require.NoError(t, model.DB.Callback().Update().Remove("test:billing_claim_failure"))
				assert.Equal(t, 100, getUserQuota(t, id))
				assert.Equal(t, 100, getTokenRemainQuota(t, id))
				assert.Equal(t, "100", client.HGet(t.Context(), "user:983", "Quota").Val())
				assert.Equal(t, "100", client.HGet(t.Context(), tokenCacheKey, "RemainQuota").Val())
				var pending model.Task
				require.NoError(t, model.DB.First(&pending, task.ID).Error)
				assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), pending.Status)
				pending.Status = model.TaskStatusSuccess
				won, err = SettleTaskBillingOnComplete(t.Context(), &mockAdaptor{}, &pending, model.TaskStatusInProgress, &relaycommon.TaskInfo{UsageFacts: map[string]any{"seconds": float64(8)}})
				require.NoError(t, err)
				assert.True(t, won)
				assert.Equal(t, 70, getUserQuota(t, id))
				assert.Equal(t, 70, getTokenRemainQuota(t, id))
				assert.Equal(t, "70", client.HGet(t.Context(), "user:983", "Quota").Val())
				assert.Equal(t, "70", client.HGet(t.Context(), tokenCacheKey, "RemainQuota").Val())
			} else {
				require.NoError(t, err)
				assert.True(t, won)
				assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), task.Status)
				assert.Equal(t, initialWallet+50, getUserQuota(t, id))
				assert.Equal(t, initialToken+50, getTokenRemainQuota(t, id))
				wallet, err := client.HGet(t.Context(), "user:983", "Quota").Int()
				require.NoError(t, err)
				assert.Equal(t, tc.wallet+50, wallet)
				token, err := client.HGet(t.Context(), tokenCacheKey, "RemainQuota").Int()
				require.NoError(t, err)
				assert.Equal(t, tc.token+50, token)
			}
		})
	}
}

func TestTaskBillingSubscriptionPeriodRefund(t *testing.T) {
	for _, tc := range []struct {
		name           string
		legacy, failed bool
		newUsage       int64
	}{
		{name: "current task partial refund"},
		{name: "legacy task partial refund", legacy: true},
		{name: "failure full refund", failed: true},
		{name: "new period consumption is preserved", newUsage: 700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupMeasuredTaskBillingDB(t)
			const id = 982
			seedUser(t, id, 10000)
			seedToken(t, id, id, "sk-period-refund", 6000)
			seedChannel(t, id)
			seedSubscription(t, id, id, 10000, 4000)
			seedChargedAccounting(t, id, id, id, 4000, 1)
			require.NoError(t, model.DB.AutoMigrate(&model.SubscriptionPlan{}))
			plan := model.SubscriptionPlan{Id: id, QuotaResetPeriod: model.SubscriptionResetCustom, QuotaResetCustomSeconds: 3600}
			require.NoError(t, model.DB.Create(&plan).Error)
			t.Cleanup(func() { require.NoError(t, model.DB.Delete(&plan).Error) })
			now := time.Now().Unix()
			period := now - 3601
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", id).Updates(map[string]any{"plan_id": id, "start_time": period, "last_reset_time": period, "next_reset_time": now - 1}).Error)
			task := makeTask(id, id, 4000, id, BillingSourceSubscription, id)
			task.SubmitTime = now - 10
			task.PrivateData.SubmitAccountingRecorded = true
			task.PrivateData.DurableBilling = !tc.legacy
			if !tc.legacy {
				task.PrivateData.SubscriptionPeriodStart = &period
			}
			expression := "tier(\"seconds\", u(\"seconds\"))"
			task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), GroupRatio: 1, QuotaPerUnit: 1000, ExprVersion: 1, TaskUsageBilling: true, UsageFacts: map[string]any{"seconds": float64(4)}}
			require.NoError(t, model.DB.Create(task).Error)
			count, err := model.ResetDueSubscriptions(10)
			require.NoError(t, err)
			require.Equal(t, 1, count)
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", id).Update("amount_used", tc.newUsage).Error)
			task.Status = model.TaskStatusSuccess
			actual := 2000
			if tc.failed {
				task.Status = model.TaskStatusFailure
				actual = 0
			}
			won, err := SettleMeasuredVideoTask(t.Context(), task, model.TaskStatusInProgress, actual, map[string]any{"seconds": float64(2)}, nil)
			require.NoError(t, err)
			require.True(t, won)
			assert.Equal(t, tc.newUsage, getSubscriptionUsed(t, id))
			assert.Equal(t, 6000+4000-actual, getTokenRemainQuota(t, id))
			var saved model.Task
			require.NoError(t, model.DB.First(&saved, task.ID).Error)
			assert.Equal(t, task.Status, saved.Status)
			assert.Equal(t, actual, saved.Quota)
		})
	}
}

func TestMixedVideoPublicBillingContract(t *testing.T) {
	for _, tc := range []struct {
		name, expression string
		wantError        bool
	}{
		{name: "request price", expression: `tier("request", u("requests") * 0.5)`},
		{name: "duration and conditions", expression: `u("resolution") == "1080p" && u("action") == "generate" ? tier("hd", u("seconds") * 0.2) : tier("base", u("requests") * 0.3)`},
		{name: "reference video multipliers", expression: `tier("seconds", u("seconds") * 0.6) * (u("video_input") == "video" ? 1.45 : 1)`},
		{name: "negative reference price forbidden", expression: `u("video_input") == "video" ? tier("video", -1) : tier("none", 1)`, wantError: true},
		{name: "negative no reference price forbidden", expression: `u("video_input") == "none" ? tier("none", -1) : tier("video", 1)`, wantError: true},
		{name: "infinite reference price forbidden", expression: `u("video_input") == "video" ? tier("video", 1e308 * 1e308) : tier("none", 1)`, wantError: true},
		{name: "provider tokens forbidden", expression: `tier("tokens", u("tokens"))`, wantError: true},
		{name: "unreachable provider tokens forbidden", expression: `true ? tier("request", u("requests")) : tier("tokens", u("tokens"))`, wantError: true},
		{name: "dynamic usage forbidden", expression: `tier("dynamic", u("sec" + "onds"))`, wantError: true},
		{name: "request probes forbidden", expression: `tier("body", param("seconds"))`, wantError: true},
		{name: "token variables forbidden", expression: `tier("tokens", p * 2)`, wantError: true},
		{name: "negative price forbidden", expression: `tier("negative", -1)`, wantError: true},
		{name: "all resolutions validated", expression: `u("resolution") == "1080p" ? tier("negative", -1) : tier("normal", 1)`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := billing_setting.ValidateMixedVideoBillingExpr(tc.expression)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}

	settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	previous := settings.MixedVideoBillingExpr
	t.Cleanup(func() { settings.MixedVideoBillingExpr = previous })
	settings.MixedVideoBillingExpr = map[string]string{"public-video": `tier("seconds", u("seconds") * 0.25)`}
	facts := map[string]any{"requests": float64(1), "seconds": float64(4), "resolution": "720p", "action": "generate"}
	frozen, err := FreezeMixedVideoBilling("public-video", facts, types.GroupRatioInfo{GroupRatio: 1})
	require.NoError(t, err)
	require.NoError(t, ValidateFrozenMixedVideoBilling(frozen.Tiered), "historical snapshots without video_input remain valid")
	assert.True(t, frozen.Tiered.MixedVideoBilling)
	assert.Equal(t, billing_setting.MixedVideoBillingSchemaVersion, frozen.Tiered.MixedVideoSchemaVersion)
	assert.Equal(t, common.QuotaRound(common.QuotaPerUnit), frozen.Price.Quota)
	facts["seconds"] = float64(99)
	settings.MixedVideoBillingExpr["public-video"] = `tier("changed", u("requests") * 99)`
	result, usage, err := EvaluateTaskCompletionUsage(frozen.Tiered, map[string]any{"seconds": float64(1), "tokens": float64(999999), "requests": float64(99)})
	require.NoError(t, err)
	assert.Equal(t, frozen.Price.Quota, result.ActualQuotaAfterGroup)
	assert.Equal(t, float64(4), usage["seconds"])
	assert.NotContains(t, usage, "tokens")

	encoded, err := common.Marshal(frozen.Tiered)
	require.NoError(t, err)
	var restored billingexpr.BillingSnapshot
	require.NoError(t, common.Unmarshal(encoded, &restored))
	result, _, err = EvaluateTaskCompletionUsage(&restored, map[string]any{"seconds": float64(0)})
	require.NoError(t, err)
	assert.Equal(t, frozen.Price.Quota, result.ActualQuotaAfterGroup)
	restored.MixedVideoSchemaVersion++
	_, _, err = EvaluateTaskCompletionUsage(&restored, nil)
	require.Error(t, err)

	_, err = FreezeMixedVideoBilling("unpriced-video", facts, types.GroupRatioInfo{GroupRatio: 1})
	require.ErrorContains(t, err, "no public billing expression")
	settings.MixedVideoBillingExpr["public-video"] = `tier("request", u("requests") * 0.5)`
	requestFacts := map[string]any{"requests": float64(1), "action": "edit"}
	_, err = FreezeMixedVideoBilling("public-video", requestFacts, types.GroupRatioInfo{GroupRatio: 1})
	require.NoError(t, err)
	settings.MixedVideoBillingExpr["public-video"] = `tier("seconds", u("seconds"))`
	_, err = FreezeMixedVideoBilling("public-video", requestFacts, types.GroupRatioInfo{GroupRatio: 1})
	require.ErrorContains(t, err, "cannot be determined")
	for _, seconds := range []float64{0, -1, 0.5, relaycommon.MaxTaskDurationSeconds + 1, math.Inf(1), math.NaN()} {
		requestFacts["seconds"] = seconds
		_, err = FreezeMixedVideoBilling("public-video", requestFacts, types.GroupRatioInfo{GroupRatio: 1})
		require.Error(t, err)
	}
	settings.MixedVideoBillingExpr["public-video"] = `tier("oversized", u("requests") * 1e20)`
	delete(requestFacts, "seconds")
	saturated, err := FreezeMixedVideoBilling("public-video", requestFacts, types.GroupRatioInfo{GroupRatio: 1})
	require.NoError(t, err)
	assert.Equal(t, common.MaxQuota, saturated.Price.Quota)
	assert.NotNil(t, saturated.Clamp)
}

func TestMixedVideoModelPricingInheritance(t *testing.T) {
	settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	previous := *settings
	t.Cleanup(func() { *settings = previous })
	const modelExpression = `tier("seconds", u("seconds") * 0.8) * (u("video_input") == "video" ? 1.45 : 1)`
	facts := map[string]any{"requests": float64(1), "seconds": float64(30), "resolution": "720p", "action": "generate", "video_input": "none"}
	for _, tc := range []struct {
		name       string
		mode       string
		expression string
		overrides  map[string]string
		videoInput string
		wantUSD    float64
		wantError  bool
	}{
		{name: "inherits model price", mode: "tiered_expr", expression: modelExpression, wantUSD: 24},
		{name: "inherits reference video multiplier once", mode: "tiered_expr", expression: modelExpression, videoInput: "video", wantUSD: 34.8},
		{name: "explicit mixed price wins", mode: "tiered_expr", expression: modelExpression, overrides: map[string]string{"public-video": `tier("custom", u("requests") * 2)`}, wantUSD: 2},
		{name: "explicit zero stays free", mode: "tiered_expr", expression: modelExpression, overrides: map[string]string{"public-video": `tier("free", u("requests") * 0)`}},
		{name: "invalid override does not inherit", mode: "tiered_expr", expression: modelExpression, overrides: map[string]string{"public-video": ""}, wantError: true},
		{name: "inactive model expression is unavailable", mode: "ratio", expression: modelExpression, wantError: true},
		{name: "missing model price does not use provider override", wantError: true},
		{name: "provider tokens are incompatible", mode: "tiered_expr", expression: `tier("tokens", u("tokens") * 0.1)`, wantError: true},
		{name: "request probes are incompatible", mode: "tiered_expr", expression: `tier("s", u("seconds")) * (header("x-fast") == "1" ? 2 : 1)`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings.BillingMode = map[string]string{"public-video": tc.mode}
			settings.BillingExpr = map[string]string{"public-video": tc.expression}
			settings.PluginBillingExpr = map[string]string{"drama::public-video": `tier("provider", u("requests") * 99)`}
			settings.MixedVideoBillingExpr = tc.overrides
			facts["video_input"] = "none"
			if tc.videoInput != "" {
				facts["video_input"] = tc.videoInput
			}
			frozen, err := FreezeMixedVideoBilling("public-video", facts, types.GroupRatioInfo{GroupRatio: 2})
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, common.QuotaRound(tc.wantUSD*common.QuotaPerUnit*2), frozen.Price.Quota)
			require.NoError(t, ValidateFrozenMixedVideoBilling(frozen.Tiered))
			settings.BillingExpr["public-video"] = `tier("changed", u("requests") * 999)`
			settings.MixedVideoBillingExpr = map[string]string{"public-video": `tier("changed", u("requests") * 888)`}
			result, _, err := EvaluateTaskCompletionUsage(frozen.Tiered, map[string]any{"seconds": float64(1), "video_input": "video"})
			require.NoError(t, err)
			assert.Equal(t, frozen.Price.Quota, result.ActualQuotaAfterGroup, "settlement retains the accepted price after either configuration changes")
		})
	}
	settings.MixedVideoBillingExpr = nil
	settings.BillingExpr["public-video"] = `tier("new", u("requests") * 3)`
	updated, err := FreezeMixedVideoBilling("public-video", facts, types.GroupRatioInfo{GroupRatio: 1})
	require.NoError(t, err)
	assert.Equal(t, common.QuotaRound(3*common.QuotaPerUnit), updated.Price.Quota)
}

func TestMixedVideoReferenceInputPricing(t *testing.T) {
	settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	previous := settings.MixedVideoBillingExpr
	t.Cleanup(func() { settings.MixedVideoBillingExpr = previous })
	const expression = `(u("resolution") == "1080p" ? tier("1080p", u("seconds") * 1.1) : tier("720p", u("seconds") * 0.6)) * (u("video_input") == "video" ? 1.45 : 1)`
	t.Run("provider specific video input cannot bypass public billing", func(t *testing.T) {
		binding := jsplugin.ProtocolBinding{Model: "public-video", Plugin: &jsplugin.LoadedPlugin{Meta: jsplugin.Meta{
			Models: []string{"public-video"},
			VideoProfiles: []jsplugin.VideoProfile{{Models: []string{"public-video"}, Modes: []jsplugin.VideoModeProfile{{
				Name: "reference_to_video", Inputs: []jsplugin.VideoInputProfile{{Name: "clip", Kind: "video", Sources: []string{"url"}, MaxItems: 1}},
			}}}},
		}}}
		_, err := EffectiveMixedVideoProfile(binding, "public-video", VideoModelOverrides{})
		require.ErrorContains(t, err, "must use video or reference_videos")
	})
	for _, tc := range []struct {
		name      string
		body      string
		operation string
		upload    string
		wantVideo bool
	}{
		{name: "text only", body: `{}`},
		{name: "images and audio only", body: `{"reference_images":["https://cdn.example/image.png"],"reference_audios":["https://cdn.example/audio.mp3"]}`},
		{name: "reference video URL", body: `{"reference_videos":["https://cdn.example/video.mp4"]}`, wantVideo: true},
		{name: "canonical reference", body: `{"references":[{"type":"video","source":"https://cdn.example/video.mp4"}]}`, wantVideo: true},
		{name: "content video", body: `{"content":[{"type":"video_url","video_url":{"url":"https://cdn.example/video.mp4"}}]}`, wantVideo: true},
		{name: "metadata video", body: `{"metadata":{"content":[{"type":"video_url","role":"reference_video","video_url":{"url":"https://cdn.example/video.mp4"}}]}}`, wantVideo: true},
		{name: "edit source video", body: `{"video":"https://cdn.example/video.mp4"}`, operation: "edit", wantVideo: true},
		{name: "extend source video", body: `{"video":{"url":"https://cdn.example/video.mp4"}}`, operation: "extend", wantVideo: true},
		{name: "empty references", body: `{"reference_videos":[]}`},
		{name: "spoofed no video", body: `{"video_input":"none","reference_videos":["https://cdn.example/video.mp4"]}`, wantVideo: true},
		{name: "spoofed with video", body: `{"video_input":"video","metadata":{"video_input":"video"}}`},
		{name: "uploaded image", body: `{}`, upload: "input_reference"},
		{name: "uploaded reference video", body: `{"video_input":"none"}`, upload: "reference_videos", wantVideo: true},
		{name: "uploaded primary video", body: `{}`, upload: "video", wantVideo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings.MixedVideoBillingExpr = map[string]string{"public-video": expression}
			body := map[string]any{"model": "public-video", "seconds": float64(5), "resolution": "720p"}
			require.NoError(t, common.UnmarshalJsonStr(tc.body, &body))
			raw, err := common.Marshal(body)
			require.NoError(t, err)
			contentType := "application/json"
			if tc.upload != "" {
				var data bytes.Buffer
				writer := multipart.NewWriter(&data)
				for field, value := range body {
					text, err := common.Marshal(value)
					require.NoError(t, err)
					if literal, ok := value.(string); ok {
						text = []byte(literal)
					}
					require.NoError(t, writer.WriteField(field, string(text)))
				}
				file, err := writer.CreateFormFile(tc.upload, "reference.bin")
				require.NoError(t, err)
				_, err = file.Write([]byte("bounded-upload-fixture"))
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				raw, contentType = data.Bytes(), writer.FormDataContentType()
			}
			view, body, err := MixedVideoRequestContext(raw, contentType, tc.operation)
			require.NoError(t, err)
			facts, err := MixedVideoPublicFacts(body, tc.operation, view.Files)
			require.NoError(t, err)
			wantInput, wantCost := "none", 3.0
			if tc.wantVideo {
				wantInput, wantCost = "video", 4.35
			}
			assert.Equal(t, wantInput, facts["video_input"])
			frozen, err := FreezeMixedVideoBilling("public-video", facts, types.GroupRatioInfo{GroupRatio: 2})
			require.NoError(t, err)
			assert.Equal(t, common.QuotaRound(wantCost*common.QuotaPerUnit*2), frozen.Price.Quota)
			facts["video_input"] = "changed"
			settings.MixedVideoBillingExpr["public-video"] = `tier("changed", u("requests") * 999)`
			encoded, err := common.Marshal(frozen.Tiered)
			require.NoError(t, err)
			var restored billingexpr.BillingSnapshot
			require.NoError(t, common.Unmarshal(encoded, &restored))
			require.NoError(t, ValidateFrozenMixedVideoBilling(&restored))
			result, usage, err := EvaluateTaskCompletionUsage(&restored, map[string]any{"video_input": "none", "seconds": float64(1)})
			require.NoError(t, err)
			assert.Equal(t, frozen.Price.Quota, result.ActualQuotaAfterGroup)
			assert.Equal(t, wantInput, usage["video_input"], "provider usage cannot replace frozen input facts")
		})
	}
	facts := map[string]any{"requests": float64(1), "seconds": float64(5), "resolution": "720p", "action": "generate"}
	require.ErrorContains(t, billing_setting.ValidateMixedVideoBillingFacts(expression, facts), "cannot be determined")
	facts["video_input"] = "invalid"
	require.ErrorContains(t, billing_setting.ValidateMixedVideoBillingFacts(expression, facts), "invalid mixed video billing fact")
	settings.MixedVideoBillingExpr["public-video"] = `tier("seconds", u("seconds") * 0.6) * (u("video_input") == "video" ? 1.45 : 0)`
	facts["video_input"] = "none"
	free, err := FreezeMixedVideoBilling("public-video", facts, types.GroupRatioInfo{GroupRatio: 1})
	require.NoError(t, err)
	assert.Zero(t, free.Price.Quota)
	assert.True(t, free.Price.FreeModel)
}

func TestMixedVideoSettlementPreservesRequestPriceAndRefundsOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		measured   bool
		failed     bool
		videoInput string
		inherited  bool
	}{
		{name: "normal completion"},
		{name: "measured channel completion", measured: true},
		{name: "terminal failure", failed: true},
		{name: "no reference video", videoInput: "none"},
		{name: "reference video completion", videoInput: "video"},
		{name: "reference video measured completion", videoInput: "video", measured: true},
		{name: "reference video failure", videoInput: "video", failed: true},
		{name: "inherited model completion", videoInput: "none", inherited: true},
		{name: "inherited reference video measured completion", videoInput: "video", measured: true, inherited: true},
		{name: "inherited reference video failure", videoInput: "video", failed: true, inherited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupMeasuredTaskBillingDB(t)
			const id = 981
			reserved := 4000
			if tc.videoInput == "video" {
				reserved = 5800
			}
			seedUser(t, id, 10000-reserved)
			seedToken(t, id, id, "sk-mixed-public-billing", 10000-reserved)
			seedChannel(t, id)
			seedChargedAccounting(t, id, id, id, reserved, 1)
			task := makeTask(id, id, reserved, id, BillingSourceWallet, 0)
			task.PrivateData.DurableBilling = true
			task.PrivateData.SubmitAccountingRecorded = true
			expression := `tier("seconds", u("seconds"))`
			facts := map[string]any{"requests": float64(1), "seconds": float64(4), "action": "generate"}
			if tc.videoInput != "" {
				expression += ` * (u("video_input") == "video" ? 1.45 : 1)`
				facts["video_input"] = tc.videoInput
			}
			task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
				ModelName: "public-video", ExprString: expression, ExprHash: billingexpr.ExprHashString(expression),
				GroupRatio: 1, QuotaPerUnit: 1000, ExprVersion: 1, TaskUsageBilling: true,
				EstimatedQuotaAfterGroup: reserved,
				MixedVideoBilling:        true, MixedVideoSchemaVersion: billing_setting.MixedVideoBillingSchemaVersion,
				UsageFacts: facts,
			}
			if tc.inherited {
				settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
				previous, previousUnit := *settings, common.QuotaPerUnit
				t.Cleanup(func() { *settings, common.QuotaPerUnit = previous, previousUnit })
				settings.MixedVideoBillingExpr = nil
				settings.BillingMode = map[string]string{"public-video": billing_setting.BillingModeTieredExpr}
				settings.BillingExpr = map[string]string{"public-video": expression}
				common.QuotaPerUnit = 1000
				frozen, err := FreezeMixedVideoBilling("public-video", facts, types.GroupRatioInfo{GroupRatio: 1})
				require.NoError(t, err)
				require.Equal(t, reserved, frozen.Price.Quota)
				task.PrivateData.BillingContext.TieredSnapshot = frozen.Tiered
				settings.BillingExpr["public-video"] = `tier("changed", u("requests") * 99)`
			}
			require.NoError(t, model.DB.Create(task).Error)
			require.NoError(t, model.DB.First(task, task.ID).Error)
			stale := *task
			task.Status = model.TaskStatusSuccess
			if tc.failed {
				task.Status = model.TaskStatusFailure
			}
			var won bool
			var err error
			if tc.measured {
				won, err = SettleMeasuredVideoTask(t.Context(), task, model.TaskStatusInProgress, 2000, map[string]any{"seconds": float64(2), "video_input": "none"}, nil)
			} else {
				won, err = SettleTaskBillingOnComplete(t.Context(), nil, task, model.TaskStatusInProgress, &relaycommon.TaskInfo{UsageFacts: map[string]any{"seconds": float64(2), "video_input": "none"}})
			}
			require.NoError(t, err)
			require.True(t, won)
			wantQuota, wantBalance := reserved, 10000-reserved
			if tc.failed {
				wantQuota, wantBalance = 0, 10000
			}
			assert.Equal(t, wantQuota, task.Quota)
			assert.Equal(t, wantBalance, getUserQuota(t, id))
			assert.Equal(t, wantBalance, getTokenRemainQuota(t, id))
			assert.Equal(t, float64(4), task.PrivateData.BillingContext.TieredSnapshot.UsageFacts["seconds"])
			stale.Status = task.Status
			won, err = SettleTaskBillingOnComplete(t.Context(), nil, &stale, model.TaskStatusInProgress, &relaycommon.TaskInfo{UsageFacts: map[string]any{"seconds": float64(999)}})
			require.NoError(t, err)
			assert.False(t, won)
			assert.Equal(t, wantBalance, getUserQuota(t, id))
		})
	}
}

func TestMixedVideoFreeReservationDoesNotChargeSubscription(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	const id = 980
	seedUser(t, id, 0)
	seedToken(t, id, id, "sk-mixed-free-billing", 0)
	seedChannel(t, id)
	seedSubscription(t, id, id, 10000, 0)
	settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	previous := settings.MixedVideoBillingExpr
	t.Cleanup(func() { settings.MixedVideoBillingExpr = previous })
	settings.MixedVideoBillingExpr = map[string]string{"free-public-video": `tier("free", u("requests") * 0)`}
	frozen, err := FreezeMixedVideoBilling("free-public-video", map[string]any{"requests": float64(1), "action": "extend"}, types.GroupRatioInfo{GroupRatio: 1})
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	info := &relaycommon.RelayInfo{UserId: id, TokenId: id, TokenKey: "sk-mixed-free-billing", TieredBillingSnapshot: frozen.Tiered, PriceData: frozen.Price}
	info.UserSetting.BillingPreference = "subscription_only"
	require.Nil(t, PreConsumeDurableTaskBilling(c, 0, info))
	assert.True(t, HasDurableTaskReservation(info, 0))
	task := makeTask(id, id, 0, id, info.BillingSource, info.SubscriptionId)
	task.PrivateData.BillingContext.TieredSnapshot = frozen.Tiered
	SnapshotTaskReservation(task, info)
	require.True(t, task.PrivateData.DurableBilling)
	require.NoError(t, model.DB.Create(task).Error)
	_, err = model.RecordMeasuredTaskSubmitAccounting(t.Context(), task)
	require.NoError(t, err)
	task.Status = model.TaskStatusSuccess
	won, err := SettleTaskBillingOnComplete(t.Context(), nil, task, model.TaskStatusInProgress, &relaycommon.TaskInfo{UsageFacts: map[string]any{"seconds": float64(999)}})
	require.NoError(t, err)
	assert.True(t, won)
	assert.Zero(t, getUserQuota(t, id))
	assert.Zero(t, getTokenRemainQuota(t, id))
	assert.Zero(t, getSubscriptionUsed(t, id))
	assert.Zero(t, task.Quota)
}

// Commit succeeds in the real database, but exactly one acknowledgement is
// lost. Counting commits selects funding, token, or refund without timing races.
type reservationCommitErrorPool struct {
	gorm.ConnPool
	commits int
	failAt  int
}

// Ordinary wallet debt reservations update their cache in a goroutine. Wait
// for that specific debit before checking balances or restoring the client.
type reservationCacheDebitHook struct {
	key, delta string
	applied    chan struct{}
}

func (h *reservationCacheDebitHook) BeforeProcess(ctx context.Context, _ redis.Cmder) (context.Context, error) {
	return ctx, nil
}

func (h *reservationCacheDebitHook) AfterProcess(_ context.Context, command redis.Cmder) error {
	args := command.Args()
	if command.Name() == "eval" && len(args) > 4 && fmt.Sprint(args[3]) == h.key && fmt.Sprint(args[4]) == h.delta {
		select {
		case h.applied <- struct{}{}:
		default:
		}
	}
	return nil
}

func (h *reservationCacheDebitHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}

func (h *reservationCacheDebitHook) AfterProcessPipeline(context.Context, []redis.Cmder) error {
	return nil
}

func (pool *reservationCommitErrorPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := pool.ConnPool.(gorm.TxBeginner).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &reservationCommitErrorTx{Tx: tx, pool: pool}, nil
}

type reservationCommitErrorTx struct {
	*sql.Tx
	pool *reservationCommitErrorPool
}

func (tx *reservationCommitErrorTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	tx.pool.commits++
	if tx.pool.commits == tx.pool.failAt {
		return assert.AnError
	}
	return nil
}

func TestBillingReservationFailureRefundDatabase(t *testing.T) {
	for _, funding := range []string{BillingSourceWallet, BillingSourceSubscription} {
		for _, durable := range []bool{false, true} {
			for _, scenario := range []string{"success", "settle_success", "token_short", "zero_initial", "refund_transient", "refund_persistent", "token_refund_failure", "funding_commit_unknown", "token_commit_unknown", "refund_commit_unknown", "token_refund_commit_unknown", "period_reset", "funding_settle_unknown", "token_settle_unknown"} {
				if scenario == "period_reset" && funding != BillingSourceSubscription {
					continue
				}
				if scenario == "zero_initial" && funding != BillingSourceWallet {
					continue
				}
				t.Run(fmt.Sprintf("%s/durable_%t/%s", funding, durable, scenario), func(t *testing.T) {
					setupMeasuredTaskBillingDB(t)
					db := model.DB
					require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}, &model.SubscriptionPreConsumeRecord{}, &model.CanvasGrant{}, &model.AsyncImageUsageProjection{}))
					require.NoError(t, model.LOG_DB.AutoMigrate(&model.AsyncImageLogReceipt{}))
					// Initialize the same token cache that request authentication
					// primes, including for real atomic insufficient-quota refusal.
					server := miniredis.RunT(t)
					client := redis.NewClient(&redis.Options{Addr: server.Addr()})
					oldRedis, oldEnabled, oldBatch := common.RDB, common.RedisEnabled, common.BatchUpdateEnabled
					common.RDB, common.RedisEnabled, common.BatchUpdateEnabled = client, true, false
					t.Cleanup(func() {
						common.RDB, common.RedisEnabled, common.BatchUpdateEnabled = oldRedis, oldEnabled, oldBatch
						require.NoError(t, client.Close())
					})
					const id = 9982
					initialQuota := 40
					if scenario == "zero_initial" {
						initialQuota = 0
					}
					initialToken := 50
					if scenario == "success" || scenario == "settle_success" || scenario == "token_commit_unknown" {
						initialToken = 100
					}
					seedUser(t, id, 1000)
					seedToken(t, id, id, "sk-reservation-regression", initialToken)
					seedChannel(t, id)
					_, err := model.GetUserCache(id)
					require.NoError(t, err)
					require.NoError(t, client.HSet(t.Context(), "token:"+common.GenerateHMAC("sk-reservation-regression"), map[string]any{"Id": id, "UserId": id, "RemainQuota": initialToken, "UsedQuota": 0, "Status": common.TokenStatusEnabled}).Err())
					var subscription model.UserSubscription
					if funding == BillingSourceSubscription {
						plan := model.SubscriptionPlan{Title: "Reservation regression", QuotaResetPeriod: model.SubscriptionResetNever}
						require.NoError(t, db.Create(&plan).Error)
						t.Cleanup(func() { require.NoError(t, db.Delete(&model.SubscriptionPlan{}, plan.Id).Error) })
						subscription = model.UserSubscription{UserId: id, PlanId: plan.Id, AmountTotal: 200, Status: "active", EndTime: time.Now().Unix() + 3600, LastResetTime: time.Now().Unix()}
						require.NoError(t, db.Create(&subscription).Error)
					}
					requestID := common.GetUUID()
					t.Cleanup(func() {
						require.NoError(t, db.Where("request_id = ?", requestID).Delete(&model.SubscriptionPreConsumeRecord{}).Error)
					})
					job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: id, TokenId: id, ChannelId: id, Status: "submitting", BillingStatus: "reserved", Quota: initialQuota, LeaseToken: "reservation-regression"}
					require.NoError(t, db.Create(&job).Error)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
					// Run refund synchronously while exercising both durable
					// and ordinary funding/token mutation implementations.
					c.Set("async_media_execution", true)
					c.Set("async_media_execution_lease_token", job.LeaseToken)
					info := &relaycommon.RelayInfo{UserId: id, TokenId: id, TokenKey: "sk-reservation-regression", ChannelMeta: &relaycommon.ChannelMeta{ChannelId: id}, RequestId: requestID, ForcePreConsume: true, UserSetting: dto.UserSetting{BillingPreference: funding + "_only"}, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: job.TaskId}}
					if durable {
						require.Nil(t, PreConsumeDurableTaskBilling(c, initialQuota, info))
					} else {
						require.Nil(t, PreConsumeBilling(c, initialQuota, info))
					}
					session := info.Billing.(*BillingSession)
					if scenario == "funding_settle_unknown" || scenario == "token_settle_unknown" {
						failAt, stage, wantToken := 1, "funding_settle", initialToken-40
						if scenario == "token_settle_unknown" {
							failAt, stage, wantToken = 2, "token_settle", initialToken-30
						}
						injected := db.Session(&gorm.Session{Context: t.Context()})
						pool := &reservationCommitErrorPool{ConnPool: db.ConnPool, failAt: failAt}
						injected.ConnPool, injected.Statement.ConnPool = pool, pool
						model.DB = injected
						settleErr := session.Settle(30)
						model.DB = db
						require.ErrorIs(t, settleErr, assert.AnError)
						require.ErrorIs(t, session.Settle(30), assert.AnError)
						require.ErrorIs(t, session.Settle(40), assert.AnError)
						require.ErrorIs(t, session.Reserve(40), assert.AnError)
						assert.False(t, HasDurableTaskReservation(info, 0))
						evidence := session.ReservationFailure()
						require.NotNil(t, evidence)
						assert.True(t, evidence.Uncertain)
						assert.Equal(t, stage, evidence.Stage)
						assert.Equal(t, 30, evidence.AttemptedQuota)
						session.Refund(c)
						session.Refund(c)
						if funding == BillingSourceWallet {
							assert.Equal(t, 970, getUserQuota(t, id))
						} else {
							assert.EqualValues(t, 30, getSubscriptionUsed(t, subscription.Id))
						}
						assert.Equal(t, wantToken, getTokenRemainQuota(t, id), "uncertain settlement must not retry or refund either accounting side")
						cachedWallet, err := client.HGet(t.Context(), "user:9982", "Quota").Int()
						require.NoError(t, err)
						cachedToken, err := client.HGet(t.Context(), "token:"+common.GenerateHMAC("sk-reservation-regression"), "RemainQuota").Int()
						require.NoError(t, err)
						assert.LessOrEqual(t, cachedWallet, getUserQuota(t, id))
						assert.LessOrEqual(t, cachedToken, wantToken, "an uncertain credit must not release spendable token quota")
						require.NoError(t, db.First(&job, job.ID).Error)
						assert.Equal(t, "failed", job.BillingStatus)
						assert.False(t, session.refunded)
						var refundCount int64
						require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", id, model.LogTypeRefund).Count(&refundCount).Error)
						assert.Zero(t, refundCount)
						return
					}
					if scenario == "period_reset" {
						require.NoError(t, db.Model(&subscription).Updates(map[string]any{"amount_used": 0, "last_reset_time": subscription.LastResetTime + 1}).Error)
					}
					if scenario == "funding_commit_unknown" || scenario == "token_commit_unknown" {
						failAt := 1
						if scenario == "token_commit_unknown" {
							failAt = 2
						}
						injected := db.Session(&gorm.Session{Context: t.Context()})
						pool := &reservationCommitErrorPool{ConnPool: db.ConnPool, failAt: failAt}
						injected.ConnPool, injected.Statement.ConnPool = pool, pool
						model.DB = injected
					}
					var cacheDebit *reservationCacheDebitHook
					if funding == BillingSourceWallet && !durable {
						cacheDebit = &reservationCacheDebitHook{key: "user:9982", delta: fmt.Sprint(initialQuota - 60), applied: make(chan struct{}, 1)}
						client.AddHook(cacheDebit)
					}
					reserveErr := session.Reserve(60)
					model.DB = db
					if cacheDebit != nil {
						select {
						case <-cacheDebit.applied:
						case <-time.After(5 * time.Second):
							t.Fatal("ordinary wallet reservation cache debit did not finish")
						}
					}
					if scenario == "success" || scenario == "settle_success" {
						require.NoError(t, reserveErr)
						require.NoError(t, session.Reserve(60))
						assert.Nil(t, session.ReservationFailure())
						if scenario == "settle_success" {
							require.NoError(t, session.Settle(50))
							require.NoError(t, session.Settle(50))
							session.Refund(c)
							assert.Equal(t, 50, getTokenRemainQuota(t, id))
							if funding == BillingSourceWallet {
								assert.Equal(t, 950, getUserQuota(t, id))
							} else {
								assert.EqualValues(t, 50, getSubscriptionUsed(t, subscription.Id))
							}
							return
						}
					} else {
						require.Error(t, reserveErr)
						require.Error(t, session.Reserve(40), "even a smaller target must not hide a failed reservation")
						require.Error(t, session.Settle(40), "zero-delta settlement must not hide a failed reservation")
						assert.False(t, HasDurableTaskReservation(info, 0))
						evidence := session.ReservationFailure()
						require.NotNil(t, evidence)
						assert.Equal(t, initialQuota, evidence.PairedQuota)
						assert.Equal(t, initialQuota, evidence.ConfirmedTokenQuota)
						assert.Equal(t, 60, evidence.AttemptedQuota)
						assert.Equal(t, scenario == "funding_commit_unknown" || scenario == "token_commit_unknown", evidence.Uncertain)
					}
					refundAttempts := 0
					if scenario == "refund_transient" || scenario == "refund_persistent" || scenario == "token_refund_failure" {
						require.NoError(t, db.Callback().Update().Before("gorm:update").Register("reservation:refund-failure", func(tx *gorm.DB) {
							updates, ok := tx.Statement.Dest.(map[string]any)
							if !ok || tx.Statement.Schema == nil {
								return
							}
							refund := tx.Statement.Schema.Name == "UserSubscription" && updates["amount_used"] == int64(0)
							if expr, ok := updates["quota"].(clause.Expr); ok && tx.Statement.Schema.Name == "User" && expr.SQL == "quota + ?" {
								refund = true
							}
							if scenario == "token_refund_failure" {
								expr, ok := updates["remain_quota"].(clause.Expr)
								refund = ok && tx.Statement.Schema.Name == "Token" && expr.SQL == "remain_quota + ?"
							}
							if refund {
								refundAttempts++
								if scenario == "refund_persistent" || refundAttempts == 1 {
									tx.AddError(assert.AnError)
								}
							}
						}))
						t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove("reservation:refund-failure")) })
					}
					if scenario == "refund_commit_unknown" || scenario == "token_refund_commit_unknown" {
						injected := db.Session(&gorm.Session{Context: t.Context()})
						pool := &reservationCommitErrorPool{ConnPool: db.ConnPool, failAt: 1}
						if scenario == "token_refund_commit_unknown" {
							pool.failAt = 2
						}
						injected.ConnPool, injected.Statement.ConnPool = pool, pool
						model.DB = injected
					}
					session.Refund(c)
					model.DB = db
					session.Refund(c)
					require.Error(t, session.Reserve(40))
					require.Error(t, session.Settle(40))
					require.NoError(t, db.First(&job, job.ID).Error)
					wantStatus := "refunded"
					wantWallet, wantSubscription, wantToken := 1000, int64(0), initialToken
					switch scenario {
					case "refund_transient":
						if funding == BillingSourceWallet {
							wantStatus, wantWallet = "failed", 940
						}
					case "refund_persistent":
						wantStatus = "failed"
						if funding == BillingSourceWallet {
							wantWallet = 940
						} else {
							wantSubscription = 60
						}
					case "token_refund_failure":
						wantStatus, wantToken = "failed", initialToken-40
					case "funding_commit_unknown":
						wantStatus = "failed"
						if funding == BillingSourceWallet {
							wantWallet = 980
						}
					case "token_commit_unknown":
						wantStatus, wantToken = "failed", 80
					case "refund_commit_unknown":
						if funding == BillingSourceWallet {
							wantStatus = "failed"
						}
					case "token_refund_commit_unknown":
						wantStatus = "failed"
					}
					assert.Equal(t, wantWallet, getUserQuota(t, id))
					assert.Equal(t, wantToken, getTokenRemainQuota(t, id))
					cachedWallet, err := client.HGet(t.Context(), "user:9982", "Quota").Int()
					require.NoError(t, err)
					cachedToken, err := client.HGet(t.Context(), "token:"+common.GenerateHMAC("sk-reservation-regression"), "RemainQuota").Int()
					require.NoError(t, err)
					assert.LessOrEqual(t, cachedWallet, wantWallet, "failed or uncertain writes cannot release unbacked wallet quota")
					assert.LessOrEqual(t, cachedToken, wantToken, "failed or uncertain writes cannot release unbacked token quota")
					if funding == BillingSourceSubscription {
						assert.Equal(t, wantSubscription, getSubscriptionUsed(t, subscription.Id))
					}
					assert.Equal(t, wantStatus, job.BillingStatus)
					assert.Equal(t, wantStatus == "refunded", session.refunded)
					if scenario == "refund_transient" || scenario == "refund_persistent" {
						if funding == BillingSourceWallet {
							assert.Equal(t, 1, refundAttempts, "wallet refunds must never retry a possibly committed credit")
						} else if scenario == "refund_transient" {
							assert.Equal(t, 2, refundAttempts)
						} else {
							assert.Equal(t, 3, refundAttempts)
						}
					}
					if scenario == "token_refund_failure" {
						assert.Equal(t, 1, refundAttempts, "token credits must not be blindly retried")
					}
					var refunds []model.Log
					require.NoError(t, model.LOG_DB.Where("user_id = ? AND type = ?", id, model.LogTypeRefund).Find(&refunds).Error)
					if wantStatus == "refunded" {
						require.Len(t, refunds, 1)
						expectedQuota := 60
						if scenario == "period_reset" {
							expectedQuota = 40
						}
						assert.Equal(t, expectedQuota, refunds[0].Quota, "refund audit uses the complete confirmed funding reservation")
					} else {
						assert.Empty(t, refunds, "uncertain or failed refunds must not manufacture a completed refund log")
					}
				})
			}
		}
	}
}
