package telemetry

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

var bandNames = []string{"B28-700", "B20-800", "B8-900", "B3-1800", "B1-2100", "B7-2600", "n78-3500", "B38-2600TDD"}

var alarmCodes = []struct{ code, severity string }{
	{"TEMP_HIGH", "minor"}, {"VSWR_HIGH", "major"}, {"OPT_RX_LOW", "major"}, {"DL_OVERDRIVE", "minor"}, {"FAN_FAIL", "critical"},
}

type simBand struct {
	Band
	dlTarget, ulTarget, vswrTarget float64
}

type simMetric struct {
	v, target float64
	noisy     bool
}

type simNode struct {
	id, chain, hop             int
	name, fw                   string
	bootAt                     time.Time
	seq                        uint64
	temp, tempTarget           float64
	fan, fanTarget, psu        float64
	rx, rxTarget, tx, txTarget float64
	lb, lbTarget               float64
	bands                      []simBand
	extra                      []simMetric
	alarms                     []Alarm
	nextAt                     time.Time
}

// Simulator emulates a fleet of Remote Nodes. The dynamics match the Node.js
// simulator: mean-reverting sensor noise, traffic-driven DL power changes,
// occasional alarms.
type Simulator struct {
	rng      *rand.Rand
	nodes    []*simNode
	interval time.Duration
}

func round(v float64, d int) float64 { p := math.Pow(10, float64(d)); return math.Round(v*p) / p }

// NewSimulator creates n nodes with 6 bands and 24 extra metrics each.
func NewSimulator(n int, interval time.Duration, seed uint64) *Simulator {
	s := &Simulator{rng: rand.New(rand.NewPCG(seed, 42)), interval: interval}
	r := s.rng
	u := func(lo, hi float64) float64 { return lo + (hi-lo)*r.Float64() }
	now := time.Now()
	for id := 1; id <= n; id++ {
		nd := &simNode{id: id, name: fmt.Sprintf("RN-%03d", id), chain: (id-1)/16 + 1, hop: (id-1)%16 + 1, fw: "4.2.1",
			bootAt: now.Add(-time.Duration(u(3600, 30*86400)) * time.Second)}
		if r.Float64() >= 0.9 {
			nd.fw = "4.1.7"
		}
		nd.temp = u(36, 48)
		nd.tempTarget = nd.temp
		// Every node has its own operating point; values wander around it.
		nd.fan = u(4200, 6400)
		nd.fanTarget = nd.fan
		nd.psu = 48 + u(-0.05, 0.05)
		nd.rx = u(-9, -5)
		nd.rxTarget = nd.rx
		nd.tx = u(0.5, 2.5)
		nd.txTarget = nd.tx
		nd.lb = u(26, 36)
		nd.lbTarget = nd.lb
		for b := 0; b < 6; b++ {
			dl := u(27, 33)
			vs := u(1.05, 1.35)
			ul := u(-100, -90)
			nd.bands = append(nd.bands, simBand{Band: Band{Name: bandNames[b], Enabled: r.Float64() > 0.05, DlOutDbm: dl, UlInDbm: ul,
				DlGainDb: 20 + r.IntN(10), UlGainDb: 12 + r.IntN(8), Vswr: vs}, dlTarget: dl, ulTarget: ul, vswrTarget: vs})
		}
		for m := 0; m < 24; m++ {
			v := u(0, 100)
			nd.extra = append(nd.extra, simMetric{v: v, target: v, noisy: r.Float64() < 0.5})
		}
		nd.nextAt = now.Add(time.Duration(r.Float64() * float64(interval)))
		s.nodes = append(s.nodes, nd)
	}
	return s
}

func (s *Simulator) evolve(n *simNode, now time.Time) {
	g := s.rng.NormFloat64
	r := s.rng
	n.seq++
	n.temp += g()*0.06 + (n.tempTarget-n.temp)*0.02
	n.fan += g()*15 + (n.fanTarget-n.fan)*0.05
	n.psu = 48 + g()*0.05
	n.rx += g()*0.02 + (n.rxTarget-n.rx)*0.05
	n.tx += g()*0.01 + (n.txTarget-n.tx)*0.05
	n.lb += g()*0.04 + (n.lbTarget-n.lb)*0.05
	for i := range n.bands {
		b := &n.bands[i]
		b.DlOutDbm += g()*0.1 + (b.dlTarget-b.DlOutDbm)*0.1
		b.UlInDbm += g()*0.3 + (b.ulTarget-b.UlInDbm)*0.2
		b.Vswr = math.Max(1.0, b.Vswr+g()*0.003+(b.vswrTarget-b.Vswr)*0.05)
	}
	for i := range n.extra {
		if e := &n.extra[i]; e.noisy {
			e.v += g()*0.15 + (e.target-e.v)*0.05
		}
	}
	if r.Float64() < 0.01 {
		n.bands[r.IntN(len(n.bands))].dlTarget = 27 + 6*r.Float64()
	}
	if r.Float64() < 0.003 {
		n.tempTarget = 36 + 14*r.Float64()
	}
	if len(n.alarms) > 0 && r.Float64() < 0.02 {
		n.alarms = n.alarms[1:]
	}
	if r.Float64() < 0.0015 {
		a := alarmCodes[r.IntN(len(alarmCodes))]
		exists := false
		for _, x := range n.alarms {
			exists = exists || x.Code == a.code
		}
		if !exists {
			n.alarms = append(n.alarms, Alarm{Code: a.code, Severity: a.severity, Since: now.UnixMilli()})
		}
	}
}

func (s *Simulator) report(n *simNode, now time.Time) *Report {
	st := State{ID: uint32(n.id), Name: n.name, Type: "remote", Chain: n.chain, Hop: n.hop, Status: "online", Fw: n.fw,
		TemperatureC: round(n.temp, 2), FanRpm: int(math.Round(n.fan)), PsuVoltageV: round(n.psu, 3),
		Optical: Optical{RxDbm: round(n.rx, 2), TxDbm: round(n.tx, 2), LaserBiasMa: round(n.lb, 2)}}
	for _, a := range n.alarms {
		if a.Severity != "minor" {
			st.Status = "degraded"
		}
	}
	st.Alarms = append([]Alarm(nil), n.alarms...)
	for _, b := range n.bands {
		st.Bands = append(st.Bands, Band{Name: b.Name, Enabled: b.Enabled, DlOutDbm: round(b.DlOutDbm, 2), UlInDbm: round(b.UlInDbm, 2),
			DlGainDb: b.DlGainDb, UlGainDb: b.UlGainDb, Vswr: round(b.Vswr, 3)})
	}
	for i, e := range n.extra {
		st.Metrics = append(st.Metrics, Metric{Name: fmt.Sprintf("m%02d", i), Value: round(e.v, 2)})
	}
	up := int64(now.Sub(n.bootAt) / time.Second)
	// Boot time instead of an uptime counter that changes on every report. Derived
	// from the truncated uptime it jitters by up to 1 s; the BootAtMs deadband absorbs that.
	st.BootAt = (now.UnixMilli() - up*1000 + 500) / 1000 * 1000
	return &Report{State: st, Seq: n.seq, ReportedAt: now.UnixMilli(), UptimeS: up}
}

// Initial returns one report per node (initial sync).
func (s *Simulator) Initial() []*Report {
	now := time.Now()
	out := make([]*Report, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, s.report(n, now))
	}
	return out
}

// Tick advances nodes whose report is due and returns their reports.
func (s *Simulator) Tick(now time.Time) []*Report {
	var out []*Report
	for _, n := range s.nodes {
		if n.nextAt.After(now) {
			continue
		}
		s.evolve(n, now)
		out = append(out, s.report(n, now))
		n.nextAt = now.Add(time.Duration(float64(s.interval) * (0.9 + 0.2*s.rng.Float64())))
	}
	return out
}

// ForceAll makes every node report on the next Tick (tests, demos).
func (s *Simulator) ForceAll() {
	for _, n := range s.nodes {
		n.nextAt = time.Time{}
	}
}
