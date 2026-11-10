package perfmetrics

import "sync/atomic"

type Store interface {
	Record(sample Sample)
	Query(params QueryParams) (QueryResult, error)
}

type Sample struct {
	Model        string
	Group        string
	LatencyMs    int64
	TtftMs       int64
	HasTtft      bool
	Success      bool
	OutputTokens int64
	GenerationMs int64
}

type QueryParams struct {
	Model  string
	Group  string
	Hours  int
	Groups []string
}

// MetricStats describes the observations in a window, not model capacity.
// Unknown outcomes count as undelivered and remain separately visible.
type MetricStats struct {
	AvgTtftMs      int64   `json:"avg_ttft_ms"`
	AvgLatencyMs   int64   `json:"avg_latency_ms"`
	SuccessRate    float64 `json:"success_rate"`
	AvgTps         float64 `json:"avg_tps"`
	RequestCount   int64   `json:"request_count"`
	SuccessCount   int64   `json:"success_count"`
	FailureCount   int64   `json:"failure_count"`
	UnknownCount   int64   `json:"unknown_count"`
	LatencyCount   int64   `json:"latency_count"`
	TotalLatencyMs int64   `json:"total_latency_ms"`
	OutputCount    int64   `json:"output_count"`
	Workload       string  `json:"workload"`

	VideoFilteredSuccessRate *FilteredSuccessRate `json:"video_filtered_success_rate,omitempty"`
	ImageFilteredSuccessRate *FilteredSuccessRate `json:"image_filtered_success_rate,omitempty"`
}

// FilteredSuccessRate counts terminal outcomes for one async workload. The
// rate is null when all of its samples were excluded.
type FilteredSuccessRate struct {
	Rate          *float64 `json:"rate"`
	ExcludedCount int64    `json:"excluded_count"`
	SampleCount   int64    `json:"sample_count"`
}

type VideoFilteredSuccessRate = FilteredSuccessRate

type BucketPoint struct {
	Ts int64 `json:"ts"`
	MetricStats
}

type GroupResult struct {
	Group string `json:"group"`
	MetricStats
	Series []BucketPoint `json:"series"`
}

type QueryResult struct {
	ModelName    string        `json:"model_name"`
	SeriesSchema string        `json:"series_schema"`
	Groups       []GroupResult `json:"groups"`
	Summary      MetricStats   `json:"summary"`
	WindowHours  int           `json:"window_hours"`
	UpdatedAt    int64         `json:"updated_at"`

	VideoErrorExclusions []string `json:"video_error_exclusions,omitempty"`
	ImageErrorExclusions []string `json:"image_error_exclusions,omitempty"`
}

type SuccessRatePoint struct {
	Ts          int64   `json:"ts"`
	SuccessRate float64 `json:"success_rate"`
}

type ModelSummary struct {
	ModelName string `json:"model_name"`
	MetricStats
	RecentSuccessSeries []SuccessRatePoint `json:"recent_success_series,omitempty"`
}

type SummaryAllResult struct {
	Models      []ModelSummary `json:"models"`
	WindowHours int            `json:"window_hours"`
	UpdatedAt   int64          `json:"updated_at"`

	VideoErrorExclusions []string `json:"video_error_exclusions,omitempty"`
	ImageErrorExclusions []string `json:"image_error_exclusions,omitempty"`
}

type bucketKey struct {
	model    string
	group    string
	bucketTs int64
}

type counters struct {
	requestCount   int64
	successCount   int64
	totalLatencyMs int64
	ttftSumMs      int64
	ttftCount      int64
	outputTokens   int64
	generationMs   int64
	unknownCount   int64
	latencyCount   int64
	outputCount    int64
	workloads      uint8

	videoFiltered taskFilterCounters
	imageFiltered taskFilterCounters
}

type taskFilterCounters struct {
	requestCount  int64
	successCount  int64
	excludedCount int64
}

func (c *taskFilterCounters) add(value taskFilterCounters) {
	c.requestCount += value.requestCount
	c.successCount += value.successCount
	c.excludedCount += value.excludedCount
}

func (c taskFilterCounters) stats() *FilteredSuccessRate {
	if c.requestCount == 0 {
		return nil
	}
	filtered := &FilteredSuccessRate{
		ExcludedCount: c.excludedCount,
		SampleCount:   c.requestCount - c.excludedCount,
	}
	if filtered.SampleCount > 0 {
		rate := float64(c.successCount) / float64(filtered.SampleCount) * 100
		filtered.Rate = &rate
	}
	return filtered
}

const (
	relayWorkload uint8 = 1 << iota
	imageWorkload
	videoWorkload
)

func (c *counters) add(value counters) {
	c.requestCount += value.requestCount
	c.successCount += value.successCount
	c.unknownCount += value.unknownCount
	c.totalLatencyMs += value.totalLatencyMs
	c.latencyCount += value.latencyCount
	c.ttftSumMs += value.ttftSumMs
	c.ttftCount += value.ttftCount
	c.outputTokens += value.outputTokens
	c.generationMs += value.generationMs
	c.outputCount += value.outputCount
	c.workloads |= value.workloads
	c.videoFiltered.add(value.videoFiltered)
	c.imageFiltered.add(value.imageFiltered)
}

func (c counters) stats() MetricStats {
	workload := "mixed"
	switch c.workloads {
	case 0, relayWorkload:
		workload = "relay"
	case imageWorkload:
		workload = "image"
	case videoWorkload:
		workload = "video"
	}
	return MetricStats{
		AvgTtftMs:    avg(c.ttftSumMs, c.ttftCount),
		AvgLatencyMs: avg(c.totalLatencyMs, c.latencyCount),
		SuccessRate:  successRate(c), AvgTps: avgTps(c),
		RequestCount: c.requestCount, SuccessCount: c.successCount,
		FailureCount: c.requestCount - c.successCount - c.unknownCount,
		UnknownCount: c.unknownCount, LatencyCount: c.latencyCount,
		TotalLatencyMs: c.totalLatencyMs, OutputCount: c.outputCount,
		Workload:                 workload,
		VideoFilteredSuccessRate: c.videoFiltered.stats(),
		ImageFilteredSuccessRate: c.imageFiltered.stats(),
	}
}

type atomicBucket struct {
	requestCount   atomic.Int64
	successCount   atomic.Int64
	totalLatencyMs atomic.Int64
	ttftSumMs      atomic.Int64
	ttftCount      atomic.Int64
	outputTokens   atomic.Int64
	generationMs   atomic.Int64
}

func (b *atomicBucket) add(sample Sample) {
	b.requestCount.Add(1)
	if sample.Success {
		b.successCount.Add(1)
	}
	if sample.LatencyMs > 0 {
		b.totalLatencyMs.Add(sample.LatencyMs)
	}
	if sample.HasTtft && sample.TtftMs >= 0 {
		b.ttftSumMs.Add(sample.TtftMs)
		b.ttftCount.Add(1)
	}
	if sample.OutputTokens > 0 && sample.GenerationMs > 0 {
		b.outputTokens.Add(sample.OutputTokens)
		b.generationMs.Add(sample.GenerationMs)
	}
}

func (b *atomicBucket) snapshot() counters {
	value := counters{
		requestCount:   b.requestCount.Load(),
		successCount:   b.successCount.Load(),
		totalLatencyMs: b.totalLatencyMs.Load(),
		ttftSumMs:      b.ttftSumMs.Load(),
		ttftCount:      b.ttftCount.Load(),
		outputTokens:   b.outputTokens.Load(),
		generationMs:   b.generationMs.Load(),
	}
	value.latencyCount = value.requestCount
	value.workloads = relayWorkload
	return value
}

func (b *atomicBucket) drain() counters {
	return counters{
		requestCount:   b.requestCount.Swap(0),
		successCount:   b.successCount.Swap(0),
		totalLatencyMs: b.totalLatencyMs.Swap(0),
		ttftSumMs:      b.ttftSumMs.Swap(0),
		ttftCount:      b.ttftCount.Swap(0),
		outputTokens:   b.outputTokens.Swap(0),
		generationMs:   b.generationMs.Swap(0),
	}
}

func (b *atomicBucket) addCounters(c counters) {
	if c.requestCount != 0 {
		b.requestCount.Add(c.requestCount)
	}
	if c.successCount != 0 {
		b.successCount.Add(c.successCount)
	}
	if c.totalLatencyMs != 0 {
		b.totalLatencyMs.Add(c.totalLatencyMs)
	}
	if c.ttftSumMs != 0 {
		b.ttftSumMs.Add(c.ttftSumMs)
	}
	if c.ttftCount != 0 {
		b.ttftCount.Add(c.ttftCount)
	}
	if c.outputTokens != 0 {
		b.outputTokens.Add(c.outputTokens)
	}
	if c.generationMs != 0 {
		b.generationMs.Add(c.generationMs)
	}
}
