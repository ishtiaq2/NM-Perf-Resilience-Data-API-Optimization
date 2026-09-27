'use strict';
/**
 * Runtime configuration from environment variables.
 * Every hotfix behaviour can be tuned (or switched off) without a rebuild.
 */

function int(name, def) {
  var v = process.env[name];
  if (v === undefined || v === '') return def;
  var n = parseInt(v, 10);
  return isFinite(n) ? n : def;
}

function str(name, def) {
  var v = process.env[name];
  return v === undefined || v === '' ? def : v;
}

// `node src/server.js --legacy` works on every OS; DAS_MODE is for service files.
var mode = process.argv.indexOf('--legacy') >= 0 ? 'legacy' : str('DAS_MODE', 'hotfix');
if (mode !== 'hotfix' && mode !== 'legacy') throw new Error('DAS_MODE must be "hotfix" or "legacy"');

module.exports = {
  /** "hotfix" (default) or "legacy" (reproduces the shipped behaviour, for before/after demos). */
  mode: mode,
  host: str('HOST', '0.0.0.0'),
  port: int('PORT', 8080),
  version: '1.1.0-hotfix',

  // Worker threads
  workers: int('DAS_WORKERS', 0), // 0 = auto: min(2, cpus - 1), at least 1
  workerNice: int('DAS_WORKER_NICE', 10), // Linux: lowers only the worker threads' priority
  workerHeapMb: int('DAS_WORKER_HEAP_MB', 96), // V8 old-generation cap per worker
  workerYoungMb: int('DAS_WORKER_YOUNG_MB', 8), // V8 young-generation cap per worker (memory vs GC frequency)
  workerQueue: int('DAS_WORKER_QUEUE', 32),
  taskTimeoutMs: int('DAS_TASK_TIMEOUT_MS', 15000),
  gzipLevel: int('DAS_GZIP_LEVEL', 1), // level 1: ~90% of the size win for ~1/3 of the CPU of level 6

  // Spectrum analyzer
  sweepTimeMs: int('DAS_SWEEP_TIME_MS', 250),
  defaultPoints: int('DAS_SPECTRUM_POINTS', 50001),
  spectrumIdleMs: int('DAS_SPECTRUM_IDLE_MS', 15000),
  spectrumMinIntervalMs: int('DAS_SPECTRUM_MIN_INTERVAL_MS', 0),
  spectrumMaxSessions: int('DAS_SPECTRUM_MAX_SESSIONS', 8),
  sweepWaitMs: int('DAS_SWEEP_WAIT_MS', 10000),
  simPrewarm: int('DAS_SIM_PREWARM', 4), // simulator: analyzers (node 1..N, port 1) generated at start-up

  // Remote nodes / volatile_data
  nodes: int('DAS_NODES', 300),
  reportIntervalMs: int('DAS_REPORT_INTERVAL_MS', 1000),
  publishIntervalMs: int('DAS_PUBLISH_INTERVAL_MS', 1000),

  // Observability
  blockWarnMs: int('DAS_BLOCK_WARN_MS', 200),
  logLevel: str('DAS_LOG', 'info')
};
