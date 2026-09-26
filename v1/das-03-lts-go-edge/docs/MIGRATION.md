# Migrating to das-edge without a big bang

The edge is built for the **strangler-fig** pattern. It goes in front of the running
system, takes over one path at a time, and forwards everything else to the Node.js
backend. The browser keeps talking to one origin throughout, and every step can be undone
by changing a port or a flag.

## Where each option fits

```
 today            quick fix (01)          optional step (02)                  LTS (03)
 ─────            ──────────────          ──────────────────                  ────────
 Node app  ──►    Node app + workers ──►  nginx + Node services      ──►      das-edge (Go)
 (blocks)         (hotfix, same URLs)     (isolation, micro-cache)   │        + legacy Node for the rest
                                                                     └─► or: nginx + das-edge for heavy paths
```

Each of these steps passes the same conformance suite (`das-00/conformance`), so the
Angular client does not change. Its adaptive transport chooses WebSocket or polling
based on `/api/capabilities`.

## Steps

Each endpoint group moves with a flag. `-delegate` lists the groups the edge leaves to the
legacy backend (`volatile`, `spectrum`, `ws`), and `-config legacy` leaves configuration
there. `/api/capabilities` always describes whoever actually answers: for delegated
groups the edge relays what the legacy backend advertises, and a shipped release
advertises nothing. Clients therefore never opt into something the answering process
cannot do.

| Step | das-edge flags (plus `-legacy unix:/run/das/core-api.sock -web /opt/das-edge/ui -auth-check /api/session`) | Served by Go | Rollback |
|---|---|---|---|
| 0 | – (ship the **hotfix**, das-01, to customers now) | – | – |
| 1 | `-delegate volatile,spectrum,ws -config legacy` | UI files, heartbeat, capabilities | stop the edge, legacy back on :443 |
| 2 | `-delegate volatile,ws -config legacy` | + spectrum (the cause of the incident) | add `spectrum` back |
| 3 | `-delegate ws -config legacy` | + volatile_data (ETag, deltas, gzip) | add `volatile` back |
| 4 | `-config legacy` | + WebSocket push (telemetry, spectrum) | add `ws` back |
| 5 | + real analyzer driver behind `spectrum.Hardware` | + the analyzer itself (binary end to end) | previous build |
| 6 | + `-ingest 10.10.0.1:9090 -nodes 0` and node firmware with the ingest client | + volatile_data merge (deltas with checksums) | nodes keep their old reporting; step 5 flags |
| 7 | `-config local` with the real configuration store | + configuration | `-config legacy` |
| 8 | move the remaining endpoints one by one, then retire the Node process | everything | – |

`make strangler` measures steps 1–4 with the **shipped** release's behaviour as the legacy
backend (`das-01 --legacy`, which blocks its event loop on spectrum). Each step runs the
das-v1 conformance suite, including the heartbeat SLO under 6 spectrum pollers:

| Step | Conformance | Heartbeat p99 under spectrum load |
|---|---|---|
| 1: everything delegated | 6 passed, 0 failed, 12 skipped (legacy features not advertised) | 6.8 ms |
| 2: spectrum on the edge | 11 passed, 0 failed, 7 skipped | 7.2 ms |
| 3: + volatile_data | 14 passed, 0 failed, 4 skipped | 6.1 ms |
| 4: + WebSocket push | 18 passed, 0 failed, 0 skipped | 7.7 ms |

Step 1 already ends the **false** "server is dead" alarm, because the heartbeat no longer
waits behind the legacy event loop. The heartbeat still honestly reports
`services.legacy: degraded` while the legacy backend is busy. Step 2 removes the cause.

The legacy backend keeps running, and serving, until step 8. At every step the edge's
heartbeat shows `services.legacy`, and the legacy process can be restarted independently.

## Mixing with the das-02 gateway

If the gateway is deployed first, nginx stays the single origin and the edge replaces
only the heavy services. `deploy/nginx/edge-behind-gateway.conf` routes
`/api/spectrum*`, `/api/volatile-data` and `/api/ws/*` to the edge on a Unix socket.

```sh
das-edge -listen unix:/run/das/edge.sock -legacy unix:/run/das/core-api.sock -nodes 0 -ingest 10.10.0.1:9090
```

Headers to keep: `Host` (the edge compares WebSocket `Origin` with it) and
`Upgrade`/`Connection` on `/api/ws/`. The snippet sets both; das-02's conformance check
"same-origin browser accepted" catches a mistake here.

## What to measure at each step

Measure the same numbers every time, on a real Master Node and against the same scenario:

```sh
node bench/heartbeat-under-load.js --base https://<device> --insecure --duration 60 --browsers 4 --analyzers-per-browser 2
node conformance/run.js --base https://<device> --insecure
```

- heartbeat p99 and false alarms under spectrum load: must stay at 0 alarms;
- spectrum responses per second and bytes per response;
- RSS/PSS of the web processes and CPU of the device;
- ingest resyncs and hash mismatches (step 4 onwards).
