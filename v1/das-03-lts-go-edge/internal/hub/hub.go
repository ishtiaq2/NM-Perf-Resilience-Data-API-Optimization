// Package hub implements the push channels of the contract (asyncapi.yaml):
// /api/ws/telemetry (JSON snapshot + deltas) and /api/ws/spectrum (binary frames).
//
// Design: every client has one writer goroutine that is WOKEN (never queued to).
// On wake-up it sends whatever brings the client to the latest state: the shared
// delta if it is exactly one revision behind, a combined catch-up delta if it
// is further behind, or the newest spectrum frame. A slow client therefore
// costs O(1) memory and never delays anyone else, and intermediate updates are
// conflated automatically.
package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"dasedge/internal/spectrum"
	"dasedge/internal/telemetry"
	"dasedge/internal/ws"
)

// Stats of a hub.
type Stats struct {
	Clients     int   `json:"clients"`
	Connections int64 `json:"connections"`
	Messages    int64 `json:"messages"`
	Snapshots   int64 `json:"snapshots"`
	CatchUps    int64 `json:"catchUps"`
	Rejected    int64 `json:"rejected"`
	WireBytes   int64 `json:"wireBytes"`
}

type counters struct{ connections, messages, snapshots, catchUps, rejected, closedBytes atomic.Int64 }

func hello(server, topic string, hbMs int) string {
	return `{"type":"hello","api":"das-v1","server":"` + server + `","topic":"` + topic + `","heartbeatMs":` + strconv.Itoa(hbMs) + `,"time":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `}`
}

func hb(rev uint64) string {
	return `{"type":"hb","time":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `,"rev":` + strconv.FormatUint(rev, 10) + `}`
}

// ---------------------------------------------------------------- telemetry

type tclient struct {
	c    *ws.Conn
	wake chan struct{}

	mu         sync.Mutex
	subscribed bool
	synced     bool   // client holds `rev`
	rev        uint64 // last revision sent
	resume     *uint64
	gen        uint64 // bumped by every "sub"; a stale send must not mark a new subscription synced
	lastSend   time.Time
}

type sharedDelta struct {
	ev *telemetry.PublishEvent
	pm *ws.PreparedMessage
}

type sharedSnap struct {
	rev uint64
	pm  *ws.PreparedMessage
}

// Telemetry is the /api/ws/telemetry hub.
type Telemetry struct {
	store      *telemetry.Store
	server     string
	heartbeat  time.Duration
	maxClients int
	log        *slog.Logger

	mu      sync.Mutex
	clients map[*tclient]struct{}
	last    atomic.Pointer[sharedDelta]
	snap    atomic.Pointer[sharedSnap]
	n       counters
}

// NewTelemetry wires the hub to the store.
func NewTelemetry(store *telemetry.Store, server string, heartbeat time.Duration, maxClients int, log *slog.Logger) *Telemetry {
	h := &Telemetry{store: store, server: server, heartbeat: heartbeat, maxClients: maxClients, log: log, clients: map[*tclient]struct{}{}}
	store.Subscribe(func(ev *telemetry.PublishEvent) {
		// Frame (and compress) the delta once; every up-to-date client gets these exact bytes.
		msg := append([]byte(`{"type":"delta",`), ev.DeltaJSON()[1:]...)
		h.last.Store(&sharedDelta{ev: ev, pm: ws.NewPreparedMessage(ws.TextMessage, msg, true)})
		h.wakeAll()
	})
	go h.ticker()
	return h
}

func (h *Telemetry) wakeAll() {
	h.mu.Lock()
	for c := range h.clients {
		select {
		case c.wake <- struct{}{}:
		default: // already pending: updates are coalesced
		}
	}
	h.mu.Unlock()
}

func (h *Telemetry) ticker() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	pings := 0
	for range t.C {
		pings++
		h.mu.Lock()
		for c := range h.clients {
			if pings%30 == 0 {
				go func(c *ws.Conn) { _ = c.Ping(nil) }(c.c)
			}
			select {
			case c.wake <- struct{}{}: // lets idle writers send "hb"
			default:
			}
		}
		h.mu.Unlock()
	}
}

func (h *Telemetry) snapshotMessage(v *telemetry.View) *ws.PreparedMessage {
	if s := h.snap.Load(); s != nil && s.rev == v.Rev {
		return s.pm
	}
	msg := append([]byte(`{"type":"snapshot",`), v.SnapshotJSON()[1:]...)
	pm := ws.NewPreparedMessage(ws.TextMessage, msg, true)
	h.snap.Store(&sharedSnap{rev: v.Rev, pm: pm})
	return pm
}

// ServeHTTP upgrades and serves one client.
func (h *Telemetry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	full := len(h.clients) >= h.maxClients
	h.mu.Unlock()
	if full {
		h.n.rejected.Add(1)
		http.Error(w, "too many clients", http.StatusServiceUnavailable)
		return
	}
	c, err := ws.Upgrade(w, r, ws.Options{EnableCompression: true, MaxMessageSize: 16 << 10})
	if err != nil {
		h.n.rejected.Add(1)
		return
	}
	cl := &tclient{c: c, wake: make(chan struct{}, 1)}
	h.mu.Lock()
	h.clients[cl] = struct{}{}
	h.mu.Unlock()
	h.n.connections.Add(1)
	defer func() {
		h.mu.Lock()
		delete(h.clients, cl)
		h.mu.Unlock()
		h.n.closedBytes.Add(c.BytesWritten())
		c.CloseNow()
	}()
	_ = c.WriteJSONText(hello(h.server, "volatile", int(h.heartbeat/time.Millisecond)))
	go h.writer(cl)
	for {
		_, data, err := c.ReadMessage(75 * time.Second) // pings every 30 s keep healthy peers talking
		if err != nil {
			return
		}
		var m struct {
			Type string  `json:"type"`
			Rev  *uint64 `json:"rev"`
			ID   *int64  `json:"id"`
		}
		if json.Unmarshal(data, &m) != nil {
			_ = c.WriteJSONText(`{"type":"error","code":"BAD_REQUEST","message":"expected a JSON object with a type"}`)
			continue
		}
		switch m.Type {
		case "sub":
			cl.mu.Lock()
			cl.subscribed, cl.synced, cl.resume = true, false, m.Rev
			cl.gen++
			cl.mu.Unlock()
			select {
			case cl.wake <- struct{}{}:
			default:
			}
		case "ping":
			id := int64(0)
			if m.ID != nil {
				id = *m.ID
			}
			_ = c.WriteJSONText(`{"type":"pong","id":` + strconv.FormatInt(id, 10) + `,"time":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `}`)
		default:
			_ = c.WriteJSONText(`{"type":"error","code":"BAD_REQUEST","message":"unknown type"}`)
		}
	}
}

func (h *Telemetry) writer(cl *tclient) {
	for {
		select {
		case <-cl.c.Done():
			return
		case <-cl.wake:
		}
		cl.mu.Lock()
		subscribed, synced, rev, resume, lastSend, gen := cl.subscribed, cl.synced, cl.rev, cl.resume, cl.lastSend, cl.gen
		cl.mu.Unlock()
		v := h.store.View()
		var err error
		sent := false
		sentRev := rev // revision the client holds after this send
		switch {
		case !subscribed:
		case !synced:
			if resume != nil {
				if d, at, ok := h.store.DeltaJSON(*resume); ok {
					err = cl.c.WriteMessage(ws.TextMessage, append([]byte(`{"type":"delta",`), d[1:]...))
					h.n.catchUps.Add(1)
					sent, sentRev = true, at
				}
			}
			if !sent {
				err = cl.c.WritePrepared(h.snapshotMessage(v))
				h.n.snapshots.Add(1)
				sent, sentRev = true, v.Rev
			}
		case rev < v.Rev:
			if s := h.last.Load(); s != nil && s.ev.Base == rev {
				err = cl.c.WritePrepared(s.pm) // shared bytes, compressed once for everyone
				sentRev = s.ev.Rev
			} else if d, at, ok := h.store.DeltaJSON(rev); ok {
				err = cl.c.WriteMessage(ws.TextMessage, append([]byte(`{"type":"delta",`), d[1:]...))
				h.n.catchUps.Add(1)
				sentRev = at
			} else {
				err = cl.c.WritePrepared(h.snapshotMessage(v))
				h.n.snapshots.Add(1)
				sentRev = v.Rev
			}
			sent = true
		case time.Since(lastSend) >= h.heartbeat:
			err = cl.c.WriteJSONText(hb(v.Rev))
			sent = true
		}
		if err != nil {
			return
		}
		if sent {
			h.n.messages.Add(1)
			cl.mu.Lock()
			if subscribed && cl.gen == gen {
				cl.synced, cl.rev, cl.resume = true, sentRev, nil
			}
			cl.lastSend = time.Now()
			cl.mu.Unlock()
			if sentRev < h.store.View().Rev {
				select { // published meanwhile: go again
				case cl.wake <- struct{}{}:
				default:
				}
			}
		}
	}
}

// Close says goodbye (1001 Going Away) to every client, on shutdown.
func (h *Telemetry) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		_ = c.c.Close(ws.CloseGoingAway, "server shutting down")
	}
}

// Stats for /api/metrics.
func (h *Telemetry) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	wire := h.n.closedBytes.Load()
	for c := range h.clients {
		wire += c.c.BytesWritten()
	}
	return Stats{Clients: len(h.clients), Connections: h.n.connections.Load(), Messages: h.n.messages.Load(), Snapshots: h.n.snapshots.Load(),
		CatchUps: h.n.catchUps.Load(), Rejected: h.n.rejected.Load(), WireBytes: wire}
}

// ---------------------------------------------------------------- spectrum

type sclient struct {
	c    *ws.Conn
	wake chan struct{}

	mu        sync.Mutex
	session   *spectrum.Session
	kind      string
	maxPoints int
	lastSent  uint32
	lastSend  time.Time
}

// Spectrum is the /api/ws/spectrum hub.
type Spectrum struct {
	mgr           *spectrum.Manager
	server        string
	defaultPoints int
	heartbeat     time.Duration
	maxClients    int

	mu      sync.Mutex
	clients map[*sclient]struct{}
	n       counters
	dropped atomic.Int64
}

// NewSpectrum wires the hub to the sweep sessions.
func NewSpectrum(mgr *spectrum.Manager, server string, defaultPoints int, heartbeat time.Duration, maxClients int) *Spectrum {
	h := &Spectrum{mgr: mgr, server: server, defaultPoints: defaultPoints, heartbeat: heartbeat, maxClients: maxClients, clients: map[*sclient]struct{}{}}
	mgr.OnSweep(func(s *spectrum.Session, _ *spectrum.Sweep) {
		h.mu.Lock()
		for c := range h.clients {
			c.mu.Lock()
			mine := c.session == s
			c.mu.Unlock()
			if !mine {
				continue
			}
			select {
			case c.wake <- struct{}{}:
			default:
				h.dropped.Add(1) // client still busy with the previous frame: newest sweep wins
			}
		}
		h.mu.Unlock()
	})
	go func() {
		t := time.NewTicker(time.Second)
		for range t.C {
			h.mu.Lock()
			for c := range h.clients {
				c.mu.Lock()
				if c.session != nil {
					c.session.Touch() // an open analyzer view keeps its session sweeping
				}
				c.mu.Unlock()
				select {
				case c.wake <- struct{}{}:
				default:
				}
			}
			h.mu.Unlock()
		}
	}()
	return h
}

// ServeHTTP upgrades and serves one analyzer view.
func (h *Spectrum) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	full := len(h.clients) >= h.maxClients
	h.mu.Unlock()
	if full {
		h.n.rejected.Add(1)
		http.Error(w, "too many clients", http.StatusServiceUnavailable)
		return
	}
	c, err := ws.Upgrade(w, r, ws.Options{EnableCompression: false, MaxMessageSize: 4096})
	if err != nil {
		h.n.rejected.Add(1)
		return
	}
	cl := &sclient{c: c, wake: make(chan struct{}, 1)}
	h.mu.Lock()
	h.clients[cl] = struct{}{}
	h.mu.Unlock()
	h.n.connections.Add(1)
	defer func() {
		h.mu.Lock()
		delete(h.clients, cl)
		h.mu.Unlock()
		h.n.closedBytes.Add(c.BytesWritten())
		c.CloseNow()
	}()
	_ = c.WriteJSONText(hello(h.server, "spectrum", int(h.heartbeat/time.Millisecond)))
	go h.writer(cl)
	for {
		_, data, err := c.ReadMessage(75 * time.Second)
		if err != nil {
			return
		}
		var m struct {
			Type      string  `json:"type"`
			NodeID    int     `json:"nodeId"`
			Port      int     `json:"port"`
			StartHz   float64 `json:"startHz"`
			StopHz    float64 `json:"stopHz"`
			Points    int     `json:"points"`
			MaxPoints int     `json:"maxPoints"`
			Encoding  string  `json:"encoding"`
		}
		if json.Unmarshal(data, &m) != nil {
			_ = c.WriteJSONText(`{"type":"error","code":"BAD_REQUEST"}`)
			continue
		}
		switch m.Type {
		case "sub":
			p, perr := spectrum.ParseParams(strconv.Itoa(m.NodeID), strconv.Itoa(m.Port), fmtHz(m.StartHz), fmtHz(m.StopHz), strconv.Itoa(m.Points), h.defaultPoints)
			if perr != nil {
				_ = c.WriteJSONText(`{"type":"error","code":"BAD_REQUEST","message":` + strconv.Quote(perr.Error()) + `}`)
				continue
			}
			s, serr := h.mgr.Session(p)
			if serr != nil {
				_ = c.WriteJSONText(`{"type":"error","code":"TOO_MANY_SESSIONS"}`)
				continue
			}
			kind := spectrum.KindI16
			if m.Encoding == "f32" {
				kind = spectrum.KindF32
			}
			mp := spectrum.NormaliseMaxPoints(m.MaxPoints)
			cl.mu.Lock()
			cl.session, cl.kind, cl.maxPoints, cl.lastSent = s, kind, mp, 0
			cl.mu.Unlock()
			_ = c.WriteJSONText(`{"type":"subscribed","nodeId":` + strconv.Itoa(int(p.NodeID)) + `,"port":` + strconv.Itoa(int(p.Port)) + `,"maxPoints":` + strconv.Itoa(mp) + `}`)
			select {
			case cl.wake <- struct{}{}:
			default:
			}
		case "unsub":
			cl.mu.Lock()
			cl.session = nil
			cl.mu.Unlock()
		}
	}
}

func fmtHz(v float64) string {
	if v <= 0 {
		return ""
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func (h *Spectrum) writer(cl *sclient) {
	for {
		select {
		case <-cl.c.Done():
			return
		case <-cl.wake:
		}
		cl.mu.Lock()
		s, kind, mp, last, lastSend := cl.session, cl.kind, cl.maxPoints, cl.lastSent, cl.lastSend
		cl.mu.Unlock()
		var err error
		if s != nil {
			if sw := s.Latest(); sw != nil && sw.ID != last {
				v := sw.Variant(h.mgr, kind, mp, false)
				err = cl.c.WritePrepared(ws.NewPreparedMessage(ws.BinaryMessage, v.Body, false))
				h.n.messages.Add(1)
				cl.mu.Lock()
				if cl.session == s {
					cl.lastSent = sw.ID
				}
				cl.lastSend = time.Now()
				cl.mu.Unlock()
				if err != nil {
					return
				}
				continue
			}
		}
		if time.Since(lastSend) >= h.heartbeat {
			if err = cl.c.WriteJSONText(hb(0)); err != nil {
				return
			}
			cl.mu.Lock()
			cl.lastSend = time.Now()
			cl.mu.Unlock()
		}
	}
}

// Close says goodbye (1001 Going Away) to every client, on shutdown.
func (h *Spectrum) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		_ = c.c.Close(ws.CloseGoingAway, "server shutting down")
	}
}

// Stats for /api/metrics.
func (h *Spectrum) Stats() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	wire := h.n.closedBytes.Load()
	for c := range h.clients {
		wire += c.c.BytesWritten()
	}
	return map[string]any{"clients": len(h.clients), "connections": h.n.connections.Load(), "frames": h.n.messages.Load(), "conflated": h.dropped.Load(), "rejected": h.n.rejected.Load(), "wireBytes": wire}
}
