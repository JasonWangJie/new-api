package model

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	ImageTaskQueued            = "queued"
	ImageTaskInvoking          = "invoking"
	ImageTaskUpstreamSucceeded = "upstream_succeeded"
	ImageTaskUploading         = "uploading"
	ImageTaskBillingPending    = "billing_pending"
	ImageTaskStorageFailed     = "storage_failed"
	ImageTaskBillingFailed     = "billing_failed"
	ImageTaskSucceeded         = "succeeded"
	ImageTaskFailed            = "failed"
	ImageTaskExpired           = "expired"
	ImageTaskExecutionUnknown  = "execution_unknown"
)

var ErrImageConflict = errors.New("image operation conflict")

// ImageGroupPolicy is separate from the legacy group and channel settings.
// PolicyKey hashes the case-sensitive group/platform pair for portable indexes.
type ImageGroupPolicy struct {
	Id           int    `json:"id"`
	PolicyKey    string `json:"-" gorm:"size:64;uniqueIndex"`
	Group        string `json:"group" gorm:"size:255"`
	Platform     string `json:"platform" gorm:"size:16"`
	Enabled      bool   `json:"enabled"`
	AsyncEnabled bool   `json:"async_enabled"`
	PoolMode     string `json:"pool_mode" gorm:"size:32"`
	Models       string `json:"models" gorm:"type:text"`
	Version      int64  `json:"version"`
}

type TokenImagePlatformMapping struct {
	Id       int    `json:"id"`
	TokenId  int    `json:"token_id" gorm:"uniqueIndex:idx_image_token_platform,priority:1"`
	Platform string `json:"platform" gorm:"size:16;uniqueIndex:idx_image_token_platform,priority:2"`
	Group    string `json:"group" gorm:"size:255"`
}

type ImageChannelPool struct {
	Id         int    `json:"id"`
	BindingKey string `json:"binding_key" gorm:"size:64;uniqueIndex:idx_image_pool_channel,priority:1"`
	Group      string `json:"group" gorm:"size:255"`
	Platform   string `json:"platform" gorm:"size:16"`
	Mode       string `json:"mode" gorm:"size:32"`
	Model      string `json:"model" gorm:"size:255"`
	Resolution string `json:"resolution" gorm:"size:16"`
	ChannelId  int    `json:"channel_id" gorm:"uniqueIndex:idx_image_pool_channel,priority:2"`
	Priority   int    `json:"priority"`
}

type AsyncImageTask struct {
	Id                   int    `json:"-"`
	TaskId               string `json:"task_id" gorm:"size:64;uniqueIndex"`
	UserId               int    `json:"user_id" gorm:"index:idx_image_user_created,priority:1"`
	TokenId              int    `json:"api_key_id" gorm:"index"`
	Group                string `json:"group" gorm:"size:255"`
	Dialect              string `json:"dialect" gorm:"size:16"`
	Platform             string `json:"platform" gorm:"size:16"`
	RequestType          string `json:"request_type" gorm:"size:32"`
	Model                string `json:"model" gorm:"size:255"`
	SourcePath           string `json:"source_path" gorm:"size:255"`
	Status               string `json:"status" gorm:"size:32;index:idx_image_status_due,priority:1"`
	BillingStatus        string `json:"billing_status" gorm:"size:32"`
	Version              int64  `json:"version"`
	Progress             int    `json:"progress"`
	RequestCipher        []byte `json:"-"`
	RequestHash          string `json:"-" gorm:"size:64"`
	PromptSummary        string `json:"prompt_summary" gorm:"type:text"`
	ReferenceUrls        string `json:"-" gorm:"type:text"`
	ChannelId            int    `json:"-" gorm:"index"`
	Attempts             string `json:"-" gorm:"type:text"`
	RequestedSize        string `json:"requested_size" gorm:"size:64"`
	RequestedResolution  string `json:"requested_resolution" gorm:"size:16"`
	AspectRatio          string `json:"aspect_ratio" gorm:"size:32"`
	ActualSize           string `json:"actual_size" gorm:"size:255"`
	ImageCount           int    `json:"image_count"`
	ResultCount          int    `json:"result_count"`
	Quota                int    `json:"quota"`
	RetryCount           int    `json:"retry_count"`
	StorageRetryCount    int    `json:"storage_retry_count"`
	BillingRetryCount    int    `json:"billing_retry_count"`
	ReferenceRetryCount  int    `json:"reference_retry_count"`
	TransientRetryCount  int    `json:"transient_retry_count"`
	CapacityRetryCount   int    `json:"capacity_retry_count"`
	ErrorCode            string `json:"error_code" gorm:"size:64"`
	ErrorMessage         string `json:"error_message" gorm:"type:text"`
	PublicErrorCode      int    `json:"-"`
	ReconciliationStatus string `json:"reconciliation_status" gorm:"size:32"`
	LeaseToken           string `json:"-" gorm:"size:64"`
	HeartbeatAt          int64  `json:"-" gorm:"type:bigint"`
	LeaseExpiresAt       int64  `json:"-" gorm:"type:bigint"`
	DispatchedAt         int64  `json:"-" gorm:"type:bigint"`
	NextAttemptAt        int64  `json:"next_attempt_at" gorm:"type:bigint;index:idx_image_status_due,priority:2"`
	CreatedAt            int64  `json:"created_at" gorm:"type:bigint;index:idx_image_user_created,priority:2"`
	UpdatedAt            int64  `json:"updated_at" gorm:"type:bigint"`
	StartedAt            int64  `json:"started_at" gorm:"type:bigint"`
	UpstreamSucceededAt  int64  `json:"upstream_succeeded_at" gorm:"type:bigint"`
	FinishedAt           int64  `json:"finished_at" gorm:"type:bigint"`
	ExpiresAt            int64  `json:"expires_at" gorm:"type:bigint;index"`
}

func (t AsyncImageTask) DisplayStatus() string {
	if t.Status == ImageTaskInvoking && t.ChannelId == 0 {
		return ImageTaskQueued
	}
	return t.Status
}

func (t AsyncImageTask) ResultsAvailable() bool {
	return t.Status == ImageTaskSucceeded && t.ResultCount > 0 && (t.ExpiresAt == 0 || t.ExpiresAt > common.GetTimestamp()) && (t.BillingStatus == "succeeded" || t.BillingStatus == "not_billable")
}

func (t AsyncImageTask) Terminal() bool {
	return t.Status == ImageTaskSucceeded || t.Status == ImageTaskFailed || t.Status == ImageTaskExpired || t.Status == ImageTaskExecutionUnknown
}

type AsyncImageIdempotency struct {
	Id          int
	TokenId     int    `gorm:"uniqueIndex:idx_image_idempotency,priority:1"`
	KeyHash     string `gorm:"size:64;uniqueIndex:idx_image_idempotency,priority:2"`
	RequestHash string `gorm:"size:64"`
	TaskId      string `gorm:"size:64"`
}

type AsyncImageEvent struct {
	Id        int    `json:"id"`
	TaskId    string `json:"-" gorm:"size:64;index"`
	EventKey  string `json:"-" gorm:"size:64;uniqueIndex"`
	EventType string `json:"event_type" gorm:"size:64"`
	Status    string `json:"status" gorm:"size:32"`
	Message   string `json:"message" gorm:"type:text"`
	CreatedAt int64  `json:"created_at" gorm:"type:bigint"`
}

type ImageOutbox struct {
	Id             int
	EventKey       string `gorm:"size:64;uniqueIndex"`
	Kind           string `gorm:"size:32"`
	AggregateId    string `gorm:"size:64"`
	Status         string `gorm:"size:16;index:idx_image_outbox_due,priority:1"`
	LeaseToken     string `gorm:"size:64"`
	LeaseExpiresAt int64  `gorm:"type:bigint"`
	NextAttemptAt  int64  `gorm:"type:bigint;index:idx_image_outbox_due,priority:2"`
	CreatedAt      int64  `gorm:"type:bigint"`
}

type AsyncImageStagingObject struct {
	Id          int
	TaskId      string `gorm:"size:64;uniqueIndex:idx_image_staging_index,priority:1"`
	ImageIndex  int    `gorm:"uniqueIndex:idx_image_staging_index,priority:2"`
	Data        []byte
	ContentType string `gorm:"size:64"`
	Checksum    string `gorm:"size:64"`
	Width       int
	Height      int
}

// ImageStorageProfile revisions remain addressable for existing ObjectRefs.
type ImageStorageProfile struct {
	Id               int    `json:"id"`
	ProfileId        string `json:"profile_id" gorm:"size:64;uniqueIndex"`
	Class            string `json:"class" gorm:"size:16;index"`
	Backend          string `json:"backend" gorm:"size:16"`
	Provider         string `json:"provider" gorm:"size:32"`
	Root             string `json:"root" gorm:"type:text"`
	Endpoint         string `json:"endpoint" gorm:"type:text"`
	Region           string `json:"region" gorm:"size:64"`
	Bucket           string `json:"bucket" gorm:"size:255"`
	Prefix           string `json:"prefix" gorm:"size:255"`
	PathStyle        bool   `json:"path_style"`
	CredentialCipher []byte `json:"-"`
	Active           bool   `json:"active"`
	CreatedAt        int64  `json:"created_at" gorm:"type:bigint"`
}

type ImageStorageObject struct {
	Id                   int    `json:"-"`
	ObjectId             string `json:"id" gorm:"size:64;uniqueIndex"`
	IdentityHash         string `json:"-" gorm:"size:64;uniqueIndex"`
	ProfileId            string `json:"-" gorm:"size:64;index"`
	Class                string `json:"-" gorm:"size:16"`
	ObjectKey            string `json:"-" gorm:"size:255"`
	ContentType          string `json:"content_type" gorm:"size:64"`
	ByteSize             int64  `json:"byte_size" gorm:"type:bigint"`
	Checksum             string `json:"checksum" gorm:"size:64"`
	Width                int    `json:"width"`
	Height               int    `json:"height"`
	Status               string `json:"-" gorm:"size:16;index"`
	DeleteLease          string `json:"-" gorm:"size:64"`
	DeleteLeaseExpiresAt int64  `json:"-" gorm:"type:bigint"`
	CreatedAt            int64  `json:"created_at" gorm:"type:bigint"`
	ExpiresAt            int64  `json:"expires_at" gorm:"type:bigint;index"`
}

type ImageUploadIntent struct {
	Id             int
	IntentKey      string `gorm:"size:64;uniqueIndex"`
	TaskId         string `gorm:"size:64;index"`
	UserId         int    `gorm:"index"`
	TokenId        int    `gorm:"index"`
	ImageIndex     int
	Class          string `gorm:"size:16"`
	ProfileId      string `gorm:"size:64"`
	ObjectKey      string `gorm:"size:255"`
	ContentType    string `gorm:"size:64"`
	ByteSize       int64  `gorm:"type:bigint"`
	Checksum       string `gorm:"size:64"`
	Status         string `gorm:"size:16;index"`
	LeaseToken     string `gorm:"size:64"`
	LeaseExpiresAt int64  `gorm:"type:bigint"`
	FirstDeleteAt  int64  `gorm:"type:bigint"`
	CreatedAt      int64  `gorm:"type:bigint"`
}

type AsyncImageResult struct {
	Id         int    `json:"id"`
	TaskId     string `json:"-" gorm:"size:64;uniqueIndex:idx_image_result_index,priority:1"`
	ImageIndex int    `json:"image_index" gorm:"uniqueIndex:idx_image_result_index,priority:2"`
	ObjectId   string `json:"-" gorm:"size:64;index"`
	CreatedAt  int64  `json:"created_at" gorm:"type:bigint"`
}

type AsyncImageBill struct {
	Id                 int
	TaskId             string `gorm:"size:64;uniqueIndex"`
	BillingRequestId   string `gorm:"size:96;uniqueIndex"`
	Fingerprint        string `gorm:"size:64"`
	UserId             int
	TokenId            int
	ChannelId          int
	Quota              int
	FundingSource      string `gorm:"size:32"`
	SubscriptionId     int
	SubscriptionAmount int64  `gorm:"type:bigint"`
	Snapshot           string `gorm:"type:text"`
	Usage              string `gorm:"type:text"`
	Status             string `gorm:"size:16"`
	LogStatus          string `gorm:"size:16"`
	LogPayload         string `gorm:"type:text"`
	CreatedAt          int64  `gorm:"type:bigint"`
	AppliedAt          int64  `gorm:"type:bigint"`
}

type AsyncImageLogReceipt struct {
	Id          int
	EventId     string `gorm:"size:96;uniqueIndex"`
	Fingerprint string `gorm:"size:64"`
}

// Durable per-event analytics avoids the legacy in-memory batch accumulator.
type AsyncImageUsageProjection struct {
	Id        int
	EventId   string `gorm:"size:96;uniqueIndex"`
	UserID    int    `gorm:"index"`
	Username  string `gorm:"size:64"`
	ModelName string `gorm:"size:255"`
	CreatedAt int64  `gorm:"type:bigint;index"`
	UseGroup  string `gorm:"size:255"`
	TokenID   int
	ChannelID int
	NodeName  string `gorm:"size:64"`
	TokenUsed int
	Count     int
	Quota     int
}

type ImageInputAdmission struct {
	Id           int
	TokenId      int    `gorm:"uniqueIndex"`
	AttemptTimes string `gorm:"type:text"`
	Version      int64
}

type ImageUploadAdmissionTicket struct {
	Id         int
	TicketId   string `gorm:"size:64;uniqueIndex"`
	TokenId    int    `gorm:"index"`
	CreatedAt  int64  `gorm:"type:bigint;index"`
	ConsumedAt int64  `gorm:"type:bigint"`
}

type ImageInputObject struct {
	Id             int
	InputId        string `gorm:"size:64;uniqueIndex"`
	TokenId        int    `gorm:"uniqueIndex:idx_image_input_key,priority:1;index"`
	KeyHash        string `gorm:"size:64;uniqueIndex:idx_image_input_key,priority:2"`
	Fingerprint    string `gorm:"size:64"`
	Filename       string `gorm:"size:255"`
	ObjectId       string `gorm:"size:64;index"`
	IntentKey      string `gorm:"size:64"`
	ByteSize       int64  `gorm:"type:bigint"`
	Status         string `gorm:"size:16"`
	LeaseToken     string `gorm:"size:64"`
	LeaseExpiresAt int64  `gorm:"type:bigint"`
	CreatedAt      int64  `gorm:"type:bigint"`
	ExpiresAt      int64  `gorm:"type:bigint"`
}

type ImageInputAlias struct {
	Id      int
	UrlHash string `gorm:"size:64;uniqueIndex"`
	InputId string `gorm:"size:64;index"`
	TokenId int
}

type ImageInputTaskReference struct {
	Id      int
	TaskId  string `gorm:"size:64;uniqueIndex:idx_image_input_task,priority:1"`
	InputId string `gorm:"size:64;uniqueIndex:idx_image_input_task,priority:2;index"`
}

type ImageLibraryItem struct {
	Id            int    `json:"-"`
	AssetId       string `json:"id" gorm:"size:64;uniqueIndex"`
	UserId        int    `json:"user_id" gorm:"index"`
	ObjectId      string `json:"-" gorm:"size:64;index"`
	SourceKey     string `json:"-" gorm:"size:64;uniqueIndex"`
	Fingerprint   string `json:"-" gorm:"size:64"`
	Source        string `json:"source" gorm:"size:32"`
	TaskId        string `json:"task_id,omitempty" gorm:"size:64"`
	ImageIndex    int    `json:"image_index"`
	Platform      string `json:"platform" gorm:"size:16"`
	Model         string `json:"model" gorm:"size:255"`
	Prompt        string `json:"prompt" gorm:"type:text"`
	Title         string `json:"title" gorm:"type:text"`
	RequestedSize string `json:"requested_size" gorm:"size:64"`
	AspectRatio   string `json:"aspect_ratio" gorm:"size:32"`
	CreatedAt     int64  `json:"created_at" gorm:"type:bigint;index"`
	ExpiresAt     int64  `json:"expires_at" gorm:"type:bigint"`
	DeletedAt     int64  `json:"-" gorm:"type:bigint"`
}

type ImagePublication struct {
	Id            int    `json:"-"`
	PublicationId string `json:"id" gorm:"size:64;uniqueIndex"`
	AssetId       string `json:"asset_id" gorm:"size:64;index"`
	UserId        int    `json:"user_id" gorm:"index"`
	Status        string `json:"status" gorm:"size:32;index"`
	Title         string `json:"title" gorm:"type:text"`
	SharePrompt   bool   `json:"share_prompt"`
	PublicPrompt  string `json:"prompt,omitempty" gorm:"type:text"`
	Reason        string `json:"reason,omitempty" gorm:"type:text"`
	Version       int64  `json:"version"`
	CreatedAt     int64  `json:"created_at" gorm:"type:bigint"`
	PublishedAt   int64  `json:"published_at" gorm:"type:bigint;index"`
	ExpiresAt     int64  `json:"expires_at" gorm:"type:bigint"`
}

type ImageSubmissionRequest struct {
	Id            int    `json:"-"`
	SubmissionId  string `json:"id" gorm:"size:64;uniqueIndex"`
	UserId        int    `json:"user_id" gorm:"index"`
	OperationKey  string `json:"-" gorm:"size:64;uniqueIndex"`
	Fingerprint   string `json:"-" gorm:"size:64"`
	ClientBlobKey string `json:"client_blob_key" gorm:"size:255"`
	Checksum      string `json:"checksum" gorm:"size:64"`
	ByteSize      int64  `json:"byte_size" gorm:"type:bigint"`
	ContentType   string `json:"content_type" gorm:"size:64"`
	Metadata      string `json:"metadata" gorm:"type:text"`
	SharePrompt   bool   `json:"share_prompt"`
	Status        string `json:"status" gorm:"size:32;index"`
	Reason        string `json:"reason,omitempty" gorm:"type:text"`
	AssetId       string `json:"asset_id,omitempty" gorm:"size:64"`
	PublicationId string `json:"publication_id,omitempty" gorm:"size:64"`
	Version       int64  `json:"version"`
	CreatedAt     int64  `json:"created_at" gorm:"type:bigint"`
	ExpiresAt     int64  `json:"expires_at" gorm:"type:bigint"`
}

type ImageReport struct {
	Id            int    `json:"id"`
	PublicationId string `json:"publication_id" gorm:"size:64;index"`
	UserId        int    `json:"user_id"`
	OpenKey       string `json:"-" gorm:"size:64;uniqueIndex"`
	Category      string `json:"category" gorm:"size:32"`
	Message       string `json:"message" gorm:"type:text"`
	Status        string `json:"status" gorm:"size:16"`
	Resolution    string `json:"resolution,omitempty" gorm:"type:text"`
	CreatedAt     int64  `json:"created_at" gorm:"type:bigint"`
}

type ImageCleanupJob struct {
	Id             int    `json:"id"`
	JobId          string `json:"job_id" gorm:"size:64;uniqueIndex"`
	Scope          string `json:"scope" gorm:"size:32"`
	Filters        string `json:"filters" gorm:"type:text"`
	Cursor         int    `json:"-"`
	UserId         int    `json:"user_id"`
	Status         string `json:"status" gorm:"size:16;index"`
	LeaseToken     string `json:"-" gorm:"size:64"`
	LeaseExpiresAt int64  `json:"-" gorm:"type:bigint"`
	Total          int64  `json:"total"`
	Processed      int64  `json:"processed"`
	DeletedBytes   int64  `json:"deleted_bytes" gorm:"type:bigint"`
	Errors         int64  `json:"errors"`
	LastError      string `json:"last_error,omitempty" gorm:"type:text"`
	CreatedAt      int64  `json:"created_at" gorm:"type:bigint"`
	FinishedAt     int64  `json:"finished_at" gorm:"type:bigint"`
}

type ImageModerationEvent struct {
	Id        int    `json:"id"`
	SubjectId string `json:"subject_id" gorm:"size:64;index"`
	ActorId   int    `json:"actor_id"`
	Action    string `json:"action" gorm:"size:32"`
	Reason    string `json:"reason" gorm:"type:text"`
	CreatedAt int64  `json:"created_at" gorm:"type:bigint"`
}

func MigrateImageModels(db *gorm.DB) error {
	return db.AutoMigrate(&ImageGroupPolicy{}, &TokenImagePlatformMapping{}, &ImageChannelPool{}, &AsyncImageTask{}, &AsyncImageIdempotency{}, &AsyncImageEvent{}, &ImageOutbox{}, &AsyncImageStagingObject{}, &ImageStorageProfile{}, &ImageStorageObject{}, &ImageUploadIntent{}, &AsyncImageResult{}, &AsyncImageBill{}, &AsyncImageUsageProjection{}, &ImageInputAdmission{}, &ImageUploadAdmissionTicket{}, &ImageInputObject{}, &ImageInputAlias{}, &ImageInputTaskReference{}, &ImageLibraryItem{}, &ImagePublication{}, &ImageSubmissionRequest{}, &ImageReport{}, &ImageCleanupJob{}, &ImageModerationEvent{})
}

// TransitionImageTask owns the state CAS and its event/outbox transaction.
// External I/O must happen before or after this transaction, never inside it.
func TransitionImageTask(ctx context.Context, task AsyncImageTask, updates map[string]any, eventType, message, outboxKind string) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return transitionImageTaskTx(tx, task, updates, eventType, message, outboxKind)
	})
}

func transitionImageTaskTx(tx *gorm.DB, task AsyncImageTask, updates map[string]any, eventType, message, outboxKind string) error {
	now := time.Now().Unix()
	updates["version"] = task.Version + 1
	updates["updated_at"] = now
	query := tx.Model(&AsyncImageTask{}).Where("task_id = ? AND version = ? AND status = ?", task.TaskId, task.Version, task.Status)
	if eventType == "claimed" {
		query = query.Where("lease_token = ? AND lease_expires_at <= ?", task.LeaseToken, now)
	} else if task.LeaseToken != "" && eventType != "terminated" && eventType != "resumed" {
		query = query.Where("lease_token = ? AND lease_expires_at > ?", task.LeaseToken, now)
	}
	result := query.Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImageConflict
	}
	status := task.Status
	if value, ok := updates["status"].(string); ok {
		status = value
	}
	event := AsyncImageEvent{TaskId: task.TaskId, EventKey: common.GetUUID(), EventType: eventType, Status: status, Message: message, CreatedAt: now}
	if err := tx.Create(&event).Error; err != nil {
		return err
	}
	if outboxKind == "" {
		return nil
	}
	return tx.Create(&ImageOutbox{EventKey: event.EventKey, Kind: outboxKind, AggregateId: task.TaskId, Status: "pending", NextAttemptAt: now, CreatedAt: now}).Error
}
