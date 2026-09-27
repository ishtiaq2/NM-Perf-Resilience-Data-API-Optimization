#!/usr/bin/env node
'use strict';
/**
 * Renders a fleetsim report (bench/results/scale-*.json) as one self-contained
 * HTML page: headline tiles, time series with a hover crosshair, and the
 * per-phase table (the table is the accessible twin of the charts).
 *
 *   node bench/report.js bench/results/latest.json bench/results/scale-report.html
 */
const fs = require('node:fs');
const path = require('node:path');

const [input = path.join(__dirname, 'results', 'latest.json'), output = path.join(__dirname, 'results', 'scale-report.html')] = process.argv.slice(2);
const r = JSON.parse(fs.readFileSync(input, 'utf8'));
const tl = r.timeline;
const cfg = r.config;
const win = Object.fromEntries(r.windows.map((w) => [w.name, w]));
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const n0 = (v) => Math.round(v).toLocaleString('en-US');
const n1 = (v) => (Math.round(v * 10) / 10).toLocaleString('en-US');
const f1 = (v) => v.toLocaleString('en-US', { minimumFractionDigits: 1, maximumFractionDigits: 1 }); // table: fixed decimals
const median = (a) => { const s = a.slice().sort((x, y) => x - y); return s.length ? s[Math.floor(s.length / 2)] : 0; };

// ------------------------------------------------------------------ series
const t = tl.map((s) => s.t);
const up = (s) => s.rssMB > 0; // the NOC answered its metrics endpoint
const series = {
  connected: tl.map((s) => s.connected),
  cpu: tl.map((s, i) => {
    if (i === 0 || !up(s)) return null;
    let j = i - 1;
    while (j > 0 && !up(tl[j])) j--;
    const d = s.cpuSeconds - tl[j].cpuSeconds;
    return d < 0 || !up(tl[j]) ? null : Math.round((d / (s.t - tl[j].t)) * 1000) / 10;
  }),
  rss: tl.map((s) => (up(s) ? s.rssMB : null)),
  live: tl.map((s) => (up(s) ? s.liveHeapMB : null)),
  events: tl.map((s, i) => {
    if (i === 0 || !up(s) || !up(tl[i - 1])) return null;
    const d = s.alarmEvents - tl[i - 1].alarmEvents;
    return d < 0 ? null : Math.round(d / (s.t - tl[i - 1].t));
  })
};

// Annotations: when things happened. Each chart marks them with a numbered
// hairline; the key sits above the charts (three text labels do not fit
// between hairlines 36 s apart).
const marks = [];
if (win.storm) marks.push({ t: win.storm.from, label: `alarm storm: ${n0(cfg.stormSites)} sites × ${cfg.stormAlarms} alarms` });
if (win.after) marks.push({ t: win.after.from, label: 'mains restored, alarms clear' });
if (r.restart) marks.push({ t: r.restart.downAt, label: 'NOC process restarted' });

// ------------------------------------------------------------------ charts
// The viewBox is close to the rendered width of a card in the two-column
// layout, so 11-unit text renders at ~11 px; narrow screens enlarge it in CSS.
const W = 540, H = 212, L = 50, T = 24, B = 26;
const tMax = Math.ceil(Math.max(...t) / 60) * 60;
function niceScale(maxV, target = 5) {
  if (!(maxV > 0)) return { max: 1, step: 0.25 };
  const raw = maxV / target;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  let step = 10 * mag;
  for (const m of [1, 2, 2.5, 5, 10]) if (m * mag >= raw) { step = m * mag; break; }
  return { max: Math.ceil(maxV / step) * step, step };
}
const tickLabel = (v) => (v >= 10000 ? n0(v / 1000) + 'k' : n0(v));
function chart(id, title, sub, list, unit, opts = {}) {
  const R = list.length > 1 ? 64 : 14; // room for direct end labels
  const x = (v) => L + (v / tMax) * (W - L - R);
  const vals = list.flatMap((s) => s.data.filter((v) => v != null));
  const sc = opts.scale || niceScale(Math.max(...vals) * 1.04);
  const y = (v) => T + (1 - v / sc.max) * (H - T - B);
  const grid = [];
  for (let v = 0; v <= sc.max + 1e-9; v += sc.step) {
    grid.push(`<line class="grid" x1="${L}" x2="${W - R}" y1="${y(v).toFixed(1)}" y2="${y(v).toFixed(1)}"/><text class="tick" x="${L - 7}" y="${(y(v) + 4).toFixed(1)}" text-anchor="end">${esc(tickLabel(v))}</text>`);
  }
  const xt = [];
  for (let s = 0; s <= tMax; s += 60) {
    const anchor = s === 0 ? 'start' : s === tMax && R < 30 ? 'end' : 'middle'; // keep edge labels inside the viewBox
    xt.push(`<text class="tick" x="${x(s).toFixed(1)}" y="${H - B + 17}" text-anchor="${anchor}">${s} s</text>`);
  }
  const ann = marks.map((m, k) => {
    const mx = x(m.t).toFixed(1);
    return `<line class="mark" x1="${mx}" x2="${mx}" y1="${T - 4}" y2="${H - B}"/><circle class="badge" cx="${mx}" cy="${T - 12}" r="7.5"/><text class="badgetext" x="${mx}" y="${T - 8.5}" text-anchor="middle">${k + 1}</text>`;
  }).join('');
  const paths = list.map((s) => {
    let d = '', pen = false;
    s.data.forEach((v, i) => {
      if (v == null) { pen = false; return; }
      d += (pen ? 'L' : 'M') + x(t[i]).toFixed(1) + ',' + y(v).toFixed(1);
      pen = true;
    });
    return `<path class="line" style="stroke:var(${s.color})" d="${d}"/>`;
  }).join('');
  // Direct labels just past the right end of each series, nudged apart if close.
  let ends = '';
  if (list.length > 1) {
    const lab = list.map((s) => {
      let i = s.data.length - 1;
      while (i > 0 && s.data[i] == null) i--;
      return { name: s.name, x: x(t[i]) + 6, y: y(s.data[i]) + 4 };
    }).sort((a, b) => a.y - b.y);
    for (let k = 1; k < lab.length; k++) if (lab[k].y - lab[k - 1].y < 13) lab[k].y = lab[k - 1].y + 13;
    ends = lab.map((l) => `<text class="endlabel" x="${l.x.toFixed(1)}" y="${l.y.toFixed(1)}">${esc(l.name)}</text>`).join('');
  }
  const legend = list.length > 1 ? `<div class="legend">${list.map((s) => `<span><i style="background:var(${s.color})"></i>${esc(s.name)}</span>`).join('')}</div>` : '';
  const data = JSON.stringify({ t, unit, series: list.map((s) => ({ name: s.name, color: s.color, data: s.data })), tMax, W, L, R });
  return `<figure class="card" id="${id}">
  <figcaption><h3>${esc(title)}</h3><p>${esc(sub)}</p></figcaption>${legend}
  <div class="plot"><svg viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(title)}">${grid.join('')}${xt.join('')}${ann}${paths}${ends}<line class="cross" x1="0" x2="0" y1="${T}" y2="${H - B}" visibility="hidden"/></svg>
  <div class="tip" hidden></div></div>
  <script type="application/json" class="data">${data.replace(/</g, '\\u003c')}</script>
</figure>`;
}

const steadyRss = median(tl.filter((x) => x.t >= win.steady.from && x.t <= win.steady.to && x.rssMB > 0).map((x) => x.rssMB));
const tiles = [
  ['Master Units connected', n0(cfg.sites), `all connected ${n1(r.connectAllS)} s after start (admission control: ${n0(r.refusedAtStart)} deferred)`],
  ['NOC memory, steady', n0(steadyRss) + ' MB', `RSS, typical (${n0(win.steady.nocRssMaxMB)} MB at most); live heap ${n0(win.steady.nocLiveHeapMaxMB)} MB, goroutine stacks ${n0(win.steady.nocStacksMaxMB)} MB`],
  ['NOC CPU, steady', n1(win.steady.nocCpuPercent) + ' %', 'of ONE core, ' + n0(win.steady.uplinkMessagesPerS) + ' device messages/s'],
  ['Alarm to dashboard, p99', n0(win.steady.alarmLatencyMs.p99) + ' ms', `steady; ${n0(win.storm.alarmLatencyMs.p99)} ms during the ${n0(cfg.stormSites * cfg.stormAlarms)}-alarm storm`],
  r.restart ? ['Fleet back after NOC restart', n1(r.restart.reconnectS) + ' s', `${n0(r.restart.refusedDuringReconnect)} reconnects deferred by admission control`] : null,
  ['NOC view vs devices', r.consistencyChecks.reduce((a, c) => a + c.mismatches, 0) + ' differ', `${r.consistencyChecks.length} checks × ${n0(cfg.sites)} sites: summary hash + alarm counts`]
].filter(Boolean);

const rows = r.windows.map((w) => `<tr><th scope="row">${esc(w.name)}</th><td>${n0(w.from)}–${n0(w.to)} s</td>
  <td>${f1(w.alarmLatencyMs.p50 || 0)}</td><td>${f1(w.alarmLatencyMs.p90 || 0)}</td><td>${f1(w.alarmLatencyMs.p99 || 0)}</td><td>${f1(w.alarmLatencyMs.max || 0)}</td><td>${n0(w.alarmLatencyMs.n)}</td>
  <td>${f1(w.nocCpuPercent)}</td><td>${n0(w.nocRssMaxMB)}</td><td>${n0(w.nocLiveHeapMaxMB)}</td><td>${n0(w.uplinkMessagesPerS)}</td><td>${n0(w.dashboardMessagesPerS)}</td><td>${n0(w.alarmEventsPerS)}</td></tr>`).join('');

const d = r.dashboards, dv = r.devices;
const html = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>NOC scale test</title>
<style>
.viz-root { color-scheme: light; --surface-1: #fcfcfb; --page: #f9f9f7; --text-primary: #0b0b0b; --text-secondary: #52514e; --muted: #898781;
  --grid: #e1e0d9; --axis: #c3c2b7; --border: rgba(11,11,11,0.10); --series-1: #2a78d6; --series-2: #eb6834; }
@media (prefers-color-scheme: dark) { :root:where(:not([data-theme="light"])) .viz-root { color-scheme: dark; --surface-1: #1a1a19; --page: #0d0d0d;
  --text-primary: #ffffff; --text-secondary: #c3c2b7; --grid: #2c2c2a; --axis: #383835; --border: rgba(255,255,255,0.10); --series-1: #3987e5; --series-2: #d95926; } }
:root[data-theme="dark"] .viz-root { color-scheme: dark; --surface-1: #1a1a19; --page: #0d0d0d; --text-primary: #ffffff; --text-secondary: #c3c2b7;
  --grid: #2c2c2a; --axis: #383835; --border: rgba(255,255,255,0.10); --series-1: #3987e5; --series-2: #d95926; }
* { box-sizing: border-box; }
body { margin: 0; }
.viz-root { background: var(--page); color: var(--text-primary); font: 14px/1.45 system-ui, -apple-system, "Segoe UI", sans-serif; padding: 24px 16px 40px; min-height: 100vh; }
.wrap { max-width: 1100px; margin: 0 auto; }
h1 { font-size: 22px; margin: 0 0 4px; } h2 { font-size: 15px; margin: 28px 0 10px; } h3 { font-size: 14px; margin: 0; }
p { margin: 0; color: var(--text-secondary); }
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: 12px; margin-top: 18px; }
.tile { background: var(--surface-1); border: 1px solid var(--border); border-radius: 10px; padding: 12px 14px; }
.tile .label { color: var(--text-secondary); font-size: 12px; }
.tile .value { font-size: 26px; font-weight: 600; margin: 2px 0; }
.tile .note { color: var(--muted); font-size: 12px; }
.charts { display: grid; grid-template-columns: repeat(auto-fit, minmax(420px, 1fr)); gap: 12px; }
@media (max-width: 480px) { .charts { grid-template-columns: 1fr; } }
.card { background: var(--surface-1); border: 1px solid var(--border); border-radius: 10px; margin: 0; padding: 12px 14px; }
.card p { font-size: 12px; }
.legend { display: flex; gap: 14px; font-size: 12px; color: var(--text-secondary); margin-top: 6px; }
.legend i { display: inline-block; width: 14px; height: 2px; margin-right: 6px; vertical-align: middle; border-radius: 1px; }
.plot { position: relative; }
svg { width: 100%; height: auto; display: block; margin-top: 6px; touch-action: none; }
.grid { stroke: var(--grid); stroke-width: 1; }
.tick { fill: var(--muted); font-size: 11px; font-variant-numeric: tabular-nums; }
.mark { stroke: var(--axis); stroke-width: 1; }
.badge { fill: var(--surface-1); stroke: var(--axis); stroke-width: 1; }
.badgetext { fill: var(--text-secondary); font-size: 10px; font-weight: 600; }
@media (max-width: 600px) { .tick, .endlabel { font-size: 15px; } .badgetext { font-size: 12px; } }
.line { fill: none; stroke-width: 2; stroke-linejoin: round; stroke-linecap: round; }
.endlabel { fill: var(--text-secondary); font-size: 11px; }
.cross { stroke: var(--muted); stroke-width: 1; }
.tip { position: absolute; top: 8px; pointer-events: none; background: var(--surface-1); border: 1px solid var(--border); border-radius: 8px; padding: 6px 9px;
  font-size: 12px; box-shadow: 0 2px 10px rgba(0,0,0,0.12); white-space: nowrap; }
.tip b { font-weight: 600; } .tip .k { display: inline-block; width: 12px; height: 2px; margin-right: 6px; vertical-align: middle; }
.tip .t { color: var(--muted); }
.annot { display: flex; flex-wrap: wrap; gap: 6px 18px; font-size: 12px; color: var(--text-secondary); margin: 10px 0 12px; }
.annot .key { display: inline-grid; place-items: center; width: 17px; height: 17px; margin-right: 6px; border: 1px solid var(--axis); border-radius: 50%; font-size: 10px; background: var(--surface-1); }
table { width: 100%; border-collapse: collapse; background: var(--surface-1); border: 1px solid var(--border); border-radius: 10px; overflow: hidden; font-size: 13px; }
th, td { padding: 7px 9px; border-bottom: 1px solid var(--grid); text-align: right; font-variant-numeric: tabular-nums; white-space: nowrap; }
th:first-child, td:first-child { text-align: left; }
thead th { color: var(--text-secondary); font-weight: 500; font-size: 12px; }
.tablewrap { overflow-x: auto; }
.foot { margin-top: 18px; font-size: 12px; color: var(--muted); }
</style></head>
<body><div class="viz-root"><div class="wrap">
<h1>NOC scale test: ${n0(cfg.sites)} Master Units</h1>
<p>One NOC process on ONE core; ${cfg.dashboards} NOC dashboards (${cfg.latencyDashboards || cfg.dashboards} measuring latency), ${cfg.tenantDashboards} customer dashboards, ${cfg.drilldowns} drill-downs; ${cfg.tenants} customers. Each device: summary deltas every ~${esc(cfg.tick)}, ${cfg.alarmsPerHour} alarm raises per hour. Duration ${esc(cfg.duration)}.</p>
<section class="tiles">${tiles.map(([l, v, nte]) => `<div class="tile"><div class="label">${esc(l)}</div><div class="value">${esc(v)}</div><div class="note">${esc(nte)}</div></div>`).join('')}</section>
<h2>Over time</h2>
<div class="annot">${marks.map((m, k) => `<span><b class="key">${k + 1}</b>${n0(m.t)} s · ${esc(m.label)}</span>`).join('')}</div>
<div class="charts">
${chart('c-conn', 'Connected Master Units', 'Every device dials out and stays connected; the NOC restart is the dip.', [{ name: 'connected', color: '--series-1', data: series.connected }], 'devices')}
${chart('c-cpu', 'NOC CPU', 'Per second, % of one core (the NOC was pinned to a single core).', [{ name: 'CPU', color: '--series-1', data: series.cpu }], '%', { scale: { max: 100, step: 25 } })}
${chart('c-mem', 'NOC memory', 'Resident memory and the live Go heap after the last GC, MB.', [{ name: 'RSS', color: '--series-1', data: series.rss }, { name: 'live heap', color: '--series-2', data: series.live }], 'MB')}
${chart('c-ev', 'Alarm events processed', 'Per second: raises, clears, acknowledgements.', [{ name: 'events', color: '--series-1', data: series.events }], 'events/s')}
</div>
<h2>By phase</h2>
<div class="tablewrap"><table><thead><tr><th>Phase</th><th>Window</th><th>p50 ms</th><th>p90 ms</th><th>p99 ms</th><th>max ms</th><th>samples</th><th>NOC CPU %</th><th>RSS MB</th><th>live heap MB</th><th>device msg/s</th><th>dashboard msg/s</th><th>alarm events/s</th></tr></thead>
<tbody>${rows}</tbody></table></div>
<p class="foot">Latency = device event timestamp → receipt by a measuring dashboard (both in the simulator process, same clock). Dashboards: ${n0(d.events)} alarm events delivered, ${d.gaps} gaps, ${d.tenantScopeViolations} tenant-isolation violations${d.missingEvents != null ? `; the measuring dashboards found ${d.missingEvents} missing and ${d.outOfOrderEvents} out-of-order events` : ''}. Devices sent ${n0(dv.messages)} messages (${n1(dv.bytes / 1e6)} MB, ${n0(dv.bytes / dv.messages)} bytes on average), ${n0(dv.resyncRequests)} resync requests. Raw data: ${esc(path.basename(input))}.</p>
</div></div>
<script>
(() => {
  for (const fig of document.querySelectorAll('figure.card')) {
    const d = JSON.parse(fig.querySelector('script.data').textContent);
    const svg = fig.querySelector('svg'), tip = fig.querySelector('.tip'), cross = fig.querySelector('.cross');
    const { W, L, R } = d;
    const show = (ev) => {
      const box = svg.getBoundingClientRect();
      const px = ((ev.clientX - box.left) / box.width) * W;
      const tv = ((px - L) / (W - L - R)) * d.tMax;
      let i = 0, best = Infinity;
      d.t.forEach((v, k) => { const e = Math.abs(v - tv); if (e < best) { best = e; i = k; } });
      const cx = L + (d.t[i] / d.tMax) * (W - L - R);
      cross.setAttribute('x1', cx); cross.setAttribute('x2', cx); cross.setAttribute('visibility', 'visible');
      tip.replaceChildren();
      const head = document.createElement('div'); head.className = 't'; head.textContent = d.t[i] + ' s'; tip.append(head);
      for (const s of d.series) {
        const row = document.createElement('div');
        const k = document.createElement('span'); k.className = 'k'; k.style.background = 'var(' + s.color + ')';
        const b = document.createElement('b'); b.textContent = s.data[i] == null ? 'n/a' : Number(s.data[i]).toLocaleString('en-US');
        row.append(k, b, ' ' + d.unit + (d.series.length > 1 ? ' · ' + s.name : ''));
        tip.append(row);
      }
      tip.hidden = false;
      const left = (cx / W) * box.width;
      tip.style.left = Math.min(Math.max(0, left + 12), box.width - tip.offsetWidth) + 'px';
    };
    svg.addEventListener('pointermove', show);
    svg.addEventListener('pointerleave', () => { tip.hidden = true; cross.setAttribute('visibility', 'hidden'); });
  }
})();
</script>
</body></html>
`;
fs.writeFileSync(output, html);
console.log('wrote ' + output);
