package fleet

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"dasnoc/internal/proto"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func newClock() *clock               { return &clock{t: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)} }
func raw(v any) json.RawMessage      { b, _ := json.Marshal(v); return b }
func admin() proto.Claims {
	return proto.Claims{Kind: proto.KindUser, Subject: "noc", Tenant: proto.AllTenants}
}
func user(tenant string) proto.Claims {
	return proto.Claims{Kind: proto.KindUser, Subject: "u-" + tenant, Tenant: tenant}
}
func hello(boot string) *proto.Message {
	return &proto.Message{T: proto.THello, Boot: boot, FW: "3.1.4", Agent: "test"}
}
func summary(name string, temp float64) map[string]json.RawMessage {
	return map[string]json.RawMessage{"name": raw(name), "nodes": raw(300), "online": raw(298), "offline": raw(2), "maxTempC": raw(temp)}
}

func newStore(c *clock) *Store {
	return New(Config{Grace: 90 * time.Second, LogCapacity: 64, ResyncMinInterval: 10 * time.Second, DetailLinger: 30 * time.Second, Now: c.now})
}

func TestSummaryDeltaChainAndResync(t *testing.T) {
	c := newClock()
	st := newStore(c)
	have, _, _ := st.Connect("S1", "airport", 1, "10.0.0.1:5000", hello("b1"))
	if have != nil {
		t.Fatal("a new site has nothing yet")
	}
	s0 := summary("Terminal 1", 41.5)
	h0, _ := proto.SummaryHash(s0)
	st.ApplySummary("S1", 1, &proto.Message{Rev: 1, Hash: h0, S: s0})

	s1 := summary("Terminal 1", 43)
	h1, _ := proto.SummaryHash(s1)
	if st.ApplyDelta("S1", 1, &proto.Message{Base: 1, Rev: 2, Hash: h1, S: map[string]json.RawMessage{"maxTempC": raw(43)}}) {
		t.Fatal("a valid delta must not ask for a resync")
	}
	d, _ := st.Site(admin(), "S1")
	if d.Rev != 2 || d.Hash != h1 || float64(d.MaxTempC) != 43 || d.Name != "Terminal 1" {
		t.Fatalf("after delta: %+v", d)
	}
	// Wrong base: resync, but at most once per ResyncMinInterval.
	if !st.ApplyDelta("S1", 1, &proto.Message{Base: 7, Rev: 8, S: map[string]json.RawMessage{"x": raw(1)}}) {
		t.Fatal("gap must ask for a full summary")
	}
	if st.ApplyDelta("S1", 1, &proto.Message{Base: 7, Rev: 8}) {
		t.Fatal("resync requests are rate limited")
	}
	// Hash mismatch (the device and the NOC disagree): counted, resync later.
	c.add(11 * time.Second)
	if !st.ApplyDelta("S1", 1, &proto.Message{Base: 2, Rev: 3, Hash: "0000000000000000", S: map[string]json.RawMessage{"maxTempC": raw(44)}}) {
		t.Fatal("hash mismatch must ask for a full summary")
	}
	if st.C.HashMismatch.Load() != 1 {
		t.Fatal("mismatch not counted")
	}
	// A field set to null is removed.
	st.ApplyDelta("S1", 1, &proto.Message{Base: 3, Rev: 4, S: map[string]json.RawMessage{"maxTempC": json.RawMessage("null")}})
	d, _ = st.Site(admin(), "S1")
	if _, ok := d.Summary["maxTempC"]; ok || d.MaxTempC.Known() {
		t.Fatal("null must delete the field")
	}
	// Same boot on reconnect: the NOC offers what it has.
	st.Disconnect("S1", 1)
	have, _, _ = st.Connect("S1", "airport", 2, "10.0.0.1:5001", hello("b1"))
	if have == nil || have.Rev != 4 || have.Boot != "b1" {
		t.Fatalf("have after reconnect: %+v", have)
	}
	have, _, _ = st.Connect("S1", "airport", 3, "10.0.0.1:5002", hello("b2"))
	if have != nil {
		t.Fatal("a new boot must resend everything")
	}
}

func TestAlarmsSequencingDuplicatesGapsAndFullResync(t *testing.T) {
	c := newClock()
	st := newStore(c)
	var events []Event
	st.OnChange(func(k ChangeKind, _ string) {})
	st.Connect("S1", "stadium", 1, "", hello("b1"))
	ev := func(seq uint64, id, sev, state string) proto.AlarmEvent {
		return proto.AlarmEvent{Seq: seq, ID: id, Code: id, Sev: sev, State: state, At: 1000 + int64(seq)}
	}
	ack, resync := st.ApplyAlarms("S1", 1, &proto.Message{Ev: []proto.AlarmEvent{ev(1, "n1:FAN", "critical", "raised"), ev(2, "n2:TEMP", "minor", "raised")}})
	if ack != 2 || resync {
		t.Fatalf("ack %d resync %v", ack, resync)
	}
	// Retransmission after a reconnect: duplicates are skipped, new ones applied.
	ack, _ = st.ApplyAlarms("S1", 1, &proto.Message{Ev: []proto.AlarmEvent{ev(2, "n2:TEMP", "minor", "raised"), ev(3, "n2:TEMP", "minor", "cleared")}})
	if ack != 3 || st.C.DuplicateEvents.Load() != 1 {
		t.Fatalf("ack %d dupes %d", ack, st.C.DuplicateEvents.Load())
	}
	v, _ := st.ListSites(admin(), SiteFilter{})
	if v[0].Status != StatusCritical || v[0].Alarms[SevCritical] != 1 || v[0].Alarms[SevMinor] != 0 {
		t.Fatalf("site view %+v", v[0])
	}
	// A gap (the device lost events): ask for the full list.
	ack, resync = st.ApplyAlarms("S1", 1, &proto.Message{Ev: []proto.AlarmEvent{ev(9, "n3:VSWR", "major", "raised")}})
	if !resync || ack != 3 {
		t.Fatalf("gap: ack %d resync %v", ack, resync)
	}
	// Operator acknowledges, then the full list arrives: kept alarms keep their ack.
	if err := st.Acknowledge(admin(), "S1", "n1:FAN", "technician on the way"); err != nil {
		t.Fatal(err)
	}
	ack, _ = st.ApplyAlarms("S1", 1, &proto.Message{Full: true, UpTo: 12, Ev: []proto.AlarmEvent{ev(1, "n1:FAN", "critical", "raised"), ev(9, "n3:VSWR", "major", "raised")}})
	if ack != 12 {
		t.Fatalf("full list ack %d", ack)
	}
	d, _ := st.Site(admin(), "S1")
	if len(d.ActiveAlarms) != 2 || d.ActiveAlarms[0].ID != "n1:FAN" || d.ActiveAlarms[0].AckBy != "noc" {
		t.Fatalf("active %+v", d.ActiveAlarms)
	}
	events, _, _ = st.EventsSince(admin(), 0, 100)
	var states []string
	for _, e := range events {
		states = append(states, e.ID+"/"+e.State)
	}
	want := []string{"n1:FAN/raised", "n2:TEMP/raised", "n2:TEMP/cleared", "n1:FAN/acknowledged", "n3:VSWR/raised"}
	if len(states) != len(want) {
		t.Fatalf("events %v", states)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("events %v, want %v", states, want)
		}
	}
}

func TestStatusGraceUnreachableAndTenantScopes(t *testing.T) {
	c := newClock()
	st := newStore(c)
	st.LoadInventory([]InventorySite{{ID: "S9", Tenant: "stadium", Name: "Arena North"}})
	st.Connect("S1", "airport", 1, "", hello("b1"))
	st.Connect("S2", "stadium", 2, "", hello("b2"))
	st.ApplyAlarms("S2", 2, &proto.Message{Ev: []proto.AlarmEvent{{Seq: 1, ID: "n5:VSWR", Code: "VSWR_HIGH", Sev: "major", State: "raised", At: 1}}})

	o := st.Overview(admin())
	if o.Sites != 3 || o.Connected != 2 || o.ByStatus[StatusOK] != 1 || o.ByStatus[StatusMajor] != 1 || o.ByStatus[StatusOffline] != 1 {
		t.Fatalf("overview %+v", o)
	}
	if o.Problems[0].ID != "S9" && o.Problems[0].ID != "S2" {
		t.Fatalf("problems %+v", o.Problems)
	}
	so := st.Overview(user("stadium"))
	if so.Sites != 2 || so.ByTenant != nil || so.Alarms[SevMajor] != 1 {
		t.Fatalf("tenant overview %+v", so)
	}
	if list, _ := st.ListSites(user("airport"), SiteFilter{}); len(list) != 1 || list[0].ID != "S1" {
		t.Fatalf("tenant list %+v", list)
	}
	if _, ok := st.Site(user("airport"), "S2"); ok {
		t.Fatal("tenant isolation: airport must not see stadium sites")
	}
	if a, _ := st.ActiveAlarms(user("airport"), AlarmFilter{}); len(a) != 0 {
		t.Fatal("tenant isolation for alarms")
	}
	if ev, _, _ := st.EventsSince(user("airport"), 0, 10); len(ev) != 0 {
		t.Fatal("tenant isolation for the alarm stream")
	}
	if err := st.Acknowledge(user("airport"), "S2", "n5:VSWR", ""); err == nil {
		t.Fatal("tenant isolation for acknowledgements")
	}

	// Disconnect: stale for the grace period, then offline with a NOC alarm.
	st.Disconnect("S1", 1)
	if v, _ := st.ListSites(admin(), SiteFilter{Query: "S1"}); v[0].Status != StatusStale {
		t.Fatalf("status %v", v[0].Status)
	}
	c.add(91 * time.Second)
	st.Sweep()
	d, _ := st.Site(admin(), "S1")
	if d.Status != StatusOffline || len(d.ActiveAlarms) != 1 || d.ActiveAlarms[0].Code != "SITE_UNREACHABLE" {
		t.Fatalf("after grace: %v %+v", d.Status, d.ActiveAlarms)
	}
	st.Connect("S1", "airport", 3, "", hello("b1"))
	d, _ = st.Site(admin(), "S1")
	if d.Status != StatusOK || len(d.ActiveAlarms) != 0 {
		t.Fatalf("after reconnect: %v %+v", d.Status, d.ActiveAlarms)
	}
	if st.Overview(admin()).Alarms[SevCritical] != 0 {
		t.Fatal("aggregate must drop the cleared NOC alarm")
	}
}

func TestTakeoverIgnoresTheOldSession(t *testing.T) {
	st := newStore(newClock())
	st.Connect("S1", "t", 1, "", hello("b"))
	_, replaced, _ := st.Connect("S1", "t", 2, "", hello("b"))
	if replaced != 1 || st.C.Takeovers.Load() != 1 {
		t.Fatalf("replaced %d", replaced)
	}
	st.ApplySummary("S1", 1, &proto.Message{Rev: 5, S: summary("old", 1)}) // late message of the old session
	st.Disconnect("S1", 1)                                                 // the old socket closes
	d, _ := st.Site(admin(), "S1")
	if !d.Connected || d.Rev != 0 {
		t.Fatalf("the old session must not affect the new one: %+v", d)
	}
}

func TestDetailStreamFollowsViewers(t *testing.T) {
	c := newClock()
	st := newStore(c)
	var mu sync.Mutex
	var calls []bool
	st.OnDetail(func(site string, on bool) { mu.Lock(); calls = append(calls, on); mu.Unlock() })
	st.Connect("S1", "t", 1, "", hello("b"))
	release, _ := st.WatchDetail("S1")
	release2, _ := st.WatchDetail("S1")
	node := func(id int, temp float64) json.RawMessage { return raw(map[string]any{"id": id, "tempC": temp}) }
	st.ApplyDetail("S1", 1, &proto.Message{Full: true, Rev: 1, Nodes: []json.RawMessage{node(1, 40), node(2, 41)}})
	st.ApplyDetail("S1", 1, &proto.Message{Base: 1, Rev: 2, Nodes: []json.RawMessage{node(2, 45)}, Gone: []int{1}})
	if st.ApplyDetail("S1", 1, &proto.Message{Base: 7, Rev: 8}) != true {
		t.Fatal("detail gap must ask for a full detail")
	}
	d, _ := st.Site(admin(), "S1")
	if d.Detail == nil || d.Detail.Count != 1 || string(d.Detail.Nodes[0]) != `{"id":2,"tempC":45}` {
		t.Fatalf("detail %+v", d.Detail)
	}
	release()
	release()
	release2()
	st.Sweep()
	c.add(31 * time.Second)
	st.Sweep()
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("detail on/off calls %v", calls)
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	c := newClock()
	st := newStore(c)
	st.Connect("S1", "airport", 1, "", hello("b"))
	st.ApplySummary("S1", 1, &proto.Message{Rev: 1, S: summary("Terminal 1", 40)})
	st.ApplyAlarms("S1", 1, &proto.Message{Ev: []proto.AlarmEvent{{Seq: 1, ID: "n1:FAN", Code: "FAN_FAIL", Sev: "critical", State: "raised", At: 777}}})
	_ = st.Acknowledge(admin(), "S1", "n1:FAN", "known")
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := st.SaveRegistry(path); err != nil {
		t.Fatal(err)
	}
	entries, err := LoadRegistry(path)
	if err != nil || len(entries) != 1 {
		t.Fatalf("%v %v", entries, err)
	}
	// A new NOC process: the site is stale, then the device resends its alarms and the ack is restored.
	c2 := newClock()
	st2 := newStore(c2)
	st2.RestoreRegistry(entries)
	if v, _ := st2.ListSites(admin(), SiteFilter{}); v[0].Status != StatusStale || v[0].Name != "Terminal 1" {
		t.Fatalf("restored %+v", v[0])
	}
	st2.Connect("S1", "airport", 9, "", hello("b"))
	st2.ApplyAlarms("S1", 9, &proto.Message{Full: true, UpTo: 1, Ev: []proto.AlarmEvent{{Seq: 1, ID: "n1:FAN", Code: "FAN_FAIL", Sev: "critical", State: "raised", At: 777}}})
	d, _ := st2.Site(admin(), "S1")
	if d.ActiveAlarms[0].AckBy != "noc" || d.ActiveAlarms[0].AckNote != "known" {
		t.Fatalf("ack not restored: %+v", d.ActiveAlarms[0])
	}
}

func TestAlarmLogGap(t *testing.T) {
	l := newAlarmLog(16)
	for i := 0; i < 40; i++ {
		l.append(Event{ID: "x"})
	}
	ev, cur, gap := l.since(3, 100, nil)
	if !gap || len(ev) != 16 || ev[0].N != 25 || cur != 40 {
		t.Fatalf("gap %v len %d first %d cursor %d", gap, len(ev), ev[0].N, cur)
	}
	ev, cur, gap = l.since(38, 100, nil)
	if gap || len(ev) != 2 || cur != 40 {
		t.Fatalf("tail: %v %d %d", gap, len(ev), cur)
	}
}

func TestConcurrentSessionsKeepAggregatesConsistent(t *testing.T) {
	st := New(Config{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "S" + string(rune('A'+i%26)) + string(rune('a'+i/26))
			st.Connect(id, []string{"a", "b"}[i%2], uint64(i+1), "", hello("b"))
			for k := 0; k < 50; k++ {
				st.ApplyAlarms(id, uint64(i+1), &proto.Message{Ev: []proto.AlarmEvent{{Seq: uint64(2*k + 1), ID: "x", Code: "X", Sev: "major", State: "raised"}, {Seq: uint64(2*k + 2), ID: "x", Code: "X", Sev: "major", State: "cleared"}}})
				_ = st.Overview(admin())
			}
			if i%3 == 0 {
				st.Disconnect(id, uint64(i+1))
			}
		}(i)
	}
	wg.Wait()
	o := st.Overview(admin())
	sum := 0
	for _, n := range o.ByStatus {
		sum += n
	}
	if o.Sites != 50 || sum != 50 || o.Alarms[SevMajor] != 0 || o.Connected != 50-17 {
		t.Fatalf("aggregates %+v", o)
	}
}

func TestAcknowledgeNoteIsCutAtACharacterBoundary(t *testing.T) {
	c := newClock()
	st := newStore(c)
	st.Connect("S1", "airport", 1, "", hello("b1"))
	st.ApplyAlarms("S1", 1, &proto.Message{Ev: []proto.AlarmEvent{{Seq: 1, ID: "n1:FAN", Node: 1, Code: "FAN", Sev: "major", State: "raised", At: 1}}})
	note := ""
	for len(note) < 199 {
		note += "a"
	}
	note += "°C over the limit" // byte 200 is the second byte of "°"
	if err := st.Acknowledge(admin(), "S1", "n1:FAN", note); err != nil {
		t.Fatal(err)
	}
	items, _ := st.ActiveAlarms(admin(), AlarmFilter{})
	if len(items) != 1 || len(items[0].AckNote) != 199 || !utf8.ValidString(items[0].AckNote) {
		t.Fatalf("note %q (%d bytes)", items[0].AckNote, len(items[0].AckNote))
	}
}
