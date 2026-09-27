// Package dash pushes the fleet to NOC dashboards over WebSocket.
//
// Pull-based conflation: a change only wakes a client's writer, and the writer then
// reads the *current* state from the store: the overview at most once a second,
// the site under drill-down at most twice a second, new alarm events at once, then
// in batches at most every 50 ms. An alarm storm of 20 000 events is a few dozen
// messages per dashboard, not 20 000; a slow dashboard falls behind by skipping
// intermediate states, never by queueing them. If it falls behind the alarm log
// itself, it gets a gap marker and reloads the active alarms.
package dash

import (
	"encoding/json"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"dasnoc/internal/fleet"
	"dasnoc/internal/proto"
	"dasnoc/internal/wsock"
)

// Subprotocol of the dashboard WebSocket.
const Subprotocol = "das-noc-dash.v1"

// Config of the hub.
type Config struct {
	OverviewInterval time.Duration // default 1 s
	SiteInterval     time.Duration // default 500 ms
	AlarmInterval    time.Duration // default 50 ms: at most 20 alarm messages per second per dashboard
	MaxBatch         int           // events per message (default 500)
	Keepalive        time.Duration // default 30 s
	MaxClients       int           // default 1 000
}

func (c *Config) defaults() {
	if c.OverviewInterval <= 0 {
		c.OverviewInterval = time.Second
	}
	if c.SiteInterval <= 0 {
		c.SiteInterval = 500 * time.Millisecond
	}
	if c.AlarmInterval <= 0 {
		c.AlarmInterval = 50 * time.Millisecond
	}
	if c.MaxBatch <= 0 {
		c.MaxBatch = 500
	}
	if c.Keepalive <= 0 {
		c.Keepalive = 30 * time.Second
	}
	if c.MaxClients <= 0 {
		c.MaxClients = 1000
	}
}

// Metrics of the hub.
type Metrics struct {
	Clients                                   atomic.Int64
	Messages, Bytes, Gaps, Rejected, Wakeups  atomic.Uint64
	OverviewMsgs, AlarmMsgs, SiteMsgs, Events atomic.Uint64
}

// Hub serves /api/ws/dashboard.
type Hub struct {
	cfg    Config
	store  *fleet.Store
	verify *proto.Verifier
	log    *slog.Logger
	M      Metrics

	mu      sync.Mutex
	clients map[*client]struct{}
	bySite  map[string]map[*client]struct{}
}

// New creates the hub and subscribes it to the store's changes.
func New(cfg Config, store *fleet.Store, verify *proto.Verifier, log *slog.Logger) *Hub {
	cfg.defaults()
	h := &Hub{cfg: cfg, store: store, verify: verify, log: log, clients: map[*client]struct{}{}, bySite: map[string]map[*client]struct{}{}}
	store.OnChange(h.notify)
	return h
}

type subscription struct {
	Overview bool   `json:"overview"`
	Alarms   bool   `json:"alarms"`
	After    uint64 `json:"after"` // resume the alarm stream after this event (0 = only new events)
	Site     string `json:"site"`  // drill-down: one site's detail, node table included
}

type client struct {
	h      *Hub
	conn   *wsock.Conn
	claims proto.Claims
	wake   chan struct{}

	mu      sync.Mutex
	sub     subscription
	cursor  uint64
	resend  bool   // a new subscription: send the current state right away
	release func() // stops watching the drill-down site

	// Owned by the writer goroutine.
	draining     bool // an alarm backlog is being sent in back-to-back batches
	lastOverview uint64
	lastSite     uint64
	sentOverview time.Time
	sentSite     time.Time
	sentAlarms   time.Time
}

func (c *client) poke() {
	select {
	case c.wake <- struct{}{}:
	default: // already woken: the writer reads the current state anyway
	}
}

func (h *Hub) notify(kind fleet.ChangeKind, site string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if kind == fleet.ChangeSite {
		for c := range h.bySite[site] {
			c.poke()
		}
		return
	}
	for c := range h.clients {
		c.poke()
	}
}

// ServeHTTP upgrades an authenticated dashboard.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	claims, err := h.verify.Verify(proto.TokenFrom(r.Header.Get("Authorization"), wsock.Subprotocols(r)))
	if err != nil || claims.Kind != proto.KindUser {
		h.M.Rejected.Add(1)
		http.Error(w, "user token required", http.StatusUnauthorized)
		return
	}
	if h.M.Clients.Load() >= int64(h.cfg.MaxClients) {
		h.M.Rejected.Add(1)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many dashboards", http.StatusServiceUnavailable)
		return
	}
	conn, err := wsock.Upgrade(w, r, wsock.Options{Subprotocols: []string{Subprotocol}, EnableCompression: true, MaxMessageSize: 16 << 10, WriteTimeout: 10 * time.Second})
	if err != nil {
		return
	}
	c := &client{h: h, conn: conn, claims: claims, wake: make(chan struct{}, 1), cursor: h.store.LastEvent()}
	go c.run() // the handler returns: the HTTP server's per-connection buffers are released
}

// run is the reader side of one dashboard: subscriptions and acknowledgements.
func (c *client) run() {
	h, conn, claims := c.h, c.conn, c.claims
	h.M.Clients.Add(1)
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.unindex(c)
		h.mu.Unlock()
		c.mu.Lock()
		if c.release != nil {
			c.release()
		}
		c.mu.Unlock()
		h.M.Clients.Add(-1)
		conn.CloseNow()
	}()
	go c.writer()
	hello := map[string]any{"t": "hello", "user": claims.Subject, "tenant": claims.Tenant, "lastEvent": c.cursor, "server": "das-noc"}
	c.send(hello)
	for {
		_, raw, err := conn.ReadMessage(3 * h.cfg.Keepalive)
		if err != nil {
			return
		}
		var m struct {
			T string `json:"t"`
			subscription
			ID   string `json:"id"`
			Note string `json:"note"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.T {
		case "sub":
			c.subscribe(m.subscription)
		case "ack":
			if err := h.store.Acknowledge(claims, m.Site, m.ID, m.Note); err != nil {
				c.send(map[string]any{"t": "error", "message": err.Error()})
			}
		}
	}
}

func (h *Hub) unindex(c *client) {
	if c.sub.Site == "" {
		return
	}
	if set := h.bySite[c.sub.Site]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.bySite, c.sub.Site)
		}
	}
}

func (c *client) subscribe(s subscription) {
	h := c.h
	if s.Site != "" && h.store.SiteVersion(c.claims, s.Site) == 0 {
		c.send(map[string]any{"t": "error", "message": "site " + s.Site + " not found"})
		s.Site = ""
	}
	h.mu.Lock()
	c.mu.Lock()
	h.unindex(c)
	old := c.sub.Site
	c.sub = s
	if s.After > 0 {
		c.cursor = s.After
	}
	c.resend = true
	if s.Site != "" {
		if h.bySite[s.Site] == nil {
			h.bySite[s.Site] = map[*client]struct{}{}
		}
		h.bySite[s.Site][c] = struct{}{}
	}
	var release func()
	if old != s.Site {
		release, c.release = c.release, nil
	}
	c.mu.Unlock()
	h.mu.Unlock()
	if release != nil {
		release()
	}
	if s.Site != "" && old != s.Site {
		rel, _ := h.store.WatchDetail(s.Site) // the device streams its node table while someone looks
		c.mu.Lock()
		c.release = rel
		c.mu.Unlock()
	}
	c.poke()
}

func (c *client) send(v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return c.sendRaw(b)
}

func (c *client) sendRaw(b []byte) bool {
	if c.conn.WriteText(b) != nil {
		return false
	}
	c.h.M.Messages.Add(1)
	c.h.M.Bytes.Add(uint64(len(b)))
	return true
}

// alarmsMessage assembles {"t":"alarms","cursor":n,"events":[...]} from events
// that were encoded once, when they were logged.
func alarmsMessage(raws []json.RawMessage, cursor uint64) []byte {
	size := 48
	for _, r := range raws {
		size += len(r) + 1
	}
	b := make([]byte, 0, size)
	b = append(b, `{"t":"alarms","cursor":`...)
	b = strconv.AppendUint(b, cursor, 10)
	b = append(b, `,"events":[`...)
	for i, r := range raws {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, r...)
	}
	return append(b, "]}"...)
}

// writer is the only goroutine that sends state to this dashboard.
func (c *client) writer() {
	h := c.h
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	ping := time.NewTicker(h.cfg.Keepalive + rand.N(time.Second))
	defer ping.Stop()
	for {
		select {
		case <-c.conn.Done():
			return
		case <-ping.C:
			_ = c.conn.Ping(nil)
			continue
		case <-c.wake:
		case <-timer.C:
		}
		h.M.Wakeups.Add(1)
		if next := c.flush(time.Now()); next > 0 {
			timer.Reset(next) // something is pending but not due yet
		}
	}
}

// flush sends what is due and returns how long until the next pending item is due (0: nothing pending).
func (c *client) flush(now time.Time) time.Duration {
	h := c.h
	c.mu.Lock()
	sub, cursor, resend := c.sub, c.cursor, c.resend
	c.resend = false
	c.mu.Unlock()
	if resend {
		c.lastOverview, c.lastSite = math.MaxUint64, math.MaxUint64 // "never sent": differs from any version
		c.sentOverview, c.sentSite = time.Time{}, time.Time{}
	}
	var wait time.Duration
	due := func(last time.Time, every time.Duration) (bool, time.Duration) {
		if d := last.Add(every).Sub(now); d > 0 {
			return false, d
		}
		return true, 0
	}
	pending := func(d time.Duration) {
		if wait == 0 || d < wait {
			wait = d
		}
	}

	if sub.Alarms && h.store.LastEvent() > cursor {
		// The interval coalesces a trickle of events into one message; it does not
		// throttle a backlog: a storm is drained in back-to-back batches (a few per
		// flush, so overview and drill-down updates still get their turn).
		if ok, d := due(c.sentAlarms, h.cfg.AlarmInterval); ok || c.draining {
			next := cursor
			for batch := 0; batch < 8; batch++ {
				raws, n, gap := h.store.EventsSinceRaw(c.claims, next, h.cfg.MaxBatch)
				if gap {
					h.M.Gaps.Add(1)
					c.send(map[string]any{"t": "gap", "cursor": n})
				}
				if len(raws) > 0 && c.sendRaw(alarmsMessage(raws, n)) {
					h.M.AlarmMsgs.Add(1)
					h.M.Events.Add(uint64(len(raws)))
				}
				next = n
				if len(raws) < h.cfg.MaxBatch {
					break
				}
			}
			c.mu.Lock()
			if c.cursor == cursor {
				c.cursor = next
			}
			c.mu.Unlock()
			c.sentAlarms = now
			c.draining = h.store.LastEvent() > next
			if c.draining {
				pending(time.Millisecond) // continue right after the other topics
			}
		} else {
			pending(d)
		}
	}
	if sub.Overview {
		if v := h.store.Version(); v != c.lastOverview {
			if ok, d := due(c.sentOverview, h.cfg.OverviewInterval); ok {
				o := h.store.Overview(c.claims)
				if c.send(map[string]any{"t": "overview", "overview": o}) {
					h.M.OverviewMsgs.Add(1)
				}
				c.lastOverview, c.sentOverview = o.Version, now
			} else {
				pending(d)
			}
		}
	}
	if sub.Site != "" {
		if v := h.store.SiteVersion(c.claims, sub.Site); v != c.lastSite && v != 0 {
			if ok, d := due(c.sentSite, h.cfg.SiteInterval); ok {
				if d, ok := h.store.Site(c.claims, sub.Site); ok && c.send(map[string]any{"t": "site", "site": d}) {
					h.M.SiteMsgs.Add(1)
					c.lastSite = d.Version
				}
				c.sentSite = now
			} else {
				pending(d)
			}
		}
	}
	return wait
}

// Stats for /internal/metrics.
func (h *Hub) Stats() map[string]any {
	return map[string]any{
		"clients": h.M.Clients.Load(), "messages": h.M.Messages.Load(), "bytes": h.M.Bytes.Load(),
		"overviewMessages": h.M.OverviewMsgs.Load(), "alarmMessages": h.M.AlarmMsgs.Load(), "siteMessages": h.M.SiteMsgs.Load(),
		"events": h.M.Events.Load(), "gaps": h.M.Gaps.Load(), "rejected": h.M.Rejected.Load(), "wakeups": h.M.Wakeups.Load(),
	}
}

// DropRevoked closes the dashboards of users whose token was revoked after they
// connected. (REST requests check every token on every request anyway.)
func (h *Hub) DropRevoked() int {
	h.mu.Lock()
	var drop []*client
	for c := range h.clients {
		if h.verify.IsRevoked(c.claims.Kind, c.claims.Subject) {
			drop = append(drop, c)
		}
	}
	h.mu.Unlock()
	for _, c := range drop {
		_ = c.conn.Close(wsock.ClosePolicyViolation, "token revoked")
	}
	return len(drop)
}

// Shutdown closes every dashboard (they reconnect to the next instance).
func (h *Hub) Shutdown() {
	h.mu.Lock()
	all := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		all = append(all, c)
	}
	h.mu.Unlock()
	for _, c := range all {
		_ = c.conn.Close(1012, "NOC restart")
	}
}
