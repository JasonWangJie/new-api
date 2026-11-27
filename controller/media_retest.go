package controller

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const videoMediaRetestRequestKey = "async_video_retest_input"

type mediaRetestOperatorContext struct{}

type mediaRetestOperator struct {
	userID, tokenID int
	requestHash     string
}

type mediaRetestSelection struct {
	ChannelID *int    `json:"channel_id"`
	Model     *string `json:"model"`
}

func mediaRetestRequestHash(c *gin.Context, fallback string) string {
	operator, _ := c.Request.Context().Value(mediaRetestOperatorContext{}).(mediaRetestOperator)
	if operator.requestHash != "" {
		return operator.requestHash
	}
	return fallback
}

type mediaReplayHeader struct {
	service.MediaRetestRequest
	ImageDetails *retainedImageDetails `json:"image_details,omitempty"`
}

type retainedImageDetails struct {
	Model, Prompt, Platform, Kind, Size, Resolution, AspectRatio string
	Count, ReferenceCount                                        int
}

func imageRetainedDetails(request service.AsyncImageRequest) retainedImageDetails {
	return retainedImageDetails{
		Model: request.Model, Prompt: request.Prompt, Platform: request.Platform, Kind: request.Kind,
		Size: request.Size, Resolution: request.Resolution, AspectRatio: request.AspectRatio,
		Count: request.Count, ReferenceCount: request.ReferenceImageCount(),
	}
}

// MediaRetestOperatorMatches binds private admission to the same operator and
// token even if credential parsing or a cache returns a different identity.
func MediaRetestOperatorMatches(c *gin.Context) bool {
	operator, present := c.Request.Context().Value(mediaRetestOperatorContext{}).(mediaRetestOperator)
	return !present || operator.userID == c.GetInt("id") && operator.tokenID == c.GetInt("token_id")
}

func encryptMediaReplaySnapshot(mediaType, taskID string, request mediaReplayHeader) (*model.MediaReplaySnapshot, error) {
	body := request.Body
	request.Body = nil
	header, err := common.Marshal(request)
	if err != nil {
		return nil, err
	}
	// Keep multipart files and inline content in their original bytes, without a second base64 encoding.
	plaintext := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(header)+len(body)), uint32(len(header)))
	plaintext = append(append(plaintext, header...), body...)
	payload, err := service.EncryptImagePayload(plaintext, "media-retest:"+mediaType+":"+taskID)
	if err != nil {
		return nil, err
	}
	return &model.MediaReplaySnapshot{Payload: payload}, nil
}

func decodeMediaReplaySnapshot(snapshot model.MediaReplaySnapshot) (mediaReplayHeader, error) {
	plaintext, err := service.DecryptImagePayload(snapshot.Payload, "media-retest:"+snapshot.MediaType+":"+snapshot.TaskId)
	var request mediaReplayHeader
	if err != nil {
		return request, err
	}
	if len(plaintext) < 4 {
		return request, errors.New("invalid retained media request")
	}
	headerSize := int(binary.BigEndian.Uint32(plaintext[:4]))
	if headerSize <= 0 || headerSize >= len(plaintext)-4 {
		return request, errors.New("invalid retained media request header")
	}
	if err := common.Unmarshal(plaintext[4:4+headerSize], &request); err != nil {
		return request, err
	}
	request.Body = plaintext[4+headerSize:]
	return request, nil
}

func confirmedImageRetestFailure(task model.AsyncImageTask) bool {
	if task.Status != model.ImageTaskFailed && task.Status != model.ImageTaskExpired || task.TerminationRequiresReconciliation() || task.UpstreamSucceededAt > 0 || task.ResultCount > 0 {
		return false
	}
	switch task.BillingStatus {
	case "refunded":
		return true
	case "not_charged", "not_billable":
		return task.Quota == 0
	}
	return false
}

func confirmedVideoRetestFailure(job model.AsyncMediaJob) bool {
	if job.Status != "failed" || job.Quota > 0 || job.OutputObjectKey != "" || job.StorageStatus != "" && job.StorageStatus != "pending" {
		return false
	}
	return job.BillingStatus == "refunded" || job.BillingStatus == "not_billable"
}

func addMediaRetestDetail(ctx context.Context, view gin.H, mediaType, taskID string, hasSnapshot, confirmedFailure bool, now int64) error {
	if hasSnapshot {
		until, err := model.FindRetainedMediaReplayExpiry(ctx, mediaType, taskID, now)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if until > now {
			view["retest_retain_until"] = until
			if mediaType == model.MediaReplayImage {
				view["retained_request_available"], view["request_retain_until"] = true, until
			}
		}
	}
	view["can_retest"] = false
	switch {
	case !confirmedFailure:
		view["retest_unavailable_reason"] = "not_confirmed_failed"
	case !hasSnapshot:
		view["retest_unavailable_reason"] = "request_unavailable"
	case view["retest_retain_until"] == nil:
		view["retest_unavailable_reason"] = "request_expired"
	default:
		view["can_retest"] = true
	}
	return nil
}

func RetestMediaTask(admission http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "private, no-store")
		var input struct {
			TokenID        int               `json:"token_id"`
			IdempotencyKey string            `json:"idempotency_key"`
			Channel        common.RawMessage `json:"channel_id"`
			PublicModel    common.RawMessage `json:"model"`
		}
		mediaType, _, mimeErr := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if mimeErr != nil || mediaType != "application/json" || common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4096), &input) != nil || input.TokenID <= 0 || !validMediaRetestKey(input.IdempotencyKey) {
			mediaRetestError(c, http.StatusBadRequest, "invalid_retest_input", "A valid API key ID and idempotency key are required")
			return
		}
		explicit := input.Channel != nil || input.PublicModel != nil
		var selection mediaRetestSelection
		var requestHash string
		if explicit {
			if common.Unmarshal(input.Channel, &selection.ChannelID) != nil || selection.ChannelID == nil || *selection.ChannelID < 0 ||
				common.Unmarshal(input.PublicModel, &selection.Model) != nil || selection.Model == nil || strings.TrimSpace(*selection.Model) == "" || len(*selection.Model) > 255 {
				mediaRetestError(c, http.StatusBadRequest, "invalid_retest_selection", "Choose a channel or automatic routing and a public model")
				return
			}
			*selection.Model = strings.TrimSpace(*selection.Model)
			// The original request is immutable and the operation key already
			// binds the source and operator. The prefix distinguishes legacy hashes.
			requestHash = "retest:" + service.ImageIdentityHash(strconv.Itoa(*selection.ChannelID), *selection.Model)[:57]
		}
		ctx := c.Request.Context()
		token, clientIP, ok := mediaRetestToken(c, input.TokenID)
		if !ok {
			return
		}
		taskID := c.Param("task_id")
		mediaType = mediaRetestType(taskID)
		operationKey := service.ImageIdentityHash("media-task-retest", strconv.Itoa(token.UserId), strconv.Itoa(token.Id), taskID, input.IdempotencyKey)
		previous, err := admittedMediaRetestTask(ctx, mediaType, token, operationKey)
		if err != nil {
			mediaRetestError(c, http.StatusServiceUnavailable, "retest_unavailable", "The retest submission could not be checked")
			return
		}
		if respondAdmittedMediaRetest(c, mediaType, previous, requestHash) {
			return
		}
		source, header, snapshot, ok := retainedMediaRetest(c, mediaType, taskID)
		if !ok {
			return
		}
		request := header.MediaRetestRequest
		operator := mediaRetestOperator{userID: token.UserId, tokenID: token.Id, requestHash: requestHash}
		admissionContext := context.WithValue(ctx, mediaRetestOperatorContext{}, operator)
		if explicit {
			if err := service.ValidateMediaRetestReferences(ctx, source, token, request); err != nil {
				if previous, err := admittedMediaRetestTask(ctx, mediaType, token, operationKey); err == nil && respondAdmittedMediaRetest(c, mediaType, previous, requestHash) {
					return
				}
				code, message := mediaRetestReferenceError(err)
				mediaRetestError(c, http.StatusConflict, code, message)
				return
			}
			request, err = service.SelectMediaRetestModel(request, *selection.Model)
			if err != nil {
				mediaRetestError(c, http.StatusConflict, "request_unavailable", "The original request cannot be replayed")
				return
			}
			probe := runMediaRetestAdmission(WithMediaRetestRouting(admissionContext, *selection.ChannelID, true), admission, token, clientIP, request, "")
			if _, available := parseMediaRetestProbe(probe, *selection.ChannelID); !available {
				if previous, err := admittedMediaRetestTask(ctx, mediaType, token, operationKey); err == nil && respondAdmittedMediaRetest(c, mediaType, previous, requestHash) {
					return
				}
				_, rejection := parseMediaRetestAdmission(probe)
				if rejection.Status >= 500 && rejection.Code != "channel_unavailable" && rejection.Code != "model_not_found" {
					mediaRetestError(c, rejection.Status, rejection.Code, rejection.Message)
				} else {
					mediaRetestError(c, http.StatusConflict, "combination_unavailable", rejection.Message)
				}
				return
			}
			admissionContext = WithMediaRetestRouting(admissionContext, *selection.ChannelID, false)
		}
		request.RetestKey = operationKey
		request, err = service.PrepareMediaRetest(ctx, source, token, request)
		if err != nil {
			if previous, err := admittedMediaRetestTask(ctx, mediaType, token, operationKey); err == nil && respondAdmittedMediaRetest(c, mediaType, previous, requestHash) {
				return
			}
			code, message := mediaRetestReferenceError(err)
			mediaRetestError(c, http.StatusConflict, code, message)
			return
		}
		if time.Now().Unix() >= snapshot.RetainUntil {
			mediaRetestError(c, http.StatusGone, "request_expired", "The original request has expired")
			return
		}
		recorder := runMediaRetestAdmission(admissionContext, admission, token, clientIP, request, operationKey)
		result, rejection := parseMediaRetestAdmission(recorder)
		if recorder.Code != http.StatusAccepted || result == "" {
			// A concurrent request may have won normal admission while we copied references.
			if previous, err := admittedMediaRetestTask(ctx, mediaType, token, operationKey); err == nil && respondAdmittedMediaRetest(c, mediaType, previous, requestHash) {
				return
			}
			mediaRetestError(c, rejection.Status, rejection.Code, rejection.Message)
			return
		}
		if recorder.Header().Get("X-Idempotency-Replayed") != "true" {
			recordMediaRetest(c, token.UserId, taskID, result, token.Id, selection, source.Model)
		}
		imageManagementData(c, gin.H{"task_id": result, "media_type": mediaType})
	}
}

func mediaRetestType(taskID string) string {
	if strings.HasPrefix(taskID, "asyncimg_") {
		return model.MediaReplayImage
	}
	return model.MediaReplayVideo
}

func mediaRetestToken(c *gin.Context, tokenID int) (model.Token, net.IP, bool) {
	var token model.Token
	identity, authenticated := middleware.GetSessionAuthIdentity(c)
	if !authenticated || identity.UserID != c.GetInt("id") {
		mediaRetestError(c, http.StatusForbidden, "operator_session_required", "Sign in to retest a media task")
		return token, nil, false
	}
	err := model.DB.WithContext(c.Request.Context()).Where("id = ? AND user_id = ?", tokenID, identity.UserID).Take(&token).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		mediaRetestError(c, http.StatusServiceUnavailable, "retest_unavailable", "API key lookup is unavailable")
		return token, nil, false
	}
	clientIP := net.ParseIP(c.ClientIP())
	if err != nil || token.Status != common.TokenStatusEnabled || token.ExpiredTime != -1 && token.ExpiredTime <= time.Now().Unix() || token.Key == "" || strings.Contains(token.Key, "-") || model.IsCanvasTokenKey(token.Key) ||
		len(token.GetIpLimits()) > 0 && (clientIP == nil || !common.IsIpInCIDRList(clientIP, token.GetIpLimits())) {
		mediaRetestError(c, http.StatusForbidden, "operator_key_unavailable", "Use one of your own active API keys allowed from this address")
		return token, nil, false
	}
	if clientIP == nil {
		mediaRetestError(c, http.StatusBadRequest, "client_address_unavailable", "The operator address could not be determined")
		return token, nil, false
	}
	return token, clientIP, true
}

func retainedMediaRetest(c *gin.Context, mediaType, taskID string) (source service.MediaRetestSource, header mediaReplayHeader, snapshot model.MediaReplaySnapshot, ok bool) {
	ctx := c.Request.Context()
	source, confirmed, hasSnapshot, err := loadMediaRetestSource(ctx, mediaType, taskID)
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, service.ErrCanvasAuthorization) || errors.Is(err, model.ErrCanvasBillingOwner) {
		mediaRetestError(c, http.StatusNotFound, "source_task_unavailable", "The original media task is unavailable")
		return source, header, snapshot, false
	}
	if err != nil {
		mediaRetestError(c, http.StatusServiceUnavailable, "retest_unavailable", "The original media task could not be checked")
		return source, header, snapshot, false
	}
	if !confirmed {
		mediaRetestError(c, http.StatusConflict, "not_confirmed_failed", "Retest is available only for a confirmed failed task with no result or pending reconciliation")
		return source, header, snapshot, false
	}
	if !hasSnapshot {
		mediaRetestError(c, http.StatusConflict, "request_unavailable", "The original request was not retained for retest")
		return source, header, snapshot, false
	}
	snapshot, err = model.GetRetainedMediaReplaySnapshot(ctx, mediaType, taskID, time.Now().Unix())
	if errors.Is(err, gorm.ErrRecordNotFound) {
		mediaRetestError(c, http.StatusGone, "request_expired", "The original request has expired")
		return source, header, snapshot, false
	}
	if err != nil {
		mediaRetestError(c, http.StatusServiceUnavailable, "retest_unavailable", "The original request could not be read")
		return source, header, snapshot, false
	}
	header, err = decodeMediaReplaySnapshot(snapshot)
	if err != nil || !validMediaRetestPath(mediaType, header.Path) {
		mediaRetestError(c, http.StatusConflict, "request_unavailable", "The original request is unavailable or cannot be replayed")
		return source, header, snapshot, false
	}
	return source, header, snapshot, true
}

func runMediaRetestAdmission(ctx context.Context, admission http.Handler, token model.Token, clientIP net.IP, request service.MediaRetestRequest, operationKey string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, request.Path, bytes.NewReader(request.Body))
	if err != nil {
		recorder.WriteHeader(http.StatusBadRequest)
		return recorder
	}
	req.RemoteAddr = net.JoinHostPort(clientIP.String(), "0")
	req.Header.Set("Content-Type", request.ContentType)
	req.Header.Set("Authorization", "Bearer sk-"+token.GetFullKey())
	if operationKey != "" {
		req.Header.Set("Idempotency-Key", operationKey)
	}
	admission.ServeHTTP(recorder, req)
	return recorder
}

type mediaRetestRejection struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func parseMediaRetestAdmission(recorder *httptest.ResponseRecorder) (string, mediaRetestRejection) {
	var result struct {
		TaskID string               `json:"task_id"`
		Error  mediaRetestRejection `json:"error"`
	}
	if common.Unmarshal(recorder.Body.Bytes(), &result) == nil {
		if recorder.Code == http.StatusAccepted && result.TaskID != "" {
			return result.TaskID, mediaRetestRejection{}
		}
		if recorder.Code >= 400 && result.Error.Message != "" {
			result.Error.Status = recorder.Code
			result.Error.Message = service.SanitizeAsyncImageText(result.Error.Message, 512)
			return "", result.Error
		}
	}
	return "", mediaRetestRejection{Status: http.StatusServiceUnavailable, Code: "retest_unavailable", Message: "The media route is currently unavailable"}
}

func mediaRetestReferenceError(err error) (string, string) {
	if errors.Is(err, service.ErrMediaRetestProviderAsset) {
		return "provider_reference_unavailable", "A reference belongs to the original upstream account and cannot be reused"
	}
	return "reference_unavailable", "An original reference is unavailable, expired, or could not be copied"
}

func validMediaRetestKey(key string) bool {
	if len(key) < 16 || len(key) > 128 {
		return false
	}
	for _, ch := range key {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '-' && ch != '_' {
			return false
		}
	}
	return true
}

func validMediaRetestPath(mediaType, path string) bool {
	if mediaType == model.MediaReplayImage {
		switch path {
		case "/v1/images/generations_oa", "/v1/images/edits_oa", "/v1/chat/completions_gm", "/v1/images/generations_sc", "/v1/images/generations_async", "/v1/images/edits_async":
			return true
		}
	} else if mediaType == model.MediaReplayVideo {
		switch path {
		case "/v1/videos/generations_async", "/v1/videos/edits_async", "/v1/videos/extensions_async":
			return true
		}
	}
	return false
}

func loadMediaRetestSource(ctx context.Context, mediaType, taskID string) (service.MediaRetestSource, bool, bool, error) {
	var source service.MediaRetestSource
	var confirmed, hasSnapshot bool
	if mediaType == model.MediaReplayImage {
		var task model.AsyncImageTask
		err := model.DB.WithContext(ctx).Select("task_id", "user_id", "token_id", "model", "channel_id", "status", "billing_status", "quota", "result_count", "upstream_succeeded_at", "error_code", "reconciliation_status", "has_replay_snapshot").Where("task_id = ?", taskID).Take(&task).Error
		if err != nil {
			return source, false, false, err
		}
		source = service.MediaRetestSource{TaskID: task.TaskId, UserID: task.UserId, TokenID: task.TokenId, Model: task.Model, ChannelID: task.ChannelId, MediaType: mediaType}
		confirmed, hasSnapshot = confirmedImageRetestFailure(task), task.HasReplaySnapshot
	} else {
		var job model.AsyncMediaJob
		err := model.DB.WithContext(ctx).Select("task_id", "user_id", "token_id", "model", "channel_id", "status", "storage_status", "billing_status", "quota", "output_object_key", "has_replay_snapshot").Where("task_id = ?", taskID).Take(&job).Error
		if err != nil {
			return source, false, false, err
		}
		source = service.MediaRetestSource{TaskID: job.TaskId, UserID: job.UserId, TokenID: job.TokenId, Model: job.Model, ChannelID: job.ChannelId, MediaType: mediaType}
		confirmed, hasSnapshot = confirmedVideoRetestFailure(job), job.HasReplaySnapshot
		if confirmed && hasSnapshot {
			var native model.Task
			err := model.DB.WithContext(ctx).Select("status").Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&native).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return source, false, hasSnapshot, err
			}
			confirmed = errors.Is(err, gorm.ErrRecordNotFound) || native.Status == model.TaskStatusFailure
		}
	}
	_, err := service.CanvasTaskOwnerManaged(ctx, source.TokenID, source.UserID)
	return source, confirmed, hasSnapshot, err
}

type admittedMediaRetest struct {
	TaskID      string `gorm:"column:task_id"`
	RequestHash string
}

func admittedMediaRetestTask(ctx context.Context, mediaType string, token model.Token, operationKey string) (admittedMediaRetest, error) {
	var task admittedMediaRetest
	keyHash := service.ImageIdentityHash(operationKey)
	var err error
	if mediaType == model.MediaReplayImage {
		err = model.DB.WithContext(ctx).Model(&model.AsyncImageIdempotency{}).
			Select("async_image_idempotencies.task_id", "async_image_idempotencies.request_hash").
			Joins("JOIN async_image_tasks ON async_image_tasks.task_id = async_image_idempotencies.task_id AND async_image_tasks.token_id = async_image_idempotencies.token_id").
			Where("async_image_idempotencies.token_id = ? AND async_image_idempotencies.key_hash = ? AND async_image_tasks.user_id = ?", token.Id, keyHash, token.UserId).Take(&task).Error
	} else {
		identityHash := service.ImageIdentityHash("async-video-idempotency", strconv.Itoa(token.Id), keyHash)
		err = model.DB.WithContext(ctx).Model(&model.AsyncMediaJob{}).Select("task_id", "request_hash").Where("identity_hash = ? AND user_id = ? AND token_id = ?", identityHash, token.UserId, token.Id).Take(&task).Error
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = nil
	}
	return task, err
}

func respondAdmittedMediaRetest(c *gin.Context, mediaType string, previous admittedMediaRetest, requestHash string) bool {
	if previous.TaskID == "" {
		return false
	}
	if previous.RequestHash != requestHash && (requestHash != "" || strings.HasPrefix(previous.RequestHash, "retest:")) {
		mediaRetestError(c, http.StatusConflict, "idempotency_conflict", "This idempotency key was used for a different retest selection")
		return true
	}
	imageManagementData(c, gin.H{"task_id": previous.TaskID, "media_type": mediaType})
	return true
}

func mediaRetestError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"success": false, "code": code, "message": message})
}

func recordMediaRetest(c *gin.Context, actor int, source, target string, tokenID int, selection mediaRetestSelection, originalModel string) {
	role := c.GetInt("role")
	admin := &model.AuditAdminInfo{AdminID: actor, AdminUsername: c.GetString("username"), AdminRole: role, AuthMethod: "session"}
	info := &model.AuditRequestInfo{Method: c.Request.Method, Route: c.FullPath(), Path: c.FullPath(), Status: http.StatusOK, Success: true}
	params := map[string]any{"source_task_id": source, "new_task_id": target, "api_key_id": tokenID, "model": originalModel}
	if selection.ChannelID != nil {
		params["channel_id"] = *selection.ChannelID
		params["model"] = *selection.Model
	}
	audit := c.Copy()
	audit.Request = c.Request.WithContext(context.WithoutCancel(c.Request.Context()))
	model.RecordOperationAuditLog(actor, role, "Retest media task", c.ClientIP(), "media_task.retest", params, admin, info, audit)
	common.SetContextKey(c, constant.ContextKeyAuditLogged, true)
}
