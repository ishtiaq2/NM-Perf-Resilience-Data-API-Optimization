# Rollout plan for the hotfix (patch release for customers in the field)

## Scope

A backend-only patch. The shipped frontend is untouched: same URLs, same response
bodies (byte-identical, covered by tests), same authentication. Customers install it
like any other backend update.

## Risk assessment

| Change | Risk | Mitigation |
|---|---|---|
| Spectrum work moves to a worker thread | Worker crash or hang | Crash → task rejected, worker respawned with back-off. Hang → per-task timeout terminates it. Both are covered by tests. |
| Sweep sessions shared by clients | Stale data served | Legacy endpoint waits for the **next** sweep (same semantics as before); sessions stop after 15 s idle |
| Responses gzip-compressed when the client accepts it | Client that cannot gunzip | Only sent when `Accept-Encoding: gzip` is present. Every browser handles it transparently. |
| ETag / 304 on volatile-data | Stale browser cache after a restart | ETags contain a per-boot id, so an old tag never matches after a restart. `Cache-Control: no-cache` forces revalidation. |
| Extra memory (cached bodies, 1-2 workers) | OOM on small devices | ~5-15 MB per worker plus ~3 MB per active analyzer session; worker heap capped (`DAS_WORKER_HEAP_MB`) |
| Old Node version on the device | Syntax / API errors | Runtime code is ES2019, checked by `npm run check:es2019`. APIs need Node 12.16+. |

## Kill switch

`DAS_MODE=legacy` restores the old code path at the next restart, with no redeploy
(systemd drop-in or `/etc/default` file). Support can apply it remotely.

## Test plan

1. `npm test` on CI: unit, compatibility (golden files from a real device) and the heartbeat SLO test.
2. Lab device, slowest supported hardware:
   * `node bench/heartbeat-under-load.js --base http://<device> --duration 300`, before and after.
   * Acceptance: heartbeat p99 < 100 ms and no timeouts with 4 engineers × 2 traces;
     config read p95 < 100 ms; spectrum updates per trace ≥ legacy.
3. Soak: 24 h with the analyzer open in 2 browsers. RSS must stay flat (± 5 %),
   with no `worker_error` or `worker_timeout` log lines.
4. Fault injection: kill the hardware daemon mid-sweep. Expect `sweep_error` logs
   and 503 + `Retry-After` on the new endpoint. The legacy endpoint returns the last
   sweep, and the heartbeat stays green.
5. Upgrade/downgrade: install over the current release, then switch `DAS_MODE` back
   and forth.

## Observability after rollout

* Heartbeat body: `eventLoop.lagP99Ms`, `workers.busy/queued`, `status: degraded`.
* `GET /api/metrics` (JSON) or `?format=prom` (Prometheus text).
* Log events (JSON lines on stderr): `event_loop_blocked` with suspect URLs,
  `sweep_error`, `worker_error`, `worker_timeout`, `config_changed`.

## Release note (customer-facing draft)

> **Improved responsiveness while using Engineering Tools.** Spectrum Analyzer data
> is now processed in the background and shared between users viewing the same
> analyzer. The user interface stays responsive and no longer reports the Master Node
> as unreachable while a sweep is running. Spectrum data traffic is reduced by about
> 80 %. No changes to the user interface or to integrations.
