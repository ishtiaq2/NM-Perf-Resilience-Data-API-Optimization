// Package configstore keeps Remote Node configuration with optimistic
// concurrency (ETag / If-Match), used when the edge serves configuration itself
// instead of proxying it to the legacy Node.js core-api.
package configstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Bands a Remote Node can enable.
var Bands = []string{"B28-700", "B20-800", "B8-900", "B3-1800", "B1-2100", "B7-2600"}

// Config is the editable configuration of one node.
type Config struct {
	Name         string          `json:"name"`
	DlGainDb     float64         `json:"dlGainDb"`
	UlGainDb     float64         `json:"ulGainDb"`
	BandsEnabled map[string]bool `json:"bandsEnabled"`
	TempAlarmC   float64         `json:"tempAlarmC"`
	Notes        string          `json:"notes"`
}

// Envelope is what GET/PUT return.
type Envelope struct {
	NodeID    int    `json:"nodeId"`
	Version   int    `json:"version"`
	UpdatedAt int64  `json:"updatedAt"`
	Config    Config `json:"config"`
}

func defaultConfig(id int) Config {
	b := map[string]bool{}
	for _, n := range Bands {
		b[n] = true
	}
	return Config{Name: fmt.Sprintf("RN-%03d", id), DlGainDb: 25, UlGainDb: 15, BandsEnabled: b, TempAlarmC: 70}
}

// Validate returns human-readable errors (empty = valid).
func Validate(c *Config) []string {
	var errs []string
	if n := utf8.RuneCountInString(c.Name); n == 0 || n > 32 {
		errs = append(errs, "name must be a string of 1-32 characters")
	}
	if c.DlGainDb < 0 || c.DlGainDb > 40 {
		errs = append(errs, "dlGainDb must be a number in [0, 40]")
	}
	if c.UlGainDb < 0 || c.UlGainDb > 30 {
		errs = append(errs, "ulGainDb must be a number in [0, 30]")
	}
	if c.TempAlarmC < 40 || c.TempAlarmC > 90 {
		errs = append(errs, "tempAlarmC must be a number in [40, 90]")
	}
	if utf8.RuneCountInString(c.Notes) > 500 {
		errs = append(errs, "notes must be a string of at most 500 characters")
	}
	if c.BandsEnabled == nil {
		errs = append(errs, "bandsEnabled must be an object")
	}
	for k := range c.BandsEnabled {
		known := false
		for _, n := range Bands {
			known = known || n == k
		}
		if !known {
			errs = append(errs, "unknown band "+k)
		}
	}
	return errs
}

// ErrSyntax means the request body is not JSON (HTTP 400).
var ErrSyntax = errors.New("invalid JSON")

// Decode parses a PUT body strictly, like the Node.js services: every field is
// required and must have the right JSON type. It returns the configuration and
// all validation errors (HTTP 422), or ErrSyntax.
func Decode(body []byte) (Config, []string, error) {
	var w struct {
		Name         *string                    `json:"name"`
		DlGainDb     *float64                   `json:"dlGainDb"`
		UlGainDb     *float64                   `json:"ulGainDb"`
		BandsEnabled map[string]json.RawMessage `json:"bandsEnabled"`
		TempAlarmC   *float64                   `json:"tempAlarmC"`
		Notes        *string                    `json:"notes"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return Config{}, nil, ErrSyntax
		}
		if te.Field == "" {
			return Config{}, []string{"body must be a JSON object"}, nil
		}
		// A wrong-typed field stays nil and is reported below; decoding continued.
	}
	var errs []string
	c := Config{BandsEnabled: map[string]bool{}}
	str := func(p *string, msg string) string {
		if p == nil {
			errs = append(errs, msg)
			return ""
		}
		return *p
	}
	num := func(p *float64, name string, lo, hi float64) float64 {
		if p == nil {
			errs = append(errs, fmt.Sprintf("%s must be a number in [%g, %g]", name, lo, hi))
			return lo
		}
		return *p
	}
	c.Name = str(w.Name, "name must be a string of 1-32 characters")
	c.DlGainDb = num(w.DlGainDb, "dlGainDb", 0, 40)
	c.UlGainDb = num(w.UlGainDb, "ulGainDb", 0, 30)
	c.TempAlarmC = num(w.TempAlarmC, "tempAlarmC", 40, 90)
	c.Notes = str(w.Notes, "notes must be a string of at most 500 characters")
	if w.BandsEnabled == nil {
		c.BandsEnabled = nil
	}
	for k, raw := range w.BandsEnabled {
		var on bool
		if json.Unmarshal(raw, &on) != nil {
			errs = append(errs, "bandsEnabled."+k+" must be boolean")
			continue
		}
		c.BandsEnabled[k] = on
	}
	seen := map[string]bool{}
	for _, e := range errs {
		seen[e] = true
	}
	for _, e := range Validate(&c) {
		if !seen[e] {
			errs = append(errs, e)
		}
	}
	return c, errs, nil
}

// Store holds all node configurations.
type Store struct {
	bootID string
	nodes  int
	mu     sync.Mutex
	items  map[int]*Envelope
}

// New creates a store for node ids 1..nodes.
func New(bootID string, nodes int) *Store {
	return &Store{bootID: bootID, nodes: nodes, items: map[int]*Envelope{}}
}

func (s *Store) get(id int) *Envelope {
	e, ok := s.items[id]
	if !ok {
		e = &Envelope{NodeID: id, Version: 1, UpdatedAt: time.Now().UnixMilli(), Config: defaultConfig(id)}
		s.items[id] = e
	}
	return e
}

// Get returns a copy of the envelope and its ETag.
func (s *Store) Get(id int) (Envelope, string, bool) {
	if id < 1 || id > s.nodes {
		return Envelope{}, "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := *s.get(id)
	return e, s.etag(&e), true
}

func (s *Store) etag(e *Envelope) string {
	return `"cfg-` + s.bootID + "-" + strconv.Itoa(e.NodeID) + "-" + strconv.Itoa(e.Version) + `"`
}

// Result of a Put.
type Result struct {
	Status int
	Env    Envelope
	ETag   string
	Errors []string
}

// strongMatch implements If-Match (RFC 9110 13.1.1): strong comparison, weak tags never match.
func strongMatch(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		if !strings.HasPrefix(t, "W/") && t == etag {
			return true
		}
	}
	return false
}

// Put replaces the configuration. ifMatch is the raw If-Match header: "" means
// unconditional (legacy clients), otherwise it must match the current ETag
// (checked under the lock, so two concurrent writers cannot both win).
// errs are validation errors from Decode (nil: validate here). Order of checks
// as in the Node.js services: 404, 412, 422.
func (s *Store) Put(id int, c Config, ifMatch string, errs []string) Result {
	if id < 1 || id > s.nodes {
		return Result{Status: 404}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.get(id)
	if ifMatch != "" && !strongMatch(ifMatch, s.etag(cur)) {
		return Result{Status: 412, Env: *cur}
	}
	if errs == nil {
		errs = Validate(&c)
	}
	if len(errs) > 0 {
		return Result{Status: 422, Errors: errs}
	}
	bands := make(map[string]bool, len(c.BandsEnabled))
	for k, v := range c.BandsEnabled {
		bands[k] = v
	}
	c.BandsEnabled = bands // never alias the caller's map
	cur.Version++
	cur.UpdatedAt = time.Now().UnixMilli()
	cur.Config = c
	return Result{Status: 200, Env: *cur, ETag: s.etag(cur)}
}
