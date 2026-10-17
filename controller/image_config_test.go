package controller

import (
	"bytes"
	"cmp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
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
						body, err := common.Marshal(gin.H{
							"group": "default", "platform": "openai", "mode": "model", "model": "image-alpha",
							"channels": []gin.H{{"id": 11, "priority": tc.weights[0]}, {"id": 21, "priority": tc.weights[1]}, {"id": 22, "priority": tc.weights[2]}},
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
