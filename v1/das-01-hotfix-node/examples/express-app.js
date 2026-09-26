'use strict';
/**
 * Mounting the hotfix into an existing Express application.
 *
 *   npm install            # installs express (dev dependency, only for this example)
 *   npm run example:express
 *
 * The three things that matter in your real app:
 *  1. Register /api/heartbeat BEFORE any heavy middleware (body parsers,
 *     compression, auth that reads files or a database, request loggers that
 *     serialise bodies).
 *  2. Create the worker pool, spectrum service and volatile store ONCE at start-up
 *     and pass in your real hardware adapter and telemetry source.
 *  3. Express 4 ignores promises returned by handlers; wrap() forwards errors.
 */

const express = require('express');
const crypto = require('crypto');
const cfg = require('../src/config');
const hotfix = require('../src/hotfix/handlers');
const { LoopMonitor } = require('../src/shared/loop-monitor');
const { SimulatedSpectrumHardware } = require('../src/shared/spectrum-sim');
const { RemoteNodeSimulator } = require('../src/shared/node-sim');

const app = express();

function log(event, fields) { process.stderr.write(JSON.stringify({ t: new Date().toISOString(), event, ...fields }) + '\n'); }

// Adapter: the hotfix registers plain (req, res) handlers through a router-like object.
const wrap = (h) => (req, res, next) => {
  try {
    const out = h(req, res);
    if (out && typeof out.then === 'function') out.catch(next);
  } catch (err) { next(err); }
};
const router = {
  get: (p, h) => app.get(p, wrap(h)),
  put: (p, h) => app.put(p, wrap(h)),
  post: (p, h) => app.post(p, wrap(h))
};

// --- your real integrations go here -----------------------------------------
const hardware = new SimulatedSpectrumHardware({ sweepTimeMs: cfg.sweepTimeMs }); // replace: object with sweep(params) -> Promise<Buffer>
hardware.prewarm([{ nodeId: 1, port: 1, points: cfg.defaultPoints }]);
const telemetry = new RemoteNodeSimulator({ nodes: cfg.nodes }); // replace: EventEmitter emitting 'report' per node update
// -----------------------------------------------------------------------------

const loop = new LoopMonitor({ warnMs: cfg.blockWarnMs, onBlocked: (info) => log('event_loop_blocked', info) });
const ctx = { cfg, bootId: crypto.randomBytes(4).toString('hex'), hardware, telemetry, loop, log };

// 1) hotfix routes first: heartbeat, spectrum, volatile-data, config, metrics
hotfix.register(router, ctx);

// 2) ...then your existing middleware and routes, unchanged
app.use(express.json({ limit: '256kb' }));
app.get('/api/legacy-example', (req, res) => res.json({ hello: 'existing route' }));

// 3) error handler: JSON errors with Retry-After for overload
app.use((err, req, res, next) => { // eslint-disable-line no-unused-vars
  const status = err.status || 500;
  if (status === 503) res.set('Retry-After', String(err.retryAfter || 1));
  res.status(status).json({ error: err.code || 'INTERNAL', message: err.message });
});

telemetry.start();
const port = Number(process.env.PORT || 8080);
app.listen(port, () => log('listening', { port, framework: 'express' }));
