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
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	gatewaydto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
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
	if c.GetBool("async_media_mixed_video") {
		c.Next()
		return
	}
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
		weight  int
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
	constraints.AddFilter(service.ConfiguredTaskVideoFilter(c))
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
		if !kitdto.PresetTaskVideoPlugin(provider) {
			profile, profileErr := service.EffectiveVideoProfile(binding, pinned.Model, capability.VideoOverrides)
			if profileErr != nil {
				capabilityErr = profileErr
				continue
			}
			if profileErr := service.ValidateVideoProfileRequest(profile, videoAction, videoRequest, videoFiles); profileErr != nil {
				capabilityErr = profileErr
				continue
			}
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
				selectedBinding := binding
				selectedPlugin, err := service.ChannelTaskVideoPlugin(binding.Plugin, &channel, pinned.Model)
				if err != nil {
					capabilityErr = err
					continue
				}
				selectedBinding.Plugin = selectedPlugin
				candidatePools := pools
				if kitdto.PresetTaskVideoPlugin(provider) {
					// A public alias can resolve to different model semantics and
					// defaults. Apply policy limits after resolving this channel.
					profile, err := service.EffectiveVideoProfile(selectedBinding, pinned.Model, capability.VideoOverrides)
					if err != nil {
						capabilityErr = err
						continue
					}
					upstream, _, err := channel.TaskVideoModelConfig(pinned.Model)
					if err != nil {
						capabilityErr = err
						continue
					}
					value, _ := c.Get(pluginruntime.ContextKeyProtocolRequest)
					request, valid := value.(pluginruntime.ProtocolRequestContext)
					if !valid {
						continue
					}
					request.UpstreamModel = upstream
					decoded, err := binding.Plugin.Engine.CallPath(context.WithoutCancel(c.Request.Context()), "protocols", []string{"openai_video", "decodeRequest"}, request.JSValue())
					intent, valid := decoded.(map[string]any)
					if err != nil || !valid {
						continue
					}
					action, _ := intent["action"].(string)
					if err := service.ValidateVideoProfileRequest(profile, action, intent["requestBody"], videoFiles); err != nil {
						capabilityErr = err
						continue
					}
					body, _ := intent["requestBody"].(map[string]any)
					resolvedResolution, _ := body["resolution"].(string)
					if selectedResolution := service.VideoPoolResolution(resolvedResolution); selectedResolution != resolution {
						candidatePools = nil
						if err := model.DB.WithContext(c.Request.Context()).Where("binding_key = ?", service.ImagePoolBindingKey(poolPolicy, pinned.Model, selectedResolution)).Find(&candidatePools).Error; err != nil {
							c.AbortWithStatus(503)
							return
						}
					}
				}
				// Automatic video routing retains its minimum global weight of one.
				weight := max(1, min(channel.GetWeight(), 1_000_000_000))
				if len(candidatePools) > 0 {
					index := slices.IndexFunc(candidatePools, func(pool model.ImageChannelPool) bool { return pool.ChannelId == channel.Id })
					if index < 0 {
						continue
					}
					service.ApplyMediaChannelPoolWeight(&channel, candidatePools[index].Priority)
					weight = channel.GetWeight()
				}
				choices = append(choices, choice{channel: channel, group: group, binding: selectedBinding, weight: weight})
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
	weights := make([]int, len(choices))
	for i := range choices {
		weights[i] = choices[i].weight
	}
	index := service.PickMediaChannelByWeight(weights, rand.Float64())
	if index < 0 {
		AsyncImagePublicError(c, 503, "provider_unavailable", "Video channel selection failed")
		c.Abort()
		return
	}
	selected := choices[index]
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
	var channel model.Channel
	if kitdto.ChannelConfiguredTaskVideoPlugin(c.GetString("task_plugin_key")) {
		value, _ := c.Get("task_video_channel_snapshot")
		data, _ := value.([]byte)
		if common.Unmarshal(data, &channel) != nil || channel.Id != job.ChannelId {
			AsyncImagePublicError(c, 503, "channel_unavailable", "Selected video channel snapshot is unavailable")
			return
		}
	} else {
		selected, err := model.CacheGetChannel(job.ChannelId)
		if err != nil {
			AsyncImagePublicError(c, 503, "channel_unavailable", "Video channel is unavailable")
			return
		}
		channel = *selected
	}
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
	if value, exists := c.Get("async_media_mixed_routing"); exists {
		request.MixedRouting, _ = value.(*service.MixedVideoRouting)
		request.InitialExecution = execution
	}
	if value, exists := c.Get(service.VideoRoutingContextKey); exists {
		request.ConditionalRouting, _ = value.(*service.VideoRoutingSnapshot)
	}
	request.Details = service.PublicVideoRoutingBody(request).RequestDetails()
	canonical, _ := c.Get("task_request")
	if err := service.FreezeAsyncVideoReferenceRecovery(&request, canonical, job.ExpiresAt); err != nil {
		AsyncImagePublicError(c, 400, "invalid_request", err.Error())
		return
	}
	if provider := c.GetString("async_media_provider"); provider != "" && request.Details != nil {
		request.Details.RequestParameters["provider"] = provider
	}
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
		if errors.Is(err, model.ErrAsyncQueueFull) {
			c.Header("Retry-After", "5")
			AsyncImagePublicError(c, 429, "async_queue_full", "Asynchronous video queue limit reached; retry after existing tasks finish")
		} else if errors.Is(err, model.ErrImageConflict) {
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
	if err := service.RefreshConditionalVideoAdmission(ctx, &job, &request); err != nil {
		return err
	}
	return executeAsyncVideoAttempt(ctx, job, request, nil)
}

func ExecuteAsyncVideoReferenceRetry(ctx context.Context, job model.AsyncMediaJob, task model.Task, request service.AsyncVideoRequest) error {
	if task.TaskID != job.TaskId || task.UserId != job.UserId || task.ChannelId != job.ChannelId || task.AttemptGeneration != job.AttemptGeneration || job.ReferenceRetryCount < 1 || task.Status != model.TaskStatusQueued || task.PrivateData.UpstreamTaskID != "" || !task.PrivateData.DurableBilling || !task.PrivateData.SubmitAccountingRecorded || !task.VideoRecoveryQuotaValid() {
		return errors.New("video reference recovery reservation is unavailable")
	}
	return executeAsyncVideoAttempt(ctx, job, request, &task)
}

func executeAsyncVideoAttempt(ctx context.Context, job model.AsyncMediaJob, request service.AsyncVideoRequest, retryTask *model.Task) error {
	var executionErr error
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", job.TokenId, job.UserId).Take(&token).Error; err != nil {
		return err
	}
	authenticate := middleware.TokenAuth()
	if model.IsCanvasTokenKey(token.Key) {
		authenticate = middleware.CanvasAsyncMediaWorkerAuth(job.TokenId, job.UserId)
	} else if err := validateMediaToken(ctx, token, request.ClientIP); err != nil {
		return err
	} else if retryTask != nil {
		// The original task already reserved its quota. Fresh database checks
		// above still enforce expiry, disabled users/tokens and IP restrictions.
		authenticate = func(c *gin.Context) {
			var owner model.User
			if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", token.UserId, common.UserStatusEnabled).Take(&owner).Error; err != nil {
				executionErr = errors.New("media owner is unavailable")
				c.Abort()
				return
			}
			owner.ToBaseUser().WriteContext(c)
			group := owner.Group
			if token.Group != "" {
				if !service.GroupInUserUsableGroups(owner.Group, token.Group) || token.Group != "auto" && !ratio_setting.ContainsGroupRatio(token.Group) {
					executionErr = errors.New("video Token group permission changed")
					c.Abort()
					return
				}
				group = token.Group
			}
			common.SetContextKey(c, constant.ContextKeyUsingGroup, group)
			if err := middleware.SetupContextForToken(c, &token); err != nil {
				executionErr = err
				c.Abort()
				return
			}
			c.Next()
		}
	}
	engine := gin.New()
	_ = engine.SetTrustedProxies(nil)
	operation := request.Operation
	path, validOperation := asyncVideoInternalPath(operation)
	if !validOperation {
		return errors.New("encrypted video operation is invalid")
	}
	handler := []gin.HandlerFunc{middleware.BodyStorageCleanup(), authenticate, func(c *gin.Context) { pinAsyncVideoWorker(c, job, request) }, func(c *gin.Context) { c.Set("async_media_provider", job.Provider); FilterAsyncVideoProvider(c) }, middleware.PrepareTaskPluginEndpoint(), func(c *gin.Context) {
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
		if retryTask != nil {
			keyIndex := slices.Index(liveChannel.GetKeys(), request.Channel.Key)
			if keyIndex < 0 || liveChannel.ChannelInfo.MultiKeyStatusList[keyIndex] != 0 && liveChannel.ChannelInfo.MultiKeyStatusList[keyIndex] != common.ChannelStatusEnabled {
				executionErr = errors.New("original video account is unavailable")
				return
			}
		}
		if kitdto.ChannelConfiguredTaskVideoPlugin(job.Provider) {
			// Model selection and capabilities were frozen at admission. Keep
			// checking live group access without consulting the edited model list.
			if !slices.Contains(strings.Split(liveChannel.Group, ","), job.Group) {
				executionErr = errors.New("selected video channel group is unavailable")
				return
			}
		} else {
			var ability model.Ability
			if err := model.DB.WithContext(ctx).Where(map[string]any{"channel_id": job.ChannelId, "group": job.Group, "enabled": true}).Where("model IN ?", []string{request.ExecutionModel(job.Model), ratio_setting.RoutingMatchModelName(request.ExecutionModel(job.Model))}).Take(&ability).Error; err != nil {
				executionErr = errors.New("selected video model is unavailable")
				return
			}
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
		policy, catalog, err := service.ResolveVideoRetryPolicy(ctx, token, job, request.MixedRouting)
		if err != nil {
			executionErr = err
			return
		}
		capability, _ := service.VideoPolicyCapability(catalog, job.Model)
		if request.MixedRouting != nil {
			capability, _ = service.MixedVideoPolicyCapability(catalog, job.Model)
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
		if request.MixedRouting != nil {
			profile, profileErr = service.EffectiveMixedVideoProfile(endpoint.Candidates[bindingIndex], job.Model, capability.VideoOverrides)
		}
		if request.ConditionalRouting.ActiveTarget() != nil {
			profile, profileErr = service.EffectiveMixedVideoProfile(endpoint.Candidates[bindingIndex], request.ExecutionModel(job.Model), service.VideoModelOverrides{})
		}
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
		if err := model.DB.WithContext(ctx).Where("binding_key = ?", service.MediaPoolBindingKey(policy, job.Model, service.VideoPoolResolution(videoResolution), "video")).Find(&pools).Error; err != nil {
			executionErr = err
			return
		}
		if request.ConditionalRouting.ActiveTarget() == nil && len(pools) > 0 && !slices.ContainsFunc(pools, func(pool model.ImageChannelPool) bool { return pool.ChannelId == job.ChannelId }) {
			executionErr = errors.New("video channel pool permission changed")
			return
		}
		info.TaskRelayInfo = &relaycommon.TaskRelayInfo{PublicTaskID: job.TaskId}
		info.LockedChannel = &request.Channel
		c.Set("async_media_execution", true)
		c.Set("async_media_execution_lease_token", job.LeaseToken)
		c.Set("async_media_inline_references", func(body any, limit int64, total *int64) (any, error) {
			prepared, err := service.InlineAsyncVideoReferences(c.Request.Context(), job, &request, body, limit, total)
			c.Set("async_media_inline_reference_mode", request.VideoReferenceTransportMode == "inline")
			return prepared, err
		})
		if retryTask != nil {
			// Submit-time provider adjustments may have changed the original
			// reservation. Preserve its settled hold and all frozen rates.
			request.Billing.Price.Quota = retryTask.Quota
		}
		c.Set("async_media_billing", request.Billing)
		if retryTask != nil {
			c.Set("async_media_reference_retry_task", *retryTask)
			c.Set("async_media_before_dispatch", func(info *relaycommon.RelayInfo) error {
				var liveToken model.Token
				if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", job.TokenId, job.UserId).Take(&liveToken).Error; err != nil {
					return err
				}
				if _, _, err := service.ResolveVideoRetryPolicy(ctx, liveToken, job, request.MixedRouting); err != nil {
					return err
				}
				var active model.Task
				if err := model.DB.WithContext(ctx).Where("id = ? AND attempt_generation = ? AND status = ?", retryTask.ID, retryTask.AttemptGeneration, model.TaskStatusQueued).Take(&active).Error; err != nil {
					return err
				}
				if active.Quota != retryTask.Quota || active.PrivateData.UpstreamTaskID != "" || !active.PrivateData.DurableBilling {
					return errors.New("video reference recovery reservation changed")
				}
				referenceMode := ""
				if details := request.RequestDetails(); details != nil && len(details.ReferenceMedia) > 0 {
					referenceMode = "original"
				}
				if slices.ContainsFunc(request.ReferenceInputs, func(input service.AsyncVideoReferenceInput) bool { return input.ObjectId != "" }) {
					referenceMode = "local_download"
				}
				decision, policyErr := service.RecheckVideoRecoveryPolicy(ctx, job, *retryTask, request)
				if policyErr != nil {
					return policyErr
				}
				if decision != nil && request.PendingDecision != nil && (decision.Revision != request.PendingDecision.Revision || decision.RuleID != request.PendingDecision.RuleID || decision.StepID != request.PendingDecision.StepID || decision.Action != request.PendingDecision.Action) {
					return service.ErrVideoRetryDeferred
				}
				if request.LocalReferencesReady && len(request.InlineReferenceInputs) > 0 {
					encoded, err := common.Marshal(request)
					if err != nil {
						return err
					}
					cipher, err := service.EncryptImagePayload(encoded, "media-request:"+job.TaskId)
					if err != nil {
						return err
					}
					if err := model.UpdateAsyncMediaJob(ctx, job, map[string]any{"request_cipher": cipher}); err != nil {
						return err
					}
					referenceMode = "local_download"
				}
				return model.DispatchAsyncVideoRecovery(ctx, job, *retryTask, decision, slices.Index(liveChannel.GetKeys(), request.Channel.Key), referenceMode)
			})
			result, taskErr := relay.RelayTaskSubmit(c, info)
			if taskErr != nil {
				if c.GetBool("async_media_submission_rejected") {
					executionErr = &service.AsyncVideoReferenceRejectedError{Reason: c.GetString("async_media_submission_retry_reason")}
				} else if errors.Is(taskErr.Error, service.ErrVideoRetryDeferred) {
					executionErr = service.ErrVideoRetryDeferred
				} else {
					executionErr = fmt.Errorf("video recovery submission failed (HTTP %d, code %s): %w", taskErr.StatusCode, taskErr.Code, taskErr.Error)
				}
				return
			}
			if result != nil && result.Immediate != nil && result.Immediate.Status == model.TaskStatusFailure && result.UpstreamTaskID == "" && result.Immediate.Url == "" && result.Immediate.RemoteUrl == "" {
				executionErr = &service.AsyncVideoReferenceRejectedError{Reason: service.VideoTaskFailureReason(result.Immediate.Reason)}
				return
			}
			if result == nil || result.UpstreamTaskID == "" || result.Immediate != nil && result.Immediate.Status != model.TaskStatusSuccess {
				executionErr = errors.New("video recovery receipt could not be confirmed")
				return
			}
			data := result.TaskData
			if result.UpstreamAsync != nil {
				data = result.UpstreamAsyncResponse
			}
			won, commitErr := model.CommitAsyncVideoReferenceSubmission(context.WithoutCancel(ctx), job, *retryTask, result.UpstreamTaskID, data, result.PluginState, result.Immediate != nil && result.Immediate.Status == model.TaskStatusSuccess)
			if commitErr != nil {
				executionErr = commitErr
			} else if !won {
				executionErr = model.ErrImageConflict
			} else if result.Immediate != nil && result.Immediate.Status == model.TaskStatusSuccess {
				var completed model.Task
				if err := model.DB.WithContext(context.WithoutCancel(ctx)).First(&completed, retryTask.ID).Error; err != nil {
					executionErr = err
					return
				}
				completed.Status, completed.Progress, completed.FinishTime = model.TaskStatusSuccess, "100%", time.Now().Unix()
				completed.PrivateData.VideoTerminalConfirmed = true
				completed.PrivateData.ResultURL = result.Immediate.Url
				if completed.PrivateData.ResultURL == "" {
					completed.PrivateData.ResultURL = result.Immediate.RemoteUrl
				}
				if _, err := service.CaptureAsyncMediaOutput(ctx, &completed); err != nil {
					executionErr = err
					return
				}
				completed.Data = service.RedactAsyncMediaData(completed.Data)
				completed.PrivateData.PluginState = service.RedactAsyncMediaData(completed.PrivateData.PluginState)
				_, executionErr = service.SettleTaskBillingOnComplete(context.WithoutCancel(ctx), nil, &completed, model.TaskStatusSubmitted, result.Immediate)
			}
			return
		}
		if request.InitialBillingChannelID == 0 {
			request.InitialBillingChannelID = job.ChannelId
		}
		initialRetry := &asyncVideoSubmissionRetry{job: &job, request: &request, liveChannel: &liveChannel, startedAt: time.Now().Unix()}
		cfg, cfgErr := service.GetImageRuntimeConfig(ctx)
		if cfgErr != nil {
			executionErr = cfgErr
			return
		}
		if request.InitialRetryState == nil {
			request.InitialRetryState = &mediaretry.State{TotalLimit: cfg.TotalRetries}
		}
		c.Set("async_media_initial_billing_channel_id", request.InitialBillingChannelID)
		c.Set("async_media_initial_retry_request", &request)
		c.Set("async_media_before_billing", func() error {
			if info.Billing != nil {
				return nil
			}
			return model.UpdateAsyncMediaJob(ctx, job, map[string]any{"billing_status": "reserving"})
		})
		c.Set("async_media_before_dispatch", func(info *relaycommon.RelayInfo) error {
			return initialRetry.dispatch(c, info)
		})
		_, taskErr := executeTaskSubmissionWith(c, info, func(c *gin.Context, info *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *gatewaydto.TaskError) {
			return retryAsyncVideoSubmission(c, info, relay.RelayTaskSubmit, initialRetry)
		})
		if taskErr != nil {
			cause := taskErr.Error
			if cause == nil {
				cause = errors.New(taskErr.Message)
			}
			executionErr = fmt.Errorf("video submission failed (HTTP %d, code %s): %w", taskErr.StatusCode, taskErr.Code, cause)
		}
	}}
	engine.POST(path, handler...)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(request.Body))
	if err != nil {
		return err
	}
	req.RemoteAddr = net.JoinHostPort(request.ClientIP, "0")
	req.Header.Set("Content-Type", request.ContentType)
	if !model.IsCanvasTokenKey(token.Key) {
		req.Header.Set("Authorization", "Bearer sk-"+token.Key)
	}
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

type asyncVideoSubmissionRetry struct {
	job         *model.AsyncMediaJob
	request     *service.AsyncVideoRequest
	liveChannel *model.Channel
	legacyUsed  bool
	startedAt   int64
	event       *model.MediaRetryEvent
	dispatched  bool
}

// submit keeps a single billing session until an accepted receipt is persisted.
// Only actual dispatch consumes a step; configuration changes during a wait
// return to the same error fact and evaluate the current revision again.
func (retry *asyncVideoSubmissionRetry) submit(c *gin.Context, info *relaycommon.RelayInfo, submit taskSubmitAttempt) (*relay.TaskSubmitResult, *gatewaydto.TaskError) {
	request, job := retry.request, retry.job
	var result *relay.TaskSubmitResult
	var taskErr *gatewaydto.TaskError
	evaluateOnly := false
	typedRejection := false
	for changes := 0; changes < 32; {
		if !evaluateOnly {
			retry.dispatched = false
			if service.VideoSubmissionRequiresReconciliation(job.Provider, request.MixedRouting != nil) {
				// A prior confirmed rejection may allow another attempt. Only a
				// successful dispatch barrier for this attempt can retain its hold.
				c.Set("async_media_reconciliation_dispatched", false)
			}
			result, taskErr = submit(c, info)
			typedRejection = taskErr == nil && result != nil && result.Immediate != nil && result.Immediate.Status == model.TaskStatusFailure && result.UpstreamTaskID == "" && result.Immediate.TaskID == "" && result.Immediate.Url == "" && result.Immediate.RemoteUrl == ""
			if typedRejection {
				// Only a driver's explicit typed terminal rejection authorizes
				// this branch. Missing IDs or HTTP 200 alone never prove refusal.
				c.Set("async_media_submission_rejected", true)
				c.Set("async_media_retryable_submission_rejection", false)
				c.Set("async_media_submission_retry_reason", result.Immediate.Reason)
				taskErr = service.TaskErrorWrapper(errors.New(result.Immediate.Reason), "upstream_rejected", http.StatusOK)
			}
		}
		if taskErr == nil {
			return result, nil
		}
		deferred := errors.Is(taskErr.Error, service.ErrVideoRetryDeferred)
		var preparation *service.MediaReferencePreparationError
		preparationFailed := errors.As(taskErr.Error, &preparation) && !retry.dispatched
		// Failed reference preparation has consumed its step; evaluateOnly
		// must still advance the policy even after a deferred dispatch.
		if !deferred && !evaluateOnly && taskErr.LocalError && !retry.dispatched && !preparationFailed {
			// A local validation or dispatch-barrier failure never reached the
			// provider. Keep any earlier unknown attempts, but do not invent a
			// new uncertain operation or consume an upstream retry for this one.
			return result, taskErr
		}
		if !deferred && !evaluateOnly {
			message := taskErr.Message
			if reason := c.GetString("async_media_submission_retry_reason"); reason != "" {
				message = reason
			}
			if info.ApiKey != "" {
				message = strings.ReplaceAll(message, info.ApiKey, "[redacted]")
			}
			deadline := job.ExpiresAt
			if end, ok := c.Request.Context().Deadline(); ok && (deadline == 0 || end.Unix() < deadline) {
				deadline = end.Unix()
			}
			fact := service.NormalizeMediaRetryFact(mediaretry.Fact{MediaType: "video", Provider: job.Provider, Model: job.Model, ChannelID: info.ChannelId, Stage: "submit", HTTPStatus: taskErr.StatusCode, ErrorCode: taskErr.Code, Message: message, Category: "unclassified", Unknown: retry.dispatched && !c.GetBool("async_media_submission_rejected"), Terminal: c.GetBool("async_media_submission_rejected"), HasReferences: request.HasReferenceInputs(), LocalSupported: (request.ReferenceRecoveryVersion == 1 || request.ReferenceRecoveryVersion == 2) && request.HasReferenceInputs() && !request.ReferenceLocalizationUnsupported, Deadline: deadline})
			fact.VideoEvidence = &mediaretry.VideoFailureEvidence{LegacySubmissionRejected: c.GetBool("async_media_retryable_submission_rejection"), SubmissionStartedAt: retry.startedAt, LegacySubmissionUsed: retry.legacyUsed}
			if preparationFailed {
				fact.Category = "reference_download"
				fact.HasReferences = request.HasReferenceInputs()
				fact.LocalSupported = fact.HasReferences
				fact.Accepted, fact.Unknown, fact.Terminal = false, false, false
				if request.PendingDecision != nil && retry.event != nil {
					next := mediaretry.Consume(*request.InitialRetryState, *request.PendingDecision)
					next.TotalUsed = request.InitialRetryState.TotalUsed
					request.InitialRetryState = &next
					request.PendingDecision = nil
					snapshot := *request
					snapshot.RetryFact = &fact
					encoded, err := common.Marshal(snapshot)
					if err != nil {
						return nil, service.TaskErrorWrapperLocal(err, "media_retry_state_failed", http.StatusInternalServerError)
					}
					cipher, err := service.EncryptImagePayload(encoded, "media-request:"+job.TaskId)
					if err != nil {
						return nil, service.TaskErrorWrapperLocal(err, "media_retry_state_failed", http.StatusInternalServerError)
					}
					if err := model.RecordAsyncVideoSubmissionPreparation(c.Request.Context(), *job, cipher, retry.event); err != nil {
						return nil, service.TaskErrorWrapperLocal(err, "media_retry_state_failed", http.StatusServiceUnavailable)
					}
					job.RequestCipher = cipher
				}
			}
			if fact.Unknown {
				fact.Category = "execution_unknown"
			}
			request.RetryFact = &fact
		} else if deferred {
			changes++
		}
		evaluateOnly = false
		decision, err := retry.decision(c.Request.Context())
		if err != nil {
			return nil, service.TaskErrorWrapperLocal(err, "media_retry_policy_unavailable", http.StatusServiceUnavailable)
		}
		state := *request.InitialRetryState
		state.Now = time.Now().Unix()
		actionAttempt := state.TotalUsed + 1
		for _, used := range state.RuleUses {
			actionAttempt += used
		}
		event, err := model.NewMediaRetryEvent(job.TaskId, int64(actionAttempt), *request.RetryFact, decision, state)
		if err != nil {
			return nil, service.TaskErrorWrapperLocal(err, "media_retry_event_failed", http.StatusInternalServerError)
		}
		if err := model.RecordMediaRetryEventTx(model.DB.WithContext(c.Request.Context()), event); err != nil {
			return nil, service.TaskErrorWrapperLocal(err, "media_retry_event_failed", http.StatusServiceUnavailable)
		}
		retry.event = event
		if !mediaretry.IsResubmission(decision.Action) || decision.BlockedReason != "" {
			request.PendingDecision = &decision
			unknownStop := decision.Matched && decision.Action == mediaretry.Stop && decision.BlockedReason == "" && request.RetryFact.Unknown && job.DispatchedAt > 0 && !c.GetBool("async_media_submission_refundable")
			if c.GetBool("async_media_execution") && info.Billing != nil && (unknownStop || request.HasRetainedSubmissionAttempts()) && info.Billing.GetPreConsumedQuota() > 0 {
				// End locally while carrying every earlier uncertain dispatch into
				// the durable Task and its existing reservation. A later rejection
				// does not confirm those earlier upstream operations.
				request.PendingDecision = &decision
				c.Set("async_media_policy_unknown_ended", true)
				if result == nil {
					result = &relay.TaskSubmitResult{Platform: constant.TaskPlatform(job.Provider), Immediate: &relaycommon.TaskInfo{Status: model.TaskStatusFailure, Reason: request.RetryFact.Message}}
				}
				result.Quota = info.Billing.GetPreConsumedQuota()
				return result, nil
			}
			if c.GetBool("async_media_reconciliation_dispatched") && !c.GetBool("async_media_submission_rejected") {
				taskErr.NoRetry = true
			}
			if typedRejection {
				return result, nil
			}
			return result, taskErr
		}
		if taskErr.NoRetry && !decision.Matched {
			return result, taskErr
		}
		if retry.dispatched && request.RetryFact.Unknown && !slices.ContainsFunc(request.InitialUnknownAttempts, func(attempt model.VideoTaskAttempt) bool {
			return attempt.Generation == int64(state.TotalUsed) && attempt.ChannelID == request.RetryFact.ChannelID
		}) {
			attempt := model.VideoTaskAttempt{ChannelID: request.RetryFact.ChannelID}
			if value, exists := c.Get("async_media_current_attempt"); exists {
				if current, ok := value.(*model.VideoTaskAttempt); ok && current != nil {
					attempt = *current
				}
			}
			attempt.Decision, attempt.Generation, attempt.Status = &decision, int64(state.TotalUsed), model.TaskStatusUnknown
			attempt.Reason, attempt.StartedAt, attempt.FinishedAt = request.RetryFact.Message, job.DispatchedAt, time.Now().Unix()
			attempt.RefundOnFailure = c.GetBool("async_media_submission_refundable")
			request.InitialUnknownAttempts = append(request.InitialUnknownAttempts, attempt)
		}
		request.PendingDecision = &decision
		if decision.DelaySeconds > 0 {
			timer := time.NewTimer(time.Duration(decision.DelaySeconds) * time.Second)
			select {
			case <-c.Request.Context().Done():
				timer.Stop()
				return nil, service.TaskErrorWrapperLocal(c.Request.Context().Err(), "request_cancelled", http.StatusRequestTimeout)
			case <-timer.C:
			}
		}
		// Check again before downloads/channel selection and once more at send.
		live, err := retry.decision(c.Request.Context())
		if err != nil {
			return nil, service.TaskErrorWrapperLocal(err, "media_retry_policy_unavailable", http.StatusServiceUnavailable)
		}
		if live.Revision != decision.Revision || live.Action != decision.Action || live.RuleID != decision.RuleID || live.StepID != decision.StepID {
			changes++
			evaluateOnly = true
			continue
		}
		if decision.Matched {
			candidate, err := service.PickVideoRetryChannel(c.Request.Context(), *job, *request, info.UpstreamModelName)
			if err != nil {
				return nil, service.TaskErrorWrapperLocal(err, "media_retry_channel_unavailable", http.StatusServiceUnavailable)
			}
			request.Channel = *candidate
			if request.MixedRouting != nil {
				var token model.Token
				if err := model.DB.WithContext(c.Request.Context()).First(&token, job.TokenId).Error; err != nil {
					return nil, service.TaskErrorWrapperLocal(err, "media_retry_channel_unavailable", 503)
				}
				_, catalog, err := service.MixedVideoPolicy(c.Request.Context(), token, request.MixedRouting)
				if err != nil {
					return nil, service.TaskErrorWrapperLocal(err, "media_retry_channel_unavailable", 403)
				}
				capability, _ := service.MixedVideoPolicyCapability(catalog, job.Model)
				decoded, err := service.RebindConditionalVideoRequest(c.Request.Context(), *job, request, *candidate, capability.VideoOverrides)
				if err != nil {
					return nil, service.TaskErrorWrapperLocal(err, "media_retry_channel_unavailable", 400)
				}
				if err := service.RebindMixedVideoRequest(request, decoded, job.ExpiresAt); err != nil {
					return nil, service.TaskErrorWrapperLocal(err, "media_retry_input_unavailable", 400)
				}
				job.Provider = decoded.Binding.Plugin.Meta.Key
				if request.ConditionalRouting != nil {
					c.Set(service.VideoRoutingContextKey, request.ConditionalRouting)
				}
				pinMixedVideoCandidate(c, decoded, request.MixedRouting, job.AttemptGeneration)
				view, _, err := service.MixedVideoRequestContext(request.Body, request.ContentType, request.Operation)
				if err != nil {
					return nil, service.TaskErrorWrapperLocal(err, "plugin_request_invalid", 400)
				}
				c.Set(pluginruntime.ContextKeyProtocolRequest, pluginruntime.ProtocolRequestContext{RouteRequestContext: view, Protocol: decoded.Binding.Protocol, Operation: decoded.Binding.Operation.Name, Model: job.Model, UpstreamModel: decoded.UpstreamModel})
				c.Set(pluginruntime.ContextKeyRouteRequest, view)
				c.Set("task_request", decoded.Intent["requestBody"])
				c.Set("task_action", decoded.Intent["action"])
				c.Set("task_plugin_key", job.Provider)
				c.Set("expected_task_plugin_key", job.Provider)
				c.Set("platform", job.Provider)
				c.Set("async_media_provider", job.Provider)
				constraints := service.GetChannelConstraints(c)
				constraints.Filters = slices.DeleteFunc(constraints.Filters, func(filter gatewaydto.ChannelFilter) bool { return filter.Kind == gatewaydto.FilterTaskPluginIdentity })
				constraints.Pins = slices.DeleteFunc(constraints.Pins, func(pin gatewaydto.ChannelPin) bool { return pin.Source == "media_policy" })
				constraints.AddPin(gatewaydto.ChannelPin{ChannelId: candidate.Id, Source: "media_policy", Rank: 20, RetryMode: gatewaydto.PinRetrySingleAttempt})
				service.AppendTaskPluginIdentityFilter(c, job.Provider)
			}
			*retry.liveChannel = model.Channel{}
			if err := model.DB.WithContext(c.Request.Context()).Where("id = ?", candidate.Id).Take(retry.liveChannel).Error; err != nil {
				return nil, service.TaskErrorWrapperLocal(err, "media_retry_channel_unavailable", http.StatusServiceUnavailable)
			}
		}
		local := decision.Action == mediaretry.LocalSameChannel || decision.Action == mediaretry.LocalOtherChannel
		if len(request.OriginalBody) == 0 {
			request.OriginalBody = append([]byte(nil), request.Body...)
		}
		request.Body = append([]byte(nil), request.OriginalBody...)
		request.LocalReferencesReady = false
		if local && retry.liveChannel.GetSetting().VideoReferenceTransportMode != "inline" {
			prepared, err := service.PrepareAsyncVideoLocalReferences(c.Request.Context(), *job, *request)
			if err != nil {
				// This action ran, but sent no generation. Retain its step usage
				// so the next failure decision advances the configured chain.
				next := mediaretry.Consume(*request.InitialRetryState, decision)
				next.TotalUsed = request.InitialRetryState.TotalUsed
				snapshot := *request
				snapshot.InitialRetryState = &next
				encoded, marshalErr := common.Marshal(snapshot)
				if marshalErr != nil {
					return nil, service.TaskErrorWrapperLocal(marshalErr, "media_retry_state_failed", http.StatusInternalServerError)
				}
				cipher, encryptErr := service.EncryptImagePayload(encoded, "media-request:"+job.TaskId)
				if encryptErr != nil {
					return nil, service.TaskErrorWrapperLocal(encryptErr, "media_retry_state_failed", http.StatusInternalServerError)
				}
				if saveErr := model.RecordAsyncVideoSubmissionPreparation(c.Request.Context(), *job, cipher, event); saveErr != nil {
					return nil, service.TaskErrorWrapperLocal(saveErr, "media_retry_state_failed", http.StatusServiceUnavailable)
				}
				request.InitialRetryState, job.RequestCipher = &next, cipher
				taskErr = service.TaskErrorWrapperLocal(err, "media_retry_input_unavailable", http.StatusBadRequest)
				evaluateOnly = true
				continue
			}
			*request = prepared
		}
		if apiErr := middleware.SetupContextForSelectedChannel(c, &request.Channel, job.Model); apiErr != nil {
			return nil, service.TaskErrorWrapperLocal(apiErr, "media_retry_channel_unavailable", http.StatusServiceUnavailable)
		}
		info.LockedChannel = &request.Channel
		common.CleanupBodyStorage(c)
		storage, err := common.CreateBodyStorage(request.Body)
		if err != nil {
			return nil, service.TaskErrorWrapperLocal(err, "read_request_body_failed", http.StatusBadRequest)
		}
		c.Set(common.KeyBodyStorage, storage)
		c.Request.Body, c.Request.ContentLength = io.NopCloser(storage), int64(len(request.Body))
		if strings.HasPrefix(strings.ToLower(request.ContentType), "application/json") {
			var body map[string]any
			if err := common.Unmarshal(request.Body, &body); err != nil {
				return nil, service.TaskErrorWrapperLocal(err, "read_request_body_failed", http.StatusBadRequest)
			}
			if value, ok := c.Get(pluginruntime.ContextKeyProtocolRequest); ok {
				protocol, valid := value.(pluginruntime.ProtocolRequestContext)
				if !valid {
					return nil, service.TaskErrorWrapperLocal(errors.New("video protocol context is unavailable"), "plugin_request_invalid", http.StatusBadRequest)
				}
				protocol.Body = map[string]any{"kind": "json", "value": body}
				protocol.RequestBody = body
				c.Set(pluginruntime.ContextKeyProtocolRequest, protocol)
			}
		}
	}
	return result, taskErr
}

func (retry *asyncVideoSubmissionRetry) decision(ctx context.Context) (mediaretry.Decision, error) {
	policy, err := service.GetMediaRetryPolicy(ctx)
	if err != nil {
		return mediaretry.Decision{}, err
	}
	state := *retry.request.InitialRetryState
	state.Now = time.Now().Unix()
	return service.PlanInitialVideoFailure(policy, *retry.request.RetryFact, state, retry.request.MixedRouting != nil)
}

func (retry *asyncVideoSubmissionRetry) dispatch(c *gin.Context, info *relaycommon.RelayInfo) error {
	ctx := c.Request.Context()
	request, job := retry.request, retry.job
	nextState := *request.InitialRetryState
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", job.TokenId, job.UserId).Take(&token).Error; err != nil {
		return err
	}
	if _, _, err := service.ResolveVideoRetryPolicy(ctx, token, *job, request.MixedRouting); err != nil {
		return err
	}
	if request.PendingDecision != nil {
		plugin, exists := pluginruntime.DefaultRegistry.Get(job.Provider)
		if !exists || plugin.Engine.SourceFingerprint() != request.PluginHash {
			return errors.New("video provider changed before submission")
		}
		if err := validateMediaToken(ctx, token, request.ClientIP); err != nil {
			return err
		}
		if token.ModelLimitsEnabled && !token.GetModelLimitsMap()[job.Model] {
			return errors.New("video model permission changed")
		}
		live, err := retry.decision(ctx)
		if err != nil {
			return err
		}
		pending := request.PendingDecision
		if live.Revision != pending.Revision || live.RuleID != pending.RuleID || live.StepID != pending.StepID || live.Action != pending.Action {
			return service.ErrVideoRetryDeferred
		}
		if !mediaretry.IsResubmission(live.Action) || live.BlockedReason != "" {
			return errors.New("video submission recovery is no longer allowed")
		}
		// Recheck current pool membership and credentials for the already
		// selected destination; do not silently pick a new account at send.
		selected := live
		selected.AllowedChannelIDs = []int{request.Channel.Id}
		check := *request
		check.PendingDecision = &selected
		candidates, err := service.VideoRetryCandidateChannels(ctx, *job, check, info.UpstreamModelName)
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			return service.ErrVideoCompatibleChannelsUnavailable
		}
		*retry.liveChannel = model.Channel{}
		if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", request.Channel.Id, common.ChannelStatusEnabled).Take(retry.liveChannel).Error; err != nil {
			return err
		}
		keyIndex := slices.Index(retry.liveChannel.GetKeys(), request.Channel.Key)
		if keyIndex < 0 || retry.liveChannel.ChannelInfo.MultiKeyStatusList[keyIndex] != 0 && retry.liveChannel.ChannelInfo.MultiKeyStatusList[keyIndex] != common.ChannelStatusEnabled {
			return errors.New("selected video account is unavailable")
		}
		nextState = mediaretry.Consume(nextState, live)
		if !live.Matched {
			nextState.TotalUsed++
		}
	}
	// The encrypted request and execution count cross the same lease/CAS
	// barrier as dispatch. A recovered uncertain send cannot restart the loop.
	snapshot := *request
	snapshot.InitialRetryState = &nextState
	if request.PendingDecision != nil && !request.PendingDecision.Matched && request.RetryFact != nil && request.RetryFact.VideoEvidence != nil {
		fact, evidence := *request.RetryFact, *request.RetryFact.VideoEvidence
		evidence.LegacySubmissionUsed = true
		fact.VideoEvidence = &evidence
		snapshot.RetryFact = &fact
	}
	encoded, err := common.Marshal(snapshot)
	if err != nil {
		return err
	}
	cipher, err := service.EncryptImagePayload(encoded, "media-request:"+job.TaskId)
	if err != nil {
		return err
	}
	channelData, err := common.Marshal(request.Channel)
	if err != nil {
		return err
	}
	channelCipher, err := service.EncryptImagePayload(channelData, "media-channel:"+job.TaskId)
	if err != nil {
		return err
	}
	referenceMode := ""
	if details := request.RequestDetails(); details != nil && len(details.ReferenceMedia) > 0 {
		referenceMode = "original"
	}
	if request.LocalReferencesReady {
		referenceMode = "local_download"
	}
	updates := map[string]any{"dispatched_at": time.Now().Unix(), "quota": info.FinalPreConsumedQuota, "billing_status": "reserved", "channel_id": request.Channel.Id, "request_cipher": cipher, "channel_cipher": channelCipher}
	if request.MixedRouting != nil {
		updates["provider"] = job.Provider
	}
	if err := model.DispatchAsyncMediaJob(ctx, *job, updates, slices.Index(retry.liveChannel.GetKeys(), request.Channel.Key), referenceMode, retry.event); err != nil {
		return err
	}
	retry.dispatched = true
	if service.VideoSubmissionRequiresReconciliation(job.Provider, request.MixedRouting != nil) {
		c.Set("async_media_reconciliation_dispatched", true)
	}
	job.ChannelId, job.RequestCipher, job.ChannelCipher = request.Channel.Id, cipher, channelCipher
	job.DispatchedAt = updates["dispatched_at"].(int64)
	request.InitialRetryState = &nextState
	request.InitialUnknownAttempts = snapshot.InitialUnknownAttempts
	request.RetryFact = snapshot.RetryFact
	if request.PendingDecision != nil && !request.PendingDecision.Matched {
		retry.legacyUsed = true
	}
	return nil
}

// retryAsyncVideoSubmission shares one billing session, account and body across
// at most two submissions. The relay alone can classify a complete supplier
// error as eligible; generic transport failures never grant a replay.
func retryAsyncVideoSubmission(c *gin.Context, info *relaycommon.RelayInfo, submit taskSubmitAttempt, policies ...*asyncVideoSubmissionRetry) (*relay.TaskSubmitResult, *gatewaydto.TaskError) {
	if len(policies) > 0 && policies[0] != nil {
		return policies[0].submit(c, info, submit)
	}
	c.Set("async_media_retryable_submission_rejection", false)
	result, taskErr := submit(c, info)
	if taskErr == nil || taskErr.NoRetry || !c.GetBool("async_media_retryable_submission_rejection") {
		return result, taskErr
	}
	logger.LogWarn(c, fmt.Sprintf("Async video submission retry task=%s reason=%s; at most one replay on the original account", info.PublicTaskID, c.GetString("async_media_submission_retry_reason")))
	timer := time.NewTimer(time.Second)
	select {
	case <-c.Request.Context().Done():
		timer.Stop()
		return nil, service.TaskErrorWrapperLocal(c.Request.Context().Err(), "request_cancelled", http.StatusRequestTimeout)
	case <-timer.C:
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "read_request_body_failed", http.StatusBadRequest)
	}
	if _, err = storage.Seek(0, io.SeekStart); err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "read_request_body_failed", http.StatusBadRequest)
	}
	c.Request.Body = io.NopCloser(storage)
	c.Set("async_media_retryable_submission_rejection", false)
	return submit(c, info)
}

func validateMediaToken(ctx context.Context, token model.Token, ipAddress string) error {
	if model.IsCanvasTokenKey(token.Key) {
		return errors.New("managed media requires the Infinite Canvas content bridge")
	}
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
	originalTask := task
	copyTask := *task
	task = &copyTask
	if task.Platform == "wolong" {
		ref, err := service.GetTaskArtifactStore().Resolve(task, "video")
		if err != nil {
			return err
		}
		if ref == nil {
			adaptor, err := initTaskArtifactAdaptor(task)
			if err != nil {
				return err
			}
			task, err = refreshWolongVideoContent(ctx, task, adaptor)
			if err != nil {
				return err
			}
		}
	}
	var restoreErr error
	if task.Platform != "wolong" {
		restoreErr = service.RestoreAsyncMediaOutput(ctx, job, task)
	}
	refreshResult := task.Platform != "wolong" && task.PrivateData.UpstreamAsync != nil && (job.StorageStatus == "failed" || job.StorageStatus == "saving")
	if restoreErr != nil && !errors.Is(restoreErr, os.ErrNotExist) && !refreshResult {
		return restoreErr
	}
	// A signed upstream URL in the saved poll snapshot can expire between
	// storage attempts. Re-poll a completed dynamic task to obtain a fresh URL;
	// this never resubmits generation or repeats billing.
	if errors.Is(restoreErr, os.ErrNotExist) || refreshResult {
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
		cfg, err := service.GetMediaRuntimeConfig(ctx)
		if err != nil {
			return err
		}
		pollContext, cancelPoll := context.WithTimeout(ctx, time.Duration(cfg.PollTimeout)*time.Second)
		defer cancelPoll()
		var response *http.Response
		if task.PrivateData.UpstreamAsync != nil {
			response, err = service.PollUpstreamAsync(pollContext, base, channel.GetSetting().Proxy, task.PrivateData.UpstreamAsync, service.UpstreamAsyncTemplateContext{
				TaskID: task.GetUpstreamTaskID(), Model: task.Properties.OriginModelName,
				UpstreamModel: task.Properties.UpstreamModelName, APIKey: key,
			})
		} else {
			poller, ok := adaptor.(service.TaskContextPollingAdaptor)
			if !ok {
				return errors.New("video provider does not support bounded content polling")
			}
			response, err = poller.FetchTaskContext(pollContext, base, key, task, channel.GetSetting().Proxy)
		}
		if err != nil {
			return err
		}
		if response == nil || response.Body == nil {
			return errors.New("completed video result is unavailable")
		}
		defer response.Body.Close()
		limit := cfg.MaxFileBytes*4 + (1 << 20)
		body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		cancelPoll()
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
			task.PrivateData.UpstreamAsyncResponse = append([]byte(nil), body...)
			task.PrivateData.ResultURL = result.URLs[0]
			originalTask.PrivateData.UpstreamAsyncResponse = task.PrivateData.UpstreamAsyncResponse
			originalTask.PrivateData.ResultURL = result.URLs[0]
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
