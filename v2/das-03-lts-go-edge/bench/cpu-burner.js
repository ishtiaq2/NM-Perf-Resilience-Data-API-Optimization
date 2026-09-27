#!/usr/bin/env node
'use strict';
/**
 * Emulates the other daemons on the Master Node (RF control, SNMP agent, alarm
 * handling, log rotation...) by burning a fixed share of one CPU core.
 *   node bench/cpu-burner.js 10     # ~10 % of one core, until killed
 */
const pct = Math.max(1, Math.min(90, Number(process.argv[2] || 10)));
const periodMs = 50;
const busyMs = (periodMs * pct) / 100;
(function cycle() {
  const t = Date.now();
  while (Date.now() - t < busyMs) { /* spin */ }
  setTimeout(cycle, periodMs - busyMs);
})();
