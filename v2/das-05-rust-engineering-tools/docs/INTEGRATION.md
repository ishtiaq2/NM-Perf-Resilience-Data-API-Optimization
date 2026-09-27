# Integration: putting das-engtools next to the Node.js app

Three setups share one binary and one API. Pick by what the device already has.

| | A. gateway (recommended) | B. front mode | C. Node.js stays in front |
|---|---|---|---|
| Who owns the port | nginx (das-02) | das-engtools | the Node.js app |
| HTTPS | nginx (HTTP/2 too) | none: plain HTTP only | Node.js, as today |
| Change to the Node.js app | none | its port (config) | mount one small module |
| Change to the frontend | none (DTF screen: new) | none | none |
| Measured here | conformance **21/21** through nginx | conformance 15 passed / 6 skipped (shipped app); **0 timeouts in 3/3 load runs** | conformance 15 passed / 6 skipped; **0 timeouts in 3/3 load runs** |
| Script | `make drop-in` | `make front` | `sh scripts/node-in-front.sh` |

The skipped checks are features the shipped app itself lacks (telemetry delta and push).
They are not skipped because of das-engtools. Behind the das-01 hotfix, front mode passes
18 and skips 3.

## A. Inside the das-02 gateway

In the das-02 gateway, nginx is the single origin, and the Node.js services sit behind it on
Unix sockets. `das-engtools` **replaces the Node.js spectrum service on the same socket**.
The existing `/api/spectrum` and `/api/ws/spectrum` locations therefore keep working
unchanged. nginx gets two more locations (`deploy/nginx/das02-engtools-locations.conf`):

- `location /api/dtf` for distance-to-fault;
- `location = /api/capabilities` → das-engtools, which asks core-api for its own features
  (`--legacy unix:/run/das/core-api.sock`) and merges them. The frontend discovers `dtf`
  without a core-api change. If das-engtools is down, an `error_page` falls back to
  core-api's answer.

Install on the device, as root, with the ARM binary built as in [BUILD.md](BUILD.md):

```sh
sh deploy/install-device.sh gateway ./das-engtools-linux-arm64
```

The script:

1. installs `/opt/das-engtools/das-engtools`, `/etc/default/das-engtools` (profile A) and
   the unit;
2. adds `das.target.d/engtools.conf`, so the stack target starts das-engtools;
3. appends the two nginx locations (keeping a `.before-engtools` copy) and runs `nginx -t`;
4. disables `das-spectrum.service`, enables `das-engtools.service`
   (`Conflicts=das-spectrum.service`), and reloads nginx.

Verify from a laptop: `node conformance/run.js --base https://<master-unit> --insecure`.

**Rollback:** `systemctl disable --now das-engtools && systemctl enable --now das-spectrum`,
then restore `das-locations.conf.before-engtools` and `nginx -s reload`. The two services use
the same socket, so nothing else changes.

`scripts/drop-in-das02.sh` performs the same steps against a local das-02 stack
(`../das-02-gateway-multiservice`, `run-local.sh`) and runs the conformance suite through nginx:

```
capabilities (merged): … "configOptimisticLocking":true,"dtf":true,…,"volatileDelta":true,"wsTelemetry":true …
distance-to-fault, node 3/1:  1139 points, range 75.056 m, resolution 0.066 m
       0.35 m  RL  30.5 dB  VSWR 1.06
       1.78 m  RL  26.0 dB  VSWR 1.11
      37.26 m  RL  22.3 dB  VSWR 1.17
21 passed, 0 failed, 0 skipped
```

## B. Front mode (no gateway on the device)

`das-engtools` takes the public HTTP port, serves the engineering tools, and forwards
**everything else** to the unchanged Node.js app. That includes WebSocket upgrades and
request bodies (streamed). The browser's `Host` header is kept, and `X-Forwarded-For`,
`-Host` and `-Proto` are added.

1. Move the Node.js app to a loopback port. This is one line in its service file, for
   example `Environment=PORT=8081 HOST=127.0.0.1`; use whatever variables the real app reads.
   A Unix socket also works: `DAS_LEGACY=unix:/run/das/core-api.sock`.
2. `sh deploy/install-device.sh front ./das-engtools-linux-arm64`. This selects profile B:
   `DAS_LISTEN=0.0.0.0:80`, `DAS_LEGACY=http://127.0.0.1:8081`.
3. `systemctl enable --now das-engtools`.

**Limits.** Front mode speaks **plain HTTP**. If the product serves HTTPS from Node.js
today, use A or C instead. Adding TLS would bring rustls and its C/assembly crypto
dependencies into a binary that is currently pure Rust. Forwarded requests time out after
30 s (`504`). A Node app that is down gives `502` for its routes, while spectrum and DTF
keep working.

**Rollback:** stop das-engtools, and put the Node.js app back on port 80.

Measured with `bench/front-under-load.js`, the standard load, the shipped app behind the
front, one shared core, and ×8 CPU emulation, over three runs: heartbeat p99 12, 40 and 58 ms;
**0 timeouts and 0 false alarms**; about 150 spectrum updates per minute per trace. The
remaining tens of milliseconds are the shipped app's own synchronous `volatile_data`
serialisation. It runs every 2 s and shows up when a heartbeat lands just behind it. Behind
the das-01 hotfix, the heartbeat p99 is 9–18 ms.

## C. The Node.js app stays in front

This is for a product whose Node.js server terminates HTTPS itself and has no nginx. The
Node app keeps the port and **pipes** the engineering routes to das-engtools on a Unix
socket. It moves bytes only (no JSON parsing, no buffering), so the event loop stays free.
The module is `examples/node-in-front/engtools-forward.js` (ES2019, no dependencies):

```js
var engtools = require('./engtools-forward')({ socketPath: '/run/das/engtools.sock' });
server.on('request', function (req, res) { if (!engtools.handle(req, res)) app(req, res); });
server.on('upgrade', function (req, socket, head) { if (!engtools.upgrade(req, socket, head)) socket.destroy(); });
// Express: app.use(function (req, res, next) { if (!engtools.handle(req, res)) next(); });  (before body parsers)
```

Run das-engtools **without** `--legacy` in this setup: `--listen unix:/run/das/engtools.sock`.
The module forwards `/api/capabilities` too, as long as the app has none of its own.
`examples/node-in-front/demo-das01.js` mounts it on the shipped app (das-01 `--legacy`);
that one mount is the whole change.

Measured with the same benchmark (`--topology node-front`) over three runs: heartbeat p99
7.8–10.3 ms, 0 timeouts, about 145 spectrum updates per minute per trace. Node.js peaks
about 10–20 MB higher than behind the Rust front, because it now pipes the 2 MB responses. **Caveat:** the ×8 CPU emulation does not
slow down Node's piping, so this setup is slightly flattered compared with A and B. On
the device, test it with `DAS_CPU_SLOWDOWN=1` and real clients.

## The frontend

- **Spectrum analyzer**: nothing changes. `/api/spectrum` keeps the shipped shape and
  semantics. A frontend that reads `/api/capabilities` (as the das-04 Angular client does)
  switches itself to the spectrum WebSocket, or to `/api/spectrum/latest` with binary frames
  and decimation when WebSockets are blocked. That is 100 KB instead of 2 MB per update, or
  about 2 KB at screen width.
- **Distance-to-fault** is new. Show it when `features.dtf` is true. Poll
  `GET /api/dtf?nodeId=…&port=…&maxDistanceM=<feeder length × 1.2>` with `If-None-Match`
  and `waitMs=5000`. Plot `returnLossDb[j]` at `startM + j × binM` (return loss, inverted
  axis), draw `thresholdDb` and the noise-floor line, and list `events`, marking
  `fault: true`. [DTF.md](DTF.md) has the fields.
- Recommended UI defaults: window `hann` (offer `kaiser` for "small fault next to the
  antenna"); a velocity factor and cable loss per feeder type from the site database.

## What to watch after rollout

- `/internal/metrics` through the gateway's own scrape, or `?format=prom`:
  - `dsp.rejected` should stay 0; otherwise raise `DAS_DSP_QUEUE` or reduce `points`;
  - `spectrum.errors` and `dtf.errors` count hardware failures;
  - `proxy.failures` counts front mode requests the Node app could not answer;
  - `process.rssMB`.
- Heartbeat latency as the UI sees it. The das-04 client's connection monitor shows
  online / degraded / offline instead of a single-miss "dead" alarm. The shipped UI's
  alarm threshold is unchanged.
- The journal: `journalctl -u das-engtools`. It contains JSON lines, one per event;
  warnings are proxy errors and watchdog check failures.
