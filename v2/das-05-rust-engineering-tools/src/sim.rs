//! Hardware simulator: a Remote Node analyzer that answers like the FPGA/DSP
//! would, with raw buffers (`fpga.rs` formats) after a realistic sweep time.
//!
//! Spectrum: EU DAS downlink carriers (LTE 800, GSM 900, LTE 1800, UMTS/LTE 2100,
//! LTE 2600), CW spurs and an intermittent interferer, with noise.
//! Reflection (S11): each node/port has a feeder with connectors, sometimes a
//! damaged connector, and an antenna at the end, with frequency-dependent cable loss.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use rustfft::num_complex::Complex;

use crate::dtf::{DtfParams, C0};
use crate::fpga::{self, S11Header, SpectrumHeader};
use crate::hw::{Analyzer, BoxFut, HwError};
use crate::spectrum::SpectrumParams;

/// Small deterministic PRNG (SplitMix64) with a Gaussian helper.
pub struct Rng(u64);

impl Rng {
    pub fn new(seed: u64) -> Self {
        Rng(seed ^ 0x9E37_79B9_7F4A_7C15)
    }
    pub fn next_u64(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9E37_79B9_7F4A_7C15);
        let mut z = self.0;
        z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
        z ^ (z >> 31)
    }
    /// Uniform in [0, 1).
    pub fn f64(&mut self) -> f64 {
        (self.next_u64() >> 11) as f64 / (1u64 << 53) as f64
    }
    /// Standard normal (Box–Muller).
    pub fn normal(&mut self) -> f64 {
        let u1 = self.f64().max(1e-300);
        let u2 = self.f64();
        (-2.0 * u1.ln()).sqrt() * (2.0 * std::f64::consts::PI * u2).cos()
    }
}

const NOISE_FLOOR: f64 = -100.0;
const CARRIERS: &[(f64, f64, f64)] = &[
    (773e6, 10e6, -62.0),
    (783e6, 10e6, -64.0),
    (796e6, 10e6, -58.0),
    (806e6, 10e6, -60.0),
    (816e6, 10e6, -63.0),
    (935.4e6, 0.2e6, -57.0),
    (936.2e6, 0.2e6, -59.0),
    (937.0e6, 0.2e6, -58.0),
    (947.6e6, 5e6, -61.0),
    (955.0e6, 5e6, -63.0),
    (1815e6, 20e6, -64.0),
    (1840e6, 20e6, -62.0),
    (1867.5e6, 15e6, -66.0),
    (2120e6, 20e6, -63.0),
    (2142.5e6, 15e6, -61.0),
    (2160e6, 10e6, -65.0),
    (2630e6, 20e6, -60.0),
    (2655e6, 20e6, -62.0),
    (2675e6, 20e6, -64.0),
];
const SPURS: &[(f64, f64)] = &[(1001.3e6, -79.0), (1500.0e6, -86.0), (2400.0e6, -81.0)];

fn baseline(p: &SpectrumParams) -> Vec<f64> {
    let step = p.step_hz();
    let mut base = vec![NOISE_FLOOR; p.points];
    for &(center, bw, level) in CARRIERS {
        let half = bw / 2.0;
        let edge = (bw * 0.03).max(2.0 * step);
        let i0 = (((center - half - edge - p.start_hz) / step).floor().max(0.0)) as usize;
        let i1 = (((center + half + edge - p.start_hz) / step).ceil()).min((p.points - 1) as f64);
        if i1 < 0.0 {
            continue;
        }
        for (i, b) in base.iter_mut().enumerate().take(i1 as usize + 1).skip(i0) {
            let d = (p.start_hz + i as f64 * step - center).abs();
            let lvl = if d <= half - edge {
                level
            } else if d <= half + edge {
                let t = (d - (half - edge)) / (2.0 * edge);
                level + (NOISE_FLOOR - level) * (1.0 - (std::f64::consts::PI * t).cos()) / 2.0
            } else {
                continue;
            };
            *b = b.max(lvl);
        }
    }
    for &(f, level) in SPURS {
        let i = ((f - p.start_hz) / step).round();
        if i >= 0.0 && (i as usize) < p.points {
            base[i as usize] = base[i as usize].max(level);
        }
    }
    base
}

/// A reflection on a feeder: one-way distance and reflection magnitude.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Reflection {
    pub distance_m: f64,
    pub rho: f64,
}

/// The simulated feeder of a node/port: port connector, jumper, sometimes a
/// damaged connector, antenna at the end. Deterministic per node/port.
pub fn feeder_for(node_id: u32, port: u16) -> Vec<Reflection> {
    let mut r = Rng::new(((node_id as u64) << 16) ^ port as u64 ^ 0xD7F);
    let antenna_m = 25.0 + 30.0 * r.f64();
    let mut f = vec![Reflection { distance_m: 0.35, rho: 0.03 }, Reflection { distance_m: 1.5 + 1.0 * r.f64(), rho: 0.05 }];
    if r.f64() < 0.5 {
        f.push(Reflection { distance_m: 6.0 + (antenna_m - 10.0) * r.f64(), rho: 0.15 + 0.1 * r.f64() });
    }
    f.push(Reflection { distance_m: antenna_m, rho: 0.06 + 0.06 * r.f64() });
    f
}

/// Reflection coefficient of a feeder across the band of `p` (cable loss grows
/// with √f around its mid-band value), with complex Gaussian noise `sigma`.
pub fn reflection_sweep(p: &DtfParams, feeder: &[Reflection], velocity_factor: f64, loss_db_per_100m: f64, sigma: f64, seed: u64) -> Vec<Complex<f64>> {
    let mut r = Rng::new(seed);
    let v = C0 * velocity_factor;
    let step = p.step_hz();
    let f_mid = (p.start_hz + p.stop_hz) / 2.0;
    (0..p.points)
        .map(|k| {
            let f = p.start_hz + k as f64 * step;
            let alpha = loss_db_per_100m / 100.0 * (f / f_mid).sqrt();
            let mut g = Complex::new(sigma * r.normal(), sigma * r.normal());
            for refl in feeder {
                let tau = 2.0 * refl.distance_m / v;
                let amp = refl.rho * 10f64.powf(-2.0 * refl.distance_m * alpha / 20.0);
                g += Complex::from_polar(amp, -2.0 * std::f64::consts::PI * f * tau);
            }
            g
        })
        .collect()
}

/// One lock per physical analyzer (node, port); the value is its sweep counter.
type AnalyzerLocks = HashMap<(u32, u16), Arc<tokio::sync::Mutex<u32>>>;

struct Template {
    sweeps: Vec<Vec<f32>>,
    next: usize,
}

/// The simulated analyzer. Sweeps on the same node/port are serialised, like a
/// single physical analyzer; each takes `sweep_time`.
pub struct SimAnalyzer {
    pub sweep_time: Duration,
    pub s11_time: Duration,
    templates: Mutex<HashMap<String, Template>>,
    locks: Mutex<AnalyzerLocks>,
}

impl SimAnalyzer {
    pub fn new(sweep_time: Duration, s11_time: Duration) -> Self {
        SimAnalyzer { sweep_time, s11_time, templates: Mutex::new(HashMap::new()), locks: Mutex::new(HashMap::new()) }
    }

    /// Generates the noise templates of an analyzer configuration ahead of time.
    pub fn prewarm(&self, p: &SpectrumParams) {
        let _ = self.next_template(p);
    }

    fn next_template(&self, p: &SpectrumParams) -> Vec<f32> {
        let key = p.key();
        let mut t = self.templates.lock().unwrap();
        if !t.contains_key(&key) {
            let base = baseline(p);
            let mut r = Rng::new(p.node_id as u64 * 31 + p.port as u64);
            let step = p.step_hz();
            let mut sweeps = Vec::with_capacity(4);
            for n in 0..4 {
                let mut out: Vec<f32> = base
                    .iter()
                    .map(|&b| {
                        let sd = if b <= NOISE_FLOOR + 0.5 { 1.6 } else { 0.45 };
                        ((b + r.normal() * sd) * 100.0).round() as f32 / 100.0 // 0.01 dB driver resolution
                    })
                    .collect();
                if n % 3 == 1 {
                    // intermittent interferer at 1890 MHz
                    let lvl = -74.0 + (r.f64() - 0.5) * 6.0;
                    let i0 = ((1890e6 - 0.2e6 - p.start_hz) / step).max(0.0) as usize;
                    let i1 = (((1890e6 + 0.2e6 - p.start_hz) / step).max(0.0) as usize).min(out.len().saturating_sub(1));
                    for v in out.iter_mut().take(i1 + 1).skip(i0) {
                        *v = v.max((lvl + r.normal() * 0.8) as f32);
                    }
                }
                sweeps.push(out);
            }
            if t.len() >= 16 {
                if let Some(k) = t.keys().next().cloned() {
                    t.remove(&k);
                }
            }
            t.insert(key.clone(), Template { sweeps, next: 0 });
        }
        let tpl = t.get_mut(&key).unwrap();
        let s = tpl.sweeps[tpl.next].clone();
        tpl.next = (tpl.next + 1) % tpl.sweeps.len();
        s
    }

    fn analyzer_lock(&self, node: u32, port: u16) -> Arc<tokio::sync::Mutex<u32>> {
        self.locks.lock().unwrap().entry((node, port)).or_insert_with(|| Arc::new(tokio::sync::Mutex::new(0))).clone()
    }
}

impl Analyzer for SimAnalyzer {
    fn sweep<'a>(&'a self, p: &'a SpectrumParams) -> BoxFut<'a, Result<Bytes, HwError>> {
        Box::pin(async move {
            if p.node_id > 4096 {
                return Err(HwError::NoSuchAnalyzer(format!("node {}", p.node_id)));
            }
            let lock = self.analyzer_lock(p.node_id, p.port);
            let mut seq = lock.lock().await; // one physical analyzer per node/port
            tokio::time::sleep(self.sweep_time).await; // the hardware sweeps; no CPU
            *seq += 1;
            let dbm = self.next_template(p);
            let h = SpectrumHeader {
                flags: 0,
                seq: *seq,
                count: dbm.len() as u32,
                start_hz: p.start_hz,
                step_hz: p.step_hz(),
                scale_db: 0.01,
                offset_db: 0.0,
                timestamp_ns: 0,
            };
            Ok(Bytes::from(fpga::encode_spectrum(&h, &dbm)))
        })
    }

    fn s11<'a>(&'a self, p: &'a DtfParams) -> BoxFut<'a, Result<Bytes, HwError>> {
        Box::pin(async move {
            if p.node_id > 4096 {
                return Err(HwError::NoSuchAnalyzer(format!("node {}", p.node_id)));
            }
            let lock = self.analyzer_lock(p.node_id, p.port);
            let mut seq = lock.lock().await;
            tokio::time::sleep(self.s11_time).await;
            *seq += 1;
            let feeder = feeder_for(p.node_id, p.port);
            // The real cable: VF 0.88, 6 dB/100 m at mid-band, whatever the request assumes.
            let gamma = reflection_sweep(p, &feeder, 0.88, 6.0, 0.0005, *seq as u64);
            let h = S11Header { flags: 0, seq: *seq, count: gamma.len() as u32, start_hz: p.start_hz, step_hz: p.step_hz(), timestamp_ns: 0 };
            Ok(Bytes::from(fpga::encode_s11(&h, &gamma)))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn sweeps_parse_back_with_carriers() {
        let sim = SimAnalyzer::new(Duration::from_millis(1), Duration::from_millis(1));
        let p = SpectrumParams { node_id: 1, port: 1, start_hz: 700e6, stop_hz: 2700e6, points: 2001, rbw_hz: 30e3 };
        let raw = sim.sweep(&p).await.unwrap();
        let (h, dbm) = fpga::parse_spectrum(&raw).unwrap();
        assert_eq!(h.count, 2001);
        let max = dbm.iter().cloned().fold(f32::MIN, f32::max);
        assert!(max > -70.0 && max < -50.0, "strongest carrier {max}");
    }
}
