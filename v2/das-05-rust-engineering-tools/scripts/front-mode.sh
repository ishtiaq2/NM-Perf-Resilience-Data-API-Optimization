#!/bin/sh
# The "tiny microservice deployed alongside Node.js" path, without nginx:
# das-engtools owns the public port and serves the engineering tools
# (spectrum analyzer, distance-to-fault); every other request goes to the
# UNCHANGED Node.js application (UI files, login, config forms, volatile_data).
# Then the das-v1 conformance suite runs against the single origin.
#
#   sh scripts/front-mode.sh                 # shipped behaviour (das-01 --legacy) behind the Rust front
#   NODE_MODE=hotfix sh scripts/front-mode.sh
#   KEEP=1 sh scripts/front-mode.sh          # leave both running (Ctrl-C to stop)
#
# Needs ../das-01-hotfix-node (Node >= 18 for the conformance runner).
set -eu
cd "$(dirname "$0")/.."
ROOT=$PWD
NODE_APP=${NODE_APP:-../das-01-hotfix-node}
NODE_MODE=${NODE_MODE:-legacy}
PORT=${PORT:-8080}
NODE_PORT=${NODE_PORT:-8081}
BIN=${BIN:-$ROOT/target/release/das-engtools}
[ -x "$BIN" ] || { echo "build first: cargo build --release"; exit 1; }
mkdir -p .run

# 1. The existing Node.js backend moves to a loopback port (one line in its service file).
( cd "$NODE_APP" && DAS_MODE=$NODE_MODE HOST=127.0.0.1 PORT=$NODE_PORT exec node src/server.js ) > .run/node.log 2>&1 &
NODE_PID=$!
cleanup() {
  kill ${RUST_PID:-} "$NODE_PID" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM
for _ in $(seq 1 100); do
  curl -fs "http://127.0.0.1:$NODE_PORT/api/heartbeat" >/dev/null 2>&1 && break
  sleep 0.1
done

# 2. das-engtools takes the public port and forwards what it does not implement.
"$BIN" --listen "127.0.0.1:$PORT" --legacy "http://127.0.0.1:$NODE_PORT" --log-format text > .run/front.log 2>&1 &
RUST_PID=$!
for _ in $(seq 1 100); do
  curl -fs "http://127.0.0.1:$PORT/internal/health" >/dev/null 2>&1 && break
  sleep 0.1
done

echo "== Node.js ($NODE_MODE) on 127.0.0.1:$NODE_PORT behind das-engtools (Rust) on 127.0.0.1:$PORT"
printf 'heartbeat (answered by Node, via the front): '; curl -s "http://127.0.0.1:$PORT/api/heartbeat"; echo
printf 'capabilities (merged):                        '; curl -s "http://127.0.0.1:$PORT/api/capabilities"; echo
node conformance/run.js --base "http://127.0.0.1:$PORT"
if [ -n "${KEEP:-}" ]; then
  echo "running: http://127.0.0.1:$PORT  (Ctrl-C to stop)"
  wait "$RUST_PID"
fi
