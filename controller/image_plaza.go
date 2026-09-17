package controller

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func ListImagePlaza(c *gin.Context) {
	query := model.DB.WithContext(c.Request.Context()).Model(&model.ImagePublication{}).Joins("JOIN image_library_items ON image_library_items.asset_id = image_publications.asset_id").Joins("JOIN image_storage_objects ON image_storage_objects.object_id = image_library_items.object_id").Where("image_publications.status = ? AND (image_publications.expires_at = 0 OR image_publications.expires_at > ?) AND image_library_items.deleted_at = 0 AND (image_library_items.expires_at = 0 OR image_library_items.expires_at > ?) AND image_storage_objects.status = ?", "published", time.Now().Unix(), time.Now().Unix(), "active")
	for _, field := range []string{"platform", "model", "aspect_ratio"} {
		if value := c.Query(field); value != "" {
			query = query.Where("image_library_items."+field+" = ?", value)
		}
	}
	if q := c.Query("q"); q != "" {
		query = query.Where("image_publications.title LIKE ? ESCAPE '!'", "%"+strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(q)+"%")
	}
	query, limit, err := imageLibraryPage(c, query, "image_publications.")
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	var publications []model.ImagePublication
	if err := query.Select("image_publications.*").Find(&publications).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	next := ""
	if len(publications) > limit {
		publications = publications[:limit]
		last := publications[len(publications)-1]
		next = imageNextCursor(last.CreatedAt, last.Id)
	}
	items := make([]gin.H, 0, len(publications))
	for _, publication := range publications {
		var asset model.ImageLibraryItem
		var object model.ImageStorageObject
		var user model.User
		if model.DB.WithContext(c.Request.Context()).Where("asset_id = ?", publication.AssetId).Take(&asset).Error != nil {
			continue
		}
		if model.DB.WithContext(c.Request.Context()).Where("object_id = ? AND status = ?", asset.ObjectId, "active").Take(&object).Error != nil {
			continue
		}
		_ = model.DB.WithContext(c.Request.Context()).Select("id", "display_name", "username").Where("id = ?", publication.UserId).Take(&user).Error
		creator := user.DisplayName
		if creator == "" {
			creator = user.Username
		}
		item := gin.H{"id": publication.PublicationId, "asset_id": publication.AssetId, "title": publication.Title, "creator": creator, "is_owner": publication.UserId == c.GetInt("id"), "platform": asset.Platform, "model": asset.Model, "aspect_ratio": asset.AspectRatio, "requested_size": asset.RequestedSize, "width": object.Width, "height": object.Height, "content_type": object.ContentType, "image_url": "/api/image-plaza/" + publication.PublicationId + "/content", "published_at": publication.PublishedAt, "expires_at": publication.ExpiresAt}
		if publication.SharePrompt {
			item["prompt"] = publication.PublicPrompt
		}
		items = append(items, item)
	}
	imageManagementData(c, gin.H{"items": items, "next_cursor": next})
}

func ImagePlazaContent(c *gin.Context) {
	ctx := c.Request.Context()
	var publication model.ImagePublication
	if err := model.DB.WithContext(ctx).Where("publication_id = ? AND status = ? AND (expires_at = 0 OR expires_at > ?)", c.Param("publication_id"), "published", time.Now().Unix()).Take(&publication).Error; err != nil {
		c.Status(404)
		return
	}
	var asset model.ImageLibraryItem
	if err := model.DB.WithContext(ctx).Where("asset_id = ? AND deleted_at = 0 AND (expires_at = 0 OR expires_at > ?)", publication.AssetId, time.Now().Unix()).Take(&asset).Error; err != nil {
		c.Status(404)
		return
	}
	var object model.ImageStorageObject
	if err := model.DB.WithContext(ctx).Where("object_id = ? AND status = ?", asset.ObjectId, "active").Take(&object).Error; err != nil {
		c.Status(404)
		return
	}
	store, err := service.GetImageObjectStorage(ctx, object)
	if err != nil {
		c.Status(503)
		return
	}
	c.Header("X-Content-Type-Options", "nosniff")
	if store.Profile.Backend == "local" {
		file, err := store.LocalPath(object.ObjectKey, false)
		if err != nil {
			c.Status(404)
			return
		}
		c.Header("Cache-Control", "public, max-age=3600")
		c.Header("Content-Type", object.ContentType)
		c.File(file)
		return
	}
	link, err := service.ImageObjectURL(ctx, object, AsyncImageGatewayBase(), 300)
	if err != nil {
		c.Status(503)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusTemporaryRedirect, link)
}

func ReportImagePublication(c *gin.Context) {
	var request struct {
		Reason  string `json:"reason"`
		Details string `json:"details"`
	}
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 16<<10), &request); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if !strings.Contains("|copyright|privacy|illegal|inappropriate|other|", "|"+request.Reason+"|") || request.Reason == "" || len(request.Details) > 10000 {
		imageLibraryFailure(c, errors.New("invalid report category or details"))
		return
	}
	var publication model.ImagePublication
	if err := model.DB.WithContext(c.Request.Context()).Where("publication_id = ? AND status = ? AND (expires_at = 0 OR expires_at > ?)", c.Param("publication_id"), "published", time.Now().Unix()).Take(&publication).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	report := model.ImageReport{PublicationId: publication.PublicationId, UserId: c.GetInt("id"), OpenKey: service.ImageIdentityHash("report", publication.PublicationId, strconv.Itoa(c.GetInt("id"))), Category: request.Reason, Message: request.Details, Status: "open", CreatedAt: time.Now().Unix()}
	if err := model.DB.WithContext(c.Request.Context()).Create(&report).Error; err != nil {
		var old model.ImageReport
		if findErr := model.DB.WithContext(c.Request.Context()).Where("open_key = ?", report.OpenKey).Take(&old).Error; findErr != nil {
			imageLibraryFailure(c, err)
			return
		}
		report = old
	}
	imageManagementData(c, report)
}

func ListAdminImagePublications(c *gin.Context) {
	query := model.DB.WithContext(c.Request.Context()).Model(&model.ImagePublication{})
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	if raw := c.Query("user_id"); raw != "" {
		id, err := strconv.Atoi(raw)
		if err != nil || id < 1 {
			imageLibraryFailure(c, errors.New("invalid user ID"))
			return
		}
		query = query.Where("user_id = ?", id)
	}
	query, limit, err := imageLibraryPage(c, query, "")
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	var items []model.ImagePublication
	if err := query.Find(&items).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = imageNextCursor(last.CreatedAt, last.Id)
	}
	imageManagementData(c, gin.H{"items": items, "next_cursor": next})
}
func ViewAdminImagePublication(c *gin.Context) {
	var publication model.ImagePublication
	if err := model.DB.WithContext(c.Request.Context()).Where("publication_id = ?", c.Param("publication_id")).Take(&publication).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	c.Params = append(c.Params, gin.Param{Key: "asset_id", Value: publication.AssetId})
	viewImageLibraryItem(c, true)
}

func ModerateImagePublication(c *gin.Context)        { moderateImageSubject(c, false) }
func ModerateDeferredImageSubmission(c *gin.Context) { moderateImageSubject(c, true) }
func moderateImageSubject(c *gin.Context, deferred bool) {
	var request struct {
		Reason string `json:"reason"`
	}
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 16<<10), &request); err != nil && !errors.Is(err, io.EOF) {
		imageLibraryFailure(c, err)
		return
	}
	action := c.Param("action")
	id := c.Param("publication_id")
	if deferred {
		id = c.Param("request_id")
	}
	if action != "approve" && action != "reject" && action != "hide" && action != "restore" {
		imageLibraryFailure(c, errors.New("invalid moderation action"))
		return
	}
	if deferred && action != "approve" && action != "reject" {
		imageLibraryFailure(c, errors.New("invalid deferred review action"))
		return
	}
	if len(request.Reason) > 10000 || ((action == "reject" || action == "hide") && strings.TrimSpace(request.Reason) == "") {
		imageLibraryFailure(c, errors.New("a review reason is required"))
		return
	}
	if err := model.ModerateImageSubject(c.Request.Context(), c.GetInt("id"), id, action, request.Reason, deferred, 0); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, gin.H{"updated": true})
}

func BatchModerateImagePublications(c *gin.Context) {
	var request struct {
		IDs    []string `json:"publication_ids"`
		Action string   `json:"action"`
		Reason string   `json:"reason"`
	}
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 32<<10), &request); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if len(request.IDs) < 1 || len(request.IDs) > 100 || len(request.Reason) > 10000 || (request.Action != "approve" && request.Action != "reject" && request.Action != "hide" && request.Action != "restore") || ((request.Action == "reject" || request.Action == "hide") && strings.TrimSpace(request.Reason) == "") {
		imageLibraryFailure(c, errors.New("invalid batch moderation request"))
		return
	}
	results := make([]gin.H, 0, len(request.IDs))
	seen := map[string]bool{}
	for _, id := range request.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		err := model.ModerateImageSubject(c.Request.Context(), c.GetInt("id"), id, request.Action, request.Reason, false, 0)
		result := gin.H{"id": id, "status": "updated"}
		if err != nil {
			result["status"] = "failed"
			result["reason"] = err.Error()
		}
		results = append(results, result)
	}
	imageManagementData(c, gin.H{"items": results})
}
func ListAdminImageReports(c *gin.Context) {
	query := model.DB.WithContext(c.Request.Context()).Model(&model.ImageReport{})
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	query, limit, err := imageLibraryPage(c, query, "")
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	var items []model.ImageReport
	if err := query.Find(&items).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = imageNextCursor(last.CreatedAt, last.Id)
	}
	imageManagementData(c, gin.H{"items": items, "next_cursor": next})
}
func ResolveImageReport(c *gin.Context) {
	var request struct {
		Status     string `json:"status"`
		Resolution string `json:"resolution"`
	}
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 16<<10), &request); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if (request.Status != "resolved" && request.Status != "dismissed") || strings.TrimSpace(request.Resolution) == "" || len(request.Resolution) > 10000 {
		imageLibraryFailure(c, errors.New("invalid report resolution"))
		return
	}
	id, err := strconv.Atoi(c.Param("report_id"))
	if err != nil || id < 1 {
		imageLibraryFailure(c, errors.New("invalid report ID"))
		return
	}
	err = model.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.ImageReport{}).Where("id = ? AND status = ?", id, "open").Updates(map[string]any{"status": request.Status, "resolution": request.Resolution, "open_key": service.ImageIdentityHash("closed-report", common.GetUUID())})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return model.ErrImageConflict
		}
		return tx.Create(&model.ImageModerationEvent{SubjectId: strconv.Itoa(id), ActorId: c.GetInt("id"), Action: request.Status, Reason: request.Resolution, CreatedAt: time.Now().Unix()}).Error
	})
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, gin.H{"updated": true})
}

func ImageLibraryStats(c *gin.Context) {
	ctx := c.Request.Context()
	counts := gin.H{}
	for name, query := range map[string]*gorm.DB{"item_count": model.DB.WithContext(ctx).Model(&model.ImageLibraryItem{}).Where("deleted_at = 0"), "object_count": model.DB.WithContext(ctx).Model(&model.ImageStorageObject{}).Where("status = ?", "active"), "pending_review": model.DB.WithContext(ctx).Model(&model.ImagePublication{}).Where("status = ?", "pending_review"), "published": model.DB.WithContext(ctx).Model(&model.ImagePublication{}).Where("status = ?", "published"), "open_reports": model.DB.WithContext(ctx).Model(&model.ImageReport{}).Where("status = ?", "open")} {
		var count int64
		if err := query.Count(&count).Error; err != nil {
			imageLibraryFailure(c, err)
			return
		}
		counts[name] = count
	}
	var deferred int64
	if err := model.DB.WithContext(ctx).Model(&model.ImageSubmissionRequest{}).Where("status = ? AND expires_at > ?", "pending_review", time.Now().Unix()).Count(&deferred).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	counts["pending_review"] = counts["pending_review"].(int64) + deferred
	var bytes int64
	if err := model.DB.WithContext(ctx).Model(&model.ImageStorageObject{}).Where("status = ?", "active").Select("COALESCE(SUM(byte_size),0)").Scan(&bytes).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	counts["total_bytes"] = bytes
	imageManagementData(c, counts)
}
func ImageLibraryMigration(c *gin.Context) {
	imageManagementData(c, gin.H{"applicable": false, "status": "not_applicable", "message": "No legacy image storage migration is configured"})
}
