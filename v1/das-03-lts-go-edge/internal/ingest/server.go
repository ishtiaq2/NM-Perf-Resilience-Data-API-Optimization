package ingest

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"dasedge/internal/telemetry"
)

// Procedure is the Connect/gRPC path of the streaming RPC.
const Procedure = "/das.v1.NodeIngestService/Report"

// ContentType of Connect streaming with the JSON codec.
const ContentType = "application/connect+json"

const (
	flagCompressed = 0x01
	flagEndStream  = 0x02
)

// Error is a Connect error (https://connectrpc.com/docs/protocol#error-codes).
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Options configure the service.
type Options struct {
	Store *telemetry.Store
	// MaxNodeID rejects node ids above it (0: no limit).
	MaxNodeID uint32
	// MinInterval: a node reporting faster is told to SLOW_DOWN (the report is still applied).
	MinInterval time.Duration
	// MaxMessageBytes per enveloped message (default 1 MiB).
	MaxMessageBytes int
	Log             *slog.Logger
}

type mirror struct {
	mu     sync.Mutex
	valid  bool
	tree   any // the node's last state as a protobuf-JSON tree (patch base)
	state  telemetry.State
	hash   uint64
	seq    uint64
	lastAt time.Time
}

// Stats of the ingest service.
type Stats struct {
	Streams        int64 `json:"streams"`
	StreamsTotal   int64 `json:"streamsTotal"`
	Reports        int64 `json:"reports"`
	Full           int64 `json:"full"`
	Deltas         int64 `json:"deltas"`
	KeepAlives     int64 `json:"keepAlives"`
	Resyncs        int64 `json:"resyncs"`
	HashMismatches int64 `json:"hashMismatches"`
	SlowDowns      int64 `json:"slowDowns"`
	BytesIn        int64 `json:"bytesIn"`
	StreamErrors   int64 `json:"streamErrors"`
}

// Service implements NodeIngestService.Report.
type Service struct {
	o       Options
	mu      sync.Mutex
	mirrors map[uint32]*mirror

	streams, streamsTotal, reports, full, deltas, keepAlives, resyncs, mismatches, slowDowns, bytesIn, streamErrors atomic.Int64
}

// New creates the service.
func New(o Options) *Service {
	if o.MaxMessageBytes <= 0 {
		o.MaxMessageBytes = 1 << 20
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Service{o: o, mirrors: map[uint32]*mirror{}}
}

// Handler serves the RPC path (mount it on an h2c listener).
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(Procedure, s.serveReport)
	return mux
}

// Stats snapshot.
func (s *Service) Stats() Stats {
	return Stats{s.streams.Load(), s.streamsTotal.Load(), s.reports.Load(), s.full.Load(), s.deltas.Load(), s.keepAlives.Load(),
		s.resyncs.Load(), s.mismatches.Load(), s.slowDowns.Load(), s.bytesIn.Load(), s.streamErrors.Load()}
}

func (s *Service) mirrorOf(id uint32) *mirror {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.mirrors[id]
	if !ok {
		m = &mirror{}
		s.mirrors[id] = m
	}
	return m
}

// readEnvelope reads one Connect envelope: flags(1) | length(4, big-endian) | payload.
func readEnvelope(r io.Reader, limit int) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, errorf("invalid_argument", "truncated envelope header")
		}
		return 0, nil, err // io.EOF: the client closed its side of the stream
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if int64(n) > int64(limit) {
		return 0, nil, errorf("resource_exhausted", "message of %d bytes exceeds %d", n, limit)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, errorf("invalid_argument", "truncated message")
	}
	return hdr[0], payload, nil
}

func writeEnvelope(w io.Writer, flags byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	buf[0] = flags
	binary.BigEndian.PutUint32(buf[1:], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

func (s *Service) serveReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != ContentType {
		w.Header().Set("Accept-Post", ContentType)
		http.Error(w, "this endpoint speaks the Connect streaming protocol with the JSON codec", http.StatusUnsupportedMediaType)
		return
	}
	rc := http.NewResponseController(w)
	if r.ProtoMajor == 1 {
		_ = rc.EnableFullDuplex() // bidirectional streaming needs HTTP/2; this keeps HTTP/1.1 tools usable
	}
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush() // the client may wait for headers before it starts sending
	s.streams.Add(1)
	s.streamsTotal.Add(1)
	defer s.streams.Add(-1)

	end := func(e *Error) {
		body := []byte("{}")
		if e != nil {
			s.streamErrors.Add(1)
			body, _ = json.Marshal(map[string]*Error{"error": e})
		}
		_ = writeEnvelope(w, flagEndStream, body)
		_ = rc.Flush()
	}
	if enc := r.Header.Get("Connect-Content-Encoding"); enc != "" && enc != "identity" {
		end(errorf("unimplemented", "message compression %q is not supported", enc))
		return
	}
	var streamNode uint32
	for {
		flags, payload, err := readEnvelope(r.Body, s.o.MaxMessageBytes)
		if err != nil {
			var ce *Error
			switch {
			case errors.Is(err, io.EOF):
				end(nil)
			case errors.As(err, &ce):
				end(ce)
			default: // connection gone
			}
			return
		}
		s.bytesIn.Add(int64(len(payload) + 5))
		if flags&flagCompressed != 0 {
			end(errorf("internal", "compressed message without a negotiated encoding"))
			return
		}
		var req ReportRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			end(errorf("invalid_argument", "invalid ReportRequest: %v", err))
			return
		}
		if req.NodeID == 0 || (s.o.MaxNodeID > 0 && req.NodeID > s.o.MaxNodeID) {
			end(errorf("invalid_argument", "node_id %d out of range", req.NodeID))
			return
		}
		if streamNode == 0 {
			streamNode = req.NodeID
		} else if req.NodeID != streamNode {
			end(errorf("invalid_argument", "one stream per node: stream of node %d got node %d", streamNode, req.NodeID))
			return
		}
		resp := s.apply(&req)
		out, _ := json.Marshal(resp)
		if err := writeEnvelope(w, 0, out); err != nil {
			return
		}
		_ = rc.Flush()
	}
}

// apply processes one report and decides the answer.
func (s *Service) apply(req *ReportRequest) *ReportResponse {
	s.reports.Add(1)
	m := s.mirrorOf(req.NodeID)
	m.mu.Lock()
	defer m.mu.Unlock()
	resp := &ReportResponse{NodeID: req.NodeID, AckedSeq: req.Seq, Action: ActionOK}
	now := time.Now()
	if s.o.MinInterval > 0 && !m.lastAt.IsZero() && now.Sub(m.lastAt) < s.o.MinInterval {
		resp.Action, resp.MinIntervalMs = ActionSlowDown, uint32(s.o.MinInterval/time.Millisecond)
		s.slowDowns.Add(1)
	}
	m.lastAt = now
	resync := func(reason string) *ReportResponse {
		s.resyncs.Add(1)
		s.o.Log.Debug("ingest_resync", "nodeId", req.NodeID, "seq", uint64(req.Seq), "reason", reason)
		return &ReportResponse{NodeID: req.NodeID, AckedSeq: req.Seq, Action: ActionResync}
	}
	commit := func(tree any, st telemetry.State, h uint64) {
		m.valid, m.tree, m.state, m.hash, m.seq = true, tree, st, h, uint64(req.Seq)
		s.o.Store.Ingest(&telemetry.Report{State: st, Seq: uint64(req.Seq), ReportedAt: int64(req.ReportedAtMs)})
	}

	if req.Full { // authoritative: accepted whatever the sequence (the node may have restarted)
		var nt NodeTelemetry
		if err := json.Unmarshal(req.State, &nt); err != nil {
			return resync("full state does not decode: " + err.Error())
		}
		nt.ID = req.NodeID
		st := nt.ToState()
		h := StateHash(&st)
		if req.StateHash != 0 && uint64(req.StateHash) != h {
			s.mismatches.Add(1) // the node hashes differently (version skew): deltas will RESYNC, fulls still work
			s.o.Log.Warn("ingest_hash_mismatch", "nodeId", req.NodeID, "node", uint64(req.StateHash), "master", h)
		}
		s.full.Add(1)
		commit(toTree(FromState(&st)), st, h)
		return resp
	}

	if !m.valid || uint64(req.BaseHash) != m.hash {
		return resync("unknown base state")
	}
	if len(req.ChangedPaths) == 0 { // keep-alive: nothing changed since base
		if req.StateHash != 0 && uint64(req.StateHash) != m.hash {
			return resync("keep-alive hash differs")
		}
		s.keepAlives.Add(1)
		m.seq = uint64(req.Seq)
		s.o.Store.Ingest(&telemetry.Report{State: m.state, Seq: uint64(req.Seq), ReportedAt: int64(req.ReportedAtMs)})
		return resp
	}
	if uint64(req.Seq) <= m.seq {
		return resync("sequence went backwards")
	}
	delta, err := decodeTree(req.State)
	if err != nil {
		return resync("delta state does not decode")
	}
	tree := clone(m.tree)
	if err := Apply(tree, delta, req.ChangedPaths); err != nil {
		return resync(err.Error())
	}
	raw, _ := json.Marshal(tree)
	var nt NodeTelemetry
	if err := json.Unmarshal(raw, &nt); err != nil {
		return resync("patched state does not decode: " + err.Error())
	}
	nt.ID = req.NodeID
	st := nt.ToState()
	h := StateHash(&st)
	if h != uint64(req.StateHash) {
		s.mismatches.Add(1)
		return resync("hash mismatch after applying " + strconv.Itoa(len(req.ChangedPaths)) + " paths")
	}
	s.deltas.Add(1)
	commit(tree, st, h)
	return resp
}
