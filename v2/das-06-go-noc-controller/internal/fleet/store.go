package fleet

import (
	"encoding/json"
	"maps"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"dasnoc/internal/proto"
)

// Config of the store.
type Config struct {
	// Grace: a disconnected site is "stale" this long, then "offline" (and a
	// SITE_UNREACHABLE alarm is raised). Short network blips and NOC restarts stay quiet.
	Grace time.Duration
	// LogCapacity: alarm events kept for dashboards that fall behind.
	LogCapacity int
	// ResyncMinInterval: at most one resync request of each kind per site in this time,
	// so a device whose hash disagrees with the NOC cannot cause a resync loop.
	ResyncMinInterval time.Duration
	// DetailLinger: detail streaming continues this long after the last viewer left.
	DetailLinger time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

// ChangeKind tells a listener what changed.
type ChangeKind int

const (
	ChangeFleet ChangeKind = iota // counts, statuses, the problem list
	ChangeAlarm                   // new alarm log events
	ChangeSite                    // one site's view (summary, alarms, detail)
)

// Counters are cumulative totals for metrics.
type Counters struct {
	Summaries, Deltas, AlarmMessages, AlarmEvents, DuplicateEvents, DetailMessages atomic.Uint64
	ResyncSummary, ResyncAlarms, ResyncDetail, HashMismatch, Takeovers             atomic.Uint64
}

type contribution struct {
	counted   bool
	tenant    string
	connected bool
	status    Status
	alarms    SevCounts
	acked     int
}

type aggregate struct {
	Sites     int          `json:"sites"`
	Connected int          `json:"connected"`
	ByStatus  StatusCounts `json:"byStatus"`
	Alarms    SevCounts    `json:"alarms"`
	Acked     int          `json:"acknowledged"`
}

func (a *aggregate) add(c contribution, sign int) {
	if !c.counted {
		return
	}
	a.Sites += sign
	if c.connected {
		a.Connected += sign
	}
	a.ByStatus[c.status] += sign
	for i := range c.alarms {
		a.Alarms[i] += sign * c.alarms[i]
	}
	a.Acked += sign * c.acked
}

// Detail is the node-level data of a site under drill-down.
type Detail struct {
	Rev       uint64
	UpdatedAt time.Time
	Nodes     map[int]json.RawMessage
}

type ackRecord struct {
	By       string `json:"by"`
	At       int64  `json:"at"`
	Note     string `json:"note,omitempty"`
	RaisedAt int64  `json:"raisedAt"`
}

// Site is one Master Unit.
type Site struct {
	mu     sync.Mutex
	ID     string
	tenant string

	inventory      bool // listed in the inventory: shown even if never seen
	connected      bool
	everConnected  bool
	session        uint64
	remote         string
	agent, fw      string
	connectedAt    time.Time
	disconnectedAt time.Time
	lastSeen       time.Time

	boot    string
	rev     uint64
	hash    string
	summary map[string]json.RawMessage
	stats   Stats

	ackSeq    uint64
	active    map[string]*Alarm // reported by the device
	nocAlarms map[string]*Alarm // raised by the NOC (SITE_UNREACHABLE)
	acks      map[string]ackRecord

	status  Status
	contrib contribution
	ver     uint64

	detail       *Detail
	watchers     int
	detailOn     bool
	detailLease  time.Time
	detailIdle   time.Time
	lastResync   [3]time.Time
	hashMismatch uint64
}

func newSite(id, tenant string) *Site {
	s := &Site{ID: id, tenant: tenant, active: map[string]*Alarm{}, nocAlarms: map[string]*Alarm{}, acks: map[string]ackRecord{}, status: StatusOffline}
	s.stats.MaxTempC, s.stats.MinRxDbm, s.stats.MaxVswr = nan(), nan(), nan() // unknown until the first summary
	return s
}

func (s *Site) sevCounts() (c SevCounts, acked int) {
	for _, set := range [2]map[string]*Alarm{s.active, s.nocAlarms} {
		for _, a := range set {
			if i := sevIndex(a.Sev); i >= 0 {
				c[i]++
			}
			if a.AckBy != "" {
				acked++
			}
		}
	}
	return c, acked
}

func (s *Site) deriveStatus(now time.Time, grace time.Duration) Status {
	if !s.connected {
		if s.everConnected && now.Sub(s.disconnectedAt) < grace {
			return StatusStale
		}
		return StatusOffline
	}
	c, _ := s.sevCounts()
	switch {
	case c[SevCritical] > 0:
		return StatusCritical
	case c[SevMajor] > 0:
		return StatusMajor
	case c[SevMinor] > 0:
		return StatusMinor
	case c[SevWarning] > 0:
		return StatusWarning
	}
	return StatusOK
}

// Store is the fleet.
type Store struct {
	cfg Config

	mu    sync.RWMutex
	sites map[string]*Site

	aggMu   sync.Mutex
	agg     aggregate
	tenants map[string]*aggregate

	version atomic.Uint64
	log     *alarmLog

	listener   atomic.Pointer[func(ChangeKind, string)]
	detailSink atomic.Pointer[func(site string, on bool)]

	ovMu    sync.Mutex
	ovCache map[string]*Overview

	C Counters
}

// New creates an empty fleet.
func New(cfg Config) *Store {
	if cfg.Grace <= 0 {
		cfg.Grace = 90 * time.Second
	}
	if cfg.LogCapacity <= 0 {
		cfg.LogCapacity = 50_000
	}
	if cfg.ResyncMinInterval <= 0 {
		cfg.ResyncMinInterval = 30 * time.Second
	}
	if cfg.DetailLinger <= 0 {
		cfg.DetailLinger = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Store{cfg: cfg, sites: map[string]*Site{}, tenants: map[string]*aggregate{}, log: newAlarmLog(cfg.LogCapacity), ovCache: map[string]*Overview{}}
}

// OnChange registers the listener (the dashboard hub). It is called without locks held.
func (st *Store) OnChange(f func(kind ChangeKind, site string)) { st.listener.Store(&f) }

// OnDetail registers the function that asks a device to start or stop its detail stream.
func (st *Store) OnDetail(f func(site string, on bool)) { st.detailSink.Store(&f) }

func (st *Store) emit(kind ChangeKind, site string) {
	if f := st.listener.Load(); f != nil {
		(*f)(kind, site)
	}
}

func (st *Store) sendDetail(site string, on bool) {
	if f := st.detailSink.Load(); f != nil {
		(*f)(site, on)
	}
}

// Version increases whenever the fleet overview may have changed.
func (st *Store) Version() uint64 { return st.version.Load() }

// LastEvent is the N of the newest alarm log event.
func (st *Store) LastEvent() uint64 { return st.log.last() }

func (st *Store) get(id string) *Site {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.sites[id]
}

func (st *Store) getOrCreate(id, tenant string) *Site {
	if s := st.get(id); s != nil {
		return s
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if s := st.sites[id]; s != nil {
		return s
	}
	s := newSite(id, tenant)
	st.sites[id] = s
	return s
}

// Len is the number of known sites.
func (st *Store) Len() int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return len(st.sites)
}

func (st *Store) all() []*Site {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]*Site, 0, len(st.sites))
	for _, s := range st.sites {
		out = append(out, s)
	}
	return out
}

// mutation collects what a change to one site did.
type mutation struct {
	changed bool    // the site's own view changed
	fleet   bool    // something in the fleet overview changed (name, for instance)
	events  []Event // alarm log events to append
	detail  int     // +1: ask the device for detail, -1: stop it
}

// mutate runs fn under the site lock, then updates the derived status, the fleet
// and tenant aggregates, the alarm log, and tells the listener, without holding
// any lock during the callbacks.
func (st *Store) mutate(s *Site, fn func(s *Site, m *mutation)) {
	var m mutation
	s.mu.Lock()
	fn(s, &m)
	now := st.cfg.Now()
	s.status = s.deriveStatus(now, st.cfg.Grace)
	alarms, acked := s.sevCounts()
	c := contribution{counted: true, tenant: s.tenant, connected: s.connected, status: s.status, alarms: alarms, acked: acked}
	fleetChanged := c != s.contrib || m.fleet
	if c != s.contrib {
		st.aggMu.Lock()
		st.agg.add(s.contrib, -1)
		st.agg.add(c, +1)
		if s.contrib.counted {
			st.tenant(s.contrib.tenant).add(s.contrib, -1)
		}
		st.tenant(c.tenant).add(c, +1)
		st.aggMu.Unlock()
		s.contrib = c
	}
	for i := range m.events {
		m.events[i].Site, m.events[i].Tenant, m.events[i].SiteName = s.ID, s.tenant, s.stats.Name
		m.events[i].ReceivedAt = now.UnixMilli()
		m.events[i] = st.log.append(m.events[i])
	}
	if m.changed || fleetChanged || len(m.events) > 0 {
		s.ver++
	}
	id := s.ID
	s.mu.Unlock()

	if fleetChanged {
		st.version.Add(1)
		st.emit(ChangeFleet, "")
	}
	if len(m.events) > 0 {
		st.emit(ChangeAlarm, "")
	}
	if m.changed || fleetChanged || len(m.events) > 0 {
		st.emit(ChangeSite, id)
	}
	if m.detail != 0 {
		st.sendDetail(id, m.detail > 0)
	}
}

// tenant returns the aggregate of a tenant; aggMu must be held.
func (st *Store) tenant(t string) *aggregate {
	a := st.tenants[t]
	if a == nil {
		a = &aggregate{}
		st.tenants[t] = a
	}
	return a
}

// ---------------------------------------------------------------- device sessions

// Connect registers a session of a device whose identity comes from its token.
// It returns what the NOC already holds (nil: the device must send everything),
// the session it replaced, if any (the caller closes that connection), and whether
// the device should stream detail right away (someone is watching the site).
func (st *Store) Connect(id, tenant string, session uint64, remote string, hello *proto.Message) (have *proto.Have, replaced uint64, detail bool) {
	s := st.getOrCreate(id, tenant)
	st.mutate(s, func(s *Site, m *mutation) {
		now := st.cfg.Now()
		if s.connected && s.session != session {
			replaced = s.session
			st.C.Takeovers.Add(1)
		}
		if s.tenant != tenant { // re-provisioned to another customer
			s.tenant = tenant
			m.fleet = true
		}
		s.connected, s.everConnected = true, true
		s.session, s.remote = session, remote
		s.agent, s.fw = hello.Agent, hello.FW
		s.connectedAt, s.lastSeen = now, now
		if s.boot == hello.Boot && s.summary != nil {
			have = &proto.Have{Boot: s.boot, Rev: s.rev, Hash: s.hash, AckSeq: s.ackSeq}
		} else {
			// New device boot or NOC restart: the device sends its summary and its full
			// active alarm list next. Until then the last known values stay visible.
			s.boot, s.rev, s.ackSeq = hello.Boot, 0, 0
		}
		if a, ok := s.nocAlarms["SITE_UNREACHABLE"]; ok {
			delete(s.nocAlarms, "SITE_UNREACHABLE")
			m.events = append(m.events, Event{ID: a.ID, Code: a.Code, Sev: a.Sev, State: "cleared", At: now.UnixMilli(), Text: "connected again"})
		}
		s.detailOn = s.watchers > 0 || now.Before(s.detailLease)
		detail = s.detailOn // sent by the caller after the welcome
		m.changed = true
	})
	return have, replaced, detail
}

// Disconnect ends a session (ignored if a newer session took over).
func (st *Store) Disconnect(id string, session uint64) {
	s := st.get(id)
	if s == nil {
		return
	}
	st.mutate(s, func(s *Site, m *mutation) {
		if !s.connected || s.session != session {
			return
		}
		s.connected = false
		s.disconnectedAt = st.cfg.Now()
		s.detail, s.detailOn = nil, false
		m.changed = true
	})
}

// Seen records traffic from a device (keep-alive).
func (st *Store) Seen(id string, session uint64) {
	if s := st.get(id); s != nil {
		s.mu.Lock()
		if s.session == session {
			s.lastSeen = st.cfg.Now()
		}
		s.mu.Unlock()
	}
}

func (st *Store) mayResync(s *Site, what int) bool {
	now := st.cfg.Now()
	if now.Sub(s.lastResync[what]) < st.cfg.ResyncMinInterval {
		return false
	}
	s.lastResync[what] = now
	return true
}

const (
	resyncSummary = iota
	resyncAlarms
	resyncDetail
)

// ApplySummary stores a full summary.
func (st *Store) ApplySummary(id string, session uint64, msg *proto.Message) {
	s := st.get(id)
	if s == nil {
		return
	}
	st.C.Summaries.Add(1)
	st.mutate(s, func(s *Site, m *mutation) {
		if s.session != session || !s.connected {
			return
		}
		hash, _ := proto.SummaryHash(msg.S)
		if msg.Hash != "" && msg.Hash != hash {
			s.hashMismatch++
			st.C.HashMismatch.Add(1)
		}
		oldName := s.stats.Name
		s.summary = maps.Clone(msg.S)
		s.rev, s.hash = msg.Rev, hash
		s.stats.fromSummary(s.summary)
		s.lastSeen = st.cfg.Now()
		m.changed = true
		m.fleet = s.stats.Name != oldName
	})
}

// ApplyDelta applies changed summary fields; it returns true when the NOC needs a
// full summary instead (a gap in the revision chain, or a hash mismatch).
func (st *Store) ApplyDelta(id string, session uint64, msg *proto.Message) (resync bool) {
	s := st.get(id)
	if s == nil {
		return false
	}
	st.C.Deltas.Add(1)
	st.mutate(s, func(s *Site, m *mutation) {
		if s.session != session || !s.connected {
			return
		}
		s.lastSeen = st.cfg.Now()
		if s.summary == nil || msg.Base != s.rev {
			resync = st.mayResync(s, resyncSummary)
			return
		}
		oldName := s.stats.Name
		for k, v := range msg.S {
			if string(v) == "null" {
				delete(s.summary, k)
			} else {
				s.summary[k] = v
			}
		}
		s.rev = msg.Rev
		s.hash, _ = proto.SummaryHash(s.summary)
		s.stats.fromSummary(s.summary)
		m.changed = true
		m.fleet = s.stats.Name != oldName
		if msg.Hash != "" && msg.Hash != s.hash {
			s.hashMismatch++
			st.C.HashMismatch.Add(1)
			resync = st.mayResync(s, resyncSummary)
		}
	})
	if resync {
		st.C.ResyncSummary.Add(1)
	}
	return resync
}

func alarmFrom(ev *proto.AlarmEvent, now time.Time) *Alarm {
	return &Alarm{ID: ev.ID, Node: ev.Node, Code: ev.Code, Sev: ev.Sev, Text: ev.Text, RaisedAt: ev.At, ReceivedAt: now.UnixMilli(), Seq: ev.Seq, Source: "device"}
}

func (s *Site) restoreAck(a *Alarm) {
	if r, ok := s.acks[a.ID]; ok && r.RaisedAt == a.RaisedAt {
		a.AckBy, a.AckAt, a.AckNote = r.By, r.At, r.Note
	}
}

// ApplyAlarms processes alarm events. It returns the cumulative sequence to
// acknowledge, and whether the NOC needs the full active list (a gap).
func (st *Store) ApplyAlarms(id string, session uint64, msg *proto.Message) (ack uint64, resync bool) {
	s := st.get(id)
	if s == nil {
		return 0, false
	}
	st.C.AlarmMessages.Add(1)
	st.mutate(s, func(s *Site, m *mutation) {
		if s.session != session || !s.connected {
			ack = 0
			return
		}
		now := st.cfg.Now()
		s.lastSeen = now
		if msg.Full {
			next := make(map[string]*Alarm, len(msg.Ev))
			for i := range msg.Ev {
				ev := &msg.Ev[i]
				if ev.State != "raised" {
					continue
				}
				a := alarmFrom(ev, now)
				if old, ok := s.active[ev.ID]; ok && old.RaisedAt == ev.At {
					a.ReceivedAt, a.AckBy, a.AckAt, a.AckNote = old.ReceivedAt, old.AckBy, old.AckAt, old.AckNote
				} else {
					s.restoreAck(a)
					m.events = append(m.events, Event{ID: a.ID, Node: a.Node, Code: a.Code, Sev: a.Sev, State: "raised", Text: a.Text, At: a.RaisedAt})
				}
				next[ev.ID] = a
			}
			for aid, old := range s.active {
				if _, ok := next[aid]; !ok {
					delete(s.acks, aid)
					m.events = append(m.events, Event{ID: old.ID, Node: old.Node, Code: old.Code, Sev: old.Sev, State: "cleared", At: now.UnixMilli(), Text: "not active any more (full resync)"})
				}
			}
			s.active = next
			s.ackSeq = msg.UpTo
			st.C.AlarmEvents.Add(uint64(len(msg.Ev)))
			m.changed = true
			ack = s.ackSeq
			return
		}
		for i := range msg.Ev {
			ev := &msg.Ev[i]
			if ev.Seq <= s.ackSeq {
				st.C.DuplicateEvents.Add(1) // retransmitted after a reconnect: already stored
				continue
			}
			if ev.Seq != s.ackSeq+1 {
				resync = st.mayResync(s, resyncAlarms) // the device lost events (outbox overflow)
				break
			}
			s.ackSeq = ev.Seq
			st.C.AlarmEvents.Add(1)
			switch ev.State {
			case "raised":
				a := alarmFrom(ev, now)
				s.restoreAck(a)
				s.active[ev.ID] = a
				m.events = append(m.events, Event{ID: a.ID, Node: a.Node, Code: a.Code, Sev: a.Sev, State: "raised", Text: a.Text, At: a.RaisedAt})
			case "cleared":
				if old, ok := s.active[ev.ID]; ok {
					delete(s.active, ev.ID)
					delete(s.acks, ev.ID)
					m.events = append(m.events, Event{ID: old.ID, Node: old.Node, Code: old.Code, Sev: old.Sev, State: "cleared", Text: ev.Text, At: ev.At})
				}
			}
			m.changed = true
		}
		ack = s.ackSeq
	})
	if resync {
		st.C.ResyncAlarms.Add(1)
	}
	return ack, resync
}

// ApplyDetail stores node-level data; it returns true when a full detail is needed.
func (st *Store) ApplyDetail(id string, session uint64, msg *proto.Message) (resync bool) {
	s := st.get(id)
	if s == nil {
		return false
	}
	st.C.DetailMessages.Add(1)
	st.mutate(s, func(s *Site, m *mutation) {
		if s.session != session || !s.connected || !s.detailOn {
			return
		}
		now := st.cfg.Now()
		s.lastSeen = now
		if msg.Full || s.detail == nil {
			if !msg.Full {
				resync = st.mayResync(s, resyncDetail)
				return
			}
			s.detail = &Detail{Nodes: make(map[int]json.RawMessage, len(msg.Nodes))}
		} else if msg.Base != s.detail.Rev {
			resync = st.mayResync(s, resyncDetail)
			return
		}
		for _, raw := range msg.Nodes {
			var n struct {
				ID int `json:"id"`
			}
			if json.Unmarshal(raw, &n) == nil {
				s.detail.Nodes[n.ID] = raw
			}
		}
		for _, g := range msg.Gone {
			delete(s.detail.Nodes, g)
		}
		s.detail.Rev, s.detail.UpdatedAt = msg.Rev, now
		m.changed = true
	})
	if resync {
		st.C.ResyncDetail.Add(1)
	}
	return resync
}

// ---------------------------------------------------------------- drill-down

// WatchDetail marks a viewer of a site's node-level detail; call the returned
// function when the viewer leaves. The device streams detail while anyone watches.
func (st *Store) WatchDetail(id string) (release func(), ok bool) {
	s := st.get(id)
	if s == nil {
		return func() {}, false
	}
	st.mutate(s, func(s *Site, m *mutation) {
		s.watchers++
		if !s.detailOn && s.connected {
			s.detailOn = true
			m.detail = +1
		}
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			st.mutate(s, func(s *Site, m *mutation) {
				s.watchers--
				if s.watchers == 0 {
					s.detailIdle = st.cfg.Now()
				}
			})
		})
	}, true
}

// LeaseDetail keeps the detail stream on for d (REST clients poll without a socket).
func (st *Store) LeaseDetail(id string, d time.Duration) bool {
	s := st.get(id)
	if s == nil {
		return false
	}
	st.mutate(s, func(s *Site, m *mutation) {
		until := st.cfg.Now().Add(d)
		if until.After(s.detailLease) {
			s.detailLease = until
		}
		if !s.detailOn && s.connected {
			s.detailOn = true
			m.detail = +1
		}
	})
	return true
}

// WantsDetail reports whether the device should stream detail (after a reconnect).
func (st *Store) WantsDetail(id string) bool {
	s := st.get(id)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.detailOn
}

// Sweep runs periodically: stale sites become offline (with a SITE_UNREACHABLE
// alarm), and detail streams nobody watches any more are stopped.
func (st *Store) Sweep() {
	now := st.cfg.Now()
	unreachable := func(s *Site) bool {
		_, raised := s.nocAlarms["SITE_UNREACHABLE"]
		return !s.connected && s.everConnected && now.Sub(s.disconnectedAt) >= st.cfg.Grace && !raised
	}
	idleDetail := func(s *Site) bool {
		return s.detailOn && s.watchers == 0 && now.After(s.detailLease) && now.Sub(s.detailIdle) >= st.cfg.DetailLinger
	}
	for _, s := range st.all() {
		s.mu.Lock()
		act := unreachable(s) || idleDetail(s)
		s.mu.Unlock()
		if !act {
			continue
		}
		st.mutate(s, func(s *Site, m *mutation) {
			if unreachable(s) {
				a := &Alarm{ID: "SITE_UNREACHABLE", Code: "SITE_UNREACHABLE", Sev: "critical", Text: "no connection from the Master Unit", RaisedAt: now.UnixMilli(), ReceivedAt: now.UnixMilli(), Source: "noc"}
				s.restoreAck(a)
				s.nocAlarms[a.ID] = a
				m.events = append(m.events, Event{ID: a.ID, Code: a.Code, Sev: a.Sev, State: "raised", Text: a.Text, At: a.RaisedAt})
				m.changed = true
			}
			if idleDetail(s) {
				s.detailOn, s.detail = false, nil
				if s.connected {
					m.detail = -1
				}
			}
		})
	}
}

// ---------------------------------------------------------------- operators

// ErrNotFound: no such site or alarm (or not visible to the caller).
type ErrNotFound struct{ What string }

func (e ErrNotFound) Error() string { return e.What + " not found" }

// Acknowledge records that an operator has seen an active alarm.
func (st *Store) Acknowledge(who proto.Claims, siteID, alarmID, note string) error {
	s := st.get(siteID)
	if s == nil {
		return ErrNotFound{"site"}
	}
	var err error
	st.mutate(s, func(s *Site, m *mutation) {
		if !who.Sees(s.tenant) {
			err = ErrNotFound{"site"}
			return
		}
		a := s.active[alarmID]
		if a == nil {
			a = s.nocAlarms[alarmID]
		}
		if a == nil {
			err = ErrNotFound{"alarm"}
			return
		}
		now := st.cfg.Now().UnixMilli()
		if len(note) > 200 { // cut at a character boundary, never inside a UTF-8 sequence
			cut := 200
			for cut > 0 && !utf8.RuneStart(note[cut]) {
				cut--
			}
			note = note[:cut]
		}
		a.AckBy, a.AckAt, a.AckNote = who.Subject, now, note
		s.acks[alarmID] = ackRecord{By: who.Subject, At: now, Note: note, RaisedAt: a.RaisedAt}
		m.events = append(m.events, Event{ID: a.ID, Node: a.Node, Code: a.Code, Sev: a.Sev, State: "acknowledged", Text: note, At: now, By: who.Subject})
		m.changed = true
	})
	return err
}
