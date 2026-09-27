# ADR 0002: Hotfix = worker threads, shared sweeps and incremental serialisation

- Status: accepted
- Date: 2026-09-26

## Context

Customers on the shipped release get false "server is dead" alarms. The fix must be
backend-only, byte-compatible, installable without new binaries or npm packages, and
reversible.

## Decision

- One **sweep session** per analyzer configuration, shared by every client; the body is
  built once per sweep.
- Parsing, serialisation and gzip run in **worker threads** at nice 10, with zero-copy
  buffer transfer.
- `volatile_data` is **serialised incrementally**: once per node report, and joined at
  most once per revision (1/s), with ETag/304.
- Opt-in `/api/spectrum/latest` (compact JSON or binary, decimated, long-poll).
- A kill switch: `DAS_MODE=legacy`.

## Consequences

- Measured: heartbeat p99 3 000 ms → 5.7 ms, false alarms 19 → 0, spectrum data
  148 → 36 MB.
- It is still one process. Other blocking code in the product can still cause stalls;
  the blocked-loop detector logs them.
- Memory rises slightly (239 vs 211 MB peak) because of the worker's V8 heap.
