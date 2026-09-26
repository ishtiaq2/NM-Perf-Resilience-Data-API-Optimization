'use strict';
/**
 * Spectrum sweep sessions: production is decoupled from HTTP requests.
 *
 * Legacy behaviour: every GET triggered a sweep and parsed/serialised the
 * result on the event loop, so N pollers meant N times the CPU on the main thread.
 *
 * Here each analyzer configuration (node, port, span, points) has one session.
 * While anyone is watching, the session sweeps in a loop, parses in a worker
 * thread and keeps the latest result as ready-to-send buffers. Requests only
 * read that cache. Load is bounded by the hardware sweep rate, not by the number
 * of clients, and the event loop only ever writes bytes.
 *
 * ES2019 / CommonJS (Node 12+).
 */

var zlib = require('zlib');
var EventEmitter = require('events');
var util = require('util');
var sim = require('./spectrum-sim');
var wp = require('./worker-pool');

function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

function sessionKey(p) { return p.nodeId + ':' + p.port + ':' + p.startHz + ':' + p.stopHz + ':' + p.points; }

/** Short stable hash (FNV-1a 32-bit, base36) for ETags. */
function fnv1a(str) {
  var h = 0x811c9dc5;
  for (var i = 0; i < str.length; i++) { h ^= str.charCodeAt(i); h = Math.imul(h, 0x01000193); }
  return (h >>> 0).toString(36);
}

/**
 * @param {object} opts
 * @param {import('./worker-pool').WorkerPool} opts.pool
 * @param {{sweep:function(object):Promise<Buffer>}} opts.hardware
 * @param {string} opts.bootId process epoch, part of every ETag
 * @param {number} [opts.idleTimeoutMs=15000] stop sweeping when nobody asked for this long
 * @param {number} [opts.minSweepIntervalMs=0] throttle to protect CPU on small devices
 * @param {number} [opts.maxSessions=8]
 * @param {number} [opts.gzipLevel=4]
 * @param {boolean} [opts.buildLegacy=true] build the legacy JSON body for every sweep
 * @param {function(string,object):void} [opts.log]
 */
function SpectrumService(opts) {
  EventEmitter.call(this);
  this.pool = opts.pool;
  this.hardware = opts.hardware;
  this.bootId = opts.bootId;
  this.idleTimeoutMs = opts.idleTimeoutMs || 15000;
  this.minSweepIntervalMs = opts.minSweepIntervalMs || 0;
  this.maxSessions = opts.maxSessions || 8;
  this.gzipLevel = opts.gzipLevel == null ? 4 : opts.gzipLevel;
  this.buildLegacy = opts.buildLegacy !== false;
  this.log = opts.log || function () {};
  this.sessions = new Map();
  this.stats = { sweeps: 0, sweepErrors: 0, variantBuilds: 0, variantHits: 0 };
}
util.inherits(SpectrumService, EventEmitter);

/** Get (or create) the session for a normalised parameter set and mark it as watched. */
SpectrumService.prototype.session = function (params) {
  var p = sim.clampParams(params);
  var key = sessionKey(p);
  var s = this.sessions.get(key);
  if (!s) {
    if (this.sessions.size >= this.maxSessions) this._evictOne();
    if (this.sessions.size >= this.maxSessions) {
      var e = new Error('too many concurrent analyzer sessions');
      e.code = 'TOO_MANY_SESSIONS';
      throw e;
    }
    s = new SweepSession(this, p, key);
    this.sessions.set(key, s);
  }
  s.touch();
  return s;
};

SpectrumService.prototype._evictOne = function () {
  var oldest = null;
  this.sessions.forEach(function (s) { if (!s.running && (!oldest || s.lastAccess < oldest.lastAccess)) oldest = s; });
  if (oldest) this.sessions.delete(oldest.key);
};

SpectrumService.prototype.status = function () {
  var list = [];
  this.sessions.forEach(function (s) {
    list.push({ key: s.key, running: s.running, sweepId: s.latest ? s.latest.sweepId : 0, idleMs: Date.now() - s.lastAccess, lastError: s.lastError || null });
  });
  return { sessions: list, stats: Object.assign({}, this.stats) };
};

SpectrumService.prototype.close = function () {
  this.sessions.forEach(function (s) { s.closed = true; });
  this.sessions.clear();
};

function SweepSession(svc, params, key) {
  this.svc = svc;
  this.params = params;
  this.key = key;
  this.etagBase = svc.bootId + '-' + fnv1a(key);
  this.latest = null;
  this.sweepSeq = 0;
  this.running = false;
  this.closed = false;
  this.lastAccess = Date.now();
  this.lastError = null;
  this.waiters = [];
}

SweepSession.prototype.touch = function () {
  this.lastAccess = Date.now();
  if (!this.running && !this.closed) this._loop();
};

SweepSession.prototype._loop = function () {
  var self = this;
  var svc = this.svc;
  this.running = true;
  (function next() {
    if (self.closed || Date.now() - self.lastAccess > svc.idleTimeoutMs) {
      self.running = false;
      return;
    }
    var started = Date.now();
    svc.hardware.sweep(self.params)
      .then(function (raw) {
        var sweepId = self.sweepSeq + 1;
        var timestamp = Date.now();
        var ab = wp.toTransferable(raw);
        return svc.pool.run('spectrum.parse', { raw: ab, sweepId: sweepId, timestamp: timestamp, legacy: svc.buildLegacy, gzipLevel: svc.gzipLevel }, [ab])
          .then(function (out) {
            self.sweepSeq = sweepId;
            var m = out.meta;
            self.latest = {
              sweepId: sweepId,
              timestamp: timestamp,
              tagBase: 'sp-' + self.etagBase + '-' + sweepId,
              meta: { sweepId: sweepId, nodeId: m.nodeId, port: m.port, timestamp: timestamp },
              startHz: m.startHz,
              stopHz: m.stopHz,
              stepHz: m.stepHz,
              count: m.count,
              power: new Float32Array(out.power),
              legacyGzip: out.legacyGzip ? Buffer.from(out.legacyGzip) : null,
              legacyIdentity: null, // Promise<Buffer>, created on first request without gzip
              variants: new Map()
            };
            self.lastError = null;
            svc.stats.sweeps++;
            var w = self.waiters.splice(0);
            for (var i = 0; i < w.length; i++) w[i].resolve(self.latest);
            svc.emit('sweep', self.key, self.latest);
          });
      })
      .catch(function (err) {
        svc.stats.sweepErrors++;
        self.lastError = err && err.code ? err.code : String(err && err.message || err);
        svc.log('sweep_error', { session: self.key, error: self.lastError });
        return sleep(err && err.code === 'POOL_BUSY' ? 100 : 500);
      })
      .then(function () {
        var wait = svc.minSweepIntervalMs - (Date.now() - started);
        return wait > 0 ? sleep(wait) : null;
      })
      .then(next);
  })();
};

/** Resolve with the next completed sweep (fresh data, like the legacy endpoint). */
SweepSession.prototype.nextSweep = function (timeoutMs) {
  var self = this;
  return new Promise(function (resolve, reject) {
    var w = { resolve: function (v) { clearTimeout(t); resolve(v); } };
    var t = setTimeout(function () {
      var i = self.waiters.indexOf(w);
      if (i >= 0) self.waiters.splice(i, 1);
      var e = new Error('no sweep within ' + timeoutMs + ' ms' + (self.lastError ? ' (' + self.lastError + ')' : ''));
      e.code = 'SWEEP_TIMEOUT';
      reject(e);
    }, timeoutMs);
    self.waiters.push(w);
  });
};

/** Latest sweep, or wait for the first one. */
SweepSession.prototype.latestOrNext = function (timeoutMs) {
  return this.latest ? Promise.resolve(this.latest) : this.nextSweep(timeoutMs);
};

/**
 * Build (once per sweep, single-flight) a wire variant of a sweep.
 * @param {object} sweep entry from `latest`
 * @param {{format:string,maxPoints:number,gzip:boolean}} v
 * @returns {Promise<{body:Buffer,encoding:string,contentType:string,etag:string}>}
 */
SweepSession.prototype.variant = function (sweep, v) {
  var key = v.format + ':' + (v.maxPoints || 0) + ':' + (v.gzip ? 'gz' : 'id');
  var cached = sweep.variants.get(key);
  var svc = this.svc;
  if (cached) { svc.stats.variantHits++; return cached; }
  svc.stats.variantBuilds++;
  // Different content-codings are different representations -> different strong ETags.
  var etag = '"' + sweep.tagBase + '-' + v.format + '-' + (v.maxPoints || 0) + (v.gzip ? '-gz' : '') + '"';
  var p = svc.pool.run('spectrum.encode', {
    meta: sweep.meta,
    startHz: sweep.startHz,
    stepHz: sweep.stepHz,
    power: sweep.power, // copied (structured clone); ~4 bytes/point
    format: v.format,
    maxPoints: v.maxPoints || 0,
    gzip: !!v.gzip,
    gzipLevel: svc.gzipLevel
  }).then(function (out) {
    return { body: Buffer.from(out.body), encoding: out.encoding, contentType: out.contentType, etag: etag, count: out.count, decimated: out.decimated };
  });
  // Do not cache failures (e.g. POOL_BUSY) - the next request retries.
  p.catch(function () { if (sweep.variants.get(key) === p) sweep.variants.delete(key); });
  if (sweep.variants.size >= 12) sweep.variants.delete(sweep.variants.keys().next().value);
  sweep.variants.set(key, p);
  return p;
};

/**
 * Legacy JSON body of a sweep (built and gzipped by the worker at parse time).
 * @returns {Promise<{body:Buffer, encoding:string, etag:string}>}
 */
SweepSession.prototype.legacyBody = function (sweep, gzip) {
  if (!sweep.legacyGzip) return Promise.reject(Object.assign(new Error('legacy body not built'), { code: 'NO_LEGACY_BODY' }));
  if (gzip) return Promise.resolve({ body: sweep.legacyGzip, encoding: 'gzip', etag: '"' + sweep.tagBase + '-legacy-gz"' });
  if (!sweep.legacyIdentity) {
    // Rare (curl, scripts): decompress once per sweep in the libuv pool, off the event loop.
    sweep.legacyIdentity = new Promise(function (resolve, reject) {
      zlib.gunzip(sweep.legacyGzip, function (err, out) { if (err) reject(err); else resolve(out); });
    });
  }
  return sweep.legacyIdentity.then(function (body) { return { body: body, encoding: 'identity', etag: '"' + sweep.tagBase + '-legacy"' }; });
};

// Exposed for tests and for callers that need to gzip small buffers without a worker.
function gzipAsync(buf, level) {
  return new Promise(function (resolve, reject) {
    zlib.gzip(buf, { level: level == null ? 4 : level }, function (err, out) { if (err) reject(err); else resolve(out); });
  });
}

module.exports = { SpectrumService: SpectrumService, SweepSession: SweepSession, sessionKey: sessionKey, fnv1a: fnv1a, gzipAsync: gzipAsync };
