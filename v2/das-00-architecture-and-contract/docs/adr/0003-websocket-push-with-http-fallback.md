# ADR 0003: WebSocket push with HTTP fallback for telemetry and spectrum

- Status: accepted
- Date: 2026-09-26

## Context

The UI polls `volatile_data` and the spectrum sequentially. Polling costs requests,
bandwidth and freshness, and on HTTP/1.1 it competes for the browser's 6 connections.

## Decision

- `/api/ws/telemetry`: a snapshot, then deltas per revision, resumable from a revision.
- `/api/ws/spectrum`: one binary DSPC frame per sweep, decimated to the requested width.
- The server has one writer per client that is **woken, never fed from a queue**, so a
  slow client is conflated to the latest state.
- The client falls back to HTTP (`?since=` deltas + ETag, then legacy polling) when a
  socket cannot be opened. It discovers availability through capabilities.
- SSE was rejected because it has no binary frames. WebTransport was rejected for now
  (proxies, Safari). gRPC-Web was rejected for the browser (needs a proxy, no
  bidirectional streaming).

## Consequences

- Measured (02, 20 dashboards): 4.6× less bandwidth, data ~130× fresher.
- Measured (03): 300 revisions on a throttled link arrive as 2 messages.
- Operations must allow WebSocket through reverse proxies (`Upgrade` headers). The
  same-origin `Origin` check prevents cross-site WebSocket hijacking.
