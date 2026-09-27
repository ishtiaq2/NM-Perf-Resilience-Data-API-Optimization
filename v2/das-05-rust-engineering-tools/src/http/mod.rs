//! HTTP routes (das-v1 contract) and handlers.
//!
//! Handlers never parse or serialise sweep data themselves: they pick the
//! session, wait if needed, and send bytes that were encoded once on the DSP pool.

pub mod neg;
pub mod proxy;
pub mod ws;

use std::collections::HashMap;
use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use axum::body::Body;
use axum::extract::{Query, Request, State};
use axum::http::header::{self, HeaderMap, HeaderValue};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::routing::get;
use axum::Router;
use serde_json::{json, Value};

use crate::app::{AppState, SERVER, VERSION};
use crate::decimate::normalise_max_points;
use crate::dsp::Busy;
use crate::dspc;
use crate::dtf::{DtfMeasure, DtfParams};
use crate::metrics::{process_stats, Route};
use crate::session::{Manager, Measure, Session};
use crate::spectrum::{Kind, SpectrumParams, Sweep, Variant, VariantKey, JSON_TYPE};
use crate::util::now_ms;
use neg::{accepts_encoding, accepts_media_type, if_none_match};
use proxy::json_error;

type Q = Query<HashMap<String, String>>;

/// The router. In front mode every path not listed here goes to the Node.js app.
pub fn router(app: AppState) -> Router {
    let mut r = Router::new()
        .route("/api/spectrum", get(spectrum_legacy))
        .route("/api/spectrum/latest", get(spectrum_latest))
        .route("/api/ws/spectrum", get(ws::ws_spectrum))
        .route("/api/dtf", get(dtf))
        .route("/api/capabilities", get(capabilities))
        .route("/api/engineering/status", get(status))
        .route("/internal/health", get(health))
        .route("/internal/metrics", get(metrics));
    if app.legacy.is_none() {
        r = r.route("/api/heartbeat", get(heartbeat)); // standalone; otherwise the Node app answers it
    }
    r.fallback(fallback).with_state(app)
}

fn busy() -> Response {
    let mut r = json_error(StatusCode::SERVICE_UNAVAILABLE, "BUSY", "engineering tools are at capacity, retry shortly");
    r.headers_mut().insert(header::RETRY_AFTER, HeaderValue::from_static("1"));
    r
}

fn sweep_timeout() -> Response {
    let mut r = json_error(StatusCode::SERVICE_UNAVAILABLE, "SWEEP_TIMEOUT", "the analyzer did not complete a measurement in time");
    r.headers_mut().insert(header::RETRY_AFTER, HeaderValue::from_static("1"));
    r
}

#[allow(clippy::result_large_err)] // the error is the ready-made HTTP response
fn session_for<M: Measure>(mgr: &Arc<Manager<M>>, p: M::Params) -> Result<Arc<Session<M>>, Response> {
    mgr.session(p).map_err(|e| {
        let mut r = json_error(StatusCode::SERVICE_UNAVAILABLE, "TOO_MANY_SESSIONS", &e.to_string());
        r.headers_mut().insert(header::RETRY_AFTER, HeaderValue::from_static("2"));
        r
    })
}

fn not_modified(etag: &str, extra: &[(&'static str, String)]) -> Response {
    let mut r = StatusCode::NOT_MODIFIED.into_response();
    let h = r.headers_mut();
    if let Ok(v) = HeaderValue::from_str(etag) {
        h.insert(header::ETAG, v);
    }
    for (k, v) in extra {
        if let Ok(v) = HeaderValue::from_str(v) {
            h.insert(*k, v);
        }
    }
    r
}

fn variant_response(v: &Variant, sw: &Sweep, cache_control: &'static str, vary: &'static str) -> Response {
    let mut r = Response::new(Body::from(v.body.clone())); // shared bytes, no copy
    let h = r.headers_mut();
    h.insert(header::CONTENT_TYPE, HeaderValue::from_static(v.content_type));
    h.insert(header::CACHE_CONTROL, HeaderValue::from_static(cache_control));
    h.insert(header::VARY, HeaderValue::from_static(vary));
    if let Ok(e) = HeaderValue::from_str(&v.etag) {
        h.insert(header::ETAG, e);
    }
    h.insert("x-sweep-id", HeaderValue::from(sw.id));
    if v.gzip {
        h.insert(header::CONTENT_ENCODING, HeaderValue::from_static("gzip"));
    }
    r
}

/// Legacy `/api/spectrum`: the shipped shape and semantics (each poll returns a sweep
/// completed after the request arrived), but every client shares one sweep and the body
/// is encoded once per sweep.
async fn spectrum_legacy(State(app): State<AppState>, Query(q): Q, headers: HeaderMap) -> Response {
    let p = match SpectrumParams::parse(&q, app.cfg.points) {
        Ok(p) => p,
        Err(e) => return json_error(StatusCode::BAD_REQUEST, "BAD_REQUEST", &e),
    };
    let s = match session_for(&app.spectrum, p) {
        Ok(s) => s,
        Err(r) => return r,
    };
    let sw = match s.next(app.cfg.sweep_wait()).await {
        Some(sw) => sw,
        None => match s.latest() {
            Some(l) => l, // better stale than an error for the shipped UI
            None => return sweep_timeout(),
        },
    };
    let key = VariantKey { kind: Kind::Legacy, max_points: 0, gzip: accepts_encoding(&headers, "gzip") };
    match sw.variant(&app.dsp, key).await {
        Ok(v) => {
            app.metrics.count(Route::SpectrumLegacy, 200, v.body.len());
            variant_response(&v, &sw, "no-store", "Accept-Encoding")
        }
        Err(Busy) => busy(),
    }
}

/// `/api/spectrum/latest`: newest sweep now, conditional (ETag), compact JSON or binary,
/// decimated on request, optional long-poll (`waitMs`).
async fn spectrum_latest(State(app): State<AppState>, Query(q): Q, headers: HeaderMap) -> Response {
    let p = match SpectrumParams::parse(&q, app.cfg.points) {
        Ok(p) => p,
        Err(e) => return json_error(StatusCode::BAD_REQUEST, "BAD_REQUEST", &e),
    };
    let s = match session_for(&app.spectrum, p) {
        Ok(s) => s,
        Err(r) => return r,
    };
    let binary = accepts_media_type(&headers, dspc::CONTENT_TYPE) || q.get("format").map(String::as_str) == Some("binary");
    let kind = match (binary, q.get("encoding").map(String::as_str)) {
        (false, _) => Kind::Json,
        (true, Some("f32")) => Kind::F32,
        (true, _) => Kind::I16,
    };
    let max_points = normalise_max_points(q.get("maxPoints").and_then(|v| v.parse::<i64>().ok()).unwrap_or(0));
    // Binary spectrum is mostly noise: gzip saves little for a lot of CPU.
    let key = VariantKey { kind, max_points, gzip: !binary && accepts_encoding(&headers, "gzip") };
    let wait_ms = q.get("waitMs").and_then(|v| v.parse::<u64>().ok()).unwrap_or(0).min(10_000);

    let Some(mut sw) = s.latest_or_next(app.cfg.sweep_wait()).await else { return sweep_timeout() };
    let tag = sw.etag(key);
    if if_none_match(&headers, &tag) {
        // The client has this sweep: wait for the next one if asked, else 304 without building anything.
        if wait_ms > 0 {
            if let Some(next) = s.after(sw.id, Duration::from_millis(wait_ms)).await {
                sw = next;
            }
        }
        if sw.etag(key) == tag {
            app.metrics.count(Route::SpectrumLatest, 304, 0);
            return not_modified(&tag, &[("cache-control", "no-cache".into()), ("vary", "Accept, Accept-Encoding".into()), ("x-sweep-id", sw.id.to_string())]);
        }
    }
    match sw.variant(&app.dsp, key).await {
        Ok(v) => {
            app.metrics.count(Route::SpectrumLatest, 200, v.body.len());
            variant_response(&v, &sw, "no-cache", "Accept, Accept-Encoding")
        }
        Err(Busy) => busy(),
    }
}

/// `/api/dtf`: the Distance-to-Fault profile of a feeder, latest measurement, ETag,
/// optional long-poll for the next one.
async fn dtf(State(app): State<AppState>, Query(q): Q, headers: HeaderMap) -> Response {
    let p = match DtfParams::parse(&q) {
        Ok(p) => p,
        Err(e) => return json_error(StatusCode::BAD_REQUEST, "BAD_REQUEST", &e),
    };
    let s = match session_for::<DtfMeasure>(&app.dtf, p) {
        Ok(s) => s,
        Err(r) => return r,
    };
    let wait_ms = q.get("waitMs").and_then(|v| v.parse::<u64>().ok()).unwrap_or(0).min(10_000);
    let Some(mut res) = s.latest_or_next(app.cfg.sweep_wait()).await else { return sweep_timeout() };
    if if_none_match(&headers, &res.etag) {
        if wait_ms > 0 {
            if let Some(next) = s.after(res.id, Duration::from_millis(wait_ms)).await {
                res = next;
            }
        }
        if if_none_match(&headers, &res.etag) {
            app.metrics.count(Route::Dtf, 304, 0);
            return not_modified(&res.etag, &[("cache-control", "no-cache".into())]);
        }
    }
    match res.json(&app.dsp).await {
        Ok(body) => {
            app.metrics.count(Route::Dtf, 200, body.len());
            let mut r = Response::new(Body::from(body));
            let h = r.headers_mut();
            h.insert(header::CONTENT_TYPE, HeaderValue::from_static(JSON_TYPE));
            h.insert(header::CACHE_CONTROL, HeaderValue::from_static("no-cache"));
            if let Ok(e) = HeaderValue::from_str(&res.etag) {
                h.insert(header::ETAG, e);
            }
            h.insert("x-measurement-id", HeaderValue::from(res.id));
            r
        }
        Err(Busy) => busy(),
    }
}

/// What this service adds to the das-v1 capabilities.
const OWN_FEATURES: [&str; 7] = ["etag", "spectrumLatest", "spectrumBinary", "spectrumDecimation", "spectrumLongPoll", "wsSpectrum", "dtf"];

/// Capabilities of the whole origin: the Node app's own features merged with the
/// engineering features served here, so clients opt into exactly what is available.
async fn capabilities(State(app): State<AppState>) -> Response {
    app.metrics.count(Route::Capabilities, 200, 0);
    let legacy = match &app.legacy {
        Some(l) => l.capabilities().await,
        None => Default::default(),
    };
    let mut features = legacy.features;
    for f in OWN_FEATURES {
        features.insert(f.into(), Value::Bool(true));
    }
    let mut limits = legacy.limits;
    limits.insert("maxSpectrumPoints".into(), json!(200001));
    limits.insert("maxDecimatedPoints".into(), json!(65536));
    limits.insert("maxDtfPoints".into(), json!(16001));
    let server = match legacy.server {
        Some(s) => format!("{SERVER} + {s}"),
        None => SERVER.to_string(),
    };
    let body = json!({
        "api": "das-v1",
        "server": server,
        "version": VERSION,
        "features": features,
        "limits": limits
    });
    ([(header::CONTENT_TYPE, JSON_TYPE), (header::CACHE_CONTROL, "no-cache")], body.to_string()).into_response()
}

/// Only when running standalone (no Node app configured).
async fn heartbeat(State(app): State<AppState>) -> Response {
    app.metrics.count(Route::Heartbeat, 200, 0);
    let body = json!({
        "status": "ok",
        "server": SERVER,
        "version": VERSION,
        "bootId": app.boot_id,
        "time": now_ms(),
        "uptimeS": (app.started.elapsed().as_secs_f64() * 10.0).round() / 10.0,
    });
    ([(header::CONTENT_TYPE, JSON_TYPE), (header::CACHE_CONTROL, "no-store")], body.to_string()).into_response()
}

/// For the gateway's health probes (das-02 core-api probes /internal/health).
async fn health(State(app): State<AppState>) -> Response {
    app.metrics.count(Route::Internal, 200, 0);
    let d = app.dsp.stats();
    let status = if d.queued * 2 > app.cfg.dsp_queue.max(1) { "degraded" } else { "ok" };
    (
        [(header::CONTENT_TYPE, JSON_TYPE), (header::CACHE_CONTROL, "no-store")],
        json!({ "status": status, "service": SERVER, "dspQueued": d.queued }).to_string(),
    )
        .into_response()
}

fn metrics_doc(app: &AppState) -> Value {
    json!({
        "service": SERVER,
        "version": VERSION,
        "bootId": app.boot_id,
        "uptimeS": app.started.elapsed().as_secs(),
        "mode": if app.cfg.front_mode() { "front" } else { "sidecar" },
        "routes": app.metrics.routes_json(),
        "dsp": app.dsp.stats(),
        "spectrum": app.spectrum.stats(),
        "dtf": app.dtf.stats(),
        "ws": {
            "clients": app.metrics.ws_clients.load(Ordering::Relaxed),
            "frames": app.metrics.ws_frames.load(Ordering::Relaxed),
            "rejected": app.metrics.ws_rejected.load(Ordering::Relaxed),
        },
        "proxy": app.legacy.as_ref().map(|l| json!({
            "target": l.base(),
            "requests": l.requests.load(Ordering::Relaxed),
            "failures": l.failures.load(Ordering::Relaxed),
        })),
        "process": process_stats(),
    })
}

async fn metrics(State(app): State<AppState>, Query(q): Q) -> Response {
    app.metrics.count(Route::Internal, 200, 0);
    let doc = metrics_doc(&app);
    if q.get("format").map(String::as_str) == Some("prom") {
        let mut numeric = doc.clone();
        if let Some(o) = numeric.as_object_mut() {
            o.remove("routes");
        }
        return ([(header::CONTENT_TYPE, "text/plain; version=0.0.4; charset=utf-8")], app.metrics.prometheus(&numeric)).into_response();
    }
    ([(header::CONTENT_TYPE, JSON_TYPE), (header::CACHE_CONTROL, "no-store")], doc.to_string()).into_response()
}

/// Engineering tools status for the UI (sessions, DSP load).
async fn status(State(app): State<AppState>) -> Response {
    let doc = json!({ "dsp": app.dsp.stats(), "spectrum": app.spectrum.stats(), "dtf": app.dtf.stats(), "process": process_stats() });
    ([(header::CONTENT_TYPE, JSON_TYPE), (header::CACHE_CONTROL, "no-store")], doc.to_string()).into_response()
}

async fn fallback(State(app): State<AppState>, req: Request) -> Response {
    match (&app.legacy, app.cfg.front_mode()) {
        (Some(l), true) => {
            let r = l.forward(req).await;
            app.metrics.count(Route::Proxy, r.status().as_u16(), 0);
            r
        }
        _ => json_error(StatusCode::NOT_FOUND, "NOT_FOUND", "no such endpoint on the engineering tools service"),
    }
}
