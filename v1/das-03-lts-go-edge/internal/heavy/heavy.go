// Package heavy runs CPU-heavy work (encoding, compression, normalisation) on a
// few dedicated OS threads with a lower scheduling priority (nice +10 on Linux).
//
// Go has no goroutine priorities: a goroutine encoding a 50 001-point sweep and
// the goroutine answering /api/heartbeat compete as equals. Dedicated reniced
// threads let the kernel prefer request handling, the same technique the Node
// hotfix uses for its worker threads. The pool also bounds heavy work to
// Workers() threads, so a burst of expensive requests cannot take every core.
//
// Before Start (tests, tools) Do simply runs the function on the caller.
// Do must not be nested: a heavy function must not call Do itself.
//
// Embedded-CPU emulation: with DAS_CPU_SLOWDOWN=K every heavy function runs K
// times (on a fast development machine). Leave it unset on the device.
package heavy

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"
)

// Factor is the emulated slowdown (>= 1), read once at start-up.
var Factor = func() int {
	n, err := strconv.Atoi(os.Getenv("DAS_CPU_SLOWDOWN"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}()

type job struct {
	fn       func()
	done     chan struct{}
	panicked any
}

var (
	jobs                   chan *job
	workers                int
	nice                   int
	queued, done, busyNano atomic.Int64
)

// Start launches n worker threads at the given nice value (Linux; ignored elsewhere).
// It returns the nice value actually applied (0 if renicing is not possible).
func Start(n, niceness int) int {
	if jobs != nil || n < 1 {
		return nice
	}
	jobs = make(chan *job)
	workers = n
	applied := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			runtime.LockOSThread() // this goroutine owns the thread for good, so the nice value sticks
			applied <- setThreadNice(niceness)
			for j := range jobs {
				t := time.Now()
				func() {
					defer func() { j.panicked = recover() }()
					j.fn()
				}()
				busyNano.Add(int64(time.Since(t)))
				done.Add(1)
				close(j.done)
			}
		}()
	}
	for i := 0; i < n; i++ {
		nice = <-applied
	}
	return nice
}

func run[T any](fn func() T) T {
	var r T
	for i := 0; i < Factor; i++ {
		r = fn()
	}
	return r
}

// Do runs fn on a heavy worker and waits for its result.
func Do[T any](fn func() T) T {
	if jobs == nil {
		return run(fn)
	}
	var out T
	j := &job{fn: func() { out = run(fn) }, done: make(chan struct{})}
	queued.Add(1)
	jobs <- j
	queued.Add(-1)
	<-j.done
	if j.panicked != nil {
		panic(fmt.Sprint("heavy: ", j.panicked))
	}
	return out
}

// Stats for /api/metrics.
func Stats() map[string]any {
	return map[string]any{"workers": workers, "nice": nice, "waiting": queued.Load(), "jobs": done.Load(),
		"busySeconds": float64(busyNano.Load()/1e6) / 1000, "cpuSlowdown": Factor}
}
