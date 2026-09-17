package controller

import (
	"encoding/base64"
	"errors"
	"fmt"
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

func imageLibraryFailure(c *gin.Context, err error) {
	status := 400
	if errors.Is(err, gorm.ErrRecordNotFound) {
		status = 404
	}
	if errors.Is(err, model.ErrImageConflict) {
		status = 409
	}
	imageManagementError(c, status, err)
}

func imageLibraryPage(c *gin.Context, query *gorm.DB, prefix string) (*gorm.DB, int, error) {
	limit := 30
	if raw := c.Query("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return query, 0, errors.New("limit must be between 1 and 100")
		}
		limit = value
	}
	ascending := c.Query("sort") == "oldest"
	if sort := c.Query("sort"); sort != "" && sort != "newest" && sort != "oldest" {
		return query, 0, errors.New("invalid image sort")
	}
	if cursor := c.Query("cursor"); cursor != "" {
		value, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return query, 0, errors.New("invalid image cursor")
		}
		stamp, id, ok := strings.Cut(string(value), ":")
		created, err := strconv.ParseInt(stamp, 10, 64)
		rowId, idErr := strconv.Atoi(id)
		if !ok || err != nil || idErr != nil || created < 0 || rowId < 1 {
			return query, 0, errors.New("invalid image cursor")
		}
		operator := "<"
		if ascending {
			operator = ">"
		}
		query = query.Where(fmt.Sprintf("(%screated_at %s ? OR (%screated_at = ? AND %sid %s ?))", prefix, operator, prefix, prefix, operator), created, created, rowId)
	}
	direction := " DESC"
	if ascending {
		direction = " ASC"
	}
	return query.Order(prefix + "created_at" + direction).Order(prefix + "id" + direction).Limit(limit + 1), limit, nil
}

func imageNextCursor(created int64, id int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%d", created, id)))
}

func imageOwnedAsset(c *gin.Context, admin bool) (model.ImageLibraryItem, error) {
	var item model.ImageLibraryItem
	query := model.DB.WithContext(c.Request.Context()).Where("asset_id = ? AND deleted_at = 0 AND (expires_at = 0 OR expires_at > ?)", c.Param("asset_id"), time.Now().Unix())
	if !admin {
		query = query.Where("user_id = ?", c.GetInt("id"))
	}
	err := query.Take(&item).Error
	return item, err
}

func imageAssetDTO(c *gin.Context, item model.ImageLibraryItem) (gin.H, error) {
	var object model.ImageStorageObject
	if err := model.DB.WithContext(c.Request.Context()).Where("object_id = ? AND status = ?", item.ObjectId, "active").Take(&object).Error; err != nil {
		return nil, err
	}
	return gin.H{"item": item, "object": object, "view_url": "/api/user/image-library/" + item.AssetId + "/view"}, nil
}

func ListImageLibrary(c *gin.Context)      { listImageLibrary(c, false) }
func ListAdminImageLibrary(c *gin.Context) { listImageLibrary(c, true) }
func listImageLibrary(c *gin.Context, admin bool) {
	query := model.DB.WithContext(c.Request.Context()).Model(&model.ImageLibraryItem{})
	if !admin {
		query = query.Where("user_id = ? AND deleted_at = 0 AND (expires_at = 0 OR expires_at > ?)", c.GetInt("id"), time.Now().Unix())
	} else if user := c.Query("user_id"); user != "" {
		id, err := strconv.Atoi(user)
		if err != nil || id < 1 {
			imageLibraryFailure(c, errors.New("invalid user ID"))
			return
		}
		query = query.Where("user_id = ?", id)
	}
	for _, field := range []string{"platform", "model", "source"} {
		if value := c.Query(field); value != "" {
			query = query.Where(field+" = ?", value)
		}
	}
	if q := c.Query("q"); q != "" {
		query = query.Where("title LIKE ? ESCAPE '!'", "%"+strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(q)+"%")
	}
	query, limit, err := imageLibraryPage(c, query, "")
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	var items []model.ImageLibraryItem
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
	data := make([]gin.H, 0, len(items))
	for _, item := range items {
		dto, err := imageAssetDTO(c, item)
		if err != nil {
			continue
		}
		if admin {
			dto["view_url"] = "/api/admin/image-library/" + item.AssetId + "/view"
		}
		data = append(data, dto)
	}
	imageManagementData(c, gin.H{"items": data, "next_cursor": next})
}

func GetImageLibraryItem(c *gin.Context) {
	item, err := imageOwnedAsset(c, false)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	dto, err := imageAssetDTO(c, item)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, dto)
}
func PatchImageLibraryItem(c *gin.Context) {
	item, err := imageOwnedAsset(c, false)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	var request struct {
		Title  *string `json:"title"`
		Prompt *string `json:"private_prompt"`
	}
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 66<<10), &request); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	updates := map[string]any{}
	if request.Title != nil {
		if len(*request.Title) > 1000 {
			imageLibraryFailure(c, errors.New("title exceeds limit"))
			return
		}
		updates["title"] = *request.Title
	}
	if request.Prompt != nil {
		if len(*request.Prompt) > 64<<10 {
			imageLibraryFailure(c, errors.New("prompt exceeds limit"))
			return
		}
		updates["prompt"] = *request.Prompt
	}
	if err := model.DB.WithContext(c.Request.Context()).Model(&model.ImageLibraryItem{}).Where("id = ? AND user_id = ?", item.Id, c.GetInt("id")).Updates(updates).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, gin.H{"updated": true})
}
func DeleteImageLibraryItem(c *gin.Context) {
	item, err := imageOwnedAsset(c, false)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if err := model.DB.WithContext(c.Request.Context()).Model(&model.ImageLibraryItem{}).Where("id = ?", item.Id).Update("deleted_at", time.Now().Unix()).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, gin.H{"deleted": true})
}

func ViewImageLibraryItem(c *gin.Context)      { viewImageLibraryItem(c, false) }
func ViewAdminImageLibraryItem(c *gin.Context) { viewImageLibraryItem(c, true) }
func viewImageLibraryItem(c *gin.Context, admin bool) {
	item, err := imageOwnedAsset(c, admin)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	var object model.ImageStorageObject
	if err := model.DB.WithContext(c.Request.Context()).Where("object_id = ? AND status = ?", item.ObjectId, "active").Take(&object).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	cfg, err := service.GetImageRuntimeConfig(c.Request.Context())
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	link, err := service.ImageObjectURL(c.Request.Context(), object, AsyncImageGatewayBase(), cfg.SignedURLExpiry)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if strings.Contains(c.GetHeader("Accept"), "application/json") {
		imageManagementData(c, gin.H{"url": link, "expires_at": time.Now().Unix() + int64(cfg.SignedURLExpiry)})
		return
	}
	c.Redirect(http.StatusTemporaryRedirect, link)
}

func ImportImageLibrary(c *gin.Context)    { importImageLibrary(c, false) }
func ImportImageLibraryURL(c *gin.Context) { importImageLibrary(c, true) }
func importImageLibrary(c *gin.Context, fromURL bool) {
	ctx := c.Request.Context()
	cfg, err := service.GetImageRuntimeConfig(ctx)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if err := model.RecordImageLibraryImport(ctx, c.GetInt("id"), cfg.ImportsPerMinute); err != nil {
		if errors.Is(err, model.ErrImageUploadRateLimited) {
			c.Header("Retry-After", "60")
			imageManagementError(c, 429, errors.New("Image import attempts exceed the rolling minute limit"))
			return
		}
		imageLibraryFailure(c, err)
		return
	}
	key, err := AsyncImageIdempotencyKey(c)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if key == "" {
		key = common.GetUUID()
	}
	var metadata service.ImageAssetMetadata
	var image service.ImageBytes
	if fromURL {
		if err := common.DecodeJson(io.LimitReader(c.Request.Body, 128<<10), &metadata); err != nil {
			imageLibraryFailure(c, err)
			return
		}
		if err := metadata.Validate(cfg, false); err != nil {
			imageLibraryFailure(c, err)
			return
		}
		var alias model.ImageInputAlias
		aliasErr := model.DB.WithContext(ctx).Where("url_hash = ?", service.ImageIdentityHash(metadata.ImageURL)).Take(&alias).Error
		if aliasErr == nil {
			var owner model.Token
			if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", alias.TokenId, c.GetInt("id")).Take(&owner).Error; err != nil {
				imageLibraryFailure(c, errors.New("SC input belongs to another user"))
				return
			}
			image, _, err = service.ResolveImageReference(ctx, owner.Id, metadata.ImageURL, cfg)
		} else if !errors.Is(aliasErr, gorm.ErrRecordNotFound) {
			imageLibraryFailure(c, aliasErr)
			return
		} else if service.IsGatewayImageReference(metadata.ImageURL) {
			imageLibraryFailure(c, errors.New("SC input alias is unavailable"))
			return
		} else {
			image, err = service.DownloadImageReference(ctx, metadata.ImageURL, cfg)
		}
	} else {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, cfg.LibraryImageBytes+128<<10)
		if err := c.Request.ParseMultipartForm(128 << 10); err != nil {
			imageLibraryFailure(c, err)
			return
		}
		defer c.Request.MultipartForm.RemoveAll()
		if raw := c.PostForm("metadata"); raw != "" {
			if err := common.UnmarshalJsonStr(raw, &metadata); err != nil {
				imageLibraryFailure(c, err)
				return
			}
		}
		file, header, readErr := c.Request.FormFile("file")
		if readErr != nil {
			imageLibraryFailure(c, readErr)
			return
		}
		defer file.Close()
		data, readErr := io.ReadAll(io.LimitReader(file, cfg.LibraryImageBytes+1))
		if readErr != nil {
			imageLibraryFailure(c, readErr)
			return
		}
		image, err = service.ValidateImageBytes(data, header.Header.Get("Content-Type"), cfg.LibraryImageBytes, cfg.LibraryImagePixels)
	}
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if int64(len(image.Data)) > cfg.LibraryImageBytes || int64(image.Width)*int64(image.Height) > cfg.LibraryImagePixels {
		imageLibraryFailure(c, errors.New("imported image exceeds limits"))
		return
	}
	item, reused, err := service.ArchiveImageBytes(ctx, c.GetInt("id"), image, metadata, key, "manual_import", "", cfg)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, gin.H{"item": item, "reused": reused})
}

func ImageLibraryFromTask(c *gin.Context) {
	var request struct {
		TaskId string `json:"task_id"`
		Index  int    `json:"image_index"`
		Title  string `json:"title"`
	}
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 4096), &request); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	cfg, err := service.GetImageRuntimeConfig(c.Request.Context())
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	item, reused, err := service.ArchiveAsyncImageResult(c.Request.Context(), c.GetInt("id"), request.TaskId, request.Index, request.Title, cfg)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, gin.H{"item": item, "reused": reused})
}

func CreateImageLibraryPublication(c *gin.Context) {
	var request struct {
		Title string `json:"public_title"`
		Share bool   `json:"share_prompt"`
	}
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 4096), &request); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if len(request.Title) > 1000 {
		imageLibraryFailure(c, errors.New("title exceeds limit"))
		return
	}
	item, err := model.CreateImagePublication(c.Request.Context(), c.GetInt("id"), c.Param("asset_id"), request.Title, request.Share)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, item)
}
func WithdrawImageLibraryPublication(c *gin.Context) {
	asset, err := imageOwnedAsset(c, false)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	var publication model.ImagePublication
	if err := model.DB.WithContext(c.Request.Context()).Where("asset_id = ? AND user_id = ? AND status IN ?", asset.AssetId, c.GetInt("id"), []string{"pending_review", "published", "hidden"}).Order("id DESC").Take(&publication).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if err := model.ModerateImageSubject(c.Request.Context(), c.GetInt("id"), publication.PublicationId, "withdraw", "", false, c.GetInt("id")); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, gin.H{"withdrawn": true})
}

func CreateDeferredImageSubmission(c *gin.Context) {
	ctx := c.Request.Context()
	cfg, err := service.GetImageRuntimeConfig(ctx)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	key, err := AsyncImageIdempotencyKey(c)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if key == "" {
		key = common.GetUUID()
	}
	var metadata service.ImageAssetMetadata
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 128<<10), &metadata); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if err := metadata.Validate(cfg, true); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	encoded, err := common.Marshal(metadata)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	now := time.Now().Unix()
	userId := c.GetInt("id")
	item := model.ImageSubmissionRequest{SubmissionId: "imgsub_" + common.GetUUID(), UserId: userId, OperationKey: service.ImageIdentityHash("deferred-submission", strconv.Itoa(userId), key), Fingerprint: service.ImageIdentityHash(string(encoded)), ClientBlobKey: metadata.ClientBlobKey, Checksum: metadata.Checksum, ByteSize: metadata.ByteSize, ContentType: metadata.ContentType, Metadata: string(encoded), SharePrompt: metadata.SharePrompt, Status: "pending_review", Version: 1, CreatedAt: now, ExpiresAt: now + 90*86400}
	reused := false
	err = model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Updating the user with its existing value acquires a portable row lock.
		if err := tx.Model(&model.User{}).Where("id = ?", userId).UpdateColumn("quota", gorm.Expr("quota")).Error; err != nil {
			return err
		}
		var old model.ImageSubmissionRequest
		findErr := tx.Where("operation_key = ?", item.OperationKey).Take(&old).Error
		if findErr == nil {
			if old.Fingerprint != item.Fingerprint {
				return model.ErrImageConflict
			}
			item, reused = old, true
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		var recent int64
		if err := tx.Model(&model.ImageSubmissionRequest{}).Where("user_id = ? AND created_at > ?", userId, now-60).Count(&recent).Error; err != nil {
			return err
		}
		if recent >= int64(cfg.SubmissionsPerMinute) {
			return errors.New("submission rate limit exceeded")
		}
		var held struct {
			Count int64
			Bytes int64
		}
		if err := tx.Model(&model.ImageSubmissionRequest{}).Select("COUNT(*) AS count, COALESCE(SUM(byte_size),0) AS bytes").Where("user_id = ? AND expires_at > ? AND status IN ?", userId, now, []string{"pending_review", "approved_pending_sync"}).Scan(&held).Error; err != nil {
			return err
		}
		if held.Count >= 40 || item.ByteSize > (400<<20)-held.Bytes {
			return errors.New("pending submission quota exceeded")
		}
		return tx.Create(&item).Error
	})
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, gin.H{"item": item, "reused": reused, "image_held_client_side": true})
}

func ListDeferredImageSubmissions(c *gin.Context)      { listDeferredImageSubmissions(c, false) }
func ListAdminDeferredImageSubmissions(c *gin.Context) { listDeferredImageSubmissions(c, true) }
func listDeferredImageSubmissions(c *gin.Context, admin bool) {
	query := model.DB.WithContext(c.Request.Context()).Model(&model.ImageSubmissionRequest{})
	if !admin {
		query = query.Where("user_id = ?", c.GetInt("id"))
	}
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	query, limit, err := imageLibraryPage(c, query, "")
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	var items []model.ImageSubmissionRequest
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
	imageManagementData(c, gin.H{"items": items, "next_cursor": next, "image_held_client_side": true})
}
func WithdrawDeferredImageSubmission(c *gin.Context) {
	if err := model.ModerateImageSubject(c.Request.Context(), c.GetInt("id"), c.Param("request_id"), "withdraw", "", true, c.GetInt("id")); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, gin.H{"withdrawn": true})
}
func SyncDeferredImageSubmission(c *gin.Context) {
	ctx := c.Request.Context()
	var submission model.ImageSubmissionRequest
	if err := model.DB.WithContext(ctx).Where("submission_id = ? AND user_id = ?", c.Param("request_id"), c.GetInt("id")).Take(&submission).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if submission.ExpiresAt <= time.Now().Unix() || (submission.Status != "approved_pending_sync" && submission.Status != "synced") {
		imageLibraryFailure(c, model.ErrImageConflict)
		return
	}
	cfg, err := service.GetImageRuntimeConfig(ctx)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, cfg.LibraryImageBytes+64<<10)
	if err := c.Request.ParseMultipartForm(64 << 10); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	defer c.Request.MultipartForm.RemoveAll()
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, cfg.LibraryImageBytes+1))
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	image, err := service.ValidateImageBytes(data, header.Header.Get("Content-Type"), cfg.LibraryImageBytes, cfg.LibraryImagePixels)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if image.Checksum != submission.Checksum || int64(len(data)) != submission.ByteSize || image.ContentType != submission.ContentType {
		imageLibraryFailure(c, errors.New("original image checksum, MIME or byte size differs from approved submission"))
		return
	}
	var metadata service.ImageAssetMetadata
	if err := common.UnmarshalJsonStr(submission.Metadata, &metadata); err != nil {
		imageLibraryFailure(c, err)
		return
	}
	asset, reused, err := service.ArchiveImageBytes(ctx, c.GetInt("id"), image, metadata, submission.SubmissionId, "realtime_import", submission.SubmissionId, cfg)
	if err != nil {
		imageLibraryFailure(c, err)
		return
	}
	if err := model.DB.WithContext(ctx).Where("submission_id = ?", submission.SubmissionId).Take(&submission).Error; err != nil {
		imageLibraryFailure(c, err)
		return
	}
	imageManagementData(c, gin.H{"item": submission, "library_item": asset, "reused": reused})
}
