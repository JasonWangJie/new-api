package controller

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
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
			previousDB := model.DB
			model.DB = db
			entities := []any{&model.User{}, &model.Token{}, &model.Channel{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}, &model.ImageOutbox{}, &model.AsyncImageResult{}, &model.ImageStorageObject{}, &model.ImageStorageProfile{}, &model.Option{}, &model.Task{}, &model.AsyncMediaJob{}, &model.MediaArtifactObject{}}
			t.Cleanup(func() {
				model.DB = previousDB
				assert.NoError(t, db.Migrator().DropTable(entities...))
				connection, closeErr := db.DB()
				if assert.NoError(t, closeErr) {
					assert.NoError(t, connection.Close())
				}
			})
			require.NoError(t, db.AutoMigrate(entities...))
			require.NoError(t, db.AutoMigrate(entities...))
			var version string
			versionQuery := "SELECT VERSION()"
			if engine == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("Database version: %s", version)
			now := time.Now().Unix()
			require.NoError(t, db.Create(&model.User{Id: 101, Username: "creator", DisplayName: "Creative account", Password: "password-secret-canary", AffCode: "creator"}).Error)
			require.NoError(t, db.Create(&model.User{Id: 102, Username: "other", AffCode: "other"}).Error)
			require.NoError(t, db.Create(&model.Token{Id: 201, UserId: 101, Name: "Studio Key", Key: "token-secret-canary"}).Error)
			require.NoError(t, db.Create(&model.Token{Id: 202, UserId: 102, Name: "Other user key", Key: "other-token-secret"}).Error)
			require.NoError(t, db.Create(&model.Channel{Id: 301, Name: "Sunburst upstream", Key: "channel-secret-canary"}).Error)
			attempts, err := common.Marshal([]service.ImageChannelAttempt{{ChannelId: 301, KeyFingerprint: "fingerprint-canary", KeyIndex: 2, StartedAt: now - 40, FinishedAt: now - 10, Dispatched: true}})
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
						assert.NotContains(t, string(history), "fingerprint-canary")
					} else {
						assert.NotContains(t, detailRecorder.Body.String(), "Sunburst upstream")
						assert.NotContains(t, detailRecorder.Body.String(), "routing-event-canary")
						assert.NotContains(t, detailRecorder.Body.String(), "fingerprint-canary")
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
		})
	}
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
