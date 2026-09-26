# ADR 0006: Remote Node ingest as a gRPC-compatible streaming contract (Connect)

- Status: proposed
- Date: 2026-09-26

## Context

Hundreds of Remote Nodes send their state to the Master Node, which merges it into
`volatile_data`. Today they send full reports. We want typed, versioned messages,
per-node flow control, deltas, and self-healing when either side restarts.

## Decision

- `das.v1.NodeIngestService.Report`: one bidirectional stream per node over HTTP/2.
  Messages: full / delta / keep-alive, with `base_hash` and `state_hash`. Answers:
  `OK`, `RESYNC`, `SLOW_DOWN`.
- The reference server speaks the **Connect** protocol (JSON codec) over h2c. With
  generated connect-go code the same path also serves gRPC and gRPC-Web with binary
  protobuf.
- The contract is checked with `buf lint` and `buf breaking`. Interop is tested with the
  official Connect-ES client.
- mTLS per node in production.

## Alternatives

- MQTT with the same payload design: a good fit for MCU-class nodes or an existing
  broker.
- NATS, CoAP/LwM2M, OPC UA PubSub, DDS: see OPTIONS.md §6.

## Consequences

- Measured: 24× fewer bytes than full JSON reports, 0 resyncs, master CPU ~2 % of a
  core for 300 nodes.
- Node firmware needs an HTTP/2 client (grpc-c++, Go, or Connect over any HTTP/2 stack).
  A mixed fleet is supported during rollout.
