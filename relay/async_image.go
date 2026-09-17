package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

type asyncImageResponseWriter struct {
	Headers http.Header
	Status  int
	Body    bytes.Buffer
	Limit   int64
}

func (writer *asyncImageResponseWriter) Header() http.Header { return writer.Headers }
func (writer *asyncImageResponseWriter) WriteHeader(status int) {
	if writer.Status == 0 {
		writer.Status = status
	}
}
func (writer *asyncImageResponseWriter) Flush() { writer.WriteHeader(http.StatusOK) }
func (writer *asyncImageResponseWriter) Write(data []byte) (int, error) {
	if writer.Status == 0 {
		writer.Status = http.StatusOK
	}
	if int64(writer.Body.Len())+int64(len(data)) > writer.Limit {
		return 0, errors.New("async image response byte limit exceeded")
	}
	return writer.Body.Write(data)
}

// EstimateAsyncImageQuota checks the configured price using validated request
// scalars. It does not select a channel, download references or reserve funds.
func EstimateAsyncImageQuota(ctx context.Context, token model.Token, request service.AsyncImageRequest, group string) (int, error) {
	if request.Count < 1 || request.Count > dto.MaxImageN {
		return 0, errors.New("invalid image count")
	}
	var user model.User
	if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", token.UserId, common.UserStatusEnabled).Take(&user).Error; err != nil {
		return 0, err
	}
	writer := &asyncImageResponseWriter{Headers: make(http.Header), Limit: 1 << 20}
	c, _ := gin.CreateTestContext(writer)
	c.Request, _ = http.NewRequestWithContext(ctx, http.MethodPost, "/v1/images/generations", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	base := model.UserBase{Id: user.Id, Group: user.Group, Quota: user.Quota, Status: user.Status, Username: user.Username, Email: user.Email, Setting: user.Setting}
	base.WriteContext(c)
	common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, group)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, request.Model)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, group)
	var parsed dto.Request
	if request.Platform == "openai" {
		native := make(map[string]common.RawMessage)
		maps.Copy(native, request.Native)
		delete(native, "images")
		delete(native, "mask")
		delete(native, "image")
		body, err := common.Marshal(native)
		if err != nil {
			return 0, err
		}
		image := &dto.ImageRequest{}
		if err := common.Unmarshal(body, image); err != nil {
			return 0, err
		}
		image.Model, image.Prompt, image.Size, image.N = request.Model, request.Prompt, request.Size, common.GetPointer(uint(request.Count))
		parsed = image
	} else {
		config, err := common.Marshal(map[string]string{"imageSize": request.Resolution, "aspectRatio": request.AspectRatio})
		if err != nil {
			return 0, err
		}
		parsed = &dto.GeminiChatRequest{Contents: []dto.GeminiChatContent{{Role: "user", Parts: []dto.GeminiPart{{Text: request.Prompt}}}}, GenerationConfig: dto.GeminiChatGenerationConfig{ResponseModalities: []string{"TEXT", "IMAGE"}, ImageConfig: config}}
	}
	var info *relaycommon.RelayInfo
	if request.Platform == "openai" {
		info = relaycommon.GenRelayInfoImage(c, parsed)
	} else {
		info = relaycommon.GenRelayInfoGemini(c, parsed)
	}
	info.ChannelMeta = &relaycommon.ChannelMeta{}
	facts := service.ImageBillingSpecifications(request, []service.ImageBytes{{Width: 1024, Height: 1024}})[0]
	encoded, err := common.Marshal(facts)
	if err != nil {
		return 0, err
	}
	info.BillingRequestInput = &billingexpr.RequestInput{Body: encoded}
	c.Set("async_image_billing_facts", facts)
	meta := parsed.GetTokenCountMeta()
	estimated, err := service.EstimateRequestToken(c, meta, info)
	if err != nil {
		return 0, err
	}
	info.SetEstimatePromptTokens(estimated)
	price, err := helper.ModelPriceHelper(c, info, estimated, meta)
	if err != nil {
		return 0, err
	}
	info.PriceData = price
	if request.Platform == "openai" {
		if err := service.EstimateImageBillingForRequest(info, request.Count, false); err != nil {
			return 0, err
		}
	}
	return info.PriceData.QuotaToPreConsume, nil
}

func ExecuteAsyncImage(ctx context.Context, task model.AsyncImageTask, request service.AsyncImageRequest, channel *model.Channel, cfg service.ImageRuntimeConfig, beforeInvoke func() error) ([]service.ImageBytes, model.AsyncImageBill, error) {
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", task.TokenId, task.UserId).Take(&token).Error; err != nil {
		return nil, model.AsyncImageBill{}, err
	}
	var user model.User
	if err := model.DB.WithContext(ctx).Where("id = ?", task.UserId).Take(&user).Error; err != nil {
		return nil, model.AsyncImageBill{}, err
	}
	limit := int64(dto.MaxImageN) * cfg.DownloadMaxBytes * 2
	writer := &asyncImageResponseWriter{Headers: make(http.Header), Limit: limit}
	c, _ := gin.CreateTestContext(writer)
	c.Set("async_image_response_limit", limit)
	upstreamPath := "/v1/images/generations"
	if request.Kind == "image_to_image" {
		upstreamPath = "/v1/images/edits"
	}
	if request.Platform == "gemini" {
		upstreamPath = "/v1beta/models/" + request.Model + ":generateContent"
	}
	c.Request, _ = http.NewRequestWithContext(ctx, http.MethodPost, upstreamPath, nil)
	c.Request.Header.Set("Content-Type", "application/json")
	base := model.UserBase{Id: user.Id, Group: user.Group, Quota: user.Quota, Status: user.Status, Username: user.Username, Email: user.Email, Setting: user.Setting}
	base.WriteContext(c)
	if err := middleware.SetupContextForToken(c, &token); err != nil {
		return nil, model.AsyncImageBill{}, err
	}
	common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, task.Group)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, task.Group)
	if err := middleware.SetupContextForSelectedChannel(c, channel, request.Model); err != nil {
		return nil, model.AsyncImageBill{}, err
	}
	var parsed dto.Request
	var nativeBody []byte
	if request.Platform == "openai" {
		var images []map[string]string
		var totalBytes, totalPixels int64
		mode := cfg.OpenAIReferenceMode
		if task.ReferenceRetryCount > 0 && mode == "passthrough_fallback_local" {
			mode = "local"
		}
		for _, part := range request.Parts {
			if part.Type != "image_url" && part.Type != "mask" {
				continue
			}
			reference := part.URL
			bound, err := service.IsBoundImageReference(ctx, task.TokenId, reference)
			if err != nil {
				return nil, model.AsyncImageBill{}, &service.AsyncImageFailure{Code: 604, Message: "SC input has expired or is unavailable"}
			}
			if bound || mode == "local" || strings.HasPrefix(strings.ToLower(reference), "data:") {
				image, _, err := service.ResolveImageReference(ctx, task.TokenId, reference, cfg)
				if err != nil {
					return nil, model.AsyncImageBill{}, &service.AsyncImageFailure{Code: 602, InternalCode: "reference_download_failed", Message: err.Error()}
				}
				totalBytes += int64(len(image.Data))
				totalPixels += int64(image.Width) * int64(image.Height)
				if totalBytes > cfg.ReferenceTotalBytes || totalPixels > cfg.ReferenceTotalPixels {
					return nil, model.AsyncImageBill{}, &service.AsyncImageFailure{Code: 604, Message: "Reference images exceed the combined byte or pixel budget"}
				}
				reference = image.DataURL()
			}
			if part.Type == "mask" {
				mask, err := common.Marshal(map[string]string{"image_url": reference})
				if err != nil {
					return nil, model.AsyncImageBill{}, err
				}
				request.Native["mask"] = mask
				continue
			}
			images = append(images, map[string]string{"image_url": reference})
		}
		if len(images) > 0 {
			data, err := common.Marshal(images)
			if err != nil {
				return nil, model.AsyncImageBill{}, err
			}
			request.Native["images"] = data
		}
		var err error
		nativeBody, err = common.Marshal(request.Native)
		if err != nil {
			return nil, model.AsyncImageBill{}, err
		}
		imageRequest := &dto.ImageRequest{}
		if err := common.Unmarshal(nativeBody, imageRequest); err != nil {
			return nil, model.AsyncImageBill{}, err
		}
		parsed = imageRequest
	} else {
		parts := make([]dto.GeminiPart, 0, len(request.Parts))
		var totalBytes, totalPixels int64
		for _, part := range request.Parts {
			if part.Type == "text" {
				parts = append(parts, dto.GeminiPart{Text: part.Text})
				continue
			}
			image, bound, err := service.ResolveImageReference(ctx, task.TokenId, part.URL, cfg)
			if err != nil {
				return nil, model.AsyncImageBill{}, &service.AsyncImageFailure{Code: 602, InternalCode: "reference_download_failed", Message: err.Error()}
			}
			totalBytes += int64(len(image.Data))
			totalPixels += int64(image.Width) * int64(image.Height)
			if totalBytes > cfg.ReferenceTotalBytes || totalPixels > cfg.ReferenceTotalPixels {
				return nil, model.AsyncImageBill{}, &service.AsyncImageFailure{Code: 604, InternalCode: "reference_budget_exceeded", Message: "Reference images exceed the combined byte or pixel budget"}
			}
			if !bound && cfg.GeminiReferenceMode != "local" && !strings.HasPrefix(strings.ToLower(part.URL), "data:") {
				parts = append(parts, dto.GeminiPart{FileData: &dto.GeminiFileData{MimeType: image.ContentType, FileUri: part.URL}})
			} else {
				parts = append(parts, dto.GeminiPart{InlineData: &dto.GeminiInlineData{MimeType: image.ContentType, Data: base64.StdEncoding.EncodeToString(image.Data)}})
			}
		}
		imageConfig := map[string]string{}
		if request.Resolution != "" {
			imageConfig["imageSize"] = request.Resolution
		}
		if request.AspectRatio != "" {
			imageConfig["aspectRatio"] = request.AspectRatio
		}
		config, err := common.Marshal(imageConfig)
		if err != nil {
			return nil, model.AsyncImageBill{}, err
		}
		geminiRequest := &dto.GeminiChatRequest{Contents: []dto.GeminiChatContent{{Role: "user", Parts: parts}}, GenerationConfig: dto.GeminiChatGenerationConfig{ResponseModalities: []string{"TEXT", "IMAGE"}, ImageConfig: config}}
		parsed = geminiRequest
		nativeBody, err = common.Marshal(geminiRequest)
		if err != nil {
			return nil, model.AsyncImageBill{}, err
		}
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(nativeBody))
	c.Request.ContentLength = int64(len(nativeBody))
	storage, err := common.CreateBodyStorage(nativeBody)
	if err != nil {
		return nil, model.AsyncImageBill{}, err
	}
	defer storage.Close()
	c.Set(common.KeyBodyStorage, storage)
	var info *relaycommon.RelayInfo
	if request.Platform == "openai" {
		info = relaycommon.GenRelayInfoImage(c, parsed)
	} else {
		info = relaycommon.GenRelayInfoGemini(c, parsed)
	}
	info.InitChannelMeta(c)
	facts := service.ImageBillingSpecifications(request, []service.ImageBytes{{Width: 1024, Height: 1024}})[0]
	encodedFacts, err := common.Marshal(facts)
	if err != nil {
		return nil, model.AsyncImageBill{}, err
	}
	info.BillingRequestInput = &billingexpr.RequestInput{Body: encodedFacts}
	c.Set("async_image_billing_facts", facts)
	meta := parsed.GetTokenCountMeta()
	estimated, err := service.EstimateRequestToken(c, meta, info)
	if err != nil {
		return nil, model.AsyncImageBill{}, err
	}
	info.SetEstimatePromptTokens(estimated)
	price, err := helper.ModelPriceHelper(c, info, estimated, meta)
	if err != nil {
		return nil, model.AsyncImageBill{}, err
	}
	info.PriceData = price
	funding, err := model.SelectImageFunding(ctx, user.Id, price.QuotaToPreConsume, user.GetSetting().BillingPreference)
	if err != nil {
		return nil, model.AsyncImageBill{}, err
	}
	if !token.UnlimitedQuota && token.RemainQuota < price.QuotaToPreConsume {
		return nil, model.AsyncImageBill{}, model.ErrImageInsufficientQuota
	}
	var usage *dto.Usage
	dispatched := false
	upstreamBeforeInvoke := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := beforeInvoke(); err != nil {
			return err
		}
		dispatched = true
		return nil
	}
	if request.Platform == "openai" {
		result, apiErr := ExecuteImageRequest(c, info, false, upstreamBeforeInvoke)
		if apiErr != nil {
			failure := service.ClassifyAsyncImageFailure(apiErr, apiErr.StatusCode)
			failure.RetryAfter = service.ImageRetryAfter(c.GetString("async_image_retry_after"), time.Now())
			if dispatched {
				switch apiErr.GetErrorCode() {
				case types.ErrorCodeDoRequestFailed, types.ErrorCodeReadResponseBodyFailed, types.ErrorCodeBadResponse, types.ErrorCodeBadResponseBody, types.ErrorCodeEmptyResponse:
					failure.ExecutionUnknown = true
					failure.Code = 608
				}
			}
			return nil, model.AsyncImageBill{}, failure
		}
		usage = result.Usage
	} else {
		if err := helper.ModelMappedHelper(c, info, parsed); err != nil {
			return nil, model.AsyncImageBill{}, err
		}
		adaptor := GetAdaptor(info.ApiType)
		if adaptor == nil {
			return nil, model.AsyncImageBill{}, errors.New("Gemini image adaptor is unavailable")
		}
		adaptor.Init(info)
		converted, err := adaptor.ConvertGeminiRequest(c, info, parsed.(*dto.GeminiChatRequest))
		if err != nil {
			return nil, model.AsyncImageBill{}, err
		}
		body, err := common.Marshal(converted)
		if err != nil {
			return nil, model.AsyncImageBill{}, err
		}
		if len(info.ParamOverride) > 0 {
			body, err = relaycommon.ApplyParamOverrideWithRelayInfo(body, info)
			if err != nil {
				return nil, model.AsyncImageBill{}, err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, model.AsyncImageBill{}, err
		}
		if err := upstreamBeforeInvoke(); err != nil {
			return nil, model.AsyncImageBill{}, err
		}
		response, err := adaptor.DoRequest(c, info, bytes.NewReader(body))
		if err != nil {
			return nil, model.AsyncImageBill{}, &service.AsyncImageFailure{Code: 608, InternalCode: "execution_unknown", Message: "Upstream transport failed after dispatch", ExecutionUnknown: true}
		}
		httpResponse, ok := response.(*http.Response)
		if !ok || httpResponse == nil {
			return nil, model.AsyncImageBill{}, &service.AsyncImageFailure{Code: 608, Message: "Upstream response is missing", ExecutionUnknown: true}
		}
		httpResponse.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(httpResponse.Body, limit+1), httpResponse.Body}
		if httpResponse.StatusCode != http.StatusOK {
			retryAfter := service.ImageRetryAfter(httpResponse.Header.Get("Retry-After"), time.Now())
			apiErr := service.RelayErrorHandler(ctx, httpResponse, false)
			failure := service.ClassifyAsyncImageFailure(apiErr, httpResponse.StatusCode)
			failure.RetryAfter = retryAfter
			return nil, model.AsyncImageBill{}, failure
		}
		value, apiErr := adaptor.DoResponse(c, httpResponse, info)
		if apiErr != nil {
			// HTTP 200 already followed a dispatched generation. A response read
			// or conversion failure cannot prove the request was not executed.
			return nil, model.AsyncImageBill{}, &service.AsyncImageFailure{Code: 608, InternalCode: "execution_unknown", Message: "Upstream image response could not be decoded after dispatch", ExecutionUnknown: true}
		}
		usage, ok = value.(*dto.Usage)
		if !ok {
			return nil, model.AsyncImageBill{}, errors.New("Gemini usage is missing")
		}
	}
	images, err := ExtractAsyncImageOutputs(ctx, writer.Body.Bytes(), request.Platform, cfg)
	if err != nil {
		return nil, model.AsyncImageBill{}, &service.AsyncImageFailure{Code: 607, InternalCode: "output_parse_failed", Message: err.Error()}
	}
	if usage == nil {
		return nil, model.AsyncImageBill{}, &service.AsyncImageFailure{Code: 607, Message: "Upstream usage is missing"}
	}
	info.BillingRequestInput = &billingexpr.RequestInput{Body: nativeBody, ImageCount: common.GetPointer(len(images))}
	bill, err := service.FixAsyncImageBill(c, task, info, usage, images, request, funding)
	if err != nil {
		return nil, model.AsyncImageBill{}, &service.AsyncImageFailure{Code: 608, InternalCode: "bill_preparation_failed", Message: err.Error(), ExecutionUnknown: true}
	}
	return images, bill, nil
}

func ExtractAsyncImageOutputs(ctx context.Context, body []byte, platform string, cfg service.ImageRuntimeConfig) ([]service.ImageBytes, error) {
	var references []string
	if platform == "openai" {
		var response struct {
			Data []struct {
				URL string `json:"url"`
				B64 string `json:"b64_json"`
			} `json:"data"`
		}
		if err := common.Unmarshal(body, &response); err != nil {
			return nil, err
		}
		for _, item := range response.Data {
			if item.B64 != "" {
				references = append(references, "data:"+http.DetectContentType(decodeAsyncImagePrefix(item.B64))+";base64,"+item.B64)
			} else if item.URL != "" {
				references = append(references, item.URL)
			} else {
				return nil, errors.New("upstream image data item is empty")
			}
		}
	} else {
		var response dto.GeminiChatResponse
		if err := common.Unmarshal(body, &response); err != nil {
			return nil, err
		}
		for _, candidate := range response.Candidates {
			for _, part := range candidate.Content.Parts {
				if part.InlineData != nil && strings.HasPrefix(part.InlineData.MimeType, "image/") {
					references = append(references, "data:"+part.InlineData.MimeType+";base64,"+part.InlineData.Data)
				}
			}
		}
	}
	if len(references) == 0 || len(references) > dto.MaxImageN {
		return nil, fmt.Errorf("upstream must return between 1 and %d complete images", dto.MaxImageN)
	}
	images := make([]service.ImageBytes, 0, len(references))
	for _, reference := range references {
		image, err := service.DownloadImageReference(ctx, reference, cfg)
		if err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	return images, nil
}

func decodeAsyncImagePrefix(value string) []byte {
	if len(value) > 512 {
		value = value[:512]
	}
	decoded, _ := base64.StdEncoding.DecodeString(value)
	return decoded
}
