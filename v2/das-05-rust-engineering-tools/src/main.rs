//! das-engtools: the Master Unit's engineering tools data plane.
//!
//!   das-engtools --listen 0.0.0.0:80 --legacy http://127.0.0.1:8081     # front mode, next to the Node app
//!   das-engtools --listen unix:/run/das/spectrum.sock                   # behind the das-02 nginx gateway

use std::net::SocketAddr;
use std::process::ExitCode;
use std::time::Duration;

use clap::Parser;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tracing::{error, info, warn};

use das_engtools::app::{App, SERVER, VERSION};
use das_engtools::config::Config;
use das_engtools::{http, sysd};

fn main() -> ExitCode {
    let cfg = Config::parse();
    let filter = tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into());
    if cfg.log_format == "text" {
        use std::io::IsTerminal;
        let ansi = std::io::stdout().is_terminal(); // no colour codes in journald or log files
        tracing_subscriber::fmt().with_env_filter(filter).with_ansi(ansi).compact().init();
    } else {
        tracing_subscriber::fmt().with_env_filter(filter).with_ansi(false).json().flatten_event(true).init();
    }
    let cpus = std::thread::available_parallelism().map(|n| n.get()).unwrap_or(1);
    let threads = if cfg.runtime_threads > 0 { cfg.runtime_threads } else { cpus.clamp(1, 2) };
    let rt = match tokio::runtime::Builder::new_multi_thread().worker_threads(threads).thread_name("rt").enable_all().build() {
        Ok(rt) => rt,
        Err(e) => {
            eprintln!("runtime: {e}");
            return ExitCode::FAILURE;
        }
    };
    match rt.block_on(run(cfg, threads)) {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            error!(error = %e, "fatal");
            ExitCode::FAILURE
        }
    }
}

async fn shutdown_signal() {
    let ctrl_c = async {
        let _ = tokio::signal::ctrl_c().await;
    };
    #[cfg(unix)]
    let term = async {
        if let Ok(mut s) = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            s.recv().await;
        }
    };
    #[cfg(not(unix))]
    let term = std::future::pending::<()>();
    tokio::select! {
        _ = ctrl_c => {},
        _ = term => {},
    }
    info!("shutting_down");
    sysd::notify("STOPPING=1");
}

/// GET /internal/health through the real listener (for the systemd watchdog).
async fn self_check(listen: &str) -> bool {
    const REQ: &[u8] = b"GET /internal/health HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n";
    let fut = async {
        let mut buf = [0u8; 12];
        if let Some(path) = listen.strip_prefix("unix:") {
            let mut s = tokio::net::UnixStream::connect(path).await.ok()?;
            s.write_all(REQ).await.ok()?;
            s.read_exact(&mut buf).await.ok()?;
        } else {
            let addr: SocketAddr = listen.parse().ok()?;
            let addr = if addr.ip().is_unspecified() { SocketAddr::from(([127, 0, 0, 1], addr.port())) } else { addr };
            let mut s = tokio::net::TcpStream::connect(addr).await.ok()?;
            s.write_all(REQ).await.ok()?;
            s.read_exact(&mut buf).await.ok()?;
        }
        Some(&buf[9..12] == b"200")
    };
    matches!(tokio::time::timeout(Duration::from_secs(2), fut).await, Ok(Some(true)))
}

fn spawn_watchdog(listen: String) {
    let Some(every) = sysd::watchdog_interval() else { return };
    info!(ping_every_ms = every.as_millis() as u64, "watchdog_enabled");
    tokio::spawn(async move {
        let mut t = tokio::time::interval(every);
        loop {
            t.tick().await;
            if self_check(&listen).await {
                sysd::notify("WATCHDOG=1");
            } else {
                warn!("watchdog_check_failed"); // no ping: systemd restarts the service
            }
        }
    });
}

async fn run(cfg: Config, runtime_threads: usize) -> Result<(), String> {
    let app = App::new(cfg.clone(), None)?;
    let router = http::router(app.clone());
    let d = app.dsp.stats();
    info!(
        server = SERVER,
        version = VERSION,
        listen = %cfg.listen,
        mode = if cfg.front_mode() { "front" } else { "sidecar" },
        legacy = cfg.legacy.as_deref().unwrap_or("-"),
        runtime_threads,
        dsp_workers = d.workers,
        dsp_nice = d.nice,
        cpu_slowdown = d.cpu_slowdown,
        "listening"
    );
    let result = if let Some(path) = cfg.listen.strip_prefix("unix:") {
        let _ = std::fs::remove_file(path); // stale socket from a previous run
        let l = tokio::net::UnixListener::bind(path).map_err(|e| format!("listen {path}: {e}"))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o660));
        }
        sysd::notify(&format!("READY=1\nSTATUS=serving on {}", cfg.listen));
        spawn_watchdog(cfg.listen.clone());
        axum::serve(l, router).with_graceful_shutdown(shutdown_signal()).await
    } else {
        let l = tokio::net::TcpListener::bind(&cfg.listen).await.map_err(|e| format!("listen {}: {e}", cfg.listen))?;
        let local = l.local_addr().map(|a| a.to_string()).unwrap_or_else(|_| cfg.listen.clone());
        sysd::notify(&format!("READY=1\nSTATUS=serving on {local}"));
        spawn_watchdog(local);
        axum::serve(l, router.into_make_service_with_connect_info::<SocketAddr>()).with_graceful_shutdown(shutdown_signal()).await
    };
    app.close();
    result.map_err(|e| e.to_string())
}
