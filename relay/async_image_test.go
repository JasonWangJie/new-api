package relay

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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
		maxReferences              int
		status, failureCode        int
		responseMessage            string
		providerCode               string
		statusMapping              string
		fileFormat                 string
		passThrough                bool
	}{
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
			headers := `{"X-Image-Probe":"synthetic"}`
			channel := &model.Channel{Id: 1, Key: "synthetic-upstream-key", BaseURL: &upstream.URL, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "image-alias", Group: "default", ModelMapping: &mapping, ParamOverride: &parameters, HeaderOverride: &headers}
			channel.StatusCodeMapping = &tc.statusMapping
			channel.SetSetting(dto.ChannelSettings{PassThroughBodyEnabled: tc.passThrough})
			body := map[string]any{
				"model": "image-alias", "prompt": "draw a river", "n": 1,
				"size": "1024x1024", "quality": "high", "response_format": "b64_json",
				"mask":      map[string]string{"image_url": maskURL},
				"watermark": false, "seed": 0, "custom_toggle": false,
				"vendor_options": map[string]any{"strength": 0, "enabled": false},
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
			assert.Equal(t, "synthetic", record.marker)
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
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}))
	user := model.User{Username: "gemini-submit-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{Id: 17, UserId: user.Id, Key: "gemini-submit-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	service.InitHttpClient()
	for _, tc := range []struct {
		name    string
		body    map[string]any
		invalid bool
	}{
		{name: "template", body: map[string]any{"prompt": "{request.prompt}", "n": "{request.n}"}},
		{name: "native_body"},
		{name: "conflicting_count", body: map[string]any{"n": float64(2)}, invalid: true},
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
			assert.Equal(t, request.Model, record.query)
			if tc.body != nil {
				assert.JSONEq(t, `{"prompt":"draw","n":1}`, record.body)
			} else {
				assert.Contains(t, record.body, `"contents"`)
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
		assert.NoError(t, db.Migrator().DropTable(&model.AsyncImageEvent{}, &model.AsyncImageTask{}))
		connection, openErr := db.DB()
		if openErr == nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}))
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}))

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
