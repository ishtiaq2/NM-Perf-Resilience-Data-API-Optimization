package telemetry

import (
	"dasedge/internal/heavy"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

func report(id uint32, temp float64, seq uint64) *Report {
	return &Report{State: State{ID: id, Name: "RN", Status: "online", TemperatureC: temp,
		Bands: []Band{{Name: "B3", DlOutDbm: 30}}, Metrics: []Metric{{Name: "m00", Value: 10}}}, Seq: seq, ReportedAt: int64(seq), UptimeS: int64(seq)}
}

type snap struct {
	Rev   uint64                     `json:"rev"`
	Nodes map[string]json.RawMessage `json:"nodes"`
}
type delta struct {
	Rev, Base uint64
	Changed   map[string]json.RawMessage `json:"changed"`
	Removed   []uint32                   `json:"removed"`
}

func TestNoiseAndCountersAreNotChanges(t *testing.T) {
	s := NewStore("b", true)
	s.Ingest(report(1, 40, 1))
	s.Publish()
	if s.Ingest(report(1, 40.3, 2)) { // within 0.5 deadband, new seq/uptime
		t.Fatal("noise + counters must not count as a change")
	}
	if s.Publish() != nil {
		t.Fatal("no revision expected")
	}
	if !s.Ingest(report(1, 41, 3)) {
		t.Fatal("a 1 degree change must be published")
	}
}

func TestDeltasReconstructSnapshot(t *testing.T) {
	s := NewStore("b", false)
	for i := uint32(1); i <= 5; i++ {
		s.Ingest(report(i, 40, 1))
	}
	s.Publish()
	var base snap
	_ = json.Unmarshal(s.View().SnapshotJSON(), &base)
	s.Ingest(report(2, 50, 2))
	s.Publish()
	s.Ingest(report(4, 60, 2))
	s.Remove(5)
	s.Publish()
	raw, _, ok := s.DeltaJSON(base.Rev)
	if !ok {
		t.Fatal("delta expected")
	}
	var d delta
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err, string(raw))
	}
	for id, n := range d.Changed {
		base.Nodes[id] = n
	}
	for _, id := range d.Removed {
		delete(base.Nodes, string(rune('0'+id)))
	}
	var latest snap
	_ = json.Unmarshal(s.View().SnapshotJSON(), &latest)
	if len(base.Nodes) != len(latest.Nodes) {
		t.Fatalf("node count %d vs %d", len(base.Nodes), len(latest.Nodes))
	}
	for id, n := range latest.Nodes {
		if string(base.Nodes[id]) != string(n) {
			t.Fatalf("node %s differs after applying delta", id)
		}
	}
	if _, _, ok := s.DeltaJSON(s.View().Rev + 1); ok {
		t.Fatal("future revision must force a snapshot")
	}
	if _, _, ok := s.DeltaJSON(999); ok {
		t.Fatal("unknown old revision must force a snapshot")
	}
}

func TestRevisionFromPreviousBootForcesSnapshot(t *testing.T) {
	boot1, boot2 := NewStore("a", false), NewStore("b", false)
	for i := uint32(1); i <= 3; i++ {
		boot1.Ingest(report(i, 40, 1))
		boot2.Ingest(report(i, 40, 1))
	}
	boot1.Publish()
	for r := 0; r < 5; r++ { // the new boot is further along than the old one ever was
		boot2.Ingest(report(1, 40+float64(r+1), uint64(r+2)))
		boot2.Publish()
	}
	if _, _, ok := boot2.DeltaJSON(boot1.View().Rev); ok {
		t.Fatalf("revision %d of the previous boot was answered with a delta (current %d)", boot1.View().Rev, boot2.View().Rev)
	}
}

func TestJSONEncodingIsValidForAnyInput(t *testing.T) {
	st := State{ID: 1, Name: "RN \"quoted\" \\ \x01 \xff é", Status: "online", TemperatureC: math.NaN(), PsuVoltageV: math.Inf(1)}
	var v map[string]any
	if err := json.Unmarshal(st.Canonical(), &v); err != nil {
		t.Fatalf("invalid JSON %s: %v", st.Canonical(), err)
	}
	if v["name"] != "RN \"quoted\" \\ \x01 \ufffd é" || v["temperatureC"] != nil {
		t.Fatalf("round trip: %#v", v)
	}
}

func TestPublishEventDeltaIsSharedAndValidJSON(t *testing.T) {
	s := NewStore("b", false)
	var got *PublishEvent
	s.Subscribe(func(e *PublishEvent) { got = e })
	s.Ingest(report(1, 40, 1))
	s.Publish()
	a, b := got.DeltaJSON(), got.DeltaJSON()
	if &a[0] != &b[0] {
		t.Fatal("delta must be serialised once")
	}
	var d delta
	if err := json.Unmarshal(a, &d); err != nil || d.Rev != d.Base+1 || d.Rev != s.View().Rev {
		t.Fatalf("bad delta %s", a)
	}
}

func TestStaleNodesGoOffline(t *testing.T) {
	s := NewStore("b", true)
	s.StaleAfter = time.Millisecond
	s.Ingest(report(1, 40, 1))
	s.Publish()
	time.Sleep(5 * time.Millisecond)
	ev := s.Publish()
	if ev == nil || len(ev.IDs) != 1 {
		t.Fatal("offline transition must be published")
	}
	var n struct{ Status string }
	var sn snap
	_ = json.Unmarshal(s.View().SnapshotJSON(), &sn)
	_ = json.Unmarshal(sn.Nodes["1"], &n)
	if n.Status != "offline" {
		t.Fatalf("status %q", n.Status)
	}
	if !s.Ingest(report(1, 40, 2)) {
		t.Fatal("a node coming back is a change")
	}
}

func TestSimulatorChangeRateWithDeadbands(t *testing.T) {
	sim := NewSimulator(300, time.Second, 11)
	raw, norm := NewStore("r", false), NewStore("n", true)
	for _, r := range sim.Initial() {
		raw.Ingest(r)
		norm.Ingest(r)
	}
	raw.Publish()
	norm.Publish()
	var rawChanged, normChanged int
	for sec := 0; sec < 60; sec++ {
		sim.ForceAll()
		for _, r := range sim.Tick(time.Now()) {
			raw.Ingest(r)
			norm.Ingest(r)
		}
		if ev := raw.Publish(); ev != nil && sec >= 20 {
			rawChanged += len(ev.IDs)
		}
		if ev := norm.Publish(); ev != nil && sec >= 20 {
			normChanged += len(ev.IDs)
		}
	}
	t.Logf("changed nodes per second: raw %.1f, normalised %.1f (of 300)", float64(rawChanged)/40, float64(normChanged)/40)
	if normChanged*4 > rawChanged {
		t.Fatalf("deadbands should suppress most changes: raw %d, normalised %d", rawChanged, normChanged)
	}
}

// Run every test with the heavy-work pool active (one worker): a nested
// heavy.Do would deadlock and time the test out.
func TestMain(m *testing.M) {
	heavy.Start(1, 0)
	os.Exit(m.Run())
}
