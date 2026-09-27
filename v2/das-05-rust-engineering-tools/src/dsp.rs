//! The DSP pool: CPU-heavy work (parsing raw analyzer buffers, FFTs, encoding,
//! compression) runs on a few dedicated OS threads with a lower scheduling
//! priority (nice +10 on Linux).
//!
//! The async runtime threads only move bytes, so an engineer's 200 001-point
//! sweep can never delay the heartbeat, an alarm poll or a config write. The
//! queue is bounded: when it is full the caller gets `Busy` (HTTP 503 with
//! Retry-After) instead of an ever-growing backlog.
//!
//! Embedded-CPU emulation for benchmarks: with `DAS_CPU_SLOWDOWN=K` every job
//! runs K times, like the Node.js and Go projects. Leave it unset on a device.

use std::panic::{self, AssertUnwindSafe};
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::mpsc::{self, SyncSender, TrySendError};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Instant;

use serde::Serialize;
use tokio::sync::oneshot;

/// Emulated slowdown factor (>= 1), read once.
pub fn slowdown() -> u32 {
    static F: OnceLock<u32> = OnceLock::new();
    *F.get_or_init(|| std::env::var("DAS_CPU_SLOWDOWN").ok().and_then(|v| v.parse::<u32>().ok()).filter(|&n| n >= 1).unwrap_or(1))
}

fn heavy<T>(f: &impl Fn() -> T) -> T {
    let mut r = f();
    for _ in 1..slowdown() {
        r = f();
    }
    r
}

/// The pool rejected the job because its queue is full.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Busy;

impl std::fmt::Display for Busy {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("dsp pool busy")
    }
}

type Job = Box<dyn FnOnce() + Send + 'static>;

struct Inner {
    tx: SyncSender<Job>,
    workers: usize,
    nice: i32,
    queued: AtomicUsize,
    jobs: AtomicU64,
    busy_ns: AtomicU64,
    rejected: AtomicU64,
}

/// Handle to the pool (cheap to clone).
#[derive(Clone)]
pub struct DspPool {
    inner: Arc<Inner>,
}

/// Pool statistics for /internal/metrics.
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct DspStats {
    pub workers: usize,
    pub nice: i32,
    pub queued: usize,
    pub jobs: u64,
    pub busy_seconds: f64,
    pub rejected: u64,
    pub cpu_slowdown: u32,
}

impl DspPool {
    /// Starts `workers` threads that renice themselves to `nice`; at most `queue` jobs wait.
    pub fn new(workers: usize, nice: i32, queue: usize) -> Self {
        let workers = workers.max(1);
        let (tx, rx) = mpsc::sync_channel::<Job>(queue.max(1));
        let rx = Arc::new(Mutex::new(rx));
        let applied = Arc::new(AtomicUsize::new(0));
        let inner = Arc::new(Inner {
            tx,
            workers,
            nice,
            queued: AtomicUsize::new(0),
            jobs: AtomicU64::new(0),
            busy_ns: AtomicU64::new(0),
            rejected: AtomicU64::new(0),
        });
        for i in 0..workers {
            let rx = rx.clone();
            let stats = inner.clone();
            let applied = applied.clone();
            std::thread::Builder::new()
                .name(format!("dsp-{i}"))
                .spawn(move || {
                    if set_thread_nice(nice) {
                        applied.fetch_add(1, Ordering::Relaxed);
                    }
                    loop {
                        // Hold the lock only while waiting; run the job without it.
                        let job = match rx.lock().map(|r| r.recv()) {
                            Ok(Ok(job)) => job,
                            _ => return,
                        };
                        stats.queued.fetch_sub(1, Ordering::Relaxed);
                        let t = Instant::now();
                        job();
                        stats.busy_ns.fetch_add(t.elapsed().as_nanos() as u64, Ordering::Relaxed);
                        stats.jobs.fetch_add(1, Ordering::Relaxed);
                    }
                })
                .expect("spawn dsp thread");
        }
        DspPool { inner }
    }

    /// Runs `f` on a DSP thread and waits for the result. A panic in `f` is
    /// re-raised in the caller's task, not in the worker.
    pub async fn run<T, F>(&self, f: F) -> Result<T, Busy>
    where
        F: Fn() -> T + Send + 'static,
        T: Send + 'static,
    {
        let (tx, rx) = oneshot::channel();
        let job: Job = Box::new(move || {
            let r = panic::catch_unwind(AssertUnwindSafe(|| heavy(&f)));
            let _ = tx.send(r);
        });
        self.inner.queued.fetch_add(1, Ordering::Relaxed);
        if let Err(e) = self.inner.tx.try_send(job) {
            self.inner.queued.fetch_sub(1, Ordering::Relaxed);
            if matches!(e, TrySendError::Full(_)) {
                self.inner.rejected.fetch_add(1, Ordering::Relaxed);
            }
            return Err(Busy);
        }
        match rx.await {
            Ok(Ok(v)) => Ok(v),
            Ok(Err(p)) => panic::resume_unwind(p),
            Err(_) => Err(Busy),
        }
    }

    pub fn stats(&self) -> DspStats {
        let i = &self.inner;
        DspStats {
            workers: i.workers,
            nice: i.nice,
            queued: i.queued.load(Ordering::Relaxed),
            jobs: i.jobs.load(Ordering::Relaxed),
            busy_seconds: (i.busy_ns.load(Ordering::Relaxed) / 1_000_000) as f64 / 1000.0,
            rejected: i.rejected.load(Ordering::Relaxed),
            cpu_slowdown: slowdown(),
        }
    }
}

/// Lowers the calling thread's priority (Linux applies PRIO_PROCESS with a
/// thread id to that thread only; raising the nice value needs no privilege).
#[cfg(target_os = "linux")]
pub fn set_thread_nice(n: i32) -> bool {
    if n == 0 {
        return false;
    }
    // SAFETY: plain syscalls on the current thread, no memory is shared.
    unsafe { libc::setpriority(libc::PRIO_PROCESS, libc::gettid() as libc::id_t, n) == 0 }
}

#[cfg(not(target_os = "linux"))]
pub fn set_thread_nice(_n: i32) -> bool {
    false
}

#[cfg(test)]
mod tests {
    use super::*;

    #[cfg(target_os = "linux")]
    fn my_nice() -> i32 {
        let s = std::fs::read_to_string("/proc/thread-self/stat").unwrap();
        let rest = &s[s.rfind(')').unwrap() + 2..];
        rest.split_whitespace().nth(16).unwrap().parse().unwrap() // field 19: nice
    }

    #[tokio::test]
    async fn jobs_run_on_low_priority_threads() {
        let pool = DspPool::new(2, 10, 8);
        assert_eq!(pool.run(|| 6 * 7).await, Ok(42));
        #[cfg(target_os = "linux")]
        {
            let n = pool.run(my_nice).await.unwrap();
            assert!(n == 10 || n == 0, "worker nice {n}"); // 0 where renicing is not allowed
            assert_ne!(my_nice(), 10, "the caller keeps its priority");
        }
    }

    #[tokio::test]
    async fn full_queue_is_rejected_not_buffered() {
        let pool = DspPool::new(1, 0, 1);
        let gate = Arc::new(std::sync::Barrier::new(2));
        let g = gate.clone();
        let blocker = tokio::spawn({
            let pool = pool.clone();
            async move {
                pool.run(move || {
                    g.wait();
                })
                .await
            }
        });
        // Wait until the worker holds the blocking job, then fill the one queue slot.
        while pool.stats().jobs == 0 && pool.stats().queued > 0 {
            tokio::task::yield_now().await;
        }
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        let queued = tokio::spawn({
            let pool = pool.clone();
            async move { pool.run(|| 1).await }
        });
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        assert_eq!(pool.run(|| 2).await, Err(Busy));
        gate.wait();
        blocker.await.unwrap().unwrap();
        assert_eq!(queued.await.unwrap(), Ok(1));
        assert_eq!(pool.stats().rejected, 1);
    }
}
