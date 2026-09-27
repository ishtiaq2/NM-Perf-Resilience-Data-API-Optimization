#!/usr/bin/env node
'use strict';
/**
 * Push vs poll for the dashboard (volatile_data), N engineers, same data.
 *
 *  poll: GET /api/volatile-data every --poll-ms with gzip + If-None-Match (what a
 *        well-behaved HTTP client does after the hotfix)
 *  push: WebSocket /api/ws/telemetry (snapshot, then deltas; permessage-deflate)
 *
 * Reports bytes on the wire, requests, data freshness (age of the data when it
 * reaches the client), and the telemetry service's CPU time (Linux).
 *
 *   node bench/push-vs-poll.js --clients 20 --duration 30
 */
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const { parseArgs } = require('node:util');
const WebSocket = require('ws');

const { values: a } = parseArgs({ options: {
  base: { type: 'string', default: 'http://127.0.0.1:8080' },
  clients: { type: 'string', default: '20' },
  duration: { type: 'string', default: '30' },
  'poll-ms': { type: 'string', default: '2000' },
  'telemetry-pid-file': { type: 'string', default: path.join(__dirname, '..', '.run', 'telemetry.pid') }
} });
const base = a.base.replace(/\/$/, '');
const N = Number(a.clients);
const D = Number(a.duration) * 1000;

function cpuSeconds() {
  try {
    const pid = fs.readFileSync(a['telemetry-pid-file'], 'utf8').trim();
    const f = fs.readFileSync(`/proc/${pid}/stat`, 'utf8');
    const parts = f.slice(f.lastIndexOf(')') + 2).split(' ');
    return (Number(parts[11]) + Number(parts[12])) / 100; // utime + stime in clock ticks (USER_HZ=100)
  } catch { return null; }
}
const pct = (arr, p) => { const s = arr.slice().sort((x, y) => x - y); return s.length ? s[Math.min(s.length - 1, Math.floor(p / 100 * s.length))] : null; };

async function poll() {
  const agent = new http.Agent({ keepAlive: true, maxSockets: 2 });
  const stats = { requests: 0, notModified: 0, bytes: 0, updates: 0, ages: [] };
  const end = Date.now() + D;
  await Promise.all(Array.from({ length: N }, (_, i) => (async () => {
    let etag = null;
    await new Promise((r) => setTimeout(r, (i * Number(a['poll-ms'])) / N));
    while (Date.now() < end) {
      await new Promise((resolve) => {
        const headers = { 'Accept-Encoding': 'gzip' };
        if (etag) headers['If-None-Match'] = etag;
        const req = http.get(base + '/api/volatile-data', { agent, headers }, (res) => {
          let head = 0;
          for (let k = 0; k < res.rawHeaders.length; k += 2) head += res.rawHeaders[k].length + res.rawHeaders[k + 1].length + 4;
          const chunks = [];
          res.on('data', (c) => chunks.push(c));
          res.on('end', () => {
            stats.requests++;
            const body = Buffer.concat(chunks);
            stats.bytes += head + body.length + 20;
            if (res.statusCode === 304) { stats.notModified++; return resolve(); }
            etag = res.headers.etag || null;
            const json = JSON.parse(require('node:zlib').gunzipSync(body).toString());
            stats.updates++;
            stats.ages.push(Date.now() - json.generatedAt);
            resolve();
          });
        });
        req.on('error', resolve);
      });
      await new Promise((r) => setTimeout(r, Number(a['poll-ms'])));
    }
  })()));
  agent.destroy();
  return stats;
}

async function push() {
  const stats = { requests: N, notModified: 0, bytes: 0, updates: 0, ages: [] };
  const sockets = await Promise.all(Array.from({ length: N }, () => new Promise((resolve, reject) => {
    const ws = new WebSocket(base.replace(/^http/, 'ws') + '/api/ws/telemetry', { perMessageDeflate: true });
    ws.on('message', (d) => {
      const m = JSON.parse(d.toString());
      if (m.type === 'hello') ws.send(JSON.stringify({ type: 'sub' }));
      if (m.type === 'snapshot' || m.type === 'delta') { stats.updates++; stats.ages.push(Date.now() - m.generatedAt); }
    });
    ws.on('open', () => resolve(ws));
    ws.on('error', reject);
  })));
  await new Promise((r) => setTimeout(r, D));
  for (const ws of sockets) { stats.bytes += ws._socket.bytesRead; ws.close(); }
  return stats;
}

(async () => {
  console.log(`${N} dashboards, ${a.duration} s per mode, ${base}\n`);
  const out = {};
  for (const [name, fn] of [['poll (2 s, ETag, gzip)', poll], ['push (WebSocket deltas)', push]]) {
    const c0 = cpuSeconds();
    out[name] = await fn();
    const c1 = cpuSeconds();
    out[name].cpu = c0 != null && c1 != null ? Math.round((c1 - c0) * 100) / 100 : null;
    await new Promise((r) => setTimeout(r, 1500));
  }
  const pad = (s, n) => String(s).padEnd(n);
  console.log(pad('mode', 26) + pad('requests', 10) + pad('304s', 7) + pad('updates', 9) + pad('wire kB/s (all clients)', 25) + pad('data age p50 / p95', 20) + 'telemetry-service CPU');
  for (const [name, s] of Object.entries(out)) {
    console.log(pad(name, 26) + pad(s.requests, 10) + pad(s.notModified, 7) + pad(s.updates, 9) + pad(Math.round(s.bytes / 1024 / Number(a.duration)), 25) +
      pad(`${pct(s.ages, 50)} / ${pct(s.ages, 95)} ms`, 20) + (s.cpu == null ? 'n/a' : `${s.cpu} s`));
  }
})();
