'use strict';
/**
 * Response formats shared by the legacy code path and the new code paths.
 *
 * The legacy shapes are what the shipped Angular frontend consumes today. The
 * new code builds them with the same functions (in a worker), so responses
 * stay byte-for-byte identical and the release in the field keeps working.
 *
 * ES2019 / CommonJS (Node 12+).
 */

/** Calibration offset the backend applies to raw analyzer readings (e.g. coupler/cable loss). */
var CAL_OFFSET_DB = 0.35;

function round2(v) { return Math.round(v * 100) / 100; }

/**
 * Legacy GET /api/spectrum response: an array of {frequency, power} objects.
 * This is intentionally the "heavy" shape - ~40 bytes of JSON per point.
 */
function legacySpectrumBody(raw, meta) {
  var data = raw.data;
  var points = new Array(data.length);
  for (var i = 0; i < data.length; i++) {
    points[i] = { frequency: data[i][0], power: round2(data[i][1] + CAL_OFFSET_DB) };
  }
  return {
    sweepId: meta.sweepId,
    nodeId: raw.analyzer.nodeId,
    port: raw.analyzer.port,
    timestamp: meta.timestamp,
    startHz: raw.sweep.startHz,
    stopHz: raw.sweep.stopHz,
    rbwHz: raw.analyzer.rbwHz,
    points: points
  };
}

/**
 * Canonical in-memory representation: implicit frequency axis + Float32Array.
 * Values carry the same calibration and rounding as the legacy shape.
 */
function canonicalFromRaw(raw) {
  var data = raw.data;
  var n = data.length;
  var power = new Float32Array(n);
  for (var i = 0; i < n; i++) power[i] = round2(data[i][1] + CAL_OFFSET_DB);
  var startHz = n > 0 ? data[0][0] : raw.sweep.startHz;
  var stepHz = n > 1 ? (data[n - 1][0] - data[0][0]) / (n - 1) : 0;
  return {
    nodeId: raw.analyzer.nodeId,
    port: raw.analyzer.port,
    rbwHz: raw.analyzer.rbwHz,
    hwSeq: raw.sweep.seq,
    startHz: startHz,
    stopHz: raw.sweep.stopHz,
    stepHz: stepHz,
    count: n,
    power: power
  };
}

/** New compact JSON shape: implicit frequency axis, ~7 bytes per point. */
function compactSpectrumJson(meta, d) {
  var n = d.power.length;
  var parts = new Array(n);
  for (var i = 0; i < n; i++) {
    var v = d.power[i];
    parts[i] = v !== v ? 'null' : String(round2(v));
  }
  return '{"sweepId":' + meta.sweepId +
    ',"nodeId":' + meta.nodeId +
    ',"port":' + meta.port +
    ',"timestamp":' + meta.timestamp +
    ',"startHz":' + d.startHz +
    ',"stepHz":' + d.stepHz +
    ',"count":' + n +
    ',"decimated":' + (d.decimated ? 'true' : 'false') +
    ',"powerDbm":[' + parts.join(',') + ']}';
}

/**
 * Legacy GET /api/volatile-data body. Node ids are integers, so JSON.stringify
 * emits them in ascending numeric order - the incremental serializer relies on this.
 */
function legacyVolatileJson(rev, generatedAt, nodesById) {
  var ids = Object.keys(nodesById);
  return JSON.stringify({ rev: rev, generatedAt: generatedAt, nodeCount: ids.length, nodes: nodesById });
}

module.exports = {
  CAL_OFFSET_DB: CAL_OFFSET_DB,
  round2: round2,
  legacySpectrumBody: legacySpectrumBody,
  canonicalFromRaw: canonicalFromRaw,
  compactSpectrumJson: compactSpectrumJson,
  legacyVolatileJson: legacyVolatileJson
};
