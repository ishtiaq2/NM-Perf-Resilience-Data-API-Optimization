# Integrating the hotfix into the real backend

Estimated effort: 2-4 developer days plus device testing. The runtime code is
ES2019 / CommonJS with **zero npm dependencies**, so it runs on the Node version
already on the device (12.16 or newer).

## 0. Confirm the blockers in your code base

```sh
sh scripts/find-blocking-calls.sh /path/to/backend      # static scan
node --cpu-prof src/index.js                             # then use the analyzer; open the .cpuprofile in Chrome DevTools
```

Expect the spectrum handler (parse / map / stringify) and the volatile_data
serialisation at the top of the profile. Fix anything else the scan finds in
request handlers, especially synchronous child processes, sync file writes and
large `console.log` calls.

## 1. Copy the modules

Copy `src/shared/` into the backend (e.g. `lib/hotfix/`). You need:

| File | Purpose |
|---|---|
| `worker-pool.js`, `spectrum-worker.js` | worker threads, bounded queue, timeouts, crash recovery, per-thread nice |
| `spectrum-service.js` | sweep sessions: one producer per analyzer configuration, shared by all clients |
| `volatile-store.js` | incremental serialisation, revisions, ETag, deltas |
| `formats.js` | **your** raw-to-legacy transformation (see step 2) |
| `spectrum-frame.js`, `decimate.js` | binary frame and peak-detector decimation (new endpoint) |
| `http-util.js`, `loop-monitor.js`, `config-store.js`, `emulation.js` | helpers |

`spectrum-sim.js`, `node-sim.js`, `prng.js` and `router.js` are only for the PoC.

## 2. Move (don't rewrite) your transformation into `formats.js`

The worker calls two functions:

* `canonicalFromRaw(raw)` turns the driver output into `{startHz, stepHz, count, power: Float32Array}`.
* `legacySpectrumBody(raw, meta)` builds **exactly** the object the current handler sends.

Paste the body of your current handler's transformation into `legacySpectrumBody`
unchanged. Then record one real sweep on a device and write a golden test:

```js
// test/golden.test.js
const raw = fs.readFileSync('fixtures/sweep-raw.bin');        // captured driver output
const expected = fs.readFileSync('fixtures/sweep-legacy.json', 'utf8'); // captured legacy response body
// ... run 'spectrum.parse' through the WorkerPool (see test/compat.test.js) and assert byte equality
```

`test/compat.test.js` shows the pattern with simulated data.

If your driver returns parsed objects (not bytes), or the driver call is synchronous,
call it **inside** the worker. Workers can use `require()`, native add-ons and
async I/O. Add a task to `spectrum-worker.js`, e.g. `'spectrum.acquireAndParse'`.

## 3. Plug in the hardware and telemetry

```js
const hardware = {
  // Must not block: async I/O only. Return the raw bytes of one sweep.
  sweep: (params) => driver.sweepAsync(params.nodeId, params.port, params.startHz, params.stopHz, params.points)
};

// Wherever Remote Node data is merged into volatile_data today:
remoteLink.on('nodeReport', (report) => store.ingest(report));   // or emit 'report' on ctx.telemetry
```

Keep your existing in-memory `volatile_data` object for other code if you need it.
The store keeps its own serialised copy per node.

## 4. Register the routes: heartbeat first

```js
// Express example: see examples/express-app.js (tested).
hotfix.register(router, { cfg, bootId, hardware, telemetry, loop, log });
app.use(bodyParser.json());   // existing middleware AFTER the hotfix routes
```

* `/api/heartbeat` must be registered before any middleware that can be slow
  (auth that hits disk, body parsing, request logging of bodies).
* Replace the existing `/api/spectrum` and `/api/volatile-data` handlers. The
  URLs and bodies stay the same.
* Express `compression()` is fine: it skips responses that already carry `Content-Encoding`.

## 5. Configure

All settings are environment variables; see `src/config.js`,
`deploy/systemd/das-backend.service.d/10-hotfix.conf` and
`deploy/sysvinit/das-backend.env`. The important ones:

| Variable | Default | Notes |
|---|---|---|
| `DAS_MODE` | `hotfix` | `legacy` = kill switch back to the old code path, no redeploy |
| `DAS_WORKERS` | auto | `min(2, cores - 1)`, at least 1 |
| `DAS_WORKER_NICE` | 10 | Linux only; lowers the worker threads, not the event loop |
| `DAS_WORKER_HEAP_MB` | 192 | V8 old-space cap per worker |
| `DAS_GZIP_LEVEL` | 1 | ~85 % smaller at ~1/3 of the CPU of level 6 |
| `DAS_SPECTRUM_MIN_INTERVAL_MS` | 0 | Throttle sweeps per session on very small CPUs |
| `DAS_SPECTRUM_IDLE_MS` | 15000 | Stop sweeping when nobody has asked for this long |
| `DAS_BLOCK_WARN_MS` | 200 | Log `event_loop_blocked` with suspect URLs above this |

## 6. Verify on a device

```sh
npm test                                                        # unit + compat + SLO tests (dev machine)
npm run check:es2019                                            # runtime code parses on Node 12
node bench/heartbeat-under-load.js --base http://<device-ip> --spectrum legacy --duration 60
```

Run the benchmark from a laptop against a staging device, before and after the
update, and attach both outputs to the release ticket.

## 7. Optional: frontend

See `frontend-patch/README.md`: tolerant connection monitor, liveness
interceptor, ETag interceptor and non-overlapping polling. Newer frontends can
move to `/api/spectrum/latest` (binary + decimation) to cut spectrum traffic from
~300 kB to ~3 kB per update.
