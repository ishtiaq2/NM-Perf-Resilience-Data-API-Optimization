package api

import (
	"math"
	"os"
	"runtime"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LagMonitor measures scheduling lag: how much later than requested a sleeping
// goroutine gets to run. It is the Go counterpart of Node's event-loop lag and
// shows when the whole process is starved of CPU (the heartbeat would suffer).
type LagMonitor struct {
	mu   sync.Mutex
	ring []float64
	next int
	n    int
}

// Lag percentiles over the monitor's window, in milliseconds.
type Lag struct {
	P50Ms float64 `json:"schedLagP50Ms"`
	P99Ms float64 `json:"schedLagP99Ms"`
	MaxMs float64 `json:"schedLagMaxMs"`
}

// NewLagMonitor samples every interval and keeps the last window samples.
func NewLagMonitor(interval time.Duration, window int) *LagMonitor {
	m := &LagMonitor{ring: make([]float64, window)}
	go func() {
		for {
			start := time.Now()
			time.Sleep(interval)
			lag := float64(time.Since(start)-interval) / float64(time.Millisecond)
			m.mu.Lock()
			m.ring[m.next] = math.Max(0, lag)
			m.next = (m.next + 1) % len(m.ring)
			m.n = min(m.n+1, len(m.ring))
			m.mu.Unlock()
		}
	}()
	return m
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// Snapshot of the current window.
func (m *LagMonitor) Snapshot() Lag {
	m.mu.Lock()
	s := slices.Clone(m.ring[:m.n])
	m.mu.Unlock()
	if len(s) == 0 {
		return Lag{}
	}
	slices.Sort(s)
	at := func(p float64) float64 { return round1(s[min(len(s)-1, int(float64(len(s))*p))]) }
	return Lag{P50Ms: at(0.5), P99Ms: at(0.99), MaxMs: round1(s[len(s)-1])}
}

// RuntimeStats come from runtime/metrics (no stop-the-world, unlike ReadMemStats).
type RuntimeStats struct {
	Goroutines    uint64  `json:"goroutines"`
	GoMaxProcs    int     `json:"gomaxprocs"`
	HeapMB        float64 `json:"heapMB"`
	GoTotalMB     float64 `json:"goTotalMB"`
	RSSMB         float64 `json:"rssMB"`
	MemLimitMB    float64 `json:"memLimitMB,omitempty"`
	GCCycles      uint64  `json:"gcCycles"`
	GCPauseP99Ms  float64 `json:"gcPauseP99Ms"`
	SchedLatP99Ms float64 `json:"schedLatencyP99Ms"`
	GoVersion     string  `json:"goVersion"`
	Arch          string  `json:"arch"`
	OpenFDs       int     `json:"openFds,omitempty"`
	ThreadsOS     int     `json:"osThreads,omitempty"`
	CPUSeconds    float64 `json:"cpuSeconds,omitempty"`
}

var runtimeSamples = func() []metrics.Sample {
	names := []string{
		"/sched/goroutines:goroutines",
		"/memory/classes/heap/objects:bytes",
		"/memory/classes/total:bytes",
		"/gc/gomemlimit:bytes",
		"/gc/cycles/total:gc-cycles",
		"/sched/pauses/total/gc:seconds",
		"/sched/latencies:seconds",
	}
	s := make([]metrics.Sample, len(names))
	for i, n := range names {
		s[i].Name = n
	}
	return s
}()

var runtimeMu sync.Mutex

func histP99Ms(h *metrics.Float64Histogram) float64 {
	var total uint64
	for _, c := range h.Counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	want := uint64(math.Ceil(float64(total) * 0.99))
	var acc uint64
	for i, c := range h.Counts {
		acc += c
		if acc >= want {
			upper := h.Buckets[i+1]
			if math.IsInf(upper, 1) {
				upper = h.Buckets[i]
			}
			return round1(upper * 1000)
		}
	}
	return 0
}

// ReadRuntime collects the process statistics shown by /api/heartbeat and /api/metrics.
func ReadRuntime() RuntimeStats {
	runtimeMu.Lock()
	metrics.Read(runtimeSamples)
	st := RuntimeStats{GoMaxProcs: runtime.GOMAXPROCS(0), GoVersion: runtime.Version(), Arch: runtime.GOARCH}
	for _, s := range runtimeSamples {
		switch s.Name {
		case "/sched/goroutines:goroutines":
			if s.Value.Kind() == metrics.KindUint64 {
				st.Goroutines = s.Value.Uint64()
			}
		case "/memory/classes/heap/objects:bytes":
			if s.Value.Kind() == metrics.KindUint64 {
				st.HeapMB = round1(float64(s.Value.Uint64()) / (1 << 20))
			}
		case "/memory/classes/total:bytes":
			if s.Value.Kind() == metrics.KindUint64 {
				st.GoTotalMB = round1(float64(s.Value.Uint64()) / (1 << 20))
			}
		case "/gc/gomemlimit:bytes":
			if s.Value.Kind() == metrics.KindUint64 && s.Value.Uint64() < math.MaxInt64 {
				st.MemLimitMB = round1(float64(s.Value.Uint64()) / (1 << 20))
			}
		case "/gc/cycles/total:gc-cycles":
			if s.Value.Kind() == metrics.KindUint64 {
				st.GCCycles = s.Value.Uint64()
			}
		case "/sched/pauses/total/gc:seconds":
			if s.Value.Kind() == metrics.KindFloat64Histogram {
				st.GCPauseP99Ms = histP99Ms(s.Value.Float64Histogram())
			}
		case "/sched/latencies:seconds":
			if s.Value.Kind() == metrics.KindFloat64Histogram {
				st.SchedLatP99Ms = histP99Ms(s.Value.Float64Histogram())
			}
		}
	}
	runtimeMu.Unlock()
	st.RSSMB, st.ThreadsOS = procStatus()
	st.CPUSeconds = procCPUSeconds()
	if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
		st.OpenFDs = len(ents)
	}
	return st
}

// procStatus reads VmRSS and Threads from /proc/self/status (Linux; zero elsewhere).
func procStatus() (rssMB float64, threads int) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		switch k {
		case "VmRSS":
			kb, _ := strconv.ParseFloat(f[0], 64)
			rssMB = round1(kb / 1024)
		case "Threads":
			threads, _ = strconv.Atoi(f[0])
		}
	}
	return rssMB, threads
}

// procCPUSeconds is user+system CPU time of the process from /proc/self/stat
// (clock ticks, USER_HZ = 100 on Linux).
func procCPUSeconds() float64 {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 13 {
		return 0
	}
	ut, _ := strconv.ParseFloat(f[11], 64)
	stt, _ := strconv.ParseFloat(f[12], 64)
	return round1((ut + stt) / 100)
}
