package relay

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestUpstreamImageResultReadRetriesExistingTask(t *testing.T) {
	service.InitHttpClient()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "12")
		_, _ = w.Write([]byte("partial"))
	}))
	defer upstream.Close()
	profile := &dto.UpstreamAsyncProfile{}
	profile.Poll.Response.DownloadHeaders = map[string]string{"Authorization": "Bearer {api_key}"}
	cfg := service.DefaultImageRuntimeConfig()
	_, err := service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + "/image"}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
	require.ErrorIs(t, err, service.ErrUpstreamAsyncImageTransport)

	failure := classifyAsyncImageOutputFailure(model.AsyncImageTask{UpstreamTaskId: "vendor-job-1"}, err)
	assert.Equal(t, 606, failure.Code)
	assert.Equal(t, "result_download_failed", failure.InternalCode)
	assert.False(t, failure.ExecutionUnknown)
	assert.Equal(t, 607, classifyAsyncImageOutputFailure(model.AsyncImageTask{}, err).Code)
}

func TestRecordAsyncImageUpstreamJobAfterDispatch(t *testing.T) {
	previousDB := model.DB
	var dialector gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "upstream-job.db"))
	if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_MYSQL_DSN"); dsn != "" {
		require.Contains(t, dsn, "/new_api_image_job_test", "use a dedicated disposable database")
		dialector = mysql.Open(dsn)
	} else if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_PG_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "dbname=new_api_image_job_test") || strings.Contains(dsn, "/new_api_image_job_test"), "use a dedicated disposable database")
		dialector = postgres.Open(dsn)
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		assert.NoError(t, db.Migrator().DropTable(&model.AsyncImageEvent{}, &model.AsyncImageTask{}))
		connection, openErr := db.DB()
		if openErr == nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}))
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}))

	now := time.Now().Unix()
	task := model.AsyncImageTask{
		TaskId:         "asyncimg_stale_after_dispatch",
		Status:         model.ImageTaskInvoking,
		Version:        1,
		LeaseToken:     "worker-lease",
		LeaseExpiresAt: now + 120,
		CreatedAt:      now,
	}
	require.NoError(t, db.Create(&task).Error)
	require.NoError(t, model.MarkAsyncImageDispatched(t.Context(), task, 20))

	require.NoError(t, recordAsyncImageUpstreamJob(t.Context(), &task, "vendor-job-1"))
	assert.Equal(t, "vendor-job-1", task.UpstreamTaskId)
	assert.EqualValues(t, 3, task.Version)
	require.NoError(t, recordAsyncImageUpstreamJob(t.Context(), &task, "vendor-job-1"))
	assert.EqualValues(t, 3, task.Version)
	require.ErrorIs(t, recordAsyncImageUpstreamJob(t.Context(), &task, "vendor-job-2"), model.ErrImageConflict)

	var stored model.AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&stored).Error)
	assert.Equal(t, "vendor-job-1", stored.UpstreamTaskId)
	var accepted int64
	require.NoError(t, db.Model(&model.AsyncImageEvent{}).Where("task_id = ? AND event_type = ?", task.TaskId, "upstream_accepted").Count(&accepted).Error)
	assert.EqualValues(t, 1, accepted)
}

func TestBuildGeminiAsyncImagePartsReferenceTransport(t *testing.T) {
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "gemini-reference-transport.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		connection, openErr := db.DB()
		if openErr == nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.ImageInputAlias{}))

	t.Run("passes external URL to upstream before fallback", func(t *testing.T) {
		cfg := service.DefaultImageRuntimeConfig()
		cfg.GeminiReferenceMode = "passthrough_fallback_local"
		referenceURL := "https://signed.example/reference.jpg?token=private"
		request := service.AsyncImageRequest{Parts: []service.AsyncImageInputPart{
			{Type: "text", Text: "edit"},
			{Type: "image_url", URL: referenceURL},
		}}

		parts, err := buildGeminiAsyncImageParts(t.Context(), model.AsyncImageTask{TokenId: 7}, request, cfg)
		require.NoError(t, err)
		require.Len(t, parts, 2)
		require.NotNil(t, parts[1].FileData)
		assert.Equal(t, referenceURL, parts[1].FileData.FileUri)
		assert.Empty(t, parts[1].FileData.MimeType)
		assert.Nil(t, parts[1].InlineData)
	})

	t.Run("indexes invalid locally transported reference without URL", func(t *testing.T) {
		cfg := service.DefaultImageRuntimeConfig()
		cfg.GeminiReferenceMode = "local"
		request := service.AsyncImageRequest{Parts: []service.AsyncImageInputPart{
			{Type: "text", Text: "edit"},
			{Type: "image_url", URL: "data:image/jpeg;base64,bm90LWltYWdl"},
		}}

		_, err := buildGeminiAsyncImageParts(t.Context(), model.AsyncImageTask{TokenId: 7}, request, cfg)
		require.Error(t, err)
		var failure *service.AsyncImageFailure
		require.ErrorAs(t, err, &failure)
		assert.Equal(t, 604, failure.Code)
		assert.Equal(t, "invalid_reference_image", failure.InternalCode)
		assert.Equal(t, "Reference image 1: invalid image header", failure.Message)
		assert.NotContains(t, failure.Message, "bm90LWltYWdl")
	})
}
