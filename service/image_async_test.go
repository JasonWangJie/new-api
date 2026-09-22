package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func asyncImageBytesFixture(t *testing.T) ImageBytes {
	t.Helper()
	bitmap := image.NewRGBA(image.Rect(0, 0, 3, 2))
	bitmap.Set(0, 0, color.RGBA{R: 255, A: 255})
	var output bytes.Buffer
	require.NoError(t, png.Encode(&output, bitmap))
	valid, err := ValidateImageBytes(output.Bytes(), "image/png", 1<<20, 100)
	require.NoError(t, err)
	return valid
}

func TestAsyncImageKnownUpstreamPollReleasesLeaseAndStaysProcessing(t *testing.T) {
	ctx := context.Background()
	previousDB, previousRedis := model.DB, common.RDB
	dialector := gorm.Dialector(sqlite.Open(filepath.Join(t.TempDir(), "polling.db")))
	databaseVersionQuery := "SELECT sqlite_version()"
	if dsn := os.Getenv("IMAGE_WORKER_TEST_MYSQL_DSN"); dsn != "" {
		require.Contains(t, dsn, "/new_api_image_worker_test", "use the dedicated disposable image worker database")
		dialector = mysql.Open(dsn)
		databaseVersionQuery = "SELECT VERSION()"
	} else if dsn := os.Getenv("IMAGE_WORKER_TEST_PG_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "dbname=new_api_image_worker_test") || strings.Contains(dsn, "/new_api_image_worker_test"), "use the dedicated disposable image worker database")
		dialector = postgres.Open(dsn)
		databaseVersionQuery = "SELECT VERSION()"
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	common.RDB = client
	t.Setenv("ASYNC_IMAGE_REDIS_PREFIX", "image-poll-test")
	t.Cleanup(func() {
		model.DB, common.RDB = previousDB, previousRedis
		_ = client.Close()
		assert.NoError(t, db.Migrator().DropTable(&model.ImageOutbox{}, &model.AsyncImageEvent{}, &model.AsyncImageTask{}))
		sqlDB, openErr := db.DB()
		if openErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}))
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}))
	var databaseVersion string
	require.NoError(t, db.Raw(databaseVersionQuery).Scan(&databaseVersion).Error)
	t.Logf("Database version: %s", databaseVersion)

	now := time.Now().Unix()
	task := model.AsyncImageTask{
		TaskId:         "asyncimg_known_upstream_poll",
		Status:         model.ImageTaskInvoking,
		Version:        1,
		UpstreamTaskId: "upstream-job-1",
		ChannelId:      7,
		DispatchedAt:   now - 5,
		StartedAt:      now - 5,
		NextAttemptAt:  now,
		LeaseToken:     "active-worker",
		LeaseExpiresAt: now + 120,
		CreatedAt:      now - 5,
	}
	require.NoError(t, db.Create(&task).Error)
	due := now + 10
	require.NoError(t, model.ScheduleAsyncImagePoll(ctx, task, due, "upstream_pending", ""))

	var scheduled model.AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&scheduled).Error)
	assert.Equal(t, model.ImageTaskInvoking, scheduled.Status)
	assert.Equal(t, model.ImageTaskInvoking, scheduled.DisplayStatus(), "an accepted upstream job is processing, not waiting for generation capacity")
	assert.Empty(t, scheduled.LeaseToken)
	assert.Zero(t, scheduled.LeaseExpiresAt, "the completed worker turn must not block the next poll for the full lease TTL")
	assert.Equal(t, task.ChannelId, scheduled.ChannelId)
	assert.Equal(t, task.DispatchedAt, scheduled.DispatchedAt)
	scheduledVersion := scheduled.Version
	require.NoError(t, model.RecoverAsyncImageTasks(ctx, 100, 1200))
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&scheduled).Error)
	assert.Equal(t, scheduledVersion, scheduled.Version, "an intentionally idle poll must not be treated as a crashed invocation")
	assert.Equal(t, due, scheduled.NextAttemptAt)

	var command model.ImageOutbox
	require.NoError(t, db.Where("aggregate_id = ? AND kind = ?", task.TaskId, "execute").Take(&command).Error)
	assert.Equal(t, due, command.NextAttemptAt, "the outbox must not deliver a delayed poll before the task is due")
	require.NoError(t, deliverAsyncImageOutbox(ctx))
	count, err := client.ZCard(ctx, ImageRedisKey("queue")).Result()
	require.NoError(t, err)
	assert.Zero(t, count)

	require.NoError(t, db.Model(&model.ImageOutbox{}).Where("id = ?", command.Id).Update("next_attempt_at", now).Error)
	require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Update("next_attempt_at", now).Error)
	require.NoError(t, deliverAsyncImageOutbox(ctx))
	score, err := client.ZScore(ctx, ImageRedisKey("queue"), task.TaskId).Result()
	require.NoError(t, err)
	assert.Equal(t, float64(now), score)
	require.NoError(t, db.Where("id = ?", command.Id).Take(&command).Error)
	assert.Equal(t, "delivered", command.Status)

	claimed, err := model.ClaimAsyncImageTask(ctx, task.TaskId, "poll-worker", 120)
	require.NoError(t, err)
	assert.Equal(t, model.ImageTaskInvoking, claimed.Status)
	assert.Equal(t, task.ChannelId, claimed.ChannelId, "polling must retain the frozen upstream channel")
	assert.Equal(t, task.DispatchedAt, claimed.DispatchedAt, "polling must not look like a fresh generation dispatch")
	require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Update("lease_expires_at", now-1).Error)
	require.NoError(t, model.RecoverAsyncImageTasks(ctx, 100, 1200))
	var recovered model.AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&recovered).Error)
	assert.Equal(t, model.ImageTaskInvoking, recovered.Status)
	assert.Empty(t, recovered.LeaseToken)
	assert.Zero(t, recovered.LeaseExpiresAt)

	retryTask, err := model.ClaimAsyncImageTask(ctx, task.TaskId, "storage-retry-worker", 120)
	require.NoError(t, err)
	cfg := DefaultImageRuntimeConfig()
	cfg.TotalRetries = 1
	cfg.TransientRetries = 1
	cfg.TransientRetryBase = 1
	cfg.TransientRetryMax = 1
	cfg.RetryJitter = 0
	require.NoError(t, failAsyncImageInvocation(ctx, retryTask, &AsyncImageFailure{Code: 606, InternalCode: "output_persist_failed", Message: "Upstream output could not be durably persisted"}, cfg))
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&recovered).Error)
	assert.Equal(t, model.ImageTaskInvoking, recovered.Status)
	assert.Equal(t, "upstream-job-1", recovered.UpstreamTaskId)
	assert.Equal(t, 7, recovered.ChannelId)
	assert.Equal(t, 1, recovered.TransientRetryCount)
	assert.Equal(t, 1, recovered.RetryCount)
}

func TestAsyncImageDurableReferenceRoutingAndRedisIsolation(t *testing.T) {
	asyncImageKeyFixture(t)
	ctx := context.Background()
	var history []ImageChannelAttempt
	for _, expected := range []struct {
		channel, pin int
		stop         bool
	}{{10, 10, false}, {10, 10, false}, {10, 0, false}, {20, 0, true}} {
		history = append(history, ImageChannelAttempt{ChannelId: expected.channel, Dispatched: true, ReferenceFailure: true, Code: 602})
		encoded, err := common.Marshal(history)
		require.NoError(t, err)
		history = nil
		require.NoError(t, common.Unmarshal(encoded, &history))
		pin, excluded, stop := ImageAttemptRouting(history, 2)
		assert.Equal(t, expected.pin, pin)
		assert.Equal(t, expected.stop, stop)
		if expected.pin == 0 {
			assert.Equal(t, []int{10}, excluded)
		}
	}
	pin, excluded, stop := ImageAttemptRouting([]ImageChannelAttempt{{ChannelId: 10, Code: 602, ReferenceFailure: false}}, 2)
	assert.Zero(t, pin)
	assert.Empty(t, excluded)
	assert.False(t, stop)
	accountA := ImageChannelAttempt{ChannelId: 10, KeyFingerprint: "account-a", Dispatched: true, ReferenceFailure: true}
	accountB := accountA
	accountB.KeyFingerprint = "account-b"
	for _, tc := range []struct {
		history  []ImageChannelAttempt
		pin      string
		excluded []string
		stop     bool
	}{
		{[]ImageChannelAttempt{accountA}, "account-a", nil, false},
		{[]ImageChannelAttempt{accountA, accountA}, "account-a", nil, false},
		{[]ImageChannelAttempt{accountA, accountA, accountA}, "", []string{"account-a"}, false},
		{[]ImageChannelAttempt{accountA, accountA, accountA, accountB}, "", nil, true},
	} {
		routing := ImageAccountAttemptRouting(tc.history, 2, "openai", 3)
		assert.Equal(t, tc.pin, routing.Pin)
		assert.Equal(t, tc.excluded, routing.Excluded)
		assert.Equal(t, tc.stop, routing.Stop)
	}
	ambiguousFallback := accountA
	ambiguousFallback.Code = 601
	ambiguousFallback.ReferenceMode = "passthrough_fallback_local"
	routing := ImageAccountAttemptRouting([]ImageChannelAttempt{ambiguousFallback}, 0, "gemini", 3)
	assert.Equal(t, "account-a", routing.Pin, "the forced local fallback must reuse the selected account even when configured reference retries are zero")
	ambiguousFallback.KeyFingerprint = ""
	routing = ImageAccountAttemptRouting([]ImageChannelAttempt{ambiguousFallback}, 0, "gemini", 3)
	assert.Equal(t, 10, routing.PinChannel, "single-key channels must also remain pinned for the forced local fallback")
	assert.True(t, ImageAccountAttemptRouting([]ImageChannelAttempt{accountA, accountA, accountA}, 2, "gemini", 0).Stop)
	accountA.ReferenceFailure, accountB.ReferenceFailure = false, false
	assert.Equal(t, "account-b", ImageAccountAttemptRouting([]ImageChannelAttempt{accountA, accountB}, 2, "gemini", 1).Pin)
	accountChannel := model.Channel{Id: 10, Key: "key-a\nkey-b", ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	available := ImageChannelAccounts(accountChannel, ImageAccountRouting{})
	require.Len(t, available, 2)
	remaining := ImageChannelAccounts(accountChannel, ImageAccountRouting{Excluded: []string{available[0].Fingerprint}})
	require.Len(t, remaining, 1)
	assert.Equal(t, "key-b", remaining[0].Key)
	accountChannel.ChannelInfo.MultiKeyStatusList = map[int]int{1: common.ChannelStatusManuallyDisabled}
	assert.Empty(t, ImageChannelAccounts(accountChannel, ImageAccountRouting{Pin: available[1].Fingerprint}))
	assert.Equal(t, 60, ImageRetryDelay(15, 60, 5, 0, 0, 900))
	assert.Equal(t, 900, ImageRetryDelay(15, 60, 0, 0, 3600*time.Second, 900))
	classified := ClassifyAsyncImageFailure(errors.New("image_url fetch failed: Bearer private-key https://signed.example/?secret=token"), 400)
	assert.Equal(t, 602, classified.Code)
	assert.NotContains(t, classified.Message, "private-key")
	assert.NotContains(t, classified.Message, "secret")
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	previous := common.RDB
	common.RDB = client
	t.Cleanup(func() { common.RDB = previous })
	t.Setenv("ASYNC_IMAGE_REDIS_PREFIX", "image-unit-test")
	cfg := DefaultImageRuntimeConfig()
	cfg.CircuitBreaker = true
	cfg.FailureThreshold = 2
	require.NoError(t, RecordImageCircuit(ctx, "async", 10, false, cfg))
	require.NoError(t, RecordImageCircuit(ctx, "async", 10, false, cfg))
	allowed, err := ImageCircuitAllows(ctx, "async", 10, cfg)
	require.NoError(t, err)
	assert.False(t, allowed)
	allowed, err = ImageCircuitAllows(ctx, "sync", 10, cfg)
	require.NoError(t, err)
	assert.True(t, allowed)
	require.NoError(t, RecordImageCircuit(ctx, "async", 10, true, cfg))
	allowed, err = ImageCircuitAllows(ctx, "async", 10, cfg)
	require.NoError(t, err)
	assert.True(t, allowed)
	valid := asyncImageBytesFixture(t)
	raw := "https://reference.example/image.png"
	identity := ImageIdentityHash("1", raw)
	plain, err := common.Marshal(valid)
	require.NoError(t, err)
	cipher, err := EncryptImagePayload(plain, "reference:"+identity)
	require.NoError(t, err)
	keys := []string{ImageRedisKey("reference-cache", "data"), ImageRedisKey("reference-cache", "expires"), ImageRedisKey("reference-cache", "sizes"), ImageRedisKey("reference-cache", "bytes")}
	result, err := imageReferenceCacheWrite.Run(ctx, client, keys, time.Now().Unix(), identity, time.Now().Unix()+60, len(cipher), len(cipher), cipher, 60).Int()
	require.NoError(t, err)
	assert.Equal(t, 1, result)
	result, err = imageReferenceCacheWrite.Run(ctx, client, keys, time.Now().Unix(), "another", time.Now().Unix()+60, len(cipher), len(cipher), cipher, 60).Int()
	require.NoError(t, err)
	assert.Zero(t, result, "the shared byte budget must reject another entry")
	cfg.ReferenceConcurrency = 1
	require.NoError(t, client.ZAdd(ctx, ImageRedisKey("reference-download-slots"), &redis.Z{Score: float64(time.Now().Unix() + 60), Member: "busy"}).Err())
	hit, err := DownloadImageReferenceCached(ctx, 1, raw, cfg)
	require.NoError(t, err)
	assert.Equal(t, valid.Data, hit.Data)
	_, err = DownloadImageReferenceCached(ctx, 2, raw, cfg)
	var failure *AsyncImageFailure
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, 603, failure.Code, "another Token cannot reuse private cached bytes")
}

func TestAsyncImageChannelReferenceCapacityRouting(t *testing.T) {
	assert.Equal(t, 14, DefaultImageRuntimeConfig().MaxReferences)

	legacyPriority, capablePriority := int64(100), int64(10)
	legacy := model.Channel{
		Id:       1,
		Type:     1,
		Key:      "legacy-key",
		Status:   common.ChannelStatusEnabled,
		Name:     "legacy-reference-limit",
		Models:   "image-model",
		Group:    "default",
		Priority: &legacyPriority,
	}
	capable := model.Channel{
		Id:       2,
		Type:     1,
		Key:      "extended-key",
		Status:   common.ChannelStatusEnabled,
		Name:     "extended-reference-limit",
		Models:   "image-model",
		Group:    "default",
		Priority: &capablePriority,
	}
	maxReferences := 14
	capable.SetSetting(dto.ChannelSettings{ImageMaxReferenceImages: &maxReferences})
	assert.Equal(t, model.DefaultImageChannelMaxReferenceImages, legacy.GetImageMaxReferenceImages())
	assert.Equal(t, maxReferences, capable.GetImageMaxReferenceImages())

	invalidLimit := dto.MaxImageN + 1
	invalid := model.Channel{}
	invalid.SetSetting(dto.ChannelSettings{ImageMaxReferenceImages: &invalidLimit})
	require.EqualError(t, invalid.ValidateSettings(), "invalid image_max_reference_images: 129")

	previousDB, previousCapability := model.DB, ImageChannelCapabilityFunc
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "reference-capacity.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	ImageChannelCapabilityFunc = func(model.Channel, string) dto.ImageCapability {
		return dto.ImageCapability{Provider: "openai", Protocol: "openai_images", Generate: true, Edit: true}
	}
	t.Cleanup(func() {
		model.DB, ImageChannelCapabilityFunc = previousDB, previousCapability
		connection, openErr := db.DB()
		if openErr == nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.ImageChannelPool{}, &model.Option{}))
	require.NoError(t, db.Create(&[]model.Channel{legacy, capable}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "default", Model: "image-model", ChannelId: legacy.Id, Enabled: true},
		{Group: "default", Model: "image-model", ChannelId: capable.Id, Enabled: true},
	}).Error)

	policy := model.ImageGroupPolicy{Group: "default", Platform: "openai", AsyncProvider: "openai", PoolMode: "resolution"}
	request := AsyncImageRequest{Provider: "openai", Platform: "openai", Model: "image-model", Kind: "image_to_image"}
	for range 9 {
		request.Parts = append(request.Parts, AsyncImageInputPart{Type: "image_url", URL: "https://example.com/reference.png"})
	}
	selected, _, err := PickImageChannel(t.Context(), policy, request, ImageAccountRouting{})
	require.NoError(t, err)
	assert.Equal(t, capable.Id, selected.Id, "an extended-capacity channel must outrank a higher-priority legacy channel")

	request.Parts = request.Parts[:8]
	selected, _, err = PickImageChannel(t.Context(), policy, request, ImageAccountRouting{})
	require.NoError(t, err)
	assert.Equal(t, legacy.Id, selected.Id, "requests within the legacy limit keep priority-based routing")

	for range 7 {
		request.Parts = append(request.Parts, AsyncImageInputPart{Type: "image_url", URL: "https://example.com/reference.png"})
	}
	selected, _, err = PickImageChannel(t.Context(), policy, request, ImageAccountRouting{})
	require.NoError(t, err)
	assert.Equal(t, legacy.Id, selected.Id, "routing falls back to the original priority order when no channel covers the request")
}

func TestClassifyGeminiAsyncImageHTTPFailurePreservesProviderDiagnostics(t *testing.T) {
	testCases := []struct {
		name         string
		body         string
		header       http.Header
		expectedCode int
		providerCode string
		requestID    string
		message      string
		fallback     bool
	}{
		{
			name:         "reference image limit from provider code",
			body:         `{"error":{"code":"too_many_reference_images","message":"Invalid request","status":"INVALID_ARGUMENT"},"request_id":"gemini-body-request"}`,
			header:       http.Header{"X-Goog-Request-Id": []string{"gemini-header-request"}},
			expectedCode: 611,
			providerCode: "too_many_reference_images",
			requestID:    "gemini-body-request",
			message:      "Invalid request",
		},
		{
			name:         "missing reference image from original message",
			body:         `{"error":{"code":"invalid_request","message":"请上传参考图，未检测到参考图"}}`,
			header:       http.Header{"X-Request-Id": []string{"gemini-missing-reference"}},
			expectedCode: 612,
			providerCode: "invalid_request",
			requestID:    "gemini-missing-reference",
			message:      "请上传参考图，未检测到参考图",
		},
		{
			name:         "unprocessable prompt keeps a redacted original message",
			body:         `{"error":{"code":400,"message":"prompt or input images could not be processed; Bearer private-key; https://signed.example/path?secret=token; ` + strings.Repeat("x", asyncImageProviderMessageLimit+20) + `","status":"INVALID_ARGUMENT"}}`,
			header:       http.Header{"X-Goog-Request-Id": []string{"gemini-unprocessable"}},
			expectedCode: 613,
			providerCode: "400",
			requestID:    "gemini-unprocessable",
			message:      "prompt or input images could not be processed",
			fallback:     true,
		},
		{
			name:         "ambiguous safety rejection can retry remote references locally",
			body:         `{"error":{"code":400,"message":"The request was rejected by the upstream safety policy.","status":"INVALID_ARGUMENT"}}`,
			expectedCode: 601,
			providerCode: "400",
			message:      "upstream safety policy",
			fallback:     true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			response := &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     testCase.header,
				Body:       io.NopCloser(strings.NewReader(testCase.body)),
			}

			failure := ClassifyGeminiAsyncImageHTTPFailure(t.Context(), response)

			require.NotNil(t, failure)
			assert.Equal(t, testCase.expectedCode, failure.Code)
			assert.Equal(t, http.StatusBadRequest, failure.HTTPStatus)
			assert.Equal(t, testCase.providerCode, failure.ProviderCode)
			assert.Equal(t, testCase.requestID, failure.UpstreamRequestID)
			assert.Contains(t, failure.Message, testCase.message)
			assert.NotContains(t, failure.Message, "private-key")
			assert.NotContains(t, failure.Message, "signed.example")
			assert.LessOrEqual(t, len([]rune(failure.Message)), asyncImageProviderMessageLimit)
			assert.Equal(t, testCase.fallback, ShouldFallbackGeminiReferenceTransport(failure))
		})
	}
}

func TestGeminiAmbiguousReferenceFailureSchedulesSingleLocalFallback(t *testing.T) {
	previousDB := model.DB
	dialector := gorm.Dialector(sqlite.Open(filepath.Join(t.TempDir(), "gemini-reference-fallback.db")))
	if dsn := os.Getenv("IMAGE_WORKER_TEST_MYSQL_DSN"); dsn != "" {
		require.Contains(t, dsn, "/new_api_image_worker_test", "use the dedicated disposable image worker database")
		dialector = mysql.Open(dsn)
	} else if dsn := os.Getenv("IMAGE_WORKER_TEST_PG_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "dbname=new_api_image_worker_test") || strings.Contains(dsn, "/new_api_image_worker_test"), "use the dedicated disposable image worker database")
		dialector = postgres.Open(dsn)
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		connection, openErr := db.DB()
		if openErr == nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}, &model.AsyncImageStagingObject{}, &model.AsyncImageBill{}))

	now := time.Now().Unix()
	attempts, err := common.Marshal([]ImageChannelAttempt{{
		ChannelId:     15,
		StartedAt:     now - 1,
		Dispatched:    true,
		ReferenceMode: "passthrough_fallback_local",
	}})
	require.NoError(t, err)
	task := model.AsyncImageTask{
		TaskId:         "asyncimg_gemini_reference_fallback",
		Status:         model.ImageTaskInvoking,
		Version:        1,
		ChannelId:      15,
		LeaseToken:     "gemini-reference-fallback-lease",
		LeaseExpiresAt: now + 60,
		DispatchedAt:   now - 1,
		RequestCipher:  []byte("encrypted-request"),
		Attempts:       string(attempts),
		CreatedAt:      now - 1,
	}
	require.NoError(t, db.Create(&task).Error)
	cfg := DefaultImageRuntimeConfig()
	cfg.GeminiReferenceMode = "passthrough_fallback_local"
	cfg.ReferenceRetries = 0
	cfg.ReferenceRetryBase = 1
	cfg.ReferenceRetryMax = 1
	cfg.RetryJitter = 0
	failure := &AsyncImageFailure{
		Code:                       601,
		InternalCode:               "upstream_failed",
		Message:                    "The request was rejected by the upstream safety policy.",
		HTTPStatus:                 http.StatusBadRequest,
		ProviderCode:               "400",
		ProviderStatus:             "INVALID_ARGUMENT",
		ReferenceTransportFallback: true,
	}

	require.NoError(t, failAsyncImageInvocation(t.Context(), task, failure, cfg))

	var stored model.AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&stored).Error)
	assert.Equal(t, model.ImageTaskQueued, stored.Status)
	assert.Equal(t, 1, stored.RetryCount)
	assert.Equal(t, 1, stored.ReferenceRetryCount)
	assert.Zero(t, stored.DispatchedAt)
	assert.Zero(t, stored.ChannelId)
	assert.Zero(t, stored.FinishedAt)
	assert.Greater(t, stored.NextAttemptAt, now)
	assert.Equal(t, "local", ResolveImageReferenceTransportMode(cfg.GeminiReferenceMode, stored.ReferenceRetryCount))
	var storedAttempts []ImageChannelAttempt
	require.NoError(t, common.UnmarshalJsonStr(stored.Attempts, &storedAttempts))
	require.Len(t, storedAttempts, 1)
	assert.True(t, storedAttempts[0].ReferenceFailure)
	assert.Equal(t, 601, storedAttempts[0].Code)
	assert.Equal(t, "passthrough_fallback_local", storedAttempts[0].ReferenceMode)

	require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Updates(map[string]any{
		"status":           model.ImageTaskInvoking,
		"version":          stored.Version + 1,
		"lease_token":      "gemini-reference-success-lease",
		"lease_expires_at": time.Now().Unix() + 60,
	}).Error)
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&stored).Error)
	bill := model.AsyncImageBill{
		TaskId:           stored.TaskId,
		BillingRequestId: "async-image:" + stored.TaskId,
		Fingerprint:      "gemini-reference-fallback-bill",
		UserId:           stored.UserId,
		TokenId:          stored.TokenId,
	}
	require.NoError(t, model.StageAsyncImageOutput(t.Context(), stored, []model.AsyncImageStagingObject{{
		Data:        []byte("generated-image"),
		Checksum:    "generated-image-checksum",
		Width:       1024,
		Height:      1024,
		ContentType: "image/png",
	}}, bill))
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&stored).Error)
	assert.Equal(t, model.ImageTaskUpstreamSucceeded, stored.Status)
	assert.Empty(t, stored.ErrorCode)
	assert.Empty(t, stored.ErrorMessage)
	assert.Zero(t, stored.PublicErrorCode)
}

func TestFailAsyncImageInvocationPersistsSanitizedProviderDiagnostics(t *testing.T) {
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "provider-diagnostics.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		connection, openErr := db.DB()
		if openErr == nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}))

	now := time.Now().Unix()
	attempts, err := common.Marshal([]ImageChannelAttempt{{ChannelId: 17, StartedAt: now - 1, Dispatched: true}})
	require.NoError(t, err)
	task := model.AsyncImageTask{
		TaskId:         "asyncimg_provider_diagnostics",
		Status:         model.ImageTaskInvoking,
		Version:        1,
		LeaseToken:     "provider-diagnostics-lease",
		LeaseExpiresAt: now + 60,
		DispatchedAt:   now - 1,
		Attempts:       string(attempts),
		CreatedAt:      now - 1,
	}
	require.NoError(t, db.Create(&task).Error)
	cfg := DefaultImageRuntimeConfig()
	cfg.TotalRetries = 0
	failure := &AsyncImageFailure{
		Code:              611,
		InternalCode:      "upstream_failed",
		Message:           "image: at most 8 images are allowed; Bearer private-key; https://signed.example/path?secret=token",
		HTTPStatus:        http.StatusBadRequest,
		ProviderCode:      "too_many_reference_images",
		ProviderStatus:    "INVALID_ARGUMENT",
		UpstreamRequestID: "gemini-request-611",
	}

	require.NoError(t, failAsyncImageInvocation(t.Context(), task, failure, cfg))

	var stored model.AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&stored).Error)
	assert.Equal(t, model.ImageTaskFailed, stored.Status)
	assert.Equal(t, 611, stored.PublicErrorCode)
	assert.Contains(t, stored.ErrorMessage, "image: at most 8 images are allowed")
	assert.NotContains(t, stored.ErrorMessage, "private-key")
	assert.NotContains(t, stored.ErrorMessage, "signed.example")
	var storedAttempts []ImageChannelAttempt
	require.NoError(t, common.UnmarshalJsonStr(stored.Attempts, &storedAttempts))
	require.Len(t, storedAttempts, 1)
	assert.Equal(t, http.StatusBadRequest, storedAttempts[0].HTTPStatus)
	assert.Equal(t, "too_many_reference_images", storedAttempts[0].ProviderCode)
	assert.Equal(t, "INVALID_ARGUMENT", storedAttempts[0].ProviderStatus)
	assert.Equal(t, "gemini-request-611", storedAttempts[0].UpstreamRequestID)
	assert.Equal(t, stored.ErrorMessage, storedAttempts[0].ErrorMessage)
}

func TestInvalidImageReferenceFailureIsIndexedAndNotRetried(t *testing.T) {
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "invalid-reference.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		connection, openErr := db.DB()
		if openErr == nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}))

	now := time.Now().Unix()
	attempts, err := common.Marshal([]ImageChannelAttempt{{ChannelId: 17, StartedAt: now - 1}})
	require.NoError(t, err)
	task := model.AsyncImageTask{
		TaskId:         "asyncimg_invalid_reference",
		Status:         model.ImageTaskInvoking,
		Version:        1,
		LeaseToken:     "invalid-reference-lease",
		LeaseExpiresAt: now + 60,
		Attempts:       string(attempts),
		CreatedAt:      now - 1,
	}
	require.NoError(t, db.Create(&task).Error)
	_, validationErr := ValidateImageBytes([]byte("oversized"), "", 1, 1)
	require.Error(t, validationErr)
	failure := NewImageReferenceFailure(3, validationErr)
	assert.Equal(t, 604, failure.Code)
	assert.Equal(t, "invalid_reference_image", failure.InternalCode)
	assert.Equal(t, "Reference image 3: image byte limit exceeded", failure.Message)
	sanitized := NewImageReferenceFailure(4, errors.New("download failed for https://signed.example/path/image.png?secret=private-token"))
	assert.NotContains(t, sanitized.Message, "signed.example")
	assert.NotContains(t, sanitized.Message, "private-token")

	require.NoError(t, failAsyncImageInvocation(t.Context(), task, failure, DefaultImageRuntimeConfig()))

	var stored model.AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&stored).Error)
	assert.Equal(t, model.ImageTaskFailed, stored.Status)
	assert.Zero(t, stored.RetryCount)
	assert.Zero(t, stored.ReferenceRetryCount)
	assert.Zero(t, stored.NextAttemptAt)
	assert.Equal(t, failure.Message, stored.ErrorMessage)
}

func asyncImageKeyFixture(t *testing.T) {
	t.Helper()
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "test")
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
}

func TestAsyncImageCipherBindingRotationAndMissingKeys(t *testing.T) {
	asyncImageKeyFixture(t)
	ciphertext, err := EncryptImagePayload([]byte("private prompt"), "task:1")
	require.NoError(t, err)
	assert.NotContains(t, string(ciphertext), "private prompt")
	plain, err := DecryptImagePayload(ciphertext, "task:1")
	require.NoError(t, err)
	assert.Equal(t, "private prompt", string(plain))
	_, err = DecryptImagePayload(ciphertext, "task:2")
	assert.Error(t, err)
	ciphertext[len(ciphertext)-1] ^= 1
	_, err = DecryptImagePayload(ciphertext, "task:1")
	assert.Error(t, err)
	ciphertext[len(ciphertext)-1] ^= 1
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))+",next:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "next")
	_, err = DecryptImagePayload(ciphertext, "task:1")
	assert.NoError(t, err)
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "")
	_, err = EncryptImagePayload([]byte("secret"), "task:1")
	assert.Error(t, err)
}

func TestAsyncImageCleanupPreviewActorScopeExpiryAndTamper(t *testing.T) {
	asyncImageKeyFixture(t)
	filters := ImageCleanupFilters{UserId: 10}
	now := int64(1000)
	preview, err := SignImageCleanupPreview("expired", filters, 7, now)
	require.NoError(t, err)
	assert.True(t, ValidateImageCleanupPreview(preview, "expired", filters, 7, now+1))
	assert.False(t, ValidateImageCleanupPreview(preview, "expired", filters, 8, now+1))
	assert.False(t, ValidateImageCleanupPreview(preview, "all", filters, 7, now+1))
	assert.False(t, ValidateImageCleanupPreview(preview, "expired", ImageCleanupFilters{UserId: 11}, 7, now+1))
	assert.False(t, ValidateImageCleanupPreview(preview, "expired", filters, 7, now+600))
	assert.False(t, ValidateImageCleanupPreview(preview+"tampered", "expired", filters, 7, now+1))
}

type imageResolverFixture struct{ addresses []netip.Addr }

func (fixture imageResolverFixture) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return fixture.addresses, nil
}

type imagePeerFixture struct {
	net.Conn
	peer   net.Addr
	closed bool
}

func (fixture *imagePeerFixture) RemoteAddr() net.Addr { return fixture.peer }
func (fixture *imagePeerFixture) Close() error         { fixture.closed = true; return nil }

func TestAsyncImageReferenceDNSRedirectAndConnectedPeer(t *testing.T) {
	cfg := DefaultImageRuntimeConfig()
	for _, tc := range []struct {
		name      string
		addresses []netip.Addr
		peer      string
		reject    bool
		dialed    bool
	}{
		{"mixed DNS", []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, "8.8.8.8", true, false},
		{"private peer", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, "127.0.0.1", true, true},
		{"changed peer", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, "9.9.9.9", true, true},
		{"pinned public peer", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, "8.8.8.8", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := &imagePeerFixture{peer: &net.TCPAddr{IP: net.ParseIP(tc.peer), Port: 443}}
			var target string
			client := imageReferenceClient(cfg, imageResolverFixture{tc.addresses}, func(_ context.Context, _, address string) (net.Conn, error) { target = address; return peer, nil })
			conn, err := client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "reference.example:443")
			assert.Equal(t, tc.reject, err != nil)
			assert.Equal(t, tc.dialed, target != "")
			if tc.dialed {
				assert.Equal(t, "8.8.8.8:443", target)
			}
			if tc.reject && tc.dialed {
				assert.True(t, peer.closed)
			}
			if !tc.reject {
				require.NotNil(t, conn)
				require.NoError(t, conn.Close())
			}
			for _, location := range []string{"https://127.0.0.1/private", "http://reference.example/x", "https://user:secret@reference.example/x"} {
				parsed, parseErr := url.Parse(location)
				require.NoError(t, parseErr)
				assert.Error(t, client.CheckRedirect(&http.Request{URL: parsed}, []*http.Request{{}}))
			}
			parsed, err := url.Parse("https://public.example/x")
			require.NoError(t, err)
			assert.NoError(t, client.CheckRedirect(&http.Request{URL: parsed}, []*http.Request{{}}))
			assert.Error(t, client.CheckRedirect(&http.Request{URL: parsed}, make([]*http.Request, cfg.DownloadRedirects+1)))
		})
	}
}

func TestAsyncImageProtocolDimensionsAndRawIdempotency(t *testing.T) {
	for _, test := range []struct{ resolution, ratio, size string }{{"1K", "21:9", "2384x1024"}, {"2K", "5:4", "2048x1632"}, {"4K", "4:5", "3272x4096"}, {"2K", "16:9", "2048x1152"}, {"1K", "auto", "auto"}} {
		size, err := MapOpenAIImageDimensions(test.resolution, test.ratio)
		require.NoError(t, err)
		assert.Equal(t, test.size, size)
	}
	_, err := MapOpenAIImageDimensions("invalid", "auto")
	assert.Error(t, err)
	assert.Equal(t, "4K", ImageNativeTier(2384, 1024, "openai"))
	assert.Equal(t, "1K", ImageNativeTier(2384, 1024, "gemini"))
	assert.Equal(t, "2K", ImageNativeTier(2048, 1152, "openai"))
	assert.Equal(t, "2K", ImageNativeTier(2048, 1152, "gemini"))
	first := AsyncImageRequestHash("openai", "bb", "/v1/images/generations_oa", []byte(`{"prompt":"x"}`))
	assert.NotEqual(t, first, AsyncImageRequestHash("openai", "bb", "/v1/images/generations_oa", []byte(`{ "prompt":"x" }`)))
	assert.NotEqual(t, first, AsyncImageRequestHash("openai", "bb", "/v1/images/edits_oa", []byte(`{"prompt":"x"}`)))
	request, err := ParseAsyncImageRequest([]byte(`{"model":"gpt-image-2","prompt":"x","resolution":"1K","aspect_ratio":"21:9","output_compression":0,"stream":false}`), "application/json", "/v1/images/generations_oa", DefaultImageRuntimeConfig())
	require.NoError(t, err)
	assert.Equal(t, "2384x1024", request.Size)
	assert.True(t, request.ExplicitTier)
	assert.Equal(t, "0", string(request.Native["output_compression"]))
	assert.Equal(t, "false", string(request.Native["stream"]))
	request, err = ParseAsyncImageRequest([]byte(`{"prompt":"x","size":"auto","resolution":"1K"}`), "application/json", "/v1/images/generations_oa", DefaultImageRuntimeConfig())
	require.NoError(t, err)
	assert.Equal(t, "AUTO", request.Resolution)
	assert.Equal(t, "4K", ImageBillingSpecifications(request, []ImageBytes{{Width: 4096, Height: 4096}})[0]["resolution"])
	for _, body := range []string{`{"prompt":"x","n":0}`, `{"prompt":"x","n":129}`, `{"prompt":"x","n":18446744073709551615}`, `{"prompt":"x","stream":true}`, `{"prompt":"x","mask":{"image_url":"x"}}`} {
		_, err := ParseAsyncImageRequest([]byte(body), "application/json", "/v1/images/generations_oa", DefaultImageRuntimeConfig())
		assert.Error(t, err, body)
	}
}

func TestAsyncImageFixedBillMixedSpecificationsAndUsageBoundaries(t *testing.T) {
	previous := model.DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "bill.db")), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() {
		model.DB = previous
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}))
	user := model.User{Username: "image-bill-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/images/generations_oa", nil)
	expression := `param("resolution") == "4K" ? tier("large", fixed(0.04)) * image_count : tier("small", fixed(0.01)) * image_count`
	count := 2
	info := &relaycommon.RelayInfo{UserId: user.Id, OriginModelName: "image-model", ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "image-model"}, StartTime: time.Now(), Request: &dto.ImageRequest{}, TieredBillingSnapshot: &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), ExprVersion: 1, GroupRatio: 1, QuotaPerUnit: 500000, EstimatedImageCount: &count}, PriceData: types.PriceData{GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
	task := model.AsyncImageTask{TaskId: "mixed-specifications", UserId: user.Id, TokenId: 1, ChannelId: 1, Model: "image-model", Group: "default"}
	request := AsyncImageRequest{Platform: "openai", Model: "image-model", Count: 7, Size: "auto", Resolution: "AUTO", Prompt: "private prompt", Native: map[string]common.RawMessage{"quality": common.RawMessage(`"high"`)}}
	info.BillingRequestInput = &billingexpr.RequestInput{Body: []byte(`{"model":"image-model","n":7,"size":"auto"}`)}
	images := []ImageBytes{{Width: 1024, Height: 1024}, {Width: 4096, Height: 4096}}
	usage := &dto.Usage{PromptTokens: 1, TotalTokens: 1}
	bill, err := FixAsyncImageBill(c, task, info, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
	require.NoError(t, err)
	assert.Equal(t, 25000, bill.Quota, "sum 1K + 4K, never largest tier multiplied by count")
	assert.NotContains(t, bill.Snapshot, "private prompt")
	assert.Contains(t, bill.Snapshot, `"n":7`)
	assert.Contains(t, bill.Snapshot, `"quality":"high"`)
	usage.PromptTokens, usage.TotalTokens = 0, 0
	zeroUsageBill, err := FixAsyncImageBill(c, task, info, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
	require.NoError(t, err)
	assert.Equal(t, 25000, zeroUsageBill.Quota, "image-count expressions charge actual outputs with zero tokens")
	var persistedUsage dto.Usage
	require.NoError(t, common.UnmarshalJsonStr(zeroUsageBill.Usage, &persistedUsage))
	assert.Zero(t, persistedUsage.PromptTokens)
	assert.Zero(t, persistedUsage.TotalTokens)
	for _, price := range []struct {
		unitPrice float64
		quota     int
	}{{0.01, 10000}, {0.1, 100000}} {
		legacyInfo := *info
		legacyInfo.TieredBillingSnapshot = nil
		legacyInfo.PriceData = types.PriceData{UsePrice: true, ModelPrice: price.unitPrice, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}
		legacyBill, err := FixAsyncImageBill(c, task, &legacyInfo, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
		require.NoError(t, err)
		assert.Equal(t, price.quota, legacyBill.Quota, "both actual images are charged at unit price %g", price.unitPrice)
	}
	assert.Zero(t, usage.PromptTokens, "billing must not fabricate upstream usage")
	for _, tc := range []struct {
		name       string
		expression string
		usage      dto.Usage
		quota      int
		wantError  bool
	}{
		{"two images at 0.1 each cost 0.2", `tier("images", fixed(0.1)) * image_count`, dto.Usage{}, 100000, false},
		{"token fallback is charged once", `len > 100 ? tier("tokens", p * 2) : tier("images", fixed(0.01)) * image_count`, dto.Usage{PromptTokens: 200, TotalTokens: 200}, 200, false},
		{"batch rounding happens once", `tier("images", fixed(0.000001)) * image_count`, dto.Usage{}, 1, false},
		{"mixed units cannot duplicate aggregate usage", `param("resolution") == "4K" ? tier("tokens", p * 2) : tier("images", fixed(0.01)) * image_count`, dto.Usage{PromptTokens: 200, TotalTokens: 200}, 0, true},
		{"missing paid token usage cannot be silently free", `tier("tokens", p * 2 + c * 8)`, dto.Usage{}, 0, true},
		{"estimated token usage cannot replace actual usage", `tier("tokens", p * 2 + c * 8)`, dto.Usage{PromptTokens: 200, TotalTokens: 200, BillingUsage: &dto.BillingUsage{Estimated: true}}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caseInfo := *info
			caseInfo.PriceData = types.PriceData{QuotaToPreConsume: 100, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}
			caseInfo.TieredBillingSnapshot = &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ExprString: tc.expression, ExprHash: billingexpr.ExprHashString(tc.expression), ExprVersion: 1, GroupRatio: 1, QuotaPerUnit: 500000, EstimatedImageCount: &count}
			if !billingexpr.UsedVarsByHash(tc.expression, caseInfo.TieredBillingSnapshot.ExprHash)["image_count"] {
				caseInfo.TieredBillingSnapshot.EstimatedImageCount = nil
			}
			actual, err := FixAsyncImageBill(c, task, &caseInfo, &tc.usage, images, request, model.ImageFundingSelection{Source: "wallet"})
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.quota, actual.Quota)
		})
	}
	request.ExplicitTier, request.Resolution, request.Size = true, "1K", "2384x1024"
	assert.Equal(t, "1K", ImageBillingSpecifications(request, images)[1]["resolution"])
	request.ExplicitTier, request.Resolution, request.Size = false, "", "4096x2304"
	assert.Equal(t, "4K", ImageBillingSpecifications(request, images)[0]["resolution"])
	usage.PromptTokens = -1
	_, err = FixAsyncImageBill(c, task, info, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
	assert.Error(t, err)
	usage.PromptTokens = common.MaxQuota + 1
	_, err = FixAsyncImageBill(c, task, info, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
	assert.Error(t, err)
	usage.PromptTokens = 1
	info.TieredBillingSnapshot.ExprString = `tier("huge", fixed(1000000)) * image_count`
	info.TieredBillingSnapshot.ExprHash = billingexpr.ExprHashString(info.TieredBillingSnapshot.ExprString)
	_, err = FixAsyncImageBill(c, task, info, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
	assert.Error(t, err)
}

func TestAsyncImageGeminiOrderedParts(t *testing.T) {
	body := `{"model":"gemini-image","messages":[{"role":"user","content":[{"type":"text","text":"first"},{"type":"image_url","image_url":{"url":"https://example.com/image.png"}},{"type":"text","text":"last"}]}],"extra_body":{"google":{"image_config":{"image_size":"2K","aspect_ratio":"auto"}}}}`
	request, err := ParseAsyncImageRequest([]byte(body), "application/json", "/v1/chat/completions_gm", DefaultImageRuntimeConfig())
	require.NoError(t, err)
	assert.Equal(t, "first\nlast", request.Prompt)
	assert.Equal(t, "image_url", request.Parts[1].Type)
	assert.Equal(t, "last", request.Parts[2].Text)
	assert.Equal(t, "2K", request.Resolution)
	assert.Empty(t, request.AspectRatio)
	_, err = ParseAsyncImageRequest([]byte(`{"model":"gemini-image","prompt":"x","size":"2048x1152"}`), "application/json", "/v1/images/generations_sc", DefaultImageRuntimeConfig())
	assert.NoError(t, err)
	body = `{"model":"gemini-2.5-flash-image","messages":[{"role":"user","content":[{"type":"text","text":"edit"},{"type":"image_url","image_url":{"url":"https://example.com/1.png"}},{"type":"image_url","image_url":{"url":"https://example.com/2.png"}},{"type":"image_url","image_url":{"url":"https://example.com/3.png"}},{"type":"image_url","image_url":{"url":"https://example.com/4.png"}}]}]}`
	request, err = ParseAsyncImageRequest([]byte(body), "application/json", "/v1/chat/completions_gm", DefaultImageRuntimeConfig())
	require.NoError(t, err)
	assert.Equal(t, 4, request.ReferenceImageCount(), "model capability policy, not a hard-coded model-name heuristic, sets the model limit")
}

func TestAsyncImageValidationContainerMIMEAndPublicAddress(t *testing.T) {
	valid := asyncImageBytesFixture(t)
	_, err := ValidateImageBytes(valid.Data, "image/jpeg", 1<<20, 100)
	assert.Error(t, err)
	_, err = ValidateImageBytes(valid.Data, "image/png", 1<<20, 5)
	assert.Error(t, err)
	normalizedPNG, err := ValidateImageBytes(append(bytes.Clone(valid.Data), []byte("trailing")...), "image/png", 1<<20, 100)
	require.NoError(t, err)
	assert.Equal(t, valid.Data, normalizedPNG.Data)
	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil))
	jpegBytes := bytes.Clone(encoded.Bytes())
	normalizedJPEG, err := ValidateImageBytes(append(bytes.Clone(jpegBytes), bytes.Repeat([]byte{0x5a}, 24)...), "image/jpeg", 1<<20, 100)
	require.NoError(t, err)
	assert.Equal(t, jpegBytes, normalizedJPEG.Data)
	_, err = ValidateImageBytes(append(bytes.Clone(jpegBytes), bytes.Repeat([]byte{0x5a}, maxNormalizedImageTrailingBytes+1)...), "image/jpeg", 1<<20, 100)
	assert.Error(t, err)
	assert.True(t, IsImageValidationError(err))
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "::ffff:127.0.0.1", "fc00::1", "2001:db8::1"} {
		assert.False(t, ImagePublicIP(netip.MustParseAddr(address)), address)
	}
	assert.True(t, ImagePublicIP(netip.MustParseAddr("8.8.8.8")))
	for _, value := range []string{"http://example.com/x", "https://user:pass@example.com/x", "https://127.0.0.1/x", "https://[::ffff:127.0.0.1]/x"} {
		_, err := ValidateImageReferenceURL(value)
		assert.Error(t, err, value)
	}
	_, err = ValidateImageReferenceURL("https://example.com:8443/x")
	assert.NoError(t, err)
	decoded, err := DownloadImageReference(context.Background(), valid.DataURL(), DefaultImageRuntimeConfig())
	require.NoError(t, err)
	assert.Equal(t, valid.Checksum, decoded.Checksum)
}

func TestResolveImageReferenceTransportMode(t *testing.T) {
	assert.Equal(t, "passthrough_fallback_local", ResolveImageReferenceTransportMode("passthrough_fallback_local", 0))
	assert.Equal(t, "local", ResolveImageReferenceTransportMode("passthrough_fallback_local", 1))
	assert.Equal(t, "passthrough", ResolveImageReferenceTransportMode("passthrough", 2))
	assert.Equal(t, "local", ResolveImageReferenceTransportMode("local", 2))
}

func TestAsyncImageLocalStorageAndDurableIntentRetry(t *testing.T) {
	asyncImageKeyFixture(t)
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "storage.db")), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	require.NoError(t, model.MigrateImageModels(db))
	require.NoError(t, model.MigrateImageModels(db))
	profile, err := SaveImageStorageProfile(context.Background(), model.ImageStorageProfile{Class: "temporary", Backend: "local", Root: t.TempDir(), Active: true}, nil)
	require.NoError(t, err)
	valid := asyncImageBytesFixture(t)
	store, err := OpenImageStorage(context.Background(), profile)
	require.NoError(t, err)
	key := "results/2026/09/16/image.png"
	intent := model.ImageUploadIntent{IntentKey: "deterministic-intent", Class: "temporary", ProfileId: profile.ProfileId, ObjectKey: key, Checksum: valid.Checksum, ByteSize: int64(len(valid.Data)), ContentType: valid.ContentType, Status: "pending", CreatedAt: time.Now().Unix()}
	first, err := StoreImageIntent(context.Background(), intent, valid, time.Now().Unix()+3600)
	require.NoError(t, err)
	second, err := StoreImageIntent(context.Background(), intent, valid, time.Now().Unix()+3600)
	require.NoError(t, err)
	assert.Equal(t, first.ObjectId, second.ObjectId)
	var count int64
	require.NoError(t, db.Model(&model.ImageStorageObject{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	read, err := store.Read(context.Background(), key, int64(len(valid.Data)))
	require.NoError(t, err)
	assert.Equal(t, valid.Data, read)
	for _, key := range []string{"../outside.png", "/outside.png", "C:/outside.png", "a/../../outside.png", "a\\outside.png"} {
		_, err := store.LocalPath(key, true)
		assert.Error(t, err, key)
	}
	expires := strconv.FormatInt(time.Now().Unix()+60, 10)
	signed, err := ImageObjectURL(context.Background(), first, "https://gateway.example", 60)
	require.NoError(t, err)
	assert.Contains(t, signed, "/api/image-objects/")
	assert.False(t, VerifyImageObjectSignature(first.ObjectId, expires, "test", "invalid"))
	link, err := url.Parse(signed)
	require.NoError(t, err)
	query := link.Query()
	assert.True(t, VerifyImageObjectSignature(first.ObjectId, query.Get("expires"), query.Get("key"), query.Get("signature")))
	assert.False(t, VerifyImageObjectSignature("another-object", query.Get("expires"), query.Get("key"), query.Get("signature")))
	assert.False(t, VerifyImageObjectSignature(first.ObjectId, "0", query.Get("key"), query.Get("signature")))
	assert.False(t, VerifyImageObjectSignature(first.ObjectId, query.Get("expires"), "unknown-key", query.Get("signature")))
	assert.False(t, VerifyImageObjectSignature(first.ObjectId, query.Get("expires")+"0", query.Get("key"), query.Get("signature")))
	require.NoError(t, store.Delete(context.Background(), key))
	require.NoError(t, store.Delete(context.Background(), key))
	_, err = store.Read(context.Background(), key, 1<<20)
	assert.ErrorIs(t, err, ErrImageObjectMissing)
	_, err = os.Stat(filepath.Join(profile.Root, "outside.png"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestVideoCapabilityNarrowingAndRequestValidation(t *testing.T) {
	profile := jsplugin.VideoProfile{Models: []string{"video-model"}, Modes: []jsplugin.VideoModeProfile{
		{Name: "text_to_video", Duration: &jsplugin.VideoDurationProfile{Min: 1, Max: 15, Step: 1, Default: 5}, Resolutions: []string{"480p", "720p", "1080p"}, DefaultResolution: "720p", AspectRatios: []string{"16:9", "9:16"}, DefaultAspectRatio: "16:9"},
		{Name: "image_to_video", Duration: &jsplugin.VideoDurationProfile{Min: 1, Max: 15, Step: 1, Default: 5}, Resolutions: []string{"720p"}, DefaultResolution: "720p", Inputs: []jsplugin.VideoInputProfile{{Name: "image", Kind: "image", Sources: []string{"url"}, MaxItems: 1, Required: true}}},
		{Name: "reference_to_video", Duration: &jsplugin.VideoDurationProfile{Min: 1, Max: 15, Step: 1, Default: 5}, Resolutions: []string{"480p", "720p"}, DefaultResolution: "720p", Inputs: []jsplugin.VideoInputProfile{{Name: "reference_images", Kind: "image", Sources: []string{"url", "data_uri"}, MaxItems: 7, Required: true}}},
	}}
	maximum := 10
	defaultDuration := 6
	narrowed, err := ApplyVideoModelOverrides(profile, VideoModelOverrides{
		Modes:             []string{"reference_to_video"},
		Duration:          &VideoDurationOverrides{Max: &maximum, Default: &defaultDuration},
		Resolutions:       []string{"720p"},
		DefaultResolution: "720p",
		MaxInputItems:     map[string]int{"reference_images": 3},
	})
	require.NoError(t, err)
	require.Len(t, narrowed.Modes, 1)
	assert.Equal(t, "reference_to_video", narrowed.Modes[0].Name)
	assert.Equal(t, 10, narrowed.Modes[0].Duration.Max)
	assert.Equal(t, 6, narrowed.Modes[0].Duration.Default)
	assert.Equal(t, []string{"720p"}, narrowed.Modes[0].Resolutions)
	assert.Equal(t, 3, narrowed.Modes[0].Inputs[0].MaxItems)
	assert.Equal(t, 15, profile.Modes[2].Duration.Max, "narrowing must not mutate the plugin declaration")

	request := map[string]any{"duration": float64(10), "resolution": "720p", "reference_images": []any{"https://cdn.example/one.png", "data:image/png;base64,eA=="}}
	require.NoError(t, ValidateVideoProfileRequest(narrowed, "reference_to_video", request, nil))
	request["duration"] = float64(11)
	assert.ErrorContains(t, ValidateVideoProfileRequest(narrowed, "reference_to_video", request, nil), "duration")
	request["duration"] = float64(10)
	request["reference_images"] = []any{"asset://one"}
	assert.ErrorContains(t, ValidateVideoProfileRequest(narrowed, "reference_to_video", request, nil), "source")
	request["reference_images"] = []any{"https://cdn.example/one.png"}
	request["reference_videos"] = []any{"https://cdn.example/video.mp4"}
	assert.ErrorContains(t, ValidateVideoProfileRequest(narrowed, "reference_to_video", request, nil), "not enabled")

	legacyNative := map[string]any{"metadata": map[string]any{"content": []any{map[string]any{
		"type": "image_url", "image_url": map[string]any{"url": "https://cdn.example/legacy.png"},
	}}}}
	require.NoError(t, ValidateVideoProfileRequest(profile, "image_to_video", legacyNative, nil))

	customInputProfile := jsplugin.VideoProfile{Models: []string{"video-model"}, Modes: []jsplugin.VideoModeProfile{{
		Name: "reference_to_video", Duration: &jsplugin.VideoDurationProfile{Values: []int{5}, Default: 5}, Resolutions: []string{"720p"}, DefaultResolution: "720p",
		Inputs: []jsplugin.VideoInputProfile{{Name: "storyboard", Kind: "image", Sources: []string{"url"}, MaxItems: 2, Required: true}},
	}}}
	require.NoError(t, ValidateVideoProfileRequest(customInputProfile, "reference_to_video", map[string]any{
		"storyboard": []any{"https://cdn.example/frame-1.png", "https://cdn.example/frame-2.png"},
	}, nil))
	assert.ErrorContains(t, ValidateVideoProfileRequest(customInputProfile, "reference_to_video", map[string]any{}, nil), "required")

	_, err = ApplyVideoModelOverrides(profile, VideoModelOverrides{Resolutions: []string{"4k"}})
	assert.ErrorContains(t, err, "expands")
	tooMany := 8
	_, err = ApplyVideoModelOverrides(profile, VideoModelOverrides{MaxInputItems: map[string]int{"reference_images": tooMany}})
	assert.ErrorContains(t, err, "expands")

	operationDuration := jsplugin.VideoDurationProfile{Min: 2, Max: 10, Step: 1, Default: 6}
	operationProfile := jsplugin.VideoProfile{Models: []string{"video-model"}, Modes: []jsplugin.VideoModeProfile{
		{Name: "edit_video", Inputs: []jsplugin.VideoInputProfile{{Name: "video", Kind: "video", Sources: []string{"url"}, MaxItems: 1, Required: true}}},
		{Name: "extend_video", Duration: &operationDuration, ExtensionDirections: []string{"forward", "backward"}, DefaultExtensionDirection: "backward", Inputs: []jsplugin.VideoInputProfile{{Name: "video", Kind: "video", Sources: []string{"url", "asset"}, MaxItems: 1, Required: true}}},
	}}
	editRequest := map[string]any{"prompt": "restyle", "video": "https://cdn.example/source.mp4"}
	require.NoError(t, ValidateVideoProfileRequest(operationProfile, "edit_video", editRequest, nil))
	editRequest["duration"] = float64(5)
	assert.ErrorContains(t, ValidateVideoProfileRequest(operationProfile, "edit_video", editRequest, nil), "fixed")
	delete(editRequest, "duration")
	delete(editRequest, "prompt")
	assert.ErrorContains(t, ValidateVideoProfileRequest(operationProfile, "edit_video", editRequest, nil), "prompt")
	editRequest["prompt"] = "restyle"
	editRequest["source_task_id"] = "old-task"
	assert.ErrorContains(t, ValidateVideoProfileRequest(operationProfile, "edit_video", editRequest, nil), "source_task_id")
	delete(editRequest, "source_task_id")

	extendRequest := map[string]any{"prompt": "continue", "video": "asset://source", "duration": float64(10), "extension_direction": "forward"}
	require.NoError(t, ValidateVideoProfileRequest(operationProfile, "extend_video", extendRequest, nil))
	extendRequest["extension_direction"] = "sideways"
	assert.ErrorContains(t, ValidateVideoProfileRequest(operationProfile, "extend_video", extendRequest, nil), "direction")
	extendRequest["extension_direction"] = "backward"
	extendRequest["seconds"] = float64(10)
	assert.ErrorContains(t, ValidateVideoProfileRequest(operationProfile, "extend_video", extendRequest, nil), "cannot both")

	directions := []string{"backward"}
	narrowedOperations, err := ApplyVideoModelOverrides(operationProfile, VideoModelOverrides{ExtensionDirections: directions, DefaultExtensionDirection: "backward"})
	require.NoError(t, err)
	assert.Empty(t, narrowedOperations.Modes[0].ExtensionDirections)
	assert.Equal(t, directions, narrowedOperations.Modes[1].ExtensionDirections)
	mixedAspectProfile := jsplugin.VideoProfile{Models: []string{"video-model"}, Modes: []jsplugin.VideoModeProfile{
		{Name: "text_to_video", AspectRatios: []string{"16:9", "9:16"}, DefaultAspectRatio: "16:9"},
		operationProfile.Modes[0],
	}}
	narrowedAspects, err := ApplyVideoModelOverrides(mixedAspectProfile, VideoModelOverrides{AspectRatios: []string{"9:16"}, DefaultAspectRatio: "9:16"})
	require.NoError(t, err)
	assert.Equal(t, []string{"9:16"}, narrowedAspects.Modes[0].AspectRatios)
	assert.Equal(t, "9:16", narrowedAspects.Modes[0].DefaultAspectRatio)
	assert.Empty(t, narrowedAspects.Modes[1].AspectRatios)
	_, err = ApplyVideoModelOverrides(jsplugin.VideoProfile{Models: []string{"video-model"}, Modes: []jsplugin.VideoModeProfile{operationProfile.Modes[0]}}, VideoModelOverrides{ExtensionDirections: directions})
	assert.ErrorContains(t, err, "unavailable")
}

func TestVideoPoolResolutionPresets(t *testing.T) {
	for _, testCase := range []struct {
		input, expected string
	}{
		{"480p", "1K"}, {"720p", "1K"}, {"1080p", "2K"}, {"2K", "2K"}, {"4K", "4K"}, {"1920x1080", "2K"}, {"3840x2160", "4K"},
	} {
		assert.Equal(t, testCase.expected, VideoPoolResolution(testCase.input), testCase.input)
	}
}

func TestVideoReadinessReportsSafeFailureReasons(t *testing.T) {
	previousDB := model.DB
	previousRedis, previousRedisEnabled := common.RDB, common.RedisEnabled
	previousSecret, previousPlugins := common.CryptoSecret, constant.TaskPluginEnabled
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "video-readiness.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	common.RDB = nil
	common.RedisEnabled = false
	common.CryptoSecret = ""
	constant.TaskPluginEnabled = false
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "")
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "")
	t.Cleanup(func() {
		model.DB = previousDB
		common.RDB, common.RedisEnabled = previousRedis, previousRedisEnabled
		common.CryptoSecret, constant.TaskPluginEnabled = previousSecret, previousPlugins
		connection, closeErr := db.DB()
		if assert.NoError(t, closeErr) {
			assert.NoError(t, connection.Close())
		}
	})
	require.NoError(t, db.AutoMigrate(&model.Option{}))
	require.NoError(t, SaveMediaRuntimeConfig(t.Context(), DefaultMediaRuntimeConfig()))

	readiness, err := GetVideoReadiness(t.Context())
	require.NoError(t, err)
	assert.False(t, readiness.Ready)
	require.Len(t, readiness.Checks, 5)
	for _, check := range readiness.Checks {
		assert.False(t, check.Ready, check.Key)
		assert.NotEmpty(t, check.Reason, check.Key)
		assert.NotContains(t, check.Reason, "ASYNC_IMAGE_PAYLOAD_KEYS")
		assert.NotContains(t, check.Reason, "CRYPTO_SECRET")
	}
}
