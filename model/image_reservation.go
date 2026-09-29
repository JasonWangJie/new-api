package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// Image reservations share the live quota cache with ordinary relays. A cache
// miss during batch updates cannot safely be hydrated from a stale DB balance.
func prepareAsyncImageQuotaCaches(ctx context.Context, userID, tokenID int) error {
	if !common.RedisEnabled || common.BatchUpdateEnabled {
		return nil
	}
	if common.RDB == nil {
		return errors.New("image quota cache is unavailable")
	}
	var user User
	if err := DB.WithContext(ctx).Unscoped().Where("id = ?", userID).Take(&user).Error; err != nil {
		return err
	}
	if !user.DeletedAt.Valid {
		if _, err := GetUserCache(userID); err != nil {
			return err
		}
	}
	var token Token
	if err := DB.WithContext(ctx).Unscoped().Where("id = ? AND user_id = ?", tokenID, userID).Take(&token).Error; err != nil {
		return err
	}
	// A deleted credential may settle already accepted work, but must not be
	// placed back in the authentication cache.
	if token.DeletedAt.Valid || token.UnlimitedQuota {
		return nil
	}
	_, err := GetTokenByKey(token.Key, true)
	return err
}

type asyncImageQuotaCacheReservation struct {
	userID   int
	tokenID  int
	tokenKey string
	wallet   int
	token    int
}

func (r *asyncImageQuotaCacheReservation) reserve(userID int, token Token, wallet, tokenQuota int) error {
	r.userID, r.tokenID, r.tokenKey = userID, token.Id, token.Key
	if !common.RedisEnabled {
		return nil
	}
	if common.RDB == nil {
		return errors.New("image quota cache is unavailable")
	}
	if wallet > 0 {
		result, err := cacheTryReserveUserQuota(userID, int64(wallet))
		if err != nil || result == cacheQuotaMiss {
			return errors.New("image wallet quota cache is unavailable; dispatch was not authorized")
		}
		if result != cacheQuotaOK {
			return ErrImageInsufficientQuota
		}
		r.wallet = wallet
	}
	if tokenQuota > 0 && !token.DeletedAt.Valid {
		result, err := cacheTryReserveTokenQuota(token.Id, token.Key, int64(tokenQuota))
		if err != nil || result == cacheQuotaMiss {
			return errors.New("image token quota cache is unavailable; dispatch was not authorized")
		}
		if result != cacheQuotaOK {
			return ErrImageInsufficientQuota
		}
		r.token = tokenQuota
	}
	return nil
}

func (r *asyncImageQuotaCacheReservation) compensate() {
	syncAsyncImageQuotaCache(r.userID, r.tokenID, r.tokenKey, r.wallet, r.token, 0)
}

// Refunds and failed-transaction compensation are deltas, never cache
// invalidation: deleting a hot cache would discard unflushed ordinary usage.
// If a process dies between the DB commit and a refund projection, the cache
// remains conservatively lower until its normal expiry; the durable ledger
// remains authoritative. Do not retry an ambiguous additive cache operation.
func syncAsyncImageQuotaCache(userID, tokenID int, tokenKey string, wallet, tokenQuota, tokenUsage int) {
	if !common.RedisEnabled || common.RDB == nil {
		return
	}
	if wallet != 0 {
		if _, err := cacheApplyUserQuotaDelta(userID, int64(wallet)); err != nil {
			common.SysError(fmt.Sprintf("image wallet cache delta unavailable for user %d; retained conservative cache: %v", userID, err))
		}
	}
	if tokenQuota != 0 || tokenUsage != 0 {
		const script = `if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[1]) or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0 or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then return -1 end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', ARGV[2])
redis.call('HINCRBY', KEYS[1], 'UsedQuota', ARGV[3])
return 1`
		if err := common.RDB.Eval(context.Background(), script, []string{getTokenCacheKey(tokenKey)}, tokenID, tokenQuota, tokenUsage-tokenQuota).Err(); err != nil {
			common.SysError(fmt.Sprintf("image token cache delta unavailable for token %d; retained conservative cache: %v", tokenID, err))
		}
	}
}

func GetAsyncImageReservation(ctx context.Context, task AsyncImageTask) (*AsyncImageBill, error) {
	var bill AsyncImageBill
	err := DB.WithContext(ctx).Where("task_id = ? AND user_id = ? AND token_id = ? AND reserved_at > 0 AND status IN ?", task.TaskId, task.UserId, task.TokenId, []string{"reserved", "fixed"}).Take(&bill).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &bill, err
}

// ReserveAsyncImageQuota atomically binds the durable hold to the leased task.
// Retry and crash recovery reuse this bill before another upstream submission.
func ReserveAsyncImageQuota(ctx context.Context, task AsyncImageTask, amount int, funding ImageFundingSelection) error {
	if amount < 0 || amount > common.MaxQuota || task.TaskId == "" {
		return ErrImageInsufficientQuota
	}
	if err := prepareAsyncImageQuotaCaches(ctx, task.UserId, task.TokenId); err != nil {
		return err
	}
	var cached asyncImageQuotaCacheReservation
	commitAttempted := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current AsyncImageTask
		if err := lockForUpdate(tx).Where("task_id = ? AND status = ? AND lease_token = ? AND lease_expires_at > ?", task.TaskId, ImageTaskInvoking, task.LeaseToken, time.Now().Unix()).Take(&current).Error; err != nil {
			return err
		}
		if current.UserId != task.UserId || current.TokenId != task.TokenId {
			return ErrImageConflict
		}
		var bill AsyncImageBill
		// The task lock serializes all ledger writers. Locking an absent bill
		// would add a MySQL gap lock and deadlock concurrent tasks sharing a user.
		err := tx.Where("task_id = ?", task.TaskId).Take(&bill).Error
		exists := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if exists && (bill.Status != "reserved" || bill.UserId != task.UserId || bill.TokenId != task.TokenId || bill.FundingSource != funding.Source || bill.SubscriptionId != funding.SubscriptionId) {
			return ErrImageConflict
		}
		if !exists {
			bill = AsyncImageBill{TaskId: task.TaskId, BillingRequestId: "async-image:" + task.TaskId, UserId: task.UserId, TokenId: task.TokenId, ChannelId: task.ChannelId, FundingSource: funding.Source, SubscriptionId: funding.SubscriptionId, ReservedAt: time.Now().Unix(), CreatedAt: time.Now().Unix(), Status: "reserved", LogStatus: "pending"}
		}
		amount = max(amount, bill.ReservedQuota)
		var user User
		if err := lockForUpdate(tx).Where("id = ? AND status = ?", task.UserId, common.UserStatusEnabled).Take(&user).Error; err != nil {
			return err
		}
		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", task.TokenId, task.UserId).Take(&token).Error; err != nil {
			return err
		}
		walletDelta, tokenDelta := 0, 0
		fundingDelta := amount - bill.ReservedQuota
		if !token.UnlimitedQuota {
			tokenDelta = max(0, amount-bill.ReservedTokenQuota)
			if token.RemainQuota < tokenDelta || token.UsedQuota > math.MaxInt64-tokenDelta {
				return ErrImageInsufficientQuota
			}
		}
		switch funding.Source {
		case "wallet":
			walletDelta = fundingDelta
			if user.Quota < walletDelta {
				return ErrImageInsufficientQuota
			}
		case "subscription":
			var sub UserSubscription
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", funding.SubscriptionId, user.Id).Take(&sub).Error; err != nil {
				return err
			}
			plan, err := getSubscriptionPlanByIdTx(tx, sub.PlanId)
			if err != nil {
				return err
			}
			if err := maybeResetUserSubscriptionWithPlanTx(tx, &sub, plan, time.Now().Unix()); err != nil {
				return err
			}
			if !exists {
				bill.SubscriptionPeriodStart = common.GetPointer(sub.LastResetTime)
			}
			if fundingDelta > 0 {
				if sub.Status != "active" || sub.EndTime <= time.Now().Unix() || !SubscriptionReservationMatchesPeriod(&sub, bill.SubscriptionPeriodStart, bill.ReservedAt) || sub.AmountUsed > math.MaxInt64-int64(fundingDelta) || sub.AmountTotal > 0 && sub.AmountTotal-sub.AmountUsed < int64(fundingDelta) {
					return ErrImageInsufficientQuota
				}
				if err := tx.Model(&sub).Update("amount_used", sub.AmountUsed+int64(fundingDelta)).Error; err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid image funding source")
		}
		if err := cached.reserve(user.Id, token, walletDelta, tokenDelta); err != nil {
			return err
		}
		if walletDelta > 0 {
			if err := tx.Model(&user).Update("quota", gorm.Expr("quota - ?", walletDelta)).Error; err != nil {
				return err
			}
		}
		if tokenDelta > 0 {
			if err := tx.Model(&token).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota - ?", tokenDelta), "used_quota": gorm.Expr("used_quota + ?", tokenDelta)}).Error; err != nil {
				return err
			}
		}
		bill.ReservedQuota, bill.ReservedTokenQuota = amount, bill.ReservedTokenQuota+tokenDelta
		if exists {
			err = tx.Model(&bill).Updates(map[string]any{"reserved_quota": bill.ReservedQuota, "reserved_token_quota": bill.ReservedTokenQuota, "channel_id": task.ChannelId}).Error
		} else {
			err = tx.Create(&bill).Error
		}
		commitAttempted = err == nil
		return err
	})
	if err != nil && !commitAttempted {
		cached.compensate()
	}
	// A commit acknowledgement can be lost after the DB committed. Keep the
	// conservative cache hold in that case; never refund an uncertain commit.
	return err
}

// RefundAsyncImageReservation only releases holds for a confirmed terminal
// failure. Execution-unknown holds require an explicit administrator decision.
func RefundAsyncImageReservation(ctx context.Context, taskID string) error {
	var userID, tokenID, walletRefund, tokenRefund int
	var tokenKey string
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task AsyncImageTask
		if err := lockForUpdate(tx).Where("task_id = ?", taskID).Take(&task).Error; err != nil {
			return err
		}
		if task.Status != ImageTaskFailed && task.Status != ImageTaskExpired {
			return nil
		}
		var bill AsyncImageBill
		err := lockForUpdate(tx).Where("task_id = ?", taskID).Take(&bill).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if bill.ReservedAt == 0 || bill.Status == "refunded" || bill.Status == "applied" {
			return nil
		}
		if bill.Status != "reserved" && bill.Status != "fixed" {
			return ErrImageConflict
		}
		var user User
		if err := lockForUpdate(tx.Unscoped()).Where("id = ?", bill.UserId).Take(&user).Error; err != nil {
			return err
		}
		var token Token
		if err := lockForUpdate(tx.Unscoped()).Where("id = ? AND user_id = ?", bill.TokenId, bill.UserId).Take(&token).Error; err != nil {
			return err
		}
		if bill.FundingSource == "wallet" {
			if user.Quota > math.MaxInt64-bill.ReservedQuota {
				return errors.New("image wallet refund overflow")
			}
			walletRefund = bill.ReservedQuota
			if err := tx.Unscoped().Model(&user).Update("quota", gorm.Expr("quota + ?", walletRefund)).Error; err != nil {
				return err
			}
		} else if bill.FundingSource == "subscription" {
			var sub UserSubscription
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", bill.SubscriptionId, bill.UserId).Take(&sub).Error; err != nil {
				return err
			}
			plan, err := getSubscriptionPlanByIdTx(tx, sub.PlanId)
			if err != nil {
				return err
			}
			if err := maybeResetUserSubscriptionWithPlanTx(tx, &sub, plan, time.Now().Unix()); err != nil {
				return err
			}
			if SubscriptionReservationMatchesPeriod(&sub, bill.SubscriptionPeriodStart, bill.ReservedAt) {
				if err := tx.Model(&sub).Update("amount_used", max(0, sub.AmountUsed-int64(bill.ReservedQuota))).Error; err != nil {
					return err
				}
			}
		} else {
			return errors.New("invalid image funding source")
		}
		tokenRefund = bill.ReservedTokenQuota
		if token.RemainQuota > math.MaxInt64-tokenRefund || token.UsedQuota < tokenRefund {
			return errors.New("invalid image token reservation refund")
		}
		if err := tx.Unscoped().Model(&token).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota + ?", tokenRefund), "used_quota": gorm.Expr("used_quota - ?", tokenRefund)}).Error; err != nil {
			return err
		}
		userID, tokenID, tokenKey = user.Id, token.Id, token.Key
		if err := tx.Model(&bill).Updates(map[string]any{"status": "refunded", "log_status": "disabled"}).Error; err != nil {
			return err
		}
		if err := tx.Model(&task).Updates(map[string]any{"billing_status": "refunded", "reconciliation_status": "refunded"}).Error; err != nil {
			return err
		}
		return tx.Create(&AsyncImageEvent{TaskId: taskID, EventKey: common.GetUUID(), EventType: "reservation_refunded", Status: task.Status, Message: "Reserved image quota released", CreatedAt: time.Now().Unix()}).Error
	})
	if err == nil {
		syncAsyncImageQuotaCache(userID, tokenID, tokenKey, walletRefund, tokenRefund, 0)
	}
	return err
}
