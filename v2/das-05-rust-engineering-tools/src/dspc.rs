//! DSPC v1: the binary spectrum frame of the das-v1 contract
//! (das-00/contract/spectrum-binary-frame.md). Little-endian, 48-byte header,
//! then int16 centi-dBm (-32768 = no data) or float32 dBm.
//!
//! Byte-identical to the Node.js (das-01/02) and Go (das-03) encoders.

pub const CONTENT_TYPE: &str = "application/vnd.das.spectrum";
pub const MAGIC: u32 = 0x4350_5344; // "DSPC"
pub const VERSION: u8 = 1;
pub const HEADER: usize = 48;
pub const ENC_I16: u8 = 1;
pub const ENC_F32: u8 = 2;
pub const I16_NODATA: i16 = -32768;

/// Everything in a frame header except the payload.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct FrameMeta {
    pub sweep_id: u32,
    pub node_id: u32,
    pub port: u16,
    pub start_hz: f64,
    pub step_hz: f64,
    pub timestamp_ms: f64,
    pub decimated: bool,
}

/// Encodes a frame with `ENC_I16` (centi-dBm) or `ENC_F32`.
pub fn encode(m: &FrameMeta, power: &[f32], enc: u8) -> Vec<u8> {
    let per = if enc == ENC_F32 { 4 } else { 2 };
    let mut b = Vec::with_capacity(HEADER + power.len() * per);
    b.extend_from_slice(&MAGIC.to_le_bytes());
    b.push(VERSION);
    b.push(enc);
    b.extend_from_slice(&u16::from(m.decimated).to_le_bytes());
    b.extend_from_slice(&m.sweep_id.to_le_bytes());
    b.extend_from_slice(&m.node_id.to_le_bytes());
    b.extend_from_slice(&m.port.to_le_bytes());
    b.extend_from_slice(&[0, 0]); // reserved
    b.extend_from_slice(&(power.len() as u32).to_le_bytes());
    b.extend_from_slice(&m.start_hz.to_le_bytes());
    b.extend_from_slice(&m.step_hz.to_le_bytes());
    b.extend_from_slice(&m.timestamp_ms.to_le_bytes());
    debug_assert_eq!(b.len(), HEADER);
    if enc == ENC_F32 {
        for &v in power {
            b.extend_from_slice(&v.to_le_bytes());
        }
    } else {
        for &v in power {
            let q = if v.is_nan() { I16_NODATA } else { crate::util::js_round(v as f64 * 100.0).clamp(-32767.0, 32767.0) as i16 };
            b.extend_from_slice(&q.to_le_bytes());
        }
    }
    b
}

fn u16_at(b: &[u8], o: usize) -> u16 {
    u16::from_le_bytes([b[o], b[o + 1]])
}
fn u32_at(b: &[u8], o: usize) -> u32 {
    u32::from_le_bytes(b[o..o + 4].try_into().unwrap())
}
fn f64_at(b: &[u8], o: usize) -> f64 {
    f64::from_le_bytes(b[o..o + 8].try_into().unwrap())
}

/// Decodes a frame.
pub fn decode(b: &[u8]) -> Result<(FrameMeta, Vec<f32>), &'static str> {
    if b.len() < HEADER {
        return Err("dspc: frame too short");
    }
    if u32_at(b, 0) != MAGIC || b[4] != VERSION {
        return Err("dspc: bad magic or version");
    }
    let enc = b[5];
    let per = match enc {
        ENC_I16 => 2,
        ENC_F32 => 4,
        _ => return Err("dspc: unknown encoding"),
    };
    let n = u32_at(b, 20) as usize;
    if b.len() < HEADER + n * per {
        return Err("dspc: truncated payload");
    }
    let m = FrameMeta {
        sweep_id: u32_at(b, 8),
        node_id: u32_at(b, 12),
        port: u16_at(b, 16),
        start_hz: f64_at(b, 24),
        step_hz: f64_at(b, 32),
        timestamp_ms: f64_at(b, 40),
        decimated: u16_at(b, 6) & 1 == 1,
    };
    let p = &b[HEADER..HEADER + n * per];
    let power = if enc == ENC_F32 {
        p.chunks_exact(4).map(|c| f32::from_le_bytes(c.try_into().unwrap())).collect()
    } else {
        p.chunks_exact(2)
            .map(|c| match i16::from_le_bytes([c[0], c[1]]) {
                I16_NODATA => f32::NAN,
                q => q as f32 / 100.0,
            })
            .collect()
    };
    Ok((m, power))
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::Engine;

    /// Produced by the Node.js encoder (das-01 src/shared/spectrum-frame.js); the Go test uses it too.
    const NODE_FRAME_B64: &str = "RFNQQwEBAQAqAAAABwAAAAIAAAAFAAAAAAAAgJPcxEEAAAAAAIjjQAAAwAZFDHpCENpe6ACA0gTw2A==";

    #[test]
    fn byte_identical_with_node_and_go() {
        let raw = base64::engine::general_purpose::STANDARD.decode(NODE_FRAME_B64).unwrap();
        let (m, p) = decode(&raw).unwrap();
        assert_eq!((m.sweep_id, m.node_id, m.port, m.start_hz, m.step_hz, m.decimated), (42, 7, 2, 700e6, 40000.0, true));
        assert_eq!(p[0], -97.12);
        assert_eq!(p[1], -60.5);
        assert!(p[2].is_nan());
        assert_eq!(p[4], -100.0);
        let again = encode(&m, &[-97.12, -60.5, f32::NAN, 12.34, -100.0], ENC_I16);
        assert_eq!(base64::engine::general_purpose::STANDARD.encode(again), NODE_FRAME_B64);
    }

    #[test]
    fn rounding_ties_match_node() {
        // Node.js: encodeFrame({sweepId: 9, nodeId: 3, port: 1, startHz: 700e6, stepHz: 40000,
        // timestampMs: 1700000000000}, Float32Array.from([-95.375, -95.625, 95.375, -0.125, -100.875]))
        const NODE_TIES_B64: &str = "RFNQQwEBAAAJAAAAAwAAAAEAAAAFAAAAAAAAgJPcxEEAAAAAAIjjQAAAgFb+vHhCv9qm2kIl9P+Z2A==";
        let m = FrameMeta { sweep_id: 9, node_id: 3, port: 1, start_hz: 700e6, step_hz: 40000.0, timestamp_ms: 1.7e12, decimated: false };
        let b = encode(&m, &[-95.375, -95.625, 95.375, -0.125, -100.875], ENC_I16);
        assert_eq!(base64::engine::general_purpose::STANDARD.encode(b), NODE_TIES_B64);
    }

    #[test]
    fn f32_round_trip_and_errors() {
        let m = FrameMeta { sweep_id: 1, node_id: 2, port: 3, start_hz: 1e9, step_hz: 5e3, timestamp_ms: 1.0, decimated: false };
        let b = encode(&m, &[-1.5, f32::NAN, 3.25], ENC_F32);
        let (m2, p) = decode(&b).unwrap();
        assert_eq!(m2, m);
        assert_eq!(p[0], -1.5);
        assert!(p[1].is_nan());
        assert_eq!(decode(&b[..HEADER + 4]).unwrap_err(), "dspc: truncated payload");
        assert_eq!(decode(&b[..10]).unwrap_err(), "dspc: frame too short");
    }
}
