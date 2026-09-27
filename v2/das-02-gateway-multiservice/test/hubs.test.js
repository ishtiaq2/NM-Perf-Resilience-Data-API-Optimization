'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const WebSocket = require('ws');
const { VolatileStore } = require('../lib/volatile-store');
const { TelemetryHub } = require('../lib/telemetry-hub');
const { originAllowed } = require('../lib/ws-util');

function node(id, t) { return { id, name: 'RN-' + id, status: 'online', temperatureC: t }; }

function fakeClient(buffered = 0) {
  const sent = [];
  return { sent, ws: { readyState: WebSocket.OPEN, bufferedAmount: buffered, send: (s) => sent.push(JSON.parse(s)) }, rev: -1, subscribed: false, lagging: false, lastSend: 0 };
}

function hubWith(store) {
  const hub = new TelemetryHub({ store, server: 'test', compress: false });
  hub.close(); // stop timers; we drive the hub by hand
  return hub;
}

test('up-to-date clients get exactly one shared delta per revision', () => {
  const store = new VolatileStore({ bootId: 'b' });
  store.ingest(node(1, 40)); store.ingest(node(2, 41)); store.publish();
  const hub = hubWith(store);
  const a = fakeClient(); const b = fakeClient();
  hub.clients.add(a); hub.clients.add(b);
  hub._subscribe(a); hub._subscribe(b);
  assert.equal(a.sent[0].type, 'snapshot');
  store.ingest(node(2, 55));
  store.publish();
  assert.deepEqual(a.sent[1], b.sent[1]);
  assert.equal(a.sent[1].type, 'delta');
  assert.deepEqual(Object.keys(a.sent[1].changed), ['2']);
  assert.equal(a.rev, store.rev);
});

test('a slow client is skipped, then gets ONE combined catch-up delta', () => {
  const store = new VolatileStore({ bootId: 'b' });
  store.ingest(node(1, 40)); store.ingest(node(2, 41)); store.publish();
  const hub = hubWith(store);
  const slow = fakeClient();
  hub.clients.add(slow);
  hub._subscribe(slow);
  slow.ws.bufferedAmount = 10 * 1024 * 1024; // socket backed up
  for (let i = 0; i < 5; i++) { store.ingest(node(1, 50 + i)); store.ingest(node(2, 60 + i)); store.publish(); }
  assert.equal(slow.sent.length, 1, 'nothing queued while slow');
  assert.ok(slow.lagging);
  slow.ws.bufferedAmount = 0;
  hub._tick();
  assert.equal(slow.sent.length, 2);
  const d = slow.sent[1];
  assert.equal(d.type, 'delta');
  assert.equal(d.rev, store.rev);
  assert.equal(d.changed['1'].temperatureC, 54);
});

test('resume: a reconnecting client gets only what it missed', () => {
  const store = new VolatileStore({ bootId: 'b' });
  store.ingest(node(1, 40)); store.ingest(node(2, 41)); store.publish();
  const had = store.rev;
  store.ingest(node(2, 70)); store.publish();
  const hub = hubWith(store);
  const c = fakeClient();
  hub.clients.add(c);
  hub._subscribe(c, had);
  assert.equal(c.sent[0].type, 'delta');
  assert.equal(c.sent[0].base, had);
  assert.deepEqual(Object.keys(c.sent[0].changed), ['2']);
  const d = fakeClient();
  hub._subscribe(d, 999);
  assert.equal(d.sent[0].type, 'snapshot', 'unknown revision (other boot) -> snapshot');
});

test('Origin check (CSWSH)', () => {
  const req = (origin, host = 'master.local') => ({ headers: origin ? { origin, host } : { host } });
  assert.equal(originAllowed(req(undefined)), true, 'non-browser client');
  assert.equal(originAllowed(req('https://master.local')), true);
  assert.equal(originAllowed(req('http://master.local:8080', 'master.local:8080')), true);
  assert.equal(originAllowed(req('https://evil.example')), false);
  assert.equal(originAllowed(req('https://tool.example'), ), false);
  assert.equal(originAllowed(req('https://tool.example'), ['https://tool.example']), true);
  assert.equal(originAllowed(req('not a url')), false);
});
