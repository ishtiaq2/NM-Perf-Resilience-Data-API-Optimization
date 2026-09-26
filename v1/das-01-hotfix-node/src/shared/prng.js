'use strict';
/** Small deterministic PRNG helpers (mulberry32 + Box-Muller). ES2019 / CommonJS. */

function mulberry32(seed) {
  var a = seed >>> 0;
  return function () {
    a = (a + 0x6d2b79f5) >>> 0;
    var t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function gaussianFactory(rand) {
  var spare = null;
  return function gaussian() {
    if (spare !== null) { var s = spare; spare = null; return s; }
    var u = 0, v = 0;
    while (u === 0) u = rand();
    while (v === 0) v = rand();
    var mag = Math.sqrt(-2.0 * Math.log(u));
    spare = mag * Math.sin(2.0 * Math.PI * v);
    return mag * Math.cos(2.0 * Math.PI * v);
  };
}

module.exports = { mulberry32: mulberry32, gaussianFactory: gaussianFactory };
