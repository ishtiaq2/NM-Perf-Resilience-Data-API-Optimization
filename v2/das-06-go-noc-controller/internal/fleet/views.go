package fleet

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"dasnoc/internal/proto"
)

// SiteView is one row of the fleet: what lists and the problem board show.
type SiteView struct {
	ID        string    `json:"id"`
	Tenant    string    `json:"tenant"`
	Status    Status    `json:"status"`
	Connected bool      `json:"connected"`
	Since     int64     `json:"since,omitempty"` // connected or disconnected at, ms
	LastSeen  int64     `json:"lastSeen,omitempty"`
	FW        string    `json:"fw,omitempty"`
	Alarms    SevCounts `json:"alarms"`
	Acked     int       `json:"acknowledged"`
	Rev       uint64    `json:"rev"`  // summary revision (as reported by the device)
	Hash      string    `json:"hash"` // canonical summary hash computed by the NOC
	Stats
}

func (s *Site) view() SiteView {
	v := SiteView{ID: s.ID, Tenant: s.tenant, Status: s.status, Connected: s.connected, FW: s.fw, Rev: s.rev, Hash: s.hash, Stats: s.stats}
	if s.connected {
		v.Since = s.connectedAt.UnixMilli()
	} else if s.everConnected {
		v.Since = s.disconnectedAt.UnixMilli()
	}
	if !s.lastSeen.IsZero() {
		v.LastSeen = s.lastSeen.UnixMilli()
	}
	v.Alarms, v.Acked = s.sevCounts()
	if v.Name == "" {
		v.Name = s.ID
	}
	return v
}

// problemLess orders sites for the problem board: most urgent status first, then
// more critical and major alarms, then name.
func problemLess(a, b *SiteView) int {
	if d := b.Status.Rank() - a.Status.Rank(); d != 0 {
		return d
	}
	for i := 0; i < numSev; i++ {
		if d := b.Alarms[i] - a.Alarms[i]; d != 0 {
			return d
		}
	}
	return strings.Compare(a.Name, b.Name)
}

// Overview is the fleet summary a dashboard shows, for one scope.
type Overview struct {
	Version   uint64                `json:"version"`
	At        int64                 `json:"at"`
	Scope     string                `json:"scope"` // "*" or a tenant
	Sites     int                   `json:"sites"`
	Connected int                   `json:"connected"`
	ByStatus  StatusCounts          `json:"byStatus"`
	Alarms    SevCounts             `json:"alarms"`
	Acked     int                   `json:"acknowledged"`
	ByTenant  map[string]*aggregate `json:"byTenant,omitempty"`
	Problems  []SiteView            `json:"problems"` // the most urgent sites
	LastEvent uint64                `json:"lastEvent"`
}

// ProblemsShown on the overview's problem board.
const ProblemsShown = 25

func scopeKey(c proto.Claims) string {
	if c.Admin() {
		return proto.AllTenants
	}
	return c.Tenant
}

// Overview of the fleet (or of one tenant). Cached per scope until the fleet version
// changes, so many dashboards cost one computation per change.
func (st *Store) Overview(who proto.Claims) *Overview {
	key := scopeKey(who)
	ver := st.version.Load()
	st.ovMu.Lock()
	if o := st.ovCache[key]; o != nil && o.Version == ver {
		st.ovMu.Unlock()
		return o
	}
	st.ovMu.Unlock()

	o := &Overview{Version: ver, At: st.cfg.Now().UnixMilli(), Scope: key, LastEvent: st.log.last()}
	st.aggMu.Lock()
	if key == proto.AllTenants {
		a := st.agg
		o.Sites, o.Connected, o.ByStatus, o.Alarms, o.Acked = a.Sites, a.Connected, a.ByStatus, a.Alarms, a.Acked
		o.ByTenant = make(map[string]*aggregate, len(st.tenants))
		for t, a := range st.tenants {
			if a.Sites > 0 {
				c := *a
				o.ByTenant[t] = &c
			}
		}
	} else if a := st.tenants[key]; a != nil {
		o.Sites, o.Connected, o.ByStatus, o.Alarms, o.Acked = a.Sites, a.Connected, a.ByStatus, a.Alarms, a.Acked
	}
	st.aggMu.Unlock()

	// The problem board: a bounded selection, not a full sort of the fleet.
	var top []SiteView
	for _, s := range st.all() {
		s.mu.Lock()
		if (key != proto.AllTenants && s.tenant != key) || s.status == StatusOK {
			s.mu.Unlock()
			continue
		}
		v := s.view()
		s.mu.Unlock()
		if len(top) < ProblemsShown {
			top = append(top, v)
			slices.SortFunc(top, func(a, b SiteView) int { return problemLess(&a, &b) })
		} else if problemLess(&v, &top[len(top)-1]) < 0 {
			top[len(top)-1] = v
			slices.SortFunc(top, func(a, b SiteView) int { return problemLess(&a, &b) })
		}
	}
	if top == nil {
		top = []SiteView{}
	}
	o.Problems = top

	st.ovMu.Lock()
	st.ovCache[key] = o
	st.ovMu.Unlock()
	return o
}

// SiteFilter selects sites for a list.
type SiteFilter struct {
	Statuses []Status // empty: all
	Tenant   string   // empty: all visible
	Query    string   // substring of id, name or venue (case-insensitive)
	Sort     string   // severity (default) | name | id | lastSeen
	Offset   int
	Limit    int // default 100, at most 1000
}

// ListSites returns one page of the visible sites and the total that matched.
func (st *Store) ListSites(who proto.Claims, f SiteFilter) ([]SiteView, int) {
	q := strings.ToLower(f.Query)
	var out []SiteView
	for _, s := range st.all() {
		s.mu.Lock()
		if !who.Sees(s.tenant) || (f.Tenant != "" && s.tenant != f.Tenant) || (len(f.Statuses) > 0 && !slices.Contains(f.Statuses, s.status)) {
			s.mu.Unlock()
			continue
		}
		v := s.view()
		s.mu.Unlock()
		if q != "" && !strings.Contains(strings.ToLower(v.ID), q) && !strings.Contains(strings.ToLower(v.Name), q) && !strings.Contains(strings.ToLower(v.Venue), q) {
			continue
		}
		out = append(out, v)
	}
	switch f.Sort {
	case "name":
		slices.SortFunc(out, func(a, b SiteView) int { return strings.Compare(a.Name, b.Name) })
	case "id":
		slices.SortFunc(out, func(a, b SiteView) int { return strings.Compare(a.ID, b.ID) })
	case "lastSeen":
		slices.SortFunc(out, func(a, b SiteView) int { return int(b.LastSeen - a.LastSeen) })
	default:
		slices.SortFunc(out, func(a, b SiteView) int { return problemLess(&a, &b) })
	}
	total := len(out)
	if f.Limit <= 0 {
		f.Limit = 100
	}
	f.Limit = min(f.Limit, 1000)
	if f.Offset >= total {
		return []SiteView{}, total
	}
	return out[f.Offset:min(total, f.Offset+f.Limit)], total
}

// SiteDetail is everything the NOC knows about one site.
type SiteDetail struct {
	SiteView
	Summary      map[string]json.RawMessage `json:"summary"`
	Boot         string                     `json:"boot,omitempty"`
	Agent        string                     `json:"agent,omitempty"`
	Remote       string                     `json:"remote,omitempty"`
	ActiveAlarms []Alarm                    `json:"activeAlarms"`
	Detail       *DetailView                `json:"detail,omitempty"`
	DetailOn     bool                       `json:"detailStreaming"`
	HashMismatch uint64                     `json:"hashMismatch"`
	Version      uint64                     `json:"version"`
}

// DetailView is the node table of a site under drill-down.
type DetailView struct {
	Rev       uint64            `json:"rev"`
	UpdatedAt int64             `json:"updatedAt"`
	Count     int               `json:"count"`
	Nodes     []json.RawMessage `json:"nodes"`
}

// Site returns one site's detail, if the caller may see it.
func (st *Store) Site(who proto.Claims, id string) (*SiteDetail, bool) {
	s := st.get(id)
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !who.Sees(s.tenant) {
		return nil, false
	}
	d := &SiteDetail{SiteView: s.view(), Summary: maps.Clone(s.summary), Boot: s.boot, Agent: s.agent, Remote: s.remote,
		DetailOn: s.detailOn, HashMismatch: s.hashMismatch, Version: s.ver}
	if d.Summary == nil {
		d.Summary = map[string]json.RawMessage{}
	}
	d.ActiveAlarms = make([]Alarm, 0, len(s.active)+len(s.nocAlarms))
	for _, set := range [2]map[string]*Alarm{s.nocAlarms, s.active} {
		for _, a := range set {
			d.ActiveAlarms = append(d.ActiveAlarms, *a)
		}
	}
	slices.SortFunc(d.ActiveAlarms, func(a, b Alarm) int {
		if d := proto.SeverityRank(b.Sev) - proto.SeverityRank(a.Sev); d != 0 {
			return d
		}
		return int(b.RaisedAt - a.RaisedAt)
	})
	if s.detail != nil {
		ids := make([]int, 0, len(s.detail.Nodes))
		for k := range s.detail.Nodes {
			ids = append(ids, k)
		}
		slices.Sort(ids)
		dv := &DetailView{Rev: s.detail.Rev, UpdatedAt: s.detail.UpdatedAt.UnixMilli(), Count: len(ids), Nodes: make([]json.RawMessage, len(ids))}
		for i, k := range ids {
			dv.Nodes[i] = s.detail.Nodes[k]
		}
		d.Detail = dv
	}
	return d, true
}

// SiteVersion is a site's change counter (0 if unknown or not visible).
func (st *Store) SiteVersion(who proto.Claims, id string) uint64 {
	s := st.get(id)
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !who.Sees(s.tenant) {
		return 0
	}
	return s.ver
}

// AlarmView is an active alarm with its site.
type AlarmView struct {
	Alarm
	Site     string `json:"site"`
	Tenant   string `json:"tenant"`
	SiteName string `json:"siteName"`
}

// AlarmFilter selects active alarms.
type AlarmFilter struct {
	MinSeverity int    // proto.SeverityRank; 0 = all
	Site        string // empty = all
	Unacked     bool
	Limit       int // default 500, at most 5000
}

// ActiveAlarms across the visible fleet, most severe and newest first.
func (st *Store) ActiveAlarms(who proto.Claims, f AlarmFilter) ([]AlarmView, int) {
	var out []AlarmView
	for _, s := range st.all() {
		s.mu.Lock()
		if !who.Sees(s.tenant) || (f.Site != "" && s.ID != f.Site) {
			s.mu.Unlock()
			continue
		}
		name := s.stats.Name
		if name == "" {
			name = s.ID
		}
		for _, set := range [2]map[string]*Alarm{s.nocAlarms, s.active} {
			for _, a := range set {
				if proto.SeverityRank(a.Sev) < f.MinSeverity || (f.Unacked && a.AckBy != "") {
					continue
				}
				out = append(out, AlarmView{Alarm: *a, Site: s.ID, Tenant: s.tenant, SiteName: name})
			}
		}
		s.mu.Unlock()
	}
	slices.SortFunc(out, func(a, b AlarmView) int {
		if d := proto.SeverityRank(b.Sev) - proto.SeverityRank(a.Sev); d != 0 {
			return d
		}
		return int(b.RaisedAt - a.RaisedAt)
	})
	total := len(out)
	if f.Limit <= 0 {
		f.Limit = 500
	}
	return out[:min(total, min(f.Limit, 5000))], total
}

// EventsSince returns alarm log events after the cursor visible to the caller, the
// new cursor, and whether events were lost to the ring's capacity (reload then).
func (st *Store) EventsSince(who proto.Claims, after uint64, max int) ([]Event, uint64, bool) {
	return st.log.since(after, max, func(e *Event) bool { return who.Sees(e.Tenant) })
}

// EventsSinceRaw is EventsSince with each event already encoded as JSON (encoded
// once when it was logged, shared by every dashboard).
func (st *Store) EventsSinceRaw(who proto.Claims, after uint64, max int) ([]json.RawMessage, uint64, bool) {
	return st.log.sinceRaw(after, max, func(e *Event) bool { return who.Sees(e.Tenant) })
}

// ProcessNow is the store's clock.
func (st *Store) ProcessNow() time.Time { return st.cfg.Now() }
