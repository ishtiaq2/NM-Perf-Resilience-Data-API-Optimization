# Building

## Toolchain

- Rust **1.85 or newer**. That is the minimum the locked dependencies need (clap 4.6,
  hyper-util 0.1.21). This repository was built and tested with **Rust 1.95.0** only.
- Nothing else. The dependency tree is pure Rust:
  - gzip is `flate2` on `miniz_oxide`;
  - the FFT is `rustfft`;
  - there is no OpenSSL, `ring` or `cc` build step.

  Cross-compiling therefore needs only a linker.

```sh
cargo build --release          # target/release/das-engtools
make test lint                 # 29 unit + 7 end-to-end tests; rustfmt + clippy -D warnings
```

Release profile: `opt-level = 3`, fat LTO, one codegen unit, stripped symbols.

## Master Unit CPUs: static ARM binaries

**Not verified in this repository.** The build environment could not download Rust's ARM
standard libraries from static.rust-lang.org. The commands below are the standard ones.
Run them once on a build machine with normal internet access, and CI keeps them honest after
that.

```sh
rustup target add aarch64-unknown-linux-musl armv7-unknown-linux-musleabihf
sudo apt install gcc-aarch64-linux-gnu gcc-arm-linux-gnueabihf     # linkers only
make cross        # dist/das-engtools-linux-arm64, dist/das-engtools-linux-armv7
```

`.cargo/config.toml` names those linkers. The **musl** targets link statically by
default: one file, and no glibc version to match on the device.

If installing cross linkers is inconvenient, [`cross`](https://github.com/cross-rs/cross)
builds inside a container that already has them:

```sh
cargo install cross
cross build --release --target aarch64-unknown-linux-musl
cross build --release --target armv7-unknown-linux-musleabihf
```

Pick the target from the Master Unit's `uname -m`: `aarch64` means 64-bit ARM, and
`armv7l` means 32-bit ARMv7 hard-float. The armv7 target does not assume NEON, so it runs on
any ARMv7-A with a VFPv3 (or newer) FPU.

Smoke test without the device, using qemu-user (`apt install qemu-user-static`):

```sh
qemu-aarch64-static dist/das-engtools-linux-arm64 --version
# With binfmt_misc registered (Debian's qemu-user-static does this), the ARM binary runs
# like a native one, so the front-mode conformance run works unchanged:
BIN=$PWD/dist/das-engtools-linux-arm64 sh scripts/front-mode.sh
```

Expected sizes, based on the x86-64 build (4.2 MB stripped, 1.8 MB gzipped) and typical
ARM code density: about 3.5–4.5 MB. Confirm with `make sizes` after `make cross`.

## CI suggestion

```yaml
# per push
- cargo fmt --check
- cargo clippy --release --all-targets -- -D warnings
- cargo test --release
- cargo run --release --example dtf-validate        # fails on any missed or phantom event
# per release
- make cross && make sizes
- qemu-aarch64-static dist/das-engtools-linux-arm64 --version
- BIN=dist/das-engtools-linux-arm64 sh scripts/front-mode.sh   # conformance under qemu-user
```

`cargo audit` (RustSec advisories) and `cargo deny` (licences, duplicate versions) are worth
adding for a product that ships in the field. Both need network access to their databases.
