'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const fs = require('node:fs');
const os = require('node:os');
const { WorkerPool } = require('../src/shared/worker-pool');

const script = path.join(__dirname, '..', 'src', 'shared', 'spectrum-worker.js');

test('runs tasks and transfers buffers zero-copy in both directions', async (t) => {
  const pool = new WorkerPool({ script, size: 1 });
  t.after(() => pool.close());
  assert.deepEqual(await pool.run('test.echo', { a: 1 }), { a: 1 });
  const ab = new ArrayBuffer(1 << 20);
  const res = await pool.run('test.transfer', { buf: ab }, [ab]);
  assert.equal(ab.byteLength, 0, 'input buffer was moved, not copied');
  assert.equal(res.inLen, 1 << 20);
  assert.ok(res.out instanceof ArrayBuffer);
  assert.equal(res.out.byteLength, 1 << 20);
});

test('the event loop stays responsive while a worker burns CPU', async (t) => {
  const pool = new WorkerPool({ script, size: 1 });
  t.after(() => pool.close());
  const spin = pool.run('test.spin', { ms: 400 });
  const lags = [];
  let last = Date.now();
  await new Promise((resolve) => {
    const iv = setInterval(() => { const now = Date.now(); lags.push(now - last - 10); last = now; }, 10);
    spin.then(() => { clearInterval(iv); resolve(); });
  });
  assert.ok(Math.max(...lags) < 60, `max timer lag ${Math.max(...lags)} ms`);
});

test('a crashing worker rejects its task and is replaced', async (t) => {
  const pool = new WorkerPool({ script, size: 1 });
  t.after(() => pool.close());
  await assert.rejects(pool.run('test.crash', {}), (e) => e.code === 'WORKER_EXIT');
  assert.deepEqual(await pool.run('test.echo', 'still alive'), 'still alive');
  assert.equal(pool.status().stats.restarts, 1);
});

test('a stuck task times out, its worker is terminated and replaced', async (t) => {
  const pool = new WorkerPool({ script, size: 1, taskTimeoutMs: 150 });
  t.after(() => pool.close());
  await assert.rejects(pool.run('test.spin', { ms: 5000 }), (e) => e.code === 'TASK_TIMEOUT');
  assert.deepEqual(await pool.run('test.echo', 1), 1);
});

test('the queue is bounded: overload is rejected, not buffered forever', async (t) => {
  const pool = new WorkerPool({ script, size: 1, maxQueue: 2 });
  t.after(() => pool.close());
  const running = pool.run('test.spin', { ms: 200 });
  const q1 = pool.run('test.echo', 1);
  const q2 = pool.run('test.echo', 2);
  await assert.rejects(pool.run('test.echo', 3), (e) => e.code === 'POOL_BUSY');
  await Promise.all([running, q1, q2]);
  assert.equal(pool.status().stats.rejected, 1);
});

test('unknown task types are reported, not hung', async (t) => {
  const pool = new WorkerPool({ script, size: 1 });
  t.after(() => pool.close());
  await assert.rejects(pool.run('nope', {}), (e) => e.code === 'UNKNOWN_TASK');
});

test('Linux: niceness lowers only the worker threads, never the event loop', { skip: process.platform !== 'linux' }, async (t) => {
  const pool = new WorkerPool({ script, size: 1, niceness: 10 });
  t.after(() => pool.close());
  const r = await pool.run('test.nice', {});
  assert.equal(r.nice, 10, 'worker thread runs at nice 10');
  assert.equal(os.getPriority(), 0, 'main thread keeps nice 0');
  // Cross-check through /proc: the main thread (tid == pid) is still at 0.
  const stat = fs.readFileSync(`/proc/self/task/${process.pid}/stat`, 'utf8');
  assert.equal(stat.slice(stat.lastIndexOf(')') + 2).split(' ')[16], '0');
});
