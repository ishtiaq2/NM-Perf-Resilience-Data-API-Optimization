'use strict';
/**
 * Simulated fleet of DAS Remote Nodes reporting telemetry to the Master Node.
 *
 * Each node reports on its own jittered interval (default 1 s). Reports are
 * fresh objects, the same as messages deserialised off the fiber link. Analog
 * values random-walk with realistic noise, and alarms are raised and cleared
 * now and then.
 *
 * Every report carries fields that change on every report (seq, reportedAt,
 * uptimeS), plus analog noise far below anything an engineer cares about. That
 * is why a naive "hash the whole blob" change detector sees a change every
 * time, and why the gateway and LTS projects add field hygiene and deadbands
 * before hashing.
 *
 * ES2019 / CommonJS (Node 12+).
 */

var EventEmitter = require('events');
var util = require('util');
var prng = require('./prng');

var BANDS = ['B28-700', 'B20-800', 'B8-900', 'B3-1800', 'B1-2100', 'B7-2600', 'n78-3500', 'B38-2600TDD'];
var ALARM_CODES = [
  { code: 'TEMP_HIGH', severity: 'minor' },
  { code: 'VSWR_HIGH', severity: 'major' },
  { code: 'OPT_RX_LOW', severity: 'major' },
  { code: 'DL_OVERDRIVE', severity: 'minor' },
  { code: 'FAN_FAIL', severity: 'critical' }
];

function r(rand, lo, hi) { return lo + (hi - lo) * rand(); }

/**
 * @param {object} [opts]
 * @param {number} [opts.nodes=300]
 * @param {number} [opts.reportIntervalMs=1000]
 * @param {number} [opts.bandsPerNode=6]
 * @param {number} [opts.extraMetrics=24] additional numeric metrics per node
 * @param {number} [opts.tickMs=50]
 * @param {number} [opts.seed=42]
 */
function RemoteNodeSimulator(opts) {
  EventEmitter.call(this);
  opts = opts || {};
  this.count = opts.nodes || 300;
  this.reportIntervalMs = opts.reportIntervalMs || 1000;
  this.bandsPerNode = Math.min(BANDS.length, opts.bandsPerNode || 6);
  this.extraMetrics = opts.extraMetrics == null ? 24 : opts.extraMetrics;
  this.tickMs = opts.tickMs || 50;
  this.rand = prng.mulberry32(opts.seed || 42);
  this.gauss = prng.gaussianFactory(this.rand);
  this.timer = null;
  this.reports = 0;
  this.startedAt = Date.now();
  this.nodes = [];
  for (var id = 1; id <= this.count; id++) this.nodes.push(this._initNode(id));
}
util.inherits(RemoteNodeSimulator, EventEmitter);

RemoteNodeSimulator.prototype._initNode = function (id) {
  var rand = this.rand;
  var bands = [];
  for (var b = 0; b < this.bandsPerNode; b++) {
    var dl = r(rand, 27, 33);
    var vs = r(rand, 1.05, 1.35);
    bands.push({
      name: BANDS[b],
      dlOutDbm: dl,
      dlTarget: dl,
      vswrTarget: vs,
      ulInDbm: r(rand, -100, -90),
      dlGainDb: 20 + Math.floor(rand() * 10),
      ulGainDb: 12 + Math.floor(rand() * 8),
      vswr: vs,
      enabled: rand() > 0.05
    });
  }
  var extra = [];
  for (var m = 0; m < this.extraMetrics; m++) { var v0 = r(rand, 0, 100); extra.push({ v: v0, target: v0, noisy: rand() < 0.5 }); }
  var t0 = r(rand, 36, 48);
  var rx0 = r(rand, -9, -5);
  return {
    id: id,
    name: 'RN-' + ('00' + id).slice(-3),
    chain: Math.floor((id - 1) / 16) + 1,
    hop: ((id - 1) % 16) + 1,
    fw: rand() < 0.9 ? '4.2.1' : '4.1.7',
    bootAt: Date.now() - Math.floor(r(rand, 3600, 30 * 86400)) * 1000,
    seq: 0,
    temperatureC: t0,
    tempTarget: t0,
    fanRpm: r(rand, 4200, 6400),
    psuVoltageV: r(rand, 47.6, 48.4),
    opt: { rxDbm: rx0, rxTarget: rx0, txDbm: r(rand, 0.5, 2.5), laserBiasMa: r(rand, 26, 36) },
    bands: bands,
    extra: extra,
    alarms: [],
    nextAt: Date.now() + Math.floor(rand() * this.reportIntervalMs)
  };
};

RemoteNodeSimulator.prototype._evolve = function (n, now) {
  var g = this.gauss, rand = this.rand;
  n.seq++;
  // Mean-reverting random walks: sensor noise around a slowly moving operating point.
  n.temperatureC += g() * 0.06 + (n.tempTarget - n.temperatureC) * 0.02;
  n.fanRpm += g() * 15 + (5300 - n.fanRpm) * 0.05;
  n.psuVoltageV = 48 + g() * 0.05;
  n.opt.rxDbm += g() * 0.02 + (n.opt.rxTarget - n.opt.rxDbm) * 0.05;
  n.opt.txDbm += g() * 0.01 + (1.5 - n.opt.txDbm) * 0.05;
  n.opt.laserBiasMa += g() * 0.04 + (31 - n.opt.laserBiasMa) * 0.05;
  for (var b = 0; b < n.bands.length; b++) {
    var band = n.bands[b];
    band.dlOutDbm += g() * 0.1 + (band.dlTarget - band.dlOutDbm) * 0.1; // follows traffic load
    band.ulInDbm += g() * 0.3 + (-95 - band.ulInDbm) * 0.2;
    band.vswr = Math.max(1.0, band.vswr + g() * 0.003 + (band.vswrTarget - band.vswr) * 0.05);
  }
  for (var m = 0; m < n.extra.length; m++) if (n.extra[m].noisy) n.extra[m].v += g() * 0.15 + (n.extra[m].target - n.extra[m].v) * 0.05;
  // Real events: traffic changes shift DL power, the site warms up or cools down.
  if (rand() < 0.01) n.bands[Math.floor(rand() * n.bands.length)].dlTarget = r(rand, 27, 33);
  if (rand() < 0.003) n.tempTarget = r(rand, 36, 50);

  // Occasional alarm raise / clear.
  if (n.alarms.length && rand() < 0.02) n.alarms.shift();
  if (rand() < 0.0015) {
    var a = ALARM_CODES[Math.floor(rand() * ALARM_CODES.length)];
    var exists = false;
    for (var k = 0; k < n.alarms.length; k++) if (n.alarms[k].code === a.code) exists = true;
    if (!exists) n.alarms.push({ code: a.code, severity: a.severity, since: now });
  }
};

function round(v, d) { var p = Math.pow(10, d); return Math.round(v * p) / p; }

/** Build the wire report (a fresh object, as if just deserialised from the fiber link). */
RemoteNodeSimulator.prototype._report = function (n, now) {
  var bands = new Array(n.bands.length);
  for (var b = 0; b < n.bands.length; b++) {
    var s = n.bands[b];
    bands[b] = {
      name: s.name,
      enabled: s.enabled,
      dlOutDbm: round(s.dlOutDbm, 2),
      ulInDbm: round(s.ulInDbm, 2),
      dlGainDb: s.dlGainDb,
      ulGainDb: s.ulGainDb,
      vswr: round(s.vswr, 3)
    };
  }
  var metrics = {};
  for (var m = 0; m < n.extra.length; m++) metrics['m' + ('0' + m).slice(-2)] = round(n.extra[m].v, 2);
  var alarms = new Array(n.alarms.length);
  for (var k = 0; k < n.alarms.length; k++) alarms[k] = { code: n.alarms[k].code, severity: n.alarms[k].severity, since: n.alarms[k].since };
  var worst = 'online';
  for (k = 0; k < alarms.length; k++) if (alarms[k].severity !== 'minor') worst = 'degraded';
  return {
    id: n.id,
    name: n.name,
    type: 'remote',
    chain: n.chain,
    hop: n.hop,
    status: worst,
    fw: n.fw,
    seq: n.seq,
    reportedAt: now,
    uptimeS: Math.floor((now - n.bootAt) / 1000),
    temperatureC: round(n.temperatureC, 2),
    fanRpm: Math.round(n.fanRpm),
    psuVoltageV: round(n.psuVoltageV, 3),
    optical: { rxDbm: round(n.opt.rxDbm, 2), txDbm: round(n.opt.txDbm, 2), laserBiasMa: round(n.opt.laserBiasMa, 2) },
    bands: bands,
    metrics: metrics,
    alarms: alarms
  };
};

RemoteNodeSimulator.prototype.tick = function () {
  var now = Date.now();
  for (var i = 0; i < this.nodes.length; i++) {
    var n = this.nodes[i];
    if (n.nextAt > now) continue;
    this._evolve(n, now);
    this.reports++;
    this.emit('report', this._report(n, now));
    n.nextAt = now + Math.floor(this.reportIntervalMs * (0.9 + 0.2 * this.rand()));
  }
};

/** Emit one report per node immediately (initial sync), then start the periodic timer. */
RemoteNodeSimulator.prototype.start = function () {
  if (this.timer) return this;
  var now = Date.now();
  for (var i = 0; i < this.nodes.length; i++) this.emit('report', this._report(this.nodes[i], now));
  var self = this;
  this.timer = setInterval(function () { self.tick(); }, this.tickMs);
  if (this.timer.unref) this.timer.unref();
  return this;
};

RemoteNodeSimulator.prototype.stop = function () {
  if (this.timer) clearInterval(this.timer);
  this.timer = null;
};

module.exports = { RemoteNodeSimulator: RemoteNodeSimulator, BANDS: BANDS };
