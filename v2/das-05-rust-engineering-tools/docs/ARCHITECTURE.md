# Architecture

## The problem it removes

The shipped backend is one Node.js process. When an engineer opens the spectrum analyzer,
the process parses the analyzer output, builds a JSON array of 50 001 `{frequency, power}`
objects (about 2 MB) and serialises it, all on the event loop. During that time nothing else
runs, including `GET /api/heartbeat`. The UI times out after 3 s and shows "server is dead"
although the server is only busy. With four engineers on the tool, 48 of 125 heartbeats
timed out in the standard benchmark.

`das-engtools` moves that work into a separate process whose design makes the stall
impossible, not merely less likely:

1. **The heavy work never runs on a request thread.** Parsing raw FPGA buffers, FFTs, JSON
   and DSPC encoding and gzip all run on dedicated **DSP threads** at a lower scheduling
   priority (nice +10). The async runtime threads only move bytes.
2. **The heavy work is done once, not once per client.** One hardware measurement serves
   every client that watches the same configuration (single-flight), and every response
   format is encoded once per sweep and then shared. Ten engineers on one trace cost one
   sweep.
3. **Overload is refused, not queued forever.** The DSP queue is bounded. When it is full,
   requests get `503` with `Retry-After` in microseconds instead of joining an ever-growing
   backlog.
4. **The Node.js app is no longer on that path.** Spectrum and DTF traffic go to
   `das-engtools`. The Node app keeps the heartbeat, login, configuration and
   `volatile_data`, which are all small and fast.

## Processes and threads

```
das-engtools (one process, ~15-20 MB)
 ├─ main thread           signal handling, then idle
 ├─ runtime threads (1-2) tokio: accept, HTTP/1.1, WebSocket, proxy to Node.js, timers
 │                          never parse, encode or compress anything large
 └─ DSP threads (N)       nice +10, bounded queue (DAS_DSP_QUEUE)
                            parse DSPR/DS11 buffers, FFT, encode JSON/DSPC, gzip
```

- Runtime threads: `DAS_RUNTIME_THREADS`, default the number of CPUs, at most 2.
- DSP threads: `DAS_DSP_WORKERS`, default CPUs − 1, at least 1. The kernel scheduler, not
  the application, keeps them from delaying the runtime threads. On a single-core Master
  Unit, a nice +10 thread gets roughly 1/10 of the share of a nice 0 thread that wants the
  CPU (the CFS weights 110 vs 1024), so an accept or a proxy hop is scheduled promptly even
  during a 200 001-point encode.
- The Node.js app runs in its own process at its usual priority.

## Measurement sessions (single-flight)

`session.rs` is generic over a `Measure` (spectrum sweep or reflection sweep). There is one
session per configuration key, for example `node:port:start:stop:points`:

- While anyone watches it (HTTP polls, long-polls, WebSocket subscribers), the session runs
  the hardware measurement in a loop. It parses each result on the DSP pool and publishes
  the result through a `tokio::sync::watch` channel.
- A `watch` channel holds only the newest value, and that gives **conflation for free**. A
  slow WebSocket client that is still receiving frame *n* skips straight to the newest
  sweep. It costs O(1) memory and never delays anyone else.
- When no one has asked for `DAS_SPECTRUM_IDLE_MS` (15 s), the session stops sweeping. The
  analyzer hardware is left alone.
- `DAS_SPECTRUM_MAX_SESSIONS` limits concurrent configurations. Idle sessions are evicted
  first. If all sessions are busy, the answer is `503 TOO_MANY_SESSIONS` with
  `Retry-After: 2`.
- `DAS_SPECTRUM_MIN_INTERVAL_MS` can space sweeps to protect the hardware or the fiber
  management channel.
- The session waits for requests in three ways:
  - `next()`: the first result finished **after** the request arrived. These are the
    shipped `/api/spectrum` semantics.
  - `latest_or_next()`: the newest result, or the first one if none exists yet.
  - `after(id)`: long-poll for a result newer than the one the client has.

## Response variants, encoded once

A sweep (`spectrum.rs`) holds its calibrated power values once (`Arc<[f32]>`). Each response
variant is identified by (kind, maxPoints, gzip), where kind is legacy JSON, compact JSON,
DSPC i16 or DSPC f32. The first request for a variant builds it on the DSP pool
(`OnceCell::get_or_try_init`). Concurrent requests for the same variant wait for that one
build, and every later request gets the same `Bytes` (zero-copy, reference counted).

The ETag of a variant is `"sp-<boot id>-<session hash>-<sweep id>-<kind>-<maxPoints>[-gz]"`.
The boot id is random per process start, so an ETag from before a restart can never match a
different sweep after it. `If-None-Match` on `/api/spectrum/latest` returns `304` without
building anything.

Formats:

- **Legacy JSON**, the shipped frontend's shape. It is byte-identical to the Node.js
  implementation; `bench/dsp-compare.js` checks this, including JavaScript's rounding of
  `-x.xx5` values.
- **Compact JSON**: an implicit frequency axis (`startHz`, `stepHz`) and `powerDbm[]`, about
  7 bytes per point.
- **DSPC**: a 48-byte header and i16 centi-dBm or f32 values. It is byte-identical with the
  Node.js and Go encoders (fixtures in the tests).
- **Peak-hold decimation** (`maxPoints`, normalised to a multiple of 64). Each output point
  is the maximum of its bucket, so a narrow spur survives a 50× reduction.

## Distance-to-fault

This follows the same pattern: the reflection sweep is acquired by the session, then parsed
and processed on the DSP pool. The profile's JSON is encoded once (`OnceCell`) and served to
every client. [DTF.md](DTF.md) describes the signal processing.

## Integration modes

| | front mode | sidecar mode |
|---|---|---|
| flags | `--legacy http://127.0.0.1:8081` (or `unix:`) | `--sidecar` (plus `--legacy` for capabilities) |
| owns the public port | yes (HTTP) | no, a gateway routes to it |
| paths it does not implement | forwarded to the Node.js app, WebSockets included | 404 |
| `/api/heartbeat` | forwarded (the Node app answers, as today) | answered only when standalone |
| `/api/capabilities` | the Node app's features + engineering tools | the same, when `--legacy` is given |

Front-mode proxying (`http/proxy.rs`):

- The browser's `Host` is kept, so the Node app's same-origin checks keep working.
- It adds `X-Forwarded-Host`, `X-Forwarded-Proto` and `X-Forwarded-For`, and strips
  hop-by-hop headers.
- Bodies are streamed, never buffered.
- TCP connections to the Node app are pooled and keep-alive. Unix-socket connections are
  opened per request, which costs microseconds.
- If the Node app is unreachable, the answer is `502 UPSTREAM_UNAVAILABLE`. If it does not
  answer within 30 s, the answer is `504`.
- WebSocket upgrades: after the Node app answers `101`, bytes are copied both ways until
  either side closes.

The capabilities merge calls the Node app's `/api/capabilities` with a 1 s timeout and caches
the answer for 5 s. A shipped release without that endpoint (404) simply contributes no
features. Limits merge the same way.

## Failure behaviour

| Situation | What happens |
|---|---|
| Hardware measurement fails or times out | The session records the error (`lastError` in `/api/engineering/status`) and retries after 500 ms for as long as someone is watching. A request that gets no result within `DAS_SWEEP_WAIT_MS` (10 s) receives `503 SWEEP_TIMEOUT` + `Retry-After: 1`. A legacy `/api/spectrum` poll gets the previous sweep if there is one: better stale than an error for the shipped UI. |
| DSP queue full | `503` + `Retry-After: 1`, counted in `/internal/metrics` (`dsp.rejected`). |
| Too many configurations | `503 TOO_MANY_SESSIONS` + `Retry-After: 2`. |
| Too many WebSocket clients | `503` before the upgrade (`DAS_MAX_WS_CLIENTS`). |
| A DSP job panics | The panic is caught on the DSP thread and re-raised in the requesting task. That one request fails, and the pool and process continue. |
| Process wedged | The systemd watchdog is fed only while a real `GET /internal/health` through the listener succeeds, so a wedged process is restarted within `WatchdogSec`. |
| Node.js app down (front mode) | Engineering tools keep working. Everything else returns `502` until the Node app is back. |
| `das-engtools` down (gateway mode) | nginx returns `502` for spectrum and DTF only. Capabilities fall back to core-api's answer (`error_page` in the nginx snippet), and the heartbeat is unaffected. |

## Observability

- Structured JSON logs (`DAS_LOG_FORMAT=json`, `RUST_LOG=info`). Plain text is available for
  development.
- `GET /internal/metrics`: requests, bytes and 304s per route; the DSP pool (workers,
  queued, jobs, busy seconds, rejected); sessions; WebSocket clients and frames; proxy
  requests and failures; and process RSS, threads and CPU seconds.
- `?format=prom` gives the Prometheus text format (`das_http_requests_total{route=…}`,
  `das_engtools_*`).
- `GET /api/engineering/status`: the same session and DSP view, for an engineering page in
  the UI.
