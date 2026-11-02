package service

import (
	"cmp"
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
)

var ErrVideoRetryDeferred = errors.New("video recovery decision must be checked again")
var ErrVideoCompatibleChannelsUnavailable = errors.New("no compatible video channel is available")

// VideoSubmissionRequiresReconciliation preserves reservations for providers
// whose submission failures may leave a billable operation running upstream.
func VideoSubmissionRequiresReconciliation(provider string) bool {
	return provider == "nldw" || provider == "yuxibox"
}

// PlanInitialVideoFailure is shared by live submission and sample previews.
// Only the captured legacy protocol evidence can grant its existing exception.
func PlanInitialVideoFailure(policy mediaretry.Policy, fact mediaretry.Fact, state mediaretry.State) (mediaretry.Decision, error) {
	decision, err := mediaretry.Evaluate(policy, fact, state)
	if err != nil || decision.Matched {
		return decision, err
	}
	evidence := fact.VideoEvidence
	if fact.MediaType != "video" || fact.Stage != "submit" || evidence == nil || !evidence.LegacySubmissionRejected || evidence.LegacySubmissionUsed || fact.HasOutput || fact.HasUpstreamTask {
		return decision, nil
	}
	deadline := fact.Deadline
	if limit := policy.LegacyTimeoutConfig["video_submission_timeout_seconds"]; limit > 0 && evidence.SubmissionStartedAt > 0 {
		deadline = min(deadline, evidence.SubmissionStartedAt+int64(limit))
	}
	if !policy.PauseResubmissions && policy.LegacyTimeoutEnabled && slices.Contains(policy.LegacyVideoChannelIDs, fact.ChannelID) && state.TotalUsed < state.TotalLimit && state.Now+1 < deadline {
		decision.Action, decision.RequestedAction, decision.DelaySeconds = mediaretry.SameChannel, mediaretry.SameChannel, 1
		decision.Category = "legacy_submission_rejection"
	}
	return decision, nil
}

// PreviewInitialVideoFailure retains the selected failure's evidence, while
// incorporating a legacy replay already consumed by the current task.
func PreviewInitialVideoFailure(ctx context.Context, taskID string, fact mediaretry.Fact, state mediaretry.State, policy mediaretry.Policy) (mediaretry.Decision, error) {
	if fact.VideoEvidence != nil {
		var job model.AsyncMediaJob
		if err := model.DB.WithContext(ctx).Where("task_id = ?", taskID).Take(&job).Error; err != nil {
			return mediaretry.Decision{}, err
		}
		plain, err := DecryptImagePayload(job.RequestCipher, "media-request:"+taskID)
		if err != nil {
			return mediaretry.Decision{}, err
		}
		var request AsyncVideoRequest
		if err := common.Unmarshal(plain, &request); err != nil {
			return mediaretry.Decision{}, err
		}
		if current := request.RetryFact; current != nil && current.VideoEvidence != nil && current.VideoEvidence.LegacySubmissionUsed {
			evidence := *fact.VideoEvidence
			evidence.LegacySubmissionUsed = true
			fact.VideoEvidence = &evidence
		}
	}
	return PlanInitialVideoFailure(policy, fact, state)
}

// ResolveVideoRetryPolicy applies the token's current group restrictions to the
// frozen routing group. A worker must never inherit authority from admission.
func ResolveVideoRetryPolicy(ctx context.Context, token model.Token, job model.AsyncMediaJob) (model.ImageGroupPolicy, []ImageModelCapability, error) {
	var owner model.User
	if err := model.DB.WithContext(ctx).Where("id = ? AND status = ?", job.UserId, common.UserStatusEnabled).Take(&owner).Error; err != nil {
		return model.ImageGroupPolicy{}, nil, err
	}
	policy, catalog, err := ResolveImagePolicy(ctx, token, job.Provider)
	if err != nil {
		return policy, catalog, err
	}
	if !GroupInUserUsableGroups(owner.Group, job.Group) || policy.Group != job.Group && policy.Group != "auto" {
		return policy, catalog, errors.New("video group permission changed")
	}
	if policy.Group == "auto" {
		groups, err := token.GetAutoGroups()
		if err != nil {
			return policy, catalog, errors.New("video automatic group permission is invalid")
		}
		if len(groups) > 0 {
			groups = FilterUserTokenAutoGroups(owner.Group, groups)
		} else {
			groups = GetUserAutoGroup(owner.Group)
		}
		if !slices.Contains(groups, job.Group) {
			return policy, catalog, errors.New("video automatic group permission changed")
		}
	}
	if _, selected := VideoPolicyCapability(catalog, job.Model); policy.Id != 0 && (!policy.Enabled || !policy.AsyncEnabled || !selected) {
		return policy, catalog, errors.New("video provider permission changed")
	}
	return policy, catalog, nil
}

// VideoRetryCandidateChannels shares authorization and compatibility checks
// between preview and dispatch, without rotating account keys or writing state.
func VideoRetryCandidateChannels(ctx context.Context, job model.AsyncMediaJob, request AsyncVideoRequest, upstreamModel string) ([]model.Channel, error) {
	decision := request.PendingDecision
	if decision == nil || request.RetryFact == nil || !mediaretry.IsResubmission(decision.Action) {
		return nil, errors.New("video recovery is missing an executable decision")
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", job.TokenId, job.UserId).Take(&token).Error; err != nil {
		return nil, err
	}
	if token.Status != common.TokenStatusEnabled && token.Status != common.TokenStatusExhausted || token.ExpiredTime != -1 && token.ExpiredTime <= time.Now().Unix() {
		return nil, errors.New("video token is unavailable")
	}
	if token.ModelLimitsEnabled && !token.GetModelLimitsMap()[job.Model] {
		return nil, errors.New("video model permission changed")
	}
	if limits := token.GetIpLimits(); len(limits) > 0 {
		ip := net.ParseIP(request.ClientIP)
		if ip == nil || !common.IsIpInCIDRList(ip, limits) {
			return nil, errors.New("video client IP is no longer allowed")
		}
	}
	policy, _, err := ResolveVideoRetryPolicy(ctx, token, job)
	if err != nil {
		return nil, err
	}
	policy.Group = job.Group
	var abilities []model.Ability
	if err := model.DB.WithContext(ctx).Where(map[string]any{"group": job.Group, "enabled": true}).Where("model IN ?", []string{job.Model, ratio_setting.RoutingMatchModelName(job.Model)}).Find(&abilities).Error; err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(abilities))
	for _, ability := range abilities {
		ids = append(ids, ability.ChannelId)
	}
	var channels []model.Channel
	if err := model.DB.WithContext(ctx).Where("id IN ? AND status = ?", ids, common.ChannelStatusEnabled).Find(&channels).Error; err != nil {
		return nil, err
	}
	resolution := ""
	if details := request.RequestDetails(); details != nil {
		resolution = VideoPoolResolution(details.RequestedResolution)
	}
	var bindings []model.ImageChannelPool
	if err := model.DB.WithContext(ctx).Where("binding_key = ?", ImagePoolBindingKey(policy, job.Model, resolution)).Find(&bindings).Error; err != nil {
		return nil, err
	}
	other := decision.Action == mediaretry.OtherChannel || decision.Action == mediaretry.LocalOtherChannel
	channels = slices.DeleteFunc(channels, func(channel model.Channel) bool {
		hasKey := false
		for index, key := range channel.GetKeys() {
			status, configured := channel.ChannelInfo.MultiKeyStatusList[index]
			if key != "" && (!channel.ChannelInfo.IsMultiKey || !configured || status == common.ChannelStatusEnabled) {
				hasKey = true
				break
			}
		}
		if !hasKey {
			return true
		}
		if other && channel.Id == request.RetryFact.ChannelID || !other && channel.Id != request.RetryFact.ChannelID || slices.Contains(decision.ExcludedChannelIDs, channel.Id) || len(decision.AllowedChannelIDs) > 0 && !slices.Contains(decision.AllowedChannelIDs, channel.Id) {
			return true
		}
		if len(bindings) > 0 && !slices.ContainsFunc(bindings, func(binding model.ImageChannelPool) bool { return binding.ChannelId == channel.Id }) {
			return true
		}
		upstream, err := ResolveImageChannelUpstreamModel(channel, job.Model)
		return err != nil || upstream != upstreamModel && !(upstreamModel == "" && upstream == job.Model) || !mediaChannelSupportsVideoPlatform(&channel, job.Model, job.Provider)
	})
	for i := range channels {
		if index := slices.IndexFunc(bindings, func(binding model.ImageChannelPool) bool { return binding.ChannelId == channels[i].Id }); index >= 0 {
			ApplyMediaChannelPoolWeight(&channels[i], bindings[index].Priority)
		}
	}
	slices.SortFunc(channels, func(a, b model.Channel) int {
		if order := cmp.Compare(b.GetPriority(), a.GetPriority()); order != 0 {
			return order
		}
		return cmp.Compare(a.Id, b.Id)
	})
	return channels, nil
}

// PickVideoRetryChannel selects an account only for a real execution attempt.
func PickVideoRetryChannel(ctx context.Context, job model.AsyncMediaJob, request AsyncVideoRequest, upstreamModel string) (*model.Channel, error) {
	channels, err := VideoRetryCandidateChannels(ctx, job, request, upstreamModel)
	if err != nil {
		return nil, err
	}
	for len(channels) > 0 {
		priority := channels[0].GetPriority()
		count := 0
		for count < len(channels) && channels[count].GetPriority() == priority {
			count++
		}
		weights := make([]int, count)
		for i := range count {
			weights[i] = channels[i].GetWeight()
		}
		index := PickMediaChannelByWeight(weights, rand.Float64())
		if index < 0 {
			return nil, errors.Join(ErrVideoCompatibleChannelsUnavailable, errors.New("video channel pool has no selectable weight"))
		}
		candidate := channels[index]
		key, _, err := candidate.GetNextEnabledKey()
		if err == nil && key != "" {
			candidate.Key, candidate.Keys, candidate.ChannelInfo = key, nil, model.ChannelInfo{}
			return &candidate, nil
		}
		channels = append(channels[:index], channels[index+1:]...)
	}
	return nil, ErrVideoCompatibleChannelsUnavailable
}

// PreviewVideoRetryEligibility checks the current durable evidence and the same
// candidate filters as execution. It never selects a key, downloads or bills.
func PreviewVideoRetryEligibility(ctx context.Context, taskID string, fact mediaretry.Fact, decision mediaretry.Decision) (MediaRetryEligibility, error) {
	result := MediaRetryEligibility{Checked: true, CandidateChannelIDs: []int{}, Warnings: []string{"dispatch_checks_repeated_before_send"}}
	var job model.AsyncMediaJob
	if err := model.DB.WithContext(ctx).Where("task_id = ?", taskID).Take(&job).Error; err != nil {
		return result, err
	}
	if job.Status == "failed" || job.Status == "expired" || job.Status == "deleted" || job.BillingStatus == "refunded" || job.BillingStatus == "not_billable" {
		result.Reason = "historical_task_not_restarted"
		return result, nil
	}
	if decision.Action == "" {
		result.Checked = false
		result.Reason = "builtin_decision_not_simulated"
		return result, nil
	}
	if decision.Action == mediaretry.Stop || decision.Action == mediaretry.ManualReview {
		result.Eligible = decision.BlockedReason == ""
		result.Reason = decision.BlockedReason
		return result, nil
	}
	var task model.Task
	err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&task).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return result, err
	}
	if !mediaretry.IsResubmission(decision.Action) {
		switch decision.Action {
		case mediaretry.PollSameJob:
			result.Eligible = task.PrivateData.UpstreamTaskID != "" && task.Status != model.TaskStatusSuccess && task.Status != model.TaskStatusFailure
		case mediaretry.ResumeDownload:
			result.Eligible = (task.PrivateData.ResultURL != "" || job.ArtifactKeys != "") && job.StorageStatus != "ready"
		case mediaretry.ResumeBilling:
			result.Eligible = task.PrivateData.DurableBilling && task.PrivateData.SubmitAccountingRecorded && job.BillingStatus == "reserved" && (task.PrivateData.ResultURL != "" || job.ArtifactKeys != "")
		}
		if !result.Eligible {
			result.Reason = "recovery_evidence_unavailable"
		}
		return result, nil
	}
	if task.ID == 0 {
		result.Reason = "initial_submission_session_not_replayable"
		return result, nil
	}
	if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
		result.Reason = "historical_task_not_restarted"
		return result, nil
	}
	if task.PrivateData.ResultURL != "" || job.OutputObjectKey != "" || job.ArtifactKeys != "" || task.PrivateData.MediaRetryFact != nil && task.PrivateData.MediaRetryFact.HasOutput {
		result.Reason = "output_already_available"
		return result, nil
	}
	configured := decision.Matched && decision.BlockedReason == "" && mediaretry.IsResubmission(decision.Action)
	if !configured && (job.Status == "execution_unknown" || job.ReferenceRecoveryStage == "unknown" || job.ReferenceRecoveryStage == "dispatched" || task.PrivateData.VideoReconciliation != nil && task.PrivateData.VideoReconciliation.ManualRequired) {
		result.Reason = "upstream_result_unknown"
		return result, nil
	}
	if !configured && task.PrivateData.UpstreamTaskID != "" && !task.PrivateData.VideoTerminalConfirmed {
		result.Reason = "current_upstream_terminal_state_unconfirmed"
		return result, nil
	}
	if !task.PrivateData.DurableBilling || !task.PrivateData.SubmitAccountingRecorded || task.Quota <= 0 || job.BillingStatus != "reserved" && !(configured && job.BillingStatus == "unknown") || task.Quota != job.Quota || task.AttemptGeneration != job.AttemptGeneration {
		result.Reason = "reservation_unavailable"
		return result, nil
	}
	plain, err := DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
	if err != nil {
		result.Reason = "request_replay_unavailable"
		return result, nil
	}
	var request AsyncVideoRequest
	if common.Unmarshal(plain, &request) != nil || len(request.Body) == 0 || request.ReferenceRecoveryVersion < 1 || request.ReferenceRecoveryVersion > 2 {
		result.Reason = "request_replay_unavailable"
		return result, nil
	}
	now := time.Now().Unix()
	if job.ExpiresAt <= now || request.ReferenceRecoveryDeadline <= now {
		result.Reason = "execution_deadline_reached"
		return result, nil
	}
	plugin, exists := jsplugin.DefaultRegistry.Get(job.Provider)
	if !exists || request.PluginHash == "" || plugin.Engine.SourceFingerprint() != request.PluginHash {
		result.Reason = "provider_capability_changed"
		return result, nil
	}
	local := decision.Action == mediaretry.LocalSameChannel || decision.Action == mediaretry.LocalOtherChannel
	if local {
		if request.ReferenceLocalizationUnsupported || len(request.ReferenceInputs) == 0 || request.ReferenceURLExpiresAt <= now || request.ReferenceURLExpiresAt > job.ExpiresAt || request.ReferenceURLExpiresAt > job.CreatedAt+7*86400 {
			result.Reason = "reference_unavailable"
			return result, nil
		}
		var body map[string]any
		data := request.Body
		if len(request.OriginalBody) > 0 {
			data = request.OriginalBody
		}
		if common.Unmarshal(data, &body) != nil {
			result.Reason = "request_replay_unavailable"
			return result, nil
		}
		for _, input := range request.ReferenceInputs {
			value, err := videoRetryReferenceValue(body, input.Path, nil)
			if err != nil || value != input.Source {
				result.Reason = "reference_slots_changed"
				return result, nil
			}
			if strings.HasPrefix(input.Source, "asset://") {
				continue
			}
			if strings.HasPrefix(input.Source, "data:") {
				if !strings.HasPrefix(input.Source, "data:"+input.Kind+"/") || !mediaInputDataURIPattern.MatchString(input.Source) {
					result.Reason = "reference_unavailable"
					return result, nil
				}
				continue
			}
			bound, err := verifiedLocalMediaReference(ctx, job, input, request.ReferenceURLExpiresAt)
			if err != nil {
				result.Reason = "reference_unavailable"
				return result, nil
			}
			if !bound {
				result.Warnings = append(result.Warnings, "remote_reference_download_not_performed")
			}
		}
	}
	request.RetryFact, request.PendingDecision = &fact, &decision
	channels, err := VideoRetryCandidateChannels(ctx, job, request, task.Properties.UpstreamModelName)
	if err != nil {
		result.Reason = "eligibility_changed"
		return result, nil
	}
	for _, channel := range channels {
		result.CandidateChannelIDs = append(result.CandidateChannelIDs, channel.Id)
	}
	result.Eligible = len(channels) > 0
	if !result.Eligible {
		result.Reason = "no_compatible_channel"
	}
	return result, nil
}

// ApplyConfiguredVideoDecision executes only the currently matched unknown-result
// action. The captured protocol evidence remains unchanged in the retry record.
func ApplyConfiguredVideoDecision(ctx context.Context, task model.Task, job model.AsyncMediaJob, fact mediaretry.Fact, decision *mediaretry.Decision, state mediaretry.State) (bool, error) {
	if !decision.Matched || decision.BlockedReason != "" || fact.HasOutput || !fact.Unknown && !(fact.Accepted && !fact.Terminal) || !mediaretry.IsResubmission(decision.Action) && decision.Action != mediaretry.Stop {
		return false, nil
	}
	if task.Status == model.TaskStatusFailure || task.Status == model.TaskStatusSuccess || job.Status == "failed" || job.Status == "expired" || job.Status == "succeeded" {
		decision.Action, decision.BlockedReason = mediaretry.ManualReview, "historical_task_not_restarted"
		return false, nil
	}
	if decision.Action == mediaretry.Stop {
		return true, model.EndAsyncVideoPolicyExecution(ctx, task, job, fact, decision, state)
	}
	if !task.PrivateData.DurableBilling || !task.PrivateData.SubmitAccountingRecorded || task.Quota <= 0 || task.Quota != job.Quota || job.BillingStatus != "reserved" && job.BillingStatus != "unknown" {
		decision.Action, decision.BlockedReason = mediaretry.ManualReview, "reservation_unavailable"
		return false, nil
	}
	plain, err := DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
	var request AsyncVideoRequest
	if err != nil || common.Unmarshal(plain, &request) != nil || len(request.Body) == 0 || request.ReferenceRecoveryVersion < 1 || request.ReferenceRecoveryVersion > 2 {
		decision.Action, decision.BlockedReason = mediaretry.ManualReview, "request_replay_unavailable"
		return false, nil
	}
	if request.ReferenceRecoveryDeadline <= time.Now().Unix() || job.ExpiresAt <= time.Now().Unix() {
		decision.Action, decision.BlockedReason = mediaretry.ManualReview, "execution_deadline_reached"
		return false, nil
	}
	request.RetryFact, request.PendingDecision, request.RetryFailedAt = &fact, decision, time.Now().Unix()
	if len(request.OriginalBody) > 0 {
		request.Body = append([]byte(nil), request.OriginalBody...)
		request.LocalReferencesReady = false
	}
	encoded, err := common.Marshal(request)
	if err != nil {
		return false, err
	}
	cipher, err := EncryptImagePayload(encoded, "media-request:"+job.TaskId)
	if err != nil {
		return false, err
	}
	local := decision.Action == mediaretry.LocalSameChannel || decision.Action == mediaretry.LocalOtherChannel
	_, err = model.ScheduleAsyncVideoReferenceRecovery(ctx, task, job, fact.Message, local, model.AsyncVideoRecoveryPlan{Fact: fact, Decision: *decision, State: state, RequestCipher: cipher})
	return true, err
}

// RetainUnconfirmedVideoBilling keeps a retired unknown attempt's hold when a
// replacement fails. Only the existing explicit reconciliation can release it.
func RetainUnconfirmedVideoBilling(ctx context.Context, task *model.Task) (bool, error) {
	if task == nil || !task.PrivateData.AsyncMedia || task.Status != model.TaskStatusFailure || !slices.ContainsFunc(task.PrivateData.VideoAttempts, func(attempt model.VideoTaskAttempt) bool { return attempt.Status == model.TaskStatusUnknown }) {
		return false, nil
	}
	var current model.Task
	if err := model.DB.WithContext(ctx).First(&current, task.ID).Error; err != nil {
		return false, err
	}
	if current.AttemptGeneration != task.AttemptGeneration || current.Status == model.TaskStatusFailure || current.Status == model.TaskStatusSuccess {
		return false, nil
	}
	var job model.AsyncMediaJob
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", current.TaskID, current.UserId).Take(&job).Error; err != nil {
		return false, err
	}
	if job.ReferenceRecoveryStage == "age_limit_refund" {
		return false, nil
	}
	fact := NormalizeMediaRetryFact(mediaretry.Fact{MediaType: "video", Provider: job.Provider, Model: job.Model, ChannelID: task.ChannelId, Stage: "terminal", ErrorCode: "upstream_failed", Message: task.FailReason, Accepted: true, Terminal: task.PrivateData.VideoTerminalConfirmed, Unknown: !task.PrivateData.VideoTerminalConfirmed, HasUpstreamTask: task.PrivateData.UpstreamTaskID != ""})
	if err := model.EndAsyncVideoPolicyExecution(ctx, current, job, fact, nil, videoRetryState(current, job)); err != nil {
		return false, err
	}
	return true, model.DB.WithContext(ctx).First(task, task.ID).Error
}

// SelectVideoRecoveryChannel pins execution while retaining the first hold's
// billing owner. The selected channel, polling profile and request move together.
func SelectVideoRecoveryChannel(ctx context.Context, job *model.AsyncMediaJob, task *model.Task, request *AsyncVideoRequest) error {
	candidate, err := PickVideoRetryChannel(ctx, *job, *request, task.Properties.UpstreamModelName)
	if err != nil {
		return err
	}
	selected := *task
	selected.ChannelId = candidate.Id
	selected.PrivateData.Key = candidate.Key
	selected.PrivateData.BillingChannelID = task.BillingChannelID()
	selected.PrivateData.MediaRetryDecision = request.PendingDecision
	upstream := task.Properties.UpstreamModelName
	if upstream == "" {
		upstream = job.Model
	}
	selected.PrivateData.UpstreamAsync, _ = candidate.GetOtherSettings().UpstreamAsync.Match("video", upstream, UpstreamAsyncOperation(task.Action))
	request.Channel = *candidate
	encoded, err := common.Marshal(request)
	if err != nil {
		return err
	}
	cipher, err := EncryptImagePayload(encoded, "media-request:"+job.TaskId)
	if err != nil {
		return err
	}
	channelData, err := common.Marshal(candidate)
	if err != nil {
		return err
	}
	channelCipher, err := EncryptImagePayload(channelData, "media-channel:"+job.TaskId)
	if err != nil {
		return err
	}
	if err := model.SelectAsyncVideoRecoveryChannel(ctx, *job, *task, selected, cipher, channelCipher); err != nil {
		return err
	}
	*task = selected
	job.ChannelId, job.RequestCipher, job.ChannelCipher = candidate.Id, cipher, channelCipher
	return nil
}

// RecheckVideoRecoveryPolicy runs again immediately before a queued send. It
// returns the live decision without incrementing usage counters.
func RecheckVideoRecoveryPolicy(ctx context.Context, job model.AsyncMediaJob, task model.Task, request AsyncVideoRequest) (*mediaretry.Decision, error) {
	policy, err := GetMediaRetryPolicy(ctx)
	if err != nil {
		return nil, errors.Join(ErrVideoRetryDeferred, err)
	}
	if policy.PauseResubmissions {
		return nil, ErrVideoRetryDeferred
	}
	RestoreVideoRetryFact(task, job, &request)
	if request.RetryFact == nil {
		return nil, nil
	}
	state := videoRetryState(task, job)
	state.Now = time.Now().Unix()
	cfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return nil, errors.Join(ErrVideoRetryDeferred, err)
	}
	state.TotalLimit = min(state.TotalLimit, cfg.TotalRetries)
	decision, err := PlanTerminalVideoFailure(policy, *request.RetryFact, state)
	if err != nil {
		return nil, err
	}
	if !decision.Matched || !mediaretry.IsResubmission(decision.Action) {
		return &decision, errors.New("video recovery is no longer permitted by the current policy")
	}
	return &decision, nil
}

// RestoreVideoRetryFact upgrades a legacy waiting recovery from its recorded
// confirmed failure, so newly saved rules apply before the next retry.
func RestoreVideoRetryFact(task model.Task, job model.AsyncMediaJob, request *AsyncVideoRequest) {
	if request == nil || request.RetryFact != nil {
		return
	}
	var legacyEvidence *mediaretry.VideoFailureEvidence
	if job.Provider == "drama" && job.ReferenceRetryCount == 1 && task.AttemptGeneration == 1 && task.Status == model.TaskStatusQueued && task.PrivateData.UpstreamTaskID == "" && task.PrivateData.ResultURL == "" && request.ReferenceRecoveryVersion == 1 && request.PendingDecision == nil && (job.ReferenceRecoveryStage == "prepare" || job.ReferenceRecoveryStage == "submit") {
		// The old host already validated and scheduled this confirmed failure.
		// Only that durable transition permits restoring its built-in evidence.
		for _, attempt := range task.PrivateData.VideoAttempts {
			if attempt.Generation == 0 && attempt.Status == string(model.TaskStatusFailure) && attempt.UpstreamTaskID != "" {
				legacyEvidence = &mediaretry.VideoFailureEvidence{LegacyReferenceEligible: isAsyncVideoReferenceDownloadFailure(attempt.Reason), LegacyBusyEligible: isAsyncVideoBusyFailure(attempt.Reason)}
				break
			}
		}
	}
	if previous := task.PrivateData.MediaRetryFact; previous != nil && previous.Stage == "terminal" && previous.Terminal && !previous.Unknown && !previous.HasOutput && previous.Provider == job.Provider && previous.Model == job.Model {
		fact := NormalizeMediaRetryFact(*previous)
		fact.LocalSupported = fact.LocalSupported && !request.ReferenceLocalizationUnsupported
		if fact.VideoEvidence == nil {
			fact.VideoEvidence = legacyEvidence
		}
		request.RetryFact, request.RetryFailedAt = &fact, task.PrivateData.MediaRetryFailedAt
		return
	}
	reason, channelID, failedAt := "", task.ChannelId, int64(0)
	for index := len(task.PrivateData.VideoAttempts) - 1; index >= 0; index-- {
		attempt := task.PrivateData.VideoAttempts[index]
		if attempt.Status == string(model.TaskStatusFailure) && attempt.Reason != "" {
			reason, failedAt = attempt.Reason, attempt.FinishedAt
			if attempt.ChannelID > 0 {
				channelID = attempt.ChannelID
			}
			break
		}
	}
	if reason == "" && task.Status == model.TaskStatusQueued && task.PrivateData.UpstreamTaskID == "" && (job.ReferenceRecoveryStage == "prepare" || job.ReferenceRecoveryStage == "submit") {
		reason = job.ErrorMessage
	}
	if reason == "" {
		return
	}
	fact := NormalizeMediaRetryFact(mediaretry.Fact{VideoEvidence: legacyEvidence, MediaType: "video", Provider: job.Provider, Model: job.Model, ChannelID: channelID, Stage: "terminal", ErrorCode: "upstream_failed", Category: "unclassified", Message: reason, Accepted: true, Terminal: true, HasReferences: len(request.ReferenceInputs) > 0, LocalSupported: len(request.ReferenceInputs) > 0 && !request.ReferenceLocalizationUnsupported, Deadline: request.ReferenceRecoveryDeadline})
	if isAsyncVideoReferenceDownloadFailure(fact.Message) {
		fact.Category = "reference_download"
	} else if isAsyncVideoBusyFailure(fact.Message) {
		fact.Category = "capacity"
	}
	request.RetryFact, request.RetryFailedAt = &fact, failedAt
}

// VideoPostprocessDecision applies rules to local work only. A generated output
// can never turn a download/billing problem into another generation request.
func VideoPostprocessDecision(ctx context.Context, job model.AsyncMediaJob, task *model.Task, stage, message string, begin bool) (mediaretry.Decision, error) {
	fact := mediaretry.Fact{MediaType: "video", Provider: job.Provider, Model: job.Model, ChannelID: task.ChannelId, Stage: stage, Message: message, ErrorCode: "video_" + stage + "_failed", Accepted: true, Terminal: true, HasOutput: true, HasUpstreamTask: task.PrivateData.UpstreamTaskID != "", Deadline: job.ExpiresAt}
	fact.Category = "storage"
	if stage == "billing" {
		fact.Category = "billing"
	}
	if begin && task.PrivateData.MediaRetryFact != nil && task.PrivateData.MediaRetryFact.Stage == stage {
		fact = *task.PrivateData.MediaRetryFact
	}
	fact = NormalizeMediaRetryFact(fact)
	state := videoRetryState(*task, job)
	decision, err := evaluateVideoRetry(ctx, fact, &state)
	if err != nil {
		return decision, err
	}
	// Legacy successes with no recovery history do not create synthetic errors.
	if !decision.Matched && begin {
		return decision, nil
	}
	consume := begin && (decision.Action == mediaretry.ResumeDownload || decision.Action == mediaretry.ResumeBilling)
	if err := model.RecordAsyncVideoRetryDecision(ctx, *task, job, fact, decision, state, consume); err != nil {
		return decision, err
	}
	task.PrivateData.MediaRetryDecision = &decision
	task.PrivateData.MediaRetryFact = &fact
	if consume {
		state = mediaretry.Consume(state, decision)
		task.PrivateData.MediaRetryState = &state
	}
	return decision, nil
}

func videoPollingDecision(ctx context.Context, task *model.Task, reason string, statusCode int, begin bool, recorded ...*mediaretry.Fact) (mediaretry.Decision, error) {
	var job model.AsyncMediaJob
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", task.TaskID, task.UserId).Take(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return mediaretry.Decision{}, nil
		}
		return mediaretry.Decision{}, err
	}
	fact := mediaretry.Fact{MediaType: "video", Provider: job.Provider, Model: job.Model, ChannelID: task.ChannelId, Stage: "poll", HTTPStatus: statusCode, ErrorCode: "upstream_result_unknown", Category: "execution_unknown", Message: reason, Unknown: true, Accepted: true, HasUpstreamTask: task.PrivateData.UpstreamTaskID != ""}
	if plain, err := DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId); err == nil {
		var request AsyncVideoRequest
		if common.Unmarshal(plain, &request) == nil {
			fact.HasReferences, fact.LocalSupported = len(request.ReferenceInputs) > 0, !request.ReferenceLocalizationUnsupported && request.ReferenceRecoveryVersion > 0
			fact.Deadline = request.ReferenceRecoveryDeadline
		}
	}
	if len(recorded) > 0 && recorded[0] != nil && recorded[0].Stage == "poll" {
		// Readiness rechecks the same captured failure after a policy change;
		// it has no new HTTP response and must retain the original match facts.
		fact = *recorded[0]
	} else if begin && task.PrivateData.MediaRetryFact != nil && task.PrivateData.MediaRetryFact.Stage == "poll" {
		fact = *task.PrivateData.MediaRetryFact
	}
	if task.PrivateData.MediaRetryFact != nil && task.PrivateData.MediaRetryFact.HasOutput {
		fact.HasOutput = true
	}
	fact = NormalizeMediaRetryFact(fact)
	state := videoRetryState(*task, job)
	decision, err := evaluateVideoRetry(ctx, fact, &state)
	if err != nil {
		return decision, err
	}
	if applied, err := ApplyConfiguredVideoDecision(ctx, *task, job, fact, &decision, state); err != nil {
		return decision, err
	} else if applied {
		if err := model.DB.WithContext(ctx).First(task, task.ID).Error; err != nil {
			return decision, err
		}
		return decision, nil
	}
	consume := begin && decision.Matched && decision.Action == mediaretry.PollSameJob
	if !begin || decision.Matched {
		if err := model.RecordAsyncVideoRetryDecision(ctx, *task, job, fact, decision, state, consume); err != nil {
			return decision, err
		}
		task.PrivateData.MediaRetryFact, task.PrivateData.MediaRetryDecision = &fact, &decision
		if consume {
			state = mediaretry.Consume(state, decision)
			task.PrivateData.MediaRetryState = &state
		}
	}
	return decision, nil
}

var ErrVideoBillingRecoveryHeld = errors.New("video billing recovery requires review or its configured delay")

// VideoBillingRetryBoundary observes the same upstream job again for its usage
// before settlement. The saved output fact is a permanent no-regeneration guard.
func VideoBillingRetryBoundary(ctx context.Context, task *model.Task, diagnostic error, begin bool) error {
	if task == nil || !task.PrivateData.AsyncMedia || task.Status != model.TaskStatusSuccess {
		return nil
	}
	var current model.Task
	if err := model.DB.WithContext(ctx).First(&current, task.ID).Error; err != nil {
		return err
	}
	if current.Status == model.TaskStatusSuccess || current.Status == model.TaskStatusFailure {
		return nil
	}
	if begin && (current.PrivateData.MediaRetryFact == nil || current.PrivateData.MediaRetryFact.Stage != "billing" || current.PrivateData.MediaRetryFact.ErrorCode != "video_billing_failed") {
		return nil
	}
	var job model.AsyncMediaJob
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", task.TaskID, task.UserId).Take(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}

	if begin && current.PrivateData.MediaRetryFact != nil {
		state := videoRetryState(current, job)
		decision, err := evaluateVideoRetry(ctx, *current.PrivateData.MediaRetryFact, &state)
		if err != nil {
			return err
		}
		if decision.Matched && (decision.Action != mediaretry.ResumeBilling || state.Now < current.PrivateData.MediaRetryFailedAt+int64(decision.DelaySeconds)) {
			return ErrVideoBillingRecoveryHeld
		}
	}
	message := ""
	if diagnostic != nil {
		message = diagnostic.Error()
	}
	decision, err := VideoPostprocessDecision(ctx, job, &current, "billing", message, begin)
	if err != nil {
		return err
	}
	if begin && decision.Matched && (decision.Action != mediaretry.ResumeBilling || time.Now().Unix() < current.PrivateData.MediaRetryFailedAt+int64(decision.DelaySeconds)) {
		return ErrVideoBillingRecoveryHeld
	}
	return nil
}

func evaluateVideoRetry(ctx context.Context, fact mediaretry.Fact, state *mediaretry.State) (mediaretry.Decision, error) {
	cfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return mediaretry.Decision{}, err
	}
	state.TotalLimit = min(state.TotalLimit, cfg.TotalRetries)
	return EvaluateMediaRetry(ctx, fact, *state)
}
