package heavy

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func threadNice(t *testing.T) int {
	b, err := os.ReadFile("/proc/thread-self/stat")
	if err != nil {
		t.Skip("no /proc")
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
	n, _ := strconv.Atoi(f[16]) // field 19: nice
	return n
}

func TestDoRunsOnLowPriorityThreadsAndPropagatesPanics(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("thread niceness is Linux-only")
	}
	if Start(2, 10) != 10 {
		t.Skip("cannot renice threads here")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := Do(func() int { return threadNice(t) }); got != 10 {
				t.Errorf("heavy work ran at nice %d", got)
			}
		}()
	}
	wg.Wait()
	if n := threadNice(t); n == 10 {
		t.Fatal("the caller's thread must keep its priority")
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "boom") {
			t.Fatalf("panic not propagated: %v", r)
		}
		if Stats()["jobs"].(int64) < 8 {
			t.Fatal("jobs not counted")
		}
	}()
	Do(func() int { panic("boom") })
}
