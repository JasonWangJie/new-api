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
	UserId    int    `json:"user_id"`
	ChannelID string `json:"channel_id,omitempty"`
	OlderThan int    `json:"older_than,omitempty"`
	AgeUnit   string `json:"age_unit,omitempty"`
	// Frozen server-side fields are never accepted from a management request.
	CutoffAt   int64    `json:"cutoff_at,omitempty"`
	ProfileIDs []string `json:"profile_ids,omitempty"`
	// Retention behavior comes from the persisted job version, never client JSON.
	IgnoreRetention bool `json:"-"`
}

func imageCleanupIgnoresRetention(job model.ImageCleanupJob, filters ImageCleanupFilters) bool {
	return (job.Source == "manual_age" || (job.Source == "scheduled" && job.PolicyID != "")) &&
		filters.ChannelID != "" && filters.CutoffAt > 0 && len(filters.ProfileIDs) > 0
}

func (filters ImageCleanupFilters) AgeSeconds() (int64, error) {
	if filters.OlderThan == 0 && filters.AgeUnit == "" {
		return 0, nil
	}
	if filters.OlderThan < 1 || filters.OlderThan > 876000 {
		return 0, errors.New("invalid image cleanup age")
	}
	switch filters.AgeUnit {
	case "hours":
		return int64(filters.OlderThan) * 3600, nil
	case "days":
		if filters.OlderThan <= 36500 {
			return int64(filters.OlderThan) * 86400, nil
		}
	}
	return 0, errors.New("invalid image cleanup age unit")
}

func imageCleanupScope(ctx context.Context, scope string, filters ImageCleanupFilters) (*gorm.DB, error) {
	if scope != "expired" && scope != "deleted" && scope != "user" && scope != "async_results" && scope != "all" {
		return nil, errors.New("invalid image cleanup scope")
	}
	if filters.UserId < 0 || (scope == "user" && filters.UserId < 1) {
		return nil, errors.New("a user ID is required for user cleanup")
	}
	now := time.Now().Unix()
	query := model.DB.WithContext(ctx).Model(&model.ImageStorageObject{}).Where("status IN ?", []string{"active", "deleting"})
	age, err := filters.AgeSeconds()
	if err != nil {
		return nil, err
	}
	if age > 0 || filters.CutoffAt != 0 {
		cutoff := filters.CutoffAt
		if cutoff == 0 {
			cutoff = now - age
		}
		query = query.Where("created_at < ?", cutoff)
	}
	if filters.ChannelID != "" {
		ids := filters.ProfileIDs
		if len(ids) == 0 {
			profiles, err := model.ImageStorageChannelProfiles(ctx, filters.ChannelID)
			if err != nil {
				return nil, err
			}
			for _, profile := range profiles {
				ids = append(ids, profile.ProfileId)
			}
		}
		if len(ids) == 0 {
			return nil, errors.New("image storage channel has no revisions")
		}
		query = query.Where("profile_id IN ?", ids)
	}
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
	return query, nil
}

func ImageCleanupCandidates(ctx context.Context, scope string, filters ImageCleanupFilters) (*gorm.DB, error) {
	query, err := imageCleanupScope(ctx, scope, filters)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	// Live references are excluded at preview time and checked again under an
	// object lock immediately before physical deletion.
	if !filters.IgnoreRetention {
		query = query.Where("expires_at = 0 OR expires_at <= ?", now)
	}
	protectedTasks := model.DB.WithContext(ctx).Model(&model.AsyncImageTask{}).Select("task_id")
	terminal := []string{model.ImageTaskSucceeded, model.ImageTaskFailed, model.ImageTaskExpired, model.ImageTaskExecutionUnknown}
	if filters.IgnoreRetention {
		archives := model.DB.WithContext(ctx).Model(&model.ImageOutbox{}).Select("aggregate_id").Where("kind = ? AND status = ?", "library_archive", "pending")
		protectedTasks = protectedTasks.Where("status NOT IN ? OR task_id IN (?)", terminal, archives)
	} else {
		protectedTasks = protectedTasks.Where("expires_at = 0 OR expires_at > ? OR status NOT IN ?", now, terminal)
	}
	query = query.Where("NOT EXISTS (?)", model.DB.WithContext(ctx).Model(&model.ImageLibraryItem{}).Select("1").Where("image_library_items.object_id = image_storage_objects.object_id AND deleted_at = 0 AND (expires_at = 0 OR expires_at > ?)", now))
	query = query.Where("NOT EXISTS (?)", model.DB.WithContext(ctx).Model(&model.ImageLibraryItem{}).Select("1").Joins("JOIN image_publications ON image_publications.asset_id = image_library_items.asset_id").Where("image_library_items.object_id = image_storage_objects.object_id AND image_publications.status IN ? AND (image_publications.expires_at = 0 OR image_publications.expires_at > ?)", []string{"pending_review", "published", "hidden"}, now))
	query = query.Where("NOT EXISTS (?)", model.DB.WithContext(ctx).Model(&model.AsyncImageResult{}).Select("1").Where("async_image_results.object_id = image_storage_objects.object_id AND task_id IN (?)", protectedTasks))
	query = query.Where("NOT EXISTS (?)", model.DB.WithContext(ctx).Model(&model.ImageInputObject{}).Select("1").Where("image_input_objects.object_id = image_storage_objects.object_id AND status IN ? AND (expires_at > ? OR lease_expires_at > ?)", []string{"active", "reserved", "uploading"}, now, now))
	query = query.Where("NOT EXISTS (?)", model.DB.WithContext(ctx).Model(&model.ImageInputObject{}).Select("1").Joins("JOIN image_input_task_references ON image_input_task_references.input_id = image_input_objects.input_id").Joins("JOIN async_image_tasks ON async_image_tasks.task_id = image_input_task_references.task_id").Where("image_input_objects.object_id = image_storage_objects.object_id AND async_image_tasks.status NOT IN ?", []string{model.ImageTaskSucceeded, model.ImageTaskFailed, model.ImageTaskExpired, model.ImageTaskExecutionUnknown}))
	query = query.Where("NOT EXISTS (?)", model.DB.WithContext(ctx).Model(&model.ImageUploadIntent{}).Select("1").Where("image_upload_intents.profile_id = image_storage_objects.profile_id AND image_upload_intents.object_key = image_storage_objects.object_key AND (image_upload_intents.status IN ? OR image_upload_intents.lease_expires_at > ?)", []string{"pending", "uploading"}, now))
	query = query.Where("NOT EXISTS (?)", model.DB.WithContext(ctx).Model(&model.ImageUploadIntent{}).Select("1").Where("image_upload_intents.profile_id = image_storage_objects.profile_id AND image_upload_intents.object_key = image_storage_objects.object_key AND task_id IN (?)", protectedTasks))
	return query, nil
}

// ImageCleanupProtectionReason classifies only the reason for retaining an
// object. The candidate query remains the source of truth for live references.
func ImageCleanupProtectionReason(ctx context.Context, object model.ImageStorageObject) (string, error) {
	return imageCleanupProtectionReason(ctx, object, ImageCleanupFilters{})
}

func imageCleanupProtectionReason(ctx context.Context, object model.ImageStorageObject, filters ImageCleanupFilters) (string, error) {
	now := time.Now().Unix()
	if !filters.IgnoreRetention && object.ExpiresAt > now {
		return "retention", nil
	}
	if !filters.IgnoreRetention {
		var task model.AsyncImageTask
		retainedTask := model.DB.WithContext(ctx).Select("id").Where("status IN ? AND (expires_at = 0 OR expires_at > ?)", []string{model.ImageTaskSucceeded, model.ImageTaskFailed, model.ImageTaskExpired, model.ImageTaskExecutionUnknown}, now).
			Where("task_id IN (?) OR task_id IN (?)", model.DB.WithContext(ctx).Model(&model.AsyncImageResult{}).Select("task_id").Where("object_id = ?", object.ObjectId), model.DB.WithContext(ctx).Model(&model.ImageUploadIntent{}).Select("task_id").Where("profile_id = ? AND object_key = ?", object.ProfileId, object.ObjectKey)).Limit(1).Find(&task)
		if retainedTask.Error != nil {
			return "", retainedTask.Error
		}
		if retainedTask.RowsAffected > 0 {
			return "retention", nil
		}
	}
	query, err := ImageCleanupCandidates(ctx, "all", filters)
	if err != nil {
		return "", err
	}
	var count int64
	if err := query.Where("object_id = ?", object.ObjectId).Count(&count).Error; err != nil {
		return "", err
	}
	if count == 0 {
		return "reference", nil
	}
	return "", nil
}

func MaintainImageStorage(ctx context.Context, cfg ImageRuntimeConfig) error {
	var failures []error
	failures = append(failures, ReapImageInputIntents(ctx, cfg))
	failures = append(failures, ReapUnconfirmedImageIntents(ctx, cfg))
	// Accounting and idempotency tombstones remain durable. Expired task
	// content is erased without making an old request eligible for execution.
	failures = append(failures, model.EraseExpiredAsyncImageTaskContent(ctx))
	var events []int
	err := model.DB.WithContext(ctx).Model(&model.ImageModerationEvent{}).Where("action = ? AND created_at < ?", "import_attempt", time.Now().Unix()-86400).Order("id").Limit(200).Pluck("id", &events).Error
	failures = append(failures, err)
	if err == nil && len(events) > 0 {
		failures = append(failures, model.DB.WithContext(ctx).Where("id IN ?", events).Delete(&model.ImageModerationEvent{}).Error)
	}
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
		if err := deleteReapedImageIntent(ctx, intent, ""); err != nil {
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
		if err := deleteReapedImageIntent(ctx, intent, input.ObjectId); err != nil {
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
	var tickets []int
	if err := model.DB.WithContext(ctx).Model(&model.ImageUploadAdmissionTicket{}).Where("created_at < ?", now-86400).Order("id").Limit(200).Pluck("id", &tickets).Error; err != nil {
		return err
	}
	if len(tickets) == 0 {
		return nil
	}
	return model.DB.WithContext(ctx).Where("id IN ?", tickets).Delete(&model.ImageUploadAdmissionTicket{}).Error
}

func ImageCleanupFingerprint(scope string, filters ImageCleanupFilters) string {
	// Internal snapshot fields are bound by the persisted preview, while the
	// fingerprint binds precisely the filters supplied by the administrator.
	return ImageIdentityHash(scope, strconv.Itoa(filters.UserId), filters.ChannelID, strconv.Itoa(filters.OlderThan), filters.AgeUnit)
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
