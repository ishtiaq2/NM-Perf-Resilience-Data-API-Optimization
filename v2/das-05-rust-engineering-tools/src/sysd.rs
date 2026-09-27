//! systemd notify protocol (sd_notify(3)) without libsystemd: READY/STOPPING and
//! a watchdog that only pings while the service really answers HTTP.

use std::os::unix::net::UnixDatagram;
use std::time::Duration;

/// Sends one state string; false when not running under systemd.
pub fn notify(state: &str) -> bool {
    let Ok(path) = std::env::var("NOTIFY_SOCKET") else { return false };
    if path.is_empty() {
        return false;
    }
    let Ok(sock) = UnixDatagram::unbound() else { return false };
    if let Some(abstract_name) = path.strip_prefix('@') {
        #[cfg(target_os = "linux")]
        {
            use std::os::linux::net::SocketAddrExt;
            if let Ok(addr) = std::os::unix::net::SocketAddr::from_abstract_name(abstract_name.as_bytes()) {
                return sock.send_to_addr(state.as_bytes(), &addr).is_ok();
            }
        }
        #[cfg(not(target_os = "linux"))]
        let _ = abstract_name;
        return false;
    }
    sock.send_to(state.as_bytes(), path).is_ok()
}

/// Half of WatchdogSec when the watchdog is enabled for this process.
pub fn watchdog_interval() -> Option<Duration> {
    let us: u64 = std::env::var("WATCHDOG_USEC").ok()?.parse().ok()?;
    if us == 0 {
        return None;
    }
    if let Ok(pid) = std::env::var("WATCHDOG_PID") {
        if pid != std::process::id().to_string() {
            return None;
        }
    }
    Some(Duration::from_micros(us / 2))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sends_ready_to_the_notify_socket() {
        let dir = std::env::temp_dir().join(format!("das-sd-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("notify.sock");
        let _ = std::fs::remove_file(&path);
        let listener = UnixDatagram::bind(&path).unwrap();
        std::env::set_var("NOTIFY_SOCKET", &path);
        assert!(notify("READY=1"));
        let mut buf = [0u8; 64];
        let n = listener.recv(&mut buf).unwrap();
        assert_eq!(&buf[..n], b"READY=1");
        std::env::remove_var("NOTIFY_SOCKET");
        assert!(!notify("READY=1"));
        let _ = std::fs::remove_dir_all(&dir);
    }
}
