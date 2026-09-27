// Package proto defines the Master Unit uplink protocol "das-noc.v1": messages,
// the canonical summary hash both ends compute, and the signed tokens that
// identify devices and NOC users. docs/PROTOCOL.md is the normative text; the
// Node.js agent (agent/noc-agent.js) implements the device side independently.
package proto

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"unicode/utf8"
)

// Subprotocol negotiated on the uplink WebSocket.
const Subprotocol = "das-noc.v1"

// Message types.
const (
	// Master Unit -> NOC
	THello   = "hello"   // first message after the handshake
	TSummary = "summary" // full site summary (after welcome, or on resync)
	TDelta   = "delta"   // changed top-level summary fields, chained by rev/base
	TAlarms  = "alarms"  // alarm events (incremental), or the full active list
	TDetail  = "detail"  // node-level data for drill-down, only while subscribed
	TPing    = "ping"    // application keep-alive, for clients whose WebSocket API hides ping frames
	// NOC -> Master Unit
	TWelcome   = "welcome"
	TAck       = "ack"        // cumulative: alarm events up to seq are stored
	TResync    = "resync"     // send a full summary / alarms / detail
	TDetailSub = "detail-sub" // start or stop the detail stream
	TPong      = "pong"       // answer to ping (same seq)
)

// Alarm severities, most severe first.
var Severities = []string{"critical", "major", "minor", "warning"}

// SeverityRank: critical 4 ... warning 1, unknown 0.
func SeverityRank(s string) int {
	switch s {
	case "critical":
		return 4
	case "major":
		return 3
	case "minor":
		return 2
	case "warning":
		return 1
	}
	return 0
}

// AlarmEvent is one alarm raise or clear, numbered by the Master Unit.
type AlarmEvent struct {
	Seq   uint64 `json:"seq"`            // strictly increasing within one boot of the device
	ID    string `json:"id"`             // stable key within the site, e.g. "n17:TEMP_HIGH"
	Node  int    `json:"node,omitempty"` // Remote Node id, 0 = the Master Unit itself
	Code  string `json:"code"`
	Sev   string `json:"sev"`   // critical | major | minor | warning
	State string `json:"state"` // raised | cleared
	At    int64  `json:"at"`    // device clock, ms since the Unix epoch
	Text  string `json:"text,omitempty"`
}

// Have is what the NOC already holds for a site (welcome). Nil after a NOC restart:
// the device then sends everything.
type Have struct {
	Boot   string `json:"boot"`
	Rev    uint64 `json:"rev"`
	Hash   string `json:"hash"`
	AckSeq uint64 `json:"ackSeq"`
}

// Message is the union of all uplink messages; T selects which fields apply.
type Message struct {
	T string `json:"t"`

	// hello
	Boot  string   `json:"boot,omitempty"`  // random per device boot (or agent start)
	FW    string   `json:"fw,omitempty"`    // device software version
	Agent string   `json:"agent,omitempty"` // uplink agent implementation/version
	Caps  []string `json:"caps,omitempty"`

	// summary, delta, detail
	Rev  uint64                     `json:"rev,omitempty"`
	Base uint64                     `json:"base,omitempty"`
	Hash string                     `json:"hash,omitempty"` // canonical hash after this message
	S    map[string]json.RawMessage `json:"s,omitempty"`

	// alarms
	Ev   []AlarmEvent `json:"ev,omitempty"`
	Full bool         `json:"full,omitempty"` // ev is the complete active list (all raised)
	UpTo uint64       `json:"upTo,omitempty"` // with full: the device's current seq

	// detail
	Nodes []json.RawMessage `json:"nodes,omitempty"`
	Gone  []int             `json:"gone,omitempty"`

	// welcome
	Site       string `json:"site,omitempty"`
	Tenant     string `json:"tenant,omitempty"`
	Session    string `json:"session,omitempty"`
	KeepaliveS int    `json:"keepaliveS,omitempty"`
	Have       *Have  `json:"have,omitempty"`

	// ack, resync, detail-sub
	Seq        uint64 `json:"seq,omitempty"`
	What       string `json:"what,omitempty"` // summary | alarms | detail
	Reason     string `json:"reason,omitempty"`
	On         bool   `json:"on,omitempty"`
	IntervalMs int    `json:"intervalMs,omitempty"`
}

// ---------------------------------------------------------------- canonical hash

// ErrNotCanonical: a value the canonical encoding cannot represent.
var ErrNotCanonical = errors.New("proto: value has no canonical encoding")

// AppendCanonical appends the canonical JSON of v: object keys sorted, no
// whitespace, strings and numbers exactly as JavaScript's JSON.stringify writes
// them. v is what encoding/json produces when decoding into `any`.
func AppendCanonical(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(b, "null"...), nil
	case bool:
		if x {
			return append(b, "true"...), nil
		}
		return append(b, "false"...), nil
	case float64:
		return AppendJSNumber(b, x), nil
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return b, ErrNotCanonical
		}
		return AppendJSNumber(b, f), nil
	case string:
		return AppendJSString(b, x), nil
	case []any:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ',')
			}
			var err error
			if b, err = AppendCanonical(b, e); err != nil {
				return b, err
			}
		}
		return append(b, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys) // byte order = UTF-16 order for keys without astral characters
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			b = AppendJSString(b, k)
			b = append(b, ':')
			var err error
			if b, err = AppendCanonical(b, x[k]); err != nil {
				return b, err
			}
		}
		return append(b, '}'), nil
	}
	return b, ErrNotCanonical
}

// AppendJSNumber formats f like JavaScript's Number.prototype.toString (which
// JSON.stringify uses): shortest round-trip digits, exponent form outside
// [1e-7, 1e21), "0" for negative zero, "null" for NaN and infinities.
func AppendJSNumber(b []byte, f float64) []byte {
	if f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308 {
		return append(b, "null"...)
	}
	if f == 0 {
		return append(b, '0')
	}
	abs := f
	if abs < 0 {
		abs = -abs
	}
	if abs >= 1e21 || abs < 1e-6 {
		s := strconv.FormatFloat(f, 'e', -1, 64) // 1e+21, 1.5e-07
		// JavaScript writes the exponent without leading zeros: 1.5e-7.
		if i := len(s) - 4; i > 0 && (s[i] == 'e') && (s[i+1] == '-' || s[i+1] == '+') && s[i+2] == '0' {
			s = s[:i+2] + s[i+3:]
		}
		return append(b, s...)
	}
	return strconv.AppendFloat(b, f, 'f', -1, 64)
}

const hexDigits = "0123456789abcdef"

// AppendJSString quotes s like JSON.stringify: escapes the quote, the backslash and
// control characters (\b \f \n \r \t, otherwise \u00xx); everything else, U+2028,
// U+2029 and <>& included, is written as is.
func AppendJSString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			if c < utf8.RuneSelf {
				b = append(b, c)
				i++
				continue
			}
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				b = append(b, `�`...) // not reachable from valid JSON input
			} else {
				b = append(b, s[i:i+size]...)
			}
			i += size
			continue
		}
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
		}
		i++
	}
	return append(b, '"')
}

// FNV1a64 of data.
func FNV1a64(data []byte) uint64 {
	h := uint64(0xcbf29ce484222325)
	for _, c := range data {
		h ^= uint64(c)
		h *= 0x100000001b3
	}
	return h
}

// HashHex renders a hash as 16 lowercase hex digits.
func HashHex(h uint64) string {
	s := strconv.FormatUint(h, 16)
	for len(s) < 16 {
		s = "0" + s
	}
	return s
}

// SummaryHash is the canonical hash of a summary given as raw top-level fields.
func SummaryHash(s map[string]json.RawMessage) (string, error) {
	obj := make(map[string]any, len(s))
	for k, raw := range s {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", err
		}
		obj[k] = v
	}
	b, err := AppendCanonical(make([]byte, 0, 512), obj)
	if err != nil {
		return "", err
	}
	return HashHex(FNV1a64(b)), nil
}
