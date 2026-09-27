'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const frame = require('../src/shared/spectrum-frame');
const { peakDecimate, normaliseMaxPoints } = require('../src/shared/decimate');

const meta = { sweepId: 42, nodeId: 7, port: 2, startHz: 700e6, stepHz: 40000, timestampMs: 1790000000000 };

test('DSPC int16 frame round-trips at 0.01 dB resolution and keeps NaN gaps', () => {
  const power = Float32Array.from([-97.12, -60.5, NaN, 12.34, -327.67]);
  const ab = frame.encodeFrame(meta, power);
  assert.equal(ab.byteLength, frame.HEADER_BYTES + power.length * 2);
  const d = frame.decodeFrame(ab);
  assert.equal(d.sweepId, 42);
  assert.equal(d.nodeId, 7);
  assert.equal(d.port, 2);
  assert.equal(d.startHz, 700e6);
  assert.equal(d.stepHz, 40000);
  assert.equal(d.timestampMs, 1790000000000);
  assert.equal(d.count, 5);
  assert.ok(Number.isNaN(d.power[2]));
  for (const i of [0, 1, 3, 4]) assert.ok(Math.abs(d.power[i] - power[i]) < 0.006, `value ${i}`);
});

test('DSPC float32 frame is exact', () => {
  const power = Float32Array.from([-97.123456, -60.5, 3.25]);
  const d = frame.decodeFrame(frame.encodeFrame({ ...meta, decimated: true }, power, frame.ENCODING.F32_DBM));
  assert.equal(d.encoding, frame.ENCODING.F32_DBM);
  assert.equal(d.decimated, true);
  assert.deepEqual(Array.from(d.power), Array.from(power));
});

test('DSPC decodes from an unaligned Buffer slice (e.g. inside a larger network buffer)', () => {
  const power = Float32Array.from([-1, -2, -3]);
  const ab = frame.encodeFrame(meta, power);
  const big = Buffer.alloc(ab.byteLength + 3);
  Buffer.from(ab).copy(big, 3);
  const d = frame.decodeFrame(big.subarray(3));
  assert.deepEqual(Array.from(d.power), [-1, -2, -3]);
});

test('DSPC rejects foreign data', () => {
  assert.throws(() => frame.decodeFrame(new ArrayBuffer(10)), /too short/);
  assert.throws(() => frame.decodeFrame(new ArrayBuffer(64)), /bad magic/);
});

test('peak decimation never loses a one-bin spur', () => {
  const n = 50001;
  const power = new Float32Array(n).fill(-100);
  power[31337] = -40; // narrow CW spur
  const d = peakDecimate(power, 700e6, 40000, 1600);
  assert.ok(d.decimated);
  assert.ok(d.power.length <= 1600);
  assert.equal(Math.max(...d.power), -40);
  // The spur lands in the bucket whose frequency span contains it.
  const idx = Array.from(d.power).indexOf(-40);
  const fSpur = 700e6 + 31337 * 40000;
  assert.ok(Math.abs(d.startHz + idx * d.stepHz - fSpur) <= d.stepHz / 2 + 1);
});

test('decimation is a no-op when the trace already fits', () => {
  const p = new Float32Array(100);
  const d = peakDecimate(p, 1, 2, 1600);
  assert.equal(d.power, p);
  assert.equal(d.decimated, false);
});

test('maxPoints is normalised to a few cacheable values', () => {
  assert.equal(normaliseMaxPoints(undefined), 0);
  assert.equal(normaliseMaxPoints('abc'), 0);
  assert.equal(normaliseMaxPoints('1'), 64);
  assert.equal(normaliseMaxPoints('1599'), 1600);
  assert.equal(normaliseMaxPoints('1601'), 1664);
  assert.equal(normaliseMaxPoints(1e9), 65536);
});
