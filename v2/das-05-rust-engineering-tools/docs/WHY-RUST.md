# Why Rust here, and what the Rust-vs-Go notes get right and wrong

The notes behind this project propose **Rust for the device data plane** (the DAS Master Unit
parsing FPGA/DSP output) and **Go for the cloud control plane** (a NOC portal aggregating
thousands of Master Units). This programme has now built and measured both sides: das-05
(Rust, device) and das-06 (Go, NOC), plus the earlier das-03 Go edge that also runs on the
device. This page checks the notes against those measurements and against primary sources.

## Verdict

**The split is a good default, but several of the reasons given are overstated.**

- Rust earns its place on the device through **lower CPU and memory per sweep**, and because
  it is the natural language for any new **low-level** code (drivers, DSP daemons) that
  would otherwise be written in C or C++.
- The device does not *need* Rust to fix the "server is dead" alarm. The Node.js hotfix and
  the Go edge both fixed it on the same emulated core. The alarm was caused by **where** the
  work ran (the Node.js event loop), not by the language.
- Go is the right tool for the NOC for the reasons the notes give. das-06 measures it.

## Claim by claim

| Claim in the notes | Assessment | Evidence |
|---|---|---|
| "Rust is currently the only memory-safe language fast enough" for the device | **Overstated.** NSA and CISA list Ada, C#, Delphi/Object Pascal, Go, Java, Python, Ruby, Rust and Swift as memory-safe languages [1]. The existing Node.js code is also memory-safe at the language level. Among languages *without* a tracing garbage collector and with C-like speed, Rust is the most mature general choice, and Ada/SPARK is the long-standing embedded alternative. | The Go edge (das-03) met the same heartbeat SLO on the same emulated core: p99 4.7 ms, 0 timeouts. |
| Go's garbage collector pauses around 50 ms | **Outdated.** Go's stop-the-world pauses have been sub-millisecond since 2017. The Go team's objective is 500 µs per GC cycle, and the measured pauses are typically 100–200 µs [2]. The real GC costs on a small single-core device are different: about 25 % of the CPU during marking, and heap headroom. `GOMEMLIMIT` (Go 1.19+) bounds the latter [3]. | The das-03 Go edge measured **GC pause p99 = 0.5 ms** over 789 GC cycles under the standard load. |
| A "CISA mandate" forces memory-safe languages | **Guidance, not a mandate.** CISA and FBI's *Product Security Bad Practices* is explicitly non-binding. For software used in critical infrastructure or national critical functions, it calls it dangerous not to publish a **memory-safety roadmap by 1 January 2026** for existing products written in memory-*unsafe* languages [4][5]. | It applies to C and C++ on the Master Unit (drivers, daemons), not to the Node.js or Go code. That is a good reason to write new low-level code in Rust, not to replace Node.js. |
| Rust parses massive FPGA/DSP output efficiently | **True, and measured.** | A 50 001-bin raw sweep parses in 0.02 ms (about 20× faster than Node.js). The legacy JSON (2 MB) builds 9× faster than in Node.js, with byte-identical output ([BENCHMARKS](BENCHMARKS.md)). |
| No GC, predictable latency, a tiny footprint (under 10 MB) | **True for the binary, close for memory.** | Binary 4.2 MB (1.8 MB gzipped); 14–17 MB PSS under load, compared with 34–43 MB for the Go edge, which does more, and 124–146 MB for the Node.js app. |
| Go suits a NOC with thousands of devices phoning home | **True.** Goroutines make one connection per Master Unit cheap. The standard library covers HTTP/1.1, HTTP/2 and TLS (das-03 and das-06 add WebSocket framing on top of it), and the Kubernetes and Prometheus ecosystems are Go-first. | das-06: 5 000 simulated Master Units on one core at 13.8 % CPU and 127 MB RSS, alarm to screen p99 51 ms, GC pause p99 ≤ 0.2 ms (das-06 `docs/WHY-GO.md`). |
| Named vendors (network-equipment makers) use Rust or Go for these roles | **Not verified here**, and not needed. | Decide on the measurements above, not on who else uses what. |

## What Rust costs this team

The team writes Angular and Node.js. Rust's costs are real:

- **Learning curve.** Ownership, borrowing and async Rust take weeks to become comfortable.
  Code review needs at least one person fluent in Rust.
- **Build times.** A release build of this service takes about a minute and a half
  (fat LTO); incremental debug builds take seconds.
- **Hiring and bus factor.** Rust developers are fewer than JavaScript or Go developers.

This project is designed to keep that cost small:

- **The Rust surface is small and stable**: about 3 800 lines, behind a fixed contract
  (das-v1). Business logic, the UI and configuration stay in Node.js.
- **The hardware seam is one trait.** Only the driver adapter touches the FPGA
  ([FPGA-INTERFACE](FPGA-INTERFACE.md)).
- **Everything is testable without hardware**: the simulator, 36 tests, the conformance
  suite, and the DTF validation.

## When Go would be the better choice on the device

- The team will own the code long-term without Rust experience. Go is closer to how a
  JavaScript team already thinks.
- One compiled language across device and cloud matters more than the last 2× of CPU
  efficiency. The das-03 Go edge already covers spectrum. DTF could be ported to it: the
  FFT is the only piece Go's standard library lacks, and a radix-2 FFT is about 40 lines.
- The device has memory to spare (40 MB rather than 15 MB).

## Recommendation

1. **Now:** the das-01 hotfix fixes the alarm in the shipped release, with no new language.
2. **Next release, if the team adopts Rust for device code:** das-05 in the das-02 gateway
   (setup A). It brings DTF, and spectrum serving that costs a fraction of the CPU, for
   about 15 MB of memory. If the team does not want a second language on the device, the
   das-03 Go edge (steps 1–2) is the same "safest path" in Go, and DTF gets ported
   (das-00 `docs/OPTIONS.md`, section 9).
3. **Low-level code:** write new drivers and DSP daemons in Rust rather than C or C++, and
   publish the memory-safety roadmap the CISA guidance asks for, for the C/C++ that remains.
4. **Cloud:** the NOC in Go (das-06).

## Sources

1. NSA and CISA, *Memory Safe Languages: Reducing Vulnerabilities in Modern Software
   Development* (June 2025):
   [cisa.gov](https://www.cisa.gov/resources-tools/resources/memory-safe-languages-reducing-vulnerabilities-modern-software-development).
   The language list is quoted in
   [Cybersecurity News](https://cybersecuritynews.com/cisa-releases-guide-to-reduce-memory-safety-vulnerabilities/).
   Earlier: CISA, NSA et al., *The Case for Memory Safe Roadmaps* (December 2023),
   [cisa.gov](https://www.cisa.gov/case-memory-safe-roadmaps).
2. R. Hudson, *Getting to Go: The Journey of Go's Garbage Collector* (Go blog, 2018):
   [go.dev/blog/ismmkeynote](https://go.dev/blog/ismmkeynote).
3. *A Guide to the Go Garbage Collector*: [go.dev/doc/gc-guide](https://go.dev/doc/gc-guide).
4. CISA and FBI, *Product Security Bad Practices*:
   [cisa.gov](https://www.cisa.gov/resources-tools/resources/product-security-bad-practices).
5. Industrial Cyber, "CISA and FBI release draft guidance on Product Security Bad Practices"
   (quotes the non-binding statement and the 1 January 2026 roadmap date):
   [industrialcyber.co](https://industrialcyber.co/cisa/cisa-and-fbi-release-draft-guidance-on-product-security-bad-practices-for-software-manufacturers/).
