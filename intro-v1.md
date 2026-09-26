# DAS Master Node web backend: false "server is dead" alarms

**Status:** delivered on 2026-09-26 as 5 zip projects (PoC quality, verified from clean
unzips).

## Problem

The Spectrum Analyzer returns 50 001+ points. The Node.js backend on the embedded Master
Node parses and serialises them for **every poll** on its single event loop (~300 ms
per trace on the device). The heartbeat waits behind that work, the UI's 3 s timeout
fires, and the UI raises a false "server is dead" alarm. `volatile_data` from hundreds of
Remote Nodes is also re-serialised per request, and every report counts as a change.

## Deliverables

| Zip | Contents | Verified |
|---|---|---|
| das-00-architecture-and-contract | Executive README, OPTIONS (decision matrix), TARGET-ARCHITECTURE, ROADMAP, PRESENTATION (slides + demo script + Q&A), 8 ADRs. Contract: OpenAPI 3.1, AsyncAPI 3.1, protobuf (buf lint), DSPC binary frame, ingest protocol. `conformance/run.js` (18 checks incl. heartbeat SLO). Collected benchmark reports. | contract lint clean |
| das-01-hotfix-node | Backend-only patch for the shipped release: worker threads (nice 10), shared sweep sessions, incremental serialisation, ETag/304, opt-in `/api/spectrum/latest`, kill switch `DAS_MODE=legacy`, optional frontend patch | 39 tests, conformance 14 pass / 4 skip (no WebSocket) |
| das-02-gateway-multiservice | nginx single origin + core/telemetry/spectrum Node services on Unix sockets, WebSocket push, systemd isolation | 11 tests, conformance 18/18 |
| das-03-lts-go-edge | Go stdlib-only single binary: UI, REST, WebSocket push, Remote Node ingest (Connect/gRPC-compatible, deltas + FNV-1a state hash, RESYNC), strangler proxy (`-legacy`, `-delegate`, `-auth-check`), low-priority heavy threads, systemd notify/watchdog, Prometheus metrics | `go test -race`, conformance 18/18, Angular e2e 3/3, Connect-ES interop, arm64/armv7 under qemu |
| das-04-angular-client | Angular 21 zoneless client: capability discovery, WebSocket push with polling fallback, tolerant heartbeat, canvas spectrum, If-Match config | 13 unit tests, e2e 3/3 against 01/02/03 |

## Key results (same load; 1 core, CPU ×8 emulated, 10 % background load)

| | Shipped | 01 | 02 | 03 |
|---|---:|---:|---:|---:|
| Heartbeat p99 | 3 000 ms | 5.7 ms | 11.1 ms | 4.7 ms |
| False alarms in 30 s | 19 | 0 | 0 | 0 |
| Spectrum responses in 30 s | 74 | 120 | 108 | 408 |
| Memory peak | 211 MB | 239 MB | 286 MB | 43 MB |

Other results:

- **Ingest (03, 300 nodes, 60 s):** 0.88 MB instead of 21.2 MB (24×), with 0 resyncs.
- **Strangler steps in front of the shipped behaviour:** conformance 6/11/14/18 passed,
  0 failures at every step.

## Recommendation

1. Ship 01 to customers now.
2. Put 04's heartbeat hysteresis and capability discovery into the product UI.
3. Adopt 03 via strangler steps (step 1 = edge in front, step 2 = spectrum moved).
   Decision point after step 2, on real hardware.
4. 02 is the plan B if the team stays on Node.

## Decisions and constraints worth remembering

- The Go module proxy was blocked by the network policy in the build environment, so 03
  uses the standard library only. Production swap-ins: coder/websocket, connect-go,
  prometheus client.
- Revisions start at a random per-boot base (2^40..2^52), so a delta request from a
  previous boot always gets a snapshot. This applies to Go and to the Node store in 01/02.
- The boot time is derived from the uptime with a 10 s deadband (fixes spurious changes).
- The conformance gzip and ETag checks for volatile-data apply only when `volatileDelta`
  is advertised (the endpoint may be delegated to the legacy app).

## Open items for the next phase

- Measure 01 and 03 on a real Master Node: `bench/heartbeat-under-load.js` and
  `conformance/run.js` from a laptop.
- Agree the deadband values with the RF/operations team.
- Decide the legacy session-check endpoint for `-auth-check`.
- Implement `spectrum.Hardware` for the real analyzer driver.
- Node firmware agent: mTLS.
