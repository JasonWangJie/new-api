package controller

import (
	"bytes"
	"errors"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/model"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

const (
	channelTestImagePendingCode           types.ErrorCode = "channel_test_upstream_image_pending"
	channelTestImageAcceptanceUnknownCode types.ErrorCode = "channel_test_upstream_acceptance_unknown"
	channelTestImageFailedCode            types.ErrorCode = "channel_test_upstream_image_failed"
	channelTestImagePreviewFailedCode     types.ErrorCode = "channel_test_upstream_image_preview_failed"
)

// A probe owns one upstream submission and waits for its result without
// creating a site task or entering the site's billing lifecycle.
func runInteractiveTabtabImageTest(c *gin.Context, info *relaycommon.RelayInfo, input *channelTestInput, body []byte) (*channelTestOutput, *types.NewAPIError) {
	ctx := c.Request.Context()
	body, err := service.TabtabImageSubmission(body, info.UpstreamModelName, 1, info.ChannelOtherSettings.TabtabImageModelProfiles)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed)
	}
	profile := service.TabtabAsyncImageProfile(info.UpstreamModelName)
	values := service.UpstreamAsyncTemplateContext{
		Model: info.OriginModelName, UpstreamModel: info.UpstreamModelName, APIKey: info.ApiKey,
	}
	request, err := service.BuildUpstreamAsyncSubmitRequest(ctx, info.ChannelBaseUrl, profile, values, nil, bytes.NewReader(body), "application/json")
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed)
	}
	if err := ctx.Err(); err != nil {
		return nil, types.NewError(err, types.ErrorCodeDoRequestFailed)
	}
	acceptanceUnknown := types.NewError(errors.New("TabTab image task acceptance could not be confirmed"), channelTestImageAcceptanceUnknownCode)
	response, err := relaychannel.DoRequest(c, request, info)
	if err != nil {
		return nil, acceptanceUnknown
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		failure := service.ClassifyTabtabImageHTTPFailure(ctx, response, c.GetString("status_code_mapping"))
		if failure.SubmissionRejected {
			return nil, types.NewError(failure, types.ErrorCodeBadResponseStatusCode)
		}
		return nil, acceptanceUnknown
	}
	normalized, err := readInteractiveTabtabTaskResponse(response, info.ChannelBaseUrl, "")
	if err == nil {
		values.TaskID, err = service.ParseUpstreamAsyncTaskID(profile, normalized)
	}
	if err != nil {
		return nil, acceptanceUnknown
	}
	resultUnknown := types.NewError(errors.New("The TabTab image task was accepted. Its result could not be confirmed"), channelTestImagePendingCode)
	result, err := service.ParseUpstreamAsyncPollResponse(profile, normalized)
	if err != nil {
		return nil, resultUnknown
	}

	cfg := service.DefaultImageRuntimeConfig()
	ticker := time.NewTicker(time.Duration(cfg.PollInterval) * time.Second)
	defer ticker.Stop()
	for waitBeforePolling := false; ; waitBeforePolling = true {
		switch result.Status {
		case model.TaskStatusSuccess:
			images, err := service.DownloadTianyueImages(ctx, result.URLs, profile, info.ChannelBaseUrl, info.ChannelSetting.Proxy, values, cfg)
			if err != nil {
				return nil, types.NewError(err, channelTestImagePreviewFailedCode)
			}
			preview, err := channelTestImagePreview(images[0])
			if err != nil {
				return nil, types.NewError(err, channelTestImagePreviewFailedCode)
			}
			return &channelTestOutput{Model: input.Model, Mode: input.Mode, EndpointType: input.EndpointType, Image: preview}, nil
		case model.TaskStatusFailure:
			message := result.Reason
			if message == "" {
				message = "The TabTab image task failed"
			}
			return nil, types.NewError(errors.New(message), channelTestImageFailedCode)
		}
		if waitBeforePolling {
			select {
			case <-ctx.Done():
				return nil, resultUnknown
			case <-ticker.C:
			}
		}
		response, err = service.PollUpstreamAsync(ctx, info.ChannelBaseUrl, info.ChannelSetting.Proxy, profile, values)
		if err != nil {
			// Only the already-accepted task's read can be retried.
			continue
		}
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
			service.CloseResponseBodyGracefully(response)
			continue
		}
		normalized, err = readInteractiveTabtabTaskResponse(response, info.ChannelBaseUrl, values.TaskID)
		if err != nil {
			return nil, resultUnknown
		}
		result, err = service.ParseUpstreamAsyncPollResponse(profile, normalized)
		if err != nil || result.Status == model.TaskStatusUnknown {
			return nil, resultUnknown
		}
	}
}

func readInteractiveTabtabTaskResponse(response *http.Response, baseURL, taskID string) ([]byte, error) {
	defer service.CloseResponseBodyGracefully(response)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, errors.New("TabTab task status could not be read")
	}
	body, err := service.ReadUpstreamAsyncResponse(response, 64<<10)
	if err != nil {
		return nil, err
	}
	return service.NormalizeTabtabImageTask(body, baseURL, taskID)
}
