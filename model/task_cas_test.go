package model

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic("failed to open test db: " + err.Error())
	}
	DB = db
	LOG_DB = db

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = true
	initCol()

	sqlDB, err := db.DB()
	if err != nil {
		panic("failed to get sql.DB: " + err.Error())
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(
		&Task{},
		&AsyncMediaJob{},
		&AsyncMediaEvent{},
		&User{},
		&UserSession{},
		&AuthFlow{},
		&ExternalIdentityClaim{},
		&Token{},
		&PasskeyCredential{},
		&TwoFA{},
		&TwoFABackupCode{},
		&Log{},
		&Channel{},
		&QuotaData{},
		&Ability{},
		&TopUp{},
		&InvoiceRequest{},
		&InvoiceRequestItem{},
		&InvoiceOrderClaim{},
		&SubscriptionPlan{},
		&SubscriptionOrder{},
		&UserSubscription{},
		&UserOAuthBinding{},
		&PerfMetric{},
		&SystemInstance{},
		&SystemTask{},
		&SystemTaskLock{},
	); err != nil {
		panic("failed to migrate: " + err.Error())
	}

	os.Exit(m.Run())
}

func truncateTables(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		DB.Exec("DELETE FROM tasks")
		DB.Exec("DELETE FROM auth_flows")
		DB.Exec("DELETE FROM external_identity_claims")
		DB.Exec("DELETE FROM user_sessions")
		DB.Exec("DELETE FROM passkey_credentials")
		DB.Exec("DELETE FROM two_fa_backup_codes")
		DB.Exec("DELETE FROM two_fas")
		DB.Exec("DELETE FROM tokens")
		DB.Exec("DELETE FROM user_oauth_bindings")
		DB.Exec("DELETE FROM users")
		DB.Exec("DELETE FROM logs")
		DB.Exec("DELETE FROM channels")
		DB.Exec("DELETE FROM quota_data")
		DB.Exec("DELETE FROM abilities")
		DB.Exec("DELETE FROM top_ups")
		DB.Exec("DELETE FROM invoice_order_claims")
		DB.Exec("DELETE FROM invoice_request_items")
		DB.Exec("DELETE FROM invoice_requests")
		DB.Exec("DELETE FROM subscription_orders")
		DB.Exec("DELETE FROM subscription_plans")
		DB.Exec("DELETE FROM user_subscriptions")
		DB.Exec("DELETE FROM perf_metrics")
		DB.Exec("DELETE FROM system_instances")
		DB.Exec("DELETE FROM system_task_locks")
		DB.Exec("DELETE FROM system_tasks")
	})
}

func insertTask(t *testing.T, task *Task) {
	t.Helper()
	task.CreatedAt = time.Now().Unix()
	task.UpdatedAt = time.Now().Unix()
	require.NoError(t, DB.Create(task).Error)
}

func TestGetTaskForProtocolObservationScopesOwnerAndPlatform(t *testing.T) {
	truncateTables(t)
	task := &Task{
		TaskID:   "task_protocol_scope",
		UserId:   7,
		Platform: "plugin-a",
		Status:   TaskStatusInProgress,
	}
	insertTask(t, task)

	got, exists, err := GetTaskForProtocolObservation(context.Background(), 7, "plugin-a", task.TaskID)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, task.ID, got.ID)

	for _, query := range []struct {
		userID   int
		platform string
	}{
		{userID: 8, platform: "plugin-a"},
		{userID: 7, platform: "plugin-b"},
	} {
		got, exists, err = GetTaskForProtocolObservation(context.Background(), query.userID, constant.TaskPlatform(query.platform), task.TaskID)
		require.NoError(t, err)
		assert.False(t, exists)
		assert.Nil(t, got)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = GetTaskForProtocolObservation(cancelled, 7, "plugin-a", task.TaskID)
	require.ErrorIs(t, err, context.Canceled)
}

// ---------------------------------------------------------------------------
// Snapshot / Equal — pure logic tests (no DB)
// ---------------------------------------------------------------------------

func TestSnapshotEqual_Same(t *testing.T) {
	s := taskSnapshot{
		Status:     TaskStatusInProgress,
		Progress:   "50%",
		StartTime:  1000,
		FinishTime: 0,
		FailReason: "",
		ResultURL:  "",
		Data:       json.RawMessage(`{"key":"value"}`),
	}
	assert.True(t, s.Equal(s))
}

func TestSnapshotEqual_DifferentStatus(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{}`)}
	b := taskSnapshot{Status: TaskStatusSuccess, Data: json.RawMessage(`{}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_DifferentProgress(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Progress: "30%", Data: json.RawMessage(`{}`)}
	b := taskSnapshot{Status: TaskStatusInProgress, Progress: "60%", Data: json.RawMessage(`{}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_DifferentData(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{"a":1}`)}
	b := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{"a":2}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_NilVsEmpty(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: nil}
	b := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage{}}
	// bytes.Equal(nil, []byte{}) == true
	assert.True(t, a.Equal(b))
}

func TestSnapshotEqual_PluginStateAndPollFailures(t *testing.T) {
	base := taskSnapshot{
		Status:       TaskStatusInProgress,
		PluginState:  json.RawMessage(`{"req_key":"a"}`),
		PollFailures: 2,
	}
	assert.True(t, base.Equal(taskSnapshot{
		Status:       TaskStatusInProgress,
		PluginState:  json.RawMessage(`{"req_key":"a"}`),
		PollFailures: 2,
	}))
	assert.False(t, base.Equal(taskSnapshot{
		Status:       TaskStatusInProgress,
		PluginState:  json.RawMessage(`{"req_key":"b"}`),
		PollFailures: 2,
	}))
	assert.False(t, base.Equal(taskSnapshot{
		Status:       TaskStatusInProgress,
		PluginState:  json.RawMessage(`{"req_key":"a"}`),
		PollFailures: 3,
	}))
}

func TestSnapshot_Roundtrip(t *testing.T) {
	task := &Task{
		Status:     TaskStatusInProgress,
		Progress:   "42%",
		StartTime:  1234,
		FinishTime: 5678,
		FailReason: "timeout",
		PrivateData: TaskPrivateData{
			ResultURL:    "https://example.com/result.mp4",
			PluginState:  json.RawMessage(`{"req_key":"keep"}`),
			PollFailures: 3,
		},
		Data: json.RawMessage(`{"model":"test-model"}`),
	}
	snap := task.Snapshot()
	assert.Equal(t, task.Status, snap.Status)
	assert.Equal(t, task.Progress, snap.Progress)
	assert.Equal(t, task.StartTime, snap.StartTime)
	assert.Equal(t, task.FinishTime, snap.FinishTime)
	assert.Equal(t, task.FailReason, snap.FailReason)
	assert.Equal(t, task.PrivateData.ResultURL, snap.ResultURL)
	assert.JSONEq(t, string(task.Data), string(snap.Data))
	assert.Equal(t, task.PrivateData.PluginState, snap.PluginState)
	assert.Equal(t, task.PrivateData.PollFailures, snap.PollFailures)
}

// ---------------------------------------------------------------------------
// UpdateWithStatus CAS — DB integration tests
// ---------------------------------------------------------------------------

func TestUpdateWithStatus_Win(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID:   "task_cas_win",
		Status:   TaskStatusInProgress,
		Progress: "50%",
		Data:     json.RawMessage(`{}`),
	}
	insertTask(t, task)

	task.Status = TaskStatusSuccess
	task.Progress = "100%"
	won, err := task.UpdateWithStatus(TaskStatusInProgress)
	require.NoError(t, err)
	assert.True(t, won)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, TaskStatusSuccess, reloaded.Status)
	assert.Equal(t, "100%", reloaded.Progress)
}

func TestUpdateWithStatus_Lose(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_cas_lose",
		Status: TaskStatusFailure,
		Data:   json.RawMessage(`{}`),
	}
	insertTask(t, task)

	task.Status = TaskStatusSuccess
	won, err := task.UpdateWithStatus(TaskStatusInProgress) // wrong fromStatus
	require.NoError(t, err)
	assert.False(t, won)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, TaskStatusFailure, reloaded.Status) // unchanged
}

func TestUpdateWithStatus_ConcurrentWinner(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_cas_race",
		Status: TaskStatusInProgress,
		Quota:  1000,
		Data:   json.RawMessage(`{}`),
	}
	insertTask(t, task)

	const goroutines = 5
	wins := make([]bool, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			t := &Task{}
			*t = Task{
				ID:       task.ID,
				TaskID:   task.TaskID,
				Status:   TaskStatusSuccess,
				Progress: "100%",
				Quota:    task.Quota,
				Data:     json.RawMessage(`{}`),
			}
			t.CreatedAt = task.CreatedAt
			t.UpdatedAt = time.Now().Unix()
			won, err := t.UpdateWithStatus(TaskStatusInProgress)
			if err == nil {
				wins[idx] = won
			}
		}(i)
	}
	wg.Wait()

	winCount := 0
	for _, w := range wins {
		if w {
			winCount++
		}
	}
	assert.Equal(t, 1, winCount, "exactly one goroutine should win the CAS")
}

func TestUpdateWithStatus_PersistsPluginStateAndPollFailures(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_cas_plugin_state",
		Status: TaskStatusInProgress,
		Data:   json.RawMessage(`{}`),
		PrivateData: TaskPrivateData{
			PluginState:  json.RawMessage(`{"req_key":"old"}`),
			PollFailures: 1,
		},
	}
	insertTask(t, task)

	task.PrivateData.PluginState = json.RawMessage(`{"req_key":"new"}`)
	task.PrivateData.PollFailures = 4
	won, err := task.UpdateWithStatus(TaskStatusInProgress)
	require.NoError(t, err)
	require.True(t, won)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, TaskStatusInProgress, reloaded.Status)
	assert.JSONEq(t, `{"req_key":"new"}`, string(reloaded.PrivateData.PluginState))
	assert.Equal(t, 4, reloaded.PrivateData.PollFailures)
}

func asyncVideoRecoveryFixture(t *testing.T) (*gorm.DB, Task, AsyncMediaJob) {
	t.Helper()
	db, image, _ := imageDatabaseFixture(t)
	require.NoError(t, db.AutoMigrate(&Task{}, &AsyncMediaJob{}, &AsyncMediaEvent{}, &MediaInputObject{}))
	require.NoError(t, db.Model(&User{}).Where("id = ?", image.UserId).Updates(map[string]any{"quota": 900, "used_quota": 100, "request_count": 1}).Error)
	require.NoError(t, db.Model(&Token{}).Where("id = ?", image.TokenId).Updates(map[string]any{"remain_quota": 900, "used_quota": 100}).Error)
	require.NoError(t, db.Model(&Channel{}).Where("id = ?", image.ChannelId).Update("used_quota", 100).Error)
	now := time.Now().Unix()
	task := Task{TaskID: "task_video_recovery", Platform: "drama", UserId: image.UserId, ChannelId: image.ChannelId, Quota: 100, Status: TaskStatusInProgress, SubmitTime: now - 10, StartTime: now - 5, Progress: "40%", Data: json.RawMessage(`{"status":"IN_PROGRESS"}`),
		Properties:  Properties{OriginModelName: "video-model", UpstreamModelName: "native-video-model"},
		PrivateData: TaskPrivateData{AsyncMedia: true, UpstreamTaskID: "native-original", TokenId: image.TokenId, BillingSource: "wallet", DurableBilling: true, SubmitAccountingRecorded: true, VideoTerminalConfirmed: true, PluginState: json.RawMessage(`{"native_id":"original"}`)}}
	require.NoError(t, db.Create(&task).Error)
	job := AsyncMediaJob{TaskId: task.TaskID, IdentityHash: "video-recovery-identity", UserId: task.UserId, TokenId: image.TokenId, ChannelId: image.ChannelId, Provider: "drama", Model: "video-model", RequestCipher: []byte("encrypted-request"), Status: "submitted", StorageStatus: "pending", BillingStatus: "reserved", Quota: task.Quota, DispatchedAt: now - 10, CreatedAt: now - 10, ExpiresAt: now + 1800}
	require.NoError(t, db.Create(&job).Error)
	return db, task, job
}

func TestAsyncMediaAuditLifecycle(t *testing.T) {
	db, image, _ := imageDatabaseFixture(t)
	require.NoError(t, db.AutoMigrate(&Task{}))
	job, reused, err := AcceptAsyncMediaJob(t.Context(), AsyncMediaJob{TaskId: "audit-video", IdentityHash: "audit-video", RequestHash: "request", UserId: image.UserId, TokenId: image.TokenId, ChannelId: image.ChannelId, Provider: "drama", Model: "video", ExpiresAt: time.Now().Unix() + 3600})
	require.NoError(t, err)
	require.False(t, reused)
	_, reused, err = AcceptAsyncMediaJob(t.Context(), job)
	require.NoError(t, err)
	require.True(t, reused)
	events, err := ListAsyncMediaEvents(t.Context(), job.TaskId)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "accepted", events[0].EventType)
	assert.Nil(t, events[0].KeyIndex, "an accepted task has no confirmed dispatch account")
	claimed, err := ClaimAsyncMediaJob(t.Context(), &job, "audit-lease", 60)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"next_attempt_at": 0}))
	require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"status": "submitting"}))
	dispatchedAt := time.Now().Unix()
	require.NoError(t, DispatchAsyncMediaJob(t.Context(), job, map[string]any{"dispatched_at": dispatchedAt, "billing_status": "reserved"}, 0, "original"))
	require.NoError(t, DispatchAsyncMediaJob(t.Context(), job, map[string]any{"dispatched_at": dispatchedAt, "billing_status": "reserved"}, 0, "original"))
	require.NoError(t, DispatchAsyncMediaJob(t.Context(), job, map[string]any{"dispatched_at": dispatchedAt}, -1, ""), "unknown historical key slots must not block submission")
	stale := job
	stale.LeaseToken = "lost-lease"
	assert.ErrorIs(t, UpdateAsyncMediaJob(t.Context(), stale, map[string]any{"status": "failed"}), ErrImageConflict)
	assert.ErrorIs(t, DispatchAsyncMediaJob(t.Context(), job, map[string]any{"dispatched_at": dispatchedAt}, 0, "https://private.example/?key=secret"), ErrImageConflict)
	task := Task{TaskID: job.TaskId, UserId: job.UserId, ChannelId: job.ChannelId, Platform: "drama", Status: TaskStatusSuccess, PrivateData: TaskPrivateData{AsyncMedia: true, TokenId: job.TokenId, UpstreamTaskID: "private-native-id"}}
	require.NoError(t, task.InsertWithContext(t.Context()))
	require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"status": "submitted", "storage_status": "ready", "billing_status": "settled"}))
	require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"status": "submitted", "storage_status": "ready", "billing_status": "settled", "next_attempt_at": time.Now().Unix() + 3}))
	require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"storage_status": "saving"}))
	require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"status": "succeeded", "storage_status": "succeeded", "error_message": "secret https://private.example/video.mp4?key=secret"}))
	events, err = ListAsyncMediaEvents(t.Context(), job.TaskId)
	require.NoError(t, err)
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.EventType)
		assert.NotContains(t, event.Message, "private")
		if event.EventType == "dispatched" {
			require.NotNil(t, event.KeyIndex)
			assert.Zero(t, *event.KeyIndex, "the first key slot must not be mistaken for unknown")
			assert.Equal(t, "original", event.ReferenceMode)
			assert.Equal(t, job.ChannelId, event.ChannelId)
		}
	}
	assert.Equal(t, []string{"accepted", "attempt_started", "dispatched", "billing_reserved", "submitted", "upstream_succeeded", "storage_ready", "billing_settled", "storage_saving", "succeeded", "storage_succeeded"}, types)
	public, err := common.Marshal(events)
	require.NoError(t, err)
	for _, private := range []string{"private", "key_index", "channel_id", "reference_mode", "attempt_generation", "event_key", "task_id"} {
		assert.NotContains(t, string(public), private)
	}
}

func TestAsyncMediaAuditAtomicTransitions(t *testing.T) {
	t.Run("repeated local storage recovery keeps distinct cycles", func(t *testing.T) {
		db, _, job := asyncVideoRecoveryFixture(t)
		for range 2 {
			require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"storage_status": "saving"}))
			require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"storage_status": "failed", "storage_retries": 1, "next_attempt_at": 1000}))
			query := db.WithContext(t.Context()).Model(&AsyncMediaJob{}).Where("id = ? AND status = ? AND storage_status = ?", job.ID, "submitted", "failed")
			changes := map[string]any{"next_attempt_at": 0, "storage_retries": 0}
			require.NoError(t, ChangeAsyncMediaJob(query, changes, "resume_requested"))
			require.NoError(t, ChangeAsyncMediaJob(query, changes, "resume_requested"), "the same accepted resume is a no-op")
		}
		require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"storage_status": "saving"}))
		require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"status": "succeeded", "storage_status": "succeeded"}))
		events, err := ListAsyncMediaEvents(t.Context(), job.TaskId)
		require.NoError(t, err)
		counts := make(map[string]int)
		for _, event := range events {
			counts[event.EventType]++
		}
		assert.Equal(t, 3, counts["storage_saving"])
		assert.Equal(t, 2, counts["storage_failed"])
		assert.Equal(t, 2, counts["resume_requested"])
		assert.Equal(t, 1, counts["storage_succeeded"])
	})
	t.Run("eligibility and rollback", func(t *testing.T) {
		db, _, job := asyncVideoRecoveryFixture(t)
		query := db.Model(&AsyncMediaJob{}).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId+1)
		assert.ErrorIs(t, ChangeAsyncMediaJob(query, map[string]any{"status": "failed"}, "terminated"), ErrImageConflict)
		query = db.Model(&AsyncMediaJob{}).Where("task_id = ?", job.TaskId).
			Where("EXISTS (SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.status = ?)", TaskStatusSuccess)
		assert.ErrorIs(t, ChangeAsyncMediaJob(query, map[string]any{"status": "failed"}, "terminated"), ErrImageConflict)
		events, err := ListAsyncMediaEvents(t.Context(), job.TaskId)
		require.NoError(t, err)
		assert.Empty(t, events, "pre-existing tasks must not acquire invented acceptance or dispatch records")
		require.NoError(t, db.Migrator().DropTable(&AsyncMediaEvent{}))
		assert.Error(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"status": "failed"}))
		var after AsyncMediaJob
		require.NoError(t, db.First(&after, job.ID).Error)
		assert.Equal(t, "submitted", after.Status, "a missing audit write rolls back the corresponding transition")
	})
	t.Run("refund and expiration are replay safe", func(t *testing.T) {
		db, _, job := asyncVideoRecoveryFixture(t)
		changes := map[string]any{"billing_status": "refunded", "status": "failed", "quota": 0}
		require.NoError(t, SaveAsyncMediaSubmissionRefund(t.Context(), job.TaskId, job.UserId, changes, nil))
		require.NoError(t, SaveAsyncMediaSubmissionRefund(t.Context(), job.TaskId, job.UserId, changes, nil))
		require.NoError(t, db.Model(&job).Update("expires_at", time.Now().Unix()-1).Error)
		require.NoError(t, db.First(&job, job.ID).Error)
		require.NoError(t, ExpireAsyncMediaJob(t.Context(), job, time.Now().Unix()))
		require.NoError(t, ExpireAsyncMediaJob(t.Context(), job, time.Now().Unix()))
		events, err := ListAsyncMediaEvents(t.Context(), job.TaskId)
		require.NoError(t, err)
		types := make([]string, 0, len(events))
		for _, event := range events {
			types = append(types, event.EventType)
		}
		assert.Equal(t, []string{"failed", "billing_refunded", "expired", "storage_expired"}, types)
	})
}

func TestAsyncVideoReferenceRecoveryPreservesHoldAndFencesOldAttempt(t *testing.T) {
	db, task, job := asyncVideoRecoveryFixture(t)
	original := task
	won, err := ScheduleAsyncVideoReferenceRecovery(t.Context(), task, job, "reference image could not be downloaded", true)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, db.First(&task, task.ID).Error)
	require.NoError(t, db.First(&job, job.ID).Error)
	assert.EqualValues(t, 1, task.AttemptGeneration)
	assert.EqualValues(t, TaskStatusQueued, task.Status)
	assert.Empty(t, task.PrivateData.UpstreamTaskID)
	assert.Empty(t, task.PrivateData.PluginState)
	assert.False(t, task.PrivateData.VideoTerminalConfirmed)
	assert.Empty(t, task.Data)
	require.Len(t, task.PrivateData.VideoAttempts, 1)
	assert.Equal(t, "native-original", task.PrivateData.VideoAttempts[0].UpstreamTaskID)
	assert.Equal(t, string(TaskStatusFailure), task.PrivateData.VideoAttempts[0].Status)
	assert.Equal(t, "reference image could not be downloaded", task.PrivateData.VideoAttempts[0].Reason)
	assert.EqualValues(t, 1, job.AttemptGeneration)
	assert.Equal(t, 1, job.ReferenceRetryCount)
	assert.Equal(t, "prepare", job.ReferenceRecoveryStage)
	assert.Equal(t, "reserved", job.BillingStatus)
	assert.Equal(t, original.Quota, job.Quota)

	stale := original
	stale.Status, stale.Quota = TaskStatusFailure, 0
	won, err = stale.UpdateWithStatus(original.Status)
	require.NoError(t, err)
	assert.False(t, won)
	require.NoError(t, stale.Update())
	require.NoError(t, stale.UpdateQuota())
	require.NoError(t, TaskBulkUpdateByID([]int64{task.ID}, map[string]any{"status": TaskStatusFailure}))
	stale.Quota = original.Quota
	won, _, err = SettleTaskBilling(t.Context(), &stale, original.Status, 0, nil, "")
	require.NoError(t, err)
	assert.False(t, won)
	_, err = RecordMeasuredTaskSubmitAccounting(t.Context(), &original)
	assert.ErrorIs(t, err, ErrTaskAttemptConflict)
	assert.Error(t, SaveAsyncMediaSubmissionRefund(t.Context(), job.TaskId, job.UserId, map[string]any{"billing_status": "refunded", "quota": 0}, nil))
	require.NoError(t, db.First(&task, task.ID).Error)
	assert.EqualValues(t, TaskStatusQueued, task.Status)
	assert.Equal(t, 100, task.Quota)

	claimed, err := ClaimAsyncMediaJob(t.Context(), &job, "recovery-lease", 60)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"reference_recovery_stage": "dispatched", "status": "submitting", "dispatched_at": time.Now().Unix()}))
	won, err = CommitAsyncVideoReferenceSubmission(t.Context(), job, task, "native-replacement", []byte(`{"status":"SUBMITTED"}`), []byte(`{"native_id":"replacement"}`))
	require.NoError(t, err)
	require.True(t, won)
	won, err = CommitAsyncVideoReferenceSubmission(t.Context(), job, task, "duplicate-replacement", nil, nil)
	require.NoError(t, err)
	assert.False(t, won)
	require.NoError(t, db.First(&task, task.ID).Error)
	require.NoError(t, db.First(&job, job.ID).Error)
	assert.Equal(t, "native-replacement", task.PrivateData.UpstreamTaskID)
	assert.EqualValues(t, TaskStatusSubmitted, task.Status)
	assert.False(t, task.PrivateData.VideoTerminalConfirmed)
	assert.Equal(t, "waiting", job.ReferenceRecoveryStage)
	assert.Equal(t, "reserved", job.BillingStatus)
	task.Status = TaskStatusSuccess
	task.PrivateData.VideoTerminalConfirmed = true
	won, held, err := SettleTaskBilling(t.Context(), &task, TaskStatusSubmitted, 80, nil, "")
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, 100, held)
	require.Len(t, task.PrivateData.VideoAttempts, 2)
	assert.Equal(t, "native-original", task.PrivateData.VideoAttempts[0].UpstreamTaskID)
	assert.Equal(t, string(TaskStatusFailure), task.PrivateData.VideoAttempts[0].Status)
	assert.Equal(t, "native-replacement", task.PrivateData.VideoAttempts[1].UpstreamTaskID)
	assert.Equal(t, string(TaskStatusSuccess), task.PrivateData.VideoAttempts[1].Status)
	assert.True(t, task.PrivateData.VideoTerminalConfirmed)
	var user User
	var token Token
	var channel Channel
	require.NoError(t, db.First(&user, task.UserId).Error)
	require.NoError(t, db.First(&token, task.PrivateData.TokenId).Error)
	require.NoError(t, db.First(&channel, task.ChannelId).Error)
	assert.Equal(t, 920, user.Quota)
	assert.Equal(t, 80, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
	assert.Equal(t, 920, token.RemainQuota)
	assert.Equal(t, 80, token.UsedQuota)
	assert.EqualValues(t, 80, channel.UsedQuota)
}

func TestAsyncVideoReferenceRecoveryRejectsUnsafeTransitions(t *testing.T) {
	for _, reason := range []string{"settled hold", "missing durable accounting", "second recovery", "expired job", "foreign provider", "existing output", "buffered output", "missing native receipt"} {
		t.Run(reason, func(t *testing.T) {
			db, task, job := asyncVideoRecoveryFixture(t)
			switch reason {
			case "settled hold":
				job.BillingStatus = "settled"
			case "missing durable accounting":
				task.PrivateData.SubmitAccountingRecorded = false
			case "second recovery":
				job.ReferenceRetryCount = 1
			case "expired job":
				job.ExpiresAt = time.Now().Unix() - 1
			case "foreign provider":
				job.Provider = "other-provider"
			case "existing output":
				task.PrivateData.ResultURL = "https://output.example/video.mp4"
			case "buffered output":
				job.OutputObjectKey = "buffered/output"
			case "missing native receipt":
				task.PrivateData.UpstreamTaskID = ""
			}
			require.NoError(t, db.Save(&task).Error)
			require.NoError(t, db.Save(&job).Error)
			won, err := ScheduleAsyncVideoReferenceRecovery(t.Context(), task, job, "reference download failed", true)
			require.NoError(t, err)
			assert.False(t, won)
			var currentTask Task
			var currentJob AsyncMediaJob
			require.NoError(t, db.First(&currentTask, task.ID).Error)
			require.NoError(t, db.First(&currentJob, job.ID).Error)
			assert.Equal(t, task.Status, currentTask.Status)
			assert.Equal(t, task.Quota, currentTask.Quota)
			assert.Equal(t, task.AttemptGeneration, currentTask.AttemptGeneration)
			assert.Equal(t, job.BillingStatus, currentJob.BillingStatus)
			assert.Equal(t, job.ReferenceRetryCount, currentJob.ReferenceRetryCount)
			assert.Empty(t, currentTask.PrivateData.VideoAttempts)
		})
	}
}

func TestAsyncVideoReferenceSubmissionReceiptRemainsPrivate(t *testing.T) {
	db, task, job := asyncVideoRecoveryFixture(t)
	task.PrivateData.UpstreamAsync = &dto.UpstreamAsyncProfile{}
	require.NoError(t, db.Model(&task).Update("private_data", task.PrivateData).Error)
	won, err := ScheduleAsyncVideoReferenceRecovery(t.Context(), task, job, "reference download failed", true)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, db.First(&task, task.ID).Error)
	require.NoError(t, db.First(&job, job.ID).Error)
	claimed, err := ClaimAsyncMediaJob(t.Context(), &job, "profile-recovery-lease", 60)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"reference_recovery_stage": "dispatched", "dispatched_at": time.Now().Unix()}))
	response := []byte(`{"task_id":"private-native-receipt","video_url":"https://private.example/secret"}`)
	won, err = CommitAsyncVideoReferenceSubmission(t.Context(), job, task, "private-native-receipt", response, nil)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, db.First(&task, task.ID).Error)
	assert.Empty(t, task.Data)
	assert.JSONEq(t, string(response), string(task.PrivateData.UpstreamAsyncResponse))
	public, err := common.Marshal(task)
	require.NoError(t, err)
	assert.NotContains(t, string(public), "private-native-receipt")
	assert.NotContains(t, string(public), "private.example")
}

func TestAsyncVideoReferenceRecoveryUnknownRetainsHold(t *testing.T) {
	db, task, job := asyncVideoRecoveryFixture(t)
	won, err := ScheduleAsyncVideoReferenceRecovery(t.Context(), task, job, "reference download failed", true)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, db.First(&task, task.ID).Error)
	require.NoError(t, db.First(&job, job.ID).Error)
	claimed, err := ClaimAsyncMediaJob(t.Context(), &job, "unknown-recovery-lease", 60)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, UpdateAsyncMediaJob(t.Context(), job, map[string]any{"reference_recovery_stage": "dispatched", "dispatched_at": time.Now().Unix()}))
	staleJob := job
	staleJob.LeaseToken = "superseded-lease"
	won, err = MarkAsyncVideoReferenceSubmissionUnknown(t.Context(), staleJob, task, "replacement receipt could not be confirmed")
	require.NoError(t, err)
	assert.False(t, won)
	won, err = MarkAsyncVideoReferenceSubmissionUnknown(t.Context(), job, task, "replacement receipt could not be confirmed")
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, db.First(&task, task.ID).Error)
	require.NoError(t, db.First(&job, job.ID).Error)
	assert.EqualValues(t, TaskStatusUnknown, task.Status)
	assert.Equal(t, 100, task.Quota)
	assert.Empty(t, task.PrivateData.UpstreamTaskID)
	require.Len(t, task.PrivateData.VideoAttempts, 2)
	assert.Equal(t, string(TaskStatusUnknown), task.PrivateData.VideoAttempts[1].Status)
	assert.EqualValues(t, 1, task.PrivateData.VideoAttempts[1].Generation)
	assert.Equal(t, "execution_unknown", job.Status)
	assert.Equal(t, "unknown", job.BillingStatus)
	assert.Equal(t, 100, job.Quota)
	assert.Zero(t, job.NextAttemptAt)
	var user User
	var token Token
	require.NoError(t, db.First(&user, task.UserId).Error)
	require.NoError(t, db.First(&token, task.PrivateData.TokenId).Error)
	assert.Equal(t, 900, user.Quota)
	assert.Equal(t, 900, token.RemainQuota)
}

func TestMediaInputObjectOwnershipAndSlotIdentity(t *testing.T) {
	db, task, job := asyncVideoRecoveryFixture(t)
	now := time.Now().Unix()
	object := MediaInputObject{ObjectId: "media-input-original", TaskId: task.TaskID, UserId: task.UserId, TokenId: job.TokenId, Slot: "0", Kind: "image", SourceHash: strings.Repeat("a", 64), Checksum: strings.Repeat("b", 64), RootPath: t.TempDir(), ObjectKey: "input/original", MimeType: "image/png", ByteSize: 32, Status: "active", CreatedAt: now, ExpiresAt: now + 300}
	saved, err := SaveMediaInputObject(t.Context(), object)
	require.NoError(t, err)
	assert.Positive(t, saved.ID)
	repeated := object
	repeated.ObjectId, repeated.ObjectKey = "media-input-repeated", "input/repeated"
	repeated.ExpiresAt += 60
	reused, err := SaveMediaInputObject(t.Context(), repeated)
	require.NoError(t, err)
	assert.Equal(t, saved.ObjectId, reused.ObjectId)
	assert.Equal(t, saved.CreatedAt, reused.CreatedAt)
	assert.Equal(t, saved.ExpiresAt, reused.ExpiresAt)
	repeated.SourceHash = strings.Repeat("c", 64)
	_, err = SaveMediaInputObject(t.Context(), repeated)
	assert.ErrorIs(t, err, ErrImageConflict)
	repeated = object
	repeated.UserId++
	_, err = SaveMediaInputObject(t.Context(), repeated)
	assert.Error(t, err)
	repeated = object
	repeated.ExpiresAt = job.ExpiresAt + 1
	_, err = SaveMediaInputObject(t.Context(), repeated)
	assert.ErrorIs(t, err, ErrImageConflict)
	repeated = object
	repeated.ObjectId = "media-input-slot-conflict"
	assert.Error(t, db.Create(&repeated).Error, "task input slots remain unique independently of helper checks")
	stale := saved
	stale.Checksum = strings.Repeat("d", 64)
	require.NoError(t, DeleteMediaInputObject(t.Context(), stale))
	objects, err := ListMediaInputObjects(t.Context(), task.TaskID, task.UserId, job.TokenId)
	require.NoError(t, err)
	require.Len(t, objects, 1)
	require.NoError(t, DeleteMediaInputObject(t.Context(), saved))
	objects, err = ListMediaInputObjects(t.Context(), task.TaskID, task.UserId, job.TokenId)
	require.NoError(t, err)
	assert.Empty(t, objects)
}

func TestAsyncVideoReferenceRecoveryQueuePhases(t *testing.T) {
	db, _, pending := asyncVideoRecoveryFixture(t)
	jobs := []AsyncMediaJob{
		{TaskId: "legacy-submit", IdentityHash: "legacy-submit", Status: "queued", StorageStatus: "pending"},
		{TaskId: "reference-prepare", IdentityHash: "reference-prepare", Status: "queued", StorageStatus: "pending", ReferenceRecoveryStage: "prepare", ReferenceRetryCount: 1, AttemptGeneration: 1},
		{TaskId: "reference-submit", IdentityHash: "reference-submit", Status: "queued", StorageStatus: "pending", ReferenceRecoveryStage: "submit", ReferenceRetryCount: 1, AttemptGeneration: 1},
		{TaskId: "video-output-ready", IdentityHash: "video-output-ready", Status: "submitted", StorageStatus: "ready"},
	}
	for i := range jobs {
		require.NoError(t, db.Create(&jobs[i]).Error)
	}
	require.NoError(t, db.Model(&jobs[0]).Update("reference_recovery_stage", nil).Error)
	for _, tc := range []struct {
		phase string
		ids   []string
	}{
		{phase: "submit", ids: []string{"legacy-submit", "reference-submit"}},
		{phase: "status", ids: []string{pending.TaskId}},
		{phase: "download", ids: []string{"reference-prepare", "video-output-ready"}},
	} {
		due, err := GetDueAsyncMediaJobsForPhase(t.Context(), time.Now().Unix(), 10, tc.phase)
		require.NoError(t, err)
		ids := make([]string, len(due))
		for i := range due {
			ids[i] = due[i].TaskId
		}
		assert.ElementsMatch(t, tc.ids, ids)
	}
	preparing := jobs[1]
	require.NoError(t, db.Model(&AsyncMediaJob{}).Where("id = ?", preparing.ID).Update("reference_recovery_stage", "submit").Error)
	claimed, err := ClaimAsyncMediaJob(t.Context(), &preparing, "stale-phase-lease", 30)
	require.NoError(t, err)
	assert.False(t, claimed)
	preparing.ReferenceRecoveryStage = "submit"
	claimed, err = ClaimAsyncMediaJob(t.Context(), &preparing, "current-phase-lease", 30)
	require.NoError(t, err)
	assert.True(t, claimed)
}
