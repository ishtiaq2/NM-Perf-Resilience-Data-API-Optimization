// Package fleet holds the NOC's live view of every Master Unit: connection
// state, the site summary each device reports, active alarms, and the fleet-wide
// aggregates dashboards show.
//
// The Master Units are the source of truth for live state. After a NOC restart
// every device sends its summary and active alarms again, so nothing here needs
// to be durable except the registry (which sites exist, even when silent) and
// operator annotations (alarm acknowledgements); see registry.go.
package fleet

import (
	"encoding/json"
	"math"
	"strconv"
)

// Num is a float that is written as null when unknown (NaN).
type Num float64

// MarshalJSON writes null for NaN and infinities.
func (n Num) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, f, 'f', -1, 64), nil
}

// Known reports whether the value is present.
func (n Num) Known() bool { return !math.IsNaN(float64(n)) }

// Status of a site as the NOC sees it.
type Status int

const (
	StatusOK       Status = iota // connected, no active alarm
	StatusWarning                // worst active alarm: warning
	StatusMinor                  // ... minor
	StatusMajor                  // ... major
	StatusCritical               // ... critical
	StatusStale                  // disconnected for less than the grace period (a reconnect is expected)
	StatusOffline                // disconnected longer, or never connected
	numStatus
)

var statusNames = [numStatus]string{"ok", "warning", "minor", "major", "critical", "stale", "offline"}

func (s Status) String() string { return statusNames[s] }

// MarshalJSON writes the status name.
func (s Status) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// ParseStatus returns the status for a name.
func ParseStatus(name string) (Status, bool) {
	for i, n := range statusNames {
		if n == name {
			return Status(i), true
		}
	}
	return 0, false
}

// Rank orders statuses by how urgently an operator should look: critical first,
// then unreachable sites, then major, stale, minor, warning, ok.
func (s Status) Rank() int {
	switch s {
	case StatusCritical:
		return 6
	case StatusOffline:
		return 5
	case StatusMajor:
		return 4
	case StatusStale:
		return 3
	case StatusMinor:
		return 2
	case StatusWarning:
		return 1
	}
	return 0
}

// Severity indexes: critical, major, minor, warning.
const (
	SevCritical = iota
	SevMajor
	SevMinor
	SevWarning
	numSev
)

func sevIndex(s string) int {
	switch s {
	case "critical":
		return SevCritical
	case "major":
		return SevMajor
	case "minor":
		return SevMinor
	case "warning":
		return SevWarning
	}
	return -1
}

// SevCounts counts active alarms by severity.
type SevCounts [numSev]int

// MarshalJSON writes {"critical":n,"major":n,"minor":n,"warning":n}.
func (c SevCounts) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]int{"critical": c[SevCritical], "major": c[SevMajor], "minor": c[SevMinor], "warning": c[SevWarning]})
}

// UnmarshalJSON reads the object form (API clients, tests).
func (c *SevCounts) UnmarshalJSON(b []byte) error {
	var m map[string]int
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*c = SevCounts{m["critical"], m["major"], m["minor"], m["warning"]}
	return nil
}

// StatusCounts counts sites by status.
type StatusCounts [numStatus]int

// MarshalJSON writes {"ok":n,...,"offline":n}.
func (c StatusCounts) MarshalJSON() ([]byte, error) {
	m := make(map[string]int, numStatus)
	for i, n := range statusNames {
		m[n] = c[i]
	}
	return json.Marshal(m)
}

// UnmarshalJSON reads the object form.
func (c *StatusCounts) UnmarshalJSON(b []byte) error {
	var m map[string]int
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for i, n := range statusNames {
		c[i] = m[n]
	}
	return nil
}

// UnmarshalJSON reads a status name.
func (s *Status) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return err
	}
	st, _ := ParseStatus(name)
	*s = st
	return nil
}

// UnmarshalJSON reads a number or null (NaN).
func (n *Num) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*n = nan()
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*n = Num(f)
	return nil
}

// Stats is the typed view of the summary fields the NOC aggregates and sorts by.
// Unknown summary fields are kept (and hashed) but not interpreted.
type Stats struct {
	Name     string `json:"name,omitempty"`
	Venue    string `json:"venue,omitempty"`
	Region   string `json:"region,omitempty"`
	Nodes    int    `json:"nodes"`
	Online   int    `json:"online"`
	Degraded int    `json:"degraded"`
	Offline  int    `json:"offline"`
	MaxTempC Num    `json:"maxTempC"`
	MinRxDbm Num    `json:"minRxDbm"`
	MaxVswr  Num    `json:"maxVswr"`
}

func (s *Stats) fromSummary(sum map[string]json.RawMessage) {
	str := func(k string) string {
		var v string
		_ = json.Unmarshal(sum[k], &v)
		return v
	}
	num := func(k string) float64 {
		var v float64
		if raw, ok := sum[k]; !ok || json.Unmarshal(raw, &v) != nil {
			return math.NaN()
		}
		return v
	}
	whole := func(k string) int {
		v := num(k)
		if math.IsNaN(v) {
			return 0
		}
		return int(v)
	}
	*s = Stats{
		Name: str("name"), Venue: str("venue"), Region: str("region"),
		Nodes: whole("nodes"), Online: whole("online"), Degraded: whole("degraded"), Offline: whole("offline"),
		MaxTempC: Num(num("maxTempC")), MinRxDbm: Num(num("minRxDbm")), MaxVswr: Num(num("maxVswr")),
	}
}

// Alarm is an active alarm at a site.
type Alarm struct {
	ID         string `json:"id"`
	Node       int    `json:"node,omitempty"`
	Code       string `json:"code"`
	Sev        string `json:"sev"`
	Text       string `json:"text,omitempty"`
	RaisedAt   int64  `json:"raisedAt"`   // device clock, ms
	ReceivedAt int64  `json:"receivedAt"` // NOC clock, ms
	Seq        uint64 `json:"seq,omitempty"`
	Source     string `json:"source"` // device | noc
	AckBy      string `json:"ackBy,omitempty"`
	AckAt      int64  `json:"ackAt,omitempty"`
	AckNote    string `json:"ackNote,omitempty"`
}

// Event is one entry of the fleet-wide alarm log that dashboards stream.
type Event struct {
	N          uint64 `json:"n"` // NOC sequence number
	Site       string `json:"site"`
	Tenant     string `json:"tenant"`
	SiteName   string `json:"siteName,omitempty"`
	ID         string `json:"id"`
	Node       int    `json:"node,omitempty"`
	Code       string `json:"code"`
	Sev        string `json:"sev"`
	State      string `json:"state"` // raised | cleared | acknowledged
	Text       string `json:"text,omitempty"`
	At         int64  `json:"at"` // device clock (NOC clock for NOC alarms), ms
	ReceivedAt int64  `json:"rx"` // NOC clock, ms
	By         string `json:"by,omitempty"`
}
