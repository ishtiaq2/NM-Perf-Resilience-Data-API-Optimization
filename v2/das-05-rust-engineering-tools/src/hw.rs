//! The seam to the RF hardware. On the Master Unit this wraps the driver that
//! commands a Remote Node's analyzer over the management channel and receives the
//! FPGA/DSP buffer back over the fiber. `sim::SimAnalyzer` implements it for
//! development, tests and demos.

use std::future::Future;
use std::pin::Pin;

use bytes::Bytes;

use crate::dtf::DtfParams;
use crate::spectrum::SpectrumParams;

pub type BoxFut<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// Why a hardware measurement failed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum HwError {
    /// The node did not answer in time (fiber link, busy analyzer).
    Timeout,
    /// The node or port does not exist or has no analyzer.
    NoSuchAnalyzer(String),
    /// Driver or transport error.
    Io(String),
}

impl std::fmt::Display for HwError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            HwError::Timeout => f.write_str("analyzer did not answer in time"),
            HwError::NoSuchAnalyzer(s) => write!(f, "no analyzer: {s}"),
            HwError::Io(s) => write!(f, "driver error: {s}"),
        }
    }
}

impl std::error::Error for HwError {}

/// A Remote Node analyzer, as seen from the Master Unit.
///
/// Both calls return the raw buffer exactly as the FPGA/DSP produced it
/// (formats in `fpga.rs`); parsing and processing happen on the DSP pool.
pub trait Analyzer: Send + Sync + 'static {
    /// One spectrum sweep (log-power per bin).
    fn sweep<'a>(&'a self, p: &'a SpectrumParams) -> BoxFut<'a, Result<Bytes, HwError>>;
    /// One reflection (S11) sweep of the antenna feeder, for Distance-to-Fault.
    fn s11<'a>(&'a self, p: &'a DtfParams) -> BoxFut<'a, Result<Bytes, HwError>>;
}
