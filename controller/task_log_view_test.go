package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVideoRoutingGenericTaskKeepsExecutionPrivate(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	db := setupVideoRetryTestDatabase(t, user, token)
	require.NoError(t, db.AutoMigrate(&model.Task{}))
	tasks := []model.Task{
		{
			TaskID: "conditional-generic-task", UserId: user.Id, Platform: "private-provider", ChannelId: 49,
			Status: model.TaskStatusInProgress, Progress: "40%", CreatedAt: 10, SubmitTime: 10,
			Properties: model.Properties{OriginModelName: "public-A", UpstreamModelName: "private-E"},
			PrivateData: model.TaskPrivateData{AsyncMedia: true, VideoRouting: &dto.VideoRoutingAudit{
				RuleID: "private-routing-rule", SourceModel: "public-A", ExecutionModel: "private-E",
			}},
		},
		{
			TaskID: "ordinary-generic-task", UserId: user.Id, Platform: "ordinary-provider",
			Status: model.TaskStatusInProgress, Progress: "40%", CreatedAt: 10, SubmitTime: 10,
		},
	}
	require.NoError(t, db.Create(&tasks).Error)
	router := gin.New()
	router.GET("/v1/tasks/:key", middleware.TokenAuth(), GetTask)
	for _, task := range tasks {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.TaskID, nil)
		request.Header.Set("Authorization", "Bearer sk-"+token.Key)
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var response map[string]any
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		assert.Equal(t, task.TaskID, response["task_id"])
		assert.Equal(t, string(model.TaskStatusInProgress), response["status"])
		assert.Equal(t, "40%", response["progress"])
		assert.EqualValues(t, 10, response["created_at"])
		if task.PrivateData.VideoRouting != nil {
			assert.NotContains(t, response, "platform")
			assert.NotContains(t, recorder.Body.String(), "private-provider")
			assert.NotContains(t, recorder.Body.String(), "private-E")
			assert.NotContains(t, recorder.Body.String(), "private-routing-rule")
		} else {
			assert.Equal(t, "ordinary-provider", response["platform"])
		}
	}
}

func TestVideoRoutingTaskDetailsKeepExecutionPrivate(t *testing.T) {
	task := &model.Task{
		TaskID:     "routed-video",
		Platform:   "private-provider",
		ChannelId:  7,
		Status:     model.TaskStatusFailure,
		FailReason: "private-E unsupported parameter from private-provider",
		Properties: model.Properties{OriginModelName: "public-A", UpstreamModelName: "private-E"},
		Data:       []byte(`{"model":"private-E","provider_detail":"private-ledger"}`),
		PrivateData: model.TaskPrivateData{AsyncMedia: true, VideoRouting: &dto.VideoRoutingAudit{
			Revision: 3, RuleID: "long-video", RuleName: "Long video route", SourceModel: "public-A",
			SourceResolution: "480p", ExecutionModel: "private-E", ExecutionResolution: "720p",
			Targets: []dto.VideoRoutingTargetAudit{{ChannelID: 7, Model: "target-alias", UpstreamModel: "private-E", Resolution: "720p"}},
		}},
	}
	for _, role := range []int{common.RoleCommonUser, common.RoleAdminUser, common.RoleRootUser} {
		view := tasksToDto([]*model.Task{task}, false, role)[0]
		assert.Equal(t, "480p", view.RequestedResolution)
		assert.Equal(t, "720p", view.ExecutionResolution)
		encoded, err := common.Marshal(view)
		require.NoError(t, err)
		assert.Contains(t, string(encoded), "public-A")
		if role == common.RoleCommonUser {
			assert.Nil(t, view.Data)
			assert.Empty(t, view.Platform)
			assert.Zero(t, view.ChannelId)
			assert.Nil(t, view.AdminInfo)
			assert.NotContains(t, string(encoded), "private-provider")
			assert.NotContains(t, string(encoded), "private-E")
			assert.NotContains(t, string(encoded), "private-ledger")
			assert.NotContains(t, string(encoded), "long-video")
		} else {
			require.NotNil(t, view.AdminInfo)
			assert.Contains(t, view.FailReason, "private-E")
			assert.Equal(t, "private-provider", view.Platform)
			assert.Equal(t, 7, view.ChannelId)
			assert.Equal(t, task.PrivateData.VideoRouting, view.AdminInfo.VideoRouting)
			assert.JSONEq(t, string(task.Data), string(view.Data))
		}
	}
	assert.Equal(t, "private-E", task.Properties.UpstreamModelName, "DTO projection must not modify persisted execution identity")
	assert.EqualValues(t, "private-provider", task.Platform)
	assert.Equal(t, 7, task.ChannelId)
	assert.NotEmpty(t, task.Data)
}

func TestVideoRoutingPublicTaskViewsOmitPrivateDiagnostics(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	db := setupVideoRetryTestDatabase(t, user, token)
	require.NoError(t, db.AutoMigrate(&model.Task{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.Channel{}, &model.CanvasGrant{}))
	previousRegistry := pluginruntime.DefaultRegistry
	pluginruntime.DefaultRegistry = pluginruntime.NewRegistry()
	t.Cleanup(func() { pluginruntime.DefaultRegistry = previousRegistry })
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	_, err = pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: "sora"})
	require.NoError(t, err)
	channel := model.Channel{Id: 49, Name: "private-channel", Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(&channel).Error)
	router := gin.New()
	router.GET("/v1/tasks/:key", middleware.TokenAuth(), GetTask)
	router.GET("/v1/videos/:task_id", middleware.TokenAuth(), func(c *gin.Context) {
		require.Nil(t, relay.RelayTaskFetch(c, relayconstant.RelayModeVideoFetchByID))
	})
	router.GET("/v1/media/:task_id", middleware.TokenAuth(), QueryAsyncMedia)
	router.GET("/api/media/:task_id", func(c *gin.Context) {
		c.Set("id", user.Id)
		GetMediaTask(c, false)
	})
	router.GET("/api/admin/media/:task_id", func(c *gin.Context) { GetMediaTask(c, true) })
	now := time.Now().Unix()
	for _, state := range []struct {
		name, jobStatus    string
		status             model.TaskStatus
		purgedBeforeSubmit bool
	}{
		{name: "completed", jobStatus: "submitted", status: model.TaskStatusSuccess},
		{name: "failed", jobStatus: "failed", status: model.TaskStatusFailure},
		{name: "reconciling", jobStatus: "reconciling", status: model.TaskStatusInProgress},
		{name: "queued-after-rejection", jobStatus: "queued", status: model.TaskStatusQueued},
		{name: "purged-before-submit", jobStatus: "failed", purgedBeforeSubmit: true},
	} {
		t.Run(state.name, func(t *testing.T) {
			task := model.Task{
				TaskID: "conditional-public-" + state.name, UserId: user.Id, ChannelId: channel.Id, Platform: "sora",
				Status: state.status, Progress: "40%", CreatedAt: 10, FinishTime: 20,
				Properties: model.Properties{OriginModelName: "public-A", UpstreamModelName: "private-E"},
				PrivateData: model.TaskPrivateData{AsyncMedia: true, TokenId: token.Id, VideoRouting: &dto.VideoRoutingAudit{
					RuleID: "private-rule", SourceModel: "public-A", ExecutionModel: "private-E", SourceResolution: "480p", ExecutionResolution: "720p",
				}},
				Data: []byte(`{"model":"private-E","platform":"private-provider","provider":"private-provider","upstream_model":"private-E","metadata":{"routing":"private-rule"},"provider_payload":{"channel_id":49},"url":"https://cdn.example/video.mp4","seconds":16,"size":"1280x720","expires_at":2000,"error":{"code":"private-E","message":"private-E unsupported parameter","details":{"provider":"private-provider"}}}`),
			}
			if state.status == model.TaskStatusFailure {
				task.FailReason = "private-E unsupported parameter from private-provider"
			}
			if state.name == "reconciling" {
				task.PrivateData.VideoReconciliation = &model.VideoTaskReconciliation{StartedAt: now, DeadlineAt: now + 100, NextPollAt: now + 10, Revision: 2, Reason: "private-E unknown on private-provider"}
			}
			if !state.purgedBeforeSubmit {
				require.NoError(t, db.Create(&task).Error)
			}
			job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: task.TaskID, UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Provider: "sora", Model: "public-A", Status: state.jobStatus, StorageStatus: "pending", BillingStatus: "reserved", ExpiresAt: now + 3600, CreatedAt: now}
			if state.name == "queued-after-rejection" || state.purgedBeforeSubmit {
				job.ErrorMessage = "private-E unsupported parameter from private-provider"
			}
			require.NoError(t, db.Create(&job).Error)
			for _, prefix := range []string{"/v1/tasks/", "/v1/videos/", "/v1/media/", "/api/media/", "/api/admin/media/"} {
				if state.purgedBeforeSubmit && (prefix == "/v1/tasks/" || prefix == "/v1/videos/") {
					continue
				}
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, prefix+task.TaskID, nil)
				request.Header.Set("Authorization", "Bearer sk-"+token.Key)
				router.ServeHTTP(recorder, request)
				require.Equal(t, http.StatusOK, recorder.Code, "%s: %s", prefix, recorder.Body.String())
				var response map[string]any
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				if prefix == "/api/admin/media/" {
					assert.Contains(t, recorder.Body.String(), "private-E")
					if state.status == model.TaskStatusFailure {
						assert.Contains(t, recorder.Body.String(), "private-provider")
					}
					continue
				}
				assert.NotContains(t, recorder.Body.String(), "private-E", prefix)
				assert.NotContains(t, recorder.Body.String(), "private-provider", prefix)
				assert.NotContains(t, recorder.Body.String(), "private-rule", prefix)
				if prefix == "/v1/videos/" {
					assert.Equal(t, "public-A", response["model"])
					assert.EqualValues(t, 16, response["seconds"])
					assert.Equal(t, "1280x720", response["size"])
					assert.NotContains(t, response, "metadata")
					assert.NotContains(t, response, "provider_payload")
					if state.status == model.TaskStatusSuccess {
						assert.Equal(t, "https://cdn.example/video.mp4", response["url"])
						assert.Equal(t, "completed", response["status"])
						assert.NotContains(t, response, "error")
					} else if state.status == model.TaskStatusFailure {
						failure, ok := response["error"].(map[string]any)
						require.True(t, ok)
						assert.Equal(t, "video_generation_failed", failure["code"])
						assert.Contains(t, failure["message"], "视频生成参数不符合上游要求")
						assert.NotContains(t, failure, "details")
					}
				}
			}
			if !state.purgedBeforeSubmit {
				var stored model.Task
				require.NoError(t, db.First(&stored, task.ID).Error)
				assert.Equal(t, task.FailReason, stored.FailReason)
				assert.JSONEq(t, string(task.Data), string(stored.Data))
				assert.Equal(t, task.PrivateData.VideoReconciliation, stored.PrivateData.VideoReconciliation)
			}
		})
	}
}

func TestTaskLogDTOSeparatesUserAdminAndRootDetails(t *testing.T) {
	task := &model.Task{
		TaskID:   "task_public",
		Platform: "document-parser",
		PrivateData: model.TaskPrivateData{
			Key:            "channel-secret-canary",
			UpstreamTaskID: "upstream-private",
			NodeName:       "node-a",
			Execution: &model.TaskExecutionSnapshot{
				RequestID:   "request-public",
				RequestPath: "/v1/documents",
				TaskPlugin: &model.TaskPluginSnapshot{
					Key:     "document-parser",
					Name:    "Document Parser",
					Version: "1.2.3",
					Author: &model.TaskPluginAuthorSnapshot{
						Name: "Community Author",
						URL:  "https://plugins.example/author",
					},
					APIVersion: 1,
					Generation: 42,
				},
			},
		},
	}

	userView := tasksToDto([]*model.Task{task}, false, common.RoleCommonUser)[0]
	assert.Nil(t, userView.AdminInfo)
	assert.Nil(t, userView.RootInfo)

	adminView := tasksToDto([]*model.Task{task}, false, common.RoleAdminUser)[0]
	require.NotNil(t, adminView.AdminInfo)
	require.NotNil(t, adminView.AdminInfo.TaskPlugin)
	assert.Equal(t, "document-parser", adminView.AdminInfo.TaskPlugin.Key)
	assert.Equal(t, "Document Parser", adminView.AdminInfo.TaskPlugin.Name)
	assert.Equal(t, "1.2.3", adminView.AdminInfo.TaskPlugin.Version)
	require.NotNil(t, adminView.AdminInfo.TaskPlugin.Author)
	assert.Equal(t, "Community Author", adminView.AdminInfo.TaskPlugin.Author.Name)
	assert.Equal(t, "https://plugins.example/author", adminView.AdminInfo.TaskPlugin.Author.URL)
	assert.Equal(t, "request-public", adminView.AdminInfo.RequestID)
	assert.Equal(t, "/v1/documents", adminView.AdminInfo.RequestPath)
	assert.Nil(t, adminView.RootInfo)

	rootView := tasksToDto([]*model.Task{task}, false, common.RoleRootUser)[0]
	require.NotNil(t, rootView.AdminInfo)
	require.NotNil(t, rootView.RootInfo)
	require.NotNil(t, rootView.RootInfo.TaskPlugin)
	assert.Equal(t, 1, rootView.RootInfo.TaskPlugin.APIVersion)
	assert.Equal(t, uint64(42), rootView.RootInfo.TaskPlugin.Generation)
	assert.Equal(t, "upstream-private", rootView.RootInfo.UpstreamTaskID)
	assert.Equal(t, "node-a", rootView.RootInfo.NodeName)

	adminJSON, err := common.Marshal(adminView)
	require.NoError(t, err)
	assert.NotContains(t, string(adminJSON), "channel-secret-canary")
	assert.NotContains(t, string(adminJSON), "upstream-private")

	rootJSON, err := common.Marshal(rootView)
	require.NoError(t, err)
	assert.NotContains(t, string(rootJSON), "channel-secret-canary")
	assert.Contains(t, string(rootJSON), "upstream-private")
}

func TestTaskLogDTODoesNotInventHistoricalPluginProvenance(t *testing.T) {
	task := &model.Task{
		TaskID:   "task_without_snapshot",
		Platform: "document-parser",
	}

	adminView := tasksToDto([]*model.Task{task}, false, common.RoleAdminUser)[0]

	assert.Nil(t, adminView.AdminInfo)
	assert.Nil(t, adminView.RootInfo)
}

func TestTaskLogDTOReplacesLegacyVideoURLWithAvailabilityFlag(t *testing.T) {
	task := &model.Task{
		TaskID:     "task_legacy_video",
		Platform:   "jimeng",
		Action:     constant.TaskActionTextToVideo,
		Status:     model.TaskStatusSuccess,
		FailReason: "https://private-upstream.invalid/video.mp4?signature=secret",
	}

	view := tasksToDto([]*model.Task{task}, false, common.RoleCommonUser)[0]
	assert.True(t, view.LegacyVideoAvailable)
	assert.Empty(t, view.ResultURL)
	assert.Empty(t, view.FailReason)
	encoded, err := common.Marshal(view)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "private-upstream.invalid")
	assert.NotContains(t, string(encoded), "result_url")
	assert.Contains(t, string(encoded), "legacy_video_available")
}

func TestTaskLogDTOKeepsFailureReasonAndDoesNotMarkPluginTaskLegacy(t *testing.T) {
	failed := &model.Task{
		TaskID:     "task_failed",
		Platform:   "jimeng",
		Action:     constant.TaskActionTextToVideo,
		Status:     model.TaskStatusFailure,
		FailReason: "provider rejected the request",
	}
	failedView := tasksToDto([]*model.Task{failed}, false, common.RoleCommonUser)[0]
	assert.Contains(t, failedView.FailReason, "视频任务失败")
	assert.Contains(t, failedView.FailReason, "原始错误：provider rejected the request")
	assert.False(t, failedView.LegacyVideoAvailable)

	pluginTask := &model.Task{
		TaskID:     "task_plugin_video",
		Platform:   "community-video",
		Action:     constant.TaskActionTextToVideo,
		Status:     model.TaskStatusSuccess,
		FailReason: "https://stale-upstream.invalid/plugin-video.mp4",
		PrivateData: model.TaskPrivateData{
			ResultURL: "https://private-upstream.invalid/plugin-video.mp4",
			Execution: &model.TaskExecutionSnapshot{
				TaskPlugin: &model.TaskPluginSnapshot{Key: "community-video"},
			},
		},
	}
	pluginView := tasksToDto([]*model.Task{pluginTask}, false, common.RoleCommonUser)[0]
	assert.False(t, pluginView.LegacyVideoAvailable)
	assert.Empty(t, pluginView.ResultURL)
	assert.Empty(t, pluginView.FailReason)
}
