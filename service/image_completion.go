package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// Leave room for SQL protocol framing; smaller MySQL packet limits are checked
// on the active database connection before allocating the encrypted envelope.
// Larger valid outputs may still finish in this invocation, but must never be
// replayed if their local processing fails without a durable checkpoint.
const maxAsyncImageCompletionBytes = 32 << 20

var ErrAsyncImageCompletionTooLarge = errors.New("async image completion exceeds its durable checkpoint limit")

// PersistAsyncImageCompletion checkpoints the bounded provider response before
// network downloads or billing queries. The existing task CAS prevents a stale
// worker from restoring a terminated task or overwriting another owner's work.
func PersistAsyncImageCompletion(ctx context.Context, task *model.AsyncImageTask, request AsyncImageRequest, completion AsyncImageCompletion) error {
	if task == nil || task.LeaseToken == "" || completion.Usage == nil || len(completion.Body) == 0 {
		return errors.New("invalid async image completion checkpoint")
	}
	// Bound the additional JSON allocation before serializing. RawMessage keeps
	// response JSON inline; byte slices and user strings can expand in JSON.
	size := uint64(len(completion.Body)) + 2*uint64(len(completion.BillingBody)) + 6*uint64(len(request.Prompt))
	for key, value := range request.Native {
		size += 6*uint64(len(key)) + uint64(len(value))
	}
	for _, part := range request.Parts {
		size += 6 * (uint64(len(part.Text)) + uint64(len(part.URL)))
	}
	if size > maxAsyncImageCompletionBytes {
		return ErrAsyncImageCompletionTooLarge
	}
	limit := int64(maxAsyncImageCompletionBytes)
	if common.UsingMainDatabase(common.DatabaseTypeMySQL) {
		// Older supported servers and administrator overrides can use much
		// smaller packets. This protocol limit cannot be read through a model.
		var packetBytes int64
		if err := model.DB.WithContext(ctx).Raw("SELECT @@max_allowed_packet").Scan(&packetBytes).Error; err != nil {
			return err
		}
		if packetBytes <= 0 {
			return errors.New("invalid MySQL async image checkpoint packet limit")
		}
		limit = min(limit, packetBytes/2)
		if size > uint64(limit) {
			return ErrAsyncImageCompletionTooLarge
		}
	}
	// Dispatch and provider receipt persistence can increment the version after
	// the executor received its value copy. Reload within the same live lease;
	// the following transition still uses the current version CAS.
	var current model.AsyncImageTask
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND status = ? AND lease_token = ? AND lease_expires_at > ?", task.TaskId, model.ImageTaskInvoking, task.LeaseToken, time.Now().Unix()).Take(&current).Error; err != nil {
		return err
	}
	if len(current.SelectedChannelCipher) == 0 {
		return errors.New("async image completion is missing its frozen channel")
	}
	if current.UpstreamSucceededAt > 0 {
		completion.EvaluatedAt = time.Unix(current.UpstreamSucceededAt, 0)
	} else if completion.EvaluatedAt.IsZero() {
		completion.EvaluatedAt = time.Now()
	}
	request.Completion = &completion
	data, err := common.Marshal(request)
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return ErrAsyncImageCompletionTooLarge
	}
	cipher, err := EncryptImagePayload(data, "task:"+task.TaskId)
	if err != nil {
		return err
	}
	succeededAt := current.UpstreamSucceededAt
	if succeededAt == 0 {
		succeededAt = completion.EvaluatedAt.Unix()
	}
	if err := model.TransitionImageTask(ctx, current, map[string]any{"request_cipher": cipher, "upstream_succeeded_at": succeededAt}, "output_received", "Provider output saved for local processing", ""); err != nil {
		return err
	}
	return model.DB.WithContext(ctx).Where("task_id = ? AND lease_token = ?", task.TaskId, task.LeaseToken).Take(task).Error
}
