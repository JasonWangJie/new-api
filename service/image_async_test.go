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
	kittypes "github.com/QuantumNous/new-api/relaykit/types"
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

func asyncImageWorkerFixture(t *testing.T) (*gorm.DB, model.AsyncImageTask, AsyncImageRequest, ImageRuntimeConfig, model.Channel) {
	t.Helper()
	asyncImageKeyFixture(t)
	previousDB, previousRedis, previousEnabled := model.DB, common.RDB, common.RedisEnabled
	previousType := common.MainDatabaseType()
	previousCapability, previousEstimate, previousExecute := ImageChannelCapabilityFunc, EstimateAsyncImageQuotaFunc, ExecuteAsyncImageFunc
	dialector := gorm.Dialector(sqlite.Open(filepath.Join(t.TempDir(), "worker.db")))
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
	common.SetMainDatabaseType(common.DatabaseType(db.Dialector.Name()))
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	if address := os.Getenv("IMAGE_WORKER_TEST_REDIS_ADDR"); address != "" {
		require.Equal(t, "127.0.0.1:26380", address, "use the disposable local Redis instance")
		require.NoError(t, client.Close())
		client = redis.NewClient(&redis.Options{Addr: address, DB: 15, MaxRetries: -1})
		require.NoError(t, client.FlushDB(t.Context()).Err())
	}
	common.RDB, common.RedisEnabled = client, true
	entities := []any{&model.Channel{}, &model.Ability{}, &model.Option{}, &model.User{}, &model.Token{}, &model.UserSubscription{}, &model.SubscriptionPlan{}, &model.AsyncImageTask{}, &model.AsyncImageBill{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.ImageStorageProfile{}}
	t.Cleanup(func() {
		model.DB, common.RDB, common.RedisEnabled = previousDB, previousRedis, previousEnabled
		common.SetMainDatabaseType(previousType)
		ImageChannelCapabilityFunc, EstimateAsyncImageQuotaFunc, ExecuteAsyncImageFunc = previousCapability, previousEstimate, previousExecute
		assert.NoError(t, client.Close())
		assert.NoError(t, db.Migrator().DropTable(entities...))
		connection, openErr := db.DB()
		if assert.NoError(t, openErr) {
			assert.NoError(t, connection.Close())
		}
	})
	require.NoError(t, db.AutoMigrate(entities...))
	cfg := DefaultImageRuntimeConfig()
	cfg.AsyncEnabled, cfg.CircuitBreaker, cfg.RetryJitter = true, true, 0
	require.NoError(t, SaveImageRuntimeConfig(t.Context(), cfg))
	ImageChannelCapabilityFunc = func(model.Channel, string) dto.ImageCapability {
		return dto.ImageCapability{Provider: "openai", Protocol: "openai_images", Generate: true, Edit: true}
	}
	EstimateAsyncImageQuotaFunc = func(context.Context, model.Token, AsyncImageRequest, string) (int, error) {
		return 100, nil
	}
	user := model.User{Username: "worker-test", Group: "default", Quota: 1000, Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "worker-token-key", Group: "default", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}
	require.NoError(t, db.Create(&token).Error)
	aPriority, bPriority := int64(100), int64(10)
	a := model.Channel{Key: "upstream-key-a", Status: common.ChannelStatusEnabled, Type: 1, Group: "default", Models: "image-model", Priority: &aPriority}
	b := model.Channel{Key: "upstream-key-b", Status: common.ChannelStatusEnabled, Type: 1, Group: "default", Models: "image-model", Priority: &bPriority}
	require.NoError(t, db.Create(&a).Error)
	require.NoError(t, db.Create(&b).Error)
	require.NoError(t, db.Create(&[]model.Ability{{Group: "default", Model: "image-model", ChannelId: a.Id, Enabled: true}, {Group: "default", Model: "image-model", ChannelId: b.Id, Enabled: true}}).Error)
	catalog, err := common.Marshal([]ImageModelCapability{{Id: "image-model", MaxOutputImages: 1, MaxReferenceImages: 1}})
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.ImageGroupPolicy{PolicyKey: ImageIdentityHash("default", "openai"), Group: "default", Platform: "openai", Enabled: true, AsyncEnabled: true, PoolMode: "resolution", Models: string(catalog)}).Error)
	require.NoError(t, db.Create(&model.ImageStorageProfile{ProfileId: "worker-local", Class: "temporary", Backend: "local", Root: t.TempDir(), Active: true}).Error)
	now := time.Now().Unix()
	task := model.AsyncImageTask{TaskId: "asyncimg_worker_test", UserId: user.Id, TokenId: token.Id, Group: "default", Dialect: "async", Platform: "openai", Provider: "openai", ExecutionProtocol: "openai_images", Model: "image-model", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker-lease", LeaseExpiresAt: now + 120, StartedAt: now, CreatedAt: now, ChannelId: a.Id}
	request := AsyncImageRequest{Dialect: "async", Platform: "openai", Provider: "openai", PolicyPlatform: "openai", Model: "image-model", Count: 1, Kind: "text_to_image", SourcePath: "/v1/images/generations_async", Billing: &MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.0002, QuotaToPreConsume: 100}, Ratios: map[string]float64{"n": 1}}}
	data, err := common.Marshal(request)
	require.NoError(t, err)
	task.RequestCipher, err = EncryptImagePayload(data, "task:"+task.TaskId)
	require.NoError(t, err)
	data, err = common.Marshal(a)
	require.NoError(t, err)
	task.SelectedChannelCipher, err = EncryptImagePayload(data, "image-channel:"+task.TaskId)
	require.NoError(t, err)
	require.NoError(t, db.Create(&task).Error)
	return db, task, request, cfg, b
}

func asyncImageWorkerQuotaCacheFixture(t *testing.T, task model.AsyncImageTask) {
	t.Helper()
	previousBatch := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatch })
	_, err := model.GetUserCache(task.UserId)
	require.NoError(t, err)
	var token model.Token
	require.NoError(t, model.DB.First(&token, task.TokenId).Error)
	// Batch mode must use already initialized live quota rather than hydrate
	// a database snapshot. Set the same quota fields used by ordinary relays.
	require.NoError(t, common.RDB.HSet(t.Context(), "token:"+common.GenerateHMAC(token.Key), "Id", token.Id, "RemainQuota", token.RemainQuota, "UsedQuota", token.UsedQuota).Err())
}

func TestAsyncImageTimeoutRuntimeConfigCompatibility(t *testing.T) {
	db, _, _, cfg, _ := asyncImageWorkerFixture(t)
	encoded, err := common.Marshal(cfg)
	require.NoError(t, err)
	var legacy map[string]any
	require.NoError(t, common.Unmarshal(encoded, &legacy))
	delete(legacy, "upstream_timeout_max_retries")
	delete(legacy, "upstream_timeout_retry_base_seconds")
	delete(legacy, "upstream_timeout_retry_max_seconds")
	encoded, err = common.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.Option{}).Where(map[string]any{"key": "image_runtime_config"}).Update("value", string(encoded)).Error)
	loaded, err := GetImageRuntimeConfig(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, loaded.TimeoutRetries)
	assert.Equal(t, 5, loaded.TimeoutRetryBase)
	assert.Equal(t, 10, loaded.TimeoutRetryMax)
	assert.Equal(t, cfg, loaded, "legacy configurations inherit only the newly added defaults")

	disabled := loaded
	disabled.TimeoutRetries = 0
	require.NoError(t, SaveImageRuntimeConfig(t.Context(), disabled))
	loaded, err = GetImageRuntimeConfig(t.Context())
	require.NoError(t, err)
	assert.Equal(t, disabled, loaded, "an explicit zero must disable timeout account switching")
	for _, tc := range []struct {
		name                   string
		retries, base, maximum int
	}{
		{name: "negative retries", retries: -1, base: 5, maximum: 10},
		{name: "excessive retries", retries: 1001, base: 5, maximum: 10},
		{name: "zero retry delay", base: 0, maximum: 10},
		{name: "maximum below base", base: 5, maximum: 4},
		{name: "excessive maximum delay", base: 5, maximum: 86401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := disabled
			invalid.TimeoutRetries, invalid.TimeoutRetryBase, invalid.TimeoutRetryMax = tc.retries, tc.base, tc.maximum
			require.Error(t, SaveImageRuntimeConfig(t.Context(), invalid))
			preserved, err := GetImageRuntimeConfig(t.Context())
			require.NoError(t, err)
			assert.Equal(t, disabled, preserved, "a rejected setting must preserve the last working configuration")
		})
	}
}

func TestAsyncImageWorkerSafeRetryUsesLiveAccountAndFrozenPrice(t *testing.T) {
	for _, condition := range []string{"reference account exhausted", "channel circuit open", "busy channel uses alternate", "namespaced busy channel uses alternate", "selected key disabled before first dispatch", "completed upstream rate limit", "HTTP 429 capacity rejection", "completed upstream transient rejection", "single healthy account retains transient retry", "legacy compatibility rejection"} {
		t.Run(condition, func(t *testing.T) {
			db, task, request, cfg, alternate := asyncImageWorkerFixture(t)
			wantChannelID, wantKey := alternate.Id, alternate.Key
			fingerprint := ImageIdentityHash("image-account", strconv.Itoa(task.ChannelId), "upstream-key-a")
			attempts := []ImageChannelAttempt{{ChannelId: task.ChannelId, KeyFingerprint: fingerprint, Dispatched: true, Code: 606}}
			switch condition {
			case "reference account exhausted":
				attempts = nil
				for range cfg.ReferenceRetries + 1 {
					attempts = append(attempts, ImageChannelAttempt{ChannelId: task.ChannelId, KeyFingerprint: fingerprint, Dispatched: true, ReferenceFailure: true, Code: 602})
				}
			case "channel circuit open":
				for range cfg.FailureThreshold {
					require.NoError(t, RecordImageCircuit(t.Context(), "async", task.ChannelId, false, cfg))
				}
			case "busy channel uses alternate", "namespaced busy channel uses alternate":
				if condition == "namespaced busy channel uses alternate" {
					common.RDB.AddHook(common.RedisNamespaceHook{Prefix: "image-worker-scope"})
					require.NoError(t, SaveImageRuntimeConfig(t.Context(), cfg))
				}
				attempts = nil
				var primary model.Channel
				require.NoError(t, db.First(&primary, task.ChannelId).Error)
				limit := 1
				primary.SetSetting(dto.ChannelSettings{ImageConcurrencyLimit: &limit})
				require.NoError(t, db.Model(&primary).Update("setting", primary.Setting).Error)
				require.NoError(t, common.RDB.ZAdd(t.Context(), ImageChannelConcurrencyKey(task.ChannelId), &redis.Z{Score: float64(time.Now().Unix() + 120), Member: "busy-worker"}).Err())
			case "selected key disabled before first dispatch":
				attempts = nil
				channelInfo := model.ChannelInfo{IsMultiKey: true, MultiKeyStatusList: map[int]int{0: common.ChannelStatusManuallyDisabled}}
				require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", task.ChannelId).Update("channel_info", channelInfo).Error)
			case "completed upstream rate limit":
				attempts[0].Code, attempts[0].FinishedAt = 605, time.Now().Unix()
			case "HTTP 429 capacity rejection":
				attempts[0].Code, attempts[0].HTTPStatus, attempts[0].FinishedAt = 603, 429, time.Now().Unix()
			case "completed upstream transient rejection":
				attempts[0].FinishedAt = time.Now().Unix()
			case "legacy compatibility rejection":
				attempts[0].KeyFingerprint, attempts[0].Code, attempts[0].HTTPStatus = "", 610, http.StatusUnprocessableEntity
				attempts[0].ProviderCode, attempts[0].FinishedAt = "image_size_not_supported", time.Now().Unix()
			case "single healthy account retains transient retry":
				attempts[0].FinishedAt = time.Now().Unix()
				require.NoError(t, db.Model(&alternate).Update("status", common.ChannelStatusManuallyDisabled).Error)
				wantChannelID, wantKey = task.ChannelId, "upstream-key-a"
			}
			encoded, err := common.Marshal(attempts)
			require.NoError(t, err)
			task.Attempts = string(encoded)
			require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Update("attempts", task.Attempts).Error)
			var selected model.Channel
			ExecuteAsyncImageFunc = func(ctx context.Context, current model.AsyncImageTask, outbound AsyncImageRequest, channel *model.Channel, _ ImageRuntimeConfig, beforeInvoke func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				selected = *channel
				assert.Equal(t, request.Billing, outbound.Billing, "retry must preserve the admitted price")
				require.NoError(t, beforeInvoke())
				require.NoError(t, db.WithContext(ctx).Model(&model.AsyncImageTask{}).Where("task_id = ?", current.TaskId).Update("upstream_task_id", "accepted-alternate-job").Error)
				return nil, model.AsyncImageBill{}, &AsyncImagePending{}
			}
			require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
			assert.Equal(t, wantChannelID, selected.Id)
			assert.Equal(t, wantKey, selected.Key)
			var saved model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&saved).Error)
			assert.Equal(t, wantChannelID, saved.ChannelId)
			data, err := DecryptImagePayload(saved.SelectedChannelCipher, "image-channel:"+task.TaskId)
			require.NoError(t, err)
			var frozen model.Channel
			require.NoError(t, common.Unmarshal(data, &frozen))
			assert.Equal(t, wantChannelID, frozen.Id, "subsequent polls must use the accepted retry account")
			assert.Equal(t, wantKey, frozen.Key)
		})
	}
}

func TestAsyncImageWorkerPreparationFailures(t *testing.T) {
	for _, tc := range []struct {
		name, status, internalCode string
		cause                      error
		code                       int
	}{
		{name: "unclassified preparation diagnostic", status: model.ImageTaskFailed, internalCode: "request_preparation_failed", cause: errors.New("invalid local image override; Bearer private-key; https://signed.example/image?token=private"), code: 610},
		{name: "preparation deadline retries before dispatch", status: model.ImageTaskQueued, internalCode: "request_preparation_timeout", cause: context.DeadlineExceeded, code: 606},
		{name: "preparation network timeout retries before dispatch", status: model.ImageTaskQueued, internalCode: "request_preparation_timeout", cause: &url.Error{Op: "Get", URL: "https://signed.example/image?token=private", Err: os.ErrDeadlineExceeded}, code: 606},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, task, _, cfg, _ := asyncImageWorkerFixture(t)
			ExecuteAsyncImageFunc = func(context.Context, model.AsyncImageTask, AsyncImageRequest, *model.Channel, ImageRuntimeConfig, func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				return nil, model.AsyncImageBill{}, tc.cause
			}
			require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
			var saved model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&saved).Error)
			assert.Equal(t, tc.status, saved.Status)
			assert.Equal(t, tc.code, saved.PublicErrorCode)
			assert.Equal(t, tc.internalCode, saved.ErrorCode)
			assert.Contains(t, saved.ErrorMessage, "preparation")
			assert.NotContains(t, saved.ErrorMessage, "private-key")
			assert.NotContains(t, saved.ErrorMessage, "signed.example")
			assert.Zero(t, saved.DispatchedAt, "preparation failures did not submit upstream generation")
			if tc.status == model.ImageTaskQueued {
				assert.Equal(t, 1, saved.RetryCount)
				assert.Equal(t, 1, saved.TransientRetryCount)
				assert.NotEmpty(t, saved.RequestCipher)
			} else {
				assert.Contains(t, saved.ErrorMessage, "invalid local image override", "the original non-secret cause must survive")
				assert.Zero(t, saved.RetryCount, "an unknown permanent cause does not authorize blind account rotation")
			}
			var attempts []ImageChannelAttempt
			require.NoError(t, common.UnmarshalJsonStr(saved.Attempts, &attempts))
			require.Len(t, attempts, 1)
			assert.Equal(t, saved.ErrorMessage, attempts[0].ErrorMessage)
			assert.False(t, attempts[0].Dispatched)
		})
	}
}

func TestAsyncImageWorkerPolicyChangeOnlyRejectsNewSubmissions(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(strconv.FormatBool(accepted), func(t *testing.T) {
			db, task, request, cfg, _ := asyncImageWorkerFixture(t)
			if accepted {
				task.UpstreamTaskId, task.DispatchedAt = "accepted-original-job", time.Now().Unix()-1
				require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Updates(map[string]any{"upstream_task_id": task.UpstreamTaskId, "dispatched_at": task.DispatchedAt}).Error)
			}
			require.NoError(t, db.Model(&model.User{}).Where("id = ?", task.UserId).Update("quota", 900).Error)
			require.NoError(t, db.Model(&model.Token{}).Where("id = ?", task.TokenId).Updates(map[string]any{"remain_quota": 900, "used_quota": 100}).Error)
			require.NoError(t, db.Create(&model.AsyncImageBill{TaskId: task.TaskId, BillingRequestId: "async-image:" + task.TaskId, UserId: task.UserId, TokenId: task.TokenId, FundingSource: "wallet", ReservedAt: time.Now().Unix(), ReservedQuota: 100, ReservedTokenQuota: 100, Status: "reserved"}).Error)
			require.NoError(t, db.Model(&model.ImageGroupPolicy{}).Where(map[string]any{"group": "default"}).Updates(map[string]any{"async_enabled": false, "models": "[]"}).Error)
			require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", task.ChannelId).Updates(map[string]any{"status": common.ChannelStatusManuallyDisabled, "key": "replacement-key"}).Error)
			cfg.AsyncEnabled = false
			calls := 0
			ExecuteAsyncImageFunc = func(_ context.Context, current model.AsyncImageTask, outbound AsyncImageRequest, channel *model.Channel, _ ImageRuntimeConfig, _ func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				calls++
				assert.Equal(t, task.UpstreamTaskId, current.UpstreamTaskId)
				assert.Equal(t, task.ChannelId, channel.Id)
				assert.Equal(t, "upstream-key-a", channel.Key, "polling must keep the account that accepted the job")
				assert.Equal(t, request.Billing, outbound.Billing)
				return nil, model.AsyncImageBill{}, &AsyncImagePending{}
			}
			require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
			var saved model.AsyncImageTask
			var bill model.AsyncImageBill
			var user model.User
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&saved).Error)
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
			require.NoError(t, db.First(&user, task.UserId).Error)
			if accepted {
				assert.Equal(t, 1, calls)
				assert.Equal(t, model.ImageTaskInvoking, saved.Status)
				assert.Equal(t, "reserved", bill.Status)
				assert.Equal(t, 900, user.Quota)
				assert.Greater(t, saved.NextAttemptAt, time.Now().Unix())
			} else {
				assert.Zero(t, calls)
				assert.Equal(t, model.ImageTaskFailed, saved.Status)
				assert.Equal(t, "refunded", bill.Status)
				assert.Equal(t, 1000, user.Quota)
			}
		})
	}
}

func TestAsyncImageWorkerAcceptedTaskRequiresDurableBillingAndChannel(t *testing.T) {
	for _, missing := range []string{"billing reservation", "frozen channel configuration", "invalid encrypted image payload", "request could not be decrypted", "request is invalid"} {
		t.Run(missing, func(t *testing.T) {
			db, task, _, cfg, _ := asyncImageWorkerFixture(t)
			task.UpstreamTaskId, task.DispatchedAt = "accepted-job", time.Now().Unix()-1
			updates := map[string]any{"upstream_task_id": task.UpstreamTaskId, "dispatched_at": task.DispatchedAt}
			switch missing {
			case "frozen channel configuration", "invalid encrypted image payload":
				task.SelectedChannelCipher = nil
				if missing == "invalid encrypted image payload" {
					task.SelectedChannelCipher = []byte{0}
				}
				updates["selected_channel_cipher"] = task.SelectedChannelCipher
			case "request could not be decrypted":
				task.RequestCipher = []byte{0}
				updates["request_cipher"] = task.RequestCipher
			case "request is invalid":
				var err error
				task.RequestCipher, err = EncryptImagePayload([]byte(`{`), "task:"+task.TaskId)
				require.NoError(t, err)
				updates["request_cipher"] = task.RequestCipher
			}
			if missing != "billing reservation" {
				require.NoError(t, db.Create(&model.AsyncImageBill{TaskId: task.TaskId, UserId: task.UserId, TokenId: task.TokenId, ReservedAt: time.Now().Unix(), ReservedQuota: 100, FundingSource: "wallet", Status: "reserved"}).Error)
			}
			require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Updates(updates).Error)
			calls := 0
			ExecuteAsyncImageFunc = func(context.Context, model.AsyncImageTask, AsyncImageRequest, *model.Channel, ImageRuntimeConfig, func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				calls++
				return nil, model.AsyncImageBill{}, &AsyncImagePending{}
			}
			require.ErrorContains(t, invokeAsyncImageTask(t.Context(), &task, cfg), missing)
			assert.Zero(t, calls, "incomplete accepted-job state must not create a new reservation or change accounts")
			var saved model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&saved).Error)
			assert.Equal(t, model.ImageTaskInvoking, saved.Status)
			if missing != "billing reservation" {
				var bill model.AsyncImageBill
				require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
				assert.Equal(t, "reserved", bill.Status, "missing reconciliation data must not refund accepted work")
			}
		})
	}
}

func TestAsyncImageWorkerAcceptedTaskWaitsForLocalCapacityWithoutRefund(t *testing.T) {
	for _, capacity := range []string{"global", "channel"} {
		t.Run(capacity, func(t *testing.T) {
			db, task, _, cfg, _ := asyncImageWorkerFixture(t)
			cfg.ImageConcurrency, cfg.TotalRetries, cfg.CapacityRetries = 1, 0, 0
			task.UpstreamTaskId, task.DispatchedAt = "accepted-capacity-job", time.Now().Unix()-1
			require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Updates(map[string]any{"upstream_task_id": task.UpstreamTaskId, "dispatched_at": task.DispatchedAt}).Error)
			require.NoError(t, db.Model(&model.User{}).Where("id = ?", task.UserId).Update("quota", 900).Error)
			require.NoError(t, db.Create(&model.AsyncImageBill{TaskId: task.TaskId, UserId: task.UserId, TokenId: task.TokenId, ReservedAt: time.Now().Unix(), ReservedQuota: 100, FundingSource: "wallet", Status: "reserved"}).Error)
			busyKey := ImageRedisKey("invoke-slots")
			if capacity == "channel" {
				busyKey = ImageChannelConcurrencyKey(task.ChannelId)
			}
			require.NoError(t, common.RDB.ZAdd(t.Context(), busyKey, &redis.Z{Score: float64(time.Now().Unix() + 120), Member: "busy-worker"}).Err())
			calls := 0
			ExecuteAsyncImageFunc = func(_ context.Context, current model.AsyncImageTask, _ AsyncImageRequest, channel *model.Channel, _ ImageRuntimeConfig, _ func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				calls++
				assert.Equal(t, "accepted-capacity-job", current.UpstreamTaskId)
				assert.Equal(t, "upstream-key-a", channel.Key)
				return nil, model.AsyncImageBill{}, &AsyncImagePending{}
			}
			for range 3 {
				require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
				require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
				assert.Equal(t, model.ImageTaskInvoking, task.Status)
				assert.Zero(t, task.RetryCount)
				assert.Zero(t, task.CapacityRetryCount)
				assert.Equal(t, "accepted-capacity-job", task.UpstreamTaskId)
				assert.Empty(t, task.LeaseToken)
				assert.Greater(t, task.NextAttemptAt, time.Now().Unix())
				var bill model.AsyncImageBill
				var user model.User
				require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
				require.NoError(t, db.First(&user, task.UserId).Error)
				assert.Equal(t, "reserved", bill.Status)
				assert.Equal(t, 900, user.Quota)
				require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Update("next_attempt_at", time.Now().Unix()).Error)
				var err error
				task, err = model.ClaimAsyncImageTask(t.Context(), task.TaskId, common.GetUUID(), 120)
				require.NoError(t, err)
			}
			assert.Zero(t, calls)
			require.NoError(t, common.RDB.ZRem(t.Context(), busyKey, "busy-worker").Err())
			require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
			assert.Equal(t, 1, calls, "released capacity resumes the original upstream job")
		})
	}
}

type imageRoutingUnavailableHook struct{}

func (imageRoutingUnavailableHook) BeforeProcess(ctx context.Context, _ redis.Cmder) (context.Context, error) {
	return ctx, nil
}

func (imageRoutingUnavailableHook) AfterProcess(context.Context, redis.Cmder) error {
	return nil
}

func (imageRoutingUnavailableHook) BeforeProcessPipeline(ctx context.Context, commands []redis.Cmder) (context.Context, error) {
	for _, command := range commands {
		if command.Name() == "zcount" {
			return ctx, errors.New("injected routing Redis outage")
		}
	}
	return ctx, nil
}

func (imageRoutingUnavailableHook) AfterProcessPipeline(context.Context, []redis.Cmder) error {
	return nil
}

func TestAsyncImageWorkerLocalDependenciesDoNotSpendGenerationRetries(t *testing.T) {
	for _, condition := range []string{"global capacity", "channel capacity", "routing controls unavailable", "quota cache missing", "quota reservation uncertain"} {
		t.Run(condition, func(t *testing.T) {
			db, task, _, cfg, alternate := asyncImageWorkerFixture(t)
			cfg.ImageConcurrency, cfg.TotalRetries, cfg.CapacityRetries, cfg.TransientRetries = 2, 0, 0, 0
			if condition == "routing controls unavailable" {
				cfg.CircuitBreaker, cfg.AccountCooldownSeconds = false, 0
				require.NoError(t, SaveImageRuntimeConfig(t.Context(), cfg))
				common.RDB.AddHook(imageRoutingUnavailableHook{})
			}
			calls := 0
			ExecuteAsyncImageFunc = func(context.Context, model.AsyncImageTask, AsyncImageRequest, *model.Channel, ImageRuntimeConfig, func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				calls++
				if condition == "quota reservation uncertain" {
					return nil, model.AsyncImageBill{}, model.ErrImageQuotaCacheUncertain
				}
				return nil, model.AsyncImageBill{}, model.ErrImageQuotaCacheUnavailable
			}
			if condition == "global capacity" {
				require.NoError(t, common.RDB.ZAdd(t.Context(), ImageRedisKey("invoke-slots"), &redis.Z{Score: float64(time.Now().Unix() + 120), Member: "busy-1"}, &redis.Z{Score: float64(time.Now().Unix() + 120), Member: "busy-2"}).Err())
			}
			if condition == "channel capacity" {
				require.NoError(t, db.Model(&alternate).Update("status", common.ChannelStatusManuallyDisabled).Error)
				var channel model.Channel
				require.NoError(t, db.First(&channel, task.ChannelId).Error)
				limit := 1
				channel.SetSetting(dto.ChannelSettings{ImageConcurrencyLimit: &limit})
				require.NoError(t, db.Model(&channel).Update("setting", channel.Setting).Error)
				require.NoError(t, common.RDB.ZAdd(t.Context(), ImageChannelConcurrencyKey(task.ChannelId), &redis.Z{Score: float64(time.Now().Unix() + 120), Member: "busy-1"}).Err())
			}
			require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			assert.Zero(t, task.RetryCount)
			assert.Zero(t, task.CapacityRetryCount)
			assert.Zero(t, task.TransientRetryCount)
			assert.Zero(t, task.DispatchedAt)
			assert.NotEmpty(t, task.RequestCipher)
			var decision ImageRetryDecision
			require.NoError(t, common.UnmarshalJsonStr(task.RetryDecision, &decision))
			if condition == "quota reservation uncertain" {
				assert.Equal(t, model.ImageTaskExecutionUnknown, task.Status)
				assert.Equal(t, "quota_cache_uncertain", task.ErrorCode)
				assert.Equal(t, "reconcile_unknown", decision.Action)
				assert.Zero(t, task.NextAttemptAt)
				assert.Equal(t, 1, calls)
			} else {
				assert.Equal(t, model.ImageTaskQueued, task.Status)
				assert.Greater(t, task.NextAttemptAt, time.Now().Unix())
				assert.LessOrEqual(t, task.NextAttemptAt, task.StartedAt+int64(cfg.ExecutionTimeout))
				if condition == "quota cache missing" {
					assert.Equal(t, "wait_local_dependency", decision.Action)
					assert.Equal(t, "prepare", decision.Stage)
					assert.Equal(t, 1, calls)
					var attempts []ImageChannelAttempt
					require.NoError(t, common.UnmarshalJsonStr(task.Attempts, &attempts))
					require.Len(t, attempts, 1)
					assert.False(t, attempts[0].Dispatched)
					assert.Positive(t, attempts[0].FinishedAt)
					assert.Equal(t, 606, attempts[0].Code)
					require.NotNil(t, attempts[0].Decision)
					assert.Equal(t, "wait_local_dependency", attempts[0].Decision.Action)
				} else if condition == "routing controls unavailable" {
					assert.Equal(t, "wait_local_dependency", decision.Action)
					assert.Equal(t, "routing", decision.Stage)
					assert.Equal(t, "routing_controls_unavailable", task.ErrorCode)
					assert.Zero(t, calls)
				} else {
					assert.Equal(t, "wait_capacity", decision.Action)
					assert.Zero(t, calls)
				}
			}
			var user model.User
			require.NoError(t, db.First(&user, task.UserId).Error)
			assert.Equal(t, 1000, user.Quota)
		})
	}
}

func TestAsyncImageChannelTimeoutRespectsExecutionDeadline(t *testing.T) {
	for _, seconds := range []int{60, 600} {
		t.Run(strconv.Itoa(seconds), func(t *testing.T) {
			db, task, _, cfg, _ := asyncImageWorkerFixture(t)
			cfg.ExecutionTimeout = 120
			var channel model.Channel
			require.NoError(t, db.First(&channel, task.ChannelId).Error)
			channel.SetSetting(dto.ChannelSettings{ImageAttemptTimeoutSeconds: &seconds})
			require.NoError(t, db.Model(&channel).Update("setting", channel.Setting).Error)
			ExecuteAsyncImageFunc = func(ctx context.Context, _ model.AsyncImageTask, _ AsyncImageRequest, _ *model.Channel, _ ImageRuntimeConfig, _ func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				assert.InDelta(t, min(seconds, cfg.ExecutionTimeout), time.Until(deadline).Seconds(), 2)
				return nil, model.AsyncImageBill{}, &AsyncImageFailure{Code: 604, InternalCode: "invalid_input", Message: "confirmed rejection"}
			}
			require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
		})
	}
}

func TestAsyncImageRetryWaitCannotExceedDeadline(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(strconv.FormatBool(accepted), func(t *testing.T) {
			db, task, _, cfg, _ := asyncImageWorkerFixture(t)
			task.StartedAt = time.Now().Unix() - int64(cfg.ExecutionTimeout) + 5
			if accepted {
				task.UpstreamTaskId = "accepted-retry-job"
			}
			require.NoError(t, db.Model(&task).Updates(map[string]any{"started_at": task.StartedAt, "upstream_task_id": task.UpstreamTaskId}).Error)
			require.NoError(t, failAsyncImageInvocation(t.Context(), task, &AsyncImageFailure{Code: 606, InternalCode: "upstream_failed", Message: "temporarily unavailable", RetryAfter: 300 * time.Second}, cfg))
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			assert.Equal(t, task.StartedAt+int64(cfg.ExecutionTimeout), task.NextAttemptAt)
		})
	}
}

func TestAsyncImagePostprocessDiagnosticsAndUncertainQuota(t *testing.T) {
	for _, storage := range []bool{false, true} {
		t.Run(strconv.FormatBool(storage), func(t *testing.T) {
			db, task, _, cfg, _ := asyncImageWorkerFixture(t)
			task.UpstreamSucceededAt = time.Now().Unix()
			task.Status = model.ImageTaskBillingPending
			if storage {
				task.Status = model.ImageTaskUploading
			}
			require.NoError(t, db.Model(&task).Updates(map[string]any{"status": task.Status, "upstream_succeeded_at": task.UpstreamSucceededAt}).Error)
			cause := errors.Join(model.ErrImageQuotaCacheUncertain, errors.New("password=private-value"))
			if storage {
				cause = errors.New("image MIME mismatch: declared image/png, detected image/jpeg")
			}
			require.NoError(t, scheduleImagePostprocessRetry(t.Context(), task, cfg, cause, storage))
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			var decision ImageRetryDecision
			require.NoError(t, common.UnmarshalJsonStr(task.RetryDecision, &decision))
			assert.Zero(t, task.RetryCount)
			assert.Positive(t, task.UpstreamSucceededAt)
			if storage {
				assert.Contains(t, task.ErrorMessage, "MIME mismatch")
				assert.Greater(t, task.NextAttemptAt, time.Now().Unix())
				assert.Equal(t, "resume_output", decision.Action)
			} else {
				assert.Equal(t, model.ImageTaskBillingFailed, task.Status)
				assert.Equal(t, "billing_quota_uncertain", task.ErrorCode)
				assert.NotContains(t, task.ErrorMessage, "private-value")
				assert.Zero(t, task.NextAttemptAt, "an uncertain financial operation must not be automatically repeated")
				assert.Equal(t, "manual_recovery", decision.Action)
				assert.Equal(t, "quota_confirmation_unknown", decision.Reason)
				notice := ImageTaskFailureNotice(task)
				assert.False(t, notice.Retryable)
				assert.Equal(t, "billing", notice.Stage)
				assert.Equal(t, "Ask an administrator to reconcile the quota reservation before trying again.", notice.Suggestion)
			}
		})
	}
}

func TestAsyncImageFailureNoticePreservesSpecificCausesAndRecovery(t *testing.T) {
	const reconcile = "Do not submit again until an administrator has checked the upstream result and billing."
	const retrySubmission = "The task is scheduled for another submission within its retry limits."
	const retryPoll = "The existing upstream job will be checked again; no new generation will be submitted."
	testCases := []struct {
		name       string
		task       model.AsyncImageTask
		decision   ImageRetryDecision
		summary    string
		suggestion string
		retryable  bool
	}{
		{
			name:    "connection loss keeps its cause without guessing from message",
			task:    model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "upstream_transport_disconnected", PublicErrorCode: 608, ErrorMessage: "context deadline exceeded"},
			summary: "Upstream connection closed before the result was confirmed", suggestion: reconcile,
		},
		{
			name:    "unknown timeout",
			task:    model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "upstream_transport_timeout", PublicErrorCode: 608},
			summary: "Upstream request timed out", suggestion: reconcile,
		},
		{
			name:    "interrupted request",
			task:    model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "upstream_transport_canceled", PublicErrorCode: 608},
			summary: "Upstream request was interrupted", suggestion: reconcile,
		},
		{
			name:    "unreadable response",
			task:    model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "upstream_response_invalid", PublicErrorCode: 608},
			summary: "Upstream response could not be read or parsed", suggestion: reconcile,
		},
		{
			name:    "legacy unknown does not infer EOF from text",
			task:    model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "execution_unknown", PublicErrorCode: 608, ErrorMessage: "EOF"},
			summary: "Upstream execution could not be confirmed", suggestion: reconcile,
		},
		{
			name:    "unknown status with a legacy transient code remains conservative",
			task:    model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "upstream_failed", PublicErrorCode: 606},
			summary: "Upstream execution could not be confirmed", suggestion: reconcile,
		},
		{
			name:     "timeout with another scheduled account",
			task:     model.AsyncImageTask{Status: model.ImageTaskQueued, ErrorCode: "upstream_transport_timeout", PublicErrorCode: 606, NextAttemptAt: 100},
			decision: ImageRetryDecision{Version: 1, Action: "switch_account", Stage: "submit"},
			summary:  "Upstream request timed out", suggestion: retrySubmission, retryable: true,
		},
		{
			name:    "account pool exhausted",
			task:    model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "timeout_accounts_exhausted", PublicErrorCode: 606},
			summary: "No unused image account remains after request timeouts", suggestion: "Add an eligible account for this model and resolution, or check why the existing account timed out.",
		},
		{
			name:    "timeout retry limit reached",
			task:    model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "timeout_retries_exhausted", PublicErrorCode: 606},
			summary: "Image request timeout retry limit reached", suggestion: "Check upstream availability and account timeout settings before submitting again.",
		},
		{
			name:    "timeout overall deadline reached",
			task:    model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "timeout_deadline_exhausted", PublicErrorCode: 606},
			summary: "Image execution deadline expired after request timeouts", suggestion: "Check upstream availability and account timeout settings before submitting again.",
		},
		{
			name:     "known job timeout continues polling",
			task:     model.AsyncImageTask{Status: model.ImageTaskInvoking, ErrorCode: "upstream_transport_timeout", PublicErrorCode: 606, UpstreamTaskId: "accepted-job", NextAttemptAt: 100},
			decision: ImageRetryDecision{Version: 1, Action: "poll_same_job", Rule: "upstream_transient", Stage: "poll"},
			summary:  "Upstream request timed out", suggestion: retryPoll, retryable: true,
		},
		{
			name:     "pending known job is not presented as a failure",
			task:     model.AsyncImageTask{Status: model.ImageTaskInvoking, ErrorCode: "upstream_transport_timeout", PublicErrorCode: 606, UpstreamTaskId: "accepted-job", NextAttemptAt: 100},
			decision: ImageRetryDecision{Version: 1, Action: "poll_same_job", Rule: "upstream_pending", Stage: "poll"},
		},
		{
			name:    "confirmed output remains a postprocess failure",
			task:    model.AsyncImageTask{Status: model.ImageTaskStorageFailed, ErrorCode: "upstream_response_invalid", PublicErrorCode: 609, UpstreamSucceededAt: 50},
			summary: "Image post-processing needs attention", suggestion: "Check storage or billing recovery; do not repeat generation.",
		},
		{
			name:     "uncertain reservation is not automatically retried",
			task:     model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "quota_cache_uncertain", PublicErrorCode: 608, NextAttemptAt: 100},
			decision: ImageRetryDecision{Version: 1, Action: "switch_account", Stage: "prepare"},
			summary:  "Image quota reservation could not be confirmed", suggestion: "Ask an administrator to reconcile the quota reservation before trying again.",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			task := testCase.task
			if testCase.decision.Version != 0 {
				encoded, err := common.Marshal(testCase.decision)
				require.NoError(t, err)
				task.RetryDecision = string(encoded)
			}
			notice := ImageTaskFailureNotice(task)
			assert.Equal(t, testCase.summary, notice.Summary)
			assert.Equal(t, testCase.suggestion, notice.Suggestion)
			assert.Equal(t, testCase.retryable, notice.Retryable)
			if testCase.summary != "" {
				assert.Regexp(t, `[\p{Han}]`, notice.SummaryText())
			}
			if testCase.suggestion != "" {
				assert.Regexp(t, `[\p{Han}]`, notice.SuggestionText())
			}
			if testCase.decision.Stage != "" {
				assert.Equal(t, testCase.decision.Stage, notice.Stage)
			}
		})
	}
}

func TestAsyncImageFailureNoticeExplainsRecordedCauses(t *testing.T) {
	testCases := []struct {
		name        string
		task        model.AsyncImageTask
		attempts    []ImageChannelAttempt
		summary     string
		params      map[string]any
		summaryText string
	}{
		{name: "safety rejection", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_failed", PublicErrorCode: 601}, summary: "Upstream safety review did not pass", summaryText: "上游安全审核未通过"},
		{name: "reference byte limit", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_failed", PublicErrorCode: 604, ErrorMessage: "image_url exceeds 50MB limit"}, summary: "Reference image exceeds the upstream limit of {{limit}}", params: map[string]any{"limit": "50MB"}, summaryText: "参考图超过上游 50MB 限制"},
		{name: "measured size is not the limit", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_failed", PublicErrorCode: 604, ErrorMessage: "Reference image size 25.4MB exceeds limit 20MB"}, summary: "Reference image exceeds the upstream limit of {{limit}}", params: map[string]any{"limit": "20MB"}, summaryText: "参考图超过上游 20MB 限制"},
		{name: "Chinese byte limit", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_failed", PublicErrorCode: 604, ErrorMessage: "参考图大小25.4MB，上限20MB"}, summary: "Reference image exceeds the upstream limit of {{limit}}", params: map[string]any{"limit": "20MB"}, summaryText: "参考图超过上游 20MB 限制"},
		{name: "measured size without a limit is not guessed", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_failed", PublicErrorCode: 604, ErrorMessage: "image_url rejected, image size 25.4MB"}, summary: "Upstream rejected image input or parameters", summaryText: "上游拒绝了图片输入或参数"},
		{name: "recorded timeout duration", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "timeout_accounts_exhausted", PublicErrorCode: 606}, attempts: []ImageChannelAttempt{{StartedAt: 100, FinishedAt: 400, Dispatched: true, TimedOut: true}}, summary: "The generation attempt timed out after {{seconds}} seconds, and no alternative account is available", params: map[string]any{"seconds": int64(300)}, summaryText: "本次生图尝试耗时 300 秒后超时，且没有备用账号可切换"},
		{name: "undispatched time is not an upstream timeout", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "timeout_accounts_exhausted", PublicErrorCode: 606}, attempts: []ImageChannelAttempt{{StartedAt: 100, FinishedAt: 400, TimedOut: true}}, summary: "No unused image account remains after request timeouts", summaryText: "请求超时后，没有剩余账号可切换"},
		{name: "job rate limit shown without changing its public category", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_job_failed", PublicErrorCode: 610, ErrorMessage: "请求过于频繁,请稍后重试"}, summary: "Upstream rate limit exceeded", summaryText: "上游请求频率受限"},
		{name: "provider rate limit code", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_job_failed", PublicErrorCode: 610}, attempts: []ImageChannelAttempt{{ProviderCode: "rate_limit_exceeded", FinishedAt: 400}}, summary: "Upstream rate limit exceeded", summaryText: "上游请求频率受限"},
		{name: "explicit text response", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_failed", PublicErrorCode: 610, ErrorMessage: "抱歉，我无法直接生成或编辑图片，但可以帮您整理提示词。"}, summary: "The upstream returned text instead of an image", summaryText: "上游只返回了文字，没有返回图片"},
		{name: "prompt words are not a no-image diagnostic", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_failed", PublicErrorCode: 610, ErrorMessage: "Please generate an image with the text no image"}, summary: "Upstream rejected the request; the exact cause is not confirmed", summaryText: "上游拒绝了请求，具体原因尚未确认"},
		{name: "quoted prompt is not a text-only reply", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_failed", PublicErrorCode: 610, ErrorMessage: "Invalid request: prompt contains 我无法直接生成图片"}, summary: "Upstream rejected the request; the exact cause is not confirmed", summaryText: "上游拒绝了请求，具体原因尚未确认"},
		{name: "known empty output", task: model.AsyncImageTask{Status: model.ImageTaskStorageFailed, ErrorCode: "output_parse_failed", PublicErrorCode: 607, ErrorMessage: "Upstream image output is missing", UpstreamSucceededAt: 99}, summary: "The upstream did not return a usable image", summaryText: "上游没有返回可用图片"},
		{name: "local reference fetch failed", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "reference_download_failed", PublicErrorCode: 602, ErrorMessage: "Reference image 1: image reference download failed"}, summary: "The reference image could not be downloaded locally", summaryText: "本站下载参考图失败"},
		{name: "both reference transports failed", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "reference_download_failed", PublicErrorCode: 602}, attempts: []ImageChannelAttempt{{Dispatched: true, FinishedAt: 20, TransportUsed: "remote", ReferenceFailure: true, Code: 602, HTTPStatus: 502}, {ReferenceMode: "local", ReferenceFallback: true, FinishedAt: 40, Code: 602}}, summary: "The upstream could not fetch the reference image, and the local download fallback also failed", summaryText: "上游抓取参考图失败，本地回退下载也失败"},
		{name: "fallback marker alone does not invent prior upstream failure", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "reference_download_failed", PublicErrorCode: 602}, attempts: []ImageChannelAttempt{{ReferenceMode: "local", ReferenceFallback: true, FinishedAt: 40, Code: 602}}, summary: "The reference image could not be downloaded locally", summaryText: "本站下载参考图失败"},
		{name: "remote reference transport failed", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_failed", PublicErrorCode: 602}, attempts: []ImageChannelAttempt{{Dispatched: true, FinishedAt: 20, TransportUsed: "remote", ReferenceFailure: true, Code: 602}}, summary: "The upstream could not fetch the reference image", summaryText: "上游抓取参考图失败"},
		{name: "MIME mismatch is not a storage outage", task: model.AsyncImageTask{Status: model.ImageTaskStorageFailed, ErrorCode: "output_parse_failed", PublicErrorCode: 607, ErrorMessage: "declared image MIME does not match its bytes", UpstreamSucceededAt: 99}, summary: "The image format declared by the upstream does not match the image data", summaryText: "上游标注的图片格式与实际图片数据不一致"},
		{name: "invalid image container", task: model.AsyncImageTask{Status: model.ImageTaskStorageFailed, ErrorCode: "output_parse_failed", PublicErrorCode: 607, ErrorMessage: "invalid image container or excessive trailing data", UpstreamSucceededAt: 99}, summary: "The upstream image container is invalid or contains excessive trailing data", summaryText: "上游返回的图片容器校验失败：结构无效或尾部附加数据过多"},
		{name: "manual termination does not invent a restart", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "admin_terminated", PublicErrorCode: 608}, summary: "The task was manually terminated", summaryText: "任务已被手动终止"},
		{name: "typed EOF diagnostic", task: model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "upstream_transport_disconnected", PublicErrorCode: 608, ErrorMessage: "Upstream connection was interrupted after dispatch; execution could not be confirmed (EOF)"}, summary: "The upstream connection ended before returning a result (EOF)", summaryText: "上游尚未返回生成结果，连接就已结束（EOF）"},
		{name: "typed partial EOF diagnostic", task: model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "upstream_transport_disconnected", PublicErrorCode: 608, ErrorMessage: "Upstream connection was interrupted after dispatch; execution could not be confirmed (unexpected EOF)"}, summary: "The upstream connection ended before returning a complete result (unexpected EOF)", summaryText: "上游结果尚未完整返回，连接就意外结束（unexpected EOF）"},
		{name: "untrusted EOF mention does not become a diagnostic", task: model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "upstream_transport_disconnected", PublicErrorCode: 608, ErrorMessage: "provider echoed prompt EOF"}, summary: "Upstream connection closed before the result was confirmed", summaryText: "上游连接提前断开，生成结果未确认"},
		{name: "old safety attempt does not relabel a later disconnect", task: model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "upstream_transport_disconnected", PublicErrorCode: 608}, attempts: []ImageChannelAttempt{{Code: 601, ErrorMessage: "safety review failed", FinishedAt: 20}, {Code: 608, Dispatched: true, FinishedAt: 40}}, summary: "Upstream connection closed before the result was confirmed", summaryText: "上游连接提前断开，生成结果未确认"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			task := testCase.task
			if testCase.attempts != nil {
				encoded, err := common.Marshal(testCase.attempts)
				require.NoError(t, err)
				task.Attempts = string(encoded)
			}
			original := task
			notice := ImageTaskFailureNotice(task)
			assert.Equal(t, testCase.summary, notice.Summary)
			assert.Equal(t, testCase.params, notice.SummaryParams)
			expectedText := testCase.summaryText
			if expectedText == "" {
				expectedText = testCase.summary
			}
			assert.Equal(t, expectedText, notice.SummaryText())
			assert.NotContains(t, notice.SummaryText(), "{{")
			assert.Equal(t, original, task, "presentation must not modify task evidence or policy")
			assert.False(t, notice.Retryable)
		})
	}
	for _, unsafe := range []any{"Bearer secret-value", "https://signed.example/?token=secret", 300, -1.0} {
		notice := ImageFailureNotice{Summary: "Reference image exceeds the upstream limit of {{limit}}", SummaryParams: map[string]any{"limit": unsafe}}
		assert.NotContains(t, notice.SummaryText(), "{{")
		assert.NotContains(t, notice.SummaryText(), "secret")
	}
}

func TestAsyncImageFailureNoticeReadableHistoricalDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, code, message, summary, advice string
		publicCode                           int
		storageFailed                        bool
	}{
		{name: "legacy unknown category retains explicit size limit", code: "upstream_failed", publicCode: 610, message: "reference image exceeds limit: 20 MB limit exceeded", summary: "参考图超过上游 20MB 限制", advice: "压缩"},
		{name: "inline reference per-image limit", code: "upstream_failed", publicCode: 610, message: "Request rejected before routing: the inline image in the image field is too large. The per-image limit is 20971520 bytes (20 MiB).", summary: "参考图超过上游 20MiB 限制", advice: "压缩"},
		{name: "dimension multiple", code: "upstream_job_failed", publicCode: 610, message: "The requested image size is invalid. Width and height must both be divisible by 16.", summary: "图片宽度和高度都必须是 16 的整数倍", advice: "16 的整数倍"},
		{name: "unsupported reference source", code: "upstream_failed", publicCode: 610, message: "Provider API error: Only a specific YouTube video URL is supported (request id: 20261021090758101641609AE8GcbCM)", summary: "上游只支持指定格式的 YouTube 视频链接", advice: "链接格式"},
		{name: "unsupported thinking level", code: "upstream_failed", publicCode: 610, message: "AI Studio 请求参数无效: 模型不支持 thinking level", summary: "当前模型不支持请求中的思考级别参数", advice: "思考级别参数"},
		{name: "no usable output or invalid count is not assumed to exceed maximum", code: "output_parse_failed", publicCode: 609, storageFailed: true, message: "upstream must return between 1 and 128 complete images", summary: "上游未返回符合要求的完整图片结果", advice: "没有可用图片或数量不符合要求"},
		{name: "output URL security remains enforced", code: "output_parse_failed", publicCode: 609, storageFailed: true, message: "image reference requires an HTTPS URL without credentials", summary: "上游返回的图片地址不符合安全 HTTPS 要求", advice: "联系服务方修正结果地址"},
		{name: "explicit content rejection is distinguished from unknown", code: "upstream_failed", publicCode: 610, message: "The request violates content policies", summary: "上游因内容规则拒绝了本次请求", advice: "提示词和参考素材"},
		{name: "output policy rejection with typographic apostrophe", code: "upstream_failed", publicCode: 610, message: "We’re so sorry, but the image we created may violate our content policies. If you think we got it wrong, please retry or edit your prompt.", summary: "上游因内容规则拒绝了本次请求", advice: "提示词和参考素材"},
		{name: "input policy rejection with ASCII apostrophe", code: "upstream_failed", publicCode: 610, message: "We're so sorry, but the prompt may violate our content policies. If you think we got it wrong, please retry or edit your prompt.", summary: "上游因内容规则拒绝了本次请求", advice: "提示词和参考素材"},
		{name: "quoted policy phrase does not classify prompt text", code: "upstream_failed", publicCode: 610, message: "Invalid request: prompt contains content policies", summary: "上游拒绝了请求，具体原因尚未确认", advice: "核查请求格式"},
		{name: "unconfirmed rejection stays conservative", code: "upstream_failed", publicCode: 610, message: "Request contains an invalid argument.", summary: "上游拒绝了请求，具体原因尚未确认", advice: "核查请求格式"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: tc.code, PublicErrorCode: tc.publicCode, ErrorMessage: tc.message}
			if tc.storageFailed {
				task.Status, task.UpstreamSucceededAt = model.ImageTaskStorageFailed, 100
			}
			original := task
			notice := ImageTaskFailureNotice(task)
			assert.Equal(t, tc.summary, notice.SummaryText())
			assert.Contains(t, notice.SuggestionText(), tc.advice)
			assert.Contains(t, notice.MessageText(task.ErrorMessage), tc.summary+"\n处理建议：")
			assert.Contains(t, notice.MessageText(task.ErrorMessage), "\n原始错误："+tc.message)
			assert.NotEmpty(t, notice.Summary, "the translated UI still needs its English key")
			assert.False(t, notice.Retryable)
			assert.Equal(t, original, task, "historical diagnostic projection must remain read-only")
		})
	}
}

func TestAsyncImageFailureNoticeCompatibility(t *testing.T) {
	const raw = "No eligible upstream image provider supports the requested image size or resolution tier. Adjust size/resolution or select a compatible route. Bearer private-key https://signed.example/image?token=private"
	const summary = "The selected upstream route does not support the requested image size or resolution"
	for _, tc := range []struct {
		name       string
		status     string
		code       string
		publicCode int
		action     string
		reason     string
		upstreamID string
		successAt  int64
		nextAt     int64
		wantKey    string
		wantText   string
		wantAdvice string
		retryable  bool
	}{
		{name: "historical rejection explains why no retry occurred", status: model.ImageTaskFailed, code: "upstream_failed", publicCode: 610, action: "stop", reason: "unknown_error", wantKey: summary, wantText: "当前上游线路不支持本次请求的图片尺寸或分辨率档位。", wantAdvice: "本任务在上游拒绝后已停止，没有安排自动换号。请管理员检查支持该尺寸或分辨率的线路及账号，确认可用后再提交。"},
		{name: "scheduled failover preserves dimensions", status: model.ImageTaskQueued, code: "upstream_image_route_incompatible", publicCode: 603, action: "switch_account", reason: "incompatible_image_route", nextAt: 100, wantKey: summary, wantText: "当前上游线路不支持本次请求的图片尺寸或分辨率档位。", wantAdvice: "本任务已安排在重试限额内换号重试，并跳过不兼容账号；保留原图片尺寸和分辨率，不自动降低画质。", retryable: true},
		{name: "exhausted compatible accounts", status: model.ImageTaskFailed, code: "image_compatible_accounts_exhausted", publicCode: 603, action: "stop", reason: "no_compatible_account", wantKey: "No other eligible image account supports the requested size or resolution", wantText: "当前没有其他可用上游账号支持本次请求的图片尺寸或分辨率，已停止自动重试。", wantAdvice: "请管理员添加或启用支持该尺寸或分辨率的账号；也可自行选择受支持的尺寸或分辨率后新建任务。"},
		{name: "compatibility budget exhausted", status: model.ImageTaskFailed, code: "upstream_image_route_incompatible", publicCode: 603, action: "stop", reason: "compatibility_retry_budget_exhausted", wantKey: summary, wantText: "当前上游线路不支持本次请求的图片尺寸或分辨率档位。", wantAdvice: "自动重试次数已达到配置的预算上限，已停止继续提交。请管理员检查兼容线路和重试设置后再新建任务。"},
		{name: "total budget exhausted", status: model.ImageTaskFailed, code: "upstream_image_route_incompatible", publicCode: 603, action: "stop", reason: "total_retry_budget_exhausted", wantKey: summary, wantText: "当前上游线路不支持本次请求的图片尺寸或分辨率档位。", wantAdvice: "自动重试次数已达到配置的预算上限，已停止继续提交。请管理员检查兼容线路和重试设置后再新建任务。"},
		{name: "deadline prevents another submission", status: model.ImageTaskExpired, code: "execution_deadline", publicCode: 608, action: "stop", reason: "execution_deadline", wantKey: summary, wantText: "当前上游线路不支持本次请求的图片尺寸或分辨率档位。", wantAdvice: "任务已达到执行截止时间，已停止继续提交。请管理员检查兼容线路，确认后再新建任务。"},
		{name: "old size rejection cannot override unknown execution", status: model.ImageTaskExecutionUnknown, code: "upstream_transport_disconnected", publicCode: 608, action: "reconcile_unknown", reason: "execution_unknown", wantKey: "Upstream connection closed before the result was confirmed", wantText: "上游连接提前断开，生成结果未确认", wantAdvice: "管理员核查上游结果和计费前，请勿再次提交。"},
		{name: "known job continues polling without another generation", status: model.ImageTaskInvoking, code: "upstream_failed", publicCode: 610, action: "poll_same_job", reason: "accepted_job", upstreamID: "accepted-job", nextAt: 100, wantKey: summary, wantText: "当前上游线路不支持本次请求的图片尺寸或分辨率档位。", wantAdvice: "将继续查询原上游任务，不会提交新的生成请求。", retryable: true},
		{name: "accepted terminal job never promises another submission", status: model.ImageTaskFailed, code: "upstream_job_failed", publicCode: 603, action: "stop", upstreamID: "accepted-job", wantKey: summary, wantText: "当前上游线路不支持本次请求的图片尺寸或分辨率档位。", wantAdvice: "管理员核查上游结果和计费前，请勿再次提交。"},
		{name: "saved output never becomes size failover", status: model.ImageTaskStorageFailed, code: "upstream_image_route_incompatible", publicCode: 609, action: "manual_recovery", successAt: 90, wantKey: "Image post-processing needs attention", wantText: "图片后处理需要检查", wantAdvice: "请检查存储或计费恢复状态，不要重复生成。"},
		{name: "safety review cannot be relabeled as compatibility", status: model.ImageTaskFailed, code: "upstream_image_route_incompatible", publicCode: 601, action: "stop", wantKey: "Upstream safety review did not pass", wantText: "上游安全审核未通过", wantAdvice: "请查看上游说明；若审核失败不符合预期，请联系服务提供方。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history, err := common.Marshal([]ImageChannelAttempt{{ChannelId: 4, Dispatched: true, FinishedAt: 50, HTTPStatus: http.StatusUnprocessableEntity, ProviderCode: "image_size_not_supported", ErrorMessage: raw}})
			require.NoError(t, err)
			decision, err := common.Marshal(ImageRetryDecision{Version: 1, Action: tc.action, Reason: tc.reason, Stage: "submit"})
			require.NoError(t, err)
			task := model.AsyncImageTask{Status: tc.status, ErrorCode: tc.code, PublicErrorCode: tc.publicCode, ErrorMessage: raw, Attempts: string(history), RetryDecision: string(decision), UpstreamTaskId: tc.upstreamID, UpstreamSucceededAt: tc.successAt, NextAttemptAt: tc.nextAt}
			original := task
			notice := ImageTaskFailureNotice(task)
			assert.Equal(t, tc.wantKey, notice.Summary)
			assert.Equal(t, tc.wantAdvice, notice.SuggestionText())
			assert.Equal(t, tc.retryable, notice.Retryable)
			if tc.wantText != "" {
				assert.Equal(t, tc.wantText, notice.SummaryText())
				assert.Contains(t, notice.MessageText(raw), tc.wantText+"\n处理建议："+tc.wantAdvice+"\n原始错误：")
			} else {
				assert.Empty(t, notice.SummaryZh)
				assert.Equal(t, tc.wantKey, notice.SummaryText())
			}
			assert.Contains(t, notice.MessageText(raw), "No eligible upstream image provider")
			assert.NotContains(t, notice.MessageText(raw), "private-key")
			assert.NotContains(t, notice.MessageText(raw), "signed.example")
			assert.Equal(t, original, task, "readable explanations cannot alter historical retry evidence")
		})
	}
	notice := ImageTaskFailureNotice(model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_image_route_incompatible", PublicErrorCode: 603})
	message := notice.MessageText(raw + " password=private-password " + strings.Repeat("x", common.LocalLogContentLimit))
	assert.LessOrEqual(t, len([]rune(message)), common.LocalLogContentLimit)
	assert.True(t, strings.HasPrefix(message, "当前上游线路不支持本次请求的图片尺寸或分辨率档位。\n处理建议："))
	assert.NotContains(t, message, "private-key")
	assert.NotContains(t, message, "private-password")
}

func TestAsyncImageTransportFailureKeepsSafeEOFDiagnostics(t *testing.T) {
	testCases := []struct {
		name            string
		cause           error
		responseInvalid bool
		knownJob        bool
		internalCode    string
		diagnostic      string
	}{
		{name: "EOF", cause: io.EOF, internalCode: "upstream_transport_disconnected", diagnostic: " (EOF)"},
		{name: "unexpected EOF", cause: io.ErrUnexpectedEOF, internalCode: "upstream_transport_disconnected", diagnostic: " (unexpected EOF)"},
		{name: "truncated response", cause: io.ErrUnexpectedEOF, responseInvalid: true, internalCode: "upstream_response_invalid", diagnostic: " (unexpected EOF)"},
		{name: "known job EOF", cause: io.EOF, knownJob: true, internalCode: "upstream_transport_disconnected", diagnostic: " (EOF)"},
		{name: "timeout priority", cause: errors.Join(context.DeadlineExceeded, io.EOF), internalCode: "upstream_transport_timeout", diagnostic: " (EOF)"},
		{name: "response classification priority", cause: context.DeadlineExceeded, responseInvalid: true, internalCode: "upstream_response_invalid"},
		{name: "canceled request", cause: context.Canceled, internalCode: "upstream_transport_canceled"},
		{name: "untyped EOF text is not an EOF sentinel", cause: errors.New("EOF"), internalCode: "upstream_transport_disconnected"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			cause := &url.Error{Op: "Post", URL: "https://private-user:private-password@upstream.example/generate?api_key=private-key", Err: testCase.cause}
			wrapped := kittypes.NewError(cause, kittypes.ErrorCodeDoRequestFailed, kittypes.ErrOptionWithHideErrMsg("upstream request failed"))
			task := model.AsyncImageTask{}
			if testCase.knownJob {
				task.UpstreamTaskId = "accepted-job"
			}
			failure := AsyncImageTransportFailure(wrapped, task, testCase.responseInvalid, nil)
			assert.Equal(t, testCase.internalCode, failure.InternalCode)
			assert.Equal(t, !testCase.knownJob, failure.ExecutionUnknown)
			if testCase.knownJob {
				assert.Equal(t, 606, failure.Code)
				assert.Contains(t, failure.Message, "only the existing upstream job may be polled again")
			} else {
				assert.Equal(t, 608, failure.Code)
			}
			if testCase.diagnostic != "" {
				assert.Contains(t, failure.Message, testCase.diagnostic)
			} else {
				assert.NotContains(t, failure.Message, "EOF")
			}
			for _, sensitive := range []string{"private-user", "private-password", "upstream.example", "api_key", "private-key"} {
				assert.NotContains(t, failure.Message, sensitive)
			}
		})
	}
}

func TestAsyncImageKnownUpstreamPollReleasesLeaseAndStaysProcessing(t *testing.T) {
	ctx := context.Background()
	previousDB, previousRedis, previousType := model.DB, common.RDB, common.MainDatabaseType()
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
	common.SetMainDatabaseType(common.DatabaseType(db.Dialector.Name()))
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	common.RDB = client
	t.Setenv("ASYNC_IMAGE_REDIS_PREFIX", "image-poll-test")
	t.Cleanup(func() {
		model.DB, common.RDB = previousDB, previousRedis
		common.SetMainDatabaseType(previousType)
		_ = client.Close()
		assert.NoError(t, db.Migrator().DropTable(&model.ImageOutbox{}, &model.AsyncImageEvent{}, &model.AsyncImageTask{}))
		sqlDB, openErr := db.DB()
		if openErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}, &model.AsyncImageBill{}))
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}, &model.AsyncImageBill{}))
	var databaseVersion string
	require.NoError(t, db.Raw(databaseVersionQuery).Scan(&databaseVersion).Error)
	t.Logf("Database version: %s", databaseVersion)

	now := time.Now().Unix()
	task := model.AsyncImageTask{
		TaskId:          "asyncimg_known_upstream_poll",
		Status:          model.ImageTaskInvoking,
		Version:         1,
		UpstreamTaskId:  "upstream-job-1",
		ChannelId:       7,
		DispatchedAt:    now - 5,
		StartedAt:       now - 5,
		NextAttemptAt:   now,
		LeaseToken:      "active-worker",
		LeaseExpiresAt:  now + 120,
		CreatedAt:       now - 5,
		ErrorCode:       "upstream_poll_failed",
		ErrorMessage:    "Temporary upstream failure",
		PublicErrorCode: 606,
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
	assert.Empty(t, scheduled.ErrorCode, "a healthy provider poll clears the current error while preserving history")
	assert.Empty(t, scheduled.ErrorMessage)
	assert.Zero(t, scheduled.PublicErrorCode)
	assert.Equal(t, task.ChannelId, scheduled.ChannelId)
	assert.Equal(t, task.DispatchedAt, scheduled.DispatchedAt)
	scheduledVersion := scheduled.Version
	require.NoError(t, model.RecoverAsyncImageTasks(ctx, 100, 1200, 0))
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
	require.NoError(t, model.RecoverAsyncImageTasks(ctx, 100, 1200, 0))
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
	assert.Zero(t, recovered.RetryCount, "polling failures do not consume the submission budget")
}

func TestAsyncImageWorkerTerminalFailuresPreserveReplayBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failure      AsyncImageFailure
		billStatus   string
		wantStatus   string
		wantBill     string
		pollFailures int
	}{
		{name: "confirmed generation timeout", failure: AsyncImageFailure{Code: 606, InternalCode: "upstream_job_failed", UpstreamTerminal: true}, billStatus: "reserved", wantStatus: model.ImageTaskQueued, wantBill: "reserved"},
		{name: "policy rejection is terminal", failure: AsyncImageFailure{Code: 601, InternalCode: "upstream_job_failed", UpstreamTerminal: true}, billStatus: "reserved", wantStatus: model.ImageTaskFailed, wantBill: "refunded"},
		{name: "poll interruption stays with the existing job", failure: AsyncImageFailure{Code: 606, InternalCode: "upstream_poll_failed"}, billStatus: "reserved", wantStatus: model.ImageTaskInvoking, wantBill: "reserved"},
		{name: "consecutive poll errors do not prove a failed generation", failure: AsyncImageFailure{Code: 605, InternalCode: "upstream_failed"}, billStatus: "reserved", wantStatus: model.ImageTaskInvoking, wantBill: "reserved", pollFailures: 10},
		{name: "poll deadline retains the accepted hold", failure: AsyncImageFailure{Code: 608, InternalCode: "execution_deadline"}, billStatus: "reserved", wantStatus: model.ImageTaskExecutionUnknown, wantBill: "reserved"},
		{name: "unrecoverable query rejection requires reconciliation", failure: AsyncImageFailure{Code: 610, HTTPStatus: http.StatusForbidden}, billStatus: "reserved", wantStatus: model.ImageTaskExecutionUnknown, wantBill: "reserved"},
		{name: "unknown execution retains its hold", failure: AsyncImageFailure{Code: 608, ExecutionUnknown: true}, billStatus: "reserved", wantStatus: model.ImageTaskExecutionUnknown, wantBill: "reserved"},
		{name: "compatibility rejection cannot replace an accepted job", failure: AsyncImageFailure{Code: 603, InternalCode: "upstream_image_route_incompatible", HTTPStatus: http.StatusUnprocessableEntity, ProviderCode: "image_size_not_supported"}, billStatus: "reserved", wantStatus: model.ImageTaskExecutionUnknown, wantBill: "reserved"},
		{name: "terminal accepted compatibility failure cannot resubmit", failure: AsyncImageFailure{Code: 603, InternalCode: "upstream_image_route_incompatible", HTTPStatus: http.StatusUnprocessableEntity, ProviderCode: "image_size_not_supported", UpstreamTerminal: true}, billStatus: "reserved", wantStatus: model.ImageTaskFailed, wantBill: "refunded"},
		{name: "a settled bill cannot fund another submission", failure: AsyncImageFailure{Code: 606, InternalCode: "upstream_job_failed", UpstreamTerminal: true}, billStatus: "applied", wantStatus: model.ImageTaskFailed, wantBill: "applied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, task, _, cfg, _ := asyncImageWorkerFixture(t)
			task.UpstreamTaskId, task.DispatchedAt = "original-provider-job", time.Now().Unix()-1
			task.TransientRetryCount = tc.pollFailures
			if tc.wantStatus == model.ImageTaskInvoking {
				cfg.TotalRetries = 0
			}
			if tc.name == "poll deadline retains the accepted hold" {
				task.StartedAt = time.Now().Unix() - int64(cfg.ExecutionTimeout) - 1
			}
			attempts, err := common.Marshal([]ImageChannelAttempt{{ChannelId: task.ChannelId, KeyFingerprint: "original-account", StartedAt: task.DispatchedAt, Dispatched: true}})
			require.NoError(t, err)
			task.Attempts = string(attempts)
			require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Updates(map[string]any{"upstream_task_id": task.UpstreamTaskId, "dispatched_at": task.DispatchedAt, "attempts": task.Attempts, "transient_retry_count": task.TransientRetryCount, "started_at": task.StartedAt}).Error)
			require.NoError(t, db.Model(&model.User{}).Where("id = ?", task.UserId).Update("quota", 900).Error)
			require.NoError(t, db.Model(&model.Token{}).Where("id = ?", task.TokenId).Updates(map[string]any{"remain_quota": 900, "used_quota": 100}).Error)
			bill := model.AsyncImageBill{TaskId: task.TaskId, UserId: task.UserId, TokenId: task.TokenId, ReservedAt: time.Now().Unix(), ReservedQuota: 100, ReservedTokenQuota: 100, FundingSource: "wallet", Status: tc.billStatus}
			require.NoError(t, db.Create(&bill).Error)
			if tc.name == "poll deadline retains the accepted hold" {
				calls := 0
				ExecuteAsyncImageFunc = func(context.Context, model.AsyncImageTask, AsyncImageRequest, *model.Channel, ImageRuntimeConfig, func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
					calls++
					return nil, model.AsyncImageBill{}, errors.New("expired polling must not execute")
				}
				require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
				assert.Zero(t, calls)
			} else {
				require.NoError(t, failAsyncImageInvocation(t.Context(), task, &tc.failure, cfg))
			}
			var saved model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&saved).Error)
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
			assert.Equal(t, tc.wantStatus, saved.Status)
			assert.Equal(t, tc.wantBill, bill.Status)
			assert.Equal(t, 100, bill.ReservedQuota)
			assert.Equal(t, task.StartedAt, saved.StartedAt, "another account does not extend the execution deadline")
			if saved.Status == model.ImageTaskQueued {
				assert.Empty(t, saved.UpstreamTaskId)
				assert.Zero(t, saved.ChannelId)
				assert.Zero(t, saved.DispatchedAt)
				assert.Empty(t, saved.SelectedChannelCipher)
				assert.Equal(t, task.RequestCipher, saved.RequestCipher, "the original request and frozen price survive")
				assert.Equal(t, 1, saved.RetryCount)
			} else {
				assert.Equal(t, task.UpstreamTaskId, saved.UpstreamTaskId)
				assert.Zero(t, saved.RetryCount)
			}
			var history []ImageChannelAttempt
			require.NoError(t, common.UnmarshalJsonStr(saved.Attempts, &history))
			require.Len(t, history, 1)
			assert.Equal(t, task.UpstreamTaskId, history[0].UpstreamTaskID, "the previous provider job remains auditable")
			if saved.Status == model.ImageTaskExecutionUnknown {
				_, err := model.ClaimAsyncImageTask(t.Context(), task.TaskId, "another-worker", cfg.WorkerLease)
				assert.ErrorIs(t, err, model.ErrImageConflict)
			}
			if saved.Status == model.ImageTaskInvoking {
				assert.Equal(t, min(tc.pollFailures+1, cfg.TransientRetries+1), saved.TransientRetryCount, "polling backoff saturates without exhausting generation retries")
				require.NoError(t, db.Model(&saved).Update("next_attempt_at", time.Now().Unix()).Error)
				saved, err = model.ClaimAsyncImageTask(t.Context(), saved.TaskId, "next-poll", cfg.WorkerLease)
				require.NoError(t, err)
				require.NoError(t, model.ScheduleAsyncImagePoll(t.Context(), saved, time.Now().Unix()+10, "upstream_pending", ""))
				require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&saved).Error)
				assert.Zero(t, saved.TransientRetryCount, "a valid pending response resets consecutive polling failures")
			}
		})
	}
}

func TestAsyncImageWorkerInvocationSlotsRemainExclusiveAfterRenewal(t *testing.T) {
	_, task, _, cfg, _ := asyncImageWorkerFixture(t)
	now := time.Now().Unix()
	keys := []string{ImageRedisKey("invoke-slots"), ImageChannelConcurrencyKey(task.ChannelId)}
	for _, key := range keys {
		acquired, err := imageInvocationAcquire.Run(t.Context(), common.RDB, []string{key}, now, now+10, 1, "active-worker", 60).Int()
		require.NoError(t, err)
		require.Equal(t, 1, acquired)
	}
	retained, err := imageInvocationRenew.Run(t.Context(), common.RDB, keys, "active-worker", now+60, 60).Int()
	require.NoError(t, err)
	require.Equal(t, 1, retained)
	for _, key := range keys {
		acquired, err := imageInvocationAcquire.Run(t.Context(), common.RDB, []string{key}, now+11, now+60, 1, "competing-worker", 60).Int()
		require.NoError(t, err)
		assert.Zero(t, acquired, "renewed upstream work still occupies both capacity limits")
	}
	require.NoError(t, common.RDB.Del(t.Context(), keys[1]).Err())
	retained, err = imageInvocationRenew.Run(t.Context(), common.RDB, keys, "active-worker", now+120, cfg.WorkerLease).Int()
	require.NoError(t, err)
	assert.Zero(t, retained, "losing either capacity slot invalidates the existing ownership")
	remaining, err := common.RDB.Exists(t.Context(), keys[1]).Result()
	require.NoError(t, err)
	assert.Zero(t, remaining, "renewal never recreates a lost slot")
}

func TestAsyncImageWorkerCompletionRecoveryUsesLocalBudgets(t *testing.T) {
	for _, tc := range []struct {
		name             string
		failure          AsyncImageFailure
		paused           string
		noAutomaticRetry bool
		requeryUsage     bool
	}{
		{name: "download", failure: AsyncImageFailure{Code: 606, InternalCode: "result_download_failed"}, paused: model.ImageTaskStorageFailed},
		{name: "billing", failure: AsyncImageFailure{Code: 606, InternalCode: "bill_preparation_failed"}, paused: model.ImageTaskBillingFailed},
		{name: "saved output timeout", failure: AsyncImageFailure{Code: 608, InternalCode: "upstream_transport_timeout"}, paused: model.ImageTaskStorageFailed},
		{name: "size rejection after output acceptance remains local", failure: AsyncImageFailure{Code: 603, InternalCode: "upstream_image_route_incompatible", HTTPStatus: http.StatusUnprocessableEntity, ProviderCode: "image_size_not_supported"}, paused: model.ImageTaskStorageFailed},
		{name: "invalid output", failure: AsyncImageFailure{Code: 607, InternalCode: "output_parse_failed"}, paused: model.ImageTaskStorageFailed, noAutomaticRetry: true},
		{name: "missing checkpoint", failure: AsyncImageFailure{Code: 607, InternalCode: "completion_checkpoint_invalid"}, paused: model.ImageTaskStorageFailed, noAutomaticRetry: true},
		{name: "missing usage re-queries the same job", failure: AsyncImageFailure{Code: 606, InternalCode: "billing_usage_missing"}, paused: model.ImageTaskBillingFailed, requeryUsage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, task, request, cfg, _ := asyncImageWorkerFixture(t)
			cfg.TotalRetries, cfg.StorageRetries, cfg.BillingRetries, cfg.RetryBackoff = 0, 1, 1, 1
			task.StartedAt, task.DispatchedAt = time.Now().Unix()-int64(cfg.ExecutionTimeout)-60, time.Now().Unix()-5
			if tc.requeryUsage {
				task.UpstreamTaskId = "accepted-job-with-missing-usage"
			}
			require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Updates(map[string]any{"started_at": task.StartedAt, "dispatched_at": task.DispatchedAt, "upstream_task_id": task.UpstreamTaskId}).Error)
			require.NoError(t, db.Model(&model.User{}).Where("id = ?", task.UserId).Update("quota", 900).Error)
			require.NoError(t, db.Model(&model.Token{}).Where("id = ?", task.TokenId).Updates(map[string]any{"remain_quota": 900, "used_quota": 100}).Error)
			require.NoError(t, db.Create(&model.AsyncImageBill{TaskId: task.TaskId, UserId: task.UserId, TokenId: task.TokenId, ReservedAt: time.Now().Unix(), ReservedQuota: 100, ReservedTokenQuota: 100, FundingSource: "wallet", Status: "reserved"}).Error)
			if tc.name == "missing checkpoint" {
				task.UpstreamSucceededAt = time.Now().Unix()
				require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Update("upstream_succeeded_at", task.UpstreamSucceededAt).Error)
			} else {
				require.NoError(t, PersistAsyncImageCompletion(t.Context(), &task, request, AsyncImageCompletion{Body: []byte(`{"data":[]}`), Usage: &dto.Usage{}, Billing: *request.Billing}))
			}
			require.NoError(t, db.Model(&model.ImageGroupPolicy{}).Where(map[string]any{"group": "default"}).Update("async_enabled", false).Error)
			require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", task.ChannelId).Updates(map[string]any{"status": common.ChannelStatusManuallyDisabled, "key": "replacement-key"}).Error)
			cfg.AsyncEnabled = false
			calls := 0
			ExecuteAsyncImageFunc = func(ctx context.Context, current model.AsyncImageTask, outbound AsyncImageRequest, channel *model.Channel, _ ImageRuntimeConfig, beforeInvoke func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				calls++
				if tc.requeryUsage && calls > 1 {
					assert.Nil(t, outbound.Completion, "cached missing usage is replaced by another query")
					assert.Equal(t, task.UpstreamTaskId, current.UpstreamTaskId)
					assert.Equal(t, request.Billing, outbound.Billing)
					deadline, bounded := ctx.Deadline()
					require.True(t, bounded, "a completed-job usage query still has a per-attempt deadline")
					assert.LessOrEqual(t, time.Until(deadline), time.Duration(cfg.AttemptTimeout)*time.Second)
				} else {
					require.NotNil(t, outbound.Completion)
				}
				require.NotNil(t, outbound.Reservation)
				assert.Equal(t, "upstream-key-a", channel.Key)
				assert.Equal(t, task.UpstreamSucceededAt, current.UpstreamSucceededAt)
				assert.NoError(t, ctx.Err(), "the generation deadline does not expire a saved completion")
				require.NotNil(t, outbound.PostprocessContext)
				assert.NoError(t, outbound.PostprocessContext.Err())
				assert.ErrorIs(t, beforeInvoke(), model.ErrImageConflict, "completion recovery cannot dispatch another generation")
				return nil, model.AsyncImageBill{}, &tc.failure
			}
			invocationStarted := time.Now().Unix()
			require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
			var saved model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&saved).Error)
			if tc.noAutomaticRetry {
				assert.Equal(t, tc.paused, saved.Status)
				assert.Zero(t, saved.NextAttemptAt, "unusable outputs are not mechanically retried")
				assert.NotEmpty(t, saved.RequestCipher, "accepted output evidence survives validation failures")
				var bill model.AsyncImageBill
				require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
				assert.Equal(t, "reserved", bill.Status)
				if tc.name == "missing checkpoint" {
					assert.Zero(t, calls, "a marker without evidence never permits a fresh submission")
				} else {
					assert.Equal(t, 1, calls)
				}
				return
			}
			assert.Equal(t, model.ImageTaskInvoking, saved.Status)
			assert.Zero(t, saved.RetryCount)
			assert.Greater(t, saved.NextAttemptAt, invocationStarted)
			assert.LessOrEqual(t, saved.NextAttemptAt, time.Now().Unix()+int64(cfg.RetryBackoff))
			assert.NotEmpty(t, saved.RequestCipher)
			if tc.requeryUsage {
				data, err := DecryptImagePayload(saved.RequestCipher, "task:"+saved.TaskId)
				require.NoError(t, err)
				var savedRequest AsyncImageRequest
				require.NoError(t, common.Unmarshal(data, &savedRequest))
				assert.Nil(t, savedRequest.Completion)
				assert.Equal(t, request.Billing, savedRequest.Billing)
				assert.Equal(t, task.UpstreamSucceededAt, saved.UpstreamSucceededAt)
				assert.Equal(t, task.UpstreamTaskId, saved.UpstreamTaskId)
			}
			require.NoError(t, db.Model(&saved).Update("next_attempt_at", time.Now().Unix()).Error)
			var err error
			saved, err = model.ClaimAsyncImageTask(t.Context(), saved.TaskId, "local-completion-worker", cfg.WorkerLease)
			require.NoError(t, err)
			require.NoError(t, invokeAsyncImageTask(t.Context(), &saved, cfg))
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&saved).Error)
			assert.Equal(t, tc.paused, saved.Status)
			assert.Zero(t, saved.NextAttemptAt)
			assert.NotEmpty(t, saved.RequestCipher)
			assert.Equal(t, task.ChannelId, saved.ChannelId)
			assert.Equal(t, task.StartedAt, saved.StartedAt)
			assert.Equal(t, 2, calls)
			var bill model.AsyncImageBill
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
			assert.Equal(t, "reserved", bill.Status, "local recovery keeps the original hold")
			require.NoError(t, model.ResumeAsyncImageTask(t.Context(), saved))
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&saved).Error)
			assert.Equal(t, model.ImageTaskInvoking, saved.Status, "administrator recovery resumes the completion phase")
			assert.Zero(t, saved.StorageRetryCount)
			assert.Zero(t, saved.BillingRetryCount)
		})
	}
}

func TestAsyncImageWorkerExhaustedSelectionRetainsAccountHistory(t *testing.T) {
	db, task, _, cfg, alternate := asyncImageWorkerFixture(t)
	cfg.CapacityRetries = 1
	finished := ImageChannelAttempt{ChannelId: task.ChannelId, KeyFingerprint: ImageIdentityHash("image-account", strconv.Itoa(task.ChannelId), "upstream-key-a"), Dispatched: true, FinishedAt: time.Now().Unix(), Code: 606, HTTPStatus: http.StatusServiceUnavailable}
	encoded, err := common.Marshal([]ImageChannelAttempt{finished})
	require.NoError(t, err)
	task.ChannelId, task.DispatchedAt, task.Attempts = 0, 0, string(encoded)
	require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Updates(map[string]any{"channel_id": 0, "dispatched_at": 0, "attempts": task.Attempts}).Error)
	require.NoError(t, db.Model(&alternate).Update("status", common.ChannelStatusManuallyDisabled).Error)
	for range cfg.FailureThreshold {
		require.NoError(t, RecordImageCircuit(t.Context(), "async", finished.ChannelId, false, cfg))
	}
	calls := 0
	ExecuteAsyncImageFunc = func(context.Context, model.AsyncImageTask, AsyncImageRequest, *model.Channel, ImageRuntimeConfig, func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
		calls++
		return nil, model.AsyncImageBill{}, errors.New("exhausted selection must not execute")
	}
	require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
	assert.Equal(t, model.ImageTaskQueued, task.Status)
	assert.Equal(t, 1, task.CapacityRetryCount)
	assert.Equal(t, string(encoded), task.Attempts, "capacity failures do not overwrite the completed account failure")
	require.NoError(t, db.Model(&task).Update("next_attempt_at", time.Now().Unix()).Error)
	task, err = model.ClaimAsyncImageTask(t.Context(), task.TaskId, "capacity-retry", cfg.WorkerLease)
	require.NoError(t, err)
	require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
	assert.Equal(t, model.ImageTaskFailed, task.Status, "no-account waiting remains bounded")
	assert.Equal(t, string(encoded), task.Attempts)
	assert.Zero(t, calls)
}

func TestAsyncImageWorkerTimeoutAccountSwitch(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mode           string
		timeoutRetries int
		totalRetries   int
		wantCalls      int
		wantRetries    int
		wantError      string
	}{
		{name: "wrapped timeout then success", timeoutRetries: 2, totalRetries: 16, wantCalls: 2, wantRetries: 1},
		{name: "three accounts time out", mode: "all timeout", timeoutRetries: 2, totalRetries: 16, wantCalls: 3, wantRetries: 2, wantError: "timeout_retries_exhausted"},
		{name: "no alternate account", mode: "disabled alternate", timeoutRetries: 2, totalRetries: 16, wantCalls: 1, wantRetries: 1, wantError: "timeout_accounts_exhausted"},
		{name: "copied credential is not another account", mode: "copied credential", timeoutRetries: 2, totalRetries: 16, wantCalls: 1, wantRetries: 1, wantError: "timeout_accounts_exhausted"},
		{name: "timeout retries disabled", timeoutRetries: 0, totalRetries: 16, wantCalls: 1, wantError: "timeout_retries_exhausted"},
		{name: "total retry budget exhausted first", mode: "all timeout", timeoutRetries: 2, totalRetries: 1, wantCalls: 2, wantRetries: 1, wantError: "timeout_retries_exhausted"},
		{name: "execution deadline wins", mode: "deadline", timeoutRetries: 2, totalRetries: 16, wantCalls: 1, wantError: "timeout_deadline_exhausted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, task, request, cfg, alternate := asyncImageWorkerFixture(t)
			asyncImageWorkerQuotaCacheFixture(t, task)
			cfg.TimeoutRetries, cfg.TotalRetries = tc.timeoutRetries, tc.totalRetries
			require.NoError(t, SaveImageRuntimeConfig(t.Context(), cfg))
			switch tc.mode {
			case "all timeout":
				third := model.Channel{Key: "upstream-key-c", Status: common.ChannelStatusEnabled, Type: 1, Group: task.Group, Models: request.Model}
				require.NoError(t, db.Create(&third).Error)
				require.NoError(t, db.Create(&model.Ability{Group: task.Group, Model: request.Model, ChannelId: third.Id, Enabled: true}).Error)
			case "disabled alternate":
				require.NoError(t, db.Model(&alternate).Update("status", common.ChannelStatusManuallyDisabled).Error)
			case "copied credential":
				require.NoError(t, db.Model(&alternate).Update("key", "upstream-key-a").Error)
			}
			outputEntities := []any{&model.AsyncImageStagingObject{}, &model.ImageUploadIntent{}, &model.ImageStorageObject{}, &model.AsyncImageResult{}, &model.AsyncImageUsageProjection{}}
			if tc.wantError == "" {
				require.NoError(t, db.AutoMigrate(outputEntities...))
				t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(outputEntities...)) })
			}
			originalStartedAt := task.StartedAt
			selected := make([]string, 0)
			ExecuteAsyncImageFunc = func(ctx context.Context, current model.AsyncImageTask, outbound AsyncImageRequest, channel *model.Channel, _ ImageRuntimeConfig, beforeInvoke func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				assert.NotContains(t, selected, channel.Key, "a task must never return to a timed-out credential")
				selected = append(selected, channel.Key)
				assert.Equal(t, request.Billing, outbound.Billing)
				deadline, bounded := ctx.Deadline()
				require.True(t, bounded)
				assert.InDelta(t, 300, time.Until(deadline).Seconds(), 2)
				require.NoError(t, model.ReserveAsyncImageQuota(ctx, current, 100, model.ImageFundingSelection{Source: "wallet"}))
				require.NoError(t, beforeInvoke())
				var user model.User
				require.NoError(t, db.First(&user, current.UserId).Error)
				assert.Equal(t, 900, user.Quota, "account switching must reuse one financial reservation")
				if len(selected) == 1 || tc.mode == "all timeout" {
					if tc.mode == "deadline" {
						require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", current.TaskId).Update("started_at", time.Now().Unix()-int64(cfg.ExecutionTimeout)-1).Error)
					}
					cause := &url.Error{Op: "Post", URL: "https://supplier.example/generate?api_key=private-token", Err: context.DeadlineExceeded}
					wrapped := kittypes.NewError(cause, kittypes.ErrorCodeDoRequestFailed, kittypes.ErrOptionWithHideErrMsg("upstream error: do request failed"))
					wrapped = kittypes.NewError(wrapped, kittypes.ErrorCodeDoRequestFailed, kittypes.ErrOptionWithHideErrMsg("safe repeated message"))
					assert.ErrorIs(t, wrapped, context.DeadlineExceeded)
					var recovered *url.Error
					require.ErrorAs(t, wrapped, &recovered)
					assert.Same(t, cause, recovered)
					assert.Equal(t, "safe repeated message", wrapped.Error())
					assert.Equal(t, "safe repeated message", wrapped.ToOpenAIError().Message)
					return nil, model.AsyncImageBill{}, wrapped
				}
				image := asyncImageBytesFixture(t)
				return &AsyncImageOutput{Images: []ImageBytes{image}}, model.AsyncImageBill{TaskId: current.TaskId, UserId: current.UserId, TokenId: current.TokenId, ChannelId: channel.Id, FundingSource: "wallet", Quota: 80, Fingerprint: "timeout-success-bill", BillingRequestId: "async-image:" + current.TaskId, LogPayload: "{}"}, nil
			}
			for range 4 {
				require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
				require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
				if task.Status != model.ImageTaskQueued {
					break
				}
				assert.Zero(t, task.ChannelId)
				assert.Zero(t, task.DispatchedAt)
				assert.Empty(t, task.SelectedChannelCipher)
				assert.Equal(t, originalStartedAt, task.StartedAt)
				assert.GreaterOrEqual(t, task.NextAttemptAt, time.Now().Unix()+4)
				assert.LessOrEqual(t, task.NextAttemptAt, time.Now().Unix()+10)
				var decision ImageRetryDecision
				require.NoError(t, common.UnmarshalJsonStr(task.RetryDecision, &decision))
				assert.Equal(t, "switch_account", decision.Action)
				assert.Equal(t, task.RetryCount, decision.Used, "the audit counts scheduled account switches")
				require.NoError(t, db.Model(&task).Update("next_attempt_at", time.Now().Unix()).Error)
				var err error
				task, err = model.ClaimAsyncImageTask(t.Context(), task.TaskId, common.GetUUID(), cfg.WorkerLease)
				require.NoError(t, err)
			}
			assert.Len(t, selected, tc.wantCalls)
			assert.Equal(t, tc.wantRetries, task.RetryCount)
			var history []ImageChannelAttempt
			require.NoError(t, common.UnmarshalJsonStr(task.Attempts, &history))
			require.Len(t, history, tc.wantCalls)
			for i, attempt := range history {
				assert.True(t, attempt.Dispatched)
				assert.True(t, attempt.RetryOnTimeout)
				assert.NotEmpty(t, attempt.CredentialFingerprint)
				assert.Equal(t, tc.wantError != "" || i == 0, attempt.TimedOut)
				assert.NotContains(t, attempt.ErrorMessage, "private-token")
			}
			if tc.wantError == "" {
				require.Equal(t, model.ImageTaskUpstreamSucceeded, task.Status)
				require.NoError(t, UploadAsyncImageTask(t.Context(), &task, cfg))
				require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
				require.NoError(t, model.ApplyAsyncImageBill(t.Context(), task.TaskId, "timeout-success-bill"))
				require.NoError(t, model.ApplyAsyncImageBill(t.Context(), task.TaskId, "timeout-success-bill"))
				require.NoError(t, db.Model(&model.AsyncImageBill{}).Where("task_id = ?", task.TaskId).Update("log_status", "disabled").Error)
				require.NoError(t, model.CompleteAsyncImageTask(t.Context(), task, false))
			} else {
				assert.Equal(t, model.ImageTaskFailed, task.Status)
				assert.Equal(t, tc.wantError, task.ErrorCode)
				require.NoError(t, model.RefundAsyncImageReservation(t.Context(), task.TaskId))
			}
			var user model.User
			var token model.Token
			var bill model.AsyncImageBill
			require.NoError(t, db.First(&user, task.UserId).Error)
			require.NoError(t, db.First(&token, task.TokenId).Error)
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			var bills, refunds int64
			require.NoError(t, db.Model(&model.AsyncImageBill{}).Where("task_id = ?", task.TaskId).Count(&bills).Error)
			require.NoError(t, db.Model(&model.AsyncImageEvent{}).Where("task_id = ? AND event_type = ?", task.TaskId, "reservation_refunded").Count(&refunds).Error)
			assert.EqualValues(t, 1, bills)
			cachedWallet, err := common.RDB.HGet(t.Context(), "user:"+strconv.Itoa(task.UserId), "Quota").Int()
			require.NoError(t, err)
			cachedToken, err := common.RDB.HGet(t.Context(), "token:"+common.GenerateHMAC(token.Key), "RemainQuota").Int()
			require.NoError(t, err)
			assert.Equal(t, user.Quota, cachedWallet)
			assert.Equal(t, token.RemainQuota, cachedToken)
			if tc.wantError == "" {
				assert.Equal(t, model.ImageTaskSucceeded, task.Status)
				assert.Equal(t, "applied", bill.Status)
				assert.Equal(t, 920, user.Quota)
				assert.Equal(t, 920, token.RemainQuota)
				assert.Equal(t, 80, token.UsedQuota)
				assert.Equal(t, 1, user.RequestCount)
				assert.Zero(t, refunds)
			} else {
				assert.Equal(t, "refunded", bill.Status)
				assert.Equal(t, 1000, user.Quota)
				assert.Equal(t, 1000, token.RemainQuota)
				assert.Zero(t, token.UsedQuota)
				assert.EqualValues(t, 1, refunds, "terminal retry and repeated refund must release the hold once")
			}
		})
	}
	wrapped := kittypes.NewError(context.DeadlineExceeded, kittypes.ErrorCodeDoRequestFailed, kittypes.ErrOptionWithHideErrMsg("safe error"))
	wrapped.SetMessage("replacement")
	assert.Equal(t, "replacement", wrapped.Error())
	assert.NotErrorIs(t, wrapped, context.DeadlineExceeded, "SetMessage preserves its existing cause-resetting contract")
}

func TestAsyncImageWorkerSingleAccountTransientRetriesRemainBounded(t *testing.T) {
	db, task, _, cfg, alternate := asyncImageWorkerFixture(t)
	cfg.TransientRetries = 2
	cfg.AccountCooldownSeconds = 0 // This case isolates submission retry limits.
	require.NoError(t, SaveImageRuntimeConfig(t.Context(), cfg))
	require.NoError(t, db.Model(&alternate).Update("status", common.ChannelStatusManuallyDisabled).Error)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", task.TokenId).Update("unlimited_quota", true).Error)
	channelID := task.ChannelId
	calls := 0
	ExecuteAsyncImageFunc = func(ctx context.Context, current model.AsyncImageTask, _ AsyncImageRequest, channel *model.Channel, _ ImageRuntimeConfig, beforeInvoke func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
		calls++
		assert.Equal(t, channelID, channel.Id)
		require.NoError(t, model.ReserveAsyncImageQuota(ctx, current, 100, model.ImageFundingSelection{Source: "wallet"}))
		require.NoError(t, beforeInvoke())
		return nil, model.AsyncImageBill{}, &AsyncImageFailure{Code: 606, InternalCode: "upstream_failed", HTTPStatus: http.StatusServiceUnavailable}
	}
	for attempt := range cfg.TransientRetries + 1 {
		require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
		require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
		var user model.User
		var bill model.AsyncImageBill
		require.NoError(t, db.First(&user, task.UserId).Error)
		require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
		if attempt < cfg.TransientRetries {
			assert.Equal(t, model.ImageTaskQueued, task.Status)
			assert.Equal(t, 900, user.Quota, "same-account retries reuse the original reservation")
			assert.Equal(t, "reserved", bill.Status)
			require.NoError(t, db.Model(&task).Update("next_attempt_at", time.Now().Unix()).Error)
			var err error
			task, err = model.ClaimAsyncImageTask(t.Context(), task.TaskId, common.GetUUID(), cfg.WorkerLease)
			require.NoError(t, err)
		} else {
			assert.Equal(t, model.ImageTaskFailed, task.Status)
			assert.Equal(t, 1000, user.Quota)
			assert.Equal(t, "refunded", bill.Status)
		}
	}
	assert.Equal(t, cfg.TransientRetries+1, calls)
	assert.Equal(t, cfg.TransientRetries, task.RetryCount)
}

func TestAsyncImageWorkerCompatibilityFailover(t *testing.T) {
	const compatibilityMessage = "No eligible upstream image provider supports the requested image size or resolution tier. Adjust size/resolution or select a compatible route."
	for _, tc := range []struct {
		name       string
		mode       string
		wantCalls  int
		wantRetry  int
		wantError  string
		wantReason string
	}{
		{name: "explicit rejection then compatible success", wantCalls: 2, wantRetry: 1},
		{name: "copied credential cannot become an alternative", mode: "copied credential", wantCalls: 2, wantRetry: 1},
		{name: "no compatible account remains", mode: "no alternate", wantCalls: 1, wantRetry: 1, wantError: "image_compatible_accounts_exhausted", wantReason: "no_compatible_account"},
		{name: "all compatible accounts reject", mode: "all reject", wantCalls: 2, wantRetry: 2, wantError: "image_compatible_accounts_exhausted", wantReason: "no_compatible_account"},
		{name: "capacity retries disabled", mode: "no capacity retry", wantCalls: 1, wantError: "upstream_image_route_incompatible", wantReason: "compatibility_retry_budget_exhausted"},
		{name: "total retries disabled", mode: "no total retry", wantCalls: 1, wantError: "upstream_image_route_incompatible", wantReason: "total_retry_budget_exhausted"},
		{name: "ordinary invalid dimensions remain terminal", mode: "invalid dimensions", wantCalls: 1, wantError: "upstream_failed", wantReason: "invalid_input"},
		{name: "safety rejection remains terminal", mode: "safety", wantCalls: 1, wantError: "upstream_failed", wantReason: "content_policy"},
		{name: "unknown execution is never resubmitted", mode: "unknown", wantCalls: 1, wantError: "upstream_image_route_incompatible", wantReason: "execution_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, task, request, cfg, alternate := asyncImageWorkerFixture(t)
			asyncImageWorkerQuotaCacheFixture(t, task)
			request.Size, request.Resolution = "4096x3272", "4K"
			catalog, err := common.Marshal([]ImageModelCapability{{Id: request.Model, Resolutions: []string{"4K"}, MaxOutputImages: 1, MaxReferenceImages: 1}})
			require.NoError(t, err)
			require.NoError(t, db.Model(&model.ImageGroupPolicy{}).Where("policy_key = ?", ImageIdentityHash("default", "openai")).Update("models", string(catalog)).Error)
			payload, err := common.Marshal(request)
			require.NoError(t, err)
			task.RequestCipher, err = EncryptImagePayload(payload, "task:"+task.TaskId)
			require.NoError(t, err)
			require.NoError(t, db.Model(&task).Update("request_cipher", task.RequestCipher).Error)
			primaryID, startedAt := task.ChannelId, task.StartedAt
			switch tc.mode {
			case "copied credential":
				priority := int64(90)
				duplicate := model.Channel{Key: "upstream-key-a", Status: common.ChannelStatusEnabled, Type: 1, Group: task.Group, Models: task.Model, Priority: &priority}
				require.NoError(t, db.Create(&duplicate).Error)
				require.NoError(t, db.Create(&model.Ability{Group: task.Group, Model: task.Model, ChannelId: duplicate.Id, Enabled: true}).Error)
			case "no alternate":
				require.NoError(t, db.Model(&alternate).Update("status", common.ChannelStatusManuallyDisabled).Error)
			case "no capacity retry":
				cfg.CapacityRetries = 0
			case "no total retry":
				cfg.TotalRetries = 0
			}
			require.NoError(t, SaveImageRuntimeConfig(t.Context(), cfg))
			outputEntities := []any{&model.AsyncImageStagingObject{}, &model.ImageUploadIntent{}, &model.ImageStorageObject{}, &model.AsyncImageResult{}, &model.AsyncImageUsageProjection{}}
			if tc.wantError == "" {
				require.NoError(t, db.AutoMigrate(outputEntities...))
				t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(outputEntities...)) })
			}
			code, message := "image_size_not_supported", compatibilityMessage
			switch tc.mode {
			case "invalid dimensions":
				code, message = "invalid_request", "unsupported_image_dimensions: dimensions must be even"
			case "safety":
				code, message = "content_policy_violation", "Upstream safety review rejected the image"
			}
			body, err := common.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
			require.NoError(t, err)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write(body)
			}))
			t.Cleanup(upstream.Close)
			var selected []string
			ExecuteAsyncImageFunc = func(ctx context.Context, current model.AsyncImageTask, outbound AsyncImageRequest, channel *model.Channel, _ ImageRuntimeConfig, beforeInvoke func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				assert.NotContains(t, selected, channel.Key, "a rejected credential cannot be reused for the same dimensions")
				selected = append(selected, channel.Key)
				assert.Equal(t, request.Size, outbound.Size, "failover never reduces image dimensions")
				assert.Equal(t, request.Resolution, outbound.Resolution)
				assert.Equal(t, request.Billing, outbound.Billing, "failover retains the admitted price")
				require.NoError(t, model.ReserveAsyncImageQuota(ctx, current, 100, model.ImageFundingSelection{Source: "wallet"}))
				require.NoError(t, beforeInvoke())
				var user model.User
				require.NoError(t, db.First(&user, current.UserId).Error)
				assert.Equal(t, 900, user.Quota, "all attempts share one reservation")
				if len(selected) == 1 || tc.mode == "all reject" {
					httpRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, nil)
					require.NoError(t, requestErr)
					response, responseErr := upstream.Client().Do(httpRequest)
					require.NoError(t, responseErr)
					failure := ClassifyOpenAIAsyncImageHTTPFailure(ctx, response, "")
					if tc.mode == "unknown" {
						failure.ExecutionUnknown = true
					}
					return nil, model.AsyncImageBill{}, failure
				}
				assert.Equal(t, alternate.Id, channel.Id)
				return &AsyncImageOutput{Images: []ImageBytes{asyncImageBytesFixture(t)}}, model.AsyncImageBill{TaskId: current.TaskId, UserId: current.UserId, TokenId: current.TokenId, ChannelId: channel.Id, FundingSource: "wallet", Quota: 80, Fingerprint: "compatible-success-bill", BillingRequestId: "async-image:" + current.TaskId, LogPayload: "{}"}, nil
			}
			for range 3 {
				require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
				require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
				if task.Status != model.ImageTaskQueued {
					break
				}
				var decision ImageRetryDecision
				require.NoError(t, common.UnmarshalJsonStr(task.RetryDecision, &decision))
				assert.Equal(t, "switch_account", decision.Action)
				assert.Equal(t, "incompatible_image_route", decision.Reason)
				assert.Equal(t, "capacity", decision.Budget)
				assert.Zero(t, task.DispatchedAt)
				assert.Equal(t, startedAt, task.StartedAt)
				require.NoError(t, db.Model(&task).Update("next_attempt_at", time.Now().Unix()).Error)
				task, err = model.ClaimAsyncImageTask(t.Context(), task.TaskId, common.GetUUID(), cfg.WorkerLease)
				require.NoError(t, err)
			}
			assert.Len(t, selected, tc.wantCalls)
			assert.Equal(t, tc.wantRetry, task.RetryCount)
			if tc.wantError == "" {
				require.Equal(t, model.ImageTaskUpstreamSucceeded, task.Status)
				require.NoError(t, UploadAsyncImageTask(t.Context(), &task, cfg))
				require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
				require.NoError(t, model.ApplyAsyncImageBill(t.Context(), task.TaskId, "compatible-success-bill"))
				require.NoError(t, model.ApplyAsyncImageBill(t.Context(), task.TaskId, "compatible-success-bill"))
				require.NoError(t, db.Model(&model.AsyncImageBill{}).Where("task_id = ?", task.TaskId).Update("log_status", "disabled").Error)
				require.NoError(t, model.CompleteAsyncImageTask(t.Context(), task, false))
			} else {
				var decision ImageRetryDecision
				require.NoError(t, common.UnmarshalJsonStr(task.RetryDecision, &decision))
				assert.Equal(t, tc.wantError, task.ErrorCode)
				assert.Equal(t, tc.wantReason, decision.Reason)
				if tc.mode == "unknown" {
					assert.Equal(t, model.ImageTaskExecutionUnknown, task.Status)
					assert.Equal(t, "reconcile_unknown", decision.Action)
				} else {
					assert.Equal(t, model.ImageTaskFailed, task.Status)
					assert.Equal(t, "stop", decision.Action)
					require.NoError(t, model.RefundAsyncImageReservation(t.Context(), task.TaskId))
				}
			}
			var user model.User
			var bill model.AsyncImageBill
			require.NoError(t, db.First(&user, task.UserId).Error)
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
			var bills, refunds int64
			require.NoError(t, db.Model(&model.AsyncImageBill{}).Where("task_id = ?", task.TaskId).Count(&bills).Error)
			require.NoError(t, db.Model(&model.AsyncImageEvent{}).Where("task_id = ? AND event_type = ?", task.TaskId, "reservation_refunded").Count(&refunds).Error)
			assert.EqualValues(t, 1, bills)
			wantQuota, wantBill, wantRefunds := 1000, "refunded", int64(1)
			if tc.wantError == "" {
				wantQuota, wantBill, wantRefunds = 920, "applied", 0
			} else if tc.mode == "unknown" {
				wantQuota, wantBill, wantRefunds = 900, "reserved", 0
			}
			assert.Equal(t, wantQuota, user.Quota)
			assert.Equal(t, wantBill, bill.Status)
			assert.Equal(t, wantRefunds, refunds)
			if tc.mode != "invalid dimensions" && tc.mode != "safety" && tc.mode != "unknown" {
				stateKeys := []string{ImageRedisKey("breaker", "async", strconv.Itoa(primaryID)) + ":count", ImageAccountCooldownKey(ImageIdentityHash("image-account", strconv.Itoa(primaryID), "upstream-key-a"))}
				states, stateErr := common.RDB.Exists(t.Context(), stateKeys...).Result()
				require.NoError(t, stateErr)
				assert.Zero(t, states, "dimension incompatibility cannot penalize unrelated requests through global circuit or cooldown state")
			}
		})
	}
}

func TestAsyncImageAccountCooldownAndSupplierBudget(t *testing.T) {
	db, task, request, cfg, alternate := asyncImageWorkerFixture(t)
	ctx := t.Context()
	cfg.AccountCooldownSeconds, cfg.SupplierRetryCapacity, cfg.SupplierRetryWindowSeconds = 60, 1, 60
	cfg.CircuitBreaker = false // Account cooldown is independently configurable.
	require.NoError(t, SaveImageRuntimeConfig(ctx, cfg))
	var primary model.Channel
	require.NoError(t, db.First(&primary, task.ChannelId).Error)
	fingerprint := ImageIdentityHash("image-account", strconv.Itoa(primary.Id), primary.Key)
	require.NoError(t, RecordImageAccountCooldown(ctx, fingerprint, &AsyncImageFailure{Code: 603, HTTPStatus: 429, RetryAfter: 120 * time.Second}, cfg))
	require.NoError(t, RecordImageAccountCooldown(ctx, fingerprint, &AsyncImageFailure{Code: 606}, cfg))
	ttl, err := common.RDB.TTL(ctx, ImageAccountCooldownKey(fingerprint)).Result()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, ttl, 119*time.Second, "a later failure cannot shorten Retry-After")
	policy := model.ImageGroupPolicy{Group: task.Group, Platform: request.Platform, PoolMode: "resolution"}
	selected, _, err := PickImageChannel(ctx, policy, request, ImageAccountRouting{})
	require.NoError(t, err)
	assert.Equal(t, alternate.Id, selected.Id)
	otherFingerprint := ImageIdentityHash("image-account", strconv.Itoa(alternate.Id), alternate.Key)
	require.NoError(t, RecordImageAccountCooldown(ctx, otherFingerprint, &AsyncImageFailure{Code: 606}, cfg))
	selected, _, err = PickImageChannel(ctx, policy, request, ImageAccountRouting{})
	require.NoError(t, err)
	assert.Equal(t, alternate.Id, selected.Id, "when all accounts cool down, use the earliest available credential without waiting")
	// Distinct credentials on the same provider origin share a retry bucket.
	assert.Equal(t, ImageSupplierRetryBudgetKey(primary, request.Model), ImageSupplierRetryBudgetKey(alternate, request.Model))
	wait, err := AcquireImageSupplierRetry(ctx, primary, request.Model, cfg)
	require.NoError(t, err)
	assert.Zero(t, wait)
	wait, err = AcquireImageSupplierRetry(ctx, alternate, request.Model, cfg)
	require.NoError(t, err)
	assert.InDelta(t, 60, wait, 1)
	require.NoError(t, ReturnImageSupplierRetry(ctx, primary, request.Model, cfg))
	wait, err = AcquireImageSupplierRetry(ctx, alternate, request.Model, cfg)
	require.NoError(t, err)
	assert.Zero(t, wait, "undispatched preparation returns its retry permit")
	base := "https://other-supplier.example/custom?token=private"
	alternate.BaseURL = &base
	assert.NotEqual(t, ImageSupplierRetryBudgetKey(primary, request.Model), ImageSupplierRetryBudgetKey(alternate, request.Model))
	assert.NotContains(t, ImageSupplierRetryBudgetKey(alternate, request.Model), "private")
}

func TestAsyncImageAllAccountCooldownRetainsRoutingControls(t *testing.T) {
	for _, condition := range []string{"disabled channel", "disabled key", "open circuit", "full channel", "timed-out credential"} {
		t.Run(condition, func(t *testing.T) {
			db, task, request, cfg, alternate := asyncImageWorkerFixture(t)
			cfg.AccountCooldownSeconds = 60
			require.NoError(t, SaveImageRuntimeConfig(t.Context(), cfg))
			primaryFingerprint := ImageIdentityHash("image-account", strconv.Itoa(task.ChannelId), "upstream-key-a")
			alternateFingerprint := ImageIdentityHash("image-account", strconv.Itoa(alternate.Id), alternate.Key)
			require.NoError(t, common.RDB.Set(t.Context(), ImageAccountCooldownKey(primaryFingerprint), "cooling", 10*time.Second).Err())
			require.NoError(t, common.RDB.Set(t.Context(), ImageAccountCooldownKey(alternateFingerprint), "cooling", time.Minute).Err())
			routing := ImageAccountRouting{}
			switch condition {
			case "disabled channel":
				require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", task.ChannelId).Update("status", common.ChannelStatusManuallyDisabled).Error)
			case "disabled key":
				info := model.ChannelInfo{IsMultiKey: true, MultiKeyStatusList: map[int]int{0: common.ChannelStatusManuallyDisabled}}
				require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", task.ChannelId).Update("channel_info", info).Error)
			case "open circuit":
				for range cfg.FailureThreshold {
					require.NoError(t, RecordImageCircuit(t.Context(), "async", task.ChannelId, false, cfg))
				}
			case "full channel":
				for i := range cfg.ImageConcurrency {
					require.NoError(t, common.RDB.ZAdd(t.Context(), ImageChannelConcurrencyKey(task.ChannelId), &redis.Z{Score: float64(time.Now().Unix() + 60), Member: strconv.Itoa(i)}).Err())
				}
			case "timed-out credential":
				routing = ImageAccountAttemptRouting([]ImageChannelAttempt{{ChannelId: task.ChannelId, KeyFingerprint: primaryFingerprint, CredentialFingerprint: ImageIdentityHash("image-credential", "upstream-key-a"), Dispatched: true, FinishedAt: time.Now().Unix(), TimedOut: true}}, cfg.ReferenceRetries, "gemini", 0)
				assert.Empty(t, routing.Pin, "an exhausted Gemini switch budget must never pin a timed-out account")
			}
			policy := model.ImageGroupPolicy{Group: task.Group, Platform: request.Platform, PoolMode: "resolution"}
			selected, _, err := PickImageChannel(t.Context(), policy, request, routing)
			require.NoError(t, err)
			assert.Equal(t, alternate.Id, selected.Id, "the earliest cooldown is not permission to bypass other routing controls")
		})
	}
}

func TestAsyncImageWorkerTimeoutPreservesExecutionBarriers(t *testing.T) {
	for _, condition := range []string{"accepted provider job", "parent cancellation", "unconfirmed response"} {
		t.Run(condition, func(t *testing.T) {
			db, task, _, cfg, _ := asyncImageWorkerFixture(t)
			asyncImageWorkerQuotaCacheFixture(t, task)
			require.NoError(t, model.ReserveAsyncImageQuota(t.Context(), task, 100, model.ImageFundingSelection{Source: "wallet"}))
			if condition == "accepted provider job" {
				task.UpstreamTaskId, task.DispatchedAt = "existing-provider-job", time.Now().Unix()-1
				require.NoError(t, db.Model(&task).Updates(map[string]any{"upstream_task_id": task.UpstreamTaskId, "dispatched_at": task.DispatchedAt}).Error)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			ExecuteAsyncImageFunc = func(_ context.Context, current model.AsyncImageTask, _ AsyncImageRequest, channel *model.Channel, _ ImageRuntimeConfig, beforeInvoke func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				calls++
				assert.Equal(t, "upstream-key-a", channel.Key)
				if condition == "accepted provider job" {
					assert.Equal(t, "existing-provider-job", current.UpstreamTaskId)
					assert.ErrorIs(t, beforeInvoke(), model.ErrImageConflict)
					return nil, model.AsyncImageBill{}, AsyncImageTransportFailure(context.DeadlineExceeded, current, false, nil)
				}
				require.NoError(t, beforeInvoke())
				if condition == "parent cancellation" {
					cancel()
					return nil, model.AsyncImageBill{}, &AsyncImageFailure{Code: 608, InternalCode: "upstream_transport_timeout", ExecutionUnknown: true}
				}
				return nil, model.AsyncImageBill{}, &AsyncImageFailure{Code: 608, InternalCode: "execution_unknown", ExecutionUnknown: true}
			}
			require.NoError(t, invokeAsyncImageTask(ctx, &task, cfg))
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			assert.Equal(t, 1, calls)
			assert.Zero(t, task.RetryCount)
			if condition == "accepted provider job" {
				assert.Equal(t, model.ImageTaskInvoking, task.Status)
				assert.Equal(t, "existing-provider-job", task.UpstreamTaskId)
				assert.Greater(t, task.NextAttemptAt, time.Now().Unix())
				cooling, err := common.RDB.Exists(t.Context(), ImageAccountCooldownKey(ImageIdentityHash("image-account", strconv.Itoa(task.ChannelId), "upstream-key-a"))).Result()
				require.NoError(t, err)
				assert.Zero(t, cooling, "poll timeouts must not cool down generation credentials")
			} else {
				assert.Equal(t, model.ImageTaskExecutionUnknown, task.Status)
				assert.Zero(t, task.NextAttemptAt)
				var attempts []ImageChannelAttempt
				require.NoError(t, common.UnmarshalJsonStr(task.Attempts, &attempts))
				require.Len(t, attempts, 1)
				assert.False(t, attempts[0].TimedOut)
			}
			var bill model.AsyncImageBill
			var user model.User
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
			require.NoError(t, db.First(&user, task.UserId).Error)
			assert.Equal(t, "reserved", bill.Status)
			assert.Equal(t, 900, user.Quota)
		})
	}
}

func TestAsyncImageTimeoutPolicyRecovery(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		policy, deadline, job, output bool
		wantStatus                    string
	}{
		{name: "new timeout policy expires and refunds", policy: true, deadline: true, wantStatus: model.ImageTaskFailed},
		{name: "legacy timeout remains unknown", deadline: true, wantStatus: model.ImageTaskExecutionUnknown},
		{name: "lost lease stays unknown", policy: true, wantStatus: model.ImageTaskExecutionUnknown},
		{name: "known job keeps polling", policy: true, job: true, wantStatus: model.ImageTaskInvoking},
		{name: "known job deadline remains unknown", policy: true, job: true, deadline: true, wantStatus: model.ImageTaskExecutionUnknown},
		{name: "saved completion keeps its hold", policy: true, output: true, deadline: true, wantStatus: model.ImageTaskInvoking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, task, _, cfg, _ := asyncImageWorkerFixture(t)
			asyncImageWorkerQuotaCacheFixture(t, task)
			require.NoError(t, model.ReserveAsyncImageQuota(t.Context(), task, 100, model.ImageFundingSelection{Source: "wallet"}))
			now := time.Now().Unix()
			updates := map[string]any{"dispatched_at": now - 5, "lease_expires_at": now - 1}
			if tc.deadline {
				updates["started_at"] = now - int64(cfg.ExecutionTimeout) - 1
			}
			if tc.job {
				updates["upstream_task_id"] = "original-provider-job"
			}
			if tc.output {
				updates["upstream_succeeded_at"] = now - 1
			}
			attempts, err := common.Marshal([]ImageChannelAttempt{{ChannelId: task.ChannelId, Dispatched: true, RetryOnTimeout: tc.policy}})
			require.NoError(t, err)
			updates["attempts"] = string(attempts)
			require.NoError(t, db.Model(&task).Updates(updates).Error)
			for range 2 {
				require.NoError(t, model.RecoverAsyncImageTasks(t.Context(), 100, cfg.ExecutionTimeout, cfg.FailedRequestRetentionHours))
			}
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			assert.Equal(t, tc.wantStatus, task.Status)
			var bill model.AsyncImageBill
			var user model.User
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
			require.NoError(t, db.First(&user, task.UserId).Error)
			if tc.wantStatus == model.ImageTaskFailed {
				assert.Equal(t, "timeout_deadline_exhausted", task.ErrorCode)
				assert.Equal(t, "refunded", bill.Status)
				assert.Equal(t, 1000, user.Quota)
				var count int64
				require.NoError(t, db.Model(&model.AsyncImageEvent{}).Where("task_id = ? AND event_type = ?", task.TaskId, "reservation_refunded").Count(&count).Error)
				assert.EqualValues(t, 1, count)
			} else {
				assert.Equal(t, "reserved", bill.Status)
				assert.Equal(t, 900, user.Quota)
			}
		})
	}
}

func TestAsyncImageRetryDecisionsAndEncryptedFailureRetention(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		failure                AsyncImageFailure
		retries                int
		wantAction, wantReason string
	}{
		{"temporary rejection", AsyncImageFailure{Code: 606, InternalCode: "upstream_failed"}, 16, "retry_submission", "temporary_failure"},
		{"invalid prompt", AsyncImageFailure{Code: 604, InternalCode: "upstream_failed"}, 16, "stop", "invalid_input"},
		{"safety policy", AsyncImageFailure{Code: 601, InternalCode: "upstream_failed"}, 16, "stop", "content_policy"},
		{"budget exhausted", AsyncImageFailure{Code: 606, InternalCode: "upstream_failed"}, 0, "stop", "total_retry_budget_exhausted"},
		{"unknown dispatch", AsyncImageFailure{Code: 608, InternalCode: "execution_unknown", ExecutionUnknown: true}, 16, "reconcile_unknown", "execution_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, task, _, cfg, _ := asyncImageWorkerFixture(t)
			cfg.TotalRetries = tc.retries
			require.NoError(t, failAsyncImageInvocation(t.Context(), task, &tc.failure, cfg))
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			var decision ImageRetryDecision
			require.NoError(t, common.UnmarshalJsonStr(task.RetryDecision, &decision))
			assert.Equal(t, tc.wantAction, decision.Action)
			assert.Equal(t, tc.wantReason, decision.Reason)
			assert.NotEmpty(t, task.RequestCipher)
			_, err := DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
			require.NoError(t, err)
			if task.Terminal() {
				assert.True(t, task.FailedRequestRetainedAt(time.Now().Unix()))
				assert.InDelta(t, time.Now().Unix()+24*3600, task.RequestRetainUntil, 1)
			} else {
				assert.Zero(t, task.RequestRetainUntil)
			}
		})
	}
	t.Run("disabled retention and cooldown deadline", func(t *testing.T) {
		db, task, _, cfg, _ := asyncImageWorkerFixture(t)
		cfg.FailedRequestRetentionHours = 0
		task.StartedAt = time.Now().Unix() - int64(cfg.ExecutionTimeout)
		require.NoError(t, db.Model(&task).Update("started_at", task.StartedAt).Error)
		require.NoError(t, DeferAsyncImageSubmission(t.Context(), task, cfg, "account_cooldown", 60, 0, 0))
		require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
		assert.Equal(t, model.ImageTaskExpired, task.Status)
		assert.Empty(t, task.RequestCipher)
		assert.Zero(t, task.NextAttemptAt)
	})
}

func TestAsyncImageSupplierWaitPreservesHoldAndBypassesAcceptedJobPolling(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry waits without consuming attempts", true: "accepted job bypasses retry controls"}[accepted], func(t *testing.T) {
			db, task, request, cfg, alternate := asyncImageWorkerFixture(t)
			cfg.SupplierRetryCapacity, cfg.SupplierRetryWindowSeconds = 1, 60
			require.NoError(t, SaveImageRuntimeConfig(t.Context(), cfg))
			var primary model.Channel
			require.NoError(t, db.First(&primary, task.ChannelId).Error)
			fingerprint := ImageIdentityHash("image-account", strconv.Itoa(primary.Id), primary.Key)
			history := []ImageChannelAttempt{{AuditVersion: 1, ChannelId: primary.Id, KeyFingerprint: fingerprint, Dispatched: true, Code: 606, FinishedAt: time.Now().Unix()}}
			if accepted {
				history[0].FinishedAt = 0
				task.UpstreamTaskId, task.DispatchedAt = "accepted-provider-job", time.Now().Unix()
				require.NoError(t, RecordImageAccountCooldown(t.Context(), fingerprint, &AsyncImageFailure{Code: 605}, cfg))
			}
			task.RetryCount = 1
			encoded, err := common.Marshal(history)
			require.NoError(t, err)
			task.Attempts = string(encoded)
			require.NoError(t, db.Model(&task).Updates(map[string]any{"attempts": task.Attempts, "retry_count": 1, "upstream_task_id": task.UpstreamTaskId, "dispatched_at": task.DispatchedAt}).Error)
			require.NoError(t, db.Model(&model.User{}).Where("id = ?", task.UserId).Update("quota", 900).Error)
			require.NoError(t, db.Model(&model.Token{}).Where("id = ?", task.TokenId).Updates(map[string]any{"remain_quota": 900, "used_quota": 100}).Error)
			require.NoError(t, db.Create(&model.AsyncImageBill{TaskId: task.TaskId, UserId: task.UserId, TokenId: task.TokenId, ReservedAt: time.Now().Unix(), ReservedQuota: 100, ReservedTokenQuota: 100, FundingSource: "wallet", Status: "reserved"}).Error)
			wait, err := AcquireImageSupplierRetry(t.Context(), alternate, request.Model, cfg)
			require.NoError(t, err)
			require.Zero(t, wait)
			calls := 0
			ExecuteAsyncImageFunc = func(_ context.Context, current model.AsyncImageTask, _ AsyncImageRequest, selected *model.Channel, _ ImageRuntimeConfig, beforeInvoke func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
				calls++
				assert.True(t, accepted)
				assert.Equal(t, primary.Id, selected.Id)
				assert.Equal(t, "accepted-provider-job", current.UpstreamTaskId)
				assert.ErrorIs(t, beforeInvoke(), model.ErrImageConflict)
				return nil, model.AsyncImageBill{}, &AsyncImagePending{TaskId: current.UpstreamTaskId}
			}
			require.NoError(t, invokeAsyncImageTask(t.Context(), &task, cfg))
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			assert.Equal(t, 1, task.RetryCount)
			assert.Zero(t, task.CapacityRetryCount)
			require.NoError(t, common.UnmarshalJsonStr(task.Attempts, &history))
			assert.Len(t, history, 1)
			var decision ImageRetryDecision
			require.NoError(t, common.UnmarshalJsonStr(task.RetryDecision, &decision))
			if accepted {
				assert.Equal(t, 1, calls)
				assert.Equal(t, "poll_same_job", decision.Action)
			} else {
				assert.Zero(t, calls)
				assert.Equal(t, model.ImageTaskQueued, task.Status)
				assert.Equal(t, "wait_supplier_budget", decision.Action)
			}
			var user model.User
			var bill model.AsyncImageBill
			require.NoError(t, db.First(&user, task.UserId).Error)
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&bill).Error)
			assert.Equal(t, 900, user.Quota)
			assert.Equal(t, "reserved", bill.Status)
			assert.NotEmpty(t, task.RequestCipher)
		})
	}
}

func TestAsyncImageChannelReferenceTransportOverrides(t *testing.T) {
	cfg := DefaultImageRuntimeConfig()
	for _, tc := range []struct {
		setting string
		count   int
		want    string
	}{{"", 0, cfg.OpenAIReferenceMode}, {"local", 0, "local"}, {"passthrough", 1, "passthrough"}, {"passthrough_fallback_local", 0, "passthrough_fallback_local"}, {"passthrough_fallback_local", 1, "local"}} {
		channel := model.Channel{}
		channel.SetSetting(dto.ChannelSettings{ImageReferenceTransportMode: tc.setting})
		require.NoError(t, channel.ValidateSettings())
		assert.Equal(t, tc.want, ImageChannelReferenceTransport(channel, "openai", tc.count, cfg))
	}
	channel := model.Channel{}
	channel.SetSetting(dto.ChannelSettings{ImageReferenceTransportMode: "invalid"})
	assert.Error(t, channel.ValidateSettings())
}

func TestAsyncImageReliabilityResolvedSamples(t *testing.T) {
	a := ImageChannelAttempt{AuditVersion: 1, ChannelId: 1, KeyFingerprint: "a", Dispatched: true, TransportUsed: "remote", ReferenceFailure: true}
	b := ImageChannelAttempt{AuditVersion: 1, ChannelId: 2, KeyFingerprint: "b", Dispatched: true, TransportUsed: "local", ReferenceFallback: true}
	var stats ImageReliabilityStats
	stats.Observe(model.AsyncImageTask{Status: model.ImageTaskSucceeded}, []ImageChannelAttempt{a})
	stats.Observe(model.AsyncImageTask{Status: model.ImageTaskSucceeded}, []ImageChannelAttempt{a, b})
	stats.Observe(model.AsyncImageTask{Status: model.ImageTaskFailed}, []ImageChannelAttempt{a, b})
	stats.Observe(model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown}, []ImageChannelAttempt{a, b})
	stats.Observe(model.AsyncImageTask{Status: model.ImageTaskQueued}, []ImageChannelAttempt{a, b})
	stats.Observe(model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "admin_terminated"}, []ImageChannelAttempt{a, b})
	stats.Observe(model.AsyncImageTask{Status: model.ImageTaskSucceeded}, []ImageChannelAttempt{{Dispatched: true, ReferenceMode: "local"}})
	assert.Equal(t, int64(7), stats.Total)
	assert.Equal(t, int64(5), stats.Audited)
	assert.Equal(t, int64(3), stats.Final.Succeeded)
	assert.Equal(t, int64(4), stats.Final.Resolved)
	require.NotNil(t, stats.Final.Percent)
	assert.Equal(t, 75.0, *stats.Final.Percent)
	assert.Equal(t, int64(1), stats.FirstSubmission.Succeeded)
	assert.Equal(t, int64(3), stats.FirstSubmission.Resolved)
	assert.Equal(t, int64(2), stats.Switch.Resolved)
	assert.Equal(t, int64(1), stats.Switch.Succeeded)
	require.NotNil(t, stats.LocalFallback.Percent)
	assert.Equal(t, 50.0, *stats.LocalFallback.Percent)
	assert.Equal(t, int64(1), stats.Unknown)
	assert.Equal(t, int64(1), stats.Pending)
	assert.Equal(t, int64(1), stats.Terminated)
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
	transientA := ImageChannelAttempt{ChannelId: 10, KeyFingerprint: "account-a", Dispatched: true, FinishedAt: 1, Code: 606}
	rateLimitedB := ImageChannelAttempt{ChannelId: 20, KeyFingerprint: "account-b", Dispatched: true, FinishedAt: 1, Code: 605}
	incompatibleA := ImageChannelAttempt{ChannelId: 10, KeyFingerprint: "account-a", CredentialFingerprint: "credential-a", Dispatched: true, FinishedAt: 1, Code: 610, HTTPStatus: http.StatusUnprocessableEntity, ProviderCode: "image_size_not_supported"}
	for _, tc := range []struct {
		name        string
		history     []ImageChannelAttempt
		platform    string
		maxSwitches int
		want        ImageAccountRouting
	}{
		{name: "completed transient failure prefers another account", history: []ImageChannelAttempt{transientA}, platform: "openai", want: ImageAccountRouting{Avoid: "account-a"}},
		{name: "only the latest transient account is avoided", history: []ImageChannelAttempt{transientA, rateLimitedB}, platform: "openai", want: ImageAccountRouting{Avoid: "account-b"}},
		{name: "Gemini switch budget retains same-account retries", history: []ImageChannelAttempt{transientA, rateLimitedB}, platform: "gemini", maxSwitches: 1, want: ImageAccountRouting{Pin: "account-b"}},
		{name: "Gemini zero switches permits bounded retries", history: []ImageChannelAttempt{transientA}, platform: "gemini", want: ImageAccountRouting{Pin: "account-a"}},
		{name: "Gemini permits a remaining switch", history: []ImageChannelAttempt{transientA}, platform: "gemini", maxSwitches: 1, want: ImageAccountRouting{Avoid: "account-a"}},
		{name: "undispatched errors retain account availability", history: []ImageChannelAttempt{{ChannelId: 10, KeyFingerprint: "account-a", FinishedAt: 1, Code: 606}}, platform: "openai"},
		{name: "incomplete dispatch never authorizes switching", history: []ImageChannelAttempt{{ChannelId: 10, KeyFingerprint: "account-a", Dispatched: true, Code: 606}}, platform: "openai"},
		{name: "legacy transient failure prefers another channel", history: []ImageChannelAttempt{{ChannelId: 10, Dispatched: true, FinishedAt: 1, Code: 606}}, platform: "openai", want: ImageAccountRouting{AvoidChannel: 10}},
		{name: "transient failure preserves a reference pin", history: []ImageChannelAttempt{{ChannelId: 10, KeyFingerprint: "account-a", Dispatched: true, ReferenceFailure: true, Code: 602}, transientA}, platform: "openai", want: ImageAccountRouting{Pin: "account-a", Avoid: "account-a"}},
		{name: "size rejection permanently excludes this task's credential", history: []ImageChannelAttempt{incompatibleA}, platform: "openai", want: ImageAccountRouting{IncompatibleAccounts: []string{"account-a"}, IncompatibleCredentials: []string{"credential-a"}}},
		{name: "Gemini zero switches stops size failover", history: []ImageChannelAttempt{incompatibleA}, platform: "gemini", want: ImageAccountRouting{IncompatibleAccounts: []string{"account-a"}, IncompatibleCredentials: []string{"credential-a"}, Stop: true}},
		{name: "legacy size rejection excludes its channel", history: []ImageChannelAttempt{{ChannelId: 10, Dispatched: true, FinishedAt: 1, HTTPStatus: http.StatusUnprocessableEntity, ProviderCode: "image_size_not_supported"}}, platform: "openai", want: ImageAccountRouting{IncompatibleChannels: []int{10}}},
		{name: "accepted jobs cannot authorize size failover", history: []ImageChannelAttempt{{ChannelId: 10, KeyFingerprint: "account-a", Dispatched: true, FinishedAt: 1, HTTPStatus: http.StatusUnprocessableEntity, ProviderCode: "image_size_not_supported", UpstreamTaskID: "accepted-job"}}, platform: "openai"},
		{name: "unfinished rejection cannot exclude an account", history: []ImageChannelAttempt{{ChannelId: 10, KeyFingerprint: "account-a", Dispatched: true, HTTPStatus: http.StatusUnprocessableEntity, ProviderCode: "image_size_not_supported"}}, platform: "openai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ImageAccountAttemptRouting(tc.history, 2, tc.platform, tc.maxSwitches))
		})
	}
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
	cfg := DefaultImageRuntimeConfig()
	cfg.AccountCooldownSeconds = 0 // Reference capacity routing is tested independently.
	require.NoError(t, SaveImageRuntimeConfig(t.Context(), cfg))
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
		httpStatus   int
		internalCode string
	}{
		{name: "size compatibility rejection", body: `{"error":{"code":"image_size_not_supported","message":"No eligible upstream image provider supports the requested image size or resolution tier. Adjust size/resolution or select a compatible route."}}`, expectedCode: 603, providerCode: "image_size_not_supported", message: "resolution tier", httpStatus: http.StatusUnprocessableEntity, internalCode: "upstream_image_route_incompatible"},
		{name: "compatibility rejection without provider code", body: `{"error":{"message":"No eligible upstream image provider supports the requested image size or resolution tier. Adjust size/resolution or select a compatible route."}}`, expectedCode: 603, message: "resolution tier", internalCode: "upstream_image_route_incompatible"},
		{name: "ordinary dimension rejection stays terminal", body: `{"error":{"code":"invalid_request","message":"unsupported_image_dimensions: dimensions must be even"}}`, expectedCode: 604, providerCode: "invalid_request", message: "dimensions must be even", httpStatus: http.StatusUnprocessableEntity},
		{name: "compatibility wording cannot override safety", body: `{"error":{"code":"image_size_not_supported","message":"No eligible upstream image provider supports the requested image size or resolution tier; safety review did not pass"}}`, expectedCode: 601, providerCode: "image_size_not_supported", message: "safety review", httpStatus: http.StatusUnprocessableEntity},
		{name: "provider safety status precedes compatibility code", body: `{"error":{"code":"image_size_not_supported","message":"No eligible upstream image provider supports the requested image size or resolution tier.","status":"SAFETY"}}`, expectedCode: 601, providerCode: "image_size_not_supported", message: "resolution tier", httpStatus: http.StatusUnprocessableEntity},
		{name: "authorization is not compatibility rejection", body: `{"error":{"code":"image_size_not_supported","message":"No eligible upstream image provider supports the requested image size or resolution tier."}}`, expectedCode: 610, providerCode: "image_size_not_supported", message: "resolution tier", httpStatus: http.StatusForbidden},
		{name: "reference fetch timeout", body: `{"error":{"code":400,"message":"reference image fetch: context deadline exceeded"}}`, expectedCode: 602, providerCode: "400", message: "reference image fetch", fallback: true},
		{name: "reference fetch without Google envelope fields", body: `{"error":{"message":"reference image fetch: Get https://signed.example/image?token=private: i/o timeout"}}`, expectedCode: 602, message: "reference image fetch", fallback: true},
		{name: "reference download proxy failure", body: `{"error":{"code":400,"message":"Proxy error: Reference image download failed; direct=timeout; proxy=502"}}`, expectedCode: 602, providerCode: "400", message: "Reference image download failed", fallback: true},
		{name: "domestic reference relay failure", body: `{"error":{"code":400,"message":"参考图 #1 国内下载中转失败"}}`, expectedCode: 602, providerCode: "400", message: "国内下载中转失败", fallback: true},
		{name: "provider prompt length limit", body: `{"error":{"code":400,"message":"POST /: Your prompt cannot exceed 10000 characters.","status":"INVALID_ARGUMENT"}}`, expectedCode: 604, providerCode: "400", message: "10000 characters"},
		{name: "known service timeout", body: `{"error":{"code":400,"message":"service timeout, please try again later."}}`, expectedCode: 606, providerCode: "400", message: "service timeout"},
		{name: "known generation timeout", body: `{"error":{"code":400,"message":"生成超时，请重试"}}`, expectedCode: 606, providerCode: "400", message: "生成超时"},
		{name: "unknown timeout is not a rejection contract", body: `{"error":{"code":400,"message":"operation timeout"}}`, expectedCode: 610, providerCode: "400", message: "operation timeout"},
		{name: "authorization rejection remains terminal", body: `{"error":{"code":403,"message":"service timeout, please try again later."}}`, expectedCode: 610, providerCode: "403", message: "service timeout", httpStatus: http.StatusForbidden},
		{name: "safety rejection precedes reference transport", body: `{"error":{"code":400,"message":"reference image fetch failed: 图片未通过安全审核"}}`, expectedCode: 601, providerCode: "400", message: "安全审核"},
		{name: "byte limit precedes reference transport", body: `{"error":{"code":400,"message":"reference image fetch failed: image_url exceeds 50MB limit"}}`, expectedCode: 604, providerCode: "400", message: "50MB limit"},
		{name: "missing input precedes reference transport", body: `{"error":{"code":400,"message":"reference image fetch failed: missing reference image"}}`, expectedCode: 612, providerCode: "400", message: "missing reference image"},
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
			status := testCase.httpStatus
			if status == 0 {
				status = http.StatusBadRequest
			}
			response := &http.Response{
				StatusCode: status,
				Header:     testCase.header,
				Body:       io.NopCloser(strings.NewReader(testCase.body)),
			}

			failure := ClassifyGeminiAsyncImageHTTPFailure(t.Context(), response)

			require.NotNil(t, failure)
			assert.Equal(t, testCase.expectedCode, failure.Code)
			if testCase.internalCode != "" {
				assert.Equal(t, testCase.internalCode, failure.InternalCode)
			}
			assert.Equal(t, status, failure.HTTPStatus)
			assert.Equal(t, testCase.providerCode, failure.ProviderCode)
			assert.Equal(t, testCase.requestID, failure.UpstreamRequestID)
			assert.Contains(t, failure.Message, testCase.message)
			assert.NotContains(t, failure.Message, "private-key")
			assert.NotContains(t, failure.Message, "signed.example")
			assert.LessOrEqual(t, len([]rune(failure.Message)), asyncImageProviderMessageLimit)
			assert.Equal(t, testCase.fallback, ShouldFallbackGeminiReferenceTransport(failure))
			assert.False(t, failure.UpstreamTerminal, "an HTTP error alone does not prove a persisted upstream job is terminal")
		})
	}
	for _, tc := range []struct {
		name       string
		status     int
		mapping    string
		wantCode   int
		wantCompat bool
	}{
		{name: "actual rejection despite server error mapping", status: 422, mapping: `{"422":"500"}`, wantCode: 603, wantCompat: true},
		{name: "mapped server error is not a compatibility rejection", status: 500, mapping: `{"500":"422"}`, wantCode: 610},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"image_size_not_supported","message":"This route does not support the requested size"}}`))}
			failure := ClassifyOpenAIAsyncImageHTTPFailure(t.Context(), response, tc.mapping)
			assert.Equal(t, tc.wantCode, failure.Code)
			assert.Equal(t, tc.status, failure.HTTPStatus)
			assert.Equal(t, tc.wantCompat, failure.InternalCode == "upstream_image_route_incompatible")
		})
	}
	assert.Equal(t, 603, ClassifyAsyncImageFailure(errors.New("no eligible image account in the selected pool"), 0).Code)
	assert.Equal(t, 606, ClassifyAsyncImageFailure(errors.New("生成超时，请重试"), 0).Code)
	assert.Equal(t, 606, ClassifyAsyncImageFailure(errors.Join(errors.New("prepare image request"), context.DeadlineExceeded), 0).Code)
	assert.Equal(t, 606, ClassifyAsyncImageFailure(&url.Error{Op: "Get", URL: "https://signed.example/image", Err: os.ErrDeadlineExceeded}, 0).Code)
	assert.Equal(t, 610, ClassifyAsyncImageFailure(errors.New("operation timeout"), 0).Code, "untyped wording does not prove a transport failure")
	assert.False(t, ShouldFallbackGeminiReferenceTransport(&AsyncImageFailure{Code: 602, HTTPStatus: http.StatusBadRequest, ExecutionUnknown: true}))
	terminal := &AsyncImageFailure{Code: 606, UpstreamTerminal: true, Message: "confirmed generation failure"}
	assert.Same(t, terminal, ClassifyAsyncImageFailure(terminal, 0), "classified domain failures retain the provider's terminal evidence")
}

func TestGeminiAsyncImageReferenceFailureSchedulesSingleLocalFallback(t *testing.T) {
	for _, code := range []int{601, 602} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			previousDB, previousType := model.DB, common.MainDatabaseType()
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
			common.SetMainDatabaseType(common.DatabaseType(db.Dialector.Name()))
			t.Cleanup(func() {
				model.DB = previousDB
				common.SetMainDatabaseType(previousType)
				assert.NoError(t, db.Migrator().DropTable(&model.AsyncImageStagingObject{}, &model.ImageOutbox{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}, &model.AsyncImageTask{}))
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
				TaskId:         "asyncimg_gemini_reference_fallback_" + strconv.Itoa(code),
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
				Code:           code,
				InternalCode:   "upstream_failed",
				Message:        "The request was rejected by the upstream safety policy.",
				HTTPStatus:     http.StatusBadRequest,
				ProviderCode:   "400",
				ProviderStatus: "INVALID_ARGUMENT",
			}
			if code == 602 {
				failure.ProviderStatus = ""
				failure.Message = "reference image fetch: i/o timeout"
			}
			failure.ReferenceTransportFallback = ShouldFallbackGeminiReferenceTransport(failure)
			require.True(t, failure.ReferenceTransportFallback)

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
			assert.Equal(t, code, storedAttempts[0].Code)
			assert.Equal(t, "passthrough_fallback_local", storedAttempts[0].ReferenceMode)
			routing := ImageAccountAttemptRouting(storedAttempts, cfg.ReferenceRetries, "gemini", cfg.GeminiAccountSwitches)
			assert.False(t, routing.Stop)
			assert.Equal(t, 15, routing.PinChannel, "try local reference transport on the original account before rotating")

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
		})
	}
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
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}, &model.AsyncImageBill{}))

	now := time.Now().Unix()
	attempts, err := common.Marshal([]ImageChannelAttempt{{ChannelId: 17, StartedAt: now - 1, Dispatched: true}})
	require.NoError(t, err)
	task := model.AsyncImageTask{
		TaskId:         "asyncimg_provider_diagnostics",
		Status:         model.ImageTaskInvoking,
		Version:        1,
		ChannelId:      17,
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
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}, &model.AsyncImageBill{}))

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

func TestAsyncImageUnifiedCountAndGeminiDimensions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		count       int
		provider    string
		resolution  string
		aspectRatio string
		references  int
		routeGemini bool
		wantError   bool
	}{
		{name: "provider count is billed and sent", body: `{"model":"image-model","prompt":"draw","create_count":2}`, count: 2},
		{name: "xai official single reference", body: `{"model":"grok-imagine-image-quality","prompt":"edit","n":1,"image":{"url":"https://example.com/input.png","type":"image_url"}}`, count: 1, references: 1},
		{name: "xai official multiple references", body: `{"model":"grok-imagine-image-quality","prompt":"edit","n":1,"images":[{"url":"https://example.com/first.png","type":"image_url"},{"url":"https://example.com/second.png"}]}`, count: 1, references: 2},
		{name: "empty image object rejected", body: `{"model":"grok-imagine-image-quality","prompt":"edit","n":1,"image":{}}`, wantError: true},
		{name: "official reference cannot bypass file ID rejection", body: `{"model":"grok-imagine-image-quality","prompt":"edit","n":1,"images":[{"url":"https://example.com/input.png","file_id":"private-file"}]}`, wantError: true},
		{name: "zero provider count", body: `{"model":"image-model","prompt":"draw","create_count":0}`, wantError: true},
		{name: "negative provider count", body: `{"model":"image-model","prompt":"draw","create_count":-1}`, wantError: true},
		{name: "provider count exceeds maximum", body: `{"model":"image-model","prompt":"draw","create_count":` + strconv.Itoa(dto.MaxImageN+1) + `}`, wantError: true},
		{name: "provider count overflows uint", body: `{"model":"image-model","prompt":"draw","create_count":18446744073709551616}`, wantError: true},
		{name: "provider count exceeds billing bound", body: `{"model":"image-model","prompt":"draw","create_count":18446744073709551615}`, wantError: true},
		{name: "explicit n conflicts with provider count", body: `{"model":"image-model","prompt":"draw","n":1,"create_count":2}`, wantError: true},
		{name: "provider count conflicts with batch", body: `{"model":"image-model","prompt":"draw","create_count":1,"batch_size":2}`, wantError: true},
		{name: "batch conflicts with provider count", body: `{"model":"image-model","prompt":"draw","batch_size":2,"create_count":1}`, wantError: true},
		{name: "parameters count is billed and sent", body: `{"model":"image-model","prompt":"draw","parameters":{"n":2}}`, count: 2},
		{name: "explicit count conflicts with parameters", body: `{"model":"image-model","prompt":"draw","n":2,"parameters":{"n":1}}`, wantError: true},
		{name: "gemini half K resolution preserves edit count", body: `{"provider":"gemini","model":"image-model","prompt":"edit","resolution":"0.5K","n":2,"images":[{"image_url":"https://example.com/input.png"}]}`, count: 2, provider: "gemini", resolution: "0.5K", references: 1},
		{name: "gemini half K size preserves edit count", body: `{"provider":"gemini","model":"image-model","prompt":"edit","size":"0.5K","n":2,"image_urls":["https://example.com/input.png"]}`, count: 2, provider: "gemini", resolution: "0.5K", references: 1},
		{name: "gemini ratio alias", body: `{"provider":"gemini","model":"image-model","prompt":"draw","resolution":"0.5K","n":2,"ratio":"4:5"}`, count: 2, provider: "gemini", resolution: "0.5K", aspectRatio: "4:5"},
		{name: "providerless half K resolution routes to gemini", body: `{"model":"image-model","prompt":"edit","resolution":"0.5K","ratio":"4:5","n":2,"images":[{"image_url":"https://example.com/input.png"}]}`, count: 2, resolution: "0.5K", aspectRatio: "4:5", references: 1, routeGemini: true},
		{name: "providerless half K size routes to gemini", body: `{"model":"image-model","prompt":"edit","size":"0.5K","ratio":"16:9","n":2,"image_urls":["https://example.com/input.png"]}`, count: 2, resolution: "0.5K", aspectRatio: "16:9", references: 1, routeGemini: true},
		{name: "openai half K resolution rejected", body: `{"provider":"openai","model":"image-model","prompt":"draw","resolution":"0.5K"}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := ParseAsyncImageRequest([]byte(tc.body), "application/json", "/v1/images/generations_async", DefaultImageRuntimeConfig())
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.count, request.Count)
			assert.Equal(t, strconv.Itoa(tc.count), string(request.Native["n"]), "native upstream count must match reservation count")
			assert.Equal(t, tc.references, request.ReferenceImageCount())
			if tc.resolution != "" {
				assert.Equal(t, tc.resolution, request.Resolution)
				assert.Equal(t, tc.provider, request.Provider)
			}
			if tc.aspectRatio != "" {
				assert.Equal(t, tc.aspectRatio, request.AspectRatio)
			}
			if tc.routeGemini {
				db, task, _, cfg, _ := asyncImageWorkerFixture(t)
				ImageChannelCapabilityFunc = func(model.Channel, string) dto.ImageCapability {
					return dto.ImageCapability{Provider: "gemini", Protocol: "gemini_native", Generate: true, Edit: true}
				}
				catalog, err := common.Marshal([]ImageModelCapability{{Id: "image-model", AllowHalfK: true, MaxOutputImages: 2, MaxReferenceImages: 1}})
				require.NoError(t, err)
				require.NoError(t, db.Create(&model.ImageGroupPolicy{PolicyKey: ImageIdentityHash("default", "gemini"), Group: "default", Platform: "gemini", Enabled: true, AsyncEnabled: true, PoolMode: "resolution", Models: string(catalog)}).Error)
				var token model.Token
				require.NoError(t, db.First(&token, task.TokenId).Error)
				policy, _, err := ValidateAsyncImageEligibility(t.Context(), token, request, cfg)
				require.NoError(t, err)
				assert.Equal(t, "gemini", policy.Platform)
				assert.Equal(t, "gemini", policy.AsyncProvider)
				selected, _, err := PickImageChannel(t.Context(), policy, request, ImageAccountRouting{})
				require.NoError(t, err)
				assert.Equal(t, "gemini", ImageChannelCapability(*selected, request.Model).Provider)
			}
		})
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
