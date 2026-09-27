# Benchmarks

Two kinds of measurement:

1. **System**: the programme's standard load against das-engtools deployed next to the
   Node.js app. The question is whether the "server is dead" alarm goes away, and at what
   cost.
2. **Hot paths**: Node.js vs Rust on the same raw FPGA buffer. The question is how much
   CPU the engineering tools cost per sweep.

Everything ran on one machine (Intel Xeon @ 2.80 GHz, 2 vCPUs, Linux 6.18). It was **not** a
Master Unit. The embedded CPU is emulated, as in every project of this programme.

## 1. System: the standard load

`bench/front-under-load.js` (`make bench`), with the load generator `bench/heartbeat-under-load.js`
shared by every project:

- 4 simulated browsers × 2 spectrum traces of 50 001 points each, polling the **legacy**
  `/api/spectrum` back to back. This is what the shipped frontend does.
- Per browser: a heartbeat every 1 000 ms with a 3 000 ms timeout (the UI's alarm), the
  dashboard (`/api/volatile-data`) every 2 s, and a configuration read every 3 s. Each
  browser uses at most 6 connections, like a real browser over HTTP/1.1. Time spent queued
  in the browser counts.
- 30 s measured after 3 s of warm-up.
- **Every server process is pinned to one core (CPU 0)**, together with a 10 % "other
  daemons" CPU burner. `DAS_CPU_SLOWDOWN=8` makes every heavy operation run 8 times, in
  both Node.js and Rust, to approximate a Cortex-A53-class CPU. The load generator runs on
  CPU 1.
- Memory is PSS (proportional set size) from `/proc/<pid>/smaps_rollup`, sampled every
  second.

### Results

The earlier rows are single runs from the earlier projects (`../das-03-lts-go-edge/bench/results/`).
Every das-05 setup was run **three times**, and the table shows all runs.

| setup | heartbeat p50 | p99 | max | timeouts | false alarms | spectrum updates / trace / min | server memory (peak PSS) |
|---|---|---|---|---|---|---|---|
| shipped Node.js app (legacy) | 2 031 ms | 3 000 ms | 3 000 ms | **48 / 125** | **19** | 18.5 | 211 MB |
| Node.js hotfix (das-01) | 1.3 ms | 5.7 ms | 5.7 ms | 0 | 0 | 30 | 239 MB |
| nginx gateway + 3 Node services (das-02) | 1.8 ms | 11.1 ms | 11.1 ms | 0 | 0 | 27 | 286 MB |
| Go edge (das-03) | 0.8 ms | 4.7 ms | 6.1 ms | 0 | 0 | 102 | 43 MB |
| **B. Rust front + shipped Node.js**, run 1 | 1.7 ms | 12.0 ms | 12.3 ms | 0 | 0 | 150 | 17 + 133 MB |
| run 2 | 1.6 ms | 40.4 ms | 40.7 ms | 0 | 0 | 151 | 16 + 128 MB |
| run 3 | 11.0 ms | 58.4 ms | 67.0 ms | 0 | 0 | 147 | 17 + 132 MB |
| **B. Rust front + Node.js hotfix**, run 1 | 1.8 ms | 9.4 ms | 10.3 ms | 0 | 0 | 144 | 15 + 127 MB |
| run 2 | 1.8 ms | 17.9 ms | 34.9 ms | 0 | 0 | 146 | 15 + 124 MB |
| run 3 | 1.9 ms | 14.0 ms | 15.2 ms | 0 | 0 | 147 | 15 + 134 MB |
| **C. shipped Node.js in front, piping to Rust**, run 1 | 1.0 ms | 9.2 ms | 9.4 ms | 0 | 0 | 144 | 14 + 146 MB |
| run 2 | 0.8 ms | 7.8 ms | 7.8 ms | 0 | 0 | 146 | 15 + 145 MB |
| run 3 | 1.0 ms | 10.3 ms | 11.2 ms | 0 | 0 | 147 | 14 + 143 MB |

"17 + 133 MB" is das-engtools + the Node.js app. The HTML report
([bench/results/latest.html](../bench/results/latest.html), screenshot
`docs/screenshots/bench-report.png`) shows the earlier projects plus the median run of each
das-05 setup. The raw JSON of every run is in `bench/results/runs/`.

### Reading the results

- **The alarm is gone with the shipped app unchanged.** In setup B, the Node.js app is the
  release with the bug. It no longer sees spectrum traffic, and 0 of 360 heartbeats timed
  out across the three runs. The worst heartbeat, 67 ms, is 2 % of the alarm timeout.
- **What is left is the shipped app's own work.** The heartbeats over 10 ms come in groups
  of four, every other second. They are the ones that land just behind the four dashboards'
  `/api/volatile-data` requests, which the shipped app serialises synchronously (about
  35–40 ms each at ×8). Whether a run shows them depends on the phase between the two pollers. That
  is why run 3 differs from run 1. Behind the hotfix, which serialises incrementally, the
  p99 drops to 9–18 ms. Fixing the dashboard path is das-01's and das-02's job, not
  das-engtools'.
- **Throughput.** 144–151 updates per trace per minute, compared with 30 for the hotfix
  and 102 for the Go edge, on the same single core. Part of the gain over the Go edge is the
  encoder (section 2). Part is that the Go edge also ran the telemetry simulator and the
  dashboards itself; here, Node.js does that work on the same core. Treat Go and Rust as the
  same class, not as a language verdict.
- **Memory.** das-engtools stays at 14–17 MB under load. That includes 4 concurrent
  50 001-point sessions shared by the 8 traces, with their encoded variants. The Node.js app next to it uses 124–146 MB.
- **Setup C** (Node.js keeps the port and pipes the engineering routes) did as well as B.
  The ×8 emulation does not slow down Node's piping, which flatters C slightly.
  [INTEGRATION.md](INTEGRATION.md) has more.

## 2. Hot paths: Node.js vs Rust

`make dsp-bench` runs `examples/dsp-bench.rs` and `bench/dsp-compare.js` on the **same
raw DSPR buffer**, at native speed (no emulation), on one thread each, and reports the
median of repeated runs. The Node.js side uses the programme's reference modules
(`bench/node-ref`, the code the shipped backend and the hotfix run). It also checks that
both produce **byte-identical legacy JSON** (sha256), and they do, for both sizes.

50 001-point sweep (Node.js 22.22):

| operation | Node.js | Rust | ratio |
|---|---|---|---|
| parse the raw FPGA sweep (97 kB) into calibrated dBm | 0.50 ms | 0.023 ms | 22× |
| legacy JSON, the shipped frontend's shape (2.0 MB) | 20.4 ms | 2.23 ms | 9.1× |
| compact JSON (366 kB) | 10.5 ms | 1.41 ms | 7.5× |
| DSPC binary frame (100 kB) | 0.60 ms | 0.32 ms | 1.9× |
| peak-hold decimation to 1 024 points | 0.15 ms | 0.04 ms | about 4× |
| gzip (fastest level) of the legacy JSON | 8.6 ms | 7.3 ms | 1.2× |
| DTF, 1 024 / 4 096 / 16 001 points | none | 0.38 / 1.27 / 4.7 ms | |

200 001-point sweep: parse 2.0 vs 0.09 ms (22×), legacy JSON 91 vs 9.4 ms (9.7×), compact
JSON 43 vs 5.6 ms (7.7×), DSPC 2.5 vs 1.3 ms (1.9×), decimation 0.47 vs 0.19 ms (2.4×), gzip
36 vs 28 ms (1.3×). The raw numbers are in `bench/results/dsp-compare-*.json`. Differences
below 0.1 ms vary by tens of percent from run to run.

What this says:

- The big gains come from **formatting and parsing numbers**: JSON text and per-element
  loops. Rust writes digits straight into a buffer, while Node.js builds 50 001 objects and
  strings first.
- **Compression** is native code on both sides (zlib vs miniz_oxide), so the language hardly
  matters there. The better lever is sending less: binary frames and decimation (das-v1)
  cut 2 MB to 2–100 kB.
- None of this was the cause of the "server is dead" alarm. That was **where** the work ran
  (the event loop), not how fast. The Node.js hotfix fixed the alarm with worker threads.
  Rust makes each sweep cheaper, so there is more headroom for sweeps, more engineers, and
  a lower CPU temperature, on the same device.

## 3. Binary and footprint

| | das-engtools (Rust) | das-edge (Go, das-03) |
|---|---|---|
| binary, x86-64, stripped | 4.2 MB (gzip 1.8 MB) | 7.8 MB (gzip 3.2 MB) |
| scope | spectrum + DTF + proxy | the whole backend (UI files, telemetry, config, ingest, spectrum) |
| memory under the standard load (PSS) | 14–17 MB | 34–43 MB |
| threads | 3 on one core (main, 1 runtime, 1 DSP) | GOMAXPROCS-driven |

The two binaries do different jobs, so this is context, not a contest. Both fit a Master
Unit comfortably.

## Reproducing

```sh
cargo build --release
node bench/front-under-load.js                         # B, shipped app (compared with das-03's results)
node bench/front-under-load.js --node-mode hotfix      # B, hotfix
node bench/front-under-load.js --topology node-front   # C
make dsp-bench                                         # section 2
```

On the device, use `--cpu-slowdown 1` and point `--base` of `heartbeat-under-load.js` at
the Master Unit. The emulation only matters on a fast PC.
