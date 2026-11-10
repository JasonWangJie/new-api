package controller

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

func TestMixedVideoCustomRetryPrecedesUnknownProtection(t *testing.T) {
	for _, tc := range []struct {
		name, action, want, blocked         string
		unmatched, paused, output, accepted bool
		used, limit                         int
		now                                 int64
	}{
		{name: "unmatched", unmatched: true, want: mediaretry.ManualReview, blocked: "mixed_video_unknown_not_replayable", limit: 2},
		{name: "same channel", action: mediaretry.SameChannel, want: mediaretry.SameChannel, limit: 2},
		{name: "other channel", action: mediaretry.OtherChannel, want: mediaretry.OtherChannel, limit: 2},
		{name: "accepted but unfinished", action: mediaretry.SameChannel, want: mediaretry.SameChannel, accepted: true, limit: 2},
		{name: "stop", action: mediaretry.Stop, want: mediaretry.Stop, limit: 2},
		{name: "manual review", action: mediaretry.ManualReview, want: mediaretry.ManualReview, limit: 2},
		{name: "pause", action: mediaretry.SameChannel, want: mediaretry.ManualReview, blocked: "resubmissions_paused", paused: true, limit: 2},
		{name: "total budget", action: mediaretry.SameChannel, want: mediaretry.ManualReview, blocked: "total_retry_budget_exhausted", limit: 0},
		{name: "deadline", action: mediaretry.SameChannel, want: mediaretry.ManualReview, blocked: "execution_deadline_reached", now: 2000, limit: 2},
		{name: "rule exhausted", action: mediaretry.SameChannel, want: mediaretry.ManualReview, used: 1, limit: 2},
		{name: "existing output", action: mediaretry.SameChannel, want: mediaretry.ResumeDownload, blocked: "output_already_available", output: true, limit: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := mediaretry.Policy{Revision: 8, PauseResubmissions: tc.paused}
			if !tc.unmatched {
				policy.Rules = []mediaretry.Rule{{ID: "upstream", Name: "Upstream failure", Enabled: true, Match: mediaretry.Match{ContainsAny: []string{"upstream task submission returned"}}, Steps: []mediaretry.Step{{ID: "action", Action: tc.action, MaxAttempts: 1}}}}
			}
			fact := mediaretry.Fact{MediaType: "video", Provider: "drama", Stage: "submit", Message: "upstream task submission returned 500: i/o timeout", Unknown: !tc.accepted, Accepted: tc.accepted, HasOutput: tc.output, Deadline: 2000}
			state := mediaretry.State{Now: tc.now, TotalLimit: tc.limit, RuleUses: map[string]int{"upstream": tc.used}, StepUses: map[string]int{"upstream/action": tc.used}}
			decision, err := service.PlanInitialVideoFailure(policy, fact, state, true)
			require.NoError(t, err)
			assert.Equal(t, !tc.unmatched, decision.Matched)
			assert.EqualValues(t, 8, decision.Revision)
			assert.Equal(t, tc.want, decision.Action)
			assert.Equal(t, tc.blocked, decision.BlockedReason)
		})
	}
}

func TestMixedVideoAdmissionAndCrossPluginRecovery(t *testing.T) {
	previousTaskTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 10
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTaskTimeout })
	user, token := setupResponsesWSRequestTest(t)
	db := setupVideoRetryTestDatabase(t, user, token)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Task{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.MediaRetryEvent{}, &model.Option{}))
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "mixed-test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "mixed-test")
	previousRegistry := pluginruntime.DefaultRegistry
	pluginruntime.DefaultRegistry = pluginruntime.NewRegistry()
	t.Cleanup(func() { pluginruntime.DefaultRegistry = previousRegistry })
	const publicModel = "shared-public-video"
	plugins := make([]*pluginruntime.LoadedPlugin, 0, 2)
	channels := make([]model.Channel, 0, 2)
	for index, key := range []string{"mixed-first", "mixed-second"} {
		target := fmt.Sprintf("vendor-model-%d", index)
		source := fmt.Sprintf(`
export const meta = {apiVersion:1,key:%q,name:%q,version:"1.0.0",author:{name:"Test"},models:[%q],fetchMode:"per_task",protocols:["openai_video"],videoProfiles:[{models:[%q],modes:[{name:"text_to_video",duration:{values:[5,10],default:%d},resolutions:["720p"],defaultResolution:"720p"},{name:"image_to_video",duration:{values:[5,10],default:%d},resolutions:["720p"],defaultResolution:"720p",inputs:[{name:"image",kind:"image",sources:["upload"],maxItems:1,required:true}]}]}]};
export const protocols={openai_video:{decodeRequest(ctx){if(ctx.upstreamModel!==%q)throw new Error("incorrect member mapping");let body=ctx.body.value;if(ctx.body.kind==="multipart"){body={};for(const key in ctx.body.fields)body[key]=ctx.body.fields[key][0];}if(body.prompt==="change-specs")body.seconds=10;return {kind:"submit",model:ctx.model,action:ctx.body.files&&ctx.body.files.length?"image_to_video":"text_to_video",requestBody:body};},render(ctx,task){return task;}}};
export function buildSubmitRequest(ctx){return {method:"POST",url:ctx.baseUrl+%q,body:ctx.requestBody};}export function parseSubmitResponse(ctx,resp){if(resp.statusCode>=400)throw new Error("receipt is unknown");return {taskId:"accepted"};}export function buildQueryRequest(){return {method:"GET",url:"https://example.invalid/query"};}export function parseTaskResult(){return {status:"SUCCESS"};}export function listArtifacts(){return [];}export function buildContentRequest(){return {method:"GET",url:"https://example.invalid/content"};}
`, key, key, target, target, 5+index*5, 5+index*5, target, fmt.Sprintf("/vendor-%d/generate", index))
		plugin, err := pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: key})
		require.NoError(t, err)
		plugins = append(plugins, plugin)
		setting := fmt.Sprintf(`{"task_plugin_key":%q}`, key)
		mapping := fmt.Sprintf(`{%q:%q}`, publicModel, target)
		channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: key, Status: common.ChannelStatusEnabled, Models: publicModel, Group: "default", Key: key + "-key", Setting: &setting, ModelMapping: &mapping}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Group: "default", Model: publicModel, Enabled: true}).Error)
		channels = append(channels, channel)
	}
	policy := model.ImageGroupPolicy{Group: "default", Platform: "mixed", PolicyKey: service.ImageIdentityHash("default", "mixed"), Enabled: true, AsyncEnabled: true, PoolMode: "model_resolution", Models: `[{"id":"shared-public-video","media_type":"video"}]`}
	require.NoError(t, db.Create(&policy).Error)
	bindingKey := service.MediaPoolBindingKey(policy, publicModel, "1K", "video")
	for index := range channels {
		require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: bindingKey, Group: "default", Platform: "mixed", Mode: policy.PoolMode, Model: publicModel, Resolution: "1K", ChannelId: channels[index].Id, Priority: index}).Error)
	}
	engine := gin.New()
	var observedFacts map[string]any
	engine.POST("/v1/videos/generations_async", func(c *gin.Context) { c.Set("token_id", token.Id); c.Set("id", user.Id); c.Next() }, PrepareAsyncVideo, PinAsyncVideoEndpoint(), FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), RouteAsyncVideo, func(c *gin.Context) {
		pinned := c.MustGet(pluginruntime.ContextKeyPinnedEndpoint).(pluginruntime.PinnedEndpoint)
		observedFacts = c.MustGet("mixed_video_billing_facts").(map[string]any)
		c.JSON(200, gin.H{"model": pinned.Model, "mapped": pinned.MappedModel, "plugin": pinned.Plugin.Meta.Key, "mixed": c.GetBool("mixed_video_billing")})
	})
	for _, multipartBody := range []bool{false, true} {
		var raw bytes.Buffer
		contentType := "application/json"
		if multipartBody {
			writer := multipart.NewWriter(&raw)
			require.NoError(t, writer.WriteField("model", publicModel))
			require.NoError(t, writer.WriteField("seconds", "5"))
			require.NoError(t, writer.WriteField("resolution", "720p"))
			file, err := writer.CreateFormFile("input_reference", "reference.png")
			require.NoError(t, err)
			_, err = file.Write([]byte("bounded-upload-fixture"))
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			contentType = writer.FormDataContentType()
		} else {
			raw.WriteString(`{"model":"shared-public-video","seconds":5,"resolution":"720p","prompt":"test"}`)
		}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", &raw)
		request.Header.Set("Content-Type", contentType)
		engine.ServeHTTP(recorder, request)
		require.Equal(t, 200, recorder.Code, recorder.Body.String())
		assert.JSONEq(t, `{"model":"shared-public-video","mapped":"vendor-model-1","plugin":"mixed-second","mixed":true}`, recorder.Body.String())
	}
	defaults := httptest.NewRecorder()
	defaultRequest := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", strings.NewReader(`{"model":"shared-public-video","prompt":"public defaults"}`))
	defaultRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(defaults, defaultRequest)
	require.Equal(t, 200, defaults.Code, defaults.Body.String())
	assert.Equal(t, float64(5), observedFacts["seconds"], "one complete stable variant determines the public default before request filtering and weighting")
	assert.Equal(t, "720p", observedFacts["resolution"])
	altered := httptest.NewRecorder()
	alteredRequest := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", strings.NewReader(`{"model":"shared-public-video","seconds":5,"resolution":"720p","prompt":"change-specs"}`))
	alteredRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(altered, alteredRequest)
	assert.Equal(t, 400, altered.Code, "a decoder cannot change five billed seconds to ten even when both durations are supported")
	assert.Contains(t, altered.Body.String(), "changed the public seconds")
	require.NoError(t, db.Model(&policy).Update("enabled", false).Error)
	recorder := httptest.NewRecorder()
	requestHTTP := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", strings.NewReader(`{"model":"shared-public-video","seconds":5,"resolution":"720p"}`))
	requestHTTP.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, requestHTTP)
	assert.Equal(t, 403, recorder.Code, "a disabled mixed policy cannot fall back")
	require.NoError(t, db.Model(&policy).Update("enabled", true).Error)

	now := time.Now().Unix()
	route := &service.MixedVideoRouting{Group: "default", BindingKey: bindingKey, PoolMode: policy.PoolMode, Model: publicModel, Operation: "create", Resolution: "1K"}
	decision := mediaretry.Decision{Matched: true, Action: mediaretry.OtherChannel, RequestedAction: mediaretry.OtherChannel, RuleID: "mixed-failure", StepID: "other", Revision: 1}
	fact := mediaretry.Fact{MediaType: "video", Provider: plugins[0].Meta.Key, Model: publicModel, ChannelID: channels[0].Id, Stage: "terminal", Accepted: true, Terminal: true, HasUpstreamTask: true, Message: "confirmed failure"}
	frozen := service.AsyncVideoRequest{MixedRouting: route, Body: []byte(`{"model":"shared-public-video","seconds":5,"resolution":"720p","prompt":"test"}`), ContentType: "application/json", Operation: "create", ClientIP: "203.0.113.10", Channel: channels[0], PluginHash: plugins[0].Engine.SourceFingerprint(), Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: plugins[0].Meta.Key, Version: "1.0.0"}}, PendingDecision: &decision, RetryFact: &fact, ReferenceRecoveryVersion: 2, ReferenceRecoveryDeadline: now + 120}
	task := model.Task{TaskID: model.GenerateTaskID(), UserId: user.Id, ChannelId: channels[0].Id, Platform: constant.TaskPlatform(plugins[0].Meta.Key), Status: model.TaskStatusSubmitted, Quota: 7, Properties: model.Properties{OriginModelName: publicModel, UpstreamModelName: "vendor-model-0"}}
	task.PrivateData.AsyncMedia, task.PrivateData.DurableBilling, task.PrivateData.SubmitAccountingRecorded = true, true, true
	task.PrivateData.TokenId, task.PrivateData.UpstreamTaskID, task.PrivateData.Execution = token.Id, "first-upstream-operation", frozen.Execution
	task.PrivateData.CurrentAttempt = &model.VideoTaskAttempt{MixedVideo: true, ChannelID: channels[0].Id, PluginKey: plugins[0].Meta.Key, PluginVersion: "1.0.0", UpstreamModel: "vendor-model-0"}
	require.NoError(t, db.Create(&task).Error)
	encoded, err := common.Marshal(frozen)
	require.NoError(t, err)
	cipher, err := service.EncryptImagePayload(encoded, "media-request:"+task.TaskID)
	require.NoError(t, err)
	job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: common.GetUUID(), UserId: user.Id, TokenId: token.Id, ChannelId: channels[0].Id, Provider: plugins[0].Meta.Key, Model: publicModel, Group: "default", Status: "submitted", BillingStatus: "reserved", RequestCipher: cipher, CreatedAt: now, ExpiresAt: now + 600, Quota: 7}
	require.NoError(t, db.Create(&job).Error)
	stale := task
	staleSnapshot := stale.Snapshot()
	won, err := model.ScheduleAsyncVideoReferenceRecovery(t.Context(), task, job, fact.Message, false, model.AsyncVideoRecoveryPlan{Decision: decision, Fact: fact, State: mediaretry.State{TotalLimit: 2}, RequestCipher: cipher})
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, db.First(&task, task.ID).Error)
	require.NoError(t, db.First(&job, job.ID).Error)
	job.LeaseToken, job.LeaseExpiresAt = "mixed-recovery-lease", now+120
	require.NoError(t, db.Model(&job).Updates(map[string]any{"lease_token": job.LeaseToken, "lease_expires_at": job.LeaseExpiresAt}).Error)
	require.NoError(t, service.SelectVideoRecoveryChannel(t.Context(), &job, &task, &frozen))
	assert.Equal(t, plugins[1].Meta.Key, job.Provider)
	assert.Equal(t, constant.TaskPlatform(plugins[1].Meta.Key), task.Platform)
	assert.Equal(t, "vendor-model-1", task.Properties.UpstreamModelName)
	assert.Equal(t, plugins[0].Meta.Key, task.PrivateData.Execution.TaskPlugin.Key)
	assert.Equal(t, plugins[1].Meta.Key, task.PrivateData.CurrentAttempt.PluginKey)
	assert.Equal(t, channels[0].Id, task.BillingChannelID())
	assert.Equal(t, 7, task.Quota)
	stale.Status = model.TaskStatusInProgress
	won, err = stale.UpdatePollingState(t.Context(), staleSnapshot)
	require.NoError(t, err)
	assert.False(t, won, "old provider polling cannot overwrite a replacement generation")
	unknown := *frozen.RetryFact
	unknown.Unknown, unknown.Terminal = true, false
	frozen.RetryFact = &unknown
	candidates, err := service.VideoRetryCandidateChannels(t.Context(), job, frozen, task.Properties.UpstreamModelName)
	require.NoError(t, err)
	require.Len(t, candidates, 1, "a matched rule can choose another plugin for an unknown execution")
	assert.Equal(t, channels[1].Id, candidates[0].Id)
	unmatched := decision
	unmatched.Matched = false
	frozen.PendingDecision = &unmatched
	_, err = service.VideoRetryCandidateChannels(t.Context(), job, frozen, task.Properties.UpstreamModelName)
	require.ErrorContains(t, err, "unknown mixed video")
	frozen.PendingDecision = &decision
	// An unknown dispatched replacement retains its original hold and encrypted
	// evidence instead of pretending the upstream failed or permitting cleanup.
	task.Status = model.TaskStatusUnknown
	task.PrivateData.VideoAttempts = append(task.PrivateData.VideoAttempts, model.VideoTaskAttempt{Status: model.TaskStatusUnknown, MixedVideo: true})
	require.NoError(t, db.Model(&task).Updates(map[string]any{"status": task.Status, "private_data": task.PrivateData}).Error)
	job.DispatchedAt = now
	require.NoError(t, db.Model(&job).Updates(map[string]any{"dispatched_at": now}).Error)
	require.NoError(t, model.EndAsyncVideoPolicyExecution(t.Context(), task, job, unknown, nil, mediaretry.State{}))
	require.NoError(t, db.First(&task, task.ID).Error)
	require.NoError(t, db.First(&job, job.ID).Error)
	assert.Equal(t, model.TaskStatusUnknown, string(task.Status))
	assert.Equal(t, "execution_unknown", job.Status)
	assert.Equal(t, "unknown", job.BillingStatus)
	assert.Equal(t, 7, task.Quota)
	assert.NotEmpty(t, job.RequestCipher)
	assert.NotEmpty(t, job.ChannelCipher)
	assert.True(t, job.HasPendingSubmissionReconciliation())
	require.ErrorIs(t, model.ExpireAsyncMediaJob(t.Context(), job, now+10000), model.ErrImageConflict)

	// Exercise the actual first-submission refund boundary on a plugin outside
	// the legacy reconciliation whitelist, including an explicit free price.
	require.NoError(t, db.AutoMigrate(&model.UserSubscription{}, &model.AsyncImageUsageProjection{}, &model.AsyncImageLogReceipt{}, &model.CanvasGrant{}, &model.Log{}, &model.Option{}, &model.MediaRetryPolicyVersion{}, &model.MediaArtifactObject{}, &model.MediaInputObject{}))
	previousSubmit := service.SubmitAsyncVideoFunc
	previousBatch, previousLogs := common.BatchUpdateEnabled, common.LogConsumeEnabled
	service.SubmitAsyncVideoFunc, common.BatchUpdateEnabled, common.LogConsumeEnabled = ExecuteAsyncVideo, false, false
	t.Cleanup(func() {
		service.SubmitAsyncVideoFunc, common.BatchUpdateEnabled, common.LogConsumeEnabled = previousSubmit, previousBatch, previousLogs
	})
	for _, tc := range []struct {
		name          string
		reserved      int
		accepted      bool
		refundFailure bool
		action        string
		paused        bool
		retryAccepted bool
	}{
		{name: "unmatched retains hold", reserved: 100},
		{name: "unmatched free request"},
		{name: "accepted without retry", accepted: true},
		{name: "refund retries", reserved: 100, refundFailure: true},
		{name: "custom same channel", reserved: 100, action: mediaretry.SameChannel, retryAccepted: true},
		{name: "custom free same channel", action: mediaretry.SameChannel, retryAccepted: true},
		{name: "custom exhausted", reserved: 100, action: mediaretry.SameChannel},
		{name: "custom paused", reserved: 100, action: mediaretry.SameChannel, paused: true},
		{name: "custom manual review", reserved: 100, action: mediaretry.ManualReview},
		{name: "custom stop", reserved: 100, action: mediaretry.Stop},
	} {
		t.Run("first_submit_"+tc.name, func(t *testing.T) {
			reserved := tc.reserved
			retryPolicy, err := model.GetMediaRetryPolicy(t.Context())
			require.NoError(t, err)
			retryPolicy.PauseResubmissions, retryPolicy.Rules = tc.paused, nil
			if tc.action != "" {
				retryPolicy.Rules = []mediaretry.Rule{{ID: "submission", Name: "Submission failure", Enabled: true, Scope: mediaretry.Scope{MediaTypes: []string{"video"}}, Match: mediaretry.Match{ContainsAny: []string{"upstream task submission returned"}}, Steps: []mediaretry.Step{{ID: "configured", Action: tc.action, MaxAttempts: 1}}}}
			}
			_, err = model.SaveMediaRetryPolicy(t.Context(), retryPolicy, retryPolicy.Revision, user.Id, nil)
			require.NoError(t, err)
			require.NoError(t, db.Model(token).Updates(map[string]any{"status": common.TokenStatusEnabled, "remain_quota": 100}).Error)
			require.NoError(t, db.First(user, user.Id).Error)
			require.NoError(t, db.First(token, token.Id).Error)
			beforeWallet, beforeToken := user.Quota, token.RemainQuota
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if tc.accepted || tc.retryAccepted && call > 1 {
					_, _ = w.Write([]byte(`{"id":"accepted"}`))
					return
				}
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream response was lost"}}`))
			}))
			t.Cleanup(upstream.Close)
			channel := channels[0]
			channel.BaseURL = &upstream.URL
			require.NoError(t, db.Model(&channel).Update("base_url", upstream.URL).Error)
			expression := fmt.Sprintf(`tier("public", u("requests") * %.12f)`, float64(reserved)/common.QuotaPerUnit)
			snapshot := &billingexpr.BillingSnapshot{ModelName: publicModel, BillingMode: "tiered_expr", ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), ExprVersion: 1, GroupRatio: 1, QuotaPerUnit: common.QuotaPerUnit, EstimatedQuotaBeforeGroup: float64(reserved), EstimatedQuotaAfterGroup: reserved, TaskUsageBilling: true, MixedVideoBilling: true, MixedVideoSchemaVersion: 1, UsageFacts: map[string]any{"requests": float64(1), "action": "generate", "seconds": float64(5), "resolution": "720p"}}
			initial := service.AsyncVideoRequest{MixedRouting: route, Body: []byte(`{"model":"shared-public-video","seconds":5,"resolution":"720p","prompt":"unknown submit"}`), ContentType: "application/json", Operation: "create", ClientIP: "203.0.113.10", Channel: channel, PluginHash: plugins[0].Engine.SourceFingerprint(), Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: plugins[0].Meta.Key, Version: "1.0.0"}}, Billing: service.MediaBillingSnapshot{Tiered: snapshot, Price: types.PriceData{Quota: reserved, QuotaToPreConsume: reserved, FreeModel: reserved == 0, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}}
			require.NoError(t, service.FreezeAsyncVideoReferenceRecovery(&initial, nil, now+600))
			require.Equal(t, 2, initial.ReferenceRecoveryVersion)
			initialJob := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Provider: plugins[0].Meta.Key, Model: publicModel, Group: "default", Status: "queued", BillingStatus: "pending", StorageStatus: "pending", LeaseToken: fmt.Sprintf("first-mixed-%d", reserved), LeaseExpiresAt: now + 120, CreatedAt: now, ExpiresAt: now + 600}
			encoded, err := common.Marshal(initial)
			require.NoError(t, err)
			initialJob.RequestCipher, err = service.EncryptImagePayload(encoded, "media-request:"+initialJob.TaskId)
			require.NoError(t, err)
			channelData, err := common.Marshal(channel)
			require.NoError(t, err)
			initialJob.ChannelCipher, err = service.EncryptImagePayload(channelData, "media-channel:"+initialJob.TaskId)
			require.NoError(t, err)
			require.NoError(t, db.Create(&initialJob).Error)
			processErr := service.ProcessAsyncMediaJob(t.Context(), initialJob, service.DefaultMediaRuntimeConfig())
			require.NoError(t, db.First(&initialJob, initialJob.ID).Error)
			require.NoError(t, db.First(user, user.Id).Error)
			require.NoError(t, db.First(token, token.Id).Error)
			expectedCalls := int32(1)
			if tc.action == mediaretry.SameChannel && !tc.paused {
				expectedCalls++
			}
			assert.Equal(t, expectedCalls, calls.Load(), "only an executable matched rule grants a resubmission")
			assert.Equal(t, beforeWallet-reserved, user.Quota, "the original hold is not refunded")
			assert.Equal(t, beforeToken-reserved, token.RemainQuota)
			if tc.accepted || tc.retryAccepted {
				require.NoError(t, processErr)
				require.Equal(t, "submitted", initialJob.Status)
				plain, err := service.DecryptImagePayload(initialJob.RequestCipher, "media-request:"+initialJob.TaskId)
				require.NoError(t, err)
				var retained service.AsyncVideoRequest
				require.NoError(t, common.Unmarshal(plain, &retained))
				assert.Equal(t, 2, retained.ReferenceRecoveryVersion)
				require.NotNil(t, retained.MixedRouting)
				assert.Equal(t, route.BindingKey, retained.MixedRouting.BindingKey)
				assert.Equal(t, initial.Body, retained.Body, "successful admission must retain the complete v2 replay body until terminal execution")
				assert.Equal(t, initial.PluginHash, retained.PluginHash)
				if tc.retryAccepted {
					require.Len(t, retained.InitialUnknownAttempts, 1)
					assert.Equal(t, plugins[0].Meta.Key, retained.InitialUnknownAttempts[0].PluginKey)
					assert.True(t, retained.InitialUnknownAttempts[0].MixedVideo)
					require.NotNil(t, retained.InitialRetryState)
					assert.Equal(t, 1, retained.InitialRetryState.TotalUsed)
				}
				return
			}
			assert.Equal(t, "execution_unknown", initialJob.Status)
			assert.Equal(t, "unknown", initialJob.BillingStatus)
			assert.NotEmpty(t, initialJob.RequestCipher)
			assert.NotEmpty(t, initialJob.ChannelCipher)
			var native model.Task
			require.NoError(t, db.Where("task_id = ?", initialJob.TaskId).Take(&native).Error)
			assert.Equal(t, model.TaskStatusUnknown, string(native.Status))
			assert.Equal(t, reserved, native.Quota)
			assert.True(t, native.PrivateData.DurableBilling)
			assert.True(t, native.PrivateData.SubmitAccountingRecorded)
			require.NotNil(t, native.PrivateData.CurrentAttempt)
			assert.True(t, native.PrivateData.CurrentAttempt.MixedVideo)
			require.NotNil(t, native.PrivateData.VideoReconciliation)
			assert.True(t, native.PrivateData.VideoReconciliation.ManualRequired)
			require.NotNil(t, native.PrivateData.MediaRetryDecision)
			assert.Equal(t, tc.action != "", native.PrivateData.MediaRetryDecision.Matched)
			if tc.paused {
				assert.Equal(t, "resubmissions_paused", native.PrivateData.MediaRetryDecision.BlockedReason)
			}
			view, _, err := videoJobView(taskSubmissionTestContext(), initialJob, true, false)
			require.NoError(t, err)
			assert.Equal(t, true, view["can_terminate"])
			stale := native
			for _, attempt := range []struct {
				admin bool
				body  string
				code  int
			}{
				{false, `{"confirm_upstream_checked":true}`, http.StatusForbidden},
				{true, `{}`, http.StatusConflict},
				{true, `{"confirm_upstream_checked":true}`, http.StatusOK},
				{true, `{"confirm_upstream_checked":true}`, http.StatusConflict},
			} {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Set("id", user.Id)
				c.Params = gin.Params{{Key: "task_id", Value: initialJob.TaskId}}
				c.Request = httptest.NewRequest(http.MethodPost, "/api/media/tasks/"+initialJob.TaskId+"/terminate", strings.NewReader(attempt.body))
				if tc.refundFailure && attempt.code == http.StatusOK {
					const callback = "mixed-reconciliation-refund-failure"
					require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
						if tx.Statement.Table == "users" {
							tx.AddError(errors.New("temporary refund failure"))
						}
					}))
					ManageMediaTask(c, attempt.admin, "terminate")
					require.NoError(t, db.Callback().Update().Remove(callback))
					require.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
					require.NoError(t, db.First(&initialJob, initialJob.ID).Error)
					assert.Equal(t, "age_limit_refund", initialJob.ReferenceRecoveryStage)
					require.NoError(t, db.Model(&initialJob).Update("next_attempt_at", 0).Error)
					require.NoError(t, service.FailOverdueAsyncMediaJobs(t.Context(), 100), "a confirmed refund retries without waiting for the task age limit")
					continue
				}
				ManageMediaTask(c, attempt.admin, "terminate")
				require.Equal(t, attempt.code, recorder.Code, recorder.Body.String())
			}
			require.NoError(t, db.First(&initialJob, initialJob.ID).Error)
			require.NoError(t, db.First(&native, native.ID).Error)
			require.NoError(t, db.First(user, user.Id).Error)
			require.NoError(t, db.First(token, token.Id).Error)
			assert.Equal(t, "failed", initialJob.Status)
			assert.Equal(t, "refunded", initialJob.BillingStatus)
			assert.Zero(t, native.Quota)
			assert.Equal(t, beforeWallet, user.Quota, "explicit reconciliation releases the original hold only once")
			assert.Equal(t, beforeToken, token.RemainQuota)
			assert.Equal(t, expectedCalls, calls.Load())
			stale.Status = model.TaskStatusSuccess
			won, _, err := model.SettleTaskBilling(t.Context(), &stale, model.TaskStatusUnknown, reserved, nil, "")
			require.NoError(t, err)
			assert.False(t, won, "a stale upstream result cannot settle after administrator reconciliation")
		})
	}
	t.Run("configured unknown background recovery", func(t *testing.T) {
		fact := mediaretry.Fact{MediaType: "video", Provider: plugins[0].Meta.Key, Model: publicModel, ChannelID: channels[0].Id, Stage: "submit", Message: "upstream task submission returned 500: i/o timeout", Unknown: true, Deadline: now + 600, VideoEvidence: &mediaretry.VideoFailureEvidence{}}
		policy := mediaretry.Policy{Revision: 1, Rules: []mediaretry.Rule{{ID: "unknown", Name: "Unknown submission", Enabled: true, Match: mediaretry.Match{ContainsAny: []string{"i/o timeout"}}, Steps: []mediaretry.Step{{ID: "same", Action: mediaretry.SameChannel, MaxAttempts: 1}}}}}
		state := mediaretry.State{TotalLimit: 1, Now: now}
		decision, err := service.PlanInitialVideoFailure(policy, fact, state, true)
		require.NoError(t, err)
		require.Equal(t, mediaretry.SameChannel, decision.Action)
		request := service.AsyncVideoRequest{MixedRouting: route, Body: []byte(`{"model":"shared-public-video","seconds":5,"resolution":"720p","prompt":"test"}`), ContentType: "application/json", Operation: "create", Channel: channels[0], PluginHash: plugins[0].Engine.SourceFingerprint(), ReferenceRecoveryVersion: 2, ReferenceRecoveryDeadline: now + 600}
		task := model.Task{TaskID: model.GenerateTaskID(), UserId: user.Id, ChannelId: channels[0].Id, Platform: constant.TaskPlatform(plugins[0].Meta.Key), Status: model.TaskStatusUnknown, Quota: 7, Properties: model.Properties{OriginModelName: publicModel, UpstreamModelName: "vendor-model-0"}}
		task.PrivateData = model.TaskPrivateData{TokenId: token.Id, AsyncMedia: true, DurableBilling: true, SubmitAccountingRecorded: true, CurrentAttempt: &model.VideoTaskAttempt{MixedVideo: true, ChannelID: channels[0].Id, PluginKey: plugins[0].Meta.Key, PluginVersion: "1.0.0", UpstreamModel: "vendor-model-0"}, VideoReconciliation: &model.VideoTaskReconciliation{ManualRequired: true}}
		require.NoError(t, db.Create(&task).Error)
		raw, err := common.Marshal(request)
		require.NoError(t, err)
		cipher, err := service.EncryptImagePayload(raw, "media-request:"+task.TaskID)
		require.NoError(t, err)
		job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: common.GetUUID(), UserId: user.Id, TokenId: token.Id, ChannelId: channels[0].Id, Provider: plugins[0].Meta.Key, Model: publicModel, Group: "default", Status: "execution_unknown", BillingStatus: "unknown", ReferenceRecoveryStage: "unknown", RequestCipher: cipher, CreatedAt: now, ExpiresAt: now + 600, Quota: 7}
		require.NoError(t, db.Create(&job).Error)
		preview, err := service.PreviewInitialVideoFailure(t.Context(), task.TaskID, fact, state, policy)
		require.NoError(t, err)
		assert.Equal(t, decision, preview)
		unmatched, err := service.PreviewInitialVideoFailure(t.Context(), task.TaskID, fact, state, mediaretry.Policy{})
		require.NoError(t, err)
		assert.Equal(t, "mixed_video_unknown_not_replayable", unmatched.BlockedReason)
		eligibility, err := service.PreviewVideoRetryEligibility(t.Context(), task.TaskID, fact, decision)
		require.NoError(t, err)
		assert.True(t, eligibility.Eligible, eligibility.Reason)
		stale, snapshot := task, task.Snapshot()
		applied, err := service.ApplyConfiguredVideoDecision(t.Context(), task, job, fact, &decision, state)
		require.NoError(t, err)
		require.True(t, applied)
		require.NoError(t, db.First(&task, task.ID).Error)
		require.NoError(t, db.First(&job, job.ID).Error)
		assert.EqualValues(t, 1, task.AttemptGeneration)
		assert.Equal(t, model.TaskStatusQueued, string(task.Status))
		assert.Equal(t, "reserved", job.BillingStatus)
		assert.Equal(t, 7, job.Quota)
		require.Len(t, task.PrivateData.VideoAttempts, 1)
		assert.Equal(t, model.TaskStatusUnknown, task.PrivateData.VideoAttempts[0].Status)
		assert.Equal(t, plugins[0].Meta.Key, task.PrivateData.VideoAttempts[0].PluginKey)
		assert.True(t, task.PrivateData.VideoAttempts[0].MixedVideo)
		assert.Zero(t, task.PrivateData.MediaRetryState.TotalUsed, "scheduling has not consumed a send")
		stale.Status = model.TaskStatusInProgress
		won, err := stale.UpdatePollingState(t.Context(), snapshot)
		require.NoError(t, err)
		assert.False(t, won)
		job.LeaseToken, job.LeaseExpiresAt = "unknown-custom-recovery", now+120
		require.NoError(t, db.Model(&job).Updates(map[string]any{"lease_token": job.LeaseToken, "lease_expires_at": job.LeaseExpiresAt}).Error)
		request.RetryFact, request.PendingDecision = &fact, &decision
		require.NoError(t, service.SelectVideoRecoveryChannel(t.Context(), &job, &task, &request))
		require.NoError(t, model.DispatchAsyncVideoRecovery(t.Context(), job, task, &decision, 0, ""))
		require.ErrorIs(t, model.DispatchAsyncVideoRecovery(t.Context(), job, task, &decision, 0, ""), model.ErrTaskAttemptConflict)
		require.NoError(t, db.First(&task, task.ID).Error)
		assert.Equal(t, 1, task.PrivateData.MediaRetryState.TotalUsed)
		assert.Equal(t, 7, task.Quota)
		var event model.MediaRetryEvent
		require.NoError(t, db.Where("task_id = ?", task.TaskID).Take(&event).Error)
		assert.True(t, event.Executed)
	})
}

func TestConditionalVideoAdmissionWorkerPreservesSourcePrice(t *testing.T) {
	for _, tc := range []struct {
		name, firstCredential string
		failures              int32
		disableQueuedTarget   bool
		originalFallback      bool
		strictUnavailable     bool
		changeQueuedConfig    bool
		queuedRevocation      string
		unknownFirst          bool
		unmatchedRetry        bool
	}{
		{name: "initial target", firstCredential: "Bearer first-key"},
		{name: "confirmed failure retry", failures: 1, firstCredential: "Bearer first-key"},
		{name: "queued target revoked", disableQueuedTarget: true, firstCredential: "Bearer second-key"},
		{name: "all targets failed restore original", failures: 2, originalFallback: true, firstCredential: "Bearer first-key"},
		{name: "first strict rule cannot escape", strictUnavailable: true},
		{name: "queued rule and price remain frozen", failures: 1, changeQueuedConfig: true, firstCredential: "Bearer first-key"},
		{name: "queued all targets revoked strict", queuedRevocation: "all"},
		{name: "queued all targets revoked original", queuedRevocation: "all", originalFallback: true, firstCredential: "Bearer original-key"},
		{name: "queued source group permission revoked", queuedRevocation: "source_group"},
		{name: "queued target ability revoked", queuedRevocation: "target_ability", firstCredential: "Bearer second-key"},
		{name: "unknown result follows matching rule", failures: 1, unknownFirst: true, firstCredential: "Bearer first-key"},
		{name: "unknown result without matching rule", failures: 1, unknownFirst: true, unmatchedRetry: true, firstCredential: "Bearer first-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, token := setupResponsesWSRequestTest(t)
			db := setupVideoRetryTestDatabase(t, user, token)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Task{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.MediaRetryEvent{}, &model.Option{}, &model.UserSubscription{}, &model.AsyncImageUsageProjection{}, &model.AsyncImageLogReceipt{}, &model.CanvasGrant{}, &model.Log{}, &model.MediaRetryPolicyVersion{}, &model.MediaArtifactObject{}, &model.MediaInputObject{}))
			t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "conditional-test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
			t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "conditional-test")
			previousRegistry, previousSubmit := pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc
			previousSecret, previousTimeout := common.CryptoSecret, constant.TaskTimeoutMinutes
			previousBatch, previousLogs := common.BatchUpdateEnabled, common.LogConsumeEnabled
			pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc = pluginruntime.NewRegistry(), ExecuteAsyncVideo
			common.CryptoSecret, constant.TaskTimeoutMinutes = "conditional-test-signing-secret", 10
			common.BatchUpdateEnabled, common.LogConsumeEnabled = false, false
			t.Cleanup(func() {
				pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc = previousRegistry, previousSubmit
				common.CryptoSecret, constant.TaskTimeoutMinutes = previousSecret, previousTimeout
				common.BatchUpdateEnabled, common.LogConsumeEnabled = previousBatch, previousLogs
			})
			redisServer := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			common.RDB, common.RedisEnabled = client, true
			t.Cleanup(func() { assert.NoError(t, client.Close()) })
			cfg := service.DefaultMediaRuntimeConfig()
			cfg.VideoAsyncEnabled, cfg.LocalPath = true, t.TempDir()
			require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
			source, err := builtinplugins.Source("drama")
			require.NoError(t, err)
			_, err = pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: "drama"})
			require.NoError(t, err)
			var calls atomic.Int32
			var submitted []map[string]any
			var credentials []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				assert.NoError(t, common.DecodeJson(r.Body, &body))
				submitted = append(submitted, body)
				credentials = append(credentials, r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) <= tc.failures {
					status := http.StatusBadRequest
					if tc.unknownFirst {
						status = http.StatusBadGateway
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":{"code":"reference_not_available","message":"reference material unavailable"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"conditional-upstream","status":"queued"}`))
			}))
			t.Cleanup(upstream.Close)
			setting := `{"task_plugin_key":"drama"}`
			channels := []model.Channel{
				{Type: constant.ChannelTypeTaskPlugin, Name: "original", Status: common.ChannelStatusEnabled, Group: "default", Models: "seedance2.5-A", Key: "original-key", Setting: &setting, BaseURL: &upstream.URL},
				{Type: constant.ChannelTypeTaskPlugin, Name: "first-target", Status: common.ChannelStatusEnabled, Group: "default", Models: "seedance2.5-E", Key: "first-key", Setting: &setting, BaseURL: &upstream.URL},
				{Type: constant.ChannelTypeTaskPlugin, Name: "second-target", Status: common.ChannelStatusEnabled, Group: "default", Models: "seedance2.5-E", Key: "second-key", Setting: &setting, BaseURL: &upstream.URL},
			}
			require.NoError(t, db.Create(&channels).Error)
			for _, channel := range channels {
				require.NoError(t, db.Create(&model.Ability{Group: "default", Model: channel.Models, ChannelId: channel.Id, Enabled: true}).Error)
			}
			policy := model.ImageGroupPolicy{Group: "default", Platform: "mixed", PolicyKey: service.ImageIdentityHash("default", "mixed"), Enabled: true, AsyncEnabled: true, PoolMode: "model", Models: `[{"id":"seedance2.5-A","media_type":"video"}]`}
			require.NoError(t, db.Create(&policy).Error)
			require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: service.MediaPoolBindingKey(policy, "seedance2.5-A", "1K", "video"), Group: "default", Platform: "mixed", Mode: "model", Model: "seedance2.5-A", ChannelId: channels[0].Id, Priority: 1}).Error)
			minimum := 15
			routing := model.VideoRoutingConfig{Version: 1, Enabled: true, Rules: []model.VideoRoutingRule{{ID: "upgrade", Name: "Upgrade", Enabled: true, Match: model.VideoRoutingMatch{Models: []string{"seedance2.5-A"}, Resolutions: []string{"480p"}, Seconds: model.VideoRoutingSeconds{Min: &minimum, MinExclusive: true}}, Targets: []model.VideoRoutingTarget{{ChannelID: channels[1].Id, Model: "seedance2.5-E", Resolution: "720p"}, {ChannelID: channels[2].Id, Model: "seedance2.5-E", Resolution: "720p"}}, OnExhausted: "fail"}}}
			if tc.originalFallback {
				routing.Rules[0].OnExhausted = "original"
			}
			if tc.strictUnavailable {
				routing.Rules = append(routing.Rules, model.VideoRoutingRule{ID: "later", Name: "Later match", Enabled: true, Match: routing.Rules[0].Match, Targets: []model.VideoRoutingTarget{{ChannelID: channels[0].Id, Model: "seedance2.5-A"}}, OnExhausted: "original"})
			}
			require.NoError(t, service.ValidateVideoRoutingConfig(t.Context(), routing))
			_, err = model.SaveVideoRoutingConfig(t.Context(), routing, 0)
			require.NoError(t, err)
			settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
			previousPrices := settings.MixedVideoBillingExpr
			settings.MixedVideoBillingExpr = map[string]string{"seedance2.5-A": `u("resolution") == "480p" ? tier("original", u("seconds") * 0.00001) : tier("other", u("seconds") * 0.001)`}
			t.Cleanup(func() { settings.MixedVideoBillingExpr = previousPrices })
			retryPolicy := mediaretry.Policy{Rules: []mediaretry.Rule{{ID: "reference", Name: "Reference recovery", Enabled: true, Scope: mediaretry.Scope{MediaTypes: []string{"video"}}, Match: mediaretry.Match{ContainsAny: []string{"material unavailable"}}, Steps: []mediaretry.Step{{ID: "next", Action: mediaretry.OtherChannel, MaxAttempts: 1}}}}}
			if tc.unmatchedRetry {
				retryPolicy.Rules = nil
			}
			if tc.originalFallback {
				retryPolicy.Rules[0].Steps[0].MaxAttempts = 2
			}
			_, err = model.SaveMediaRetryPolicy(t.Context(), retryPolicy, 0, user.Id, nil)
			require.NoError(t, err)
			engine := gin.New()
			engine.POST("/v1/videos/generations_async", middleware.TokenAuth(), PrepareAsyncVideo, PinAsyncVideoEndpoint(), FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), RouteAsyncVideo, middleware.Distribute(), AcceptAsyncVideo)
			if tc.strictUnavailable {
				require.NoError(t, db.Model(&model.Channel{}).Where("id IN ?", []int{channels[1].Id, channels[2].Id}).Update("status", common.ChannelStatusManuallyDisabled).Error)
			}
			if tc.name == "initial target" {
				providerEngine := gin.New()
				providerEngine.POST("/v1/videos/generations_async", middleware.TokenAuth(), PrepareAsyncVideo, PinAsyncVideoEndpoint(), FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), RouteAsyncVideo, middleware.Distribute(), func(c *gin.Context) {
					_, conditional := c.Get(service.VideoRoutingContextKey)
					assert.False(t, conditional)
					assert.False(t, c.GetBool("async_media_mixed_video"))
					assert.Equal(t, channels[0].Id, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
					storage, err := common.GetBodyStorage(c)
					require.NoError(t, err)
					raw, err := storage.Bytes()
					require.NoError(t, err)
					assert.Contains(t, string(raw), `"resolution":"480p"`)
					c.Status(http.StatusNoContent)
				})
				providerRequest := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", strings.NewReader(`{"provider":"drama","model":"seedance2.5-A","prompt":"explicit provider","seconds":20,"resolution":"480p","aspect_ratio":"16:9"}`))
				providerRequest.Header.Set("Content-Type", "application/json")
				providerRequest.Header.Set("Authorization", "Bearer sk-"+token.Key)
				providerRecorder := httptest.NewRecorder()
				providerEngine.ServeHTTP(providerRecorder, providerRequest)
				require.Equal(t, http.StatusNoContent, providerRecorder.Code, providerRecorder.Body.String())
				assert.Zero(t, calls.Load())
			}
			recorder := httptest.NewRecorder()
			requestHTTP := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", strings.NewReader(`{"model":"seedance2.5-A","prompt":"unchanged prompt","seconds":20,"resolution":"480p","aspect_ratio":"16:9"}`))
			requestHTTP.Header.Set("Content-Type", "application/json")
			requestHTTP.Header.Set("Authorization", "Bearer sk-"+token.Key)
			engine.ServeHTTP(recorder, requestHTTP)
			if tc.strictUnavailable {
				require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
				assert.Contains(t, recorder.Body.String(), "no compatible conditional video target")
				assert.Zero(t, calls.Load())
				var jobs int64
				require.NoError(t, db.Model(&model.AsyncMediaJob{}).Count(&jobs).Error)
				assert.Zero(t, jobs)
				return
			}
			require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())
			assert.Zero(t, calls.Load(), "admission must not send a paid generation")
			var accepted struct {
				TaskID string `json:"task_id"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &accepted))
			var job model.AsyncMediaJob
			require.NoError(t, db.Where("task_id = ?", accepted.TaskID).Take(&job).Error)
			assert.Equal(t, "seedance2.5-A", job.Model)
			assert.Equal(t, channels[1].Id, job.ChannelId)
			plain, err := service.DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
			require.NoError(t, err)
			var frozen service.AsyncVideoRequest
			require.NoError(t, common.Unmarshal(plain, &frozen))
			require.NotNil(t, frozen.ConditionalRouting)
			assert.Equal(t, "480p", frozen.ConditionalRouting.SourceFacts["resolution"])
			assert.Equal(t, "480p", frozen.Details.RequestedResolution)
			assert.Equal(t, "seedance2.5-A", frozen.Billing.Tiered.ModelName)
			assert.Equal(t, 100, frozen.Billing.Price.Quota)
			assert.Contains(t, string(frozen.Body), `"resolution":"720p"`)
			for _, admin := range []bool{false, true} {
				view, _, err := videoJobView(taskSubmissionTestContext(), job, admin, false)
				require.NoError(t, err)
				assert.Equal(t, "seedance2.5-A", view["model"])
				assert.Equal(t, "480p", view["requested_resolution"])
				assert.Equal(t, "720p", view["execution_resolution"])
				_, hasRouting := view["video_routing"]
				assert.Equal(t, admin, hasRouting)
				_, hasPlatform := view["platform"]
				_, hasProvider := view["provider"]
				assert.Equal(t, admin, hasPlatform)
				assert.Equal(t, admin, hasProvider)
			}
			if tc.name == "initial target" {
				var nativeCount int64
				require.NoError(t, db.Model(&model.Task{}).Where("task_id = ?", job.TaskId).Count(&nativeCount).Error)
				require.Zero(t, nativeCount)
				for _, projection := range []struct {
					status    string
					cipher    []byte
					expiresAt int64
				}{
					{status: "queued", cipher: job.RequestCipher, expiresAt: time.Now().Unix() - 1},
					{status: "expired", expiresAt: time.Now().Unix() - 1},
					{status: "failed", expiresAt: job.ExpiresAt},
				} {
					copy := job
					copy.Status, copy.RequestCipher, copy.ExpiresAt = projection.status, projection.cipher, projection.expiresAt
					for _, admin := range []bool{false, true} {
						view, _, err := videoJobView(taskSubmissionTestContext(), copy, admin, false)
						require.NoError(t, err)
						assert.Equal(t, "seedance2.5-A", view["model"])
						_, hasPlatform := view["platform"]
						_, hasProvider := view["provider"]
						_, hasRouting := view["video_routing"]
						assert.Equal(t, admin, hasPlatform, "status=%s", projection.status)
						assert.Equal(t, admin, hasProvider, "status=%s", projection.status)
						assert.Equal(t, admin && len(projection.cipher) > 0, hasRouting)
						if len(projection.cipher) > 0 {
							assert.Equal(t, "480p", view["requested_resolution"])
							assert.Equal(t, "720p", view["execution_resolution"])
						}
					}
				}
			}
			if tc.disableQueuedTarget {
				require.NoError(t, db.Model(&channels[1]).Update("status", common.ChannelStatusManuallyDisabled).Error)
			}
			switch tc.queuedRevocation {
			case "all":
				require.NoError(t, db.Model(&model.Channel{}).Where("id IN ?", []int{channels[1].Id, channels[2].Id}).Update("status", common.ChannelStatusManuallyDisabled).Error)
			case "source_group":
				require.NoError(t, db.Model(token).Update("group", "revoked").Error)
			case "target_ability":
				require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channels[1].Id).Update("enabled", false).Error)
			}
			if tc.changeQueuedConfig {
				latest, err := model.GetVideoRoutingConfig(t.Context())
				require.NoError(t, err)
				latest.Enabled = false
				latest.Rules[0].Targets = []model.VideoRoutingTarget{{ChannelID: channels[0].Id, Model: "seedance2.5-A", Resolution: "1080p"}}
				latest.Rules[0].OnExhausted = "original"
				_, err = model.SaveVideoRoutingConfig(t.Context(), latest, latest.Revision)
				require.NoError(t, err)
				settings.MixedVideoBillingExpr = map[string]string{"seedance2.5-A": `tier("new price", 0.01)`}
			}
			claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "conditional-worker", 120)
			require.NoError(t, err)
			require.True(t, claimed)
			processErr := service.ProcessAsyncMediaJob(t.Context(), job, cfg)
			blockedBeforeSend := tc.queuedRevocation == "source_group" || tc.queuedRevocation == "all" && !tc.originalFallback
			if blockedBeforeSend || tc.unmatchedRetry {
				if blockedBeforeSend {
					require.Error(t, processErr)
				}
				require.NoError(t, db.First(&job, job.ID).Error)
				require.NoError(t, db.First(user, user.Id).Error)
				require.NoError(t, db.First(token, token.Id).Error)
				if tc.unknownFirst {
					assert.EqualValues(t, 1, calls.Load(), "an unmatched unknown execution cannot replay")
					assert.Equal(t, "execution_unknown", job.Status)
					assert.Equal(t, "unknown", job.BillingStatus)
					assert.Equal(t, 900, user.Quota)
					assert.Zero(t, token.RemainQuota)
					var uncertain model.Task
					require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&uncertain).Error)
					assert.Equal(t, model.TaskStatus(model.TaskStatusUnknown), uncertain.Status)
					assert.Equal(t, 100, uncertain.Quota)
					assert.Equal(t, "seedance2.5-A", uncertain.Properties.OriginModelName)
					assert.Equal(t, "seedance2.5-E", uncertain.Properties.UpstreamModelName)
					require.NotNil(t, uncertain.PrivateData.VideoRouting)
					assert.Equal(t, "480p", uncertain.PrivateData.VideoRouting.SourceResolution)
					assert.Equal(t, "720p", uncertain.PrivateData.VideoRouting.ExecutionResolution)
					require.NotNil(t, uncertain.PrivateData.VideoReconciliation)
					assert.True(t, uncertain.PrivateData.VideoReconciliation.ManualRequired)
					require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
					assert.EqualValues(t, 1, calls.Load(), "worker recovery must also leave the uncertain attempt alone")
					require.NoError(t, db.First(user, user.Id).Error)
					require.NoError(t, db.First(token, token.Id).Error)
					assert.Equal(t, 900, user.Quota)
					assert.Zero(t, token.RemainQuota)
				} else {
					assert.Zero(t, calls.Load())
					assert.Equal(t, "failed", job.Status)
					assert.Equal(t, "not_billable", job.BillingStatus)
					assert.Zero(t, job.DispatchedAt)
					assert.Zero(t, job.Quota)
					assert.Equal(t, 1000, user.Quota)
					assert.Equal(t, 100, token.RemainQuota)
				}
				var nativeCount int64
				require.NoError(t, db.Model(&model.Task{}).Where("task_id = ?", job.TaskId).Count(&nativeCount).Error)
				if tc.unknownFirst {
					assert.EqualValues(t, 1, nativeCount)
				} else {
					assert.Zero(t, nativeCount)
				}
				return
			}
			require.NoError(t, processErr)
			require.NoError(t, db.First(&job, job.ID).Error)
			assert.Equal(t, "submitted", job.Status)
			expectedCalls, expectedChannel := tc.failures+1, channels[1].Id
			if tc.failures > 0 {
				expectedChannel = channels[2].Id
			}
			if tc.disableQueuedTarget || tc.queuedRevocation == "target_ability" {
				expectedChannel = channels[2].Id
			}
			executionModel, executionResolution := "seedance2.5-E", "720p"
			if tc.originalFallback {
				expectedChannel, executionModel, executionResolution = channels[0].Id, "seedance2.5-A", "480p"
			}
			assert.Equal(t, expectedCalls, calls.Load())
			require.Len(t, submitted, int(expectedCalls))
			for index, body := range submitted {
				modelName, resolution := "seedance2.5-E", "720p"
				if index == len(submitted)-1 {
					modelName, resolution = executionModel, executionResolution
				}
				assert.Equal(t, modelName, body["model"])
				assert.Equal(t, resolution, body["resolution"])
				assert.Equal(t, float64(20), body["seconds"])
				assert.Equal(t, "unchanged prompt", body["prompt"])
			}
			assert.Equal(t, tc.firstCredential, credentials[0])
			if tc.failures > 0 {
				assert.Equal(t, "Bearer second-key", credentials[1])
			}
			if tc.originalFallback && tc.failures > 0 {
				assert.Equal(t, "Bearer original-key", credentials[2])
			}
			var task model.Task
			require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&task).Error)
			assert.Equal(t, expectedChannel, task.ChannelId)
			assert.Equal(t, "seedance2.5-A", task.Properties.OriginModelName)
			assert.Equal(t, executionModel, task.Properties.UpstreamModelName)
			assert.Equal(t, 100, task.Quota)
			if tc.unknownFirst {
				require.Len(t, task.PrivateData.VideoAttempts, 1)
				assert.Equal(t, model.TaskStatusUnknown, task.PrivateData.VideoAttempts[0].Status)
				assert.Equal(t, "drama", task.PrivateData.VideoAttempts[0].PluginKey)
				assert.Equal(t, channels[1].Id, task.BillingChannelID())
			}
			require.NotNil(t, task.PrivateData.VideoRouting)
			assert.EqualValues(t, 1, task.PrivateData.VideoRouting.Revision)
			assert.Equal(t, "upgrade", task.PrivateData.VideoRouting.RuleID)
			assert.Equal(t, tc.originalFallback, task.PrivateData.VideoRouting.IsFallback)
			for _, admin := range []bool{false, true} {
				view, _, err := videoJobView(taskSubmissionTestContext(), job, admin, false)
				require.NoError(t, err)
				assert.Equal(t, "seedance2.5-A", view["model"])
				assert.Equal(t, "480p", view["requested_resolution"])
				assert.Equal(t, executionResolution, view["execution_resolution"])
				_, hasRouting := view["video_routing"]
				assert.Equal(t, admin, hasRouting)
				_, hasPlatform := view["platform"]
				_, hasProvider := view["provider"]
				assert.Equal(t, admin, hasPlatform)
				assert.Equal(t, admin, hasProvider)
			}
			require.NoError(t, db.First(user, user.Id).Error)
			require.NoError(t, db.First(token, token.Id).Error)
			assert.Equal(t, 900, user.Quota)
			assert.Zero(t, token.RemainQuota)
		})
	}
}

type taskSubmissionTestBilling struct {
	events     *[]string
	settleErr  error
	reserveErr error
	onSettle   func()
	refunds    int
}

func (b *taskSubmissionTestBilling) Settle(int) error {
	*b.events = append(*b.events, "settle")
	if b.onSettle != nil {
		b.onSettle()
	}
	return b.settleErr
}

func (b *taskSubmissionTestBilling) Refund(*gin.Context) {
	*b.events = append(*b.events, "refund")
	b.refunds++
}

func (b *taskSubmissionTestBilling) NeedsRefund() bool        { return b.refunds == 0 }
func (b *taskSubmissionTestBilling) GetPreConsumedQuota() int { return 0 }
func (b *taskSubmissionTestBilling) Reserve(int) error {
	*b.events = append(*b.events, "reserve")
	return b.reserveErr
}

func TestMixedVideoResolutionMappingDispatch(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	dialect := cmp.Or(os.Getenv("RETRY_POLICY_TEST_DIALECT"), "sqlite")
	db := modelManagementDB(t, dialect, os.Getenv("RETRY_POLICY_TEST_DSN"))
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.Task{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.MediaRetryEvent{}, &model.UserSubscription{}, &model.AsyncImageUsageProjection{}, &model.AsyncImageLogReceipt{}, &model.CanvasGrant{}, &model.Log{}, &model.MediaRetryPolicyVersion{}, &model.MediaArtifactObject{}, &model.MediaInputObject{}))
	token.RemainQuota = 1000
	require.NoError(t, db.Create(user).Error)
	require.NoError(t, db.Create(token).Error)
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "video-map:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "video-map")
	previousRegistry, previousSubmit := pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc
	previousSecret, previousTimeout, previousUnit := common.CryptoSecret, constant.TaskTimeoutMinutes, common.QuotaPerUnit
	previousRedis, previousRedisEnabled := common.RDB, common.RedisEnabled
	previousBatch := common.BatchUpdateEnabled
	pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc = pluginruntime.NewRegistry(), ExecuteAsyncVideo
	common.CryptoSecret, constant.TaskTimeoutMinutes, common.QuotaPerUnit = "video-map-fixture-secret", 10, 500000
	common.BatchUpdateEnabled = false
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc = previousRegistry, previousSubmit
		common.CryptoSecret, constant.TaskTimeoutMinutes, common.QuotaPerUnit = previousSecret, previousTimeout, previousUnit
		common.RDB, common.RedisEnabled, common.BatchUpdateEnabled = previousRedis, previousRedisEnabled, previousBatch
		assert.NoError(t, client.Close())
	})
	cfg := service.DefaultMediaRuntimeConfig()
	cfg.VideoAsyncEnabled, cfg.LocalPath = true, t.TempDir()
	require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
	source, err := builtinplugins.Source("yali")
	require.NoError(t, err)
	_, err = pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: "yali"})
	require.NoError(t, err)
	requests := make(chan map[string]any, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := common.DecodeJson(r.Body, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"mapped-video","status":"queued"}`)
	}))
	t.Cleanup(upstream.Close)
	const publicModel = "minimax-h3-sec"
	setting := `{"task_plugin_key":"yali","task_plugin_model_configs":{
		"MiniMaxH3-480p-sec":{"duration":{"min":4,"max":15,"step":1,"default":5},"modes":["text_to_video"],"resolutions":["480p"],"aspect_ratios":["16:9"],"references":{"images":0,"videos":0,"audios":0},"billing_mode":"per_second"},
		"MiniMaxH3-2k-pro-sec":{"duration":{"min":4,"max":15,"step":1,"default":5},"modes":["text_to_video"],"resolutions":["768p","2k"],"aspect_ratios":["16:9"],"references":{"images":0,"videos":0,"audios":0},"billing_mode":"per_second"}
	},"video_resolution_model_mapping":{"minimax-h3-sec":{"480p":"MiniMaxH3-480p-sec","720p":"MiniMaxH3-2k-pro-sec","2k":"MiniMaxH3-2k-pro-sec"}}}`
	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "Yali resolution fixture", Status: common.ChannelStatusEnabled, Group: "default", Models: publicModel, Key: "fixture-key", Setting: &setting, BaseURL: &upstream.URL}
	require.NoError(t, channel.Insert())
	policy := model.ImageGroupPolicy{Group: "default", Platform: "mixed", PolicyKey: service.ImageIdentityHash("default", "mixed"), Enabled: true, AsyncEnabled: true, PoolMode: "model_resolution", Models: `[{"id":"minimax-h3-sec","media_type":"video"}]`}
	require.NoError(t, db.Create(&policy).Error)
	for _, tier := range []string{"1K", "2K"} {
		require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: service.MediaPoolBindingKey(policy, publicModel, tier, "video"), Group: "default", Platform: "mixed", Mode: policy.PoolMode, Model: publicModel, Resolution: tier, ChannelId: channel.Id, Priority: 1}).Error)
	}
	const price = `u("resolution") == "480p" ? tier("480p", u("seconds") * 0.00001) : (u("resolution") == "720p" ? tier("720p", u("seconds") * 0.00002) : tier("2k", u("seconds") * 0.00003))`
	prices := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	previousPrices := prices.MixedVideoBillingExpr
	prices.MixedVideoBillingExpr = map[string]string{publicModel: price}
	t.Cleanup(func() { prices.MixedVideoBillingExpr = previousPrices })
	engine := gin.New()
	engine.POST("/v1/videos/generations_async", middleware.TokenAuth(), PrepareAsyncVideo, PinAsyncVideoEndpoint(), FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), RouteAsyncVideo, middleware.Distribute(), AcceptAsyncVideo)
	accept := func(t *testing.T, resolution string, status int) model.AsyncMediaJob {
		t.Helper()
		body := gin.H{"model": publicModel, "prompt": "test motion", "seconds": 5, "aspect_ratio": "16:9"}
		if resolution != "" {
			body["resolution"] = resolution
		}
		encoded, err := common.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-"+token.Key)
		res := httptest.NewRecorder()
		engine.ServeHTTP(res, req)
		require.Equal(t, status, res.Code, res.Body.String())
		if status != http.StatusAccepted {
			return model.AsyncMediaJob{}
		}
		var accepted struct {
			TaskID string `json:"task_id"`
		}
		require.NoError(t, common.Unmarshal(res.Body.Bytes(), &accepted))
		var job model.AsyncMediaJob
		require.NoError(t, db.Where("task_id = ?", accepted.TaskID).Take(&job).Error)
		assert.Equal(t, channel.Id, job.ChannelId)
		return job
	}
	accept(t, "", http.StatusBadRequest)
	accept(t, "1080p", http.StatusBadRequest)
	for _, tc := range []struct {
		name, resolution, upstreamModel, upstreamResolution string
		quota                                               int
	}{
		{"480p", "480p", "MiniMaxH3-480p-sec", "480p", 25},
		{"720p", "720p", "MiniMaxH3-2k-pro-sec", "768P", 50},
		{"2k", "2k", "MiniMaxH3-2k-pro-sec", "2k", 75},
		{"conditional", "480p", "MiniMaxH3-2k-pro-sec", "768P", 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "conditional" {
				rules := model.VideoRoutingConfig{Version: 1, Enabled: true, Rules: []model.VideoRoutingRule{{
					ID: "mapped-tier", Name: "Mapped tier", Enabled: true,
					Match:       model.VideoRoutingMatch{Models: []string{publicModel}, Resolutions: []string{"480p"}},
					Targets:     []model.VideoRoutingTarget{{ChannelID: channel.Id, Model: publicModel, Resolution: "720p"}},
					OnExhausted: "fail",
				}}}
				require.NoError(t, service.ValidateVideoRoutingConfig(t.Context(), rules))
				_, err := model.SaveVideoRoutingConfig(t.Context(), rules, 0)
				require.NoError(t, err)
			}
			preview, err := service.PreviewMediaRouting(t.Context(), service.MediaRoutingPreviewRequest{
				Platform: "mixed", Group: "default", MediaType: "video", Model: publicModel, Resolution: tc.resolution,
				Operation: "text_to_video", Seconds: common.GetPointer(5),
			})
			require.NoError(t, err)
			require.Len(t, preview.Eligible, 1, "%+v", preview)
			assert.Equal(t, tc.upstreamModel, preview.Eligible[0].UpstreamModel)
			job := accept(t, tc.resolution, http.StatusAccepted)
			require.Empty(t, requests)
			require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("setting", `{"task_plugin_key":"yali"}`).Error)
			t.Cleanup(func() {
				assert.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("setting", setting).Error)
			})
			if tc.name == "480p" {
				plain, err := service.DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
				require.NoError(t, err)
				var retry service.AsyncVideoRequest
				require.NoError(t, common.Unmarshal(plain, &retry))
				retry.PendingDecision = &mediaretry.Decision{Action: mediaretry.SameChannel, Matched: true}
				retry.RetryFact = &mediaretry.Fact{ChannelID: channel.Id, Provider: "yali", Terminal: true}
				same, err := service.PickVideoRetryChannel(t.Context(), job, retry, tc.upstreamModel)
				require.NoError(t, err)
				candidate, err := service.RebindConditionalVideoRequest(t.Context(), job, &retry, *same, service.VideoModelOverrides{})
				require.NoError(t, err)
				assert.Equal(t, tc.upstreamModel, candidate.UpstreamModel)
				other := channel
				other.Id, other.Name, other.Key = 0, "alternate resolution fixture", "fixture-key-2"
				otherSetting := strings.ReplaceAll(setting, tc.upstreamModel, "alternate-480p")
				other.Setting = &otherSetting
				require.NoError(t, other.Insert())
				pool := model.ImageChannelPool{BindingKey: service.MediaPoolBindingKey(policy, publicModel, "1K", "video"), Group: "default", Platform: "mixed", Mode: policy.PoolMode, Model: publicModel, Resolution: "1K", ChannelId: other.Id, Priority: 1}
				require.NoError(t, db.Create(&pool).Error)
				t.Cleanup(func() {
					assert.NoError(t, db.Delete(&pool).Error)
					assert.NoError(t, db.Where("channel_id = ?", other.Id).Delete(&model.Ability{}).Error)
					assert.NoError(t, db.Delete(&other).Error)
				})
				retry.PendingDecision.Action = mediaretry.OtherChannel
				next, err := service.PickVideoRetryChannel(t.Context(), job, retry, tc.upstreamModel)
				require.NoError(t, err)
				candidate, err = service.RebindConditionalVideoRequest(t.Context(), job, &retry, *next, service.VideoModelOverrides{})
				require.NoError(t, err)
				require.NoError(t, service.RebindMixedVideoRequest(&retry, candidate, job.ExpiresAt))
				assert.Equal(t, other.Id, retry.ResolutionMapping.ChannelID)
				assert.Equal(t, "alternate-480p", retry.ResolutionMapping.UpstreamModel)
				assert.Equal(t, tc.quota, retry.Billing.Price.Quota)
				assert.Equal(t, tc.resolution, retry.Billing.Tiered.UsageFacts["resolution"])
			}
			claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "video-map-worker", 120)
			require.NoError(t, err)
			require.True(t, claimed)
			require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
			require.Len(t, requests, 1)
			body := <-requests
			assert.Equal(t, tc.upstreamModel, body["model"])
			assert.Equal(t, tc.upstreamResolution, body["resolution"])
			var task model.Task
			require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&task).Error)
			assert.Equal(t, tc.upstreamModel, task.Properties.UpstreamModelName)
			assert.Equal(t, tc.quota, task.Quota)
			require.NotNil(t, task.PrivateData.BillingContext)
			assert.Equal(t, tc.resolution, task.PrivateData.BillingContext.TieredSnapshot.UsageFacts["resolution"])
		})
	}
}

func TestMixedVideoPricingAPIThroughSubmissionAndSettlement(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	dialect := os.Getenv("RETRY_POLICY_TEST_DIALECT")
	if dialect == "" {
		dialect = "sqlite"
	}
	db := modelManagementDB(t, dialect, os.Getenv("RETRY_POLICY_TEST_DSN"))
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.Task{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.MediaRetryEvent{}, &model.UserSubscription{}, &model.AsyncImageUsageProjection{}, &model.AsyncImageLogReceipt{}, &model.CanvasGrant{}, &model.Log{}, &model.MediaRetryPolicyVersion{}, &model.MediaArtifactObject{}, &model.MediaInputObject{}))
	token.RemainQuota = 1000
	require.NoError(t, db.Create(user).Error)
	require.NoError(t, db.Create(token).Error)
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "pricing-chain:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "pricing-chain")
	previousRegistry, previousSubmit := pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc
	previousSecret, previousTimeout := common.CryptoSecret, constant.TaskTimeoutMinutes
	previousBatch, previousLogs, previousExport, previousUnit := common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled, common.QuotaPerUnit
	pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc = pluginruntime.NewRegistry(), ExecuteAsyncVideo
	common.CryptoSecret, constant.TaskTimeoutMinutes = "pricing-chain-signing-secret", 10
	common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled, common.QuotaPerUnit = false, true, false, 500000
	t.Cleanup(func() {
		pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc = previousRegistry, previousSubmit
		common.CryptoSecret, constant.TaskTimeoutMinutes = previousSecret, previousTimeout
		common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled, common.QuotaPerUnit = previousBatch, previousLogs, previousExport, previousUnit
	})
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	cfg := service.DefaultMediaRuntimeConfig()
	cfg.VideoAsyncEnabled, cfg.LocalPath = true, t.TempDir()
	require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
	source, err := builtinplugins.Source("drama")
	require.NoError(t, err)
	_, err = pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: "drama"})
	require.NoError(t, err)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"pricing-upstream-%d","status":"queued"}`, calls.Add(1))
	}))
	t.Cleanup(upstream.Close)
	const modelName = "seedance2.5-A"
	setting := `{"task_plugin_key":"drama"}`
	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "pricing-chain", Status: common.ChannelStatusEnabled, Group: "default", Models: modelName, Key: "fixture-key", Setting: &setting, BaseURL: &upstream.URL}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: modelName, ChannelId: channel.Id, Enabled: true}).Error)
	for _, platform := range []string{"mixed", "drama"} {
		policy := model.ImageGroupPolicy{Group: "default", Platform: platform, PolicyKey: service.ImageIdentityHash("default", platform), Enabled: true, AsyncEnabled: true, PoolMode: "model", Models: `[{"id":"seedance2.5-A","media_type":"video"}]`}
		require.NoError(t, db.Create(&policy).Error)
		require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: service.MediaPoolBindingKey(policy, modelName, "1K", "video"), Group: "default", Platform: platform, Mode: "model", Model: modelName, ChannelId: channel.Id, Priority: 1}).Error)
	}
	const providerExpression = `tier("provider", u("seconds") * 0.00003)`
	savePrice := func(expression string) {
		t.Helper()
		snapshot, err := model.GetModelPricingSnapshot([]string{modelName})
		require.NoError(t, err)
		require.Len(t, snapshot.Entries, 1)
		change := model.ModelPricingChange{ModelName: modelName, ExpectedVersion: snapshot.Entries[0].Version, Pricing: model.PricingValues{
			billing_setting.MixedVideoBillingExprOption: expression,
			billing_setting.PluginBillingExprOption:     map[string]string{"drama": providerExpression},
		}}
		response := modelManagementRequest(t, UpdateModelPricingConfig, http.MethodPatch, "/api/option/model_pricing", gin.H{"changes": []model.ModelPricingChange{change}}, nil)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), `"success":true`)
	}
	engine := gin.New()
	engine.POST("/v1/videos/generations_async", middleware.TokenAuth(), PrepareAsyncVideo, PinAsyncVideoEndpoint(), FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), RouteAsyncVideo, middleware.Distribute(), AcceptAsyncVideo)
	accept := func(provider string, quota int, expression string) model.AsyncMediaJob {
		t.Helper()
		body, err := common.Marshal(gin.H{"model": modelName, "provider": provider, "prompt": "pricing chain", "seconds": 20, "resolution": "480p", "aspect_ratio": "16:9"})
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer sk-"+token.Key)
		recorder := httptest.NewRecorder()
		beforeCalls := calls.Load()
		engine.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())
		assert.Equal(t, beforeCalls, calls.Load(), "admission must not submit upstream")
		var accepted struct {
			TaskID string `json:"task_id"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &accepted))
		var job model.AsyncMediaJob
		require.NoError(t, db.Where("task_id = ?", accepted.TaskID).Take(&job).Error)
		plain, err := service.DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
		require.NoError(t, err)
		var frozen service.AsyncVideoRequest
		require.NoError(t, common.Unmarshal(plain, &frozen))
		require.NotNil(t, frozen.Billing.Tiered)
		assert.Equal(t, quota, frozen.Billing.Price.Quota)
		assert.Equal(t, expression, frozen.Billing.Tiered.ExprString)
		assert.Equal(t, provider == "", frozen.Billing.Tiered.MixedVideoBilling)
		return job
	}
	dispatch := func(job model.AsyncMediaJob, quota int) model.Task {
		t.Helper()
		claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "pricing-chain-worker", 120)
		require.NoError(t, err)
		require.True(t, claimed)
		require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
		var task model.Task
		require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&task).Error)
		assert.Equal(t, quota, task.Quota)
		return task
	}
	assertAccounting := func(balance, used int) {
		t.Helper()
		require.NoError(t, db.First(user, user.Id).Error)
		require.NoError(t, db.First(token, token.Id).Error)
		require.NoError(t, db.First(&channel, channel.Id).Error)
		assert.Equal(t, balance, user.Quota)
		assert.Equal(t, balance, token.RemainQuota)
		assert.Equal(t, used, user.UsedQuota)
		assert.Equal(t, used, token.UsedQuota)
		assert.EqualValues(t, used, channel.UsedQuota)
	}
	settle := func(task model.Task, status model.TaskStatus, seconds float64, quota int) {
		t.Helper()
		previous := task.Status
		task.Status = status
		stale := task
		result := &relaycommon.TaskInfo{UsageFacts: map[string]any{"seconds": seconds}}
		won, err := service.SettleTaskBillingOnComplete(t.Context(), nil, &task, previous, result)
		require.NoError(t, err)
		require.True(t, won)
		assert.Equal(t, quota, task.Quota)
		won, err = service.SettleTaskBillingOnComplete(t.Context(), nil, &stale, previous, result)
		require.NoError(t, err)
		assert.False(t, won, "duplicate terminal updates must not charge or refund again")
	}
	const priceA = `tier("A", u("seconds") * 0.00001)`
	const priceB = `tier("B", u("seconds") * 0.00002)`
	savePrice(priceA)
	first := accept("", 100, priceA)
	assertAccounting(1000, 0)
	// The accepted job must retain A even if the administrator changes the
	// price while it is queued, before the worker reserves any funds.
	savePrice(priceB)
	firstTask := dispatch(first, 100)
	assertAccounting(900, 100)
	settle(firstTask, model.TaskStatusSuccess, 999, 100)
	assertAccounting(900, 100)
	secondTask := dispatch(accept("", 200, priceB), 200)
	assertAccounting(700, 300)
	settle(secondTask, model.TaskStatusFailure, 20, 0)
	assertAccounting(900, 100)
	providerTask := dispatch(accept("drama", 300, providerExpression), 300)
	assertAccounting(600, 400)
	settle(providerTask, model.TaskStatusSuccess, 20, 300)
	assertAccounting(600, 400)
	assert.EqualValues(t, 3, calls.Load())
	var logs []model.Log
	require.NoError(t, db.Where("user_id = ? AND type IN ?", user.Id, []int{model.LogTypeConsume, model.LogTypeRefund}).Order("id").Find(&logs).Error)
	require.Len(t, logs, 4, "three reservations and one refund must be auditable exactly once")
	assert.Equal(t, []int{100, 200, 200, 300}, []int{logs[0].Quota, logs[1].Quota, logs[2].Quota, logs[3].Quota})
	assert.Equal(t, model.LogTypeRefund, logs[2].Type)
}

func TestMixedVideoBuiltinPublicNormalization(t *testing.T) {
	previousRegistry := pluginruntime.DefaultRegistry
	pluginruntime.DefaultRegistry = pluginruntime.NewRegistry()
	t.Cleanup(func() { pluginruntime.DefaultRegistry = previousRegistry })
	source, err := os.ReadFile("../plugins/tasks/drama/plugin.js")
	require.NoError(t, err)
	plugin, err := pluginruntime.DefaultRegistry.RegisterFactory(string(source), pluginruntime.Options{Key: "drama"})
	require.NoError(t, err)
	const publicModel = "public-drama-video"
	setting := `{"task_plugin_key":"drama"}`
	for _, tc := range []struct {
		name, model, operation, fields, resolution, wireResolution, ratio, wantError string
	}{
		{name: "F default retains wire resolution", model: "seedance2.0-F", fields: `"seconds":4`, resolution: "720px", ratio: "16:9"},
		{name: "F explicit wire resolution", model: "seedance2.0-F", fields: `"seconds":4,"resolution":"720px"`, resolution: "720px", ratio: "16:9"},
		{name: "H3 camel case portrait", model: "minimax-h3", fields: `"seconds":4,"resolution":"720p","aspectRatio":"9:16"`, resolution: "720p", wireResolution: "768p", ratio: "9:16"},
		{name: "H3 short portrait alias", model: "minimax-h3", fields: `"seconds":4,"resolution":"720p","ratio":"9:16"`, resolution: "720p", wireResolution: "768p", ratio: "9:16"},
		{name: "matching aliases", model: "minimax-h3", fields: `"seconds":4,"aspectRatio":"9:16","aspect_ratio":"9:16","ratio":"9:16"`, resolution: "480p", ratio: "9:16"},
		{name: "conflicting aliases", model: "minimax-h3", fields: `"seconds":4,"aspectRatio":"9:16","aspect_ratio":"16:9"`, wantError: "conflicting mixed video aspect ratios"},
		{name: "F wire resolution is not global", model: "minimax-h3", fields: `"seconds":4,"resolution":"720px"`, wantError: "resolution is invalid"},
		{name: "editing inherits source duration", model: "seedance2.5-G", operation: "edit", fields: `"video":"https://example.invalid/source.mp4"`, resolution: "720p", ratio: "adaptive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			operation := tc.operation
			if operation == "" {
				operation = "create"
			}
			mapping := fmt.Sprintf(`{%q:%q}`, publicModel, tc.model)
			channel := model.Channel{Id: 399, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Models: publicModel, Group: "default", Setting: &setting, ModelMapping: &mapping, Key: "test-drama-key"}
			binding, _, err := service.MixedVideoChannelBinding(channel, publicModel, operation)
			require.NoError(t, err)
			profile, err := service.EffectiveMixedVideoProfile(binding, publicModel, service.VideoModelOverrides{})
			require.NoError(t, err)
			raw := []byte(fmt.Sprintf(`{"model":%q,"prompt":"a portrait",%s}`, publicModel, tc.fields))
			normalized, _, body, err := service.NormalizeMixedVideoDefaults(raw, "application/json", operation, []pluginruntime.VideoProfile{profile})
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.resolution, body["resolution"])
			assert.Equal(t, tc.ratio, body["aspect_ratio"])
			candidate, err := service.DecodeMixedVideoCandidate(t.Context(), channel, publicModel, operation, normalized, service.VideoModelOverrides{}, 1, "application/json")
			require.NoError(t, err)
			facts, err := service.MixedVideoPublicFacts(body, operation, nil, profile)
			require.NoError(t, err)
			publicResolution := tc.resolution
			if publicResolution == "720px" {
				publicResolution = "720p"
			}
			assert.Equal(t, publicResolution, facts["resolution"], "the public fact does not change the provider's model-specific wire value")
			descriptor, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": publicModel, "upstreamModel": tc.model, "action": candidate.Intent["action"], "baseUrl": "https://example.invalid", "apiKey": "test-drama-key", "requestBody": candidate.Intent["requestBody"]})
			require.NoError(t, err)
			request, ok := descriptor.(map[string]any)
			require.True(t, ok)
			wire, ok := request["body"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, cmp.Or(tc.wireResolution, tc.resolution), wire["resolution"])
			assert.Equal(t, tc.ratio, wire["aspect_ratio"])
			if tc.resolution == "720px" {
				_, err := service.MixedVideoPublicFacts(body, operation, nil)
				require.ErrorContains(t, err, "resolution is invalid", "720px requires the actual bound model profile")
			}
			if tc.operation == "edit" {
				assert.NotContains(t, body, "seconds")
				assert.NotContains(t, facts, "seconds")
				require.NoError(t, billing_setting.ValidateMixedVideoBillingFacts(`tier("request", u("requests") * 0.5)`, facts))
				require.ErrorContains(t, billing_setting.ValidateMixedVideoBillingFacts(`tier("seconds", u("seconds") * 0.1)`, facts), "cannot be determined")
			}
		})
	}
}

func TestMixedVideoGooglePublicParametersAndUploads(t *testing.T) {
	previousRegistry := pluginruntime.DefaultRegistry
	pluginruntime.DefaultRegistry = pluginruntime.NewRegistry()
	t.Cleanup(func() { pluginruntime.DefaultRegistry = previousRegistry })
	for _, key := range []string{"google", "xai", "sora", "drama"} {
		source, err := builtinplugins.Source(key)
		require.NoError(t, err)
		_, err = pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: key})
		require.NoError(t, err)
	}
	const publicModel = "shared-video-parameters"
	member := func(t *testing.T, key, target string) (model.Channel, pluginruntime.ProtocolBinding, pluginruntime.VideoProfile) {
		t.Helper()
		setting := fmt.Sprintf(`{"task_plugin_key":%q}`, key)
		mapping := fmt.Sprintf(`{%q:%q}`, publicModel, target)
		channel := model.Channel{Id: 399, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Models: publicModel, Group: "default", Setting: &setting, ModelMapping: &mapping}
		binding, _, err := service.MixedVideoChannelBinding(channel, publicModel, "create")
		require.NoError(t, err)
		profile, err := service.EffectiveMixedVideoProfile(binding, publicModel, service.VideoModelOverrides{})
		require.NoError(t, err)
		return channel, binding, profile
	}
	for _, tc := range []struct {
		name, key, target, fields, ratio, wantError string
		seconds                                     int
		decodeError                                 bool
	}{
		{name: "native four seconds", fields: `"metadata":{"durationSeconds":4}`, seconds: 4, ratio: "16:9"},
		{name: "top native seconds", fields: `"durationSeconds":6`, seconds: 6, ratio: "16:9"},
		{name: "matching durations", fields: `"seconds":4,"metadata":{"durationSeconds":4}`, seconds: 4, ratio: "16:9"},
		{name: "conflicting native duration", fields: `"seconds":4,"metadata":{"durationSeconds":8}`, wantError: "conflicting mixed video durations"},
		{name: "native duration zero", fields: `"metadata":{"durationSeconds":0}`, wantError: "bounded positive whole number"},
		{name: "native duration overflow", fields: `"metadata":{"durationSeconds":18446744073709551615}`, wantError: "bounded positive whole number"},
		{name: "portrait pixels", fields: `"size":"720x1280"`, seconds: 8, ratio: "9:16"},
		{name: "matching pixel ratio", fields: `"size":"720x1280","aspect_ratio":"9:16"`, seconds: 8, ratio: "9:16"},
		{name: "conflicting pixel ratio", fields: `"size":"720x1280","aspect_ratio":"16:9"`, wantError: "conflicting mixed video size aspect ratios"},
		{name: "conflicting metadata pixels", fields: `"size":"720x1280","metadata":{"size":"1280x720"}`, wantError: "conflicting mixed video size aspect ratios"},
		{name: "Google square retains native landscape mapping", fields: `"size":"1280x1280"`, seconds: 8, ratio: "16:9"},
		{name: "xAI square retains declared shape", key: "xai", target: "grok-imagine-video-1.5", fields: `"seconds":8,"size":"1280x1280","aspect_ratio":"1:1"`, seconds: 8, ratio: "1:1"},
		{name: "Sora retains size and portrait aspect", key: "sora", target: "sora-2", fields: `"seconds":8,"size":"720x1280"`, seconds: 8, ratio: "9:16"},
		{name: "Drama pixel size remains unsupported", key: "drama", target: "minimax-h3", fields: `"seconds":4,"size":"720x1280"`, wantError: "resolution is not supported", decodeError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, target := tc.key, tc.target
			if key == "" {
				key, target = "google", "veo-3.1-generate-preview"
			}
			channel, binding, profile := member(t, key, target)
			raw := []byte(fmt.Sprintf(`{"model":%q,"prompt":"cat",%s}`, publicModel, tc.fields))
			normalized, _, body, err := service.NormalizeMixedVideoDefaults(raw, "application/json", "create", []pluginruntime.VideoProfile{profile})
			if tc.wantError != "" && !tc.decodeError {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			candidate, err := service.DecodeMixedVideoCandidate(t.Context(), channel, publicModel, "create", normalized, service.VideoModelOverrides{}, 0)
			if tc.decodeError {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			facts, err := service.MixedVideoPublicFacts(body, "create", nil, profile)
			require.NoError(t, err)
			assert.EqualValues(t, tc.seconds, facts["seconds"])
			if tc.ratio == "" {
				assert.NotContains(t, body, "aspect_ratio")
			} else {
				assert.Equal(t, tc.ratio, body["aspect_ratio"])
			}
			ctx := map[string]any{"model": publicModel, "upstreamModel": target, "action": candidate.Intent["action"], "requestBody": candidate.Intent["requestBody"], "baseUrl": "https://example.invalid", "apiKey": "test-only"}
			value, err := binding.Plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, err)
			wire := value.(map[string]any)["body"].(map[string]any)
			if key == "google" {
				parameters := wire["parameters"].(map[string]any)
				assert.EqualValues(t, tc.seconds, parameters["durationSeconds"])
				assert.Equal(t, tc.ratio, parameters["aspectRatio"])
				value, err = binding.Plugin.Engine.Call(t.Context(), "extractUsage", ctx)
				require.NoError(t, err)
				assert.EqualValues(t, facts["seconds"], value.(map[string]any)["seconds"])
			} else if key == "sora" {
				assert.Equal(t, "720x1280", wire["size"])
				assert.NotContains(t, wire, "aspect_ratio")
			} else {
				assert.Equal(t, tc.ratio, wire["aspect_ratio"])
			}
		})
	}
	for _, field := range []string{"image", "input_reference"} {
		t.Run("Google upload "+field, func(t *testing.T) {
			channel, binding, profile := member(t, "google", "veo-3.1-generate-preview")
			var data bytes.Buffer
			writer := multipart.NewWriter(&data)
			require.NoError(t, writer.WriteField("model", publicModel))
			require.NoError(t, writer.WriteField("prompt", "animate this frame"))
			file, err := writer.CreateFormFile(field, "frame.png")
			require.NoError(t, err)
			_, err = file.Write([]byte("image-fixture"))
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			normalized, contentType, _, err := service.NormalizeMixedVideoDefaults(data.Bytes(), writer.FormDataContentType(), "create", []pluginruntime.VideoProfile{profile})
			require.NoError(t, err)
			candidate, err := service.DecodeMixedVideoCandidate(t.Context(), channel, publicModel, "create", normalized, service.VideoModelOverrides{}, 0, contentType)
			require.NoError(t, err)
			view, _, err := service.MixedVideoRequestContext(normalized, contentType, "create")
			require.NoError(t, err)
			value, err := binding.Plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": publicModel, "upstreamModel": "veo-3.1-generate-preview", "requestBody": candidate.Intent["requestBody"], "files": view.Files, "baseUrl": "https://example.invalid"})
			require.NoError(t, err)
			wire := value.(map[string]any)["body"].(map[string]any)
			image := wire["instances"].([]any)[0].(map[string]any)["image"].(map[string]any)
			placeholder := image["inlineData"].(map[string]any)["data"].(map[string]any)
			assert.Equal(t, "request_file:"+field, placeholder["__fileRef"])
		})
	}
	t.Run("uploads cannot bypass count or source limits", func(t *testing.T) {
		_, _, profile := member(t, "google", "veo-3.1-generate-preview")
		for _, fields := range [][]string{{"image", "image"}, {"image", "input_reference"}} {
			var data bytes.Buffer
			writer := multipart.NewWriter(&data)
			require.NoError(t, writer.WriteField("model", publicModel))
			for _, field := range fields {
				file, err := writer.CreateFormFile(field, "frame.png")
				require.NoError(t, err)
				_, err = file.Write([]byte("image-fixture"))
				require.NoError(t, err)
			}
			require.NoError(t, writer.Close())
			_, _, _, err := service.NormalizeMixedVideoDefaults(data.Bytes(), writer.FormDataContentType(), "create", []pluginruntime.VideoProfile{profile})
			require.Error(t, err, "two distinct files remain two inputs even when their field names match")
		}
		_, _, _, err := service.NormalizeMixedVideoDefaults([]byte(fmt.Sprintf(`{"model":%q,"image":{"__fileRef":"request_file:image"}}`, publicModel)), "application/json", "create", []pluginruntime.VideoProfile{profile})
		require.Error(t, err, "a JSON placeholder without an actual upload is not an upload source")
	})
}

func TestPresentTaskSubmissionUsesNativePresenterAfterPersistence(t *testing.T) {
	plugin, err := pluginruntime.CompilePlugin(`
export const meta = {apiVersion:1,key:"presenter-test",name:"Presenter",version:"1.0.0",author:{name:"Test"},models:["model"],fetchMode:"per_task",routes:[{method:"POST",path:"/vendor/jobs",type:"submit",decode:"decode",render:"created"}]};
export const native = {decode:function(ctx){return {kind:"submit",model:"model",requestBody:ctx.body.value};},created:function(ctx,task){return {data:{task_id:task.task_id},upstream:task.data};}};
export function buildSubmitRequest(){return {}} export function parseSubmitResponse(){return {taskId:"upstream"}} export function buildQueryRequest(){return {}} export function parseTaskResult(){return {status:"SUCCESS"}}
`, pluginruntime.Options{})
	require.NoError(t, err)
	priceData := types.PriceData{}
	priceData.AddOtherRatio("seconds", 5)
	task := &model.Task{TaskID: "task_public", SubmitTime: 123}
	task.SetData(map[string]any{"task_id": "upstream_private"})
	outcome := &taskSubmissionOutcome{
		Result:    &relay.TaskSubmitResult{},
		Task:      task,
		RelayInfo: &relaycommon.RelayInfo{PriceData: priceData},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/vendor/jobs", strings.NewReader(`{"model":"model"}`))
	c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Plugin: plugin, Route: plugin.Meta.Routes[0]})
	c.Set(pluginruntime.ContextKeyRouteRequest, pluginruntime.RouteRequestContext{Path: "/vendor/jobs", Method: http.MethodPost, Body: map[string]any{"kind": "json", "value": map[string]any{"model": "model"}}})

	presentTaskSubmission(c, outcome)

	assert.JSONEq(t, `{
		"data":{"task_id":"task_public"},
		"upstream":{"task_id":"upstream_private"}
	}`, recorder.Body.String())
	assert.JSONEq(t, `{"seconds":5}`, recorder.Header().Get("X-New-Api-Other-Ratios"))
}

func TestPresentTaskSubmissionFallbackUsesPersistedPublicID(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	outcome := &taskSubmissionOutcome{
		Result:    &relay.TaskSubmitResult{},
		Task:      &model.Task{TaskID: "task_persisted", SubmitTime: 456},
		RelayInfo: &relaycommon.RelayInfo{OriginModelName: "video-model"},
	}

	presentTaskSubmission(c, outcome)

	assert.JSONEq(t, `{
		"id":"task_persisted",
		"task_id":"task_persisted",
		"status":"queued",
		"model":"video-model",
		"created_at":456
	}`, recorder.Body.String())
}

func TestPresentTaskSubmissionUsesHostOpenAIVideoReceipt(t *testing.T) {
	for _, operation := range []string{"create", "edit", "extend"} {
		t.Run(operation, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Set(pluginruntime.ContextKeyPinnedEndpoint, pluginruntime.PinnedEndpoint{
				Protocol:  "openai_video",
				Operation: pluginruntime.HostProtocolOperation{Name: operation},
			})
			task := &model.Task{
				TaskID:     "task_public",
				Status:     model.TaskStatusSubmitted,
				Progress:   "0%",
				CreatedAt:  456,
				Properties: model.Properties{OriginModelName: "video-model"},
			}
			outcome := &taskSubmissionOutcome{Result: &relay.TaskSubmitResult{}, Task: task, RelayInfo: &relaycommon.RelayInfo{}}

			presentTaskSubmission(c, outcome)

			assert.JSONEq(t, `{"id":"task_public","object":"video","model":"video-model","status":"queued","progress":0,"created_at":456}`, recorder.Body.String())
			assert.NotContains(t, recorder.Body.String(), "task_id")

			task.Status = model.TaskStatusFailure
			task.FailReason = "The request failed because the output video may contain sensitive information."
			failedRecorder := httptest.NewRecorder()
			failedContext, _ := gin.CreateTestContext(failedRecorder)
			failedContext.Set(pluginruntime.ContextKeyPinnedEndpoint, pluginruntime.PinnedEndpoint{
				Protocol:  "openai_video",
				Operation: pluginruntime.HostProtocolOperation{Name: operation},
			})
			presentTaskSubmission(failedContext, outcome)
			var failedVideo relaydto.OpenAIVideo
			require.NoError(t, common.Unmarshal(failedRecorder.Body.Bytes(), &failedVideo))
			assert.Equal(t, "failed", failedVideo.Status)
			require.NotNil(t, failedVideo.Error)
			assert.Contains(t, failedVideo.Error.Message, "敏感")
			assert.Contains(t, failedVideo.Error.Message, task.FailReason)
		})
	}
}

func TestExecuteTaskSubmissionRefundsWhenInsertFails(t *testing.T) {
	events := make([]string, 0, 3)
	database := setupTaskSubmissionDatabase(t, false, &events)
	_ = database
	billing := &taskSubmissionTestBilling{events: &events}
	c := taskSubmissionTestContext()
	info := taskSubmissionRelayInfo(billing)

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		return &relay.TaskSubmitResult{
			UpstreamTaskID: "upstream_private",
			Platform:       constant.TaskPlatform("plugin"),
		}, nil
	})

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "task_insert_failed", taskErr.Code)
	assert.Equal(t, []string{"reserve", "insert", "refund"}, events)
	assert.Equal(t, 1, billing.refunds)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionSettlementFailureStaysDurableAndWritesNothing(t *testing.T) {
	events := make([]string, 0, 3)
	database := setupTaskSubmissionDatabase(t, true, &events)
	billing := &taskSubmissionTestBilling{events: &events, settleErr: errors.New("settlement failed")}
	c := taskSubmissionTestContext()
	info := taskSubmissionRelayInfo(billing)

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		return &relay.TaskSubmitResult{
			UpstreamTaskID: "upstream_private",
			Platform:       constant.TaskPlatform("plugin"),
		}, nil
	})

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "task_billing_settlement_failed", taskErr.Code)
	assert.Equal(t, []string{"reserve", "insert", "settle"}, events)
	assert.Zero(t, billing.refunds)
	var count int64
	require.NoError(t, database.Model(&model.Task{}).Where("task_id = ?", "task_public").Count(&count).Error)
	assert.Equal(t, int64(1), count)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionPersistsPinnedPluginProvenance(t *testing.T) {
	events := make([]string, 0, 3)
	database := setupTaskSubmissionDatabase(t, true, &events)
	previousLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = false
	t.Cleanup(func() { common.LogConsumeEnabled = previousLogConsumeEnabled })

	c := taskSubmissionTestContext()
	c.Set(common.RequestIdKey, "request-public")
	c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{
		Generation: &pluginruntime.RoutingGeneration{Number: 42},
		Plugin: &pluginruntime.LoadedPlugin{Meta: pluginruntime.Meta{
			Key:        "document-parser",
			Name:       "Document Parser",
			Version:    "1.2.3",
			APIVersion: 1,
			Author: pluginruntime.AuthorMeta{
				Name: "Community Author",
				URL:  "https://plugins.example/author",
			},
		}},
	})
	billing := &taskSubmissionTestBilling{events: &events}
	info := taskSubmissionRelayInfo(billing)

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		return &relay.TaskSubmitResult{
			UpstreamTaskID: "upstream-private",
			Platform:       constant.TaskPlatform("document-parser"),
		}, nil
	})

	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	require.NotNil(t, outcome.Task.PrivateData.Execution)
	require.NotNil(t, outcome.Task.PrivateData.Execution.TaskPlugin)
	assert.Equal(t, "request-public", outcome.Task.PrivateData.Execution.RequestID)
	assert.Equal(t, "/plugin/submit", outcome.Task.PrivateData.Execution.RequestPath)
	assert.Equal(t, "1.2.3", outcome.Task.PrivateData.Execution.TaskPlugin.Version)
	assert.Equal(t, uint64(42), outcome.Task.PrivateData.Execution.TaskPlugin.Generation)
	require.NotNil(t, outcome.Task.PrivateData.Execution.TaskPlugin.Author)
	assert.Equal(t, "Community Author", outcome.Task.PrivateData.Execution.TaskPlugin.Author.Name)
	assert.Equal(t, "https://plugins.example/author", outcome.Task.PrivateData.Execution.TaskPlugin.Author.URL)

	var stored model.Task
	require.NoError(t, database.Where("task_id = ?", "task_public").First(&stored).Error)
	require.NotNil(t, stored.PrivateData.Execution)
	require.NotNil(t, stored.PrivateData.Execution.TaskPlugin)
	assert.Equal(t, "document-parser", stored.PrivateData.Execution.TaskPlugin.Key)
	require.NotNil(t, stored.PrivateData.Execution.TaskPlugin.Author)
	assert.Equal(t, "Community Author", stored.PrivateData.Execution.TaskPlugin.Author.Name)
	assert.Equal(t, "upstream-private", stored.PrivateData.UpstreamTaskID)
}

func TestExecuteTaskSubmissionRefundsCancellationBeforeDurableBarrier(t *testing.T) {
	events := make([]string, 0, 2)
	setupTaskSubmissionDatabase(t, true, &events)
	billing := &taskSubmissionTestBilling{events: &events}
	c := taskSubmissionTestContext()
	requestContext, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestContext)
	info := taskSubmissionRelayInfo(billing)

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		cancel()
		return &relay.TaskSubmitResult{
			UpstreamTaskID: "upstream_private",
			Platform:       constant.TaskPlatform("plugin"),
		}, nil
	})

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "request_cancelled", taskErr.Code)
	assert.Equal(t, []string{"refund"}, events)
	assert.Equal(t, 1, billing.refunds)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionDisconnectBeforeUpstreamAcceptanceSkipsSubmitAndRefunds(t *testing.T) {
	events := make([]string, 0, 1)
	setupTaskSubmissionDatabase(t, true, &events)
	billing := &taskSubmissionTestBilling{events: &events}
	c := taskSubmissionTestContext()
	requestContext, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(requestContext)
	info := taskSubmissionRelayInfo(billing)
	submitted := false

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		submitted = true
		return nil, nil
	})

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "request_cancelled", taskErr.Code)
	assert.False(t, submitted)
	assert.Equal(t, []string{"refund"}, events)
	assert.Equal(t, 1, billing.refunds)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionCallerCancellationDuringSubmitRefundsBeforeDurableBarrier(t *testing.T) {
	for _, unconfirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("previous_unknown_%t", unconfirmed), func(t *testing.T) {
			events := make([]string, 0, 1)
			setupTaskSubmissionDatabase(t, true, &events)
			billing := &taskSubmissionTestBilling{events: &events}
			c := taskSubmissionTestContext()
			if unconfirmed {
				c.Set("async_media_initial_retry_request", &service.AsyncVideoRequest{InitialUnknownAttempts: []model.VideoTaskAttempt{{Status: model.TaskStatusUnknown}}})
				c.Set("async_media_submission_rejected", true)
			}
			requestContext, cancel := context.WithCancel(c.Request.Context())
			c.Request = c.Request.WithContext(requestContext)
			info := taskSubmissionRelayInfo(billing)
			submitStarted := make(chan struct{})
			done := make(chan struct{})
			var outcome *taskSubmissionOutcome
			var taskErr *dto.TaskError

			go func() {
				defer close(done)
				outcome, taskErr = executeTaskSubmissionWith(c, info, func(c *gin.Context, _ *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
					close(submitStarted)
					<-c.Request.Context().Done()
					return nil, service.TaskErrorWrapperLocal(c.Request.Context().Err(), "do_request_failed", http.StatusInternalServerError)
				})
			}()
			select {
			case <-submitStarted:
			case <-time.After(2 * time.Second):
				require.FailNow(t, "submission did not start")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				require.FailNow(t, "submission did not stop after disconnect")
			}

			assert.Nil(t, outcome)
			require.NotNil(t, taskErr)
			assert.Equal(t, "request_cancelled", taskErr.Code)
			if unconfirmed {
				assert.Empty(t, events)
				assert.Zero(t, billing.refunds, "disconnect cannot refund an earlier unconfirmed submission")
			} else {
				assert.Equal(t, []string{"refund"}, events)
				assert.Equal(t, 1, billing.refunds)
			}
			assert.False(t, c.Writer.Written())
		})
	}
}

func TestExecuteTaskSubmissionDisconnectAfterDurableInsertDoesNotRefund(t *testing.T) {
	events := make([]string, 0, 3)
	database := setupTaskSubmissionDatabase(t, true, &events)
	previousLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = false
	t.Cleanup(func() { common.LogConsumeEnabled = previousLogConsumeEnabled })
	c := taskSubmissionTestContext()
	requestContext, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestContext)
	billing := &taskSubmissionTestBilling{
		events:   &events,
		onSettle: cancel,
	}
	info := taskSubmissionRelayInfo(billing)

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		return &relay.TaskSubmitResult{
			UpstreamTaskID: "upstream_private",
			Platform:       constant.TaskPlatform("plugin"),
		}, nil
	})

	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	assert.Equal(t, "task_public", outcome.Task.TaskID)
	assert.Equal(t, []string{"reserve", "insert", "settle"}, events)
	assert.Zero(t, billing.refunds)
	var count int64
	require.NoError(t, database.Model(&model.Task{}).Where("task_id = ?", "task_public").Count(&count).Error)
	assert.Equal(t, int64(1), count)
	assert.False(t, c.Writer.Written())
}

func TestAsyncVideoSubmissionRetryKeepsOneBillingLifecycle(t *testing.T) {
	oldLogConsume := common.LogConsumeEnabled
	common.LogConsumeEnabled = false
	t.Cleanup(func() { common.LogConsumeEnabled = oldLogConsume })
	for _, tc := range []struct {
		name                            string
		retry, noRetry, success, cancel bool
		calls                           int
	}{
		{name: "replay succeeds", retry: true, success: true, calls: 2},
		{name: "replay fails and refunds once", retry: true, calls: 2},
		{name: "unclassified failure", calls: 1},
		{name: "no retry protection", retry: true, noRetry: true, calls: 1},
		{name: "cancel before replay", retry: true, cancel: true, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			database := setupTaskSubmissionDatabase(t, true, &events)
			require.NoError(t, database.AutoMigrate(&model.TaskPerfSample{}, &model.AsyncMediaJob{}))
			billing := &taskSubmissionTestBilling{events: &events}
			c := taskSubmissionTestContext()
			c.Set("async_media_execution", true)
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			info := taskSubmissionRelayInfo(billing)
			originalChannel := info.LockedChannel
			calls := 0
			outcome, taskErr := executeTaskSubmissionWith(c, info, func(c *gin.Context, current *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
				return retryAsyncVideoSubmission(c, current, func(c *gin.Context, attempt *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
					calls++
					assert.Same(t, info, attempt)
					assert.Same(t, billing, attempt.Billing, "the existing reservation survives the replay")
					assert.Same(t, originalChannel, attempt.LockedChannel)
					body, err := io.ReadAll(c.Request.Body)
					require.NoError(t, err)
					assert.Equal(t, `{}`, string(body), "rewind the identical original request before replay")
					if calls == 2 && tc.success {
						return &relay.TaskSubmitResult{UpstreamTaskID: "accepted-id", Platform: "drama", Quota: 123}, nil
					}
					c.Set("async_media_retryable_submission_rejection", tc.retry)
					c.Set("async_media_submission_retry_reason", "submission_unknown_duplicate_risk_accepted")
					if tc.cancel {
						cancel()
					}
					failure := service.TaskErrorWrapper(errors.New("task submission state is unknown"), "fail_to_fetch_task", 502)
					failure.NoRetry = tc.noRetry
					return nil, failure
				})
			})
			assert.Equal(t, tc.calls, calls, "submission and replay share one budget")
			var taskCount int64
			require.NoError(t, database.Model(&model.Task{}).Count(&taskCount).Error)
			if tc.success {
				require.Nil(t, taskErr)
				require.NotNil(t, outcome)
				assert.EqualValues(t, 1, taskCount)
				assert.Zero(t, billing.refunds)
				assert.Equal(t, []string{"reserve", "insert", "settle"}, events)
				assert.Equal(t, 123, outcome.Task.Quota)
			} else {
				require.NotNil(t, taskErr)
				assert.Nil(t, outcome)
				assert.Zero(t, taskCount)
				assert.Equal(t, 1, billing.refunds)
				assert.Equal(t, []string{"refund"}, events)
			}
		})
	}
}

func TestTypedInitialVideoFailureUsesOneBillingLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, action, status, receipt, output string
		calls                                 int
		changePolicy, preparationFailure      bool
	}{
		{"typed same", mediaretry.SameChannel, string(model.TaskStatusFailure), "", "", 2, false, false},
		{"typed other", mediaretry.OtherChannel, string(model.TaskStatusFailure), "", "", 2, false, false},
		{"unknown without receipt", mediaretry.OtherChannel, string(model.TaskStatusUnknown), "", "", 1, false, false},
		{"receipt is retained", mediaretry.OtherChannel, string(model.TaskStatusFailure), "original-receipt", "", 1, false, false},
		{"output is retained", mediaretry.OtherChannel, string(model.TaskStatusFailure), "", "https://media.example/result.mp4", 1, false, false},
		{"changed policy advances past failed local preparation", mediaretry.SameChannel, string(model.TaskStatusFailure), "", "", 3, true, false},
		{"inline preparation advances the rule without dispatch", mediaretry.SameChannel, string(model.TaskStatusFailure), "", "", 3, false, true},
		{"xai multipart local retry", mediaretry.LocalSameChannel, string(model.TaskStatusFailure), "", "", 2, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldLogs := common.LogConsumeEnabled
			common.LogConsumeEnabled = false
			t.Cleanup(func() { common.LogConsumeEnabled = oldLogs })
			events := []string{}
			db := setupTaskSubmissionDatabase(t, true, &events)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}, &model.Token{}, &model.Option{}, &model.ImageGroupPolicy{}, &model.ImageChannelPool{}, &model.TokenImagePlatformMapping{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.MediaRetryPolicyVersion{}, &model.MediaRetryEvent{}))
			t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "typed-retry:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
			t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "typed-retry")
			previousRegistry := pluginruntime.DefaultRegistry
			pluginruntime.DefaultRegistry = pluginruntime.NewRegistry()
			t.Cleanup(func() { pluginruntime.DefaultRegistry = previousRegistry })
			provider, modelName := "drama", "seedance2.0-E-720p"
			if tc.action == mediaretry.LocalSameChannel {
				provider, modelName = "xai", "grok-imagine-video"
			}
			source, err := builtinplugins.Source(provider)
			require.NoError(t, err)
			plugin, err := pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: provider})
			require.NoError(t, err)
			user := model.User{Id: 1, Username: "typed-user", Group: "default", Status: common.UserStatusEnabled}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{Id: 1, UserId: 1, Key: "typed-token", Status: common.TokenStatusEnabled, ExpiredTime: -1}
			require.NoError(t, db.Create(&token).Error)
			setting := fmt.Sprintf(`{"task_plugin_key":%q}`, provider)
			channels := []model.Channel{{Id: 1, Type: constant.ChannelTypeTaskPlugin, Key: "first", Models: modelName, Group: "default", Status: common.ChannelStatusEnabled, Setting: &setting}, {Id: 2, Type: constant.ChannelTypeTaskPlugin, Key: "second", Models: modelName, Group: "default", Status: common.ChannelStatusEnabled, Setting: &setting}}
			require.NoError(t, db.Create(&channels).Error)
			for _, channel := range channels {
				require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Model: channel.Models, Group: "default", Enabled: true}).Error)
			}
			policy := mediaretry.Policy{Rules: []mediaretry.Rule{{ID: "typed", Name: "Typed failure", Enabled: true, Match: mediaretry.Match{ContainsAny: []string{"typed rejection"}}, Steps: []mediaretry.Step{{ID: "retry", Action: tc.action, MaxAttempts: 1}}}}}
			if tc.preparationFailure {
				policy.Rules[0].Steps = []mediaretry.Step{{ID: "prepare", Action: mediaretry.SameChannel, MaxAttempts: 1}, {ID: "fallback", Action: mediaretry.SameChannel, MaxAttempts: 1}}
			}
			_, err = model.SaveMediaRetryPolicy(t.Context(), policy, 0, 1, nil)
			require.NoError(t, err)
			now := time.Now().Unix()
			job := model.AsyncMediaJob{TaskId: "task_public", IdentityHash: "typed", UserId: 1, TokenId: 1, ChannelId: 1, Provider: provider, Model: channels[0].Models, Group: "default", Status: "queued", LeaseToken: "lease", LeaseExpiresAt: now + 120, CreatedAt: now, ExpiresAt: now + 120}
			require.NoError(t, db.Create(&job).Error)
			request := service.AsyncVideoRequest{Body: []byte(`{}`), ContentType: "application/json", Channel: channels[0], PluginHash: plugin.Engine.SourceFingerprint(), InitialRetryState: &mediaretry.State{TotalLimit: 3}}
			if provider == "xai" {
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				require.NoError(t, writer.WriteField("model", modelName))
				require.NoError(t, writer.WriteField("image", "https://cdn.example.com/reference.png"))
				require.NoError(t, writer.Close())
				request.Body, request.ContentType = body.Bytes(), writer.FormDataContentType()
				request.Execution = &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: provider}}
				request.ReferenceRecoveryVersion, request.ReferenceRecoveryDeadline = 2, now+120
				request.InlineReferenceInputs = []service.AsyncVideoReferenceInput{{Kind: "image", Source: "https://cdn.example.com/reference.png", Path: "/image/url"}}
			}
			if tc.changePolicy {
				// Generation is still allowed, but local recovery's earlier
				// deadline has expired. Its failure must advance to the fallback.
				request.Body = []byte(`{"images":["https://reference.example/image.png"]}`)
				request.ReferenceRecoveryVersion, request.ReferenceRecoveryDeadline, request.ReferenceURLExpiresAt = 2, now-1, now+120
				request.ReferenceInputs = []service.AsyncVideoReferenceInput{{Index: 0, Path: "/images/0", Source: "https://reference.example/image.png", Kind: "image"}}
			}
			retry := &asyncVideoSubmissionRetry{job: &job, request: &request, liveChannel: &channels[0], startedAt: now}
			c := taskSubmissionTestContext()
			defer common.CleanupBodyStorage(c)
			require.Nil(t, middleware.SetupContextForSelectedChannel(c, &request.Channel, job.Model))
			c.Set("async_media_initial_billing_channel_id", 1)
			c.Set("async_media_initial_retry_request", &request)
			billing := &taskSubmissionTestBilling{events: &events}
			info := taskSubmissionRelayInfo(billing)
			info.OriginModelName = job.Model
			info.UpstreamModelName = job.Model
			info.TokenId = 1
			events = nil
			calls := 0
			outcome, taskErr := executeTaskSubmissionWith(c, info, func(c *gin.Context, info *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
				return retryAsyncVideoSubmission(c, info, func(c *gin.Context, attempt *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
					assert.Same(t, billing, attempt.Billing)
					attempt.InitChannelMeta(c)
					calls++
					if tc.preparationFailure && calls <= 2 {
						request.InlineReferenceInputs = []service.AsyncVideoReferenceInput{{Kind: "image", Source: "https://cdn.example.com/ref.png", Path: "/image/url"}}
						err := &service.MediaReferencePreparationError{Code: "media_reference_download_failed", Status: 502, Cause: errors.New("typed rejection: reference download failed")}
						return nil, service.TaskErrorWrapperLocal(err, err.Code, err.Status)
					}
					if provider == "xai" && calls == 2 {
						body, err := io.ReadAll(c.Request.Body)
						require.NoError(t, err)
						assert.Equal(t, request.Body, body, "local retry preserves the multipart source for normalization")
						assert.Contains(t, request.ContentType, "multipart/form-data")
					}

					if tc.changePolicy && calls == 2 {
						latest, err := model.GetMediaRetryPolicy(t.Context())
						require.NoError(t, err)
						latest.Rules[0].Steps = []mediaretry.Step{{ID: "local", Action: mediaretry.LocalSameChannel, MaxAttempts: 1}, {ID: "fallback", Action: mediaretry.SameChannel, MaxAttempts: 1}}
						_, err = model.SaveMediaRetryPolicy(t.Context(), latest, latest.Revision, 1, nil)
						require.NoError(t, err)
						err = retry.dispatch(c, attempt)
						require.ErrorIs(t, err, service.ErrVideoRetryDeferred)
						return nil, service.TaskErrorWrapperLocal(err, "media_dispatch_failed", http.StatusInternalServerError)
					}
					require.NoError(t, retry.dispatch(c, attempt))
					if calls == 1 {
						quota := 123
						if tc.status == string(model.TaskStatusFailure) {
							quota = 0
						}
						return &relay.TaskSubmitResult{Platform: "drama", Quota: quota, UpstreamTaskID: tc.receipt, Immediate: &relaycommon.TaskInfo{Status: tc.status, Reason: "typed rejection", Url: tc.output}}, nil
					}
					return &relay.TaskSubmitResult{Platform: "drama", Quota: 123, UpstreamTaskID: "accepted-retry"}, nil
				}, retry)
			})
			require.Nil(t, taskErr)
			require.NotNil(t, outcome)
			assert.Equal(t, tc.calls, calls)
			assert.Zero(t, billing.refunds)
			assert.Equal(t, 1, outcome.Task.BillingChannelID())
			if tc.preparationFailure {
				assert.Equal(t, 1, request.InitialRetryState.TotalUsed)
				assert.Equal(t, map[string]int{"typed/prepare": 1, "typed/fallback": 1}, request.InitialRetryState.StepUses)
				require.NotNil(t, request.RetryFact)
				assert.Equal(t, "reference_download", request.RetryFact.Category)
				assert.Equal(t, "submit", request.RetryFact.Stage)
				assert.False(t, request.RetryFact.Unknown)
			} else if tc.changePolicy {
				assert.Equal(t, 1, request.InitialRetryState.TotalUsed, "only the fallback generated another upstream request")
				assert.Equal(t, map[string]int{"typed/local": 1, "typed/fallback": 1}, request.InitialRetryState.StepUses)
				assert.Equal(t, "accepted-retry", outcome.Task.PrivateData.UpstreamTaskID)
			} else {
				assert.Equal(t, tc.calls-1, request.InitialRetryState.TotalUsed)
			}
			assert.Equal(t, 1, strings.Count(strings.Join(events, ","), "settle"))
		})
	}
}

func TestDurableVideoSubmissionReservationReconciliationDatabase(t *testing.T) {
	t.Run("pre-consumption validation and existing holds", func(t *testing.T) {
		user, token := setupResponsesWSRequestTest(t)
		db := setupVideoRetryTestDatabase(t, user, token)
		require.NoError(t, db.AutoMigrate(&model.TaskPerfSample{}, &model.AsyncMediaJob{}))
		for _, tc := range []struct {
			name, validation, billingStatus      string
			priorUnknown, dispatched, staleLease bool
			reservedQuota                        int
		}{
			{name: "negative reservation", billingStatus: "not_billable"},
			{name: "invalid zero mixed snapshot", validation: "invalid_zero", billingStatus: "not_billable"},
			{name: "zero reservation for nonzero mixed price", validation: "nonzero_mixed", billingStatus: "not_billable"},
			{name: "previous unknown attempt retains reconciliation", priorUnknown: true, billingStatus: "reserving"},
			{name: "persisted dispatch retains reconciliation", dispatched: true, billingStatus: "reserving"},
			{name: "persisted quota retains reconciliation", reservedQuota: 100, billingStatus: "reserving"},
			{name: "stale worker cannot close another reservation", staleLease: true, billingStatus: "reserving"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: user.Id, TokenId: token.Id, Status: "submitting", BillingStatus: "reserving", Quota: tc.reservedQuota, LeaseToken: "current-lease"}
				if tc.dispatched {
					job.DispatchedAt = time.Now().Unix()
				}
				require.NoError(t, db.Create(&job).Error)
				c := taskSubmissionTestContext()
				c.Set("async_media_execution", true)
				c.Set("async_media_execution_lease_token", job.LeaseToken)
				if tc.staleLease {
					c.Set("async_media_execution_lease_token", "expired-lease")
				}
				if tc.priorUnknown {
					c.Set("async_media_initial_retry_request", &service.AsyncVideoRequest{InitialUnknownAttempts: []model.VideoTaskAttempt{{Status: model.TaskStatusUnknown}}})
				}
				info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: job.TaskId}}
				quota := -1
				if tc.validation != "" {
					quota = 0
					info.TieredBillingSnapshot = &billingexpr.BillingSnapshot{MixedVideoBilling: true}
					if tc.validation == "nonzero_mixed" {
						const expression = `tier("request", u("requests"))`
						info.TieredBillingSnapshot = &billingexpr.BillingSnapshot{ModelName: "public-video", ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), ExprVersion: billingexpr.ExprVersion(expression), GroupRatio: 1, QuotaPerUnit: 1, EstimatedQuotaAfterGroup: 1, TaskUsageBilling: true, MixedVideoBilling: true, MixedVideoSchemaVersion: billing_setting.MixedVideoBillingSchemaVersion, UsageFacts: map[string]any{"requests": float64(1), "action": "generate"}}
						require.NoError(t, service.ValidateFrozenMixedVideoBilling(info.TieredBillingSnapshot))
					}
				}
				apiErr := service.PreConsumeDurableTaskBilling(c, quota, info)
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
				assert.Nil(t, info.Billing)
				require.NoError(t, db.First(&job, job.ID).Error)
				assert.Equal(t, tc.billingStatus, job.BillingStatus)
				assert.Equal(t, tc.reservedQuota, job.Quota)
				require.NoError(t, db.First(user, user.Id).Error)
				require.NoError(t, db.First(token, token.Id).Error)
				assert.Equal(t, 1000, user.Quota)
				assert.Equal(t, 100, token.RemainQuota)
			})
		}
	})
	t.Run("reclaimed submission preserves billing outcomes", func(t *testing.T) {
		user, token := setupResponsesWSRequestTest(t)
		db := setupVideoRetryTestDatabase(t, user, token)
		require.NoError(t, db.AutoMigrate(&model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.Task{}, &model.AsyncMediaEvent{}))
		t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "reclaimed-video:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
		t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "reclaimed-video")
		previousSubmit := service.SubmitAsyncVideoFunc
		t.Cleanup(func() { service.SubmitAsyncVideoFunc = previousSubmit })
		for _, tc := range []struct {
			name, billing, status, wantStatus string
			staleLease                        bool
			wantSubmits                       int
		}{
			{name: "failed compensation", billing: "failed", status: "submitting", wantStatus: "failed"},
			{name: "uncertain accounting", billing: "unknown", status: "submitting", wantStatus: "failed"},
			{name: "confirmed refund", billing: "refunded", status: "submitting", wantStatus: "failed"},
			{name: "confirmed absence of charge", billing: "not_billable", status: "submitting", wantStatus: "failed"},
			{name: "stale worker cannot close reclaimed lease", billing: "failed", status: "submitting", wantStatus: "submitting", staleLease: true},
			{name: "ordinary queued submission", billing: "pending", status: "queued", wantStatus: "submitted", wantSubmits: 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				now := time.Now().Unix()
				job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: user.Id, TokenId: token.Id, Status: tc.status, BillingStatus: tc.billing, StorageStatus: "pending", LeaseToken: "interrupted-lease", LeaseExpiresAt: now - 1, CreatedAt: now, ExpiresAt: now + 86400}
				var err error
				job.RequestCipher, err = service.EncryptImagePayload([]byte(`{}`), "media-request:"+job.TaskId)
				require.NoError(t, err)
				require.NoError(t, db.Create(&job).Error)
				eligible, err := model.GetDueAsyncMediaJobsForPhase(t.Context(), now, 100, "submit")
				require.NoError(t, err)
				require.True(t, slices.ContainsFunc(eligible, func(candidate model.AsyncMediaJob) bool { return candidate.ID == job.ID }))
				stale := job
				claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "recovered-lease", 120)
				require.NoError(t, err)
				require.True(t, claimed)
				var submits int
				service.SubmitAsyncVideoFunc = func(context.Context, model.AsyncMediaJob, service.AsyncVideoRequest) error {
					submits++
					return nil
				}
				workerJob := job
				if tc.staleLease {
					workerJob = stale
				}
				require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), workerJob, service.DefaultMediaRuntimeConfig()))
				require.NoError(t, db.First(&job, job.ID).Error)
				assert.Equal(t, tc.wantSubmits, submits)
				assert.Equal(t, tc.wantStatus, job.Status)
				assert.Equal(t, tc.billing, job.BillingStatus)
				assert.Zero(t, job.Quota)
				assert.Zero(t, job.DispatchedAt)
				if tc.staleLease {
					assert.Equal(t, "recovered-lease", job.LeaseToken)
				} else {
					assert.Empty(t, job.LeaseToken)
				}
			})
		}
	})
	t.Run("dispatched reservation failure retains separate financial holds", func(t *testing.T) {
		for _, staleLease := range []bool{false, true} {
			t.Run(fmt.Sprintf("stale lease %t", staleLease), func(t *testing.T) {
				user, token := setupResponsesWSRequestTest(t)
				db := setupVideoRetryTestDatabase(t, user, token)
				require.NoError(t, db.AutoMigrate(&model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.Task{}, &model.AsyncMediaEvent{}, &model.Log{}))
				require.NoError(t, db.Model(token).Update("remain_quota", 40).Error)
				t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "reserve-failure:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
				t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "reserve-failure")
				previousBatch := common.BatchUpdateEnabled
				common.BatchUpdateEnabled = false
				t.Cleanup(func() { common.BatchUpdateEnabled = previousBatch })
				now := time.Now().Unix()
				request := service.AsyncVideoRequest{Body: []byte(`{"model":"plugin-model","prompt":"retain original input"}`), ContentType: "application/json", Operation: "create"}
				raw, err := common.Marshal(request)
				require.NoError(t, err)
				job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: user.Id, TokenId: token.Id, ChannelId: 1, Provider: "drama", Model: "plugin-model", Status: "submitting", BillingStatus: "reserved", Quota: 40, LeaseToken: "dispatch-lease", LeaseExpiresAt: now + 120, CreatedAt: now, ExpiresAt: now + 86400}
				job.RequestCipher, err = service.EncryptImagePayload(raw, "media-request:"+job.TaskId)
				require.NoError(t, err)
				require.NoError(t, db.Create(&job).Error)
				originalCipher := bytes.Clone(job.RequestCipher)
				c := taskSubmissionTestContext()
				defer common.CleanupBodyStorage(c)
				c.Set("async_media_execution", true)
				c.Set("async_media_execution_lease_token", job.LeaseToken)
				c.Set("async_media_initial_retry_request", &request)
				info := taskSubmissionRelayInfo(nil)
				info.UserId, info.TokenId, info.TokenKey, info.PublicTaskID = user.Id, token.Id, token.Key, job.TaskId
				info.UserSetting.BillingPreference = "wallet_only"
				require.Nil(t, service.PreConsumeDurableTaskBilling(c, 40, info))
				var submissions int
				outcome, taskErr := executeTaskSubmissionWith(c, info, func(c *gin.Context, _ *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
					submissions++
					updates := map[string]any{"dispatched_at": now}
					if staleLease {
						updates["lease_token"] = "replacement-lease"
					}
					require.NoError(t, db.Model(&job).Updates(updates).Error)
					c.Set("async_media_reconciliation_dispatched", true)
					return &relay.TaskSubmitResult{Platform: "drama", Quota: 60, UpstreamTaskID: "accepted-before-topup"}, nil
				})
				require.Nil(t, outcome)
				require.NotNil(t, taskErr)
				assert.Equal(t, http.StatusForbidden, taskErr.StatusCode)
				assert.Equal(t, 1, submissions)
				assert.Error(t, info.Billing.Reserve(60), "the partial reservation cannot be charged again")
				assert.Error(t, info.Billing.Settle(40), "the old paired quota cannot hide the failed top-up")
				assert.False(t, service.HasDurableTaskReservation(info, 40))
				require.NoError(t, db.First(user, user.Id).Error)
				require.NoError(t, db.First(token, token.Id).Error)
				assert.Equal(t, 940, user.Quota, "accepted execution retains the confirmed 60-unit funding hold")
				assert.Zero(t, token.RemainQuota)
				assert.Equal(t, 40, token.UsedQuota, "only the original paired token reservation succeeded")
				require.NoError(t, db.First(&job, job.ID).Error)
				assert.Equal(t, 40, job.Quota, "a partial hold must not replace the paired settlement quota")
				assert.Equal(t, int64(now), job.DispatchedAt)
				var taskCount, refundCount int64
				require.NoError(t, db.Model(&model.Task{}).Where("task_id = ?", job.TaskId).Count(&taskCount).Error)
				require.NoError(t, db.Model(&model.Log{}).Where("user_id = ? AND type = ?", user.Id, model.LogTypeRefund).Count(&refundCount).Error)
				assert.Zero(t, taskCount, "partial accounting must not enter normal task settlement")
				assert.Zero(t, refundCount)
				decoded, err := service.DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
				require.NoError(t, err)
				var retained service.AsyncVideoRequest
				require.NoError(t, common.Unmarshal(decoded, &retained))
				assert.Equal(t, request.Body, retained.Body)
				assert.Equal(t, request.ContentType, retained.ContentType)
				if staleLease {
					assert.Equal(t, "replacement-lease", job.LeaseToken)
					assert.Equal(t, "reserved", job.BillingStatus)
					assert.Equal(t, originalCipher, job.RequestCipher)
					assert.Nil(t, retained.ReservationFailure)
					return
				}
				assert.Equal(t, "failed", job.BillingStatus)
				require.NotNil(t, retained.ReservationFailure)
				assert.Equal(t, service.BillingSourceWallet, retained.ReservationFailure.FundingSource)
				assert.Equal(t, 60, retained.ReservationFailure.ConfirmedFundingQuota)
				assert.Equal(t, 40, retained.ReservationFailure.ConfirmedTokenQuota)
				assert.Equal(t, 40, retained.ReservationFailure.PairedQuota)
				assert.Equal(t, 60, retained.ReservationFailure.AttemptedQuota)
				assert.Equal(t, "token_reserve", retained.ReservationFailure.Stage)
				assert.False(t, retained.ReservationFailure.Uncertain)
			})
		}
	})
	for _, provider := range []string{"nldw", "yuxibox"} {
		t.Run(provider, func(t *testing.T) {
			modelName := "sd2.5pp"
			if provider == "yuxibox" {
				modelName = "seedance-2.5-yx-sz"
			}
			baseUser, baseToken := setupResponsesWSRequestTest(t)
			db := setupVideoRetryTestDatabase(t, baseUser, baseToken)
			versionQuery := "SELECT version()"
			if db.Dialector.Name() == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			var databaseVersion string
			require.NoError(t, db.Raw(versionQuery).Scan(&databaseVersion).Error)
			t.Logf("reservation reconciliation database: %s %s", db.Dialector.Name(), databaseVersion)
			// The shared cross-database fixture inserts this unused seed with an
			// explicit ID. Remove it so PostgreSQL can generate the first test token.
			require.NoError(t, db.Unscoped().Delete(baseToken).Error)
			require.NoError(t, db.AutoMigrate(&model.UserSubscription{}, &model.Channel{}, &model.Ability{}, &model.Task{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.Log{}, &model.Option{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.MediaArtifactObject{}, &model.MediaRetryPolicyVersion{}, &model.MediaRetryEvent{}, &model.AsyncImageUsageProjection{}, &model.AsyncImageLogReceipt{}, &model.CanvasGrant{}))
			require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}, &model.SubscriptionPreConsumeRecord{}))
			t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "nldw-submit:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
			t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "nldw-submit")
			previousRegistry := pluginruntime.DefaultRegistry
			pluginruntime.DefaultRegistry = pluginruntime.NewRegistry()
			t.Cleanup(func() { pluginruntime.DefaultRegistry = previousRegistry })
			source, err := builtinplugins.Source(provider)
			require.NoError(t, err)
			// Leave descriptor validation intact, then fail in the real host
			// serializer when a plugin references an unavailable upload.
			source = strings.Replace(source, "export function buildSubmitRequest(ctx) {", `export function buildSubmitRequest(ctx) {
  if (ctx.requestBody.prompt === "missing file reference") return {method:"POST",url:ctx.baseUrl+"/v1/videos",body:{image:{__fileRef:"request_file:missing",encoding:"base64"}}};`, 1)
			plugin, err := pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: provider})
			require.NoError(t, err)
			previousSubmit := service.SubmitAsyncVideoFunc
			service.SubmitAsyncVideoFunc = ExecuteAsyncVideo
			t.Cleanup(func() { service.SubmitAsyncVideoFunc = previousSubmit })
			previousLogs, previousBatch := common.LogConsumeEnabled, common.BatchUpdateEnabled
			common.LogConsumeEnabled, common.BatchUpdateEnabled = false, false
			t.Cleanup(func() { common.LogConsumeEnabled, common.BatchUpdateEnabled = previousLogs, previousBatch })
			for index, tc := range []struct {
				name, body                           string
				httpStatus                           int
				lostReceipt, beforeSend, insertFails bool
				buildFails                           bool
				wantCalls, wantWallet                int
				wantStatus, wantBilling              string
				configured                           string
				billingFailure                       string
				wantToken                            int
				subscriptionSuccess                  bool
			}{
				{name: "accepted", body: `{"id":"nldw-accepted","status":"queued"}`, httpStatus: 200, wantCalls: 1, wantWallet: 900, wantStatus: "submitted", wantBilling: "reserved"},
				{name: "subscription accepted reserves only token and subscription", subscriptionSuccess: true, body: `{"id":"subscription-accepted","status":"queued"}`, httpStatus: 200, wantCalls: 1, wantWallet: 1000, wantToken: 0, wantStatus: "submitted", wantBilling: "reserved"},
				{name: "receipt lost after acceptance", lostReceipt: true, wantCalls: 1, wantWallet: 900, wantStatus: "execution_unknown", wantBilling: "unknown"},
				{name: "receipt has no id", body: `{"status":"queued"}`, httpStatus: 200, wantCalls: 1, wantWallet: 900, wantStatus: "execution_unknown", wantBilling: "unknown"},
				{name: "malformed receipt", body: `{"id":`, httpStatus: 200, wantCalls: 1, wantWallet: 900, wantStatus: "execution_unknown", wantBilling: "unknown"},
				{name: "temporary upstream failure", body: `{"error":{"message":"temporary failure"}}`, httpStatus: 500, wantCalls: 1, wantWallet: 900, wantStatus: "execution_unknown", wantBilling: "unknown"},
				{name: "bad gateway without receipt", body: `{"error":{"message":"upstream response lost"}}`, httpStatus: 502, wantCalls: 1, wantWallet: 900, wantStatus: "execution_unknown", wantBilling: "unknown"},
				{name: "bad gateway with accepted receipt", body: `{"id":"accepted-task","status":"queued","error":{"message":"response incomplete"}}`, httpStatus: 502, wantCalls: 1, wantWallet: 900, wantStatus: "submitted", wantBilling: "reserved"},
				{name: "submission unknown gateway error refunds", body: `{"error":{"message":"task submission state is unknown","type":"upstream_error"}}`, httpStatus: 502, wantCalls: 1, wantWallet: 1000, wantStatus: "failed", wantBilling: "refunded"},
				{name: "no upstream channel refunds", body: `{"error":{"message":"No available channel for model video under group example (distributor) (request id: example)","type":"new_api_error","code":"model_not_found"}}`, httpStatus: 503, wantCalls: 1, wantWallet: 1000, wantStatus: "failed", wantBilling: "refunded"},
				{name: "refundable submissions exhaust configured retry", body: `{"error":{"message":"task submission state is unknown","type":"upstream_error"}}`, httpStatus: 502, configured: "refundable_retry", wantCalls: 2, wantWallet: 1000, wantStatus: "failed", wantBilling: "refunded"},
				{name: "refundable submission cannot clear an earlier unrelated unknown", body: `{"error":{"message":"temporary failure"}}`, httpStatus: 500, configured: "unrelated_first", wantCalls: 2, wantWallet: 900, wantStatus: "failed", wantBilling: "unknown"},
				{name: "explicit refusal", body: `{"error":{"code":"invalid_request","message":"unsupported prompt"}}`, httpStatus: 400, wantCalls: 1, wantWallet: 1000, wantStatus: "failed", wantBilling: "refunded"},
				{name: "dispatch barrier rejected", beforeSend: true, wantWallet: 1000, wantStatus: "failed", wantBilling: "refunded"},
				{name: "custom retry cannot turn a dispatch barrier rejection into unknown execution", beforeSend: true, configured: "before_send", wantWallet: 1000, wantStatus: "failed", wantBilling: "refunded"},
				{name: "request body construction failure does not enter upstream retries", buildFails: true, configured: "build_request", body: `{"id":"unexpected-receipt","status":"queued"}`, httpStatus: 200, wantWallet: 1000, wantStatus: "failed", wantBilling: "refunded"},
				{name: "token rejection has no financial hold", billingFailure: "token_short", wantWallet: 1000, wantToken: 50, wantStatus: "failed", wantBilling: "not_billable"},
				{name: "custom retry cannot repeat a token rejection", billingFailure: "token_short", configured: "preconsume", wantWallet: 1000, wantToken: 50, wantStatus: "failed", wantBilling: "not_billable"},
				{name: "wallet rejection has no financial hold", billingFailure: "wallet_short", configured: "preconsume", wantWallet: 50, wantToken: 100, wantStatus: "failed", wantBilling: "not_billable"},
				{name: "atomic wallet rejection restores token debit", billingFailure: "wallet_race", configured: "preconsume", wantWallet: 50, wantToken: 100, wantStatus: "failed", wantBilling: "not_billable"},
				{name: "failed token rollback cannot try another funding source", billingFailure: "rollback", configured: "preconsume", wantWallet: 50, wantToken: 150, wantStatus: "failed", wantBilling: "failed"},
				{name: "token write error keeps uncertain billing", billingFailure: "token_db", configured: "preconsume", wantWallet: 1000, wantToken: 100, wantStatus: "failed", wantBilling: "unknown"},
				{name: "wallet write error keeps uncertain billing", billingFailure: "wallet_db", configured: "preconsume", wantWallet: 1000, wantToken: 100, wantStatus: "failed", wantBilling: "unknown"},
				{name: "subscription absence restores token debit", billingFailure: "subscription_absent", configured: "preconsume", wantWallet: 1000, wantToken: 100, wantStatus: "failed", wantBilling: "not_billable"},
				{name: "subscription exhaustion restores token debit", billingFailure: "subscription_short", configured: "preconsume", wantWallet: 1000, wantToken: 100, wantStatus: "failed", wantBilling: "not_billable"},
				{name: "subscription query error keeps uncertain billing", billingFailure: "subscription_db", configured: "preconsume", wantWallet: 1000, wantToken: 100, wantStatus: "failed", wantBilling: "unknown"},
				{name: "clamped frozen price has no financial hold", billingFailure: "clamp", wantWallet: 1000, wantToken: 100, wantStatus: "failed", wantBilling: "not_billable"},
				{name: "accepted receipt cannot persist", body: `{"id":"nldw-accepted","status":"queued"}`, httpStatus: 200, insertFails: true, wantCalls: 1, wantWallet: 900, wantStatus: "execution_unknown", wantBilling: "unknown"},
				{name: "custom unknown then explicit refusal retains hold", body: `{"error":{"message":"temporary failure"}}`, httpStatus: 500, configured: "rejection", wantCalls: 2, wantWallet: 900, wantStatus: "failed", wantBilling: "unknown"},
				{name: "custom unknown then explicit refusal cannot persist retains hold", body: `{"error":{"message":"temporary failure"}}`, httpStatus: 500, configured: "rejection", insertFails: true, wantCalls: 2, wantWallet: 900, wantStatus: "execution_unknown", wantBilling: "unknown"},
				{name: "custom unknown retry budget stop retains hold", body: `{"error":{"message":"temporary failure"}}`, httpStatus: 500, configured: "budget", wantCalls: 2, wantWallet: 900, wantStatus: "failed", wantBilling: "unknown"},
				{name: "custom unknown local preparation stop retains hold", body: `{"error":{"message":"temporary failure"}}`, httpStatus: 500, configured: "local", wantCalls: 1, wantWallet: 900, wantStatus: "failed", wantBilling: "unknown"},
			} {
				if tc.lostReceipt && provider == "yuxibox" {
					tc.name = "transport submission failure refunds"
					tc.wantWallet, tc.wantStatus, tc.wantBilling = 1000, "failed", "refunded"
				}
				t.Run(tc.name, func(t *testing.T) {
					user := model.User{Username: fmt.Sprintf("nldw-submission-%d", index), AffCode: common.GetUUID(), Status: common.UserStatusEnabled, Group: "default", Quota: 1000, AuthVersion: 1}
					if tc.billingFailure == "wallet_short" {
						user.Quota = 50
					}
					if tc.billingFailure == "rollback" {
						user.SetSetting(relaydto.UserSetting{BillingPreference: "wallet_first"})
					}
					if tc.subscriptionSuccess || strings.HasPrefix(tc.billingFailure, "subscription_") {
						user.SetSetting(relaydto.UserSetting{BillingPreference: "subscription_only"})
					}
					require.NoError(t, db.Create(&user).Error)
					var subscription model.UserSubscription
					if tc.subscriptionSuccess || tc.billingFailure == "subscription_short" {
						plan := model.SubscriptionPlan{Title: "Submission subscription", QuotaResetPeriod: model.SubscriptionResetNever}
						require.NoError(t, db.Create(&plan).Error)
						subscription = model.UserSubscription{UserId: user.Id, PlanId: plan.Id, AmountTotal: 100, AmountUsed: 50, StartTime: time.Now().Unix() - 60, EndTime: time.Now().Unix() + 3600, Status: "active"}
						if tc.subscriptionSuccess {
							subscription.AmountUsed = 0
						}
						require.NoError(t, db.Create(&subscription).Error)
					}
					token := model.Token{UserId: user.Id, Key: common.GetUUID(), Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100}
					if tc.billingFailure == "token_short" {
						token.RemainQuota = 50
					} else if tc.billingFailure == "rollback" {
						token.RemainQuota = 250
					}
					require.NoError(t, db.Create(&token).Error)
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						assert.Equal(t, "/v1/videos", r.URL.Path)
						assert.Equal(t, "Bearer upstream-test-key", r.Header.Get("Authorization"))
						if provider == "nldw" {
							assert.NotEmpty(t, r.Header.Get("Idempotency-Key"))
						}
						_, readErr := io.Copy(io.Discard, r.Body)
						assert.NoError(t, readErr)
						if tc.lostReceipt {
							connection, _, hijackErr := w.(http.Hijacker).Hijack()
							if assert.NoError(t, hijackErr) {
								assert.NoError(t, connection.Close())
							}
							return
						}
						w.Header().Set("Content-Type", "application/json")
						if tc.configured == "rejection" && calls.Load() > 1 {
							w.WriteHeader(http.StatusBadRequest)
							_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"unsupported prompt"}}`))
							return
						}
						if tc.configured == "unrelated_first" && calls.Load() > 1 {
							w.WriteHeader(http.StatusBadGateway)
							_, _ = w.Write([]byte(`{"error":{"message":"task submission state is unknown"}}`))
							return
						}
						w.WriteHeader(tc.httpStatus)
						_, _ = w.Write([]byte(tc.body))
					}))
					t.Cleanup(upstream.Close)
					setting := fmt.Sprintf(`{"task_plugin_key":%q}`, provider)
					channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: provider + " fixture", Key: "upstream-test-key", BaseURL: &upstream.URL, Models: modelName, Group: "default", Status: common.ChannelStatusEnabled, Setting: &setting}
					require.NoError(t, db.Create(&channel).Error)
					require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Group: "default", Model: modelName, Enabled: true}).Error)
					now := time.Now().Unix()
					job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Provider: provider, Model: modelName, Group: "default", Status: "queued", BillingStatus: "pending", StorageStatus: "pending", LeaseToken: "video-lease", LeaseExpiresAt: now + 120, CreatedAt: now, ExpiresAt: now + 86400}
					request := service.AsyncVideoRequest{Body: []byte(fmt.Sprintf(`{"model":%q,"prompt":"test generation","seconds":5,"resolution":"720p"}`, modelName)), ContentType: "application/json", ClientIP: "203.0.113.10", Channel: channel, PluginHash: plugin.Engine.SourceFingerprint(), Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: provider, Version: plugin.Meta.Version, APIVersion: 1}}, Billing: service.MediaBillingSnapshot{Price: types.PriceData{Quota: 100, QuotaToPreConsume: 100}}}
					if tc.buildFails {
						request.Body = bytes.Replace(request.Body, []byte("test generation"), []byte("missing file reference"), 1)
					}
					if tc.billingFailure == "clamp" {
						request.Billing.Clamp = &common.QuotaClamp{Op: "QuotaRound", Kind: common.QuotaClampOverflow, Original: 3e9, Clamped: 2147483647}
					}
					if tc.configured != "" {
						policy, err := model.GetMediaRetryPolicy(t.Context())
						require.NoError(t, err)
						action := mediaretry.SameChannel
						if tc.configured == "local" {
							action = mediaretry.LocalSameChannel
							request.ReferenceRecoveryVersion, request.ReferenceRecoveryDeadline = 2, now+120
							request.ReferenceInputs = []service.AsyncVideoReferenceInput{{Index: 0, Path: "/images/0", Source: "https://reference.example/image.png", Kind: "image"}}
						}
						statuses := []int{500}
						if tc.configured == "preconsume" {
							statuses = []int{403, 500}
						} else if tc.configured == "refundable_retry" {
							statuses = []int{502}
						}
						policy.Rules = []mediaretry.Rule{{ID: "custom-unknown", Name: "Custom unknown", Enabled: true, Scope: mediaretry.Scope{ChannelIDs: []int{channel.Id}}, Match: mediaretry.Match{HTTPStatuses: statuses}, Steps: []mediaretry.Step{{ID: "retry", Action: action, MaxAttempts: 1}}, OnExhausted: mediaretry.Stop}}
						_, err = model.SaveMediaRetryPolicy(t.Context(), policy, policy.Revision, 1, nil)
						require.NoError(t, err)
					}
					encoded, err := common.Marshal(request)
					require.NoError(t, err)
					job.RequestCipher, err = service.EncryptImagePayload(encoded, "media-request:"+job.TaskId)
					require.NoError(t, err)
					require.NoError(t, db.Create(&job).Error)
					var barrierAttempts atomic.Int32
					var billingAttempts, tokenReservations, tokenRollbacks atomic.Int32
					if tc.billingFailure == "subscription_db" {
						const callback = "video-subscription-query-failure"
						require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
							if tx.Statement.Table == "user_subscriptions" {
								// Matching business-error text is insufficient evidence
								// that a database error is a confirmed quota rejection.
								tx.AddError(errors.New("subscription quota insufficient: database query unavailable"))
							}
						}))
						t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove(callback)) })
					}
					if tc.billingFailure != "" {
						const callback = "video-preconsume-failure"
						require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
							updates, ok := tx.Statement.Dest.(map[string]any)
							if !ok {
								return
							}
							if updates["billing_status"] == "reserving" {
								if billingAttempts.Add(1) > 1 {
									assert.Fail(t, "local pre-consumption failure must not re-enter custom retries")
									tx.AddError(errors.New("repeated pre-consumption attempt"))
								}
							}
							if expr, ok := updates["remain_quota"].(clause.Expr); ok {
								switch expr.SQL {
								case "remain_quota - ?":
									tokenReservations.Add(1)
									if tc.billingFailure == "token_db" {
										tx.AddError(errors.New("token reservation unavailable"))
									}
								case "remain_quota + ?":
									tokenRollbacks.Add(1)
									if tc.billingFailure == "rollback" {
										tx.AddError(errors.New("token rollback unavailable"))
									}
								}
							}
							if expr, ok := updates["quota"].(clause.Expr); ok && expr.SQL == "quota - ?" {
								switch tc.billingFailure {
								case "wallet_race", "rollback":
									// Simulate spending after the balance read but before
									// the atomic reservation, on the same transaction.
									result := tx.Session(&gorm.Session{NewDB: true}).Model(&model.User{}).Where("id = ?", user.Id).Update("quota", 50)
									tx.AddError(result.Error)
								case "wallet_db":
									tx.AddError(errors.New("wallet reservation unavailable"))
								}
							}
						}))
						t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
					}
					if tc.beforeSend {
						const callback = "nldw-dispatch-rejected"
						require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
							updates, ok := tx.Statement.Dest.(map[string]any)
							if ok && updates["dispatched_at"] != nil {
								barrierAttempts.Add(1)
								tx.AddError(errors.New("dispatch barrier rejected"))
							}
						}))
						t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
					}
					if tc.insertFails {
						const callback = "nldw-task-insert-rejected"
						require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
							if _, ok := tx.Statement.Dest.(*model.Task); ok {
								tx.AddError(errors.New("task persistence unavailable"))
							}
						}))
						t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove(callback)) })
					}
					if tc.buildFails {
						const callback = "local-body-failure-retry"
						require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
							if _, ok := tx.Statement.Dest.(*model.MediaRetryEvent); ok {
								// Fail immediately if the regression returns, instead
								// of waiting for the unconsumed retry budget to loop.
								assert.Fail(t, "local body construction failure entered upstream retry policy")
								tx.AddError(errors.New("unexpected retry after local body construction failure"))
							}
						}))
						t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove(callback)) })
					}
					err = service.ProcessAsyncMediaJob(t.Context(), job, service.DefaultMediaRuntimeConfig())
					if tc.wantStatus == "submitted" {
						require.NoError(t, err)
					} else {
						require.Error(t, err)
					}
					if tc.buildFails {
						require.ErrorContains(t, err, "build_request_failed")
					}
					if tc.billingFailure == "subscription_db" {
						require.ErrorContains(t, err, "update_data_error")
					}
					require.NoError(t, db.First(&job, job.ID).Error)
					require.NoError(t, db.First(&user, user.Id).Error)
					require.NoError(t, db.First(&token, token.Id).Error)
					assert.EqualValues(t, tc.wantCalls, calls.Load())
					assert.Equal(t, tc.wantWallet, user.Quota)
					wantToken := tc.wantWallet - 900
					if tc.billingFailure != "" || tc.subscriptionSuccess {
						wantToken = tc.wantToken
					}
					assert.Equal(t, wantToken, token.RemainQuota)
					assert.Equal(t, tc.wantStatus, job.Status)
					assert.Equal(t, tc.wantBilling, job.BillingStatus)
					if tc.wantBilling == "refunded" {
						assert.Zero(t, job.Quota)
						assert.Zero(t, token.UsedQuota)
						var refunds []model.Log
						require.NoError(t, model.LOG_DB.Where("user_id = ? AND type = ?", user.Id, model.LogTypeRefund).Find(&refunds).Error)
						require.Len(t, refunds, 1)
						assert.Equal(t, 100, refunds[0].Quota)
					}
					if tc.subscriptionSuccess {
						require.NoError(t, db.First(&subscription, subscription.Id).Error)
						assert.EqualValues(t, 100, subscription.AmountUsed)
						assert.Equal(t, 100, token.UsedQuota)
						assert.Equal(t, 100, job.Quota)
					}
					if tc.billingFailure != "" {
						assert.EqualValues(t, 1, billingAttempts.Load())
						if tc.billingFailure == "wallet_race" || tc.billingFailure == "rollback" || tc.billingFailure == "wallet_db" || strings.HasPrefix(tc.billingFailure, "subscription_") {
							assert.EqualValues(t, 1, tokenReservations.Load(), "failed compensation cannot fall back to a second funding source")
							assert.EqualValues(t, 1, tokenRollbacks.Load())
						}
						var retryEvents, consumeLogs, refundLogs int64
						require.NoError(t, db.Model(&model.MediaRetryEvent{}).Where("task_id = ?", job.TaskId).Count(&retryEvents).Error)
						require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).Count(&consumeLogs).Error)
						require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", user.Id, model.LogTypeRefund).Count(&refundLogs).Error)
						assert.Zero(t, retryEvents)
						assert.Zero(t, consumeLogs)
						assert.Zero(t, refundLogs, "an unsuccessful reservation must not invent a completed task refund")
						if tc.billingFailure == "subscription_short" {
							require.NoError(t, db.First(&subscription, subscription.Id).Error)
							assert.EqualValues(t, 50, subscription.AmountUsed)
						}
					}
					if tc.beforeSend || tc.buildFails || tc.billingFailure != "" {
						if tc.beforeSend {
							assert.EqualValues(t, 1, barrierAttempts.Load(), "local failures cannot loop through upstream retries")
						}
						assert.Zero(t, job.DispatchedAt)
						var nativeCount int64
						require.NoError(t, db.Model(&model.Task{}).Where("task_id = ?", job.TaskId).Count(&nativeCount).Error)
						assert.Zero(t, nativeCount, "a rejected dispatch cannot create a retained unknown task")
						if tc.billingFailure == "rollback" {
							require.NoError(t, db.Callback().Update().Remove("video-preconsume-failure"))
							// Simulate a crash after the failed compensation was recorded,
							// before the worker finalized the submission status.
							require.NoError(t, db.Model(&job).Updates(map[string]any{"status": "submitting", "lease_token": "interrupted-lease", "lease_expires_at": now - 1, "next_attempt_at": 0}).Error)
							require.NoError(t, db.First(&job, job.ID).Error)
							eligible, err := model.GetDueAsyncMediaJobsForPhase(t.Context(), now, 100, "submit")
							require.NoError(t, err)
							require.True(t, slices.ContainsFunc(eligible, func(candidate model.AsyncMediaJob) bool { return candidate.ID == job.ID }))
							claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "recovered-lease", 120)
							require.NoError(t, err)
							require.True(t, claimed)
							var recoveredSubmits int
							service.SubmitAsyncVideoFunc = func(ctx context.Context, recoveredJob model.AsyncMediaJob, recoveredRequest service.AsyncVideoRequest) error {
								recoveredSubmits++
								return ExecuteAsyncVideo(ctx, recoveredJob, recoveredRequest)
							}
							require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, service.DefaultMediaRuntimeConfig()))
							service.SubmitAsyncVideoFunc = ExecuteAsyncVideo
							require.NoError(t, db.First(&job, job.ID).Error)
							require.NoError(t, db.First(&user, user.Id).Error)
							require.NoError(t, db.First(&token, token.Id).Error)
							assert.Zero(t, recoveredSubmits)
							assert.Zero(t, calls.Load())
							assert.Equal(t, "failed", job.Status)
							assert.Equal(t, "failed", job.BillingStatus)
							assert.Equal(t, 50, user.Quota)
							assert.Equal(t, 150, token.RemainQuota)
							assert.Equal(t, 100, token.UsedQuota)
						}
						return
					}
					if tc.configured != "" && tc.insertFails {
						var nativeCount int64
						require.NoError(t, db.Model(&model.Task{}).Where("task_id = ?", job.TaskId).Count(&nativeCount).Error)
						assert.Zero(t, nativeCount)
						plain, err := service.DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
						require.NoError(t, err)
						var frozen service.AsyncVideoRequest
						require.NoError(t, common.Unmarshal(plain, &frozen))
						require.NotEmpty(t, frozen.InitialUnknownAttempts)
						assert.Equal(t, model.TaskStatusUnknown, frozen.InitialUnknownAttempts[0].Status)
						return
					}
					if tc.configured != "" && tc.wantBilling == "unknown" {
						assert.Equal(t, "policy_stopped_unknown", job.ReferenceRecoveryStage)
						var native model.Task
						require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&native).Error)
						require.NotEmpty(t, native.PrivateData.VideoAttempts)
						assert.Equal(t, model.TaskStatusUnknown, native.PrivateData.VideoAttempts[0].Status)
						view, _, err := videoJobView(taskSubmissionTestContext(), job, true, false)
						require.NoError(t, err)
						assert.Equal(t, true, view["can_terminate"])
						require.NoError(t, service.ReconcileTerminatedAsyncMediaReservation(t.Context(), job))
						require.NoError(t, db.First(&user, user.Id).Error)
						assert.Equal(t, 1000, user.Quota)
						return
					}
					if tc.wantBilling == "unknown" {
						assert.Equal(t, 100, job.Quota)
						assert.NotEmpty(t, job.ChannelCipher)
						plain, err := service.DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
						require.NoError(t, err)
						var frozen service.AsyncVideoRequest
						require.NoError(t, common.Unmarshal(plain, &frozen))
						assert.Equal(t, request.Body, frozen.Body)
						viewContext := taskSubmissionTestContext()
						view, _, err := videoJobView(viewContext, job, true, false)
						require.NoError(t, err)
						assert.Equal(t, true, view["can_terminate"], "administrators can reconcile and end this retained operation")
						require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, service.DefaultMediaRuntimeConfig()))
						require.NoError(t, db.First(&job, job.ID).Error)
						assert.Equal(t, "execution_unknown", job.Status)
						assert.EqualValues(t, 1, calls.Load(), "reprocessing unknown execution must not generate again")
						if tc.lostReceipt {
							originalRequest, originalChannel := bytes.Clone(job.RequestCipher), bytes.Clone(job.ChannelCipher)
							require.NoError(t, db.Model(&job).Update("expires_at", now-1).Error)
							require.NoError(t, db.First(&job, job.ID).Error)
							// A cleanup snapshot can predate the worker publishing the
							// unknown state; eligibility must be rechecked in the write.
							stale := job
							stale.Status = "submitting"
							require.ErrorIs(t, model.ExpireAsyncMediaJob(t.Context(), stale, now), model.ErrImageConflict)
							terminal := job
							terminal.ID, terminal.TaskId, terminal.IdentityHash = 0, model.GenerateTaskID(), common.GetUUID()
							terminal.Status, terminal.BillingStatus, terminal.Quota = "failed", "refunded", 0
							require.NoError(t, db.Create(&terminal).Error)
							cfg := service.DefaultMediaRuntimeConfig()
							cfg.LocalPath = t.TempDir()
							require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
							require.NoError(t, service.CleanupMediaObjects(t.Context()))
							require.NoError(t, db.First(&job, job.ID).Error)
							assert.Equal(t, "execution_unknown", job.Status, "retention must preserve an unresolved dispatch and its financial hold")
							assert.True(t, bytes.Equal(originalRequest, job.RequestCipher), "retain the frozen request")
							assert.True(t, bytes.Equal(originalChannel, job.ChannelCipher), "retain the frozen channel")
							require.NoError(t, db.First(&user, user.Id).Error)
							require.NoError(t, db.First(&token, token.Id).Error)
							assert.Equal(t, 900, user.Quota)
							assert.Zero(t, token.RemainQuota)
							require.NoError(t, db.First(&terminal, terminal.ID).Error)
							assert.Equal(t, "failed", terminal.Status, "retention cleanup must preserve the confirmed execution outcome")
							assert.Equal(t, "expired", terminal.StorageStatus, "confirmed financial completion still permits retention cleanup")
							assert.Empty(t, terminal.RequestCipher)
							assert.Empty(t, terminal.ChannelCipher)
							view, _, err = videoJobView(viewContext, job, true, false)
							require.NoError(t, err)
							assert.Equal(t, true, view["can_terminate"], "retention must not remove administrator reconciliation")
							for _, attempt := range []struct {
								admin bool
								body  string
								code  int
							}{
								{false, `{"confirm_upstream_checked":true}`, http.StatusConflict},
								{true, `{}`, http.StatusConflict},
								{true, `{"confirm_upstream_checked":true}`, http.StatusOK},
								{true, `{"confirm_upstream_checked":true}`, http.StatusConflict},
							} {
								recorder := httptest.NewRecorder()
								c, _ := gin.CreateTestContext(recorder)
								c.Set("id", user.Id)
								c.Params = gin.Params{{Key: "task_id", Value: job.TaskId}}
								c.Request = httptest.NewRequest(http.MethodPost, "/api/media/tasks/"+job.TaskId+"/terminate", strings.NewReader(attempt.body))
								ManageMediaTask(c, attempt.admin, "terminate")
								require.Equal(t, attempt.code, recorder.Code, recorder.Body.String())
							}
							require.NoError(t, db.First(&job, job.ID).Error)
							assert.Equal(t, "failed", job.Status)
							assert.Equal(t, "unknown", job.BillingStatus, "manual termination does not invent a refund")
							assert.Equal(t, 100, job.Quota)
							require.NoError(t, service.CleanupMediaObjects(t.Context()))
							require.NoError(t, db.First(&job, job.ID).Error)
							assert.Equal(t, "failed", job.Status, "retention cleanup must preserve the administrator's local termination")
							assert.Equal(t, "expired", job.StorageStatus, "the confirmed local termination releases retained payloads")
							assert.Empty(t, job.RequestCipher)
							assert.Empty(t, job.ChannelCipher)
							require.NoError(t, db.First(&user, user.Id).Error)
							require.NoError(t, db.First(&token, token.Id).Error)
							assert.Equal(t, 900, user.Quota)
							assert.Zero(t, token.RemainQuota)
							assert.EqualValues(t, 1, calls.Load(), "cleanup and manual reconciliation never create another video")
						}
					}
				})
			}
		})
	}
}

func TestYuxiboxPollingHonorsPluginContractDatabase(t *testing.T) {
	baseUser, baseToken := setupResponsesWSRequestTest(t)
	db := setupVideoRetryTestDatabase(t, baseUser, baseToken)
	require.NoError(t, db.Unscoped().Delete(baseToken).Error)
	require.NoError(t, db.AutoMigrate(&model.UserSubscription{}, &model.Channel{}, &model.Ability{}, &model.Task{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.Log{}, &model.Option{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.MediaArtifactObject{}, &model.MediaRetryPolicyVersion{}, &model.MediaRetryEvent{}, &model.AsyncImageUsageProjection{}, &model.AsyncImageLogReceipt{}, &model.CanvasGrant{}))
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "yuxibox-poll:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "yuxibox-poll")
	previousRegistry := pluginruntime.DefaultRegistry
	pluginruntime.DefaultRegistry = pluginruntime.NewRegistry()
	t.Cleanup(func() { pluginruntime.DefaultRegistry = previousRegistry })
	source, err := builtinplugins.Source("yuxibox")
	require.NoError(t, err)
	plugin, err := pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: "yuxibox"})
	require.NoError(t, err)
	previousSubmit, previousAdaptor := service.SubmitAsyncVideoFunc, service.GetTaskAdaptorFunc
	service.SubmitAsyncVideoFunc = ExecuteAsyncVideo
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
	t.Cleanup(func() { service.SubmitAsyncVideoFunc, service.GetTaskAdaptorFunc = previousSubmit, previousAdaptor })
	previousLogs, previousBatch := common.LogConsumeEnabled, common.BatchUpdateEnabled
	common.LogConsumeEnabled, common.BatchUpdateEnabled = true, false
	t.Cleanup(func() { common.LogConsumeEnabled, common.BatchUpdateEnabled = previousLogs, previousBatch })
	cfg := service.DefaultMediaRuntimeConfig()
	cfg.LocalPath, cfg.MaxFileBytes = t.TempDir(), 4096
	require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
	service.InitHttpClient()
	for index, tc := range []struct {
		name, body        string
		status            model.TaskStatus
		wallet, token     int
		quota, refundLogs int
	}{
		{name: "legacy failure for another task", body: `{"code":"success","data":{"task_id":"other-task","status":"FAILURE","fail_reason":"wrong task"}}`, status: model.TaskStatusInProgress, wallet: 900, quota: 100},
		{name: "legacy success for another task", body: `{"code":"success","data":{"task_id":"other-task","status":"SUCCESS"}}`, status: model.TaskStatusInProgress, wallet: 900, quota: 100},
		{name: "native completed", body: `{"id":"accepted-task","status":"completed"}`, status: model.TaskStatusSuccess, wallet: 900, quota: 100},
		{name: "native failure", body: `{"id":"accepted-task","status":"failed","error":{"message":"generation rejected"}}`, status: model.TaskStatusFailure, wallet: 1000, token: 100, refundLogs: 1},
		{name: "native failure with output", body: `{"id":"accepted-task","status":"failed","download_url":"https://cdn.example/finished.mp4"}`, status: model.TaskStatusInProgress, wallet: 900, quota: 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := model.User{Username: fmt.Sprintf("yuxibox-poll-%d", index), AffCode: common.GetUUID(), Status: common.UserStatusEnabled, Group: "default", Quota: 1000, AuthVersion: 1}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: common.GetUUID(), Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100}
			require.NoError(t, db.Create(&token).Error)
			var submits, polls atomic.Int32
			var pollBody atomic.Value
			pollBody.Store(`{"id":"accepted-task","status":"in_progress"}`)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer original-key", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost && r.URL.Path == "/v1/videos" {
					submits.Add(1)
					_, _ = io.WriteString(w, `{"id":"accepted-task","status":"queued"}`)
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == "/v1/videos/accepted-task" {
					polls.Add(1)
					_, _ = io.WriteString(w, pollBody.Load().(string))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			t.Cleanup(upstream.Close)
			setting := `{"task_plugin_key":"yuxibox"}`
			channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "Yuxibox poll fixture", Key: "original-key", BaseURL: &upstream.URL, Models: "seedance-2.5-yx-sz", Group: "default", Status: common.ChannelStatusEnabled, Setting: &setting}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Group: "default", Model: channel.Models, Enabled: true}).Error)
			now := time.Now().Unix()
			job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Provider: "yuxibox", Model: channel.Models, Group: "default", Status: "queued", BillingStatus: "pending", StorageStatus: "pending", LeaseToken: "poll-fixture", LeaseExpiresAt: now + 120, CreatedAt: now, ExpiresAt: now + 3600}
			request := service.AsyncVideoRequest{Body: []byte(`{"model":"seedance-2.5-yx-sz","prompt":"poll fixture","seconds":5,"resolution":"720p"}`), ContentType: "application/json", ClientIP: "203.0.113.10", Channel: channel, PluginHash: plugin.Engine.SourceFingerprint(), Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "yuxibox", Version: plugin.Meta.Version, APIVersion: 1}}, Billing: service.MediaBillingSnapshot{Price: types.PriceData{Quota: 100, QuotaToPreConsume: 100, UsePrice: true}}}
			encoded, err := common.Marshal(request)
			require.NoError(t, err)
			job.RequestCipher, err = service.EncryptImagePayload(encoded, "media-request:"+job.TaskId)
			require.NoError(t, err)
			require.NoError(t, db.Create(&job).Error)
			require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
			var task model.Task
			require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&task).Error)
			for phase := range 3 {
				if phase == 1 {
					pollBody.Store(tc.body)
				}
				require.NoError(t, service.UpdateVideoTasks(t.Context(), "yuxibox", map[int][]string{channel.Id: {task.GetUpstreamTaskID()}}, map[string]*model.Task{task.GetUpstreamTaskID(): &task}))
				require.NoError(t, db.First(&task, task.ID).Error)
				if phase == 0 {
					require.EqualValues(t, model.TaskStatusInProgress, task.Status)
					continue
				}
				assert.Equal(t, tc.status, task.Status)
				assert.Equal(t, tc.quota, task.Quota)
				require.NoError(t, db.First(&user, user.Id).Error)
				require.NoError(t, db.First(&token, token.Id).Error)
				assert.Equal(t, tc.wallet, user.Quota)
				assert.Equal(t, tc.token, token.RemainQuota)
				var consumes, refunds int64
				require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).Count(&consumes).Error)
				require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", user.Id, model.LogTypeRefund).Count(&refunds).Error)
				assert.EqualValues(t, 1, consumes, "polling must not duplicate the submission charge")
				assert.EqualValues(t, tc.refundLogs, refunds, "only a confirmed native failure refunds once")
			}
			assert.EqualValues(t, 1, submits.Load())
			assert.EqualValues(t, 2, polls.Load(), "terminal or reconciled tasks do not poll again immediately")
		})
	}
}

type huahuoVideoRoundTrip func(*http.Request) (*http.Response, error)

func (roundTrip huahuoVideoRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestHuahuoVideoDurableLifecycle(t *testing.T) {
	const publicModel, upstreamModel = "huahuo-public-fixture", "inmind/video/new-test-model"
	for _, scenario := range []string{"completed", "concurrency rejection", "lost receipt"} {
		t.Run(scenario, func(t *testing.T) {
			user, token := setupResponsesWSRequestTest(t)
			db := setupVideoRetryTestDatabase(t, user, token)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Task{}, &model.TaskPlugin{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.MediaRetryEvent{}, &model.Option{}, &model.UserSubscription{}, &model.AsyncImageUsageProjection{}, &model.AsyncImageLogReceipt{}, &model.CanvasGrant{}, &model.Log{}, &model.MediaRetryPolicyVersion{}, &model.MediaArtifactObject{}, &model.MediaInputObject{}))
			t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "huahuo-test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
			t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "huahuo-test")
			previousRegistry, previousSubmit := pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc
			previousAdaptor, previousPersist := service.GetTaskAdaptorFunc, service.PersistAsyncVideoFunc
			previousSecret, previousEnabled := common.CryptoSecret, constant.TaskPluginEnabled
			previousBatch, previousLogs := common.BatchUpdateEnabled, common.LogConsumeEnabled
			pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc = pluginruntime.NewRegistry(), ExecuteAsyncVideo
			service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
			service.PersistAsyncVideoFunc = PersistAsyncVideo
			common.CryptoSecret, constant.TaskPluginEnabled = "huahuo-test-signing", true
			common.BatchUpdateEnabled, common.LogConsumeEnabled = false, true
			t.Cleanup(func() {
				pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc = previousRegistry, previousSubmit
				service.GetTaskAdaptorFunc, service.PersistAsyncVideoFunc = previousAdaptor, previousPersist
				common.CryptoSecret, constant.TaskPluginEnabled = previousSecret, previousEnabled
				common.BatchUpdateEnabled, common.LogConsumeEnabled = previousBatch, previousLogs
			})
			redisServer := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			common.RDB, common.RedisEnabled = client, true
			t.Cleanup(func() { assert.NoError(t, client.Close()) })
			cfg := service.DefaultMediaRuntimeConfig()
			cfg.VideoAsyncEnabled, cfg.LocalPath = true, t.TempDir()
			require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
			source, err := builtinplugins.Source(constant.TaskPlatformHuahuoVideo)
			require.NoError(t, err)
			_, err = pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: constant.TaskPlatformHuahuoVideo})
			require.NoError(t, err)

			const origin, cdn = "https://8.8.8.8", "https://1.1.1.1"
			baseURL, mapping := origin, `{"`+publicModel+`":"`+upstreamModel+`"}`
			channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "Huahuo video fixture", Key: "huahuo-origin-key", BaseURL: &baseURL, ModelMapping: &mapping, Models: publicModel, Group: "default", Status: common.ChannelStatusEnabled}
			zero := 0
			channel.SetSetting(relaydto.ChannelSettings{TaskPluginKey: constant.TaskPlatformHuahuoVideo, TaskPluginModelConfigs: map[string]relaydto.TaskVideoModelConfig{
				upstreamModel: {Duration: relaydto.TaskVideoDuration{Values: []int{5}}, Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"}, Modes: []string{"text_to_video"}, References: relaydto.TaskVideoReferences{Images: &zero, Videos: &zero, Audios: &zero}, BillingMode: "per_second"},
			}})
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Group: "default", Model: publicModel, Enabled: true}).Error)
			snapshot, err := model.GetTaskPluginSyncSnapshot()
			require.NoError(t, err)
			require.NoError(t, pluginruntime.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, snapshot.VideoModels))
			withTieredBillingConfig(t, map[string]string{publicModel: "tiered_expr"}, map[string]string{publicModel: `u("seconds") * 0.00004`})
			_, err = model.SaveMediaRetryPolicy(t.Context(), mediaretry.Policy{Rules: []mediaretry.Rule{{ID: "replay", Name: "Replay", Enabled: true, Scope: mediaretry.Scope{MediaTypes: []string{"video"}}, Match: mediaretry.Match{ContainsAny: []string{"not submitted", "EOF"}}, Steps: []mediaretry.Step{{ID: "same", Action: mediaretry.SameChannel, MaxAttempts: 1}}}}}, 0, user.Id, nil)
			require.NoError(t, err)

			var job model.AsyncMediaJob
			var submits, polls, downloads atomic.Int32
			fixture := huahuoVideoRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.Body != nil {
					defer request.Body.Close()
				}
				recorder := httptest.NewRecorder()
				recorder.Header().Set("Content-Type", "application/json")
				switch request.Method + " " + request.URL.String() {
				case "POST " + origin + "/v1/videos":
					submits.Add(1)
					assert.Equal(t, "Bearer huahuo-origin-key", request.Header.Get("Authorization"))
					assert.Equal(t, job.TaskId, request.Header.Get("Idempotency-Key"))
					var body map[string]any
					assert.NoError(t, common.DecodeJson(request.Body, &body))
					assert.Equal(t, upstreamModel, body["model"])
					assert.Equal(t, "5", body["seconds"])
					assert.Equal(t, "16:9", body["size"])
					switch scenario {
					case "lost receipt":
						return nil, io.ErrUnexpectedEOF
					case "concurrency rejection":
						recorder.WriteHeader(http.StatusTooManyRequests)
						_, _ = io.WriteString(recorder, `{"error":{"code":"video_concurrency_limit","type":"rate_limit_error","message":"not submitted","limit":10}}`)
					default:
						_, _ = io.WriteString(recorder, `{"id":"vendor-private-task","status":"queued"}`)
					}
				case "GET " + origin + "/v1/videos/vendor-private-task":
					assert.Equal(t, "Bearer huahuo-origin-key", request.Header.Get("Authorization"))
					switch polls.Add(1) {
					case 1:
						recorder.WriteHeader(http.StatusNotFound)
					case 2:
						_, _ = io.WriteString(recorder, `{"id":"vendor-private-task","status":"failed","credit_state":"refunded","result":{"url":"https://example.invalid/result.mp4"}}`)
					default:
						_, _ = io.WriteString(recorder, `{"id":"vendor-private-task","status":"completed","progress":100}`)
					}
				case "GET " + origin + "/v1/videos/vendor-private-task/content":
					assert.Equal(t, "Bearer huahuo-origin-key", request.Header.Get("Authorization"))
					recorder.Header().Set("Location", cdn+"/private-signed.mp4?signature=fixture")
					recorder.WriteHeader(http.StatusFound)
				case "GET " + cdn + "/private-signed.mp4?signature=fixture":
					assert.Empty(t, request.Header.Get("Authorization"))
					if downloads.Add(1) == 1 {
						recorder.WriteHeader(http.StatusServiceUnavailable)
						break
					}
					recorder.Header().Set("Content-Type", "video/mp4")
					_, _ = recorder.Write(taskMediaMP4Fixture())
				default:
					t.Errorf("unexpected Huahuo request %s %s", request.Method, request.URL)
					recorder.WriteHeader(http.StatusNotFound)
				}
				return recorder.Result(), nil
			})
			fetch := system_setting.GetFetchSetting()
			previousFetch := *fetch
			fetch.EnableSSRFProtection, fetch.AllowPrivateIp, fetch.ApplyIPFilterForDomain = true, false, true
			t.Cleanup(func() { *fetch = previousFetch })
			service.InitHttpClient()
			relayClient, contentClient := service.GetHttpClient(), service.GetSSRFProtectedHTTPClient()
			oldRelayTransport, oldContentTransport := relayClient.Transport, contentClient.Transport
			relayClient.Transport, contentClient.Transport = fixture, fixture
			t.Cleanup(func() { relayClient.Transport, contentClient.Transport = oldRelayTransport, oldContentTransport })

			engine := gin.New()
			engine.POST("/v1/videos/generations_async", middleware.TokenAuth(), PrepareAsyncVideo, PinAsyncVideoEndpoint(), FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), RouteAsyncVideo, middleware.Distribute(), AcceptAsyncVideo)
			submit := func(prompt string) *httptest.ResponseRecorder {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", strings.NewReader(`{"provider":"huahuo-video","model":"`+publicModel+`","prompt":"`+prompt+`","seconds":5,"resolution":"720p","aspect_ratio":"16:9"}`))
				request.Header.Set("Authorization", "Bearer sk-"+token.Key)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", "huahuo-client-fixture")
				engine.ServeHTTP(recorder, request)
				return recorder
			}
			recorder := submit("fixture")
			require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())
			require.NoError(t, db.Where("user_id = ?", user.Id).Take(&job).Error)
			if scenario == "completed" {
				repeated := submit("fixture")
				require.Equal(t, http.StatusAccepted, repeated.Code, repeated.Body.String())
				assert.Contains(t, repeated.Body.String(), job.TaskId)
				conflicting := submit("different request")
				require.Equal(t, http.StatusConflict, conflicting.Code, conflicting.Body.String())
			}
			assert.Zero(t, submits.Load())
			claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "huahuo-submit", 120)
			require.NoError(t, err)
			require.True(t, claimed)
			processErr := service.ProcessAsyncMediaJob(t.Context(), job, cfg)
			if scenario == "completed" {
				require.NoError(t, processErr)
			} else {
				require.Error(t, processErr)
			}
			require.NoError(t, db.First(&job, job.ID).Error)
			assert.EqualValues(t, 1, submits.Load())
			if scenario != "completed" {
				if scenario == "concurrency rejection" {
					assert.Equal(t, "failed", job.Status)
					assert.Equal(t, "refunded", job.BillingStatus)
				} else {
					assert.Equal(t, "execution_unknown", job.Status)
					assert.Equal(t, "unknown", job.BillingStatus)
				}
				require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
				assert.EqualValues(t, 1, submits.Load())
				require.NoError(t, db.First(token, token.Id).Error)
				assert.Equal(t, map[string]int{"concurrency rejection": 100, "lost receipt": 0}[scenario], token.RemainQuota)
				return
			}
			var task model.Task
			require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&task).Error)
			assert.Equal(t, publicModel, task.Properties.OriginModelName)
			assert.Equal(t, upstreamModel, task.Properties.UpstreamModelName)
			assert.Equal(t, 100, task.Quota)
			for phase := range 4 {
				id := task.GetUpstreamTaskID()
				require.NoError(t, service.UpdateVideoTasks(t.Context(), task.Platform, map[int][]string{channel.Id: {id}}, map[string]*model.Task{id: &task}))
				require.NoError(t, db.First(&task, task.ID).Error)
				if phase < 2 {
					assert.NotEqual(t, model.TaskStatus(model.TaskStatusFailure), task.Status)
					assert.Equal(t, 100, task.Quota)
					require.NotNil(t, task.PrivateData.VideoReconciliation)
					assert.False(t, task.PrivateData.VideoReconciliation.ManualRequired)
					before := task.Snapshot()
					task.PrivateData.VideoReconciliation.NextPollAt = time.Now().Unix() - 1
					updated, updateErr := task.UpdatePollingState(t.Context(), before)
					require.NoError(t, updateErr)
					require.True(t, updated)
				}
			}
			require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
			require.NoError(t, db.First(&job, job.ID).Error)
			for _, stage := range []string{"ready", "failed", "succeeded"} {
				require.NoError(t, db.Model(&job).Update("next_attempt_at", 0).Error)
				job.NextAttemptAt = 0
				claimed, err = model.ClaimAsyncMediaJob(t.Context(), &job, "huahuo-"+stage, 120)
				require.NoError(t, err)
				require.True(t, claimed)
				storageErr := service.ProcessAsyncMediaJob(t.Context(), job, cfg)
				if stage == "failed" {
					require.Error(t, storageErr)
				} else {
					require.NoError(t, storageErr)
				}
				require.NoError(t, db.First(&job, job.ID).Error)
				require.Equal(t, stage, job.StorageStatus, job.ErrorMessage)
			}
			assert.Equal(t, "succeeded", job.Status, job.ErrorMessage)
			assert.EqualValues(t, 2, downloads.Load())
			assert.EqualValues(t, 1, submits.Load())
			view, _, err := videoJobView(taskSubmissionTestContext(), job, false, false)
			require.NoError(t, err)
			encoded, err := common.Marshal(view)
			require.NoError(t, err)
			assert.Contains(t, string(encoded), job.TaskId)
			assert.NotContains(t, string(encoded), "vendor-private-task")
			assert.NotContains(t, string(encoded), "private-signed.mp4")
			assert.NotContains(t, string(encoded), "huahuo-origin-key")
			require.NoError(t, db.First(token, token.Id).Error)
			assert.Zero(t, token.RemainQuota)
			var consumes int64
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).Count(&consumes).Error)
			assert.EqualValues(t, 1, consumes)
		})
	}
}

func TestWolongVideoDurableLifecycle(t *testing.T) {
	const publicModel = "wolong-public-fixture"
	for _, tc := range []struct {
		scenario, billingMode                 string
		aliasDefaults, reference, reconciling bool
	}{
		{scenario: "completed"}, {scenario: "refunded"}, {scenario: "balance rejection"}, {scenario: "lost receipt"},
		{scenario: "refunded", reconciling: true},
		{scenario: "completed", billingMode: "per_request"}, {scenario: "completed", billingMode: "per_second"},
		{scenario: "completed", aliasDefaults: true}, {scenario: "completed", aliasDefaults: true, reference: true},
	} {
		name := strings.TrimSpace(tc.scenario + " " + tc.billingMode)
		if tc.aliasDefaults {
			name += " alias defaults"
		}
		if tc.reference {
			name += " image reference"
		}
		if tc.reconciling {
			name += " after reconciliation"
		}
		t.Run(name, func(t *testing.T) {
			scenario, upstreamModel := tc.scenario, "haya-mo-seedance-25"
			if tc.billingMode != "" {
				upstreamModel = "future-wolong-model"
			}
			user, token := setupResponsesWSRequestTest(t)
			db := setupVideoRetryTestDatabase(t, user, token)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Task{}, &model.TaskPlugin{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.MediaRetryEvent{}, &model.Option{}, &model.UserSubscription{}, &model.AsyncImageUsageProjection{}, &model.AsyncImageLogReceipt{}, &model.CanvasGrant{}, &model.Log{}, &model.MediaRetryPolicyVersion{}, &model.MediaArtifactObject{}, &model.MediaInputObject{}))
			t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "wolong-test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
			t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "wolong-test")
			previousRegistry, previousSubmit := pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc
			previousAdaptor := service.GetTaskAdaptorFunc
			previousSecret, previousEnabled := common.CryptoSecret, constant.TaskPluginEnabled
			previousBatch, previousLogs := common.BatchUpdateEnabled, common.LogConsumeEnabled
			pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc = pluginruntime.NewRegistry(), ExecuteAsyncVideo
			service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
			common.CryptoSecret, constant.TaskPluginEnabled = "wolong-test-signing", true
			common.BatchUpdateEnabled, common.LogConsumeEnabled = false, true
			t.Cleanup(func() {
				pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc = previousRegistry, previousSubmit
				service.GetTaskAdaptorFunc = previousAdaptor
				common.CryptoSecret, constant.TaskPluginEnabled = previousSecret, previousEnabled
				common.BatchUpdateEnabled, common.LogConsumeEnabled = previousBatch, previousLogs
			})
			redisServer := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			common.RDB, common.RedisEnabled = client, true
			t.Cleanup(func() { assert.NoError(t, client.Close()) })
			cfg := service.DefaultMediaRuntimeConfig()
			cfg.VideoAsyncEnabled, cfg.LocalPath = true, t.TempDir()
			require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
			source, err := builtinplugins.Source("wolong")
			require.NoError(t, err)
			_, err = pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: "wolong"})
			require.NoError(t, err)
			const origin = "https://8.8.8.8"
			baseURL, mapping := origin, `{"`+publicModel+`":"`+upstreamModel+`"}`
			ch := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "Wolong fixture", Key: "wl_fixture", BaseURL: &baseURL, ModelMapping: &mapping, Models: publicModel, Group: "default", Status: common.ChannelStatusEnabled}
			settings := relaydto.ChannelSettings{TaskPluginKey: "wolong"}
			if tc.billingMode != "" {
				zero := 0
				settings.TaskPluginModelConfigs = map[string]relaydto.TaskVideoModelConfig{upstreamModel: {
					Duration:    relaydto.TaskVideoDuration{Values: []int{5, 10}, Default: 5},
					Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"}, Modes: []string{"text_to_video"},
					References: relaydto.TaskVideoReferences{Images: &zero, Videos: &zero, Audios: &zero}, BillingMode: tc.billingMode,
				}}
			}
			ch.SetSetting(settings)
			require.NoError(t, ch.ValidateTaskVideoModelBindings())
			require.NoError(t, db.Create(&ch).Error)
			require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Group: "default", Model: publicModel, Enabled: true}).Error)
			snapshot, err := model.GetTaskPluginSyncSnapshot()
			require.NoError(t, err)
			require.NoError(t, pluginruntime.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, snapshot.VideoModels))
			expression := `u("video_count") * 0.0002`
			if tc.billingMode == "per_second" {
				expression = `u("seconds") * 0.00004`
			}
			withTieredBillingConfig(t, map[string]string{publicModel: "tiered_expr"}, map[string]string{publicModel: expression})
			_, err = model.SaveMediaRetryPolicy(t.Context(), mediaretry.Policy{Rules: []mediaretry.Rule{{ID: "replay", Name: "Replay", Enabled: true, Scope: mediaretry.Scope{MediaTypes: []string{"video"}}, Match: mediaretry.Match{ContainsAny: []string{"fixture", "EOF"}}, Steps: []mediaretry.Step{{ID: "same", Action: mediaretry.SameChannel, MaxAttempts: 1}}}}}, 0, user.Id, nil)
			require.NoError(t, err)
			var job model.AsyncMediaJob
			var submits, polls atomic.Int32
			service.InitHttpClient()
			httpClient := service.GetHttpClient()
			oldTransport := httpClient.Transport
			httpClient.Transport = huahuoVideoRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.Body != nil {
					defer request.Body.Close()
				}
				recorder := httptest.NewRecorder()
				recorder.Header().Set("Content-Type", "application/json")
				assert.Equal(t, "Bearer wl_fixture", request.Header.Get("Authorization"))
				switch request.Method + " " + request.URL.String() {
				case "POST " + origin + "/v1/videos/generations":
					submits.Add(1)
					assert.Equal(t, job.TaskId, request.Header.Get("Idempotency-Key"))
					var body map[string]any
					assert.NoError(t, common.DecodeJson(request.Body, &body))
					assert.Equal(t, upstreamModel, body["model"])
					assert.EqualValues(t, 5, body["duration"])
					if tc.reference {
						assert.Equal(t, []any{"https://media.example.com/reference.png"}, body["image_urls"])
					}
					switch scenario {
					case "lost receipt":
						return nil, io.ErrUnexpectedEOF
					case "balance rejection":
						recorder.WriteHeader(http.StatusPaymentRequired)
						_, _ = io.WriteString(recorder, `{"ok":false,"code":"INSUFFICIENT_CREDITS","error":"fixture balance insufficient","need":60,"balance":3}`)
					default:
						recorder.WriteHeader(http.StatusInternalServerError)
						_, _ = io.WriteString(recorder, `{"ok":false,"error":"fixture accepted receipt","task":{"id":"task_vendor_private","status":"running"}}`)
					}
				case "GET " + origin + "/v1/tasks/task_vendor_private":
					phase := polls.Add(1)
					if tc.reconciling && phase == 1 {
						recorder.WriteHeader(http.StatusNotFound)
						_, _ = io.WriteString(recorder, `{"ok":false,"error":"fixture query unavailable"}`)
					} else if scenario == "refunded" {
						_, _ = fmt.Fprintf(recorder, `{"ok":true,"request_id":"req_poll_%d","task":{"id":"task_vendor_private","status":"failed","error":"fixture failure","data":[],"charged_amount":60,"refunded_amount":60,"refunded":true}}`, phase)
					} else if phase == 1 {
						_, _ = io.WriteString(recorder, `{"ok":true,"task":{"id":"task_vendor_private","status":"needs_review","progress":25.5}}`)
					} else if phase == 2 {
						_, _ = io.WriteString(recorder, `{"ok":true,"task":{"id":"task_vendor_private","status":"failed","error":"fixture partial refund","charged_amount":60,"refunded_amount":20,"refunded":false}}`)
					} else {
						_, _ = io.WriteString(recorder, `{"ok":true,"task":{"id":"task_vendor_private","status":"succeeded","progress":100,"data":[{"url":"/media/video.mp4?e=fixture","mime":"video/mp4"}]}}`)
					}
				default:
					t.Errorf("unexpected Wolong request %s %s", request.Method, request.URL)
					recorder.WriteHeader(http.StatusNotFound)
				}
				return recorder.Result(), nil
			})
			t.Cleanup(func() { httpClient.Transport = oldTransport })
			engine := gin.New()
			engine.POST("/v1/videos/generations_async", middleware.TokenAuth(), PrepareAsyncVideo, PinAsyncVideoEndpoint(), FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), RouteAsyncVideo, middleware.Distribute(), AcceptAsyncVideo)
			input := map[string]any{"provider": "wolong", "model": publicModel, "prompt": "fixture"}
			if !tc.aliasDefaults {
				input["seconds"], input["resolution"], input["aspect_ratio"] = 5, "720p", "16:9"
			}
			if tc.reference {
				input["input_reference"] = "https://media.example.com/reference.png"
				input["mode"] = "reference_to_video"
			}
			inputJSON, err := common.Marshal(input)
			require.NoError(t, err)
			for range 2 {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", bytes.NewReader(inputJSON))
				request.Header.Set("Authorization", "Bearer sk-"+token.Key)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", "wolong-client-fixture")
				engine.ServeHTTP(recorder, request)
				require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())
			}
			require.NoError(t, db.Where("user_id = ?", user.Id).Take(&job).Error)
			if tc.billingMode != "" {
				// Accepted requests retain their capability and pricing snapshots
				// when an administrator later changes the channel and unit price.
				changed := settings.TaskPluginModelConfigs[upstreamModel]
				changed.Duration = relaydto.TaskVideoDuration{Values: []int{10}, Default: 10}
				settings.TaskPluginModelConfigs[upstreamModel] = changed
				ch.SetSetting(settings)
				require.NoError(t, db.Model(&ch).Update("setting", ch.Setting).Error)
				snapshot, err := model.GetTaskPluginSyncSnapshot()
				require.NoError(t, err)
				require.NoError(t, pluginruntime.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, snapshot.VideoModels))
				withTieredBillingConfig(t, map[string]string{publicModel: "tiered_expr"}, map[string]string{publicModel: expression + " * 100"})
			}
			claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "wolong-submit", 120)
			require.NoError(t, err)
			require.True(t, claimed)
			processErr := service.ProcessAsyncMediaJob(t.Context(), job, cfg)
			if scenario == "balance rejection" || scenario == "lost receipt" {
				require.Error(t, processErr)
				require.NoError(t, db.First(&job, job.ID).Error)
				if scenario == "balance rejection" {
					assert.Equal(t, "refunded", job.BillingStatus)
				} else {
					assert.Equal(t, "unknown", job.BillingStatus)
				}
				require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
			} else {
				require.NoError(t, processErr)
				var task model.Task
				require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&task).Error)
				phases := 3
				if scenario == "refunded" {
					phases = 1
					if tc.reconciling {
						phases = 2
					}
				}
				for phase := range phases {
					id := task.GetUpstreamTaskID()
					require.NoError(t, service.UpdateVideoTasks(t.Context(), task.Platform, map[int][]string{ch.Id: {id}}, map[string]*model.Task{id: &task}))
					require.NoError(t, db.First(&task, task.ID).Error)
					if scenario == "completed" && phase < 2 || tc.reconciling && phase == 0 {
						assert.Equal(t, 100, task.Quota)
						assert.NotEqual(t, model.TaskStatus(model.TaskStatusFailure), task.Status)
						if tc.reconciling {
							require.NotNil(t, task.PrivateData.VideoReconciliation)
						}
						if task.PrivateData.VideoReconciliation != nil {
							before := task.Snapshot()
							task.PrivateData.VideoReconciliation.NextPollAt = time.Now().Unix() - 1
							updated, updateErr := task.UpdatePollingState(t.Context(), before)
							require.NoError(t, updateErr)
							require.True(t, updated)
						}
					}
				}
				if scenario == "refunded" {
					assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), task.Status)
					assert.Zero(t, task.Quota)
					assert.Equal(t, "fixture failure", task.FailReason)
					assert.True(t, task.PrivateData.VideoTerminalConfirmed)
					assert.Nil(t, task.PrivateData.VideoReconciliation)
					require.NoError(t, db.Model(&job).Update("next_attempt_at", 0).Error)
					require.NoError(t, db.First(&job, job.ID).Error)
					claimed, err = model.ClaimAsyncMediaJob(t.Context(), &job, "wolong-finalize", 120)
					require.NoError(t, err)
					require.True(t, claimed)
					require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
					require.NoError(t, db.First(&job, job.ID).Error)
					assert.Equal(t, "failed", job.Status)
					assert.Equal(t, "refunded", job.BillingStatus)
					assert.Zero(t, job.Quota)
					id, previousPolls := task.GetUpstreamTaskID(), polls.Load()
					require.NoError(t, service.UpdateVideoTasks(t.Context(), task.Platform, map[int][]string{ch.Id: {id}}, map[string]*model.Task{id: &task}))
					assert.Equal(t, previousPolls, polls.Load(), "terminal tasks must not be queried or refunded again")
				} else {
					assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
				}
			}
			assert.EqualValues(t, 1, submits.Load(), "no retry rule may regenerate the same Wolong task")
			require.NoError(t, db.First(token, token.Id).Error)
			refunded := scenario == "refunded" || scenario == "balance rejection"
			if refunded {
				assert.Equal(t, 100, token.RemainQuota)
				assert.Zero(t, token.UsedQuota)
				require.NoError(t, db.First(user, user.Id).Error)
				assert.Equal(t, 1000, user.Quota)
				assert.Zero(t, user.UsedQuota)
				require.NoError(t, db.First(&ch, ch.Id).Error)
				assert.Zero(t, ch.UsedQuota)
				quota, err := model.GetUserQuota(user.Id, false)
				require.NoError(t, err)
				assert.Equal(t, user.Quota, quota)
				cachedToken, err := model.GetTokenByKey(token.Key, false)
				require.NoError(t, err)
				assert.Equal(t, token.RemainQuota, cachedToken.RemainQuota)
			} else {
				assert.Zero(t, token.RemainQuota)
			}
			var consumes, refunds int64
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).Count(&consumes).Error)
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", user.Id, model.LogTypeRefund).Count(&refunds).Error)
			assert.LessOrEqual(t, consumes, int64(1))
			assert.LessOrEqual(t, refunds, int64(1))
			if refunded {
				assert.EqualValues(t, 1, refunds)
			}
			if scenario == "completed" {
				assert.EqualValues(t, 1, consumes)
				assert.Zero(t, refunds)
			}
			if scenario == "lost receipt" {
				assert.Zero(t, refunds)
			}
		})
	}
}

func TestPresetVideoCustomModelDurableLifecycle(t *testing.T) {
	const publicModel, upstreamModel = "custom-public-fixture", "future-vendor-model"
	for _, provider := range []string{"drama", "yali", "tabtab", "nldw", "yuxibox", "tianyue"} {
		for _, unit := range []string{"per_request", "per_second"} {
			t.Run(provider+"/"+unit, func(t *testing.T) {
				user, token := setupResponsesWSRequestTest(t)
				db := setupVideoRetryTestDatabase(t, user, token)
				require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Task{}, &model.TaskPlugin{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.MediaRetryEvent{}, &model.Option{}, &model.UserSubscription{}, &model.AsyncImageUsageProjection{}, &model.AsyncImageLogReceipt{}, &model.CanvasGrant{}, &model.Log{}, &model.MediaRetryPolicyVersion{}, &model.MediaArtifactObject{}, &model.MediaInputObject{}))
				t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "custom-video:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
				t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "custom-video")
				previousRegistry, previousSubmit, previousAdaptor := pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc, service.GetTaskAdaptorFunc
				previousSecret, previousEnabled := common.CryptoSecret, constant.TaskPluginEnabled
				previousBatch, previousLogs := common.BatchUpdateEnabled, common.LogConsumeEnabled
				pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc = pluginruntime.NewRegistry(), ExecuteAsyncVideo
				service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
				common.CryptoSecret, constant.TaskPluginEnabled = "custom-video-signing", true
				common.BatchUpdateEnabled, common.LogConsumeEnabled = false, true
				t.Cleanup(func() {
					pluginruntime.DefaultRegistry, service.SubmitAsyncVideoFunc, service.GetTaskAdaptorFunc = previousRegistry, previousSubmit, previousAdaptor
					common.CryptoSecret, constant.TaskPluginEnabled = previousSecret, previousEnabled
					common.BatchUpdateEnabled, common.LogConsumeEnabled = previousBatch, previousLogs
				})
				redisServer := miniredis.RunT(t)
				client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
				common.RDB, common.RedisEnabled = client, true
				t.Cleanup(func() { assert.NoError(t, client.Close()) })
				cfg := service.DefaultMediaRuntimeConfig()
				cfg.VideoAsyncEnabled, cfg.LocalPath = true, t.TempDir()
				require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
				source, err := builtinplugins.Source(provider)
				require.NoError(t, err)
				_, err = pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: provider})
				require.NoError(t, err)
				baseURL, mapping := "https://8.8.8.8", `{"`+publicModel+`":"`+upstreamModel+`"}`
				ch := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "Custom video fixture", Key: "fixture_key", BaseURL: &baseURL, ModelMapping: &mapping, Models: publicModel, Group: "default", Status: common.ChannelStatusEnabled}
				zero := 0
				settings := relaydto.ChannelSettings{TaskPluginKey: provider, TaskPluginModelConfigs: map[string]relaydto.TaskVideoModelConfig{upstreamModel: {
					Duration: relaydto.TaskVideoDuration{Values: []int{5, 10}, Default: 5}, Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"}, Modes: []string{"text_to_video"},
					References: relaydto.TaskVideoReferences{Images: &zero, Videos: &zero, Audios: &zero}, BillingMode: unit,
				}}}
				ch.SetSetting(settings)
				require.NoError(t, ch.ValidateTaskVideoModelBindings())
				require.NoError(t, db.Create(&ch).Error)
				require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Group: "default", Model: publicModel, Enabled: true}).Error)
				snapshot, err := model.GetTaskPluginSyncSnapshot()
				require.NoError(t, err)
				require.NoError(t, pluginruntime.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, snapshot.VideoModels))
				expression := `u("requests") * 0.0002`
				if unit == "per_second" {
					expression = `u("seconds") * 0.00004`
				}
				withTieredBillingConfig(t, map[string]string{publicModel: "tiered_expr"}, map[string]string{publicModel: expression})
				var submits, polls atomic.Int32
				service.InitHttpClient()
				httpClient := service.GetHttpClient()
				oldTransport := httpClient.Transport
				httpClient.Transport = huahuoVideoRoundTrip(func(request *http.Request) (*http.Response, error) {
					if request.Body != nil {
						defer request.Body.Close()
					}
					recorder := httptest.NewRecorder()
					recorder.Header().Set("Content-Type", "application/json")
					assert.Equal(t, "Bearer fixture_key", request.Header.Get("Authorization"))
					submitPath, queryPath := "/v1/videos", "/v1/videos/custom_upstream_id"
					if provider == "tabtab" {
						submitPath, queryPath = "/api/task/create", "/api/task/status?id=custom_upstream_id"
					}
					switch request.Method + " " + request.URL.RequestURI() {
					case "POST " + submitPath:
						submits.Add(1)
						var body map[string]any
						assert.NoError(t, common.DecodeJson(request.Body, &body))
						assert.Equal(t, upstreamModel, body["model"])
						switch provider {
						case "tabtab":
							args, ok := body["args"].(map[string]any)
							assert.True(t, ok)
							assert.EqualValues(t, 5, args["duration"])
						case "tianyue":
							assert.EqualValues(t, 1, body["duration"])
							assert.EqualValues(t, 5, body["video_duration"])
						default:
							assert.EqualValues(t, 5, body["seconds"])
						}
						if provider == "tabtab" {
							_, _ = io.WriteString(recorder, `{"code":"OK","data":{"id":"custom_upstream_id","status":"processing"}}`)
						} else {
							_, _ = io.WriteString(recorder, `{"id":"custom_upstream_id","status":"queued"}`)
						}
					case "GET " + queryPath:
						polls.Add(1)
						if provider == "tabtab" {
							_, _ = io.WriteString(recorder, `{"code":"OK","data":{"id":"custom_upstream_id","status":"succeeded","results":[{"url":"https://8.8.8.8/video.mp4","mime_type":"video/mp4"}]}}`)
						} else {
							_, _ = io.WriteString(recorder, `{"id":"custom_upstream_id","status":"completed","seconds":3,"url":"https://8.8.8.8/video.mp4"}`)
						}
					default:
						t.Errorf("unexpected %s request %s %s", provider, request.Method, request.URL)
						recorder.WriteHeader(http.StatusNotFound)
					}
					return recorder.Result(), nil
				})
				t.Cleanup(func() { httpClient.Transport = oldTransport })
				engine := gin.New()
				engine.POST("/v1/videos/generations_async", middleware.TokenAuth(), PrepareAsyncVideo, PinAsyncVideoEndpoint(), FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), RouteAsyncVideo, middleware.Distribute(), AcceptAsyncVideo)
				inputJSON, err := common.Marshal(map[string]any{"provider": provider, "model": publicModel, "prompt": "fixture", "seconds": 5, "resolution": "720p", "aspect_ratio": "16:9"})
				require.NoError(t, err)
				for range 2 {
					recorder := httptest.NewRecorder()
					request := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", bytes.NewReader(inputJSON))
					request.Header.Set("Authorization", "Bearer sk-"+token.Key)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Idempotency-Key", "custom-client-fixture")
					engine.ServeHTTP(recorder, request)
					require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())
				}
				var job model.AsyncMediaJob
				require.NoError(t, db.Where("user_id = ?", user.Id).Take(&job).Error)
				changed := settings.TaskPluginModelConfigs[upstreamModel]
				changed.Duration = relaydto.TaskVideoDuration{Values: []int{10}, Default: 10}
				settings.TaskPluginModelConfigs[upstreamModel] = changed
				ch.SetSetting(settings)
				require.NoError(t, db.Model(&ch).Update("setting", ch.Setting).Error)
				snapshot, err = model.GetTaskPluginSyncSnapshot()
				require.NoError(t, err)
				require.NoError(t, pluginruntime.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, snapshot.VideoModels))
				withTieredBillingConfig(t, map[string]string{publicModel: "tiered_expr"}, map[string]string{publicModel: expression + " * 100"})
				claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "custom-submit", 120)
				require.NoError(t, err)
				require.True(t, claimed)
				require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), job, cfg))
				var task model.Task
				require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&task).Error)
				assert.Equal(t, upstreamModel, task.Properties.UpstreamModelName)
				assert.Equal(t, 100, task.Quota)
				staleTask := task
				for _, current := range []*model.Task{&task, &staleTask} {
					id := current.GetUpstreamTaskID()
					require.NoError(t, service.UpdateVideoTasks(t.Context(), current.Platform, map[int][]string{ch.Id: {id}}, map[string]*model.Task{id: current}))
				}
				require.NoError(t, db.First(&task, task.ID).Error)
				assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
				assert.Equal(t, 100, task.Quota, "completion must retain the admitted quantity and price")
				assert.EqualValues(t, 1, submits.Load())
				assert.Positive(t, polls.Load())
				require.NoError(t, db.First(token, token.Id).Error)
				assert.Zero(t, token.RemainQuota)
				var consumes, refunds int64
				require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).Count(&consumes).Error)
				require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", user.Id, model.LogTypeRefund).Count(&refunds).Error)
				assert.EqualValues(t, 1, consumes)
				assert.Zero(t, refunds)
			})
		}
	}
}

func setupTaskSubmissionDatabase(t *testing.T, migrate bool, events *[]string) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.Callback().Create().Before("gorm:create").Register("test:task-submit-order", func(*gorm.DB) {
		*events = append(*events, "insert")
	}))
	if migrate {
		require.NoError(t, database.AutoMigrate(&model.Task{}))
	}
	model.DB = database
	t.Cleanup(func() { model.DB = previousDB })
	return database
}

func taskSubmissionTestContext() *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/plugin/submit", strings.NewReader(`{}`))
	return c
}

func taskSubmissionRelayInfo(billing relaycommon.BillingSettler) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		UserId:          1,
		UsingGroup:      "default",
		OriginModelName: "plugin-model",
		Billing:         billing,
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID:  "task_public",
			LockedChannel: &model.Channel{Id: 1, Type: constant.ChannelTypeTaskPlugin, Name: "plugin"},
		},
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 1, ChannelType: constant.ChannelTypeTaskPlugin},
	}
}

// Uses the real submit adaptor, expression evaluator, BillingSession, task row
// and consume log. Set TEST_TASK_DB_DIALECT plus TEST_MYSQL_DSN or
// TEST_POSTGRES_DSN to exercise the same contract on an external test database.
// Unique table prefixes keep the fixture isolated from all existing tables.
func TestImmediateTaskSettlementDatabase(t *testing.T) {
	dialect := common.DatabaseType(os.Getenv("TEST_TASK_DB_DIALECT"))
	var driver gorm.Dialector
	switch dialect {
	case "", common.DatabaseTypeSQLite:
		dialect = common.DatabaseTypeSQLite
		driver = sqlite.Open(":memory:")
	case common.DatabaseTypeMySQL:
		require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
		driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
	case common.DatabaseTypePostgreSQL:
		require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
		driver = postgres.New(postgres.Config{DSN: os.Getenv("TEST_POSTGRES_DSN"), PreferSimpleProtocol: true})
	default:
		t.Fatalf("unsupported test dialect %q", dialect)
	}
	db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("tsubmit_%d_", time.Now().UnixNano())}})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	models := []any{&model.User{}, &model.Channel{}, &model.Task{}, &model.Log{}}
	require.NoError(t, db.AutoMigrate(models...))
	t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(models...)) })
	var version string
	if dialect == common.DatabaseTypeSQLite {
		require.NoError(t, db.Raw("SELECT sqlite_version()").Scan(&version).Error)
	} else {
		require.NoError(t, db.Raw("SELECT version()").Scan(&version).Error)
	}
	t.Logf("database: %s %s", dialect, version)
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldMain, oldLog := common.MainDatabaseType(), common.LogDatabaseType()
	oldRedis, oldMemory, oldBatch, oldConsume, oldExport := common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(dialect, dialect)
	common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled = false, false, false, true, false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.SetDatabaseTypes(oldMain, oldLog)
		common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled = oldRedis, oldMemory, oldBatch, oldConsume, oldExport
	})

	const expression = `u("units") == 7 ? tier("missing", u("missing") * 1.0) : u("units") == 8 ? tier("invalid", -1.0) : u("units") > 3 ? tier("bulk", u("units") * 0.01) : tier("small", u("units") * 0.01)`
	withTieredBillingConfig(t, map[string]string{"document-model": "tiered_expr"}, map[string]string{"document-model": expression})
	const source = `
export const meta={apiVersion:1,key:"generic-settlement",name:"Generic settlement",version:"1.0.0",author:{name:"Test"},models:["document-model"],fetchMode:"per_task",usageSchema:{units:{type:"number",unit:"count"}}};
export function buildSubmitRequest(ctx){return {url:ctx.baseUrl+"/compile",body:ctx.requestBody};}
export function parseSubmitResponse(ctx,resp){return {taskId:"vendor-job",taskData:resp.body,immediate:{status:resp.body.status,reason:"provider rejected job"}};}
export function extractUsage(){return {units:4};}
export function extractUsageOnComplete(ctx,result,body){return body.usage;}
export function parseTaskResult(){throw new Error("completed submissions must not poll");}
export function buildQueryRequest(){throw new Error("completed submissions must not poll");}
`
	plugin, err := pluginruntime.CompilePlugin(source, pluginruntime.Options{})
	require.NoError(t, err)
	for index, tc := range []struct {
		name, status string
		actual       any
		count        float64
	}{
		{"partial", "SUCCESS", 2, 2}, {"zero", "SUCCESS", 0, 0}, {"larger", "SUCCESS", 6, 6},
		{"invalid usage", "SUCCESS", -1, 4}, {"expression failure", "SUCCESS", 7, 4}, {"negative result", "SUCCESS", 8, 4}, {"missing usage", "SUCCESS", nil, 4}, {"failed", "FAILURE", 9, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				body := map[string]any{"status": tc.status}
				if tc.actual != nil {
					body["usage"] = map[string]any{"units": tc.actual}
				}
				encoded, err := common.Marshal(body)
				if err != nil {
					panic(err)
				}
				_, _ = w.Write(encoded)
			}))
			defer server.Close()
			initial := int(20 * common.QuotaPerUnit)
			user := model.User{Username: fmt.Sprintf("task_user_%d", index), AffCode: fmt.Sprintf("task_aff_%d", index), Quota: initial}
			require.NoError(t, db.Create(&user).Error)
			ch := model.Channel{Name: "test provider", Type: constant.ChannelTypeTaskPlugin}
			require.NoError(t, db.Create(&ch).Error)
			c := taskSubmissionTestContext()
			c.Set("group", "default")
			c.Set("username", user.Username)
			c.Set("task_request", map[string]any{"model": "document-model"})
			c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Plugin: plugin})
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "document-model")
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
			common.SetContextKey(c, constant.ContextKeyChannelId, ch.Id)
			common.SetContextKey(c, constant.ContextKeyChannelType, ch.Type)
			info := taskSubmissionRelayInfo(nil)
			info.UserId = user.Id
			info.OriginModelName = "document-model"
			info.UserGroup = "default"
			info.UsingGroup = "default"
			info.IsPlayground = true
			info.UserSetting.BillingPreference = "wallet_only"
			info.PublicTaskID = model.GenerateTaskID()
			info.LockedChannel = &ch
			outcome, taskErr := executeTaskSubmission(c, info)
			require.Nil(t, taskErr)
			require.NotNil(t, outcome)
			want := common.QuotaRound(tc.count * 0.01 * common.QuotaPerUnit)
			assert.Equal(t, want, outcome.Result.Quota)
			assert.Equal(t, want, info.PriceData.Quota)
			var stored model.Task
			require.NoError(t, db.Where("task_id = ?", info.PublicTaskID).First(&stored).Error)
			assert.Equal(t, want, stored.Quota)
			assert.Equal(t, model.TaskStatus(tc.status), stored.Status)
			assert.Positive(t, stored.FinishTime)
			assert.Equal(t, float64(4), info.TieredBillingSnapshot.EstimatedQuotaBeforeGroup/(0.01*common.QuotaPerUnit))
			if tc.status == "SUCCESS" {
				assert.Equal(t, tc.count, stored.PrivateData.BillingContext.TieredSnapshot.UsageFacts["units"])
			}
			var updated model.User
			require.NoError(t, db.First(&updated, user.Id).Error)
			assert.Equal(t, initial-want, updated.Quota)
			assert.Equal(t, want, updated.UsedQuota)
			var logs []model.Log
			require.NoError(t, db.Where("user_id = ?", user.Id).Find(&logs).Error)
			require.Len(t, logs, 1)
			assert.Equal(t, want, logs[0].Quota)
			var other map[string]any
			require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
			if tc.status == "SUCCESS" {
				assert.Equal(t, tc.count, other["usage_facts"].(map[string]any)["units"])
			}
			assert.False(t, c.Writer.Written(), "presentation must follow persistence and settlement")
			require.NoError(t, info.Billing.Settle(want))
			info.Billing.Refund(c)
			require.NoError(t, db.First(&updated, user.Id).Error)
			assert.Equal(t, initial-want, updated.Quota, "terminal settlement is idempotent")
		})
	}
}

func TestAcceptedSubmitStreamNeverRetries(t *testing.T) {
	c := taskSubmissionTestContext()
	assert.False(t, shouldRetryTaskRelay(c, 1, &dto.TaskError{StatusCode: 502, LocalError: true, NoRetry: true}, 3))
}

func TestExecuteTaskSubmissionRefundsWhenFinalReserveFails(t *testing.T) {
	events := []string{}
	setupTaskSubmissionDatabase(t, true, &events)
	billing := &taskSubmissionTestBilling{events: &events, reserveErr: errors.New("insufficient funds")}
	c := taskSubmissionTestContext()
	info := taskSubmissionRelayInfo(billing)
	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		return &relay.TaskSubmitResult{Platform: "plugin", Quota: 600, Immediate: &relaycommon.TaskInfo{Status: "SUCCESS"}}, nil
	})
	require.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, http.StatusForbidden, taskErr.StatusCode)
	assert.Equal(t, []string{"reserve", "refund"}, events)
	assert.Equal(t, 1, billing.refunds)
	assert.False(t, c.Writer.Written())
}

func TestMixedVideoConfirmedFailureRecovery(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mixed, unknown bool
		output         bool
		response       string
	}{
		{name: "ordinary confirmed", response: `{"id":"native-terminal","status":"failed"}`},
		{name: "mixed confirmed", mixed: true, response: `{"id":"native-terminal","status":"failed"}`},
		{name: "mixed conflicting status", mixed: true, unknown: true, response: `{"id":"native-terminal","status":"processing"}`},
		{name: "mixed conflicting identity", mixed: true, unknown: true, response: `{"id":"another-native","status":"failed"}`},
		{name: "mixed output present", mixed: true, unknown: true, output: true, response: `{"id":"native-terminal","status":"failed","video_url":"https://example.invalid/result.mp4"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, token := setupResponsesWSRequestTest(t)
			db := setupVideoRetryTestDatabase(t, user, token)
			require.NoError(t, db.AutoMigrate(&model.Option{}, &model.MediaRetryPolicyVersion{}, &model.MediaRetryEvent{}, &model.Task{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}))
			t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "third:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
			t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "third")
			policy := mediaretry.Policy{Rules: []mediaretry.Rule{{ID: "terminal", Name: "Confirmed failure retry", Enabled: true, Scope: mediaretry.Scope{MediaTypes: []string{"video"}}, Match: mediaretry.Match{ContainsAny: []string{"confirmed provider failure"}}, Steps: []mediaretry.Step{{ID: "other", Action: mediaretry.OtherChannel, MaxAttempts: 1}}}}}
			_, err := model.SaveMediaRetryPolicy(t.Context(), policy, 0, user.Id, nil)
			require.NoError(t, err)
			now := time.Now().Unix()
			task := model.Task{TaskID: model.GenerateTaskID(), UserId: user.Id, ChannelId: 11, Platform: "xai", Status: model.TaskStatusSubmitted, Quota: 100, Properties: model.Properties{OriginModelName: "public-video", UpstreamModelName: "grok-imagine-video-1.5"}}
			task.PrivateData.TokenId, task.PrivateData.UpstreamTaskID = token.Id, "native-terminal"
			task.PrivateData.AsyncMedia, task.PrivateData.DurableBilling, task.PrivateData.SubmitAccountingRecorded = true, true, true
			request := service.AsyncVideoRequest{Body: []byte(`{"model":"public-video","seconds":5,"resolution":"720p","prompt":"test"}`), ContentType: "application/json", Operation: "create", ReferenceRecoveryVersion: 2, ReferenceRecoveryDeadline: now + 120, ReferenceURLExpiresAt: now + 3600}
			if tc.mixed {
				request.MixedRouting = &service.MixedVideoRouting{Group: "default", Model: "public-video", Operation: "create", Resolution: "1K"}
				task.PrivateData.CurrentAttempt = &model.VideoTaskAttempt{MixedVideo: true, PluginKey: "xai", ChannelID: 11, UpstreamModel: "grok-imagine-video-1.5"}
			}
			require.NoError(t, db.Create(&task).Error)
			raw, err := common.Marshal(request)
			require.NoError(t, err)
			cipher, err := service.EncryptImagePayload(raw, "media-request:"+task.TaskID)
			require.NoError(t, err)
			job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: common.GetUUID(), UserId: user.Id, TokenId: token.Id, ChannelId: 11, Provider: "xai", Model: "public-video", Group: "default", Status: "submitted", BillingStatus: "reserved", Quota: 100, RequestCipher: cipher, CreatedAt: now, ExpiresAt: now + 600}
			require.NoError(t, db.Create(&job).Error)
			result := &relaycommon.TaskInfo{TaskID: "native-terminal", Status: model.TaskStatusFailure, Reason: "confirmed provider failure"}
			recovered, err := service.TryAsyncVideoReferenceRecovery(t.Context(), task, result, 200, []byte(tc.response))
			require.NoError(t, err)
			require.True(t, recovered)
			require.NoError(t, db.First(&task, task.ID).Error)
			require.NoError(t, db.First(&job, job.ID).Error)
			assert.Equal(t, 100, task.Quota)
			assert.Equal(t, 100, job.Quota)
			assert.Equal(t, "reserved", job.BillingStatus)
			if tc.output {
				assert.Equal(t, model.TaskStatusSubmitted, string(task.Status))
				assert.Zero(t, task.AttemptGeneration)
				assert.Empty(t, job.ReferenceRecoveryStage)
				assert.Equal(t, "native-terminal", task.PrivateData.UpstreamTaskID)
				require.NotNil(t, task.PrivateData.VideoReconciliation)
				assert.True(t, task.PrivateData.VideoReconciliation.ManualRequired)
				require.NotNil(t, task.PrivateData.MediaRetryFact)
				assert.True(t, task.PrivateData.MediaRetryFact.Unknown)
				return
			}
			assert.Equal(t, model.TaskStatusQueued, string(task.Status))
			assert.Empty(t, task.PrivateData.UpstreamTaskID)
			require.Len(t, task.PrivateData.VideoAttempts, 1)
			expectedStatus := model.TaskStatusFailure
			if tc.unknown {
				expectedStatus = model.TaskStatusUnknown
				require.NotNil(t, task.PrivateData.MediaRetryFact)
				assert.True(t, task.PrivateData.MediaRetryFact.Unknown, "the rule does not turn uncertain evidence into confirmed failure")
			}
			assert.Equal(t, expectedStatus, task.PrivateData.VideoAttempts[0].Status)
			assert.Equal(t, "native-terminal", task.PrivateData.VideoAttempts[0].UpstreamTaskID)
			assert.Equal(t, int64(1), task.AttemptGeneration, "confirmed terminal failure must reach a new recovery generation")
			assert.Equal(t, "submit", job.ReferenceRecoveryStage)
			assert.Nil(t, task.PrivateData.VideoReconciliation, "known failed execution must not become unknown/manual")
		})
	}
}

func TestMixedVideoBuiltinInputProjection(t *testing.T) {
	const public = "third-smoke-public"
	member := func(t *testing.T, key, target string) (model.Channel, pluginruntime.ProtocolBinding, pluginruntime.VideoProfile) {
		t.Helper()
		channel := model.Channel{Id: 1, Type: constant.ChannelTypeTaskPlugin, Name: key, Key: "no-network", Status: common.ChannelStatusEnabled, Models: public, Group: "default"}
		channel.SetSetting(relaydto.ChannelSettings{TaskPluginKey: key})
		mapping, err := common.Marshal(map[string]string{public: target})
		require.NoError(t, err)
		channel.ModelMapping = common.GetPointer(string(mapping))
		binding, _, err := service.MixedVideoChannelBinding(channel, public, "create")
		require.NoError(t, err)
		profile, err := service.EffectiveMixedVideoProfile(binding, public, service.VideoModelOverrides{})
		require.NoError(t, err)
		return channel, binding, profile
	}
	for _, tc := range []struct{ name, key, target, fields string }{
		{"Tianyue one declared reference image", "tianyue", "L-sd2-video", `"seconds":5,"resolution":"720p","reference_images":["https://cdn.example/frame.png"]`},
		{"Drama primary image alias is ordinary reference", "drama", "minimax-h3", `"seconds":4,"resolution":"720p","image":"https://cdn.example/frame.png"`},
		{"Doubao native content first frame", "doubao", "doubao-seedance-1-0-pro-250528", `"seconds":5,"resolution":"720p","content":[{"type":"text","text":"animate"},{"type":"image_url","role":"first_frame","image_url":{"url":"https://cdn.example/frame.png"}}]`},
		{"Tianyue native output duration", "tianyue", "L-sd2-video", `"resolution":"720p","video_duration":4`},
		{"Tianyue native per-operation duration unit", "tianyue", "L-sd2-video", `"resolution":"720p","video_duration":4,"duration":1`},
		{"control Tianyue explicit primary image", "tianyue", "L-sd2-video", `"seconds":5,"resolution":"720p","image":"https://cdn.example/frame.png"`},
		{"control Tianyue two declared reference images", "tianyue", "L-sd2-video", `"seconds":5,"resolution":"720p","reference_images":["https://cdn.example/frame.png","https://cdn.example/other.png"]`},
		{"control Drama explicit reference image", "drama", "minimax-h3", `"seconds":4,"resolution":"720p","reference_images":["https://cdn.example/frame.png"]`},
		{"control Doubao structured first frame", "doubao", "doubao-seedance-1-0-pro-250528", `"seconds":5,"resolution":"720p","image":"https://cdn.example/frame.png"`},
		{"control Tianyue public output duration", "tianyue", "L-sd2-video", `"seconds":4,"resolution":"720p"`},
		{"control Tianyue preserves primary and reference identities", "tianyue", "L-sd2-video", `"seconds":5,"resolution":"720p","image":"https://cdn.example/frame.png","reference_images":["https://cdn.example/other.png"]`},
		{"control Drama preserves primary alias and reference identities", "drama", "minimax-h3", `"seconds":4,"resolution":"720p","image":"https://cdn.example/frame.png","reference_images":["https://cdn.example/other.png"]`},
		{"control explicit frame roles remain distinct", "drama", "seedance2.0-A", `"seconds":4,"resolution":"720p","aspect_ratio":"Auto","references":[{"type":"image","role":"first_frame","source":"https://cdn.example/frame.png"},{"type":"image","role":"last_frame","source":"https://cdn.example/other.png"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel, binding, profile := member(t, tc.key, tc.target)
			raw := []byte(fmt.Sprintf(`{"model":%q,"prompt":"animate",%s}`, public, tc.fields))
			var body map[string]any
			require.NoError(t, common.Unmarshal(raw, &body))
			value, err := binding.Plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": public, "upstreamModel": tc.target, "operation": "create", "body": map[string]any{"kind": "json", "value": body},
			})
			require.NoError(t, err, "the actual builtin decoder accepts this input")
			intent := value.(map[string]any)
			driver := map[string]any{"model": public, "upstreamModel": tc.target, "action": intent["action"], "requestBody": intent["requestBody"], "baseUrl": "https://third-smoke.invalid", "apiKey": "no-network"}
			value, err = binding.Plugin.Engine.Call(t.Context(), "buildSubmitRequest", driver)
			require.NoError(t, err, "the actual builtin builds a provider-native request")
			wire := value.(map[string]any)["body"]
			usage, err := binding.Plugin.Engine.Call(t.Context(), "extractUsage", driver)
			require.NoError(t, err)
			normalized, _, publicBody, normalizeErr := service.NormalizeMixedVideoDefaults(raw, "application/json", "create", []pluginruntime.VideoProfile{profile})
			var candidate service.MixedVideoCandidate
			decodeErr := normalizeErr
			if normalizeErr == nil {
				candidate, decodeErr = service.DecodeMixedVideoCandidate(t.Context(), channel, public, "create", normalized, service.VideoModelOverrides{}, 0)
			}
			facts, _ := service.MixedVideoPublicFacts(publicBody, "create", nil, profile)
			t.Logf("key=%s input=%s native.action=%v native.wire=%v native.usage=%v mixed.body=%v mixed.facts=%v mixed.intent=%v error=%v", tc.key, raw, intent["action"], wire, usage, publicBody, facts, candidate.Intent, decodeErr)
			require.NoError(t, decodeErr, "a real builtin's supported request must survive mixed admission")
			driver["requestBody"], driver["action"] = candidate.Intent["requestBody"], candidate.Intent["action"]
			value, err = binding.Plugin.Engine.Call(t.Context(), "buildSubmitRequest", driver)
			require.NoError(t, err)
			expected, err := common.Marshal(wire)
			require.NoError(t, err)
			actual, err := common.Marshal(value.(map[string]any)["body"])
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(actual), "mixed projection must preserve the real provider payload, including every media identity and frame role")
			if seconds, supplied := body["video_duration"]; supplied {
				assert.EqualValues(t, seconds, facts["seconds"], "a native duration unit must not replace output seconds in the public price")
			}
		})
	}
	t.Run("native duration aliases remain bounded and channel-specific", func(t *testing.T) {
		_, _, profile := member(t, "tianyue", "L-sd2-video")
		for _, fields := range []string{
			`"seconds":5,"video_duration":4`, `"duration":2,"video_duration":4`,
			`"video_duration":0`, `"video_duration":4.5`, `"video_duration":18446744073709551615`,
		} {
			raw := []byte(fmt.Sprintf(`{"model":%q,"prompt":"animate","resolution":"720p",%s}`, public, fields))
			_, _, _, err := service.NormalizeMixedVideoDefaults(raw, "application/json", "create", []pluginruntime.VideoProfile{profile})
			require.Error(t, err, fields)
		}
		for _, tc := range []struct{ key, target string }{{"google", "veo-3.1-generate-preview"}, {"xai", "grok-imagine-video-1.5"}, {"sora", "sora-2"}} {
			channel, _, profile := member(t, tc.key, tc.target)
			raw, _, _, err := service.NormalizeMixedVideoDefaults([]byte(fmt.Sprintf(`{"model":%q,"prompt":"animate","resolution":"720p","video_duration":4,"duration":1}`, public)), "application/json", "create", []pluginruntime.VideoProfile{profile})
			require.NoError(t, err)
			_, err = service.DecodeMixedVideoCandidate(t.Context(), channel, public, "create", raw, service.VideoModelOverrides{}, 0)
			require.ErrorContains(t, err, "Tianyue per-operation channel", tc.key)
		}
	})
	t.Run("unconsumed media cannot be dropped by alias projection", func(t *testing.T) {
		channel, _, profile := member(t, "google", "veo-3.1-generate-preview")
		raw, _, _, err := service.NormalizeMixedVideoDefaults([]byte(fmt.Sprintf(`{"model":%q,"prompt":"animate","seconds":8,"resolution":"720p","reference_images":["data:image/png;base64,QQ=="]}`, public)), "application/json", "create", []pluginruntime.VideoProfile{profile})
		require.NoError(t, err)
		_, err = service.DecodeMixedVideoCandidate(t.Context(), channel, public, "create", raw, service.VideoModelOverrides{}, 0)
		require.Error(t, err, "Google does not consume reference_images as a primary image")
	})
	t.Run("Sora advertised image file alias", func(t *testing.T) {
		channel, _, profile := member(t, "sora", "sora-2")
		for _, field := range []string{"input_reference", "image"} {
			t.Run(field, func(t *testing.T) {
				var data bytes.Buffer
				writer := multipart.NewWriter(&data)
				require.NoError(t, writer.WriteField("model", public))
				require.NoError(t, writer.WriteField("prompt", "animate this"))
				require.NoError(t, writer.WriteField("seconds", "8"))
				require.NoError(t, writer.WriteField("size", "1280x720"))
				file, err := writer.CreateFormFile(field, "frame.png")
				require.NoError(t, err)
				_, err = file.Write([]byte("image-fixture"))
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				raw, contentType, body, err := service.NormalizeMixedVideoDefaults(data.Bytes(), writer.FormDataContentType(), "create", []pluginruntime.VideoProfile{profile})
				require.NoError(t, err)
				candidate, err := service.DecodeMixedVideoCandidate(t.Context(), channel, public, "create", raw, service.VideoModelOverrides{}, 0, contentType)
				t.Logf("field=%s declared.image.sources=%v normalized=%v intent=%v error=%v", field, profile.Modes[1].Inputs[0].Sources, body, candidate.Intent, err)
				assert.NoError(t, err, "the advertised image/upload input must match the actual decoder file field")
			})
		}
	})
}

func TestMixedVideoSoraPublicDimensions(t *testing.T) {
	previousRegistry := pluginruntime.DefaultRegistry
	pluginruntime.DefaultRegistry = pluginruntime.NewRegistry()
	t.Cleanup(func() { pluginruntime.DefaultRegistry = previousRegistry })
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	plugin, err := pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: "sora"})
	require.NoError(t, err)
	const publicModel = "public-sora-video"
	channel := model.Channel{
		Id: 1, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled,
		Models: publicModel, Key: "fixture-key", Group: "default",
		Setting:      common.GetPointer(`{"task_plugin_key":"sora"}`),
		ModelMapping: common.GetPointer(`{"public-sora-video":"sora-2-pro"}`),
	}
	binding, _, err := service.MixedVideoChannelBinding(channel, publicModel, "create")
	require.NoError(t, err)
	profile, err := service.EffectiveMixedVideoProfile(binding, publicModel, service.VideoModelOverrides{})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, fields, size string
		seconds            float64
	}{
		{name: "public defaults", size: "720x1280", seconds: 4},
		{name: "public landscape", fields: `,"seconds":8,"resolution":"720p","aspect_ratio":"16:9"`, size: "1280x720", seconds: 8},
		{name: "native portrait", fields: `,"seconds":12,"size":"720x1280"`, size: "720x1280", seconds: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"model":%q,"prompt":"animate"%s}`, publicModel, tc.fields))
			normalized, _, body, err := service.NormalizeMixedVideoDefaults(raw, "application/json", "create", []pluginruntime.VideoProfile{profile})
			require.NoError(t, err)
			candidate, err := service.DecodeMixedVideoCandidate(t.Context(), channel, publicModel, "create", normalized, service.VideoModelOverrides{}, 0)
			require.NoError(t, err)
			facts, err := service.MixedVideoPublicFacts(body, "create", nil, profile)
			require.NoError(t, err)
			assert.Equal(t, "720p", facts["resolution"])
			assert.EqualValues(t, tc.seconds, facts["seconds"])
			ctx := map[string]any{"model": publicModel, "upstreamModel": "sora-2-pro", "requestBody": candidate.Intent["requestBody"], "baseUrl": "https://example.invalid", "apiKey": "fixture-key"}
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
			require.NoError(t, err)
			wire := value.(map[string]any)["body"].(map[string]any)
			assert.Equal(t, tc.size, wire["size"])
			assert.EqualValues(t, tc.seconds, wire["seconds"])
			assert.NotContains(t, wire, "resolution")
			assert.NotContains(t, wire, "aspect_ratio")
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err)
			assert.Equal(t, tc.size, value.(map[string]any)["size"])
			assert.EqualValues(t, tc.seconds, value.(map[string]any)["seconds"])
		})
	}
	for _, fields := range []string{
		`,"seconds":8,"resolution":"1080p"`,
		`,"seconds":8,"size":"1792x1024"`,
		`,"seconds":8,"size":"1024x1792"`,
	} {
		raw := []byte(fmt.Sprintf(`{"model":%q,"prompt":"animate"%s}`, publicModel, fields))
		normalized, _, _, err := service.NormalizeMixedVideoDefaults(raw, "application/json", "create", []pluginruntime.VideoProfile{profile})
		if err == nil {
			_, err = service.DecodeMixedVideoCandidate(t.Context(), channel, publicModel, "create", normalized, service.VideoModelOverrides{}, 0)
		}
		require.Error(t, err, "unsupported mixed dimensions must fail before a retail reservation: %s", fields)
	}
}

func TestSoraVideoPolicyAspectRatioAdmission(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	db := setupVideoRetryTestDatabase(t, user, token)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}, &model.TaskPerfSample{}, &model.AsyncMediaJob{}))
	previousRegistry := pluginruntime.DefaultRegistry
	pluginruntime.DefaultRegistry = pluginruntime.NewRegistry()
	t.Cleanup(func() { pluginruntime.DefaultRegistry = previousRegistry })
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	plugin, err := pluginruntime.DefaultRegistry.RegisterFactory(source, pluginruntime.Options{Key: "sora"})
	require.NoError(t, err)
	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "sora-policy", Status: common.ChannelStatusEnabled, Models: "sora-2-pro", Group: "default", Key: "fixture-key", Setting: common.GetPointer(`{"task_plugin_key":"sora"}`)}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Group: "default", Model: "sora-2-pro", Enabled: true}).Error)
	policy := model.ImageGroupPolicy{Group: "default", Platform: "sora", PolicyKey: service.ImageIdentityHash("default", "sora"), Enabled: true, AsyncEnabled: true, PoolMode: "resolution", Models: `[{"id":"sora-2-pro","media_type":"video"}]`}
	require.NoError(t, db.Create(&policy).Error)
	engine := gin.New()
	engine.POST("/v1/videos/generations_async", func(c *gin.Context) {
		c.Set("token_id", token.Id)
		c.Set("id", user.Id)
		c.Next()
	}, PrepareAsyncVideo, PinAsyncVideoEndpoint(), FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), RouteAsyncVideo, func(c *gin.Context) {
		wire, err := plugin.Engine.Call(c.Request.Context(), "buildSubmitRequest", map[string]any{
			"model": "sora-2-pro", "upstreamModel": "sora-2-pro", "requestBody": c.MustGet("task_request"), "action": c.GetString("task_action"), "baseUrl": "https://example.invalid",
		})
		require.NoError(t, err)
		usage, err := plugin.Engine.Call(c.Request.Context(), "extractUsage", map[string]any{
			"model": "sora-2-pro", "upstreamModel": "sora-2-pro", "requestBody": c.MustGet("task_request"), "action": c.GetString("task_action"),
		})
		require.NoError(t, err)
		c.JSON(http.StatusOK, gin.H{"wire": wire, "usage": usage})
	})
	for _, tc := range []struct {
		name, allowed, fields, wantSize string
		errorText                       string
		durationOnly, wantSeconds       int
		multipart                       bool
	}{
		{name: "unrestricted default", wantSize: "720x1280"},
		{name: "native high landscape preserved", fields: `,"size":"1792x1024"`, wantSize: "1792x1024"},
		{name: "native high portrait preserved", fields: `,"size":"1024x1792"`, wantSize: "1024x1792"},
		{name: "allowed public landscape", allowed: "16:9", fields: `,"aspect_ratio":"16:9"`, wantSize: "1280x720"},
		{name: "forbidden public portrait", allowed: "16:9", fields: `,"aspect_ratio":"9:16"`},
		{name: "forbidden camel case alias", allowed: "16:9", fields: `,"aspectRatio":"9:16"`},
		{name: "forbidden metadata alias", allowed: "16:9", fields: `,"metadata":{"ratio":"9:16"}`},
		{name: "forbidden native portrait", allowed: "16:9", fields: `,"size":"720x1280"`},
		{name: "forbidden native high portrait", allowed: "16:9", fields: `,"size":"1024x1792"`},
		{name: "forbidden provider default", allowed: "16:9"},
		{name: "allowed native high portrait", allowed: "9:16", fields: `,"size":"1024x1792"`, wantSize: "1024x1792"},
		{name: "forbidden native high landscape", allowed: "9:16", fields: `,"size":"1792x1024"`},
		{name: "multipart forbidden portrait", allowed: "16:9", fields: `,"aspect_ratio":"9:16"`, multipart: true},
		{name: "multipart allowed landscape", allowed: "16:9", fields: `,"aspect_ratio":"16:9"`, wantSize: "1280x720", multipart: true},
		{name: "allowed native duration", durationOnly: 12, fields: `,"seconds":12`, wantSize: "720x1280", wantSeconds: 12},
		{name: "allowed metadata duration", durationOnly: 12, fields: `,"metadata":{"duration":12}`, wantSize: "720x1280", wantSeconds: 12},
		{name: "allowed metadata seconds", durationOnly: 12, fields: `,"metadata":{"seconds":"12"}`, wantSize: "720x1280", wantSeconds: 12},
		{name: "matching duration aliases", durationOnly: 12, fields: `,"seconds":12,"metadata":{"duration":12}`, wantSize: "720x1280", wantSeconds: 12},
		{name: "metadata cannot mask duration", durationOnly: 12, fields: `,"seconds":4,"metadata":{"duration":12}`, errorText: "must match"},
		{name: "native cannot mask metadata duration", durationOnly: 12, fields: `,"seconds":12,"metadata":{"duration":4}`, errorText: "must match"},
		{name: "forbidden native duration", durationOnly: 12, fields: `,"seconds":4`, errorText: "outside the enabled capability"},
		{name: "forbidden default duration", durationOnly: 12, errorText: "outside the enabled capability"},
		{name: "multipart metadata duration", durationOnly: 12, fields: `,"metadata":{"duration":12}`, wantSize: "720x1280", wantSeconds: 12, multipart: true},
		{name: "multipart conflicting duration", durationOnly: 12, fields: `,"seconds":4,"metadata":{"duration":12}`, errorText: "must match", multipart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := `[{"id":"sora-2-pro","media_type":"video"}]`
			if tc.allowed != "" {
				models = fmt.Sprintf(`[{"id":"sora-2-pro","media_type":"video","video_overrides":{"aspect_ratios":[%q]}}]`, tc.allowed)
			}
			if tc.durationOnly > 0 {
				models = fmt.Sprintf(`[{"id":"sora-2-pro","media_type":"video","video_overrides":{"duration":{"values":[%d],"default":%d}}}]`, tc.durationOnly, tc.durationOnly)
			}
			require.NoError(t, db.Model(&policy).Update("models", models).Error)
			raw := []byte(`{"provider":"sora","model":"sora-2-pro","prompt":"animate"` + tc.fields + `}`)
			contentType := "application/json"
			if tc.multipart {
				var fields map[string]any
				require.NoError(t, common.Unmarshal(raw, &fields))
				var data bytes.Buffer
				writer := multipart.NewWriter(&data)
				for key, value := range fields {
					text := fmt.Sprint(value)
					if key == "metadata" {
						encoded, err := common.Marshal(value)
						require.NoError(t, err)
						text = string(encoded)
					}
					require.NoError(t, writer.WriteField(key, text))
				}
				require.NoError(t, writer.Close())
				raw, contentType = data.Bytes(), writer.FormDataContentType()
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/videos/generations_async", bytes.NewReader(raw))
			request.Header.Set("Content-Type", contentType)
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if tc.wantSize == "" {
				require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
				message := tc.errorText
				if message == "" {
					message = "not enabled"
				}
				assert.Contains(t, response.Body.String(), message)
				return
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var wire struct {
				Wire struct {
					Body map[string]any `json:"body"`
				} `json:"wire"`
				Usage map[string]any `json:"usage"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &wire))
			assert.Equal(t, tc.wantSize, wire.Wire.Body["size"])
			assert.NotContains(t, wire.Wire.Body, "aspect_ratio", "policy metadata must not leak into the upstream wire body")
			if tc.wantSeconds > 0 {
				assert.EqualValues(t, tc.wantSeconds, wire.Wire.Body["seconds"])
				assert.Equal(t, wire.Wire.Body["seconds"], wire.Usage["seconds"], "admitted duration must match both the provider request and the bill")
			}
		})
	}
}
