'use strict';
/**
 * Minimal, dependency-free worker_threads pool for CPU-heavy work
 * (parsing, serialising, compressing large payloads).
 *
 * The rule it enforces: the main thread never touches a large payload.
 * Workers receive raw bytes (transferred, zero-copy), do all CPU work, and hand
 * back ready-to-send Buffers (transferred, zero-copy).
 *
 * Features
 *  - bounded queue: rejects with code POOL_BUSY instead of building an unbounded backlog
 *  - per-task timeout: a stuck task terminates its worker, which is respawned
 *  - crash isolation: a worker that throws or exits is replaced (exponential backoff)
 *  - lower CPU priority for workers (Linux: nice is per-thread, so only the
 *    worker threads are de-prioritised and the event loop always wins the CPU)
 *  - V8 heap cap per worker (resourceLimits) to protect a RAM-constrained device
 *
 * Node 12+ (worker_threads, resourceLimits need 12.16+/13.2+). ES2019 / CommonJS.
 */

var wt = require('worker_threads');
var os = require('os');

function poolError(code, message) {
  var e = new Error(message);
  e.code = code;
  return e;
}

function defaultSize() {
  var cpus = (os.cpus() || []).length || 1;
  // Leave one core to the event loop; never more than 2 workers on an embedded box.
  return Math.max(1, Math.min(2, cpus - 1));
}

/**
 * @param {object} opts
 * @param {string} opts.script absolute path to the worker script
 * @param {number} [opts.size]
 * @param {number} [opts.maxQueue=32]
 * @param {number} [opts.taskTimeoutMs=15000]
 * @param {number} [opts.niceness=10] 0 = leave priority unchanged
 * @param {object} [opts.resourceLimits] e.g. {maxOldGenerationSizeMb: 192}
 * @param {object} [opts.workerData]
 * @param {function(string,object):void} [opts.log]
 */
function WorkerPool(opts) {
  if (!opts || !opts.script) throw new TypeError('WorkerPool: opts.script is required');
  this.script = opts.script;
  this.size = opts.size || defaultSize();
  this.maxQueue = opts.maxQueue == null ? 32 : opts.maxQueue;
  this.taskTimeoutMs = opts.taskTimeoutMs || 15000;
  this.niceness = opts.niceness == null ? 10 : opts.niceness;
  this.resourceLimits = opts.resourceLimits;
  this.workerData = opts.workerData || {};
  this.log = opts.log || function () {};
  this.slots = [];
  this.queue = [];
  this.nextId = 1;
  this.closed = false;
  this.stats = { submitted: 0, completed: 0, failed: 0, timeouts: 0, rejected: 0, restarts: 0 };
  for (var i = 0; i < this.size; i++) this.slots.push(this._spawn({ index: i, backoffMs: 100 }));
}

WorkerPool.prototype._spawn = function (slot) {
  var self = this;
  var data = Object.assign({}, this.workerData, { niceness: this.niceness });
  var options = { workerData: data };
  if (this.resourceLimits) options.resourceLimits = this.resourceLimits;
  var worker = new wt.Worker(this.script, options);
  slot.worker = worker;
  slot.task = null;
  slot.alive = true;
  slot.timer = null;

  worker.on('message', function (msg) {
    var task = slot.task;
    if (!task || !msg || msg.id !== task.id) return;
    clearTimeout(slot.timer);
    slot.task = null;
    slot.backoffMs = 100; // healthy again
    if (msg.ok) { self.stats.completed++; task.resolve(msg.result); } else { self.stats.failed++; task.reject(poolError(msg.code || 'TASK_FAILED', msg.error)); }
    self._drain();
  });
  worker.on('error', function (err) {
    self.log('worker_error', { slot: slot.index, error: String(err && err.stack || err) });
    // 'exit' follows; the in-flight task is rejected there.
    slot.lastError = err;
  });
  worker.on('exit', function (code) {
    slot.alive = false;
    clearTimeout(slot.timer);
    var task = slot.task;
    slot.task = null;
    if (task) {
      self.stats.failed++;
      task.reject(slot.timedOut ? poolError('TASK_TIMEOUT', 'task ' + task.type + ' exceeded ' + self.taskTimeoutMs + ' ms') :
        poolError('WORKER_EXIT', 'worker exited with code ' + code + (slot.lastError ? ': ' + slot.lastError.message : '')));
    }
    slot.timedOut = false;
    slot.lastError = null;
    if (self.closed) return;
    self.stats.restarts++;
    var delay = slot.backoffMs;
    slot.backoffMs = Math.min(5000, slot.backoffMs * 2);
    setTimeout(function () { if (!self.closed) { self._spawn(slot); self._drain(); } }, delay);
  });
  return slot;
};

/**
 * Run a task in a worker.
 * @param {string} type handler name registered in the worker script
 * @param {*} payload structured-cloneable payload
 * @param {Array<ArrayBuffer>} [transferList] buffers to move (not copy) to the worker
 * @returns {Promise<*>}
 */
WorkerPool.prototype.run = function (type, payload, transferList) {
  var self = this;
  if (this.closed) return Promise.reject(poolError('POOL_CLOSED', 'pool is closed'));
  if (this.queue.length >= this.maxQueue) {
    this.stats.rejected++;
    return Promise.reject(poolError('POOL_BUSY', 'worker queue full (' + this.maxQueue + ')'));
  }
  this.stats.submitted++;
  return new Promise(function (resolve, reject) {
    self.queue.push({ id: self.nextId++, type: type, payload: payload, transfer: transferList || [], resolve: resolve, reject: reject });
    self._drain();
  });
};

WorkerPool.prototype._drain = function () {
  for (var i = 0; i < this.slots.length && this.queue.length; i++) {
    var slot = this.slots[i];
    if (!slot.alive || slot.task) continue;
    var task = this.queue.shift();
    slot.task = task;
    try {
      slot.worker.postMessage({ id: task.id, type: task.type, payload: task.payload }, task.transfer);
    } catch (err) {
      slot.task = null;
      this.stats.failed++;
      task.reject(err);
      continue;
    }
    slot.timer = setTimeout(this._onTimeout.bind(this, slot, task), this.taskTimeoutMs);
  }
};

WorkerPool.prototype._onTimeout = function (slot, task) {
  if (slot.task !== task) return;
  this.stats.timeouts++;
  slot.timedOut = true;
  this.log('worker_timeout', { slot: slot.index, type: task.type });
  slot.worker.terminate(); // synchronous JS cannot be interrupted any other way
};

WorkerPool.prototype.status = function () {
  var busy = 0, alive = 0;
  for (var i = 0; i < this.slots.length; i++) { if (this.slots[i].alive) alive++; if (this.slots[i].task) busy++; }
  return { size: this.size, alive: alive, busy: busy, queued: this.queue.length, stats: Object.assign({}, this.stats) };
};

WorkerPool.prototype.close = function () {
  this.closed = true;
  var q = this.queue.splice(0);
  for (var i = 0; i < q.length; i++) q[i].reject(poolError('POOL_CLOSED', 'pool is closed'));
  var ps = [];
  for (var j = 0; j < this.slots.length; j++) if (this.slots[j].worker) ps.push(this.slots[j].worker.terminate());
  return Promise.all(ps);
};

/**
 * Helper for worker scripts: register handlers and apply niceness.
 * Each handler returns {result, transfer?} or a Promise of it.
 * @param {Object<string,function(*):*>} handlers
 */
function serveTasks(handlers) {
  var data = wt.workerData || {};
  if (data.niceness && process.platform === 'linux') {
    // On Linux setpriority(PRIO_PROCESS, 0) applies to the calling *thread* only.
    // On macOS/Windows it would affect the whole process, so skip it there.
    try { os.setPriority(data.niceness); } catch (e) { /* not fatal */ }
  }
  wt.parentPort.on('message', function (msg) {
    var h = handlers[msg.type];
    if (!h) {
      wt.parentPort.postMessage({ id: msg.id, ok: false, code: 'UNKNOWN_TASK', error: 'unknown task type ' + msg.type });
      return;
    }
    Promise.resolve()
      .then(function () { return h(msg.payload); })
      .then(function (out) {
        out = out || {};
        wt.parentPort.postMessage({ id: msg.id, ok: true, result: out.result }, out.transfer || []);
      }, function (err) {
        wt.parentPort.postMessage({ id: msg.id, ok: false, code: err && err.code, error: String(err && err.message || err) });
      });
  });
}

/** Make a Buffer transferable: returns its ArrayBuffer if exclusively owned, else a copy. */
function toTransferable(buf) {
  if (buf.byteOffset === 0 && buf.byteLength === buf.buffer.byteLength && !(typeof SharedArrayBuffer !== 'undefined' && buf.buffer instanceof SharedArrayBuffer)) {
    return buf.buffer;
  }
  var ab = new ArrayBuffer(buf.byteLength);
  new Uint8Array(ab).set(buf);
  return ab;
}

module.exports = { WorkerPool: WorkerPool, serveTasks: serveTasks, toTransferable: toTransferable, defaultSize: defaultSize };
