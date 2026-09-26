package spectrum

import (
	"bytes"
	"compress/gzip"
	"math"
	"strconv"
	"sync"
)

func round2(v float32) float64 { return math.Round(float64(v)*100) / 100 }

func appendNum(b []byte, v float64) []byte { return strconv.AppendFloat(b, v, 'f', -1, 64) }

// LegacyJSON renders the shape the shipped frontend consumes:
// {"sweepId",...,"points":[{"frequency":Hz,"power":dBm},...]}. It is ~40 bytes per
// point, kept only for backward compatibility.
func LegacyJSON(s *Sweep) []byte {
	b := make([]byte, 0, 64+len(s.Power)*40)
	b = append(b, `{"sweepId":`...)
	b = strconv.AppendUint(b, uint64(s.ID), 10)
	b = append(b, `,"nodeId":`...)
	b = strconv.AppendUint(b, uint64(s.P.NodeID), 10)
	b = append(b, `,"port":`...)
	b = strconv.AppendUint(b, uint64(s.P.Port), 10)
	b = append(b, `,"timestamp":`...)
	b = strconv.AppendInt(b, s.Timestamp, 10)
	b = append(b, `,"startHz":`...)
	b = appendNum(b, s.P.StartHz)
	b = append(b, `,"stopHz":`...)
	b = appendNum(b, s.P.StopHz)
	b = append(b, `,"rbwHz":`...)
	b = appendNum(b, s.P.RbwHz)
	b = append(b, `,"points":[`...)
	step := s.P.StepHz()
	for i, v := range s.Power {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"frequency":`...)
		b = appendNum(b, math.Round(s.P.StartHz+float64(i)*step))
		b = append(b, `,"power":`...)
		if v != v {
			b = append(b, "null"...)
		} else {
			b = appendNum(b, round2(v))
		}
		b = append(b, '}')
	}
	return append(b, "]}"...)
}

// CompactJSON renders the implicit-axis JSON of /api/spectrum/latest.
func CompactJSON(s *Sweep, power []float32, startHz, stepHz float64, decimated bool) []byte {
	b := make([]byte, 0, 160+len(power)*8)
	b = append(b, `{"sweepId":`...)
	b = strconv.AppendUint(b, uint64(s.ID), 10)
	b = append(b, `,"nodeId":`...)
	b = strconv.AppendUint(b, uint64(s.P.NodeID), 10)
	b = append(b, `,"port":`...)
	b = strconv.AppendUint(b, uint64(s.P.Port), 10)
	b = append(b, `,"timestamp":`...)
	b = strconv.AppendInt(b, s.Timestamp, 10)
	b = append(b, `,"startHz":`...)
	b = appendNum(b, startHz)
	b = append(b, `,"stepHz":`...)
	b = appendNum(b, stepHz)
	b = append(b, `,"count":`...)
	b = strconv.AppendInt(b, int64(len(power)), 10)
	b = append(b, `,"decimated":`...)
	b = strconv.AppendBool(b, decimated)
	b = append(b, `,"powerDbm":[`...)
	for i, v := range power {
		if i > 0 {
			b = append(b, ',')
		}
		if v != v {
			b = append(b, "null"...)
		} else {
			b = appendNum(b, round2(v))
		}
	}
	return append(b, "]}"...)
}

var gzPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed); return w }}

// Gzip at BestSpeed: ~85 % smaller JSON for a fraction of the default level's CPU.
func Gzip(data []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(data) / 5)
	w := gzPool.Get().(*gzip.Writer)
	w.Reset(&out)
	_, _ = w.Write(data)
	_ = w.Close()
	gzPool.Put(w)
	return out.Bytes()
}
