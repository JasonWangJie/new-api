package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"gorm.io/gorm"
)

var ErrMediaRetestReferenceUnavailable = errors.New("original reference media is unavailable for retest")
var ErrMediaRetestProviderAsset = errors.New("original provider-owned assets cannot be used with the operator's token")

type MediaRetestRequest struct {
	Path                string
	ContentType         string
	Body                []byte
	VideoReferencePaths []jsplugin.VideoRetryInput `json:"video_reference_paths,omitempty"`
	RetestKey           string                     `json:"-"`
}

type MediaRetestSource struct {
	TaskID    string
	UserID    int
	TokenID   int
	MediaType string
	Model     string
	ChannelID int
}

// SelectMediaRetestModel replaces only the public model and the provider pin.
// Multipart files and the ordering of repeated material fields are preserved.
func SelectMediaRetestModel(request MediaRetestRequest, publicModel string) (MediaRetestRequest, error) {
	mediaType, params, err := mime.ParseMediaType(request.ContentType)
	if err != nil {
		return request, err
	}
	switch mediaType {
	case "application/json":
		var fields map[string]common.RawMessage
		if err := common.Unmarshal(request.Body, &fields); err != nil || fields == nil {
			return request, ErrMediaRetestReferenceUnavailable
		}
		value, err := common.Marshal(publicModel)
		if err != nil {
			return request, err
		}
		for name := range fields {
			if strings.EqualFold(name, "model") || strings.EqualFold(name, "provider") {
				delete(fields, name)
			}
		}
		fields["model"] = value
		request.Body, err = common.Marshal(fields)
		return request, err
	case "multipart/form-data":
		var output bytes.Buffer
		writer := multipart.NewWriter(&output)
		if err := writer.SetBoundary(params["boundary"]); err != nil {
			return request, err
		}
		reader := multipart.NewReader(bytes.NewReader(request.Body), params["boundary"])
		for {
			part, err := reader.NextRawPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return request, err
			}
			if part.FileName() == "" && (part.FormName() == "provider" || part.FormName() == "model") {
				continue
			}
			destination, err := writer.CreatePart(part.Header)
			if err != nil {
				return request, err
			}
			if _, err := io.Copy(destination, part); err != nil {
				return request, err
			}
		}
		if err := writer.WriteField("model", publicModel); err != nil {
			return request, err
		}
		if err := writer.Close(); err != nil {
			return request, err
		}
		request.Body = output.Bytes()
		return request, nil
	default:
		return request, ErrMediaRetestReferenceUnavailable
	}
}

func MediaRetestVideoReferencePaths(plugin *jsplugin.LoadedPlugin) []jsplugin.VideoRetryInput {
	if plugin == nil {
		return nil
	}
	paths, _ := videoRetryInputSelectors(plugin)
	return slices.Clone(paths)
}

type mediaRetestReference struct {
	kind  string
	value string
}

type mediaRetestPathPart struct {
	value string
	index bool
}

type mediaRetestPreparation struct {
	source      MediaRetestSource
	target      model.Token
	retestKey   string
	videoPaths  []jsplugin.VideoRetryInput
	imageConfig ImageRuntimeConfig
	mediaConfig MediaRuntimeConfig
	remaining   int64
	count       int
	checked     map[mediaRetestReference]string
	pinAliases  map[string]bool
	readOnly    bool
}

// CollectMediaRetestVideoInputPins discovers existing owned SC aliases only.
// Admission locks the resulting input rows before storing replay references.
func CollectMediaRetestVideoInputPins(ctx context.Context, tokenID int, request MediaRetestRequest) ([]string, error) {
	if tokenID <= 0 {
		return nil, ErrMediaRetestReferenceUnavailable
	}
	mediaType, params, err := mime.ParseMediaType(request.ContentType)
	if err != nil {
		return nil, err
	}
	preparation := mediaRetestPreparation{videoPaths: request.VideoReferencePaths, remaining: int64(len(request.Body)), pinAliases: make(map[string]bool)}
	switch mediaType {
	case "application/json":
		_, err = preparation.rewriteJSON(ctx, request.Body, nil, "", "", 0)
	case "multipart/form-data":
		_, err = preparation.rewriteMultipart(ctx, request.Body, params["boundary"])
	}
	if err != nil || len(preparation.pinAliases) == 0 {
		return nil, err
	}
	var inputIDs []string
	err = model.DB.WithContext(ctx).Model(&model.ImageInputAlias{}).
		Joins("JOIN image_input_objects ON image_input_objects.input_id = image_input_aliases.input_id AND image_input_objects.token_id = image_input_aliases.token_id").
		Where("image_input_aliases.url_hash IN ? AND image_input_aliases.token_id = ? AND image_input_objects.status = ? AND image_input_objects.expires_at > ?", slices.Collect(maps.Keys(preparation.pinAliases)), tokenID, "active", time.Now().Unix()).
		Distinct().Order("image_input_aliases.input_id").Pluck("image_input_aliases.input_id", &inputIDs).Error
	return inputIDs, err
}

// PrepareMediaRetest updates only media slots in the original client request.
// Protocol validation, routing, and payment still run through ordinary admission.
func PrepareMediaRetest(ctx context.Context, source MediaRetestSource, target model.Token, request MediaRetestRequest) (MediaRetestRequest, error) {
	return prepareMediaRetest(ctx, source, target, request, false)
}

// ValidateMediaRetestReferences checks retained reference ownership and local
// availability. Admission rechecks and copies references for the operator.
func ValidateMediaRetestReferences(ctx context.Context, source MediaRetestSource, target model.Token, request MediaRetestRequest) error {
	_, err := prepareMediaRetest(ctx, source, target, request, true)
	return err
}

func prepareMediaRetest(ctx context.Context, source MediaRetestSource, target model.Token, request MediaRetestRequest, readOnly bool) (MediaRetestRequest, error) {
	if source.TaskID == "" || source.UserID <= 0 || source.TokenID <= 0 || target.Id <= 0 || target.UserId <= 0 || request.RetestKey == "" && !readOnly {
		return MediaRetestRequest{}, ErrMediaRetestReferenceUnavailable
	}
	if source.MediaType == "" {
		source.MediaType = "image"
		if strings.Contains(request.Path, "/videos/") {
			source.MediaType = "video"
		}
	}
	imageConfig, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return MediaRetestRequest{}, err
	}
	mediaConfig, err := GetMediaRuntimeConfig(ctx)
	if err != nil {
		return MediaRetestRequest{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(max(mediaConfig.DownloadTimeout, imageConfig.DownloadTimeout))*time.Second)
	defer cancel()
	preparation := mediaRetestPreparation{
		source: source, target: target, retestKey: request.RetestKey, videoPaths: request.VideoReferencePaths,
		imageConfig: imageConfig, mediaConfig: mediaConfig, remaining: mediaConfig.MaxFileBytes,
		checked: make(map[mediaRetestReference]string), readOnly: readOnly,
	}
	mediaType, params, err := mime.ParseMediaType(request.ContentType)
	if err != nil {
		return MediaRetestRequest{}, ErrMediaRetestReferenceUnavailable
	}
	switch mediaType {
	case "application/json":
		if !bytes.HasPrefix(bytes.TrimSpace(request.Body), []byte("{")) {
			return MediaRetestRequest{}, ErrMediaRetestReferenceUnavailable
		}
		request.Body, err = preparation.rewriteJSON(ctx, request.Body, nil, "", "", 0)
	case "multipart/form-data":
		request.Body, err = preparation.rewriteMultipart(ctx, request.Body, params["boundary"])
	default:
		err = ErrMediaRetestReferenceUnavailable
	}
	if err != nil {
		return MediaRetestRequest{}, err
	}
	return request, nil
}

func (preparation *mediaRetestPreparation) rewriteMultipart(ctx context.Context, body []byte, boundary string) ([]byte, error) {
	var output bytes.Buffer
	var destination io.Writer = &output
	if preparation.pinAliases != nil {
		destination = io.Discard
	}
	writer := multipart.NewWriter(destination)
	if err := writer.SetBoundary(boundary); err != nil {
		return nil, ErrMediaRetestReferenceUnavailable
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, ErrMediaRetestReferenceUnavailable
		}
		out, err := writer.CreatePart(part.Header)
		if err != nil {
			return nil, err
		}
		if part.FileName() != "" {
			written, err := io.Copy(out, io.LimitReader(part, preparation.remaining+1))
			if err != nil || written > preparation.remaining {
				return nil, ErrMediaRetestReferenceUnavailable
			}
			preparation.remaining -= written
			continue
		}
		value, err := io.ReadAll(part)
		if err != nil {
			return nil, ErrMediaRetestReferenceUnavailable
		}
		name := part.FormName()
		key := normalizeMediaRetestField(name)
		kind := mediaRetestKind(key)
		if preparation.pinAliases == nil && mediaRetestAccountField(key) && len(bytes.TrimSpace(value)) > 0 {
			return nil, ErrMediaRetestProviderAsset
		}
		trimmed := bytes.TrimSpace(value)
		if (kind != "" || key == "metadata" || key == "references" || key == "content") && len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			value, err = preparation.rewriteJSON(ctx, value, []mediaRetestPathPart{{value: name}}, key, kind, 0)
		} else if kind != "" {
			if preparation.pinAliases == nil && key == "referenceaudios" && classifyVideoInputSource(string(value), "reference_audios") == "voice_id" {
				return nil, ErrMediaRetestProviderAsset
			}
			var replacement string
			replacement, err = preparation.reference(ctx, kind, string(value))
			value = []byte(replacement)
		}
		if err != nil {
			return nil, err
		}
		if _, err := out.Write(value); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func normalizeMediaRetestField(field string) string {
	return strings.NewReplacer("_", "", "-", "", "[", "", "]", "").Replace(strings.ToLower(field))
}

func mediaRetestAccountField(field string) bool {
	return slices.Contains([]string{"assetid", "assetids", "fileid", "fileids", "voiceid", "voiceids", "sourcetaskid"}, field)
}

func mediaRetestKind(field string) string {
	switch field {
	case "image", "images", "imageurl", "imageurls", "imgurl", "inputimage", "inputimages", "inputreference", "referenceimage", "referenceimages", "firstimage", "lastimage", "firstframe", "lastframe", "firstframeimage", "lastframeimage", "firstframeurl", "lastframeurl", "startframe", "endframe", "imagetail", "mask", "maskurl":
		return "image"
	case "video", "videos", "videourl", "videourls", "inputvideo", "inputvideos", "referencevideo", "referencevideos", "videoreference", "firstclip":
		return "video"
	case "audio", "audios", "audiourl", "audiourls", "inputaudio", "inputaudios", "referenceaudio", "referenceaudios", "audioreferences", "drivingaudio":
		return "audio"
	default:
		return ""
	}
}

func mediaRetestObjectKind(object map[string]common.RawMessage) string {
	for _, field := range []string{"type", "mimeType", "mime_type", "media_type", "mediaType"} {
		var value string
		if common.Unmarshal(object[field], &value) != nil {
			continue
		}
		if kind := mediaRetestKind(normalizeMediaRetestField(value)); kind != "" {
			return kind
		}
		if kind, _, ok := strings.Cut(value, "/"); ok && slices.Contains([]string{"image", "video", "audio"}, kind) {
			return kind
		}
	}
	return ""
}

func mediaRetestStandardPath(path []mediaRetestPathPart) bool {
	for _, part := range path {
		if part.index {
			continue
		}
		field := normalizeMediaRetestField(part.value)
		if mediaRetestKind(field) == "" && !slices.Contains([]string{"messages", "contents", "content", "parts", "input", "references", "metadata", "parameters", "source", "url", "filedata", "inlinedata", "data"}, field) {
			return false
		}
	}
	return true
}

func (preparation *mediaRetestPreparation) rewriteJSON(ctx context.Context, input []byte, path []mediaRetestPathPart, field, kind string, depth int) ([]byte, error) {
	if depth > 32 {
		if preparation.pinAliases != nil {
			return input, nil
		}
		return nil, ErrMediaRetestReferenceUnavailable
	}
	value := bytes.TrimSpace(input)
	if len(value) == 0 {
		return nil, ErrMediaRetestReferenceUnavailable
	}
	switch value[0] {
	case '{':
		var object map[string]common.RawMessage
		if common.Unmarshal(value, &object) != nil {
			return nil, ErrMediaRetestReferenceUnavailable
		}
		standard := mediaRetestStandardPath(path)
		if declared := mediaRetestObjectKind(object); standard && declared != "" {
			kind = declared
		}
		changed := false
		for _, name := range slices.Sorted(maps.Keys(object)) {
			original := object[name]
			key := normalizeMediaRetestField(name)
			if preparation.pinAliases == nil && mediaRetestAccountField(key) && !slices.Contains([]string{"null", "\"\"", "[]"}, string(bytes.TrimSpace(original))) {
				return nil, ErrMediaRetestProviderAsset
			}
			childKind := ""
			if standard {
				childKind = mediaRetestKind(key)
				if childKind == "" && slices.Contains([]string{"url", "source", "uri", "fileuri", "filedata", "inlinedata", "data"}, key) {
					childKind = kind
				}
			}
			updated, err := preparation.rewriteJSON(ctx, original, append(path, mediaRetestPathPart{value: name}), key, childKind, depth+1)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(updated, original) {
				object[name], changed = updated, true
			}
		}
		if changed {
			return common.Marshal(object)
		}
	case '[':
		var array []common.RawMessage
		if common.Unmarshal(value, &array) != nil {
			return nil, ErrMediaRetestReferenceUnavailable
		}
		changed := false
		for index, original := range array {
			updated, err := preparation.rewriteJSON(ctx, original, append(path, mediaRetestPathPart{value: strconv.Itoa(index), index: true}), field, kind, depth+1)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(updated, original) {
				array[index], changed = updated, true
			}
		}
		if changed {
			return common.Marshal(array)
		}
	case '"':
		if field == "prompt" || field == "text" {
			return input, nil
		}
		if kind == "" {
			kind = preparation.pluginReferenceKind(path)
		}
		if preparation.pinAliases != nil && kind != "image" {
			return input, nil
		}
		var raw string
		if common.Unmarshal(value, &raw) != nil {
			return nil, ErrMediaRetestReferenceUnavailable
		}
		if preparation.pinAliases == nil && mediaRetestProviderSource(raw) {
			return nil, ErrMediaRetestProviderAsset
		}
		if kind != "" {
			if preparation.pinAliases == nil && field == "referenceaudios" && classifyVideoInputSource(raw, "reference_audios") == "voice_id" {
				return nil, ErrMediaRetestProviderAsset
			}
			updated, err := preparation.reference(ctx, kind, raw)
			if err != nil {
				return nil, err
			}
			if updated != raw {
				return common.Marshal(updated)
			}
		}
	}
	return input, nil
}

func (preparation *mediaRetestPreparation) pluginReferenceKind(path []mediaRetestPathPart) string {
	for _, selector := range preparation.videoPaths {
		parts := strings.Split(strings.TrimPrefix(selector.Path, "/"), "/")
		if len(parts) != len(path) {
			continue
		}
		matched := true
		for i, part := range parts {
			if part == "*" && !path[i].index || part != "*" && (path[i].index || part != path[i].value) {
				matched = false
				break
			}
		}
		if matched {
			return selector.Kind
		}
	}
	return ""
}

func mediaRetestProviderSource(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "asset://") || strings.HasPrefix(value, "file://") || strings.HasPrefix(value, "voice://") || strings.HasPrefix(value, "gs://")
}

func (preparation *mediaRetestPreparation) reference(ctx context.Context, kind, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	lower := strings.ToLower(value)
	if preparation.pinAliases != nil {
		if kind == "image" && len(preparation.pinAliases) < 64 && (strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")) {
			preparation.pinAliases[ImageIdentityHash(value)] = true
		}
		return raw, nil
	}
	if mediaRetestProviderSource(lower) || strings.HasPrefix(lower, "file_") || strings.HasPrefix(lower, "file-") {
		return "", ErrMediaRetestProviderAsset
	}
	if strings.HasPrefix(lower, "data:") || value == "" {
		return raw, nil
	}
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		return raw, nil
	}
	if !slices.Contains([]string{"image", "video", "audio"}, kind) {
		return "", ErrMediaRetestReferenceUnavailable
	}
	reference := mediaRetestReference{kind: kind, value: value}
	if previous, found := preparation.checked[reference]; found {
		return previous, nil
	}
	preparation.count++
	if preparation.count > 64 {
		return "", ErrMediaRetestReferenceUnavailable
	}
	parsed, err := parseMediaInputURL(value)
	if err != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	updated, local := "", false
	if preparation.source.MediaType == model.MediaReplayVideo {
		var cached model.MediaInputObject
		err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ? AND token_id = ? AND kind = ? AND source_hash = ? AND status = ?", preparation.source.TaskID, preparation.source.UserID, preparation.source.TokenID, kind, ImageIdentityHash(value), "active").Order("id").Take(&cached).Error
		if err == nil {
			updated, err = preparation.copyMediaFile(ctx, reference, cached.RootPath, cached.ObjectKey, cached.MimeType, cached.Checksum, cached.ByteSize)
			if err != nil {
				return "", err
			}
			local = true
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return "", ErrMediaRetestReferenceUnavailable
		}
	}
	if !local {
		updated, local, err = preparation.copyLocalReference(ctx, reference, parsed)
		if err != nil {
			return "", err
		}
	}
	if !local {
		if err := validatePublicMediaInputURL(parsed); err != nil {
			return "", ErrMediaRetestReferenceUnavailable
		}
		if !preparation.readOnly {
			if err := preparation.checkRemoteReference(ctx, kind, parsed); err != nil {
				return "", err
			}
		}
		updated = raw
	}
	preparation.checked[reference] = updated
	return updated, nil
}

func (preparation *mediaRetestPreparation) checkRemoteReference(ctx context.Context, kind string, parsed *url.URL) error {
	client := mediaInputClient(preparation.mediaConfig, preparation.imageConfig, net.DefaultResolver, (&net.Dialer{Timeout: time.Duration(preparation.mediaConfig.DownloadTimeout) * time.Second}).DialContext)
	defer client.CloseIdleConnections()
	checkRedirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, previous []*http.Request) error {
		if mediaRetestLocalPath(request.URL.Path) {
			return ErrMediaRetestReferenceUnavailable
		}
		return checkRedirect(request, previous)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return ErrMediaRetestReferenceUnavailable
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(request)
	if err != nil {
		return ErrMediaRetestReferenceUnavailable
	}
	defer response.Body.Close()
	limit := preparation.remaining
	if kind == "image" {
		limit = min(limit, preparation.imageConfig.DownloadMaxBytes)
	}
	if response.StatusCode != http.StatusOK || response.ContentLength > limit || response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" {
		return ErrMediaRetestReferenceUnavailable
	}
	file, err := os.CreateTemp("", "new-api-media-retest-*")
	if err != nil {
		return ErrMediaRetestReferenceUnavailable
	}
	defer os.Remove(file.Name())
	defer file.Close()
	size, err := io.Copy(file, io.LimitReader(response.Body, limit+1))
	if err != nil || size > limit || response.ContentLength >= 0 && size != response.ContentLength {
		return ErrMediaRetestReferenceUnavailable
	}
	if _, _, err := ValidateMediaInputFile(ctx, file, size, kind, response.Header.Get("Content-Type"), preparation.imageConfig); err != nil {
		return ErrMediaRetestReferenceUnavailable
	}
	preparation.remaining -= size
	return nil
}

func (preparation *mediaRetestPreparation) copyLocalReference(ctx context.Context, reference mediaRetestReference, parsed *url.URL) (string, bool, error) {
	// SC aliases also include storage-engine private hosts, so resolve their
	// exact immutable URL hash before checking the site's configured origins.
	if reference.kind == "image" {
		if link, found, err := preparation.copyImageAlias(ctx, reference); found || err != nil {
			return link, found, err
		}
	}
	for _, address := range []string{system_setting.ServerAddress, system_setting.TaskPublicAddress} {
		base := strings.TrimSuffix(strings.TrimRight(address, "/"), "/v1")
		origin, err := url.Parse(base)
		if err != nil || !mediaReferenceSameOrigin(parsed, origin) {
			continue
		}
		path, found := strings.CutPrefix(parsed.Path, strings.TrimRight(origin.Path, "/"))
		if !found {
			continue
		}
		switch {
		case strings.HasPrefix(path, "/api/media-inputs/"), strings.HasPrefix(path, "/v1/media/objects/"), strings.HasPrefix(path, storedResultFilePath):
			link, err := preparation.copyGatewayReference(ctx, reference, path)
			return link, true, err
		case strings.HasPrefix(path, "/api/image-objects/"), strings.HasPrefix(path, "/api/integrations/canvas/inputs/"), strings.HasPrefix(path, "/api/media/"), strings.HasPrefix(path, "/v1/media/"):
			return "", true, ErrMediaRetestReferenceUnavailable
		}
	}
	if mediaRetestLocalPath(parsed.Path) {
		return "", true, ErrMediaRetestReferenceUnavailable
	}
	return "", false, nil
}

func mediaRetestLocalPath(path string) bool {
	return strings.Contains(path, "/api/image-objects/imo_") || strings.Contains(path, "/api/media-inputs/minput_") ||
		strings.Contains(path, "/api/integrations/canvas/inputs/sci_") || strings.Contains(path, "/v1/media/objects/") ||
		strings.Contains(path, storedResultFilePath+"img_") || strings.Contains(path, storedResultFilePath+"vid_")
}

func (preparation *mediaRetestPreparation) copyImageAlias(ctx context.Context, reference mediaRetestReference) (string, bool, error) {
	db := model.DB.WithContext(ctx)
	var alias model.ImageInputAlias
	if err := db.Where("url_hash = ?", ImageIdentityHash(reference.value)).Take(&alias).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", false, nil
		}
		return "", false, ErrMediaRetestReferenceUnavailable
	}
	var input model.ImageInputObject
	if alias.TokenId != preparation.source.TokenID || db.Where("input_id = ? AND token_id = ? AND status = ?", alias.InputId, preparation.source.TokenID, "active").Take(&input).Error != nil {
		return "", true, ErrMediaRetestReferenceUnavailable
	}
	if input.ExpiresAt <= time.Now().Unix() {
		var linked model.ImageInputTaskReference
		if db.Where("task_id = ? AND input_id = ?", preparation.source.TaskID, input.InputId).Take(&linked).Error != nil {
			return "", true, ErrMediaRetestReferenceUnavailable
		}
	}
	var object model.ImageStorageObject
	if db.Where("object_id = ? AND status = ?", input.ObjectId, "active").Take(&object).Error != nil || input.ByteSize != object.ByteSize {
		return "", true, ErrMediaRetestReferenceUnavailable
	}
	link, err := preparation.copyImageObject(ctx, reference, object, input.Filename)
	return link, true, err
}

func (preparation *mediaRetestPreparation) copyImageObject(ctx context.Context, reference mediaRetestReference, source model.ImageStorageObject, filename string) (string, error) {
	cfg := preparation.imageConfig
	limit := min(preparation.remaining, cfg.SingleUploadBytes, cfg.DownloadMaxBytes)
	if source.ByteSize <= 0 || source.ByteSize > limit {
		return "", ErrMediaRetestReferenceUnavailable
	}
	if preparation.readOnly {
		preparation.remaining -= source.ByteSize
		return reference.value, nil
	}
	store, err := GetImageObjectStorage(ctx, source)
	if err != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	data, err := store.Read(ctx, source.ObjectKey, limit)
	if err != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	image, err := ValidateImageBytes(data, source.ContentType, limit, cfg.DownloadMaxPixels)
	if err != nil || source.ByteSize != int64(len(image.Data)) || source.Checksum != image.Checksum {
		return "", ErrMediaRetestReferenceUnavailable
	}
	if preparation.source.MediaType == "video" {
		return preparation.publishCopiedMedia(ctx, reference, bytes.NewReader(image.Data), image.ContentType, image.Checksum, source.ByteSize)
	}
	preparation.remaining -= source.ByteSize
	targetStore, err := GetImageStorage(ctx, "temporary")
	if err != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	key := ImageIdentityHash("retest-image", preparation.source.TaskID, strconv.Itoa(preparation.target.Id), preparation.retestKey, source.ObjectId)
	now := time.Now().Unix()
	input := model.ImageInputObject{InputId: "sci_" + key[:32], TokenId: preparation.target.Id, KeyHash: key, Fingerprint: image.Checksum, Filename: filename, ByteSize: source.ByteSize, Status: "reserved", LeaseToken: common.GetUUID(), LeaseExpiresAt: now + int64(cfg.UploadTimeout) + 120, CreatedAt: now, ExpiresAt: now + min(int64(cfg.InputRetentionHours)*3600, model.MediaReplayRetentionSeconds)}
	input.IntentKey = ImageIdentityHash("sc-input", input.InputId)
	ticket, err := model.RecordImageUploadAttempt(ctx, preparation.target.Id, cfg.UploadsPerMinute)
	if err != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	input, reused, err := model.ReserveImageInput(ctx, ticket, input, cfg.InputBytesPerKey)
	if err != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	var object model.ImageStorageObject
	if reused {
		if model.DB.WithContext(ctx).Where("object_id = ? AND status = ?", input.ObjectId, "active").Take(&object).Error != nil {
			return "", ErrMediaRetestReferenceUnavailable
		}
	} else {
		extension := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp"}[image.ContentType]
		if extension == "" {
			return "", ErrMediaRetestReferenceUnavailable
		}
		intent := model.ImageUploadIntent{IntentKey: input.IntentKey, Class: "temporary", UserId: preparation.target.UserId, TokenId: preparation.target.Id, ProfileId: targetStore.Profile.ProfileId, ObjectKey: "inputs/retest/" + key + extension, ContentType: image.ContentType, ByteSize: source.ByteSize, Checksum: image.Checksum, Status: "pending", CreatedAt: input.CreatedAt}
		object, err = StoreImageIntent(ctx, intent, image, input.ExpiresAt)
		if err != nil {
			_ = model.DB.WithContext(context.WithoutCancel(ctx)).Model(&model.ImageInputObject{}).Where("input_id = ? AND status = ?", input.InputId, "reserved").Update("status", "failed").Error
			return "", ErrMediaRetestReferenceUnavailable
		}
	}
	base := strings.TrimSuffix(strings.TrimRight(system_setting.ServerAddress, "/"), "/v1")
	link, err := ImageObjectURL(ctx, object, base, cfg.SignedURLExpiry)
	if err != nil || model.ConfirmImageInput(ctx, input, object, ImageIdentityHash(link)) != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	return link, nil
}

func (preparation *mediaRetestPreparation) copyGatewayReference(ctx context.Context, reference mediaRetestReference, path string) (string, error) {
	now := time.Now().Unix()
	db := model.DB.WithContext(ctx)
	if name, found := strings.CutPrefix(path, storedResultFilePath); found && reference.kind == "image" {
		object, owner, err := ReadImageResultFile(ctx, name, now)
		if err != nil || owner.UserId != preparation.source.UserID || owner.TokenId != preparation.source.TokenID {
			return "", ErrMediaRetestReferenceUnavailable
		}
		return preparation.copyImageObject(ctx, reference, object, name)
	}
	if id, found := strings.CutPrefix(path, "/api/media-inputs/"); found {
		id, valid := strings.CutSuffix(id, "/content")
		var object model.MediaInputObject
		if !valid || strings.Contains(id, "/") || db.Where("object_id = ? AND user_id = ? AND token_id = ? AND kind = ? AND status = ?", id, preparation.source.UserID, preparation.source.TokenID, reference.kind, "active").Take(&object).Error != nil || object.ExpiresAt <= now && object.TaskId != preparation.source.TaskID {
			return "", ErrMediaRetestReferenceUnavailable
		}
		return preparation.copyMediaFile(ctx, reference, object.RootPath, object.ObjectKey, object.MimeType, object.Checksum, object.ByteSize)
	}
	if reference.kind != "video" {
		return "", ErrMediaRetestReferenceUnavailable
	}
	var artifact model.MediaArtifactObject
	if name, found := strings.CutPrefix(path, storedResultFilePath); found {
		object, owner, err := ReadMediaResultFile(ctx, name, now)
		if err != nil || owner.UserId != preparation.source.UserID || owner.TokenId != preparation.source.TokenID {
			return "", ErrMediaRetestReferenceUnavailable
		}
		artifact = object
	} else {
		id := strings.TrimPrefix(path, "/v1/media/objects/")
		if strings.Contains(id, "/") || db.Where("object_id = ? AND user_id = ? AND token_id = ? AND status = ? AND expires_at > ?", id, preparation.source.UserID, preparation.source.TokenID, "active", now).Take(&artifact).Error != nil {
			return "", ErrMediaRetestReferenceUnavailable
		}
	}
	return preparation.copyMediaFile(ctx, reference, artifact.RootPath, artifact.ObjectKey, artifact.MimeType, artifact.Checksum, artifact.ByteSize)
}

func (preparation *mediaRetestPreparation) copyMediaFile(ctx context.Context, reference mediaRetestReference, rootPath, key, mimeType, checksum string, size int64) (string, error) {
	if size <= 0 || size > preparation.remaining {
		return "", ErrMediaRetestReferenceUnavailable
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	defer root.Close()
	input, err := root.Open(key)
	if err != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	defer input.Close()
	if stat, err := input.Stat(); err != nil || !stat.Mode().IsRegular() || stat.Size() != size {
		return "", ErrMediaRetestReferenceUnavailable
	}
	if preparation.readOnly {
		preparation.remaining -= size
		return reference.value, nil
	}
	return preparation.publishCopiedMedia(ctx, reference, input, mimeType, checksum, size)
}

func (preparation *mediaRetestPreparation) publishCopiedMedia(ctx context.Context, reference mediaRetestReference, input io.Reader, mimeType, checksum string, size int64) (string, error) {
	targetPath, err := filepath.Abs(filepath.Join(preparation.mediaConfig.LocalPath, "inputs"))
	if err != nil || os.MkdirAll(targetPath, 0700) != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	targetRoot, err := os.OpenRoot(targetPath)
	if err != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	defer targetRoot.Close()
	identity := ImageIdentityHash("retest-media", preparation.source.TaskID, strconv.Itoa(preparation.target.Id), preparation.retestKey)
	slot := ImageIdentityHash(reference.kind, reference.value)
	objectID := "minput_" + ImageIdentityHash(identity, slot, checksum)[:32]
	fileIdentity := objectID + "-" + common.GetUUID()
	temporary := fileIdentity + ".part"
	output, err := targetRoot.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	defer targetRoot.Remove(temporary)
	defer output.Close()
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(output, hash), &mediaValidationReader{ctx: ctx, reader: io.LimitReader(input, size+1)})
	if err != nil || count != size || hex.EncodeToString(hash.Sum(nil)) != checksum {
		return "", ErrMediaRetestReferenceUnavailable
	}
	contentType, extension, err := ValidateMediaInputFile(ctx, output, size, reference.kind, mimeType, preparation.imageConfig)
	if err != nil || output.Sync() != nil || output.Close() != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	objectKey := fileIdentity + extension
	if err := targetRoot.Rename(temporary, objectKey); err != nil {
		return "", ErrMediaRetestReferenceUnavailable
	}
	now := time.Now().Unix()
	object := model.MediaInputObject{ObjectId: objectID, TaskId: model.MediaRetestInputTaskPrefix + identity[:32], Slot: slot, Kind: reference.kind, SourceHash: ImageIdentityHash(reference.value), UserId: preparation.target.UserId, TokenId: preparation.target.Id, RootPath: targetPath, ObjectKey: objectKey, MimeType: contentType, ByteSize: size, Checksum: checksum, Status: "active", CreatedAt: now, ExpiresAt: now + model.MediaRetestInputRetentionSeconds}
	object, err = model.SaveMediaRetestInput(ctx, object, model.MediaReplaySnapshot{MediaType: preparation.source.MediaType, TaskId: preparation.source.TaskID}, preparation.imageConfig.InputBytesPerKey)
	if err != nil {
		var existing model.MediaInputObject
		if errors.Is(model.DB.WithContext(context.WithoutCancel(ctx)).Where("object_id = ? AND object_key = ?", objectID, objectKey).Take(&existing).Error, gorm.ErrRecordNotFound) {
			_ = targetRoot.Remove(objectKey)
		}
		return "", ErrMediaRetestReferenceUnavailable
	}
	if object.ObjectKey != objectKey {
		_ = targetRoot.Remove(objectKey)
	}
	preparation.remaining -= size
	return MediaInputObjectURL(object)
}
