package controller

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
		userID := c.GetInt("id")
		query = query.Where("user_id = ?", userID)
		ownerSubject := model.DB.WithContext(c.Request.Context()).Model(&model.User{}).
			Select("COALESCE(canvas_subject, '')").Where("id = ?", userID)
		previousAccounts := model.DB.WithContext(c.Request.Context()).Model(&model.CanvasGrant{}).
			Select("token_id").Where("user_id = ?", userID).Where("subject NOT IN (?)", ownerSubject)
		query = query.Where("token_id NOT IN (?)", previousAccounts)
		for _, key := range []string{"channel_id", "account_id", "user_id"} {
			if _, present := c.GetQuery(key); present {
				return nil, errors.New("User image task filters cannot include another user or channel")
			}
		}
	}
	for param, column := range map[string]string{"task_id": "task_id", "protocol": "dialect", "request_type": "request_type", "billing_status": "billing_status", "group": "group"} {
		if value := c.Query(param); value != "" {
			query = query.Where(clause.Eq{Column: column, Value: value})
		}
	}
	if platform := c.Query("platform"); platform != "" {
		query = query.Where("provider = ? OR (provider IS NULL OR provider = '') AND platform = ?", platform, platform)
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
			query = query.Where("(status = ? AND COALESCE(upstream_task_id, '') = '') OR (status = ? AND channel_id = 0 AND COALESCE(upstream_task_id, '') = '')", model.ImageTaskQueued, model.ImageTaskInvoking)
		} else if status == model.ImageTaskInvoking {
			query = query.Where("(status = ? AND channel_id > 0) OR (status = ? AND COALESCE(upstream_task_id, '') <> '')", status, model.ImageTaskQueued)
		} else {
			query = query.Where("status = ?", status)
		}
	}
	if pattern := mediaTaskSearchPattern(c.Query("model")); pattern != "" {
		query = query.Where("LOWER(model) LIKE ? ESCAPE '!'", pattern)
	}
	if pattern := mediaTaskSearchPattern(c.Query("q")); pattern != "" {
		matches := model.DB.WithContext(c.Request.Context()).Where("LOWER(task_id) LIKE ? ESCAPE '!' OR LOWER(model) LIKE ? ESCAPE '!' OR LOWER(prompt_summary) LIKE ? ESCAPE '!' OR LOWER(requested_resolution) LIKE ? ESCAPE '!' OR LOWER(requested_size) LIKE ? ESCAPE '!' OR LOWER(actual_size) LIKE ? ESCAPE '!' OR LOWER(aspect_ratio) LIKE ? ESCAPE '!'", pattern, pattern, pattern, pattern, pattern, pattern, pattern)
		if admin {
			channels := model.DB.WithContext(c.Request.Context()).Model(&model.Channel{}).Select("id").Where("LOWER(name) LIKE ? ESCAPE '!'", pattern)
			matches = matches.Or("channel_id IN (?)", channels)
		}
		query = query.Where(matches)
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
		SucceededQuota  int64    `json:"-"`
		SucceededCost   float64  `json:"succeeded_cost"`
		AverageDuration *float64 `json:"average_duration_ms"`
	}
	selection := `COUNT(*) AS total,
COALESCE(SUM(CASE WHEN (status = 'queued' AND COALESCE(upstream_task_id, '') = '') OR (status = 'invoking' AND channel_id = 0 AND COALESCE(upstream_task_id, '') = '') THEN 1 ELSE 0 END),0) AS queued,
COALESCE(SUM(CASE WHEN (status = 'queued' AND COALESCE(upstream_task_id, '') <> '') OR (status IN ('invoking','upstream_succeeded','uploading','billing_pending') AND NOT (status = 'invoking' AND channel_id = 0 AND COALESCE(upstream_task_id, '') = '')) THEN 1 ELSE 0 END),0) AS processing,
COALESCE(SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END),0) AS succeeded,
COALESCE(SUM(CASE WHEN status IN ('failed','expired','execution_unknown','storage_failed','billing_failed') THEN 1 ELSE 0 END),0) AS failed,
COALESCE(SUM(CASE WHEN status = 'succeeded' THEN image_count ELSE 0 END),0) AS image_count,
COALESCE(SUM(CASE WHEN billing_status IN ('succeeded','not_billable') THEN quota ELSE 0 END),0) AS quota,
COALESCE(SUM(CASE WHEN status = 'succeeded' AND billing_status IN ('succeeded','not_billable') THEN quota ELSE 0 END),0) AS succeeded_quota,
AVG(CASE WHEN status = 'succeeded' AND finished_at >= created_at THEN (finished_at-created_at)*1000.0 ELSE NULL END) AS average_duration`
	if err := query.Session(&gorm.Session{}).Select(selection).Scan(&stats).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	stats.SucceededCost = float64(stats.SucceededQuota) / common.QuotaPerUnit
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
	items, err := imageTasksToDTO(c.Request.Context(), tasks, admin, false)
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	imageManagementData(c, gin.H{"items": items, "total": total, "page": page, "page_size": size, "pages": (total + int64(size) - 1) / int64(size), "stats": stats})
}

// imageTasksToDTO resolves display metadata in batches and keeps routing
// identities and account history confined to the administrator response.
func imageTasksToDTO(ctx context.Context, tasks []model.AsyncImageTask, admin, includeAttempts bool) ([]gin.H, error) {
	items := make([]gin.H, 0, len(tasks))
	if len(tasks) == 0 {
		return items, nil
	}
	taskIDs := make([]string, 0, len(tasks))
	tokenIDs, userIDs, channelIDs := make([]int, 0, len(tasks)), make([]int, 0, len(tasks)), make([]int, 0, len(tasks))
	histories := make(map[string][]service.ImageChannelAttempt)
	for _, task := range tasks {
		taskIDs = append(taskIDs, task.TaskId)
		tokenIDs = append(tokenIDs, task.TokenId)
		if admin {
			userIDs = append(userIDs, task.UserId)
			channelIDs = append(channelIDs, task.ChannelId)
			if includeAttempts && task.Attempts != "" {
				var history []service.ImageChannelAttempt
				if common.UnmarshalJsonStr(task.Attempts, &history) == nil && history != nil {
					histories[task.TaskId] = history
					for _, attempt := range history {
						channelIDs = append(channelIDs, attempt.ChannelId)
					}
				}
			}
		}
	}
	var tokens []model.Token
	if err := model.DB.WithContext(ctx).Select("id", "user_id", "name").Where("id IN ?", tokenIDs).Find(&tokens).Error; err != nil {
		return nil, err
	}
	tokenNames := make(map[int]model.Token, len(tokens))
	for _, token := range tokens {
		tokenNames[token.Id] = token
	}
	userNames, channelNames := make(map[int]string), make(map[int]string)
	if admin {
		var users []model.User
		if err := model.DB.WithContext(ctx).Select("id", "username", "display_name").Where("id IN ?", userIDs).Find(&users).Error; err != nil {
			return nil, err
		}
		for _, user := range users {
			name := user.DisplayName
			if name == "" {
				name = user.Username
			}
			userNames[user.Id] = name
		}
		var channels []model.Channel
		if err := model.DB.WithContext(ctx).Select("id", "name").Where("id IN ?", channelIDs).Find(&channels).Error; err != nil {
			return nil, err
		}
		for _, channel := range channels {
			channelNames[channel.Id] = channel.Name
		}
	}
	var storage []struct {
		TaskId   string
		Provider string
	}
	if err := model.DB.WithContext(ctx).Model(&model.AsyncImageResult{}).
		Select("async_image_results.task_id, image_storage_profiles.provider").
		Joins("JOIN image_storage_objects ON image_storage_objects.object_id = async_image_results.object_id").
		Joins("JOIN image_storage_profiles ON image_storage_profiles.profile_id = image_storage_objects.profile_id").
		Where("async_image_results.task_id IN ?", taskIDs).Distinct().Scan(&storage).Error; err != nil {
		return nil, err
	}
	providers := make(map[string][]string)
	for _, object := range storage {
		providers[object.TaskId] = append(providers[object.TaskId], object.Provider)
	}
	for _, task := range tasks {
		data := imageTaskDTO(task, admin)
		data["api_key_name"] = ""
		if token, found := tokenNames[task.TokenId]; found && token.UserId == task.UserId {
			data["api_key_name"] = token.Name
		}
		data["storage_providers"] = providers[task.TaskId]
		if admin {
			data["user_name"] = userNames[task.UserId]
			data["channel_name"] = channelNames[task.ChannelId]
			if includeAttempts {
				history := make([]gin.H, 0, len(histories[task.TaskId]))
				for _, attempt := range histories[task.TaskId] {
					attempt = service.SanitizeAsyncImageAttempt(attempt)
					history = append(history, gin.H{
						"channel_name":             channelNames[attempt.ChannelId],
						"key_index":                attempt.KeyIndex,
						"started_at":               attempt.StartedAt,
						"finished_at":              attempt.FinishedAt,
						"dispatched":               attempt.Dispatched,
						"reference_mode":           attempt.ReferenceMode,
						"reference_transport_used": attempt.TransportUsed,
						"retry_decision":           attempt.Decision,
						"error_code":               attempt.Code,
						"http_status":              attempt.HTTPStatus,
						"error_message":            attempt.ErrorMessage,
						"provider_code":            attempt.ProviderCode,
						"provider_status":          attempt.ProviderStatus,
						"upstream_request_id":      attempt.UpstreamRequestID,
					})
				}
				data["attempt_history"] = history
				_, historyAvailable := histories[task.TaskId]
				data["attempt_history_unavailable"] = task.Attempts != "" && !historyAvailable
			}
		}
		items = append(items, data)
	}
	return items, nil
}

func imageTaskDTO(task model.AsyncImageTask, admin bool) gin.H {
	displayStatus := task.DisplayStatus()
	data := gin.H{"id": task.TaskId, "task_id": task.TaskId, "provider": task.Provider, "media_type": "image", "stage": mediaStage(displayStatus), "protocol": task.Dialect, "platform": task.Platform, "request_type": task.RequestType, "model": task.Model, "status": displayStatus, "billing_status": task.BillingStatus, "progress": task.Progress, "requested_size": task.RequestedSize, "requested_resolution": task.RequestedResolution, "actual_size": task.ActualSize, "aspect_ratio": task.AspectRatio, "image_count": task.ImageCount, "result_count": task.ResultCount, "quota": task.Quota, "cost": float64(task.Quota) / common.QuotaPerUnit, "currency": "USD", "prompt_summary": task.PromptSummary, "retry_count": task.RetryCount, "error_code": task.ErrorCode, "error_message": task.ErrorMessage, "api_key_id": task.TokenId, "group": task.Group, "created_at": task.CreatedAt, "updated_at": task.UpdatedAt, "started_at": task.StartedAt, "upstream_succeeded_at": task.UpstreamSucceededAt, "finished_at": task.FinishedAt, "expires_at": task.ExpiresAt, "next_attempt_at": task.NextAttemptAt, "duration_ms": nil, "can_resume": task.Status == model.ImageTaskStorageFailed || task.Status == model.ImageTaskBillingFailed, "can_terminate": false}
	if task.ErrorCode == "billing_quota_uncertain" && !admin {
		data["can_resume"] = false
	}
	notice := service.ImageTaskFailureNotice(task)
	data["failure_code"] = task.PublicErrorCode
	data["error_message"] = notice.MessageText(task.ErrorMessage)
	data["error_summary"], data["error_suggestion"] = notice.SummaryText(), notice.SuggestionText()
	if len(notice.SummaryParams) > 0 || notice.SummaryZh != "" {
		data["error_summary_key"], data["error_summary_params"] = notice.Summary, notice.SummaryParams
	}
	if notice.SummaryZh != "" {
		data["error_suggestion_key"] = notice.Suggestion
		data["error_detail"] = service.SanitizeAsyncImageDiagnostic(task.ErrorMessage, common.LocalLogContentLimit)
	}
	data["error_retryable"], data["error_stage"] = notice.Retryable, notice.Stage
	if task.ResultSource != "" {
		data["result_source"] = task.ResultSource
	}
	var decision service.ImageRetryDecision
	if common.UnmarshalJsonStr(task.RetryDecision, &decision) == nil && decision.Version == 1 {
		data["retry_decision"] = decision
	}
	if task.FinishedAt > 0 {
		data["duration_ms"] = max(0, task.FinishedAt-task.CreatedAt) * 1000
	}
	if admin {
		data["retained_request_available"] = task.FailedRequestRetainedAt(time.Now().Unix())
		data["request_retain_until"] = task.RequestRetainUntil
		data["user_id"] = task.UserId
		data["channel_id"] = task.ChannelId
		data["attempts"] = ""
		var attempts []service.ImageChannelAttempt
		if common.UnmarshalJsonStr(task.Attempts, &attempts) == nil {
			for i := range attempts {
				attempts[i] = service.SanitizeAsyncImageAttempt(attempts[i])
			}
			if encoded, err := common.Marshal(attempts); err == nil {
				data["attempts"] = string(encoded)
			}
		}
		data["reference_urls"] = task.ReferenceUrls
		data["can_terminate"] = !task.Terminal() || task.Status == model.ImageTaskExecutionUnknown
		data["reconciliation_status"] = task.ReconciliationStatus
	}
	return data
}

// GetImageRetainedRequest exposes a diagnostic whitelist, never credentials,
// billing snapshots, image bodies or usable signed reference URLs.
func GetImageRetainedRequest(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	var task model.AsyncImageTask
	if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ?", c.Param("task_id")).Take(&task).Error; err != nil || !task.FailedRequestRetainedAt(time.Now().Unix()) {
		imageManagementError(c, 404, errors.New("Retained image request is unavailable or expired"))
		return
	}
	data, err := service.DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
	if err != nil {
		imageManagementError(c, 503, errors.New("Retained image request cannot be decrypted"))
		return
	}
	var request service.AsyncImageRequest
	if err := common.Unmarshal(data, &request); err != nil {
		imageManagementError(c, 503, errors.New("Retained image request is invalid"))
		return
	}
	// Existing masking removes credential patterns and reference URL secrets.
	const maxPromptRunes = 65536
	imageManagementData(c, gin.H{"task_id": task.TaskId, "retain_until": task.RequestRetainUntil, "request": gin.H{
		"model":             service.SanitizeAsyncImageText(request.Model, 256),
		"prompt":            service.SanitizeAsyncImageText(request.Prompt, maxPromptRunes),
		"prompt_characters": utf8.RuneCountInString(request.Prompt),
		"prompt_bytes":      len(request.Prompt),
		"prompt_truncated":  utf8.RuneCountInString(request.Prompt) > maxPromptRunes,
		"platform":          service.SanitizeAsyncImageText(request.Platform, 64),
		"kind":              service.SanitizeAsyncImageText(request.Kind, 64),
		"count":             request.Count,
		"size":              service.SanitizeAsyncImageText(request.Size, 128),
		"resolution":        service.SanitizeAsyncImageText(request.Resolution, 64),
		"aspect_ratio":      service.SanitizeAsyncImageText(request.AspectRatio, 64),
		"reference_count":   request.ReferenceImageCount(),
	}})
}

func GetImageReliability(c *gin.Context) {
	query, err := imageTaskListQuery(c, true)
	if err != nil {
		imageManagementError(c, 400, err)
		return
	}
	location, _ := time.LoadLocation(c.DefaultQuery("timezone", "UTC")) // Validated by the list query.
	start, end := c.Query("start_date"), c.Query("end_date")
	if start == "" && end == "" {
		start = time.Now().In(location).Format("2006-01-02")
		end = start
	}
	startTime, startErr := time.ParseInLocation("2006-01-02", start, location)
	endTime, endErr := time.ParseInLocation("2006-01-02", end, location)
	if startErr != nil || endErr != nil || endTime.AddDate(0, 0, 1).After(startTime.AddDate(0, 0, 31)) {
		imageManagementError(c, 400, errors.New("Reliability statistics require a complete date range of at most 31 days"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	report, err := service.ImageReliability(query.WithContext(ctx))
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	channelIDs := make([]int, 0, len(report.Groups))
	for _, group := range report.Groups {
		if group.ChannelID > 0 {
			channelIDs = append(channelIDs, group.ChannelID)
		}
	}
	if len(channelIDs) > 0 {
		var channels []model.Channel
		if err := model.DB.WithContext(ctx).Select("id", "name").Where("id IN ?", channelIDs).Find(&channels).Error; err != nil {
			imageManagementError(c, 503, err)
			return
		}
		names := make(map[int]string, len(channels))
		for _, channel := range channels {
			names[channel.Id] = channel.Name
		}
		for i := range report.Groups {
			report.Groups[i].ChannelName = names[report.Groups[i].ChannelID]
		}
	}
	imageManagementData(c, report)
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
	managed, err := service.CanvasImageTaskManaged(c.Request.Context(), task)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, service.ErrCanvasAuthorization) || errors.Is(err, model.ErrCanvasBillingOwner) {
			status = http.StatusNotFound
		}
		imageManagementError(c, status, errors.New("Image task is unavailable"))
		return
	}
	var events []model.AsyncImageEvent
	if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ?", task.TaskId).Order("id").Find(&events).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	if task.UpstreamTaskId != "" {
		seen := make(map[string]bool)
		visible := events[:0]
		for _, event := range events {
			if event.Status == model.ImageTaskInvoking {
				switch event.EventType {
				case "claimed", "channel_selected", "upstream_pending", "recovered", "invocation_failed":
					if seen[event.EventType] {
						continue
					}
					seen[event.EventType] = true
				}
			}
			visible = append(visible, event)
		}
		events = visible
	}
	for i := range events {
		if !admin || events[i].EventType == "upstream_result_received" {
			events[i].Message = ""
		}
	}
	results := make([]gin.H, 0)
	if task.ResultsAvailable() && task.ResultSource != model.ImageResultSourceUpstream {
		var saved []model.AsyncImageResult
		if err := model.DB.WithContext(c.Request.Context()).Where("task_id = ?", task.TaskId).Order("image_index").Find(&saved).Error; err != nil {
			imageManagementError(c, 503, err)
			return
		}
		objectIDs := make([]string, 0, len(saved))
		for _, result := range saved {
			objectIDs = append(objectIDs, result.ObjectId)
		}
		objectsByID := make(map[string]model.ImageStorageObject, len(objectIDs))
		if len(objectIDs) > 0 {
			var objects []model.ImageStorageObject
			if err := model.DB.WithContext(c.Request.Context()).Where("object_id IN ?", objectIDs).Find(&objects).Error; err != nil {
				imageManagementError(c, 503, err)
				return
			}
			for _, object := range objects {
				objectsByID[object.ObjectId] = object
			}
		}
		base := "/api/user/async-image-tasks/"
		if admin {
			base = "/api/admin/async-image-tasks/"
		}
		cfg, cfgErr := service.GetImageRuntimeConfig(c.Request.Context())
		for _, result := range saved {
			object, ok := objectsByID[result.ObjectId]
			if !ok {
				imageManagementError(c, 503, errors.New("Image storage object is unavailable"))
				return
			}
			view := base + task.TaskId + "/results/" + strconv.Itoa(result.ImageIndex) + "/view"
			item := gin.H{"id": result.Id, "image_index": result.ImageIndex, "content_type": object.ContentType, "byte_size": object.ByteSize, "checksum": object.Checksum, "width": object.Width, "height": object.Height, "view_url": view, "created_at": result.CreatedAt, "expires_at": object.ExpiresAt}
			if managed {
				item["url"], item["authenticated"] = view, true
			} else if cfgErr == nil {
				if link, signErr := service.ImageObjectURL(c.Request.Context(), object, AsyncImageGatewayBase(), cfg.SignedURLExpiry); signErr == nil {
					item["url"] = link
				}
			}
			results = append(results, item)
		}
	}
	if task.ResultsAvailable() && task.ResultSource == model.ImageResultSourceUpstream {
		urls, err := service.AsyncImageUpstreamResultURLs(c.Request.Context(), task)
		if err != nil {
			imageManagementError(c, 503, err)
			return
		}
		for i, resultURL := range urls {
			item := gin.H{"id": i, "image_index": i, "url": resultURL, "view_url": resultURL, "source": "upstream", "content_type": "", "byte_size": 0, "checksum": "", "width": 0, "height": 0, "created_at": task.UpstreamSucceededAt, "expires_at": task.ExpiresAt}
			if managed {
				base := "/api/user/async-image-tasks/"
				if admin {
					base = "/api/admin/async-image-tasks/"
				}
				view := base + task.TaskId + "/results/" + strconv.Itoa(i) + "/view"
				item["url"], item["view_url"], item["authenticated"] = view, view, true
			}
			results = append(results, item)
		}
	}
	items, err := imageTasksToDTO(c.Request.Context(), []model.AsyncImageTask{task}, admin, true)
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	imageManagementData(c, gin.H{"task": items[0], "results": results, "events": events})
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
	managed, err := service.CanvasImageTaskManaged(ctx, task)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, service.ErrCanvasAuthorization) || errors.Is(err, model.ErrCanvasBillingOwner) {
			status = http.StatusNotFound
		}
		imageManagementError(c, status, errors.New("Image task result is unavailable"))
		return
	}
	if managed {
		canvasResponseHeaders(c)
		if index >= task.ResultCount || task.ExpiresAt > 0 && task.ExpiresAt <= time.Now().Unix() {
			imageManagementError(c, http.StatusNotFound, errors.New("Image task result was not found"))
			return
		}
		if strings.Contains(c.GetHeader("Accept"), "application/json") {
			imageManagementData(c, gin.H{"url": c.Request.URL.Path, "expires_at": task.ExpiresAt, "authenticated": true})
			return
		}
		serveAuthorizedImageTaskResult(c, task, index)
		return
	}
	if task.ResultSource == model.ImageResultSourceUpstream {
		urls, err := service.AsyncImageUpstreamResultURLs(ctx, task)
		if err != nil {
			imageManagementError(c, 503, err)
			return
		}
		if index >= len(urls) {
			imageManagementError(c, 404, errors.New("Image task result was not found"))
			return
		}
		c.Header("Cache-Control", "private, no-store")
		if strings.Contains(c.GetHeader("Accept"), "application/json") {
			imageManagementData(c, gin.H{"url": urls[index], "expires_at": task.ExpiresAt, "source": "upstream"})
			return
		}
		c.Redirect(http.StatusTemporaryRedirect, urls[index])
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
		if task.ErrorCode == "billing_quota_uncertain" {
			var confirmation struct {
				QuotaReconciled bool `json:"confirm_quota_reconciled"`
			}
			if decodeErr := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &confirmation); decodeErr != nil || !confirmation.QuotaReconciled {
				imageManagementError(c, http.StatusBadRequest, errors.New("Confirm the quota reservation has been reconciled before resuming billing"))
				return
			}
			err = model.ResumeAsyncImageTaskAfterQuotaReconciliation(c.Request.Context(), task)
		} else {
			err = model.ResumeAsyncImageTask(c.Request.Context(), task)
		}
	} else if task.Status == model.ImageTaskExecutionUnknown {
		var confirmation struct {
			UpstreamChecked bool `json:"confirm_upstream_checked"`
		}
		if decodeErr := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &confirmation); decodeErr != nil || !confirmation.UpstreamChecked {
			imageManagementError(c, http.StatusConflict, errors.New("Verify the upstream task before releasing reserved quota"))
			return
		}
		err = model.TerminateAsyncImageTaskAfterReconciliation(c.Request.Context(), task)
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
		TaskIds         []string `json:"task_ids"`
		UpstreamChecked bool     `json:"confirm_upstream_checked"`
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
		} else if task.Status == model.ImageTaskExecutionUnknown {
			if !input.UpstreamChecked {
				item["message"] = "Verify the upstream task before releasing reserved quota"
			} else if err := model.TerminateAsyncImageTaskAfterReconciliation(c.Request.Context(), task); err != nil {
				item["message"] = err.Error()
			} else {
				item["status"] = "terminated"
			}
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
