package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrImageObjectMissing = errors.New("image storage object is missing")

type ImageStorageCredentials struct {
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	SessionToken string `json:"session_token,omitempty"`
}

// ImageStorage resolves immutable configuration revisions. The local backend
// never exposes a filesystem path, and S3 clients never use environment credentials.
type ImageStorage struct {
	Profile model.ImageStorageProfile
	Client  *s3.Client
}

func OpenImageStorage(ctx context.Context, profile model.ImageStorageProfile) (*ImageStorage, error) {
	store := &ImageStorage{Profile: profile}
	if profile.Backend == "local" {
		if !filepath.IsAbs(profile.Root) || strings.TrimSpace(profile.Root) == "" {
			return nil, errors.New("local image storage requires an absolute directory")
		}
		if err := os.MkdirAll(profile.Root, 0700); err != nil {
			return nil, err
		}
		root, err := filepath.EvalSymlinks(profile.Root)
		if err != nil {
			return nil, err
		}
		store.Profile.Root = root
		return store, nil
	}
	if profile.Backend != "s3" || profile.Bucket == "" || profile.Region == "" {
		return nil, errors.New("invalid image storage backend or bucket")
	}
	if profile.Endpoint != "" {
		endpoint, err := url.Parse(profile.Endpoint)
		if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return nil, errors.New("S3 endpoint requires HTTPS without credentials or query")
		}
	}
	data, err := DecryptImagePayload(profile.CredentialCipher, "storage:"+profile.ProfileId)
	if err != nil {
		return nil, err
	}
	var credential ImageStorageCredentials
	if err := common.Unmarshal(data, &credential); err != nil {
		return nil, err
	}
	if credential.AccessKey == "" || credential.SecretKey == "" {
		return nil, errors.New("S3 image storage credentials are required")
	}
	store.Client = s3.NewFromConfig(aws.Config{Region: profile.Region, Credentials: credentials.NewStaticCredentialsProvider(credential.AccessKey, credential.SecretKey, credential.SessionToken), HTTPClient: &http.Client{Timeout: 60 * time.Second}, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired}, func(options *s3.Options) {
		options.UsePathStyle = profile.PathStyle
		if profile.Endpoint != "" {
			options.BaseEndpoint = aws.String(strings.TrimSuffix(profile.Endpoint, "/"))
		}
	})
	return store, nil
}

func GetImageStorage(ctx context.Context, class string) (*ImageStorage, error) {
	var profile model.ImageStorageProfile
	if err := model.DB.WithContext(ctx).Where("class = ? AND active = ?", class, true).Order("id DESC").Take(&profile).Error; err != nil {
		return nil, err
	}
	return OpenImageStorage(ctx, profile)
}

func EnsureDefaultImageStorage(ctx context.Context, deploymentDirectory string) error {
	if !filepath.IsAbs(deploymentDirectory) {
		return errors.New("image deployment directory must be absolute")
	}
	for _, class := range []string{"temporary", "durable"} {
		var count int64
		if err := model.DB.WithContext(ctx).Model(&model.ImageStorageProfile{}).Where("class = ?", class).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if _, err := SaveImageStorageProfile(ctx, model.ImageStorageProfile{Class: class, Backend: "local", Provider: "local", Root: filepath.Join(deploymentDirectory, "images", class), Active: true}, nil); err != nil {
			return err
		}
	}
	return nil
}

func GetImageObjectStorage(ctx context.Context, object model.ImageStorageObject) (*ImageStorage, error) {
	var profile model.ImageStorageProfile
	if err := model.DB.WithContext(ctx).Where("profile_id = ?", object.ProfileId).Take(&profile).Error; err != nil {
		return nil, err
	}
	return OpenImageStorage(ctx, profile)
}

func (store *ImageStorage) LocalPath(key string, createParent bool) (string, error) {
	if key == "" || path.IsAbs(key) || path.Clean(key) != key || strings.ContainsAny(key, "\\:\x00") || key == ".." || strings.HasPrefix(key, "../") {
		return "", errors.New("invalid image object key")
	}
	root := store.Profile.Root
	target := filepath.Join(root, filepath.FromSlash(key))
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("image object key escaped storage root")
	}
	parent := root
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, component := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, component)
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) && createParent {
			err = os.Mkdir(parent, 0700)
			if errors.Is(err, os.ErrExist) {
				err = nil
			}
			if err == nil {
				info, err = os.Lstat(parent)
			}
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("image storage contains an unsafe directory")
		}
	}
	if info, err := os.Lstat(target); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return "", errors.New("image storage target is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return target, nil
}

func (store *ImageStorage) Key(key string) string { return path.Join(store.Profile.Prefix, key) }

func (store *ImageStorage) Read(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("invalid image read limit")
	}
	var reader io.ReadCloser
	if store.Profile.Backend == "local" {
		filename, err := store.LocalPath(key, false)
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrImageObjectMissing
		}
		if err != nil {
			return nil, err
		}
		file, err := os.Open(filename)
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrImageObjectMissing
		}
		if err != nil {
			return nil, err
		}
		reader = file
	} else {
		output, err := store.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(store.Profile.Bucket), Key: aws.String(store.Key(key))})
		if err != nil {
			var apiError smithy.APIError
			if errors.As(err, &apiError) && (apiError.ErrorCode() == "NoSuchKey" || apiError.ErrorCode() == "NotFound") {
				return nil, ErrImageObjectMissing
			}
			return nil, err
		}
		reader = output.Body
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("stored image exceeds read limit")
	}
	return data, nil
}

// Confirm verifies physical bytes, not an ETag or caller-controlled metadata.
func (store *ImageStorage) Confirm(ctx context.Context, key string, image ImageBytes) error {
	if store.Profile.Backend == "s3" {
		output, err := store.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(store.Profile.Bucket), Key: aws.String(store.Key(key))})
		if err != nil {
			return err
		}
		if aws.ToInt64(output.ContentLength) != int64(len(image.Data)) {
			return model.ErrImageConflict
		}
	}
	data, err := store.Read(ctx, key, int64(len(image.Data)))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if len(data) != len(image.Data) || hex.EncodeToString(sum[:]) != image.Checksum {
		return model.ErrImageConflict
	}
	return nil
}

func (store *ImageStorage) Put(ctx context.Context, key string, image ImageBytes) error {
	existing, err := store.Read(ctx, key, int64(len(image.Data)))
	if err == nil {
		sum := sha256.Sum256(existing)
		if len(existing) != len(image.Data) || hex.EncodeToString(sum[:]) != image.Checksum {
			return model.ErrImageConflict
		}
		return store.Confirm(ctx, key, image)
	}
	if !errors.Is(err, ErrImageObjectMissing) {
		return err
	}
	if store.Profile.Backend == "local" {
		filename, err := store.LocalPath(key, true)
		if err != nil {
			return err
		}
		temporary, err := os.CreateTemp(filepath.Dir(filename), ".image-*")
		if err != nil {
			return err
		}
		name := temporary.Name()
		defer os.Remove(name)
		_, writeErr := temporary.Write(image.Data)
		if writeErr == nil {
			writeErr = temporary.Sync()
		}
		closeErr := temporary.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		// Linking publishes a complete file without overwriting a concurrent upload.
		if err := os.Link(name, filename); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	} else {
		_, err := store.Client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(store.Profile.Bucket), Key: aws.String(store.Key(key)), Body: bytes.NewReader(image.Data), ContentType: aws.String(image.ContentType), ContentLength: aws.Int64(int64(len(image.Data))), Metadata: map[string]string{"sha256": image.Checksum}})
		if err != nil {
			return err
		}
	}
	return store.Confirm(ctx, key, image)
}

func (store *ImageStorage) Delete(ctx context.Context, key string) error {
	if store.Profile.Backend == "local" {
		filename, err := store.LocalPath(key, false)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := os.Remove(filename); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		if _, err := store.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(store.Profile.Bucket), Key: aws.String(store.Key(key))}); err != nil {
			return err
		}
	}
	_, err := store.Read(ctx, key, 1)
	if errors.Is(err, ErrImageObjectMissing) {
		return nil
	}
	if err == nil {
		return errors.New("deleted image still exists")
	}
	return err
}

func SaveImageStorageProfile(ctx context.Context, profile model.ImageStorageProfile, credential *ImageStorageCredentials) (model.ImageStorageProfile, error) {
	if profile.Class != "temporary" && profile.Class != "durable" {
		return profile, errors.New("invalid image storage class")
	}
	if profile.Backend == "local" && profile.Root == "" {
		executable, err := os.Executable()
		if err != nil {
			return profile, err
		}
		profile.Root = filepath.Join(filepath.Dir(executable), "images", profile.Class)
	}
	if profile.Backend == "s3" && profile.Endpoint == "" {
		switch profile.Provider {
		case "aliyun":
			profile.Endpoint = "https://oss-" + strings.TrimPrefix(profile.Region, "oss-") + ".aliyuncs.com"
		case "tencent":
			profile.Endpoint = "https://cos." + profile.Region + ".myqcloud.com"
		case "qiniu":
			profile.Endpoint = "https://s3." + profile.Region + ".qiniucs.com"
		case "aws":
		default:
			return profile, errors.New("an explicit HTTPS endpoint is required for this storage provider")
		}
	}
	if profile.Prefix != "" && (path.Clean(profile.Prefix) != profile.Prefix || strings.HasPrefix(profile.Prefix, "/") || strings.Contains(profile.Prefix, "..") || strings.ContainsAny(profile.Prefix, "\\:\x00")) {
		return profile, errors.New("invalid image storage prefix")
	}
	if profile.Provider == "superbed" {
		return profile, errors.New("Superbed is not supported")
	}
	profile.Id = 0
	profile.ProfileId = "isp_" + common.GetUUID()
	profile.CreatedAt = time.Now().Unix()
	if credential != nil {
		data, err := common.Marshal(credential)
		if err != nil {
			return profile, err
		}
		profile.CredentialCipher, err = EncryptImagePayload(data, "storage:"+profile.ProfileId)
		if err != nil {
			return profile, err
		}
	}
	if _, err := OpenImageStorage(ctx, profile); err != nil {
		return profile, err
	}
	err := model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if profile.Active {
			if err := tx.Model(&model.ImageStorageProfile{}).Where("class = ? AND active = ?", profile.Class, true).Update("active", false).Error; err != nil {
				return err
			}
		}
		return tx.Create(&profile).Error
	})
	return profile, err
}

func ImageObjectURL(ctx context.Context, object model.ImageStorageObject, base string, seconds int) (string, error) {
	if object.Status != "active" {
		return "", ErrImageObjectMissing
	}
	if object.ExpiresAt > 0 {
		seconds = min(seconds, int(object.ExpiresAt-time.Now().Unix()))
	}
	if seconds <= 0 {
		return "", ErrImageObjectMissing
	}
	store, err := GetImageObjectStorage(ctx, object)
	if err != nil {
		return "", err
	}
	if store.Profile.Backend == "s3" {
		output, err := s3.NewPresignClient(store.Client).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(store.Profile.Bucket), Key: aws.String(store.Key(object.ObjectKey))}, s3.WithPresignExpires(time.Duration(min(seconds, 604800))*time.Second))
		if err != nil {
			return "", err
		}
		return output.URL, nil
	}
	keys, active, err := imagePayloadKeys()
	if err != nil {
		return "", err
	}
	expires := strconv.FormatInt(time.Now().Unix()+int64(seconds), 10)
	mac := hmac.New(sha256.New, keys[active])
	_, _ = mac.Write([]byte("new-api:image-view:" + object.ObjectId + ":" + expires))
	return strings.TrimSuffix(base, "/") + "/api/image-objects/" + url.PathEscape(object.ObjectId) + "/content?expires=" + expires + "&key=" + url.QueryEscape(active) + "&signature=" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifyImageObjectSignature(objectId, expires, keyId, signature string) bool {
	timestamp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || timestamp <= time.Now().Unix() {
		return false
	}
	keys, _, err := imagePayloadKeys()
	if err != nil || keys[keyId] == nil {
		return false
	}
	value, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, keys[keyId])
	_, _ = mac.Write([]byte("new-api:image-view:" + objectId + ":" + expires))
	return hmac.Equal(value, mac.Sum(nil))
}

// StoreImageIntent records the deterministic identity before external I/O.
// Every retry uses that same profile/key and confirms its complete bytes.
func StoreImageIntent(ctx context.Context, intent model.ImageUploadIntent, image ImageBytes, expiresAt int64) (model.ImageStorageObject, error) {
	result := model.DB.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "intent_key"}}, DoNothing: true}).Create(&intent)
	if result.Error != nil {
		return model.ImageStorageObject{}, result.Error
	}
	var saved model.ImageUploadIntent
	if err := model.DB.WithContext(ctx).Where("intent_key = ?", intent.IntentKey).Take(&saved).Error; err != nil {
		return model.ImageStorageObject{}, err
	}
	if saved.Checksum != image.Checksum || saved.ByteSize != int64(len(image.Data)) || saved.ProfileId != intent.ProfileId || saved.ObjectKey != intent.ObjectKey {
		return model.ImageStorageObject{}, model.ErrImageConflict
	}
	// A confirmed object is immutable. Reuse it without taking an upload lease
	// or writing again, including concurrent deferred publication syncs.
	if saved.Status == "confirmed" {
		var existing model.ImageStorageObject
		err := model.DB.WithContext(ctx).Where("identity_hash = ? AND status = ?", ImageIdentityHash(saved.ProfileId, saved.ObjectKey), "active").Take(&existing).Error
		if err != nil {
			return existing, err
		}
		if existing.Checksum != image.Checksum || existing.ByteSize != int64(len(image.Data)) {
			return existing, model.ErrImageConflict
		}
		store, err := GetImageObjectStorage(ctx, existing)
		if err != nil {
			return existing, err
		}
		data, err := store.Read(ctx, existing.ObjectKey, int64(len(image.Data)))
		if err != nil {
			return existing, err
		}
		checksum := sha256.Sum256(data)
		if hex.EncodeToString(checksum[:]) != existing.Checksum {
			return existing, model.ErrImageConflict
		}
		return existing, nil
	}
	lease := common.GetUUID()
	claim := model.DB.WithContext(ctx).Model(&model.ImageUploadIntent{}).Where("id = ? AND status IN ? AND lease_expires_at <= ?", saved.Id, []string{"pending", "confirmed", "uploading"}, time.Now().Unix()).Updates(map[string]any{"status": "uploading", "lease_token": lease, "lease_expires_at": time.Now().Unix() + 720})
	if claim.Error != nil {
		return model.ImageStorageObject{}, claim.Error
	}
	if claim.RowsAffected != 1 {
		return model.ImageStorageObject{}, model.ErrImageConflict
	}
	object := model.ImageStorageObject{ObjectId: "imo_" + ImageIdentityHash(saved.IntentKey)[:32], IdentityHash: ImageIdentityHash(saved.ProfileId, saved.ObjectKey), ProfileId: saved.ProfileId, Class: saved.Class, ObjectKey: saved.ObjectKey, ContentType: image.ContentType, ByteSize: int64(len(image.Data)), Checksum: image.Checksum, Width: image.Width, Height: image.Height, Status: "active", CreatedAt: saved.CreatedAt, ExpiresAt: expiresAt}
	store, err := GetImageObjectStorage(ctx, object)
	if err != nil {
		return object, err
	}
	if err := store.Put(ctx, saved.ObjectKey, image); err != nil {
		return object, err
	}
	err = model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		confirmation := tx.Model(&model.ImageUploadIntent{}).Where("id = ? AND lease_token = ? AND status = ?", saved.Id, lease, "uploading").Updates(map[string]any{"status": "confirmed", "lease_token": "", "lease_expires_at": 0})
		if confirmation.Error != nil {
			return confirmation.Error
		}
		if confirmation.RowsAffected != 1 {
			return model.ErrImageConflict
		}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "identity_hash"}}, DoNothing: true}).Create(&object).Error; err != nil {
			return err
		}
		var existing model.ImageStorageObject
		if err := tx.Where("identity_hash = ?", object.IdentityHash).Take(&existing).Error; err != nil {
			return err
		}
		if existing.Checksum != object.Checksum || existing.Status != "active" {
			return model.ErrImageConflict
		}
		object = existing
		return nil
	})
	return object, err
}

func ImageResultObjectKey(task model.AsyncImageTask, index int, contentType string) string {
	extension := ".png"
	if contentType == "image/jpeg" {
		extension = ".jpg"
	} else if contentType == "image/webp" {
		extension = ".webp"
	}
	date := time.Unix(task.CreatedAt, 0).UTC()
	return fmt.Sprintf("results/%s/%s_%s%s", date.Format("2006/01/02"), date.Format("20060102150405"), ImageIdentityHash(task.TaskId, strconv.Itoa(index))[:32], extension)
}
