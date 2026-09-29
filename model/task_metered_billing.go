package model

import (
	"context"
	"errors"
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

// SettleTaskBilling commits a terminal task and its balance changes together.
// An unaffordable completion becomes a failure with a full reservation refund;
// it never publishes a successful result that has not been paid for.
func SettleTaskBilling(ctx context.Context, task *Task, fromStatus TaskStatus, actualQuota int, usageFacts map[string]any, matchedTier string) (won bool, reservedQuota int, err error) {
	if task == nil || task.ID <= 0 || task.Quota < 0 || actualQuota < 0 {
		return false, 0, errors.New("invalid task settlement inputs")
	}
	if fromStatus == TaskStatusSuccess || fromStatus == TaskStatusFailure ||
		(task.Status != TaskStatusSuccess && task.Status != TaskStatusFailure) {
		return false, 0, errors.New("task settlement must transition from non-terminal to terminal")
	}
	reservedQuota = task.Quota
	if task.Status == TaskStatusFailure && actualQuota != 0 {
		return false, reservedQuota, errors.New("failed task must refund its full reservation")
	}
	updated := *task
	updated.Quota = actualQuota
	if billingContext := task.PrivateData.BillingContext; billingContext != nil {
		copied := *billingContext
		if billingContext.TieredSnapshot != nil {
			snapshot := *billingContext.TieredSnapshot
			snapshot.UsageFacts = maps.Clone(snapshot.UsageFacts)
			if len(usageFacts) > 0 {
				if snapshot.UsageFacts == nil {
					snapshot.UsageFacts = make(map[string]any)
				}
				maps.Copy(snapshot.UsageFacts, usageFacts)
			}
			if task.Status == TaskStatusSuccess && matchedTier != "" {
				snapshot.EstimatedTier = matchedTier
			}
			copied.TieredSnapshot = &snapshot
		}
		updated.PrivateData.BillingContext = &copied
	}
	var tokenKey string
	// Cache reservations are made while holding the task row lock. Track them
	// separately so a database rollback or token rejection compensates exactly
	// the cache changes made by this attempt.
	userCacheReserved, tokenCacheReserved := 0, 0
	delta := actualQuota - reservedQuota
	transactionBodySucceeded := false
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := lockForUpdate(tx).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		if current.Status != fromStatus || current.Quota != reservedQuota {
			return errMeasuredTaskCASLost
		}
		if current.PrivateData.DurableBilling && !current.PrivateData.SubmitAccountingRecorded {
			return errors.New("task submission accounting is not yet recorded")
		}
		if current.UserId != task.UserId || current.ChannelId != task.ChannelId {
			return errors.New("task settlement identity mismatch")
		}
		privateData := current.PrivateData
		var subscription UserSubscription
		samePeriod := true
		if privateData.BillingSource == "subscription" {
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", privateData.SubscriptionId, task.UserId).Take(&subscription).Error; err != nil {
				return err
			}
			samePeriod = SubscriptionReservationMatchesPeriod(&subscription, privateData.SubscriptionPeriodStart, current.SubmitTime)
		}
		var user User
		if err := lockForUpdate(tx.Unscoped()).Where("id = ?", task.UserId).Take(&user).Error; err != nil {
			return err
		}
		var token Token
		if privateData.TokenId > 0 {
			if err := lockForUpdate(tx.Unscoped()).Where("id = ?", privateData.TokenId).Take(&token).Error; err != nil {
				return err
			}
			tokenKey = token.Key
		}
		affordable := true
		if delta > 0 {
			if privateData.BillingSource == "subscription" {
				affordable = samePeriod && subscription.Status == "active" && subscription.EndTime > common.GetTimestamp() &&
					(subscription.AmountTotal <= 0 || int64(delta) <= subscription.AmountTotal-subscription.AmountUsed)
			} else if user.Quota < delta {
				affordable = false
			} else if common.RedisEnabled {
				result, cacheErr := cacheTryReserveUserQuota(task.UserId, int64(delta))
				if cacheErr != nil || result == cacheQuotaMiss {
					if common.BatchUpdateEnabled {
						return errors.New("task settlement wallet cache unavailable during batch updates")
					}
					affordable = user.Quota >= delta
				} else {
					affordable = result == cacheQuotaOK
					if affordable {
						userCacheReserved = delta
					}
				}
			} else {
				affordable = user.Quota >= delta
			}
			if affordable && privateData.TokenId > 0 && !token.UnlimitedQuota {
				if token.RemainQuota < delta {
					affordable = false
				} else if common.RedisEnabled {
					result, cacheErr := cacheTryReserveTokenQuota(token.Id, token.Key, int64(delta))
					if cacheErr != nil || result == cacheQuotaMiss {
						if common.BatchUpdateEnabled {
							return errors.New("task settlement token cache unavailable during batch updates")
						}
						affordable = token.RemainQuota >= delta
					} else {
						affordable = result == cacheQuotaOK
						if affordable {
							tokenCacheReserved = delta
						}
					}
				} else {
					affordable = token.RemainQuota >= delta
				}
			}
		}
		if !affordable {
			updated.Status = TaskStatusFailure
			updated.Progress = "100%"
			updated.FailReason = "insufficient quota to settle completed task"
			updated.Quota = 0
			updated.Data = nil
			updated.PrivateData.ResultURL = ""
			updated.PrivateData.PluginState = nil
			updated.PrivateData.UpstreamAsyncResponse = nil
			delta = -reservedQuota
		}
		if delta != 0 {
			credit := -delta
			if privateData.BillingSource == "subscription" && samePeriod {
				// A reset expires the old period's reservation. Refund only within
				// that period, never against usage accrued after a reset.
				used := max(subscription.AmountUsed+int64(delta), 0)
				if err := tx.Model(&UserSubscription{}).Where("id = ?", subscription.Id).Update("amount_used", used).Error; err != nil {
					return err
				}
			}
			userUpdates := map[string]any{"used_quota": gorm.Expr("used_quota + ?", delta)}
			userQuery := tx.Unscoped().Model(&User{}).Where("id = ?", task.UserId)
			if current.PrivateData.SubmitAccountingRecorded && credit > 0 {
				userQuery = userQuery.Where("used_quota >= ?", credit)
			}
			if privateData.BillingSource != "subscription" {
				userUpdates["quota"] = gorm.Expr("quota - ?", delta)
				if credit > 0 {
					userQuery = userQuery.Where("quota <= ?", common.MaxWalletQuota-credit)
				}
			}
			result := userQuery.Updates(userUpdates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("task user refund or charge could not be applied")
			}
			if privateData.TokenId > 0 {
				query := tx.Unscoped().Model(&Token{}).Where("id = ?", privateData.TokenId)
				if credit > 0 {
					query = query.Where("remain_quota <= ?", common.MaxWalletQuota-credit)
					if current.PrivateData.SubmitAccountingRecorded {
						query = query.Where("used_quota >= ?", credit)
					}
				}
				result := query.Updates(map[string]any{
					"remain_quota":  gorm.Expr("remain_quota - ?", delta),
					"used_quota":    gorm.Expr("used_quota + ?", delta),
					"accessed_time": common.GetTimestamp(),
				})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return errors.New("task token refund could not be applied")
				}
			}
			query := tx.Model(&Channel{}).Where("id = ?", task.ChannelId)
			if current.PrivateData.SubmitAccountingRecorded && credit > 0 {
				query = query.Where("used_quota >= ?", credit)
			}
			result = query.Update("used_quota", gorm.Expr("used_quota + ?", delta))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				var count int64
				if err := tx.Model(&Channel{}).Where("id = ?", task.ChannelId).Count(&count).Error; err != nil {
					return err
				}
				if count != 0 {
					return errors.New("task channel accounting could not be applied")
				}
			}
		}
		claim := tx.Model(&Task{}).Where("id = ? AND status = ? AND quota = ?", task.ID, fromStatus, reservedQuota).Select("*").Updates(&updated)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			return errMeasuredTaskCASLost
		}
		transactionBodySucceeded = true
		return nil
	})
	if err != nil && transactionBodySucceeded {
		// Commit may have succeeded before its acknowledgement was lost. Keep
		// the cache hold until the durable task state can be observed again;
		// adding it back here could make already spent quota available twice.
		return false, reservedQuota, err
	}
	cacheDelta := 0
	if err == nil {
		cacheDelta = -delta
		*task = updated
	}
	if task.PrivateData.BillingSource != "subscription" {
		syncDurableUserQuotaCache(task.UserId, cacheDelta+userCacheReserved)
	}
	if tokenKey != "" {
		syncDurableTokenQuotaCache(task.PrivateData.TokenId, tokenKey, cacheDelta+tokenCacheReserved)
	}
	if errors.Is(err, errMeasuredTaskCASLost) {
		return false, reservedQuota, nil
	}
	return err == nil, reservedQuota, err
}
