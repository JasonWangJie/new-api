package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDisabledTaskArtifactStoreHasNoStorageBehavior(t *testing.T) {
	store := &disabledArtifactStore{}
	require.NotNil(t, store)
	assert.False(t, store.Enabled())

	task := &model.Task{TaskID: "task-disabled-store"}
	ref, err := store.Resolve(task, "video")
	require.NoError(t, err)
	assert.Nil(t, ref)

	ref, err = store.Persist(t.Context(), task, types.TaskArtifact{Key: "video", Type: "video"}, strings.NewReader("content"))
	assert.Nil(t, ref)
	assert.ErrorIs(t, err, ErrTaskArtifactStoreDisabled)
	assert.ErrorIs(t, store.Serve(&gin.Context{}, task, &StoredArtifactRef{Backend: "s3"}), ErrTaskArtifactStoreDisabled)
}

var _ TaskArtifactStore = disabledArtifactStore{}

func TestLocalMediaStorageAndRecovery(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "media.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	old := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = old; connection, _ := db.DB(); _ = connection.Close() })
	require.NoError(t, db.AutoMigrate(&model.Option{}, &model.Task{}, &model.AsyncMediaJob{}, &model.MediaArtifactObject{}))
	cfg := DefaultMediaRuntimeConfig()
	cfg.VideoAsyncEnabled, cfg.LocalPath, cfg.MaxFileBytes = true, t.TempDir(), 4096
	require.NoError(t, SaveMediaRuntimeConfig(t.Context(), cfg))
	job := model.AsyncMediaJob{TaskId: "task-local-media", IdentityHash: "local-media", UserId: 1, TokenId: 2, Status: "submitted", StorageStatus: "pending", ExpiresAt: time.Now().Unix() + 3600}
	require.NoError(t, db.Create(&job).Error)
	task := &model.Task{TaskID: job.TaskId, UserId: 1, Status: model.TaskStatusSuccess, Quota: 120}
	require.NoError(t, db.Create(task).Error)
	store := localMediaStore{}
	data := mediaTestMP4()
	artifact := types.TaskArtifact{Key: "video", Type: "video"}
	for _, corrupt := range [][]byte{[]byte("<html>error</html>"), data[:len(data)-1], bytes.Repeat([]byte("x"), 4097)} {
		_, err := store.Persist(t.Context(), task, artifact, bytes.NewReader(corrupt))
		require.Error(t, err)
		ref, err := store.Resolve(task, artifact.Key)
		require.NoError(t, err)
		require.Nil(t, ref)
	}
	// A failed disk write leaves no published object and can be retried.
	badPath := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(badPath, []byte("occupied"), 0600))
	originalPath := cfg.LocalPath
	cfg.LocalPath = badPath
	require.NoError(t, SaveMediaRuntimeConfig(t.Context(), cfg))
	_, err = store.Persist(t.Context(), task, artifact, bytes.NewReader(data))
	require.Error(t, err)
	cfg.LocalPath = originalPath
	require.NoError(t, SaveMediaRuntimeConfig(t.Context(), cfg))
	ref, err := store.Persist(t.Context(), task, artifact, bytes.NewReader(data))
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.EqualValues(t, len(data), ref.Size)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(data)), ref.Checksum)
	duplicate, err := store.Persist(t.Context(), task, artifact, strings.NewReader("must not read again"))
	require.NoError(t, err)
	assert.Equal(t, ref, duplicate)
	other, err := store.Resolve(&model.Task{TaskID: task.TaskID, UserId: 9}, artifact.Key)
	require.NoError(t, err)
	assert.Nil(t, other)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(method, "/media", nil)
		c.Request.Header.Set("Range", "bytes=8-15")
		require.NoError(t, store.Serve(c, task, ref))
		assert.Equal(t, http.StatusPartialContent, recorder.Code)
		assert.Equal(t, "8", recorder.Header().Get("Content-Length"))
		if method == http.MethodHead {
			assert.Empty(t, recorder.Body.Bytes())
		} else {
			assert.Equal(t, data[8:16], recorder.Body.Bytes())
		}
	}
	var object model.MediaArtifactObject
	require.NoError(t, db.Take(&object).Error)
	oldSecret := common.CryptoSecret
	common.CryptoSecret = "media-signature-test-secret"
	t.Cleanup(func() { common.CryptoSecret = oldSecret })
	link, err := MediaObjectURL(object, "https://gateway.example", 60)
	require.NoError(t, err)
	parsed, err := url.Parse(link)
	require.NoError(t, err)
	assert.True(t, VerifyMediaObjectAccess(object, parsed.Query().Get("expires"), parsed.Query().Get("access")))
	changed := object
	changed.ObjectId = "other-object"
	assert.False(t, VerifyMediaObjectAccess(changed, parsed.Query().Get("expires"), parsed.Query().Get("access")))
	assert.False(t, VerifyMediaObjectAccess(object, "1", parsed.Query().Get("access")))
	oldSubmit, oldPersist := SubmitAsyncVideoFunc, PersistAsyncVideoFunc
	t.Cleanup(func() { SubmitAsyncVideoFunc, PersistAsyncVideoFunc = oldSubmit, oldPersist })
	submissions, saves := 0, 0
	SubmitAsyncVideoFunc = func(context.Context, model.AsyncMediaJob, AsyncVideoRequest) error { submissions++; return nil }
	PersistAsyncVideoFunc = func(ctx context.Context, job model.AsyncMediaJob, task *model.Task) error {
		saves++
		if saves == 1 {
			return errors.New("disk temporarily unavailable")
		}
		_, err := store.Resolve(task, "video")
		return err
	}
	require.NoError(t, db.Model(&job).Update("storage_status", "ready").Error)
	for attempt := range 2 {
		require.NoError(t, db.Model(&job).Update("next_attempt_at", 0).Error)
		require.NoError(t, db.Where("id = ?", job.ID).Take(&job).Error)
		claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, common.GetUUID(), 30)
		require.NoError(t, err)
		require.True(t, claimed)
		second := job
		claimed, err = model.ClaimAsyncMediaJob(t.Context(), &second, "duplicate", 30)
		require.NoError(t, err)
		assert.False(t, claimed)
		err = ProcessAsyncMediaJob(t.Context(), job, cfg)
		if attempt == 0 {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}
	require.NoError(t, db.Where("id = ?", job.ID).Take(&job).Error)
	assert.Equal(t, "succeeded", job.Status)
	assert.Equal(t, "settled", job.BillingStatus)
	assert.Equal(t, 120, job.Quota)
	assert.Zero(t, submissions, "storage recovery must not generate or pre-consume again")
	assert.Equal(t, 2, saves)
	for _, billing := range []string{"pending", "reserving", "reserved"} {
		uncertain := model.AsyncMediaJob{TaskId: "uncertain-" + billing, IdentityHash: "uncertain-" + billing, UserId: 1, Status: "submitting", BillingStatus: billing, ExpiresAt: time.Now().Unix() + 3600}
		if billing == "pending" {
			uncertain.DispatchedAt = time.Now().Unix()
		}
		if billing == "reserved" {
			uncertain.Status = "submitted"
		}
		require.NoError(t, db.Create(&uncertain).Error)
		claimed, err := model.ClaimAsyncMediaJob(t.Context(), &uncertain, common.GetUUID(), 30)
		require.NoError(t, err)
		require.True(t, claimed)
		require.NoError(t, ProcessAsyncMediaJob(t.Context(), uncertain, cfg))
		require.NoError(t, db.Where("id = ?", uncertain.ID).Take(&uncertain).Error)
		assert.Equal(t, "failed", uncertain.Status)
		assert.Equal(t, "unknown", uncertain.BillingStatus)
	}
	assert.Zero(t, submissions)
	require.NoError(t, db.Model(&object).Update("expires_at", 1).Error)
	require.NoError(t, db.Model(&job).Update("expires_at", 1).Error)
	require.NoError(t, CleanupMediaObjects(t.Context()))
	_, err = os.Stat(filepath.Join(ref.Bucket, ref.ObjectKey))
	assert.ErrorIs(t, err, os.ErrNotExist)
	ref, err = store.Resolve(task, "video")
	require.NoError(t, err)
	assert.Nil(t, ref)
}

func TestAsyncMediaPoolsKeepSubmissionAndStatusAvailableDuringDownload(t *testing.T) {
	setupTaskPollingDatabase(t)
	asyncImageKeyFixture(t)
	entities := []any{&model.Option{}, &model.AsyncMediaJob{}, &model.MediaArtifactObject{}}
	for range 2 {
		require.NoError(t, model.DB.AutoMigrate(entities...))
	}
	t.Cleanup(func() { assert.NoError(t, model.DB.Migrator().DropTable(entities...)) })
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	previousRedis, previousRedisEnabled := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = previousRedis, previousRedisEnabled
		assert.NoError(t, client.Close())
	})
	cfg := DefaultMediaRuntimeConfig()
	cfg.SubmitConcurrency, cfg.StatusConcurrency, cfg.DownloadConcurrency = 1, 1, 1
	cfg.DownloadTimeout = 60
	require.NoError(t, SaveMediaRuntimeConfig(t.Context(), cfg))
	previousSubmit, previousPersist := SubmitAsyncVideoFunc, PersistAsyncVideoFunc
	t.Cleanup(func() { SubmitAsyncVideoFunc, PersistAsyncVideoFunc = previousSubmit, previousPersist })
	downloadStarted, releaseDownload := make(chan struct{}), make(chan struct{})
	var submissions, downloads atomic.Int32
	PersistAsyncVideoFunc = func(ctx context.Context, _ model.AsyncMediaJob, _ *model.Task) error {
		downloads.Add(1)
		close(downloadStarted)
		select {
		case <-releaseDownload:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	SubmitAsyncVideoFunc = func(ctx context.Context, job model.AsyncMediaJob, _ AsyncVideoRequest) error {
		submissions.Add(1)
		return model.DB.WithContext(ctx).Create(&model.Task{TaskID: job.TaskId, UserId: job.UserId, Status: model.TaskStatusInProgress}).Error
	}
	now := time.Now().Unix()
	slow := model.AsyncMediaJob{TaskId: "slow-download", IdentityHash: "slow-download", UserId: 1, Status: "submitted", StorageStatus: "ready", ExpiresAt: now + 3600}
	require.NoError(t, model.DB.Create(&slow).Error)
	require.NoError(t, model.DB.Create(&model.Task{TaskID: slow.TaskId, UserId: 1, Status: model.TaskStatusSuccess}).Error)
	first, err := DispatchAsyncMediaJobs(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		select {
		case <-releaseDownload:
		default:
			close(releaseDownload)
		}
		first.Wait()
	})
	select {
	case <-downloadStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("download worker did not start")
	}
	// Even an older completed result cannot consume the next submission slot.
	backlog := model.AsyncMediaJob{TaskId: "download-backlog", IdentityHash: "download-backlog", UserId: 1, Status: "submitted", StorageStatus: "ready", ExpiresAt: now + 3600}
	require.NoError(t, model.DB.Create(&backlog).Error)
	require.NoError(t, model.DB.Create(&model.Task{TaskID: backlog.TaskId, UserId: 1, Status: model.TaskStatusSuccess}).Error)
	queued := model.AsyncMediaJob{TaskId: "queued-video", IdentityHash: "queued-video", UserId: 1, Status: "queued", StorageStatus: "pending", ExpiresAt: now + 3600}
	queued.RequestCipher, err = EncryptImagePayload([]byte(`{}`), "media-request:"+queued.TaskId)
	require.NoError(t, err)
	require.NoError(t, model.DB.Create(&queued).Error)
	status := model.AsyncMediaJob{TaskId: "status-video", IdentityHash: "status-video", UserId: 1, Status: "submitted", StorageStatus: "pending", ExpiresAt: now + 3600}
	require.NoError(t, model.DB.Create(&status).Error)
	require.NoError(t, model.DB.Create(&model.Task{TaskID: status.TaskId, UserId: 1, Status: model.TaskStatusSuccess}).Error)
	second, err := DispatchAsyncMediaJobs(t.Context(), cfg)
	require.NoError(t, err)
	second.Wait()
	assert.EqualValues(t, 1, submissions.Load())
	assert.EqualValues(t, 1, downloads.Load())
	require.NoError(t, model.DB.First(&queued, queued.ID).Error)
	assert.Equal(t, "submitted", queued.Status)
	require.NoError(t, model.DB.First(&status, status.ID).Error)
	assert.Equal(t, "ready", status.StorageStatus, "status workers hand completed results to the download pool")
	stale := status
	stale.StorageStatus = "pending"
	claimed, err := model.ClaimAsyncMediaJob(t.Context(), &stale, "stale-phase", 30)
	require.NoError(t, err)
	assert.False(t, claimed, "a stale status candidate cannot claim a download job")
	duplicate := slow
	claimed, err = model.ClaimAsyncMediaJob(t.Context(), &duplicate, "duplicate-download", 30)
	require.NoError(t, err)
	assert.False(t, claimed, "each task retains one exclusive lease across pools")
	legacy := cfg
	legacy.SubmitConcurrency, legacy.StatusConcurrency, legacy.SubmitTimeout = 0, 0, 0
	require.NoError(t, SaveMediaRuntimeConfig(t.Context(), legacy))
	loaded, err := GetMediaRuntimeConfig(t.Context())
	require.NoError(t, err)
	assert.Equal(t, cfg.DownloadConcurrency, loaded.SubmitConcurrency)
	assert.Equal(t, DefaultMediaRuntimeConfig().StatusConcurrency, loaded.StatusConcurrency)
	assert.Equal(t, cfg.DownloadTimeout, loaded.SubmitTimeout)
	assert.Equal(t, cfg.DownloadConcurrency, loaded.DownloadConcurrency)
	// A persisted pre-upgrade JSON object has no new worker properties at all.
	require.NoError(t, model.DB.Model(&model.Option{}).Where(map[string]any{"key": "media_runtime_config"}).Update("value", `{"local_path":"data/media","retention_days":90,"signed_url_expiry_seconds":3600,"max_file_bytes":1073741824,"download_timeout_seconds":900,"download_concurrency":7,"storage_retry_attempts":5}`).Error)
	loaded, err = GetMediaRuntimeConfig(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 7, loaded.SubmitConcurrency)
	assert.Equal(t, 900, loaded.SubmitTimeout)
	assert.Equal(t, 8, loaded.StatusConcurrency)
	close(releaseDownload)
	first.Wait()
	require.NoError(t, model.DB.First(&slow, slow.ID).Error)
	assert.Equal(t, "succeeded", slow.Status)
}

func mediaTestMP4() []byte {
	box := func(kind string, payload []byte) []byte {
		result := make([]byte, 8+len(payload))
		binary.BigEndian.PutUint32(result, uint32(len(result)))
		copy(result[4:8], kind)
		copy(result[8:], payload)
		return result
	}
	handler := make([]byte, 25)
	copy(handler[8:12], "vide")
	data := box("ftyp", []byte("isom\x00\x00\x00\x00"))
	data = append(data, box("moov", box("trak", box("mdia", box("hdlr", handler))))...)
	return append(data, box("mdat", []byte("video-sample"))...)
}
