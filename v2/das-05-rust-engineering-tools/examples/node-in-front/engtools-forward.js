'use strict';
/**
 * Option C of docs/INTEGRATION.md: the Node.js application stays the front (it may
 * terminate HTTPS itself) and forwards the engineering-tools routes to das-engtools
 * over a Unix socket. It moves bytes only (no JSON parsing, no buffering), so the
 * event loop stays free: a 2 MB legacy sweep costs Node a few socket reads/writes.
 *
 * Copy this file into the app and mount it before the app's own routes:
 *
 *   var engtools = require('./engtools-forward')({ socketPath: '/run/das/engtools.sock' });
 *   server.on('request', function (req, res) { if (!engtools.handle(req, res)) app(req, res); });
 *   server.on('upgrade', function (req, socket, head) { if (!engtools.upgrade(req, socket, head)) socket.destroy(); });
 *
 * With Express: app.use(function (req, res, next) { if (!engtools.handle(req, res)) next(); });
 * (mount it before body parsers and compression).
 *
 * ES2019 / CommonJS, no dependencies (Node 12+).
 */

var http = require('http');
var net = require('net');

// /api/capabilities is forwarded only while the app has none of its own: das-engtools
// then describes the engineering features (run it WITHOUT --legacy in this setup).
var DEFAULT_ROUTES = /^\/api\/(spectrum(\/latest)?|dtf|engineering\/status|capabilities)(\?|$)/;
var WS_ROUTES = /^\/api\/ws\/spectrum(\?|$)/;
var HOP = ['connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization', 'te', 'trailer', 'transfer-encoding', 'upgrade'];

module.exports = function createEngtoolsForward(opts) {
  var socketPath = opts.socketPath;
  var routes = opts.routes || DEFAULT_ROUTES;
  var agent = new http.Agent({ keepAlive: true, maxSockets: 16 });

  function forwardedHeaders(req) {
    var h = {};
    Object.keys(req.headers).forEach(function (k) { if (HOP.indexOf(k) < 0) h[k] = req.headers[k]; });
    var ip = req.socket.remoteAddress || '';
    h['x-forwarded-for'] = req.headers['x-forwarded-for'] ? req.headers['x-forwarded-for'] + ', ' + ip : ip;
    h['x-forwarded-proto'] = req.socket.encrypted ? 'https' : 'http';
    h['x-forwarded-host'] = req.headers.host || '';
    return h;
  }

  /** Returns true when the request was taken over. */
  function handle(req, res) {
    if (!routes.test(req.url)) return false;
    var up = http.request({ socketPath: socketPath, agent: agent, method: req.method, path: req.url, headers: forwardedHeaders(req) }, function (r) {
      var h = {};
      Object.keys(r.headers).forEach(function (k) { if (HOP.indexOf(k) < 0) h[k] = r.headers[k]; });
      res.writeHead(r.statusCode, h);
      r.pipe(res); // streams; back-pressure is handled by pipe()
    });
    up.setTimeout(30000, function () { up.destroy(new Error('timeout')); });
    up.on('error', function () {
      if (!res.headersSent) {
        res.writeHead(502, { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store' });
        res.end('{"error":"UPSTREAM_UNAVAILABLE","message":"engineering tools service unreachable"}');
      } else {
        res.destroy();
      }
    });
    res.on('close', function () { if (!res.writableFinished) up.destroy(); }); // browser went away
    req.pipe(up);
    return true;
  }

  /** WebSocket upgrades for /api/ws/spectrum: replay the handshake, then splice both ways. */
  function upgrade(req, socket, head) {
    if (!WS_ROUTES.test(req.url)) return false;
    var up = net.connect(socketPath, function () {
      var lines = [req.method + ' ' + req.url + ' HTTP/1.1'];
      for (var i = 0; i < req.rawHeaders.length; i += 2) lines.push(req.rawHeaders[i] + ': ' + req.rawHeaders[i + 1]);
      up.write(lines.join('\r\n') + '\r\n\r\n');
      if (head && head.length) up.write(head);
      up.pipe(socket);
      socket.pipe(up);
    });
    up.on('error', function () { socket.destroy(); });
    socket.on('error', function () { up.destroy(); });
    return true;
  }

  return { handle: handle, upgrade: upgrade, close: function () { agent.destroy(); } };
};
