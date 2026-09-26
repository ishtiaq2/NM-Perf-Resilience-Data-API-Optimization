# ADR 0005: A Go single-binary edge server as the long-term data plane

- Status: proposed (decision point after migration step 2)
- Date: 2026-09-26

## Context

Node.js needs extra processes or threads to keep CPU work off the request path, uses
~200+ MB on the device, and ships as a runtime plus node_modules. The target device is an
embedded ARM Linux system shared with RF and alarm daemons.

## Decision

Build `das-edge` in Go:

- one static, cross-compiled binary (arm64/armv7 ≈ 7.5 MB);
- the UI, REST, WebSocket push and node ingest, all from one process;
- heavy work on reniced OS threads, and limits everywhere;
- systemd `Type=notify` with a watchdog that exercises the real heartbeat path.

The PoC uses only the standard library, because module downloads were blocked in the
build environment. Production may adopt `coder/websocket`, `connect-go` and the
Prometheus client at single seams.

## Alternatives

- Rust: leaner, steeper for the team.
- C++: high risk.
- Java or .NET: too heavy.
- Staying on Node with 02: plan B.

See OPTIONS.md.

## Consequences

- Measured: heartbeat p99 4.7 ms, 408 vs 120 spectrum responses (hotfix), 43 MB PSS,
  conformance 18/18.
- The team needs Go skills (2 weeks ramp-up with the PoC as the curriculum).
- The build pipeline needs Go (or vendored modules) and a Yocto/Buildroot recipe for one
  binary.
