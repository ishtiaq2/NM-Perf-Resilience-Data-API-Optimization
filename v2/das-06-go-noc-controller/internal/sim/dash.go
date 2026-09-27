package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"dasnoc/internal/dash"
	"dasnoc/internal/wsock"
)

// Latency collects alarm delivery latencies: device event time to dashboard receipt.
type Latency struct {
	mu      sync.Mutex
	samples []float64 // ms
	max     int
}

// NewLatency keeps at most max samples between two Takes.
func NewLatency(max int) *Latency { return &Latency{max: max} }

func (l *Latency) add(ms float64) {
	l.mu.Lock()
	if len(l.samples) < l.max {
		l.samples = append(l.samples, ms)
	}
	l.mu.Unlock()
}

// Take returns the samples collected since the last Take and resets.
func (l *Latency) Take() []float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.samples
	l.samples = nil
	return s
}

// Percentiles of samples (sorted in place).
func Percentiles(s []float64) map[string]float64 {
	if len(s) == 0 {
		return map[string]float64{"n": 0}
	}
	slices.Sort(s)
	at := func(p float64) float64 { return s[min(len(s)-1, int(float64(len(s))*p))] }
	return map[string]float64{"n": float64(len(s)), "p50": at(0.5), "p90": at(0.9), "p99": at(0.99), "max": s[len(s)-1]}
}

// DashStats are shared by all simulated dashboards.
type DashStats struct {
	Connected                                            atomic.Int64
	Connects, Failures                                   atomic.Uint64
	Messages, Bytes, Overviews, AlarmMsgs, Events, Sites atomic.Uint64
	Gaps, ScopeViolations                                atomic.Uint64
	// Checked by the dashboards that decode every event of the whole fleet: the NOC's
	// event numbers must arrive without holes (other than announced gaps) and in order.
	Missing, OutOfOrder atomic.Uint64
}

// Dashboard is a simulated NOC operator screen.
type Dashboard struct {
	URL     string // ws://host:port/api/ws/dashboard
	Token   string
	Tenant  string // "*" for NOC staff
	Site    string // drill-down site (optional)
	Stats   *DashStats
	Latency *Latency // nil: do not measure
}

// Run keeps the dashboard connected until ctx ends.
func (d *Dashboard) Run(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := wsock.Dial(ctx, d.URL, wsock.DialOptions{Subprotocols: []string{dash.Subprotocol, "bearer." + d.Token}, MaxMessageSize: 16 << 20})
		if err != nil {
			d.Stats.Failures.Add(1)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		d.Stats.Connects.Add(1)
		d.Stats.Connected.Add(1)
		d.session(ctx, conn)
		d.Stats.Connected.Add(-1)
		conn.CloseNow()
	}
}

func (d *Dashboard) session(ctx context.Context, conn *wsock.Conn) {
	sub, _ := json.Marshal(map[string]any{"t": "sub", "overview": true, "alarms": true, "site": d.Site})
	if conn.WriteText(sub) != nil {
		return
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.CloseNow()
		case <-stop:
		}
	}()
	var lastN uint64 // per session: a new subscription starts with new events only
	for {
		_, raw, err := conn.ReadMessage(2 * time.Minute)
		if err != nil {
			return
		}
		now := float64(time.Now().UnixNano()) / 1e6
		d.Stats.Messages.Add(1)
		d.Stats.Bytes.Add(uint64(len(raw)))
		if d.Latency == nil && d.Tenant == "*" && bytes.HasPrefix(raw, []byte(`{"t":"alarms"`)) {
			// A load-only screen: count, do not decode (the simulator's CPU is not the NOC's).
			d.Stats.AlarmMsgs.Add(1)
			d.Stats.Events.Add(uint64(bytes.Count(raw, []byte(`{"n":`))))
			continue
		}
		var m struct {
			T      string `json:"t"`
			Cursor uint64 `json:"cursor"`
			Events []struct {
				N      uint64 `json:"n"`
				Tenant string `json:"tenant"`
				State  string `json:"state"`
				Code   string `json:"code"`
				At     int64  `json:"at"`
			} `json:"events"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.T {
		case "error":
			// The drill-down site is not registered yet (the dashboard came up before the
			// device): subscribe again shortly, as an operator would click again.
			if d.Site != "" {
				time.AfterFunc(500*time.Millisecond, func() { _ = conn.WriteText(sub) })
			}
		case "overview":
			d.Stats.Overviews.Add(1)
		case "site":
			d.Stats.Sites.Add(1)
		case "gap":
			d.Stats.Gaps.Add(1)
			lastN = m.Cursor // announced: the events up to here are skipped
		case "alarms":
			d.Stats.AlarmMsgs.Add(1)
			d.Stats.Events.Add(uint64(len(m.Events)))
			for _, e := range m.Events {
				if d.Tenant == "*" {
					switch {
					case lastN != 0 && e.N <= lastN:
						d.Stats.OutOfOrder.Add(1)
					case lastN != 0 && e.N > lastN+1:
						d.Stats.Missing.Add(e.N - lastN - 1)
					}
					if e.N > lastN {
						lastN = e.N
					}
				}
				if d.Tenant != "*" && e.Tenant != d.Tenant {
					d.Stats.ScopeViolations.Add(1) // must never happen: tenant isolation
				}
				if d.Latency != nil && e.State == "raised" && e.Code != "SITE_UNREACHABLE" {
					d.Latency.add(now - float64(e.At))
				}
			}
		}
	}
}
