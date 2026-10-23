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
	"github.com/QuantumNous/new-api/relay/channel/volcengine"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
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
	imageOperation := upstreamAsyncImageOperation(c)
	asyncProfile, hasAsyncProfile := info.ChannelOtherSettings.UpstreamAsync.Match(dto.UpstreamAsyncMediaImage, info.UpstreamModelName, imageOperation)
	if !hasAsyncProfile && info.ChannelType == constant.ChannelTypeNewAPI && info.ChannelSetting.TianyueMediaCompatibility {
		asyncProfile = service.TianyueAsyncImageProfile(info.UpstreamModelName)
		hasAsyncProfile = asyncProfile != nil
		c.Set(contextKeyTianyueImageProfile, hasAsyncProfile)
	}
	if hasAsyncProfile && !c.GetBool("async_image_execution") {
		return nil, types.NewErrorWithStatusCode(fmt.Errorf("channel model requires the asynchronous image endpoint"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	imageCount, err := request.ImageCount(info.ChannelType == constant.ChannelTypeAli)
	if err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	validatedImageCount := imageCount
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
			mapped := c.GetString(asyncImageResolutionModelContextKey)
			if info.ChannelType == constant.ChannelTypeVolcEngine || info.ImageMixedBilling {
				mapped = info.UpstreamModelName
			}
			if mapped != "" {
				jsonData, err = sjson.SetBytes(jsonData, "model", mapped)
				if err != nil {
					return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
				}
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
			jsonData, newAPIError = prepareImageJSONSubmission(c, info, request, convertedRequest)
			if newAPIError != nil {
				return nil, newAPIError
			}

		}
	}
	if jsonData != nil {
		if info.ImageMixedBilling {
			// A provider converter or channel override cannot change the mixed
			// task's admitted quantity, including native Imagen sampleCount.
			probe, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(jsonData))
			if err != nil {
				return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			if err := service.ValidateMixedImageSubmitCount(probe, validatedImageCount); err != nil {
				return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
		}
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
		if info.ChannelType == constant.ChannelTypeVolcEngine {
			jsonData, err = volcengine.PrepareImageSubmission(jsonData, info.UpstreamModelName, imageCount, c.GetBool("async_image_execution"))
			if err != nil {
				return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
		}
		if c.GetBool("async_image_execution") && info.ChannelType == constant.ChannelTypeNewAPI && info.ChannelSetting.TianyueMediaCompatibility {
			if err := service.ValidateTianyueImageSubmission(jsonData, imageCount); err != nil {
				return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
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
	var submitRequest *http.Request
	if hasAsyncProfile && asyncProfile.Submit.Request != nil && c.GetString("async_image_resume_task") == "" {
		source, _ := c.Get(contextKeyUpstreamAsyncSubmitSource)
		submitRequest, err = service.BuildUpstreamAsyncSubmitRequest(c.Request.Context(), info.ChannelBaseUrl, asyncProfile, upstreamAsyncImageTemplateContext(info, ""), source, requestBody, c.Request.Header.Get("Content-Type"))
		if err != nil {
			return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeConvertRequestFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if asyncProfile.Submit.Request.Body != nil {
			if info.ChannelType == constant.ChannelTypeVolcEngine {
				if submitRequest.GetBody == nil {
					return nil, types.NewErrorWithStatusCode(fmt.Errorf("Seedream image submit body is unavailable"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
				}
				reader, readErr := submitRequest.GetBody()
				if readErr != nil {
					return nil, types.NewError(readErr, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
				}
				encoded, readErr := io.ReadAll(io.LimitReader(reader, (64<<20)+1))
				_ = reader.Close()
				if readErr == nil && len(encoded) <= 64<<20 {
					encoded, readErr = volcengine.PrepareImageSubmission(encoded, info.UpstreamModelName, imageCount, true)
				} else if readErr == nil {
					readErr = fmt.Errorf("Seedream image submit body exceeds the byte limit")
				}
				if readErr != nil {
					return nil, types.NewErrorWithStatusCode(readErr, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
				}
				_ = submitRequest.Body.Close()
				submitRequest.Body = io.NopCloser(bytes.NewReader(encoded))
				submitRequest.ContentLength = int64(len(encoded))
				submitRequest.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }
			}
			if info.ChannelType == constant.ChannelTypeNewAPI && info.ChannelSetting.TianyueMediaCompatibility {
				if submitRequest.GetBody == nil {
					return nil, types.NewErrorWithStatusCode(fmt.Errorf("TianYue image submit body is unavailable"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
				}
				reader, readErr := submitRequest.GetBody()
				if readErr != nil {
					return nil, types.NewError(readErr, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
				}
				encoded, readErr := io.ReadAll(io.LimitReader(reader, (64<<20)+1))
				_ = reader.Close()
				if readErr == nil && len(encoded) <= 64<<20 {
					readErr = service.ValidateTianyueImageSubmission(encoded, imageCount)
				} else if readErr == nil {
					readErr = fmt.Errorf("TianYue image submit body exceeds the byte limit")
				}
				if readErr != nil {
					return nil, types.NewErrorWithStatusCode(readErr, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
				}
			}
			if err := validateAsyncImageResolutionSubmitModel(c, submitRequest); err != nil {
				return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			validateCount := service.ValidateUpstreamAsyncImageSubmitCount
			if info.ImageMixedBilling {
				validateCount = service.ValidateMixedImageSubmitCount
			}
			if err := validateCount(submitRequest, imageCount); err != nil {
				return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
		}
	}
	if beforeInvoke != nil && c.GetString("async_image_resume_task") == "" {
		if err := beforeInvoke(); err != nil {
			return nil, types.NewError(err, types.ErrorCodeDoRequestFailed, types.ErrOptionWithSkipRetry())
		}
	}

	var resp any
	if taskId := c.GetString("async_image_resume_task"); taskId != "" {
		if hasAsyncProfile {
			resp, err = service.PollUpstreamAsync(c.Request.Context(), info.ChannelBaseUrl, info.ChannelSetting.Proxy, asyncProfile, upstreamAsyncImageTemplateContext(info, taskId))
		} else if provider, ok := adaptor.(channel.AsyncImagePollingProvider); ok {
			resp, err = provider.PollImage(c, info, taskId)
		} else {
			err = fmt.Errorf("image adaptor cannot resume upstream jobs")
		}
	} else if submitRequest != nil {
		wasStream := info.IsStream
		info.IsStream = false
		resp, err = channel.DoRequest(c, submitRequest, info)
		info.IsStream = wasStream
	} else {
		resp, err = adaptor.DoRequest(c, info, requestBody)
	}
	if err != nil {
		if reserve {
			_ = service.RecordImageCircuit(c.Request.Context(), "sync", info.ChannelId, false, circuit)
		}
		if hasAsyncProfile && c.GetString("async_image_resume_task") != "" {
			// The async executor owns the known-job barrier and retains the
			// transport cause to distinguish timeouts from disconnections.
			return nil, types.NewError(err, types.ErrorCodeDoRequestFailed)
		}
		return nil, types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}
	var httpResp *http.Response
	if resp != nil {
		httpResp = resp.(*http.Response)
		if !reserve && c.GetBool("async_image_execution") {
			c.Set(asyncImageUpstreamRequestIDContextKey, service.AsyncImageResponseRequestID(httpResp.Header))
			c.Set(asyncImageUpstreamStatusContextKey, httpResp.StatusCode)
		}
		if !reserve {
			if limit := c.GetInt64("async_image_response_limit"); limit > 0 {
				httpResp.Body = struct {
					io.Reader
					io.Closer
				}{io.LimitReader(httpResp.Body, limit+1), httpResp.Body}
			}
		}
		info.IsStream = info.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")
		if (!hasAsyncProfile && httpResp.StatusCode != http.StatusOK) || (hasAsyncProfile && (httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices)) {
			if (httpResp.StatusCode == http.StatusCreated && info.ApiType == constant.APITypeReplicate) || (c.GetBool("async_image_execution") && httpResp.StatusCode == http.StatusAccepted && info.ApiType == constant.APITypeAli) {
				// replicate channel returns 201 Created when using Prefer: wait, treat it as success.
				httpResp.StatusCode = http.StatusOK
			} else {
				if !reserve {
					c.Set("async_image_retry_after", httpResp.Header.Get("Retry-After"))
				}
				if !reserve && c.GetBool("async_image_execution") && (info.ApiType == constant.APITypeOpenAI || info.ApiType == constant.APITypeXai || info.ApiType == constant.APITypeNewAPI) {
					failure := service.ClassifyOpenAIAsyncImageHTTPFailure(c.Request.Context(), httpResp, statusCodeMappingStr)
					if info.ApiType == constant.APITypeNewAPI && c.GetString("async_image_resume_task") == "" {
						// A gateway's error codes describe its own upstream call, not
						// this connection. Keep ambiguous submissions reserved while
						// retaining the provider's HTTP response for reconciliation.
						switch types.ErrorCode(failure.ProviderCode) {
						case types.ErrorCodeDoRequestFailed, types.ErrorCodeReadResponseBodyFailed, types.ErrorCodeBadResponse, types.ErrorCodeBadResponseBody, types.ErrorCodeEmptyResponse:
							failure.Code, failure.InternalCode = 608, "execution_unknown"
							failure.ExecutionUnknown = true
						}
					}
					return nil, types.NewErrorWithStatusCode(failure, types.ErrorCodeBadResponseStatusCode, failure.HTTPStatus)
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
	if hasAsyncProfile {
		usage, apiErr := handleUpstreamAsyncImageResponse(c, info, httpResp, asyncProfile)
		if apiErr != nil {
			return nil, apiErr
		}
		return &ImageExecutionResult{Request: request, Usage: usage}, nil
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

// Resolution mappings are routing decisions, so later administrator overrides
// cannot silently send a different model than the selected profile and audit.
func validateAsyncImageResolutionModel(c *gin.Context, body []byte, required bool) error {
	expected := c.GetString(asyncImageResolutionModelContextKey)
	if expected == "" {
		return nil
	}
	actual := gjson.GetBytes(body, "model")
	if !actual.Exists() && !required {
		return nil
	}
	if actual.Type != gjson.String || actual.String() != expected {
		return fmt.Errorf("outbound image model conflicts with the asynchronous resolution model mapping")
	}
	return nil
}

// Custom protocols may use another field or the URL for their model. Validate
// an explicit model without adding a field to an administrator's template.
func validateAsyncImageResolutionSubmitModel(c *gin.Context, request *http.Request) error {
	if c.GetString(asyncImageResolutionModelContextKey) == "" {
		return nil
	}
	if request.GetBody == nil {
		return fmt.Errorf("upstream async submit body is unavailable")
	}
	body, err := request.GetBody()
	if err != nil {
		return err
	}
	defer body.Close()
	encoded, err := io.ReadAll(io.LimitReader(body, (64<<20)+1))
	if err != nil || len(encoded) > 64<<20 {
		return fmt.Errorf("upstream async submit body exceeds the byte limit")
	}
	return validateAsyncImageResolutionModel(c, encoded, false)
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

// prepareImageJSONSubmission is shared by admission and dispatch. It performs
// no I/O and applies the same native-field and administrator-override rules.
func prepareImageJSONSubmission(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ImageRequest, converted any) ([]byte, *types.NewAPIError) {
	data, err := common.Marshal(converted)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	if c.GetBool("async_image_execution") && info.ApiType != constant.APITypeAli && info.ApiType != constant.APITypeReplicate && info.ApiType != constant.APITypeGemini && info.ApiType != constant.APITypeVertexAi && info.ApiType != constant.APITypeXai {
		var fields map[string]common.RawMessage
		if err := common.Unmarshal(data, &fields); err != nil || fields == nil {
			return nil, types.NewError(fmt.Errorf("image adaptor must produce a JSON object"), types.ErrorCodeConvertRequestFailed)
		}
		for key, value := range request.Extra {
			fields[key] = value
		}
		data, err = common.Marshal(fields)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed)
		}
	}
	requiresModel := gjson.GetBytes(data, "model").Exists()
	if len(info.ParamOverride) > 0 {
		data, err = relaycommon.ApplyParamOverrideWithRelayInfo(data, info)
		if err != nil {
			return nil, newAPIErrorFromParamOverride(err)
		}
	}
	if err := validateAsyncImageResolutionModel(c, data, requiresModel); err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	return data, nil
}
