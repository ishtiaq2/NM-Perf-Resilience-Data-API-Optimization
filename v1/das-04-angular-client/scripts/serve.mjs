#!/usr/bin/env node
/**
 * Preview the built client against ANY backend: a customer's device on the
 * shipped release, the hotfix, the gateway or the Go edge server. It serves
 * dist/ and forwards /api (HTTP and WebSocket) to --api. The browser still
 * sees one origin, so there is no CORS and nothing to change on the device.
 *
 *   npm run build
 *   node scripts/serve.mjs --api http://192.168.1.10 --port 4300
 *
 * Zero dependencies (Node 18+). For development use `npm start` (ng serve + proxy.conf.json).
 */
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import tls from 'node:tls';
import fs from 'node:fs';
import path from 'node:path';
import { parseArgs } from 'node:util';
import { fileURLToPath } from 'node:url';

const { values: a } = parseArgs({ options: {
  api: { type: 'string', default: 'http://127.0.0.1:8080' },
  port: { type: 'string', default: '4300' },
  host: { type: 'string', default: '127.0.0.1' },
  dist: { type: 'string', default: path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'dist', 'das-04-angular-client', 'browser') },
  insecure: { type: 'boolean', default: false }
} });

const api = new URL(a.api);
const secure = api.protocol === 'https:';
const root = path.resolve(a.dist);
const types = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.json': 'application/json', '.svg': 'image/svg+xml', '.png': 'image/png', '.ico': 'image/x-icon', '.woff2': 'font/woff2', '.txt': 'text/plain' };

if (!fs.existsSync(path.join(root, 'index.html'))) { console.error(`no build in ${root} - run "npm run build" first`); process.exit(1); }

function proxyHeaders(req) {
  // Present the upstream's own origin, so its WebSocket Origin check sees a same-origin request.
  const h = { ...req.headers, host: api.host };
  if (h.origin) h.origin = api.origin;
  return h;
}

function proxy(req, res) {
  const mod = secure ? https : http;
  const up = mod.request({ protocol: api.protocol, hostname: api.hostname, port: api.port, method: req.method, path: req.url, headers: proxyHeaders(req), rejectUnauthorized: !a.insecure }, (r) => {
    res.writeHead(r.statusCode, r.headers);
    r.pipe(res);
  });
  up.on('error', (e) => { res.writeHead(502, { 'Content-Type': 'text/plain' }); res.end('backend unreachable: ' + e.message); });
  req.pipe(up);
}

function serveStatic(req, res) {
  const u = decodeURIComponent(new URL(req.url, 'http://x').pathname);
  let file = path.join(root, path.normalize(u).replace(/^(\.\.[/\\])+/, ''));
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) file = path.join(root, 'index.html'); // SPA fallback
  const hashed = /-[A-Z0-9]{8}\.(js|css)$/.test(file);
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream', 'Cache-Control': hashed ? 'public, max-age=31536000, immutable' : 'no-cache' });
  fs.createReadStream(file).pipe(res);
}

const server = http.createServer((req, res) => (req.url.startsWith('/api/') ? proxy(req, res) : serveStatic(req, res)));

// WebSocket: forward the upgrade handshake and then splice the two sockets.
server.on('upgrade', (req, socket, head) => {
  if (!req.url.startsWith('/api/')) { socket.destroy(); return; }
  const port = Number(api.port) || (secure ? 443 : 80);
  const upstream = secure ? tls.connect({ host: api.hostname, port, servername: api.hostname, rejectUnauthorized: !a.insecure }) : net.connect(port, api.hostname);
  upstream.once(secure ? 'secureConnect' : 'connect', () => {
    const h = proxyHeaders(req);
    let raw = `${req.method} ${req.url} HTTP/1.1\r\n`;
    for (const [k, v] of Object.entries(h)) raw += `${k}: ${v}\r\n`;
    upstream.write(raw + '\r\n');
    if (head && head.length) upstream.write(head);
    socket.pipe(upstream).pipe(socket);
  });
  const close = () => { socket.destroy(); upstream.destroy(); };
  upstream.on('error', close);
  socket.on('error', close);
});

server.listen(Number(a.port), a.host, () => {
  console.log(`DAS client on http://${a.host}:${a.port}  ->  API ${api.origin}`);
});
