# das-03 · Long-term (LTS): the Go edge server

One static binary on the Master Node serves the web UI, the REST API, WebSocket push and
the Remote Node ingest stream, all from **one origin**. It forwards everything it does not
implement yet to the existing Node.js backend (strangler fig), so it can go in front of
today's software without a big-bang rewrite.

It implements the das-v1 contract from `das-00` and passes the same conformance suite
as the hotfix (`das-01`) and the gateway (`das-02`). The Angular client (`das-04`)
runs against it unchanged.

```
             browser (Angular)                       Remote Nodes (fiber)
                    │ HTTPS, HTTP/2 + WebSocket              │ Connect streaming, h2c
                    ▼                                        ▼
 ┌───────────────────────────────── das-edge ─────────────────────────────────────┐
 │ static UI (gzip once, cached)      REST das-v1          ingest (NodeIngestService)│
 │ /api/ws/telemetry  /api/ws/spectrum                     deltas + state hash       │
 │        │                 │               │                     │                 │
 │        ▼                 ▼               ▼                     ▼                 │
 │  telemetry store: revisions, deltas, ETags   ◄───────── normalise + deadbands     │
 │  spectrum sessions: one sweep, every variant encoded ONCE (low-priority threads)  │
 │  /api/* not implemented here  ──────► legacy Node.js backend (unix socket)        │
 └──────────────────────────────────────────────────────────────────────────────────┘
```

## Results

Same scenario as `das-01`'s demo and `das-02`'s benchmark: 4 engineers × 2 spectrum
traces (legacy endpoint, 50 001 points) plus dashboards and config reads, for 30 s.
The server is pinned to **one core**, the embedded CPU is emulated (every heavy
operation × 8), and 10 % of that core is taken by "other daemons".

| | Shipped release | Hotfix (01) | Gateway (02) | **Go edge (03)** |
|---|---:|---:|---:|---:|
| Heartbeat p99 | 3 000 ms (timeout) | 5.7 ms | 11.1 ms | **4.7 ms** |
| Heartbeat timeouts / false "server dead" alarms | 48 / 19 | 0 / 0 | 0 / 0 | **0 / 0** |
| Spectrum responses in 30 s (p50 latency) | 74 (3.4 s) | 120 (2.2 s) | 108 (2.4 s) | **408 (0.6 s)** |
| Dashboard p50 / config API p50 | 2.1 s / 2.2 s | 28 ms / 1.4 ms | 1.2 ms / 1.7 ms | **1.0 ms / 0.8 ms** |
| Server memory, peak | 211 MB RSS | 239 MB RSS | 286 MB PSS (4 processes) | **43 MB PSS** |

Full report with latency timelines: `bench/results/latest.html` (re-run: `make bench`).

Ingest path (`make demo-ingest`): 300 simulated Remote Nodes streaming for 60 s sent
**0.88 MB instead of 21.2 MB** (24× less; 40× in steady state), with 0 resyncs and 0 hash
mismatches. The master spent 1.4 s of CPU on it. Details: [docs/INGEST.md](docs/INGEST.md).

Also verified:

- **Conformance:** 18/18 checks over HTTP, 17/17 over HTTPS with HTTP/2 (`make conformance`).
- **Migration steps:** each strangler step in front of the shipped behaviour passes conformance, including the heartbeat SLO (`make strangler`).
- **Angular client:** its Playwright end-to-end tests pass 3/3 with the edge serving the build (`make e2e`).
- **Connect-ES client:** the official client, generated from `contract/proto`, streams to the ingest endpoint (`interop/connect-es`).
- **ARM builds:** the arm64 and armv7 binaries run under qemu.
- **systemd:** READY, the watchdog and STOPPING were checked on a notify socket, and the unit passes `systemd-analyze verify`.

## Why it fixes the problem for good

| Cause of the false "server is dead" alarm | What the edge does |
|---|---|
| Serialising 50 001 points blocks the only event loop | Each sweep is encoded **once per variant** (legacy JSON, compact JSON, binary, decimated) and shared by every client. Encoding runs on dedicated **nice +10 threads**, and the kernel preempts them for request handling. |
| Every poll re-encodes the same data | Snapshots, deltas, sweeps and static files are built once per change. A request only looks up a map and writes bytes. |
| Sequential polling of huge payloads | Push over WebSocket (snapshot, then deltas, binary spectrum frames). A slow client is **conflated** (it gets the latest state) instead of queued. Poll clients get ETag/304, `?since=` deltas and long-poll. |
| Every Remote Node report looks like a change | Field hygiene and deadbands at the node **and** at the master. Nodes send deltas with a **state hash** (checksum), and the master verifies the hash before applying a delta. |
| A heavy client can starve everyone | Heavy work is bounded to `GOMAXPROCS-1` threads, and sessions, WebSocket clients and message sizes have limits. The watchdog only pings while the heartbeat path works. |

## Quick start

Requires Go 1.24+ to build. The target needs nothing (static binary, no libc).

```sh
make build                     # bin/das-edge, bin/das-nodesim
./bin/das-edge -log text       # 300 simulated nodes + simulated analyzer on :8080
# open http://localhost:8080 (built-in status page: live node grid + spectrum trace)

./bin/das-edge -web ../das-04-angular-client/dist/das-04-angular-client/browser   # the real UI
make demo-ingest               # real data path: 300 nodes stream over h2c, no simulator in the edge
make cross sizes               # arm64 / armv7 / amd64 binaries (≈7.5 MB, ≈3 MB gzipped)
make race conformance e2e bench
```

In front of the existing backend (strangler fig):

```sh
# step 1: the edge only serves the UI and the heartbeat; everything else goes to the Node app
das-edge -web /opt/das/ui -legacy unix:/run/das/core-api.sock -auth-check /api/session \
         -delegate volatile,spectrum,ws -config legacy
# step 2: take over the spectrum analyzer (the cause of the incident)
das-edge ... -delegate volatile,ws -config legacy
```

- Every `/api/*` path the edge does not serve goes to the legacy backend, with the
  browser's `Host`.
- `/api/capabilities` merges what the edge serves with what the legacy backend
  advertises, so clients never opt into a feature the answering process lacks.
- `-auth-check` keeps the shipped login: the edge asks the legacy backend whether a
  session is valid (like nginx `auth_request`) and caches the answer.
- The heartbeat reports the legacy backend under `services.legacy`.

The step-by-step plan is in [docs/MIGRATION.md](docs/MIGRATION.md).

## What is inside

| Package | Role |
|---|---|
| `cmd/das-edge` | Wiring, flags (all also `DAS_*` env vars), TLS/HTTP/2, h2c ingest listener, pprof, graceful shutdown, systemd notify. |
| `cmd/das-nodesim` | Remote Node fleet simulator speaking the ingest protocol (for demos and load tests). |
| `internal/api` | das-v1 REST handlers, content negotiation, ETag/304, route metrics, Prometheus text, scheduling-lag monitor, strangler proxy. |
| `internal/telemetry` | volatile_data: typed node state, deadbands, canonical encoding + FNV-1a hash, revisions (random per-boot base), deltas, shared snapshots. |
| `internal/spectrum` | Sweep sessions (single-flight: one hardware sweep for all viewers), peak-detector decimation, DSPC binary frame, variant cache. |
| `internal/hub` | WebSocket channels: per-client writer woken (never queued), shared pre-compressed deltas, conflation, limits. |
| `internal/ws` | RFC 6455 + permessage-deflate server, prepared (compress-once) messages. |
| `internal/ingest` | `das.v1.NodeIngestService/Report` over Connect (JSON codec): deltas by field path, hash verification, RESYNC, SLOW_DOWN, keep-alives. Node-side agent. |
| `internal/heavy` | Low-priority worker threads for encoding and compression (+ CPU-slowdown emulation for benchmarks). |
| `internal/static` | UI files: SPA fallback, immutable caching for fingerprinted assets, gzip once. Embeds the status page. |
| `internal/sysd` | sd_notify: READY/STOPPING/STATUS and a watchdog that only pings while the heartbeat works. |
| `internal/configstore` | Node configuration with optimistic locking (used when the edge owns configuration). |

**Standard library only.** The build environment's network policy blocked the Go module
proxy, so the PoC uses no third-party modules: the WebSocket, Connect framing and systemd
notify code are small and tested. For production, the seams are ready for
`github.com/coder/websocket`, `connectrpc.com/connect` (generated from the same protos,
which adds gRPC and gRPC-Web on the same path) and the Prometheus client. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#dependencies).

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): data flow, concurrency model, memory, failure modes and design decisions.
- [docs/INGEST.md](docs/INGEST.md): the Remote Node protocol (deltas, checksums, RESYNC), measured savings, and the gRPC path.
- [docs/OPERATIONS.md](docs/OPERATIONS.md): systemd, flags and environment, metrics, logs, pprof, TLS, upgrades and rollback.
- [docs/MIGRATION.md](docs/MIGRATION.md): moving from the shipped Node backend to the edge step by step, alone or behind the das-02 gateway.
- [docs/BENCHMARKS.md](docs/BENCHMARKS.md): methodology and all measurements.

## Layout

```
cmd/            das-edge, das-nodesim
internal/       packages above (each with tests: go test -race ./...)
conformance/    das-v1 conformance suite (copy of das-00/conformance)
bench/          heartbeat-under-load benchmark + edge driver, results/
deploy/         systemd unit + environment file, nginx snippet for mixing with das-02
interop/        Connect-ES client check against the ingest endpoint
scripts/        conformance, ingest demo, Angular end-to-end
docs/           design and operations documentation, screenshots
```
