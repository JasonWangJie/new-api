package mediaretry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyMatchingAndActionBudgets(t *testing.T) {
	policy := Policy{Revision: 4, Rules: []Rule{{ID: "reference", Name: "Reference failed", Enabled: true, Category: "reference_fetch", MaxAttempts: 3,
		Scope: Scope{MediaTypes: []string{"video"}, Providers: []string{"drama"}, Categories: []string{"upstream_error"}},
		Match: Match{HTTPStatuses: []int{400}, ContainsAny: []string{"FETCH", "download"}, ContainsAll: []string{"reference"}, Excludes: []string{"invalid content"}},
		Steps: []Step{{ID: "local", Action: LocalSameChannel, MaxAttempts: 2, DelaySeconds: 3, Backoff: "exponential", MaxDelaySeconds: 5}, {ID: "switch", Action: LocalOtherChannel, MaxAttempts: 2}}, OnExhausted: Stop}}}
	fact := Fact{MediaType: "video", Provider: "drama", Category: "upstream_error", HTTPStatus: 400, Message: "Reference fetch failed", Terminal: true, Accepted: true, LocalSupported: true, HasReferences: true}
	state := State{TotalLimit: 10, Now: 100}
	compiled, err := Compile(policy)
	require.NoError(t, err)
	first := compiled.Evaluate(fact, state)
	assert.True(t, first.Matched)
	assert.Equal(t, LocalSameChannel, first.Action)
	assert.Equal(t, 3, first.DelaySeconds)
	assert.Equal(t, 3, first.RuleLimit)
	assert.Equal(t, 2, first.StepLimit)
	assert.Zero(t, first.StepUsed)
	state = Consume(state, first)
	second := compiled.Evaluate(fact, state)
	assert.Equal(t, 5, second.DelaySeconds)
	assert.Equal(t, 1, second.StepUsed)
	assert.Equal(t, 1, second.TotalUsed)
	state = Consume(state, second)
	third := compiled.Evaluate(fact, state)
	assert.Equal(t, LocalOtherChannel, third.Action)
	state = Consume(state, third)
	last := compiled.Evaluate(fact, state)
	assert.True(t, last.Exhausted)
	assert.Equal(t, Stop, last.Action)
	fact.Message += ": invalid content"
	assert.False(t, compiled.Evaluate(fact, state).Matched)
	fact.Message = "Reference fetch failed"
	fact.Provider = "different-provider"
	assert.False(t, compiled.Evaluate(fact, state).Matched)
}

func TestPolicyExecutionGuards(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fact       Fact
		paused     bool
		limit      int
		wantAction string
		wantReason string
	}{
		{"confirmed failure can retry", Fact{Accepted: true, Terminal: true}, false, 3, SameChannel, ""},
		{"custom retry replaces unknown receipt", Fact{Unknown: true, HasUpstreamTask: true, LegacyTimeoutAuthorized: true}, false, 3, SameChannel, ""},
		{"expired unknown receipt needs review", Fact{Unknown: true, HasUpstreamTask: true, Deadline: 100}, false, 3, ManualReview, "execution_deadline_reached"},
		{"custom retry replaces unknown without receipt", Fact{Unknown: true}, false, 3, SameChannel, ""},
		{"custom retry replaces accepted pending", Fact{Accepted: true, HasUpstreamTask: true}, false, 3, SameChannel, ""},
		{"success output resumes storage", Fact{HasOutput: true}, false, 3, ResumeDownload, "output_already_available"},
		{"billing resumes existing output", Fact{HasOutput: true, Stage: "billing"}, false, 3, ResumeBilling, "output_already_available"},
		{"paused policy prevents generation", Fact{}, true, 3, ManualReview, "resubmissions_paused"},
		{"zero budget prevents generation", Fact{}, false, 0, ManualReview, "total_retry_budget_exhausted"},
		{"expired generation stops", Fact{Deadline: 100}, false, 3, ManualReview, "execution_deadline_reached"},
		{"pausing cannot extend deadline", Fact{Deadline: 100}, true, 3, ManualReview, "execution_deadline_reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := Policy{PauseResubmissions: tc.paused, LegacyTimeoutEnabled: true, Rules: []Rule{{ID: "rule", Name: "Retry", Enabled: true, Match: Match{ContainsAny: []string{"busy"}}, Steps: []Step{{ID: "same", Action: SameChannel, MaxAttempts: 1}}}}}
			fact := tc.fact
			fact.Message = "busy"
			decision, err := Evaluate(policy, fact, State{Now: 100, TotalLimit: tc.limit})
			require.NoError(t, err)
			assert.Equal(t, tc.wantAction, decision.Action)
			assert.Equal(t, tc.wantReason, decision.BlockedReason)
		})
	}
}

func TestPolicyCustomActionsPreserveUnknownFacts(t *testing.T) {
	for _, action := range []string{SameChannel, OtherChannel, LocalSameChannel, LocalOtherChannel, Stop, ManualReview, PollSameJob} {
		t.Run(action, func(t *testing.T) {
			policy := Policy{Rules: []Rule{{ID: "custom", Name: "Custom recovery", Enabled: true, Match: Match{HTTPStatuses: []int{400}, ContainsAny: []string{"请提供请求 ID 联系支持"}}, Steps: []Step{{ID: "step", Action: action, MaxAttempts: 1}}, OnExhausted: Stop}}}
			fact := Fact{HTTPStatus: 400, Message: "请求处理失败，请提供请求 ID 联系支持", Accepted: true, Unknown: true, HasUpstreamTask: true, HasReferences: true, LocalSupported: true}
			state := State{TotalLimit: 16}
			decision, err := Evaluate(policy, fact, state)
			require.NoError(t, err)
			assert.True(t, decision.Matched)
			assert.Equal(t, action, decision.Action)
			assert.Empty(t, decision.BlockedReason)
			assert.True(t, fact.Unknown)
			assert.False(t, fact.Terminal, "a custom action must not fabricate upstream confirmation")
			state = Consume(state, decision)
			decision, err = Evaluate(policy, fact, state)
			require.NoError(t, err)
			assert.True(t, decision.Exhausted)
			assert.Equal(t, Stop, decision.Action)
			fact.Message = "different error"
			decision, err = Evaluate(policy, fact, state)
			require.NoError(t, err)
			assert.False(t, decision.Matched)
			assert.Empty(t, decision.Action, "unmatched failures still use built-in recovery")
		})
	}
}

func TestPolicyRevisionDoesNotResetStepCounts(t *testing.T) {
	state := State{RuleUses: map[string]int{"rule": 1}, StepUses: map[string]int{"rule/first": 1}, TotalUsed: 1, TotalLimit: 4}
	policy := Policy{Revision: 7, Rules: []Rule{{ID: "rule", Name: "Retry", Enabled: true, MaxAttempts: 1, Match: Match{ErrorCodes: []string{"busy"}}, Steps: []Step{{ID: "replacement", Action: SameChannel, MaxAttempts: 4}}, OnExhausted: ManualReview}}}
	decision, err := Evaluate(policy, Fact{ErrorCode: "busy"}, state)
	require.NoError(t, err)
	assert.True(t, decision.Exhausted)
	assert.Equal(t, ManualReview, decision.Action)
	copy := Consume(state, Decision{Matched: true, RuleID: "other", StepID: "poll", Action: PollSameJob})
	assert.Equal(t, 1, copy.TotalUsed)
	assert.Equal(t, 1, copy.StepUses["other/poll"])
	assert.NotContains(t, state.StepUses, "other/poll")
	guarded := Consume(state, Decision{Matched: true, RuleID: "rule", StepID: "first", Action: PollSameJob, RequestedAction: SameChannel, BlockedReason: "upstream_result_unknown"})
	assert.Equal(t, 2, guarded.StepUses["rule/first"], "a protected query that actually started still consumes its step")
	assert.Equal(t, 1, guarded.TotalUsed, "querying an existing operation does not consume generation budget")
}

func TestPolicyPostprocessingCannotSkipStorageOrIgnoreStop(t *testing.T) {
	for _, test := range []struct{ action, stage, reason string }{{Stop, "upload", "output_already_available"}, {ResumeBilling, "upload", "output_storage_incomplete"}} {
		policy := Policy{Rules: []Rule{{ID: "post", Name: "Postprocess", Enabled: true, Match: Match{ContainsAny: []string{"busy"}}, Steps: []Step{{ID: "post", Action: test.action, MaxAttempts: 1}}}}}
		decision, err := Evaluate(policy, Fact{Stage: test.stage, HasOutput: true, Message: "busy"}, State{TotalLimit: 1})
		require.NoError(t, err)
		assert.Equal(t, ManualReview, decision.Action)
		assert.Equal(t, test.reason, decision.BlockedReason)
	}
}

func TestPolicyRejectsInvalidConditionsAndOrdersRules(t *testing.T) {
	base := Rule{ID: "rule", Name: "Retry", Enabled: true, Match: Match{Regex: `(?i)busy`}, Steps: []Step{{ID: "same", Action: SameChannel, MaxAttempts: 1}}}
	for _, tc := range []struct {
		name   string
		change func(*Rule)
	}{
		{"invalid regex", func(r *Rule) { r.Match.Regex = "[" }},
		{"empty positive condition", func(r *Rule) { r.Match = Match{Excludes: []string{"no"}} }},
		{"unbounded step", func(r *Rule) { r.Steps = []Step{{ID: "same", Action: SameChannel, MaxAttempts: 101}} }},
		{"invalid action", func(r *Rule) { r.Steps = []Step{{ID: "same", Action: "exec", MaxAttempts: 1}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule := base
			tc.change(&rule)
			_, err := Compile(Policy{Rules: []Rule{rule}})
			require.Error(t, err)
		})
	}
	second := base
	second.ID, second.Category = "second", "later"
	first := base
	first.Category = "first"
	decision, err := Evaluate(Policy{Rules: []Rule{first, second}}, Fact{Message: "BUSY"}, State{TotalLimit: 3})
	require.NoError(t, err)
	assert.Equal(t, "first", decision.Category)
	first.Enabled = false
	decision, err = Evaluate(Policy{Rules: []Rule{first, second}}, Fact{Message: "BUSY"}, State{TotalLimit: 3})
	require.NoError(t, err)
	assert.Equal(t, "later", decision.Category)
}
