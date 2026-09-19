package controller

import (
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

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
	if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ? AND user_id = ? AND token_id = ?", c.Param("task_id"), c.GetInt("id"), c.GetInt("token_id")).Take(&job).Error; err != nil {
		AsyncImagePublicError(c, 404, "task_not_found", "Media task was not found")
		return
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
	status, progress := job.Status, 0
	if status == "submitting" || status == "submitted" {
		status = "invoking"
		progress = 10
	}
	if job.StorageStatus == "saving" {
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
	view := gin.H{"id": job.TaskId, "task_id": job.TaskId, "media_type": "video", "platform": job.Provider, "provider": job.Provider, "protocol": "openai_video", "model": job.Model, "request_type": "text_to_video", "status": status, "stage": mediaStage(status), "progress": progress, "billing_status": job.BillingStatus, "storage_status": job.StorageStatus, "quota": job.Quota, "cost": float64(job.Quota) / common.QuotaPerUnit, "group": job.Group, "api_key_id": job.TokenId, "image_count": 0, "result_count": 0, "created_at": job.CreatedAt, "started_at": job.DispatchedAt, "finished_at": int64(0), "expires_at": job.ExpiresAt, "next_attempt_at": job.NextAttemptAt, "retry_count": 0, "storage_retry_count": job.StorageRetries, "error_message": job.ErrorMessage, "error_code": "", "prompt_summary": "", "requested_size": "", "actual_size": "", "aspect_ratio": "", "can_resume": job.StorageStatus == "failed" && job.LeaseExpiresAt <= time.Now().Unix(), "can_terminate": job.Status == "queued" && job.DispatchedAt == 0 && job.LeaseExpiresAt <= time.Now().Unix(), "storage_providers": []string{"local"}}
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
		view["updated_at"] = task.UpdatedAt
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
			view["status"], view["stage"], view["error_message"] = "failed", "failed", task.FailReason
			if job.BillingStatus != "failed" {
				view["billing_status"] = "refunded"
			}
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, err
	}
	results := make([]gin.H, 0)
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
		link, err := service.MediaObjectURL(object, AsyncImageGatewayBase(), cfg.SignedURLExpiry)
		if err != nil {
			return nil, nil, err
		}
		results = append(results, gin.H{"id": object.ID, "image_index": i, "key": object.ArtifactKey, "url": link, "view_url": link, "content_type": object.MimeType, "width": 0, "height": 0, "byte_size": object.ByteSize, "checksum": object.Checksum, "expires_at": min(object.ExpiresAt, time.Now().Unix()+int64(cfg.SignedURLExpiry))})
	}
	view["result_count"] = len(results)
	if len(results) == 0 {
		view["status"], view["stage"], view["storage_status"] = "expired", "failed", "expired"
	}
	return view, results, nil
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
	query := model.DB.WithContext(c.Request.Context()).Where("task_id = ?", c.Param("task_id"))
	if !admin {
		query = query.Where("user_id = ?", c.GetInt("id"))
	}
	var job model.AsyncMediaJob
	if err := query.Take(&job).Error; err != nil {
		imageManagementError(c, 404, errors.New("Media task was not found"))
		return
	}
	view, results, err := videoJobView(c, job, admin)
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	imageManagementData(c, gin.H{"task": view, "results": results, "events": []any{}})
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
	query := model.DB.WithContext(c.Request.Context()).Model(&model.AsyncMediaJob{}).Where("task_id = ? AND lease_expires_at <= ? AND expires_at > ?", c.Param("task_id"), time.Now().Unix(), time.Now().Unix())
	if !admin {
		query = query.Where("user_id = ?", c.GetInt("id"))
	}
	updates := map[string]any{}
	switch action {
	case "resume":
		query = query.Where("status = ? AND storage_status = ?", "submitted", "failed")
		updates["next_attempt_at"], updates["storage_retries"], updates["error_message"] = 0, 0, ""
	case "terminate":
		query = query.Where("status = ? AND dispatched_at = 0", "queued")
		updates["status"], updates["request_cipher"], updates["billing_status"] = "failed", nil, "not_billable"
	default:
		imageManagementError(c, 400, errors.New("Invalid media task operation"))
		return
	}
	result := query.Updates(updates)
	if result.Error != nil {
		imageManagementError(c, 503, result.Error)
		return
	}
	if result.RowsAffected != 1 {
		imageManagementError(c, 409, errors.New("Media task cannot be changed in its current phase"))
		return
	}
	imageManagementData(c, gin.H{"task_id": c.Param("task_id")})
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
		videos = videos.Where("user_id = ?", c.GetInt("id"))
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
	for key, column := range map[string]string{"task_id": "task_id", "model": "model", "group": "group", "api_key_id": "token_id", "user_id": "user_id", "channel_id": "channel_id", "billing_status": "billing_status", "provider": "provider"} {
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
		if kind == "text_to_video" {
			videos = videos.Where("NOT EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.action <> ?)", kind)
		} else if kind == "image_to_video" {
			videos = videos.Where("EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.action = ?)", kind)
		} else {
			videos = videos.Where("1 = 0")
		}
	}
	if storage := c.Query("storage_provider"); storage != "" && storage != "local" {
		videos = videos.Where("1 = 0")
	}
	if status := c.Query("status"); status != "" {
		switch status {
		case "invoking":
			videos = videos.Where("status IN ? AND storage_status = ?", []string{"submitting", "submitted"}, "pending")
		case "uploading":
			videos = videos.Where("storage_status = ?", "saving")
		case "storage_failed":
			videos = videos.Where("storage_status = ?", "failed")
		default:
			videos = videos.Where("status = ?", status)
		}
	}
	if term := c.Query("q"); term != "" {
		pattern := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(term) + "%"
		videos = videos.Where("task_id LIKE ? ESCAPE '!' OR model LIKE ? ESCAPE '!'", pattern, pattern)
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
		switch stage {
		case "queued":
			videos = videos.Where("status = ?", "queued")
		case "generating":
			videos = videos.Where("status IN ? AND storage_status = ? AND NOT EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status IN ?)", []string{"submitting", "submitted"}, "pending", []model.TaskStatus{model.TaskStatusSuccess, model.TaskStatusFailure})
		case "saving":
			videos = videos.Where("storage_status IN ? OR (storage_status = ? AND EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status = ?))", []string{"saving", "failed"}, "pending", model.TaskStatusSuccess)
		case "completed":
			videos = videos.Where("status = ?", "succeeded")
		case "failed":
			videos = videos.Where("status IN ? OR EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status = ?)", []string{"failed", "execution_unknown", "expired"}, model.TaskStatusFailure)
		}
	}
	videoProjection := "task_id, created_at, CASE WHEN storage_status = 'failed' THEN 'storage_failed' WHEN storage_status = 'saving' THEN 'uploading' WHEN status IN ('submitting','submitted') AND EXISTS(SELECT 1 FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id AND tasks.status = 'SUCCESS') THEN 'upstream_succeeded' WHEN status IN ('submitting','submitted') THEN 'invoking' ELSE status END AS status, COALESCE((SELECT quota FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id LIMIT 1), quota) AS quota, billing_status, 0 AS image_count, COALESCE((SELECT finish_time FROM tasks WHERE tasks.task_id = async_media_jobs.task_id AND tasks.user_id = async_media_jobs.user_id LIMIT 1),0) AS finished_at, created_at AS updated_at, 'video' AS media_type"
	union := model.DB.Raw("? UNION ALL ?", images.Select("task_id, created_at, status, quota, billing_status, image_count, finished_at, updated_at, 'image' AS media_type"), videos.Select(videoProjection))
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
		ImageCount        int64    `json:"image_count"`
		AverageDurationMS *float64 `json:"average_duration_ms"`
	}
	if err := query.Session(&gorm.Session{}).Select("COUNT(*) AS total, COALESCE(SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END),0) AS queued, COALESCE(SUM(CASE WHEN status IN ('invoking','uploading','upstream_succeeded','billing_pending') THEN 1 ELSE 0 END),0) AS processing, COALESCE(SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END),0) AS succeeded, COALESCE(SUM(CASE WHEN status IN ('failed','expired','execution_unknown','storage_failed','billing_failed') THEN 1 ELSE 0 END),0) AS failed, COALESCE(SUM(CASE WHEN billing_status IN ('succeeded','reserved','settled') THEN quota ELSE 0 END),0) AS quota, COALESCE(SUM(image_count),0) AS image_count, AVG(CASE WHEN finished_at > 0 THEN CASE WHEN finished_at > created_at THEN (finished_at-created_at)*1000 ELSE 0 END ELSE NULL END) AS average_duration_ms").Scan(&stats).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	imageManagementData(c, gin.H{"items": ordered, "total": total, "pages": (total + int64(size) - 1) / int64(size), "stats": stats, "page": page, "page_size": size})
}
