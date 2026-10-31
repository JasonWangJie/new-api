package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/tidwall/gjson"
)

var tianyueImageAspectRatios = []string{"auto", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "1:1"}

// TianyueImageRequest uses the vendor's image contract for both generation and
// reference-image editing. The vendor bills one image via duration=1; arbitrary
// client multipliers never enter this payload.
func TianyueImageRequest(request AsyncImageRequest, references []string) (map[string]common.RawMessage, error) {
	if request.Count != 1 {
		return nil, errors.New("TianYue image requests require n=1")
	}
	for _, reference := range references {
		if _, err := ValidateImageReferenceURL(reference); err != nil {
			return nil, err
		}
	}
	ratio := request.AspectRatio
	if ratio == "" && request.Size != "" && request.Size != "auto" {
		widthText, heightText, ok := strings.Cut(request.Size, "x")
		width, widthErr := strconv.Atoi(widthText)
		height, heightErr := strconv.Atoi(heightText)
		if !ok || widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
			return nil, errors.New("TianYue image size must have valid dimensions or an explicit aspect_ratio")
		}
		divisor, remainder := width, height
		for remainder != 0 {
			divisor, remainder = remainder, divisor%remainder
		}
		ratio = fmt.Sprintf("%d:%d", width/divisor, height/divisor)
	}
	if ratio == "" || ratio == "auto" {
		ratio = "1:1"
	}
	if !slices.Contains(tianyueImageAspectRatios, ratio) {
		return nil, errors.New("TianYue image aspect_ratio must be auto, 16:9, 9:16, 4:3, 3:4, 3:2, 2:3, or 1:1")
	}
	fields := map[string]any{"model": request.Model, "prompt": request.Prompt, "duration": 1, "aspect_ratio": ratio}
	resolution := AsyncImageRoutingResolution(request)
	if resolution != "" && resolution != "AUTO" {
		fields["output_resolution"] = resolution
	}
	if len(references) > 0 {
		fields["images"] = references
	}
	if detail, exists := request.Native["detail_level"]; exists {
		var value string
		if common.Unmarshal(detail, &value) != nil {
			return nil, errors.New("invalid TianYue image detail_level")
		}
		fields["detail_level"] = value
	}
	encoded, err := common.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var native map[string]common.RawMessage
	err = common.Unmarshal(encoded, &native)
	return native, err
}

// TianyueAsyncImageProfile uses the vendor's asynchronous media endpoint for
// every image model, including models without the optional -yb suffix.
func TianyueAsyncImageProfile(modelName string) *dto.UpstreamAsyncProfile {
	return &dto.UpstreamAsyncProfile{
		ID: "tianyue-image", MediaType: dto.UpstreamAsyncMediaImage,
		Models: []string{modelName}, Operations: []string{dto.UpstreamAsyncOperationGenerate, dto.UpstreamAsyncOperationEdit},
		Submit: dto.UpstreamAsyncSubmit{TaskIDPath: "task_id", Request: &dto.UpstreamAsyncSubmitRequest{Method: "POST", Path: "/v1/video/generations", Headers: map[string]string{"Authorization": "Bearer {api_key}"}}},
		Poll: dto.UpstreamAsyncPoll{
			Request: dto.UpstreamAsyncPollRequest{Method: "GET", Path: "/v1/video/generations/{task_id}", Headers: map[string]string{"Authorization": "Bearer {api_key}"}},
			Response: dto.UpstreamAsyncPollResponse{
				StatusPath: "status", ProgressPath: "progress", FailureReasonPath: "reason", ResultPath: "url",
				StatusValues: dto.UpstreamAsyncStatusValues{
					Queued:     []dto.UpstreamAsyncStatusValue{dto.UpstreamAsyncStatusValue(`"QUEUED"`), dto.UpstreamAsyncStatusValue(`"NOT_START"`)},
					InProgress: []dto.UpstreamAsyncStatusValue{dto.UpstreamAsyncStatusValue(`"IN_PROGRESS"`), dto.UpstreamAsyncStatusValue(`"SUBMITTED"`)},
					Succeeded:  []dto.UpstreamAsyncStatusValue{dto.UpstreamAsyncStatusValue(`"SUCCESS"`)},
					Failed:     []dto.UpstreamAsyncStatusValue{dto.UpstreamAsyncStatusValue(`"FAILED"`), dto.UpstreamAsyncStatusValue(`"FAILURE"`)},
				},
				DownloadHeaders: map[string]string{"Authorization": "Bearer {api_key}"},
			},
		},
	}
}

// NormalizeTianyueImageTask handles the vendor's wrapped image tasks while
// retaining the host's existing receipt persistence, polling and download path.
func NormalizeTianyueImageTask(body []byte, baseURL string) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("TianYue image task response is invalid")
	}
	fields := make(map[string]any)
	for target, paths := range map[string][]string{
		"task_id":  {"data.task_id", "task_id", "id"},
		"status":   {"data.status", "status"},
		"progress": {"data.progress", "progress"},
		"reason":   {"data.fail_reason", "error.message", "data.data.error.message", "message"},
		"url":      {"data.data.content_url", "data.data.media_url", "data.data.video_url", "data.data.url", "data.result_url", "content_url", "media_url", "video_url", "url", "metadata.url"},
	} {
		for _, path := range paths {
			value := gjson.GetBytes(body, path)
			if !value.Exists() || value.Type == gjson.Null || value.Type == gjson.String && strings.TrimSpace(value.Str) == "" {
				continue
			}
			if target == "progress" && (value.Type == gjson.String || value.Type == gjson.Number) {
				fields[target] = value.Value()
				break
			}
			if value.Type != gjson.String {
				continue
			}
			text := strings.TrimSpace(value.Str)
			if target == "status" {
				text = strings.ToUpper(text)
				switch text {
				case "COMPLETED":
					text = "SUCCESS"
				case "RUNNING":
					text = "IN_PROGRESS"
				}
			}
			if target == "url" {
				parsed, err := url.Parse(text)
				if err != nil || parsed.User != nil || parsed.Fragment != "" {
					continue
				}
				if strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "//") {
					base, err := url.Parse(strings.TrimRight(baseURL, "/") + "/")
					if err != nil {
						continue
					}
					text = base.ResolveReference(parsed).String()
				}
			}
			fields[target] = text
			break
		}
	}
	return common.Marshal(fields)
}

// ClassifyTianyueImageTaskFailure permits the vendor's explicit temporary
// failure only for the accepted task being polled. The complete raw envelope
// must remain free of output or contradictory diagnostics before normalization
// drops fields that are not part of the host's status projection.
func ClassifyTianyueImageTaskFailure(body []byte, taskID string) *AsyncImageFailure {
	if taskID == "" {
		return nil
	}
	var envelope map[string]any
	if common.Unmarshal(body, &envelope) != nil || envelope == nil {
		return nil
	}
	normalized, err := NormalizeTianyueImageTask(body, "")
	if err != nil || gjson.GetBytes(normalized, "task_id").Str != taskID {
		return nil
	}
	reason := gjson.GetBytes(normalized, "reason").Str
	failure := &AsyncImageFailure{Code: 606}
	switch reason {
	case "生成失败，请稍后重试":
	case "无法下载参考图，请确认链接可直接访问，且未过期或要求登录":
		failure.Code, failure.RetrySameChannel = 602, true
	case "连接上游服务失败":
		failure.RetrySameChannel = true
	default:
		return nil
	}
	status := gjson.GetBytes(normalized, "status").Str
	if status != "FAILED" && status != "FAILURE" {
		return nil
	}
	task, wrapped := envelope["data"].(map[string]any)
	if wrapped {
		delete(envelope, "data")
	} else {
		task = envelope
	}
	scopes := []map[string]any{envelope, task}
	if native, ok := task["data"].(map[string]any); ok && len(native) > 0 {
		// The gateway's failed task may include its own failed provider task.
		// Only this closed operation is lineage, rather than a new receipt.
		nativeStatus, _ := native["status"].(string)
		nativeTaskID, _ := native["task_id"].(string)
		providerError, ok := native["error"].(map[string]any)
		if !ok || !strings.EqualFold(nativeStatus, "failed") || providerError["code"] != "task_failed" || providerError["message"] != reason {
			return nil
		}
		if nativeTaskID == "" {
			// Some connection failures close before the vendor receives an
			// inner task ID. The outer accepted task must still match exactly.
			if reason != "连接上游服务失败" || native["task_id"] != nil && native["task_id"] != "" {
				return nil
			}
		} else if !strings.HasPrefix(nativeTaskID, "task_") || len(nativeTaskID) > 191 {
			return nil
		}
		for _, field := range []string{"message", "fail_reason"} {
			if native[field] != reason && (reason != "连接上游服务失败" || native[field] != nil && native[field] != "") {
				return nil
			}
		}
		if failure.Code == 602 {
			// These two exact locations use "none" as a failed-task sentinel.
			// Actual URLs and output in every other location remain barriers.
			if native["url"] == "none" {
				delete(native, "url")
			}
			if metadata, ok := native["metadata"].(map[string]any); ok && metadata["url"] == "none" {
				delete(metadata, "url")
			}
		}
		delete(task, "data")
		delete(native, "status")
		delete(native, "task_id")
		delete(providerError, "code")
		scopes = append(scopes, native, providerError)
		delete(native, "error")
	}
	for index, scope := range scopes {
		// Failed gateway rows can copy their diagnostic into result_url.
		// Only the exact known reason is a placeholder rather than an output.
		if index == 1 && scope["result_url"] == reason {
			delete(scope, "result_url")
		}
		for _, field := range []string{"message", "fail_reason"} {
			if scope[field] == reason {
				delete(scope, field)
			}
		}
		for _, field := range []string{"task_id", "id"} {
			if value, exists := scope[field]; exists {
				// The outer gateway row has a numeric database ID in addition to
				// its authoritative public task_id; it is not an operation receipt.
				if number, ok := value.(float64); wrapped && index == 1 && field == "id" && ok && number > 0 && math.Trunc(number) == number {
					delete(scope, field)
					continue
				}
				if value != nil && value != "" && value != taskID {
					return nil
				}
				delete(scope, field)
			}
		}
		if value, exists := scope["status"]; exists {
			text, ok := value.(string)
			if value != nil && (!ok || text != "" && !strings.EqualFold(text, "FAILED") && !strings.EqualFold(text, "FAILURE")) {
				return nil
			}
			delete(scope, "status")
		}
		// "success" is the vendor envelope's transport code, not the failed
		// native task status. It is allowed only on the outer wrapped response.
		if index == 0 && wrapped && scope["code"] == "success" {
			delete(scope, "code")
		}
	}
	for _, scope := range scopes {
		if !isImageGatewayFailureMetadata(scope, http.StatusInternalServerError, "", 0) {
			return nil
		}
	}
	return failure
}

// ValidateTianyueImageSubmission runs after administrator parameter overrides,
// so even those cannot increase the vendor's per-image billing multiplier or
// turn a validated reference into Base64 or an unrelated object structure.
func ValidateTianyueImageSubmission(body []byte, count int) error {
	if count != 1 || gjson.GetBytes(body, "duration").Raw != "1" {
		return errors.New("TianYue image requests require one image and duration=1")
	}
	if ratio := gjson.GetBytes(body, "aspect_ratio"); ratio.Type != gjson.String || !slices.Contains(tianyueImageAspectRatios, ratio.Str) {
		return errors.New("TianYue image aspect_ratio must be auto, 16:9, 9:16, 4:3, 3:4, 3:2, 2:3, or 1:1")
	}
	if value := gjson.GetBytes(body, "images"); value.Exists() {
		if !value.IsArray() {
			return errors.New("TianYue image references must be a URL array")
		}
		for _, reference := range value.Array() {
			if reference.Type != gjson.String {
				return errors.New("TianYue image references must be URL strings")
			}
			if _, err := ValidateImageReferenceURL(reference.Str); err != nil {
				return err
			}
		}
	}
	return nil
}

// TianyueImageResultProfile keeps channel credentials on the channel origin.
// Vendor responses also contain public CDN URLs, which must be fetched
// anonymously through the existing public-address and redirect validation.
func TianyueImageResultProfile(profile *dto.UpstreamAsyncProfile, rawURL, baseURL string) (*dto.UpstreamAsyncProfile, error) {
	result, err := url.Parse(rawURL)
	if err != nil || result.Host == "" || (result.Scheme != "http" && result.Scheme != "https") || result.User != nil || result.Fragment != "" {
		return nil, errors.New("TianYue image result URL is invalid")
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" || profile == nil {
		return nil, errors.New("TianYue image result configuration is invalid")
	}
	if sameUpstreamAsyncOrigin(base, result) {
		return profile, nil
	}
	anonymous := *profile
	anonymous.Poll.Response.DownloadHeaders = nil
	return &anonymous, nil
}

func DownloadTianyueImages(ctx context.Context, urls []string, profile *dto.UpstreamAsyncProfile, baseURL, proxy string, values UpstreamAsyncTemplateContext, cfg ImageRuntimeConfig) ([]ImageBytes, error) {
	if len(urls) == 0 || len(urls) > dto.MaxImageN {
		return nil, errors.New("TianYue image result count is invalid")
	}
	images := make([]ImageBytes, 0, len(urls))
	for _, rawURL := range urls {
		scoped, err := TianyueImageResultProfile(profile, rawURL, baseURL)
		if err != nil {
			return nil, err
		}
		items, err := DownloadUpstreamAsyncImages(ctx, []string{rawURL}, scoped, baseURL, proxy, values, cfg)
		if err != nil {
			return nil, err
		}
		images = append(images, items...)
	}
	return images, nil
}

// TianyueImageReferenceURL grants access to one validated reference in the
// encrypted task request. The capability expires independently of API tokens
// and becomes unusable as soon as the task stops invoking or its owner revokes
// access. It creates no additional storage objects or cleanup obligations.
func TianyueImageReferenceURL(task model.AsyncImageTask, index int, cfg ImageRuntimeConfig) (string, error) {
	base := strings.TrimSuffix(strings.TrimRight(system_setting.ServerAddress, "/"), "/v1")
	if _, err := ValidateImageReferenceURL(base + "/"); err != nil {
		return "", errors.New("TianYue reference images require a public HTTPS server address")
	}
	keys, active, err := imagePayloadKeys()
	if err != nil {
		return "", err
	}
	deadline := time.Now().Unix() + int64(min(cfg.SignedURLExpiry, cfg.ExecutionTimeout))
	if task.StartedAt > 0 {
		deadline = min(deadline, model.AsyncImageExecutionDeadline(task, cfg.ExecutionTimeout))
	}
	if deadline <= time.Now().Unix() || index < 0 || task.TaskId == "" {
		return "", errors.New("image reference is unavailable")
	}
	part := strconv.Itoa(index)
	expires := strconv.FormatInt(deadline, 10)
	signature := tianyueReferenceSignature(keys[active], task.TaskId, part, expires)
	query := url.Values{"expires": {expires}, "key": {active}, "signature": {base64.RawURLEncoding.EncodeToString(signature)}}
	return base + "/api/image-task-references/" + url.PathEscape(task.TaskId) + "/" + part + "/content?" + query.Encode(), nil
}

func tianyueReferenceSignature(key []byte, taskID, index, expires string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("new-api:tianyue-image-reference:v1:" + taskID + ":" + index + ":" + expires))
	return mac.Sum(nil)
}

// ReadTianyueImageReference rechecks persisted ownership and revocation before
// resolving the original input. A signature cannot select another task, part,
// expiry, storage object, or browser/API authentication context.
func ReadTianyueImageReference(ctx context.Context, taskID, index, expires, keyID, signature string) (ImageBytes, error) {
	unavailable := errors.New("image reference is unavailable")
	deadline, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || deadline <= time.Now().Unix() {
		return ImageBytes{}, unavailable
	}
	part, err := strconv.Atoi(index)
	if err != nil || part < 0 || strconv.Itoa(part) != index {
		return ImageBytes{}, unavailable
	}
	keys, _, err := imagePayloadKeys()
	if err != nil || keys[keyID] == nil {
		return ImageBytes{}, unavailable
	}
	provided, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || !hmac.Equal(provided, tianyueReferenceSignature(keys[keyID], taskID, index, expires)) {
		return ImageBytes{}, unavailable
	}
	var task model.AsyncImageTask
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND status = ?", taskID, model.ImageTaskInvoking).Take(&task).Error; err != nil {
		return ImageBytes{}, unavailable
	}
	if task.ExpiresAt > 0 && task.ExpiresAt <= time.Now().Unix() {
		return ImageBytes{}, unavailable
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", task.TokenId, task.UserId).Take(&token).Error; err != nil ||
		(token.Status != common.TokenStatusEnabled && token.Status != common.TokenStatusExhausted) ||
		(token.ExpiredTime != -1 && token.ExpiredTime <= time.Now().Unix()) {
		return ImageBytes{}, unavailable
	}
	if model.IsCanvasTokenKey(token.Key) {
		if _, _, _, err := ValidateCanvasWorkerCredential(ctx, token.Id, task.UserId); err != nil {
			return ImageBytes{}, unavailable
		}
	} else {
		var owner model.User
		if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", task.UserId, common.UserStatusEnabled).Take(&owner).Error; err != nil {
			return ImageBytes{}, unavailable
		}
	}
	payload, err := DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
	if err != nil {
		return ImageBytes{}, unavailable
	}
	var request AsyncImageRequest
	if err := common.Unmarshal(payload, &request); err != nil || part >= len(request.Parts) || request.Parts[part].Type != "image_url" {
		return ImageBytes{}, unavailable
	}
	cfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return ImageBytes{}, unavailable
	}
	image, _, err := ResolveImageReference(ctx, task.TokenId, request.Parts[part].URL, cfg)
	if err != nil {
		return ImageBytes{}, unavailable
	}
	return image, nil
}
