package service

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

type ImageAssetMetadata struct {
	Title          string `json:"title"`
	PrivatePrompt  string `json:"private_prompt"`
	Prompt         string `json:"prompt,omitempty"`
	PublicTitle    string `json:"public_title"`
	SharePrompt    bool   `json:"share_prompt"`
	Platform       string `json:"platform"`
	Model          string `json:"model"`
	GenerationMode string `json:"generation_mode"`
	SourceType     string `json:"source_type"`
	RequestedSize  string `json:"requested_size"`
	AspectRatio    string `json:"aspect_ratio"`
	Quality        string `json:"quality"`
	ContentType    string `json:"content_type"`
	ByteSize       int64  `json:"byte_size"`
	Checksum       string `json:"checksum_sha256"`
	ClientBlobKey  string `json:"client_blob_key"`
	ImageURL       string `json:"image_url,omitempty"`
}

func (metadata ImageAssetMetadata) Validate(cfg ImageRuntimeConfig, deferred bool) error {
	for _, value := range []string{metadata.Title, metadata.PublicTitle, metadata.PrivatePrompt, metadata.Prompt, metadata.ClientBlobKey, metadata.Model} {
		if !utf8.ValidString(value) {
			return errors.New("invalid UTF-8 image metadata")
		}
	}
	if len(metadata.Title) > 1000 || len(metadata.PublicTitle) > 1000 || len(metadata.PrivatePrompt) > 64<<10 || len(metadata.Prompt) > 64<<10 || len(metadata.Model) > 255 || len(metadata.ClientBlobKey) > 255 || len(metadata.RequestedSize) > 64 || len(metadata.AspectRatio) > 32 {
		return errors.New("image metadata exceeds limits")
	}
	if metadata.Platform != "" && metadata.Platform != "openai" && metadata.Platform != "gemini" {
		return errors.New("invalid image platform")
	}
	if deferred {
		checksum, err := hex.DecodeString(metadata.Checksum)
		if err != nil || len(checksum) != 32 || strings.ToLower(metadata.Checksum) != metadata.Checksum || metadata.ByteSize <= 0 || metadata.ByteSize > cfg.LibraryImageBytes || metadata.ClientBlobKey == "" || (metadata.ContentType != "image/png" && metadata.ContentType != "image/jpeg" && metadata.ContentType != "image/webp") {
			return errors.New("invalid original image identity")
		}
	}
	return nil
}

func ArchiveImageBytes(ctx context.Context, userId int, image ImageBytes, metadata ImageAssetMetadata, operation, source, submissionId string, cfg ImageRuntimeConfig) (model.ImageLibraryItem, bool, error) {
	if err := metadata.Validate(cfg, false); err != nil {
		return model.ImageLibraryItem{}, false, err
	}
	encoded, err := common.Marshal(metadata)
	if err != nil {
		return model.ImageLibraryItem{}, false, err
	}
	fingerprint := ImageIdentityHash(image.Checksum, strconv.Itoa(len(image.Data)), string(encoded))
	sourceKey := ImageIdentityHash("image-library", strconv.Itoa(userId), source, operation)
	var old model.ImageLibraryItem
	err = model.DB.WithContext(ctx).Where("source_key = ?", sourceKey).Take(&old).Error
	if err == nil {
		if old.UserId != userId || old.Fingerprint != fingerprint || old.DeletedAt != 0 {
			return old, false, model.ErrImageConflict
		}
		return old, true, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return old, false, err
	}
	intentKey := ImageIdentityHash("image-archive", sourceKey)
	var intent model.ImageUploadIntent
	err = model.DB.WithContext(ctx).Where("intent_key = ?", intentKey).Take(&intent).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		store, getErr := GetImageStorage(ctx, "durable")
		if getErr != nil {
			return old, false, getErr
		}
		now := time.Now().Unix()
		key := ImageResultObjectKey(model.AsyncImageTask{TaskId: sourceKey, CreatedAt: now}, 0, image.ContentType)
		intent = model.ImageUploadIntent{IntentKey: intentKey, UserId: userId, Class: "durable", ProfileId: store.Profile.ProfileId, ObjectKey: key, ContentType: image.ContentType, ByteSize: int64(len(image.Data)), Checksum: image.Checksum, Status: "pending", CreatedAt: now}
	} else if err != nil {
		return old, false, err
	}
	object, err := StoreImageIntent(ctx, intent, image, 0)
	if err != nil {
		return old, false, err
	}
	prompt := metadata.PrivatePrompt
	if prompt == "" {
		prompt = metadata.Prompt
	}
	item := model.ImageLibraryItem{AssetId: "img_" + common.GetUUID(), UserId: userId, ObjectId: object.ObjectId, SourceKey: sourceKey, Fingerprint: fingerprint, Source: source, Platform: metadata.Platform, Model: metadata.Model, Prompt: prompt, Title: metadata.Title, RequestedSize: metadata.RequestedSize, AspectRatio: metadata.AspectRatio, CreatedAt: intent.CreatedAt, ExpiresAt: intent.CreatedAt + int64(cfg.LibraryRetentionDays)*86400}
	return model.CommitImageLibraryItem(ctx, item, cfg.LibraryItems, cfg.LibraryBytes, submissionId)
}

func ArchiveAsyncImageResult(ctx context.Context, userId int, taskId string, index int, title string, cfg ImageRuntimeConfig) (model.ImageLibraryItem, bool, error) {
	var task model.AsyncImageTask
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", taskId, userId).Take(&task).Error; err != nil {
		return model.ImageLibraryItem{}, false, err
	}
	if !task.ResultsAvailable() || index < 0 || index >= task.ImageCount {
		return model.ImageLibraryItem{}, false, model.ErrImageConflict
	}
	var result model.AsyncImageResult
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND image_index = ?", taskId, index).Take(&result).Error; err != nil {
		return model.ImageLibraryItem{}, false, err
	}
	var object model.ImageStorageObject
	if err := model.DB.WithContext(ctx).Where("object_id = ? AND status = ?", result.ObjectId, "active").Take(&object).Error; err != nil {
		return model.ImageLibraryItem{}, false, err
	}
	store, err := GetImageObjectStorage(ctx, object)
	if err != nil {
		return model.ImageLibraryItem{}, false, err
	}
	data, err := store.Read(ctx, object.ObjectKey, cfg.LibraryImageBytes)
	if err != nil {
		return model.ImageLibraryItem{}, false, err
	}
	image, err := ValidateImageBytes(data, object.ContentType, cfg.LibraryImageBytes, cfg.LibraryImagePixels)
	if err != nil {
		return model.ImageLibraryItem{}, false, err
	}
	if image.Checksum != object.Checksum {
		return model.ImageLibraryItem{}, false, model.ErrImageConflict
	}
	metadata := ImageAssetMetadata{Title: title, Platform: task.Platform, Model: task.Model, RequestedSize: task.RequestedSize, AspectRatio: task.AspectRatio}
	item, reused, err := ArchiveImageBytes(ctx, userId, image, metadata, taskId+":"+strconv.Itoa(index), "async_task", "", cfg)
	return item, reused, err
}
