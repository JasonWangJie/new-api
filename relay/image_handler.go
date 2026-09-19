package relay

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

func ExecuteImageRequest(c *gin.Context, info *relaycommon.RelayInfo, reserve bool, beforeInvoke func() error) (*ImageExecutionResult, *types.NewAPIError) {
	var newAPIError *types.NewAPIError
	info.InitChannelMeta(c)
	info.BillingImageCount = nil
	info.ImageRequestCount = 0

	imageReq, ok := info.Request.(*dto.ImageRequest)
	if !ok {
		return nil, types.NewErrorWithStatusCode(fmt.Errorf("invalid request type, expected dto.ImageRequest, got %T", info.Request), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	circuit := service.DefaultImageRuntimeConfig()
	if reserve && model.DB != nil {
		var err error
		circuit, err = service.GetImageRuntimeConfig(c.Request.Context())
		if err != nil {
			return nil, types.NewErrorWithStatusCode(fmt.Errorf("image runtime configuration is unavailable"), types.ErrorCodeModelPriceError, http.StatusServiceUnavailable)
		}
		allowed, err := service.ImageCircuitAllows(c.Request.Context(), "sync", info.ChannelId, circuit)
		if err != nil || !allowed {
			return nil, types.NewErrorWithStatusCode(fmt.Errorf("image channel circuit is unavailable"), types.ErrorCodeDoRequestFailed, http.StatusServiceUnavailable)
		}
	}

	request, err := common.DeepCopy(imageReq)
	if err != nil {
		return nil, types.NewError(fmt.Errorf("failed to copy request to ImageRequest: %w", err), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	err = helper.ModelMappedHelper(c, info, request)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return nil, types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)
	imageCount, err := request.ImageCount(info.ChannelType == constant.ChannelTypeAli)
	if err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	promptExtend := request.BillingParameters != nil && request.BillingParameters.PromptExtend != nil && *request.BillingParameters.PromptExtend

	var requestBody io.Reader
	var jsonData []byte

	if model_setting.GetGlobalSettings().PassThroughRequestEnabled || info.ChannelSetting.PassThroughBodyEnabled {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if strings.Contains(c.Request.Header.Get("Content-Type"), "multipart/form-data") {
			requestBody = common.NewReplayableBodyReader(storage)
		} else {
			jsonData, err = storage.Bytes()
			if err != nil {
				return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
		}
	} else {
		var convertedRequest any
		if c.GetString("async_image_resume_task") != "" {
			convertedRequest = request
		} else {
			convertedRequest, err = adaptor.ConvertImageRequest(c, info, *request)
		}
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed)
		}
		relaycommon.AppendRequestConversionFromRequest(info, convertedRequest)

		switch convertedRequest.(type) {
		case *bytes.Buffer:
			requestBody = convertedRequest.(io.Reader)
		default:
			jsonData, err = common.Marshal(convertedRequest)
			if err != nil {
				return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
			}
			if c.GetBool("async_image_execution") {
				if info.ApiType != constant.APITypeAli && info.ApiType != constant.APITypeReplicate && info.ApiType != constant.APITypeGemini && info.ApiType != constant.APITypeVertexAi {
					var fields map[string]common.RawMessage
					if err := common.Unmarshal(jsonData, &fields); err != nil {
						return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed)
					}
					for key, value := range request.Extra {
						fields[key] = value
					}
					jsonData, err = common.Marshal(fields)
					if err != nil {
						return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed)
					}
				}
			}

			// apply param override
			if len(info.ParamOverride) > 0 {
				jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
				if err != nil {
					return nil, newAPIErrorFromParamOverride(err)
				}
			}

		}
	}
	if jsonData != nil {
		// This is a different trust boundary from ingress: channel overrides
		// and pass-through bodies can change the quantity actually submitted.
		var outbound struct {
			N          *uint                       `json:"n"`
			Parameters *dto.ImageBillingParameters `json:"parameters"`
		}
		if err := common.Unmarshal(jsonData, &outbound); err != nil {
			return nil, types.NewErrorWithStatusCode(fmt.Errorf("invalid image billing parameters: %w", err), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if c.GetBool("async_image_execution") {
			var fields map[string]common.RawMessage
			if err := common.Unmarshal(jsonData, &fields); err != nil {
				return nil, types.NewError(err, types.ErrorCodeInvalidRequest)
			}
			quantities := make(map[string]common.RawMessage)
			for _, key := range []string{"batch_size", "num_outputs"} {
				if raw, ok := fields[key]; ok {
					quantities[key] = raw
				}
			}
			if raw, ok := fields["input"]; ok {
				var input map[string]common.RawMessage
				if err := common.Unmarshal(raw, &input); err != nil {
					return nil, types.NewErrorWithStatusCode(fmt.Errorf("invalid provider image input: %w", err), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
				}
				if count, exists := input["num_outputs"]; exists {
					quantities["input.num_outputs"] = count
				}
			}
			if raw, ok := fields["sequential_image_generation_options"]; ok {
				var options map[string]common.RawMessage
				if err := common.Unmarshal(raw, &options); err != nil {
					return nil, types.NewErrorWithStatusCode(fmt.Errorf("invalid sequential image options: %w", err), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
				}
				if count, exists := options["max_images"]; exists {
					quantities["sequential_image_generation_options.max_images"] = count
				}
			}
			for name, raw := range quantities {
				var count uint
				if common.Unmarshal(raw, &count) != nil || count < 1 || count > dto.MaxImageN || int(count) != imageCount {
					return nil, types.NewErrorWithStatusCode(fmt.Errorf("%s conflicts with the validated image count", name), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
				}
			}
		}
		quantityRequest := dto.ImageRequest{N: outbound.N, BillingParameters: outbound.Parameters}
		if quantityRequest.N == nil {
			quantityRequest.N = common.GetPointer(uint(imageCount))
		}
		imageCount, err = quantityRequest.ImageCount(info.ChannelType == constant.ChannelTypeAli)
		if err != nil {
			return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if outbound.Parameters != nil {
			promptExtend = outbound.Parameters.PromptExtend != nil && *outbound.Parameters.PromptExtend
		} else if c.GetString("async_image_resume_task") == "" {
			promptExtend = false
		}
		if info.ChannelType == constant.ChannelTypeAli {
			// Always send the same explicit quantity that is reserved, including
			// when an empty parameters object accompanies a top-level n.
			jsonData, err = sjson.SetBytes(jsonData, "parameters.n", imageCount)
			if err != nil {
				return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
			}
		}
		if reserve {
			logger.LogDebug(c, "image request body: %s", jsonData)
		}
		body, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		defer closer.Close()
		requestBody = body
	}
	var billingErr *types.NewAPIError
	if reserve {
		billingErr = service.PrepareImageBillingForRequest(c, info, imageCount, promptExtend)
	} else {
		billingErr = service.EstimateImageBillingForRequest(info, imageCount, promptExtend)
	}
	if billingErr != nil {
		return nil, billingErr
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")
	if err := c.Request.Context().Err(); err != nil {
		return nil, types.NewError(err, types.ErrorCodeDoRequestFailed, types.ErrOptionWithSkipRetry())
	}
	if beforeInvoke != nil && c.GetString("async_image_resume_task") == "" {
		if err := beforeInvoke(); err != nil {
			return nil, types.NewError(err, types.ErrorCodeDoRequestFailed, types.ErrOptionWithSkipRetry())
		}
	}

	var resp any
	if taskId := c.GetString("async_image_resume_task"); taskId != "" {
		if provider, ok := adaptor.(channel.AsyncImagePollingProvider); ok {
			resp, err = provider.PollImage(c, info, taskId)
		} else {
			err = fmt.Errorf("image adaptor cannot resume upstream jobs")
		}
	} else {
		resp, err = adaptor.DoRequest(c, info, requestBody)
	}
	if err != nil {
		if reserve {
			_ = service.RecordImageCircuit(c.Request.Context(), "sync", info.ChannelId, false, circuit)
		}
		return nil, types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}
	var httpResp *http.Response
	if resp != nil {
		httpResp = resp.(*http.Response)
		if !reserve {
			if limit := c.GetInt64("async_image_response_limit"); limit > 0 {
				httpResp.Body = struct {
					io.Reader
					io.Closer
				}{io.LimitReader(httpResp.Body, limit+1), httpResp.Body}
			}
		}
		info.IsStream = info.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")
		if httpResp.StatusCode != http.StatusOK {
			if (httpResp.StatusCode == http.StatusCreated && info.ApiType == constant.APITypeReplicate) || (c.GetBool("async_image_execution") && httpResp.StatusCode == http.StatusAccepted && info.ApiType == constant.APITypeAli) {
				// replicate channel returns 201 Created when using Prefer: wait, treat it as success.
				httpResp.StatusCode = http.StatusOK
			} else {
				if !reserve {
					c.Set("async_image_retry_after", httpResp.Header.Get("Retry-After"))
				}
				if reserve && (httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= 500) {
					_ = service.RecordImageCircuit(c.Request.Context(), "sync", info.ChannelId, false, circuit)
				}
				newAPIError = service.RelayErrorHandler(c.Request.Context(), httpResp, false)
				// reset status code 重置状态码
				service.ResetStatusCode(newAPIError, statusCodeMappingStr)
				return nil, newAPIError
			}
		}
	}

	usage, newAPIError := adaptor.DoResponse(c, httpResp, info)
	if newAPIError != nil {
		// reset status code 重置状态码
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return nil, newAPIError
	}

	imageUsage, ok := usage.(*dto.Usage)
	if !ok || imageUsage == nil {
		return nil, types.NewError(fmt.Errorf("image usage is missing"), types.ErrorCodeBadResponse)
	}
	if reserve {
		_ = service.RecordImageCircuit(c.Request.Context(), "sync", info.ChannelId, true, circuit)
	}
	return &ImageExecutionResult{Request: request, Usage: imageUsage}, nil
}

type ImageExecutionResult struct {
	Request *dto.ImageRequest
	Usage   *dto.Usage
}

func ImageHelper(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	result, err := ExecuteImageRequest(c, info, true, nil)
	if err != nil {
		return err
	}
	if result.Usage.TotalTokens == 0 {
		result.Usage.TotalTokens = 1
	}
	if result.Usage.PromptTokens == 0 {
		result.Usage.PromptTokens = 1
	}
	quality := result.Request.Quality
	if quality == "" {
		quality = "standard"
	}
	var logContent []string
	if result.Request.Size != "" {
		logContent = append(logContent, fmt.Sprintf("大小 %s", result.Request.Size))
	}
	logContent = append(logContent, fmt.Sprintf("品质 %s", quality))
	count := uint(1)
	if result.Request.N != nil {
		count = *result.Request.N
	}
	if count > 0 {
		logContent = append(logContent, fmt.Sprintf("生成数量 %d", count))
	}
	service.PostTextConsumeQuota(c, info, result.Usage, logContent)
	return nil
}
