#!/usr/bin/env node
'use strict';
/**
 * Before/after demo: runs the same load against the server in legacy mode
 * (the shipped behaviour) and in hotfix mode, then prints a comparison and
 * writes Markdown, JSON and HTML reports to bench/results/.
 *
 *   npm run demo                          # 30 s per mode, embedded CPU emulated (x8), 10 % background load
 *   npm run demo:raw                      # same, at full laptop speed (x1)
 *   node bench/compare.js --duration 60 --browsers 6 --points 100001
 *
 * On Linux with >= 2 CPUs the server is pinned to CPU 0 and the load generator
 * to CPU 1 (taskset). The server then runs on a single core, like a small
 * embedded SoC, and the load generator does not steal its CPU.
 * Requires Node 18+ on the machine running the demo.
 */

const { spawn, spawnSync } = require('node:child_process');
const { parseArgs } = require('node:util');
const path = require('node:path');
const fs = require('node:fs');
const os = require('node:os');
const http = require('node:http');
const { renderReport } = require('./report-html');

const { values: a } = parseArgs({
  options: {
    duration: { type: 'string', default: '30' },
    // Default scenario: 4 engineers, each watching 2 traces (e.g. DL + UL port).
    browsers: { type: 'string', default: '4' },
    'analyzers-per-browser': { type: 'string', default: '2' },
    points: { type: 'string', default: '50001' },
    'hb-timeout': { type: 'string', default: '3000' },
    'hb-interval': { type: 'string', default: '1000' },
    spectrum: { type: 'string', default: 'legacy' },
    modes: { type: 'string', default: 'legacy,hotfix' },
    // 8 ~ Cortex-A53-class SoC vs. a modern laptop core. Use --cpu-slowdown 1 on the real device.
    'cpu-slowdown': { type: 'string', default: '8' },
    // Share of the server's core used by "other daemons" on the Master Node (0 = none).
    'background-load': { type: 'string', default: '10' },
    'no-pin': { type: 'boolean', default: false }
  }
});

const root = path.join(__dirname, '..');
const canPin = !a['no-pin'] && process.platform === 'linux' && os.cpus().length >= 2 && spawnSync('taskset', ['-V']).status === 0;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function waitReady(port, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  return new Promise((resolve, reject) => {
    (function poll() {
      const req = http.get({ host: '127.0.0.1', port, path: '/api/heartbeat', timeout: 1000 }, (res) => { res.resume(); res.statusCode === 200 ? resolve() : retry(); });
      req.on('error', retry);
      req.on('timeout', () => { req.destroy(); });
      function retry() { if (Date.now() > deadline) reject(new Error('server did not start')); else setTimeout(poll, 250); }
    })();
  });
}

function run(cmd, args, opts) {
  const full = canPin ? ['taskset', ['-c', opts.cpu, cmd, ...args]] : [cmd, args];
  return spawn(full[0], full[1], { cwd: root, env: { ...process.env, ...(opts.env || {}) }, stdio: opts.stdio || 'inherit' });
}

async function runMode(mode, port) {
  const logFile = path.join(root, 'bench', 'results', `server-${mode}.log`);
  const log = fs.openSync(logFile, 'w');
  const server = run(process.execPath, ['src/server.js'], {
    cpu: '0',
    // MALLOC_ARENA_MAX=2 is part of the recommended deployment (see deploy/); applied to both modes.
    env: { DAS_MODE: mode, PORT: String(port), HOST: '127.0.0.1', DAS_SPECTRUM_POINTS: a.points, DAS_CPU_SLOWDOWN: a['cpu-slowdown'], MALLOC_ARENA_MAX: '2' },
    stdio: ['ignore', log, log]
  });
  const burner = Number(a['background-load']) > 0 ? run(process.execPath, [path.join(__dirname, 'cpu-burner.js'), a['background-load']], { cpu: '0', stdio: 'ignore' }) : null;
  const rss = [];
  const rssTimer = setInterval(() => {
    try {
      const m = /VmRSS:\s+(\d+)/.exec(fs.readFileSync(`/proc/${server.pid}/status`, 'utf8'));
      if (m) rss.push(Number(m[1]) / 1024);
    } catch { /* not Linux or process gone */ }
  }, 1000);
  try {
    await waitReady(port, 30000);
    await sleep(1500); // let the node simulator settle
    const out = path.join(os.tmpdir(), `das-bench-${mode}-${process.pid}.json`);
    const bench = run(process.execPath, [path.join(__dirname, 'heartbeat-under-load.js'),
      '--base', `http://127.0.0.1:${port}`, '--duration', a.duration, '--browsers', a.browsers, '--analyzers-per-browser', a['analyzers-per-browser'], '--points', a.points,
      '--hb-timeout', a['hb-timeout'], '--hb-interval', a['hb-interval'], '--spectrum', a.spectrum, '--label', mode, '--out', out], { cpu: '1' });
    const code = await new Promise((r) => bench.on('exit', r));
    if (code !== 0) throw new Error(`benchmark exited with ${code}`);
    const result = JSON.parse(fs.readFileSync(out, 'utf8'));
    fs.unlinkSync(out);
    if (rss.length) result.server = { rssStartMb: Math.round(rss[0]), rssPeakMb: Math.round(Math.max(...rss)), rssEndMb: Math.round(rss[rss.length - 1]) };
    return result;
  } finally {
    clearInterval(rssTimer);
    if (burner) burner.kill('SIGKILL');
    server.kill('SIGTERM');
    await new Promise((r) => server.on('exit', r));
    fs.closeSync(log);
  }
}

function markdown(runs, env) {
  const cols = runs.map((r) => r.label);
  const line = (name, f) => `| ${name} | ${runs.map(f).join(' | ')} |`;
  return [
    `# Heartbeat under spectrum-analyzer load`,
    '',
    env,
    '',
    `| Metric | ${cols.join(' | ')} |`,
    `|---|${cols.map(() => '---:').join('|')}|`,
    line('Heartbeat p50', (r) => `${r.heartbeat.p50} ms`),
    line('Heartbeat p99', (r) => `${r.heartbeat.p99} ms`),
    line('Heartbeat max', (r) => `${r.heartbeat.max} ms`),
    line('Heartbeat timeouts', (r) => `${r.heartbeat.timeouts} / ${r.heartbeat.n}`),
    line('False "server dead" alarms (1-miss rule)', (r) => r.heartbeat.alarmsStrict),
    line('False "server dead" alarms (3-miss rule)', (r) => r.heartbeat.alarmsTolerant),
    line('Server event-loop lag p99 (worst)', (r) => (r.heartbeat.serverLagP99MaxMs == null ? 'n/a' : `${r.heartbeat.serverLagP99MaxMs} ms`)),
    line('Spectrum responses (200 / 304)', (r) => `${r.spectrum.ok} / ${r.spectrum.notModified}`),
    line('Spectrum latency p50 / p95', (r) => `${r.spectrum.p50} / ${r.spectrum.p95} ms`),
    line('Spectrum updates per trace per minute', (r) => Math.round((r.spectrum.ok / (r.params.browsers * r.params.analyzersPerBrowser) / r.params.durationS) * 600) / 10),
    line('Hardware sweeps performed', (r) => r.spectrum.distinctSweepsDelivered),
    line('Spectrum data transferred', (r) => `${r.spectrum.mbReceived} MB`),
    line('Dashboard (volatile-data) p95', (r) => `${r.dashboard.p95} ms`),
    line('Dashboard 304 responses', (r) => `${r.dashboard.notModified} / ${r.dashboard.n}`),
    line('Dashboard data transferred', (r) => `${r.dashboard.kbReceived} kB`),
    line('Config read p95 / max', (r) => `${r.configApi.p95} / ${r.configApi.max} ms`),
    line('Server RSS start / peak', (r) => (r.server ? `${r.server.rssStartMb} / ${r.server.rssPeakMb} MB` : 'n/a')),
    ''
  ].join('\n');
}

(async () => {
  fs.mkdirSync(path.join(root, 'bench', 'results'), { recursive: true });
  const modes = a.modes.split(',').map((s) => s.trim()).filter(Boolean);
  const runs = [];
  for (let i = 0; i < modes.length; i++) runs.push(await runMode(modes[i], 18180 + i));
  const cpu = (os.cpus()[0] || {}).model || 'unknown CPU';
  const env = `Measured ${new Date().toISOString().slice(0, 16).replace('T', ' ')} UTC on ${cpu}, Node ${process.version}` +
    (canPin ? ', server pinned to 1 CPU core (taskset -c 0), load generator on another core.' : ' (no CPU pinning).') +
    (Number(a['cpu-slowdown']) > 1 ? ` Embedded CPU emulated with DAS_CPU_SLOWDOWN=${a['cpu-slowdown']} (all heavy CPU work runs ${a['cpu-slowdown']}x, in both modes).` : ' Absolute numbers on an embedded ARM SoC will be several times higher; the ratio is what matters.') +
    (Number(a['background-load']) > 0 ? ` Other Master Node daemons emulated by a ${a['background-load']} % CPU load on the server core.` : '');
  const md = markdown(runs, env);
  const stamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
  const base = path.join(root, 'bench', 'results');
  fs.writeFileSync(path.join(base, `compare-${stamp}.json`), JSON.stringify(runs, null, 2));
  fs.writeFileSync(path.join(base, `compare-${stamp}.md`), md);
  fs.writeFileSync(path.join(base, `compare-${stamp}.html`), renderReport(runs, { subtitle: 'DAS Master Node backend, legacy vs hotfix.', footnote: env }));
  fs.copyFileSync(path.join(base, `compare-${stamp}.html`), path.join(base, 'latest.html'));
  fs.copyFileSync(path.join(base, `compare-${stamp}.md`), path.join(base, 'latest.md'));
  console.log('\n' + md);
  console.log(`Reports: bench/results/compare-${stamp}.{md,json,html} (latest.html = newest)`);
})().catch((err) => { console.error(err); process.exit(1); });
