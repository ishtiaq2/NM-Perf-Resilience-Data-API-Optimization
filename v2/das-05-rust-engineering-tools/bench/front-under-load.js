#!/usr/bin/env node
'use strict';
/**
 * The standard scenario of every project in this programme (4 engineers x 2
 * legacy spectrum traces of 50 001 points + dashboards + config reads), against
 * the "safest path": das-engtools (Rust) in front of the UNCHANGED shipped
 * Node.js application (das-01 in --legacy mode, i.e. the release with the bug).
 *
 * Both processes share ONE core, like a single-core Master Unit, with the
 * embedded CPU emulated (x8) and 10 % of that core used by "other daemons".
 * The load generator runs on another core.
 *
 *   cargo build --release && node bench/front-under-load.js [--duration 30] \
 *     [--node-mode legacy|hotfix] [--topology front|node-front] \
 *     [--compare-with ../das-03-lts-go-edge/bench/results/edge-XYZ.json | --no-compare]
 *
 * --topology node-front measures option C instead: the Node.js app keeps the port and
 * pipes the engineering routes to das-engtools (examples/node-in-front).
 *
 * By default the newest das-03 result file (legacy, hotfix, gateway, Go edge)
 * is used for the comparison. Linux recommended (taskset, /proc). Node 18+.
 */
const { spawn, spawnSync } = require('node:child_process');
const { parseArgs } = require('node:util');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { renderReport } = require('./report-html');

const { values: a } = parseArgs({ options: {
  duration: { type: 'string', default: '30' },
  'cpu-slowdown': { type: 'string', default: '8' },
  'background-load': { type: 'string', default: '10' },
  spectrum: { type: 'string', default: 'legacy' },
  port: { type: 'string', default: '18490' },
  'node-port': { type: 'string', default: '18491' },
  'node-mode': { type: 'string', default: 'legacy' },
  'node-app': { type: 'string', default: path.join(__dirname, '..', '..', 'das-01-hotfix-node') },
  // front: das-engtools owns the port (option B). node-front: the Node.js app owns it and
  // pipes the engineering routes to das-engtools on a Unix socket (option C).
  topology: { type: 'string', default: 'front' },
  label: { type: 'string', default: '' },
  'compare-with': { type: 'string' },
  'no-compare': { type: 'boolean', default: false }
} });

const root = path.join(__dirname, '..');
const bin = path.join(root, 'target', 'release', 'das-engtools');
if (!fs.existsSync(bin)) { console.error('target/release/das-engtools not found: run `cargo build --release` first'); process.exit(2); }
const nodeServer = path.join(a['node-app'], 'src', 'server.js');
if (!fs.existsSync(nodeServer)) { console.error(`${nodeServer} not found (--node-app)`); process.exit(2); }
const canPin = process.platform === 'linux' && os.cpus().length >= 2 && spawnSync('taskset', ['-V']).status === 0;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const base = `http://127.0.0.1:${a.port}`;
const nodeFront = a.topology === 'node-front';
if (!['front', 'node-front'].includes(a.topology)) { console.error('--topology front|node-front'); process.exit(2); }
const nodeKind = a['node-mode'] === 'legacy' ? 'shipped' : 'hotfix';
const label = a.label || (nodeFront ? `${nodeKind} Node front + Rust tools (05)` : `Rust front + ${nodeKind} Node (05)`);
const sock = path.join(os.tmpdir(), `das-engtools-bench-${process.pid}.sock`);

function pssMb(pid) {
  try {
    const m = /Pss:\s+(\d+)/.exec(fs.readFileSync(`/proc/${pid}/smaps_rollup`, 'utf8'));
    return m ? Number(m[1]) / 1024 : null;
  } catch { return null; }
}
const pinned = (cpu, cmd, args, opts) => spawn(canPin ? 'taskset' : cmd, canPin ? ['-c', cpu, cmd, ...args] : args, opts);
const summary = (xs) => (xs.length ? { startMb: Math.round(xs[0]), peakMb: Math.round(Math.max(...xs)), endMb: Math.round(xs[xs.length - 1]) } : null);

function newestEdgeResult() {
  const dir = path.join(root, '..', 'das-03-lts-go-edge', 'bench', 'results');
  try {
    const f = fs.readdirSync(dir).filter((n) => /^edge-.*\.json$/.test(n)).sort().pop();
    return f ? path.join(dir, f) : null;
  } catch { return null; }
}

async function waitOk(url, tries = 150) {
  for (let i = 0; i < tries; i++) {
    try { if ((await fetch(url)).status === 200) return true; } catch { /* starting */ }
    await sleep(100);
  }
  return false;
}

(async () => {
  fs.mkdirSync(path.join(root, '.run'), { recursive: true });
  const emu = { DAS_CPU_SLOWDOWN: a['cpu-slowdown'] };
  const nodeLog = fs.openSync(path.join(root, '.run', 'bench-node.log'), 'w');
  const rustLog = fs.openSync(path.join(root, '.run', 'bench-rust.log'), 'w');
  // MALLOC_ARENA_MAX=2 for Node.js, as in every other run of the programme.
  const nodeEnv = { ...process.env, ...emu, DAS_MODE: a['node-mode'], HOST: '127.0.0.1', DAS_SPECTRUM_POINTS: '50001', MALLOC_ARENA_MAX: '2' };
  let node, rust, healthUrl;
  if (nodeFront) {
    // Option C: das-engtools on a Unix socket, the Node.js app (plus engtools-forward.js) on the port.
    rust = pinned('0', bin, ['--listen', `unix:${sock}`, '--log-format', 'text'], { env: { ...process.env, ...emu }, stdio: ['ignore', rustLog, rustLog] });
    for (let i = 0; i < 50 && !fs.existsSync(sock); i++) await sleep(100);
    node = pinned('0', process.execPath, [path.join(root, 'examples', 'node-in-front', 'demo-das01.js')], {
      env: { ...nodeEnv, PORT: a.port, DAS_ENGTOOLS_SOCKET: sock }, stdio: ['ignore', nodeLog, nodeLog]
    });
    healthUrl = `${base}/api/heartbeat`;
  } else {
    // Option B: the Node.js application moves to a loopback port; das-engtools owns the public
    // port and forwards everything it does not implement to Node.
    node = pinned('0', process.execPath, [nodeServer], { cwd: a['node-app'], env: { ...nodeEnv, PORT: a['node-port'] }, stdio: ['ignore', nodeLog, nodeLog] });
    rust = pinned('0', bin, ['--listen', `127.0.0.1:${a.port}`, '--legacy', `http://127.0.0.1:${a['node-port']}`, '--log-format', 'text'], {
      env: { ...process.env, ...emu }, stdio: ['ignore', rustLog, rustLog]
    });
    healthUrl = `${base}/internal/health`;
  }
  const burner = Number(a['background-load']) > 0
    ? pinned('0', process.execPath, [path.join(__dirname, 'cpu-burner.js'), a['background-load']], { stdio: 'ignore' })
    : null;
  try {
    if (!nodeFront && !(await waitOk(`http://127.0.0.1:${a['node-port']}/api/heartbeat`))) throw new Error('Node app did not start (see .run/bench-node.log)');
    if (!(await waitOk(healthUrl))) throw new Error('the server did not start (see .run/bench-*.log)');
    await sleep(2000);
    const out = path.join(os.tmpdir(), `das-front-bench-${process.pid}.json`);
    const benchArgs = [path.join(__dirname, 'heartbeat-under-load.js'), '--base', base, '--duration', a.duration,
      '--browsers', '4', '--analyzers-per-browser', '2', '--spectrum', a.spectrum, '--label', label, '--out', out];
    const pss = { rust: [], node: [], total: [] };
    // taskset execs the program in place, so the child pids are the servers' pids.
    const timer = setInterval(() => {
      const r = pssMb(rust.pid), n = pssMb(node.pid);
      if (r != null) pss.rust.push(r);
      if (n != null) pss.node.push(n);
      if (r != null && n != null) pss.total.push(r + n);
    }, 1000);
    const bench = pinned('1', process.execPath, benchArgs, { stdio: 'inherit' });
    const code = await new Promise((r) => bench.on('exit', r));
    clearInterval(timer);
    if (code !== 0) throw new Error('benchmark failed');
    const result = JSON.parse(fs.readFileSync(out, 'utf8'));
    fs.unlinkSync(out);
    const t = summary(pss.total);
    if (t) {
      result.server = { rssStartMb: t.startMb, rssPeakMb: t.peakMb, rssEndMb: t.endMb, note: 'PSS of das-engtools + the Node.js app',
        parts: { dasEngtools: summary(pss.rust), node: summary(pss.node) } };
    }
    if (!nodeFront) {
      try { result.engtoolsMetrics = await (await fetch(`${base}/internal/metrics`)).json(); } catch { /* optional */ }
    }
    const runs = [];
    const cmp = a['no-compare'] ? null : (a['compare-with'] || newestEdgeResult());
    if (cmp) runs.push(...JSON.parse(fs.readFileSync(cmp, 'utf8')));
    runs.push(result);
    const dir = path.join(root, 'bench', 'results');
    fs.mkdirSync(dir, { recursive: true });
    const stamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
    const foot = `Measured ${new Date().toISOString().slice(0, 16).replace('T', ' ')} UTC on ${(os.cpus()[0] || {}).model}. ` +
      (canPin ? 'das-engtools and the Node.js app share ONE core; load generator on another core. ' : '') +
      (nodeFront ? 'Node.js pipes the engineering routes to das-engtools; the emulation does not slow down that piping. ' : '') +
      `Embedded CPU emulated (DAS_CPU_SLOWDOWN=${a['cpu-slowdown']}) in both processes, ${a['background-load']} % background load on the server core.` +
      (cmp ? ` Earlier runs from ${path.relative(root, cmp)}.` : '');
    fs.writeFileSync(path.join(dir, `front-${stamp}.json`), JSON.stringify(runs, null, 2));
    fs.writeFileSync(path.join(dir, `front-${stamp}.html`), renderReport(runs, {
      subtitle: 'Shipped behaviour vs hotfix vs gateway vs Go edge vs the Rust engineering-tools front (Node.js unchanged behind it), same load.',
      footnote: foot
    }));
    fs.copyFileSync(path.join(dir, `front-${stamp}.html`), path.join(dir, 'latest.html'));
    const s = result.server || {}, p = s.parts || {};
    console.log(`\n${label}: heartbeat p99 ${result.heartbeat.p99} ms, ${result.heartbeat.timeouts} timeouts, ${result.heartbeat.alarmsStrict} false alarms`);
    console.log(`PSS peak: das-engtools ${(p.dasEngtools || {}).peakMb} MB + Node.js ${(p.node || {}).peakMb} MB = ${s.rssPeakMb} MB`);
    console.log(`Report: bench/results/front-${stamp}.html (latest.html)`);
  } finally {
    if (burner) burner.kill('SIGKILL');
    rust.kill('SIGTERM');
    node.kill('SIGTERM');
    try { fs.unlinkSync(sock); } catch { /* not created */ }
  }
})().catch((e) => { console.error(e); process.exitCode = 1; });
