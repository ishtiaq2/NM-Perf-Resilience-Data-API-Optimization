package spectrum

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"testing"
	"time"
)

// Frame produced by the Node.js encoder (das-01 src/shared/spectrum-frame.js).
const nodeFrameB64 = "RFNQQwEBAQAqAAAABwAAAAIAAAAFAAAAAAAAgJPcxEEAAAAAAIjjQAAAwAZFDHpCENpe6ACA0gTw2A=="

func TestDecodeNodeFrameAndRoundTrip(t *testing.T) {
	raw, _ := base64.StdEncoding.DecodeString(nodeFrameB64)
	m, p, err := DecodeFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.SweepID != 42 || m.NodeID != 7 || m.Port != 2 || m.StartHz != 700e6 || m.StepHz != 40000 || !m.Decimated {
		t.Fatalf("header %+v", m)
	}
	if p[0] != -97.12 || p[1] != -60.5 || !math.IsNaN(float64(p[2])) || p[4] != -100 {
		t.Fatalf("payload %v", p)
	}
	// Go must produce the same bytes as Node for the same input.
	again := EncodeFrame(m, []float32{-97.12, -60.5, float32(math.NaN()), 12.34, -100}, EncI16)
	if base64.StdEncoding.EncodeToString(again) != nodeFrameB64 {
		t.Fatalf("Go encoder differs from Node encoder:\n%s\n%s", base64.StdEncoding.EncodeToString(again), nodeFrameB64)
	}
}

// Negative half-way values (common with FPGAs reporting 1/8 or 1/16 dB steps) must
// round like JavaScript's Math.round, i.e. towards +Inf, as the shipped backend does.
func TestRoundingTiesMatchNode(t *testing.T) {
	const nodeTiesB64 = "RFNQQwEBAAAJAAAAAwAAAAEAAAAFAAAAAAAAgJPcxEEAAAAAAIjjQAAAgFb+vHhCv9qm2kIl9P+Z2A=="
	ties := []float32{-95.375, -95.625, 95.375, -0.125, -100.875}
	m := FrameMeta{SweepID: 9, NodeID: 3, Port: 1, StartHz: 700e6, StepHz: 40000, TimestampMs: 1.7e12}
	if got := base64.StdEncoding.EncodeToString(EncodeFrame(m, ties, EncI16)); got != nodeTiesB64 {
		t.Fatalf("DSPC differs from Node:\n%s\n%s", got, nodeTiesB64)
	}
	want := []string{"-95.37", "-95.62", "95.38", "-0.12", "-100.87"}
	for i, v := range ties {
		if got := string(appendNum(nil, round2(v))); got != want[i] {
			t.Fatalf("round2(%v) = %s, Node prints %s", v, got, want[i])
		}
	}
}

func TestPeakDecimationKeepsSpurs(t *testing.T) {
	p := make([]float32, 50001)
	for i := range p {
		p[i] = -100
	}
	p[31337] = -40
	out, start, step, dec := PeakDecimate(p, 700e6, 40000, 1600)
	if !dec || len(out) > 1600 {
		t.Fatalf("len %d dec %v", len(out), dec)
	}
	peak := 0
	for i, v := range out {
		if v > out[peak] {
			peak = i
		}
	}
	if out[peak] != -40 || math.Abs(start+float64(peak)*step-(700e6+31337*40000)) > step/2+1 {
		t.Fatalf("spur lost or misplaced")
	}
	if NormaliseMaxPoints(1599) != 1600 || NormaliseMaxPoints(0) != 0 || NormaliseMaxPoints(1e9) != 65536 {
		t.Fatal("normalise")
	}
}

func TestLegacyJSONShape(t *testing.T) {
	p, _ := ParseParams("3", "2", "", "", "2001", 50001)
	s := &Sweep{ID: 9, P: p, Timestamp: 1790000000123, Power: []float32{-97.12, -60.5}}
	s.P.Points = 2
	s.P.StopHz = s.P.StartHz + 40000
	var v struct {
		SweepID int `json:"sweepId"`
		Points  []struct {
			Frequency float64 `json:"frequency"`
			Power     float64 `json:"power"`
		} `json:"points"`
	}
	if err := json.Unmarshal(LegacyJSON(s), &v); err != nil {
		t.Fatal(err)
	}
	if v.SweepID != 9 || len(v.Points) != 2 || v.Points[0].Power != -97.12 || v.Points[1].Frequency != 700040000 {
		t.Fatalf("%+v", v)
	}
}

func TestSessionsShareSweepsAndStopWhenIdle(t *testing.T) {
	hw := NewSimHardware(20 * time.Millisecond)
	m := NewManager(hw, "boot", 150*time.Millisecond, 0, 4)
	defer m.Close()
	p, _ := ParseParams("1", "1", "", "", "2001", 2001)
	a, _ := m.Session(p)
	b, _ := m.Session(p)
	if a != b {
		t.Fatal("same parameters must share one session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s1, err := a.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := a.Next(ctx)
	if s2.ID <= s1.ID {
		t.Fatal("Next must return a newer sweep")
	}
	v1 := s2.Variant(m, KindI16, 256, false)
	v2 := s2.Variant(m, KindI16, 256, false)
	if v1 != v2 || m.variantBuilds.Load() != 1 {
		t.Fatal("variant must be built once per sweep")
	}
	time.Sleep(400 * time.Millisecond) // idle timeout
	a.mu.Lock()
	running := a.running
	a.mu.Unlock()
	if running {
		t.Fatal("session should stop sweeping when nobody watches")
	}
}
