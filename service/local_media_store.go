package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	mp4 "github.com/abema/go-mp4"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type localMediaStore struct{}

func (localMediaStore) Enabled() bool {
	if model.DB == nil {
		return false
	}
	cfg, err := GetMediaRuntimeConfig(context.Background())
	return err == nil && cfg.VideoAsyncEnabled
}

func (localMediaStore) Resolve(task *model.Task, key string) (*StoredArtifactRef, error) {
	var object model.MediaArtifactObject
	err := model.DB.Where("identity_hash = ? AND user_id = ? AND status = ? AND expires_at > ?", ImageIdentityHash("video-artifact", task.TaskID, key), task.UserId, "active", time.Now().Unix()).Take(&object).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &StoredArtifactRef{Backend: "local", Bucket: object.RootPath, ObjectKey: object.ObjectKey, MimeType: object.MimeType, Size: object.ByteSize, Checksum: object.Checksum}, nil
}

func (store localMediaStore) Persist(ctx context.Context, task *model.Task, artifact types.TaskArtifact, content io.Reader) (*StoredArtifactRef, error) {
	var job model.AsyncMediaJob
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", task.TaskID, task.UserId).Take(&job).Error; err != nil {
		return nil, err
	}
	if ref, err := store.Resolve(task, artifact.Key); err != nil || ref != nil {
		return ref, err
	}
	cfg, err := GetMediaRuntimeConfig(ctx)
	if err != nil {
		return nil, err
	}
	rootPath, err := filepath.Abs(cfg.LocalPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(rootPath, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	objectID := "media_" + common.GetUUID()
	temporary := objectID + ".part"
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	defer root.Remove(temporary)
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(content, cfg.MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size == 0 || size > cfg.MaxFileBytes {
		return nil, errors.New("video file byte limit exceeded")
	}
	mimeType, extension, err := ValidateVideoFile(file, size)
	if err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	key := objectID + extension
	if err := root.Rename(temporary, key); err != nil {
		return nil, err
	}
	object := model.MediaArtifactObject{ObjectId: objectID, IdentityHash: ImageIdentityHash("video-artifact", task.TaskID, artifact.Key), TaskId: task.TaskID, ArtifactKey: artifact.Key, UserId: job.UserId, TokenId: job.TokenId, RootPath: rootPath, ObjectKey: key, MimeType: mimeType, ByteSize: size, Checksum: hex.EncodeToString(hash.Sum(nil)), Status: "active", CreatedAt: time.Now().Unix(), ExpiresAt: job.ExpiresAt}
	result := model.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&object)
	if result.Error != nil || result.RowsAffected == 0 {
		_ = root.Remove(key)
		if result.Error != nil {
			return nil, result.Error
		}
	}
	return store.Resolve(task, artifact.Key)
}

// ValidateVideoFile checks ISO BMFF box boundaries and required video container
// boxes without loading the media payload. Truncated and HTML/JSON bodies fail.
func ValidateVideoFile(file *os.File, size int64) (string, string, error) {
	var header [16]byte
	ftyp, moov, mdat := false, false, false
	for offset := int64(0); offset < size; {
		if size-offset < 8 {
			return "", "", errors.New("truncated video container")
		}
		if _, err := file.ReadAt(header[:8], offset); err != nil {
			return "", "", err
		}
		boxSize := int64(binary.BigEndian.Uint32(header[:4]))
		headerSize := int64(8)
		if boxSize == 1 {
			if _, err := file.ReadAt(header[8:], offset+8); err != nil {
				return "", "", err
			}
			large := binary.BigEndian.Uint64(header[8:])
			if large > uint64(size-offset) {
				return "", "", errors.New("invalid video box length")
			}
			boxSize, headerSize = int64(large), 16
		} else if boxSize == 0 {
			boxSize = size - offset
		}
		if boxSize < headerSize || boxSize > size-offset {
			return "", "", errors.New("invalid video box length")
		}
		switch string(header[4:8]) {
		case "ftyp":
			ftyp = boxSize >= headerSize+8
		case "moov":
			moov = boxSize > headerSize
		case "mdat":
			mdat = boxSize > headerSize
		}
		offset += boxSize
	}
	if !ftyp || !moov || !mdat {
		return "", "", errors.New("unsupported or damaged video container")
	}
	videoTrack := false
	boxes := 0
	_, err := mp4.ReadBoxStructure(file, func(handle *mp4.ReadHandle) (any, error) {
		boxes++
		if boxes > 100000 || handle.BoxInfo.Offset > uint64(size) || handle.BoxInfo.Size > uint64(size)-handle.BoxInfo.Offset {
			return nil, errors.New("invalid video metadata bounds")
		}
		switch handle.BoxInfo.Type {
		case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl():
			return handle.Expand()
		case mp4.BoxTypeHdlr():
			box, _, err := handle.ReadPayload()
			if err != nil {
				return nil, err
			}
			if handler, ok := box.(*mp4.Hdlr); ok && string(handler.HandlerType[:]) == "vide" && len(handle.Path) == 4 && handle.Path[0] == mp4.BoxTypeMoov() && handle.Path[1] == mp4.BoxTypeTrak() && handle.Path[2] == mp4.BoxTypeMdia() {
				videoTrack = true
			}
		}
		return nil, nil
	})
	if err != nil {
		return "", "", err
	}
	if !videoTrack {
		return "", "", errors.New("video container has no video track")
	}
	return "video/mp4", ".mp4", nil
}

func (localMediaStore) Serve(c *gin.Context, task *model.Task, ref *StoredArtifactRef) error {
	root, err := os.OpenRoot(ref.Bucket)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.Open(ref.ObjectKey)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() != ref.Size {
		return errors.New("stored media is unavailable")
	}
	c.Header("Content-Type", ref.MimeType)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-store")
	c.Header("ETag", "\""+ref.Checksum+"\"")
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s.mp4\"", task.TaskID))
	http.ServeContent(c.Writer, c.Request, ref.ObjectKey, stat.ModTime(), file)
	c.Writer.WriteHeaderNow()
	return nil
}

func MediaObjectURL(object model.MediaArtifactObject, base string, ttl int) (string, error) {
	if common.CryptoSecret == "" || object.ExpiresAt <= time.Now().Unix() || ttl <= 0 {
		return "", errors.New("media signing is unavailable")
	}
	expires := min(object.ExpiresAt, time.Now().Unix()+int64(ttl))
	message := object.ObjectId + "\x00" + object.TaskId + "\x00" + strconv.FormatInt(expires, 10)
	mac := hmac.New(sha256.New, []byte(common.CryptoSecret))
	_, _ = mac.Write([]byte(message))
	return strings.TrimRight(base, "/") + "/v1/media/objects/" + object.ObjectId + "?expires=" + strconv.FormatInt(expires, 10) + "&access=" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifyMediaObjectAccess(object model.MediaArtifactObject, expires, access string) bool {
	deadline, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || deadline <= time.Now().Unix() || deadline > object.ExpiresAt || common.CryptoSecret == "" {
		return false
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(access)
	if err != nil || len(signature) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(common.CryptoSecret))
	_, _ = mac.Write([]byte(object.ObjectId + "\x00" + object.TaskId + "\x00" + expires))
	return hmac.Equal(signature, mac.Sum(nil))
}

func CleanupMediaObjects(ctx context.Context) error {
	var jobs []model.AsyncMediaJob
	now := time.Now().Unix()
	if err := model.DB.WithContext(ctx).Where("expires_at <= ? AND lease_expires_at <= ? AND (status <> ? OR output_object_key <> '' OR request_cipher IS NOT NULL OR channel_cipher IS NOT NULL)", now, now, "expired").Limit(100).Find(&jobs).Error; err != nil {
		return err
	}
	for _, job := range jobs {
		if err := RemoveAsyncMediaOutput(job); err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Model(&job).Where("lease_expires_at <= ?", now).Updates(map[string]any{"status": "expired", "storage_status": "expired", "request_cipher": nil, "channel_cipher": nil, "output_root_path": "", "output_object_key": ""}).Error; err != nil {
			return err
		}
	}
	var objects []model.MediaArtifactObject
	if err := model.DB.WithContext(ctx).Where("expires_at <= ?", time.Now().Unix()).Limit(100).Find(&objects).Error; err != nil {
		return err
	}
	for _, object := range objects {
		if err := model.DB.WithContext(ctx).Model(&object).Update("status", "expired").Error; err != nil {
			return err
		}
		root, err := os.OpenRoot(object.RootPath)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		} else if err == nil {
			err = root.Remove(object.ObjectKey)
			root.Close()
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := model.DB.WithContext(ctx).Delete(&object).Error; err != nil {
			return err
		}
	}
	cfg, err := GetMediaRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	rootPath, err := filepath.Abs(cfg.LocalPath)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Duration(cfg.DownloadTimeout+420) * time.Second)
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !(strings.HasPrefix(name, "media_") || strings.HasPrefix(name, "media-output-")) || !(strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".mp4")) {
			continue
		}
		stat, err := entry.Info()
		if err != nil || !stat.Mode().IsRegular() || !stat.ModTime().Before(cutoff) {
			continue
		}
		var count int64
		if err := model.DB.WithContext(ctx).Model(&model.MediaArtifactObject{}).Where("root_path = ? AND object_key = ?", rootPath, name).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if err := model.DB.WithContext(ctx).Model(&model.AsyncMediaJob{}).Where("output_root_path = ? AND output_object_key = ?", rootPath, name).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed++
		if removed >= 100 {
			break
		}
	}
	return nil
}
