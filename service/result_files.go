package service

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const storedResultFilePath = "/v1/media/files/"

var storedResultFileIDPattern = regexp.MustCompile(`^(img_|vid_)[0-9a-f]{32}$`)

func storedResultFileName(fileID, contentType string) string {
	if !storedResultFileIDPattern.MatchString(fileID) {
		return ""
	}
	if contentType == "video/mp4" && strings.HasPrefix(fileID, "vid_") {
		return fileID + ".mp4"
	}
	if !strings.HasPrefix(fileID, "img_") {
		return ""
	}
	switch contentType {
	case "image/png":
		return fileID + ".png"
	case "image/jpeg":
		return fileID + ".jpg"
	case "image/webp":
		return fileID + ".webp"
	case "image/gif":
		return fileID + ".gif"
	case "image/svg+xml":
		return fileID + ".svg"
	default:
		return ""
	}
}

// ImageResultFileURL issues a shareable filename after the caller authorizes the task query.
func ImageResultFileURL(ctx context.Context, object model.ImageStorageObject, base string) (string, error) {
	now := time.Now().Unix()
	if _, err := imageResultFileTask(ctx, object, now); err != nil {
		return "", err
	}
	if link := ImageResultDeliveryURL(object); link != "" {
		return link, nil
	}
	return storedResultFileURL(ctx, &model.ImageStorageObject{}, object.ObjectId, object.ResultFileId, "img_", object.ContentType, base, now)
}

// MediaResultFileURL issues a shareable filename after the caller authorizes the task query.
func MediaResultFileURL(ctx context.Context, object model.MediaArtifactObject, base string) (string, error) {
	now := time.Now().Unix()
	if _, err := mediaResultFileJob(ctx, object, now); err != nil {
		return "", err
	}
	return storedResultFileURL(ctx, &model.MediaArtifactObject{}, object.ObjectId, object.ResultFileId, "vid_", object.MimeType, base, now)
}

func storedResultFileURL(ctx context.Context, objectModel any, objectID string, existing *string, prefix, contentType, base string, now int64) (string, error) {
	fileID := ""
	if existing != nil {
		fileID = *existing
	} else {
		fileID = prefix + common.GetUUID()
	}
	filename := storedResultFileName(fileID, contentType)
	if filename == "" {
		return "", gorm.ErrRecordNotFound
	}
	if existing == nil {
		lookup := model.DB.WithContext(ctx).Model(objectModel).Where("object_id = ? AND status = ? AND (expires_at = 0 OR expires_at > ?)", objectID, "active", now)
		updated := lookup.Session(&gorm.Session{Logger: gormlogger.Discard}).Where("result_file_id IS NULL").UpdateColumn("result_file_id", fileID)
		if updated.Error != nil {
			return "", updated.Error
		}
		if updated.RowsAffected == 0 {
			// A concurrent query may have issued the stable filename already.
			var issued struct{ ResultFileId *string }
			if err := lookup.Select("result_file_id").Take(&issued).Error; err != nil {
				return "", err
			}
			if issued.ResultFileId == nil {
				return "", gorm.ErrRecordNotFound
			}
			filename = storedResultFileName(*issued.ResultFileId, contentType)
			if filename == "" {
				return "", gorm.ErrRecordNotFound
			}
		}
	}
	return strings.TrimRight(base, "/") + storedResultFilePath + filename, nil
}

func ReadImageResultFile(ctx context.Context, filename string, now int64) (model.ImageStorageObject, model.AsyncImageTask, error) {
	var object model.ImageStorageObject
	var task model.AsyncImageTask
	fileID, ext, ok := strings.Cut(filename, ".")
	if !ok || !strings.HasPrefix(fileID, "img_") || !storedResultFileIDPattern.MatchString(fileID) {
		return object, task, gorm.ErrRecordNotFound
	}
	switch ext {
	case "png", "jpg", "webp", "gif", "svg":
	default:
		return object, task, gorm.ErrRecordNotFound
	}
	if err := model.DB.WithContext(ctx).Session(&gorm.Session{Logger: gormlogger.Discard}).Where("result_file_id = ?", fileID).Take(&object).Error; err != nil {
		return object, task, err
	}
	if object.ResultFileId == nil || filename != storedResultFileName(*object.ResultFileId, object.ContentType) {
		return object, task, gorm.ErrRecordNotFound
	}
	task, err := imageResultFileTask(ctx, object, now)
	return object, task, err
}

func imageResultFileTask(ctx context.Context, object model.ImageStorageObject, now int64) (model.AsyncImageTask, error) {
	var task model.AsyncImageTask
	if object.Status != "active" || object.ExpiresAt > 0 && object.ExpiresAt <= now || object.Class != "" && object.Class != "temporary" {
		return task, gorm.ErrRecordNotFound
	}
	db := model.DB.WithContext(ctx)
	var result model.AsyncImageResult
	if err := db.Where("object_id = ?", object.ObjectId).Take(&result).Error; err != nil {
		return task, err
	}
	if err := db.Where("task_id = ?", result.TaskId).Take(&task).Error; err != nil {
		return task, err
	}
	if !task.ResultsAvailable() || (task.ExpiresAt > 0 && task.ExpiresAt <= now) || task.ResultSource == model.ImageResultSourceUpstream || result.ImageIndex < 0 || result.ImageIndex >= task.ResultCount {
		return task, gorm.ErrRecordNotFound
	}
	var count int64
	if err := db.Model(&model.AsyncImageResult{}).Where("task_id = ?", task.TaskId).Count(&count).Error; err != nil {
		return task, err
	}
	if count != int64(task.ResultCount) {
		return task, gorm.ErrRecordNotFound
	}
	if managed, err := CanvasImageTaskManaged(ctx, task); err != nil {
		return task, err
	} else if managed {
		return task, gorm.ErrRecordNotFound
	}
	if managed, err := CanvasImageObjectManaged(ctx, object); err != nil {
		return task, err
	} else if managed {
		return task, gorm.ErrRecordNotFound
	}
	return task, nil
}

func ReadMediaResultFile(ctx context.Context, filename string, now int64) (model.MediaArtifactObject, model.AsyncMediaJob, error) {
	var object model.MediaArtifactObject
	var job model.AsyncMediaJob
	fileID, ok := strings.CutSuffix(filename, ".mp4")
	if !ok || !strings.HasPrefix(fileID, "vid_") || !storedResultFileIDPattern.MatchString(fileID) {
		return object, job, gorm.ErrRecordNotFound
	}
	if err := model.DB.WithContext(ctx).Session(&gorm.Session{Logger: gormlogger.Discard}).Where("result_file_id = ?", fileID).Take(&object).Error; err != nil {
		return object, job, err
	}
	if object.ResultFileId == nil || filename != storedResultFileName(*object.ResultFileId, object.MimeType) {
		return object, job, gorm.ErrRecordNotFound
	}
	job, err := mediaResultFileJob(ctx, object, now)
	return object, job, err
}

func mediaResultFileJob(ctx context.Context, object model.MediaArtifactObject, now int64) (model.AsyncMediaJob, error) {
	var job model.AsyncMediaJob
	if object.Status != "active" || object.ExpiresAt <= now {
		return job, gorm.ErrRecordNotFound
	}
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ? AND token_id = ? AND status = ? AND storage_status = ? AND expires_at > ?", object.TaskId, object.UserId, object.TokenId, "succeeded", "succeeded", now).Take(&job).Error; err != nil {
		return job, err
	}
	if job.BillingStatus == "failed" {
		return job, gorm.ErrRecordNotFound
	}
	if managed, err := CanvasTaskOwnerManaged(ctx, job.TokenId, job.UserId); err != nil {
		return job, err
	} else if managed {
		return job, gorm.ErrRecordNotFound
	}
	return job, nil
}
