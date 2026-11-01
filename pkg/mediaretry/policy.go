// Package mediaretry evaluates administrator-authored recovery policies without
// performing network requests, changing task state, or moving funds.
package mediaretry

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

const (
	SameChannel       = "same_channel"
	OtherChannel      = "other_channel"
	LocalSameChannel  = "local_same_channel"
	LocalOtherChannel = "local_other_channel"
	Stop              = "stop"
	ManualReview      = "manual_review"
	PollSameJob       = "poll_same_job"
	ResumeDownload    = "resume_download"
	ResumeBilling     = "resume_billing"
)

type Policy struct {
	Revision               int64          `json:"revision"`
	PauseResubmissions     bool           `json:"pause_resubmissions"`
	LegacyTimeoutEnabled   bool           `json:"legacy_timeout_enabled"`
	LegacyTimeoutScopeKeys []string       `json:"legacy_timeout_scope_keys,omitempty"`
	LegacyTimeoutConfig    map[string]int `json:"legacy_timeout_config,omitempty"`
	LegacyVideoChannelIDs  []int          `json:"legacy_video_channel_ids,omitempty"`
	Rules                  []Rule         `json:"rules"`
}

type Rule struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Enabled            bool   `json:"enabled"`
	Category           string `json:"category"`
	Scope              Scope  `json:"scope"`
	Match              Match  `json:"match"`
	Steps              []Step `json:"steps"`
	OnExhausted        string `json:"on_exhausted"`
	MaxAttempts        int    `json:"max_attempts"`
	AllowedChannelIDs  []int  `json:"allowed_channel_ids,omitempty"`
	ExcludedChannelIDs []int  `json:"excluded_channel_ids,omitempty"`
}

type Scope struct {
	MediaTypes []string `json:"media_types,omitempty"`
	Providers  []string `json:"providers,omitempty"`
	Models     []string `json:"models,omitempty"`
	ChannelIDs []int    `json:"channel_ids,omitempty"`
	Stages     []string `json:"stages,omitempty"`
	Categories []string `json:"categories,omitempty"`
}

type Match struct {
	HTTPStatuses []int    `json:"http_statuses,omitempty"`
	ErrorCodes   []string `json:"error_codes,omitempty"`
	ContainsAny  []string `json:"contains_any,omitempty"`
	ContainsAll  []string `json:"contains_all,omitempty"`
	Excludes     []string `json:"excludes,omitempty"`
	Regex        string   `json:"regex,omitempty"`
}

type Step struct {
	ID              string `json:"id"`
	Action          string `json:"action"`
	MaxAttempts     int    `json:"max_attempts"`
	DelaySeconds    int    `json:"delay_seconds"`
	Backoff         string `json:"backoff"`
	MaxDelaySeconds int    `json:"max_delay_seconds"`
}

// Facts about acceptance and completion come from the host, never from a text
// matcher. Message is a bounded, sanitized error message, not a request body.
type Fact struct {
	ImageEvidence           *ImageFailureEvidence `json:"image_evidence,omitempty"`
	VideoEvidence           *VideoFailureEvidence `json:"video_evidence,omitempty"`
	MediaType               string                `json:"media_type"`
	Provider                string                `json:"provider"`
	Model                   string                `json:"model"`
	ChannelID               int                   `json:"channel_id"`
	Stage                   string                `json:"stage"`
	HTTPStatus              int                   `json:"http_status"`
	ErrorCode               string                `json:"error_code"`
	Category                string                `json:"category"`
	Message                 string                `json:"message"`
	Unknown                 bool                  `json:"unknown"`
	Accepted                bool                  `json:"accepted"`
	Terminal                bool                  `json:"terminal"`
	HasOutput               bool                  `json:"has_output"`
	HasUpstreamTask         bool                  `json:"has_upstream_task"`
	HasReferences           bool                  `json:"has_references"`
	LocalSupported          bool                  `json:"local_supported"`
	LegacyTimeoutAuthorized bool                  `json:"legacy_timeout_authorized"`
	Deadline                int64                 `json:"deadline"`
}

// VideoFailureEvidence is produced only by host protocol validation. Error
// keywords cannot grant either an ambiguous submission replay or a legacy path.
type VideoFailureEvidence struct {
	LegacySubmissionRejected bool  `json:"legacy_submission_rejected,omitempty"`
	SubmissionStartedAt      int64 `json:"submission_started_at,omitempty"`
	LegacySubmissionUsed     bool  `json:"legacy_submission_used,omitempty"`
	LegacyReferenceEligible  bool  `json:"legacy_reference_eligible,omitempty"`
	LegacyBusyEligible       bool  `json:"legacy_busy_eligible,omitempty"`
	TerminalResponseConflict bool  `json:"terminal_response_conflict,omitempty"`
}

// ImageFailureEvidence retains the host's typed protocol outcome for a
// side-effect-free replay of built-in decisions. Matchers never inspect it.
type ImageFailureEvidence struct {
	FailureAttempt               int64  `json:"failure_attempt"`
	Postprocess                  bool   `json:"postprocess,omitempty"`
	QuotaUncertain               bool   `json:"quota_uncertain,omitempty"`
	RetryAfterSeconds            int64  `json:"retry_after_seconds,omitempty"`
	Code                         int    `json:"code"`
	InternalCode                 string `json:"internal_code"`
	UpstreamTaskID               string `json:"upstream_task_id,omitempty"`
	DispatchedAt                 int64  `json:"dispatched_at"`
	UpstreamTerminal             bool   `json:"upstream_terminal"`
	RetrySameChannel             bool   `json:"retry_same_channel"`
	RetryableHTTPResponse        bool   `json:"retryable_http_response"`
	SynchronousRetryableResponse bool   `json:"synchronous_retryable_response"`
	SubmissionRejected           bool   `json:"submission_rejected"`
	ReferenceRejectionVerified   bool   `json:"reference_rejection_verified"`
	ReferenceTransportFallback   bool   `json:"reference_transport_fallback"`
}

// Uses are keyed by stable rule IDs and ruleID/stepID, not policy revisions.
// The host increments them transactionally only when dispatch is committed.
type State struct {
	RuleUses   map[string]int `json:"rule_uses,omitempty"`
	StepUses   map[string]int `json:"step_uses,omitempty"`
	TotalUsed  int            `json:"total_used"`
	TotalLimit int            `json:"total_limit"`
	Now        int64          `json:"now"`
}

type Decision struct {
	Matched            bool   `json:"matched"`
	RuleID             string `json:"rule_id,omitempty"`
	StepID             string `json:"step_id,omitempty"`
	Revision           int64  `json:"revision"`
	Category           string `json:"category,omitempty"`
	Action             string `json:"action,omitempty"`
	RequestedAction    string `json:"requested_action,omitempty"`
	DelaySeconds       int    `json:"delay_seconds"`
	BlockedReason      string `json:"blocked_reason,omitempty"`
	Exhausted          bool   `json:"exhausted"`
	AllowedChannelIDs  []int  `json:"allowed_channel_ids,omitempty"`
	ExcludedChannelIDs []int  `json:"excluded_channel_ids,omitempty"`
	RuleUsed           int    `json:"rule_used"`
	RuleLimit          int    `json:"rule_limit"`
	StepUsed           int    `json:"step_used"`
	StepLimit          int    `json:"step_limit"`
	TotalUsed          int    `json:"total_used"`
	TotalLimit         int    `json:"total_limit"`
}

type CompiledPolicy struct {
	policy   Policy
	patterns []*regexp.Regexp
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func Compile(policy Policy) (*CompiledPolicy, error) {
	if policy.Revision < 0 || policy.Revision > 1<<53-1 || len(policy.Rules) > 200 {
		return nil, fmt.Errorf("invalid recovery policy revision or rule count")
	}
	compiled := &CompiledPolicy{policy: policy, patterns: make([]*regexp.Regexp, len(policy.Rules))}
	ids := make(map[string]bool, len(policy.Rules))
	for i, rule := range policy.Rules {
		if !identifier.MatchString(rule.ID) || ids[rule.ID] || strings.TrimSpace(rule.Name) == "" || len(rule.Name) > 256 || len(rule.Category) > 64 {
			return nil, fmt.Errorf("rule %d has an invalid or duplicate identity", i+1)
		}
		ids[rule.ID] = true
		if len(rule.Steps) == 0 || len(rule.Steps) > 8 || rule.OnExhausted != "" && rule.OnExhausted != Stop && rule.OnExhausted != ManualReview {
			return nil, fmt.Errorf("rule %s has invalid recovery steps", rule.ID)
		}
		if len(rule.Scope.Categories)+len(rule.Match.HTTPStatuses)+len(rule.Match.ErrorCodes)+len(rule.Match.ContainsAny)+len(rule.Match.ContainsAll)+len(rule.Match.Regex) == 0 {
			return nil, fmt.Errorf("rule %s requires a positive error matching condition", rule.ID)
		}
		if rule.MaxAttempts < 0 || rule.MaxAttempts > 100 {
			return nil, fmt.Errorf("rule %s has an invalid total attempt limit", rule.ID)
		}
		for _, values := range [][]string{rule.Scope.MediaTypes, rule.Scope.Providers, rule.Scope.Models, rule.Scope.Stages, rule.Scope.Categories, rule.Match.ErrorCodes, rule.Match.ContainsAny, rule.Match.ContainsAll, rule.Match.Excludes} {
			if len(values) > 100 {
				return nil, fmt.Errorf("rule %s has too many matching values", rule.ID)
			}
			for _, value := range values {
				if strings.TrimSpace(value) == "" || len(value) > 512 {
					return nil, fmt.Errorf("rule %s has an empty or oversized matching value", rule.ID)
				}
			}
		}
		for _, media := range rule.Scope.MediaTypes {
			if media != "image" && media != "video" {
				return nil, fmt.Errorf("rule %s has an invalid media type", rule.ID)
			}
		}
		for _, channels := range [][]int{rule.Scope.ChannelIDs, rule.AllowedChannelIDs, rule.ExcludedChannelIDs} {
			if len(channels) > 200 {
				return nil, fmt.Errorf("rule %s has too many channels", rule.ID)
			}
			for _, id := range channels {
				if id <= 0 {
					return nil, fmt.Errorf("rule %s has an invalid channel", rule.ID)
				}
			}
		}
		if len(rule.Match.HTTPStatuses) > 100 {
			return nil, fmt.Errorf("rule %s has too many HTTP statuses", rule.ID)
		}
		for _, status := range rule.Match.HTTPStatuses {
			if status < 100 || status > 599 {
				return nil, fmt.Errorf("rule %s has an invalid HTTP status", rule.ID)
			}
		}
		if len(rule.Match.Regex) > 1024 {
			return nil, fmt.Errorf("rule %s regular expression is too long", rule.ID)
		}
		if rule.Match.Regex != "" {
			pattern, err := regexp.Compile(rule.Match.Regex)
			if err != nil {
				return nil, fmt.Errorf("rule %s has an invalid regular expression: %w", rule.ID, err)
			}
			compiled.patterns[i] = pattern
		}
		steps := make(map[string]bool, len(rule.Steps))
		for _, step := range rule.Steps {
			if !identifier.MatchString(step.ID) || steps[step.ID] || !slices.Contains([]string{SameChannel, OtherChannel, LocalSameChannel, LocalOtherChannel, Stop, ManualReview, PollSameJob, ResumeDownload, ResumeBilling}, step.Action) {
				return nil, fmt.Errorf("rule %s has an invalid or duplicate step", rule.ID)
			}
			steps[step.ID] = true
			if step.MaxAttempts < 1 || step.MaxAttempts > 100 || step.DelaySeconds < 0 || step.DelaySeconds > 86400 || step.MaxDelaySeconds < 0 || step.MaxDelaySeconds > 86400 || step.MaxDelaySeconds != 0 && step.MaxDelaySeconds < step.DelaySeconds || step.Backoff != "" && step.Backoff != "fixed" && step.Backoff != "exponential" {
				return nil, fmt.Errorf("rule %s step %s has invalid retry limits", rule.ID, step.ID)
			}
		}
	}
	return compiled, nil
}

func IsResubmission(action string) bool {
	return action == SameChannel || action == OtherChannel || action == LocalSameChannel || action == LocalOtherChannel
}

func StepKey(ruleID, stepID string) string { return ruleID + "/" + stepID }

// Consume records an action that actually began. Scheduling alone must never
// consume a step. Polling/postprocessing use their step budget, not generation's.
func Consume(state State, decision Decision) State {
	if !decision.Matched || decision.RuleID == "" || decision.StepID == "" || decision.BlockedReason != "" && decision.Action != PollSameJob && decision.Action != ResumeDownload && decision.Action != ResumeBilling {
		return state
	}
	rules := make(map[string]int, len(state.RuleUses)+1)
	maps.Copy(rules, state.RuleUses)
	steps := make(map[string]int, len(state.StepUses)+1)
	maps.Copy(steps, state.StepUses)
	rules[decision.RuleID]++
	steps[StepKey(decision.RuleID, decision.StepID)]++
	state.RuleUses, state.StepUses = rules, steps
	if IsResubmission(decision.Action) {
		state.TotalUsed++
	}
	return state
}

func Evaluate(policy Policy, fact Fact, state State) (Decision, error) {
	compiled, err := Compile(policy)
	if err != nil {
		return Decision{}, err
	}
	return compiled.Evaluate(fact, state), nil
}

func (compiled *CompiledPolicy) Evaluate(fact Fact, state State) Decision {
	decision := Decision{Revision: compiled.policy.Revision, TotalUsed: state.TotalUsed, TotalLimit: state.TotalLimit}
	for i, rule := range compiled.policy.Rules {
		if !rule.Enabled || !matches(rule, compiled.patterns[i], fact) {
			continue
		}
		decision.Matched, decision.RuleID, decision.Category = true, rule.ID, rule.Category
		decision.AllowedChannelIDs = slices.Clone(rule.AllowedChannelIDs)
		decision.ExcludedChannelIDs = slices.Clone(rule.ExcludedChannelIDs)
		ruleLimit := rule.MaxAttempts
		if ruleLimit == 0 {
			for _, step := range rule.Steps {
				ruleLimit += step.MaxAttempts
			}
		}
		decision.RuleUsed, decision.RuleLimit = state.RuleUses[rule.ID], ruleLimit
		for _, step := range rule.Steps {
			if state.RuleUses[rule.ID] >= ruleLimit {
				break
			}
			used := max(0, state.StepUses[StepKey(rule.ID, step.ID)])
			if used >= step.MaxAttempts {
				continue
			}
			decision.StepID, decision.Action, decision.RequestedAction = step.ID, step.Action, step.Action
			decision.StepUsed, decision.StepLimit = used, step.MaxAttempts
			delayLimit := step.MaxDelaySeconds
			if delayLimit == 0 {
				delayLimit = 86400
			}
			decision.DelaySeconds = step.DelaySeconds
			if step.Backoff == "exponential" {
				for range min(used, 31) {
					decision.DelaySeconds = min(delayLimit, decision.DelaySeconds*2)
				}
			}
			return compiled.guard(decision, fact, state)
		}
		decision.Exhausted = true
		decision.Action = rule.OnExhausted
		if decision.Action == "" {
			decision.Action = ManualReview
		}
		decision.RequestedAction = decision.Action
		return compiled.guard(decision, fact, state)
	}
	return decision
}

func matches(rule Rule, pattern *regexp.Regexp, fact Fact) bool {
	for _, field := range []struct {
		values []string
		value  string
	}{{rule.Scope.MediaTypes, fact.MediaType}, {rule.Scope.Providers, fact.Provider}, {rule.Scope.Models, fact.Model}, {rule.Scope.Stages, fact.Stage}, {rule.Scope.Categories, fact.Category}, {rule.Match.ErrorCodes, fact.ErrorCode}} {
		if len(field.values) > 0 && !slices.Contains(field.values, field.value) {
			return false
		}
	}
	if len(rule.Scope.ChannelIDs) > 0 && !slices.Contains(rule.Scope.ChannelIDs, fact.ChannelID) || len(rule.Match.HTTPStatuses) > 0 && !slices.Contains(rule.Match.HTTPStatuses, fact.HTTPStatus) {
		return false
	}
	message := strings.ToLower(fact.Message)
	for _, word := range rule.Match.Excludes {
		if strings.Contains(message, strings.ToLower(word)) {
			return false
		}
	}
	for _, word := range rule.Match.ContainsAll {
		if !strings.Contains(message, strings.ToLower(word)) {
			return false
		}
	}
	if len(rule.Match.ContainsAny) > 0 {
		found := false
		for _, word := range rule.Match.ContainsAny {
			if strings.Contains(message, strings.ToLower(word)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return pattern == nil || pattern.MatchString(fact.Message)
}

func (compiled *CompiledPolicy) guard(decision Decision, fact Fact, state State) Decision {
	if fact.HasOutput && decision.Action == Stop {
		decision.Action, decision.BlockedReason = ManualReview, "output_already_available"
		return decision
	}
	if fact.HasOutput && IsResubmission(decision.Action) {
		decision.Action, decision.BlockedReason = ResumeDownload, "output_already_available"
		if fact.Stage == "billing" {
			decision.Action = ResumeBilling
		}
		return decision
	}
	if (fact.Unknown || fact.Accepted && !fact.Terminal) && (IsResubmission(decision.Action) || decision.Action == Stop) {
		decision.Action, decision.BlockedReason = ManualReview, "upstream_result_unknown"
		if fact.HasUpstreamTask {
			decision.Action = PollSameJob
		}
	}
	if IsResubmission(decision.Action) {
		switch {
		case fact.Deadline > 0 && (state.Now >= fact.Deadline || int64(decision.DelaySeconds) >= fact.Deadline-state.Now):
			decision.BlockedReason = "execution_deadline_reached"
		case state.TotalLimit >= 0 && state.TotalUsed >= state.TotalLimit:
			decision.BlockedReason = "total_retry_budget_exhausted"
		case compiled.policy.PauseResubmissions:
			decision.BlockedReason = "resubmissions_paused"
		case (decision.Action == LocalSameChannel || decision.Action == LocalOtherChannel) && (!fact.HasReferences || !fact.LocalSupported):
			decision.BlockedReason = "local_reference_transport_unavailable"
		}
		if decision.BlockedReason != "" {
			decision.Action = ManualReview
		}
	}
	if decision.Action == PollSameJob && !fact.HasUpstreamTask {
		decision.Action, decision.BlockedReason = ManualReview, "upstream_task_unavailable"
	}
	if decision.Action == PollSameJob && !fact.HasOutput && fact.Deadline > 0 && (state.Now >= fact.Deadline || int64(decision.DelaySeconds) >= fact.Deadline-state.Now) {
		decision.Action, decision.BlockedReason = ManualReview, "execution_deadline_reached"
	}
	if (decision.Action == ResumeDownload || decision.Action == ResumeBilling) && !fact.HasOutput {
		decision.Action, decision.BlockedReason = ManualReview, "upstream_output_unavailable"
	}
	if decision.Action == ResumeBilling && fact.Stage != "billing" {
		decision.Action, decision.BlockedReason = ManualReview, "output_storage_incomplete"
	}
	return decision
}
