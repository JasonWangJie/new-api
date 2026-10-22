package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrAsyncQueueFull = errors.New("asynchronous media admission limit reached")

// enforceAsyncAdmission runs while the caller holds the user's row lock, before
// inserting any task or idempotency record. Image and video queues each have
// their own budget, so a video backlog cannot block image admission.
func enforceAsyncAdmission(tx *gorm.DB, entity any, userID, tokenID, requestBytes int) error {
	tokenLimit := int64(max(1, common.GetEnvOrDefault("ASYNC_MEDIA_TOKEN_ACTIVE_TASK_LIMIT", 100)))
	userLimit := int64(max(1, common.GetEnvOrDefault("ASYNC_MEDIA_USER_ACTIVE_TASK_LIMIT", 500)))
	tokenBytes := int64(max(1, common.GetEnvOrDefault("ASYNC_MEDIA_TOKEN_PENDING_BYTES", 128<<20)))
	userBytes := int64(max(1, common.GetEnvOrDefault("ASYNC_MEDIA_USER_PENDING_BYTES", 512<<20)))
	var usage []struct {
		TokenID int
		Tasks   int64
		Bytes   int64
	}
	// LENGTH on a binary column is byte length on SQLite, MySQL and PostgreSQL.
	if err := tx.Model(entity).Select("token_id, COUNT(*) AS tasks, COALESCE(SUM(LENGTH(request_cipher)), 0) AS bytes").
		Where("user_id = ? AND status NOT IN ?", userID, []string{ImageTaskSucceeded, ImageTaskFailed, ImageTaskExpired, ImageTaskExecutionUnknown}).
		Group("token_id").Scan(&usage).Error; err != nil {
		return err
	}
	remainingTasks, remainingBytes := userLimit, userBytes-int64(requestBytes)
	if int64(requestBytes) > tokenBytes || remainingBytes < 0 {
		return ErrAsyncQueueFull
	}
	for _, row := range usage {
		remainingTasks -= row.Tasks
		remainingBytes -= row.Bytes
		if remainingTasks <= 0 || remainingBytes < 0 || row.TokenID == tokenID && (row.Tasks >= tokenLimit || row.Bytes > tokenBytes-int64(requestBytes)) {
			return ErrAsyncQueueFull
		}
	}
	return nil
}
