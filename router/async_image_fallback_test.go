package router

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAsyncImageUpstreamFallbackBilling(t *testing.T) {
	for _, tc := range []struct {
		name         string
		expression   string
		quota        int
		requested    int
		usage        bool
		subscription bool
		retry        bool
		lowBalance   bool
		logFailure   bool
		privateURL   bool
		downloadKey  bool
		missingUsage bool
	}{
		{name: "legacy_quantity_after_download_retry", quota: 20000, retry: true},
		{name: "only_actual_results_are_charged", quota: 20000, requested: 3},
		{name: "frozen_resolution_expression", expression: `param("resolution") == "2K" ? tier("2k", fixed(0.02)) * image_count : tier("1k", fixed(0.01)) * image_count`, quota: 40000},
		{name: "actual_tokens_are_not_multiplied_by_images", expression: `tier("tokens", p * 2 + c * 8)`, quota: 180, usage: true},
		{name: "subscription_funding", quota: 20000, subscription: true},
		{name: "explicit_zero_price", expression: `tier("free", fixed(0)) * image_count`},
		{name: "insufficient_balance_then_concurrent_settlement", quota: 20000, lowBalance: true},
		{name: "log_retry_does_not_repeat_debit", quota: 20000, logFailure: true},
		{name: "required_token_usage_is_missing", expression: `tier("tokens", p * 2 + c * 8)`, missingUsage: true},
		{name: "private_result_cannot_fallback", privateURL: true},
		{name: "credentialed_result_cannot_fallback", downloadKey: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousDB, previousLogs, previousRedis := model.DB, model.LOG_DB, common.RDB
			previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
			previousEnabled, previousMemory, previousConsume, previousExport, previousExecutor := common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled, common.DataExportEnabled, service.ExecuteAsyncImageFunc
			previousEstimator := service.EstimateAsyncImageQuotaFunc
			previousResolver, previousQuotaUnit := net.DefaultResolver, common.QuotaPerUnit
			pricing := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
			previousPricing := *pricing
			previousGroups, previousGroupOverrides := ratio_setting.GroupRatio2JSONString(), ratio_setting.GroupGroupRatio2JSONString()
			previousPrices, err := common.Marshal(ratio_setting.GetModelPriceCopy())
			require.NoError(t, err)
			t.Cleanup(func() {
				model.DB, model.LOG_DB, common.RDB = previousDB, previousLogs, previousRedis
				common.SetDatabaseTypes(previousMainType, previousLogType)
				common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled, common.DataExportEnabled, service.ExecuteAsyncImageFunc = previousEnabled, previousMemory, previousConsume, previousExport, previousExecutor
				service.EstimateAsyncImageQuotaFunc = previousEstimator
				net.DefaultResolver, common.QuotaPerUnit = previousResolver, previousQuotaUnit
				*pricing = previousPricing
				assert.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(previousPrices)))
				assert.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousGroups))
				assert.NoError(t, ratio_setting.UpdateGroupGroupRatioByJSONString(previousGroupOverrides))
			})
			model.DB, model.LOG_DB = imageRouterDatabase(t, "fallback-main"), imageRouterDatabase(t, "fallback-logs")
			common.SetDatabaseTypes(common.DatabaseType(model.DB.Dialector.Name()), common.DatabaseType(model.LOG_DB.Dialector.Name()))
			imageLogs := model.LOG_DB
			t.Setenv("LOG_SQL_DSN", "")
			previousMaster := common.IsMasterNode
			common.IsMasterNode = false
			err = model.InitLogDB()
			common.IsMasterNode = previousMaster
			require.NoError(t, err)
			model.LOG_DB = imageLogs
			common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled, common.DataExportEnabled = true, false, true, true
			common.QuotaPerUnit = 500000
			require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"image-fallback-model":0.01}`))
			require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":2}`))
			require.NoError(t, ratio_setting.UpdateGroupGroupRatioByJSONString(`{}`))
			*pricing = billing_setting.BillingSetting{}
			if tc.expression != "" {
				pricing.BillingMode = map[string]string{"image-fallback-model": billing_setting.BillingModeTieredExpr}
				pricing.BillingExpr = map[string]string{"image-fallback-model": tc.expression}
			}
			require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Option{}, &model.SubscriptionPlan{}, &model.UserSubscription{}))
			require.NoError(t, model.MigrateImageModels(model.DB))
			require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
			require.NoError(t, model.MigrateImageLogs(model.LOG_DB))
			var version string
			query := "SELECT VERSION()"
			if model.DB.Dialector.Name() == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			require.NoError(t, model.DB.Raw(query).Scan(&version).Error)
			t.Logf("Database version: %s", version)
			redisServer := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			common.RDB = redisClient
			t.Cleanup(func() { assert.NoError(t, redisClient.Close()) })
			t.Setenv("ASYNC_IMAGE_REDIS_PREFIX", "fallback:"+common.GetUUID())
			t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "fallback:"+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")))
			t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "fallback")
			// Public media DNS fails deterministically; the local upstream uses an
			// IP literal. No external provider or CDN is contacted by this test.
			mediaResolver := &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("test media DNS unavailable")
			}}
			service.InitHttpClient()
			cfg := service.DefaultImageRuntimeConfig()
			cfg.AsyncEnabled, cfg.AutoArchive = true, true
			cfg.TransientRetries, cfg.TotalRetries, cfg.BillingRetries = 0, 0, 0
			if tc.retry {
				cfg.TransientRetries, cfg.TotalRetries = 1, 1
			}
			require.NoError(t, service.SaveImageRuntimeConfig(t.Context(), cfg))
			_, err = service.SaveImageStorageProfile(t.Context(), model.ImageStorageProfile{Class: "temporary", Backend: "local", Provider: "local", Root: t.TempDir(), Active: true}, nil)
			require.NoError(t, err)
			service.EstimateAsyncImageQuotaFunc = relay.EstimateAsyncImageQuota
			user := model.User{Username: "fallback-user", AffCode: "fallback-user", Group: "default", Status: common.UserStatusEnabled, Quota: 100000}
			if tc.subscription {
				user.SetSetting(dto.UserSetting{BillingPreference: "subscription_only"})
			}
			require.NoError(t, model.DB.Create(&user).Error)
			var subscription model.UserSubscription
			if tc.subscription {
				plan := model.SubscriptionPlan{Title: "Fallback image plan"}
				require.NoError(t, model.DB.Create(&plan).Error)
				subscription = model.UserSubscription{UserId: user.Id, PlanId: plan.Id, AmountTotal: 100000, Status: "active", StartTime: time.Now().Unix(), EndTime: time.Now().Unix() + 86400}
				require.NoError(t, model.DB.Create(&subscription).Error)
			}
			token := model.Token{UserId: user.Id, Key: "fallback-test-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100000, Group: "default"}
			require.NoError(t, model.DB.Create(&token).Error)
			urls := []string{"https://cdn.example.com/result-1.png", "https://cdn.example.com/result-2.png"}
			if tc.privateURL {
				urls[0] = "http://127.0.0.1/private.png"
			}
			var polls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method, "fallback must never submit another generation")
				assert.Equal(t, "/tasks/vendor-job", r.URL.Path)
				assert.Equal(t, "Bearer upstream-test-key", r.Header.Get("Authorization"))
				polls.Add(1)
				body := map[string]any{"status": "done", "urls": urls}
				if tc.usage {
					body["usage"] = map[string]int{"input": 10, "output": 20}
				}
				encoded, marshalErr := common.Marshal(body)
				if !assert.NoError(t, marshalErr) {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(encoded)
			}))
			t.Cleanup(upstream.Close)
			var asyncConfig dto.UpstreamAsyncConfig
			require.NoError(t, common.UnmarshalJsonStr(`{"profiles":[{"id":"fallback","media_type":"image","models":["image-fallback-model"],"operations":["generate"],"submit":{"task_id_path":"id"},"poll":{"request":{"method":"GET","path":"/tasks/{task_id}","headers":{"Authorization":"Bearer {api_key}"}},"response":{"status_path":"status","status_values":{"succeeded":["done"],"failed":["error"]},"result_path":"urls","usage_paths":{"prompt_tokens":"usage.input","completion_tokens":"usage.output"}}}}]}`, &asyncConfig))
			if tc.downloadKey {
				asyncConfig.Profiles[0].Poll.Response.DownloadHeaders = map[string]string{"Authorization": "Bearer {api_key}"}
			}
			require.NoError(t, asyncConfig.Validate())
			channel := model.Channel{Key: "upstream-test-key", BaseURL: &upstream.URL, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "image-fallback-model", Group: "default"}
			channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamAsync: &asyncConfig})
			require.NoError(t, model.DB.Create(&channel).Error)
			require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: "image-fallback-model", ChannelId: channel.Id, Enabled: true}).Error)
			catalog, err := common.Marshal([]service.ImageModelCapability{{Id: "image-fallback-model", MaxOutputImages: 3, MaxReferenceImages: 1, Resolutions: []string{"2K"}}})
			require.NoError(t, err)
			require.NoError(t, model.DB.Create(&model.ImageGroupPolicy{PolicyKey: service.ImageIdentityHash("default", "openai"), Group: "default", Platform: "openai", Enabled: true, AsyncEnabled: true, PoolMode: "resolution", Models: string(catalog), Version: 1}).Error)
			requested := 2
			if tc.requested > 0 {
				requested = tc.requested
			}
			body, err := common.Marshal(map[string]any{"model": "image-fallback-model", "prompt": "draw a panda", "n": requested, "resolution": "2K", "aspect_ratio": "16:9"})
			require.NoError(t, err)
			request, err := service.ParseAsyncImageRequest(body, "application/json", "/v1/images/generations_async", cfg)
			require.NoError(t, err)
			frozen, err := relay.FreezeAsyncImageBilling(t.Context(), token, request, "default", &channel)
			require.NoError(t, err)
			request.Billing = &frozen
			requestBytes, err := common.Marshal(request)
			require.NoError(t, err)
			now := time.Now().Unix()
			task := model.AsyncImageTask{TaskId: "asyncimg_fallback", UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Group: "default", Platform: "openai", Dialect: "async", Model: request.Model, Status: model.ImageTaskQueued, BillingStatus: "pending", Version: 1, UpstreamTaskId: "vendor-job", Provider: "upstream_async", DispatchedAt: now, StartedAt: now, CreatedAt: now, NextAttemptAt: now, ExpiresAt: now + 3600, RequestedResolution: "2K"}
			task.RequestCipher, err = service.EncryptImagePayload(requestBytes, "task:"+task.TaskId)
			require.NoError(t, err)
			channelBytes, err := common.Marshal(channel)
			require.NoError(t, err)
			task.SelectedChannelCipher, err = service.EncryptImagePayload(channelBytes, "image-channel:"+task.TaskId)
			require.NoError(t, err)
			require.NoError(t, model.DB.Create(&task).Error)
			// Change live pricing after admission. Completion must use the frozen
			// expression and group multiplier rather than the new administrator value.
			if tc.expression != "" {
				pricing.BillingExpr[request.Model] = `tier("changed", fixed(0.5)) * image_count`
			}
			service.ExecuteAsyncImageFunc = func(ctx context.Context, task model.AsyncImageTask, req service.AsyncImageRequest, selected *model.Channel, runtime service.ImageRuntimeConfig, dispatch func() error) (*service.AsyncImageOutput, model.AsyncImageBill, error) {
				net.DefaultResolver = mediaResolver
				output, bill, executeErr := relay.ExecuteAsyncImage(ctx, task, req, selected, runtime, dispatch)
				net.DefaultResolver = previousResolver
				if tc.lowBalance && executeErr == nil {
					executeErr = model.DB.Model(&model.User{}).Where("id = ?", user.Id).Update("quota", 1).Error
				}
				return output, bill, executeErr
			}
			if tc.logFailure {
				failOnce := true
				require.NoError(t, model.LOG_DB.Callback().Create().Before("gorm:create").Register("fallback:test-log", func(tx *gorm.DB) {
					if failOnce && tx.Statement.Table == "logs" {
						failOnce = false
						tx.AddError(errors.New("test log temporarily unavailable"))
					}
				}))
			}
			require.NoError(t, service.RunAsyncImageTask(t.Context(), task.TaskId))
			if tc.retry {
				require.NoError(t, model.DB.Where("task_id = ?", task.TaskId).Take(&task).Error)
				assert.Equal(t, model.ImageTaskInvoking, task.Status)
				assert.Equal(t, 1, task.TransientRetryCount)
				assert.Empty(t, fallbackQuery(t, task)["data"])
				require.NoError(t, model.DB.Model(&task).Update("next_attempt_at", now).Error)
				require.NoError(t, service.RunAsyncImageTask(t.Context(), task.TaskId))
			}
			require.NoError(t, model.DB.Where("task_id = ?", task.TaskId).Take(&task).Error)
			if tc.missingUsage || tc.privateURL || tc.downloadKey {
				assert.Equal(t, model.ImageTaskFailed, task.Status, task.ErrorMessage)
				assert.Empty(t, fallbackQuery(t, task)["data"])
				require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&user).Error)
				assert.Equal(t, 100000, user.Quota)
				assert.Zero(t, user.UsedQuota)
				return
			}
			if tc.lowBalance || tc.logFailure {
				require.Equal(t, model.ImageTaskBillingFailed, task.Status, task.ErrorMessage)
				assert.False(t, task.ResultsAvailable())
				assert.Empty(t, fallbackQuery(t, task)["data"])
				for _, admin := range []bool{false, true} {
					recorder := fallbackTaskDetail(t, task, admin)
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					assert.NotContains(t, recorder.Body.String(), urls[0], "pending settlement must not disclose the unpaid result")
				}
				require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&user).Error)
				if tc.lowBalance {
					assert.Zero(t, user.UsedQuota)
					require.NoError(t, model.DB.Model(&user).Update("quota", 100000).Error)
				} else {
					assert.Equal(t, tc.quota, user.UsedQuota)
				}
				require.NoError(t, model.ResumeAsyncImageTask(t.Context(), task))
				if tc.lowBalance {
					var bill model.AsyncImageBill
					require.NoError(t, model.DB.Where("task_id = ?", task.TaskId).Take(&bill).Error)
					results := make(chan error, 2)
					var settlements sync.WaitGroup
					for range 2 {
						settlements.Go(func() { results <- model.ApplyAsyncImageBill(t.Context(), task.TaskId, bill.Fingerprint) })
					}
					settlements.Wait()
					for range 2 {
						settlementErr := <-results
						if model.DB.Dialector.Name() == "sqlite" && settlementErr != nil && strings.Contains(settlementErr.Error(), "SQLITE_BUSY") {
							// SQLite can reject a competing write transaction. Retry the
							// same immutable bill after both attempts have finished.
							require.NoError(t, model.ApplyAsyncImageBill(t.Context(), task.TaskId, bill.Fingerprint))
						} else {
							require.NoError(t, settlementErr)
						}
					}
				}
				require.NoError(t, service.RunAsyncImageTask(t.Context(), task.TaskId))
				require.NoError(t, model.DB.Where("task_id = ?", task.TaskId).Take(&task).Error)
			}
			require.Equal(t, model.ImageTaskSucceeded, task.Status, "%s: %s", task.ErrorCode, task.ErrorMessage)
			assert.Equal(t, model.ImageResultSourceUpstream, task.ResultSource)
			assert.Equal(t, tc.quota, task.Quota)
			assert.Empty(t, task.ErrorCode)
			assert.Empty(t, task.ErrorMessage)
			assert.Zero(t, task.NextAttemptAt)
			queryResult := fallbackQuery(t, task)
			assert.Equal(t, "succeeded", queryResult["status"])
			assert.Equal(t, "upstream", queryResult["storage_status"])
			assert.Equal(t, "upstream", queryResult["result_source"])
			assert.Equal(t, []any{map[string]any{"url": urls[0]}, map[string]any{"url": urls[1]}}, queryResult["data"])
			if tc.quota == 0 {
				assert.Equal(t, "not_billable", task.BillingStatus)
			} else {
				assert.Equal(t, "succeeded", task.BillingStatus)
			}
			require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&user).Error)
			require.NoError(t, model.DB.Where("id = ?", token.Id).Take(&token).Error)
			require.NoError(t, model.DB.Where("id = ?", channel.Id).Take(&channel).Error)
			walletQuota := 100000 - tc.quota
			if tc.subscription {
				walletQuota = 100000
				require.NoError(t, model.DB.Where("id = ?", subscription.Id).Take(&subscription).Error)
				assert.EqualValues(t, tc.quota, subscription.AmountUsed)
			}
			assert.Equal(t, walletQuota, user.Quota)
			assert.Equal(t, tc.quota, user.UsedQuota)
			assert.Equal(t, 1, user.RequestCount)
			assert.Equal(t, 100000-tc.quota, token.RemainQuota)
			assert.EqualValues(t, tc.quota, channel.UsedQuota)
			var bill model.AsyncImageBill
			require.NoError(t, model.DB.Where("task_id = ?", task.TaskId).Take(&bill).Error)
			if tc.subscription {
				assert.Equal(t, "subscription", bill.FundingSource)
				assert.Equal(t, subscription.Id, bill.SubscriptionId)
			} else {
				assert.Equal(t, "wallet", bill.FundingSource)
			}
			require.NoError(t, model.ApplyAsyncImageBill(t.Context(), task.TaskId, bill.Fingerprint))
			require.NoError(t, model.ConfirmAsyncImageLog(t.Context(), task.TaskId))
			assert.ErrorIs(t, service.RunAsyncImageTask(t.Context(), task.TaskId), model.ErrImageConflict)
			assert.ErrorIs(t, model.ResumeAsyncImageTask(t.Context(), task), model.ErrImageConflict)
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("request_id = ?", bill.BillingRequestId).Find(&logs).Error)
			require.Len(t, logs, 1)
			assert.Equal(t, tc.quota, logs[0].Quota)
			assert.Contains(t, logs[0].Other, `"result_source":"upstream"`)
			assert.Contains(t, logs[0].Other, `"image_billing_specification_source":"request"`)
			assert.NotContains(t, logs[0].Other, `"actual_width"`)
			var localResults, archives, milestones int64
			require.NoError(t, model.DB.Model(&model.AsyncImageResult{}).Where("task_id = ?", task.TaskId).Count(&localResults).Error)
			require.NoError(t, model.DB.Model(&model.ImageOutbox{}).Where("aggregate_id = ? AND kind = ?", task.TaskId, "library_archive").Count(&archives).Error)
			require.NoError(t, model.DB.Model(&model.AsyncImageEvent{}).Where("task_id = ? AND event_type = ?", task.TaskId, "upstream_result_received").Count(&milestones).Error)
			assert.Zero(t, localResults, "fallback media must not masquerade as stored objects")
			assert.Zero(t, archives, "unvalidated remote media must not be automatically archived")
			assert.EqualValues(t, 1, milestones)
			expectedPolls := 1
			if tc.retry {
				expectedPolls++
			}
			assert.EqualValues(t, expectedPolls, polls.Load(), "billing recovery must not requery or resubmit the upstream")
			for _, admin := range []bool{false, true} {
				recorder := fallbackTaskDetail(t, task, admin)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				assert.Contains(t, recorder.Body.String(), urls[0])
				assert.Contains(t, recorder.Body.String(), `"source":"upstream"`)
			}
			if tc.retry {
				for _, delivery := range []struct {
					name    string
					index   string
					json    bool
					admin   bool
					user    int
					status  int
					expired bool
				}{
					{name: "owner_json", index: "1", json: true, user: user.Id, status: 200},
					{name: "owner_redirect", index: "1", user: user.Id, status: 307},
					{name: "administrator_json", index: "1", json: true, admin: true, status: 200},
					{name: "other_owner", index: "1", user: user.Id + 1, status: 404},
					{name: "invalid_index", index: "-1", user: user.Id, status: 400},
					{name: "missing_index", index: "2", user: user.Id, status: 404},
					{name: "expired_result", index: "1", user: user.Id, status: 404, expired: true},
				} {
					t.Run(delivery.name, func(t *testing.T) {
						if delivery.expired {
							require.NoError(t, model.DB.Model(&task).Update("expires_at", now-1).Error)
						}
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Set("id", delivery.user)
						c.Params = gin.Params{{Key: "task_id", Value: task.TaskId}, {Key: "image_index", Value: delivery.index}}
						c.Request = httptest.NewRequest(http.MethodGet, "/api/user/async-image-tasks/"+task.TaskId+"/results/"+delivery.index+"/view", nil)
						if delivery.json {
							c.Request.Header.Set("Accept", "application/json")
						}
						controller.ViewImageTaskResult(c, delivery.admin)
						require.Equal(t, delivery.status, recorder.Code, recorder.Body.String())
						if delivery.status == 200 {
							assert.Contains(t, recorder.Body.String(), urls[1])
							assert.Contains(t, recorder.Body.String(), `"source":"upstream"`)
						} else if delivery.status == 307 {
							assert.Equal(t, urls[1], recorder.Header().Get("Location"))
						} else {
							assert.NotContains(t, recorder.Body.String(), urls[1])
							assert.Empty(t, recorder.Header().Get("Location"))
						}
					})
				}
				for _, identity := range []struct{ user, token int }{{user.Id + 1, token.Id}, {user.Id, token.Id + 1}, {user.Id, token.Id}} {
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Set("id", identity.user)
					c.Set("token_id", identity.token)
					c.Params = gin.Params{{Key: "task_id", Value: task.TaskId}}
					c.Request = httptest.NewRequest(http.MethodGet, "/v1/media/tasks_async/"+task.TaskId, nil)
					controller.QueryAsyncImage(c)
					assert.Equal(t, http.StatusNotFound, recorder.Code)
					assert.NotContains(t, recorder.Body.String(), urls[0])
				}
			}
			require.NoError(t, model.DB.Model(&task).Update("expires_at", now-1).Error)
			require.NoError(t, model.DB.Where("task_id = ?", task.TaskId).Take(&task).Error)
			assert.False(t, task.ResultsAvailable())
		})
	}
}

func fallbackQuery(t *testing.T, task model.AsyncImageTask) gin.H {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", task.UserId)
	c.Set("token_id", task.TokenId)
	c.Params = gin.Params{{Key: "task_id", Value: task.TaskId}}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/media/tasks_async/"+task.TaskId, nil)
	controller.QueryAsyncImage(c)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var result gin.H
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
	return result
}

func fallbackTaskDetail(t *testing.T, task model.AsyncImageTask, admin bool) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", task.UserId)
	c.Params = gin.Params{{Key: "task_id", Value: task.TaskId}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/user/async-image-tasks/"+task.TaskId, nil)
	controller.GetImageTask(c, admin)
	return recorder
}

type fallbackMigrationTrace struct {
	logger.Interface
	changes []string
}

func (trace *fallbackMigrationTrace) Trace(_ context.Context, _ time.Time, sql func() (string, int64), _ error) {
	statement, _ := sql()
	upper := strings.ToUpper(strings.TrimSpace(statement))
	if strings.HasPrefix(upper, "CREATE ") || strings.HasPrefix(upper, "ALTER ") || strings.HasPrefix(upper, "DROP ") {
		trace.changes = append(trace.changes, statement)
	}
}

func TestAsyncImageUpstreamFallbackTaskMigration(t *testing.T) {
	db := imageRouterDatabase(t, "fallback-migration")
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}))
	trace := &fallbackMigrationTrace{Interface: logger.Default.LogMode(logger.Silent)}
	require.NoError(t, db.Session(&gorm.Session{Logger: trace}).AutoMigrate(&model.AsyncImageTask{}))
	assert.Empty(t, trace.changes, "restarting a fresh database must not alter the task schema again")
	legacy := model.AsyncImageTask{TaskId: "legacy-before-fallback", UserId: 17, TokenId: 23, Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ImageCount: 1, ResultCount: 1, Version: 8, Quota: 400, RequestCipher: []byte("preserved-legacy-cipher")}
	require.NoError(t, db.Create(&legacy).Error)
	// Recreate the previous task schema by removing only the newly added column.
	require.NoError(t, db.Migrator().DropColumn(&model.AsyncImageTask{}, "result_source"))
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}))
	trace.changes = nil
	require.NoError(t, db.Session(&gorm.Session{Logger: trace}).AutoMigrate(&model.AsyncImageTask{}))
	assert.Empty(t, trace.changes, "restarting after upgrade must not alter the task schema again")
	var preserved model.AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", legacy.TaskId).Take(&preserved).Error)
	assert.Empty(t, preserved.ResultSource, "previous results continue to use local storage")
	assert.Equal(t, legacy.Quota, preserved.Quota)
	assert.Equal(t, legacy.Version, preserved.Version)
	assert.Equal(t, legacy.RequestCipher, preserved.RequestCipher)
	duplicate := legacy
	duplicate.Id = 0
	assert.Error(t, db.Create(&duplicate).Error, "migration must preserve the unique task identity")
}
