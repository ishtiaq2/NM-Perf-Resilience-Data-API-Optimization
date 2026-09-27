//! Content negotiation and conditional-request helpers (RFC 9110).

use axum::http::header::{ACCEPT, ACCEPT_ENCODING, HOST, IF_NONE_MATCH, ORIGIN};
use axum::http::{HeaderMap, HeaderName};

fn q_list<'a>(h: &'a HeaderMap, name: HeaderName) -> impl Iterator<Item = (String, f64)> + 'a {
    h.get_all(name).into_iter().filter_map(|v| v.to_str().ok()).flat_map(|v| {
        v.split(',').filter_map(|part| {
            let mut seg = part.split(';');
            let value = seg.next()?.trim().to_ascii_lowercase();
            if value.is_empty() {
                return None;
            }
            let mut q = 1.0;
            for p in seg {
                if let Some((k, val)) = p.split_once('=') {
                    if k.trim() == "q" {
                        q = val.trim().parse::<f64>().unwrap_or(0.0);
                    }
                }
            }
            Some((value, q))
        })
    })
}

/// An explicit entry for `enc` decides; otherwise `*`.
pub fn accepts_encoding(h: &HeaderMap, enc: &str) -> bool {
    let mut star = None;
    for (v, q) in q_list(h, ACCEPT_ENCODING) {
        if v == enc {
            return q > 0.0;
        }
        if v == "*" {
            star = Some(q);
        }
    }
    star.is_some_and(|q| q > 0.0)
}

/// True only when the client explicitly asks for `media_type` (opt-in formats).
pub fn accepts_media_type(h: &HeaderMap, media_type: &str) -> bool {
    q_list(h, ACCEPT).any(|(v, q)| v == media_type && q > 0.0)
}

/// If-None-Match with weak comparison.
pub fn if_none_match(h: &HeaderMap, etag: &str) -> bool {
    let Some(v) = h.get(IF_NONE_MATCH).and_then(|v| v.to_str().ok()) else { return false };
    if v.trim() == "*" {
        return true;
    }
    let want = etag.trim_start_matches("W/");
    v.split(',').any(|t| t.trim().trim_start_matches("W/") == want)
}

/// WebSocket handshakes from browsers must be same-origin (CSWSH protection);
/// tools without an Origin header are allowed.
pub fn same_origin(h: &HeaderMap) -> bool {
    let Some(origin) = h.get(ORIGIN).and_then(|v| v.to_str().ok()) else { return true };
    let Some(host) = h.get(HOST).and_then(|v| v.to_str().ok()) else { return false };
    match origin.split_once("://") {
        Some((_, rest)) => rest.eq_ignore_ascii_case(host),
        None => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderValue;

    fn h(name: HeaderName, v: &str) -> HeaderMap {
        let mut m = HeaderMap::new();
        m.insert(name, HeaderValue::from_str(v).unwrap());
        m
    }

    #[test]
    fn encodings() {
        for (v, want) in
            [("gzip", true), ("gzip;q=0", false), ("*", true), ("br, *;q=0.1", true), ("gzip;q=0, *", false), ("identity", false), ("GZIP;q=0.5", true)]
        {
            assert_eq!(accepts_encoding(&h(ACCEPT_ENCODING, v), "gzip"), want, "{v}");
        }
    }

    #[test]
    fn conditional_and_origin() {
        assert!(if_none_match(&h(IF_NONE_MATCH, "W/\"a\", \"b\""), "\"a\""));
        assert!(!if_none_match(&h(IF_NONE_MATCH, "\"b\""), "\"a\""));
        let mut m = h(ORIGIN, "https://master.local:8443");
        m.insert(HOST, HeaderValue::from_static("master.local:8443"));
        assert!(same_origin(&m));
        m.insert(ORIGIN, HeaderValue::from_static("https://evil.example"));
        assert!(!same_origin(&m));
        assert!(same_origin(&HeaderMap::new()));
        assert!(accepts_media_type(&h(ACCEPT, "application/vnd.das.spectrum, application/json;q=0.5"), "application/vnd.das.spectrum"));
    }
}
