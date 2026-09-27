'use strict';
/**
 * DAS Master Node web backend - PoC server for the hotfix.
 *
 *   DAS_MODE=hotfix node src/server.js   (default) the fix
 *   DAS_MODE=legacy node src/server.js   reproduces the shipped behaviour
 *
 * Zero npm dependencies: it runs on the device by copying the folder.
 * In your real code base you keep your framework (Express etc.) and mount the
 * same handlers; see docs/INTEGRATION.md and examples/express-app.js.
 */

var http = require('http');
var crypto = require('crypto');
var cfg = require('./config');
var createRouter = require('./shared/router').createRouter;
var LoopMonitor = require('./shared/loop-monitor').LoopMonitor;
var SimulatedSpectrumHardware = require('./shared/spectrum-sim').SimulatedSpectrumHardware;
var RemoteNodeSimulator = require('./shared/node-sim').RemoteNodeSimulator;

function log(event, fields) {
  var line = Object.assign({ t: new Date().toISOString(), event: event }, fields || {});
  process.stderr.write(JSON.stringify(line) + '\n');
}

function start(overrides) {
  var conf = Object.assign({}, cfg, overrides || {});
  var bootId = crypto.randomBytes(4).toString('hex');
  var router = createRouter({ log: log });
  var hardware = new SimulatedSpectrumHardware({ sweepTimeMs: conf.sweepTimeMs });
  // Simulator only: pre-generate sweep data for analyzers 1..N (port 1) so the
  // simulator's own CPU cost is paid before listening, not on the first request.
  var warm = [];
  for (var a = 1; a <= conf.simPrewarm; a++) warm.push({ nodeId: a, port: 1, points: conf.defaultPoints });
  hardware.prewarm(warm);
  var loop = new LoopMonitor({
    warnMs: conf.blockWarnMs,
    recentRequests: router.recent,
    onBlocked: function (info) { log('event_loop_blocked', info); }
  });
  var nodeSim = new RemoteNodeSimulator({ nodes: conf.nodes, reportIntervalMs: conf.reportIntervalMs });
  // ctx is the integration seam: in the real backend, pass the real hardware adapter
  // (sweep(params) -> Promise<Buffer>) and the real telemetry source (emits 'report').
  var ctx = { cfg: conf, bootId: bootId, hardware: hardware, telemetry: nodeSim, loop: loop, log: log };

  var mod = conf.mode === 'legacy' ? require('./legacy/handlers') : require('./hotfix/handlers');
  var app = mod.register(router, ctx) || {};
  nodeSim.start();

  var server = http.createServer(router.handle);
  server.keepAliveTimeout = 65000; // longer than typical proxy/browser idle timeouts
  server.headersTimeout = 66000;

  var ready = new Promise(function (resolve, reject) {
    server.once('error', reject);
    server.listen(conf.port, conf.host, function () {
      var addr = server.address();
      log('listening', { mode: conf.mode, host: conf.host, port: addr.port, bootId: bootId, pid: process.pid, node: process.version });
      resolve(addr.port);
    });
  });

  function close() {
    nodeSim.stop();
    loop.stop();
    return new Promise(function (resolve) {
      server.close(function () { resolve(); });
      // Drop idle keep-alive sockets so close() completes promptly.
      if (server.closeIdleConnections) server.closeIdleConnections();
    }).then(function () { return app.close ? app.close() : null; });
  }

  return { server: server, ready: ready, close: close, ctx: ctx, app: app };
}

if (require.main === module) {
  var inst = start();
  inst.ready.catch(function (err) { log('listen_failed', { error: String(err) }); process.exit(1); });
  var stopping = false;
  var shutdown = function (sig) {
    if (stopping) return;
    stopping = true;
    log('shutdown', { signal: sig });
    var force = setTimeout(function () { process.exit(0); }, 5000);
    if (force.unref) force.unref();
    inst.close().then(function () { process.exit(0); });
  };
  process.on('SIGTERM', function () { shutdown('SIGTERM'); });
  process.on('SIGINT', function () { shutdown('SIGINT'); });
}

module.exports = { start: start };
