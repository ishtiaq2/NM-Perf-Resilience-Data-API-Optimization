#!/usr/bin/env node
'use strict';
/**
 * Same scenario as das-01's `npm run demo` and das-02's gateway benchmark,
 * against the Go edge: 4 engineers x 2 legacy spectrum traces (50 001 points)
 * + dashboards + config reads. The edge is pinned to ONE core with the embedded
 * CPU emulated (x8), and 10 % of that core goes to "other daemons".
 *
 *   make build && node bench/edge-under-load.js [--duration 30] \
 *     [--compare-with ../das-02-gateway-multiservice/bench/results/gateway-XYZ.json]
 *
 * Linux recommended (taskset, /proc). Needs Node 18+ for the load generator only.
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
  port: { type: 'string', default: '18480' },
  label: { type: 'string', default: 'Go edge (03)' },
  'compare-with': { type: 'string' }
} });

const root = path.join(__dirname, '..');
const bin = path.join(root, 'bin', 'das-edge');
if (!fs.existsSync(bin)) { console.error('bin/das-edge not found: run `make build` first'); process.exit(2); }
const canPin = process.platform === 'linux' && os.cpus().length >= 2 && spawnSync('taskset', ['-V']).status === 0;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const base = `http://127.0.0.1:${a.port}`;

function pssMb(pid) {
  try {
    const m = /Pss:\s+(\d+)/.exec(fs.readFileSync(`/proc/${pid}/smaps_rollup`, 'utf8'));
    return m ? Math.round(Number(m[1]) / 1024) : null;
  } catch { return null; }
}

(async () => {
  const env = { ...process.env, DAS_CPU_SLOWDOWN: a['cpu-slowdown'], DAS_LOG_FORMAT: 'text' };
  const args = ['-listen', `127.0.0.1:${a.port}`];
  fs.mkdirSync(path.join(root, '.run'), { recursive: true });
  const log = fs.openSync(path.join(root, '.run', 'edge-bench.log'), 'w');
  const edge = spawn(canPin ? 'taskset' : bin, canPin ? ['-c', '0', bin, ...args] : args, { env, stdio: ['ignore', log, log] });
  const burner = Number(a['background-load']) > 0 ? spawn(canPin ? 'taskset' : process.execPath, canPin ? ['-c', '0', process.execPath, path.join(__dirname, 'cpu-burner.js'), a['background-load']] : [path.join(__dirname, 'cpu-burner.js'), a['background-load']], { stdio: 'ignore' }) : null;
  try {
    for (let i = 0; i < 100; i++) {
      try { if ((await fetch(`${base}/api/heartbeat`)).status === 200) break; } catch { /* starting */ }
      await sleep(100);
    }
    await sleep(2000);
    const out = path.join(os.tmpdir(), `das-edge-bench-${process.pid}.json`);
    const benchArgs = [path.join(__dirname, 'heartbeat-under-load.js'), '--base', base, '--duration', a.duration,
      '--browsers', '4', '--analyzers-per-browser', '2', '--spectrum', a.spectrum, '--label', a.label, '--out', out];
    const pss = [];
    // taskset execs the edge in place, so the child pid is the edge's pid.
    const pssTimer = setInterval(() => { const v = pssMb(edge.pid); if (v != null) pss.push(v); }, 1000);
    const bench = spawn(canPin ? 'taskset' : process.execPath, canPin ? ['-c', '1', process.execPath, ...benchArgs] : benchArgs, { stdio: 'inherit' });
    const code = await new Promise((r) => bench.on('exit', r));
    clearInterval(pssTimer);
    if (code !== 0) throw new Error('benchmark failed');
    const result = JSON.parse(fs.readFileSync(out, 'utf8'));
    fs.unlinkSync(out);
    if (pss.length) result.server = { rssStartMb: pss[0], rssPeakMb: Math.max(...pss), rssEndMb: pss[pss.length - 1], note: 'PSS of the das-edge process' };
    try { result.edgeMetrics = await (await fetch(`${base}/api/metrics`)).json(); } catch { /* optional */ }
    const runs = [];
    if (a['compare-with']) runs.push(...JSON.parse(fs.readFileSync(a['compare-with'], 'utf8')));
    runs.push(result);
    const dir = path.join(root, 'bench', 'results');
    fs.mkdirSync(dir, { recursive: true });
    const stamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
    const foot = `Measured ${new Date().toISOString().slice(0, 16).replace('T', ' ')} UTC on ${(os.cpus()[0] || {}).model}. ` +
      (canPin ? 'Server pinned to ONE core; load generator on another core. ' : '') +
      `Embedded CPU emulated (DAS_CPU_SLOWDOWN=${a['cpu-slowdown']}), ${a['background-load']} % background load on the server core.`;
    fs.writeFileSync(path.join(dir, `edge-${stamp}.json`), JSON.stringify(runs, null, 2));
    fs.writeFileSync(path.join(dir, `edge-${stamp}.html`), renderReport(runs, { subtitle: 'Legacy vs hotfix vs gateway vs Go edge under the same load.', footnote: foot }));
    fs.copyFileSync(path.join(dir, `edge-${stamp}.html`), path.join(dir, 'latest.html'));
    const s = result.server || {};
    console.log(`\nGo edge: heartbeat p99 ${result.heartbeat.p99} ms, ${result.heartbeat.timeouts} timeouts, ${result.heartbeat.alarmsStrict} false alarms; PSS peak ${s.rssPeakMb} MB`);
    console.log(`Report: bench/results/edge-${stamp}.html (latest.html)`);
  } finally {
    if (burner) burner.kill('SIGKILL');
    edge.kill('SIGTERM');
  }
})().catch((e) => { console.error(e); process.exit(1); });
