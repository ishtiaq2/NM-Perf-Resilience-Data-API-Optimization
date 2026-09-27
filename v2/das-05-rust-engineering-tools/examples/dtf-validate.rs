//! Validation of the distance-to-fault event detection against known feeders.
//!
//!   cargo run --release --example dtf-validate
//!
//! For 80 simulated feeders (port connector, jumper, sometimes a damaged
//! connector, antenna), three windows, three sweep sizes and five noise levels,
//! it counts reflections that were missed, events that match no reflection
//! ("phantoms", the dangerous kind: a false fault sends a technician up a mast)
//! and return-loss errors. Configurations whose feeder is longer than 80 % of
//! the unambiguous range are skipped: reflections beyond the range alias into
//! it, which is physics (choose more points, or let maxDistanceM choose them).

use std::collections::HashMap;

use das_engtools::dtf::{self, DtfParams, Window};
use das_engtools::sim;

fn main() {
    let mut failed = false;
    println!("{:>8} {:>12} {:>7} {:>9} {:>14}", "noise σ", "reflections", "missed", "phantoms", "worst RL error");
    for sigma in [0.0f64, 0.0005, 0.003, 0.01, 0.03] {
        let (mut total, mut missed, mut phantoms, mut worst) = (0, 0, 0, 0.0f64);
        for window in [Window::Rect, Window::Hann, Window::Kaiser] {
            for points in [1024usize, 4096, 16001] {
                for node in 1..=40u32 {
                    for port in 1..=2u16 {
                        let mut p = DtfParams::parse(&HashMap::new()).expect("defaults");
                        (p.points, p.window, p.node_id, p.port) = (points, window, node, port);
                        let feeder = sim::feeder_for(node, port);
                        let gamma = sim::reflection_sweep(&p, &feeder, 0.88, 6.0, sigma, (node * 7 + port as u32) as u64);
                        let prof = dtf::compute(&p, &gamma, p.step_hz());
                        if feeder.iter().any(|r| r.distance_m > 0.8 * prof.max_range_m) {
                            continue;
                        }
                        let near = |d: f64, r: &sim::Reflection| (d - r.distance_m).abs() <= 2.0 * prof.resolution_m;
                        for r in &feeder {
                            total += 1;
                            match prof.events.iter().find(|e| near(e.distance_m, r)) {
                                Some(e) => worst = worst.max((e.return_loss_db + 20.0 * r.rho.log10()).abs()),
                                None => {
                                    missed += 1;
                                    eprintln!("missed {r:?} ({window:?}, {points} pts, node {node} port {port})");
                                }
                            }
                        }
                        for e in &prof.events {
                            if !feeder.iter().any(|r| near(e.distance_m, r)) {
                                phantoms += 1;
                                eprintln!("phantom {e:?} ({window:?}, {points} pts, node {node} port {port})");
                            }
                        }
                    }
                }
            }
        }
        println!("{sigma:>8} {total:>12} {missed:>7} {phantoms:>9} {worst:>11.2} dB");
        failed |= missed > 0 || phantoms > 0 || worst > 1.5;
    }
    if failed {
        std::process::exit(1);
    }
}
