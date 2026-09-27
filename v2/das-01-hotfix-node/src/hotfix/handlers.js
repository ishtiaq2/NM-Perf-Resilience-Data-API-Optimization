'use strict';
/**
 * HOTFIX MODE - same URLs and same response bodies as the shipped release,
 * but the event loop never parses, transforms or serialises large payloads.
 *
 *   GET /api/heartbeat          cheap, never waits on anything, reports loop health
 *   GET /api/spectrum           legacy shape, byte-identical; built once per sweep in a worker
 *   GET /api/spectrum/latest    NEW (opt-in): compact JSON or binary frame, ETag/304,
 *                               peak-detector decimation (?maxPoints=), long-poll (?waitMs=)
 *   GET /api/volatile-data      legacy shape, byte-identical; incremental serialisation,
 *                               ETag/304, gzip once per revision; NEW opt-in ?since=<rev> delta
 *   GET/PUT /api/nodes/:id/config  unchanged + optional If-Match optimistic locking
 *   GET /api/capabilities       NEW: lets newer frontends discover opt-in features
 *   GET /api/metrics            NEW: JSON or Prometheus text (?format=prom)
 */

var path = require('path');
var httpUtil = require('../shared/http-util');
var decimate = require('../shared/decimate');
var api = require('../shared/api-handlers');
var WorkerPool = require('../shared/worker-pool').WorkerPool;
var SpectrumService = require('../shared/spectrum-service').SpectrumService;
var VolatileStore = require('../shared/volatile-store').VolatileStore;
var ConfigStore = require('../shared/config-store').ConfigStore;

function register(router, ctx) {
  var cfg = ctx.cfg;
  var log = ctx.log;
  var startedAt = Date.now();
  var metrics = api.createCounters();
  var count = metrics.count;

  var pool = new WorkerPool({
    script: path.join(__dirname, '..', 'shared', 'spectrum-worker.js'),
    size: cfg.workers || undefined,
    niceness: cfg.workerNice,
    maxQueue: cfg.workerQueue,
    taskTimeoutMs: cfg.taskTimeoutMs,
    // A small young generation keeps each worker's heap around 20-30 MB instead of
    // ~90 MB (measured with 50k-point sweeps) at the same speed.
    resourceLimits: { maxOldGenerationSizeMb: cfg.workerHeapMb, maxYoungGenerationSizeMb: cfg.workerYoungMb },
    log: log
  });

  var spectrum = new SpectrumService({
    pool: pool,
    hardware: ctx.hardware,
    bootId: ctx.bootId,
    idleTimeoutMs: cfg.spectrumIdleMs,
    minSweepIntervalMs: cfg.spectrumMinIntervalMs,
    maxSessions: cfg.spectrumMaxSessions,
    gzipLevel: cfg.gzipLevel,
    buildLegacy: true,
    log: log
  });

  // normalize:false keeps the payload byte-identical to the shipped release.
  var store = new VolatileStore({ bootId: ctx.bootId, publishIntervalMs: cfg.publishIntervalMs, gzipLevel: cfg.gzipLevel, normalize: false });
  ctx.telemetry.on('report', function (r) { store.ingest(r); });
  store.start();

  var configs = new ConfigStore({ bootId: ctx.bootId, nodes: cfg.nodes });
  var sp = api.spectrumHandlers({ spectrum: spectrum, defaultPoints: cfg.defaultPoints, sweepWaitMs: cfg.sweepWaitMs, count: count });
  var cf = api.configHandlers({ configs: configs, log: log, count: count });

  // ---------------------------------------------------------------- heartbeat
  // Registered first, does no I/O, allocates almost nothing. It measures the
  // event loop instead of depending on it being idle.
  router.get('/api/heartbeat', function (req, res) {
    var loop = ctx.loop.snapshot();
    var ps = pool.status();
    var degraded = loop.lagP99Ms > 250 || ps.alive < ps.size;
    httpUtil.sendJson(res, 200, {
      status: degraded ? 'degraded' : 'ok',
      server: 'das-hotfix-node',
      version: cfg.version,
      mode: 'hotfix',
      bootId: ctx.bootId,
      time: Date.now(),
      uptimeS: Math.round((Date.now() - startedAt) / 100) / 10,
      eventLoop: loop,
      workers: { size: ps.size, alive: ps.alive, busy: ps.busy, queued: ps.queued }
    }, { 'Cache-Control': 'no-store' });
    count('heartbeat', 200);
  });

  router.get('/api/capabilities', function (req, res) {
    httpUtil.sendJson(res, 200, {
      api: 'das-v1',
      server: 'das-hotfix-node',
      version: cfg.version,
      features: {
        etag: true,
        volatileDelta: true,
        spectrumLatest: true,
        spectrumBinary: true,
        spectrumDecimation: true,
        spectrumLongPoll: true,
        configOptimisticLocking: true,
        wsTelemetry: false,
        wsSpectrum: false
      },
      limits: { maxSpectrumPoints: 200001, maxDecimatedPoints: decimate.MAX_POINTS, recommendedPollMs: { heartbeat: 2000, volatileData: 2000 } }
    }, { 'Cache-Control': 'no-cache' });
  });

  router.get('/api/volatile-data', api.volatileHandler({ store: store, gzipLevel: cfg.gzipLevel, count: count }));
  router.get('/api/spectrum', sp.legacy);
  router.get('/api/spectrum/latest', sp.latest);
  router.get(/^\/api\/nodes\/(\d+)\/config$/, cf.get);
  router.put(/^\/api\/nodes\/(\d+)\/config$/, cf.put);

  // ------------------------------------------------------------------ metrics
  router.get('/api/metrics', function (req, res) {
    var loop = ctx.loop.snapshot();
    var ps = pool.status();
    var ss = spectrum.status();
    if (httpUtil.queryOf(req).format === 'prom') {
      res.setHeader('Content-Type', 'text/plain; version=0.0.4');
      res.end(api.promText({
        das_event_loop_lag_p99_ms: loop.lagP99Ms,
        das_event_loop_lag_max_ms: loop.lagMaxMs,
        das_event_loop_blocked_total: loop.blockedEvents,
        das_worker_queue_depth: ps.queued,
        das_worker_tasks_completed_total: ps.stats.completed,
        das_worker_tasks_failed_total: ps.stats.failed,
        das_worker_tasks_rejected_total: ps.stats.rejected,
        das_spectrum_sweeps_total: ss.stats.sweeps,
        das_volatile_revision: store.rev,
        das_process_rss_bytes: process.memoryUsage().rss
      }, metrics.counters));
      return;
    }
    httpUtil.sendJson(res, 200, {
      server: 'das-hotfix-node',
      uptimeS: Math.round((Date.now() - startedAt) / 1000),
      eventLoop: loop,
      workers: ps,
      spectrum: ss,
      volatile: { rev: store.rev, nodes: store.size(), stats: store.stats },
      routes: metrics.counters,
      memory: process.memoryUsage()
    }, { 'Cache-Control': 'no-store' });
  });

  return {
    close: function () { store.stop(); spectrum.close(); return pool.close(); },
    pool: pool,
    spectrum: spectrum,
    store: store
  };
}

module.exports = { register: register };
