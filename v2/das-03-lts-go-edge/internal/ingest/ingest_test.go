package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dasedge/internal/telemetry"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestWireRoundTripIsCanonical(t *testing.T) {
	for _, r := range telemetry.NewSimulator(5, time.Second, 3).Initial() {
		w := FromState(&r.State)
		b, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte(`"bootAtMs":"`)) || !bytes.Contains(b, []byte(`"status":"NODE_STATUS_`)) {
			t.Fatalf("not the protobuf JSON mapping: %s", b)
		}
		var back NodeTelemetry
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		a, c := w.ToState(), back.ToState()
		if !bytes.Equal(a.Canonical(), c.Canonical()) {
			t.Fatalf("round trip changed the state:\n%s\n%s", a.Canonical(), c.Canonical())
		}
		if !bytes.Equal(r.State.Canonical(), a.Canonical()) {
			t.Fatalf("simulator state does not survive the wire mapping:\n%s\n%s", r.State.Canonical(), a.Canonical())
		}
	}
}

func TestEnumsAndInt64AcceptBothForms(t *testing.T) {
	var n NodeTelemetry
	if err := json.Unmarshal([]byte(`{"status":2,"bootAtMs":1700000000000,"alarms":[{"severity":"ALARM_SEVERITY_MAJOR","sinceMs":"5"}]}`), &n); err != nil {
		t.Fatal(err)
	}
	st := n.ToState()
	if st.Status != "degraded" || st.BootAt != 1700000000000 || st.Alarms[0].Severity != "major" || st.Alarms[0].Since != 5 {
		t.Fatalf("%+v", st)
	}
	if err := json.Unmarshal([]byte(`{"status":"BOGUS"}`), &n); err == nil {
		t.Fatal("unknown enum name must fail")
	}
}

func TestDiffPruneApplyReconstructs(t *testing.T) {
	sim := telemetry.NewSimulator(3, time.Second, 9)
	prev := map[uint32]*telemetry.State{}
	for _, r := range sim.Initial() {
		st := r.State
		prev[st.ID] = &st
	}
	for tick := 0; tick < 200; tick++ {
		sim.ForceAll()
		for _, r := range sim.Tick(time.Now()) {
			a, b := FromState(prev[r.ID]), FromState(&r.State)
			ta, tb := toTree(a), toTree(b)
			paths := Diff(ta, tb)
			payload, _ := json.Marshal(Prune(tb, paths))
			delta, _ := decodeTree(payload)
			base := clone(ta)
			if err := Apply(base, delta, paths); err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(base)
			want, _ := json.Marshal(tb)
			if !bytes.Equal(got, want) {
				t.Fatalf("paths %v did not reconstruct:\n%s\n%s", paths, got, want)
			}
			st := r.State
			prev[r.ID] = &st
		}
	}
	// A field that becomes zero is omitted from the JSON but still transferred by its path.
	a := toTree(NodeTelemetry{ID: 1, TemperatureC: 40, Optical: &Optical{RxDbm: -7}})
	b := toTree(NodeTelemetry{ID: 1, TemperatureC: 0, Optical: &Optical{RxDbm: -7}})
	paths := Diff(a, b)
	if len(paths) != 1 || paths[0] != "temperature_c" {
		t.Fatalf("paths %v", paths)
	}
	if err := Apply(a, Prune(b, paths), paths); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.(map[string]any)["temperatureC"]; ok {
		t.Fatal("zero value must be applied (removed from the tree)")
	}
}

type fleet struct {
	svc    *Service
	store  *telemetry.Store
	url    string
	agents []*Agent
	recvd  []*atomic.Int64
	stats  *AgentStats
	close  func()
}

func startMaster(t *testing.T, store *telemetry.Store) (*Service, string, func()) {
	t.Helper()
	svc := New(Options{Store: store, MaxNodeID: 1000, Log: quiet})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: svc.Handler(), Protocols: &p}
	go srv.Serve(ln)
	return svc, "http://" + ln.Addr().String(), func() { srv.Close() }
}

func startFleet(t *testing.T, n int) *fleet {
	t.Helper()
	store := telemetry.NewStore("t", false)
	svc, url, stop := startMaster(t, store)
	f := &fleet{svc: svc, store: store, url: url, stats: &AgentStats{}, close: stop}
	client := NewH2CClient()
	for i := 1; i <= n; i++ {
		a := NewAgent(uint32(i), f.stats)
		s, err := Open(context.Background(), client, url)
		if err != nil {
			t.Fatal(err)
		}
		a.Attach(s)
		got := &atomic.Int64{}
		go func() {
			for {
				r, err := s.Recv()
				if err != nil {
					return
				}
				a.Handle(r)
				got.Add(1)
			}
		}()
		f.agents = append(f.agents, a)
		f.recvd = append(f.recvd, got)
	}
	t.Cleanup(stop)
	return f
}

// settle waits until every agent got an answer for everything it sent.
func (f *fleet) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for i, a := range f.agents {
		for {
			a.mu.Lock()
			seq := int64(a.seq)
			a.mu.Unlock()
			if f.recvd[i].Load() >= seq {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("agent %d: %d answers for %d reports", a.NodeID, f.recvd[i].Load(), seq)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
}

func (f *fleet) run(t *testing.T, sim *telemetry.Simulator, ticks int) {
	t.Helper()
	now := time.Now()
	for tick := 0; tick < ticks; tick++ {
		sim.ForceAll()
		now = now.Add(time.Second)
		for _, r := range sim.Tick(now) {
			if err := f.agents[r.ID-1].Offer(r, now); err != nil {
				t.Fatal(err)
			}
		}
		f.settle(t)
	}
}

func (f *fleet) assertConsistent(t *testing.T) {
	t.Helper()
	for _, a := range f.agents {
		st, h, ok := f.store.Get(a.NodeID)
		a.mu.Lock()
		sent, sentHash := a.sent, a.sentHash
		a.mu.Unlock()
		if !ok || !bytes.Equal(st.Canonical(), sent.Canonical()) || h != sentHash {
			t.Fatalf("node %d: master copy differs from the node's state", a.NodeID)
		}
	}
}

func TestStreamsKeepMasterConsistentWithDeltas(t *testing.T) {
	const nodes = 20
	f := startFleet(t, nodes)
	sim := telemetry.NewSimulator(nodes, time.Second, 5)
	now := time.Now()
	for _, r := range sim.Initial() {
		if err := f.agents[r.ID-1].Offer(r, now); err != nil {
			t.Fatal(err)
		}
	}
	f.settle(t)
	f.run(t, sim, 40)
	f.assertConsistent(t)
	st := f.svc.Stats()
	t.Logf("master: %+v", st)
	t.Logf("nodes: offered %d, suppressed %d, full %d, deltas %d, keep-alives %d", f.stats.Offered.Load(), f.stats.Suppressed.Load(), f.stats.Full.Load(), f.stats.Deltas.Load(), f.stats.KeepAlives.Load())
	if st.Full != nodes || st.Deltas == 0 || st.Resyncs != 0 || st.HashMismatches != 0 {
		t.Fatalf("unexpected stats %+v", st)
	}
	if f.stats.Suppressed.Load() == 0 {
		t.Fatal("deadbands should suppress some reports at the source")
	}
}

func TestMasterRestartHealsWithResync(t *testing.T) {
	f := startFleet(t, 5)
	sim := telemetry.NewSimulator(5, time.Second, 8)
	now := time.Now()
	for _, r := range sim.Initial() {
		_ = f.agents[r.ID-1].Offer(r, now)
	}
	f.settle(t)
	f.run(t, sim, 5)
	// The master loses its copies (restart with the same streams, or a bug).
	f.svc.mu.Lock()
	f.svc.mirrors = map[uint32]*mirror{}
	f.svc.mu.Unlock()
	f.run(t, sim, 15)
	f.assertConsistent(t)
	if f.svc.Stats().Resyncs == 0 || f.stats.Resyncs.Load() == 0 {
		t.Fatal("deltas against a lost base must be answered with RESYNC")
	}
}

func TestCorruptDeltaIsDetectedByHash(t *testing.T) {
	store := telemetry.NewStore("t", false)
	svc := New(Options{Store: store, Log: quiet})
	st := telemetry.NewSimulator(1, time.Second, 1).Initial()[0].State
	w := FromState(&st)
	norm := w.ToState()
	full, _ := json.Marshal(w)
	if r := svc.apply(&ReportRequest{NodeID: 1, Seq: 1, Full: true, State: full, StateHash: U64(StateHash(&norm))}); r.Action != ActionOK {
		t.Fatal(r.Action)
	}
	// Claims temperature 99 but the hash is of the old state: must not be applied.
	r := svc.apply(&ReportRequest{NodeID: 1, Seq: 2, BaseHash: U64(StateHash(&norm)), StateHash: U64(StateHash(&norm)),
		State: []byte(`{"temperatureC":99}`), ChangedPaths: []string{"temperature_c"}})
	if r.Action != ActionResync {
		t.Fatalf("got %s", r.Action)
	}
	if got, _, _ := store.Get(1); got.TemperatureC == 99 {
		t.Fatal("unverified delta was applied")
	}
}

func openRaw(t *testing.T, url, contentType string, body io.Reader) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+Procedure, body)
	req.Header.Set("Content-Type", contentType)
	resp, err := NewH2CClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func envelope(v any) []byte {
	var b bytes.Buffer
	p, _ := json.Marshal(v)
	_ = writeEnvelope(&b, 0, p)
	return b.Bytes()
}

func TestProtocolErrors(t *testing.T) {
	_, url, stop := startMaster(t, telemetry.NewStore("t", false))
	defer stop()
	if r := openRaw(t, url, "application/json", strings.NewReader("{}")); r.StatusCode != http.StatusUnsupportedMediaType || r.Header.Get("Accept-Post") != ContentType {
		t.Fatalf("status %d", r.StatusCode)
	}
	endError := func(body []byte) *Error {
		r := openRaw(t, url, ContentType, bytes.NewReader(body))
		s := &Stream{body: r.Body}
		defer r.Body.Close()
		for {
			_, err := s.Recv()
			var ce *Error
			if errors.As(err, &ce) {
				return ce
			}
			if err != nil {
				return nil
			}
		}
	}
	if e := endError([]byte{0, 0, 0, 0, 3, 'n', 'o', 't'}); e == nil || e.Code != "invalid_argument" {
		t.Fatalf("bad JSON: %v", e)
	}
	if e := endError(envelope(map[string]any{"nodeId": 0})); e == nil || e.Code != "invalid_argument" {
		t.Fatalf("node 0: %v", e)
	}
	two := append(envelope(map[string]any{"nodeId": 1, "seq": "1", "full": true, "state": map[string]any{"id": 1}}), envelope(map[string]any{"nodeId": 2, "seq": "1"})...)
	if e := endError(two); e == nil || !strings.Contains(e.Message, "one stream per node") {
		t.Fatalf("two nodes on one stream: %v", e)
	}
	if e := endError([]byte{0, 0x7f, 0, 0, 0}); e == nil || e.Code != "resource_exhausted" {
		t.Fatalf("oversized: %v", e)
	}
	if e := endError(nil); e != nil {
		t.Fatalf("an empty stream ends cleanly, got %v", e)
	}
}
