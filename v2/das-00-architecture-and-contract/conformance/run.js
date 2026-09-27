#!/usr/bin/env node
'use strict';
/**
 * DAS API conformance suite: one set of checks, run against every backend
 * implementation (Node hotfix, gateway + services, Go edge, Rust engineering
 * tools). It is the executable form of contract/COMPATIBILITY.md. Optional
 * features are checked only when /api/capabilities advertises them.
 *
 *   node conformance/run.js --base http://127.0.0.1:8080
 *   node conformance/run.js --base https://master-node.local --insecure --skip-slo
 *
 * Zero dependencies; needs Node 22+ (global fetch + WebSocket) on the machine
 * running the checks, not on the device.
 */

const http = require('node:http');
const https = require('node:https');
const { parseArgs } = require('node:util');

const { values: args } = parseArgs({
  options: {
    base: { type: 'string', default: 'http://127.0.0.1:8080' },
    points: { type: 'string', default: '50001' },
    'slo-ms': { type: 'string', default: '250' },
    'slo-seconds': { type: 'string', default: '10' },
    'skip-slo': { type: 'boolean', default: false },
    insecure: { type: 'boolean', default: false },
    verbose: { type: 'boolean', default: false }
  }
});
const BASE = args.base.replace(/\/$/, '');
if (args.insecure) process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const results = [];
let current = '';
function section(name) { current = name; console.log(`\n# ${name}`); }
function record(status, name, detail) {
  results.push({ section: current, name, status, detail });
  const mark = status === 'pass' ? 'ok  ' : status === 'skip' ? 'skip' : 'FAIL';
  console.log(`${mark} ${name}${detail ? ` - ${detail}` : ''}`);
}
async function check(name, fn) {
  try {
    const detail = await fn();
    if (detail && detail.skip) record('skip', name, detail.skip);
    else record('pass', name, typeof detail === 'string' ? detail : '');
  } catch (err) {
    record('fail', name, err && err.message ? err.message : String(err));
    if (args.verbose && err && err.stack) console.log(err.stack);
  }
}
function assert(cond, msg) { if (!cond) throw new Error(msg); }

async function get(path, headers = {}, init = {}) {
  const res = await fetch(BASE + path, { headers, ...init });
  const buf = Buffer.from(await res.arrayBuffer());
  return { status: res.status, headers: res.headers, buf, json: () => JSON.parse(buf.toString('utf8')) };
}

// Raw request (fetch hides Content-Encoding handling and cannot send a forged Origin on upgrades).
function raw(path, headers = {}) {
  const u = new URL(BASE + path);
  const mod = u.protocol === 'https:' ? https : http;
  return new Promise((resolve, reject) => {
    const req = mod.request({ hostname: u.hostname, port: u.port, path: u.pathname + u.search, headers, rejectUnauthorized: !args.insecure });
    req.on('response', (res) => { res.resume(); res.on('end', () => resolve({ status: res.statusCode, headers: res.headers })); });
    req.on('upgrade', (res, socket) => { socket.destroy(); resolve({ status: res.statusCode, headers: res.headers }); });
    req.on('error', reject);
    req.setTimeout(10000, () => req.destroy(new Error('timeout')));
    req.end();
  });
}

function decodeFrame(ab) {
  const dv = new DataView(ab);
  assert(ab.byteLength >= 48, 'frame shorter than header');
  assert(dv.getUint32(0, true) === 0x43505344, 'bad magic');
  assert(dv.getUint8(4) === 1, 'unsupported version');
  const encoding = dv.getUint8(5);
  const count = dv.getUint32(20, true);
  const bytes = encoding === 1 ? 2 : 4;
  assert(ab.byteLength >= 48 + count * bytes, 'truncated payload');
  const power = encoding === 1 ? Array.from(new Int16Array(ab, 48, count), (v) => (v === -32768 ? NaN : v / 100)) : Array.from(new Float32Array(ab, 48, count));
  return { encoding, decimated: (dv.getUint16(6, true) & 1) === 1, sweepId: dv.getUint32(8, true), nodeId: dv.getUint32(12, true), port: dv.getUint16(16, true), count, startHz: dv.getFloat64(24, true), stepHz: dv.getFloat64(32, true), timestampMs: dv.getFloat64(40, true), power };
}

function wsOpen(path) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(BASE.replace(/^http/, 'ws') + path);
    ws.binaryType = 'arraybuffer';
    const queue = [];
    const waiters = [];
    ws.onmessage = (ev) => {
      const msg = typeof ev.data === 'string' ? JSON.parse(ev.data) : ev.data;
      const w = waiters.findIndex((x) => x.pred(msg));
      if (w >= 0) { const [x] = waiters.splice(w, 1); clearTimeout(x.t); x.resolve(msg); } else queue.push(msg);
    };
    ws.onerror = () => reject(new Error('websocket error'));
    ws.onopen = () => resolve({
      ws,
      send: (o) => ws.send(JSON.stringify(o)),
      next: (pred, ms = 10000, what = 'message') => new Promise((res, rej) => {
        const i = queue.findIndex(pred);
        if (i >= 0) { res(queue.splice(i, 1)[0]); return; }
        const t = setTimeout(() => rej(new Error(`no ${what} within ${ms} ms`)), ms);
        waiters.push({ pred, resolve: res, t });
      }),
      close: () => ws.close()
    });
  });
}

async function main() {
  console.log(`DAS conformance suite -> ${BASE}`);
  let caps = null;

  section('core');
  await check('heartbeat: 200 JSON, not cacheable, fast', async () => {
    const t = Date.now();
    const r = await get('/api/heartbeat');
    const ms = Date.now() - t;
    assert(r.status === 200, `status ${r.status}`);
    const j = r.json();
    assert(['ok', 'degraded'].includes(j.status), `status field "${j.status}"`);
    assert(/no-store/.test(r.headers.get('cache-control') || ''), 'Cache-Control must contain no-store');
    assert(ms < 1000, `took ${ms} ms`);
    return `${j.server || 'unknown server'}, ${ms} ms`;
  });
  await check('capabilities: das-v1', async () => {
    const r = await get('/api/capabilities');
    if (r.status === 404) return { skip: 'legacy backend (404) - opt-in checks will be skipped' };
    assert(r.status === 200, `status ${r.status}`);
    caps = r.json();
    assert(caps.api === 'das-v1', `api "${caps.api}"`);
    return Object.entries(caps.features).filter(([, v]) => v).map(([k]) => k).join(', ');
  });
  const f = (caps && caps.features) || {};

  section('volatile-data');
  let snap = null;
  await check('snapshot shape (rev, generatedAt, nodeCount, nodes)', async () => {
    const r = await get('/api/volatile-data', { 'Accept-Encoding': 'gzip' });
    assert(r.status === 200, `status ${r.status}`);
    snap = r.json();
    for (const k of ['rev', 'generatedAt', 'nodeCount', 'nodes']) assert(k in snap, `missing ${k}`);
    assert(Object.keys(snap.nodes).length === snap.nodeCount, 'nodeCount does not match nodes');
    const first = snap.nodes[Object.keys(snap.nodes)[0]];
    assert(first && 'id' in first && 'name' in first && 'status' in first, 'node lacks id/name/status');
    return `${snap.nodeCount} nodes, rev ${snap.rev}`;
  });
  await check('gzip is negotiated', async () => {
    if (!caps) return { skip: 'legacy backend: responses are not compressed' };
    if (!f.volatileDelta) return { skip: 'volatile-data still served by the legacy implementation (volatileDelta not advertised)' };
    const r = await raw('/api/volatile-data', { 'Accept-Encoding': 'gzip' });
    assert(r.headers['content-encoding'] === 'gzip', `Content-Encoding "${r.headers['content-encoding']}"`);
    const p = await raw('/api/volatile-data', {});
    assert(!p.headers['content-encoding'], 'identity requested but got ' + p.headers['content-encoding']);
  });
  await check('ETag + 304 Not Modified', async () => {
    if (!f.etag) return { skip: 'etag not advertised' };
    if (!f.volatileDelta) return { skip: 'volatile-data still served by the legacy implementation (volatileDelta not advertised)' };
    for (let i = 0; i < 6; i++) {
      const a = await get('/api/volatile-data', { 'Accept-Encoding': 'gzip' });
      const tag = a.headers.get('etag');
      assert(tag, 'no ETag header');
      const b = await get('/api/volatile-data', { 'Accept-Encoding': 'gzip', 'If-None-Match': tag });
      if (b.status === 304) return `304 on ${tag}`;
      assert(b.status === 200 && b.headers.get('etag') !== tag, `status ${b.status} with the same ETag`);
      await sleep(50); // revision changed in between; try again
    }
    throw new Error('never got a 304');
  });
  await check('delta via ?since=<rev>', async () => {
    if (!f.volatileDelta) return { skip: 'volatileDelta not advertised' };
    const base = (await get('/api/volatile-data')).json();
    await sleep(1500);
    const d = (await get(`/api/volatile-data?since=${base.rev}`)).json();
    assert(d.base === base.rev, `base ${d.base} != ${base.rev}`);
    assert(d.rev >= base.rev && typeof d.changed === 'object' && Array.isArray(d.removed), 'bad delta shape');
    const future = (await get('/api/volatile-data?since=999999999')).json();
    assert(!('base' in future) && future.nodes, 'unknown revision must return a full snapshot');
    return `${Object.keys(d.changed).length} changed nodes in ${d.rev - base.rev} revisions`;
  });

  section('spectrum');
  const pts = 2001;
  await check('legacy /api/spectrum keeps its shape', async () => {
    const r = await get(`/api/spectrum?nodeId=1&port=1&points=${pts}`, { 'Accept-Encoding': 'gzip' });
    assert(r.status === 200, `status ${r.status}`);
    const j = r.json();
    for (const k of ['sweepId', 'nodeId', 'port', 'timestamp', 'startHz', 'stopHz', 'rbwHz', 'points']) assert(k in j, `missing ${k}`);
    assert(j.points.length === pts, `points ${j.points.length}`);
    assert(typeof j.points[0].frequency === 'number' && typeof j.points[0].power === 'number', 'point shape');
    const k = await get(`/api/spectrum?nodeId=1&port=1&points=${pts}`);
    assert(k.json().sweepId > j.sweepId, 'consecutive polls must return newer sweeps');
  });
  let etagJson = null;
  await check('latest: compact JSON', async () => {
    if (!f.spectrumLatest) return { skip: 'spectrumLatest not advertised' };
    const r = await get(`/api/spectrum/latest?nodeId=1&port=1&points=${pts}`, { Accept: 'application/json' });
    assert(r.status === 200, `status ${r.status}`);
    const j = r.json();
    assert(j.count === pts && j.powerDbm.length === pts, `count ${j.count}`);
    assert(Math.abs(j.stepHz - (2700e6 - 700e6) / (pts - 1)) < 1, `stepHz ${j.stepHz}`);
    etagJson = r.headers.get('etag');
    assert(etagJson, 'no ETag');
  });
  await check('latest: binary DSPC frame', async () => {
    if (!f.spectrumBinary) return { skip: 'spectrumBinary not advertised' };
    const r = await get(`/api/spectrum/latest?nodeId=1&port=1&points=${pts}`, { Accept: 'application/vnd.das.spectrum' });
    assert(r.status === 200, `status ${r.status}`);
    assert((r.headers.get('content-type') || '').startsWith('application/vnd.das.spectrum'), 'content-type');
    const fr = decodeFrame(r.buf.buffer.slice(r.buf.byteOffset, r.buf.byteOffset + r.buf.byteLength));
    assert(fr.count === pts && fr.nodeId === 1 && fr.port === 1, `count ${fr.count} node ${fr.nodeId} port ${fr.port}`);
    assert(fr.power.every((v) => Number.isNaN(v) || (v > -200 && v < 50)), 'implausible power values');
    return `${r.buf.length} bytes for ${pts} points`;
  });
  await check('latest: peak-detector decimation', async () => {
    if (!f.spectrumDecimation) return { skip: 'spectrumDecimation not advertised' };
    const full = await get(`/api/spectrum/latest?nodeId=1&port=1&points=${pts}`, { Accept: 'application/vnd.das.spectrum' });
    const dec = await get(`/api/spectrum/latest?nodeId=1&port=1&points=${pts}&maxPoints=256`, { Accept: 'application/vnd.das.spectrum' });
    const a = decodeFrame(full.buf.buffer.slice(full.buf.byteOffset, full.buf.byteOffset + full.buf.byteLength));
    const b = decodeFrame(dec.buf.buffer.slice(dec.buf.byteOffset, dec.buf.byteOffset + dec.buf.byteLength));
    assert(b.decimated && b.count <= 256, `decimated=${b.decimated} count=${b.count}`);
    if (a.sweepId === b.sweepId) {
      const maxA = Math.max(...a.power.filter((v) => !Number.isNaN(v)));
      const maxB = Math.max(...b.power.filter((v) => !Number.isNaN(v)));
      assert(Math.abs(maxA - maxB) < 0.011, `peak lost: ${maxA} vs ${maxB}`);
    }
    return `${b.count} points, ${dec.buf.length} bytes`;
  });
  await check('latest: 304 for a sweep the client already has', async () => {
    if (!f.spectrumLatest || !f.etag) return { skip: 'not advertised' };
    for (let i = 0; i < 6; i++) {
      const a = await get(`/api/spectrum/latest?nodeId=1&port=1&points=${pts}`, { Accept: 'application/json' });
      const b = await get(`/api/spectrum/latest?nodeId=1&port=1&points=${pts}`, { Accept: 'application/json', 'If-None-Match': a.headers.get('etag') });
      if (b.status === 304) return;
    }
    throw new Error('never got a 304');
  });
  await check('latest: long-poll returns the next sweep', async () => {
    if (!f.spectrumLongPoll) return { skip: 'spectrumLongPoll not advertised' };
    const a = await get(`/api/spectrum/latest?nodeId=1&port=1&points=${pts}`, { Accept: 'application/vnd.das.spectrum' });
    const t = Date.now();
    const b = await get(`/api/spectrum/latest?nodeId=1&port=1&points=${pts}&waitMs=5000`, { Accept: 'application/vnd.das.spectrum', 'If-None-Match': a.headers.get('etag') });
    assert(b.status === 200, `status ${b.status}`);
    const fa = decodeFrame(a.buf.buffer.slice(a.buf.byteOffset, a.buf.byteOffset + a.buf.byteLength));
    const fb = decodeFrame(b.buf.buffer.slice(b.buf.byteOffset, b.buf.byteOffset + b.buf.byteLength));
    assert(fb.sweepId > fa.sweepId, 'not a newer sweep');
    return `waited ${Date.now() - t} ms`;
  });

  section('distance-to-fault');
  let dtfTag = null;
  let dtfId = 0;
  await check('dtf: distance profile and events', async () => {
    if (!f.dtf) return { skip: 'dtf not advertised' };
    const r = await get('/api/dtf?nodeId=1&port=1&maxDistanceM=60');
    assert(r.status === 200, `status ${r.status}`);
    assert(/^application\/json/.test(r.headers.get('content-type') || ''), 'JSON expected');
    assert(/no-cache/.test(r.headers.get('cache-control') || ''), 'Cache-Control must contain no-cache');
    dtfTag = r.headers.get('etag');
    assert(dtfTag, 'ETag missing');
    const j = r.json();
    for (const k of ['measurementId', 'points', 'resolutionM', 'maxRangeM', 'startM', 'binM', 'count', 'returnLossDb', 'events']) assert(k in j, `field ${k} missing`);
    assert(j.maxRangeM >= 60, `maxDistanceM=60 must select enough points (range ${j.maxRangeM} m)`);
    assert(Array.isArray(j.returnLossDb) && j.returnLossDb.length === j.count, 'returnLossDb length != count');
    for (const e of j.events) {
      assert(['distanceM', 'returnLossDb', 'vswr', 'fault'].every((k) => k in e), 'event fields');
      assert(e.distanceM >= 0 && e.distanceM <= j.maxRangeM, `event at ${e.distanceM} m`);
    }
    dtfId = j.measurementId;
    const faults = j.events.filter((e) => e.fault).length;
    return `${j.points} points, ${j.resolutionM} m resolution, ${j.events.length} events (${faults} faults)`;
  });
  await check('dtf: 304 / long-poll for the next measurement', async () => {
    if (!f.dtf || !dtfTag) return { skip: f.dtf ? 'no first measurement' : 'dtf not advertised' };
    const t = Date.now();
    const r = await get('/api/dtf?nodeId=1&port=1&maxDistanceM=60&waitMs=5000', { 'If-None-Match': dtfTag });
    assert(r.status === 200 || r.status === 304, `status ${r.status}`);
    if (r.status === 200) assert(r.json().measurementId > dtfId, 'not a newer measurement');
    return `${r.status === 200 ? 'next measurement' : '304'} after ${Date.now() - t} ms`;
  });
  await check('dtf: invalid parameters are rejected with 400', async () => {
    if (!f.dtf) return { skip: 'dtf not advertised' };
    const r = await get('/api/dtf?nodeId=1&port=1&velocityFactor=2');
    assert(r.status === 400, `status ${r.status}`);
    assert(r.json().error, 'JSON error body expected');
  });

  section('configuration');
  await check('optimistic locking: 200 / 412 / 422 / 404', async () => {
    const g = await get('/api/nodes/7/config');
    assert(g.status === 200, `GET status ${g.status}`);
    const tag = g.headers.get('etag');
    const cur = g.json();
    const put = (body, extra = {}) => fetch(`${BASE}/api/nodes/7/config`, { method: 'PUT', headers: { 'Content-Type': 'application/json', ...extra }, body: JSON.stringify(body) });
    if (f.configOptimisticLocking) {
      assert(tag, 'no ETag on GET');
      const ok = await put({ ...cur.config, notes: 'conformance ' + Date.now() }, { 'If-Match': tag });
      assert(ok.status === 200, `PUT status ${ok.status}`);
      const body = await ok.json();
      assert(body.version === cur.version + 1, `version ${body.version}`);
      const stale = await put({ ...cur.config, dlGainDb: 10 }, { 'If-Match': tag });
      assert(stale.status === 412, `stale PUT status ${stale.status}`);
    }
    const bad = await put({ ...cur.config, dlGainDb: 999 });
    assert(bad.status === 422, `invalid PUT status ${bad.status}`);
    const missing = await get('/api/nodes/99999/config');
    assert(missing.status === 404, `unknown node status ${missing.status}`);
  });

  section('websocket');
  let snapRev = null;
  await check('telemetry: hello + snapshot', async () => {
    if (!f.wsTelemetry) return { skip: 'wsTelemetry not advertised' };
    const c = await wsOpen('/api/ws/telemetry');
    try {
      const hello = await c.next((m) => m.type === 'hello', 5000, 'hello');
      assert(hello.api === 'das-v1' && hello.heartbeatMs > 0, 'bad hello');
      c.send({ type: 'sub' });
      const s = await c.next((m) => m.type === 'snapshot', 10000, 'snapshot');
      assert(s.nodes && s.nodeCount === Object.keys(s.nodes).length, 'bad snapshot');
      snapRev = s.rev;
      const d = await c.next((m) => m.type === 'delta', 15000, 'delta');
      assert(d.base >= snapRev && d.rev > d.base, `delta base ${d.base} rev ${d.rev} after snapshot ${snapRev}`);
      return `snapshot rev ${s.rev} (${s.nodeCount} nodes), first delta ${Object.keys(d.changed).length} nodes`;
    } finally { c.close(); }
  });
  await check('telemetry: resume from a revision', async () => {
    if (!f.wsTelemetry || snapRev == null) return { skip: 'no snapshot revision' };
    const c = await wsOpen('/api/ws/telemetry');
    try {
      c.send({ type: 'sub', rev: snapRev });
      const m = await c.next((x) => x.type === 'delta' || x.type === 'snapshot', 10000, 'delta/snapshot');
      if (m.type === 'delta') assert(m.base === snapRev, `base ${m.base} != ${snapRev}`);
      return m.type === 'delta' ? `catch-up delta ${snapRev} -> ${m.rev}` : 'history expired, got snapshot';
    } finally { c.close(); }
  });
  await check('telemetry: same-origin browser accepted, foreign Origin rejected (CSWSH)', async () => {
    if (!f.wsTelemetry) return { skip: 'wsTelemetry not advertised' };
    const upgrade = { Connection: 'Upgrade', Upgrade: 'websocket', 'Sec-WebSocket-Version': '13', 'Sec-WebSocket-Key': 'dGhlIHNhbXBsZSBub25jZQ==' };
    // A browser always sends Origin; through a gateway the service must still see the browser's Host.
    const same = await raw('/api/ws/telemetry', { ...upgrade, Origin: new URL(BASE).origin });
    assert(same.status === 101, `same-origin upgrade refused with ${same.status} (gateway not forwarding Host?)`);
    const r = await raw('/api/ws/telemetry', { ...upgrade, Origin: 'https://evil.example' });
    assert(r.status !== 101, 'upgrade accepted from a foreign origin');
    return `same-origin 101, foreign ${r.status}`;
  });
  await check('spectrum: binary frames pushed per sweep', async () => {
    if (!f.wsSpectrum) return { skip: 'wsSpectrum not advertised' };
    const c = await wsOpen('/api/ws/spectrum');
    try {
      await c.next((m) => m.type === 'hello', 5000, 'hello');
      c.send({ type: 'sub', nodeId: 2, port: 1, points: pts, maxPoints: 512 });
      await c.next((m) => m.type === 'subscribed', 5000, 'subscribed');
      const a = decodeFrame(await c.next((m) => m instanceof ArrayBuffer, 10000, 'frame'));
      const b = decodeFrame(await c.next((m) => m instanceof ArrayBuffer, 10000, 'second frame'));
      assert(a.nodeId === 2 && b.sweepId > a.sweepId, `sweeps ${a.sweepId} -> ${b.sweepId}`);
      assert(a.count <= 512 && a.decimated, `count ${a.count}`);
      return `frames of ${a.count} points, sweep ${a.sweepId} -> ${b.sweepId}`;
    } finally { c.close(); }
  });

  section('SLO: heartbeat under spectrum load');
  await check(`heartbeat p99 < ${args['slo-ms']} ms while 6 analyzers poll ${args.points}-point sweeps`, async () => {
    if (args['skip-slo']) return { skip: '--skip-slo' };
    let running = true;
    const pollers = Array.from({ length: 6 }, (_, i) => (async () => {
      while (running) {
        try { await get(`/api/spectrum?nodeId=${(i % 3) + 1}&port=1&points=${args.points}`, { 'Accept-Encoding': 'gzip' }); } catch { await sleep(200); }
      }
    })());
    await sleep(1000);
    const lat = [];
    let failures = 0;
    const end = Date.now() + Number(args['slo-seconds']) * 1000;
    while (Date.now() < end) {
      const t = performance.now();
      try {
        const r = await get('/api/heartbeat', {}, { signal: AbortSignal.timeout(3000) });
        if (r.status !== 200) failures++;
      } catch { failures++; }
      lat.push(performance.now() - t);
      await sleep(200);
    }
    running = false;
    await Promise.all(pollers);
    lat.sort((a, b) => a - b);
    const p99 = lat[Math.min(lat.length - 1, Math.floor(lat.length * 0.99))];
    assert(failures === 0, `${failures} heartbeat failures`);
    assert(p99 < Number(args['slo-ms']), `p99 ${p99.toFixed(1)} ms`);
    return `p50 ${lat[Math.floor(lat.length / 2)].toFixed(1)} ms, p99 ${p99.toFixed(1)} ms over ${lat.length} samples`;
  });

  const fail = results.filter((r) => r.status === 'fail').length;
  const pass = results.filter((r) => r.status === 'pass').length;
  const skip = results.filter((r) => r.status === 'skip').length;
  console.log(`\n${pass} passed, ${fail} failed, ${skip} skipped`);
  process.exit(fail ? 1 : 0);
}

main().catch((err) => { console.error(err); process.exit(2); });
