#!/bin/sh
# Option C: the Node.js app stays the front (it may terminate HTTPS) and forwards the
# engineering-tools routes to das-engtools on a Unix socket (examples/node-in-front).
# Demo with the shipped behaviour (../das-01-hotfix-node --legacy), then conformance.
#   sh scripts/node-in-front.sh
set -eu
cd "$(dirname "$0")/.."
ROOT=$PWD
BIN=${BIN:-$ROOT/target/release/das-engtools}
PORT=${PORT:-8080}
[ -x "$BIN" ] || { echo "build first: cargo build --release"; exit 1; }
mkdir -p .run
SOCK=$(mktemp -u "${TMPDIR:-/tmp}/das-engtools-XXXXXX.sock")
"$BIN" --listen "unix:$SOCK" --log-format text > .run/node-in-front-rust.log 2>&1 &
RUST_PID=$!
for _ in $(seq 1 50); do [ -S "$SOCK" ] && break; sleep 0.1; done
DAS_ENGTOOLS_SOCKET=$SOCK DAS_MODE=${NODE_MODE:-legacy} HOST=127.0.0.1 PORT=$PORT node examples/node-in-front/demo-das01.js > .run/node-in-front-node.log 2>&1 &
NODE_PID=$!
cleanup() { kill "$NODE_PID" "$RUST_PID" 2>/dev/null || true; wait 2>/dev/null || true; rm -f "$SOCK"; }
trap cleanup EXIT INT TERM
for _ in $(seq 1 100); do curl -fs "http://127.0.0.1:$PORT/api/heartbeat" >/dev/null 2>&1 && break; sleep 0.1; done
echo "== Node.js (${NODE_MODE:-legacy}) in front on :$PORT, engineering routes piped to das-engtools ($SOCK)"
node conformance/run.js --base "http://127.0.0.1:$PORT"
