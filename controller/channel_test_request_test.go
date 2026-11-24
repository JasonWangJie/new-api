package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/ali"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func convertChatCompatibilityRequest(t *testing.T, request *dto.GeneralOpenAIRequest, channelType int, mapping map[string]string) []byte {
	t.Helper()
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })
	settings := model_setting.GetGlobalSettings()
	oldPassThrough, oldBlacklist := settings.PassThroughRequestEnabled, settings.ThinkingModelBlacklist
	oldEffortTailModels := settings.EffortTailModelIDs
	settings.PassThroughRequestEnabled = false
	settings.ThinkingModelBlacklist = nil
	settings.EffortTailModelIDs = []string{"gpt-5.1-codex-max"}
	t.Cleanup(func() {
		settings.PassThroughRequestEnabled = oldPassThrough
		settings.ThinkingModelBlacklist = oldBlacklist
		settings.EffortTailModelIDs = oldEffortTailModels
	})

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if mapping != nil {
		encoded, err := common.Marshal(mapping)
		require.NoError(t, err)
		c.Set("model_mapping", string(encoded))
	}
	info := &relaycommon.RelayInfo{
		OriginModelName: request.Model,
		Request:         request,
		RelayFormat:     types.RelayFormatOpenAI,
		IsStream:        request.IsStream(nil),
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:          channelType,
			UpstreamModelName:    request.Model,
			SupportStreamOptions: true,
		},
	}
	require.NoError(t, helper.ModelMappedHelper(c, info, request))
	require.NoError(t, helper.ApplyReasoningModelSuffix(c, info, request))
	var converted any
	var err error
	if channelType == constant.ChannelTypeAli {
		converted, err = (&ali.Adaptor{}).ConvertOpenAIRequest(c, info, request)
	} else {
		converted, err = (&openai.Adaptor{}).ConvertOpenAIRequest(c, info, request)
	}
	require.NoError(t, err)
	encoded, err := common.Marshal(converted)
	require.NoError(t, err)
	return encoded
}

func TestChannelTestOpenAIChatCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name        string
		model       string
		upstream    string
		endpoint    string
		channelType int
		stream      bool
		wantLimit   string
	}{
		{name: "GPT6 automatic", model: "gpt-6-astra", upstream: "gpt-6-astra", channelType: constant.ChannelTypeOpenAI, wantLimit: "max_completion_tokens"},
		{name: "GPT6 explicit Azure stream", model: "gpt-6-astra", upstream: "gpt-6-astra", endpoint: string(constant.EndpointTypeOpenAI), channelType: constant.ChannelTypeAzure, stream: true, wantLimit: "max_completion_tokens"},
		{name: "alias maps to GPT6", model: "customer-model", upstream: "gpt-6-astra", channelType: constant.ChannelTypeOpenAI, wantLimit: "max_completion_tokens"},
		{name: "GPT5 alias maps to Qwen", model: "gpt-5.6-luna", upstream: "qwen-turbo", channelType: constant.ChannelTypeAli, wantLimit: "max_tokens"},
		{name: "GPT5 stream", model: "gpt-5.6-luna", upstream: "gpt-5.6-luna", channelType: constant.ChannelTypeOpenAI, stream: true, wantLimit: "max_completion_tokens"},
		{name: "GPT4 explicit", model: "gpt-4.1", upstream: "gpt-4.1", endpoint: string(constant.EndpointTypeOpenAI), channelType: constant.ChannelTypeOpenAI, wantLimit: "max_tokens"},
		{name: "o series", model: "o3-mini", upstream: "o3-mini", channelType: constant.ChannelTypeAzure, wantLimit: "max_completion_tokens"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request, ok := buildTestRequest(tt.model, tt.endpoint, &model.Channel{}, tt.stream).(*dto.GeneralOpenAIRequest)
			require.True(t, ok)
			encoded := convertChatCompatibilityRequest(t, request, tt.channelType, map[string]string{tt.model: tt.upstream})
			want := map[string]any{
				"model":      tt.upstream,
				"messages":   []dto.Message{{Role: "user", Content: "hi"}},
				"stream":     tt.stream,
				tt.wantLimit: 16,
			}
			if tt.stream {
				want["stream_options"] = map[string]any{"include_usage": true}
			}
			wantJSON, err := common.Marshal(want)
			require.NoError(t, err)
			assert.JSONEq(t, string(wantJSON), string(encoded))
		})
	}
}

func TestOpenAIChatSamplingCompatibility(t *testing.T) {
	const sampling = `{"temperature":0.2,"top_p":0.8,"logprobs":true,"top_logprobs":5}`
	for _, tt := range []struct {
		name       string
		model      string
		effort     string
		reasoning  string
		mapping    map[string]string
		zeroValues bool
		wantModel  string
		wantEffort string
		wantRole   string
		wantParams string
	}{
		{name: "GPT5.1 explicit none", model: "gpt-5.1", effort: "none", wantEffort: "none", wantRole: "developer", wantParams: sampling},
		{name: "GPT5.2 default none", model: "gpt-5.2", wantRole: "developer", wantParams: sampling},
		{name: "GPT5.2 dated snapshot", model: "gpt-5.2-2025-12-11", wantRole: "developer", wantParams: sampling},
		{name: "GPT5.4 reasoning", model: "gpt-5.4", effort: "high", wantEffort: "high", wantRole: "developer", wantParams: `{}`},
		{name: "GPT5.4 snapshot none", model: "gpt-5.4-2026-03-05", effort: "none", wantEffort: "none", wantRole: "developer", wantParams: sampling},
		{name: "explicit zero values", model: "gpt-5.4", effort: "none", zeroValues: true, wantEffort: "none", wantRole: "developer", wantParams: `{"temperature":0,"top_p":0,"logprobs":false}`},
		{name: "GPT5 original", model: "gpt-5", wantRole: "developer", wantParams: `{}`},
		{name: "GPT5.6 existing policy", model: "gpt-5.6-luna", wantRole: "developer", wantParams: `{}`},
		{name: "pro variant", model: "gpt-5.2-pro-2025-12-11", wantRole: "developer", wantParams: `{}`},
		{name: "chat variant", model: "gpt-5.2-chat-latest", wantRole: "developer", wantParams: `{}`},
		{name: "codex model name keeps max", model: "gpt-5.1-codex-max", wantRole: "developer", wantParams: `{}`},
		{name: "GPT6", model: "gpt-6-astra", wantRole: "developer", wantParams: `{}`},
		{name: "GPT6 snapshot", model: "gpt-6-astra-2026-09-03", wantRole: "developer", wantParams: `{}`},
		{name: "GPT6 effort suffix", model: "gpt-6-astra-high", wantModel: "gpt-6-astra", wantEffort: "high", wantRole: "developer", wantParams: `{}`},
		{name: "none effort suffix", model: "gpt-5.2-none", wantModel: "gpt-5.2", wantEffort: "none", wantRole: "developer", wantParams: sampling},
		{name: "modifier overrides explicit effort", model: "gpt-5.2@thinking:off", effort: "high", wantModel: "gpt-5.2", wantEffort: "none", wantRole: "developer", wantParams: sampling},
		{name: "mapped modifier wins", model: "customer-model@thinking:off", mapping: map[string]string{"customer-model": "gpt-5.2@effort:high"}, wantModel: "gpt-5.2", wantEffort: "high", wantRole: "developer", wantParams: `{}`},
		{name: "nested reasoning disabled", model: "gpt-5.2", reasoning: `{"enabled":false}`, wantEffort: "none", wantRole: "developer", wantParams: sampling},
		{name: "o1 mini role exception", model: "o1-mini", wantRole: "system", wantParams: `{"top_p":0.8,"logprobs":true,"top_logprobs":5}`},
		{name: "GPT4 unchanged", model: "gpt-4.1", wantRole: "system", wantParams: sampling},
		{name: "future model unchanged", model: "gpt-7", wantRole: "system", wantParams: sampling},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := &dto.GeneralOpenAIRequest{
				Model: tt.model,
				Messages: []dto.Message{
					{Role: "system", Content: "first instruction"},
					{Role: "system", Content: "second instruction"},
					{Role: "user", Content: "hi"},
				},
				ReasoningEffort: tt.effort,
			}
			require.NoError(t, common.UnmarshalJsonStr(sampling, request))
			if tt.reasoning != "" {
				request.Reasoning = []byte(tt.reasoning)
			}
			if tt.zeroValues {
				request.Temperature = lo.ToPtr(0.0)
				request.TopP = lo.ToPtr(0.0)
				request.LogProbs = lo.ToPtr(false)
				request.TopLogProbs = nil
			}
			encoded := convertChatCompatibilityRequest(t, request, constant.ChannelTypeOpenAI, tt.mapping)
			var want map[string]any
			require.NoError(t, common.UnmarshalJsonStr(tt.wantParams, &want))
			want["model"] = tt.model
			if tt.wantModel != "" {
				want["model"] = tt.wantModel
			}
			want["messages"] = []dto.Message{
				{Role: tt.wantRole, Content: "first instruction"},
				{Role: "system", Content: "second instruction"},
				{Role: "user", Content: "hi"},
			}
			if tt.wantEffort != "" {
				want["reasoning_effort"] = tt.wantEffort
			}
			wantJSON, err := common.Marshal(want)
			require.NoError(t, err)
			assert.JSONEq(t, string(wantJSON), string(encoded))
		})
	}
}

func TestOpenAIChatTokenLimitCompatibility(t *testing.T) {
	for _, modelName := range []string{"gpt-5", "o3-mini", "gpt-6-astra"} {
		for _, tt := range []struct {
			name  string
			input string
			want  string
		}{
			{name: "omitted", input: `{}`, want: `{}`},
			{name: "legacy only", input: `{"max_tokens":100}`, want: `{"max_completion_tokens":100}`},
			{name: "completion only", input: `{"max_completion_tokens":50}`, want: `{"max_completion_tokens":50}`},
			{name: "both positive stay present", input: `{"max_tokens":100,"max_completion_tokens":50}`, want: `{"max_tokens":100,"max_completion_tokens":50}`},
			{name: "zero completion falls back", input: `{"max_tokens":100,"max_completion_tokens":0}`, want: `{"max_completion_tokens":100}`},
			{name: "legacy zero stays present", input: `{"max_tokens":0}`, want: `{"max_tokens":0}`},
			{name: "completion zero stays present", input: `{"max_completion_tokens":0}`, want: `{"max_completion_tokens":0}`},
			{name: "both zero stay present", input: `{"max_tokens":0,"max_completion_tokens":0}`, want: `{"max_tokens":0,"max_completion_tokens":0}`},
		} {
			t.Run(modelName+"/"+tt.name, func(t *testing.T) {
				request := &dto.GeneralOpenAIRequest{Model: modelName, Messages: []dto.Message{{Role: "user", Content: "hi"}}}
				require.NoError(t, common.UnmarshalJsonStr(tt.input, request))
				encoded := convertChatCompatibilityRequest(t, request, constant.ChannelTypeOpenAI, nil)
				want := dto.GeneralOpenAIRequest{Model: modelName, Messages: []dto.Message{{Role: "user", Content: "hi"}}}
				require.NoError(t, common.UnmarshalJsonStr(tt.want, &want))
				wantJSON, err := common.Marshal(want)
				require.NoError(t, err)
				assert.JSONEq(t, string(wantJSON), string(encoded))
			})
		}
	}
}

func TestDirectOpenAIResponsesKeepsExistingParameters(t *testing.T) {
	const body = `{"model":"gpt-6-astra","input":"hi","max_output_tokens":100,"temperature":0.2,"top_p":0.8,"top_logprobs":5,"include":["message.output_text.logprobs"],"reasoning":{"effort":"high"}}`
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.UnmarshalJsonStr(body, &request))
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-6-astra",
		RelayFormat:     types.RelayFormatOpenAIResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			UpstreamModelName: "gpt-6-astra",
		},
	}
	converted, err := (&openai.Adaptor{}).ConvertOpenAIResponsesRequest(nil, info, request)
	require.NoError(t, err)
	encoded, err := common.Marshal(converted)
	require.NoError(t, err)
	assert.JSONEq(t, body, string(encoded))
}

func TestInteractiveChannelTestReturnsSelectedModelOutputWithoutSiteBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, _ := newAuditTestDatabase(t, "sqlite", "")
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.User{}, &model.Log{}, &model.AsyncImageTask{}))
	previousDB, previousLog := model.DB, model.LOG_DB
	previousRedis, previousCache, previousConsumeLog := common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled
	previousStreamingTimeout := constant.StreamingTimeout
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled = false, false, true
	constant.StreamingTimeout = 10
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLog
		common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled = previousRedis, previousCache, previousConsumeLog
		constant.StreamingTimeout = previousStreamingTimeout
	})
	service.InitHttpClient()
	user := model.User{Username: "channel-tester", Group: "default", Quota: 10000}
	require.NoError(t, db.Create(&user).Error)

	var pngBody bytes.Buffer
	require.NoError(t, png.Encode(&pngBody, image.NewRGBA(image.Rect(0, 0, 3, 2))))
	imageBase64 := base64.StdEncoding.EncodeToString(pngBody.Bytes())
	geminiImage := `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"` +
		imageBase64 + `"}}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`
	const prompt = "Type and verify this test prompt"
	const overrideCredential = "provider-opaque-override-credential"
	const oauthAccessToken = "provider-opaque-oauth-access-token"
	const unknownUpstreamErrorCode = "upstream-private-opaque-code"
	for _, scenario := range []struct {
		name, model, promptPath, result string
		channelType                     int
		mode, defaultMode               string
		endpoint                        constant.EndpointType
		requestPath                     string
		expectedText                    string
		expectedErrorCode               types.ErrorCode
	}{
		{
			name: "chat", model: "unpriced-interactive-model", channelType: constant.ChannelTypeOpenAI,
			mode: channelTestModeChat, endpoint: constant.EndpointTypeOpenAI, requestPath: "/v1/chat/completions",
			promptPath: "messages.0.content", result: `{"choices":[{"message":{"role":"assistant","content":"Hello from this channel","reasoning_content":"hidden reasoning"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
			expectedText: "Hello from this channel",
		},
		{
			name: "upstream error masks raw keys and override values", model: "unpriced-interactive-model", channelType: constant.ChannelTypeOpenAI,
			mode: channelTestModeChat, endpoint: constant.EndpointTypeOpenAI, requestPath: "/v1/chat/completions",
			promptPath: "messages.0.content", result: `{"error":{"type":"upstream_error","code":"` + unknownUpstreamErrorCode +
				`","message":"Rejected fixture-credential and ` + overrideCredential + `"}}`,
			expectedText: "Rejected *** and ***", expectedErrorCode: types.ErrorCodeBadResponse,
		},
		{
			name: "Codex stream masks nested OAuth access token", model: "gpt-6.1-sol", channelType: constant.ChannelTypeCodex,
			mode: channelTestModeChat, endpoint: constant.EndpointTypeOpenAIResponse, requestPath: "/backend-api/codex/responses",
			promptPath: "input.0.content",
			result: `data: {"type":"response.output_text.delta","delta":"The reflected token was ` + oauthAccessToken + `"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":5}}}` + "\n\n",
			expectedText: "The reflected token was ***",
		},
		{
			name: "mapped OpenAI image", model: "custom-model-alias", channelType: constant.ChannelTypeOpenAI,
			mode: channelTestModeImage, endpoint: constant.EndpointTypeImageGeneration, requestPath: "/v1/images/generations",
			promptPath: "prompt", result: `{"data":[{"b64_json":"` + imageBase64 + `"}]}`,
		},
		{
			name: "native Gemini image", model: "gemini-3-pro-image-preview", channelType: constant.ChannelTypeGemini,
			mode: channelTestModeImage, endpoint: constant.EndpointTypeGemini, requestPath: "/v1beta/models/gemini-3-pro-image-preview:generateContent",
			promptPath: "contents.0.parts.0.text", result: geminiImage,
		},
		{
			name: "Gemini alias manual image mode", model: "gemini-nano-banana-2.1", channelType: constant.ChannelTypeGemini,
			mode: channelTestModeImage, defaultMode: channelTestModeChat, endpoint: constant.EndpointTypeGemini,
			requestPath: "/v1beta/models/gemini-nano-banana-2.1:generateContent", promptPath: "contents.0.parts.0.text",
			result: geminiImage,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var input common.RawMessage
				if !assert.NoError(t, common.DecodeJson(r.Body, &input)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, scenario.requestPath, r.URL.Path)
				assert.Equal(t, prompt, gjson.GetBytes(input, scenario.promptPath).String())
				switch scenario.endpoint {
				case constant.EndpointTypeImageGeneration:
					assert.Equal(t, "gpt-image-2.5-flare", gjson.GetBytes(input, "model").String())
					assert.EqualValues(t, 1, gjson.GetBytes(input, "n").Int())
				case constant.EndpointTypeGemini:
					assert.Equal(t, "IMAGE", gjson.GetBytes(input, "generationConfig.responseModalities.1").String())
				}
				if scenario.channelType == constant.ChannelTypeCodex {
					assert.Equal(t, "Bearer "+oauthAccessToken, r.Header.Get("Authorization"))
					assert.True(t, gjson.GetBytes(input, "stream").Bool())
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				if scenario.expectedErrorCode != "" {
					assert.Equal(t, overrideCredential, r.Header.Get("X-Fixture"))
				}
				_, _ = w.Write([]byte(scenario.result))
			}))
			defer upstream.Close()
			channel := model.Channel{
				Type: scenario.channelType, Models: scenario.model, BaseURL: &upstream.URL, Key: "fixture-credential",
				TestTime: 700, ResponseTime: 800, Status: common.ChannelStatusEnabled,
			}
			if scenario.endpoint == constant.EndpointTypeImageGeneration {
				channel.ModelMapping = lo.ToPtr(`{"custom-model-alias":"alias-hop","alias-hop":"gpt-image-2.5-flare"}`)
			}
			if scenario.expectedErrorCode != "" {
				channel.HeaderOverride = lo.ToPtr(`{"x-fixture":"` + overrideCredential + `"}`)
			}
			if scenario.channelType == constant.ChannelTypeCodex {
				channel.Key = `{"account_id":"fixture-account","access_token":"` + oauthAccessToken + `"}`
			}
			require.NoError(t, db.Create(&channel).Error)
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set("id", user.Id) })
			router.GET("/test/:id/options", GetChannelTestOptions)
			router.POST("/test/:id", RunInteractiveChannelTest)
			path := "/test/" + strconv.Itoa(channel.Id)
			options := httptest.NewRecorder()
			router.ServeHTTP(options, httptest.NewRequest(http.MethodGet, path+"/options", nil))
			require.Equal(t, http.StatusOK, options.Code)
			defaultMode := scenario.defaultMode
			if defaultMode == "" {
				defaultMode = scenario.mode
			}
			assert.Equal(t, defaultMode, gjson.GetBytes(options.Body.Bytes(), "data.models.0.default_mode").String())
			assert.Equal(t, string(scenario.endpoint), gjson.GetBytes(options.Body.Bytes(), "data.models.0."+scenario.mode+"_endpoint_types.0").String())

			testRequest := channelTestInput{Model: scenario.model, Mode: scenario.mode, Prompt: prompt}
			body, err := common.Marshal(testRequest)
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			var response struct {
				Success   bool              `json:"success"`
				Message   string            `json:"message"`
				ErrorCode types.ErrorCode   `json:"error_code"`
				Data      channelTestOutput `json:"data"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			if scenario.expectedErrorCode != "" {
				assert.False(t, response.Success)
				assert.Equal(t, scenario.expectedText, response.Message)
				assert.Equal(t, scenario.expectedErrorCode, response.ErrorCode)
			} else {
				require.True(t, response.Success, response.Message)
				assert.Equal(t, scenario.model, response.Data.Model)
				assert.Equal(t, scenario.endpoint, response.Data.EndpointType)
				if scenario.mode == channelTestModeChat {
					assert.Equal(t, scenario.expectedText, response.Data.Text)
				} else {
					require.NotNil(t, response.Data.Image)
					assert.Equal(t, 3, response.Data.Image.Width)
					assert.Equal(t, 2, response.Data.Image.Height)
					assert.Equal(t, "data:image/png;base64,"+imageBase64, response.Data.Image.DataURL)
				}
			}

			invalid := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"not-configured","mode":"chat"}`))
			invalid.Header.Set("Content-Type", "application/json")
			rejected := httptest.NewRecorder()
			router.ServeHTTP(rejected, invalid)
			assert.Equal(t, http.StatusBadRequest, rejected.Code)
			assert.EqualValues(t, 1, requests.Load())
			var persisted model.Channel
			require.NoError(t, db.First(&persisted, channel.Id).Error)
			assert.EqualValues(t, 700, persisted.TestTime)
			assert.EqualValues(t, 800, persisted.ResponseTime)
		})
	}
	t.Run("TabTab submits once and observes only the accepted task", func(t *testing.T) {
		const receipt = `{"code":"OK","data":{"id":"tabtab-task-fixture","status":"processing"}}`
		const success = `{"code":"OK","data":{"id":"tabtab-task-fixture","status":"succeeded","results":[{"url":"/files/image.png","mime_type":"image/png"}]}}`
		const failure = `{"code":"OK","data":{"id":"tabtab-task-fixture","status":"failed","error_message":"Fixture provider declined"}}`
		for _, scenario := range []struct {
			name, submit, poll string
			wantPolls          int32
			firstPollError     bool
			cancelAtPoll       bool
			wantError          types.ErrorCode
		}{
			{name: "successful polled preview", submit: receipt, poll: success, wantPolls: 1},
			{name: "successful terminal receipt", submit: success},
			{name: "retry only status after a transient 503", submit: receipt, poll: success, wantPolls: 2, firstPollError: true},
			{name: "terminal provider failure", submit: receipt, poll: failure, wantPolls: 1, wantError: channelTestImageFailedCode},
			{name: "canceled accepted task remains pending", submit: receipt, poll: receipt, wantPolls: 1, cancelAtPoll: true, wantError: channelTestImagePendingCode},
			{name: "ambiguous submission does not poll or resubmit", submit: `{"code":"OK","data":{"status":"processing"}}`, wantError: channelTestImageAcceptanceUnknownCode},
		} {
			t.Run(scenario.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				var submissions, polls, downloads atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "Bearer tabtab-fixture-key", r.Header.Get("Authorization"))
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/task/create":
						submissions.Add(1)
						assert.Equal(t, http.MethodPost, r.Method)
						body, err := io.ReadAll(r.Body)
						assert.NoError(t, err)
						assert.JSONEq(t, `{"model":"seedream-5.5","args":{"prompt":"`+prompt+`","size":"1K","ratio":"1:1"}}`, string(body))
						_, _ = io.WriteString(w, scenario.submit)
					case "/api/task/status":
						poll := polls.Add(1)
						assert.Equal(t, http.MethodGet, r.Method)
						assert.Equal(t, "tabtab-task-fixture", r.URL.Query().Get("id"))
						if poll == 1 && scenario.firstPollError {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						_, _ = io.WriteString(w, scenario.poll)
						if scenario.cancelAtPoll {
							w.(http.Flusher).Flush()
							cancel()
						}
					case "/files/image.png":
						downloads.Add(1)
						assert.Equal(t, http.MethodGet, r.Method)
						w.Header().Set("Content-Type", "image/png")
						_, _ = w.Write(pngBody.Bytes())
					default:
						t.Errorf("Unexpected TabTab request %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer upstream.Close()
				channel := model.Channel{
					Type: constant.ChannelTypeTabTab, Models: "seedream-5.5,seedream-5-pro,unknown",
					ModelMapping: lo.ToPtr(`{"seedream-5-pro":"seedream-5.5"}`),
					BaseURL:      &upstream.URL, Key: "tabtab-fixture-key", Status: common.ChannelStatusEnabled,
				}
				require.NoError(t, db.Create(&channel).Error)
				router := gin.New()
				router.Use(func(c *gin.Context) { c.Set("id", user.Id) })
				router.GET("/test/:id/options", GetChannelTestOptions)
				router.POST("/test/:id", RunInteractiveChannelTest)
				path := "/test/" + strconv.Itoa(channel.Id)
				options := httptest.NewRecorder()
				router.ServeHTTP(options, httptest.NewRequest(http.MethodGet, path+"/options", nil))
				require.Equal(t, http.StatusOK, options.Code)
				models := gjson.GetBytes(options.Body.Bytes(), "data.models").Array()
				require.Len(t, models, 3)
				assert.Equal(t, "image-generation", models[0].Get("image_endpoint_types.0").String())
				assert.Equal(t, "image-generation", models[1].Get("image_endpoint_types.0").String())
				assert.False(t, models[2].Get("image_endpoint_types").Exists())

				body := strings.NewReader(`{"model":"seedream-5-pro","mode":"image","prompt":"` + prompt + `"}`)
				request := httptest.NewRequestWithContext(ctx, http.MethodPost, path, body)
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				result := gjson.ParseBytes(recorder.Body.Bytes())
				assert.EqualValues(t, 1, submissions.Load())
				assert.Equal(t, scenario.wantPolls, polls.Load())
				if scenario.wantError != "" {
					assert.False(t, result.Get("success").Bool(), recorder.Body.String())
					assert.Equal(t, string(scenario.wantError), result.Get("error_code").String(), recorder.Body.String())
					assert.Zero(t, downloads.Load())
					return
				}
				require.True(t, result.Get("success").Bool(), recorder.Body.String())
				assert.EqualValues(t, 1, downloads.Load())
				assert.Equal(t, "seedream-5-pro", result.Get("data.model").String())
				assert.Equal(t, "data:image/png;base64,"+imageBase64, result.Get("data.image.data_url").String())
				assert.EqualValues(t, 3, result.Get("data.image.width").Int())
				assert.EqualValues(t, 2, result.Get("data.image.height").Int())
			})
		}
	})
	t.Run("request cancellation and expired deadline", func(t *testing.T) {
		clientContext, cancelRequest := context.WithCancel(t.Context())
		defer cancelRequest()
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), 5*time.Second)
		defer stopWatchdog()
		upstreamCanceled := make(chan error, 1)
		var attempts atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			cancelRequest()
			select {
			case <-r.Context().Done():
				upstreamCanceled <- r.Context().Err()
			case <-watchdog.Done():
			}
		}))
		defer upstream.Close()
		channel := model.Channel{
			Type: constant.ChannelTypeOpenAI, Models: "interactive-context-model",
			BaseURL: &upstream.URL, Key: "fixture-credential", Status: common.ChannelStatusEnabled,
		}
		require.NoError(t, db.Create(&channel).Error)
		run := func(ctx context.Context) gjson.Result {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Set("id", user.Id)
			c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(channel.Id)}}
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/test/"+strconv.Itoa(channel.Id),
				strings.NewReader(`{"model":"interactive-context-model","mode":"chat"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			RunInteractiveChannelTest(c)
			require.Equal(t, http.StatusOK, recorder.Code)
			response := gjson.ParseBytes(recorder.Body.Bytes())
			assert.False(t, response.Get("success").Bool())
			assert.False(t, response.Get("data").Exists())
			return response
		}

		run(clientContext)
		select {
		case err := <-upstreamCanceled:
			require.ErrorIs(t, err, context.Canceled)
		case <-watchdog.Done():
			t.Fatal("canceling the interactive request did not cancel the upstream request")
		}
		expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancelExpired()
		assert.Equal(t, "Channel test timed out", run(expired).Get("message").String())
		assert.EqualValues(t, 1, attempts.Load(), "an expired deadline must not submit another upstream request")
	})
	var consumeLogs int64
	require.NoError(t, db.Model(&model.Log{}).Count(&consumeLogs).Error)
	assert.Zero(t, consumeLogs)
	var siteTasks int64
	require.NoError(t, db.Model(&model.AsyncImageTask{}).Count(&siteTasks).Error)
	assert.Zero(t, siteTasks)
	var persistedUser model.User
	require.NoError(t, db.First(&persistedUser, user.Id).Error)
	assert.EqualValues(t, user.Quota, persistedUser.Quota)
}

func TestInteractiveChannelTestShowsAssistantTextAfterResponsesStreamReasoning(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/test/1", nil)
	reasoning := strings.Repeat("r", 9<<10)
	response := []byte(`data: {"type":"response.reasoning_summary_text.delta","delta":"` + reasoning + `"}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":"Hello"}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":" world"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello world"}]}]}}` + "\n\n")
	input := &channelTestInput{
		Model: "gpt-6.1-sol", Mode: channelTestModeChat, EndpointType: constant.EndpointTypeOpenAIResponse,
	}
	output, err := readInteractiveChannelTestOutput(c, response, input, true)
	require.NoError(t, err)
	assert.Equal(t, "Hello world", output.Text)
	partial, _, _ := bytes.Cut(response, []byte(`data: {"type":"response.completed"`))
	_, err = readInteractiveChannelTestOutput(c, partial, input, true)
	require.ErrorContains(t, err, "without a completion event")
	_, err = readInteractiveChannelTestOutput(c, append(bytes.Clone(partial), []byte(`data: {"type":"response.incomplete"}`)...), input, true)
	require.ErrorContains(t, err, "status incomplete")
}

func TestInteractiveChannelTestImagePreviewBoundaries(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/test/1", nil)
	var pixels bytes.Buffer
	require.NoError(t, png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 3, 2))))
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pixels.Bytes())
	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="3" height="2"><rect width="3" height="2" fill="red"/></svg>`
	input := &channelTestInput{
		Model: "gpt-image-2.5-flare", Mode: channelTestModeImage, EndpointType: constant.EndpointTypeImageGeneration,
	}
	for _, scenario := range []struct{ name, body, diagnostic string }{
		{name: "URL image", body: `{"data":[{"url":"` + dataURL + `"}]}`},
		{name: "empty result", body: `{"data":[]}`, diagnostic: "complete images"},
		{name: "empty image item", body: `{"data":[{}]}`, diagnostic: "upstream image data item is empty"},
		{name: "multiple images", body: `{"data":[{"url":"` + dataURL + `"},{"url":"` + dataURL + `"}]}`, diagnostic: "complete images"},
		{name: "private URL", body: `{"data":[{"url":"https://127.0.0.1/image.png"}]}`, diagnostic: "image reference address is not public"},
		{name: "SVG", body: `{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString([]byte(svg)) + `"}]}`, diagnostic: "The upstream returned SVG"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			output, err := readInteractiveChannelTestOutput(c, []byte(scenario.body), input, false)
			if scenario.diagnostic != "" {
				require.ErrorContains(t, err, scenario.diagnostic)
				assert.Nil(t, output)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, &channelTestImage{DataURL: dataURL, Width: 3, Height: 2}, output.Image)
		})
	}
}
