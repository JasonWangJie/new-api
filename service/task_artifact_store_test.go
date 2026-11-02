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
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
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
	require.NoError(t, db.AutoMigrate(&model.Option{}, &model.Task{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.MediaRetryEvent{}, &model.MediaArtifactObject{}))
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
	entities := []any{&model.Option{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.MediaRetryEvent{}, &model.MediaArtifactObject{}}
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

func TestAsyncMediaPollingWithoutFrozenJobPreservesUnconfirmedTask(t *testing.T) {
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
	require.NoError(t, model.DB.AutoMigrate(&model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.MediaRetryEvent{}, &model.Option{}))
	seedUser(t, 1, 1000)
	seedToken(t, 1, 1, "missing-channel-token", 1000)
	task := &model.Task{TaskID: "public-missing-job", UserId: 1, ChannelId: 99999, Quota: 400, Status: model.TaskStatusSubmitted, PrivateData: model.TaskPrivateData{AsyncMedia: true, TokenId: 1, UpstreamTaskID: "native-missing-job"}}
	require.NoError(t, model.DB.Create(task).Error)
	var pollErr error
	require.NotPanics(t, func() {
		pollErr = updateVideoTasks(t.Context(), "drama", task.ChannelId, []string{task.PrivateData.UpstreamTaskID}, map[string]*model.Task{task.PrivateData.UpstreamTaskID: task})
	})
	require.ErrorContains(t, pollErr, "CacheGetChannel failed")
	assert.Zero(t, adaptorCalls, "missing local metadata must never start an upstream call")
	require.NoError(t, model.DB.First(task, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), task.Status)
	require.NotNil(t, task.PrivateData.VideoReconciliation)
	assert.Contains(t, task.PrivateData.VideoReconciliation.Reason, "channel_unavailable")
	assert.Empty(t, task.FailReason)
	assert.Equal(t, 400, task.Quota)
	var owner model.User
	var token model.Token
	require.NoError(t, model.DB.First(&owner, 1).Error)
	require.NoError(t, model.DB.First(&token, 1).Error)
	assert.Equal(t, 1000, owner.Quota)
	assert.Zero(t, owner.UsedQuota)
	assert.Equal(t, 1000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
}

func TestVideoPoolFallbackChannelRouting(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, scenario := range []string{"other channel", "local other channel", "same channel", "fallback just failed", "disabled fallback", "key selection failed", "rule excludes fallback", "other namespace", "unknown execution"} {
			t.Run(fmt.Sprintf("mixed=%t/%s", mixed, scenario), func(t *testing.T) {
				db, imageTask, _, _, fallback := asyncImageWorkerFixture(t)
				const videoModel = "seedance2.0-E-720p"
				source, err := builtinplugins.Source("drama")
				require.NoError(t, err)
				previousRegistry := jsplugin.DefaultRegistry
				jsplugin.DefaultRegistry = jsplugin.NewRegistry()
				t.Cleanup(func() { jsplugin.DefaultRegistry = previousRegistry })
				_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "drama"})
				require.NoError(t, err)
				require.NoError(t, db.Model(&model.Channel{}).Where("id IN ?", []int{imageTask.ChannelId, fallback.Id}).Updates(map[string]any{"type": constant.ChannelTypeTaskPlugin, "models": videoModel, "setting": `{"task_plugin_key":"drama"}`}).Error)
				require.NoError(t, db.Model(&model.Ability{}).Where("model = ?", imageTask.Model).Update("model", videoModel).Error)
				regular := model.Channel{Key: "video-regular", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeTaskPlugin, Group: imageTask.Group, Models: videoModel, Setting: common.GetPointer(`{"task_plugin_key":"drama"}`)}
				require.NoError(t, db.Create(&regular).Error)
				require.NoError(t, db.Create(&model.Ability{Group: imageTask.Group, Model: videoModel, ChannelId: regular.Id, Enabled: true}).Error)
				platform := "drama"
				if mixed {
					platform = "mixed"
				}
				policy := model.ImageGroupPolicy{PolicyKey: ImageIdentityHash(imageTask.Group, platform), Group: imageTask.Group, Platform: platform, Enabled: true, AsyncEnabled: true, PoolMode: "model_resolution", Models: `[{"id":"seedance2.0-E-720p","media_type":"video"}]`}
				require.NoError(t, db.Create(&policy).Error)
				key := MediaPoolBindingKey(policy, videoModel, "1K", "video")
				bindings := []model.ImageChannelPool{
					{BindingKey: key, Group: policy.Group, Platform: platform, Mode: policy.PoolMode, Model: videoModel, Resolution: "1K", ChannelId: imageTask.ChannelId},
					{BindingKey: key, Group: policy.Group, Platform: platform, Mode: policy.PoolMode, Model: videoModel, Resolution: "1K", ChannelId: fallback.Id, IsFallback: true},
					{BindingKey: key, Group: policy.Group, Platform: platform, Mode: policy.PoolMode, Model: videoModel, Resolution: "1K", ChannelId: regular.Id, Priority: 10},
				}
				require.NoError(t, db.Create(&bindings).Error)
				job := model.AsyncMediaJob{TaskId: "video-fallback", UserId: imageTask.UserId, TokenId: imageTask.TokenId, Group: imageTask.Group, Model: videoModel, Provider: "drama", ChannelId: imageTask.ChannelId}
				request := AsyncVideoRequest{
					Body: []byte(`{"model":"seedance2.0-E-720p","prompt":"safe retry","seconds":5}`), ContentType: "application/json", Operation: "create",
					Details:   &AsyncVideoRequestDetails{RequestedResolution: "720p"},
					RetryFact: &mediaretry.Fact{ChannelID: imageTask.ChannelId, Terminal: true}, PendingDecision: &mediaretry.Decision{Action: mediaretry.OtherChannel},
				}
				if mixed {
					request.MixedRouting = &MixedVideoRouting{Group: policy.Group, BindingKey: key, PoolMode: policy.PoolMode, Model: videoModel, Operation: "create", Resolution: "1K"}
				}
				want := regular.Id
				switch scenario {
				case "other channel":
					want = fallback.Id
				case "local other channel":
					request.PendingDecision.Action = mediaretry.LocalOtherChannel
					want = fallback.Id
				case "same channel":
					request.PendingDecision.Action = mediaretry.SameChannel
					want = imageTask.ChannelId
				case "fallback just failed":
					request.RetryFact.ChannelID = fallback.Id
				case "disabled fallback":
					require.NoError(t, db.Model(&fallback).Update("status", common.ChannelStatusManuallyDisabled).Error)
				case "key selection failed":
					previousCache := common.MemoryCacheEnabled
					common.MemoryCacheEnabled = false
					t.Cleanup(func() { common.MemoryCacheEnabled = previousCache })
					require.NoError(t, db.Model(&fallback).Update("channel_info", model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling}).Error)
					const callback = "test:fallback-key-unavailable"
					require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
						if _, ok := tx.Statement.Dest.(*model.Channel); ok {
							tx.AddError(errors.New("selected key is unavailable"))
						}
					}))
					t.Cleanup(func() { assert.NoError(t, db.Callback().Query().Remove(callback)) })
				case "rule excludes fallback":
					request.PendingDecision.AllowedChannelIDs = []int{regular.Id}
				case "other namespace":
					require.NoError(t, db.Model(&bindings[1]).Update("is_fallback", false).Error)
					other := bindings[1]
					other.Id, other.IsFallback = 0, true
					other.BindingKey = ImagePoolBindingKey(policy, videoModel, "1K")
					if !mixed {
						other.Resolution = "2K"
						other.BindingKey = ImagePoolBindingKey(policy, videoModel, other.Resolution)
					}
					require.NoError(t, db.Create(&other).Error)
				case "unknown execution":
					request.RetryFact.Unknown, request.RetryFact.Terminal = true, false
					want = fallback.Id
				}
				selected, err := PickVideoRetryChannel(t.Context(), job, request, videoModel)
				if mixed && scenario == "unknown execution" {
					require.Error(t, err)
					assert.Nil(t, selected)
					return
				}
				require.NoError(t, err)
				require.NotNil(t, selected)
				assert.Equal(t, want, selected.Id)
			})
		}
	}
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
	const accessFailure = "Network Error, Generation Failed. 可能是您提供的素材存储链接无法被我们的服务访问，或受到访问限制，导致素材读取失败。请切换到我们的素材存储后再提交，具体使用方法请参阅文档。我们的素材仅存储 2 小时，并针对常见访问限制做了适配，可有效减少此类问题。"
	const busyFailure = "系统繁忙，请稍后重试"
	fixtures := []struct{ taskID, nativeID, reason string }{
		{"task_FwV2DiYrdzCducOB6afosCUjmfAPDBZu", "task_cbOfBRgeOKfwVr55TqUlglRzj6wGujFQ", "（图片1、图片2、图片3）素材下载超时，请联系管理员处理"},
		{"task_PTZmKMXaTwOZUeGry9Fhe4FL3CFKaVgH", "task_Tx94WYxNNea4pAMZD93i0AzCFZNlVGNd", "（图片1）素材下载超时，请联系管理员处理"},
		{"task_wn9d7GWnH2QFqlziTtF4XDmvg5GaSEuc", "task_Wb0fFbn2r0yyrU0F12NAnqtAA3kbIlV1", "（图片1）素材下载超时，请联系管理员处理"},
		{"task_gd54MLEovqx1TcoHvLc4OhqCLhTnObct", "task_PpDB8GnxAkBU1IRBaLcsl6j7QjxJStkk", accessFailure},
		{"task_RaD05eH47s8QouGKYpUewxrg0twZ3kQc", "task_7D0cVrqLjXIKXRgD5RCVL9CL4yUxpWai", accessFailure},
		{"task_iaBKWpln9O4V1LzqDywTbhQtAsZlrGLc", "task_WAORtCnyJnTbwEgTxj4UMfrNcJTcEogQ", accessFailure},
		{"task_CPV3DGDT8swevT3l4V1aBGLmLc6J5PUP", "task_5RS8MpltthtFophnFWiYIKnCNCEzpXvG", accessFailure},
		{"task_w5DlX8LzaeJBl8gu8asT1Viayd10GbS7", "busy-native-id", busyFailure},
	}
	type recoveryCase struct {
		outcome string
		fixture int
	}
	for _, funding := range []string{"wallet", "subscription"} {
		cases := []recoveryCase{{"success", 0}, {"second_failure", 1}, {"download_failure", 2}, {"refund_failure", 0}, {"submission_unknown", 0}, {"explicit_rejection", 0}}
		cases = append(cases, recoveryCase{"success", 7}, recoveryCase{"second_failure", 7}, recoveryCase{"submission_unknown", 7}, recoveryCase{"explicit_rejection", 7})
		if funding == "wallet" {
			cases = append(cases, recoveryCase{"success", 3}, recoveryCase{"download_failure", 5})
		} else {
			cases = append(cases, recoveryCase{"second_failure", 4}, recoveryCase{"submission_unknown", 6})
		}
		for _, test := range cases {
			outcome, fixture := test.outcome, fixtures[test.fixture]
			busy := fixture.reason == busyFailure
			t.Run(funding+"/"+outcome+"/"+fixture.taskID, func(t *testing.T) {
				setupMeasuredTaskBillingDB(t)
				asyncImageKeyFixture(t)
				extra := []any{&model.Ability{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.Option{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.MediaRetryEvent{}, &model.MediaArtifactObject{}, &model.MediaInputObject{}, &model.ImageInputAlias{}, &model.ImageInputObject{}}
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
				require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 1).Update("group", "default").Error)
				require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("group", "default").Error)
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 1).Updates(map[string]any{"type": constant.ChannelTypeTaskPlugin, "models": "seedance2.0-E-720p", "group": "default", "setting": `{"task_plugin_key":"drama"}`}).Error)
				require.NoError(t, model.DB.Create(&model.Ability{ChannelId: 1, Group: "default", Model: "seedance2.0-E-720p", Enabled: true}).Error)
				source, err := builtinplugins.Source("drama")
				require.NoError(t, err)
				previousRegistry := jsplugin.DefaultRegistry
				jsplugin.DefaultRegistry = jsplugin.NewRegistry()
				t.Cleanup(func() { jsplugin.DefaultRegistry = previousRegistry })
				_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "drama"})
				require.NoError(t, err)
				require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("expired_time", -1).Error)
				cfg := DefaultMediaRuntimeConfig()
				cfg.LocalPath, cfg.MaxFileBytes = t.TempDir(), 1<<20
				require.NoError(t, SaveMediaRuntimeConfig(t.Context(), cfg))
				now := time.Now().Unix()
				task := makeTask(1, 1, 100, 1, funding, 1)
				task.TaskID, task.Platform, task.SubmitTime = fixture.taskID, "drama", now-20
				task.Properties.OriginModelName = "seedance2.0-E-720p"
				task.PrivateData.AsyncMedia, task.PrivateData.DurableBilling, task.PrivateData.SubmitAccountingRecorded = true, true, true
				task.PrivateData.UpstreamTaskID, task.PrivateData.Key = fixture.nativeID, "sk-test"
				task.PrivateData.BillingContext.PerCallBilling = true
				require.NoError(t, model.DB.Create(task).Error)
				stale := *task
				job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: ImageIdentityHash(task.TaskID), UserId: 1, TokenId: 1, ChannelId: 1, Provider: "drama", Model: "seedance2.0-E-720p", Group: "default", Status: "submitted", StorageStatus: "pending", BillingStatus: "reserved", Quota: 100, CreatedAt: now, ExpiresAt: now + 6*86400}
				canonical := map[string]any{"model": "seedance2.0-E-720p", "prompt": "图片1 and 视频1 unchanged", "duration": 5, "references": []any{map[string]any{"type": "image", "role": "reference", "source": "https://source.example/image.png"}, map[string]any{"type": "video", "role": "reference", "source": "asset://preserved"}}}
				if busy {
					delete(canonical, "references")
					canonical["prompt"] = "A video without reference media"
				}
				var channel model.Channel
				require.NoError(t, model.DB.First(&channel, 1).Error)
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
						{"generic_network_error", string(response), "Network Error, Generation Failed.", 200},
						{"reference_ready_with_network_error", string(response), "参考素材已就绪。Network Error, Generation Failed.", 200},
						{"reference_download_succeeded", string(response), "素材下载成功但视频生成失败", 200},
						{"reference_download_succeeded_then_generation_failed", string(response), "素材下载成功后视频生成失败", 200},
						{"reference_download_succeeded_english", string(response), "Reference image downloaded successfully but video generation failed", 200},
						{"reference_download_succeeded_with_network_error", string(response), "素材下载成功后出现网络错误", 200},
						{"reference_download_succeeded_with_network_error_english", string(response), "Reference image downloaded successfully and an unrelated network error occurred", 200},
						{"reference_download_not_failed", string(response), "素材下载没有失败", 200},
						{"reference_access_not_restricted", string(response), "参考素材访问不受限制", 200},
						{"reference_source_not_expired", string(response), "Source URL is not expired", 200},
						{"reference_download_did_not_fail", string(response), "Reference image download did not fail", 200},
						{"reference_did_not_download_fail", `{"status":"failed"}`, "参考素材没有下载失败，只是渲染出错", 200},
						{"no_failure_during_reference_download", `{"status":"failed"}`, "No failure occurred while downloading the reference image; rendering failed", 200},
						{"reference_decode_failure", `{"status":"failed"}`, "Could not decode downloaded reference image", 200},
						{"reference_decoding_failure", `{"status":"failed"}`, "Reference image decoding failed after download", 200},
						{"reference_corrupt_media", `{"status":"failed"}`, "Reference image could not be read because the downloaded media is corrupt", 200},
						{"reference_corrupted_media", `{"status":"failed"}`, "Failed to read the corrupted reference image", 200},
						{"reference_damaged_media", `{"status":"failed"}`, "参考素材读取失败：文件已损坏", 200},
						{"policy_after_reference_url", `{"status":"failed"}`, "参考素材下载失败 https://source.example/image.png内容安全审核拒绝", 200},
						{"copyright_after_reference_url", `{"status":"failed"}`, "Reference image download failed https://source.example/image.png,copyright", 200},
						{"reference_url_sentence_then_rendering_failure", `{"status":"failed"}`, "Reference image download https://source.example/image.png. Renderer failed", 200},
						{"generation_connection_failure", string(response), "素材已下载成功；无法连接生成服务", 200},
						{"generated_video_download_failure", string(response), "素材已下载成功；无法下载生成视频", 200},
						{"output_download_failed", string(response), "素材下载成功；生成的视频成片下载失败", 200},
						{"invalid_reference_dimensions", string(response), "参考图下载失败：图片尺寸非法，width must be at most 1600", 200},
						{"reference_authentication_failure", string(response), "参考素材读取失败：authentication failed", 200},
						{"reference_api_key_failure", string(response), "Source media fetch failed: invalid API key", 200},
						{"ambiguous_generation_failure", string(response), "生成失败，检查上传素材是否公网可达，是否输入了被允许的参数，是否侵犯了版权保护。", 200},
						{"access_failure_with_policy_diagnostic", string(response), accessFailure + " 内容安全审核未通过", 200},
						{"http_502", string(response), fixture.reason, 502},
						{"safety", `{"status":"failed","error":{"code":"safety_violation"}}`, fixture.reason, 200},
						{"copyright_type", `{"status":"failed","error":{"type":"copyright_restriction"}}`, fixture.reason, 200},
						{"copyright_message", `{"status":"failed","error":{"message":"版权保护拒绝"}}`, fixture.reason, 200},
						{"numeric_auth_code", `{"status":"failed","error":{"code":403}}`, fixture.reason, 200},
						{"auth_type", `{"status":"failed","error":{"type":"authentication_error"}}`, fixture.reason, 200},
						{"other_native_task", `{"id":"another-native-id","status":"failed"}`, fixture.reason, 200},
						{"parameters", `{"status":"failed","error":{"message":"invalid parameter"}}`, fixture.reason, 200},
						{"success_evidence", `{"status":"failed","success":true}`, fixture.reason, 200},
						{"singular_video", `{"status":"failed","video":"data:video/mp4;base64,AAAA"}`, fixture.reason, 200},
						{"base64_output", `{"status":"failed","bytesBase64Encoded":"AAAA"}`, fixture.reason, 200},
						{"result_url", `{"status":"failed","metadata":{"url":"https://output.example/video.mp4"}}`, fixture.reason, 200},
						{"busy_with_authentication_failure", `{"status":"failed","error":{"message":"系统繁忙；鉴权失败"}}`, "系统繁忙；鉴权失败", 200},
						{"busy_with_moderation_failure", `{"status":"failed","error":{"message":"系统繁忙；内容安全审核未通过"}}`, "系统繁忙；内容安全审核未通过", 200},
						{"busy_with_parameter_failure", `{"status":"failed","error":{"message":"server busy; invalid parameter"}}`, "server busy; invalid parameter", 200},
						{"busy_with_output", `{"status":"failed","metadata":{"url":"系统繁忙，请稍后重试"},"video_url":"https://output.example/video.mp4"}`, busyFailure, 200},
						{"service_is_not_busy", `{"status":"failed","metadata":{"url":"The service is not busy; rendering failed"}}`, "The service is not busy; rendering failed", 200},
						{"service_is_not_overloaded", `{"status":"failed","metadata":{"url":"The service is not overloaded; rendering failed"}}`, "The service is not overloaded; rendering failed", 200},
						{"chinese_service_is_not_busy", `{"status":"failed","metadata":{"url":"系统并不繁忙，生成失败"}}`, "系统并不繁忙，生成失败", 200},
					} {
						t.Run(unsafe.name, func(t *testing.T) {
							scheduled, err := TryAsyncVideoReferenceRecovery(t.Context(), *task, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: unsafe.reason}, unsafe.status, []byte(unsafe.body))
							require.NoError(t, err)
							switch unsafe.name {
							case "incomplete", "other_native_task", "success_evidence", "singular_video", "base64_output", "result_url", "busy_with_output":
								assert.True(t, scheduled, "conflicting failure evidence retains the original operation for reconciliation")
							default:
								assert.False(t, scheduled)
							}
							// Each response is an independent provider observation fixture.
							require.NoError(t, model.DB.Model(task).Update("private_data", task.PrivateData).Error)
							require.NoError(t, model.DB.Model(&job).Updates(map[string]any{"status": "submitted", "next_attempt_at": 0}).Error)
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
					if fixture.reason == accessFailure {
						for _, variant := range []struct{ name, reason string }{
							{"timeout_with_new_wording", "上游提示：素材 下载 超时，请稍后处理。"},
							{"reference_access_restricted", "参考图访问受限，请切换素材存储后重试"},
							{"material_service_unreachable", "上游无法访问素材服务，请联系管理员"},
							{"quoted_resource_unreachable", "引用资源地址不可达，请更换链接"},
							{"input_image_load_failure", "输入图片加载失败，请检查素材地址"},
							{"one_reference_succeeded_another_failed", "素材1下载成功；素材2下载失败，请更换来源"},
							{"english_timeout", "Reference video download timed out; please use another media link."},
							{"english_read_failure", "Failed to READ reference image. Please try an accessible source."},
							{"english_fetch_failure", "Upstream could not fetch the INPUT_IMAGE; please retry with a public URL."},
							{"english_restricted_access", "Access to reference material is restricted. Please use public hosting."},
							{"english_contraction", "The material store couldn't be accessed; retry with a public link."},
							{"english_expired_source", "Source URL expired, please upload the reference again."},
							{"english_reference_url_timeout", "Reference image https://source.example/ref_image-1.png download timed out"},
							{"chinese_reference_url_timeout", "参考素材https://source.example/ref_image-1.png?signature=opaque_token下载超时"},
							{"reference_url_contains_processing_words", "Reference image https://source.example/generated_video/corrupted-reference.png download timed out"},
							{"reference_failure_without_error_details", "Reference image download failed with no error details"},
							{"reference_timeout_without_error_code", "Reference image download timed out with no error code"},
							{"first_reference_local_timeout", "reference 1 (image): local reference media download failed after 3 attempts: temporary reference media download failure: timeout"},
							{"second_reference_local_timeout", "reference 2 (image): local reference media download failed after 5 attempts: temporary reference media download failure: timeout"},
							{"third_reference_local_connection_failure", "reference 3 (video): local reference media download failed: connection failure"},
							{"last_reference_local_timeout", "reference 64 (audio): local reference media download timed out"},
						} {
							t.Run(variant.name, func(t *testing.T) {
								body, err := common.Marshal(map[string]any{"id": fixture.nativeID, "status": "failed", "metadata": map[string]any{"url": variant.reason}})
								require.NoError(t, err)
								handled, err := TryAsyncVideoReferenceRecovery(t.Context(), *task, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: variant.reason}, 200, body)
								require.NoError(t, err)
								assert.True(t, handled, "recognized reference failures wait for the existing receipt projection before scheduling")
							})
						}
					}
					if busy {
						for _, reason := range []string{"上游系统繁忙，请稍后再试（请求 abc123）", "服务器繁忙，请重试", "服务繁忙", "The SERVICE is BUSY, please retry later", "Generation service temporarily unavailable; retry later", "Server overloaded, please try again"} {
							t.Run(reason, func(t *testing.T) {
								body, err := common.Marshal(map[string]any{"id": fixture.nativeID, "status": "failed", "metadata": map[string]any{"url": reason}})
								require.NoError(t, err)
								handled, err := TryAsyncVideoReferenceRecovery(t.Context(), *task, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: reason}, 200, body)
								require.NoError(t, err)
								assert.True(t, handled, "busy wording variations preserve the existing reservation while its receipt projection completes")
							})
						}
					}
					var projection model.Task
					require.NoError(t, model.DB.First(&projection, task.ID).Error)
					assert.Zero(t, projection.AttemptGeneration)
					assert.Equal(t, 100, projection.Quota)
					require.NoError(t, model.DB.Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID).Updates(map[string]any{"status": "submitted", "quota": 100}).Error)
				}
				poller := &scriptedPollingAdaptor{body: response, parse: &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: fixture.reason}}
				require.NoError(t, updateVideoSingleTask(t.Context(), poller, &channel, task.TaskID, map[string]*model.Task{task.TaskID: task}))
				require.NoError(t, model.DB.First(&job, job.ID).Error)
				stage := "prepare"
				if busy {
					stage = "submit"
					assert.GreaterOrEqual(t, job.NextAttemptAt, now+5, "a busy provider must receive a bounded retry delay")
					claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, common.GetUUID(), 120)
					require.NoError(t, err)
					assert.False(t, claimed, "the retry must not run before its delay elapses")
					require.NoError(t, model.DB.Model(&job).Update("next_attempt_at", 0).Error)
				}
				require.Equal(t, stage, job.ReferenceRecoveryStage)
				require.EqualValues(t, 1, job.AttemptGeneration)
				require.NoError(t, model.DB.First(task, task.ID).Error)
				assert.EqualValues(t, model.TaskStatusQueued, task.Status)
				assert.Equal(t, 100, task.Quota)
				require.Len(t, task.PrivateData.VideoAttempts, 1)
				if busy {
					task.PrivateData.VideoAttempts[0].FinishedAt = now - 10
					require.NoError(t, model.DB.Model(task).Update("private_data", task.PrivateData).Error)
					plain, err := DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
					require.NoError(t, err)
					var waiting AsyncVideoRequest
					require.NoError(t, common.Unmarshal(plain, &waiting))
					waiting.RetryFailedAt = now - 10
					encoded, err := common.Marshal(waiting)
					require.NoError(t, err)
					cipher, err := EncryptImagePayload(encoded, "media-request:"+job.TaskId)
					require.NoError(t, err)
					require.NoError(t, model.DB.Model(&job).Update("request_cipher", cipher).Error)
				}
				assert.Equal(t, fixture.reason, task.PrivateData.VideoAttempts[0].Reason, "retain the original diagnostic without relabeling it as a timeout")
				scheduled, err := TryAsyncVideoReferenceRecovery(t.Context(), *task, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: fixture.reason}, 200, response)
				require.NoError(t, err)
				assert.False(t, scheduled, "a replacement generation cannot schedule another reference recovery")
				// A duplicate old poll cannot settle or refund the new generation.
				stale.Status, stale.FailReason = model.TaskStatusFailure, fixture.reason
				won, err := SettleTaskBillingOnComplete(t.Context(), nil, &stale, model.TaskStatusInProgress, relaycommon.FailTaskInfo(fixture.reason))
				require.NoError(t, err)
				assert.False(t, won)
				inputPath := filepath.Join(cfg.LocalPath, "cached.png")
				image := asyncImageBytesFixture(t)
				if !busy {
					require.NoError(t, os.WriteFile(inputPath, image.Data, 0600))
					_, err = model.SaveMediaInputObject(t.Context(), model.MediaInputObject{ObjectId: "minput_cached", TaskId: job.TaskId, Slot: "0", Kind: "image", SourceHash: ImageIdentityHash("https://source.example/image.png"), UserId: 1, TokenId: 1, RootPath: cfg.LocalPath, ObjectKey: "cached.png", MimeType: "image/png", ByteSize: int64(len(image.Data)), Checksum: fmt.Sprintf("%x", sha256.Sum256(image.Data)), Status: "active", CreatedAt: now, ExpiresAt: request.ReferenceURLExpiresAt})
					require.NoError(t, err)
				}
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
					if !busy {
						refs := changed["references"].([]any)
						assert.Contains(t, refs[0].(map[string]any)["source"], "/api/media-inputs/minput_cached/content?")
						assert.Equal(t, "asset://preserved", refs[1].(map[string]any)["source"])
						refs[0].(map[string]any)["source"] = "https://source.example/image.png"
					} else {
						assert.Empty(t, prepared.ReferenceInputs)
					}
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
				phases := 2
				if busy {
					phases = 1
				}
				for attempt := range phases {
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

func TestVideoRecoveryRechecksLivePolicyWithoutResettingUse(t *testing.T) {
	t.Run("initial legacy planner uses typed evidence and live controls", func(t *testing.T) {
		for _, tc := range []struct {
			name, want string
			change     string
		}{
			{name: "captured exception", want: mediaretry.SameChannel},
			{name: "message alone cannot grant replay", change: "no evidence"},
			{name: "new channel cannot inherit exception", change: "new channel"},
			{name: "already replayed", change: "used"},
			{name: "pause applies", change: "pause"},
			{name: "legacy switch applies", change: "disabled"},
			{name: "captured deadline applies", change: "deadline"},
			{name: "global zero disables", change: "budget"},
			{name: "custom unknown rule explicitly selects another channel", change: "custom", want: mediaretry.OtherChannel},
		} {
			t.Run(tc.name, func(t *testing.T) {
				policy := mediaretry.Policy{Revision: 3, LegacyTimeoutEnabled: true, LegacyVideoChannelIDs: []int{1}, LegacyTimeoutConfig: map[string]int{"video_submission_timeout_seconds": 120}}
				fact := mediaretry.Fact{MediaType: "video", Provider: "drama", ChannelID: 1, Stage: "submit", Message: "task submission state is unknown", Unknown: true, Deadline: 1000, VideoEvidence: &mediaretry.VideoFailureEvidence{LegacySubmissionRejected: true, SubmissionStartedAt: 100}}
				state := mediaretry.State{Now: 110, TotalLimit: 16}
				switch tc.change {
				case "no evidence":
					fact.VideoEvidence = nil
				case "new channel":
					fact.ChannelID = 2
				case "used":
					fact.VideoEvidence.LegacySubmissionUsed = true
				case "pause":
					policy.PauseResubmissions = true
				case "disabled":
					policy.LegacyTimeoutEnabled = false
				case "deadline":
					state.Now = 219
				case "budget":
					state.TotalLimit = 0
				case "custom":
					policy.Rules = []mediaretry.Rule{{ID: "unknown", Name: "Unknown response", Enabled: true, Match: mediaretry.Match{ContainsAny: []string{"unknown"}}, Steps: []mediaretry.Step{{ID: "send", Action: mediaretry.OtherChannel, MaxAttempts: 1}}}}
				}
				decision, err := PlanInitialVideoFailure(policy, fact, state)
				require.NoError(t, err)
				assert.Equal(t, tc.want, decision.Action)
				assert.EqualValues(t, 3, decision.Revision)
			})
		}
	})
	t.Run("legacy wait retains exact recorded status", func(t *testing.T) {
		fact := mediaretry.Fact{MediaType: "video", Provider: "drama", Model: "model", Stage: "terminal", HTTPStatus: 200, ErrorCode: "provider-specific", ChannelID: 9, Terminal: true, Accepted: true, Message: "server busy"}
		task := model.Task{ChannelId: 10, PrivateData: model.TaskPrivateData{MediaRetryFact: &fact, MediaRetryFailedAt: 123}}
		request := AsyncVideoRequest{}
		RestoreVideoRetryFact(task, model.AsyncMediaJob{Provider: "drama", Model: "model"}, &request)
		require.NotNil(t, request.RetryFact)
		assert.Equal(t, 200, request.RetryFact.HTTPStatus)
		assert.Equal(t, "provider-specific", request.RetryFact.ErrorCode)
		assert.Equal(t, 9, request.RetryFact.ChannelID)
		assert.EqualValues(t, 123, request.RetryFailedAt)
	})
	t.Run("unpausing a poll rule retains its HTTP status", func(t *testing.T) {
		setupMeasuredTaskBillingDB(t)
		asyncImageKeyFixture(t)
		extra := []any{&model.Option{}, &model.MediaRetryPolicyVersion{}}
		require.NoError(t, model.DB.AutoMigrate(extra...))
		t.Cleanup(func() {
			for _, row := range extra {
				assert.NoError(t, model.DB.Where("1 = 1").Delete(row).Error)
			}
		})
		task := makeTask(1, 1, 100, 1, "wallet", 0)
		task.TaskID, task.Platform = "task-paused-poll-rule", "drama"
		task.PrivateData.AsyncMedia, task.PrivateData.DurableBilling, task.PrivateData.SubmitAccountingRecorded = true, true, true
		task.PrivateData.UpstreamTaskID = "native-original"
		require.NoError(t, model.DB.Create(task).Error)
		now := time.Now().Unix()
		request := AsyncVideoRequest{Body: []byte(`{"model":"test-model","prompt":"retry"}`), ReferenceRecoveryVersion: 2, ReferenceRecoveryDeadline: now + 3600}
		encoded, err := common.Marshal(request)
		require.NoError(t, err)
		cipher, err := EncryptImagePayload(encoded, "media-request:"+task.TaskID)
		require.NoError(t, err)
		job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: ImageIdentityHash(task.TaskID), UserId: 1, TokenId: 1, ChannelId: 1, Provider: "drama", Model: "test-model", Group: "default", Status: "submitted", BillingStatus: "reserved", Quota: 100, RequestCipher: cipher, CreatedAt: now, ExpiresAt: now + 86400}
		require.NoError(t, model.DB.Create(&job).Error)
		policy, err := model.GetMediaRetryPolicy(t.Context())
		require.NoError(t, err)
		policy.PauseResubmissions = true
		policy.Rules = []mediaretry.Rule{{ID: "poll-503", Name: "Retry polling failure", Enabled: true, Scope: mediaretry.Scope{MediaTypes: []string{"video"}, Stages: []string{"poll"}}, Match: mediaretry.Match{HTTPStatuses: []int{503}, ErrorCodes: []string{"upstream_result_unknown"}}, Steps: []mediaretry.Step{{ID: "switch", Action: mediaretry.OtherChannel, MaxAttempts: 1}}}}
		policy, err = model.SaveMediaRetryPolicy(t.Context(), policy, policy.Revision, 1, nil)
		require.NoError(t, err)
		require.NoError(t, reconcileAsyncVideoTask(t.Context(), task, task.Snapshot(), "upstream_error", 503))
		require.NoError(t, model.DB.First(task, task.ID).Error)
		require.NotNil(t, task.PrivateData.VideoReconciliation)
		assert.True(t, task.PrivateData.VideoReconciliation.ManualRequired)
		require.NotNil(t, task.PrivateData.MediaRetryFact)
		assert.Equal(t, 503, task.PrivateData.MediaRetryFact.HTTPStatus)
		assert.Equal(t, "resubmissions_paused", task.PrivateData.MediaRetryDecision.BlockedReason)
		policy.PauseResubmissions = false
		_, err = model.SaveMediaRetryPolicy(t.Context(), policy, policy.Revision, 1, nil)
		require.NoError(t, err)
		ready, err := asyncVideoPollReady(t.Context(), task)
		require.NoError(t, err)
		assert.False(t, ready, "the configured replacement is queued instead of polling the retired job")
		assert.EqualValues(t, 1, task.AttemptGeneration)
		assert.EqualValues(t, model.TaskStatusQueued, task.Status)
		require.NotNil(t, task.PrivateData.MediaRetryFact)
		assert.Equal(t, 503, task.PrivateData.MediaRetryFact.HTTPStatus)
		assert.Equal(t, mediaretry.OtherChannel, task.PrivateData.MediaRetryDecision.Action)
		require.NoError(t, model.DB.First(&job, job.ID).Error)
		assert.Equal(t, "queued", job.Status)
		assert.Equal(t, "reserved", job.BillingStatus)
		assert.Equal(t, 100, job.Quota)
		assert.Empty(t, task.PrivateData.MediaRetryState.StepUses, "waiting does not consume the unsent action")
	})
	job, _, _ := mediaInputFixture(t)
	require.NoError(t, model.DB.AutoMigrate(&model.MediaRetryPolicyVersion{}, &model.MediaRetryEvent{}))
	t.Cleanup(func() {
		assert.NoError(t, model.DB.Where(map[string]any{"key": model.MediaRetryPolicyOptionKey}).Delete(&model.Option{}).Error)
		assert.NoError(t, model.DB.Where("1 = 1").Delete(&model.MediaRetryPolicyVersion{}).Error)
	})
	policy, err := model.GetMediaRetryPolicy(t.Context())
	require.NoError(t, err)
	policy.Rules = []mediaretry.Rule{{ID: "busy", Name: "Busy", Enabled: true, Scope: mediaretry.Scope{MediaTypes: []string{"video"}}, Match: mediaretry.Match{ContainsAny: []string{"busy"}}, Steps: []mediaretry.Step{{ID: "first", Action: mediaretry.SameChannel, MaxAttempts: 1}, {ID: "next", Action: mediaretry.OtherChannel, MaxAttempts: 2}}, OnExhausted: mediaretry.ManualReview}}
	policy, err = model.SaveMediaRetryPolicy(t.Context(), policy, policy.Revision, 1, nil)
	require.NoError(t, err)
	fact := mediaretry.Fact{MediaType: "video", Provider: "xai", Model: "video", ChannelID: 1, Stage: "terminal", Message: "busy", Accepted: true, Terminal: true, HasUpstreamTask: true, HasReferences: true, LocalSupported: true, Deadline: time.Now().Unix() + 600}
	task := model.Task{AttemptGeneration: 1, PrivateData: model.TaskPrivateData{MediaRetryState: &mediaretry.State{TotalUsed: 1, TotalLimit: 16, RuleUses: map[string]int{"busy": 1}, StepUses: map[string]int{"busy/first": 1}}}}
	request := AsyncVideoRequest{RetryFact: &fact, ReferenceRecoveryDeadline: fact.Deadline}
	decision, err := RecheckVideoRecoveryPolicy(t.Context(), job, task, request)
	require.NoError(t, err)
	assert.Equal(t, mediaretry.OtherChannel, decision.Action)
	policy.Rules[0].Steps[1].Action = mediaretry.LocalOtherChannel
	policy, err = model.SaveMediaRetryPolicy(t.Context(), policy, policy.Revision, 1, nil)
	require.NoError(t, err)
	decision, err = RecheckVideoRecoveryPolicy(t.Context(), job, task, request)
	require.NoError(t, err)
	assert.Equal(t, policy.Revision, decision.Revision)
	assert.Equal(t, mediaretry.LocalOtherChannel, decision.Action)
	assert.Equal(t, 1, task.PrivateData.MediaRetryState.TotalUsed)
	policy.PauseResubmissions = true
	policy, err = model.SaveMediaRetryPolicy(t.Context(), policy, policy.Revision, 1, nil)
	require.NoError(t, err)
	_, err = RecheckVideoRecoveryPolicy(t.Context(), job, task, request)
	assert.ErrorIs(t, err, ErrVideoRetryDeferred)
	policy.PauseResubmissions = false
	_, err = model.SaveMediaRetryPolicy(t.Context(), policy, policy.Revision, 1, nil)
	require.NoError(t, err)
	fact.Unknown = true
	decision, err = RecheckVideoRecoveryPolicy(t.Context(), job, task, request)
	require.NoError(t, err)
	assert.Equal(t, mediaretry.LocalOtherChannel, decision.Action)
	assert.True(t, fact.Unknown, "explicit recovery does not rewrite protocol evidence")
	fact.Unknown = false
	fact.HasOutput = true
	decision, err = RecheckVideoRecoveryPolicy(t.Context(), job, task, request)
	require.Error(t, err)
	assert.Equal(t, mediaretry.ResumeDownload, decision.Action)
}

func TestTerminalVideoBuiltInPlanRequiresRecordedEvidence(t *testing.T) {
	base := mediaretry.Fact{MediaType: "video", Provider: "drama", Category: "capacity", Stage: "terminal", Message: "server busy", Accepted: true, Terminal: true, HasUpstreamTask: true, Deadline: 2000, VideoEvidence: &mediaretry.VideoFailureEvidence{LegacyBusyEligible: true}}
	for _, tc := range []struct {
		name            string
		evidence, pause bool
		now             int64
		used            int
		action          string
	}{
		{name: "recorded terminal permits built-in", evidence: true, now: 1000, action: mediaretry.SameChannel},
		{name: "keywords do not create evidence", now: 1000},
		{name: "pause guards built-in", evidence: true, pause: true, now: 1000, action: mediaretry.ManualReview},
		{name: "original deadline remains fixed", evidence: true, now: 2000},
		{name: "built-in remains one attempt", evidence: true, now: 1000, used: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fact := base
			if !tc.evidence {
				fact.VideoEvidence = nil
			}
			decision, err := PlanTerminalVideoFailure(mediaretry.Policy{PauseResubmissions: tc.pause}, fact, mediaretry.State{Now: tc.now, TotalUsed: tc.used, TotalLimit: 16})
			require.NoError(t, err)
			assert.Equal(t, tc.action, decision.Action)
			if tc.action == mediaretry.SameChannel {
				assert.Equal(t, "builtin-drama-busy", decision.RuleID)
				assert.Equal(t, 5, decision.DelaySeconds)
			}
		})
	}
}

func TestMixedVideoStopPausesPollingAndFencesStaleResults(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}, &model.MediaRetryPolicyVersion{}))
	t.Cleanup(func() {
		assert.NoError(t, model.DB.Where("1 = 1").Delete(&model.Option{}).Error)
		assert.NoError(t, model.DB.Where("1 = 1").Delete(&model.MediaRetryPolicyVersion{}).Error)
	})
	seedUser(t, 1, 900)
	seedToken(t, 1, 1, "mixed-stop-token", 900)
	seedChannel(t, 1)
	seedChargedAccounting(t, 1, 1, 1, 100, 1)
	task := makeTask(1, 1, 100, 1, "wallet", 0)
	task.TaskID, task.Platform = "task-mixed-stop", "mixed-provider"
	task.PrivateData.AsyncMedia, task.PrivateData.DurableBilling, task.PrivateData.SubmitAccountingRecorded = true, true, true
	task.PrivateData.UpstreamTaskID = "native-still-running"
	task.PrivateData.CurrentAttempt = &model.VideoTaskAttempt{MixedVideo: true, ChannelID: 1, PluginKey: "mixed-provider"}
	require.NoError(t, model.DB.Create(task).Error)
	stale := *task
	now := time.Now().Unix()
	job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: ImageIdentityHash(task.TaskID), UserId: 1, TokenId: 1, ChannelId: 1, Provider: "mixed-provider", Model: "test-model", Group: "default", Status: "submitted", BillingStatus: "reserved", Quota: 100, CreatedAt: now, ExpiresAt: now + 3600, DispatchedAt: now}
	require.NoError(t, model.DB.Create(&job).Error)
	policy, err := model.GetMediaRetryPolicy(t.Context())
	require.NoError(t, err)
	policy.Rules = []mediaretry.Rule{{ID: "stop-unknown", Name: "Stop unknown", Enabled: true, OnExhausted: mediaretry.Stop, Scope: mediaretry.Scope{MediaTypes: []string{"video"}}, Match: mediaretry.Match{ContainsAny: []string{"busy"}}, Steps: []mediaretry.Step{{ID: "stop", Action: mediaretry.Stop, MaxAttempts: 1}}}}
	_, err = model.SaveMediaRetryPolicy(t.Context(), policy, policy.Revision, 1, nil)
	require.NoError(t, err)
	_, err = videoPollingDecision(t.Context(), task, "server busy", 503, false)
	require.NoError(t, err)
	require.NoError(t, model.DB.First(&job, job.ID).Error)
	assert.Equal(t, "execution_unknown", job.Status)
	assert.Equal(t, "unknown", job.BillingStatus)
	assert.Equal(t, 100, task.Quota)
	generation := task.AttemptGeneration
	ready, err := asyncVideoPollReady(t.Context(), task)
	require.NoError(t, err)
	assert.False(t, ready, "Stop keeps the mixed operation paused for explicit reconciliation")
	_, err = videoPollingDecision(t.Context(), task, "server busy", 503, false)
	require.NoError(t, err)
	assert.Equal(t, generation, task.AttemptGeneration, "a repeated stop does not create another generation")
	fromStatus := stale.Status
	stale.Status = model.TaskStatusSuccess
	won, _, err := model.SettleTaskBilling(t.Context(), &stale, fromStatus, 100, nil, "")
	require.NoError(t, err)
	assert.False(t, won, "the stopped generation cannot publish a late upstream success")
	require.NoError(t, ReconcileTerminatedAsyncMediaReservation(t.Context(), job))
	require.NoError(t, model.DB.First(&job, job.ID).Error)
	assert.Equal(t, "failed", job.Status)
	assert.Equal(t, "refunded", job.BillingStatus)
	var owner model.User
	require.NoError(t, model.DB.First(&owner, 1).Error)
	assert.Equal(t, 1000, owner.Quota, "explicit reconciliation also handles a retained native task ID")
	assert.Error(t, ReconcileTerminatedAsyncMediaReservation(t.Context(), job))
}

func TestConfiguredVideoRecoveryRetainsConflictingResult(t *testing.T) {
	for _, tc := range []struct {
		name, response, parsedURL            string
		output, replayUnavailable            bool
		capabilityMissing, accountingPending bool
		stop, poll, replacement, unknownJob  bool
	}{
		{name: "raw output URL", response: `{"id":"native-original","status":"failed","video_url":"https://cdn.example/video.mp4"}`, output: true},
		{name: "unavailable replay still retains output", response: `{"status":"failed","video_url":"https://cdn.example/video.mp4"}`, output: true, replayUnavailable: true},
		{name: "raw inline output", response: `{"status":"failed","bytesBase64Encoded":"AAAA"}`, output: true},
		{name: "parsed output URL", response: `{"status":"failed"}`, parsedURL: "https://cdn.example/video.mp4", output: true},
		{name: "accepted status", response: `{"status":"in_progress"}`},
		{name: "different native receipt", response: `{"id":"other-native","status":"failed"}`},
		{name: "non-string native receipt", response: `{"id":123,"status":"failed"}`},
		{name: "unknown stop retains its hold", response: `{"status":"queued"}`, stop: true},
		{name: "poll unknown schedules another generation", poll: true},
		{name: "poll unknown stop retains its hold", poll: true, stop: true},
		{name: "replacement unknown schedules another generation", replacement: true},
		{name: "replacement unknown stop retains its hold", replacement: true, stop: true},
		{name: "persisted unknown hold schedules another generation", response: `{"status":"queued"}`, unknownJob: true},
		{name: "missing replay capability records blocked stop", response: `{"id":"native-original","status":"failed"}`, capabilityMissing: true},
		{name: "submission accounting precedes capability stop", response: `{"id":"native-original","status":"failed"}`, capabilityMissing: true, accountingPending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupMeasuredTaskBillingDB(t)
			asyncImageKeyFixture(t)
			extra := []any{&model.Option{}, &model.MediaRetryPolicyVersion{}}
			require.NoError(t, model.DB.AutoMigrate(extra...))
			t.Cleanup(func() {
				for _, row := range extra {
					assert.NoError(t, model.DB.Where("1 = 1").Delete(row).Error)
				}
			})
			seedUser(t, 1, 900)
			seedToken(t, 1, 1, "video-conflict-token", 900)
			seedChannel(t, 1)
			seedChargedAccounting(t, 1, 1, 1, 100, 1)
			task := makeTask(1, 1, 100, 1, "wallet", 0)
			task.TaskID, task.Platform = "task-conflicting-response", "drama"
			task.PrivateData.AsyncMedia, task.PrivateData.DurableBilling, task.PrivateData.SubmitAccountingRecorded = true, true, true
			task.PrivateData.UpstreamTaskID = "native-original"
			if tc.replacement {
				task.Status, task.PrivateData.UpstreamTaskID = model.TaskStatusQueued, ""
			}
			if tc.unknownJob {
				task.Status = model.TaskStatusUnknown
			}
			if tc.accountingPending {
				task.PrivateData.SubmitAccountingRecorded = false
			}
			require.NoError(t, model.DB.Create(task).Error)
			stale := *task
			now := time.Now().Unix()
			request := AsyncVideoRequest{Body: []byte(`{"model":"test-model","prompt":"safe retry"}`), ReferenceRecoveryVersion: 2, ReferenceRecoveryDeadline: now + 3600}
			if tc.capabilityMissing {
				request.ReferenceRecoveryVersion = 0
			}
			encoded, err := common.Marshal(request)
			require.NoError(t, err)
			cipher, err := EncryptImagePayload(encoded, "media-request:"+task.TaskID)
			require.NoError(t, err)
			if tc.replayUnavailable {
				cipher = []byte("unavailable-encrypted-request")
			}
			job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: ImageIdentityHash(task.TaskID), UserId: 1, TokenId: 1, ChannelId: 1, Provider: "drama", Model: "test-model", Group: "default", Status: "submitted", BillingStatus: "reserved", Quota: 100, RequestCipher: cipher, CreatedAt: now, ExpiresAt: now + 86400}
			if tc.replacement {
				job.Status, job.ReferenceRecoveryStage, job.DispatchedAt = "queued", "dispatched", now-10
			}
			if tc.unknownJob {
				job.Status, job.BillingStatus, job.ReferenceRecoveryStage = "execution_unknown", "unknown", "unknown"
			}
			require.NoError(t, model.DB.Create(&job).Error)
			policy, err := model.GetMediaRetryPolicy(t.Context())
			require.NoError(t, err)
			policy.Rules = []mediaretry.Rule{{ID: "busy-retry", Name: "Retry busy", Enabled: true, Scope: mediaretry.Scope{MediaTypes: []string{"video"}}, Match: mediaretry.Match{ContainsAny: []string{"busy"}}, Steps: []mediaretry.Step{{ID: "switch", Action: mediaretry.OtherChannel, MaxAttempts: 2}}}}
			if tc.stop {
				policy.Rules[0].Steps[0].Action = mediaretry.Stop
			}
			_, err = model.SaveMediaRetryPolicy(t.Context(), policy, policy.Revision, 1, nil)
			require.NoError(t, err)
			result := &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: "server busy", Url: tc.parsedURL}
			var handled bool
			if tc.poll {
				_, err = videoPollingDecision(t.Context(), task, "server busy", 503, false)
				handled = true
			} else if tc.replacement {
				handled, err = tryUnknownVideoRecovery(t.Context(), *task, job, "server busy")
			} else {
				handled, err = TryAsyncVideoReferenceRecovery(t.Context(), *task, result, 200, []byte(tc.response))
			}
			require.NoError(t, err)
			require.NoError(t, model.DB.First(task, task.ID).Error)
			if tc.capabilityMissing {
				assert.Zero(t, task.AttemptGeneration)
				assert.Equal(t, 100, task.Quota)
				if tc.accountingPending {
					assert.True(t, handled, "the original submit projection keeps its hold before replay capability is decided")
					assert.Nil(t, task.PrivateData.MediaRetryDecision, "incomplete submit accounting cannot publish a stop decision")
					var owner model.User
					require.NoError(t, model.DB.First(&owner, 1).Error)
					assert.Equal(t, 900, owner.Quota)
					return
				}
				assert.False(t, handled, "confirmed terminal failure can continue to its ordinary refund")
				require.NotNil(t, task.PrivateData.MediaRetryDecision)
				assert.Equal(t, mediaretry.Stop, task.PrivateData.MediaRetryDecision.Action)
				assert.Equal(t, "request_replay_unavailable", task.PrivateData.MediaRetryDecision.BlockedReason)
				var event model.MediaRetryEvent
				require.NoError(t, model.DB.Where("task_id = ?", task.TaskID).Take(&event).Error)
				assert.Equal(t, mediaretry.Stop, event.Action)
				assert.False(t, event.Executed)
				var recorded mediaretry.Decision
				require.NoError(t, common.UnmarshalJsonStr(event.DecisionJSON, &recorded))
				assert.Equal(t, "request_replay_unavailable", recorded.BlockedReason)
				assert.Equal(t, mediaretry.OtherChannel, recorded.RequestedAction)
				failed := *task
				failed.Status = model.TaskStatusFailure
				won, err := SettleTaskBillingOnComplete(t.Context(), nil, &failed, task.Status, relaycommon.FailTaskInfo("confirmed failure"))
				require.NoError(t, err)
				assert.True(t, won)
				won, err = SettleTaskBillingOnComplete(t.Context(), nil, &failed, task.Status, relaycommon.FailTaskInfo("confirmed failure"))
				require.NoError(t, err)
				assert.False(t, won, "a blocked retry does not duplicate its final refund")
				var owner model.User
				require.NoError(t, model.DB.First(&owner, 1).Error)
				assert.Equal(t, 1000, owner.Quota)
				return
			}
			require.True(t, handled)
			require.NotNil(t, task.PrivateData.MediaRetryFact)
			assert.Equal(t, tc.output, task.PrivateData.MediaRetryFact.HasOutput)
			assert.True(t, task.PrivateData.MediaRetryFact.Unknown)
			assert.False(t, task.PrivateData.MediaRetryFact.Terminal)
			if !tc.output {
				assert.EqualValues(t, 1, task.AttemptGeneration)
				assert.Equal(t, 100, task.Quota)
				require.NoError(t, model.DB.First(&job, job.ID).Error)
				if tc.stop {
					assert.EqualValues(t, model.TaskStatusFailure, task.Status)
					assert.Equal(t, "failed", job.Status)
					assert.Equal(t, "unknown", job.BillingStatus)
					assert.Zero(t, job.NextAttemptAt)
					assert.False(t, task.PrivateData.VideoTerminalConfirmed)
				} else {
					assert.EqualValues(t, model.TaskStatusQueued, task.Status)
					assert.Empty(t, task.PrivateData.UpstreamTaskID)
					assert.Equal(t, "queued", job.Status)
					assert.Equal(t, "reserved", job.BillingStatus)
					assert.Equal(t, 1, job.ReferenceRetryCount)
					require.NotEmpty(t, task.PrivateData.VideoAttempts)
					assert.Equal(t, model.TaskStatusUnknown, task.PrivateData.VideoAttempts[len(task.PrivateData.VideoAttempts)-1].Status)
				}
				stale.Status = model.TaskStatusFailure
				won, staleErr := SettleTaskBillingOnComplete(t.Context(), nil, &stale, model.TaskStatusInProgress, relaycommon.FailTaskInfo("stale failure"))
				require.NoError(t, staleErr)
				assert.False(t, won, "the retired generation cannot refund the retained hold")
				var owner model.User
				require.NoError(t, model.DB.First(&owner, 1).Error)
				assert.Equal(t, 900, owner.Quota)
				if !tc.stop {
					fromStatus := task.Status
					task.Status, task.FailReason = model.TaskStatusFailure, "replacement failed"
					task.PrivateData.VideoTerminalConfirmed = true
					won, settleErr := SettleTaskBillingOnComplete(t.Context(), nil, task, fromStatus, relaycommon.FailTaskInfo(task.FailReason))
					require.NoError(t, settleErr)
					assert.True(t, won)
					require.NoError(t, model.DB.First(&job, job.ID).Error)
					assert.Equal(t, "unknown", job.BillingStatus, "a failed replacement cannot confirm an earlier unknown generation")
					assert.Equal(t, 100, task.Quota)
				}
				require.NoError(t, ReconcileTerminatedAsyncMediaReservation(t.Context(), job))
				require.NoError(t, model.DB.First(&owner, 1).Error)
				assert.Equal(t, 1000, owner.Quota, "explicit upstream verification can release the original hold once")
				assert.Error(t, ReconcileTerminatedAsyncMediaReservation(t.Context(), job))
				return
			}
			assert.False(t, mediaretry.IsResubmission(task.PrivateData.MediaRetryDecision.Action))
			preview, err := PlanTerminalVideoFailure(policy, *task.PrivateData.MediaRetryFact, videoRetryState(*task, job))
			require.NoError(t, err)
			assert.Equal(t, task.PrivateData.MediaRetryDecision.Action, preview.Action, "preview and execution use the same conflict decision")
			assert.Zero(t, task.AttemptGeneration)
			assert.Equal(t, "native-original", task.PrivateData.UpstreamTaskID)
			assert.Equal(t, 100, task.Quota)
			require.NoError(t, model.DB.First(&job, job.ID).Error)
			assert.Equal(t, "reserved", job.BillingStatus)
			assert.Zero(t, job.ReferenceRetryCount)
			if tc.output {
				failed := *task
				failed.Status = model.TaskStatusFailure
				won, _, err := model.SettleTaskBilling(t.Context(), &failed, task.Status, 0, nil, "")
				require.ErrorContains(t, err, "confirmed video output")
				assert.False(t, won, "raw confirmed output cannot be refunded by a later contradictory failure")
			}
			var owner model.User
			require.NoError(t, model.DB.First(&owner, 1).Error)
			assert.Equal(t, 900, owner.Quota)
			assert.Equal(t, 100, owner.UsedQuota)
			assert.Equal(t, 1, owner.RequestCount)
		})
	}
}
