//! `/api/ws/spectrum` (das-v1 AsyncAPI): one binary DSPC frame per sweep,
//! decimated to the width the client asks for.
//!
//! Each client is one task. It waits on the session's watch channel, which only
//! ever holds the newest sweep: while a slow client is still receiving a frame,
//! intermediate sweeps are skipped (conflated), so it costs O(1) memory and
//! never delays anyone else.

use std::collections::HashMap;
use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::{Duration, Instant};

use axum::extract::ws::{Message, WebSocket, WebSocketUpgrade};
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use serde::Deserialize;
use serde_json::json;
use tokio::sync::watch;

use crate::app::{AppState, SERVER};
use crate::decimate::normalise_max_points;
use crate::http::neg::same_origin;
use crate::metrics::Route;
use crate::session::Session;
use crate::spectrum::{Kind, SpectrumMeasure, SpectrumParams, Sweep, VariantKey};
use crate::util::now_ms;

const HEARTBEAT: Duration = Duration::from_secs(5);

#[derive(Deserialize, Default)]
#[serde(rename_all = "camelCase")]
struct ClientMsg {
    #[serde(rename = "type")]
    ty: String,
    node_id: Option<f64>,
    port: Option<f64>,
    start_hz: Option<f64>,
    stop_hz: Option<f64>,
    points: Option<f64>,
    max_points: Option<f64>,
    encoding: Option<String>,
    id: Option<i64>,
}

struct Sub {
    session: Arc<Session<SpectrumMeasure>>,
    rx: watch::Receiver<Option<Arc<Sweep>>>,
    key: VariantKey,
    last: u32,
}

pub async fn ws_spectrum(State(app): State<AppState>, headers: HeaderMap, ws: WebSocketUpgrade) -> Response {
    app.metrics.count(Route::WsSpectrum, 101, 0);
    if !same_origin(&headers) {
        return (StatusCode::FORBIDDEN, "cross-origin websocket refused").into_response();
    }
    if app.ws_clients.fetch_add(1, Ordering::AcqRel) >= app.cfg.max_ws {
        app.ws_clients.fetch_sub(1, Ordering::AcqRel);
        app.metrics.ws_rejected.fetch_add(1, Ordering::Relaxed);
        return (StatusCode::SERVICE_UNAVAILABLE, [("retry-after", "2")], "too many clients").into_response();
    }
    let failed = app.clone();
    ws.max_message_size(4096)
        .on_failed_upgrade(move |_| {
            failed.ws_clients.fetch_sub(1, Ordering::AcqRel);
        })
        .on_upgrade(move |socket| async move {
            app.metrics.ws_clients.fetch_add(1, Ordering::Relaxed);
            run(socket, app.clone()).await;
            app.metrics.ws_clients.fetch_sub(1, Ordering::Relaxed);
            app.ws_clients.fetch_sub(1, Ordering::AcqRel);
        })
}

fn text(s: String) -> Message {
    Message::Text(s.into())
}

async fn changed(sub: &mut Option<Sub>) -> Result<(), watch::error::RecvError> {
    match sub {
        Some(s) => s.rx.changed().await,
        None => std::future::pending().await,
    }
}

/// Handles one text message; returns the reply and, for "sub"/"unsub", the new subscription.
fn handle(app: &AppState, raw: &str) -> (Option<Message>, Option<Option<Sub>>) {
    let m: ClientMsg = match serde_json::from_str(raw) {
        Ok(m) => m,
        Err(_) => return (Some(text(json!({"type":"error","code":"BAD_REQUEST","message":"expected a JSON object with a type"}).to_string())), None),
    };
    match m.ty.as_str() {
        "sub" => {
            let mut q = HashMap::new();
            for (k, v) in [("nodeId", m.node_id), ("port", m.port), ("startHz", m.start_hz), ("stopHz", m.stop_hz), ("points", m.points)] {
                if let Some(v) = v {
                    q.insert(k.to_string(), format!("{v}"));
                }
            }
            let p = match SpectrumParams::parse(&q, app.cfg.points) {
                Ok(p) => p,
                Err(e) => return (Some(text(json!({"type":"error","code":"BAD_REQUEST","message":e}).to_string())), None),
            };
            let session = match app.spectrum.session(p.clone()) {
                Ok(s) => s,
                Err(_) => return (Some(text(json!({"type":"error","code":"TOO_MANY_SESSIONS"}).to_string())), None),
            };
            let kind = if m.encoding.as_deref() == Some("f32") { Kind::F32 } else { Kind::I16 };
            let max_points = normalise_max_points(m.max_points.unwrap_or(0.0) as i64);
            let mut rx = session.subscribe();
            rx.mark_changed(); // send the current sweep right away, if there is one
            let reply = json!({"type":"subscribed","nodeId":p.node_id,"port":p.port,"maxPoints":max_points}).to_string();
            (Some(text(reply)), Some(Some(Sub { session, rx, key: VariantKey { kind, max_points, gzip: false }, last: 0 })))
        }
        "unsub" => (None, Some(None)),
        "ping" => (Some(text(json!({"type":"pong","id":m.id.unwrap_or(0),"time":now_ms()}).to_string())), None),
        _ => (Some(text(json!({"type":"error","code":"BAD_REQUEST","message":"unknown type"}).to_string())), None),
    }
}

async fn run(mut socket: WebSocket, app: AppState) {
    let hello = json!({"type":"hello","api":"das-v1","server":SERVER,"topic":"spectrum","heartbeatMs":HEARTBEAT.as_millis() as u64,"time":now_ms()});
    if socket.send(text(hello.to_string())).await.is_err() {
        return;
    }
    let mut sub: Option<Sub> = None;
    let mut tick = tokio::time::interval(Duration::from_secs(1));
    let mut last_send = Instant::now();
    loop {
        tokio::select! {
            msg = socket.recv() => {
                let Some(Ok(msg)) = msg else { break };
                match msg {
                    Message::Text(t) => {
                        let (reply, new_sub) = handle(&app, t.as_str());
                        if let Some(s) = new_sub {
                            sub = s;
                        }
                        if let Some(r) = reply {
                            if socket.send(r).await.is_err() {
                                break;
                            }
                            last_send = Instant::now();
                        }
                    }
                    Message::Close(_) => break,
                    _ => {} // ping/pong are answered by the library; binary input is ignored
                }
            }
            res = changed(&mut sub) => {
                if res.is_err() {
                    sub = None; // session dropped
                    continue;
                }
                let Some(s) = sub.as_mut() else { continue };
                let newest = s.rx.borrow_and_update().clone();
                let Some(sw) = newest else { continue };
                if sw.id == s.last {
                    continue;
                }
                let key = s.key;
                s.last = sw.id;
                if let Ok(v) = sw.variant(&app.dsp, key).await {
                    if socket.send(Message::Binary(v.body.clone())).await.is_err() {
                        break;
                    }
                    app.metrics.ws_frames.fetch_add(1, Ordering::Relaxed);
                    last_send = Instant::now();
                }
            }
            _ = tick.tick() => {
                if let Some(s) = &sub {
                    s.session.touch(&app.spectrum); // an open analyzer view keeps its session sweeping
                }
                if last_send.elapsed() >= HEARTBEAT {
                    if socket.send(text(json!({"type":"hb","time":now_ms(),"rev":0}).to_string())).await.is_err() {
                        break;
                    }
                    last_send = Instant::now();
                }
            }
        }
    }
}
