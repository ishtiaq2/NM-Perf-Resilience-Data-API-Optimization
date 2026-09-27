//! Application state shared by all handlers, and its construction.

use std::sync::atomic::AtomicUsize;
use std::sync::Arc;
use std::time::{Duration, Instant};

use crate::config::Config;
use crate::dsp::DspPool;
use crate::dtf::DtfMeasure;
use crate::http::proxy::Legacy;
use crate::hw::Analyzer;
use crate::metrics::Metrics;
use crate::session::{Limits, Manager};
use crate::sim::SimAnalyzer;
use crate::spectrum::{SpectrumMeasure, SpectrumParams};
use crate::util::boot_id;

pub const SERVER: &str = "das-engtools";
pub const VERSION: &str = env!("CARGO_PKG_VERSION");

pub struct App {
    pub cfg: Config,
    pub boot_id: String,
    pub started: Instant,
    pub dsp: DspPool,
    pub spectrum: Arc<Manager<SpectrumMeasure>>,
    pub dtf: Arc<Manager<DtfMeasure>>,
    pub legacy: Option<Legacy>,
    pub metrics: Metrics,
    pub ws_clients: AtomicUsize,
}

pub type AppState = Arc<App>;

impl App {
    /// Builds the application with the given hardware (the simulator when `hw` is None).
    pub fn new(cfg: Config, hw: Option<Arc<dyn Analyzer>>) -> Result<AppState, String> {
        let cpus = std::thread::available_parallelism().map(|n| n.get()).unwrap_or(1);
        let workers = if cfg.dsp_workers > 0 { cfg.dsp_workers } else { cpus.saturating_sub(1).max(1) };
        let dsp = DspPool::new(workers, cfg.dsp_nice, cfg.dsp_queue);
        let hw: Arc<dyn Analyzer> = match hw {
            Some(h) => h,
            None => {
                let sim = SimAnalyzer::new(Duration::from_millis(cfg.sweep_time_ms), Duration::from_millis(cfg.s11_time_ms));
                for node in 1..=cfg.prewarm {
                    let mut q = std::collections::HashMap::new();
                    q.insert("nodeId".to_string(), node.to_string());
                    if let Ok(p) = SpectrumParams::parse(&q, cfg.points) {
                        sim.prewarm(&p);
                    }
                }
                Arc::new(sim)
            }
        };
        let boot = boot_id();
        let limits = |timeout_s: u64| Limits {
            idle: cfg.idle(),
            min_interval: Duration::from_millis(cfg.min_interval_ms),
            max_sessions: cfg.max_sessions.max(1),
            timeout: Duration::from_secs(timeout_s),
        };
        let spectrum = Manager::new(SpectrumMeasure { hw: hw.clone(), dsp: dsp.clone() }, format!("sp-{boot}"), limits(30));
        let dtf = Manager::new(DtfMeasure { hw, dsp: dsp.clone() }, format!("dtf-{boot}"), limits(30));
        let legacy = match &cfg.legacy {
            Some(url) => Some(Legacy::new(url)?),
            None => None,
        };
        Ok(Arc::new(App {
            cfg,
            boot_id: boot,
            started: Instant::now(),
            dsp,
            spectrum,
            dtf,
            legacy,
            metrics: Metrics::default(),
            ws_clients: AtomicUsize::new(0),
        }))
    }

    /// Stops measuring (shutdown).
    pub fn close(&self) {
        self.spectrum.close();
        self.dtf.close();
    }
}
