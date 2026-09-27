package wsock

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func echoServer(t *testing.T, opt Options) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/busy" {
			w.Header().Set("Retry-After", "7")
			http.Error(w, "admission control", http.StatusServiceUnavailable)
			return
		}
		c, err := Upgrade(w, r, opt)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, msg, err := c.ReadMessage(5 * time.Second)
			if err != nil {
				return
			}
			if string(msg) == "proto?" {
				msg = []byte("proto=" + c.Subprotocol)
			}
			if err := c.WriteMessage(typ, msg); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(s *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + path
}

func TestClientServerRoundTrip(t *testing.T) {
	srv := echoServer(t, Options{Subprotocols: []string{"das-noc.v1"}, MaxMessageSize: 1 << 20})
	c, err := Dial(context.Background(), wsURL(srv, "/x"), DialOptions{Subprotocols: []string{"das-noc.v1", "bearer.secret-token"}, MaxMessageSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if c.Subprotocol != "das-noc.v1" {
		t.Fatalf("subprotocol %q", c.Subprotocol)
	}
	big := bytes.Repeat([]byte("0123456789"), 20000) // 200 kB: 64-bit length header, masked by the client
	for _, m := range [][]byte{[]byte("hello"), []byte("proto?"), big, make([]byte, 126), make([]byte, 65535)} {
		if err := c.WriteText(m); err != nil {
			t.Fatal(err)
		}
		_, got, err := c.ReadMessage(2 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		want := m
		if string(m) == "proto?" {
			want = []byte("proto=das-noc.v1")
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("echo of %d bytes differs (%d bytes back)", len(m), len(got))
		}
	}
	if err := c.Ping([]byte("keepalive")); err != nil {
		t.Fatal(err)
	}
	_ = c.Close(CloseNormal, "bye")
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("close handshake did not finish")
	}
}

func TestHandshakeRefusalCarriesRetryAfter(t *testing.T) {
	srv := echoServer(t, Options{})
	_, err := Dial(context.Background(), wsURL(srv, "/busy"), DialOptions{})
	var he *HandshakeError
	if !errors.As(err, &he) || he.Status != 503 || he.RetryAfter != 7*time.Second {
		t.Fatalf("got %v", err)
	}
}

func TestServerRejectsForeignOriginAndUnknownSubprotocolIsNotSelected(t *testing.T) {
	srv := echoServer(t, Options{Subprotocols: []string{"das-noc.v1"}})
	_, err := Dial(context.Background(), wsURL(srv, "/x"), DialOptions{Header: http.Header{"Origin": {"https://evil.example"}}})
	var he *HandshakeError
	if !errors.As(err, &he) || he.Status != 403 {
		t.Fatalf("foreign origin: %v", err)
	}
	c, err := Dial(context.Background(), wsURL(srv, "/x"), DialOptions{Subprotocols: []string{"other"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if c.Subprotocol != "" {
		t.Fatalf("selected %q", c.Subprotocol)
	}
}

func TestMessageSizeLimit(t *testing.T) {
	srv := echoServer(t, Options{MaxMessageSize: 1000})
	c, err := Dial(context.Background(), wsURL(srv, "/x"), DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_ = c.WriteText(make([]byte, 2000))
	_, _, err = c.ReadMessage(2 * time.Second)
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != CloseMessageTooBig {
		t.Fatalf("got %v", err)
	}
}

func TestIdleTimeout(t *testing.T) {
	srv := echoServer(t, Options{})
	c, err := Dial(context.Background(), wsURL(srv, "/x"), DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	start := time.Now()
	if _, _, err := c.ReadMessage(150 * time.Millisecond); err == nil {
		t.Fatal("expected a timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatal("idle timeout not honoured")
	}
}

func TestPreparedMessageCompressedForDeflateClients(t *testing.T) {
	pm := NewPreparedMessage(TextMessage, bytes.Repeat([]byte(`{"k":"v"}`), 200), true)
	plain, z := pm.frame(false), pm.frame(true)
	if len(z) >= len(plain)/4 || z[0]&0x40 == 0 {
		t.Fatalf("compressed frame %d bytes (plain %d), rsv1=%v", len(z), len(plain), z[0]&0x40 != 0)
	}
	if &pm.frame(true)[0] != &z[0] {
		t.Fatal("a prepared message must be framed once")
	}
}
