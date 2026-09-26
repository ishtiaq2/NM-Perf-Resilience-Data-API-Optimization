'use strict';
/**
 * Common bootstrap for every backend service behind the gateway:
 * structured logging, router, event-loop monitor, /internal/health,
 * WebSocket upgrade dispatch, listening on a Unix socket (production) or TCP
 * port (development), and graceful shutdown.
 *
 * Each service is its own OS process with its own event loop, so a spectrum
 * burst can no longer delay a heartbeat or a configuration write. systemd
 * (deploy/systemd) assigns each one its own CPU weight, niceness and memory cap.
 *
 * ES2019 / CommonJS (Node 12+).
 */

var http = require('http');
var fs = require('fs');
var net = require('net');
var crypto = require('crypto');
var createRouter = require('./router').createRouter;
var LoopMonitor = require('./loop-monitor').LoopMonitor;
var httpUtil = require('./http-util');

function envStr(name, def) { var v = process.env[name]; return v === undefined || v === '' ? def : v; }
function envInt(name, def) { var n = parseInt(process.env[name], 10); return isFinite(n) ? n : def; }

/** "unix:/run/das/core-api.sock" | "127.0.0.1:8081" | "8081" */
function parseListen(spec) {
  if (/^unix:/.test(spec)) return { path: spec.slice(5) };
  var m = /^(?:(.+):)?(\d+)$/.exec(spec);
  if (!m) throw new Error('invalid LISTEN value: ' + spec);
  return { host: m[1] || '127.0.0.1', port: parseInt(m[2], 10) };
}

/** Remove a Unix socket file left behind by a crashed process (but never a live one). */
function clearStaleSocket(path) {
  return new Promise(function (resolve, reject) {
    if (!fs.existsSync(path)) return resolve();
    var c = net.connect(path);
    c.once('connect', function () { c.destroy(); reject(new Error('socket ' + path + ' is in use by another process')); });
    c.once('error', function () { try { fs.unlinkSync(path); } catch (e) { /* ignore */ } resolve(); });
  });
}

/**
 * @param {string} name service name (log field, health output)
 * @param {object} [opts]
 * @param {string} [opts.listen] default for LISTEN
 */
function createService(name, opts) {
  opts = opts || {};
  var bootId = crypto.randomBytes(4).toString('hex');
  var startedAt = Date.now();
  function log(event, fields) {
    process.stderr.write(JSON.stringify(Object.assign({ t: new Date().toISOString(), svc: name, event: event }, fields || {})) + '\n');
  }
  var router = createRouter({ log: log });
  var loop = new LoopMonitor({
    warnMs: envInt('DAS_BLOCK_WARN_MS', 200),
    recentRequests: router.recent,
    onBlocked: function (info) { log('event_loop_blocked', info); }
  });
  var upgrades = [];
  var closers = [];
  var healthExtra = function () { return undefined; };

  router.get('/internal/health', function (req, res) {
    var l = loop.snapshot();
    httpUtil.sendJson(res, 200, {
      service: name,
      status: l.lagP99Ms > 250 ? 'degraded' : 'ok',
      bootId: bootId,
      pid: process.pid,
      uptimeS: Math.round((Date.now() - startedAt) / 1000),
      eventLoop: l,
      rssMb: Math.round(process.memoryUsage().rss / 1048576),
      detail: healthExtra()
    }, { 'Cache-Control': 'no-store' });
  });

  var server = http.createServer(router.handle);
  server.keepAliveTimeout = 65000;
  server.headersTimeout = 66000;
  server.on('upgrade', function (req, socket, head) {
    var path = httpUtil.pathOf(req);
    for (var i = 0; i < upgrades.length; i++) {
      if (upgrades[i].path === path) { upgrades[i].handler(req, socket, head); return; }
    }
    socket.end('HTTP/1.1 404 Not Found\r\nConnection: close\r\nContent-Length: 0\r\n\r\n');
  });

  function listen(spec) {
    var where = parseListen(spec || envStr('LISTEN', opts.listen || '127.0.0.1:0'));
    var ready = where.path ? clearStaleSocket(where.path) : Promise.resolve();
    return ready.then(function () {
      return new Promise(function (resolve, reject) {
        server.once('error', reject);
        var done = function () {
          if (where.path) {
            // Group read/write so the gateway (nginx) user can connect; nothing for others.
            fs.chmodSync(where.path, parseInt(envStr('DAS_SOCKET_MODE', '660'), 8));
          }
          var addr = server.address();
          var at = where.path ? 'unix:' + where.path : (typeof addr === 'object' ? addr.address + ':' + addr.port : String(addr));
          log('listening', { at: at, bootId: bootId, pid: process.pid, node: process.version });
          resolve({ at: at, port: typeof addr === 'object' && addr ? addr.port : null, path: where.path || null });
        };
        if (where.path) server.listen(where.path, done); else server.listen(where.port, where.host, done);
      });
    });
  }

  function close() {
    loop.stop();
    var jobs = closers.map(function (fn) { try { return fn(); } catch (e) { return null; } });
    return Promise.all(jobs).then(function () {
      return new Promise(function (resolve) {
        server.close(function () { resolve(); });
        if (server.closeIdleConnections) server.closeIdleConnections();
        setTimeout(resolve, 2000).unref();
      });
    });
  }

  function runForever() {
    var stopping = false;
    function shutdown(sig) {
      if (stopping) return;
      stopping = true;
      log('shutdown', { signal: sig });
      setTimeout(function () { process.exit(0); }, 5000).unref();
      close().then(function () { process.exit(0); });
    }
    process.on('SIGTERM', function () { shutdown('SIGTERM'); });
    process.on('SIGINT', function () { shutdown('SIGINT'); });
  }

  return {
    name: name,
    bootId: bootId,
    startedAt: startedAt,
    log: log,
    router: router,
    loop: loop,
    server: server,
    onUpgrade: function (path, handler) { upgrades.push({ path: path, handler: handler }); },
    onClose: function (fn) { closers.push(fn); },
    setHealthExtra: function (fn) { healthExtra = fn; },
    listen: listen,
    close: close,
    runForever: runForever
  };
}

module.exports = { createService: createService, parseListen: parseListen, envStr: envStr, envInt: envInt };
