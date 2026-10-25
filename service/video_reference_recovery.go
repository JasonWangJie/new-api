package service

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// AsyncVideoReferenceInput identifies a slot in the canonical request. Display
// snapshots and download completion order must never determine these positions.
type AsyncVideoReferenceInput struct {
	Index     int    `json:"index"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	Source    string `json:"source"`
	Path      string `json:"path"`
	KindIndex int    `json:"kind_index"`
	ObjectId  string `json:"object_id,omitempty"`
}

var SubmitAsyncVideoReferenceRetryFunc func(context.Context, model.AsyncMediaJob, model.Task, AsyncVideoRequest) error

// AsyncVideoReferenceRejectedError is reserved for a parsed, explicit upstream
// failure. Transport errors and unparseable receipts remain unknown.
type AsyncVideoReferenceRejectedError struct{ Reason string }

func (err *AsyncVideoReferenceRejectedError) Error() string { return err.Reason }

var videoReferenceTimeoutPattern = regexp.MustCompile(`^(?:[（(](?:图片|视频|音频)[0-9]{1,2}(?:[、,，](?:图片|视频|音频)?[0-9]{1,2})*[）)]\s*)?素材下载超时[，,]请联系管理员处理[。.]?$`)

// FreezeAsyncVideoReferenceRecovery is called only for a validated Drama
// request before its first dispatch. It captures the canonical reference order.
func FreezeAsyncVideoReferenceRecovery(request *AsyncVideoRequest, canonical any, expiresAt int64) error {
	if request == nil || request.Execution == nil || request.Execution.TaskPlugin == nil || request.Execution.TaskPlugin.Key != "drama" {
		return nil
	}
	body, ok := canonical.(map[string]any)
	if !ok {
		return nil
	}
	references, ok := body["references"].([]any)
	if !ok || len(references) == 0 || len(references) > 64 {
		return nil
	}
	now := time.Now().Unix()
	if constant.TaskTimeoutMinutes <= 0 {
		return nil
	}
	deadline := now + int64(constant.TaskTimeoutMinutes)*60
	// A replacement may be dispatched just before the original deadline. If
	// no TTL was supplied, cover the provider's maximum permitted TTL without
	// inserting or changing a generation parameter in the request.
	upstreamLifetime := int64(259200)
	if value, exists := body["execution_expires_after"]; exists {
		seconds, valid := videoNumber(value)
		if !valid || seconds < 3600 || seconds > 259200 {
			return errors.New("invalid video reference execution expiry")
		}
		upstreamLifetime = int64(seconds)
	}
	urlExpiresAt := deadline + upstreamLifetime + 3600
	if deadline <= now || urlExpiresAt > now+7*86400 || urlExpiresAt > expiresAt {
		return nil
	}
	counts := make(map[string]int)
	inputs := make([]AsyncVideoReferenceInput, len(references))
	external := false
	for index, value := range references {
		reference, ok := value.(map[string]any)
		if !ok {
			return errors.New("invalid canonical video reference")
		}
		kind, _ := reference["type"].(string)
		role, _ := reference["role"].(string)
		source, _ := reference["source"].(string)
		if (kind != "image" && kind != "video" && kind != "audio") || source == "" {
			return errors.New("invalid canonical video reference slot")
		}
		counts[kind]++
		inputs[index] = AsyncVideoReferenceInput{Index: index, Kind: kind, Role: role, Source: source, Path: "/references/" + strconv.Itoa(index) + "/source", KindIndex: counts[kind]}
		external = external || strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "http://")
	}
	if !external {
		return nil
	}
	encoded, err := common.Marshal(body)
	if err != nil {
		return err
	}
	request.Body, request.ContentType = encoded, "application/json"
	request.ReferenceInputs, request.ReferenceRecoveryVersion = inputs, 1
	request.ReferenceRecoveryDeadline, request.ReferenceURLExpiresAt = deadline, urlExpiresAt
	return nil
}

// TryAsyncVideoReferenceRecovery runs before the terminal billing transition.
// A transport or ambiguous polling failure must never reach this decision.
func TryAsyncVideoReferenceRecovery(ctx context.Context, task model.Task, result *relaycommon.TaskInfo, httpStatus int, response []byte) (bool, error) {
	if !task.PrivateData.AsyncMedia || task.Platform != "drama" || task.AttemptGeneration != 0 || result == nil || result.Status != string(model.TaskStatusFailure) || httpStatus < http.StatusOK || httpStatus >= http.StatusMultipleChoices || result.Url != "" || result.RemoteUrl != "" || task.PrivateData.ResultURL != "" || !videoReferenceTimeoutPattern.MatchString(strings.TrimSpace(result.Reason)) {
		return false, nil
	}
	var snapshot map[string]any
	if common.Unmarshal(response, &snapshot) != nil || snapshot == nil || snapshot["status"] != "failed" || videoReferenceOutputEvidence(snapshot, result.Reason, 0) {
		return false, nil
	}
	if result.TaskID != "" && result.TaskID != task.PrivateData.UpstreamTaskID {
		return false, nil
	}
	for _, field := range []string{"id", "task_id", "taskId"} {
		if identity, exists := snapshot[field]; exists && identity != task.PrivateData.UpstreamTaskID {
			return false, nil
		}
	}
	var job model.AsyncMediaJob
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", task.TaskID, task.UserId).Take(&job).Error; err != nil {
		return false, err
	}
	if job.ReferenceRetryCount != 0 || job.BillingStatus != "reserved" || len(job.RequestCipher) == 0 {
		return false, nil
	}
	plain, err := DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
	if err != nil {
		return false, nil
	}
	var request AsyncVideoRequest
	if common.Unmarshal(plain, &request) != nil || request.ReferenceRecoveryVersion != 1 || len(request.ReferenceInputs) == 0 || len(request.Body) == 0 || request.ReferenceRecoveryDeadline <= time.Now().Unix() || request.ReferenceURLExpiresAt <= time.Now().Unix() || request.Channel.Id != task.ChannelId || request.Channel.Key == "" {
		return false, nil
	}
	if task.PrivateData.DurableBilling && task.PrivateData.SubmitAccountingRecorded && task.Quota > 0 && job.AttemptGeneration == task.AttemptGeneration &&
		(job.Status == "submitting" || job.Status == "queued" || job.Status == "submitted" && job.Quota != task.Quota) {
		// A receipt can become pollable just before the submission worker
		// publishes its final quota/status projection. Let that worker (or its
		// lease recovery) finish instead of refunding this recoverable failure.
		return true, nil
	}
	return model.ScheduleAsyncVideoReferenceRecovery(ctx, task, job, SanitizeAsyncImageText(result.Reason, common.LocalLogContentLimit))
}

func videoReferenceOutputEvidence(value any, diagnostic string, depth int) bool {
	if depth > 16 {
		return true
	}
	switch typed := value.(type) {
	case map[string]any:
		for field, child := range typed {
			name := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(field))
			if name == "prompt" || name == "input" || name == "request" || name == "references" {
				continue
			}
			if name == "success" || name == "succeeded" || name == "completed" {
				if successful, ok := child.(bool); ok && successful {
					return true
				}
			}
			if name == "errorcode" || name == "code" {
				switch code := child.(type) {
				case nil:
				case string:
					if code = strings.TrimSpace(code); code != "" && code != "0" {
						return true
					}
				case float64:
					if code != 0 {
						return true
					}
				default:
					return true
				}
			}
			if text, ok := child.(string); ok {
				text = strings.ToLower(strings.TrimSpace(text))
				if name == "status" && (text == "success" || text == "succeeded" || text == "completed" || text == "done") {
					return true
				}
				if name == "error" || name == "errors" || name == "reason" || name == "message" || name == "type" {
					for _, excluded := range []string{"safety", "censor", "policy", "violation", "invalid", "parameter", "auth", "forbidden", "permission", "rate_limit", "quota", "审核", "违规", "不合规", "参数", "安全", "鉴权", "权限"} {
						if strings.Contains(text, excluded) {
							return true
						}
					}
				}
			}
			if name == "url" || name == "remoteurl" || name == "videourl" || name == "lastframeurl" || name == "outputurl" {
				if text, ok := child.(string); ok && strings.TrimSpace(text) != "" && strings.TrimSpace(text) != strings.TrimSpace(diagnostic) {
					return true
				}
			}
			if name == "result" || name == "results" || name == "output" || name == "outputs" || name == "artifacts" || name == "video" || name == "videos" || name == "image" || name == "images" || name == "audio" || name == "audios" || name == "file" || name == "files" || name == "b64json" || name == "b64video" || name == "bytesbase64encoded" || name == "base64" || name == "lastframe" {
				switch item := child.(type) {
				case nil:
				case string:
					if strings.TrimSpace(item) != "" {
						return true
					}
				case []any:
					if len(item) > 0 {
						return true
					}
				case map[string]any:
					if len(item) > 0 {
						return true
					}
				default:
					return true
				}
			}
			if videoReferenceOutputEvidence(child, diagnostic, depth+1) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if videoReferenceOutputEvidence(child, diagnostic, depth+1) {
				return true
			}
		}
	}
	return false
}

// ProcessAsyncVideoReferenceRecovery owns its lease separately from the normal
// status worker, which must not interpret the failed attempt as task failure.
func ProcessAsyncVideoReferenceRecovery(parent context.Context, job model.AsyncMediaJob, cfg MediaRuntimeConfig) error {
	var task model.Task
	if err := model.DB.WithContext(parent).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&task).Error; err != nil {
		return err
	}
	if task.AttemptGeneration != job.AttemptGeneration {
		return model.ErrImageConflict
	}
	updates := map[string]any{"lease_token": "", "lease_expires_at": 0, "next_attempt_at": 0}
	defer func() {
		if err := model.UpdateAsyncMediaJob(context.WithoutCancel(parent), job, updates); err != nil && !errors.Is(err, model.ErrImageConflict) {
			common.SysError("video reference recovery lease: " + err.Error())
		} else if err == nil && updates["reference_recovery_stage"] == "complete" {
			if err := RetireMediaInputObjects(context.WithoutCancel(parent), &task); err != nil {
				common.SysError("media input retirement: " + err.Error())
			}
		}
	}()
	if task.Status == model.TaskStatusFailure || task.Status == model.TaskStatusSuccess {
		job.Status = "failed"
		updates["status"], updates["billing_status"], updates["quota"], updates["reference_recovery_stage"] = "failed", "refunded", task.Quota, "complete"
		updates["error_message"] = task.FailReason
		if task.Status == model.TaskStatusSuccess {
			job.Status = "succeeded"
			updates["status"], updates["billing_status"], updates["storage_status"], updates["next_attempt_at"] = "submitted", "settled", "ready", time.Now().Unix()
		}
		updates["request_cipher"] = retainAsyncVideoRequestDetails(job)
		return nil
	}
	if job.ReferenceRecoveryStage == "dispatched" || job.ReferenceRecoveryStage == "unknown" {
		_, err := model.MarkAsyncVideoReferenceSubmissionUnknown(context.WithoutCancel(parent), job, task, "Upstream recovery submission could not be confirmed; the video operation will not be repeated")
		return err
	}
	var request AsyncVideoRequest
	var err error
	if job.ReferenceRecoveryStage == "refund" {
		err = errors.New(job.ErrorMessage)
	} else {
		var plain []byte
		plain, err = DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
		if err == nil {
			err = common.Unmarshal(plain, &request)
		}
	}
	if err == nil && (request.ReferenceRecoveryVersion != 1 || request.ReferenceRecoveryDeadline <= time.Now().Unix()) {
		err = errors.New("video reference recovery deadline expired")
	}
	if err == nil && job.ReferenceRecoveryStage == "prepare" {
		deadline := time.Unix(request.ReferenceRecoveryDeadline, 0)
		if downloadDeadline := time.Now().Add(time.Duration(cfg.DownloadTimeout) * time.Second); downloadDeadline.Before(deadline) {
			deadline = downloadDeadline
		}
		ctx, cancel := context.WithDeadline(parent, deadline)
		defer cancel()
		request, err = PrepareAsyncVideoLocalReferences(ctx, job, request)
		if err == nil {
			encoded, encodeErr := common.Marshal(request)
			var cipher []byte
			if encodeErr == nil {
				cipher, encodeErr = EncryptImagePayload(encoded, "media-request:"+job.TaskId)
			}
			if encodeErr == nil {
				encodeErr = model.UpdateAsyncMediaJob(ctx, job, map[string]any{"reference_recovery_stage": "submit", "request_cipher": cipher})
			}
			if encodeErr != nil {
				err = encodeErr
			} else {
				// Release the download slot before taking a submission slot. Both
				// phases retain the original deadline and generation.
				updates["reference_recovery_stage"], updates["next_attempt_at"] = "submit", time.Now().Unix()
				return nil
			}
		}
	}
	if err == nil {
		if SubmitAsyncVideoReferenceRetryFunc == nil {
			err = errors.New("video reference recovery submission is unavailable")
		} else {
			ctx, cancel := context.WithDeadline(parent, time.Unix(request.ReferenceRecoveryDeadline, 0))
			defer cancel()
			err = SubmitAsyncVideoReferenceRetryFunc(ctx, job, task, request)
		}
	}
	var current model.AsyncMediaJob
	if readErr := model.DB.WithContext(context.WithoutCancel(parent)).Where("id = ?", job.ID).Take(&current).Error; readErr != nil {
		return readErr
	}
	if current.AttemptGeneration != job.AttemptGeneration {
		return model.ErrImageConflict
	}
	if current.ReferenceRecoveryStage == "waiting" {
		updates["status"], updates["reference_recovery_stage"] = "submitted", "waiting"
		return nil
	}
	if current.ReferenceRecoveryStage == "dispatched" || current.ReferenceRecoveryStage == "unknown" {
		var rejected *AsyncVideoReferenceRejectedError
		if !errors.As(err, &rejected) {
			reason := "Upstream recovery submission could not be confirmed; the video operation will not be repeated"
			if err != nil {
				reason = VideoTaskFailureReason(reason + ": " + err.Error())
			}
			_, markErr := model.MarkAsyncVideoReferenceSubmissionUnknown(context.WithoutCancel(parent), job, task, reason)
			return errors.Join(err, markErr)
		}
	}
	if err == nil {
		err = errors.New("video reference recovery produced no durable task receipt")
	}
	reason := VideoTaskFailureReason(err.Error())
	// Persist the decision to stop generation before refunding. A failed
	// financial transaction must retry only this refund, never prepare inputs
	// or submit another upstream operation after the source becomes reachable.
	updates["reference_recovery_stage"], updates["error_message"] = "refund", reason
	if job.ReferenceRecoveryStage != "refund" {
		if persistErr := model.UpdateAsyncMediaJob(context.WithoutCancel(parent), job, map[string]any{"reference_recovery_stage": "refund", "error_message": reason}); persistErr != nil {
			updates["next_attempt_at"] = time.Now().Unix() + 3
			return persistErr
		}
	}
	task.Status, task.FailReason, task.Progress, task.FinishTime = model.TaskStatusFailure, reason, "100%", time.Now().Unix()
	if won, settleErr := SettleTaskBillingOnComplete(context.WithoutCancel(parent), nil, &task, model.TaskStatusQueued, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: task.FailReason}); settleErr != nil {
		updates["next_attempt_at"] = time.Now().Unix() + 3
		return settleErr
	} else if !won {
		return model.ErrImageConflict
	}
	updates["status"], updates["billing_status"], updates["reference_recovery_stage"] = "failed", "refunded", "complete"
	updates["quota"], updates["error_message"] = 0, task.FailReason
	job.Status = "failed"
	updates["request_cipher"] = retainAsyncVideoRequestDetails(job)
	return err
}
