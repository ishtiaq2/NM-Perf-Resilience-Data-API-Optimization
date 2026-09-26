package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const jsonType = "application/json; charset=utf-8"

type qItem struct {
	value string
	q     float64
}

// parseQList parses Accept / Accept-Encoding values into lower-cased items with their q.
func parseQList(values []string) []qItem {
	var out []qItem
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			seg := strings.Split(part, ";")
			name := strings.ToLower(strings.TrimSpace(seg[0]))
			if name == "" {
				continue
			}
			q := 1.0
			for _, p := range seg[1:] {
				k, val, ok := strings.Cut(strings.TrimSpace(p), "=")
				if ok && strings.TrimSpace(k) == "q" {
					if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
						q = f
					} else {
						q = 0
					}
				}
			}
			out = append(out, qItem{name, q})
		}
	}
	return out
}

// acceptsEncoding: an explicit entry for enc decides, otherwise "*" (RFC 9110 12.5.3).
func acceptsEncoding(r *http.Request, enc string) bool {
	star := -1.0
	for _, it := range parseQList(r.Header.Values("Accept-Encoding")) {
		if it.value == enc {
			return it.q > 0
		}
		if it.value == "*" {
			star = it.q
		}
	}
	return star > 0
}

// acceptsMediaType is true only when the client explicitly asks for the type (opt-in formats).
func acceptsMediaType(r *http.Request, mediaType string) bool {
	for _, it := range parseQList(r.Header.Values("Accept")) {
		if it.value == mediaType {
			return it.q > 0
		}
	}
	return false
}

// ifNoneMatch uses weak comparison (RFC 9110 13.1.2).
func ifNoneMatch(r *http.Request, etag string) bool {
	h := r.Header.Get("If-None-Match")
	if h == "" || etag == "" {
		return false
	}
	if strings.TrimSpace(h) == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, t := range strings.Split(h, ",") {
		if strings.TrimPrefix(strings.TrimSpace(t), "W/") == want {
			return true
		}
	}
	return false
}

func writeBody(w http.ResponseWriter, r *http.Request, status int, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any, cacheControl string) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"INTERNAL","message":"encoding failed"}`)
	}
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	writeBody(w, r, status, jsonType, body)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, r, status, map[string]string{"error": code, "message": msg}, "no-store")
}

func notModified(w http.ResponseWriter, etag string) {
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusNotModified)
}

// ------------------------------------------------------------ route metrics

type routeCounter struct {
	name                                 string
	requests, notModified, errors, bytes atomic.Int64
}

type routes struct {
	mu   sync.Mutex
	list []*routeCounter
}

func (rs *routes) add(name string) *routeCounter {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, c := range rs.list {
		if c.name == name {
			return c
		}
	}
	c := &routeCounter{name: name}
	rs.list = append(rs.list, c)
	return c
}

// RouteStats is one route's counters.
type RouteStats struct {
	Requests    int64 `json:"requests"`
	NotModified int64 `json:"notModified"`
	Errors      int64 `json:"errors"`
	Bytes       int64 `json:"bytes"`
}

func (rs *routes) snapshot() map[string]RouteStats {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make(map[string]RouteStats, len(rs.list))
	for _, c := range rs.list {
		out[c.name] = RouteStats{c.requests.Load(), c.notModified.Load(), c.errors.Load(), c.bytes.Load()}
	}
	return out
}

// recorder captures status and body size. Unwrap lets http.ResponseController
// (WebSocket hijack, reverse proxy flush) reach the real connection.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (rec *recorder) WriteHeader(code int) {
	if rec.status == 0 {
		rec.status = code
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += int64(n)
	return n, err
}

func (rec *recorder) Flush() { _ = http.NewResponseController(rec.ResponseWriter).Flush() }

func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

func counted(c *routeCounter, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		h(rec, r)
		c.requests.Add(1)
		c.bytes.Add(rec.bytes)
		switch {
		case rec.status == http.StatusNotModified:
			c.notModified.Add(1)
		case rec.status >= 500:
			c.errors.Add(1)
		}
	}
}
