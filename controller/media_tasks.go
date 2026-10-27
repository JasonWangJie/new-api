package controller

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// mediaTaskSearchPattern keeps keywords literal and matches ASCII names
// without depending on a database's default LIKE collation.
func mediaTaskSearchPattern(term string) string {
	term = strings.TrimSpace(term)
	if term == "" {
		return ""
	}
	return "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(strings.ToLower(term)) + "%"
}

func mediaStage(status string) string {
	switch status {
	case "queued":
		return "queued"
	case "invoking", "submitting", "submitted":
		return "generating"
	case "upstream_succeeded", "uploading", "billing_pending", "storage_failed", "billing_failed":
		return "saving"
	case "succeeded":
		return "completed"
	default:
		return "failed"
	}
}

func QueryAsyncMedia(c *gin.Context) {
	if strings.HasPrefix(c.Param("task_id"), "asyncimg_") {
		QueryAsyncImage(c)
		return
	}
	var job model.AsyncMediaJob
	query := model.DB.WithContext(c.Request.Context()).Where("task_id = ? AND user_id = ?", c.Param("task_id"), c.GetInt("id"))
	grantValue, canvas := c.Get(service.CanvasGrantContextKey)
	if !canvas {
		query = query.Where("token_id = ?", c.GetInt("token_id"))
	}
	if err := query.Take(&job).Error; err != nil {
		AsyncImagePublicError(c, 404, "task_not_found", "Media task was not found")
		return
	}
	if canvas {
		grant, ok := grantValue.(*service.CanvasGrantState)
		if !ok || grant == nil || !canvasHistoricalOwner(c, grant, job.TokenId, job.UserId) {
			AsyncImagePublicError(c, 404, "task_not_found", "Media task was not found")
			return
		}
	}
	view, results, err := videoJobView(c, job, false)
	if err != nil {
		AsyncImagePublicError(c, 503, "storage_unavailable", "Media results are unavailable")
		return
	}
	c.Header("Cache-Control", "no-store")
	view["data"] = results
	c.JSON(http.StatusOK, view)
}

func videoJobView(c *gin.Context, job model.AsyncMediaJob, admin bool) (gin.H, []gin.H, error) {
	managed, ownerErr := service.CanvasTaskOwnerManaged(c.Request.Context(), job.TokenId, job.UserId)
	if ownerErr != nil && !errors.Is(ownerErr, service.ErrCanvasAuthorization) && !errors.Is(ownerErr, model.ErrCanvasBillingOwner) {
		return nil, nil, ownerErr
	}
	_, canvas := c.Get(service.CanvasGrantContextKey)
	now := time.Now().Unix()
	status, progress := job.Status, 0
	if status == "execution_unknown" {
		status = "failed"
	}
	if status == "submitting" || status == "submitted" {
		status = "invoking"
		progress = 10
	}
	if job.ReferenceRecoveryStage == "prepare" || job.ReferenceRecoveryStage == "submit" || job.ReferenceRecoveryStage == "dispatched" || job.ReferenceRecoveryStage == "refund" {
		status, progress = "queued", 0
	}
	if job.StorageStatus == "ready" || job.StorageStatus == "saving" {
		status = "uploading"
		progress = 80
	}
	if job.StorageStatus == "failed" {
		status = "storage_failed"
		progress = 80
	}
	if job.Status == "succeeded" {
		status = "succeeded"
		progress = 100
	}
	if job.ExpiresAt <= time.Now().Unix() {
		status, progress = "expired", 100
	}
	canTerminate := job.LeaseExpiresAt <= time.Now().Unix() &&
		(job.Status == "queued" && job.DispatchedAt == 0 && job.AttemptGeneration == 0 || admin && job.Status == "execution_unknown")
	view := gin.H{"id": job.TaskId, "task_id": job.TaskId, "media_type": "video", "platform": job.Provider, "provider": job.Provider, "protocol": "openai_video", "model": job.Model, "request_type": "text_to_video", "status": status, "stage": mediaStage(status), "progress": progress, "billing_status": job.BillingStatus, "storage_status": job.StorageStatus, "quota": job.Quota, "cost": float64(job.Quota) / common.QuotaPerUnit, "group": job.Group, "api_key_id": job.TokenId, "image_count": 0, "result_count": 0, "created_at": job.CreatedAt, "started_at": job.DispatchedAt, "finished_at": int64(0), "expires_at": job.ExpiresAt, "next_attempt_at": job.NextAttemptAt, "retry_count": 0, "storage_retry_count": job.StorageRetries, "error_message": job.ErrorMessage, "error_code": "", "prompt_summary": "", "requested_size": "", "actual_size": "", "aspect_ratio": "", "can_resume": false, "can_terminate": canTerminate, "storage_providers": []string{"local"}}
	view["retry_count"] = job.ReferenceRetryCount
	if admin {
		view["user_id"], view["channel_id"] = job.UserId, job.ChannelId
	}
	var token model.Token
	if err := model.DB.WithContext(c.Request.Context()).Select("name").Where("id = ? AND user_id = ?", job.TokenId, job.UserId).Take(&token).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, err
	}
	view["api_key_name"] = token.Name
	if job.ExpiresAt <= time.Now().Unix() {
		view["can_resume"], view["can_terminate"] = false, false
	}
	view["duration_ms"], view["updated_at"], view["upstream_succeeded_at"] = nil, job.CreatedAt, int64(0)
	if admin {
		var user model.User
		if err := model.DB.WithContext(c.Request.Context()).Select("username,display_name").Where("id = ?", job.UserId).Take(&user).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, err
		}
		view["user_name"] = user.DisplayName
		if user.DisplayName == "" {
			view["user_name"] = user.Username
		}
		var selected model.Channel
		if err := model.DB.WithContext(c.Request.Context()).Select("name").Where("id = ?", job.ChannelId).Take(&selected).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, err
		}
		view["channel_name"] = selected.Name
	}
	var task model.Task
	err := model.DB.WithContext(c.Request.Context()).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&task).Error
	if err == nil {
		if job.Status == "execution_unknown" {
			view["can_terminate"] = admin && job.ReferenceRecoveryStage == "unknown" && task.AttemptGeneration == job.AttemptGeneration && task.Status == model.TaskStatusUnknown && job.LeaseExpiresAt <= now
		}
		view["can_resume"] = task.Status == model.TaskStatusSuccess && job.ExpiresAt > time.Now().Unix() && job.LeaseExpiresAt <= time.Now().Unix() &&
			(job.Status == "submitted" && job.StorageStatus == "failed" || job.Status == "succeeded" && job.StorageStatus == "upstream")
		view["updated_at"] = task.UpdatedAt
		if upstreamProgress, ok := service.PublicTaskUpstreamProgress(&task); ok {
			view["upstream_progress"] = upstreamProgress
		}
		if job.Provider == "drama" && (job.Status == "submitting" || job.Status == "submitted") && job.StorageStatus == "pending" && job.ExpiresAt > now &&
			slices.Contains([]model.TaskStatus{model.TaskStatusNotStart, model.TaskStatusSubmitted, model.TaskStatusQueued}, task.Status) {
			view["status"], view["stage"] = "queued", "queued"
		}
		if job.StorageStatus == "pending" && task.Status != model.TaskStatusSuccess && task.Status != model.TaskStatusFailure {
			if percent, err := strconv.Atoi(strings.TrimSuffix(task.Progress, "%")); err == nil {
				view["progress"] = min(69, max(10, min(100, max(0, percent))*70/100))
			}
		}
		view["quota"], view["cost"], view["finished_at"] = task.Quota, float64(task.Quota)/common.QuotaPerUnit, task.FinishTime
		view["request_type"] = task.Action
		if task.FinishTime > 0 {
			view["duration_ms"] = max(0, task.FinishTime-job.CreatedAt) * 1000
		}
		if task.Status == model.TaskStatusSuccess {
			view["upstream_succeeded_at"] = task.FinishTime
			if job.BillingStatus != "failed" {
				view["billing_status"] = "settled"
			}
		}
		if task.Status == model.TaskStatusSuccess && job.StorageStatus == "pending" {
			view["stage"], view["status"], view["progress"] = "saving", "upstream_succeeded", 70
		}
		if task.Status == model.TaskStatusFailure {
			view["status"], view["stage"], view["error_message"] = "failed", "failed", service.PublicTaskFailureReason(&task)
			if job.BillingStatus != "failed" {
				view["billing_status"] = "failed"
				if task.Quota == 0 {
					view["billing_status"] = "refunded"
				}
			}
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, err
	}
	if job.Provider == "drama" && job.ExpiresAt <= now {
		view["status"], view["stage"], view["progress"] = "expired", "failed", 100
	}
	if (job.Status == "execution_unknown" || job.BillingStatus == "unknown") && (errors.Is(err, gorm.ErrRecordNotFound) || job.ReferenceRecoveryStage == "unknown") {
		view["error_code"], view["error_retryable"] = "execution_unknown", false
	}
	if status, _ := view["status"].(string); slices.Contains([]string{"failed", "execution_unknown", "storage_failed", "billing_failed", "expired"}, status) || job.BillingStatus == "failed" {
		reason, _ := view["error_message"].(string)
		if job.Status == "execution_unknown" && strings.TrimSpace(reason) == "" {
			reason = "execution_unknown"
		}
		if reason != "" || status == "failed" {
			notice := service.VideoTaskFailureNotice(reason)
			view["error_summary"], view["error_suggestion"] = notice.Summary, notice.Suggestion
			view["error_detail"] = notice.Detail
			view["error_message"] = service.VideoTaskFailureReason(reason)
		}
	}
	results := make([]gin.H, 0)
	if ownerErr != nil {
		return view, results, nil
	}
	if job.StorageStatus == "upstream" && job.ExpiresAt > time.Now().Unix() {
		if link := service.PublicUpstreamAsyncVideoResultURL(&task); link != "" {
			item := gin.H{"id": job.ID, "image_index": 0, "key": "video", "url": link, "view_url": link, "source": "upstream", "content_type": "video/mp4", "width": 0, "height": 0, "byte_size": 0, "checksum": "", "expires_at": job.ExpiresAt}
			if managed {
				link = mediaTaskResultPath(job.TaskId, "video", admin)
				if canvas {
					link = service.CanvasContentPath("media", job.TaskId, "video")
				}
				item["url"], item["view_url"], item["authenticated"] = link, link, true
			}
			results = append(results, item)
			view["result_count"] = 1
		}
		return view, results, nil
	}
	// A partial manifest remains hidden until every generated video is saved.
	if job.StorageStatus != "succeeded" || job.ExpiresAt <= time.Now().Unix() {
		return view, results, nil
	}
	cfg, err := service.GetMediaRuntimeConfig(c.Request.Context())
	if err != nil {
		return nil, nil, err
	}
	var objects []model.MediaArtifactObject
	if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ? AND user_id = ? AND status = ? AND expires_at > ?", job.TaskId, job.UserId, "active", time.Now().Unix()).Order("artifact_key").Find(&objects).Error; err != nil {
		return nil, nil, err
	}
	for i, object := range objects {
		link := ""
		if managed {
			link = mediaTaskResultPath(job.TaskId, object.ArtifactKey, admin)
			if canvas {
				link = service.CanvasContentPath("media", job.TaskId, object.ArtifactKey)
			}
		} else {
			link, err = service.MediaObjectURL(object, AsyncImageGatewayBase(), cfg.SignedURLExpiry)
			if err != nil {
				return nil, nil, err
			}
		}
		item := gin.H{"id": object.ID, "image_index": i, "key": object.ArtifactKey, "url": link, "view_url": link, "content_type": object.MimeType, "width": 0, "height": 0, "byte_size": object.ByteSize, "checksum": object.Checksum, "expires_at": min(object.ExpiresAt, time.Now().Unix()+int64(cfg.SignedURLExpiry))}
		if managed {
			item["authenticated"] = true
		}
		results = append(results, item)
	}
	view["result_count"] = len(results)
	if len(results) == 0 {
		view["status"], view["stage"], view["storage_status"] = "expired", "failed", "expired"
	}
	return view, results, nil
}

func mediaTaskResultPath(taskID, artifact string, admin bool) string {
	base := "/api/user/media-tasks/"
	if admin {
		base = "/api/admin/media-tasks/"
	}
	return base + url.PathEscape(taskID) + "/results/" + url.PathEscape(artifact) + "/view"
}

// ViewMediaTaskResult serves dashboard results under the current user session.
// Managed outputs use this route instead of anonymous storage capabilities.
func ViewMediaTaskResult(c *gin.Context, admin bool) {
	query := model.DB.WithContext(c.Request.Context()).Where("task_id = ?", c.Param("task_id"))
	if !admin {
		query = query.Where("user_id = ?", c.GetInt("id"))
	}
	var job model.AsyncMediaJob
	if err := query.Take(&job).Error; err != nil {
		imageManagementError(c, http.StatusNotFound, errors.New("Media task result was not found"))
		return
	}
	if managed, err := service.CanvasTaskOwnerManaged(c.Request.Context(), job.TokenId, job.UserId); err != nil || !managed {
		imageManagementError(c, http.StatusNotFound, errors.New("Media task result was not found"))
		return
	}
	serveAuthorizedMediaTaskResult(c, job, c.Param("artifact_key"))
}

// serveAuthorizedMediaTaskResult is shared by Canvas and dashboard reads.
// Callers establish grant or dashboard ownership before entering this function.
func serveAuthorizedMediaTaskResult(c *gin.Context, job model.AsyncMediaJob, artifact string) {
	canvasResponseHeaders(c)
	if job.Status != "succeeded" || job.ExpiresAt <= time.Now().Unix() || job.BillingStatus == "failed" || !taskArtifactKeyPattern.MatchString(artifact) {
		c.Status(http.StatusNotFound)
		return
	}
	task := &model.Task{TaskID: job.TaskId, UserId: job.UserId}
	if job.StorageStatus == "upstream" && artifact == "video" {
		if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ? AND user_id = ? AND status = ?", job.TaskId, job.UserId, model.TaskStatusSuccess).Take(task).Error; err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		if link := service.PublicUpstreamAsyncVideoResultURL(task); link != "" {
			descriptor := &relaychannel.TaskContentRequest{URL: link, Method: c.Request.Method, Credentialless: true}
			if err := proxyTaskMedia(c, task, descriptor); err != nil && !c.Writer.Written() {
				writeTaskMediaProxyError(c, err)
			}
			return
		}
	}
	if job.StorageStatus != "succeeded" {
		c.Status(http.StatusNotFound)
		return
	}
	var object model.MediaArtifactObject
	if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ? AND artifact_key = ? AND user_id = ? AND token_id = ? AND status = ? AND expires_at > ?", job.TaskId, artifact, job.UserId, job.TokenId, "active", time.Now().Unix()).Take(&object).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	store := service.GetTaskArtifactStore()
	ref, err := store.Resolve(task, artifact)
	if err != nil || ref == nil {
		c.Status(http.StatusNotFound)
		return
	}
	if err := store.Serve(c, task, ref); err != nil && !c.Writer.Written() {
		c.Status(http.StatusNotFound)
	}
}

func MediaObjectContent(c *gin.Context) {
	var object model.MediaArtifactObject
	if err := model.DB.WithContext(c.Request.Context()).Where("object_id = ? AND status = ? AND expires_at > ?", c.Param("object_id"), "active", time.Now().Unix()).Take(&object).Error; err != nil || !service.VerifyMediaObjectAccess(object, c.Query("expires"), c.Query("access")) {
		AsyncImagePublicError(c, 404, "object_not_found", "Media object was not found")
		return
	}
	var token model.Token
	if err := model.DB.WithContext(c.Request.Context()).Where("id = ? AND user_id = ?", object.TokenId, object.UserId).Take(&token).Error; err != nil || validateMediaToken(c.Request.Context(), token, c.ClientIP()) != nil {
		AsyncImagePublicError(c, 404, "object_not_found", "Media object was not found")
		return
	}
	var job model.AsyncMediaJob
	if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ? AND user_id = ? AND token_id = ? AND storage_status = ?", object.TaskId, object.UserId, object.TokenId, "succeeded").Take(&job).Error; err != nil {
		AsyncImagePublicError(c, 404, "object_not_found", "Media object was not found")
		return
	}
	store := service.GetTaskArtifactStore()
	task := &model.Task{TaskID: object.TaskId, UserId: object.UserId}
	ref, err := store.Resolve(task, object.ArtifactKey)
	if err != nil || ref == nil {
		AsyncImagePublicError(c, 404, "object_not_found", "Media object was not found")
		return
	}
	if err := store.Serve(c, task, ref); err != nil && !c.Writer.Written() {
		AsyncImagePublicError(c, 404, "object_not_found", "Media object was not found")
	}
}

func GetMediaConfiguration(c *gin.Context) {
	cfg, err := service.GetMediaRuntimeConfig(c.Request.Context())
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	imageManagementData(c, cfg)
}

func UpdateMediaConfiguration(c *gin.Context) {
	var cfg service.MediaRuntimeConfig
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &cfg); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	if err := service.SaveMediaRuntimeConfig(c.Request.Context(), cfg); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	imageManagementData(c, cfg)
}

func GetMediaTask(c *gin.Context, admin bool) {
	if strings.HasPrefix(c.Param("task_id"), "asyncimg_") {
		GetImageTask(c, admin)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	query := model.DB.WithContext(c.Request.Context()).Where("task_id = ?", c.Param("task_id"))
	if !admin {
		query = query.Where("user_id = ?", c.GetInt("id"))
	}
	var job model.AsyncMediaJob
	if err := query.Take(&job).Error; err != nil {
		imageManagementError(c, 404, errors.New("Media task was not found"))
		return
	}
	if _, err := service.CanvasTaskOwnerManaged(c.Request.Context(), job.TokenId, job.UserId); err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, service.ErrCanvasAuthorization) || errors.Is(err, model.ErrCanvasBillingOwner) {
			status = http.StatusNotFound
		}
		imageManagementError(c, status, errors.New("Media task is unavailable"))
		return
	}
	view, results, err := videoJobView(c, job, admin)
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	// Complete request parameters and caller-owned signed reference URLs remain
	// owner/admin detail data. Management lists project specifications only.
	if details := videoJobRequestDetails(job); details != nil {
		if details.RequestedSeconds != nil {
			view["requested_seconds"] = *details.RequestedSeconds
		}
		if details.RequestedResolution != "" {
			view["requested_resolution"] = details.RequestedResolution
		}
		if len(details.ReferenceMedia) > 0 {
			view["reference_media"] = details.ReferenceMedia
		}
		view["request_parameters"] = details.RequestParameters
		if prompt, ok := details.RequestParameters["prompt"].(string); ok {
			view["prompt_summary"] = service.SanitizeAsyncImageText(prompt, 512)
		}
	}
	events, err := videoTaskAudit(c, job, view, admin)
	if err != nil {
		imageManagementError(c, http.StatusServiceUnavailable, err)
		return
	}
	imageManagementData(c, gin.H{"task": view, "results": results, "events": events})
}

// videoJobRequestDetails uses the already-loaded encrypted request, without
// additional database reads or inferring missing historical request settings.
func videoJobRequestDetails(job model.AsyncMediaJob) *service.AsyncVideoRequestDetails {
	if job.ExpiresAt <= time.Now().Unix() || len(job.RequestCipher) == 0 {
		return nil
	}
	plain, err := service.DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
	if err != nil {
		return nil
	}
	var request service.AsyncVideoRequest
	if common.Unmarshal(plain, &request) != nil {
		return nil
	}
	return request.RequestDetails()
}

// videoTaskAudit projects only persisted milestones. In particular, a native
// receipt is acceptance, not proof that generation succeeded, and an old task
// without a dispatch record must not acquire a fictitious account attempt.
func videoTaskAudit(c *gin.Context, job model.AsyncMediaJob, view gin.H, admin bool) ([]model.AsyncMediaEvent, error) {
	events, err := model.ListAsyncMediaEvents(c.Request.Context(), job.TaskId)
	if err != nil {
		return nil, err
	}
	if !admin {
		for i := range events {
			events[i].Message = ""
		}
		return events, nil
	}
	view["reconciliation_status"] = view["billing_status"]
	switch view["billing_status"] {
	case "unknown":
		view["reconciliation_status"] = "upstream_unconfirmed"
	case "not_billable":
		view["reconciliation_status"] = "not_charged"
	case "failed":
		view["reconciliation_status"] = "billing_failed"
	}
	attempts := make([]gin.H, 0)
	positions := make(map[int64]int)
	channelIDs := make([]int, 0)
	completeHistory := false
	for _, event := range events {
		if event.EventType == "accepted" {
			completeHistory = true
		}
		position, exists := positions[event.AttemptGeneration]
		if event.EventType == "attempt_started" || event.EventType == "dispatched" {
			if !exists {
				position = len(attempts)
				positions[event.AttemptGeneration] = position
				attempts = append(attempts, gin.H{"channel_name": "", "channel_id": event.ChannelId, "key_index": nil, "started_at": event.CreatedAt, "finished_at": int64(0), "dispatched": false, "reference_mode": "", "error_code": 0, "status": "selected"})
				channelIDs = append(channelIDs, event.ChannelId)
			}
		} else if !exists {
			continue
		}
		attempt := attempts[position]
		switch event.EventType {
		case "dispatched":
			attempt["dispatched"], attempt["reference_mode"] = true, event.ReferenceMode
			attempt["key_index"] = event.KeyIndex
			attempt["status"] = "processing"
		case "submitted":
			if attempt["finished_at"] == int64(0) {
				attempt["status"] = "accepted"
			}
		case "upstream_succeeded":
			attempt["status"], attempt["finished_at"], attempt["error_code"] = "succeeded", event.CreatedAt, 0
		case "upstream_failed":
			attempt["status"], attempt["finished_at"] = "failed", event.CreatedAt
		case "execution_unknown", "billing_unknown", "expired":
			if attempt["status"] != "succeeded" && attempt["status"] != "failed" {
				attempt["status"], attempt["finished_at"] = "execution_unknown", event.CreatedAt
			}
		case "failed":
			if attempt["finished_at"] == int64(0) {
				status := "failed"
				if view["billing_status"] == "unknown" {
					status = "execution_unknown"
				}
				attempt["status"], attempt["finished_at"] = status, event.CreatedAt
			}
		}
	}
	if len(channelIDs) > 0 {
		var native model.Task
		if err := model.DB.WithContext(c.Request.Context()).Select("private_data", "attempt_generation", "fail_reason").Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&native).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		for _, previous := range native.PrivateData.VideoAttempts {
			if position, exists := positions[previous.Generation]; exists && strings.TrimSpace(previous.Reason) != "" {
				attempts[position]["error_message"] = service.VideoTaskFailureReason(previous.Reason)
			}
		}
		if position, exists := positions[job.AttemptGeneration]; exists {
			attempt := attempts[position]
			if attempt["status"] == "failed" || attempt["status"] == "execution_unknown" {
				reason := job.ErrorMessage
				if native.AttemptGeneration == job.AttemptGeneration && strings.TrimSpace(native.FailReason) != "" {
					reason = native.FailReason
				}
				if strings.TrimSpace(reason) != "" {
					attempt["error_message"] = service.VideoTaskFailureReason(reason)
				}
			}
		}
		var channels []model.Channel
		if err := model.DB.WithContext(c.Request.Context()).Select("id", "name").Where("id IN ?", channelIDs).Find(&channels).Error; err != nil {
			return nil, err
		}
		names := make(map[int]string, len(channels))
		for _, channel := range channels {
			names[channel.Id] = channel.Name
		}
		for _, attempt := range attempts {
			attempt["channel_name"] = names[attempt["channel_id"].(int)]
			delete(attempt, "channel_id")
		}
	}
	view["attempt_history"], view["attempt_history_unavailable"] = attempts, !completeHistory
	return events, nil
}

func ManageMediaTask(c *gin.Context, admin bool, action string) {
	if strings.HasPrefix(c.Param("task_id"), "asyncimg_") {
		if admin {
			ManageImageTask(c, action)
			return
		}
		if action != "resume" {
			imageManagementError(c, 403, errors.New("Permission denied"))
			return
		}
		var task model.AsyncImageTask
		if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ? AND user_id = ?", c.Param("task_id"), c.GetInt("id")).Take(&task).Error; err != nil {
			imageManagementError(c, 404, errors.New("Image task was not found"))
			return
		}
		if _, err := service.CanvasImageTaskManaged(c.Request.Context(), task); err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, service.ErrCanvasAuthorization) || errors.Is(err, model.ErrCanvasBillingOwner) {
				status = http.StatusNotFound
			}
			imageManagementError(c, status, errors.New("Image task is unavailable"))
			return
		}
		if task.ErrorCode == "billing_quota_uncertain" {
			imageManagementError(c, http.StatusConflict, errors.New("Quota reservation reconciliation by an administrator is required before resuming billing"))
			return
		}
		if err := model.ResumeAsyncImageTask(c.Request.Context(), task); err != nil {
			status := 503
			if errors.Is(err, model.ErrImageConflict) {
				status = 409
			}
			imageManagementError(c, status, err)
			return
		}
		imageManagementData(c, gin.H{"task_id": task.TaskId})
		return
	}
	query := model.DB.WithContext(c.Request.Context()).Model(&model.AsyncMediaJob{}).Where("task_id = ? AND lease_expires_at <= ?", c.Param("task_id"), time.Now().Unix())
	if admin {
		query = query.Where("expires_at > ? OR (status = ? AND reference_retry_count = ? AND reference_recovery_stage = ?)", time.Now().Unix(), "execution_unknown", 1, "unknown")
	} else {
		query = query.Where("expires_at > ?", time.Now().Unix())
	}
	if !admin {
		query = query.Where("user_id = ?", c.GetInt("id"))
		var job model.AsyncMediaJob
		if err := query.Session(&gorm.Session{}).Take(&job).Error; err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, gorm.ErrRecordNotFound) {
				status = http.StatusConflict
			}
			imageManagementError(c, status, errors.New("Media task cannot be changed in its current phase"))
			return
		}
		if _, err := service.CanvasTaskOwnerManaged(c.Request.Context(), job.TokenId, job.UserId); err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, service.ErrCanvasAuthorization) || errors.Is(err, model.ErrCanvasBillingOwner) {
				status = http.StatusNotFound
			}
			imageManagementError(c, status, errors.New("Media task is unavailable"))
			return
		}
	}
	updates := map[string]any{}
	var reconciledTask *model.Task
	switch action {
	case "resume":
		query = query.Where("((status = ? AND storage_status = ?) OR (status = ? AND storage_status = ?))", "submitted", "failed", "succeeded", "upstream").
			Where("EXISTS (SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status = ?)", model.TaskStatusSuccess)
		updates["status"], updates["storage_status"] = "submitted", "failed"
		updates["next_attempt_at"], updates["storage_retries"], updates["error_message"] = 0, 0, ""
	case "terminate":
		var job model.AsyncMediaJob
		if err := query.Session(&gorm.Session{}).Take(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				imageManagementError(c, http.StatusConflict, errors.New("Media task cannot be changed in its current phase"))
			} else {
				imageManagementError(c, http.StatusServiceUnavailable, err)
			}
			return
		}
		if job.Status == "execution_unknown" {
			if !admin {
				imageManagementError(c, http.StatusForbidden, errors.New("Permission denied"))
				return
			}
			var confirmation struct {
				UpstreamChecked bool `json:"confirm_upstream_checked"`
			}
			if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &confirmation); err != nil || !confirmation.UpstreamChecked {
				imageManagementError(c, http.StatusConflict, errors.New("Verify the upstream task before terminating local processing"))
				return
			}
			// Unknown submission does not prove whether upstream billing occurred.
			reason := "Terminated by administrator after upstream verification"
			if detail := service.VideoTaskFailureNotice(job.ErrorMessage).Detail; detail != "" {
				reason += "\n" + detail
			}
			updates["error_message"] = service.VideoTaskFailureReason(reason)
			if job.ReferenceRetryCount == 1 && job.ReferenceRecoveryStage == "unknown" {
				var task model.Task
				if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ? AND user_id = ? AND attempt_generation = ?", job.TaskId, job.UserId, job.AttemptGeneration).Take(&task).Error; err != nil || !task.PrivateData.DurableBilling || task.PrivateData.UpstreamTaskID != "" {
					imageManagementError(c, http.StatusConflict, errors.New("Media task reservation requires reconciliation"))
					return
				}
				if task.Status == model.TaskStatusUnknown {
					task.Status, task.FailReason, task.Progress, task.FinishTime = model.TaskStatusFailure, updates["error_message"].(string), "100%", time.Now().Unix()
					won, settleErr := service.SettleTaskBillingOnComplete(c.Request.Context(), nil, &task, model.TaskStatusUnknown, relaycommon.FailTaskInfo(task.FailReason))
					if settleErr != nil || !won {
						imageManagementError(c, http.StatusConflict, errors.New("Media task reservation could not be reconciled"))
						return
					}
				} else if task.Status != model.TaskStatusFailure || task.Quota != 0 {
					imageManagementError(c, http.StatusConflict, errors.New("Media task cannot be changed in its current phase"))
					return
				}
				reconciledTask = &task
				query = query.Where("id = ? AND attempt_generation = ? AND reference_recovery_stage = ? AND status = ?", job.ID, job.AttemptGeneration, "unknown", "execution_unknown")
				updates["billing_status"], updates["quota"], updates["reference_recovery_stage"] = "refunded", 0, "complete"
			} else {
				query = query.Where("status = ? AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id)", "execution_unknown")
			}
		} else {
			query = query.Where("status = ? AND dispatched_at = 0 AND attempt_generation = 0", "queued")
			updates["billing_status"] = "not_billable"
		}
		updates["status"], updates["request_cipher"] = "failed", nil
		updates["lease_token"], updates["lease_expires_at"], updates["next_attempt_at"] = "", 0, 0
	default:
		imageManagementError(c, 400, errors.New("Invalid media task operation"))
		return
	}
	eventType := "terminated"
	if action == "resume" {
		eventType = "resume_requested"
	}
	if err := model.ChangeAsyncMediaJob(query, updates, eventType); err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, model.ErrImageConflict) || errors.Is(err, gorm.ErrRecordNotFound) {
			status = http.StatusConflict
		}
		imageManagementError(c, status, errors.New("Media task cannot be changed in its current phase"))
		return
	}
	if reconciledTask != nil {
		if err := service.RetireMediaInputObjects(c.Request.Context(), reconciledTask); err != nil {
			common.SysError("media input retirement: " + err.Error())
		}
	}
	imageManagementData(c, gin.H{"task_id": c.Param("task_id")})
}

func GetMediaTaskPlatforms(c *gin.Context) {
	platforms, err := service.ConfiguredMediaTaskPlatforms(c.Request.Context())
	if err != nil {
		imageManagementError(c, http.StatusServiceUnavailable, err)
		return
	}
	imageManagementData(c, platforms)
}

func ListMediaTasks(c *gin.Context, admin bool) {
	page, pageErr := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, sizeErr := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if pageErr != nil || sizeErr != nil || page < 1 || page > math.MaxInt32/100 || size < 1 || size > 100 {
		imageManagementError(c, 400, errors.New("Invalid media task pagination"))
		return
	}
	images, err := imageTaskListQuery(c, admin)
	if err != nil {
		imageManagementError(c, 400, err)
		return
	}
	videos := model.DB.WithContext(c.Request.Context()).Model(&model.AsyncMediaJob{})
	if !admin {
		userID := c.GetInt("id")
		videos = videos.Where("user_id = ?", userID)
		ownerSubject := model.DB.WithContext(c.Request.Context()).Model(&model.User{}).
			Select("COALESCE(canvas_subject, '')").Where("id = ?", userID)
		previousAccounts := model.DB.WithContext(c.Request.Context()).Model(&model.CanvasGrant{}).
			Select("token_id").Where("user_id = ?", userID).Where("subject NOT IN (?)", ownerSubject)
		videos = videos.Where("token_id NOT IN (?)", previousAccounts)
	}
	mediaType := c.Query("media_type")
	if mediaType != "" && mediaType != "image" && mediaType != "video" {
		imageManagementError(c, 400, errors.New("Invalid media type"))
		return
	}
	if mediaType == "image" {
		videos = videos.Where("1 = 0")
	}
	if mediaType == "video" {
		images = images.Where("1 = 0")
	}
	for key, column := range map[string]string{"task_id": "task_id", "group": "group", "api_key_id": "token_id", "user_id": "user_id", "channel_id": "channel_id", "billing_status": "billing_status", "provider": "provider"} {
		if value := c.Query(key); value != "" {
			videos = videos.Where(clause.Eq{Column: column, Value: value})
		}
	}
	if provider := c.Query("provider"); provider != "" {
		images = images.Where("provider = ? OR (provider IS NULL OR provider = '') AND platform = ?", provider, provider)
	}
	if platform := c.Query("platform"); platform != "" {
		videos = videos.Where("provider = ?", platform)
	}
	if protocol := c.Query("protocol"); protocol != "" && protocol != "openai_video" {
		videos = videos.Where("1 = 0")
	}
	if kind := c.Query("request_type"); kind != "" {
		if kind == "video" {
			images = images.Where("1 = 0")
		} else if kind == "text_to_video" {
			videos = videos.Where("NOT EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.action <> ?)", kind)
		} else if kind == "image_to_video" || kind == "remix" || kind == "extend" || kind == "video_to_video" {
			videos = videos.Where("EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.action = ?)", kind)
		} else {
			videos = videos.Where("1 = 0")
		}
	}
	if storage := c.Query("storage_provider"); storage != "" && storage != "local" {
		videos = videos.Where("1 = 0")
	}
	// The media job owns submission/storage scheduling; the linked task owns
	// Drama's upstream queue. Use the same public state for filtering and stats.
	now := time.Now().Unix()
	dramaStatus := gorm.Expr(`CASE
		WHEN expires_at <= ? THEN 'expired'
		WHEN storage_status = 'succeeded' AND NOT EXISTS(SELECT 1 FROM media_artifact_objects
			WHERE media_artifact_objects.task_id = async_media_jobs.task_id
			AND media_artifact_objects.user_id = async_media_jobs.user_id
			AND media_artifact_objects.status = 'active' AND media_artifact_objects.expires_at > ?) THEN 'expired'
		WHEN EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id
			AND tasks.user_id = async_media_jobs.user_id AND tasks.status = 'FAILURE') THEN 'failed'
		WHEN storage_status = 'pending' AND EXISTS(SELECT 1 FROM tasks
			WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id
			AND tasks.status = 'SUCCESS') THEN 'upstream_succeeded'
		WHEN status = 'succeeded' THEN 'succeeded'
		WHEN status = 'execution_unknown' THEN 'failed'
		WHEN storage_status = 'failed' THEN 'storage_failed'
		WHEN storage_status IN ('ready','saving') THEN 'uploading'
		WHEN status IN ('submitting','submitted') AND storage_status = 'pending'
			AND EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id
				AND tasks.user_id = async_media_jobs.user_id
				AND tasks.status IN ('NOT_START','SUBMITTED','QUEUED')) THEN 'queued'
		WHEN status IN ('submitting','submitted') THEN 'invoking'
		ELSE status END`, now, now)
	if status := c.Query("status"); status != "" {
		legacyStatus := model.DB.Where("status = ?", status)
		switch status {
		case "failed":
			legacyStatus = model.DB.Where("status IN ? OR EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status = ?)", []string{"failed", "execution_unknown"}, model.TaskStatusFailure)
		case "invoking":
			legacyStatus = model.DB.Where("status IN ? AND storage_status = ?", []string{"submitting", "submitted"}, "pending")
		case "uploading":
			legacyStatus = model.DB.Where("storage_status IN ?", []string{"ready", "saving"})
		case "storage_failed":
			legacyStatus = model.DB.Where("storage_status = ?", "failed")
		}
		videos = videos.Where(model.DB.Where("provider = ? AND (?) = ?", "drama", dramaStatus, status).
			Or(model.DB.Where("provider IS NULL OR provider <> ?", "drama").Where(legacyStatus)))
	}
	if pattern := mediaTaskSearchPattern(c.Query("model")); pattern != "" {
		videos = videos.Where("LOWER(model) LIKE ? ESCAPE '!'", pattern)
	}
	if pattern := mediaTaskSearchPattern(c.Query("q")); pattern != "" {
		matches := model.DB.WithContext(c.Request.Context()).Where("LOWER(task_id) LIKE ? ESCAPE '!' OR LOWER(model) LIKE ? ESCAPE '!'", pattern, pattern)
		if admin {
			channels := model.DB.WithContext(c.Request.Context()).Model(&model.Channel{}).Select("id").Where("LOWER(name) LIKE ? ESCAPE '!'", pattern)
			matches = matches.Or("channel_id IN (?)", channels)
		}
		videos = videos.Where(matches)
	}
	location, err := time.LoadLocation(c.DefaultQuery("timezone", "UTC"))
	if err != nil {
		imageManagementError(c, 400, err)
		return
	}
	start, end := c.Query("start_date"), c.Query("end_date")
	if start == "" && end == "" {
		start = time.Now().In(location).Format("2006-01-02")
		end = start
	}
	for _, boundary := range []struct {
		value string
		end   bool
	}{{start, false}, {end, true}} {
		if boundary.value == "" {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", boundary.value, location)
		if err != nil {
			imageManagementError(c, 400, err)
			return
		}
		if boundary.end {
			videos = videos.Where("created_at < ?", date.AddDate(0, 0, 1).Unix())
		} else {
			videos = videos.Where("created_at >= ?", date.Unix())
		}
	}
	if stage := c.Query("stage"); stage != "" {
		states := map[string][]string{"queued": {"queued"}, "generating": {"invoking"}, "saving": {"upstream_succeeded", "uploading", "billing_pending", "storage_failed", "billing_failed"}, "completed": {"succeeded"}, "failed": {"failed", "expired", "execution_unknown"}}
		selected, ok := states[stage]
		if !ok {
			imageManagementError(c, 400, errors.New("Invalid media phase"))
			return
		}
		images = images.Where("status IN ?", selected)
		var legacyStage *gorm.DB
		switch stage {
		case "queued":
			legacyStage = model.DB.Where("status = ?", "queued")
		case "generating":
			legacyStage = model.DB.Where("status IN ? AND storage_status = ? AND NOT EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status IN ?)", []string{"submitting", "submitted"}, "pending", []model.TaskStatus{model.TaskStatusSuccess, model.TaskStatusFailure})
		case "saving":
			legacyStage = model.DB.Where("storage_status IN ? OR (storage_status = ? AND EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status = ?))", []string{"ready", "saving", "failed"}, "pending", model.TaskStatusSuccess)
		case "completed":
			legacyStage = model.DB.Where("status = ?", "succeeded")
		case "failed":
			legacyStage = model.DB.Where("status IN ? OR EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status = ?)", []string{"failed", "execution_unknown", "expired"}, model.TaskStatusFailure)
		}
		videos = videos.Where(model.DB.Where("provider = ? AND (?) IN ?", "drama", dramaStatus, selected).
			Or(model.DB.Where("provider IS NULL OR provider <> ?", "drama").Where(legacyStage)))
	}
	videoProjection := "task_id, created_at, CASE WHEN provider = 'drama' THEN (?) WHEN storage_status = 'failed' THEN 'storage_failed' WHEN storage_status IN ('ready','saving') THEN 'uploading' WHEN status = 'execution_unknown' THEN 'failed' WHEN status IN ('submitting','submitted') AND EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status = 'SUCCESS') THEN 'upstream_succeeded' WHEN status IN ('submitting','submitted') THEN 'invoking' ELSE status END AS status, COALESCE((SELECT quota FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id LIMIT 1), quota) AS quota, billing_status, 0 AS image_count, COALESCE((SELECT finish_time FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id LIMIT 1),0) AS finished_at, CASE WHEN provider = 'drama' THEN COALESCE((SELECT updated_at FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id LIMIT 1), created_at) ELSE created_at END AS updated_at, 'video' AS media_type"
	union := model.DB.Raw("? UNION ALL ?", images.Select("task_id, created_at, status, quota, billing_status, image_count, finished_at, updated_at, 'image' AS media_type"), videos.Select(videoProjection, dramaStatus))
	query := model.DB.WithContext(c.Request.Context()).Table("(?) AS media_tasks", union)
	var total int64
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	sortBy := c.DefaultQuery("sort_by", "created_at")
	if !slices.Contains([]string{"created_at", "finished_at", "updated_at", "status", "quota", "image_count"}, sortBy) {
		imageManagementError(c, 400, errors.New("Invalid media task sorting"))
		return
	}
	direction := strings.ToUpper(c.DefaultQuery("sort_order", "desc"))
	if direction != "ASC" && direction != "DESC" {
		imageManagementError(c, 400, errors.New("Invalid media task sorting"))
		return
	}
	var rows []struct {
		TaskId    string
		MediaType string
	}
	if err := query.Session(&gorm.Session{}).Select("task_id,media_type").Order(sortBy + " " + direction).Order("task_id DESC").Offset((page - 1) * size).Limit(size).Scan(&rows).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	imageIDs, videoIDs := make([]string, 0), make([]string, 0)
	for _, row := range rows {
		if row.MediaType == "image" {
			imageIDs = append(imageIDs, row.TaskId)
		} else {
			videoIDs = append(videoIDs, row.TaskId)
		}
	}
	var imageTasks []model.AsyncImageTask
	if len(imageIDs) > 0 {
		if err := model.DB.WithContext(c.Request.Context()).Where("task_id IN ?", imageIDs).Find(&imageTasks).Error; err != nil {
			imageManagementError(c, 503, err)
			return
		}
	}
	items, err := imageTasksToDTO(c.Request.Context(), imageTasks, admin, false)
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	byID := make(map[string]gin.H)
	for _, item := range items {
		item["media_type"] = "image"
		item["stage"] = mediaStage(item["status"].(string))
		byID[item["task_id"].(string)] = item
	}
	var jobs []model.AsyncMediaJob
	if len(videoIDs) > 0 {
		if err := model.DB.WithContext(c.Request.Context()).Where("task_id IN ?", videoIDs).Find(&jobs).Error; err != nil {
			imageManagementError(c, 503, err)
			return
		}
	}
	for _, job := range jobs {
		view, _, err := videoJobView(c, job, admin)
		if err != nil {
			imageManagementError(c, 503, err)
			return
		}
		if details := videoJobRequestDetails(job); details != nil {
			if details.RequestedSeconds != nil {
				view["requested_seconds"] = *details.RequestedSeconds
			}
			if details.RequestedResolution != "" {
				view["requested_resolution"] = details.RequestedResolution
			}
		}
		byID[job.TaskId] = view
	}
	ordered := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		ordered = append(ordered, byID[row.TaskId])
	}
	var stats struct {
		Total             int64    `json:"total"`
		Queued            int64    `json:"queued"`
		Processing        int64    `json:"processing"`
		Succeeded         int64    `json:"succeeded"`
		Failed            int64    `json:"failed"`
		Quota             int64    `json:"quota"`
		SucceededQuota    int64    `json:"-"`
		SucceededCost     float64  `json:"succeeded_cost"`
		ImageCount        int64    `json:"image_count"`
		AverageDurationMS *float64 `json:"average_duration_ms"`
	}
	if err := query.Session(&gorm.Session{}).Select("COUNT(*) AS total, COALESCE(SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END),0) AS queued, COALESCE(SUM(CASE WHEN status IN ('invoking','uploading','upstream_succeeded','billing_pending') THEN 1 ELSE 0 END),0) AS processing, COALESCE(SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END),0) AS succeeded, COALESCE(SUM(CASE WHEN status IN ('failed','expired','execution_unknown','storage_failed','billing_failed') THEN 1 ELSE 0 END),0) AS failed, COALESCE(SUM(CASE WHEN billing_status IN ('succeeded','reserved','settled') THEN quota ELSE 0 END),0) AS quota, COALESCE(SUM(CASE WHEN status = 'succeeded' AND billing_status IN ('succeeded','not_billable','settled') THEN quota ELSE 0 END),0) AS succeeded_quota, COALESCE(SUM(image_count),0) AS image_count, AVG(CASE WHEN finished_at > 0 THEN CASE WHEN finished_at > created_at THEN (finished_at-created_at)*1000 ELSE 0 END ELSE NULL END) AS average_duration_ms").Scan(&stats).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	stats.SucceededCost = float64(stats.SucceededQuota) / common.QuotaPerUnit
	imageManagementData(c, gin.H{"items": ordered, "total": total, "pages": (total + int64(size) - 1) / int64(size), "stats": stats, "page": page, "page_size": size})
}
