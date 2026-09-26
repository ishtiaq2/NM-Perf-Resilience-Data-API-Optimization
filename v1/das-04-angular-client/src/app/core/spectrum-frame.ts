/**
 * DSPC v1 binary spectrum frame decoder (contract/spectrum-binary-frame.md).
 * Zero parsing: the payload is viewed in place as an Int16Array or Float32Array.
 */

export const DSPC_CONTENT_TYPE = 'application/vnd.das.spectrum';
const MAGIC = 0x43505344;
const HEADER = 48;

export interface SpectrumTrace {
  sweepId: number;
  nodeId: number;
  port: number;
  startHz: number;
  stepHz: number;
  timestampMs: number;
  decimated: boolean;
  /** dBm; NaN = no data */
  power: Float32Array;
  /** Bytes that crossed the network for this trace (for the stats panel). */
  wireBytes: number;
}

export function decodeFrame(buf: ArrayBuffer): SpectrumTrace {
  if (buf.byteLength < HEADER) throw new Error('DSPC frame too short');
  const dv = new DataView(buf);
  if (dv.getUint32(0, true) !== MAGIC) throw new Error('not a DSPC frame');
  if (dv.getUint8(4) !== 1) throw new Error('unsupported DSPC version ' + dv.getUint8(4));
  const encoding = dv.getUint8(5);
  const count = dv.getUint32(20, true);
  const power = new Float32Array(count);
  if (encoding === 1) {
    if (buf.byteLength < HEADER + count * 2) throw new Error('truncated DSPC frame');
    const raw = new Int16Array(buf, HEADER, count);
    for (let i = 0; i < count; i++) power[i] = raw[i] === -32768 ? NaN : raw[i] / 100;
  } else if (encoding === 2) {
    if (buf.byteLength < HEADER + count * 4) throw new Error('truncated DSPC frame');
    power.set(new Float32Array(buf, HEADER, count));
  } else {
    throw new Error('unknown DSPC encoding ' + encoding);
  }
  return {
    sweepId: dv.getUint32(8, true),
    nodeId: dv.getUint32(12, true),
    port: dv.getUint16(16, true),
    decimated: (dv.getUint16(6, true) & 1) === 1,
    startHz: dv.getFloat64(24, true),
    stepHz: dv.getFloat64(32, true),
    timestampMs: dv.getFloat64(40, true),
    power,
    wireBytes: buf.byteLength
  };
}

/** Peak-detector decimation on the client, for legacy full-resolution traces. */
export function peakDecimate(t: SpectrumTrace, maxPoints: number): SpectrumTrace {
  const n = t.power.length;
  if (!maxPoints || n <= maxPoints) return t;
  const bucket = Math.ceil(n / maxPoints);
  const m = Math.ceil(n / bucket);
  const out = new Float32Array(m);
  for (let b = 0; b < m; b++) {
    let max = -Infinity;
    const end = Math.min(n, (b + 1) * bucket);
    for (let i = b * bucket; i < end; i++) if (t.power[i] > max) max = t.power[i];
    out[b] = max === -Infinity ? NaN : max;
  }
  return { ...t, power: out, decimated: true, startHz: t.startHz + ((bucket - 1) * t.stepHz) / 2, stepHz: t.stepHz * bucket };
}
