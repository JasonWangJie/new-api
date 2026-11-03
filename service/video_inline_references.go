package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
)

// MediaReferencePreparationError proves that no generation request was sent.
// Only this typed local failure participates in custom submission retry rules.
type MediaReferencePreparationError struct {
	Code   string
	Status int
	Cause  error
}

func (err *MediaReferencePreparationError) Error() string { return err.Cause.Error() }
func (err *MediaReferencePreparationError) Unwrap() error { return err.Cause }

// InlineAsyncVideoReferences operates on the driver's owned JSON descriptor,
// after JavaScript has returned and before the durable dispatch barrier. The
// persisted input manifest contains URLs and object IDs, never encoded bytes.
// Explicit local retry actions keep their signed-URL behavior on channels
// configured for passthrough; the selected channel's inline mode takes priority.
func InlineAsyncVideoReferences(ctx context.Context, job model.AsyncMediaJob, request *AsyncVideoRequest, body any, limit int64, total *int64) (any, error) {
	request.VideoReferenceTransportMode = ""
	if request.Execution == nil || request.Execution.TaskPlugin == nil || request.Execution.TaskPlugin.Key != "xai" {
		return body, nil
	}
	var channel model.Channel
	if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", request.Channel.Id, common.ChannelStatusEnabled).Take(&channel).Error; err != nil {
		return nil, err
	}
	request.VideoReferenceTransportMode = channel.GetSetting().VideoReferenceTransportMode
	localRetry := request.PendingDecision != nil && (request.PendingDecision.Action == mediaretry.LocalSameChannel || request.PendingDecision.Action == mediaretry.LocalOtherChannel)
	if request.VideoReferenceTransportMode != "inline" && !localRetry {
		return body, nil
	}
	if err := channel.ValidateSettings(); err != nil {
		return nil, err
	}
	inputs := xaiInlineReferenceInputs(body)
	request.InlineReferenceInputs = inputs
	if len(inputs) == 0 {
		return body, nil
	}
	err := inlineVideoReferenceInputs(ctx, job, request, body, limit, total)
	if err == nil {
		request.LocalReferencesReady = true
		return body, nil
	}
	code, status := "media_reference_download_failed", http.StatusBadGateway
	switch {
	case errors.Is(err, errMediaInputBatchLimit), errors.Is(err, errMediaInputTooLarge):
		code, status = "media_reference_too_large", http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrMediaInputUnavailable), errors.Is(err, errMediaInputInvalid):
		code, status = "media_reference_invalid", http.StatusBadRequest
	}
	return nil, &MediaReferencePreparationError{Code: code, Status: status, Cause: err}
}

// xaiInlineReferenceInputs recognizes only normalized xAI media fields. In
// particular, URLs in prompts, metadata, file_id and existing data URIs are
// never interpreted as download instructions.
func xaiInlineReferenceInputs(body any) []AsyncVideoReferenceInput {
	value, ok := body.(map[string]any)
	if !ok {
		return nil
	}
	var inputs []AsyncVideoReferenceInput
	type referenceSlot struct {
		path, kind string
		value      any
	}
	slots := []referenceSlot{{"/image", "image", value["image"]}, {"/last_frame", "image", value["last_frame"]}, {"/video", "video", value["video"]}}
	if references, ok := value["reference_images"].([]any); ok {
		for index, item := range references {
			slots = append(slots, referenceSlot{"/reference_images/" + strconv.Itoa(index), "image", item})
		}
	}
	if keyframes, ok := value["keyframes"].([]any); ok {
		for index, item := range keyframes {
			if frame, ok := item.(map[string]any); ok {
				slots = append(slots, referenceSlot{"/keyframes/" + strconv.Itoa(index) + "/image", "image", frame["image"]})
			}
		}
	}
	for _, slot := range slots {
		media, ok := slot.value.(map[string]any)
		if !ok {
			continue
		}
		source, ok := media["url"].(string)
		if !ok || !(strings.HasPrefix(strings.ToLower(source), "https://") || strings.HasPrefix(strings.ToLower(source), "http://")) {
			continue
		}
		path := slot.path + "/url"
		inputs = append(inputs, AsyncVideoReferenceInput{Index: len(inputs), Kind: slot.kind, Source: source, Path: path, ObjectSlot: "inline:" + path})
	}
	return inputs
}

func inlineVideoReferenceInputs(ctx context.Context, job model.AsyncMediaJob, request *AsyncVideoRequest, body any, limit int64, total *int64) error {
	if len(request.InlineReferenceInputs) > 64 || job.ExpiresAt <= time.Now().Unix() || request.ReferenceRecoveryDeadline > 0 && request.ReferenceRecoveryDeadline <= time.Now().Unix() {
		return ErrMediaInputUnavailable
	}
	if request.VideoReferenceTransportMode == "inline" && limit <= *total {
		return errMediaInputBatchLimit
	}
	cfg, err := GetMediaRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	imageCfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	inline := request.VideoReferenceTransportMode == "inline"
	if inline {
		cfg.MaxFileBytes = min(cfg.MaxFileBytes, limit-*total)
		imageCfg.DownloadMaxBytes = min(imageCfg.DownloadMaxBytes, int64(20<<20), cfg.MaxFileBytes)
	}
	deadline := job.ExpiresAt
	if request.ReferenceRecoveryDeadline > 0 {
		deadline = min(deadline, request.ReferenceRecoveryDeadline)
	}
	ctx, cancel := context.WithDeadline(ctx, time.Unix(deadline, 0))
	defer cancel()
	ctx, cancelDownload := context.WithTimeout(ctx, time.Duration(cfg.DownloadTimeout)*time.Second)
	defer cancelDownload()
	expiresAt := min(job.ExpiresAt, job.CreatedAt+7*86400)
	stored, err := model.ListMediaInputObjects(ctx, job.TaskId, job.UserId, job.TokenId)
	if err != nil {
		return err
	}
	bySlot := make(map[string]model.MediaInputObject, len(stored))
	for _, object := range stored {
		bySlot[object.Slot] = object
	}
	var remaining atomic.Int64
	remaining.Store(cfg.MaxFileBytes)
	client := mediaInputClient(cfg, imageCfg, net.DefaultResolver, (&net.Dialer{Timeout: time.Duration(min(cfg.DownloadTimeout, imageCfg.DownloadTimeout)) * time.Second}).DialContext)
	defer client.CloseIdleConnections()
	// Acquire the same distributed budget as the media download workers, lazily
	// so already stored inputs do not compete for network download capacity.
	lease, slotKey := "", ImageRedisKey("video-media-slots")
	defer func() {
		if lease != "" {
			_ = common.RDB.ZRem(context.WithoutCancel(ctx), slotKey, lease).Err()
		}
	}()
	for index, input := range request.InlineReferenceInputs {
		if _, err := validateMediaInputURL(input.Source); err != nil {
			return fmt.Errorf("reference %d: %w", index+1, ErrMediaInputUnavailable)
		}
		object, exists := bySlot[input.StorageSlot()]
		sourceMatches := exists && object.SourceHash == ImageIdentityHash(input.Source)
		if !sourceMatches {
			// Earlier local retries may already own the same bytes under a
			// canonical slot and have rewritten the driver URL to a signed link.
			for _, candidate := range stored {
				if candidate.Kind != input.Kind {
					continue
				}
				matches := candidate.SourceHash == ImageIdentityHash(input.Source)
				if !matches {
					link, err := MediaInputObjectURL(candidate)
					matches = err == nil && link == input.Source
				}
				if matches {
					object, exists = candidate, true
					sourceMatches = true
					break
				}
			}
		}
		if exists {
			if !sourceMatches || object.Status != "active" || object.ExpiresAt < deadline || object.Kind != input.Kind {
				return ErrMediaInputUnavailable
			}
		} else {
			if lease == "" {
				if common.RDB == nil || !common.RedisEnabled {
					return errors.New("reference download capacity is unavailable")
				}
				candidate := common.GetUUID()
				for {
					now := time.Now().Unix()
					acquired, err := imageInvocationAcquire.Run(ctx, common.RDB, []string{slotKey}, now, now+int64(cfg.DownloadTimeout+120), cfg.DownloadConcurrency, candidate, cfg.DownloadTimeout+120).Int()
					if err != nil {
						return err
					}
					if acquired == 1 {
						lease = candidate
						break
					}
					timer := time.NewTimer(200 * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						return ctx.Err()
					case <-timer.C:
					}
				}
			}
			var downloadErr error
			object, downloadErr = downloadMediaInput(ctx, job, input, cfg, imageCfg, expiresAt, &remaining, client)
			if downloadErr != nil {
				return downloadErr
			}
		}
		maxBytes := cfg.MaxFileBytes
		if input.Kind == "image" {
			maxBytes = imageCfg.DownloadMaxBytes
		}
		if object.ByteSize <= 0 || object.ByteSize > maxBytes || inline && object.ByteSize > limit-*total {
			return errMediaInputTooLarge
		}
		if exists && !reserveMediaInputBytes(&remaining, object.ByteSize) {
			return errMediaInputBatchLimit
		}
		if inline && input.Kind == "video" && object.MimeType != "video/mp4" || input.Kind == "image" && !strings.HasPrefix(object.MimeType, "image/") {
			return errMediaInputInvalid
		}
		if !inline {
			if err := verifyStoredMediaInput(ctx, object); err != nil {
				return err
			}
			link, err := MediaInputObjectURL(object)
			if err != nil {
				return err
			}
			if _, err := videoRetryReferenceValue(body.(map[string]any), input.Path, &link); err != nil {
				return err
			}
			request.InlineReferenceInputs[index].ObjectId = object.ObjectId
			continue
		}
		root, err := os.OpenRoot(object.RootPath)
		if err != nil {
			return ErrMediaInputUnavailable
		}
		file, err := root.Open(object.ObjectKey)
		if err != nil {
			root.Close()
			return ErrMediaInputUnavailable
		}
		data, readErr := io.ReadAll(io.LimitReader(&mediaValidationReader{ctx: ctx, reader: file}, maxBytes+1))
		file.Close()
		root.Close()
		if readErr != nil {
			return readErr
		}
		if int64(len(data)) != object.ByteSize {
			return ErrMediaInputUnavailable
		}
		checksum := sha256.Sum256(data)
		if hex.EncodeToString(checksum[:]) != object.Checksum {
			return ErrMediaInputUnavailable
		}
		encoded := "data:" + object.MimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
		if _, err := videoRetryReferenceValue(body.(map[string]any), input.Path, &encoded); err != nil {
			return err
		}
		*total += object.ByteSize
		request.InlineReferenceInputs[index].ObjectId = object.ObjectId
	}
	return nil
}
