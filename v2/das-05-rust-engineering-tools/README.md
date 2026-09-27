# das-05 · Rust engineering tools for the DAS Master Unit

`das-engtools` is a small Rust service that takes the two heavy engineering tools off the
Node.js backend on the head-end Master Unit:

- the **Spectrum Analyzer**: raw FPGA/DSP sweeps of up to 200 001 bins, served as the shipped
  frontend's JSON, compact JSON, the binary DSPC frame, peak-hold decimated traces and a
  WebSocket push;
- **Distance-to-Fault (DTF)**: reflection (S11) sweeps of an antenna feeder, turned into a
  distance profile with located faults (connector, jumper, damaged cable, antenna).

It runs **alongside** the existing Node.js application, which keeps what it is good at (UI
files, login, configuration forms, `volatile_data`). The browser still talks to one origin,
and the API is the programme's das-v1 contract, so the Angular frontend needs no change for
the spectrum analyzer. It only needs a new screen for DTF, which it discovers through
`/api/capabilities`.

This is the "safest path" from the architecture notes: a multi-service gateway in which a
tiny compiled microservice handles the engineering tools and the heavy traffic is routed to
it. The Go side of the split (the NOC fleet controller) is `das-06`.

## Results (measured in this repository)

| | |
|---|---|
| **The alarm problem** | Standard load (4 engineers × 2 traces of 50 001 points, one core shared by all server processes, embedded CPU emulated ×8): the shipped app alone lets **48 of 125 heartbeats time out** (19 false "server is dead" alarms). With `das-engtools` next to the **unchanged** shipped app there were **0 timeouts and 0 false alarms in all 6 runs**. In front of the app (setup B), the heartbeat p99 was 12–58 ms; with the app in front, piping to das-engtools (setup C), it was 8–10 ms. The alarm timeout is 3 000 ms. |
| **Spectrum throughput** | 144–151 updates per minute per trace, compared with 18.5 for the shipped app, 30 for the Node hotfix and 102 for the Go edge (das-03). This is on the same single core, with the Node.js app sharing it. |
| **Memory** | 14–17 MB PSS under that load. The binary is 4.2 MB (x86-64 glibc build, stripped, 1.8 MB gzipped); the ARM targets are musl, i.e. fully static (not built here, see below). No garbage collector and no JIT. |
| **Hot paths vs Node.js** | On the same raw FPGA buffer, Rust parses it about 20× faster, builds the legacy JSON 9× faster and the compact JSON 7.5× faster. gzip is only 1.2× faster, because both sides use native deflate. The legacy JSON is **byte-identical** to what the Node.js backend produces ([BENCHMARKS](docs/BENCHMARKS.md)). |
| **DTF correctness** | 2 511 simulated reflections × 5 noise levels, 3 windows and 3 sweep sizes: **0 missed, 0 phantom faults**, worst return-loss error 0.79 dB (`make dtf-validate`). |
| **Contract** | das-v1 conformance: **21/21** inside the das-02 nginx gateway, which includes the new DTF checks. In front of the shipped app, and with the shipped app in front: 15 passed, 0 failed, 6 skipped. The skips are telemetry features the shipped app itself lacks. In front of the hotfix: 18 passed, 0 failed, 3 skipped. |
| **Tests** | 29 unit tests and 7 end-to-end tests (real sockets, simulator hardware). `clippy -D warnings` and `rustfmt` are clean. |

Every number comes from scripts in this repository ([docs/BENCHMARKS.md](docs/BENCHMARKS.md)).
**Not verified here:** ARM builds and runs, and the real FPGA driver. The build environment
could not download Rust's ARM standard library; [docs/BUILD.md](docs/BUILD.md) has the
commands. The simulator implements the same `Analyzer` trait a real driver adapter would
([docs/FPGA-INTERFACE.md](docs/FPGA-INTERFACE.md)).

## Quick start

```sh
cargo build --release
./target/release/das-engtools --listen 127.0.0.1:8090 --log-format text   # simulated analyzers
curl 'http://127.0.0.1:8090/api/dtf?nodeId=3&port=1&maxDistanceM=60'
curl -o /dev/null -w '%{size_download} bytes\n' 'http://127.0.0.1:8090/api/spectrum?nodeId=1&port=1'
```

| | |
|---|---|
| `make test` / `make lint` | unit and end-to-end tests / `rustfmt` + `clippy -D warnings` |
| `make front` | in front of the unchanged shipped Node.js app (`../das-01-hotfix-node --legacy`), then conformance (`NODE_MODE=hotfix` for the hotfix) |
| `make drop-in` | inside the das-02 nginx gateway, replacing the Node.js spectrum service, then conformance through nginx |
| `make node-front` | the Node.js app keeps the port and pipes the engineering routes to das-engtools, then conformance |
| `make bench` | the standard load, compared with the earlier projects ([report](bench/results/latest.html)) |
| `make dsp-bench` | Node.js vs Rust on the hot paths, plus the byte-identity check |
| `make dtf-validate` | DTF detection against known feeders |
| `make cross` | static ARM binaries (needs the ARM targets, see [BUILD](docs/BUILD.md)) |

The scripts expect the sibling projects (`../das-01-hotfix-node`, `../das-02-gateway-multiservice`)
and Node.js 22+ for the conformance runner and load generator.

## How it fits on the Master Unit

**A. In the das-02 gateway (recommended).** nginx keeps TLS, HTTP/2 and the UI files.
`das-engtools` takes over the spectrum-service socket, and nginx gets two more locations
(`/api/dtf`, `/api/capabilities`).

```
browser ──HTTPS──▶ nginx ─┬─ /api/spectrum*, /api/ws/spectrum, /api/dtf, /api/capabilities ─▶ das-engtools (Rust)  unix:/run/das/spectrum.sock
                          ├─ /api/volatile-data, /api/ws/telemetry ────────────────────────▶ telemetry-service (Node)
                          └─ everything else (login, config, UI API) ───────────────────────▶ core-api (Node)
```

**B. Front mode (no gateway on the device).** `das-engtools` owns the HTTP port and forwards
everything it does not implement to the unchanged Node.js app. WebSocket upgrades are
forwarded too. The Node app moves to `127.0.0.1:8081`, which is one line in its service file.

```
browser ──HTTP──▶ das-engtools :80 ─┬─ spectrum, DTF (served here)
                                    └─ everything else ──▶ Node.js app 127.0.0.1:8081 (unchanged)
```

**C. The Node.js app stays in front** (it may terminate HTTPS itself). It mounts one small
module that pipes the engineering routes to das-engtools on a Unix socket, bytes only.

[docs/INTEGRATION.md](docs/INTEGRATION.md) covers all three setups: install and rollback,
the systemd and nginx files, the measurements for each, and the frontend side.

## API (das-v1)

| Endpoint | |
|---|---|
| `GET /api/spectrum` | The shipped frontend's shape (`points: [{frequency, power}]`). It waits for the next sweep as the shipped app did, and it is gzip-aware. |
| `GET /api/spectrum/latest` | Compact JSON or a binary DSPC frame (`Accept`), with `maxPoints` peak-hold decimation, an ETag/304, and `waitMs` long-poll. |
| `GET /api/ws/spectrum` | WebSocket: one binary frame per sweep, decimated per client. Frames are conflated for slow clients, and the Origin is checked. |
| `GET /api/dtf` | Distance-to-fault profile and events, with an ETag/304 and `waitMs` long-poll ([DTF.md](docs/DTF.md)). |
| `GET /api/capabilities` | The whole origin's features: the Node app's features merged with the engineering tools. |
| `GET /api/engineering/status` | Sessions, DSP pool and process statistics, for an engineering status page. |
| `GET /internal/health`, `/internal/metrics[?format=prom]` | For the gateway's health probe and for Prometheus. nginx never exposes them. |

Measurements are **single-flight**: ten engineers watching the same trace cost one hardware
sweep, not ten. Every response format is encoded once per sweep and then shared.

## Repository layout

```
src/
  main.rs            runtime, listeners (TCP / Unix), systemd notify + watchdog, graceful stop
  config.rs          flags; every flag has a DAS_* environment variable
  app.rs             wiring: DSP pool, session managers, legacy app
  hw.rs              the Analyzer trait: the seam to the real FPGA driver
  fpga.rs            raw FPGA/DSP buffer formats (spectrum DSPR, reflection DS11)
  dsp.rs             DSP thread pool: nice +10, bounded queue, 503 when full
  session.rs         single-flight measurement sessions (watch channel = automatic conflation)
  spectrum.rs        sweep variants: legacy JSON, compact JSON, DSPC, gzip; encoded once, shared
  dtf.rs             distance-to-fault: window, IFFT, loss compensation, CFAR event detection
  decimate.rs dspc.rs util.rs metrics.rs sysd.rs sim.rs
  http/              routes, content negotiation, WebSocket, legacy proxy (TCP/Unix, upgrades)
tests/api.rs         end-to-end tests over real sockets
examples/            dsp-bench (hot paths), dtf-validate (detection vs known feeders),
                     node-in-front/ (the Node.js module for setup C, and a demo on the shipped app)
bench/               load driver, Node.js-vs-Rust comparison, results (HTML report)
conformance/run.js   das-v1 conformance suite (synced from das-00)
scripts/             front-mode.sh (B), drop-in-das02.sh (A), node-in-front.sh (C)
deploy/              systemd unit + env, das.target drop-in, nginx locations, install script
docs/                ARCHITECTURE, DTF, FPGA-INTERFACE, INTEGRATION, BENCHMARKS, BUILD, WHY-RUST
```

## Documents

- [ARCHITECTURE](docs/ARCHITECTURE.md): why the heartbeat can no longer stall, the thread
  model, sessions, caching, and failure behaviour.
- [DTF](docs/DTF.md): the distance-to-fault method, its parameters, limits and validation.
- [FPGA-INTERFACE](docs/FPGA-INTERFACE.md): raw buffer formats, and how to plug in the real
  driver.
- [INTEGRATION](docs/INTEGRATION.md): gateway, front mode, Node-in-front, rollout and
  rollback, and the frontend.
- [BENCHMARKS](docs/BENCHMARKS.md): method, all runs, the Node.js-vs-Rust hot paths, and caveats.
- [BUILD](docs/BUILD.md): toolchain, static ARM builds, sizes and CI.
- [WHY-RUST](docs/WHY-RUST.md): which claims in the Rust-vs-Go notes hold, which are
  overstated, and when Go would do as well.
