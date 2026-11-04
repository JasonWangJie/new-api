package service

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"gorm.io/gorm"
)

// AsyncVideoReferenceInput identifies a slot in the canonical request. Display
// snapshots and download completion order must never determine these positions.
type AsyncVideoReferenceInput struct {
	Index      int    `json:"index"`
	Kind       string `json:"kind"`
	Role       string `json:"role"`
	Source     string `json:"source"`
	Path       string `json:"path"`
	KindIndex  int    `json:"kind_index"`
	ObjectId   string `json:"object_id,omitempty"`
	ObjectSlot string `json:"object_slot,omitempty"`
}

func (input AsyncVideoReferenceInput) StorageSlot() string {
	if input.ObjectSlot != "" {
		return input.ObjectSlot
	}
	return strconv.Itoa(input.Index)
}

var SubmitAsyncVideoReferenceRetryFunc func(context.Context, model.AsyncMediaJob, model.Task, AsyncVideoRequest) error

// AsyncVideoReferenceRejectedError is reserved for a parsed, explicit upstream
// failure. Transport errors and unparseable receipts remain unknown.
type AsyncVideoReferenceRejectedError struct{ Reason string }

func (err *AsyncVideoReferenceRejectedError) Error() string { return err.Reason }

var (
	videoReferenceURLPattern            = regexp.MustCompile(`(?i)\bhttps?://[^\s\p{Han}<>"'(),;，。；！？、（）【】《》]+`)
	videoReferenceInputPattern          = regexp.MustCompile(`素材|参考(?:图(?:片|像)?|视频|音频|资源|文件|链接)|引用(?:资源|素材|图片|图像|视频|音频|文件|链接)|输入(?:图片|图像|视频|音频|资源|文件)|\b(?:references?|materials?|assets?)\b|\b(?:input|source|referenced|uploaded)[ _-]+(?:images?|videos?|audio|media|files?|resources?|urls?|links?)\b|\bmedia[ _-]+(?:urls?|links?|files?|resources?)\b`)
	videoReferenceFetchPattern          = regexp.MustCompile(`下载|读取|读入|访问|拉取|获取|加载|抓取|不可达|无法连接|连不上|过期|\b(?:download(?:ing|ed)?|fetch(?:ing|ed)?|read(?:ing)?|access(?:ing|ed|ible)?|load(?:ing|ed)?|retriev(?:e|ing|ed)|unreachable|inaccessible|expired)\b`)
	videoReferenceFailedPattern         = regexp.MustCompile(`失败|超时|无法|不能|未能|不可|不通|受限|限制|受阻|拒绝|禁止|过期|错误|\b(?:fail(?:ed|ure|ing)?|error|timeout|timed?[ _-]*out|unable|cannot|can['’]?t|couldn['’]t|could[ _-]+not|not[ _-]+(?:publicly[ _-]+)?accessible|unreachable|inaccessible|restricted|restrictions?|blocked|forbidden|denied|expired)\b`)
	videoReferenceFetchSucceededPattern = regexp.MustCompile(`成功(?:下载|读取|读入|访问|拉取|获取|加载|抓取)|(?:下载|读取|读入|访问|拉取|获取|加载|抓取)(?:已经|已)?成功|(?:没有|并未|未|不)(?:发生)?(?:下载|读取|读入|访问|拉取|获取|加载|抓取)?(?:失败|超时|过期|受限)|不受(?:到)?(?:访问)?限制|\b(?:download(?:ed|ing)?|fetch(?:ed|ing)?|read|access(?:ed|ing)?|load(?:ed|ing)?)[ _-]+(?:successfully|succeeded)\b|\bsuccessfully[ _-]+(?:download(?:ed|ing)?|fetch(?:ed|ing)?|read|access(?:ed|ing)?|load(?:ed|ing)?)\b|\bno[ _-]+(?:failures?|errors?|timeouts?)[ _-]+(?:occurred|happened|(?:was|were)[ _-]+reported)\b|\b(?:not|never|didn['’]t|doesn['’]t)[ _-]+(?:fail(?:ed)?|timeout|timed?[ _-]*out|expired|restricted|blocked|unreachable|inaccessible)\b`)
	videoReferenceOtherFailurePattern   = regexp.MustCompile(`参数|尺寸|宽高|分辨率|时长|不支持|格式|解码|损坏|損壞|审核|违规|不合规|安全|版权|鉴权|认证|凭证|登录|登錄|限流|余额|额度|成片|输出|生成结果|生成产物|\b(?:invalid|unsupported|decod(?:e|ed|ing|er)|corrupt(?:ed|ion)?|parameters?|aspect[ _-]?ratio|dimensions?|resolution|width|height|duration|safety|censor(?:ship)?|moderation|policy|copyright|violation|auth(?:entication|orization)?|unauthenticated|unauthorized|credentials?|login|log[ _-]+in|api[ _-]?key|access[ _-]?token|rate[ _-]?limit|quota|outputs?|results?|generated[ _-]+(?:video|media))\b`)
	videoBusyPattern                    = regexp.MustCompile(`(?:系统|服务(?:器)?|上游)(?:当前|暂时|正在|目前)?(?:非常|过于)?(?:繁忙|忙碌|忙)|\b(?:(?:server|system|service)(?: is)?(?: currently| temporarily)? busy|overloaded|service(?: is)?(?: temporarily)? unavailable|temporarily unavailable)\b`)
	videoBusyNegatedPattern             = regexp.MustCompile(`(?:并不|并未|没有|不是|并非|不|未)(?:处于|出现|发生|存在)?(?:系统|服务(?:器)?|上游)?(?:当前|暂时)?(?:繁忙|忙碌|忙|不可用)|\b(?:not|never|no|isn['’]?t|aren['’]?t)\b[^.;。；]{0,48}\b(?:busy|overloaded|unavailable)\b`)
)

func isAsyncVideoReferenceDownloadFailure(reason string) bool {
	// URL paths and signed parameters are opaque, not failure diagnostics. Mask
	// them before normalization, preserving sentence ends and attached CJK text.
	reason = videoReferenceURLPattern.ReplaceAllStringFunc(reason, func(value string) string {
		trimmed := strings.TrimRight(value, ".!?")
		return " [url]" + value[len(trimmed):]
	})
	reason = strings.NewReplacer("_", " ", "-", " ").Replace(reason)
	reason = strings.ToLower(strings.Join(strings.Fields(reason), " "))
	if videoReferenceOtherFailurePattern.MatchString(reason) {
		return false
	}
	if !videoReferenceInputPattern.MatchString(reason) {
		return false
	}
	// Keep the failed fetch in one diagnostic clause. A successfully downloaded
	// reference followed by an unrelated generation failure is not recoverable.
	reason = strings.NewReplacer("但是", "，", "但", "，", " but ", "，", "generation failed", "", "generation failure", "", "生成失败", "").Replace(reason)
	for _, clause := range strings.FieldsFunc(reason, func(character rune) bool {
		return strings.ContainsRune("。！？.!?;；,，", character)
	}) {
		if videoReferenceFetchSucceededPattern.MatchString(clause) {
			continue
		}
		if videoReferenceInputPattern.MatchString(clause) && videoReferenceFetchPattern.MatchString(clause) && videoReferenceFailedPattern.MatchString(clause) {
			return true
		}
	}
	return false
}

// isAsyncVideoBusyFailure recognizes a temporary capacity failure, while
// excluding permanent failures even when they also suggest trying again.
func isAsyncVideoBusyFailure(reason string) bool {
	reason = videoReferenceURLPattern.ReplaceAllString(reason, " [url] ")
	reason = strings.ToLower(strings.Join(strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(reason)), " "))
	if videoReferenceOtherFailurePattern.MatchString(reason) || videoReferenceFetchSucceededPattern.MatchString(reason) || videoBusyNegatedPattern.MatchString(reason) {
		return false
	}
	return videoBusyPattern.MatchString(reason)
}

// FreezeAsyncVideoReferenceRecovery freezes replay inputs after validation and
// before the first dispatch. Existing Drama requests retain canonical slots.
func FreezeAsyncVideoReferenceRecovery(request *AsyncVideoRequest, canonical any, expiresAt int64) error {
	if request == nil || request.Execution == nil || request.Execution.TaskPlugin == nil {
		return nil
	}
	if request.Execution.TaskPlugin.Key == "xai" {
		request.InlineReferenceInputs = xaiInlineReferenceInputs(canonical)
	}
	if request.MixedRouting != nil {
		return freezeVideoRetryInputs(request, expiresAt)
	}
	if request.Execution.TaskPlugin.Key != "drama" {
		return freezeVideoRetryInputs(request, expiresAt)
	}
	body, ok := canonical.(map[string]any)
	if !ok {
		return nil
	}
	references, ok := body["references"].([]any)
	if body["references"] != nil && !ok || len(references) > 64 {
		return nil
	}
	now := time.Now().Unix()
	if constant.TaskTimeoutMinutes <= 0 {
		return nil
	}
	deadline := now + int64(constant.TaskTimeoutMinutes)*60
	// A replacement may be dispatched just before the original deadline. If
	// no TTL was supplied, cover the provider's maximum permitted TTL without
	// inserting or changing a generation parameter in the request.
	upstreamLifetime := int64(259200)
	if value, exists := body["execution_expires_after"]; exists {
		seconds, valid := videoNumber(value)
		if !valid || seconds < 3600 || seconds > 259200 {
			return errors.New("invalid video reference execution expiry")
		}
		upstreamLifetime = int64(seconds)
	}
	urlExpiresAt := deadline + upstreamLifetime + 3600
	if deadline <= now || urlExpiresAt > now+7*86400 || urlExpiresAt > expiresAt {
		return nil
	}
	counts := make(map[string]int)
	inputs := make([]AsyncVideoReferenceInput, len(references))
	for index, value := range references {
		reference, ok := value.(map[string]any)
		if !ok {
			return errors.New("invalid canonical video reference")
		}
		kind, _ := reference["type"].(string)
		role, _ := reference["role"].(string)
		source, _ := reference["source"].(string)
		if (kind != "image" && kind != "video" && kind != "audio") || source == "" {
			return errors.New("invalid canonical video reference slot")
		}
		counts[kind]++
		inputs[index] = AsyncVideoReferenceInput{Index: index, Kind: kind, Role: role, Source: source, Path: "/references/" + strconv.Itoa(index) + "/source", KindIndex: counts[kind]}
	}
	encoded, err := common.Marshal(body)
	if err != nil {
		return err
	}
	request.Body, request.ContentType = encoded, "application/json"
	request.OriginalBody = append([]byte(nil), encoded...)
	request.ReferenceInputs, request.ReferenceRecoveryVersion = inputs, 1
	request.ReferenceRecoveryDeadline, request.ReferenceURLExpiresAt = deadline, urlExpiresAt
	return nil
}

// videoRetryInputSelectors gives admission and replay the same plugin capabilities.
func videoRetryInputSelectors(plugin *jsplugin.LoadedPlugin) (selectors []jsplugin.VideoRetryInput, legacyDrama bool) {
	if len(plugin.Meta.VideoRetryInputs) > 0 || plugin.Meta.Key != "drama" {
		return plugin.Meta.VideoRetryInputs, false
	}
	// Older site overrides predate videoRetryInputs. Keep explicit declarations
	// authoritative and other plugins opt-in.
	return []jsplugin.VideoRetryInput{
		{Path: "/references/*/source", Kind: "media"},
		{Path: "/reference_images/*", Kind: "image"},
		{Path: "/images/*", Kind: "image"},
		{Path: "/image_urls/*", Kind: "image"},
		{Path: "/referenceImages/*", Kind: "image"},
		{Path: "/reference_videos/*", Kind: "video"},
		{Path: "/videos/*", Kind: "video"},
		{Path: "/video_urls/*", Kind: "video"},
		{Path: "/referenceVideos/*", Kind: "video"},
		{Path: "/reference_audios/*", Kind: "audio"},
		{Path: "/audios/*", Kind: "audio"},
		{Path: "/audio_urls/*", Kind: "audio"},
		{Path: "/referenceAudios/*", Kind: "audio"},
		{Path: "/image", Kind: "image"},
		{Path: "/input_reference", Kind: "image"},
		{Path: "/first_image", Kind: "image"},
		{Path: "/last_frame", Kind: "image"},
		{Path: "/last_image", Kind: "image"},
		{Path: "/video", Kind: "video"},
	}, true
}

func freezeVideoRetryInputs(request *AsyncVideoRequest, expiresAt int64) error {
	plugin, found := jsplugin.DefaultRegistry.Get(request.Execution.TaskPlugin.Key)
	if !found || constant.TaskTimeoutMinutes <= 0 {
		return nil
	}
	now := time.Now().Unix()
	request.ReferenceRecoveryVersion = 2
	request.ReferenceRecoveryDeadline = min(now+int64(constant.TaskTimeoutMinutes)*60, expiresAt)
	request.ReferenceURLExpiresAt = min(request.ReferenceRecoveryDeadline+259200+3600, expiresAt, now+7*86400)
	request.OriginalBody = append([]byte(nil), request.Body...)
	if !strings.HasPrefix(strings.ToLower(request.ContentType), "application/json") {
		return nil
	}
	var body map[string]any
	if err := common.Unmarshal(request.Body, &body); err != nil {
		return err
	}
	selectors, legacyDrama := videoRetryInputSelectors(plugin)
	seen := make(map[string]bool)
	counts := make(map[string]int)
	for _, selector := range selectors {
		slots := []videoRetrySlot{{Value: body}}
		for part := range strings.SplitSeq(strings.TrimPrefix(selector.Path, "/"), "/") {
			var next []videoRetrySlot
			for _, slot := range slots {
				switch value := slot.Value.(type) {
				case map[string]any:
					if child, ok := value[part]; ok {
						next = append(next, videoRetrySlot{Path: slot.Path + "/" + part, Value: child, Parent: value})
					}
				case []any:
					if part == "*" {
						for i, child := range value {
							next = append(next, videoRetrySlot{Path: slot.Path + "/" + strconv.Itoa(i), Value: child})
						}
					}
				}
			}
			slots = next
		}
		for _, slot := range slots {
			source, ok := slot.Value.(string)
			if object, wrapped := slot.Value.(map[string]any); legacyDrama && wrapped {
				field := "url"
				source, ok = object[field].(string)
				if source == "" {
					field = "source"
					source, ok = object[field].(string)
				}
				slot.Path += "/" + field
			}
			scheme, _, absolute := strings.Cut(source, "://")
			if !ok || seen[slot.Path] || !absolute || !(strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https")) {
				continue
			}
			kind := selector.Kind
			role, _ := slot.Parent["role"].(string)
			if kind == "media" {
				kind, _ = slot.Parent["type"].(string)
				switch kind {
				case "first_frame", "last_frame", "reference_image":
					kind = "image"
				case "first_clip", "reference_video":
					kind = "video"
				case "driving_audio", "reference_audio":
					kind = "audio"
				}
			}
			if kind != "image" && kind != "video" && kind != "audio" {
				request.ReferenceLocalizationUnsupported = true
				continue
			}
			if len(request.ReferenceInputs) >= 64 {
				return errors.New("video retry references exceed the supported limit")
			}
			seen[slot.Path] = true
			counts[kind]++
			request.ReferenceInputs = append(request.ReferenceInputs, AsyncVideoReferenceInput{Index: len(request.ReferenceInputs), Kind: kind, Role: role, Source: source, Path: slot.Path, KindIndex: counts[kind]})
		}
	}
	return nil
}

type videoRetrySlot struct {
	Path   string
	Value  any
	Parent map[string]any
}

// videoRetryReferenceValue resolves an exact frozen input slot. Unlike a JSON
// patch it cannot create keys, grow arrays, or alter any other request content.
func videoRetryReferenceValue(body map[string]any, path string, replacement *string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	var current any = body
	for index, part := range parts {
		last := index == len(parts)-1
		switch value := current.(type) {
		case map[string]any:
			child, ok := value[part]
			if !ok {
				return "", ErrMediaInputUnavailable
			}
			if last {
				source, ok := child.(string)
				if !ok {
					return "", ErrMediaInputUnavailable
				}
				if replacement != nil {
					value[part] = *replacement
				}
				return source, nil
			}
			current = child
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(value) {
				return "", ErrMediaInputUnavailable
			}
			if last {
				source, ok := value[i].(string)
				if !ok {
					return "", ErrMediaInputUnavailable
				}
				if replacement != nil {
					value[i] = *replacement
				}
				return source, nil
			}
			current = value[i]
		default:
			return "", ErrMediaInputUnavailable
		}
	}
	return "", ErrMediaInputUnavailable
}

// TryAsyncVideoReferenceRecovery runs before the terminal billing transition.
// A transport or ambiguous polling failure must never reach this decision.
func TryAsyncVideoReferenceRecovery(ctx context.Context, task model.Task, result *relaycommon.TaskInfo, httpStatus int, response []byte) (bool, error) {
	if !task.PrivateData.AsyncMedia || task.PrivateData.UpstreamTaskID == "" || result == nil || result.Status != string(model.TaskStatusFailure) || httpStatus < http.StatusOK || httpStatus >= http.StatusMultipleChoices {
		return false, nil
	}
	if fact := task.PrivateData.MediaRetryFact; fact != nil && fact.HasOutput {
		return true, errors.New("video output was already confirmed; reconcile the original result and billing instead of regenerating or refunding")
	}
	_, recovered, err := tryConfiguredVideoRecovery(ctx, task, result, httpStatus, response)
	return recovered, err
}

func tryConfiguredVideoRecovery(ctx context.Context, task model.Task, result *relaycommon.TaskInfo, httpStatus int, response []byte) (bool, bool, error) {
	var job model.AsyncMediaJob
	if err := model.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", task.TaskID, task.UserId).Take(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, false, nil
		}
		return false, false, err
	}
	var request AsyncVideoRequest
	plain, decryptErr := DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
	requestErr := decryptErr
	if requestErr == nil {
		requestErr = common.Unmarshal(plain, &request)
	}
	fact := mediaretry.Fact{MediaType: "video", Provider: job.Provider, Model: job.Model, ChannelID: task.ChannelId, Stage: "terminal", HTTPStatus: httpStatus, ErrorCode: "upstream_failed", Message: result.Reason, Accepted: true, Terminal: true, HasUpstreamTask: task.PrivateData.UpstreamTaskID != "", HasReferences: request.HasReferenceInputs(), LocalSupported: request.HasReferenceInputs() && !request.ReferenceLocalizationUnsupported, Deadline: request.ReferenceRecoveryDeadline}
	fact.Category = "unclassified"
	if isAsyncVideoReferenceDownloadFailure(fact.Message) {
		fact.Category = "reference_download"
	} else if isAsyncVideoBusyFailure(fact.Message) {
		fact.Category = "capacity"
	}
	output, conflict := videoRecoveryResponseEvidence(response, task.PrivateData.UpstreamTaskID, result.Reason)
	fact.HasOutput = output || result.Url != "" || result.RemoteUrl != "" || task.PrivateData.ResultURL != ""
	fact.Unknown = conflict || fact.HasOutput || result.TaskID != "" && result.TaskID != task.PrivateData.UpstreamTaskID
	if fact.Unknown {
		fact.Terminal, fact.Stage, fact.ErrorCode = false, "poll", "upstream_result_conflicting"
		fact.VideoEvidence = &mediaretry.VideoFailureEvidence{TerminalResponseConflict: true}
	}
	if requestErr != nil && !fact.Unknown {
		if decryptErr != nil {
			return false, false, nil
		}
		return false, false, requestErr
	}
	if task.Platform == "drama" && task.AttemptGeneration == 0 && job.ReferenceRetryCount == 0 && !fact.Unknown {
		var snapshot map[string]any
		if common.Unmarshal(response, &snapshot) == nil && snapshot != nil && snapshot["status"] == "failed" && !videoReferenceOutputEvidence(snapshot, result.Reason, 0) {
			fact.VideoEvidence = &mediaretry.VideoFailureEvidence{LegacyReferenceEligible: isAsyncVideoReferenceDownloadFailure(result.Reason), LegacyBusyEligible: isAsyncVideoBusyFailure(result.Reason)}
		}
	}
	fact = NormalizeMediaRetryFact(fact)
	state := videoRetryState(task, job)
	policy, err := GetMediaRetryPolicy(ctx)
	if err != nil {
		return true, false, err
	}
	cfg, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return true, false, err
	}
	state.TotalLimit = min(state.TotalLimit, cfg.TotalRetries)
	decision, err := PlanTerminalVideoFailure(policy, fact, state)
	if err != nil {
		return true, false, err
	}
	if applied, err := ApplyConfiguredVideoDecision(ctx, task, job, fact, &decision, state); err != nil || applied {
		return true, true, err
	}
	if fact.Unknown {
		// A failure envelope containing output, an accepted status, or another
		// native identity cannot prove that this generation failed. Retain its
		// receipt and financial hold independently of editable matching rules.
		if err := model.RecordAsyncVideoRetryDecision(ctx, task, job, fact, decision, state, false); err != nil {
			return true, true, err
		}
		cfg, err := GetMediaRuntimeConfig(ctx)
		if err != nil {
			return true, true, err
		}
		before := task.Snapshot()
		reconciliation := model.VideoTaskReconciliation{}
		if task.PrivateData.VideoReconciliation != nil {
			reconciliation = *task.PrivateData.VideoReconciliation
		}
		now := time.Now().Unix()
		if reconciliation.StartedAt == 0 {
			reconciliation.StartedAt, reconciliation.DeadlineAt = now, now+int64(cfg.ReconciliationTimeout)*60
		}
		reconciliation.Revision++
		reconciliation.Reason = "Upstream failure conflicts with output, status, or task identity; reconcile the original operation"
		reconciliation.ManualRequired = reconciliation.ManualRequired || decision.Action != mediaretry.PollSameJob || now >= reconciliation.DeadlineAt
		reconciliation.NextPollAt = 0
		if !reconciliation.ManualRequired {
			reconciliation.NextPollAt = min(reconciliation.DeadlineAt, now+int64(max(cfg.ReconciliationPollInterval, decision.DelaySeconds)))
		}
		task.PrivateData.MediaRetryFact, task.PrivateData.MediaRetryDecision, task.PrivateData.VideoReconciliation = &fact, &decision, &reconciliation
		_, err = task.UpdatePollingState(ctx, before)
		return true, true, err
	}
	if !decision.Matched {
		if err := model.RecordAsyncVideoRetryDecision(ctx, task, job, fact, decision, state, false); err != nil {
			return true, false, err
		}
		return false, false, nil
	}
	if !mediaretry.IsResubmission(decision.Action) {
		if err := model.RecordAsyncVideoRetryDecision(ctx, task, job, fact, decision, state, false); err != nil {
			return true, false, err
		}
		if decision.BlockedReason == "resubmissions_paused" {
			return true, true, nil
		}
		if decision.Action == mediaretry.ManualReview || decision.Action == mediaretry.PollSameJob {
			before := task.Snapshot()
			task.PrivateData.MediaRetryDecision = &decision
			task.PrivateData.VideoReconciliation = &model.VideoTaskReconciliation{StartedAt: time.Now().Unix(), DeadlineAt: time.Now().Unix(), ManualRequired: true, Reason: fact.Message, Revision: 1}
			_, err := task.UpdatePollingState(ctx, before)
			return true, true, err
		}
		return true, false, nil
	}
	if job.BillingStatus == "reserved" && job.AttemptGeneration == task.AttemptGeneration &&
		(job.Status == "submitting" || job.Status == "queued" || job.Status == "submitted" && (job.Quota != task.Quota || !task.PrivateData.SubmitAccountingRecorded || !task.PrivateData.DurableBilling)) {
		// A visible native receipt may precede the original submit worker's
		// accounting projection. Keep that worker's hold until it is complete.
		return true, true, nil
	}
	blockedReason := ""
	switch {
	case request.ReferenceRecoveryVersion == 0 || len(request.Body) == 0:
		blockedReason = "request_replay_unavailable"
	case request.ReferenceRecoveryDeadline <= time.Now().Unix():
		blockedReason = "execution_deadline_reached"
	case job.BillingStatus != "reserved" || !task.VideoRecoveryQuotaValid():
		blockedReason = "reservation_unavailable"
	case !task.PrivateData.SubmitAccountingRecorded || !task.PrivateData.DurableBilling:
		blockedReason = "submission_accounting_unavailable"
	case task.PrivateData.UpstreamTaskID == "":
		blockedReason = "upstream_task_unavailable"
	}
	if blockedReason != "" {
		decision.Action, decision.BlockedReason = mediaretry.Stop, blockedReason
		if err := model.RecordAsyncVideoRetryDecision(ctx, task, job, fact, decision, state, false); err != nil {
			return true, false, err
		}
		return true, false, nil
	}
	request.RetryFact, request.PendingDecision = &fact, &decision
	request.RetryFailedAt = time.Now().Unix()
	if len(request.OriginalBody) > 0 {
		request.Body = append([]byte(nil), request.OriginalBody...)
		request.LocalReferencesReady = false
	}
	encoded, err := common.Marshal(request)
	if err != nil {
		return true, false, err
	}
	cipher, err := EncryptImagePayload(encoded, "media-request:"+job.TaskId)
	if err != nil {
		return true, false, err
	}
	local := decision.Action == mediaretry.LocalSameChannel || decision.Action == mediaretry.LocalOtherChannel
	won, err := model.ScheduleAsyncVideoReferenceRecovery(ctx, task, job, fact.Message, local, model.AsyncVideoRecoveryPlan{Decision: decision, Fact: fact, State: state, RequestCipher: cipher})
	return true, won, err
}

func videoRetryState(task model.Task, job model.AsyncMediaJob) mediaretry.State {
	used := job.ReferenceRetryCount
	if job.ReferenceRecoveryStage == "prepare" || job.ReferenceRecoveryStage == "submit" {
		used = max(used-1, 0)
	}
	state := mediaretry.State{TotalUsed: used, TotalLimit: 16, Now: time.Now().Unix()}
	if task.PrivateData.MediaRetryState != nil {
		state = *task.PrivateData.MediaRetryState
		state.Now = time.Now().Unix()
	}
	return state
}

// videoRecoveryResponseEvidence checks protocol evidence, not error keywords.
// Input echoes are excluded so a reference URL is never mistaken for an output.
func videoRecoveryResponseEvidence(response []byte, upstreamID, diagnostic string) (bool, bool) {
	var root map[string]any
	if len(response) > 2<<20 || common.Unmarshal(response, &root) != nil || root == nil {
		return false, true
	}
	type node struct {
		value any
		depth int
	}
	pending := []node{{value: root}}
	output, conflict := false, false
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 256 {
			return output, true
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current.depth > 16 {
			return output, true
		}
		switch value := current.value.(type) {
		case []any:
			for _, child := range value {
				pending = append(pending, node{child, current.depth + 1})
			}
		case map[string]any:
			for field, child := range value {
				name := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(field))
				switch name {
				case "prompt", "input", "request", "references", "parameters", "inputimage", "inputreference":
					continue
				}
				if current.depth == 0 && (name == "id" || name == "taskid" || name == "requestid") {
					if id, ok := child.(string); !ok || id != upstreamID {
						conflict = true
					}
				}
				if name == "success" || name == "succeeded" || name == "completed" {
					if yes, ok := child.(bool); ok && yes {
						conflict = true
					}
				}
				if name == "status" || name == "state" {
					if status, ok := child.(string); ok {
						switch strings.ToLower(strings.TrimSpace(status)) {
						case "success", "succeeded", "completed", "done", "accepted", "queued", "pending", "processing", "in_progress", "in-progress", "submitted", "running":
							conflict = true
						}
					}
				}
				switch name {
				case "url", "remoteurl", "videourl", "lastframeurl", "outputurl":
					if text, ok := child.(string); ok && (strings.HasPrefix(strings.TrimSpace(text), "https://") || strings.HasPrefix(strings.TrimSpace(text), "http://") || strings.HasPrefix(strings.TrimSpace(text), "data:")) && strings.TrimSpace(text) != strings.TrimSpace(diagnostic) {
						output = true
					}
				case "result", "results", "output", "outputs", "artifacts", "video", "videos", "image", "images", "audio", "audios", "file", "files", "b64json", "b64video", "bytesbase64encoded", "base64", "lastframe":
					switch content := child.(type) {
					case nil:
					case string:
						output = output || strings.TrimSpace(content) != ""
					case []any:
						output = output || len(content) > 0
					case map[string]any:
						output = output || len(content) > 0
					default:
						output = true
					}
				}
				pending = append(pending, node{child, current.depth + 1})
			}
		}
	}
	return output, conflict
}

func videoReferenceOutputEvidence(value any, diagnostic string, depth int) bool {
	if depth > 16 {
		return true
	}
	switch typed := value.(type) {
	case map[string]any:
		for field, child := range typed {
			name := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(field))
			if name == "prompt" || name == "input" || name == "request" || name == "references" {
				continue
			}
			if name == "success" || name == "succeeded" || name == "completed" {
				if successful, ok := child.(bool); ok && successful {
					return true
				}
			}
			if name == "errorcode" || name == "code" {
				switch code := child.(type) {
				case nil:
				case string:
					if code = strings.TrimSpace(code); code != "" && code != "0" {
						return true
					}
				case float64:
					if code != 0 {
						return true
					}
				default:
					return true
				}
			}
			if text, ok := child.(string); ok {
				text = strings.ToLower(strings.TrimSpace(text))
				if name == "status" && (text == "success" || text == "succeeded" || text == "completed" || text == "done") {
					return true
				}
				if name == "error" || name == "errors" || name == "reason" || name == "message" || name == "type" {
					for _, excluded := range []string{"safety", "censor", "policy", "violation", "copyright", "invalid", "parameter", "auth", "forbidden", "permission", "rate_limit", "quota", "审核", "违规", "不合规", "版权", "参数", "安全", "鉴权", "权限"} {
						if strings.Contains(text, excluded) {
							return true
						}
					}
				}
			}
			if name == "url" || name == "remoteurl" || name == "videourl" || name == "lastframeurl" || name == "outputurl" {
				if text, ok := child.(string); ok && strings.TrimSpace(text) != "" && strings.TrimSpace(text) != strings.TrimSpace(diagnostic) {
					return true
				}
			}
			if name == "result" || name == "results" || name == "output" || name == "outputs" || name == "artifacts" || name == "video" || name == "videos" || name == "image" || name == "images" || name == "audio" || name == "audios" || name == "file" || name == "files" || name == "b64json" || name == "b64video" || name == "bytesbase64encoded" || name == "base64" || name == "lastframe" {
				switch item := child.(type) {
				case nil:
				case string:
					if strings.TrimSpace(item) != "" {
						return true
					}
				case []any:
					if len(item) > 0 {
						return true
					}
				case map[string]any:
					if len(item) > 0 {
						return true
					}
				default:
					return true
				}
			}
			if videoReferenceOutputEvidence(child, diagnostic, depth+1) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if videoReferenceOutputEvidence(child, diagnostic, depth+1) {
				return true
			}
		}
	}
	return false
}

// ProcessAsyncVideoReferenceRecovery owns its lease separately from the normal
// status worker, which must not interpret the failed attempt as task failure.
func ProcessAsyncVideoReferenceRecovery(parent context.Context, job model.AsyncMediaJob, cfg MediaRuntimeConfig) error {
	var task model.Task
	if err := model.DB.WithContext(parent).Where("task_id = ? AND user_id = ?", job.TaskId, job.UserId).Take(&task).Error; err != nil {
		return err
	}
	if task.AttemptGeneration != job.AttemptGeneration {
		return model.ErrImageConflict
	}
	updates := map[string]any{"lease_token": "", "lease_expires_at": 0, "next_attempt_at": 0}
	defer func() {
		if err := model.UpdateAsyncMediaJob(context.WithoutCancel(parent), job, updates); err != nil && !errors.Is(err, model.ErrImageConflict) {
			common.SysError("video reference recovery lease: " + err.Error())
		} else if err == nil && updates["reference_recovery_stage"] == "complete" {
			if err := RetireMediaInputObjects(context.WithoutCancel(parent), &task); err != nil {
				common.SysError("media input retirement: " + err.Error())
			}
		}
	}()
	if task.Status == model.TaskStatusFailure || task.Status == model.TaskStatusSuccess {
		job.Status = "failed"
		updates["status"], updates["billing_status"], updates["quota"], updates["reference_recovery_stage"] = "failed", "refunded", task.Quota, "complete"
		updates["error_message"] = task.FailReason
		if task.Status == model.TaskStatusSuccess {
			job.Status = "succeeded"
			updates["status"], updates["billing_status"], updates["storage_status"], updates["next_attempt_at"] = "submitted", "settled", "ready", time.Now().Unix()
		}
		updates["request_cipher"] = retainAsyncVideoRequestDetails(job)
		return nil
	}
	if job.ReferenceRecoveryStage == "dispatched" || job.ReferenceRecoveryStage == "unknown" {
		if applied, err := tryUnknownVideoRecovery(parent, task, job, "Upstream recovery submission could not be confirmed"); err != nil || applied {
			return err
		}
		_, err := model.MarkAsyncVideoReferenceSubmissionUnknown(context.WithoutCancel(parent), job, task, "Upstream recovery submission could not be confirmed; the video operation will not be repeated")
		return err
	}
	var request AsyncVideoRequest
	var err error
	if job.ReferenceRecoveryStage == "refund" {
		err = errors.New(job.ErrorMessage)
	} else {
		var plain []byte
		plain, err = DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
		if err == nil {
			err = common.Unmarshal(plain, &request)
		}
	}
	if err == nil && ((request.ReferenceRecoveryVersion != 1 && request.ReferenceRecoveryVersion != 2) || request.ReferenceRecoveryDeadline <= time.Now().Unix()) {
		err = errors.New("video reference recovery deadline expired")
	}
	if err == nil {
		RestoreVideoRetryFact(task, job, &request)
		decision, policyErr := RecheckVideoRecoveryPolicy(parent, job, task, request)
		if errors.Is(policyErr, ErrVideoRetryDeferred) {
			updates["next_attempt_at"] = time.Now().Unix() + 3
			return policyErr
		}
		err = policyErr
		if decision != nil && request.RetryFact != nil {
			if recordErr := model.RecordAsyncVideoRetryDecision(parent, task, job, *request.RetryFact, *decision, videoRetryState(task, job), false); recordErr != nil {
				updates["next_attempt_at"] = time.Now().Unix() + 3
				return recordErr
			}
			if decision.Action == mediaretry.ManualReview || decision.Action == mediaretry.PollSameJob {
				reason := "Video recovery requires manual review"
				if decision.BlockedReason != "" {
					reason += ": " + decision.BlockedReason
				}
				if holdErr := model.HoldAsyncVideoRecovery(parent, job, task, reason); holdErr != nil {
					return holdErr
				}
				updates["status"], updates["reference_recovery_stage"] = "execution_unknown", "manual"
				return nil
			}
		}
		if err == nil && decision != nil {
			request.PendingDecision = decision
			if due := request.RetryFailedAt + int64(decision.DelaySeconds); request.RetryFailedAt > 0 && due > time.Now().Unix() {
				updates["next_attempt_at"] = due
				return nil
			}
			wantsLocal := decision.Action == mediaretry.LocalSameChannel || decision.Action == mediaretry.LocalOtherChannel
			prepared := request.LocalReferencesReady
			deferredPreparation := request.Execution != nil && request.Execution.TaskPlugin != nil && request.Execution.TaskPlugin.Key == "xai"
			if wantsLocal && job.ReferenceRecoveryStage == "submit" && !prepared && !deferredPreparation {
				// A changed action re-enters the download pool rather than using
				// a submission slot for potentially slow input downloads.
				updates["reference_recovery_stage"], updates["next_attempt_at"] = "prepare", time.Now().Unix()
				return nil
			}
			if !wantsLocal {
				if len(request.OriginalBody) > 0 {
					request.Body = append([]byte(nil), request.OriginalBody...)
					request.LocalReferencesReady = false
				}
				if job.ReferenceRecoveryStage == "prepare" {
					job.ReferenceRecoveryStage = "submit"
					updates["reference_recovery_stage"] = "submit"
					err = model.UpdateAsyncMediaJob(parent, job, map[string]any{"reference_recovery_stage": "submit"})
				}
			}
		}
	}
	preparationAttempted := false
	if err == nil && job.ReferenceRecoveryStage == "prepare" {
		deadline := time.Unix(request.ReferenceRecoveryDeadline, 0)
		if downloadDeadline := time.Now().Add(time.Duration(cfg.DownloadTimeout) * time.Second); downloadDeadline.Before(deadline) {
			deadline = downloadDeadline
		}
		ctx, cancel := context.WithDeadline(parent, deadline)
		defer cancel()
		preparationAttempted = true
		request, err = PrepareAsyncVideoLocalReferences(ctx, job, request)
		if err == nil {
			encoded, encodeErr := common.Marshal(request)
			var cipher []byte
			if encodeErr == nil {
				cipher, encodeErr = EncryptImagePayload(encoded, "media-request:"+job.TaskId)
			}
			if encodeErr == nil {
				encodeErr = model.UpdateAsyncMediaJob(ctx, job, map[string]any{"reference_recovery_stage": "submit", "request_cipher": cipher})
			}
			if encodeErr != nil {
				err = encodeErr
			} else {
				// Release the download slot before taking a submission slot. Both
				// phases retain the original deadline and generation.
				updates["reference_recovery_stage"], updates["next_attempt_at"] = "submit", time.Now().Unix()
				return nil
			}
		}
	}
	if err == nil && request.PendingDecision != nil {
		err = SelectVideoRecoveryChannel(parent, &job, &task, &request)
		if err != nil {
			updates["next_attempt_at"], updates["error_message"] = time.Now().Unix()+3, VideoTaskFailureReason(err.Error())
			return err
		}
	}
	if err == nil {
		if SubmitAsyncVideoReferenceRetryFunc == nil {
			err = errors.New("video reference recovery submission is unavailable")
		} else {
			ctx, cancel := context.WithDeadline(parent, time.Unix(request.ReferenceRecoveryDeadline, 0))
			defer cancel()
			err = SubmitAsyncVideoReferenceRetryFunc(ctx, job, task, request)
		}
	}
	var inlineFailure *MediaReferencePreparationError
	if errors.As(err, &inlineFailure) {
		preparationAttempted = true
		request.RetryFact = &mediaretry.Fact{MediaType: "video", Provider: job.Provider, Model: job.Model, ChannelID: request.Channel.Id, Stage: "submit", HTTPStatus: inlineFailure.Status, ErrorCode: inlineFailure.Code, Category: "reference_download", Message: VideoTaskFailureReason(inlineFailure.Error()), HasReferences: request.HasReferenceInputs(), LocalSupported: request.HasReferenceInputs(), Deadline: request.ReferenceRecoveryDeadline}
	}
	if err != nil && preparationAttempted && request.PendingDecision != nil && request.RetryFact != nil {
		// Failed local preparation used the action even though it sent no
		// generation request. Consume it once before advancing the chain.
		decision := *request.PendingDecision
		state := videoRetryState(task, job)
		if recordErr := model.RecordAsyncVideoRetryDecision(parent, task, job, *request.RetryFact, decision, state, true, true); recordErr != nil {
			updates["next_attempt_at"] = time.Now().Unix() + 3
			return recordErr
		}
		if readErr := model.DB.WithContext(parent).First(&task, task.ID).Error; readErr != nil {
			return readErr
		}
		nextState := videoRetryState(task, job)
		decision, decisionErr := evaluateVideoRetry(parent, *request.RetryFact, &nextState)
		if decisionErr != nil {
			updates["next_attempt_at"] = time.Now().Unix() + 3
			return decisionErr
		}
		if mediaretry.IsResubmission(decision.Action) || decision.BlockedReason == "resubmissions_paused" {
			request.PendingDecision, request.RetryFailedAt = &decision, time.Now().Unix()
			if len(request.OriginalBody) > 0 {
				request.Body = append([]byte(nil), request.OriginalBody...)
				request.LocalReferencesReady = false
			}
			encoded, encodeErr := common.Marshal(request)
			if encodeErr != nil {
				return encodeErr
			}
			cipher, encodeErr := EncryptImagePayload(encoded, "media-request:"+job.TaskId)
			if encodeErr != nil {
				return encodeErr
			}
			local := decision.RequestedAction == mediaretry.LocalSameChannel || decision.RequestedAction == mediaretry.LocalOtherChannel
			won, scheduleErr := model.ScheduleAsyncVideoReferenceRecovery(parent, task, job, VideoTaskFailureReason(err.Error()), local, model.AsyncVideoRecoveryPlan{Decision: decision, Fact: *request.RetryFact, State: videoRetryState(task, job), PreparationFailed: true, RequestCipher: cipher})
			if scheduleErr != nil {
				return scheduleErr
			}
			if won {
				return nil
			}
		}
		if decision.Action == mediaretry.ManualReview || decision.Action == mediaretry.PollSameJob {
			if holdErr := model.HoldAsyncVideoRecovery(parent, job, task, "Local reference preparation requires manual review"); holdErr != nil {
				return holdErr
			}
			updates["status"], updates["reference_recovery_stage"] = "execution_unknown", "manual"
			return nil
		}
	}
	var current model.AsyncMediaJob
	if readErr := model.DB.WithContext(context.WithoutCancel(parent)).Where("id = ?", job.ID).Take(&current).Error; readErr != nil {
		return readErr
	}
	if current.AttemptGeneration != job.AttemptGeneration {
		return model.ErrImageConflict
	}
	if errors.Is(err, ErrVideoRetryDeferred) && current.DispatchedAt == 0 {
		updates["next_attempt_at"] = time.Now().Unix() + 3
		return err
	}
	if current.ReferenceRecoveryStage == "waiting" {
		updates["status"], updates["reference_recovery_stage"] = "submitted", "waiting"
		return nil
	}
	if current.ReferenceRecoveryStage == "dispatched" || current.ReferenceRecoveryStage == "unknown" {
		var rejected *AsyncVideoReferenceRejectedError
		if !errors.As(err, &rejected) {
			reason := "Upstream recovery submission could not be confirmed; the video operation will not be repeated"
			if err != nil {
				reason = VideoTaskFailureReason(reason + ": " + err.Error())
			}
			if applied, retryErr := tryUnknownVideoRecovery(context.WithoutCancel(parent), task, current, reason); retryErr != nil || applied {
				return errors.Join(err, retryErr)
			}
			_, markErr := model.MarkAsyncVideoReferenceSubmissionUnknown(context.WithoutCancel(parent), job, task, reason)
			return errors.Join(err, markErr)
		}
		// An explicit parsed rejection can continue the configured chain. The
		// old dispatch remains fenced until the next generation is committed.
		if readErr := model.DB.WithContext(parent).First(&task, task.ID).Error; readErr != nil {
			return readErr
		}
		recovered, recoveryErr := tryRejectedVideoRecovery(parent, task, current, request, rejected.Reason)
		if recoveryErr != nil {
			updates["next_attempt_at"] = time.Now().Unix() + 3
			return recoveryErr
		}
		if recovered {
			return nil
		}
	}
	if err == nil {
		err = errors.New("video reference recovery produced no durable task receipt")
	}
	reason := VideoTaskFailureReason(err.Error())
	// Persist the decision to stop generation before refunding. A failed
	// financial transaction must retry only this refund, never prepare inputs
	// or submit another upstream operation after the source becomes reachable.
	updates["reference_recovery_stage"], updates["error_message"] = "refund", reason
	if job.ReferenceRecoveryStage != "refund" {
		if persistErr := model.UpdateAsyncMediaJob(context.WithoutCancel(parent), job, map[string]any{"reference_recovery_stage": "refund", "error_message": reason}); persistErr != nil {
			updates["next_attempt_at"] = time.Now().Unix() + 3
			return persistErr
		}
	}
	task.Status, task.FailReason, task.Progress, task.FinishTime = model.TaskStatusFailure, reason, "100%", time.Now().Unix()
	if won, settleErr := SettleTaskBillingOnComplete(context.WithoutCancel(parent), nil, &task, model.TaskStatusQueued, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Reason: task.FailReason}); settleErr != nil {
		updates["next_attempt_at"] = time.Now().Unix() + 3
		return settleErr
	} else if !won {
		return model.ErrImageConflict
	}
	updates["status"], updates["billing_status"], updates["reference_recovery_stage"] = "failed", "refunded", "complete"
	updates["quota"], updates["error_message"] = 0, task.FailReason
	job.Status = "failed"
	updates["request_cipher"] = retainAsyncVideoRequestDetails(job)
	return err
}

func tryUnknownVideoRecovery(ctx context.Context, task model.Task, job model.AsyncMediaJob, reason string) (bool, error) {
	plain, err := DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
	if err != nil {
		return false, nil
	}
	var request AsyncVideoRequest
	if common.Unmarshal(plain, &request) != nil {
		return false, nil
	}
	fact := NormalizeMediaRetryFact(mediaretry.Fact{MediaType: "video", Provider: job.Provider, Model: job.Model, ChannelID: task.ChannelId, Stage: "submit", ErrorCode: "upstream_submission_unknown", Category: "execution_unknown", Message: VideoTaskFailureReason(reason), Unknown: true, Accepted: true, HasUpstreamTask: task.PrivateData.UpstreamTaskID != "", HasReferences: request.HasReferenceInputs(), LocalSupported: !request.ReferenceLocalizationUnsupported && request.ReferenceRecoveryVersion > 0, Deadline: request.ReferenceRecoveryDeadline})
	state := videoRetryState(task, job)
	decision, err := evaluateVideoRetry(ctx, fact, &state)
	if err != nil {
		return false, err
	}
	return ApplyConfiguredVideoDecision(ctx, task, job, fact, &decision, state)
}

func tryRejectedVideoRecovery(ctx context.Context, task model.Task, job model.AsyncMediaJob, request AsyncVideoRequest, reason string) (bool, error) {
	fact := mediaretry.Fact{MediaType: "video", Provider: job.Provider, Model: job.Model, ChannelID: task.ChannelId, Stage: "submit", ErrorCode: "upstream_rejected", Message: VideoTaskFailureReason(reason), Terminal: true, HasReferences: request.HasReferenceInputs(), LocalSupported: request.HasReferenceInputs() && !request.ReferenceLocalizationUnsupported, Deadline: request.ReferenceRecoveryDeadline}
	fact.Category = "unclassified"
	if isAsyncVideoReferenceDownloadFailure(fact.Message) {
		fact.Category = "reference_download"
	} else if isAsyncVideoBusyFailure(fact.Message) {
		fact.Category = "capacity"
	}
	fact = NormalizeMediaRetryFact(fact)
	state := videoRetryState(task, job)
	decision, err := evaluateVideoRetry(ctx, fact, &state)
	if err != nil {
		return false, err
	}
	if !decision.Matched || !mediaretry.IsResubmission(decision.Action) && decision.BlockedReason != "resubmissions_paused" {
		if err := model.RecordAsyncVideoRetryDecision(ctx, task, job, fact, decision, state, false); err != nil {
			return false, err
		}
		if decision.Action == mediaretry.ManualReview || decision.Action == mediaretry.PollSameJob {
			return true, model.HoldAsyncVideoRecovery(ctx, job, task, "Video recovery requires manual review: "+decision.BlockedReason, true)
		}
		return false, nil
	}
	request.RetryFact, request.PendingDecision, request.RetryFailedAt = &fact, &decision, time.Now().Unix()
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
	local := decision.RequestedAction == mediaretry.LocalSameChannel || decision.RequestedAction == mediaretry.LocalOtherChannel
	return model.ScheduleAsyncVideoReferenceRecovery(ctx, task, job, fact.Message, local, model.AsyncVideoRecoveryPlan{Decision: decision, Fact: fact, State: state, RejectedSubmission: true, RequestCipher: cipher})
}
