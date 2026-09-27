# The FPGA/DSP interface

`das-engtools` talks to the analyzer hardware through a single trait (`src/hw.rs`):

```rust
pub trait Analyzer: Send + Sync + 'static {
    /// One spectrum sweep: the raw buffer exactly as the FPGA/DSP produced it.
    fn sweep<'a>(&'a self, p: &'a SpectrumParams) -> BoxFut<'a, Result<Bytes, HwError>>;
    /// One reflection (S11) sweep of the feeder, for distance-to-fault.
    fn s11<'a>(&'a self, p: &'a DtfParams) -> BoxFut<'a, Result<Bytes, HwError>>;
}
```

Everything above the trait (sessions, parsing, DSP, formats, HTTP) is hardware-independent
and tested. `sim::SimAnalyzer` implements the trait for development, tests and demos. On the
Master Unit, one adapter implements it on top of the real driver. **That adapter is the only
hardware-specific code, and it does not exist yet**, because this repository was built
without access to the driver or its documentation.

## Raw formats (stand-ins)

The layouts below are the ones the simulator produces and `fpga.rs` parses. They stand in for
the real driver's documented formats. If the real formats differ, only
`fpga::parse_spectrum` and `fpga::parse_s11` change, plus their tests.

**Spectrum sweep `DSPR` v1**: a 48-byte header, then one i16 log-power code per bin, all
little-endian.

| offset | type | field |
|---|---|---|
| 0 | `[u8; 4]` | magic `DSPR` |
| 4 | u16 | version (1) |
| 6 | u16 | flags (bit 0: ADC overload) |
| 8 | u32 | hardware sweep sequence |
| 12 | u32 | number of bins *n* |
| 16 | f64 | first bin frequency (Hz) |
| 24 | f64 | bin spacing (Hz) |
| 32 | f32 | calibration scale (dB per code) |
| 36 | f32 | calibration offset (dB) |
| 40 | u64 | hardware timestamp (ns) |
| 48 | i16 × *n* | codes: dBm = code × scale + offset; −32768 = no data |

A 50 001-bin sweep is 100 050 bytes, compared with about 2 MB as the legacy JSON. Parsing it
into calibrated f32 dBm takes 0.02 ms on x86-64: one pass, no allocation except the output.
Values are computed in f32, the way a DSP would, and they are what every response format is
built from.

**Reflection sweep `DS11` v1**: a 40-byte header, then complex Γ as Q15 I/Q pairs.

| offset | type | field |
|---|---|---|
| 0 | `[u8; 4]` | magic `DS11` |
| 4 | u16 | version (1) |
| 6 | u16 | flags |
| 8 | u32 | hardware sequence |
| 12 | u32 | number of frequency points *n* |
| 16 | f64 | first frequency (Hz) |
| 24 | f64 | frequency step (Hz) |
| 32 | u64 | hardware timestamp (ns) |
| 40 | (i16, i16) × *n* | Γ = (I + jQ) / 32768 |

Q15 limits |Γ| to below 1 with a step of 1/32768 (−90 dB). After the inverse FFT, the
quantisation noise floor lies another 10·log10(N) dB lower, which is far below any fault
that matters. **The S11 must already be calibrated at the port** (open/short/load
correction) by the FPGA or the driver. [DTF.md](DTF.md) explains why.

Checks, with an error returned rather than a guess:

- the magic, the version, and the buffer length (header + *n* × element size);
- for spectrum sweeps, whether the bin count and the frequency grid match the request.
  Every response format derives its frequency axis from the request.

## Writing the real adapter

The adapter's job: command the Remote Node's analyzer (through the management channel, as
the Node.js backend does today) and hand back the buffer, **without doing any CPU-heavy work
on the async runtime**. Parsing happens later, on the DSP pool.

```rust
pub struct DriverAnalyzer {
    dev: tokio::io::unix::AsyncFd<std::fs::File>,   // e.g. /dev/das-analyzer or a UIO device
    locks: Mutex<HashMap<(u32, u16), Arc<tokio::sync::Mutex<()>>>>, // one sweep per physical analyzer
}

impl Analyzer for DriverAnalyzer {
    fn sweep<'a>(&'a self, p: &'a SpectrumParams) -> BoxFut<'a, Result<Bytes, HwError>> {
        Box::pin(async move {
            let _one_at_a_time = self.lock(p.node_id, p.port).lock().await;
            self.command_sweep(p)?;                        // ioctl / write: start, stop, points, RBW
            let buf = tokio::time::timeout(Duration::from_secs(5), self.read_result())
                .await
                .map_err(|_| HwError::Timeout)??;          // DMA-complete interrupt, then one copy out
            Ok(Bytes::from(buf))
        })
    }
    // s11(): the same, with the reflection-measurement command.
}
```

Then pass it in `main.rs`: `App::new(cfg, Some(Arc::new(DriverAnalyzer::open("/dev/…")?)))`.
With `None`, the simulator is used.

Common driver shapes, and how to wait for them without blocking the runtime:

| The driver offers | Wait with |
|---|---|
| A character device whose `read()` blocks until the DMA completes | `tokio::io::unix::AsyncFd` if it supports `poll()`. Otherwise `tokio::task::spawn_blocking` (one blocking thread per in-flight sweep, which is fine at these rates). |
| UIO: interrupt count via `read()` on `/dev/uioN`, buffer via `mmap` | `AsyncFd` on the UIO fd. Copy the buffer out of the mapped region (≈100 KB, microseconds) before re-arming. |
| An existing C daemon on a Unix socket (what the Node.js backend calls today) | `tokio::net::UnixStream`. If it prints JSON rather than binary, parse that in `fpga.rs` instead. serde_json with a streaming visitor does 2 MB in a few ms on a DSP thread. It is slower than binary, but needs no driver change for a first rollout. |

Map failures to `HwError`:

- `Timeout` covers a busy analyzer or a fiber link problem.
- `NoSuchAnalyzer` covers an unknown node or port. The session reports it, and the request
  gets `503` after the sweep wait.
- `Io` covers everything else.

The session keeps retrying (every 500 ms) only while someone is watching, so a dead analyzer
costs nothing when nobody looks at it.

**Serialise per physical analyzer.** Two sessions with different settings on the same node
and port must not interleave commands. The simulator uses one `tokio::sync::Mutex` per
(node, port); the adapter should do the same.

**systemd sandbox.** The unit ships with `PrivateDevices=yes`. For a real device node,
replace it with `DevicePolicy=closed` and `DeviceAllow=/dev/<node> rw` (see
`deploy/systemd/das-engtools.service`).

## Next steps that need the hardware team

1. Share the real sweep and S11 buffer formats, or the driver API. Adapt `fpga.rs` and its
   tests; the fixtures become real captures.
2. Write the adapter and run the conformance suite with `--skip-slo` against real Remote Nodes.
3. Report the `flags` (ADC overload) in the API. The parser already reads them, and the UI
   should warn "input overload, add attenuation".
4. Measure a feeder with known discontinuities (see [DTF.md](DTF.md), Validation) before
   trusting fault distances.
