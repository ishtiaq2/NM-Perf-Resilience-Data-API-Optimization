'use strict';
/**
 * HTTP handler factories shared by every Node implementation of the DAS API
 * (hotfix single process, and the split services behind the gateway).
 * Plain (req, res) functions: they work with the PoC router and with Express.
 *
 * ES2019 / CommonJS (Node 12+).
 */

var httpUtil = require('./http-util');
var frame = require('./spectrum-frame');
var decimate = require('./decimate');
var gzipAsync = require('./spectrum-service').gzipAsync;

var JSON_TYPE = 'application/json; charset=utf-8';

/** Per-route request/304/error/byte counters for /api/metrics. */
function createCounters() {
  var counters = {};
  return {
    counters: counters,
    count: function (route, status, bytes) {
      var c = counters[route] || (counters[route] = { requests: 0, notModified: 0, errors: 0, bytes: 0 });
      c.requests++;
      if (status === 304) c.notModified++;
      if (status >= 500) c.errors++;
      c.bytes += bytes || 0;
    }
  };
}

/**
 * GET /api/spectrum (legacy shape) and GET /api/spectrum/latest (compact/binary).
 * @param {{spectrum: object, defaultPoints: number, sweepWaitMs: number, count: function}} o
 */
function spectrumHandlers(o) {
  var spectrum = o.spectrum;
  var count = o.count || function () {};

  function sessionFor(q) {
    try {
      return spectrum.session({ nodeId: q.nodeId, port: q.port, startHz: q.startHz, stopHz: q.stopHz, points: q.points || o.defaultPoints });
    } catch (e) {
      if (e.code === 'TOO_MANY_SESSIONS') { e.status = 503; e.retryAfter = 2; } else { e.status = 400; e.code = e.code || 'BAD_REQUEST'; }
      throw e;
    }
  }

  // Legacy endpoint: same shape and semantics (each poll returns a sweep
  // completed after the request arrived), but all clients share one sweep and
  // the body was built once, in a worker, when the sweep completed.
  function legacy(req, res) {
    var q = httpUtil.queryOf(req);
    var session = sessionFor(q);
    var gz = httpUtil.acceptsEncoding(req, 'gzip');
    return session.nextSweep(o.sweepWaitMs)
      .catch(function (err) {
        if (session.latest) return session.latest; // better stale than an error for the shipped UI
        err.status = 503;
        throw err;
      })
      .then(function (sweep) {
        return session.legacyBody(sweep, gz).then(function (b) {
          var h = { 'Content-Type': JSON_TYPE, 'Cache-Control': 'no-store', ETag: b.etag, Vary: 'Accept-Encoding', 'X-Sweep-Id': String(sweep.sweepId) };
          if (b.encoding === 'gzip') h['Content-Encoding'] = 'gzip';
          httpUtil.sendBuffer(res, 200, b.body, h);
          count('spectrum-legacy', 200, b.body.length);
        });
      });
  }

  // New endpoint: latest sweep immediately, conditional, compact, optionally
  // decimated to the display width, optionally binary, optionally long-poll.
  function latest(req, res) {
    var q = httpUtil.queryOf(req);
    var session = sessionFor(q);
    var binary = httpUtil.acceptsMediaType(req, frame.CONTENT_TYPE) || q.format === 'binary';
    var v = {
      format: binary ? (q.encoding === 'f32' ? 'binary-f32' : 'binary-i16') : 'json',
      maxPoints: decimate.normaliseMaxPoints(q.maxPoints),
      gzip: !binary && httpUtil.acceptsEncoding(req, 'gzip') // binary spectrum is mostly noise: gzip saves ~10 % for a lot of CPU
    };
    var waitMs = Math.max(0, Math.min(10000, parseInt(q.waitMs, 10) || 0));
    function tagOf(sweep) { return '"' + sweep.tagBase + '-' + v.format + '-' + v.maxPoints + (v.gzip ? '-gz' : '') + '"'; }
    function send(sweep) {
      return session.variant(sweep, v).then(function (out) {
        var h = { 'Content-Type': out.contentType, 'Cache-Control': 'no-cache', ETag: out.etag, Vary: 'Accept, Accept-Encoding', 'X-Sweep-Id': String(sweep.sweepId) };
        if (out.encoding === 'gzip') h['Content-Encoding'] = 'gzip';
        httpUtil.sendBuffer(res, 200, out.body, h);
        count('spectrum-latest', 200, out.body.length);
      });
    }
    function notModified(sweep) {
      httpUtil.notModified(res, tagOf(sweep), { 'Cache-Control': 'no-cache', Vary: 'Accept, Accept-Encoding', 'X-Sweep-Id': String(sweep.sweepId) });
      count('spectrum-latest', 304);
    }
    return session.latestOrNext(o.sweepWaitMs)
      .then(function (sweep) {
        if (!httpUtil.ifNoneMatch(req, tagOf(sweep))) return send(sweep);
        // The client already has this sweep: long-poll for the next one if asked,
        // otherwise answer 304 without building anything.
        if (waitMs > 0) return session.nextSweep(waitMs).then(send, function () { notModified(sweep); });
        notModified(sweep);
      })
      .catch(function (err) {
        if (err.code === 'POOL_BUSY' || err.code === 'SWEEP_TIMEOUT') { err.status = 503; err.retryAfter = 1; }
        throw err;
      });
  }

  return { legacy: legacy, latest: latest, sessionFor: sessionFor };
}

/**
 * GET /api/volatile-data (snapshot with ETag/304/gzip, or ?since=<rev> delta).
 * @param {{store: object, gzipLevel: number, count: function}} o
 */
function volatileHandler(o) {
  var store = o.store;
  var count = o.count || function () {};
  return function (req, res) {
    var q = httpUtil.queryOf(req);
    var gz = httpUtil.acceptsEncoding(req, 'gzip');
    if (q.since !== undefined && q.since !== '') {
      var dj = store.deltaJson(q.since);
      if (dj !== null) {
        var body = Buffer.from(dj);
        var headers = { 'Content-Type': JSON_TYPE, 'Cache-Control': 'no-store', 'X-Revision': String(store.rev) };
        if (gz && body.length > 16384) {
          return gzipAsync(body, o.gzipLevel).then(function (z) {
            headers['Content-Encoding'] = 'gzip';
            headers.Vary = 'Accept-Encoding';
            httpUtil.sendBuffer(res, 200, z, headers);
            count('volatile-delta', 200, z.length);
          });
        }
        httpUtil.sendBuffer(res, 200, body, headers);
        count('volatile-delta', 200, body.length);
        return;
      }
      // Unknown, expired or from a previous boot: fall through to a full snapshot.
    }
    return store.snapshotBody(gz).then(function (s) {
      var h = { 'Content-Type': JSON_TYPE, 'Cache-Control': 'no-cache', ETag: s.etag, Vary: 'Accept-Encoding', 'X-Revision': String(s.rev) };
      if (httpUtil.ifNoneMatch(req, s.etag)) { httpUtil.notModified(res, s.etag, h); count('volatile', 304); return; }
      if (s.encoding === 'gzip') h['Content-Encoding'] = 'gzip';
      httpUtil.sendBuffer(res, 200, s.body, h);
      count('volatile', 200, s.body.length);
    });
  };
}

/**
 * GET/PUT /api/nodes/:id/config with ETag / If-Match optimistic locking.
 * @param {{configs: object, log: function, count: function}} o
 */
function configHandlers(o) {
  var configs = o.configs;
  var count = o.count || function () {};
  var log = o.log || function () {};
  function get(req, res) {
    var id = parseInt(req.params[0], 10);
    var c = configs.get(id);
    if (!c) return httpUtil.sendJson(res, 404, { error: 'NOT_FOUND', message: 'unknown node ' + id });
    var etag = configs.etag(id);
    if (httpUtil.ifNoneMatch(req, etag)) return httpUtil.notModified(res, etag, { 'Cache-Control': 'no-cache' });
    httpUtil.sendJson(res, 200, c, { ETag: etag, 'Cache-Control': 'no-cache' });
    count('config-get', 200);
  }
  function put(req, res) {
    var id = parseInt(req.params[0], 10);
    return httpUtil.readJson(req).then(function (body) {
      var pre = configs.has(id) ? httpUtil.ifMatch(req, configs.etag(id)) : null;
      var out = configs.put(id, body, pre);
      var headers = out.status === 200 ? { ETag: configs.etag(id) } : undefined;
      if (out.status === 200) log('config_changed', { nodeId: id, version: out.body.version });
      httpUtil.sendJson(res, out.status, out.body, headers);
      count('config-put', out.status);
    });
  }
  return { get: get, put: put };
}

/** Render counters and gauges as Prometheus text exposition format. */
function promText(gauges, counters) {
  var lines = [];
  Object.keys(gauges).forEach(function (k) {
    if (typeof gauges[k] !== 'number') return;
    lines.push('# TYPE ' + k + ' gauge', k + ' ' + gauges[k]);
  });
  if (counters) {
    lines.push('# TYPE das_http_requests_total counter');
    Object.keys(counters).forEach(function (r) {
      lines.push('das_http_requests_total{route="' + r + '"} ' + counters[r].requests);
      lines.push('das_http_not_modified_total{route="' + r + '"} ' + counters[r].notModified);
      lines.push('das_http_bytes_total{route="' + r + '"} ' + counters[r].bytes);
    });
  }
  return lines.join('\n') + '\n';
}

module.exports = { createCounters: createCounters, spectrumHandlers: spectrumHandlers, volatileHandler: volatileHandler, configHandlers: configHandlers, promText: promText, JSON_TYPE: JSON_TYPE };
