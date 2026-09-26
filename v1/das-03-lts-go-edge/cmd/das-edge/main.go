// Command das-edge is the Master Node's single-binary edge server (LTS target):
// the web UI, the REST API, WebSocket push, Remote Node ingest and the
// strangler-fig proxy to the legacy Node.js backend, on one origin.
//
//	das-edge                                  # demo: 300 simulated nodes, simulated analyzer
//	das-edge -web /opt/das/ui -legacy unix:/run/das/core-api.sock -nodes 0 -ingest :9090
//
// Every flag has an environment variable (DAS_*) for systemd units.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dasedge/internal/api"
	"dasedge/internal/configstore"
	"dasedge/internal/heavy"
	"dasedge/internal/hub"
	"dasedge/internal/ingest"
	"dasedge/internal/spectrum"
	"dasedge/internal/static"
	"dasedge/internal/sysd"
	"dasedge/internal/telemetry"
)

// version is set at build time: -ldflags "-X main.version=3.0.0".
var version = "3.0.0-dev"

const serverName = "das-edge-go"

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return n
	}
	return def
}

func envDur(name string, def time.Duration) time.Duration {
	if ms, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return time.Duration(ms) * time.Millisecond
	}
	return def
}

type config struct {
	listen, tlsCert, tlsKey, web, legacy, configMode, ingestAddr, pprofAddr, logFormat, logLevel string
	authCheck, authExempt, delegate                                                              string
	authTTL                                                                                      time.Duration
	nodes, defaultPoints, maxSessions, maxWS, prewarm, memLimitMB, heavyWorkers, heavyNice       int
	reportEvery, publishEvery, staleAfter, sweepTime, sweepWait, spectrumIdle, minInterval       time.Duration
	wsHeartbeat, ingestMinInterval                                                               time.Duration
	showVersion                                                                                  bool
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.listen, "listen", env("DAS_LISTEN", ":8080"), "HTTP listen address, or unix:/path.sock (DAS_LISTEN)")
	flag.StringVar(&c.tlsCert, "tls-cert", env("DAS_TLS_CERT", ""), "TLS certificate: serve HTTPS with HTTP/2 (DAS_TLS_CERT)")
	flag.StringVar(&c.tlsKey, "tls-key", env("DAS_TLS_KEY", ""), "TLS private key (DAS_TLS_KEY)")
	flag.StringVar(&c.web, "web", env("DAS_WEB_ROOT", ""), "directory with the web UI (Angular dist/browser); default: built-in status page (DAS_WEB_ROOT)")
	flag.StringVar(&c.legacy, "legacy", env("DAS_LEGACY", ""), "legacy backend for everything not implemented here: http://host:port or unix:/path.sock (DAS_LEGACY)")
	flag.StringVar(&c.delegate, "delegate", env("DAS_DELEGATE", ""), "endpoint groups left to the legacy backend: volatile,spectrum,ws (strangler steps) (DAS_DELEGATE)")
	flag.StringVar(&c.configMode, "config", env("DAS_CONFIG", ""), `who serves /api/nodes/{id}/config: "local" or "legacy" (default: legacy if -legacy is set) (DAS_CONFIG)`)
	flag.StringVar(&c.authCheck, "auth-check", env("DAS_AUTH_CHECK_PATH", ""), "legacy path that answers 2xx for a logged-in session, e.g. /api/session; empty = no authentication (DAS_AUTH_CHECK_PATH)")
	flag.StringVar(&c.authExempt, "auth-exempt", env("DAS_AUTH_EXEMPT", "heartbeat,capabilities"), "routes that stay open without a session (DAS_AUTH_EXEMPT)")
	flag.DurationVar(&c.authTTL, "auth-ttl", envDur("DAS_AUTH_TTL_MS", 30*time.Second), "how long a session check is cached (DAS_AUTH_TTL_MS)")
	flag.StringVar(&c.ingestAddr, "ingest", env("DAS_INGEST_LISTEN", ""), "listen address for Remote Node streams (Connect over h2c), e.g. :9090; empty = off (DAS_INGEST_LISTEN)")
	flag.StringVar(&c.pprofAddr, "pprof", env("DAS_PPROF", ""), "pprof listen address, e.g. 127.0.0.1:6060; empty = off (DAS_PPROF)")
	flag.StringVar(&c.logFormat, "log", env("DAS_LOG_FORMAT", "json"), "log format: json or text (DAS_LOG_FORMAT)")
	flag.StringVar(&c.logLevel, "log-level", env("DAS_LOG", "info"), "debug, info, warn or error (DAS_LOG)")
	flag.IntVar(&c.nodes, "nodes", envInt("DAS_NODES", 300), "simulated Remote Nodes (0 = none: real nodes report via -ingest) (DAS_NODES)")
	flag.IntVar(&c.defaultPoints, "points", envInt("DAS_SPECTRUM_POINTS", 50001), "sweep points when a client does not say (DAS_SPECTRUM_POINTS)")
	flag.IntVar(&c.maxSessions, "max-sessions", envInt("DAS_SPECTRUM_MAX_SESSIONS", 8), "concurrent analyzer configurations (DAS_SPECTRUM_MAX_SESSIONS)")
	flag.IntVar(&c.maxWS, "max-ws", envInt("DAS_MAX_WS_CLIENTS", 64), "WebSocket clients per channel (DAS_MAX_WS_CLIENTS)")
	flag.IntVar(&c.prewarm, "prewarm", envInt("DAS_SIM_PREWARM", 4), "simulator: analyzers generated at start-up (DAS_SIM_PREWARM)")
	flag.IntVar(&c.memLimitMB, "mem-limit-mb", envInt("DAS_MEM_LIMIT_MB", 0), "Go soft memory limit in MiB (0 = GOMEMLIMIT or none) (DAS_MEM_LIMIT_MB)")
	flag.IntVar(&c.heavyWorkers, "heavy-workers", envInt("DAS_HEAVY_WORKERS", 0), "threads for encoding/compression (0 = GOMAXPROCS-1, at least 1) (DAS_HEAVY_WORKERS)")
	flag.IntVar(&c.heavyNice, "heavy-nice", envInt("DAS_HEAVY_NICE", 10), "nice value of those threads (Linux) (DAS_HEAVY_NICE)")
	flag.DurationVar(&c.reportEvery, "report-interval", envDur("DAS_REPORT_INTERVAL_MS", time.Second), "simulated node report interval (DAS_REPORT_INTERVAL_MS)")
	flag.DurationVar(&c.publishEvery, "publish-interval", envDur("DAS_PUBLISH_INTERVAL_MS", time.Second), "telemetry revision interval, i.e. batching (DAS_PUBLISH_INTERVAL_MS)")
	flag.DurationVar(&c.staleAfter, "stale-after", envDur("DAS_STALE_AFTER_MS", 15*time.Second), "a node without reports for this long is offline (DAS_STALE_AFTER_MS)")
	flag.DurationVar(&c.sweepTime, "sweep-time", envDur("DAS_SWEEP_TIME_MS", 250*time.Millisecond), "simulated analyzer sweep duration (DAS_SWEEP_TIME_MS)")
	flag.DurationVar(&c.sweepWait, "sweep-wait", envDur("DAS_SWEEP_WAIT_MS", 10*time.Second), "longest wait for a sweep before 503 (DAS_SWEEP_WAIT_MS)")
	flag.DurationVar(&c.spectrumIdle, "spectrum-idle", envDur("DAS_SPECTRUM_IDLE_MS", 15*time.Second), "stop sweeping when nobody asked for this long (DAS_SPECTRUM_IDLE_MS)")
	flag.DurationVar(&c.minInterval, "spectrum-min-interval", envDur("DAS_SPECTRUM_MIN_INTERVAL_MS", 0), "minimum time between sweeps of one session (DAS_SPECTRUM_MIN_INTERVAL_MS)")
	flag.DurationVar(&c.wsHeartbeat, "ws-heartbeat", envDur("DAS_WS_HEARTBEAT_MS", 5*time.Second), `WebSocket "hb" after this much silence (DAS_WS_HEARTBEAT_MS)`)
	flag.DurationVar(&c.ingestMinInterval, "ingest-min-interval", envDur("DAS_INGEST_MIN_INTERVAL_MS", 200*time.Millisecond), "nodes reporting faster are told to slow down (DAS_INGEST_MIN_INTERVAL_MS)")
	flag.BoolVar(&c.showVersion, "version", false, "print the version and exit")
	flag.Parse()
	return c
}

func newLogger(format, level string) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level))
	opts := &slog.HandlerOptions{Level: lv}
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func listen(addr string) (net.Listener, error) {
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		_ = os.Remove(path) // stale socket from a previous run
		ln, err := net.Listen("unix", path)
		if err == nil {
			_ = os.Chmod(path, 0o660)
		}
		return ln, err
	}
	return net.Listen("tcp", addr)
}

// selfCheck returns the watchdog probe: GET /api/heartbeat through the real listener.
func selfCheck(ln net.Listener, useTLS bool) func(context.Context) error {
	tr := &http.Transport{DisableKeepAlives: true}
	scheme, host := "http", ln.Addr().String()
	if ln.Addr().Network() == "unix" {
		path := ln.Addr().String()
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
		host = "localhost"
	} else if tcp, ok := ln.Addr().(*net.TCPAddr); ok && tcp.IP.IsUnspecified() {
		host = net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port))
	}
	if useTLS {
		scheme = "https"
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // loopback self-check of our own certificate
	}
	client := &http.Client{Transport: tr}
	url := scheme + "://" + host + "/api/heartbeat"
	return func(ctx context.Context) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("heartbeat status %d", resp.StatusCode)
		}
		return nil
	}
}

func main() {
	c := parseFlags()
	if c.showVersion {
		fmt.Println(serverName, version, runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH)
		return
	}
	log := newLogger(c.logFormat, c.logLevel)
	slog.SetDefault(log)
	if err := run(c, log); err != nil {
		log.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func run(c config, log *slog.Logger) error {
	if c.memLimitMB > 0 {
		debug.SetMemoryLimit(int64(c.memLimitMB) << 20)
	}
	// Heavy work (encoding, compression) runs on dedicated low-priority threads.
	// With a single CPU, one extra P lets request handling run while a heavy job
	// is in progress; the kernel then prefers it because of the nice values.
	if runtime.GOMAXPROCS(0) < 2 {
		runtime.GOMAXPROCS(2)
	}
	workers := c.heavyWorkers
	if workers <= 0 {
		workers = max(1, runtime.GOMAXPROCS(0)-1)
	}
	niceApplied := heavy.Start(workers, c.heavyNice)
	var b [4]byte
	_, _ = rand.Read(b[:])
	bootID := hex.EncodeToString(b[:])
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ---- state: Remote Node telemetry (volatile_data)
	store := telemetry.NewStore(bootID, true)
	store.StaleAfter = c.staleAfter
	if c.nodes > 0 {
		sim := telemetry.NewSimulator(c.nodes, c.reportEvery, 1)
		for _, r := range sim.Initial() {
			store.Ingest(r)
		}
		go func() { // simulated Remote Nodes; replace with ingest from real nodes (-nodes 0 -ingest :9090)
			t := time.NewTicker(50 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-t.C:
					for _, r := range sim.Tick(now) {
						store.Ingest(r)
					}
				}
			}
		}()
	}
	store.Publish()
	go store.Run(c.publishEvery, ctx.Done())

	// ---- spectrum analyzer sessions
	hw := spectrum.NewSimHardware(c.sweepTime)
	for i := 1; i <= c.prewarm; i++ {
		if p, err := spectrum.ParseParams(strconv.Itoa(i), "1", "", "", "", c.defaultPoints); err == nil {
			hw.Prewarm(p)
		}
	}
	mgr := spectrum.NewManager(hw, bootID, c.spectrumIdle, c.minInterval, c.maxSessions)
	defer mgr.Close()

	// ---- push channels
	teleHub := hub.NewTelemetry(store, serverName, c.wsHeartbeat, c.maxWS, log)
	specHub := hub.NewSpectrum(mgr, serverName, c.defaultPoints, c.wsHeartbeat, c.maxWS)

	// ---- strangler fig: legacy backend for everything not implemented here
	var legacy *api.Legacy
	if c.legacy != "" {
		var err error
		if legacy, err = api.NewLegacy(c.legacy, log); err != nil {
			return err
		}
		go legacy.RunProbe(ctx, 2*time.Second)
	}
	var delegated []string
	for _, g := range strings.Split(c.delegate, ",") {
		switch g = strings.TrimSpace(g); g {
		case "":
		case "volatile", "spectrum", "ws":
			delegated = append(delegated, g)
		default:
			return fmt.Errorf("-delegate: unknown group %q (volatile, spectrum, ws; configuration: -config legacy)", g)
		}
	}
	if len(delegated) > 0 && legacy == nil {
		log.Warn("delegate_without_legacy", "groups", delegated, "effect", "these endpoints answer 404")
	}
	var auth *api.ForwardAuth
	switch {
	case c.authCheck != "" && legacy == nil:
		return errors.New("-auth-check needs -legacy (the legacy backend validates sessions)")
	case c.authCheck != "":
		auth = api.NewForwardAuth(legacy, c.authCheck, c.authTTL)
	case legacy != nil:
		log.Warn("auth_disabled", "hint", "set -auth-check to protect the endpoints the edge serves with the legacy login")
	}
	var configs *configstore.Store
	switch c.configMode {
	case "local":
		configs = configstore.New(bootID, max(c.nodes, 1024))
	case "legacy":
		if legacy == nil {
			return errors.New("-config legacy needs -legacy")
		}
	case "":
		if legacy == nil {
			configs = configstore.New(bootID, max(c.nodes, 1024))
		}
	default:
		return fmt.Errorf("-config must be local or legacy, not %q", c.configMode)
	}

	// ---- Remote Node ingest (Connect over h2c on the fiber management network)
	var ing *ingest.Service
	var ingestSrv *http.Server
	if c.ingestAddr != "" {
		ing = ingest.New(ingest.Options{Store: store, MaxNodeID: 4096, MinInterval: c.ingestMinInterval, Log: log})
		var p http.Protocols
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
		ingestSrv = &http.Server{Handler: ing.Handler(), Protocols: &p, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute,
			HTTP2: &http.HTTP2Config{MaxConcurrentStreams: 1024, SendPingTimeout: 20 * time.Second, PingTimeout: 10 * time.Second}}
		ln, err := listen(c.ingestAddr)
		if err != nil {
			return fmt.Errorf("ingest listener: %w", err)
		}
		go func() {
			if err := ingestSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("ingest_server", "error", err.Error())
			}
		}()
		log.Info("ingest_listening", "addr", ln.Addr().String(), "procedure", ingest.Procedure)
	}

	// ---- web UI
	var ui fs.FS = static.Embedded()
	if c.web != "" {
		if st, err := os.Stat(c.web); err != nil || !st.IsDir() {
			return fmt.Errorf("-web %q is not a directory", c.web)
		}
		ui = os.DirFS(c.web)
	}

	handler := api.New(api.Options{
		Server: serverName, Version: version, BootID: bootID,
		Store: store, Spectrum: mgr, Configs: configs, Legacy: legacy, Static: static.New(ui),
		Auth: auth, AuthExempt: strings.Split(c.authExempt, ","), Delegate: delegated,
		TelemetryWS: teleHub, SpectrumWS: specHub,
		DefaultPoints: c.defaultPoints, SweepWait: c.sweepWait, Log: log,
		Extra: func() map[string]any {
			out := map[string]any{"wsTelemetry": teleHub.Stats(), "wsSpectrum": specHub.Stats(), "heavy": heavy.Stats()}
			if ing != nil {
				out["ingest"] = ing.Stats()
			}
			return out
		},
	})

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	ln, err := listen(c.listen)
	if err != nil {
		return fmt.Errorf("listener: %w", err)
	}
	useTLS := c.tlsCert != "" && c.tlsKey != ""
	go func() {
		var err error
		if useTLS {
			err = srv.ServeTLS(ln, c.tlsCert, c.tlsKey) // HTTP/2 negotiated via ALPN
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http_server", "error", err.Error())
			stop()
		}
	}()

	if c.pprofAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		go func() { _ = http.ListenAndServe(c.pprofAddr, mux) }()
	}

	log.Info("listening", "addr", ln.Addr().String(), "tls", useTLS, "version", version, "bootId", bootID,
		"nodesSimulated", c.nodes, "legacy", c.legacy, "configOwner", map[bool]string{true: "edge", false: "legacy"}[configs != nil], "delegated", delegated,
		"gomaxprocs", runtime.GOMAXPROCS(0), "heavyWorkers", workers, "heavyNice", niceApplied, "cpuSlowdown", heavy.Factor)
	_, _ = sysd.Notify("READY=1\nSTATUS=serving on " + ln.Addr().String())
	go sysd.RunWatchdog(ctx, selfCheck(ln, useTLS), log)

	<-ctx.Done()
	log.Info("shutting_down")
	_, _ = sysd.Notify("STOPPING=1")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	teleHub.Close()
	specHub.Close()
	if ingestSrv != nil {
		_ = ingestSrv.Shutdown(sctx)
	}
	return srv.Shutdown(sctx)
}
