'use strict';
/**
 * volatile_data store: merges Remote Node reports into the Master Node state
 * and serves it without re-serialising the whole blob per request.
 *
 * Techniques
 *  - Incremental serialisation: each node is stringified once when its report
 *    arrives. A full snapshot is a cheap string join of those parts, built at most
 *    once per published revision and shared by every client.
 *  - Change detection: a node only counts as changed when its (normalised) JSON
 *    differs from the last published one. The comparison is exact, like a
 *    checksum with no collision risk.
 *  - Batching: revisions are published every `publishIntervalMs`, so 300 reports/s
 *    become at most one new revision per second.
 *  - ETag / 304 on snapshots (bootId + revision) and deltas (`since=<rev>`).
 *  - Optional normalisation (gateway and LTS projects): field hygiene (drop
 *    per-report counters, publish bootAt instead of uptime) plus per-metric
 *    deadbands, so analog noise does not register as a change.
 *
 * With `normalize: false` the stored node objects are the raw reports, and the
 * snapshot is byte-identical to the legacy JSON.stringify output. With
 * `normalize: true` change detection ignores per-report counters and noise, but
 * the published objects still carry every legacy field (plus `bootAt`).
 *
 * ES2019 / CommonJS (Node 12+).
 */

var EventEmitter = require('events');
var util = require('util');
var zlib = require('zlib');
var emu = require('./emulation');

/**
 * Deadbands in engineering units. Keys are paths; [] = any array index, * = any key.
 * These are product decisions, not constants: agree them with the RF/ops team
 * (typically at or below the display resolution and well below alarm thresholds).
 */
var DEFAULT_DEADBANDS = {
  temperatureC: 0.5,
  fanRpm: 250,
  psuVoltageV: 0.25,
  'optical.rxDbm': 0.3,
  'optical.txDbm': 0.3,
  'optical.laserBiasMa': 1.0,
  'bands[].dlOutDbm': 1.0,
  'bands[].ulInDbm': 3,
  'bands[].vswr': 0.05,
  'metrics.*': 2,
  // A boot time derived from a truncated uptime counter jitters by up to 1 s;
  // a real reboot moves it by far more.
  bootAt: 10000
};

function lookupDeadband(spec, path) {
  if (Object.prototype.hasOwnProperty.call(spec, path)) return spec[path];
  var dot = path.lastIndexOf('.');
  if (dot > 0) {
    var wild = path.slice(0, dot) + '.*';
    if (Object.prototype.hasOwnProperty.call(spec, wild)) return spec[wild];
  }
  return 0;
}

/**
 * Keep the previously published value while the new one stays inside its deadband
 * (the reported value lags the raw value by less than the deadband, the classic
 * SCADA/OPC UA reporting behaviour).
 */
function applyDeadband(prev, next, spec, path) {
  if (typeof next === 'number' && typeof prev === 'number') {
    var db = lookupDeadband(spec, path);
    return db && Math.abs(next - prev) < db ? prev : next;
  }
  if (Array.isArray(next)) {
    var outA = new Array(next.length);
    for (var i = 0; i < next.length; i++) {
      outA[i] = applyDeadband(Array.isArray(prev) ? prev[i] : undefined, next[i], spec, path + '[]');
    }
    return outA;
  }
  if (next && typeof next === 'object') {
    var out = {};
    var keys = Object.keys(next);
    for (var k = 0; k < keys.length; k++) {
      var key = keys[k];
      out[key] = applyDeadband(prev && typeof prev === 'object' ? prev[key] : undefined, next[key], spec, path ? path + '.' + key : key);
    }
    return out;
  }
  return next;
}

/** Field hygiene: drop per-report counters that would defeat change detection. */
function hygiene(report) {
  var out = {};
  var keys = Object.keys(report);
  for (var i = 0; i < keys.length; i++) {
    var k = keys[i];
    if (k === 'seq' || k === 'reportedAt') continue;
    if (k === 'uptimeS') {
      // Publish a stable boot timestamp (10 s granularity) instead of a counter
      // that changes on every report. The UI derives uptime from it.
      var now = typeof report.reportedAt === 'number' ? report.reportedAt : Date.now();
      out.bootAt = Math.round((now - report.uptimeS * 1000) / 1000) * 1000; // jitter absorbed by the bootAt deadband
      continue;
    }
    out[k] = report[k];
  }
  return out;
}

function revisionBase() {
  return Math.pow(2, 40) + Math.floor(Math.random() * Math.pow(2, 51));
}

function gzipAsync(buf, level) {
  return new Promise(function (resolve, reject) {
    zlib.gzip(buf, { level: level }, function (err, out) { if (err) reject(err); else resolve(out); });
  });
}

/**
 * @param {object} opts
 * @param {string} opts.bootId
 * @param {number} [opts.publishIntervalMs=1000]
 * @param {number} [opts.gzipLevel=4]
 * @param {boolean} [opts.normalize=false] field hygiene + deadbands
 * @param {object} [opts.deadbands]
 * @param {number} [opts.staleAfterMs=0] mark nodes offline after this silence (0 = off)
 * @param {number} [opts.historySize=120] revisions kept for delta requests
 * @param {number} [opts.revBase] first revision (default: random per boot; tests use 0)
 */
function VolatileStore(opts) {
  EventEmitter.call(this);
  opts = opts || {};
  this.bootId = opts.bootId || Date.now().toString(36);
  this.publishIntervalMs = opts.publishIntervalMs || 1000;
  this.gzipLevel = opts.gzipLevel == null ? 4 : opts.gzipLevel;
  this.normalize = !!opts.normalize;
  this.deadbands = opts.deadbands || DEFAULT_DEADBANDS;
  this.staleAfterMs = opts.staleAfterMs || 0;
  this.historySize = opts.historySize || 120;
  this.nodes = new Map(); // id -> {obj, json, lastSeen, offline}
  this.sortedIds = null;
  // Revisions start at a random per-boot offset (like a TCP initial sequence
  // number), so a revision a client kept from a previous boot is older than the
  // history (or in the future) and is answered with a snapshot, never with a delta
  // that does not apply to what the client holds. Safe JSON integer (< 2^53).
  this.rev = opts.revBase != null ? opts.revBase : revisionBase();
  this.generatedAt = Date.now();
  this.pending = new Set();
  this.removedPending = new Set();
  this.history = []; // [{rev, ids:[...], removed:[...]}]
  this.snap = null;
  this.timer = null;
  this.stats = { reports: 0, unchangedReports: 0, publishes: 0, snapshotBuilds: 0 };
}
util.inherits(VolatileStore, EventEmitter);

var VOLATILE_FIELDS = ['seq', 'reportedAt', 'uptimeS'];

VolatileStore.prototype.ingest = function (report) {
  this.stats.reports++;
  var id = report.id;
  var entry = this.nodes.get(id);
  var self = this;
  var norm = null;
  var key = emu.heavy(function () {
    if (!self.normalize) return JSON.stringify(report);
    // Change detection runs on the normalised state: no per-report counters,
    // analog values held within their deadband.
    norm = applyDeadband(entry ? entry.norm : undefined, hygiene(report), self.deadbands, '');
    return JSON.stringify(norm);
  });
  var now = Date.now();
  if (entry) {
    entry.lastSeen = now;
    if (entry.key === key && !entry.offline) { this.stats.unchangedReports++; return false; }
  }
  var obj = report;
  var json = key;
  if (this.normalize) {
    // Publish the normalised state plus the legacy per-report fields (as of this
    // publish), so no field existing clients read ever disappears.
    obj = Object.assign({}, norm);
    for (var i = 0; i < VOLATILE_FIELDS.length; i++) if (VOLATILE_FIELDS[i] in report) obj[VOLATILE_FIELDS[i]] = report[VOLATILE_FIELDS[i]];
    json = JSON.stringify(obj);
  }
  if (!entry) {
    this.nodes.set(id, { obj: obj, json: json, key: key, norm: norm, lastSeen: now, offline: false });
    this.sortedIds = null;
  } else {
    entry.obj = obj;
    entry.json = json;
    entry.key = key;
    entry.norm = norm;
    entry.offline = false;
  }
  this.pending.add(id);
  return true;
};

VolatileStore.prototype.remove = function (id) {
  if (!this.nodes.delete(id)) return;
  this.sortedIds = null;
  this.pending.delete(id);
  this.removedPending.add(id);
};

VolatileStore.prototype._checkStale = function (now) {
  if (!this.staleAfterMs) return;
  var self = this;
  this.nodes.forEach(function (e, id) {
    if (!e.offline && now - e.lastSeen > self.staleAfterMs) {
      e.offline = true;
      e.obj = Object.assign({}, e.obj, { status: 'offline' });
      e.json = JSON.stringify(e.obj);
      self.pending.add(id);
    }
  });
};

/** Publish a new revision if anything changed since the last one. */
VolatileStore.prototype.publish = function () {
  var now = Date.now();
  this._checkStale(now);
  if (!this.pending.size && !this.removedPending.size) return null;
  var ids = Array.from(this.pending);
  var removed = Array.from(this.removedPending);
  this.pending.clear();
  this.removedPending.clear();
  var base = this.rev;
  this.rev++;
  this.generatedAt = now;
  this.snap = null;
  this.history.push({ rev: this.rev, ids: ids, removed: removed });
  if (this.history.length > this.historySize) this.history.shift();
  this.stats.publishes++;
  var ev = { rev: this.rev, base: base, ids: ids, removed: removed, generatedAt: now };
  this.emit('publish', ev);
  return ev;
};

VolatileStore.prototype.start = function () {
  if (this.timer) return this;
  var self = this;
  this.timer = setInterval(function () { self.publish(); }, this.publishIntervalMs);
  if (this.timer.unref) this.timer.unref();
  return this;
};

VolatileStore.prototype.stop = function () { clearInterval(this.timer); this.timer = null; };

VolatileStore.prototype._ids = function () {
  if (!this.sortedIds) this.sortedIds = Array.from(this.nodes.keys()).sort(function (a, b) { return a - b; });
  return this.sortedIds;
};

VolatileStore.prototype.etag = function (gzip) {
  return '"vd-' + this.bootId + '-' + this.rev + (gzip ? '-gz' : '') + '"';
};

/** JSON text of the published snapshot. Same bytes as JSON.stringify({rev, generatedAt, nodeCount, nodes}). */
VolatileStore.prototype._buildSnapshotJson = function () {
  var self = this;
  var ids = this._ids();
  this.stats.snapshotBuilds++;
  return emu.heavy(function () {
    var parts = new Array(ids.length);
    for (var i = 0; i < ids.length; i++) parts[i] = '"' + ids[i] + '":' + self.nodes.get(ids[i]).json;
    return '{"rev":' + self.rev + ',"generatedAt":' + self.generatedAt + ',"nodeCount":' + ids.length + ',"nodes":{' + parts.join(',') + '}}';
  });
};

/** Snapshot JSON text for the current revision, built at most once per revision. */
VolatileStore.prototype.snapshotJson = function () {
  if (!this._snapJson || this._snapJson.rev !== this.rev) this._snapJson = { rev: this.rev, json: this._buildSnapshotJson() };
  return this._snapJson.json;
};

/**
 * Snapshot body for the current revision, built once and shared by all requests.
 * @param {boolean} gzip
 * @returns {Promise<{body:Buffer, etag:string, rev:number, encoding:string}>}
 */
VolatileStore.prototype.snapshotBody = function (gzip) {
  if (!this.snap || this.snap.rev !== this.rev) this.snap = { rev: this.rev, identity: null, gzip: null };
  var snap = this.snap;
  var self = this;
  if (!snap.identity) snap.identity = Promise.resolve(Buffer.from(this.snapshotJson()));
  if (!gzip) return snap.identity.then(function (b) { return { body: b, etag: '"vd-' + self.bootId + '-' + snap.rev + '"', rev: snap.rev, encoding: 'identity' }; });
  if (!snap.gzip) {
    // zlib's async API runs in the libuv threadpool, off the event loop.
    snap.gzip = snap.identity.then(function (b) { return emu.heavyAsync(function () { return gzipAsync(b, self.gzipLevel); }); });
    snap.gzip.catch(function () { snap.gzip = null; });
  }
  return snap.gzip.then(function (b) { return { body: b, etag: '"vd-' + self.bootId + '-' + snap.rev + '-gz"', rev: snap.rev, encoding: 'gzip' }; });
};

/**
 * Delta since a revision the client already has.
 * @returns {string|null} JSON text, or null when the client must take a full snapshot
 */
VolatileStore.prototype.deltaJson = function (since) {
  since = Number(since);
  if (!isFinite(since) || since > this.rev || since < 0) return null;
  if (since === this.rev) return '{"rev":' + this.rev + ',"base":' + since + ',"generatedAt":' + this.generatedAt + ',"changed":{},"removed":[]}';
  var oldest = this.history.length ? this.history[0].rev : this.rev + 1;
  if (since < oldest - 1) return null; // history no longer covers the gap
  var changed = new Set();
  var removed = new Set();
  for (var i = 0; i < this.history.length; i++) {
    var h = this.history[i];
    if (h.rev <= since) continue;
    for (var j = 0; j < h.ids.length; j++) { changed.add(h.ids[j]); removed.delete(h.ids[j]); }
    for (var k = 0; k < h.removed.length; k++) { removed.add(h.removed[k]); changed.delete(h.removed[k]); }
  }
  var ids = Array.from(changed).sort(function (a, b) { return a - b; });
  var parts = [];
  for (var m = 0; m < ids.length; m++) {
    var e = this.nodes.get(ids[m]);
    if (e) parts.push('"' + ids[m] + '":' + e.json);
  }
  return '{"rev":' + this.rev + ',"base":' + since + ',"generatedAt":' + this.generatedAt + ',"changed":{' + parts.join(',') + '},"removed":' + JSON.stringify(Array.from(removed)) + '}';
};

/** Delta JSON for exactly one publish event (used by push transports). */
VolatileStore.prototype.publishDeltaJson = function (ev) {
  var parts = [];
  var ids = ev.ids.slice().sort(function (a, b) { return a - b; });
  for (var i = 0; i < ids.length; i++) {
    var e = this.nodes.get(ids[i]);
    if (e) parts.push('"' + ids[i] + '":' + e.json);
  }
  return '{"rev":' + ev.rev + ',"base":' + ev.base + ',"generatedAt":' + ev.generatedAt + ',"changed":{' + parts.join(',') + '},"removed":' + JSON.stringify(ev.removed) + '}';
};

VolatileStore.prototype.nodeObject = function (id) { var e = this.nodes.get(id); return e ? e.obj : undefined; };
VolatileStore.prototype.size = function () { return this.nodes.size; };

module.exports = { VolatileStore: VolatileStore, applyDeadband: applyDeadband, hygiene: hygiene, DEFAULT_DEADBANDS: DEFAULT_DEADBANDS, revisionBase: revisionBase };
