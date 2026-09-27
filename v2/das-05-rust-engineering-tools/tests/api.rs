//! End-to-end tests: the real router on a loopback port, the simulator as
//! hardware, requests over real sockets.

use std::collections::HashMap;
use std::io::Read;
use std::net::SocketAddr;
use std::time::{Duration, Instant};

use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::routing::get;
use axum::Router;
use clap::Parser;
use futures_util::{SinkExt, StreamExt};
use http_body_util::BodyExt;
use hyper_util::client::legacy::{connect::HttpConnector, Client};
use hyper_util::rt::TokioExecutor;
use serde_json::Value;
use tokio_tungstenite::tungstenite::{client::IntoClientRequest, Message};

use das_engtools::app::App;
use das_engtools::config::Config;
use das_engtools::dspc;
use das_engtools::http::router;

struct Resp {
    status: StatusCode,
    headers: axum::http::HeaderMap,
    body: Vec<u8>,
}

impl Resp {
    fn json(&self) -> Value {
        serde_json::from_slice(&self.body).unwrap_or_else(|e| panic!("not JSON ({e}): {}", String::from_utf8_lossy(&self.body[..self.body.len().min(200)])))
    }
    fn header(&self, name: &str) -> String {
        self.headers.get(name).and_then(|v| v.to_str().ok()).unwrap_or("").to_string()
    }
}

async fn start(args: &[&str]) -> SocketAddr {
    let mut argv = vec!["das-engtools", "--sweep-time-ms", "30", "--s11-time-ms", "30", "--points", "2001", "--prewarm", "0"];
    argv.extend_from_slice(args);
    let cfg = Config::parse_from(argv);
    let app = App::new(cfg, None).unwrap();
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(l, router(app).into_make_service_with_connect_info::<SocketAddr>()).await.unwrap();
    });
    addr
}

async fn get_req(addr: SocketAddr, path: &str, headers: &[(&str, &str)]) -> Resp {
    let client: Client<HttpConnector, Body> = Client::builder(TokioExecutor::new()).build(HttpConnector::new());
    let mut b = Request::get(format!("http://{addr}{path}"));
    for (k, v) in headers {
        b = b.header(*k, *v);
    }
    let resp = client.request(b.body(Body::empty()).unwrap()).await.unwrap();
    let status = resp.status();
    let headers = resp.headers().clone();
    let mut body = resp.into_body().collect().await.unwrap().to_bytes().to_vec();
    if headers.get("content-encoding").map(|v| v == "gzip").unwrap_or(false) {
        let mut out = Vec::new();
        flate2::read::GzDecoder::new(&body[..]).read_to_end(&mut out).unwrap();
        body = out;
    }
    Resp { status, headers, body }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn legacy_spectrum_keeps_the_shipped_shape_and_semantics() {
    let addr = start(&[]).await;
    let a = get_req(addr, "/api/spectrum?nodeId=1&port=1&points=2001", &[("accept-encoding", "gzip")]).await;
    assert_eq!(a.status, 200);
    assert_eq!(a.header("content-encoding"), "gzip");
    assert!(a.header("cache-control").contains("no-store"));
    let j = a.json();
    for k in ["sweepId", "nodeId", "port", "timestamp", "startHz", "stopHz", "rbwHz", "points"] {
        assert!(j.get(k).is_some(), "missing {k}");
    }
    assert_eq!(j["points"].as_array().unwrap().len(), 2001);
    assert_eq!(j["points"][0]["frequency"], 700_000_000);
    assert!(j["points"][0]["power"].is_number());
    let b = get_req(addr, "/api/spectrum?nodeId=1&port=1&points=2001", &[]).await;
    assert_eq!(b.header("content-encoding"), "");
    assert!(b.json()["sweepId"].as_u64() > j["sweepId"].as_u64(), "each poll returns a newer sweep");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn latest_json_binary_decimation_304_and_long_poll() {
    let addr = start(&[]).await;
    let q = "/api/spectrum/latest?nodeId=1&port=1&points=2001";
    let j = get_req(addr, q, &[("accept", "application/json")]).await.json();
    assert_eq!(j["count"], 2001);
    assert!((j["stepHz"].as_f64().unwrap() - 2e9 / 2000.0).abs() < 1.0);

    let full = get_req(addr, q, &[("accept", dspc::CONTENT_TYPE)]).await;
    assert_eq!(full.header("content-type"), dspc::CONTENT_TYPE);
    let (fm, fp) = dspc::decode(&full.body).unwrap();
    assert_eq!((fp.len(), fm.node_id, fm.port), (2001, 1, 1));

    let dq = format!("{q}&maxPoints=256");
    let dec = get_req(addr, &dq, &[("accept", dspc::CONTENT_TYPE)]).await;
    let (dm, dp) = dspc::decode(&dec.body).unwrap();
    assert!(dm.decimated && dp.len() <= 256);
    if dm.sweep_id == fm.sweep_id {
        let max = |p: &[f32]| p.iter().cloned().filter(|v| !v.is_nan()).fold(f32::MIN, f32::max);
        assert!((max(&fp) - max(&dp)).abs() < 0.011, "peak lost");
    }

    let tag = dec.header("etag");
    let nm = get_req(addr, &dq, &[("accept", dspc::CONTENT_TYPE), ("if-none-match", &tag)]).await;
    assert!(nm.status == 304 || nm.header("x-sweep-id") != dec.header("x-sweep-id"));

    let t = Instant::now();
    let lp = get_req(addr, &format!("{dq}&waitMs=3000"), &[("accept", dspc::CONTENT_TYPE), ("if-none-match", &tag)]).await;
    assert_eq!(lp.status, 200);
    assert!(dspc::decode(&lp.body).unwrap().0.sweep_id > dm.sweep_id);
    assert!(t.elapsed() < Duration::from_secs(2), "long-poll returns as soon as the next sweep completes");

    let bad = get_req(addr, "/api/spectrum/latest?startHz=2e9&stopHz=1e9", &[]).await;
    assert_eq!(bad.status, 400);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn distance_to_fault_finds_the_simulated_feeder() {
    let addr = start(&[]).await;
    let r = get_req(addr, "/api/dtf?nodeId=3&port=1", &[]).await;
    assert_eq!(r.status, 200, "{}", String::from_utf8_lossy(&r.body));
    let j = r.json();
    let events = j["events"].as_array().unwrap();
    let res = j["resolutionM"].as_f64().unwrap();
    for refl in das_engtools::sim::feeder_for(3, 1) {
        let rl = -20.0 * refl.rho.log10();
        if rl >= 35.0 {
            continue;
        }
        let e = events.iter().find(|e| (e["distanceM"].as_f64().unwrap() - refl.distance_m).abs() <= 2.0 * res);
        let e = e.unwrap_or_else(|| panic!("no event near {} m in {events:?}", refl.distance_m));
        assert!((e["returnLossDb"].as_f64().unwrap() - rl).abs() < 1.5, "RL at {} m: {} vs {rl}", refl.distance_m, e["returnLossDb"]);
        assert_eq!(e["fault"], rl < 20.0);
    }
    assert_eq!(j["count"].as_u64().unwrap() as usize, j["returnLossDb"].as_array().unwrap().len());
    let tag = r.header("etag");
    let nm = get_req(addr, "/api/dtf?nodeId=3&port=1", &[("if-none-match", &tag)]).await;
    assert!(nm.status == 304 || nm.header("etag") != tag);
    assert_eq!(get_req(addr, "/api/dtf?velocityFactor=2", &[]).await.status, 400);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn websocket_pushes_decimated_frames_and_checks_origin() {
    let addr = start(&[]).await;
    let url = format!("ws://{addr}/api/ws/spectrum");
    let (mut ws, _) = tokio_tungstenite::connect_async(&url).await.unwrap();
    let hello = ws.next().await.unwrap().unwrap();
    assert!(hello.to_text().unwrap().contains("\"hello\""));
    ws.send(Message::Text(r#"{"type":"sub","nodeId":2,"port":1,"points":2001,"maxPoints":512}"#.into())).await.unwrap();
    let mut last = 0;
    let mut frames = 0;
    while frames < 3 {
        match tokio::time::timeout(Duration::from_secs(5), ws.next()).await.unwrap().unwrap().unwrap() {
            Message::Binary(b) => {
                let (m, p) = dspc::decode(&b).unwrap();
                assert!(m.node_id == 2 && m.decimated && p.len() <= 512);
                assert!(m.sweep_id > last);
                last = m.sweep_id;
                frames += 1;
            }
            Message::Text(t) => assert!(t.contains("subscribed") || t.contains("hb"), "{t}"),
            _ => {}
        }
    }
    let mut req = url.clone().into_client_request().unwrap();
    req.headers_mut().insert("origin", "https://evil.example".parse().unwrap());
    let err = tokio_tungstenite::connect_async(req).await.unwrap_err();
    assert!(err.to_string().contains("403"), "{err}");
}

async fn fake_legacy(with_capabilities: bool) -> SocketAddr {
    let mut r = Router::new().route("/api/heartbeat", get(|| async { ([("cache-control", "no-store")], r#"{"status":"ok","server":"das-legacy"}"#) })).route(
        "/api/echo",
        get(|req: Request<Body>| async move {
            let h = |n: &str| req.headers().get(n).and_then(|v| v.to_str().ok()).unwrap_or("").to_string();
            serde_json::json!({ "host": h("host"), "xfh": h("x-forwarded-host"), "xff": h("x-forwarded-for"), "path": req.uri().to_string() }).to_string()
        }),
    );
    r = r.route(
        "/api/ws/telemetry",
        get(|ws: axum::extract::ws::WebSocketUpgrade| async move {
            ws.on_upgrade(|mut socket| async move {
                while let Some(Ok(axum::extract::ws::Message::Text(t))) = socket.recv().await {
                    let _ = socket.send(axum::extract::ws::Message::Text(format!("echo:{}", t.as_str()).into())).await;
                }
            })
        }),
    );
    if with_capabilities {
        r = r.route(
            "/api/capabilities",
            get(|| async { r#"{"api":"das-v1","server":"node-hotfix","features":{"etag":true,"volatileDelta":true,"wsTelemetry":true}}"# }),
        );
    }
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move { axum::serve(l, r).await.unwrap() });
    addr
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn front_mode_forwards_everything_else_and_merges_capabilities() {
    for with_caps in [false, true] {
        let legacy = fake_legacy(with_caps).await;
        let url = format!("http://{legacy}");
        let addr = start(&["--legacy", &url]).await;
        let hb = get_req(addr, "/api/heartbeat", &[]).await;
        assert_eq!(hb.json()["server"], "das-legacy", "the Node app keeps answering the heartbeat");
        let echo = get_req(addr, "/api/echo?x=1", &[]).await.json();
        assert_eq!(echo["host"], addr.to_string(), "the Node app sees the browser's Host");
        assert_eq!(echo["xfh"], addr.to_string());
        assert_eq!(echo["xff"], "127.0.0.1");
        assert_eq!(echo["path"], "/api/echo?x=1");
        let caps = get_req(addr, "/api/capabilities", &[]).await.json();
        let f: HashMap<String, bool> = serde_json::from_value(caps["features"].clone()).unwrap();
        assert!(f["spectrumLatest"] && f["wsSpectrum"] && f["dtf"]);
        assert_eq!(f.get("volatileDelta").copied().unwrap_or(false), with_caps, "legacy features are relayed");
        assert_eq!(f.get("wsTelemetry").copied().unwrap_or(false), with_caps, "the Node app's WebSocket is forwarded");
        // A WebSocket of the Node app, through the front.
        let (mut ws, resp) = tokio_tungstenite::connect_async(format!("ws://{addr}/api/ws/telemetry")).await.expect("upgrade through the front");
        assert_eq!(resp.status(), 101);
        ws.send(Message::Text("ping".into())).await.unwrap();
        let echo = tokio::time::timeout(Duration::from_secs(2), ws.next()).await.unwrap().unwrap().unwrap();
        assert_eq!(echo, Message::Text("echo:ping".into()));
        let sp = get_req(addr, "/api/spectrum/latest?points=2001", &[]).await;
        assert_eq!(sp.status, 200, "engineering paths stay here");
    }
    let dead = start(&["--legacy", "http://127.0.0.1:9"]).await;
    let r = get_req(dead, "/api/anything", &[]).await;
    assert_eq!(r.status, 502);
    assert_eq!(r.json()["error"], "UPSTREAM_UNAVAILABLE");
    let side = start(&["--legacy", "http://127.0.0.1:9", "--sidecar"]).await;
    assert_eq!(get_req(side, "/api/anything", &[]).await.status, 404, "sidecar mode forwards nothing");
}

/// The das-02 gateway layout: core-api (Node.js) on a Unix socket, das-engtools on
/// the spectrum socket in sidecar mode, asking core-api for its capabilities.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn legacy_over_a_unix_socket() {
    let dir = std::env::temp_dir().join(format!("das-engtools-test-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let sock = dir.join("core-api.sock");
    let _ = std::fs::remove_file(&sock);
    let r = Router::new()
        .route("/api/heartbeat", get(|| async { r#"{"status":"ok","server":"das-core-api"}"# }))
        .route("/api/echo", get(|req: Request<Body>| async move { req.uri().to_string() }))
        .route(
            "/api/capabilities",
            get(|| async {
                r#"{"api":"das-v1","server":"core-api","features":{"volatileDelta":true,"wsTelemetry":true},"limits":{"recommendedPollMs":{"heartbeat":2000}}}"#
            }),
        );
    let l = tokio::net::UnixListener::bind(&sock).unwrap();
    tokio::spawn(async move { axum::serve(l, r).await.unwrap() });
    let url = format!("unix:{}", sock.display());

    let side = start(&["--sidecar", "--legacy", &url]).await;
    let caps = get_req(side, "/api/capabilities", &[]).await.json();
    assert_eq!(caps["server"], "das-engtools + core-api");
    assert_eq!(caps["features"]["volatileDelta"], true, "core-api's features are relayed");
    assert_eq!(caps["features"]["wsTelemetry"], true, "the gateway routes that WebSocket, not das-engtools");
    assert_eq!(caps["features"]["dtf"], true);
    assert_eq!(caps["limits"]["recommendedPollMs"]["heartbeat"], 2000, "core-api's limits are kept");
    assert_eq!(caps["limits"]["maxDtfPoints"], 16001);
    assert_eq!(get_req(side, "/api/echo", &[]).await.status, 404, "sidecar mode forwards nothing");

    let front = start(&["--legacy", &url]).await;
    assert_eq!(get_req(front, "/api/heartbeat", &[]).await.json()["server"], "das-core-api");
    assert_eq!(get_req(front, "/api/echo?x=1", &[]).await.body, b"/api/echo?x=1");
    let _ = std::fs::remove_dir_all(&dir);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn metrics_and_health() {
    let addr = start(&[]).await;
    get_req(addr, "/api/spectrum/latest?points=2001", &[]).await;
    let h = get_req(addr, "/internal/health", &[]).await.json();
    assert_eq!(h["status"], "ok");
    let m = get_req(addr, "/internal/metrics", &[]).await.json();
    assert!(m["dsp"]["jobs"].as_u64().unwrap() >= 1);
    assert_eq!(m["routes"]["spectrum-latest"]["requests"], 1);
    let p = String::from_utf8(get_req(addr, "/internal/metrics?format=prom", &[]).await.body).unwrap();
    assert!(p.contains("das_http_requests_total{route=\"spectrum-latest\"} 1"), "{p}");
    assert!(p.contains("das_engtools_dsp_workers"));
}
