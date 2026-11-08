package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
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

func TestAsyncImageAdmissionPreservesMixedRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, _ := newAuditTestDatabase(t, "sqlite", "")
	previousDB, previousType := model.DB, common.MainDatabaseType()
	previousRedis, previousRDB := common.RedisEnabled, common.RDB
	previousCapability, previousValidate := service.ImageChannelCapabilityFunc, service.ValidateMixedImageChannelRequestFunc
	previousEstimate, previousFreeze, previousExecute := service.EstimateAsyncImageQuotaFunc, service.FreezeAsyncImageBillingFunc, service.ExecuteAsyncImageFunc
	previousGroups, previousRatios := setting.UserUsableGroups2JSONString(), ratio_setting.GroupRatio2JSONString()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	model.DB, common.RDB, common.RedisEnabled = db, client, true
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB, common.RDB, common.RedisEnabled = previousDB, previousRDB, previousRedis
		common.SetMainDatabaseType(previousType)
		service.ImageChannelCapabilityFunc, service.ValidateMixedImageChannelRequestFunc = previousCapability, previousValidate
		service.EstimateAsyncImageQuotaFunc, service.FreezeAsyncImageBillingFunc, service.ExecuteAsyncImageFunc = previousEstimate, previousFreeze, previousExecute
		assert.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousGroups))
		assert.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios))
		assert.NoError(t, client.Close())
	})
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "admission:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "admission")
	require.NoError(t, db.AutoMigrate(&model.Option{}, &model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.ImageStorageProfile{}, &model.ImageInputAlias{}, &model.TaskPerfSample{}, &model.AsyncImageTask{}, &model.AsyncImageBill{}, &model.AsyncImageEvent{}, &model.ImageOutbox{}, &model.MediaRetryPolicyVersion{}, &model.MediaRetryEvent{}))
	require.NoError(t, model.InitializeMediaRetryPolicy(db))
	cfg := service.DefaultImageRuntimeConfig()
	cfg.AsyncEnabled = true
	require.NoError(t, service.SaveImageRuntimeConfig(t.Context(), cfg))
	service.ImageChannelCapabilityFunc = func(channel model.Channel, _ string) dto.ImageCapability {
		if channel.Type == constant.ChannelTypeGemini {
			return dto.ImageCapability{Provider: "gemini", Protocol: "gemini_native", Generate: true, Edit: true}
		}
		return dto.ImageCapability{Provider: "openai", Protocol: "openai_images", Generate: true, Edit: true}
	}
	service.ValidateMixedImageChannelRequestFunc = nil
	service.EstimateAsyncImageQuotaFunc = func(context.Context, model.Token, service.AsyncImageRequest, string) (int, error) { return 0, nil }
	service.FreezeAsyncImageBillingFunc = func(context.Context, model.Token, service.AsyncImageRequest, string, *model.Channel) (service.MediaBillingSnapshot, error) {
		return service.MediaBillingSnapshot{}, nil
	}
	user := model.User{Username: "mixed-admission", Group: "default", Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "fixture-mixed-admission", Group: "default", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	require.NoError(t, db.Create(&model.ImageStorageProfile{ProfileId: "admission-local", Class: "temporary", Backend: "local", Root: t.TempDir(), Active: true}).Error)
	const publicModel = "seedream-5-pro"
	for _, platform := range []string{"mixed", "gemini"} {
		policy := model.ImageGroupPolicy{PolicyKey: service.ImageIdentityHash("default", platform), Group: "default", Platform: platform, Enabled: true, AsyncEnabled: true, PoolMode: "model_resolution", Models: `[{"id":"seedream-5-pro","resolutions":["1K"],"max_output_images":1,"max_reference_images":8}]`}
		require.NoError(t, db.Create(&policy).Error)
	}
	channels := []model.Channel{
		{Id: 1, Type: constant.ChannelTypeOpenAI, Key: "fixture-openai", Models: publicModel, Group: "default", Status: common.ChannelStatusEnabled},
		{Id: 24, Type: constant.ChannelTypeGemini, Key: "fixture-gemini", Models: publicModel, Group: "default", Status: common.ChannelStatusEnabled, ModelMapping: common.GetPointer(`{"seedream-5-pro":"gemini-3.1-flash-image-preview"}`)},
	}
	require.NoError(t, db.Create(&channels).Error)
	for _, channel := range channels {
		require.NoError(t, db.Create(&model.Ability{Group: "default", Model: publicModel, ChannelId: channel.Id, Enabled: true}).Error)
	}
	for _, test := range []struct {
		name, provider, platform, policyPlatform, executionProvider, protocol, size string
		channelID                                                                   int
	}{
		{name: "mixed Gemini", platform: "openai", policyPlatform: "mixed", executionProvider: "gemini", protocol: "gemini_native", size: "2384x1024", channelID: 24},
		{name: "mixed OpenAI", platform: "openai", policyPlatform: "mixed", executionProvider: "openai", protocol: "openai_images", size: "2384x1024", channelID: 1},
		{name: "explicit Gemini", provider: "gemini", platform: "gemini", policyPlatform: "gemini", executionProvider: "gemini", protocol: "gemini_native", size: "1K", channelID: 24},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, db.Where("1 = 1").Delete(&model.ImageChannelPool{}).Error)
			for _, platform := range []string{"mixed", "gemini"} {
				policy := model.ImageGroupPolicy{Group: "default", Platform: platform, PoolMode: "model_resolution"}
				require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: service.ImagePoolBindingKey(policy, publicModel, "1K"), Group: "default", Platform: platform, Mode: "model_resolution", Model: publicModel, Resolution: "1K", ChannelId: test.channelID, Priority: 1}).Error)
			}
			body := map[string]any{"model": publicModel, "prompt": "A wide landscape", "size": test.size, "resolution": "1K", "aspect_ratio": "21:9", "image": []string{"https://reference.example/input.png"}}
			if test.provider != "" {
				body["provider"] = test.provider
			}
			raw, err := common.Marshal(body)
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations_async", bytes.NewReader(raw))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("id", user.Id)
			c.Set("token_id", token.Id)
			SubmitAsyncImage(c)
			require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())
			var accepted struct {
				TaskID string `json:"task_id"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &accepted))
			require.NotEmpty(t, accepted.TaskID)
			var task model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", accepted.TaskID).Take(&task).Error)
			encoded, err := service.DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
			require.NoError(t, err)
			var request service.AsyncImageRequest
			require.NoError(t, common.Unmarshal(encoded, &request))
			assert.Equal(t, test.platform, request.Platform)
			assert.Equal(t, test.policyPlatform, request.PolicyPlatform)
			assert.Equal(t, test.provider, request.Provider)
			assert.Equal(t, test.platform, task.Platform)
			assert.Equal(t, test.executionProvider, task.Provider)
			assert.Equal(t, test.protocol, task.ExecutionProtocol)
			assert.Equal(t, 1, request.ReferenceImageCount())
			assert.Equal(t, test.size, request.Size)
			assert.Equal(t, test.provider == "", request.MixedRouting != nil)
			calls := 0
			service.ExecuteAsyncImageFunc = func(ctx context.Context, current model.AsyncImageTask, outbound service.AsyncImageRequest, channel *model.Channel, _ service.ImageRuntimeConfig, beforeInvoke func() error) (*service.AsyncImageOutput, model.AsyncImageBill, error) {
				calls++
				assert.Equal(t, test.channelID, channel.Id)
				assert.Equal(t, request.Platform, outbound.Platform)
				assert.Equal(t, request.MixedRouting, outbound.MixedRouting)
				require.NoError(t, beforeInvoke(), "persisted admission state must pass both worker eligibility checks")
				require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", current.TaskId).Update("upstream_task_id", "fixture-accepted").Error)
				return nil, model.AsyncImageBill{}, &service.AsyncImagePending{TaskId: "fixture-accepted"}
			}
			require.NoError(t, service.RunAsyncImageTask(t.Context(), task.TaskId))
			assert.Equal(t, 1, calls)
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			assert.Equal(t, model.ImageTaskInvoking, task.Status)
			assert.Positive(t, task.DispatchedAt)
			assert.Equal(t, "fixture-accepted", task.UpstreamTaskId)
			assert.Empty(t, task.ErrorCode)
		})
	}
}

func testMixedVideoPublicPricingUsesGroupPolicyAndHostSchema(t *testing.T, db *gorm.DB) {
	t.Cleanup(func() {
		assert.NoError(t, db.Where("policy_key IN ?", []string{"one", "two", "three"}).Delete(&model.ImageGroupPolicy{}).Error)
	})
	for _, policy := range []model.ImageGroupPolicy{
		{PolicyKey: "one", Group: "default", Platform: "mixed", Enabled: true, AsyncEnabled: true, PoolMode: "model_resolution", Models: `[{"id":"video-a","media_type":"video"},{"id":"video-b","media_type":"video"},{"id":"image-a","media_type":"image"}]`},
		{PolicyKey: "two", Group: "private", Platform: "mixed", Enabled: true, AsyncEnabled: true, Models: `[{"id":"video-a","media_type":"video"}]`},
		{PolicyKey: "three", Group: "disabled", Platform: "mixed", Enabled: false, AsyncEnabled: true, Models: `[{"id":"video-a","media_type":"video"}]`},
	} {
		require.NoError(t, db.Create(&policy).Error)
	}
	settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	previous := *settings
	t.Cleanup(func() { *settings = previous })
	settings.BillingMode = map[string]string{"video-a": "tiered_expr", "video-b": "tiered_expr"}
	settings.BillingExpr = map[string]string{"video-a": `tier("base", u("requests") * 1)`, "video-b": `tier("invalid", u("video_count"))`}
	settings.MixedVideoBillingExpr = map[string]string{"video-a": `tier("mixed", u("requests") * 2)`}
	prices := []model.Pricing{{ModelName: "video-a", BillingExpr: "provider", BillingPluginVariants: []model.PricingPluginVariant{{PluginKey: "vendor"}}}, {ModelName: "video-b"}, {ModelName: "image-a"}}
	groups := map[string]string{"default": "Default", "disabled": "Disabled"}
	require.NoError(t, addMixedVideoPricing(t.Context(), prices, groups))
	require.NotNil(t, prices[0].MixedVideoPricing)
	assert.Equal(t, []string{"default", "disabled"}, prices[0].MixedVideoPricing.Groups)
	assert.Empty(t, prices[0].MixedVideoPricing.EnabledGroups)
	assert.Equal(t, "override", prices[0].MixedVideoPricing.Source)
	assert.Equal(t, settings.MixedVideoBillingExpr["video-a"], prices[0].MixedVideoPricing.BillingExpr)
	assert.Contains(t, prices[0].MixedVideoPricing.BillingUsageSchema, "video_input")
	assert.Equal(t, "provider", prices[0].BillingExpr)
	assert.Equal(t, "vendor", prices[0].BillingPluginVariants[0].PluginKey)
	require.NotNil(t, prices[1].MixedVideoPricing)
	assert.Equal(t, "unconfigured", prices[1].MixedVideoPricing.Source)
	assert.Nil(t, prices[2].MixedVideoPricing)
	delete(settings.MixedVideoBillingExpr, "video-a")
	require.NoError(t, addMixedVideoPricing(t.Context(), prices, groups))
	assert.Equal(t, "model", prices[0].MixedVideoPricing.Source)
	assert.Equal(t, settings.BillingExpr["video-a"], prices[0].MixedVideoPricing.BillingExpr)

	cfg := service.DefaultMediaRuntimeConfig()
	cfg.VideoAsyncEnabled = true
	encoded, err := common.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.Option{Key: "media_runtime_config", Value: string(encoded)}).Error)
	t.Cleanup(func() {
		assert.NoError(t, db.Where(map[string]any{"key": "media_runtime_config"}).Delete(&model.Option{}).Error)
	})
	require.NoError(t, addMixedVideoPricing(t.Context(), prices, groups))
	assert.Equal(t, []string{"default"}, prices[0].MixedVideoPricing.EnabledGroups)
	assert.Equal(t, []string{"default"}, prices[1].MixedVideoPricing.EnabledGroups)

}

func TestMixedImageConfigurationDatabaseMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var dialector gorm.Dialector
			versionQuery := "SELECT sqlite_version()"
			switch engine {
			case "mysql":
				dsn := os.Getenv("IMAGE_CONFIG_TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("IMAGE_CONFIG_TEST_MYSQL_DSN is not configured")
				}
				require.Contains(t, dsn, "/new_api_image_config_test", "Use a dedicated disposable database")
				dialector = mysql.Open(dsn)
				versionQuery = "SELECT VERSION()"
			case "postgres":
				dsn := os.Getenv("IMAGE_CONFIG_TEST_PG_DSN")
				if dsn == "" {
					t.Skip("IMAGE_CONFIG_TEST_PG_DSN is not configured")
				}
				require.True(t, strings.Contains(dsn, "dbname=new_api_image_config_test") || strings.Contains(dsn, "/new_api_image_config_test"), "Use a dedicated disposable database")
				dialector = postgres.Open(dsn)
				versionQuery = "SELECT VERSION()"
			default:
				dialector = sqlite.Open(filepath.Join(t.TempDir(), "mixed-images.db"))
			}
			db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			if engine == "sqlite" {
				connection.SetMaxOpenConns(1)
			}
			previousDB, previousType := model.DB, common.MainDatabaseType()
			previousLogDB, previousLogType, previousRedis := model.LOG_DB, common.LogDatabaseType(), common.RedisEnabled
			previousCapability := service.ImageChannelCapabilityFunc
			model.DB = db
			model.LOG_DB, common.RedisEnabled = db, false
			common.SetMainDatabaseType(common.DatabaseType(engine))
			common.SetLogDatabaseType(common.DatabaseType(engine))
			service.ImageChannelCapabilityFunc = func(channel model.Channel, _ string) dto.ImageCapability {
				provider := ""
				switch channel.Type {
				case 1:
					provider = "openai"
				case 24:
					return dto.ImageCapability{Provider: "gemini", Protocol: "gemini_native", Generate: true, Edit: true}
				case 45:
					provider = "volcengine"
				case 60:
					provider = "newapi"
				}
				return dto.ImageCapability{Provider: provider, Protocol: "openai_images", Generate: provider != "", Edit: provider != "", ReferenceField: "image"}
			}
			entities := []any{&model.ImageChannelPool{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.Ability{}, &model.Channel{}, &model.Token{}, &model.User{}, &model.Option{}, &model.AuditLog{}}
			t.Cleanup(func() {
				model.DB, service.ImageChannelCapabilityFunc = previousDB, previousCapability
				common.SetMainDatabaseType(previousType)
				model.LOG_DB, common.RedisEnabled = previousLogDB, previousRedis
				common.SetLogDatabaseType(previousLogType)
				assert.NoError(t, db.Migrator().DropTable(entities...))
				assert.NoError(t, connection.Close())
			})
			require.NoError(t, db.AutoMigrate(entities...))
			require.NoError(t, db.AutoMigrate(entities...))
			t.Run("mixed video public pricing", func(t *testing.T) { testMixedVideoPublicPricingUsesGroupPolicyAndHostSchema(t, db) })
			var version string
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("Database version: %s", version)

			const publicModel = "seedream-5-pro"
			artsOrigin, otherOrigin, openaiOrigin := "https://apis.artsapi.example", "https://other-newapi.example", "https://openai.example"
			channels := []model.Channel{
				{Id: 1, Type: 1, Name: "OpenAI image provider", Key: "fixture-openai-key", BaseURL: &openaiOrigin, Status: common.ChannelStatusEnabled, Models: publicModel, ModelMapping: common.GetPointer(`{"seedream-5-pro":"gpt-image-1"}`), Group: "default"},
				{Id: 24, Type: 24, Name: "Gemini native image provider", Key: "fixture-gemini-key", Status: common.ChannelStatusEnabled, Models: publicModel, Group: "default"},
				{Id: 29, Type: 60, Name: "Legacy NewAPI ArtsAPI", Key: "fixture-shared-key", BaseURL: &artsOrigin, Status: common.ChannelStatusEnabled, Models: publicModel, Group: "default"},
				{Id: 45, Type: 45, Name: "Native Ark ArtsAPI", Key: "fixture-shared-key", BaseURL: &artsOrigin, Status: common.ChannelStatusEnabled, Models: publicModel, Group: "default"},
				{Id: 60, Type: 60, Name: "Other NewAPI", Key: "fixture-other-key", BaseURL: &otherOrigin, Status: common.ChannelStatusEnabled, Models: publicModel, Group: "default"},
				{Id: 54, Type: 54, Name: "Video task provider", Key: "fixture-video-key", Status: common.ChannelStatusEnabled, Models: publicModel, Group: "default"},
			}
			require.NoError(t, db.Create(&channels).Error)
			for _, channel := range channels {
				require.NoError(t, db.Create(&model.Ability{Group: "default", Model: publicModel, ChannelId: channel.Id, Enabled: true}).Error)
			}
			catalog, err := common.Marshal([]service.ImageModelCapability{{Id: publicModel, Label: "Seedream", MediaType: "image", MaxOutputImages: 1, MaxReferenceImages: 8, Resolutions: []string{"1K", "2K"}}})
			require.NoError(t, err)
			require.NoError(t, db.Create(&model.User{Id: 701, Username: "mixed-db-fixture", Group: "default", Status: common.UserStatusEnabled}).Error)
			token := model.Token{Id: 702, UserId: 701, Key: "mixed-database-fixture-token", Group: "default", Status: common.TokenStatusEnabled, ExpiredTime: -1}
			require.NoError(t, db.Create(&token).Error)
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set("id", 701); c.Set("role", common.RoleRootUser) })
			router.PUT("/policy", SaveImagePolicy)
			router.DELETE("/policy", DeleteImagePolicy)
			router.PUT("/pool", SaveImageChannelPool)
			router.GET("/options", GetImageChannelPoolOptions)
			router.POST("/preview", PreviewMediaRouting)
			router.GET("/capabilities/:token_id", GetImageWorkbenchCapabilities)
			router.PUT("/token/:id", UpdateTokenImageMappings)
			router.PUT("/option", UpdateOption)
			call := func(method, path string, input any) *httptest.ResponseRecorder {
				body, marshalErr := common.Marshal(input)
				require.NoError(t, marshalErr)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(method, path, bytes.NewReader(body)))
				return recorder
			}
			oldPolicy := model.ImageGroupPolicy{Group: "default", Platform: "newapi", Enabled: true, AsyncEnabled: true, PoolMode: "model_resolution", Models: string(catalog)}
			recorder := call(http.MethodPut, "/policy", oldPolicy)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.NoError(t, db.Where("policy_key = ?", service.ImageIdentityHash("default", "newapi")).Take(&oldPolicy).Error)
			legacyPool := model.ImageChannelPool{Id: 446, BindingKey: service.ImagePoolBindingKey(oldPolicy, publicModel, "1K"), Group: "default", Platform: "newapi", Mode: "model_resolution", Model: publicModel, Resolution: "1K", ChannelId: 29, Priority: 7}
			require.NoError(t, db.Create(&legacyPool).Error)

			mixedPolicy := oldPolicy
			mixedPolicy.Id, mixedPolicy.Version, mixedPolicy.Platform = 0, 0, "mixed"
			recorder = call(http.MethodPut, "/policy", mixedPolicy)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.False(t, service.RegisteredMediaProvider("mixed"), "the routing scope must not become an upstream provider")
			require.NoError(t, db.Where("policy_key = ?", service.ImageIdentityHash("default", "mixed")).Take(&mixedPolicy).Error)
			recorder = call(http.MethodGet, "/options?group=default&platform=mixed", nil)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			var options struct {
				Data service.ImagePoolSelectionOptions `json:"data"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &options))
			require.Len(t, options.Data.Models, 1)
			assert.ElementsMatch(t, []int{1, 24, 29, 45, 60}, options.Data.Models[0].ChannelIds)
			assert.NotContains(t, recorder.Body.String(), "fixture-shared-key")

			for _, mode := range []string{"resolution", "model", "model_resolution"} {
				t.Run(mode+" copy and revision", func(t *testing.T) {
					body := gin.H{"group": "default", "platform": "mixed", "mode": mode, "model": publicModel, "resolution": "1K", "channels": []gin.H{{"id": 1, "priority": 5}, {"id": 45, "priority": 7}, {"id": 60, "priority": 3}}}
					recorder := call(http.MethodPut, "/pool", body)
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					key := service.ImagePoolBindingKey(model.ImageGroupPolicy{Group: "default", Platform: "mixed", PoolMode: mode}, publicModel, "1K")
					var rows []model.ImageChannelPool
					require.NoError(t, db.Where("binding_key = ?", key).Order("channel_id").Find(&rows).Error)
					require.Len(t, rows, 3)
					assert.Equal(t, 5, rows[0].Priority)
					assert.Equal(t, 7, rows[1].Priority)
					assert.Equal(t, 3, rows[2].Priority)
					recorder = call(http.MethodPut, "/pool", body)
					assert.Equal(t, http.StatusConflict, recorder.Code, "copy cannot overwrite an existing binding")
					body["expected_ids"] = []int{rows[0].Id, rows[1].Id, rows[2].Id}
					body["channels"] = []gin.H{{"id": 1, "priority": 5}, {"id": 45, "priority": 2}, {"id": 60, "priority": 8}}
					recorder = call(http.MethodPut, "/pool", body)
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					candidates, err := service.ImageCandidateChannels(t.Context(), model.ImageGroupPolicy{Group: "default", Platform: "mixed", PoolMode: mode, AsyncEnabled: true}, publicModel, "1K")
					require.NoError(t, err)
					weights := make(map[int]int)
					for _, candidate := range candidates {
						weights[candidate.Id] = candidate.GetWeight()
						assert.Zero(t, candidate.GetPriority(), "mixed members share the same weighted scheduling tier")
					}
					assert.Equal(t, map[int]int{1: 5, 45: 2, 60: 8}, weights)
					recorder = call(http.MethodPut, "/pool", body)
					assert.Equal(t, http.StatusConflict, recorder.Code, "a stale editor cannot replace a newer revision")
					var unchanged model.ImageChannelPool
					require.NoError(t, db.Where("id = ?", 446).Take(&unchanged).Error)
					assert.Equal(t, legacyPool, unchanged)
				})
			}
			t.Run("preview includes additional image provider and explains protocol exclusions", func(t *testing.T) {
				recorder := call(http.MethodPost, "/preview", gin.H{"group": "default", "platform": "mixed", "media_type": "image", "model": publicModel, "resolution": "1K", "operation": "text_to_image"})
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var response struct {
					Data service.MediaRoutingPreview `json:"data"`
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				providers, weights := make(map[int]string), make(map[int]int)
				for _, candidate := range response.Data.Eligible {
					providers[candidate.ChannelID], weights[candidate.ChannelID] = candidate.Provider, candidate.Weight
					assert.Equal(t, "openai_images", candidate.Protocol)
					if candidate.ChannelID == 1 {
						assert.Equal(t, "gpt-image-1", candidate.UpstreamModel)
					}
				}
				assert.Equal(t, map[int]string{1: "openai", 45: "volcengine", 60: "newapi"}, providers)
				assert.Equal(t, map[int]int{1: 5, 45: 2, 60: 8}, weights)
				excluded := make(map[int][]string)
				for _, candidate := range response.Data.Excluded {
					excluded[candidate.ChannelID] = candidate.Reasons
					if candidate.ChannelID == 54 {
						assert.Contains(t, candidate.Reasons, "platform_mismatch")
					}
				}
				assert.Contains(t, excluded, 24)
				assert.Contains(t, excluded, 54)
				assert.NotContains(t, recorder.Body.String(), "fixture-openai-key")
			})
			t.Run("image bindings reject video-only members and video preview reports exclusion", func(t *testing.T) {
				for _, channelID := range []int{54} {
					recorder := call(http.MethodPut, "/pool", gin.H{"group": "default", "platform": "mixed", "mode": "model_resolution", "model": publicModel, "resolution": "2K", "channels": []gin.H{{"id": channelID, "priority": 1}}})
					assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
				}
				recorder := call(http.MethodPost, "/preview", gin.H{"group": "default", "platform": "mixed", "media_type": "video", "model": publicModel, "resolution": "480p"})
				assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				assert.Contains(t, recorder.Body.String(), "model_not_in_policy")
			})

			t.Run("duplicate supplier is rejected without exposing credentials", func(t *testing.T) {
				recorder := call(http.MethodPut, "/pool", gin.H{"group": "default", "platform": "mixed", "mode": "model_resolution", "model": publicModel, "resolution": "2K", "channels": []gin.H{{"id": 29, "priority": 1}, {"id": 45, "priority": 1}}})
				assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
				assert.Contains(t, recorder.Body.String(), "duplicate")
				assert.NotContains(t, recorder.Body.String(), "fixture-shared-key")
			})
			t.Run("closed binding survives storage and prevents unbound candidates", func(t *testing.T) {
				recorder := call(http.MethodPut, "/pool", gin.H{"group": "default", "platform": "mixed", "mode": "model_resolution", "model": publicModel, "resolution": "2K", "channels": []gin.H{}})
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var rows []model.ImageChannelPool
				require.NoError(t, db.Where("binding_key = ?", service.ImagePoolBindingKey(mixedPolicy, publicModel, "2K")).Find(&rows).Error)
				require.Len(t, rows, 1)
				assert.Zero(t, rows[0].ChannelId)
				candidates, err := service.ImageCandidateChannels(context.Background(), mixedPolicy, publicModel, "2K")
				require.NoError(t, err)
				assert.Empty(t, candidates)
			})
			t.Run("workbench exposes mixed scope without a provider", func(t *testing.T) {
				require.NoError(t, db.Model(&model.Channel{}).Where("id IN ?", []int{45, 60}).Update("status", common.ChannelStatusManuallyDisabled).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Model(&model.Channel{}).Where("id IN ?", []int{45, 60}).Update("status", common.ChannelStatusEnabled).Error)
				})
				previousPrices := ratio_setting.ModelPrice2JSONString()
				t.Cleanup(func() { assert.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrices)) })
				prices := ratio_setting.GetModelPriceCopy()
				prices[publicModel] = 0.12
				encoded, err := common.Marshal(prices)
				require.NoError(t, err)
				require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(encoded)))
				recorder := call(http.MethodGet, "/capabilities/"+strconv.Itoa(token.Id), nil)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var response struct {
					Data struct {
						Models []map[string]any `json:"models"`
					} `json:"data"`
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				foundMixed, foundLegacy := false, false
				for _, capability := range response.Data.Models {
					if capability["id"] != publicModel {
						continue
					}
					switch capability["platform"] {
					case "mixed":
						foundMixed = true
						assert.Equal(t, "openai_images", capability["protocol"])
						assert.NotContains(t, capability, "provider")
					case "newapi":
						foundLegacy = true
						assert.Equal(t, "newapi", capability["provider"])
					}
				}
				assert.True(t, foundMixed)
				assert.True(t, foundLegacy)

				// An incompatible member must not hide a model that another
				// member can execute. The fixed count remains usable by the API.
				require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 60).Updates(map[string]any{"status": common.ChannelStatusEnabled, "param_override": `{"n":2}`}).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 60).Update("param_override", nil).Error)
				})
				var healthy, fixedCount model.Channel
				require.NoError(t, db.First(&healthy, 1).Error)
				require.NoError(t, db.First(&fixedCount, 60).Error)
				probe := service.AsyncImageRequest{Platform: "openai", Dialect: "async", Model: publicModel, Kind: "text_to_image", Count: 1, Resolution: "1K", Size: "1024x1024"}
				require.True(t, service.MixedImageChannelRequestSupported(healthy, probe))
				require.False(t, service.MixedImageChannelRequestSupported(fixedCount, probe))
				probe.Count = 2
				require.True(t, service.MixedImageChannelRequestSupported(fixedCount, probe))
				recorder = call(http.MethodGet, "/capabilities/"+strconv.Itoa(token.Id), nil)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				foundMixed = false
				for _, capability := range response.Data.Models {
					if capability["id"] == publicModel && capability["platform"] == "mixed" {
						foundMixed = true
						assert.Equal(t, float64(1), capability["capability"].(map[string]any)["max_output_images"])
					}
				}
				assert.True(t, foundMixed, "the healthy member must remain available")

				var expandedCatalog []service.ImageModelCapability
				require.NoError(t, common.Unmarshal(catalog, &expandedCatalog))
				expandedCatalog[0].MaxOutputImages = 4
				expandedJSON, err := common.Marshal(expandedCatalog)
				require.NoError(t, err)
				require.NoError(t, db.Model(&mixedPolicy).Update("models", string(expandedJSON)).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Model(&mixedPolicy).Update("models", string(catalog)).Error)
				})
				recorder = call(http.MethodGet, "/capabilities/"+strconv.Itoa(token.Id), nil)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				var mixedCapability map[string]any
				for _, entry := range response.Data.Models {
					if entry["id"] == publicModel && entry["platform"] == "mixed" {
						mixedCapability = entry["capability"].(map[string]any)
					}
				}
				require.NotNil(t, mixedCapability)
				assert.Equal(t, float64(4), mixedCapability["max_output_images"], "a fixed-count member cannot shrink a healthy member's continuous range")

				// The existing UI starts at one, so a fixed-two-only pool must not
				// advertise a range that includes an unsupported default request.
				require.NoError(t, db.Model(&healthy).Update("status", common.ChannelStatusManuallyDisabled).Error)
				recorder = call(http.MethodGet, "/capabilities/"+strconv.Itoa(token.Id), nil)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				for _, entry := range response.Data.Models {
					assert.False(t, entry["id"] == publicModel && entry["platform"] == "mixed")
				}

				// A mapped member that only supports 2K must be probed with the
				// same dimensions as a real 2K submission, not an empty tier.
				healthy.SetSetting(dto.ChannelSettings{ImageResolutionModelMapping: map[string]map[string]string{publicModel: {"2K": "gpt-image-1"}}})
				require.NoError(t, db.Model(&healthy).Updates(map[string]any{"status": common.ChannelStatusEnabled, "setting": healthy.Setting}).Error)
				require.NoError(t, db.Model(&fixedCount).Update("status", common.ChannelStatusManuallyDisabled).Error)
				twoKBinding := service.ImagePoolBindingKey(mixedPolicy, publicModel, "2K")
				require.NoError(t, db.Model(&model.ImageChannelPool{}).Where("binding_key = ?", twoKBinding).Update("channel_id", healthy.Id).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Model(&healthy).Updates(map[string]any{"status": common.ChannelStatusEnabled, "setting": nil}).Error)
					assert.NoError(t, db.Model(&model.ImageChannelPool{}).Where("binding_key = ?", twoKBinding).Update("channel_id", 0).Error)
				})
				previousValidation := service.ValidateMixedImageChannelRequestFunc
				t.Cleanup(func() { service.ValidateMixedImageChannelRequestFunc = previousValidation })
				var probes []service.AsyncImageRequest
				service.ValidateMixedImageChannelRequestFunc = func(channel model.Channel, request service.AsyncImageRequest) error {
					probes = append(probes, request)
					return previousValidation(channel, request)
				}
				recorder = call(http.MethodGet, "/capabilities/"+strconv.Itoa(token.Id), nil)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				require.NotEmpty(t, probes)
				for _, request := range probes {
					assert.Equal(t, "2K", request.Resolution)
					assert.Equal(t, "2048x2048", request.Size)
				}
				mixedCapability = nil
				for _, entry := range response.Data.Models {
					if entry["id"] == publicModel && entry["platform"] == "mixed" {
						mixedCapability = entry["capability"].(map[string]any)
					}
				}
				require.NotNil(t, mixedCapability)
				assert.Equal(t, []any{"2K"}, mixedCapability["resolutions"])
				assert.Equal(t, float64(4), mixedCapability["max_output_images"])

				// A provider may accept one and three outputs but reject two.
				// The workbench must not advertise the hole as a continuous range.
				service.ValidateMixedImageChannelRequestFunc = func(channel model.Channel, request service.AsyncImageRequest) error {
					if request.Count == 2 {
						return errors.New("fixture provider rejects two outputs")
					}
					return previousValidation(channel, request)
				}
				probe.Count, probe.Resolution, probe.Size = 3, "2K", "2048x2048"
				require.True(t, service.MixedImageChannelRequestSupported(healthy, probe))
				recorder = call(http.MethodGet, "/capabilities/"+strconv.Itoa(token.Id), nil)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				mixedCapability = nil
				for _, entry := range response.Data.Models {
					if entry["id"] == publicModel && entry["platform"] == "mixed" {
						mixedCapability = entry["capability"].(map[string]any)
					}
				}
				require.NotNil(t, mixedCapability)
				assert.Equal(t, float64(1), mixedCapability["max_output_images"])
			})
			t.Run("policy revision and token mapping stay independent", func(t *testing.T) {
				stalePolicy := mixedPolicy
				mixedPolicy.Enabled = false
				recorder := call(http.MethodPut, "/policy", mixedPolicy)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				recorder = call(http.MethodPut, "/policy", stalePolicy)
				assert.Equal(t, http.StatusConflict, recorder.Code)
				var legacy model.ImageGroupPolicy
				require.NoError(t, db.Where("id = ?", oldPolicy.Id).Take(&legacy).Error)
				assert.Equal(t, oldPolicy, legacy)
				require.NoError(t, db.Create(&model.TokenImagePlatformMapping{TokenId: token.Id, Platform: "newapi", Group: "legacy-only"}).Error)
				path := "/token/" + strconv.Itoa(token.Id)
				recorder = call(http.MethodPut, path, gin.H{"mixed": "default"})
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				policy, _, err := service.ResolveImagePolicy(context.Background(), token, "mixed")
				require.NoError(t, err)
				assert.Equal(t, "default", policy.Group)
				policy, _, err = service.ResolveImagePolicy(context.Background(), token, "newapi")
				require.NoError(t, err)
				assert.Equal(t, "legacy-only", policy.Group)
				recorder = call(http.MethodPut, path, gin.H{"mixed": "forbidden-mixed-group"})
				assert.Equal(t, http.StatusForbidden, recorder.Code)
				recorder = call(http.MethodPut, path, gin.H{"mixed": nil})
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var mappings []model.TokenImagePlatformMapping
				require.NoError(t, db.Where("token_id = ?", token.Id).Find(&mappings).Error)
				require.Len(t, mappings, 1)
				assert.Equal(t, "newapi", mappings[0].Platform)
				var channel model.Channel
				require.NoError(t, db.Where("id = ?", 29).Take(&channel).Error)
				assert.Equal(t, 60, channel.Type)
			})
			t.Run("mixed video aliases preserve image pools and expose provider variants", func(t *testing.T) {
				for i, key := range []string{"mixed-video-first", "mixed-video-second"} {
					upstreamModel := fmt.Sprintf("vendor-video-%d", i)
					source := fmt.Sprintf(`
export const meta = {apiVersion:1,key:%q,name:"Mixed fixture",version:"1.0.0",author:{name:"Test"},models:[%q],fetchMode:"per_task",protocols:["openai_video"],videoProfiles:[{models:[%q],modes:[{name:"text_to_video",duration:{min:1,max:10,step:1,default:5},resolutions:["720p"],defaultResolution:"720p"}]}]};
export function buildSubmitRequest(){return {};}
export function parseSubmitResponse(){return {};}
export function buildQueryRequest(){return {};}
export function parseTaskResult(){return {};}
export const protocols={openai_video:{decodeRequest(ctx){return {kind:"submit",model:ctx.model,action:"text_to_video",requestBody:ctx.body.value};},render(){return {};}}};
export function listArtifacts(){return [];}
export function buildContentRequest(){return {};}
`, key, upstreamModel, upstreamModel)
					_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
					require.NoError(t, err)
					t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(key) })
					mapping, err := common.Marshal(map[string]string{publicModel: upstreamModel})
					require.NoError(t, err)
					baseURL := fmt.Sprintf("https://mixed-video-%d.example", i)
					channel := model.Channel{Id: 101 + i, Type: 61, Name: key, Key: "fixture-video-key", BaseURL: &baseURL, Status: common.ChannelStatusEnabled, Models: publicModel, Group: "default", ModelMapping: common.GetPointer(string(mapping))}
					channel.SetSetting(dto.ChannelSettings{TaskPluginKey: key})
					require.NoError(t, db.Create(&channel).Error)
					require.NoError(t, db.Create(&model.Ability{Group: "default", Model: publicModel, ChannelId: channel.Id, Enabled: true}).Error)
				}
				var policy model.ImageGroupPolicy
				require.NoError(t, db.Where("policy_key = ?", service.ImageIdentityHash("default", "mixed")).Take(&policy).Error)
				var catalog []service.ImageModelCapability
				require.NoError(t, common.UnmarshalJsonStr(policy.Models, &catalog))
				catalog = append(catalog, service.ImageModelCapability{Id: publicModel, Label: "Mixed video", MediaType: "video"})
				encoded, err := common.Marshal(catalog)
				require.NoError(t, err)
				policy.Models, policy.Enabled, policy.AsyncEnabled = string(encoded), true, true
				recorder := call(http.MethodPut, "/policy", policy)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				recorder = call(http.MethodGet, "/options?group=default&platform=mixed&media_type=video", nil)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var options struct {
					Data service.ImagePoolSelectionOptions
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &options))
				require.Len(t, options.Data.Models, 1)
				assert.Equal(t, []int{101, 102}, options.Data.Models[0].ChannelIds)
				imageKey := service.ImagePoolBindingKey(policy, publicModel, "1K")
				videoKey := service.MediaPoolBindingKey(policy, publicModel, "1K", "video")
				require.NotEqual(t, imageKey, videoKey)
				var before []model.ImageChannelPool
				require.NoError(t, db.Where("binding_key = ?", imageKey).Order("id").Find(&before).Error)
				recorder = call(http.MethodPut, "/pool", gin.H{"group": "default", "platform": "mixed", "media_type": "video", "mode": policy.PoolMode, "model": publicModel, "resolution": "1K", "channels": []gin.H{{"id": 101, "weight": 1}, {"id": 102, "weight": 1}}})
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				assert.Contains(t, recorder.Body.String(), `"media_type":"video"`)
				var after []model.ImageChannelPool
				require.NoError(t, db.Where("binding_key = ?", imageKey).Order("id").Find(&after).Error)
				assert.Equal(t, before, after)
				recorder = call(http.MethodPost, "/preview", gin.H{"group": "default", "platform": "mixed", "media_type": "video", "model": publicModel, "resolution": "720p", "seconds": 5})
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var preview struct{ Data service.MediaRoutingPreview }
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &preview))
				require.Len(t, preview.Data.Eligible, 2)
				assert.Equal(t, "vendor-video-0", preview.Data.Eligible[0].UpstreamModel)
				assert.Equal(t, "mixed-video-second", preview.Data.Eligible[1].Provider)
				assert.NotContains(t, recorder.Body.String(), "fixture-video-key")
				profiles, err := service.MixedVideoModelProfiles(t.Context(), policy, publicModel, service.VideoModelOverrides{}, true)
				require.NoError(t, err)
				require.NotEmpty(t, profiles)
				// Repeated migration leaves both namespaces and legacy data intact.
				require.NoError(t, db.AutoMigrate(entities...))
				require.NoError(t, db.AutoMigrate(entities...))
				var count int64
				require.NoError(t, db.Model(&model.ImageChannelPool{}).Where("binding_key = ?", videoKey).Count(&count).Error)
				assert.EqualValues(t, 2, count)
			})
			t.Run("mixed video defaults and prices match enabled capabilities", func(t *testing.T) {
				const source = `
export const meta={apiVersion:1,key:"mixed-facts",name:"Mixed facts",version:"1.0.0",author:{name:"Test"},models:["vendor-default","vendor-edit"],fetchMode:"per_task",protocols:["openai_video"],videoProfiles:[{models:["vendor-default"],modes:[{name:"text_to_video",duration:{min:8,max:8,step:1,default:8},resolutions:["1080p"],defaultResolution:"1080p"}]},{models:["vendor-edit"],modes:[{name:"edit_video",inputs:[{name:"video",kind:"video",sources:["url"],maxItems:1,required:true}]}]}]};
export function buildSubmitRequest(){return {};}
export function parseSubmitResponse(){return {};}
export function buildQueryRequest(){return {};}
export function parseTaskResult(){return {};}
export const protocols={openai_video:{decodeRequest(ctx){return {kind:"submit",model:ctx.model,action:"text_to_video",requestBody:ctx.body.value};},render(){return {};}}};
export function listArtifacts(){return [];}
export function buildContentRequest(){return {};}
`
				_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
				require.NoError(t, err)
				t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister("mixed-facts") })
				settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
				previousPrices, previousOptions := settings.MixedVideoBillingExpr, common.OptionMap
				settings.MixedVideoBillingExpr, common.OptionMap = make(map[string]string), make(map[string]string)
				t.Cleanup(func() { settings.MixedVideoBillingExpr, common.OptionMap = previousPrices, previousOptions })
				for i, name := range []string{"default", "edit"} {
					publicName, upstreamName := "public-"+name, "vendor-"+name
					mapping, err := common.Marshal(map[string]string{publicName: upstreamName})
					require.NoError(t, err)
					channel := model.Channel{Id: 201 + i, Type: 61, Name: name, Key: "fixture-facts-key", Status: common.ChannelStatusEnabled, Models: publicName, Group: "mixed-facts", ModelMapping: common.GetPointer(string(mapping))}
					channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "mixed-facts"})
					require.NoError(t, db.Create(&channel).Error)
					require.NoError(t, db.Create(&model.Ability{Group: "mixed-facts", Model: publicName, ChannelId: channel.Id, Enabled: true}).Error)
				}
				policy := model.ImageGroupPolicy{Group: "mixed-facts", Platform: "mixed", PoolMode: "model_resolution"}
				catalog := []service.ImageModelCapability{{Id: "public-default", MediaType: "video"}, {Id: "public-edit", MediaType: "video"}}
				encoded, err := common.Marshal(catalog)
				require.NoError(t, err)
				policy.Models, policy.Enabled, policy.AsyncEnabled = string(encoded), true, true
				recorder := call(http.MethodPut, "/policy", policy)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, db.Where("policy_key = ?", service.ImageIdentityHash(policy.Group, policy.Platform)).Take(&policy).Error)
				key := service.MediaPoolBindingKey(policy, "public-default", "2K", "video")
				require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: key, Group: policy.Group, Platform: "mixed", Mode: policy.PoolMode, Model: "public-default", Resolution: "2K", ChannelId: 201, Priority: 1}).Error)
				recorder = call(http.MethodPost, "/preview", gin.H{"group": policy.Group, "platform": "mixed", "media_type": "video", "model": "public-default"})
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var preview struct{ Data service.MediaRoutingPreview }
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &preview))
				require.Len(t, preview.Data.Eligible, 1, recorder.Body.String())
				assert.Equal(t, key, preview.Data.BindingKey)
				assert.Equal(t, "1080p", preview.Data.Resolution)
				require.NotNil(t, preview.Data.Seconds)
				assert.Equal(t, 8, *preview.Data.Seconds)
				profiles, err := service.MixedVideoModelProfiles(t.Context(), policy, "public-default", service.VideoModelOverrides{}, true)
				require.NoError(t, err)
				_, _, admitted, err := service.NormalizeMixedVideoDefaults([]byte(`{"model":"public-default","prompt":"test"}`), "application/json", "create", profiles)
				require.NoError(t, err)
				assert.Equal(t, admitted["resolution"], preview.Data.Resolution)
				assert.Equal(t, admitted["seconds"], float64(*preview.Data.Seconds))
				// A missing pool in an earlier auto group must not exclude a later valid group.
				previousUsable, previousRatios := setting.UserUsableGroups2JSONString(), ratio_setting.GroupRatio2JSONString()
				t.Cleanup(func() {
					require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousUsable))
					require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios))
				})
				require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","mixed-facts":"Mixed"}`))
				require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"mixed-facts":1}`))
				autoPolicy := model.ImageGroupPolicy{Group: "auto", Platform: "mixed", PolicyKey: service.ImageIdentityHash("auto", "mixed"), Enabled: true, AsyncEnabled: true, PoolMode: policy.PoolMode, Models: `[{"id":"public-default","media_type":"video"}]`, Version: 1}
				require.NoError(t, db.Create(&autoPolicy).Error)
				autoToken := model.Token{Id: 703, UserId: 701, Group: "auto", Key: "mixed-auto-fixture", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true, AutoGroups: `["default","mixed-facts"]`}
				require.NoError(t, db.Create(&autoToken).Error)
				require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "public-default", ChannelId: 201, Enabled: true}).Error)
				recorder = call(http.MethodPost, "/preview", gin.H{"token_id": 703, "platform": "mixed", "media_type": "video", "model": "public-default"})
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &preview))
				require.Len(t, preview.Data.Eligible, 1, recorder.Body.String())
				assert.Equal(t, "mixed-facts", preview.Data.Eligible[0].Group)
				assert.NotContains(t, preview.Data.Reasons, "mixed_pool_missing")
				// An incompatible price must leave persisted and active prices untouched.
				prices, err := common.Marshal(map[string]string{"public-edit": `tier("time", u("seconds") * 0.1)`})
				require.NoError(t, err)
				recorder = call(http.MethodPut, "/option", gin.H{"key": billing_setting.MixedVideoBillingExprOption, "value": string(prices)})
				assert.Contains(t, recorder.Body.String(), `"success":false`)
				assert.Contains(t, recorder.Body.String(), "seconds")
				assert.Empty(t, settings.MixedVideoBillingExpr)
				var priceRows int64
				require.NoError(t, db.Model(&model.Option{}).Where(map[string]any{"key": billing_setting.MixedVideoBillingExprOption}).Count(&priceRows).Error)
				assert.Zero(t, priceRows)
				policy.Enabled = false
				recorder = call(http.MethodPut, "/policy", policy)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, db.Where("policy_key = ?", policy.PolicyKey).Take(&policy).Error)
				recorder = call(http.MethodPut, "/option", gin.H{"key": billing_setting.MixedVideoBillingExprOption, "value": string(prices)})
				assert.Contains(t, recorder.Body.String(), `"success":true`)
				policy.Enabled = true
				recorder = call(http.MethodPut, "/policy", policy)
				assert.Equal(t, http.StatusBadRequest, recorder.Code)
				assert.Contains(t, recorder.Body.String(), "seconds")
				var unchanged model.ImageGroupPolicy
				require.NoError(t, db.Where("policy_key = ?", policy.PolicyKey).Take(&unchanged).Error)
				assert.False(t, unchanged.Enabled)
				assert.Equal(t, policy.Version, unchanged.Version)
				prices, err = common.Marshal(map[string]string{"public-edit": `tier("edit", u("requests") * 0.1)`, "public-default": `tier("time", u("seconds") * 0.1)`})
				require.NoError(t, err)
				recorder = call(http.MethodPut, "/option", gin.H{"key": billing_setting.MixedVideoBillingExprOption, "value": string(prices)})
				require.Contains(t, recorder.Body.String(), `"success":true`)
				recorder = call(http.MethodPut, "/policy", policy)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			})
			t.Run("inactive mixed drafts and current pooled pricing", func(t *testing.T) {
				const group, publicModel = "mixed-lifecycle", "lifecycle-video"
				const catalog = `[{"id":"lifecycle-video","media_type":"video"}]`
				policy := model.ImageGroupPolicy{Group: group, Platform: "mixed", PolicyKey: service.ImageIdentityHash(group, "mixed"), Enabled: true, AsyncEnabled: true, PoolMode: "model", Models: catalog, Version: 1}
				require.NoError(t, db.Create(&policy).Error)
				stale := policy
				policy.Enabled = false
				recorder := call(http.MethodPut, "/policy", policy)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, db.Where("policy_key = ?", policy.PolicyKey).Take(&policy).Error)
				assert.False(t, policy.Enabled, "a policy can be disabled after its last compatible channel disappears")
				stale.Enabled = false
				assert.Equal(t, http.StatusConflict, call(http.MethodPut, "/policy", stale).Code)
				policy.Enabled, policy.AsyncEnabled = true, false
				recorder = call(http.MethodPut, "/policy", policy)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, db.Where("id = ?", policy.Id).Take(&policy).Error)
				assert.False(t, policy.AsyncEnabled, "asynchronous video can also be disabled without a live channel")
				for _, override := range []string{
					`{"modes":["unknown"]}`, `{"modes":["text_to_video","text_to_video"]}`,
					`{"duration":{"min":0}}`, fmt.Sprintf(`{"duration":{"max":%d}}`, relaycommon.MaxTaskDurationSeconds+1),
					`{"duration":{"min":10,"max":5}}`, `{"duration":{"min":8,"max":8,"default":6}}`,
					`{"duration":{"values":[8,8]}}`, `{"duration":{"values":[4],"default":8}}`,
					`{"max_input_items":{"video":0}}`, `{"max_input_items":{"video":65}}`,
					`{"resolutions":["720p","720p"]}`, `{"extension_directions":["unknown"]}`,
					`{"resolutions":["1080p"],"default_resolution":"720p"}`,
				} {
					invalid := policy
					invalid.Models = fmt.Sprintf(`[{"id":%q,"media_type":"video","video_overrides":%s}]`, publicModel, override)
					assert.Equal(t, http.StatusBadRequest, call(http.MethodPut, "/policy", invalid).Code, override)
				}
				invalid := policy
				invalid.Models = "["
				assert.Equal(t, http.StatusBadRequest, call(http.MethodPut, "/policy", invalid).Code)
				var unchanged model.ImageGroupPolicy
				require.NoError(t, db.Where("id = ?", policy.Id).Take(&unchanged).Error)
				assert.Equal(t, policy, unchanged)
				policy.Models = `[{"id":"lifecycle-video","media_type":"video","video_overrides":{"modes":[],"resolutions":[],"aspect_ratios":[],"extension_directions":[],"duration":{"values":[]}}}]`
				recorder = call(http.MethodPut, "/policy", policy)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, db.Where("id = ?", policy.Id).Take(&policy).Error)
				var draft []service.ImageModelCapability
				require.NoError(t, common.UnmarshalJsonStr(policy.Models, &draft))
				require.Len(t, draft, 1)
				assert.NotNil(t, draft[0].VideoOverrides.Modes)
				assert.NotNil(t, draft[0].VideoOverrides.Resolutions)
				assert.NotNil(t, draft[0].VideoOverrides.AspectRatios)
				assert.NotNil(t, draft[0].VideoOverrides.ExtensionDirections)
				require.NotNil(t, draft[0].VideoOverrides.Duration)
				assert.NotNil(t, draft[0].VideoOverrides.Duration.Values)

				const source = `
export const meta={apiVersion:1,key:"mixed-lifecycle",name:"Mixed lifecycle",version:"1.0.0",author:{name:"Test"},models:["vendor-timed","vendor-edit"],fetchMode:"per_task",protocols:["openai_video"],videoProfiles:[{models:["vendor-timed"],modes:[{name:"text_to_video",duration:{values:[8],default:8},resolutions:["1080p"],defaultResolution:"1080p"}]},{models:["vendor-edit"],modes:[{name:"edit_video",inputs:[{name:"video",kind:"video",sources:["url"],maxItems:1,required:true}]}]}]};
export function buildSubmitRequest(){return {};}
export function parseSubmitResponse(){return {};}
export function buildQueryRequest(){return {};}
export function parseTaskResult(){return {};}
export const protocols={openai_video:{decodeRequest(ctx){return {kind:"submit",model:ctx.model,action:ctx.operation==="edit"?"edit_video":"text_to_video",requestBody:ctx.body.value};},render(){return {};}}};
export function listArtifacts(){return [];}
export function buildContentRequest(){return {};}
`
				_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
				require.NoError(t, err)
				t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister("mixed-lifecycle") })
				mapping := `{"lifecycle-video":"vendor-timed"}`
				channel := model.Channel{Id: 301, Type: 61, Name: "Mixed lifecycle", Key: "fixture-lifecycle-key", Status: common.ChannelStatusEnabled, Models: publicModel, Group: group, ModelMapping: &mapping}
				channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "mixed-lifecycle"})
				require.NoError(t, db.Create(&channel).Error)
				require.NoError(t, db.Create(&model.Ability{Group: group, Model: publicModel, ChannelId: channel.Id, Enabled: true}).Error)
				policy.Enabled, policy.AsyncEnabled = true, true
				assert.Equal(t, http.StatusBadRequest, call(http.MethodPut, "/policy", policy).Code, "empty allowlists must not become unrestricted on save")
				policy.Models = catalog
				recorder = call(http.MethodPut, "/policy", policy)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, db.Where("id = ?", policy.Id).Take(&policy).Error)
				key := service.MediaPoolBindingKey(policy, publicModel, "", "video")
				require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: key, Group: group, Platform: "mixed", Mode: "model", Model: publicModel, ChannelId: channel.Id, Priority: 1}).Error)

				previousUsable, previousRatios := setting.UserUsableGroups2JSONString(), ratio_setting.GroupRatio2JSONString()
				require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","mixed-lifecycle":"Lifecycle"}`))
				require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"mixed-lifecycle":1}`))
				t.Cleanup(func() {
					require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousUsable))
					require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios))
				})
				capabilityToken := model.Token{Id: 740, UserId: 701, Group: group, Key: "mixed-lifecycle-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
				require.NoError(t, db.Create(&capabilityToken).Error)
				redisServer := miniredis.RunT(t)
				previousRDB, previousRedis, previousSecret, previousPlugins := common.RDB, common.RedisEnabled, common.CryptoSecret, constant.TaskPluginEnabled
				common.RDB = redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
				common.RedisEnabled, common.CryptoSecret, constant.TaskPluginEnabled = true, "fixture-media-signing-key", true
				t.Cleanup(func() {
					assert.NoError(t, common.RDB.Close())
					common.RDB, common.RedisEnabled, common.CryptoSecret, constant.TaskPluginEnabled = previousRDB, previousRedis, previousSecret, previousPlugins
				})
				t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "lifecycle:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
				t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "lifecycle")
				cfg := service.DefaultMediaRuntimeConfig()
				cfg.VideoAsyncEnabled = true
				require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
				readiness, err := service.GetVideoReadiness(t.Context())
				require.NoError(t, err)
				require.True(t, readiness.Ready, "%+v", readiness.Checks)
				settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
				previousPrices, previousOptions := settings.MixedVideoBillingExpr, common.OptionMap
				settings.MixedVideoBillingExpr, common.OptionMap = make(map[string]string), make(map[string]string)
				t.Cleanup(func() { settings.MixedVideoBillingExpr, common.OptionMap = previousPrices, previousOptions })
				setPrice := func(expression string) {
					expressions := make(map[string]string)
					if expression != "" {
						expressions[publicModel] = expression
					}
					encoded, err := common.Marshal(expressions)
					require.NoError(t, err)
					recorder := call(http.MethodPut, "/option", gin.H{"key": billing_setting.MixedVideoBillingExprOption, "value": string(encoded)})
					require.Contains(t, recorder.Body.String(), `"success":true`)
				}
				assertPricing := func(status string, available bool, reason string) {
					recorder := call(http.MethodGet, "/capabilities/740", nil)
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					var response struct {
						Data struct {
							Models []struct {
								Id              string `json:"id"`
								RoutingPlatform string `json:"routing_platform"`
								PricingStatus   string `json:"pricing_status"`
								Available       bool   `json:"available"`
								Reason          string `json:"availability_reason"`
							} `json:"video_models"`
						}
					}
					require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
					for _, entry := range response.Data.Models {
						if entry.Id == publicModel && entry.RoutingPlatform == "mixed" {
							assert.Equal(t, status, entry.PricingStatus)
							assert.Equal(t, available, entry.Available)
							assert.Equal(t, reason, entry.Reason)
							return
						}
					}
					t.Fatal("mixed lifecycle capability is missing")
				}
				setPrice(`tier("time", u("seconds") * 0.1)`)
				assertPricing("configured", true, "")
				require.NoError(t, db.Model(&channel).Update("model_mapping", `{"lifecycle-video":"vendor-edit"}`).Error)
				assertPricing("invalid_configuration", false, "Pricing configuration is invalid")
				setPrice(`tier("request", u("requests") * 0.1)`)
				assertPricing("configured", true, "")
				setPrice("")
				assertPricing("needs_configuration", true, "")
				setPrice(`tier("request", u("requests") * 0.1)`)
				require.NoError(t, db.Where("binding_key = ?", key).Delete(&model.ImageChannelPool{}).Error)
				assertPricing("invalid_configuration", false, "No executable mixed video channel is available")
			})
			t.Run("deleted policy drafts cannot recreate or replace a new policy", func(t *testing.T) {
				previousRegistry := jsplugin.DefaultRegistry
				jsplugin.DefaultRegistry = jsplugin.NewRegistry()
				t.Cleanup(func() { jsplugin.DefaultRegistry = previousRegistry })
				source, err := builtinplugins.Source("xai")
				require.NoError(t, err)
				_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "xai"})
				require.NoError(t, err)
				const group, publicModel = "mixed-delete-cas", "delete-cas-video"
				mapping := `{"delete-cas-video":"grok-imagine-video-1.5"}`
				channel := model.Channel{Id: 401, Type: 61, Name: "Policy deletion CAS", Key: "fixture-delete-key", Status: common.ChannelStatusEnabled, Models: publicModel, Group: group, ModelMapping: &mapping}
				channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "xai"})
				require.NoError(t, db.Create(&channel).Error)
				require.NoError(t, db.Create(&model.Ability{Group: group, Model: publicModel, ChannelId: channel.Id, Enabled: true}).Error)
				policy := model.ImageGroupPolicy{Group: group, Platform: "mixed", Enabled: true, AsyncEnabled: true, PoolMode: "model", Models: `[{"id":"delete-cas-image","media_type":"image","max_output_images":1},{"id":"delete-cas-video","media_type":"video"}]`}
				recorder := call(http.MethodPut, "/policy", policy)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, db.Where("policy_key = ?", service.ImageIdentityHash(group, "mixed")).Take(&policy).Error)
				versionOne := policy
				policy.Models = strings.Replace(policy.Models, "delete-cas-image", "delete-cas-image-v2", 1)
				recorder = call(http.MethodPut, "/policy", policy)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, db.Where("id = ?", policy.Id).Take(&policy).Error)
				require.Equal(t, int64(2), policy.Version)
				recorder = call(http.MethodDelete, "/policy", gin.H{"id": policy.Id, "group": group, "platform": "mixed", "version": policy.Version})
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				recorder = call(http.MethodPut, "/policy", policy)
				assert.Equal(t, http.StatusConflict, recorder.Code, "a deleted nonzero revision is an update, never a create")
				var missing model.ImageGroupPolicy
				require.ErrorIs(t, db.Where("policy_key = ?", policy.PolicyKey).Take(&missing).Error, gorm.ErrRecordNotFound)
				legacyStale := versionOne
				legacyStale.Id = 0
				assert.Equal(t, http.StatusConflict, call(http.MethodPut, "/policy", legacyStale).Code)
				invalidCreate := policy
				invalidCreate.Version = 0
				assert.Equal(t, http.StatusConflict, call(http.MethodPut, "/policy", invalidCreate).Code)

				fresh := versionOne
				fresh.Id, fresh.Version = 0, 0
				fresh.Models = strings.Replace(fresh.Models, "delete-cas-image", "new-policy-image", 1)
				recorder = call(http.MethodPut, "/policy", fresh)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, db.Where("policy_key = ?", policy.PolicyKey).Take(&fresh).Error)
				require.NotEqual(t, policy.Id, fresh.Id)
				require.Equal(t, versionOne.Version, fresh.Version, "deletion and recreation can reuse the numeric revision")
				assert.Equal(t, http.StatusConflict, call(http.MethodPut, "/policy", versionOne).Code, "the old row ID fences a stale same-version draft")
				assert.Equal(t, http.StatusConflict, call(http.MethodDelete, "/policy", gin.H{"id": versionOne.Id, "group": group, "platform": "mixed", "version": versionOne.Version}).Code)
				var unchanged model.ImageGroupPolicy
				require.NoError(t, db.Where("id = ?", fresh.Id).Take(&unchanged).Error)
				assert.Equal(t, fresh, unchanged)

				// Older callers may omit the row ID for a matching-version update.
				legacyUpdate := fresh
				legacyUpdate.Id = 0
				legacyUpdate.AsyncEnabled = false
				recorder = call(http.MethodPut, "/policy", legacyUpdate)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.NoError(t, db.Where("id = ?", fresh.Id).Take(&fresh).Error)
				assert.False(t, fresh.AsyncEnabled)
				assert.Equal(t, int64(2), fresh.Version)
				recorder = call(http.MethodDelete, "/policy", gin.H{"group": group, "platform": "mixed", "version": fresh.Version})
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			})
		})
	}
}
