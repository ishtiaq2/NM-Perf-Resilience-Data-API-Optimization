#!/usr/bin/env node
'use strict';
/**
 * Heartbeat-under-load benchmark: "does the UI think the server is dead while
 * an engineer runs the spectrum analyzer?"
 *
 * Each simulated browser uses one HTTP agent limited to 6 connections per
 * origin, like Chrome/Firefox over HTTP/1.1. Latency is measured from the moment
 * the request is issued, so time spent queued in the browser behind large
 * spectrum downloads counts, just as it does in the real UI.
 *
 * Per browser:
 *  - heartbeat poller   (fixed interval, per-request timeout = the UI's alarm timeout)
 *  - N spectrum pollers (sequential: next request right after the previous response)
 *  - dashboard poller   (GET /api/volatile-data)
 *  - config reader      (GET /api/nodes/{id}/config)
 *
 * Like a browser HTTP cache, it sends If-None-Match when a previous 200 had an
 * ETag and was not marked no-store (disable with --no-browser-cache).
 *
 * Runs against any implementation (hotfix, gateway, Go edge) - only the base URL changes.
 * Requires Node 18+ on the machine running the benchmark (not on the device).
 *
 *   node bench/heartbeat-under-load.js --base http://192.168.1.10 --duration 60
 */

const http = require('node:http');
const https = require('node:https');
const { parseArgs } = require('node:util');
const { performance } = require('node:perf_hooks');
const fs = require('node:fs');

const { values: a } = parseArgs({
  options: {
    base: { type: 'string', default: 'http://127.0.0.1:8080' },
    duration: { type: 'string', default: '30' },
    warmup: { type: 'string', default: '3' },
    browsers: { type: 'string', default: '3' },
    'analyzers-per-browser': { type: 'string', default: '1' },
    spectrum: { type: 'string', default: 'legacy' }, // legacy | latest-json | latest-binary | none
    points: { type: 'string', default: '50001' },
    'max-points': { type: 'string', default: '0' },
    'spectrum-delay': { type: 'string', default: '0' },
    'hb-interval': { type: 'string', default: '1000' },
    'hb-timeout': { type: 'string', default: '3000' },
    'dashboard-interval': { type: 'string', default: '2000' },
    'config-interval': { type: 'string', default: '3000' },
    'max-sockets': { type: 'string', default: '6' },
    'no-browser-cache': { type: 'boolean', default: false },
    insecure: { type: 'boolean', default: false },
    label: { type: 'string', default: '' },
    out: { type: 'string', default: '' },
    quiet: { type: 'boolean', default: false }
  }
});

const cfg = {
  base: a.base.replace(/\/$/, ''),
  durationS: Number(a.duration),
  warmupS: Number(a.warmup),
  browsers: Number(a.browsers),
  analyzersPerBrowser: Number(a['analyzers-per-browser']),
  spectrum: a.spectrum,
  points: Number(a.points),
  maxPoints: Number(a['max-points']),
  spectrumDelayMs: Number(a['spectrum-delay']),
  hbIntervalMs: Number(a['hb-interval']),
  hbTimeoutMs: Number(a['hb-timeout']),
  dashboardIntervalMs: Number(a['dashboard-interval']),
  configIntervalMs: Number(a['config-interval']),
  maxSockets: Number(a['max-sockets']),
  browserCache: !a['no-browser-cache'],
  label: a.label || a.spectrum
};

const isHttps = cfg.base.startsWith('https:');
const transport = isHttps ? https : http;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const t0 = performance.now();
const since = () => (performance.now() - t0) / 1000;

function pct(sorted, p) {
  if (!sorted.length) return null;
  return sorted[Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length))];
}
function summarize(lat) {
  const s = lat.slice().sort((x, y) => x - y);
  const r2 = (v) => (v == null ? null : Math.round(v * 10) / 10);
  return { n: s.length, p50: r2(pct(s, 50)), p95: r2(pct(s, 95)), p99: r2(pct(s, 99)), max: r2(s[s.length - 1] ?? null) };
}

class Browser {
  constructor(id) {
    this.id = id;
    const opts = { keepAlive: true, maxSockets: cfg.maxSockets };
    if (isHttps && a.insecure) opts.rejectUnauthorized = false;
    this.agent = new transport.Agent(opts);
    this.etags = new Map();
  }

  /** Issue a GET like the browser would. Resolves with timing and metadata; never rejects. */
  get(path, { timeoutMs = 30000, headers = {} } = {}) {
    return new Promise((resolve) => {
      const start = performance.now();
      const h = { 'Accept-Encoding': 'gzip', ...headers };
      const cached = cfg.browserCache ? this.etags.get(path) : undefined;
      if (cached) h['If-None-Match'] = cached;
      let done = false;
      let bytes = 0;
      let head = Buffer.alloc(0);
      const finish = (r) => { if (done) return; done = true; clearTimeout(timer); resolve({ t: since(), latencyMs: performance.now() - start, bytes, ...r }); };
      const req = transport.request(cfg.base + path, { method: 'GET', agent: this.agent, headers: h }, (res) => {
        res.on('data', (c) => { bytes += c.length; if (head.length < 1024) head = Buffer.concat([head, c.subarray(0, 1024 - head.length)]); });
        res.on('end', () => {
          const cc = String(res.headers['cache-control'] || '');
          if (res.statusCode === 200 && res.headers.etag && !cc.includes('no-store')) this.etags.set(path, res.headers.etag);
          finish({ status: res.statusCode, headers: res.headers, head });
        });
        res.on('error', (err) => finish({ status: 0, error: String(err.message || err) }));
      });
      const timer = setTimeout(() => { req.destroy(new Error('timeout')); finish({ status: 0, timedOut: true }); }, timeoutMs);
      req.on('error', (err) => finish({ status: 0, error: String(err.message || err) }));
      req.end();
    });
  }
}

const results = { heartbeat: [], spectrum: [], dashboard: [], config: [] };
let running = true;
const warmupEnd = cfg.warmupS;

async function heartbeatLoop(b) {
  let fails = 0;
  const alarms = { strict: 0, tolerant: 0 };
  let inStrict = false;
  let inTolerant = false;
  const pending = [];
  while (running) {
    const p = b.get('/api/heartbeat', { timeoutMs: cfg.hbTimeoutMs, headers: { Accept: 'application/json' } }).then((r) => {
      let serverLag = null;
      if (r.status === 200) {
        try { const j = JSON.parse(r.head.toString('utf8')); serverLag = j.eventLoop ? j.eventLoop.lagP99Ms : j.runtime && j.runtime.schedLagP99Ms != null ? j.runtime.schedLagP99Ms : null; } catch { /* truncated or not JSON */ }
      }
      const ok = r.status === 200;
      if (r.t >= warmupEnd) {
        fails = ok ? 0 : fails + 1;
        if (!ok && !inStrict) { alarms.strict++; inStrict = true; }
        if (ok) inStrict = false;
        if (fails >= 3 && !inTolerant) { alarms.tolerant++; inTolerant = true; }
        if (ok) inTolerant = false;
        results.heartbeat.push({ browser: b.id, t: r.t, ok, timedOut: !!r.timedOut, latencyMs: Math.min(r.latencyMs, cfg.hbTimeoutMs), serverLag });
      }
    });
    pending.push(p);
    await sleep(cfg.hbIntervalMs);
  }
  await Promise.all(pending);
  return alarms;
}

function spectrumPath(nodeId) {
  const common = `nodeId=${nodeId}&port=1&points=${cfg.points}`;
  if (cfg.spectrum === 'legacy') return `/api/spectrum?${common}`;
  return `/api/spectrum/latest?${common}` + (cfg.maxPoints ? `&maxPoints=${cfg.maxPoints}` : '') + '&waitMs=2000';
}

async function spectrumLoop(b, nodeId) {
  const path = spectrumPath(nodeId);
  const headers = cfg.spectrum === 'latest-binary' ? { Accept: 'application/vnd.das.spectrum' } : { Accept: 'application/json' };
  while (running) {
    const r = await b.get(path, { timeoutMs: 30000, headers });
    if (r.t >= warmupEnd) {
      let sweepId = r.headers && r.headers['x-sweep-id'] ? Number(r.headers['x-sweep-id']) : null;
      if (sweepId == null && r.head && r.status === 200 && !(r.headers['content-encoding'])) {
        const m = /"sweepId":(\d+)/.exec(r.head.toString('latin1'));
        if (m) sweepId = Number(m[1]);
      }
      results.spectrum.push({ browser: b.id, nodeId, t: r.t, status: r.status, latencyMs: r.latencyMs, bytes: r.bytes, sweepId, error: r.error, timedOut: !!r.timedOut });
    }
    if (r.status !== 200 && r.status !== 304) await sleep(500);
    if (cfg.spectrumDelayMs) await sleep(cfg.spectrumDelayMs);
  }
}

async function intervalLoop(b, kind, intervalMs, pathFn) {
  if (!intervalMs) return;
  await sleep(Math.random() * intervalMs);
  const pending = [];
  while (running) {
    pending.push(b.get(pathFn(), { timeoutMs: 10000, headers: { Accept: 'application/json' } }).then((r) => {
      if (r.t >= warmupEnd) results[kind].push({ browser: b.id, t: r.t, status: r.status, latencyMs: r.latencyMs, bytes: r.bytes, timedOut: !!r.timedOut });
    }));
    await sleep(intervalMs);
  }
  await Promise.all(pending);
}

async function main() {
  const probe = await new Browser(-1).get('/api/heartbeat', { timeoutMs: 5000 });
  if (probe.status !== 200) {
    console.error(`cannot reach ${cfg.base}/api/heartbeat (status ${probe.status} ${probe.error || ''})`);
    process.exit(2);
  }
  if (!a.quiet) console.error(`[bench] ${cfg.label}: ${cfg.browsers} browsers x ${cfg.analyzersPerBrowser} analyzer(s) (${cfg.spectrum}, ${cfg.points} pts), hb every ${cfg.hbIntervalMs} ms, timeout ${cfg.hbTimeoutMs} ms, ${cfg.durationS}s (+${cfg.warmupS}s warm-up) -> ${cfg.base}`);

  const browsers = Array.from({ length: cfg.browsers }, (_, i) => new Browser(i));
  const hbTasks = [];
  const other = [];
  let analyzer = 0;
  for (const b of browsers) {
    hbTasks.push(heartbeatLoop(b));
    if (cfg.spectrum !== 'none') for (let k = 0; k < cfg.analyzersPerBrowser; k++) other.push(spectrumLoop(b, (analyzer++ % 4) + 1));
    other.push(intervalLoop(b, 'dashboard', cfg.dashboardIntervalMs, () => '/api/volatile-data'));
    other.push(intervalLoop(b, 'config', cfg.configIntervalMs, () => `/api/nodes/${1 + Math.floor(Math.random() * 50)}/config`));
  }
  await sleep((cfg.warmupS + cfg.durationS) * 1000);
  running = false;
  const alarmsPerBrowser = await Promise.all(hbTasks);
  await Promise.race([Promise.all(other), sleep(15000)]);
  for (const b of browsers) b.agent.destroy();

  const hb = results.heartbeat;
  const sp = results.spectrum;
  const okSp = sp.filter((r) => r.status === 200);
  const sweepsSeen = new Set(okSp.filter((r) => r.sweepId != null).map((r) => `${r.nodeId}:${r.sweepId}`));
  const lags = hb.map((r) => r.serverLag).filter((v) => typeof v === 'number');
  const summary = {
    label: cfg.label,
    params: cfg,
    heartbeat: {
      ...summarize(hb.map((r) => r.latencyMs)),
      timeouts: hb.filter((r) => r.timedOut).length,
      errors: hb.filter((r) => !r.ok && !r.timedOut).length,
      alarmsStrict: alarmsPerBrowser.reduce((s, x) => s + x.strict, 0),
      alarmsTolerant: alarmsPerBrowser.reduce((s, x) => s + x.tolerant, 0),
      serverLagP99MaxMs: lags.length ? Math.max(...lags) : null
    },
    spectrum: {
      ...summarize(sp.map((r) => r.latencyMs)),
      ok: okSp.length,
      notModified: sp.filter((r) => r.status === 304).length,
      errors: sp.filter((r) => r.status !== 200 && r.status !== 304).length,
      mbReceived: Math.round(sp.reduce((s, r) => s + r.bytes, 0) / 1e4) / 100,
      distinctSweepsDelivered: sweepsSeen.size,
      responsesPerS: Math.round((okSp.length / cfg.durationS) * 100) / 100
    },
    dashboard: {
      ...summarize(results.dashboard.map((r) => r.latencyMs)),
      notModified: results.dashboard.filter((r) => r.status === 304).length,
      errors: results.dashboard.filter((r) => r.status !== 200 && r.status !== 304).length,
      kbReceived: Math.round(results.dashboard.reduce((s, r) => s + r.bytes, 0) / 1e3)
    },
    configApi: {
      ...summarize(results.config.map((r) => r.latencyMs)),
      errors: results.config.filter((r) => r.status !== 200 && r.status !== 304).length
    },
    timeline: hb.map((r) => ({ t: Math.round((r.t - warmupEnd) * 1000) / 1000, ms: Math.round(r.latencyMs * 10) / 10, ok: r.ok }))
  };

  if (a.out) fs.writeFileSync(a.out, JSON.stringify(summary, null, 2));
  if (!a.quiet) {
    const h = summary.heartbeat;
    const s = summary.spectrum;
    console.log(`\n=== ${summary.label} ===`);
    console.log(`heartbeat   p50 ${h.p50} ms | p99 ${h.p99} ms | max ${h.max} ms | timeouts ${h.timeouts}/${h.n} | false "server dead" alarms: ${h.alarmsStrict} (1-miss rule), ${h.alarmsTolerant} (3-miss rule)`);
    console.log(`spectrum    ${s.ok} responses (${s.notModified} x 304) | p50 ${s.p50} ms | p95 ${s.p95} ms | ${s.mbReceived} MB | ${s.distinctSweepsDelivered} distinct sweeps`);
    console.log(`dashboard   p50 ${summary.dashboard.p50} ms | p95 ${summary.dashboard.p95} ms | 304s ${summary.dashboard.notModified}/${summary.dashboard.n} | ${summary.dashboard.kbReceived} kB`);
    console.log(`config API  p50 ${summary.configApi.p50} ms | p95 ${summary.configApi.p95} ms | max ${summary.configApi.max} ms`);
    if (h.serverLagP99MaxMs != null) console.log(`server event-loop lag p99 (worst reported) ${h.serverLagP99MaxMs} ms`);
  }
}

main().catch((err) => { console.error(err); process.exit(1); });
