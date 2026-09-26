//go:build linux

package heavy

import "syscall"

// setThreadNice renices the calling thread only (Linux applies PRIO_PROCESS
// with a thread id to that thread). Raising the nice value needs no privilege.
func setThreadNice(n int) int {
	if n == 0 {
		return 0
	}
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, syscall.Gettid(), n); err != nil {
		return 0
	}
	return n
}
