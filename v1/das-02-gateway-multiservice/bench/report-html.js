'use strict';
/**
 * Render benchmark summaries as a self-contained HTML report (no external
 * assets): a summary table plus one small-multiple chart per run of heartbeat
 * latency over time, all on the same y-scale so the runs compare directly.
 */

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
const fmt = (v, unit = '') => (v == null ? '–' : `${Number(v).toLocaleString('en-US')}${unit}`);

function niceTicks(max, count = 6) {
  const raw = max / count;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw) || raw;
  const out = [];
  for (let v = 0; v <= max + 1e-9; v += step) out.push(Math.round(v));
  return out;
}

function chart(run, idx, yMax, timeoutMs) {
  const W = 960, H = 260, L = 64, R = 20, T = 18, B = 36;
  const dur = run.params.durationS;
  const x = (t) => L + (Math.max(0, Math.min(dur, t)) / dur) * (W - L - R);
  const y = (ms) => T + (1 - Math.min(ms, yMax) / yMax) * (H - T - B);
  const ticks = niceTicks(yMax);
  const grid = ticks.map((v) => `<line x1="${L}" x2="${W - R}" y1="${y(v)}" y2="${y(v)}" class="grid"/><text x="${L - 8}" y="${y(v) + 4}" class="tick" text-anchor="end">${v.toLocaleString('en-US')}</text>`).join('');
  const xt = [];
  const xs = dur <= 30 ? 5 : dur <= 90 ? 10 : 30;
  for (let s = 0; s <= dur; s += xs) xt.push(`<text x="${x(s)}" y="${H - B + 18}" class="tick" text-anchor="middle">${s}s</text>`);
  const pts = run.timeline.filter((p) => p.t >= 0 && p.t <= dur);
  const okDots = pts.filter((p) => p.ok).map((p) => `<circle cx="${x(p.t).toFixed(1)}" cy="${y(p.ms).toFixed(1)}" r="4" class="dot-ok"/>`).join('');
  const bad = pts.filter((p) => !p.ok).map((p) => {
    const cx = x(p.t).toFixed(1), cy = y(Math.min(p.ms, yMax)).toFixed(1);
    return `<g class="dot-bad" transform="translate(${cx},${cy})"><circle r="6" class="ring"/><path d="M-4,-4L4,4M4,-4L-4,4"/></g>`;
  }).join('');
  const data = JSON.stringify(pts.map((p) => [Math.round(p.t * 100) / 100, p.ms, p.ok ? 1 : 0]));
  return `
  <figure class="panel" data-idx="${idx}">
    <figcaption><strong>${esc(run.label)}</strong><span>${fmt(run.heartbeat.timeouts)} of ${fmt(run.heartbeat.n)} heartbeats timed out · p99 ${fmt(run.heartbeat.p99, ' ms')}</span></figcaption>
    <svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Heartbeat latency over time for ${esc(run.label)}">
      ${grid}
      <line x1="${L}" x2="${W - R}" y1="${y(0)}" y2="${y(0)}" class="axis"/>
      <line x1="${L}" x2="${W - R}" y1="${y(timeoutMs)}" y2="${y(timeoutMs)}" class="limit"/>
      ${xt.join('')}
      <text x="14" y="${T + (H - T - B) / 2}" class="tick" transform="rotate(-90 14 ${T + (H - T - B) / 2})" text-anchor="middle">latency (ms)</text>
      ${okDots}${bad}
      <line class="cross" x1="0" x2="0" y1="${T}" y2="${H - B}" visibility="hidden"/>
      <rect class="hit" x="${L}" y="${T}" width="${W - L - R}" height="${H - T - B}" fill="transparent"/>
    </svg>
    <script type="application/json" class="pts">${data}</script>
    <script type="application/json" class="geom">${JSON.stringify({ W, H, L, R, T, B, dur, yMax })}</script>
  </figure>`;
}

function bucketTable(runs, timeoutMs) {
  const edges = [[0, 10, '< 10 ms'], [10, 100, '10–100 ms'], [100, 1000, '100 ms – 1 s'], [1000, timeoutMs, `1 s – ${timeoutMs / 1000} s`]];
  const rows = edges.map(([lo, hi, name]) => `<tr><th scope="row">${name}</th>${runs.map((r) => `<td>${fmt(r.timeline.filter((p) => p.ok && p.ms >= lo && p.ms < hi).length)}</td>`).join('')}</tr>`);
  rows.push(`<tr><th scope="row">timed out (alarm)</th>${runs.map((r) => `<td>${fmt(r.timeline.filter((p) => !p.ok).length)}</td>`).join('')}</tr>`);
  return `<table><thead><tr><th>Heartbeat latency</th>${runs.map((r) => `<th>${esc(r.label)}</th>`).join('')}</tr></thead><tbody>${rows.join('')}</tbody></table>`;
}

function summaryTable(runs) {
  const row = (name, f) => `<tr><th scope="row">${name}</th>${runs.map((r) => `<td>${f(r)}</td>`).join('')}</tr>`;
  return `<table><thead><tr><th>Metric</th>${runs.map((r) => `<th>${esc(r.label)}</th>`).join('')}</tr></thead><tbody>
    ${row('Heartbeat p50', (r) => fmt(r.heartbeat.p50, ' ms'))}
    ${row('Heartbeat p99', (r) => fmt(r.heartbeat.p99, ' ms'))}
    ${row('Heartbeat max', (r) => fmt(r.heartbeat.max, ' ms'))}
    ${row('Heartbeat timeouts', (r) => `${fmt(r.heartbeat.timeouts)} / ${fmt(r.heartbeat.n)}`)}
    ${row('False “server dead” alarms (1 miss)', (r) => fmt(r.heartbeat.alarmsStrict))}
    ${row('False “server dead” alarms (3 misses)', (r) => fmt(r.heartbeat.alarmsTolerant))}
    ${row('Server event-loop / scheduler lag p99 (worst)', (r) => fmt(r.heartbeat.serverLagP99MaxMs, ' ms'))}
    ${row('Spectrum responses (200 / 304)', (r) => `${fmt(r.spectrum.ok)} / ${fmt(r.spectrum.notModified)}`)}
    ${row('Spectrum p50 / p95', (r) => `${fmt(r.spectrum.p50, ' ms')} / ${fmt(r.spectrum.p95, ' ms')}`)}
    ${row('Spectrum updates per trace per minute', (r) => fmt(Math.round((r.spectrum.ok / (r.params.browsers * r.params.analyzersPerBrowser) / r.params.durationS) * 600) / 10))}
    ${row('Hardware sweeps performed', (r) => fmt(r.spectrum.distinctSweepsDelivered))}
    ${row('Spectrum data transferred', (r) => fmt(r.spectrum.mbReceived, ' MB'))}
    ${row('Dashboard p95 / 304s', (r) => `${fmt(r.dashboard.p95, ' ms')} / ${fmt(r.dashboard.notModified)}`)}
    ${row('Dashboard data transferred', (r) => fmt(r.dashboard.kbReceived, ' kB'))}
    ${row('Config read p95 / max', (r) => `${fmt(r.configApi.p95, ' ms')} / ${fmt(r.configApi.max, ' ms')}`)}
    ${runs.some((r) => r.server) ? row('Server memory (RSS) start / peak', (r) => (r.server ? `${fmt(r.server.rssStartMb, ' MB')} / ${fmt(r.server.rssPeakMb, ' MB')}` : '–')) : ''}
  </tbody></table>`;
}

function renderReport(runs, meta = {}) {
  const timeoutMs = runs[0].params.hbTimeoutMs;
  const yMax = Math.round(timeoutMs * 1.1);
  const c = runs[0].params;
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Heartbeat under load</title>
<style>
:root { color-scheme: light; --surface:#fcfcfb; --page:#f9f9f7; --ink:#0b0b0b; --ink2:#52514e; --muted:#898781; --grid:#e1e0d9; --axis:#c3c2b7; --s1:#2a78d6; --crit:#d03b3b; --ring:rgba(11,11,11,.10); }
@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) { color-scheme: dark; --surface:#1a1a19; --page:#0d0d0d; --ink:#fff; --ink2:#c3c2b7; --grid:#2c2c2a; --axis:#383835; --s1:#3987e5; --ring:rgba(255,255,255,.10); } }
:root[data-theme="dark"] { color-scheme: dark; --surface:#1a1a19; --page:#0d0d0d; --ink:#fff; --ink2:#c3c2b7; --grid:#2c2c2a; --axis:#383835; --s1:#3987e5; --ring:rgba(255,255,255,.10); }
* { box-sizing: border-box; }
body { margin:0; background:var(--page); color:var(--ink); font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif; }
main { max-width: 1040px; margin: 0 auto; padding: 28px 16px 48px; }
h1 { font-size: 22px; margin: 0 0 4px; } p.sub { color: var(--ink2); margin: 0 0 20px; }
.card { background: var(--surface); border: 1px solid var(--ring); border-radius: 10px; padding: 16px; margin-bottom: 16px; }
.legend { display:flex; gap:18px; color:var(--ink2); font-size:13px; margin: 0 0 8px; flex-wrap: wrap; }
.legend span { display:inline-flex; align-items:center; gap:6px; }
.panel { margin: 0 0 8px; position: relative; } .panel figcaption { display:flex; gap:12px; align-items:baseline; flex-wrap: wrap; margin-bottom: 4px; }
.panel figcaption span { color: var(--ink2); font-size: 13px; }
.panel svg { width: 100%; height: auto; display:block; overflow: visible; }
.legend svg { width: 12px; height: 12px; display: inline-block; flex: none; }
.grid { stroke: var(--grid); stroke-width: 1; } .axis { stroke: var(--axis); stroke-width: 1; }
.tick { fill: var(--muted); font-size: 12px; font-variant-numeric: tabular-nums; }
.limit { stroke: var(--crit); stroke-width: 1; } .limit-label { fill: var(--ink2); font-size: 12px; }
.dot-ok { fill: var(--s1); stroke: var(--surface); stroke-width: 2; }
.dot-bad path { stroke: var(--crit); stroke-width: 2.2; stroke-linecap: round; } .dot-bad .ring { fill: var(--surface); }
.cross { stroke: var(--axis); stroke-width: 1; }
.tip { position:absolute; pointer-events:none; background:var(--surface); border:1px solid var(--ring); border-radius:8px; padding:6px 10px; font-size:13px; box-shadow:0 4px 16px rgba(0,0,0,.12); white-space:nowrap; }
.tip b { font-size: 15px; } .tip .k { display:inline-block; width:14px; height:2px; vertical-align:middle; margin-right:6px; }
table { border-collapse: collapse; width: 100%; font-size: 14px; }
th, td { text-align: right; padding: 6px 10px; border-bottom: 1px solid var(--grid); font-variant-numeric: tabular-nums; }
th:first-child, td:first-child { text-align: left; } thead th { color: var(--ink2); font-weight: 600; } tbody th { font-weight: 500; }
.note { color: var(--ink2); font-size: 13px; }
.scroll { overflow-x: auto; }
</style></head>
<body><main>
<h1>Heartbeat latency while engineers run the spectrum analyzer</h1>
<p class="sub">${esc(meta.subtitle || '')} ${c.browsers} browsers × ${c.analyzersPerBrowser} analyzer trace${c.analyzersPerBrowser > 1 ? 's' : ''}, ${fmt(c.points)}-point sweeps, heartbeat every ${fmt(c.hbIntervalMs)} ms with a ${fmt(c.hbTimeoutMs)} ms UI timeout, ${c.durationS} s per run. Each browser is limited to ${c.maxSockets} connections per origin, like a real browser over HTTP/1.1.</p>
<div class="card">
  <div class="legend"><span><svg width="12" height="12"><circle cx="6" cy="6" r="4" fill="var(--s1)"/></svg>heartbeat answered</span><span><svg width="12" height="12"><path d="M2,2L10,10M10,2L2,10" stroke="var(--crit)" stroke-width="2.2" stroke-linecap="round"/></svg>heartbeat timed out → UI raises “server is dead”</span><span><svg width="16" height="12" style="width:16px"><line x1="0" x2="16" y1="6" y2="6" stroke="var(--crit)" stroke-width="1"/></svg>UI alarm timeout (${timeoutMs.toLocaleString('en-US')} ms)</span></div>
  ${runs.map((r, i) => chart(r, i, yMax, timeoutMs)).join('')}
</div>
<div class="card scroll">${summaryTable(runs)}</div>
<div class="card scroll">${bucketTable(runs, timeoutMs)}</div>
<p class="note">${esc(meta.footnote || '')}</p>
</main>
<script>
document.querySelectorAll('.panel').forEach(function (panel) {
  var pts = JSON.parse(panel.querySelector('.pts').textContent);
  var g = JSON.parse(panel.querySelector('.geom').textContent);
  var svg = panel.querySelector('svg'), hit = panel.querySelector('.hit'), cross = panel.querySelector('.cross');
  var tip = document.createElement('div'); tip.className = 'tip'; tip.hidden = true; panel.appendChild(tip);
  function px(t) { return g.L + (t / g.dur) * (g.W - g.L - g.R); }
  function py(ms) { return g.T + (1 - Math.min(ms, g.yMax) / g.yMax) * (g.H - g.T - g.B); }
  hit.addEventListener('pointermove', function (e) {
    var r = svg.getBoundingClientRect(), sx = (e.clientX - r.left) * (g.W / r.width), sy = (e.clientY - r.top) * (g.H / r.height);
    var best = null, bd = Infinity;
    for (var i = 0; i < pts.length; i++) { var dx = px(pts[i][0]) - sx, dy = (py(pts[i][1]) - sy) * 0.35, d = dx * dx + dy * dy; if (d < bd) { bd = d; best = pts[i]; } }
    if (!best) return;
    cross.setAttribute('x1', px(best[0])); cross.setAttribute('x2', px(best[0])); cross.setAttribute('visibility', 'visible');
    tip.replaceChildren();
    var b = document.createElement('b'); b.textContent = best[2] ? best[1].toLocaleString('en-US') + ' ms' : 'timed out'; tip.appendChild(b);
    var line = document.createElement('div'); var k = document.createElement('span'); k.className = 'k'; k.style.background = best[2] ? 'var(--s1)' : 'var(--crit)'; line.appendChild(k);
    line.appendChild(document.createTextNode((best[2] ? 'answered' : 'no answer within the UI timeout') + ' · t = ' + best[0] + ' s')); tip.appendChild(line);
    tip.hidden = false;
    var pr = panel.getBoundingClientRect(), left = e.clientX - pr.left + 14, top = e.clientY - pr.top + 14;
    if (left + 260 > pr.width) left = e.clientX - pr.left - 270;
    tip.style.left = left + 'px'; tip.style.top = top + 'px';
  });
  hit.addEventListener('pointerleave', function () { tip.hidden = true; cross.setAttribute('visibility', 'hidden'); });
});
</script>
</body></html>`;
}

module.exports = { renderReport };
