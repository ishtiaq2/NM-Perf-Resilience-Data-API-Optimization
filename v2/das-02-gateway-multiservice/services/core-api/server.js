'use strict';
/**
 * core-api: the existing application, now without heavy data paths.
 *
 *   GET /api/heartbeat            liveness + health of the other services
 *   GET /api/capabilities         feature discovery (WebSocket push available)
 *   GET/PUT /api/nodes/:id/config configuration with optimistic locking
 *   GET /api/metrics              own + aggregated service metrics
 *   ...                           every other existing endpoint stays here
 *
 * The heartbeat now answers "is the Master Node reachable?" separately from
 * "is every service healthy?". A saturated spectrum-service shows up as
 * `status: degraded, services.spectrum: degraded` instead of a dead server.
 */

var http = require('http');
var svc = require('../../lib/service');
var httpUtil = require('../../lib/http-util');
var api = require('../../lib/api-handlers');
var decimate = require('../../lib/decimate');
var ConfigStore = require('../../lib/config-store').ConfigStore;

/** GET a small JSON document from another service (Unix socket or TCP) with a hard timeout. */
function probe(target, path, timeoutMs) {
  var where = svc.parseListen(target);
  return new Promise(function (resolve) {
    var t0 = Date.now();
    var opts = where.path ? { socketPath: where.path, path: path } : { host: where.host, port: where.port, path: path };
    var req = http.get(opts, function (res) {
      var chunks = [];
      res.on('data', function (c) { chunks.push(c); });
      res.on('end', function () {
        try { resolve({ ok: res.statusCode === 200, ms: Date.now() - t0, body: JSON.parse(Buffer.concat(chunks).toString('utf8')) }); } catch (e) { resolve({ ok: false, ms: Date.now() - t0 }); }
      });
    });
    req.setTimeout(timeoutMs, function () { req.destroy(); });
    req.on('error', function () { resolve({ ok: false, ms: Date.now() - t0 }); });
  });
}

function start(overrides) {
  var o = Object.assign({
    listen: svc.envStr('LISTEN', '127.0.0.1:8081'),
    nodes: svc.envInt('DAS_NODES', 300),
    upstreams: {
      spectrum: svc.envStr('DAS_UPSTREAM_SPECTRUM', '127.0.0.1:8082'),
      telemetry: svc.envStr('DAS_UPSTREAM_TELEMETRY', '127.0.0.1:8083')
    },
    probeIntervalMs: svc.envInt('DAS_HEALTH_PROBE_MS', 2000),
    version: '2.0.0-gateway'
  }, overrides || {});

  var s = svc.createService('core-api');
  var metrics = api.createCounters();
  var configs = new ConfigStore({ bootId: s.bootId, nodes: o.nodes });
  var cf = api.configHandlers({ configs: configs, log: s.log, count: metrics.count });

  // Service health, probed in the background. The heartbeat itself never waits on I/O.
  var health = {};
  Object.keys(o.upstreams).forEach(function (k) { health[k] = { status: 'unknown', misses: 0 }; });
  function probeAll() {
    Object.keys(o.upstreams).forEach(function (k) {
      probe(o.upstreams[k], '/internal/health', 1000).then(function (r) {
        var h = health[k];
        if (r.ok) { h.misses = 0; h.status = r.body.status === 'ok' && r.ms < 500 ? 'ok' : 'degraded'; h.detail = r.body; }
        else { h.misses++; h.status = h.misses >= 3 ? 'down' : 'degraded'; }
        h.checkedAt = Date.now();
      });
    });
  }
  probeAll();
  var probeTimer = setInterval(probeAll, o.probeIntervalMs);
  probeTimer.unref();

  s.router.get('/api/heartbeat', function (req, res) {
    var loop = s.loop.snapshot();
    var services = {};
    var degraded = loop.lagP99Ms > 250;
    Object.keys(health).forEach(function (k) { services[k] = health[k].status; if (health[k].status !== 'ok') degraded = true; });
    httpUtil.sendJson(res, 200, {
      status: degraded ? 'degraded' : 'ok',
      server: 'das-core-api',
      version: o.version,
      bootId: s.bootId,
      time: Date.now(),
      uptimeS: Math.round((Date.now() - s.startedAt) / 100) / 10,
      eventLoop: loop,
      services: services
    }, { 'Cache-Control': 'no-store' });
    metrics.count('heartbeat', 200);
  });

  s.router.get('/api/capabilities', function (req, res) {
    httpUtil.sendJson(res, 200, {
      api: 'das-v1',
      server: 'das-gateway (nginx) + core-api/spectrum-service/telemetry-service',
      version: o.version,
      features: {
        etag: true, volatileDelta: true, spectrumLatest: true, spectrumBinary: true, spectrumDecimation: true,
        spectrumLongPoll: true, configOptimisticLocking: true, wsTelemetry: true, wsSpectrum: true
      },
      limits: { maxSpectrumPoints: 200001, maxDecimatedPoints: decimate.MAX_POINTS, recommendedPollMs: { heartbeat: 2000, volatileData: 2000 } }
    }, { 'Cache-Control': 'no-cache' });
  });

  s.router.get(/^\/api\/nodes\/(\d+)\/config$/, cf.get);
  s.router.put(/^\/api\/nodes\/(\d+)\/config$/, cf.put);

  // Aggregated metrics (best effort, 1 s per service).
  s.router.get('/api/metrics', function (req, res) {
    var names = Object.keys(o.upstreams);
    return Promise.all(names.map(function (k) { return probe(o.upstreams[k], '/internal/metrics', 1000); })).then(function (rs) {
      var out = { 'core-api': { eventLoop: s.loop.snapshot(), routes: metrics.counters, health: health, memory: process.memoryUsage() } };
      names.forEach(function (k, i) { out[k] = rs[i].ok ? rs[i].body : { error: 'unreachable' }; });
      if (httpUtil.queryOf(req).format === 'prom') {
        var g = { das_core_event_loop_lag_p99_ms: out['core-api'].eventLoop.lagP99Ms };
        names.forEach(function (k) { g['das_service_up{service="' + k + '"}'] = health[k].status === 'ok' ? 1 : 0; });
        res.setHeader('Content-Type', 'text/plain; version=0.0.4');
        res.end(api.promText(g, metrics.counters));
        return;
      }
      httpUtil.sendJson(res, 200, out, { 'Cache-Control': 'no-store' });
    });
  });

  s.setHealthExtra(function () { return { services: Object.keys(health).reduce(function (acc, k) { acc[k] = health[k].status; return acc; }, {}) }; });
  s.onClose(function () { clearInterval(probeTimer); });
  return { service: s, ready: s.listen(o.listen), health: health };
}

if (require.main === module) {
  var inst = start();
  inst.ready.catch(function (err) { inst.service.log('listen_failed', { error: String(err) }); process.exit(1); });
  inst.service.runForever();
}

module.exports = { start: start, probe: probe };
