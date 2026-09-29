package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"gorm.io/gorm"
)

var ErrImageInsufficientQuota = errors.New("insufficient image billing quota")

type ImageFundingSelection struct {
	Source         string `json:"source"`
	SubscriptionId int    `json:"subscription_id"`
}

// SelectImageFunding checks funding without reserving wallet or token quota.
// A due subscription reset is evaluated as available and applied at settlement.
func SelectImageFunding(ctx context.Context, userId, amount int, preference string) (ImageFundingSelection, error) {
	if amount < 0 {
		return ImageFundingSelection{}, ErrImageInsufficientQuota
	}
	var user User
	if err := DB.WithContext(ctx).Where("id = ? AND status = ?", userId, common.UserStatusEnabled).First(&user).Error; err != nil {
		return ImageFundingSelection{}, err
	}
	if amount == 0 {
		return ImageFundingSelection{Source: "wallet"}, nil
	}
	walletAvailable := user.Quota >= amount
	var subs []UserSubscription
	now := time.Now().Unix()
	if err := DB.WithContext(ctx).Where("user_id = ? AND status = ? AND end_time > ?", userId, "active", now).Order("end_time asc, id asc").Find(&subs).Error; err != nil {
		return ImageFundingSelection{}, err
	}
	var selected int
	allowOverflow := false
	for _, sub := range subs {
		allowOverflow = allowOverflow || sub.AllowWalletOverflow
		used := sub.AmountUsed
		if sub.NextResetTime > 0 && sub.NextResetTime <= now {
			used = 0
		}
		if selected == 0 && (sub.AmountTotal == 0 || sub.AmountTotal-used >= int64(amount)) {
			selected = sub.Id
		}
	}
	pref := common.NormalizeBillingPreference(preference)
	if pref == "wallet_only" || pref == "wallet_first" && walletAvailable {
		if walletAvailable {
			return ImageFundingSelection{Source: "wallet"}, nil
		}
		return ImageFundingSelection{}, ErrImageInsufficientQuota
	}
	if selected > 0 {
		return ImageFundingSelection{Source: "subscription", SubscriptionId: selected}, nil
	}
	if pref != "subscription_only" && walletAvailable && (pref == "wallet_first" || len(subs) == 0 || allowOverflow) {
		return ImageFundingSelection{Source: "wallet"}, nil
	}
	return ImageFundingSelection{}, ErrImageInsufficientQuota
}

// StageAsyncImageOutput fixes the entire output and bill in one main-DB commit.
func StageAsyncImageOutput(ctx context.Context, task AsyncImageTask, images []AsyncImageStagingObject, bill AsyncImageBill) error {
	return stageAsyncImageOutput(ctx, task, images, nil, bill)
}

// StageAsyncImageUpstreamOutput fixes the public result manifest and bill
// without creating local storage objects for media that was not downloaded.
func StageAsyncImageUpstreamOutput(ctx context.Context, task AsyncImageTask, urls []string, bill AsyncImageBill) error {
	return stageAsyncImageOutput(ctx, task, nil, urls, bill)
}

func stageAsyncImageOutput(ctx context.Context, task AsyncImageTask, images []AsyncImageStagingObject, urls []string, bill AsyncImageBill) error {
	count := len(images) + len(urls)
	if count == 0 || count > dto.MaxImageN || len(images) > 0 && len(urls) > 0 ||
		bill.Quota < 0 || bill.Quota > math.MaxInt32 || bill.TaskId != task.TaskId || bill.TokenId != task.TokenId || bill.UserId != task.UserId || bill.Fingerprint == "" || bill.BillingRequestId != "async-image:"+task.TaskId {
		return errors.New("invalid immutable image output")
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().Unix()
		var sizes []string
		for _, image := range images {
			sizes = append(sizes, fmt.Sprintf("%dx%d", image.Width, image.Height))
		}
		actualSize := strings.Join(sizes, ",")
		if len(actualSize) > 250 {
			actualSize = actualSize[:250] + "..."
		}
		status, source, resultCount, progress := ImageTaskUpstreamSucceeded, "", 0, 65
		if len(urls) > 0 {
			if task.UpstreamTaskId == "" {
				return ErrImageConflict
			}
			status, source, resultCount, progress = ImageTaskBillingPending, ImageResultSourceUpstream, count, 85
		}
		result := tx.Model(&AsyncImageTask{}).Where("task_id = ? AND version = ? AND status = ? AND lease_token = ? AND lease_expires_at > ?", task.TaskId, task.Version, ImageTaskInvoking, task.LeaseToken, now).Updates(map[string]any{"status": status, "version": task.Version + 1, "updated_at": now, "upstream_succeeded_at": now, "request_cipher": nil, "image_count": count, "result_count": resultCount, "result_source": source, "quota": bill.Quota, "billing_status": "pending", "progress": progress, "attempts": task.Attempts, "actual_size": actualSize, "error_code": "", "error_message": "", "public_error_code": 0})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrImageConflict
		}
		if len(urls) > 0 {
			recorded, err := getAsyncImageUpstreamResultURLs(tx, task.TaskId)
			if err != nil {
				return err
			}
			if !slices.Equal(recorded, urls) {
				return ErrImageConflict
			}
		}
		for index := range images {
			image := &images[index]
			if len(image.Data) == 0 || image.Checksum == "" || image.Width <= 0 || image.Height <= 0 {
				return errors.New("invalid staged image")
			}
			image.TaskId, image.ImageIndex = task.TaskId, index
			if err := tx.Create(image).Error; err != nil {
				return err
			}
		}
		bill.Status, bill.LogStatus, bill.CreatedAt = "fixed", "pending", now
		var reservation AsyncImageBill
		// The task CAS above already holds the ledger's serialization lock.
		reservationErr := tx.Where("task_id = ?", task.TaskId).Take(&reservation).Error
		if reservationErr == nil {
			if reservation.Status != "reserved" || reservation.FundingSource != bill.FundingSource || reservation.SubscriptionId != bill.SubscriptionId || reservation.UserId != bill.UserId || reservation.TokenId != bill.TokenId {
				return ErrImageConflict
			}
			bill.Id, bill.ReservedAt = reservation.Id, reservation.ReservedAt
			bill.ReservedQuota, bill.ReservedTokenQuota = reservation.ReservedQuota, reservation.ReservedTokenQuota
			bill.SubscriptionPeriodStart = reservation.SubscriptionPeriodStart
		} else if !errors.Is(reservationErr, gorm.ErrRecordNotFound) {
			return reservationErr
		}
		if err := tx.Save(&bill).Error; err != nil {
			return err
		}
		eventKey := common.GetUUID()
		var received int64
		if task.UpstreamTaskId != "" {
			if err := tx.Model(&AsyncImageEvent{}).Where("task_id = ? AND event_type = ?", task.TaskId, upstreamImageResultEvent).Count(&received).Error; err != nil {
				return err
			}
		}
		if received == 0 {
			event := AsyncImageEvent{TaskId: task.TaskId, EventKey: eventKey, EventType: "upstream_succeeded", Status: ImageTaskUpstreamSucceeded, Message: "Image output and immutable bill persisted", CreatedAt: now}
			if err := tx.Create(&event).Error; err != nil {
				return err
			}
		}
		return tx.Create(&ImageOutbox{EventKey: eventKey, Kind: "postprocess", AggregateId: task.TaskId, Status: "pending", NextAttemptAt: now, CreatedAt: now}).Error
	})
}

// ApplyAsyncImageBill commits every main-DB financial effect with the ledger.
// Repeating the same fingerprint succeeds; a different command never replaces it.
func ApplyAsyncImageBill(ctx context.Context, taskId, fingerprint string) error {
	var pending AsyncImageBill
	if err := DB.WithContext(ctx).Where("task_id = ?", taskId).Take(&pending).Error; err != nil {
		return err
	}
	if pending.Status != "applied" && (pending.Quota > pending.ReservedQuota || pending.Quota > pending.ReservedTokenQuota) {
		if err := prepareAsyncImageQuotaCaches(ctx, pending.UserId, pending.TokenId); err != nil {
			return err
		}
	}
	var cached asyncImageQuotaCacheReservation
	commitAttempted := false
	var userID, tokenID, walletRefund, tokenRefund, tokenUsage int
	var tokenKey string
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task AsyncImageTask
		if err := lockForUpdate(tx).Where("task_id = ?", taskId).First(&task).Error; err != nil {
			return err
		}
		var bill AsyncImageBill
		if err := lockForUpdate(tx).Where("task_id = ?", taskId).First(&bill).Error; err != nil {
			return err
		}
		if bill.Fingerprint != fingerprint || bill.TokenId != task.TokenId || bill.UserId != task.UserId {
			return ErrImageConflict
		}
		if bill.Status == "applied" {
			return nil
		}
		if task.Terminal() || task.Status != ImageTaskBillingPending || task.ResultCount != task.ImageCount || task.ResultCount == 0 || bill.Status != "fixed" || bill.Quota < 0 || bill.Quota > math.MaxInt32 {
			return ErrImageConflict
		}
		var resultCount int64
		if err := tx.Model(&AsyncImageResult{}).Where("task_id = ?", taskId).Count(&resultCount).Error; err != nil {
			return err
		}
		if task.ResultSource == ImageResultSourceUpstream {
			urls, err := getAsyncImageUpstreamResultURLs(tx, taskId)
			if err != nil {
				return err
			}
			if resultCount != 0 || len(urls) != task.ImageCount {
				return ErrImageConflict
			}
		} else if task.ResultSource != "" || resultCount != int64(task.ImageCount) {
			return ErrImageConflict
		}
		var user User
		if err := lockForUpdate(tx.Unscoped()).Where("id = ?", bill.UserId).First(&user).Error; err != nil {
			return err
		}
		var token Token
		// Soft-deleted credentials can settle previous work, never authenticate it.
		if err := lockForUpdate(tx.Unscoped()).Where("id = ? AND user_id = ?", bill.TokenId, bill.UserId).First(&token).Error; err != nil {
			return err
		}
		amount := bill.Quota
		fundingDelta, tokenDelta := amount-bill.ReservedQuota, amount-bill.ReservedTokenQuota
		if !token.UnlimitedQuota && token.RemainQuota < max(0, tokenDelta) {
			return ErrImageInsufficientQuota
		}
		if user.UsedQuota > math.MaxInt64-amount || tokenDelta > 0 && token.UsedQuota > math.MaxInt64-tokenDelta || tokenDelta < 0 && token.UsedQuota < -tokenDelta || user.RequestCount == math.MaxInt64 {
			return errors.New("image accounting counter overflow")
		}
		walletCharge, tokenCharge := 0, 0
		if bill.FundingSource == "wallet" {
			if fundingDelta > 0 && user.Quota < fundingDelta || fundingDelta < 0 && user.Quota > math.MaxInt64+fundingDelta {
				return ErrImageInsufficientQuota
			}
			walletCharge = max(0, fundingDelta)
			walletRefund = max(0, -fundingDelta)
			user.Quota -= fundingDelta
		} else if bill.FundingSource == "subscription" {
			var sub UserSubscription
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", bill.SubscriptionId, bill.UserId).First(&sub).Error; err != nil {
				return err
			}
			plan, err := getSubscriptionPlanByIdTx(tx, sub.PlanId)
			if err != nil {
				return err
			}
			if err := maybeResetUserSubscriptionWithPlanTx(tx, &sub, plan, time.Now().Unix()); err != nil {
				return err
			}
			samePeriod := bill.ReservedAt == 0 || SubscriptionReservationMatchesPeriod(&sub, bill.SubscriptionPeriodStart, bill.ReservedAt)
			if fundingDelta > 0 && (!samePeriod || sub.Status != "active" || sub.EndTime <= time.Now().Unix() || sub.AmountUsed > math.MaxInt64-int64(fundingDelta) || sub.AmountTotal > 0 && sub.AmountTotal-sub.AmountUsed < int64(fundingDelta)) {
				return ErrImageInsufficientQuota
			}
			if bill.SubscriptionAmount != int64(amount) {
				return ErrImageConflict
			}
			if samePeriod {
				if err := tx.Model(&UserSubscription{}).Where("id = ?", sub.Id).Update("amount_used", max(0, sub.AmountUsed+int64(fundingDelta))).Error; err != nil {
					return err
				}
			}
		} else {
			return errors.New("invalid image funding source")
		}
		if !token.UnlimitedQuota {
			tokenCharge = max(0, tokenDelta)
		}
		tokenRefund = max(0, -tokenDelta)
		if token.RemainQuota > math.MaxInt64-tokenRefund {
			return errors.New("image token refund overflow")
		}
		if err := cached.reserve(user.Id, token, walletCharge, tokenCharge); err != nil {
			return err
		}
		if err := tx.Unscoped().Model(&User{}).Where("id = ?", user.Id).Updates(map[string]any{"quota": user.Quota, "used_quota": user.UsedQuota + amount, "request_count": user.RequestCount + 1}).Error; err != nil {
			return err
		}
		tokenUpdates := map[string]any{"used_quota": token.UsedQuota + tokenDelta, "accessed_time": time.Now().Unix(), "remain_quota": token.RemainQuota - tokenCharge + tokenRefund}
		if err := tx.Unscoped().Model(&Token{}).Where("id = ?", token.Id).Updates(tokenUpdates).Error; err != nil {
			return err
		}
		var channel Channel
		if err := lockForUpdate(tx).Where("id = ?", bill.ChannelId).First(&channel).Error; err != nil {
			return err
		}
		if channel.UsedQuota > math.MaxInt64-int64(amount) {
			return errors.New("image channel counter overflow")
		}
		if err := tx.Model(&Channel{}).Where("id = ?", channel.Id).Update("used_quota", channel.UsedQuota+int64(amount)).Error; err != nil {
			return err
		}
		if common.DataExportEnabled {
			var log Log
			if err := common.UnmarshalJsonStr(bill.LogPayload, &log); err != nil {
				return err
			}
			data := AsyncImageUsageProjection{EventId: bill.BillingRequestId, UserID: user.Id, Username: user.Username, ModelName: task.Model, CreatedAt: bill.CreatedAt - bill.CreatedAt%3600, UseGroup: task.Group, TokenID: token.Id, ChannelID: channel.Id, NodeName: common.NodeName, Count: 1, Quota: amount, TokenUsed: log.PromptTokens + log.CompletionTokens}
			if err := tx.Create(&data).Error; err != nil {
				return err
			}
		}
		result := tx.Model(&AsyncImageBill{}).Where("id = ? AND status = ? AND fingerprint = ?", bill.Id, "fixed", fingerprint).Updates(map[string]any{"status": "applied", "applied_at": time.Now().Unix()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: concurrent image bill", ErrImageConflict)
		}
		userID, tokenID, tokenKey = user.Id, token.Id, token.Key
		if token.UnlimitedQuota {
			tokenUsage = max(0, tokenDelta)
		}
		commitAttempted = true
		return nil
	})
	if err != nil {
		if !commitAttempted {
			cached.compensate()
		}
		return err
	}
	syncAsyncImageQuotaCache(userID, tokenID, tokenKey, walletRefund, tokenRefund, tokenUsage)
	return nil
}
