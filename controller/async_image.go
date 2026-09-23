package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func AsyncImagePublicError(c *gin.Context, status int, code, message string) {
	c.Header("Cache-Control", "no-store")
	c.JSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "code": code, "message": message}})
}

func AsyncImageGatewayBase() string {
	base := strings.TrimRight(system_setting.ServerAddress, "/")
	if strings.HasSuffix(base, "/v1") {
		base = strings.TrimSuffix(base, "/v1")
	}
	return base
}

func AsyncImageIdempotencyKey(c *gin.Context) (string, error) {
	key := c.GetHeader("Idempotency-Key")
	if len(key) > 255 || !utf8.ValidString(key) {
		return "", errors.New("Idempotency-Key must not exceed 255 UTF-8 bytes")
	}
	if key == "" {
		return "", nil
	}
	return service.ImageIdentityHash(key), nil
}

func SubmitAsyncImage(c *gin.Context) {
	ctx := c.Request.Context()
	cfg, err := service.GetImageRuntimeConfig(ctx)
	if err != nil {
		AsyncImagePublicError(c, 503, "async_image_unavailable", "Async image configuration is unavailable")
		return
	}
	key, err := AsyncImageIdempotencyKey(c)
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_idempotency_key", err.Error())
		return
	}
	// The body ceiling is derived from the independently bounded image count and
	// per-image byte limit; there is no separate combined reference-image quota.
	maxBody := int64(cfg.MaxReferences+1)*cfg.DownloadMaxBytes/3*4 + 1<<20
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBody))
	if err != nil {
		AsyncImagePublicError(c, 413, "request_too_large", "Image request exceeds the byte limit")
		return
	}
	request, err := service.ParseAsyncImageRequest(raw, c.GetHeader("Content-Type"), c.Request.URL.Path, cfg)
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_request", err.Error())
		return
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", c.GetInt("token_id"), c.GetInt("id")).Take(&token).Error; err != nil {
		AsyncImagePublicError(c, 503, "database_unavailable", "Token lookup is unavailable")
		return
	}
	fingerprint := service.AsyncImageRequestHash(request.Platform, request.Dialect, request.SourcePath, raw)
	if key != "" {
		var binding model.AsyncImageIdempotency
		err := model.DB.WithContext(ctx).Where("token_id = ? AND key_hash = ?", token.Id, key).Take(&binding).Error
		if err == nil {
			if binding.RequestHash != fingerprint {
				AsyncImagePublicError(c, 409, "async_image_idempotency_conflict", "Idempotency-Key was used for different request bytes")
				return
			}
			var task model.AsyncImageTask
			if err := model.DB.WithContext(ctx).Where("task_id = ? AND token_id = ?", binding.TaskId, token.Id).Take(&task).Error; err != nil {
				AsyncImagePublicError(c, 409, "async_image_result_unavailable", "Idempotent task is unavailable")
				return
			}
			respondAsyncImageAccepted(c, task, true)
			return
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			AsyncImagePublicError(c, 503, "database_unavailable", "Idempotency lookup is unavailable")
			return
		}
	}
	policy, _, err := service.ValidateAsyncImageEligibility(ctx, token, request, cfg)
	if err != nil {
		var invalid *service.AsyncImageFailure
		if errors.As(err, &invalid) && invalid.HTTPStatus == 400 {
			AsyncImagePublicError(c, 400, "invalid_request", invalid.Message)
			return
		}
		AsyncImagePublicError(c, 403, "async_image_unavailable", err.Error())
		return
	}
	if err := common.RDB.Ping(ctx).Err(); err != nil {
		AsyncImagePublicError(c, 503, "async_image_queue_unavailable", "Redis scheduling is unavailable")
		return
	}
	inputIds, err := service.AsyncImageInputReferences(ctx, token.Id, request)
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_reference_image", err.Error())
		return
	}
	task := model.AsyncImageTask{TaskId: "asyncimg_" + common.GetUUID(), UserId: token.UserId, TokenId: token.Id, Group: policy.Group, Platform: request.Platform, Dialect: request.Dialect, Model: request.Model, RequestType: request.Kind, SourcePath: request.SourcePath, RequestedSize: request.Size, RequestedResolution: request.Resolution, AspectRatio: request.AspectRatio, RequestHash: fingerprint, ExpiresAt: time.Now().Unix() + int64(cfg.TaskRetentionDays)*86400}
	if request.Dialect == "async" {
		request.PolicyPlatform = policy.Platform
		request.ClientIP = c.ClientIP()
		selected, _, err := service.PickImageChannel(ctx, policy, request, service.ImageAccountRouting{})
		if err != nil {
			AsyncImagePublicError(c, 503, "channel_unavailable", err.Error())
			return
		}
		capability := service.ImageChannelCapability(*selected, request.Model)
		task.Provider, task.ExecutionProtocol, task.ChannelId = capability.Provider, capability.Protocol, selected.Id
		request.Provider = capability.Provider
		if capability.Protocol == "gemini_native" {
			request.Platform = "gemini"
		} else {
			request.Platform = "openai"
		}
		channelData, err := common.Marshal(selected)
		if err == nil {
			task.SelectedChannelCipher, err = service.EncryptImagePayload(channelData, "image-channel:"+task.TaskId)
		}
		if err != nil {
			AsyncImagePublicError(c, 503, "encryption_unavailable", "Image channel could not be frozen")
			return
		}
		if service.FreezeAsyncImageBillingFunc == nil {
			AsyncImagePublicError(c, 503, "pricing_unavailable", "Image pricing is unavailable")
			return
		}
		billing, err := service.FreezeAsyncImageBillingFunc(ctx, token, request, task.Group, selected)
		if err != nil {
			AsyncImagePublicError(c, 400, "pricing_unavailable", err.Error())
			return
		}
		request.Billing = &billing
		task.Platform = request.Platform
	}
	if urls := request.ReferenceURLs(); len(urls) > 0 {
		preview, err := common.Marshal(urls)
		if err != nil {
			AsyncImagePublicError(c, 400, "invalid_request", "Image references could not be encoded")
			return
		}
		task.ReferenceUrls = string(preview)
	}
	if cfg.PromptPreview {
		characters := []rune(request.Prompt)
		task.PromptSummary = string(characters[:min(len(characters), cfg.PromptPreviewChars)])
	}
	encoded, err := common.Marshal(request)
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_request", "Image request could not be encoded")
		return
	}
	task.RequestCipher, err = service.EncryptImagePayload(encoded, "task:"+task.TaskId)
	if err != nil {
		AsyncImagePublicError(c, 503, "async_image_encryption_unavailable", "Stable image payload encryption keys are required")
		return
	}
	task, reused, err := model.AcceptAsyncImageTask(ctx, task, key, inputIds)
	if err != nil {
		if errors.Is(err, model.ErrImageConflict) {
			AsyncImagePublicError(c, 409, "async_image_idempotency_conflict", err.Error())
		} else {
			AsyncImagePublicError(c, 503, "database_unavailable", "Image admission could not be persisted")
		}
		return
	}
	respondAsyncImageAccepted(c, task, reused)
}

func respondAsyncImageAccepted(c *gin.Context, task model.AsyncImageTask, reused bool) {
	queryURL := AsyncImageGatewayBase() + "/v1/images/tasks_async/" + url.PathEscape(task.TaskId)
	if task.Dialect == "async" {
		queryURL = AsyncImageGatewayBase() + "/v1/media/tasks_async/" + url.PathEscape(task.TaskId)
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Location", queryURL)
	c.Header("Retry-After", "3")
	if reused {
		c.Header("X-Idempotency-Replayed", "true")
	}
	response := gin.H{"task_id": task.TaskId, "query_url": queryURL}
	if task.Platform == "gemini" && task.Dialect == "bb" {
		response["id"] = task.TaskId
		response["object"] = "image.task"
		response["status"] = "queued"
	}
	c.JSON(http.StatusAccepted, response)
}

func QueryAsyncImage(c *gin.Context) {
	ctx := c.Request.Context()
	var task model.AsyncImageTask
	err := model.DB.WithContext(ctx).Where("task_id = ? AND token_id = ? AND user_id = ?", c.Param("task_id"), c.GetInt("token_id"), c.GetInt("id")).Take(&task).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		AsyncImagePublicError(c, 404, "task_not_found", "Image task was not found")
		return
	}
	if err != nil {
		AsyncImagePublicError(c, 503, "database_unavailable", "Image task lookup is unavailable")
		return
	}
	if task.Terminal() && task.ExpiresAt > 0 && task.ExpiresAt <= time.Now().Unix() {
		AsyncImagePublicError(c, 404, "task_not_found", "Image task was not found")
		return
	}
	c.Header("Cache-Control", "no-store")
	response := gin.H{"task_id": task.TaskId}
	if strings.Contains(c.Request.URL.Path, "/media/") {
		displayStatus := task.DisplayStatus()
		response["media_type"], response["provider"], response["protocol"] = "image", task.Provider, task.ExecutionProtocol
		response["stage"], response["progress"], response["billing_status"], response["quota"] = mediaStage(displayStatus), task.Progress, task.BillingStatus, task.Quota
		response["storage_status"] = "pending"
		if task.ResultsAvailable() {
			response["storage_status"] = "succeeded"
		}
		if task.Status == model.ImageTaskStorageFailed {
			response["storage_status"] = "failed"
		}
	}
	if task.ResultsAvailable() {
		cfg, err := service.GetImageRuntimeConfig(ctx)
		if err != nil {
			AsyncImagePublicError(c, 503, "storage_unavailable", "Image links are unavailable")
			return
		}
		var results []model.AsyncImageResult
		if err := model.DB.WithContext(ctx).Where("task_id = ?", task.TaskId).Order("image_index").Find(&results).Error; err != nil {
			AsyncImagePublicError(c, 503, "storage_unavailable", "Image results are unavailable")
			return
		}
		if len(results) != task.ResultCount {
			AsyncImagePublicError(c, 503, "storage_unavailable", "Image result manifest is incomplete")
			return
		}
		data := make([]gin.H, 0, len(results))
		for _, result := range results {
			var object model.ImageStorageObject
			if err := model.DB.WithContext(ctx).Where("object_id = ? AND status = ?", result.ObjectId, "active").Take(&object).Error; err != nil {
				AsyncImagePublicError(c, 503, "storage_unavailable", "Image object is unavailable")
				return
			}
			link, err := service.ImageObjectURL(ctx, object, AsyncImageGatewayBase(), cfg.SignedURLExpiry)
			if err != nil {
				AsyncImagePublicError(c, 503, "storage_unavailable", "Image signing is unavailable")
				return
			}
			data = append(data, gin.H{"url": link})
		}
		response["status"] = "succeeded"
		response["data"] = data
		c.JSON(200, response)
		return
	}
	if task.ErrorCode == "result_download_failed" {
		urls, err := model.GetAsyncImageUpstreamResultURLs(ctx, task.TaskId)
		if err != nil {
			AsyncImagePublicError(c, 503, "database_unavailable", "Upstream image results are unavailable")
			return
		}
		if len(urls) > 0 {
			data := make([]gin.H, 0, len(urls))
			for _, resultURL := range urls {
				if !service.PublicUpstreamAsyncResultURL(resultURL) {
					data = nil
					break
				}
				data = append(data, gin.H{"url": resultURL})
			}
			if len(data) == len(urls) {
				response["data"], response["result_source"] = data, "upstream"
			}
		}
	}
	if task.Terminal() || (task.Status == model.ImageTaskStorageFailed || task.Status == model.ImageTaskBillingFailed) && task.NextAttemptAt == 0 {
		code := task.PublicErrorCode
		if code < 601 || code > 613 {
			code = 610
		}
		response["status"] = "failed"
		response["error_code"] = code
		response["fail_reason"] = task.ErrorMessage
		c.JSON(200, response)
		return
	}
	status := "processing"
	if task.DisplayStatus() == model.ImageTaskQueued {
		status = "queued"
	}
	response["status"] = status
	c.Header("Retry-After", "3")
	c.JSON(200, response)
}

func UploadAsyncImageInput(c *gin.Context) {
	ctx := c.Request.Context()
	cfg, err := service.GetImageRuntimeConfig(ctx)
	if err != nil {
		AsyncImagePublicError(c, 503, "async_image_unavailable", "Image configuration is unavailable")
		return
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", c.GetInt("token_id"), c.GetInt("id")).Take(&token).Error; err != nil {
		AsyncImagePublicError(c, 503, "database_unavailable", "Token lookup is unavailable")
		return
	}
	policy, _, err := service.ResolveImagePolicy(ctx, token, "gemini")
	if err != nil || !cfg.AsyncEnabled || !policy.Enabled || !policy.AsyncEnabled || !common.RedisEnabled || common.RDB == nil {
		AsyncImagePublicError(c, 403, "async_image_unavailable", "SC input uploads are unavailable for this Token")
		return
	}
	var owner model.User
	if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", token.UserId, common.UserStatusEnabled).Take(&owner).Error; err != nil {
		AsyncImagePublicError(c, 403, "async_image_unavailable", "SC input owner is unavailable")
		return
	}
	if !service.IsUserSelectableGroup(owner.Group, policy.Group) {
		AsyncImagePublicError(c, 403, "async_image_unavailable", "SC image group is outside user permissions")
		return
	}
	if !service.ImageEncryptionAvailable() {
		AsyncImagePublicError(c, 503, "async_image_encryption_unavailable", "Stable image encryption keys are required")
		return
	}
	if err := common.RDB.Ping(ctx).Err(); err != nil {
		AsyncImagePublicError(c, 503, "async_image_queue_unavailable", "Redis scheduling is unavailable")
		return
	}
	store, err := service.GetImageStorage(ctx, "temporary")
	if err != nil {
		AsyncImagePublicError(c, 503, "storage_unavailable", "Temporary image storage is unavailable")
		return
	}
	key, err := AsyncImageIdempotencyKey(c)
	if err != nil {
		AsyncImagePublicError(c, 400, "invalid_idempotency_key", err.Error())
		return
	}
	if key == "" {
		key = service.ImageIdentityHash(common.GetUUID())
	}
	ticket, err := model.RecordImageUploadAttempt(ctx, token.Id, cfg.UploadsPerMinute)
	if err != nil {
		respondImageUploadError(c, err)
		return
	}
	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		AsyncImagePublicError(c, 400, "invalid_request", "SC upload requires multipart/form-data and one file field")
		return
	}
	reader := multipart.NewReader(http.MaxBytesReader(c.Writer, c.Request.Body, cfg.SingleUploadBytes+64<<10), params["boundary"])
	var data []byte
	var filename, declared string
	count := 0
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var byteError *http.MaxBytesError
			if errors.As(err, &byteError) {
				AsyncImagePublicError(c, 413, "request_too_large", "Uploaded image exceeds the byte limit")
				return
			}
			AsyncImagePublicError(c, 400, "invalid_request", "Invalid multipart upload")
			return
		}
		if part.FormName() != "file" || count > 0 {
			_ = part.Close()
			AsyncImagePublicError(c, 400, "invalid_request", "SC upload accepts exactly one file field")
			return
		}
		count++
		filename = part.FileName()
		declared = part.Header.Get("Content-Type")
		data, err = io.ReadAll(io.LimitReader(part, cfg.SingleUploadBytes+1))
		_ = part.Close()
		if err != nil || int64(len(data)) > cfg.SingleUploadBytes {
			AsyncImagePublicError(c, 413, "request_too_large", "Uploaded image exceeds the byte limit")
			return
		}
	}
	if count != 1 || len(data) == 0 {
		AsyncImagePublicError(c, 400, "invalid_request", "A nonempty file field is required")
		return
	}
	filename = path.Base(strings.ReplaceAll(filename, "\\", "/"))
	filename = strings.TrimSpace(strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return -1
		}
		return value
	}, filename))
	if filename == "." {
		filename = ""
	}
	if filename == "" {
		extension := ".png"
		if http.DetectContentType(data) == "image/jpeg" {
			extension = ".jpg"
		} else if http.DetectContentType(data) == "image/webp" {
			extension = ".webp"
		}
		filename = "image" + extension
	}
	if len(filename) > 255 {
		AsyncImagePublicError(c, 400, "invalid_request", "Filename exceeds 255 UTF-8 bytes")
		return
	}
	sum := sha256.Sum256(data)
	fingerprint := service.ImageIdentityHash(hex.EncodeToString(sum[:]), declared, filename, strconv.Itoa(len(data)))
	now := time.Now().Unix()
	input := model.ImageInputObject{InputId: "sci_" + common.GetUUID(), TokenId: token.Id, KeyHash: key, Fingerprint: fingerprint, Filename: filename, ByteSize: int64(len(data)), Status: "reserved", LeaseToken: common.GetUUID(), LeaseExpiresAt: now + int64(cfg.UploadTimeout) + 120, CreatedAt: now, ExpiresAt: now + int64(cfg.InputRetentionHours)*3600}
	input.IntentKey = service.ImageIdentityHash("sc-input", input.InputId)
	input, reused, err := model.ReserveImageInput(ctx, ticket, input, cfg.InputBytesPerKey)
	if err != nil {
		respondImageUploadError(c, err)
		return
	}
	var object model.ImageStorageObject
	if reused {
		if err := model.DB.WithContext(ctx).Where("object_id = ? AND status = ?", input.ObjectId, "active").Take(&object).Error; err != nil {
			respondImageUploadError(c, model.ErrImageUploadUnavailable)
			return
		}
	} else {
		image, err := service.ValidateImageBytes(data, declared, cfg.SingleUploadBytes, cfg.DownloadMaxPixels)
		if err != nil {
			_ = model.DB.WithContext(ctx).Model(&model.ImageInputObject{}).Where("input_id = ? AND status = ?", input.InputId, "reserved").Update("status", "rejected").Error
			AsyncImagePublicError(c, 400, "invalid_image", err.Error())
			return
		}
		extension := ".png"
		if image.ContentType == "image/jpeg" {
			extension = ".jpg"
		} else if image.ContentType == "image/webp" {
			extension = ".webp"
		}
		key := time.Unix(input.CreatedAt, 0).UTC().Format("inputs/2006/01/02/") + service.ImageIdentityHash(input.InputId)[:32] + extension
		intent := model.ImageUploadIntent{IntentKey: input.IntentKey, Class: "temporary", UserId: token.UserId, TokenId: token.Id, ProfileId: store.Profile.ProfileId, ObjectKey: key, ContentType: image.ContentType, ByteSize: int64(len(data)), Checksum: image.Checksum, Status: "pending", CreatedAt: now}
		writeCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.UploadTimeout)*time.Second)
		object, err = service.StoreImageIntent(writeCtx, intent, image, input.ExpiresAt)
		cancel()
		if err != nil {
			_ = model.DB.WithContext(context.WithoutCancel(ctx)).Model(&model.ImageInputObject{}).Where("input_id = ? AND status = ?", input.InputId, "reserved").Update("status", "failed").Error
			AsyncImagePublicError(c, 503, "storage_unavailable", "SC image upload could not be confirmed")
			return
		}
	}
	link, err := service.ImageObjectURL(ctx, object, AsyncImageGatewayBase(), cfg.SignedURLExpiry)
	if err != nil {
		AsyncImagePublicError(c, 503, "storage_unavailable", "Image signing is unavailable")
		return
	}
	if err := model.ConfirmImageInput(ctx, input, object, service.ImageIdentityHash(link)); err != nil {
		respondImageUploadError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	if reused {
		c.Header("X-Idempotency-Replayed", "true")
	}
	c.JSON(200, gin.H{"url": link, "created_at": input.CreatedAt})
}

func respondImageUploadError(c *gin.Context, err error) {
	status, code, message := 503, "database_unavailable", "Image upload admission is unavailable"
	switch {
	case errors.Is(err, model.ErrImageConflict):
		status, code, message = 409, "async_image_upload_idempotency_conflict", "Idempotency-Key was used for different upload bytes or metadata"
	case errors.Is(err, model.ErrImageUploadRateLimited):
		status, code, message = 429, err.Error(), "SC upload attempts exceed the rolling minute limit"
	case errors.Is(err, model.ErrImageUploadByteQuota):
		status, code, message = 409, err.Error(), "SC input byte quota is exhausted"
	case errors.Is(err, model.ErrImageUploadInProgress):
		status, code, message = 409, err.Error(), "SC image upload is in progress"
	case errors.Is(err, model.ErrImageUploadUnavailable):
		status, code, message = 409, err.Error(), "SC input result is unavailable; use a new Idempotency-Key"
	case errors.Is(err, model.ErrImageUploadAliasLimit):
		status, code, message = 429, err.Error(), "SC input signature alias limit was reached"
	}
	if status == 429 || errors.Is(err, model.ErrImageUploadInProgress) {
		c.Header("Retry-After", "60")
	}
	AsyncImagePublicError(c, status, code, message)
}

func GetImageObjectContent(c *gin.Context) {
	if !service.VerifyImageObjectSignature(c.Param("object_id"), c.Query("expires"), c.Query("key"), c.Query("signature")) {
		c.Status(404)
		return
	}
	var object model.ImageStorageObject
	if err := model.DB.WithContext(c.Request.Context()).Where("object_id = ? AND status = ? AND (expires_at = 0 OR expires_at > ?)", c.Param("object_id"), "active", time.Now().Unix()).Take(&object).Error; err != nil {
		c.Status(404)
		return
	}
	store, err := service.GetImageObjectStorage(c.Request.Context(), object)
	if err != nil || store.Profile.Backend != "local" {
		c.Status(503)
		return
	}
	filename, err := store.LocalPath(object.ObjectKey, false)
	if err != nil {
		c.Status(404)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Type", object.ContentType)
	c.File(filename)
}
