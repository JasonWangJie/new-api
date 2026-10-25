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
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
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

func (a *scriptedPollingAdaptor) FetchTaskContext(ctx context.Context, base, key string, task *model.Task, proxy string) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a.FetchTask(base, key, task, proxy)
}

func TestAsyncMediaPollingWithoutFrozenJobUsesMissingChannelFailure(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	previousCache, previousFactory := common.MemoryCacheEnabled, GetTaskAdaptorFunc
	common.MemoryCacheEnabled = false
	adaptorCalls := 0
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		adaptorCalls++
		return &scriptedPollingAdaptor{}
	}
	t.Cleanup(func() {
		common.MemoryCacheEnabled, GetTaskAdaptorFunc = previousCache, previousFactory
	})
	require.NoError(t, model.DB.AutoMigrate(&model.AsyncMediaJob{}))
	seedUser(t, 1, 1000)
	seedToken(t, 1, 1, "missing-channel-token", 1000)
	task := &model.Task{TaskID: "public-missing-job", UserId: 1, ChannelId: 99999, Status: model.TaskStatusSubmitted, PrivateData: model.TaskPrivateData{AsyncMedia: true, TokenId: 1, UpstreamTaskID: "native-missing-job"}}
	require.NoError(t, model.DB.Create(task).Error)
	var pollErr error
	require.NotPanics(t, func() {
		pollErr = updateVideoTasks(t.Context(), "drama", task.ChannelId, []string{task.PrivateData.UpstreamTaskID}, map[string]*model.Task{task.PrivateData.UpstreamTaskID: task})
	})
	require.ErrorContains(t, pollErr, "CacheGetChannel failed")
	assert.Zero(t, adaptorCalls, "missing local metadata must never start an upstream call")
	require.NoError(t, model.DB.First(task, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), task.Status)
	assert.Contains(t, task.FailReason, "Failed to get channel info")
	assert.Zero(t, task.Quota)
	var owner model.User
	var token model.Token
	require.NoError(t, model.DB.First(&owner, 1).Error)
	require.NoError(t, model.DB.First(&token, 1).Error)
	assert.Equal(t, 1000, owner.Quota)
	assert.Zero(t, owner.UsedQuota)
	assert.Equal(t, 1000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
}

func TestAsyncVideoReferenceRecoverySnapshotWindow(t *testing.T) {
	oldTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 60
	t.Cleanup(func() { constant.TaskTimeoutMinutes = oldTimeout })
	for _, test := range []struct {
		name        string
		ttl         any
		retention   int64
		lifetime    int64
		wantEnabled bool
	}{
		{"explicit_upstream_ttl", 3600, 7 * 86400, 3600, true},
		{"unspecified_ttl_conservative_window", nil, 7 * 86400, 259200, true},
		{"insufficient_retention", 3600, 3600, 3600, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().Unix()
			body := map[string]any{"model": "seedance2.0-A", "prompt": "图片1 视频1 图片2", "references": []any{
				map[string]any{"type": "image", "role": "first_frame", "source": "https://source.example/first.png"},
				map[string]any{"type": "video", "role": "reference", "source": "https://source.example/main.mp4"},
				map[string]any{"type": "image", "role": "last_frame", "source": "https://source.example/last.png"},
			}}
			if test.ttl != nil {
				body["execution_expires_after"] = test.ttl
			}
			request := AsyncVideoRequest{Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "drama"}}}
			require.NoError(t, FreezeAsyncVideoReferenceRecovery(&request, body, now+test.retention))
			if !test.wantEnabled {
				assert.Zero(t, request.ReferenceRecoveryVersion)
				return
			}
			require.Equal(t, 1, request.ReferenceRecoveryVersion)
			assert.InDelta(t, now+3600, request.ReferenceRecoveryDeadline, 1)
			assert.Equal(t, request.ReferenceRecoveryDeadline+test.lifetime+3600, request.ReferenceURLExpiresAt, "even a last-moment replacement has its full upstream TTL plus grace")
			assert.LessOrEqual(t, request.ReferenceURLExpiresAt, now+7*86400)
			require.Len(t, request.ReferenceInputs, 3)
			assert.Equal(t, []int{1, 1, 2}, []int{request.ReferenceInputs[0].KindIndex, request.ReferenceInputs[1].KindIndex, request.ReferenceInputs[2].KindIndex})
			assert.Equal(t, "first_frame", request.ReferenceInputs[0].Role)
			assert.Equal(t, "last_frame", request.ReferenceInputs[2].Role)
			original, err := common.Marshal(body)
			require.NoError(t, err)
			assert.JSONEq(t, string(original), string(request.Body))
		})
	}
}

func TestAsyncVideoReferenceRecoveryLifecycle(t *testing.T) {
	// These are fixed, sanitized diagnostic fixtures from already-refunded
	// operations. Tests create new local rows; historical tasks are never replayed.
	fixtures := []struct{ taskID, nativeID, reason string }{
		{"task_FwV2DiYrdzCducOB6afosCUjmfAPDBZu", "task_cbOfBRgeOKfwVr55TqUlglRzj6wGujFQ", "（图片1、图片2、图片3）素材下载超时，请联系管理员处理"},
		{"task_PTZmKMXaTwOZUeGry9Fhe4FL3CFKaVgH", "task_Tx94WYxNNea4pAMZD93i0AzCFZNlVGNd", "（图片1）素材下载超时，请联系管理员处理"},
		{"task_wn9d7GWnH2QFqlziTtF4XDmvg5GaSEuc", "task_Wb0fFbn2r0yyrU0F12NAnqtAA3kbIlV1", "（图片1）素材下载超时，请联系管理员处理"},
	}
	for _, funding := range []string{"wallet", "subscription"} {
		for _, outcome := range []string{"success", "second_failure", "download_failure", "refund_failure", "submission_unknown", "explicit_rejection"} {
			t.Run(funding+"/"+outcome, func(t *testing.T) {
				setupMeasuredTaskBillingDB(t)
				asyncImageKeyFixture(t)
				extra := []any{&model.Option{}, &model.AsyncMediaJob{}, &model.MediaArtifactObject{}, &model.MediaInputObject{}, &model.ImageInputAlias{}, &model.ImageInputObject{}}
				for range 2 {
					require.NoError(t, model.DB.AutoMigrate(extra...))
				}
				t.Cleanup(func() {
					for _, row := range extra {
						assert.NoError(t, model.DB.Where("1 = 1").Delete(row).Error)
					}
				})
				oldSecret, oldBase, oldTimeout, oldSubmit := common.CryptoSecret, system_setting.TaskPublicAddress, constant.TaskTimeoutMinutes, SubmitAsyncVideoReferenceRetryFunc
				common.CryptoSecret, system_setting.TaskPublicAddress, constant.TaskTimeoutMinutes = "video-recovery-local-test", "https://gateway.example", 60
				t.Cleanup(func() {
					common.CryptoSecret, system_setting.TaskPublicAddress, constant.TaskTimeoutMinutes, SubmitAsyncVideoReferenceRetryFunc = oldSecret, oldBase, oldTimeout, oldSubmit
				})
				wallet := 900
				if funding == "subscription" {
					wallet = 1000
					seedSubscription(t, 1, 1, 1000, 100)
				}
				seedUser(t, 1, wallet)
				seedToken(t, 1, 1, "test-video-key", 900)
				seedChannel(t, 1)
				seedChargedAccounting(t, 1, 1, 1, 100, 1)
				require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("expired_time", -1).Error)
				cfg := DefaultMediaRuntimeConfig()
				cfg.LocalPath, cfg.MaxFileBytes = t.TempDir(), 1<<20
				require.NoError(t, SaveMediaRuntimeConfig(t.Context(), cfg))
				now := time.Now().Unix()
				fixture := fixtures[0]
				if outcome == "second_failure" {
					fixture = fixtures[1]
				}
				if outcome == "download_failure" {
					fixture = fixtures[2]
				}
				task := makeTask(1, 1, 100, 1, funding, 1)
				task.TaskID, task.Platform, task.SubmitTime = fixture.taskID, "drama", now-20
				task.PrivateData.AsyncMedia, task.PrivateData.DurableBilling, task.PrivateData.SubmitAccountingRecorded = true, true, true
				task.PrivateData.UpstreamTaskID, task.PrivateData.Key = fixture.nativeID, "sk-test"
				task.PrivateData.BillingContext.PerCallBilling = true
				require.NoError(t, model.DB.Create(task).Error)
				stale := *task
				job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: ImageIdentityHash(task.TaskID), UserId: 1, TokenId: 1, ChannelId: 1, Provider: "drama", Model: "test-model", Status: "submitted", StorageStatus: "pending", BillingStatus: "reserved", Quota: 100, CreatedAt: now, ExpiresAt: now + 6*86400}
				canonical := map[string]any{"model": "test-model", "prompt": "图片1 and 视频1 unchanged", "duration": 5, "references": []any{map[string]any{"type": "image", "role": "reference", "source": "https://source.example/image.png"}, map[string]any{"type": "video", "role": "reference", "source": "asset://preserved"}}}
				channel := model.Channel{Id: 1, Key: "sk-test", Status: common.ChannelStatusEnabled}
				request := AsyncVideoRequest{Channel: channel, Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "drama", Version: "test"}}}
				require.NoError(t, FreezeAsyncVideoReferenceRecovery(&request, canonical, job.ExpiresAt))
				require.Equal(t, 1, request.ReferenceRecoveryVersion)
				request.Details = request.RequestDetails()
				encoded, err := common.Marshal(request)
				require.NoError(t, err)
				job.RequestCipher, err = EncryptImagePayload(encoded, "media-request:"+job.TaskId)
				require.NoError(t, err)
				encoded, err = common.Marshal(channel)
				require.NoError(t, err)
				job.ChannelCipher, err = EncryptImagePayload(encoded, "media-channel:"+job.TaskId)
				require.NoError(t, err)
				require.NoError(t, model.DB.Create(&job).Error)
				response, err := common.Marshal(map[string]any{"id": fixture.nativeID, "status": "failed", "metadata": map[string]any{"url": fixture.reason}})
				require.NoError(t, err)
				if funding == "wallet" && outcome == "success" {
					originalCipher := append([]byte(nil), job.RequestCipher...)
					for _, unsafe := range []struct {
						name, body, reason string
						status             int
					}{
						{"incomplete", `{"status":"failed"`, fixture.reason, 200},
						{"poll_timeout", string(response), "upstream request timeout", 200},
						{"http_502", string(response), fixture.reason, 502},
						{"safety", `{"status":"failed","error":{"code":"safety_violation"}}`, fixture.reason, 200},
						{"numeric_auth_code", `{"status":"failed","error":{"code":403}}`, fixture.reason, 200},
						{"auth_type", `{"status":"failed","error":{"type":"authentication_error"}}`, fixture.reason, 200},
						{"other_native_task", `{"id":"another-native-id","status":"failed"}`, fixture.reason, 200},
						{"parameters", `{"status":"failed","error":{"message":"invalid parameter"}}`, fixture.reason, 200},
						{"success_evidence", `{"status":"failed","success":true}`, fixture.reason, 200},
						{"singular_video", `{"status":"failed","video":"data:video/mp4;base64,AAAA"}`, fixture.reason, 200},
						{"base64_output", `{"status":"failed","bytesBase64Encoded":"AAAA"}`, fixture.reason, 200},
						{"result_url", `{"status":"failed","metadata":{"url":"https://output.example/video.mp4"}}`, fixture.reason, 200},
					} {
						t.Run(unsafe.name, func(t *testing.T) {
							scheduled, err := TryAsyncVideoReferenceRecovery(t.Context(), *task, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: unsafe.reason}, unsafe.status, []byte(unsafe.body))
							require.NoError(t, err)
							assert.False(t, scheduled)
						})
					}
					require.NoError(t, model.DB.Model(&job).Update("billing_status", "refunded").Error)
					scheduled, err := TryAsyncVideoReferenceRecovery(t.Context(), *task, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: fixture.reason}, 200, response)
					require.NoError(t, err)
					assert.False(t, scheduled, "a refunded task cannot reuse its former reservation")
					require.NoError(t, model.DB.Model(&job).Update("billing_status", "reserved").Error)
					legacy, err := EncryptImagePayload([]byte(`{"request_details":{"prompt":"legacy"}}`), "media-request:"+job.TaskId)
					require.NoError(t, err)
					require.NoError(t, model.DB.Model(&job).Update("request_cipher", legacy).Error)
					scheduled, err = TryAsyncVideoReferenceRecovery(t.Context(), *task, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: fixture.reason}, 200, response)
					require.NoError(t, err)
					assert.False(t, scheduled, "display-only legacy snapshots are not replayable")
					require.NoError(t, model.DB.Model(&job).Update("request_cipher", originalCipher).Error)
					require.NoError(t, model.DB.Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID).Updates(map[string]any{"status": "submitting", "quota": 80}).Error)
					handled, err := TryAsyncVideoReferenceRecovery(t.Context(), *task, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: fixture.reason}, 200, response)
					require.NoError(t, err)
					assert.True(t, handled, "the submit receipt projection must finish before recovery or refund")
					var projection model.Task
					require.NoError(t, model.DB.First(&projection, task.ID).Error)
					assert.Zero(t, projection.AttemptGeneration)
					assert.Equal(t, 100, projection.Quota)
					require.NoError(t, model.DB.Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID).Updates(map[string]any{"status": "submitted", "quota": 100}).Error)
				}
				poller := &scriptedPollingAdaptor{body: response, parse: &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: fixture.reason}}
				require.NoError(t, updateVideoSingleTask(t.Context(), poller, &channel, task.TaskID, map[string]*model.Task{task.TaskID: task}))
				require.NoError(t, model.DB.First(&job, job.ID).Error)
				require.Equal(t, "prepare", job.ReferenceRecoveryStage)
				require.EqualValues(t, 1, job.AttemptGeneration)
				require.NoError(t, model.DB.First(task, task.ID).Error)
				assert.EqualValues(t, model.TaskStatusQueued, task.Status)
				assert.Equal(t, 100, task.Quota)
				// A duplicate old poll cannot settle or refund the new generation.
				stale.Status, stale.FailReason = model.TaskStatusFailure, fixture.reason
				won, err := SettleTaskBillingOnComplete(t.Context(), nil, &stale, model.TaskStatusInProgress, relaycommon.FailTaskInfo(fixture.reason))
				require.NoError(t, err)
				assert.False(t, won)
				image := asyncImageBytesFixture(t)
				inputPath := filepath.Join(cfg.LocalPath, "cached.png")
				require.NoError(t, os.WriteFile(inputPath, image.Data, 0600))
				_, err = model.SaveMediaInputObject(t.Context(), model.MediaInputObject{ObjectId: "minput_cached", TaskId: job.TaskId, Slot: "0", Kind: "image", SourceHash: ImageIdentityHash("https://source.example/image.png"), UserId: 1, TokenId: 1, RootPath: cfg.LocalPath, ObjectKey: "cached.png", MimeType: "image/png", ByteSize: int64(len(image.Data)), Checksum: fmt.Sprintf("%x", sha256.Sum256(image.Data)), Status: "active", CreatedAt: now, ExpiresAt: request.ReferenceURLExpiresAt})
				require.NoError(t, err)
				if outcome == "download_failure" || outcome == "refund_failure" {
					require.NoError(t, os.Remove(inputPath))
				}
				if outcome == "refund_failure" {
					// A broken accounting row makes the refund transaction fail.
					// Repairing it must resume only the refund, even if the source
					// becomes available before the worker runs again.
					require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("used_quota", 0).Error)
				}
				submissions := 0
				SubmitAsyncVideoReferenceRetryFunc = func(ctx context.Context, current model.AsyncMediaJob, active model.Task, prepared AsyncVideoRequest) error {
					submissions++
					var changed map[string]any
					require.NoError(t, common.Unmarshal(prepared.Body, &changed))
					refs := changed["references"].([]any)
					assert.Contains(t, refs[0].(map[string]any)["source"], "/api/media-inputs/minput_cached/content?")
					assert.Equal(t, "asset://preserved", refs[1].(map[string]any)["source"])
					refs[0].(map[string]any)["source"] = "https://source.example/image.png"
					original, err := common.Marshal(canonical)
					require.NoError(t, err)
					actual, err := common.Marshal(changed)
					require.NoError(t, err)
					assert.JSONEq(t, string(original), string(actual))
					require.NoError(t, model.UpdateAsyncMediaJob(ctx, current, map[string]any{"reference_recovery_stage": "dispatched", "dispatched_at": time.Now().Unix()}))
					if outcome == "submission_unknown" {
						return errors.New("connection closed after write")
					}
					if outcome == "explicit_rejection" {
						return &AsyncVideoReferenceRejectedError{Reason: "明确拒绝生成"}
					}
					won, err := model.CommitAsyncVideoReferenceSubmission(ctx, current, active, "replacement-native", []byte(`{"status":"queued"}`), nil)
					require.True(t, won)
					return err
				}
				for attempt := range 2 {
					require.NoError(t, model.DB.First(&job, job.ID).Error)
					if job.Status == "failed" || job.Status == "execution_unknown" {
						break
					}
					claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, common.GetUUID(), 120)
					require.NoError(t, err)
					require.True(t, claimed)
					processErr := ProcessAsyncMediaJob(t.Context(), job, cfg)
					if outcome == "refund_failure" && attempt == 0 {
						require.Error(t, processErr)
						require.NoError(t, model.DB.First(&job, job.ID).Error)
						assert.Equal(t, "refund", job.ReferenceRecoveryStage)
						assert.Equal(t, "reserved", job.BillingStatus, "a failed refund must not claim that funds were returned")
						assert.Contains(t, job.ErrorMessage, "reference 1", "retain the actual preparation failure")
						assert.Zero(t, submissions)
						require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("used_quota", 100).Error)
						require.NoError(t, os.WriteFile(inputPath, image.Data, 0600))
						require.NoError(t, model.DB.Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID).Updates(map[string]any{"next_attempt_at": 0, "expires_at": time.Now().Unix() - 1}).Error)
						require.NoError(t, CleanupMediaObjects(t.Context()))
						require.NoError(t, model.DB.First(&job, job.ID).Error)
						assert.Equal(t, "queued", job.Status, "expiry must not remove a pending refund from its queue")
						assert.Empty(t, job.RequestCipher)
						assert.Empty(t, job.ChannelCipher)
						require.NoError(t, updateVideoSingleTask(t.Context(), poller, &channel, task.TaskID, map[string]*model.Task{task.TaskID: task}), "pending financial recovery must not need channel decryption or polling")
						continue
					}
					if outcome == "download_failure" || outcome == "refund_failure" || outcome == "submission_unknown" || outcome == "explicit_rejection" {
						if processErr != nil {
							assert.Error(t, processErr)
						}
					} else {
						require.NoError(t, processErr)
					}
				}
				require.NoError(t, model.DB.First(task, task.ID).Error)
				if outcome == "success" || outcome == "second_failure" {
					terminal := model.TaskStatus(model.TaskStatusSuccess)
					if outcome == "second_failure" {
						terminal = model.TaskStatusFailure
					}
					task.Status, task.FinishTime = terminal, now
					result := &relaycommon.TaskInfo{Status: string(terminal), Reason: fixture.reason}
					won, err := SettleTaskBillingOnComplete(t.Context(), nil, task, model.TaskStatusSubmitted, result)
					require.NoError(t, err)
					require.True(t, won)
					won, err = SettleTaskBillingOnComplete(t.Context(), nil, task, model.TaskStatusSubmitted, result)
					require.NoError(t, err)
					assert.False(t, won)
				}
				if outcome == "submission_unknown" {
					require.EqualValues(t, model.TaskStatusUnknown, task.Status)
					require.NoError(t, model.DB.Model(task).Update("submit_time", now-7200).Error)
					sweepTimedOutTasks(t.Context())
					require.NoError(t, model.DB.First(task, task.ID).Error)
					assert.EqualValues(t, model.TaskStatusUnknown, task.Status, "timeout sweep must preserve an unknown replacement hold")
					require.NoError(t, model.DB.First(&job, job.ID).Error)
					assert.Equal(t, "execution_unknown", job.Status)
					require.NoError(t, ProcessAsyncMediaJob(t.Context(), job, cfg))
				}
				refunded := outcome == "second_failure" || outcome == "download_failure" || outcome == "refund_failure" || outcome == "explicit_rejection"
				var user model.User
				var token model.Token
				var ch model.Channel
				require.NoError(t, model.DB.First(&user, 1).Error)
				require.NoError(t, model.DB.First(&token, 1).Error)
				require.NoError(t, model.DB.First(&ch, 1).Error)
				used, remain := 100, 900
				if refunded {
					used, remain = 0, 1000
				}
				assert.Equal(t, used, user.UsedQuota)
				assert.Equal(t, 1, user.RequestCount)
				assert.Equal(t, used, token.UsedQuota)
				assert.Equal(t, remain, token.RemainQuota)
				assert.EqualValues(t, used, ch.UsedQuota)
				if funding == "wallet" {
					assert.Equal(t, remain, user.Quota)
				} else {
					assert.Equal(t, 1000, user.Quota)
					var subscription model.UserSubscription
					require.NoError(t, model.DB.First(&subscription, 1).Error)
					assert.EqualValues(t, used, subscription.AmountUsed)
				}
				var refunds int64
				require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("type = ?", model.LogTypeRefund).Count(&refunds).Error)
				if refunded {
					assert.EqualValues(t, 1, refunds)
				} else {
					assert.Zero(t, refunds)
				}
				if outcome == "download_failure" || outcome == "refund_failure" {
					assert.Zero(t, submissions)
				} else {
					assert.Equal(t, 1, submissions)
				}
			})
		}
	}
}
