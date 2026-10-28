package controller

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay/channel/volcengine"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting"
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

func TestTabtabWorkbenchCapabilitiesDatabaseMatrix(t *testing.T) {
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var dialector gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "tabtab-capabilities.db"))
			if engine == "mysql" {
				dsn := os.Getenv("IMAGE_CONFIG_TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("IMAGE_CONFIG_TEST_MYSQL_DSN is not configured")
				}
				require.Contains(t, dsn, "/new_api_image_config_test")
				dialector = mysql.Open(dsn)
			} else if engine == "postgres" {
				dsn := os.Getenv("IMAGE_CONFIG_TEST_PG_DSN")
				if dsn == "" {
					t.Skip("IMAGE_CONFIG_TEST_PG_DSN is not configured")
				}
				require.True(t, strings.Contains(dsn, "dbname=new_api_image_config_test") || strings.Contains(dsn, "/new_api_image_config_test"))
				dialector = postgres.Open(dsn)
			}
			db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			previousDB, previousType, previousRedis, previousRedisEnabled := model.DB, common.MainDatabaseType(), common.RDB, common.RedisEnabled
			model.DB = db
			common.SetMainDatabaseType(common.DatabaseType(engine))
			redisServer := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			common.RDB, common.RedisEnabled = redisClient, true
			entities := []any{&model.Channel{}, &model.Ability{}, &model.ImageGroupPolicy{}, &model.ImageChannelPool{}, &model.TokenImagePlatformMapping{}, &model.Option{}, &model.User{}, &model.Token{}, &model.ImageStorageProfile{}}
			t.Cleanup(func() {
				model.DB, common.RDB, common.RedisEnabled = previousDB, previousRedis, previousRedisEnabled
				common.SetMainDatabaseType(previousType)
				assert.NoError(t, redisClient.Close())
				assert.NoError(t, db.Migrator().DropTable(entities...))
				assert.NoError(t, connection.Close())
			})
			t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "tabtab-cap:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32)))
			t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "tabtab-cap")
			require.NoError(t, db.AutoMigrate(entities...))
			cfg := service.DefaultImageRuntimeConfig()
			cfg.AsyncEnabled = true
			require.NoError(t, service.SaveImageRuntimeConfig(t.Context(), cfg))
			require.NoError(t, db.Create(&model.ImageStorageProfile{ProfileId: "tabtab-storage", Class: "temporary", Backend: "local", Provider: "local", Root: t.TempDir(), Active: true}).Error)
			user := model.User{Username: "tabtab-cap-user", Group: "default", Status: common.UserStatusEnabled, Quota: 1000000}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: "tabtab-cap-token", Group: "default", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
			require.NoError(t, db.Create(&token).Error)
			channel := model.Channel{Type: constant.ChannelTypeTabTab, Name: "TabTab capability fixture", Key: "sk_private-capability-key", Models: "public-gpt,fixed,lite", Group: "default", Status: common.ChannelStatusEnabled, ModelMapping: common.GetPointer(`{"public-gpt":"redirect","redirect":"gpt-image-2","fixed":"mj","lite":"gemini-3.1-flash-lite-image"}`)}
			require.NoError(t, db.Create(&channel).Error)
			for _, name := range []string{"public-gpt", "fixed", "lite"} {
				require.NoError(t, db.Create(&model.Ability{Group: "default", Model: name, ChannelId: channel.Id, Enabled: true}).Error)
			}
			catalog, err := common.Marshal([]service.ImageModelCapability{
				{Id: "public-gpt", MaxOutputImages: 8, MaxReferenceImages: 4, Resolutions: []string{"1K", "2K", "4K"}, Qualities: []string{"high", "xhigh"}, Formats: []string{"png", "gif"}, Backgrounds: []string{"opaque", "invalid"}},
				{Id: "fixed", MaxOutputImages: 8, MaxReferenceImages: 4, Resolutions: []string{"1K", "2K"}},
				{Id: "lite", MaxOutputImages: 8, MaxReferenceImages: 4, Resolutions: []string{"1K", "2K"}},
			})
			require.NoError(t, err)
			policy := model.ImageGroupPolicy{PolicyKey: service.ImageIdentityHash("default", "tabtab"), Group: "default", Platform: "tabtab", PoolMode: "model_resolution", Models: string(catalog), Enabled: true, AsyncEnabled: true}
			require.NoError(t, db.Create(&policy).Error)
			fixedBinding := model.ImageChannelPool{BindingKey: service.ImagePoolBindingKey(policy, "fixed", "AUTO"), Group: "default", Platform: "tabtab", Mode: "model_resolution", Model: "fixed", Resolution: "AUTO", ChannelId: channel.Id}
			liteBinding := model.ImageChannelPool{BindingKey: service.ImagePoolBindingKey(policy, "lite", "AUTO"), Group: "default", Platform: "tabtab", Mode: "model_resolution", Model: "lite", Resolution: "AUTO", ChannelId: channel.Id}
			require.NoError(t, db.Create(&fixedBinding).Error)
			require.NoError(t, db.Create(&liteBinding).Error)
			mixedCatalog, err := common.Marshal([]service.ImageModelCapability{{Id: "fixed", MaxOutputImages: 8, MaxReferenceImages: 4, Resolutions: []string{"1K", "2K"}}})
			require.NoError(t, err)
			mixedPolicy := model.ImageGroupPolicy{PolicyKey: service.ImageIdentityHash("default", "mixed"), Group: "default", Platform: "mixed", PoolMode: "model_resolution", Models: string(mixedCatalog), Enabled: true, AsyncEnabled: true}
			require.NoError(t, db.Create(&mixedPolicy).Error)
			require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: service.ImagePoolBindingKey(mixedPolicy, "fixed", "AUTO"), Group: "default", Platform: "mixed", Mode: "model_resolution", Model: "fixed", Resolution: "AUTO", ChannelId: channel.Id}).Error)
			previousPrices := ratio_setting.ModelPrice2JSONString()
			t.Cleanup(func() { assert.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrices)) })
			prices := ratio_setting.GetModelPriceCopy()
			prices["fixed"] = 0.01
			encodedPrices, err := common.Marshal(prices)
			require.NoError(t, err)
			require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(encodedPrices)))
			for _, closed := range []bool{false, true} {
				t.Run(fmt.Sprintf("closed_AUTO_%t", closed), func(t *testing.T) {
					if closed {
						require.NoError(t, db.Model(&fixedBinding).Update("channel_id", 0).Error)
					}
					router := gin.New()
					router.Use(func(c *gin.Context) { c.Set("id", user.Id) })
					router.GET("/capabilities/:token_id", GetImageWorkbenchCapabilities)
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/capabilities/"+strconv.Itoa(token.Id), nil))
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					assert.NotContains(t, recorder.Body.String(), "sk_private-capability-key")
					var response struct {
						Data struct {
							Models []struct {
								ID         string                       `json:"id"`
								Platform   string                       `json:"platform"`
								Available  bool                         `json:"available"`
								Parameters []string                     `json:"supported_parameters"`
								Capability service.ImageModelCapability `json:"capability"`
							} `json:"models"`
						} `json:"data"`
					}
					require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
					seen := make(map[string]bool)
					mixedFixed := false
					for _, entry := range response.Data.Models {
						if entry.Platform == "mixed" && entry.ID == "fixed" {
							mixedFixed = true
							assert.True(t, entry.Available)
							assert.Equal(t, []string{"AUTO"}, entry.Capability.Resolutions)
							assert.NotContains(t, entry.Parameters, "size")
							assert.NotContains(t, entry.Parameters, "aspect_ratio")
						}
						if entry.Platform != "tabtab" {
							continue
						}
						seen[entry.ID] = true
						assert.True(t, entry.Available)
						assert.Equal(t, 1, entry.Capability.MaxOutputImages)
						if entry.ID == "public-gpt" {
							assert.Equal(t, []string{"1K", "2K"}, entry.Capability.Resolutions)
							assert.Equal(t, []string{"high"}, entry.Capability.Qualities)
							assert.Equal(t, []string{"png"}, entry.Capability.Formats)
							assert.Equal(t, []string{"opaque"}, entry.Capability.Backgrounds)
						} else {
							assert.Equal(t, []string{"AUTO"}, entry.Capability.Resolutions)
							assert.NotContains(t, entry.Parameters, "size")
							assert.NotContains(t, entry.Parameters, "resolution")
							if entry.ID == "lite" {
								assert.Contains(t, entry.Parameters, "aspect_ratio")
							} else {
								assert.NotContains(t, entry.Parameters, "aspect_ratio")
							}
						}
					}
					assert.True(t, seen["public-gpt"], "chained aliases must resolve through discovery and admission")
					assert.True(t, seen["lite"])
					assert.Equal(t, !closed, seen["fixed"], "fixed variants must honor the actual AUTO pool")
					assert.True(t, mixedFixed, "mixed AUTO bindings must be discovered even without an AUTO catalog entry")
				})
			}
			request, err := service.ParseAsyncImageRequest([]byte(`{"model":"fixed","prompt":"draw","resolution":"AUTO","n":1}`), "application/json", "/v1/images/generations_async", cfg)
			require.NoError(t, err)
			require.NoError(t, service.PrepareMixedImageAdmission(t.Context(), token, &request))
			require.NotNil(t, request.MixedRouting)
			chosen, _, err := service.PickImageChannel(t.Context(), mixedPolicy, request, service.ImageAccountRouting{})
			require.NoError(t, err)
			assert.Equal(t, channel.Id, chosen.Id)
			named := service.AsyncImageRequest{Platform: "openai", Provider: "tabtab", Dialect: "async", Model: "public-gpt", Prompt: "draw", Count: 2}
			_, _, err = service.PickImageChannel(t.Context(), policy, named, service.ImageAccountRouting{})
			require.Error(t, err, "provider count validation must run before account selection")
			named.Count, named.Resolution = 1, "4K"
			_, _, err = service.PickImageChannel(t.Context(), policy, named, service.ImageAccountRouting{})
			require.Error(t, err, "unsupported provider dimensions must run before account selection")
		})
	}
}

func TestImageChannelPoolOptionsAndValidationDatabaseMatrix(t *testing.T) {
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
				require.Contains(t, dsn, "/new_api_image_config_test", "Use the dedicated disposable image configuration database")
				dialector = mysql.Open(dsn)
				versionQuery = "SELECT VERSION()"
			case "postgres":
				dsn := os.Getenv("IMAGE_CONFIG_TEST_PG_DSN")
				if dsn == "" {
					t.Skip("IMAGE_CONFIG_TEST_PG_DSN is not configured")
				}
				require.True(t, strings.Contains(dsn, "dbname=new_api_image_config_test") || strings.Contains(dsn, "/new_api_image_config_test"), "Use the dedicated disposable image configuration database")
				dialector = postgres.Open(dsn)
				versionQuery = "SELECT VERSION()"
			default:
				dialector = sqlite.Open(filepath.Join(t.TempDir(), "image-config.db"))
			}

			db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			if engine == "sqlite" {
				connection, err := db.DB()
				require.NoError(t, err)
				connection.SetMaxOpenConns(1)
			}
			previousDB := model.DB
			previousType := common.MainDatabaseType()
			previousRedis, previousRedisEnabled := common.RDB, common.RedisEnabled
			previousCapability := service.ImageChannelCapabilityFunc
			model.DB = db
			common.SetMainDatabaseType(common.DatabaseType(engine))
			redisServer := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			common.RDB, common.RedisEnabled = redisClient, true
			service.ImageChannelCapabilityFunc = func(channel model.Channel, _ string) dto.ImageCapability {
				switch channel.Type {
				case 1:
					return dto.ImageCapability{Provider: "openai", Protocol: "openai_images", Generate: true}
				case 2:
					return dto.ImageCapability{Provider: "gemini", Protocol: "gemini_native", Generate: true}
				default:
					return dto.ImageCapability{}
				}
			}
			entities := []any{&model.Channel{}, &model.Ability{}, &model.ImageGroupPolicy{}, &model.ImageChannelPool{}, &model.TokenImagePlatformMapping{}, &model.Option{}, &model.User{}, &model.Token{}}
			t.Cleanup(func() {
				model.DB = previousDB
				common.SetMainDatabaseType(previousType)
				common.RDB, common.RedisEnabled = previousRedis, previousRedisEnabled
				assert.NoError(t, redisClient.Close())
				service.ImageChannelCapabilityFunc = previousCapability
				assert.NoError(t, db.Migrator().DropTable(entities...))
				connection, closeErr := db.DB()
				if assert.NoError(t, closeErr) {
					assert.NoError(t, connection.Close())
				}
			})
			require.NoError(t, db.AutoMigrate(entities...))
			require.NoError(t, db.AutoMigrate(entities...))
			var version string
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("Database version: %s", version)

			channels := []model.Channel{
				{Id: 11, Type: 1, Name: "OpenAI primary", Key: "openai-secret", Status: common.ChannelStatusEnabled, Models: "image-alpha,hidden-model", Group: "default"},
				{Id: 12, Type: 2, Name: "Gemini account", Key: "gemini-secret", Status: common.ChannelStatusEnabled, Models: "image-alpha", Group: "default"},
				{Id: 13, Type: 1, Name: "Disabled OpenAI", Key: "disabled-secret", Status: common.ChannelStatusManuallyDisabled, Models: "image-alpha", Group: "default"},
			}
			require.NoError(t, db.Create(&channels).Error)
			require.NoError(t, db.Create(&[]model.Ability{
				{Group: "default", Model: "image-alpha", ChannelId: 11, Enabled: true},
				{Group: "default", Model: "image-alpha", ChannelId: 12, Enabled: true},
				{Group: "default", Model: "image-alpha", ChannelId: 13, Enabled: true},
				{Group: "default", Model: "hidden-model", ChannelId: 11, Enabled: true},
			}).Error)
			catalog, err := common.Marshal([]service.ImageModelCapability{{Id: "image-alpha", Label: "Image Alpha", MaxOutputImages: 1}})
			require.NoError(t, err)
			require.NoError(t, db.Create(&model.ImageGroupPolicy{
				PolicyKey:    service.ImageIdentityHash("default", "openai"),
				Group:        "default",
				Platform:     "openai",
				PoolMode:     "model",
				Models:       string(catalog),
				Enabled:      true,
				AsyncEnabled: true,
				Version:      1,
			}).Error)

			optionsRecorder := httptest.NewRecorder()
			optionsContext, _ := gin.CreateTestContext(optionsRecorder)
			optionsContext.Request = httptest.NewRequest(http.MethodGet, "/?group=default&platform=openai", nil)
			GetImageChannelPoolOptions(optionsContext)
			require.Equal(t, http.StatusOK, optionsRecorder.Code, optionsRecorder.Body.String())
			var optionsResponse struct {
				Data service.ImagePoolSelectionOptions `json:"data"`
			}
			require.NoError(t, common.Unmarshal(optionsRecorder.Body.Bytes(), &optionsResponse))
			require.Equal(t, []service.ImagePoolModelOption{{Id: "image-alpha", Label: "Image Alpha", MediaTypes: []string{"image"}, ChannelIds: []int{11}}}, optionsResponse.Data.Models)
			assert.Equal(t, []service.ImagePoolChannelOption{{Id: 11, Name: "OpenAI primary"}}, optionsResponse.Data.Channels)
			for _, secret := range []string{"openai-secret", "gemini-secret", "disabled-secret"} {
				assert.NotContains(t, optionsRecorder.Body.String(), secret)
			}

			validBody, err := common.Marshal(gin.H{
				"group": "default", "platform": "openai", "mode": "model", "model": "image-alpha",
				"channels": []gin.H{{"id": 11, "priority": 7}},
			})
			require.NoError(t, err)
			validRecorder := httptest.NewRecorder()
			validContext, _ := gin.CreateTestContext(validRecorder)
			validContext.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(validBody))
			SaveImageChannelPool(validContext)
			require.Equal(t, http.StatusOK, validRecorder.Code, validRecorder.Body.String())
			var saved model.ImageChannelPool
			require.NoError(t, db.Where("channel_id = ?", 11).Take(&saved).Error)
			assert.Equal(t, 7, saved.Priority)
			t.Run("routing preview", func(t *testing.T) { verifyMediaRoutingPreview(t, db, redisServer) })

			t.Run("pool compare and swap covers concurrent creation and editing", func(t *testing.T) {
				const group = "pool-without-policy"
				require.NoError(t, db.Create(&model.Ability{Group: group, Model: "image-alpha", ChannelId: 11, Enabled: true}).Error)
				key := service.ImagePoolBindingKey(model.ImageGroupPolicy{Group: group, Platform: "openai", PoolMode: "resolution"}, "", "4K")
				t.Cleanup(func() {
					assert.NoError(t, db.Where("binding_key = ?", key).Delete(&model.ImageChannelPool{}).Error)
					assert.NoError(t, db.Where(map[string]any{"group": group}).Delete(&model.Ability{}).Error)
				})
				for _, phase := range []string{"create", "edit"} {
					var expectedIDs []int
					require.NoError(t, db.Model(&model.ImageChannelPool{}).Where("binding_key = ?", key).Order("id").Pluck("id", &expectedIDs).Error)
					responses := make([]*httptest.ResponseRecorder, 2)
					start := make(chan struct{})
					var workers sync.WaitGroup
					for index := range 2 {
						body, err := common.Marshal(gin.H{"group": group, "platform": "openai", "mode": "resolution", "resolution": "4K", "expected_ids": expectedIDs, "channels": []gin.H{{"id": 11, "priority": index + 1}}})
						require.NoError(t, err)
						workers.Go(func() {
							<-start
							recorder := httptest.NewRecorder()
							context, _ := gin.CreateTestContext(recorder)
							context.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
							SaveImageChannelPool(context)
							responses[index] = recorder
						})
					}
					close(start)
					workers.Wait()
					require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, []int{responses[0].Code, responses[1].Code}, "%s: %s / %s", phase, responses[0].Body.String(), responses[1].Body.String())
					var rows []model.ImageChannelPool
					require.NoError(t, db.Where("binding_key = ?", key).Find(&rows).Error)
					require.Len(t, rows, 1, "only the winning edit is stored, with no guard row")
					assert.Equal(t, 11, rows[0].ChannelId)
					assert.NotContains(t, expectedIDs, rows[0].Id, "a replacement gets a fresh revision")
				}
				var before []model.ImageChannelPool
				require.NoError(t, db.Where("binding_key = ?", key).Find(&before).Error)
				require.Len(t, before, 1)
				body, err := common.Marshal(gin.H{"group": group, "platform": "openai", "mode": "resolution", "resolution": "4K", "expected_ids": []int{before[0].Id}, "channels": []gin.H{}})
				require.NoError(t, err)
				const callback = "test:pool-replacement-failure"
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
					if rows, ok := tx.Statement.Dest.(*[]model.ImageChannelPool); ok && len(*rows) > 0 {
						tx.AddError(errors.New("injected pool write failure"))
					}
				}))
				recorder := httptest.NewRecorder()
				context, _ := gin.CreateTestContext(recorder)
				context.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
				SaveImageChannelPool(context)
				require.NoError(t, db.Callback().Create().Remove(callback))
				assert.Equal(t, http.StatusBadRequest, recorder.Code)
				var after []model.ImageChannelPool
				require.NoError(t, db.Where("binding_key = ?", key).Find(&after).Error)
				assert.Equal(t, before, after, "failed replacement rolls back all rows, including its guard")
				recorder = httptest.NewRecorder()
				context, _ = gin.CreateTestContext(recorder)
				context.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
				SaveImageChannelPool(context)
				after = nil
				require.NoError(t, db.Where("binding_key = ?", key).Find(&after).Error)
				require.Len(t, after, 1)
				assert.Zero(t, after[0].ChannelId, "closed bindings retain a real revision")
				body, err = common.Marshal(gin.H{"group": group, "platform": "openai", "mode": "resolution", "resolution": "4K", "expected_ids": []int{after[0].Id}, "channels": []gin.H{{"id": 11, "priority": 3}}})
				require.NoError(t, err)
				recorder = httptest.NewRecorder()
				context, _ = gin.CreateTestContext(recorder)
				context.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
				SaveImageChannelPool(context)
				assert.Equal(t, http.StatusOK, recorder.Code)
			})

			invalidBody, err := common.Marshal(gin.H{
				"group": "default", "platform": "openai", "mode": "model", "model": "image-alpha",
				"channels": []gin.H{{"id": 12, "priority": 9}},
			})
			require.NoError(t, err)
			invalidRecorder := httptest.NewRecorder()
			invalidContext, _ := gin.CreateTestContext(invalidRecorder)
			invalidContext.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(invalidBody))
			SaveImageChannelPool(invalidContext)
			assert.Equal(t, http.StatusBadRequest, invalidRecorder.Code)
			assert.Contains(t, invalidRecorder.Body.String(), "not enabled for the selected group, platform, and model")
			require.NoError(t, db.Where("channel_id = ?", 11).Take(&saved).Error)
			assert.Equal(t, 7, saved.Priority, "an invalid edit must not replace the saved pool")

			for _, weight := range []int{-1, 1000001} {
				body, err := common.Marshal(gin.H{
					"group": "default", "platform": "openai", "mode": "model", "model": "image-alpha",
					"channels": []gin.H{{"id": 11, "priority": weight}},
				})
				require.NoError(t, err)
				recorder := httptest.NewRecorder()
				context, _ := gin.CreateTestContext(recorder)
				context.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
				SaveImageChannelPool(context)
				assert.Equal(t, http.StatusBadRequest, recorder.Code, "invalid weight %d", weight)
				require.NoError(t, db.Where("channel_id = ?", 11).Take(&saved).Error)
				assert.Equal(t, 7, saved.Priority, "invalid weights retain the saved binding")
			}

			t.Run("pool weights override global routing and preserve zero fallback", func(t *testing.T) {
				globalPriority, globalWeight := int64(100), uint(999)
				require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 11).Updates(map[string]any{"priority": globalPriority, "weight": globalWeight}).Error)
				require.NoError(t, db.Create(&[]model.Channel{
					{Id: 21, Type: 1, Name: "Weighted second", Key: "second-key", Status: common.ChannelStatusEnabled, Models: "image-alpha", Group: "default"},
					{Id: 22, Type: 1, Name: "Weighted third", Key: "third-key", Status: common.ChannelStatusEnabled, Models: "image-alpha", Group: "default"},
				}).Error)
				require.NoError(t, db.Create(&[]model.Ability{
					{Group: "default", Model: "image-alpha", ChannelId: 21, Enabled: true},
					{Group: "default", Model: "image-alpha", ChannelId: 22, Enabled: true},
				}).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Where("binding_key = ?", saved.BindingKey).Delete(&model.ImageChannelPool{}).Error)
					assert.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: saved.BindingKey, Group: "default", Platform: "openai", Mode: "model", Model: "image-alpha", ChannelId: 11, Priority: 7}).Error)
					assert.NoError(t, db.Where("channel_id IN ?", []int{21, 22}).Delete(&model.Ability{}).Error)
					assert.NoError(t, db.Where("id IN ?", []int{21, 22}).Delete(&model.Channel{}).Error)
					assert.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 11).Updates(map[string]any{"priority": 0, "weight": 0}).Error)
				})
				policy := model.ImageGroupPolicy{Group: "default", Platform: "openai", PoolMode: "model"}
				for _, tc := range []struct {
					name     string
					weights  []int
					selected []int
				}{
					{name: "10:9:8", weights: []int{10, 9, 8}, selected: []int{11, 11, 21, 21, 22, 22}},
					{name: "all zero", weights: []int{0, 0, 0}, selected: []int{11, 21, 21, 22, 22, 22}},
					{name: "positive excludes zero", weights: []int{0, 9, 0}, selected: []int{21, 21, 21, 21, 21, 21}},
				} {
					t.Run(tc.name, func(t *testing.T) {
						var expectedIDs []int
						require.NoError(t, db.Model(&model.ImageChannelPool{}).Where("binding_key = ?", saved.BindingKey).Order("id").Pluck("id", &expectedIDs).Error)
						body, err := common.Marshal(gin.H{
							"group": "default", "platform": "openai", "mode": "model", "model": "image-alpha",
							"expected_ids": expectedIDs,
							"channels":     []gin.H{{"id": 11, "priority": tc.weights[0]}, {"id": 21, "priority": tc.weights[1]}, {"id": 22, "priority": tc.weights[2]}},
						})
						require.NoError(t, err)
						recorder := httptest.NewRecorder()
						context, _ := gin.CreateTestContext(recorder)
						context.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
						SaveImageChannelPool(context)
						require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
						candidates, err := service.ImageCandidateChannels(t.Context(), policy, "image-alpha", "")
						require.NoError(t, err)
						require.Len(t, candidates, 3)
						slices.SortFunc(candidates, func(a, b model.Channel) int { return cmp.Compare(a.Id, b.Id) })
						weights := make([]int, len(candidates))
						for i := range candidates {
							assert.Zero(t, candidates[i].GetPriority(), "the pool overrides global priorities")
							weights[i] = candidates[i].GetWeight()
						}
						assert.Equal(t, tc.weights, weights)
						for i, position := range []float64{0, 0.37, 0.5, 0.70, 0.71, 0.99} {
							index := service.PickMediaChannelByWeight(weights, position)
							require.GreaterOrEqual(t, index, 0)
							assert.Equal(t, tc.selected[i], candidates[index].Id, "position %g", position)
						}
					})
				}
				request := service.AsyncImageRequest{Model: "image-alpha", Platform: "openai", Provider: "openai", Count: 1, Kind: "text_to_image"}
				selected, _, err := service.PickImageChannel(t.Context(), policy, request, service.ImageAccountRouting{})
				require.NoError(t, err)
				assert.Equal(t, 21, selected.Id, "zero pool weight ignores even a higher global priority and weight")
				videoCatalog, err := common.Marshal([]service.ImageModelCapability{{Id: "image-alpha", MediaType: "video"}})
				require.NoError(t, err)
				require.NoError(t, db.Model(&model.ImageGroupPolicy{}).Where("policy_key = ?", service.ImageIdentityHash("default", "openai")).Update("models", string(videoCatalog)).Error)
				user := model.User{Username: "pool-video-user", Group: "default"}
				require.NoError(t, db.Create(&user).Error)
				token := model.Token{UserId: user.Id, Key: "pool-video-token", Group: "default"}
				require.NoError(t, db.Create(&token).Error)
				plugin := &jsplugin.LoadedPlugin{Meta: jsplugin.Meta{Key: "openai", Models: []string{"image-alpha"}, ChannelTypes: []int{1}}}
				videoRecorder := httptest.NewRecorder()
				videoContext, _ := gin.CreateTestContext(videoRecorder)
				videoContext.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", nil)
				videoContext.Set("id", user.Id)
				videoContext.Set("token_id", token.Id)
				videoContext.Set("task_request", map[string]any{"model": "image-alpha", "prompt": "weighted video", "seconds": 5, "resolution": "720p"})
				videoContext.Set(jsplugin.ContextKeyPinnedEndpoint, jsplugin.PinnedEndpoint{Model: "image-alpha", Candidates: []jsplugin.ProtocolBinding{{Plugin: plugin, Protocol: "openai_video"}}})
				RouteAsyncVideo(videoContext)
				require.False(t, videoContext.IsAborted(), videoRecorder.Body.String())
				pin, pinned, _ := service.GetChannelConstraints(videoContext).ResolvedPin()
				require.True(t, pinned)
				assert.Equal(t, 21, pin.ChannelId, "video uses the same zero-weight exclusion as images")
				require.NoError(t, db.Model(&model.ImageGroupPolicy{}).Where("policy_key = ?", service.ImageIdentityHash("default", "openai")).Update("models", string(catalog)).Error)
				require.NoError(t, db.Model(&model.ImageChannelPool{}).Where("binding_key = ? AND channel_id = ?", saved.BindingKey, 21).Update("priority", -9).Error)
				candidates, err := service.ImageCandidateChannels(t.Context(), policy, "image-alpha", "")
				require.NoError(t, err)
				for i := range candidates {
					assert.Zero(t, candidates[i].GetWeight(), "historical negative priorities are zero weights")
				}
				require.NoError(t, db.Where("binding_key = ?", saved.BindingKey).Delete(&model.ImageChannelPool{}).Error)
				selected, _, err = service.PickImageChannel(t.Context(), policy, request, service.ImageAccountRouting{})
				require.NoError(t, err)
				assert.Equal(t, 11, selected.Id, "missing binding preserves global priority routing")
			})

			t.Run("resolution mappings survive storage edits and channel copies", func(t *testing.T) {
				group := "resolution-mapping-test"
				ordinaryMapping := `{"seedream-5-pro":"legacy-image","seedream-5-pro-2K":"wrong-chain-target"}`
				channel := model.Channel{Type: 1, Name: "One upstream", Key: "mapping-test-key", Status: common.ChannelStatusEnabled, Models: "seedream-5-pro,other-image", Group: group, ModelMapping: &ordinaryMapping}
				settings := dto.ChannelSettings{ImageResolutionModelMapping: dto.ImageResolutionModelMapping{
					"seedream-5-pro": {"1K": "seedream-5-pro-1K", "2K": "seedream-5-pro-2K"},
				}}
				channel.SetSetting(settings)
				require.NoError(t, channel.ValidateSettings())
				require.NoError(t, channel.Insert())
				defer func() {
					assert.NoError(t, db.Where(map[string]any{"group": group}).Delete(&model.Ability{}).Error)
					assert.NoError(t, db.Where(map[string]any{"group": group}).Delete(&model.Channel{}).Error)
					assert.NoError(t, db.Where(map[string]any{"group": group}).Delete(&model.ImageChannelPool{}).Error)
					assert.NoError(t, db.Where(map[string]any{"group": group}).Delete(&model.ImageGroupPolicy{}).Error)
				}()
				loaded, err := model.GetChannelById(channel.Id, true)
				require.NoError(t, err)
				assert.Equal(t, settings.ImageResolutionModelMapping, loaded.GetSetting().ImageResolutionModelMapping)

				policy := model.ImageGroupPolicy{Group: group, Platform: "openai", PoolMode: "model_resolution", AsyncEnabled: true}
				for _, resolution := range []string{"1K", "2K"} {
					require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: service.ImagePoolBindingKey(policy, "seedream-5-pro", resolution), Group: group, Platform: "openai", Mode: "model_resolution", Model: "seedream-5-pro", Resolution: resolution, ChannelId: channel.Id, Priority: 7}).Error)
					candidates, err := service.ImageCandidateChannels(t.Context(), policy, "seedream-5-pro", resolution)
					require.NoError(t, err)
					require.Len(t, candidates, 1)
					var mapping map[string]string
					require.NoError(t, common.UnmarshalJsonStr(candidates[0].GetModelMapping(), &mapping))
					assert.Equal(t, "seedream-5-pro-"+resolution, mapping["seedream-5-pro"])
					assert.Equal(t, 7, candidates[0].GetWeight())
				}
				for _, resolution := range []string{"4K", "", "AUTO"} {
					candidates, err := service.ImageCandidateChannels(t.Context(), policy, "seedream-5-pro", resolution)
					require.NoError(t, err)
					assert.Empty(t, candidates, "unconfigured tier %q must not use the ordinary mapping", resolution)
				}
				candidates, err := service.ImageCandidateChannels(t.Context(), policy, "other-image", "4K")
				require.NoError(t, err)
				require.Len(t, candidates, 1, "rules for one model must not restrict another model")
				assert.Equal(t, ordinaryMapping, candidates[0].GetModelMapping())
				policy.AsyncEnabled = false
				candidates, err = service.ImageCandidateChannels(t.Context(), policy, "seedream-5-pro", "4K")
				require.NoError(t, err)
				require.Len(t, candidates, 1, "synchronous routing retains the ordinary mapping")
				assert.Equal(t, ordinaryMapping, candidates[0].GetModelMapping())

				t.Run("variant-only aliases stay visible only for asynchronous policies", func(t *testing.T) {
					originalCapability := service.ImageChannelCapabilityFunc
					service.ImageChannelCapabilityFunc = func(channel model.Channel, modelName string) dto.ImageCapability {
						capability := originalCapability(channel, modelName)
						if modelName == "seedream-5-pro" {
							var mapping map[string]string
							if common.UnmarshalJsonStr(channel.GetModelMapping(), &mapping) != nil {
								capability.Generate = false
							} else {
								capability.Generate = mapping[modelName] == "seedream-5-pro-1K" || mapping[modelName] == "seedream-5-pro-2K"
							}
						}
						return capability
					}
					defer func() { service.ImageChannelCapabilityFunc = originalCapability }()
					options, err := service.GetImagePoolSelectionOptions(t.Context(), group, "openai")
					require.NoError(t, err)
					assert.True(t, slices.ContainsFunc(options.Models, func(option service.ImagePoolModelOption) bool {
						return option.Id == "seedream-5-pro" && slices.Contains(option.ChannelIds, channel.Id) && slices.Contains(option.MediaTypes, "image")
					}), "a variant-only alias must be selectable before its policy is created")
					catalog, err := common.Marshal([]service.ImageModelCapability{{Id: "seedream-5-pro", Resolutions: []string{"1K", "2K", "4K"}, MaxOutputImages: 1}})
					require.NoError(t, err)
					configuredPolicy := model.ImageGroupPolicy{PolicyKey: service.ImageIdentityHash(group, "openai"), Group: group, Platform: "openai", PoolMode: "model_resolution", Models: string(catalog), Enabled: true, AsyncEnabled: true}
					require.NoError(t, db.Create(&configuredPolicy).Error)
					defer func() { assert.NoError(t, db.Delete(&configuredPolicy).Error) }()
					user := model.User{Username: "resolution-capability-user", AffCode: "imgres", Group: "default", Status: common.UserStatusEnabled}
					require.NoError(t, db.Create(&user).Error)
					token := model.Token{UserId: user.Id, Key: "resolution-capability-token", Group: group, Status: common.TokenStatusEnabled, ExpiredTime: -1}
					require.NoError(t, db.Create(&token).Error)
					for _, async := range []bool{true, false} {
						require.NoError(t, db.Model(&configuredPolicy).Update("async_enabled", async).Error)
						options, err = service.GetImagePoolSelectionOptions(t.Context(), group, "openai")
						require.NoError(t, err)
						if async {
							require.Len(t, options.Models, 1)
							assert.Equal(t, []int{channel.Id}, options.Models[0].ChannelIds)
						} else {
							assert.Empty(t, options.Models, "synchronous policies must not gain support from asynchronous mappings")
						}
						recorder := httptest.NewRecorder()
						ctx, _ := gin.CreateTestContext(recorder)
						ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
						ctx.Params = gin.Params{{Key: "token_id", Value: strconv.Itoa(token.Id)}}
						ctx.Set("id", user.Id)
						GetImageWorkbenchCapabilities(ctx)
						require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
						var response struct {
							Data struct {
								Models []struct {
									Id         string                       `json:"id"`
									Capability service.ImageModelCapability `json:"capability"`
								} `json:"models"`
							} `json:"data"`
						}
						require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
						if async {
							require.Len(t, response.Data.Models, 1, "enumeration must not require an unspecified-resolution route")
							assert.Equal(t, "seedream-5-pro", response.Data.Models[0].Id)
							assert.Equal(t, []string{"1K", "2K"}, response.Data.Models[0].Capability.Resolutions)
						} else {
							assert.Empty(t, response.Data.Models, "synchronous discovery must continue using the ordinary mapping")
						}
					}
				})

				loaded, err = model.GetChannelById(channel.Id, true)
				require.NoError(t, err)
				assert.Equal(t, ordinaryMapping, loaded.GetModelMapping(), "routing must not persist its effective mapping")
				loaded.Name = "Renamed upstream"
				require.NoError(t, loaded.Update())
				loaded, err = model.GetChannelById(channel.Id, true)
				require.NoError(t, err)
				assert.Equal(t, settings.ImageResolutionModelMapping, loaded.GetSetting().ImageResolutionModelMapping)
				clone := *loaded
				clone.Id, clone.Name = 0, "Copied upstream"
				require.NoError(t, clone.ValidateSettings())
				require.NoError(t, clone.Insert())
				copied, err := model.GetChannelById(clone.Id, true)
				require.NoError(t, err)
				assert.Equal(t, settings.ImageResolutionModelMapping, copied.GetSetting().ImageResolutionModelMapping)
				updated := loaded.GetSetting()
				updated.ImageResolutionModelMapping["seedream-5-pro"]["2K"] = "replacement-image-2K"
				loaded.SetSetting(updated)
				require.NoError(t, loaded.ValidateSettings())
				require.NoError(t, loaded.Update())
				loaded, err = model.GetChannelById(channel.Id, true)
				require.NoError(t, err)
				assert.Equal(t, "replacement-image-2K", loaded.GetSetting().ImageResolutionModelMapping["seedream-5-pro"]["2K"])
				copied, err = model.GetChannelById(clone.Id, true)
				require.NoError(t, err)
				assert.Equal(t, "seedream-5-pro-2K", copied.GetSetting().ImageResolutionModelMapping["seedream-5-pro"]["2K"])
				updated.ImageResolutionModelMapping = nil
				loaded.SetSetting(updated)
				require.NoError(t, loaded.Update())
				loaded, err = model.GetChannelById(channel.Id, true)
				require.NoError(t, err)
				assert.Empty(t, loaded.GetSetting().ImageResolutionModelMapping)
			})

			invalidModelBody, err := common.Marshal(gin.H{
				"group": "default", "platform": "openai", "mode": "model", "model": "manual-model",
				"channels": []gin.H{},
			})
			require.NoError(t, err)
			invalidModelRecorder := httptest.NewRecorder()
			invalidModelContext, _ := gin.CreateTestContext(invalidModelRecorder)
			invalidModelContext.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(invalidModelBody))
			SaveImageChannelPool(invalidModelContext)
			assert.Equal(t, http.StatusBadRequest, invalidModelRecorder.Code)
			assert.Contains(t, invalidModelRecorder.Body.String(), "Pool model is not enabled")

			otherPolicy := model.ImageGroupPolicy{PolicyKey: service.ImageIdentityHash("Default", "openai"), Group: "Default", Platform: "openai", Models: "[]", PoolMode: "model", Version: 1}
			require.NoError(t, db.Create(&otherPolicy).Error)
			require.NoError(t, db.Create(&model.Channel{Id: 14, Type: 1, Name: "OpenAI secondary", Key: "secondary-secret", Status: common.ChannelStatusEnabled, Models: "image-alpha", Group: "default"}).Error)
			require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "image-alpha", ChannelId: 14, Enabled: true}).Error)
			closedKey := service.ImagePoolBindingKey(model.ImageGroupPolicy{Group: "default", Platform: "openai", PoolMode: "model"}, "hidden-model", "")
			otherKey := service.ImagePoolBindingKey(otherPolicy, "image-alpha", "")
			require.NoError(t, db.Create(&[]model.ImageChannelPool{
				{BindingKey: saved.BindingKey, Group: "default", Platform: "openai", Mode: "model", Model: "image-alpha", ChannelId: 14},
				{BindingKey: closedKey, Group: "default", Platform: "openai", Mode: "model", Model: "hidden-model"},
				{BindingKey: otherKey, Group: "Default", Platform: "openai", Mode: "model", Model: "image-alpha", ChannelId: 11},
			}).Error)

			deleteRequest := func(path string, body any, role int) *httptest.ResponseRecorder {
				encoded, encodeErr := common.Marshal(body)
				require.NoError(t, encodeErr)
				engine := gin.New()
				engine.Use(func(c *gin.Context) {
					c.Set("id", 730001)
					c.Set("role", role)
				})
				engine.DELETE("/policy", middleware.RequirePermission(authz.ImageConfigManage), DeleteImagePolicy)
				engine.DELETE("/pool/:binding_key", middleware.RequirePermission(authz.ImageConfigManage), DeleteImageChannelPool)
				recorder := httptest.NewRecorder()
				engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, path, bytes.NewReader(encoded)))
				return recorder
			}
			policyBody := gin.H{"group": "default", "platform": "openai", "version": 1}
			for _, path := range []string{"/policy", "/pool/" + saved.BindingKey} {
				recorder := deleteRequest(path, policyBody, common.RoleCommonUser)
				assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
			}
			for _, body := range []gin.H{
				{"group": "", "platform": "openai", "version": 1},
				{"group": "default", "platform": "openai"},
				{"group": "default", "platform": "openai", "version": -1},
			} {
				assert.Equal(t, http.StatusBadRequest, deleteRequest("/policy", body, common.RoleRootUser).Code)
			}
			assert.Equal(t, http.StatusConflict, deleteRequest("/policy", gin.H{"group": "default", "platform": "openai", "version": 0}, common.RoleRootUser).Code)
			for _, key := range []string{"invalid", strings.Repeat("z", 64)} {
				assert.Equal(t, http.StatusBadRequest, deleteRequest("/pool/"+key, nil, common.RoleRootUser).Code)
			}
			var policies []model.ImageGroupPolicy
			var pools []model.ImageChannelPool
			require.NoError(t, db.Find(&policies).Error)
			require.NoError(t, db.Find(&pools).Error)
			assert.Len(t, policies, 2, "denied, invalid and stale requests retain policies")
			assert.Len(t, pools, 4, "denied and invalid requests retain every pool row")

			recorder := deleteRequest("/policy", policyBody, common.RoleRootUser)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.NoError(t, db.Find(&policies).Error)
			assert.Equal(t, []model.ImageGroupPolicy{otherPolicy}, policies, "case-sensitive sibling policy is retained")
			require.NoError(t, db.Find(&pools).Error)
			assert.Len(t, pools, 4, "deleting a policy does not cascade to channel pools")
			resolved, models, err := service.ResolveImagePolicy(t.Context(), model.Token{Group: "default"}, "openai")
			require.NoError(t, err)
			assert.False(t, resolved.Enabled)
			assert.False(t, resolved.AsyncEnabled)
			assert.Empty(t, models)
			assert.Equal(t, http.StatusNotFound, deleteRequest("/policy", policyBody, common.RoleRootUser).Code)

			recorder = deleteRequest("/pool/"+saved.BindingKey, nil, common.RoleRootUser)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.NoError(t, db.Order("id").Find(&pools).Error)
			require.Len(t, pools, 2, "all channel rows of the selected binding are removed")
			assert.Equal(t, closedKey, pools[0].BindingKey)
			assert.Equal(t, otherKey, pools[1].BindingKey)
			candidates, err := service.ImageCandidateChannels(t.Context(), model.ImageGroupPolicy{Group: "default", Platform: "openai", PoolMode: "model"}, "image-alpha", "")
			require.NoError(t, err)
			require.Len(t, candidates, 2, "missing binding restores eligible group channels")
			assert.ElementsMatch(t, []int{11, 14}, []int{candidates[0].Id, candidates[1].Id})
			assert.Equal(t, http.StatusNotFound, deleteRequest("/pool/"+saved.BindingKey, nil, common.RoleRootUser).Code)

			candidates, err = service.ImageCandidateChannels(t.Context(), model.ImageGroupPolicy{Group: "default", Platform: "openai", PoolMode: "model"}, "hidden-model", "")
			require.NoError(t, err)
			assert.Empty(t, candidates, "an explicit empty binding remains closed until deleted")
			recorder = deleteRequest("/pool/"+closedKey, nil, common.RoleRootUser)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			candidates, err = service.ImageCandidateChannels(t.Context(), model.ImageGroupPolicy{Group: "default", Platform: "openai", PoolMode: "model"}, "hidden-model", "")
			require.NoError(t, err)
			require.Len(t, candidates, 1)
			assert.Equal(t, 11, candidates[0].Id)
			require.NoError(t, db.Find(&pools).Error)
			require.Len(t, pools, 1)
			assert.Equal(t, otherKey, pools[0].BindingKey)
		})
	}
}

func verifyMediaRoutingPreview(t *testing.T, db *gorm.DB, redisServer *miniredis.Miniredis) {
	previousAuto := setting.AutoGroups2JsonString()
	previousUsable := setting.UserUsableGroups2JSONString()
	previousRatios := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["default"]`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","preview-image-family":"Image","preview-video-group":"Video","preview-video-secondary":"Video secondary"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"preview-image-family":1,"preview-video-group":1,"preview-video-secondary":1,"preview-video-denied":1}`))
	t.Cleanup(func() {
		assert.NoError(t, setting.UpdateAutoGroupsByJsonString(previousAuto))
		assert.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousUsable))
		assert.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios))
	})
	previousCapability := service.ImageChannelCapabilityFunc
	service.ImageChannelCapabilityFunc = func(channel model.Channel, name string) dto.ImageCapability {
		if channel.Type == 3 {
			return dto.ImageCapability{Provider: "newapi", Protocol: "openai_images", Generate: true}
		}
		if channel.Type == 4 {
			return dto.ImageCapability{Provider: "newapi", Protocol: "gemini_native", Generate: true}
		}
		if channel.Type == constant.ChannelTypeVolcEngine {
			return (&volcengine.Adaptor{}).ImageCapability(name)
		}
		return previousCapability(channel, name)
	}
	t.Cleanup(func() { service.ImageChannelCapabilityFunc = previousCapability })
	inheritedChannel := model.Channel{Type: 3, Name: "Inherited image channel", Key: "private-inherited-credential", Status: common.ChannelStatusEnabled, Models: "inherited-image", Group: "preview-image-family"}
	require.NoError(t, db.Create(&inheritedChannel).Error)
	require.NoError(t, db.Create(&model.Ability{Group: inheritedChannel.Group, Model: "inherited-image", ChannelId: inheritedChannel.Id, Enabled: true}).Error)
	otherFamilyChannels := []model.Channel{
		{Type: 1, Name: "Other provider in family", Key: "private-other-provider", Status: common.ChannelStatusEnabled, Models: "inherited-image", Group: inheritedChannel.Group},
		{Type: 4, Name: "Provider in unconfigured family", Key: "private-other-family", Status: common.ChannelStatusEnabled, Models: "inherited-image", Group: inheritedChannel.Group},
		{Type: constant.ChannelTypeVolcEngine, Name: "Seedream reference channel", Key: "private-reference-provider", Status: common.ChannelStatusEnabled, Models: "doubao-seedream-4-5-251128", Group: inheritedChannel.Group},
	}
	require.NoError(t, db.Create(&otherFamilyChannels).Error)
	for _, channel := range otherFamilyChannels {
		require.NoError(t, db.Create(&model.Ability{Group: channel.Group, Model: channel.Models, ChannelId: channel.Id, Enabled: true}).Error)
	}
	inheritedPolicy := model.ImageGroupPolicy{PolicyKey: service.ImageIdentityHash(inheritedChannel.Group, "openai"), Group: inheritedChannel.Group, Platform: "openai", PoolMode: "model", Models: `[{"id":"inherited-image","media_type":"image"},{"id":"doubao-seedream-4-5-251128","media_type":"image","max_reference_images":2}]`, Enabled: true, AsyncEnabled: true}
	require.NoError(t, db.Create(&inheritedPolicy).Error)
	t.Cleanup(func() {
		assert.NoError(t, db.Where("channel_id = ?", inheritedChannel.Id).Delete(&model.Ability{}).Error)
		assert.NoError(t, db.Delete(&inheritedChannel).Error)
		assert.NoError(t, db.Delete(&inheritedPolicy).Error)
		for _, channel := range otherFamilyChannels {
			assert.NoError(t, db.Where("channel_id = ?", channel.Id).Delete(&model.Ability{}).Error)
			assert.NoError(t, db.Delete(&channel).Error)
		}
	})
	t.Helper()
	settings := dto.ChannelSettings{ImageResolutionModelMapping: dto.ImageResolutionModelMapping{"image-alpha": {"2K": "upstream-image-2K"}}}
	settingJSON, err := common.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 11).Update("setting", string(settingJSON)).Error)
	t.Cleanup(func() { assert.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 11).Update("setting", "").Error) })
	const pluginKey = "routing-preview-video"
	_, err = jsplugin.DefaultRegistry.Register(`
export const meta = {apiVersion: 1, key: "routing-preview-video", name: "Routing preview", version: "1.0.0", author: {name: "Test"}, models: ["preview-video"], fetchMode: "per_task", protocols: ["openai_video"], videoProfiles: [{models: ["preview-video"], modes: [{name: "text_to_video", duration: {min: 1, max: 10, step: 1, default: 5}, resolutions: ["720p"], defaultResolution: "720p"}]}]};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
export const protocols = {openai_video: {decodeRequest() { return {}; }, render() { return {}; }}};
export function listArtifacts() { return []; }
export function buildContentRequest() { return {}; }
`, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(pluginKey) })
	video := model.Channel{Type: 61, Name: "Preview video channel", Key: "private-video-credential", Status: common.ChannelStatusEnabled, Models: "preview-video", Group: "preview-video-group"}
	video.SetSetting(dto.ChannelSettings{TaskPluginKey: pluginKey})
	require.NoError(t, db.Create(&video).Error)
	for _, group := range []string{video.Group, "preview-video-secondary", "preview-video-denied"} {
		require.NoError(t, db.Create(&model.Ability{Group: group, Model: "preview-video", ChannelId: video.Id, Enabled: true}).Error)
	}
	t.Cleanup(func() {
		assert.NoError(t, db.Where("channel_id = ?", video.Id).Delete(&model.Ability{}).Error)
		assert.NoError(t, db.Delete(&video).Error)
	})
	owner := model.User{Username: "routing-preview-owner", Group: "default", Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(&owner).Error)
	tokens := []model.Token{
		{UserId: owner.Id, Key: "preview-token-credential", Group: "default", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100},
		{UserId: owner.Id, Key: "preview-disabled-credential", Group: "default", Status: common.TokenStatusDisabled, ExpiredTime: -1, RemainQuota: 100},
		{UserId: owner.Id, Key: "preview-limited-credential", Group: "default", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100, ModelLimitsEnabled: true, ModelLimits: "other-image"},
		{UserId: owner.Id, Key: "preview-inherited-credential", Group: inheritedChannel.Group, Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100},
		{UserId: owner.Id, Key: "preview-auto-credential", Group: "auto", AutoGroups: `["preview-video-denied","preview-video-secondary","preview-video-group"]`, Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100},
		{UserId: owner.Id, Key: "preview-revoked-credential", Group: "auto", AutoGroups: `["preview-video-denied"]`, Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100},
	}
	require.NoError(t, db.Create(&tokens).Error)
	t.Cleanup(func() {
		assert.NoError(t, db.Unscoped().Where("user_id = ?", owner.Id).Delete(&model.Token{}).Error)
		assert.NoError(t, db.Unscoped().Delete(&owner).Error)
	})
	for _, body := range []string{
		`{"provider":"newapi","model":"inherited-image","prompt":"routing preview","resolution":"auto"}`,
		`{"provider":"volcengine","model":"doubao-seedream-4-5-251128","prompt":"routing preview","resolution":"1K","image":"https://example.com/reference.png"}`,
	} {
		request, err := service.ParseAsyncImageRequest([]byte(body), "application/json", "/v1/images/generations_async", service.DefaultImageRuntimeConfig())
		require.NoError(t, err)
		policy, _, err := service.ResolveAsyncImagePolicy(t.Context(), tokens[3], request)
		require.NoError(t, err)
		channels, err := service.ImageCandidateChannels(t.Context(), policy, request.Model, service.AsyncImageRoutingResolution(request))
		require.NoError(t, err)
		require.True(t, slices.ContainsFunc(channels, func(channel model.Channel) bool {
			capability := service.ImageChannelCapability(channel, request.Model)
			return capability.Provider == request.Provider && service.ImageCapabilitySupportsRequest(capability, request)
		}), "the actual async generation path must support this preview fixture")
	}
	const callback = "test:routing-preview-must-not-write"
	failWrite := func(tx *gorm.DB) { tx.AddError(errors.New("routing preview must be read only")) }
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, failWrite))
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, failWrite))
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(callback, failWrite))
	t.Cleanup(func() {
		assert.NoError(t, db.Callback().Create().Remove(callback))
		assert.NoError(t, db.Callback().Update().Remove(callback))
		assert.NoError(t, db.Callback().Delete().Remove(callback))
	})
	redisKeys := redisServer.Keys()
	for _, test := range []struct {
		name       string
		request    service.MediaRoutingPreviewRequest
		wantID     int
		wantModel  string
		wantReason string
		wantGroups []string
	}{
		{"image mapping", service.MediaRoutingPreviewRequest{Group: "default", Platform: "openai", MediaType: "image", Model: "image-alpha", Resolution: "2K"}, 11, "upstream-image-2K", "", []string{"default"}},
		{"missing tier", service.MediaRoutingPreviewRequest{Group: "default", Platform: "openai", MediaType: "image", Model: "image-alpha", Resolution: "4K"}, 0, "", "resolution_mapping_missing", []string{"default"}},
		{"auto still requires configured channel mapping", service.MediaRoutingPreviewRequest{Group: "default", Platform: "openai", MediaType: "image", Model: "image-alpha", Resolution: "AUTO"}, 0, "", "resolution_mapping_missing", []string{"default"}},
		{"token resolves effective group", service.MediaRoutingPreviewRequest{TokenID: tokens[0].Id, Group: "untrusted", Platform: "openai", MediaType: "image", Model: "image-alpha", Resolution: "2K"}, 11, "upstream-image-2K", "", []string{"default"}},
		{"disabled token", service.MediaRoutingPreviewRequest{TokenID: tokens[1].Id, Platform: "openai", MediaType: "image", Model: "image-alpha", Resolution: "2K"}, 0, "", "token_disabled", []string{"default"}},
		{"token model restriction", service.MediaRoutingPreviewRequest{TokenID: tokens[2].Id, Platform: "openai", MediaType: "image", Model: "image-alpha", Resolution: "2K"}, 0, "", "token_model_not_allowed", []string{"default"}},
		{"token inherits protocol family policy", service.MediaRoutingPreviewRequest{TokenID: tokens[3].Id, Platform: "newapi", MediaType: "image", Model: "inherited-image", Resolution: "1K"}, inheritedChannel.Id, "inherited-image", "", []string{inheritedChannel.Group}},
		{"explicit group inherits protocol family policy", service.MediaRoutingPreviewRequest{Group: inheritedChannel.Group, Platform: "newapi", MediaType: "image", Model: "inherited-image", Resolution: "1K"}, inheritedChannel.Id, "inherited-image", "", []string{inheritedChannel.Group}},
		{"auto inherits protocol family policy", service.MediaRoutingPreviewRequest{TokenID: tokens[3].Id, Platform: "newapi", MediaType: "image", Model: "inherited-image", Resolution: "AUTO"}, inheritedChannel.Id, "inherited-image", "", []string{inheritedChannel.Group}},
		{"generation reference input", service.MediaRoutingPreviewRequest{TokenID: tokens[3].Id, Platform: "volcengine", MediaType: "image", Model: "doubao-seedream-4-5-251128", Resolution: "1K", Operation: "image_to_image"}, otherFamilyChannels[2].Id, "doubao-seedream-4-5-251128", "", []string{inheritedChannel.Group}},
		{"explicit group respects provider within family", service.MediaRoutingPreviewRequest{Group: inheritedChannel.Group, Platform: "openai", MediaType: "image", Model: "inherited-image", Resolution: "1K"}, otherFamilyChannels[0].Id, "inherited-image", "", []string{inheritedChannel.Group}},
		{"video default routing", service.MediaRoutingPreviewRequest{Group: video.Group, Platform: pluginKey, MediaType: "video", Model: "preview-video", Resolution: "720p"}, video.Id, "preview-video", "", []string{video.Group}},
		{"video unsupported duration", service.MediaRoutingPreviewRequest{Group: video.Group, Platform: pluginKey, MediaType: "video", Model: "preview-video", Resolution: "720p", Seconds: common.GetPointer(11)}, 0, "", "video_parameters_not_supported", []string{video.Group}},
		{"video unsupported operation", service.MediaRoutingPreviewRequest{Group: video.Group, Platform: pluginKey, MediaType: "video", Model: "preview-video", Resolution: "720p", Operation: "edit_video"}, 0, "", "operation_not_supported", []string{video.Group}},
		{"video auto custom groups", service.MediaRoutingPreviewRequest{TokenID: tokens[4].Id, Platform: pluginKey, MediaType: "video", Model: "preview-video", Resolution: "720p"}, video.Id, "preview-video", "", []string{"preview-video-secondary", video.Group}},
		{"video auto revoked groups", service.MediaRoutingPreviewRequest{TokenID: tokens[5].Id, Platform: pluginKey, MediaType: "video", Model: "preview-video", Resolution: "720p"}, 0, "", "token_group_not_allowed", []string{}},
		{"auto group requires token context", service.MediaRoutingPreviewRequest{Group: "auto", Platform: pluginKey, MediaType: "video", Model: "preview-video", Resolution: "720p"}, 0, "", "token_required_for_auto_group", []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := common.Marshal(test.request)
			require.NoError(t, err)
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set("id", 730001); c.Set("role", common.RoleRootUser) })
			router.POST("/preview", middleware.RequirePermission(authz.ImageConfigRead), PreviewMediaRouting)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/preview", bytes.NewReader(encoded)))
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			var response struct {
				Data service.MediaRoutingPreview `json:"data"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Equal(t, []string{"configuration_preview_only"}, response.Data.Warnings)
			assert.Equal(t, test.wantGroups, response.Data.RoutingGroups)
			if test.wantID > 0 {
				require.Len(t, response.Data.Eligible, len(test.wantGroups))
				for index, candidate := range response.Data.Eligible {
					assert.Equal(t, test.wantID, candidate.ChannelID)
					assert.Equal(t, test.wantModel, candidate.UpstreamModel)
					assert.Equal(t, test.wantGroups[index], candidate.Group)
				}
			} else {
				assert.Empty(t, response.Data.Eligible)
				assert.True(t, slices.Contains(response.Data.Reasons, test.wantReason) || slices.ContainsFunc(response.Data.Excluded, func(channel service.MediaRoutingPreviewChannel) bool {
					return slices.Contains(channel.Reasons, test.wantReason)
				}))
			}
			for _, secret := range []string{"openai-secret", "gemini-secret", "disabled-secret", "private-video-credential", "preview-token-credential", "preview-disabled-credential", "preview-limited-credential"} {
				assert.NotContains(t, recorder.Body.String(), secret)
			}
		})
	}
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("id", 730002); c.Set("role", common.RoleCommonUser) })
	router.POST("/preview", middleware.RequirePermission(authz.ImageConfigRead), PreviewMediaRouting)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(`{}`)))
	assert.Equal(t, http.StatusForbidden, recorder.Code, "ordinary users cannot inspect administrator routing")
	for _, body := range []string{
		`{"group":"default","platform":"openai","media_type":"image","model":"image-alpha","token_id":-1}`,
		`{"platform":"openai","media_type":"image","model":"image-alpha"}`,
		`{"group":"default","platform":"openai","media_type":"image","model":"image-alpha","resolution":"8K"}`,
		`{"group":"default","platform":"openai","media_type":"image","model":"image-alpha","seconds":0}`,
		`{"group":"default","platform":"openai","media_type":"image","model":"image-alpha","operation":"edit_video"}`,
	} {
		router := gin.New()
		router.Use(func(c *gin.Context) { c.Set("id", 730001); c.Set("role", common.RoleRootUser) })
		router.POST("/preview", middleware.RequirePermission(authz.ImageConfigRead), PreviewMediaRouting)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(body)))
		assert.Equal(t, http.StatusBadRequest, recorder.Code, body)
	}
	assert.Equal(t, redisKeys, redisServer.Keys(), "preview must not reserve concurrency slots or mutate health state")
}

func TestSaveMediaPolicyAcceptsImageVideoCatalogAndRejectsCapabilityExpansion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "media-policy.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		connection, closeErr := db.DB()
		if assert.NoError(t, closeErr) {
			assert.NoError(t, connection.Close())
		}
	})
	require.NoError(t, db.AutoMigrate(&model.ImageGroupPolicy{}))

	save := func(models any) *httptest.ResponseRecorder {
		catalogJSON, catalogErr := common.Marshal(models)
		require.NoError(t, catalogErr)
		body, marshalErr := common.Marshal(gin.H{
			"group": "default", "platform": "xai", "pool_mode": "model_resolution", "enabled": true, "async_enabled": true, "models": string(catalogJSON),
		})
		require.NoError(t, marshalErr)
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
		SaveImagePolicy(context)
		return recorder
	}

	models := []service.ImageModelCapability{
		{Id: "grok-imagine-video-1.5", Label: "Grok image-compatible entry", MediaType: "image", MaxOutputImages: 1},
		{Id: "grok-imagine-video-1.5", Label: "Grok Imagine Video 1.5", MediaType: "video", VideoOverrides: service.VideoModelOverrides{Modes: []string{"text_to_video", "image_to_video"}, Resolutions: []string{"720p"}, DefaultResolution: "720p"}},
	}
	recorder := save(models)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var saved model.ImageGroupPolicy
	require.NoError(t, db.Where("platform = ?", "xai").Take(&saved).Error)
	var catalog []service.ImageModelCapability
	require.NoError(t, common.UnmarshalJsonStr(saved.Models, &catalog))
	require.Len(t, catalog, 2)
	assert.Equal(t, "image", catalog[0].MediaType)
	assert.Equal(t, "video", catalog[1].MediaType)

	models[1].VideoOverrides.Resolutions = []string{"4k"}
	recorder = save(models)
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "expands")

	require.NoError(t, db.Where("platform = ?", "xai").Delete(&model.ImageGroupPolicy{}).Error)
	recorder = save([]service.ImageModelCapability{{Id: "grok-imagine-video-1.5", Label: "Legacy hand-authored video"}})
	assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
}
