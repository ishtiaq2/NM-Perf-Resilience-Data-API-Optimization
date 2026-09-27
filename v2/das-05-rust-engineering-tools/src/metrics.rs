//! Per-route counters and process statistics, as JSON and Prometheus text.

use std::sync::atomic::{AtomicU64, Ordering};

use serde_json::{json, Map, Value};

/// Routes that are counted.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Route {
    SpectrumLegacy,
    SpectrumLatest,
    WsSpectrum,
    Dtf,
    Capabilities,
    Heartbeat,
    Proxy,
    Internal,
}

const ROUTES: [(Route, &str); 8] = [
    (Route::SpectrumLegacy, "spectrum-legacy"),
    (Route::SpectrumLatest, "spectrum-latest"),
    (Route::WsSpectrum, "ws-spectrum"),
    (Route::Dtf, "dtf"),
    (Route::Capabilities, "capabilities"),
    (Route::Heartbeat, "heartbeat"),
    (Route::Proxy, "proxy"),
    (Route::Internal, "internal"),
];

#[derive(Default)]
struct Counter {
    requests: AtomicU64,
    not_modified: AtomicU64,
    errors: AtomicU64,
    bytes: AtomicU64,
}

/// Route counters plus a few service-wide ones.
#[derive(Default)]
pub struct Metrics {
    routes: [Counter; 8],
    pub ws_clients: AtomicU64,
    pub ws_frames: AtomicU64,
    pub ws_rejected: AtomicU64,
    pub proxy_failures: AtomicU64,
}

impl Metrics {
    pub fn count(&self, r: Route, status: u16, bytes: usize) {
        let c = &self.routes[r as usize];
        c.requests.fetch_add(1, Ordering::Relaxed);
        c.bytes.fetch_add(bytes as u64, Ordering::Relaxed);
        if status == 304 {
            c.not_modified.fetch_add(1, Ordering::Relaxed);
        } else if status >= 500 {
            c.errors.fetch_add(1, Ordering::Relaxed);
        }
    }

    pub fn routes_json(&self) -> Value {
        let mut m = Map::new();
        for (r, name) in ROUTES {
            let c = &self.routes[r as usize];
            m.insert(
                name.to_string(),
                json!({
                    "requests": c.requests.load(Ordering::Relaxed),
                    "notModified": c.not_modified.load(Ordering::Relaxed),
                    "errors": c.errors.load(Ordering::Relaxed),
                    "bytes": c.bytes.load(Ordering::Relaxed),
                }),
            );
        }
        Value::Object(m)
    }

    /// Prometheus text exposition of the route counters and numeric leaves of `extra`.
    pub fn prometheus(&self, extra: &Value) -> String {
        let mut out = String::new();
        for (metric, field) in [
            ("das_http_requests_total", "requests"),
            ("das_http_not_modified_total", "notModified"),
            ("das_http_errors_total", "errors"),
            ("das_http_response_bytes_total", "bytes"),
        ] {
            out.push_str(&format!("# TYPE {metric} counter\n"));
            let routes = self.routes_json();
            for (name, v) in routes.as_object().unwrap() {
                out.push_str(&format!("{metric}{{route=\"{name}\"}} {}\n", v[field]));
            }
        }
        flatten("das_engtools", extra, &mut out);
        out
    }
}

fn snake(k: &str) -> String {
    let mut s = String::new();
    let b = k.as_bytes();
    for (i, &c) in b.iter().enumerate() {
        if c.is_ascii_uppercase() {
            if i > 0 && (b[i - 1].is_ascii_lowercase() || b[i - 1].is_ascii_digit()) {
                s.push('_');
            }
            s.push(c.to_ascii_lowercase() as char);
        } else if c.is_ascii_alphanumeric() {
            s.push(c as char);
        } else {
            s.push('_');
        }
    }
    s
}

fn flatten(prefix: &str, v: &Value, out: &mut String) {
    match v {
        Value::Object(m) => {
            let mut keys: Vec<_> = m.keys().collect();
            keys.sort();
            for k in keys {
                flatten(&format!("{prefix}_{}", snake(k)), &m[k], out);
            }
        }
        Value::Number(n) => out.push_str(&format!("# TYPE {prefix} gauge\n{prefix} {n}\n")),
        Value::Bool(b) => out.push_str(&format!("# TYPE {prefix} gauge\n{prefix} {}\n", u8::from(*b))),
        _ => {}
    }
}

/// Resident memory and CPU time from /proc (Linux; zeros elsewhere).
pub fn process_stats() -> Value {
    let mut rss_mb = 0.0;
    let mut threads = 0u64;
    if let Ok(s) = std::fs::read_to_string("/proc/self/status") {
        for line in s.lines() {
            if let Some(v) = line.strip_prefix("VmRSS:") {
                rss_mb = v.trim().trim_end_matches("kB").trim().parse::<f64>().unwrap_or(0.0) / 1024.0;
            } else if let Some(v) = line.strip_prefix("Threads:") {
                threads = v.trim().parse().unwrap_or(0);
            }
        }
    }
    let mut cpu_seconds = 0.0;
    if let Ok(s) = std::fs::read_to_string("/proc/self/stat") {
        if let Some(i) = s.rfind(')') {
            let f: Vec<&str> = s[i + 2..].split_whitespace().collect();
            if f.len() > 12 {
                let ticks = f[11].parse::<f64>().unwrap_or(0.0) + f[12].parse::<f64>().unwrap_or(0.0);
                cpu_seconds = ticks / 100.0; // USER_HZ
            }
        }
    }
    json!({ "rssMB": (rss_mb * 10.0).round() / 10.0, "threads": threads, "cpuSeconds": (cpu_seconds * 10.0f64).round() / 10.0 })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn prometheus_text() {
        let m = Metrics::default();
        m.count(Route::Dtf, 200, 10);
        m.count(Route::Dtf, 304, 0);
        let t = m.prometheus(&json!({ "dsp": { "busySeconds": 1.5, "workers": 1 } }));
        assert!(t.contains("das_http_requests_total{route=\"dtf\"} 2"));
        assert!(t.contains("das_http_not_modified_total{route=\"dtf\"} 1"));
        assert!(t.contains("das_engtools_dsp_busy_seconds 1.5"));
    }
}
