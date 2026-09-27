'use strict';
/**
 * DSPC v1 - compact binary spectrum frame (little-endian).
 *
 * The frequency axis is implicit (startHz + i * stepHz), so only the power
 * values travel on the wire. With int16 centi-dBm encoding a 50 001-point
 * sweep is ~100 KB instead of ~2 MB of JSON, and the browser can view it
 * as an Int16Array without parsing anything.
 *
 *  offset size  field
 *  0      u32   magic 0x43505344 ("DSPC" as bytes)
 *  4      u8    version (1)
 *  5      u8    encoding: 1 = int16 centi-dBm, 2 = float32 dBm
 *  6      u16   flags: bit0 = decimated (peak detector)
 *  8      u32   sweepId
 *  12     u32   nodeId
 *  16     u16   port
 *  18     u16   reserved (0)
 *  20     u32   count (number of power values)
 *  24     f64   startHz (frequency of value 0)
 *  32     f64   stepHz
 *  40     f64   timestampMs (epoch ms, sweep completion)
 *  48     ...   payload: count * 2 (int16) or count * 4 (float32) bytes
 *
 * int16 value -32768 means "no data" (decoded as NaN).
 *
 * Runtime code in this file is ES2019 / CommonJS so it runs on Node 12+.
 */

var MAGIC = 0x43505344;
var VERSION = 1;
var HEADER_BYTES = 48;
var ENCODING = { I16_CENTI_DBM: 1, F32_DBM: 2 };
var FLAGS = { DECIMATED: 1 };
var I16_NO_DATA = -32768;
var CONTENT_TYPE = 'application/vnd.das.spectrum';

var LITTLE_ENDIAN_HOST = new Uint8Array(new Uint16Array([1]).buffer)[0] === 1;

/**
 * Encode a sweep into a DSPC v1 frame.
 * @param {{sweepId:number,nodeId:number,port:number,startHz:number,stepHz:number,timestampMs:number,decimated?:boolean}} meta
 * @param {Float32Array|number[]} power dBm values
 * @param {number} [encoding] ENCODING.I16_CENTI_DBM (default) or ENCODING.F32_DBM
 * @returns {ArrayBuffer} a standalone ArrayBuffer (transferable between threads)
 */
function encodeFrame(meta, power, encoding) {
  var enc = encoding || ENCODING.I16_CENTI_DBM;
  if (enc !== ENCODING.I16_CENTI_DBM && enc !== ENCODING.F32_DBM) {
    throw new RangeError('unknown spectrum encoding ' + enc);
  }
  var count = power.length;
  var bytesPer = enc === ENCODING.I16_CENTI_DBM ? 2 : 4;
  var ab = new ArrayBuffer(HEADER_BYTES + count * bytesPer);
  var dv = new DataView(ab);
  dv.setUint32(0, MAGIC, true);
  dv.setUint8(4, VERSION);
  dv.setUint8(5, enc);
  dv.setUint16(6, meta.decimated ? FLAGS.DECIMATED : 0, true);
  dv.setUint32(8, meta.sweepId >>> 0, true);
  dv.setUint32(12, meta.nodeId >>> 0, true);
  dv.setUint16(16, meta.port & 0xffff, true);
  dv.setUint16(18, 0, true);
  dv.setUint32(20, count >>> 0, true);
  dv.setFloat64(24, meta.startHz, true);
  dv.setFloat64(32, meta.stepHz, true);
  dv.setFloat64(40, meta.timestampMs || Date.now(), true);

  var i, v;
  if (LITTLE_ENDIAN_HOST) {
    if (enc === ENCODING.I16_CENTI_DBM) {
      var out16 = new Int16Array(ab, HEADER_BYTES, count);
      for (i = 0; i < count; i++) {
        v = power[i];
        if (v !== v) { out16[i] = I16_NO_DATA; continue; } // NaN
        v = Math.round(v * 100);
        out16[i] = v > 32767 ? 32767 : (v < -32767 ? -32767 : v);
      }
    } else {
      new Float32Array(ab, HEADER_BYTES, count).set(power);
    }
  } else {
    // Big-endian host (rare): write explicitly as little-endian.
    for (i = 0; i < count; i++) {
      v = power[i];
      if (enc === ENCODING.I16_CENTI_DBM) {
        var q = v !== v ? I16_NO_DATA : Math.max(-32767, Math.min(32767, Math.round(v * 100)));
        dv.setInt16(HEADER_BYTES + i * 2, q, true);
      } else {
        dv.setFloat32(HEADER_BYTES + i * 4, v, true);
      }
    }
  }
  return ab;
}

/**
 * Decode a DSPC v1 frame.
 * @param {ArrayBuffer|Uint8Array|Buffer} input
 * @returns {{version:number,encoding:number,decimated:boolean,sweepId:number,nodeId:number,port:number,count:number,startHz:number,stepHz:number,timestampMs:number,power:Float32Array}}
 */
function decodeFrame(input) {
  var ab, base;
  if (input instanceof ArrayBuffer) { ab = input; base = 0; } else { ab = input.buffer; base = input.byteOffset; }
  var len = input.byteLength;
  if (len < HEADER_BYTES) throw new RangeError('frame too short: ' + len);
  var dv = new DataView(ab, base, len);
  if (dv.getUint32(0, true) !== MAGIC) throw new TypeError('bad magic: not a DSPC frame');
  var version = dv.getUint8(4);
  if (version !== VERSION) throw new TypeError('unsupported DSPC version ' + version);
  var enc = dv.getUint8(5);
  var flags = dv.getUint16(6, true);
  var count = dv.getUint32(20, true);
  var bytesPer = enc === ENCODING.I16_CENTI_DBM ? 2 : (enc === ENCODING.F32_DBM ? 4 : 0);
  if (!bytesPer) throw new TypeError('unknown encoding ' + enc);
  if (len < HEADER_BYTES + count * bytesPer) throw new RangeError('truncated payload');

  var power = new Float32Array(count);
  var i;
  var payloadOffset = base + HEADER_BYTES;
  var aligned = payloadOffset % bytesPer === 0;
  if (LITTLE_ENDIAN_HOST && aligned) {
    if (enc === ENCODING.I16_CENTI_DBM) {
      var in16 = new Int16Array(ab, payloadOffset, count);
      for (i = 0; i < count; i++) power[i] = in16[i] === I16_NO_DATA ? NaN : in16[i] / 100;
    } else {
      power.set(new Float32Array(ab, payloadOffset, count));
    }
  } else {
    for (i = 0; i < count; i++) {
      if (enc === ENCODING.I16_CENTI_DBM) {
        var s = dv.getInt16(HEADER_BYTES + i * 2, true);
        power[i] = s === I16_NO_DATA ? NaN : s / 100;
      } else {
        power[i] = dv.getFloat32(HEADER_BYTES + i * 4, true);
      }
    }
  }
  return {
    version: version,
    encoding: enc,
    decimated: (flags & FLAGS.DECIMATED) !== 0,
    sweepId: dv.getUint32(8, true),
    nodeId: dv.getUint32(12, true),
    port: dv.getUint16(16, true),
    count: count,
    startHz: dv.getFloat64(24, true),
    stepHz: dv.getFloat64(32, true),
    timestampMs: dv.getFloat64(40, true),
    power: power
  };
}

module.exports = {
  MAGIC: MAGIC,
  VERSION: VERSION,
  HEADER_BYTES: HEADER_BYTES,
  ENCODING: ENCODING,
  FLAGS: FLAGS,
  I16_NO_DATA: I16_NO_DATA,
  CONTENT_TYPE: CONTENT_TYPE,
  encodeFrame: encodeFrame,
  decodeFrame: decodeFrame
};
