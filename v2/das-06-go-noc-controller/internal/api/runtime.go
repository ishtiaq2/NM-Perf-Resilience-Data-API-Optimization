package api

import (
	"math"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var samples = func() []metrics.Sample {
	names := []string{
		"/sched/goroutines:goroutines",
		"/memory/classes/heap/objects:bytes",
		"/memory/classes/total:bytes",
		"/gc/gomemlimit:bytes",
		"/gc/cycles/total:gc-cycles",
		"/sched/pauses/total/gc:seconds",
		"/gc/heap/live:bytes",
		"/memory/classes/heap/stacks:bytes",
	}
	s := make([]metrics.Sample, len(names))
	for i, n := range names {
		s[i].Name = n
	}
	return s
}()

var samplesMu sync.Mutex

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// histQuantileMs returns the upper bucket bound of the q-quantile, in ms.
func histQuantileMs(h *metrics.Float64Histogram, q float64) float64 {
	var total uint64
	for _, c := range h.Counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	want := uint64(math.Ceil(float64(total) * q))
	var acc uint64
	for i, c := range h.Counts {
		acc += c
		if acc >= want {
			upper := h.Buckets[i+1]
			if math.IsInf(upper, 1) {
				upper = h.Buckets[i]
			}
			return math.Round(upper*1e5) / 1e2
		}
	}
	return 0
}

func rssMB() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				kb, _ := strconv.ParseFloat(f[0], 64)
				return round1(kb / 1024)
			}
		}
	}
	return 0
}

func cpuSeconds() float64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	sec := func(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
	return math.Round((sec(ru.Utime)+sec(ru.Stime))*100) / 100
}

func openFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

// processStats: memory, CPU, goroutines, and GC pauses: the distribution of the
// individual GC stop-the-world pauses since the process started
// (/sched/pauses/total/gc:seconds; the quantiles are histogram bucket bounds).
func processStats() map[string]any {
	samplesMu.Lock()
	metrics.Read(samples)
	out := map[string]any{
		"goroutines":   samples[0].Value.Uint64(),
		"heapMB":       round1(float64(samples[1].Value.Uint64()) / (1 << 20)),
		"goTotalMB":    round1(float64(samples[2].Value.Uint64()) / (1 << 20)),
		"gcCycles":     samples[4].Value.Uint64(),
		"gcPauseP50Ms": histQuantileMs(samples[5].Value.Float64Histogram(), 0.5),
		"gcPauseP99Ms": histQuantileMs(samples[5].Value.Float64Histogram(), 0.99),
		"liveHeapMB":   round1(float64(samples[6].Value.Uint64()) / (1 << 20)), // after the last GC
		"stacksMB":     round1(float64(samples[7].Value.Uint64()) / (1 << 20)),
	}
	if l := samples[3].Value.Uint64(); l < math.MaxInt64 {
		out["memLimitMB"] = round1(float64(l) / (1 << 20))
	}
	samplesMu.Unlock()
	out["rssMB"] = rssMB()
	out["cpuSeconds"] = cpuSeconds()
	out["openFds"] = openFDs()
	out["gomaxprocs"] = runtime.GOMAXPROCS(0)
	out["goVersion"] = runtime.Version()
	return out
}
