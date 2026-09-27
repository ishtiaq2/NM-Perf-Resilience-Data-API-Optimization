//! Spectrum Analyzer: parameters, sweeps, and every wire representation of a
//! sweep, each encoded once per sweep on the DSP pool and shared by all clients.

use std::collections::HashMap;
use std::io::Write;
use std::sync::{Arc, Mutex};

use bytes::Bytes;
use flate2::write::GzEncoder;
use flate2::Compression;
use tokio::sync::OnceCell;

use crate::decimate::peak_decimate;
use crate::dsp::{Busy, DspPool};
use crate::dspc::{self, FrameMeta};
use crate::fpga;
use crate::hw::{Analyzer, BoxFut};
use crate::session::Measure;
use crate::util::{now_ms, push_f64, push_int, push_round2};

pub const JSON_TYPE: &str = "application/json; charset=utf-8";

/// One analyzer configuration (one sweep session).
#[derive(Debug, Clone, PartialEq)]
pub struct SpectrumParams {
    pub node_id: u32,
    pub port: u16,
    pub start_hz: f64,
    pub stop_hz: f64,
    pub points: usize,
    pub rbw_hz: f64,
}

impl SpectrumParams {
    pub fn key(&self) -> String {
        format!("{}:{}:{}:{}:{}", self.node_id, self.port, self.start_hz, self.stop_hz, self.points)
    }

    /// Frequency step of the full-resolution grid.
    pub fn step_hz(&self) -> f64 {
        (self.stop_hz - self.start_hz) / (self.points - 1) as f64
    }

    /// Validates query values; missing ones take the defaults (same rules as the
    /// Node.js and Go services).
    pub fn parse(q: &HashMap<String, String>, default_points: usize) -> Result<Self, String> {
        let mut p = SpectrumParams { node_id: 1, port: 1, start_hz: 700e6, stop_hz: 2700e6, points: default_points, rbw_hz: 30_000.0 };
        if let Some(v) = q.get("nodeId").and_then(|v| v.parse::<u32>().ok()).filter(|&v| v > 0) {
            p.node_id = v;
        }
        if let Some(v) = q.get("port").and_then(|v| v.parse::<u16>().ok()).filter(|&v| v > 0) {
            p.port = v;
        }
        if let Some(v) = q.get("startHz").and_then(|v| v.parse::<f64>().ok()).filter(|v| v.is_finite() && *v > 0.0) {
            p.start_hz = v;
        }
        if let Some(v) = q.get("stopHz").and_then(|v| v.parse::<f64>().ok()).filter(|v| v.is_finite() && *v > 0.0) {
            p.stop_hz = v;
        }
        if let Some(v) = q.get("points").and_then(|v| v.parse::<usize>().ok()).filter(|&v| v > 0) {
            p.points = v;
        }
        if p.stop_hz <= p.start_hz {
            return Err("stopHz must be greater than startHz".into());
        }
        p.points = p.points.clamp(101, 200_001);
        Ok(p)
    }
}

/// Wire representations of a sweep.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Kind {
    /// The shipped frontend's shape: `points: [{frequency, power}]` (~40 bytes per point).
    Legacy,
    /// Compact JSON with an implicit frequency axis.
    Json,
    /// DSPC binary frame, int16 centi-dBm.
    I16,
    /// DSPC binary frame, float32.
    F32,
}

impl Kind {
    pub fn name(self) -> &'static str {
        match self {
            Kind::Legacy => "legacy",
            Kind::Json => "json",
            Kind::I16 => "i16",
            Kind::F32 => "f32",
        }
    }
}

/// Which representation: kind, decimation width (0 = full), gzip.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub struct VariantKey {
    pub kind: Kind,
    pub max_points: usize,
    pub gzip: bool,
}

/// One encoded representation, shared by reference between all clients.
#[derive(Debug)]
pub struct Variant {
    pub body: Bytes,
    pub content_type: &'static str,
    pub gzip: bool,
    pub etag: String,
    pub count: usize,
    pub decimated: bool,
}

/// One completed sweep. `power` is never modified after publication.
pub struct Sweep {
    pub id: u32,
    pub params: SpectrumParams,
    pub timestamp: i64,
    pub power: Arc<[f32]>,
    tag: String,
    variants: Mutex<HashMap<VariantKey, Arc<OnceCell<Arc<Variant>>>>>,
}

impl Sweep {
    pub fn new(id: u32, params: SpectrumParams, timestamp: i64, power: Arc<[f32]>, tag: String) -> Self {
        Sweep { id, params, timestamp, power, tag, variants: Mutex::new(HashMap::new()) }
    }

    /// ETag of a variant, without building it (for 304 decisions).
    pub fn etag(&self, k: VariantKey) -> String {
        format!("\"{}-{}-{}{}\"", self.tag, k.kind.name(), k.max_points, if k.gzip { "-gz" } else { "" })
    }

    /// The requested representation, built once per sweep (concurrent callers wait for
    /// the first build) on the DSP pool.
    pub async fn variant(self: &Arc<Self>, dsp: &DspPool, k: VariantKey) -> Result<Arc<Variant>, Busy> {
        let cell = self.variants.lock().unwrap().entry(k).or_insert_with(|| Arc::new(OnceCell::new())).clone();
        let v = cell
            .get_or_try_init(|| async {
                let sw = self.clone();
                dsp.run(move || build_variant(&sw, k)).await.map(Arc::new)
            })
            .await?;
        Ok(v.clone())
    }
}

fn gzip(data: &[u8]) -> Vec<u8> {
    let mut e = GzEncoder::new(Vec::with_capacity(data.len() / 5), Compression::fast());
    let _ = e.write_all(data);
    e.finish().unwrap_or_default()
}

/// Builds one representation (CPU-heavy: runs on the DSP pool).
pub fn build_variant(s: &Sweep, k: VariantKey) -> Variant {
    let p = &s.params;
    let (body, content_type, count, decimated) = match k.kind {
        Kind::Legacy => (legacy_json(s), JSON_TYPE, s.power.len(), false),
        kind => {
            let (power, start, step, dec) = peak_decimate(&s.power, p.start_hz, p.step_hz(), k.max_points);
            let body = match kind {
                Kind::Json => compact_json(s, &power, start, step, dec),
                _ => {
                    let meta = FrameMeta {
                        sweep_id: s.id,
                        node_id: p.node_id,
                        port: p.port,
                        start_hz: start,
                        step_hz: step,
                        timestamp_ms: s.timestamp as f64,
                        decimated: dec,
                    };
                    dspc::encode(&meta, &power, if kind == Kind::F32 { dspc::ENC_F32 } else { dspc::ENC_I16 })
                }
            };
            let ct = if kind == Kind::Json { JSON_TYPE } else { dspc::CONTENT_TYPE };
            (body, ct, power.len(), dec)
        }
    };
    let body = if k.gzip { gzip(&body) } else { body };
    Variant { body: Bytes::from(body), content_type, gzip: k.gzip, etag: s.etag(k), count, decimated }
}

/// The shipped frontend's shape, byte-compatible with the Node.js and Go services:
/// `{"sweepId",…,"points":[{"frequency":Hz,"power":dBm},…]}`.
pub fn legacy_json(s: &Sweep) -> Vec<u8> {
    let p = &s.params;
    let step = p.step_hz();
    let mut b = Vec::with_capacity(192 + s.power.len() * 40);
    b.extend_from_slice(b"{\"sweepId\":");
    push_int(&mut b, s.id);
    b.extend_from_slice(b",\"nodeId\":");
    push_int(&mut b, p.node_id);
    b.extend_from_slice(b",\"port\":");
    push_int(&mut b, p.port);
    b.extend_from_slice(b",\"timestamp\":");
    push_int(&mut b, s.timestamp);
    b.extend_from_slice(b",\"startHz\":");
    push_f64(&mut b, p.start_hz);
    b.extend_from_slice(b",\"stopHz\":");
    push_f64(&mut b, p.stop_hz);
    b.extend_from_slice(b",\"rbwHz\":");
    push_f64(&mut b, p.rbw_hz);
    b.extend_from_slice(b",\"points\":[");
    for (i, &v) in s.power.iter().enumerate() {
        if i > 0 {
            b.push(b',');
        }
        b.extend_from_slice(b"{\"frequency\":");
        push_int(&mut b, (p.start_hz + i as f64 * step).round() as i64);
        b.extend_from_slice(b",\"power\":");
        push_round2(&mut b, v);
        b.push(b'}');
    }
    b.extend_from_slice(b"]}");
    b
}

/// Compact JSON of `/api/spectrum/latest` (implicit frequency axis).
pub fn compact_json(s: &Sweep, power: &[f32], start_hz: f64, step_hz: f64, decimated: bool) -> Vec<u8> {
    let p = &s.params;
    let mut b = Vec::with_capacity(192 + power.len() * 8);
    b.extend_from_slice(b"{\"sweepId\":");
    push_int(&mut b, s.id);
    b.extend_from_slice(b",\"nodeId\":");
    push_int(&mut b, p.node_id);
    b.extend_from_slice(b",\"port\":");
    push_int(&mut b, p.port);
    b.extend_from_slice(b",\"timestamp\":");
    push_int(&mut b, s.timestamp);
    b.extend_from_slice(b",\"startHz\":");
    push_f64(&mut b, start_hz);
    b.extend_from_slice(b",\"stepHz\":");
    push_f64(&mut b, step_hz);
    b.extend_from_slice(b",\"count\":");
    push_int(&mut b, power.len());
    b.extend_from_slice(if decimated { b",\"decimated\":true" } else { b",\"decimated\":false" });
    b.extend_from_slice(b",\"powerDbm\":[");
    for (i, &v) in power.iter().enumerate() {
        if i > 0 {
            b.push(b',');
        }
        push_round2(&mut b, v);
    }
    b.extend_from_slice(b"]}");
    b
}

/// Spectrum sweeps from the hardware, parsed on the DSP pool.
pub struct SpectrumMeasure {
    pub hw: Arc<dyn Analyzer>,
    pub dsp: DspPool,
}

impl Measure for SpectrumMeasure {
    type Params = SpectrumParams;
    type Output = Sweep;

    fn key(p: &SpectrumParams) -> String {
        p.key()
    }

    fn measure<'a>(&'a self, p: &'a SpectrumParams, id: u32, tag: String) -> BoxFut<'a, Result<Sweep, String>> {
        Box::pin(async move {
            let raw = self.hw.sweep(p).await.map_err(|e| e.to_string())?;
            let (hdr, power) = self.dsp.run(move || fpga::parse_spectrum(&raw)).await.map_err(|e| e.to_string())?.map_err(|e| e.to_string())?;
            if hdr.count as usize != p.points {
                return Err(format!("analyzer returned {} bins, {} requested", hdr.count, p.points));
            }
            // Every response format derives its frequency axis from the request: the
            // hardware must have swept exactly that grid.
            let step = p.step_hz();
            if (hdr.start_hz - p.start_hz).abs() > step / 2.0 || (hdr.step_hz - step).abs() > step * 1e-6 {
                return Err(format!("analyzer swept {} Hz + n x {} Hz, {} Hz + n x {} Hz requested", hdr.start_hz, hdr.step_hz, p.start_hz, step));
            }
            Ok(Sweep::new(id, p.clone(), now_ms(), power.into(), tag))
        })
    }

    fn id(o: &Sweep) -> u32 {
        o.id
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sweep() -> Sweep {
        let mut q = HashMap::new();
        q.insert("nodeId".to_string(), "3".to_string());
        q.insert("port".to_string(), "2".to_string());
        let mut p = SpectrumParams::parse(&q, 50001).unwrap();
        p.points = 2;
        p.stop_hz = p.start_hz + 40000.0;
        Sweep::new(9, p, 1_790_000_000_123, Arc::from(vec![-97.12f32, -60.5]), "sp-boot-1-9".into())
    }

    #[test]
    fn legacy_shape_is_the_shipped_one() {
        let body = String::from_utf8(legacy_json(&sweep())).unwrap();
        assert_eq!(
            body,
            r#"{"sweepId":9,"nodeId":3,"port":2,"timestamp":1790000000123,"startHz":700000000,"stopHz":700040000,"rbwHz":30000,"points":[{"frequency":700000000,"power":-97.12},{"frequency":700040000,"power":-60.5}]}"#
        );
        let v: serde_json::Value = serde_json::from_str(&body).unwrap();
        assert_eq!(v["points"][1]["power"], -60.5);
    }

    #[test]
    fn variants_have_distinct_tags_and_types() {
        let s = sweep();
        let a = build_variant(&s, VariantKey { kind: Kind::I16, max_points: 0, gzip: false });
        let b = build_variant(&s, VariantKey { kind: Kind::Json, max_points: 0, gzip: true });
        assert_eq!(a.content_type, dspc::CONTENT_TYPE);
        assert_eq!(b.content_type, JSON_TYPE);
        assert_ne!(a.etag, b.etag);
        assert!(b.etag.ends_with("-gz\""));
        let (m, p) = dspc::decode(&a.body).unwrap();
        assert_eq!((m.sweep_id, m.node_id, m.port, p.len()), (9, 3, 2, 2));
        let mut d = flate2::read::GzDecoder::new(&b.body[..]);
        let mut json = String::new();
        std::io::Read::read_to_string(&mut d, &mut json).unwrap();
        assert!(json.contains(r#""powerDbm":[-97.12,-60.5]"#));
    }

    #[test]
    fn params_validation() {
        let mut q = HashMap::new();
        q.insert("startHz".to_string(), "2e9".to_string());
        q.insert("stopHz".to_string(), "1e9".to_string());
        assert!(SpectrumParams::parse(&q, 2001).is_err());
        let mut q = HashMap::new();
        q.insert("points".to_string(), "5".to_string());
        q.insert("nodeId".to_string(), "abc".to_string());
        let p = SpectrumParams::parse(&q, 2001).unwrap();
        assert_eq!((p.points, p.node_id), (101, 1));
    }
}
