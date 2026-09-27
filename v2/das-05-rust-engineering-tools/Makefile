# das-engtools: the Master Unit's engineering-tools data plane (Rust).

CARGO ?= cargo
BIN   := target/release/das-engtools

.PHONY: all build test lint fmt run front drop-in node-front conformance bench dsp-bench dtf-validate cross sizes clean

all: lint test build

build:
	$(CARGO) build --release

test:
	$(CARGO) test --release

lint:
	$(CARGO) fmt --check
	$(CARGO) clippy --release --all-targets -- -D warnings

fmt:
	$(CARGO) fmt

# Standalone, simulated analyzer, text logs: http://127.0.0.1:8090/api/dtf?nodeId=3&port=1&maxDistanceM=60
run: build
	./$(BIN) --listen 127.0.0.1:8090 --log-format text

# Front mode in front of the unchanged shipped Node.js app (../das-01-hotfix-node --legacy),
# then the das-v1 conformance suite. NODE_MODE=hotfix for the hotfixed app.
front: build
	sh scripts/front-mode.sh

# Inside the das-02 nginx gateway, replacing the Node.js spectrum service; conformance through nginx.
drop-in: build
	sh scripts/drop-in-das02.sh

# The Node.js app keeps the port and pipes the engineering routes to das-engtools (setup C).
node-front: build
	sh scripts/node-in-front.sh

conformance: front drop-in node-front

# The standard load (4 engineers x 2 traces of 50 001 points) against Rust front + shipped Node,
# one core, embedded CPU emulated; compared with the earlier projects' results.
bench: build
	node bench/front-under-load.js

# Node.js vs Rust on the hot paths (same raw FPGA buffer, byte-identical legacy JSON check).
dsp-bench:
	$(CARGO) build --release --example dsp-bench
	node bench/dsp-compare.js
	node bench/dsp-compare.js --points 200001

# Distance-to-fault detection against 80 simulated feeders x 3 windows x 3 sizes x 5 noise levels.
dtf-validate:
	$(CARGO) run --release --example dtf-validate

# Master Unit CPUs (64-bit ARM, 32-bit ARMv7 hard-float): static musl binaries.
# Needs: rustup target add aarch64-unknown-linux-musl armv7-unknown-linux-musleabihf
#        and the linkers named in .cargo/config.toml (or use `cross`, see docs/BUILD.md).
cross:
	$(CARGO) build --release --target aarch64-unknown-linux-musl
	$(CARGO) build --release --target armv7-unknown-linux-musleabihf
	mkdir -p dist
	cp target/aarch64-unknown-linux-musl/release/das-engtools dist/das-engtools-linux-arm64
	cp target/armv7-unknown-linux-musleabihf/release/das-engtools dist/das-engtools-linux-armv7

sizes: build
	@for f in $(BIN) dist/das-engtools-*; do [ -f $$f ] && printf '%-40s %6.2f MB  (gzip %5.2f MB)\n' $$f $$(echo "$$(stat -c %s $$f) / 1048576" | bc -l) $$(echo "$$(gzip -9c $$f | wc -c) / 1048576" | bc -l); done; true

clean:
	$(CARGO) clean
	rm -rf .run dist
