package controller

import (
	"errors"
	"io"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"
)

type imageCleanupRequest struct {
	Scope              string                      `json:"scope"`
	Filters            service.ImageCleanupFilters `json:"filters"`
	PreviewFingerprint string                      `json:"preview_fingerprint"`
}

func PreviewImageCleanup(c *gin.Context) {
	var request imageCleanupRequest
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 4096), &request); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	query, err := service.ImageCleanupCandidates(c.Request.Context(), request.Scope, request.Filters)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	var summary struct {
		Count int64
		Bytes int64
	}
	if err := query.Select("COUNT(*) AS count, COALESCE(SUM(byte_size),0) AS bytes").Scan(&summary).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	preview, err := service.SignImageCleanupPreview(request.Scope, request.Filters, c.GetInt("id"), time.Now().Unix())
	if err != nil {
		imageManagementError(c, 503, errors.New("Stable image keys are required for cleanup previews"))
		return
	}
	imageManagementData(c, gin.H{"matched_items": summary.Count, "matched_bytes": summary.Bytes, "preview_fingerprint": preview})
}
func CreateImageCleanup(c *gin.Context) {
	var request imageCleanupRequest
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 4096), &request); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if !service.ValidateImageCleanupPreview(request.PreviewFingerprint, request.Scope, request.Filters, c.GetInt("id"), time.Now().Unix()) {
		imageLibraryFailure(c, errors.New("preview the same cleanup scope and filters before creating a job"))
		return
	}
	query, err := service.ImageCleanupCandidates(c.Request.Context(), request.Scope, request.Filters)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	filters, err := common.Marshal(request.Filters)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	job := model.ImageCleanupJob{JobId: "imgclean_" + service.ImageIdentityHash(request.PreviewFingerprint)[:32], Scope: request.Scope, Filters: string(filters), UserId: c.GetInt("id"), Status: "queued", Total: count, CreatedAt: time.Now().Unix()}
	if err := model.DB.WithContext(c.Request.Context()).Clauses(clause.OnConflict{DoNothing: true}).Create(&job).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if err := model.DB.WithContext(c.Request.Context()).Where("job_id = ? AND user_id = ?", job.JobId, c.GetInt("id")).Take(&job).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, job)
}
func ListImageCleanupJobs(c *gin.Context) {
	query, limit, err := imageLibraryPage(c, model.DB.WithContext(c.Request.Context()).Model(&model.ImageCleanupJob{}), "")
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	var jobs []model.ImageCleanupJob
	if err := query.Find(&jobs).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	next := ""
	if len(jobs) > limit {
		jobs = jobs[:limit]
		last := jobs[len(jobs)-1]
		next = imageNextCursor(last.CreatedAt, last.Id)
	}
	imageManagementData(c, gin.H{"items": jobs, "next_cursor": next})
}
