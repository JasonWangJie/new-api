package controller

import (
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func GetMediaRetryPolicy(c *gin.Context) {
	policy, err := model.GetMediaRetryPolicy(c.Request.Context())
	if err != nil {
		imageManagementError(c, 503, errors.New("Recovery rules are unavailable"))
		return
	}
	imageManagementData(c, policy)
}

func UpdateMediaRetryPolicy(c *gin.Context) {
	input := struct {
		mediaretry.Policy
		Revision *int64 `json:"revision"`
	}{}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &input); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	if input.Revision == nil {
		imageManagementError(c, 428, errors.New("Configuration revision is required. Reload it before saving"))
		return
	}
	input.Policy.Revision = *input.Revision
	if _, err := mediaretry.Compile(input.Policy); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	policy, err := model.SaveMediaRetryPolicy(c.Request.Context(), input.Policy, *input.Revision, c.GetInt("id"), nil)
	if err != nil {
		if errors.Is(err, model.ErrOptionRevisionConflict) {
			imageManagementError(c, 409, err)
		} else {
			imageManagementError(c, 503, errors.New("Recovery rules could not be saved"))
		}
		return
	}
	recordManageAudit(c, "media_retry_policy.update", map[string]any{"revision": policy.Revision, "rule_count": len(policy.Rules)})
	imageManagementData(c, policy)
}

func PreviewMediaRetryPolicy(c *gin.Context) {
	input := struct {
		Policy  *mediaretry.Policy `json:"policy"`
		Fact    mediaretry.Fact    `json:"fact"`
		State   *mediaretry.State  `json:"state"`
		TaskID  string             `json:"task_id"`
		EventID int64              `json:"event_id"`
	}{}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &input); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	policy := input.Policy
	if policy == nil {
		current, err := model.GetMediaRetryPolicy(c.Request.Context())
		if err != nil {
			imageManagementError(c, 503, errors.New("Recovery rules are unavailable"))
			return
		}
		policy = &current
	}
	mode := "text_only"
	var recordedDecision *mediaretry.Decision
	selectedTaskID := input.TaskID
	state := mediaretry.State{TotalLimit: -1, Now: time.Now().Unix()}
	if input.State != nil {
		state = *input.State
	}
	if input.EventID > 0 || input.TaskID != "" {
		var event model.MediaRetryEvent
		var err error
		if input.EventID > 0 {
			event, err = model.GetMediaRetryEvent(c.Request.Context(), input.EventID)
		} else {
			event, err = model.GetLatestMediaRetryEvent(c.Request.Context(), input.TaskID)
		}
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				imageManagementError(c, 404, errors.New("No recorded recovery decision is available for this task"))
			} else {
				imageManagementError(c, 503, errors.New("Recovery sample is unavailable"))
			}
			return
		}
		if err := common.UnmarshalJsonStr(event.FactJSON, &input.Fact); err != nil {
			imageManagementError(c, 503, errors.New("Recovery sample is invalid"))
			return
		}
		mode = "sample"
		selectedTaskID = event.TaskID
		var recorded mediaretry.Decision
		if common.UnmarshalJsonStr(event.DecisionJSON, &recorded) == nil {
			recordedDecision = &recorded
		}
		if event.StateJSON != "" {
			if err := common.UnmarshalJsonStr(event.StateJSON, &state); err != nil {
				imageManagementError(c, 503, errors.New("Recovery sample state is invalid"))
				return
			}
		}
	}
	input.Fact = service.NormalizeMediaRetryFact(input.Fact)
	recordedState := state
	stateSource := "text"
	contextAvailable := true
	if mode == "sample" {
		stateSource = "recorded_snapshot"
		current, currentErr := service.CurrentMediaRetryPreviewState(c.Request.Context(), selectedTaskID, input.Fact.MediaType, state)
		if currentErr == nil {
			state = current
			stateSource = "current_task"
		} else {
			contextAvailable = false
		}
	}
	state.Now = time.Now().Unix()
	decision, err := mediaretry.Evaluate(*policy, input.Fact, state)
	if err != nil {
		imageManagementError(c, 400, err)
		return
	}
	decisionSource := "custom_matcher"
	if mode == "sample" && contextAvailable && input.Fact.MediaType == "image" && input.Fact.ImageEvidence != nil {
		decision, err = service.PreviewImageFailureDecision(c.Request.Context(), selectedTaskID, input.Fact, state, *policy)
		if err != nil {
			imageManagementError(c, 503, errors.New("Recovery planning context is unavailable"))
			return
		}
		decisionSource = "current_custom"
		if !decision.Matched {
			decisionSource = "current_builtin"
		}
	}
	if mode == "sample" && contextAvailable && input.Fact.MediaType == "video" && input.Fact.VideoEvidence != nil {
		switch input.Fact.Stage {
		case "submit":
			decision, err = service.PreviewInitialVideoFailure(c.Request.Context(), selectedTaskID, input.Fact, state, *policy)
		case "terminal":
			decision, err = service.PlanTerminalVideoFailure(*policy, input.Fact, state)
		case "poll":
			if input.Fact.VideoEvidence.TerminalResponseConflict {
				decision, err = service.PlanTerminalVideoFailure(*policy, input.Fact, state)
			}
		}
		if err != nil {
			imageManagementError(c, 503, errors.New("Recovery planning context is unavailable"))
			return
		}
		decisionSource = "current_custom"
		if !decision.Matched || strings.HasPrefix(decision.RuleID, "builtin-") {
			decisionSource = "current_builtin"
		}
	}
	eligibility := service.MediaRetryEligibility{CandidateChannelIDs: []int{}, Reason: "text_matching_only"}
	if mode == "sample" && contextAvailable {
		switch input.Fact.MediaType {
		case "image":
			eligibility, err = service.PreviewImageRetryEligibility(c.Request.Context(), selectedTaskID, input.Fact, decision)
		case "video":
			eligibility, err = service.PreviewVideoRetryEligibility(c.Request.Context(), selectedTaskID, input.Fact, decision)
		}
		if err != nil {
			eligibility = service.MediaRetryEligibility{Checked: true, CandidateChannelIDs: []int{}, Reason: "task_context_unavailable"}
		}
	}
	if mode == "sample" && !contextAvailable {
		eligibility = service.MediaRetryEligibility{Checked: true, CandidateChannelIDs: []int{}, Reason: "task_context_unavailable"}
	}
	source := "unavailable"
	if recordedDecision != nil && recordedDecision.Action != "" {
		source = "recorded_custom"
		if !recordedDecision.Matched || strings.HasPrefix(recordedDecision.RuleID, "builtin-") {
			source = "recorded_builtin"
		}
	}
	imageManagementData(c, gin.H{"decision": decision, "decision_source": decisionSource, "mode": mode, "fact": input.Fact, "state": state, "recorded_state": recordedState, "state_source": stateSource, "eligibility": eligibility, "recorded_decision": recordedDecision, "recorded_decision_source": source})
}

func ListMediaRetryPolicyVersions(c *gin.Context) {
	page, pageSize := mediaRetryPagination(c)
	items, total, err := model.ListMediaRetryPolicyVersions(c.Request.Context(), page, pageSize)
	if err != nil {
		imageManagementError(c, 503, errors.New("Recovery policy history is unavailable"))
		return
	}
	imageManagementData(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func GetMediaRetryPolicyVersion(c *gin.Context) {
	revision, err := strconv.ParseInt(c.Param("revision"), 10, 64)
	if err != nil || revision < 0 {
		imageManagementError(c, 400, model.ErrOptionRevisionInvalid)
		return
	}
	version, err := model.GetMediaRetryPolicyVersion(c.Request.Context(), revision)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			imageManagementError(c, 404, errors.New("Recovery policy version was not found"))
		} else {
			imageManagementError(c, 503, errors.New("Recovery policy history is unavailable"))
		}
		return
	}
	var policy mediaretry.Policy
	if err := common.UnmarshalJsonStr(version.Config, &policy); err != nil {
		imageManagementError(c, 503, errors.New("Recovery policy version is invalid"))
		return
	}
	imageManagementData(c, gin.H{"version": version, "policy": policy})
}

func RollbackMediaRetryPolicy(c *gin.Context) {
	var input struct {
		Revision       *int64 `json:"revision"`
		TargetRevision *int64 `json:"target_revision"`
	}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 4096), &input); err != nil {
		imageManagementError(c, 400, err)
		return
	}
	if input.Revision == nil || input.TargetRevision == nil {
		imageManagementError(c, 428, errors.New("Current and target revisions are required"))
		return
	}
	if *input.Revision < 0 || *input.TargetRevision < 0 || *input.Revision >= model.MaxOptionRevision {
		imageManagementError(c, 400, model.ErrOptionRevisionInvalid)
		return
	}
	policy, err := model.SaveMediaRetryPolicy(c.Request.Context(), mediaretry.Policy{}, *input.Revision, c.GetInt("id"), input.TargetRevision)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrOptionRevisionConflict):
			imageManagementError(c, 409, err)
		case errors.Is(err, gorm.ErrRecordNotFound):
			imageManagementError(c, 404, errors.New("Recovery policy version was not found"))
		default:
			imageManagementError(c, 503, errors.New("Recovery policy rollback failed"))
		}
		return
	}
	recordManageAudit(c, "media_retry_policy.rollback", map[string]any{"revision": policy.Revision, "source_revision": *input.TargetRevision})
	imageManagementData(c, policy)
}

func mediaRetryPagination(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	return min(max(page, 1), 100000), min(max(pageSize, 1), 100)
}

func ListMediaRetrySamples(c *gin.Context) {
	page, pageSize := mediaRetryPagination(c)
	filter, err := mediaRetrySampleFilters(c)
	if err != nil {
		imageManagementError(c, 400, err)
		return
	}
	items, total, err := model.ListMediaRetrySamples(c.Request.Context(), filter, page, pageSize)
	if err != nil {
		imageManagementError(c, 503, errors.New("Recovery samples are unavailable"))
		return
	}
	imageManagementData(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func mediaRetrySampleFilters(c *gin.Context) (model.MediaRetrySampleFilter, error) {
	filter := model.MediaRetrySampleFilter{MediaType: c.Query("media_type"), Provider: c.Query("provider"), Model: c.Query("model"), Stage: c.Query("stage"), RuleID: c.Query("rule_id"), Category: c.Query("category")}
	filter.ChannelID, _ = strconv.Atoi(c.Query("channel_id"))
	filter.Start, _ = strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	filter.End, _ = strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	if filter.Start == 0 {
		filter.Start = time.Now().AddDate(0, 0, -30).Unix()
	}
	if matched := c.Query("matched"); matched != "" {
		value, err := strconv.ParseBool(matched)
		if err != nil {
			return filter, errors.New("Invalid match filter")
		}
		filter.Matched = &value
	}
	return filter, nil
}

func ListMediaRetrySampleEvents(c *gin.Context) {
	fingerprint := c.Param("event_id")
	decoded, err := hex.DecodeString(fingerprint)
	if err != nil || len(decoded) != 32 {
		imageManagementError(c, 400, errors.New("Invalid recovery sample"))
		return
	}
	filter, err := mediaRetrySampleFilters(c)
	if err != nil {
		imageManagementError(c, 400, err)
		return
	}
	page, pageSize := mediaRetryPagination(c)
	items, total, err := model.ListMediaRetrySampleEvents(c.Request.Context(), fingerprint, filter, page, pageSize)
	if err != nil {
		imageManagementError(c, 503, errors.New("Recovery samples are unavailable"))
		return
	}
	imageManagementData(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func GetMediaRetrySample(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("event_id"), 10, 64)
	if err != nil || id <= 0 {
		imageManagementError(c, 400, errors.New("Invalid recovery sample"))
		return
	}
	event, err := model.GetMediaRetryEvent(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			imageManagementError(c, 404, errors.New("Recovery sample was not found"))
		} else {
			imageManagementError(c, 503, errors.New("Recovery sample is unavailable"))
		}
		return
	}
	var fact mediaretry.Fact
	var decision mediaretry.Decision
	if common.UnmarshalJsonStr(event.FactJSON, &fact) != nil || common.UnmarshalJsonStr(event.DecisionJSON, &decision) != nil {
		imageManagementError(c, 503, errors.New("Recovery sample is invalid"))
		return
	}
	imageManagementData(c, gin.H{"fact": fact, "decision": decision, "task_id": event.TaskID, "created_at": event.CreatedAt})
}
