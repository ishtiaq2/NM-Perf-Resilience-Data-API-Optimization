package spectrum

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"dasedge/internal/heavy"
)

// Sweep is one completed analyzer sweep. Power is never modified after publication.
type Sweep struct {
	ID        uint32
	P         Params
	Timestamp int64 // epoch ms
	Power     []float32
	tag       string

	mu       sync.Mutex
	variants map[string]*Variant
}

// Variant is one wire representation of a sweep, built once and shared by all clients.
type Variant struct {
	once        sync.Once
	Body        []byte
	ContentType string
	Gzip        bool
	ETag        string
	Count       int
	Decimated   bool
}

// Variant kinds.
const (
	KindLegacy = "legacy" // legacy JSON array of {frequency, power}
	KindJSON   = "json"   // compact JSON (implicit frequency axis)
	KindI16    = "i16"    // DSPC int16 centi-dBm
	KindF32    = "f32"    // DSPC float32
)

// Tag returns the ETag of a variant without building it (for 304 decisions).
func (s *Sweep) Tag(kind string, maxPoints int, gz bool) string {
	t := `"` + s.tag + "-" + kind + "-" + strconv.Itoa(maxPoints)
	if gz {
		t += "-gz"
	}
	return t + `"`
}

// Variant builds (once per sweep and variant) the representation the client asked for.
// CPU-heavy encoding is bounded by the manager's semaphore, so a burst of distinct
// requests can never take every core away from the rest of the server.
func (s *Sweep) Variant(m *Manager, kind string, maxPoints int, gz bool) *Variant {
	key := kind + ":" + strconv.Itoa(maxPoints) + ":" + strconv.FormatBool(gz)
	s.mu.Lock()
	if s.variants == nil {
		s.variants = map[string]*Variant{}
	}
	v, ok := s.variants[key]
	if !ok {
		v = &Variant{}
		s.variants[key] = v
	}
	s.mu.Unlock()
	v.once.Do(func() {
		m.sem <- struct{}{}
		defer func() { <-m.sem }()
		m.variantBuilds.Add(1)
		v.ETag = s.Tag(kind, maxPoints, gz)
		v.Body = heavy.Do(func() []byte {
			power, start, step, dec := s.Power, s.P.StartHz, s.P.StepHz(), false
			if kind != KindLegacy {
				power, start, step, dec = PeakDecimate(s.Power, s.P.StartHz, s.P.StepHz(), maxPoints)
			}
			v.Count, v.Decimated = len(power), dec
			var body []byte
			switch kind {
			case KindLegacy:
				body = LegacyJSON(s)
				v.ContentType = "application/json; charset=utf-8"
			case KindJSON:
				body = CompactJSON(s, power, start, step, dec)
				v.ContentType = "application/json; charset=utf-8"
			default:
				enc := byte(EncI16)
				if kind == KindF32 {
					enc = EncF32
				}
				body = EncodeFrame(FrameMeta{SweepID: s.ID, NodeID: s.P.NodeID, Port: s.P.Port, StartHz: start, StepHz: step, TimestampMs: float64(s.Timestamp), Decimated: dec}, power, enc)
				v.ContentType = ContentType
			}
			if gz {
				body = Gzip(body)
			}
			return body
		})
		v.Gzip = gz
	})
	return v
}

// Session produces sweeps for one analyzer configuration while anyone is watching.
type Session struct {
	m       *Manager
	P       Params
	Key     string
	tagBase string

	mu         sync.Mutex
	latest     *Sweep
	next       chan struct{}
	lastAccess time.Time
	running    bool
	seq        uint32
	lastErr    string
}

// Touch marks the session as watched and starts sweeping if needed.
func (s *Session) Touch() {
	s.mu.Lock()
	s.lastAccess = time.Now()
	start := !s.running
	s.running = true
	s.mu.Unlock()
	if start {
		go s.loop()
	}
}

func (s *Session) loop() {
	for {
		s.mu.Lock()
		if time.Since(s.lastAccess) > s.m.idle || s.m.closed.Load() {
			s.running = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		power, err := s.m.hw.Sweep(ctx, s.P)
		cancel()
		if err != nil {
			s.m.sweepErrors.Add(1)
			s.mu.Lock()
			s.lastErr = err.Error()
			s.mu.Unlock()
			time.Sleep(500 * time.Millisecond)
			continue
		}
		s.mu.Lock()
		s.seq++
		sw := &Sweep{ID: s.seq, P: s.P, Timestamp: time.Now().UnixMilli(), Power: power, tag: s.tagBase + "-" + strconv.FormatUint(uint64(s.seq), 10)}
		s.latest = sw
		ch := s.next
		s.next = make(chan struct{})
		s.lastErr = ""
		s.mu.Unlock()
		close(ch) // wakes every waiter (legacy polls, long-polls)
		s.m.sweeps.Add(1)
		if hook := s.m.onSweep.Load(); hook != nil {
			(*hook)(s, sw)
		}
		if wait := s.m.minInterval - time.Since(started); wait > 0 {
			time.Sleep(wait)
		}
	}
}

// Latest completed sweep or nil.
func (s *Session) Latest() *Sweep {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest
}

// Next waits for the next completed sweep (legacy semantics: data newer than the request).
func (s *Session) Next(ctx context.Context) (*Sweep, error) {
	s.mu.Lock()
	ch := s.next
	s.mu.Unlock()
	select {
	case <-ch:
		return s.Latest(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// After returns the first sweep with an ID greater than id, waiting if needed
// (long-poll). Unlike Next it cannot miss a sweep that completes while the
// caller is deciding to wait.
func (s *Session) After(ctx context.Context, id uint32) (*Sweep, error) {
	for {
		s.mu.Lock()
		l, ch := s.latest, s.next
		s.mu.Unlock()
		if l != nil && l.ID > id {
			return l, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// LatestOrNext returns the latest sweep or waits for the first one.
func (s *Session) LatestOrNext(ctx context.Context) (*Sweep, error) {
	if l := s.Latest(); l != nil {
		return l, nil
	}
	return s.Next(ctx)
}

// ErrTooManySessions is returned when every session slot is busy.
var ErrTooManySessions = errors.New("too many concurrent analyzer sessions")

// Manager owns all sweep sessions.
type Manager struct {
	hw          Hardware
	bootID      string
	idle        time.Duration
	minInterval time.Duration
	maxSessions int
	sem         chan struct{}

	mu       sync.Mutex
	sessions map[string]*Session
	closed   atomic.Bool
	onSweep  atomic.Pointer[func(*Session, *Sweep)]

	sweeps, sweepErrors, variantBuilds atomic.Int64
}

// NewManager creates the session manager. Heavy encoding may use at most
// GOMAXPROCS-1 cores (min 1), keeping one core for request handling.
func NewManager(hw Hardware, bootID string, idle, minInterval time.Duration, maxSessions int) *Manager {
	return &Manager{hw: hw, bootID: bootID, idle: idle, minInterval: minInterval, maxSessions: maxSessions,
		sem: make(chan struct{}, max(1, runtime.GOMAXPROCS(0)-1)), sessions: map[string]*Session{}}
}

// OnSweep registers the push hook (WebSocket hub).
func (m *Manager) OnSweep(fn func(*Session, *Sweep)) { m.onSweep.Store(&fn) }

// Session returns (creating if needed) the session for p and marks it watched.
func (m *Manager) Session(p Params) (*Session, error) {
	key := p.Key()
	m.mu.Lock()
	s, ok := m.sessions[key]
	if !ok {
		if len(m.sessions) >= m.maxSessions {
			for k, old := range m.sessions {
				old.mu.Lock()
				idle := !old.running
				old.mu.Unlock()
				if idle {
					delete(m.sessions, k)
					break
				}
			}
		}
		if len(m.sessions) >= m.maxSessions {
			m.mu.Unlock()
			return nil, ErrTooManySessions
		}
		h := fnv.New32a()
		h.Write([]byte(key))
		s = &Session{m: m, P: p, Key: key, next: make(chan struct{}), tagBase: fmt.Sprintf("sp-%s-%x", m.bootID, h.Sum32())}
		m.sessions[key] = s
	}
	m.mu.Unlock()
	s.Touch()
	return s, nil
}

// Close stops all sessions.
func (m *Manager) Close() { m.closed.Store(true) }

// Stats for /api/metrics.
func (m *Manager) Stats() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := []map[string]any{}
	for _, s := range m.sessions {
		s.mu.Lock()
		list = append(list, map[string]any{"key": s.Key, "running": s.running, "sweepId": s.seq, "lastError": s.lastErr})
		s.mu.Unlock()
	}
	return map[string]any{"sessions": list, "sweeps": m.sweeps.Load(), "sweepErrors": m.sweepErrors.Load(), "variantBuilds": m.variantBuilds.Load()}
}
