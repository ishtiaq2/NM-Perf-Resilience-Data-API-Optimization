'use strict';
/**
 * Event-loop health monitor + blocked-loop detector.
 *
 * - Samples timer drift every `sampleMs` and keeps a sliding window, giving
 *   p50/p99/max lag for the heartbeat and metrics.
 * - When one tick is late by more than `warnMs`, calls `onBlocked` with the
 *   duration and the requests that started shortly before the stall, which
 *   points straight at the handler that blocked the loop. Useful in the field.
 *
 * Works on Node 8+ (performance.eventLoopUtilization is used when present).
 * ES2019 / CommonJS.
 */

var perf = require('perf_hooks');

function nowMs() {
  var t = process.hrtime();
  return t[0] * 1e3 + t[1] / 1e6;
}

/**
 * @param {object} [opts]
 * @param {number} [opts.sampleMs=20]
 * @param {number} [opts.windowMs=10000]
 * @param {number} [opts.warnMs=200]
 * @param {function(object):void} [opts.onBlocked]
 * @param {function():Array} [opts.recentRequests] returns [{method,url,startedAt}] (startedAt = monotonic ms)
 */
function LoopMonitor(opts) {
  opts = opts || {};
  this.sampleMs = opts.sampleMs || 20;
  this.warnMs = opts.warnMs || 200;
  this.onBlocked = opts.onBlocked || null;
  this.recentRequests = opts.recentRequests || null;
  var n = Math.max(10, Math.round((opts.windowMs || 10000) / this.sampleMs));
  this.ring = new Float64Array(n);
  this.filled = 0;
  this.pos = 0;
  this.blockedEvents = 0;
  this.worstMs = 0;
  this.last = nowMs();
  this.eluPrev = perf.performance && perf.performance.eventLoopUtilization ? perf.performance.eventLoopUtilization() : null;
  this.cache = null;
  this.cacheAt = 0;
  var self = this;
  this.timer = setInterval(function () { self._tick(); }, this.sampleMs);
  if (this.timer.unref) this.timer.unref();
}

LoopMonitor.prototype._tick = function () {
  var t = nowMs();
  var lag = t - this.last - this.sampleMs;
  this.last = t;
  if (lag < 0) lag = 0;
  this.ring[this.pos] = lag;
  this.pos = (this.pos + 1) % this.ring.length;
  if (this.filled < this.ring.length) this.filled++;
  if (lag > this.worstMs) this.worstMs = lag;
  if (lag >= this.warnMs) {
    this.blockedEvents++;
    if (this.onBlocked) {
      var suspects = [];
      if (this.recentRequests) {
        var from = t - lag - this.sampleMs - 5;
        var reqs = this.recentRequests();
        for (var i = 0; i < reqs.length; i++) {
          if (reqs[i].startedAt >= from) suspects.push(reqs[i].method + ' ' + reqs[i].url);
        }
      }
      try { this.onBlocked({ blockedMs: Math.round(lag), suspects: suspects }); } catch (e) { /* never throw from the monitor */ }
    }
  }
};

/** @returns {{lagP50Ms:number,lagP99Ms:number,lagMaxMs:number,utilization:(number|null),blockedEvents:number}} */
LoopMonitor.prototype.snapshot = function () {
  var t = nowMs();
  if (this.cache && t - this.cacheAt < 250) return this.cache;
  var n = this.filled;
  var arr = Array.prototype.slice.call(this.ring, 0, n).sort(function (a, b) { return a - b; });
  function pct(p) { return n ? arr[Math.min(n - 1, Math.floor((p / 100) * n))] : 0; }
  var util = null;
  if (this.eluPrev) {
    var cur = perf.performance.eventLoopUtilization();
    util = perf.performance.eventLoopUtilization(cur, this.eluPrev).utilization;
    this.eluPrev = cur;
  }
  this.cache = {
    lagP50Ms: Math.round(pct(50) * 100) / 100,
    lagP99Ms: Math.round(pct(99) * 100) / 100,
    lagMaxMs: Math.round((n ? arr[n - 1] : 0) * 100) / 100,
    utilization: util == null ? null : Math.round(util * 1000) / 1000,
    blockedEvents: this.blockedEvents
  };
  this.cacheAt = t;
  return this.cache;
};

LoopMonitor.prototype.stop = function () { clearInterval(this.timer); };

module.exports = { LoopMonitor: LoopMonitor, nowMs: nowMs };
