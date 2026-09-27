// Package sim simulates the device side of das-noc.v1 (Master Units) and NOC
// dashboards, for tests and for cmd/fleetsim, which runs thousands of them.
//
// A simulated Master Unit behaves like the real agent must: it dials out, says
// hello, sends what the NOC is missing (per the welcome), then reports summary
// deltas, alarm events with sequence numbers kept in an outbox until acknowledged,
// and node-level detail while subscribed. It reconnects with jittered exponential
// backoff and honours the NOC's Retry-After.
package sim

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"dasnoc/internal/proto"
	"dasnoc/internal/wsock"
)

// FleetStats are shared by all simulated Master Units.
type FleetStats struct {
	Dials, Connects, Refused, AuthFailed, Failures, Disconnects atomic.Uint64
	Connected                                                   atomic.Int64
	Messages, Bytes                                             atomic.Uint64
	Resyncs, Acks                                               atomic.Uint64
	Raised, Cleared                                             atomic.Uint64
}

// MUConfig of one simulated Master Unit.
type MUConfig struct {
	URL           string // ws://host:port/uplink/v1
	Token         string
	Site          string
	Name          string
	Venue         string
	Region        string
	Nodes         int           // Remote Nodes behind this head-end (default 300)
	Tick          time.Duration // summary model step (default 5 s)
	AlarmsPerHour float64       // spontaneous alarm raises per hour (default 12)
	OutboxMax     int           // unacknowledged events kept (default 10 000)
	BackoffBase   time.Duration // default 1 s
	BackoffMax    time.Duration // default 60 s
	TLS           *tls.Config
	LocalAddr     net.Addr
	Seed          uint64
	Stats         *FleetStats
	Paused        *atomic.Bool // freezes the model (consistency checks)
}

type nodeModel struct {
	ID     int     `json:"id"`
	Name   string  `json:"name"`
	Type   string  `json:"type"`
	Chain  int     `json:"chain"`
	Hop    int     `json:"hop"`
	Status string  `json:"status"`
	TempC  float64 `json:"tempC"`
	RxDbm  float64 `json:"rxDbm"`
	TxDbm  float64 `json:"txDbm"`
	Vswr   float64 `json:"vswr"`
	PsuV   float64 `json:"psuV"`
	FW     string  `json:"fw"`
}

// MasterUnit is one simulated device.
type MasterUnit struct {
	cfg MUConfig
	rng *rand.Rand

	mu       sync.Mutex
	boot     string
	summary  map[string]any
	lastSent map[string]string // canonical value of each field as the NOC has it
	rev      uint64
	hash     string
	temp     float64
	rx       float64
	vswr     float64
	offline  int
	degraded int

	seq      uint64
	outbox   []proto.AlarmEvent
	overflow bool
	active   map[string]proto.AlarmEvent

	detailOn       bool
	detailInterval time.Duration
	detailRev      uint64
	nodes          []nodeModel

	conn   *wsock.Conn
	inject chan int
	poke   chan struct{} // detail-sub arrived: re-evaluate the loop's timers
}

var alarmCodes = []struct{ code, sev string }{
	{"TEMP_HIGH", "minor"}, {"DL_OVERDRIVE", "minor"}, {"VSWR_HIGH", "major"}, {"OPT_RX_LOW", "major"}, {"FAN_FAIL", "critical"}, {"LINK_DEGRADED", "warning"},
}

// NewMasterUnit creates a device with a deterministic model.
func NewMasterUnit(cfg MUConfig) *MasterUnit {
	if cfg.Nodes <= 0 {
		cfg.Nodes = 300
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 5 * time.Second
	}
	if cfg.AlarmsPerHour <= 0 {
		cfg.AlarmsPerHour = 12
	}
	if cfg.OutboxMax <= 0 {
		cfg.OutboxMax = 10_000
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 60 * time.Second
	}
	if cfg.Stats == nil {
		cfg.Stats = &FleetStats{}
	}
	rng := rand.New(rand.NewPCG(cfg.Seed, 0x9E3779B97F4A7C15))
	m := &MasterUnit{cfg: cfg, rng: rng, active: map[string]proto.AlarmEvent{}, inject: make(chan int, 4), poke: make(chan struct{}, 1)}
	m.boot = strconv.FormatUint(rng.Uint64()|1<<60, 16)[:12]
	m.temp, m.rx, m.vswr = 38+rng.Float64()*10, -9+rng.Float64()*3, 1.15+rng.Float64()*0.2
	m.rebuild()
	return m
}

// Boot is the current boot id.
func (m *MasterUnit) Boot() string { m.mu.Lock(); defer m.mu.Unlock(); return m.boot }

// Reboot simulates a device restart: new boot id, sequence and outbox reset.
func (m *MasterUnit) Reboot() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.boot = strconv.FormatUint(m.rng.Uint64()|1<<60, 16)[:12]
	m.seq, m.outbox, m.overflow = 0, nil, false
	m.rev = 0
	if m.conn != nil {
		m.conn.CloseNow()
	}
}

// round to a multiple of step (0.5, 0.1, 0.01), as the nearest decimal: multiplying
// by step instead would give -7.300000000000001 for -73 x 0.1.
func round(v, step float64) float64 {
	inv := math.Round(1 / step)
	return math.Round(v*inv) / inv
}

// rebuild derives the summary from the model; the caller holds mu.
func (m *MasterUnit) rebuild() {
	n := m.cfg.Nodes
	m.summary = map[string]any{
		"name": m.cfg.Name, "venue": m.cfg.Venue, "region": m.cfg.Region, "fw": "3.1.4",
		"nodes": float64(n), "online": float64(n - m.offline), "offline": float64(m.offline), "degraded": float64(m.degraded),
		"maxTempC": round(m.temp, 0.5), "minRxDbm": round(m.rx, 0.1), "maxVswr": round(m.vswr, 0.01),
	}
	b, _ := proto.AppendCanonical(nil, m.summary)
	m.hash = proto.HashHex(proto.FNV1a64(b))
}

func canonical(v any) string { b, _ := proto.AppendCanonical(nil, v); return string(b) }

// step moves the model and returns a delta message if anything the NOC sees changed.
func (m *MasterUnit) step() *proto.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg.Paused != nil && m.cfg.Paused.Load() {
		return nil
	}
	r := m.rng
	m.temp += r.NormFloat64()*0.35 + (43-m.temp)*0.02
	m.rx += r.NormFloat64()*0.05 + (-7.5-m.rx)*0.02
	m.vswr = math.Max(1.02, m.vswr+r.NormFloat64()*0.004+(1.25-m.vswr)*0.02)
	if r.Float64() < 0.01 {
		m.offline = max(0, m.offline+[]int{-1, 1}[r.IntN(2)])
	}
	if r.Float64() < 0.02 {
		m.degraded = max(0, m.degraded+[]int{-1, 1}[r.IntN(2)])
	}
	m.rebuild()
	return m.deltaLocked()
}

func (m *MasterUnit) deltaLocked() *proto.Message {
	if m.lastSent == nil {
		return nil // the NOC has no summary yet: a full one goes out on (re)connect
	}
	changed := map[string]json.RawMessage{}
	for k, v := range m.summary {
		c := canonical(v)
		if m.lastSent[k] != c {
			changed[k] = json.RawMessage(c)
			m.lastSent[k] = c
		}
	}
	for k := range m.lastSent {
		if _, ok := m.summary[k]; !ok {
			changed[k] = json.RawMessage("null")
			delete(m.lastSent, k)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	base := m.rev
	m.rev++
	return &proto.Message{T: proto.TDelta, Base: base, Rev: m.rev, Hash: m.hash, S: changed}
}

func (m *MasterUnit) fullSummaryLocked() *proto.Message {
	s := make(map[string]json.RawMessage, len(m.summary))
	m.lastSent = make(map[string]string, len(m.summary))
	for k, v := range m.summary {
		c := canonical(v)
		s[k] = json.RawMessage(c)
		m.lastSent[k] = c
	}
	m.rev++
	return &proto.Message{T: proto.TSummary, Rev: m.rev, Hash: m.hash, S: s}
}

func (m *MasterUnit) fullAlarmsLocked() *proto.Message {
	ev := make([]proto.AlarmEvent, 0, len(m.active))
	for _, a := range m.active {
		ev = append(ev, a)
	}
	slices.SortFunc(ev, func(a, b proto.AlarmEvent) int { return int(a.Seq) - int(b.Seq) })
	return &proto.Message{T: proto.TAlarms, Full: true, UpTo: m.seq, Ev: ev}
}

// event records an alarm raise/clear in the outbox; the caller holds mu.
func (m *MasterUnit) eventLocked(id string, node int, code, sev, state, text string) proto.AlarmEvent {
	m.seq++
	ev := proto.AlarmEvent{Seq: m.seq, ID: id, Node: node, Code: code, Sev: sev, State: state, At: time.Now().UnixMilli(), Text: text}
	if state == "raised" {
		m.active[id] = ev
		m.cfg.Stats.Raised.Add(1)
	} else {
		delete(m.active, id)
		m.cfg.Stats.Cleared.Add(1)
	}
	m.outbox = append(m.outbox, ev)
	if len(m.outbox) > m.cfg.OutboxMax {
		m.outbox = m.outbox[len(m.outbox)-m.cfg.OutboxMax:]
		m.overflow = true // the NOC will see a gap and ask for the full list
	}
	return ev
}

// randomAlarm raises or clears one alarm.
func (m *MasterUnit) randomAlarm() []proto.AlarmEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rng
	if n := len(m.active); n > 0 && r.Float64() < math.Min(0.9, 0.35+0.15*float64(n)) {
		ids := make([]string, 0, n)
		for id := range m.active {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		a := m.active[ids[r.IntN(len(ids))]]
		return []proto.AlarmEvent{m.eventLocked(a.ID, a.Node, a.Code, a.Sev, "cleared", "")}
	}
	c := alarmCodes[r.IntN(len(alarmCodes))]
	node := 1 + r.IntN(m.cfg.Nodes)
	id := "n" + strconv.Itoa(node) + ":" + c.code
	if _, ok := m.active[id]; ok {
		return nil
	}
	return []proto.AlarmEvent{m.eventLocked(id, node, c.code, c.sev, "raised", "")}
}

// Inject raises n critical alarms at once (a power failure at the venue), or
// clears them with n = 0. It works while disconnected too (the outbox keeps them).
func (m *MasterUnit) Inject(n int) {
	select {
	case m.inject <- n:
	default:
	}
}

func (m *MasterUnit) injected(n int) []proto.AlarmEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []proto.AlarmEvent
	if n == 0 {
		for id, a := range m.active {
			if a.Code == "PSU_FAIL" {
				out = append(out, m.eventLocked(id, a.Node, a.Code, a.Sev, "cleared", "mains restored"))
			}
		}
		return out
	}
	for i := 1; i <= min(n, m.cfg.Nodes); i++ {
		id := "n" + strconv.Itoa(i) + ":PSU_FAIL"
		if _, ok := m.active[id]; !ok {
			out = append(out, m.eventLocked(id, i, "PSU_FAIL", "critical", "raised", "mains failure, on battery"))
		}
	}
	return out
}

// Expect is what the NOC should show for this device (consistency checks).
func (m *MasterUnit) Expect() (hash string, alarms [4]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.active {
		if i := proto.SeverityRank(a.Sev); i > 0 {
			alarms[4-i]++
		}
	}
	return m.hash, alarms
}

// ---------------------------------------------------------------- connection

func (m *MasterUnit) send(conn *wsock.Conn, msg *proto.Message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	m.cfg.Stats.Messages.Add(1)
	m.cfg.Stats.Bytes.Add(uint64(len(b)))
	return conn.WriteText(b)
}

// Run keeps the device connected until ctx ends.
func (m *MasterUnit) Run(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		m.cfg.Stats.Dials.Add(1)
		conn, err := wsock.Dial(ctx, m.cfg.URL, wsock.DialOptions{
			Subprotocols: []string{proto.Subprotocol, "bearer." + m.cfg.Token}, TLSConfig: m.cfg.TLS,
			MaxMessageSize: 1 << 20, LocalAddr: m.cfg.LocalAddr, HandshakeTimeout: 15 * time.Second,
		})
		if err != nil {
			wait := time.Duration(0)
			var he *wsock.HandshakeError
			switch {
			case errors.As(err, &he) && he.Status == 401:
				m.cfg.Stats.AuthFailed.Add(1)
				wait = m.cfg.BackoffMax // a bad token will not fix itself quickly
			case errors.As(err, &he) && he.RetryAfter > 0:
				m.cfg.Stats.Refused.Add(1)
				wait = he.RetryAfter + time.Duration(rand.Int64N(int64(time.Second)))
			default:
				m.cfg.Stats.Failures.Add(1)
				// Full jitter: uniform in [0, min(max, base x 2^attempt)].
				ceil := min(m.cfg.BackoffMax, m.cfg.BackoffBase<<min(attempt, 10))
				wait = time.Duration(rand.Int64N(int64(ceil) + 1))
			}
			attempt++
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		attempt = 0
		m.session(ctx, conn)
		conn.CloseNow()
		// A short jittered pause: a NOC restart must not bring every device back in the same millisecond.
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(rand.Int64N(int64(2 * time.Second)))):
		}
	}
}

func (m *MasterUnit) session(ctx context.Context, conn *wsock.Conn) {
	if m.send(conn, &proto.Message{T: proto.THello, Boot: m.Boot(), FW: "3.1.4", Agent: "fleetsim/1", Caps: []string{"detail"}}) != nil {
		return
	}
	_, raw, err := conn.ReadMessage(10 * time.Second)
	var w proto.Message
	if err != nil || json.Unmarshal(raw, &w) != nil || w.T != proto.TWelcome {
		m.cfg.Stats.Failures.Add(1)
		return
	}
	m.cfg.Stats.Connects.Add(1)
	m.cfg.Stats.Connected.Add(1)
	defer func() {
		m.cfg.Stats.Connected.Add(-1)
		m.cfg.Stats.Disconnects.Add(1)
		m.mu.Lock()
		m.conn, m.detailOn = nil, false
		m.mu.Unlock()
	}()
	idle := 3 * time.Minute
	if w.KeepaliveS > 0 {
		idle = time.Duration(w.KeepaliveS) * time.Second * 5 / 2
	}

	// Send what the NOC is missing.
	m.mu.Lock()
	m.conn = conn
	var sync []*proto.Message
	have := w.Have
	if have == nil || have.Boot != m.boot {
		sync = append(sync, m.fullSummaryLocked(), m.fullAlarmsLocked())
	} else {
		if have.Rev != m.rev || have.Hash != m.hash {
			sync = append(sync, m.fullSummaryLocked())
		}
		m.dropAckedLocked(have.AckSeq)
		if len(m.outbox) > 0 {
			if m.outbox[0].Seq != have.AckSeq+1 || m.overflow {
				sync = append(sync, m.fullAlarmsLocked())
			} else {
				sync = append(sync, &proto.Message{T: proto.TAlarms, Ev: slices.Clone(m.outbox)})
			}
		}
	}
	m.mu.Unlock()
	for _, s := range sync {
		if m.send(conn, s) != nil {
			return
		}
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer cancel()
		for {
			_, raw, err := conn.ReadMessage(idle)
			if err != nil {
				return
			}
			var msg proto.Message
			if json.Unmarshal(raw, &msg) != nil {
				continue
			}
			m.handle(conn, &msg)
		}
	}()

	tick := time.NewTicker(m.cfg.Tick + time.Duration(rand.Int64N(int64(m.cfg.Tick/5)+1)))
	defer tick.Stop()
	alarm := time.NewTimer(m.nextAlarm())
	defer alarm.Stop()
	var detail <-chan time.Time
	var detailTicker *time.Ticker
	defer func() {
		if detailTicker != nil {
			detailTicker.Stop()
		}
	}()
	for {
		// Detail streaming follows the NOC's detail-sub.
		m.mu.Lock()
		on, every := m.detailOn, m.detailInterval
		m.mu.Unlock()
		if on && detailTicker == nil {
			detailTicker = time.NewTicker(every)
			detail = detailTicker.C
		} else if !on && detailTicker != nil {
			detailTicker.Stop()
			detailTicker, detail = nil, nil
		}
		select {
		case <-sctx.Done():
			return
		case <-tick.C:
			if d := m.step(); d != nil && m.send(conn, d) != nil {
				return
			}
		case <-alarm.C:
			alarm.Reset(m.nextAlarm())
			if m.cfg.Paused != nil && m.cfg.Paused.Load() {
				continue
			}
			if ev := m.randomAlarm(); len(ev) > 0 && m.send(conn, &proto.Message{T: proto.TAlarms, Ev: ev}) != nil {
				return
			}
		case n := <-m.inject:
			if ev := m.injected(n); len(ev) > 0 && m.send(conn, &proto.Message{T: proto.TAlarms, Ev: ev}) != nil {
				return
			}
		case <-detail:
			if d := m.detailDelta(); d != nil && m.send(conn, d) != nil {
				return
			}
		case <-m.poke:
		}
	}
}

func (m *MasterUnit) nextAlarm() time.Duration {
	mean := time.Hour.Seconds() / m.cfg.AlarmsPerHour
	return time.Duration(rand.ExpFloat64() * mean * float64(time.Second))
}

func (m *MasterUnit) dropAckedLocked(seq uint64) {
	i := 0
	for i < len(m.outbox) && m.outbox[i].Seq <= seq {
		i++
	}
	m.outbox = m.outbox[i:]
	if len(m.outbox) == 0 {
		m.overflow = false
	}
}

func (m *MasterUnit) handle(conn *wsock.Conn, msg *proto.Message) {
	switch msg.T {
	case proto.TAck:
		m.cfg.Stats.Acks.Add(1)
		m.mu.Lock()
		m.dropAckedLocked(msg.Seq)
		m.mu.Unlock()
	case proto.TResync:
		m.cfg.Stats.Resyncs.Add(1)
		m.mu.Lock()
		var out *proto.Message
		switch msg.What {
		case "summary":
			out = m.fullSummaryLocked()
		case "alarms":
			out = m.fullAlarmsLocked()
		case "detail":
			out = m.fullDetailLocked()
		}
		m.mu.Unlock()
		if out != nil {
			_ = m.send(conn, out)
		}
	case proto.TDetailSub:
		m.mu.Lock()
		m.detailOn = msg.On
		m.detailInterval = max(200*time.Millisecond, time.Duration(msg.IntervalMs)*time.Millisecond)
		var out *proto.Message
		if msg.On {
			out = m.fullDetailLocked()
		}
		m.mu.Unlock()
		if out != nil {
			_ = m.send(conn, out)
		}
		select {
		case m.poke <- struct{}{}:
		default:
		}
	}
}

// ---------------------------------------------------------------- node detail

func (m *MasterUnit) ensureNodesLocked() {
	if m.nodes != nil {
		return
	}
	r := rand.New(rand.NewPCG(m.cfg.Seed, 7))
	types := []string{"Stratus", "Nimbus"}
	m.nodes = make([]nodeModel, m.cfg.Nodes)
	for i := range m.nodes {
		m.nodes[i] = nodeModel{ID: i + 1, Name: fmt.Sprintf("RN-%03d", i+1), Type: types[r.IntN(2)], Chain: i/16 + 1, Hop: i%16 + 1, Status: "online",
			TempC: round(36+r.Float64()*12, 0.1), RxDbm: round(-9+r.Float64()*4, 0.1), TxDbm: round(0.5+r.Float64()*2, 0.1),
			Vswr: round(1.05+r.Float64()*0.3, 0.01), PsuV: round(47.8+r.Float64()*0.4, 0.01), FW: []string{"4.2.1", "4.2.1", "4.1.7"}[r.IntN(3)]}
	}
}

func (m *MasterUnit) nodeStatusLocked(n *nodeModel) string {
	worst := 0
	prefix := "n" + strconv.Itoa(n.ID) + ":"
	for id, a := range m.active {
		if len(id) > len(prefix) && id[:len(prefix)] == prefix {
			worst = max(worst, proto.SeverityRank(a.Sev))
		}
	}
	switch {
	case worst >= 3:
		return "alarm"
	case worst > 0:
		return "degraded"
	}
	return "online"
}

func (m *MasterUnit) fullDetailLocked() *proto.Message {
	m.ensureNodesLocked()
	nodes := make([]json.RawMessage, len(m.nodes))
	for i := range m.nodes {
		m.nodes[i].Status = m.nodeStatusLocked(&m.nodes[i])
		nodes[i], _ = json.Marshal(&m.nodes[i])
	}
	m.detailRev++
	return &proto.Message{T: proto.TDetail, Full: true, Rev: m.detailRev, Nodes: nodes}
}

func (m *MasterUnit) detailDelta() *proto.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.detailOn {
		return nil
	}
	m.ensureNodesLocked()
	r := m.rng
	var nodes []json.RawMessage
	for k := 0; k < max(1, len(m.nodes)/10); k++ {
		n := &m.nodes[r.IntN(len(m.nodes))]
		n.TempC = round(n.TempC+r.NormFloat64()*0.3, 0.1)
		n.RxDbm = round(n.RxDbm+r.NormFloat64()*0.05, 0.1)
		n.Status = m.nodeStatusLocked(n)
		b, _ := json.Marshal(n)
		nodes = append(nodes, b)
	}
	base := m.detailRev
	m.detailRev++
	return &proto.Message{T: proto.TDetail, Base: base, Rev: m.detailRev, Nodes: nodes}
}
