// Package sysd speaks the systemd notify protocol (sd_notify(3)) without cgo or
// libsystemd: READY=1 once the server listens, STATUS= lines, STOPPING=1 on
// shutdown, and the watchdog keep-alive. Outside systemd every call is a no-op.
package sysd

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"
)

// Notify sends one state string ("READY=1", "WATCHDOG=1", "STATUS=...").
// It returns false when not running under systemd (NOTIFY_SOCKET unset).
func Notify(state string) (bool, error) {
	name := os.Getenv("NOTIFY_SOCKET")
	if name == "" {
		return false, nil
	}
	if name[0] == '@' { // abstract socket namespace
		name = "\x00" + name[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}

// WatchdogInterval is how often to ping: half of WatchdogSec, or 0 when the
// watchdog is not enabled for this process.
func WatchdogInterval() time.Duration {
	us, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || us <= 0 {
		return 0
	}
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0
	}
	return time.Duration(us) * time.Microsecond / 2
}

// RunWatchdog pings systemd only while check succeeds. check should exercise the
// real serving path (for example GET /api/heartbeat over loopback), so that a
// wedged server is restarted by systemd instead of being kept alive by a timer.
func RunWatchdog(ctx context.Context, check func(context.Context) error, log *slog.Logger) {
	every := WatchdogInterval()
	if every <= 0 {
		return
	}
	log.Info("watchdog_enabled", "pingEvery", every.String())
	t := time.NewTicker(every)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, every)
		err := check(cctx)
		cancel()
		if err != nil {
			failures++
			log.Warn("watchdog_check_failed", "error", err.Error(), "consecutive", failures)
			continue // no ping: systemd restarts the service after WatchdogSec
		}
		failures = 0
		_, _ = Notify("WATCHDOG=1")
	}
}
