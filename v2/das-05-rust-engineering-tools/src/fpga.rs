//! Raw buffers produced by the analyzer FPGA/DSP (what the driver hands over
//! after DMA), parsed without any intermediate format.
//!
//! Spectrum sweep (`DSPR`, 48-byte header + one i16 log-power code per bin):
//!
//! | offset | type | field |
//! |---|---|---|
//! | 0 | [u8; 4] | magic `DSPR` |
//! | 4 | u16 | version (1) |
//! | 6 | u16 | flags (bit 0: ADC overload) |
//! | 8 | u32 | hardware sweep sequence |
//! | 12 | u32 | number of bins |
//! | 16 | f64 | first bin frequency (Hz) |
//! | 24 | f64 | bin spacing (Hz) |
//! | 32 | f32 | calibration scale (dB per code) |
//! | 36 | f32 | calibration offset (dB) |
//! | 40 | u64 | hardware timestamp (ns) |
//! | 48 | i16 × n | log-power codes; dBm = code × scale + offset; -32768 = no data |
//!
//! Reflection sweep for Distance-to-Fault (`DS11`, 40-byte header + complex S11):
//!
//! | offset | type | field |
//! |---|---|---|
//! | 0 | [u8; 4] | magic `DS11` |
//! | 4 | u16 | version (1) |
//! | 6 | u16 | flags |
//! | 8 | u32 | hardware sequence |
//! | 12 | u32 | number of frequency points |
//! | 16 | f64 | first frequency (Hz) |
//! | 24 | f64 | frequency step (Hz) |
//! | 32 | u64 | hardware timestamp (ns) |
//! | 40 | (i16, i16) × n | reflection coefficient I/Q, Q15 fixed point |
//!
//! All little-endian. The layouts stand in for the real driver's documented
//! formats; only these two functions change when they differ.

use rustfft::num_complex::Complex;

pub const SPECTRUM_MAGIC: [u8; 4] = *b"DSPR";
pub const S11_MAGIC: [u8; 4] = *b"DS11";
pub const SPECTRUM_HEADER: usize = 48;
pub const S11_HEADER: usize = 40;
pub const NO_DATA: i16 = i16::MIN;
const Q15: f64 = 32768.0;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RawError {
    Short,
    Magic,
    Version(u16),
    Length { want: usize, got: usize },
}

impl std::fmt::Display for RawError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RawError::Short => f.write_str("raw buffer shorter than its header"),
            RawError::Magic => f.write_str("raw buffer has the wrong magic"),
            RawError::Version(v) => write!(f, "unsupported raw buffer version {v}"),
            RawError::Length { want, got } => write!(f, "raw buffer length {got}, header says {want}"),
        }
    }
}

impl std::error::Error for RawError {}

/// Header of a raw spectrum sweep.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct SpectrumHeader {
    pub flags: u16,
    pub seq: u32,
    pub count: u32,
    pub start_hz: f64,
    pub step_hz: f64,
    pub scale_db: f32,
    pub offset_db: f32,
    pub timestamp_ns: u64,
}

/// Header of a raw reflection (S11) sweep.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct S11Header {
    pub flags: u16,
    pub seq: u32,
    pub count: u32,
    pub start_hz: f64,
    pub step_hz: f64,
    pub timestamp_ns: u64,
}

fn u16_at(b: &[u8], o: usize) -> u16 {
    u16::from_le_bytes([b[o], b[o + 1]])
}
fn u32_at(b: &[u8], o: usize) -> u32 {
    u32::from_le_bytes(b[o..o + 4].try_into().unwrap())
}
fn u64_at(b: &[u8], o: usize) -> u64 {
    u64::from_le_bytes(b[o..o + 8].try_into().unwrap())
}
fn f32_at(b: &[u8], o: usize) -> f32 {
    f32::from_le_bytes(b[o..o + 4].try_into().unwrap())
}
fn f64_at(b: &[u8], o: usize) -> f64 {
    f64::from_le_bytes(b[o..o + 8].try_into().unwrap())
}

fn check(raw: &[u8], magic: [u8; 4], header: usize, per: usize) -> Result<usize, RawError> {
    if raw.len() < header {
        return Err(RawError::Short);
    }
    if raw[0..4] != magic {
        return Err(RawError::Magic);
    }
    let v = u16_at(raw, 4);
    if v != 1 {
        return Err(RawError::Version(v));
    }
    let n = u32_at(raw, 12) as usize;
    let want = header + n * per;
    if raw.len() != want {
        return Err(RawError::Length { want, got: raw.len() });
    }
    Ok(n)
}

/// Parses a raw spectrum sweep into calibrated dBm values.
pub fn parse_spectrum(raw: &[u8]) -> Result<(SpectrumHeader, Vec<f32>), RawError> {
    let n = check(raw, SPECTRUM_MAGIC, SPECTRUM_HEADER, 2)?;
    let h = SpectrumHeader {
        flags: u16_at(raw, 6),
        seq: u32_at(raw, 8),
        count: n as u32,
        start_hz: f64_at(raw, 16),
        step_hz: f64_at(raw, 24),
        scale_db: f32_at(raw, 32),
        offset_db: f32_at(raw, 36),
        timestamp_ns: u64_at(raw, 40),
    };
    let (scale, offset) = (h.scale_db, h.offset_db);
    let power = raw[SPECTRUM_HEADER..]
        .chunks_exact(2)
        .map(|c| match i16::from_le_bytes([c[0], c[1]]) {
            NO_DATA => f32::NAN,
            code => code as f32 * scale + offset,
        })
        .collect();
    Ok((h, power))
}

/// Builds a raw spectrum sweep (simulator, tests): the inverse of `parse_spectrum`.
pub fn encode_spectrum(h: &SpectrumHeader, dbm: &[f32]) -> Vec<u8> {
    let mut b = Vec::with_capacity(SPECTRUM_HEADER + dbm.len() * 2);
    b.extend_from_slice(&SPECTRUM_MAGIC);
    b.extend_from_slice(&1u16.to_le_bytes());
    b.extend_from_slice(&h.flags.to_le_bytes());
    b.extend_from_slice(&h.seq.to_le_bytes());
    b.extend_from_slice(&(dbm.len() as u32).to_le_bytes());
    b.extend_from_slice(&h.start_hz.to_le_bytes());
    b.extend_from_slice(&h.step_hz.to_le_bytes());
    b.extend_from_slice(&h.scale_db.to_le_bytes());
    b.extend_from_slice(&h.offset_db.to_le_bytes());
    b.extend_from_slice(&h.timestamp_ns.to_le_bytes());
    for &v in dbm {
        let code = if v.is_nan() { NO_DATA } else { (((v - h.offset_db) / h.scale_db) as f64).round().clamp(-32767.0, 32767.0) as i16 };
        b.extend_from_slice(&code.to_le_bytes());
    }
    b
}

/// Parses a raw reflection sweep into complex reflection coefficients.
pub fn parse_s11(raw: &[u8]) -> Result<(S11Header, Vec<Complex<f64>>), RawError> {
    let n = check(raw, S11_MAGIC, S11_HEADER, 4)?;
    let h = S11Header {
        flags: u16_at(raw, 6),
        seq: u32_at(raw, 8),
        count: n as u32,
        start_hz: f64_at(raw, 16),
        step_hz: f64_at(raw, 24),
        timestamp_ns: u64_at(raw, 32),
    };
    let gamma = raw[S11_HEADER..]
        .chunks_exact(4)
        .map(|c| {
            let i = i16::from_le_bytes([c[0], c[1]]) as f64 / Q15;
            let q = i16::from_le_bytes([c[2], c[3]]) as f64 / Q15;
            Complex::new(i, q)
        })
        .collect();
    Ok((h, gamma))
}

/// Builds a raw reflection sweep (simulator, tests).
pub fn encode_s11(h: &S11Header, gamma: &[Complex<f64>]) -> Vec<u8> {
    let mut b = Vec::with_capacity(S11_HEADER + gamma.len() * 4);
    b.extend_from_slice(&S11_MAGIC);
    b.extend_from_slice(&1u16.to_le_bytes());
    b.extend_from_slice(&h.flags.to_le_bytes());
    b.extend_from_slice(&h.seq.to_le_bytes());
    b.extend_from_slice(&(gamma.len() as u32).to_le_bytes());
    b.extend_from_slice(&h.start_hz.to_le_bytes());
    b.extend_from_slice(&h.step_hz.to_le_bytes());
    b.extend_from_slice(&h.timestamp_ns.to_le_bytes());
    let q15 = |v: f64| (v * Q15).round().clamp(-32767.0, 32767.0) as i16;
    for g in gamma {
        b.extend_from_slice(&q15(g.re).to_le_bytes());
        b.extend_from_slice(&q15(g.im).to_le_bytes());
    }
    b
}

#[cfg(test)]
mod tests {
    use super::*;

    fn header() -> SpectrumHeader {
        SpectrumHeader { flags: 0, seq: 9, count: 0, start_hz: 700e6, step_hz: 40e3, scale_db: 0.01, offset_db: 0.0, timestamp_ns: 5 }
    }

    #[test]
    fn spectrum_round_trip_at_0_01_db() {
        let dbm = [-100.0f32, -61.23, f32::NAN, -0.01];
        let raw = encode_spectrum(&header(), &dbm);
        assert_eq!(raw.len(), SPECTRUM_HEADER + 8);
        let (h, p) = parse_spectrum(&raw).unwrap();
        assert_eq!((h.seq, h.count, h.start_hz, h.step_hz), (9, 4, 700e6, 40e3));
        assert!((p[0] + 100.0).abs() < 0.006 && (p[1] + 61.23).abs() < 0.006 && p[2].is_nan());
    }

    #[test]
    fn malformed_buffers_are_rejected() {
        let raw = encode_spectrum(&header(), &[-90.0, -91.0]);
        assert_eq!(parse_spectrum(&raw[..10]).unwrap_err(), RawError::Short);
        assert_eq!(parse_spectrum(&raw[..raw.len() - 1]).unwrap_err(), RawError::Length { want: 52, got: 51 });
        let mut bad = raw.clone();
        bad[0] = b'X';
        assert_eq!(parse_spectrum(&bad).unwrap_err(), RawError::Magic);
        let mut v2 = raw;
        v2[4] = 2;
        assert_eq!(parse_spectrum(&v2).unwrap_err(), RawError::Version(2));
    }

    #[test]
    fn s11_round_trip_q15() {
        let h = S11Header { flags: 0, seq: 1, count: 0, start_hz: 1e9, step_hz: 1e6, timestamp_ns: 0 };
        let g = [Complex::new(0.5, -0.25), Complex::new(-0.999, 0.001)];
        let (h2, g2) = parse_s11(&encode_s11(&h, &g)).unwrap();
        assert_eq!(h2.count, 2);
        for (a, b) in g.iter().zip(&g2) {
            assert!((a - b).norm() < 1e-4);
        }
    }
}
