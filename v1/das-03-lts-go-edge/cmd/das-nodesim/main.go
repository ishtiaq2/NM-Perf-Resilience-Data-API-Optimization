// Command das-nodesim simulates a fleet of Remote Nodes reporting to the Master
// Node over das.v1.NodeIngestService (Connect streaming, JSON codec, h2c):
// report-by-exception with deadbands, deltas with state hashes, keep-alives,
// RESYNC handling and reconnects with a full report.
//
//	das-edge -nodes 0 -ingest 127.0.0.1:9090 &
//	das-nodesim -master http://127.0.0.1:9090 -nodes 300
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"dasedge/internal/ingest"
	"dasedge/internal/telemetry"
)

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

func main() {
	master := flag.String("master", "http://127.0.0.1:9090", "Master Node ingest URL (h2c)")
	nodes := flag.Int("nodes", 300, "number of Remote Nodes")
	interval := flag.Duration("interval", time.Second, "measurement interval per node")
	keepAlive := flag.Duration("keepalive", 5*time.Second, "keep-alive while nothing changes")
	seed := flag.Uint64("seed", 1, "simulation seed")
	every := flag.Duration("stats", 10*time.Second, "print statistics this often")
	duration := flag.Duration("duration", 0, "stop after this long (0 = until interrupted)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	client := ingest.NewH2CClient()
	stats := &ingest.AgentStats{}
	var connected, reconnects, notConnected atomic.Int64
	agents := make([]*ingest.Agent, *nodes)
	for i := range agents {
		a := ingest.NewAgent(uint32(i+1), stats)
		a.KeepAlive = *keepAlive
		agents[i] = a
		go func() { // one long-lived stream per node, reconnecting with backoff
			backoff := 500 * time.Millisecond
			for ctx.Err() == nil {
				s, err := ingest.Open(ctx, client, *master)
				if err != nil {
					time.Sleep(backoff/2 + rand.N(backoff))
					backoff = min(2*backoff, 10*time.Second)
					continue
				}
				backoff = 500 * time.Millisecond
				a.Attach(s)
				connected.Add(1)
				for {
					r, err := s.Recv()
					if err != nil {
						break
					}
					a.Handle(r)
				}
				connected.Add(-1)
				reconnects.Add(1)
				a.Attach(nil)
				_ = s.Close()
			}
		}()
	}

	sim := telemetry.NewSimulator(*nodes, *interval, *seed)
	var fullEquivalent atomic.Int64 // bytes if every measurement were sent as a full JSON state
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	report := time.NewTicker(*every)
	defer report.Stop()
	start := time.Now()
	lastOffered := int64(0)
	print := func() {
		off := stats.Offered.Load()
		out := map[string]any{
			"t":            int(time.Since(start).Seconds()),
			"connected":    connected.Load(),
			"offeredPerS":  float64(off-lastOffered) / every.Seconds(),
			"offered":      off,
			"suppressed":   stats.Suppressed.Load(),
			"full":         stats.Full.Load(),
			"deltas":       stats.Deltas.Load(),
			"keepAlives":   stats.KeepAlives.Load(),
			"resyncs":      stats.Resyncs.Load(),
			"slowDowns":    stats.SlowDowns.Load(),
			"reconnects":   reconnects.Load(),
			"notConnected": notConnected.Load(),
			"sentMB":       round2(float64(stats.BytesSent.Load()) / 1e6),
			"fullStateMB":  round2(float64(fullEquivalent.Load()) / 1e6),
		}
		if sent := stats.BytesSent.Load(); sent > 0 {
			out["savedFactor"] = round2(float64(fullEquivalent.Load()) / float64(sent))
		}
		lastOffered = off
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
	}
	for {
		select {
		case <-ctx.Done():
			print()
			os.Exit(0)
		case <-report.C:
			print()
		case now := <-tick.C:
			for _, r := range sim.Tick(now) {
				w := ingest.FromState(&r.State)
				b, _ := json.Marshal(w)
				fullEquivalent.Add(int64(len(b)) + 5)
				if err := agents[r.ID-1].Offer(r, now); err != nil {
					notConnected.Add(1) // stream not (yet) open: this measurement is dropped, the next one is sent
				}
			}
		}
	}
}
