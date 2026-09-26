'use strict';
/**
 * Peak-detector decimation for spectrum traces.
 *
 * A browser chart cannot show more points than it has horizontal pixels, so
 * sending 50 000 points to a 1 600 px canvas wastes bandwidth and CPU on both
 * ends. Averaging or picking every n-th point would hide narrow carriers and
 * spurs, which are exactly what an RF engineer is looking for. Like a real
 * spectrum analyzer's "positive peak" detector, we keep the maximum of every
 * bucket so no peak is lost.
 *
 * ES2019 / CommonJS (Node 12+).
 */

var MIN_POINTS = 16;
var MAX_POINTS = 1 << 16;

/**
 * Normalise a client-supplied maxPoints value. Rounds up to a multiple of 64 so
 * the number of distinct cached variants stays small.
 * @param {*} v
 * @returns {number} 0 when no decimation was requested
 */
function normaliseMaxPoints(v) {
  var n = parseInt(v, 10);
  if (!isFinite(n) || n <= 0) return 0;
  n = Math.max(MIN_POINTS, Math.min(MAX_POINTS, n));
  return Math.ceil(n / 64) * 64;
}

/**
 * @param {Float32Array} power
 * @param {number} startHz
 * @param {number} stepHz
 * @param {number} maxPoints 0 = no decimation
 * @returns {{power: Float32Array, startHz: number, stepHz: number, decimated: boolean, bucket: number}}
 */
function peakDecimate(power, startHz, stepHz, maxPoints) {
  var n = power.length;
  if (!maxPoints || n <= maxPoints) {
    return { power: power, startHz: startHz, stepHz: stepHz, decimated: false, bucket: 1 };
  }
  var bucket = Math.ceil(n / maxPoints);
  var m = Math.ceil(n / bucket);
  var out = new Float32Array(m);
  for (var b = 0; b < m; b++) {
    var max = -Infinity;
    var end = Math.min(n, (b + 1) * bucket);
    for (var i = b * bucket; i < end; i++) {
      var v = power[i];
      if (v > max) max = v; // NaN never wins a comparison, so gaps are skipped
    }
    out[b] = max === -Infinity ? NaN : max;
  }
  return {
    power: out,
    // Each output point sits at the centre of its bucket.
    startHz: startHz + ((bucket - 1) * stepHz) / 2,
    stepHz: stepHz * bucket,
    decimated: true,
    bucket: bucket
  };
}

module.exports = { peakDecimate: peakDecimate, normaliseMaxPoints: normaliseMaxPoints, MIN_POINTS: MIN_POINTS, MAX_POINTS: MAX_POINTS };
