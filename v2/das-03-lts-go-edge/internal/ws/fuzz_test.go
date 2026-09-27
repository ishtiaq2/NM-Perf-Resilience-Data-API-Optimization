package ws

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"
	"unicode/utf8"
)

// fuzzConn feeds bytes to the reader and discards what the connection writes.
type fuzzConn struct{ r *bytes.Reader }

func (f *fuzzConn) Read(b []byte) (int, error)       { return f.r.Read(b) }
func (f *fuzzConn) Write(b []byte) (int, error)      { return len(b), nil }
func (f *fuzzConn) Close() error                     { return nil }
func (f *fuzzConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (f *fuzzConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (f *fuzzConn) SetDeadline(time.Time) error      { return nil }
func (f *fuzzConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fuzzConn) SetWriteDeadline(time.Time) error { return nil }

func clientFrame(first byte, payload []byte) []byte {
	mask := [4]byte{1, 2, 3, 4}
	b := frameHeader(nil, first&0x80 != 0, first&0x40 != 0, first&0x0F, len(payload))
	b[1] |= 0x80 // masked
	b = append(b, mask[:]...)
	for i, c := range payload {
		b = append(b, c^mask[i&3])
	}
	return b
}

// FuzzReadMessage: whatever a browser sends, the reader must not panic, must not
// return more than the size limit, and must only return valid UTF-8 as text.
//
//	go test -run '^$' -fuzz FuzzReadMessage -fuzztime 60s ./internal/ws
func FuzzReadMessage(f *testing.F) {
	f.Add(clientFrame(0x81, []byte(`{"maxPoints":2000}`)))
	f.Add(append(clientFrame(0x01, []byte("frag")), clientFrame(0x80, []byte("ment"))...))
	f.Add(append(clientFrame(0x89, []byte("ping")), clientFrame(0x81, []byte("after ping"))...))
	f.Add(clientFrame(0xC1, deflate([]byte(`{"nodeId":1,"port":1}`))))
	f.Add(clientFrame(0x88, []byte{0x03, 0xE8, 'b', 'y', 'e'}))
	f.Add([]byte("\x8b\xff\xff00000000000")) // control frame, 64-bit length with the top bit set
	f.Add([]byte("\x81\xff\xff00000000000")) // data frame, same
	const limit = 4096
	f.Fuzz(func(t *testing.T, data []byte) {
		fc := &fuzzConn{r: bytes.NewReader(data)}
		c := &Conn{nc: fc, br: bufio.NewReader(fc), maxMsg: limit, writeTimeout: time.Second, closed: make(chan struct{}), compress: true}
		for i := 0; i < 64; i++ {
			typ, msg, err := c.ReadMessage(0)
			if err != nil {
				return
			}
			if len(msg) > limit {
				t.Fatalf("message of %d bytes, limit %d", len(msg), limit)
			}
			if typ == TextMessage && !utf8.Valid(msg) {
				t.Fatal("invalid UTF-8 returned as text")
			}
		}
	})
}
