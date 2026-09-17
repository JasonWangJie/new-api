package service

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

type ImageCleanupFilters struct {
	UserId int `json:"user_id"`
}

func ImageCleanupCandidates(ctx context.Context, scope string, filters ImageCleanupFilters) (*gorm.DB, error) {
	if scope != "expired" && scope != "deleted" && scope != "user" && scope != "async_results" && scope != "all" {
		return nil, errors.New("invalid image cleanup scope")
	}
	if filters.UserId < 0 || (scope == "user" && filters.UserId < 1) {
		return nil, errors.New("a user ID is required for user cleanup")
	}
	now := time.Now().Unix()
	query := model.DB.WithContext(ctx).Model(&model.ImageStorageObject{}).Where("status IN ?", []string{"active", "deleting"})
	if scope == "expired" {
		query = query.Where("expires_at > 0 AND expires_at <= ?", now)
	}
	if scope == "deleted" {
		query = query.Where("object_id IN (?)", model.DB.WithContext(ctx).Model(&model.ImageLibraryItem{}).Select("object_id").Where("deleted_at > 0"))
	}
	if scope == "async_results" {
		query = query.Where("object_id IN (?)", model.DB.WithContext(ctx).Model(&model.AsyncImageResult{}).Select("object_id"))
	}
	if filters.UserId > 0 {
		query = query.Where("object_id IN (?) OR object_id IN (?)", model.DB.WithContext(ctx).Model(&model.ImageLibraryItem{}).Select("object_id").Where("user_id = ?", filters.UserId), model.DB.WithContext(ctx).Model(&model.ImageUploadIntent{}).Select("image_storage_objects.object_id").Joins("JOIN image_storage_objects ON image_storage_objects.profile_id = image_upload_intents.profile_id AND image_storage_objects.object_key = image_upload_intents.object_key").Where("image_upload_intents.user_id = ?", filters.UserId))
	}
	// Live references are excluded at preview time and checked again under an
	// object lock immediately before physical deletion.
	query = query.Where("object_id NOT IN (?)", model.DB.WithContext(ctx).Model(&model.ImageLibraryItem{}).Select("object_id").Where("deleted_at = 0 AND (expires_at = 0 OR expires_at > ?)", now))
	query = query.Where("object_id NOT IN (?)", model.DB.WithContext(ctx).Model(&model.ImageLibraryItem{}).Select("image_library_items.object_id").Joins("JOIN image_publications ON image_publications.asset_id = image_library_items.asset_id").Where("image_publications.status IN ? AND (image_publications.expires_at = 0 OR image_publications.expires_at > ?)", []string{"pending_review", "published", "hidden"}, now))
	query = query.Where("object_id NOT IN (?)", model.DB.WithContext(ctx).Model(&model.AsyncImageResult{}).Select("async_image_results.object_id").Joins("JOIN async_image_tasks ON async_image_tasks.task_id = async_image_results.task_id").Where("async_image_tasks.expires_at > ? OR async_image_tasks.status NOT IN ?", now, []string{model.ImageTaskSucceeded, model.ImageTaskFailed, model.ImageTaskExpired, model.ImageTaskExecutionUnknown}))
	query = query.Where("object_id NOT IN (?)", model.DB.WithContext(ctx).Model(&model.ImageInputObject{}).Select("object_id").Where("object_id <> '' AND status IN ? AND (expires_at > ? OR lease_expires_at > ?)", []string{"active", "reserved", "uploading"}, now, now))
	return query, nil
}

func RunImageCleanupJob(ctx context.Context, job model.ImageCleanupJob, cfg ImageRuntimeConfig) error {
	lease := common.GetUUID()
	now := time.Now().Unix()
	result := model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND status IN ? AND lease_expires_at <= ?", job.Id, []string{"queued", "running"}, now).Updates(map[string]any{"status": "running", "lease_token": lease, "lease_expires_at": now + int64(cfg.WorkerLease)})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return nil
	}
	var filters ImageCleanupFilters
	if err := common.UnmarshalJsonStr(job.Filters, &filters); err != nil {
		return err
	}
	query, err := ImageCleanupCandidates(ctx, job.Scope, filters)
	if err != nil {
		return err
	}
	var objects []model.ImageStorageObject
	if err := query.Where("id > ?", job.Cursor).Order("id").Limit(20).Find(&objects).Error; err != nil {
		return err
	}
	for _, candidate := range objects {
		object, err := model.ClaimImageObjectDeletion(ctx, candidate.ObjectId, lease, time.Now().Unix(), cfg.WorkerLease)
		if errors.Is(err, model.ErrImageConflict) || errors.Is(err, gorm.ErrRecordNotFound) {
			job.Cursor = candidate.Id
			continue
		}
		if err == nil {
			var store *ImageStorage
			store, err = GetImageObjectStorage(ctx, object)
			if err == nil {
				err = store.Delete(ctx, object.ObjectKey)
			}
		}
		if err == nil {
			update := model.DB.WithContext(ctx).Model(&model.ImageStorageObject{}).Where("id = ? AND status = ? AND delete_lease = ?", object.Id, "deleting", lease).Updates(map[string]any{"status": "deleted", "delete_lease": "", "delete_lease_expires_at": 0})
			err = update.Error
			if err == nil && update.RowsAffected != 1 {
				err = model.ErrImageConflict
			}
		}
		job.Processed++
		if err != nil {
			job.Errors++
			job.LastError = "Image deletion could not be confirmed"
		} else {
			job.DeletedBytes += object.ByteSize
		}
		job.Cursor = candidate.Id
	}
	status := "queued"
	finished := int64(0)
	if len(objects) < 20 {
		status = "completed"
		if job.Errors > 0 {
			status = "completed_with_errors"
		}
		finished = time.Now().Unix()
	}
	return model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ?", job.Id, lease).Updates(map[string]any{"cursor": job.Cursor, "status": status, "processed": job.Processed, "deleted_bytes": job.DeletedBytes, "errors": job.Errors, "last_error": job.LastError, "lease_token": "", "lease_expires_at": 0, "finished_at": finished}).Error
}

func MaintainImageStorage(ctx context.Context, cfg ImageRuntimeConfig) error {
	var failures []error
	var jobs []model.ImageCleanupJob
	if err := model.DB.WithContext(ctx).Where("status IN ? AND lease_expires_at <= ?", []string{"queued", "running"}, time.Now().Unix()).Order("id").Limit(2).Find(&jobs).Error; err != nil {
		return err
	}
	for _, job := range jobs {
		if err := RunImageCleanupJob(ctx, job, cfg); err != nil {
			failures = append(failures, err)
		}
	}
	var archives []model.ImageOutbox
	if err := model.DB.WithContext(ctx).Where("kind = ? AND status = ? AND next_attempt_at <= ?", "library_archive", "pending", time.Now().Unix()).Order("id").Limit(5).Find(&archives).Error; err != nil {
		return err
	}
	for _, command := range archives {
		if !cfg.AutoArchive {
			if err := model.DB.WithContext(ctx).Model(&model.ImageOutbox{}).Where("id = ? AND status = ?", command.Id, "pending").Update("status", "skipped").Error; err != nil {
				return err
			}
			continue
		}
		var task model.AsyncImageTask
		if err := model.DB.WithContext(ctx).Where("task_id = ?", command.AggregateId).Take(&task).Error; err != nil {
			return err
		}
		var archiveError error
		for i := range task.ImageCount {
			if _, _, err := ArchiveAsyncImageResult(ctx, task.UserId, task.TaskId, i, "", cfg); err != nil {
				archiveError = err
				break
			}
		}
		if archiveError != nil {
			failures = append(failures, archiveError)
			if err := model.DB.WithContext(ctx).Model(&model.ImageOutbox{}).Where("id = ? AND status = ?", command.Id, "pending").Update("next_attempt_at", time.Now().Unix()+int64(cfg.RetryBackoff)).Error; err != nil {
				return err
			}
			continue
		}
		if err := model.DB.WithContext(ctx).Model(&model.ImageOutbox{}).Where("id = ? AND status = ?", command.Id, "pending").Update("status", "delivered").Error; err != nil {
			return err
		}
	}
	failures = append(failures, ReapImageInputIntents(ctx, cfg))
	failures = append(failures, ReapUnconfirmedImageIntents(ctx, cfg))
	// Accounting and idempotency tombstones remain durable. Expired task
	// content is erased without making an old request eligible for execution.
	failures = append(failures, model.EraseExpiredAsyncImageTaskContent(ctx))
	failures = append(failures, model.DB.WithContext(ctx).Where("action = ? AND created_at < ?", "import_attempt", time.Now().Unix()-86400).Delete(&model.ImageModerationEvent{}).Error)
	return errors.Join(failures...)
}

func ReapUnconfirmedImageIntents(ctx context.Context, cfg ImageRuntimeConfig) error {
	now := time.Now().Unix()
	var intents []model.ImageUploadIntent
	query := model.DB.WithContext(ctx).Where("status IN ? AND created_at <= ? AND lease_expires_at <= ?", []string{"pending", "uploading", "deleting"}, now-86400, now)
	query = query.Where("intent_key NOT IN (?)", model.DB.WithContext(ctx).Model(&model.ImageInputObject{}).Select("intent_key"))
	if err := query.Order("id").Limit(20).Find(&intents).Error; err != nil {
		return err
	}
	for _, candidate := range intents {
		lease := common.GetUUID()
		intent, err := model.ClaimUnconfirmedImageIntent(ctx, candidate.Id, lease, cfg.WorkerLease, now)
		if errors.Is(err, model.ErrImageConflict) || errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		var profile model.ImageStorageProfile
		if err := model.DB.WithContext(ctx).Where("profile_id = ?", intent.ProfileId).Take(&profile).Error; err != nil {
			return err
		}
		store, err := OpenImageStorage(ctx, profile)
		if err != nil {
			return err
		}
		if err := store.Delete(ctx, intent.ObjectKey); err != nil {
			continue
		}
		updates := map[string]any{"first_delete_at": now, "lease_token": "", "lease_expires_at": 0}
		if intent.FirstDeleteAt > 0 {
			updates["status"] = "deleted"
		}
		if err := model.DB.WithContext(ctx).Model(&model.ImageUploadIntent{}).Where("id = ? AND status = ? AND lease_token = ?", intent.Id, "deleting", lease).Updates(updates).Error; err != nil {
			return err
		}
	}
	return nil
}

// Reservations are released only after two confirmed deletes, ten minutes
// apart. Expired aliases remain as tombstones and can never become remote URLs.
func ReapImageInputIntents(ctx context.Context, cfg ImageRuntimeConfig) error {
	now := time.Now().Unix()
	var inputs []model.ImageInputObject
	if err := model.DB.WithContext(ctx).Where("status IN ? AND lease_expires_at <= ? AND (expires_at <= ? OR status <> ?)", []string{"reserved", "failed", "active", "deleting"}, now, now, "active").Order("id").Limit(20).Find(&inputs).Error; err != nil {
		return err
	}
	for _, input := range inputs {
		var references int64
		if err := model.DB.WithContext(ctx).Model(&model.ImageInputTaskReference{}).Joins("JOIN async_image_tasks ON async_image_tasks.task_id = image_input_task_references.task_id").Where("image_input_task_references.input_id = ? AND async_image_tasks.status NOT IN ?", input.InputId, []string{model.ImageTaskSucceeded, model.ImageTaskFailed, model.ImageTaskExpired, model.ImageTaskExecutionUnknown}).Count(&references).Error; err != nil {
			return err
		}
		if references > 0 {
			continue
		}
		var intent model.ImageUploadIntent
		err := model.DB.WithContext(ctx).Where("intent_key = ?", input.IntentKey).Take(&intent).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := model.DB.WithContext(ctx).Model(&model.ImageInputObject{}).Where("id = ? AND lease_expires_at <= ?", input.Id, now).Update("status", "deleted").Error; err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if intent.LeaseExpiresAt > now || (intent.FirstDeleteAt > 0 && now-intent.FirstDeleteAt < 600) {
			continue
		}
		lease := common.GetUUID()
		intent, err = model.ClaimImageInputDeletion(ctx, input, lease, cfg.WorkerLease)
		if errors.Is(err, model.ErrImageConflict) || errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		var profile model.ImageStorageProfile
		if err := model.DB.WithContext(ctx).Where("profile_id = ?", intent.ProfileId).Take(&profile).Error; err != nil {
			return err
		}
		store, err := OpenImageStorage(ctx, profile)
		if err != nil {
			return err
		}
		if err := store.Delete(ctx, intent.ObjectKey); err != nil {
			continue
		}
		if intent.FirstDeleteAt == 0 {
			if err := model.DB.WithContext(ctx).Model(&model.ImageUploadIntent{}).Where("id = ? AND lease_token = ?", intent.Id, lease).Updates(map[string]any{"first_delete_at": now, "lease_token": "", "lease_expires_at": 0}).Error; err != nil {
				return err
			}
			continue
		}
		if err := model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			result := tx.Model(&model.ImageUploadIntent{}).Where("id = ? AND lease_token = ?", intent.Id, lease).Updates(map[string]any{"status": "deleted", "lease_token": "", "lease_expires_at": 0})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return model.ErrImageConflict
			}
			if err := tx.Model(&model.ImageInputObject{}).Where("id = ?", input.Id).Updates(map[string]any{"status": "deleted", "lease_token": "", "lease_expires_at": 0}).Error; err != nil {
				return err
			}
			return tx.Model(&model.ImageStorageObject{}).Where("object_id = ?", input.ObjectId).Update("status", "deleted").Error
		}); err != nil {
			return err
		}
	}
	// Attempt tickets contain no credentials and are not used after ten minutes.
	return model.DB.WithContext(ctx).Where("created_at < ?", now-86400).Delete(&model.ImageUploadAdmissionTicket{}).Error
}

func ImageCleanupFingerprint(scope string, filters ImageCleanupFilters) string {
	return ImageIdentityHash(scope, strconv.Itoa(filters.UserId))
}

func SignImageCleanupPreview(scope string, filters ImageCleanupFilters, actor int, now int64) (string, error) {
	data, err := common.Marshal(map[string]any{"fingerprint": ImageCleanupFingerprint(scope, filters), "expires_at": now + 600, "nonce": common.GetUUID()})
	if err != nil {
		return "", err
	}
	cipher, err := EncryptImagePayload(data, "cleanup-preview:"+strconv.Itoa(actor))
	return base64.RawURLEncoding.EncodeToString(cipher), err
}

func ValidateImageCleanupPreview(token, scope string, filters ImageCleanupFilters, actor int, now int64) bool {
	cipher, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return false
	}
	data, err := DecryptImagePayload(cipher, "cleanup-preview:"+strconv.Itoa(actor))
	if err != nil {
		return false
	}
	var preview struct {
		Fingerprint string `json:"fingerprint"`
		ExpiresAt   int64  `json:"expires_at"`
	}
	return common.Unmarshal(data, &preview) == nil && preview.Fingerprint == ImageCleanupFingerprint(scope, filters) && preview.ExpiresAt > now && preview.ExpiresAt <= now+600
}
