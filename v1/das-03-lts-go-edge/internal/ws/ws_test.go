package ws

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- a minimal client for tests (masks frames as RFC 6455 requires) --------

type testClient struct {
	nc  net.Conn
	br  *bufio.Reader
	ext string
}

func dial(t *testing.T, url string, hdr map[string]string) (*testClient, *http.Response) {
	t.Helper()
	u := strings.TrimPrefix(url, "http://")
	host, path, _ := strings.Cut(u, "/")
	nc, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	req := "GET /" + path + " HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	for k, v := range hdr {
		req += k + ": " + v + "\r\n"
	}
	if _, err := nc.Write([]byte(req + "\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &testClient{nc: nc, br: br, ext: resp.Header.Get("Sec-WebSocket-Extensions")}, resp
}

func (c *testClient) send(op byte, fin bool, rsv1 bool, payload []byte) {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	if rsv1 {
		b0 |= 0x40
	}
	var h []byte
	h = append(h, b0)
	n := len(payload)
	switch {
	case n < 126:
		h = append(h, 0x80|byte(n))
	case n <= 0xFFFF:
		h = append(h, 0x80|126, byte(n>>8), byte(n))
	default:
		h = append(h, 0x80|127)
		h = binary.BigEndian.AppendUint64(h, uint64(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	h = append(h, mask[:]...)
	m := make([]byte, n)
	for i := range payload {
		m[i] = payload[i] ^ mask[i&3]
	}
	_, _ = c.nc.Write(append(h, m...))
}

func (c *testClient) read(t *testing.T) (op byte, rsv1 bool, payload []byte) {
	t.Helper()
	_ = c.nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		t.Fatal(err)
	}
	if h[1]&0x80 != 0 {
		t.Fatal("server frames must not be masked")
	}
	n := int(h[1] & 0x7F)
	if n == 126 {
		var b [2]byte
		_, _ = io.ReadFull(c.br, b[:])
		n = int(binary.BigEndian.Uint16(b[:]))
	} else if n == 127 {
		var b [8]byte
		_, _ = io.ReadFull(c.br, b[:])
		n = int(binary.BigEndian.Uint64(b[:]))
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		t.Fatal(err)
	}
	return h[0] & 0x0F, h[0]&0x40 != 0, payload
}

func inflate(t *testing.T, b []byte) []byte {
	t.Helper()
	r := flate.NewReader(io.MultiReader(bytes.NewReader(b), bytes.NewReader([]byte{0, 0, 0xff, 0xff})))
	out, err := io.ReadAll(r)
	if err != nil && err != io.ErrUnexpectedEOF {
		t.Fatal(err)
	}
	return out
}

// echo server: echoes every data message; records the last error
func echoServer(t *testing.T, opt Options) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, opt)
		if err != nil {
			return
		}
		for {
			typ, msg, err := c.ReadMessage(0)
			if err != nil {
				return
			}
			_ = c.WriteMessage(typ, msg)
		}
	}))
}

func TestHandshakeAcceptKey(t *testing.T) {
	srv := echoServer(t, Options{})
	defer srv.Close()
	c, resp := dial(t, srv.URL+"/ws", nil)
	defer c.nc.Close()
	if resp.StatusCode != 101 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// Example from RFC 6455 section 1.3
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept key %q", got)
	}
}

func TestOriginCheck(t *testing.T) {
	srv := echoServer(t, Options{})
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	_, bad := dial(t, srv.URL+"/ws", map[string]string{"Origin": "https://evil.example"})
	if bad.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: status %d", bad.StatusCode)
	}
	_, ok := dial(t, srv.URL+"/ws", map[string]string{"Origin": "http://" + host})
	if ok.StatusCode != 101 {
		t.Fatalf("same origin: status %d", ok.StatusCode)
	}
}

func TestEchoTextBinaryFragmentedAndPing(t *testing.T) {
	srv := echoServer(t, Options{MaxMessageSize: 1 << 20})
	defer srv.Close()
	c, _ := dial(t, srv.URL+"/ws", nil)
	defer c.nc.Close()

	c.send(opText, true, false, []byte("hello"))
	if op, _, p := c.read(t); op != opText || string(p) != "hello" {
		t.Fatalf("got %d %q", op, p)
	}
	big := bytes.Repeat([]byte{7}, 70000) // 64-bit length path
	c.send(opBinary, true, false, big)
	if op, _, p := c.read(t); op != opBinary || !bytes.Equal(p, big) {
		t.Fatalf("binary echo failed (%d bytes)", len(p))
	}
	// Fragmented message with a ping in between (control frames may be interleaved).
	c.send(opText, false, false, []byte("frag"))
	c.send(opPing, true, false, []byte("p"))
	if op, _, p := c.read(t); op != opPong || string(p) != "p" {
		t.Fatalf("expected pong, got %d %q", op, p)
	}
	c.send(opContinuation, true, false, []byte("mented"))
	if _, _, p := c.read(t); string(p) != "fragmented" {
		t.Fatalf("got %q", p)
	}
}

func TestPermessageDeflate(t *testing.T) {
	srv := echoServer(t, Options{EnableCompression: true})
	defer srv.Close()
	c, _ := dial(t, srv.URL+"/ws", map[string]string{"Sec-WebSocket-Extensions": "permessage-deflate; client_max_window_bits"})
	defer c.nc.Close()
	if !strings.Contains(c.ext, "permessage-deflate") || !strings.Contains(c.ext, "no_context_takeover") {
		t.Fatalf("extension not negotiated: %q", c.ext)
	}
	msg := []byte(strings.Repeat(`{"id":1,"temperatureC":41.2,"status":"online"},`, 200))
	// client -> server compressed (RSV1)
	c.send(opText, true, true, deflate(msg))
	op, rsv1, p := c.read(t)
	if op != opText || !rsv1 {
		t.Fatalf("expected a compressed text frame, op=%d rsv1=%v", op, rsv1)
	}
	if got := inflate(t, p); !bytes.Equal(got, msg) {
		t.Fatalf("round trip mismatch")
	}
	if len(p) > len(msg)/5 {
		t.Fatalf("poor compression: %d -> %d", len(msg), len(p))
	}
}

func TestPreparedMessageIsFramedOnce(t *testing.T) {
	pm := NewPreparedMessage(TextMessage, []byte(strings.Repeat("x", 4000)), true)
	a := pm.frame(true)
	b := pm.frame(true)
	if &a[0] != &b[0] {
		t.Fatal("compressed frame rebuilt for every client")
	}
	if len(pm.frame(false)) != 4000+4 {
		t.Fatalf("plain frame length %d", len(pm.frame(false)))
	}
}

func TestProtocolErrors(t *testing.T) {
	srv := echoServer(t, Options{MaxMessageSize: 1024})
	defer srv.Close()

	// unmasked frame -> close 1002
	c, _ := dial(t, srv.URL+"/ws", nil)
	_, _ = c.nc.Write([]byte{0x81, 0x02, 'h', 'i'})
	if op, _, p := c.read(t); op != opClose || binary.BigEndian.Uint16(p) != CloseProtocolError {
		t.Fatalf("expected close 1002, got op %d", op)
	}
	c.nc.Close()

	// too big -> close 1009
	c2, _ := dial(t, srv.URL+"/ws", nil)
	c2.send(opBinary, true, false, make([]byte, 2000))
	if op, _, p := c2.read(t); op != opClose || binary.BigEndian.Uint16(p) != CloseMessageTooBig {
		t.Fatalf("expected close 1009, got op %d", op)
	}
	c2.nc.Close()

	// invalid UTF-8 text -> close 1007
	c3, _ := dial(t, srv.URL+"/ws", nil)
	c3.send(opText, true, false, []byte{0xff, 0xfe})
	if op, _, p := c3.read(t); op != opClose || binary.BigEndian.Uint16(p) != CloseInvalidPayload {
		t.Fatalf("expected close 1007, got op %d", op)
	}
	c3.nc.Close()
}

func TestCloseHandshake(t *testing.T) {
	srv := echoServer(t, Options{})
	defer srv.Close()
	c, _ := dial(t, srv.URL+"/ws", nil)
	defer c.nc.Close()
	c.send(opClose, true, false, binary.BigEndian.AppendUint16(nil, CloseNormal))
	if op, _, p := c.read(t); op != opClose || binary.BigEndian.Uint16(p) != CloseNormal {
		t.Fatalf("expected close reply")
	}
}
