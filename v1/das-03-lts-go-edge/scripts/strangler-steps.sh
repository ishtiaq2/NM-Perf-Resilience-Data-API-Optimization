#!/bin/sh
# Strangler-fig migration, step by step, against the SHIPPED behaviour: das-01 in
# --legacy mode (blocks its event loop on spectrum) is the legacy backend, and the
# edge takes over one endpoint group per step. Each step runs the full das-v1
# conformance suite, including the heartbeat SLO under spectrum load.
#   sh scripts/strangler-steps.sh        (needs ../das-01-hotfix-node and Node 22+)
set -eu
cd "$(dirname "$0")/.."
P1=${P1_DIR:-../das-01-hotfix-node}
mkdir -p .run
PORT=18090 HOST=127.0.0.1 node "$P1/src/server.js" --legacy > .run/legacy.log 2>&1 &
LEGACY=$!
EDGE=""
trap 'kill $LEGACY $EDGE 2>/dev/null || true' EXIT INT TERM
sleep 2
step=1
for delegate in "volatile,spectrum,ws" "volatile,ws" "ws" ""; do
  ./bin/das-edge -listen 127.0.0.1:18380 -legacy http://127.0.0.1:18090 ${delegate:+-delegate $delegate} -config legacy -log text > ".run/strangler-step$step.log" 2>&1 &
  EDGE=$!
  sleep 2.5
  echo "== step $step: -delegate '${delegate}'"
  node conformance/run.js --base http://127.0.0.1:18380 | grep -E "FAIL|p99 <|passed"
  kill $EDGE; wait $EDGE 2>/dev/null || true; EDGE=""
  step=$((step + 1))
done
