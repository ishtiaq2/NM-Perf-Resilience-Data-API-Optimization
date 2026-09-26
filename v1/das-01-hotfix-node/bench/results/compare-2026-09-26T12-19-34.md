# Heartbeat under spectrum-analyzer load

Measured 2026-09-26 12:19 UTC on Intel(R) Xeon(R) Processor @ 2.80GHz, Node v22.22.2, server pinned to 1 CPU core (taskset -c 0), load generator on another core. Embedded CPU emulated with DAS_CPU_SLOWDOWN=8 (all heavy CPU work runs 8x, in both modes). Other Master Node daemons emulated by a 10 % CPU load on the server core.

| Metric | legacy | hotfix |
|---|---:|---:|
| Heartbeat p50 | 2031.3 ms | 1.3 ms |
| Heartbeat p99 | 3000 ms | 5.7 ms |
| Heartbeat max | 3000 ms | 5.7 ms |
| Heartbeat timeouts | 48 / 125 | 0 / 120 |
| False "server dead" alarms (1-miss rule) | 19 | 0 |
| False "server dead" alarms (3-miss rule) | 3 | 0 |
| Server event-loop lag p99 (worst) | n/a | 7.86 ms |
| Spectrum responses (200 / 304) | 74 / 0 | 120 / 0 |
| Spectrum latency p50 / p95 | 3394.1 / 8630.6 ms | 2150.5 / 2291 ms |
| Spectrum updates per trace per minute | 18.5 | 30 |
| Hardware sweeps performed | 74 | 60 |
| Spectrum data transferred | 148.37 MB | 35.96 MB |
| Dashboard (volatile-data) p95 | 10001.3 ms | 49.6 ms |
| Dashboard 304 responses | 0 / 62 | 0 / 60 |
| Dashboard data transferred | 17415 kB | 5170 kB |
| Config read p95 / max | 10001.2 / 10001.4 ms | 4.2 / 4.5 ms |
| Server RSS start / peak | 141 / 211 MB | 147 / 239 MB |
