// Package ingest implements das.v1.NodeIngestService (contract/proto/das/v1/ingest.proto):
// each Remote Node keeps one bidirectional stream to the Master Node and sends
// its state only when it changed, as a delta against the last state it sent,
// plus a hash of the resulting state so the master can verify the delta
// without comparing data. Any divergence (master restart, lost message, bug)
// is answered with RESYNC and healed by one full report.
//
// Wire protocol: Connect (https://connectrpc.com/docs/protocol) streaming with
// the JSON codec (protobuf JSON mapping), over HTTP/2 without TLS (h2c) on the
// fiber management network. It is the same service a generated connect-go
// handler serves; with generated code the endpoint also speaks gRPC and
// gRPC-Web with binary protobuf (see docs/INGEST.md).
package ingest

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"dasedge/internal/telemetry"
)

// U64 is a protobuf uint64 in JSON: written as a string, read from a string or a number.
type U64 uint64

func (v U64) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatUint(uint64(v), 10) + `"`), nil
}

func (v *U64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" {
		*v = 0
		return nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid uint64 %s", b)
	}
	*v = U64(n)
	return nil
}

// I64 is a protobuf int64 in JSON.
type I64 int64

func (v I64) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatInt(int64(v), 10) + `"`), nil
}

func (v *I64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" {
		*v = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid int64 %s", b)
	}
	*v = I64(n)
	return nil
}

// enum decodes a protobuf enum given by name or by number.
func enum(b []byte, names []string) (string, error) {
	var s string
	if json.Unmarshal(b, &s) == nil {
		if slices.Contains(names, s) {
			if s == names[0] {
				return "", nil // UNSPECIFIED is the zero value
			}
			return s, nil
		}
		return "", fmt.Errorf("unknown enum value %q", s)
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil || n < 0 || n >= len(names) {
		return "", fmt.Errorf("invalid enum %s", b)
	}
	if n == 0 {
		return "", nil
	}
	return names[n], nil
}

var nodeStatusNames = []string{"NODE_STATUS_UNSPECIFIED", "NODE_STATUS_ONLINE", "NODE_STATUS_DEGRADED", "NODE_STATUS_OFFLINE"}
var severityNames = []string{"ALARM_SEVERITY_UNSPECIFIED", "ALARM_SEVERITY_MINOR", "ALARM_SEVERITY_MAJOR", "ALARM_SEVERITY_CRITICAL"}
var actionNames = []string{"ACTION_UNSPECIFIED", "ACTION_OK", "ACTION_RESYNC", "ACTION_SLOW_DOWN"}

// NodeStatus is das.v1.NodeStatus.
type NodeStatus string

func (s *NodeStatus) UnmarshalJSON(b []byte) error {
	v, err := enum(b, nodeStatusNames)
	*s = NodeStatus(v)
	return err
}

// Severity is das.v1.AlarmSeverity.
type Severity string

func (s *Severity) UnmarshalJSON(b []byte) error {
	v, err := enum(b, severityNames)
	*s = Severity(v)
	return err
}

// Action is das.v1.ReportResponse.Action.
type Action string

// Actions.
const (
	ActionOK       Action = "ACTION_OK"
	ActionResync   Action = "ACTION_RESYNC"
	ActionSlowDown Action = "ACTION_SLOW_DOWN"
)

func (a *Action) UnmarshalJSON(b []byte) error {
	v, err := enum(b, actionNames)
	*a = Action(v)
	return err
}

// Optical is das.v1.Optical.
type Optical struct {
	RxDbm       float64 `json:"rxDbm,omitempty"`
	TxDbm       float64 `json:"txDbm,omitempty"`
	LaserBiasMa float64 `json:"laserBiasMa,omitempty"`
}

// Band is das.v1.Band.
type Band struct {
	Name     string  `json:"name,omitempty"`
	Enabled  bool    `json:"enabled,omitempty"`
	DlOutDbm float64 `json:"dlOutDbm,omitempty"`
	UlInDbm  float64 `json:"ulInDbm,omitempty"`
	DlGainDb float64 `json:"dlGainDb,omitempty"`
	UlGainDb float64 `json:"ulGainDb,omitempty"`
	Vswr     float64 `json:"vswr,omitempty"`
}

// Alarm is das.v1.Alarm.
type Alarm struct {
	Code     string   `json:"code,omitempty"`
	Severity Severity `json:"severity,omitempty"`
	SinceMs  I64      `json:"sinceMs,omitempty"`
}

// NodeTelemetry is das.v1.NodeTelemetry in the protobuf JSON mapping
// (lowerCamelCase names, 64-bit integers as strings, enums by name, zero values omitted).
type NodeTelemetry struct {
	ID           uint32             `json:"id,omitempty"`
	Name         string             `json:"name,omitempty"`
	Status       NodeStatus         `json:"status,omitempty"`
	Fw           string             `json:"fw,omitempty"`
	BootAtMs     I64                `json:"bootAtMs,omitempty"`
	TemperatureC float64            `json:"temperatureC,omitempty"`
	FanRpm       uint32             `json:"fanRpm,omitempty"`
	PsuVoltageV  float64            `json:"psuVoltageV,omitempty"`
	Optical      *Optical           `json:"optical,omitempty"`
	Bands        []Band             `json:"bands,omitempty"`
	Metrics      map[string]float64 `json:"metrics,omitempty"`
	Alarms       []Alarm            `json:"alarms,omitempty"`
	Chain        uint32             `json:"chain,omitempty"`
	Hop          uint32             `json:"hop,omitempty"`
}

// ReportRequest is das.v1.ReportRequest. State stays raw so the server can read
// it as a message (full report) or as a tree of changed fields (delta).
type ReportRequest struct {
	NodeID       uint32          `json:"nodeId,omitempty"`
	Seq          U64             `json:"seq,omitempty"`
	StateHash    U64             `json:"stateHash,omitempty"`
	BaseHash     U64             `json:"baseHash,omitempty"`
	Full         bool            `json:"full,omitempty"`
	State        json.RawMessage `json:"state,omitempty"`
	ChangedPaths []string        `json:"changedPaths,omitempty"`
	ReportedAtMs I64             `json:"reportedAtMs,omitempty"`
}

// ReportResponse is das.v1.ReportResponse.
type ReportResponse struct {
	NodeID        uint32 `json:"nodeId,omitempty"`
	AckedSeq      U64    `json:"ackedSeq,omitempty"`
	Action        Action `json:"action,omitempty"`
	MinIntervalMs uint32 `json:"minIntervalMs,omitempty"`
}

var statusToWire = map[string]NodeStatus{"online": "NODE_STATUS_ONLINE", "degraded": "NODE_STATUS_DEGRADED", "offline": "NODE_STATUS_OFFLINE"}
var severityToWire = map[string]Severity{"minor": "ALARM_SEVERITY_MINOR", "major": "ALARM_SEVERITY_MAJOR", "critical": "ALARM_SEVERITY_CRITICAL"}

func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0 // proto3 JSON can carry NaN as a string; the REST side publishes null. Kept simple here.
	}
	return v
}

// FromState converts the master's model to the wire message.
func FromState(s *telemetry.State) NodeTelemetry {
	n := NodeTelemetry{ID: s.ID, Name: s.Name, Status: statusToWire[s.Status], Fw: s.Fw, BootAtMs: I64(s.BootAt),
		TemperatureC: finite(s.TemperatureC), FanRpm: uint32(max(0, s.FanRpm)), PsuVoltageV: finite(s.PsuVoltageV),
		Chain: uint32(max(0, s.Chain)), Hop: uint32(max(0, s.Hop))}
	if o := s.Optical; o != (telemetry.Optical{}) {
		n.Optical = &Optical{RxDbm: finite(o.RxDbm), TxDbm: finite(o.TxDbm), LaserBiasMa: finite(o.LaserBiasMa)}
	}
	for _, b := range s.Bands {
		n.Bands = append(n.Bands, Band{Name: b.Name, Enabled: b.Enabled, DlOutDbm: finite(b.DlOutDbm), UlInDbm: finite(b.UlInDbm),
			DlGainDb: float64(b.DlGainDb), UlGainDb: float64(b.UlGainDb), Vswr: finite(b.Vswr)})
	}
	if len(s.Metrics) > 0 {
		n.Metrics = make(map[string]float64, len(s.Metrics))
		for _, m := range s.Metrics {
			n.Metrics[m.Name] = finite(m.Value)
		}
	}
	for _, a := range s.Alarms {
		n.Alarms = append(n.Alarms, Alarm{Code: a.Code, Severity: severityToWire[a.Severity], SinceMs: I64(a.Since)})
	}
	return n
}

func lower(prefix, v string) string {
	if v == "" {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(v, prefix))
}

// ToState converts a wire message to the master's model. Remote Nodes hash
// ToState(FromState(x)), so both sides hash exactly the same canonical bytes.
func (n *NodeTelemetry) ToState() telemetry.State {
	s := telemetry.State{ID: n.ID, Name: n.Name, Type: "remote", Chain: int(n.Chain), Hop: int(n.Hop),
		Status: lower("NODE_STATUS_", string(n.Status)), Fw: n.Fw, BootAt: int64(n.BootAtMs),
		TemperatureC: n.TemperatureC, FanRpm: int(n.FanRpm), PsuVoltageV: n.PsuVoltageV}
	if n.Optical != nil {
		s.Optical = telemetry.Optical{RxDbm: n.Optical.RxDbm, TxDbm: n.Optical.TxDbm, LaserBiasMa: n.Optical.LaserBiasMa}
	}
	for _, b := range n.Bands {
		s.Bands = append(s.Bands, telemetry.Band{Name: b.Name, Enabled: b.Enabled, DlOutDbm: b.DlOutDbm, UlInDbm: b.UlInDbm,
			DlGainDb: int(math.Round(b.DlGainDb)), UlGainDb: int(math.Round(b.UlGainDb)), Vswr: b.Vswr})
	}
	names := make([]string, 0, len(n.Metrics))
	for k := range n.Metrics {
		names = append(names, k)
	}
	slices.Sort(names) // canonical order
	for _, k := range names {
		s.Metrics = append(s.Metrics, telemetry.Metric{Name: k, Value: n.Metrics[k]})
	}
	for _, a := range n.Alarms {
		s.Alarms = append(s.Alarms, telemetry.Alarm{Code: a.Code, Severity: lower("ALARM_SEVERITY_", string(a.Severity)), Since: int64(a.SinceMs)})
	}
	return s
}

// StateHash is the hash both ends compute: FNV-1a 64 of the canonical encoding.
func StateHash(s *telemetry.State) uint64 { return telemetry.Hash64(s.Canonical()) }
