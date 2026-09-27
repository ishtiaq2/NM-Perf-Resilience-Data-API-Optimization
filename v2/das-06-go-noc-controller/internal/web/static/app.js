'use strict';
// DAS NOC page: fleet tiles, problem board, live alarm feed and a site drill-down,
// all fed by one WebSocket (/api/ws/dashboard) plus a few REST calls. No framework,
// no build step; everything the server sends is rendered as text, never as HTML.
(() => {
  const $ = (id) => document.getElementById(id);
  const STATUS_RANK = { critical: 6, offline: 5, major: 4, stale: 3, minor: 2, warning: 1, ok: 0 };
  const NODE_RANK = { alarm: 2, degraded: 1, online: 0 };
  let token = null;
  let ws = null;
  let backoff = 500;
  let selected = null;
  let searchTimer = null;
  let searching = false;
  let lastOverview = null;
  const feed = [];
  let feedTimes = [];

  try {
    const m = /(?:^|&)token=([^&]+)/.exec(location.hash.slice(1));
    if (m) {
      sessionStorage.setItem('nocToken', decodeURIComponent(m[1]));
      history.replaceState(null, '', location.pathname);
    }
    token = sessionStorage.getItem('nocToken');
  } catch (e) { /* storage blocked: the login form still works for this page view */ }

  function el(tag, attrs, ...children) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (k === 'class') e.className = v;
      else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
      else e.setAttribute(k, v);
    }
    for (const c of children) if (c != null) e.append(c instanceof Node ? c : String(c));
    return e;
  }
  const badge = (status) => el('span', { class: 'badge st-' + status }, status);
  const fmt = (v, digits) => (v == null ? '–' : Number(v).toFixed(digits));
  const ago = (ms) => {
    if (!ms) return '–';
    const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
    return s < 60 ? s + ' s' : s < 3600 ? Math.round(s / 60) + ' min' : Math.round(s / 3600) + ' h';
  };
  const clock = (ms) => new Date(ms).toLocaleTimeString([], { hour12: false });
  function counts(a) {
    const parts = [];
    for (const [k, cls] of [['critical', 'st-critical'], ['major', 'st-major'], ['minor', 'st-minor'], ['warning', 'st-warning']]) {
      if (a && a[k]) parts.push(el('b', { class: cls }, a[k] + ' ' + k[0].toUpperCase()), ' ');
    }
    return parts.length ? el('span', { class: 'cnt' }, ...parts) : el('span', { class: 'muted' }, '–');
  }

  async function api(path, opts) {
    const res = await fetch(path, { ...opts, headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json', ...((opts && opts.headers) || {}) } });
    if (res.status === 401) { logout(); throw new Error('unauthorized'); }
    if (res.status === 204) return null;
    return res.json();
  }

  function logout() {
    try { sessionStorage.removeItem('nocToken'); } catch (e) { /* ignore */ }
    token = null;
    if (ws) ws.close();
    $('app').hidden = true;
    $('login').hidden = false;
  }

  // ------------------------------------------------------------------ overview
  function renderOverview(o) {
    lastOverview = o;
    const b = o.byStatus || {};
    $('t-sites').textContent = o.sites;
    $('t-connected').textContent = o.connected;
    for (const k of ['critical', 'offline', 'major', 'stale', 'minor', 'ok']) $('t-' + k).textContent = b[k] || 0;
    const a = o.alarms || {};
    $('t-alarms').textContent = (a.critical || 0) + (a.major || 0) + (a.minor || 0) + (a.warning || 0);
    $('t-acked').textContent = o.acknowledged ? '(' + o.acknowledged + ' acknowledged)' : '';
    if (!searching) renderSites(o.problems || []);
    const tw = $('tenants-wrap');
    if (o.byTenant) {
      tw.hidden = false;
      const rows = Object.entries(o.byTenant).sort((x, y) => (y[1].byStatus.critical + y[1].byStatus.offline) - (x[1].byStatus.critical + x[1].byStatus.offline) || x[0].localeCompare(y[0]));
      $('tenants').replaceChildren(...rows.map(([t, v]) => el('tr', {},
        el('td', {}, t), el('td', {}, v.sites), el('td', {}, v.connected),
        el('td', { class: v.byStatus.critical ? 'st-critical' : '' }, v.byStatus.critical),
        el('td', { class: v.byStatus.offline ? 'st-offline' : '' }, v.byStatus.offline),
        el('td', {}, (v.alarms.critical || 0) + (v.alarms.major || 0) + (v.alarms.minor || 0) + (v.alarms.warning || 0)))));
    } else tw.hidden = true;
  }

  function renderSites(list) {
    const rows = list.map((s) => el('tr', { class: 'click' + (s.id === selected ? ' sel' : ''), onclick: () => select(s.id) },
      el('td', {}, el('div', {}, s.name || s.id), el('div', { class: 'muted' }, s.id + ' · ' + s.tenant)),
      el('td', {}, badge(s.status)),
      el('td', {}, counts(s.alarms)),
      el('td', { class: 'muted' }, ago(s.since))));
    if (!rows.length) rows.push(el('tr', {}, el('td', { colspan: 4, class: 'muted' }, searching ? 'No matching site.' : 'Every site is fine.')));
    $('problems').replaceChildren(...rows);
  }

  $('search').addEventListener('input', (e) => {
    clearTimeout(searchTimer);
    const q = e.target.value.trim();
    searchTimer = setTimeout(async () => {
      searching = q !== '';
      if (!searching) { if (lastOverview) renderSites(lastOverview.problems || []); return; }
      const r = await api('/api/sites?limit=50&q=' + encodeURIComponent(q));
      renderSites(r.items || []);
    }, 200);
  });

  // ---------------------------------------------------------------- alarm feed
  function pushEvents(events) {
    const now = Date.now();
    for (const e of events) { feed.unshift(e); feedTimes.push(now); }
    feed.length = Math.min(feed.length, 200);
    feedTimes = feedTimes.filter((t) => now - t < 60000);
    $('feed-rate').textContent = feedTimes.length + ' events / min';
    renderFeed();
  }

  function renderFeed() {
    $('feed').replaceChildren(...feed.slice(0, 120).map((e) => {
      const canAck = e.state === 'raised' && !e.acked;
      return el('li', { class: e.state === 'cleared' ? 'cleared' : '' },
        el('span', { class: 'time' }, clock(e.rx || e.at)),
        el('span', {}, e.state === 'raised' ? badge(e.sev) : el('span', { class: 'muted' }, e.state)),
        el('span', { class: 'what', title: (e.text || '') },
          el('span', { class: 'site', onclick: () => select(e.site) }, e.siteName || e.site), ' · ', e.code, e.node ? ' · node ' + e.node : ''),
        canAck ? el('button', { class: 'small', onclick: () => ack(e.site, e.id, e) }, 'Ack') : el('span', { class: 'muted' }, e.by ? e.by : ''));
    }));
  }

  async function ack(site, id, ev) {
    const note = window.prompt('Acknowledge ' + id + ' at ' + site + ' - note (optional):', '');
    if (note === null) return;
    await api('/api/alarms/acknowledge', { method: 'POST', body: JSON.stringify({ site, id, note }) });
    if (ev) { ev.acked = true; renderFeed(); }
  }

  // --------------------------------------------------------------- drill-down
  function select(id) {
    selected = id;
    $('d-title').textContent = id;
    $('d-status').replaceChildren();
    $('d-body').replaceChildren(el('span', { class: 'muted' }, 'Loading…'));
    subscribe();
    if (lastOverview && !searching) renderSites(lastOverview.problems || []);
  }

  function renderSite(d) {
    if (d.id !== selected) return;
    $('d-title').textContent = (d.name || d.id) + ' (' + d.id + ')';
    $('d-status').replaceChildren(badge(d.status));
    const kv = [
      ['Customer', d.tenant], ['Venue', d.venue || '–'], ['Region', d.region || '–'], ['Software', d.fw || '–'],
      ['Remote Nodes', d.nodes], ['Online / offline', d.online + ' / ' + d.offline], ['Degraded', d.degraded],
      ['Max temperature', fmt(d.maxTempC, 1) + ' °C'], ['Min optical Rx', fmt(d.minRxDbm, 1) + ' dBm'], ['Max VSWR', fmt(d.maxVswr, 2)],
      [d.connected ? 'Connected for' : 'Disconnected for', ago(d.since)], ['Summary rev', d.rev + ' · ' + (d.hash || '').slice(0, 8)],
    ];
    const body = [el('div', { class: 'kv' }, ...kv.map(([k, v]) => el('div', {}, el('span', {}, k), v)))];
    const alarms = d.activeAlarms || [];
    body.push(el('h3', {}, 'Active alarms (' + alarms.length + ')'));
    if (alarms.length) {
      body.push(el('table', {}, el('thead', {}, el('tr', {}, el('th', {}, 'Severity'), el('th', {}, 'Alarm'), el('th', {}, 'Raised'), el('th', {}, ''))),
        el('tbody', {}, ...alarms.slice(0, 100).map((a) => el('tr', {},
          el('td', {}, badge(a.sev)),
          el('td', {}, a.code + (a.node ? ' · node ' + a.node : ''), a.text ? el('div', { class: 'muted' }, a.text) : null),
          el('td', { class: 'muted' }, ago(a.raisedAt)),
          el('td', {}, a.ackBy ? el('span', { class: 'muted', title: a.ackNote || '' }, '✓ ' + a.ackBy) : el('button', { class: 'small', onclick: () => ack(d.id, a.id) }, 'Ack')))))));
    }
    const det = d.detail;
    body.push(el('h3', {}, 'Remote Nodes' + (det ? ' (' + det.count + ', updated ' + ago(det.updatedAt) + ' ago)' : '')));
    if (!det) {
      body.push(el('div', { class: 'muted' }, d.connected ? 'Requesting the node table from the Master Unit…' : 'The Master Unit is not connected.'));
    } else {
      const nodes = det.nodes.slice().sort((x, y) => (NODE_RANK[y.status] || 0) - (NODE_RANK[x.status] || 0) || x.id - y.id);
      body.push(el('table', { class: 'nodes' },
        el('thead', {}, el('tr', {}, ...['Node', 'Type', 'Chain/hop', 'Temp °C', 'Rx dBm', 'Tx dBm', 'VSWR', 'PSU V', 'FW'].map((h) => el('th', {}, h)))),
        el('tbody', {}, ...nodes.slice(0, 500).map((n) => el('tr', {},
          el('td', {}, el('span', { class: 'dot ' + (n.status || '') }), n.name || n.id),
          el('td', {}, n.type || '–'), el('td', {}, (n.chain || '–') + '/' + (n.hop || '–')),
          el('td', {}, fmt(n.tempC, 1)), el('td', {}, fmt(n.rxDbm, 1)), el('td', {}, fmt(n.txDbm, 1)),
          el('td', {}, fmt(n.vswr, 2)), el('td', {}, fmt(n.psuV, 2)), el('td', {}, n.fw || '–'))))));
    }
    $('d-body').replaceChildren(...body);
  }

  // ---------------------------------------------------------------- transport
  function subscribe() {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ t: 'sub', overview: true, alarms: true, site: selected || '' }));
    }
  }

  function connect() {
    const url = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/api/ws/dashboard';
    ws = new WebSocket(url, ['das-noc-dash.v1', 'bearer.' + token]);
    ws.onopen = () => {
      backoff = 500;
      $('conn').textContent = 'live';
      $('conn').className = 'conn on';
      subscribe();
    };
    ws.onmessage = (m) => {
      let msg;
      try { msg = JSON.parse(m.data); } catch (e) { return; }
      switch (msg.t) {
        case 'hello': $('scope').textContent = msg.user + ' · ' + (msg.tenant === '*' ? 'all customers' : msg.tenant); break;
        case 'overview': renderOverview(msg.overview); break;
        case 'alarms': pushEvents(msg.events); break;
        case 'gap': loadActive(); break;
        case 'site': renderSite(msg.site); break;
        case 'error': $('d-body').replaceChildren(el('span', { class: 'muted' }, msg.message)); break;
      }
    };
    ws.onclose = () => {
      $('conn').textContent = 'reconnecting';
      $('conn').className = 'conn off';
      if (token) setTimeout(connect, backoff);
      backoff = Math.min(backoff * 2, 15000);
    };
  }

  async function loadActive() {
    const r = await api('/api/alarms?limit=50');
    feed.length = 0;
    for (const a of (r.items || []).slice().reverse()) {
      feed.unshift({ site: a.site, siteName: a.siteName, id: a.id, code: a.code, sev: a.sev, node: a.node, state: 'raised', at: a.raisedAt, rx: a.receivedAt, acked: !!a.ackBy, by: a.ackBy, text: a.text });
    }
    renderFeed();
  }

  async function start() {
    $('login').hidden = true;
    $('app').hidden = false;
    try {
      renderOverview(await api('/api/fleet/summary'));
      await loadActive();
    } catch (e) { return; }
    connect();
    setInterval(() => { if (!searching && lastOverview) renderSites(lastOverview.problems || []); }, 10000); // refresh "since" columns
  }

  $('login-form').addEventListener('submit', (e) => {
    e.preventDefault();
    token = $('token').value.trim();
    try { sessionStorage.setItem('nocToken', token); } catch (err) { /* ignore */ }
    if (token) start();
  });

  if (token) start(); else $('login').hidden = false;
})();
