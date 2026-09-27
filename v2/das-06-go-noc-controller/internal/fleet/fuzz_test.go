package fleet

import (
	"bytes"
	"encoding/json"
	"testing"

	"dasnoc/internal/proto"
)

// FuzzDeviceMessages feeds arbitrary message sequences from two devices (one of
// them reconnecting) into the store. Whatever a buggy or hostile Master Unit sends,
// the store must not panic, and the fleet aggregates it maintains incrementally
// must equal a recount from the sites.
//
//	go test -run '^$' -fuzz FuzzDeviceMessages -fuzztime 60s ./internal/fleet
func FuzzDeviceMessages(f *testing.F) {
	f.Add([]byte(`{"t":"summary","rev":1,"s":{"name":"A","nodes":3,"online":3,"maxTempC":41}}
{"t":"delta","base":1,"rev":2,"s":{"online":2,"offline":1,"name":null}}
{"t":"alarms","ev":[{"seq":1,"id":"n1:X","node":1,"code":"X","sev":"critical","state":"raised","at":5}]}
{"t":"alarms","ev":[{"seq":3,"id":"n2:Y","code":"Y","sev":"minor","state":"raised","at":6}]}
{"t":"alarms","full":true,"upTo":9,"ev":[{"seq":9,"id":"n3:Z","code":"Z","sev":"major","state":"raised","at":7}]}
{"t":"detail","rev":1,"full":true,"nodes":[{"id":1},{"id":2,"x":[1,2]}]}
{"t":"detail","base":1,"rev":2,"nodes":[{"id":"bad"}],"gone":[1,99]}
reconnect
{"t":"alarms","ev":[{"seq":1,"id":"n1:X","code":"X","sev":"critical","state":"cleared","at":8}]}
ack
sweep`))
	f.Add([]byte("{\"t\":\"delta\",\"base\":0,\"rev\":1,\"s\":{}}\n{\"t\":\"summary\",\"s\":null}\n{\"t\":\"alarms\",\"full\":true}"))
	f.Fuzz(func(t *testing.T, data []byte) {
		c := newClock()
		st := newStore(c)
		st.LoadInventory([]InventorySite{{ID: "S9", Tenant: "b"}})
		session := uint64(1)
		st.Connect("S1", "a", session, "", hello("b1"))
		st.Connect("S2", "b", 7, "", hello("b2"))
		release, _ := st.WatchDetail("S1")
		defer release()
		for i, line := range bytes.Split(data, []byte("\n")) {
			if i > 200 {
				break
			}
			site, sess := "S1", session
			if i%3 == 2 {
				site, sess = "S2", 7
			}
			switch string(line) {
			case "reconnect":
				session++
				st.Connect("S1", "a", session, "", hello("b1"))
				continue
			case "ack":
				_ = st.Acknowledge(admin(), "S1", "n1:X", "seen")
				continue
			case "sweep":
				st.Disconnect("S2", 7)
				c.add(200e9)
				st.Sweep()
				continue
			}
			var m proto.Message
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			switch m.T {
			case proto.TSummary:
				st.ApplySummary(site, sess, &m)
			case proto.TDelta:
				st.ApplyDelta(site, sess, &m)
			case proto.TAlarms:
				st.ApplyAlarms(site, sess, &m)
			case proto.TDetail:
				st.ApplyDetail(site, sess, &m)
			}
		}
		o := st.Overview(admin())
		items, total := st.ListSites(admin(), SiteFilter{Limit: 1000})
		if total != o.Sites || len(items) != o.Sites {
			t.Fatalf("overview has %d sites, the list %d", o.Sites, total)
		}
		var alarms SevCounts
		var byStatus StatusCounts
		connected := 0
		for _, it := range items {
			for k := range alarms {
				alarms[k] += it.Alarms[k]
			}
			byStatus[it.Status]++
			if it.Connected {
				connected++
			}
		}
		if alarms != o.Alarms || byStatus != o.ByStatus || connected != o.Connected {
			t.Fatalf("aggregates drifted: overview %v %v %d, recount %v %v %d", o.Alarms, o.ByStatus, o.Connected, alarms, byStatus, connected)
		}
		if _, err := json.Marshal(o); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"S1", "S2", "S9"} {
			if d, ok := st.Site(admin(), id); ok {
				if _, err := json.Marshal(d); err != nil {
					t.Fatalf("site %s does not encode: %v", id, err)
				}
			}
		}
	})
}
