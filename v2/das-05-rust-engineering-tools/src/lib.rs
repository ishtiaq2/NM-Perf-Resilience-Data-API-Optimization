//! DAS Master Unit data plane: the Engineering Tools (Spectrum Analyzer and
//! Distance-to-Fault) as a small Rust service next to the existing Node.js
//! application.
//!
//! * Raw analyzer output (FPGA/DSP buffers) is parsed and processed in Rust,
//!   on dedicated low-priority threads, without a garbage collector.
//! * One hardware measurement is shared by every viewer (single-flight
//!   sessions), and every representation is encoded once per measurement.
//! * The HTTP/WebSocket API is the das-v1 contract, so the Angular UI and the
//!   shipped frontend keep working. Everything else goes to the Node.js app.

pub mod app;
pub mod config;
pub mod decimate;
pub mod dsp;
pub mod dspc;
pub mod dtf;
pub mod fpga;
pub mod http;
pub mod hw;
pub mod metrics;
pub mod session;
pub mod sim;
pub mod spectrum;
pub mod sysd;
pub mod util;
