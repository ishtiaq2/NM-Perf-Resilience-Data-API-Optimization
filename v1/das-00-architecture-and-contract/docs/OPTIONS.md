# Options analysis

Every option considered, including the ones we did not choose, and why. The scores are
engineering judgement, backed by measurements from the PoC projects where available.
Revisit them with numbers from a real Master Node.

## Criteria

| Criterion | Weight | Why |
|---|---:|---|
| Removes the root cause (CPU work on the path of the heartbeat) | must | Otherwise the alarm returns under a different load |
| Compatible: one origin, shipped UI keeps working | must | Customers run the shipped frontend; no CORS/second port |
| Embedded footprint (RAM, CPU, flash, dependencies) | 3 | The Master Node's RF/alarm daemons need their share |
| Delivery risk and reversibility | 3 | Field devices; a bad update is expensive |
| Fit with team skills (Angular/TypeScript/Node) | 2 | Speed now, hiring later |
| Long-term maintainability (security updates, LTS toolchain) | 2 | 10+ year product life |
| Scales with nodes × engineers × data size | 2 | Hundreds of nodes, several engineers, 200k-point sweeps |

## 1. Fix it inside Node.js

| Option | Verdict | Notes |
|---|---|---|
| **Worker threads + shared sweeps + incremental serialisation** (01) | **Chosen for the hotfix** | Moves CPU work off the event loop, does it once per change instead of per request, and runs the workers at nice 10. Measured: 19 → 0 false alarms, heartbeat p99 5.7 ms. It is still one process, so a blocking bug elsewhere still hurts (the detector logs it). |
| `cluster` / several Node processes behind one port | No | Every process still blocks on its own requests. `volatile_data` would have to be shared between processes (IPC or Redis), and the spectrum work would be duplicated. |
| Streaming JSON (`JSONStream`, `stream-json`) | Partial | Smaller blocks, but the same total CPU on the one thread, and it is still per request. |
| Faster frameworks (Fastify, uWebSockets.js, hyper-express) | No (alone) | They speed up routing and I/O, not `JSON.stringify` of 50 000 points. uWS.js is a good WebSocket engine if the team stays on Node (02). |
| Bun / Deno | No | Same single-threaded model. Bun has no 32-bit ARM build, and both are new runtimes to qualify on an embedded Linux. |
| Bigger device CPU | No | It moves the cliff, not the design (see ROOT-CAUSE: the shipped code runs near the cliff already). |

## 2. The front door: web servers for embedded Linux

The front door is the process that owns the port the browser talks to (TLS, static
files, routing). It does not run application logic.

| Server | RAM (measured / typical) | HTTP/2 | WebSocket proxy | Cache | In Yocto / Buildroot | Verdict |
|---|---|---|---|---|---|---|
| **nginx** | ~9 MB PSS (measured, master + worker + cache mgr) | ✔ | ✔ | ✔ micro-cache | ✔ / ✔ | **Chosen for 02** |
| lighttpd | ~2–4 MB | ✔ (1.4.56+) | limited | ✗ | ✔ / ✔ | Smallest; weaker proxying and no cache |
| HAProxy | ~10 MB | ✔ | ✔ | small | ✔ / ✔ | Excellent proxy, no static files |
| H2O | ~5–10 MB | ✔ (+HTTP/3) | ✔ | ✔ | not checked | Good, smaller community |
| Caddy / Traefik | 30–60 MB (Go, large binaries) | ✔ | ✔ | plugin | – | Built for containers and automatic ACME, not for a SoC |
| Envoy | 50+ MB | ✔ | ✔ | ✔ | – | Too heavy for this device |
| Mongoose / CivetWeb (C, embeddable) | < 1 MB | ✗ / partial | ✔ | ✗ | ✔ | For C firmware, not a web stack |
| **das-edge itself (Go, 03)** | 20–27 MB idle, 43 MB PSS under load | ✔ (ALPN) | ✔ (is the app) | in-process | static binary | **LTS: the app is the front door**; nginx optional |

## 3. Language for the data plane (03)

| | Go | Rust | C++ (Drogon/Beast) | Java/Kotlin | .NET (AOT) | Elixir/Erlang | Node.js |
|---|---|---|---|---|---|---|---|
| Blocking can starve the heartbeat? | No (preemptive scheduler, reniced threads) | No (tokio + spawn_blocking) | No (thread pools) | No | No | No (per-process scheduling) | **Yes** (one loop per process) |
| Memory for this workload | **~43 MB PSS** (measured) | ~10–20 MB | ~10–20 MB | 100+ MB | 40–80 MB | 40–60 MB | 239 MB (measured) |
| Deployment | **one static binary**, cross-compiled, no libc | static binary | libs, toolchain per target | JVM | runtime or AOT | BEAM runtime | Node + node_modules |
| Build for ARMv7/ARM64 | `GOARCH=arm GOARM=7` (seconds) | cross toolchain | Yocto SDK | JVM on target | AOT per RID | Yocto layer | available |
| Learning curve for a TS team | **low** | high (ownership, async) | high (memory safety) | medium | medium | medium-high | none |
| HTTP/2, TLS, WebSocket | stdlib (WS: small lib) | hyper/axum, tungstenite | Beast/Drogon | Netty/Jetty | Kestrel | Cowboy | ws, http2 |
| gRPC / Connect | connect-go, grpc-go | tonic | grpc++ | grpc-java | grpc-dotnet | grpc | connect-node |
| Memory safety | GC | compile-time | manual | GC | GC | GC | GC |
| Verdict | **Chosen** | Strong alternative if the team already knows Rust: smaller footprint, slower delivery | Only if the product team already maintains C++ services on the SoC | Too heavy | Possible; smaller embedded ecosystem | Great concurrency model, niche skills | Keep for the UI toolchain and as the legacy backend during migration |

Why Go over Rust here: both remove the root cause. Go gets there with a much shorter
learning curve for a TypeScript team. Its standard library covers HTTP/2, TLS, gzip and
JSON with no dependencies, and cross-compiling is a two-variable environment change. Its
memory (43 MB measured) is well within budget. If a later phase has to fit a much
smaller device, Rust is the step down, and the contract (00) keeps that option open.

## 4. Topology

| Option | Isolation | Moving parts | RAM | Verdict |
|---|---|---|---|---|
| One Node process (shipped, 01) | none | 1 | 211–239 MB | Hotfix only |
| nginx + separate Node services (02) | per process + cgroups | 4 + sockets | 286 MB | Good mid-term if staying on Node |
| Dedicated spectrum sidecar only (Node or Go) behind nginx | spectrum isolated | 2–3 | medium | Possible first step; 03's `-delegate` achieves the same with fewer parts |
| **One Go edge + legacy Node behind it (03)** | work isolation via priority threads, limits, conflation | 2 during migration, 1 after | 43 MB + legacy until retired | **LTS** |
| Many microservices (per API) | high | many | high | Wrong trade-off for one SoC |

## 5. Browser ↔ Master Node transport

| Transport | Push | Binary | Through proxies | Browser support | Notes | Verdict |
|---|---|---|---|---|---|---|
| HTTP polling (improved: ETag/304, `?since=` deltas, long-poll, gzip) | pseudo | ✔ | ✔ | all | Stays as a fallback and for commands | **Keep (fallback)** |
| **WebSocket** | ✔ | ✔ | mostly | all | Subscribe, resume from a revision, binary frames, permessage-deflate | **Chosen for push** |
| Server-Sent Events | ✔ | ✗ | ✔ | all | Text only; 6-connection limit on HTTP/1.1 | Good for text-only feeds |
| WebTransport (HTTP/3) | ✔ | ✔ | often blocked (UDP) | Chromium, Firefox; not Safari | Future option for spectrum streaming | Later |
| gRPC-Web | server streaming only | ✔ | needs a proxy/translation | all | Adds Envoy or connect-web; no bidirectional streaming | No for the browser |
| MQTT over WebSocket (broker) | ✔ | ✔ | mostly | via JS client | Natural if nodes also use MQTT; second protocol and ACL model | Alternative |

## 6. Remote Node → Master Node

| Option | Delta + integrity | Flow control | Footprint on node | Verdict |
|---|---|---|---|---|
| Current full-state reports | ✗ | ✗ | – | Replace |
| **gRPC/Connect bidirectional stream with deltas + state hash** (03) | ✔ (hash per state, RESYNC) | HTTP/2 per stream, SLOW_DOWN | needs an HTTP/2 client (grpc-c++, Go, or Connect over HTTP/2) | **Chosen** (measured 24× fewer bytes) |
| MQTT 3.1.1/5 (Mosquitto on master), retained topics per node | ✔ with the same payload design | QoS, broker limits | very small clients (Paho, even on MCUs) | **Good alternative**, especially for MCU-class nodes |
| NATS / NATS JetStream | ✔ (subjects per node) | ✔ | small Go/C clients | Good if a message bus is wanted anyway |
| CoAP / LwM2M | ✔ (observe) | ✔ (UDP) | tiny | For constrained devices; less tooling for this data size |
| OPC UA PubSub | ✔ (deadbands are native) | ✔ | heavy stacks | Telecom/industrial fit, but heavy |
| SNMP polling/traps | ✗ | ✗ | small | Keep for NMS integration, not as the data path |
| DDS | ✔ | ✔ | heavy | Overkill |

The payload design (normalise, deadband, canonical hash, deltas by path, RESYNC) is
independent of the transport, so the same design works over MQTT if that becomes the
choice.

## 7. Payload formats

| Data | Format | Why |
|---|---|---|
| Spectrum sweep to browser | **DSPC binary frame**: 48-byte header + int16 centi-dBm (or float32), peak-decimated to screen width | 2 bytes/point instead of ~40; a 1 600 px screen never needs 50 000 points; no parse cost in the browser |
| Telemetry to browser | JSON snapshot + JSON deltas, gzip/deflate | Debuggable; deltas keep it small; compressed once and shared |
| Node → master | protobuf (JSON codec in the PoC; binary with generated code) | Typed contract, versioned with `buf breaking` |
| Legacy endpoints | unchanged JSON | Shipped UI |

CBOR, MessagePack and FlatBuffers were considered. They add a decoder without improving
on the binary frame (for spectrum) or on gzip-ed JSON deltas (for telemetry).

## 8. Change detection ("checksum, push only if changed")

| Approach | Problem | Verdict |
|---|---|---|
| Timestamps / sequence numbers | Change on every report | ✗ (these are exactly the fields to drop) |
| Hash of the raw report | Changes on every report (noise, counters) | ✗ |
| **Field hygiene + deadbands + canonical hash** | – | **Chosen**: 300 reports/s become ~11 real changes/s (03 measurement) |
| Deltas by field path + base/state hash | – | **Chosen** for node → master |
| Revisions + deltas + ETags | – | **Chosen** for master → browser |
| CRDTs | Solve multi-writer merge, which does not exist here (each node owns its state) | Overkill |

## Decision matrix (1 = poor, 5 = excellent)

| Solution | Root cause | Compat. | Footprint (×3) | Risk (×3) | Skills (×2) | Maint. (×2) | Scale (×2) | Weighted |
|---|---|---|---:|---:|---:|---:|---:|---:|
| 01 Hotfix (Node workers) | ✔ | ✔ | 2 | **5** | **5** | 2 | 2 | 39 |
| 02 nginx + Node services | ✔ | ✔ | 1 | 3 | 4 | 3 | 3 | 32 |
| **03 Go edge (strangler)** | ✔ | ✔ | **5** | 4 | 3 | **5** | **5** | **53** |
| Rust edge (same design) | ✔ | ✔ | 5 | 3 | 1 | 4 | 5 | 44 |
| C++ edge (same design) | ✔ | ✔ | 5 | 2 | 1 | 2 | 5 | 37 |
| Node `cluster` | ✗ | ✔ | – | – | – | – | – | excluded |
| Faster Node framework only | ✗ | ✔ | – | – | – | – | – | excluded |

Weighted score = 3 × footprint + 3 × risk + 2 × skills + 2 × maintainability +
2 × scale.

Maximum 60. 03 scores highest (53), then a Rust edge (44), then 01 (39).

01 scores well because it is the right tool for **now**: lowest risk, no new skills. It
does not score well as the destination. 03 is the destination; 02 is the fallback if Go
is not an option. 02 scores lowest mainly on footprint and moving parts, but it keeps
the team in Node.
