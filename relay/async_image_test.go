package relay

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAsyncImageUnreadableReceiptDoesNotRepeatSubmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		limit     int64
		readError bool
	}{
		{name: "truncated_json", body: `{"id":"accepted-job"`, limit: 1024},
		{name: "non_json", body: `<html>gateway error</html>`, limit: 1024},
		{name: "oversized_receipt", body: `{"id":"accepted-job"}`, limit: 4},
		{name: "read_failure", limit: 1024, readError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, upstreamID := range []string{"", "accepted-job"} {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Set("async_image_resume_task", upstreamID)
				c.Set("async_image_response_limit", tc.limit)
				var reader io.Reader = strings.NewReader(tc.body)
				if tc.readError {
					reader = iotest.ErrReader(io.ErrUnexpectedEOF)
				}
				response := &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(reader)}
				_, apiErr := handleUpstreamAsyncImageResponse(c, &relaycommon.RelayInfo{}, response, &dto.UpstreamAsyncProfile{})
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, apiErr, &failure)
				if upstreamID == "" {
					assert.Equal(t, 608, failure.Code)
					assert.Equal(t, "execution_unknown", failure.InternalCode)
					assert.True(t, failure.ExecutionUnknown, "an unreadable accepted submission must never be retried")
				} else {
					assert.Equal(t, 606, failure.Code)
					assert.False(t, failure.ExecutionUnknown, "known upstream jobs can safely retry polling")
				}
			}
		})
	}
}

func TestGeminiAsyncImageCustomSubmitAndResume(t *testing.T) {
	previousDB, previousCount, previousRedis := model.DB, constant.CountToken, common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "gemini-submit.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled = db, false, false
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.RedisEnabled = previousDB, previousCount, previousRedis
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}))
	user := model.User{Username: "gemini-submit-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "gemini-submit-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	service.InitHttpClient()
	for _, tc := range []struct {
		name    string
		body    map[string]any
		invalid bool
	}{
		{name: "template", body: map[string]any{"prompt": "{request.prompt}", "n": "{request.n}"}},
		{name: "native_body"},
		{name: "conflicting_count", body: map[string]any{"n": float64(2)}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type recordedRequest struct{ method, path, auth, query, body string }
			received := make(chan recordedRequest, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				received <- recordedRequest{r.Method, r.URL.Path, r.Header.Get("X-Custom-Key"), r.URL.Query().Get("model"), string(body)}
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"status":"running"}`))
					return
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"id":"vendor-job"}`))
			}))
			defer upstream.Close()
			var config dto.UpstreamAsyncConfig
			require.NoError(t, common.UnmarshalJsonStr(`{"profiles":[{"id":"custom","media_type":"image","models":["gemini-image-test"],"operations":["generate"],"submit":{"task_id_path":"id","request":{"method":"POST","path":"/custom/jobs","headers":{"X-Custom-Key":"{api_key}"},"query":{"model":"{upstream_model}"}}},"poll":{"request":{"method":"GET","path":"/tasks/{task_id}"},"response":{"status_path":"status","status_values":{"in_progress":["running"],"succeeded":["done"],"failed":["failed"]},"result_path":"urls"}}}]}`, &config))
			config.Profiles[0].Submit.Request.Body = tc.body
			require.NoError(t, config.Validate())
			channel := &model.Channel{Id: 1, Key: "upstream-key", BaseURL: &upstream.URL, Type: constant.ChannelTypeGemini, Status: common.ChannelStatusEnabled, Models: "gemini-image-test", Group: "default"}
			channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamAsync: &config})
			request := service.AsyncImageRequest{Platform: "gemini", Model: "gemini-image-test", Prompt: "draw", Count: 1, Parts: []service.AsyncImageInputPart{{Type: "text", Text: "draw"}}, Billing: &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}}
			task := model.AsyncImageTask{TaskId: "gemini-submit-" + tc.name, UserId: user.Id, TokenId: token.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120}
			require.NoError(t, db.Create(&task).Error)
			dispatches := 0
			_, _, err := ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error {
				dispatches++
				return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
			})
			if tc.invalid {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, http.StatusBadRequest, failure.HTTPStatus)
				assert.Zero(t, dispatches)
				assert.Empty(t, received)
				return
			}
			var pending *service.AsyncImagePending
			require.ErrorAs(t, err, &pending)
			assert.Equal(t, 1, dispatches)
			record := <-received
			assert.Equal(t, http.MethodPost, record.method)
			assert.Equal(t, "/custom/jobs", record.path)
			assert.Equal(t, "upstream-key", record.auth)
			assert.Equal(t, request.Model, record.query)
			if tc.body != nil {
				assert.JSONEq(t, `{"prompt":"draw","n":1}`, record.body)
			} else {
				assert.Contains(t, record.body, `"contents"`)
			}
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			assert.Equal(t, "vendor-job", task.UpstreamTaskId)
			_, _, err = ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error { dispatches++; return nil })
			require.ErrorAs(t, err, &pending)
			assert.Equal(t, 1, dispatches, "polling must not cross the submission barrier again")
			record = <-received
			assert.Equal(t, http.MethodGet, record.method)
			assert.Equal(t, "/tasks/vendor-job", record.path)
		})
	}
}

func TestAsyncImageSettlementPreservesPromptExtension(t *testing.T) {
	previousDB, previousQuotaUnit := model.DB, common.QuotaPerUnit
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "ali-image-bill.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, common.QuotaPerUnit = db, 500000
	t.Cleanup(func() {
		model.DB, common.QuotaPerUnit = previousDB, previousQuotaUnit
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}))
	user := model.User{Username: "ali-image-bill-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	for _, tc := range []struct {
		name             string
		extend, fallback bool
		want             int
	}{
		{name: "local_extended", extend: true, want: 10000},
		{name: "fallback_extended", extend: true, fallback: true, want: 10000},
		{name: "local_disabled", want: 5000},
		{name: "fallback_disabled", fallback: true, want: 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations_async", nil)
			images := []service.ImageBytes{{Width: 1024, Height: 1024}}
			if tc.fallback {
				c.Set("async_image_result_source", "upstream")
				images[0] = service.ImageBytes{}
			}
			info := &relaycommon.RelayInfo{UserId: user.Id, OriginModelName: "z-image", ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeAli, UpstreamModelName: "z-image"}, StartTime: time.Now(), Request: &dto.ImageRequest{}, PriceData: types.PriceData{UsePrice: true, ModelPrice: 0.01, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
			require.Nil(t, service.EstimateImageBillingForRequest(info, 2, tc.extend))
			assert.Equal(t, 2*tc.want, info.PriceData.QuotaToPreConsume)
			bill, err := service.FixAsyncImageBill(c, model.AsyncImageTask{TaskId: "ali-bill-" + tc.name, UserId: user.Id, TokenId: 1, ChannelId: 1}, info, &dto.Usage{}, images, service.AsyncImageRequest{Platform: "openai", Model: "z-image", Count: 2}, model.ImageFundingSelection{Source: "wallet"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, bill.Quota, "settle actual image count using the captured prompt-extension multiplier")
		})
	}
}

func TestUpstreamImageResultReadRetriesExistingTask(t *testing.T) {
	service.InitHttpClient()
	var pngBytes bytes.Buffer
	require.NoError(t, png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	redirectAuthorization := make(chan string, 1)
	privateRedirects := make(chan struct{}, 1)
	privateDestination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		privateRedirects <- struct{}{}
	}))
	defer privateDestination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/partial":
			w.Header().Set("Content-Length", "12")
			_, _ = w.Write([]byte("partial"))
		case "/busy":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/redirect":
			http.Redirect(w, request, "/image", http.StatusFound)
		case "/redirect-private":
			http.Redirect(w, request, privateDestination.URL, http.StatusFound)
		case "/image":
			redirectAuthorization <- request.Header.Get("Authorization")
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes.Bytes())
		}
	}))
	defer upstream.Close()
	profile := &dto.UpstreamAsyncProfile{}
	profile.Poll.Response.DownloadHeaders = map[string]string{"Authorization": "Bearer {api_key}"}
	cfg := service.DefaultImageRuntimeConfig()
	for _, path := range []string{"/partial", "/busy", "/unavailable"} {
		_, err := service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + path}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
		require.ErrorIs(t, err, service.ErrUpstreamAsyncImageTransport, path)
		failure := classifyAsyncImageOutputFailure(model.AsyncImageTask{UpstreamTaskId: "vendor-job-1"}, err)
		assert.Equal(t, 606, failure.Code, path)
		assert.Equal(t, "result_download_failed", failure.InternalCode, path)
		assert.False(t, failure.ExecutionUnknown, path)
		assert.Equal(t, 607, classifyAsyncImageOutputFailure(model.AsyncImageTask{}, err).Code, path)
	}
	_, err := service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + "/missing"}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
	require.Error(t, err)
	assert.NotErrorIs(t, err, service.ErrUpstreamAsyncImageTransport)
	images, err := service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + "/redirect"}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
	require.NoError(t, err)
	require.Len(t, images, 1)
	assert.Equal(t, "Bearer test-key", <-redirectAuthorization)
	_, err = service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + "/redirect-private"}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
	require.ErrorContains(t, err, "not public")
	assert.Empty(t, privateRedirects)
}

func TestRecordAsyncImageUpstreamJobAfterDispatch(t *testing.T) {
	previousDB, previousType := model.DB, common.MainDatabaseType()
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
	common.SetMainDatabaseType(common.DatabaseType(db.Dialector.Name()))
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
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
