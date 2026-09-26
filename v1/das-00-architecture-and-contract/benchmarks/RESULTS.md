# Results from all projects

The HTML reports in this folder are self-contained; open them in a browser. The JSON
files hold the raw data and re-render with `node bench/render-report.js <file>.json` in
any project.

| File | Content |
|---|---|
| `01-legacy-vs-hotfix.html` | das-01 `npm run demo`: shipped behaviour vs hotfix |
| `02-gateway.html` | das-02 `bench/gateway-under-load.js`, with the 01 runs for comparison |
| `03-all-four-backends.html` | das-03 `make bench`, with the 01 and 02 runs: **the overview** |

## Test conditions (identical for every run)

- **Device emulation:**
  - the server pinned to **one core** (`taskset`), the load generator on another;
  - every heavy operation run 8× (`DAS_CPU_SLOWDOWN=8`) to approximate an embedded ARM
    core;
  - a process burning 10 % of the server core, standing in for the other daemons.
- **Load:**
  - 4 browsers, each limited to 6 connections per origin like a real browser;
  - per browser: 2 spectrum traces on the legacy endpoint (50 001 points), a dashboard
    poll every 2 s, a config read every 3 s, and a heartbeat every 1 s with the UI's
    3 s alarm timeout;
  - 30 s after a 3 s warm-up.
- **Hardware:** Intel Xeon @ 2.10 GHz (development VM), Linux, Node 22, Go 1.24.

These numbers compare designs under the same conditions. Repeat them on a real Master
Node before quoting absolute values: every project has the same `heartbeat-under-load.js`,
which runs from a laptop against a device.

## Heartbeat and responsiveness

| | Shipped release | 01 Hotfix | 02 Gateway | 03 Go edge |
|---|---:|---:|---:|---:|
| Heartbeat p50 / p99 | 2 031 / 3 000 ms | 1.3 / 5.7 ms | 1.8 / 11.1 ms | 0.8 / 4.7 ms |
| Heartbeats timed out | 48 of 125 | 0 of 120 | 0 of 120 | 0 of 120 |
| False "server dead" alarms (1 miss / 3 misses) | 19 / 3 | 0 / 0 | 0 / 0 | 0 / 0 |
| Config read p50 / p95 | 2 166 / 10 001 ms | 1.4 / 4.2 ms | 1.7 / 5.2 ms | 0.8 / 3.4 ms |
| Dashboard p50 / p95 | 2 128 / 10 001 ms (15 errors) | 28 / 50 ms | 1.2 / 58 ms | 1.0 / 210 ms |
| Dashboard data (304s) | 17.4 MB (0) | 5.2 MB (0) | 5.1 MB (3) | 3.3 MB (24) |

## Spectrum

| | Shipped release | 01 Hotfix | 02 Gateway | 03 Go edge |
|---|---:|---:|---:|---:|
| Responses in 30 s | 74 | 120 | 108 | **408** |
| Latency p50 / p95 | 3 394 / 8 631 ms | 2 151 / 2 291 ms | 2 385 / 2 609 ms | 602 / 798 ms |
| Updates per trace per minute | 18.5 | 30 | 27 | 102 |
| Hardware sweeps | 74 (one per request) | 60 (shared) | 54 (shared) | 207 (shared) |
| Data sent | 148 MB | 36 MB | 32 MB | 130 MB (3.6× the updates) |

The legacy endpoint is kept for compatibility and is still ~300 kB per response after
gzip. With the opt-in binary endpoint or WebSocket at screen width (1 600 points), a
response is 2–3 kB: a 99 % cut.

## Memory

| | Shipped release | 01 Hotfix | 02 Gateway | 03 Go edge |
|---|---:|---:|---:|---:|
| Peak | 211 MB RSS | 239 MB RSS | 286 MB PSS (nginx + 3 services) | **43 MB PSS** |

## Other experiments

| Project | Experiment | Result |
|---|---|---|
| 02 | Isolation: spectrum-service blocked for 5 s | heartbeat 3–5 ms throughout, `services.spectrum: degraded`, telemetry and config unaffected |
| 02 | Push vs poll, 20 dashboards, 30 s | 20 vs 299 requests, 190 vs 872 kB/s, data age p95 25 ms vs 2 722 ms |
| 03 | Remote Node ingest, 300 nodes, 60 s | 0.88 MB instead of 21.2 MB (24×, 40× in steady state), 0 resyncs, 0 hash mismatches, 1.4 s master CPU |
| 03 | Encoding on reniced threads vs plain goroutines | heartbeat p99 26.9 → 4.7 ms, spectrum responses 268 → 408 |
| 03 | Slow WebSocket client (throttled link), 300 revisions | 2 messages delivered (conflated), no queue growth |
| 03 | Strangler steps 1–4 in front of the shipped behaviour | conformance 6/11/14/18 passed, 0 failed; heartbeat p99 < 8 ms at every step |
| 03 | Telemetry change rate | 300 reports/s → ~11 published changes/s (field hygiene + deadbands) |
| 03 | ARM builds | arm64 7.3 MB, armv7 7.5 MB; run under qemu |

## Conformance (`conformance/run.js`)

| Backend | Result |
|---|---|
| 01 hotfix | 14 passed, 4 skipped (WebSocket push is not part of the hotfix) |
| 02 gateway | 18/18 |
| 03 Go edge | 18/18 over HTTP; 17/17 + SLO skipped over HTTPS/HTTP/2 |
| 04 Angular client | Playwright end-to-end 3/3 against 01 (via serve.mjs), 02 and 03 |
