# Presenting this to the team and management

## The story in three sentences

1. The "server is dead" alarm is false: the server is busy serialising spectrum data on
   its only thread, and the heartbeat waits behind it.
2. A backend-only hotfix removes it for customers **now** (19 → 0 false alarms in the
   same test).
3. The long-term fix is a small Go edge server that pushes only what changed. It is
   introduced step by step in front of today's software, so every step is reversible
   and the browser always sees one server.

## Slide outline (about 12 slides, 20 minutes plus demo)

| # | Slide | Key message | Material |
|---|---|---|---|
| 1 | Title | "From false alarms to a fleet-scale web backend" | – |
| 2 | Customer impact | Engineers see "server dead" when they use the Spectrum Analyzer; trust erodes | support tickets, symptom description (01 ROOT-CAUSE) |
| 3 | Root cause | One thread; work per request; 2 MB JSON per poll; the embedded CPU is 8× slower than a laptop | 01 `docs/ROOT-CAUSE.md` sequence diagram |
| 4 | Evidence | Reproduced: 19 false alarms in 30 s with 4 engineers | 01 `bench/results/latest.html` (legacy panel) |
| 5 | Principles | Work per change, push what changed, priorities, one origin + contract, strangler | TARGET-ARCHITECTURE principles |
| 6 | Hotfix (01) | Backend-only, same URLs/bytes, kill switch: 0 alarms, p99 5.7 ms | 01 README results table |
| 7 | Options we evaluated | Your 5 ideas + gRPC + checksum: what we took and what we did not | README "Your ideas" table, OPTIONS matrix |
| 8 | Target architecture | Go edge: UI + API + push + node ingest in one 7 MB binary | TARGET-ARCHITECTURE component diagram |
| 9 | Numbers | Legacy vs 01 vs 02 vs 03 on the same emulated device | 03 `bench/results/latest.html`, table in README |
| 10 | Remote Nodes | Deltas + state hash: 24× fewer bytes on the fiber, self-healing RESYNC | 03 `docs/INGEST.md` |
| 11 | Migration | Strangler steps, one flag each, measured against shipped behaviour | 03 `docs/MIGRATION.md` table |
| 12 | Roadmap, risks, ask | Phases, decision point after step 2, plan B (02); ask: approve 01 rollout + phase 2 staffing | ROADMAP |

A slide deck with these slides can be generated from this outline.

## Live demo script (about 12 minutes)

Preparation (the day before): unzip all projects side by side, then run:

- `npm ci` in 01, 02 and 04;
- `make build` in 03;
- `npx ng build` in 04;
- one run of each demo, so that the HTML reports exist as a fallback.

Requirements: Linux or WSL with Node 22+, Go 1.24+, nginx, and 2+ CPU cores.

### 1. Reproduce the incident and the hotfix (3 min, 01)

```sh
cd das-01-hotfix-node && npm run demo     # ~75 s; legacy vs hotfix under identical load
```

Open `bench/results/latest.html`. In the legacy panel, red crosses mark heartbeats that
timed out, each one a "server is dead" pop-up. The hotfix panel is flat at ~1 ms. Point
at the table: config reads went from 10 s timeouts to 4 ms.

### 2. Isolation: a bug in one service does not take down the UI (2 min, 02)

```sh
cd das-02-gateway-multiservice && sh scripts/run-local.sh &    # nginx + 3 services
npm run demo:isolation                                         # blocks spectrum-service for 5 s
```

While spectrum-service is blocked, the heartbeat stays at 3–5 ms and reports
`services.spectrum: degraded`; telemetry and configuration keep working.

### 3. The LTS edge (5 min, 03)

```sh
cd das-03-lts-go-edge
./bin/das-edge -web ../das-04-angular-client/dist/das-04-angular-client/browser -log text
```

1. Open http://localhost:8080. It is the Angular UI, served by one 7 MB binary.
2. Open the dashboard and point out the rows highlighting as deltas arrive.
3. Open the spectrum page and point out that each update is a 2–3 kB binary frame
   (decimated to the canvas width) instead of 2 MB of JSON.
4. Open `/api/metrics` and show the telemetry revisions and WebSocket counters.
5. Stop the edge, then show the Remote Node protocol:

```sh
make demo-ingest            # 300 simulated nodes stream deltas with checksums
```

Point at `savedFactor` in the output (≈ 11× after 15 s, 24× after 60 s) and at
`resyncs: 0`.

```sh
make strangler              # the migration, measured: conformance at every step
```

### 4. The UI behaves well under any backend (2 min, 04)

With the edge running, block the network in DevTools (offline) for 5 s. The status goes
"degraded", then "offline" only after 3 misses **and** 15 s. After reconnecting,
telemetry resumes from the last revision with no full reload. On the node page, press
"Simulate a colleague's edit" and then save: you get a clear conflict (412), not a
silent overwrite.

**Fallback if something fails live:** the pre-generated reports:

- 01: `bench/results/latest.html`
- 02: `bench/results/latest.html`
- 03: `bench/results/latest.html` and `docs/screenshots/`

## Questions to expect

**Why not just optimise the Node code?**
We did (01), and it works. But it is still one thread: the next blocking call anywhere in
the code base brings the alarm back. The LTS design makes that class of problem
impossible, with priorities and limits, and cuts memory from ~240 MB to ~43 MB.

**Why Go and not Rust or C++?**
All three remove the root cause. Go gets a TypeScript team productive fastest, builds one
static binary for ARM in seconds, and uses 43 MB here. Rust would be leaner and slower to
staff. Scores are in OPTIONS.md.

**Is Go proven on embedded Linux?**
Yes. containerd, Docker, the Kubernetes node agent, Tailscale, Telegraf and many IoT
gateway daemons are Go programs running on ARM Linux. The binaries here were run under
qemu for arm64 and armv7.

**Do customers need a new frontend for the hotfix?**
No. URLs, response bytes and authentication are unchanged. The optional frontend patch
only adds heartbeat hysteresis.

**How do we roll back?**
- 01: `DAS_MODE=legacy` and a restart.
- 03: every migration step is one flag (`-delegate`); step 1 is undone by stopping the
  edge.

**Does the edge bypass our login?**
No. `-auth-check` asks the existing backend whether the session is valid (like nginx
`auth_request`) and caches the answer for 30 s. Proxied paths are authenticated by the
existing backend itself.

**Why WebSocket rather than gRPC in the browser?**
gRPC in browsers needs gRPC-Web and a translating proxy, and it has no bidirectional
streaming. WebSocket carries both JSON deltas and binary spectrum frames, works through
the same origin, and falls back to HTTP. gRPC-style contracts are used where they shine:
Remote Node → Master.

**What does "checksum" buy us?**
A node sends only what changed, as a delta against a state the master confirmed. The
hash proves both sides agree. If they ever diverge (restart, lost message, bug), the
master asks for one full report and both are consistent again. Unverified data is never
shown.

**What does it cost?**
About 8 months of 2–3 developers for the full LTS path (ROADMAP, indicative), with
value delivered after each phase: the hotfix in weeks, and the root cause removed on new
releases after phase 2.

**What if the real analyzer or nodes behave differently from the simulators?**
The analyzer is behind a single Go interface (`spectrum.Hardware`), and the node agent is
a reference implementation. Both are validated on hardware in phases 2 and 4, with the
same benchmark and conformance tools used in this PoC.
