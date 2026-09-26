package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dasedge/internal/telemetry"
)

// NewH2CClient returns an HTTP client that speaks HTTP/2 without TLS (prior
// knowledge), as used on the fiber management network.
func NewH2CClient() *http.Client {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: &p, MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute}}
}

// Stream is the client side of one Report stream.
type Stream struct {
	pw   *io.PipeWriter
	body io.ReadCloser
	mu   sync.Mutex
	sent atomic.Int64
}

// Open starts a Report stream at baseURL (http://master:9090).
func Open(ctx context.Context, client *http.Client, baseURL string) (*Stream, error) {
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+Procedure, pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := client.Do(req)
	if err != nil {
		pw.CloseWithError(err)
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		pw.Close()
		return nil, fmt.Errorf("ingest: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return &Stream{pw: pw, body: resp.Body}, nil
}

// Send one report (safe for concurrent use). Returns the bytes written.
func (s *Stream) Send(r *ReportRequest) (int, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeEnvelope(s.pw, 0, b); err != nil {
		return 0, err
	}
	s.sent.Add(int64(len(b) + 5))
	return len(b) + 5, nil
}

// Recv the next response; io.EOF when the server ended the stream cleanly,
// *Error when it ended it with an error.
func (s *Stream) Recv() (*ReportResponse, error) {
	flags, payload, err := readEnvelope(s.body, 1<<20)
	if err != nil {
		return nil, err
	}
	if flags&flagEndStream != 0 {
		var end struct {
			Error *Error `json:"error"`
		}
		if json.Unmarshal(payload, &end) == nil && end.Error != nil {
			return nil, end.Error
		}
		return nil, io.EOF
	}
	var resp ReportResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CloseSend ends the request side; the server then ends the stream.
func (s *Stream) CloseSend() error { return s.pw.Close() }

// Close aborts the stream.
func (s *Stream) Close() error {
	s.pw.CloseWithError(errors.New("closed"))
	return s.body.Close()
}

// BytesSent on this stream.
func (s *Stream) BytesSent() int64 { return s.sent.Load() }

// AgentStats counts what a Remote Node agent did.
type AgentStats struct {
	Offered, Suppressed, Full, Deltas, KeepAlives, Resyncs, SlowDowns, BytesSent atomic.Int64
}

// Agent is the Remote Node side: report-by-exception with deadbands, deltas
// against the last sent state, keep-alives while nothing changes, and a full
// report whenever the master asks for a resync.
type Agent struct {
	NodeID    uint32
	Deadbands telemetry.Deadbands
	KeepAlive time.Duration
	Stats     *AgentStats

	mu          sync.Mutex
	stream      *Stream
	sent        *telemetry.State // last state sent (deadbanded)
	sentTree    any
	sentHash    uint64
	needFull    bool
	seq         uint64
	lastSend    time.Time
	minInterval time.Duration
}

// NewAgent creates an agent for one node.
func NewAgent(id uint32, stats *AgentStats) *Agent {
	return &Agent{NodeID: id, Deadbands: telemetry.DefaultDeadbands, KeepAlive: 5 * time.Second, Stats: stats, needFull: true}
}

// Attach a (new) stream; the next report is a full one.
func (a *Agent) Attach(s *Stream) {
	a.mu.Lock()
	a.stream, a.needFull = s, true
	a.mu.Unlock()
}

// Offer a local measurement; the agent decides whether and what to send.
func (a *Agent) Offer(r *telemetry.Report, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Stats.Offered.Add(1)
	if a.stream == nil {
		return errors.New("no stream")
	}
	if a.minInterval > 0 && now.Sub(a.lastSend) < a.minInterval {
		a.Stats.Suppressed.Add(1)
		return nil
	}
	st := r.State
	st.ID = a.NodeID
	if a.sent != nil && !a.needFull {
		st = telemetry.ApplyDeadband(a.sent, st, a.Deadbands)
	}
	wire := FromState(&st)
	norm := wire.ToState() // hash exactly what the master will reconstruct
	h := StateHash(&norm)
	req := &ReportRequest{NodeID: a.NodeID, StateHash: U64(h), ReportedAtMs: I64(r.ReportedAt)}
	switch {
	case a.needFull || a.sent == nil:
		b, _ := json.Marshal(wire)
		req.Full, req.State = true, b
		a.Stats.Full.Add(1)
	case h == a.sentHash:
		if now.Sub(a.lastSend) < a.KeepAlive {
			a.Stats.Suppressed.Add(1)
			return nil
		}
		req.BaseHash = U64(a.sentHash)
		a.Stats.KeepAlives.Add(1)
	default:
		tree := toTree(wire)
		paths := Diff(a.sentTree, tree)
		b, _ := json.Marshal(Prune(tree, paths))
		req.BaseHash, req.State, req.ChangedPaths = U64(a.sentHash), b, paths
		a.Stats.Deltas.Add(1)
	}
	a.seq++
	req.Seq = U64(a.seq)
	n, err := a.stream.Send(req)
	if err != nil {
		return err
	}
	a.Stats.BytesSent.Add(int64(n))
	a.sent, a.sentTree, a.sentHash, a.needFull, a.lastSend = &norm, toTree(wire), h, false, now
	return nil
}

// Handle a response from the master.
func (a *Agent) Handle(r *ReportResponse) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch r.Action {
	case ActionResync:
		a.needFull = true
		a.Stats.Resyncs.Add(1)
	case ActionSlowDown:
		a.minInterval = time.Duration(r.MinIntervalMs) * time.Millisecond
		a.Stats.SlowDowns.Add(1)
	case ActionOK:
		a.minInterval = 0
	}
}
