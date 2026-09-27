// Package wsock is a small, dependency-free WebSocket (RFC 6455) implementation
// for both ends of a connection: Upgrade on the server (the NOC accepting Master
// Units and dashboards) and Dial on the client (the Master Unit agent, the fleet
// simulator). It is derived from the das-03 edge's server-only package.
//
// Built with the Go standard library only (the build environment has no module
// proxy). Properties the NOC depends on:
//
//   - Small per-connection memory: one read buffer (1 KiB by default, the HTTP
//     server's 4 KiB buffer is dropped after the handshake), no write buffer, no
//     goroutine of its own. 5 000 idle Master Units cost little more than their
//     sockets.
//   - PreparedMessage: a broadcast is framed (and optionally compressed) once and
//     the same bytes go to every dashboard.
//   - Every write is bounded by WriteTimeout, so a stuck peer is dropped instead of
//     holding memory or a lock.
//
// For production the same API can be backed by github.com/coder/websocket.
package wsock

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// MessageType is the data opcode of a message.
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

	// Close codes (RFC 6455 section 7.4.1) and the application range.
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseProtocolError   = 1002
	CloseInvalidPayload  = 1007
	ClosePolicyViolation = 1008
	CloseMessageTooBig   = 1009
	CloseTryAgainLater   = 1013
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// ErrClosed is returned after the connection has been closed.
var ErrClosed = errors.New("wsock: connection closed")

// CloseError carries the close code the peer sent (or that we sent on a protocol error).
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("wsock: closed with code %d %s", e.Code, e.Reason)
}

type role int

const (
	server role = iota
	client
)

// Conn is one WebSocket connection, server or client side.
type Conn struct {
	nc           net.Conn
	br           *bufio.Reader
	role         role
	writeMu      sync.Mutex
	compress     bool
	maxMsg       int64
	writeTimeout time.Duration
	closeOnce    sync.Once
	closed       chan struct{}
	sentClose    bool
	// Subprotocol is the negotiated subprotocol ("" if none).
	Subprotocol string
	bytesOut    atomic.Int64
	bytesIn     atomic.Int64
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

// Subprotocols lists the subprotocols a client offered, in order.
func Subprotocols(r *http.Request) []string {
	var out []string
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

func offersDeflate(h http.Header) bool {
	for _, v := range h.Values("Sec-WebSocket-Extensions") {
		for _, ext := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(strings.SplitN(ext, ";", 2)[0]), "permessage-deflate") {
				return true
			}
		}
	}
	return false
}

// SameOrigin accepts handshakes without an Origin header (devices, tools) or whose
// Origin host equals the Host the request was sent to (browsers on this origin).
func SameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	i := strings.Index(origin, "://")
	return i >= 0 && strings.EqualFold(origin[i+3:], r.Host)
}

// IsUpgrade reports whether r is a WebSocket handshake.
func IsUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && headerContainsToken(r.Header, "Connection", "upgrade") && headerContainsToken(r.Header, "Upgrade", "websocket")
}

// Options for Upgrade.
type Options struct {
	// CheckOrigin rejects cross-site handshakes; nil means SameOrigin.
	CheckOrigin func(r *http.Request) bool
	// Subprotocols this server speaks, in preference order. The first one the client
	// also offered is selected. Offered entries the server does not know (for example
	// a "bearer.<token>" credential) are ignored.
	Subprotocols []string
	// MaxMessageSize limits messages from the peer (default 64 KiB).
	MaxMessageSize int64
	// EnableCompression negotiates permessage-deflate (no context takeover) when offered.
	EnableCompression bool
	// WriteTimeout bounds every write (default 10 s).
	WriteTimeout time.Duration
	// ReadBufferSize of the connection after the handshake (default 1 KiB).
	ReadBufferSize int
}

func newConn(nc net.Conn, br *bufio.Reader, r role, maxMsg int64, writeTimeout time.Duration) *Conn {
	c := &Conn{nc: nc, br: br, role: r, maxMsg: maxMsg, writeTimeout: writeTimeout, closed: make(chan struct{})}
	if c.maxMsg <= 0 {
		c.maxMsg = 64 << 10
	}
	if c.writeTimeout <= 0 {
		c.writeTimeout = 10 * time.Second
	}
	return c
}

// Upgrade performs the server side of the opening handshake. On error it has
// already written an HTTP error response.
func Upgrade(w http.ResponseWriter, r *http.Request, opt Options) (*Conn, error) {
	if !IsUpgrade(r) {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return nil, errors.New("wsock: not a websocket handshake")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusBadRequest)
		return nil, errors.New("wsock: bad version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if k, err := base64.StdEncoding.DecodeString(key); err != nil || len(k) != 16 {
		http.Error(w, "bad Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("wsock: bad key")
	}
	check := opt.CheckOrigin
	if check == nil {
		check = SameOrigin
	}
	if !check(r) {
		http.Error(w, "cross-origin websocket refused", http.StatusForbidden)
		return nil, errors.New("wsock: origin refused")
	}
	selected := ""
	if len(opt.Subprotocols) > 0 {
		offered := Subprotocols(r)
	pick:
		for _, s := range opt.Subprotocols {
			for _, o := range offered {
				if o == s {
					selected = s
					break pick
				}
			}
		}
	}
	nc, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "websocket needs HTTP/1.1", http.StatusHTTPVersionNotSupported)
		return nil, fmt.Errorf("wsock: cannot hijack: %w", err)
	}
	br := brw.Reader
	if br.Buffered() == 0 { // drop the HTTP server's 4 KiB buffer: memory per idle connection matters
		size := opt.ReadBufferSize
		if size <= 0 {
			size = 1024
		}
		br = bufio.NewReaderSize(nc, size)
	}
	c := newConn(nc, br, server, opt.MaxMessageSize, opt.WriteTimeout)
	c.Subprotocol = selected
	sum := sha1.Sum([]byte(key + guid))
	var resp bytes.Buffer
	resp.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ")
	resp.WriteString(base64.StdEncoding.EncodeToString(sum[:]))
	resp.WriteString("\r\n")
	if selected != "" {
		resp.WriteString("Sec-WebSocket-Protocol: " + selected + "\r\n")
	}
	if opt.EnableCompression && offersDeflate(r.Header) {
		c.compress = true
		resp.WriteString("Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover; client_no_context_takeover\r\n")
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

// DialOptions for Dial.
type DialOptions struct {
	Header           http.Header   // extra handshake headers (e.g. Authorization)
	Subprotocols     []string      // offered, in order
	TLSConfig        *tls.Config   // for wss:// (nil = system roots)
	HandshakeTimeout time.Duration // connect + TLS + handshake (default 10 s)
	MaxMessageSize   int64         // limit for messages from the server (default 64 KiB)
	WriteTimeout     time.Duration // default 10 s
	ReadBufferSize   int           // default 1 KiB
	LocalAddr        net.Addr      // optional source address (load tests with many connections)
}

// HandshakeError is returned by Dial when the server answered but refused the
// upgrade. RetryAfter carries the server's Retry-After hint (admission control).
type HandshakeError struct {
	Status     int
	RetryAfter time.Duration
	Body       string
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("wsock: handshake refused: HTTP %d %s", e.Status, strings.TrimSpace(e.Body))
}

// Dial opens a client connection to a ws:// or wss:// URL.
func Dial(ctx context.Context, rawURL string, opt DialOptions) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	secure := false
	switch u.Scheme {
	case "ws", "http":
	case "wss", "https":
		secure = true
	default:
		return nil, fmt.Errorf("wsock: unsupported scheme %q", u.Scheme)
	}
	host := u.Host
	if u.Port() == "" {
		if secure {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	timeout := opt.HandshakeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := net.Dialer{LocalAddr: opt.LocalAddr, KeepAlive: 30 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(deadline)
	}
	if secure {
		cfg := opt.TLSConfig
		if cfg == nil {
			cfg = &tls.Config{}
		}
		cfg = cfg.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = u.Hostname()
		}
		cfg.NextProtos = []string{"http/1.1"}
		tc := tls.Client(nc, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, err
		}
		nc = tc
	}
	var keyRaw [16]byte
	binary.LittleEndian.PutUint64(keyRaw[:8], rand.Uint64())
	binary.LittleEndian.PutUint64(keyRaw[8:], rand.Uint64())
	key := base64.StdEncoding.EncodeToString(keyRaw[:])
	path := u.RequestURI()
	var req bytes.Buffer
	req.WriteString("GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\n")
	if len(opt.Subprotocols) > 0 {
		req.WriteString("Sec-WebSocket-Protocol: " + strings.Join(opt.Subprotocols, ", ") + "\r\n")
	}
	for k, vs := range opt.Header {
		for _, v := range vs {
			req.WriteString(k + ": " + v + "\r\n")
		}
	}
	req.WriteString("\r\n")
	if _, err := nc.Write(req.Bytes()); err != nil {
		nc.Close()
		return nil, err
	}
	size := opt.ReadBufferSize
	if size <= 0 {
		size = 1024
	}
	br := bufio.NewReaderSize(nc, size)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		nc.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		nc.Close()
		he := &HandshakeError{Status: resp.StatusCode, Body: string(body)}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
			he.RetryAfter = time.Duration(s) * time.Second
		}
		return nil, he
	}
	sum := sha1.Sum([]byte(key + guid))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		nc.Close()
		return nil, errors.New("wsock: bad Sec-WebSocket-Accept")
	}
	_ = nc.SetDeadline(time.Time{})
	c := newConn(nc, br, client, opt.MaxMessageSize, opt.WriteTimeout)
	c.Subprotocol = resp.Header.Get("Sec-WebSocket-Protocol")
	return c, nil
}

// RemoteAddr of the peer.
func (c *Conn) RemoteAddr() net.Addr { return c.nc.RemoteAddr() }

// Compressed reports whether permessage-deflate was negotiated.
func (c *Conn) Compressed() bool { return c.compress }

// BytesWritten and BytesRead on the wire so far (frame headers included).
func (c *Conn) BytesWritten() int64 { return c.bytesOut.Load() }
func (c *Conn) BytesRead() int64    { return c.bytesIn.Load() }

// Done is closed when the connection is closed.
func (c *Conn) Done() <-chan struct{} { return c.closed }

// ---------------------------------------------------------------- framing

func appendHeader(buf []byte, fin, rsv1 bool, op byte, n int, masked bool) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	if rsv1 {
		b0 |= 0x40
	}
	var m byte
	if masked {
		m = 0x80
	}
	buf = append(buf, b0)
	switch {
	case n < 126:
		buf = append(buf, m|byte(n))
	case n <= 0xFFFF:
		buf = append(buf, m|126, byte(n>>8), byte(n))
	default:
		buf = append(buf, m|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	return buf
}

var flateWriters = sync.Pool{New: func() any { w, _ := flate.NewWriter(nil, flate.BestSpeed); return w }}

// deflate compresses one message independently (no context takeover) and strips the
// sync-flush tail, as RFC 7692 section 7.2.1 requires.
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

// PreparedMessage is a server message framed once and written to many connections.
type PreparedMessage struct {
	typ        MessageType
	data       []byte
	plainOnce  sync.Once
	plain      []byte
	deflOnce   sync.Once
	deflated   []byte
	compressOK bool
}

// NewPreparedMessage prepares data for broadcast; compressible=false skips deflate.
func NewPreparedMessage(t MessageType, data []byte, compressible bool) *PreparedMessage {
	return &PreparedMessage{typ: t, data: data, compressOK: compressible && len(data) >= 512}
}

// Len is the payload size before compression.
func (pm *PreparedMessage) Len() int { return len(pm.data) }

func (pm *PreparedMessage) frame(compressed bool) []byte {
	if compressed && pm.compressOK {
		pm.deflOnce.Do(func() {
			z := deflate(pm.data)
			pm.deflated = append(appendHeader(make([]byte, 0, len(z)+10), true, true, byte(pm.typ), len(z), false), z...)
		})
		return pm.deflated
	}
	pm.plainOnce.Do(func() {
		pm.plain = append(appendHeader(make([]byte, 0, len(pm.data)+10), true, false, byte(pm.typ), len(pm.data), false), pm.data...)
	})
	return pm.plain
}

// frameFor builds one frame as this side must send it: unmasked from the server,
// masked with a fresh key from the client.
func (c *Conn) frameFor(op byte, payload []byte) []byte {
	if c.role == server {
		return append(appendHeader(make([]byte, 0, len(payload)+10), true, false, op, len(payload), false), payload...)
	}
	b := appendHeader(make([]byte, 0, len(payload)+14), true, false, op, len(payload), true)
	var key [4]byte
	binary.LittleEndian.PutUint32(key[:], rand.Uint32())
	b = append(b, key[:]...)
	start := len(b)
	b = append(b, payload...)
	for i := range payload {
		b[start+i] ^= key[i&3]
	}
	return b
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
	c.bytesOut.Add(int64(n))
	if err != nil {
		c.shutdown()
	}
	return err
}

// WritePrepared writes a prepared broadcast message (server side only).
func (c *Conn) WritePrepared(pm *PreparedMessage) error {
	if c.role != server {
		return c.WriteMessage(pm.typ, pm.data)
	}
	return c.writeRaw(pm.frame(c.compress))
}

// WriteMessage writes one data message.
func (c *Conn) WriteMessage(t MessageType, data []byte) error {
	if c.role == server && c.compress && t == TextMessage && len(data) >= 512 {
		return c.WritePrepared(NewPreparedMessage(t, data, true))
	}
	return c.writeRaw(c.frameFor(byte(t), data))
}

// WriteText writes one text message.
func (c *Conn) WriteText(data []byte) error { return c.WriteMessage(TextMessage, data) }

// Ping sends a ping control frame.
func (c *Conn) Ping(payload []byte) error { return c.writeRaw(c.frameFor(opPing, payload)) }

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
	_ = c.writeLocked(c.frameFor(opClose, p))
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

// ReadMessage returns the next data message. Control frames are handled here: a
// ping gets a pong, a close completes the closing handshake. idle > 0 is the
// longest silence (no frame at all, pings included) before the read fails.
func (c *Conn) ReadMessage(idle time.Duration) (MessageType, []byte, error) {
	var (
		msg        []byte
		msgType    MessageType
		compressed bool
		inMessage  bool
	)
	for {
		if idle > 0 {
			_ = c.nc.SetReadDeadline(time.Now().Add(idle))
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
		if c.role == server && !masked {
			return 0, nil, c.protocolError(CloseProtocolError, "client frames must be masked")
		}
		if c.role == client && masked {
			return 0, nil, c.protocolError(CloseProtocolError, "server frames must not be masked")
		}
		n := int64(h[1] & 0x7F)
		hdr := 2
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				c.shutdown()
				return 0, nil, err
			}
			n, hdr = int64(binary.BigEndian.Uint16(b[:])), hdr+2
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				c.shutdown()
				return 0, nil, err
			}
			n, hdr = int64(binary.BigEndian.Uint64(b[:])), hdr+8
		}
		// RFC 6455 5.2: the most significant bit of a 64-bit length must be 0. As an
		// int64 such a length is negative, and it must be refused before any check
		// that only looks for "too big" (found by FuzzReadMessage: a control frame
		// with a negative length reached make() and panicked the process).
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
		if masked {
			if _, err := io.ReadFull(c.br, mask[:]); err != nil {
				c.shutdown()
				return 0, nil, err
			}
			hdr += 4
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			c.shutdown()
			return 0, nil, err
		}
		c.bytesIn.Add(int64(hdr) + n)
		if masked {
			for i := range payload {
				payload[i] ^= mask[i&3]
			}
		}
		switch op {
		case opPing:
			_ = c.writeRaw(c.frameFor(opPong, payload))
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
