package agent

import (
	"errors"
	"log/slog"
	"math"
	"runtime"
	"sync/atomic"

	"github.com/henrygd/beszel/internal/entities/system"
	"github.com/shirou/gopsutil/v4/cpu"
)

var lastCpuTimes = make(map[uint16]cpu.TimesStat)
var lastPerCoreCpuTimes = make(map[uint16][]cpu.TimesStat)

// cpuZeroPolls counts consecutive default-interval polls that reported 0%
// CPU without an error, so a persistently dead source is surfaced as a
// warning instead of silently pinning the graph at zero forever.
var cpuZeroPolls atomic.Int32

// init initializes the CPU monitoring by storing the initial CPU times
// for the default 60-second cache interval.
func init() {
	if times, err := cpu.Times(false); err == nil && len(times) > 0 {
		lastCpuTimes[60000] = times[0]
	} else if androidNoProcStat() {
		// Unrooted Android: /proc/stat is SELinux-denied; seed the fallback.
		androidCpuTimes(false) //nolint:errcheck
	}
	if perCoreTimes, err := cpu.Times(true); err == nil && len(perCoreTimes) > 0 {
		lastPerCoreCpuTimes[60000] = perCoreTimes
	} else if androidNoProcStat() {
		androidCpuTimes(true) //nolint:errcheck
	}
}

// noteCpuPollOutcome tracks consecutive zero-CPU polls at the default
// interval and warns once when a dead source is suspected. A working CPU
// source only ever reports brief zero readings, so several consecutive zero
// polls mean the data source is producing nothing useful.
func noteCpuPollOutcome(cacheTimeMs uint16, err error, cpuValue float64) {
	if cacheTimeMs != defaultDataCacheTimeMs {
		return
	}
	if err == nil && cpuValue == 0 {
		if n := cpuZeroPolls.Add(1); n == 3 {
			slog.Warn("CPU reported 0% for 3 consecutive polls",
				"hint", "run with LOG_LEVEL=debug to inspect CPU sampling",
				"android_hint", "on Termux the CPU fallback needs readable /proc/[pid]/stat or cpufreq stats")
		}
		return
	}
	cpuZeroPolls.Store(0)
}

// seedFallbackBaseline replaces a missing or all-zero baseline with the
// fallback sample, so the next poll produces a real delta instead of either
// a garbage delta against a dead all-zero gopsutil baseline or an
// average-since-boot value.
func seedFallbackBaseline(cacheTimeMs uint16, sample cpu.TimesStat) {
	if cur, ok := lastCpuTimes[60000]; !ok || cpuTimesAllZero(cur) {
		slog.Debug("cpu: seeding missing/dead baseline from android fallback sample")
		lastCpuTimes[60000] = sample
		lastCpuTimes[cacheTimeMs] = sample
	}
}

// seedFallbackPerCoreBaseline is seedFallbackBaseline for the per-core map:
// it seeds when the baseline is missing, has the wrong core count, or is
// all-zero (dead /proc/stat counters).
func seedFallbackPerCoreBaseline(cacheTimeMs uint16, samples []cpu.TimesStat) {
	baseline, ok := lastPerCoreCpuTimes[60000]
	if ok && len(baseline) == len(samples) && len(baseline) > 0 && !cpuTimesAllZero(baseline[0]) {
		return
	}
	slog.Debug("cpu: seeding missing/dead per-core baseline from android fallback samples")
	lastPerCoreCpuTimes[60000] = samples
	lastPerCoreCpuTimes[cacheTimeMs] = samples
}

// CpuMetrics contains detailed CPU usage breakdown
type CpuMetrics struct {
	Total   float64
	User    float64
	System  float64
	Iowait  float64
	Steal   float64
	Idle    float64
	Irq     float64
	Softirq float64
	Nice    float64
	// fromCgroup is set when Total comes from cgroup accounting rather than
	// /proc/stat, so per-core /proc/stat usage would not match it.
	fromCgroup bool
}

// getCpuMetrics calculates detailed CPU usage metrics using cached previous measurements.
// It returns percentages for total, user, system, iowait, and steal time.
func getCpuMetrics(cacheTimeMs uint16, forceUseCgroup bool) (CpuMetrics, error) {
	// Inside LXC, lxcfs serves /proc/stat with the host cores' counters, not
	// the guest's own usage. Prefer cgroup accounting there or when requested. (#2332)
	if metrics, ok := containerCpuMetrics(cacheTimeMs, forceUseCgroup); ok {
		metrics.fromCgroup = true
		return metrics, nil
	}
	times, err := cpu.Times(false)
	if androidNoProcStat() && (err != nil || len(times) == 0 || cpuTimesAllZero(times[0])) {
		// Unrooted Android fallback (henrygd/beszel#2346): synthesize
		// cumulative counters from process ticks or cpufreq residency.
		// The fallback also engages when gopsutil "succeeds" but /proc/stat
		// reads back all-zero counters (restricted kernels), which would
		// otherwise silently pin the reported CPU at 0%.
		if fallback, ferr := androidCpuTimes(false); ferr == nil && len(fallback) > 0 {
			slog.Debug("cpu: android fallback sample", "busy_seconds", fallback[0].User, "idle_seconds", fallback[0].Idle)
			seedFallbackBaseline(cacheTimeMs, fallback[0])
			return cpuMetricsFromTimes(cacheTimeMs, fallback[0]), nil
		} else if ferr != nil {
			slog.Debug("cpu: android fallback failed", "err", ferr)
			return CpuMetrics{}, ferr
		}
	}
	if err != nil || len(times) == 0 {
		if err == nil {
			// Make an empty-but-successful cpu.Times visible instead of
			// silently publishing zero CPU forever.
			err = errors.New("cpu.Times returned no data")
		}
		return CpuMetrics{}, err
	}
	slog.Debug("cpu: gopsutil sample", "user_seconds", times[0].User, "idle_seconds", times[0].Idle)
	return cpuMetricsFromTimes(cacheTimeMs, times[0]), nil
}

// cpuMetricsFromTimes calculates host CPU usage from the latest counters.
func cpuMetricsFromTimes(cacheTimeMs uint16, current cpu.TimesStat) CpuMetrics {
	// if cacheTimeMs is not in lastCpuTimes, use 60000 as fallback lastCpuTime
	if _, ok := lastCpuTimes[cacheTimeMs]; !ok {
		lastCpuTimes[cacheTimeMs] = lastCpuTimes[60000]
	}

	t1 := lastCpuTimes[cacheTimeMs]
	t2 := current
	// A counter reset or wrap must establish a new baseline even if this
	// sample cannot produce a valid delta.
	lastCpuTimes[cacheTimeMs] = current

	t1All, _ := getAllBusy(t1)
	t2All, _ := getAllBusy(t2)

	totalDelta := t2All - t1All
	if totalDelta <= 0 {
		slog.Debug("cpu: delta not positive; reporting zero", "t1_all", t1All, "t2_all", t2All, "total_delta", totalDelta)
		return CpuMetrics{}
	}

	metrics := CpuMetrics{
		Total:   calculateBusy(t1, t2),
		User:    clampPercent((t2.User - t1.User) / totalDelta * 100),
		System:  clampPercent((t2.System - t1.System) / totalDelta * 100),
		Iowait:  clampPercent((t2.Iowait - t1.Iowait) / totalDelta * 100),
		Steal:   clampPercent((t2.Steal - t1.Steal) / totalDelta * 100),
		Idle:    clampPercent((t2.Idle - t1.Idle) / totalDelta * 100),
		Irq:     clampPercent((t2.Irq - t1.Irq) / totalDelta * 100),
		Softirq: clampPercent((t2.Softirq - t1.Softirq) / totalDelta * 100),
		Nice:    clampPercent((t2.Nice - t1.Nice) / totalDelta * 100),
	}

	slog.Debug("cpu: metrics", "total", metrics.Total, "user", metrics.User, "system", metrics.System, "total_delta", totalDelta)
	return metrics
}

// clampPercent ensures the percentage is between 0 and 100
func clampPercent(value float64) float64 {
	return math.Min(100, math.Max(0, value))
}

// getPerCoreCpuUsage calculates per-core CPU busy usage as integer percentages (0-100).
// It uses cached previous measurements for the provided cache interval.
func getPerCoreCpuUsage(cacheTimeMs uint16) (system.Uint8Slice, error) {
	perCoreTimes, err := cpu.Times(true)
	if androidNoProcStat() && (err != nil || len(perCoreTimes) == 0 || cpuTimesAllZero(perCoreTimes[0])) {
		// Unrooted Android fallback (henrygd/beszel#2346).
		if fallback, ferr := androidCpuTimes(true); ferr == nil && len(fallback) > 0 {
			seedFallbackPerCoreBaseline(cacheTimeMs, fallback)
			perCoreTimes = fallback
			err = nil
		} else if ferr != nil {
			slog.Debug("cpu: android per-core fallback failed", "err", ferr)
			return nil, ferr
		}
	}
	if err != nil || len(perCoreTimes) == 0 {
		if err != nil {
			slog.Debug("cpu: per-core times unavailable", "err", err)
		}
		return nil, err
	}

	// Initialize cache if needed
	if _, ok := lastPerCoreCpuTimes[cacheTimeMs]; !ok {
		lastPerCoreCpuTimes[cacheTimeMs] = lastPerCoreCpuTimes[60000]
	}

	lastTimes := lastPerCoreCpuTimes[cacheTimeMs]

	// Limit to the number of cores available in both samples
	length := min(len(lastTimes), len(perCoreTimes))

	usage := make([]uint8, length)
	for i := 0; i < length; i++ {
		t1 := lastTimes[i]
		t2 := perCoreTimes[i]
		usage[i] = uint8(math.Round(calculateBusy(t1, t2)))
	}

	lastPerCoreCpuTimes[cacheTimeMs] = perCoreTimes
	slog.Debug("cpu: per-core usage", "cores", length, "usage", usage)
	return usage, nil
}

// calculateBusy calculates the CPU busy percentage between two time points.
// It computes the ratio of busy time to total time elapsed between t1 and t2,
// returning a percentage clamped between 0 and 100.
func calculateBusy(t1, t2 cpu.TimesStat) float64 {
	t1All, t1Busy := getAllBusy(t1)
	t2All, t2Busy := getAllBusy(t2)

	if t2All <= t1All || t2Busy <= t1Busy {
		return 0
	}
	return clampPercent((t2Busy - t1Busy) / (t2All - t1All) * 100)
}

// getAllBusy calculates the total CPU time and busy CPU time from CPU times statistics.
// On Linux, it excludes guest and guest_nice time from the total to match kernel behavior.
// Returns total CPU time and busy CPU time (total minus idle and I/O wait time).
func getAllBusy(t cpu.TimesStat) (float64, float64) {
	tot := t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq +
		t.Softirq + t.Steal + t.Guest + t.GuestNice
	if runtime.GOOS == "linux" {
		tot -= t.Guest     // Linux 2.6.24+
		tot -= t.GuestNice // Linux 3.2.0+
	}

	busy := tot - t.Idle - t.Iowait

	return tot, busy
}
