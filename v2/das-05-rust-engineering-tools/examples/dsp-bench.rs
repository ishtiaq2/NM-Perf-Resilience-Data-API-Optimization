//! Micro-benchmark of the Master Unit hot paths at native speed (no CPU emulation):
//! raw FPGA buffer -> calibrated dBm -> the response formats, and DTF.
//!
//!   cargo run --release --example dsp-bench
//!   cargo run --release --example dsp-bench -- --points 200001 --json
//!   cargo run --release --example dsp-bench -- --write-raw .run/sweep.dspr --write-legacy .run/legacy-rust.json
//!
//! `bench/dsp-compare.js` runs the same operations in Node.js on the same raw
//! buffer and checks that both produce byte-identical legacy JSON.

use std::collections::HashMap;
use std::io::Write;
use std::sync::Arc;
use std::time::{Duration, Instant};

use das_engtools::dtf::{self, DtfParams, Window};
use das_engtools::hw::Analyzer;
use das_engtools::sim::{self, SimAnalyzer};
use das_engtools::spectrum::{self, SpectrumParams, Sweep};
use das_engtools::{decimate, dspc, fpga};
use flate2::write::GzEncoder;
use flate2::Compression;
use serde_json::json;

/// Median wall time of `f` in milliseconds (at least 15 runs or ~0.4 s).
fn median_ms<T>(mut f: impl FnMut() -> T) -> f64 {
    for _ in 0..3 {
        std::hint::black_box(f());
    }
    let mut xs = Vec::new();
    let t0 = Instant::now();
    while xs.len() < 15 || (t0.elapsed() < Duration::from_millis(400) && xs.len() < 2000) {
        let t = Instant::now();
        std::hint::black_box(f());
        xs.push(t.elapsed().as_secs_f64() * 1e3);
    }
    xs.sort_by(|a, b| a.total_cmp(b));
    xs[xs.len() / 2]
}

fn arg(args: &[String], name: &str) -> Option<String> {
    args.iter().position(|a| a == name).and_then(|i| args.get(i + 1).cloned())
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let points: usize = arg(&args, "--points").and_then(|v| v.parse().ok()).unwrap_or(50_001);
    let as_json = args.iter().any(|a| a == "--json");

    // One realistic sweep from the simulated analyzer, as the FPGA would deliver it.
    let params = SpectrumParams { node_id: 1, port: 1, start_hz: 700e6, stop_hz: 2700e6, points, rbw_hz: 30_000.0 };
    let hw = SimAnalyzer::new(Duration::ZERO, Duration::ZERO);
    let rt = tokio::runtime::Builder::new_current_thread().enable_time().build().expect("runtime");
    let raw = rt.block_on(hw.sweep(&params)).expect("sweep");
    if let Some(path) = arg(&args, "--write-raw") {
        std::fs::write(&path, &raw).expect("write raw");
    }

    let (h, power) = fpga::parse_spectrum(&raw).expect("parse");
    let power: Arc<[f32]> = power.into();
    // Fixed metadata so that other implementations can reproduce the exact bytes.
    let sweep = Sweep::new(1, params.clone(), 1_700_000_000_000, power.clone(), "bench".into());
    let legacy = spectrum::legacy_json(&sweep);
    if let Some(path) = arg(&args, "--write-legacy") {
        std::fs::write(&path, &legacy).expect("write legacy");
    }
    let meta = dspc::FrameMeta { sweep_id: 1, node_id: 1, port: 1, start_hz: h.start_hz, step_hz: h.step_hz, timestamp_ms: 1.7e12, decimated: false };
    let gz = |b: &[u8]| {
        let mut e = GzEncoder::new(Vec::with_capacity(b.len() / 4), Compression::fast());
        e.write_all(b).unwrap();
        e.finish().unwrap()
    };

    let mut rows: Vec<(String, f64, usize)> = Vec::new();
    rows.push((format!("parse raw FPGA sweep ({points} pts, {} kB)", raw.len() / 1024), median_ms(|| fpga::parse_spectrum(&raw).unwrap()), power.len() * 4));
    rows.push(("legacy JSON (shipped frontend shape)".into(), median_ms(|| spectrum::legacy_json(&sweep)), legacy.len()));
    let compact = spectrum::compact_json(&sweep, &power, h.start_hz, h.step_hz, false);
    rows.push((
        "compact JSON (/api/spectrum/latest)".into(),
        median_ms(|| spectrum::compact_json(&sweep, &power, h.start_hz, h.step_hz, false)),
        compact.len(),
    ));
    let frame = dspc::encode(&meta, &power, dspc::ENC_I16);
    rows.push(("DSPC binary frame (i16 centi-dBm)".into(), median_ms(|| dspc::encode(&meta, &power, dspc::ENC_I16)), frame.len()));
    let dec = decimate::peak_decimate(&power, h.start_hz, h.step_hz, 1024);
    rows.push(("peak-hold decimation to 1024 px".into(), median_ms(|| decimate::peak_decimate(&power, h.start_hz, h.step_hz, 1024).0.len()), dec.0.len() * 4));
    let zipped = gz(&legacy);
    rows.push(("gzip (fast) of the legacy JSON".into(), median_ms(|| gz(&legacy)), zipped.len()));

    // Distance-to-fault: reflection sweep -> windowed inverse FFT -> events.
    for n in [1024usize, 4096, 16001] {
        let mut p = DtfParams::parse(&HashMap::new()).expect("defaults");
        p.points = n;
        p.window = Window::Kaiser;
        let gamma = sim::reflection_sweep(&p, &sim::feeder_for(1, 1), 0.88, 6.0, 0.0005, 1);
        let step = p.step_hz();
        let prof = dtf::compute(&p, &gamma, step);
        rows.push((format!("DTF {n} pts (Kaiser, {} events)", prof.events.len()), median_ms(|| dtf::compute(&p, &gamma, step)), prof.return_loss_db.len() * 4));
    }

    if as_json {
        let out: Vec<_> = rows.iter().map(|(n, ms, b)| json!({ "op": n, "ms": (ms * 1000.0).round() / 1000.0, "bytes": b })).collect();
        println!("{}", json!({ "impl": "rust", "points": points, "results": out }));
    } else {
        println!("das-engtools hot paths, native speed, median of repeated runs ({points}-point sweep)");
        for (n, ms, b) in &rows {
            println!("  {n:<46} {ms:>9.3} ms   {:>9} bytes out", b);
        }
    }
}
