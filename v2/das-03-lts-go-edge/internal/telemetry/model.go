// Package telemetry holds the Master Node's view of every Remote Node
// (volatile_data): typed state, change detection with field hygiene and
// deadbands, revisions, deltas and pre-serialised snapshots.
package telemetry

import (
	"hash/fnv"
	"math"
	"strconv"
	"unicode/utf8"
)

// Band is one RF band of a Remote Node.
type Band struct {
	Name     string
	Enabled  bool
	DlOutDbm float64
	UlInDbm  float64
	DlGainDb int
	UlGainDb int
	Vswr     float64
}

// Alarm is an active alarm.
type Alarm struct {
	Code     string
	Severity string
	Since    int64
}

// Optical link levels on the fiber.
type Optical struct{ RxDbm, TxDbm, LaserBiasMa float64 }

// Metric is a named numeric metric (kept sorted by name for canonical encoding).
type Metric struct {
	Name  string
	Value float64
}

// State is the normalised node state: what is compared, hashed and published.
// Per-report counters (sequence, report time, uptime) are deliberately absent.
type State struct {
	ID           uint32
	Name         string
	Type         string
	Chain, Hop   int
	Status       string
	Fw           string
	BootAt       int64
	TemperatureC float64
	FanRpm       int
	PsuVoltageV  float64
	Optical      Optical
	Bands        []Band
	Metrics      []Metric
	Alarms       []Alarm
}

// Report is what a Remote Node sends: its state plus per-report counters.
type Report struct {
	State
	Seq        uint64
	ReportedAt int64 // epoch ms
	UptimeS    int64
}

// Deadbands in engineering units (same defaults as the Node.js services).
type Deadbands struct {
	TemperatureC, FanRpm, PsuVoltageV    float64
	RxDbm, TxDbm, LaserBiasMa            float64
	DlOutDbm, UlInDbm, Vswr, MetricValue float64
	// BootAtMs absorbs the ±1 s jitter of a boot time derived from a truncated
	// uptime counter; a real reboot moves it by far more.
	BootAtMs float64
}

// DefaultDeadbands are product decisions; agree them with the RF/ops team.
var DefaultDeadbands = Deadbands{
	TemperatureC: 0.5, FanRpm: 250, PsuVoltageV: 0.25,
	RxDbm: 0.3, TxDbm: 0.3, LaserBiasMa: 1.0,
	DlOutDbm: 1.0, UlInDbm: 3, Vswr: 0.05, MetricValue: 2,
	BootAtMs: 10000,
}

func hold(prev, next, db float64) float64 {
	if db > 0 && math.Abs(next-prev) < db {
		return prev
	}
	return next
}

// ApplyDeadband keeps previously published analog values while the new ones
// stay inside their deadband (SCADA/OPC UA-style reporting). prev may be nil.
func ApplyDeadband(prev *State, next State, db Deadbands) State {
	if prev == nil {
		return next
	}
	out := next
	out.BootAt = int64(hold(float64(prev.BootAt), float64(next.BootAt), db.BootAtMs))
	out.TemperatureC = hold(prev.TemperatureC, next.TemperatureC, db.TemperatureC)
	out.FanRpm = int(hold(float64(prev.FanRpm), float64(next.FanRpm), db.FanRpm))
	out.PsuVoltageV = hold(prev.PsuVoltageV, next.PsuVoltageV, db.PsuVoltageV)
	out.Optical = Optical{
		RxDbm:       hold(prev.Optical.RxDbm, next.Optical.RxDbm, db.RxDbm),
		TxDbm:       hold(prev.Optical.TxDbm, next.Optical.TxDbm, db.TxDbm),
		LaserBiasMa: hold(prev.Optical.LaserBiasMa, next.Optical.LaserBiasMa, db.LaserBiasMa),
	}
	out.Bands = make([]Band, len(next.Bands))
	for i, b := range next.Bands {
		if i < len(prev.Bands) && prev.Bands[i].Name == b.Name {
			pb := prev.Bands[i]
			b.DlOutDbm = hold(pb.DlOutDbm, b.DlOutDbm, db.DlOutDbm)
			b.UlInDbm = hold(pb.UlInDbm, b.UlInDbm, db.UlInDbm)
			b.Vswr = hold(pb.Vswr, b.Vswr, db.Vswr)
		}
		out.Bands[i] = b
	}
	out.Metrics = make([]Metric, len(next.Metrics))
	for i, m := range next.Metrics {
		if i < len(prev.Metrics) && prev.Metrics[i].Name == m.Name {
			m.Value = hold(prev.Metrics[i].Value, m.Value, db.MetricValue)
		}
		out.Metrics[i] = m
	}
	return out
}

// --------------------------------------------------------------- encoding

// appendFloat writes the shortest decimal that round-trips a float64. NaN and
// ±Inf (a failed sensor) have no JSON form and are written as null.
func appendFloat(b []byte, v float64) []byte {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return append(b, "null"...)
	}
	return strconv.AppendFloat(b, v, 'f', -1, 64)
}

// appendString writes a JSON string (RFC 8259): quote, backslash and control
// characters are escaped, invalid UTF-8 becomes U+FFFD. (strconv.Quote is not
// JSON: it emits \x escapes.)
func appendString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				b = append(b, '\\', c)
			case c < 0x20:
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			default:
				b = append(b, c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, `�`...)
		} else {
			b = append(b, s[i:i+size]...)
		}
		i += size
	}
	return append(b, '"')
}

// AppendJSON writes the state as a JSON object with the contract's field names
// (openapi.yaml NodeTelemetry). extra lets callers append per-report fields.
// Field order is fixed: the output is canonical and its hash is stable.
func (s *State) AppendJSON(b []byte, extra func([]byte) []byte) []byte {
	b = append(b, `{"id":`...)
	b = strconv.AppendUint(b, uint64(s.ID), 10)
	b = append(b, `,"name":`...)
	b = appendString(b, s.Name)
	b = append(b, `,"type":`...)
	b = appendString(b, s.Type)
	b = append(b, `,"chain":`...)
	b = strconv.AppendInt(b, int64(s.Chain), 10)
	b = append(b, `,"hop":`...)
	b = strconv.AppendInt(b, int64(s.Hop), 10)
	b = append(b, `,"status":`...)
	b = appendString(b, s.Status)
	b = append(b, `,"fw":`...)
	b = appendString(b, s.Fw)
	b = append(b, `,"bootAt":`...)
	b = strconv.AppendInt(b, s.BootAt, 10)
	b = append(b, `,"temperatureC":`...)
	b = appendFloat(b, s.TemperatureC)
	b = append(b, `,"fanRpm":`...)
	b = strconv.AppendInt(b, int64(s.FanRpm), 10)
	b = append(b, `,"psuVoltageV":`...)
	b = appendFloat(b, s.PsuVoltageV)
	b = append(b, `,"optical":{"rxDbm":`...)
	b = appendFloat(b, s.Optical.RxDbm)
	b = append(b, `,"txDbm":`...)
	b = appendFloat(b, s.Optical.TxDbm)
	b = append(b, `,"laserBiasMa":`...)
	b = appendFloat(b, s.Optical.LaserBiasMa)
	b = append(b, `},"bands":[`...)
	for i, x := range s.Bands {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":`...)
		b = appendString(b, x.Name)
		b = append(b, `,"enabled":`...)
		b = strconv.AppendBool(b, x.Enabled)
		b = append(b, `,"dlOutDbm":`...)
		b = appendFloat(b, x.DlOutDbm)
		b = append(b, `,"ulInDbm":`...)
		b = appendFloat(b, x.UlInDbm)
		b = append(b, `,"dlGainDb":`...)
		b = strconv.AppendInt(b, int64(x.DlGainDb), 10)
		b = append(b, `,"ulGainDb":`...)
		b = strconv.AppendInt(b, int64(x.UlGainDb), 10)
		b = append(b, `,"vswr":`...)
		b = appendFloat(b, x.Vswr)
		b = append(b, '}')
	}
	b = append(b, `],"metrics":{`...)
	for i, m := range s.Metrics {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendString(b, m.Name)
		b = append(b, ':')
		b = appendFloat(b, m.Value)
	}
	b = append(b, `},"alarms":[`...)
	for i, a := range s.Alarms {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"code":`...)
		b = appendString(b, a.Code)
		b = append(b, `,"severity":`...)
		b = appendString(b, a.Severity)
		b = append(b, `,"since":`...)
		b = strconv.AppendInt(b, a.Since, 10)
		b = append(b, '}')
	}
	b = append(b, ']')
	if extra != nil {
		b = extra(b)
	}
	return append(b, '}')
}

// Canonical encoding (no per-report fields) and its FNV-1a 64 hash. Remote
// Nodes compute the same hash over the same encoding, so the master can verify
// a delta without seeing the whole state (see internal/ingest).
func (s *State) Canonical() []byte { return s.AppendJSON(make([]byte, 0, 1400), nil) }

// Hash64 of a canonical encoding.
func Hash64(canonical []byte) uint64 {
	h := fnv.New64a()
	h.Write(canonical)
	return h.Sum64()
}
