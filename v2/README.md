# DAS Master Node web backend: false "server is dead" alarms, and the Rust/Go split

**Status:** delivered as 7 zip projects (PoC quality, verified from clean unzips).
das-00 to das-04 on 2026-09-26; das-05 (Rust) and das-06 (Go NOC) on 2026-09-27, together
with updates to das-00 (contract, recommendation) and das-03 (security fix).

## Problem

The Spectrum Analyzer returns 50 001+ points. The Node.js backend on the embedded Master
Node parses and serialises them for **every poll** on its single event loop (~300 ms
per trace on the device). The heartbeat waits behind that work, the UI's 3 s timeout
fires, and the UI raises a false "server is dead" alarm. `volatile_data` from hundreds of
Remote Nodes is also re-serialised per request, and every report counts as a change.

A later set of notes proposed **Rust for the device data plane**, **Go for the cloud NOC**,
and as the "safest path" a tiny compiled Engineering Tools microservice next to Node.js.
das-05 and das-06 build and measure that proposal.

## Deliverables

| Zip | Contents | Verified |
|---|---|---|
| das-00-architecture-and-contract | Executive README, OPTIONS (decision matrix + section 9 "safest path": Go 03 vs Rust 05), TARGET-ARCHITECTURE, ROADMAP, PRESENTATION, 8 ADRs. Contract: OpenAPI 3.1 v1.2.0 (incl. `/api/dtf`, `dtf` capability), AsyncAPI 3.1, protobuf, DSPC frame, ingest protocol, COMPATIBILITY (incl. das-noc.v1 as a separate contract). `conformance/run.js` (21 checks; 3 DTF checks run when `dtf` is advertised). | Redocly lint valid (1 pre-existing license warning) |
| das-01-hotfix-node | Backend-only patch for the shipped release: worker threads (nice 10), shared sweep sessions, incremental serialisation, ETag/304, kill switch `DAS_MODE=legacy` | 39 tests, conformance 14 pass / 4 skip |
| das-02-gateway-multiservice | nginx single origin + core/telemetry/spectrum Node services, WebSocket push, systemd isolation | 11 tests, conformance 18/18 |
| das-03-lts-go-edge | Go stdlib-only single binary: UI, REST, WebSocket push, Remote Node ingest, strangler proxy. **2026-09-27:** WebSocket frame-reader crash fixed (found by a new fuzz test), JS-compatible rounding ties | `go test`, conformance 18 pass / 3 skip (no dtf) |
| das-04-angular-client | Angular 21 zoneless client: capability discovery, push with polling fallback, tolerant heartbeat, canvas spectrum | 13 unit tests, e2e 3/3 |
| das-05-rust-engineering-tools | Rust (axum/tokio/rustfft) service: Spectrum Analyzer from raw FPGA/DSP buffers (DSPR/DS11), legacy JSON byte-identical with Node, DSPC frames, WebSocket push, **Distance-to-Fault** (window, IFFT, loss compensation, OS-CFAR). Setups: inside the 02 gateway, in front of the unchanged Node app, or behind it | 29 unit + 7 e2e tests, clippy clean; conformance 21/21 in the gateway; DTF 0 missed / 0 phantom |
| das-06-go-noc-controller | Go stdlib-only NOC: `das-noc.v1` outbound WebSocket per Master Unit (deltas + canonical hash, alarm seq/ack/outbox, detail on demand), admission control, tenant-scoped REST and dashboard push, NOC page, live token revocation, Prometheus, pprof. Device side: `agent/noc-agent.js` sidecar. Dockerfile, k8s manifests, systemd units. | 27 tests + 2 fuzz targets under `-race`; agent interop with das-01 PASS; 5 000- and 10 000-site scale runs |

## Key results

Device, same load (1 core, CPU ×8 emulated, 10 % background load):

| | Shipped | 01 | 02 | 03 | shipped + 05 |
|---|---:|---:|---:|---:|---:|
| Heartbeat p99 | 3 000 ms | 5.7 ms | 11.1 ms | 4.7 ms | 12–58 ms (front), 8–10 ms (Node in front) |
| False alarms | 19 in 30 s | 0 | 0 | 0 | 0 in 6 runs |
| Memory | 211 MB | 239 MB | 286 MB | 43 MB | 14–17 MB for the Rust service |

NOC (das-06), one process on one core:

- 5 000 simulated Master Units + 26 dashboards: 13.8 % CPU, 127 MB RSS, alarm to dashboard
  p50 15 ms / p99 51 ms; 20 000-alarm storm p99 674 ms (501 ms in an earlier run), 0 events
  lost or out of order; restart under load: fleet back in 32 s; 0 differences NOC vs
  devices in 3 full checks; GC pause p99 ≤ 0.2 ms.
- 10 000 sites: 22 % CPU, 232 MB RSS, p99 53 ms steady; 0 differences.

## Recommendation (as of 2026-09-27)

1. Ship 01 to customers now.
2. Put 04's heartbeat hysteresis and capability discovery into the product UI.
3. Next device release, the "safest path": **05 in the 02 gateway if the team adopts Rust
   for low-level device code anyway**; otherwise **03 steps 1–2** (same outcome in Go,
   shorter learning curve; port DTF).
4. LTS: 03 via strangler steps (highest decision-matrix score).
5. Cloud: 06, independent of the device path. Add SSO before customer access (user tokens
   do not expire).

## Claims in the Rust/Go notes that were corrected (with sources in WHY-RUST / WHY-GO)

- Rust is not "the only memory-safe language fast enough": NSA/CISA (June 2025) list Ada,
  C#, Delphi/Object Pascal, Go, Java, Python, Ruby, Rust, Swift.
- Go GC pauses are not ~50 ms: Go's objective is 500 µs/cycle; measured p99 0.5 ms on the
  device (03) and ≤ 0.33 ms in the NOC.
- The CISA/FBI "Product Security Bad Practices" is non-binding guidance (memory-safety
  roadmap by 1 Jan 2026 for products in memory-unsafe languages), not a mandate.
- Vendor claims (Ericsson/Cisco/Ubiquiti) were not verified and not used.

## Decisions and constraints worth remembering

- The Go module proxy and Rust's ARM std downloads were blocked in the build environment:
  03 and 06 use the standard library only (own WebSocket code, now fuzzed); das-05's ARM
  builds are documented but not verified. Go cross-compiles to arm64 without downloads.
- Hand-written WebSocket readers must refuse 64-bit lengths with the top bit set (a
  negative int64 size crashed both 03 and 06 before the fix). Production: use a maintained
  WebSocket library behind the same API.
- Revisions start at a random per-boot base (2^40..2^52), so a delta request from a
  previous boot always gets a snapshot.
- das-noc.v1 canonical hash: sorted keys, JSON.stringify strings and numbers, FNV-1a 64;
  JS and Go implementations agree (test vectors in das-06 docs/PROTOCOL.md).
- JS `Math.round` ties differ from Rust/Go `round`: `js_round`/`jsRound` keep outputs
  byte-identical with Node.

## Open items for the next phase

- Measure 01, 03 and 05 on a real Master Node; build and run 05 for ARM.
- Implement the real analyzer driver adapter (05 `Analyzer` trait / 03 `spectrum.Hardware`).
- NOC: SSO for users, a soak test on real infrastructure, TLS/mTLS at the ingress, an
  agent build for the Node version actually shipped on the Master Units.
- Agree deadband values with the RF/operations team; decide the legacy session-check
  endpoint for `-auth-check`.
