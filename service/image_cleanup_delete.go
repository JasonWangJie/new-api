package service

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errImageSharedLocation = errors.New("image physical location is shared or cannot be proven exclusive")

type imageCleanupProfilesKey struct{}

func withImageCleanupProfiles(ctx context.Context) (context.Context, error) {
	if ctx.Value(imageCleanupProfilesKey{}) != nil {
		return ctx, nil
	}
	var profiles []model.ImageStorageProfile
	if err := model.DB.WithContext(ctx).Limit(501).Find(&profiles).Error; err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, imageCleanupProfilesKey{}, profiles), nil
}

func deleteReapedImageIntent(parent context.Context, intent model.ImageUploadIntent, objectID string) error {
	cfg, err := GetImageCleanupConfig(parent)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	object := model.ImageStorageObject{ObjectId: objectID, ProfileId: intent.ProfileId, ObjectKey: intent.ObjectKey}
	if err := imagePhysicalLocationExclusive(ctx, object, intent.IntentKey); err != nil {
		return err
	}
	store, err := GetImageObjectStorage(ctx, object)
	if err != nil {
		return err
	}
	if err := store.PhysicalDeletionSupported(ctx); err != nil {
		return err
	}
	return store.Delete(ctx, intent.ObjectKey)
}

// EnsureImageObjectExclusive protects old configuration revisions which can
// name the same physical object. New namespaced writes cannot introduce aliases
// into these legacy keys. Ambiguous historical locations are never deleted.
func EnsureImageObjectExclusive(ctx context.Context, object model.ImageStorageObject) error {
	return imagePhysicalLocationExclusive(ctx, object, "")
}

// Reconcile stopped work using metadata only. Cancellation cannot undo a
// request already accepted by storage. An object is made readable again only
// after a ten-minute uncertainty window and a confirmed presence check.
func reconcileStoppedImageCleanup(ctx context.Context, cfg ImageCleanupConfig, budget *imageCleanupBudget) error {
	var entries []model.ImageCleanupRetry
	query := model.DB.WithContext(ctx).Model(&model.ImageCleanupRetry{}).Select("image_cleanup_retries.*").Joins("JOIN image_cleanup_jobs ON image_cleanup_jobs.job_id = image_cleanup_retries.job_id").Joins("JOIN image_storage_objects ON image_storage_objects.object_id = image_cleanup_retries.object_id").Where("image_cleanup_jobs.status IN ? AND image_cleanup_retries.outcome = ? AND image_cleanup_retries.next_attempt_at <= ? AND image_storage_objects.status = ? AND image_storage_objects.delete_lease_expires_at <= ?", []string{"paused", "cancelled", "completed_with_errors"}, "retry", time.Now().Unix(), "deleting", time.Now().Unix()-600)
	if err := query.Order("image_cleanup_retries.id").Limit(5).Find(&entries).Error; err != nil {
		return err
	}
	for _, entry := range entries {
		if err := model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("job_id = ? AND status IN ? AND lease_expires_at <= ?", entry.JobId, []string{"paused", "cancelled", "completed_with_errors"}, time.Now().Unix()).Updates(map[string]any{"lease_token": "", "lease_expires_at": 0}).Error; err != nil {
			return err
		}
		var job model.ImageCleanupJob
		if err := model.DB.WithContext(ctx).Where("job_id = ? AND status IN ? AND lease_token = ?", entry.JobId, []string{"paused", "cancelled", "completed_with_errors"}, "").Take(&job).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		} else if err != nil {
			return err
		}
		var object model.ImageStorageObject
		if err := model.DB.WithContext(ctx).Where("object_id = ?", entry.ObjectId).Take(&object).Error; err != nil {
			return err
		}
		if object.Status != "deleting" || object.DeleteLeaseExpiresAt+600 > time.Now().Unix() {
			continue
		}
		store, err := GetImageObjectStorage(ctx, object)
		if err != nil {
			continue
		}
		operation, cancel := context.WithTimeout(ctx, time.Duration(cfg.RequestTimeoutSeconds)*time.Second)
		missing := false
		if store.Profile.Backend == "local" {
			err = budget.Wait(operation)
			if err == nil {
				var filename string
				filename, err = store.LocalPath(object.ObjectKey, false)
				if err == nil {
					_, err = os.Stat(filename)
				}
				missing = errors.Is(err, os.ErrNotExist)
			}
		} else {
			_, err = store.Client.HeadObject(operation, &s3.HeadObjectInput{Bucket: aws.String(store.Profile.Bucket), Key: aws.String(store.Key(object.ObjectKey))})
			var apiError smithy.APIError
			missing = errors.As(err, &apiError) && (apiError.ErrorCode() == "NotFound" || apiError.ErrorCode() == "NoSuchKey")
		}
		cancel()
		if err != nil && !missing {
			_ = model.WithImageCleanupJobLease(ctx, job.Id, "", func(tx *gorm.DB) error {
				return tx.Model(&model.ImageCleanupRetry{}).Where("id = ? AND outcome = ?", entry.Id, "retry").Update("next_attempt_at", time.Now().Unix()+300).Error
			})
			continue
		}
		if missing {
			if err := finishImageCleanupObject(ctx, job, entry, object, object.DeleteLease, "deleted"); err != nil {
				return err
			}
			continue
		}
		if err := model.WithImageCleanupJobLease(ctx, job.Id, "", func(tx *gorm.DB) error {
			update := tx.Model(&model.ImageStorageObject{}).Where("id = ? AND status = ? AND delete_lease = ?", object.Id, "deleting", object.DeleteLease).Updates(map[string]any{"status": "active", "delete_lease": "", "delete_lease_expires_at": 0})
			if update.Error != nil || update.RowsAffected != 1 {
				return update.Error
			}
			// Paused jobs retain their retry, so resume can attempt the deletion.
			if job.Status == "paused" {
				return nil
			}
			return tx.Model(&model.ImageCleanupRetry{}).Where("id = ?", entry.Id).Update("outcome", "skipped").Error
		}); err != nil {
			return err
		}
	}
	return nil
}

func imagePhysicalLocationExclusive(ctx context.Context, object model.ImageStorageObject, ignoreIntent string) error {
	ctx, err := withImageCleanupProfiles(ctx)
	if err != nil {
		return err
	}
	profiles := ctx.Value(imageCleanupProfilesKey{}).([]model.ImageStorageProfile)
	var profile model.ImageStorageProfile
	for _, candidate := range profiles {
		if candidate.ProfileId == object.ProfileId {
			profile = candidate
			break
		}
	}
	if profile.ProfileId == "" {
		if err := model.DB.WithContext(ctx).Where("profile_id = ?", object.ProfileId).Take(&profile).Error; err != nil {
			return err
		}
	}
	physical := path.Join(profile.Prefix, profile.Namespace, object.ObjectKey)
	if profile.Backend == "local" {
		root, err := filepath.EvalSymlinks(profile.Root)
		if err != nil {
			return errImageSharedLocation
		}
		physical = filepath.Join(root, filepath.FromSlash(path.Join(profile.Namespace, object.ObjectKey)))
	}
	paged := len(profiles) > 500
	cursor := 0
	for {
		peers := profiles
		if paged {
			query := model.DB.WithContext(ctx).Where("backend = ? AND id > ?", profile.Backend, cursor)
			if profile.Backend != "local" {
				query = query.Where("bucket = ?", profile.Bucket)
			}
			peers = nil
			if err := query.Order("id").Limit(100).Find(&peers).Error; err != nil {
				return err
			}
			if len(peers) == 0 {
				break
			}
			cursor = peers[len(peers)-1].Id
		}
		for _, peer := range peers {
			if err := ctx.Err(); err != nil {
				return err
			}
			if peer.Backend != profile.Backend {
				continue
			}
			var key string
			if profile.Backend == "local" {
				root, err := filepath.EvalSymlinks(peer.Root)
				if err != nil {
					return errImageSharedLocation
				}
				root = filepath.Join(root, peer.Namespace)
				rel, err := filepath.Rel(root, physical)
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
					continue
				}
				key = filepath.ToSlash(rel)
				if peer.ProfileId != profile.ProfileId && profile.Namespace == "" && peer.Namespace == "" {
					return errImageSharedLocation
				}
			} else {
				// Bucket equality is deliberately conservative across endpoint aliases.
				if peer.Bucket != profile.Bucket {
					continue
				}
				prefix := path.Join(peer.Prefix, peer.Namespace)
				if prefix == "." || prefix == "" {
					key = physical
				} else {
					var found bool
					key, found = strings.CutPrefix(physical, strings.TrimSuffix(prefix, "/")+"/")
					if !found {
						continue
					}
				}
				if peer.ProfileId != profile.ProfileId && profile.Namespace == "" && peer.Namespace == "" {
					return errImageSharedLocation
				}
			}
			var count int64
			if err := model.DB.WithContext(ctx).Model(&model.ImageStorageObject{}).Where("profile_id = ? AND object_key = ? AND object_id <> ?", peer.ProfileId, key, object.ObjectId).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return errImageSharedLocation
			}
			query := model.DB.WithContext(ctx).Model(&model.ImageUploadIntent{}).Where("profile_id = ? AND object_key = ?", peer.ProfileId, key)
			if ignoreIntent != "" {
				query = query.Where("intent_key <> ?", ignoreIntent)
			}
			if peer.ProfileId == object.ProfileId && key == object.ObjectKey {
				query = query.Where("status NOT IN ?", []string{"confirmed", "deleted"})
			}
			if err := query.Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return errImageSharedLocation
			}
		}
		if !paged || len(peers) < 100 {
			break
		}
	}
	return nil
}

func executeImageCleanupObject(ctx context.Context, job model.ImageCleanupJob, lease string, candidate model.ImageStorageObject, budget *imageCleanupBudget, pending *model.ImageCleanupRetry) error {
	imageCleanupActiveWorkers.Add(1)
	defer imageCleanupActiveWorkers.Add(-1)
	var current model.ImageCleanupJob
	if err := model.DB.WithContext(ctx).Where("id = ? AND lease_token = ?", job.Id, lease).Take(&current).Error; err != nil {
		return err
	}
	if current.Control != "" {
		return context.Canceled
	}
	cfg, err := GetImageCleanupConfig(ctx)
	if err != nil {
		return err
	}
	if reason := imageCleanupLoad.Check(cfg); reason != "" {
		_ = model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ?", job.Id, lease).Updates(map[string]any{"last_error": reason, "next_attempt_at": time.Now().Unix() + 15}).Error
		return context.Canceled
	}
	entry := model.ImageCleanupRetry{JobId: job.JobId, ObjectId: candidate.ObjectId, Outcome: "retry"}
	if pending != nil {
		entry = *pending
	} else {
		if err := model.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&entry).Error; err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Where("job_id = ? AND object_id = ?", job.JobId, candidate.ObjectId).Take(&entry).Error; err != nil {
			return err
		}
		if entry.Outcome == "deleted" || entry.Outcome == "skipped" || entry.Attempts >= 3 || entry.NextAttemptAt > time.Now().Unix() {
			return nil
		}
	}
	var object model.ImageStorageObject
	var filters ImageCleanupFilters
	if err := common.UnmarshalJsonStr(job.Filters, &filters); err != nil {
		return err
	}
	filters.IgnoreRetention = job.Source == "scheduled" && job.PolicyID != "" && filters.ChannelID != "" && filters.CutoffAt > 0 && len(filters.ProfileIDs) > 0
	eligible, err := ImageCleanupCandidates(ctx, job.Scope, filters)
	if err != nil {
		return err
	}
	var count int64
	if err := eligible.Where("id = ?", candidate.Id).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		reason, err := imageCleanupProtectionReason(ctx, candidate, filters)
		if err != nil {
			return err
		}
		if reason == "" {
			reason = "reference"
		}
		entry.LastError = reason
		return finishImageCleanupObject(ctx, job, entry, candidate, lease, "skipped")
	}
	var store *ImageStorage
	configurationError := false
	operation, cancelOperation := context.WithTimeout(ctx, time.Duration(cfg.RequestTimeoutSeconds)*time.Second)
	defer cancelOperation()
	err = EnsureImageObjectExclusive(operation, candidate)
	configurationError = errors.Is(err, gorm.ErrRecordNotFound)
	operation = WithImageStorageRequestBudget(operation, func(requestContext context.Context) error {
		if err := budget.Wait(requestContext); err != nil {
			return err
		}
		var owner model.ImageCleanupJob
		if err := model.DB.WithContext(requestContext).Select("control", "lease_token", "lease_expires_at").Where("id = ?", job.Id).Take(&owner).Error; err != nil {
			return err
		}
		if owner.Control != "" || owner.LeaseToken != lease || owner.LeaseExpiresAt <= time.Now().Unix() {
			return context.Canceled
		}
		return nil
	})
	if err == nil {
		var profile model.ImageStorageProfile
		err = model.DB.WithContext(operation).Where("profile_id = ?", candidate.ProfileId).Take(&profile).Error
		configurationError = errors.Is(err, gorm.ErrRecordNotFound)
		if err == nil {
			store, err = OpenImageStorage(operation, profile)
			configurationError = err != nil && operation.Err() == nil
		}
	}
	if err == nil {
		err = store.PhysicalDeletionSupported(operation)
	}
	if err == nil {
		if filters.IgnoreRetention {
			object, err = model.ClaimImageObjectPolicyDeletion(operation, candidate.ObjectId, lease, time.Now().Unix(), cfg.RequestTimeoutSeconds+30, filters.CutoffAt, filters.ProfileIDs)
		} else {
			object, err = model.ClaimImageObjectDeletion(operation, candidate.ObjectId, lease, time.Now().Unix(), cfg.RequestTimeoutSeconds+30)
		}
	}
	if !configurationError && (errors.Is(err, errImageSharedLocation) || errors.Is(err, model.ErrImageConflict) || errors.Is(err, gorm.ErrRecordNotFound)) {
		if candidate.Status == "deleting" && candidate.DeleteLeaseExpiresAt > time.Now().Unix() {
			return recordImageCleanupRetry(ctx, job, entry, errors.New("waiting for an existing object lease"))
		}
		entry.LastError = "location"
		if !errors.Is(err, errImageSharedLocation) {
			reason, checkErr := imageCleanupProtectionReason(ctx, candidate, filters)
			if checkErr != nil {
				return checkErr
			}
			if reason == "" {
				reason = "reference"
			}
			entry.LastError = reason
		}
		return finishImageCleanupObject(ctx, job, entry, candidate, lease, "skipped")
	}
	if err == nil {
		err = store.Delete(operation, object.ObjectKey)
	}
	finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer stop()
	if err != nil {
		var apiError smithy.APIError
		if configurationError || errors.Is(err, ErrImageStorageVersioned) || errors.As(err, &apiError) && (apiError.ErrorCode() == "AccessDenied" || apiError.ErrorCode() == "InvalidAccessKeyId" || apiError.ErrorCode() == "SignatureDoesNotMatch") {
			_ = model.DB.WithContext(finish).Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ? AND control = ?", job.Id, lease, "").Updates(map[string]any{"status": "paused", "control": "pause", "last_error": "Storage deletion permission is unavailable; verify channel credentials before resuming"}).Error
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = errors.New("Storage delete request timed out")
		}
		return recordImageCleanupRetry(finish, job, entry, err)
	}
	return finishImageCleanupObject(finish, job, entry, object, lease, "deleted")
}

func finishImageCleanupObject(ctx context.Context, job model.ImageCleanupJob, entry model.ImageCleanupRetry, object model.ImageStorageObject, lease, outcome string) error {
	return model.WithImageCleanupJobLease(ctx, job.Id, job.LeaseToken, func(tx *gorm.DB) error {
		reason := ""
		if outcome == "skipped" {
			reason = entry.LastError
			if reason != "location" && reason != "retention" {
				reason = "reference"
			}
		}
		change := tx.Model(&model.ImageCleanupRetry{}).Where("id = ? AND outcome NOT IN ?", entry.Id, []string{"deleted", "skipped"}).Updates(map[string]any{"outcome": outcome, "last_error": reason, "next_attempt_at": 0})
		if change.Error != nil || change.RowsAffected != 1 {
			return change.Error
		}
		updates := map[string]any{"processed": gorm.Expr("processed + 1"), "scanned": gorm.Expr("scanned + 1")}
		if outcome == "deleted" {
			deleted := tx.Model(&model.ImageStorageObject{}).Where("id = ? AND status = ? AND delete_lease = ?", object.Id, "deleting", lease).Updates(map[string]any{"status": "deleted", "delete_lease": "", "delete_lease_expires_at": 0})
			if deleted.Error != nil {
				return deleted.Error
			}
			if deleted.RowsAffected != 1 {
				return model.ErrImageConflict
			}
			updates["deleted"], updates["deleted_bytes"] = gorm.Expr("deleted + 1"), gorm.Expr("deleted_bytes + ?", object.ByteSize)
		} else {
			updates["skipped"] = gorm.Expr("skipped + 1")
			updates["protected"] = gorm.Expr("protected + 1")
			switch reason {
			case "location":
				updates["location_skipped"] = gorm.Expr("location_skipped + 1")
			case "retention":
				updates["retention_skipped"] = gorm.Expr("retention_skipped + 1")
			default:
				updates["reference_skipped"] = gorm.Expr("reference_skipped + 1")
			}
		}
		update := tx.Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ?", job.Id, job.LeaseToken).Updates(updates)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return model.ErrImageConflict
		}
		return nil
	})
}

func recordImageCleanupRetry(ctx context.Context, job model.ImageCleanupJob, entry model.ImageCleanupRetry, cause error) error {
	message := "Image deletion could not be confirmed; check storage availability and deletion permissions"
	if errors.Is(cause, ErrImageStorageVersioned) {
		message = "Physical cleanup is unavailable for versioned storage"
	}
	return model.WithImageCleanupJobLease(ctx, job.Id, job.LeaseToken, func(tx *gorm.DB) error {
		attempts := entry.Attempts + 1
		if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
			attempts = entry.Attempts
		}
		delay := min(900, 30*(1<<min(attempts, 3)))
		delay += rand.IntN(delay/5 + 1)
		change := tx.Model(&model.ImageCleanupRetry{}).Where("id = ? AND attempts = ? AND outcome NOT IN ?", entry.Id, entry.Attempts, []string{"deleted", "skipped"}).Updates(map[string]any{"outcome": "retry", "attempts": attempts, "next_attempt_at": time.Now().Unix() + int64(delay), "last_error": message})
		if change.Error != nil || change.RowsAffected != 1 {
			return change.Error
		}
		updates := map[string]any{"last_error": message}
		if entry.Attempts < 3 && attempts >= 3 {
			updates["errors"] = gorm.Expr("errors + 1")
		}
		return tx.Model(&model.ImageCleanupJob{}).Where("id = ?", job.Id).Updates(updates).Error
	})
}
