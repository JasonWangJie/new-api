package controller

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func canvasResponseHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
}

func canvasError(c *gin.Context, err error) {
	status, message := http.StatusServiceUnavailable, service.ErrCanvasUnavailable.Error()
	if errors.Is(err, service.ErrCanvasAuthorization) {
		status, message = http.StatusUnauthorized, service.ErrCanvasAuthorization.Error()
	}
	c.JSON(status, gin.H{"error": "canvas_authorization_failed", "message": message})
}

func canvasClientRequest(c *gin.Context) bool {
	canvasResponseHeaders(c)
	if !c.GetBool("canvas_client_authenticated") || c.GetHeader("Origin") != "" {
		canvasError(c, service.ErrCanvasAuthorization)
		return false
	}
	return true
}

func AuthorizeInfiniteCanvas(c *gin.Context) {
	canvasResponseHeaders(c)
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok {
		canvasError(c, service.ErrCanvasAuthorization)
		return
	}
	var request struct {
		ClientID    string `json:"client_id"`
		RedirectURI string `json:"redirect_uri"`
		State       string `json:"state"`
		Challenge   string `json:"code_challenge"`
		Method      string `json:"code_challenge_method"`
		UserID      any    `json:"user_id"`
	}
	if common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 16384), &request) != nil || request.UserID != nil {
		c.JSON(400, gin.H{"error": "invalid_authorization_request"})
		return
	}
	redirect, err := service.AuthorizeCanvas(c.Request.Context(), identity, request.ClientID, request.RedirectURI, request.State, request.Challenge, request.Method)
	if err != nil {
		canvasError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"redirect_url": redirect})
}

func ExchangeInfiniteCanvas(c *gin.Context) {
	if !canvasClientRequest(c) {
		return
	}
	var request struct {
		Code        string `json:"code"`
		RedirectURI string `json:"redirect_uri"`
		Verifier    string `json:"code_verifier"`
	}
	if common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 16384), &request) != nil {
		c.JSON(400, gin.H{"error": "invalid_exchange_request"})
		return
	}
	result, err := service.ExchangeCanvasCode(c.Request.Context(), request.Code, request.RedirectURI, request.Verifier)
	if err != nil {
		canvasError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func IntrospectInfiniteCanvas(c *gin.Context) {
	if !canvasClientRequest(c) {
		return
	}
	var request struct {
		GrantID  string `json:"grant_id"`
		Activity bool   `json:"activity"`
	}
	if common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4096), &request) != nil {
		c.JSON(400, gin.H{"error": "invalid_introspection_request"})
		return
	}
	grant, user, err := service.GetCanvasGrant(c.Request.Context(), request.GrantID, request.Activity)
	if err != nil {
		if errors.Is(err, service.ErrCanvasUnavailable) {
			canvasError(c, err)
			return
		}
		c.JSON(200, gin.H{"active": false, "reason": service.CanvasInactiveReason(err)})
		return
	}
	c.JSON(200, gin.H{"active": true, "user": service.CanvasUser{ID: user.Id, Subject: grant.Subject, Username: user.Username, DisplayName: user.DisplayName}, "expires_at": grant.ExpiresAt})
}

func RevokeInfiniteCanvas(c *gin.Context) {
	if !canvasClientRequest(c) {
		return
	}
	var request struct {
		GrantID string `json:"grant_id"`
	}
	if common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4096), &request) != nil {
		c.JSON(400, gin.H{"error": "invalid_revocation_request"})
		return
	}
	if err := service.RevokeCanvasGrant(c.Request.Context(), request.GrantID); err != nil {
		canvasError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func canvasHistoryIdentity(c *gin.Context) (*service.CanvasGrantState, bool) {
	if !canvasClientRequest(c) {
		return nil, false
	}
	grant, _, err := service.GetCanvasGrant(c.Request.Context(), c.GetString("canvas_request_grant"), false)
	if err != nil {
		canvasError(c, err)
		return nil, false
	}
	return grant, true
}

// canvasHistoricalOwner checks both immutable application provenance and the
// current authorized user. Old credentials are never activated for this read.
func canvasHistoricalOwner(c *gin.Context, grant *service.CanvasGrantState, tokenID, userID int) bool {
	if userID != grant.Identity.UserID {
		c.Status(404)
		return false
	}
	var record model.CanvasGrant
	if err := model.DB.WithContext(c.Request.Context()).Where("token_id = ? AND user_id = ? AND client_id = ? AND subject = ?", tokenID, userID, grant.ClientID, grant.Subject).Take(&record).Error; err != nil {
		c.Status(404)
		return false
	}
	return true
}

func CanvasHistoryTask(c *gin.Context) {
	grant, ok := canvasHistoryIdentity(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	kind := c.Param("kind")
	if kind == "media" && strings.HasPrefix(c.Param("task_id"), "asyncimg_") {
		kind = "images"
	}
	switch kind {
	case "images":
		var task model.AsyncImageTask
		if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", c.Param("task_id"), grant.Identity.UserID).Take(&task).Error; err != nil {
			c.Status(404)
			return
		}
		if !canvasHistoricalOwner(c, grant, task.TokenId, task.UserId) {
			return
		}
		if task.ExpiresAt > 0 && task.ExpiresAt <= time.Now().Unix() {
			c.Status(404)
			return
		}
		data := make([]gin.H, 0, task.ResultCount)
		if task.ResultsAvailable() {
			for index := range task.ResultCount {
				data = append(data, gin.H{"url": service.CanvasContentPath("images", task.TaskId, strconv.Itoa(index))})
			}
		}
		c.JSON(200, gin.H{"task_id": task.TaskId, "status": task.DisplayStatus(), "progress": task.Progress, "billing_status": task.BillingStatus, "quota": task.Quota, "error_message": service.SanitizeAsyncImageDiagnostic(task.ErrorMessage, common.LocalLogContentLimit), "data": data})
	case "media":
		var job model.AsyncMediaJob
		if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", c.Param("task_id"), grant.Identity.UserID).Take(&job).Error; err != nil {
			c.Status(404)
			return
		}
		if !canvasHistoricalOwner(c, grant, job.TokenId, job.UserId) {
			return
		}
		if job.ExpiresAt <= time.Now().Unix() {
			c.Status(404)
			return
		}
		var objects []model.MediaArtifactObject
		if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ? AND token_id = ? AND status = ? AND expires_at > ?", job.TaskId, job.UserId, job.TokenId, "active", time.Now().Unix()).Find(&objects).Error; err != nil {
			c.Status(503)
			return
		}
		data := make([]gin.H, 0, len(objects))
		for _, object := range objects {
			data = append(data, gin.H{"key": object.ArtifactKey, "url": service.CanvasContentPath("media", job.TaskId, object.ArtifactKey), "content_type": object.MimeType})
		}
		c.JSON(200, gin.H{"task_id": job.TaskId, "status": job.Status, "billing_status": job.BillingStatus, "storage_status": job.StorageStatus, "quota": job.Quota, "data": data})
	default:
		c.Status(404)
	}
}

func CanvasInputContent(c *gin.Context) {
	canvasResponseHeaders(c)
	ctx := c.Request.Context()
	grant, err := service.ValidateCanvasInputAccess(ctx, c.Param("input_id"), c.Query("grant"), c.Query("expires"), c.Query("signature"))
	if err != nil {
		c.Status(404)
		return
	}
	var input model.ImageInputObject
	if err := model.DB.WithContext(ctx).Where("input_id = ? AND token_id = ? AND status = ? AND expires_at > ?", c.Param("input_id"), grant.TokenID, "active", time.Now().Unix()).Take(&input).Error; err != nil {
		c.Status(404)
		return
	}
	var object model.ImageStorageObject
	if err := model.DB.WithContext(ctx).Where("object_id = ? AND status = ? AND (expires_at = 0 OR expires_at > ?)", input.ObjectId, "active", time.Now().Unix()).Take(&object).Error; err != nil {
		c.Status(404)
		return
	}
	var intent model.ImageUploadIntent
	if err := model.DB.WithContext(ctx).Where("intent_key = ? AND token_id = ? AND user_id = ? AND profile_id = ? AND object_key = ?", input.IntentKey, grant.TokenID, grant.Identity.UserID, object.ProfileId, object.ObjectKey).Take(&intent).Error; err != nil {
		c.Status(404)
		return
	}
	store, err := service.GetImageObjectStorage(ctx, object)
	if err != nil {
		c.Status(503)
		return
	}
	if store.Profile.Backend == "local" {
		filename, err := store.LocalPath(object.ObjectKey, false)
		if err != nil {
			c.Status(404)
			return
		}
		c.Header("Content-Type", object.ContentType)
		c.File(filename)
		return
	}
	cfg, err := service.GetImageRuntimeConfig(ctx)
	if err != nil {
		c.Status(503)
		return
	}
	if object.ByteSize > cfg.DownloadMaxBytes {
		c.Status(413)
		return
	}
	if c.Request.Method == http.MethodHead {
		c.Header("Content-Type", object.ContentType)
		c.Header("Content-Length", strconv.FormatInt(object.ByteSize, 10))
		c.Status(200)
		return
	}
	data, err := store.Read(ctx, object.ObjectKey, cfg.DownloadMaxBytes)
	if err != nil {
		c.Status(503)
		return
	}
	c.Data(200, object.ContentType, data)
}

func CanvasHistoryContent(c *gin.Context) {
	grant, ok := canvasHistoryIdentity(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	switch c.Param("kind") {
	case "images":
		var task model.AsyncImageTask
		if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", c.Param("task_id"), grant.Identity.UserID).Take(&task).Error; err != nil {
			c.Status(404)
			return
		}
		if !canvasHistoricalOwner(c, grant, task.TokenId, task.UserId) {
			return
		}
		if !task.ResultsAvailable() || task.ExpiresAt > 0 && task.ExpiresAt <= time.Now().Unix() {
			c.Status(404)
			return
		}
		index, err := strconv.Atoi(c.Param("artifact"))
		if err != nil || index < 0 || index >= task.ResultCount {
			c.Status(404)
			return
		}
		if task.ResultSource == model.ImageResultSourceUpstream {
			urls, err := service.AsyncImageUpstreamResultURLs(ctx, task)
			if err != nil || index >= len(urls) {
				c.Status(503)
				return
			}
			cfg, err := service.GetImageRuntimeConfig(ctx)
			if err != nil {
				c.Status(503)
				return
			}
			// Reuse the reference downloader's DNS, connected-peer, redirect,
			// byte and pixel limits. Application credentials never enter it.
			image, err := service.DownloadImageReference(ctx, urls[index], cfg)
			if err != nil {
				c.Status(503)
				return
			}
			c.Header("Content-Type", image.ContentType)
			c.Header("Content-Length", strconv.Itoa(len(image.Data)))
			if c.Request.Method == http.MethodHead {
				c.Status(200)
				return
			}
			c.Data(200, image.ContentType, image.Data)
			return
		}
		var result model.AsyncImageResult
		if err := model.DB.WithContext(ctx).Where("task_id = ? AND image_index = ?", task.TaskId, index).Take(&result).Error; err != nil {
			c.Status(404)
			return
		}
		var object model.ImageStorageObject
		if err := model.DB.WithContext(ctx).Where("object_id = ? AND status = ? AND (expires_at = 0 OR expires_at > ?)", result.ObjectId, "active", time.Now().Unix()).Take(&object).Error; err != nil {
			c.Status(404)
			return
		}
		store, err := service.GetImageObjectStorage(ctx, object)
		if err != nil {
			c.Status(503)
			return
		}
		if store.Profile.Backend == "local" {
			filename, err := store.LocalPath(object.ObjectKey, false)
			if err != nil {
				c.Status(404)
				return
			}
			c.Header("Content-Type", object.ContentType)
			c.File(filename)
			return
		}
		cfg, err := service.GetImageRuntimeConfig(ctx)
		if err != nil {
			c.Status(503)
			return
		}
		if object.ByteSize > cfg.DownloadMaxBytes {
			c.Status(413)
			return
		}
		if c.Request.Method == http.MethodHead {
			c.Header("Content-Type", object.ContentType)
			c.Header("Content-Length", strconv.FormatInt(object.ByteSize, 10))
			c.Status(200)
			return
		}
		data, err := store.Read(ctx, object.ObjectKey, cfg.DownloadMaxBytes)
		if err != nil {
			c.Status(503)
			return
		}
		c.Data(200, object.ContentType, data)
	case "media":
		var object model.MediaArtifactObject
		if err := model.DB.WithContext(ctx).Where("task_id = ? AND artifact_key = ? AND user_id = ? AND status = ? AND expires_at > ?", c.Param("task_id"), c.Param("artifact"), grant.Identity.UserID, "active", time.Now().Unix()).Take(&object).Error; err != nil {
			c.Status(404)
			return
		}
		if !canvasHistoricalOwner(c, grant, object.TokenId, object.UserId) {
			return
		}
		store := service.GetTaskArtifactStore()
		task := &model.Task{TaskID: object.TaskId, UserId: object.UserId}
		ref, err := store.Resolve(task, object.ArtifactKey)
		if err != nil || ref == nil {
			c.Status(404)
			return
		}
		if err := store.Serve(c, task, ref); err != nil && !c.Writer.Written() {
			c.Status(404)
		}
	default:
		c.Status(404)
	}
}
