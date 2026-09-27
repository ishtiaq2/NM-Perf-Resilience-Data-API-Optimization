// Package uplink is the NOC end of the Master Unit connections: every Master Unit
// dials out to the NOC (no inbound firewall rule at the venue) and keeps one
// WebSocket open, over which it reports its site summary as deltas, its alarms
// with sequence numbers, and node-level detail while an operator drills down.
//
// Protecting the NOC from its own fleet matters as much as the happy path: after
// a NOC restart or a regional network event thousands of devices reconnect at
// once. Authenticated handshakes pass a token bucket (admission control); the rest
// are refused cheaply, before the upgrade, with a randomised Retry-After that
// spreads them out.
package uplink

import (
	"encoding/json"
	"errors"
	"log/slog"
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

// Config of the uplink endpoint.
type Config struct {
	Keepalive      time.Duration // NOC pings each device this often (default 25 s)
	IdleTimeout    time.Duration // no frame for this long: the device is gone (default 2.5 x keepalive)
	HelloTimeout   time.Duration // the device must say hello within (default 10 s)
	AdmitRate      float64       // handshakes per second (default 200)
	AdmitBurst     int           // bucket size (default 2 x rate)
	RetryAfterMax  int           // refused devices retry after 1..N seconds (default 15)
	MaxSessions    int           // concurrent devices (default 20 000)
	MaxMessageSize int64         // largest message from a device (default 1 MiB)
	DetailInterval time.Duration // requested detail update interval (default 1 s)
}

func (c *Config) defaults() {
	if c.Keepalive <= 0 {
		c.Keepalive = 25 * time.Second
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = c.Keepalive * 5 / 2
	}
	if c.HelloTimeout <= 0 {
		c.HelloTimeout = 10 * time.Second
	}
	if c.AdmitRate <= 0 {
		c.AdmitRate = 200
	}
	if c.AdmitBurst <= 0 {
		c.AdmitBurst = int(2 * c.AdmitRate)
	}
	if c.RetryAfterMax <= 0 {
		c.RetryAfterMax = 15
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = 20_000
	}
	if c.MaxMessageSize <= 0 {
		c.MaxMessageSize = 1 << 20
	}
	if c.DetailInterval <= 0 {
		c.DetailInterval = time.Second
	}
}

// Metrics are cumulative counters (Active is a gauge).
type Metrics struct {
	Handshakes, Accepted, RejectedAdmission, RejectedAuth, RejectedCapacity, RejectedProtocol atomic.Uint64
	Messages, BadMessages, Replaced, Pings                                                    atomic.Uint64
	Active                                                                                    atomic.Int64
	BytesIn, BytesOut                                                                         atomic.Uint64
}

type session struct {
	id     uint64
	site   string
	tenant string
	conn   *wsock.Conn
	ping   *time.Timer
}

func (s *session) send(m *proto.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.conn.WriteText(b)
}

// Server accepts Master Unit connections.
type Server struct {
	cfg    Config
	store  *fleet.Store
	verify *proto.Verifier
	log    *slog.Logger
	M      Metrics

	mu       sync.Mutex
	sessions map[string]*session
	nextID   atomic.Uint64
	closing  atomic.Bool

	admMu  sync.Mutex
	tokens float64
	last   time.Time
}

// New creates the uplink endpoint and connects it to the store's detail requests.
func New(cfg Config, store *fleet.Store, verify *proto.Verifier, log *slog.Logger) *Server {
	cfg.defaults()
	s := &Server{cfg: cfg, store: store, verify: verify, log: log, sessions: map[string]*session{}, tokens: float64(cfg.AdmitBurst), last: time.Now()}
	s.nextID.Store(uint64(time.Now().UnixNano()) & 0xFFFFFFFF << 16) // distinct across NOC restarts
	store.OnDetail(func(site string, on bool) {
		if sess := s.session(site); sess != nil {
			go func() { _ = sess.send(s.detailSub(on)) }()
		}
	})
	return s
}

func (s *Server) detailSub(on bool) *proto.Message {
	return &proto.Message{T: proto.TDetailSub, On: on, IntervalMs: int(s.cfg.DetailInterval / time.Millisecond)}
}

func (s *Server) session(site string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[site]
}

// admit takes one token from the handshake bucket.
func (s *Server) admit() bool {
	s.admMu.Lock()
	defer s.admMu.Unlock()
	now := time.Now()
	s.tokens = min(float64(s.cfg.AdmitBurst), s.tokens+now.Sub(s.last).Seconds()*s.cfg.AdmitRate)
	s.last = now
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

func (s *Server) refuse(w http.ResponseWriter, code int, msg string) {
	if code == http.StatusServiceUnavailable {
		// Randomised: 5 000 refused devices must not all come back in the same second.
		w.Header().Set("Retry-After", strconv.Itoa(1+rand.IntN(s.cfg.RetryAfterMax)))
	}
	http.Error(w, msg, code)
}

// ServeHTTP handles GET /uplink/v1 (the WebSocket handshake).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.M.Handshakes.Add(1)
	if s.closing.Load() {
		s.refuse(w, http.StatusServiceUnavailable, "NOC restarting")
		return
	}
	if !wsock.IsUpgrade(r) {
		s.M.RejectedProtocol.Add(1)
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return
	}
	// The token is checked first (an HMAC, about a microsecond), so requests without
	// a valid device token never use up admission slots meant for real devices.
	claims, err := s.verify.Verify(proto.TokenFrom(r.Header.Get("Authorization"), wsock.Subprotocols(r)))
	if err != nil || claims.Kind != proto.KindSite {
		s.M.RejectedAuth.Add(1)
		http.Error(w, "device token required", http.StatusUnauthorized)
		return
	}
	if !s.admit() {
		s.M.RejectedAdmission.Add(1)
		s.refuse(w, http.StatusServiceUnavailable, "admission control: retry later")
		return
	}
	if s.M.Active.Load() >= int64(s.cfg.MaxSessions) && s.session(claims.Subject) == nil {
		s.M.RejectedCapacity.Add(1)
		s.refuse(w, http.StatusServiceUnavailable, "NOC at capacity")
		return
	}
	conn, err := wsock.Upgrade(w, r, wsock.Options{
		Subprotocols:   []string{proto.Subprotocol},
		CheckOrigin:    func(r *http.Request) bool { return r.Header.Get("Origin") == "" }, // devices only, never browsers
		MaxMessageSize: s.cfg.MaxMessageSize,
		WriteTimeout:   10 * time.Second,
	})
	if err != nil {
		s.M.RejectedProtocol.Add(1)
		return
	}
	if conn.Subprotocol != proto.Subprotocol {
		s.M.RejectedProtocol.Add(1)
		_ = conn.Close(wsock.ClosePolicyViolation, "subprotocol "+proto.Subprotocol+" required")
		return
	}
	s.M.Accepted.Add(1)
	s.M.Active.Add(1)
	// The session gets its own goroutine and the handler returns: the HTTP server's
	// per-connection state (a 4 KiB read buffer, a 4 KiB write buffer, the parsed
	// request) becomes garbage instead of living as long as the device stays
	// connected. At 5 000 devices that is about 45 MB.
	remote := r.RemoteAddr
	go func() {
		defer s.M.Active.Add(-1)
		s.run(conn, claims, remote)
	}()
}

func (s *Server) run(conn *wsock.Conn, claims proto.Claims, remote string) {
	defer func() {
		conn.CloseNow()
		s.M.BytesIn.Add(uint64(conn.BytesRead()))
		s.M.BytesOut.Add(uint64(conn.BytesWritten()))
	}()
	_, raw, err := conn.ReadMessage(s.cfg.HelloTimeout)
	if err != nil {
		return
	}
	var hello proto.Message
	if json.Unmarshal(raw, &hello) != nil || hello.T != proto.THello || hello.Boot == "" {
		s.M.RejectedProtocol.Add(1)
		_ = conn.Close(wsock.ClosePolicyViolation, "hello expected")
		return
	}
	sess := &session{id: s.nextID.Add(1), site: claims.Subject, tenant: claims.Tenant, conn: conn}
	// Registering in the store and in the session map happen together, so that two
	// connections of the same device racing each other end with one winner everywhere.
	s.mu.Lock()
	have, _, detail := s.store.Connect(sess.site, sess.tenant, sess.id, remote, &hello)
	old := s.sessions[sess.site]
	s.sessions[sess.site] = sess
	s.mu.Unlock()
	if old != nil {
		s.M.Replaced.Add(1) // e.g. the device lost its link and reconnected before the old socket timed out
		_ = old.conn.Close(4000, "replaced by a newer connection")
	}
	var timer atomic.Pointer[time.Timer]
	defer func() {
		conn.CloseNow()
		if t := timer.Load(); t != nil {
			t.Stop()
		}
		s.mu.Lock()
		if s.sessions[sess.site] == sess {
			delete(s.sessions, sess.site)
		}
		s.mu.Unlock()
		s.store.Disconnect(sess.site, sess.id)
	}()

	welcome := &proto.Message{T: proto.TWelcome, Site: sess.site, Tenant: sess.tenant, Session: strconv.FormatUint(sess.id, 36),
		KeepaliveS: int(s.cfg.Keepalive / time.Second), Have: have}
	if sess.send(welcome) != nil {
		return
	}
	if detail {
		_ = sess.send(s.detailSub(true))
	}
	// Keep-alive pings from a timer, not a goroutine per device: 5 000 idle devices
	// cost 5 000 timers. The first ping is spread over the interval.
	var ping func()
	ping = func() {
		select {
		case <-conn.Done():
			return
		default:
		}
		if conn.Ping(nil) == nil {
			s.M.Pings.Add(1)
		}
		timer.Store(time.AfterFunc(s.cfg.Keepalive, ping))
	}
	timer.Store(time.AfterFunc(s.cfg.Keepalive/2+rand.N(s.cfg.Keepalive/2), ping))

	for {
		_, raw, err := conn.ReadMessage(s.cfg.IdleTimeout)
		if err != nil {
			var ce *wsock.CloseError
			if !errors.As(err, &ce) && !s.closing.Load() {
				s.log.Debug("uplink_closed", "site", sess.site, "err", err)
			}
			return
		}
		s.M.Messages.Add(1)
		var m proto.Message
		if json.Unmarshal(raw, &m) != nil {
			s.M.BadMessages.Add(1)
			continue
		}
		switch m.T {
		case proto.TSummary:
			s.store.ApplySummary(sess.site, sess.id, &m)
		case proto.TDelta:
			if s.store.ApplyDelta(sess.site, sess.id, &m) {
				_ = sess.send(&proto.Message{T: proto.TResync, What: "summary", Reason: "revision gap or hash mismatch"})
			}
		case proto.TAlarms:
			ack, resync := s.store.ApplyAlarms(sess.site, sess.id, &m)
			if ack > 0 {
				_ = sess.send(&proto.Message{T: proto.TAck, Seq: ack})
			}
			if resync {
				_ = sess.send(&proto.Message{T: proto.TResync, What: "alarms", Reason: "sequence gap"})
			}
		case proto.TPing:
			_ = sess.send(&proto.Message{T: proto.TPong, Seq: m.Seq})
		case proto.TDetail:
			if s.store.ApplyDetail(sess.site, sess.id, &m) {
				_ = sess.send(&proto.Message{T: proto.TResync, What: "detail", Reason: "revision gap"})
			}
		default:
			s.M.BadMessages.Add(1) // unknown types are ignored: newer devices, older NOC
		}
	}
}

// Sessions is the number of connected devices.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// Verifier is the token verifier the endpoint uses.
func (s *Server) Verifier() *proto.Verifier { return s.verify }

// DropRevoked closes the connections of devices whose token was revoked after
// they connected (a stolen or replaced Master Unit); their reconnects are refused.
func (s *Server) DropRevoked() int {
	s.mu.Lock()
	var drop []*session
	for site, sess := range s.sessions {
		if s.verify.IsRevoked(proto.KindSite, site) {
			drop = append(drop, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range drop {
		_ = sess.conn.Close(wsock.ClosePolicyViolation, "token revoked")
	}
	return len(drop)
}

// Shutdown refuses new devices and closes every connection with "service restart";
// devices reconnect (with jitter) to the next instance.
func (s *Server) Shutdown() {
	s.closing.Store(true)
	s.mu.Lock()
	all := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		all = append(all, sess)
	}
	s.mu.Unlock()
	for _, sess := range all {
		_ = sess.conn.Close(1012, "NOC restart")
	}
}

// Stats for /internal/metrics.
func (s *Server) Stats() map[string]any {
	return map[string]any{
		"active": s.M.Active.Load(), "handshakes": s.M.Handshakes.Load(), "accepted": s.M.Accepted.Load(),
		"rejected": map[string]uint64{"admission": s.M.RejectedAdmission.Load(), "auth": s.M.RejectedAuth.Load(),
			"capacity": s.M.RejectedCapacity.Load(), "protocol": s.M.RejectedProtocol.Load()},
		"messages": s.M.Messages.Load(), "badMessages": s.M.BadMessages.Load(), "replaced": s.M.Replaced.Load(),
		"pings": s.M.Pings.Load(), "bytesIn": s.M.BytesIn.Load(), "bytesOut": s.M.BytesOut.Load(),
		"admitRate": s.cfg.AdmitRate, "keepaliveS": s.cfg.Keepalive.Seconds(),
	}
}
