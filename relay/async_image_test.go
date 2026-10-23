package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
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

func TestAsyncImageUnreadableReceiptDoesNotRepeatSubmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		limit     int64
		readError bool
	}{
		{name: "truncated_json", body: `{"id":"accepted-job"`, limit: 1024},
		{name: "non_json", body: `<html>gateway error</html>`, limit: 1024},
		{name: "oversized_receipt", body: `{"id":"accepted-job"}`, limit: 4},
		{name: "read_failure", limit: 1024, readError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, upstreamID := range []string{"", "accepted-job"} {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Set("async_image_resume_task", upstreamID)
				c.Set("async_image_response_limit", tc.limit)
				var reader io.Reader = strings.NewReader(tc.body)
				if tc.readError {
					reader = iotest.ErrReader(io.ErrUnexpectedEOF)
				}
				response := &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(reader)}
				_, apiErr := handleUpstreamAsyncImageResponse(c, &relaycommon.RelayInfo{}, response, &dto.UpstreamAsyncProfile{})
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, apiErr, &failure)
				if upstreamID == "" {
					assert.Equal(t, 608, failure.Code)
					assert.Equal(t, "execution_unknown", failure.InternalCode)
					assert.True(t, failure.ExecutionUnknown, "an unreadable accepted submission must never be retried")
				} else {
					assert.Equal(t, 606, failure.Code)
					assert.False(t, failure.ExecutionUnknown, "known upstream jobs can safely retry polling")
				}
			}
		})
	}
}

func TestAsyncImageTerminalFailureClassification(t *testing.T) {
	var profile dto.UpstreamAsyncProfile
	require.NoError(t, common.UnmarshalJsonStr(`{"poll":{"response":{"status_path":"status","status_values":{"failed":["failed"]},"failure_reason_path":"reason","result_path":"urls"}}}`, &profile))
	for _, tc := range []struct {
		name, reason string
		code         int
	}{
		{name: "service_timeout", reason: "Service timeout, please try again later", code: 606},
		{name: "policy_precedes_timeout", reason: "图片审核失败，生成超时，请重试", code: 601},
		{name: "prompt_length_limit", reason: "POST /: Your prompt cannot exceed 10000 characters.", code: 604},
		{name: "busy_ascii_comma", reason: "服务繁忙,请稍后重试", code: 606},
		{name: "busy_fullwidth_comma", reason: "服务繁忙，请稍后重试", code: 606},
		{name: "asset_busy", reason: "参考image上传失败：POST / 上游服务繁忙：asset name service busy, please try again later", code: 606},
		{name: "internal_error", reason: "Internal error encountered.", code: 606},
		{name: "oversize_before_busy", reason: "参考image上传失败：参考图过大（25.4MB，上限 20MB）；服务繁忙,请稍后重试", code: 604},
		{name: "guardrail_before_busy", reason: "guardrails detection failed: 服务繁忙,请稍后重试", code: 601},
		{name: "invalid_argument_remains_conservative", reason: "AI Studio ProxyUnaryCall 返回 HTTP 400、协议错误码 3: Request contains an invalid argument.", code: 610},
		{name: "unclassified_failure", reason: "provider-specific rejection", code: 610},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set("async_image_resume_task", "accepted-job")
			c.Set("async_image_response_limit", int64(1024))
			body, err := common.Marshal(map[string]string{"status": "failed", "reason": tc.reason})
			require.NoError(t, err)
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
			_, apiErr := handleUpstreamAsyncImageResponse(c, &relaycommon.RelayInfo{}, response, &profile)
			var failure *service.AsyncImageFailure
			require.ErrorAs(t, apiErr, &failure)
			assert.Equal(t, tc.code, failure.Code)
			assert.Equal(t, tc.reason, failure.Message)
			assert.Equal(t, "upstream_job_failed", failure.InternalCode)
			assert.True(t, failure.UpstreamTerminal)
			assert.False(t, failure.ExecutionUnknown)
		})
	}
}

func TestAsyncImageFailureDiagnosticsAndTransportBarrier(t *testing.T) {
	for _, tc := range []struct {
		name, body, message string
		status, code        int
	}{
		{name: "nested_detail", body: `{"detail":[{"loc":["body","references",0,"source"],"msg":"reference image size exceeds 20MB","input":"private-prompt"}],"prompt":"private-prompt","request_id":"diagnostic-id"}`, message: "/body/references/0/source: reference image size exceeds 20MB", status: 400, code: 604},
		{name: "encoded_json_error", body: `{"error":"{\"message\":\"服务繁忙,请稍后重试\",\"input\":\"private-prompt\"}"}`, message: "服务繁忙,请稍后重试", status: 400, code: 606},
		{name: "malformed_json_error", body: `{"error":"{\"message\":\"broken\",\"input\":\"private-prompt\""}`, message: "Upstream image request failed", status: 400, code: 610},
		{name: "no_diagnostic_fields", body: `{"input":{"request_id":"private-prompt"},"prompt":"private-prompt"}`, message: "Upstream image request failed", status: 400, code: 610},
		{name: "explicit_busy_rejection", body: `{"error":{"message":"服务繁忙，请稍后重试"}}`, message: "服务繁忙，请稍后重试", status: 400, code: 606},
		{name: "authorization_not_busy_contract", body: `{"error":{"message":"服务繁忙，请稍后重试"}}`, message: "服务繁忙，请稍后重试", status: 403, code: 610},
		{name: "limit_not_busy", body: `{"error":{"message":"参考image上传失败：参考图过大（25.4MB，上限 20MB）；服务繁忙,请稍后重试"}}`, message: "25.4MB，上限 20MB", status: 400, code: 604},
		{name: "refusal_not_busy", body: `{"error":{"message":"upstream refusal: 服务繁忙,请稍后重试"}}`, message: "upstream refusal", status: 400, code: 601},
		{name: "generic_invalid_argument", body: `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`, message: "Request contains an invalid argument.", status: 400, code: 610},
		{name: "third_party_unavailable_is_not_review", body: `{"error":{"message":"third-party service unavailable"}}`, message: "third-party service unavailable", status: 503, code: 606},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Header: http.Header{"X-Request-Id": []string{"header-diagnostic-id"}}, Body: io.NopCloser(strings.NewReader(tc.body))}
			failure := service.ClassifyGeminiAsyncImageHTTPFailure(t.Context(), response)
			assert.Equal(t, tc.code, failure.Code)
			assert.Contains(t, failure.Message, tc.message)
			assert.NotContains(t, failure.Message, "private-prompt")
			assert.NotContains(t, failure.UpstreamRequestID, "private-prompt")
			assert.NotEmpty(t, failure.UpstreamRequestID)
			assert.False(t, failure.ExecutionUnknown)
			assert.False(t, failure.UpstreamTerminal, "an HTTP query error cannot terminate an accepted upstream job")
		})
	}
	assert.Empty(t, service.SanitizeAsyncImageDiagnostic(`{"input":"private-prompt"`, 1024))
	for _, tc := range []struct {
		name, internal string
		err            error
		response       bool
	}{
		{name: "deadline", internal: "upstream_transport_timeout", err: &url.Error{Op: "Post", URL: "https://signed.example/image?token=private-token", Err: context.DeadlineExceeded}},
		{name: "disconnect", internal: "upstream_transport_disconnected", err: io.ErrUnexpectedEOF},
		{name: "canceled", internal: "upstream_transport_canceled", err: context.Canceled},
		{name: "parse", internal: "upstream_response_invalid", err: errors.New(`bad response contains private-prompt`), response: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, jobID := range []string{"", "accepted-job"} {
				failure := service.AsyncImageTransportFailure(tc.err, model.AsyncImageTask{UpstreamTaskId: jobID}, tc.response, http.Header{"X-Request-Id": []string{"upstream-request-id"}})
				assert.Equal(t, tc.internal, failure.InternalCode)
				assert.Equal(t, "upstream-request-id", failure.UpstreamRequestID)
				assert.NotContains(t, failure.Message, "signed.example")
				assert.NotContains(t, failure.Message, "private-token")
				assert.NotContains(t, failure.Message, "private-prompt")
				assert.Equal(t, jobID == "", failure.ExecutionUnknown)
				if jobID == "" {
					assert.Equal(t, 608, failure.Code)
				} else {
					assert.Equal(t, 606, failure.Code)
					assert.Contains(t, failure.Message, "only the existing upstream job may be polled again")
				}
			}
		})
	}
}

func TestAsyncImageCompletionRecoveryPreservesAcceptedOutputAndPrice(t *testing.T) {
	previousDB, previousCount, previousRedis := model.DB, constant.CountToken, common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "completion-recovery.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled = db, false, false
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.RedisEnabled = previousDB, previousCount, previousRedis
		connection, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}))
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "completion-test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "completion-test")
	user := model.User{Username: "completion-recovery-user", Status: common.UserStatusDisabled}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "completion-recovery-token", ModelLimitsEnabled: true, ModelLimits: "removed-model", UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	require.NoError(t, db.Delete(&token).Error)
	var pngImage bytes.Buffer
	require.NoError(t, png.Encode(&pngImage, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	imageData := base64.StdEncoding.EncodeToString(pngImage.Bytes())
	for _, tc := range []struct {
		name, platform    string
		resolutionMapping bool
	}{
		{name: "openai", platform: "openai"},
		{name: "gemini", platform: "gemini"},
		{name: "openai_resolution_mapping", platform: "openai", resolutionMapping: true},
		{name: "gemini_resolution_mapping", platform: "gemini", resolutionMapping: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response any = dto.ImageResponse{Data: []dto.ImageData{{B64Json: imageData}}}
			channelType := constant.ChannelTypeOpenAI
			if tc.platform == "gemini" {
				channelType = constant.ChannelTypeGemini
				response = dto.GeminiChatResponse{Candidates: []dto.GeminiChatCandidate{{Content: dto.GeminiChatContent{Parts: []dto.GeminiPart{{InlineData: &dto.GeminiInlineData{MimeType: "image/png", Data: imageData}}}}}}}
			}
			body, err := common.Marshal(response)
			require.NoError(t, err)
			completion := &service.AsyncImageCompletion{
				Body: body, Usage: &dto.Usage{}, BillingBody: []byte(`{"n":1}`), UpstreamModel: "frozen-upstream", BillingModel: "frozen-billing",
				Billing: service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}},
			}
			request := service.AsyncImageRequest{
				Platform: tc.platform, Model: "accepted-image", Count: 1, Kind: "image_to_image", Completion: completion,
				Parts:       []service.AsyncImageInputPart{{Type: "image_url", URL: "https://expired.invalid/reference.png"}},
				Native:      map[string]common.RawMessage{"ignored_after_completion": common.RawMessage(`invalid JSON`)},
				Billing:     &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 999}},
				Reservation: &model.AsyncImageBill{FundingSource: "wallet"},
			}
			invalidMapping := "invalid mapping"
			unreachableURL := "http://127.0.0.1:1"
			channel := &model.Channel{Id: 14, Type: channelType, Key: "frozen-key", BaseURL: &unreachableURL, Status: common.ChannelStatusManuallyDisabled, ModelMapping: &invalidMapping}
			if tc.resolutionMapping {
				channel.SetSetting(dto.ChannelSettings{ImageResolutionModelMapping: dto.ImageResolutionModelMapping{"accepted-image": {"1K": "changed-1k-upstream", "2K": "changed-2k-upstream"}}})
				completion.BillingModel = request.Model
			}
			task := model.AsyncImageTask{TaskId: "completion-" + tc.name, UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Group: "default", UpstreamSucceededAt: time.Now().Unix()}
			attemptContext, cancel := context.WithCancel(t.Context())
			cancel()
			dispatches := 0
			output, bill, err := ExecuteAsyncImage(attemptContext, task, request, channel, service.DefaultImageRuntimeConfig(), func() error {
				dispatches++
				return nil
			})
			require.NoError(t, err)
			require.NotNil(t, output)
			require.Len(t, output.Images, 1)
			assert.Equal(t, 2, output.Images[0].Width)
			assert.Equal(t, 3, output.Images[0].Height)
			assert.Equal(t, 5000, bill.Quota, "recovery must use the accepted price, even if current pricing changed")
			var log model.Log
			require.NoError(t, common.UnmarshalJsonStr(bill.LogPayload, &log))
			assert.Equal(t, completion.BillingModel, log.ModelName)
			if tc.resolutionMapping {
				assert.Equal(t, request.Model, log.ModelName, "resolution aliases must not replace the public billing model")
			}
			assert.Zero(t, dispatches, "accepted output must never generate or poll again")
		})
	}

	service.InitHttpClient()
	for _, tc := range []struct {
		name, imageData string
		valid           bool
	}{
		{name: "oversized_valid_output", imageData: imageData, valid: true},
		{name: "oversized_invalid_output", imageData: base64.StdEncoding.EncodeToString([]byte("not an image"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := map[string]any{
				"candidates":    []dto.GeminiChatCandidate{{Content: dto.GeminiChatContent{Parts: []dto.GeminiPart{{InlineData: &dto.GeminiInlineData{MimeType: "image/png", Data: tc.imageData}}}}}},
				"usageMetadata": map[string]int{"promptTokenCount": 1, "candidatesTokenCount": 1, "totalTokenCount": 2},
				"metadata":      "",
			}
			encoded, err := common.Marshal(response)
			require.NoError(t, err)
			response["metadata"] = strings.Repeat("x", (32<<20)+1-len(encoded))
			encoded, err = common.Marshal(response)
			require.NoError(t, err)
			upstreamRequests := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamRequests <- r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(encoded)
			}))
			defer upstream.Close()
			freshUser := model.User{Username: tc.name, AffCode: tc.name, Status: common.UserStatusEnabled, Quota: 1000000}
			require.NoError(t, db.Create(&freshUser).Error)
			freshToken := model.Token{UserId: freshUser.Id, Key: tc.name, Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
			require.NoError(t, db.Create(&freshToken).Error)
			request := service.AsyncImageRequest{
				Platform: "gemini", Dialect: "async", Model: "gemini-image-test", Resolution: "2K", Prompt: "draw", Count: 1,
				Parts:   []service.AsyncImageInputPart{{Type: "text", Text: "draw"}},
				Billing: &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}},
			}
			channel := &model.Channel{Id: 14, Type: constant.ChannelTypeGemini, Key: "synthetic-upstream-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled}
			channel.SetSetting(dto.ChannelSettings{ImageResolutionModelMapping: dto.ImageResolutionModelMapping{"gemini-image-test": {"1K": "gemini-image-test-1k", "2K": "gemini-image-test-2k"}}})
			channel.ModelMapping = common.GetPointer(`{"gemini-image-test":"gemini-image-test-2k","gemini-image-test-2k":"gemini-image-test-2k"}`)
			task := model.AsyncImageTask{TaskId: tc.name, UserId: freshUser.Id, TokenId: freshToken.Id, ChannelId: channel.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "oversized-worker", LeaseExpiresAt: time.Now().Unix() + 120}
			requestBytes, err := common.Marshal(request)
			require.NoError(t, err)
			task.RequestCipher, err = service.EncryptImagePayload(requestBytes, "task:"+task.TaskId)
			require.NoError(t, err)
			channelBytes, err := common.Marshal(channel)
			require.NoError(t, err)
			task.SelectedChannelCipher, err = service.EncryptImagePayload(channelBytes, "image-channel:"+task.TaskId)
			require.NoError(t, err)
			require.NoError(t, db.Create(&task).Error)
			output, bill, err := ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error {
				return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
			})
			if tc.valid {
				require.NoError(t, err)
				require.NotNil(t, output)
				require.Len(t, output.Images, 1)
				assert.Equal(t, 2, output.Images[0].Width)
				assert.Equal(t, 5000, bill.Quota, "large metadata must not prevent a valid current invocation from finishing")
				var log model.Log
				require.NoError(t, common.UnmarshalJsonStr(bill.LogPayload, &log))
				assert.Equal(t, request.Model, log.ModelName, "the resolution-specific upstream name must not change the public billing model")
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
				assert.Equal(t, "gemini-image-test-2k", other["upstream_model_name"])
			} else {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.True(t, failure.ExecutionUnknown, "unsaved output cannot authorize a second generation")
				assert.Equal(t, 608, failure.Code)
				assert.Nil(t, output)
			}
			assert.Contains(t, <-upstreamRequests, "/models/gemini-image-test-2k:")
			assert.Empty(t, upstreamRequests)
			var stored model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&stored).Error)
			assert.Zero(t, stored.UpstreamSucceededAt)
			assert.Equal(t, task.RequestCipher, stored.RequestCipher, "oversized checkpoints must not overwrite the original request")
		})
	}
}

func TestOpenAIAsyncImageOutboundParameters(t *testing.T) {
	previousDB, previousCount, previousRedis := model.DB, constant.CountToken, common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "openai-outbound.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled = db, false, false
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.RedisEnabled = previousDB, previousCount, previousRedis
		connection, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}, &model.ImageInputAlias{}))
	user := model.User{Username: "openai-outbound-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "openai-outbound-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	service.InitHttpClient()

	const referenceURL = "https://example.com/reference%20image.png?sig=synthetic%2Fsignature%2Bvalue&part=1"
	const maskURL = "https://example.com/mask.png?sig=synthetic%3Dmask"
	var pngImage, jpegImage bytes.Buffer
	require.NoError(t, png.Encode(&pngImage, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	require.NoError(t, jpeg.Encode(&jpegImage, image.NewRGBA(image.Rect(0, 0, 2, 3)), nil))
	for _, tc := range []struct {
		name, path, referenceField string
		providerStatus, requestID  string
		secrets                    []string
		channelReferenceLimit      int64
		localReferenceRejected     bool
		unreadableResponse         bool
		maxReferences              int
		status, failureCode        int
		responseMessage            string
		providerCode               string
		statusMapping              string
		fileFormat                 string
		passThrough                bool
		xai                        bool
		resolutionMapping          bool
		resolution                 string
		parameterOverride          string
		modelConflict              bool
		templateBody               map[string]any
		expectedTemplate           string
	}{
		{name: "resolution_mapping_json", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true},
		{name: "resolution_mapping_json_passthrough", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, passThrough: true},
		{name: "resolution_mapping_2k_json_passthrough", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, resolution: "2K", passThrough: true},
		{name: "resolution_mapping_multipart_passthrough", path: "/v1/images/edits_async", fileFormat: "jpeg", resolutionMapping: true, passThrough: true},
		{name: "resolution_mapping_rejects_model_override", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, parameterOverride: `{"model":"other-model"}`, modelConflict: true},
		{name: "resolution_mapping_rejects_model_deletion", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, parameterOverride: `{"operations":[{"path":"model","mode":"delete"}]}`, modelConflict: true},
		{name: "resolution_mapping_rejects_template_alias", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, templateBody: map[string]any{"model": "{request.model}", "n": "{request.n}"}, modelConflict: true},
		{name: "resolution_mapping_template_model", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, templateBody: map[string]any{"model": "{upstream_model}", "n": "{request.n}"}, expectedTemplate: `{"model":"gpt-image-2","n":1}`},
		{name: "resolution_mapping_template_custom_field", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, templateBody: map[string]any{"provider_model": "{upstream_model}", "n": "{request.n}"}, expectedTemplate: `{"provider_model":"gpt-image-2","n":1}`},
		{name: "xai_normalized_geometry", path: "/v1/images/generations_async", referenceField: "images", xai: true},
		{name: "xai_provider_diagnostic", path: "/v1/images/generations_async", referenceField: "images", xai: true, status: http.StatusBadRequest, failureCode: 610, responseMessage: "unknown provider for model grok-imagine-image-2.0"},
		{name: "edits_oa", path: "/v1/images/edits_oa", referenceField: "image_urls"},
		{name: "edits_async", path: "/v1/images/edits_async", referenceField: "image"},
		{name: "generations_oa_with_reference", path: "/v1/images/generations_oa", referenceField: "images"},
		{name: "generations_async_with_reference", path: "/v1/images/generations_async", referenceField: "images"},
		{name: "edits_oa_body_passthrough", path: "/v1/images/edits_oa", referenceField: "images", passThrough: true},
		{name: "edits_async_body_passthrough", path: "/v1/images/edits_async", referenceField: "image", passThrough: true},
		{name: "edits_async_multipart_png", path: "/v1/images/edits_async", fileFormat: "png"},
		{name: "edits_async_multipart_jpeg", path: "/v1/images/edits_async", fileFormat: "jpeg"},
		{name: "edits_async_multipart_jpeg_body_passthrough", path: "/v1/images/edits_async", fileFormat: "jpeg", passThrough: true},
		{name: "edits_oa_multipart_png", path: "/v1/images/edits_oa", fileFormat: "png"},
		{name: "multipart_mask_does_not_count_as_reference", path: "/v1/images/edits_async", fileFormat: "png", maxReferences: 1},
		{name: "channel_reference_limit_before_dispatch", path: "/v1/images/edits_async", fileFormat: "png", channelReferenceLimit: 8, localReferenceRejected: true},
		{name: "channel_reference_limit_keeps_remote_provider_reason", path: "/v1/images/edits_async", referenceField: "images", channelReferenceLimit: 8, status: http.StatusBadRequest, failureCode: 604, responseMessage: "参考image上传失败：参考图过大（25.4MB，上限 20MB）"},
		{name: "accepted_response_parse_failure_keeps_request_id", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusOK, unreadableResponse: true},
		{name: "reference_fetch_diagnostics", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusBadRequest, failureCode: 602, providerCode: "image_fetch_failed", responseMessage: "image_url fetch failed for https://example.com/private.png?token=secret-url; authorization=Bearer secret-bearer api_key=secret-key"},
		{name: "upstream_service_diagnostics", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusBadGateway, failureCode: 606, providerCode: "overloaded", responseMessage: "upstream image worker unavailable"},
		{name: "mapped_status_keeps_original_diagnostic", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusBadGateway, failureCode: 605, providerCode: "overloaded", responseMessage: "upstream image worker unavailable", statusMapping: `{"502":"429"}`},
		{name: "mapped_status_not_overridden_by_generic_error_type", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusBadRequest, failureCode: 605, responseMessage: "please retry later", statusMapping: `{"400":"429"}`},
		{name: "server_status_not_overridden_by_generic_error_type", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusBadGateway, failureCode: 606, responseMessage: "please retry later"},
		{
			name: "credential_diagnostics_redact_all_provider_fields", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusUnauthorized, failureCode: 610,
			responseMessage: "Incorrect API key provided: sk-proj-SYNTHETIC123456789NOTAREALKEY",
			providerCode:    "https://example.com/private-code?signature=SYNTHETIC-CODE-SECRET",
			providerStatus:  "sk-proj-SYNTHETICSTATUS123456789NOTAREALKEY",
			requestID:       "https://example.com/private-request?signature=SYNTHETIC-REQUEST-SECRET sk-proj-SYNTHETICREQUEST123456789NOTAREALKEY",
			secrets: []string{
				"sk-proj-SYNTHETIC123456789NOTAREALKEY", "SYNTHETIC-CODE-SECRET", "sk-proj-SYNTHETICSTATUS123456789NOTAREALKEY",
				"SYNTHETIC-REQUEST-SECRET", "sk-proj-SYNTHETICREQUEST123456789NOTAREALKEY", "https://example.com/private-code", "https://example.com/private-request",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type recordedRequest struct {
				method, path, contentType, authorization, marker string
				body                                             []byte
			}
			received := make(chan recordedRequest, 1)
			responseStatus := tc.status
			if responseStatus == 0 {
				responseStatus = http.StatusBadRequest
			}
			responseMessage := tc.responseMessage
			if responseMessage == "" {
				responseMessage = "synthetic request capture complete"
			}
			providerError := map[string]string{"message": responseMessage, "type": "invalid_request_error"}
			if tc.providerCode != "" {
				providerError["code"] = tc.providerCode
			}
			if tc.providerStatus != "" {
				providerError["status"] = tc.providerStatus
			}
			requestID := tc.requestID
			if requestID == "" {
				requestID = "synthetic-upstream-request"
			}
			responseBody, err := common.Marshal(map[string]any{"error": providerError})
			require.NoError(t, err)
			if tc.unreadableResponse {
				responseBody = []byte(`{"data":[`)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, readErr := io.ReadAll(r.Body)
				if !assert.NoError(t, readErr) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				received <- recordedRequest{r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type"), r.Header.Get("Authorization"), r.Header.Get("X-Image-Probe"), body}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Request-Id", requestID)
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(responseStatus)
				_, _ = w.Write(responseBody)
			}))
			defer upstream.Close()
			mapping := `{"image-alias":"gpt-image-2"}`
			parameters := `{"quality":"low","vendor_override":true}`
			if tc.parameterOverride != "" {
				parameters = tc.parameterOverride
			}
			headers := `{"X-Image-Probe":"synthetic"}`
			channel := &model.Channel{Id: 1, Key: "synthetic-upstream-key", BaseURL: &upstream.URL, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "image-alias", Group: "default", ModelMapping: &mapping, ParamOverride: &parameters, HeaderOverride: &headers}
			if tc.xai {
				channel.Type = constant.ChannelTypeXai
				mapping = `{"image-alias":"grok-imagine-image-quality"}`
				channel.ParamOverride = nil
			}
			channel.StatusCodeMapping = &tc.statusMapping
			channelSettings := dto.ChannelSettings{PassThroughBodyEnabled: tc.passThrough}
			if tc.resolutionMapping {
				channelSettings.ImageResolutionModelMapping = dto.ImageResolutionModelMapping{"image-alias": {"1K": "gpt-image-2", "2K": "gpt-image-2-2k"}}
				mapping = `{"image-alias":"gpt-image-2","gpt-image-2":"gpt-image-2"}`
				if tc.resolution == "2K" {
					mapping = `{"image-alias":"gpt-image-2-2k","gpt-image-2-2k":"gpt-image-2-2k"}`
				}
			}
			if tc.channelReferenceLimit > 0 {
				channelSettings.ImageMaxReferenceBytes = &tc.channelReferenceLimit
			}
			channel.SetSetting(channelSettings)
			if tc.templateBody != nil {
				var config dto.UpstreamAsyncConfig
				require.NoError(t, common.UnmarshalJsonStr(`{"profiles":[{"id":"resolution-template","media_type":"image","models":["gpt-image-2"],"operations":["edit"],"submit":{"task_id_path":"id","request":{"method":"POST","path":"/v1/images/edits","headers":{"Authorization":"Bearer {api_key}"}}},"poll":{"request":{"method":"GET","path":"/tasks/{task_id}"},"response":{"status_path":"status","status_values":{"succeeded":["done"],"failed":["failed"]},"result_path":"urls"}}}]}`, &config))
				config.Profiles[0].Submit.Request.Body = tc.templateBody
				require.NoError(t, config.Validate())
				channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamAsync: &config})
			}
			body := map[string]any{
				"model": "image-alias", "prompt": "draw a river", "n": 1,
				"size": "1024x1024", "quality": "high", "response_format": "b64_json",
				"mask":      map[string]string{"image_url": maskURL},
				"watermark": false, "seed": 0, "custom_toggle": false,
				"vendor_options": map[string]any{"strength": 0, "enabled": false},
			}
			if tc.resolution == "2K" {
				body["size"] = "2048x2048"
			}
			if tc.xai {
				delete(body, "mask")
				body["resolution"], body["aspect_ratio"], body["size"] = "2K", "16:9", "2048x1152"
			}
			if tc.referenceField == "images" {
				body[tc.referenceField] = []map[string]string{{"image_url": referenceURL}}
			} else if tc.referenceField != "" {
				body[tc.referenceField] = []string{referenceURL}
			}
			raw, err := common.Marshal(body)
			require.NoError(t, err)
			contentType := "application/json"
			imageBytes, imageFilename := pngImage.Bytes(), "input.png"
			if tc.fileFormat != "" {
				var formBody bytes.Buffer
				form := multipart.NewWriter(&formBody)
				for key, value := range body {
					if key == "mask" {
						continue
					}
					text, ok := value.(string)
					if !ok {
						encoded, err := common.Marshal(value)
						require.NoError(t, err)
						text = string(encoded)
					}
					require.NoError(t, form.WriteField(key, text))
				}
				if tc.fileFormat == "jpeg" {
					imageBytes, imageFilename = jpegImage.Bytes(), "input.jpg"
				}
				part, err := form.CreateFormFile("image", imageFilename)
				require.NoError(t, err)
				_, err = part.Write(imageBytes)
				require.NoError(t, err)
				part, err = form.CreateFormFile("mask", "mask.png")
				require.NoError(t, err)
				_, err = part.Write(pngImage.Bytes())
				require.NoError(t, err)
				require.NoError(t, form.Close())
				raw, contentType = formBody.Bytes(), form.FormDataContentType()
			}
			cfg := service.DefaultImageRuntimeConfig()
			cfg.OpenAIReferenceMode = "passthrough"
			if tc.maxReferences > 0 {
				cfg.MaxReferences = tc.maxReferences
			}
			request, err := service.ParseAsyncImageRequest(raw, contentType, tc.path, cfg)
			require.NoError(t, err)
			request.Billing = &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
			persisted, err := common.Marshal(request)
			require.NoError(t, err)
			request = service.AsyncImageRequest{}
			require.NoError(t, common.Unmarshal(persisted, &request))
			task := model.AsyncImageTask{TaskId: "openai-outbound-" + tc.name, UserId: user.Id, TokenId: token.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120}
			require.NoError(t, db.Create(&task).Error)
			dispatches := 0
			_, _, err = ExecuteAsyncImage(t.Context(), task, request, channel, cfg, func() error {
				dispatches++
				return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
			})
			require.Error(t, err)
			if tc.modelConflict {
				assert.ErrorContains(t, err, "outbound image model conflicts")
				assert.Zero(t, dispatches, "model conflicts must fail before quota reservation or dispatch")
				assert.Empty(t, received)
				var reservations int64
				require.NoError(t, db.Model(&model.AsyncImageBill{}).Where("task_id = ?", task.TaskId).Count(&reservations).Error)
				assert.Zero(t, reservations)
				return
			}
			if tc.localReferenceRejected {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, 604, failure.Code)
				assert.Contains(t, failure.Message, "Reference image 1:")
				assert.Contains(t, failure.Message, "reference image limit: 8 bytes")
				assert.Zero(t, dispatches, "a known oversized reference must be rejected before quota reservation or upstream dispatch")
				assert.Empty(t, received)
				return
			}
			if tc.unreadableResponse {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, 608, failure.Code)
				assert.Equal(t, "upstream_response_invalid", failure.InternalCode)
				assert.Equal(t, http.StatusOK, failure.HTTPStatus, "a response parser error must retain the actual upstream status")
				assert.True(t, failure.ExecutionUnknown)
				assert.Equal(t, requestID, failure.UpstreamRequestID)
				assert.Equal(t, 1, dispatches)
				assert.Len(t, received, 1)
				return
			}
			if tc.failureCode != 0 {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, tc.failureCode, failure.Code)
				assert.Equal(t, tc.status, failure.HTTPStatus, "attempt diagnostics must keep the real upstream status")
				assert.Equal(t, 7*time.Second, failure.RetryAfter)
				if len(tc.secrets) > 0 {
					assert.Contains(t, failure.Message, "Incorrect API key provided:")
					for field, value := range map[string]string{"message": failure.Message, "provider_code": failure.ProviderCode, "provider_status": failure.ProviderStatus, "request_id": failure.UpstreamRequestID} {
						assert.NotEmpty(t, value, "field %s should retain a redacted diagnostic", field)
						for _, secret := range tc.secrets {
							assert.NotContains(t, value, secret, "field %s must not persist provider-echoed credentials or signed URLs", field)
						}
					}
				} else {
					expectedCode := tc.providerCode
					if expectedCode == "" {
						expectedCode = "invalid_request_error"
					}
					assert.Equal(t, expectedCode, failure.ProviderCode)
					assert.Equal(t, requestID, failure.UpstreamRequestID)
				}
				if tc.failureCode == 602 {
					assert.Contains(t, failure.Message, "image_url fetch failed")
					for _, secret := range []string{"secret-url", "secret-bearer", "secret-key", "https://example.com/private.png"} {
						assert.NotContains(t, failure.Message, secret)
					}
				} else if len(tc.secrets) == 0 {
					assert.Equal(t, tc.responseMessage, failure.Message)
				}
			}
			require.Equal(t, 1, dispatches)
			require.Len(t, received, 1)
			record := <-received
			assert.Equal(t, http.MethodPost, record.method)
			assert.Equal(t, "/v1/images/edits", record.path)
			assert.Equal(t, "Bearer synthetic-upstream-key", record.authorization)
			if tc.expectedTemplate != "" {
				assert.JSONEq(t, tc.expectedTemplate, string(record.body))
				return
			}
			assert.Equal(t, "synthetic", record.marker)
			if tc.xai {
				assert.JSONEq(t, `{"model":"grok-imagine-image-quality","prompt":"draw a river","n":1,"resolution":"2k","aspect_ratio":"16:9","response_format":"b64_json","image":{"url":"`+referenceURL+`","type":"image_url"}}`, string(record.body))
				return
			}
			if tc.fileFormat != "" {
				require.Contains(t, record.contentType, "multipart/form-data", "file uploads must retain their upstream transport format")
				replayed := httptest.NewRequest(http.MethodPost, record.path, bytes.NewReader(record.body))
				replayed.Header.Set("Content-Type", record.contentType)
				require.NoError(t, replayed.ParseMultipartForm(1<<20))
				defer replayed.MultipartForm.RemoveAll()
				imageField, expectedModel := "image", "gpt-image-2"
				if tc.passThrough {
					imageField, expectedModel = "image[]", "image-alias"
				}
				if tc.resolutionMapping {
					expectedModel = "gpt-image-2"
				}
				assert.Equal(t, expectedModel, replayed.PostForm.Get("model"))
				assert.Equal(t, "draw a river", replayed.PostForm.Get("prompt"))
				for field, value := range map[string]string{"n": "1", "size": "1024x1024", "response_format": "b64_json", "watermark": "false", "seed": "0", "custom_toggle": "false"} {
					assert.Equal(t, value, replayed.PostForm.Get(field), "multipart field %s must survive persistence and relay conversion", field)
				}
				assert.JSONEq(t, `{"strength":0,"enabled":false}`, replayed.PostForm.Get("vendor_options"))
				for _, file := range []struct {
					field, contentType string
					data               []byte
				}{
					{field: imageField, contentType: "image/" + tc.fileFormat, data: imageBytes},
					{field: "mask", contentType: "image/png", data: pngImage.Bytes()},
				} {
					require.Len(t, replayed.MultipartForm.File[file.field], 1)
					header := replayed.MultipartForm.File[file.field][0]
					assert.Equal(t, file.contentType, header.Header.Get("Content-Type"), "field %s must declare its real image encoding", file.field)
					reader, err := header.Open()
					require.NoError(t, err)
					actual, err := io.ReadAll(reader)
					require.NoError(t, err)
					require.NoError(t, reader.Close())
					assert.Equal(t, file.data, actual)
				}
				return
			}
			assert.Equal(t, "application/json", record.contentType)
			var outbound map[string]common.RawMessage
			require.NoError(t, common.Unmarshal(record.body, &outbound))
			expectedModel, expectedQuality := `"gpt-image-2"`, `"low"`
			if tc.passThrough {
				expectedModel, expectedQuality = `"image-alias"`, `"high"`
				assert.NotContains(t, outbound, "vendor_override", "body passthrough bypasses channel parameter overrides")
			} else {
				assert.JSONEq(t, "true", string(outbound["vendor_override"]))
			}
			if tc.resolutionMapping {
				expectedModel = `"gpt-image-2"`
				if tc.resolution == "2K" {
					expectedModel = `"gpt-image-2-2k"`
				}
			}
			assert.JSONEq(t, expectedModel, string(outbound["model"]))
			assert.JSONEq(t, expectedQuality, string(outbound["quality"]))
			for _, key := range []string{"prompt", "n", "size", "response_format", "watermark", "seed", "custom_toggle", "vendor_options", "mask"} {
				encoded, err := common.Marshal(body[key])
				require.NoError(t, err)
				assert.JSONEq(t, string(encoded), string(outbound[key]), "field %s must survive persistence and relay conversion", key)
			}
			var images []struct {
				URL string `json:"image_url"`
			}
			require.NoError(t, common.Unmarshal(outbound["images"], &images))
			require.Len(t, images, 1)
			assert.Equal(t, referenceURL, images[0].URL, "external signed URL bytes must be preserved")
			assert.NotContains(t, outbound, "image")
			assert.NotContains(t, outbound, "image_urls")
		})
	}
}

func TestTianyueAsyncImageOutbound(t *testing.T) {
	previousDB, previousCount, previousRedis, previousAddress := model.DB, constant.CountToken, common.RedisEnabled, system_setting.ServerAddress
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tianyue-outbound.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled, system_setting.ServerAddress = db, false, false, "https://gateway.example"
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.RedisEnabled, system_setting.ServerAddress = previousDB, previousCount, previousRedis, previousAddress
		connection, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	})
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "tianyue:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "tianyue")
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}, &model.ImageInputAlias{}))
	user := model.User{Username: "tianyue-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "tianyue-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	service.InitHttpClient()
	var bitmap bytes.Buffer
	require.NoError(t, png.Encode(&bitmap, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	for _, tc := range []struct {
		name                           string
		reference, multipart, yb, mask bool
		disabled, badOverride          bool
		badTemplate, customProfile     bool
		providerErrorCode              string
		disconnect                     bool
	}{
		{name: "synchronous_text"},
		{name: "provider_request_failure", providerErrorCode: "do_request_failed"},
		{name: "provider_response_read_failure", providerErrorCode: "read_response_body_failed"},
		{name: "provider_bad_response", providerErrorCode: "bad_response"},
		{name: "provider_bad_response_body", providerErrorCode: "bad_response_body"},
		{name: "provider_empty_response", providerErrorCode: "empty_response"},
		{name: "transport_eof", disconnect: true},
		{name: "uploaded_reference", reference: true},
		{name: "multipart_reference", reference: true, multipart: true},
		{name: "asynchronous_receipt_and_poll", reference: true, yb: true},
		{name: "mask_is_rejected", reference: true, mask: true},
		{name: "unsafe_override_is_rejected", badOverride: true},
		{name: "unsafe_async_template_is_rejected", yb: true, badTemplate: true},
		{name: "custom_profile_with_builtin_id_is_preserved", yb: true, customProfile: true},
		{name: "ordinary_newapi_edits_are_preserved", reference: true, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type receivedRequest struct {
				path string
				body map[string]common.RawMessage
			}
			received := make(chan receivedRequest, 3)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]common.RawMessage
				if r.Method == http.MethodPost {
					assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
					assert.NoError(t, common.DecodeJson(r.Body, &body))
				}
				assert.Equal(t, "Bearer tianyue-upstream-key", r.Header.Get("Authorization"))
				received <- receivedRequest{r.URL.Path, body}
				if tc.disconnect {
					connection, _, err := w.(http.Hijacker).Hijack()
					if assert.NoError(t, err) {
						assert.NoError(t, connection.Close())
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.providerErrorCode != "" {
					w.Header().Set(common.RequestIdKey, "tianyue-provider-request-id")
					w.WriteHeader(http.StatusInternalServerError)
					encoded, err := common.Marshal(map[string]any{"error": map[string]string{"message": "TianYue could not confirm image generation; Authorization: Bearer private-provider-key", "type": "new_api_error", "code": tc.providerErrorCode}})
					if assert.NoError(t, err) {
						_, _ = w.Write(encoded)
					}
					return
				}
				if tc.yb {
					if tc.customProfile {
						if r.Method == http.MethodPost {
							w.WriteHeader(http.StatusAccepted)
							_, _ = w.Write([]byte(`{"job":{"identifier":"custom-job","state":"queued"}}`))
						} else {
							_, _ = w.Write([]byte(`{"job":{"identifier":"custom-job","state":"running","progress":30}}`))
						}
						return
					}
					if r.Method == http.MethodPost {
						w.WriteHeader(http.StatusAccepted)
						_, _ = w.Write([]byte(`{"code":"success","data":{"task_id":"tianyue-job","status":"IN_PROGRESS"}}`))
					} else {
						_, _ = w.Write([]byte(`{"code":"success","data":{"task_id":"tianyue-job","status":"IN_PROGRESS","progress":"30%"}}`))
					}
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"synthetic capture completed"}}`))
			}))
			t.Cleanup(upstream.Close)
			mapped := "seedream-5-pro-1K"
			if tc.yb {
				mapped = "gpt-image-2-yb"
			}
			mapping, err := common.Marshal(map[string]string{"image-alias": mapped})
			require.NoError(t, err)
			mappingText := string(mapping)
			channel := &model.Channel{Id: 30, Type: constant.ChannelTypeNewAPI, Key: "tianyue-upstream-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, Models: "image-alias", Group: "default", ModelMapping: &mappingText}
			channel.SetSetting(dto.ChannelSettings{TianyueMediaCompatibility: !tc.disabled})
			if tc.badOverride {
				override := `{"duration":2}`
				channel.ParamOverride = &override
			}
			if tc.badTemplate || tc.customProfile {
				var custom dto.UpstreamAsyncConfig
				require.NoError(t, common.Unmarshal([]byte(`{"profiles":[{"id":"tianyue-image","media_type":"image","models":["gpt-image-2-yb"],"operations":["generate"],"submit":{"task_id_path":"job.identifier","request":{"method":"POST","path":"/custom/images/jobs","headers":{"Authorization":"Bearer {api_key}"}}},"poll":{"request":{"method":"GET","path":"/custom/images/jobs/{task_id}","headers":{"Authorization":"Bearer {api_key}"}},"response":{"status_path":"job.state","progress_path":"job.progress","status_values":{"queued":["queued"],"in_progress":["running"],"succeeded":["done"],"failed":["failed"]},"result_path":"job.output"}}}]}`), &custom))
				if tc.badTemplate {
					custom.Profiles[0].Submit.Request.Body = map[string]any{"model": "{upstream_model}", "prompt": "{request.prompt}", "duration": float64(2)}
				}
				require.NoError(t, custom.Validate())
				channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamAsync: &custom})
			}
			request := service.AsyncImageRequest{Platform: "openai", Provider: "newapi", Dialect: "async", Model: "image-alias", Prompt: "test picture", Count: 1, Kind: "text_to_image", Size: "1024x1024", Resolution: "1K", SourcePath: "/v1/images/generations_async", Native: map[string]common.RawMessage{"model": common.RawMessage(`"image-alias"`), "prompt": common.RawMessage(`"test picture"`), "n": common.RawMessage(`1`), "size": common.RawMessage(`"1024x1024"`)}}
			if tc.reference {
				request.Kind, request.SourcePath = "image_to_image", "/v1/images/edits_async"
				request.Parts = []service.AsyncImageInputPart{{Type: "image_url", URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(bitmap.Bytes())}}
			}
			if tc.multipart {
				request.InputFormat = "multipart"
			}
			if tc.mask {
				request.Parts = append(request.Parts, service.AsyncImageInputPart{Type: "mask", URL: request.Parts[0].URL})
			}
			request.Billing = &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
			task := model.AsyncImageTask{TaskId: "tianyue-" + tc.name, UserId: user.Id, TokenId: token.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120, StartedAt: time.Now().Unix()}
			payload, err := common.Marshal(request)
			require.NoError(t, err)
			task.RequestCipher, err = service.EncryptImagePayload(payload, "task:"+task.TaskId)
			require.NoError(t, err)
			require.NoError(t, db.Create(&task).Error)
			dispatches := 0
			beforeInvoke := func() error {
				dispatches++
				return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
			}
			_, _, err = ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), beforeInvoke)
			require.Error(t, err)
			if tc.mask || tc.badOverride || tc.badTemplate {
				assert.Zero(t, dispatches)
				assert.Empty(t, received)
				if tc.badTemplate {
					assert.ErrorContains(t, err, "duration=1")
				}
				return
			}
			require.Len(t, received, 1)
			sent := <-received
			assert.Equal(t, 1, dispatches)
			if tc.disabled {
				assert.Equal(t, "/v1/images/edits", sent.path)
				var references []map[string]string
				require.NoError(t, common.Unmarshal(sent.body["images"], &references))
				require.Len(t, references, 1)
				assert.True(t, strings.HasPrefix(references[0]["image_url"], "data:"))
				return
			}
			expectedPath := "/v1/images/generations"
			if tc.yb {
				expectedPath = "/v1/video/generations"
			}
			if tc.customProfile {
				expectedPath = "/custom/images/jobs"
			}
			assert.Equal(t, expectedPath, sent.path)
			assert.JSONEq(t, `"`+mapped+`"`, string(sent.body["model"]))
			assert.JSONEq(t, `1`, string(sent.body["duration"]))
			assert.JSONEq(t, `"1:1"`, string(sent.body["aspect_ratio"]))
			assert.JSONEq(t, `"1K"`, string(sent.body["output_resolution"]))
			assert.NotContains(t, sent.body, "size")
			if tc.providerErrorCode != "" || tc.disconnect {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, 608, failure.Code)
				assert.True(t, failure.ExecutionUnknown, "an unconfirmed dispatched generation must retain its reservation for reconciliation")
				assert.False(t, failure.UpstreamTerminal)
				if tc.disconnect {
					assert.Equal(t, "upstream_transport_disconnected", failure.InternalCode)
					assert.Contains(t, failure.Message, "(EOF)")
					assert.Zero(t, failure.HTTPStatus)
					assert.Empty(t, failure.UpstreamRequestID)
					assert.Empty(t, failure.ProviderCode)
				} else {
					assert.Equal(t, "execution_unknown", failure.InternalCode)
					assert.Contains(t, failure.Message, "TianYue could not confirm image generation")
					assert.NotContains(t, failure.Message, "private-provider-key")
					assert.Equal(t, http.StatusInternalServerError, failure.HTTPStatus)
					assert.Equal(t, "tianyue-provider-request-id", failure.UpstreamRequestID)
					assert.Equal(t, tc.providerErrorCode, failure.ProviderCode)
				}
			}
			if tc.reference {
				var references []string
				require.NoError(t, common.Unmarshal(sent.body["images"], &references))
				require.Len(t, references, 1)
				assert.True(t, strings.HasPrefix(references[0], "https://gateway.example/api/image-task-references/"))
				assert.Contains(t, references[0], "signature=")
				assert.NotContains(t, references[0], "data:")
			}
			if tc.yb {
				var pending *service.AsyncImagePending
				require.ErrorAs(t, err, &pending)
				require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
				expectedTaskID, expectedPollPath := "tianyue-job", "/v1/video/generations/tianyue-job"
				if tc.customProfile {
					expectedTaskID, expectedPollPath = "custom-job", "/custom/images/jobs/custom-job"
				}
				assert.Equal(t, expectedTaskID, task.UpstreamTaskId)
				_, _, err = ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), beforeInvoke)
				require.ErrorAs(t, err, &pending)
				require.Len(t, received, 1)
				assert.Equal(t, expectedPollPath, (<-received).path)
				assert.Equal(t, 1, dispatches, "polling a known task must never submit again")
			}
		})
	}
}

func TestGeminiAsyncImageCustomSubmitAndResume(t *testing.T) {
	previousDB, previousCount, previousRedis := model.DB, constant.CountToken, common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "gemini-submit.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled = db, false, false
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.RedisEnabled = previousDB, previousCount, previousRedis
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}, &model.ImageInputAlias{}))
	user := model.User{Username: "gemini-submit-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{Id: 17, UserId: user.Id, Key: "gemini-submit-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	service.InitHttpClient()
	var referenceBytes bytes.Buffer
	require.NoError(t, png.Encode(&referenceBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	referenceBase64 := base64.StdEncoding.EncodeToString(referenceBytes.Bytes())
	canonicalReference := "data:image/png;base64," + referenceBase64
	sourceReference := "data:image/png;base64," + referenceBase64[:32] + "\n" + referenceBase64[32:]
	for _, tc := range []struct {
		name              string
		body              map[string]any
		reference         bool
		invalid           bool
		resolutionMapping bool
		parameterOverride string
		expectedBody      string
	}{
		{name: "template", body: map[string]any{"prompt": "{request.prompt}", "n": "{request.n}"}},
		{name: "native_body"},
		{name: "converted_reference_template", body: map[string]any{"image_urls": "{request.image_urls}", "n": "{request.n}"}, reference: true},
		{name: "conflicting_count", body: map[string]any{"n": float64(2)}, invalid: true},
		{name: "resolution_mapping_native_body", resolutionMapping: true},
		{name: "resolution_mapping_custom_field", resolutionMapping: true, body: map[string]any{"provider_model": "{upstream_model}", "n": "{request.n}"}, expectedBody: `{"provider_model":"gemini-image-test","n":1}`},
		{name: "resolution_mapping_conflicting_template", resolutionMapping: true, body: map[string]any{"model": "{request.model}"}, invalid: true},
		{name: "resolution_mapping_conflicting_override", resolutionMapping: true, parameterOverride: `{"model":"other-model"}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type recordedRequest struct{ method, path, auth, query, body string }
			received := make(chan recordedRequest, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				received <- recordedRequest{r.Method, r.URL.Path, r.Header.Get("X-Custom-Key"), r.URL.Query().Get("model"), string(body)}
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"status":"running"}`))
					return
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"id":"vendor-job"}`))
			}))
			defer upstream.Close()
			var config dto.UpstreamAsyncConfig
			require.NoError(t, common.UnmarshalJsonStr(`{"profiles":[{"id":"custom","media_type":"image","models":["gemini-image-test"],"operations":["generate"],"submit":{"task_id_path":"id","request":{"method":"POST","path":"/custom/jobs","headers":{"X-Custom-Key":"{api_key}"},"query":{"model":"{upstream_model}"}}},"poll":{"request":{"method":"GET","path":"/tasks/{task_id}"},"response":{"status_path":"status","status_values":{"in_progress":["running"],"succeeded":["done"],"failed":["failed"]},"result_path":"urls"}}}]}`, &config))
			config.Profiles[0].Submit.Request.Body = tc.body
			require.NoError(t, config.Validate())
			channel := &model.Channel{Id: 1, Key: "upstream-key", BaseURL: &upstream.URL, Type: constant.ChannelTypeGemini, Status: common.ChannelStatusEnabled, Models: "gemini-image-test", Group: "default"}
			channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamAsync: &config})
			request := service.AsyncImageRequest{Platform: "gemini", Model: "gemini-image-test", Prompt: "draw", Count: 1, Parts: []service.AsyncImageInputPart{{Type: "text", Text: "draw"}}, Billing: &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}}
			if tc.resolutionMapping {
				request.Model, request.Resolution = "gemini-alias", "1K"
				channel.ModelMapping = common.GetPointer(`{"gemini-alias":"gemini-image-test","gemini-image-test":"gemini-image-test"}`)
				channel.SetSetting(dto.ChannelSettings{ImageResolutionModelMapping: dto.ImageResolutionModelMapping{"gemini-alias": {"1K": "gemini-image-test"}}})
			}
			if tc.parameterOverride != "" {
				channel.ParamOverride = &tc.parameterOverride
			}
			if tc.reference {
				channel.SetSetting(dto.ChannelSettings{ImageReferenceTransportMode: "local"})
				request.Parts = append(request.Parts, service.AsyncImageInputPart{Type: "image_url", URL: sourceReference})
			}
			task := model.AsyncImageTask{TaskId: "gemini-submit-" + tc.name, UserId: user.Id, TokenId: token.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120}
			require.NoError(t, db.Create(&task).Error)
			dispatches := 0
			_, _, err := ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error {
				dispatches++
				return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
			})
			if tc.invalid {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, http.StatusBadRequest, failure.HTTPStatus)
				assert.Zero(t, dispatches)
				assert.Empty(t, received)
				return
			}
			var pending *service.AsyncImagePending
			require.ErrorAs(t, err, &pending)
			assert.Equal(t, 1, dispatches)
			record := <-received
			assert.Equal(t, http.MethodPost, record.method)
			assert.Equal(t, "/custom/jobs", record.path)
			assert.Equal(t, "upstream-key", record.auth)
			assert.Equal(t, "gemini-image-test", record.query)
			if tc.expectedBody != "" {
				assert.JSONEq(t, tc.expectedBody, record.body)
			} else if tc.reference {
				expected, err := common.Marshal(map[string]any{"image_urls": []string{canonicalReference}, "n": 1})
				require.NoError(t, err)
				assert.JSONEq(t, string(expected), record.body, "custom templates must receive the validated and converted reference")
			} else if tc.body != nil {
				assert.JSONEq(t, `{"prompt":"draw","n":1}`, record.body)
			} else {
				assert.Contains(t, record.body, `"contents"`)
				assert.NotContains(t, record.body, `"model"`, "Gemini uses the URL model without an injected JSON field")
			}
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			assert.Equal(t, "vendor-job", task.UpstreamTaskId)
			request.Reservation, err = model.GetAsyncImageReservation(t.Context(), task)
			require.NoError(t, err)
			require.NotNil(t, request.Reservation)
			request.ClientIP = "192.0.2.10"
			request.Parts = append(request.Parts, service.AsyncImageInputPart{Type: "image_url", URL: "https://expired.invalid/reference.png"})
			require.NoError(t, db.Model(&token).Update("allow_ips", "198.51.100.0/24").Error)
			require.NoError(t, db.Model(&user).Update("status", common.UserStatusDisabled).Error)
			require.NoError(t, db.Delete(&token).Error)
			defer func() {
				require.NoError(t, db.Unscoped().Model(&model.Token{}).Where("id = ?", token.Id).Updates(map[string]any{"deleted_at": nil, "allow_ips": nil}).Error)
				require.NoError(t, db.Model(&user).Update("status", common.UserStatusEnabled).Error)
			}()
			_, _, err = ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error { dispatches++; return nil })
			require.ErrorAs(t, err, &pending)
			assert.Equal(t, 1, dispatches, "polling must not cross the submission barrier again")
			record = <-received
			assert.Equal(t, http.MethodGet, record.method)
			assert.Equal(t, "/tasks/vendor-job", record.path)
		})
	}
}

func TestAsyncImageSettlementPreservesPromptExtension(t *testing.T) {
	previousDB, previousQuotaUnit := model.DB, common.QuotaPerUnit
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "ali-image-bill.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, common.QuotaPerUnit = db, 500000
	t.Cleanup(func() {
		model.DB, common.QuotaPerUnit = previousDB, previousQuotaUnit
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}))
	user := model.User{Username: "ali-image-bill-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	for _, tc := range []struct {
		name             string
		extend, fallback bool
		want             int
	}{
		{name: "local_extended", extend: true, want: 10000},
		{name: "fallback_extended", extend: true, fallback: true, want: 10000},
		{name: "local_disabled", want: 5000},
		{name: "fallback_disabled", fallback: true, want: 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations_async", nil)
			images := []service.ImageBytes{{Width: 1024, Height: 1024}}
			if tc.fallback {
				c.Set("async_image_result_source", "upstream")
				images[0] = service.ImageBytes{}
			}
			info := &relaycommon.RelayInfo{UserId: user.Id, OriginModelName: "z-image", ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeAli, UpstreamModelName: "z-image"}, StartTime: time.Now(), Request: &dto.ImageRequest{}, PriceData: types.PriceData{UsePrice: true, ModelPrice: 0.01, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
			require.Nil(t, service.EstimateImageBillingForRequest(info, 2, tc.extend))
			assert.Equal(t, 2*tc.want, info.PriceData.QuotaToPreConsume)
			bill, err := service.FixAsyncImageBill(c, model.AsyncImageTask{TaskId: "ali-bill-" + tc.name, UserId: user.Id, TokenId: 1, ChannelId: 1}, info, &dto.Usage{}, images, service.AsyncImageRequest{Platform: "openai", Model: "z-image", Count: 2}, model.ImageFundingSelection{Source: "wallet"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, bill.Quota, "settle actual image count using the captured prompt-extension multiplier")
		})
	}
}

func TestUpstreamImageResultReadRetriesExistingTask(t *testing.T) {
	service.InitHttpClient()
	var pngBytes bytes.Buffer
	require.NoError(t, png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	redirectAuthorization := make(chan string, 1)
	privateRedirects := make(chan struct{}, 1)
	privateDestination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		privateRedirects <- struct{}{}
	}))
	defer privateDestination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/partial":
			w.Header().Set("Content-Length", "12")
			_, _ = w.Write([]byte("partial"))
		case "/busy":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/redirect":
			http.Redirect(w, request, "/image", http.StatusFound)
		case "/redirect-private":
			http.Redirect(w, request, privateDestination.URL, http.StatusFound)
		case "/image":
			redirectAuthorization <- request.Header.Get("Authorization")
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes.Bytes())
		}
	}))
	defer upstream.Close()
	profile := &dto.UpstreamAsyncProfile{}
	profile.Poll.Response.DownloadHeaders = map[string]string{"Authorization": "Bearer {api_key}"}
	cfg := service.DefaultImageRuntimeConfig()
	for _, path := range []string{"/partial", "/busy", "/unavailable"} {
		_, err := service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + path}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
		require.ErrorIs(t, err, service.ErrUpstreamAsyncImageTransport, path)
		failure := classifyAsyncImageOutputFailure(model.AsyncImageTask{UpstreamTaskId: "vendor-job-1"}, err)
		assert.Equal(t, 606, failure.Code, path)
		assert.Equal(t, "result_download_failed", failure.InternalCode, path)
		assert.False(t, failure.ExecutionUnknown, path)
		assert.Equal(t, 607, classifyAsyncImageOutputFailure(model.AsyncImageTask{}, err).Code, path)
	}
	_, err := service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + "/missing"}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
	require.Error(t, err)
	assert.NotErrorIs(t, err, service.ErrUpstreamAsyncImageTransport)
	images, err := service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + "/redirect"}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
	require.NoError(t, err)
	require.Len(t, images, 1)
	assert.Equal(t, "Bearer test-key", <-redirectAuthorization)
	_, err = service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + "/redirect-private"}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
	require.ErrorContains(t, err, "not public")
	assert.Empty(t, privateRedirects)
}

func TestRecordAsyncImageUpstreamJobAfterDispatch(t *testing.T) {
	previousDB, previousType := model.DB, common.MainDatabaseType()
	var dialector gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "upstream-job.db"))
	if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_MYSQL_DSN"); dsn != "" {
		require.Contains(t, dsn, "/new_api_image_job_test", "use a dedicated disposable database")
		dialector = mysql.Open(dsn)
	} else if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_PG_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "dbname=new_api_image_job_test") || strings.Contains(dsn, "/new_api_image_job_test"), "use a dedicated disposable database")
		dialector = postgres.Open(dsn)
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseType(db.Dialector.Name()))
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
		assert.NoError(t, db.Migrator().DropTable(&model.AsyncImageBill{}, &model.AsyncImageEvent{}, &model.AsyncImageTask{}))
		connection, openErr := db.DB()
		if openErr == nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}))
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}))
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "checkpoint-test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "checkpoint-test")

	now := time.Now().Unix()
	task := model.AsyncImageTask{
		TaskId:         "asyncimg_stale_after_dispatch",
		Status:         model.ImageTaskInvoking,
		Version:        1,
		LeaseToken:     "worker-lease",
		LeaseExpiresAt: now + 120,
		CreatedAt:      now,
	}
	require.NoError(t, db.Create(&task).Error)
	require.NoError(t, model.MarkAsyncImageDispatched(t.Context(), task, 20))

	require.NoError(t, recordAsyncImageUpstreamJob(t.Context(), &task, "vendor-job-1"))
	assert.Equal(t, "vendor-job-1", task.UpstreamTaskId)
	assert.EqualValues(t, 3, task.Version)
	require.NoError(t, recordAsyncImageUpstreamJob(t.Context(), &task, "vendor-job-1"))
	assert.EqualValues(t, 3, task.Version)
	require.ErrorIs(t, recordAsyncImageUpstreamJob(t.Context(), &task, "vendor-job-2"), model.ErrImageConflict)

	var stored model.AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&stored).Error)
	assert.Equal(t, "vendor-job-1", stored.UpstreamTaskId)
	var accepted int64
	require.NoError(t, db.Model(&model.AsyncImageEvent{}).Where("task_id = ? AND event_type = ?", task.TaskId, "upstream_accepted").Count(&accepted).Error)
	assert.EqualValues(t, 1, accepted)

	request := service.AsyncImageRequest{Platform: "openai", Model: "checkpoint-image", Count: 1}
	completion := service.AsyncImageCompletion{EvaluatedAt: time.Date(2026, time.October, 13, 8, 15, 0, 0, time.UTC), Body: []byte(`{"data":[]}`), Usage: &dto.Usage{}}
	for _, name := range []string{"current_lease_after_dispatch", "expired_lease", "superseded_lease", "admin_terminated"} {
		t.Run(name, func(t *testing.T) {
			checkpointTask := model.AsyncImageTask{TaskId: "checkpoint-" + name, Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "checkpoint-worker", LeaseExpiresAt: now + 120, ChannelId: 20}
			requestBytes, err := common.Marshal(request)
			require.NoError(t, err)
			checkpointTask.RequestCipher, err = service.EncryptImagePayload(requestBytes, "task:"+checkpointTask.TaskId)
			require.NoError(t, err)
			checkpointTask.SelectedChannelCipher, err = service.EncryptImagePayload([]byte(`{"id":20,"key":"synthetic-key"}`), "image-channel:"+checkpointTask.TaskId)
			require.NoError(t, err)
			if name == "expired_lease" {
				checkpointTask.LeaseExpiresAt = now - 1
			}
			require.NoError(t, db.Create(&checkpointTask).Error)
			switch name {
			case "current_lease_after_dispatch":
				require.NoError(t, model.MarkAsyncImageDispatched(t.Context(), checkpointTask, checkpointTask.ChannelId))
			case "superseded_lease":
				require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", checkpointTask.TaskId).Updates(map[string]any{"lease_token": "replacement-worker", "version": 2}).Error)
			case "admin_terminated":
				require.NoError(t, model.TerminateAsyncImageTask(t.Context(), checkpointTask))
			}
			var before model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", checkpointTask.TaskId).Take(&before).Error)
			err = service.PersistAsyncImageCompletion(t.Context(), &checkpointTask, request, completion)
			if name == "current_lease_after_dispatch" {
				require.NoError(t, err, "a live owner must reload the version incremented by dispatch")
				assert.Equal(t, before.Version+1, checkpointTask.Version)
				assert.Equal(t, completion.EvaluatedAt.Unix(), checkpointTask.UpstreamSucceededAt)
				data, err := service.DecryptImagePayload(checkpointTask.RequestCipher, "task:"+checkpointTask.TaskId)
				require.NoError(t, err)
				var recovered service.AsyncImageRequest
				require.NoError(t, common.Unmarshal(data, &recovered))
				require.NotNil(t, recovered.Completion)
				assert.JSONEq(t, string(completion.Body), string(recovered.Completion.Body))
				laterCompletion := completion
				laterCompletion.EvaluatedAt = completion.EvaluatedAt.Add(time.Hour)
				require.NoError(t, service.PersistAsyncImageCompletion(t.Context(), &checkpointTask, request, laterCompletion))
				assert.Equal(t, completion.EvaluatedAt.Unix(), checkpointTask.UpstreamSucceededAt, "repeat checkpoints must retain the first provider completion time")
				data, err = service.DecryptImagePayload(checkpointTask.RequestCipher, "task:"+checkpointTask.TaskId)
				require.NoError(t, err)
				var repeated service.AsyncImageRequest
				require.NoError(t, common.Unmarshal(data, &repeated))
				require.NotNil(t, repeated.Completion)
				assert.True(t, completion.EvaluatedAt.Equal(repeated.Completion.EvaluatedAt), "the saved pricing time must match the first completion marker")
				return
			}
			require.Error(t, err)
			var after model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", checkpointTask.TaskId).Take(&after).Error)
			assert.Equal(t, before.Status, after.Status)
			assert.Equal(t, before.Version, after.Version)
			assert.Equal(t, before.RequestCipher, after.RequestCipher)
			assert.Equal(t, before.UpstreamSucceededAt, after.UpstreamSucceededAt, "a stale worker cannot revive or overwrite a task")
		})
	}

	var pngImage bytes.Buffer
	require.NoError(t, png.Encode(&pngImage, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	for _, tc := range []struct {
		name  string
		bytes int
	}{
		{name: "checkpoint_at_32_mib", bytes: 32 << 20},
		{name: "checkpoint_above_32_mib", bytes: (32 << 20) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("IMAGE_UPSTREAM_JOB_TEST_SMALL_PACKET") != "" {
				require.Equal(t, "mysql", db.Dialector.Name(), "small-packet mode requires the isolated MySQL fixture")
				t.Skip("the isolated small-packet fixture exercises its lower checkpoint limit")
			}
			response := map[string]any{"data": []dto.ImageData{{B64Json: base64.StdEncoding.EncodeToString(pngImage.Bytes())}}, "metadata": ""}
			body, err := common.Marshal(response)
			require.NoError(t, err)
			boundedCompletion := service.AsyncImageCompletion{EvaluatedAt: completion.EvaluatedAt, Body: body, Usage: &dto.Usage{}}
			encodedRequest := request
			encodedRequest.Completion = &boundedCompletion
			baseEnvelope, err := common.Marshal(encodedRequest)
			require.NoError(t, err)
			response["metadata"] = strings.Repeat("x", tc.bytes-len(baseEnvelope))
			boundedCompletion.Body, err = common.Marshal(response)
			require.NoError(t, err)
			checkpointTask := model.AsyncImageTask{TaskId: tc.name, Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "boundary-worker", LeaseExpiresAt: time.Now().Unix() + 120}
			requestBytes, err := common.Marshal(request)
			require.NoError(t, err)
			checkpointTask.RequestCipher, err = service.EncryptImagePayload(requestBytes, "task:"+checkpointTask.TaskId)
			require.NoError(t, err)
			checkpointTask.SelectedChannelCipher, err = service.EncryptImagePayload([]byte(`{"id":20,"key":"synthetic-key"}`), "image-channel:"+checkpointTask.TaskId)
			require.NoError(t, err)
			originalCipher := append([]byte(nil), checkpointTask.RequestCipher...)
			require.NoError(t, db.Create(&checkpointTask).Error)
			err = service.PersistAsyncImageCompletion(t.Context(), &checkpointTask, request, boundedCompletion)
			if tc.name == "checkpoint_above_32_mib" {
				require.ErrorIs(t, err, service.ErrAsyncImageCompletionTooLarge)
			} else {
				require.NoError(t, err)
			}
			var stored model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", checkpointTask.TaskId).Take(&stored).Error)
			if err != nil {
				assert.Equal(t, originalCipher, stored.RequestCipher)
				assert.Zero(t, stored.UpstreamSucceededAt)
				assert.EqualValues(t, 1, stored.Version)
				return
			}
			assert.Positive(t, stored.UpstreamSucceededAt)
			data, err := service.DecryptImagePayload(stored.RequestCipher, "task:"+stored.TaskId)
			require.NoError(t, err)
			var recovered service.AsyncImageRequest
			require.NoError(t, common.Unmarshal(data, &recovered))
			require.NotNil(t, recovered.Completion)
			var imageResponse dto.ImageResponse
			require.NoError(t, common.Unmarshal(recovered.Completion.Body, &imageResponse))
			require.Len(t, imageResponse.Data, 1)
			assert.Equal(t, base64.StdEncoding.EncodeToString(pngImage.Bytes()), imageResponse.Data[0].B64Json)
		})
	}
	t.Run("checkpoint_respects_mysql_small_packet", func(t *testing.T) {
		if os.Getenv("IMAGE_UPSTREAM_JOB_TEST_SMALL_PACKET") == "" {
			t.Skip("requires an isolated MySQL instance configured with a 4 MiB packet")
		}
		require.Equal(t, "mysql", db.Dialector.Name())
		var packetBytes int64
		require.NoError(t, db.Raw("SELECT @@max_allowed_packet").Scan(&packetBytes).Error)
		require.EqualValues(t, 4<<20, packetBytes, "configure the disposable instance before starting the test process")
		response := map[string]any{"data": []dto.ImageData{{B64Json: base64.StdEncoding.EncodeToString(pngImage.Bytes())}}, "metadata": ""}
		body, err := common.Marshal(response)
		require.NoError(t, err)
		response["metadata"] = strings.Repeat("x", (3<<20)-len(body))
		body, err = common.Marshal(response)
		require.NoError(t, err)
		checkpointTask := model.AsyncImageTask{TaskId: "checkpoint-small-mysql-packet", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "small-packet-worker", LeaseExpiresAt: time.Now().Unix() + 120}
		requestBytes, err := common.Marshal(request)
		require.NoError(t, err)
		checkpointTask.RequestCipher, err = service.EncryptImagePayload(requestBytes, "task:"+checkpointTask.TaskId)
		require.NoError(t, err)
		checkpointTask.SelectedChannelCipher, err = service.EncryptImagePayload([]byte(`{"id":20,"key":"synthetic-key"}`), "image-channel:"+checkpointTask.TaskId)
		require.NoError(t, err)
		originalCipher := append([]byte(nil), checkpointTask.RequestCipher...)
		require.NoError(t, db.Create(&checkpointTask).Error)
		err = service.PersistAsyncImageCompletion(t.Context(), &checkpointTask, request, service.AsyncImageCompletion{Body: body, Usage: &dto.Usage{}})
		require.ErrorIs(t, err, service.ErrAsyncImageCompletionTooLarge)
		var stored model.AsyncImageTask
		require.NoError(t, db.Where("task_id = ?", checkpointTask.TaskId).Take(&stored).Error)
		assert.Equal(t, originalCipher, stored.RequestCipher, "an output exceeding the server packet budget must not overwrite encrypted evidence")
		assert.Zero(t, stored.UpstreamSucceededAt)
		assert.EqualValues(t, 1, stored.Version)
	})
}

func TestTianyueSynchronousImageCompletionRecovery(t *testing.T) {
	previousDB, previousType, previousCount, previousRedis := model.DB, common.MainDatabaseType(), constant.CountToken, common.RedisEnabled
	var dialector gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "tianyue-sync-completion.db"))
	if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_MYSQL_DSN"); dsn != "" {
		require.Contains(t, dsn, "/new_api_image_job_test", "use a dedicated disposable database")
		dialector = mysql.Open(dsn)
	} else if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_PG_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "dbname=new_api_image_job_test") || strings.Contains(dsn, "/new_api_image_job_test"), "use a dedicated disposable database")
		dialector = postgres.Open(dsn)
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled = db, false, false
	common.SetMainDatabaseType(common.DatabaseType(db.Dialector.Name()))
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.RedisEnabled = previousDB, previousCount, previousRedis
		common.SetMainDatabaseType(previousType)
		assert.NoError(t, db.Migrator().DropTable(&model.AsyncImageBill{}, &model.AsyncImageEvent{}, &model.AsyncImageTask{}, &model.UserSubscription{}, &model.Token{}, &model.User{}))
		connection, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}))
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "tianyue-sync:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "tianyue-sync")
	user := model.User{Username: "tianyue-sync-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "tianyue-sync-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	service.InitHttpClient()
	var bitmap bytes.Buffer
	require.NoError(t, png.Encode(&bitmap, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	received := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Method + " " + r.URL.Path
		assert.Equal(t, "Bearer tianyue-upstream-key", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/images/generations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"/image.png"}]}`))
		case "/image.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(bitmap.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	channel := &model.Channel{Id: 34, Type: constant.ChannelTypeNewAPI, Key: "tianyue-upstream-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled}
	channel.SetSetting(dto.ChannelSettings{TianyueMediaCompatibility: true})
	request := service.AsyncImageRequest{
		Platform: "openai", Provider: "newapi", Dialect: "async", Model: "seedream-5-pro", Prompt: "test picture", Count: 1,
		Kind: "text_to_image", Size: "1024x1024", Resolution: "1K", SourcePath: "/v1/images/generations_async",
		Native:  map[string]common.RawMessage{"model": common.RawMessage(`"seedream-5-pro"`), "prompt": common.RawMessage(`"test picture"`), "n": common.RawMessage(`1`)},
		Billing: &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}},
	}
	task := model.AsyncImageTask{TaskId: "tianyue-sync-completion", UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120}
	payload, err := common.Marshal(request)
	require.NoError(t, err)
	task.RequestCipher, err = service.EncryptImagePayload(payload, "task:"+task.TaskId)
	require.NoError(t, err)
	channelPayload, err := common.Marshal(channel)
	require.NoError(t, err)
	task.SelectedChannelCipher, err = service.EncryptImagePayload(channelPayload, "image-channel:"+task.TaskId)
	require.NoError(t, err)
	require.NoError(t, db.Create(&task).Error)
	dispatches := 0
	output, bill, err := ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error {
		dispatches++
		return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
	})
	require.NoError(t, err, "a synchronous provider response must finish without a provider job ID")
	require.NotNil(t, output)
	require.Len(t, output.Images, 1)
	assert.Equal(t, 2, output.Images[0].Width)
	assert.Equal(t, 3, output.Images[0].Height)
	assert.Equal(t, 5000, bill.Quota)
	require.Len(t, received, 2)
	assert.Equal(t, "POST /v1/images/generations", <-received)
	assert.Equal(t, "GET /image.png", <-received)
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
	assert.Empty(t, task.UpstreamTaskId, "synchronous completion must not invent a provider job")
	assert.Positive(t, task.UpstreamSucceededAt)
	payload, err = service.DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
	require.NoError(t, err)
	var recovered service.AsyncImageRequest
	require.NoError(t, common.Unmarshal(payload, &recovered))
	require.NotNil(t, recovered.Completion)
	require.NotNil(t, recovered.Completion.Profile)
	assert.Equal(t, []string{upstream.URL + "/image.png"}, recovered.Completion.URLs)
	recoveredOutput, recoveredBill, err := ExecuteAsyncImage(t.Context(), task, recovered, channel, service.DefaultImageRuntimeConfig(), func() error {
		dispatches++
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, recoveredOutput)
	require.Len(t, recoveredOutput.Images, 1)
	assert.Equal(t, output.Images[0].Data, recoveredOutput.Images[0].Data)
	assert.Equal(t, bill.Quota, recoveredBill.Quota)
	require.Len(t, received, 1)
	assert.Equal(t, "GET /image.png", <-received)
	assert.Empty(t, received, "checkpoint recovery must only download the accepted image")
	assert.Equal(t, 1, dispatches, "checkpoint recovery must not repeat submission")
	require.NoError(t, db.Take(&user, user.Id).Error)
	assert.Equal(t, 995000, user.Quota, "checkpoint recovery must reuse the existing reservation")
	var reservations int64
	require.NoError(t, db.Model(&model.AsyncImageBill{}).Where("task_id = ?", task.TaskId).Count(&reservations).Error)
	assert.EqualValues(t, 1, reservations)
}

func TestBuildGeminiAsyncImagePartsReferenceTransport(t *testing.T) {
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "gemini-reference-transport.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		connection, openErr := db.DB()
		if openErr == nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.ImageInputAlias{}))

	t.Run("passes external URL to upstream before fallback", func(t *testing.T) {
		cfg := service.DefaultImageRuntimeConfig()
		cfg.GeminiReferenceMode = "passthrough_fallback_local"
		referenceURL := "https://signed.example/reference.jpg?token=private"
		request := service.AsyncImageRequest{Parts: []service.AsyncImageInputPart{
			{Type: "text", Text: "edit"},
			{Type: "image_url", URL: referenceURL},
		}}

		parts, err := buildGeminiAsyncImageParts(t.Context(), model.AsyncImageTask{TokenId: 7}, request, cfg)
		require.NoError(t, err)
		require.Len(t, parts, 2)
		require.NotNil(t, parts[1].FileData)
		assert.Equal(t, referenceURL, parts[1].FileData.FileUri)
		assert.Empty(t, parts[1].FileData.MimeType)
		assert.Nil(t, parts[1].InlineData)
	})

	t.Run("indexes invalid locally transported reference without URL", func(t *testing.T) {
		cfg := service.DefaultImageRuntimeConfig()
		cfg.GeminiReferenceMode = "local"
		request := service.AsyncImageRequest{Parts: []service.AsyncImageInputPart{
			{Type: "text", Text: "edit"},
			{Type: "image_url", URL: "data:image/jpeg;base64,bm90LWltYWdl"},
		}}

		_, err := buildGeminiAsyncImageParts(t.Context(), model.AsyncImageTask{TokenId: 7}, request, cfg)
		require.Error(t, err)
		var failure *service.AsyncImageFailure
		require.ErrorAs(t, err, &failure)
		assert.Equal(t, 604, failure.Code)
		assert.Equal(t, "invalid_reference_image", failure.InternalCode)
		assert.Equal(t, "Reference image 1: invalid image header", failure.Message)
		assert.NotContains(t, failure.Message, "bm90LWltYWdl")
	})
}
