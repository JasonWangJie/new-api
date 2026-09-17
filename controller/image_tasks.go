package controller

import (
	"errors"
	"math"
	"net/http"
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

func imageTaskListQuery(c *gin.Context, admin bool) (*gorm.DB, error) {
	query := model.DB.WithContext(c.Request.Context()).Model(&model.AsyncImageTask{})
	if !admin {
		query = query.Where("user_id = ?", c.GetInt("id"))
		for _, key := range []string{"channel_id", "account_id", "user_id"} {
			if _, present := c.GetQuery(key); present {
				return nil, errors.New("User image task filters cannot include another user or channel")
			}
		}
	}
	for param, column := range map[string]string{"task_id": "task_id", "protocol": "dialect", "platform": "platform", "request_type": "request_type", "billing_status": "billing_status", "model": "model", "group": "group"} {
		if value := c.Query(param); value != "" {
			query = query.Where(clause.Eq{Column: column, Value: value})
		}
	}
	for param, column := range map[string]string{"api_key_id": "token_id", "channel_id": "channel_id", "account_id": "channel_id", "user_id": "user_id"} {
		if value := c.Query(param); value != "" {
			number, err := strconv.Atoi(value)
			if err != nil || number < 1 {
				return nil, errors.New("Invalid image task numeric filter")
			}
			query = query.Where(clause.Eq{Column: column, Value: number})
		}
	}
	if status := c.Query("status"); status != "" {
		if status == model.ImageTaskQueued {
			query = query.Where("status = ? OR (status = ? AND channel_id = 0)", model.ImageTaskQueued, model.ImageTaskInvoking)
		} else if status == model.ImageTaskInvoking {
			query = query.Where("status = ? AND channel_id > 0", status)
		} else {
			query = query.Where("status = ?", status)
		}
	}
	if term := c.Query("q"); term != "" {
		escaped := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(term)
		pattern := "%" + escaped + "%"
		query = query.Where("task_id LIKE ? ESCAPE '!' OR model LIKE ? ESCAPE '!' OR prompt_summary LIKE ? ESCAPE '!'", pattern, pattern, pattern)
	}
	if provider := c.Query("storage_provider"); provider != "" {
		sub := model.DB.Model(&model.AsyncImageResult{}).Select("async_image_results.task_id").Joins("JOIN image_storage_objects ON image_storage_objects.object_id = async_image_results.object_id").Joins("JOIN image_storage_profiles ON image_storage_profiles.profile_id = image_storage_objects.profile_id").Where("image_storage_profiles.provider = ?", provider)
		query = query.Where("task_id IN (?)", sub)
	}
	zone := c.DefaultQuery("timezone", "UTC")
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, errors.New("Invalid image task timezone")
	}
	start, end := c.Query("start_date"), c.Query("end_date")
	if start == "" && end == "" {
		start = time.Now().In(location).Format("2006-01-02")
		end = start
	}
	var startTime, endTime time.Time
	if start != "" {
		startTime, err = time.ParseInLocation("2006-01-02", start, location)
		if err != nil {
			return nil, errors.New("Invalid image task start date")
		}
		query = query.Where("created_at >= ?", startTime.Unix())
	}
	if end != "" {
		endTime, err = time.ParseInLocation("2006-01-02", end, location)
		if err != nil {
			return nil, errors.New("Invalid image task end date")
		}
		endTime = endTime.AddDate(0, 0, 1)
		query = query.Where("created_at < ?", endTime.Unix())
	}
	if !startTime.IsZero() && !endTime.IsZero() && !startTime.Before(endTime) {
		return nil, errors.New("Image task date range is reversed")
	}
	return query, nil
}

func ListImageTasks(c *gin.Context, admin bool) {
	query, err := imageTaskListQuery(c, admin)
	if err != nil {
		imageManagementError(c, 400, err)
		return
	}
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 || page > math.MaxInt32/100 {
		imageManagementError(c, 400, errors.New("Invalid task page"))
		return
	}
	size, err := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if err != nil || size < 1 || size > 100 {
		imageManagementError(c, 400, errors.New("Task page size must be between 1 and 100"))
		return
	}
	var total int64
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	var stats struct {
		Total           int64    `json:"total"`
		Queued          int64    `json:"queued"`
		Processing      int64    `json:"processing"`
		Succeeded       int64    `json:"succeeded"`
		Failed          int64    `json:"failed"`
		ImageCount      int64    `json:"image_count"`
		Quota           int64    `json:"quota"`
		AverageDuration *float64 `json:"average_duration_ms"`
	}
	selection := `COUNT(*) AS total,
COALESCE(SUM(CASE WHEN status = 'queued' OR (status = 'invoking' AND channel_id = 0) THEN 1 ELSE 0 END),0) AS queued,
COALESCE(SUM(CASE WHEN status IN ('invoking','upstream_succeeded','uploading','billing_pending') AND NOT (status = 'invoking' AND channel_id = 0) THEN 1 ELSE 0 END),0) AS processing,
COALESCE(SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END),0) AS succeeded,
COALESCE(SUM(CASE WHEN status IN ('failed','expired','execution_unknown','storage_failed','billing_failed') THEN 1 ELSE 0 END),0) AS failed,
COALESCE(SUM(CASE WHEN status = 'succeeded' THEN image_count ELSE 0 END),0) AS image_count,
COALESCE(SUM(CASE WHEN billing_status IN ('succeeded','not_billable') THEN quota ELSE 0 END),0) AS quota,
AVG(CASE WHEN status = 'succeeded' AND finished_at >= created_at THEN (finished_at-created_at)*1000.0 ELSE NULL END) AS average_duration`
	if err := query.Session(&gorm.Session{}).Select(selection).Scan(&stats).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	allowed := map[string]string{"created_at": "created_at", "submitted_at": "created_at", "finished_at": "finished_at", "updated_at": "updated_at", "status": "status", "quota": "quota", "image_count": "image_count", "duration_ms": "(CASE WHEN finished_at > 0 THEN finished_at-created_at ELSE 0 END)"}
	column, ok := allowed[c.DefaultQuery("sort_by", "created_at")]
	if !ok {
		imageManagementError(c, 400, errors.New("Invalid image task sort column"))
		return
	}
	direction := strings.ToUpper(c.DefaultQuery("sort_order", "desc"))
	if direction != "ASC" && direction != "DESC" {
		imageManagementError(c, 400, errors.New("Invalid image task sort direction"))
		return
	}
	var tasks []model.AsyncImageTask
	if err := query.Session(&gorm.Session{}).Order(column + " " + direction).Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&tasks).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	items := make([]gin.H, 0, len(tasks))
	for _, task := range tasks {
		items = append(items, imageTaskDTO(task, admin))
	}
	imageManagementData(c, gin.H{"items": items, "total": total, "page": page, "page_size": size, "pages": (total + int64(size) - 1) / int64(size), "stats": stats})
}

func imageTaskDTO(task model.AsyncImageTask, admin bool) gin.H {
	data := gin.H{"id": task.TaskId, "task_id": task.TaskId, "protocol": task.Dialect, "platform": task.Platform, "request_type": task.RequestType, "model": task.Model, "status": task.DisplayStatus(), "billing_status": task.BillingStatus, "progress": task.Progress, "requested_size": task.RequestedSize, "requested_resolution": task.RequestedResolution, "actual_size": task.ActualSize, "aspect_ratio": task.AspectRatio, "image_count": task.ImageCount, "result_count": task.ResultCount, "quota": task.Quota, "cost": float64(task.Quota) / common.QuotaPerUnit, "currency": "USD", "prompt_summary": task.PromptSummary, "retry_count": task.RetryCount, "error_code": task.ErrorCode, "error_message": task.ErrorMessage, "api_key_id": task.TokenId, "group": task.Group, "created_at": task.CreatedAt, "updated_at": task.UpdatedAt, "started_at": task.StartedAt, "upstream_succeeded_at": task.UpstreamSucceededAt, "finished_at": task.FinishedAt, "expires_at": task.ExpiresAt, "next_attempt_at": task.NextAttemptAt, "duration_ms": nil, "can_resume": false, "can_terminate": false}
	if task.FinishedAt > 0 {
		data["duration_ms"] = max(0, task.FinishedAt-task.CreatedAt) * 1000
	}
	if admin {
		data["user_id"] = task.UserId
		data["channel_id"] = task.ChannelId
		data["attempts"] = task.Attempts
		data["reference_urls"] = task.ReferenceUrls
		data["can_resume"] = task.Status == model.ImageTaskStorageFailed || task.Status == model.ImageTaskBillingFailed
		data["can_terminate"] = !task.Terminal()
		data["reconciliation_status"] = task.ReconciliationStatus
	}
	return data
}

func GetImageTask(c *gin.Context, admin bool) {
	query := model.DB.WithContext(c.Request.Context()).Where("task_id = ?", c.Param("task_id"))
	if !admin {
		query = query.Where("user_id = ?", c.GetInt("id"))
	}
	var task model.AsyncImageTask
	if err := query.Take(&task).Error; err != nil {
		imageManagementError(c, 404, errors.New("Image task was not found"))
		return
	}
	var events []model.AsyncImageEvent
	if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ?", task.TaskId).Order("id").Find(&events).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	results := make([]gin.H, 0)
	if task.ResultsAvailable() {
		var saved []model.AsyncImageResult
		if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ?", task.TaskId).Order("image_index").Find(&saved).Error; err != nil {
			imageManagementError(c, 503, err)
			return
		}
		for _, result := range saved {
			var object model.ImageStorageObject
			if err := model.DB.WithContext(c.Request.Context()).Where("object_id = ?", result.ObjectId).Take(&object).Error; err != nil {
				imageManagementError(c, 503, err)
				return
			}
			base := "/api/user/async-image-tasks/"
			if admin {
				base = "/api/admin/async-image-tasks/"
			}
			view := base + task.TaskId + "/results/" + strconv.Itoa(result.ImageIndex) + "/view"
			results = append(results, gin.H{"id": result.Id, "image_index": result.ImageIndex, "content_type": object.ContentType, "byte_size": object.ByteSize, "checksum": object.Checksum, "width": object.Width, "height": object.Height, "view_url": view, "created_at": result.CreatedAt, "expires_at": object.ExpiresAt})
		}
	}
	imageManagementData(c, gin.H{"task": imageTaskDTO(task, admin), "results": results, "events": events})
}

func ViewImageTaskResult(c *gin.Context, admin bool) {
	ctx := c.Request.Context()
	var task model.AsyncImageTask
	query := model.DB.WithContext(ctx).Where("task_id = ?", c.Param("task_id"))
	if !admin {
		query = query.Where("user_id = ?", c.GetInt("id"))
	}
	if err := query.Take(&task).Error; err != nil || !task.ResultsAvailable() {
		imageManagementError(c, 404, errors.New("Image task result was not found"))
		return
	}
	index, err := strconv.Atoi(c.Param("image_index"))
	if err != nil || index < 0 {
		imageManagementError(c, 400, errors.New("Invalid image index"))
		return
	}
	var result model.AsyncImageResult
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND image_index = ?", task.TaskId, index).Take(&result).Error; err != nil {
		imageManagementError(c, 404, errors.New("Image task result was not found"))
		return
	}
	var object model.ImageStorageObject
	if err := model.DB.WithContext(ctx).Where("object_id = ? AND status = ?", result.ObjectId, "active").Take(&object).Error; err != nil {
		imageManagementError(c, 503, errors.New("Image storage object is unavailable"))
		return
	}
	cfg, err := service.GetImageRuntimeConfig(ctx)
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	link, err := service.ImageObjectURL(ctx, object, AsyncImageGatewayBase(), cfg.SignedURLExpiry)
	if err != nil {
		imageManagementError(c, 503, errors.New("Image signing is unavailable"))
		return
	}
	c.Header("Cache-Control", "private, no-store")
	if strings.Contains(c.GetHeader("Accept"), "application/json") {
		imageManagementData(c, gin.H{"url": link, "expires_at": min(object.ExpiresAt, time.Now().Unix()+int64(cfg.SignedURLExpiry))})
		return
	}
	c.Redirect(http.StatusTemporaryRedirect, link)
}

func ManageImageTask(c *gin.Context, action string) {
	var task model.AsyncImageTask
	if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ?", c.Param("task_id")).Take(&task).Error; err != nil {
		imageManagementError(c, 404, errors.New("Image task was not found"))
		return
	}
	var err error
	if action == "resume" {
		err = model.ResumeAsyncImageTask(c.Request.Context(), task)
	} else {
		err = model.TerminateAsyncImageTask(c.Request.Context(), task)
	}
	if err != nil {
		status := 503
		if errors.Is(err, model.ErrImageConflict) {
			status = 409
		}
		imageManagementError(c, status, err)
		return
	}
	imageManagementData(c, gin.H{"task_id": task.TaskId})
}

func BatchTerminateImageTasks(c *gin.Context) {
	var input struct {
		TaskIds []string `json:"task_ids"`
	}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &input); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	if len(input.TaskIds) == 0 || len(input.TaskIds) > 100 {
		imageManagementError(c, 400, errors.New("Select between 1 and 100 task IDs from the current page"))
		return
	}
	seen := make(map[string]bool)
	items := make([]gin.H, 0, len(input.TaskIds))
	for _, raw := range input.TaskIds {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		item := gin.H{"task_id": id, "status": "failed"}
		var task model.AsyncImageTask
		if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ?", id).Take(&task).Error; err != nil {
			item["message"] = "Image task was not found"
		} else if task.Terminal() {
			item["status"] = "skipped"
			item["message"] = "Task is already terminal"
		} else if err := model.TerminateAsyncImageTask(c.Request.Context(), task); err != nil {
			item["message"] = err.Error()
		} else {
			item["status"] = "terminated"
		}
		items = append(items, item)
	}
	imageManagementData(c, gin.H{"items": items})
}
