'use strict';
/**
 * LEGACY MODE - a faithful reproduction of the shipped behaviour, kept for
 * before/after demos and benchmarks. Do not use in production.
 *
 * Anti-patterns reproduced on purpose (all run on the event loop):
 *  1. Each GET /api/spectrum triggers its own hardware sweep, so N pollers mean N sweeps.
 *  2. JSON.parse of the ~1.5 MB raw driver output.
 *  3. Mapping 50 000 points into {frequency, power} objects.
 *  4. JSON.stringify of a ~2 MB response.
 *  5. GET /api/volatile-data re-stringifies the whole merged state per request.
 * While any of this runs, nothing else - including /api/heartbeat - can be served.
 */

var formats = require('../shared/formats');
var sim = require('../shared/spectrum-sim');
var httpUtil = require('../shared/http-util');
var ConfigStore = require('../shared/config-store').ConfigStore;
var emu = require('../shared/emulation'); // DAS_CPU_SLOWDOWN: emulate the device CPU on a fast PC

function register(router, ctx) {
  var cfg = ctx.cfg;
  var hardware = ctx.hardware;
  var nodes = {}; // volatile_data.nodes, merged in place
  var rev = 0;
  var sweepId = 0;
  var configs = new ConfigStore({ bootId: ctx.bootId, nodes: cfg.nodes });

  ctx.telemetry.on('report', function (r) { nodes[r.id] = r; rev++; });

  router.get('/api/heartbeat', function (req, res) {
    httpUtil.sendJson(res, 200, { status: 'ok', server: 'das-legacy', time: Date.now() }, { 'Cache-Control': 'no-store' });
  });

  router.get('/api/volatile-data', function (req, res) {
    var body = emu.heavy(function () { return formats.legacyVolatileJson(rev, Date.now(), nodes); }); // whole blob, every request
    res.setHeader('Content-Type', 'application/json; charset=utf-8');
    res.end(body);
  });

  router.get('/api/spectrum', function (req, res) {
    var q = httpUtil.queryOf(req);
    var params = sim.clampParams({ nodeId: q.nodeId, port: q.port, startHz: q.startHz, stopHz: q.stopHz, points: q.points || cfg.defaultPoints });
    return hardware.sweep(params).then(function (raw) {
      var meta = { sweepId: ++sweepId, timestamp: Date.now() };
      var json = emu.heavy(function () {
        var obj = JSON.parse(raw.toString('utf8')); // blocks: parse ~1.5 MB
        var body = formats.legacySpectrumBody(obj, meta); // blocks: 50k objects
        return JSON.stringify(body); // blocks: ~2 MB string
      });
      res.setHeader('Content-Type', 'application/json; charset=utf-8');
      res.end(json);
    });
  });

  router.get(/^\/api\/nodes\/(\d+)\/config$/, function (req, res) {
    var id = parseInt(req.params[0], 10);
    var c = configs.get(id);
    if (!c) return httpUtil.sendJson(res, 404, { error: 'NOT_FOUND' });
    httpUtil.sendJson(res, 200, c);
  });

  router.put(/^\/api\/nodes\/(\d+)\/config$/, function (req, res) {
    var id = parseInt(req.params[0], 10);
    return httpUtil.readJson(req).then(function (body) {
      var out = configs.put(id, body, null); // legacy: last write wins
      httpUtil.sendJson(res, out.status, out.body);
    });
  });
}

module.exports = { register: register };
