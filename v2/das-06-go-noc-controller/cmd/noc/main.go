// Command noc is the DAS NOC fleet controller: thousands of Master Units dial in
// and report their state, NOC operators and customers watch the fleet.
//
//	DAS_NOC_SECRET=... noc -listen :8080                  # devices: ws://host:8080/uplink/v1
//	noc token -kind site -subject S00042 -tenant airport  # provisioning: a device token
//	noc -demo                                             # prints an admin URL for the NOC page
//
// Every flag can also be set with an environment variable: -admit-rate is
// DAS_NOC_ADMIT_RATE, and so on.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dasnoc/internal/api"
	"dasnoc/internal/dash"
	"dasnoc/internal/fleet"
	"dasnoc/internal/proto"
	"dasnoc/internal/uplink"
)

var version = "0.6.0"

// envDefaults lets every flag be set from DAS_NOC_<FLAG_NAME>.
func envDefaults(fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		name := "DAS_NOC_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v, ok := os.LookupEnv(name); ok {
			_ = f.Value.Set(v)
			if f.Name == "secret" || f.Name == "secret-prev" {
				f.DefValue = "(set by " + name + ")" // -h must not print it
			} else {
				f.DefValue = v
			}
		}
	})
}

func readSecret(value, file string) ([]byte, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		return []byte(strings.TrimSpace(string(b))), nil
	}
	return []byte(value), nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "token" {
		os.Exit(tokenCmd(os.Args[2:]))
	}
	fs := flag.NewFlagSet("noc", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "public listener: devices (/uplink/v1), dashboards, REST, NOC page")
	internalListen := fs.String("internal-listen", "127.0.0.1:9090", "internal listener: /metrics (Prometheus), /healthz (probes)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate (PEM); empty: plain HTTP (TLS at the ingress)")
	tlsKey := fs.String("tls-key", "", "TLS key (PEM)")
	secret := fs.String("secret", "", "token signing secret (prefer -secret-file or DAS_NOC_SECRET)")
	secretFile := fs.String("secret-file", "", "file with the token signing secret")
	secretPrev := fs.String("secret-prev", "", "previous secret, still accepted during a rotation")
	secretPrevFile := fs.String("secret-prev-file", "", "file with the previous secret")
	revokedFile := fs.String("revoked-file", "", "revoked token subjects, one per line: site:<id> or user:<name> (re-read when it changes, and on SIGHUP)")
	inventory := fs.String("inventory", "", "JSON inventory of expected sites [{id, tenant, name, venue, region}]")
	dataDir := fs.String("data-dir", "", "where the site registry and alarm acknowledgements are kept across restarts")
	grace := fs.Duration("grace", 90*time.Second, "a disconnected site is 'stale' this long, then 'offline' with a critical alarm")
	keepalive := fs.Duration("keepalive", 25*time.Second, "ping interval towards devices")
	admitRate := fs.Float64("admit-rate", 200, "device handshakes admitted per second")
	admitBurst := fs.Int("admit-burst", 400, "handshake burst")
	retryAfterMax := fs.Int("retry-after-max", 15, "refused devices retry after 1..N s (randomised)")
	maxSites := fs.Int("max-sites", 20000, "concurrent device connections at most")
	logFormat := fs.String("log", "text", "log format: text or json")
	logLevel := fs.String("log-level", "info", "debug, info, warn, error")
	demo := fs.Bool("demo", false, "print a URL with an admin token for the NOC page")
	showVersion := fs.Bool("version", false, "print the version")
	envDefaults(fs)
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println("noc", version)
		return
	}

	var level slog.Level
	_ = level.UnmarshalText([]byte(*logLevel))
	opts := &slog.HandlerOptions{Level: level}
	var log *slog.Logger
	if *logFormat == "json" {
		log = slog.New(slog.NewJSONHandler(os.Stdout, opts))
	} else {
		log = slog.New(slog.NewTextHandler(os.Stdout, opts))
	}

	cur, err := readSecret(*secret, *secretFile)
	if err != nil || len(cur) < 16 {
		log.Error("a token signing secret of at least 16 bytes is required (-secret-file or DAS_NOC_SECRET)", "err", err)
		os.Exit(2)
	}
	verify := &proto.Verifier{Secrets: [][]byte{cur}}
	if prev, _ := readSecret(*secretPrev, *secretPrevFile); len(prev) > 0 {
		verify.Secrets = append(verify.Secrets, prev)
	}
	var revokedStamp string
	if *revokedFile != "" {
		list, stamp, err := loadRevoked(*revokedFile)
		if err != nil {
			log.Error("revoked file", "err", err)
			os.Exit(2)
		}
		verify.SetRevoked(list)
		revokedStamp = stamp
	}

	store := fleet.New(fleet.Config{Grace: *grace})
	registryPath := ""
	if *dataDir != "" {
		if err := os.MkdirAll(*dataDir, 0o750); err != nil {
			log.Error("data dir", "err", err)
			os.Exit(2)
		}
		registryPath = filepath.Join(*dataDir, "registry.json")
		entries, err := fleet.LoadRegistry(registryPath)
		if err != nil {
			log.Error("registry", "err", err)
			os.Exit(2)
		}
		store.RestoreRegistry(entries)
		log.Info("registry_restored", "sites", len(entries))
	}
	if *inventory != "" {
		inv, err := fleet.LoadInventoryFile(*inventory)
		if err != nil {
			log.Error("inventory", "err", err)
			os.Exit(2)
		}
		store.LoadInventory(inv)
		log.Info("inventory_loaded", "sites", len(inv))
	}

	up := uplink.New(uplink.Config{Keepalive: *keepalive, AdmitRate: *admitRate, AdmitBurst: *admitBurst, RetryAfterMax: *retryAfterMax, MaxSessions: *maxSites}, store, verify, log)
	hub := dash.New(dash.Config{}, store, verify, log)
	srv := &api.Server{Store: store, Uplink: up, Hub: hub, Verify: verify, Log: log, Version: version, Started: time.Now()}

	if *demo {
		tok, _ := proto.Mint(cur, proto.Claims{Kind: proto.KindUser, Subject: "demo-admin", Tenant: proto.AllTenants})
		host := *listen
		if strings.HasPrefix(host, ":") {
			host = "127.0.0.1" + host
		}
		scheme := "http"
		if *tlsCert != "" {
			scheme = "https"
		}
		fmt.Printf("NOC page: %s://%s/#token=%s\n", scheme, host, tok)
	}

	public := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug)}
	internal := &http.Server{Addr: *internalListen, Handler: srv.InternalHandler(), ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 2)
	go func() {
		var err error
		if *tlsCert != "" {
			err = public.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = public.ListenAndServe()
		}
		errc <- err
	}()
	go func() { errc <- internal.ListenAndServe() }()
	log.Info("noc_listening", "version", version, "listen", *listen, "internal", *internalListen, "tls", *tlsCert != "",
		"sites", store.Len(), "admitRate", *admitRate, "keepalive", keepalive.String(), "grace", grace.String())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	// The revoked list is re-read when the file changes (a Kubernetes ConfigMap
	// update, an edit on disk) or on SIGHUP; connections of revoked devices and
	// users are closed at once.
	reloadRevoked := func(force bool) {
		if *revokedFile == "" {
			return
		}
		list, stamp, err := loadRevoked(*revokedFile)
		if err != nil {
			log.Warn("revoked_file_unreadable", "err", err) // keep the previous list
			return
		}
		if !force && stamp == revokedStamp {
			return
		}
		revokedStamp = stamp
		verify.SetRevoked(list)
		log.Info("revoked_list_loaded", "entries", len(list), "devicesDropped", up.DropRevoked(), "dashboardsDropped", hub.DropRevoked())
	}
	revokedCheck := time.NewTicker(10 * time.Second)
	defer revokedCheck.Stop()
	sweep := time.NewTicker(2 * time.Second)
	save := time.NewTicker(30 * time.Second)
	status := time.NewTicker(60 * time.Second)
	defer sweep.Stop()
	defer save.Stop()
	defer status.Stop()
	saveRegistry := func() {
		if registryPath != "" {
			if err := store.SaveRegistry(registryPath); err != nil {
				log.Warn("registry_save_failed", "err", err)
			}
		}
	}
loop:
	for {
		select {
		case err := <-errc:
			if !errors.Is(err, http.ErrServerClosed) {
				log.Error("listener_failed", "err", err)
				os.Exit(1)
			}
		case <-sweep.C:
			store.Sweep()
		case <-revokedCheck.C:
			reloadRevoked(false)
		case <-hup:
			reloadRevoked(true)
		case <-save.C:
			saveRegistry()
		case <-status.C:
			o := store.Overview(proto.Claims{Kind: proto.KindUser, Tenant: proto.AllTenants})
			log.Info("fleet", "sites", o.Sites, "connected", o.Connected, "critical", o.ByStatus[fleet.StatusCritical], "offline", o.ByStatus[fleet.StatusOffline],
				"alarms", o.Alarms[fleet.SevCritical]+o.Alarms[fleet.SevMajor]+o.Alarms[fleet.SevMinor]+o.Alarms[fleet.SevWarning], "dashboards", hub.M.Clients.Load())
		case sig := <-stop:
			log.Info("shutting_down", "signal", sig.String())
			break loop
		}
	}
	// Devices reconnect (with jitter) to whichever instance comes up next.
	up.Shutdown()
	hub.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = public.Shutdown(ctx)
	_ = internal.Shutdown(ctx)
	saveRegistry()
}

// loadRevoked reads a revoked-subjects file: one "site:<id>" or "user:<name>" per
// line, # comments. The stamp changes whenever the file does.
func loadRevoked(path string) (map[string]bool, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	list := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			list[line] = true
		}
	}
	return list, proto.HashHex(proto.FNV1a64(b)), nil
}

func tokenCmd(args []string) int {
	fs := flag.NewFlagSet("noc token", flag.ExitOnError)
	kind := fs.String("kind", "site", "site (a Master Unit) or user (an operator)")
	subject := fs.String("subject", "", "site id or user name ([A-Za-z0-9_-], up to 64)")
	tenant := fs.String("tenant", "", "tenant id; '*' = the whole fleet (users only)")
	secret := fs.String("secret", "", "signing secret (default: DAS_NOC_SECRET)")
	secretFile := fs.String("secret-file", "", "file with the signing secret (default: DAS_NOC_SECRET_FILE)")
	_ = fs.Parse(args)
	if *secret == "" {
		*secret = os.Getenv("DAS_NOC_SECRET")
	}
	if *secretFile == "" {
		*secretFile = os.Getenv("DAS_NOC_SECRET_FILE")
	}
	s, err := readSecret(*secret, *secretFile)
	if err != nil || len(s) < 16 {
		fmt.Fprintln(os.Stderr, "a secret of at least 16 bytes is required")
		return 2
	}
	tok, err := proto.Mint(s, proto.Claims{Kind: *kind, Subject: *subject, Tenant: *tenant})
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid token claims:", err)
		return 2
	}
	fmt.Println(tok)
	return 0
}
