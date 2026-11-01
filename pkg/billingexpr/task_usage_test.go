package billingexpr

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskUsageExpressionUsesFactsAndTaskQuotaConversion(t *testing.T) {
	expression := `tier("1080p", u("seconds") * (u("resolution") == "1080p" ? 0.4 : 0.2))`
	cost, trace, err := RunExprWithRequest(expression, TokenParams{}, RequestInput{Usage: map[string]any{"seconds": 10.0, "resolution": "1080p"}})
	require.NoError(t, err)
	assert.Equal(t, 4.0, cost)
	assert.Equal(t, "1080p", trace.MatchedTier)
	result, err := ComputeTieredQuotaWithRequest(&BillingSnapshot{ExprString: expression, ExprHash: ExprHashString(expression), GroupRatio: 2, QuotaPerUnit: 500000, ExprVersion: 1, TaskUsageBilling: true}, TokenParams{}, RequestInput{Usage: map[string]any{"seconds": 10.0, "resolution": "1080p"}})
	require.NoError(t, err)
	assert.Equal(t, 4_000_000, result.ActualQuotaAfterGroup)
}

func TestTaskUsageReferenceVideoMultipliersApplyToWholePrice(t *testing.T) {
	const secondsExpression = `(u("resolution") == "480p" ? tier("480p", 0.5 + u("seconds") * 0.5) : tier("720p", 1 + u("seconds") * 0.75)) * (u("video_input") == "video" ? 1.5 : 1.25)`
	const requestsExpression = `(u("resolution") == "480p" ? tier("480p", 0.5 + u("requests") * 2) : tier("720p", 1 + u("requests") * 3)) * (u("video_input") == "video" ? 1.5 : 0)`
	for _, testCase := range []struct {
		name       string
		expression string
		usage      map[string]any
		groupRatio float64
		costUSD    float64
		quota      int
	}{
		{
			name:       "seconds without reference video multiplies usage and additional charge",
			expression: secondsExpression,
			usage:      map[string]any{"seconds": 5.0, "resolution": "480p", "video_input": "none"},
			groupRatio: 2,
			costUSD:    3.75,
			quota:      3_750_000,
		},
		{
			name:       "seconds with reference video preserves resolution price and group ratio",
			expression: secondsExpression,
			usage:      map[string]any{"seconds": 5.0, "resolution": "720p", "video_input": "video"},
			groupRatio: 2,
			costUSD:    7.125,
			quota:      7_125_000,
		},
		{
			name:       "requests with reference video multiplies per-request price and additional charge",
			expression: requestsExpression,
			usage:      map[string]any{"requests": 1.0, "resolution": "720p", "video_input": "video"},
			groupRatio: 1.5,
			costUSD:    6,
			quota:      4_500_000,
		},
		{
			name:       "explicit zero multiplier makes the whole request free",
			expression: requestsExpression,
			usage:      map[string]any{"requests": 1.0, "resolution": "480p", "video_input": "none"},
			groupRatio: 1.5,
			costUSD:    0,
			quota:      0,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot := &BillingSnapshot{
				ExprString:       testCase.expression,
				ExprHash:         ExprHashString(testCase.expression),
				GroupRatio:       testCase.groupRatio,
				QuotaPerUnit:     500000,
				ExprVersion:      1,
				TaskUsageBilling: true,
			}
			result, err := ComputeTieredQuotaWithRequest(snapshot, TokenParams{}, RequestInput{Usage: testCase.usage})
			require.NoError(t, err)
			assert.Equal(t, testCase.costUSD*500000, result.ActualQuotaBeforeGroup)
			assert.Equal(t, testCase.quota, result.ActualQuotaAfterGroup)
			assert.Equal(t, testCase.usage["resolution"], result.MatchedTier)
		})
	}
}
