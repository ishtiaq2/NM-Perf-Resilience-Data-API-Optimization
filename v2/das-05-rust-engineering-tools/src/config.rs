//! Command line and environment configuration (every flag has a DAS_* variable).

use std::time::Duration;

use clap::Parser;

#[derive(Parser, Debug, Clone)]
#[command(
    name = "das-engtools",
    version,
    about = "DAS Master Unit engineering tools (Spectrum Analyzer, Distance-to-Fault) in Rust",
    long_about = "Serves /api/spectrum*, /api/ws/spectrum and /api/dtf (das-v1 contract).\n\
                  Front mode (--legacy URL): owns the port and forwards every other request (WebSockets included)\n\
                  to the Node.js app. Sidecar mode (--sidecar, or no --legacy): serves only its own paths, behind\n\
                  a gateway such as nginx; with --legacy it still merges the app's capabilities."
)]
pub struct Config {
    /// Listen address: host:port or unix:/path.sock
    #[arg(long, env = "DAS_LISTEN", default_value = "127.0.0.1:8090")]
    pub listen: String,

    /// The existing Node.js application (http://host:port or unix:/path.sock). Everything this
    /// service does not implement is forwarded there, and /api/capabilities merges its features.
    #[arg(long, env = "DAS_LEGACY")]
    pub legacy: Option<String>,

    /// Do not forward other paths even with --legacy (behind a gateway that routes only
    /// the engineering paths here; --legacy is then used for capabilities only).
    /// DAS_SIDECAR accepts 1/0, true/false, yes/no, on/off.
    #[arg(long, env = "DAS_SIDECAR", action = clap::ArgAction::SetTrue, value_parser = clap::builder::BoolishValueParser::new())]
    pub sidecar: bool,

    /// Sweep points when a client does not say
    #[arg(long, env = "DAS_SPECTRUM_POINTS", default_value_t = 50001)]
    pub points: usize,

    /// Concurrent analyzer configurations (spectrum + DTF each)
    #[arg(long, env = "DAS_SPECTRUM_MAX_SESSIONS", default_value_t = 8)]
    pub max_sessions: usize,

    /// Stop measuring when nobody asked for this long (ms)
    #[arg(long, env = "DAS_SPECTRUM_IDLE_MS", default_value_t = 15_000)]
    pub idle_ms: u64,

    /// Minimum time between two sweeps of one session (ms)
    #[arg(long, env = "DAS_SPECTRUM_MIN_INTERVAL_MS", default_value_t = 0)]
    pub min_interval_ms: u64,

    /// Longest wait for a measurement before answering 503 (ms)
    #[arg(long, env = "DAS_SWEEP_WAIT_MS", default_value_t = 10_000)]
    pub sweep_wait_ms: u64,

    /// Simulator: spectrum sweep duration (ms)
    #[arg(long, env = "DAS_SWEEP_TIME_MS", default_value_t = 250)]
    pub sweep_time_ms: u64,

    /// Simulator: reflection (S11) sweep duration (ms)
    #[arg(long, env = "DAS_S11_TIME_MS", default_value_t = 400)]
    pub s11_time_ms: u64,

    /// Simulator: analyzers (node 1..N, port 1) generated at start-up
    #[arg(long, env = "DAS_SIM_PREWARM", default_value_t = 4)]
    pub prewarm: u32,

    /// WebSocket clients at most
    #[arg(long, env = "DAS_MAX_WS_CLIENTS", default_value_t = 64)]
    pub max_ws: usize,

    /// DSP threads (0 = CPUs - 1, at least 1)
    #[arg(long, env = "DAS_DSP_WORKERS", default_value_t = 0)]
    pub dsp_workers: usize,

    /// Nice value of the DSP threads (Linux)
    #[arg(long, env = "DAS_DSP_NICE", default_value_t = 10)]
    pub dsp_nice: i32,

    /// Jobs that may wait for a DSP thread before requests get 503
    #[arg(long, env = "DAS_DSP_QUEUE", default_value_t = 64)]
    pub dsp_queue: usize,

    /// Async runtime threads (0 = CPUs, at most 2)
    #[arg(long, env = "DAS_RUNTIME_THREADS", default_value_t = 0)]
    pub runtime_threads: usize,

    /// Log format: json or text
    #[arg(long, env = "DAS_LOG_FORMAT", default_value = "json")]
    pub log_format: String,
}

impl Config {
    pub fn idle(&self) -> Duration {
        Duration::from_millis(self.idle_ms)
    }
    pub fn sweep_wait(&self) -> Duration {
        Duration::from_millis(self.sweep_wait_ms)
    }
    /// True when other paths are forwarded to the legacy application.
    pub fn front_mode(&self) -> bool {
        self.legacy.is_some() && !self.sidecar
    }

    /// Defaults for tests and embedding.
    pub fn for_tests() -> Self {
        Config::parse_from(["das-engtools", "--sweep-time-ms", "20", "--s11-time-ms", "20", "--points", "2001", "--prewarm", "0"])
    }
}
