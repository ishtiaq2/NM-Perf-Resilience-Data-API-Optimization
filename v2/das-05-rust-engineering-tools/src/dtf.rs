//! Distance-to-Fault (DTF): where along the antenna feeder the reflections are.
//!
//! The Remote Node's analyzer measures the reflection coefficient Γ(f) of the
//! feeder at N frequencies. A reflection at one-way distance d arrives with the
//! round-trip delay τ = 2d / (c·VF), i.e. as a phase ramp e^(−j2πfτ) across the
//! band. An inverse FFT turns the band into a delay (= distance) profile:
//!
//! 1. window Γ (Hann by default) to suppress the side lobes of strong reflections,
//! 2. zero-pad to ≥ 4N points (smooth curve, accurate peak positions),
//! 3. inverse FFT, normalised by the window sum (a single reflection of
//!    magnitude ρ gives a peak of height ρ),
//! 4. bin j is at distance j · c·VF / (2 · M · Δf),
//! 5. compensate the round-trip cable loss (2 · d · α / 100 dB),
//! 6. return loss RL = −20 log10 |Γ(d)|, and events are local maxima; events
//!    with RL below the threshold are faults.
//!
//! Resolution is c·VF / (2 · span); the unambiguous range is c·VF / (2 · Δf).

use std::cell::RefCell;
use std::collections::HashMap;
use std::sync::Arc;

use bytes::Bytes;
use rustfft::num_complex::Complex;
use rustfft::FftPlanner;
use tokio::sync::OnceCell;

use crate::dsp::{Busy, DspPool};
use crate::fpga;
use crate::hw::{Analyzer, BoxFut};
use crate::session::Measure;
use crate::util::{now_ms, push_f64, push_int, push_round2};

/// Speed of light in vacuum (m/s).
pub const C0: f64 = 299_792_458.0;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Window {
    Rect,
    Hann,
    /// Kaiser with β = 6: lower side lobes, wider main lobe.
    Kaiser,
}

impl Window {
    pub fn name(self) -> &'static str {
        match self {
            Window::Rect => "rect",
            Window::Hann => "hann",
            Window::Kaiser => "kaiser",
        }
    }

    /// Peaks this far below a stronger neighbour (within a few resolution cells)
    /// are treated as the window's side lobes, not as reflections. (Peak side
    /// lobes: rectangular -13 dB, Hann -31 dB, Kaiser β = 6 about -44 dB.)
    fn side_lobe_db(self) -> f64 {
        match self {
            Window::Rect => 13.0,
            Window::Hann => 28.0,
            Window::Kaiser => 40.0,
        }
    }

    /// Leakage envelope in dB below a peak, `cells` resolution cells away: the
    /// main lobe within one cell, the first side lobes up to six cells, then a
    /// conservative 6 dB per octave roll-off (rectangular and Kaiser windows roll
    /// off at about that rate, Hann at 18 dB per octave).
    pub fn leakage_db(self, cells: f64) -> f64 {
        if cells <= 1.0 {
            0.0
        } else if cells <= 6.0 {
            self.side_lobe_db()
        } else {
            self.side_lobe_db() + 6.02 * (cells / 6.0).log2()
        }
    }

    fn coefficients(self, n: usize) -> Vec<f64> {
        let last = (n.max(2) - 1) as f64;
        (0..n)
            .map(|i| match self {
                Window::Rect => 1.0,
                Window::Hann => 0.5 - 0.5 * (2.0 * std::f64::consts::PI * i as f64 / last).cos(),
                Window::Kaiser => {
                    let r = 2.0 * i as f64 / last - 1.0;
                    bessel_i0(6.0 * (1.0 - r * r).max(0.0).sqrt()) / bessel_i0(6.0)
                }
            })
            .collect()
    }
}

fn bessel_i0(x: f64) -> f64 {
    let (mut sum, mut term) = (1.0, 1.0);
    for k in 1..50 {
        term *= (x / (2.0 * k as f64)).powi(2);
        sum += term;
        if term < 1e-12 * sum {
            break;
        }
    }
    sum
}

/// One DTF configuration (one measurement session).
#[derive(Debug, Clone, PartialEq)]
pub struct DtfParams {
    pub node_id: u32,
    pub port: u16,
    pub start_hz: f64,
    pub stop_hz: f64,
    /// Frequency points of the reflection sweep.
    pub points: usize,
    /// Velocity factor of the feeder cable (0.5–1.0; foam-dielectric coax ≈ 0.88).
    pub velocity_factor: f64,
    /// Cable loss at mid-band, dB per 100 m (compensated along the distance axis).
    pub cable_loss_db_per_100m: f64,
    pub window: Window,
    /// Shorter display range than the unambiguous range, in metres (0 = full range).
    pub max_distance_m: f64,
    /// Events with a return loss below this are faults (dB).
    pub threshold_db: f64,
}

fn get_f64(q: &HashMap<String, String>, k: &str) -> Option<f64> {
    q.get(k).and_then(|v| v.parse::<f64>().ok()).filter(|v| v.is_finite())
}

impl DtfParams {
    pub fn key(&self) -> String {
        format!(
            "{}:{}:{}:{}:{}:{}:{}:{}:{}:{}",
            self.node_id,
            self.port,
            self.start_hz,
            self.stop_hz,
            self.points,
            self.velocity_factor,
            self.cable_loss_db_per_100m,
            self.window.name(),
            self.max_distance_m,
            self.threshold_db
        )
    }

    pub fn step_hz(&self) -> f64 {
        (self.stop_hz - self.start_hz) / (self.points - 1) as f64
    }

    /// Validates query values; missing ones take the defaults.
    pub fn parse(q: &HashMap<String, String>) -> Result<Self, String> {
        let mut p = DtfParams {
            node_id: 1,
            port: 1,
            start_hz: 700e6,
            stop_hz: 2700e6,
            points: 1024,
            velocity_factor: 0.88,
            cable_loss_db_per_100m: 6.0,
            window: Window::Hann,
            max_distance_m: 0.0,
            threshold_db: 20.0,
        };
        if let Some(v) = q.get("nodeId").and_then(|v| v.parse::<u32>().ok()).filter(|&v| v > 0) {
            p.node_id = v;
        }
        if let Some(v) = q.get("port").and_then(|v| v.parse::<u16>().ok()).filter(|&v| v > 0) {
            p.port = v;
        }
        if let Some(v) = get_f64(q, "startHz").filter(|v| *v > 0.0) {
            p.start_hz = v;
        }
        if let Some(v) = get_f64(q, "stopHz").filter(|v| *v > 0.0) {
            p.stop_hz = v;
        }
        if let Some(v) = q.get("points").and_then(|v| v.parse::<usize>().ok()) {
            p.points = v.clamp(64, 16_001);
        }
        if let Some(v) = get_f64(q, "velocityFactor") {
            if !(0.5..=1.0).contains(&v) {
                return Err("velocityFactor must be in [0.5, 1.0]".into());
            }
            p.velocity_factor = v;
        }
        if let Some(v) = get_f64(q, "cableLossDbPer100m") {
            if !(0.0..=50.0).contains(&v) {
                return Err("cableLossDbPer100m must be in [0, 50]".into());
            }
            p.cable_loss_db_per_100m = v;
        }
        if let Some(w) = q.get("window") {
            p.window = match w.as_str() {
                "rect" => Window::Rect,
                "hann" => Window::Hann,
                "kaiser" => Window::Kaiser,
                _ => return Err("window must be rect, hann or kaiser".into()),
            };
        }
        if let Some(v) = get_f64(q, "maxDistanceM").filter(|v| *v >= 0.0) {
            p.max_distance_m = v;
        }
        if let Some(v) = get_f64(q, "thresholdDb").filter(|v| (0.0..=60.0).contains(v)) {
            p.threshold_db = v;
        }
        if p.stop_hz <= p.start_hz {
            return Err("stopHz must be greater than startHz".into());
        }
        if !q.contains_key("points") && p.max_distance_m > 0.0 {
            // Enough points for the requested distance plus 25 %: reflections beyond
            // the unambiguous range (points - 1) x resolution alias into it.
            let resolution = C0 * p.velocity_factor / (2.0 * (p.stop_hz - p.start_hz));
            p.points = ((1.25 * p.max_distance_m / resolution).ceil() as usize + 1).clamp(64, 16_001);
        }
        Ok(p)
    }
}

/// A reflection event along the feeder.
#[derive(Debug, Clone, PartialEq)]
pub struct Event {
    pub distance_m: f64,
    pub return_loss_db: f64,
    pub vswr: f64,
    pub fault: bool,
}

/// The processed distance profile.
#[derive(Debug, Clone)]
pub struct Profile {
    /// Return loss per distance bin (dB), value j at `start_m + j · bin_m`. Long
    /// ranges are peak-hold decimated to at most `MAX_PROFILE_POINTS` values (the
    /// worst return loss of each group of bins is kept, like a spectrum trace).
    pub return_loss_db: Vec<f32>,
    pub start_m: f64,
    pub bin_m: f64,
    /// Two-point resolution of the measurement (c · VF / 2B).
    pub resolution_m: f64,
    pub max_range_m: f64,
    /// Measured noise floor as a return loss at 0 m (dB). With loss compensation the
    /// displayed floor rises by 2 · loss dB per metre of distance.
    pub noise_floor_rl_db: f64,
    /// Events sorted by distance: the `MAX_EVENTS` strongest if there were more.
    pub events: Vec<Event>,
    pub events_truncated: bool,
}

thread_local! {
    // FFT plans are expensive to build and reusable: one planner per DSP thread.
    static PLANNER: RefCell<FftPlanner<f64>> = RefCell::new(FftPlanner::new());
}

/// Events weaker than this are not reported at all (dB return loss).
const EVENT_FLOOR_DB: f64 = 35.0;
/// Peaks must also stand this far above the measured noise floor (dB). Noise
/// magnitudes are Rayleigh distributed: P(|x| > 5.6 × median) ≈ 2^-31 per cell.
/// This matters at long range, where loss compensation lifts the noise (a
/// 1 km range at 6 dB/100 m applies 120 dB of round-trip gain at the far end).
const NOISE_MARGIN_DB: f64 = 15.0;
/// Local noise estimate (OS-CFAR, as in radar detection): the median of the
/// cells from 3 to 32 resolution cells either side of a peak. Near strong
/// reflections the floor is their leakage rather than the receiver noise.
const CFAR_GUARD_CELLS: f64 = 3.0;
const CFAR_REF_CELLS: f64 = 32.0;
/// At most this many events are reported (the strongest ones).
pub const MAX_EVENTS: usize = 64;
/// The return-loss trace is peak-hold decimated to at most this many values.
pub const MAX_PROFILE_POINTS: usize = 4096;

/// Computes the distance profile of `gamma` measured with frequency step `step_hz`.
pub fn compute(p: &DtfParams, gamma: &[Complex<f64>], step_hz: f64) -> Profile {
    let n = gamma.len().max(2);
    let m = (n * 4).next_power_of_two();
    let w = p.window.coefficients(gamma.len());
    let w_sum: f64 = w.iter().sum::<f64>().max(f64::MIN_POSITIVE);
    let mut buf = vec![Complex::new(0.0, 0.0); m];
    for (k, g) in gamma.iter().enumerate() {
        buf[k] = g * w[k];
    }
    PLANNER.with(|pl| pl.borrow_mut().plan_fft_inverse(m).process(&mut buf));

    let v = C0 * p.velocity_factor;
    let max_range_m = v / (2.0 * step_hz);
    let bin_m = max_range_m / m as f64;
    let resolution_m = v / (2.0 * step_hz * (n - 1) as f64);
    let shown = if p.max_distance_m > 0.0 { p.max_distance_m.min(max_range_m) } else { max_range_m };
    let bins = ((shown / bin_m).floor() as usize + 1).min(m);
    let alpha = p.cable_loss_db_per_100m / 100.0;

    // Uncompensated magnitude over the whole unambiguous range: its median is the
    // noise floor (reflections occupy only a few cells).
    let raw: Vec<f64> = buf.iter().map(|c| c.norm() / w_sum).collect();
    let noise = {
        let mut v = raw.clone();
        let mid = v.len() / 2;
        *v.select_nth_unstable_by(mid, |a, b| a.total_cmp(b)).1
    };
    let noise_threshold = noise * 10f64.powf(NOISE_MARGIN_DB / 20.0);
    // Round-trip cable loss compensation along the distance axis.
    let mag: Vec<f64> = (0..bins).map(|j| raw[j] * 10f64.powf(2.0 * j as f64 * bin_m * alpha / 20.0)).collect();
    let rl = |g: f64| (-20.0 * g.max(1e-6).log10()).max(0.0);

    // Events. The inverse FFT is circular: the leakage of a reflection close to the
    // port (negative distances) wraps to the far end of the range, where the loss
    // compensation multiplies it. So peaks are found and compared on the measured
    // (uncompensated) magnitudes over the whole circular range, and a peak counts
    // only if it is (1) a local maximum within ± one resolution cell, (2) clearly
    // above the noise, (3) not explained by the window leakage of a stronger peak
    // anywhere, (4) clearly above the local floor (noise plus leakage), and (5)
    // within the shown range with a return loss below the event floor.
    let guard = ((resolution_m / bin_m).ceil() as usize).max(1);
    let cell_bins = resolution_m / bin_m;
    let circ = |a: usize, b: usize| {
        let d = a.abs_diff(b);
        d.min(m - d)
    };
    let at = |j: isize| raw[j.rem_euclid(m as isize) as usize];
    let mut peaks: Vec<usize> = (0..m)
        .filter(|&j| {
            let g = raw[j];
            g >= noise_threshold
                && (1..=guard as isize).all(|d| {
                    let (l, r) = (at(j as isize - d), at(j as isize + d));
                    l < g && r <= g // ties go to the first bin
                })
        })
        .collect();
    peaks.sort_by(|&a, &b| raw[b].total_cmp(&raw[a])); // strongest first
    peaks.truncate(4 * MAX_EVENTS);
    let floor = 10f64.powf(-EVENT_FLOOR_DB / 20.0);
    let margin = 10f64.powf(NOISE_MARGIN_DB / 20.0);
    let (cfar_g, cfar_r) = ((CFAR_GUARD_CELLS * cell_bins).ceil() as isize, (CFAR_REF_CELLS * cell_bins).ceil() as isize);
    let local_floor = |j: usize| {
        let mut v: Vec<f64> = (cfar_g..=cfar_r).flat_map(|d| [at(j as isize - d), at(j as isize + d)]).collect();
        let mid = v.len() / 2;
        *v.select_nth_unstable_by(mid, |a, b| a.total_cmp(b)).1
    };
    let mut events = Vec::new();
    for (idx, &j) in peaks.iter().enumerate() {
        if j >= bins || mag[j] < floor || raw[j] < local_floor(j) * margin {
            continue;
        }
        let leaked = peaks[..idx].iter().any(|&k| {
            let cells = circ(j, k) as f64 / cell_bins;
            raw[j] < raw[k] * 10f64.powf(-p.window.leakage_db(cells) / 20.0)
        });
        if leaked {
            continue;
        }
        // Parabolic interpolation of the peak position between bins.
        let (a, b, c) = (at(j as isize - 1), raw[j], at(j as isize + 1));
        let den = a - 2.0 * b + c;
        let delta = if den.abs() > 1e-300 { (0.5 * (a - c) / den).clamp(-0.5, 0.5) } else { 0.0 };
        let g = mag[j];
        let gamma_abs = g.min(0.999_999);
        let return_loss_db = rl(g);
        events.push(Event {
            distance_m: ((j as f64 + delta) * bin_m).max(0.0),
            return_loss_db,
            vswr: ((1.0 + gamma_abs) / (1.0 - gamma_abs)).min(99.99),
            fault: return_loss_db < p.threshold_db,
        });
    }
    events.sort_by(|a, b| a.distance_m.total_cmp(&b.distance_m));
    let events_truncated = events.len() > MAX_EVENTS;
    if events_truncated {
        events.sort_by(|a, b| a.return_loss_db.total_cmp(&b.return_loss_db)); // strongest first
        events.truncate(MAX_EVENTS);
        events.sort_by(|a, b| a.distance_m.total_cmp(&b.distance_m));
    }

    // Display trace: peak-hold (worst return loss) decimation of long ranges.
    let bucket = bins.div_ceil(MAX_PROFILE_POINTS).max(1);
    let return_loss_db = mag.chunks(bucket).map(|c| rl(c.iter().fold(0.0, |a: f64, &b| a.max(b))) as f32).collect();
    Profile {
        return_loss_db,
        start_m: (bucket - 1) as f64 * bin_m / 2.0,
        bin_m: bin_m * bucket as f64,
        resolution_m,
        max_range_m,
        noise_floor_rl_db: rl(noise),
        events,
        events_truncated,
    }
}

/// A completed DTF measurement.
pub struct DtfResult {
    pub id: u32,
    pub params: DtfParams,
    pub timestamp: i64,
    pub profile: Profile,
    pub etag: String,
    json: OnceCell<Bytes>,
}

impl DtfResult {
    /// The JSON document, encoded once (on the DSP pool) and shared.
    pub async fn json(self: &Arc<Self>, dsp: &DspPool) -> Result<Bytes, Busy> {
        let body = self
            .json
            .get_or_try_init(|| async {
                let r = self.clone();
                dsp.run(move || Bytes::from(result_json(&r))).await
            })
            .await?;
        Ok(body.clone())
    }
}

fn round3(v: f64) -> f64 {
    (v * 1000.0).round() / 1000.0
}

/// `{"measurementId",…,"returnLossDb":[…],"events":[…]}`.
pub fn result_json(r: &DtfResult) -> Vec<u8> {
    let p = &r.params;
    let pr = &r.profile;
    let mut b = Vec::with_capacity(512 + pr.return_loss_db.len() * 6);
    b.extend_from_slice(b"{\"measurementId\":");
    push_int(&mut b, r.id);
    b.extend_from_slice(b",\"nodeId\":");
    push_int(&mut b, p.node_id);
    b.extend_from_slice(b",\"port\":");
    push_int(&mut b, p.port);
    b.extend_from_slice(b",\"timestamp\":");
    push_int(&mut b, r.timestamp);
    b.extend_from_slice(b",\"startHz\":");
    push_f64(&mut b, p.start_hz);
    b.extend_from_slice(b",\"stopHz\":");
    push_f64(&mut b, p.stop_hz);
    b.extend_from_slice(b",\"points\":");
    push_int(&mut b, p.points);
    b.extend_from_slice(b",\"velocityFactor\":");
    push_f64(&mut b, p.velocity_factor);
    b.extend_from_slice(b",\"cableLossDbPer100m\":");
    push_f64(&mut b, p.cable_loss_db_per_100m);
    b.extend_from_slice(b",\"window\":\"");
    b.extend_from_slice(p.window.name().as_bytes());
    b.extend_from_slice(b"\",\"thresholdDb\":");
    push_f64(&mut b, p.threshold_db);
    b.extend_from_slice(b",\"maxDistanceM\":");
    push_f64(&mut b, p.max_distance_m);
    b.extend_from_slice(b",\"resolutionM\":");
    push_f64(&mut b, round3(pr.resolution_m));
    b.extend_from_slice(b",\"maxRangeM\":");
    push_f64(&mut b, round3(pr.max_range_m));
    b.extend_from_slice(b",\"noiseFloorRlDb\":");
    push_round2(&mut b, pr.noise_floor_rl_db as f32);
    b.extend_from_slice(b",\"startM\":");
    push_f64(&mut b, (pr.start_m * 1e5).round() / 1e5);
    b.extend_from_slice(b",\"binM\":");
    push_f64(&mut b, (pr.bin_m * 1e5).round() / 1e5);
    b.extend_from_slice(b",\"count\":");
    push_int(&mut b, pr.return_loss_db.len());
    b.extend_from_slice(b",\"returnLossDb\":[");
    for (i, &v) in pr.return_loss_db.iter().enumerate() {
        if i > 0 {
            b.push(b',');
        }
        push_round2(&mut b, v);
    }
    b.extend_from_slice(b"],\"events\":[");
    for (i, e) in pr.events.iter().enumerate() {
        if i > 0 {
            b.push(b',');
        }
        b.extend_from_slice(b"{\"distanceM\":");
        push_f64(&mut b, round3(e.distance_m));
        b.extend_from_slice(b",\"returnLossDb\":");
        push_round2(&mut b, e.return_loss_db as f32);
        b.extend_from_slice(b",\"vswr\":");
        push_round2(&mut b, e.vswr as f32);
        b.extend_from_slice(if e.fault { b",\"fault\":true}" } else { b",\"fault\":false}" });
    }
    b.extend_from_slice(if pr.events_truncated { b"],\"eventsTruncated\":true}" } else { b"],\"eventsTruncated\":false}" });
    b
}

/// Reflection sweeps from the hardware, turned into distance profiles on the DSP pool.
pub struct DtfMeasure {
    pub hw: Arc<dyn Analyzer>,
    pub dsp: DspPool,
}

impl Measure for DtfMeasure {
    type Params = DtfParams;
    type Output = DtfResult;

    fn key(p: &DtfParams) -> String {
        p.key()
    }

    fn measure<'a>(&'a self, p: &'a DtfParams, id: u32, tag: String) -> BoxFut<'a, Result<DtfResult, String>> {
        Box::pin(async move {
            let raw = self.hw.s11(p).await.map_err(|e| e.to_string())?;
            let params = p.clone();
            let profile = self
                .dsp
                .run(move || -> Result<Profile, String> {
                    let (h, gamma) = fpga::parse_s11(&raw).map_err(|e| e.to_string())?;
                    if gamma.len() < 2 {
                        return Err("reflection sweep needs at least 2 points".into());
                    }
                    Ok(compute(&params, &gamma, h.step_hz))
                })
                .await
                .map_err(|e| e.to_string())??;
            Ok(DtfResult { id, params: p.clone(), timestamp: now_ms(), profile, etag: format!("\"{tag}\""), json: OnceCell::new() })
        })
    }

    fn id(o: &DtfResult) -> u32 {
        o.id
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sim::{feeder_for, reflection_sweep, Reflection};

    fn params() -> DtfParams {
        DtfParams::parse(&HashMap::new()).unwrap()
    }

    #[test]
    fn finds_reflections_at_the_right_distance_and_level() {
        let p = params();
        let feeder = vec![
            Reflection { distance_m: 1.8, rho: 0.05 },  // jumper connector, RL 26 dB
            Reflection { distance_m: 17.3, rho: 0.2 },  // damaged connector, RL 14 dB
            Reflection { distance_m: 42.0, rho: 0.08 }, // antenna, RL 21.9 dB
        ];
        let gamma = reflection_sweep(&p, &feeder, 0.88, p.cable_loss_db_per_100m, 0.0005, 7);
        let prof = compute(&p, &gamma, p.step_hz());
        assert!((prof.resolution_m - C0 * 0.88 / (2.0 * 2e9)).abs() < 1e-6);
        for r in &feeder {
            let want_rl = -20.0 * r.rho.log10();
            let e = prof
                .events
                .iter()
                .find(|e| (e.distance_m - r.distance_m).abs() <= prof.resolution_m)
                .unwrap_or_else(|| panic!("no event near {} m: {:?}", r.distance_m, prof.events));
            assert!((e.return_loss_db - want_rl).abs() < 1.0, "RL {} vs {} at {} m", e.return_loss_db, want_rl, r.distance_m);
        }
        let faults: Vec<_> = prof.events.iter().filter(|e| e.fault).collect();
        assert_eq!(faults.len(), 1, "only the damaged connector is below 20 dB: {faults:?}");
        assert!((faults[0].distance_m - 17.3).abs() < 0.05);
    }

    #[test]
    fn window_side_lobes_are_not_reported_as_events() {
        let p = params();
        let gamma = reflection_sweep(&p, &[Reflection { distance_m: 30.0, rho: 0.9 }], 0.88, 0.0, 0.0, 1);
        let mut p0 = p.clone();
        p0.cable_loss_db_per_100m = 0.0;
        let prof = compute(&p0, &gamma, p.step_hz());
        assert_eq!(prof.events.len(), 1, "{:?}", prof.events);
        assert!(prof.events[0].vswr > 10.0);
    }

    #[test]
    fn long_range_noise_is_not_reported_as_events() {
        // 16 001 points over 2 GHz: 1 055 m unambiguous range. At the far end the loss
        // compensation applies 2 x 10.55 x 6 dB = 127 dB of gain to the noise.
        let mut p = params();
        p.points = 16_001;
        p.window = Window::Kaiser;
        let feeder = vec![Reflection { distance_m: 2.0, rho: 0.05 }, Reflection { distance_m: 23.5, rho: 0.2 }, Reflection { distance_m: 48.0, rho: 0.09 }];
        let gamma = reflection_sweep(&p, &feeder, 0.88, p.cable_loss_db_per_100m, 0.0005, 3);
        let prof = compute(&p, &gamma, p.step_hz());
        assert!(prof.max_range_m > 1000.0);
        assert!(!prof.events_truncated);
        assert_eq!(prof.events.len(), feeder.len(), "noise reported as events: {:?}", prof.events);
        for (e, r) in prof.events.iter().zip(&feeder) {
            assert!((e.distance_m - r.distance_m).abs() <= 2.0 * prof.resolution_m, "{e:?} vs {r:?}");
            assert!((e.return_loss_db + 20.0 * r.rho.log10()).abs() < 1.0, "{e:?} vs {r:?}");
        }
        assert!(prof.noise_floor_rl_db > 60.0, "noise floor {} dB", prof.noise_floor_rl_db);
        // The trace is peak-hold decimated for display: 65 536 bins -> 4 096 values.
        assert_eq!(prof.return_loss_db.len(), MAX_PROFILE_POINTS);
        assert!((prof.bin_m * prof.return_loss_db.len() as f64 - prof.max_range_m).abs() < prof.bin_m);
        let worst = prof.return_loss_db.iter().take((60.0 / prof.bin_m) as usize).fold(f32::MAX, |a, &b| a.min(b));
        assert!((worst - 13.98).abs() < 1.0, "the damaged connector must survive decimation: {worst}");
    }

    #[test]
    fn events_are_capped() {
        let p = params();
        // 100 small reflections every 0.6 m: more than MAX_EVENTS.
        let feeder: Vec<_> = (0..100).map(|i| Reflection { distance_m: 1.0 + i as f64 * 0.6, rho: 0.03 + 0.001 * i as f64 }).collect();
        let mut p0 = p.clone();
        p0.cable_loss_db_per_100m = 0.0;
        let gamma = reflection_sweep(&p0, &feeder, 0.88, 0.0, 0.0, 1);
        let prof = compute(&p0, &gamma, p0.step_hz());
        assert!(prof.events_truncated);
        assert_eq!(prof.events.len(), MAX_EVENTS);
        assert!(prof.events.windows(2).all(|w| w[0].distance_m < w[1].distance_m), "sorted by distance");
    }

    #[test]
    fn simulated_feeders_are_deterministic_per_port() {
        assert_eq!(feeder_for(3, 1), feeder_for(3, 1));
        assert_ne!(feeder_for(3, 1), feeder_for(3, 2));
    }

    #[test]
    fn params_validation() {
        let mut q = HashMap::new();
        q.insert("velocityFactor".to_string(), "1.5".to_string());
        assert!(DtfParams::parse(&q).is_err());
        let mut q = HashMap::new();
        q.insert("window".to_string(), "kaiser".to_string());
        q.insert("points".to_string(), "10".to_string());
        let p = DtfParams::parse(&q).unwrap();
        assert_eq!((p.window, p.points), (Window::Kaiser, 64));
        // Without `points`, maxDistanceM picks enough points for the feeder (+25 %).
        let mut q = HashMap::new();
        q.insert("maxDistanceM".to_string(), "60".to_string());
        let p = DtfParams::parse(&q).unwrap();
        let range = (p.points - 1) as f64 * C0 * p.velocity_factor / (2.0 * (p.stop_hz - p.start_hz));
        assert_eq!(p.points, 1139);
        assert!((75.0..75.2).contains(&range), "{range}");
    }
}
