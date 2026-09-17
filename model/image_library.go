package model

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// CommitImageLibraryItem serializes each user's durable quota and locks the
// object shared by every publisher and cleaner before attaching a reference.
func CommitImageLibraryItem(ctx context.Context, item ImageLibraryItem, maxItems int, maxBytes int64, submissionId string) (ImageLibraryItem, bool, error) {
	reused := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).Where("id = ?", item.UserId).Take(&user).Error; err != nil {
			return err
		}
		var old ImageLibraryItem
		err := tx.Where("source_key = ?", item.SourceKey).Take(&old).Error
		if err == nil {
			if old.UserId != item.UserId || old.Fingerprint != item.Fingerprint || old.DeletedAt != 0 {
				return ErrImageConflict
			}
			item, reused = old, true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var object ImageStorageObject
		if err := lockForUpdate(tx).Where("object_id = ? AND status = ? AND class = ?", item.ObjectId, "active", "durable").Take(&object).Error; err != nil {
			return err
		}
		now := time.Now().Unix()
		var usage struct {
			Count int64
			Bytes int64
		}
		if err := tx.Model(&ImageLibraryItem{}).Select("COUNT(*) AS count, COALESCE(SUM(image_storage_objects.byte_size), 0) AS bytes").Joins("JOIN image_storage_objects ON image_storage_objects.object_id = image_library_items.object_id").Where("image_library_items.user_id = ? AND image_library_items.deleted_at = 0 AND (image_library_items.expires_at = 0 OR image_library_items.expires_at > ?)", item.UserId, now).Scan(&usage).Error; err != nil {
			return err
		}
		if usage.Count >= int64(maxItems) || object.ByteSize > maxBytes-usage.Bytes {
			return errors.New("image library quota exceeded")
		}
		var submission ImageSubmissionRequest
		if submissionId != "" {
			if err := lockForUpdate(tx).Where("submission_id = ? AND user_id = ?", submissionId, item.UserId).Take(&submission).Error; err != nil {
				return err
			}
			if submission.Status != "approved_pending_sync" || submission.ExpiresAt <= now || submission.Checksum != object.Checksum || submission.ByteSize != object.ByteSize || submission.ContentType != object.ContentType {
				return ErrImageConflict
			}
		}
		if err := tx.Create(&item).Error; err != nil {
			return err
		}
		if submissionId == "" {
			return nil
		}
		publication := ImagePublication{PublicationId: "imgpub_" + common.GetUUID(), AssetId: item.AssetId, UserId: item.UserId, Status: "published", Title: submission.TitleForPublication(), SharePrompt: submission.SharePrompt, Version: 1, CreatedAt: now, PublishedAt: now, ExpiresAt: item.ExpiresAt}
		if publication.SharePrompt {
			publication.PublicPrompt = item.Prompt
		}
		if err := tx.Create(&publication).Error; err != nil {
			return err
		}
		result := tx.Model(&ImageSubmissionRequest{}).Where("id = ? AND version = ? AND status = ?", submission.Id, submission.Version, "approved_pending_sync").Updates(map[string]any{"status": "synced", "asset_id": item.AssetId, "publication_id": publication.PublicationId, "version": submission.Version + 1})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrImageConflict
		}
		return tx.Create(&ImageModerationEvent{SubjectId: submission.SubmissionId, ActorId: item.UserId, Action: "sync", CreatedAt: now}).Error
	})
	return item, reused, err
}

func (submission ImageSubmissionRequest) TitleForPublication() string {
	var metadata struct {
		PublicTitle string `json:"public_title"`
		Title       string `json:"title"`
	}
	if common.UnmarshalJsonStr(submission.Metadata, &metadata) != nil {
		return ""
	}
	if metadata.PublicTitle != "" {
		return metadata.PublicTitle
	}
	return metadata.Title
}

func CreateImagePublication(ctx context.Context, userId int, assetId, title string, share bool) (ImagePublication, error) {
	var publication ImagePublication
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item ImageLibraryItem
		if err := lockForUpdate(tx).Where("asset_id = ? AND user_id = ? AND deleted_at = 0 AND (expires_at = 0 OR expires_at > ?)", assetId, userId, time.Now().Unix()).Take(&item).Error; err != nil {
			return err
		}
		var object ImageStorageObject
		if err := lockForUpdate(tx).Where("object_id = ? AND status = ? AND class = ?", item.ObjectId, "active", "durable").Take(&object).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&ImagePublication{}).Where("asset_id = ? AND status IN ?", assetId, []string{"pending_review", "published", "hidden"}).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrImageConflict
		}
		publication = ImagePublication{PublicationId: "imgpub_" + common.GetUUID(), AssetId: assetId, UserId: userId, Title: title, SharePrompt: share, Status: "pending_review", Version: 1, CreatedAt: time.Now().Unix(), ExpiresAt: item.ExpiresAt}
		if share {
			publication.PublicPrompt = item.Prompt
		}
		return tx.Create(&publication).Error
	})
	return publication, err
}

func ModerateImageSubject(ctx context.Context, actorId int, id, action, reason string, deferred bool, ownerId int) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var status string
		var version int64
		var query *gorm.DB
		if deferred {
			var item ImageSubmissionRequest
			q := lockForUpdate(tx).Where("submission_id = ?", id)
			if ownerId != 0 {
				q = q.Where("user_id = ?", ownerId)
			}
			if err := q.Take(&item).Error; err != nil {
				return err
			}
			if item.ExpiresAt <= time.Now().Unix() {
				return ErrImageConflict
			}
			status, version = item.Status, item.Version
			query = tx.Model(&ImageSubmissionRequest{}).Where("id = ? AND version = ?", item.Id, version)
		} else {
			var item ImagePublication
			q := lockForUpdate(tx).Where("publication_id = ?", id)
			if ownerId != 0 {
				q = q.Where("user_id = ?", ownerId)
			}
			if err := q.Take(&item).Error; err != nil {
				return err
			}
			status, version = item.Status, item.Version
			if action == "approve" || action == "restore" {
				var asset ImageLibraryItem
				if err := tx.Where("asset_id = ? AND deleted_at = 0 AND (expires_at = 0 OR expires_at > ?)", item.AssetId, time.Now().Unix()).Take(&asset).Error; err != nil {
					return err
				}
				var object ImageStorageObject
				if err := lockForUpdate(tx).Where("object_id = ? AND status = ? AND class = ?", asset.ObjectId, "active", "durable").Take(&object).Error; err != nil {
					return err
				}
			}
			query = tx.Model(&ImagePublication{}).Where("id = ? AND version = ?", item.Id, version)
		}
		target := ""
		switch action {
		case "approve":
			if status == "pending_review" {
				target = "published"
				if deferred {
					target = "approved_pending_sync"
				}
			}
		case "reject":
			if status == "pending_review" {
				target = "rejected"
			}
		case "hide":
			if !deferred && status == "published" {
				target = "hidden"
			}
		case "restore":
			if !deferred && status == "hidden" {
				target = "published"
			}
		case "withdraw":
			if status == "pending_review" || status == "approved_pending_sync" || (!deferred && (status == "published" || status == "hidden")) {
				target = "withdrawn"
			}
		}
		if target == "" {
			return ErrImageConflict
		}
		updates := map[string]any{"status": target, "reason": reason, "version": version + 1}
		if !deferred && target == "published" {
			updates["published_at"] = time.Now().Unix()
		}
		if err := query.Updates(updates).Error; err != nil {
			return err
		}
		return tx.Create(&ImageModerationEvent{SubjectId: id, ActorId: actorId, Action: action, Reason: reason, CreatedAt: time.Now().Unix()}).Error
	})
}

func RecordImageLibraryImport(ctx context.Context, userId, limit int) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).Where("id = ? AND status = ?", userId, common.UserStatusEnabled).Take(&user).Error; err != nil {
			return err
		}
		now := time.Now().Unix()
		subject := "image-library-import:" + strconv.Itoa(userId)
		var count int64
		if err := tx.Model(&ImageModerationEvent{}).Where("subject_id = ? AND action = ? AND created_at > ?", subject, "import_attempt", now-60).Count(&count).Error; err != nil {
			return err
		}
		if count >= int64(limit) {
			return ErrImageUploadRateLimited
		}
		return tx.Create(&ImageModerationEvent{SubjectId: subject, ActorId: userId, Action: "import_attempt", CreatedAt: now}).Error
	})
}

// Unconfirmed writes share their intent lock with upload confirmation. A
// live task, SC input or confirmed object prevents orphan deletion.
func ClaimUnconfirmedImageIntent(ctx context.Context, id int, lease string, seconds int, now int64) (ImageUploadIntent, error) {
	var intent ImageUploadIntent
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("id = ? AND status IN ? AND created_at <= ? AND lease_expires_at <= ?", id, []string{"pending", "uploading", "deleting"}, now-86400, now).Take(&intent).Error; err != nil {
			return err
		}
		if intent.FirstDeleteAt > 0 && now-intent.FirstDeleteAt < 600 {
			return ErrImageConflict
		}
		var count int64
		if err := tx.Model(&ImageInputObject{}).Where("intent_key = ?", intent.IntentKey).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrImageConflict
		}
		if err := tx.Model(&ImageStorageObject{}).Where("profile_id = ? AND object_key = ?", intent.ProfileId, intent.ObjectKey).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrImageConflict
		}
		if intent.TaskId != "" {
			var task AsyncImageTask
			err := lockForUpdate(tx).Where("task_id = ?", intent.TaskId).Take(&task).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err == nil && (!task.Terminal() || task.ExpiresAt == 0 || task.ExpiresAt > now) {
				return ErrImageConflict
			}
		}
		result := tx.Model(&ImageUploadIntent{}).Where("id = ? AND status = ? AND lease_expires_at <= ?", intent.Id, intent.Status, now).Updates(map[string]any{"status": "deleting", "lease_token": lease, "lease_expires_at": now + int64(seconds)})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrImageConflict
		}
		return nil
	})
	return intent, err
}

// ClaimImageObjectDeletion holds the same object lock used when registering a
// task manifest, library asset or publication. No file operation occurs here.
func ClaimImageObjectDeletion(ctx context.Context, objectId, lease string, now int64, leaseSeconds int) (ImageStorageObject, error) {
	var object ImageStorageObject
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("object_id = ?", objectId).Take(&object).Error; err != nil {
			return err
		}
		if object.Status == "deleted" {
			return gorm.ErrRecordNotFound
		}
		if object.Status == "deleting" && object.DeleteLeaseExpiresAt > now {
			return ErrImageConflict
		}
		var count int64
		if err := tx.Model(&ImageLibraryItem{}).Where("object_id = ? AND deleted_at = 0 AND (expires_at = 0 OR expires_at > ?)", objectId, now).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrImageConflict
		}
		if err := tx.Model(&ImageLibraryItem{}).Joins("JOIN image_publications ON image_publications.asset_id = image_library_items.asset_id").Where("image_library_items.object_id = ? AND image_publications.status IN ? AND (image_publications.expires_at = 0 OR image_publications.expires_at > ?)", objectId, []string{"pending_review", "published", "hidden"}, now).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrImageConflict
		}
		if err := tx.Model(&AsyncImageResult{}).Joins("JOIN async_image_tasks ON async_image_tasks.task_id = async_image_results.task_id").Where("async_image_results.object_id = ? AND (async_image_tasks.expires_at > ? OR async_image_tasks.status NOT IN ?)", objectId, now, []string{ImageTaskSucceeded, ImageTaskFailed, ImageTaskExpired, ImageTaskExecutionUnknown}).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrImageConflict
		}
		if err := tx.Model(&ImageInputObject{}).Where("object_id = ? AND (expires_at > ? OR lease_expires_at > ?) AND status IN ?", objectId, now, now, []string{"active", "reserved", "uploading"}).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrImageConflict
		}
		if err := tx.Model(&ImageInputTaskReference{}).Joins("JOIN image_input_objects ON image_input_objects.input_id = image_input_task_references.input_id").Joins("JOIN async_image_tasks ON async_image_tasks.task_id = image_input_task_references.task_id").Where("image_input_objects.object_id = ? AND async_image_tasks.status NOT IN ?", objectId, []string{ImageTaskSucceeded, ImageTaskFailed, ImageTaskExpired, ImageTaskExecutionUnknown}).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrImageConflict
		}
		result := tx.Model(&ImageStorageObject{}).Where("id = ? AND status = ? AND delete_lease = ?", object.Id, object.Status, object.DeleteLease).Updates(map[string]any{"status": "deleting", "delete_lease": lease, "delete_lease_expires_at": now + int64(leaseSeconds)})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrImageConflict
		}
		object.Status, object.DeleteLease = "deleting", lease
		return nil
	})
	return object, err
}
