#!/bin/sh
# Start the edge on a local port, run the das-v1 conformance suite, stop it.
#   sh scripts/conformance.sh [port]
set -eu
cd "$(dirname "$0")/.."
PORT=${1:-18380}
mkdir -p .run
./bin/das-edge -listen "127.0.0.1:$PORT" -log text > .run/conformance-edge.log 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT INT TERM
for _ in $(seq 1 50); do
  if curl -fs "http://127.0.0.1:$PORT/api/heartbeat" > /dev/null 2>&1; then break; fi
  sleep 0.1
done
node conformance/run.js --base "http://127.0.0.1:$PORT"
