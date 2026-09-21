package service

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

// Local gateway URLs are resolved through their persisted SC identity only.
// They are never downloaded over HTTP or passed to an upstream as remote input.
func IsGatewayImageReference(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	base, err := url.Parse(strings.TrimSuffix(strings.TrimRight(system_setting.ServerAddress, "/"), "/v1"))
	if err != nil || parsed.Scheme != base.Scheme || parsed.Host != base.Host {
		return false
	}
	prefix := strings.TrimRight(base.Path, "/") + "/api/image-objects/"
	object, valid := strings.CutPrefix(parsed.Path, prefix)
	identity, validPath := strings.CutSuffix(object, "/content")
	return valid && validPath && strings.HasPrefix(identity, "imo_") && !strings.Contains(identity, "/")
}

// IsBoundImageReference prevents known SC aliases from bypassing ownership or
// tombstones when the selected transport mode would otherwise pass a URL on.
func IsBoundImageReference(ctx context.Context, tokenId int, raw string) (bool, error) {
	var alias model.ImageInputAlias
	err := model.DB.WithContext(ctx).Where("url_hash = ?", ImageIdentityHash(raw)).Take(&alias).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if IsGatewayImageReference(raw) {
			return true, newImageValidationError("SC input alias is unavailable")
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if alias.TokenId != tokenId {
		return true, newImageValidationError("SC input belongs to another Token")
	}
	var count int64
	err = model.DB.WithContext(ctx).Model(&model.ImageInputObject{}).Where("input_id = ? AND token_id = ? AND status = ? AND expires_at > ?", alias.InputId, tokenId, "active", time.Now().Unix()).Count(&count).Error
	if err != nil {
		return true, err
	}
	if count != 1 {
		return true, newImageValidationError("SC input has expired or is unavailable")
	}
	return true, nil
}

func ResolveImageReference(ctx context.Context, tokenId int, raw string, cfg ImageRuntimeConfig) (ImageBytes, bool, error) {
	var alias model.ImageInputAlias
	err := model.DB.WithContext(ctx).Where("url_hash = ?", ImageIdentityHash(raw)).Take(&alias).Error
	if err == nil {
		if alias.TokenId != tokenId {
			return ImageBytes{}, true, newImageValidationError("SC input belongs to another Token")
		}
		var input model.ImageInputObject
		if err := model.DB.WithContext(ctx).Where("input_id = ? AND token_id = ? AND status = ? AND expires_at > ?", alias.InputId, tokenId, "active", time.Now().Unix()).Take(&input).Error; err != nil {
			return ImageBytes{}, true, newImageValidationError("SC input has expired or is unavailable")
		}
		var object model.ImageStorageObject
		if err := model.DB.WithContext(ctx).Where("object_id = ? AND status = ?", input.ObjectId, "active").Take(&object).Error; err != nil {
			return ImageBytes{}, true, err
		}
		store, err := GetImageObjectStorage(ctx, object)
		if err != nil {
			return ImageBytes{}, true, err
		}
		data, err := store.Read(ctx, object.ObjectKey, cfg.DownloadMaxBytes)
		if err != nil {
			return ImageBytes{}, true, err
		}
		image, err := ValidateImageBytes(data, object.ContentType, cfg.DownloadMaxBytes, cfg.DownloadMaxPixels)
		if err == nil && (image.Checksum != object.Checksum || int64(len(data)) != input.ByteSize) {
			return ImageBytes{}, true, model.ErrImageConflict
		}
		return image, true, err
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return ImageBytes{}, false, err
	}
	if IsGatewayImageReference(raw) {
		return ImageBytes{}, true, newImageValidationError("SC input alias is unavailable")
	}
	image, err := DownloadImageReferenceCached(ctx, tokenId, raw, cfg)
	return image, false, err
}

func ResolveImageReferenceTransportMode(configured string, referenceRetryCount int) string {
	if configured == "passthrough_fallback_local" && referenceRetryCount > 0 {
		return "local"
	}
	return configured
}

var imageReferenceCacheWrite = redis.NewScript(`local expired=redis.call('ZRANGEBYSCORE',KEYS[2],'-inf',ARGV[1],'LIMIT',0,100); local used=tonumber(redis.call('GET',KEYS[4]) or '0'); for _,id in ipairs(expired) do used=used-tonumber(redis.call('HGET',KEYS[3],id) or '0'); redis.call('HDEL',KEYS[1],id); redis.call('HDEL',KEYS[3],id); redis.call('ZREM',KEYS[2],id); end; used=math.max(0,used); local old=tonumber(redis.call('HGET',KEYS[3],ARGV[2]) or '0'); local size=tonumber(ARGV[4]); if used-old+size>tonumber(ARGV[5]) then redis.call('SET',KEYS[4],used,'EX',ARGV[7]);return 0 end; redis.call('HSET',KEYS[1],ARGV[2],ARGV[6]); redis.call('HSET',KEYS[3],ARGV[2],size);redis.call('ZADD',KEYS[2],ARGV[3],ARGV[2]);redis.call('SET',KEYS[4],used-old+size,'EX',ARGV[7]);for i=1,3 do redis.call('EXPIRE',KEYS[i],ARGV[7]);end;return 1`)

// DownloadImageReferenceCached keeps private reference bytes encrypted and
// scoped to the submitting Token. Redis enforces the shared byte and download
// limits; there is no process-local cache or concurrency fallback.
func DownloadImageReferenceCached(ctx context.Context, tokenId int, raw string, cfg ImageRuntimeConfig) (ImageBytes, error) {
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		return DownloadImageReference(ctx, raw, cfg)
	}
	if _, err := ValidateImageReferenceURL(raw); err != nil {
		return ImageBytes{}, newImageValidationError(err.Error())
	}
	if common.RDB == nil {
		return ImageBytes{}, errors.New("Redis is required for reference download admission")
	}
	identity := ImageIdentityHash(strconv.Itoa(tokenId), raw)
	cacheKeys := []string{ImageRedisKey("reference-cache", "data"), ImageRedisKey("reference-cache", "expires"), ImageRedisKey("reference-cache", "sizes"), ImageRedisKey("reference-cache", "bytes")}
	if cfg.ReferenceCacheTTL > 0 && cfg.ReferenceCacheBytes > 0 {
		score, err := common.RDB.ZScore(ctx, cacheKeys[1], identity).Result()
		if err == nil && score > float64(time.Now().Unix()) {
			cached, err := common.RDB.HGet(ctx, cacheKeys[0], identity).Bytes()
			if err == nil && int64(len(cached)) < cfg.DownloadMaxBytes*2+1024 {
				plain, err := DecryptImagePayload(cached, "reference:"+identity)
				if err == nil {
					var image ImageBytes
					if common.Unmarshal(plain, &image) == nil {
						verified, err := ValidateImageBytes(image.Data, image.ContentType, cfg.DownloadMaxBytes, cfg.DownloadMaxPixels)
						if err == nil && verified.Checksum == image.Checksum {
							return verified, nil
						}
					}
				}
			}
		}
	}
	lease := common.GetUUID()
	key := ImageRedisKey("reference-download-slots")
	now := time.Now().Unix()
	acquired, err := imageInvocationAcquire.Run(ctx, common.RDB, []string{key}, now, now+int64(cfg.DownloadTimeout+10), cfg.ReferenceConcurrency, lease, cfg.DownloadTimeout+10).Int()
	if err != nil {
		return ImageBytes{}, err
	}
	if acquired != 1 {
		return ImageBytes{}, &AsyncImageFailure{Code: 603, InternalCode: "reference_capacity_wait", Message: "Reference download capacity is busy"}
	}
	defer common.RDB.ZRem(context.WithoutCancel(ctx), key, lease)
	image, err := DownloadImageReference(ctx, raw, cfg)
	if err != nil {
		return image, err
	}
	if cfg.ReferenceCacheTTL > 0 && cfg.ReferenceCacheBytes > 0 {
		plain, err := common.Marshal(image)
		if err == nil {
			cipher, err := EncryptImagePayload(plain, "reference:"+identity)
			if err == nil && int64(len(cipher)) <= cfg.ReferenceCacheBytes {
				_ = imageReferenceCacheWrite.Run(ctx, common.RDB, cacheKeys, time.Now().Unix(), identity, time.Now().Unix()+int64(cfg.ReferenceCacheTTL), len(cipher), cfg.ReferenceCacheBytes, cipher, cfg.ReferenceCacheTTL).Err()
			}
		}
	}
	return image, nil
}

func AsyncImageInputReferences(ctx context.Context, tokenId int, request AsyncImageRequest) ([]string, error) {
	ids := make([]string, 0)
	for _, part := range request.Parts {
		if part.Type != "image_url" && part.Type != "mask" {
			continue
		}
		var alias model.ImageInputAlias
		err := model.DB.WithContext(ctx).Where("url_hash = ?", ImageIdentityHash(part.URL)).Take(&alias).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if IsGatewayImageReference(part.URL) {
				return nil, newImageValidationError("SC input alias is unavailable")
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if alias.TokenId != tokenId {
			return nil, newImageValidationError("SC input belongs to another Token")
		}
		var input model.ImageInputObject
		if err := model.DB.WithContext(ctx).Where("input_id = ? AND token_id = ? AND status = ? AND expires_at > ?", alias.InputId, tokenId, "active", time.Now().Unix()).Take(&input).Error; err != nil {
			return nil, newImageValidationError("SC input has expired or is unavailable")
		}
		ids = append(ids, input.InputId)
	}
	return ids, nil
}
