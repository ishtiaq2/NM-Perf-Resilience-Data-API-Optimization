# Architecture decision records

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-one-origin-one-versioned-contract.md) | One origin and one versioned contract for every backend | accepted |
| [0002](0002-hotfix-workers-shared-sweeps.md) | Hotfix = worker threads, shared sweeps and incremental serialisation | accepted |
| [0003](0003-websocket-push-with-http-fallback.md) | WebSocket push with HTTP fallback for telemetry and spectrum | accepted |
| [0004](0004-change-detection-deadbands-hashes.md) | Change detection by field hygiene, deadbands and a canonical state hash | accepted |
| [0005](0005-go-single-binary-edge.md) | A Go single-binary edge server as the long-term data plane | proposed (decision point after migration step 2) |
| [0006](0006-connect-grpc-contract-for-node-ingest.md) | Remote Node ingest as a gRPC-compatible streaming contract (Connect) | proposed |
| [0007](0007-strangler-fig-migration.md) | Migrate by strangler fig, one endpoint group per step | accepted |
| [0008](0008-binary-spectrum-frames-peak-decimation.md) | Binary spectrum frames and server-side peak decimation | accepted |
