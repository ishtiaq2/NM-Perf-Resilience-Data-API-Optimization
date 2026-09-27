# Roadmap

Durations are **indicative** for a team of 2–3 backend developers plus 1 frontend
developer. Check them against your release train. Every phase ends with measurements on
a real Master Node, using the same tools as the PoC.

```mermaid
gantt
    dateFormat  YYYY-MM-DD
    axisFormat  %b
    section Customers
    01 hotfix: hardening, field test, release   :a1, 2026-10-05, 4w
    section Frontend
    04 heartbeat hysteresis + capability discovery :b1, after a1, 4w
    section LTS (03)
    Go ramp-up + real analyzer driver seam     :c0, 2026-10-19, 4w
    Step 1-2 edge in front, spectrum on Go     :c1, after c0, 6w
    Step 3-4 telemetry + WebSocket push        :c2, after c1, 6w
    Step 6 node ingest (firmware + master)     :c3, after c2, 10w
    Step 7-8 configuration, retire Node        :c4, after c3, 8w
```

## Phase 0: hotfix for shipped systems (01), weeks 1–4

- **Scope:** port the hotfix into the product code base (`docs/INTEGRATION.md` in 01).
  Run `scripts/find-blocking-calls.sh` on the real code. Field-test on 2–3 customer
  systems.
- **Exit criteria:**
  - 0 false alarms in a 60 min soak with 4 analyzer traces on real hardware;
  - heartbeat p99 < 100 ms;
  - response bodies byte-identical (compat tests);
  - the `DAS_MODE=legacy` rollback tested.
- **Risks:**
  - Other blocking code in the product → the blocked-loop detector logs the URL;
    `find-blocking-calls.sh`.
  - Worker memory on small devices → `DAS_WORKER_HEAP_MB`, `MALLOC_ARENA_MAX=2`.

## Phase 1: frontend resilience (04), weeks 5–8

- **Scope:**
  - adopt `ConnectionMonitorService` (online/degraded/offline with hysteresis);
  - adopt capability discovery and the adaptive telemetry and spectrum transports;
  - canvas spectrum rendering.
- **Exit criteria:** the UI runs unchanged against the shipped backend, 01 and 03.
  Playwright end-to-end tests run against each in CI.

## Phase 2: Go edge in front, spectrum moved (03 steps 1–2), weeks 3–14

- **Scope:**
  - Go ramp-up. The PoC code is the curriculum: `internal/spectrum`, `internal/hub`.
  - Implement `spectrum.Hardware` for the real analyzer.
  - Package das-edge in the Yocto/Buildroot image, with the systemd unit.
  - Put the legacy app behind a Unix socket and enable `-auth-check`.
- **Decision point:** keep Go (continue) or switch to plan B (02). Base it on step 2
  measured on hardware and on team feedback.
- **Exit criteria:**
  - conformance 11/11 applicable checks;
  - heartbeat SLO on hardware;
  - memory < 64 MB for the edge;
  - rollback by flag tested.

## Phase 3: telemetry and push (03 steps 3–4), +6 weeks

- **Scope:**
  - `volatile_data` served by the edge;
  - WebSocket push enabled;
  - deadbands agreed with the RF/operations team (they are product decisions).
- **Exit criteria:** conformance 18/18; push-vs-poll measured with 20 dashboards on
  hardware.

## Phase 4: Remote Node ingest (03 step 6), +10 weeks

- **Scope:**
  - node firmware agent (reference: `internal/ingest.Agent`), using connect-go/grpc
    generated from `contract/proto`;
  - mTLS per node;
  - a mixed-fleet period in which old nodes keep their current reporting.
- **Exit criteria:**
  - resyncs < 1 per node per day;
  - hash mismatches 0 in steady state;
  - bytes on the fiber reduced ≥ 10× versus today, measured.

## Phase 5: configuration and retirement (03 steps 7–8), +8 weeks

- **Scope:** configuration served by the edge on the real configuration backend, then the
  remaining endpoints one by one under the conformance suite. The Node process is
  retired.
- **Exit criteria:**
  - no `legacy-proxy` requests in `/api/metrics` for 2 releases;
  - one binary on the device.

## Plan B: 02 (stay on Node)

If the decision point in phase 2 goes against Go, deploy 02 (nginx + three Node services)
in the next release instead. The frontend work (04) is identical. The ingest protocol can
be implemented in Node with connect-node from the same `.proto`. The Go edge can be
reconsidered later, behind the same nginx (`das-03/deploy/nginx/edge-behind-gateway.conf`).

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Team unfamiliar with Go | medium | medium | Small, readable PoC as the starting point; stdlib-only; pair with an experienced Go developer for 2 phases; plan B (02) |
| Module proxy blocked in the build environment (as in this PoC) | medium | low | Vendor modules (`go mod vendor`) or a local Athens/GOPROXY mirror; the PoC needs none |
| Real analyzer driver behaves differently from the simulator (timing, errors) | high | medium | `spectrum.Hardware` seam, sweep timeouts, error counters; test on hardware in phase 2 |
| Deadband values hide a real event | low | high | Deadbands well below alarm thresholds; alarms and status always pass through; agreed with RF/ops |
| Firmware update of hundreds of nodes takes long | high | low | Mixed fleet supported: legacy reporting keeps working |
| Customers with old browsers or proxies block WebSocket | medium | low | Automatic fallback to HTTP deltas (04, tested) |
| Memory growth under unusual load | low | medium | `GOMEMLIMIT` + `MemoryMax`, limits everywhere, soak tests with `/api/metrics` |

## Staffing and skills

- Backend: 2 developers on 03, 1 on hotfix/porting during phase 0. Go training:
  2 weeks with the PoC code.
- Frontend: 1 developer on 04 integration.
- Firmware: 1 developer for the node agent in phase 4.
- QA: conformance, benchmark and Playwright suites in CI, plus a hardware-in-the-loop
  bench with 1 Master Node and 2–3 Remote Nodes.
