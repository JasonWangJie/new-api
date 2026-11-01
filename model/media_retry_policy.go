package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const MediaRetryPolicyOptionKey = "media_retry_policy"

type MediaRetryPolicyVersion struct {
	ID             int    `json:"-"`
	Revision       int64  `json:"revision" gorm:"uniqueIndex"`
	CreatedAt      int64  `json:"created_at" gorm:"type:bigint"`
	ActorID        int    `json:"actor_id"`
	Action         string `json:"action" gorm:"size:16"`
	SourceRevision int64  `json:"source_revision"`
	Config         string `json:"-" gorm:"size:1048576"`
}

// Decision events are persisted in the same primary-database transaction as
// the task transition. FactJSON contains sanitized error facts, never requests.
type MediaRetryEvent struct {
	ID           int64  `json:"id"`
	EventKey     string `json:"-" gorm:"size:64;uniqueIndex"`
	TaskID       string `json:"task_id" gorm:"size:64;index"`
	Attempt      int64  `json:"attempt"`
	CreatedAt    int64  `json:"created_at" gorm:"type:bigint;index"`
	Fingerprint  string `json:"fingerprint" gorm:"size:64;index"`
	FailureKey   string `json:"-" gorm:"size:64;index"`
	MediaType    string `json:"media_type" gorm:"size:16"`
	Provider     string `json:"provider" gorm:"size:64"`
	Model        string `json:"model" gorm:"size:255"`
	ChannelID    int    `json:"channel_id" gorm:"index"`
	Stage        string `json:"stage" gorm:"size:64"`
	ErrorCode    string `json:"error_code" gorm:"size:128"`
	Category     string `json:"category" gorm:"size:64"`
	HTTPStatus   int    `json:"http_status"`
	Message      string `json:"message" gorm:"type:text"`
	Matched      bool   `json:"matched"`
	RuleID       string `json:"rule_id" gorm:"size:64;index"`
	StepID       string `json:"step_id" gorm:"size:64"`
	Revision     int64  `json:"revision"`
	Action       string `json:"action" gorm:"size:64"`
	Executed     bool   `json:"executed"`
	Outcome      string `json:"outcome" gorm:"size:32"`
	FactJSON     string `json:"-" gorm:"type:text"`
	DecisionJSON string `json:"-" gorm:"type:text"`
	StateJSON    string `json:"-" gorm:"size:1048576"`
}

// InitializeMediaRetryPolicy captures the pre-upgrade timeout exception once.
// Newly created pools never become eligible merely because they share defaults.
func InitializeMediaRetryPolicy(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		guard := Option{Key: MediaRetryPolicyOptionKey, Value: `{"revision":0,"rules":[]}`}
		inserted := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&guard)
		if inserted.Error != nil {
			return inserted.Error
		}
		if inserted.RowsAffected == 0 {
			return nil
		}
		limits := map[string]int{"execution_timeout_seconds": 1200, "account_attempt_timeout_seconds": 300, "total_max_retries": 16, "upstream_timeout_max_retries": 2, "upstream_timeout_retry_base_seconds": 5, "upstream_timeout_retry_max_seconds": 10}
		mixedEnabled := false
		var runtime Option
		err := tx.Where(map[string]any{"key": "image_runtime_config"}).Take(&runtime).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			var fields map[string]common.RawMessage
			if err := common.UnmarshalJsonStr(runtime.Value, &fields); err != nil {
				return err
			}
			for key := range limits {
				if raw, exists := fields[key]; exists {
					value := 0
					if err := common.Unmarshal(raw, &value); err != nil {
						return err
					}
					limits[key] = value
				}
			}
			if raw, exists := fields["mixed_timeout_retry_enabled"]; exists {
				if err := common.Unmarshal(raw, &mixedEnabled); err != nil {
					return err
				}
			}
		}
		var groups []ImageGroupPolicy
		if err := tx.Where("enabled = ? AND async_enabled = ? AND platform IN ?", true, true, []string{"openai", "gemini", "mixed"}).Find(&groups).Error; err != nil {
			return err
		}
		policy := mediaretry.Policy{Rules: []mediaretry.Rule{}, LegacyTimeoutConfig: limits}
		// Only already enabled Drama channels inherit its previous one-replay
		// submission exception; newly created channels never acquire it.
		if tx.Migrator().HasTable(&Channel{}) {
			var channels []Channel
			if err := tx.Select("id", "setting").Where("status = ? AND type = ?", common.ChannelStatusEnabled, constant.ChannelTypeTaskPlugin).Find(&channels).Error; err != nil {
				return err
			}
			for _, channel := range channels {
				if channel.GetSetting().TaskPluginKey == "drama" {
					policy.LegacyVideoChannelIDs = append(policy.LegacyVideoChannelIDs, channel.Id)
				}
			}
		}
		limits["video_submission_max_retries"] = 1
		limits["video_submission_retry_delay_seconds"] = 1
		var mediaRuntime Option
		mediaErr := tx.Where(map[string]any{"key": "media_runtime_config"}).Take(&mediaRuntime).Error
		if mediaErr != nil && !errors.Is(mediaErr, gorm.ErrRecordNotFound) {
			return mediaErr
		}
		submitTimeout := 120
		if mediaErr == nil {
			var media struct {
				SubmitTimeout   int `json:"submit_timeout_seconds"`
				DownloadTimeout int `json:"download_timeout_seconds"`
			}
			media.DownloadTimeout = 900 // Existing configs inherit the download default.
			if err := common.UnmarshalJsonStr(mediaRuntime.Value, &media); err != nil {
				return err
			}
			submitTimeout = media.SubmitTimeout
			if submitTimeout == 0 {
				submitTimeout = media.DownloadTimeout
			}
			if submitTimeout == 0 {
				submitTimeout = 120
			}
		}
		limits["video_submission_timeout_seconds"] = submitTimeout
		for _, group := range groups {
			if group.PolicyKey != "" && (group.Platform != "mixed" || mixedEnabled) {
				policy.LegacyTimeoutScopeKeys = append(policy.LegacyTimeoutScopeKeys, group.PolicyKey)
			}
		}
		policy.LegacyTimeoutEnabled = (len(policy.LegacyTimeoutScopeKeys) > 0 && limits["upstream_timeout_max_retries"] > 0 || len(policy.LegacyVideoChannelIDs) > 0) && limits["total_max_retries"] > 0
		encoded, err := common.Marshal(policy)
		if err != nil {
			return err
		}
		if err := tx.Model(&Option{}).Where(map[string]any{"key": MediaRetryPolicyOptionKey}).Update("value", string(encoded)).Error; err != nil {
			return err
		}
		return tx.Create(&MediaRetryPolicyVersion{Revision: 0, CreatedAt: time.Now().Unix(), Action: "baseline", Config: string(encoded)}).Error
	})
}

func GetMediaRetryPolicy(ctx context.Context) (mediaretry.Policy, error) {
	policy := mediaretry.Policy{Rules: []mediaretry.Rule{}}
	var option Option
	err := DB.WithContext(ctx).Where(map[string]any{"key": MediaRetryPolicyOptionKey}).Take(&option).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return policy, nil
	}
	if err != nil {
		return policy, err
	}
	if err := common.UnmarshalJsonStr(option.Value, &policy); err != nil {
		return policy, err
	}
	_, err = mediaretry.Compile(policy)
	return policy, err
}

// SaveMediaRetryPolicy and its version snapshot commit atomically. Rollback is
// a new revision so readers and optimistic writers never observe time travel.
func SaveMediaRetryPolicy(ctx context.Context, policy mediaretry.Policy, expectedRevision int64, actorID int, sourceRevision *int64) (mediaretry.Policy, error) {
	if expectedRevision < 0 || expectedRevision >= MaxOptionRevision || actorID <= 0 {
		return policy, ErrOptionRevisionInvalid
	}
	if sourceRevision != nil {
		version, err := GetMediaRetryPolicyVersion(ctx, *sourceRevision)
		if err != nil {
			return policy, err
		}
		if err := common.UnmarshalJsonStr(version.Config, &policy); err != nil {
			return policy, err
		}
	}
	policy.Revision = expectedRevision + 1
	if _, err := mediaretry.Compile(policy); err != nil {
		return policy, err
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		guard := Option{Key: MediaRetryPolicyOptionKey, Value: `{"revision":0,"rules":[]}`}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"key"})}).Create(&guard).Error; err != nil {
			return err
		}
		var current Option
		if err := lockForUpdate(tx).Where(map[string]any{"key": MediaRetryPolicyOptionKey}).Take(&current).Error; err != nil {
			return err
		}
		var previous mediaretry.Policy
		if err := common.UnmarshalJsonStr(current.Value, &previous); err != nil {
			return err
		}
		if previous.Revision != expectedRevision {
			return ErrOptionRevisionConflict
		}
		policy.LegacyTimeoutScopeKeys, policy.LegacyTimeoutConfig = previous.LegacyTimeoutScopeKeys, previous.LegacyTimeoutConfig
		policy.LegacyVideoChannelIDs = previous.LegacyVideoChannelIDs
		if len(policy.LegacyTimeoutScopeKeys) == 0 && len(policy.LegacyVideoChannelIDs) == 0 {
			policy.LegacyTimeoutEnabled = false
		}
		encoded, err := common.Marshal(policy)
		if err != nil {
			return err
		}
		if len(encoded) > 1<<20 {
			return errors.New("recovery policy exceeds the configuration size limit")
		}
		now := time.Now().Unix()
		baseline := MediaRetryPolicyVersion{Revision: previous.Revision, CreatedAt: now, Action: "baseline", Config: current.Value}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&baseline).Error; err != nil {
			return err
		}
		version := MediaRetryPolicyVersion{Revision: policy.Revision, CreatedAt: now, ActorID: actorID, Action: "save", Config: string(encoded)}
		if sourceRevision != nil {
			version.Action, version.SourceRevision = "rollback", *sourceRevision
		}
		if err := tx.Create(&version).Error; err != nil {
			return err
		}
		return tx.Model(&Option{}).Where(map[string]any{"key": MediaRetryPolicyOptionKey}).Update("value", string(encoded)).Error
	})
	return policy, err
}

func GetMediaRetryPolicyVersion(ctx context.Context, revision int64) (MediaRetryPolicyVersion, error) {
	var version MediaRetryPolicyVersion
	err := DB.WithContext(ctx).Where("revision = ?", revision).Take(&version).Error
	return version, err
}

func ListMediaRetryPolicyVersions(ctx context.Context, page, pageSize int) ([]MediaRetryPolicyVersion, int64, error) {
	versions := make([]MediaRetryPolicyVersion, 0)
	query := DB.WithContext(ctx).Model(&MediaRetryPolicyVersion{})
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := query.Select("id", "revision", "created_at", "actor_id", "action", "source_revision").Order("revision DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&versions).Error
	return versions, total, err
}

var retrySampleVariable = regexp.MustCompile(`https?://\S+|\b[0-9a-fA-F]{16,}\b|\b[0-9]{4,}\b`)

// NewMediaRetryEvent requires an already sanitized message from the host's
// existing error extractor. It never accepts or inspects a provider body.
func NewMediaRetryEvent(taskID string, attempt int64, fact mediaretry.Fact, decision mediaretry.Decision, states ...mediaretry.State) (*MediaRetryEvent, error) {
	if taskID == "" || len(taskID) > 64 || attempt < 0 || len(fact.Message) > 8192 || len(fact.Model) > 255 || len(fact.Provider) > 64 || len(fact.ErrorCode) > 128 || len(fact.Stage) > 64 {
		return nil, errors.New("invalid media recovery event")
	}
	factData, err := common.Marshal(fact)
	if err != nil {
		return nil, err
	}
	decisionData, err := common.Marshal(decision)
	if err != nil {
		return nil, err
	}
	stateJSON := ""
	if len(states) > 0 {
		encoded, err := common.Marshal(states[0])
		if err != nil {
			return nil, err
		}
		stateJSON = string(encoded)
	}
	fingerprintSource := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s", fact.MediaType, fact.Provider, fact.Stage, fact.ErrorCode, fact.HTTPStatus, retrySampleVariable.ReplaceAllString(strings.ToLower(fact.Message), "<variable>"))
	eventSource := fmt.Sprintf("%s:%d:%s:%d:%s:%s", taskID, attempt, fact.Stage, decision.Revision, decision.RuleID, decision.StepID)
	failureSource := fmt.Sprintf("%s:%d:%s", taskID, attempt, fact.Stage)
	category := decision.Category
	if category == "" {
		category = fact.Category
	}
	return &MediaRetryEvent{EventKey: fmt.Sprintf("%x", sha256.Sum256([]byte(eventSource))), TaskID: taskID, Attempt: attempt,
		CreatedAt: time.Now().Unix(), Fingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte(fingerprintSource))), FailureKey: fmt.Sprintf("%x", sha256.Sum256([]byte(failureSource))), MediaType: fact.MediaType,
		Provider: fact.Provider, Model: fact.Model, ChannelID: fact.ChannelID, Stage: fact.Stage, ErrorCode: fact.ErrorCode, HTTPStatus: fact.HTTPStatus,
		Message: fact.Message, Category: category, Matched: decision.Matched, RuleID: decision.RuleID, StepID: decision.StepID, Revision: decision.Revision, Action: decision.Action,
		FactJSON: string(factData), DecisionJSON: string(decisionData), StateJSON: stateJSON}, nil
}

func RecordMediaRetryEventTx(tx *gorm.DB, event *MediaRetryEvent) error {
	if event == nil {
		return nil
	}
	if event.EventKey == "" || event.TaskID == "" {
		return errors.New("invalid media recovery event identity")
	}
	// Use a copy so callers can retry after rollback without retaining an ID
	// allocated by the failed transaction.
	row := *event
	row.ID = 0
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

func MarkMediaRetryEventExecutedTx(tx *gorm.DB, taskID string, decision mediaretry.Decision) error {
	var event MediaRetryEvent
	err := tx.Where("task_id = ? AND revision = ? AND rule_id = ? AND step_id = ?", taskID, decision.Revision, decision.RuleID, decision.StepID).Order("id DESC").Take(&event).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return tx.Model(&MediaRetryEvent{}).Where("id = ?", event.ID).Update("executed", true).Error
}

func MarkMediaRetryTaskOutcomeTx(tx *gorm.DB, taskID, outcome string) error {
	if outcome != "succeeded" && outcome != "failed" && outcome != "unknown" {
		return errors.New("invalid media recovery outcome")
	}
	return tx.Model(&MediaRetryEvent{}).Where("task_id = ? AND executed = ?", taskID, true).Update("outcome", outcome).Error
}

func GetMediaRetryEvent(ctx context.Context, id int64) (MediaRetryEvent, error) {
	var event MediaRetryEvent
	err := DB.WithContext(ctx).First(&event, id).Error
	return event, err
}

// PruneMediaRetryEvents bounds diagnostic retention without deleting live task
// counters, configuration history or recovery decisions on the task itself.
func PruneMediaRetryEvents(ctx context.Context, before int64, limit int) error {
	if before <= 0 || limit < 1 || limit > 1000 {
		return errors.New("invalid recovery event retention batch")
	}
	var ids []int64
	if err := DB.WithContext(ctx).Model(&MediaRetryEvent{}).Where("created_at < ?", before).Order("id").Limit(limit).Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return DB.WithContext(ctx).Where("id IN ? AND created_at < ?", ids, before).Delete(&MediaRetryEvent{}).Error
}

func GetLatestMediaRetryEvent(ctx context.Context, taskID string) (MediaRetryEvent, error) {
	var event MediaRetryEvent
	err := DB.WithContext(ctx).Where("task_id = ?", taskID).Order("id DESC").Take(&event).Error
	return event, err
}

type MediaRetrySampleFilter struct {
	MediaType, Provider, Model, Stage, RuleID, Category string
	ChannelID                                           int
	Matched                                             *bool
	Start, End                                          int64
}

type MediaRetrySample struct {
	Fingerprint        string `json:"fingerprint"`
	MediaType          string `json:"media_type"`
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	Stage              string `json:"stage"`
	ErrorCode          string `json:"error_code"`
	Category           string `json:"category"`
	HTTPStatus         int    `json:"http_status"`
	Message            string `json:"message"`
	Count              int64  `json:"count"`
	TaskCount          int64  `json:"task_count"`
	ExecutedCount      int64  `json:"executed_count"`
	SucceededTaskCount int64  `json:"succeeded_task_count"`
	LastSeen           int64  `json:"last_seen"`
	LatestEventID      int64  `json:"latest_event_id"`
	Matched            bool   `json:"matched"`
	RuleID             string `json:"rule_id"`
}

func mediaRetrySampleQuery(ctx context.Context, filter MediaRetrySampleFilter) *gorm.DB {
	query := DB.WithContext(ctx).Model(&MediaRetryEvent{})
	for key, value := range map[string]string{"media_type": filter.MediaType, "provider": filter.Provider, "model": filter.Model, "stage": filter.Stage, "rule_id": filter.RuleID, "category": filter.Category} {
		if value != "" {
			query = query.Where(map[string]any{key: value})
		}
	}
	if filter.ChannelID > 0 {
		query = query.Where("channel_id = ?", filter.ChannelID)
	}
	if filter.Matched != nil {
		query = query.Where("matched = ?", *filter.Matched)
	}
	if filter.Start > 0 {
		query = query.Where("created_at >= ?", filter.Start)
	}
	if filter.End > 0 {
		query = query.Where("created_at <= ?", filter.End)
	}
	return query
}

func ListMediaRetrySampleEvents(ctx context.Context, fingerprint string, filter MediaRetrySampleFilter, page, pageSize int) ([]MediaRetryEvent, int64, error) {
	query := mediaRetrySampleQuery(ctx, filter).Where("fingerprint = ?", fingerprint)
	var total int64
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	events := make([]MediaRetryEvent, 0)
	err := query.Order("created_at DESC, id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&events).Error
	return events, total, err
}

func ListMediaRetrySamples(ctx context.Context, filter MediaRetrySampleFilter, page, pageSize int) ([]MediaRetrySample, int64, error) {
	query := mediaRetrySampleQuery(ctx, filter)
	var total int64
	if err := query.Session(&gorm.Session{}).Distinct("fingerprint").Count(&total).Error; err != nil {
		return nil, 0, err
	}
	samples := make([]MediaRetrySample, 0)
	err := query.Session(&gorm.Session{}).Select("fingerprint, COUNT(DISTINCT failure_key) AS count, COUNT(DISTINCT task_id) AS task_count, MAX(created_at) AS last_seen, MAX(id) AS latest_event_id, SUM(CASE WHEN executed = ? THEN 1 ELSE 0 END) AS executed_count, COUNT(DISTINCT CASE WHEN executed = ? AND outcome = ? THEN task_id ELSE NULL END) AS succeeded_task_count", true, true, "succeeded").Group("fingerprint").Order("last_seen DESC, fingerprint").Offset((page - 1) * pageSize).Limit(pageSize).Scan(&samples).Error
	if err != nil || len(samples) == 0 {
		return samples, total, err
	}
	ids := make([]int64, len(samples))
	for i := range samples {
		ids[i] = samples[i].LatestEventID
	}
	var events []MediaRetryEvent
	if err := DB.WithContext(ctx).Where("id IN ?", ids).Find(&events).Error; err != nil {
		return nil, 0, err
	}
	byID := make(map[int64]MediaRetryEvent, len(events))
	for _, event := range events {
		byID[event.ID] = event
	}
	for i := range samples {
		event := byID[samples[i].LatestEventID]
		samples[i].MediaType, samples[i].Provider, samples[i].Model, samples[i].Stage = event.MediaType, event.Provider, event.Model, event.Stage
		samples[i].ErrorCode, samples[i].HTTPStatus, samples[i].Message = event.ErrorCode, event.HTTPStatus, event.Message
		samples[i].Matched, samples[i].RuleID = event.Matched, event.RuleID
		samples[i].Category = event.Category
	}
	return samples, total, nil
}
