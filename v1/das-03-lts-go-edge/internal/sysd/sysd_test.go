package sysd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func listen(t *testing.T) *net.UnixConn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.sock")
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Skip("unixgram not available:", err)
	}
	t.Cleanup(func() { c.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	return c
}

func read(t *testing.T, c *net.UnixConn) string {
	t.Helper()
	buf := make([]byte, 256)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func TestNotify(t *testing.T) {
	c := listen(t)
	if ok, err := Notify("READY=1"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if got := read(t, c); got != "READY=1" {
		t.Fatalf("got %q", got)
	}
}

func TestNotifyOutsideSystemd(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if ok, err := Notify("READY=1"); ok || err != nil {
		t.Fatal("must be a no-op without NOTIFY_SOCKET")
	}
}

func TestWatchdogPingsOnlyWhileHealthy(t *testing.T) {
	c := listen(t)
	t.Setenv("WATCHDOG_USEC", "40000") // ping every 20 ms
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))
	healthy := make(chan bool, 1)
	healthy <- true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := true
	go RunWatchdog(ctx, func(context.Context) error {
		select {
		case state = <-healthy:
		default:
		}
		if !state {
			return errors.New("wedged")
		}
		return nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := read(t, c); got != "WATCHDOG=1" {
		t.Fatalf("got %q", got)
	}
	healthy <- false
	time.Sleep(60 * time.Millisecond)
	for { // drain pings sent before the failure was noticed
		_ = c.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
		if _, err := c.Read(make([]byte, 64)); err != nil {
			break
		}
	}
	_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, err := c.Read(make([]byte, 64)); err == nil {
		t.Fatal("watchdog kept pinging although the check failed")
	}
}

func TestWatchdogDisabledForOtherPID(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "1000000")
	t.Setenv("WATCHDOG_PID", "1")
	if WatchdogInterval() != 0 {
		t.Fatal("watchdog is meant for another process")
	}
}
