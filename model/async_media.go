package model

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// AsyncMediaJob owns submission and storage leases. Upstream task accounting
// remains in Task; retrying storage never enters the submission path.
type AsyncMediaJob struct {
	ID              int    `json:"-"`
	TaskId          string `json:"task_id" gorm:"size:64;uniqueIndex"`
	IdentityHash    string `json:"-" gorm:"size:64;uniqueIndex"`
	RequestHash     string `json:"-" gorm:"size:64"`
	UserId          int    `json:"user_id" gorm:"index"`
	TokenId         int    `json:"api_key_id" gorm:"index"`
	ChannelId       int    `json:"-"`
	Provider        string `json:"provider" gorm:"size:32"`
	Model           string `json:"model" gorm:"size:255"`
	Group           string `json:"group" gorm:"size:64"`
	RequestCipher   []byte `json:"-"`
	ChannelCipher   []byte `json:"-"`
	OutputRootPath  string `json:"-" gorm:"type:text"`
	OutputObjectKey string `json:"-" gorm:"size:128"`
	ArtifactKeys    string `json:"-" gorm:"type:text"`
	Status          string `json:"status" gorm:"size:32;index"`
	StorageStatus   string `json:"storage_status" gorm:"size:32"`
	BillingStatus   string `json:"billing_status" gorm:"size:32"`
	Quota           int    `json:"quota"`
	DispatchedAt    int64  `json:"-"`
	LeaseToken      string `json:"-" gorm:"size:64"`
	LeaseExpiresAt  int64  `json:"-"`
	NextAttemptAt   int64  `json:"-" gorm:"index"`
	StorageRetries  int    `json:"storage_retry_count"`
	ErrorMessage    string `json:"error_message,omitempty" gorm:"type:text"`
	CreatedAt       int64  `json:"created_at" gorm:"index"`
	ExpiresAt       int64  `json:"expires_at"`
}

type MediaArtifactObject struct {
	ID           int    `json:"-"`
	ObjectId     string `json:"object_id" gorm:"size:64;uniqueIndex"`
	IdentityHash string `json:"-" gorm:"size:64;uniqueIndex"`
	TaskId       string `json:"task_id" gorm:"size:64;index"`
	ArtifactKey  string `json:"key" gorm:"size:128"`
	UserId       int    `json:"-" gorm:"index"`
	TokenId      int    `json:"-"`
	RootPath     string `json:"-" gorm:"type:text"`
	ObjectKey    string `json:"-" gorm:"size:255"`
	MimeType     string `json:"mime_type" gorm:"size:64"`
	ByteSize     int64  `json:"byte_size"`
	Checksum     string `json:"checksum" gorm:"size:64"`
	CreatedAt    int64  `json:"created_at"`
	ExpiresAt    int64  `json:"expires_at" gorm:"index"`
	Status       string `json:"-" gorm:"size:16"`
}

func AcceptAsyncMediaJob(ctx context.Context, job AsyncMediaJob) (AsyncMediaJob, bool, error) {
	reused := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", job.TokenId, job.UserId).Take(&token).Error; err != nil {
			return err
		}
		var existing AsyncMediaJob
		err := tx.Where("identity_hash = ?", job.IdentityHash).Take(&existing).Error
		if err == nil {
			if existing.RequestHash != job.RequestHash {
				return ErrImageConflict
			}
			job, reused = existing, true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		job.Status, job.StorageStatus, job.BillingStatus = "queued", "pending", "pending"
		job.CreatedAt = time.Now().Unix()
		return tx.Create(&job).Error
	})
	return job, reused, err
}

func ClaimAsyncMediaJob(ctx context.Context, job *AsyncMediaJob, lease string, seconds int) (bool, error) {
	now := time.Now().Unix()
	result := DB.WithContext(ctx).Model(&AsyncMediaJob{}).Where("id = ? AND lease_expires_at <= ? AND next_attempt_at <= ? AND status IN ?", job.ID, now, now, []string{"queued", "submitting", "submitted"}).Updates(map[string]any{"lease_token": lease, "lease_expires_at": now + int64(seconds)})
	if result.Error != nil || result.RowsAffected == 0 {
		return false, result.Error
	}
	job.LeaseToken, job.LeaseExpiresAt = lease, now+int64(seconds)
	return true, nil
}

func UpdateAsyncMediaJob(ctx context.Context, job AsyncMediaJob, updates map[string]any) error {
	result := DB.WithContext(ctx).Model(&AsyncMediaJob{}).Where("id = ? AND lease_token = ?", job.ID, job.LeaseToken).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImageConflict
	}
	return nil
}
