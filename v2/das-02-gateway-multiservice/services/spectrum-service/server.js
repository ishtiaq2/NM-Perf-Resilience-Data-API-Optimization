'use strict';
/**
 * spectrum-service: the dedicated server for heavy spectrum-analyzer data.
 *
 *   GET /api/spectrum          legacy JSON (shipped frontend), shared sweeps
 *   GET /api/spectrum/latest   compact JSON / binary DSPC, decimation, ETag, long-poll
 *   WS  /api/ws/spectrum       binary frame push per sweep (conflated)
 *
 * It runs in its own process (and systemd cgroup with lower CPU weight and a
 * memory cap). Whatever happens here cannot delay the heartbeat, configuration or
 * telemetry. Parsing and encoding still run in worker threads, so this service's
 * own event loop stays responsive for its WebSocket clients.
 */

var path = require('path');
var svc = require('../../lib/service');
var httpUtil = require('../../lib/http-util');
var api = require('../../lib/api-handlers');
var WorkerPool = require('../../lib/worker-pool').WorkerPool;
var SpectrumService = require('../../lib/spectrum-service').SpectrumService;
var SpectrumHub = require('../../lib/spectrum-hub').SpectrumHub;
var SimulatedSpectrumHardware = require('../../lib/spectrum-sim').SimulatedSpectrumHardware;

function start(overrides) {
  var o = Object.assign({
    listen: svc.envStr('LISTEN', '127.0.0.1:8082'),
    defaultPoints: svc.envInt('DAS_SPECTRUM_POINTS', 50001),
    sweepTimeMs: svc.envInt('DAS_SWEEP_TIME_MS', 250),
    idleMs: svc.envInt('DAS_SPECTRUM_IDLE_MS', 15000),
    minIntervalMs: svc.envInt('DAS_SPECTRUM_MIN_INTERVAL_MS', 0),
    maxSessions: svc.envInt('DAS_SPECTRUM_MAX_SESSIONS', 8),
    sweepWaitMs: svc.envInt('DAS_SWEEP_WAIT_MS', 10000),
    workers: svc.envInt('DAS_WORKERS', 0),
    workerNice: svc.envInt('DAS_WORKER_NICE', 10),
    workerHeapMb: svc.envInt('DAS_WORKER_HEAP_MB', 96),
    workerYoungMb: svc.envInt('DAS_WORKER_YOUNG_MB', 8),
    gzipLevel: svc.envInt('DAS_GZIP_LEVEL', 1),
    simPrewarm: svc.envInt('DAS_SIM_PREWARM', 4),
    demo: svc.envStr('DAS_DEMO', '0') === '1'
  }, overrides || {});

  // Simulator only: pre-generate sweep data before listening.
  var hardware = new SimulatedSpectrumHardware({ sweepTimeMs: o.sweepTimeMs });
  var warm = [];
  for (var a = 1; a <= o.simPrewarm; a++) warm.push({ nodeId: a, port: 1, points: o.defaultPoints });
  hardware.prewarm(warm);

  var s = svc.createService('spectrum-service');
  var metrics = api.createCounters();
  var pool = new WorkerPool({
    script: path.join(__dirname, '..', '..', 'lib', 'spectrum-worker.js'),
    size: o.workers || undefined,
    niceness: o.workerNice,
    resourceLimits: { maxOldGenerationSizeMb: o.workerHeapMb, maxYoungGenerationSizeMb: o.workerYoungMb },
    log: s.log
  });
  var spectrum = new SpectrumService({
    pool: pool, hardware: hardware, bootId: s.bootId, idleTimeoutMs: o.idleMs, minSweepIntervalMs: o.minIntervalMs,
    maxSessions: o.maxSessions, gzipLevel: o.gzipLevel, buildLegacy: true, log: s.log
  });
  var hub = new SpectrumHub({ spectrum: spectrum, server: 'das-spectrum-service', log: s.log, defaultPoints: o.defaultPoints });
  var sp = api.spectrumHandlers({ spectrum: spectrum, defaultPoints: o.defaultPoints, sweepWaitMs: o.sweepWaitMs, count: metrics.count });

  s.router.get('/api/spectrum', sp.legacy);
  s.router.get('/api/spectrum/latest', sp.latest);
  s.onUpgrade('/api/ws/spectrum', function (req, socket, head) { hub.handleUpgrade(req, socket, head); });

  s.router.get('/internal/metrics', function (req, res) {
    httpUtil.sendJson(res, 200, { service: s.name, eventLoop: s.loop.snapshot(), workers: pool.status(), spectrum: spectrum.status(), ws: hub.status(), routes: metrics.counters, memory: process.memoryUsage() });
  });
  s.setHealthExtra(function () { var ps = pool.status(); return { workersAlive: ps.alive, workersBusy: ps.busy, queued: ps.queued, wsClients: hub.clients.size }; });

  if (o.demo) {
    // Isolation demo only (DAS_DEMO=1): simulate a bug that blocks THIS process.
    s.router.post('/internal/debug/block', function (req, res) {
      var ms = Math.min(20000, parseInt(httpUtil.queryOf(req).ms, 10) || 3000);
      s.log('debug_block', { ms: ms });
      var t = Date.now();
      while (Date.now() - t < ms) { /* deliberately block the event loop */ }
      httpUtil.sendJson(res, 200, { blockedMs: ms });
    });
  }

  s.onClose(function () { hub.close(); spectrum.close(); return pool.close(); });
  return { service: s, ready: s.listen(o.listen), spectrum: spectrum, hub: hub, pool: pool };
}

if (require.main === module) {
  var inst = start();
  inst.ready.catch(function (err) { inst.service.log('listen_failed', { error: String(err) }); process.exit(1); });
  inst.service.runForever();
}

module.exports = { start: start };
