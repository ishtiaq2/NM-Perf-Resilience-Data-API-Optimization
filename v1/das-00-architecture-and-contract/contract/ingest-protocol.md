# Remote Node ingest protocol (`das.v1.NodeIngestService`)

How a Remote Node reports its telemetry to the Master Node: only when it changed, as a
delta that the master can verify with a hash. Reference implementation:
`das-03-lts-go-edge/internal/ingest` (server and node agent). A Connect-ES client
generated from these `.proto` files is tested against it (`das-03/interop/connect-es`).

## Transport

- One long-lived bidirectional stream per node: `rpc Report(stream ReportRequest)
  returns (stream ReportResponse)`, over HTTP/2.
- The reference implementation speaks the **Connect** protocol with the JSON codec
  (`application/connect+json`) over h2c on the fiber management network. Each message is
  an envelope: flags (1 byte), length (4 bytes, big-endian), then the protobuf-JSON
  message. The stream ends with flag `0x02` and `{}` or `{"error":{"code","message"}}`.
- A connect-go server generated from the same `.proto` also accepts **gRPC** and
  **gRPC-Web** with binary protobuf on the same path, so node firmware can use whatever
  its toolchain supports.
- Production: TLS with a client certificate per Remote Node (mTLS). The messages do not
  change.

## Canonical state and hash

The hash is what makes "push only when it changed" safe.

1. **Normalise.** Drop per-report counters (sequence number, report time, uptime; the
   boot time replaces uptime). Apply the deadbands: a value is published only when it
   moves by more than its deadband from the last published value.

   | Field | Deadband |
   |---|---:|
   | temperature | 0.5 °C |
   | fan | 250 rpm |
   | PSU | 0.25 V |
   | optical Rx/Tx | 0.3 dB |
   | laser bias | 1 mA |
   | DL out | 1 dB |
   | UL in | 3 dB |
   | VSWR | 0.05 |
   | generic metrics | 2 |
   | boot time | 10 s |

2. **Encode canonically.** JSON with a fixed field order, as `das-v1` publishes a node:

   `id, name, type, chain, hop, status, fw, bootAt, temperatureC, fanRpm, psuVoltageV,
   optical{rxDbm, txDbm, laserBiasMa}, bands[{name, enabled, dlOutDbm, ulInDbm,
   dlGainDb, ulGainDb, vswr}], metrics{sorted by name}, alarms[{code, severity, since}]`

   Numbers use the shortest decimal that round-trips a float64. Strings use RFC 8259
   escaping. NaN and ±Inf become `null`.
3. **Hash.** `state_hash` = FNV-1a 64 of those bytes. FNV-1a is in every standard library
   and cheap on small CPUs. The hash guards against divergence, not against attackers;
   mTLS protects the channel.

A node computes the hash over the state **as the master will reconstruct it**
(`ToState(FromState(x))` in the reference implementation), so both ends hash the same
bytes.

## Messages

| Report | Fields | Master behaviour |
|---|---|---|
| Full | `full=true`, `state` = complete state, `state_hash` | Accepted whatever the sequence number (the node may have rebooted). The master stores its own copy and hash. A different hash is counted as `hashMismatches` (version skew) and logged. |
| Delta | `base_hash` = state the delta applies to, `changed_paths`, `state` = only those fields, `state_hash` = hash after applying | Applied only if the master's copy has `base_hash` **and** the result hashes to `state_hash`. Otherwise `ACTION_RESYNC`, and nothing is applied. |
| Keep-alive | `full=false`, no `changed_paths`, `base_hash = state_hash` = current hash | Marks the node alive. `RESYNC` if the master does not have that state. |

`changed_paths` uses protobuf field names:

- `temperature_c`
- `optical.rx_dbm`
- `bands.2.dl_out_dbm` (an index into a repeated field)
- `metrics.m03` (a key of a map field)
- `alarms` (a whole list; used whenever a list changes length)

A listed field that is absent from `state` now has its default value, because proto3
JSON omits zeros.

Responses carry `acked_seq` and an action:

- `ACTION_OK`
- `ACTION_RESYNC`: the next report must be full.
- `ACTION_SLOW_DOWN` with `min_interval_ms`: the node reported faster than the master
  wants; the report was still applied.

## Liveness

A node sends a keep-alive when nothing changed for 5 s. The master publishes a node as
`offline` after 15 s without any report, and brings it back on the next one. After a
reconnect the first report is full.

## Measured (das-03, 300 simulated nodes, 60 s)

| | Every measurement as full JSON | This protocol |
|---|---:|---:|
| Bytes | 21.2 MB | 0.88 MB (24× less, 40× in steady state) |
| Resyncs / hash mismatches | – | 0 / 0 |
