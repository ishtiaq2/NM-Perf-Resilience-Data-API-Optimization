package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dasnoc/internal/dash"
	"dasnoc/internal/fleet"
	"dasnoc/internal/proto"
	"dasnoc/internal/sim"
	"dasnoc/internal/uplink"
)

var secret = []byte("test-secret")

type noc struct {
	srv   *httptest.Server
	store *fleet.Store
	up    *uplink.Server
	hub   *dash.Hub
	ws    string
}

func startNOC(t *testing.T, ucfg uplink.Config) *noc { return startNOCAt(t, ucfg, "") }

// startNOCAt starts a NOC on addr ("" = any free port).
func startNOCAt(t *testing.T, ucfg uplink.Config, addr string) *noc {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	verify := &proto.Verifier{Secrets: [][]byte{secret}}
	store := fleet.New(fleet.Config{Grace: 2 * time.Second, ResyncMinInterval: 100 * time.Millisecond})
	up := uplink.New(ucfg, store, verify, log)
	hub := dash.New(dash.Config{OverviewInterval: 100 * time.Millisecond, SiteInterval: 50 * time.Millisecond, AlarmInterval: 10 * time.Millisecond}, store, verify, log)
	s := &Server{Store: store, Uplink: up, Hub: hub, Verify: verify, Log: log, Version: "test", Started: time.Now()}
	srv := httptest.NewUnstartedServer(s.Handler())
	if addr != "" {
		var l net.Listener
		var err error
		for i := 0; i < 50; i++ { // the old listener's port may take a moment to be free
			if l, err = net.Listen("tcp", addr); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		srv.Listener.Close()
		srv.Listener = l
	}
	srv.Start()
	t.Cleanup(func() { up.Shutdown(); hub.Shutdown(); srv.Close() })
	return &noc{srv: srv, store: store, up: up, hub: hub, ws: "ws" + strings.TrimPrefix(srv.URL, "http")}
}

func token(t *testing.T, kind, subject, tenant string) string {
	tok, err := proto.Mint(secret, proto.Claims{Kind: kind, Subject: subject, Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (n *noc) get(t *testing.T, path, tok string, out any) int {
	t.Helper()
	req, _ := http.NewRequest("GET", n.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mus(t *testing.T, n *noc, count int, tenantOf func(int) string, stats *sim.FleetStats, paused *atomic.Bool) []*sim.MasterUnit {
	var out []*sim.MasterUnit
	for i := 1; i <= count; i++ {
		id := fmt.Sprintf("S%04d", i)
		out = append(out, sim.NewMasterUnit(sim.MUConfig{URL: n.ws + "/uplink/v1", Token: token(t, proto.KindSite, id, tenantOf(i)), Site: id,
			Name: "Venue " + id, Venue: "Hall", Region: "north", Nodes: 40, Tick: 30 * time.Millisecond, AlarmsPerHour: 3600,
			BackoffBase: 50 * time.Millisecond, BackoffMax: 500 * time.Millisecond, Seed: uint64(i), Stats: stats, Paused: paused}))
	}
	return out
}

type listResp struct {
	Total int              `json:"total"`
	Items []fleet.SiteView `json:"items"`
}

// consistent: every device's summary hash and alarm counts match what the NOC shows.
func consistent(t *testing.T, n *noc, devices []*sim.MasterUnit, admin string) (bad int) {
	var list listResp
	n.get(t, "/api/sites?limit=1000&sort=id", admin, &list)
	byID := map[string]fleet.SiteView{}
	for _, v := range list.Items {
		byID[v.ID] = v
	}
	for i, d := range devices {
		v := byID[fmt.Sprintf("S%04d", i+1)]
		h, alarms := d.Expect()
		if v.Hash != h || [4]int(v.Alarms) != alarms || !v.Connected {
			bad++
			if testing.Verbose() {
				t.Logf("%s: hash %s/%s alarms %v/%v connected %v", v.ID, v.Hash, h, v.Alarms, alarms, v.Connected)
			}
		}
	}
	return bad
}

func TestEndToEndFleet(t *testing.T) {
	n := startNOC(t, uplink.Config{Keepalive: time.Second})
	admin := token(t, proto.KindUser, "noc-alice", proto.AllTenants)
	tenantUser := token(t, proto.KindUser, "bob", "stadium")
	stats := &sim.FleetStats{}
	var paused atomic.Bool
	devices := mus(t, n, 12, func(i int) string { return []string{"airport", "stadium"}[i%2] }, stats, &paused)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, d := range devices {
		go d.Run(ctx)
	}
	// Dashboards: NOC staff and one customer, the latter must see only its own tenant.
	dstats := &sim.DashStats{}
	lat := sim.NewLatency(100000)
	go (&sim.Dashboard{URL: n.ws + "/api/ws/dashboard", Token: admin, Tenant: "*", Site: "S0001", Stats: dstats, Latency: lat}).Run(ctx)
	go (&sim.Dashboard{URL: n.ws + "/api/ws/dashboard", Token: tenantUser, Tenant: "stadium", Stats: dstats}).Run(ctx)

	waitFor(t, "12 connected sites", 5*time.Second, func() bool { return n.store.Overview(proto.Claims{Kind: "user", Tenant: "*"}).Connected == 12 })
	time.Sleep(600 * time.Millisecond) // deltas and alarms flow

	// Storm: 3 sites lose mains power, 25 critical alarms each.
	for _, d := range devices[:3] {
		d.Inject(25)
	}
	waitFor(t, "storm alarms visible", 5*time.Second, func() bool {
		var r struct{ Total int }
		n.get(t, "/api/alarms?min=critical", admin, &r)
		return r.Total >= 75
	})
	// Quiet the model, then every device must match the NOC exactly.
	paused.Store(true)
	time.Sleep(300 * time.Millisecond)
	waitFor(t, "consistency", 5*time.Second, func() bool { return consistent(t, n, devices, admin) == 0 })

	// Tenant scoping on REST.
	var list listResp
	n.get(t, "/api/sites?limit=100", tenantUser, &list)
	if list.Total != 6 {
		t.Fatalf("stadium user sees %d sites", list.Total)
	}
	for _, v := range list.Items {
		if v.Tenant != "stadium" {
			t.Fatal("tenant isolation")
		}
	}
	if code := n.get(t, "/api/sites/S0002", tenantUser, nil); code != 404 { // S0002 is an airport site
		t.Fatalf("cross-tenant site read: %d", code)
	}
	if code := n.get(t, "/api/fleet/summary", "v1.user.x.y.bad", nil); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	// Drill-down: the admin dashboard watches S0001, so its node table streams.
	var sd fleet.SiteDetail
	waitFor(t, "node detail of S0001", 5*time.Second, func() bool {
		n.get(t, "/api/sites/S0001", admin, &sd)
		return sd.Detail != nil && sd.Detail.Count == 40 && sd.DetailOn
	})
	// Acknowledge over REST.
	req, _ := http.NewRequest("POST", n.srv.URL+"/api/alarms/acknowledge", strings.NewReader(`{"site":"S0001","id":"n1:PSU_FAIL","note":"generator started"}`))
	req.Header.Set("Authorization", "Bearer "+admin)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 204 {
		t.Fatalf("acknowledge: %v %v", resp.StatusCode, err)
	}

	waitFor(t, "dashboard alarm events", 3*time.Second, func() bool { return dstats.Events.Load() > 75 })
	if v := dstats.ScopeViolations.Load(); v != 0 {
		t.Fatalf("%d events leaked across tenants", v)
	}
	p := sim.Percentiles(lat.Take())
	if p["n"] < 75 || p["p99"] > 500 {
		t.Fatalf("alarm latency %v", p)
	}
	t.Logf("alarm latency device->dashboard: %v", p)

	// NOC restart: the process goes away (closing every uplink with "service restart")
	// and a fresh one with an empty memory takes the same address. Every device
	// reconnects and resends its summary and its active alarms.
	addr := n.srv.Listener.Addr().String()
	n.up.Shutdown()
	n.hub.Shutdown()
	n.srv.Close()
	n2 := startNOCAt(t, uplink.Config{Keepalive: time.Second}, addr)
	waitFor(t, "devices back on the new NOC", 10*time.Second, func() bool {
		return n2.store.Overview(proto.Claims{Kind: "user", Tenant: "*"}).Connected == 12
	})
	waitFor(t, "state rebuilt from the devices", 5*time.Second, func() bool { return consistent(t, n2, devices, admin) == 0 })
	var r struct{ Total int }
	n2.get(t, "/api/alarms?min=critical", admin, &r)
	if r.Total < 75 {
		t.Fatalf("after the restart the NOC shows %d critical alarms, the devices have >= 75", r.Total)
	}
}

func TestAdmissionControlSpreadsAReconnectStorm(t *testing.T) {
	n := startNOC(t, uplink.Config{AdmitRate: 20, AdmitBurst: 5, RetryAfterMax: 1, Keepalive: time.Second})
	stats := &sim.FleetStats{}
	devices := mus(t, n, 40, func(int) string { return "t1" }, stats, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	for _, d := range devices {
		go d.Run(ctx)
	}
	waitFor(t, "all 40 admitted", 10*time.Second, func() bool { return stats.Connected.Load() == 40 })
	took := time.Since(start)
	if stats.Refused.Load() == 0 {
		t.Fatal("admission control never refused anyone")
	}
	if took < 1500*time.Millisecond {
		t.Fatalf("40 devices at 20/s (burst 5) cannot all connect in %v", took)
	}
	m := n.up.Stats()
	t.Logf("40 devices admitted in %v; %d refusals with Retry-After; uplink %v", took.Round(time.Millisecond), stats.Refused.Load(), m["rejected"])
}

func TestAuthTakeoverAndResync(t *testing.T) {
	n := startNOC(t, uplink.Config{Keepalive: time.Second})
	stats := &sim.FleetStats{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A device with a token signed by another secret is refused.
	bad, _ := proto.Mint([]byte("wrong"), proto.Claims{Kind: proto.KindSite, Subject: "S0099", Tenant: "t"})
	go sim.NewMasterUnit(sim.MUConfig{URL: n.ws + "/uplink/v1", Token: bad, Site: "S0099", BackoffMax: time.Hour, Stats: stats}).Run(ctx)
	waitFor(t, "auth refusal", 3*time.Second, func() bool { return stats.AuthFailed.Load() == 1 })

	// Two processes with the same device identity: the newer connection wins.
	devices := mus(t, n, 1, func(int) string { return "t" }, stats, nil)
	twin := mus(t, n, 1, func(int) string { return "t" }, stats, nil)
	go devices[0].Run(ctx)
	waitFor(t, "first connection", 3*time.Second, func() bool { return stats.Connected.Load() == 1 })
	twinCtx, stopTwin := context.WithCancel(ctx)
	go twin[0].Run(twinCtx)
	waitFor(t, "takeover", 3*time.Second, func() bool { return n.store.C.Takeovers.Load() >= 1 })
	stopTwin()
	admin := token(t, proto.KindUser, "noc", proto.AllTenants)
	var sd fleet.SiteDetail
	waitFor(t, "the original device holds the site again", 5*time.Second, func() bool {
		n.get(t, "/api/sites/S0001", admin, &sd)
		return sd.Connected && sd.Boot == devices[0].Boot()
	})

	// A device reboot: new boot id, so the NOC gets a full summary and the full alarm list.
	before := n.store.C.Summaries.Load()
	devices[0].Reboot()
	waitFor(t, "reconnect after reboot", 5*time.Second, func() bool {
		n.get(t, "/api/sites/S0001", admin, &sd)
		return sd.Connected && sd.Boot == devices[0].Boot() && n.store.C.Summaries.Load() > before
	})
	if h, _ := devices[0].Expect(); sd.Hash != h {
		t.Fatalf("summary after reboot: NOC %s, device %s", sd.Hash, h)
	}
}

func TestRevocationTakesEffectWithoutARestart(t *testing.T) {
	n := startNOC(t, uplink.Config{Keepalive: time.Second})
	stats := &sim.FleetStats{}
	devices := mus(t, n, 3, func(int) string { return "t" }, stats, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, d := range devices {
		go d.Run(ctx)
	}
	waitFor(t, "three devices", 5*time.Second, func() bool { return n.up.Sessions() == 3 })
	operator := token(t, proto.KindUser, "leaver", proto.AllTenants)
	if code := n.get(t, "/api/me", operator, nil); code != http.StatusOK {
		t.Fatalf("before revocation: %d", code)
	}

	n.up.Verifier().SetRevoked(map[string]bool{"site:S0002": true, "user:leaver": true})
	if dropped := n.up.DropRevoked(); dropped != 1 {
		t.Fatalf("dropped %d device connections, want 1", dropped)
	}
	waitFor(t, "the revoked device refused when it reconnects", 5*time.Second, func() bool { return stats.AuthFailed.Load() >= 1 })
	if s := n.up.Sessions(); s != 2 {
		t.Fatalf("%d devices connected, want 2", s)
	}
	if code := n.get(t, "/api/me", operator, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked user token: %d, want 401", code)
	}
}
