package service

import (
	"context"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
)

var mediaRetryCompiler struct {
	sync.Mutex
	document string
	compiled *mediaretry.CompiledPolicy
}

// MediaRetryEligibility describes a read-only dispatch snapshot. The executor
// always repeats these checks immediately before sending the request.
type MediaRetryEligibility struct {
	Checked             bool     `json:"checked"`
	Eligible            bool     `json:"eligible"`
	Reason              string   `json:"reason,omitempty"`
	CandidateChannelIDs []int    `json:"candidate_channel_ids"`
	Warnings            []string `json:"warnings,omitempty"`
}

// GetMediaRetryPolicy reads the primary database at each decision boundary.
// Only immutable compilation is cached; periodic option synchronization is not
// an authority for dispatching billable replacement requests.
func GetMediaRetryPolicy(ctx context.Context) (mediaretry.Policy, error) {
	return model.GetMediaRetryPolicy(ctx)
}

// NormalizeMediaRetryFact is shared by previews, execution and event writers.
// Only extracted error diagnostics participate; request bodies never do.
func NormalizeMediaRetryFact(fact mediaretry.Fact) mediaretry.Fact {
	fact.Message = SanitizeAsyncImageDiagnostic(fact.Message, min(8192, asyncImageProviderMessageLimit))
	fact.ErrorCode = SanitizeAsyncImageDiagnostic(fact.ErrorCode, 128)
	if fact.ImageEvidence != nil {
		evidence := *fact.ImageEvidence
		evidence.InternalCode = SanitizeAsyncImageDiagnostic(evidence.InternalCode, 128)
		evidence.UpstreamTaskID = SanitizeAsyncImageDiagnostic(evidence.UpstreamTaskID, 256)
		fact.ImageEvidence = &evidence
	}
	return fact
}

func EvaluateMediaRetry(ctx context.Context, fact mediaretry.Fact, state mediaretry.State) (mediaretry.Decision, error) {
	policy, err := GetMediaRetryPolicy(ctx)
	if err != nil {
		return mediaretry.Decision{}, err
	}
	return evaluateMediaRetryPolicy(policy, fact, state)
}

func evaluateMediaRetryPolicy(policy mediaretry.Policy, fact mediaretry.Fact, state mediaretry.State) (mediaretry.Decision, error) {
	fact = NormalizeMediaRetryFact(fact)
	encoded, err := common.Marshal(policy)
	if err != nil {
		return mediaretry.Decision{}, err
	}
	mediaRetryCompiler.Lock()
	compiled := mediaRetryCompiler.compiled
	if compiled == nil || mediaRetryCompiler.document != string(encoded) {
		compiled, err = mediaretry.Compile(policy)
		if err == nil {
			mediaRetryCompiler.document, mediaRetryCompiler.compiled = string(encoded), compiled
		}
	}
	mediaRetryCompiler.Unlock()
	if err != nil {
		return mediaretry.Decision{}, err
	}
	return compiled.Evaluate(fact, state), nil
}
