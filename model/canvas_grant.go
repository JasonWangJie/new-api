package model

import (
	"context"
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// CanvasGrant keeps historical task ownership after ephemeral grants expire.
// No usable credentials, authorization codes or session secrets are persisted here.
type CanvasGrant struct {
	ID        string `json:"grant_id" gorm:"type:varchar(64);primaryKey"`
	TokenID   int    `json:"-" gorm:"not null;uniqueIndex"`
	UserID    int    `json:"-" gorm:"not null;index"`
	ClientID  string `json:"-" gorm:"type:varchar(64);not null;index"`
	Subject   string `json:"-" gorm:"type:varchar(64);not null;index"`
	CreatedAt int64  `json:"-" gorm:"type:bigint;not null"`
	ExpiresAt int64  `json:"-" gorm:"type:bigint;not null"`
	RevokedAt int64  `json:"-" gorm:"type:bigint;not null"`
}

func IsCanvasTokenKey(key string) bool {
	// MySQL collations may ignore case. A differently cased namespace must still
	// enter the managed guard, which compares the entire stored key exactly.
	return len(key) >= 3 && strings.EqualFold(key[:3], "ic_")
}

var ErrCanvasBillingOwner = errors.New("Infinite Canvas billing owner no longer exists")
var ErrCanvasWalletInsufficient = errors.New("Infinite Canvas wallet quota is insufficient")

func CanvasBillingSubject(tx *gorm.DB, tokenID, userID int) (string, error) {
	if tokenID <= 0 {
		return "", nil
	}
	var token Token
	if err := tx.Unscoped().First(&token, tokenID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}
	if !IsCanvasTokenKey(token.Key) {
		return "", nil
	}
	var grant CanvasGrant
	if err := tx.Where("token_id = ? AND user_id = ?", tokenID, userID).Take(&grant).Error; err != nil || grant.Subject == "" {
		return "", ErrCanvasBillingOwner
	}
	return grant.Subject, nil
}

func ValidateCanvasBillingOwner(tx *gorm.DB, token Token, user User) (string, error) {
	if !IsCanvasTokenKey(token.Key) {
		return "", nil
	}
	subject, err := CanvasBillingSubject(tx, token.Id, user.Id)
	if err != nil || user.CanvasSubject == nil || *user.CanvasSubject != subject {
		return "", ErrCanvasBillingOwner
	}
	return subject, nil
}

// LockCanvasBillingOwner is used only by managed funding transactions. Ordinary
// callers pass an empty subject and retain their existing lock/order policy.
func LockCanvasBillingOwner(tx *gorm.DB, userID int, subject string) error {
	if subject == "" {
		return nil
	}
	var user User
	if err := lockForUpdate(tx.Unscoped()).Where("id = ? AND canvas_subject = ?", userID, subject).Take(&user).Error; err != nil {
		return ErrCanvasBillingOwner
	}
	return nil
}

func RefreshCanvasSubjectCache(userID int, subject string) error {
	var user User
	if err := DB.First(&user, userID).Error; err != nil {
		return err
	}
	if user.CanvasSubject == nil || *user.CanvasSubject != subject {
		return ErrCanvasBillingOwner
	}
	if err := updateUserCache(user); err != nil {
		return err
	}
	if !common.BatchUpdateEnabled {
		_, err := GetUserCache(userID)
		return err
	}
	return nil
}

// A generation belongs to one existing Redis hash, never to the durable user.
// Rehydration intentionally does not restore it. Delayed refunds/compensation
// must not add quota to a new hash whose SQL snapshot already includes them.
func canvasQuotaCacheGeneration(userID int, subject string) (string, error) {
	if !common.RedisEnabled || subject == "" {
		return "", nil
	}
	const script = `
if tonumber(redis.call('HGET',KEYS[1],'Id') or '0') ~= tonumber(ARGV[1]) or redis.call('HGET',KEYS[1],'CanvasSubject') ~= ARGV[2] or tonumber(redis.call('HGET',KEYS[1],'CacheSchema') or '0') ~= tonumber(ARGV[3]) or redis.call('HEXISTS',KEYS[1],'Quota') == 0 then return '' end
redis.call('HSETNX',KEYS[1],'CanvasQuotaGeneration',ARGV[4])
return redis.call('HGET',KEYS[1],'CanvasQuotaGeneration')`
	return common.RDB.Eval(context.Background(), script, []string{getUserCacheKey(userID)}, userID, subject, userCacheSchemaVersion, common.GetUUID()).Text()
}

// AdjustCanvasWallet preserves the shared live balance while binding every SQL
// update and cache delta to the original account incarnation. It never batches
// subject-bound deltas into the legacy user-ID-only queue.
func AdjustCanvasWallet(userID int, subject string, delta int, requireAvailable bool) error {
	if subject == "" {
		return ErrCanvasBillingOwner
	}
	if delta == 0 {
		return nil
	}
	if delta > common.MaxWalletQuota || delta < -common.MaxWalletQuota {
		return ErrWalletQuotaLimitExceeded
	}
	if common.RedisEnabled {
		if err := RefreshCanvasSubjectCache(userID, subject); err != nil {
			return err
		}
	}
	generation, err := canvasQuotaCacheGeneration(userID, subject)
	if err != nil || common.RedisEnabled && generation == "" && delta < 0 {
		return ErrImageQuotaCacheUnavailable
	}
	reserved := 0
	bodySucceeded := false
	err = DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).Where("id = ? AND canvas_subject = ?", userID, subject).Take(&user).Error; err != nil {
			return ErrCanvasBillingOwner
		}
		if delta > 0 && user.Quota > common.MaxWalletQuota-delta || delta < 0 && user.Quota < -common.MaxWalletQuota-delta {
			return ErrWalletQuotaLimitExceeded
		}
		if delta < 0 && common.RedisEnabled {
			var result cacheQuotaResult
			var err error
			if requireAvailable {
				result, err = cacheTryReserveUserQuota(userID, int64(-delta), subject, generation)
			} else {
				result, err = cacheApplyUserQuotaDelta(userID, int64(delta), subject, generation)
			}
			if err != nil || result == cacheQuotaMiss {
				return ErrImageQuotaCacheUnavailable
			}
			if result != cacheQuotaOK {
				return ErrCanvasWalletInsufficient
			}
			reserved = -delta
		} else if delta < 0 && requireAvailable && user.Quota < -delta {
			return ErrCanvasWalletInsufficient
		}
		result := tx.Model(&User{}).Where("id = ? AND canvas_subject = ?", userID, subject).Update("quota", gorm.Expr("quota + ?", delta))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrCanvasBillingOwner
		}
		bodySucceeded = true
		return nil
	})
	if err != nil {
		if reserved > 0 && !bodySucceeded {
			_, _ = cacheApplyUserQuotaDelta(userID, int64(reserved), subject, generation)
		}
		return err
	}
	if delta > 0 && common.RedisEnabled {
		if _, err := cacheApplyUserQuotaDelta(userID, int64(delta), subject, generation); err != nil {
			common.SysError("Canvas wallet cache projection failed; retained conservative balance")
		}
	}
	return nil
}

func UpdateCanvasUsage(ctx context.Context, userID int, subject string, quota, count int) error {
	result := DB.WithContext(ctx).Model(&User{}).Where("id = ? AND canvas_subject = ?", userID, subject).Updates(map[string]any{"used_quota": gorm.Expr("used_quota + ?", quota), "request_count": gorm.Expr("request_count + ?", count)})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCanvasBillingOwner
	}
	return nil
}
