package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestImageChannelPoolOptionsAndValidationDatabaseMatrix(t *testing.T) {
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
			previousCapability := service.ImageChannelCapabilityFunc
			model.DB = db
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
			entities := []any{&model.Channel{}, &model.Ability{}, &model.ImageGroupPolicy{}, &model.ImageChannelPool{}}
			t.Cleanup(func() {
				model.DB = previousDB
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
				PolicyKey: service.ImageIdentityHash("default", "openai"),
				Group:     "default",
				Platform:  "openai",
				PoolMode:  "model",
				Models:    string(catalog),
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
			require.Equal(t, []service.ImagePoolModelOption{{Id: "image-alpha", Label: "Image Alpha", ChannelIds: []int{11}}}, optionsResponse.Data.Models)
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
		})
	}
}
