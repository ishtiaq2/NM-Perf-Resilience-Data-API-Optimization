'use strict';
/**
 * telemetry-service: owns volatile_data.
 *
 *   ingest                   Remote Node reports (simulated here; on the device: the fiber-link receiver)
 *   GET /api/volatile-data   snapshot (ETag/304/gzip) or ?since=<rev> delta
 *   WS  /api/ws/telemetry    push: snapshot + one delta per published revision
 *
 * Change detection runs on normalised state (field hygiene + deadbands), so
 * clients receive data only when an engineer would see a difference: the
 * "push only if the checksum changed" idea, done without false positives from
 * counters and analog noise.
 */

var svc = require('../../lib/service');
var httpUtil = require('../../lib/http-util');
var api = require('../../lib/api-handlers');
var VolatileStore = require('../../lib/volatile-store').VolatileStore;
var TelemetryHub = require('../../lib/telemetry-hub').TelemetryHub;
var RemoteNodeSimulator = require('../../lib/node-sim').RemoteNodeSimulator;

function start(overrides) {
  var o = Object.assign({
    listen: svc.envStr('LISTEN', '127.0.0.1:8083'),
    nodes: svc.envInt('DAS_NODES', 300),
    reportIntervalMs: svc.envInt('DAS_REPORT_INTERVAL_MS', 1000),
    publishIntervalMs: svc.envInt('DAS_PUBLISH_INTERVAL_MS', 1000),
    gzipLevel: svc.envInt('DAS_GZIP_LEVEL', 1),
    normalize: svc.envStr('DAS_TELEMETRY_NORMALIZE', '1') === '1',
    heartbeatMs: svc.envInt('DAS_WS_HEARTBEAT_MS', 5000)
  }, overrides || {});

  var s = svc.createService('telemetry-service');
  var metrics = api.createCounters();
  var store = new VolatileStore({
    bootId: s.bootId,
    publishIntervalMs: o.publishIntervalMs,
    gzipLevel: o.gzipLevel,
    normalize: o.normalize,
    staleAfterMs: o.reportIntervalMs * 5
  });
  var hub = new TelemetryHub({ store: store, server: 'das-telemetry-service', log: s.log, heartbeatMs: o.heartbeatMs });

  // Ingest. On the device, replace the simulator with the receiver of Remote Node
  // reports and call store.ingest(report) for each report.
  var nodes = new RemoteNodeSimulator({ nodes: o.nodes, reportIntervalMs: o.reportIntervalMs });
  nodes.on('report', function (r) { store.ingest(r); });
  nodes.start();
  store.start();

  s.router.get('/api/volatile-data', api.volatileHandler({ store: store, gzipLevel: o.gzipLevel, count: metrics.count }));
  s.onUpgrade('/api/ws/telemetry', function (req, socket, head) { hub.handleUpgrade(req, socket, head); });
  s.router.get('/internal/metrics', function (req, res) {
    httpUtil.sendJson(res, 200, { service: s.name, eventLoop: s.loop.snapshot(), store: { rev: store.rev, nodes: store.size(), stats: store.stats }, ws: hub.status(), routes: metrics.counters, memory: process.memoryUsage() });
  });
  s.setHealthExtra(function () { return { rev: store.rev, nodes: store.size(), wsClients: hub.clients.size }; });

  s.onClose(function () { nodes.stop(); store.stop(); hub.close(); });
  return { service: s, ready: s.listen(o.listen), store: store, hub: hub, nodes: nodes };
}

if (require.main === module) {
  var inst = start();
  inst.ready.catch(function (err) { inst.service.log('listen_failed', { error: String(err) }); process.exit(1); });
  inst.service.runForever();
}

module.exports = { start: start };
