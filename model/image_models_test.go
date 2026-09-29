package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func imageTestDatabase(t *testing.T, name string) *gorm.DB {
	t.Helper()
	if dsn := os.Getenv("ASYNC_IMAGE_TEST_MYSQL_DSN"); dsn != "" {
		cfg, err := mysqlDriver.ParseDSN(dsn)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(cfg.DBName, "new_api_image_test_"), "use an isolated image test database")
		cfg.DBName = ""
		admin, err := sql.Open("mysql", cfg.FormatDSN())
		require.NoError(t, err)
		database := "new_api_image_test_" + strings.ToLower(common.GetUUID())
		_, err = admin.ExecContext(t.Context(), "CREATE DATABASE `"+database+"` CHARACTER SET utf8mb4")
		require.NoError(t, err)
		cfg.DBName = database
		db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)
		t.Cleanup(func() {
			connection, _ := db.DB()
			_ = connection.Close()
			_, err := admin.ExecContext(context.Background(), "DROP DATABASE `"+database+"`")
			assert.NoError(t, err)
			_ = admin.Close()
		})
		return db
	}
	dsn := os.Getenv("ASYNC_IMAGE_TEST_PG_DSN")
	if dsn == "" {
		db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), name+".db")), &gorm.Config{})
		require.NoError(t, err)
		t.Cleanup(func() { connection, _ := db.DB(); _ = connection.Close() })
		return db
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(cfg.Database, "new_api_image_test_"), "PostgreSQL tests require a dedicated new_api_image_test_ database")
	schema := "image_test_" + strings.ToLower(common.GetUUID())
	admin := stdlib.OpenDB(*cfg)
	_, err = admin.ExecContext(context.Background(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema
	connection := stdlib.OpenDB(*cfg)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: connection}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = connection.Close()
		_, dropErr := admin.ExecContext(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		assert.NoError(t, dropErr)
		_ = admin.Close()
	})
	return db
}

func imageDatabaseFixture(t *testing.T) (*gorm.DB, AsyncImageTask, AsyncImageBill) {
	t.Helper()
	previousDB, previousLogDB := DB, LOG_DB
	previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousExport, previousLogs := common.RedisEnabled, common.DataExportEnabled, common.LogConsumeEnabled
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.SetLogDatabaseType(common.DatabaseTypeSQLite)
	if os.Getenv("ASYNC_IMAGE_TEST_PG_DSN") != "" {
		common.SetMainDatabaseType(common.DatabaseTypePostgreSQL)
		common.SetLogDatabaseType(common.DatabaseTypePostgreSQL)
	}
	common.RedisEnabled, common.DataExportEnabled, common.LogConsumeEnabled = false, true, true
	db := imageTestDatabase(t, "main")
	logDB := imageTestDatabase(t, "logs")
	DB, LOG_DB = db, logDB
	common.SetDatabaseTypes(common.DatabaseType(db.Dialector.Name()), common.DatabaseType(logDB.Dialector.Name()))
	initCol()
	t.Cleanup(func() {
		mainSQL, _ := db.DB()
		_ = mainSQL.Close()
		logSQL, _ := logDB.DB()
		_ = logSQL.Close()
		DB, LOG_DB = previousDB, previousLogDB
		common.SetMainDatabaseType(previousMainType)
		common.SetLogDatabaseType(previousLogType)
		initCol()
		common.RedisEnabled, common.DataExportEnabled, common.LogConsumeEnabled = previousRedis, previousExport, previousLogs
	})
	require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &Channel{}, &UserSubscription{}, &SubscriptionPlan{}, &QuotaData{}))
	require.NoError(t, logDB.AutoMigrate(&Log{}))
	require.NoError(t, MigrateImageLogs(logDB))
	user := User{Username: "image-test", Quota: 1000, Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(&user).Error)
	token := Token{UserId: user.Id, Key: "image-test-key", RemainQuota: 1000}
	require.NoError(t, db.Create(&token).Error)
	channel := Channel{Key: "upstream-test-key"}
	require.NoError(t, db.Create(&channel).Error)
	// Upgrade an existing baseline with representative user, Token and channel
	// data, then run the new migration again to verify idempotency.
	require.NoError(t, MigrateImageModels(db))
	require.NoError(t, MigrateImageModels(db))
	var preserved User
	require.NoError(t, db.First(&preserved, user.Id).Error)
	assert.Equal(t, user.Quota, preserved.Quota)
	now := time.Now().Unix()
	task := AsyncImageTask{TaskId: "asyncimg_test", UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Status: ImageTaskInvoking, Version: 1, LeaseToken: "lease-test", LeaseExpiresAt: now + 120, CreatedAt: now, RequestCipher: []byte("encrypted"), ExpiresAt: now + 86400, Model: "image-model", Group: "default"}
	require.NoError(t, db.Create(&task).Error)
	log := Log{UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Quota: 100, Type: LogTypeConsume, RequestId: "async-image:" + task.TaskId, CreatedAt: now, PromptTokens: 2, CompletionTokens: 3}
	payload, err := common.Marshal(log)
	require.NoError(t, err)
	bill := AsyncImageBill{TaskId: task.TaskId, BillingRequestId: log.RequestId, Fingerprint: "fixed-fingerprint", UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Quota: 100, FundingSource: "wallet", LogPayload: string(payload)}
	return db, task, bill
}

func stageImageBillFixture(t *testing.T, db *gorm.DB, task AsyncImageTask, bill AsyncImageBill) {
	t.Helper()
	require.NoError(t, StageAsyncImageOutput(context.Background(), task, []AsyncImageStagingObject{{Data: []byte("image"), Checksum: "checksum", Width: 1024, Height: 1024, ContentType: "image/png"}}, bill))
	require.NoError(t, db.Create(&AsyncImageResult{TaskId: task.TaskId, ObjectId: "object", ImageIndex: 0}).Error)
	require.NoError(t, db.Model(&AsyncImageTask{}).Where("task_id = ?", task.TaskId).Updates(map[string]any{"status": ImageTaskBillingPending, "result_count": 1}).Error)
}

func TestAsyncImageBillAtomicSettlementAndLogRetry(t *testing.T) {
	db, task, bill := imageDatabaseFixture(t)
	stageImageBillFixture(t, db, task, bill)
	require.NoError(t, db.Unscoped().Model(&Token{}).Where("id = ?", task.TokenId).Update("remain_quota", 50).Error)
	assert.ErrorIs(t, ApplyAsyncImageBill(context.Background(), task.TaskId, bill.Fingerprint), ErrImageInsufficientQuota)
	var user User
	require.NoError(t, db.First(&user, task.UserId).Error)
	assert.Equal(t, 1000, user.Quota, "token failure must not debit the wallet")
	require.NoError(t, db.Unscoped().Model(&Token{}).Where("id = ?", task.TokenId).Update("remain_quota", 1000).Error)
	require.NoError(t, db.Delete(&Token{}, task.TokenId).Error)
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		connection, err := db.DB()
		require.NoError(t, err)
		connection.SetMaxOpenConns(1)
	}
	start := make(chan struct{})
	settled := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			settled <- ApplyAsyncImageBill(context.Background(), task.TaskId, bill.Fingerprint)
		}()
	}
	close(start)
	for range 2 {
		require.NoError(t, <-settled)
	}
	assert.ErrorIs(t, ApplyAsyncImageBill(context.Background(), task.TaskId, "changed-price"), ErrImageConflict)
	require.NoError(t, db.First(&user, task.UserId).Error)
	assert.Equal(t, 900, user.Quota)
	assert.Equal(t, 100, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
	var token Token
	require.NoError(t, db.Unscoped().First(&token, task.TokenId).Error)
	assert.Equal(t, 900, token.RemainQuota)
	var projectionCount int64
	require.NoError(t, db.Model(&AsyncImageUsageProjection{}).Count(&projectionCount).Error)
	assert.EqualValues(t, 1, projectionCount)
	require.NoError(t, LOG_DB.Migrator().DropTable(&Log{}))
	assert.Error(t, ConfirmAsyncImageLog(context.Background(), task.TaskId))
	require.NoError(t, LOG_DB.AutoMigrate(&Log{}))
	require.NoError(t, ConfirmAsyncImageLog(context.Background(), task.TaskId))
	require.NoError(t, ConfirmAsyncImageLog(context.Background(), task.TaskId))
	var logCount int64
	require.NoError(t, LOG_DB.Model(&Log{}).Where("request_id = ?", bill.BillingRequestId).Count(&logCount).Error)
	assert.EqualValues(t, 1, logCount)
	require.NoError(t, db.First(&user, task.UserId).Error)
	assert.Equal(t, 900, user.Quota)

	ending := task
	ending.Id, ending.TaskId = 0, "asyncimg_termination"
	require.NoError(t, db.Create(&ending).Error)
	endingBill := bill
	endingBill.TaskId, endingBill.BillingRequestId, endingBill.Fingerprint = ending.TaskId, "async-image:"+ending.TaskId, "termination-fingerprint"
	var endingLog Log
	require.NoError(t, common.UnmarshalJsonStr(bill.LogPayload, &endingLog))
	endingLog.RequestId = endingBill.BillingRequestId
	endingPayload, err := common.Marshal(endingLog)
	require.NoError(t, err)
	endingBill.LogPayload = string(endingPayload)
	stageImageBillFixture(t, db, ending, endingBill)
	require.NoError(t, db.Where("task_id = ?", ending.TaskId).Take(&ending).Error)
	start = make(chan struct{})
	applied, terminated := make(chan error, 1), make(chan error, 1)
	go func() {
		<-start
		applied <- ApplyAsyncImageBill(context.Background(), ending.TaskId, endingBill.Fingerprint)
	}()
	go func() {
		<-start
		terminated <- TerminateAsyncImageTask(context.Background(), ending)
	}()
	close(start)
	applyErr, terminateErr := <-applied, <-terminated
	require.NoError(t, terminateErr)
	if applyErr != nil {
		require.ErrorIs(t, applyErr, ErrImageConflict)
	}
	require.NoError(t, db.Where("task_id = ?", ending.TaskId).Take(&endingBill).Error)
	require.NoError(t, db.Where("task_id = ?", ending.TaskId).Take(&ending).Error)
	require.NoError(t, db.First(&user, task.UserId).Error)
	assert.Equal(t, ImageTaskFailed, ending.Status)
	assert.Zero(t, ending.NextAttemptAt)
	assert.Empty(t, ending.RequestCipher)
	if endingBill.Status == "applied" {
		assert.Equal(t, 800, user.Quota)
		assert.Equal(t, "settled", ending.ReconciliationStatus)
		assert.Equal(t, "succeeded", ending.BillingStatus)
	} else {
		assert.Equal(t, "fixed", endingBill.Status)
		assert.Equal(t, 900, user.Quota)
		assert.Equal(t, "not_charged", ending.ReconciliationStatus)
	}
}

func TestAsyncImageOutputAndTerminalCAS(t *testing.T) {
	db, task, bill := imageDatabaseFixture(t)
	assert.Error(t, StageAsyncImageOutput(context.Background(), task, []AsyncImageStagingObject{{Data: []byte("invalid")}}, bill))
	var stored AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", task.TaskId).First(&stored).Error)
	assert.Equal(t, ImageTaskInvoking, stored.Status)
	assert.Equal(t, task.Version, stored.Version)
	require.NoError(t, TransitionImageTask(context.Background(), task, map[string]any{"status": ImageTaskFailed, "error_code": "admin_terminated", "request_cipher": nil}, "terminated", "Ended by administrator", ""))
	assert.ErrorIs(t, StageAsyncImageOutput(context.Background(), task, []AsyncImageStagingObject{{Data: []byte("late"), Checksum: "late", Width: 1, Height: 1}}, bill), ErrImageConflict)
	assert.ErrorIs(t, TransitionImageTask(context.Background(), task, map[string]any{"status": ImageTaskSucceeded}, "late", "", ""), ErrImageConflict)
	var count int64
	require.NoError(t, db.Model(&AsyncImageBill{}).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, db.Where("task_id = ?", task.TaskId).First(&stored).Error)
	assert.Empty(t, stored.RequestCipher)
	assert.False(t, stored.ResultsAvailable())
	crashed := task
	crashed.Id = 0
	crashed.TaskId = "crashed-postprocessing"
	crashed.Status = ImageTaskBillingPending
	crashed.LeaseExpiresAt = time.Now().Unix() - 1
	crashed.NextAttemptAt = time.Now().Unix()
	require.NoError(t, db.Create(&crashed).Error)
	claimed, err := ClaimAsyncImageTask(context.Background(), crashed.TaskId, "replacement-worker", 120)
	require.NoError(t, err, "expired post-processing leases must be reclaimable after a process crash")
	assert.Equal(t, ImageTaskBillingPending, claimed.Status)
	assert.Equal(t, "replacement-worker", claimed.LeaseToken)
	assert.ErrorIs(t, TransitionImageTask(context.Background(), crashed, map[string]any{"status": ImageTaskSucceeded}, "stale_worker", "", ""), ErrImageConflict)
	assert.ErrorIs(t, HeartbeatAsyncImageTask(context.Background(), crashed.TaskId, crashed.LeaseToken, 120), ErrImageConflict)
}

func TestRecoverAsyncImageTaskContinuesKnownUpstreamPolling(t *testing.T) {
	db, task, _ := imageDatabaseFixture(t)
	now := time.Now().Unix()
	task.Id = 0
	task.TaskId = "known-upstream-recovery"
	task.StartedAt = now - 120
	task.DispatchedAt = now - 120
	task.UpstreamTaskId = "upstream-image-task"
	task.LeaseExpiresAt = now - 1
	require.NoError(t, db.Create(&task).Error)

	require.NoError(t, RecoverAsyncImageTasks(context.Background(), 100, 60))

	var recovered AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&recovered).Error)
	assert.Equal(t, ImageTaskInvoking, recovered.Status)
	assert.Equal(t, task.UpstreamTaskId, recovered.UpstreamTaskId)
	assert.Equal(t, task.RequestCipher, recovered.RequestCipher)
	assert.Empty(t, recovered.LeaseToken)
	assert.Zero(t, recovered.LeaseExpiresAt)
	assert.Equal(t, now, recovered.NextAttemptAt)

	var outbox ImageOutbox
	require.NoError(t, db.Where("aggregate_id = ? AND kind = ?", task.TaskId, "execute").Take(&outbox).Error)
	assert.Equal(t, "pending", outbox.Status)
}

func TestAsyncImageSubscriptionSettlement(t *testing.T) {
	db, task, bill := imageDatabaseFixture(t)
	plan := SubscriptionPlan{Title: "Image test plan"}
	require.NoError(t, db.Create(&plan).Error)
	sub := UserSubscription{UserId: task.UserId, PlanId: plan.Id, AmountTotal: 1000, Status: "active", StartTime: time.Now().Unix(), EndTime: time.Now().Unix() + 86400}
	require.NoError(t, db.Create(&sub).Error)
	selection, err := SelectImageFunding(context.Background(), task.UserId, 100, "subscription_only")
	require.NoError(t, err)
	assert.Equal(t, sub.Id, selection.SubscriptionId)
	bill.FundingSource, bill.SubscriptionId, bill.SubscriptionAmount = "subscription", sub.Id, 100
	stageImageBillFixture(t, db, task, bill)
	require.NoError(t, ApplyAsyncImageBill(context.Background(), task.TaskId, bill.Fingerprint))
	require.NoError(t, ApplyAsyncImageBill(context.Background(), task.TaskId, bill.Fingerprint))
	require.NoError(t, db.First(&sub, sub.Id).Error)
	assert.EqualValues(t, 100, sub.AmountUsed)
	var user User
	require.NoError(t, db.First(&user, task.UserId).Error)
	assert.Equal(t, 1000, user.Quota)
}

func TestAsyncImageLibraryPublicationAndReferenceCleanup(t *testing.T) {
	db, task, _ := imageDatabaseFixture(t)
	ctx := context.Background()
	require.NoError(t, RecordImageLibraryImport(ctx, task.UserId, 1))
	assert.ErrorIs(t, RecordImageLibraryImport(ctx, task.UserId, 1), ErrImageUploadRateLimited)
	secondUser := User{Username: "second-import-user", AffCode: "second-import-aff", Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(&secondUser).Error)
	require.NoError(t, RecordImageLibraryImport(ctx, secondUser.Id, 1), "import limits are per user")
	now := time.Now().Unix()
	object := ImageStorageObject{ObjectId: "durable-original", IdentityHash: "durable-original", Class: "durable", Status: "active", Checksum: "original-sha256", ContentType: "image/png", ByteSize: 100, CreatedAt: now}
	require.NoError(t, db.Create(&object).Error)
	submission := ImageSubmissionRequest{SubmissionId: "imgsub_local", UserId: task.UserId, OperationKey: "submission-key", Fingerprint: "submission-fp", Checksum: object.Checksum, ContentType: object.ContentType, ByteSize: object.ByteSize, Status: "pending_review", Version: 1, CreatedAt: now, ExpiresAt: now + 86400, Metadata: `{"public_title":"Public title","private_prompt":"Private prompt"}`}
	require.NoError(t, db.Create(&submission).Error)
	require.NoError(t, ModerateImageSubject(ctx, 99, submission.SubmissionId, "approve", "", true, 0))
	require.NoError(t, db.Where("id = ?", submission.Id).Take(&submission).Error)
	assert.Equal(t, "approved_pending_sync", submission.Status)
	var count int64
	require.NoError(t, db.Model(&ImagePublication{}).Count(&count).Error)
	assert.Zero(t, count, "metadata approval must not publish an image")
	item := ImageLibraryItem{AssetId: "img_local", UserId: task.UserId, ObjectId: object.ObjectId, SourceKey: "archive-key", Fingerprint: "archive-fp", Prompt: "Private prompt", Title: "Private title", CreatedAt: now, ExpiresAt: now + 86400}
	created, reused, err := CommitImageLibraryItem(ctx, item, 100, 1000, submission.SubmissionId)
	require.NoError(t, err)
	assert.False(t, reused)
	_, reused, err = CommitImageLibraryItem(ctx, item, 100, 1000, submission.SubmissionId)
	require.NoError(t, err)
	assert.True(t, reused)
	var publication ImagePublication
	require.NoError(t, db.Where("asset_id = ?", created.AssetId).Take(&publication).Error)
	assert.Equal(t, "published", publication.Status)
	assert.Empty(t, publication.PublicPrompt, "private prompt must not be published by default")
	_, err = ClaimImageObjectDeletion(ctx, object.ObjectId, "cleaner", now, 120)
	assert.ErrorIs(t, err, ErrImageConflict)
	require.NoError(t, db.Model(&ImageLibraryItem{}).Where("id = ?", created.Id).Update("deleted_at", now).Error)
	_, err = ClaimImageObjectDeletion(ctx, object.ObjectId, "cleaner", now, 120)
	assert.ErrorIs(t, err, ErrImageConflict, "published references still protect a deleted library item")
	require.NoError(t, ModerateImageSubject(ctx, task.UserId, publication.PublicationId, "withdraw", "", false, task.UserId))
	claimed, err := ClaimImageObjectDeletion(ctx, object.ObjectId, "cleaner", now, 120)
	require.NoError(t, err)
	assert.Equal(t, "deleting", claimed.Status)
	second := item
	second.AssetId = "img_second"
	second.SourceKey = "second-key"
	_, _, err = CommitImageLibraryItem(ctx, second, 100, 1000, "")
	assert.Error(t, err, "a deleting object cannot acquire a new asset reference")
	intent := ImageUploadIntent{IntentKey: "live-task-intent", TaskId: task.TaskId, ProfileId: "fixture", ObjectKey: "pending.png", Status: "pending", CreatedAt: now - 86401}
	require.NoError(t, db.Create(&intent).Error)
	_, err = ClaimUnconfirmedImageIntent(ctx, intent.Id, "cleaner", 120, now)
	assert.ErrorIs(t, err, ErrImageConflict, "unconfirmed output for a live task is protected")
	orphan := intent
	orphan.Id, orphan.TaskId, orphan.IntentKey, orphan.ObjectKey = 0, "", "orphan-intent", "orphan.png"
	require.NoError(t, db.Create(&orphan).Error)
	_, err = ClaimUnconfirmedImageIntent(ctx, orphan.Id, "orphan-cleaner", 120, now)
	require.NoError(t, err)
	require.NoError(t, db.Model(&ImageUploadIntent{}).Where("id = ?", orphan.Id).Updates(map[string]any{"first_delete_at": now, "lease_expires_at": 0}).Error)
	_, err = ClaimUnconfirmedImageIntent(ctx, orphan.Id, "too-early", 120, now+599)
	assert.ErrorIs(t, err, ErrImageConflict)
	_, err = ClaimUnconfirmedImageIntent(ctx, orphan.Id, "second-delete", 120, now+600)
	require.NoError(t, err)
}

func TestAsyncImageReservationSettlementAndCompetition(t *testing.T) {
	db, task, bill := imageDatabaseFixture(t)
	if db.Dialector.Name() == "sqlite" {
		connection, err := db.DB()
		require.NoError(t, err)
		connection.SetMaxOpenConns(1)
	}
	other := task
	other.Id, other.TaskId = 0, "competing-image"
	require.NoError(t, db.Create(&other).Error)
	start, finished := make(chan struct{}), make(chan error, 2)
	for _, candidate := range []AsyncImageTask{task, other} {
		go func() {
			<-start
			finished <- ReserveAsyncImageQuota(t.Context(), candidate, 600, ImageFundingSelection{Source: "wallet"})
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		err := <-finished
		if err == nil {
			winners++
		} else {
			assert.ErrorIs(t, err, ErrImageInsufficientQuota)
		}
	}
	assert.Equal(t, 1, winners)
	var reservation AsyncImageBill
	require.NoError(t, db.Where("status = ?", "reserved").Take(&reservation).Error)
	task = AsyncImageTask{}
	require.NoError(t, db.Where("task_id = ?", reservation.TaskId).Take(&task).Error)
	require.NoError(t, ReserveAsyncImageQuota(t.Context(), task, 600, ImageFundingSelection{Source: "wallet"}))
	assert.Equal(t, 400, getUserQuotaFromDB(t, task.UserId))
	reserved, err := TryReserveUserQuota(task.UserId, 500)
	require.NoError(t, err)
	assert.False(t, reserved, "ordinary relays cannot spend the image hold")
	bill.TaskId, bill.BillingRequestId = task.TaskId, "async-image:"+task.TaskId
	stageImageBillFixture(t, db, task, bill)
	require.NoError(t, ApplyAsyncImageBill(t.Context(), task.TaskId, bill.Fingerprint))
	require.NoError(t, ApplyAsyncImageBill(t.Context(), task.TaskId, bill.Fingerprint))
	assert.Equal(t, 900, getUserQuotaFromDB(t, task.UserId))
	token := getTokenFromDB(t, task.TokenId)
	assert.Equal(t, 900, token.RemainQuota)
	assert.Equal(t, 100, token.UsedQuota)
	var user User
	require.NoError(t, db.First(&user, task.UserId).Error)
	assert.Equal(t, 1, user.RequestCount)
	assert.Equal(t, 100, user.UsedQuota)
}

func TestAsyncImageReservationRecoveryAndReconciliation(t *testing.T) {
	for _, status := range []string{ImageTaskFailed, ImageTaskExpired, ImageTaskExecutionUnknown} {
		t.Run(status, func(t *testing.T) {
			db, task, _ := imageDatabaseFixture(t)
			require.NoError(t, ReserveAsyncImageQuota(t.Context(), task, 600, ImageFundingSelection{Source: "wallet"}))
			// Simulate a crash after terminal state persistence but before refund.
			require.NoError(t, db.Model(&task).Update("status", status).Error)
			require.NoError(t, RecoverAsyncImageTasks(t.Context(), 100, 60))
			if status == ImageTaskExecutionUnknown {
				assert.Equal(t, 400, getUserQuotaFromDB(t, task.UserId))
				require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
				assert.ErrorIs(t, TerminateAsyncImageTask(t.Context(), task), ErrImageConflict)
				require.NoError(t, TerminateAsyncImageTaskAfterReconciliation(t.Context(), task))
			}
			require.NoError(t, RefundAsyncImageReservation(t.Context(), task.TaskId))
			assert.Equal(t, 1000, getUserQuotaFromDB(t, task.UserId))
			token := getTokenFromDB(t, task.TokenId)
			assert.Equal(t, 1000, token.RemainQuota)
			assert.Zero(t, token.UsedQuota)
			var bill AsyncImageBill
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
			assert.Equal(t, "refunded", bill.Status)
		})
	}
}

func TestAsyncImageReservationSharesBatchQuotaAndCompensates(t *testing.T) {
	db, task, bill := imageDatabaseFixture(t)
	resetBatchUpdateTestState(t)
	server := useUserCacheMiniRedis(t)
	_, err := GetUserCache(task.UserId)
	require.NoError(t, err)
	token, err := GetTokenByKey("image-test-key", true)
	require.NoError(t, err)
	common.BatchUpdateEnabled = true
	reserved, err := TryReserveUserQuota(task.UserId, 500)
	require.NoError(t, err)
	require.True(t, reserved)
	reserved, err = TryReserveTokenQuota(task.TokenId, token.Key, 500, false)
	require.NoError(t, err)
	require.True(t, reserved)
	assert.Equal(t, 1000, getUserQuotaFromDB(t, task.UserId), "ordinary quota is still waiting in the batch")
	assert.ErrorIs(t, ReserveAsyncImageQuota(t.Context(), task, 600, ImageFundingSelection{Source: "wallet"}), ErrImageInsufficientQuota)
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("image-reservation-test-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "async_image_bills" {
			tx.AddError(errors.New("injected reservation ledger failure"))
		}
	}))
	assert.ErrorContains(t, ReserveAsyncImageQuota(t.Context(), task, 400, ImageFundingSelection{Source: "wallet"}), "injected reservation ledger failure")
	require.NoError(t, db.Callback().Create().Remove("image-reservation-test-failure"))
	cached, err := GetUserCache(task.UserId)
	require.NoError(t, err)
	assert.Equal(t, 500, cached.Quota)
	cachedToken, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 500, cachedToken.RemainQuota)
	require.NoError(t, ReserveAsyncImageQuota(t.Context(), task, 400, ImageFundingSelection{Source: "wallet"}))
	stageImageBillFixture(t, db, task, bill)
	require.NoError(t, ApplyAsyncImageBill(t.Context(), task.TaskId, bill.Fingerprint))
	require.NoError(t, RefreshAsyncImageBillingCache(t.Context(), task.TaskId))
	cached, err = GetUserCache(task.UserId)
	require.NoError(t, err)
	assert.Equal(t, 400, cached.Quota, "settlement must preserve the other request's pending 500 deduction")
	cachedToken, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 400, cachedToken.RemainQuota)
	batchUpdate()
	assert.Equal(t, 400, getUserQuotaFromDB(t, task.UserId))
	other := task
	other.Id, other.TaskId, other.Status = 0, "cold-cache-image", ImageTaskInvoking
	require.NoError(t, db.Create(&other).Error)
	server.Del(getUserCacheKey(task.UserId))
	assert.ErrorContains(t, ReserveAsyncImageQuota(t.Context(), other, 100, ImageFundingSelection{Source: "wallet"}), "cache is unavailable")
	assert.Equal(t, 400, getUserQuotaFromDB(t, task.UserId))
}

func TestAsyncImageReservationRefundRecoveryDoesNotStarve(t *testing.T) {
	db, task, _ := imageDatabaseFixture(t)
	require.NoError(t, ReserveAsyncImageQuota(t.Context(), task, 100, ImageFundingSelection{Source: "wallet"}))
	other := task
	other.Id, other.TaskId = 0, "later-refundable-image"
	require.NoError(t, db.Create(&other).Error)
	require.NoError(t, ReserveAsyncImageQuota(t.Context(), other, 100, ImageFundingSelection{Source: "wallet"}))
	require.NoError(t, db.Model(&AsyncImageTask{}).Where("task_id IN ?", []string{task.TaskId, other.TaskId}).Update("status", ImageTaskFailed).Error)
	// A corrupt older ledger must not occupy the head of every recovery batch.
	require.NoError(t, db.Model(&AsyncImageBill{}).Where("task_id = ?", task.TaskId).Update("token_id", -1).Error)
	assert.Error(t, RecoverAsyncImageTasks(t.Context(), 1, 60))
	// Accepted-work refunds still apply to soft-deleted financial records.
	require.NoError(t, db.Delete(&User{}, task.UserId).Error)
	require.NoError(t, db.Delete(&Token{}, task.TokenId).Error)
	require.NoError(t, RecoverAsyncImageTasks(t.Context(), 1, 60))
	var user User
	require.NoError(t, db.Unscoped().First(&user, task.UserId).Error)
	assert.Equal(t, 900, user.Quota)
	var token Token
	require.NoError(t, db.Unscoped().First(&token, task.TokenId).Error)
	assert.Equal(t, 900, token.RemainQuota)
	assert.Equal(t, 100, token.UsedQuota)
	var count int64
	require.NoError(t, RefundAsyncImageReservation(t.Context(), other.TaskId))
	require.NoError(t, db.Model(&AsyncImageEvent{}).Where("task_id = ? AND event_type = ?", other.TaskId, "reservation_refunded").Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestAsyncImageReservationAdditionalSettlement(t *testing.T) {
	for _, quota := range []int{700, 1100} {
		t.Run(fmt.Sprint(quota), func(t *testing.T) {
			db, task, bill := imageDatabaseFixture(t)
			require.NoError(t, ReserveAsyncImageQuota(t.Context(), task, 600, ImageFundingSelection{Source: "wallet"}))
			bill.Quota = quota
			stageImageBillFixture(t, db, task, bill)
			err := ApplyAsyncImageBill(t.Context(), task.TaskId, bill.Fingerprint)
			if quota > 1000 {
				assert.ErrorIs(t, err, ErrImageInsufficientQuota)
				assert.Equal(t, 400, getUserQuotaFromDB(t, task.UserId))
				assert.Equal(t, 400, getTokenFromDB(t, task.TokenId).RemainQuota)
			} else {
				require.NoError(t, err)
				require.NoError(t, ApplyAsyncImageBill(t.Context(), task.TaskId, bill.Fingerprint))
				assert.Equal(t, 300, getUserQuotaFromDB(t, task.UserId))
				assert.Equal(t, 300, getTokenFromDB(t, task.TokenId).RemainQuota)
			}
		})
	}
}

func TestAsyncImageReservationSubscriptionPeriod(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_period", true: "reset_period"}[changed], func(t *testing.T) {
			db, task, bill := imageDatabaseFixture(t)
			plan := SubscriptionPlan{Title: "Image reservation plan"}
			require.NoError(t, db.Create(&plan).Error)
			sub := UserSubscription{UserId: task.UserId, PlanId: plan.Id, AmountTotal: 1000, Status: "active", LastResetTime: 1, StartTime: time.Now().Unix(), EndTime: time.Now().Unix() + 86400}
			require.NoError(t, db.Create(&sub).Error)
			require.NoError(t, ReserveAsyncImageQuota(t.Context(), task, 600, ImageFundingSelection{Source: "subscription", SubscriptionId: sub.Id}))
			if changed {
				require.NoError(t, db.Model(&sub).Updates(map[string]any{"last_reset_time": 2, "amount_used": 200}).Error)
			}
			bill.FundingSource, bill.SubscriptionId, bill.SubscriptionAmount = "subscription", sub.Id, int64(bill.Quota)
			stageImageBillFixture(t, db, task, bill)
			require.NoError(t, ApplyAsyncImageBill(t.Context(), task.TaskId, bill.Fingerprint))
			require.NoError(t, db.First(&sub, sub.Id).Error)
			if changed {
				assert.EqualValues(t, 200, sub.AmountUsed)
			} else {
				assert.EqualValues(t, 100, sub.AmountUsed)
			}
			assert.Equal(t, 1000, getUserQuotaFromDB(t, task.UserId))
			assert.Equal(t, 900, getTokenFromDB(t, task.TokenId).RemainQuota)
		})
	}
}

type imageReservationMigrationTrace struct {
	logger.Interface
	changes []string
}

func (trace *imageReservationMigrationTrace) Trace(_ context.Context, _ time.Time, sql func() (string, int64), _ error) {
	statement, _ := sql()
	upper := strings.ToUpper(strings.TrimSpace(statement))
	if strings.HasPrefix(upper, "CREATE ") || strings.HasPrefix(upper, "ALTER ") || strings.HasPrefix(upper, "DROP ") {
		trace.changes = append(trace.changes, statement)
	}
}

func TestAsyncImageReservationMigration(t *testing.T) {
	// This is the bill schema at the audit base, before durable reservations.
	type legacyBill struct {
		Id                                int
		TaskId                            string `gorm:"size:64;uniqueIndex"`
		BillingRequestId                  string `gorm:"size:96;uniqueIndex"`
		Fingerprint                       string `gorm:"size:64"`
		UserId, TokenId, ChannelId, Quota int
		FundingSource                     string `gorm:"size:32"`
		SubscriptionId                    int
		SubscriptionAmount                int64  `gorm:"type:bigint"`
		Snapshot, Usage                   string `gorm:"type:text"`
		Status, LogStatus                 string `gorm:"size:16"`
		LogPayload                        string `gorm:"type:text"`
		CreatedAt, AppliedAt              int64  `gorm:"type:bigint"`
	}
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgrade"}[upgrade], func(t *testing.T) {
			db := imageTestDatabase(t, "reservation-migration")
			query := "SELECT VERSION()"
			if db.Dialector.Name() == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("Database version: %s", version)
			if upgrade {
				require.NoError(t, db.Table("async_image_bills").AutoMigrate(&legacyBill{}))
				for _, status := range []string{"fixed", "applied"} {
					require.NoError(t, db.Table("async_image_bills").Create(&legacyBill{TaskId: status, BillingRequestId: "async-image:" + status, Fingerprint: "legacy-" + status, Quota: 123, FundingSource: "wallet", Snapshot: "legacy-snapshot", Status: status, LogStatus: "pending"}).Error)
				}
			}
			require.NoError(t, db.AutoMigrate(&AsyncImageBill{}))
			trace := &imageReservationMigrationTrace{Interface: logger.Default.LogMode(logger.Silent)}
			require.NoError(t, db.Session(&gorm.Session{Logger: trace}).AutoMigrate(&AsyncImageBill{}))
			assert.Empty(t, trace.changes, "the second startup must not alter this schema")
			if upgrade {
				var bills []AsyncImageBill
				require.NoError(t, db.Order("id").Find(&bills).Error)
				require.Len(t, bills, 2)
				for _, bill := range bills {
					assert.Equal(t, 123, bill.Quota)
					assert.Equal(t, "legacy-snapshot", bill.Snapshot)
					assert.Zero(t, bill.ReservedAt)
					assert.Zero(t, bill.ReservedQuota)
					assert.Zero(t, bill.ReservedTokenQuota)
					assert.Nil(t, bill.SubscriptionPeriodStart)
				}
				duplicate := bills[0]
				duplicate.Id = 0
				duplicate.BillingRequestId = "different-request"
				assert.Error(t, db.Create(&duplicate).Error)
				duplicate.TaskId = "different-task"
				duplicate.BillingRequestId = bills[0].BillingRequestId
				assert.Error(t, db.Create(&duplicate).Error)
			}
		})
	}
}
