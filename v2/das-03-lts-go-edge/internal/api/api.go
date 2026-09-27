// Package api serves the REST side of the das-v1 contract (contract/openapi.yaml)
// from the in-memory state, mounts the WebSocket channels, and forwards
// everything it does not implement to the legacy backend (strangler fig).
//
// Nothing here parses or serialises large data per request: snapshots, deltas
// and spectrum variants are built once per change and shared, so a request
// costs a map lookup and a socket write, whatever else is going on.
package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"dasedge/internal/configstore"
	"dasedge/internal/heavy"
	"dasedge/internal/spectrum"
	"dasedge/internal/telemetry"
)

// Options wire the API to the rest of the server.
type Options struct {
	Server, Version, BootID string

	Store    *telemetry.Store
	Spectrum *spectrum.Manager
	// Configs serves /api/nodes/{id}/config locally; nil forwards it to Legacy.
	Configs *configstore.Store
	// Legacy receives every /api request not handled here; nil answers 404.
	Legacy *Legacy
	// Static serves everything outside /api (the web UI).
	Static http.Handler
	// Delegate names endpoint groups the edge does NOT serve, so they reach the
	// legacy backend (strangler fig): "volatile", "spectrum", "ws". Configuration
	// is delegated by leaving Configs nil.
	Delegate []string
	// Auth protects the endpoints served here (nil: no authentication, PoC).
	// AuthExempt lists route names that stay open (default: heartbeat, capabilities).
	Auth       *ForwardAuth
	AuthExempt []string

	TelemetryWS, SpectrumWS http.Handler

	DefaultPoints int
	SweepWait     time.Duration // longest wait for a sweep before answering 503
	GzipMinBytes  int           // deltas smaller than this are sent uncompressed

	// Extra contributes sections to /api/metrics (hubs, ingest, ...).
	Extra func() map[string]any
	Log   *slog.Logger
}

// API is the HTTP handler of the edge server.
type API struct {
	o       Options
	mux     *http.ServeMux
	started time.Time
	lag     *LagMonitor
	routes  routes
}

// New builds the handler.
func New(o Options) *API {
	if o.SweepWait <= 0 {
		o.SweepWait = 10 * time.Second
	}
	if o.GzipMinBytes <= 0 {
		o.GzipMinBytes = 16 << 10
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.AuthExempt == nil {
		o.AuthExempt = []string{"heartbeat", "capabilities"}
	}
	a := &API{o: o, mux: http.NewServeMux(), started: time.Now(), lag: NewLagMonitor(100*time.Millisecond, 300)}
	open := func(pattern, name string, h http.HandlerFunc) { a.mux.Handle(pattern, counted(a.routes.add(name), h)) }
	handle := func(pattern, name string, h http.HandlerFunc) {
		if o.Auth != nil && !slices.Contains(o.AuthExempt, name) {
			h = o.Auth.Wrap(h)
		}
		open(pattern, name, h)
	}
	handle("GET /api/heartbeat", "heartbeat", a.heartbeat)
	handle("GET /api/capabilities", "capabilities", a.capabilities)
	if a.serves("volatile") {
		handle("GET /api/volatile-data", "volatile", a.volatile)
	}
	if a.serves("spectrum") {
		handle("GET /api/spectrum", "spectrum-legacy", a.spectrumLegacy)
		handle("GET /api/spectrum/latest", "spectrum-latest", a.spectrumLatest)
	}
	handle("GET /api/metrics", "metrics", a.metrics)
	if o.TelemetryWS != nil && a.serves("ws") {
		handle("GET /api/ws/telemetry", "ws-telemetry", o.TelemetryWS.ServeHTTP)
	}
	if o.SpectrumWS != nil && a.serves("ws") {
		handle("GET /api/ws/spectrum", "ws-spectrum", o.SpectrumWS.ServeHTTP)
	}
	if o.Configs != nil {
		handle("GET /api/nodes/{id}/config", "config-get", a.configGet)
		handle("PUT /api/nodes/{id}/config", "config-put", a.configPut)
	}
	if o.Legacy != nil {
		open("/api/", "legacy-proxy", o.Legacy.ServeHTTP) // the legacy backend enforces its own authentication
	} else {
		open("/api/", "not-found", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
		})
	}
	if o.Static != nil {
		a.mux.Handle("/", o.Static)
	}
	return a
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// serves reports whether the edge itself serves an endpoint group.
func (a *API) serves(group string) bool {
	if group == "config" {
		return a.o.Configs != nil
	}
	return !slices.Contains(a.o.Delegate, group)
}

// Lag exposes the scheduling-lag monitor (watchdog, metrics).
func (a *API) Lag() *LagMonitor { return a.lag }

// ---------------------------------------------------------------- core

func (a *API) heartbeat(w http.ResponseWriter, r *http.Request) {
	lag := a.lag.Snapshot()
	services := map[string]string{}
	degraded := lag.P99Ms > 250
	if a.o.Legacy != nil {
		st := a.o.Legacy.Status()
		services["legacy"] = st
		degraded = degraded || (st != "ok" && st != "unknown")
	}
	status := "ok"
	if degraded {
		status = "degraded"
	}
	rt := ReadRuntime()
	writeJSON(w, r, http.StatusOK, map[string]any{
		"status":   status,
		"server":   a.o.Server,
		"version":  a.o.Version,
		"bootId":   a.o.BootID,
		"time":     time.Now().UnixMilli(),
		"uptimeS":  round1(time.Since(a.started).Seconds()),
		"runtime":  map[string]any{"goroutines": rt.Goroutines, "gomaxprocs": rt.GoMaxProcs, "heapMB": rt.HeapMB, "rssMB": rt.RSSMB, "schedLagP50Ms": lag.P50Ms, "schedLagP99Ms": lag.P99Ms, "schedLagMaxMs": lag.MaxMs},
		"services": services,
	}, "no-store")
}

// featureGroups maps each opt-in feature to the endpoint group that provides it.
var featureGroups = map[string]string{
	"volatileDelta": "volatile", "spectrumLatest": "spectrum", "spectrumBinary": "spectrum", "spectrumDecimation": "spectrum",
	"spectrumLongPoll": "spectrum", "configOptimisticLocking": "config", "wsTelemetry": "ws", "wsSpectrum": "ws",
}

// capabilities advertises what the edge serves, and for delegated groups what
// the legacy backend advertises itself (a shipped release without
// /api/capabilities advertises nothing), so clients never opt into a feature
// the answering process does not have.
func (a *API) capabilities(w http.ResponseWriter, r *http.Request) {
	var legacy map[string]bool
	if a.o.Legacy != nil {
		legacy = a.o.Legacy.Features()
	}
	features := map[string]bool{}
	for f, g := range featureGroups {
		if a.serves(g) {
			features[f] = true
		} else {
			features[f] = legacy[f]
		}
	}
	features["wsTelemetry"] = features["wsTelemetry"] && (a.o.TelemetryWS != nil || !a.serves("ws"))
	features["wsSpectrum"] = features["wsSpectrum"] && (a.o.SpectrumWS != nil || !a.serves("ws"))
	features["etag"] = a.serves("volatile") || a.serves("spectrum") || a.serves("config") || legacy["etag"]
	writeJSON(w, r, http.StatusOK, map[string]any{
		"api":      "das-v1",
		"server":   a.o.Server,
		"version":  a.o.Version,
		"features": features,
		"limits":   map[string]any{"maxSpectrumPoints": 200001, "maxDecimatedPoints": 1 << 16, "recommendedPollMs": map[string]int{"heartbeat": 2000, "volatileData": 2000}},
	}, "no-cache")
}

// ---------------------------------------------------------------- volatile_data

func (a *API) volatile(w http.ResponseWriter, r *http.Request) {
	gz := acceptsEncoding(r, "gzip")
	h := w.Header()
	if s := r.URL.Query().Get("since"); s != "" {
		if since, err := strconv.ParseUint(s, 10, 64); err == nil {
			if body, rev, ok := a.o.Store.DeltaJSON(since); ok {
				h.Set("Cache-Control", "no-store")
				h.Set("X-Revision", strconv.FormatUint(rev, 10))
				if gz && len(body) >= a.o.GzipMinBytes {
					body = heavy.Do(func() []byte { return spectrum.Gzip(body) })
					h.Set("Content-Encoding", "gzip")
					h.Set("Vary", "Accept-Encoding")
				}
				writeBody(w, r, http.StatusOK, jsonType, body)
				return
			}
		}
		// Unknown, expired, future or previous-boot revision: full snapshot below.
	}
	v := a.o.Store.View()
	etag := a.o.Store.ETag(v, gz)
	h.Set("Cache-Control", "no-cache")
	h.Set("Vary", "Accept-Encoding")
	h.Set("X-Revision", strconv.FormatUint(v.Rev, 10))
	if ifNoneMatch(r, etag) {
		notModified(w, etag)
		return
	}
	h.Set("ETag", etag)
	body := v.SnapshotJSON()
	if gz {
		body = v.SnapshotGzip()
		h.Set("Content-Encoding", "gzip")
	}
	writeBody(w, r, http.StatusOK, jsonType, body)
}

// ---------------------------------------------------------------- spectrum

func (a *API) session(w http.ResponseWriter, r *http.Request) *spectrum.Session {
	q := r.URL.Query()
	p, err := spectrum.ParseParams(q.Get("nodeId"), q.Get("port"), q.Get("startHz"), q.Get("stopHz"), q.Get("points"), a.o.DefaultPoints)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return nil
	}
	s, err := a.o.Spectrum.Session(p)
	if err != nil {
		w.Header().Set("Retry-After", "2")
		writeError(w, r, http.StatusServiceUnavailable, "TOO_MANY_SESSIONS", err.Error())
		return nil
	}
	return s
}

func sweepHeaders(w http.ResponseWriter, v *spectrum.Variant, sw *spectrum.Sweep, cacheControl, vary string) {
	h := w.Header()
	h.Set("Cache-Control", cacheControl)
	h.Set("Vary", vary)
	h.Set("ETag", v.ETag)
	h.Set("X-Sweep-Id", strconv.FormatUint(uint64(sw.ID), 10))
	if v.Gzip {
		h.Set("Content-Encoding", "gzip")
	}
}

// spectrumLegacy keeps the shipped endpoint's shape and semantics (every poll
// returns a sweep completed after the request arrived), but all clients share
// one sweep and its body is encoded once per sweep, not once per request.
func (a *API) spectrumLegacy(w http.ResponseWriter, r *http.Request) {
	s := a.session(w, r)
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.o.SweepWait)
	sw, err := s.Next(ctx)
	cancel()
	if err != nil {
		if r.Context().Err() != nil {
			return // client gave up
		}
		if sw = s.Latest(); sw == nil { // better stale than an error for the shipped UI
			w.Header().Set("Retry-After", "1")
			writeError(w, r, http.StatusServiceUnavailable, "SWEEP_TIMEOUT", "the analyzer did not complete a sweep in time")
			return
		}
	}
	v := sw.Variant(a.o.Spectrum, spectrum.KindLegacy, 0, acceptsEncoding(r, "gzip"))
	sweepHeaders(w, v, sw, "no-store", "Accept-Encoding")
	writeBody(w, r, http.StatusOK, v.ContentType, v.Body)
}

// spectrumLatest: newest sweep now, conditional (ETag), compact or binary,
// decimated to the display width on request, optional long-poll (waitMs).
func (a *API) spectrumLatest(w http.ResponseWriter, r *http.Request) {
	s := a.session(w, r)
	if s == nil {
		return
	}
	q := r.URL.Query()
	binary := acceptsMediaType(r, spectrum.ContentType) || q.Get("format") == "binary"
	kind := spectrum.KindJSON
	if binary {
		kind = spectrum.KindI16
		if q.Get("encoding") == "f32" {
			kind = spectrum.KindF32
		}
	}
	mp, _ := strconv.Atoi(q.Get("maxPoints"))
	mp = spectrum.NormaliseMaxPoints(mp)
	gz := !binary && acceptsEncoding(r, "gzip") // binary spectrum is mostly noise: gzip saves little for a lot of CPU
	waitMs, _ := strconv.Atoi(q.Get("waitMs"))
	waitMs = max(0, min(10000, waitMs))

	ctx, cancel := context.WithTimeout(r.Context(), a.o.SweepWait)
	sw, err := s.LatestOrNext(ctx)
	cancel()
	if err != nil {
		if r.Context().Err() == nil {
			w.Header().Set("Retry-After", "1")
			writeError(w, r, http.StatusServiceUnavailable, "SWEEP_TIMEOUT", "the analyzer did not complete a sweep in time")
		}
		return
	}
	if tag := sw.Tag(kind, mp, gz); ifNoneMatch(r, tag) {
		// The client already has this sweep: wait for the next one if asked, else 304 without building anything.
		if waitMs > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), time.Duration(waitMs)*time.Millisecond)
			next, err := s.After(ctx, sw.ID)
			cancel()
			if err == nil {
				sw = next
			} else if r.Context().Err() != nil {
				return
			}
		}
		if sw.Tag(kind, mp, gz) == tag {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Vary", "Accept, Accept-Encoding")
			w.Header().Set("X-Sweep-Id", strconv.FormatUint(uint64(sw.ID), 10))
			notModified(w, tag)
			return
		}
	}
	v := sw.Variant(a.o.Spectrum, kind, mp, gz)
	sweepHeaders(w, v, sw, "no-cache", "Accept, Accept-Encoding")
	writeBody(w, r, http.StatusOK, v.ContentType, v.Body)
}

// ---------------------------------------------------------------- configuration

func nodeID(r *http.Request) int {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		return -1
	}
	return id
}

func (a *API) configGet(w http.ResponseWriter, r *http.Request) {
	id := nodeID(r)
	env, etag, ok := a.o.Configs.Get(id)
	if !ok {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "unknown node "+r.PathValue("id"))
		return
	}
	if ifNoneMatch(r, etag) {
		w.Header().Set("Cache-Control", "no-cache")
		notModified(w, etag)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, r, http.StatusOK, env, "no-cache")
}

func (a *API) configPut(w http.ResponseWriter, r *http.Request) {
	id := nodeID(r)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "configuration body is limited to 256 KiB")
			return
		}
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "could not read body")
		return
	}
	c, errs, err := configstore.Decode(body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON")
		return
	}
	if errs == nil {
		errs = []string{} // validated, nothing wrong
	}
	res := a.o.Configs.Put(id, c, r.Header.Get("If-Match"), errs)
	switch res.Status {
	case http.StatusOK:
		a.o.Log.Info("config_changed", "nodeId", id, "version", res.Env.Version)
		w.Header().Set("ETag", res.ETag)
		writeJSON(w, r, http.StatusOK, res.Env, "no-store")
	case http.StatusNotFound:
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "unknown node "+r.PathValue("id"))
	case http.StatusPreconditionFailed:
		writeJSON(w, r, http.StatusPreconditionFailed, map[string]any{
			"error":   "PRECONDITION_FAILED",
			"message": "node " + strconv.Itoa(id) + " was changed by someone else (now version " + strconv.Itoa(res.Env.Version) + "); reload and re-apply",
			"current": res.Env,
		}, "no-store")
	default:
		writeJSON(w, r, http.StatusUnprocessableEntity, map[string]any{"error": "VALIDATION_FAILED", "errors": res.Errors}, "no-store")
	}
}

// ---------------------------------------------------------------- metrics

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	st := a.o.Store
	v := st.View()
	edge := map[string]any{
		"server": a.o.Server, "version": a.o.Version, "bootId": a.o.BootID, "uptimeS": round1(time.Since(a.started).Seconds()),
		"runtime": ReadRuntime(), "lag": a.lag.Snapshot(), "routes": a.routes.snapshot(), "cpuSlowdown": heavy.Factor,
	}
	if a.o.Auth != nil {
		edge["auth"] = a.o.Auth.Stats()
	}
	if a.o.Legacy != nil {
		edge["legacy"] = map[string]any{"target": a.o.Legacy.Target(), "status": a.o.Legacy.Status(), "requests": a.o.Legacy.Requests.Load(), "failures": a.o.Legacy.Failures.Load()}
	}
	out := map[string]any{
		"edge":      edge,
		"telemetry": map[string]any{"rev": v.Rev, "nodes": v.NodeCount(), "reports": st.Reports.Load(), "unchanged": st.Unchanged.Load(), "publishes": st.Publishes.Load()},
		"spectrum":  a.o.Spectrum.Stats(),
	}
	if a.o.Extra != nil {
		for k, x := range a.o.Extra() {
			out[k] = x
		}
	}
	if r.URL.Query().Get("format") == "prom" {
		writeBody(w, r, http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", promText(out))
		return
	}
	writeJSON(w, r, http.StatusOK, out, "no-store")
}
