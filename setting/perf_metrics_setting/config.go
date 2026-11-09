package perf_metrics_setting

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

const VideoErrorExclusionsOptionKey = "perf_metrics_setting.video_error_exclusions"

// ParseVideoErrorExclusions treats each trimmed line as a literal substring.
// The same rules are returned publicly so viewers can inspect the calculation.
func ParseVideoErrorExclusions(value string) ([]string, error) {
	if len(value) > 16384 || !utf8.ValidString(value) {
		return nil, errors.New("Video error exclusions must be valid UTF-8 and at most 16384 bytes")
	}
	var rules []string
	seen := make(map[string]struct{})
	for line := range strings.SplitSeq(value, "\n") {
		rule := strings.TrimSpace(line)
		if rule == "" {
			continue
		}
		if utf8.RuneCountInString(rule) > 256 || strings.ContainsFunc(rule, unicode.IsControl) {
			return nil, errors.New("Each video error exclusion must be at most 256 characters with no control characters")
		}
		if _, ok := seen[rule]; ok {
			continue
		}
		if len(rules) == 40 {
			return nil, errors.New("Use at most 40 distinct video error exclusions")
		}
		seen[rule] = struct{}{}
		rules = append(rules, rule)
	}
	return rules, nil
}

func GetVideoErrorExclusions() []string {
	common.OptionMapRWMutex.RLock()
	value := common.OptionMap[VideoErrorExclusionsOptionKey]
	common.OptionMapRWMutex.RUnlock()
	rules, _ := ParseVideoErrorExclusions(value)
	return rules
}

type PerfMetricsSetting struct {
	Enabled       bool   `json:"enabled"`
	FlushInterval int    `json:"flush_interval"`
	BucketTime    string `json:"bucket_time"`
	RetentionDays int    `json:"retention_days"`
}

var perfMetricsSetting = PerfMetricsSetting{
	Enabled:       true,
	FlushInterval: 5,
	BucketTime:    "hour",
	RetentionDays: 0,
}

func init() {
	config.GlobalConfig.Register("perf_metrics_setting", &perfMetricsSetting)
}

func GetSetting() PerfMetricsSetting {
	return perfMetricsSetting
}

func GetBucketSeconds() int64 {
	switch perfMetricsSetting.BucketTime {
	case "minute":
		return 60
	case "5min":
		return 300
	case "hour":
		return 3600
	default:
		return 3600
	}
}

func GetFlushIntervalMinutes() int {
	if perfMetricsSetting.FlushInterval < 1 {
		return 1
	}
	return perfMetricsSetting.FlushInterval
}
