// Command fleetsim runs thousands of simulated Master Units and NOC dashboards
// against a NOC and measures it: time to connect the fleet, NOC memory and CPU,
// alarm latency from the device's event to the dashboard (steady state and during
// an alarm storm), reconnection after a NOC restart, and whether the NOC's view
// matches every device exactly.
//
//	fleetsim -url http://127.0.0.1:8080 -metrics http://127.0.0.1:9090/metrics?format=json \
//	         -sites 5000 -dashboards 20 -duration 5m -storm-at 2m -report report.json
//
// It mints device and user tokens itself, so it needs the NOC's secret
// (DAS_NOC_SECRET): a test tool, not something to run against production.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"dasnoc/internal/proto"
	"dasnoc/internal/sim"
)

type sample struct {
	T            float64 `json:"t"`
	Connected    int64   `json:"connected"`
	NocConnected float64 `json:"nocConnected"`
	RSSMB        float64 `json:"rssMB"`
	CPUSeconds   float64 `json:"cpuSeconds"`
	Goroutines   float64 `json:"goroutines"`
	UplinkMsgs   float64 `json:"uplinkMessages"`
	DashMsgs     float64 `json:"dashMessages"`
	Events       float64 `json:"alarmEvents"`
	GCPauseP99   float64 `json:"gcPauseP99Ms"`
	HeapMB       float64 `json:"heapMB"`
	LiveHeapMB   float64 `json:"liveHeapMB"`
	StacksMB     float64 `json:"stacksMB"`
}

type window struct {
	Name        string             `json:"name"`
	From        float64            `json:"from"`
	To          float64            `json:"to"`
	Latency     map[string]float64 `json:"alarmLatencyMs"`
	NocCPUPct   float64            `json:"nocCpuPercent"`
	NocRSSMaxMB float64            `json:"nocRssMaxMB"`
	LiveHeapMB  float64            `json:"nocLiveHeapMaxMB"`
	StacksMB    float64            `json:"nocStacksMaxMB"`
	UplinkMsgsS float64            `json:"uplinkMessagesPerS"`
	DashMsgsS   float64            `json:"dashboardMessagesPerS"`
	EventsS     float64            `json:"alarmEventsPerS"`
}

type check struct {
	At         float64  `json:"at"`
	Sites      int      `json:"sites"`
	Mismatches int      `json:"mismatches"`
	Examples   []string `json:"examples,omitempty"`
}

type report struct {
	Config         map[string]any `json:"config"`
	ConnectAllS    float64        `json:"connectAllS"`
	RefusedAtStart uint64         `json:"refusedAtStart"`
	Windows        []window       `json:"windows"`
	Restart        map[string]any `json:"restart,omitempty"`
	Checks         []check        `json:"consistencyChecks"`
	Devices        map[string]any `json:"devices"`
	Dashboards     map[string]any `json:"dashboards"`
	Sim            map[string]any `json:"simulator"`
	Timeline       []sample       `json:"timeline"`
}

func getJSON(url, token string, out any) error {
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func num(m map[string]any, path ...string) float64 {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = mm[p]
	}
	f, _ := cur.(float64)
	return f
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8080", "NOC base URL")
	metricsURL := flag.String("metrics", "http://127.0.0.1:9090/metrics?format=json", "NOC internal metrics (JSON); empty: do not sample")
	secretFlag := flag.String("secret", os.Getenv("DAS_NOC_SECRET"), "the NOC's token secret (to mint test tokens)")
	sites := flag.Int("sites", 5000, "simulated Master Units")
	tenants := flag.Int("tenants", 10, "customers the sites belong to")
	tick := flag.Duration("tick", 5*time.Second, "device model step")
	alarmsPerHour := flag.Float64("alarms-per-hour", 12, "spontaneous alarm raises per device per hour")
	dashboards := flag.Int("dashboards", 20, "NOC staff dashboards")
	latencyDashboards := flag.Int("latency-dashboards", 2, "of which this many decode every event and measure alarm latency (the others only count)")
	tenantDashboards := flag.Int("tenant-dashboards", 4, "customer dashboards (checked for tenant isolation)")
	drilldowns := flag.Int("drilldowns", 2, "dashboards watching one site's node table")
	duration := flag.Duration("duration", 5*time.Minute, "test length")
	stormAt := flag.Duration("storm-at", 2*time.Minute, "when a regional power failure hits (0: never)")
	stormSites := flag.Int("storm-sites", 1000, "sites losing mains power")
	stormAlarms := flag.Int("storm-alarms", 20, "critical alarms raised per storm site")
	stormClear := flag.Duration("storm-clear-after", 45*time.Second, "mains restored after")
	checks := flag.String("checks", "100s,280s", "consistency checks at these offsets (comma separated)")
	reportPath := flag.String("report", "", "write the JSON report here")
	flag.Parse()
	secret := []byte(*secretFlag)
	if len(secret) < 16 {
		fmt.Fprintln(os.Stderr, "fleetsim: -secret (or DAS_NOC_SECRET) of at least 16 bytes required")
		os.Exit(2)
	}
	wsBase := "ws" + strings.TrimPrefix(*base, "http")
	mint := func(kind, subject, tenant string) string {
		t, err := proto.Mint(secret, proto.Claims{Kind: kind, Subject: subject, Tenant: tenant})
		if err != nil {
			panic(err)
		}
		return t
	}
	admin := mint(proto.KindUser, "fleetsim-admin", proto.AllTenants)
	kinds := []string{"airport", "stadium", "hospital", "campus", "metro"}
	regions := []string{"north", "south", "east", "west", "central", "coast"}
	tenantName := func(i int) string { return fmt.Sprintf("%s-%02d", kinds[i%len(kinds)], i/len(kinds)+1) }

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	fstats := &sim.FleetStats{}
	var paused atomic.Bool
	devices := make([]*sim.MasterUnit, *sites)
	for i := range devices {
		id := fmt.Sprintf("S%05d", i+1)
		ten := tenantName(i % *tenants)
		devices[i] = sim.NewMasterUnit(sim.MUConfig{
			URL: wsBase + "/uplink/v1", Token: mint(proto.KindSite, id, ten), Site: id,
			Name:  fmt.Sprintf("%s %s %d", strings.ToUpper(ten[:1])+ten[1:strings.Index(ten, "-")], regions[i%len(regions)], i+1),
			Venue: ten, Region: regions[i%len(regions)], Nodes: 120 + (i*37)%361, Tick: *tick, AlarmsPerHour: *alarmsPerHour,
			Seed: uint64(i + 1), Stats: fstats, Paused: &paused,
		})
	}
	dstats := &sim.DashStats{}
	lat := sim.NewLatency(4_000_000)
	var wg sync.WaitGroup
	start := time.Now()
	since := func() float64 { return math.Round(time.Since(start).Seconds()*10) / 10 }
	for _, d := range devices {
		wg.Add(1)
		go func(d *sim.MasterUnit) { defer wg.Done(); d.Run(ctx) }(d)
	}
	for i := 0; i < *dashboards+*drilldowns; i++ {
		site := ""
		if i >= *dashboards {
			site = fmt.Sprintf("S%05d", i-*dashboards+1)
		}
		d := &sim.Dashboard{URL: wsBase + "/api/ws/dashboard", Token: admin, Tenant: "*", Site: site, Stats: dstats}
		if i < *latencyDashboards {
			d.Latency = lat
		}
		wg.Add(1)
		go func() { defer wg.Done(); d.Run(ctx) }()
	}
	for i := 0; i < *tenantDashboards; i++ {
		ten := tenantName(i % *tenants)
		d := &sim.Dashboard{URL: wsBase + "/api/ws/dashboard", Token: mint(proto.KindUser, "customer-"+strconv.Itoa(i), ten), Tenant: ten, Stats: dstats}
		wg.Add(1)
		go func() { defer wg.Done(); d.Run(ctx) }()
	}

	rep := &report{Config: map[string]any{"sites": *sites, "tenants": *tenants, "tick": tick.String(), "alarmsPerHour": *alarmsPerHour,
		"dashboards": *dashboards, "latencyDashboards": *latencyDashboards, "tenantDashboards": *tenantDashboards, "drilldowns": *drilldowns, "duration": duration.String(),
		"stormAt": stormAt.String(), "stormSites": *stormSites, "stormAlarms": *stormAlarms, "stormClearAfter": stormClear.String(),
		"cpus": runtime.NumCPU()}}

	var checkAt []float64
	for _, s := range strings.Split(*checks, ",") {
		if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil {
			checkAt = append(checkAt, d.Seconds())
		}
	}
	consistency := func() check {
		paused.Store(true)
		time.Sleep(3 * time.Second) // in-flight deltas and alarms land
		c := check{At: since(), Sites: len(devices)}
		byID := map[string]map[string]any{}
		for off := 0; off < len(devices); off += 1000 {
			var page struct{ Items []map[string]any }
			if err := getJSON(fmt.Sprintf("%s/api/sites?sort=id&limit=1000&offset=%d", *base, off), admin, &page); err != nil {
				c.Examples = append(c.Examples, err.Error())
				break
			}
			for _, it := range page.Items {
				id, _ := it["id"].(string)
				byID[id] = it
			}
		}
		for i, d := range devices {
			id := fmt.Sprintf("S%05d", i+1)
			v := byID[id]
			hash, alarms := d.Expect()
			got, _ := v["hash"].(string)
			a, _ := v["alarms"].(map[string]any)
			gotAlarms := [4]int{int(num(a, "critical")), int(num(a, "major")), int(num(a, "minor")), int(num(a, "warning"))}
			connected, _ := v["connected"].(bool)
			if got != hash || gotAlarms != alarms || !connected {
				c.Mismatches++
				if len(c.Examples) < 5 {
					c.Examples = append(c.Examples, fmt.Sprintf("%s: hash %s/%s alarms %v/%v connected %v", id, got, hash, gotAlarms, alarms, connected))
				}
			}
		}
		paused.Store(false)
		return c
	}

	// Sampling loop.
	var (
		allAt, restartDown, restartUp float64
		refusedAtStart                uint64
		samples                       []sample
		marks                         = map[string]int{} // window boundaries: name -> sample index
		stormDone, clearDone          bool
		windowLat                     = map[string][]float64{}
		refusedBeforeRestart          uint64
	)
	takeInto := func(name string) { windowLat[name] = append(windowLat[name], lat.Take()...) }
	phase := "connect"
	tickr := time.NewTicker(time.Second)
	defer tickr.Stop()
	end := time.After(*duration)
	nextCheck := 0
	fmt.Printf("fleetsim: %d sites, %d tenants, %d+%d+%d dashboards against %s for %v\n", *sites, *tenants, *dashboards, *drilldowns, *tenantDashboards, *base, *duration)
run:
	for {
		select {
		case <-ctx.Done():
			break run
		case <-end:
			break run
		case <-tickr.C:
		}
		t := since()
		s := sample{T: t, Connected: fstats.Connected.Load()}
		if *metricsURL != "" {
			var m map[string]any
			if getJSON(*metricsURL, "", &m) == nil {
				s.NocConnected, s.RSSMB, s.CPUSeconds = num(m, "fleet", "connected"), num(m, "process", "rssMB"), num(m, "process", "cpuSeconds")
				s.Goroutines, s.UplinkMsgs, s.DashMsgs = num(m, "process", "goroutines"), num(m, "uplink", "messages"), num(m, "dashboard", "messages")
				s.Events, s.GCPauseP99, s.HeapMB = num(m, "fleet", "alarmEvents"), num(m, "process", "gcPauseP99Ms"), num(m, "process", "heapMB")
				s.LiveHeapMB, s.StacksMB = num(m, "process", "liveHeapMB"), num(m, "process", "stacksMB")
			}
		}
		samples = append(samples, s)
		all := s.Connected == int64(len(devices))
		switch {
		case phase == "connect" && all:
			allAt, refusedAtStart = t, fstats.Refused.Load()
			phase = "settle"
			fmt.Printf("[%6.1fs] all %d sites connected (%d refusals by admission control)\n", t, len(devices), refusedAtStart)
		case phase == "settle" && t >= allAt+10:
			lat.Take() // discard the connect phase
			marks["steady"] = len(samples) - 1
			phase = "steady"
		case (phase == "steady" || phase == "storm" || phase == "after") && s.Connected < int64(len(devices))/2:
			restartDown, refusedBeforeRestart = t, fstats.Refused.Load()
			takeInto(phase)
			// The window ends with the last sample the old NOC process answered: CPU
			// seconds only grow within one process, so a drop marks the new one.
			end := len(samples) - 1
			for end > 0 {
				prev := end - 1
				for prev > 0 && samples[prev].CPUSeconds == 0 {
					prev--
				}
				if samples[end].CPUSeconds != 0 && samples[end].CPUSeconds >= samples[prev].CPUSeconds {
					break
				}
				end = prev
			}
			marks[phase+"-end"] = end
			phase = "restart"
			fmt.Printf("[%6.1fs] the NOC went away: %d sites connected\n", t, s.Connected)
		case phase == "restart" && all:
			restartUp = t
			lat.Take()
			phase = "recovered"
			marks["recovered"] = len(samples) - 1
			fmt.Printf("[%6.1fs] all sites back after %.1f s\n", t, restartUp-restartDown)
			checkAt = append(checkAt, t+10)
			slices.Sort(checkAt[nextCheck:]) // the post-restart check comes before later scheduled ones
		}
		if *stormAt > 0 && !stormDone && phase == "steady" && t >= stormAt.Seconds() {
			takeInto("steady")
			marks["steady-end"] = len(samples) - 1
			marks["storm"] = len(samples) - 1
			for _, d := range devices[:min(*stormSites, len(devices))] {
				d.Inject(*stormAlarms)
			}
			stormDone, phase = true, "storm"
			fmt.Printf("[%6.1fs] storm: %d sites x %d critical alarms\n", t, *stormSites, *stormAlarms)
		}
		if stormDone && !clearDone && t >= stormAt.Seconds()+stormClear.Seconds() {
			if phase == "storm" {
				takeInto("storm")
				marks["storm-end"] = len(samples) - 1
				marks["after"] = len(samples) - 1
				phase = "after"
			}
			for _, d := range devices[:min(*stormSites, len(devices))] {
				d.Inject(0)
			}
			clearDone = true
			fmt.Printf("[%6.1fs] mains restored at the storm sites\n", t)
		}
		if nextCheck < len(checkAt) && t >= checkAt[nextCheck] && (phase == "steady" || phase == "after" || phase == "recovered" || phase == "storm") {
			c := consistency()
			rep.Checks = append(rep.Checks, c)
			fmt.Printf("[%6.1fs] consistency: %d of %d sites differ from their device %v\n", c.At, c.Mismatches, c.Sites, c.Examples)
			nextCheck++
		}
		if int(t)%10 == 0 {
			fmt.Printf("[%6.1fs] %s: connected %d (NOC %0.f), NOC rss %.0f MB, cpu %.1f s, goroutines %.0f, dash msgs %.0f, gc p99 %.2f ms\n",
				t, phase, s.Connected, s.NocConnected, s.RSSMB, s.CPUSeconds, s.Goroutines, s.DashMsgs, s.GCPauseP99)
		}
	}
	takeInto(phase)
	marks[phase+"-end"] = len(samples) - 1
	cancel()
	wg.Wait()

	// Windows.
	win := func(name, from, to string) {
		i, ok1 := marks[from]
		j, ok2 := marks[to]
		if !ok1 || !ok2 || j <= i {
			return
		}
		a, b := samples[i], samples[j]
		dt := b.T - a.T
		w := window{Name: name, From: a.T, To: b.T, Latency: sim.Percentiles(windowLat[name])}
		if dt > 0 {
			w.NocCPUPct = math.Round((b.CPUSeconds-a.CPUSeconds)/dt*1000) / 10
			w.UplinkMsgsS = math.Round((b.UplinkMsgs - a.UplinkMsgs) / dt)
			w.DashMsgsS = math.Round((b.DashMsgs - a.DashMsgs) / dt)
			w.EventsS = math.Round((b.Events - a.Events) / dt)
		}
		for _, s := range samples[i : j+1] {
			w.NocRSSMaxMB = math.Max(w.NocRSSMaxMB, s.RSSMB)
			w.LiveHeapMB = math.Max(w.LiveHeapMB, s.LiveHeapMB)
			w.StacksMB = math.Max(w.StacksMB, s.StacksMB)
		}
		rep.Windows = append(rep.Windows, w)
	}
	win("steady", "steady", "steady-end")
	win("storm", "storm", "storm-end")
	win("after", "after", "after-end")
	win("recovered", "recovered", "recovered-end")
	rep.ConnectAllS, rep.RefusedAtStart = allAt, refusedAtStart
	if restartDown > 0 {
		rep.Restart = map[string]any{"downAt": restartDown, "allBackAt": restartUp, "reconnectS": math.Round((restartUp-restartDown)*10) / 10,
			"refusedDuringReconnect": fstats.Refused.Load() - refusedBeforeRestart}
	}
	rep.Devices = map[string]any{"dials": fstats.Dials.Load(), "connects": fstats.Connects.Load(), "refused": fstats.Refused.Load(), "failures": fstats.Failures.Load(),
		"messages": fstats.Messages.Load(), "bytes": fstats.Bytes.Load(), "resyncRequests": fstats.Resyncs.Load(), "acks": fstats.Acks.Load(),
		"alarmsRaised": fstats.Raised.Load(), "alarmsCleared": fstats.Cleared.Load()}
	rep.Dashboards = map[string]any{"connects": dstats.Connects.Load(), "messages": dstats.Messages.Load(), "bytes": dstats.Bytes.Load(),
		"overviews": dstats.Overviews.Load(), "alarmMessages": dstats.AlarmMsgs.Load(), "events": dstats.Events.Load(),
		"siteMessages": dstats.Sites.Load(), "gaps": dstats.Gaps.Load(), "tenantScopeViolations": dstats.ScopeViolations.Load(),
		"missingEvents": dstats.Missing.Load(), "outOfOrderEvents": dstats.OutOfOrder.Load()}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	rep.Sim = map[string]any{"goroutinesAtEnd": runtime.NumGoroutine(), "heapMB": math.Round(float64(ms.HeapAlloc)/(1<<20)*10) / 10}
	rep.Timeline = samples

	fmt.Printf("\n== fleetsim report: %d sites\n", len(devices))
	fmt.Printf("connect: all sites in %.1f s, %d admission refusals\n", rep.ConnectAllS, rep.RefusedAtStart)
	for _, w := range rep.Windows {
		fmt.Printf("%-9s %5.0f-%5.0fs  latency p50 %.1f / p99 %.1f / max %.1f ms (n=%.0f)  NOC cpu %.1f%%  rss max %.0f MB (live heap %.0f, stacks %.0f)  uplink %0.f msg/s  dashboards %0.f msg/s  events %0.f/s\n",
			w.Name, w.From, w.To, w.Latency["p50"], w.Latency["p99"], w.Latency["max"], w.Latency["n"], w.NocCPUPct, w.NocRSSMaxMB, w.LiveHeapMB, w.StacksMB, w.UplinkMsgsS, w.DashMsgsS, w.EventsS)
	}
	if rep.Restart != nil {
		fmt.Printf("restart: all sites back in %.1f s (%v refusals)\n", rep.Restart["reconnectS"], rep.Restart["refusedDuringReconnect"])
	}
	for _, c := range rep.Checks {
		fmt.Printf("consistency at %.0fs: %d mismatches of %d\n", c.At, c.Mismatches, c.Sites)
	}
	fmt.Printf("tenant isolation violations: %d, dashboard gaps: %d, missing events: %d, out of order: %d\n",
		dstats.ScopeViolations.Load(), dstats.Gaps.Load(), dstats.Missing.Load(), dstats.OutOfOrder.Load())
	if *reportPath != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*reportPath, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}
