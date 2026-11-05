package service

import "github.com/QuantumNous/new-api/pkg/mediaretry"

// PlanTerminalVideoFailure is shared by execution and read-only preview. The
// built-in fallback requires evidence captured by the native result observer;
// administrator-supplied keywords cannot create that evidence.
func PlanTerminalVideoFailure(policy mediaretry.Policy, fact mediaretry.Fact, state mediaretry.State) (mediaretry.Decision, error) {
	decision, err := mediaretry.Evaluate(policy, fact, state)
	decision = guardIdempotentVideoReplay(fact, decision)
	if err != nil {
		return decision, err
	}
	if !decision.Matched && fact.Unknown && fact.VideoEvidence != nil && fact.VideoEvidence.TerminalResponseConflict {
		if !decision.Matched && fact.HasUpstreamTask {
			decision.Action = mediaretry.PollSameJob
		} else if decision.Action != mediaretry.PollSameJob || !fact.HasUpstreamTask {
			decision.Action, decision.BlockedReason = mediaretry.ManualReview, "conflicting_upstream_result"
		}
		return decision, nil
	}
	if decision.Matched {
		return decision, nil
	}
	if fact.Provider != "drama" || fact.Category == "" || fact.VideoEvidence == nil || !fact.Terminal || fact.Unknown || fact.HasOutput || state.TotalUsed != 0 || state.TotalLimit <= 0 || fact.Deadline <= state.Now {
		return decision, nil
	}
	rule := mediaretry.Rule{Enabled: true, MaxAttempts: 1, OnExhausted: mediaretry.Stop, Scope: mediaretry.Scope{Categories: []string{fact.Category}}}
	if fact.VideoEvidence.LegacyReferenceEligible && fact.HasReferences && fact.LocalSupported {
		rule.ID, rule.Name, rule.Category = "builtin-drama-reference", "Built-in video reference recovery", "reference_download"
		rule.Steps = []mediaretry.Step{{ID: "local-once", Action: mediaretry.LocalSameChannel, MaxAttempts: 1}}
	} else if fact.VideoEvidence.LegacyBusyEligible {
		rule.ID, rule.Name, rule.Category = "builtin-drama-busy", "Built-in video capacity recovery", "capacity"
		rule.Steps = []mediaretry.Step{{ID: "same-once", Action: mediaretry.SameChannel, MaxAttempts: 1, DelaySeconds: 5}}
	} else {
		return decision, nil
	}
	policy.Rules = []mediaretry.Rule{rule}
	return mediaretry.Evaluate(policy, fact, state)
}
