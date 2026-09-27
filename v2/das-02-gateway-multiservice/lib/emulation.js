'use strict';
/**
 * Embedded-CPU emulation for demos on fast development machines.
 *
 * A modern laptop core is roughly 6-10x faster single-threaded than a
 * Cortex-A53-class SoC running V8. With DAS_CPU_SLOWDOWN=K every piece of heavy
 * CPU work (parse, transform, serialise, compress) runs K times, in both legacy
 * and hotfix modes, wherever that work happens (event loop, worker thread or
 * libuv pool). That approximates the device's CPU budget without special
 * hardware. Leave it at 1 (the default) on the real device.
 *
 * ES2019 / CommonJS.
 */

var factor = Math.max(1, parseInt(process.env.DAS_CPU_SLOWDOWN || '1', 10) || 1);

/** Run fn `factor` times and return the last result. */
function heavy(fn) {
  var r;
  for (var i = 0; i < factor; i++) r = fn();
  return r;
}

/** Async variant: run the promise-returning fn `factor` times sequentially. */
function heavyAsync(fn) {
  var p = fn();
  for (var i = 1; i < factor; i++) p = p.then(function () { return fn(); });
  return p;
}

module.exports = { factor: factor, heavy: heavy, heavyAsync: heavyAsync };
