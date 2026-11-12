package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SelectImageResultDelivery freezes one delivery decision without replacing the
// validated storage object or the immutable generation bill. Upload retries and
// task recovery therefore never request a second generation.
func SelectImageResultDelivery(ctx context.Context, task model.AsyncImageTask, object *model.ImageStorageObject, image ImageBytes) error {
	if object.DeliverySource != "" {
		return nil
	}
	source, link, reason := "local", "", "disabled"
	cfg, configErr := GetImageHostingConfig(ctx)
	if configErr != nil {
		reason = "configuration_unavailable"
	} else if cfg.Enabled && cfg.ImageEnabled {
		managed, err := CanvasImageTaskManaged(ctx, task)
		if err != nil || managed {
			// Public image hosts must never weaken confidential-client ownership
			// checks, including when provenance cannot be established.
			reason = "protected_result"
		} else {
			uploaded, err := UploadHostedImage(ctx, object.ObjectId, image.SourceURL, image)
			if err == nil {
				if _, err = ValidateHostedImageURL(uploaded); err == nil {
					source, link, reason = "hosted", uploaded, "uploaded"
				}
			}
			if source != "hosted" {
				reason = "upload_failed"
				if errors.Is(err, ErrImageHostingCircuitOpen) {
					reason = "circuit_open"
				} else if errors.Is(err, ErrImageHostingBusy) {
					reason = "upload_busy"
				}
				if cfg.URLFallback == "upstream" && PublicUpstreamAsyncResultURL(image.SourceURL) {
					source, link = "upstream", image.SourceURL
				}
			}
		}
	}
	return model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		changed := tx.Model(&model.ImageStorageObject{}).
			Where("object_id = ? AND status = ? AND checksum = ?", object.ObjectId, "active", object.Checksum).
			Where("delivery_source IS NULL OR delivery_source = ?", "").
			Updates(map[string]any{"delivery_source": source, "delivery_url": link})
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected == 0 {
			return tx.Where("object_id = ? AND status = ?", object.ObjectId, "active").Take(object).Error
		}
		object.DeliverySource, object.DeliveryURL = source, link
		if reason == "disabled" {
			return nil
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.AsyncImageEvent{
			TaskId: task.TaskId, EventKey: ImageIdentityHash("image-delivery", task.TaskId, object.ObjectId),
			EventType: "image_delivery_selected", Status: model.ImageTaskUploading,
			// Do not record URLs, tokens, raw HTTP responses or provider headers.
			Message: "Image delivery: " + source + " (" + reason + ")", CreatedAt: time.Now().Unix(),
		}).Error
	})
}

// ImageResultDeliveryURL returns only a previously selected public delivery
// capability. Callers must still check task ownership, expiry and availability.
func ImageResultDeliveryURL(object model.ImageStorageObject) string {
	if object.Status != "active" || object.ExpiresAt > 0 && object.ExpiresAt <= time.Now().Unix() {
		return ""
	}
	switch object.DeliverySource {
	case "hosted":
		if _, err := ValidateHostedImageURL(object.DeliveryURL); err == nil {
			return object.DeliveryURL
		}
	case "upstream":
		if PublicUpstreamAsyncResultURL(object.DeliveryURL) {
			return object.DeliveryURL
		}
	}
	return ""
}
