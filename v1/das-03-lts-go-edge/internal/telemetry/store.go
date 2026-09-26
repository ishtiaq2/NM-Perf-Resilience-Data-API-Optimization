package telemetry

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"dasedge/internal/heavy"
	"dasedge/internal/spectrum"
)

type entry struct {
	norm     State
	canon    []byte
	hash     uint64
	json     []byte // published JSON (normalised state + per-report fields)
	lastSeen time.Time
	offline  bool
}

// View is an immutable published revision. Snapshot bytes are built lazily,
// once, and shared by every HTTP request and WebSocket client.
type View struct {
	Rev         uint64
	GeneratedAt int64
	ids         []uint32
	nodes       map[uint32][]byte

	snapOnce, gzOnce sync.Once
	snap, gz         []byte
}

// SnapshotJSON: {"rev","generatedAt","nodeCount","nodes":{"<id>":{...}}}
func (v *View) SnapshotJSON() []byte {
	v.snapOnce.Do(func() {
		v.snap = heavy.Do(func() []byte {
			size := 96
			for _, id := range v.ids {
				size += len(v.nodes[id]) + 12
			}
			b := make([]byte, 0, size)
			b = append(b, `{"rev":`...)
			b = strconv.AppendUint(b, v.Rev, 10)
			b = append(b, `,"generatedAt":`...)
			b = strconv.AppendInt(b, v.GeneratedAt, 10)
			b = append(b, `,"nodeCount":`...)
			b = strconv.AppendInt(b, int64(len(v.ids)), 10)
			b = append(b, `,"nodes":{`...)
			for i, id := range v.ids {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, '"')
				b = strconv.AppendUint(b, uint64(id), 10)
				b = append(b, `":`...)
				b = append(b, v.nodes[id]...)
			}
			return append(b, "}}"...)
		})
	})
	return v.snap
}

// SnapshotGzip is gzip(SnapshotJSON), built once per revision.
func (v *View) SnapshotGzip() []byte {
	v.gzOnce.Do(func() {
		js := v.SnapshotJSON() // not inside heavy.Do: heavy work must not nest
		v.gz = heavy.Do(func() []byte { return spectrum.Gzip(js) })
	})
	return v.gz
}

// NodeCount in this revision.
func (v *View) NodeCount() int { return len(v.ids) }

// PublishEvent describes one new revision (for push transports).
type PublishEvent struct {
	Rev, Base uint64
	IDs       []uint32
	Removed   []uint32
	View      *View
	once      sync.Once
	delta     []byte
}

// DeltaJSON for exactly this publish (base -> rev), built once and shared.
func (e *PublishEvent) DeltaJSON() []byte {
	e.once.Do(func() { e.delta = deltaJSON(e.View, e.Base, e.IDs, e.Removed) })
	return e.delta
}

func deltaJSON(v *View, base uint64, ids, removed []uint32) []byte {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	b := make([]byte, 0, 128+len(ids)*1400)
	b = append(b, `{"rev":`...)
	b = strconv.AppendUint(b, v.Rev, 10)
	b = append(b, `,"base":`...)
	b = strconv.AppendUint(b, base, 10)
	b = append(b, `,"generatedAt":`...)
	b = strconv.AppendInt(b, v.GeneratedAt, 10)
	b = append(b, `,"changed":{`...)
	first := true
	for _, id := range ids {
		js, ok := v.nodes[id]
		if !ok {
			continue
		}
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(b, '"')
		b = strconv.AppendUint(b, uint64(id), 10)
		b = append(b, `":`...)
		b = append(b, js...)
	}
	b = append(b, `},"removed":[`...)
	for i, id := range removed {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendUint(b, uint64(id), 10)
	}
	return append(b, "]}"...)
}

type hist struct {
	rev          uint64
	ids, removed []uint32
}

// Store merges Remote Node state and publishes revisions.
type Store struct {
	BootID     string
	Normalize  bool
	Deadbands  Deadbands
	StaleAfter time.Duration
	History    int

	mu      sync.Mutex
	nodes   map[uint32]*entry
	pending map[uint32]struct{}
	gone    map[uint32]struct{}
	rev     uint64
	history []hist
	view    atomic.Pointer[View]
	subs    []func(*PublishEvent)

	Reports, Unchanged, Publishes atomic.Int64
}

// RevisionBase returns a random starting revision for this boot (like a TCP
// initial sequence number), between 2^40 and 2^52, so it stays a safe JSON integer.
// A revision a client kept from a previous boot is then older than the history,
// or newer than the current revision, and is answered with a snapshot, never with
// a delta that does not apply to the data the client holds.
func RevisionBase() uint64 { return 1<<40 + rand.Uint64N(1<<51) }

// NewStore creates an empty store; its first (empty) revision is RevisionBase().
func NewStore(bootID string, normalize bool) *Store {
	s := &Store{BootID: bootID, Normalize: normalize, Deadbands: DefaultDeadbands, History: 120,
		nodes: map[uint32]*entry{}, pending: map[uint32]struct{}{}, gone: map[uint32]struct{}{}, rev: RevisionBase()}
	s.view.Store(&View{Rev: s.rev, nodes: map[uint32][]byte{}, GeneratedAt: time.Now().UnixMilli()})
	return s
}

// Subscribe to publish events (called on the publishing goroutine; must not block).
func (s *Store) Subscribe(fn func(*PublishEvent)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

func publishedJSON(st *State, r *Report) []byte {
	return st.AppendJSON(make([]byte, 0, 1500), func(b []byte) []byte {
		b = append(b, `,"seq":`...)
		b = strconv.AppendUint(b, r.Seq, 10)
		b = append(b, `,"reportedAt":`...)
		b = strconv.AppendInt(b, r.ReportedAt, 10)
		b = append(b, `,"uptimeS":`...)
		return strconv.AppendInt(b, r.UptimeS, 10)
	})
}

// Ingest a report. Returns true when the node's normalised state changed.
func (s *Store) Ingest(r *Report) bool {
	s.Reports.Add(1)
	s.mu.Lock()
	prev := s.nodes[r.ID]
	s.mu.Unlock()
	var norm State
	canon := heavy.Do(func() []byte {
		norm = r.State
		if s.Normalize && prev != nil {
			norm = ApplyDeadband(&prev.norm, r.State, s.Deadbands)
		}
		return norm.Canonical()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.nodes[r.ID]
	if e != nil {
		e.lastSeen = time.Now()
		if !e.offline && bytes.Equal(e.canon, canon) {
			s.Unchanged.Add(1)
			return false
		}
	} else {
		e = &entry{}
		s.nodes[r.ID] = e
	}
	e.norm, e.canon, e.hash, e.offline, e.lastSeen = norm, canon, Hash64(canon), false, time.Now()
	e.json = publishedJSON(&norm, r)
	s.pending[r.ID] = struct{}{}
	delete(s.gone, r.ID)
	return true
}

// Get returns a node's normalised state and hash (ingest protocol).
func (s *Store) Get(id uint32) (State, uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.nodes[id]
	if !ok {
		return State{}, 0, false
	}
	return e.norm, e.hash, true
}

// Remove a node (e.g. decommissioned).
func (s *Store) Remove(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[id]; ok {
		delete(s.nodes, id)
		delete(s.pending, id)
		s.gone[id] = struct{}{}
	}
}

// Publish a new revision if anything changed; returns nil otherwise.
func (s *Store) Publish() *PublishEvent {
	now := time.Now()
	s.mu.Lock()
	if s.StaleAfter > 0 {
		for id, e := range s.nodes {
			if !e.offline && now.Sub(e.lastSeen) > s.StaleAfter {
				e.offline = true
				st := e.norm
				st.Status = "offline"
				e.json = st.AppendJSON(nil, nil)
				e.canon = nil
				s.pending[id] = struct{}{}
			}
		}
	}
	if len(s.pending) == 0 && len(s.gone) == 0 {
		s.mu.Unlock()
		return nil
	}
	ids := make([]uint32, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	removed := make([]uint32, 0, len(s.gone))
	for id := range s.gone {
		removed = append(removed, id)
	}
	clear(s.pending)
	clear(s.gone)
	base := s.rev
	s.rev++
	all := make([]uint32, 0, len(s.nodes))
	nodes := make(map[uint32][]byte, len(s.nodes))
	for id, e := range s.nodes {
		all = append(all, id)
		nodes[id] = e.json // immutable slices: safe to share
	}
	slices.Sort(all)
	v := &View{Rev: s.rev, GeneratedAt: now.UnixMilli(), ids: all, nodes: nodes}
	s.view.Store(v)
	s.history = append(s.history, hist{rev: s.rev, ids: ids, removed: removed})
	if len(s.history) > s.History {
		s.history = s.history[len(s.history)-s.History:]
	}
	subs := slices.Clone(s.subs)
	s.mu.Unlock()
	s.Publishes.Add(1)
	ev := &PublishEvent{Rev: v.Rev, Base: base, IDs: ids, Removed: removed, View: v}
	for _, fn := range subs {
		fn(ev)
	}
	return ev
}

// View returns the latest published revision.
func (s *Store) View() *View { return s.view.Load() }

// DeltaJSON from revision `since` to the current view; ok=false means the
// client must take a snapshot (unknown, expired or future revision).
func (s *Store) DeltaJSON(since uint64) ([]byte, uint64, bool) {
	v := s.View()
	if since > v.Rev {
		return nil, v.Rev, false
	}
	if since == v.Rev {
		return deltaJSON(v, since, nil, nil), v.Rev, true
	}
	s.mu.Lock()
	if len(s.history) == 0 || since < s.history[0].rev-1 {
		s.mu.Unlock()
		return nil, v.Rev, false
	}
	changed := map[uint32]struct{}{}
	removed := map[uint32]struct{}{}
	for _, h := range s.history {
		if h.rev <= since || h.rev > v.Rev {
			continue
		}
		for _, id := range h.ids {
			changed[id] = struct{}{}
			delete(removed, id)
		}
		for _, id := range h.removed {
			removed[id] = struct{}{}
			delete(changed, id)
		}
	}
	s.mu.Unlock()
	ids := make([]uint32, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	rm := make([]uint32, 0, len(removed))
	for id := range removed {
		rm = append(rm, id)
	}
	slices.Sort(rm)
	return deltaJSON(v, since, ids, rm), v.Rev, true
}

// ETag of the current snapshot.
func (s *Store) ETag(v *View, gz bool) string {
	t := `"vd-` + s.BootID + "-" + strconv.FormatUint(v.Rev, 10)
	if gz {
		t += "-gz"
	}
	return t + `"`
}

// Run publishes every interval until stop is closed.
// Each new snapshot is encoded and compressed right away, in the background,
// so dashboard polls and new WebSocket subscribers find it ready.
func (s *Store) Run(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if ev := s.Publish(); ev != nil {
				go ev.View.SnapshotGzip()
			}
		}
	}
}
