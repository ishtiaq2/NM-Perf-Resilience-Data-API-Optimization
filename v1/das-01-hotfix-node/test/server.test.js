'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const zlib = require('node:zlib');
const { start } = require('../src/server');
const frame = require('../src/shared/spectrum-frame');

function get(port, path, headers = {}, method = 'GET', body) {
  return new Promise((resolve, reject) => {
    const t0 = process.hrtime.bigint();
    const req = http.request({ host: '127.0.0.1', port, path, method, headers }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => {
        let buf = Buffer.concat(chunks);
        if (res.headers['content-encoding'] === 'gzip') buf = zlib.gunzipSync(buf);
        resolve({ status: res.statusCode, headers: res.headers, body: buf, ms: Number(process.hrtime.bigint() - t0) / 1e6 });
      });
    });
    req.on('error', reject);
    if (body) req.end(JSON.stringify(body)); else req.end();
  });
}

let inst;
let port;
test.before(async () => {
  inst = start({ mode: 'hotfix', port: 0, host: '127.0.0.1', nodes: 60, sweepTimeMs: 60, defaultPoints: 20001, simPrewarm: 2, publishIntervalMs: 200, workers: 1 });
  port = await inst.ready;
});
test.after(() => inst.close());

test('heartbeat reports event-loop health and is never cached', async () => {
  const r = await get(port, '/api/heartbeat');
  assert.equal(r.status, 200);
  assert.equal(r.headers['cache-control'], 'no-store');
  const j = JSON.parse(r.body);
  assert.equal(j.status, 'ok');
  assert.equal(typeof j.eventLoop.lagP99Ms, 'number');
});

test('capabilities advertise the opt-in features', async () => {
  const j = JSON.parse((await get(port, '/api/capabilities')).body);
  assert.equal(j.api, 'das-v1');
  assert.equal(j.features.etag, true);
  assert.equal(j.features.spectrumBinary, true);
  assert.equal(j.features.wsTelemetry, false);
});

test('legacy spectrum endpoint keeps its shape, gzip is negotiated', async () => {
  const r = await get(port, '/api/spectrum?nodeId=1&port=1', { 'Accept-Encoding': 'gzip' });
  assert.equal(r.status, 200);
  assert.equal(r.headers['content-encoding'], 'gzip');
  const j = JSON.parse(r.body);
  assert.deepEqual(Object.keys(j), ['sweepId', 'nodeId', 'port', 'timestamp', 'startHz', 'stopHz', 'rbwHz', 'points']);
  assert.equal(j.points.length, 20001);
  assert.deepEqual(Object.keys(j.points[0]), ['frequency', 'power']);
  const plain = await get(port, '/api/spectrum?nodeId=1&port=1');
  assert.equal(plain.headers['content-encoding'], undefined);
  assert.ok(JSON.parse(plain.body).sweepId > j.sweepId, 'each legacy poll returns a newer sweep');
});

test('latest spectrum: binary frame, decimation, 304 and long-poll', async () => {
  const bin = await get(port, '/api/spectrum/latest?nodeId=2&port=1&maxPoints=1000', { Accept: frame.CONTENT_TYPE });
  assert.equal(bin.status, 200);
  assert.equal(bin.headers['content-type'], frame.CONTENT_TYPE);
  const f = frame.decodeFrame(bin.body);
  assert.ok(f.decimated);
  assert.ok(f.count <= 1024);
  assert.equal(f.nodeId, 2);

  const again = await get(port, '/api/spectrum/latest?nodeId=2&port=1&maxPoints=1000', { Accept: frame.CONTENT_TYPE, 'If-None-Match': bin.headers.etag });
  if (again.status === 200) assert.notEqual(again.headers.etag, bin.headers.etag, 'only a newer sweep may return 200');
  else assert.equal(again.status, 304);

  const lp = await get(port, '/api/spectrum/latest?nodeId=2&port=1&maxPoints=1000&waitMs=3000', { Accept: frame.CONTENT_TYPE, 'If-None-Match': bin.headers.etag });
  assert.equal(lp.status, 200, 'long-poll returns the next sweep');
  assert.ok(frame.decodeFrame(lp.body).sweepId > f.sweepId);

  const js = await get(port, '/api/spectrum/latest?nodeId=2&port=1', { Accept: 'application/json', 'Accept-Encoding': 'gzip' });
  const j = JSON.parse(js.body);
  assert.equal(j.count, 20001);
  assert.equal(j.powerDbm.length, 20001);
  assert.equal(typeof j.stepHz, 'number');
});

test('volatile-data: 304 on unchanged revision, delta via ?since', async () => {
  await new Promise((r) => setTimeout(r, 300));
  const a = await get(port, '/api/volatile-data', { 'Accept-Encoding': 'gzip' });
  assert.equal(a.status, 200);
  const snap = JSON.parse(a.body);
  assert.equal(snap.nodeCount, 60);
  const b = await get(port, '/api/volatile-data', { 'Accept-Encoding': 'gzip', 'If-None-Match': a.headers.etag });
  assert.ok(b.status === 304 || b.headers.etag !== a.headers.etag);
  await new Promise((r) => setTimeout(r, 450));
  const d = JSON.parse((await get(port, `/api/volatile-data?since=${snap.rev}`)).body);
  assert.equal(d.base, snap.rev);
  assert.ok(d.rev > snap.rev);
  assert.ok(Object.keys(d.changed).length > 0);
});

test('config: optimistic locking with If-Match prevents lost updates', async () => {
  const g = await get(port, '/api/nodes/5/config');
  const cur = JSON.parse(g.body);
  const etag = g.headers.etag;
  const next = { ...cur.config, dlGainDb: 30 };
  const ok = await get(port, '/api/nodes/5/config', { 'Content-Type': 'application/json', 'If-Match': etag }, 'PUT', next);
  assert.equal(ok.status, 200);
  assert.equal(JSON.parse(ok.body).version, cur.version + 1);
  const stale = await get(port, '/api/nodes/5/config', { 'Content-Type': 'application/json', 'If-Match': etag }, 'PUT', { ...cur.config, dlGainDb: 10 });
  assert.equal(stale.status, 412);
  const bad = await get(port, '/api/nodes/5/config', { 'Content-Type': 'application/json' }, 'PUT', { ...cur.config, dlGainDb: 99 });
  assert.equal(bad.status, 422);
  assert.equal((await get(port, '/api/nodes/999/config')).status, 404);
});

test('SLO: heartbeat p99 < 100 ms while 6 analyzers poll 20k-point sweeps back-to-back', async () => {
  let running = true;
  const pollers = [];
  for (let i = 0; i < 6; i++) {
    pollers.push((async () => {
      while (running) await get(port, `/api/spectrum?nodeId=${(i % 2) + 1}&port=1`, { 'Accept-Encoding': 'gzip' });
    })());
  }
  await new Promise((r) => setTimeout(r, 300));
  const lat = [];
  for (let i = 0; i < 40; i++) {
    lat.push((await get(port, '/api/heartbeat')).ms);
    await new Promise((r) => setTimeout(r, 50));
  }
  running = false;
  await Promise.all(pollers);
  lat.sort((x, y) => x - y);
  const p99 = lat[Math.floor(lat.length * 0.99) - 1];
  assert.ok(p99 < 100, `heartbeat p99 ${p99.toFixed(1)} ms`);
});
