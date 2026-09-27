//! The existing Node.js application ("legacy"), reached over TCP or a Unix socket.
//!
//! Front mode: this service owns the port and forwards every request it does
//! not implement to the Node.js application, which keeps doing what it is good
//! at (UI files, login, configuration forms). The browser keeps one origin; the
//! Node app sees the browser's Host header.
//!
//! Sidecar mode behind a gateway: the legacy application is only asked for its
//! capabilities, so `/api/capabilities` describes the whole origin.

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use axum::body::Body;
use axum::extract::connect_info::ConnectInfo;
use axum::extract::Request;
use axum::http::header::{self, HeaderMap, HeaderName, HeaderValue};
use axum::http::{StatusCode, Uri};
use axum::response::{IntoResponse, Response};
use http_body_util::BodyExt;
use hyper::body::Incoming;
use hyper_util::client::legacy::connect::HttpConnector;
use hyper_util::client::legacy::Client;
use hyper_util::rt::{TokioExecutor, TokioIo};
use serde_json::{json, Map, Value};

/// What the legacy application says about itself (from its /api/capabilities).
#[derive(Clone, Default)]
pub struct LegacyCaps {
    pub features: Map<String, Value>,
    pub limits: Map<String, Value>,
    pub server: Option<String>,
}

enum Upstream {
    /// `http://host:port`: pooled keep-alive connections.
    Tcp { base: String, client: Client<HttpConnector, Body> },
    /// `unix:/run/das/core-api.sock`: one connection per request (a local socket
    /// connect costs microseconds; no pool to manage).
    Unix { path: PathBuf },
}

/// The legacy (Node.js) application.
pub struct Legacy {
    target: String,
    upstream: Upstream,
    caps: Mutex<Option<(Instant, LegacyCaps)>>,
    pub requests: AtomicU64,
    pub failures: AtomicU64,
}

const HOP_BY_HOP: [&str; 8] = ["connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade"];

fn strip_hop_by_hop(h: &mut HeaderMap) {
    let listed: Vec<HeaderName> = h
        .get_all(header::CONNECTION)
        .iter()
        .filter_map(|v| v.to_str().ok())
        .flat_map(|v| v.split(','))
        .filter_map(|t| HeaderName::from_bytes(t.trim().as_bytes()).ok())
        .collect();
    for n in listed {
        h.remove(n);
    }
    for n in HOP_BY_HOP {
        h.remove(n);
    }
}

/// `Connection: upgrade` plus an `Upgrade` header (a WebSocket handshake).
fn is_upgrade(h: &HeaderMap) -> bool {
    h.contains_key(header::UPGRADE)
        && h.get_all(header::CONNECTION).iter().filter_map(|v| v.to_str().ok()).any(|v| v.split(',').any(|t| t.trim().eq_ignore_ascii_case("upgrade")))
}

pub fn json_error(status: StatusCode, code: &str, message: &str) -> Response {
    (
        status,
        [(header::CONTENT_TYPE, "application/json; charset=utf-8"), (header::CACHE_CONTROL, "no-store")],
        json!({ "error": code, "message": message }).to_string(),
    )
        .into_response()
}

impl Legacy {
    /// `http://host:port` (put the Node app on 127.0.0.1) or `unix:/path/to.sock`.
    pub fn new(url: &str) -> Result<Self, String> {
        let upstream = if let Some(path) = url.strip_prefix("unix:") {
            if path.is_empty() {
                return Err(format!("--legacy {url}: expected unix:/path/to/socket"));
            }
            Upstream::Unix { path: PathBuf::from(path) }
        } else {
            let base = url.trim_end_matches('/').to_string();
            let uri: Uri = base.parse().map_err(|e| format!("--legacy {url}: {e}"))?;
            if uri.scheme_str() != Some("http") || uri.authority().is_none() {
                return Err(format!("--legacy {url}: expected http://host:port or unix:/path"));
            }
            let mut conn = HttpConnector::new();
            conn.set_connect_timeout(Some(Duration::from_secs(2)));
            conn.set_nodelay(true);
            let client = Client::builder(TokioExecutor::new()).pool_idle_timeout(Duration::from_secs(90)).build(conn);
            Upstream::Tcp { base, client }
        };
        Ok(Legacy { target: url.to_string(), upstream, caps: Mutex::new(None), requests: AtomicU64::new(0), failures: AtomicU64::new(0) })
    }

    pub fn base(&self) -> &str {
        &self.target
    }

    /// Sends a request whose URI is origin-form (`/path?query`).
    async fn send(&self, mut req: Request) -> Result<Response<Incoming>, String> {
        match &self.upstream {
            Upstream::Tcp { base, client } => {
                let pq = req.uri().path_and_query().map(|x| x.as_str()).unwrap_or("/");
                *req.uri_mut() = format!("{base}{pq}").parse::<Uri>().map_err(|e| e.to_string())?;
                client.request(req).await.map_err(|e| e.to_string())
            }
            Upstream::Unix { path } => {
                let stream = tokio::net::UnixStream::connect(path).await.map_err(|e| format!("{}: {e}", path.display()))?;
                let (mut sender, conn) = hyper::client::conn::http1::handshake(TokioIo::new(stream)).await.map_err(|e| e.to_string())?;
                tokio::spawn(async move {
                    let _ = conn.with_upgrades().await; // ends with the response, or hands over an upgraded stream
                });
                if !req.headers().contains_key(header::HOST) {
                    req.headers_mut().insert(header::HOST, HeaderValue::from_static("localhost"));
                }
                sender.send_request(req).await.map_err(|e| e.to_string())
            }
        }
    }

    /// Forwards the request and streams the answer back. WebSocket upgrades (the
    /// Node app's telemetry push, for instance) are forwarded too: after the Node
    /// app answers 101, bytes are copied both ways until either side closes.
    pub async fn forward(&self, mut req: Request) -> Response {
        self.requests.fetch_add(1, Ordering::Relaxed);
        let pq = req.uri().path_and_query().map(|x| x.as_str()).unwrap_or("/").to_string();
        match pq.parse::<Uri>() {
            Ok(u) => *req.uri_mut() = u,
            Err(_) => return json_error(StatusCode::BAD_REQUEST, "BAD_REQUEST", "invalid request target"),
        }
        let upgrade = if is_upgrade(req.headers()) { req.headers().get(header::UPGRADE).cloned() } else { None };
        let on_client = upgrade.as_ref().map(|_| hyper::upgrade::on(&mut req));
        let peer = req.extensions().get::<ConnectInfo<SocketAddr>>().map(|c| c.0.ip());
        let h = req.headers_mut();
        strip_hop_by_hop(h);
        if let Some(proto) = upgrade {
            h.insert(header::UPGRADE, proto);
            h.insert(header::CONNECTION, HeaderValue::from_static("upgrade"));
        }
        if let Some(host) = h.get(header::HOST).cloned() {
            h.insert(HeaderName::from_static("x-forwarded-host"), host); // Host itself is kept: same-origin checks keep working
        }
        h.insert(HeaderName::from_static("x-forwarded-proto"), HeaderValue::from_static("http"));
        if let Some(ip) = peer {
            let xff = match h.get("x-forwarded-for").and_then(|v| v.to_str().ok()) {
                Some(prev) => format!("{prev}, {ip}"),
                None => ip.to_string(),
            };
            if let Ok(v) = HeaderValue::from_str(&xff) {
                h.insert(HeaderName::from_static("x-forwarded-for"), v);
            }
        }
        match tokio::time::timeout(Duration::from_secs(30), self.send(req)).await {
            Ok(Ok(mut resp)) => {
                if let (StatusCode::SWITCHING_PROTOCOLS, Some(on_client)) = (resp.status(), on_client) {
                    let on_upstream = hyper::upgrade::on(&mut resp);
                    tokio::spawn(async move {
                        match tokio::try_join!(on_client, on_upstream) {
                            Ok((client, upstream)) => {
                                let _ = tokio::io::copy_bidirectional(&mut TokioIo::new(client), &mut TokioIo::new(upstream)).await;
                            }
                            Err(e) => tracing::debug!(error = %e, "legacy_upgrade_failed"),
                        }
                    });
                    let (parts, _) = resp.into_parts(); // 101 keeps its Connection/Upgrade headers
                    return Response::from_parts(parts, Body::empty());
                }
                let (mut parts, body) = resp.into_parts();
                strip_hop_by_hop(&mut parts.headers);
                Response::from_parts(parts, Body::new(body))
            }
            Ok(Err(e)) => {
                self.failures.fetch_add(1, Ordering::Relaxed);
                tracing::warn!(error = %e, "legacy_proxy_error");
                json_error(StatusCode::BAD_GATEWAY, "UPSTREAM_UNAVAILABLE", "legacy backend unreachable")
            }
            Err(_) => {
                self.failures.fetch_add(1, Ordering::Relaxed);
                json_error(StatusCode::GATEWAY_TIMEOUT, "UPSTREAM_TIMEOUT", "legacy backend did not answer in time")
            }
        }
    }

    /// The legacy application's capabilities (empty for a shipped release without
    /// /api/capabilities); cached for 5 s.
    pub async fn capabilities(&self) -> LegacyCaps {
        if let Some((at, c)) = self.caps.lock().unwrap().as_ref() {
            if at.elapsed() < Duration::from_secs(5) {
                return c.clone();
            }
        }
        let mut caps = LegacyCaps::default();
        let req = Request::get("/api/capabilities").body(Body::empty()).expect("valid request");
        if let Ok(Ok(resp)) = tokio::time::timeout(Duration::from_secs(1), self.send(req)).await {
            let ok = resp.status() == StatusCode::OK;
            if let Ok(Ok(body)) = tokio::time::timeout(Duration::from_secs(1), resp.into_body().collect()).await {
                if ok {
                    if let Ok(v) = serde_json::from_slice::<Value>(&body.to_bytes()) {
                        caps.features = v.get("features").and_then(|f| f.as_object()).cloned().unwrap_or_default();
                        caps.limits = v.get("limits").and_then(|f| f.as_object()).cloned().unwrap_or_default();
                        caps.server = v.get("server").and_then(|s| s.as_str()).map(str::to_string);
                    }
                }
            }
        }
        *self.caps.lock().unwrap() = Some((Instant::now(), caps.clone()));
        caps
    }
}
