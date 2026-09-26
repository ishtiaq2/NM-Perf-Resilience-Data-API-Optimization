# das-edge architecture

## Constraints this design starts from

- **Embedded Master Node.** It has a few ARM cores and a few hundred MB of RAM, which it
  shares with RF control, SNMP, alarm handling and other daemons. It boots from flash and
  may have no internet access.
- **Hundreds of Remote Nodes** report telemetry that is merged into `volatile_data`.
- **Large data on demand.** A spectrum sweep has up to 200 001 points.
- **One origin for the browser.** There is one host and one certificate, no CORS, and
  existing URLs keep working.
- **Customers already run the shipped release.** Everything has to be additive and
  reversible.

## Request paths

| Path | Work per request | Built once per |
|---|---|---|
| `GET /api/heartbeat` | a map, JSON of ~300 bytes | request (tiny) |
| `GET /api/volatile-data` | lookup of the current revision; 304 if the ETag matches | revision (1 s); snapshot and gzip built right after publish, in the background |
| `GET /api/volatile-data?since=r` | union of changed ids from the revision history + node JSON splice | request (small) |
| `GET /api/spectrum` (legacy) | wait for the next sweep, write the shared body | sweep × encoding (legacy JSON, gzip or not) |
| `GET /api/spectrum/latest` | ETag check (304 builds nothing), optional long-poll | sweep × variant (json/i16/f32 × maxPoints × gzip) |
| `GET/PUT /api/nodes/{id}/config` | edge: in-memory store with If-Match; or proxied to legacy | – |
| `/api/ws/telemetry` | writer goroutine wakes, writes shared pre-compressed delta | revision |
| `/api/ws/spectrum` | writer goroutine wakes, writes the shared binary frame | sweep × variant |
| any other `/api/*` | reverse proxy to the legacy backend | – |
| UI files | bytes from memory (gzip done once) | file |

The rule behind every row: **work is proportional to changes, not to requests or
clients.** Ten engineers watching the same analyzer cost one sweep, one encoding per
representation and ten socket writes.

## Concurrency model

- **Goroutine per connection** (net/http). There is no event loop to block. A slow
  handler delays only its own client.
- **Heavy work goes to low-priority threads** (`internal/heavy`). Encoding, compression
  and normalisation run on `GOMAXPROCS-1` dedicated OS threads, each reniced to +10. Go
  has no goroutine priorities; the kernel does, so request handling on the other threads
  wins the CPU when both are runnable. On a single-core device the edge still runs
  GOMAXPROCS=2 so that one P is always free for requests.
  - Measured on one core with ×8 CPU emulation: heartbeat p99 went from 26.9 ms with
    the work on ordinary goroutines to 4.7 ms with it on reniced threads.
  - Rule: a function passed to `heavy.Do` must not call `heavy.Do`. The test suites run
    with a one-thread pool, so nesting would deadlock and fail the tests.
- **Spectrum sessions are single-flight.** Each analyzer configuration (node, port, range,
  points) has one sweep loop. It runs only while someone watches: every request or open
  WebSocket touches it, and after 15 s idle it stops. Every waiter is woken by closing a
  channel.
- **Telemetry is batched.** Reports update per-node entries as they arrive, and a
  revision is published at most once per second. A revision is an immutable `View`
  (sorted ids, per-node JSON slices shared between revisions). Snapshot bytes are built
  lazily and at most once (`sync.Once`).
- **WebSocket hubs have one writer goroutine per client**, woken through a 1-slot channel
  and never fed from a queue. On wake-up the writer sends whatever brings its client to
  the latest state:
  - the shared delta, if the client is exactly one revision behind;
  - a merged catch-up delta, if it is further behind;
  - a snapshot, if the history has expired.

  A slow client therefore costs O(1) memory and is conflated automatically: in a test,
  300 revisions on a throttled link arrived as 2 messages. A client that cannot absorb a
  write within 10 s is disconnected.
- **Everything has a limit:**
  - 8 analyzer sessions and 64 WebSocket clients per channel (503 + Retry-After);
  - 64 KiB request headers and 256 KiB config bodies;
  - 16 KiB / 4 KiB WebSocket messages from clients and 1 MiB ingest messages;
  - 10 s write timeout.

## Revisions, ETags and deltas

- Revisions start at a **random per-boot base** between 2^40 and 2^52, like a TCP
  initial sequence number. A client that kept a revision from before a reboot therefore
  holds a number that is either older than the history or newer than the current
  revision. Either way it gets a snapshot, never a delta against data it does not have.
  JSON-safe (< 2^53).
- ETags contain the boot id and the revision (`"vd-<boot>-<rev>[-gz]"`). Identity and
  gzip representations have different tags. `If-None-Match` uses weak comparison;
  `If-Match` on config uses strong comparison.
- History: 120 revisions (2 minutes). Older `since=` values get a snapshot.

## Memory

- Snapshots, deltas, sweeps and variants are immutable once published and shared by
  reference. There are no per-request copies of large data.
- `GOMEMLIMIT` (soft) and systemd `MemoryMax` (hard) are both set. At idle the edge uses
  about 20–27 MB RSS. Under the benchmark load (8 analyzers × 50 001 points, 4
  dashboards) its PSS peaked at 43 MB, against 239 MB for the Node hotfix and 286 MB
  for the gateway stack.
- Spectrum variants are dropped with their sweep. Only the latest sweep per session is
  referenced, and the simulator keeps at most 16 templates.

## Failure modes

| Failure | Behaviour |
|---|---|
| Legacy backend down | Proxied paths answer 502 `UPSTREAM_UNAVAILABLE`. The heartbeat says `degraded` with `services.legacy: down`. Everything the edge serves keeps working. |
| Analyzer does not complete a sweep | `/api/spectrum` returns the last sweep if there is one (the shipped UI prefers stale data to an error), else 503 `SWEEP_TIMEOUT` with Retry-After. |
| Too many analyzer configurations or WebSocket clients | 503 with Retry-After; idle sessions are evicted first. |
| A Remote Node's delta does not match | `RESYNC`: the node sends one full report. Unverified data is never applied (hash check). |
| Master restarts | Node streams reconnect with a full report. Browser clients get a new boot id, so no old ETag or revision can match. |
| Process wedged (deadlock, CPU starvation) | The systemd watchdog is fed only while a real `GET /api/heartbeat` through the listener succeeds; otherwise systemd restarts the service within 10 s. |
| Slow or stalled browser | Conflation, then disconnect after the 10 s write timeout; nobody else waits. |
| Burst of expensive distinct requests | Heavy work queues on the low-priority threads; requests that need nothing heavy are unaffected. |

## Security

- **Authentication stays with the shipped release.** `-auth-check /api/session` makes the
  edge ask the legacy backend whether the request's cookie/Authorization is logged in,
  like nginx `auth_request`. The answer is cached per credential (hashed) for 30 s.
  Checks fail closed. Proxied paths are authenticated by the legacy backend itself. The
  heartbeat and capabilities stay open by default, so a logged-out UI can still tell
  that the server is alive.
- **WebSocket upgrades require a same-origin `Origin`** (CSWSH protection); non-browser
  tools without an `Origin` are allowed.
- TLS with HTTP/2 when `-tls-cert` and `-tls-key` are given. WebSockets use HTTP/1.1
  upgrades on the same port.
- The systemd unit sandboxes the process: no new privileges, read-only system, private
  /tmp, no devices, restricted address families, `CAP_NET_BIND_SERVICE` only.
- Ingest listens on the fiber management network (`-ingest 10.x.x.x:9090`). For
  production add mTLS per Remote Node (the Connect protocol is unchanged over TLS).

## Design decisions (short form; the full ADRs are in das-00)

| Decision | Chosen | Alternatives considered | Why |
|---|---|---|---|
| Language for the device server | **Go** | Rust, C++, Node.js (cluster/workers), Java | Static cross-compiled binary, preemptive scheduler, low memory, fast to learn for a TypeScript team, excellent HTTP/2 and TLS in the standard library. Rust would be leaner but slower to staff and build. |
| Topology | **One process, one origin** (strangler proxy to legacy) | Many services behind nginx (das-02) | Fewer moving parts on a small device. das-02 remains a valid intermediate step, and the two combine (`deploy/nginx`). |
| Browser push | **WebSocket** | SSE, WebTransport, long-poll | Bidirectional (subscribe, resume, ping), binary frames for spectrum, universal browser support. Long-poll stays as a fallback. SSE lacks binary data; WebTransport is not yet reliable in embedded browsers or enterprise proxies. |
| Browser payloads | **JSON + one binary frame type** | Protobuf over WebSocket, gRPC-Web | JSON stays debuggable and compatible with the shipped UI. The spectrum, the only large payload, uses the binary DSPC frame (4 bytes/point instead of ~40). |
| Node ↔ Master | **Connect streaming (gRPC-compatible), protobuf contract** | MQTT, NATS, raw TCP, polling | One long-lived HTTP/2 stream per node with flow control; typed contract; the same handler can serve gRPC. MQTT is a good fit if a broker is already present (see das-00 options). |
| Change detection | **Canonical encoding + FNV-1a 64 hash, deadbands, deltas by field path** | Whole-state CRC only, timestamps | The hash detects divergence cheaply; field paths keep deltas small; deadbands make "changed" mean something to an engineer. |

## Dependencies

The PoC uses the **Go standard library only**, because the build environment could not
reach the Go module proxy. The replacements for production are drop-in at single seams:

| Here | Production option | Seam |
|---|---|---|
| `internal/ws` (RFC 6455 + permessage-deflate) | `github.com/coder/websocket` | `internal/hub` only (`Upgrade`, `WritePrepared`, `ReadMessage`) |
| `internal/ingest` Connect framing + JSON mapping | `connectrpc.com/connect` + `buf generate` (connect-go), which adds gRPC and gRPC-Web with binary protobuf on the same path | `ingest.Service.Handler()` |
| `internal/api/prom.go` | `github.com/prometheus/client_golang` | `/api/metrics?format=prom` |
| `internal/sysd` | `github.com/coreos/go-systemd/daemon` | `sysd.Notify`, `RunWatchdog` |

`interop/connect-es` checks today's implementation against the official Connect-ES
client generated from the same `.proto` files.

## Hardware seam

The analyzer is behind `spectrum.Hardware`:

```go
type Hardware interface {
    Sweep(ctx context.Context, p Params) ([]float32, error)
}
```

On the device this wraps the FPGA/DSP driver and returns calibrated dBm values, binary
end to end with no JSON. The simulator (`SimHardware`) sweeps in 250 ms and serialises
sweeps per physical analyzer.
