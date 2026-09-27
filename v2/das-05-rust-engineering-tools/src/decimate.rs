//! Positive-peak decimation to the display width: every output point is the
//! maximum of its bucket, so narrow carriers and one-bin spurs survive (like a
//! spectrum analyzer's peak detector). Same algorithm as the Node.js and Go code.

use std::borrow::Cow;

/// Rounds a client's requested point count up to a multiple of 64 in [16, 65536]
/// (0 = no decimation), so the number of cached variants stays small.
pub fn normalise_max_points(n: i64) -> usize {
    if n <= 0 {
        return 0;
    }
    let n = n.clamp(16, 1 << 16) as usize;
    n.div_ceil(64) * 64
}

/// Result of a decimation: points, start frequency, step, decimated?
pub type Decimated<'a> = (Cow<'a, [f32]>, f64, f64, bool);

/// Keeps the maximum of every bucket; output points sit at bucket centres. NaN never wins.
pub fn peak_decimate(p: &[f32], start_hz: f64, step_hz: f64, max_points: usize) -> Decimated<'_> {
    let n = p.len();
    if max_points == 0 || n <= max_points {
        return (Cow::Borrowed(p), start_hz, step_hz, false);
    }
    let bucket = n.div_ceil(max_points);
    let m = n.div_ceil(bucket);
    let mut out = Vec::with_capacity(m);
    for chunk in p.chunks(bucket) {
        let mut best = f32::NEG_INFINITY;
        for &v in chunk {
            if v > best {
                best = v;
            }
        }
        out.push(if best == f32::NEG_INFINITY { f32::NAN } else { best });
    }
    (Cow::Owned(out), start_hz + (bucket - 1) as f64 * step_hz / 2.0, step_hz * bucket as f64, true)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keeps_a_one_bin_spur() {
        let mut p = vec![-100.0f32; 50001];
        p[31337] = -40.0;
        let (out, start, step, dec) = peak_decimate(&p, 700e6, 40000.0, 1600);
        assert!(dec && out.len() <= 1600);
        let peak = out.iter().enumerate().max_by(|a, b| a.1.total_cmp(b.1)).unwrap().0;
        assert_eq!(out[peak], -40.0);
        assert!((start + peak as f64 * step - (700e6 + 31337.0 * 40000.0)).abs() <= step / 2.0 + 1.0);
    }

    #[test]
    fn normalise() {
        assert_eq!(normalise_max_points(1599), 1600);
        assert_eq!(normalise_max_points(0), 0);
        assert_eq!(normalise_max_points(1_000_000_000), 65536);
        assert_eq!(normalise_max_points(3), 64);
    }

    #[test]
    fn nan_only_bucket_stays_nan_and_small_input_is_untouched() {
        let (out, _, _, dec) = peak_decimate(&[f32::NAN, f32::NAN, -1.0, -2.0], 0.0, 1.0, 2);
        assert!(dec && out[0].is_nan() && out[1] == -1.0);
        let (same, _, _, dec) = peak_decimate(&[1.0, 2.0], 0.0, 1.0, 16);
        assert!(!dec && matches!(same, Cow::Borrowed(_)));
    }
}
