'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { VolatileStore, applyDeadband, hygiene, DEFAULT_DEADBANDS } = require('../src/shared/volatile-store');

function node(id, extra = {}) {
  return { id, name: 'RN-' + id, seq: 1, reportedAt: 1790000000000, uptimeS: 100, temperatureC: 40, bands: [{ name: 'B3', dlOutDbm: 30, vswr: 1.2 }], metrics: { m00: 10 }, ...extra };
}

test('unchanged reports do not create a new revision', () => {
  const s = new VolatileStore({ bootId: 'b', revBase: 0 });
  s.ingest(node(1));
  assert.ok(s.publish());
  assert.equal(s.ingest(node(1)), false);
  assert.equal(s.publish(), null);
  assert.equal(s.rev, 1);
});

test('ETag carries boot id and revision, so a restarted server never matches an old tag', () => {
  const a = new VolatileStore({ bootId: 'boot1', revBase: 0 });
  a.ingest(node(1)); a.publish();
  const b = new VolatileStore({ bootId: 'boot2', revBase: 0 });
  b.ingest(node(1)); b.publish();
  assert.equal(a.rev, b.rev);
  assert.notEqual(a.etag(), b.etag());
});

test('deltas reconstruct the latest snapshot from an older one', async () => {
  const s = new VolatileStore({ bootId: 'b' });
  for (let i = 1; i <= 5; i++) s.ingest(node(i));
  s.publish();
  const base = JSON.parse((await s.snapshotBody(false)).body.toString());
  s.ingest(node(2, { temperatureC: 55 }));
  s.publish();
  s.ingest(node(4, { temperatureC: 61 }));
  s.ingest(node(6));
  s.remove(5);
  s.publish();
  const d = JSON.parse(s.deltaJson(base.rev));
  assert.equal(d.base, base.rev);
  assert.deepEqual(Object.keys(d.changed).sort(), ['2', '4', '6']);
  assert.deepEqual(d.removed, [5]);
  const nodes = { ...base.nodes, ...d.changed };
  for (const id of d.removed) delete nodes[id];
  const latest = JSON.parse((await s.snapshotBody(false)).body.toString());
  assert.deepEqual(nodes, latest.nodes);
});

test('delta for the current revision is empty; unknown or expired revisions return null', () => {
  const s = new VolatileStore({ bootId: 'b', historySize: 2, revBase: 0 });
  s.ingest(node(1)); s.publish();
  assert.deepEqual(JSON.parse(s.deltaJson(s.rev)).changed, {});
  for (let i = 0; i < 5; i++) { s.ingest(node(1, { temperatureC: 40 + i + 1 })); s.publish(); }
  assert.equal(s.deltaJson(1), null, 'history no longer covers rev 1');
  assert.equal(s.deltaJson(999), null, 'future revision (e.g. from a previous boot)');
  assert.notEqual(s.deltaJson(s.rev - 1), null);
});

test('a revision kept from a previous boot is answered with a snapshot, never a wrong delta', () => {
  const boot1 = new VolatileStore({ bootId: 'a' });
  const boot2 = new VolatileStore({ bootId: 'b' });
  for (const s of [boot1, boot2]) { s.ingest(node(1)); s.ingest(node(2)); s.publish(); }
  for (let i = 0; i < 5; i++) { boot2.ingest(node(1, { temperatureC: 50 + i })); boot2.publish(); }
  assert.ok(boot1.rev >= 2 ** 40 && Number.isSafeInteger(boot2.rev), 'random per-boot revision base');
  assert.equal(boot2.deltaJson(boot1.rev), null);
});

test('boot time jitter from a truncated uptime counter is not a change', () => {
  const s = new VolatileStore({ bootId: 'b', normalize: true });
  s.ingest(node(1, { reportedAt: 1790000000000, uptimeS: 100 }));
  s.publish();
  // 1.999 s later the truncated uptime says 101 s: the derived boot time moved by 999 ms.
  assert.equal(s.ingest(node(1, { seq: 2, reportedAt: 1790000001999, uptimeS: 101 })), false);
  // A reboot is a change.
  assert.equal(s.ingest(node(1, { seq: 3, reportedAt: 1790000003000, uptimeS: 2 })), true);
});

test('deadband keeps the published value until the change is significant', () => {
  const prev = { temperatureC: 40.0, bands: [{ dlOutDbm: 30.0 }], metrics: { m00: 10 } };
  const small = applyDeadband(prev, { temperatureC: 40.3, bands: [{ dlOutDbm: 30.2 }], metrics: { m00: 11 } }, DEFAULT_DEADBANDS, '');
  assert.deepEqual(small, prev);
  const big = applyDeadband(prev, { temperatureC: 41.0, bands: [{ dlOutDbm: 30.2 }], metrics: { m00: 13 } }, DEFAULT_DEADBANDS, '');
  assert.equal(big.temperatureC, 41.0);
  assert.equal(big.bands[0].dlOutDbm, 30.0);
  assert.equal(big.metrics.m00, 13);
});

test('field hygiene removes per-report counters and publishes a stable boot time', () => {
  const a = hygiene(node(1, { seq: 1, reportedAt: 1790000000000, uptimeS: 100 }));
  const b = hygiene(node(1, { seq: 2, reportedAt: 1790000001000, uptimeS: 101 }));
  assert.equal(a.seq, undefined);
  assert.equal(a.reportedAt, undefined);
  assert.equal(a.uptimeS, undefined);
  assert.deepEqual(a, b);
});

test('normalised store suppresses noise-only reports', () => {
  const s = new VolatileStore({ bootId: 'b', normalize: true });
  s.ingest(node(1));
  s.publish();
  const changed = s.ingest(node(1, { seq: 2, reportedAt: 1790000001000, uptimeS: 101, temperatureC: 40.2 }));
  assert.equal(changed, false);
  assert.equal(s.publish(), null);
});

test('stale nodes are marked offline and come back on the next report', () => {
  const s = new VolatileStore({ bootId: 'b', staleAfterMs: 1 });
  s.ingest(node(1));
  s.publish();
  s.nodes.get(1).lastSeen -= 10;
  const ev = s.publish();
  assert.deepEqual(ev.ids, [1]);
  assert.equal(s.nodeObject(1).status, 'offline');
  assert.equal(s.ingest(node(1)), true);
});

test('normalised store still publishes every legacy field (no field ever disappears)', async () => {
  const s = new VolatileStore({ bootId: 'b', normalize: true });
  const r = node(1, { seq: 7, reportedAt: 1790000000000, uptimeS: 100 });
  s.ingest(r);
  s.publish();
  const published = JSON.parse((await s.snapshotBody(false)).body.toString()).nodes['1'];
  for (const k of Object.keys(r)) assert.ok(k in published, `field ${k} missing`);
  assert.equal(published.bootAt, 1789999900000);
  // A significant change republishes with the current per-report fields.
  s.ingest(node(1, { seq: 9, reportedAt: 1790000002000, uptimeS: 102, temperatureC: 45 }));
  s.publish();
  const again = JSON.parse((await s.snapshotBody(false)).body.toString()).nodes['1'];
  assert.equal(again.seq, 9);
  assert.equal(again.temperatureC, 45);
});
