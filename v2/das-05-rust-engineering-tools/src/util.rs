//! Small helpers: JSON number formatting identical to the Node.js and Go
//! implementations, FNV-1a, clocks.

use std::io::Write;
use std::time::{SystemTime, UNIX_EPOCH};

/// Milliseconds since the Unix epoch.
pub fn now_ms() -> i64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_millis() as i64).unwrap_or(0)
}

/// FNV-1a 32-bit, used for ETag bases (same as the Go implementation).
pub fn fnv1a32(data: &[u8]) -> u32 {
    let mut h: u32 = 0x811c_9dc5;
    for &b in data {
        h ^= b as u32;
        h = h.wrapping_mul(0x0100_0193);
    }
    h
}

/// Appends an integer.
pub fn push_int(out: &mut Vec<u8>, v: impl itoa::Integer) {
    let mut buf = itoa::Buffer::new();
    out.extend_from_slice(buf.format(v).as_bytes());
}

/// JavaScript's `Math.round`: nearest integer, ties towards +infinity. (`f64::round`
/// sends negative ties away from zero: -9537.5 gives -9538, JavaScript -9537.) The
/// shipped Node.js backend is the reference for every number the old frontend sees.
pub fn js_round(x: f64) -> f64 {
    let r = x.round();
    if x - r == 0.5 {
        r + 1.0
    } else {
        r
    }
}

/// Appends `v` rounded to two decimals (`Math.round(v * 100) / 100`), formatted the
/// way JavaScript prints such values (`-95.37`, `-95.3`, `-95`, `0`). NaN and
/// infinities become `null`.
pub fn push_round2(out: &mut Vec<u8>, v: f32) {
    if !v.is_finite() {
        out.extend_from_slice(b"null");
        return;
    }
    let q = js_round(v as f64 * 100.0) as i64;
    if q < 0 {
        out.push(b'-');
    }
    let a = q.unsigned_abs();
    push_int(out, a / 100);
    let frac = a % 100;
    if frac != 0 {
        out.push(b'.');
        out.push(b'0' + (frac / 10) as u8);
        if frac % 10 != 0 {
            out.push(b'0' + (frac % 10) as u8);
        }
    }
}

/// Appends a float in its shortest round-trip positional form (Go `'f', -1`,
/// JavaScript for the magnitudes used here). Non-finite values become `null`.
pub fn push_f64(out: &mut Vec<u8>, v: f64) {
    if !v.is_finite() {
        out.extend_from_slice(b"null");
        return;
    }
    let _ = write!(out, "{}", v);
}

/// Random-enough identifier for this process lifetime (8 hex digits).
pub fn boot_id() -> String {
    use std::collections::hash_map::RandomState;
    use std::hash::{BuildHasher, Hasher};
    let mut h = RandomState::new().build_hasher();
    h.write_u128(SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_nanos()).unwrap_or(0));
    h.write_u32(std::process::id());
    format!("{:08x}", h.finish() as u32)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn r2(v: f32) -> String {
        let mut b = Vec::new();
        push_round2(&mut b, v);
        String::from_utf8(b).unwrap()
    }

    #[test]
    fn round2_matches_js_and_go() {
        assert_eq!(r2(-95.37), "-95.37");
        assert_eq!(r2(-95.3), "-95.3");
        assert_eq!(r2(-95.0), "-95");
        assert_eq!(r2(0.0), "0");
        assert_eq!(r2(-0.5), "-0.5");
        assert_eq!(r2(-0.05), "-0.05");
        assert_eq!(r2(12.345_678), "12.35");
        assert_eq!(r2(f32::NAN), "null");
    }

    #[test]
    fn round2_ties_go_up_like_javascript() {
        // Expected values printed by Node.js: String(Math.round(Math.fround(v) * 100) / 100).
        // Such ties are common with FPGAs that report power in 1/8 or 1/16 dB steps.
        assert_eq!(r2(-95.375), "-95.37");
        assert_eq!(r2(-95.625), "-95.62");
        assert_eq!(r2(95.375), "95.38");
        assert_eq!(r2(-0.125), "-0.12");
        assert_eq!(r2(-100.875), "-100.87");
        assert_eq!(r2(-0.005), "0");
        assert_eq!(js_round(-0.5), 0.0);
        assert_eq!(js_round(2.5), 3.0);
        assert_eq!(js_round(0.49999999999999994), 0.0);
    }

    #[test]
    fn f64_is_positional() {
        let mut b = Vec::new();
        push_f64(&mut b, 700e6);
        b.push(b' ');
        push_f64(&mut b, 700_020_000.5);
        b.push(b' ');
        push_f64(&mut b, 40000.0);
        assert_eq!(String::from_utf8(b).unwrap(), "700000000 700020000.5 40000");
    }

    #[test]
    fn fnv_known_value() {
        assert_eq!(fnv1a32(b""), 0x811c_9dc5);
        assert_eq!(fnv1a32(b"a"), 0xe40c_292c);
    }
}
