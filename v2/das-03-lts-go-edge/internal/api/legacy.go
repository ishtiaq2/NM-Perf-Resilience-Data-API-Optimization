package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Legacy is the strangler-fig seam: every /api request the edge does not
// implement itself is forwarded to the existing Node.js backend, so features
// move to Go one endpoint at a time while the browser keeps one origin.
type Legacy struct {
	target  string
	base    *url.URL
	proxy   *httputil.ReverseProxy
	client  *http.Client
	log     *slog.Logger
	healthy atomic.Value // string: ok | degraded | down | unknown
	// features the legacy backend advertises in /api/capabilities (none for a shipped release)
	features atomic.Pointer[map[string]bool]

	mu     sync.Mutex
	misses int

	Requests, Failures atomic.Int64
}

// NewLegacy accepts "http://host:port" or "unix:/path/to/socket".
func NewLegacy(target string, log *slog.Logger) (*Legacy, error) {
	dialer := &net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DialContext:           dialer.DialContext,
	}
	var base *url.URL
	if path, ok := strings.CutPrefix(target, "unix:"); ok {
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		}
		base = &url.URL{Scheme: "http", Host: "legacy"}
	} else {
		u, err := url.Parse(target)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("legacy upstream %q: want http://host:port or unix:/path", target)
		}
		base = u
	}
	l := &Legacy{target: target, base: base, log: log, client: &http.Client{Transport: tr, Timeout: time.Second}}
	l.healthy.Store("unknown")
	l.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			pr.Out.Host = pr.In.Host // the legacy app keeps seeing the browser's Host (same-origin checks, redirects)
			pr.SetXForwarded()
		},
		Transport: tr,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			l.Failures.Add(1)
			if errors.Is(err, context.Canceled) {
				return // the browser went away
			}
			log.Warn("legacy_proxy_error", "path", r.URL.Path, "error", err.Error())
			writeError(w, r, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "legacy backend unreachable")
		},
	}
	return l, nil
}

func (l *Legacy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.Requests.Add(1)
	l.proxy.ServeHTTP(w, r)
}

// Status for the heartbeat's services map.
func (l *Legacy) Status() string { return l.healthy.Load().(string) }

// Features advertised by the legacy backend (nil until the first successful probe).
func (l *Legacy) Features() map[string]bool {
	if f := l.features.Load(); f != nil {
		return *f
	}
	return nil
}

func (l *Legacy) probeFeatures(ctx context.Context) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, l.base.JoinPath("/api/capabilities").String(), nil)
	resp, err := l.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	f := map[string]bool{}
	if resp.StatusCode == http.StatusOK {
		var c struct {
			Features map[string]bool `json:"features"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&c) == nil && c.Features != nil {
			f = c.Features
		}
	}
	l.features.Store(&f) // 404: a shipped release, no opt-in features
}

// Target as configured.
func (l *Legacy) Target() string { return l.target }

// Probe checks the legacy heartbeat once (1 s budget).
func (l *Legacy) Probe(ctx context.Context) {
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, l.base.JoinPath("/api/heartbeat").String(), nil)
	resp, err := l.client.Do(req)
	ok := err == nil && resp.StatusCode == http.StatusOK
	elapsed := time.Since(start)
	if resp != nil {
		resp.Body.Close()
	}
	if ok {
		l.probeFeatures(ctx)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case ok && elapsed < 500*time.Millisecond:
		l.misses = 0
		l.healthy.Store("ok")
	case ok:
		l.misses = 0
		l.healthy.Store("degraded")
	default:
		l.misses++
		if l.misses >= 3 {
			l.healthy.Store("down")
		} else {
			l.healthy.Store("degraded")
		}
	}
}

// RunProbe probes every interval until ctx ends.
func (l *Legacy) RunProbe(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		l.Probe(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
