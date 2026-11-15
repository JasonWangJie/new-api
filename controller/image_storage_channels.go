package controller

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func imageStorageWriteError(c *gin.Context, err error) {
	status, message := http.StatusBadRequest, "Image storage configuration is invalid or unavailable"
	if errors.Is(err, model.ErrOptionRevisionConflict) {
		status, message = http.StatusConflict, err.Error()
	} else if errors.Is(err, model.ErrActiveImageStorageChannel) {
		status, message = http.StatusConflict, model.ErrActiveImageStorageChannel.Error()
	} else if errors.Is(err, gorm.ErrRecordNotFound) {
		status, message = http.StatusNotFound, "Image storage profile was not found"
	}
	imageManagementError(c, status, errors.New(message))
}

func DeleteImageStorage(c *gin.Context) {
	var input struct {
		Revision *int64 `json:"revision"`
	}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4096), &input); err != nil {
		imageStorageWriteError(c, err)
		return
	}
	if input.Revision == nil {
		imageManagementError(c, http.StatusPreconditionRequired, errors.New("Configuration revision is required. Reload it before saving"))
		return
	}
	revision, err := model.RemoveImageStorageChannel(c.Request.Context(), c.Param("profile_id"), *input.Revision)
	if err != nil {
		imageStorageWriteError(c, err)
		return
	}
	imageManagementData(c, gin.H{"storage_revision": revision})
}

func UpdateImageStorage(c *gin.Context) {
	var input struct {
		Profile     model.ImageStorageProfile        `json:"profile"`
		Credentials *service.ImageStorageCredentials `json:"credentials"`
		Revision    *int64                           `json:"revision"`
	}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &input); err != nil {
		imageStorageWriteError(c, err)
		return
	}
	if input.Revision == nil {
		imageManagementError(c, http.StatusPreconditionRequired, errors.New("Configuration revision is required. Reload it before saving"))
		return
	}
	profile, revision, err := service.SaveImageStorageChannel(c.Request.Context(), input.Profile, input.Credentials, c.Param("profile_id"), input.Revision)
	if err != nil {
		imageStorageWriteError(c, err)
		return
	}
	imageManagementData(c, gin.H{"profile": profile, "storage_revision": revision})
}

func SetImageStorageActive(c *gin.Context, active bool) {
	var input struct {
		Revision *int64 `json:"revision"`
	}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4096), &input); err != nil {
		imageStorageWriteError(c, err)
		return
	}
	if input.Revision == nil {
		imageManagementError(c, http.StatusPreconditionRequired, errors.New("Configuration revision is required. Reload it before saving"))
		return
	}
	var profile model.ImageStorageProfile
	revision, err := model.WithImageStorageRevision(c.Request.Context(), input.Revision, func(tx *gorm.DB) error {
		if err := tx.Where("profile_id = ?", c.Param("profile_id")).Take(&profile).Error; err != nil {
			return err
		}
		if !profile.CurrentRevision {
			return model.ErrOptionRevisionConflict
		}
		if !active && profile.Active {
			return errors.New("enable another storage channel before disabling the current channel")
		}
		if active {
			if err := service.ValidateImageStoragePublicBaseURL(profile.PublicBaseURL); err != nil {
				return err
			}
			if profile.Provider == "r2" && profile.PublicBaseURL == "" {
				return errors.New("R2 storage requires a public access domain")
			}
			if _, err := service.OpenImageStorage(c.Request.Context(), profile); err != nil {
				return err
			}
			if err := tx.Model(&model.ImageStorageProfile{}).Where("class = ? AND active = ?", profile.Class, true).Update("active", false).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&model.ImageStorageProfile{}).Where("id = ?", profile.Id).Update("active", active).Error; err != nil {
			return err
		}
		profile.Active = active
		profile.CredentialConfigured = len(profile.CredentialCipher) > 0
		return nil
	})
	if err != nil {
		imageStorageWriteError(c, err)
		return
	}
	imageManagementData(c, gin.H{"profile": profile, "storage_revision": revision})
}

// RotateImageStorageCredentials explicitly updates all revisions of one location.
// Different endpoints require separate rotation, so old credentials never travel
// to a newly selected destination as a side effect of editing a channel.
func RotateImageStorageCredentials(c *gin.Context) {
	var input struct {
		Revision    *int64                          `json:"revision"`
		Credentials service.ImageStorageCredentials `json:"credentials"`
	}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10), &input); err != nil {
		imageStorageWriteError(c, err)
		return
	}
	if input.Revision == nil {
		imageManagementError(c, http.StatusPreconditionRequired, errors.New("Configuration revision is required. Reload it before saving"))
		return
	}
	if input.Credentials.AccessKey == "" || input.Credentials.SecretKey == "" {
		imageStorageWriteError(c, errors.New("credentials required"))
		return
	}
	data, err := common.Marshal(input.Credentials)
	if err != nil {
		imageStorageWriteError(c, err)
		return
	}
	var current model.ImageStorageProfile
	revision, err := model.WithImageStorageRevision(c.Request.Context(), input.Revision, func(tx *gorm.DB) error {
		if err := tx.Where("profile_id = ?", c.Param("profile_id")).Take(&current).Error; err != nil {
			return err
		}
		if current.Backend != "s3" {
			return errors.New("credentials require S3 storage")
		}
		var profiles []model.ImageStorageProfile
		if err := tx.Where("channel_id = ?", current.ChannelId).Find(&profiles).Error; err != nil {
			return err
		}
		for _, profile := range profiles {
			if profile.Backend != current.Backend || profile.Endpoint != current.Endpoint || profile.Provider != current.Provider || profile.Bucket != current.Bucket {
				continue
			}
			ciphertext, err := service.EncryptImagePayload(data, "storage:"+profile.ProfileId)
			if err != nil {
				return err
			}
			if err := tx.Model(&model.ImageStorageProfile{}).Where("id = ?", profile.Id).Update("credential_cipher", ciphertext).Error; err != nil {
				return err
			}
		}
		current.CredentialConfigured = true
		return nil
	})
	if err != nil {
		imageStorageWriteError(c, err)
		return
	}
	imageManagementData(c, gin.H{"profile": current, "storage_revision": revision})
}
