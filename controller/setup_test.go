package controller

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupPersistenceDatabase(t *testing.T, kind, dsn string) *gorm.DB {
	t.Helper()
	db, _ := newAuditTestDatabase(t, kind, dsn)
	previousDB, previousOptions := model.DB, common.OptionMap
	previousSetup := constant.Setup
	previousSelfUse, previousDemo := operation_setting.SelfUseModeEnabled, operation_setting.DemoSiteEnabled
	model.DB, common.OptionMap, constant.Setup = db, map[string]string{}, false
	operation_setting.SelfUseModeEnabled, operation_setting.DemoSiteEnabled = false, false
	t.Cleanup(func() {
		model.DB, common.OptionMap, constant.Setup = previousDB, previousOptions, previousSetup
		operation_setting.SelfUseModeEnabled, operation_setting.DemoSiteEnabled = previousSelfUse, previousDemo
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Option{}, &model.Setup{}))
	versionQuery := "SELECT version()"
	if kind == "sqlite" {
		versionQuery = "SELECT sqlite_version()"
	}
	var version string
	require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
	t.Logf("%s version: %s", kind, version)
	return db
}

func setupRequest(t *testing.T, request SetupRequest) bool {
	t.Helper()
	body, err := common.Marshal(request)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(body))
	PostSetup(context)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool `json:"success"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	return response.Success
}

func TestSetupAndOptionPersistenceDatabaseMatrix(t *testing.T) {
	for _, dialect := range []struct{ name, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.name, func(t *testing.T) {
			dsn := os.Getenv(dialect.env)
			if dialect.env != "" && dsn == "" {
				t.Skip(dialect.env + " is not configured")
			}
			db := setupPersistenceDatabase(t, dialect.name, dsn)
			request := SetupRequest{Username: "  rootadmin  ", Password: " setup password with spaces ", ConfirmPassword: " setup password with spaces ", SelfUseModeEnabled: true, DemoSiteEnabled: true}
			for _, username := range []string{"", " \t\n\u3000", "1234567890123"} {
				invalid := request
				invalid.Username = username
				assert.False(t, setupRequest(t, invalid), "invalid username must not initialize the instance")
			}
			for _, failedTable := range []string{"users", "options", "setups"} {
				t.Run("rollback_"+failedTable, func(t *testing.T) {
					const callback = "test:setup-persistence-failure"
					require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
						if tx.Statement.Table == failedTable {
							tx.AddError(errors.New("injected persistence failure"))
						}
					}))
					assert.False(t, setupRequest(t, request))
					require.NoError(t, db.Callback().Create().Remove(callback))
					for _, table := range []any{&model.User{}, &model.Option{}, &model.Setup{}} {
						var count int64
						require.NoError(t, db.Model(table).Count(&count).Error)
						assert.Zero(t, count)
					}
					assert.False(t, constant.Setup)
					assert.False(t, operation_setting.SelfUseModeEnabled)
					assert.False(t, operation_setting.DemoSiteEnabled)
					assert.Empty(t, common.OptionMap)
				})
			}
			require.True(t, setupRequest(t, request))
			assert.True(t, constant.Setup)
			assert.True(t, operation_setting.SelfUseModeEnabled)
			assert.True(t, operation_setting.DemoSiteEnabled)
			var root model.User
			require.NoError(t, db.Where("role = ?", common.RoleRootUser).First(&root).Error)
			assert.Equal(t, "rootadmin", root.Username)
			assert.True(t, common.ValidatePasswordAndHash(request.Password, root.Password))
			assert.NotEqual(t, request.Password, root.Password)
			assert.False(t, setupRequest(t, request), "setup replay is rejected")
			constant.Setup = false
			assert.False(t, setupRequest(t, request), "a stale process flag cannot bypass the persisted setup marker")
			constant.Setup = true

			for _, scenario := range []struct{ name, key, callback string }{
				{"existing option update", "SelfUseModeEnabled", "update"},
				{"new option create", "SetupRegressionOption", "create"},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					writeErr := errors.New("injected option write failure")
					const callback = "test:option-write-failure"
					failWrite := func(tx *gorm.DB) {
						if tx.Statement.Table == "options" {
							tx.AddError(writeErr)
						}
					}
					if scenario.callback == "create" {
						require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, failWrite))
					} else {
						require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, failWrite))
					}
					assert.ErrorIs(t, model.UpdateOption(scenario.key, "false"), writeErr)
					if scenario.callback == "create" {
						require.NoError(t, db.Callback().Create().Remove(callback))
						var count int64
						require.NoError(t, db.Model(&model.Option{}).Where(&model.Option{Key: scenario.key}).Count(&count).Error)
						assert.Zero(t, count)
						assert.NotContains(t, common.OptionMap, scenario.key)
					} else {
						require.NoError(t, db.Callback().Update().Remove(callback))
						var option model.Option
						require.NoError(t, db.Where(&model.Option{Key: scenario.key}).First(&option).Error)
						assert.Equal(t, "true", option.Value)
						assert.Equal(t, "true", common.OptionMap[scenario.key])
						assert.True(t, operation_setting.SelfUseModeEnabled)
					}
					require.NoError(t, model.UpdateOption(scenario.key, "false"))
					assert.Equal(t, "false", common.OptionMap[scenario.key])
					var option model.Option
					require.NoError(t, db.Where(&model.Option{Key: scenario.key}).First(&option).Error)
					assert.Equal(t, "false", option.Value)
				})
			}
		})
	}
}
