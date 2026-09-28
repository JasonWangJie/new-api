package model

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var errMeasuredTaskCASLost = errors.New("measured task terminal transition was already claimed")

// RecordMeasuredTaskSubmitAccounting atomically persists the submit-side usage
// counters and a task marker. A repeated call after an ambiguous response is a
// no-op, so neither request count nor usage can be applied twice.
func RecordMeasuredTaskSubmitAccounting(ctx context.Context, task *Task) (recorded bool, err error) {
	if task == nil || task.ID <= 0 || task.UserId <= 0 || task.ChannelId <= 0 || task.Quota < 0 {
		return false, errors.New("invalid measured task accounting inputs")
	}
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		if current.PrivateData.SubmitAccountingRecorded {
			return nil
		}
		if current.UserId != task.UserId || current.ChannelId != task.ChannelId || current.Quota != task.Quota {
			return errors.New("measured task accounting inputs do not match persisted task")
		}
		userResult := tx.Unscoped().Model(&User{}).Where("id = ?", task.UserId).Updates(map[string]any{
			"used_quota":    gorm.Expr("used_quota + ?", task.Quota),
			"request_count": gorm.Expr("request_count + 1"),
		})
		if userResult.Error != nil {
			return userResult.Error
		}
		if userResult.RowsAffected != 1 {
			return errors.New("measured task user accounting could not be applied")
		}
		channelResult := tx.Model(&Channel{}).Where("id = ?", task.ChannelId).
			Update("used_quota", gorm.Expr("used_quota + ?", task.Quota))
		if channelResult.Error != nil {
			return channelResult.Error
		}
		if channelResult.RowsAffected != 1 && task.Quota != 0 {
			var count int64
			if err := tx.Model(&Channel{}).Where("id = ?", task.ChannelId).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return errors.New("measured task channel accounting could not be applied")
			}
		}
		current.PrivateData.SubmitAccountingRecorded = true
		if err := tx.Model(&Task{}).Where("id = ?", task.ID).Update("private_data", current.PrivateData).Error; err != nil {
			return err
		}
		recorded = true
		return nil
	})
	if err == nil {
		task.PrivateData.SubmitAccountingRecorded = true
	}
	return recorded, err
}

// SettleMeasuredVideoTask commits a measured asynchronous task's terminal state
// and its refund as one primary-database transaction. The caller supplies the
// complete terminal task view, but its original Quota is the reserved amount.
// A losing poller observes won=false and must not write a second billing log.
func SettleMeasuredVideoTask(ctx context.Context, task *Task, fromStatus TaskStatus, actualQuota int, usageFacts map[string]any, matchedTier string) (won bool, reservedQuota int, err error) {
	if task == nil || task.ID <= 0 {
		return false, 0, errors.New("measured task is missing")
	}
	if fromStatus == TaskStatusSuccess || fromStatus == TaskStatusFailure ||
		(task.Status != TaskStatusSuccess && task.Status != TaskStatusFailure) {
		return false, 0, errors.New("measured task transition must be non-terminal to terminal")
	}
	reservedQuota = task.Quota
	if actualQuota < 0 || actualQuota > reservedQuota {
		return false, reservedQuota, fmt.Errorf("measured task quota %d is outside reserved amount %d", actualQuota, reservedQuota)
	}
	if task.Status == TaskStatusFailure && actualQuota != 0 {
		return false, reservedQuota, errors.New("failed measured task must refund its full reservation")
	}
	if task.PrivateData.BillingContext == nil || task.PrivateData.BillingContext.TieredSnapshot == nil {
		return false, reservedQuota, errors.New("measured task billing snapshot is missing")
	}
	if !task.PrivateData.SubmitAccountingRecorded {
		return false, reservedQuota, errors.New("measured task submission accounting is not yet recorded")
	}

	updated := *task
	updated.Quota = actualQuota
	privateData := updated.PrivateData
	billingContext := *privateData.BillingContext
	snapshot := *billingContext.TieredSnapshot
	if len(usageFacts) > 0 {
		facts := maps.Clone(snapshot.UsageFacts)
		if facts == nil {
			facts = make(map[string]any, len(usageFacts))
		}
		maps.Copy(facts, usageFacts)
		snapshot.UsageFacts = facts
	}
	if task.Status == TaskStatusSuccess && matchedTier != "" {
		snapshot.EstimatedTier = matchedTier
	}
	billingContext.TieredSnapshot = &snapshot
	privateData.BillingContext = &billingContext
	updated.PrivateData = privateData
	credit := reservedQuota - actualQuota
	var tokenKey string

	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Select("id", "status", "quota", "private_data").Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		if !current.PrivateData.SubmitAccountingRecorded {
			return errors.New("measured task submission accounting is not yet recorded")
		}
		// Claim first inside the transaction. Any later failure rolls the claim
		// back, leaving the non-terminal task available to the next poll.
		claim := tx.Model(&Task{}).
			Where("id = ? AND status = ? AND quota = ?", task.ID, fromStatus, reservedQuota).
			Select("*").Updates(&updated)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			return errMeasuredTaskCASLost
		}
		if credit == 0 {
			return nil
		}

		if privateData.BillingSource == "subscription" {
			if privateData.SubscriptionId <= 0 {
				return errors.New("measured task subscription is missing")
			}
			result := tx.Model(&UserSubscription{}).
				Where("id = ? AND user_id = ? AND amount_used >= ?", privateData.SubscriptionId, task.UserId, credit).
				Update("amount_used", gorm.Expr("amount_used - ?", credit))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("measured task subscription refund could not be applied")
			}
		}

		userUpdates := map[string]any{"used_quota": gorm.Expr("used_quota - ?", credit)}
		userQuery := tx.Unscoped().Model(&User{}).Where("id = ? AND used_quota >= ?", task.UserId, credit)
		if privateData.BillingSource != "subscription" {
			userUpdates["quota"] = gorm.Expr("quota + ?", credit)
			userQuery = userQuery.Where("quota <= ?", common.MaxWalletQuota-credit)
		}
		userResult := userQuery.Updates(userUpdates)
		if userResult.Error != nil {
			return userResult.Error
		}
		if userResult.RowsAffected != 1 {
			return errors.New("measured task user refund could not be applied")
		}

		if privateData.TokenId > 0 {
			var token Token
			if err := tx.Unscoped().Select("key").Where("id = ?", privateData.TokenId).First(&token).Error; err != nil {
				return err
			}
			tokenKey = token.Key
			tokenResult := tx.Unscoped().Model(&Token{}).
				Where("id = ? AND used_quota >= ? AND remain_quota <= ?", privateData.TokenId, credit, common.MaxWalletQuota-credit).
				Updates(map[string]any{
					"remain_quota":  gorm.Expr("remain_quota + ?", credit),
					"used_quota":    gorm.Expr("used_quota - ?", credit),
					"accessed_time": common.GetTimestamp(),
				})
			if tokenResult.Error != nil {
				return tokenResult.Error
			}
			if tokenResult.RowsAffected != 1 {
				return errors.New("measured task token refund could not be applied")
			}
		}

		channelResult := tx.Model(&Channel{}).Where("id = ? AND used_quota >= ?", task.ChannelId, credit).
			Update("used_quota", gorm.Expr("used_quota - ?", credit))
		if channelResult.Error != nil {
			return channelResult.Error
		}
		if channelResult.RowsAffected != 1 {
			var count int64
			if err := tx.Model(&Channel{}).Where("id = ?", task.ChannelId).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return errors.New("measured task channel accounting could not be applied")
			}
		}
		return nil
	})
	if errors.Is(err, errMeasuredTaskCASLost) {
		return false, reservedQuota, nil
	}
	if err != nil {
		return false, reservedQuota, err
	}
	*task = updated
	if credit > 0 {
		// Preserve other ordinary requests' unflushed batch deltas in the live
		// cache by applying only this committed refund.
		if privateData.BillingSource != "subscription" {
			syncDurableUserQuotaCache(task.UserId, credit)
		}
		if tokenKey != "" {
			syncDurableTokenQuotaCache(privateData.TokenId, tokenKey, credit)
		}
	}
	return true, reservedQuota, nil
}
