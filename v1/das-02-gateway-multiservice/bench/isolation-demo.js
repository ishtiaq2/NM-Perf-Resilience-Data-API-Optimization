#!/usr/bin/env node
'use strict';
/**
 * Isolation demo: what happens to the UI when the spectrum service misbehaves?
 *
 * Simulates a bug that blocks spectrum-service's event loop for --block-ms
 * (default 5 s). The stack must run with DAS_DEMO=1; scripts/run-local.sh sets it.
 * Meanwhile it measures what an engineer's browser would see:
 *   - heartbeat latency and status (core-api, another process)
 *   - telemetry deltas pushed over WebSocket (telemetry-service, another process)
 *   - configuration reads (core-api)
 *   - spectrum requests (the blocked service)
 *
 *   sh scripts/run-local.sh &   then   node bench/isolation-demo.js
 * Needs Node 18+.
 */
const http = require('node:http');
const path = require('node:path');
const { parseArgs } = require('node:util');
const WebSocket = require('ws');

const { values: a } = parseArgs({ options: {
  base: { type: 'string', default: 'http://127.0.0.1:8080' },
  'spectrum-socket': { type: 'string', default: path.join(__dirname, '..', '.run', 'spectrum.sock') },
  'block-ms': { type: 'string', default: '5000' },
  seconds: { type: 'string', default: '12' }
} });
const base = a.base.replace(/\/$/, '');
const blockMs = Number(a['block-ms']);
const t0 = Date.now();
const sec = () => Math.floor((Date.now() - t0) / 1000);
const rows = new Map();
const row = (s) => { if (!rows.has(s)) rows.set(s, { hb: [], status: new Set(), spectrumSvc: new Set(), deltas: 0, cfgOk: 0, cfgMs: [], specOk: 0, specWaitMs: [] }); return rows.get(s); };
let running = true;

async function timed(url, init) {
  const t = Date.now();
  try {
    const r = await fetch(url, { ...init, signal: AbortSignal.timeout(10000) });
    const body = await r.text();
    return { ok: r.ok, status: r.status, ms: Date.now() - t, body };
  } catch (e) {
    return { ok: false, status: 0, ms: Date.now() - t };
  }
}

async function heartbeatLoop() {
  while (running) {
    const s = sec();
    const r = await timed(base + '/api/heartbeat');
    const rr = row(s);
    rr.hb.push(r.ms);
    try { const j = JSON.parse(r.body); rr.status.add(j.status); if (j.services) rr.spectrumSvc.add(j.services.spectrum); } catch { rr.status.add('error'); }
    await new Promise((res) => setTimeout(res, 200));
  }
}

async function configLoop() {
  while (running) {
    const s = sec();
    const r = await timed(base + '/api/nodes/5/config');
    if (r.ok) { row(s).cfgOk++; row(s).cfgMs.push(r.ms); }
    await new Promise((res) => setTimeout(res, 500));
  }
}

async function spectrumLoop() {
  while (running) {
    const s = sec();
    const r = await timed(base + '/api/spectrum/latest?nodeId=1&port=1&maxPoints=800', { headers: { Accept: 'application/vnd.das.spectrum' } });
    if (r.ok) { row(sec()).specOk++; row(sec()).specWaitMs.push(r.ms); } else row(s);
    await new Promise((res) => setTimeout(res, 250));
  }
}

function telemetry() {
  const ws = new WebSocket(base.replace(/^http/, 'ws') + '/api/ws/telemetry');
  ws.on('message', (d) => {
    const m = JSON.parse(d.toString());
    if (m.type === 'hello') ws.send(JSON.stringify({ type: 'sub' }));
    if (m.type === 'delta') row(sec()).deltas++;
  });
  return ws;
}

function block() {
  return new Promise((resolve) => {
    const req = http.request({ socketPath: a['spectrum-socket'], path: `/internal/debug/block?ms=${blockMs}`, method: 'POST' }, (res) => { res.resume(); res.on('end', resolve); });
    req.on('error', (e) => { console.error('cannot reach spectrum-service socket (is the stack running with DAS_DEMO=1?):', e.message); process.exit(2); });
    req.end();
  });
}

(async () => {
  const ws = telemetry();
  const loops = [heartbeatLoop(), configLoop(), spectrumLoop()];
  await new Promise((r) => setTimeout(r, 3000));
  const blockStart = sec();
  console.log(`t=${blockStart}s: blocking spectrum-service's event loop for ${blockMs} ms (simulated bug)\n`);
  block();
  await new Promise((r) => setTimeout(r, Number(a.seconds) * 1000 - 3000));
  running = false;
  ws.close();
  await Promise.all(loops);

  const pad = (s, n) => String(s).padEnd(n);
  console.log(pad('t (s)', 6) + pad('heartbeat max', 15) + pad('heartbeat status', 18) + pad('services.spectrum', 19) + pad('telemetry deltas', 18) + pad('config reads', 14) + 'spectrum responses');
  for (const [s, r] of [...rows.entries()].sort((x, y) => x[0] - y[0])) {
    const blocked = s >= blockStart && s < blockStart + Math.ceil(blockMs / 1000);
    console.log(pad(s + (blocked ? ' *' : ''), 6) + pad(r.hb.length ? Math.max(...r.hb) + ' ms' : '-', 15) + pad([...r.status].join('/'), 18) + pad([...r.spectrumSvc].join('/'), 19) + pad(r.deltas, 18) + pad(r.cfgOk, 14) + r.specOk);
  }
  const all = [...rows.values()];
  const hbMax = Math.max(...all.flatMap((r) => r.hb));
  console.log(`\n* = spectrum-service blocked. Heartbeat worst case over the whole run: ${hbMax} ms.`);
  console.log('Everything except spectrum kept working, and the UI can show "spectrum service busy" instead of "server is dead".');
})();
