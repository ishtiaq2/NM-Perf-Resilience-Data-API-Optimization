#!/usr/bin/env node
'use strict';
/**
 * Same scenario as das-01's `npm run demo`, against the gateway stack:
 * 4 engineers x 2 spectrum traces + dashboards + config reads. nginx and all
 * three services are pinned to ONE core with the embedded CPU emulated (x8), and
 * 10 % of that core goes to "other daemons". Writes an HTML/Markdown report.
 *
 *   node bench/gateway-under-load.js [--duration 30] [--compare-with ../das-01-hotfix-node/bench/results/compare-XYZ.json]
 * Linux + nginx required. Needs Node 18+.
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
  'compare-with': { type: 'string' }
} });

const root = path.join(__dirname, '..');
const canPin = process.platform === 'linux' && os.cpus().length >= 2 && spawnSync('taskset', ['-V']).status === 0;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** Total PSS (proportional set size, shared pages split fairly) of nginx + the 3 services, in MB. */
function stackPssMb() {
  const pids = [];
  for (const n of ['core-api', 'spectrum', 'telemetry', 'nginx']) {
    try { pids.push(fs.readFileSync(path.join(root, '.run', `${n}.pid`), 'utf8').trim()); } catch { /* not running */ }
  }
  const nginx = pids[pids.length - 1];
  for (const d of fs.readdirSync('/proc').filter((x) => /^\d+$/.test(x))) {
    try { const st = fs.readFileSync(`/proc/${d}/stat`, 'utf8'); if (st.slice(st.lastIndexOf(')') + 2).split(' ')[1] === nginx) pids.push(d); } catch { /* gone */ }
  }
  let kb = 0;
  for (const pid of pids) {
    try { const m = /Pss:\s+(\d+)/.exec(fs.readFileSync(`/proc/${pid}/smaps_rollup`, 'utf8')); if (m) kb += Number(m[1]); } catch { /* gone */ }
  }
  return Math.round(kb / 1024);
}

(async () => {
  spawnSync('sh', ['scripts/run-local.sh', 'stop'], { cwd: root });
  const env = { ...process.env, DAS_CPU_SLOWDOWN: a['cpu-slowdown'], MALLOC_ARENA_MAX: '2' };
  if (canPin) env.PIN_CPU = '0';
  const stack = spawn('sh', ['scripts/run-local.sh'], { cwd: root, env, stdio: 'ignore' });
  const burner = Number(a['background-load']) > 0 ? spawn(canPin ? 'taskset' : process.execPath, canPin ? ['-c', '0', process.execPath, path.join(__dirname, 'cpu-burner.js'), a['background-load']] : [path.join(__dirname, 'cpu-burner.js'), a['background-load']], { stdio: 'ignore' }) : null;
  try {
    for (let i = 0; i < 100; i++) {
      try { if ((await fetch('http://127.0.0.1:8080/api/heartbeat')).status === 200) break; } catch { /* starting */ }
      await sleep(200);
    }
    await sleep(2000);
    const out = path.join(os.tmpdir(), `das-gw-bench-${process.pid}.json`);
    const benchArgs = [path.join(__dirname, 'heartbeat-under-load.js'), '--base', 'http://127.0.0.1:8080', '--duration', a.duration,
      '--browsers', '4', '--analyzers-per-browser', '2', '--spectrum', a.spectrum, '--label', 'gateway (02)', '--out', out];
    const pss = [];
    const pssTimer = setInterval(() => { try { pss.push(stackPssMb()); } catch { /* not Linux */ } }, 1000);
    const bench = spawn(canPin ? 'taskset' : process.execPath, canPin ? ['-c', '1', process.execPath, ...benchArgs] : benchArgs, { stdio: 'inherit' });
    const code = await new Promise((r) => bench.on('exit', r));
    clearInterval(pssTimer);
    if (code !== 0) throw new Error('benchmark failed');
    const result = JSON.parse(fs.readFileSync(out, 'utf8'));
    fs.unlinkSync(out);
    if (pss.length) result.server = { rssStartMb: pss[0], rssPeakMb: Math.max(...pss), rssEndMb: pss[pss.length - 1], note: 'PSS of nginx + 3 services' };
    const runs = [];
    if (a['compare-with']) runs.push(...JSON.parse(fs.readFileSync(a['compare-with'], 'utf8')));
    runs.push(result);
    const dir = path.join(root, 'bench', 'results');
    fs.mkdirSync(dir, { recursive: true });
    const stamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
    const foot = `Measured ${new Date().toISOString().slice(0, 16).replace('T', ' ')} UTC on ${(os.cpus()[0] || {}).model}, Node ${process.version}. ` +
      (canPin ? 'nginx + 3 services pinned to ONE core; load generator on another core. ' : '') +
      `Embedded CPU emulated (DAS_CPU_SLOWDOWN=${a['cpu-slowdown']}), ${a['background-load']} % background load on the device core.`;
    fs.writeFileSync(path.join(dir, `gateway-${stamp}.json`), JSON.stringify(runs, null, 2));
    fs.writeFileSync(path.join(dir, `gateway-${stamp}.html`), renderReport(runs, { subtitle: 'Gateway + split services under the das-01 demo load.', footnote: foot }));
    fs.copyFileSync(path.join(dir, `gateway-${stamp}.html`), path.join(dir, 'latest.html'));
    console.log(`\nReport: bench/results/gateway-${stamp}.html (latest.html)`);
  } finally {
    if (burner) burner.kill('SIGKILL');
    spawnSync('sh', ['scripts/run-local.sh', 'stop'], { cwd: root });
    stack.kill('SIGTERM');
  }
})().catch((e) => { console.error(e); process.exit(1); });
