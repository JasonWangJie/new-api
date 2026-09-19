package relay

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAsyncImageAdaptorCapabilitiesAndConversion(t *testing.T) {
	for _, item := range []struct {
		channel                   int
		model, provider, protocol string
		edit                      bool
	}{
		{constant.ChannelTypeOpenAI, "gpt-image-2", "openai", "openai_images", true},
		{constant.ChannelTypeOpenAIMax, "private-image-model", "openai", "openai_images", true},
		{constant.ChannelTypeCustom, "private-image-model", "openai", "openai_images", true},
		{constant.ChannelTypeAzure, "gpt-image-2", "azure", "openai_images", true},
		{constant.ChannelTypeAli, "qwen-image", "ali", "openai_images", true},
		{constant.ChannelTypeVolcEngine, "doubao-seedream-4-0-250828", "volcengine", "openai_images", false},
		{constant.ChannelTypeVolcEngine, "seedream-4-0-250828", "volcengine", "openai_images", false},
		{constant.ChannelTypeJimeng, "jimeng_high_aes_general_v21_L", "jimeng", "openai_images", false},
		{constant.ChannelTypeMiniMax, "image-01", "minimax", "openai_images", false},
		{constant.ChannelTypeXai, "grok-2-image", "xai", "openai_images", false},
		{constant.ChannelTypeReplicate, "black-forest-labs/flux-schnell", "replicate", "openai_images", true},
		{constant.ChannelTypeZhipu_v4, "cogview-4", "zhipu_v4", "openai_images", false},
		{constant.ChannelTypeSiliconFlow, "Kwai-Kolors/Kolors", "siliconflow", "openai_images", false},
		{constant.ChannelTypeGemini, "imagen-4.0-generate-001", "gemini", "openai_images", false},
		{constant.ChannelTypeVertexAi, "imagen-4.0-generate-001", "vertex", "openai_images", false},
		{constant.ChannelTypeGemini, "gemini-2.5-flash-image", "gemini", "gemini_native", true},
		{constant.ChannelTypeVertexAi, "gemini-2.5-flash-image", "vertex", "gemini_native", true},
		{constant.ChannelTypeGemini, "gemini-2.0-flash-exp-image-generation", "gemini", "gemini_native", true},
		{constant.ChannelTypeVertexAi, "gemini-2.0-flash-exp-image-generation", "vertex", "gemini_native", true},
		{constant.ChannelTypeOpenRouter, "gpt-image-2", "openrouter", "openai_images", true},
		{constant.ChannelTypeXinference, "gpt-image-2", "xinference", "openai_images", true},
		{constant.ChannelTypeMoonshot, "private-image-model", "moonshot", "openai_images", true},
		{constant.ChannelTypeNewAPI, "gpt-image-2", "newapi", "openai_images", true},
		{constant.ChannelTypeSub2API, "gpt-image-2", "sub2api", "openai_images", true},
	} {
		t.Run(item.provider+"/"+item.model, func(t *testing.T) {
			capability := imageChannelCapability(model.Channel{Type: item.channel}, item.model)
			require.True(t, capability.Generate)
			assert.Equal(t, item.provider, capability.Provider)
			assert.Equal(t, item.protocol, capability.Protocol)
			assert.Equal(t, item.edit, capability.Edit)
			mapping, err := common.Marshal(map[string]string{"studio-alias": item.model})
			require.NoError(t, err)
			mappingText := string(mapping)
			mapped := imageChannelCapability(model.Channel{Type: item.channel, ModelMapping: &mappingText}, "studio-alias")
			assert.Equal(t, capability, mapped)
			if item.protocol == "gemini_native" {
				return
			}
			apiType, _ := common.ChannelType2APIType(item.channel)
			adaptor := GetAdaptor(apiType)
			require.NotNil(t, adaptor)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: item.channel, ApiType: apiType, UpstreamModelName: item.model}, OriginModelName: item.model, RelayMode: relayconstant.RelayModeImagesGenerations}
			adaptor.Init(info)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Set("async_image_execution", true)
			c.Set("async_image_upstream_job", func(string) error { return nil })
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader("{}"))
			c.Request.Header.Set("Content-Type", "application/json")
			n, watermark := uint(1), false
			converted, err := adaptor.ConvertImageRequest(c, info, dto.ImageRequest{Model: item.model, Prompt: "a blue river", N: &n, Size: "1024x1024", Watermark: &watermark, Extra: map[string]common.RawMessage{"seed": []byte("0")}})
			require.NoError(t, err)
			wire, err := common.Marshal(converted)
			require.NoError(t, err)
			assert.Contains(t, string(wire), "a blue river")
			body := `{"data":[{"url":"https://example.com/result.png"}]}`
			switch item.channel {
			case constant.ChannelTypeAli:
				body = `{"output":{"choices":[{"message":{"content":[{"image":"https://example.com/result.png"}]}}]},"usage":{"image_count":1}}`
			case constant.ChannelTypeJimeng:
				body = `{"code":10000,"data":{"image_urls":["https://example.com/result.png"]}}`
			case constant.ChannelTypeMiniMax:
				body = `{"base_resp":{"status_code":0},"data":{"image_urls":["https://example.com/result.png"]}}`
			case constant.ChannelTypeReplicate:
				body = `{"id":"prediction-1","status":"succeeded","output":["https://example.com/result.png"]}`
			case constant.ChannelTypeGemini, constant.ChannelTypeVertexAi:
				body = `{"predictions":[{"bytesBase64Encoded":"AA=="}]}`
			}
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
			_, responseErr := adaptor.DoResponse(c, response, info)
			require.Nil(t, responseErr)
			var result dto.ImageResponse
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
			require.Len(t, result.Data, 1)
			if _, polling := adaptor.(channel.AsyncImagePollingProvider); polling {
				assert.Contains(t, []int{constant.ChannelTypeAli, constant.ChannelTypeReplicate}, item.channel)
			}
		})
	}
	for _, unsupported := range []int{constant.ChannelTypeUnknown, constant.ChannelTypeAnthropic, constant.ChannelTypeBaidu, constant.ChannelTypeOllama, constant.ChannelTypeMidjourney, constant.ChannelTypeMidjourneyPlus, constant.ChannelTypeKling, constant.ChannelTypeSora, constant.ChannelTypeTaskPlugin} {
		assert.False(t, imageChannelCapability(model.Channel{Type: unsupported}, "made-up-image-model").Generate)
	}
	assert.False(t, imageChannelCapability(model.Channel{Type: constant.ChannelTypeGemini}, "gemini-2.5-pro").Generate)
	assert.False(t, imageChannelCapability(model.Channel{Type: constant.ChannelTypeAli}, "qwen-plus").Generate)
	assert.True(t, service.ImageChannelSupportsPlatform(model.Channel{Type: constant.ChannelTypeVertexAi}, "imagen-4.0-generate-001", "openai"))
	assert.False(t, service.ImageChannelSupportsPlatform(model.Channel{Type: constant.ChannelTypeVertexAi}, "imagen-4.0-generate-001", "gemini"))
	assert.True(t, service.ImageChannelSupportsPlatform(model.Channel{Type: constant.ChannelTypeVertexAi}, "gemini-2.5-flash-image", "gemini"))
	assert.False(t, service.ImageChannelSupportsPlatform(model.Channel{Type: constant.ChannelTypeVertexAi}, "gemini-2.5-flash-image", "openai"))
	advancedSettings, err := common.Marshal(dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: "/v1/images/generations", Models: []string{"private-image-model"}}}}})
	require.NoError(t, err)
	advanced := imageChannelCapability(model.Channel{Type: constant.ChannelTypeAdvancedCustom, OtherSettings: string(advancedSettings)}, "private-image-model")
	assert.True(t, advanced.Generate)
	assert.Equal(t, "advanced_custom", advanced.Provider)
}

func TestAsyncImageExtensionParametersAndQuantityBounds(t *testing.T) {
	cfg := service.DefaultImageRuntimeConfig()
	jsonBody := `{"provider":"minimax","model":"image-01","prompt":"a river","size":"1024x1024","seed":0,"prompt_optimizer":false,"aspect_ratio":"16:9","parameters":{"prompt_extend":false}}`
	parsed, err := service.ParseAsyncImageRequest([]byte(jsonBody), "application/json", "/v1/images/generations_async", cfg)
	require.NoError(t, err)
	assert.NotContains(t, parsed.Native, "provider")
	assert.Equal(t, "0", string(parsed.Native["seed"]))
	assert.Equal(t, "false", string(parsed.Native["prompt_optimizer"]))
	assert.Equal(t, `"16:9"`, string(parsed.Native["aspect_ratio"]))
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for name, value := range map[string]string{"provider": "minimax", "model": "image-01", "prompt": "a river", "seed": "0", "prompt_optimizer": "false", "parameters": `{"prompt_extend":false}`} {
		require.NoError(t, writer.WriteField(name, value))
	}
	require.NoError(t, writer.Close())
	parsed, err = service.ParseAsyncImageRequest(form.Bytes(), writer.FormDataContentType(), "/v1/images/generations_async", cfg)
	require.NoError(t, err)
	assert.Equal(t, "0", string(parsed.Native["seed"]))
	assert.Equal(t, "false", string(parsed.Native["prompt_optimizer"]))
	assert.JSONEq(t, `{"prompt_extend":false}`, string(parsed.Native["parameters"]))
	for _, quantity := range []string{`"n":129`, `"n":18446744073709551615`, `"parameters":{"n":129}`, `"batch_size":129`, `"input":{"num_outputs":129}`, `"sequential_image_generation_options":{"max_images":129}`, `"n":2,"input":{"num_outputs":3}`} {
		_, err := service.ParseAsyncImageRequest([]byte(`{"model":"gpt-image-2","prompt":"a river",`+quantity+`}`), "application/json", "/v1/images/generations_async", cfg)
		require.Error(t, err, quantity)
	}
	_, err = service.ParseAsyncImageRequest([]byte(`{"model":"gpt-image-2","prompt":"edit"}`), "application/json", "/v1/images/edits_async", cfg)
	require.Error(t, err)
}

func TestAsyncImageUpstreamJobPolling(t *testing.T) {
	service.InitHttpClient()
	for _, item := range []struct {
		channel                  int
		model, pending, complete string
	}{
		{constant.ChannelTypeAli, "wanx-v1", `{"output":{"task_id":"job-1","task_status":"PENDING"}}`, `{"output":{"task_id":"job-1","task_status":"SUCCEEDED","results":[{"url":"https://example.com/result.png"}]}}`},
		{constant.ChannelTypeReplicate, "black-forest-labs/flux-schnell", `{"id":"job-1","status":"processing"}`, `{"id":"job-1","status":"succeeded","output":["https://example.com/result.png"]}`},
	} {
		t.Run(item.model, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "Bearer frozen-key", r.Header.Get("Authorization"))
				assert.True(t, strings.HasSuffix(r.URL.Path, "/job-1"))
				_, err := io.WriteString(w, item.complete)
				assert.NoError(t, err)
			}))
			defer server.Close()
			apiType, _ := common.ChannelType2APIType(item.channel)
			adaptor := GetAdaptor(apiType)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: item.channel, ApiType: apiType, UpstreamModelName: item.model, ChannelBaseUrl: server.URL, ApiKey: "frozen-key"}, OriginModelName: item.model, RelayMode: relayconstant.RelayModeImagesGenerations}
			adaptor.Init(info)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader("{}"))
			c.Set("async_image_execution", true)
			storedID := ""
			c.Set("async_image_upstream_job", func(id string) error { storedID = id; return nil })
			_, responseErr := adaptor.DoResponse(c, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(item.pending))}, info)
			require.NotNil(t, responseErr)
			var pending *service.AsyncImagePending
			require.True(t, errors.As(responseErr, &pending))
			require.Equal(t, "job-1", storedID)
			c.Set("async_image_resume_task", storedID)
			response, err := adaptor.(channel.AsyncImagePollingProvider).PollImage(c, info, storedID)
			require.NoError(t, err)
			_, responseErr = adaptor.DoResponse(c, response, info)
			require.Nil(t, responseErr)
			var result dto.ImageResponse
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
			require.Len(t, result.Data, 1)
		})
	}
}
