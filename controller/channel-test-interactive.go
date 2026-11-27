package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/tidwall/gjson"
)

const (
	channelTestModeChat           = "chat"
	channelTestModeImage          = "image"
	channelTestPromptLimit        = 4096
	channelTestTextResponseLimit  = 1 << 20
	channelTestImageResponseLimit = 44 << 20
)

type channelTestInput struct {
	Model        string                `json:"model"`
	Mode         string                `json:"mode"`
	Prompt       string                `json:"prompt"`
	ImageURLs    []string              `json:"image_urls,omitempty"`
	EndpointType constant.EndpointType `json:"endpoint_type"`
	Stream       bool                  `json:"stream"`
	requestID    string
}

type channelTestModelOptions struct {
	Model              string                  `json:"model"`
	DefaultMode        string                  `json:"default_mode"`
	ChatEndpointTypes  []constant.EndpointType `json:"chat_endpoint_types,omitempty"`
	ImageEndpointTypes []constant.EndpointType `json:"image_endpoint_types,omitempty"`
}

type channelTestOutput struct {
	Model        string                `json:"model"`
	Mode         string                `json:"mode"`
	EndpointType constant.EndpointType `json:"endpoint_type"`
	Text         string                `json:"text,omitempty"`
	Image        *channelTestImage     `json:"image,omitempty"`
}

type channelTestImage struct {
	DataURL string `json:"data_url"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

// Interactive modes use the selected channel's protocol capabilities and model mapping.
func interactiveChannelTestOptions(channel *model.Channel, modelName string) channelTestModelOptions {
	options := channelTestModelOptions{Model: modelName}
	if !supportsChannelTest(channel.Type) {
		return options
	}
	mappedModel, err := service.ResolveImageChannelUpstreamModel(*channel, modelName)
	if err != nil {
		return options
	}
	channelSettings := channel.GetOtherSettings()
	if channel.Type == constant.ChannelTypeTabTab {
		if service.TabtabImageCapability(mappedModel, channelSettings.TabtabImageModelProfiles).Generate {
			options.DefaultMode = channelTestModeImage
			options.ImageEndpointTypes = []constant.EndpointType{constant.EndpointTypeImageGeneration}
		}
		return options
	}
	if channelSettings.UpstreamAsync.HasMatch(dto.UpstreamAsyncMediaImage, mappedModel, "generate") {
		return options
	}
	if channel.Type == constant.ChannelTypeNewAPI && channel.GetSetting().TianyueMediaCompatibility &&
		service.TianyueAsyncImageProfile(mappedModel) != nil {
		return options
	}

	endpoints := common.GetEndpointTypesByChannelType(channel.Type, mappedModel)
	advanced := constant.IsAdvancedCustomChannel(channel.Type)
	if advanced {
		if channelSettings.AdvancedCustom == nil {
			return options
		}
		endpoints = channelSettings.AdvancedCustom.SupportedEndpointTypesForModel(mappedModel)
	}
	apiType, _ := common.ChannelType2APIType(channel.Type)
	adaptor := relay.GetAdaptor(apiType)
	if adaptor == nil {
		return options
	}
	imageProvider, supportsImage := adaptor.(relaychannel.ImageCapabilityProvider)
	var capability dto.ImageCapability
	if supportsImage {
		capability = imageProvider.ImageCapability(mappedModel)
	}
	modelLower := strings.ToLower(mappedModel)
	explicitImage := slices.Contains(endpoints, constant.EndpointTypeImageGeneration)
	declaredImage := capability.Generate && capability.DeclaresModel(mappedModel)
	if (explicitImage || !advanced && declaredImage) && capability.Generate {
		options.DefaultMode = channelTestModeImage
		endpoint := constant.EndpointTypeImageGeneration
		if capability.Protocol == "gemini_native" && !strings.HasPrefix(modelLower, "imagen") {
			endpoint = constant.EndpointTypeGemini
		}
		options.ImageEndpointTypes = []constant.EndpointType{endpoint}
		return options
	}
	if explicitImage || strings.Contains(modelLower, "rerank") || strings.Contains(modelLower, "embed") ||
		strings.HasPrefix(modelLower, "m3e") || strings.Contains(modelLower, "bge-") ||
		channel.Type == constant.ChannelTypeMokaAI {
		return options
	}

	for _, endpoint := range endpoints {
		switch endpoint {
		case constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse, constant.EndpointTypeAnthropic, constant.EndpointTypeGemini:
			options.ChatEndpointTypes = append(options.ChatEndpointTypes, endpoint)
		}
	}
	switch channel.Type {
	case constant.ChannelTypeOpenAI, constant.ChannelTypeCustom, constant.ChannelTypeAzure:
		if !slices.Contains(options.ChatEndpointTypes, constant.EndpointTypeOpenAIResponse) {
			options.ChatEndpointTypes = append(options.ChatEndpointTypes, constant.EndpointTypeOpenAIResponse)
		}
	}
	if strings.Contains(modelLower, "codex") || common.IsOpenAIResponseOnlyModel(modelLower) {
		index := slices.Index(options.ChatEndpointTypes, constant.EndpointTypeOpenAIResponse)
		if index > 0 {
			copy(options.ChatEndpointTypes[1:index+1], options.ChatEndpointTypes[:index])
			options.ChatEndpointTypes[0] = constant.EndpointTypeOpenAIResponse
		}
	}
	if len(options.ChatEndpointTypes) > 0 {
		options.DefaultMode = channelTestModeChat
	}
	// Proprietary model aliases may need a manual image protocol selection.
	if !advanced && !common.IsOpenAITextModel(mappedModel) {
		switch {
		case capability.Protocol == "gemini_native" && !strings.HasPrefix(modelLower, "imagen"):
			options.ImageEndpointTypes = []constant.EndpointType{constant.EndpointTypeGemini}
		case capability.OpenAICompatibleRequest && capability.Generate:
			options.ImageEndpointTypes = []constant.EndpointType{constant.EndpointTypeImageGeneration}
		}
		if options.DefaultMode == "" && len(options.ImageEndpointTypes) > 0 {
			options.DefaultMode = channelTestModeImage
		}
	}
	return options
}

func GetChannelTestOptions(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "Invalid channel ID")
		return
	}
	channel, err := model.GetChannelById(id, false)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	configuredModels := channel.GetModels()
	models := make([]channelTestModelOptions, 0, len(configuredModels))
	seen := make(map[string]struct{}, len(configuredModels))
	for _, name := range configuredModels {
		name = strings.TrimSpace(name)
		if _, exists := seen[name]; exists || name == "" {
			continue
		}
		seen[name] = struct{}{}
		models = append(models, interactiveChannelTestOptions(channel, name))
	}
	common.ApiSuccess(c, gin.H{"models": models})
}

func RunInteractiveChannelTest(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var input channelTestInput
	contentType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || contentType != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"success": false, "message": "Content-Type must be application/json"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10))
	if err != nil || common.Unmarshal(body, &input) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid channel test request"})
		return
	}
	input.Model = strings.TrimSpace(input.Model)
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Model == "" || (input.Mode != channelTestModeChat && input.Mode != channelTestModeImage) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Select a model and a supported test mode"})
		return
	}
	if utf8.RuneCountInString(input.Prompt) > channelTestPromptLimit {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Test prompt exceeds 4096 characters"})
		return
	}
	if len(input.ImageURLs) > 0 {
		references := input.ImageURLs[:0]
		for index, raw := range input.ImageURLs {
			reference := strings.TrimSpace(raw)
			if reference == "" {
				continue
			}
			if _, err := service.ValidateImageReferenceURL(reference); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": fmt.Sprintf("Reference image %d: %s", index+1, err)})
				return
			}
			references = append(references, reference)
		}
		input.ImageURLs = references
		if len(references) > 0 && input.Mode != channelTestModeImage {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Reference image URLs are only supported in image generation tests"})
			return
		}
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid channel ID"})
		return
	}
	channel, err := model.CacheGetChannel(id)
	if err != nil {
		channel, err = model.GetChannelById(id, true)
		if err != nil {
			common.ApiError(c, err)
			return
		}
	}
	if !slices.ContainsFunc(channel.GetModels(), func(name string) bool { return strings.TrimSpace(name) == input.Model }) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Model is not configured on this channel"})
		return
	}
	if len(input.ImageURLs) > 0 {
		limit := min(service.DefaultImageRuntimeConfig().MaxReferences, channel.GetImageMaxReferenceImages())
		if len(input.ImageURLs) > limit {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": fmt.Sprintf("The selected channel accepts up to %d reference image URLs", limit)})
			return
		}
	}
	options := interactiveChannelTestOptions(channel, input.Model)
	endpoints := options.ChatEndpointTypes
	timeout := 90 * time.Second
	if input.Mode == channelTestModeImage {
		endpoints = options.ImageEndpointTypes
		timeout = 180 * time.Second
		input.Stream = false
	}
	if len(endpoints) == 0 || (input.EndpointType != "" && !slices.Contains(endpoints, input.EndpointType)) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "The selected channel model does not support this test mode or endpoint"})
		return
	}
	if input.EndpointType == "" {
		input.EndpointType = endpoints[0]
	}
	if input.Prompt == "" {
		input.Prompt = "hi"
		if input.Mode == channelTestModeImage {
			input.Prompt = "a cute cat"
		}
	}
	if channel.Type == constant.ChannelTypeCodex {
		input.Stream = true
	}
	testUserID, err := resolveChannelTestUserID(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	input.requestID = c.GetString(common.RequestIdKey)
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	start := time.Now()
	result := testChannel(ctx, channel, testUserID, input.Model, string(input.EndpointType), input.Stream, &input)
	elapsed := time.Since(start)
	if result.localErr != nil {
		message := sanitizeInteractiveChannelTestText(result.context, result.localErr.Error(), common.LocalLogContentLimit)
		code := result.newAPIError.GetErrorCode()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && code != channelTestImagePendingCode &&
			code != channelTestImageAcceptanceUnknownCode && code != channelTestImagePreviewFailedCode {
			message = "Channel test timed out"
		}
		response := gin.H{"success": false, "message": message, "time": elapsed.Seconds()}
		if result.newAPIError != nil {
			response["error_code"] = interactiveChannelTestErrorCode(code)
		}
		c.JSON(http.StatusOK, response)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "time": elapsed.Seconds(), "data": result.output})
}

func interactiveChannelTestErrorCode(code types.ErrorCode) types.ErrorCode {
	switch code {
	case types.ErrorCodeInvalidRequest, types.ErrorCodeInvalidApiType, types.ErrorCodeJsonMarshalFailed,
		types.ErrorCodeDoRequestFailed, types.ErrorCodeGetChannelFailed, types.ErrorCodeGenRelayInfoFailed,
		types.ErrorCodeChannelNoAvailableKey, types.ErrorCodeChannelParamOverrideInvalid, types.ErrorCodeChannelHeaderOverrideInvalid,
		types.ErrorCodeChannelModelMappedError, types.ErrorCodeChannelInvalidKey, types.ErrorCodeConvertRequestFailed,
		types.ErrorCodeReadResponseBodyFailed, types.ErrorCodeBadResponseStatusCode, types.ErrorCodeBadResponseBody,
		types.ErrorCodeEmptyResponse, types.ErrorCodeModelNotFound, types.ErrorCodePromptBlocked,
		channelTestImagePendingCode, channelTestImageAcceptanceUnknownCode, channelTestImageFailedCode, channelTestImagePreviewFailedCode:
		return code
	default:
		return types.ErrorCodeBadResponse
	}
}

func readInteractiveChannelTestError(c *gin.Context, response *http.Response) *types.NewAPIError {
	body := http.MaxBytesReader(nil, response.Body, 64<<10)
	defer body.Close()
	payload, err := io.ReadAll(body)
	message := fmt.Sprintf("Upstream returned HTTP %d", response.StatusCode)
	var upstream dto.GeneralErrorResponse
	if err == nil && common.Unmarshal(payload, &upstream) == nil {
		if detail := sanitizeInteractiveChannelTestText(c, upstream.ToMessage(), common.LocalLogContentLimit); detail != "" {
			message += ": " + detail
		}
	}
	return types.NewErrorWithStatusCode(errors.New(message), types.ErrorCodeBadResponseStatusCode, response.StatusCode)
}

func applyInteractiveChannelTestPrompt(request dto.Request, input *channelTestInput) error {
	const maxOutputTokens = uint(1024)
	switch request := request.(type) {
	case *dto.GeneralOpenAIRequest:
		request.Messages[0].SetStringContent(input.Prompt)
		request.MaxTokens = lo.ToPtr(maxOutputTokens)
	case *dto.OpenAIResponsesRequest:
		var err error
		request.Input, err = common.Marshal([]dto.Message{{Role: "user", Content: input.Prompt}})
		request.MaxOutputTokens = lo.ToPtr(maxOutputTokens)
		return err
	case *dto.ClaudeRequest:
		request.Messages[0].Content = input.Prompt
		request.MaxTokens = lo.ToPtr(maxOutputTokens)
	case *dto.GeminiChatRequest:
		request.Contents[0].Parts[0].Text = input.Prompt
		if input.Mode == channelTestModeImage {
			request.GenerationConfig = dto.GeminiChatGenerationConfig{ResponseModalities: []string{"TEXT", "IMAGE"}}
		}
	case *dto.ImageRequest:
		request.Prompt = input.Prompt
	default:
		return errors.New("Unsupported channel test request")
	}
	return nil
}

func prepareInteractiveChannelTestReferences(c *gin.Context, info *relaycommon.RelayInfo, adaptor relaychannel.Adaptor, input *channelTestInput) error {
	provider, ok := adaptor.(relaychannel.ImageCapabilityProvider)
	if !ok {
		return errors.New("The selected channel model does not support reference images")
	}
	capability := provider.ImageCapability(info.UpstreamModelName)
	if info.ChannelType == constant.ChannelTypeTabTab {
		capability = service.TabtabImageCapability(info.UpstreamModelName, info.ChannelOtherSettings.TabtabImageModelProfiles)
	}
	if !capability.Edit && capability.ReferenceField == "" {
		return errors.New("The selected channel model does not support reference images")
	}

	cfg := service.DefaultImageRuntimeConfig()
	remainingBytes := cfg.DownloadMaxBytes
	if limit := info.ChannelSetting.ImageMaxReferenceBytes; limit != nil && *limit > 0 {
		cfg.DownloadMaxBytes = min(cfg.DownloadMaxBytes, *limit)
	}
	switch request := info.Request.(type) {
	case *dto.GeminiChatRequest:
		for index, reference := range input.ImageURLs {
			parsed, _ := service.ValidateImageReferenceURL(reference)
			mimeType := mime.TypeByExtension(strings.ToLower(path.Ext(parsed.Path)))
			if slices.Contains([]string{"image/jpeg", "image/png", "image/webp", "image/gif"}, mimeType) && info.ChannelSetting.ImageReferenceTransportMode != "local" {
				request.Contents[0].Parts = append(request.Contents[0].Parts, dto.GeminiPart{FileData: &dto.GeminiFileData{MimeType: mimeType, FileUri: reference}})
				continue
			}
			cfg.DownloadMaxBytes = min(cfg.DownloadMaxBytes, remainingBytes)
			image, err := service.DownloadImageReference(c.Request.Context(), reference, cfg)
			if err != nil {
				return service.NewImageReferenceFailure(index+1, err)
			}
			remainingBytes -= int64(len(image.Data))
			request.Contents[0].Parts = append(request.Contents[0].Parts, dto.GeminiPart{InlineData: &dto.GeminiInlineData{MimeType: image.ContentType, Data: base64.StdEncoding.EncodeToString(image.Data)}})
		}
	case *dto.ImageRequest:
		references := input.ImageURLs
		if info.ChannelSetting.ImageReferenceTransportMode == "local" && capability.EditFormat != "multipart" {
			references = make([]string, 0, len(input.ImageURLs))
			for index, reference := range input.ImageURLs {
				cfg.DownloadMaxBytes = min(cfg.DownloadMaxBytes, remainingBytes)
				image, err := service.DownloadImageReference(c.Request.Context(), reference, cfg)
				if err != nil {
					return service.NewImageReferenceFailure(index+1, err)
				}
				remainingBytes -= int64(len(image.Data))
				references = append(references, image.DataURL())
			}
		}
		switch capability.ReferenceField {
		case "image":
			var err error
			request.Image, err = common.Marshal(references)
			return err
		case "images":
			var err error
			request.Images, err = common.Marshal(references)
			return err
		}
		info.RelayMode = relayconstant.RelayModeImagesEdits
		info.RequestURLPath = "/v1/images/edits"
		c.Request.URL.Path = info.RequestURLPath
		if info.ChannelOtherSettings.UpstreamAsync.HasMatch(dto.UpstreamAsyncMediaImage, info.UpstreamModelName, dto.UpstreamAsyncOperationEdit) {
			return errors.New("The selected channel model requires an asynchronous image editing endpoint")
		}
		if capability.EditFormat == "multipart" {
			if info.ChannelType == constant.ChannelTypeReplicate && len(input.ImageURLs) != 1 {
				return errors.New("The selected channel model accepts one reference image")
			}
			return prepareInteractiveChannelTestImageForm(c, request, references, cfg, remainingBytes)
		}
		images := make([]map[string]string, 0, len(references))
		for _, reference := range references {
			images = append(images, map[string]string{"image_url": reference})
		}
		var err error
		request.Images, err = common.Marshal(images)
		return err
	default:
		return errors.New("The selected channel model does not support reference images")
	}
	return nil
}

func prepareInteractiveChannelTestImageForm(c *gin.Context, request *dto.ImageRequest, references []string, cfg service.ImageRuntimeConfig, remainingBytes int64) error {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for field, value := range map[string]string{"model": request.Model, "prompt": request.Prompt, "size": request.Size, "n": "1"} {
		if err := form.WriteField(field, value); err != nil {
			return err
		}
	}
	for index, reference := range references {
		cfg.DownloadMaxBytes = min(cfg.DownloadMaxBytes, remainingBytes)
		image, err := service.DownloadImageReference(c.Request.Context(), reference, cfg)
		if err != nil {
			return service.NewImageReferenceFailure(index+1, err)
		}
		remainingBytes -= int64(len(image.Data))
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
			"name": "image[]", "filename": fmt.Sprintf("reference-%d.%s", index+1, strings.TrimPrefix(image.ContentType, "image/")),
		}))
		header.Set("Content-Type", image.ContentType)
		part, err := form.CreatePart(header)
		if err != nil {
			return err
		}
		if _, err := part.Write(image.Data); err != nil {
			return err
		}
	}
	if err := form.Close(); err != nil {
		return err
	}
	data := body.Bytes()
	c.Request.Body = io.NopCloser(bytes.NewReader(data))
	c.Request.ContentLength = int64(len(data))
	c.Request.Header.Set("Content-Type", form.FormDataContentType())
	_, err := c.MultipartForm()
	c.Request.Body = io.NopCloser(bytes.NewReader(data))
	return err
}

func validateInteractiveChannelTestReferences(before, after []byte, format types.RelayFormat, channelType int) error {
	if format == types.RelayFormatOpenAIImage && constant.IsAdvancedCustomChannel(channelType) &&
		!gjson.GetBytes(before, "image").Exists() && !gjson.GetBytes(after, "images").Exists() {
		// Custom routes can translate the OpenAI edit body to a provider's URL list.
		original := gjson.GetBytes(before, "images.#.image_url").Array()
		native := gjson.GetBytes(after, "image")
		if len(original) > 0 && native.IsArray() && slices.EqualFunc(original, native.Array(), func(a, b gjson.Result) bool {
			return a.Type == gjson.String && b.Type == gjson.String && a.Str == b.Str
		}) {
			return nil
		}
	}
	fields := []string{"image", "images"}
	switch {
	case format == types.RelayFormatGemini:
		fields = []string{"contents.#.parts.#.fileData", "contents.#.parts.#.inlineData"}
	case channelType == constant.ChannelTypeAli:
		fields = []string{"input.images", "input.messages.#.content.#.image"}
	case channelType == constant.ChannelTypeReplicate:
		fields = []string{"input.image_prompt"}
	}
	for _, field := range fields {
		original := gjson.GetBytes(before, field)
		if original.Exists() && original.Raw != gjson.GetBytes(after, field).Raw {
			return errors.New("Channel parameter overrides cannot replace or remove the test reference images")
		}
	}
	return nil
}

func validateInteractiveChannelImageRequest(body []byte, format types.RelayFormat) error {
	if format == types.RelayFormatGemini {
		if count := gjson.GetBytes(body, "generationConfig.candidateCount"); count.Exists() && count.Raw != "1" {
			return errors.New("Gemini image tests require one candidate")
		}
		modalities := gjson.GetBytes(body, "generationConfig.responseModalities").Array()
		if !slices.ContainsFunc(modalities, func(mode gjson.Result) bool { return mode.String() == "IMAGE" }) {
			return errors.New("Gemini image tests require the IMAGE response modality")
		}
	}
	probe := &http.Request{
		GetBody: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil },
	}
	return service.ValidateMixedImageSubmitCount(probe, 1)
}

func sanitizeInteractiveChannelTestText(c *gin.Context, text string, limit int) string {
	if c != nil {
		headerValues := common.GetContextKeyStringSlice(c, constant.ContextKeyChannelTestCredentials)
		secrets := make([]string, 0, 2*len(headerValues)+1)
		for _, value := range headerValues {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			secrets = append(secrets, value)
			if separator := strings.IndexAny(value, " \t;"); separator >= 0 {
				switch strings.ToLower(value[:separator]) {
				case "bearer", "basic", "token":
					if token := strings.TrimSpace(value[separator+1:]); token != "" {
						secrets = append(secrets, token)
					}
				}
			}
		}
		if key := common.GetContextKeyString(c, constant.ContextKeyChannelKey); key != "" {
			secrets = append(secrets, key)
		}
		slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
		for _, secret := range secrets {
			text = strings.ReplaceAll(text, secret, "***")
		}
	}
	return service.SanitizeAsyncImageText(text, limit)
}

func readInteractiveChannelTestOutput(c *gin.Context, body []byte, input *channelTestInput, stream bool) (*channelTestOutput, error) {
	if err := c.Request.Context().Err(); err != nil {
		return nil, err
	}
	output := &channelTestOutput{Model: input.Model, Mode: input.Mode, EndpointType: input.EndpointType}
	if input.Mode == channelTestModeImage {
		platform := "openai"
		if input.EndpointType == constant.EndpointTypeGemini {
			platform = "gemini"
		}
		image, err := relay.ExtractSingleImageOutput(c.Request.Context(), body, platform, service.DefaultImageRuntimeConfig())
		if err != nil {
			return nil, err
		}
		output.Image, err = channelTestImagePreview(image)
		if err != nil {
			return nil, err
		}
		return output, nil
	}
	var answer strings.Builder
	if stream {
		responsesCompleted := false
		for line := range bytes.SplitSeq(body, []byte{'\n'}) {
			payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
			if !ok {
				continue
			}
			event := gjson.ParseBytes(bytes.TrimSpace(payload))
			status, isResponsesEvent := strings.CutPrefix(event.Get("type").String(), "response.")
			if input.EndpointType == constant.EndpointTypeOpenAIResponse && isResponsesEvent {
				if status == "completed" || status == "done" {
					responsesCompleted = true
					status = event.Get("response.status").String()
				}
				switch status {
				case "failed", "incomplete", "cancelled", "canceled":
					return nil, fmt.Errorf("Upstream Responses request ended with status %s", status)
				}
			}
			appendChannelTestAnswer(&answer, event, input.EndpointType)
		}
		if input.EndpointType == constant.EndpointTypeOpenAIResponse && !responsesCompleted {
			return nil, errors.New("Upstream Responses stream ended without a completion event")
		}
	} else {
		appendChannelTestAnswer(&answer, gjson.ParseBytes(body), input.EndpointType)
	}
	output.Text = sanitizeInteractiveChannelTestText(c, answer.String(), 64<<10)
	if output.Text == "" {
		return nil, errors.New("Upstream returned no assistant text")
	}
	return output, nil
}

func channelTestImagePreview(image service.ImageBytes) (*channelTestImage, error) {
	if image.ContentType == "image/svg+xml" {
		return nil, errors.New("The upstream returned SVG. The test preview requires a raster image")
	}
	return &channelTestImage{DataURL: image.DataURL(), Width: image.Width, Height: image.Height}, nil
}

func appendChannelTestAnswer(answer *strings.Builder, response gjson.Result, endpoint constant.EndpointType) {
	switch endpoint {
	case constant.EndpointTypeOpenAIResponse:
		switch response.Get("type").String() {
		case "response.output_text.delta", "response.refusal.delta":
			answer.WriteString(response.Get("delta").String())
			return
		case "response.completed", "response.done":
			if answer.Len() > 0 {
				return
			}
			response = response.Get("response")
		}
		for _, item := range response.Get("output").Array() {
			if item.Get("type").String() != "message" || item.Get("role").String() != "assistant" {
				continue
			}
			for _, part := range item.Get("content").Array() {
				switch part.Get("type").String() {
				case "output_text":
					answer.WriteString(part.Get("text").String())
				case "refusal":
					answer.WriteString(part.Get("refusal").String())
				}
			}
		}
	case constant.EndpointTypeAnthropic:
		if response.Get("delta.type").String() == "text_delta" {
			answer.WriteString(response.Get("delta.text").String())
		}
		if response.Get("content_block.type").String() == "text" {
			answer.WriteString(response.Get("content_block.text").String())
		}
		for _, part := range response.Get("content").Array() {
			if part.Get("type").String() == "text" {
				answer.WriteString(part.Get("text").String())
			}
		}
	case constant.EndpointTypeGemini:
		for _, part := range response.Get("candidates.0.content.parts").Array() {
			if !part.Get("thought").Bool() {
				answer.WriteString(part.Get("text").String())
			}
		}
	default:
		message := response.Get("choices.0.delta")
		if !message.Exists() {
			message = response.Get("choices.0.message")
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			answer.WriteString(content.String())
		} else {
			for _, part := range content.Array() {
				if part.Get("type").String() == "text" {
					answer.WriteString(part.Get("text").String())
				}
			}
		}
		answer.WriteString(message.Get("refusal").String())
	}
}
