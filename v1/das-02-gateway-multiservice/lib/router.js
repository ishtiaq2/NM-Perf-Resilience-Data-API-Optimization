'use strict';
/**
 * Tiny dependency-free router (for the PoC server). Your real app keeps its
 * Express/Fastify/Koa router; the handlers are plain (req, res) functions.
 *
 * It also keeps a ring buffer of recently started requests, so the blocked-loop
 * detector can name the handler that stalled the event loop.
 *
 * ES2019 / CommonJS (Node 12+).
 */

var httpUtil = require('./http-util');
var nowMs = require('./loop-monitor').nowMs;

function createRouter(opts) {
  opts = opts || {};
  var routes = [];
  var recent = [];
  var RECENT_MAX = 64;
  var log = opts.log || function () {};

  function add(method, pattern, handler) { routes.push({ method: method, pattern: pattern, handler: handler }); }

  function fail(res, err) {
    var status = err && err.status ? err.status : 500;
    if (status >= 500) log('handler_error', { error: String(err && err.stack || err) });
    if (res.headersSent) { res.destroy(); return; }
    var headers = { 'Cache-Control': 'no-store' };
    if (status === 503) headers['Retry-After'] = String(err.retryAfter || 1);
    httpUtil.sendJson(res, status, { error: (err && err.code) || 'INTERNAL', message: String(err && err.message || err) }, headers);
  }

  function handle(req, res) {
    recent.push({ method: req.method, url: req.url, startedAt: nowMs() });
    if (recent.length > RECENT_MAX) recent.shift();
    var path = httpUtil.pathOf(req);
    var method = req.method === 'HEAD' ? 'GET' : req.method;
    var allowed = [];
    for (var i = 0; i < routes.length; i++) {
      var r = routes[i];
      var m = null;
      if (typeof r.pattern === 'string') { if (r.pattern !== path) continue; } else { m = r.pattern.exec(path); if (!m) continue; }
      if (r.method !== method) { allowed.push(r.method); continue; }
      req.params = m ? (m.groups || m.slice(1)) : {};
      try {
        var out = r.handler(req, res);
        if (out && typeof out.then === 'function') out.then(null, function (err) { fail(res, err); });
      } catch (err) { fail(res, err); }
      return;
    }
    if (allowed.length) {
      httpUtil.sendJson(res, 405, { error: 'METHOD_NOT_ALLOWED' }, { Allow: allowed.join(', ') });
      return;
    }
    httpUtil.sendJson(res, 404, { error: 'NOT_FOUND', message: 'no route for ' + req.method + ' ' + path });
  }

  return {
    get: function (p, h) { add('GET', p, h); },
    put: function (p, h) { add('PUT', p, h); },
    post: function (p, h) { add('POST', p, h); },
    handle: handle,
    fail: fail,
    recent: function () { return recent.slice(); }
  };
}

module.exports = { createRouter: createRouter };
