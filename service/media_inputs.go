package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var ErrMediaInputUnavailable = errors.New("reference media is unavailable")

var mediaInputDataURIPattern = regexp.MustCompile(`^data:[^;,]+;base64,[A-Za-z0-9+/]+={0,2}$`)

// PrepareAsyncVideoLocalReferences replaces only frozen canonical reference
// slots. Download completion order never changes media order, roles or prompts.
func PrepareAsyncVideoLocalReferences(ctx context.Context, job model.AsyncMediaJob, request AsyncVideoRequest) (AsyncVideoRequest, error) {
	now := time.Now().Unix()
	if request.ReferenceRecoveryVersion != 1 || len(request.ReferenceInputs) == 0 || len(request.ReferenceInputs) > 64 || request.ReferenceRecoveryDeadline <= now || request.ReferenceURLExpiresAt <= now || request.ReferenceURLExpiresAt > job.ExpiresAt || request.ReferenceURLExpiresAt > job.CreatedAt+7*86400 {
		return request, ErrMediaInputUnavailable
	}
	var body map[string]any
	if err := common.Unmarshal(request.Body, &body); err != nil {
		return request, err
	}
	references, ok := body["references"].([]any)
	if !ok || len(references) != len(request.ReferenceInputs) {
		return request, ErrMediaInputUnavailable
	}
	request.ReferenceInputs = append([]AsyncVideoReferenceInput(nil), request.ReferenceInputs...)
	for index, input := range request.ReferenceInputs {
		reference, ok := references[index].(map[string]any)
		if !ok || input.Index != index || reference["type"] != input.Kind || reference["role"] != input.Role || reference["source"] != input.Source || input.Path != "/references/"+strconv.Itoa(index)+"/source" {
			return request, ErrMediaInputUnavailable
		}
	}
	cfg, err := GetMediaRuntimeConfig(ctx)
	if err != nil {
		return request, err
	}
	imageCfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return request, err
	}
	ctx, cancel := context.WithDeadline(ctx, time.Unix(request.ReferenceRecoveryDeadline, 0))
	defer cancel()
	stored, err := model.ListMediaInputObjects(ctx, job.TaskId, job.UserId, job.TokenId)
	if err != nil {
		return request, err
	}
	bySlot := make(map[string]model.MediaInputObject, len(stored))
	for _, object := range stored {
		bySlot[object.Slot] = object
	}
	var remaining atomic.Int64
	remaining.Store(cfg.MaxFileBytes)
	objects := make([]*model.MediaInputObject, len(request.ReferenceInputs))
	client := mediaInputClient(cfg, imageCfg, net.DefaultResolver, (&net.Dialer{Timeout: time.Duration(cfg.DownloadTimeout) * time.Second}).DialContext)
	defer client.CloseIdleConnections()
	// The outer media download pool owns the shared concurrency budget. A
	// serial batch cannot multiply that global limit by its reference count.
	for index, input := range request.ReferenceInputs {
		if err := ctx.Err(); err != nil {
			return request, err
		}
		if strings.HasPrefix(input.Source, "asset://") {
			continue
		}
		if classifyVideoInputSource(input.Source, "") == "data_uri" {
			// Preserve admitted inline data verbatim, including base64 accepted
			// by the provider's syntax without canonical padding or pad bits.
			if !strings.HasPrefix(input.Source, "data:"+input.Kind+"/") || !mediaInputDataURIPattern.MatchString(input.Source) {
				return request, fmt.Errorf("reference %d: %w", index+1, ErrMediaInputUnavailable)
			}
			continue
		}
		local, localErr := verifiedLocalMediaReference(ctx, job, input, request.ReferenceURLExpiresAt)
		if localErr != nil {
			return request, fmt.Errorf("reference %d: %w", index+1, localErr)
		}
		if local {
			continue
		}
		if _, err := validateMediaInputURL(input.Source); err != nil {
			return request, fmt.Errorf("reference %d requires a public HTTP or HTTPS URL", index+1)
		}
		slot := strconv.Itoa(input.Index)
		if object, exists := bySlot[slot]; exists {
			if object.Status != "active" || object.ExpiresAt < request.ReferenceURLExpiresAt || object.Kind != input.Kind || object.SourceHash != ImageIdentityHash(input.Source) || object.ByteSize <= 0 || !reserveMediaInputBytes(&remaining, object.ByteSize) {
				return request, fmt.Errorf("reference %d: %w", index+1, ErrMediaInputUnavailable)
			}
			if err := verifyStoredMediaInput(ctx, object); err != nil {
				return request, fmt.Errorf("reference %d: %w", index+1, err)
			}
			objects[index] = &object
			continue
		}
		object, err := downloadMediaInput(ctx, job, input, cfg, imageCfg, request.ReferenceURLExpiresAt, &remaining, client)
		if err != nil {
			return request, fmt.Errorf("reference %d (%s): %w", index+1, input.Kind, err)
		}
		objects[index] = &object
	}
	for index, object := range objects {
		if object == nil {
			continue
		}
		link, err := MediaInputObjectURL(*object)
		if err != nil {
			return request, err
		}
		references[index].(map[string]any)["source"] = link
		request.ReferenceInputs[index].ObjectId = object.ObjectId
	}
	request.Body, err = common.Marshal(body)
	return request, err
}

func reserveMediaInputBytes(remaining *atomic.Int64, size int64) bool {
	for available := remaining.Load(); size > 0 && size <= available; available = remaining.Load() {
		if remaining.CompareAndSwap(available, available-size) {
			return true
		}
	}
	return false
}

type mediaInputBudgetReader struct {
	reader    io.Reader
	remaining *atomic.Int64
	ctx       context.Context
}

func (reader *mediaInputBudgetReader) Read(target []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := reader.reader.Read(target)
	if n > 0 && !reserveMediaInputBytes(reader.remaining, int64(n)) {
		return 0, errors.New("reference media batch byte limit exceeded")
	}
	return n, err
}

func downloadMediaInput(ctx context.Context, job model.AsyncMediaJob, input AsyncVideoReferenceInput, cfg MediaRuntimeConfig, imageCfg ImageRuntimeConfig, expiresAt int64, remaining *atomic.Int64, client *http.Client) (model.MediaInputObject, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.DownloadTimeout)*time.Second)
	defer cancel()
	urlValue, err := validateMediaInputURL(input.Source)
	if err != nil {
		return model.MediaInputObject{}, ErrMediaInputUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlValue.String(), nil)
	if err != nil {
		return model.MediaInputObject{}, ErrMediaInputUnavailable
	}
	req.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(req)
	if err != nil {
		return model.MediaInputObject{}, errors.New("reference media download failed")
	}
	defer response.Body.Close()
	limit := cfg.MaxFileBytes
	if input.Kind == "image" {
		limit = min(limit, imageCfg.DownloadMaxBytes)
	}
	if response.StatusCode != http.StatusOK || response.ContentLength > limit || response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" {
		return model.MediaInputObject{}, errors.New("reference media response is unavailable or exceeds limits")
	}
	rootPath, err := filepath.Abs(filepath.Join(cfg.LocalPath, "inputs"))
	if err != nil {
		return model.MediaInputObject{}, err
	}
	if err := os.MkdirAll(rootPath, 0700); err != nil {
		return model.MediaInputObject{}, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return model.MediaInputObject{}, err
	}
	defer root.Close()
	objectID := "minput_" + common.GetUUID()
	temporary := objectID + ".part"
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return model.MediaInputObject{}, err
	}
	defer file.Close()
	defer root.Remove(temporary)
	hash := sha256.New()
	reader := &mediaInputBudgetReader{reader: io.LimitReader(response.Body, limit+1), remaining: remaining, ctx: ctx}
	size, err := io.Copy(io.MultiWriter(file, hash), reader)
	if err != nil {
		return model.MediaInputObject{}, err
	}
	if size <= 0 || size > limit || response.ContentLength >= 0 && response.ContentLength != size {
		return model.MediaInputObject{}, errors.New("truncated or oversized reference media")
	}
	contentType, extension, err := ValidateMediaInputFile(ctx, file, size, input.Kind, response.Header.Get("Content-Type"), imageCfg)
	if err != nil {
		return model.MediaInputObject{}, err
	}
	if err := file.Sync(); err != nil {
		return model.MediaInputObject{}, err
	}
	if err := file.Close(); err != nil {
		return model.MediaInputObject{}, err
	}
	key := objectID + extension
	if err := root.Rename(temporary, key); err != nil {
		return model.MediaInputObject{}, err
	}
	object := model.MediaInputObject{ObjectId: objectID, TaskId: job.TaskId, Slot: strconv.Itoa(input.Index), Kind: input.Kind, SourceHash: ImageIdentityHash(input.Source), UserId: job.UserId, TokenId: job.TokenId, RootPath: rootPath, ObjectKey: key, MimeType: contentType, ByteSize: size, Checksum: hex.EncodeToString(hash.Sum(nil)), Status: "active", CreatedAt: time.Now().Unix(), ExpiresAt: expiresAt}
	saved, err := model.SaveMediaInputObject(ctx, object)
	if err != nil || saved.ObjectId != objectID {
		_ = root.Remove(key)
	}
	return saved, err
}

func validateMediaInputURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" || strings.Contains(raw, "\\") || strings.ContainsFunc(raw, unicode.IsSpace) || strings.ContainsFunc(raw, unicode.IsControl) {
		return nil, ErrMediaInputUnavailable
	}
	// Reuse the existing public-address validator without changing the image
	// input contract: only this media-recovery downloader also accepts HTTP.
	checked := *parsed
	checked.Scheme = "https"
	if _, err := ValidateImageReferenceURL(checked.String()); err != nil {
		return nil, ErrMediaInputUnavailable
	}
	return parsed, nil
}

func mediaInputClient(cfg MediaRuntimeConfig, imageCfg ImageRuntimeConfig, resolver imageReferenceResolver, dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	imageCfg.DownloadTimeout = cfg.DownloadTimeout
	client := imageReferenceClient(imageCfg, resolver, dial)
	client.CheckRedirect = func(redirect *http.Request, previous []*http.Request) error {
		if len(previous) > imageCfg.DownloadRedirects {
			return errors.New("reference media redirect limit exceeded")
		}
		if _, err := validateMediaInputURL(redirect.URL.String()); err != nil {
			return err
		}
		clear(redirect.Header)
		redirect.Header.Set("Accept-Encoding", "identity")
		return nil
	}
	return client
}

func verifyStoredMediaInput(ctx context.Context, object model.MediaInputObject) error {
	root, err := os.OpenRoot(object.RootPath)
	if err != nil {
		return ErrMediaInputUnavailable
	}
	defer root.Close()
	file, err := root.Open(object.ObjectKey)
	if err != nil {
		return ErrMediaInputUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != object.ByteSize {
		return ErrMediaInputUnavailable
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, &mediaValidationReader{ctx: ctx, reader: io.LimitReader(file, object.ByteSize+1)}); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != object.Checksum {
		return ErrMediaInputUnavailable
	}
	return nil
}

func mediaInputPublicBase() (string, error) {
	base := strings.TrimSpace(system_setting.TaskPublicAddress)
	if base == "" {
		base = strings.TrimSpace(system_setting.ServerAddress)
	}
	base = strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
	parsed, err := ValidateImageReferenceURL(base + "/")
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("reference media requires a public HTTPS server address")
	}
	return base, nil
}

func mediaInputSignature(object model.MediaInputObject, expires string) []byte {
	mac := hmac.New(sha256.New, []byte(common.CryptoSecret))
	_, _ = mac.Write([]byte("new-api:media-input:v1\x00" + object.ObjectId + "\x00" + object.TaskId + "\x00" + object.Slot + "\x00" + expires))
	return mac.Sum(nil)
}

func MediaInputObjectURL(object model.MediaInputObject) (string, error) {
	base, err := mediaInputPublicBase()
	if err != nil {
		return "", err
	}
	if common.CryptoSecret == "" || object.Status != "active" || object.ExpiresAt <= time.Now().Unix() || object.ExpiresAt > object.CreatedAt+7*86400 {
		return "", ErrMediaInputUnavailable
	}
	expires := strconv.FormatInt(object.ExpiresAt, 10)
	query := url.Values{"expires": {expires}, "access": {base64.RawURLEncoding.EncodeToString(mediaInputSignature(object, expires))}}
	return base + "/api/media-inputs/" + url.PathEscape(object.ObjectId) + "/content?" + query.Encode(), nil
}

// ReadMediaInputObject validates the object-scoped capability and current
// ownership. Upstream fetches do not inherit browser sessions or API IP limits.
func ReadMediaInputObject(ctx context.Context, objectID, expires, access string) (model.MediaInputObject, error) {
	object, err := model.GetMediaInputObject(ctx, objectID)
	if err != nil || common.CryptoSecret == "" || object.Status != "active" {
		return model.MediaInputObject{}, ErrMediaInputUnavailable
	}
	deadline, err := strconv.ParseInt(expires, 10, 64)
	provided, decodeErr := base64.RawURLEncoding.Strict().DecodeString(access)
	if err != nil || decodeErr != nil || len(provided) != sha256.Size || deadline <= time.Now().Unix() || object.ExpiresAt <= time.Now().Unix() || deadline > object.CreatedAt+7*86400 || !hmac.Equal(provided, mediaInputSignature(object, expires)) {
		return model.MediaInputObject{}, ErrMediaInputUnavailable
	}
	var job model.AsyncMediaJob
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ? AND token_id = ? AND expires_at > ? AND status <> ?", object.TaskId, object.UserId, object.TokenId, time.Now().Unix(), "expired").Take(&job).Error; err != nil {
		return model.MediaInputObject{}, ErrMediaInputUnavailable
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", object.TokenId, object.UserId).Take(&token).Error; err != nil || token.Status != common.TokenStatusEnabled && token.Status != common.TokenStatusExhausted || token.ExpiredTime != -1 && token.ExpiredTime <= time.Now().Unix() {
		return model.MediaInputObject{}, ErrMediaInputUnavailable
	}
	if model.IsCanvasTokenKey(token.Key) {
		if _, _, _, err := ValidateCanvasWorkerCredential(ctx, token.Id, object.UserId); err != nil {
			return model.MediaInputObject{}, ErrMediaInputUnavailable
		}
	} else {
		var owner model.User
		if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", object.UserId, common.UserStatusEnabled).Take(&owner).Error; err != nil {
			return model.MediaInputObject{}, ErrMediaInputUnavailable
		}
	}
	return object, nil
}

func ServeMediaInputObject(c *gin.Context, object model.MediaInputObject) error {
	root, err := os.OpenRoot(object.RootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.Open(object.ObjectKey)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != object.ByteSize {
		return ErrMediaInputUnavailable
	}
	c.Header("Content-Type", object.MimeType)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.Header("Content-Disposition", "inline; filename=\""+object.ObjectId+filepath.Ext(object.ObjectKey)+"\"")
	c.Header("ETag", "\""+object.Checksum+"\"")
	http.ServeContent(c.Writer, c.Request, object.ObjectKey, info.ModTime(), file)
	c.Writer.WriteHeaderNow()
	return nil
}

func verifiedLocalMediaReference(ctx context.Context, job model.AsyncMediaJob, input AsyncVideoReferenceInput, requiredExpiry int64) (bool, error) {
	parsed, err := validateMediaInputURL(input.Source)
	if err != nil {
		return false, err
	}
	if input.Kind == "image" {
		bound, err := IsBoundImageReference(ctx, job.TokenId, input.Source)
		if err != nil {
			return true, ErrMediaInputUnavailable
		}
		if bound {
			var alias model.ImageInputAlias
			var imageInput model.ImageInputObject
			var object model.ImageStorageObject
			if model.DB.WithContext(ctx).Where("url_hash = ? AND token_id = ?", ImageIdentityHash(input.Source), job.TokenId).Take(&alias).Error != nil || model.DB.WithContext(ctx).Where("input_id = ? AND token_id = ? AND status = ? AND expires_at >= ?", alias.InputId, job.TokenId, "active", requiredExpiry).Take(&imageInput).Error != nil || model.DB.WithContext(ctx).Where("object_id = ? AND status = ? AND (expires_at = 0 OR expires_at >= ?)", imageInput.ObjectId, "active", requiredExpiry).Take(&object).Error != nil {
				return true, ErrMediaInputUnavailable
			}
			query := parsed.Query()
			deadline, _ := strconv.ParseInt(query.Get("expires"), 10, 64)
			if strings.Contains(parsed.Path, "/api/image-objects/") {
				identity := strings.TrimSuffix(parsed.Path[strings.LastIndex(parsed.Path, "/api/image-objects/")+len("/api/image-objects/"):], "/content")
				if identity != object.ObjectId || !VerifyImageObjectSignature(identity, query.Get("expires"), query.Get("key"), query.Get("signature")) {
					return true, ErrMediaInputUnavailable
				}
			} else {
				issued, issuedErr := time.Parse("20060102T150405Z", query.Get("X-Amz-Date"))
				seconds, secondsErr := strconv.ParseInt(query.Get("X-Amz-Expires"), 10, 64)
				if issuedErr != nil || secondsErr != nil || seconds <= 0 || seconds > 604800 || query.Get("X-Amz-Signature") == "" {
					return true, ErrMediaInputUnavailable
				}
				deadline = issued.Unix() + seconds
			}
			if deadline < requiredExpiry {
				return true, ErrMediaInputUnavailable
			}
			return true, nil
		}
	}
	base, err := mediaInputPublicBase()
	if err != nil {
		return false, err
	}
	origin, _ := url.Parse(base)
	port, originPort := parsed.Port(), origin.Port()
	if port == "" {
		port = "443"
	}
	if originPort == "" {
		originPort = "443"
	}
	if !strings.EqualFold(parsed.Scheme, origin.Scheme) || !strings.EqualFold(parsed.Hostname(), origin.Hostname()) || port != originPort {
		return false, nil
	}
	artifactPrefix := strings.TrimRight(origin.Path, "/") + "/v1/media/objects/"
	if identity, found := strings.CutPrefix(parsed.Path, artifactPrefix); found {
		var object model.MediaArtifactObject
		deadline, _ := strconv.ParseInt(parsed.Query().Get("expires"), 10, 64)
		if identity == "" || strings.Contains(identity, "/") || input.Kind != "video" || deadline < requiredExpiry || model.DB.WithContext(ctx).Where("object_id = ? AND user_id = ? AND token_id = ? AND status = ? AND expires_at >= ?", identity, job.UserId, job.TokenId, "active", requiredExpiry).Take(&object).Error != nil || !VerifyMediaObjectAccess(object, parsed.Query().Get("expires"), parsed.Query().Get("access")) {
			return true, ErrMediaInputUnavailable
		}
		return true, nil
	}
	prefix := strings.TrimRight(origin.Path, "/") + "/api/media-inputs/"
	identity, valid := strings.CutPrefix(parsed.Path, prefix)
	identity, validEnd := strings.CutSuffix(identity, "/content")
	if !valid || !validEnd || strings.Contains(identity, "/") {
		return true, ErrMediaInputUnavailable
	}
	object, err := ReadMediaInputObject(ctx, identity, parsed.Query().Get("expires"), parsed.Query().Get("access"))
	deadline, _ := strconv.ParseInt(parsed.Query().Get("expires"), 10, 64)
	if err != nil || object.UserId != job.UserId || object.TokenId != job.TokenId || object.Kind != input.Kind || object.ExpiresAt < requiredExpiry || deadline < requiredExpiry {
		return true, ErrMediaInputUnavailable
	}
	return true, nil
}

func CleanupMediaInputs(ctx context.Context, now int64) error {
	objects, err := model.GetExpiredMediaInputObjects(ctx, now, 100)
	if err != nil {
		return err
	}
	for _, object := range objects {
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
		if err := model.DeleteMediaInputObject(ctx, object); err != nil {
			return err
		}
	}
	cfg, err := GetMediaRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	rootPath, err := filepath.Abs(filepath.Join(cfg.LocalPath, "inputs"))
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
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	cutoff, removed := time.Unix(now-int64(cfg.DownloadTimeout+420), 0), 0
	for removed < 100 {
		entries, readErr := directory.ReadDir(100)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			if !strings.HasPrefix(name, "minput_") || !slices.Contains([]string{".part", ".jpg", ".png", ".webp", ".mp4", ".mov", ".webm", ".mp3", ".wav", ".flac", ".m4a", ".aac", ".ogg", ".aiff"}, filepath.Ext(name)) {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
				continue
			}
			var count int64
			if err := model.DB.WithContext(ctx).Model(&model.MediaInputObject{}).Where("root_path = ? AND object_key = ?", rootPath, name).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				continue
			}
			if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			removed++
			if removed == 100 {
				break
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return nil
}

// RetireMediaInputObjects keeps capability URLs usable for one hour after a
// confirmed terminal generation. Unknown upstream outcomes retain hard expiry.
func RetireMediaInputObjects(ctx context.Context, task *model.Task) error {
	if task == nil || task.Status != model.TaskStatusSuccess && task.Status != model.TaskStatusFailure {
		return nil
	}
	var persisted model.Task
	if err := model.DB.WithContext(ctx).Where("id = ? AND task_id = ? AND user_id = ? AND status = ? AND attempt_generation = ?", task.ID, task.TaskID, task.UserId, task.Status, task.AttemptGeneration).Take(&persisted).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if persisted.AttemptGeneration > 0 && persisted.Status == model.TaskStatusFailure && persisted.PrivateData.UpstreamTaskID != "" && !persisted.PrivateData.VideoTerminalConfirmed {
		// A local polling cutoff or deadline does not prove that an accepted
		// upstream operation has finished fetching its queued reference media.
		return nil
	}
	var job model.AsyncMediaJob
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ? AND attempt_generation = ?", task.TaskID, task.UserId, task.AttemptGeneration).Take(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	grace := time.Now().Unix() + 3600
	terminal := model.DB.Model(&model.Task{}).Select("1").Where("id = ? AND status = ? AND attempt_generation = ?", task.ID, task.Status, task.AttemptGeneration)
	generation := model.DB.Model(&model.AsyncMediaJob{}).Select("1").Where("id = ? AND attempt_generation = ?", job.ID, task.AttemptGeneration)
	return model.DB.WithContext(ctx).Model(&model.MediaInputObject{}).
		Where("task_id = ? AND user_id = ? AND token_id = ? AND expires_at > ?", job.TaskId, job.UserId, job.TokenId, grace).
		Where("EXISTS (?) AND EXISTS (?)", terminal, generation).
		Update("expires_at", grace).Error
}
