package dash

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dasnoc/internal/fleet"
	"dasnoc/internal/proto"
	"dasnoc/internal/wsock"
)

var secret = []byte("dash-test-secret")

func setup(t *testing.T) (*fleet.Store, *Hub, string) {
	t.Helper()
	store := fleet.New(fleet.Config{LogCapacity: 100_000})
	verify := &proto.Verifier{Secrets: [][]byte{secret}}
	hub := New(Config{}, store, verify, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	return store, hub, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, url, tenant string) *wsock.Conn {
	t.Helper()
	tok, _ := proto.Mint(secret, proto.Claims{Kind: proto.KindUser, Subject: "u", Tenant: tenant})
	c, err := wsock.Dial(context.Background(), url, wsock.DialOptions{Subprotocols: []string{Subprotocol, "bearer." + tok}, MaxMessageSize: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseNow)
	return c
}

// raise pushes n alarm events for one site, in device batches of 100.
func raise(store *fleet.Store, site, tenant string, n int) {
	store.Connect(site, tenant, 1, "", &proto.Message{Boot: "b"})
	seq := uint64(0)
	for i := 0; i < n; i += 100 {
		var ev []proto.AlarmEvent
		for k := i; k < min(n, i+100); k++ {
			seq++
			ev = append(ev, proto.AlarmEvent{Seq: seq, ID: fmt.Sprintf("n%d:PSU_FAIL", k), Code: "PSU_FAIL", Sev: "critical", State: "raised", At: time.Now().UnixMilli()})
		}
		store.ApplyAlarms(site, 1, &proto.Message{Ev: ev})
	}
}

func TestAlarmStormIsBatchedNotQueued(t *testing.T) {
	store, hub, url := setup(t)
	c := dial(t, url, proto.AllTenants)
	_ = c.WriteText([]byte(`{"t":"sub","alarms":true,"overview":true}`))
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	raise(store, "S1", "t1", 20_000) // a regional power failure
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the store must not wait for dashboards: %v", d)
	}
	events, alarmMsgs, overviews := 0, 0, 0
	deadline := time.Now().Add(5 * time.Second)
	for events < 20_000 && time.Now().Before(deadline) {
		_, raw, err := c.ReadMessage(2 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			T      string            `json:"t"`
			Events []json.RawMessage `json:"events"`
		}
		_ = json.Unmarshal(raw, &m)
		switch m.T {
		case "alarms":
			alarmMsgs++
			events += len(m.Events)
		case "overview":
			overviews++
		case "gap":
			t.Fatal("no gap expected: the log holds the storm")
		}
	}
	if events != 20_000 {
		t.Fatalf("received %d of 20000 events", events)
	}
	// 20 000 events in batches of at most 500: 40 messages, not 20 000.
	if alarmMsgs > 60 {
		t.Fatalf("%d alarm messages for one storm", alarmMsgs)
	}
	t.Logf("20000 events -> %d alarm messages, %d overview messages, %v; hub %v", alarmMsgs, overviews, time.Since(start).Round(time.Millisecond), hub.Stats())
}

func TestSlowDashboardDoesNotSlowTheFleet(t *testing.T) {
	store, _, url := setup(t)
	stuck := dial(t, url, proto.AllTenants) // subscribes, then never reads again
	_ = stuck.WriteText([]byte(`{"t":"sub","alarms":true,"overview":true}`))
	ok := dial(t, url, proto.AllTenants)
	_ = ok.WriteText([]byte(`{"t":"sub","alarms":true}`))
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	for i := 0; i < 20; i++ {
		raise(store, fmt.Sprintf("S%d", i), "t1", 2000)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("40 000 events took %v with a stuck dashboard attached", d)
	}
	got := 0
	for got < 40_000 {
		_, raw, err := ok.ReadMessage(3 * time.Second)
		if err != nil {
			t.Fatalf("healthy dashboard: %v after %d events", err, got)
		}
		var m struct {
			T      string            `json:"t"`
			Events []json.RawMessage `json:"events"`
		}
		_ = json.Unmarshal(raw, &m)
		got += len(m.Events)
	}
}

func TestTenantScopedStream(t *testing.T) {
	store, _, url := setup(t)
	c := dial(t, url, "t2")
	_ = c.WriteText([]byte(`{"t":"sub","alarms":true}`))
	time.Sleep(50 * time.Millisecond)
	raise(store, "A", "t1", 50)
	raise(store, "B", "t2", 5)
	for n := 0; n < 5; {
		_, raw, err := c.ReadMessage(2 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			T      string        `json:"t"`
			Events []fleet.Event `json:"events"`
		}
		_ = json.Unmarshal(raw, &m)
		for _, e := range m.Events {
			if e.Tenant != "t2" {
				t.Fatalf("tenant t2 received an event of %s", e.Tenant)
			}
			n++
		}
	}
	// A foreign site cannot be subscribed to (and its existence is not revealed differently).
	_ = c.WriteText([]byte(`{"t":"sub","site":"A"}`))
	for {
		_, raw, err := c.ReadMessage(2 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"t":"error"`) {
			if !strings.Contains(string(raw), "not found") {
				t.Fatalf("unexpected error %s", raw)
			}
			return
		}
		if strings.Contains(string(raw), `"t":"site"`) {
			t.Fatal("tenant t2 received site A")
		}
	}
}
