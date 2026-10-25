package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestImageTaskPresentationDatabaseMatrix(t *testing.T) {
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var dialector gorm.Dialector
			switch engine {
			case "mysql":
				dsn := os.Getenv("IMAGE_TASK_TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("IMAGE_TASK_TEST_MYSQL_DSN is not configured")
				}
				require.Contains(t, dsn, "/new_api_image_test_presentation", "Use the dedicated disposable presentation database")
				dialector = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("IMAGE_TASK_TEST_PG_DSN")
				if dsn == "" {
					t.Skip("IMAGE_TASK_TEST_PG_DSN is not configured")
				}
				require.Contains(t, dsn, "dbname=new_api_image_test_presentation", "Use the dedicated disposable presentation database")
				dialector = postgres.Open(dsn)
			default:
				dialector = sqlite.Open(filepath.Join(t.TempDir(), "presentation.db"))
			}
			db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			logDialector := gorm.Dialector(sqlite.Open(filepath.Join(t.TempDir(), "refund-logs.db")))
			logEngine := "sqlite"
			if engine == "mysql" && os.Getenv("IMAGE_TASK_TEST_LOG_MYSQL_DSN") != "" {
				dsn := os.Getenv("IMAGE_TASK_TEST_LOG_MYSQL_DSN")
				require.Contains(t, dsn, "/new_api_refund_test_logs", "Use the dedicated disposable refund log database")
				logDialector, logEngine = mysql.Open(dsn), engine
			}
			if engine == "postgres" && os.Getenv("IMAGE_TASK_TEST_LOG_PG_DSN") != "" {
				dsn := os.Getenv("IMAGE_TASK_TEST_LOG_PG_DSN")
				require.Contains(t, dsn, "dbname=new_api_refund_test_logs", "Use the dedicated disposable refund log database")
				logDialector, logEngine = postgres.Open(dsn), engine
			}
			logDB, err := gorm.Open(logDialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			previousDB, previousLogDB, previousType := model.DB, model.LOG_DB, common.MainDatabaseType()
			previousLogType := common.LogDatabaseType()
			model.DB, model.LOG_DB = db, logDB
			common.SetMainDatabaseType(common.DatabaseType(engine))
			common.SetLogDatabaseType(common.DatabaseType(logEngine))
			entities := []any{&model.User{}, &model.Token{}, &model.CanvasGrant{}, &model.Channel{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}, &model.ImageOutbox{}, &model.AsyncImageResult{}, &model.ImageStorageObject{}, &model.ImageStorageProfile{}, &model.Option{}, &model.Task{}, &model.AsyncMediaJob{}, &model.MediaArtifactObject{}}
			t.Cleanup(func() {
				model.DB, model.LOG_DB = previousDB, previousLogDB
				common.SetMainDatabaseType(previousType)
				common.SetLogDatabaseType(previousLogType)
				assert.NoError(t, db.Migrator().DropTable(entities...))
				assert.NoError(t, logDB.Migrator().DropTable(&model.Log{}, &model.AsyncImageLogReceipt{}))
				logConnection, logCloseErr := logDB.DB()
				if assert.NoError(t, logCloseErr) {
					assert.NoError(t, logConnection.Close())
				}
				connection, closeErr := db.DB()
				if assert.NoError(t, closeErr) {
					assert.NoError(t, connection.Close())
				}
			})
			for range 2 {
				require.NoError(t, db.AutoMigrate(&model.AsyncMediaJob{}))
			}
			// Rebuild the released table shape, then exercise the same startup
			// migration against both existing data and its unique task identifier.
			require.NoError(t, db.Migrator().DropColumn(&model.AsyncMediaJob{}, "RefundLogPayload"))
			legacy := model.AsyncMediaJob{TaskId: "legacy-video-refund-upgrade", IdentityHash: "legacy-video-refund-identity", UserId: 90101, TokenId: 90201, Provider: "drama", Model: "legacy-video-model", Group: "refund-upgrade", Status: "execution_unknown", StorageStatus: "pending", BillingStatus: "unknown", Quota: 120, ErrorMessage: "historical video submission diagnostic", CreatedAt: 1700000000, ExpiresAt: 1800000000}
			require.NoError(t, db.Omit("RefundLogPayload").Create(&legacy).Error)
			for range 2 {
				require.NoError(t, db.AutoMigrate(entities...))
				require.NoError(t, logDB.AutoMigrate(&model.Log{}, &model.AsyncImageLogReceipt{}))
			}
			var upgraded model.AsyncMediaJob
			require.NoError(t, db.Take(&upgraded, legacy.ID).Error)
			assert.Equal(t, legacy, upgraded, "adding the refund log outbox must preserve historical task and accounting data")
			assert.Empty(t, upgraded.RefundLogPayload)
			duplicateLegacy := legacy
			duplicateLegacy.ID, duplicateLegacy.IdentityHash = 0, "legacy-video-refund-duplicate"
			require.Error(t, db.Create(&duplicateLegacy).Error, "upgrading must preserve task_id uniqueness")
			require.NoError(t, db.Delete(&legacy).Error)
			var version string
			versionQuery := "SELECT VERSION()"
			if engine == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("Database version: %s", version)
			logVersionQuery := "SELECT VERSION()"
			if logEngine == "sqlite" {
				logVersionQuery = "SELECT sqlite_version()"
			}
			var logVersion string
			require.NoError(t, logDB.Raw(logVersionQuery).Scan(&logVersion).Error)
			t.Logf("Separate log database (%s) version: %s", logEngine, logVersion)
			now := time.Now().Unix()
			require.NoError(t, db.Create(&model.User{Id: 101, Username: "creator", DisplayName: "Creative account", Password: "password-secret-canary", AffCode: "creator"}).Error)
			require.NoError(t, db.Create(&model.User{Id: 102, Username: "other", AffCode: "other"}).Error)
			require.NoError(t, db.Create(&model.Token{Id: 201, UserId: 101, Name: "Studio Key", Key: "token-secret-canary"}).Error)
			require.NoError(t, db.Create(&model.Token{Id: 202, UserId: 102, Name: "Other user key", Key: "other-token-secret"}).Error)
			require.NoError(t, db.Create(&model.Channel{Id: 301, Name: "Sunburst upstream", Key: "channel-secret-canary"}).Error)
			attempts, err := common.Marshal([]service.ImageChannelAttempt{{ChannelId: 301, KeyFingerprint: "fingerprint-canary", KeyIndex: 2, StartedAt: now - 40, FinishedAt: now - 10, Dispatched: true, HTTPStatus: http.StatusBadRequest, ErrorMessage: `{"message":"sanitized provider detail","input":"history-private-prompt"}`, ProviderCode: "invalid_request", ProviderStatus: "INVALID_ARGUMENT", UpstreamRequestID: "provider-request-1"}})
			require.NoError(t, err)
			task := model.AsyncImageTask{TaskId: "asyncimg_success", UserId: 101, TokenId: 201, ChannelId: 301, Group: "default", Platform: "openai", Dialect: "bb", RequestType: "image_to_image", Model: "gpt-image-2.5-sunburst", Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ImageCount: 1, ResultCount: 1, CreatedAt: now - 50, StartedAt: now - 40, FinishedAt: now - 10, Attempts: string(attempts), ReferenceUrls: `["https://example.com/private-reference"]`, RequestedResolution: "2K", ActualSize: "2048x1186"}
			require.NoError(t, db.Create(&task).Error)
			polling := model.AsyncImageTask{TaskId: "asyncimg_upstream_polling", UserId: 101, TokenId: 201, ChannelId: 301, Group: "default", Platform: "openai", Dialect: "async", RequestType: "text_to_image", Model: "async-image-model", Status: model.ImageTaskQueued, UpstreamTaskId: "private-upstream-job", DispatchedAt: now - 20, StartedAt: now - 20, NextAttemptAt: now + 10, CreatedAt: now - 20}
			require.NoError(t, db.Create(&polling).Error)
			require.NoError(t, db.Create(&model.AsyncImageTask{TaskId: "asyncimg_other", UserId: 102, TokenId: 202, Status: model.ImageTaskQueued, CreatedAt: now - 50}).Error)
			require.NoError(t, db.Create(&model.AsyncImageEvent{TaskId: task.TaskId, EventKey: "presentation-event", EventType: "settled", Status: model.ImageTaskSucceeded, Message: "routing-event-canary", CreatedAt: now - 10}).Error)
			require.NoError(t, db.Create(&model.ImageStorageProfile{ProfileId: "presentation-profile", Provider: "local", Class: "temporary", Root: "private-storage-canary"}).Error)
			require.NoError(t, db.Create(&model.ImageStorageObject{ObjectId: "presentation-output", ProfileId: "presentation-profile", ObjectKey: "private-object-canary", Width: 2048, Height: 1186, Status: "active"}).Error)
			require.NoError(t, db.Create(&model.AsyncImageResult{TaskId: task.TaskId, ImageIndex: 0, ObjectId: "presentation-output"}).Error)
			t.Run("public image query failure semantics", func(t *testing.T) {
				for _, tc := range []struct {
					name, status, action, summary string
					task                          model.AsyncImageTask
					retryable                     bool
				}{
					{name: "quota_wait", status: "queued", action: "wait_local_dependency", summary: "图片额度校验暂时不可用", retryable: true, task: model.AsyncImageTask{Status: model.ImageTaskQueued, ErrorCode: "quota_cache_unavailable", PublicErrorCode: 606, ErrorMessage: "Local wallet cache temporarily unavailable", NextAttemptAt: now + 10}},
					{name: "capacity_wait", status: "queued", action: "wait_capacity", summary: "图片处理容量暂时不可用", retryable: true, task: model.AsyncImageTask{Status: model.ImageTaskQueued, ErrorCode: "local_capacity_busy", PublicErrorCode: 603, ErrorMessage: "Local image capacity is busy", NextAttemptAt: now + 10}},
					{name: "material_rejected", status: "failed", summary: "参考图超过上游 20MB 限制", task: model.AsyncImageTask{Status: model.ImageTaskFailed, ErrorCode: "upstream_job_failed", PublicErrorCode: 604, ErrorMessage: "参考图过大（25.4MB，上限 20MB）"}},
					{name: "pending_job", status: "processing", task: model.AsyncImageTask{Status: model.ImageTaskQueued, UpstreamTaskId: "accepted", NextAttemptAt: now + 10}},
					{name: "unknown", status: "failed", summary: "上游请求超时", task: model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, ErrorCode: "upstream_transport_timeout", PublicErrorCode: 608, ErrorMessage: `{"error":{"message":"request timed out; Bearer private-token"},"input":"private-prompt"}`, DispatchedAt: now - 1}},
				} {
					t.Run(tc.name, func(t *testing.T) {
						tc.task.TaskId, tc.task.UserId, tc.task.TokenId = "public_notice_"+tc.name, 101, 201
						if tc.action != "" {
							decision, err := common.Marshal(service.ImageRetryDecision{Version: 1, Action: tc.action, Stage: "prepare"})
							require.NoError(t, err)
							tc.task.RetryDecision = string(decision)
						}
						require.NoError(t, db.Create(&tc.task).Error)
						t.Cleanup(func() { assert.NoError(t, db.Delete(&tc.task).Error) })
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Set("id", 101)
						c.Set("token_id", 201)
						c.Params = gin.Params{{Key: "task_id", Value: tc.task.TaskId}}
						c.Request = httptest.NewRequest(http.MethodGet, "/v1/images/tasks_async/"+tc.task.TaskId, nil)
						QueryAsyncImage(c)
						require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
						var response map[string]any
						require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
						assert.Equal(t, tc.status, response["status"])
						assert.EqualValues(t, tc.task.PublicErrorCode, response["failure_code"])
						assert.Equal(t, tc.summary, response["error_summary"])
						assert.Equal(t, tc.retryable, response["error_retryable"])
						assert.Contains(t, response, "error_stage")
						assert.NotContains(t, recorder.Body.String(), "private-token")
						assert.NotContains(t, recorder.Body.String(), "private-prompt")
						if tc.status == "failed" {
							assert.EqualValues(t, tc.task.PublicErrorCode, response["error_code"])
							assert.Equal(t, response["error_message"], response["fail_reason"])
						} else {
							assert.NotContains(t, response, "fail_reason")
						}
						if tc.name == "material_rejected" {
							assert.Contains(t, response["fail_reason"], "上限 20MB")
						}
						if tc.retryable {
							assert.NotEmpty(t, response["error_suggestion"])
							assert.NotEmpty(t, response["error_message"])
						}
					})
				}
			})
			t.Run("image size compatibility explanations share the committed retry decision", func(t *testing.T) {
				const upstreamReason = "No eligible upstream image provider supports the requested image size or resolution tier. Adjust size/resolution or select a compatible route."
				for _, item := range []struct {
					name, status, internal, action, reason, suggestion string
					publicCode                                         int
					retryable                                          bool
				}{
					{"legacy", model.ImageTaskFailed, "upstream_failed", "stop", "unknown_error", "停止", 610, false},
					{"switch", model.ImageTaskQueued, "upstream_image_route_incompatible", "switch_account", "incompatible_image_route", "安排", 603, true},
					{"exhausted", model.ImageTaskFailed, "image_compatible_accounts_exhausted", "stop", "no_compatible_account", "管理员", 603, false},
					{"budget", model.ImageTaskFailed, "upstream_image_route_incompatible", "stop", "compatibility_retry_budget_exhausted", "预算", 603, false},
				} {
					t.Run(item.name, func(t *testing.T) {
						history, err := common.Marshal([]service.ImageChannelAttempt{{ChannelId: 301, Dispatched: true, FinishedAt: now - 1, HTTPStatus: http.StatusUnprocessableEntity, ProviderCode: "image_size_not_supported", ErrorMessage: upstreamReason}})
						require.NoError(t, err)
						decision := service.ImageRetryDecision{Version: 1, Stage: "submit", Action: item.action, Reason: item.reason, Rule: item.internal, Budget: "capacity", Used: 1, Limit: 3, TotalUsed: 1, TotalLimit: 16}
						current := model.AsyncImageTask{TaskId: "asyncimg_compatibility_" + item.name, UserId: 101, TokenId: 201, ChannelId: 301, Group: "default", Model: "image-model", Platform: "openai", Dialect: "async", Status: item.status, BillingStatus: "refunded", PublicErrorCode: item.publicCode, ErrorCode: item.internal, ErrorMessage: upstreamReason + "; Bearer compatibility-secret-canary", Attempts: string(history), RequestedSize: "2048x1152", RequestedResolution: "2K", CreatedAt: now, ExpiresAt: now + 3600}
						if item.retryable {
							current.NextAttemptAt, current.BillingStatus = now+10, "reserved"
							decision.NextAttemptAt = current.NextAttemptAt
						}
						encoded, err := common.Marshal(decision)
						require.NoError(t, err)
						current.RetryDecision = string(encoded)
						require.NoError(t, db.Create(&current).Error)
						t.Cleanup(func() {
							assert.NoError(t, db.Where("task_id = ?", current.TaskId).Delete(&model.AsyncImageTask{}).Error)
						})
						var explanation string
						for _, endpoint := range []string{"image-public", "media-public", "owner", "admin", "owner-list", "admin-list", "media-list"} {
							recorder := httptest.NewRecorder()
							c, _ := gin.CreateTestContext(recorder)
							c.Set("id", current.UserId)
							c.Set("token_id", current.TokenId)
							c.Params = gin.Params{{Key: "task_id", Value: current.TaskId}}
							path := "/v1/images/tasks_async/" + current.TaskId
							if endpoint != "image-public" {
								path = "/v1/media/tasks_async/" + current.TaskId
							}
							c.Request = httptest.NewRequest(http.MethodGet, path+"?task_id="+current.TaskId+"&media_type=image", nil)
							switch endpoint {
							case "image-public", "media-public":
								QueryAsyncImage(c)
							case "owner-list", "admin-list":
								ListImageTasks(c, endpoint == "admin-list")
							case "media-list":
								ListMediaTasks(c, false)
							default:
								GetMediaTask(c, endpoint == "admin")
							}
							require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
							var body map[string]any
							require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
							view := body
							if strings.HasSuffix(endpoint, "-list") {
								items := body["data"].(map[string]any)["items"].([]any)
								require.Len(t, items, 1, endpoint)
								view = items[0].(map[string]any)
							} else if endpoint == "owner" || endpoint == "admin" {
								view = body["data"].(map[string]any)["task"].(map[string]any)
							}
							assert.Contains(t, view["error_summary"], "上游", endpoint)
							assert.Contains(t, view["error_summary"], "分辨率", endpoint)
							assert.Contains(t, view["error_suggestion"], item.suggestion, endpoint)
							assert.NotEmpty(t, view["error_summary_key"], endpoint)
							assert.NotEmpty(t, view["error_suggestion_key"], endpoint)
							assert.Equal(t, item.retryable, view["error_retryable"], endpoint)
							assert.Contains(t, view["error_detail"], upstreamReason, endpoint)
							assert.Contains(t, view["error_message"], view["error_summary"], endpoint)
							assert.Contains(t, view["error_message"], view["error_suggestion"], endpoint)
							assert.NotContains(t, recorder.Body.String(), "compatibility-secret-canary")
							if explanation == "" {
								explanation = view["error_message"].(string)
							} else {
								assert.Equal(t, explanation, view["error_message"], endpoint)
							}
							if strings.HasSuffix(endpoint, "public") && !item.retryable {
								assert.Equal(t, explanation, view["fail_reason"])
							}
						}
						var persisted model.AsyncImageTask
						require.NoError(t, db.Where("task_id = ?", current.TaskId).Take(&persisted).Error)
						assert.Equal(t, current, persisted, "readable errors must not change historical retry or billing records")
					})
				}
			})
			t.Run("reliability and encrypted request diagnostics", func(t *testing.T) {
				t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "test")
				t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
				request := service.AsyncImageRequest{Model: "image-model", Prompt: `{"scene":"山水","note":"Bearer credential-canary"}`, Size: "Bearer size-secret-canary", Resolution: "api_key=resolution-secret-canary", Count: 1, Parts: []service.AsyncImageInputPart{{Type: "image_url", URL: "https://example.com/ref?signature=private-url-canary"}}, ClientIP: "client-ip-canary", Billing: &service.MediaBillingSnapshot{}}
				plain, err := common.Marshal(request)
				require.NoError(t, err)
				cipher, err := service.EncryptImagePayload(plain, "task:asyncimg_diagnostics")
				require.NoError(t, err)
				diagnostics := model.AsyncImageTask{TaskId: "asyncimg_diagnostics", Status: model.ImageTaskFailed, UserId: 101, TokenId: 201, Group: "diagnostic-test", Model: "image-model", RequestCipher: cipher, RequestRetainUntil: now + 3600, CreatedAt: now}
				require.NoError(t, db.Create(&diagnostics).Error)
				second := diagnostics
				second.Id, second.TaskId, second.Status = 0, diagnostics.TaskId+"_second", model.ImageTaskSucceeded
				require.NoError(t, db.Create(&second).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Where("task_id IN ?", []string{diagnostics.TaskId, second.TaskId}).Delete(&model.AsyncImageTask{}).Error)
				})
				router := gin.New()
				router.GET("/request/:task_id", GetImageRetainedRequest)
				router.GET("/reliability", GetImageReliability)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest("GET", "/request/"+diagnostics.TaskId, nil))
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				assert.Contains(t, recorder.Header().Get("Cache-Control"), "no-store")
				assert.Contains(t, recorder.Body.String(), "山水")
				assert.Contains(t, recorder.Body.String(), `\"scene\":\"山水\"`, "retained request prompts preserve JSON-shaped user content")
				for _, secret := range []string{"credential-canary", "size-secret-canary", "resolution-secret-canary", "private-url-canary", "client-ip-canary", "billing", "request_cipher"} {
					assert.NotContains(t, recorder.Body.String(), secret)
				}
				query := "/reliability?group=diagnostic-test&page_size=1"
				recorder = httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest("GET", query, nil))
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var report struct {
					Data service.ImageReliabilityReport
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &report))
				assert.Equal(t, int64(2), report.Data.Totals.Total, "statistics must not inherit the task page size")
				assert.Equal(t, int64(1), report.Data.Totals.Failed)
				assert.Equal(t, int64(1), report.Data.Totals.Succeeded)
				assert.Zero(t, report.Data.Totals.Audited)
				assert.Nil(t, report.Data.Totals.LocalFallback.Percent)
				for _, dates := range []string{"&start_date=2026-01-01&end_date=2026-12-31", "&start_date=2026-01-01", "&timezone=invalid"} {
					recorder = httptest.NewRecorder()
					router.ServeHTTP(recorder, httptest.NewRequest("GET", query+dates, nil))
					assert.Equal(t, http.StatusBadRequest, recorder.Code)
				}
				require.NoError(t, db.Model(&diagnostics).Update("request_retain_until", now-1).Error)
				recorder = httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest("GET", "/request/"+diagnostics.TaskId, nil))
				assert.Equal(t, http.StatusNotFound, recorder.Code)
				assert.NotContains(t, recorder.Body.String(), "山水")
			})

			t.Run("task search", func(t *testing.T) {
				require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Updates(map[string]any{"requested_size": "1792x1024", "aspect_ratio": "16:9", "prompt_summary": "A futuristic city"}).Error)
				require.NoError(t, db.Create(&model.Channel{Id: 302, Name: "Studio_%! upstream", Key: "search-channel-secret"}).Error)
				searchImages := []model.AsyncImageTask{
					{TaskId: "asyncimg_search_literal", UserId: 101, TokenId: 201, ChannelId: 302, Model: "literal_%!image", Status: model.ImageTaskQueued, CreatedAt: now},
					{TaskId: "asyncimg_search_decoy", UserId: 101, TokenId: 201, Model: "literalXYZ!image", Status: model.ImageTaskQueued, CreatedAt: now},
					{TaskId: "asyncimg_search_foreign", UserId: 102, TokenId: 202, ChannelId: 302, Model: "literal_%!image", Status: model.ImageTaskQueued, CreatedAt: now},
				}
				searchVideos := []model.AsyncMediaJob{
					{TaskId: "video_search_owned", IdentityHash: "search-owned", UserId: 101, TokenId: 201, ChannelId: 301, Model: "SearchVideo-Model", Status: "queued", StorageStatus: "pending", CreatedAt: now, ExpiresAt: now + 3600},
					{TaskId: "video_search_foreign", IdentityHash: "search-foreign", UserId: 102, TokenId: 202, ChannelId: 302, Model: "SearchVideo-Model", Status: "queued", StorageStatus: "pending", CreatedAt: now, ExpiresAt: now + 3600},
				}
				require.NoError(t, db.Create(&searchImages).Error)
				require.NoError(t, db.Create(&searchVideos).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Delete(&searchImages).Error)
					assert.NoError(t, db.Delete(&searchVideos).Error)
					assert.NoError(t, db.Delete(&model.Channel{}, 302).Error)
				})
				for _, handler := range []struct {
					name  string
					media bool
					list  func(*gin.Context, bool)
				}{{"images", false, ListImageTasks}, {"media", true, ListMediaTasks}} {
					t.Run(handler.name, func(t *testing.T) {
						for _, tc := range []struct {
							name     string
							admin    bool
							params   url.Values
							imageIDs []string
							videoIDs []string
						}{
							{name: "complete task ID", params: url.Values{"q": {task.TaskId}}, imageIDs: []string{task.TaskId}},
							{name: "partial task ID", params: url.Values{"q": {"upstream_poll"}}, imageIDs: []string{polling.TaskId}},
							{name: "partial model ignores case and surrounding whitespace", params: url.Values{"q": {"  IMAGE-2.5-SUN  "}}, imageIDs: []string{task.TaskId}},
							{name: "model filter accepts partial names", params: url.Values{"model": {"2.5-SUN"}}, imageIDs: []string{task.TaskId}},
							{name: "requested resolution", params: url.Values{"q": {"2k"}}, imageIDs: []string{task.TaskId}},
							{name: "requested size", params: url.Values{"q": {"1792"}}, imageIDs: []string{task.TaskId}},
							{name: "actual size", params: url.Values{"q": {"1186"}}, imageIDs: []string{task.TaskId}},
							{name: "aspect ratio", params: url.Values{"q": {"16:9"}}, imageIDs: []string{task.TaskId}},
							{name: "prompt remains searchable", params: url.Values{"q": {"futuristic"}}, imageIDs: []string{task.TaskId}},
							{name: "administrator partial channel name", admin: true, params: url.Values{"q": {"BURST UP"}}, imageIDs: []string{task.TaskId, polling.TaskId}, videoIDs: []string{"video_search_owned"}},
							{name: "owner cannot infer channel names", params: url.Values{"q": {"burst up"}}},
							{name: "channel matches obey status filter", admin: true, params: url.Values{"q": {"burst up"}, "status": {"failed"}}},
							{name: "channel matches obey date filter", admin: true, params: url.Values{"q": {"burst up"}, "start_date": {"2099-01-01"}, "end_date": {"2099-01-01"}}},
							{name: "literal wildcards preserve ownership", params: url.Values{"q": {"literal_%!"}}, imageIDs: []string{"asyncimg_search_literal"}},
							{name: "model filter treats wildcards literally", params: url.Values{"model": {"literal_%!"}}, imageIDs: []string{"asyncimg_search_literal"}},
							{name: "channel wildcards are literal", admin: true, params: url.Values{"q": {"Studio_%!"}}, imageIDs: []string{"asyncimg_search_literal", "asyncimg_search_foreign"}, videoIDs: []string{"video_search_foreign"}},
							{name: "other user task ID stays hidden", params: url.Values{"q": {"search_foreign"}}},
							{name: "video model search preserves ownership", params: url.Values{"q": {"VIDEO-MOD"}}, videoIDs: []string{"video_search_owned"}},
							{name: "video model filter accepts partial names", params: url.Values{"model": {"VIDEO-MOD"}}, videoIDs: []string{"video_search_owned"}},
						} {
							t.Run(tc.name, func(t *testing.T) {
								date := time.Unix(now, 0).UTC().Format("2006-01-02")
								if tc.params.Get("start_date") == "" {
									tc.params.Set("start_date", date)
									tc.params.Set("end_date", date)
								}
								tc.params.Set("timezone", "UTC")
								recorder := httptest.NewRecorder()
								c, _ := gin.CreateTestContext(recorder)
								c.Set("id", 101)
								c.Request = httptest.NewRequest(http.MethodGet, "/?"+tc.params.Encode(), nil)
								handler.list(c, tc.admin)
								require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
								var response struct {
									Data struct {
										Items []struct {
											TaskID string `json:"task_id"`
										}
										Total int
										Stats struct{ Total int }
									}
								}
								require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
								want := append([]string{}, tc.imageIDs...)
								if handler.media {
									want = append(want, tc.videoIDs...)
								}
								ids := make([]string, 0, len(response.Data.Items))
								for _, item := range response.Data.Items {
									ids = append(ids, item.TaskID)
								}
								assert.ElementsMatch(t, want, ids)
								assert.Equal(t, len(want), response.Data.Total)
								assert.Equal(t, len(want), response.Data.Stats.Total)
							})
						}
					})
				}
			})

			t.Run("displayed platform filters", func(t *testing.T) {
				images := []model.AsyncImageTask{
					{TaskId: "asyncimg_platform_legacy_openai", UserId: 101, TokenId: 201, Group: "platform-filter", Platform: "openai", Status: model.ImageTaskQueued, CreatedAt: now},
					{TaskId: "asyncimg_platform_legacy_gemini", UserId: 101, TokenId: 201, Group: "platform-filter", Platform: "gemini", Status: model.ImageTaskQueued, CreatedAt: now},
					{TaskId: "asyncimg_platform_provider_gemini", UserId: 101, TokenId: 201, Group: "platform-filter", Platform: "openai", Provider: "gemini", Status: model.ImageTaskQueued, CreatedAt: now},
					{TaskId: "asyncimg_platform_other_provider", UserId: 101, TokenId: 201, Group: "platform-filter", Platform: "openai", Provider: "doubao", Status: model.ImageTaskQueued, CreatedAt: now},
					{TaskId: "asyncimg_platform_foreign", UserId: 102, TokenId: 202, Group: "platform-filter", Platform: "gemini", Provider: "gemini", Status: model.ImageTaskQueued, CreatedAt: now},
				}
				videos := []model.AsyncMediaJob{
					{TaskId: "video_platform_openai", IdentityHash: "platform-openai", UserId: 101, TokenId: 201, Group: "platform-filter", Provider: "openai", Status: "queued", StorageStatus: "pending", CreatedAt: now, ExpiresAt: now + 3600},
					{TaskId: "video_platform_gemini", IdentityHash: "platform-gemini", UserId: 101, TokenId: 201, Group: "platform-filter", Provider: "gemini", Status: "queued", StorageStatus: "pending", CreatedAt: now, ExpiresAt: now + 3600},
				}
				require.NoError(t, db.Create(&images).Error)
				require.NoError(t, db.Model(&images[1]).Update("provider", nil).Error)
				require.NoError(t, db.Create(&videos).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Delete(&images).Error)
					assert.NoError(t, db.Delete(&videos).Error)
				})
				for _, handler := range []struct {
					name  string
					media bool
					list  func(*gin.Context, bool)
				}{{"images", false, ListImageTasks}, {"media", true, ListMediaTasks}} {
					t.Run(handler.name, func(t *testing.T) {
						for _, tc := range []struct {
							name     string
							admin    bool
							platform string
							imageIDs []string
							videoIDs []string
						}{
							{name: "OpenAI excludes a different displayed provider", platform: "openai", imageIDs: []string{images[0].TaskId}, videoIDs: []string{videos[0].TaskId}},
							{name: "Gemini includes provider and legacy NULL fallback within owner", platform: "gemini", imageIDs: []string{images[1].TaskId, images[2].TaskId}, videoIDs: []string{videos[1].TaskId}},
							{name: "administrator Gemini includes other users", admin: true, platform: "gemini", imageIDs: []string{images[1].TaskId, images[2].TaskId, images[4].TaskId}, videoIDs: []string{videos[1].TaskId}},
							{name: "unselected platform includes all owner providers", imageIDs: []string{images[0].TaskId, images[1].TaskId, images[2].TaskId, images[3].TaskId}, videoIDs: []string{videos[0].TaskId, videos[1].TaskId}},
							{name: "unmatched platform returns empty results and statistics", platform: "missing"},
						} {
							t.Run(tc.name, func(t *testing.T) {
								date := time.Unix(now, 0).UTC().Format("2006-01-02")
								params := url.Values{"group": {"platform-filter"}, "platform": {tc.platform}, "start_date": {date}, "end_date": {date}, "timezone": {"UTC"}}
								recorder := httptest.NewRecorder()
								c, _ := gin.CreateTestContext(recorder)
								c.Set("id", 101)
								c.Request = httptest.NewRequest(http.MethodGet, "/?"+params.Encode(), nil)
								handler.list(c, tc.admin)
								require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
								var response struct {
									Data struct {
										Items []struct {
											TaskID string `json:"task_id"`
										}
										Total int
										Stats struct{ Total int }
									}
								}
								require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
								want := append([]string{}, tc.imageIDs...)
								if handler.media {
									want = append(want, tc.videoIDs...)
								}
								ids := make([]string, 0, len(response.Data.Items))
								for _, item := range response.Data.Items {
									ids = append(ids, item.TaskID)
								}
								assert.ElementsMatch(t, want, ids)
								assert.Equal(t, len(want), response.Data.Total)
								assert.Equal(t, len(want), response.Data.Stats.Total)
							})
						}
					})
				}
			})

			t.Run("successful actual cost follows filters across every page", func(t *testing.T) {
				images := []model.AsyncImageTask{
					{TaskId: "cost_image_alpha", UserId: 101, TokenId: 201, ChannelId: 301, Group: "cost-summary", Platform: "openai", Model: "cost-alpha", Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", Quota: 125000, CreatedAt: now},
					{TaskId: "cost_image_beta", UserId: 101, TokenId: 201, Group: "cost-summary", Platform: "gemini", Model: "cost-beta", Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", Quota: 375000, CreatedAt: now},
					{TaskId: "cost_image_free", UserId: 101, TokenId: 201, Group: "cost-summary", Status: model.ImageTaskSucceeded, BillingStatus: "not_billable", CreatedAt: now},
					{TaskId: "cost_image_unsettled", UserId: 101, TokenId: 201, Group: "cost-summary", Status: model.ImageTaskSucceeded, BillingStatus: "pending", Quota: 910000, CreatedAt: now},
					{TaskId: "cost_image_pending", UserId: 101, TokenId: 201, Group: "cost-summary", Status: model.ImageTaskBillingPending, BillingStatus: "pending", Quota: 300000, CreatedAt: now},
					{TaskId: "cost_image_failed", UserId: 101, TokenId: 201, Group: "cost-summary", Status: model.ImageTaskFailed, BillingStatus: "succeeded", Quota: 400000, CreatedAt: now},
					{TaskId: "cost_image_refunded", UserId: 101, TokenId: 201, Group: "cost-summary", Status: model.ImageTaskFailed, BillingStatus: "refunded", Quota: 800000, CreatedAt: now},
					{TaskId: "cost_image_foreign", UserId: 102, TokenId: 202, Group: "cost-summary", Platform: "openai", Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", Quota: 500000, CreatedAt: now},
				}
				videos := []model.AsyncMediaJob{
					{TaskId: "cost_video_final", IdentityHash: "cost-video-final", UserId: 101, TokenId: 201, ChannelId: 301, Group: "cost-summary", Provider: "openai", Status: "succeeded", StorageStatus: "upstream", BillingStatus: "settled", Quota: 999999, CreatedAt: now, ExpiresAt: now + 3600},
					{TaskId: "cost_video_standalone", IdentityHash: "cost-video-standalone", UserId: 101, TokenId: 201, Group: "cost-summary", Provider: "gemini", Status: "succeeded", StorageStatus: "upstream", BillingStatus: "settled", Quota: 250000, CreatedAt: now, ExpiresAt: now + 3600},
					{TaskId: "cost_video_reserved", IdentityHash: "cost-video-reserved", UserId: 101, TokenId: 201, Group: "cost-summary", Status: "submitted", StorageStatus: "pending", BillingStatus: "reserved", Quota: 900000, CreatedAt: now, ExpiresAt: now + 3600},
					{TaskId: "cost_video_failed", IdentityHash: "cost-video-failed", UserId: 101, TokenId: 201, Group: "cost-summary", Status: "failed", StorageStatus: "pending", BillingStatus: "refunded", Quota: 800000, CreatedAt: now, ExpiresAt: now + 3600},
					{TaskId: "cost_video_billing_failed", IdentityHash: "cost-video-billing-failed", UserId: 101, TokenId: 201, Group: "cost-summary", Status: "succeeded", StorageStatus: "upstream", BillingStatus: "failed", Quota: 700000, CreatedAt: now, ExpiresAt: now + 3600},
				}
				settled := model.Task{TaskID: videos[0].TaskId, UserId: 101, Status: model.TaskStatusSuccess, Quota: 625000, FinishTime: now}
				require.NoError(t, db.Create(&images).Error)
				require.NoError(t, db.Create(&videos).Error)
				require.NoError(t, db.Create(&settled).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Delete(&images).Error)
					assert.NoError(t, db.Delete(&videos).Error)
					assert.NoError(t, db.Delete(&settled).Error)
				})
				for _, handler := range []struct {
					name string
					list func(*gin.Context, bool)
				}{{"images", ListImageTasks}, {"media", ListMediaTasks}} {
					t.Run(handler.name, func(t *testing.T) {
						for _, tc := range []struct {
							name       string
							admin      bool
							params     url.Values
							imageQuota int64
							videoQuota int64
						}{
							{name: "administrator all matches", admin: true, imageQuota: 1000000, videoQuota: 875000},
							{name: "owner only", imageQuota: 500000, videoQuota: 875000},
							{name: "last page keeps full summary", admin: true, params: url.Values{"page": {"2"}}, imageQuota: 1000000, videoQuota: 875000},
							{name: "success filter", admin: true, params: url.Values{"status": {"succeeded"}}, imageQuota: 1000000, videoQuota: 875000},
							{name: "failed excludes previously charged and refunded", admin: true, params: url.Values{"status": {"failed"}}},
							{name: "billing filter", admin: true, params: url.Values{"billing_status": {"succeeded"}}, imageQuota: 1000000},
							{name: "free successful tasks", admin: true, params: url.Values{"billing_status": {"not_billable"}}},
							{name: "platform filter", admin: true, params: url.Values{"platform": {"openai"}}, imageQuota: 625000, videoQuota: 625000},
							{name: "model filter", admin: true, params: url.Values{"model": {"cost-alpha"}}, imageQuota: 125000},
							{name: "channel search", admin: true, params: url.Values{"q": {"Sunburst"}}, imageQuota: 125000, videoQuota: 625000},
							{name: "API key filter", admin: true, params: url.Values{"api_key_id": {"202"}}, imageQuota: 500000},
							{name: "date excludes all", admin: true, params: url.Values{"start_date": {"2099-01-01"}, "end_date": {"2099-01-01"}}},
							{name: "no matches", admin: true, params: url.Values{"q": {"missing-cost-task"}}},
						} {
							t.Run(tc.name, func(t *testing.T) {
								params := tc.params
								if params == nil {
									params = url.Values{}
								}
								params.Set("group", "cost-summary")
								params.Set("page_size", "1")
								params.Set("timezone", "UTC")
								if params.Get("start_date") == "" {
									date := time.Unix(now, 0).UTC().Format("2006-01-02")
									params.Set("start_date", date)
									params.Set("end_date", date)
								}
								recorder := httptest.NewRecorder()
								c, _ := gin.CreateTestContext(recorder)
								c.Set("id", 101)
								c.Request = httptest.NewRequest(http.MethodGet, "/?"+params.Encode(), nil)
								handler.list(c, tc.admin)
								require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
								var response struct {
									Data struct {
										Items []gin.H
										Stats struct {
											SucceededCost *float64 `json:"succeeded_cost"`
										}
									}
								}
								require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
								require.NotNil(t, response.Data.Stats.SucceededCost)
								quota := tc.imageQuota
								if handler.name == "media" {
									quota += tc.videoQuota
								}
								assert.Equal(t, float64(quota)/common.QuotaPerUnit, *response.Data.Stats.SucceededCost)
								assert.LessOrEqual(t, len(response.Data.Items), 1)
							})
						}
					})
				}
			})

			t.Run("platform options follow configured channels instead of task rows", func(t *testing.T) {
				previousCapability, previousRegistry := service.ImageChannelCapabilityFunc, jsplugin.DefaultRegistry
				jsplugin.DefaultRegistry = jsplugin.NewRegistry()
				service.ImageChannelCapabilityFunc = func(channel model.Channel, modelName string) dto.ImageCapability {
					if modelName != "image-model" {
						return dto.ImageCapability{}
					}
					providers := map[int]string{constant.ChannelTypeOpenAI: "openai", constant.ChannelTypeGemini: "gemini", constant.ChannelTypeAzure: "azure"}
					provider := providers[channel.Type]
					return dto.ImageCapability{Provider: provider, Generate: provider != ""}
				}
				t.Cleanup(func() {
					service.ImageChannelCapabilityFunc, jsplugin.DefaultRegistry = previousCapability, previousRegistry
				})
				require.NoError(t, jsplugin.DefaultRegistry.ReplaceOverrides([]*jsplugin.LoadedPlugin{
					{Meta: jsplugin.Meta{Key: "kling", Models: []string{"video-model"}, ChannelTypes: []int{constant.ChannelTypeKling}, Protocols: []jsplugin.ProtocolClaim{{Name: "openai_video"}}}},
					{Meta: jsplugin.Meta{Key: "unconfigured-video", Models: []string{"video-model"}, Protocols: []jsplugin.ProtocolClaim{{Name: "openai_video"}}}},
				}))
				pluginSetting := `{"task_plugin_key":"kling"}`
				channels := []model.Channel{
					{Id: 401, Type: constant.ChannelTypeOpenAI, Models: "image-model", Name: "private-channel", Key: "private-key"},
					{Id: 402, Type: constant.ChannelTypeOpenAI, Models: "image-model"},
					{Id: 403, Type: constant.ChannelTypeGemini, Models: " image-model , chat-model "},
					{Id: 404, Type: constant.ChannelTypeAzure, Models: "image-model", Status: common.ChannelStatusManuallyDisabled},
					{Id: 405, Type: constant.ChannelTypeTaskPlugin, Models: "video-model", Setting: &pluginSetting},
					{Id: 406, Type: constant.ChannelTypeKling, Models: "video-model"},
					{Id: 407, Type: constant.ChannelTypeTaskPlugin, Models: "video-model"},
					{Id: 408, Type: constant.ChannelTypeGemini, Models: "chat-model"},
				}
				require.NoError(t, db.Create(&channels).Error)
				t.Cleanup(func() { assert.NoError(t, db.Delete(&channels).Error) })
				for _, removeChannels := range []bool{false, true} {
					if removeChannels {
						require.NoError(t, db.Delete(&channels).Error)
					}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodGet, "/api/user/media-tasks/platforms", nil)
					GetMediaTaskPlatforms(c)
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					var response struct {
						Success bool
						Data    []string
					}
					require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
					require.True(t, response.Success)
					if removeChannels {
						assert.Equal(t, []string{}, response.Data, "existing task rows do not retain removed platform options")
					} else {
						assert.Equal(t, []string{"azure", "gemini", "kling", "openai"}, response.Data, "deduplicate configured providers and exclude unbound plugins and chat-only models")
					}
				}
			})

			for _, admin := range []bool{true, false} {
				t.Run(map[bool]string{true: "administrator", false: "owner"}[admin], func(t *testing.T) {
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Set("id", 101)
					date := time.Now().UTC().Format("2006-01-02")
					c.Request = httptest.NewRequest(http.MethodGet, "/?start_date="+date+"&end_date="+date+"&timezone=UTC", nil)
					ListImageTasks(c, admin)
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					var list struct {
						Data struct {
							Items []gin.H
							Stats map[string]float64
						}
					}
					require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &list))
					assert.Equal(t, float64(1), list.Data.Stats["succeeded"])
					assert.Equal(t, float64(1), list.Data.Stats["processing"])
					if admin {
						assert.Len(t, list.Data.Items, 3)
					} else {
						require.Len(t, list.Data.Items, 2)
						assert.Equal(t, float64(0), list.Data.Stats["queued"])
					}
					for _, item := range list.Data.Items {
						if item["id"] == polling.TaskId {
							assert.Equal(t, model.ImageTaskInvoking, item["status"])
							assert.Equal(t, "generating", item["stage"])
						}
						if item["id"] != task.TaskId {
							continue
						}
						assert.Equal(t, "Studio Key", item["api_key_name"])
						assert.Equal(t, []any{"local"}, item["storage_providers"])
						if admin {
							assert.Equal(t, "Creative account", item["user_name"])
							assert.Equal(t, "Sunburst upstream", item["channel_name"])
						} else {
							for _, field := range []string{"channel_name", "user_name", "channel_id", "user_id", "attempts", "attempt_history", "reference_urls"} {
								assert.NotContains(t, item, field)
							}
						}
					}
					for _, secret := range []string{"password-secret-canary", "token-secret-canary", "channel-secret-canary", "private-storage-canary", "private-object-canary"} {
						assert.NotContains(t, recorder.Body.String(), secret)
					}
					assert.NotContains(t, recorder.Body.String(), polling.UpstreamTaskId)
					detailRecorder := httptest.NewRecorder()
					detailContext, _ := gin.CreateTestContext(detailRecorder)
					detailContext.Set("id", 101)
					detailContext.Params = gin.Params{{Key: "task_id", Value: task.TaskId}}
					detailContext.Request = httptest.NewRequest(http.MethodGet, "/", nil)
					GetImageTask(detailContext, admin)
					require.Equal(t, http.StatusOK, detailRecorder.Code, detailRecorder.Body.String())
					var detail struct {
						Data struct {
							Task    gin.H
							Results []gin.H
						}
					}
					require.NoError(t, common.Unmarshal(detailRecorder.Body.Bytes(), &detail))
					assert.Equal(t, "Studio Key", detail.Data.Task["api_key_name"])
					assert.Len(t, detail.Data.Results, 1)
					if admin {
						history, historyErr := common.Marshal(detail.Data.Task["attempt_history"])
						assert.Contains(t, detailRecorder.Body.String(), "routing-event-canary")
						require.NoError(t, historyErr)
						assert.Contains(t, string(history), "Sunburst upstream")
						assert.Contains(t, string(history), `"http_status":400`)
						assert.Contains(t, string(history), `"error_message":"sanitized provider detail"`)
						assert.Contains(t, string(history), `"provider_code":"invalid_request"`)
						assert.Contains(t, string(history), `"provider_status":"INVALID_ARGUMENT"`)
						assert.Contains(t, string(history), `"upstream_request_id":"provider-request-1"`)
						assert.NotContains(t, string(history), "fingerprint-canary")
						assert.NotContains(t, detailRecorder.Body.String(), "history-private-prompt", "both readable and legacy serialized history must redact echoed input")
					} else {
						assert.NotContains(t, detailRecorder.Body.String(), "Sunburst upstream")
						assert.NotContains(t, detailRecorder.Body.String(), "routing-event-canary")
						assert.NotContains(t, detailRecorder.Body.String(), "fingerprint-canary")
						assert.NotContains(t, detailRecorder.Body.String(), "provider-request-1")
					}
				})
			}
			for status, expected := range map[string]int{model.ImageTaskQueued: 0, model.ImageTaskInvoking: 1} {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Set("id", 101)
				date := time.Now().UTC().Format("2006-01-02")
				c.Request = httptest.NewRequest(http.MethodGet, "/?status="+status+"&start_date="+date+"&end_date="+date+"&timezone=UTC", nil)
				ListImageTasks(c, false)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var response struct {
					Data struct {
						Items []gin.H
					}
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				assert.Len(t, response.Data.Items, expected)
			}
			deniedRecorder := httptest.NewRecorder()
			denied, _ := gin.CreateTestContext(deniedRecorder)
			denied.Set("id", 102)
			denied.Params = gin.Params{{Key: "task_id", Value: task.TaskId}}
			denied.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			GetImageTask(denied, false)
			assert.Equal(t, http.StatusNotFound, deniedRecorder.Code)
			assert.False(t, strings.Contains(deniedRecorder.Body.String(), "Studio Key"))

			recoverable := model.AsyncImageTask{TaskId: "asyncimg_recoverable", UserId: 101, TokenId: 201, Status: model.ImageTaskStorageFailed, ImageCount: 1, CreatedAt: now}
			require.NoError(t, db.Create(&recoverable).Error)
			require.NoError(t, db.Create(&model.AsyncImageBill{TaskId: recoverable.TaskId, BillingRequestId: "recoverable-bill", UserId: 101, TokenId: 201, CreatedAt: now}).Error)
			deniedRecorder = httptest.NewRecorder()
			denied, _ = gin.CreateTestContext(deniedRecorder)
			denied.Set("id", 102)
			denied.Params = gin.Params{{Key: "task_id", Value: recoverable.TaskId}}
			denied.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			ManageMediaTask(denied, false, "resume")
			assert.Equal(t, http.StatusNotFound, deniedRecorder.Code)
			ownerRecorder := httptest.NewRecorder()
			owner, _ := gin.CreateTestContext(ownerRecorder)
			owner.Set("id", 101)
			owner.Params = gin.Params{{Key: "task_id", Value: recoverable.TaskId}}
			owner.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			ManageMediaTask(owner, false, "resume")
			require.Equal(t, http.StatusOK, ownerRecorder.Code, ownerRecorder.Body.String())
			require.NoError(t, db.Where("task_id = ?", recoverable.TaskId).Take(&recoverable).Error)
			assert.Equal(t, model.ImageTaskUpstreamSucceeded, recoverable.Status)
			t.Run("uncertain billing requires administrator quota reconciliation", func(t *testing.T) {
				for _, tc := range []struct {
					name, body string
					admin      bool
					direct     bool
					want       int
				}{
					{name: "administrator_missing_confirmation", admin: true, direct: true, body: `{}`, want: http.StatusBadRequest},
					{name: "administrator_false_confirmation", admin: true, direct: true, body: `{"confirm_quota_reconciled":false}`, want: http.StatusBadRequest},
					{name: "administrator_invalid_confirmation", admin: true, direct: true, body: `{"confirm_quota_reconciled":`, want: http.StatusBadRequest},
					{name: "owner_cannot_resume", body: `{}`, want: http.StatusConflict},
					{name: "owner_cannot_bypass_with_flag", body: `{"confirm_quota_reconciled":true}`, want: http.StatusConflict},
					{name: "administrator_confirmed", admin: true, direct: true, body: `{"confirm_quota_reconciled":true}`, want: http.StatusOK},
					{name: "unified_administrator_confirmed", admin: true, body: `{"confirm_quota_reconciled":true}`, want: http.StatusOK},
				} {
					t.Run(tc.name, func(t *testing.T) {
						uncertain := model.AsyncImageTask{TaskId: "asyncimg_billing_uncertain_" + tc.name, UserId: 101, TokenId: 201, Version: 1, Status: model.ImageTaskBillingFailed, BillingStatus: "failed", ErrorCode: "billing_quota_uncertain", ErrorMessage: "Quota mutation result needs reconciliation", PublicErrorCode: 609, UpstreamSucceededAt: now - 5, ImageCount: 1, ResultCount: 1, BillingRetryCount: 2, CreatedAt: now, ExpiresAt: now + 3600}
						require.NoError(t, db.Create(&uncertain).Error)
						bill := model.AsyncImageBill{TaskId: uncertain.TaskId, BillingRequestId: "uncertain-bill-" + tc.name, UserId: 101, TokenId: 201, Status: "reserved", CreatedAt: now}
						require.NoError(t, db.Create(&bill).Error)
						t.Cleanup(func() {
							assert.NoError(t, db.Where("aggregate_id = ?", uncertain.TaskId).Delete(&model.ImageOutbox{}).Error)
							assert.NoError(t, db.Where("task_id = ?", uncertain.TaskId).Delete(&model.AsyncImageEvent{}).Error)
							assert.NoError(t, db.Delete(&bill).Error)
							assert.NoError(t, db.Delete(&uncertain).Error)
						})
						assert.Equal(t, false, imageTaskDTO(uncertain, false)["can_resume"])
						assert.Equal(t, true, imageTaskDTO(uncertain, true)["can_resume"])
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Set("id", 101)
						c.Params = gin.Params{{Key: "task_id", Value: uncertain.TaskId}}
						c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/media-tasks/"+uncertain.TaskId+"/resume", strings.NewReader(tc.body))
						if tc.direct {
							ManageImageTask(c, "resume")
						} else {
							ManageMediaTask(c, tc.admin, "resume")
						}
						require.Equal(t, tc.want, recorder.Code, recorder.Body.String())
						var stored model.AsyncImageTask
						require.NoError(t, db.Where("id = ?", uncertain.Id).Take(&stored).Error)
						var storedBill model.AsyncImageBill
						require.NoError(t, db.Where("id = ?", bill.Id).Take(&storedBill).Error)
						assert.Equal(t, bill, storedBill, "manual recovery must never mutate the wallet or replay billing in the HTTP handler")
						if tc.want != http.StatusOK {
							assert.Equal(t, uncertain, stored, "unconfirmed recovery must leave all accounting and task fields unchanged")
							return
						}
						assert.Equal(t, model.ImageTaskBillingPending, stored.Status)
						assert.Empty(t, stored.ErrorCode)
						assert.Zero(t, stored.PublicErrorCode)
						assert.Positive(t, stored.NextAttemptAt)
						assert.Equal(t, uncertain.UpstreamSucceededAt, stored.UpstreamSucceededAt)
						assert.Equal(t, uncertain.ImageCount, stored.ImageCount)
						assert.Equal(t, uncertain.ResultCount, stored.ResultCount)
					})
				}
			})

			t.Run("Drama upstream queue and progress remain separate from local processing", func(t *testing.T) {
				job := model.AsyncMediaJob{TaskId: "task_drama_progress", IdentityHash: "drama-progress", UserId: 101, TokenId: 201, ChannelId: 301, Provider: "drama", Model: "seedance2.0-A", Status: "submitted", StorageStatus: "pending", BillingStatus: "reserved", DispatchedAt: now - 5, CreatedAt: now, ExpiresAt: now + 3600}
				upstream := model.Task{TaskID: job.TaskId, UserId: job.UserId, ChannelId: job.ChannelId, Platform: "drama", Action: "text_to_video", Status: model.TaskStatusQueued, Progress: "0%", Data: []byte(`{"status":"queued","progress":0,"id":"private-drama-id"}`)}
				require.NoError(t, db.Create(&job).Error)
				require.NoError(t, db.Create(&upstream).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Delete(&upstream).Error)
					assert.NoError(t, db.Delete(&job).Error)
				})
				for _, tc := range []struct {
					name, jobStatus, storage, provider, data, taskProgress, status, stage string
					taskStatus                                                            model.TaskStatus
					progress                                                              int
					taskQuota                                                             int
					jobBilling, billing                                                   string
					upstreamProgress                                                      any
					expired                                                               bool
					noTask, terminable                                                    bool
					output                                                                string
				}{
					{name: "local queue before submission", jobStatus: "queued", noTask: true, terminable: true, status: "queued", stage: "queued", progress: 0},
					{name: "submission before task receipt", jobStatus: "submitting", noTask: true, status: "invoking", stage: "generating", progress: 10},
					{name: "unknown submission is a public terminal failure", jobStatus: "execution_unknown", noTask: true, status: "execution_unknown", stage: "failed", progress: 0},
					{name: "not started", taskStatus: model.TaskStatusNotStart, taskProgress: "0%", data: `{"status":"queued","progress":0}`, status: "queued", stage: "queued", progress: 10, upstreamProgress: float64(0)},
					{name: "submitted", taskStatus: model.TaskStatusSubmitted, taskProgress: "10%", data: `{"status":"queued","progress":0}`, status: "queued", stage: "queued", progress: 10, upstreamProgress: float64(0)},
					{name: "queued", taskStatus: model.TaskStatusQueued, taskProgress: "20%", data: `{"status":"queued","progress":0}`, status: "queued", stage: "queued", progress: 14, upstreamProgress: float64(0)},
					{name: "generating", taskStatus: model.TaskStatusInProgress, taskProgress: "50%", data: `{"status":"in_progress","progress":37.5}`, status: "invoking", stage: "generating", progress: 35, upstreamProgress: float64(37.5)},
					{name: "missing upstream progress", taskStatus: model.TaskStatusInProgress, taskProgress: "30%", data: `{"status":"in_progress"}`, status: "invoking", stage: "generating", progress: 21},
					{name: "invalid upstream progress", taskStatus: model.TaskStatusInProgress, taskProgress: "30%", data: `{"status":"in_progress","progress":101}`, status: "invoking", stage: "generating", progress: 21},
					{name: "awaiting upstream billing", taskStatus: model.TaskStatusInProgress, taskProgress: "99%", data: `{"status":"completed","progress":100}`, status: "invoking", stage: "generating", progress: 69, upstreamProgress: float64(100)},
					{name: "upstream completed", taskStatus: model.TaskStatusSuccess, taskProgress: "100%", data: `{"status":"completed","progress":100}`, status: "upstream_succeeded", stage: "saving", progress: 70, upstreamProgress: float64(100)},
					{name: "known completion supersedes unknown submission", jobStatus: "execution_unknown", taskStatus: model.TaskStatusSuccess, taskProgress: "100%", data: `{"status":"completed","progress":100}`, status: "upstream_succeeded", stage: "saving", progress: 70, upstreamProgress: float64(100)},
					{name: "completed awaiting storage", jobStatus: "succeeded", taskStatus: model.TaskStatusSuccess, taskProgress: "100%", data: `{"status":"completed","progress":100}`, status: "upstream_succeeded", stage: "saving", progress: 70, upstreamProgress: float64(100)},
					{name: "saving takes precedence", taskStatus: model.TaskStatusQueued, storage: "saving", taskProgress: "20%", data: `{"status":"queued","progress":0}`, status: "uploading", stage: "saving", progress: 80, upstreamProgress: float64(0)},
					{name: "storage failure takes precedence", taskStatus: model.TaskStatusQueued, storage: "failed", taskProgress: "20%", data: `{"status":"queued","progress":0}`, status: "storage_failed", stage: "saving", progress: 80, upstreamProgress: float64(0)},
					{name: "completed job takes precedence", taskStatus: model.TaskStatusQueued, jobStatus: "succeeded", storage: "upstream", taskProgress: "20%", data: `{"status":"queued","progress":0}`, status: "succeeded", stage: "completed", progress: 100, upstreamProgress: float64(0)},
					{name: "missing saved output", taskStatus: model.TaskStatusSuccess, jobStatus: "succeeded", storage: "succeeded", taskProgress: "100%", data: `{"status":"completed","progress":100}`, status: "expired", stage: "failed", progress: 100, upstreamProgress: float64(100)},
					{name: "expired saved output", taskStatus: model.TaskStatusSuccess, jobStatus: "succeeded", storage: "succeeded", output: "expired", taskProgress: "100%", data: `{"status":"completed","progress":100}`, status: "expired", stage: "failed", progress: 100, upstreamProgress: float64(100)},
					{name: "foreign saved output", taskStatus: model.TaskStatusSuccess, jobStatus: "succeeded", storage: "succeeded", output: "foreign", taskProgress: "100%", data: `{"status":"completed","progress":100}`, status: "expired", stage: "failed", progress: 100, upstreamProgress: float64(100)},
					{name: "failure takes precedence", taskStatus: model.TaskStatusFailure, taskProgress: "100%", data: `{"status":"failed","progress":100}`, status: "failed", stage: "failed", progress: 10, upstreamProgress: float64(100)},
					{name: "failed task refund remains unconfirmed", taskStatus: model.TaskStatusFailure, taskProgress: "100%", taskQuota: 120, billing: "failed", data: `{"status":"failed","progress":100}`, status: "failed", stage: "failed", progress: 10, upstreamProgress: float64(100)},
					{name: "zero task quota cannot clear known billing failure", taskStatus: model.TaskStatusFailure, taskProgress: "100%", jobBilling: "failed", billing: "failed", data: `{"status":"failed","progress":100}`, status: "failed", stage: "failed", progress: 10, upstreamProgress: float64(100)},
					{name: "expiry takes precedence", taskStatus: model.TaskStatusQueued, taskProgress: "20%", data: `{"status":"queued","progress":0}`, status: "expired", stage: "failed", progress: 100, expired: true, upstreamProgress: float64(0)},
					{name: "other providers retain their status", taskStatus: model.TaskStatusQueued, provider: "sora", taskProgress: "20%", data: `{"status":"queued","progress":0}`, status: "invoking", stage: "generating", progress: 14},
				} {
					t.Run(tc.name, func(t *testing.T) {
						job.Status, job.StorageStatus, job.Provider, job.ExpiresAt, job.DispatchedAt = "submitted", "pending", "drama", now+3600, now-5
						job.BillingStatus, job.Quota, job.ErrorMessage = "reserved", 0, ""
						if tc.jobBilling != "" {
							job.BillingStatus = tc.jobBilling
						}
						if tc.jobStatus != "" {
							job.Status = tc.jobStatus
						}
						if job.Status == "execution_unknown" {
							job.BillingStatus, job.Quota = "unknown", 2250000
							job.ErrorMessage = "Upstream submission could not be confirmed; the video operation will not be repeated"
						}
						if tc.storage != "" {
							job.StorageStatus = tc.storage
						}
						if tc.provider != "" {
							job.Provider = tc.provider
						}
						if tc.expired {
							job.ExpiresAt = now - 1
						}
						if tc.terminable {
							job.DispatchedAt = 0
						}
						upstream.Platform = constant.TaskPlatform(job.Provider)
						upstream.Status, upstream.Progress, upstream.Data = tc.taskStatus, tc.taskProgress, []byte(tc.data)
						upstream.Quota = tc.taskQuota
						require.NoError(t, db.Save(&job).Error)
						if tc.noTask {
							require.NoError(t, db.Delete(&upstream).Error)
						} else {
							require.NoError(t, db.Save(&upstream).Error)
						}
						if tc.output != "" {
							object := model.MediaArtifactObject{ObjectId: "drama-output-" + tc.output, IdentityHash: "drama-output-" + tc.output, TaskId: job.TaskId, UserId: job.UserId, Status: "active", ExpiresAt: now + 3600}
							if tc.output == "expired" {
								object.ExpiresAt = now - 1
							} else {
								object.UserId = 102
							}
							require.NoError(t, db.Create(&object).Error)
							t.Cleanup(func() { assert.NoError(t, db.Delete(&object).Error) })
						}
						for _, surface := range []string{"public", "owner", "administrator"} {
							recorder := httptest.NewRecorder()
							c, _ := gin.CreateTestContext(recorder)
							c.Set("id", 101)
							c.Set("token_id", 201)
							c.Params = gin.Params{{Key: "task_id", Value: job.TaskId}}
							c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
							if surface == "public" {
								QueryAsyncMedia(c)
							} else {
								GetMediaTask(c, surface == "administrator")
							}
							require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
							var view gin.H
							require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &view))
							if surface != "public" {
								view = view["data"].(map[string]any)["task"].(map[string]any)
							}
							expectedStatus := tc.status
							expectedTerminable := tc.terminable
							if tc.status == "execution_unknown" {
								expectedStatus = "failed"
								assert.Equal(t, "unknown", view["billing_status"], surface)
								assert.Equal(t, float64(job.Quota), view["quota"], surface)
								assert.Equal(t, 4.5, view["cost"], surface)
								assert.Zero(t, view["finished_at"], surface)
								assert.False(t, view["can_resume"].(bool), surface)
								if surface == "public" {
									assert.Equal(t, "execution_unknown", view["error_code"])
									assert.Equal(t, false, view["error_retryable"])
								} else {
									expectedTerminable = surface == "administrator"
								}
							} else {
								assert.NotEqual(t, "execution_unknown", view["error_code"], surface)
								assert.NotContains(t, view, "error_retryable", surface)
							}
							assert.Equal(t, expectedStatus, view["status"], surface)
							if tc.billing != "" {
								assert.Equal(t, tc.billing, view["billing_status"], surface)
							}
							assert.Equal(t, tc.stage, view["stage"], surface)
							assert.Equal(t, float64(tc.progress), view["progress"], surface)
							if tc.upstreamProgress == nil {
								assert.NotContains(t, view, "upstream_progress", surface)
							} else {
								assert.Equal(t, tc.upstreamProgress, view["upstream_progress"], surface)
							}
							assert.Equal(t, expectedTerminable, view["can_terminate"], surface)
						}
						var persisted model.AsyncMediaJob
						require.NoError(t, db.Where("id = ?", job.ID).Take(&persisted).Error)
						assert.Equal(t, job, persisted, "queries must preserve the job, quotation, billing and scheduling state")
						filterStatus := tc.status
						if filterStatus == "execution_unknown" {
							filterStatus = "failed"
						}
						for _, admin := range []bool{false, true} {
							for _, filter := range []string{"", "&status=" + filterStatus, "&stage=" + tc.stage, "&status=queued&stage=generating"} {
								recorder := httptest.NewRecorder()
								c, _ := gin.CreateTestContext(recorder)
								c.Set("id", 101)
								c.Request = httptest.NewRequest(http.MethodGet, "/?task_id="+job.TaskId+filter, nil)
								ListMediaTasks(c, admin)
								var response struct {
									Data struct {
										Items []gin.H
										Total int
										Stats map[string]float64
									}
								}
								require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
								require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
								if filter == "&status=queued&stage=generating" {
									assert.Empty(t, response.Data.Items)
									assert.Zero(t, response.Data.Total)
									continue
								}
								require.Len(t, response.Data.Items, 1, filter)
								assert.Equal(t, 1, response.Data.Total, filter)
								expectedStatus := tc.status
								if expectedStatus == "execution_unknown" {
									expectedStatus = "failed"
								}
								assert.Equal(t, expectedStatus, response.Data.Items[0]["status"], filter)
								if tc.billing != "" {
									assert.Equal(t, tc.billing, response.Data.Items[0]["billing_status"], filter)
								}
								stat := "processing"
								if tc.status == "queued" || tc.status == "succeeded" {
									stat = tc.status
								}
								if tc.stage == "failed" || tc.status == "storage_failed" {
									stat = "failed"
								}
								assert.Equal(t, float64(1), response.Data.Stats[stat], filter)
								if stat != "queued" {
									assert.Zero(t, response.Data.Stats["queued"], filter)
								}
							}
						}
					})
				}
				require.NoError(t, db.Model(&job).Updates(map[string]any{"provider": "drama", "status": "submitted", "storage_status": "pending", "expires_at": now + 3600}).Error)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Set("id", 101)
				c.Params = gin.Params{{Key: "task_id", Value: job.TaskId}}
				c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
				ManageMediaTask(c, false, "terminate")
				assert.Equal(t, http.StatusConflict, recorder.Code, "an accepted upstream task must not become locally terminable")
				var stored model.AsyncMediaJob
				require.NoError(t, db.Where("id = ?", job.ID).Take(&stored).Error)
				assert.Equal(t, "submitted", stored.Status)
				require.NoError(t, db.Delete(&upstream).Error)
				require.NoError(t, db.Model(&job).Updates(map[string]any{"status": "queued", "dispatched_at": 0}).Error)
				localRecorder := httptest.NewRecorder()
				localContext, _ := gin.CreateTestContext(localRecorder)
				localContext.Set("id", 101)
				localContext.Params = c.Params
				localContext.Request = httptest.NewRequest(http.MethodPost, "/", nil)
				ManageMediaTask(localContext, false, "terminate")
				assert.Equal(t, http.StatusOK, localRecorder.Code, "a local queue without an upstream task remains terminable")
			})

			t.Run("mixed media queues preserve owner filters statistics and update ordering", func(t *testing.T) {
				jobs := []model.AsyncMediaJob{
					{TaskId: "mixed_drama_local", Provider: "drama", Status: "queued"},
					{TaskId: "mixed_drama_upstream", Provider: "drama", Status: "submitted"},
					{TaskId: "mixed_drama_running", Provider: "drama", Status: "submitted"},
					{TaskId: "mixed_sora", Provider: "sora", Status: "submitted"},
					{TaskId: "mixed_null_provider", Status: "submitted"},
					{TaskId: "mixed_foreign", Provider: "drama", Status: "submitted", UserId: 102, TokenId: 202},
					{TaskId: "mixed_foreign_task", Provider: "drama", Status: "submitted"},
				}
				for i := range jobs {
					jobs[i].IdentityHash, jobs[i].Group = jobs[i].TaskId, "drama-mixed"
					jobs[i].StorageStatus, jobs[i].CreatedAt, jobs[i].ExpiresAt = "pending", now-100+int64(i), now+3600
					if jobs[i].UserId == 0 {
						jobs[i].UserId, jobs[i].TokenId = 101, 201
					}
				}
				upstreamTasks := []model.Task{
					{TaskID: jobs[1].TaskId, UserId: 101, Platform: "drama", Status: model.TaskStatusQueued, UpdatedAt: now + 20, Data: []byte(`{"status":"queued","progress":0}`)},
					{TaskID: jobs[2].TaskId, UserId: 101, Platform: "drama", Status: model.TaskStatusInProgress, UpdatedAt: now + 10, Data: []byte(`{"status":"in_progress","progress":40}`)},
					{TaskID: jobs[3].TaskId, UserId: 101, Platform: "sora", Status: model.TaskStatusQueued},
					{TaskID: jobs[5].TaskId, UserId: 102, Platform: "drama", Status: model.TaskStatusQueued},
					{TaskID: jobs[6].TaskId, UserId: 102, Platform: "drama", Status: model.TaskStatusQueued, Data: []byte(`{"status":"queued","progress":99}`)},
				}
				images := []model.AsyncImageTask{
					{TaskId: "asyncimg_mixed_queue", UserId: 101, TokenId: 201, Group: "drama-mixed", Status: model.ImageTaskQueued, CreatedAt: now - 80},
					{TaskId: "asyncimg_mixed_running", UserId: 101, TokenId: 201, ChannelId: 301, Group: "drama-mixed", Status: model.ImageTaskInvoking, CreatedAt: now - 70},
				}
				require.NoError(t, db.Create(&jobs).Error)
				require.NoError(t, db.Model(&jobs[4]).Update("provider", nil).Error)
				require.NoError(t, db.Create(&upstreamTasks).Error)
				require.NoError(t, db.Create(&images).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Delete(&images).Error)
					assert.NoError(t, db.Delete(&upstreamTasks).Error)
					assert.NoError(t, db.Delete(&jobs).Error)
				})
				for _, tc := range []struct {
					name, filter              string
					admin                     bool
					ids                       []string
					total, queued, processing int
				}{
					{name: "owner combined queues", filter: "status=queued", ids: []string{jobs[0].TaskId, jobs[1].TaskId, images[0].TaskId}, total: 3, queued: 3},
					{name: "owner generating excludes both drama queues", filter: "stage=generating", ids: []string{jobs[2].TaskId, jobs[3].TaskId, jobs[4].TaskId, jobs[6].TaskId, images[1].TaskId}, total: 5, processing: 5},
					{name: "administrator includes foreign queue", admin: true, filter: "stage=queued", ids: []string{jobs[0].TaskId, jobs[1].TaskId, jobs[5].TaskId, images[0].TaskId}, total: 4, queued: 4},
					{name: "provider intersects queue and stage", filter: "provider=drama&stage=queued&status=queued", ids: []string{jobs[0].TaskId, jobs[1].TaskId}, total: 2, queued: 2},
					{name: "sora queue status retains old classification", filter: "provider=sora&status=queued", total: 0},
					{name: "last page keeps all matching statistics", filter: "status=queued&page=2&page_size=2&sort_by=created_at&sort_order=asc", ids: []string{images[0].TaskId}, total: 3, queued: 3},
					{name: "updated sorting uses polled task timestamp", filter: "provider=drama&sort_by=updated_at&sort_order=desc&page_size=1", ids: []string{jobs[1].TaskId}, total: 4, queued: 2, processing: 2},
				} {
					t.Run(tc.name, func(t *testing.T) {
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Set("id", 101)
						c.Request = httptest.NewRequest(http.MethodGet, "/?group=drama-mixed&start_date=2000-01-01&end_date=2100-01-01&"+tc.filter, nil)
						ListMediaTasks(c, tc.admin)
						require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
						var response struct {
							Data struct {
								Items []gin.H
								Total int
								Stats map[string]float64
							}
						}
						require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
						ids := make([]string, 0, len(response.Data.Items))
						for _, item := range response.Data.Items {
							ids = append(ids, item["task_id"].(string))
							if item["task_id"] == jobs[6].TaskId {
								assert.NotContains(t, item, "upstream_progress", "another owner's task snapshot must not contribute progress")
							}
						}
						assert.ElementsMatch(t, tc.ids, ids)
						assert.Equal(t, tc.total, response.Data.Total)
						assert.Equal(t, float64(tc.total), response.Data.Stats["total"])
						assert.Equal(t, float64(tc.queued), response.Data.Stats["queued"])
						assert.Equal(t, float64(tc.processing), response.Data.Stats["processing"])
					})
				}
			})

			video := model.AsyncMediaJob{TaskId: "task-matrix-video", IdentityHash: "matrix-video", UserId: 101, TokenId: 201, ChannelId: 301, Provider: "sora", Model: "sora-2", Group: "default", Status: "submitted", StorageStatus: "failed", BillingStatus: "settled", Quota: 500, CreatedAt: now, ExpiresAt: now + 3600}
			video, reused, err := model.AcceptAsyncMediaJob(t.Context(), video)
			require.NoError(t, err)
			assert.False(t, reused)
			same, reused, err := model.AcceptAsyncMediaJob(t.Context(), video)
			require.NoError(t, err)
			assert.True(t, reused)
			assert.Equal(t, video.TaskId, same.TaskId)
			conflict := video
			conflict.RequestHash = "different"
			_, _, err = model.AcceptAsyncMediaJob(t.Context(), conflict)
			assert.ErrorIs(t, err, model.ErrImageConflict)
			require.NoError(t, db.Model(&video).Updates(map[string]any{"status": "submitted", "storage_status": "failed", "billing_status": "settled"}).Error)
			for _, query := range []string{"request_type=video", "media_type=video&request_type=video", "request_type=text_to_video"} {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Set("id", 101)
				c.Request = httptest.NewRequest(http.MethodGet, "/?"+query, nil)
				ListMediaTasks(c, false)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var response struct {
					Data struct {
						Items []gin.H
						Total int
					}
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				require.Len(t, response.Data.Items, 1, query)
				assert.Equal(t, video.TaskId, response.Data.Items[0]["id"])
				assert.Equal(t, 1, response.Data.Total)
			}
			// A delayed pair of old upstream jobs must leave room for new work.
			waiting := []model.AsyncMediaJob{
				{TaskId: "wait-old-1", IdentityHash: "wait-old-1", Status: "submitted", CreatedAt: now - 100},
				{TaskId: "wait-old-2", IdentityHash: "wait-old-2", Status: "submitted", CreatedAt: now - 99},
				{TaskId: "wait-new", IdentityHash: "wait-new", Status: "queued", CreatedAt: now},
			}
			require.NoError(t, db.Create(&waiting).Error)
			require.NoError(t, db.Model(&video).Update("next_attempt_at", now+60).Error)
			due, err := model.GetDueAsyncMediaJobs(t.Context(), now, 2)
			require.NoError(t, err)
			require.Len(t, due, 2)
			assert.Equal(t, waiting[0].TaskId, due[0].TaskId)
			assert.Equal(t, waiting[1].TaskId, due[1].TaskId)
			require.NoError(t, db.Model(&model.AsyncMediaJob{}).Where("id IN ?", []int{waiting[0].ID, waiting[1].ID}).Update("next_attempt_at", now+3).Error)
			due, err = model.GetDueAsyncMediaJobs(t.Context(), now+3, 2)
			require.NoError(t, err)
			require.Len(t, due, 2)
			assert.Equal(t, waiting[2].TaskId, due[0].TaskId, "new submissions get a turn even while old upstream jobs remain pending")
			require.NoError(t, db.Model(&waiting[2]).Update("next_attempt_at", now+10).Error)
			arrival := model.AsyncMediaJob{TaskId: "wait-new-arrival", IdentityHash: "wait-new-arrival", Status: "queued", CreatedAt: now + 4}
			require.NoError(t, db.Create(&arrival).Error)
			due, err = model.GetDueAsyncMediaJobs(t.Context(), now+4, 2)
			require.NoError(t, err)
			require.Len(t, due, 2)
			assert.Equal(t, waiting[0].TaskId, due[0].TaskId, "continuous new submissions must not starve overdue upstream reconciliation")
			require.NoError(t, db.Delete(&arrival).Error)
			require.NoError(t, db.Where("id IN ?", []int{waiting[0].ID, waiting[1].ID, waiting[2].ID}).Delete(&model.AsyncMediaJob{}).Error)
			require.NoError(t, db.Model(&video).Update("next_attempt_at", 0).Error)
			t.Run("ready video remains in saving filters and statistics", func(t *testing.T) {
				completed := model.Task{TaskID: video.TaskId, UserId: video.UserId, Status: model.TaskStatusSuccess}
				require.NoError(t, db.Create(&completed).Error)
				for _, provider := range []string{"sora", "drama"} {
					require.NoError(t, db.Model(&video).Updates(map[string]any{"provider": provider, "storage_status": "ready"}).Error)
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Set("id", video.UserId)
					c.Request = httptest.NewRequest(http.MethodGet, "/?media_type=video&stage=saving&status=uploading", nil)
					ListMediaTasks(c, false)
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					var response struct {
						Data struct {
							Items []gin.H
							Total int
							Stats map[string]float64
						}
					}
					require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
					require.Len(t, response.Data.Items, 1, provider)
					assert.Equal(t, "uploading", response.Data.Items[0]["status"])
					assert.Equal(t, "saving", response.Data.Items[0]["stage"])
					assert.Equal(t, "ready", response.Data.Items[0]["storage_status"])
					assert.Equal(t, float64(1), response.Data.Stats["processing"])
				}
				require.NoError(t, db.Delete(&completed).Error)
				require.NoError(t, db.Model(&video).Updates(map[string]any{"provider": "sora", "storage_status": "failed"}).Error)
			})
			for _, owner := range []int{101, 102} {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Set("id", owner)
				c.Request = httptest.NewRequest("GET", "/?media_type=video&provider=sora&stage=saving", nil)
				ListMediaTasks(c, false)
				require.Equal(t, 200, recorder.Code, recorder.Body.String())
				var response struct {
					Data struct {
						Items []gin.H
						Total int
					}
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				if owner == 101 {
					require.Len(t, response.Data.Items, 1)
					assert.Equal(t, "video", response.Data.Items[0]["media_type"])
				} else {
					assert.Empty(t, response.Data.Items)
				}
				assert.NotContains(t, recorder.Body.String(), "token-secret-canary")
			}
			claimed, err := model.ClaimAsyncMediaJob(t.Context(), &video, "matrix-lease", 30)
			require.NoError(t, err)
			require.True(t, claimed)
			duplicate, err := model.ClaimAsyncMediaJob(t.Context(), &video, "other-lease", 30)
			require.NoError(t, err)
			assert.False(t, duplicate)
			require.NoError(t, model.UpdateAsyncMediaJob(t.Context(), video, map[string]any{"lease_token": "", "lease_expires_at": 0}))
			// A mismatched token owner must never expose another user's key name.
			task.TokenId = 202
			items, err := imageTasksToDTO(t.Context(), []model.AsyncImageTask{task}, false, false)
			require.NoError(t, err)
			assert.Empty(t, items[0]["api_key_name"])
			task.Attempts = "null"
			items, err = imageTasksToDTO(t.Context(), []model.AsyncImageTask{task}, true, true)
			require.NoError(t, err)
			assert.Equal(t, true, items[0]["attempt_history_unavailable"])
			// Completed references follow task retention; active requests are retained.
			require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", task.TaskId).Updates(map[string]any{"expires_at": now - 1, "prompt_summary": "expired prompt", "request_cipher": []byte("expired cipher")}).Error)
			require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", "asyncimg_other").Updates(map[string]any{"expires_at": now - 1, "reference_urls": "active-reference"}).Error)
			require.NoError(t, model.EraseExpiredAsyncImageTaskContent(t.Context()))
			require.NoError(t, model.EraseExpiredAsyncImageTaskContent(t.Context()))
			var expired, active model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&expired).Error)
			require.NoError(t, db.Where("task_id = ?", "asyncimg_other").Take(&active).Error)
			assert.Empty(t, expired.ReferenceUrls)
			assert.Empty(t, expired.PromptSummary)
			assert.Empty(t, expired.RequestCipher)
			assert.Equal(t, model.ImageTaskSucceeded, expired.Status)
			assert.Equal(t, "active-reference", active.ReferenceUrls)

			fallback := model.AsyncImageTask{TaskId: "asyncimg_public_fallback", UserId: 101, TokenId: 201, ChannelId: 301, Status: model.ImageTaskInvoking, Version: 1, UpstreamTaskId: "upstream-fallback", LeaseToken: "image-fallback-lease", LeaseExpiresAt: now + 120, ErrorCode: "result_download_failed", CreatedAt: now, ExpiresAt: now + 3600}
			require.NoError(t, db.Create(&fallback).Error)
			firstURL := "https://cdn.example.com/first.png?signature=result-only"
			latestURL := "https://cdn.example.com/latest.png?signature=result-only"
			require.NoError(t, model.RecordAsyncImageUpstreamResult(t.Context(), fallback, []string{firstURL}))
			require.NoError(t, model.RecordAsyncImageUpstreamResult(t.Context(), fallback, []string{latestURL}))
			require.NoError(t, model.ScheduleAsyncImagePoll(t.Context(), fallback, now+10, "upstream_pending", ""))
			var milestoneCount, queueCount int64
			require.NoError(t, db.Model(&model.AsyncImageEvent{}).Where("task_id = ?", fallback.TaskId).Count(&milestoneCount).Error)
			require.NoError(t, db.Model(&model.ImageOutbox{}).Where("aggregate_id = ? AND kind = ?", fallback.TaskId, "execute").Count(&queueCount).Error)
			assert.EqualValues(t, 1, milestoneCount, "repeated polls must not grow the timeline")
			assert.EqualValues(t, 1, queueCount, "suppressing the timeline must keep the durable poll command")
			require.NoError(t, db.Model(&model.ImageOutbox{}).Where("aggregate_id = ? AND kind = ?", fallback.TaskId, "execute").Update("status", "delivered").Error)
			require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", fallback.TaskId).Update("next_attempt_at", now).Error)
			claimedImage, err := model.ClaimAsyncImageTask(t.Context(), fallback.TaskId, "image-second-lease", 120)
			require.NoError(t, err)
			require.NoError(t, model.ScheduleAsyncImagePoll(t.Context(), claimedImage, now+10, "upstream_pending", ""))
			require.NoError(t, db.Model(&model.AsyncImageEvent{}).Where("task_id = ?", fallback.TaskId).Count(&milestoneCount).Error)
			require.NoError(t, db.Model(&model.ImageOutbox{}).Where("aggregate_id = ? AND kind = ?", fallback.TaskId, "execute").Count(&queueCount).Error)
			assert.EqualValues(t, 1, milestoneCount)
			assert.EqualValues(t, 1, queueCount, "delivered poll commands should be compacted")
			for _, key := range []string{"legacy-poll-one", "legacy-poll-two"} {
				require.NoError(t, db.Create(&model.AsyncImageEvent{TaskId: fallback.TaskId, EventKey: key, EventType: "upstream_pending", Status: model.ImageTaskInvoking, CreatedAt: now}).Error)
			}

			queryRecorder := httptest.NewRecorder()
			queryContext, _ := gin.CreateTestContext(queryRecorder)
			queryContext.Set("id", 101)
			queryContext.Set("token_id", 201)
			queryContext.Params = gin.Params{{Key: "task_id", Value: fallback.TaskId}}
			queryContext.Request = httptest.NewRequest(http.MethodGet, "/v1/media/tasks_async/"+fallback.TaskId, nil)
			QueryAsyncImage(queryContext)
			require.Equal(t, http.StatusOK, queryRecorder.Code)
			var queried gin.H
			require.NoError(t, common.Unmarshal(queryRecorder.Body.Bytes(), &queried))
			assert.Empty(t, queried["data"], "unsettled fallback results must not be released")
			assert.NotContains(t, queryRecorder.Body.String(), latestURL)
			otherRecorder := httptest.NewRecorder()
			otherContext, _ := gin.CreateTestContext(otherRecorder)
			otherContext.Set("id", 102)
			otherContext.Set("token_id", 202)
			otherContext.Params = gin.Params{{Key: "task_id", Value: fallback.TaskId}}
			otherContext.Request = httptest.NewRequest(http.MethodGet, "/v1/media/tasks_async/"+fallback.TaskId, nil)
			QueryAsyncImage(otherContext)
			assert.Equal(t, http.StatusNotFound, otherRecorder.Code)
			assert.NotContains(t, otherRecorder.Body.String(), latestURL)

			detailRecorder := httptest.NewRecorder()
			detailContext, _ := gin.CreateTestContext(detailRecorder)
			detailContext.Set("id", 101)
			detailContext.Params = gin.Params{{Key: "task_id", Value: fallback.TaskId}}
			detailContext.Request = httptest.NewRequest(http.MethodGet, "/api/user/async-image-tasks/"+fallback.TaskId, nil)
			GetImageTask(detailContext, false)
			require.Equal(t, http.StatusOK, detailRecorder.Code, detailRecorder.Body.String())
			var fallbackDetail struct {
				Data struct {
					Results []gin.H
					Events  []model.AsyncImageEvent
				}
			}
			require.NoError(t, common.Unmarshal(detailRecorder.Body.Bytes(), &fallbackDetail))
			assert.Empty(t, fallbackDetail.Data.Results, "unsettled fallback results must not be released")
			assert.NotContains(t, detailRecorder.Body.String(), latestURL)
			require.Len(t, fallbackDetail.Data.Events, 2, "legacy repeated poll events should display once")
			assert.Equal(t, "", fallbackDetail.Data.Events[0].Message)

			require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", fallback.TaskId).Updates(map[string]any{"status": model.ImageTaskFailed, "expires_at": now - 1}).Error)
			for _, admin := range []bool{false, true} {
				expiredRecorder := httptest.NewRecorder()
				expiredContext, _ := gin.CreateTestContext(expiredRecorder)
				expiredContext.Set("id", 101)
				expiredContext.Params = gin.Params{{Key: "task_id", Value: fallback.TaskId}}
				expiredContext.Request = httptest.NewRequest(http.MethodGet, "/api/user/async-image-tasks/"+fallback.TaskId, nil)
				GetImageTask(expiredContext, admin)
				require.Equal(t, http.StatusOK, expiredRecorder.Code, expiredRecorder.Body.String())
				var expiredDetail struct {
					Data struct {
						Results []gin.H
					}
				}
				require.NoError(t, common.Unmarshal(expiredRecorder.Body.Bytes(), &expiredDetail))
				assert.Empty(t, expiredDetail.Data.Results, "expired task results must be hidden for owners and administrators")
				assert.NotContains(t, expiredRecorder.Body.String(), latestURL)
			}

			t.Run("video request details survive submission and storage", func(t *testing.T) {
				t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "test")
				t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
				request := service.AsyncVideoRequest{
					Body:        []byte(`{"model":"video-model","prompt":"A cinematic landscape","seconds":"8","image":"https://cdn.example.com/input.png?signature=owner-signature","reference_images":["https://cdn.example.com/input.png?signature=owner-signature"],"metadata":{"content":[{"type":"video_url","video_url":{"url":"https://cdn.example.com/reference.mp4"}},{"type":"audio_url","audio_url":{"url":"https://cdn.example.com/sound.mp3"}}],"authorization":"nested-secret-canary"},"watermark":false,"seed":0,"api_key":"body-key-canary","image_base64":"binary-canary"}`),
					ContentType: "application/json", ClientIP: "client-ip-canary", Channel: model.Channel{Key: "channel-credential-canary"},
					Execution: &model.TaskExecutionSnapshot{RequestID: "private-request-id-canary"},
				}
				request.Details = request.RequestDetails()
				encoded, err := common.Marshal(request)
				require.NoError(t, err)
				job := model.AsyncMediaJob{TaskId: "task_video_request_details", IdentityHash: "video-request-details", UserId: 101, TokenId: 201, ChannelId: 301, Provider: "test-video", Model: "video-model", Status: "queued", StorageStatus: "pending", BillingStatus: "pending", RequestCipher: nil, CreatedAt: now, ExpiresAt: now + 3600, LeaseToken: "video-details-lease", LeaseExpiresAt: now + 120}
				job.RequestCipher, err = service.EncryptImagePayload(encoded, "media-request:"+job.TaskId)
				require.NoError(t, err)
				require.NoError(t, db.Create(&job).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Where("task_id = ?", job.TaskId).Delete(&model.AsyncMediaJob{}).Error)
					assert.NoError(t, db.Where("task_id = ?", job.TaskId).Delete(&model.Task{}).Error)
				})
				previousSubmit, previousPersist := service.SubmitAsyncVideoFunc, service.PersistAsyncVideoFunc
				t.Cleanup(func() { service.SubmitAsyncVideoFunc, service.PersistAsyncVideoFunc = previousSubmit, previousPersist })
				service.SubmitAsyncVideoFunc = func(ctx context.Context, current model.AsyncMediaJob, submitted service.AsyncVideoRequest) error {
					assert.Equal(t, request.Body, submitted.Body, "display filtering must not modify upstream request bytes")
					assert.Equal(t, request.Channel.Key, submitted.Channel.Key)
					return db.WithContext(ctx).Create(&model.Task{TaskID: current.TaskId, UserId: current.UserId, ChannelId: current.ChannelId, Status: model.TaskStatusSubmitted, Action: "reference_to_video", CreatedAt: now}).Error
				}
				cfg := service.DefaultMediaRuntimeConfig()
				require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
				require.NoError(t, db.Where("id = ?", job.ID).Take(&job).Error)
				assert.Equal(t, "submitted", job.Status)
				retained, err := service.DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
				require.NoError(t, err)
				var snapshot map[string]any
				require.NoError(t, common.Unmarshal(retained, &snapshot))
				assert.Len(t, snapshot, 1, "the execution envelope must be replaced by a display-only snapshot")
				assert.Contains(t, snapshot, "request_details")
				for _, secret := range []string{"client-ip-canary", "channel-credential-canary", "private-request-id-canary", "nested-secret-canary", "body-key-canary", "binary-canary", `"Channel"`, `"Billing"`, `"Body"`} {
					assert.NotContains(t, string(retained), secret)
				}
				// Polling an accepted task and saving a completed video preserve
				// the display snapshot without retaining the execution envelope.
				require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
				require.NoError(t, db.Where("id = ?", job.ID).Take(&job).Error)
				require.NoError(t, db.Model(&model.Task{}).Where("task_id = ?", job.TaskId).Updates(map[string]any{"status": model.TaskStatusSuccess, "finish_time": now}).Error)
				saves := 0
				service.PersistAsyncVideoFunc = func(context.Context, model.AsyncMediaJob, *model.Task) error { saves++; return nil }
				require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
				require.NoError(t, db.Where("id = ?", job.ID).Take(&job).Error)
				assert.Equal(t, "ready", job.StorageStatus)
				assert.Zero(t, saves, "status confirmation hands the result to a separate download worker")
				require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
				require.NoError(t, db.Where("id = ?", job.ID).Take(&job).Error)
				assert.Equal(t, 1, saves)
				assert.Equal(t, "succeeded", job.Status)
				assert.NotEmpty(t, job.RequestCipher)
				// An upstream result avoids requiring a local artifact for this
				// presentation fixture, while exercising the successful-task view.
				require.NoError(t, db.Model(&job).Update("storage_status", "upstream").Error)
				for _, item := range []struct {
					name   string
					userID int
					admin  bool
					want   int
				}{{"owner", 101, false, http.StatusOK}, {"administrator", 102, true, http.StatusOK}, {"other user", 102, false, http.StatusNotFound}} {
					t.Run(item.name, func(t *testing.T) {
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Set("id", item.userID)
						c.Params = gin.Params{{Key: "task_id", Value: job.TaskId}}
						c.Request = httptest.NewRequest(http.MethodGet, "/api/media-tasks/"+job.TaskId, nil)
						GetMediaTask(c, item.admin)
						require.Equal(t, item.want, recorder.Code, recorder.Body.String())
						assert.Contains(t, recorder.Header().Get("Cache-Control"), "no-store")
						if item.want != http.StatusOK {
							assert.NotContains(t, recorder.Body.String(), "owner-signature")
							assert.NotContains(t, recorder.Body.String(), "A cinematic landscape")
							return
						}
						var response struct {
							Data struct {
								Task service.AsyncVideoRequestDetails
							}
						}
						require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
						require.NotNil(t, response.Data.Task.RequestedSeconds)
						assert.Equal(t, float64(8), *response.Data.Task.RequestedSeconds)
						assert.ElementsMatch(t, []service.AsyncVideoReferenceMedia{{Type: "image", URL: "https://cdn.example.com/input.png?signature=owner-signature"}, {Type: "video", URL: "https://cdn.example.com/reference.mp4"}, {Type: "audio", URL: "https://cdn.example.com/sound.mp3"}}, response.Data.Task.ReferenceMedia)
						assert.Equal(t, false, response.Data.Task.RequestParameters["watermark"])
						assert.Equal(t, float64(0), response.Data.Task.RequestParameters["seed"])
					})
				}
				for _, list := range []bool{false, true} {
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Set("id", 101)
					c.Set("token_id", 201)
					c.Params = gin.Params{{Key: "task_id", Value: job.TaskId}}
					c.Request = httptest.NewRequest(http.MethodGet, "/?task_id="+job.TaskId, nil)
					if list {
						ListMediaTasks(c, false)
					} else {
						QueryAsyncMedia(c)
					}
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					for _, private := range []string{"request_parameters", "reference_media", "requested_seconds", "owner-signature", "A cinematic landscape"} {
						assert.NotContains(t, recorder.Body.String(), private)
					}
				}
				require.NoError(t, db.Model(&job).Update("expires_at", now-1).Error)
				require.NoError(t, db.Where("id = ?", job.ID).Take(&job).Error)
				require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
				require.NoError(t, db.Where("id = ?", job.ID).Take(&job).Error)
				assert.Empty(t, job.RequestCipher, "existing expiration cleanup removes request details")
				for _, expiresAt := range []int64{now - 1, now + 3600} {
					require.NoError(t, db.Model(&job).Update("expires_at", expiresAt).Error)
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Set("id", 101)
					c.Params = gin.Params{{Key: "task_id", Value: job.TaskId}}
					c.Request = httptest.NewRequest(http.MethodGet, "/api/media-tasks/"+job.TaskId, nil)
					GetMediaTask(c, false)
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					for _, field := range []string{"request_parameters", "reference_media", "requested_seconds"} {
						assert.NotContains(t, recorder.Body.String(), field, "expired and legacy tasks must not fabricate request parameters")
					}
				}
			})

			t.Run("video failures share readable details across queries", func(t *testing.T) {
				const rawReason = "The request failed because the output video may contain sensitive information."
				for _, item := range []struct {
					name, raw, status, jobStatus, billing, summary, suggestion, detail string
					withTask                                                           bool
				}{
					{name: "history", raw: rawReason, summary: "敏感", detail: rawReason, withTask: true},
					{name: "no-task", raw: rawReason, summary: "敏感", detail: rawReason},
					{name: "unknown-502", raw: "Upstream submission could not be confirmed; the video operation will not be repeated\nupstream video submission failed (HTTP 502, code task_submission_unknown): Task submission state is unknown.", status: "execution_unknown", billing: "unknown", summary: "提交结果尚未确认", suggestion: "不会自动再次提交", detail: "HTTP 502, code task_submission_unknown"},
					{name: "unknown-no-receipt", raw: "Upstream submission could not be confirmed; the video operation will not be repeated", status: "execution_unknown", billing: "unknown", summary: "无法判断上游是否已受理", suggestion: "核对上游任务状态和费用", detail: "Upstream submission could not be confirmed"},
					{name: "unknown-empty-reason", status: "execution_unknown", billing: "unknown", summary: "提交结果尚未确认", suggestion: "不会自动再次提交", detail: "execution_unknown"},
					{name: "administrator-ended", raw: "Terminated by administrator after upstream verification", billing: "unknown", summary: "管理员已结束本地任务处理", suggestion: "不代表上游任务已取消或费用已退回", detail: "Terminated by administrator"},
					{name: "billing-confirmation", raw: "Task billing confirmation requires reconciliation", status: "invoking", jobStatus: "submitted", billing: "failed", summary: "视频任务费用确认尚未完成", suggestion: "此状态不代表费用已退回", detail: "Task billing confirmation requires reconciliation"},
					{name: "video-copyright", raw: "The request failed because the output video may be related to copyright restrictions.", summary: "生成的视频可能涉及版权限制", suggestion: "获准使用的素材", detail: "output video"},
					{name: "duration", raw: "Video duration must be between 2s and 15 seconds.", summary: "2–15 秒", suggestion: "2–15 秒", detail: "between 2s and 15 seconds"},
					{name: "missing-result-url", raw: "上游成功响应缺少有效成片地址", summary: "没有提供可用的成片地址", suggestion: "不要重复发起视频生成", detail: "缺少有效成片地址"},
					{name: "upstream-busy", raw: "服务器繁忙，请稍后再试", summary: "上游服务当前繁忙", suggestion: "稍后手动重试", detail: "服务器繁忙"},
					{name: "generic-422", raw: `{"error":{"code":"upstream_error","message":"生成失败，检查上传素材是否公网可达，是否输入了被允许的参数，是否侵犯了版权保护。"},"data":{"upstream_status":422,"upstream_body":{"detail":"生成失败，检查上传素材是否公网可达，是否输入了被允许的参数，是否侵犯了版权保护。","error_code":"generation_failed"}}}`, summary: "可能原因，未说明具体触发项", suggestion: "公网可达", detail: "是否侵犯了版权保护"},
				} {
					t.Run(item.name, func(t *testing.T) {
						taskID, storedReason := "video-readable-"+item.name, item.raw
						if item.withTask {
							storedReason = "Video generation failed"
							task := model.Task{TaskID: taskID, Platform: "drama", UserId: 101, Status: model.TaskStatusFailure, FailReason: storedReason, PrivateData: model.TaskPrivateData{AsyncMedia: true}, Data: []byte(`{"status":"failed","metadata":{"url":"` + item.raw + `"}}`)}
							require.NoError(t, db.Create(&task).Error)
						}
						if item.status == "" {
							item.status = "failed"
						}
						if item.billing == "" {
							item.billing = "refunded"
						}
						if item.jobStatus == "" {
							item.jobStatus = item.status
						}
						job := model.AsyncMediaJob{TaskId: taskID, IdentityHash: taskID, UserId: 101, TokenId: 201, ChannelId: 301, Provider: "drama", Status: item.jobStatus, StorageStatus: "pending", BillingStatus: item.billing, ErrorMessage: storedReason, CreatedAt: now, ExpiresAt: now + 3600}
						require.NoError(t, db.Create(&job).Error)
						var publicReason string
						for _, endpoint := range []string{"public", "owner", "admin", "owner-list", "admin-list"} {
							recorder := httptest.NewRecorder()
							c, _ := gin.CreateTestContext(recorder)
							c.Set("id", job.UserId)
							c.Set("token_id", job.TokenId)
							c.Params = gin.Params{{Key: "task_id", Value: taskID}}
							c.Request = httptest.NewRequest(http.MethodGet, "/v1/media/tasks_async/"+taskID+"?task_id="+taskID, nil)
							switch endpoint {
							case "public":
								QueryAsyncMedia(c)
							case "owner-list", "admin-list":
								ListMediaTasks(c, endpoint == "admin-list")
							default:
								GetMediaTask(c, endpoint == "admin")
							}
							require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
							var body map[string]any
							require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
							view := body
							if strings.HasSuffix(endpoint, "-list") {
								items := body["data"].(map[string]any)["items"].([]any)
								require.Len(t, items, 1)
								view = items[0].(map[string]any)
							} else if endpoint != "public" {
								view = body["data"].(map[string]any)["task"].(map[string]any)
							}
							expectedStatus := item.status
							if item.status == "execution_unknown" {
								expectedStatus = "failed"
								if endpoint == "public" {
									assert.Equal(t, "execution_unknown", view["error_code"])
									assert.Equal(t, false, view["error_retryable"])
								}
							}
							assert.Equal(t, expectedStatus, view["status"], endpoint)
							assert.Equal(t, item.billing, view["billing_status"], endpoint)
							assert.Contains(t, view["error_summary"], item.summary, endpoint)
							assert.Contains(t, view["error_suggestion"], item.suggestion, endpoint)
							assert.NotEmpty(t, view["error_suggestion"], endpoint)
							assert.Contains(t, view["error_detail"], item.detail, endpoint)
							assert.Contains(t, view["error_message"], item.detail, endpoint)
							if item.name == "generic-422" {
								assert.NotContains(t, view["error_summary"], "版权限制", "the upstream offered possibilities, not a confirmed copyright finding")
							}
							if endpoint == "public" {
								publicReason = view["error_message"].(string)
							} else {
								assert.Equal(t, publicReason, view["error_message"], endpoint)
							}
							assert.Equal(t, publicReason, service.VideoTaskFailureReason(publicReason), "repeated query projection must be idempotent")
						}
						var persisted model.AsyncMediaJob
						require.NoError(t, db.Where("task_id = ?", taskID).Take(&persisted).Error)
						assert.Equal(t, job, persisted, "queries must leave historical diagnostics and accounting unchanged")
					})
				}
			})

			t.Run("video submission refunds preserve accounting and terminal state", func(t *testing.T) {
				t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "test")
				t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
				previousSubmit, previousRedis := service.SubmitAsyncVideoFunc, common.RedisEnabled
				common.RedisEnabled = false
				t.Cleanup(func() {
					service.SubmitAsyncVideoFunc, common.RedisEnabled = previousSubmit, previousRedis
				})
				const initialQuota, reservedQuota = 1000, 120
				for index, item := range []struct {
					name, failedTable, billing                string
					reserve, recovery, lateRefund             bool
					logFailure, ackFailure, disableConsumeLog bool
					wallet, token, quota, logs                int
				}{
					{name: "refunded", reserve: true, disableConsumeLog: true, billing: "refunded", wallet: initialQuota, token: initialQuota, logs: 1},
					{name: "funding failure", reserve: true, failedTable: "users", billing: "failed", wallet: initialQuota - reservedQuota, token: initialQuota, quota: reservedQuota},
					{name: "token failure", reserve: true, failedTable: "tokens", billing: "failed", wallet: initialQuota, token: initialQuota - reservedQuota, quota: reservedQuota},
					{name: "not reserved", billing: "not_billable", wallet: initialQuota, token: initialQuota},
					{name: "reservation recovered after interruption", reserve: true, recovery: true, billing: "unknown", wallet: initialQuota - reservedQuota, token: initialQuota - reservedQuota, quota: reservedQuota},
					{name: "late refund survives stale recovery update", reserve: true, recovery: true, lateRefund: true, billing: "refunded", wallet: initialQuota, token: initialQuota, logs: 1},
					{name: "refund log database unavailable", reserve: true, logFailure: true, billing: "refunded", wallet: initialQuota, token: initialQuota, logs: 1},
					{name: "refund log written before acknowledgement failure", reserve: true, ackFailure: true, billing: "refunded", wallet: initialQuota, token: initialQuota, logs: 1},
				} {
					t.Run(item.name, func(t *testing.T) {
						if item.disableConsumeLog {
							previousConsumeLog := common.LogConsumeEnabled
							common.LogConsumeEnabled = false
							t.Cleanup(func() { common.LogConsumeEnabled = previousConsumeLog })
						}
						user := model.User{Id: 1101 + index, Username: "video-refund-" + strings.ReplaceAll(item.name, " ", "-"), AffCode: "video-refund-" + string(rune('a'+index)), Quota: initialQuota, UsedQuota: 37}
						token := model.Token{Id: 1201 + index, UserId: user.Id, Name: "Refund key", Key: "video-refund-key-" + string(rune('a'+index)), RemainQuota: initialQuota, UsedQuota: 23}
						require.NoError(t, db.Create(&user).Error)
						require.NoError(t, db.Create(&token).Error)
						job := model.AsyncMediaJob{TaskId: "video-refund-" + string(rune('a'+index)), IdentityHash: "video-refund-" + string(rune('a'+index)), UserId: user.Id, TokenId: token.Id, ChannelId: 301, Provider: "drama", Model: "video-refund-model", Group: "default", Status: "queued", StorageStatus: "pending", BillingStatus: "pending", CreatedAt: now, ExpiresAt: now + 3600, LeaseToken: "refund-lease", LeaseExpiresAt: now + 120}
						var err error
						job.RequestCipher, err = service.EncryptImagePayload([]byte(`{}`), "media-request:"+job.TaskId)
						require.NoError(t, err)
						require.NoError(t, db.Create(&job).Error)
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
						c.Set("async_media_execution", true)
						info := &relaycommon.RelayInfo{
							UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, OriginModelName: job.Model, UsingGroup: job.Group,
							UserSetting:   dto.UserSetting{BillingPreference: "wallet_only"},
							ChannelMeta:   &relaycommon.ChannelMeta{ChannelId: job.ChannelId},
							TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: job.TaskId},
						}
						if item.recovery {
							require.Nil(t, service.PreConsumeDurableTaskBilling(c, reservedQuota, info))
							require.NoError(t, db.Model(&job).Updates(map[string]any{"status": "submitting", "billing_status": "reserved", "quota": reservedQuota, "dispatched_at": now}).Error)
							require.NoError(t, db.Take(&job, job.ID).Error)
						}
						if item.lateRefund {
							refundPending := true
							require.NoError(t, db.Callback().Update().Before("gorm:begin_transaction").Register("video-refund-before-recovery", func(tx *gorm.DB) {
								updates, ok := tx.Statement.Dest.(map[string]any)
								if refundPending && ok && tx.Statement.Table == "async_media_jobs" && updates["status"] == "failed" {
									refundPending = false
									info.Billing.Refund(c)
								}
							}))
							t.Cleanup(func() { assert.NoError(t, db.Callback().Update().Remove("video-refund-before-recovery")) })
						}
						var submissions int
						submitErr := errors.New("video submission failed (HTTP 429, code fail_to_fetch_task): upstream capacity exhausted")
						service.SubmitAsyncVideoFunc = func(ctx context.Context, current model.AsyncMediaJob, _ service.AsyncVideoRequest) error {
							submissions++
							require.False(t, item.recovery, "lease recovery must not repeat an upstream submission")
							if !item.reserve {
								return submitErr
							}
							require.Nil(t, service.PreConsumeDurableTaskBilling(c, reservedQuota, info))
							require.NoError(t, model.UpdateAsyncMediaJob(ctx, current, map[string]any{"billing_status": "reserved", "quota": reservedQuota, "dispatched_at": now}))
							var reservedUser model.User
							var reservedToken model.Token
							require.NoError(t, db.Take(&reservedUser, user.Id).Error)
							require.NoError(t, db.Take(&reservedToken, token.Id).Error)
							assert.Equal(t, initialQuota-reservedQuota, reservedUser.Quota)
							assert.Equal(t, initialQuota-reservedQuota, reservedToken.RemainQuota)
							if item.failedTable != "" {
								require.NoError(t, db.Callback().Update().Before("gorm:update").Register("video-refund-failure", func(tx *gorm.DB) {
									if tx.Statement.Table == item.failedTable {
										tx.AddError(errors.New("injected refund write failure"))
									}
								}))
								t.Cleanup(func() { assert.NoError(t, db.Callback().Update().Remove("video-refund-failure")) })
							}
							if item.logFailure {
								require.NoError(t, logDB.Callback().Create().Before("gorm:create").Register("video-refund-log-failure", func(tx *gorm.DB) {
									if tx.Statement.Table == "logs" {
										tx.AddError(errors.New("injected refund log database failure"))
									}
								}))
								t.Cleanup(func() { assert.NoError(t, logDB.Callback().Create().Remove("video-refund-log-failure")) })
							}
							if item.ackFailure {
								require.NoError(t, db.Callback().Update().Before("gorm:update").Register("video-refund-log-ack-failure", func(tx *gorm.DB) {
									updates, ok := tx.Statement.Dest.(map[string]any)
									if ok && tx.Statement.Table == "async_media_jobs" && updates["refund_log_payload"] == "" {
										tx.AddError(errors.New("injected refund log acknowledgement failure"))
									}
								}))
								t.Cleanup(func() { assert.NoError(t, db.Callback().Update().Remove("video-refund-log-ack-failure")) })
							}
							// Refund must survive cancellation of the upstream submission context.
							cancelled, cancel := context.WithCancel(c.Request.Context())
							cancel()
							c.Request = c.Request.WithContext(cancelled)
							info.Billing.Refund(c)
							if item.failedTable != "" {
								require.NoError(t, db.Callback().Update().Remove("video-refund-failure"))
							}
							info.Billing.Refund(c)
							return submitErr
						}
						processErr := service.ProcessAsyncMediaJob(t.Context(), job, service.DefaultMediaRuntimeConfig())
						if item.recovery {
							require.NoError(t, processErr)
						} else {
							require.ErrorIs(t, processErr, submitErr)
						}
						require.NoError(t, db.Take(&job, job.ID).Error)
						assert.Equal(t, "failed", job.Status)
						assert.Equal(t, item.billing, job.BillingStatus)
						assert.Equal(t, item.quota, job.Quota)
						assert.Empty(t, job.LeaseToken)
						assert.Zero(t, job.NextAttemptAt)
						if item.logFailure || item.ackFailure {
							assert.NotEmpty(t, job.RefundLogPayload, "refund log stays pending until its persistence can be acknowledged")
							var count int64
							require.NoError(t, logDB.Model(&model.Log{}).Where("token_id = ?", token.Id).Count(&count).Error)
							if item.ackFailure {
								assert.EqualValues(t, 1, count, "the log was committed before the acknowledgement failed")
							} else {
								assert.Zero(t, count)
							}
							require.NoError(t, db.Take(&user, user.Id).Error)
							require.NoError(t, db.Take(&token, token.Id).Error)
							assert.Equal(t, initialQuota, user.Quota, "log failure must not undo the completed wallet refund")
							assert.Equal(t, initialQuota, token.RemainQuota)
							if item.logFailure {
								require.NoError(t, logDB.Callback().Create().Remove("video-refund-log-failure"))
							} else {
								require.NoError(t, db.Callback().Update().Remove("video-refund-log-ack-failure"))
							}
							for range 2 {
								require.NoError(t, model.ReplayAsyncMediaRefundLogs(t.Context(), 10))
							}
							require.NoError(t, db.Take(&job, job.ID).Error)
							assert.Empty(t, job.RefundLogPayload)
						}
						require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, service.DefaultMediaRuntimeConfig()))
						require.NoError(t, db.Take(&job, job.ID).Error)
						assert.Equal(t, "failed", job.Status)
						assert.Equal(t, item.billing, job.BillingStatus, "worker recovery must preserve the confirmed refund result")
						assert.Equal(t, item.quota, job.Quota)
						expectedSubmissions := 1
						if item.recovery {
							expectedSubmissions = 0
						}
						assert.Equal(t, expectedSubmissions, submissions)
						require.NoError(t, db.Take(&user, user.Id).Error)
						require.NoError(t, db.Take(&token, token.Id).Error)
						assert.Equal(t, item.wallet, user.Quota)
						assert.Equal(t, item.token, token.RemainQuota)
						assert.Equal(t, 23+initialQuota-item.token, token.UsedQuota)
						assert.Equal(t, 37, user.UsedQuota, "submission rollback must not reverse consumption that was never recorded")
						var logs []model.Log
						require.NoError(t, logDB.Where("token_id = ?", token.Id).Find(&logs).Error)
						require.Len(t, logs, item.logs, "only completed refunds produce a usage-log entry; failed submission needs no consume log")
						if item.logs > 0 {
							assert.Equal(t, model.LogTypeRefund, logs[0].Type)
							assert.Equal(t, reservedQuota, logs[0].Quota)
							assert.Equal(t, job.Model, logs[0].ModelName)
							assert.Equal(t, job.Group, logs[0].Group)
							assert.Equal(t, job.ChannelId, logs[0].ChannelId)
							var other map[string]any
							require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
							assert.Equal(t, true, other["is_task"])
							assert.Equal(t, job.TaskId, other["task_id"])
							assert.Equal(t, service.BillingSourceWallet, other["billing_source"])
							assert.NotEmpty(t, other["reason"])
						}
					})
				}
			})
			t.Run("video submission errors retain diagnostics without replay", func(t *testing.T) {
				t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "test")
				t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
				previousSubmit := service.SubmitAsyncVideoFunc
				t.Cleanup(func() { service.SubmitAsyncVideoFunc = previousSubmit })
				for _, dispatched := range []bool{true, false} {
					name, status, billing, summary := "before-dispatch", "failed", "not_billable", "视频生成参数不符合上游要求"
					diagnostic := "Invalid parameter: duration; Authorization: Basic cHJpdmF0ZS1jcmVkZW50aWFs; api_key=private-api-canary"
					if dispatched {
						name, status, billing, summary = "after-dispatch", "failed", "unknown", "视频提交结果尚未确认"
						diagnostic = `video submission failed (HTTP 502, code fail_to_fetch_task): {"code":"task_submission_unknown","message":"task submission state is unknown","data":null,"request":{"prompt":"private-request-canary","api_key":"private-api-canary"}}`
					}
					t.Run(name, func(t *testing.T) {
						request := service.AsyncVideoRequest{Body: []byte(`{"model":"video-model","prompt":"private-request-canary"}`), ContentType: "application/json"}
						encoded, err := common.Marshal(request)
						require.NoError(t, err)
						job := model.AsyncMediaJob{TaskId: "video-submit-" + name, IdentityHash: "video-submit-" + name, UserId: 101, TokenId: 201, ChannelId: 301, Provider: "drama", Status: "queued", StorageStatus: "pending", BillingStatus: "pending", CreatedAt: now, ExpiresAt: now + 3600, LeaseToken: "video-submit-lease", LeaseExpiresAt: now + 120}
						job.RequestCipher, err = service.EncryptImagePayload(encoded, "media-request:"+job.TaskId)
						require.NoError(t, err)
						require.NoError(t, db.Create(&job).Error)
						originalCipher := bytes.Clone(job.RequestCipher)
						submissions := 0
						submitErr := errors.New(diagnostic)
						service.SubmitAsyncVideoFunc = func(ctx context.Context, current model.AsyncMediaJob, submitted service.AsyncVideoRequest) error {
							submissions++
							assert.Equal(t, request.Body, submitted.Body)
							if dispatched {
								require.NoError(t, model.UpdateAsyncMediaJob(ctx, current, map[string]any{"dispatched_at": now, "billing_status": "reserved", "quota": 120}))
							}
							return submitErr
						}
						require.ErrorIs(t, service.ProcessAsyncMediaJob(t.Context(), job, service.DefaultMediaRuntimeConfig()), submitErr)
						require.NoError(t, db.Where("id = ?", job.ID).Take(&job).Error)
						assert.Equal(t, status, job.Status)
						assert.Equal(t, billing, job.BillingStatus)
						assert.Zero(t, job.NextAttemptAt)
						assert.Empty(t, job.LeaseToken)
						assert.Zero(t, job.LeaseExpiresAt)
						assert.Equal(t, originalCipher, job.RequestCipher)
						assert.Contains(t, job.ErrorMessage, "处理建议：")
						if dispatched {
							assert.Equal(t, now, job.DispatchedAt)
							assert.Contains(t, job.ErrorMessage, "HTTP 502, code fail_to_fetch_task")
							assert.Contains(t, job.ErrorMessage, "task_submission_unknown")
							originalReason := job.ErrorMessage
							require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, service.DefaultMediaRuntimeConfig()))
							require.NoError(t, db.Where("id = ?", job.ID).Take(&job).Error)
							assert.Equal(t, originalReason, job.ErrorMessage, "lease recovery must retain the original sanitized submission diagnostic")
							assert.Equal(t, "failed", job.Status)
							assert.Equal(t, "unknown", job.BillingStatus)
							assert.Zero(t, job.NextAttemptAt)
							assert.Equal(t, originalCipher, job.RequestCipher)
						}
						assert.Equal(t, 1, submissions, "unknown dispatch must not submit the video again")
						for _, private := range []string{"cHJpdmF0ZS1jcmVkZW50aWFs", "private-api-canary", "private-request-canary"} {
							assert.NotContains(t, job.ErrorMessage, private)
						}
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Set("id", job.UserId)
						c.Set("token_id", job.TokenId)
						c.Params = gin.Params{{Key: "task_id", Value: job.TaskId}}
						c.Request = httptest.NewRequest(http.MethodGet, "/v1/media/tasks_async/"+job.TaskId, nil)
						QueryAsyncMedia(c)
						require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
						var view map[string]any
						require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &view))
						assert.Equal(t, status, view["status"])
						assert.Equal(t, billing, view["billing_status"])
						assert.Contains(t, view["error_summary"], summary)
						assert.NotEmpty(t, view["error_suggestion"])
						if dispatched {
							assert.Contains(t, view["error_detail"], "HTTP 502, code fail_to_fetch_task")
							assert.Contains(t, view["error_detail"], "task_submission_unknown")
						}
						for _, private := range []string{"cHJpdmF0ZS1jcmVkZW50aWFs", "private-api-canary", "private-request-canary"} {
							assert.NotContains(t, recorder.Body.String(), private)
						}
					})
				}
			})

			t.Run("unknown video execution requires administrator reconciliation", func(t *testing.T) {
				const submissionDiagnostic = "video submission failed (HTTP 502, code fail_to_fetch_task): code task_submission_unknown: task submission state is unknown"
				for _, item := range []struct {
					name       string
					status     string
					admin      bool
					body       string
					leased     bool
					expired    bool
					accepted   bool
					otherUser  bool
					want       int
					terminable bool
				}{
					{name: "confirmed administrator", status: "execution_unknown", admin: true, body: `{"confirm_upstream_checked":true}`, want: http.StatusOK, terminable: true},
					{name: "missing confirmation", status: "execution_unknown", admin: true, body: `{}`, want: http.StatusConflict, terminable: true},
					{name: "invalid confirmation", status: "execution_unknown", admin: true, body: `{"confirm_upstream_checked":`, want: http.StatusConflict, terminable: true},
					{name: "owner cannot reconcile", status: "execution_unknown", body: `{"confirm_upstream_checked":true}`, want: http.StatusForbidden},
					{name: "active lease", status: "execution_unknown", admin: true, body: `{"confirm_upstream_checked":true}`, leased: true, want: http.StatusConflict},
					{name: "expired job", status: "execution_unknown", admin: true, body: `{"confirm_upstream_checked":true}`, expired: true, want: http.StatusConflict},
					{name: "accepted upstream task", status: "execution_unknown", admin: true, body: `{"confirm_upstream_checked":true}`, accepted: true, want: http.StatusConflict},
					{name: "other user", status: "queued", otherUser: true, want: http.StatusConflict, terminable: true},
					{name: "queued owner", status: "queued", want: http.StatusOK, terminable: true},
					{name: "submitted job", status: "submitted", admin: true, body: `{"confirm_upstream_checked":true}`, want: http.StatusConflict},
					{name: "failed job", status: "failed", admin: true, body: `{"confirm_upstream_checked":true}`, want: http.StatusConflict},
				} {
					t.Run(item.name, func(t *testing.T) {
						job := model.AsyncMediaJob{TaskId: "video-terminate-" + strings.ReplaceAll(item.name, " ", "-"), IdentityHash: "terminate-" + item.name, UserId: 101, TokenId: 201, Status: item.status, StorageStatus: "pending", BillingStatus: "unknown", Quota: 500, RequestCipher: []byte("encrypted-video-request"), CreatedAt: now, ExpiresAt: now + 3600, LeaseToken: "expired-video-lease", NextAttemptAt: now + 30}
						if item.status == "execution_unknown" {
							job.ErrorMessage = service.VideoTaskFailureReason("Upstream submission could not be confirmed; the video operation will not be repeated\n" + submissionDiagnostic)
						}
						if item.status != "queued" {
							job.DispatchedAt = now - 30
						}
						if item.leased {
							job.LeaseExpiresAt = now + 120
						}
						if item.expired {
							job.ExpiresAt = now - 1
						}
						require.NoError(t, db.Create(&job).Error)
						if item.accepted {
							require.NoError(t, db.Create(&model.Task{TaskID: job.TaskId, UserId: job.UserId, Status: model.TaskStatusSubmitted, Quota: job.Quota}).Error)
						}
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Set("id", job.UserId)
						if item.otherUser {
							c.Set("id", 102)
						}
						c.Params = gin.Params{{Key: "task_id", Value: job.TaskId}}
						c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/media-tasks/"+job.TaskId+"/terminate", strings.NewReader(item.body))
						view, _, err := videoJobView(c, job, item.admin)
						require.NoError(t, err)
						assert.Equal(t, item.terminable, view["can_terminate"])
						ManageMediaTask(c, item.admin, "terminate")
						require.Equal(t, item.want, recorder.Code, recorder.Body.String())
						var stored model.AsyncMediaJob
						require.NoError(t, db.Where("id = ?", job.ID).Take(&stored).Error)
						if item.want != http.StatusOK {
							assert.Equal(t, job, stored, "rejected termination must leave the job unchanged")
							return
						}
						assert.Equal(t, "failed", stored.Status)
						assert.Empty(t, stored.RequestCipher)
						if item.status == "execution_unknown" {
							assert.Equal(t, job.BillingStatus, stored.BillingStatus)
							assert.Equal(t, job.Quota, stored.Quota)
							assert.Contains(t, stored.ErrorMessage, "Terminated by administrator")
							assert.Contains(t, stored.ErrorMessage, submissionDiagnostic, "ending local processing must retain the original upstream submission diagnostic")
							assert.Equal(t, 1, strings.Count(stored.ErrorMessage, "处理建议："))
							assert.Equal(t, 1, strings.Count(stored.ErrorMessage, "原始错误："))
							assert.Equal(t, stored.ErrorMessage, service.VideoTaskFailureReason(stored.ErrorMessage))
							assert.Zero(t, stored.NextAttemptAt)
							assert.ErrorIs(t, model.UpdateAsyncMediaJob(t.Context(), job, map[string]any{"status": "submitted"}), model.ErrImageConflict, "an expired worker must not revive the terminated task")
						} else {
							assert.Equal(t, "not_billable", stored.BillingStatus)
						}
						view, _, err = videoJobView(c, stored, item.admin)
						require.NoError(t, err)
						assert.Equal(t, "failed", view["status"])
						assert.Equal(t, false, view["can_terminate"])
						if item.status == "execution_unknown" {
							assert.Contains(t, view["error_summary"], "管理员已结束本地任务处理")
							assert.Contains(t, view["error_suggestion"], "不代表上游任务已取消或费用已退回")
							assert.Equal(t, "unknown", view["billing_status"])
							assert.Equal(t, job.Quota, view["quota"])
							assert.Contains(t, view["error_detail"], submissionDiagnostic)
							assert.Equal(t, stored.ErrorMessage, view["error_message"], "query projection must not add another explanation layer")
						}
						repeated := httptest.NewRecorder()
						repeatContext, _ := gin.CreateTestContext(repeated)
						repeatContext.Set("id", job.UserId)
						repeatContext.Params = c.Params
						repeatContext.Request = httptest.NewRequest(http.MethodPost, c.Request.URL.String(), strings.NewReader(item.body))
						ManageMediaTask(repeatContext, item.admin, "terminate")
						assert.Equal(t, http.StatusConflict, repeated.Code, "termination must not be repeated")
					})
				}
			})
			t.Run("unknown execution requires administrator reconciliation", func(t *testing.T) {
				unknown := model.AsyncImageTask{TaskId: "asyncimg_unknown_reconcile", UserId: 101, TokenId: 201, Status: model.ImageTaskExecutionUnknown, Version: 1, CreatedAt: now, ExpiresAt: now + 3600}
				require.NoError(t, db.Create(&unknown).Error)
				assert.Equal(t, false, imageTaskDTO(unknown, false)["can_terminate"])
				assert.Equal(t, true, imageTaskDTO(unknown, true)["can_terminate"])
				for _, item := range []struct {
					name  string
					admin bool
					body  string
					want  int
				}{
					{"owner cannot release unknown work", false, `{"confirm_upstream_checked":true}`, http.StatusForbidden},
					{"administrator must confirm upstream check", true, `{}`, http.StatusConflict},
					{"administrator releases after confirmation", true, `{"confirm_upstream_checked":true}`, http.StatusOK},
				} {
					t.Run(item.name, func(t *testing.T) {
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Set("id", 101)
						c.Params = gin.Params{{Key: "task_id", Value: unknown.TaskId}}
						c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/media-tasks/"+unknown.TaskId+"/terminate", strings.NewReader(item.body))
						ManageMediaTask(c, item.admin, "terminate")
						assert.Equal(t, item.want, recorder.Code, recorder.Body.String())
					})
				}
				require.NoError(t, db.Where("task_id = ?", unknown.TaskId).Take(&unknown).Error)
				assert.Equal(t, model.ImageTaskFailed, unknown.Status)
			})
		})
	}
}

func TestImageTaskFailureNoticeContract(t *testing.T) {
	for _, tc := range []struct {
		name, summary, suggestion, stage string
		task                             model.AsyncImageTask
		action, rule                     string
		retryable                        bool
	}{
		{name: "material_limit", task: model.AsyncImageTask{Status: model.ImageTaskFailed, PublicErrorCode: 604, ErrorCode: "upstream_job_failed", ErrorMessage: "参考图过大（25.4MB，上限 20MB）", DispatchedAt: 1}, summary: "参考图超过上游 20MB 限制", suggestion: "压缩每张参考图", stage: "submit"},
		{name: "review_is_neutral", task: model.AsyncImageTask{Status: model.ImageTaskFailed, PublicErrorCode: 601, ErrorCode: "upstream_job_failed", ErrorMessage: "guardrails detection failed", DispatchedAt: 1}, summary: "上游安全审核未通过", suggestion: "若审核失败不符合预期", stage: "submit"},
		{name: "generic_argument_not_user_fault", task: model.AsyncImageTask{Status: model.ImageTaskFailed, PublicErrorCode: 610, ErrorCode: "upstream_failed", ErrorMessage: "Request contains an invalid argument.", DispatchedAt: 1}, summary: "上游拒绝了请求，具体原因尚未确认", suggestion: "服务提供方", stage: "submit"},
		{name: "busy_recovery_committed", task: model.AsyncImageTask{Status: model.ImageTaskQueued, PublicErrorCode: 606, ErrorCode: "upstream_job_failed", ErrorMessage: "服务繁忙,请稍后重试", NextAttemptAt: 123}, action: "retry_submission", summary: "上游图片服务暂时异常", suggestion: "重试限制内再次提交", stage: "submit", retryable: true},
		{name: "busy_no_recovery_committed", task: model.AsyncImageTask{Status: model.ImageTaskFailed, PublicErrorCode: 606, ErrorCode: "upstream_job_failed", ErrorMessage: "服务繁忙,请稍后重试"}, summary: "上游图片服务暂时异常", suggestion: "服务提供方", stage: "routing"},
		{name: "accepted_job_poll_only", task: model.AsyncImageTask{Status: model.ImageTaskQueued, UpstreamTaskId: "accepted", PublicErrorCode: 608, ErrorCode: "upstream_response_invalid", ErrorMessage: "poll response unavailable", NextAttemptAt: 123}, action: "poll_same_job", summary: "上游响应读取或解析失败", suggestion: "不会提交新的生成请求", stage: "poll", retryable: true},
		{name: "accepted_job_cannot_claim_submission", task: model.AsyncImageTask{Status: model.ImageTaskQueued, UpstreamTaskId: "accepted", PublicErrorCode: 606, ErrorCode: "upstream_failed", ErrorMessage: "transient", NextAttemptAt: 123}, action: "retry_submission", summary: "上游图片服务暂时异常", suggestion: "服务提供方", stage: "submit"},
		{name: "output_processing_only", task: model.AsyncImageTask{Status: model.ImageTaskUploading, UpstreamSucceededAt: 1, PublicErrorCode: 607, ErrorCode: "upload_failed", ErrorMessage: "storage unavailable", NextAttemptAt: 123}, action: "resume_output", summary: "图片后处理需要检查", suggestion: "不会重复生成", stage: "postprocess", retryable: true},
		{name: "unknown_never_resubmits", task: model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, PublicErrorCode: 608, ErrorCode: "upstream_transport_timeout", ErrorMessage: "timeout", DispatchedAt: 1, NextAttemptAt: 123}, action: "retry_submission", summary: "上游请求超时", suggestion: "请勿再次提交", stage: "submit"},
		{name: "admin_ended_unknown_stays_conservative", task: model.AsyncImageTask{Status: model.ImageTaskFailed, PublicErrorCode: 608, ErrorCode: "admin_terminated", ErrorMessage: "ended", DispatchedAt: 1}, action: "poll_same_job", rule: "recovered", summary: "任务已被手动终止", stage: "poll"},
		{name: "local_quota_cache_wait", task: model.AsyncImageTask{Status: model.ImageTaskQueued, PublicErrorCode: 606, ErrorCode: "quota_cache_unavailable", ErrorMessage: "quota cache unavailable", NextAttemptAt: 123}, action: "wait_local_dependency", summary: "图片额度校验暂时不可用", suggestion: "等待本地依赖恢复", stage: "prepare", retryable: true},
		{name: "quota_unknown_is_local", task: model.AsyncImageTask{Status: model.ImageTaskExecutionUnknown, PublicErrorCode: 608, ErrorCode: "quota_cache_uncertain", ErrorMessage: "quota cache uncertain"}, summary: "无法确认图片额度预留结果", suggestion: "核对额度预留", stage: "routing"},
		{name: "local_capacity_wait", task: model.AsyncImageTask{Status: model.ImageTaskQueued, PublicErrorCode: 603, ErrorCode: "local_capacity_busy", ErrorMessage: "busy", NextAttemptAt: 123}, action: "wait_capacity", summary: "图片处理容量暂时不可用", suggestion: "等待可用容量", stage: "routing", retryable: true},
		{name: "expired_before_dispatch", task: model.AsyncImageTask{Status: model.ImageTaskExpired, PublicErrorCode: 608, ErrorCode: "execution_deadline", ErrorMessage: "deadline"}, summary: "图片执行期限已到", suggestion: "派发上游前已过期", stage: "routing"},
		{name: "healthy_poll_hides_old_error", task: model.AsyncImageTask{Status: model.ImageTaskQueued, UpstreamTaskId: "accepted", PublicErrorCode: 606, ErrorCode: "upstream_failed", ErrorMessage: "old poll failure", NextAttemptAt: 123}, action: "poll_same_job", rule: "upstream_pending", stage: "poll"},
		{name: "succeeded_hides_old_error", task: model.AsyncImageTask{Status: model.ImageTaskSucceeded, PublicErrorCode: 606, ErrorCode: "upstream_failed", ErrorMessage: "old poll failure"}, stage: "routing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.action != "" {
				decision, err := common.Marshal(service.ImageRetryDecision{Version: 1, Action: tc.action, Rule: tc.rule, Stage: tc.stage})
				require.NoError(t, err)
				tc.task.RetryDecision = string(decision)
			}
			data := imageTaskDTO(tc.task, false)
			assert.Equal(t, tc.task.PublicErrorCode, data["failure_code"])
			assert.Equal(t, tc.summary, data["error_summary"])
			if tc.name == "material_limit" {
				assert.Equal(t, "Reference image exceeds the upstream limit of {{limit}}", data["error_summary_key"])
				assert.Equal(t, map[string]any{"limit": "20MB"}, data["error_summary_params"])
			}
			assert.Contains(t, data["error_suggestion"], tc.suggestion)
			assert.Equal(t, tc.retryable, data["error_retryable"])
			assert.Equal(t, tc.stage, data["error_stage"])
		})
	}
	data := imageTaskDTO(model.AsyncImageTask{Status: model.ImageTaskFailed, PublicErrorCode: 610, ErrorMessage: `{"detail":{"message":"upstream rejected request; Bearer private-token"},"input":"private-prompt"}`}, false)
	assert.Contains(t, data["error_message"], "upstream rejected request")
	assert.NotContains(t, data["error_message"], "private-token")
	assert.NotContains(t, data["error_message"], "private-prompt")
}

func TestAsyncImageExternalReferencePreview(t *testing.T) {
	request := service.AsyncImageRequest{Parts: []service.AsyncImageInputPart{
		{Type: "text", Text: "draw this"},
		{Type: "image_url", URL: " https://example.com/reference.png "},
		{Type: "image_url", URL: "http://example.com/reference2.png"},
		{Type: "image_url", URL: "data:image/png;base64,private-base64-canary"},
		{Type: "image_url", URL: "newapi-input://private-input"},
		{Type: "mask", URL: "https://example.com/mask.png"},
	}}
	assert.Equal(t, []string{"https://example.com/reference.png", "http://example.com/reference2.png"}, request.ReferenceURLs())
}

func TestAsyncVideoRequestDetails(t *testing.T) {
	for _, item := range []struct {
		name    string
		body    string
		seconds float64
		media   []service.AsyncVideoReferenceMedia
		absent  []string
	}{
		{
			name:    "nested generation settings and mixed references",
			body:    `{"parameters":{"duration":6},"metadata":{"duration":9,"input":{"media":[{"type":"reference_video","url":"https://cdn.example.com/clip.mp4?signature=caller-owned"},{"type":"reference_audio","url":"https://cdn.example.com/audio.mp3"}]},"first_frame_url":"https://cdn.example.com/first.png","private_key":"private-key-canary"},"prompt":"landscape Bearer text-credential-canary","nested":{"x-api-key":"nested-key-canary"}}`,
			seconds: 6,
			media:   []service.AsyncVideoReferenceMedia{{Type: "video", URL: "https://cdn.example.com/clip.mp4?signature=caller-owned"}, {Type: "audio", URL: "https://cdn.example.com/audio.mp3"}, {Type: "image", URL: "https://cdn.example.com/first.png"}},
			absent:  []string{"private-key-canary", "text-credential-canary", "nested-key-canary"},
		},
		{
			name:  "reference duration does not become requested duration",
			body:  `{"reference_videos":[{"url":"https://cdn.example.com/reference.mp4","duration":15}]}`,
			media: []service.AsyncVideoReferenceMedia{{Type: "video", URL: "https://cdn.example.com/reference.mp4"}},
		},
		{
			name:   "binary payloads and unsafe URLs stay hidden",
			body:   `{"seconds":"NaN","image":"HTTPS://url-user:url-secret-canary@cdn.example.com/ref.png","video":"javascript:video-url-canary","audio":"data:audio/mp3;base64,inline-audio-canary","nested":{"image_base64":"raw-binary-canary"}}`,
			absent: []string{"url-secret-canary", "inline-audio-canary", "raw-binary-canary"},
		},
		{
			name:   "oversized fields are bounded",
			body:   `{"prompt":"` + strings.Repeat("p", 65537) + `","image":"https://cdn.example.com/ref.png"}`,
			media:  []service.AsyncVideoReferenceMedia{{Type: "image", URL: "https://cdn.example.com/ref.png"}},
			absent: []string{strings.Repeat("p", 1024)},
		},
	} {
		t.Run(item.name, func(t *testing.T) {
			details := (service.AsyncVideoRequest{Body: []byte(item.body), ContentType: "application/json"}).RequestDetails()
			require.NotNil(t, details)
			if item.seconds == 0 {
				assert.Nil(t, details.RequestedSeconds)
			} else {
				require.NotNil(t, details.RequestedSeconds)
				assert.Equal(t, item.seconds, *details.RequestedSeconds)
			}
			assert.ElementsMatch(t, item.media, details.ReferenceMedia)
			encoded, err := common.Marshal(details)
			require.NoError(t, err)
			for _, private := range item.absent {
				assert.NotContains(t, string(encoded), private)
			}
			assert.Less(t, len(encoded), 256<<10)
		})
	}
	t.Run("multipart keeps fields and file metadata without bytes", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		require.NoError(t, writer.WriteField("seconds", "12"))
		require.NoError(t, writer.WriteField("metadata", `{"audio_url":"https://cdn.example.com/sound.mp3","password":"form-secret-canary"}`))
		file, err := writer.CreateFormFile("input_reference", "reference.png")
		require.NoError(t, err)
		_, err = file.Write([]byte("uploaded-file-bytes-canary"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		details := (service.AsyncVideoRequest{Body: body.Bytes(), ContentType: writer.FormDataContentType()}).RequestDetails()
		require.NotNil(t, details)
		require.NotNil(t, details.RequestedSeconds)
		assert.Equal(t, float64(12), *details.RequestedSeconds)
		assert.Equal(t, []service.AsyncVideoReferenceMedia{{Type: "audio", URL: "https://cdn.example.com/sound.mp3"}}, details.ReferenceMedia)
		encoded, err := common.Marshal(details)
		require.NoError(t, err)
		assert.Contains(t, string(encoded), "reference.png")
		assert.NotContains(t, string(encoded), "uploaded-file-bytes-canary")
		assert.NotContains(t, string(encoded), "form-secret-canary")
	})
	for _, request := range []service.AsyncVideoRequest{{Body: []byte(`{"model":`), ContentType: "application/json"}, {Body: []byte(`{}`), ContentType: "invalid"}} {
		assert.Nil(t, request.RequestDetails())
	}
}

func TestAsyncVideoReferenceRetryAdmissionAndAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, changed, wantError string
	}{
		{name: "exhausted token reuses its original reservation"},
		{name: "submit adjustment retains the actual original reservation", changed: "adjusted hold"},
		{name: "current user group restrictions apply", changed: "group", wantError: "group permission changed"},
		{name: "disabled token cannot resubmit", changed: "disabled token", wantError: "Token is unavailable"},
		{name: "expired token cannot resubmit", changed: "expired token", wantError: "Token is unavailable"},
		{name: "disabled owner cannot resubmit", changed: "disabled owner", wantError: "owner is unavailable"},
		{name: "client IP restriction remains active", changed: "IP", wantError: "outside Token permissions"},
		{name: "removed model permission cannot resubmit", changed: "model", wantError: "model permission changed"},
		{name: "removed original account cannot resubmit", changed: "account", wantError: "account is unavailable"},
		{name: "disabled original account cannot resubmit", changed: "disabled account", wantError: "account is unavailable"},
		{name: "disabled channel cannot resubmit", changed: "channel", wantError: "channel is unavailable"},
		{name: "changed plugin cannot resubmit", changed: "plugin", wantError: "provider changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, token := setupResponsesWSRequestTest(t)
			db := model.DB
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Task{}, &model.AsyncMediaJob{}, &model.Log{}, &model.Option{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}))
			require.NoError(t, db.Model(user).Updates(map[string]any{"group": "vip", "quota": 900, "used_quota": 100, "request_count": 1}).Error)
			require.NoError(t, db.Model(token).Updates(map[string]any{"group": "default", "status": common.TokenStatusExhausted, "remain_quota": 0, "used_quota": 100}).Error)
			require.NoError(t, db.Take(user, user.Id).Error)
			require.NoError(t, db.Take(token, token.Id).Error)
			groups := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup
			previousGroups := groups.ReadAll()
			t.Cleanup(func() { groups.Clear(); groups.AddAll(previousGroups) })
			groups.Set("vip", map[string]string{})

			source, err := builtinplugins.Source("drama")
			require.NoError(t, err)
			previousRegistry := jsplugin.DefaultRegistry
			jsplugin.DefaultRegistry = jsplugin.NewRegistry()
			t.Cleanup(func() { jsplugin.DefaultRegistry = previousRegistry })
			plugin, err := jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "drama"})
			require.NoError(t, err)
			var submissions atomic.Int32
			posted := make(chan map[string]any, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				submissions.Add(1)
				assert.Equal(t, "/v1/videos", r.URL.Path)
				assert.Equal(t, "Bearer original-upstream-key", r.Header.Get("Authorization"))
				var body map[string]any
				if err := common.DecodeJson(r.Body, &body); !assert.NoError(t, err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				posted <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"replacement-native-job","status":"queued"}`))
			}))
			t.Cleanup(upstream.Close)
			setting := `{"task_plugin_key":"drama"}`
			channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Key: "original-upstream-key", BaseURL: &upstream.URL, Models: "seedance2.5-E", Group: "default", Status: common.ChannelStatusEnabled, Setting: &setting, UsedQuota: 100}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Group: "default", Model: "seedance2.5-E", Enabled: true}).Error)
			now := time.Now().Unix()
			job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: fmt.Sprintf("reference-retry-%d", user.Id), UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Provider: "drama", Model: "seedance2.5-E", Group: "default", Status: "queued", BillingStatus: "reserved", StorageStatus: "pending", Quota: 100, AttemptGeneration: 1, ReferenceRetryCount: 1, ReferenceRecoveryStage: "submit", LeaseToken: "reference-recovery-lease", LeaseExpiresAt: now + 120, CreatedAt: now, ExpiresAt: now + 86400}
			require.NoError(t, db.Create(&job).Error)
			execution := &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "drama", Version: plugin.Meta.Version, APIVersion: 1}}
			task := model.Task{TaskID: job.TaskId, Platform: "drama", UserId: user.Id, ChannelId: channel.Id, Group: "default", Status: model.TaskStatusQueued, Quota: 100, AttemptGeneration: 1, SubmitTime: now, Properties: model.Properties{OriginModelName: job.Model, UpstreamModelName: job.Model}, PrivateData: model.TaskPrivateData{AsyncMedia: true, DurableBilling: true, SubmitAccountingRecorded: true, TokenId: token.Id, Key: channel.Key, Execution: execution}}
			require.NoError(t, db.Create(&task).Error)
			references := []any{map[string]any{"type": "audio", "role": "reference", "source": "https://gateway.example/audio.mp3"}, map[string]any{"type": "image", "role": "reference", "source": "https://gateway.example/image.png"}, map[string]any{"type": "video", "role": "reference", "source": "https://gateway.example/video.mp4"}}
			body, err := common.Marshal(map[string]any{"model": job.Model, "prompt": "keep media order", "seconds": 8, "resolution": "720p", "references": references})
			require.NoError(t, err)
			request := service.AsyncVideoRequest{Body: body, ContentType: "application/json", ClientIP: "203.0.113.10", Channel: channel, Execution: execution, PluginHash: plugin.Engine.SourceFingerprint(), Billing: service.MediaBillingSnapshot{Price: types.PriceData{Quota: 100, QuotaToPreConsume: 100}}}
			switch tc.changed {
			case "adjusted hold":
				request.Billing.Price.Quota, request.Billing.Price.QuotaToPreConsume = 80, 80
			case "group":
				groups.Set("vip", map[string]string{"-:default": "restricted"})
			case "disabled token":
				require.NoError(t, db.Model(token).Update("status", common.TokenStatusDisabled).Error)
			case "expired token":
				require.NoError(t, db.Model(token).Update("expired_time", now-1).Error)
			case "disabled owner":
				require.NoError(t, db.Model(user).Update("status", common.UserStatusDisabled).Error)
			case "IP":
				require.NoError(t, db.Model(token).Update("allow_ips", "198.51.100.0/24").Error)
			case "model":
				require.NoError(t, db.Model(token).Updates(map[string]any{"model_limits_enabled": true, "model_limits": "other-video-model"}).Error)
			case "account":
				require.NoError(t, db.Model(&channel).Update("key", "different-upstream-key").Error)
			case "disabled account":
				info := model.ChannelInfo{IsMultiKey: true, MultiKeyStatusList: map[int]int{0: common.ChannelStatusManuallyDisabled}}
				require.NoError(t, db.Model(&channel).Update("channel_info", info).Error)
			case "channel":
				require.NoError(t, db.Model(&channel).Update("status", common.ChannelStatusManuallyDisabled).Error)
			case "plugin":
				request.PluginHash = "different-plugin-fingerprint"
			}
			err = ExecuteAsyncVideoReferenceRetry(t.Context(), job, task, request)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				assert.Zero(t, submissions.Load(), "rejected recovery must not reach upstream")
			} else {
				require.NoError(t, err)
				assert.EqualValues(t, 1, submissions.Load())
				assert.Equal(t, references, (<-posted)["references"], "retry changes no material position or role")
				require.Error(t, ExecuteAsyncVideoReferenceRetry(t.Context(), job, task, request), "the stale queued attempt must not dispatch twice")
				assert.EqualValues(t, 1, submissions.Load())
			}
			var storedTask model.Task
			require.NoError(t, db.Take(&storedTask, task.ID).Error)
			assert.Equal(t, task.TaskID, storedTask.TaskID)
			assert.Equal(t, 100, storedTask.Quota)
			if tc.wantError == "" {
				assert.Equal(t, "replacement-native-job", storedTask.PrivateData.UpstreamTaskID)
				assert.EqualValues(t, model.TaskStatusSubmitted, storedTask.Status)
			}
			var tasks, logs int64
			require.NoError(t, db.Model(&model.Task{}).Where("task_id = ?", task.TaskID).Count(&tasks).Error)
			require.NoError(t, db.Model(&model.Log{}).Where("user_id = ?", user.Id).Count(&logs).Error)
			assert.EqualValues(t, 1, tasks, "replacement native receipt belongs to the original task")
			assert.Zero(t, logs, "recovery must not emit a second consume or refund")
			require.NoError(t, db.Take(user, user.Id).Error)
			require.NoError(t, db.Take(token, token.Id).Error)
			require.NoError(t, db.Take(&channel, channel.Id).Error)
			assert.Equal(t, 900, user.Quota)
			assert.Equal(t, 100, user.UsedQuota)
			assert.Equal(t, 1, user.RequestCount)
			assert.Zero(t, token.RemainQuota)
			assert.Equal(t, 100, token.UsedQuota)
			assert.EqualValues(t, 100, channel.UsedQuota)
		})
	}
}

func TestAsyncVideoReferenceRecoveryAdministratorReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name, stage, body string
		admin, expired    bool
		want              int
	}{
		{name: "administrator confirms unknown submission", stage: "unknown", admin: true, body: `{"confirm_upstream_checked":true}`, want: http.StatusOK},
		{name: "administrator may reconcile an expired unknown submission", stage: "unknown", admin: true, expired: true, body: `{"confirm_upstream_checked":true}`, want: http.StatusOK},
		{name: "administrator must confirm upstream verification", stage: "unknown", admin: true, body: `{}`, want: http.StatusConflict},
		{name: "owner cannot reconcile unknown submission", stage: "unknown", body: `{"confirm_upstream_checked":true}`, want: http.StatusForbidden},
		{name: "administrator cannot terminate an ordinary queued recovery", stage: "prepare", admin: true, body: `{"confirm_upstream_checked":true}`, want: http.StatusConflict},
		{name: "owner cannot terminate an ordinary queued recovery", stage: "prepare", body: `{"confirm_upstream_checked":true}`, want: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, token := setupResponsesWSRequestTest(t)
			db := model.DB
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Task{}, &model.AsyncMediaJob{}, &model.Log{}, &model.MediaInputObject{}, &model.CanvasGrant{}))
			require.NoError(t, db.Model(user).Updates(map[string]any{"quota": 900, "used_quota": 100, "request_count": 1}).Error)
			require.NoError(t, db.Model(token).Updates(map[string]any{"remain_quota": 0, "used_quota": 100}).Error)
			channel := model.Channel{Key: "original-upstream-key", UsedQuota: 100}
			require.NoError(t, db.Create(&channel).Error)
			now := time.Now().Unix()
			job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: fmt.Sprintf("reference-reconcile-%d", user.Id), UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Provider: "drama", Model: "seedance2.5-E", Group: "default", Status: "execution_unknown", BillingStatus: "unknown", StorageStatus: "pending", Quota: 100, AttemptGeneration: 1, ReferenceRetryCount: 1, ReferenceRecoveryStage: tc.stage, RequestCipher: []byte("encrypted-original-request"), CreatedAt: now, ExpiresAt: now + 86400, ErrorMessage: "Upstream recovery submission could not be confirmed"}
			task := model.Task{TaskID: job.TaskId, Platform: "drama", UserId: user.Id, ChannelId: channel.Id, Group: "default", Status: model.TaskStatusUnknown, Quota: 100, AttemptGeneration: 1, SubmitTime: now, PrivateData: model.TaskPrivateData{AsyncMedia: true, DurableBilling: true, SubmitAccountingRecorded: true, TokenId: token.Id, BillingSource: "wallet", Key: channel.Key}}
			if tc.expired {
				job.ExpiresAt = now - 1
			}
			if tc.stage == "prepare" {
				job.Status, job.BillingStatus, task.Status = "queued", "reserved", model.TaskStatusQueued
			}
			require.NoError(t, db.Create(&job).Error)
			require.NoError(t, db.Create(&task).Error)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Set("id", user.Id)
			c.Params = gin.Params{{Key: "task_id", Value: job.TaskId}}
			c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/media-tasks/"+job.TaskId+"/terminate", strings.NewReader(tc.body))
			ManageMediaTask(c, tc.admin, "terminate")
			require.Equal(t, tc.want, recorder.Code, recorder.Body.String())
			var storedTask model.Task
			var storedJob model.AsyncMediaJob
			require.NoError(t, db.Take(&storedTask, task.ID).Error)
			require.NoError(t, db.Take(&storedJob, job.ID).Error)
			wallet, quota, used, refunds := 900, 100, 100, int64(0)
			if tc.want == http.StatusOK {
				wallet, quota, used, refunds = 1000, 0, 0, 1
				assert.EqualValues(t, model.TaskStatusFailure, storedTask.Status)
				assert.Equal(t, "failed", storedJob.Status)
				assert.Equal(t, "refunded", storedJob.BillingStatus)
				assert.Equal(t, "complete", storedJob.ReferenceRecoveryStage)
				assert.Empty(t, storedJob.RequestCipher)
				repeated := httptest.NewRecorder()
				repeatContext, _ := gin.CreateTestContext(repeated)
				repeatContext.Set("id", user.Id)
				repeatContext.Params = c.Params
				repeatContext.Request = httptest.NewRequest(http.MethodPost, c.Request.URL.String(), strings.NewReader(tc.body))
				ManageMediaTask(repeatContext, tc.admin, "terminate")
				assert.Equal(t, http.StatusConflict, repeated.Code, "confirmation cannot refund a second time")
			} else {
				assert.Equal(t, task.Status, storedTask.Status)
				assert.Equal(t, job.Status, storedJob.Status)
				assert.Equal(t, job.BillingStatus, storedJob.BillingStatus)
				assert.Equal(t, job.RequestCipher, storedJob.RequestCipher)
			}
			assert.Equal(t, quota, storedTask.Quota)
			assert.Equal(t, quota, storedJob.Quota)
			require.NoError(t, db.Take(user, user.Id).Error)
			require.NoError(t, db.Take(token, token.Id).Error)
			require.NoError(t, db.Take(&channel, channel.Id).Error)
			assert.Equal(t, wallet, user.Quota)
			assert.Equal(t, wallet-900, token.RemainQuota)
			assert.Equal(t, used, user.UsedQuota)
			assert.Equal(t, used, token.UsedQuota)
			assert.EqualValues(t, used, channel.UsedQuota)
			assert.Equal(t, 1, user.RequestCount)
			var logs []model.Log
			require.NoError(t, db.Where("user_id = ?", user.Id).Find(&logs).Error)
			require.Len(t, logs, int(refunds))
			if len(logs) > 0 {
				assert.Equal(t, model.LogTypeRefund, logs[0].Type)
				assert.Equal(t, 100, logs[0].Quota)
			}
		})
	}
}
