package service

import (
	"errors"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
)

type imageCleanupLoadGuard struct {
	mu          sync.Mutex
	checked     time.Time
	total       float64
	idle        float64
	cgroupUsage float64
	high        int
	low         int
	paused      bool
	reason      string
}

var imageCleanupLoad imageCleanupLoadGuard

var imageCleanupActiveWorkers atomic.Int64

type ImageCleanupRuntimeStatus struct {
	ActiveWorkers  int64  `json:"active_workers"`
	PressureReason string `json:"pressure_reason"`
	SampledAt      int64  `json:"sampled_at"`
}

func GetImageCleanupRuntimeStatus() ImageCleanupRuntimeStatus {
	imageCleanupLoad.mu.Lock()
	defer imageCleanupLoad.mu.Unlock()
	status := ImageCleanupRuntimeStatus{ActiveWorkers: imageCleanupActiveWorkers.Load(), PressureReason: imageCleanupLoad.reason}
	if !imageCleanupLoad.checked.IsZero() {
		status.SampledAt = imageCleanupLoad.checked.Unix()
	}
	return status
}

// Check samples counters without sleeping or depending on the optional UI
// performance monitor. A container's limits override host-wide denominators.
func (guard *imageCleanupLoadGuard) Check(cfg ImageCleanupConfig) string {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	now := time.Now()
	if !guard.checked.IsZero() && now.Sub(guard.checked) < 5*time.Second {
		return guard.reason
	}
	times, err := cpu.Times(false)
	memory, memErr := mem.VirtualMemory()
	if err != nil || memErr != nil || len(times) == 0 {
		guard.reason = "Cleanup is waiting for valid CPU and memory measurements"
		return guard.reason
	}
	stats := times[0]
	total := stats.User + stats.System + stats.Idle + stats.Nice + stats.Iowait + stats.Irq + stats.Softirq + stats.Steal
	idle := stats.Idle + stats.Iowait
	usage := float64(0)
	if total > guard.total {
		usage = 100 * (1 - (idle-guard.idle)/(total-guard.total))
	}
	memoryUsage := memory.UsedPercent
	first := guard.checked.IsZero()
	valid := !math.IsNaN(total) && !math.IsInf(total, 0) && !math.IsNaN(idle) && !math.IsInf(idle, 0) && total >= idle && idle >= 0 && (first || total > guard.total && idle >= guard.idle && idle-guard.idle <= total-guard.total)
	if runtime.GOOS == "linux" {
		sample, sampleErr := readImageCleanupContainerSample(os.ReadFile)
		if sampleErr != nil {
			valid = false
		} else {
			memoryUsage = max(memoryUsage, sample.MemoryPercent)
			if sample.CPUCores > 0 && !first {
				if sample.CPUSeconds < guard.cgroupUsage {
					valid = false
				} else {
					usage = max(usage, 100*(sample.CPUSeconds-guard.cgroupUsage)/now.Sub(guard.checked).Seconds()/sample.CPUCores)
				}
			}
			guard.cgroupUsage = sample.CPUSeconds
		}
	}
	guard.checked, guard.total, guard.idle = now, total, idle
	if !valid {
		guard.reason = "Cleanup is waiting for valid host and container resource measurements"
		return guard.reason
	}
	if first {
		guard.reason = "Cleanup is waiting for an initial CPU sample"
		return guard.reason
	}
	return guard.observePressure(cfg, usage, memoryUsage)
}

func (guard *imageCleanupLoadGuard) observePressure(cfg ImageCleanupConfig, usage, memoryUsage float64) string {
	if math.IsNaN(usage) || math.IsInf(usage, 0) || usage < 0 || math.IsNaN(memoryUsage) || math.IsInf(memoryUsage, 0) || memoryUsage < 0 {
		guard.reason = "Cleanup is waiting for valid CPU and memory measurements"
		guard.low = 0
		return guard.reason
	}
	if usage >= float64(cfg.CPUPausePercent) || memoryUsage >= float64(cfg.MemoryPausePercent) {
		guard.high++
		guard.low = 0
		if guard.high >= 2 {
			guard.paused = true
		}
	} else if usage < float64(cfg.CPUPausePercent-10) && memoryUsage < float64(cfg.MemoryPausePercent-10) {
		guard.low++
		guard.high = 0
		if guard.low >= 3 {
			guard.paused = false
		}
	} else {
		guard.high, guard.low = 0, 0
	}
	guard.reason = ""
	if guard.paused {
		guard.reason = "Cleanup is waiting for CPU or memory pressure to fall"
	}
	return guard.reason
}

type imageCleanupContainerSample struct{ MemoryPercent, CPUSeconds, CPUCores float64 }

func readImageCleanupContainerSample(readFile func(string) ([]byte, error)) (imageCleanupContainerSample, error) {
	var sample imageCleanupContainerSample
	_, dockerErr := readFile("/.dockerenv")
	_, podmanErr := readFile("/run/.containerenv")
	cgroups, _ := readFile("/proc/self/cgroup")
	// A cgroup namespace can hide container names ("0::/"). If a resource
	// controller is present, require readable limits even without marker files.
	container := dockerErr == nil || podmanErr == nil || len(strings.TrimSpace(string(cgroups))) > 0
	used, usedErr := readFile("/sys/fs/cgroup/memory.current")
	limit, limitErr := readFile("/sys/fs/cgroup/memory.max")
	if usedErr != nil || limitErr != nil {
		used, usedErr = readFile("/sys/fs/cgroup/memory/memory.usage_in_bytes")
		limit, limitErr = readFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	}
	memoryKnown := usedErr == nil && limitErr == nil
	if memoryKnown {
		usage, err := strconv.ParseFloat(strings.TrimSpace(string(used)), 64)
		if err != nil || math.IsNaN(usage) || math.IsInf(usage, 0) || usage < 0 {
			return sample, errors.New("invalid container memory counter")
		}
		if strings.TrimSpace(string(limit)) != "max" {
			maximum, err := strconv.ParseFloat(strings.TrimSpace(string(limit)), 64)
			if err != nil || math.IsNaN(maximum) || math.IsInf(maximum, 0) || maximum <= 0 {
				return sample, errors.New("invalid container memory limit")
			}
			sample.MemoryPercent = 100 * usage / maximum
		}
	}
	maximum, maxErr := readFile("/sys/fs/cgroup/cpu.max")
	counter, counterErr := readFile("/sys/fs/cgroup/cpu.stat")
	cpuKnown := maxErr == nil && counterErr == nil
	var quotaText, periodText, usageText string
	divisor := float64(1e6)
	if cpuKnown {
		fields := strings.Fields(string(maximum))
		if len(fields) != 2 {
			return sample, errors.New("invalid container CPU limit")
		}
		quotaText, periodText = fields[0], fields[1]
		for line := range strings.SplitSeq(string(counter), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "usage_usec" {
				usageText = fields[1]
			}
		}
	} else {
		quota, e1 := readFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
		period, e2 := readFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
		usage, e3 := readFile("/sys/fs/cgroup/cpuacct/cpuacct.usage")
		if e1 != nil || e2 != nil || e3 != nil {
			quota, e1 = readFile("/sys/fs/cgroup/cpu,cpuacct/cpu.cfs_quota_us")
			period, e2 = readFile("/sys/fs/cgroup/cpu,cpuacct/cpu.cfs_period_us")
			usage, e3 = readFile("/sys/fs/cgroup/cpu,cpuacct/cpuacct.usage")
		}
		cpuKnown = e1 == nil && e2 == nil && e3 == nil
		quotaText, periodText, usageText = strings.TrimSpace(string(quota)), strings.TrimSpace(string(period)), strings.TrimSpace(string(usage))
		divisor = 1e9
	}
	if cpuKnown {
		usage, err := strconv.ParseFloat(usageText, 64)
		if err != nil || math.IsNaN(usage) || math.IsInf(usage, 0) || usage < 0 {
			return sample, errors.New("invalid container CPU counter")
		}
		sample.CPUSeconds = usage / divisor
		if quotaText != "max" && quotaText != "-1" {
			quota, e1 := strconv.ParseFloat(quotaText, 64)
			period, e2 := strconv.ParseFloat(periodText, 64)
			if e1 != nil || e2 != nil || math.IsNaN(quota) || math.IsInf(quota, 0) || math.IsNaN(period) || math.IsInf(period, 0) || quota <= 0 || period <= 0 {
				return sample, errors.New("invalid container CPU quota")
			}
			sample.CPUCores = quota / period
		}
	}
	if container && (!memoryKnown || !cpuKnown) {
		return sample, errors.New("container limits are unavailable")
	}
	return sample, nil
}
