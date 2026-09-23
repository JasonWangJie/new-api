package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	gatewaydto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// PrepareAsyncVideo uses the existing /videos protocol pipeline after removing
// the gateway-only provider field. Multipart parts and headers stay intact.
func PrepareAsyncVideo(c *gin.Context) {
	prepareAsyncVideo(c, "create")
}

func PrepareAsyncVideoEdit(c *gin.Context) {
	prepareAsyncVideo(c, "edit")
}

func PrepareAsyncVideoExtension(c *gin.Context) {
	prepareAsyncVideo(c, "extend")
}

func asyncVideoInternalPath(operation string) (string, bool) {
	switch operation {
	case "", "create":
		return "/v1/videos", true
	case "edit":
		return "/v1/videos/edits", true
	case "extend":
		return "/v1/videos/extensions", true
	default:
		return "", false
	}
}

func prepareAsyncVideo(c *gin.Context, operation string) {
	defer common.CleanupBodyStorage(c)
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<20))
	if err != nil {
		AsyncImagePublicError(c, 413, "request_too_large", "Video request exceeds the byte limit")
		c.Abort()
		return
	}
	contentType := c.GetHeader("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_request", "Invalid content type")
		c.Abort()
		return
	}
	provider := ""
	body := raw
	hashBody := raw
	switch mediaType {
	case "application/json":
		var fields map[string]common.RawMessage
		if err := common.Unmarshal(raw, &fields); err != nil {
			AsyncImagePublicError(c, 400, "invalid_request", "Invalid JSON request")
			c.Abort()
			return
		}
		if value, present := fields["provider"]; present {
			if err := common.Unmarshal(value, &provider); err != nil {
				AsyncImagePublicError(c, 400, "invalid_request", "Invalid provider")
				c.Abort()
				return
			}
			delete(fields, "provider")
			body, err = common.Marshal(fields)
		}
	case "multipart/form-data":
		if operation != "create" {
			err = errors.New("video editing and extension require JSON")
			break
		}
		reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		var encoded bytes.Buffer
		writer := multipart.NewWriter(&encoded)
		var canonical bytes.Buffer
		found := false
		for {
			part, readErr := reader.NextPart()
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				err = readErr
				break
			}
			partHeaders, headerErr := common.Marshal(part.Header)
			if headerErr != nil {
				err = headerErr
				break
			}
			canonical.Write(partHeaders)
			canonical.WriteByte(0)
			if part.FormName() == "provider" {
				if found || part.FileName() != "" {
					err = errors.New("duplicate provider field")
					break
				}
				value, readErr := io.ReadAll(io.LimitReader(part, 129))
				if readErr != nil || len(value) > 128 {
					err = errors.New("invalid provider")
					break
				}
				provider, found = string(value), true
				digest := sha256.Sum256(value)
				canonical.WriteString(hex.EncodeToString(digest[:]))
				canonical.WriteByte(0)
				continue
			}
			output, writeErr := writer.CreatePart(part.Header)
			if writeErr != nil {
				err = writeErr
				break
			}
			digest := sha256.New()
			if _, err = io.Copy(output, io.TeeReader(part, digest)); err != nil {
				break
			}
			canonical.WriteString(hex.EncodeToString(digest.Sum(nil)))
			canonical.WriteByte(0)
		}
		if closeErr := writer.Close(); err == nil {
			err = closeErr
		}
		body, contentType = encoded.Bytes(), writer.FormDataContentType()
		hashBody = canonical.Bytes()
	default:
		err = errors.New("video request requires JSON or multipart")
	}
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_request", err.Error())
		c.Abort()
		return
	}
	c.Set("async_media_provider", provider)
	c.Set("async_media_operation", operation)
	c.Set("async_media_request_hash", service.AsyncImageRequestHash("video", "openai_video", c.Request.URL.Path, hashBody))
	key, err := AsyncImageIdempotencyKey(c)
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_idempotency_key", err.Error())
		c.Abort()
		return
	}
	if key != "" {
		var existing model.AsyncMediaJob
		identity := service.ImageIdentityHash("async-video-idempotency", strconv.Itoa(c.GetInt("token_id")), key)
		lookupErr := model.DB.WithContext(c.Request.Context()).Where("identity_hash = ? AND user_id = ?", identity, c.GetInt("id")).Take(&existing).Error
		if lookupErr == nil {
			if existing.RequestHash != c.GetString("async_media_request_hash") {
				AsyncImagePublicError(c, 409, "idempotency_conflict", "Idempotency-Key was used for a different video request")
			} else {
				respondAsyncVideoAccepted(c, existing, true)
				c.Set("async_public_success", true)
			}
			c.Abort()
			return
		}
		if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			AsyncImagePublicError(c, 503, "database_unavailable", "Video admission is unavailable")
			c.Abort()
			return
		}
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	c.Request.Header.Set("Content-Type", contentType)
	internalPath, ok := asyncVideoInternalPath(operation)
	if !ok {
		AsyncImagePublicError(c, 400, "invalid_request", "Invalid video operation")
		c.Abort()
		return
	}
	c.Request.URL.Path = internalPath
	c.Next()
}

func FilterAsyncVideoProvider(c *gin.Context) {
	value, _ := c.Get(pluginruntime.ContextKeyPinnedEndpoint)
	pinned, ok := value.(pluginruntime.PinnedEndpoint)
	if !ok || pinned.Plugin == nil || pinned.Protocol != "openai_video" {
		AsyncImagePublicError(c, 400, "unsupported_model", "Model has no capability for this video operation")
		c.Abort()
		return
	}
	if provider := c.GetString("async_media_provider"); provider != "" {
		candidates := slices.DeleteFunc(slices.Clone(pinned.Candidates), func(binding pluginruntime.ProtocolBinding) bool {
			return binding.Plugin == nil || binding.Plugin.Meta.Key != provider
		})
		if len(candidates) == 0 {
			AsyncImagePublicError(c, 400, "unsupported_provider", "Provider does not support this video model")
			c.Abort()
			return
		}
		pinned.Plugin, pinned.Operation, pinned.Candidates = candidates[0].Plugin, candidates[0].Operation, candidates
		c.Set(pluginruntime.ContextKeyPinnedEndpoint, pinned)
		c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Generation: pinned.Generation, Plugin: pinned.Plugin})
	}
	c.Next()
}

func asyncVideoCapabilityRequest(c *gin.Context) (map[string]any, []map[string]any, string, string, error) {
	requestValue, exists := c.Get("task_request")
	if !exists {
		return nil, nil, "", "", errors.New("video request is unavailable")
	}
	requestData, err := common.Marshal(requestValue)
	if err != nil {
		return nil, nil, "", "", err
	}
	request := make(map[string]any)
	if err := common.Unmarshal(requestData, &request); err != nil {
		return nil, nil, "", "", err
	}
	var parameters struct {
		Size       string `json:"size"`
		Resolution string `json:"resolution"`
		Metadata   struct {
			Size       string `json:"size"`
			Resolution string `json:"resolution"`
		} `json:"metadata"`
	}
	if err := common.Unmarshal(requestData, &parameters); err != nil {
		return nil, nil, "", "", err
	}
	resolution := parameters.Resolution
	if resolution == "" {
		resolution = parameters.Metadata.Resolution
	}
	if resolution == "" {
		resolution = parameters.Size
	}
	if resolution == "" {
		resolution = parameters.Metadata.Size
	}
	var files []map[string]any
	if value, ok := c.Get(pluginruntime.ContextKeyRouteRequest); ok {
		if routeRequest, ok := value.(pluginruntime.RouteRequestContext); ok {
			files = routeRequest.Files
		}
	}
	action := c.GetString("task_action")
	if action == "" {
		action = "text_to_video"
		if request["image"] != nil || request["input_reference"] != nil || len(files) > 0 {
			action = "image_to_video"
		}
	}
	return request, files, action, resolution, nil
}

// RouteAsyncVideo applies provider policy before freezing a channel. A provider
// can inherit the Token group or override it through the existing mapping.
func RouteAsyncVideo(c *gin.Context) {
	value, _ := c.Get(pluginruntime.ContextKeyPinnedEndpoint)
	pinned, ok := value.(pluginruntime.PinnedEndpoint)
	if !ok {
		c.AbortWithStatus(400)
		return
	}
	var token model.Token
	if err := model.DB.WithContext(c.Request.Context()).Where("id = ? AND user_id = ?", c.GetInt("token_id"), c.GetInt("id")).Take(&token).Error; err != nil {
		c.AbortWithStatus(503)
		return
	}
	var owner model.User
	if err := model.DB.WithContext(c.Request.Context()).Where("id = ?", token.UserId).Take(&owner).Error; err != nil {
		c.AbortWithStatus(503)
		return
	}
	type choice struct {
		channel model.Channel
		group   string
		binding pluginruntime.ProtocolBinding
	}
	choices := make([]choice, 0)
	videoRequest, videoFiles, videoAction, videoResolution, err := asyncVideoCapabilityRequest(c)
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_request", err.Error())
		c.Abort()
		return
	}
	resolution := service.VideoPoolResolution(videoResolution)
	constraints := service.GetChannelConstraints(c)
	forced, hasForced, _ := constraints.ResolvedPin()
	var capabilityErr error
	for _, binding := range pinned.Candidates {
		if binding.Plugin == nil || binding.Protocol != "openai_video" {
			continue
		}
		provider := binding.Plugin.Meta.Key
		policy, catalog, err := service.ResolveImagePolicy(c.Request.Context(), token, provider)
		if err != nil {
			c.AbortWithStatus(503)
			return
		}
		capability, selected := service.VideoPolicyCapability(catalog, pinned.Model)
		if policy.Id != 0 && (!policy.Enabled || !policy.AsyncEnabled || !selected) {
			continue
		}
		profile, profileErr := service.EffectiveVideoProfile(binding, pinned.Model, capability.VideoOverrides)
		if profileErr != nil {
			capabilityErr = profileErr
			continue
		}
		if profileErr = service.ValidateVideoProfileRequest(profile, videoAction, videoRequest, videoFiles); profileErr != nil {
			capabilityErr = profileErr
			continue
		}
		groups := []string{policy.Group}
		if policy.Group == "auto" {
			groups = service.GetRequestAutoGroups(c, owner.Group)
		}
		for _, group := range groups {
			if !service.GroupInUserUsableGroups(owner.Group, group) {
				continue
			}
			var abilities []model.Ability
			if err := model.DB.WithContext(c.Request.Context()).Where(map[string]any{"group": group, "enabled": true}).Where("model IN ?", []string{pinned.Model, ratio_setting.RoutingMatchModelName(pinned.Model)}).Find(&abilities).Error; err != nil {
				c.AbortWithStatus(503)
				return
			}
			ids := make([]int, 0, len(abilities))
			for _, ability := range abilities {
				ids = append(ids, ability.ChannelId)
			}
			if len(ids) == 0 {
				continue
			}
			var channels []model.Channel
			if err := model.DB.WithContext(c.Request.Context()).Where("id IN ? AND status = ?", ids, common.ChannelStatusEnabled).Find(&channels).Error; err != nil {
				c.AbortWithStatus(503)
				return
			}
			poolPolicy := policy
			poolPolicy.Group = group
			var pools []model.ImageChannelPool
			if err := model.DB.WithContext(c.Request.Context()).Where("binding_key = ?", service.ImagePoolBindingKey(poolPolicy, pinned.Model, resolution)).Find(&pools).Error; err != nil {
				c.AbortWithStatus(503)
				return
			}
			for _, channel := range channels {
				if hasForced && channel.Id != forced.ChannelId {
					continue
				}
				filter := gatewaydto.ChannelFilter{Kind: gatewaydto.FilterTaskPluginIdentity, TaskPluginKey: provider, TaskPluginChannelTypes: binding.Plugin.Meta.ChannelTypes}
				if matches, _ := model.ChannelSatisfiesFilters(&channel, pinned.Model, append(slices.Clone(constraints.Filters), filter)); !matches {
					continue
				}
				if len(pools) > 0 {
					index := slices.IndexFunc(pools, func(pool model.ImageChannelPool) bool { return pool.ChannelId == channel.Id })
					if index < 0 {
						continue
					}
					priority := int64(pools[index].Priority)
					channel.Priority = &priority
				}
				choices = append(choices, choice{channel: channel, group: group, binding: binding})
			}
		}
	}
	if len(choices) == 0 {
		if capabilityErr != nil {
			AsyncImagePublicError(c, 400, "invalid_request", capabilityErr.Error())
			c.Abort()
			return
		}
		AsyncImagePublicError(c, 403, "provider_unavailable", "No authorized video channel is available")
		c.Abort()
		return
	}
	priority := choices[0].channel.GetPriority()
	for _, choice := range choices {
		priority = max(priority, choice.channel.GetPriority())
	}
	choices = slices.DeleteFunc(choices, func(choice choice) bool { return choice.channel.GetPriority() != priority })
	var weight float64
	for _, choice := range choices {
		weight += float64(max(1, min(choice.channel.GetWeight(), 1_000_000_000)))
	}
	pick := rand.Float64() * weight
	selected := choices[len(choices)-1]
	for _, choice := range choices {
		pick -= float64(max(1, min(choice.channel.GetWeight(), 1_000_000_000)))
		if pick < 0 {
			selected = choice
			break
		}
	}
	pinned.Candidates, pinned.Plugin, pinned.Protocol, pinned.Operation = []pluginruntime.ProtocolBinding{selected.binding}, selected.binding.Plugin, selected.binding.Protocol, selected.binding.Operation
	c.Set(pluginruntime.ContextKeyPinnedEndpoint, pinned)
	c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Generation: pinned.Generation, Plugin: pinned.Plugin})
	c.Set("expected_task_plugin_key", selected.binding.Plugin.Meta.Key)
	c.Set("task_plugin_key", selected.binding.Plugin.Meta.Key)
	c.Set("platform", selected.binding.Plugin.Meta.Key)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, selected.group)
	common.SetContextKey(c, constant.ContextKeyAutoGroup, selected.group)
	constraints.AddPin(gatewaydto.ChannelPin{ChannelId: selected.channel.Id, Source: "media_policy", Rank: 20, RetryMode: gatewaydto.PinRetrySingleAttempt})
	c.Next()
}

func AcceptAsyncVideo(c *gin.Context) {
	ctx := c.Request.Context()
	cfg, err := service.GetMediaRuntimeConfig(ctx)
	if err != nil || !cfg.VideoAsyncEnabled || !service.ImageEncryptionAvailable() || common.CryptoSecret == "" || common.RDB == nil || !common.RedisEnabled {
		AsyncImagePublicError(c, 503, "async_video_unavailable", "Async video service is disabled or unavailable")
		return
	}
	if err := common.RDB.Ping(ctx).Err(); err != nil {
		AsyncImagePublicError(c, 503, "async_video_unavailable", "Media queue is unavailable")
		return
	}
	key, err := AsyncImageIdempotencyKey(c)
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_idempotency_key", err.Error())
		return
	}
	job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), TokenId: c.GetInt("token_id"), UserId: c.GetInt("id"), ChannelId: common.GetContextKeyInt(c, constant.ContextKeyChannelId), RequestHash: c.GetString("async_media_request_hash"), Model: c.GetString("original_model"), Group: common.GetContextKeyString(c, constant.ContextKeyUsingGroup), ExpiresAt: time.Now().Unix() + int64(cfg.RetentionDays)*86400}
	job.IdentityHash = service.ImageIdentityHash("async-video-idempotency", strconv.Itoa(job.TokenId), key)
	if key == "" {
		job.IdentityHash = service.ImageIdentityHash("async-video-task", job.TaskId)
	}
	if key != "" {
		var existing model.AsyncMediaJob
		lookupErr := model.DB.WithContext(ctx).Where("identity_hash = ? AND user_id = ?", job.IdentityHash, job.UserId).Take(&existing).Error
		if lookupErr == nil {
			if existing.RequestHash != job.RequestHash {
				AsyncImagePublicError(c, 409, "idempotency_conflict", "Idempotency-Key was used for a different video request")
				return
			}
			respondAsyncVideoAccepted(c, existing, true)
			return
		}
		if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			AsyncImagePublicError(c, 503, "database_unavailable", "Video admission is unavailable")
			return
		}
	}
	selected, err := model.CacheGetChannel(job.ChannelId)
	if err != nil {
		AsyncImagePublicError(c, 503, "channel_unavailable", "Video channel is unavailable")
		return
	}
	channel := *selected
	channel.Key, channel.Keys, channel.ChannelInfo = common.GetContextKeyString(c, constant.ContextKeyChannelKey), nil, model.ChannelInfo{}
	channelData, err := common.Marshal(channel)
	if err == nil {
		job.ChannelCipher, err = service.EncryptImagePayload(channelData, "media-channel:"+job.TaskId)
	}
	if err != nil {
		AsyncImagePublicError(c, 503, "encryption_unavailable", "Video channel could not be frozen")
		return
	}
	execution := service.TaskExecutionSnapshotFromContext(c)
	if execution == nil || execution.TaskPlugin == nil {
		AsyncImagePublicError(c, 400, "unsupported_provider", "Video provider must support the task plugin protocol")
		return
	}
	job.Provider = execution.TaskPlugin.Key
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_request", err.Error())
		return
	}
	info.TaskRelayInfo = &relaycommon.TaskRelayInfo{PublicTaskID: job.TaskId}
	c.Set("async_media_prepare", true)
	_, taskErr := relay.RelayTaskSubmit(c, info)
	if taskErr != nil {
		AsyncImagePublicError(c, taskErr.StatusCode, "invalid_request", taskErr.Error.Error())
		return
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_request", "Video body is unavailable")
		return
	}
	body, err := storage.Bytes()
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_request", "Video body is unavailable")
		return
	}
	request := service.AsyncVideoRequest{Body: body, ContentType: c.GetHeader("Content-Type"), ClientIP: c.ClientIP(), Operation: c.GetString("async_media_operation"), Channel: channel, Execution: execution, Billing: service.MediaBillingSnapshot{Price: info.PriceData, Ratios: info.PriceData.OtherRatios(), Tiered: info.TieredBillingSnapshot, Clamp: info.QuotaClamp}}
	if value, ok := c.Get(pluginruntime.ContextKeyPinnedPlugin); ok {
		if pinned, ok := value.(pluginruntime.PinnedPlugin); ok && pinned.Plugin != nil {
			request.PluginHash = pinned.Plugin.Engine.SourceFingerprint()
		}
	}
	if request.PluginHash == "" {
		AsyncImagePublicError(c, 503, "provider_unavailable", "Video provider could not be frozen")
		return
	}
	encoded, err := common.Marshal(request)
	if err == nil {
		job.RequestCipher, err = service.EncryptImagePayload(encoded, "media-request:"+job.TaskId)
	}
	if err != nil {
		AsyncImagePublicError(c, 503, "encryption_unavailable", "Video request encryption is unavailable")
		return
	}
	job, reused, err := model.AcceptAsyncMediaJob(ctx, job)
	if err != nil {
		if errors.Is(err, model.ErrImageConflict) {
			AsyncImagePublicError(c, 409, "idempotency_conflict", "Idempotency-Key was used for a different video request")
		} else {
			AsyncImagePublicError(c, 503, "database_unavailable", "Video admission could not be persisted")
		}
		return
	}
	respondAsyncVideoAccepted(c, job, reused)
}

func respondAsyncVideoAccepted(c *gin.Context, job model.AsyncMediaJob, reused bool) {
	queryURL := AsyncImageGatewayBase() + "/v1/media/tasks_async/" + job.TaskId
	c.Header("Cache-Control", "no-store")
	c.Header("Location", queryURL)
	c.Header("Retry-After", "3")
	if reused {
		c.Header("X-Idempotency-Replayed", "true")
	}
	c.JSON(http.StatusAccepted, gin.H{"task_id": job.TaskId, "query_url": queryURL})
}

func ExecuteAsyncVideo(ctx context.Context, job model.AsyncMediaJob, request service.AsyncVideoRequest) error {
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", job.TokenId, job.UserId).Take(&token).Error; err != nil {
		return err
	}
	if err := validateMediaToken(ctx, token, request.ClientIP); err != nil {
		return err
	}
	engine := gin.New()
	_ = engine.SetTrustedProxies(nil)
	var executionErr error
	operation := request.Operation
	path, validOperation := asyncVideoInternalPath(operation)
	if !validOperation {
		return errors.New("encrypted video operation is invalid")
	}
	handler := []gin.HandlerFunc{middleware.BodyStorageCleanup(), middleware.TokenAuth(), middleware.PinTaskPluginEndpoint(), func(c *gin.Context) { c.Set("async_media_provider", job.Provider); FilterAsyncVideoProvider(c) }, middleware.PrepareTaskPluginEndpoint(), func(c *gin.Context) {
		value, _ := c.Get(pluginruntime.ContextKeyPinnedPlugin)
		pinned, ok := value.(pluginruntime.PinnedPlugin)
		endpointValue, _ := c.Get(pluginruntime.ContextKeyPinnedEndpoint)
		endpoint, endpointOK := endpointValue.(pluginruntime.PinnedEndpoint)
		if !ok || !endpointOK || pinned.Plugin == nil || pinned.Plugin.Engine.SourceFingerprint() != request.PluginHash {
			executionErr = errors.New("video provider changed before submission")
			return
		}
		current := service.TaskExecutionSnapshotFromContext(c)
		if current == nil || current.TaskPlugin == nil || request.Execution == nil || request.Execution.TaskPlugin == nil || current.TaskPlugin.Version != request.Execution.TaskPlugin.Version {
			executionErr = errors.New("video provider version changed before submission")
			return
		}
		if token.ModelLimitsEnabled && !token.GetModelLimitsMap()[job.Model] {
			executionErr = errors.New("video model permission changed")
			return
		}
		var liveChannel model.Channel
		if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", job.ChannelId, common.ChannelStatusEnabled).Take(&liveChannel).Error; err != nil {
			executionErr = errors.New("selected video channel is unavailable")
			return
		}
		if liveChannel.Type == constant.ChannelTypeTaskPlugin && liveChannel.GetSetting().TaskPluginKey != job.Provider || liveChannel.Type != constant.ChannelTypeTaskPlugin && !slices.Contains(pinned.Plugin.Meta.ChannelTypes, liveChannel.Type) {
			executionErr = errors.New("selected video channel capability changed")
			return
		}
		var ability model.Ability
		if err := model.DB.WithContext(ctx).Where(map[string]any{"channel_id": job.ChannelId, "group": job.Group, "enabled": true}).Where("model IN ?", []string{job.Model, ratio_setting.RoutingMatchModelName(job.Model)}).Take(&ability).Error; err != nil {
			executionErr = errors.New("selected video model is unavailable")
			return
		}
		if setupErr := middleware.SetupContextForSelectedChannel(c, &request.Channel, job.Model); setupErr != nil {
			executionErr = setupErr
			return
		}
		info, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
		if err != nil {
			executionErr = err
			return
		}
		policy, catalog, err := service.ResolveImagePolicy(ctx, token, job.Provider)
		capability, selected := service.VideoPolicyCapability(catalog, job.Model)
		if err != nil || policy.Group != job.Group && policy.Group != "auto" || !service.GroupInUserUsableGroups(common.GetContextKeyString(c, constant.ContextKeyUserGroup), job.Group) || policy.Id != 0 && (!policy.Enabled || !policy.AsyncEnabled || !selected) {
			executionErr = errors.New("video provider permission changed")
			return
		}
		bindingIndex := slices.IndexFunc(endpoint.Candidates, func(binding pluginruntime.ProtocolBinding) bool {
			return binding.Plugin != nil && binding.Plugin.Meta.Key == job.Provider && binding.Protocol == "openai_video"
		})
		if bindingIndex < 0 {
			executionErr = errors.New("video provider capability changed")
			return
		}
		videoRequest, videoFiles, videoAction, videoResolution, requestErr := asyncVideoCapabilityRequest(c)
		if requestErr != nil {
			executionErr = errors.New("video parameters are unavailable")
			return
		}
		profile, profileErr := service.EffectiveVideoProfile(endpoint.Candidates[bindingIndex], job.Model, capability.VideoOverrides)
		if profileErr != nil {
			executionErr = errors.New("video provider capability changed")
			return
		}
		if profileErr = service.ValidateVideoProfileRequest(profile, videoAction, videoRequest, videoFiles); profileErr != nil {
			executionErr = profileErr
			return
		}
		info.UsingGroup = job.Group
		common.SetContextKey(c, constant.ContextKeyUsingGroup, job.Group)
		common.SetContextKey(c, constant.ContextKeyAutoGroup, job.Group)
		policy.Group = job.Group
		var pools []model.ImageChannelPool
		if err := model.DB.WithContext(ctx).Where("binding_key = ?", service.ImagePoolBindingKey(policy, job.Model, service.VideoPoolResolution(videoResolution))).Find(&pools).Error; err != nil {
			executionErr = err
			return
		}
		if len(pools) > 0 && !slices.ContainsFunc(pools, func(pool model.ImageChannelPool) bool { return pool.ChannelId == job.ChannelId }) {
			executionErr = errors.New("video channel pool permission changed")
			return
		}
		info.TaskRelayInfo = &relaycommon.TaskRelayInfo{PublicTaskID: job.TaskId}
		info.LockedChannel = &request.Channel
		c.Set("async_media_execution", true)
		c.Set("async_media_billing", request.Billing)
		c.Set("async_media_before_billing", func() error {
			return model.UpdateAsyncMediaJob(ctx, job, map[string]any{"billing_status": "reserving"})
		})
		c.Set("async_media_before_dispatch", func(info *relaycommon.RelayInfo) error {
			return model.UpdateAsyncMediaJob(ctx, job, map[string]any{"dispatched_at": time.Now().Unix(), "quota": info.FinalPreConsumedQuota, "billing_status": "reserved"})
		})
		_, taskErr := executeTaskSubmissionWith(c, info, relay.RelayTaskSubmit)
		if taskErr != nil {
			executionErr = taskErr.Error
		}
	}}
	engine.POST(path, handler...)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(request.Body))
	if err != nil {
		return err
	}
	req.RemoteAddr = net.JoinHostPort(request.ClientIP, "0")
	req.Header.Set("Content-Type", request.ContentType)
	req.Header.Set("Authorization", "Bearer sk-"+token.Key)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	if executionErr != nil {
		return executionErr
	}
	if response.Code >= 400 {
		return errors.New("video request authorization or protocol validation failed")
	}
	return nil
}

func validateMediaToken(ctx context.Context, token model.Token, ipAddress string) error {
	if token.Status != common.TokenStatusEnabled && token.Status != common.TokenStatusExhausted || token.ExpiredTime != -1 && token.ExpiredTime <= time.Now().Unix() {
		return errors.New("media Token is unavailable")
	}
	var owner model.User
	if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", token.UserId, common.UserStatusEnabled).Take(&owner).Error; err != nil {
		return errors.New("media owner is unavailable")
	}
	if limits := token.GetIpLimits(); len(limits) > 0 {
		if ip := net.ParseIP(ipAddress); ip == nil || !common.IsIpInCIDRList(ip, limits) {
			return errors.New("client address is outside Token permissions")
		}
	}
	return nil
}

type mediaPipeWriter struct {
	headers http.Header
	pipe    *io.PipeWriter
	status  int
}

func (writer *mediaPipeWriter) Header() http.Header { return writer.headers }
func (writer *mediaPipeWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
}
func (writer *mediaPipeWriter) Flush() { writer.WriteHeader(http.StatusOK) }
func (writer *mediaPipeWriter) Write(data []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	if writer.status != http.StatusOK {
		return 0, fmt.Errorf("video download returned status %d", writer.status)
	}
	return writer.pipe.Write(data)
}

func PersistAsyncVideo(ctx context.Context, job model.AsyncMediaJob, task *model.Task) error {
	if job.ArtifactKeys != "" {
		var keys []string
		if err := common.UnmarshalJsonStr(job.ArtifactKeys, &keys); err != nil {
			return err
		}
		complete := len(keys) > 0
		for _, key := range keys {
			ref, err := service.GetTaskArtifactStore().Resolve(task, key)
			if err != nil {
				return err
			}
			if ref == nil {
				complete = false
			}
		}
		if complete {
			return nil
		}
	}
	copyTask := *task
	task = &copyTask
	restoreErr := service.RestoreAsyncMediaOutput(ctx, job, task)
	if restoreErr != nil && !errors.Is(restoreErr, os.ErrNotExist) {
		return restoreErr
	}
	// A signed upstream URL in the saved poll snapshot can expire between
	// storage attempts. Re-poll a completed dynamic task to obtain a fresh URL;
	// this never resubmits generation or repeats billing.
	if errors.Is(restoreErr, os.ErrNotExist) || (job.StorageStatus == "failed" || job.StorageStatus == "saving") && task.PrivateData.UpstreamAsync != nil {
		adaptor, err := initTaskArtifactAdaptor(task)
		if err != nil {
			return err
		}
		channel, err := service.FrozenAsyncMediaChannel(ctx, task)
		if err != nil {
			return err
		}
		key := task.PrivateData.Key
		if key == "" {
			key = channel.Key
		}
		base := channel.GetBaseURL()
		if base == "" {
			base = constant.GetChannelBaseURL(channel.Type)
		}
		var response *http.Response
		if task.PrivateData.UpstreamAsync != nil {
			response, err = service.PollUpstreamAsync(ctx, base, channel.GetSetting().Proxy, task.PrivateData.UpstreamAsync, service.UpstreamAsyncTemplateContext{
				TaskID: task.GetUpstreamTaskID(), Model: task.Properties.OriginModelName,
				UpstreamModel: task.Properties.UpstreamModelName, APIKey: key,
			})
		} else {
			poller, ok := adaptor.(service.TaskContextPollingAdaptor)
			if !ok {
				return errors.New("video provider does not support bounded content polling")
			}
			response, err = poller.FetchTaskContext(ctx, base, key, task, channel.GetSetting().Proxy)
		}
		if err != nil {
			return err
		}
		if response == nil || response.Body == nil {
			return errors.New("completed video result is unavailable")
		}
		defer response.Body.Close()
		cfg, err := service.GetMediaRuntimeConfig(ctx)
		if err != nil {
			return err
		}
		limit := cfg.MaxFileBytes*4 + (1 << 20)
		body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil {
			return err
		}
		statusOK := response.StatusCode == http.StatusOK
		if task.PrivateData.UpstreamAsync != nil {
			statusOK = response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
		}
		if !statusOK || int64(len(body)) > limit {
			return errors.New("completed video result is unavailable")
		}
		if task.PrivateData.UpstreamAsync != nil {
			result, parseErr := service.ParseUpstreamAsyncPollResponse(task.PrivateData.UpstreamAsync, body)
			if parseErr != nil {
				return parseErr
			}
			if result.Status != model.TaskStatusSuccess || len(result.URLs) == 0 {
				return errors.New("completed video result is unavailable")
			}
			task.PrivateData.UpstreamAsyncResponse = append(task.PrivateData.UpstreamAsyncResponse[:0], body...)
			task.PrivateData.ResultURL = result.URLs[0]
			task.Data = nil
		} else {
			result, parseErr := adaptor.ParseTaskResult(task, response, body)
			if parseErr != nil {
				return parseErr
			}
			if model.TaskStatus(result.Status) != model.TaskStatusSuccess {
				return errors.New("completed video result is unavailable")
			}
			task.Data = body
			if len(result.PluginState) > 0 {
				task.PrivateData.PluginState = result.PluginState
			}
			if result.Url != "" {
				task.PrivateData.ResultURL = result.Url
			}
		}
		if _, err := service.CaptureAsyncMediaOutput(ctx, task); err != nil {
			return err
		}
	}
	artifacts, err := projectTaskArtifacts(task)
	if err != nil {
		return err
	}
	if len(artifacts) == 0 {
		return errors.New("video result manifest is empty")
	}
	adaptor, err := initTaskArtifactAdaptor(task)
	if err != nil {
		return err
	}
	provider, ok := adaptor.(relaychannel.TaskContentRequestProvider)
	if !ok {
		return errors.New("video provider has no content interface")
	}
	store := service.GetTaskArtifactStore()
	count := 0
	for _, artifact := range artifacts {
		if artifact.Type != "video" {
			continue
		}
		count++
		if ref, err := store.Resolve(task, artifact.Key); err != nil {
			return err
		} else if ref != nil {
			continue
		}
		descriptor, err := provider.BuildContentRequest(task, artifact.Key, relaychannel.TaskArtifactClientRequest{Method: http.MethodGet})
		if err != nil {
			return err
		}
		reader, writer := io.Pipe()
		response := &mediaPipeWriter{headers: make(http.Header), pipe: writer}
		c, _ := gin.CreateTestContext(response)
		c.Request, _ = http.NewRequestWithContext(ctx, http.MethodGet, "/internal/media/download", nil)
		cfg, err := service.GetMediaRuntimeConfig(ctx)
		if err != nil {
			_ = reader.Close()
			_ = writer.Close()
			return err
		}
		c.Set("async_media_inline_limit", cfg.MaxFileBytes*4/3+1024)
		go func() { _ = writer.CloseWithError(proxyTaskMedia(c, task, descriptor)) }()
		_, persistErr := store.Persist(ctx, task, artifact, reader)
		_ = reader.CloseWithError(persistErr)
		if persistErr != nil {
			return persistErr
		}
	}
	if count == 0 {
		return errors.New("video result manifest contains no videos")
	}
	keys := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.Type == "video" {
			keys = append(keys, artifact.Key)
		}
	}
	encodedKeys, err := common.Marshal(keys)
	if err != nil {
		return err
	}
	if err := model.DB.WithContext(ctx).Model(&model.AsyncMediaJob{}).Where("id = ? AND lease_token = ?", job.ID, job.LeaseToken).Update("artifact_keys", string(encodedKeys)).Error; err != nil {
		return err
	}
	return nil
}
