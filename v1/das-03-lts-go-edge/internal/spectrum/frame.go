package spectrum

import (
	"encoding/binary"
	"errors"
	"math"
)

// DSPC v1 binary spectrum frame (contract/spectrum-binary-frame.md), little-endian.
const (
	ContentType  = "application/vnd.das.spectrum"
	frameMagic   = 0x43505344
	frameVersion = 1
	HeaderBytes  = 48
	EncI16       = 1
	EncF32       = 2
	i16NoData    = -32768
)

// FrameMeta is everything in a frame header except the payload.
type FrameMeta struct {
	SweepID     uint32
	NodeID      uint32
	Port        uint16
	StartHz     float64
	StepHz      float64
	TimestampMs float64
	Decimated   bool
}

// EncodeFrame builds a DSPC v1 frame. enc is EncI16 (centi-dBm) or EncF32.
func EncodeFrame(m FrameMeta, power []float32, enc byte) []byte {
	per := 2
	if enc == EncF32 {
		per = 4
	}
	b := make([]byte, HeaderBytes+len(power)*per)
	binary.LittleEndian.PutUint32(b[0:], frameMagic)
	b[4] = frameVersion
	b[5] = enc
	if m.Decimated {
		binary.LittleEndian.PutUint16(b[6:], 1)
	}
	binary.LittleEndian.PutUint32(b[8:], m.SweepID)
	binary.LittleEndian.PutUint32(b[12:], m.NodeID)
	binary.LittleEndian.PutUint16(b[16:], m.Port)
	binary.LittleEndian.PutUint32(b[20:], uint32(len(power)))
	binary.LittleEndian.PutUint64(b[24:], math.Float64bits(m.StartHz))
	binary.LittleEndian.PutUint64(b[32:], math.Float64bits(m.StepHz))
	binary.LittleEndian.PutUint64(b[40:], math.Float64bits(m.TimestampMs))
	p := b[HeaderBytes:]
	for i, v := range power {
		if enc == EncF32 {
			binary.LittleEndian.PutUint32(p[i*4:], math.Float32bits(v))
			continue
		}
		var q int16 = i16NoData
		if v == v { // not NaN
			r := math.Round(float64(v) * 100)
			q = int16(max(-32767, min(32767, r)))
		}
		binary.LittleEndian.PutUint16(p[i*2:], uint16(q))
	}
	return b
}

// DecodeFrame parses a DSPC v1 frame.
func DecodeFrame(b []byte) (FrameMeta, []float32, error) {
	var m FrameMeta
	if len(b) < HeaderBytes {
		return m, nil, errors.New("dspc: frame too short")
	}
	if binary.LittleEndian.Uint32(b) != frameMagic || b[4] != frameVersion {
		return m, nil, errors.New("dspc: bad magic or version")
	}
	enc := b[5]
	n := int(binary.LittleEndian.Uint32(b[20:]))
	per := 2
	if enc == EncF32 {
		per = 4
	} else if enc != EncI16 {
		return m, nil, errors.New("dspc: unknown encoding")
	}
	if len(b) < HeaderBytes+n*per {
		return m, nil, errors.New("dspc: truncated payload")
	}
	m = FrameMeta{
		SweepID:     binary.LittleEndian.Uint32(b[8:]),
		NodeID:      binary.LittleEndian.Uint32(b[12:]),
		Port:        binary.LittleEndian.Uint16(b[16:]),
		StartHz:     math.Float64frombits(binary.LittleEndian.Uint64(b[24:])),
		StepHz:      math.Float64frombits(binary.LittleEndian.Uint64(b[32:])),
		TimestampMs: math.Float64frombits(binary.LittleEndian.Uint64(b[40:])),
		Decimated:   binary.LittleEndian.Uint16(b[6:])&1 == 1,
	}
	out := make([]float32, n)
	p := b[HeaderBytes:]
	for i := range out {
		if enc == EncF32 {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(p[i*4:]))
			continue
		}
		q := int16(binary.LittleEndian.Uint16(p[i*2:]))
		if q == i16NoData {
			out[i] = float32(math.NaN())
		} else {
			out[i] = float32(q) / 100
		}
	}
	return m, out, nil
}
