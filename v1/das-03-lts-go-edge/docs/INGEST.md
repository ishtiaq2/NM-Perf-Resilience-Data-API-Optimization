# Remote Node ingest: push only what changed, verify it with a checksum

The Remote Nodes collect their own telemetry and merge it into `volatile_data` on the
Master Node. This project replaces "report everything, every time" with the
`das.v1.NodeIngestService` contract (`das-00/contract/proto/das/v1/ingest.proto`).
The contract is the same whether it is served by this Go implementation or by generated
gRPC code.

## Protocol in one picture

```
Remote Node 17                                          Master Node (das-edge)
──────────────                                          ──────────────────────
open stream  POST /das.v1.NodeIngestService/Report   ─►  (one stream per node, HTTP/2)
seq 1  full=true  state={…}  state_hash=H1           ─►  store; hash(state)==H1 → mirror=H1
                                                     ◄─  ack 1  ACTION_OK
(measurements inside deadbands: nothing is sent)
seq 2  base_hash=H1 state_hash=H2                    ─►  mirror==H1? apply paths → hash==H2? → mirror=H2
       changed_paths=["temperature_c","bands.2.dl_out_dbm"]
       state={temperatureC:47.5,bands:[{},{},{dlOutDbm:31.2}]}
                                                     ◄─  ack 2  ACTION_OK
(5 s without a change)
seq 3  base_hash=H2 state_hash=H2  (keep-alive)      ─►  still alive, nothing to publish
                                                     ◄─  ack 3  ACTION_OK
                    … master restarts, mirror lost …
seq 9  base_hash=H7 …                                ─►  unknown base
                                                     ◄─  ack 9  ACTION_RESYNC
seq 10 full=true state={…} state_hash=H8             ─►  consistent again
```

- **Report by exception at the source.** The node applies the same deadbands as the
  master: 0.5 °C, 1 dB DL power, 0.3 dB optical, 10 s boot time and so on. It sends
  nothing while its state stays inside them, apart from a keep-alive every 5 s.
- **Deltas by field path.** `changed_paths` names the fields with protobuf names; list
  elements are addressed by index and map entries by key. A listed field that is missing
  from `state` now has its default value, because proto3 JSON omits zeros. A list whose
  length changes (alarms added or cleared) is sent whole.
- **Checksum.** `state_hash` is FNV-1a 64 over the canonical encoding of the normalised
  state: fixed field order, shortest round-trip decimals. `base_hash` names the state the
  delta applies to. The master applies a delta only if its copy has `base_hash` **and**
  the patched result hashes to `state_hash`. Anything else (lost message, master restart,
  bug, version skew) is answered with `RESYNC`, and the next report is full. Unverified
  data is never published.
- **Flow control.** HTTP/2 applies backpressure per stream. `ACTION_SLOW_DOWN` tells a
  node that reports faster than `-ingest-min-interval` (default 200 ms) to back off; the
  report is still applied.
- **Offline detection.** A node that sends nothing (not even keep-alives) for
  `-stale-after` (15 s) is published as `offline`. It comes back on its next report.

## Measured

`make demo-ingest` (or `das-edge -nodes 0 -ingest …` + `das-nodesim -nodes 300`),
60 seconds, 300 nodes measuring once per second:

| | Every measurement as full JSON | With this protocol |
|---|---:|---:|
| Messages | 17 836 | 3 807 (300 full, 528 deltas, 2 979 keep-alives) |
| Bytes on the fiber | 21.2 MB | **0.88 MB** (24× less; 40× in steady state) |
| Resyncs / hash mismatches | – | 0 / 0 |
| Master CPU | – | 1.4 s in 60 s (≈ 2 % of one core) |

Keep-alives are now the largest share. Raising `-keepalive` to 10 s (with
`-stale-after 30s` on the master) halves them.

The Go tests also cover the edge cases:

- `TestMasterRestartHealsWithResync`: the master loses its copies; the nodes are told
  to resync and end up identical.
- `TestCorruptDeltaIsDetectedByHash`: a delta that claims 99 °C with a stale hash is
  refused.
- `TestDiffPruneApplyReconstructs`: 600 random deltas rebuild the exact state, including
  fields that became zero.

## Wire format

This implementation speaks the **Connect protocol** (streaming, `application/connect+json`)
over **HTTP/2 without TLS** (h2c, prior knowledge). Each message is an envelope: 1 flag
byte, a 4-byte big-endian length, then the protobuf-JSON message. The stream ends with an
end-stream envelope (flag `0x02`) carrying `{}` or `{"error":{"code":…,"message":…}}`.

```json
{"nodeId":17,"seq":"2","stateHash":"1438826130921044123","baseHash":"9922103377120451022",
 "state":{"temperatureC":47.5,"bands":[{},{},{"dlOutDbm":31.2}]},
 "changedPaths":["temperature_c","bands.2.dl_out_dbm"],"reportedAtMs":"1790441227583"}
```

Interoperability is tested, not assumed: `interop/connect-es` generates a client from the
`.proto` files with the official Connect-ES toolchain and streams to the edge:

```sh
cd interop/connect-es && npm install && npm run generate
../../bin/das-edge -nodes 0 -ingest 127.0.0.1:9090 -listen 127.0.0.1:8080 &
node interop.mjs http://127.0.0.1:9090 http://127.0.0.1:8080
# ok  bidirectional stream: OK, RESYNC
# ok  node 901 published: RN-901, 41.5 °C, alarm TEMP_HIGH/minor
```

## gRPC

With access to the Go module proxy, run `buf generate` with `protoc-gen-go` and
`protoc-gen-connect-go` and implement the generated `NodeIngestServiceHandler` with the
logic in `server.go` (`apply`). connect-go then serves **gRPC, gRPC-Web and Connect** on
the same path, with binary protobuf. Remote Node firmware can use grpc-c++, grpc-go,
nanopb+custom framing, or Connect over plain HTTP/2, whichever fits its toolchain. The
contract does not change.

Why not raw gRPC in the PoC: the Go gRPC stack needs third-party modules, and the Connect
protocol is wire-compatible with generated Connect clients while staying debuggable with
curl. For microcontroller-class nodes, MQTT with the same delta/hash payload is a
reasonable alternative (see das-00 `docs/OPTIONS.md`).

## Node-side agent

`internal/ingest.Agent` is the reference behaviour for node firmware. `cmd/das-nodesim`
uses it for 300 simulated nodes.

- `Offer(report)` applies the deadbands and decides between nothing, a keep-alive, a
  delta and a full report.
- `Handle(response)` processes `RESYNC` (next report is full) and `SLOW_DOWN`.
- `Attach(stream)` runs after every (re)connect; the next report is full.
