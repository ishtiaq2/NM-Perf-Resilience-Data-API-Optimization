#!/usr/bin/env node
'use strict';
/**
 * Node.js vs Rust on the Master Unit's hot paths, same input, same machine,
 * native speed (no CPU emulation), one thread each:
 *
 *   raw FPGA sweep (DSPR, i16 codes) -> calibrated dBm -> legacy JSON / compact
 *   JSON / DSPC frame / peak-hold decimation / gzip
 *
 * The Node.js side uses the programme's reference modules (bench/node-ref, the
 * same code the shipped backend and the hotfix run) plus a DSPR parser written
 * the idiomatic way (DataView). It also checks that both produce byte-identical
 * legacy JSON from the same raw buffer.
 *
 *   cargo build --release --example dsp-bench && node bench/dsp-compare.js [--points 50001]
 */
const { spawnSync } = require('node:child_process');
const { parseArgs } = require('node:util');
const { performance } = require('node:perf_hooks');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const zlib = require('node:zlib');
const { round2, compactSpectrumJson } = require('./node-ref/formats');
const { encodeFrame, ENCODING } = require('./node-ref/spectrum-frame');
const { peakDecimate } = require('./node-ref/decimate');

const { values: a } = parseArgs({ options: { points: { type: 'string', default: '50001' } } });
const root = path.join(__dirname, '..');
const exe = path.join(root, 'target', 'release', 'examples', 'dsp-bench');
if (!fs.existsSync(exe)) { console.error('run `cargo build --release --example dsp-bench` first'); process.exit(2); }
const run = path.join(root, '.run');
fs.mkdirSync(run, { recursive: true });
const rawFile = path.join(run, `sweep-${a.points}.dspr`);
const rustLegacyFile = path.join(run, `legacy-rust-${a.points}.json`);

// 1. Rust: timings, plus the raw buffer and its legacy JSON for the Node.js side.
const r = spawnSync(exe, ['--points', a.points, '--json', '--write-raw', rawFile, '--write-legacy', rustLegacyFile], { encoding: 'utf8' });
if (r.status !== 0) { console.error(r.stderr); process.exit(1); }
const rust = JSON.parse(r.stdout.trim().split('\n').pop()).results;

// 2. Node.js: the same operations on the same bytes.
function median(fn) {
  for (let i = 0; i < 3; i++) fn();
  const xs = [];
  const t0 = performance.now();
  while (xs.length < 15 || (performance.now() - t0 < 1500 && xs.length < 2000)) {
    const t = performance.now();
    fn();
    xs.push(performance.now() - t);
  }
  xs.sort((x, y) => x - y);
  return xs[xs.length >> 1];
}

/** DSPR v1: 48-byte header, then i16 codes; dBm = code * scale + offset (f32 math, like the DSP). */
function parseDspr(buf) {
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  if (dv.getUint32(0, true) !== 0x52505344 || dv.getUint16(4, true) !== 1) throw new Error('not a DSPR v1 buffer');
  const count = dv.getUint32(12, true);
  const h = { seq: dv.getUint32(8, true), count, startHz: dv.getFloat64(16, true), stepHz: dv.getFloat64(24, true),
    scale: dv.getFloat32(32, true), offset: dv.getFloat32(36, true) };
  const power = new Float32Array(count);
  for (let i = 0; i < count; i++) {
    const code = dv.getInt16(48 + i * 2, true);
    power[i] = code === -32768 ? NaN : Math.fround(Math.fround(code * h.scale) + h.offset);
  }
  return { h, power };
}

/** What the shipped backend does: an array of objects, then JSON.stringify. */
function legacyJson(h, power) {
  const points = new Array(power.length);
  for (let i = 0; i < power.length; i++) points[i] = { frequency: Math.round(h.startHz + i * h.stepHz), power: round2(power[i]) };
  return JSON.stringify({ sweepId: 1, nodeId: 1, port: 1, timestamp: 1700000000000, startHz: 700e6, stopHz: 2700e6, rbwHz: 30000, points });
}

const raw = fs.readFileSync(rawFile);
const { h, power } = parseDspr(raw);
const legacy = Buffer.from(legacyJson(h, power));
const meta = { sweepId: 1, nodeId: 1, port: 1, startHz: h.startHz, stepHz: h.stepHz, timestampMs: 1.7e12, timestamp: 1700000000000, decimated: false };
const node = [
  median(() => parseDspr(raw)),
  median(() => Buffer.from(legacyJson(h, power))),
  median(() => compactSpectrumJson(meta, { power, startHz: h.startHz, stepHz: h.stepHz, decimated: false })),
  median(() => encodeFrame(meta, power, ENCODING.I16_CENTI_DBM)),
  median(() => peakDecimate(power, h.startHz, h.stepHz, 1024)),
  median(() => zlib.gzipSync(legacy, { level: 1 }))
];

const sha = (b) => crypto.createHash('sha256').update(b).digest('hex');
const identical = sha(legacy) === sha(fs.readFileSync(rustLegacyFile));
const rows = node.map((ms, i) => ({ op: rust[i].op, nodeMs: Math.round(ms * 1000) / 1000, rustMs: rust[i].ms, ratio: Math.round((ms / rust[i].ms) * 10) / 10 }));

console.log(`Node.js ${process.version} vs Rust, ${a.points}-point sweep, native speed, one thread, median of repeated runs`);
console.log(`${'operation'.padEnd(46)} ${'Node.js'.padStart(10)} ${'Rust'.padStart(10)}  Node/Rust`);
for (const x of rows) console.log(`${x.op.padEnd(46)} ${x.nodeMs.toFixed(3).padStart(7)} ms ${x.rustMs.toFixed(3).padStart(7)} ms  ${String(x.ratio).padStart(6)}x`);
for (const x of rust.slice(node.length)) console.log(`${x.op.padEnd(46)} ${'-'.padStart(10)} ${x.ms.toFixed(3).padStart(7)} ms   (no Node.js implementation)`);
console.log(`legacy JSON byte-identical (sha256): ${identical ? 'yes' : 'NO'} (${legacy.length} bytes)`);

const dir = path.join(root, 'bench', 'results');
fs.mkdirSync(dir, { recursive: true });
const out = { measured: new Date().toISOString(), cpu: (os.cpus()[0] || {}).model, node: process.version, points: Number(a.points), legacyByteIdentical: identical, rows, rustOnly: rust.slice(node.length) };
fs.writeFileSync(path.join(dir, `dsp-compare-${a.points}.json`), JSON.stringify(out, null, 2));
if (!identical) process.exitCode = 1;
