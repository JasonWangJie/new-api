package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

type cacheQuotaResult int

const (
	cacheQuotaInsufficient cacheQuotaResult = iota
	cacheQuotaOK
	cacheQuotaMiss
)

const userQuotaReserveScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') ~= tonumber(ARGV[3])
  or redis.call('HEXISTS', KEYS[1], 'Quota') == 0 then
  return -1
end
local quota = tonumber(redis.call('HGET', KEYS[1], 'Quota'))
if quota == nil or quota < tonumber(ARGV[1]) then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'Quota', -tonumber(ARGV[1]))
return 1`

const userQuotaDeltaScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') ~= tonumber(ARGV[3])
  or redis.call('HEXISTS', KEYS[1], 'Quota') == 0 then
  return -1
end
redis.call('HINCRBY', KEYS[1], 'Quota', ARGV[1])
return 1`

const tokenQuotaReserveScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0
  or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then
  return -1
end
local remain = tonumber(redis.call('HGET', KEYS[1], 'RemainQuota'))
if remain == nil or remain < tonumber(ARGV[1]) then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', -tonumber(ARGV[1]))
redis.call('HINCRBY', KEYS[1], 'UsedQuota', tonumber(ARGV[1]))
redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[3])
return 1`

const tokenQuotaDeltaScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0
  or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then
  return -1
end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', tonumber(ARGV[1]))
redis.call('HINCRBY', KEYS[1], 'UsedQuota', -tonumber(ARGV[1]))
redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[3])
return 1`

func quotaResultFromLua(result int, err error) (cacheQuotaResult, error) {
	if err != nil {
		return cacheQuotaMiss, err
	}
	switch result {
	case 1:
		return cacheQuotaOK, nil
	case 0:
		return cacheQuotaInsufficient, nil
	default:
		return cacheQuotaMiss, nil
	}
}

func cacheTryReserveUserQuota(userID int, amount int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), userQuotaReserveScript,
		[]string{getUserCacheKey(userID)}, amount, userID, userCacheSchemaVersion).Int()
	return quotaResultFromLua(result, err)
}

func cacheApplyUserQuotaDelta(userID int, delta int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), userQuotaDeltaScript,
		[]string{getUserCacheKey(userID)}, delta, userID, userCacheSchemaVersion).Int()
	return quotaResultFromLua(result, err)
}

func cacheTryReserveTokenQuota(id int, key string, amount int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), tokenQuotaReserveScript,
		[]string{getTokenCacheKey(key)}, amount, id, common.GetTimestamp()).Int()
	return quotaResultFromLua(result, err)
}

func cacheApplyTokenQuotaDelta(id int, key string, delta int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), tokenQuotaDeltaScript,
		[]string{getTokenCacheKey(key)}, delta, id, common.GetTimestamp()).Int()
	return quotaResultFromLua(result, err)
}

// persistUserQuotaDelta 把已在缓存侧预扣成功的增量落库；批量模式下入队，
// 直写模式下要求行存在（用户已删除时报错，交由调用方补偿缓存）。
func persistUserQuotaDeltaWithMode(id int, delta int, durable bool) error {
	if common.BatchUpdateEnabled && !durable {
		addNewRecord(BatchUpdateTypeUserQuota, id, delta)
		return nil
	}
	result := DB.Model(&User{}).Where("id = ?", id).Update("quota", gorm.Expr("quota + ?", delta))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func persistTokenQuotaDeltaWithMode(id int, delta int, durable bool) error {
	if common.BatchUpdateEnabled && !durable {
		addNewRecord(BatchUpdateTypeTokenQuota, id, delta)
		return nil
	}
	result := DB.Model(&Token{}).Where("id = ?", id).Updates(
		map[string]any{
			"remain_quota":  gorm.Expr("remain_quota + ?", delta),
			"used_quota":    gorm.Expr("used_quota - ?", delta),
			"accessed_time": common.GetTimestamp(),
		},
	)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func reserveUserQuotaDB(id int, quota int) (bool, error) {
	result := DB.Model(&User{}).
		Where("id = ? AND quota >= ?", id, quota).
		Update("quota", gorm.Expr("quota - ?", quota))
	return result.RowsAffected == 1, result.Error
}

func reserveTokenQuotaDB(id int, quota int) (bool, error) {
	result := DB.Model(&Token{}).
		Where("id = ? AND remain_quota >= ?", id, quota).
		Updates(map[string]any{
			"remain_quota":  gorm.Expr("remain_quota - ?", quota),
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"accessed_time": common.GetTimestamp(),
		})
	return result.RowsAffected == 1, result.Error
}

// TryReserveUserQuota atomically checks and deducts a user's wallet quota.
// 缓存命中时以缓存余额为准（避免批量模式下过期的数据库余额放大并发超扣）；
// Redis 异常或水合失败时降级为数据库条件更新，保证服务可用。
func TryReserveUserQuota(id int, quota int) (bool, error) {
	return tryReserveUserQuota(id, quota, false)
}

// TryReserveUserQuotaDurable persists a task reservation immediately even when
// ordinary quota updates are batched. Async tasks can outlive the current
// process, so their reservation must survive a restart before dispatch.
func TryReserveUserQuotaDurable(id int, quota int) (bool, error) {
	return tryReserveUserQuota(id, quota, true)
}

func tryReserveUserQuota(id int, quota int, durable bool) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota 不能为负数！")
	}
	if quota == 0 {
		return true, nil
	}
	if !common.RedisEnabled {
		return reserveUserQuotaDB(id, quota)
	}

	result, err := cacheTryReserveUserQuota(id, int64(quota))
	if err == nil && result == cacheQuotaMiss {
		// A cold cache cannot prove the database includes batches currently
		// flushing or owned by another node. Do not hydrate a spendable balance.
		if durable && common.BatchUpdateEnabled {
			return false, errors.New("durable task user quota cache is cold during batch updates")
		}
		if _, hydrateErr := GetUserCache(id); hydrateErr == nil {
			result, err = cacheTryReserveUserQuota(id, int64(quota))
		}
	}
	if err != nil || result == cacheQuotaMiss {
		if durable && common.BatchUpdateEnabled {
			return false, errors.New("durable task user quota cache is unavailable during batch updates")
		}
		if err != nil {
			common.SysLog("user quota cache reserve unavailable, falling back to database: " + err.Error())
		}
		reserved, reserveErr := reserveUserQuotaDB(id, quota)
		if reserved && durable {
			syncDurableUserQuotaCache(id, -quota)
		}
		return reserved, reserveErr
	}
	if result == cacheQuotaInsufficient {
		return false, nil
	}
	if err = persistUserQuotaDeltaWithMode(id, -quota, durable); err != nil {
		compensated, compensateErr := cacheApplyUserQuotaDelta(id, int64(quota))
		if compensateErr != nil || compensated != cacheQuotaOK {
			common.SysError(fmt.Sprintf("failed to compensate reserved user quota: result=%d error=%v", compensated, compensateErr))
		}
		return false, err
	}
	return true, nil
}

// TryReserveTokenQuota atomically checks and deducts a token quota. Unlimited
// tokens skip the balance check but still update remain/used accounting.
func TryReserveTokenQuota(id int, key string, quota int, unlimited bool) (bool, error) {
	return tryReserveTokenQuota(id, key, quota, unlimited, false)
}

// TryReserveTokenQuotaDurable is the token counterpart of the durable wallet
// reservation used by asynchronous task submission.
func TryReserveTokenQuotaDurable(id int, key string, quota int, unlimited bool) (bool, error) {
	return tryReserveTokenQuota(id, key, quota, unlimited, true)
}

func tryReserveTokenQuota(id int, key string, quota int, unlimited, durable bool) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota 不能为负数！")
	}
	if quota == 0 {
		return true, nil
	}
	if unlimited {
		if durable {
			return true, AdjustTokenQuotaDurable(id, key, -quota)
		}
		return true, DecreaseTokenQuota(id, key, quota)
	}
	if !common.RedisEnabled {
		return reserveTokenQuotaDB(id, quota)
	}

	result, err := cacheTryReserveTokenQuota(id, key, int64(quota))
	if err == nil && result == cacheQuotaMiss {
		if durable && common.BatchUpdateEnabled {
			return false, errors.New("durable task token quota cache is cold during batch updates")
		}
		if _, hydrateErr := GetTokenByKey(key, true); hydrateErr == nil {
			result, err = cacheTryReserveTokenQuota(id, key, int64(quota))
		}
	}
	if err != nil || result == cacheQuotaMiss {
		if durable && common.BatchUpdateEnabled {
			return false, errors.New("durable task token quota cache is unavailable during batch updates")
		}
		if err != nil {
			common.SysLog("token quota cache reserve unavailable, falling back to database: " + err.Error())
		}
		reserved, reserveErr := reserveTokenQuotaDB(id, quota)
		if reserved && durable {
			syncDurableTokenQuotaCache(id, key, -quota)
		}
		return reserved, reserveErr
	}
	if result == cacheQuotaInsufficient {
		return false, nil
	}
	if err = persistTokenQuotaDeltaWithMode(id, -quota, durable); err != nil {
		compensated, compensateErr := cacheApplyTokenQuotaDelta(id, key, int64(quota))
		if compensateErr != nil || compensated != cacheQuotaOK {
			common.SysError(fmt.Sprintf("failed to compensate reserved token quota: result=%d error=%v", compensated, compensateErr))
		}
		return false, err
	}
	return true, nil
}

// AdjustTokenQuotaDurable applies a signed token balance change directly to the
// database. Positive delta refunds quota; negative delta consumes it. It is
// used when rolling back or settling a durable asynchronous reservation.
func AdjustTokenQuotaDurable(id int, key string, delta int) error {
	if delta == 0 {
		return nil
	}
	var err error
	if delta > 0 {
		err = increaseTokenQuota(id, delta)
	} else {
		err = decreaseTokenQuota(id, -delta)
	}
	if err != nil {
		return err
	}
	syncDurableTokenQuotaCache(id, key, delta)
	return nil
}

// AdjustUserQuotaDurable applies a signed wallet change directly to the
// database and syncs the live cache after commit. Async task rollbacks must
// not remain only in a process-local batch queue.
func AdjustUserQuotaDurable(id int, delta int) error {
	if delta == 0 {
		return nil
	}
	var err error
	if delta > 0 {
		err = increaseUserQuota(id, delta)
	} else {
		err = decreaseUserQuota(id, -delta)
	}
	if err != nil {
		return err
	}
	syncDurableUserQuotaCache(id, delta)
	return nil
}

// Applying the committed delta to a live cache preserves other quota changes
// that still reside in the ordinary batch queue. Deleting that cache would
// rehydrate from a database that may not yet include those changes.
func syncDurableUserQuotaCache(id, delta int) {
	if !common.RedisEnabled || delta == 0 {
		return
	}
	result, err := cacheApplyUserQuotaDelta(id, int64(delta))
	if err != nil {
		common.SysError("durable task user quota cache sync failed: " + err.Error())
		if invalidateErr := invalidateUserCache(id); invalidateErr != nil {
			common.SysError("durable task user quota cache invalidation failed: " + invalidateErr.Error())
		}
		return
	}
	if result == cacheQuotaMiss && common.BatchUpdateEnabled {
		common.SysError("durable task user quota cache missing while batch updates may be pending")
	}
}

func syncDurableTokenQuotaCache(id int, key string, delta int) {
	if !common.RedisEnabled || key == "" || delta == 0 {
		return
	}
	result, err := cacheApplyTokenQuotaDelta(id, key, int64(delta))
	if err != nil {
		common.SysError("durable task token quota cache sync failed: " + err.Error())
		if invalidateErr := invalidateTokenCacheForMutation(key); invalidateErr != nil {
			common.SysError("durable task token quota cache invalidation failed: " + invalidateErr.Error())
		}
		return
	}
	if result == cacheQuotaMiss && common.BatchUpdateEnabled {
		common.SysError("durable task token quota cache missing while batch updates may be pending")
	}
}
