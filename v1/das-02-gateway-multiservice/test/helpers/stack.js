'use strict';
/**
 * Start the full gateway stack for tests: three services in-process on Unix
 * sockets in a temp dir, plus a real nginx (skipped if nginx is not installed).
 */
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn, spawnSync } = require('node:child_process');

const ROOT = path.join(__dirname, '..', '..');

function haveNginx() { return spawnSync('nginx', ['-v']).status === 0; }

function freePort() {
  return new Promise((resolve) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => resolve(p)); });
  });
}

async function startServices(dir, extra = {}) {
  process.env.DAS_UPSTREAM_SPECTRUM = 'unix:' + path.join(dir, 'spectrum.sock');
  process.env.DAS_UPSTREAM_TELEMETRY = 'unix:' + path.join(dir, 'telemetry.sock');
  const telemetry = require(path.join(ROOT, 'services/telemetry-service/server.js')).start({ listen: 'unix:' + path.join(dir, 'telemetry.sock'), nodes: 40, publishIntervalMs: 250, ...(extra.telemetry || {}) });
  const spectrum = require(path.join(ROOT, 'services/spectrum-service/server.js')).start({ listen: 'unix:' + path.join(dir, 'spectrum.sock'), defaultPoints: 5001, sweepTimeMs: 50, simPrewarm: 2, workers: 1, demo: true, ...(extra.spectrum || {}) });
  const core = require(path.join(ROOT, 'services/core-api/server.js')).start({ listen: 'unix:' + path.join(dir, 'core-api.sock'), nodes: 40, probeIntervalMs: 300, upstreams: { spectrum: process.env.DAS_UPSTREAM_SPECTRUM, telemetry: process.env.DAS_UPSTREAM_TELEMETRY }, ...(extra.core || {}) });
  await Promise.all([telemetry.ready, spectrum.ready, core.ready]);
  return { telemetry, spectrum, core };
}

async function startStack(extra = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'das-gw-'));
  const services = await startServices(dir, extra);
  const httpPort = await freePort();
  const out = path.join(dir, 'nginx');
  // Render the local profile into the temp dir, pointing at the temp sockets.
  const render = spawnSync(process.execPath, [path.join(ROOT, 'scripts/render-nginx.js'), '--profile', 'local', '--out', out, '--http-port', String(httpPort), '--https-port', String(await freePort())], { encoding: 'utf8' });
  if (render.status !== 0) throw new Error(render.stderr);
  for (const f of ['nginx.conf', 'das-locations.conf', 'das-proxy-headers.conf', 'das-ws-headers.conf']) {
    const p = path.join(out, f);
    fs.writeFileSync(p, fs.readFileSync(p, 'utf8').split(path.join(ROOT, '.run')).join(dir));
  }
  for (const d of ['log', 'nginx-cache', 'nginx-tmp']) fs.mkdirSync(path.join(dir, d), { recursive: true });
  const tls = path.join(ROOT, '.run', 'tls');
  if (fs.existsSync(tls)) fs.cpSync(tls, path.join(dir, 'tls'), { recursive: true });
  const nginx = spawn('nginx', ['-p', out, '-c', path.join(out, 'nginx.conf'), '-g', 'daemon off;'], { stdio: 'ignore' });
  const base = `http://127.0.0.1:${httpPort}`;
  for (let i = 0; i < 100; i++) {
    try { const r = await fetch(base + '/api/heartbeat'); if (r.status === 200) break; } catch { /* starting */ }
    await new Promise((r) => setTimeout(r, 100));
  }
  return {
    base,
    dir,
    services,
    async stop() {
      nginx.kill('SIGTERM');
      await new Promise((r) => nginx.once('exit', r));
      await Promise.all([services.core.service.close(), services.spectrum.service.close(), services.telemetry.service.close()]);
      fs.rmSync(dir, { recursive: true, force: true });
    }
  };
}

module.exports = { haveNginx, startStack, startServices, freePort, ROOT };
