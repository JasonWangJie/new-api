package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

// MediaPollOutput lives in a private spool file, never in a database blob.
// It preserves inline vendor results until the ordinary artifact store commits.
type MediaPollOutput struct {
	Data        json.RawMessage
	PluginState json.RawMessage
	ResultURL   string
}

func CaptureAsyncMediaOutput(ctx context.Context, task *model.Task) (bool, error) {
	if !task.PrivateData.AsyncMedia {
		return false, nil
	}
	var job model.AsyncMediaJob
	err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", task.TaskID, task.UserId).Take(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cfg, err := GetMediaRuntimeConfig(ctx)
	if err != nil {
		return true, err
	}
	data, err := common.Marshal(MediaPollOutput{Data: task.Data, PluginState: task.PrivateData.PluginState, ResultURL: task.PrivateData.ResultURL})
	if err != nil {
		return true, err
	}
	if int64(len(data)) > cfg.MaxFileBytes*4+(1<<20) {
		return true, errors.New("video result metadata byte limit exceeded")
	}
	rootPath, err := filepath.Abs(cfg.LocalPath)
	if err != nil {
		return true, err
	}
	if err := os.MkdirAll(rootPath, 0700); err != nil {
		return true, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return true, err
	}
	defer root.Close()
	key := "media-output-" + common.GetUUID() + ".json"
	file, err := root.OpenFile(key+".part", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return true, err
	}
	defer root.Remove(key + ".part")
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return true, writeErr
	}
	if closeErr != nil {
		return true, closeErr
	}
	if err := root.Rename(key+".part", key); err != nil {
		return true, err
	}
	if err := model.DB.WithContext(ctx).Model(&model.AsyncMediaJob{}).Where("id = ?", job.ID).Updates(map[string]any{"output_root_path": rootPath, "output_object_key": key}).Error; err != nil {
		_ = root.Remove(key)
		return true, err
	}
	if job.OutputObjectKey != "" {
		oldRoot, err := os.OpenRoot(job.OutputRootPath)
		if err == nil {
			_ = oldRoot.Remove(job.OutputObjectKey)
			_ = oldRoot.Close()
		}
	}
	return true, nil
}

func RestoreAsyncMediaOutput(ctx context.Context, job model.AsyncMediaJob, task *model.Task) error {
	if job.OutputObjectKey == "" {
		return os.ErrNotExist
	}
	root, err := os.OpenRoot(job.OutputRootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.Open(job.OutputObjectKey)
	if err != nil {
		return err
	}
	defer file.Close()
	cfg, err := GetMediaRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	// Multi-result responses share a bounded spool budget; every decoded file
	// additionally passes the artifact store's own byte limit.
	limit := cfg.MaxFileBytes*4 + 1<<20
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("video result metadata byte limit exceeded")
	}
	var output MediaPollOutput
	if err := common.Unmarshal(data, &output); err != nil {
		return err
	}
	task.Data, task.PrivateData.PluginState, task.PrivateData.ResultURL = output.Data, output.PluginState, output.ResultURL
	return nil
}

func RemoveAsyncMediaOutput(job model.AsyncMediaJob) error {
	if job.OutputObjectKey == "" {
		return nil
	}
	root, err := os.OpenRoot(job.OutputRootPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	err = root.Remove(job.OutputObjectKey)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func FrozenAsyncMediaChannel(ctx context.Context, task *model.Task) (*model.Channel, error) {
	if !task.PrivateData.AsyncMedia {
		return nil, nil
	}
	var job model.AsyncMediaJob
	err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", task.TaskID, task.UserId).Take(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data, err := DecryptImagePayload(job.ChannelCipher, "media-channel:"+job.TaskId)
	if err != nil {
		return nil, err
	}
	var channel model.Channel
	if err := common.Unmarshal(data, &channel); err != nil {
		return nil, err
	}
	return &channel, nil
}

func RedactAsyncMediaData(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	var value any
	if common.Unmarshal(data, &value) != nil {
		return nil
	}
	redactInlineMedia(value)
	encoded, err := common.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

func redactInlineMedia(value any) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "bytesBase64Encoded" || key == "b64_json" {
				delete(value, key)
				continue
			}
			if text, ok := child.(string); ok && (len(text) > 64<<10 || len(text) > 5 && text[:5] == "data:") {
				value[key] = ""
				continue
			}
			redactInlineMedia(child)
		}
	case []any:
		for index, child := range value {
			if text, ok := child.(string); ok && (len(text) > 64<<10 || len(text) > 5 && text[:5] == "data:") {
				value[index] = ""
				continue
			}
			redactInlineMedia(child)
		}
	}
}
