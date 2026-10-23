package controller

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

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
			previousCapability := service.ImageChannelCapabilityFunc
			model.DB = db
			common.SetMainDatabaseType(common.DatabaseType(engine))
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
			entities := []any{&model.ImageChannelPool{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.Ability{}, &model.Channel{}, &model.Token{}, &model.User{}, &model.Option{}}
			t.Cleanup(func() {
				model.DB, service.ImageChannelCapabilityFunc = previousDB, previousCapability
				common.SetMainDatabaseType(previousType)
				assert.NoError(t, db.Migrator().DropTable(entities...))
				assert.NoError(t, connection.Close())
			})
			require.NoError(t, db.AutoMigrate(entities...))
			require.NoError(t, db.AutoMigrate(entities...))
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
			router.Use(func(c *gin.Context) { c.Set("id", 701) })
			router.PUT("/policy", SaveImagePolicy)
			router.PUT("/pool", SaveImageChannelPool)
			router.GET("/options", GetImageChannelPoolOptions)
			router.POST("/preview", PreviewMediaRouting)
			router.GET("/capabilities/:token_id", GetImageWorkbenchCapabilities)
			router.PUT("/token/:id", UpdateTokenImageMappings)
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
			assert.ElementsMatch(t, []int{1, 29, 45, 60}, options.Data.Models[0].ChannelIds)
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
					if candidate.ChannelID == 24 || candidate.ChannelID == 54 {
						assert.Contains(t, candidate.Reasons, "platform_mismatch")
					}
				}
				assert.Contains(t, excluded, 24)
				assert.Contains(t, excluded, 54)
				assert.NotContains(t, recorder.Body.String(), "fixture-openai-key")
			})
			t.Run("native image and video task protocols cannot be saved in mixed pools", func(t *testing.T) {
				for _, channelID := range []int{24, 54} {
					recorder := call(http.MethodPut, "/pool", gin.H{"group": "default", "platform": "mixed", "mode": "model_resolution", "model": publicModel, "resolution": "2K", "channels": []gin.H{{"id": channelID, "priority": 1}}})
					assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
				}
				recorder := call(http.MethodPost, "/preview", gin.H{"group": "default", "platform": "mixed", "media_type": "video", "model": publicModel, "resolution": "480p"})
				assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
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
		})
	}
}
