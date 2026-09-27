# Why Go for the NOC, and what the Rust-vs-Go notes get right and wrong

The notes behind this programme put **Go on the cloud control plane**: a NOC that
thousands of Master Units in stadiums and airports phone home to, deployed on Kubernetes.
das-06 builds that NOC and measures it. This page checks the Go half of the notes against
those measurements and primary sources. The Rust half (the device data plane) is assessed
in das-05's WHY-RUST.md.

## Verdict

**Go is a good fit here, for most of the reasons the notes give.** The workload is many
long-lived, mostly idle connections, fan-out to dashboards, and JSON: exactly what
goroutines and the standard library are built for. One core carried 5 000 sites at 14 %
CPU and 10 000 at 22 %, with alarm updates on screen in 51 ms at p99. The claims about
garbage-collector pauses are outdated, and the memory-safety "mandate" is guidance.

## Claim by claim

| Claim in the notes | Assessment | Evidence |
|---|---|---|
| Goroutines handle thousands of concurrent device connections cheaply | **True, and measured.** One goroutine per Master Unit, blocked in a read almost all the time. | 5 000 sites: 5 058 goroutines, 40 MB of stacks, 127 MB RSS in total. 10 000 sites: 79 MB of stacks, 232 MB RSS ([SCALE](SCALE.md)). |
| Go's GC pauses around 50 ms, which a NOC can tolerate but a device cannot | **Outdated on the number.** Go's stop-the-world pauses have been sub-millisecond since 2017; the Go team's objective is 500 µs per GC cycle [2]. The real GC costs are CPU during marking and heap headroom (RSS ≈ 2 × live heap with the default `GOGC`), which `GOMEMLIMIT` bounds [3]. | Stop-the-world pause p99 **≤ 0.20 ms** over the 5 000-site run and **≤ 0.33 ms** at 10 000 sites, including the alarm storms. The das-03 Go edge on the device measured 0.5 ms. |
| Go is the language of Kubernetes and cloud-native tooling | **True, and it shows in operations.** | One static binary (7.4 MB), a distroless non-root image, `net/http` with HTTP/2 and TLS, Prometheus text format in 90 lines. Cross-compiling for arm64 is one environment variable and needed no download; das-05's Rust ARM targets could not be installed in the same environment. |
| Go is memory-safe | **True, with one caveat.** Go is on the NSA/CISA list of memory-safe languages [1]. The caveat: a data race on a multi-word value (slice, string, interface, map) can corrupt memory, as the Go memory model states [4]. | Every test runs under the race detector (`make check`) [5]. It found one race during development, in the simulator's random-number generator. |
| A "CISA mandate" requires memory-safe languages | **Guidance, not a mandate** [6][7]. It asks for a memory-safety roadmap for products written in memory-unsafe languages. | Go and TypeScript are memory-safe; the NOC adds no C or C++. |
| Go is productive for a team that writes TypeScript | **Plausible.** Go's model (structs, interfaces, explicit errors, `async` replaced by blocking calls in goroutines) is closer to TypeScript than Rust's ownership model. | 5 600 lines of Go plus 1 300 lines of tests, with no dependency outside the standard library. |
| Named vendors use Go (or Rust) for these roles | **Not verified here**, and not needed. | Decide on measurements, not on who else uses what. |

## What Go cost here

- **No WebSocket in the standard library.** The environment this was built in could not
  reach the Go module proxy, so `internal/wsock` (730 lines) implements RFC 6455 on top of
  `net/http`. A fuzz test (`FuzzReadMessage`) found a crash in it within seconds: a frame
  with a 64-bit length whose top bit is set became a negative size, passed the "too big"
  check, and panicked the process, taking every connection down with it. It is fixed, the
  input is kept as a regression test, and 2.3 million fuzzed inputs have passed since. The
  same bug was in the das-03 Go edge's WebSocket reader and is fixed there too. **For
  production, back the same API with a maintained library** (for example
  `github.com/coder/websocket`), which has years of fuzzing and review behind it.
- **GC headroom.** With the default `GOGC=100`, the heap grows to about twice the live data
  between collections. At 10 000 sites that is roughly 60 MB of live heap and a heap that
  swings between 60 and 120 MB. `GOMEMLIMIT` (set in the deployment) keeps it under the
  container limit; for a denser process, lower `GOGC` trades CPU for memory.
- **Data races need tooling.** Go makes it easy to share memory between goroutines by
  accident. The race detector catches what the tests exercise; the design keeps shared
  state behind a few locks (one per site, one for the aggregates, one for the event log) so
  there is little to get wrong.

## Would Rust, or Node.js, do as well for the NOC?

- **Rust** would use less memory per connection (async tasks instead of goroutine stacks,
  no GC headroom): perhaps half. At about 20 KB per site that is 50 MB saved at 5 000 sites,
  which is not a constraint in the cloud. It would cost the team more to write and review.
- **Node.js** could hold 5 000 WebSockets too. Its limits are the single event loop (the
  alarm storm, the overview computation and the JSON encoding all share it, the same way
  the Master Unit's backend got into trouble), so using more than one core means worker
  threads or a cluster with shared state. This was not built or measured here; it is an
  assessment.

The NOC's work is I/O, fan-out and bookkeeping, not computation. Go does that with little
code, uses every core without extra machinery, and deploys as one small binary. That is the
case for Go here; it does not need the overstated claims.

## Sources

1. NSA and CISA, *Memory Safe Languages: Reducing Vulnerabilities in Modern Software
   Development* (June 2025):
   [cisa.gov](https://www.cisa.gov/resources-tools/resources/memory-safe-languages-reducing-vulnerabilities-modern-software-development);
   the language list is quoted in
   [Cybersecurity News](https://cybersecuritynews.com/cisa-releases-guide-to-reduce-memory-safety-vulnerabilities/).
2. R. Hudson, *Getting to Go: The Journey of Go's Garbage Collector* (Go blog, 2018):
   [go.dev/blog/ismmkeynote](https://go.dev/blog/ismmkeynote).
3. *A Guide to the Go Garbage Collector* (`GOGC`, `GOMEMLIMIT`):
   [go.dev/doc/gc-guide](https://go.dev/doc/gc-guide).
4. *The Go Memory Model* (races on multi-word values):
   [go.dev/ref/mem](https://go.dev/ref/mem).
5. *Data Race Detector*: [go.dev/doc/articles/race_detector](https://go.dev/doc/articles/race_detector).
6. CISA and FBI, *Product Security Bad Practices*:
   [cisa.gov](https://www.cisa.gov/resources-tools/resources/product-security-bad-practices).
7. Industrial Cyber, "CISA and FBI release draft guidance on Product Security Bad Practices"
   (the non-binding statement and the 1 January 2026 roadmap date):
   [industrialcyber.co](https://industrialcyber.co/cisa/cisa-and-fbi-release-draft-guidance-on-product-security-bad-practices-for-software-manufacturers/).
