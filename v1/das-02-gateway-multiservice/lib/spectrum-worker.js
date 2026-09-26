'use strict';
/**
 * Worker-thread script: all CPU-heavy spectrum work happens here, never on the
 * event loop.
 *
 *  spectrum.parse   raw driver bytes -> canonical Float32Array (+ legacy JSON, gzipped once)
 *  sys.memory       heap statistics of this worker (for /api/metrics)
 *  spectrum.encode  canonical -> requested wire variant (binary/compact JSON, decimated, gzipped)
 *
 * Test-only tasks (test.*) let the unit tests exercise crash, timeout and
 * transfer behaviour.
 *
 * ES2019 / CommonJS (Node 12+).
 */

var zlib = require('zlib');
var pool = require('./worker-pool');
var formats = require('./formats');
var frame = require('./spectrum-frame');
var decimate = require('./decimate');
var emu = require('./emulation');

function gzip(buf, level) {
  return zlib.gzipSync(buf, { level: level == null ? 4 : level });
}

var handlers = {
  'spectrum.parse': function (p) {
    // emu.heavy() repeats the work only when DAS_CPU_SLOWDOWN > 1 (demo on a fast PC).
    return emu.heavy(function () {
      var raw = JSON.parse(Buffer.from(p.raw).toString('utf8'));
      var c = formats.canonicalFromRaw(raw);
      var result = {
        meta: { nodeId: c.nodeId, port: c.port, rbwHz: c.rbwHz, hwSeq: c.hwSeq, startHz: c.startHz, stopHz: c.stopHz, stepHz: c.stepHz, count: c.count },
        power: c.power.buffer,
        legacyGzip: null
      };
      var transfer = [c.power.buffer];
      if (p.legacy) {
        // Exactly what the legacy handler produced, so shipped clients see identical bytes.
        // Only the gzip form is kept (every browser accepts it); the rare client
        // without gzip gets it decompressed on demand in the libuv pool.
        var json = Buffer.from(JSON.stringify(formats.legacySpectrumBody(raw, { sweepId: p.sweepId, timestamp: p.timestamp })));
        result.legacyGzip = pool.toTransferable(gzip(json, p.gzipLevel));
        transfer.push(result.legacyGzip);
      }
      return { result: result, transfer: transfer };
    });
  },

  'spectrum.encode': function (p) {
    var power = p.power instanceof Float32Array ? p.power : new Float32Array(p.power);
    return emu.heavy(function () {
      var d = decimate.peakDecimate(power, p.startHz, p.stepHz, p.maxPoints || 0);
      var body, contentType;
      if (p.format === 'binary-i16' || p.format === 'binary-f32') {
        var meta = { sweepId: p.meta.sweepId, nodeId: p.meta.nodeId, port: p.meta.port, startHz: d.startHz, stepHz: d.stepHz, timestampMs: p.meta.timestamp, decimated: d.decimated };
        body = Buffer.from(frame.encodeFrame(meta, d.power, p.format === 'binary-f32' ? frame.ENCODING.F32_DBM : frame.ENCODING.I16_CENTI_DBM));
        contentType = frame.CONTENT_TYPE;
      } else {
        body = Buffer.from(formats.compactSpectrumJson(p.meta, d));
        contentType = 'application/json; charset=utf-8';
      }
      var encoding = 'identity';
      if (p.gzip) { body = gzip(body, p.gzipLevel); encoding = 'gzip'; }
      var ab = pool.toTransferable(body);
      return { result: { body: ab, encoding: encoding, contentType: contentType, count: d.power.length, decimated: d.decimated }, transfer: [ab] };
    });
  },

  // Memory of this worker's V8 isolate (reported by /api/metrics).
  'sys.memory': function () {
    var v8 = require('v8');
    var h = v8.getHeapStatistics();
    return { result: { heapTotal: h.total_heap_size, heapUsed: h.used_heap_size, external: h.external_memory, mallocedMemory: h.malloced_memory } };
  },

  // ---- test helpers -------------------------------------------------------
  'test.echo': function (p) { return { result: p }; },
  'test.spin': function (p) { var t = Date.now(); while (Date.now() - t < p.ms) { /* burn CPU */ } return { result: { spunMs: Date.now() - t } }; },
  'test.crash': function () { setImmediate(function () { throw new Error('boom'); }); return new Promise(function () {}); },
  'test.transfer': function (p) {
    var inLen = p.buf.byteLength;
    var out = new ArrayBuffer(inLen);
    new Uint8Array(out).set(new Uint8Array(p.buf));
    return { result: { inLen: inLen, out: out }, transfer: [out] };
  },
  'test.nice': function () { return { result: { nice: require('os').getPriority() } }; }
};

pool.serveTasks(handlers);
