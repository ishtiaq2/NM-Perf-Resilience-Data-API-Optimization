//! Single-flight measurement sessions.
//!
//! One session exists per measurement configuration (node, port, range, points,
//! ...). While anyone watches it (HTTP polls, long-polls, WebSocket
//! subscribers), it runs the hardware measurement in a loop and publishes each
//! result once, to everyone. Ten engineers looking at the same trace cost one
//! sweep, not ten. Nobody watching for `idle`: the session stops sweeping.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use tokio::sync::watch;

use crate::hw::BoxFut;
use crate::util::fnv1a32;

/// A kind of hardware measurement.
pub trait Measure: Send + Sync + 'static {
    type Params: Clone + Send + Sync + 'static;
    type Output: Send + Sync + 'static;
    /// Session key: equal keys share one session.
    fn key(p: &Self::Params) -> String;
    /// One measurement: hardware acquisition, then parsing/processing.
    /// `tag` is the ETag base of the result (unique per boot, session and id).
    fn measure<'a>(&'a self, p: &'a Self::Params, id: u32, tag: String) -> BoxFut<'a, Result<Self::Output, String>>;
    /// Monotonic id of a result within its session.
    fn id(o: &Self::Output) -> u32;
}

/// Every session slot is busy.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TooManySessions;

impl std::fmt::Display for TooManySessions {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("too many concurrent analyzer sessions")
    }
}

/// Session tuning.
#[derive(Debug, Clone, Copy)]
pub struct Limits {
    /// Stop measuring when nobody asked for this long.
    pub idle: Duration,
    /// Minimum time between the starts of two measurements (protects the hardware).
    pub min_interval: Duration,
    /// Maximum concurrent configurations.
    pub max_sessions: usize,
    /// Longest a single hardware measurement may take.
    pub timeout: Duration,
}

pub struct Session<M: Measure> {
    pub key: String,
    pub params: M::Params,
    tag_base: String,
    tx: watch::Sender<Option<Arc<M::Output>>>,
    last_access: Mutex<Instant>,
    running: AtomicBool,
    seq: AtomicU32,
    last_error: Mutex<Option<String>>,
}

pub struct Manager<M: Measure> {
    pub measure: M,
    limits: Limits,
    prefix: String,
    sessions: Mutex<HashMap<String, Arc<Session<M>>>>,
    closed: AtomicBool,
    pub measurements: AtomicU64,
    pub errors: AtomicU64,
}

impl<M: Measure> Manager<M> {
    /// `prefix` starts every ETag (e.g. `sp-<bootId>`), so tags never survive a restart.
    pub fn new(measure: M, prefix: String, limits: Limits) -> Arc<Self> {
        Arc::new(Manager {
            measure,
            limits,
            prefix,
            sessions: Mutex::new(HashMap::new()),
            closed: AtomicBool::new(false),
            measurements: AtomicU64::new(0),
            errors: AtomicU64::new(0),
        })
    }

    /// Returns (creating if needed) the session for `p` and marks it watched.
    pub fn session(self: &Arc<Self>, p: M::Params) -> Result<Arc<Session<M>>, TooManySessions> {
        let key = M::key(&p);
        let s = {
            let mut map = self.sessions.lock().unwrap();
            match map.get(&key) {
                Some(s) => s.clone(),
                None => {
                    if map.len() >= self.limits.max_sessions {
                        let idle = map.iter().find(|(_, s)| !s.running.load(Ordering::Acquire)).map(|(k, _)| k.clone());
                        if let Some(k) = idle {
                            map.remove(&k);
                        }
                    }
                    if map.len() >= self.limits.max_sessions {
                        return Err(TooManySessions);
                    }
                    let s = Arc::new(Session {
                        tag_base: format!("{}-{:x}", self.prefix, fnv1a32(key.as_bytes())),
                        key: key.clone(),
                        params: p,
                        tx: watch::channel(None).0,
                        last_access: Mutex::new(Instant::now()),
                        running: AtomicBool::new(false),
                        seq: AtomicU32::new(0),
                        last_error: Mutex::new(None),
                    });
                    map.insert(key, s.clone());
                    s
                }
            }
        };
        s.touch(self);
        Ok(s)
    }

    /// Stops all sessions (shutdown).
    pub fn close(&self) {
        self.closed.store(true, Ordering::Release);
    }

    pub fn stats(&self) -> Value {
        let list: Vec<Value> = self.sessions.lock().unwrap().values().map(|s| s.stats()).collect();
        json!({
            "sessions": list,
            "measurements": self.measurements.load(Ordering::Relaxed),
            "errors": self.errors.load(Ordering::Relaxed),
        })
    }
}

impl<M: Measure> Session<M> {
    /// Marks the session as watched and starts the measurement loop if needed.
    pub fn touch(self: &Arc<Self>, mgr: &Arc<Manager<M>>) {
        *self.last_access.lock().unwrap() = Instant::now();
        if !self.running.swap(true, Ordering::AcqRel) {
            let (s, m) = (self.clone(), mgr.clone());
            tokio::spawn(async move { s.run(m).await });
        }
    }

    async fn run(self: Arc<Self>, mgr: Arc<Manager<M>>) {
        loop {
            let watched = self.last_access.lock().unwrap().elapsed() <= mgr.limits.idle;
            if !watched || mgr.closed.load(Ordering::Acquire) {
                self.running.store(false, Ordering::Release);
                // A touch that raced with this check must not be lost.
                let raced = self.last_access.lock().unwrap().elapsed() <= mgr.limits.idle;
                if raced && !mgr.closed.load(Ordering::Acquire) && !self.running.swap(true, Ordering::AcqRel) {
                    continue;
                }
                return;
            }
            let started = Instant::now();
            let id = self.seq.load(Ordering::Acquire) + 1;
            let tag = format!("{}-{}", self.tag_base, id);
            match tokio::time::timeout(mgr.limits.timeout, mgr.measure.measure(&self.params, id, tag)).await {
                Ok(Ok(out)) => {
                    self.seq.store(id, Ordering::Release);
                    *self.last_error.lock().unwrap() = None;
                    self.tx.send_replace(Some(Arc::new(out))); // wakes every waiter
                    mgr.measurements.fetch_add(1, Ordering::Relaxed);
                }
                Ok(Err(e)) => {
                    mgr.errors.fetch_add(1, Ordering::Relaxed);
                    *self.last_error.lock().unwrap() = Some(e);
                    tokio::time::sleep(Duration::from_millis(500)).await;
                }
                Err(_) => {
                    mgr.errors.fetch_add(1, Ordering::Relaxed);
                    *self.last_error.lock().unwrap() = Some("measurement timed out".into());
                }
            }
            let wait = mgr.limits.min_interval.saturating_sub(started.elapsed());
            if !wait.is_zero() {
                tokio::time::sleep(wait).await;
            }
        }
    }

    /// Latest result, if any.
    pub fn latest(&self) -> Option<Arc<M::Output>> {
        self.tx.borrow().clone()
    }

    /// A receiver for push transports (it only ever holds the latest result:
    /// a slow consumer skips intermediate results automatically).
    pub fn subscribe(&self) -> watch::Receiver<Option<Arc<M::Output>>> {
        self.tx.subscribe()
    }

    /// Legacy semantics: the next result completed after this call.
    pub async fn next(&self, timeout: Duration) -> Option<Arc<M::Output>> {
        let mut rx = self.tx.subscribe(); // the current value counts as seen
        match tokio::time::timeout(timeout, rx.changed()).await {
            Ok(Ok(())) => rx.borrow().clone(),
            _ => None,
        }
    }

    /// Long-poll: the first result with an id greater than `after`.
    pub async fn after(&self, after: u32, timeout: Duration) -> Option<Arc<M::Output>> {
        let mut rx = self.tx.subscribe();
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            let current = rx.borrow_and_update().clone();
            if let Some(o) = current {
                if M::id(&o) > after {
                    return Some(o);
                }
            }
            match tokio::time::timeout_at(deadline, rx.changed()).await {
                Ok(Ok(())) => continue,
                _ => return None,
            }
        }
    }

    /// The latest result, or the first one if there is none yet.
    pub async fn latest_or_next(&self, timeout: Duration) -> Option<Arc<M::Output>> {
        match self.latest() {
            Some(l) => Some(l),
            None => self.after(0, timeout).await,
        }
    }

    pub fn stats(&self) -> Value {
        json!({
            "key": self.key,
            "running": self.running.load(Ordering::Relaxed),
            "id": self.seq.load(Ordering::Relaxed),
            "lastError": *self.last_error.lock().unwrap(),
        })
    }
}
