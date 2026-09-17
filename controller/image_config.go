package controller

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func imageManagementError(c *gin.Context, status int, err error) {
	c.JSON(status, gin.H{"success": false, "message": err.Error()})
}
func imageManagementData(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": data})
}

func GetImageConfiguration(c *gin.Context) {
	ctx := c.Request.Context()
	cfg, err := service.GetImageRuntimeConfig(ctx)
	if err != nil {
		imageManagementError(c, 503, err)
		return
	}
	var profiles []model.ImageStorageProfile
	var policies []model.ImageGroupPolicy
	var pools []model.ImageChannelPool
	if err := model.DB.WithContext(ctx).Order("id DESC").Find(&profiles).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	if err := model.DB.WithContext(ctx).Find(&policies).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	if err := model.DB.WithContext(ctx).Find(&pools).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	imageManagementData(c, gin.H{"runtime": cfg, "storage_profiles": profiles, "policies": policies, "pools": pools, "storage_providers": []string{"local", "aws", "aliyun", "tencent", "qiniu", "r2", "custom_s3"}})
}

func UpdateImageRuntime(c *gin.Context) {
	var cfg service.ImageRuntimeConfig
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &cfg); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	if err := service.SaveImageRuntimeConfig(c.Request.Context(), cfg); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	imageManagementData(c, cfg)
}

func SaveImagePolicy(c *gin.Context) {
	var policy model.ImageGroupPolicy
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &policy); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	if policy.Group == "" || len(policy.Group) > 64 || policy.Platform != "openai" && policy.Platform != "gemini" || policy.PoolMode != "resolution" && policy.PoolMode != "model" && policy.PoolMode != "model_resolution" {
		imageManagementError(c, 400, errors.New("Invalid image platform group or pool mode"))
		return
	}
	var catalog []service.ImageModelCapability
	if err := common.UnmarshalJsonStr(policy.Models, &catalog); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	seen := make(map[string]bool)
	for i := range catalog {
		catalog[i].Id = strings.TrimSpace(catalog[i].Id)
		if err := service.ValidateImageModelName(catalog[i].Id); err != nil {
			imageManagementError(c, 400, err)
			return
		}
		if seen[catalog[i].Id] || catalog[i].MaxOutputImages < 1 || catalog[i].MaxOutputImages > dto.MaxImageN || catalog[i].MaxReferenceImages < 0 || catalog[i].MaxReferenceImages > dto.MaxImageN {
			imageManagementError(c, 400, errors.New("Invalid or duplicate image model capability"))
			return
		}
		seen[catalog[i].Id] = true
	}
	encoded, err := common.Marshal(catalog)
	if err != nil {
		imageManagementError(c, 400, err)
		return
	}
	policy.Id = 0
	policy.Models = string(encoded)
	policy.PolicyKey = service.ImageIdentityHash(policy.Group, policy.Platform)
	err = model.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var existing model.ImageGroupPolicy
		err := tx.Where("policy_key = ?", policy.PolicyKey).Take(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			policy.Version = 1
			return tx.Create(&policy).Error
		}
		if err != nil {
			return err
		}
		if existing.Version != policy.Version {
			return model.ErrImageConflict
		}
		policy.Version++
		result := tx.Model(&model.ImageGroupPolicy{}).Where("id = ? AND version = ?", existing.Id, existing.Version).Updates(map[string]any{"enabled": policy.Enabled, "async_enabled": policy.AsyncEnabled, "pool_mode": policy.PoolMode, "models": policy.Models, "version": policy.Version})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return model.ErrImageConflict
		}
		policy.Id = existing.Id
		return nil
	})
	if err != nil {
		status := 400
		if errors.Is(err, model.ErrImageConflict) {
			status = 409
		}
		imageManagementError(c, status, err)
		return
	}
	imageManagementData(c, policy)
}

func SaveImageChannelPool(c *gin.Context) {
	var input struct {
		Group      string `json:"group"`
		Platform   string `json:"platform"`
		Mode       string `json:"mode"`
		Model      string `json:"model"`
		Resolution string `json:"resolution"`
		Channels   []struct {
			Id       int `json:"id"`
			Priority int `json:"priority"`
		} `json:"channels"`
	}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &input); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	if input.Group == "" || len(input.Group) > 64 || input.Platform != "openai" && input.Platform != "gemini" || input.Mode != "resolution" && input.Mode != "model" && input.Mode != "model_resolution" {
		imageManagementError(c, 400, errors.New("Invalid image channel pool"))
		return
	}
	input.Model = strings.TrimSpace(input.Model)
	if input.Mode != "resolution" {
		if err := service.ValidateImageModelName(input.Model); err != nil {
			imageManagementError(c, 400, err)
			return
		}
	}
	if input.Mode != "model" && input.Resolution != "1K" && input.Resolution != "2K" && input.Resolution != "4K" {
		imageManagementError(c, 400, errors.New("Invalid pool resolution"))
		return
	}
	if input.Mode == "resolution" {
		input.Model = ""
	}
	if input.Mode == "model" {
		input.Resolution = ""
	}
	policy := model.ImageGroupPolicy{Group: input.Group, Platform: input.Platform, PoolMode: input.Mode}
	key := service.ImagePoolBindingKey(policy, input.Model, input.Resolution)
	seen := make(map[int]bool)
	rows := make([]model.ImageChannelPool, 0, len(input.Channels))
	for _, channel := range input.Channels {
		if channel.Id < 1 || seen[channel.Id] || channel.Priority < -1000000 || channel.Priority > 1000000 {
			imageManagementError(c, 400, errors.New("Invalid or duplicate pool channel"))
			return
		}
		seen[channel.Id] = true
		rows = append(rows, model.ImageChannelPool{BindingKey: key, Group: input.Group, Platform: input.Platform, Mode: input.Mode, Model: input.Model, Resolution: input.Resolution, ChannelId: channel.Id, Priority: channel.Priority})
	}
	// An explicit empty binding remains closed rather than losing its identity.
	if len(rows) == 0 {
		rows = append(rows, model.ImageChannelPool{BindingKey: key, Group: input.Group, Platform: input.Platform, Mode: input.Mode, Model: input.Model, Resolution: input.Resolution})
	}
	err := model.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("binding_key = ?", key).Delete(&model.ImageChannelPool{}).Error; err != nil {
			return err
		}
		return tx.Create(&rows).Error
	})
	if err != nil {
		imageManagementError(c, 400, err)
		return
	}
	imageManagementData(c, rows)
}

func SaveImageStorage(c *gin.Context) {
	var input struct {
		Profile     model.ImageStorageProfile        `json:"profile"`
		Credentials *service.ImageStorageCredentials `json:"credentials"`
	}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &input); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	profile, err := service.SaveImageStorageProfile(c.Request.Context(), input.Profile, input.Credentials)
	if err != nil {
		imageManagementError(c, 400, errors.New("Image storage configuration is invalid or unavailable"))
		return
	}
	imageManagementData(c, profile)
}

func TestImageStorage(c *gin.Context) {
	ctx := c.Request.Context()
	var profile model.ImageStorageProfile
	if err := model.DB.WithContext(ctx).Where("profile_id = ?", c.Param("profile_id")).Take(&profile).Error; err != nil {
		imageManagementError(c, 404, errors.New("Image storage profile was not found"))
		return
	}
	store, err := service.OpenImageStorage(ctx, profile)
	if err != nil {
		imageManagementError(c, 400, errors.New("Image storage is unavailable"))
		return
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		imageManagementError(c, 500, err)
		return
	}
	bitmap, err := service.ValidateImageBytes(encoded.Bytes(), "image/png", 1<<20, 100)
	if err != nil {
		imageManagementError(c, 500, err)
		return
	}
	intent := model.ImageUploadIntent{IntentKey: common.GetUUID(), ProfileId: profile.ProfileId, Class: profile.Class, UserId: c.GetInt("id"), ObjectKey: "connection-tests/" + common.GetUUID() + ".png", ContentType: bitmap.ContentType, ByteSize: int64(len(bitmap.Data)), Checksum: bitmap.Checksum, Status: "pending", CreatedAt: time.Now().Unix()}
	object, writeErr := service.StoreImageIntent(ctx, intent, bitmap, time.Now().Unix()+60)
	deleteErr := store.Delete(ctx, intent.ObjectKey)
	if deleteErr == nil && object.Id > 0 {
		deleteErr = model.DB.WithContext(ctx).Model(&model.ImageStorageObject{}).Where("object_id = ?", object.ObjectId).Update("status", "deleted").Error
	}
	if writeErr != nil || deleteErr != nil {
		imageManagementError(c, 503, errors.New("Image storage read/write/delete test failed; its cleanup intent is retained"))
		return
	}
	imageManagementData(c, gin.H{"write": true, "read": true, "delete": true})
}

func GetTokenImageMappings(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		imageManagementError(c, 400, errors.New("Invalid Token ID"))
		return
	}
	var token model.Token
	ctx := c.Request.Context()
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", id, c.GetInt("id")).Take(&token).Error; err != nil {
		imageManagementError(c, 404, errors.New("Token was not found"))
		return
	}
	var mappings []model.TokenImagePlatformMapping
	if err := model.DB.WithContext(ctx).Where("token_id = ?", id).Find(&mappings).Error; err != nil {
		imageManagementError(c, 503, errors.New("Image group settings are unavailable"))
		return
	}
	groups := map[string]*string{"openai": nil, "gemini": nil}
	for _, mapping := range mappings {
		group := mapping.Group
		groups[mapping.Platform] = &group
	}
	imageManagementData(c, groups)
}

func UpdateTokenImageMappings(c *gin.Context) {
	ctx := c.Request.Context()
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		imageManagementError(c, 400, err)
		return
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", id, c.GetInt("id")).Take(&token).Error; err != nil {
		imageManagementError(c, 404, errors.New("Token was not found"))
		return
	}
	var input map[string]*string
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<16), &input); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	var user model.User
	if err := model.DB.WithContext(ctx).Where("id = ?", token.UserId).Take(&user).Error; err != nil {
		imageManagementError(c, 503, err)
		return
	}
	allowed := service.GetUserUsableGroups(user.Group)
	for platform, group := range input {
		if platform != "openai" && platform != "gemini" {
			imageManagementError(c, 400, errors.New("Invalid image platform"))
			return
		}
		if group != nil {
			if _, ok := allowed[*group]; !ok {
				imageManagementError(c, 403, errors.New("Image group is outside user permissions"))
				return
			}
		}
	}
	err = model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for platform, group := range input {
			if group == nil {
				if err := tx.Where("token_id = ? AND platform = ?", token.Id, platform).Delete(&model.TokenImagePlatformMapping{}).Error; err != nil {
					return err
				}
				continue
			}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "token_id"}, {Name: "platform"}}, DoUpdates: clause.AssignmentColumns([]string{"group"})}).Create(&model.TokenImagePlatformMapping{TokenId: token.Id, Platform: platform, Group: *group}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		imageManagementError(c, 400, err)
		return
	}
	imageManagementData(c, input)
}
