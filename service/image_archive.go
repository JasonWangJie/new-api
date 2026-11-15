package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

type imageLibraryArchiveHandler struct{}

func (imageLibraryArchiveHandler) Type() string { return "image_library_archive" }
func (imageLibraryArchiveHandler) Enabled() bool {
	if model.DB == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var command model.ImageOutbox
	now := time.Now().Unix()
	result := model.DB.WithContext(ctx).Select("id").Where("kind = ? AND status = ? AND next_attempt_at <= ? AND lease_expires_at <= ?", "library_archive", "pending", now, now).Limit(1).Find(&command)
	return result.Error == nil && result.RowsAffected == 1
}
func (imageLibraryArchiveHandler) Interval() time.Duration { return 5 * time.Second }
func (imageLibraryArchiveHandler) NewPayload() any         { return nil }

func init() { RegisterSystemTaskHandler(imageLibraryArchiveHandler{}) }

func (imageLibraryArchiveHandler) Run(ctx context.Context, task *model.SystemTask, runner string) {
	cfg, err := GetImageRuntimeConfig(ctx)
	if err == nil {
		err = MaintainImageArchives(ctx, cfg)
	}
	if err != nil {
		failSystemTask(task, runner, errors.New("image library archive is waiting for database or storage availability"))
		return
	}
	if err := model.FinishSystemTask(task.TaskID, runner, model.SystemTaskStatusSucceeded, nil, ""); err != nil {
		logSystemTaskLockError(ctx, task, err)
	}
}

// MaintainImageArchives delivers one existing archive command with the normal
// upload budget. Cleanup's short time slices and DELETE rate never wrap it.
func MaintainImageArchives(parent context.Context, cfg ImageRuntimeConfig) error {
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.UploadTimeout)*time.Second)
	defer cancel()
	var command model.ImageOutbox
	now := time.Now().Unix()
	err := model.DB.WithContext(ctx).Where("kind = ? AND status = ? AND next_attempt_at <= ? AND lease_expires_at <= ?", "library_archive", "pending", now, now).Order("next_attempt_at,id").Take(&command).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	lease := common.GetUUID()
	claim := model.DB.WithContext(ctx).Model(&model.ImageOutbox{}).Where("id = ? AND status = ? AND lease_expires_at <= ?", command.Id, "pending", now).Updates(map[string]any{"lease_token": lease, "lease_expires_at": now + int64(cfg.UploadTimeout) + 30})
	if claim.Error != nil || claim.RowsAffected != 1 {
		return claim.Error
	}
	status := "skipped"
	if cfg.AutoArchive {
		status = "delivered"
		var task model.AsyncImageTask
		err = model.DB.WithContext(ctx).Where("task_id = ?", command.AggregateId).Take(&task).Error
		if err == nil {
			for i := range task.ImageCount {
				if _, _, err = ArchiveAsyncImageResult(ctx, task.UserId, task.TaskId, i, "", cfg); err != nil {
					break
				}
			}
		}
	}
	finish, stop := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
	defer stop()
	updates := map[string]any{"status": status, "lease_token": "", "lease_expires_at": 0}
	if err != nil {
		updates["status"], updates["next_attempt_at"] = "pending", time.Now().Unix()+int64(cfg.RetryBackoff)
	}
	result := model.DB.WithContext(finish).Model(&model.ImageOutbox{}).Where("id = ? AND status = ? AND lease_token = ?", command.Id, "pending", lease).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return model.ErrImageConflict
	}
	return err
}
