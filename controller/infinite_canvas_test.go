package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	gatewaytypes "github.com/QuantumNous/new-api/types"
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
	"gorm.io/gorm/schema"
)

type canvasFixture struct {
	db       *gorm.DB
	redis    *miniredis.Miniredis
	user     model.User
	identity service.AuthIdentity
	proof    string
	verifier string
	router   *gin.Engine
}

type canvasQuotaProjectionHook struct{ before func(redis.Cmder) }

func (h canvasQuotaProjectionHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	h.before(cmd)
	return ctx, nil
}
func (h canvasQuotaProjectionHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (h canvasQuotaProjectionHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (h canvasQuotaProjectionHook) AfterProcessPipeline(context.Context, []redis.Cmder) error {
	return nil
}

func newCanvasFixture(t *testing.T) *canvasFixture {
	t.Helper()
	previousDB, previousLogDB, previousRedis, previousRDB, previousSecret := model.DB, model.LOG_DB, common.RedisEnabled, common.RDB, common.SessionSecret
	previousSettings := system_setting.InfiniteCanvasSettingsSnapshot()
	previousType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	f := &canvasFixture{}
	dialect := os.Getenv("TEST_CANVAS_DIALECT")
	if dialect == "" {
		dialect = "sqlite"
	}
	require.Contains(t, []string{"sqlite", "mysql", "postgres"}, dialect)
	f.db, _ = newAuditTestDatabase(t, dialect, os.Getenv("TEST_"+strings.ToUpper(dialect)+"_DSN"))
	f.db.Logger = logger.Default.LogMode(logger.Silent)
	sqlDB, err := f.db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, f.db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.Token{}, &model.CanvasGrant{}, &model.AuditLog{}, &model.AsyncImageTask{}, &model.AsyncImageResult{}, &model.ImageStorageObject{}, &model.ImageUploadIntent{}, &model.ImageInputObject{}, &model.MediaArtifactObject{}, &model.AsyncMediaJob{}, &model.ImageStorageProfile{}, &model.AsyncImageEvent{}, &model.Option{}))
	f.redis = miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: f.redis.Addr()})
	model.DB, model.LOG_DB, common.RedisEnabled, common.RDB, common.SessionSecret = f.db, f.db, true, client, "canvas-fixture-signing-secret"
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	if dialect == "mysql" {
		common.SetMainDatabaseType(common.DatabaseTypeMySQL)
	} else if dialect == "postgres" {
		common.SetMainDatabaseType(common.DatabaseTypePostgreSQL)
	}
	t.Setenv("LOG_SQL_DSN", "")
	previousMaster := common.IsMasterNode
	common.IsMasterNode = false
	require.NoError(t, model.InitLogDB()) // Initialize dialect-specific quoted columns for ordinary TokenAuth.
	common.IsMasterNode = previousMaster
	t.Setenv("INFINITE_CANVAS_CLIENT_SECRET", strings.Repeat("secret", 8))
	require.NoError(t, config.UpdateConfigFromMap(config.GlobalConfig.Get("infinite_canvas"), map[string]string{"enabled": "true", "url": "https://canvas.example.test", "client_id": "infinite-canvas"}))
	t.Cleanup(func() {
		require.NoError(t, config.UpdateConfigFromMap(config.GlobalConfig.Get("infinite_canvas"), map[string]string{"enabled": fmt.Sprint(previousSettings.Enabled), "url": previousSettings.URL, "client_id": previousSettings.ClientID}))
		model.DB, model.LOG_DB, common.RedisEnabled, common.RDB, common.SessionSecret = previousDB, previousLogDB, previousRedis, previousRDB, previousSecret
		common.SetDatabaseTypes(previousType, previousLogType)
		_ = client.Close()
	})
	f.user = model.User{Username: "canvas-user", DisplayName: "Canvas User", AffCode: "canvas-owner", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Group: "default", AuthVersion: 1, Quota: 1000000}
	require.NoError(t, f.db.Create(&f.user).Error)
	f.identity = service.AuthIdentity{UserID: f.user.Id, SessionID: "canvas-parent", UserAuthVersion: 1, SessionVersion: 1}
	require.NoError(t, f.db.Create(&model.UserSession{SID: f.identity.SessionID, UserID: f.user.Id, Version: 1, UserAuthVersion: 1, Status: model.UserSessionStatusActive, RefreshHash: "not-a-usable-token", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()}).Error)
	f.proof = "Basic " + base64.StdEncoding.EncodeToString([]byte("infinite-canvas:"+system_setting.CanvasClientSecret()))
	f.verifier = strings.Repeat("p", 64)
	gin.SetMode(gin.TestMode)
	f.router = gin.New()
	api := f.router.Group("/api", middleware.CanvasIntegrationCredentials())
	api.POST("/integrations/canvas/authorize", middleware.UserAuth(), AuthorizeInfiniteCanvas)
	api.POST("/integrations/canvas/exchange", ExchangeInfiniteCanvas)
	api.POST("/integrations/canvas/introspect", IntrospectInfiniteCanvas)
	api.POST("/integrations/canvas/revoke", RevokeInfiniteCanvas)
	api.GET("/integrations/canvas/history/:kind/:task_id", CanvasHistoryTask)
	api.GET("/integrations/canvas/history/:kind/:task_id/content/:artifact", CanvasHistoryContent)
	api.HEAD("/integrations/canvas/history/:kind/:task_id/content/:artifact", CanvasHistoryContent)
	api.GET("/integrations/canvas/inputs/:input_id/content", CanvasInputContent)
	api.GET("/user/async-image-tasks", middleware.UserAuth(), func(c *gin.Context) { ListImageTasks(c, false) })
	api.GET("/image-objects/:object_id/content", GetImageObjectContent)
	api.GET("/user/async-image-tasks/:task_id", middleware.UserAuth(), func(c *gin.Context) { GetImageTask(c, false) })
	api.GET("/user/async-image-tasks/:task_id/results/:image_index/view", middleware.UserAuth(), func(c *gin.Context) { ViewImageTaskResult(c, false) })
	api.GET("/admin/async-image-tasks/:task_id", middleware.AdminAuth(), func(c *gin.Context) { GetImageTask(c, true) })
	api.GET("/admin/async-image-tasks/:task_id/results/:image_index/view", middleware.AdminAuth(), func(c *gin.Context) { ViewImageTaskResult(c, true) })
	api.GET("/user/media-tasks", middleware.UserAuth(), func(c *gin.Context) { ListMediaTasks(c, false) })
	api.GET("/user/media-tasks/:task_id", middleware.UserAuth(), func(c *gin.Context) { GetMediaTask(c, false) })
	api.GET("/admin/media-tasks/:task_id", middleware.AdminAuth(), func(c *gin.Context) { GetMediaTask(c, true) })
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		api.Handle(method, "/user/media-tasks/:task_id/results/:artifact_key/view", middleware.UserAuth(), func(c *gin.Context) { ViewMediaTaskResult(c, false) })
		api.Handle(method, "/admin/media-tasks/:task_id/results/:artifact_key/view", middleware.AdminAuth(), func(c *gin.Context) { ViewMediaTaskResult(c, true) })
	}
	api.POST("/token/:id/key", func(c *gin.Context) { c.Set("id", f.user.Id) }, GetTokenKey)
	api.POST("/token/batch/keys", func(c *gin.Context) { c.Set("id", f.user.Id) }, GetTokenKeysBatch)
	api.PUT("/token/", func(c *gin.Context) { c.Set("id", f.user.Id) }, UpdateToken)
	for _, path := range []string{"/v1/models", "/v1/videos/:task_id"} {
		f.router.GET(path, middleware.TokenAuth(), func(c *gin.Context) {
			if c.Query("catalog") == "1" {
				ListModels(c, constant.ChannelTypeOpenAI)
				return
			}
			c.JSON(200, gin.H{"id": c.GetInt("id"), "token_id": c.GetInt("token_id"), "proof": c.GetHeader(service.CanvasClientProofHeader), "grant_header": c.GetHeader(service.CanvasGrantHeader)})
		})
	}
	f.router.POST("/v1/images/generations", middleware.TokenAuth(), func(c *gin.Context) { c.JSON(200, gin.H{"id": c.GetInt("id")}) })
	f.router.GET("/v1/images/tasks_async/:task_id", middleware.TokenAuth(), QueryAsyncImage)
	f.router.GET("/v1/media/tasks_async/:task_id", middleware.TokenAuth(), QueryAsyncMedia)
	f.router.GET("/v1/media/objects/:object_id", MediaObjectContent)
	f.router.GET("/api/log/token", middleware.TokenAuthReadOnly(), func(c *gin.Context) { c.Status(200) })
	f.router.GET("/v1/images/tasks_async/stats", middleware.AsyncTaskStatsAuth(), func(c *gin.Context) { c.Status(200) })
	return f
}

func (f *canvasFixture) code(t *testing.T) string {
	t.Helper()
	digest := sha256.Sum256([]byte(f.verifier))
	redirect, err := service.AuthorizeCanvas(context.Background(), f.identity, "infinite-canvas", "https://canvas.example.test/auth/callback", strings.Repeat("state", 8), base64.RawURLEncoding.EncodeToString(digest[:]), "S256")
	require.NoError(t, err)
	u, err := url.Parse(redirect)
	require.NoError(t, err)
	return u.Query().Get("code")
}

func (f *canvasFixture) grant(t *testing.T) *service.CanvasExchange {
	t.Helper()
	result, err := service.ExchangeCanvasCode(context.Background(), f.code(t), "https://canvas.example.test/auth/callback", f.verifier)
	require.NoError(t, err)
	return result
}

func (f *canvasFixture) request(method, path, body, authorization, proof, grant string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if proof != "" {
		request.Header.Set(service.CanvasClientProofHeader, proof)
	}
	if grant != "" {
		request.Header.Set(service.CanvasGrantHeader, grant)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	return response
}

func TestCanvasCodeExchangeSecurity(t *testing.T) {
	t.Run("PKCE failure does not consume and successful replay revokes", func(t *testing.T) {
		f := newCanvasFixture(t)
		code := f.code(t)
		_, err := service.ExchangeCanvasCode(context.Background(), code, "https://canvas.example.test/auth/callback", strings.Repeat("x", 64))
		require.Error(t, err)
		grant, err := service.ExchangeCanvasCode(context.Background(), code, "https://canvas.example.test/auth/callback", f.verifier)
		require.NoError(t, err)
		assert.Equal(t, 200, f.request("GET", "/v1/models", "", "Bearer "+grant.APIKey, f.proof, "").Code)
		_, err = service.ExchangeCanvasCode(context.Background(), code, "https://canvas.example.test/auth/callback", f.verifier)
		require.Error(t, err)
		assert.Equal(t, 401, f.request("GET", "/v1/models", "", "Bearer "+grant.APIKey, f.proof, "").Code)
	})
	t.Run("concurrent exchange cannot leave a live replayed grant", func(t *testing.T) {
		f := newCanvasFixture(t)
		code := f.code(t)
		var wg sync.WaitGroup
		results := make(chan *service.CanvasExchange, 2)
		for range 2 {
			wg.Go(func() {
				result, _ := service.ExchangeCanvasCode(context.Background(), code, "https://canvas.example.test/auth/callback", f.verifier)
				results <- result
			})
		}
		wg.Wait()
		close(results)
		for result := range results {
			if result != nil {
				assert.Equal(t, 401, f.request("GET", "/v1/models", "", "Bearer "+result.APIKey, f.proof, "").Code)
			}
		}
	})
	t.Run("expired code and callback substitution", func(t *testing.T) {
		f := newCanvasFixture(t)
		code := f.code(t)
		_, err := service.ExchangeCanvasCode(context.Background(), code, "https://attacker.example/auth/callback", f.verifier)
		require.Error(t, err)
		for _, key := range f.redis.Keys() {
			if strings.HasPrefix(key, "canvas:code:") {
				raw := f.redis.HGet(key, "payload")
				var value map[string]any
				require.NoError(t, common.UnmarshalJsonStr(raw, &value))
				value["expires_at"] = time.Now().Add(-time.Second).Unix()
				encoded, err := common.Marshal(value)
				require.NoError(t, err)
				f.redis.HSet(key, "payload", string(encoded))
			}
		}
		_, err = service.ExchangeCanvasCode(context.Background(), code, "https://canvas.example.test/auth/callback", f.verifier)
		require.Error(t, err)
	})
}

func TestCanvasAuthorizeRequiresBrowserSessionAndRegisteredCallback(t *testing.T) {
	f := newCanvasFixture(t)
	jwt, _, err := service.IssueAccessToken(f.identity)
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(f.verifier))
	request := map[string]any{"client_id": "infinite-canvas", "redirect_uri": "https://canvas.example.test/auth/callback", "state": strings.Repeat("state", 8), "code_challenge": base64.RawURLEncoding.EncodeToString(digest[:]), "code_challenge_method": "S256"}
	body, err := common.Marshal(request)
	require.NoError(t, err)
	response := f.request("POST", "/api/integrations/canvas/authorize", string(body), "Bearer "+jwt, "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "api_key")
	assert.Contains(t, response.Body.String(), "redirect_url")
	pat := strings.Repeat("a", 32)
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("access_token", pat).Error)
	assert.Equal(t, 401, f.request("POST", "/api/integrations/canvas/authorize", string(body), f.proof, "", "").Code)
	assert.Equal(t, 401, f.request("POST", "/api/integrations/canvas/authorize", string(body), "Bearer "+pat, "", "").Code)
	for _, change := range []struct {
		key   string
		value any
	}{{"redirect_uri", "https://attacker.example/auth/callback"}, {"client_id", "other-app"}, {"code_challenge_method", "plain"}, {"user_id", 99999}} {
		original, present := request[change.key]
		request[change.key] = change.value
		encoded, err := common.Marshal(request)
		require.NoError(t, err)
		assert.NotEqual(t, 200, f.request("POST", "/api/integrations/canvas/authorize", string(encoded), "Bearer "+jwt, "", "").Code)
		if present {
			request[change.key] = original
		} else {
			delete(request, change.key)
		}
	}
}

func TestCanvasOriginNormalizationPreservesOnlyEquivalentAuthorizations(t *testing.T) {
	for _, raw := range []string{"https://CANVAS.EXAMPLE.TEST", "https://canvas.example.test:443", "https://CANVAS.EXAMPLE.TEST:443/"} {
		t.Run(raw, func(t *testing.T) {
			f := newCanvasFixture(t)
			require.NoError(t, config.UpdateConfigFromMap(config.GlobalConfig.Get("infinite_canvas"), map[string]string{"url": raw}))
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest("GET", "/api/status", nil)
			GetStatus(c)
			require.Equal(t, 200, response.Code, response.Body.String())
			var status struct {
				Data struct {
					Canvas system_setting.InfiniteCanvasSettings `json:"infinite_canvas"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &status))
			assert.True(t, status.Data.Canvas.Enabled)
			assert.Equal(t, "https://canvas.example.test", status.Data.Canvas.URL)
			grant := f.grant(t) // Both authorization and exchange use the canonical fixed callback.
			assert.Equal(t, 200, f.request("GET", "/v1/models", "", "Bearer "+grant.APIKey, f.proof, "").Code)
			common.OptionMapRWMutex.Lock()
			previousOptions := maps.Clone(common.OptionMap)
			if common.OptionMap == nil {
				common.OptionMap = make(map[string]string)
			}
			common.OptionMapRWMutex.Unlock()
			t.Cleanup(func() {
				common.OptionMapRWMutex.Lock()
				common.OptionMap = previousOptions
				common.OptionMapRWMutex.Unlock()
			})
			require.NoError(t, model.UpdateOption("infinite_canvas.url", raw))
			assert.Equal(t, "https://canvas.example.test", system_setting.InfiniteCanvasSettingsSnapshot().URL)
			assert.Equal(t, 200, f.request("GET", "/v1/models", "", "Bearer "+grant.APIKey, f.proof, "").Code)
			var payload service.CanvasGrantState
			key := "canvas:grant:" + grant.GrantID
			require.NoError(t, common.UnmarshalJsonStr(f.redis.HGet(key, "payload"), &payload))
			payload.ConfigHash = common.GenerateHMACWithKey([]byte("infinite-canvas-v1:"+common.SessionSecret), "infinite-canvas\x00"+raw+"\x00"+system_setting.CanvasClientSecret())
			encoded, err := common.Marshal(payload)
			require.NoError(t, err)
			f.redis.HSet(key, "payload", string(encoded))
			assert.Equal(t, 401, f.request("GET", "/v1/models", "", "Bearer "+grant.APIKey, f.proof, "").Code, "legacy raw fingerprints require reauthorization")
			current := f.grant(t)
			for settingKey, value := range map[string]string{"url": "https://other.example.test", "client_id": "other-client"} {
				require.NoError(t, config.UpdateConfigFromMap(config.GlobalConfig.Get("infinite_canvas"), map[string]string{settingKey: value}))
				_, _, err := service.GetCanvasGrant(t.Context(), current.GrantID, false)
				require.Error(t, err)
				require.NoError(t, config.UpdateConfigFromMap(config.GlobalConfig.Get("infinite_canvas"), map[string]string{"url": "https://canvas.example.test", "client_id": "infinite-canvas"}))
			}
			t.Setenv("INFINITE_CANVAS_CLIENT_SECRET", strings.Repeat("changed", 8))
			_, _, err = service.GetCanvasGrant(t.Context(), current.GrantID, false)
			require.Error(t, err)
		})
	}
}

func TestCanvasCredentialAndRevocationBoundaries(t *testing.T) {
	for _, scenario := range []string{"missing application proof", "wrong application proof", "parent logout", "disabled user", "password version", "session version", "session expired", "token disabled", "token deleted", "token expired", "grant revoked", "subject replaced", "client replaced", "application disabled", "Redis loss", "idle timeout", "IP permission changed", "group permission changed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCanvasFixture(t)
			grant := f.grant(t)
			proof := f.proof
			switch scenario {
			case "missing application proof":
				proof = ""
			case "wrong application proof":
				proof = "Basic " + base64.StdEncoding.EncodeToString([]byte("infinite-canvas:wrong"))
			case "parent logout":
				require.NoError(t, f.db.Model(&model.UserSession{}).Where("sid = ?", f.identity.SessionID).Update("status", model.UserSessionStatusRevoked).Error)
			case "disabled user":
				require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("status", common.UserStatusDisabled).Error)
			case "password version":
				require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("auth_version", 2).Error)
			case "session version":
				require.NoError(t, f.db.Model(&model.UserSession{}).Where("sid = ?", f.identity.SessionID).Update("version", 2).Error)
			case "session expired":
				require.NoError(t, f.db.Model(&model.UserSession{}).Where("sid = ?", f.identity.SessionID).Update("expires_at", time.Now().Unix()-1).Error)
			case "token disabled":
				require.NoError(t, f.db.Model(&model.Token{}).Where("user_id = ?", f.user.Id).Update("status", common.TokenStatusDisabled).Error)
			case "token deleted":
				require.NoError(t, f.db.Where("user_id = ?", f.user.Id).Delete(&model.Token{}).Error)
			case "token expired":
				require.NoError(t, f.db.Model(&model.Token{}).Where("user_id = ?", f.user.Id).Update("expired_time", time.Now().Unix()-1).Error)
			case "grant revoked":
				require.NoError(t, service.RevokeCanvasGrant(t.Context(), grant.GrantID))
			case "subject replaced":
				require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("canvas_subject", "ics_"+common.GetUUID()).Error)
			case "client replaced":
				require.NoError(t, f.db.Model(&model.CanvasGrant{}).Where("id = ?", grant.GrantID).Update("client_id", "another-app").Error)
			case "application disabled":
				require.NoError(t, config.UpdateConfigFromMap(config.GlobalConfig.Get("infinite_canvas"), map[string]string{"enabled": "false"}))
			case "Redis loss":
				f.redis.FlushAll()
			case "idle timeout":
				f.redis.HSet("canvas:grant:"+grant.GrantID, "last_active", fmt.Sprint(time.Now().Add(-service.CanvasIdleTTL).Unix()))
			case "IP permission changed":
				require.NoError(t, f.db.Model(&model.Token{}).Where("user_id = ?", f.user.Id).Update("allow_ips", "198.51.100.0/24").Error)
			case "group permission changed":
				require.NoError(t, f.db.Model(&model.Token{}).Where("user_id = ?", f.user.Id).Update("group", "canvas-worker-forbidden-group").Error)
			}
			response := f.request("GET", "/v1/models", "", "Bearer "+grant.APIKey, proof, "")
			assert.NotEqual(t, 200, response.Code, response.Body.String())
			if !strings.Contains(scenario, "application proof") {
				var record model.CanvasGrant
				require.NoError(t, f.db.First(&record, "id = ?", grant.GrantID).Error)
				internal := gin.New()
				internal.POST("/v1/videos", middleware.CanvasAsyncMediaWorkerAuth(record.TokenID, f.user.Id), func(c *gin.Context) { c.Status(http.StatusNoContent) })
				request := httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
				request.RemoteAddr = "192.0.2.1:0"
				workerResponse := httptest.NewRecorder()
				internal.ServeHTTP(workerResponse, request)
				assert.NotEqual(t, http.StatusNoContent, workerResponse.Code, "worker must reject "+scenario)
			}
		})
	}
	t.Run("owner context and stripped proof; scopes never expose logs or stats", func(t *testing.T) {
		f := newCanvasFixture(t)
		grant := f.grant(t)
		response := f.request("GET", "/v1/models", "", "Bearer "+grant.APIKey, f.proof, grant.GrantID)
		require.Equal(t, 200, response.Code, response.Body.String())
		var identity struct {
			ID    int    `json:"id"`
			Proof string `json:"proof"`
			Grant string `json:"grant_header"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &identity))
		assert.Equal(t, f.user.Id, identity.ID)
		assert.Empty(t, identity.Proof)
		assert.Empty(t, identity.Grant)
		for _, path := range []string{"/api/log/token", "/v1/images/tasks_async/stats"} {
			assert.Equal(t, 401, f.request("GET", path, "", "Bearer "+grant.APIKey, f.proof, "").Code)
		}
		assert.Equal(t, 401, f.request("GET", "/v1/models?key="+url.QueryEscape(grant.APIKey), "", "", f.proof, "").Code)
		assert.Equal(t, 401, f.request("GET", "/v1/models", "", "Bearer "+grant.APIKey+"-1", f.proof, "").Code)
		assert.Equal(t, 401, f.request("GET", "/v1/models", "", "Bearer "+strings.Replace(grant.APIKey, "ic_", "IC_", 1), f.proof, "").Code)
		assert.Equal(t, 200, f.request("POST", "/v1/images/generations", `{"user_id":999999}`, "Bearer "+grant.APIKey, f.proof, "").Code)
	})
}

func TestCanvasHTTPAndManagedTokenProtection(t *testing.T) {
	f := newCanvasFixture(t)
	grant := f.grant(t)
	var token model.Token
	require.NoError(t, f.db.Where("user_id = ?", f.user.Id).First(&token).Error)
	for _, request := range []struct{ method, path, body string }{
		{"POST", fmt.Sprintf("/api/token/%d/key", token.Id), "{}"},
		{"POST", "/api/token/batch/keys", fmt.Sprintf(`{"ids":[%d]}`, token.Id)},
		{"PUT", "/api/token/", fmt.Sprintf(`{"id":%d,"expired_time":-1,"unlimited_quota":true}`, token.Id)},
	} {
		response := f.request(request.method, request.path, request.body, "", "", "")
		assert.NotContains(t, response.Body.String(), token.Key)
	}
	_, user, err := service.GetCanvasGrant(context.Background(), grant.GrantID, false)
	require.NoError(t, err)
	assert.Equal(t, f.user.Id, user.Id)
	response := f.request("POST", "/api/integrations/canvas/introspect", fmt.Sprintf(`{"grant_id":%q}`, grant.GrantID), f.proof, "", "")
	assert.Equal(t, 200, response.Code)
	assert.Contains(t, response.Body.String(), `"active":true`)
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	response = f.request("POST", "/api/integrations/canvas/revoke", fmt.Sprintf(`{"grant_id":%q}`, grant.GrantID), f.proof, "", "")
	assert.Equal(t, 204, response.Code, response.Body.String())
	assert.Equal(t, 401, f.request("GET", "/v1/models", "", "Bearer "+grant.APIKey, f.proof, "").Code)
}

func TestCanvasModelListUsesCurrentPermissions(t *testing.T) {
	initModelListColumnNames(t)
	f := newCanvasFixture(t)
	require.NoError(t, f.db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Model{}, &model.Vendor{}))
	previous := operation_setting.SelfUseModeEnabled
	operation_setting.SelfUseModeEnabled = true
	t.Cleanup(func() { operation_setting.SelfUseModeEnabled = previous })
	require.NoError(t, f.db.Create(&[]model.Ability{{Group: "default", Model: "canvas-default", ChannelId: 1, Enabled: true}, {Group: "premium", Model: "canvas-premium", ChannelId: 2, Enabled: true}, {Group: "default", Model: "canvas-disabled", ChannelId: 3, Enabled: false}}).Error)
	grant := f.grant(t)
	response := f.request("GET", "/v1/models?catalog=1", "", "Bearer "+grant.APIKey, f.proof, "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "canvas-default")
	assert.NotContains(t, response.Body.String(), "canvas-premium")
	assert.NotContains(t, response.Body.String(), "canvas-disabled")
	// Subscription group transitions can preserve AuthVersion; access must still
	// follow the authoritative current group on the next request.
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("group", "premium").Error)
	response = f.request("GET", "/v1/models?catalog=1", "", "Bearer "+grant.APIKey, f.proof, "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "canvas-premium")
	assert.NotContains(t, response.Body.String(), "canvas-default")
	require.NoError(t, f.db.Model(&model.Token{}).Where("user_id = ?", f.user.Id).Updates(map[string]any{"model_limits_enabled": true, "model_limits": "canvas-default"}).Error)
	response = f.request("GET", "/v1/models?catalog=1", "", "Bearer "+grant.APIKey, f.proof, "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "canvas-premium")
}

func TestCanvasHistoricalTaskProvenanceSurvivesReauthorization(t *testing.T) {
	f := newCanvasFixture(t)
	old := f.grant(t)
	var oldGrant model.CanvasGrant
	require.NoError(t, f.db.First(&oldGrant, "id = ?", old.GrantID).Error)
	task := model.AsyncImageTask{TaskId: "asyncimg_history", UserId: f.user.Id, TokenId: oldGrant.TokenID, Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ResultCount: 1, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	require.NoError(t, f.db.Create(&task).Error)
	require.NoError(t, service.RevokeCanvasGrant(context.Background(), old.GrantID))
	current := f.grant(t)
	assert.Equal(t, old.User.Subject, current.User.Subject, "reauthorization preserves the account subject")
	response := f.request("GET", "/api/integrations/canvas/history/images/"+task.TaskId, "", f.proof, "", current.GrantID)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), service.CanvasContentPath("images", task.TaskId, "0"))
	assert.NotContains(t, response.Body.String(), "signature=")
	ordinary := model.Token{UserId: f.user.Id, Key: "ordinary" + common.GetUUID()}
	require.NoError(t, f.db.Create(&ordinary).Error)
	task.TaskId = "asyncimg_not_canvas"
	task.Id = 0
	task.TokenId = ordinary.Id
	require.NoError(t, f.db.Create(&task).Error)
	assert.Equal(t, 404, f.request("GET", "/api/integrations/canvas/history/images/"+task.TaskId, "", f.proof, "", current.GrantID).Code)
	// A different account incarnation reusing the same numeric ID must get a
	// different subject and cannot claim the original application's data.
	newSubject := "ics_" + common.GetUUID()
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("canvas_subject", newSubject).Error)
	_, _, err := service.GetCanvasGrant(context.Background(), current.GrantID, false)
	require.Error(t, err)
	replacement := f.grant(t)
	assert.Equal(t, newSubject, replacement.User.Subject)
	assert.NotEqual(t, old.User.Subject, replacement.User.Subject)
	assert.Equal(t, 404, f.request("GET", "/api/integrations/canvas/history/images/asyncimg_history", "", f.proof, "", replacement.GrantID).Code)
	task.TaskId = "asyncimg_other_user"
	task.Id = 0
	task.UserId = f.user.Id + 999
	task.TokenId = oldGrant.TokenID
	require.NoError(t, f.db.Create(&task).Error)
	assert.Equal(t, 404, f.request("GET", "/api/integrations/canvas/history/images/"+task.TaskId, "", f.proof, "", replacement.GrantID).Code)
	// BFF polling does not renew the idle deadline, while an explicit foreground
	// interaction does; an expired deadline cannot be revived by activity=true.
	f.redis.HSet("canvas:grant:"+replacement.GrantID, "last_active", fmt.Sprint(time.Now().Add(-service.CanvasIdleTTL).Unix()))
	_, _, err = service.GetCanvasGrant(context.Background(), replacement.GrantID, true)
	require.Error(t, err)
}

func TestCanvasUpstreamImageManifestUsesPrivateContentBridge(t *testing.T) {
	f := newCanvasFixture(t)
	grant := f.grant(t)
	var record model.CanvasGrant
	require.NoError(t, f.db.First(&record, "id = ?", grant.GrantID).Error)
	require.NoError(t, f.db.AutoMigrate(&model.AsyncImageEvent{}, &model.Option{}))
	task := model.AsyncImageTask{TaskId: "asyncimg_upstream_canvas", UserId: f.user.Id, TokenId: record.TokenID, Status: model.ImageTaskInvoking, UpstreamTaskId: "provider-result", LeaseToken: "manifest-lease", LeaseExpiresAt: time.Now().Add(time.Minute).Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()}
	require.NoError(t, f.db.Create(&task).Error)
	providerURL := "https://cdn.example.com/image.png?signature=provider-result-canary"
	require.NoError(t, model.RecordAsyncImageUpstreamResult(t.Context(), task, []string{providerURL}))
	require.NoError(t, f.db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Updates(map[string]any{"status": model.ImageTaskSucceeded, "billing_status": "not_billable", "result_count": 1, "image_count": 1, "result_source": model.ImageResultSourceUpstream}).Error)
	path := "/api/integrations/canvas/history/images/" + task.TaskId
	contentPath := service.CanvasContentPath("images", task.TaskId, "0")
	response := f.request("GET", path, "", f.proof, "", grant.GrantID)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), contentPath)
	assert.NotContains(t, response.Body.String(), providerURL)
	assert.NotContains(t, response.Body.String(), "provider-result-canary")
	for _, prefix := range []string{"/v1/images/tasks_async/", "/v1/media/tasks_async/"} {
		response = f.request("GET", prefix+task.TaskId, "", "Bearer "+grant.APIKey, f.proof, "")
		require.Equal(t, 200, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), contentPath)
		assert.NotContains(t, response.Body.String(), "provider-result-canary")
	}
	jwt, _, err := service.IssueAccessToken(f.identity)
	require.NoError(t, err)
	response = f.request("GET", "/api/user/async-image-tasks/"+task.TaskId, "", "Bearer "+jwt, "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"authenticated":true`)
	assert.NotContains(t, response.Body.String(), "provider-result-canary")
	assert.Equal(t, 404, f.request("GET", service.CanvasContentPath("images", task.TaskId, "1"), "", f.proof, "", grant.GrantID).Code)
	// Both the public-manifest gate and the existing HTTPS downloader must
	// reject unsafe persisted inputs before any outbound network access.
	for _, target := range []string{"https://127.0.0.1/private", "http://8.8.8.8/image.png"} {
		encoded, err := common.Marshal([]string{target})
		require.NoError(t, err)
		require.NoError(t, f.db.Model(&model.AsyncImageEvent{}).Where("task_id = ?", task.TaskId).Update("message", string(encoded)).Error)
		for _, method := range []string{"GET", "HEAD"} {
			response = f.request(method, contentPath, "", f.proof, "", grant.GrantID)
			assert.Equal(t, 503, response.Code, response.Body.String())
			assert.Empty(t, response.Body.String())
			assert.Empty(t, response.Header().Get("Location"))
		}
	}
	require.NoError(t, f.db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Update("expires_at", time.Now().Add(-time.Second).Unix()).Error)
	assert.Equal(t, 404, f.request("GET", contentPath, "", f.proof, "", grant.GrantID).Code)
	require.NoError(t, f.db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Update("expires_at", time.Now().Add(time.Hour).Unix()).Error)
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("canvas_subject", "ics_"+common.GetUUID()).Error)
	replacement := f.grant(t)
	assert.Equal(t, 404, f.request("GET", path, "", f.proof, "", replacement.GrantID).Code)
	assert.Equal(t, 404, f.request("GET", contentPath, "", f.proof, "", replacement.GrantID).Code)
}

func TestCanvasImageContentAuthorizationAndOrdinaryKeyCompatibility(t *testing.T) {
	f := newCanvasFixture(t)
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	grant := f.grant(t)
	var record model.CanvasGrant
	require.NoError(t, f.db.First(&record, "id = ?", grant.GrantID).Error)
	profile := model.ImageStorageProfile{ProfileId: "canvas-results", Class: "temporary", Backend: "local", Root: t.TempDir(), Active: true}
	require.NoError(t, f.db.Create(&profile).Error)
	content := []byte("private-canvas-image-content")
	require.NoError(t, os.WriteFile(filepath.Join(profile.Root, "result.png"), content, 0600))
	object := model.ImageStorageObject{ObjectId: "canvas-result", ProfileId: profile.ProfileId, ObjectKey: "result.png", ContentType: "image/png", ByteSize: int64(len(content)), Status: "active", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	require.NoError(t, f.db.Create(&object).Error)
	task := model.AsyncImageTask{TaskId: "asyncimg_canvas_content", UserId: f.user.Id, TokenId: record.TokenID, Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ResultCount: 1, ExpiresAt: object.ExpiresAt}
	require.NoError(t, f.db.Create(&task).Error)
	require.NoError(t, f.db.Create(&model.AsyncImageResult{TaskId: task.TaskId, ImageIndex: 0, ObjectId: object.ObjectId}).Error)
	queryPath := "/v1/images/tasks_async/" + task.TaskId
	response := f.request("GET", queryPath, "", "Bearer "+grant.APIKey, f.proof, "")
	require.Equal(t, 200, response.Code, response.Body.String())
	var manifest struct {
		Status string `json:"status"`
		Data   []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &manifest))
	require.Equal(t, "succeeded", manifest.Status)
	require.Len(t, manifest.Data, 1)
	contentPath := manifest.Data[0].URL
	assert.Equal(t, service.CanvasContentPath("images", task.TaskId, "0"), contentPath)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		response = f.request(method, contentPath, "", f.proof, "", grant.GrantID)
		require.Equal(t, 200, response.Code, response.Body.String())
		assert.Contains(t, response.Header().Get("Cache-Control"), "no-store")
		assert.Equal(t, "image/png", response.Header().Get("Content-Type"))
		if method == http.MethodGet {
			assert.Equal(t, content, response.Body.Bytes())
		} else {
			assert.Empty(t, response.Body.Bytes())
		}
	}
	assert.Equal(t, 401, f.request("GET", contentPath, "", "", "", grant.GrantID).Code)
	signed, err := service.ImageObjectURL(t.Context(), object, "", 3600)
	require.NoError(t, err)
	assert.Equal(t, 404, f.request("GET", signed, "", "", "", "").Code)
	jwt, _, err := service.IssueAccessToken(f.identity)
	require.NoError(t, err)
	viewPath := "/api/user/async-image-tasks/" + task.TaskId + "/results/0/view"
	response = f.request("GET", viewPath, "", "Bearer "+jwt, "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Equal(t, content, response.Body.Bytes())
	assert.Empty(t, response.Header().Get("Location"))
	// Local uploads may legitimately exceed the independently configured remote
	// download limit. They remain readable by upstreams and the owner's dashboard.
	input := model.ImageInputObject{InputId: "sci_canvas_content", TokenId: record.TokenID, ObjectId: object.ObjectId, IntentKey: "canvas-content-input", Status: "active", ExpiresAt: object.ExpiresAt}
	require.NoError(t, f.db.Create(&input).Error)
	require.NoError(t, f.db.Create(&model.ImageUploadIntent{IntentKey: input.IntentKey, TokenId: record.TokenID, UserId: f.user.Id, ProfileId: object.ProfileId, ObjectKey: object.ObjectKey}).Error)
	grantState, _, err := service.GetCanvasGrant(t.Context(), grant.GrantID, false)
	require.NoError(t, err)
	inputPath := service.CanvasInputURL("", input.InputId, grantState, 3600)
	cfg := service.DefaultImageRuntimeConfig()
	cfg.DownloadMaxBytes = 1
	require.NoError(t, service.SaveImageRuntimeConfig(t.Context(), cfg))
	response = f.request("GET", inputPath, "", "", "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Equal(t, content, response.Body.Bytes())
	response = f.request("GET", viewPath, "", "Bearer "+jwt, "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Equal(t, content, response.Body.Bytes())
	require.NoError(t, service.SaveImageRuntimeConfig(t.Context(), service.DefaultImageRuntimeConfig()))
	jsonRequest := httptest.NewRequest("GET", viewPath, nil)
	jsonRequest.Header.Set("Authorization", "Bearer "+jwt)
	jsonRequest.Header.Set("Accept", "application/json")
	response = httptest.NewRecorder()
	f.router.ServeHTTP(response, jsonRequest)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"url":"`+viewPath+`"`)
	assert.Contains(t, response.Body.String(), `"authenticated":true`)
	assert.NotContains(t, response.Body.String(), "signature=")
	response = f.request("GET", "/api/user/async-image-tasks/"+task.TaskId, "", "Bearer "+jwt, "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"authenticated":true`)
	assert.NotContains(t, response.Body.String(), "signature=")
	// S3 results use the same private projection; signing must never escape in
	// either the public poll response or the dashboard result manifest.
	credential, err := common.Marshal(service.ImageStorageCredentials{AccessKey: "canvas-s3-test", SecretKey: "canvas-s3-test-secret"})
	require.NoError(t, err)
	cipher, err := service.EncryptImagePayload(credential, "storage:"+profile.ProfileId)
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&profile).Updates(map[string]any{"backend": "s3", "bucket": "canvas-private", "region": "us-east-1", "credential_cipher": cipher}).Error)
	s3Signed, err := service.ImageObjectURL(t.Context(), object, "", 3600)
	require.NoError(t, err)
	assert.Contains(t, s3Signed, "X-Amz-Signature=")
	for _, path := range []string{queryPath, "/api/user/async-image-tasks/" + task.TaskId} {
		authorization, proof := "Bearer "+grant.APIKey, f.proof
		if strings.HasPrefix(path, "/api/") {
			authorization, proof = "Bearer "+jwt, ""
		}
		response = f.request("GET", path, "", authorization, proof, "")
		require.Equal(t, 200, response.Code, response.Body.String())
		assert.NotContains(t, response.Body.String(), "X-Amz-")
		assert.NotContains(t, response.Body.String(), "amazonaws.com")
	}
	require.NoError(t, f.db.Model(&profile).Update("backend", "local").Error)
	admin := model.User{Username: "canvas-admin", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1}
	require.NoError(t, f.db.Create(&admin).Error)
	adminIdentity := service.AuthIdentity{UserID: admin.Id, SessionID: "canvas-admin-parent", UserAuthVersion: 1, SessionVersion: 1}
	require.NoError(t, f.db.Create(&model.UserSession{SID: adminIdentity.SessionID, UserID: admin.Id, Version: 1, UserAuthVersion: 1, Status: model.UserSessionStatusActive, RefreshHash: "admin-refresh-hash", ExpiresAt: object.ExpiresAt}).Error)
	adminJWT, _, err := service.IssueAccessToken(adminIdentity)
	require.NoError(t, err)
	adminView := "/api/admin/async-image-tasks/" + task.TaskId + "/results/0/view"
	response = f.request("GET", adminView, "", "Bearer "+adminJWT, "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Equal(t, content, response.Body.Bytes())
	assert.Equal(t, 404, f.request("GET", viewPath, "", "Bearer "+adminJWT, "", "").Code)
	assert.Equal(t, 401, f.request("GET", viewPath, "", "", "", "").Code)
	require.NoError(t, service.RevokeCanvasGrant(t.Context(), grant.GrantID))
	assert.Equal(t, 401, f.request("GET", contentPath, "", f.proof, "", grant.GrantID).Code)
	assert.Equal(t, 200, f.request("GET", viewPath, "", "Bearer "+jwt, "", "").Code, "Canvas revocation preserves the owner's dashboard access")
	current := f.grant(t)
	assert.Equal(t, 200, f.request("GET", queryPath, "", "Bearer "+current.APIKey, f.proof, "").Code)
	assert.Equal(t, 200, f.request("GET", contentPath, "", f.proof, "", current.GrantID).Code)

	ordinary := model.Token{UserId: f.user.Id, Key: "ordinary" + common.GetUUID(), Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true, Group: "default"}
	require.NoError(t, f.db.Create(&ordinary).Error)
	ordinaryObject := object
	ordinaryObject.Id, ordinaryObject.ObjectId, ordinaryObject.IdentityHash, ordinaryObject.ObjectKey = 0, "ordinary-result", "ordinary-result-identity", "ordinary.png"
	require.NoError(t, os.WriteFile(filepath.Join(profile.Root, ordinaryObject.ObjectKey), content, 0600))
	require.NoError(t, f.db.Create(&ordinaryObject).Error)
	ordinaryTask := task
	ordinaryTask.Id, ordinaryTask.TaskId, ordinaryTask.TokenId = 0, "asyncimg_ordinary_content", ordinary.Id
	require.NoError(t, f.db.Create(&ordinaryTask).Error)
	require.NoError(t, f.db.Create(&model.AsyncImageResult{TaskId: ordinaryTask.TaskId, ImageIndex: 0, ObjectId: ordinaryObject.ObjectId}).Error)
	response = f.request("GET", "/v1/images/tasks_async/"+ordinaryTask.TaskId, "", "Bearer "+ordinary.Key, "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &manifest))
	require.Len(t, manifest.Data, 1)
	assert.Contains(t, manifest.Data[0].URL, "/api/image-objects/ordinary-result/content?")
	response = f.request("GET", manifest.Data[0].URL, "", "", "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Equal(t, content, response.Body.Bytes())
	assert.Equal(t, 307, f.request("GET", "/api/user/async-image-tasks/"+ordinaryTask.TaskId+"/results/0/view", "", "Bearer "+jwt, "", "").Code)
	assert.Equal(t, 404, f.request("GET", "/v1/images/tasks_async/"+ordinaryTask.TaskId, "", "Bearer "+current.APIKey, f.proof, "").Code)
	assert.Equal(t, 404, f.request("GET", queryPath, "", "Bearer "+ordinary.Key, "", "").Code)

	listPath := "/api/user/async-image-tasks?start_date=2000-01-01&end_date=2100-01-01"
	response = f.request("GET", listPath, "", "Bearer "+jwt, "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	var listing struct {
		Data struct {
			Total int `json:"total"`
			Items []struct {
				TaskID string `json:"task_id"`
			} `json:"items"`
			Stats struct {
				Total int `json:"total"`
			} `json:"stats"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &listing))
	assert.Equal(t, 2, listing.Data.Total)
	assert.Equal(t, 2, listing.Data.Stats.Total)
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("canvas_subject", "ics_"+common.GetUUID()).Error)
	replacement := f.grant(t)
	assert.Equal(t, 404, f.request("GET", queryPath, "", "Bearer "+replacement.APIKey, f.proof, "").Code)
	assert.Equal(t, 404, f.request("GET", contentPath, "", f.proof, "", replacement.GrantID).Code)
	assert.Equal(t, 404, f.request("GET", viewPath, "", "Bearer "+jwt, "", "").Code)
	assert.Equal(t, 404, f.request("GET", adminView, "", "Bearer "+adminJWT, "", "").Code)
	for _, subject := range []any{replacement.User.Subject, nil} {
		require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("canvas_subject", subject).Error)
		response = f.request("GET", listPath, "", "Bearer "+jwt, "", "")
		require.Equal(t, 200, response.Code, response.Body.String())
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &listing))
		assert.Equal(t, 1, listing.Data.Total)
		assert.Equal(t, 1, listing.Data.Stats.Total)
		require.Len(t, listing.Data.Items, 1)
		assert.Equal(t, ordinaryTask.TaskId, listing.Data.Items[0].TaskID)
	}
}

func TestCanvasInternalCredentialsDoNotConsumeManualKeySlots(t *testing.T) {
	f := newCanvasFixture(t)
	_ = f.grant(t)
	manual := model.Token{UserId: f.user.Id, Key: "ordinary" + common.GetUUID(), Name: "Infinite Canvas", UnlimitedQuota: true}
	require.NoError(t, f.db.Create(&manual).Error)
	count, err := model.CountUserTokens(f.user.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
	listed, err := model.GetAllUserTokens(f.user.Id, 0, 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, manual.Id, listed[0].Id)
	searched, total, err := model.SearchUserTokens(f.user.Id, "Infinite Canvas", "", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, searched, 1)
	assert.Equal(t, manual.Id, searched[0].Id)
}

func TestCanvasWalletCacheAndAccountReuse(t *testing.T) {
	f := newCanvasFixture(t)
	grant := f.grant(t)
	previousBatch := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatch })
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("quota", 1000).Error)
	f.redis.HSet(fmt.Sprintf("user:%d", f.user.Id), "Quota", "700")
	require.NoError(t, model.AdjustCanvasWallet(f.user.Id, grant.User.Subject, -100, true))
	assert.Equal(t, "600", f.redis.HGet(fmt.Sprintf("user:%d", f.user.Id), "Quota"), "managed calls preserve pending ordinary cache deltas")
	var owner model.User
	require.NoError(t, f.db.First(&owner, f.user.Id).Error)
	assert.Equal(t, 900, owner.Quota)
	require.ErrorIs(t, model.AdjustCanvasWallet(f.user.Id, grant.User.Subject, -601, true), model.ErrCanvasWalletInsufficient)
	replacement := "ics_" + common.GetUUID()
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("canvas_subject", replacement).Error)
	for _, delta := range []int{-50, 50} {
		require.ErrorIs(t, model.AdjustCanvasWallet(f.user.Id, grant.User.Subject, delta, false), model.ErrCanvasBillingOwner)
	}
	assert.Equal(t, "600", f.redis.HGet(fmt.Sprintf("user:%d", f.user.Id), "Quota"))
	require.NoError(t, f.db.First(&owner, f.user.Id).Error)
	assert.Equal(t, 900, owner.Quota)
}

func TestCanvasWalletProjectionNeverCreditsRebuiltCacheTwice(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%t", rollback), func(t *testing.T) {
			f := newCanvasFixture(t)
			grant := f.grant(t)
			key := fmt.Sprintf("user:%d", f.user.Id)
			require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("quota", 100).Error)
			f.redis.HSet(key, "Quota", "100")
			var injected atomic.Bool
			common.RDB.AddHook(canvasQuotaProjectionHook{before: func(cmd redis.Cmder) {
				args := cmd.Args()
				if len(args) < 9 || cmd.Name() != "eval" || fmt.Sprint(args[3]) != key || !strings.Contains(fmt.Sprint(args[1]), "'Quota', ARGV[1]") {
					return
				}
				delta, err := strconv.Atoi(fmt.Sprint(args[4]))
				if err != nil || delta <= 0 || !injected.CompareAndSwap(false, true) {
					return
				}
				f.redis.Del(key)
				rebuilt, err := model.GetUserCache(f.user.Id)
				require.NoError(t, err)
				if rollback {
					assert.Equal(t, 100, rebuilt.Quota)
				} else {
					assert.Equal(t, 140, rebuilt.Quota)
				}
			}})
			if rollback {
				var failed atomic.Bool
				require.NoError(t, f.db.Callback().Update().Before("gorm:update").Register("canvas_wallet_failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "users" && failed.CompareAndSwap(false, true) {
						tx.AddError(fmt.Errorf("injected wallet write failure"))
					}
				}))
				defer f.db.Callback().Update().Remove("canvas_wallet_failure")
				err := model.AdjustCanvasWallet(f.user.Id, grant.User.Subject, -40, true)
				require.Error(t, err)
			} else {
				require.NoError(t, model.AdjustCanvasWallet(f.user.Id, grant.User.Subject, 40, false))
			}
			require.True(t, injected.Load(), "fault injection must occur at the delayed positive quota projection")
			var owner model.User
			require.NoError(t, f.db.First(&owner, f.user.Id).Error)
			assert.Equal(t, strconv.Itoa(owner.Quota), f.redis.HGet(key, "Quota"), "cache rebuilt from final SQL must not receive the old generation's credit")
		})
	}
}

func TestCanvasColdCacheRechecksBeforePublishingBalance(t *testing.T) {
	for _, initiallyBound := range []bool{true, false} {
		t.Run(fmt.Sprintf("initially_bound=%t", initiallyBound), func(t *testing.T) {
			f := newCanvasFixture(t)
			grant := f.grant(t)
			require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("quota", 100).Error)
			if !initiallyBound {
				require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("canvas_subject", nil).Error)
			}
			f.redis.Del(fmt.Sprintf("user:%d", f.user.Id))
			var changed atomic.Bool
			require.NoError(t, f.db.Callback().Query().After("gorm:query").Register("canvas_concurrent_funding", func(tx *gorm.DB) {
				if tx.Statement.Table != "users" || !changed.CompareAndSwap(false, true) {
					return
				}
				// Complete funding immediately after the initial, unlocked SQL read.
				// Cold publication must re-read under its own funding row lock.
				require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Updates(map[string]any{"quota": 60, "canvas_subject": grant.User.Subject}).Error)
			}))
			defer f.db.Callback().Query().Remove("canvas_concurrent_funding")
			cache, err := model.GetUserCache(f.user.Id)
			require.NoError(t, err)
			require.True(t, changed.Load())
			assert.Equal(t, 60, cache.Quota)
			assert.Equal(t, grant.User.Subject, cache.CanvasSubject)
			assert.Equal(t, "60", f.redis.HGet(fmt.Sprintf("user:%d", f.user.Id), "Quota"))
		})
	}
}

func runCanvasFundingOwnership(t *testing.T, db *gorm.DB, user model.User, token model.Token, grant model.CanvasGrant) {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}, &model.Task{}, &model.AsyncImageTask{}, &model.AsyncImageBill{}, &model.AsyncImageResult{}, &model.Channel{}))
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", user.Id).Updates(map[string]any{"canvas_subject": grant.Subject, "quota": 1000, "used_quota": 100}).Error)
	require.NoError(t, model.AdjustCanvasWallet(user.Id, grant.Subject, -100, true))
	require.NoError(t, model.AdjustCanvasWallet(user.Id, grant.Subject, 40, false))
	plan := model.SubscriptionPlan{Title: "Canvas ownership", QuotaResetPeriod: "never", TotalAmount: 1000}
	require.NoError(t, db.Create(&plan).Error)
	sub := model.UserSubscription{UserId: user.Id, PlanId: plan.Id, AmountTotal: 1000, Status: "active", EndTime: time.Now().Add(time.Hour).Unix()}
	require.NoError(t, db.Create(&sub).Error)
	requestID := "canvas-funding-" + common.GetUUID()
	_, err := model.PreConsumeUserSubscription(requestID, user.Id, "canvas", 0, 100, grant.Subject)
	require.NoError(t, err)
	require.NoError(t, model.PostConsumeCanvasSubscriptionDelta(sub.Id, 20, grant.Subject))
	task := model.Task{TaskID: "task_" + common.GetUUID(), UserId: user.Id, Status: model.TaskStatusInProgress, Quota: 60, PrivateData: model.TaskPrivateData{TokenId: token.Id, DurableBilling: true, SubmitAccountingRecorded: true}}
	require.NoError(t, db.Create(&task).Error)
	image := model.AsyncImageTask{TaskId: "asyncimg_" + common.GetUUID(), UserId: user.Id, TokenId: token.Id, Status: model.ImageTaskFailed}
	require.NoError(t, db.Create(&image).Error)
	bill := model.AsyncImageBill{TaskId: image.TaskId, BillingRequestId: "bill_" + common.GetUUID(), UserId: user.Id, TokenId: token.Id, ReservedQuota: 60, ReservedAt: time.Now().Unix(), Status: "reserved", FundingSource: "wallet"}
	require.NoError(t, db.Create(&bill).Error)
	replacement := "ics_" + common.GetUUID()
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", user.Id).Update("canvas_subject", replacement).Error)
	for _, delta := range []int{-50, 50} {
		require.ErrorIs(t, model.AdjustCanvasWallet(user.Id, grant.Subject, delta, false), model.ErrCanvasBillingOwner)
	}
	_, err = model.PreConsumeUserSubscription("new_"+common.GetUUID(), user.Id, "canvas", 0, 50, grant.Subject)
	require.ErrorIs(t, err, model.ErrCanvasBillingOwner)
	for _, delta := range []int64{-50, 50} {
		require.ErrorIs(t, model.PostConsumeCanvasSubscriptionDelta(sub.Id, delta, grant.Subject), model.ErrCanvasBillingOwner)
	}
	require.ErrorIs(t, model.RefundCanvasSubscriptionPreConsume(requestID, grant.Subject), model.ErrCanvasBillingOwner)
	require.ErrorIs(t, model.RefundAsyncImageReservation(context.Background(), image.TaskId), model.ErrCanvasBillingOwner)
	task.Status = model.TaskStatusFailure
	_, _, err = model.SettleTaskBilling(context.Background(), &task, model.TaskStatusInProgress, 0, nil, "")
	require.ErrorIs(t, err, model.ErrCanvasBillingOwner)
	require.ErrorIs(t, model.UpdateCanvasUsage(context.Background(), user.Id, grant.Subject, 50, 1), model.ErrCanvasBillingOwner)
	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	assert.Equal(t, 940, stored.Quota, "neither debit nor refund can cross an account incarnation")
	assert.Equal(t, 100, stored.UsedQuota)
	require.NoError(t, db.First(&sub, sub.Id).Error)
	assert.EqualValues(t, 120, sub.AmountUsed)
	require.NoError(t, db.First(&bill, bill.Id).Error)
	assert.Equal(t, "reserved", bill.Status, "failed ownership keeps the ledger pending")
	require.NoError(t, db.First(&task, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusInProgress, task.Status)
	assert.Equal(t, 60, task.Quota)
}

func TestCanvasNativeTaskOwnershipAndUserPagination(t *testing.T) {
	f := newCanvasFixture(t)
	old := f.grant(t)
	var record model.CanvasGrant
	require.NoError(t, f.db.First(&record, "id = ?", old.GrantID).Error)
	require.NoError(t, f.db.AutoMigrate(&model.Task{}))
	managed := model.Task{TaskID: "old-managed-video", UserId: f.user.Id, PrivateData: model.TaskPrivateData{TokenId: record.TokenID}}
	ordinary := model.Task{TaskID: "ordinary-video", UserId: f.user.Id}
	require.NoError(t, f.db.Create(&managed).Error)
	require.NoError(t, f.db.Create(&ordinary).Error)
	f.router.GET("/user-tasks", func(c *gin.Context) { c.Set("id", f.user.Id) }, GetUserTask)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Set("id", f.user.Id)
	assert.True(t, service.CanvasTaskAccess(c, &managed), "same account ordinary credential retains own task access")
	response := f.request("GET", "/user-tasks?ps=1", "", "", "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"total":2`)
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("canvas_subject", "ics_"+common.GetUUID()).Error)
	assert.False(t, service.CanvasTaskAccess(c, &managed))
	assert.True(t, service.CanvasTaskAccess(c, &ordinary))
	response = f.request("GET", "/user-tasks?ps=1", "", "", "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"total":1`)
	assert.Contains(t, response.Body.String(), ordinary.TaskID)
	assert.NotContains(t, response.Body.String(), managed.TaskID)
	current := f.grant(t)
	state, _, err := service.GetCanvasGrant(context.Background(), current.GrantID, false)
	require.NoError(t, err)
	c.Set(service.CanvasGrantContextKey, state)
	assert.False(t, service.CanvasTaskAccess(c, &managed))
}

func TestCanvasHardDeleteRetainsOnlyManagedTokenAnchors(t *testing.T) {
	f := newCanvasFixture(t)
	grant := f.grant(t)
	require.NoError(t, f.db.AutoMigrate(&model.ExternalIdentityClaim{}, &model.TwoFABackupCode{}, &model.TwoFA{}, &model.AuthFlow{}, &model.PasskeyCredential{}, &model.UserOAuthBinding{}))
	ordinary := model.Token{UserId: f.user.Id, Key: "ordinary-delete-" + common.GetUUID()}
	require.NoError(t, f.db.Create(&ordinary).Error)
	var record model.CanvasGrant
	require.NoError(t, f.db.First(&record, "id = ?", grant.GrantID).Error)
	require.NoError(t, model.HardDeleteUserById(f.user.Id))
	var token model.Token
	require.NoError(t, f.db.Unscoped().First(&token, record.TokenID).Error)
	assert.Equal(t, common.TokenStatusDisabled, token.Status)
	assert.True(t, strings.HasPrefix(token.Key, "ic_revoked_"))
	assert.NotEqual(t, strings.TrimPrefix(grant.APIKey, "sk-"), token.Key)
	assert.ErrorIs(t, f.db.Unscoped().First(&model.Token{}, ordinary.Id).Error, gorm.ErrRecordNotFound)
	subject, err := model.CanvasBillingSubject(f.db, record.TokenID, f.user.Id)
	require.NoError(t, err)
	assert.Equal(t, grant.User.Subject, subject)
}

func TestCanvasAsyncVideoWorkerLifecycle(t *testing.T) {
	f := newCanvasFixture(t)
	f.router.GET("/api/task/:task_id/artifacts", middleware.UserAuth(), GetDashboardTaskArtifacts)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		f.router.Handle(method, "/v1/videos/:task_id/content", middleware.TokenAuth(), VideoProxy)
		f.router.Handle(method, "/v1/tasks/:key/artifacts/:artifact_key/content", middleware.TokenAuth(), TaskArtifactContent)
	}
	previousSubmit, previousPersist := service.SubmitAsyncVideoFunc, service.PersistAsyncVideoFunc
	previousBatch, previousLogs, previousMemory := common.BatchUpdateEnabled, common.LogConsumeEnabled, common.MemoryCacheEnabled
	common.BatchUpdateEnabled, common.LogConsumeEnabled, common.MemoryCacheEnabled = false, false, false
	t.Cleanup(func() {
		service.SubmitAsyncVideoFunc, service.PersistAsyncVideoFunc = previousSubmit, previousPersist
		common.BatchUpdateEnabled, common.LogConsumeEnabled, common.MemoryCacheEnabled = previousBatch, previousLogs, previousMemory
	})
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "test")
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	require.NoError(t, f.db.AutoMigrate(&model.Task{}, &model.Channel{}, &model.Ability{}, &model.TokenImagePlatformMapping{}, &model.ImageGroupPolicy{}, &model.ImageChannelPool{}, &model.Log{}))
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("setting", `{"billing_preference":"wallet_only"}`).Error)
	grant := f.grant(t)
	var record model.CanvasGrant
	require.NoError(t, f.db.First(&record, "id = ?", grant.GrantID).Error)
	lastActive := fmt.Sprint(time.Now().Add(-time.Minute).Unix())
	f.redis.HSet("canvas:grant:"+grant.GrantID, "last_active", lastActive)
	internal := gin.New()
	for _, path := range []string{"/v1/videos", "/v1/videos/edits", "/v1/videos/extensions"} {
		internal.POST(path, middleware.CanvasAsyncMediaWorkerAuth(record.TokenID, f.user.Id), func(c *gin.Context) {
			assert.Equal(t, f.user.Id, c.GetInt("id"))
			assert.Equal(t, record.TokenID, c.GetInt("token_id"))
			assert.Equal(t, grant.User.Subject, c.GetString("canvas_subject"))
			assert.Equal(t, "default", common.GetContextKeyString(c, constant.ContextKeyUsingGroup))
			state, exists := c.Get(service.CanvasGrantContextKey)
			require.True(t, exists)
			assert.Equal(t, grant.GrantID, state.(*service.CanvasGrantState).ID)
			assert.Empty(t, c.GetHeader(service.CanvasClientProofHeader))
			c.Status(http.StatusNoContent)
		})
		response := httptest.NewRecorder()
		internal.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
	}
	assert.Equal(t, lastActive, f.redis.HGet("canvas:grant:"+grant.GrantID, "last_active"), "background work must not prolong user activity")
	_, _, _, err := service.ValidateCanvasWorkerCredential(t.Context(), record.TokenID, f.user.Id+1)
	require.ErrorIs(t, err, service.ErrCanvasAuthorization)
	var token model.Token
	require.NoError(t, f.db.First(&token, record.TokenID).Error)
	require.Error(t, validateMediaToken(t.Context(), token, "192.0.2.1"), "the anonymous media guard must still reject managed credentials")
	assert.Equal(t, http.StatusUnauthorized, f.request("GET", "/v1/models", "", "Bearer "+grant.APIKey, "", "").Code)

	const source = `
export const meta = {apiVersion:1,key:"canvas-worker-video",name:"Canvas worker video",version:"1.0.0",author:{name:"Test"},models:["canvas-worker-video"],fetchMode:"per_task",protocols:["openai_video"]};
export function buildSubmitRequest(ctx) { return {url:ctx.baseUrl+"/submit",method:"POST",headers:{Authorization:"Bearer "+ctx.apiKey},body:ctx.requestBody}; }
export function parseSubmitResponse(ctx,response) { return {taskId:response.body.id}; }
export function buildQueryRequest(ctx) { return {url:ctx.baseUrl+"/query"}; }
export function parseTaskResult() { return {status:"IN_PROGRESS"}; }
export function listArtifacts() { return []; }
export function buildContentRequest() { throw new Error("artifact_not_found"); }
export const protocols = {openai_video:{decodeRequest(ctx){return {kind:"submit",model:ctx.model,action:"text_to_video",requestBody:ctx.body.value};},render(){return {};}}};`
	plugin, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, jsplugin.DefaultRegistry.Unregister(plugin.Meta.Key)) })
	box := func(kind string, payload []byte) []byte {
		result := make([]byte, 8+len(payload))
		binary.BigEndian.PutUint32(result, uint32(len(result)))
		copy(result[4:8], kind)
		copy(result[8:], payload)
		return result
	}
	videoHandler := make([]byte, 25)
	copy(videoHandler[8:12], "vide")
	video := box("ftyp", []byte("isom\x00\x00\x00\x00"))
	video = append(video, box("moov", box("trak", box("mdia", box("hdlr", videoHandler))))...)
	video = append(video, box("mdat", []byte("generated-canvas-video"))...)
	var submissions atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/generated.mp4" {
			assert.Empty(t, r.Header.Get("Authorization"))
			assert.Empty(t, r.Header.Get(service.CanvasClientProofHeader))
			assert.Empty(t, r.Header.Get(service.CanvasGrantHeader))
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Set-Cookie", "private-upstream-cookie")
			http.ServeContent(w, r, "generated.mp4", time.Time{}, bytes.NewReader(video))
			return
		}
		submissions.Add(1)
		assert.Equal(t, "/submit", r.URL.Path)
		assert.Equal(t, "Bearer canvas-upstream-secret", r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get(service.CanvasClientProofHeader))
		assert.Empty(t, r.Header.Get(service.CanvasGrantHeader))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"upstream-canvas-video"}`)
	}))
	t.Cleanup(upstream.Close)
	service.InitHttpClient()
	channelSetting := `{"task_plugin_key":"canvas-worker-video"}`
	channel := model.Channel{Name: "canvas-worker-video", Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "canvas-upstream-secret", BaseURL: &upstream.URL, Setting: &channelSetting, Models: "canvas-worker-video", Group: "default"}
	require.NoError(t, f.db.Create(&channel).Error)
	require.NoError(t, f.db.Create(&model.Ability{ChannelId: channel.Id, Model: "canvas-worker-video", Group: "default", Enabled: true}).Error)
	request := service.AsyncVideoRequest{Body: []byte(`{"model":"canvas-worker-video","prompt":"draw a landscape"}`), ContentType: "application/json", ClientIP: "192.0.2.1", Channel: channel, PluginHash: plugin.Engine.SourceFingerprint(), Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: plugin.Meta.Key, Version: plugin.Meta.Version}}, Billing: service.MediaBillingSnapshot{Price: gatewaytypes.PriceData{Quota: 100, QuotaToPreConsume: 100, UsePrice: true, GroupRatioInfo: gatewaytypes.GroupRatioInfo{GroupRatio: 1}}}}
	encoded, err := common.Marshal(request)
	require.NoError(t, err)
	job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: f.user.Id, TokenId: record.TokenID, ChannelId: channel.Id, Provider: plugin.Meta.Key, Model: "canvas-worker-video", Group: "default", Status: "queued", BillingStatus: "pending", StorageStatus: "pending", LeaseToken: "canvas-worker-lease", LeaseExpiresAt: time.Now().Unix() + 120, ExpiresAt: time.Now().Unix() + 3600}
	job.RequestCipher, err = service.EncryptImagePayload(encoded, "media-request:"+job.TaskId)
	require.NoError(t, err)
	channelBytes, err := common.Marshal(channel)
	require.NoError(t, err)
	job.ChannelCipher, err = service.EncryptImagePayload(channelBytes, "media-channel:"+job.TaskId)
	require.NoError(t, err)
	require.NoError(t, f.db.Create(&job).Error)
	service.SubmitAsyncVideoFunc = ExecuteAsyncVideo
	cfg := service.DefaultMediaRuntimeConfig()
	cfg.VideoAsyncEnabled, cfg.LocalPath = true, t.TempDir()
	require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
	require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
	require.NoError(t, f.db.First(&job, job.ID).Error)
	assert.Equal(t, "submitted", job.Status)
	assert.Positive(t, job.DispatchedAt)
	var accepted model.Task
	require.NoError(t, f.db.Where("task_id = ?", job.TaskId).Take(&accepted).Error)
	assert.Equal(t, record.TokenID, accepted.PrivateData.TokenId)
	assert.Equal(t, 100, accepted.Quota)
	var owner model.User
	require.NoError(t, f.db.First(&owner, f.user.Id).Error)
	assert.Equal(t, 999900, owner.Quota)
	assert.EqualValues(t, 1, submissions.Load())
	assert.Equal(t, lastActive, f.redis.HGet("canvas:grant:"+grant.GrantID, "last_active"))
	require.NoError(t, service.RevokeCanvasGrant(t.Context(), grant.GrantID))
	// Accepted work keeps its settlement/storage path after revocation.
	require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
	require.NoError(t, f.db.Model(&accepted).Update("status", model.TaskStatusSuccess).Error)
	service.PersistAsyncVideoFunc = func(ctx context.Context, _ model.AsyncMediaJob, task *model.Task) error {
		_, err := service.GetTaskArtifactStore().Persist(ctx, task, gatewaytypes.TaskArtifact{Key: "video", Type: "video", MimeType: "video/mp4"}, bytes.NewReader(video))
		return err
	}
	require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
	require.NoError(t, f.db.First(&job, job.ID).Error)
	assert.Equal(t, "submitted", job.Status)
	assert.Equal(t, "ready", job.StorageStatus)
	claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, common.GetUUID(), 30)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
	require.NoError(t, f.db.First(&job, job.ID).Error)
	assert.Equal(t, "succeeded", job.Status)
	assert.Equal(t, "settled", job.BillingStatus)
	for _, dispatched := range []bool{false, true} {
		queued := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: f.user.Id, TokenId: record.TokenID, ChannelId: channel.Id, Provider: plugin.Meta.Key, Model: job.Model, Group: "default", Status: "queued", BillingStatus: "pending", LeaseToken: "next-canvas-lease", ExpiresAt: time.Now().Unix() + 3600}
		queued.RequestCipher, err = service.EncryptImagePayload(encoded, "media-request:"+queued.TaskId)
		require.NoError(t, err)
		if dispatched {
			queued.DispatchedAt, queued.BillingStatus = time.Now().Unix(), "reserved"
		}
		require.NoError(t, f.db.Create(&queued).Error)
		processErr := service.ProcessAsyncMediaJob(t.Context(), queued, cfg)
		require.NoError(t, f.db.First(&queued, queued.ID).Error)
		if dispatched {
			require.NoError(t, processErr)
			assert.Equal(t, "execution_unknown", queued.Status)
			assert.Equal(t, "unknown", queued.BillingStatus)
		} else {
			require.Error(t, processErr)
			assert.Equal(t, "failed", queued.Status)
			assert.Zero(t, queued.DispatchedAt)
		}
	}
	assert.EqualValues(t, 1, submissions.Load(), "revoked queued and uncertain jobs must never submit again")
	require.NoError(t, f.db.First(&owner, f.user.Id).Error)
	assert.Equal(t, 999900, owner.Quota, "revocation/uncertainty cannot duplicate a charge or refund accepted work")
	current := f.grant(t)
	jwt, _, err := service.IssueAccessToken(f.identity)
	require.NoError(t, err)
	admin := model.User{Username: "canvas-video-admin", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1}
	require.NoError(t, f.db.Create(&admin).Error)
	adminIdentity := service.AuthIdentity{UserID: admin.Id, SessionID: "canvas-video-admin-parent", UserAuthVersion: 1, SessionVersion: 1}
	require.NoError(t, f.db.Create(&model.UserSession{SID: adminIdentity.SessionID, UserID: admin.Id, Version: 1, UserAuthVersion: 1, Status: model.UserSessionStatusActive, RefreshHash: "admin-refresh-hash", ExpiresAt: job.ExpiresAt}).Error)
	adminJWT, _, err := service.IssueAccessToken(adminIdentity)
	require.NoError(t, err)
	ordinary := model.Token{UserId: f.user.Id, Key: "ordinaryvideo" + common.GetUUID(), Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, f.db.Create(&ordinary).Error)
	contentPath := service.CanvasContentPath("media", job.TaskId, "video")
	userView := mediaTaskResultPath(job.TaskId, "video", false)
	adminView := mediaTaskResultPath(job.TaskId, "video", true)
	legacyVideo := "/v1/videos/" + job.TaskId + "/content"
	legacyArtifact := "/v1/tasks/" + job.TaskId + "/artifacts/video/content"
	legacyManifest := "/api/task/" + job.TaskId + "/artifacts"
	assert.Equal(t, 401, f.request("GET", contentPath, "", f.proof, "", grant.GrantID).Code)
	assert.Equal(t, 401, f.request("GET", userView, "", "", "", "").Code)
	assert.Equal(t, 404, f.request("GET", userView, "", "Bearer "+adminJWT, "", "").Code)
	var object model.MediaArtifactObject
	require.NoError(t, f.db.Where("task_id = ?", job.TaskId).Take(&object).Error)
	signed, err := service.MediaObjectURL(object, "", 3600)
	require.NoError(t, err)
	assert.Equal(t, 404, f.request("GET", signed, "", "", "", "").Code)
	// Keep the URL public while pinning all test transfer connections to the
	// local stub. The normal proxy still performs URL and header checks.
	proxyClient := service.GetSSRFProtectedHTTPClient()
	previousTransport := proxyClient.Transport
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Listener.Addr().String())
	}}
	proxyClient.Transport = transport
	t.Cleanup(func() { proxyClient.Transport = previousTransport; transport.CloseIdleConnections() })
	for _, source := range []string{"local", "upstream"} {
		t.Run(source+" generated result access", func(t *testing.T) {
			if source == "upstream" {
				accepted.PrivateData.UpstreamAsync = &relaydto.UpstreamAsyncProfile{}
				accepted.PrivateData.ResultURL = "http://8.8.8.8/generated.mp4?signature=canvas-private-video"
				require.NoError(t, f.db.Model(&accepted).Update("private_data", accepted.PrivateData).Error)
				require.NoError(t, f.db.Model(&job).Update("storage_status", "upstream").Error)
			}
			for _, endpoint := range []struct{ path, auth, proof, grant string }{
				{"/v1/media/tasks_async/" + job.TaskId, "Bearer " + current.APIKey, f.proof, ""},
				{"/api/integrations/canvas/history/media/" + job.TaskId, f.proof, "", current.GrantID},
				{"/api/user/media-tasks/" + job.TaskId, "Bearer " + jwt, "", ""},
				{"/api/admin/media-tasks/" + job.TaskId, "Bearer " + adminJWT, "", ""},
				{legacyManifest, "Bearer " + jwt, "", ""},
				{legacyManifest, "Bearer " + adminJWT, "", ""},
			} {
				response := f.request("GET", endpoint.path, "", endpoint.auth, endpoint.proof, endpoint.grant)
				require.Equal(t, 200, response.Code, response.Body.String())
				if strings.Contains(endpoint.path, "/api/user/") || strings.Contains(endpoint.path, "/api/admin/") || endpoint.path == legacyManifest {
					assert.Contains(t, response.Body.String(), "/results/video/view")
					assert.Contains(t, response.Body.String(), `"authenticated":true`)
				} else {
					assert.Contains(t, response.Body.String(), "/content/video")
				}
				assert.NotContains(t, response.Body.String(), "access=")
				assert.NotContains(t, response.Body.String(), "canvas-private-video")
				assert.NotContains(t, response.Body.String(), "/v1/media/objects/")
			}
			for _, endpoint := range []struct{ path, auth, grant, proof string }{
				{contentPath, f.proof, current.GrantID, ""}, {userView, "Bearer " + jwt, "", ""}, {adminView, "Bearer " + adminJWT, "", ""},
				{legacyVideo, "Bearer " + current.APIKey, "", f.proof},
				{legacyArtifact, "Bearer " + ordinary.Key, "", ""},
			} {
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					request := httptest.NewRequest(method, endpoint.path, nil)
					request.Header.Set("Authorization", endpoint.auth)
					request.Header.Set(service.CanvasGrantHeader, endpoint.grant)
					if endpoint.proof != "" {
						request.Header.Set(service.CanvasClientProofHeader, endpoint.proof)
					}
					request.Header.Set("Range", "bytes=0-3")
					response := httptest.NewRecorder()
					f.router.ServeHTTP(response, request)
					require.Equal(t, http.StatusPartialContent, response.Code, response.Body.String())
					assert.Equal(t, "bytes 0-3/"+strconv.Itoa(len(video)), response.Header().Get("Content-Range"))
					assert.Contains(t, response.Header().Get("Cache-Control"), "no-store")
					assert.Equal(t, "video/mp4", response.Header().Get("Content-Type"))
					assert.Empty(t, response.Header().Get("Set-Cookie"))
					assert.Empty(t, response.Header().Get("Location"))
					if method == http.MethodGet {
						assert.Equal(t, video[:4], response.Body.Bytes())
					} else {
						assert.Empty(t, response.Body.Bytes())
					}
				}
			}
			if source == "local" {
				require.NoError(t, f.db.Delete(&accepted).Error)
				for _, endpoint := range []struct{ path, auth, grant string }{
					{contentPath, f.proof, current.GrantID}, {userView, "Bearer " + jwt, ""},
				} {
					response := f.request("GET", endpoint.path, "", endpoint.auth, "", endpoint.grant)
					require.Equal(t, 200, response.Code, response.Body.String())
					assert.Equal(t, video, response.Body.Bytes(), "local artifacts outlive the separately retained task row")
				}
				require.NoError(t, f.db.Create(&accepted).Error)
			}
		})
	}
	require.NoError(t, service.RevokeCanvasGrant(t.Context(), current.GrantID))
	assert.Equal(t, 401, f.request("GET", contentPath, "", f.proof, "", current.GrantID).Code)
	assert.Equal(t, 401, f.request("GET", legacyVideo, "", "Bearer "+current.APIKey, f.proof, "").Code)
	assert.Equal(t, 200, f.request("GET", legacyManifest, "", "Bearer "+jwt, "", "").Code)
	assert.Equal(t, 200, f.request("GET", userView, "", "Bearer "+jwt, "", "").Code, "grant revocation preserves dashboard access")
	replacement := f.grant(t)
	assert.Equal(t, 200, f.request("GET", contentPath, "", f.proof, "", replacement.GrantID).Code)
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("canvas_subject", "ics_"+common.GetUUID()).Error)
	assert.Equal(t, 404, f.request("GET", userView, "", "Bearer "+jwt, "", "").Code)
	assert.Equal(t, 404, f.request("GET", adminView, "", "Bearer "+adminJWT, "", "").Code)
	assert.Equal(t, 404, f.request("GET", legacyArtifact, "", "Bearer "+ordinary.Key, "", "").Code)
	assert.Equal(t, 404, f.request("GET", legacyManifest, "", "Bearer "+adminJWT, "", "").Code)
	response := f.request("GET", "/api/user/media-tasks?media_type=video", "", "Bearer "+jwt, "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), job.TaskId)
	ordinaryJob := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: f.user.Id, TokenId: ordinary.Id, Status: "succeeded", StorageStatus: "upstream", BillingStatus: "settled", ExpiresAt: job.ExpiresAt}
	require.NoError(t, f.db.Create(&ordinaryJob).Error)
	ordinaryTask := accepted
	ordinaryTask.ID, ordinaryTask.TaskID, ordinaryTask.PrivateData.TokenId = 0, ordinaryJob.TaskId, ordinary.Id
	require.NoError(t, f.db.Create(&ordinaryTask).Error)
	ordinaryContent := "/v1/videos/" + ordinaryTask.TaskID + "/content"
	response = f.request("GET", ordinaryContent, "", "Bearer "+ordinary.Key, "", "")
	require.Equal(t, http.StatusTemporaryRedirect, response.Code, response.Body.String())
	assert.Equal(t, ordinaryTask.PrivateData.ResultURL, response.Header().Get("Location"), "ordinary tasks retain the existing upstream redirect")
	response = f.request("GET", "/api/task/"+ordinaryTask.TaskID+"/artifacts", "", "Bearer "+jwt, "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), ordinaryTask.PrivateData.ResultURL)
	assert.NotContains(t, response.Body.String(), `"authenticated":true`)
	ordinaryView := mediaTaskResultPath(ordinaryJob.TaskId, "video", false)
	assert.Equal(t, 404, f.request("GET", ordinaryView, "", "Bearer "+jwt, "", "").Code)
	ordinary.Status = common.TokenStatusDisabled
	require.NoError(t, ordinary.Update())
	assert.Equal(t, 401, f.request("GET", ordinaryContent, "", "Bearer "+ordinary.Key, "", "").Code, "the public route retains token revocation enforcement")
	assert.Equal(t, 404, f.request("GET", ordinaryView, "", "Bearer "+jwt, "", "").Code, "the managed content route cannot bypass ordinary token revocation")
}

func TestCanvasRealRelayChargesTheAuthorizedOwner(t *testing.T) {
	f := newCanvasFixture(t)
	previousBatch, previousLogs, previousCount, previousQuota, previousMemory := common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken, common.QuotaPerUnit, common.MemoryCacheEnabled
	common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken, common.QuotaPerUnit, common.MemoryCacheEnabled = false, true, false, 500000, false
	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		switch key {
		case "billing_setting.billing_mode", "billing_setting.billing_expr", "group_ratio_setting.group_ratio", "perf_metrics_setting.enabled":
			saved[key] = value
		}
		return nil
	}))
	t.Cleanup(func() {
		common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken, common.QuotaPerUnit, common.MemoryCacheEnabled = previousBatch, previousLogs, previousCount, previousQuota, previousMemory
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"billing_setting.billing_mode": `{"canvas-text":"tiered_expr","canvas-image":"tiered_expr"}`, "billing_setting.billing_expr": `{"canvas-text":"tier(\"request\", fixed(0.002))","canvas-image":"tier(\"request\", fixed(0.002))"}`, "group_ratio_setting.group_ratio": `{"default":1}`, "perf_metrics_setting.enabled": "false"}))
	require.NoError(t, f.db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Log{}, &model.Option{}))
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("setting", `{"billing_preference":"wallet_only"}`).Error)
	grant := f.grant(t)
	var record model.CanvasGrant
	require.NoError(t, f.db.First(&record, "id = ?", grant.GrantID).Error)
	var seen atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := seen.Add(1)
		assert.Equal(t, "Bearer upstream-test-secret", r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get(service.CanvasClientProofHeader))
		assert.Empty(t, r.Header.Get(service.CanvasGrantHeader))
		var owner model.User
		if assert.NoError(t, f.db.First(&owner, f.user.Id).Error) {
			assert.Less(t, owner.Quota, 1000000, "real gateway reserves wallet funds before the upstream call")
		}
		w.Header().Set("Content-Type", "application/json")
		if requestNumber == 4 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"test failure","type":"invalid_request_error"}}`)
			return
		}
		if strings.Contains(r.URL.Path, "/images/") {
			fmt.Fprint(w, `{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`)
			return
		}
		fmt.Fprint(w, `{"id":"chat-test","object":"chat.completion","model":"canvas-text","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() {
		assert.Eventually(t, func() bool {
			var owner model.User
			var charged int64
			if f.db.First(&owner, f.user.Id).Error != nil {
				return false
			}
			if f.db.Model(&model.Log{}).Select("COALESCE(SUM(quota),0)").Where("type = ?", model.LogTypeConsume).Scan(&charged).Error != nil {
				return false
			}
			return owner.Quota == 1000000-int(charged)
		}, time.Second, 10*time.Millisecond, "accepted request refunds finish before shared globals are restored")
	})
	service.InitHttpClient()
	channel := model.Channel{Name: "canvas-real-relay", Key: "upstream-test-secret", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Group: "default", Models: "canvas-text,canvas-image", BaseURL: &upstream.URL}
	require.NoError(t, f.db.Create(&channel).Error)
	for _, name := range []string{"canvas-text", "canvas-image"} {
		require.NoError(t, f.db.Create(&model.Ability{ChannelId: channel.Id, Model: name, Group: "default", Enabled: true}).Error)
	}
	engine := gin.New()
	engine.POST("/v1/chat/completions", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
	engine.POST("/v1/images/generations", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIImage) })
	engine.POST("/v1/images/edits", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIImage) })
	f.router = engine
	response := f.request("POST", "/v1/chat/completions", `{"model":"canvas-text","messages":[{"role":"user","content":"hello"}],"user_id":999999}`, "Bearer "+grant.APIKey, f.proof, "")
	require.Equal(t, 200, response.Code, response.Body.String())
	response = f.request("POST", "/v1/images/generations", `{"model":"canvas-image","prompt":"draw","n":1,"response_format":"b64_json"}`, "Bearer "+grant.APIKey, f.proof, "")
	require.Equal(t, 200, response.Code, response.Body.String())
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	require.NoError(t, form.WriteField("model", "canvas-image"))
	require.NoError(t, form.WriteField("prompt", "edit"))
	part, err := form.CreateFormFile("image", "reference.png")
	require.NoError(t, err)
	_, err = part.Write([]byte("\x89PNG\r\n\x1a\nreference"))
	require.NoError(t, err)
	require.NoError(t, form.Close())
	request := httptest.NewRequest("POST", "/v1/images/edits", &body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+grant.APIKey)
	request.Header.Set(service.CanvasClientProofHeader, f.proof)
	response = httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	require.Equal(t, 200, response.Code, response.Body.String())
	var logs []model.Log
	require.NoError(t, f.db.Where("type = ?", model.LogTypeConsume).Order("id").Find(&logs).Error)
	require.Len(t, logs, 3)
	for _, entry := range logs {
		assert.Equal(t, f.user.Id, entry.UserId)
		assert.Equal(t, record.TokenID, entry.TokenId)
		assert.Equal(t, 1000, entry.Quota)
		assert.Contains(t, []string{"canvas-text", "canvas-image"}, entry.ModelName)
	}
	var owner model.User
	require.NoError(t, f.db.First(&owner, f.user.Id).Error)
	assert.Equal(t, 997000, owner.Quota)
	assert.Equal(t, 3000, owner.UsedQuota)
	assert.Equal(t, 3, owner.RequestCount)
	assert.EqualValues(t, 3, seen.Load())
	response = f.request("POST", "/v1/chat/completions", `{"model":"canvas-text","messages":[{"role":"user","content":"fails"}]}`, "Bearer "+grant.APIKey, f.proof, "")
	require.Equal(t, 400, response.Code, response.Body.String())
	require.Eventually(t, func() bool { return f.db.First(&owner, f.user.Id).Error == nil && owner.Quota == 997000 }, time.Second, 10*time.Millisecond, "failed upstream request refunds its reservation")
	assert.EqualValues(t, 4, seen.Load(), "invalid request errors do not create a paid retry")
}

// External DSNs must name disposable databases. Prefixing this matrix leaves
// unrelated tables intact and lets repeated runs prove migration idempotency.
func TestCanvasManagementAccountIncarnationBoundaries(t *testing.T) {
	f := newCanvasFixture(t)
	require.NoError(t, f.db.AutoMigrate(&model.Task{}, &model.AsyncImageBill{}, &model.ImageOutbox{}))
	grant := f.grant(t)
	var record model.CanvasGrant
	require.NoError(t, f.db.First(&record, "id = ?", grant.GrantID).Error)
	jwt, _, err := service.IssueAccessToken(f.identity)
	require.NoError(t, err)
	for _, action := range []string{"resume", "terminate"} {
		f.router.POST("/api/user/media-tasks/:task_id/"+action, middleware.UserAuth(), func(c *gin.Context) {
			ManageMediaTask(c, false, action)
		})
	}
	ordinary := model.Token{UserId: f.user.Id, Key: "ordinary-management-" + common.GetUUID(), Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, f.db.Create(&ordinary).Error)
	for _, scenario := range []struct {
		name    string
		tokenID int
		want    int
	}{
		{"same subject", record.TokenID, http.StatusOK},
		{"revoked grant preserves dashboard ownership", record.TokenID, http.StatusOK},
		{"reused account denies old tasks", record.TokenID, http.StatusNotFound},
		{"ordinary tasks preserve numeric ownership", ordinary.Id, http.StatusOK},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if scenario.name == "revoked grant preserves dashboard ownership" {
				require.NoError(t, service.RevokeCanvasGrant(t.Context(), grant.GrantID))
			}
			if scenario.name == "reused account denies old tasks" {
				require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("canvas_subject", "ics_"+common.GetUUID()).Error)
			}
			for _, action := range []string{"resume", "terminate"} {
				status, storage := "queued", "pending"
				if action == "resume" {
					status, storage = "succeeded", "upstream"
				}
				job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: f.user.Id, TokenId: scenario.tokenID, Status: status, StorageStatus: storage, BillingStatus: "pending", ExpiresAt: time.Now().Unix() + 3600}
				require.NoError(t, f.db.Create(&job).Error)
				if action == "resume" {
					require.NoError(t, f.db.Create(&model.Task{TaskID: job.TaskId, UserId: job.UserId, Status: model.TaskStatusSuccess}).Error)
				}
				response := f.request("POST", "/api/user/media-tasks/"+job.TaskId+"/"+action, "{}", "Bearer "+jwt, "", "")
				require.Equal(t, scenario.want, response.Code, response.Body.String())
				var stored model.AsyncMediaJob
				require.NoError(t, f.db.First(&stored, job.ID).Error)
				if scenario.want == http.StatusNotFound {
					assert.Equal(t, job, stored, "denied operations must preserve billing and execution state")
				} else if action == "resume" {
					assert.Equal(t, "submitted", stored.Status)
				} else {
					assert.Equal(t, "failed", stored.Status)
					assert.Equal(t, "not_billable", stored.BillingStatus)
				}
			}
			image := model.AsyncImageTask{TaskId: "asyncimg_" + common.GetUUID(), UserId: f.user.Id, TokenId: scenario.tokenID, Status: model.ImageTaskStorageFailed, ImageCount: 1}
			require.NoError(t, f.db.Create(&image).Error)
			require.NoError(t, f.db.Create(&model.AsyncImageBill{TaskId: image.TaskId, BillingRequestId: common.GetUUID(), UserId: image.UserId, TokenId: image.TokenId}).Error)
			response := f.request("POST", "/api/user/media-tasks/"+image.TaskId+"/resume", "{}", "Bearer "+jwt, "", "")
			require.Equal(t, scenario.want, response.Code, response.Body.String())
			var stored model.AsyncImageTask
			require.NoError(t, f.db.Where("task_id = ?", image.TaskId).Take(&stored).Error)
			if scenario.want == http.StatusNotFound {
				assert.Equal(t, image, stored)
			} else {
				assert.Equal(t, model.ImageTaskUpstreamSucceeded, stored.Status)
			}
		})
	}
}

func TestCanvasDatabaseCompatibility(t *testing.T) {
	engines := []struct {
		name, dsn string
		dialect   func(string) gorm.Dialector
	}{
		{"sqlite", ":memory:", func(dsn string) gorm.Dialector { return sqlite.Open(dsn) }},
		{"mysql", os.Getenv("TEST_MYSQL_DSN"), func(dsn string) gorm.Dialector { return mysql.Open(dsn) }},
		{"postgres", os.Getenv("TEST_POSTGRES_DSN"), func(dsn string) gorm.Dialector { return postgres.Open(dsn) }},
	}
	for _, engine := range engines {
		t.Run(engine.name, func(t *testing.T) {
			if engine.dsn == "" {
				t.Skip("set a disposable TEST_MYSQL_DSN / TEST_POSTGRES_DSN to run the real database matrix")
			}
			for _, mode := range []string{"fresh", "upgrade_current_branch_baseline"} {
				t.Run(mode, func(t *testing.T) {
					prefix := "icmatrix_" + common.GetUUID()[:8] + "_"
					db, err := gorm.Open(engine.dialect(engine.dsn), &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: prefix}, Logger: logger.Default.LogMode(logger.Silent)})
					require.NoError(t, err)
					sqlDB, err := db.DB()
					require.NoError(t, err)
					t.Cleanup(func() { _ = sqlDB.Close() })
					var version string
					if engine.name == "sqlite" {
						require.NoError(t, db.Raw("SELECT sqlite_version()").Scan(&version).Error)
					} else {
						require.NoError(t, db.Raw("SELECT version()").Scan(&version).Error)
					}
					t.Logf("database=%s version=%s baseline=current-branch minus CanvasSubject; released-version upgrade remains a separate check", engine.name, version)
					require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSession{}))
					if mode != "fresh" {
						require.NoError(t, db.Migrator().DropColumn(&model.User{}, "CanvasSubject"))
					}
					user := model.User{Username: "existing-owner", AffCode: prefix, AuthVersion: 1, Group: "default", Quota: 12345, Status: common.UserStatusEnabled}
					var token model.Token
					if mode != "fresh" {
						require.NoError(t, db.Omit("CanvasSubject").Create(&user).Error)
						token = model.Token{UserId: user.Id, Key: "existing" + common.GetUUID(), Name: "existing token", UnlimitedQuota: true, ExpiredTime: -1, Status: common.TokenStatusEnabled}
						require.NoError(t, db.Create(&token).Error)
					}
					for range 2 {
						require.NoError(t, db.AutoMigrate(&model.CanvasGrant{}, &model.User{}, &model.Token{}, &model.UserSession{}))
					}
					if mode != "fresh" {
						var stored model.User
						require.NoError(t, db.First(&stored, user.Id).Error)
						assert.Equal(t, user.Quota, stored.Quota)
						assert.Nil(t, stored.CanvasSubject, "existing accounts migrate to NULL, not a shared empty unique value")
						var storedToken model.Token
						require.NoError(t, db.First(&storedToken, token.Id).Error)
						assert.Equal(t, token.Key, storedToken.Key)
					} else {
						require.NoError(t, db.Create(&user).Error)
						token = model.Token{UserId: user.Id, Key: "ordinary" + common.GetUUID()}
						require.NoError(t, db.Create(&token).Error)
					}
					internal := model.Token{UserId: user.Id, Key: "ic_" + common.GetUUID(), Name: "internal application"}
					require.NoError(t, db.Create(&internal).Error)
					previousDB, previousRedis, previousType := model.DB, common.RedisEnabled, common.MainDatabaseType()
					model.DB, common.RedisEnabled = db, false
					if engine.name == "mysql" {
						common.SetMainDatabaseType(common.DatabaseTypeMySQL)
					} else if engine.name == "postgres" {
						common.SetMainDatabaseType(common.DatabaseTypePostgreSQL)
					} else {
						common.SetMainDatabaseType(common.DatabaseTypeSQLite)
					}
					defer func() {
						model.DB, common.RedisEnabled = previousDB, previousRedis
						common.SetMainDatabaseType(previousType)
					}()
					count, err := model.CountUserTokens(user.Id)
					require.NoError(t, err)
					assert.EqualValues(t, 1, count)
					listed, err := model.GetAllUserTokens(user.Id, 0, 10)
					require.NoError(t, err)
					require.Len(t, listed, 1)
					assert.Equal(t, token.Id, listed[0].Id)
					searched, total, err := model.SearchUserTokens(user.Id, "", "", 0, 10)
					require.NoError(t, err)
					assert.EqualValues(t, 1, total)
					require.Len(t, searched, 1)
					assert.Equal(t, token.Id, searched[0].Id)
					otherLegacy := model.User{Username: "other-legacy", AffCode: "other" + prefix, AuthVersion: 1}
					require.NoError(t, db.Create(&otherLegacy).Error, "multiple legacy NULL subjects remain valid")
					row := model.CanvasGrant{ID: "icg_" + common.GetUUID(), TokenID: internal.Id, UserID: user.Id, ClientID: "infinite-canvas", Subject: "ics_" + common.GetUUID(), CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()}
					require.NoError(t, db.Transaction(func(tx *gorm.DB) error { return tx.Create(&row).Error }))
					var stored model.CanvasGrant
					require.NoError(t, db.Where("token_id = ? AND user_id = ?", internal.Id, user.Id).Take(&stored).Error)
					assert.Equal(t, row.ID, stored.ID)
					duplicate := row
					duplicate.ID = "icg_" + common.GetUUID()
					assert.Error(t, db.Create(&duplicate).Error, "one immutable provenance record per token")
					runCanvasFundingOwnership(t, db, user, internal, row)
					require.NoError(t, db.Delete(&internal).Error)
					require.NoError(t, db.First(&stored, "id = ?", row.ID).Error, "soft deletion must preserve provenance")
				})
			}
		})
	}
}
