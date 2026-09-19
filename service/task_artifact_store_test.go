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
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
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
	for _, billing := range []string{"pending", "reserving"} {
		uncertain := model.AsyncMediaJob{TaskId: "uncertain-" + billing, IdentityHash: "uncertain-" + billing, UserId: 1, Status: "submitting", BillingStatus: billing, ExpiresAt: time.Now().Unix() + 3600}
		if billing == "pending" {
			uncertain.DispatchedAt = time.Now().Unix()
		}
		require.NoError(t, db.Create(&uncertain).Error)
		claimed, err := model.ClaimAsyncMediaJob(t.Context(), &uncertain, common.GetUUID(), 30)
		require.NoError(t, err)
		require.True(t, claimed)
		require.NoError(t, ProcessAsyncMediaJob(t.Context(), uncertain, cfg))
		require.NoError(t, db.Where("id = ?", uncertain.ID).Take(&uncertain).Error)
		assert.Equal(t, "execution_unknown", uncertain.Status)
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
