'use strict';
/**
 * End-to-end through a real nginx: one origin, many services.
 * Skipped when nginx is not installed.
 */
const test = require('node:test');
const assert = require('node:assert/strict');
const WebSocket = require('ws');
const { haveNginx, startStack } = require('./helpers/stack');

const skip = !haveNginx() && 'nginx not installed';
let stack;
test.before(async () => { if (!skip) stack = await startStack(); });
test.after(async () => { if (stack) await stack.stop(); });

test('one origin: each path is answered by its own service', { skip }, async () => {
  const hb = await (await fetch(stack.base + '/api/heartbeat')).json();
  assert.equal(hb.server, 'das-core-api');
  const vd = await fetch(stack.base + '/api/volatile-data');
  assert.equal(vd.status, 200);
  assert.ok(['MISS', 'HIT', 'UPDATING', 'EXPIRED', 'STALE'].includes(vd.headers.get('x-cache-status')));
  const sp = await fetch(stack.base + '/api/spectrum/latest?nodeId=1&port=1', { headers: { Accept: 'application/vnd.das.spectrum' } });
  assert.equal(sp.headers.get('content-type'), 'application/vnd.das.spectrum');
  const cfg = await fetch(stack.base + '/api/nodes/3/config');
  assert.equal(cfg.status, 200);
  const ui = await fetch(stack.base + '/some/client/route');
  assert.match(await ui.text(), /<html/i, 'SPA fallback to index.html');
});

test('internal endpoints are not reachable through the gateway', { skip }, async () => {
  assert.equal((await fetch(stack.base + '/internal/health')).status, 404);
  assert.equal((await fetch(stack.base + '/internal/debug/block?ms=1', { method: 'POST' })).status, 404);
});

test('heartbeat reports the health of every service', { skip }, async () => {
  await new Promise((r) => setTimeout(r, 700));
  const hb = await (await fetch(stack.base + '/api/heartbeat')).json();
  assert.deepEqual(Object.keys(hb.services).sort(), ['spectrum', 'telemetry']);
  assert.equal(hb.services.telemetry, 'ok');
});

test('micro-cache: 30 concurrent dashboard polls cost the telemetry service at most 2 requests', { skip }, async () => {
  const m0 = await internalMetrics();
  await Promise.all(Array.from({ length: 30 }, () => fetch(stack.base + '/api/volatile-data', { headers: { 'Accept-Encoding': 'gzip' } }).then((r) => r.arrayBuffer())));
  const m1 = await internalMetrics();
  const upstream = (m1.volatile ? m1.volatile.requests : 0) - (m0.volatile ? m0.volatile.requests : 0);
  assert.ok(upstream <= 2, `telemetry-service saw ${upstream} requests for 30 client requests`);
});

async function internalMetrics() {
  // Read the telemetry service's counters directly over its Unix socket.
  const http = require('node:http');
  const path = require('node:path');
  return new Promise((resolve, reject) => {
    http.get({ socketPath: path.join(stack.dir, 'telemetry.sock'), path: '/internal/metrics' }, (res) => {
      let s = '';
      res.on('data', (c) => { s += c; });
      res.on('end', () => resolve(JSON.parse(s).routes));
    }).on('error', reject);
  });
}

test('WebSocket telemetry through nginx: snapshot, then deltas; foreign Origin rejected', { skip }, async () => {
  const ws = new WebSocket(stack.base.replace('http', 'ws') + '/api/ws/telemetry');
  const msgs = [];
  await new Promise((resolve, reject) => {
    ws.on('message', (d) => {
      const m = JSON.parse(d.toString());
      msgs.push(m);
      if (m.type === 'hello') ws.send(JSON.stringify({ type: 'sub' }));
      if (msgs.some((x) => x.type === 'snapshot') && m.type === 'delta') resolve();
    });
    ws.on('error', reject);
    setTimeout(() => reject(new Error('no delta')), 5000);
  });
  ws.close();
  const snap = msgs.find((m) => m.type === 'snapshot');
  assert.equal(snap.nodeCount, 40);
  assert.ok(msgs.find((m) => m.type === 'delta').base >= snap.rev);

  const evil = new WebSocket(stack.base.replace('http', 'ws') + '/api/ws/telemetry', { origin: 'https://evil.example' });
  const status = await new Promise((resolve) => {
    evil.on('unexpected-response', (req, res) => resolve(res.statusCode));
    evil.on('open', () => resolve(101));
    evil.on('error', () => resolve(-1));
  });
  assert.equal(status, 403);
});

test('WebSocket spectrum through nginx: decimated binary frames per sweep', { skip }, async () => {
  const frame = require('../lib/spectrum-frame');
  const ws = new WebSocket(stack.base.replace('http', 'ws') + '/api/ws/spectrum');
  const frames = [];
  await new Promise((resolve, reject) => {
    ws.on('message', (d, isBinary) => {
      if (!isBinary) {
        const m = JSON.parse(d.toString());
        if (m.type === 'hello') ws.send(JSON.stringify({ type: 'sub', nodeId: 1, port: 1, maxPoints: 256 }));
        return;
      }
      frames.push(frame.decodeFrame(d));
      if (frames.length === 3) resolve();
    });
    ws.on('error', reject);
    setTimeout(() => reject(new Error(`only ${frames.length} frames`)), 5000);
  });
  ws.close();
  assert.ok(frames.every((f) => f.decimated && f.count <= 256));
  assert.ok(frames[2].sweepId > frames[0].sweepId);
});

test('gateway answers "degraded" (not dead) while core-api restarts', { skip }, async () => {
  await stack.services.core.service.close();
  const r = await fetch(stack.base + '/api/heartbeat');
  assert.equal(r.status, 200);
  const j = await r.json();
  assert.equal(j.status, 'degraded');
  assert.equal(j.server, 'das-gateway');
  assert.equal(j.services['core-api'], 'down');
});
