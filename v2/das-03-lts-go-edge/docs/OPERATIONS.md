# Operating das-edge on the Master Node

## Install

```sh
make cross                                        # dist/das-edge-linux-{arm64,armv7,amd64}
scp dist/das-edge-linux-arm64 master:/opt/das-edge/das-edge
scp -r ../das-04-angular-client/dist/das-04-angular-client/browser master:/opt/das-edge/ui
scp deploy/systemd/das-edge.service master:/etc/systemd/system/
scp deploy/systemd/das-edge.env master:/etc/default/das-edge      # edit: TLS, legacy socket, ingest address
ssh master 'useradd -r -s /usr/sbin/nologin das; systemctl daemon-reload; systemctl enable --now das-edge'
```

The binary is static, so it needs no Go runtime, libc or node_modules on the device.
Sizes: arm64 7.3 MB, armv7 7.5 MB (about 3 MB gzipped).

## systemd

`deploy/systemd/das-edge.service`:

- `Type=notify`: systemd considers the service started when the listeners are bound
  (`READY=1`, plus a `STATUS=` line).
- `WatchdogSec=10`: the edge pings every 5 s, **but only after a real
  `GET /api/heartbeat` through its own listener succeeded**. A deadlocked or starved
  process is therefore restarted, not kept alive by an independent timer.
  `Restart=always` with a start-rate limit.
- `STOPPING=1`, then a graceful shutdown: WebSocket clients get `1001 Going Away` and
  reconnect elsewhere or later; in-flight requests get up to 5 s.
- Memory: `GOMEMLIMIT=96MiB` (the GC works towards it) and `MemoryMax=160M` (the kernel
  enforces it).
- `Nice=-5`, `CPUWeight=400` for request handling. Encoding threads renice themselves to
  +10.
- Sandboxing: `ProtectSystem=strict`, `NoNewPrivileges`, `PrivateDevices`, restricted
  address families; `CAP_NET_BIND_SERVICE` only (for port 443).

Check the unit with `systemd-analyze verify /etc/systemd/system/das-edge.service`.

## Configuration

Every flag has an environment variable, which is what the unit file uses (EnvironmentFile
`/etc/default/das-edge`).

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-listen` | `DAS_LISTEN` | `:8080` | `host:port` or `unix:/path.sock` (behind nginx) |
| `-tls-cert`, `-tls-key` | `DAS_TLS_CERT`, `DAS_TLS_KEY` | – | HTTPS with HTTP/2 (ALPN); WebSockets upgrade over HTTP/1.1 on the same port |
| `-web` | `DAS_WEB_ROOT` | built-in status page | directory of the Angular build |
| `-legacy` | `DAS_LEGACY` | – | legacy backend: `http://host:port` or `unix:/path.sock` |
| `-config` | `DAS_CONFIG` | `legacy` if `-legacy`, else `local` | who serves `/api/nodes/{id}/config` |
| `-auth-check` | `DAS_AUTH_CHECK_PATH` | – (off) | legacy path answering 2xx for a logged-in session, e.g. `/api/session` |
| `-auth-exempt` | `DAS_AUTH_EXEMPT` | `heartbeat,capabilities` | routes open without a session |
| `-auth-ttl` | `DAS_AUTH_TTL_MS` | 30 s | cache time of a session check |
| `-ingest` | `DAS_INGEST_LISTEN` | – (off) | Remote Node streams (Connect over h2c), e.g. `10.10.0.1:9090` |
| `-ingest-min-interval` | `DAS_INGEST_MIN_INTERVAL_MS` | 200 ms | faster reporters get `SLOW_DOWN` |
| `-nodes` | `DAS_NODES` | 300 | simulated Remote Nodes (0 on a real system) |
| `-stale-after` | `DAS_STALE_AFTER_MS` | 15 s | node published as offline after this silence |
| `-publish-interval` | `DAS_PUBLISH_INTERVAL_MS` | 1 s | telemetry revision batching |
| `-points` | `DAS_SPECTRUM_POINTS` | 50 001 | sweep points when the client does not say |
| `-max-sessions` | `DAS_SPECTRUM_MAX_SESSIONS` | 8 | concurrent analyzer configurations |
| `-spectrum-idle` | `DAS_SPECTRUM_IDLE_MS` | 15 s | stop sweeping when nobody watches |
| `-spectrum-min-interval` | `DAS_SPECTRUM_MIN_INTERVAL_MS` | 0 | minimum time between sweeps (protects the hardware) |
| `-sweep-wait` | `DAS_SWEEP_WAIT_MS` | 10 s | longest wait for a sweep before 503 |
| `-sweep-time` | `DAS_SWEEP_TIME_MS` | 250 ms | simulator only |
| `-max-ws` | `DAS_MAX_WS_CLIENTS` | 64 | WebSocket clients per channel |
| `-ws-heartbeat` | `DAS_WS_HEARTBEAT_MS` | 5 s | `hb` message after this much silence |
| `-heavy-workers` | `DAS_HEAVY_WORKERS` | GOMAXPROCS-1 | encoding/compression threads |
| `-heavy-nice` | `DAS_HEAVY_NICE` | 10 | their nice value |
| `-mem-limit-mb` | `DAS_MEM_LIMIT_MB` | – | Go soft memory limit (or use `GOMEMLIMIT`) |
| `-pprof` | `DAS_PPROF` | – (off) | e.g. `127.0.0.1:6060`, never on a public interface |
| `-log`, `-log-level` | `DAS_LOG_FORMAT`, `DAS_LOG` | `json`, `info` | structured logs on stderr (journald) |

`DAS_CPU_SLOWDOWN=K` repeats every heavy operation K times, to emulate a slower CPU on a
development machine. Never set it on a device.

## Observability

- `GET /api/heartbeat` returns `status` (`ok`/`degraded`), `runtime` (goroutines, heap,
  RSS, scheduling-lag percentiles) and `services.legacy`. It is cheap enough to poll
  every second.
- `GET /api/metrics` is JSON: route counters (requests, 304s, 5xx, bytes), runtime,
  scheduling lag, telemetry revisions, spectrum sessions, WebSocket hubs (clients,
  snapshots, catch-ups, conflated frames, wire bytes), ingest (streams, full, deltas,
  keep-alives, resyncs, hash mismatches), heavy pool (jobs, busy seconds, queue), auth
  and legacy proxy.
- `GET /api/metrics?format=prom` is the same in Prometheus text format. Examples:
  - `das_http_requests_total{route="spectrum-latest"}`
  - `das_edge_lag_sched_lag_p99_ms`
  - `das_ingest_resyncs`
  - `das_heavy_busy_seconds`
  - `das_ws_telemetry_clients`
- Logs are JSON lines: `listening`, `ingest_listening`, `config_changed`,
  `legacy_proxy_error`, `ingest_hash_mismatch`, `watchdog_check_failed`, `shutting_down`.
- `-pprof 127.0.0.1:6060` enables CPU and heap profiles:
  `go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=20`.

Alert on these first:

- the heartbeat is `degraded` for more than 1 minute;
- `das_ingest_resyncs` or `das_ingest_hash_mismatches` keep increasing (a firmware and
  master version mismatch);
- `das_http_errors_total` increases;
- RSS approaches `MemoryMax`.

## Upgrade and rollback

- The edge is one file. For an upgrade, replace `/opt/das-edge/das-edge` and restart.
  Clients reconnect, and the new boot id invalidates all ETags and revisions, so no
  client keeps stale data.
- To roll back, stop the edge and let the legacy Node backend listen on the public port
  again (or point nginx back at it). As long as the legacy backend keeps running behind
  the edge during the migration, rollback is a port change (see MIGRATION.md).
- Contract compatibility: before a release, run `node conformance/run.js --base https://<device> --insecure`.
