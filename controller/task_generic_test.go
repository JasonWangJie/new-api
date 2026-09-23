package controller

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupGenericTaskTest(t *testing.T) *model.Task {
	t.Helper()
	originalDB := model.DB
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.Task{}, &model.Channel{}, &model.User{}))
	model.DB = database
	t.Cleanup(func() {
		model.DB = originalDB
		common.RedisEnabled = previousRedisEnabled
	})

	require.NoError(t, database.Create(&model.User{
		Id: 7, Username: "artifact-owner", Status: common.UserStatusEnabled,
		Role: common.RoleCommonUser, Group: "default",
	}).Error)
	baseURL := "https://example.com"
	require.NoError(t, database.Create(&model.Channel{
		Id: 1, Name: "artifact", Key: "key", BaseURL: &baseURL, Status: common.ChannelStatusEnabled,
	}).Error)
	task := &model.Task{
		TaskID: "task_generic", Platform: "document", UserId: 7, ChannelId: 1,
		Status: model.TaskStatusSuccess, Progress: "100%", SubmitTime: 10, FinishTime: 20,
	}
	require.NoError(t, database.Create(task).Error)
	return task
}

func allowPrivateTaskMediaTest(t *testing.T) {
	t.Helper()
	originalFetchSetting := *system_setting.GetFetchSetting()
	system_setting.GetFetchSetting().EnableSSRFProtection = true
	system_setting.GetFetchSetting().AllowPrivateIp = true
	system_setting.GetFetchSetting().AllowedPorts = []string{"1-65535"}
	t.Cleanup(func() { *system_setting.GetFetchSetting() = originalFetchSetting })
	service.InitHttpClient()
}

func TestGetTaskDoesNotProjectArtifacts(t *testing.T) {
	task := setupGenericTaskTest(t)
	task.FailReason = "https://stale-upstream.invalid/video.mp4"
	task.PrivateData = model.TaskPrivateData{
		ResultURL: "https://private-upstream.invalid/video.mp4",
		Execution: &model.TaskExecutionSnapshot{
			TaskPlugin: &model.TaskPluginSnapshot{Key: "missing-plugin", Name: "Missing"},
		},
	}
	require.NoError(t, model.DB.Save(task).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", 7)
	c.Params = gin.Params{{Key: "key", Value: task.TaskID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.TaskID, nil)

	GetTask(c)

	assert.Equal(t, http.StatusOK, recorder.Code)
	var response map[string]any
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, task.TaskID, response["task_id"])
	assert.NotContains(t, response, "artifacts")
	assert.NotContains(t, recorder.Body.String(), "upstream.invalid")
}

func TestGetTaskArtifactsReturnsEmptyForLegacyTask(t *testing.T) {
	task := setupGenericTaskTest(t)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", task.UserId)
	c.Params = gin.Params{{Key: "key", Value: task.TaskID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.TaskID+"/artifacts", nil)

	GetTaskArtifacts(c)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
	var response struct {
		TaskID    string                 `json:"task_id"`
		Artifacts []taskArtifactResponse `json:"artifacts"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, task.TaskID, response.TaskID)
	assert.Empty(t, response.Artifacts)
}

func TestTaskArtifactAuthorizationKeepsForeignTasksHidden(t *testing.T) {
	task := setupGenericTaskTest(t)

	commonUser, _ := gin.CreateTestContext(httptest.NewRecorder())
	commonUser.Set("id", 8)
	commonUser.Set("role", common.RoleCommonUser)
	_, exists, err := getTaskForArtifactRequest(commonUser, task.TaskID)
	require.NoError(t, err)
	assert.False(t, exists)

	admin, _ := gin.CreateTestContext(httptest.NewRecorder())
	admin.Set("id", 8)
	admin.Set("role", common.RoleAdminUser)
	found, exists, err := getTaskForArtifactRequest(admin, task.TaskID)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, task.TaskID, found.TaskID)

	apiToken, _ := gin.CreateTestContext(httptest.NewRecorder())
	apiToken.Set("id", 8)
	apiToken.Set("role", common.RoleRootUser)
	apiToken.Set("token_id", 99)
	_, exists, err = getTaskForArtifactRequest(apiToken, task.TaskID)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestDashboardTaskArtifactsReturnsLegacyCapabilityWithoutUpstreamURL(t *testing.T) {
	task := setupGenericTaskTest(t)
	previousSecret := common.CryptoSecret
	previousPublicAddress := system_setting.TaskPublicAddress
	common.CryptoSecret = "controller-task-artifact-access-secret"
	system_setting.TaskPublicAddress = "https://gateway.example/prefix"
	t.Cleanup(func() {
		common.CryptoSecret = previousSecret
		system_setting.TaskPublicAddress = previousPublicAddress
	})
	task.Action = constant.TaskActionTextToVideo
	task.FailReason = "https://upstream.invalid/private-video.mp4?signature=secret"
	require.NoError(t, model.DB.Save(task).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", task.UserId)
	c.Set("role", common.RoleCommonUser)
	c.Params = gin.Params{{Key: "task_id", Value: task.TaskID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/task/"+task.TaskID+"/artifacts", nil)

	GetDashboardTaskArtifacts(c)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Artifacts        []taskArtifactResponse `json:"artifacts"`
			LegacyContentURL string                 `json:"legacy_content_url"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Empty(t, response.Data.Artifacts)
	contentURL, err := url.Parse(response.Data.LegacyContentURL)
	require.NoError(t, err)
	assert.Equal(t, "/prefix/v1/tasks/"+task.TaskID+"/artifacts/video/content", contentURL.Path)
	assert.True(t, service.VerifyTaskArtifactAccess(
		contentURL.Query().Get(service.TaskArtifactAccessQueryParameter),
		task.TaskID,
		"video",
	))
	assert.NotContains(t, recorder.Body.String(), "upstream.invalid")
	assert.NotContains(t, recorder.Body.String(), "signature=secret")
}

func TestTaskArtifactAccessRequiresActiveOwner(t *testing.T) {
	task := setupGenericTaskTest(t)
	task.Action = constant.TaskActionTextToVideo
	task.FailReason = "https://upstream.invalid/private-video.mp4"
	require.NoError(t, model.DB.Save(task).Error)
	require.NoError(t, model.DB.Model(&model.User{}).
		Where("id = ?", task.UserId).
		Update("status", common.UserStatusDisabled).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(middleware.TaskArtifactAccessContextKey, true)
	c.Params = gin.Params{
		{Key: "key", Value: task.TaskID},
		{Key: "artifact_key", Value: "video"},
	}
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/v1/tasks/"+task.TaskID+"/artifacts/video/content",
		nil,
	)

	TaskArtifactContent(c)

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
}

func TestTaskArtifactAccessRejectsAmbiguousHistoricalTaskID(t *testing.T) {
	task := setupGenericTaskTest(t)
	task.Action = constant.TaskActionTextToVideo
	task.FailReason = "https://first-upstream.invalid/video.mp4"
	require.NoError(t, model.DB.Save(task).Error)
	require.NoError(t, model.DB.Create(&model.User{
		Id: 8, Username: "other-artifact-owner", Status: common.UserStatusEnabled,
		Role: common.RoleCommonUser, Group: "default", AffCode: "artifact-owner-8",
	}).Error)
	require.NoError(t, model.DB.Create(&model.Task{
		TaskID: task.TaskID, Platform: task.Platform, UserId: 8, ChannelId: task.ChannelId,
		Action: constant.TaskActionTextToVideo, Status: model.TaskStatusSuccess,
		FailReason: "https://second-upstream.invalid/video.mp4",
	}).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(middleware.TaskArtifactAccessContextKey, true)
	c.Params = gin.Params{
		{Key: "key", Value: task.TaskID},
		{Key: "artifact_key", Value: "video"},
	}
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/v1/tasks/"+task.TaskID+"/artifacts/video/content",
		nil,
	)

	TaskArtifactContent(c)

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "upstream.invalid")
}

func TestLegacyVideoArtifactContentUsesGetResultURL(t *testing.T) {
	task := setupGenericTaskTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "bytes=0-3", r.Header.Get("Range"))
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-3/4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("data"))
	}))
	defer upstream.Close()
	allowPrivateTaskMediaTest(t)

	task.Action = constant.TaskActionTextToVideo
	task.PrivateData.ResultURL = upstream.URL
	task.FailReason = "https://stale.invalid/legacy-fallback.mp4"
	require.NoError(t, model.DB.Save(task).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(middleware.TaskArtifactAccessContextKey, true)
	c.Params = gin.Params{
		{Key: "key", Value: task.TaskID},
		{Key: "artifact_key", Value: "video"},
	}
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/v1/tasks/"+task.TaskID+"/artifacts/video/content",
		nil,
	)
	c.Request.Header.Set("Range", "bytes=0-3")

	TaskArtifactContent(c)

	assert.Equal(t, http.StatusPartialContent, recorder.Code)
	assert.Equal(t, "data", recorder.Body.String())
	assert.Equal(t, "bytes 0-3/4", recorder.Header().Get("Content-Range"))
}

func TestDisabledArtifactStorePreservesPluginUpstreamContent(t *testing.T) {
	task := setupGenericTaskTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "provider-key", r.Header.Get("x-goog-api-key"))
		assert.Equal(t, "bytes=0-13", r.Header.Get("Range"))
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-13/14")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("artifact-bytes"))
	}))
	defer upstream.Close()
	allowPrivateTaskMediaTest(t)
	previousMemoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCache })

	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", task.ChannelId).Updates(map[string]any{
		"type":     constant.ChannelTypeGemini,
		"key":      "provider-key",
		"base_url": upstream.URL,
	}).Error)
	task.Platform = constant.TaskPlatform("google")
	task.PrivateData.Execution = &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{
		Key: "google", Name: "Google Veo (Gemini API)", Version: "1.0.0", APIVersion: 1,
	}}
	task.SetData(map[string]any{"response": map[string]any{
		"generateVideoResponse": map[string]any{
			"generatedVideos": []any{map[string]any{"video": map[string]any{"uri": upstream.URL}}},
		},
	}})
	require.NoError(t, model.DB.Save(task).Error)
	require.False(t, service.GetTaskArtifactStore().Enabled())

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(middleware.TaskArtifactAccessContextKey, true)
	c.Params = gin.Params{
		{Key: "key", Value: task.TaskID},
		{Key: "artifact_key", Value: "video"},
	}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.TaskID+"/artifacts/video/content", nil)
	c.Request.Header.Set("Range", "bytes=0-13")

	TaskArtifactContent(c)

	assert.Equal(t, http.StatusPartialContent, recorder.Code)
	assert.Equal(t, "artifact-bytes", recorder.Body.String())
	assert.Equal(t, "video/mp4", recorder.Header().Get("Content-Type"))
	assert.Equal(t, "bytes 0-13/14", recorder.Header().Get("Content-Range"))
}

func TestProjectedTaskArtifactValidationRejectsAmbiguousIdentity(t *testing.T) {
	validated, err := validateProjectedTaskArtifacts([]relaychannel.TaskArtifact{
		{Key: "video-main", Type: "video", MimeType: "video/mp4"},
		{Key: "cover.main", Type: "image", MimeType: "image/png"},
	})
	require.NoError(t, err)
	require.Len(t, validated, 2)
	assert.Equal(t, "video-main", validated[0].Key)

	for _, artifacts := range [][]relaychannel.TaskArtifact{
		{{Key: "../video", Type: "video"}},
		{{Key: "video/0", Type: "video"}},
		{{Key: "video-main", Type: "video"}, {Key: "video-main", Type: "image"}},
		{{Key: "video-main", Type: "unknown"}},
		{{Key: "video-main", Type: "video", MimeType: "video/mp4\r\nX-Test: injected"}},
	} {
		_, err := validateProjectedTaskArtifacts(artifacts)
		assert.ErrorIs(t, err, errTaskArtifactPlugin)
	}
}

func TestProxyTaskMediaForwardsRangeAndFiltersResponseHeaders(t *testing.T) {
	task := setupGenericTaskTest(t)
	var receivedRange, receivedAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedRange = r.Header.Get("Range")
		receivedAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Set-Cookie", "provider=secret")
		w.Header().Set("WWW-Authenticate", "Bearer provider")
		w.Header().Set("X-Provider-Secret", "hidden")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("data"))
	}))
	defer upstream.Close()

	allowPrivateTaskMediaTest(t)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/tasks/task_generic/artifacts/video-main/content", nil)
	c.Request.Header.Set("Range", "bytes=0-3")

	err := proxyTaskMedia(c, task, &relaychannel.TaskContentRequest{
		URL:     upstream.URL,
		Method:  http.MethodGet,
		Headers: map[string]string{"Authorization": "Bearer provider-secret"},
	})

	require.NoError(t, err)
	assert.Equal(t, http.StatusPartialContent, recorder.Code)
	assert.Equal(t, "data", recorder.Body.String())
	assert.Equal(t, "bytes=0-3", receivedRange)
	assert.Equal(t, "Bearer provider-secret", receivedAuthorization)
	assert.Equal(t, "bytes 0-3/10", recorder.Header().Get("Content-Range"))
	assert.Equal(t, "bytes", recorder.Header().Get("Accept-Ranges"))
	assert.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
	assert.Equal(t, "sandbox; default-src 'none'", recorder.Header().Get("Content-Security-Policy"))
	assert.Equal(t, "no-referrer", recorder.Header().Get("Referrer-Policy"))
	assert.Equal(t, "nosniff", recorder.Header().Get("X-Content-Type-Options"))
	assert.Empty(t, recorder.Header().Get("Set-Cookie"))
	assert.Empty(t, recorder.Header().Get("WWW-Authenticate"))
	assert.Empty(t, recorder.Header().Get("X-Provider-Secret"))
}

func TestProxyTaskMediaPassesThroughUnsatisfiedRange(t *testing.T) {
	task := setupGenericTaskTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes */10")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer upstream.Close()

	allowPrivateTaskMediaTest(t)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/content", nil)

	require.NoError(t, proxyTaskMedia(c, task, &relaychannel.TaskContentRequest{
		URL: upstream.URL, Method: http.MethodGet,
	}))
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, recorder.Code)
	assert.Equal(t, "bytes */10", recorder.Header().Get("Content-Range"))
	assert.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
}

func TestTaskMediaResponseHeaderTimeoutDoesNotTruncateBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(75 * time.Millisecond)
		_, _ = w.Write([]byte("complete-body"))
	}))
	defer upstream.Close()

	request, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	require.NoError(t, err)
	response, err := doTaskMediaRequest(upstream.Client(), request, 20*time.Millisecond)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Equal(t, "complete-body", string(body))
}

func TestTaskMediaResponseHeaderTimeoutCancelsBeforeHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(75 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	defer upstream.Close()

	request, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	require.NoError(t, err)
	_, err = doTaskMediaRequest(upstream.Client(), request, 10*time.Millisecond)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWriteVideoDataURLStreamsAndSupportsHead(t *testing.T) {
	const dataURL = "data:video/mp4;base64,Y29tcGxldGUtYm9keQ=="

	getRecorder := httptest.NewRecorder()
	getContext, _ := gin.CreateTestContext(getRecorder)
	getContext.Request = httptest.NewRequest(http.MethodGet, "/content", nil)
	require.NoError(t, writeVideoDataURL(getContext, dataURL))
	assert.Equal(t, http.StatusOK, getRecorder.Code)
	assert.Equal(t, "complete-body", getRecorder.Body.String())
	assert.Equal(t, "13", getRecorder.Header().Get("Content-Length"))

	headRecorder := httptest.NewRecorder()
	headContext, _ := gin.CreateTestContext(headRecorder)
	headContext.Request = httptest.NewRequest(http.MethodHead, "/content", nil)
	require.NoError(t, writeVideoDataURL(headContext, dataURL))
	assert.Equal(t, http.StatusOK, headRecorder.Code)
	assert.Empty(t, headRecorder.Body.String())
	assert.Equal(t, "13", headRecorder.Header().Get("Content-Length"))
}

func TestWriteVideoDataURLRejectsOversizedPayloadBeforeDecode(t *testing.T) {
	previousLimit := taskMediaDataURLMaxEncodedBytes
	taskMediaDataURLMaxEncodedBytes = 32
	t.Cleanup(func() { taskMediaDataURLMaxEncodedBytes = previousLimit })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/content", nil)

	err := writeVideoDataURL(c, "data:video/mp4;base64,"+strings.Repeat("A", 64))

	assert.ErrorIs(t, err, errTaskMediaRequestRejected)
	assert.Empty(t, recorder.Header().Get("Content-Type"))
}

func TestProxyTaskMediaAllowsOnlyCredentiallessCrossOriginRedirect(t *testing.T) {
	task := setupGenericTaskTest(t)
	var destinationAuthorization, destinationProviderKey, destinationRange string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationAuthorization = r.Header.Get("Authorization")
		destinationProviderKey = r.Header.Get("X-Provider-Key")
		destinationRange = r.Header.Get("Range")
		_, _ = w.Write([]byte("redirected"))
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer source.Close()
	allowPrivateTaskMediaTest(t)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/content", nil)
	c.Request.Header.Set("Range", "bytes=0-3")

	err := proxyTaskMedia(c, task, &relaychannel.TaskContentRequest{
		URL: source.URL, Method: http.MethodGet, Credentialless: true,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "redirected", recorder.Body.String())
	assert.Empty(t, destinationAuthorization)
	assert.Equal(t, "bytes=0-3", destinationRange)

	destinationRange = ""
	rejectedRecorder := httptest.NewRecorder()
	rejectedContext, _ := gin.CreateTestContext(rejectedRecorder)
	rejectedContext.Request = httptest.NewRequest(http.MethodGet, "/content", nil)
	err = proxyTaskMedia(rejectedContext, task, &relaychannel.TaskContentRequest{
		URL: source.URL, Method: http.MethodGet,
		Headers: map[string]string{"Authorization": "Bearer provider-secret"},
	})
	var proxyErr *taskMediaProxyError
	require.ErrorAs(t, err, &proxyErr)
	assert.Equal(t, "artifact_request_rejected", proxyErr.code)
	assert.Empty(t, destinationRange)

	// Dynamic upstream downloads may start with same-origin credentials and
	// continue at a public CDN. The redirected request must lose every secret.
	task.PrivateData.UpstreamAsync = &relaydto.UpstreamAsyncProfile{}
	redirectedRecorder := httptest.NewRecorder()
	redirectedContext, _ := gin.CreateTestContext(redirectedRecorder)
	redirectedContext.Request = httptest.NewRequest(http.MethodGet, "/content", nil)
	redirectedContext.Request.Header.Set("Range", "bytes=0-3")
	require.NoError(t, proxyTaskMedia(redirectedContext, task, &relaychannel.TaskContentRequest{
		URL: source.URL, Method: http.MethodGet,
		Headers: map[string]string{"Authorization": "Bearer provider-secret", "X-Provider-Key": "private"},
	}))
	assert.Equal(t, http.StatusOK, redirectedRecorder.Code)
	assert.Equal(t, "redirected", redirectedRecorder.Body.String())
	assert.Empty(t, destinationAuthorization)
	assert.Empty(t, destinationProviderKey)
	assert.Equal(t, "bytes=0-3", destinationRange)
}

func TestPersistAsyncVideoRefreshesDynamicResultURL(t *testing.T) {
	task := setupGenericTaskTest(t)
	connection, err := model.DB.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	allowPrivateTaskMediaTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}, &model.AsyncMediaJob{}, &model.MediaArtifactObject{}))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "test")
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:"+base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))
	cfg := service.DefaultMediaRuntimeConfig()
	cfg.VideoAsyncEnabled, cfg.LocalPath, cfg.MaxFileBytes = true, t.TempDir(), 4096
	require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))

	box := func(kind string, payload []byte) []byte {
		result := make([]byte, 8+len(payload))
		binary.BigEndian.PutUint32(result, uint32(len(result)))
		copy(result[4:8], kind)
		copy(result[8:], payload)
		return result
	}
	handler := make([]byte, 25)
	copy(handler[8:12], "vide")
	video := box("ftyp", []byte("isom\x00\x00\x00\x00"))
	video = append(video, box("moov", box("trak", box("mdia", box("hdlr", handler))))...)
	video = append(video, box("mdat", []byte("video-sample"))...)

	var upstream *httptest.Server
	polls, freshDownloads, staleDownloads := 0, 0, 0
	upstream = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/tasks/vendor-1":
			polls++
			assert.Equal(t, "Bearer frozen-key", request.Header.Get("Authorization"))
			_, _ = io.WriteString(writer, `{"status":"done","url":"`+upstream.URL+`/fresh"}`)
		case "/fresh":
			freshDownloads++
			writer.Header().Set("Content-Type", "video/mp4")
			_, _ = writer.Write(video)
		case "/stale":
			staleDownloads++
			writer.WriteHeader(http.StatusGone)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	var profile relaydto.UpstreamAsyncProfile
	require.NoError(t, common.Unmarshal([]byte(`{"id":"video-job","media_type":"video","models":["mapped"],"operations":["generate"],"submit":{"task_id_path":"id"},"poll":{"request":{"method":"GET","path":"/tasks/{task_id}","headers":{"Authorization":"Bearer {api_key}"}},"response":{"status_path":"status","status_values":{"succeeded":["done"],"failed":["failed"]},"result_path":"url"}}}`), &profile))
	require.NoError(t, (&relaydto.UpstreamAsyncConfig{Profiles: []relaydto.UpstreamAsyncProfile{profile}}).Validate())
	frozen := model.Channel{Id: task.ChannelId, Type: constant.ChannelTypeKling, Key: "frozen-key", BaseURL: &upstream.URL}
	channelBytes, err := common.Marshal(frozen)
	require.NoError(t, err)
	channelCipher, err := service.EncryptImagePayload(channelBytes, "media-channel:"+task.TaskID)
	require.NoError(t, err)
	job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: "dynamic-refresh", UserId: task.UserId, TokenId: 2, ChannelId: task.ChannelId, Status: "submitted", StorageStatus: "failed", ChannelCipher: channelCipher, ExpiresAt: time.Now().Unix() + 3600}
	require.NoError(t, model.DB.Create(&job).Error)
	task.Platform = constant.TaskPlatform("kling")
	task.PrivateData = model.TaskPrivateData{
		AsyncMedia: true, UpstreamTaskID: "vendor-1", ResultURL: upstream.URL + "/stale",
		UpstreamAsync: &profile, Key: "frozen-key",
		Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "kling"}},
	}
	task.Properties.OriginModelName, task.Properties.UpstreamModelName = "client-model", "mapped"
	require.NoError(t, model.DB.Save(task).Error)
	captured, err := service.CaptureAsyncMediaOutput(t.Context(), task)
	require.NoError(t, err)
	require.True(t, captured)
	require.NoError(t, model.DB.First(&job, job.ID).Error)

	require.NoError(t, PersistAsyncVideo(t.Context(), job, task))
	assert.Equal(t, 1, polls)
	assert.Equal(t, 1, freshDownloads)
	assert.Zero(t, staleDownloads)
	ref, err := service.GetTaskArtifactStore().Resolve(task, "video")
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.EqualValues(t, len(video), ref.Size)
}

func TestTaskMediaRequestHeaderPolicy(t *testing.T) {
	header := http.Header{}
	require.NoError(t, applyTaskMediaRequestHeaders(header, map[string]string{
		"Authorization": "Bearer provider-secret",
		"X-Signature":   "signed",
	}))
	assert.Equal(t, "Bearer provider-secret", header.Get("Authorization"))
	assert.Equal(t, "signed", header.Get("X-Signature"))

	for _, name := range []string{"Host", "Content-Length", "Accept-Encoding", "Connection", "Proxy-Authorization", "Transfer-Encoding"} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, applyTaskMediaRequestHeaders(http.Header{}, map[string]string{name: "bad"}), errTaskMediaRequestRejected)
		})
	}
	assert.ErrorIs(t, applyTaskMediaRequestHeaders(http.Header{}, map[string]string{"X-Test": "bad\r\ninjected"}), errTaskMediaRequestRejected)
}

func TestCredentiallessTaskMediaDescriptorRejectsCredentialsAndBody(t *testing.T) {
	task := setupGenericTaskTest(t)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/content", nil)

	for _, descriptor := range []*relaychannel.TaskContentRequest{
		{URL: "https://example.com/video", Method: http.MethodPost, Credentialless: true},
		{URL: "https://example.com/video", Method: http.MethodGet, Body: []byte("secret"), Credentialless: true},
		{URL: "https://example.com/video", Method: http.MethodGet, Headers: map[string]string{"X-Key": "secret"}, Credentialless: true},
	} {
		err := proxyTaskMedia(c, task, descriptor)
		var proxyErr *taskMediaProxyError
		require.ErrorAs(t, err, &proxyErr)
		assert.Equal(t, "artifact_request_rejected", proxyErr.code)
	}
}

func TestSelfTaskMediaURLGuard(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "https://gateway.example/v1/videos/task-1/content", nil)
	c.Request.Host = "gateway.example"

	selfURL, err := url.Parse("https://gateway.example/v1/videos/task-1/content")
	require.NoError(t, err)
	assert.True(t, isSelfTaskMediaURL(c, selfURL))

	remoteURL, err := url.Parse("https://cdn.example/v1/videos/task-1/content")
	require.NoError(t, err)
	assert.False(t, isSelfTaskMediaURL(c, remoteURL))
	assert.True(t, isTaskMediaFallbackLoop(remoteURL.String(), "task-1"))
	assert.False(t, isTaskMediaFallbackLoop(remoteURL.String(), "task-2"))
}
