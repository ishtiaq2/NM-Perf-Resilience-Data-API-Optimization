#!/usr/bin/env node
'use strict';
/**
 * Render the gateway configuration for a profile.
 *
 *   node scripts/render-nginx.js --profile device --out build/nginx   # files to install on the Master Node
 *   node scripts/render-nginx.js --profile local  --out .run/nginx    # local demo (used by scripts/run-local.sh)
 *
 * Validate the result with:  nginx -t -p <out> -c <out>/nginx.conf
 */
const fs = require('node:fs');
const path = require('node:path');
const { parseArgs } = require('node:util');

const { values: a } = parseArgs({ options: {
  profile: { type: 'string', default: 'local' },
  'sock-dir': { type: 'string' }, // local profile: where the service sockets are (default: .run)
  out: { type: 'string', default: '.run/nginx' },
  'http-port': { type: 'string' },
  'https-port': { type: 'string' },
  'web-root': { type: 'string' }
} });

const root = path.resolve(__dirname, '..');
const out = path.resolve(a.out);
const run = a.profile === 'device' ? '/run/das' : path.join(root, '.run');
const certDir = a.profile === 'device' ? '/etc/das/tls' : path.join(run, 'tls');
const haveLocalCert = fs.existsSync(path.join(certDir, 'device.crt'));

const P = a.profile === 'device' ? {
  PREFIX: '/etc/das/nginx',
  CONF_FILE: '/etc/das/nginx/nginx.conf',
  PID_FILE: '/run/das/nginx.pid',
  LOG_DIR: '/var/log/das',
  RUN_DIR: '/run/das',
  MIME_TYPES: '/etc/das/nginx/mime.types',
  USER_DIRECTIVE: 'user dasgw das;   # worker user must be in group "das" to reach the 0660 service sockets',
  WEB_ROOT: a['web-root'] || '/usr/share/das-ui',
  UPSTREAM_CORE: 'unix:/run/das/core-api.sock',
  UPSTREAM_SPECTRUM: 'unix:/run/das/spectrum.sock',
  UPSTREAM_TELEMETRY: 'unix:/run/das/telemetry.sock',
  LOCATIONS_FILE: '/etc/das/nginx/das-locations.conf',
  PROXY_HEADERS_FILE: '/etc/das/nginx/das-proxy-headers.conf',
  WS_HEADERS_FILE: '/etc/das/nginx/das-ws-headers.conf',
  HTTP_LISTEN: a['http-port'] || '80',
  HTTPS_LISTEN: a['https-port'] || '443',
  TLS_CERT: '/etc/das/tls/device.crt',
  TLS_KEY: '/etc/das/tls/device.key',
  TLS: true
} : {
  PREFIX: out,
  CONF_FILE: path.join(out, 'nginx.conf'),
  PID_FILE: path.join(run, 'nginx.pid'),
  LOG_DIR: path.join(run, 'log'),
  RUN_DIR: run,
  MIME_TYPES: path.join(root, 'gateway', 'nginx', 'mime.types'),
  // nginx started as root drops its workers to "nobody", which cannot open the 0660
  // service sockets; for a local run as root keep the workers as root.
  USER_DIRECTIVE: process.getuid && process.getuid() === 0 ? 'user root;   # local run as root' : '# (local profile: runs as the current user)',
  WEB_ROOT: a['web-root'] || path.join(root, 'web'),
  UPSTREAM_CORE: 'unix:' + path.join(a['sock-dir'] || run, 'core-api.sock'),
  UPSTREAM_SPECTRUM: 'unix:' + path.join(a['sock-dir'] || run, 'spectrum.sock'),
  UPSTREAM_TELEMETRY: 'unix:' + path.join(a['sock-dir'] || run, 'telemetry.sock'),
  LOCATIONS_FILE: path.join(out, 'das-locations.conf'),
  PROXY_HEADERS_FILE: path.join(out, 'das-proxy-headers.conf'),
  WS_HEADERS_FILE: path.join(out, 'das-ws-headers.conf'),
  HTTP_LISTEN: a['http-port'] || '8080',
  HTTPS_LISTEN: a['https-port'] || '8443',
  TLS_CERT: path.join(certDir, 'device.crt'),
  TLS_KEY: path.join(certDir, 'device.key'),
  TLS: haveLocalCert
};

P.HTTP_SERVER = [
  '    server {',
  `        listen ${P.HTTP_LISTEN};`,
  a.profile === 'device' ? '        # To force HTTPS (recommended): replace the include below with  return 301 https://$host$request_uri;' : '',
  `        include ${P.LOCATIONS_FILE};`,
  '    }'
].filter(Boolean).join('\n');
P.TLS_SERVER = P.TLS ? [
  '    # HTTPS + HTTP/2: one multiplexed connection per browser instead of 6,',
  '    # so a heartbeat never queues behind spectrum downloads in the browser.',
  '    server {',
  `        listen ${P.HTTPS_LISTEN} ssl http2;   # nginx >= 1.25.1 prefers: listen ${P.HTTPS_LISTEN} ssl; http2 on;`,
  `        ssl_certificate     ${P.TLS_CERT};`,
  `        ssl_certificate_key ${P.TLS_KEY};`,
  '        ssl_protocols TLSv1.2 TLSv1.3;',
  '        ssl_session_cache shared:SSL:1m;',
  '        ssl_session_timeout 1h;',
  `        include ${P.LOCATIONS_FILE};`,
  '    }'
].join('\n') : '    # (TLS server disabled: no certificate found; run scripts/gen-selfsigned.sh)';

function render(file) {
  return fs.readFileSync(path.join(root, 'gateway', 'nginx', file), 'utf8').replace(/@@([A-Z_]+)@@/g, (m, k) => {
    if (!(k in P)) throw new Error(`unknown placeholder ${m} in ${file}`);
    return String(P[k]);
  });
}

fs.mkdirSync(out, { recursive: true });
fs.writeFileSync(path.join(out, 'nginx.conf'), render('nginx.conf.template'));
fs.writeFileSync(path.join(out, 'das-locations.conf'), render('das-locations.conf.template'));
fs.writeFileSync(path.join(out, 'das-proxy-headers.conf'), render('das-proxy-headers.conf.template'));
fs.writeFileSync(path.join(out, 'das-ws-headers.conf'), render('das-ws-headers.conf.template'));
if (a.profile === 'device') fs.copyFileSync(path.join(root, 'gateway', 'nginx', 'mime.types'), path.join(out, 'mime.types'));
if (a.profile === 'local') {
  for (const d of ['log', 'nginx-cache', 'nginx-tmp']) fs.mkdirSync(path.join(run, d), { recursive: true });
}
console.log(`rendered ${a.profile} profile -> ${out} (TLS ${P.TLS ? 'on' : 'off'})`);
