package hub

import (
	"bufio"
	"crypto/rand"
	"dasedge/internal/heavy"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"dasedge/internal/spectrum"
	"dasedge/internal/telemetry"
)

// ------------------------------------------------ minimal client (tests only)

type client struct {
	nc net.Conn
	br *bufio.Reader
}

func dial(t *testing.T, srv *httptest.Server, path string, hdr map[string]string) (*client, int) {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	nc, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	req := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	for k, v := range hdr {
		req += k + ": " + v + "\r\n"
	}
	if _, err := nc.Write([]byte(req + "\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	return &client{nc: nc, br: br}, resp.StatusCode
}

func (c *client) send(t *testing.T, s string) {
	t.Helper()
	p := []byte(s)
	h := []byte{0x81, 0x80 | byte(len(p))}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	h = append(h, mask[:]...)
	for i := range p {
		p[i] ^= mask[i&3]
	}
	if _, err := c.nc.Write(append(h, p...)); err != nil {
		t.Fatal(err)
	}
}

// next returns the next data message (text or binary), skipping pings.
func (c *client) next(t *testing.T, timeout time.Duration) (byte, []byte) {
	t.Helper()
	_ = c.nc.SetReadDeadline(time.Now().Add(timeout))
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			t.Fatalf("read: %v", err)
		}
		n := int64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			_, _ = io.ReadFull(c.br, b[:])
			n = int64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			_, _ = io.ReadFull(c.br, b[:])
			n = int64(binary.BigEndian.Uint64(b[:]))
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(c.br, p); err != nil {
			t.Fatalf("read payload: %v", err)
		}
		if op := h[0] & 0x0f; op == 1 || op == 2 {
			return op, p
		}
	}
}

type msg struct {
	Type    string                     `json:"type"`
	Rev     uint64                     `json:"rev"`
	Base    uint64                     `json:"base"`
	Nodes   map[string]json.RawMessage `json:"nodes"`
	Changed map[string]json.RawMessage `json:"changed"`
}

func (c *client) nextJSON(t *testing.T, want ...string) msg {
	t.Helper()
	for {
		op, p := c.next(t, 5*time.Second)
		if op != 1 {
			continue
		}
		var m msg
		if err := json.Unmarshal(p, &m); err != nil {
			t.Fatalf("bad JSON %s", p)
		}
		for _, w := range want {
			if m.Type == w {
				return m
			}
		}
	}
}

// ------------------------------------------------------------------ tests

func telemetryHub(t *testing.T, maxClients int) (*telemetry.Store, *telemetry.Simulator, *httptest.Server) {
	store := telemetry.NewStore("t", false)
	sim := telemetry.NewSimulator(30, time.Second, 4)
	for _, r := range sim.Initial() {
		store.Ingest(r)
	}
	store.Publish()
	h := NewTelemetry(store, "test", 5*time.Second, maxClients, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return store, sim, srv
}

func publish(store *telemetry.Store, sim *telemetry.Simulator) *telemetry.PublishEvent {
	for {
		sim.ForceAll()
		for _, r := range sim.Tick(time.Now()) {
			store.Ingest(r)
		}
		if ev := store.Publish(); ev != nil {
			return ev
		}
	}
}

func TestTelemetrySnapshotThenChainedDeltas(t *testing.T) {
	store, sim, srv := telemetryHub(t, 8)
	c, code := dial(t, srv, "/", nil)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", code)
	}
	c.nextJSON(t, "hello")
	c.send(t, `{"type":"sub"}`)
	s := c.nextJSON(t, "snapshot")
	if len(s.Nodes) != 30 || s.Rev != store.View().Rev {
		t.Fatalf("snapshot rev %d with %d nodes", s.Rev, len(s.Nodes))
	}
	rev := s.Rev
	for i := 0; i < 5; i++ {
		ev := publish(store, sim)
		d := c.nextJSON(t, "delta", "snapshot")
		if d.Type != "delta" || d.Base != rev || d.Rev != ev.Rev || len(d.Changed) != len(ev.IDs) {
			t.Fatalf("delta %d: type %s base %d rev %d (want base %d rev %d)", i, d.Type, d.Base, d.Rev, rev, ev.Rev)
		}
		rev = d.Rev
	}
}

func TestTelemetryResume(t *testing.T) {
	store, sim, srv := telemetryHub(t, 8)
	from := store.View().Rev
	for i := 0; i < 3; i++ {
		publish(store, sim)
	}
	c, _ := dial(t, srv, "/", nil)
	c.send(t, `{"type":"sub","rev":`+jsonNum(from)+`}`)
	d := c.nextJSON(t, "delta", "snapshot")
	if d.Type != "delta" || d.Base != from || d.Rev != store.View().Rev {
		t.Fatalf("resume: %s base %d rev %d", d.Type, d.Base, d.Rev)
	}
}

// slowConn makes every server write take a while (a congested or slow link).
type slowConn struct {
	net.Conn
	delay time.Duration
}

func (c slowConn) Write(b []byte) (int, error) { time.Sleep(c.delay); return c.Conn.Write(b) }

type slowListener struct {
	net.Listener
	delay time.Duration
}

func (l slowListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return slowConn{c, l.delay}, nil
}

func TestSlowClientIsConflatedNotQueued(t *testing.T) {
	store := telemetry.NewStore("t", false)
	for i := uint32(1); i <= 30; i++ {
		store.Ingest(&telemetry.Report{State: telemetry.State{ID: i, Name: "RN", Status: "online"}})
	}
	store.Publish()
	h := NewTelemetry(store, "test", 5*time.Second, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = slowListener{srv.Listener, 20 * time.Millisecond}
	srv.Start()
	defer srv.Close()
	c, _ := dial(t, srv, "/", nil)
	c.send(t, `{"type":"sub"}`)
	rev := c.nextJSON(t, "snapshot").Rev
	// 300 revisions in far less time than the link needs for 300 messages.
	const revisions = 300
	for i := 0; i < revisions; i++ {
		store.Ingest(&telemetry.Report{State: telemetry.State{ID: uint32(i%30 + 1), Name: "RN", Status: "online", TemperatureC: float64(i)}})
		store.Publish()
	}
	latest := store.View().Rev
	msgs := 0
	for rev != latest {
		m := c.nextJSON(t, "delta", "snapshot")
		if m.Type == "delta" && m.Base != rev {
			t.Fatalf("gap: delta base %d after rev %d", m.Base, rev)
		}
		rev = m.Rev
		msgs++
	}
	st := h.Stats()
	t.Logf("%d revisions, %d messages on a slow link (catch-up deltas %d, snapshots %d)", revisions, msgs, st.CatchUps, st.Snapshots)
	if msgs > revisions/4 { // one catch-up delta, or a snapshot once the history (120 revisions) is exceeded
		t.Fatalf("%d messages for %d revisions: updates were queued, not conflated", msgs, revisions)
	}
}

func jsonNum(v uint64) string { b, _ := json.Marshal(v); return string(b) }

func TestTelemetryLimitsAndOrigin(t *testing.T) {
	_, _, srv := telemetryHub(t, 1)
	if _, code := dial(t, srv, "/", map[string]string{"Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", code)
	}
	c, code := dial(t, srv, "/", map[string]string{"Origin": srv.URL})
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("same origin: %d", code)
	}
	c.nextJSON(t, "hello")
	if _, code := dial(t, srv, "/", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("over the client limit: %d", code)
	}
}

func TestSpectrumFramesPerSweep(t *testing.T) {
	mgr := spectrum.NewManager(spectrum.NewSimHardware(20*time.Millisecond), "t", 5*time.Second, 0, 4)
	defer mgr.Close()
	srv := httptest.NewServer(NewSpectrum(mgr, "test", 2001, 5*time.Second, 4))
	defer srv.Close()
	c, _ := dial(t, srv, "/", nil)
	c.nextJSON(t, "hello")
	c.send(t, `{"type":"sub","nodeId":3,"port":1,"points":2001,"maxPoints":256}`)
	c.nextJSON(t, "subscribed")
	var last uint32
	for i := 0; i < 3; i++ {
		op, p := c.next(t, 5*time.Second)
		for op != 2 {
			op, p = c.next(t, 5*time.Second)
		}
		m, power, err := spectrum.DecodeFrame(p)
		if err != nil || m.NodeID != 3 || !m.Decimated || len(power) > 256 || m.SweepID <= last {
			t.Fatalf("frame %d: %+v (%d points) err %v", i, m, len(power), err)
		}
		last = m.SweepID
	}
}

// Run every test with the heavy-work pool active (one worker): a nested
// heavy.Do would deadlock and time the test out.
func TestMain(m *testing.M) {
	heavy.Start(1, 0)
	os.Exit(m.Run())
}
