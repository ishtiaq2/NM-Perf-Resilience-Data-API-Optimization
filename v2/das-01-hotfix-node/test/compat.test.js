'use strict';
/**
 * Backward compatibility: the hotfix must return the same bytes as the shipped
 * release, so customers can install it without a frontend update.
 */
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const zlib = require('node:zlib');
const formats = require('../src/shared/formats');
const { SimulatedSpectrumHardware } = require('../src/shared/spectrum-sim');
const { WorkerPool, toTransferable } = require('../src/shared/worker-pool');
const { VolatileStore } = require('../src/shared/volatile-store');
const { RemoteNodeSimulator } = require('../src/shared/node-sim');

const script = path.join(__dirname, '..', 'src', 'shared', 'spectrum-worker.js');

test('spectrum: worker-built legacy body is byte-identical to the legacy handler output', async (t) => {
  const hw = new SimulatedSpectrumHardware({ sweepTimeMs: 0 });
  const raw = await hw.sweep({ nodeId: 3, port: 2, points: 20001 });
  const meta = { sweepId: 9, timestamp: 1790000000123 };
  const legacy = JSON.stringify(formats.legacySpectrumBody(JSON.parse(raw.toString('utf8')), meta));

  const pool = new WorkerPool({ script, size: 1 });
  t.after(() => pool.close());
  const ab = toTransferable(Buffer.from(raw));
  const out = await pool.run('spectrum.parse', { raw: ab, sweepId: 9, timestamp: 1790000000123, legacy: true, gzipLevel: 1 }, [ab]);
  assert.equal(zlib.gunzipSync(Buffer.from(out.legacyGzip)).toString('utf8'), legacy);
  // Canonical data agrees with the legacy values.
  const power = new Float32Array(out.power);
  const pts = JSON.parse(legacy).points;
  assert.equal(power.length, pts.length);
  for (let i = 0; i < pts.length; i += 997) assert.equal(formats.round2(power[i]), pts[i].power);
  assert.equal(out.meta.startHz + 5 * out.meta.stepHz, pts[5].frequency);
});

test('volatile_data: incremental snapshot is byte-identical to JSON.stringify of the merged state', async () => {
  const sim = new RemoteNodeSimulator({ nodes: 50, seed: 3 });
  const store = new VolatileStore({ bootId: 't', normalize: false });
  const merged = {};
  sim.on('report', (r) => { merged[r.id] = r; store.ingest(r); });
  sim.start(); // initial report from every node
  for (let i = 0; i < 5; i++) { for (const n of sim.nodes) n.nextAt = 0; sim.tick(); }
  sim.stop();
  store.publish();
  const snap = await store.snapshotBody(false);
  const expected = formats.legacyVolatileJson(store.rev, store.generatedAt, merged);
  assert.equal(snap.body.toString('utf8'), expected);
  const gz = await store.snapshotBody(true);
  assert.equal(zlib.gunzipSync(gz.body).toString('utf8'), expected);
  assert.notEqual(gz.etag, snap.etag, 'gzip and identity are different representations');
});
