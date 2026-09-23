package relay

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	contextKeyUpstreamAsyncImageProfile = "upstream_async_image_profile"
	contextKeyUpstreamAsyncImageURLs    = "upstream_async_image_urls"
	contextKeyUpstreamAsyncOperation    = "upstream_async_operation"
	contextKeyUpstreamAsyncSubmitSource = "upstream_async_submit_source"
)

func upstreamAsyncImageOperation(c *gin.Context) string {
	if operation := c.GetString(contextKeyUpstreamAsyncOperation); operation == dto.UpstreamAsyncOperationEdit {
		return operation
	}
	if c != nil && c.Request != nil && strings.Contains(c.Request.URL.Path, "/images/edits") {
		return dto.UpstreamAsyncOperationEdit
	}
	return dto.UpstreamAsyncOperationGenerate
}

func handleUpstreamAsyncImageResponse(c *gin.Context, info *relaycommon.RelayInfo, response *http.Response, profile *dto.UpstreamAsyncProfile) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(response)
	body, err := service.ReadUpstreamAsyncResponse(response, c.GetInt64("async_image_response_limit"))
	if err != nil {
		return nil, types.NewError(&service.AsyncImageFailure{Code: 606, InternalCode: "upstream_response_invalid", Message: "Upstream image task response could not be parsed"}, types.ErrorCodeBadResponse)
	}
	resumeTaskID := c.GetString("async_image_resume_task")
	if resumeTaskID == "" {
		taskID, parseErr := service.ParseUpstreamAsyncTaskID(profile, body)
		if parseErr != nil {
			return nil, types.NewError(&service.AsyncImageFailure{Code: 608, InternalCode: "execution_unknown", Message: "Upstream image task ID could not be parsed", ExecutionUnknown: true}, types.ErrorCodeBadResponse)
		}
		hook, exists := c.Get("async_image_upstream_job")
		record, ok := hook.(func(string) error)
		if !exists || !ok {
			return nil, types.NewError(&service.AsyncImageFailure{Code: 608, InternalCode: "execution_unknown", Message: "Upstream image task could not be persisted", ExecutionUnknown: true}, types.ErrorCodeBadResponse)
		}
		if err := record(taskID); err != nil {
			return nil, types.NewError(&service.AsyncImageFailure{Code: 608, InternalCode: "execution_unknown", Message: "Upstream image task could not be persisted", ExecutionUnknown: true}, types.ErrorCodeBadResponse)
		}
		return nil, types.NewError(&service.AsyncImagePending{TaskId: taskID}, types.ErrorCodeBadResponse)
	}

	parsed, err := service.ParseUpstreamAsyncPollResponse(profile, body)
	if err != nil {
		if errors.Is(err, service.ErrUpstreamAsyncBillingUsage) {
			return nil, types.NewError(&service.AsyncImageFailure{Code: 606, InternalCode: "billing_usage_missing", Message: "Upstream image task reported invalid billing usage"}, types.ErrorCodeBadResponse)
		}
		return nil, types.NewError(&service.AsyncImageFailure{Code: 606, InternalCode: "upstream_result_parse_failed", Message: "Upstream image task result could not be parsed"}, types.ErrorCodeBadResponse)
	}
	switch parsed.Status {
	case "QUEUED", "IN_PROGRESS", "SUBMITTED", "NOT_START":
		return nil, types.NewError(&service.AsyncImagePending{TaskId: resumeTaskID}, types.ErrorCodeBadResponse)
	case "FAILURE":
		message := parsed.Reason
		if message == "" {
			message = "Upstream image task failed"
		}
		return nil, types.NewError(&service.AsyncImageFailure{Code: 610, InternalCode: "upstream_job_failed", Message: message}, types.ErrorCodeBadResponse)
	case "SUCCESS":
	default:
		return nil, types.NewError(&service.AsyncImageFailure{Code: 606, InternalCode: "upstream_status_unknown", Message: "Upstream image task returned an unknown status"}, types.ErrorCodeBadResponse)
	}
	if len(parsed.URLs) == 0 {
		return nil, types.NewError(&service.AsyncImageFailure{Code: 606, InternalCode: "upstream_result_missing", Message: "Upstream image task result URL is missing"}, types.ErrorCodeBadResponse)
	}
	c.Set(contextKeyUpstreamAsyncImageProfile, profile)
	c.Set(contextKeyUpstreamAsyncImageURLs, append([]string(nil), parsed.URLs...))
	responseData := make([]dto.ImageData, len(parsed.URLs))
	for index, resultURL := range parsed.URLs {
		responseData[index].Url = resultURL
	}
	encoded, err := common.Marshal(dto.ImageResponse{Created: time.Now().Unix(), Data: responseData})
	if err != nil {
		return nil, types.NewError(errors.New("failed to encode upstream image results"), types.ErrorCodeBadResponseBody)
	}
	service.IOCopyBytesGracefully(c, nil, encoded)
	if parsed.Usage == nil {
		return &dto.Usage{UsageSource: "upstream_async"}, nil
	}
	return parsed.Usage, nil
}

func upstreamAsyncImageTemplateContext(info *relaycommon.RelayInfo, taskID string) service.UpstreamAsyncTemplateContext {
	return service.UpstreamAsyncTemplateContext{
		TaskID:        taskID,
		Model:         info.OriginModelName,
		UpstreamModel: info.UpstreamModelName,
		APIKey:        info.ApiKey,
	}
}
