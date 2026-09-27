package wsock

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"
	"unicode/utf8"
)

// fuzzConn feeds bytes to the reader and discards what the connection writes
// (pongs, close frames).
type fuzzConn struct{ r *bytes.Reader }

func (f *fuzzConn) Read(b []byte) (int, error)       { return f.r.Read(b) }
func (f *fuzzConn) Write(b []byte) (int, error)      { return len(b), nil }
func (f *fuzzConn) Close() error                     { return nil }
func (f *fuzzConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (f *fuzzConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (f *fuzzConn) SetDeadline(time.Time) error      { return nil }
func (f *fuzzConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fuzzConn) SetWriteDeadline(time.Time) error { return nil }
func maskedFrame(first byte, payload []byte) []byte {
	mask := [4]byte{1, 2, 3, 4}
	b := appendHeader(nil, first&0x80 != 0, first&0x40 != 0, first&0x0F, len(payload), true)
	b = append(b, mask[:]...)
	for i, c := range payload {
		b = append(b, c^mask[i&3])
	}
	return b
}

// FuzzReadMessage: whatever a device or a browser sends, the server-side reader
// must not panic, must not return more than the size limit, and must only return
// valid UTF-8 as text. It is the code the whole internet can reach.
//
//	go test -run '^$' -fuzz FuzzReadMessage -fuzztime 60s ./internal/wsock
func FuzzReadMessage(f *testing.F) {
	f.Add(maskedFrame(0x81, []byte(`{"t":"hello","boot":"b1"}`)))
	f.Add(append(maskedFrame(0x01, []byte("frag")), maskedFrame(0x80, []byte("ment"))...))
	f.Add(append(maskedFrame(0x89, []byte("ping")), maskedFrame(0x81, []byte("after ping"))...))
	f.Add(maskedFrame(0xC1, deflate([]byte(`{"t":"sub","overview":true}`))))
	f.Add(maskedFrame(0x88, []byte{0x03, 0xE8, 'b', 'y', 'e'}))
	f.Add([]byte{0x81, 0xFF, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) // 2^63-1 bytes announced
	const limit = 4096
	f.Fuzz(func(t *testing.T, data []byte) {
		fc := &fuzzConn{r: bytes.NewReader(data)}
		c := newConn(fc, bufio.NewReader(fc), server, limit, time.Second)
		c.compress = true
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
