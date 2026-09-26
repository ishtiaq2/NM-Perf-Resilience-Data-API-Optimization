package spectrum

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"
)

// Params identify one analyzer configuration (one sweep session).
type Params struct {
	NodeID  uint32
	Port    uint16
	StartHz float64
	StopHz  float64
	Points  int
	RbwHz   float64
}

// Key of the sweep session.
func (p Params) Key() string {
	return fmt.Sprintf("%d:%d:%g:%g:%d", p.NodeID, p.Port, p.StartHz, p.StopHz, p.Points)
}

// StepHz of the (full resolution) frequency grid.
func (p Params) StepHz() float64 { return (p.StopHz - p.StartHz) / float64(p.Points-1) }

// ParseParams validates query values; empty strings take the defaults.
func ParseParams(nodeID, port, startHz, stopHz, points string, defaultPoints int) (Params, error) {
	p := Params{NodeID: 1, Port: 1, StartHz: 700e6, StopHz: 2700e6, Points: defaultPoints, RbwHz: 30000}
	if v, err := strconv.ParseUint(nodeID, 10, 32); err == nil && v > 0 {
		p.NodeID = uint32(v)
	}
	if v, err := strconv.ParseUint(port, 10, 16); err == nil && v > 0 {
		p.Port = uint16(v)
	}
	if v, err := strconv.ParseFloat(startHz, 64); err == nil && v > 0 {
		p.StartHz = v
	}
	if v, err := strconv.ParseFloat(stopHz, 64); err == nil && v > 0 {
		p.StopHz = v
	}
	if v, err := strconv.Atoi(points); err == nil && v > 0 {
		p.Points = v
	}
	if p.StopHz <= p.StartHz {
		return p, errors.New("stopHz must be greater than startHz")
	}
	p.Points = max(101, min(200001, p.Points))
	return p, nil
}

// Hardware is the analyzer driver seam. On the device this wraps the FPGA/DSP
// interface and returns calibrated power values: binary end to end, no JSON.
type Hardware interface {
	Sweep(ctx context.Context, p Params) ([]float32, error)
}

type carrier struct{ center, bw, level float64 }

var carriers = []carrier{
	{773e6, 10e6, -62}, {783e6, 10e6, -64}, {796e6, 10e6, -58}, {806e6, 10e6, -60}, {816e6, 10e6, -63},
	{935.4e6, 0.2e6, -57}, {936.2e6, 0.2e6, -59}, {937.0e6, 0.2e6, -58}, {947.6e6, 5e6, -61}, {955.0e6, 5e6, -63},
	{1815e6, 20e6, -64}, {1840e6, 20e6, -62}, {1867.5e6, 15e6, -66},
	{2120e6, 20e6, -63}, {2142.5e6, 15e6, -61}, {2160e6, 10e6, -65},
	{2630e6, 20e6, -60}, {2655e6, 20e6, -62}, {2675e6, 20e6, -64},
}

var spurs = []struct{ f, level float64 }{{1001.3e6, -79}, {1500.0e6, -86}, {2400.0e6, -81}}

const noiseFloor = -100.0

func baseline(p Params) []float64 {
	step := p.StepHz()
	base := make([]float64, p.Points)
	for i := range base {
		base[i] = noiseFloor
	}
	for _, c := range carriers {
		half := c.bw / 2
		edge := math.Max(c.bw*0.03, 2*step)
		i0 := max(0, int(math.Floor((c.center-half-edge-p.StartHz)/step)))
		i1 := min(p.Points-1, int(math.Ceil((c.center+half+edge-p.StartHz)/step)))
		for i := i0; i <= i1; i++ {
			d := math.Abs(p.StartHz + float64(i)*step - c.center)
			var lvl float64
			switch {
			case d <= half-edge:
				lvl = c.level
			case d <= half+edge:
				t := (d - (half - edge)) / (2 * edge)
				lvl = c.level + (noiseFloor-c.level)*(1-math.Cos(math.Pi*t))/2
			default:
				continue
			}
			base[i] = math.Max(base[i], lvl)
		}
	}
	for _, s := range spurs {
		i := int(math.Round((s.f - p.StartHz) / step))
		if i >= 0 && i < p.Points {
			base[i] = math.Max(base[i], s.level)
		}
	}
	return base
}

type model struct {
	templates [][]float32
	next      int
}

// SimHardware simulates the analyzer: a sweep takes SweepTime (sleep, no CPU),
// sweeps on the same analyzer are serialised, and data comes from pre-generated
// noisy templates (EU DAS downlink carriers, CW spurs, an intermittent interferer).
type SimHardware struct {
	SweepTime time.Duration
	mu        sync.Mutex
	models    map[string]*model
	locks     map[string]*sync.Mutex
}

// NewSimHardware creates the simulator.
func NewSimHardware(sweepTime time.Duration) *SimHardware {
	return &SimHardware{SweepTime: sweepTime, models: map[string]*model{}, locks: map[string]*sync.Mutex{}}
}

func (h *SimHardware) model(p Params) *model {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.models[p.Key()]; ok {
		return m
	}
	base := baseline(p)
	rng := rand.New(rand.NewPCG(uint64(p.NodeID)*31+uint64(p.Port), 7919))
	step := p.StepHz()
	m := &model{}
	for t := 0; t < 4; t++ {
		out := make([]float32, len(base))
		for i, b := range base {
			sd := 0.45
			if b <= noiseFloor+0.5 {
				sd = 1.6
			}
			out[i] = float32(b + rng.NormFloat64()*sd)
		}
		if t%3 == 1 { // intermittent interferer at 1890 MHz
			i0 := max(0, int((1890e6-0.2e6-p.StartHz)/step))
			i1 := min(len(out)-1, int((1890e6+0.2e6-p.StartHz)/step))
			lvl := -74 + (rng.Float64()-0.5)*6
			for i := i0; i <= i1; i++ {
				out[i] = float32(math.Max(float64(out[i]), lvl+rng.NormFloat64()*0.8))
			}
		}
		// The driver reports 0.01 dB resolution.
		for i := range out {
			out[i] = float32(math.Round(float64(out[i])*100) / 100)
		}
		m.templates = append(m.templates, out)
	}
	if len(h.models) >= 16 {
		for k := range h.models {
			delete(h.models, k)
			break
		}
	}
	h.models[p.Key()] = m
	return m
}

// Prewarm generates the templates ahead of time.
func (h *SimHardware) Prewarm(ps ...Params) {
	for _, p := range ps {
		h.model(p)
	}
}

// Sweep implements Hardware.
func (h *SimHardware) Sweep(ctx context.Context, p Params) ([]float32, error) {
	akey := fmt.Sprintf("%d:%d", p.NodeID, p.Port)
	h.mu.Lock()
	lk, ok := h.locks[akey]
	if !ok {
		lk = &sync.Mutex{}
		h.locks[akey] = lk
	}
	h.mu.Unlock()
	lk.Lock() // one physical analyzer per node/port
	defer lk.Unlock()
	m := h.model(p)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(h.SweepTime):
	}
	h.mu.Lock()
	tpl := m.templates[m.next]
	m.next = (m.next + 1) % len(m.templates)
	h.mu.Unlock()
	return append([]float32(nil), tpl...), nil
}
