'use strict';
/**
 * Simulated RF spectrum-analyzer hardware.
 *
 * It stands in for the real driver: a sweep takes `sweepTimeMs` of wall-clock time
 * (asynchronous, no CPU), sweeps on the same analyzer (nodeId:port) are serialised
 * like on real hardware, and the result is delivered as a raw JSON byte buffer:
 *
 *   {"analyzer":{"nodeId":1,"port":1,"rbwHz":30000},
 *    "sweep":{"seq":17,"startHz":700000000,"stopHz":2700000000,"points":50001},
 *    "data":[[700000000,-99.12],[700040000,-98.87], ...]}
 *
 * The spectrum contains a noise floor, typical EU downlink carriers
 * (B28/B20/B8/B3/B1/B7), a few narrow CW spurs (1-2 bins wide, which is why
 * decimation must use a peak detector) and an intermittent interferer.
 *
 * Templates are generated once per sweep configuration and reused round-robin,
 * so generating the data costs no CPU per sweep. Real bytes come off a socket
 * or a device file in the same way.
 *
 * ES2019 / CommonJS (Node 12+).
 */

var prng = require('./prng');

var DEFAULT_PARAMS = { startHz: 700e6, stopHz: 2700e6, points: 50001, rbwHz: 30000 };

var CARRIERS = [
  { band: 'B28', center: 773e6, bw: 10e6, level: -62 },
  { band: 'B28', center: 783e6, bw: 10e6, level: -64 },
  { band: 'B20', center: 796e6, bw: 10e6, level: -58 },
  { band: 'B20', center: 806e6, bw: 10e6, level: -60 },
  { band: 'B20', center: 816e6, bw: 10e6, level: -63 },
  { band: 'B8', center: 935.4e6, bw: 0.2e6, level: -57 },
  { band: 'B8', center: 936.2e6, bw: 0.2e6, level: -59 },
  { band: 'B8', center: 937.0e6, bw: 0.2e6, level: -58 },
  { band: 'B8', center: 947.6e6, bw: 5e6, level: -61 },
  { band: 'B8', center: 955.0e6, bw: 5e6, level: -63 },
  { band: 'B3', center: 1815e6, bw: 20e6, level: -64 },
  { band: 'B3', center: 1840e6, bw: 20e6, level: -62 },
  { band: 'B3', center: 1867.5e6, bw: 15e6, level: -66 },
  { band: 'B1', center: 2120e6, bw: 20e6, level: -63 },
  { band: 'B1', center: 2142.5e6, bw: 15e6, level: -61 },
  { band: 'B1', center: 2160e6, bw: 10e6, level: -65 },
  { band: 'B7', center: 2630e6, bw: 20e6, level: -60 },
  { band: 'B7', center: 2655e6, bw: 20e6, level: -62 },
  { band: 'B7', center: 2675e6, bw: 20e6, level: -64 }
];

var SPURS = [
  { f: 1001.3e6, level: -79 },
  { f: 1500.0e6, level: -86 },
  { f: 2400.0e6, level: -81 }
];

var INTERFERER = { f: 1890e6, bw: 0.4e6, level: -74 };
var NOISE_FLOOR_DBM = -100;

function clampParams(p) {
  p = p || {};
  var startHz = Number(p.startHz) || DEFAULT_PARAMS.startHz;
  var stopHz = Number(p.stopHz) || DEFAULT_PARAMS.stopHz;
  var points = parseInt(p.points, 10) || DEFAULT_PARAMS.points;
  if (stopHz <= startHz) throw new RangeError('stopHz must be greater than startHz');
  points = Math.max(101, Math.min(200001, points));
  return {
    nodeId: parseInt(p.nodeId, 10) || 1,
    port: parseInt(p.port, 10) || 1,
    startHz: startHz,
    stopHz: stopHz,
    points: points,
    rbwHz: Number(p.rbwHz) || DEFAULT_PARAMS.rbwHz
  };
}

/** Clean (noise-free) spectrum profile for a sweep configuration. */
function baselineProfile(startHz, stopHz, points) {
  var step = (stopHz - startHz) / (points - 1);
  var base = new Float32Array(points);
  base.fill(NOISE_FLOOR_DBM);
  var c, i, f, d, half, edge, t, lvl, i0, i1;
  for (var k = 0; k < CARRIERS.length; k++) {
    c = CARRIERS[k];
    half = c.bw / 2;
    edge = Math.max(c.bw * 0.03, 2 * step);
    i0 = Math.max(0, Math.floor((c.center - half - edge - startHz) / step));
    i1 = Math.min(points - 1, Math.ceil((c.center + half + edge - startHz) / step));
    for (i = i0; i <= i1; i++) {
      f = startHz + i * step;
      d = Math.abs(f - c.center);
      if (d <= half - edge) lvl = c.level;
      else if (d <= half + edge) {
        t = (d - (half - edge)) / (2 * edge);
        lvl = c.level + (NOISE_FLOOR_DBM - c.level) * (1 - Math.cos(Math.PI * t)) / 2;
      } else continue;
      if (lvl > base[i]) base[i] = lvl;
    }
  }
  for (k = 0; k < SPURS.length; k++) {
    i = Math.round((SPURS[k].f - startHz) / step);
    if (i >= 0 && i < points) base[i] = Math.max(base[i], SPURS[k].level);
  }
  return base;
}

/**
 * Generate one noisy sweep (Float32Array dBm).
 * @param {Float32Array} base
 * @param {number} startHz
 * @param {number} stepHz
 * @param {function():number} rand
 * @param {boolean} withInterferer
 */
function noisySweep(base, startHz, stepHz, rand, withInterferer) {
  var gaussian = prng.gaussianFactory(rand);
  var n = base.length;
  var out = new Float32Array(n);
  for (var i = 0; i < n; i++) {
    var b = base[i];
    // Noise floor fluctuates more than a carrier's flat top.
    out[i] = b <= NOISE_FLOOR_DBM + 0.5 ? b + gaussian() * 1.6 : b + gaussian() * 0.45;
  }
  if (withInterferer) {
    var i0 = Math.max(0, Math.floor((INTERFERER.f - INTERFERER.bw / 2 - startHz) / stepHz));
    var i1 = Math.min(n - 1, Math.ceil((INTERFERER.f + INTERFERER.bw / 2 - startHz) / stepHz));
    var lvl = INTERFERER.level + (rand() - 0.5) * 6;
    for (var j = i0; j <= i1; j++) out[j] = Math.max(out[j], lvl + gaussian() * 0.8);
  }
  return out;
}

/** Serialise the "data" array the way a C driver daemon would print it. */
function rawDataJson(startHz, stepHz, power) {
  var parts = new Array(power.length);
  for (var i = 0; i < power.length; i++) {
    parts[i] = '[' + Math.round(startHz + i * stepHz) + ',' + power[i].toFixed(2) + ']';
  }
  return '[' + parts.join(',') + ']';
}

/**
 * @param {object} [opts]
 * @param {number} [opts.sweepTimeMs=250] physical sweep duration
 * @param {number} [opts.templates=4] pre-generated sweeps per configuration
 * @param {number} [opts.seed=7]
 */
function SimulatedSpectrumHardware(opts) {
  opts = opts || {};
  this.sweepTimeMs = opts.sweepTimeMs == null ? 250 : Number(opts.sweepTimeMs);
  this.templateCount = opts.templates || 4;
  this.seed = opts.seed || 7;
  this.models = new Map();
  this.queues = new Map();
  this.sweepsStarted = 0;
}

SimulatedSpectrumHardware.prototype._model = function (p) {
  var key = p.nodeId + ':' + p.port + ':' + p.startHz + ':' + p.stopHz + ':' + p.points;
  var m = this.models.get(key);
  if (m) return m;
  var stepHz = (p.stopHz - p.startHz) / (p.points - 1);
  var base = baselineProfile(p.startHz, p.stopHz, p.points);
  var rand = prng.mulberry32(this.seed * 7919 + p.nodeId * 31 + p.port);
  var templates = [];
  for (var t = 0; t < this.templateCount; t++) {
    var power = noisySweep(base, p.startHz, stepHz, rand, t % 3 === 1);
    templates.push(Buffer.from(rawDataJson(p.startHz, stepHz, power)));
  }
  m = { key: key, params: p, stepHz: stepHz, templates: templates, next: 0, seq: 0 };
  if (this.models.size >= 16) this.models.delete(this.models.keys().next().value);
  this.models.set(key, m);
  return m;
};

/**
 * Build the sweep templates up front (one-time CPU cost at start-up instead of
 * on the first request). Real hardware produces the data on its DSP/FPGA; the
 * simulator should not show up as event-loop blocking.
 * @param {Array<object>} list parameter sets
 */
SimulatedSpectrumHardware.prototype.prewarm = function (list) {
  for (var i = 0; i < list.length; i++) this._model(clampParams(list[i]));
  return this;
};

/**
 * Run one physical sweep.
 * @param {object} params {nodeId, port, startHz, stopHz, points, rbwHz}
 * @returns {Promise<Buffer>} raw driver output, an exclusively-owned buffer
 */
SimulatedSpectrumHardware.prototype.sweep = function (params) {
  var self = this;
  var p = clampParams(params);
  var analyzerKey = p.nodeId + ':' + p.port;
  var prev = this.queues.get(analyzerKey) || Promise.resolve();
  var run = prev.then(function () {
    var model = self._model(p);
    self.sweepsStarted++;
    return new Promise(function (resolve) {
      setTimeout(function () {
        model.seq++;
        var tpl = model.templates[model.next];
        model.next = (model.next + 1) % model.templates.length;
        var head = Buffer.from(
          '{"analyzer":{"nodeId":' + p.nodeId + ',"port":' + p.port + ',"rbwHz":' + p.rbwHz + '},' +
          '"sweep":{"seq":' + model.seq + ',"startHz":' + p.startHz + ',"stopHz":' + p.stopHz + ',"points":' + p.points + '},' +
          '"data":'
        );
        var tail = Buffer.from('}');
        // Buffer.concat returns a fresh buffer; copy into a standalone ArrayBuffer
        // so it can be transferred to a worker thread without another copy.
        var total = head.length + tpl.length + tail.length;
        var out = Buffer.from(new ArrayBuffer(total));
        head.copy(out, 0);
        tpl.copy(out, head.length);
        tail.copy(out, head.length + tpl.length);
        resolve(out);
      }, self.sweepTimeMs);
    });
  });
  // Keep the queue alive even if a sweep fails.
  this.queues.set(analyzerKey, run.catch(function () {}));
  return run;
};

module.exports = {
  SimulatedSpectrumHardware: SimulatedSpectrumHardware,
  clampParams: clampParams,
  baselineProfile: baselineProfile,
  noisySweep: noisySweep,
  rawDataJson: rawDataJson,
  DEFAULT_PARAMS: DEFAULT_PARAMS,
  CARRIERS: CARRIERS,
  SPURS: SPURS
};
