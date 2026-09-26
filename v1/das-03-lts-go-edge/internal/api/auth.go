package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ForwardAuth keeps the shipped release's authentication for the endpoints the
// edge serves itself, without reimplementing it: like nginx auth_request, it
// asks the legacy backend (GET CheckPath with the request's Cookie and
// Authorization headers) and allows the request on a 2xx answer. Answers are
// cached per credential (TTL for allow, a few seconds for deny), so the backend
// sees about one check per session and TTL. It fails closed: when the backend
// cannot answer, protected endpoints return 401 (the heartbeat stays exempt).
type ForwardAuth struct {
	legacy    *Legacy
	checkPath string
	ttl       time.Duration

	mu    sync.Mutex
	cache map[[32]byte]authEntry

	Checks, Allowed, Denied, Failures atomic.Int64
}

type authEntry struct {
	ok    bool
	until time.Time
}

// NewForwardAuth checks credentials against legacy's checkPath (e.g. "/api/session").
func NewForwardAuth(legacy *Legacy, checkPath string, ttl time.Duration) *ForwardAuth {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &ForwardAuth{legacy: legacy, checkPath: checkPath, ttl: ttl, cache: map[[32]byte]authEntry{}}
}

// Wrap protects h.
func (f *ForwardAuth) Wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if f.allowed(r) {
			f.Allowed.Add(1)
			h(w, r)
			return
		}
		f.Denied.Add(1)
		writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "login required")
	}
}

func (f *ForwardAuth) allowed(r *http.Request) bool {
	cookie, authz := r.Header.Get("Cookie"), r.Header.Get("Authorization")
	key := sha256.Sum256([]byte(cookie + "\x00" + authz)) // credentials are never kept in clear
	now := time.Now()
	f.mu.Lock()
	e, ok := f.cache[key]
	f.mu.Unlock()
	if ok && now.Before(e.until) {
		return e.ok
	}
	f.Checks.Add(1)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.legacy.base.JoinPath(f.checkPath).String(), nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	req.Header.Set("X-Forwarded-Host", r.Host)
	resp, err := f.legacy.client.Do(req)
	if err != nil {
		f.Failures.Add(1)
		return false // fail closed, and do not cache: the next request asks again
	}
	resp.Body.Close()
	allow := resp.StatusCode >= 200 && resp.StatusCode < 300
	ttl := f.ttl
	if !allow {
		ttl = 5 * time.Second // a fresh login is noticed quickly
	}
	f.mu.Lock()
	if len(f.cache) > 10000 { // bounded: drop everything expired, else start over
		for k, v := range f.cache {
			if now.After(v.until) {
				delete(f.cache, k)
			}
		}
		if len(f.cache) > 10000 {
			clear(f.cache)
		}
	}
	f.cache[key] = authEntry{ok: allow, until: now.Add(ttl)}
	f.mu.Unlock()
	return allow
}

// Stats for /api/metrics.
func (f *ForwardAuth) Stats() map[string]int64 {
	f.mu.Lock()
	n := len(f.cache)
	f.mu.Unlock()
	return map[string]int64{"checks": f.Checks.Load(), "allowed": f.Allowed.Load(), "denied": f.Denied.Load(), "failures": f.Failures.Load(), "cached": int64(n)}
}
