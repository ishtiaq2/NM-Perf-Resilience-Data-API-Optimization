#!/usr/bin/env node
'use strict';
/**
 * noc-agent: the Master Unit side of das-noc.v1, as a small sidecar next to the
 * EXISTING Node.js backend. It reads volatile_data from the local API (the same
 * endpoint the Angular UI polls), derives the site summary and alarm events, and
 * keeps one outbound WebSocket to the NOC. No change to the shipped application;
 * no inbound port at the venue.
 *
 *   NOC_URL=wss://noc.example.com/uplink/v1 NOC_TOKEN=v1.site.S00042.airport.xxx \
 *   LOCAL_API=http://127.0.0.1:8080 SITE_NAME="Terminal 2" SITE_VENUE="Airport North" \
 *   node agent/noc-agent.js
 *
 * Needs a WebSocket client: the global one (Node 22+) or the `ws` package.
 * Zero other dependencies. docs/PROTOCOL.md is the specification it implements.
 */

const http = require('node:http');
const https = require('node:https');
const crypto = require('node:crypto');

const env = (k, d) => (process.env[k] === undefined || process.env[k] === '' ? d : process.env[k]);
const cfg = {
  nocUrl: env('NOC_URL', 'ws://127.0.0.1:8080/uplink/v1'),
  token: env('NOC_TOKEN', ''),
  localApi: env('LOCAL_API', 'http://127.0.0.1:8080'),
  pollMs: Number(env('POLL_MS', '5000')),
  staleMs: Number(env('NODE_STALE_MS', '15000')), // no report for this long: the Remote Node is offline
  name: env('SITE_NAME', ''),
  venue: env('SITE_VENUE', ''),
  region: env('SITE_REGION', ''),
  fw: env('SITE_FW', ''),
  outboxMax: Number(env('OUTBOX_MAX', '10000')),
  log: env('LOG', 'info')
};
const log = (event, fields) => { if (cfg.log !== 'quiet') process.stderr.write(JSON.stringify({ t: new Date().toISOString(), event, ...fields }) + '\n'); };

let WS = globalThis.WebSocket;

// ------------------------------------------------------------------ canonical hash
// Keys sorted, no whitespace, strings and numbers as JSON.stringify writes them;
// FNV-1a 64 of the UTF-8 bytes, 16 lowercase hex digits. Must equal the NOC's.
function canonical(v) {
  if (v === null || typeof v !== 'object') return JSON.stringify(v === undefined ? null : v);
  if (Array.isArray(v)) return '[' + v.map(canonical).join(',') + ']';
  return '{' + Object.keys(v).sort().map((k) => JSON.stringify(k) + ':' + canonical(v[k])).join(',') + '}';
}
function fnv1a64(str) {
  let h = 0xcbf29ce484222325n;
  for (const b of Buffer.from(str, 'utf8')) { h ^= BigInt(b); h = (h * 0x100000001b3n) & 0xffffffffffffffffn; }
  return h.toString(16).padStart(16, '0');
}

// ------------------------------------------------------------------ device state
const state = {
  boot: crypto.randomBytes(6).toString('hex'), // new sequence space on every agent start
  summary: null, hash: '', rev: 0, lastSent: null,
  seq: 0, outbox: [], overflow: false, active: new Map(),
  nodes: new Map(), detailOn: false, detailEvery: 1000, detailRev: 0, detailTimer: null, detailSent: new Map()
};

const round = (v, step) => Math.round(v / step) * step;
const clean = (v, step) => Number(round(v, step).toFixed(3)); // no 38.00000000000001

function summarize(vd) {
  const now = Date.now();
  let online = 0, offline = 0, degraded = 0, maxT = null, minRx = null, maxVswr = null;
  const alarms = new Map();
  state.nodes = new Map();
  for (const n of Object.values(vd.nodes || {})) {
    const stale = now - (n.reportedAt || 0) > cfg.staleMs;
    if (stale) offline++; else online++;
    if (!stale && n.status === 'degraded') degraded++;
    if (typeof n.temperatureC === 'number') maxT = maxT === null ? n.temperatureC : Math.max(maxT, n.temperatureC);
    const rx = n.optical && n.optical.rxDbm;
    if (typeof rx === 'number') minRx = minRx === null ? rx : Math.min(minRx, rx);
    let nodeVswr = null;
    for (const b of n.bands || []) {
      if (b.enabled && typeof b.vswr === 'number') {
        nodeVswr = nodeVswr === null ? b.vswr : Math.max(nodeVswr, b.vswr);
        maxVswr = maxVswr === null ? b.vswr : Math.max(maxVswr, b.vswr);
      }
    }
    for (const a of n.alarms || []) {
      alarms.set('n' + n.id + ':' + a.code, { node: n.id, code: a.code, sev: a.severity || 'minor', text: '' });
    }
    if (stale) alarms.set('n' + n.id + ':NODE_OFFLINE', { node: n.id, code: 'NODE_OFFLINE', sev: 'major', text: 'no report from the Remote Node' });
    state.nodes.set(n.id, {
      id: n.id, name: n.name, type: n.type, chain: n.chain, hop: n.hop,
      status: stale ? 'offline' : (n.alarms || []).some((a) => a.severity === 'critical' || a.severity === 'major') ? 'alarm' : n.status === 'degraded' ? 'degraded' : 'online',
      tempC: clean(n.temperatureC, 0.1), rxDbm: rx == null ? null : clean(rx, 0.1), txDbm: n.optical ? clean(n.optical.txDbm, 0.1) : null,
      vswr: nodeVswr === null ? null : clean(nodeVswr, 0.01), psuV: clean(n.psuVoltageV, 0.01), fw: n.fw
    });
  }
  const s = {
    name: cfg.name, venue: cfg.venue, region: cfg.region, fw: cfg.fw,
    nodes: online + offline, online, offline, degraded,
    maxTempC: maxT === null ? null : clean(maxT, 0.5), minRxDbm: minRx === null ? null : clean(minRx, 0.1), maxVswr: maxVswr === null ? null : clean(maxVswr, 0.01)
  };
  // Unknown values are left out: in a delta, null means "field removed".
  for (const k of Object.keys(s)) if (s[k] === null) delete s[k];
  return { summary: s, alarms };
}

function event(id, a, stateName) {
  state.seq++;
  const ev = { seq: state.seq, id, node: a.node, code: a.code, sev: a.sev, state: stateName, at: Date.now() };
  if (a.text) ev.text = a.text;
  state.outbox.push(ev);
  if (state.outbox.length > cfg.outboxMax) { state.outbox.splice(0, state.outbox.length - cfg.outboxMax); state.overflow = true; }
  return ev;
}

// Compares the new picture with the last one: summary delta and alarm events.
function update(vd) {
  const { summary, alarms } = summarize(vd);
  const out = [];
  state.summary = summary;
  state.hash = fnv1a64(canonical(summary));
  if (state.lastSent) {
    const changed = {};
    for (const [k, v] of Object.entries(summary)) {
      const c = canonical(v);
      if (state.lastSent[k] !== c) { changed[k] = v; state.lastSent[k] = c; }
    }
    for (const k of Object.keys(state.lastSent)) {
      if (!(k in summary)) { changed[k] = null; delete state.lastSent[k]; }
    }
    if (Object.keys(changed).length) {
      const base = state.rev;
      state.rev++;
      out.push({ t: 'delta', base, rev: state.rev, hash: state.hash, s: changed });
    }
  }
  const evs = [];
  for (const [id, a] of alarms) {
    if (!state.active.has(id)) { state.active.set(id, event(id, a, 'raised')); evs.push(state.active.get(id)); }
  }
  for (const [id, a] of state.active) {
    if (!alarms.has(id)) { state.active.delete(id); evs.push(event(id, a, 'cleared')); }
  }
  if (evs.length) out.push({ t: 'alarms', ev: evs });
  return out;
}

function fullSummary() {
  state.lastSent = {};
  for (const [k, v] of Object.entries(state.summary)) state.lastSent[k] = canonical(v);
  state.rev++;
  return { t: 'summary', rev: state.rev, hash: state.hash, s: state.summary };
}
const fullAlarms = () => ({ t: 'alarms', full: true, upTo: state.seq, ev: [...state.active.values()].sort((a, b) => a.seq - b.seq) });

function detailMessage(full) {
  const nodes = [];
  for (const n of state.nodes.values()) {
    const c = JSON.stringify(n);
    if (full || state.detailSent.get(n.id) !== c) { nodes.push(n); state.detailSent.set(n.id, c); }
  }
  const gone = full ? [] : [...state.detailSent.keys()].filter((id) => !state.nodes.has(id));
  for (const id of gone) state.detailSent.delete(id);
  if (!full && !nodes.length && !gone.length) return null;
  const base = state.detailRev;
  state.detailRev++;
  const m = { t: 'detail', rev: state.detailRev, nodes };
  if (full) m.full = true; else { m.base = base; if (gone.length) m.gone = gone; }
  return m;
}

// ------------------------------------------------------------------ local API
function getJSON(url) {
  return new Promise((resolve, reject) => {
    const lib = url.startsWith('https:') ? https : http;
    const req = lib.get(url, { timeout: 4000, headers: { Accept: 'application/json' } }, (res) => {
      if (res.statusCode !== 200) { res.resume(); reject(new Error('HTTP ' + res.statusCode)); return; }
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => { try { resolve(JSON.parse(Buffer.concat(chunks).toString('utf8'))); } catch (e) { reject(e); } });
    });
    req.on('timeout', () => req.destroy(new Error('timeout')));
    req.on('error', reject);
  });
}

// ------------------------------------------------------------------ uplink
let ws = null;
let welcomed = false;
let attempt = 0;
let pingTimer = null;
let lastPong = 0;
let pingSeq = 0;

function send(m) {
  if (ws && welcomed && ws.readyState === 1) { ws.send(JSON.stringify(m)); return true; }
  return false;
}

function onWelcome(w) {
  welcomed = true;
  attempt = 0;
  const have = w.have;
  const out = [];
  if (!have || have.boot !== state.boot) {
    out.push(fullSummary(), fullAlarms());
  } else {
    if (have.rev !== state.rev || have.hash !== state.hash) out.push(fullSummary());
    state.outbox = state.outbox.filter((e) => e.seq > have.ackSeq);
    if (state.outbox.length) out.push(state.outbox[0].seq !== have.ackSeq + 1 || state.overflow ? fullAlarms() : { t: 'alarms', ev: state.outbox.slice() });
  }
  for (const m of out) send(m);
  log('connected', { site: w.site, tenant: w.tenant, resumed: !!have, sent: out.map((m) => m.t) });
  const every = (w.keepaliveS || 25) * 1000;
  lastPong = Date.now();
  clearInterval(pingTimer);
  pingTimer = setInterval(() => {
    if (Date.now() - lastPong > 2.5 * every) { log('noc_silent', {}); ws.close(); return; } // half-open link
    send({ t: 'ping', seq: ++pingSeq });
  }, every);
}

function onMessage(raw) {
  let m;
  try { m = JSON.parse(typeof raw === 'string' ? raw : raw.toString()); } catch (e) { return; }
  lastPong = Date.now();
  switch (m.t) {
    case 'welcome': onWelcome(m); break;
    case 'ack': state.outbox = state.outbox.filter((e) => e.seq > m.seq); if (!state.outbox.length) state.overflow = false; break;
    case 'resync':
      if (m.what === 'summary') send(fullSummary());
      else if (m.what === 'alarms') send(fullAlarms());
      else if (m.what === 'detail') send(detailMessage(true));
      break;
    case 'detail-sub':
      state.detailOn = !!m.on;
      state.detailEvery = Math.max(500, m.intervalMs || 1000);
      clearInterval(state.detailTimer);
      if (state.detailOn) {
        state.detailSent.clear();
        send(detailMessage(true));
        state.detailTimer = setInterval(() => { const d = detailMessage(false); if (d) send(d); }, state.detailEvery);
      }
      break;
    case 'pong': break;
  }
}

function connect() {
  welcomed = false;
  try {
    ws = new WS(cfg.nocUrl, ['das-noc.v1', 'bearer.' + cfg.token]);
  } catch (e) { return retry(); }
  const on = (ev, fn) => (ws.addEventListener ? ws.addEventListener(ev, fn) : ws.on(ev, fn));
  on('open', () => ws.send(JSON.stringify({ t: 'hello', boot: state.boot, fw: cfg.fw, agent: 'noc-agent.js/1', caps: ['detail'] })));
  on('message', (e) => onMessage(e && e.data !== undefined ? e.data : e));
  on('close', () => {
    clearInterval(pingTimer);
    clearInterval(state.detailTimer);
    state.detailOn = false;
    if (welcomed) log('disconnected', {});
    welcomed = false;
    retry();
  });
  on('error', () => { /* followed by close */ });
}

function retry() {
  // Full jitter; the browser-style WebSocket API does not expose the NOC's
  // Retry-After, so exponential backoff spreads a reconnect storm instead.
  const ceil = Math.min(60000, 1000 * 2 ** Math.min(attempt, 10));
  attempt++;
  setTimeout(connect, Math.floor(Math.random() * ceil));
}

async function poll() {
  try {
    const vd = await getJSON(cfg.localApi.replace(/\/$/, '') + '/api/volatile-data');
    for (const m of update(vd)) send(m);
  } catch (e) {
    log('local_api_error', { error: String(e.message || e) });
  }
}

if (require.main === module) {
  if (!cfg.token) { console.error('NOC_TOKEN is required'); process.exit(2); }
  if (!WS) {
    try { WS = require('ws'); } catch (e) { console.error('needs Node 22+ (global WebSocket) or the "ws" package'); process.exit(2); }
  }
  (async () => {
    await poll(); // the first summary before the first connect
    connect();
    setInterval(poll, cfg.pollMs);
    log('started', { boot: state.boot, noc: cfg.nocUrl, localApi: cfg.localApi, pollMs: cfg.pollMs });
  })();
}

module.exports = { canonical, fnv1a64, summarize, update, state };
