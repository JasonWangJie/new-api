package controller

import (
	"errors"
	"io"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

type imageCleanupRequest struct {
	Scope              string                      `json:"scope"`
	Filters            service.ImageCleanupFilters `json:"filters"`
	PreviewFingerprint string                      `json:"preview_fingerprint"`
}

func imageCleanupResponse(job model.ImageCleanupJob, actor int) any {
	token := ""
	if job.Status == "preview_ready" && job.UserId == actor {
		token = job.PreviewToken
	}
	return struct {
		model.ImageCleanupJob
		PreviewFingerprint string `json:"preview_fingerprint,omitempty"`
	}{job, token}
}

func PreviewImageCleanup(c *gin.Context) {
	var request imageCleanupRequest
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 4096), &request); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	job, err := service.NewImageCleanupPreview(c.Request.Context(), request.Scope, request.Filters, c.GetInt("id"))
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	service.WakeImageCleanup()
	imageManagementData(c, imageCleanupResponse(job, c.GetInt("id")))
}

func CreateImageCleanup(c *gin.Context) {
	var request imageCleanupRequest
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 4096), &request); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	job, err := service.ConfirmImageCleanupPreview(c.Request.Context(), request.Scope, request.Filters, request.PreviewFingerprint, c.GetInt("id"))
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	service.WakeImageCleanup()
	imageManagementData(c, imageCleanupResponse(job, c.GetInt("id")))
}

func ListImageCleanupJobs(c *gin.Context) {
	jobs, err := model.RecentImageCleanupJobs(c.Request.Context(), time.Now().Unix())
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	items := make([]any, 0, len(jobs))
	for _, job := range jobs {
		items = append(items, imageCleanupResponse(job, c.GetInt("id")))
	}
	imageManagementData(c, gin.H{"items": items, "next_cursor": ""})
}

func GetImageCleanupJob(c *gin.Context) {
	var job model.ImageCleanupJob
	if err := model.DB.WithContext(c.Request.Context()).Where("job_id = ?", c.Param("job_id")).Take(&job).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, imageCleanupResponse(job, c.GetInt("id")))
}

func ControlImageCleanupJob(c *gin.Context) {
	job, err := service.ControlImageCleanup(c.Request.Context(), c.Param("job_id"), c.Param("action"))
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	service.WakeImageCleanup()
	_ = model.DB.WithContext(c.Request.Context()).Create(&model.ImageModerationEvent{SubjectId: job.JobId, ActorId: c.GetInt("id"), Action: "cleanup_" + c.Param("action"), CreatedAt: time.Now().Unix()}).Error
	imageManagementData(c, imageCleanupResponse(job, c.GetInt("id")))
}

func GetImageCleanupConfig(c *gin.Context) {
	cfg, err := service.GetImageCleanupConfig(c.Request.Context())
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, struct {
		service.ImageCleanupConfig
		Runtime service.ImageCleanupRuntimeStatus `json:"runtime"`
	}{cfg, service.GetImageCleanupRuntimeStatus()})
}

func SaveImageCleanupConfig(c *gin.Context) {
	cfg := service.DefaultImageCleanupConfig()
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 128<<10), &cfg); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if err := service.SaveImageCleanupConfig(c.Request.Context(), cfg); err != nil {
		if errors.Is(err, model.ErrOptionRevisionConflict) {
			imageManagementError(c, 409, err)
		} else {
			imageLibraryFailure(c, err)
		}
		return
	}
	service.WakeImageCleanup()
	GetImageCleanupConfig(c)
}
