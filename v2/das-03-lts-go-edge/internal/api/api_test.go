package api

import (
	"bytes"
	"compress/gzip"
	"dasedge/internal/heavy"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"dasedge/internal/configstore"
	"dasedge/internal/spectrum"
	"dasedge/internal/static"
	"dasedge/internal/telemetry"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	srv   *httptest.Server
	store *telemetry.Store
	sim   *telemetry.Simulator
}

func newEnv(t *testing.T, mutate func(*Options)) *env {
	t.Helper()
	store := telemetry.NewStore("boot1", true)
	sim := telemetry.NewSimulator(40, time.Second, 2)
	for _, r := range sim.Initial() {
		store.Ingest(r)
	}
	store.Publish()
	mgr := spectrum.NewManager(spectrum.NewSimHardware(30*time.Millisecond), "boot1", 5*time.Second, 0, 8)
	t.Cleanup(mgr.Close)
	o := Options{Server: "das-edge-go", Version: "test", BootID: "boot1", Store: store, Spectrum: mgr,
		Configs: configstore.New("boot1", 40), DefaultPoints: 2001, SweepWait: 3 * time.Second, Log: quiet,
		Static: static.New(fstest.MapFS{
			"index.html":       {Data: []byte("<!doctype html><title>app</title>")},
			"main-7XQ2ZK5D.js": {Data: bytes.Repeat([]byte("console.log('das');\n"), 200)},
		})}
	if mutate != nil {
		mutate(&o)
	}
	srv := httptest.NewServer(New(o))
	t.Cleanup(srv.Close)
	return &env{srv: srv, store: store, sim: sim}
}

func (e *env) publish() {
	for {
		e.sim.ForceAll()
		for _, r := range e.sim.Tick(time.Now()) {
			e.store.Ingest(r)
		}
		if e.store.Publish() != nil {
			return
		}
	}
}

type result struct {
	status int
	h      http.Header
	body   []byte
}

func (r result) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("not JSON (%v): %.200s", err, r.body)
	}
	return m
}

// do sends a request without Go's transparent gzip handling, so encodings are visible.
func (e *env) do(t *testing.T, method, path string, body string, hdr ...string) result {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Accept-Encoding", "identity")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		b, _ = io.ReadAll(zr)
	}
	return result{resp.StatusCode, resp.Header, b}
}

func TestHeartbeatAndCapabilities(t *testing.T) {
	e := newEnv(t, nil)
	r := e.do(t, "GET", "/api/heartbeat", "")
	if r.status != 200 || !strings.Contains(r.h.Get("Cache-Control"), "no-store") {
		t.Fatalf("%d %v", r.status, r.h)
	}
	if hb := r.json(t); hb["status"] != "ok" || hb["server"] != "das-edge-go" {
		t.Fatalf("%v", hb)
	}
	c := e.do(t, "GET", "/api/capabilities", "").json(t)
	f := c["features"].(map[string]any)
	if c["api"] != "das-v1" || f["etag"] != true || f["wsTelemetry"] != false {
		t.Fatalf("%v", c)
	}
}

func TestVolatileDataConditionalCompressedAndDelta(t *testing.T) {
	e := newEnv(t, nil)
	plain := e.do(t, "GET", "/api/volatile-data", "")
	gz := e.do(t, "GET", "/api/volatile-data", "", "Accept-Encoding", "gzip")
	if plain.h.Get("Content-Encoding") != "" || gz.h.Get("Content-Encoding") != "gzip" || !bytes.Equal(plain.body, gz.body) {
		t.Fatal("gzip must be negotiated and carry the same document")
	}
	snap := plain.json(t)
	if int(snap["nodeCount"].(float64)) != 40 || len(snap["nodes"].(map[string]any)) != 40 {
		t.Fatalf("snapshot shape: %v", snap["nodeCount"])
	}
	tag := plain.h.Get("ETag")
	if nm := e.do(t, "GET", "/api/volatile-data", "", "If-None-Match", tag); nm.status != 304 || len(nm.body) != 0 {
		t.Fatalf("expected 304, got %d", nm.status)
	}
	if gz.h.Get("ETag") == tag {
		t.Fatal("gzip and identity representations need different ETags")
	}
	rev := strconv.FormatUint(uint64(snap["rev"].(float64)), 10)
	e.publish()
	d := e.do(t, "GET", "/api/volatile-data?since="+rev, "").json(t)
	if strconv.FormatUint(uint64(d["base"].(float64)), 10) != rev || d["changed"] == nil || d["removed"] == nil {
		t.Fatalf("delta: %v", d)
	}
	for _, since := range []string{"999999999", "abc", strconv.FormatUint(uint64(snap["rev"].(float64))+1000, 10)} {
		if full := e.do(t, "GET", "/api/volatile-data?since="+since, "").json(t); full["nodes"] == nil || full["base"] != nil {
			t.Fatalf("since=%s must return a snapshot", since)
		}
	}
	if e.do(t, "GET", "/api/volatile-data", "", "If-None-Match", tag).status != 200 {
		t.Fatal("a new revision must not match the old ETag")
	}
}

func TestSpectrumLegacyAndLatest(t *testing.T) {
	e := newEnv(t, nil)
	a := e.do(t, "GET", "/api/spectrum?nodeId=1&port=1&points=2001", "")
	var leg struct {
		SweepID uint32 `json:"sweepId"`
		Points  []struct{ Frequency, Power float64 }
	}
	if err := json.Unmarshal(a.body, &leg); err != nil || len(leg.Points) != 2001 || leg.Points[0].Frequency != 700e6 {
		t.Fatalf("legacy shape: %v (%d points)", err, len(leg.Points))
	}
	b := e.do(t, "GET", "/api/spectrum?nodeId=1&port=1&points=2001", "")
	var leg2 struct {
		SweepID uint32 `json:"sweepId"`
	}
	_ = json.Unmarshal(b.body, &leg2)
	if leg2.SweepID <= leg.SweepID {
		t.Fatal("each legacy poll must return a newer sweep")
	}

	j := e.do(t, "GET", "/api/spectrum/latest?nodeId=1&port=1&points=2001", "").json(t)
	if int(j["count"].(float64)) != 2001 || len(j["powerDbm"].([]any)) != 2001 {
		t.Fatalf("compact: %v", j["count"])
	}
	bin := e.do(t, "GET", "/api/spectrum/latest?nodeId=1&port=1&points=2001&maxPoints=256", "", "Accept", spectrum.ContentType)
	m, p, err := spectrum.DecodeFrame(bin.body)
	if err != nil || bin.h.Get("Content-Type") != spectrum.ContentType || !m.Decimated || len(p) > 256 {
		t.Fatalf("binary: %v %+v %d", err, m, len(p))
	}
	tag := bin.h.Get("ETag")
	if nm := e.do(t, "GET", "/api/spectrum/latest?nodeId=1&port=1&points=2001&maxPoints=256", "", "Accept", spectrum.ContentType, "If-None-Match", tag); nm.status != 304 && nm.h.Get("X-Sweep-Id") == bin.h.Get("X-Sweep-Id") {
		t.Fatalf("same sweep must be 304, got %d", nm.status)
	}
	start := time.Now()
	lp := e.do(t, "GET", "/api/spectrum/latest?nodeId=1&port=1&points=2001&maxPoints=256&waitMs=3000", "", "Accept", spectrum.ContentType, "If-None-Match", tag)
	m2, _, err := spectrum.DecodeFrame(lp.body)
	if lp.status != 200 || err != nil || m2.SweepID <= m.SweepID {
		t.Fatalf("long-poll: %d %v sweep %d after %d", lp.status, err, m2.SweepID, m.SweepID)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("long-poll must return as soon as the next sweep completes")
	}
	if bad := e.do(t, "GET", "/api/spectrum/latest?startHz=2e9&stopHz=1e9", ""); bad.status != 400 {
		t.Fatalf("invalid range: %d", bad.status)
	}
}

func TestConfigOptimisticLocking(t *testing.T) {
	e := newEnv(t, nil)
	g := e.do(t, "GET", "/api/nodes/7/config", "")
	tag := g.h.Get("ETag")
	env := g.json(t)
	cfg := env["config"].(map[string]any)
	cfg["notes"] = "changed"
	body, _ := json.Marshal(cfg)
	ok := e.do(t, "PUT", "/api/nodes/7/config", string(body), "Content-Type", "application/json", "If-Match", tag)
	if ok.status != 200 || ok.json(t)["version"].(float64) != env["version"].(float64)+1 || ok.h.Get("ETag") == tag {
		t.Fatalf("PUT: %d %s", ok.status, ok.body)
	}
	stale := e.do(t, "PUT", "/api/nodes/7/config", string(body), "If-Match", tag)
	if stale.status != 412 || stale.json(t)["current"] == nil {
		t.Fatalf("stale: %d", stale.status)
	}
	cfg["dlGainDb"] = 999
	delete(cfg, "notes")
	body, _ = json.Marshal(cfg)
	bad := e.do(t, "PUT", "/api/nodes/7/config", string(body))
	errs := bad.json(t)["errors"].([]any)
	if bad.status != 422 || len(errs) != 2 {
		t.Fatalf("invalid: %d %v", bad.status, errs)
	}
	if r := e.do(t, "PUT", "/api/nodes/7/config", "{not json"); r.status != 400 {
		t.Fatalf("syntax: %d", r.status)
	}
	if r := e.do(t, "PUT", "/api/nodes/7/config", `{"name":5}`); r.status != 422 {
		t.Fatalf("wrong type: %d", r.status)
	}
	if r := e.do(t, "GET", "/api/nodes/99999/config", ""); r.status != 404 {
		t.Fatalf("unknown node: %d", r.status)
	}
}

func TestStranglerProxy(t *testing.T) {
	var gotHost, gotPath string
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"from":"legacy"}`))
	}))
	defer legacy.Close()
	l, err := NewLegacy(legacy.URL, quiet)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(o *Options) { o.Legacy, o.Configs = l, nil })
	r := e.do(t, "GET", "/api/alarms/history?from=1", "")
	if r.status != 200 || r.json(t)["from"] != "legacy" || gotPath != "/api/alarms/history" {
		t.Fatalf("not forwarded: %d %s", r.status, r.body)
	}
	if gotHost != strings.TrimPrefix(e.srv.URL, "http://") {
		t.Fatalf("legacy must see the browser's Host, got %q", gotHost)
	}
	if r := e.do(t, "PUT", "/api/nodes/3/config", "{}"); gotPath != "/api/nodes/3/config" || r.status != 200 {
		t.Fatal("configuration must go to the legacy backend when the edge does not own it")
	}
	if r := e.do(t, "GET", "/api/heartbeat", ""); r.json(t)["server"] != "das-edge-go" {
		t.Fatal("implemented endpoints must stay on the edge")
	}
	legacy.Close()
	if r := e.do(t, "GET", "/api/alarms/history", ""); r.status != 502 || r.json(t)["error"] != "UPSTREAM_UNAVAILABLE" {
		t.Fatalf("upstream down: %d %s", r.status, r.body)
	}
	for i := 0; i < 3; i++ {
		l.Probe(t.Context())
	}
	hb := e.do(t, "GET", "/api/heartbeat", "").json(t)
	if hb["status"] != "degraded" || hb["services"].(map[string]any)["legacy"] != "down" {
		t.Fatalf("heartbeat must report the legacy backend as down: %v", hb)
	}
}

func TestForwardAuthUsesTheLegacySession(t *testing.T) {
	var checks int
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/session" {
			checks++
			if strings.Contains(r.Header.Get("Cookie"), "session=good") {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"from":"legacy"}`))
	}))
	defer legacy.Close()
	l, _ := NewLegacy(legacy.URL, quiet)
	ws := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusSwitchingProtocols) })
	e := newEnv(t, func(o *Options) {
		o.Legacy, o.Auth, o.TelemetryWS = l, NewForwardAuth(l, "/api/session", time.Minute), ws
	})
	if r := e.do(t, "GET", "/api/volatile-data", ""); r.status != 401 || r.json(t)["error"] != "UNAUTHORIZED" {
		t.Fatalf("no session: %d", r.status)
	}
	if r := e.do(t, "GET", "/api/volatile-data", "", "Cookie", "session=bad"); r.status != 401 {
		t.Fatalf("bad session: %d", r.status)
	}
	before := checks
	for i := 0; i < 3; i++ {
		if r := e.do(t, "GET", "/api/volatile-data", "", "Cookie", "session=good"); r.status != 200 {
			t.Fatalf("good session: %d", r.status)
		}
	}
	if checks-before != 1 {
		t.Fatalf("%d checks for one session: answers must be cached", checks-before)
	}
	if r := e.do(t, "GET", "/api/heartbeat", ""); r.status != 200 {
		t.Fatal("the heartbeat must stay open: a logged-out UI still needs to know the server is alive")
	}
	if r := e.do(t, "GET", "/api/ws/telemetry", ""); r.status != 401 {
		t.Fatalf("websocket upgrade without session: %d", r.status)
	}
	legacy.Close()
	if r := e.do(t, "GET", "/api/spectrum/latest", "", "Cookie", "session=other"); r.status != 401 {
		t.Fatalf("fail closed when the session cannot be checked: %d", r.status)
	}
}

func TestDelegatedGroupsGoToLegacyAndCapabilitiesFollow(t *testing.T) {
	for _, shipped := range []bool{true, false} {
		legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/capabilities":
				if shipped {
					http.NotFound(w, r) // the shipped release has no capabilities endpoint
					return
				}
				_, _ = w.Write([]byte(`{"api":"das-v1","features":{"etag":true,"spectrumLatest":true,"spectrumBinary":true,"configOptimisticLocking":true}}`))
			case "/api/heartbeat":
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			default:
				_, _ = w.Write([]byte(`{"from":"legacy"}`))
			}
		}))
		l, _ := NewLegacy(legacy.URL, quiet)
		l.Probe(t.Context())
		e := newEnv(t, func(o *Options) { o.Legacy, o.Configs, o.Delegate = l, nil, []string{"spectrum", "ws"} })
		if r := e.do(t, "GET", "/api/spectrum/latest", ""); r.json(t)["from"] != "legacy" {
			t.Fatal("a delegated group must reach the legacy backend")
		}
		if r := e.do(t, "GET", "/api/volatile-data", ""); r.json(t)["nodes"] == nil {
			t.Fatal("groups that are not delegated stay on the edge")
		}
		f := e.do(t, "GET", "/api/capabilities", "").json(t)["features"].(map[string]any)
		if f["volatileDelta"] != true || f["wsTelemetry"] != false || f["spectrumLatest"] != !shipped || f["configOptimisticLocking"] != !shipped || f["spectrumLongPoll"] != false {
			t.Fatalf("shipped=%v: capabilities must describe whoever answers: %v", shipped, f)
		}
		legacy.Close()
	}
}

func TestStaticUI(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.do(t, "GET", "/", ""); r.status != 200 || !strings.Contains(string(r.body), "<title>app") || r.h.Get("Cache-Control") != "no-cache" {
		t.Fatalf("index: %d %v", r.status, r.h)
	}
	if r := e.do(t, "GET", "/nodes/12", ""); r.status != 200 || !strings.Contains(string(r.body), "<title>app") {
		t.Fatal("client-side routes must return index.html")
	}
	if r := e.do(t, "GET", "/missing.js", ""); r.status != 404 {
		t.Fatalf("missing asset: %d", r.status)
	}
	js := e.do(t, "GET", "/main-7XQ2ZK5D.js", "", "Accept-Encoding", "gzip")
	if js.h.Get("Content-Encoding") != "gzip" || !strings.Contains(js.h.Get("Cache-Control"), "immutable") || len(js.body) != 4000 {
		t.Fatalf("asset: %v (%d bytes)", js.h, len(js.body))
	}
	if r := e.do(t, "GET", "/main-7XQ2ZK5D.js", "", "Accept-Encoding", "gzip", "If-None-Match", js.h.Get("ETag")); r.status != 304 {
		t.Fatalf("revalidation: %d", r.status)
	}
	if r := e.do(t, "GET", "/api/nope", ""); r.status != 404 || r.json(t)["error"] != "NOT_FOUND" {
		t.Fatal("unknown API paths are JSON 404s, not the app")
	}
}

func TestMetrics(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Extra = func() map[string]any { return map[string]any{"ingest": map[string]int{"streams": 3}} }
	})
	e.do(t, "GET", "/api/heartbeat", "")
	m := e.do(t, "GET", "/api/metrics", "").json(t)
	if m["telemetry"].(map[string]any)["nodes"].(float64) != 40 || m["ingest"] == nil {
		t.Fatalf("%v", m)
	}
	p := string(e.do(t, "GET", "/api/metrics?format=prom", "").body)
	for _, want := range []string{`das_http_requests_total{route="heartbeat"} 1`, "das_edge_runtime_goroutines ", "das_ingest_streams 3", "das_telemetry_nodes 40"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prometheus output lacks %q", want)
		}
	}
}

func TestAcceptEncodingRules(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	for h, want := range map[string]bool{"gzip": true, "gzip;q=0": false, "*": true, "br, *;q=0.1": true, "gzip;q=0, *": false, "identity": false, "GZIP;q=0.5": true} {
		r.Header.Set("Accept-Encoding", h)
		if acceptsEncoding(r, "gzip") != want {
			t.Errorf("Accept-Encoding %q: want %v", h, want)
		}
	}
}

// Run every test with the heavy-work pool active (one worker): a nested
// heavy.Do would deadlock and time the test out.
func TestMain(m *testing.M) {
	heavy.Start(1, 0)
	os.Exit(m.Run())
}
