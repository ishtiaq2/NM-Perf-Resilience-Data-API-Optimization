// Package ws is a small, dependency-free WebSocket (RFC 6455) server
// implementation with permessage-deflate (RFC 7692, no context takeover).
//
// Why not a library: this proof of concept is built with the Go standard
// library only (the build environment had no module proxy). Two properties
// matter for the DAS edge and are designed in:
//
//   - PreparedMessage: a broadcast message is framed (and compressed) ONCE and
//     the same bytes are written to every client. Without context takeover,
//     every compressed message is independent, so this is legal. Cost grows with
//     the number of distinct messages, not with the number of engineers.
//   - Writes never block the caller for long: WriteTimeout bounds every write, and
//     the hubs give each client its own writer goroutine that coalesces updates.
//
// For production the same API can be backed by github.com/coder/websocket or
// gorilla/websocket; see internal/hub for the only call sites.
package ws

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MessageType is the WebSocket data opcode of a message.
type MessageType int

const (
	TextMessage   MessageType = 1
	BinaryMessage MessageType = 2

	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA

	// Close codes (RFC 6455 section 7.4.1).
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseProtocolError   = 1002
	CloseInvalidPayload  = 1007
	ClosePolicyViolation = 1008
	CloseMessageTooBig   = 1009
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// ErrClosed is returned after the connection has been closed.
var ErrClosed = errors.New("ws: connection closed")

// CloseError carries the close code the peer sent (or that we sent on a protocol error).
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("ws: closed with code %d %s", e.Code, e.Reason)
}

// Options for Upgrade.
type Options struct {
	// CheckOrigin rejects cross-site handshakes. nil = same-origin check (Origin host == Host) or no Origin.
	CheckOrigin func(r *http.Request) bool
	// MaxMessageSize limits messages FROM the client (default 64 KiB).
	MaxMessageSize int64
	// EnableCompression negotiates permessage-deflate when the client offers it.
	EnableCompression bool
	// WriteTimeout bounds every write (default 10 s). A client that cannot absorb data in
	// that time is disconnected instead of holding server memory.
	WriteTimeout time.Duration
}

// SameOrigin accepts handshakes without an Origin header (non-browser tools) or
// whose Origin host equals the Host the request was sent to.
func SameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	i := strings.Index(origin, "://")
	if i < 0 {
		return false
	}
	return strings.EqualFold(origin[i+3:], r.Host)
}

// Conn is a server-side WebSocket connection.
type Conn struct {
	nc           net.Conn
	br           *bufio.Reader
	writeMu      sync.Mutex
	compress     bool
	maxMsg       int64
	writeTimeout time.Duration
	closeOnce    sync.Once
	closed       chan struct{}
	sentClose    bool
	// Subprotocol-free; Extensions holds the negotiated extension string (for diagnostics).
	Extensions string
	bytesOut   int64
	statsMu    sync.Mutex
}

func headerContainsToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func offersDeflate(h http.Header) bool {
	for _, v := range h.Values("Sec-WebSocket-Extensions") {
		for _, ext := range strings.Split(v, ",") {
			name := strings.TrimSpace(strings.SplitN(ext, ";", 2)[0])
			if strings.EqualFold(name, "permessage-deflate") {
				return true
			}
		}
	}
	return false
}

// Upgrade performs the opening handshake. On error it has already written an HTTP error response.
func Upgrade(w http.ResponseWriter, r *http.Request, opt Options) (*Conn, error) {
	if r.Method != http.MethodGet || !headerContainsToken(r.Header, "Connection", "upgrade") || !headerContainsToken(r.Header, "Upgrade", "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return nil, errors.New("ws: not a websocket handshake")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusBadRequest)
		return nil, errors.New("ws: bad version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if k, err := base64.StdEncoding.DecodeString(key); err != nil || len(k) != 16 {
		http.Error(w, "bad Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("ws: bad key")
	}
	check := opt.CheckOrigin
	if check == nil {
		check = SameOrigin
	}
	if !check(r) {
		http.Error(w, "cross-origin websocket refused", http.StatusForbidden)
		return nil, errors.New("ws: origin refused")
	}
	// ResponseController also finds the connection through middleware wrappers (Unwrap).
	nc, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "websocket needs HTTP/1.1", http.StatusHTTPVersionNotSupported)
		return nil, fmt.Errorf("ws: cannot hijack: %w", err)
	}
	sum := sha1.Sum([]byte(key + guid))
	c := &Conn{nc: nc, br: brw.Reader, maxMsg: opt.MaxMessageSize, writeTimeout: opt.WriteTimeout, closed: make(chan struct{})}
	if c.maxMsg <= 0 {
		c.maxMsg = 64 << 10
	}
	if c.writeTimeout <= 0 {
		c.writeTimeout = 10 * time.Second
	}
	var resp bytes.Buffer
	resp.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ")
	resp.WriteString(base64.StdEncoding.EncodeToString(sum[:]))
	resp.WriteString("\r\n")
	if opt.EnableCompression && offersDeflate(r.Header) {
		c.compress = true
		c.Extensions = "permessage-deflate; server_no_context_takeover; client_no_context_takeover"
		resp.WriteString("Sec-WebSocket-Extensions: " + c.Extensions + "\r\n")
	}
	resp.WriteString("\r\n")
	_ = nc.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	if _, err := nc.Write(resp.Bytes()); err != nil {
		nc.Close()
		return nil, err
	}
	_ = nc.SetDeadline(time.Time{})
	return c, nil
}

// Compressed reports whether permessage-deflate was negotiated.
func (c *Conn) Compressed() bool { return c.compress }

// RemoteAddr of the peer.
func (c *Conn) RemoteAddr() net.Addr { return c.nc.RemoteAddr() }

// BytesWritten on the wire so far (after compression, including frame headers).
func (c *Conn) BytesWritten() int64 { c.statsMu.Lock(); defer c.statsMu.Unlock(); return c.bytesOut }

// Done is closed when the connection is closed.
func (c *Conn) Done() <-chan struct{} { return c.closed }

// ---------------------------------------------------------------- framing

func frameHeader(buf []byte, fin bool, rsv1 bool, op byte, n int) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	if rsv1 {
		b0 |= 0x40
	}
	buf = append(buf, b0)
	switch {
	case n < 126:
		buf = append(buf, byte(n))
	case n <= 0xFFFF:
		buf = append(buf, 126, byte(n>>8), byte(n))
	default:
		buf = append(buf, 127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	return buf
}

var flateWriters = sync.Pool{New: func() any { w, _ := flate.NewWriter(nil, flate.BestSpeed); return w }}

// deflate compresses one message independently (no context takeover) and strips the
// 0x00 0x00 0xff 0xff tail of the sync flush, as RFC 7692 section 7.2.1 requires.
func deflate(data []byte) []byte {
	var out bytes.Buffer
	fw := flateWriters.Get().(*flate.Writer)
	fw.Reset(&out)
	_, _ = fw.Write(data)
	_ = fw.Flush()
	flateWriters.Put(fw)
	b := out.Bytes()
	if len(b) >= 4 && bytes.Equal(b[len(b)-4:], []byte{0, 0, 0xff, 0xff}) {
		b = b[:len(b)-4]
	}
	if len(b) == 0 {
		b = []byte{0}
	}
	return b
}

// PreparedMessage is a message framed once (plain and, lazily, compressed) and
// written to many connections.
type PreparedMessage struct {
	typ        MessageType
	data       []byte
	plainOnce  sync.Once
	plain      []byte
	deflOnce   sync.Once
	deflated   []byte
	compressOK bool
}

// NewPreparedMessage prepares data for broadcast. compressible=false skips deflate
// (e.g. binary spectrum frames, which are mostly noise).
func NewPreparedMessage(t MessageType, data []byte, compressible bool) *PreparedMessage {
	return &PreparedMessage{typ: t, data: data, compressOK: compressible && len(data) >= 512}
}

// Len is the uncompressed payload size.
func (pm *PreparedMessage) Len() int { return len(pm.data) }

func (pm *PreparedMessage) frame(compressed bool) []byte {
	if compressed && pm.compressOK {
		pm.deflOnce.Do(func() {
			z := deflate(pm.data)
			pm.deflated = append(frameHeader(make([]byte, 0, len(z)+10), true, true, byte(pm.typ), len(z)), z...)
		})
		return pm.deflated
	}
	pm.plainOnce.Do(func() {
		pm.plain = append(frameHeader(make([]byte, 0, len(pm.data)+10), true, false, byte(pm.typ), len(pm.data)), pm.data...)
	})
	return pm.plain
}

func (c *Conn) writeRaw(b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.sentClose {
		return ErrClosed // nothing may follow a close frame
	}
	return c.writeLocked(b)
}

func (c *Conn) writeLocked(b []byte) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	_ = c.nc.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	n, err := c.nc.Write(b)
	c.statsMu.Lock()
	c.bytesOut += int64(n)
	c.statsMu.Unlock()
	if err != nil {
		c.shutdown()
	}
	return err
}

// WritePrepared writes a prepared message (compressed if negotiated).
func (c *Conn) WritePrepared(pm *PreparedMessage) error { return c.writeRaw(pm.frame(c.compress)) }

// WriteMessage writes one message (compressed if negotiated and worthwhile).
func (c *Conn) WriteMessage(t MessageType, data []byte) error {
	return c.WritePrepared(NewPreparedMessage(t, data, t == TextMessage))
}

// WriteJSONText is a convenience for small text messages (never compressed).
func (c *Conn) WriteJSONText(s string) error {
	return c.writeRaw(append(frameHeader(make([]byte, 0, len(s)+10), true, false, opText, len(s)), s...))
}

// Ping sends a ping control frame.
func (c *Conn) Ping(payload []byte) error {
	return c.writeRaw(append(frameHeader(nil, true, false, opPing, len(payload)), payload...))
}

// Close starts the closing handshake and closes the TCP connection shortly after.
func (c *Conn) Close(code int, reason string) error {
	c.sendClose(code, reason)
	go func() {
		t := time.NewTimer(2 * time.Second)
		defer t.Stop()
		select {
		case <-c.closed:
		case <-t.C:
			c.shutdown()
		}
	}()
	return nil
}

// CloseNow terminates the connection without the closing handshake.
func (c *Conn) CloseNow() { c.shutdown() }

func (c *Conn) sendClose(code int, reason string) {
	p := binary.BigEndian.AppendUint16(nil, uint16(code))
	if len(reason) > 120 {
		reason = reason[:120]
	}
	p = append(p, reason...)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.sentClose {
		return
	}
	c.sentClose = true
	_ = c.writeLocked(append(frameHeader(nil, true, false, opClose, len(p)), p...))
}

func (c *Conn) shutdown() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.nc.Close()
	})
}

// ---------------------------------------------------------------- reading

func (c *Conn) protocolError(code int, msg string) error {
	c.sendClose(code, msg)
	c.shutdown()
	return &CloseError{Code: code, Reason: msg}
}

// ReadMessage returns the next data message. Control frames are handled
// internally: ping gets a pong, and close completes the closing handshake.
// deadline=0 means no read deadline.
func (c *Conn) ReadMessage(deadline time.Duration) (MessageType, []byte, error) {
	var (
		msg        []byte
		msgType    MessageType
		compressed bool
		inMessage  bool
	)
	for {
		if deadline > 0 {
			_ = c.nc.SetReadDeadline(time.Now().Add(deadline))
		}
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			c.shutdown()
			return 0, nil, err
		}
		fin := h[0]&0x80 != 0
		rsv1 := h[0]&0x40 != 0
		if h[0]&0x30 != 0 {
			return 0, nil, c.protocolError(CloseProtocolError, "reserved bits set")
		}
		op := h[0] & 0x0F
		masked := h[1]&0x80 != 0
		if !masked {
			return 0, nil, c.protocolError(CloseProtocolError, "client frames must be masked")
		}
		n := int64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				c.shutdown()
				return 0, nil, err
			}
			n = int64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				c.shutdown()
				return 0, nil, err
			}
			n = int64(binary.BigEndian.Uint64(b[:]))
		}
		// RFC 6455 5.2: the most significant bit of a 64-bit length must be 0. As an
		// int64 such a length is negative and would pass the size checks below, then
		// panic in make() and take the whole process down (found by FuzzReadMessage).
		if n < 0 {
			return 0, nil, c.protocolError(CloseProtocolError, "invalid frame length")
		}
		isControl := op&0x8 != 0
		if isControl && (n > 125 || !fin) {
			return 0, nil, c.protocolError(CloseProtocolError, "bad control frame")
		}
		if !isControl && int64(len(msg))+n > c.maxMsg {
			return 0, nil, c.protocolError(CloseMessageTooBig, "message too big")
		}
		var mask [4]byte
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			c.shutdown()
			return 0, nil, err
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			c.shutdown()
			return 0, nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
		switch op {
		case opPing:
			_ = c.writeRaw(append(frameHeader(nil, true, false, opPong, len(payload)), payload...))
			continue
		case opPong:
			continue
		case opClose:
			code := CloseNormal
			reason := ""
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload))
				reason = string(payload[2:])
			}
			c.sendClose(code, "")
			c.shutdown()
			return 0, nil, &CloseError{Code: code, Reason: reason}
		case opText, opBinary:
			if inMessage {
				return 0, nil, c.protocolError(CloseProtocolError, "new message inside a fragmented one")
			}
			if rsv1 && !c.compress {
				return 0, nil, c.protocolError(CloseProtocolError, "RSV1 without permessage-deflate")
			}
			msgType, compressed, inMessage = MessageType(op), rsv1, true
			msg = append(msg[:0], payload...)
		case opContinuation:
			if !inMessage {
				return 0, nil, c.protocolError(CloseProtocolError, "unexpected continuation")
			}
			msg = append(msg, payload...)
		default:
			return 0, nil, c.protocolError(CloseProtocolError, "reserved opcode")
		}
		if !fin {
			continue
		}
		if compressed {
			r := flate.NewReader(io.MultiReader(bytes.NewReader(msg), bytes.NewReader([]byte{0, 0, 0xff, 0xff})))
			out, err := io.ReadAll(io.LimitReader(r, c.maxMsg+1))
			_ = r.Close()
			if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, nil, c.protocolError(CloseInvalidPayload, "bad deflate data")
			}
			if int64(len(out)) > c.maxMsg {
				return 0, nil, c.protocolError(CloseMessageTooBig, "message too big")
			}
			msg = out
		}
		if msgType == TextMessage && !utf8.Valid(msg) {
			return 0, nil, c.protocolError(CloseInvalidPayload, "invalid UTF-8")
		}
		return msgType, msg, nil
	}
}
