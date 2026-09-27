#!/bin/sh
# Run the Angular reference client's Playwright tests against the Go edge, with
# the edge serving the production build from the same origin (-web).
# Needs ../das-04-angular-client with `npm ci` done and Playwright's Chromium.
set -eu
cd "$(dirname "$0")/.."
NG=${NG_DIR:-../das-04-angular-client}
PORT=${PORT:-18580}
if [ ! -d "$NG/dist/das-04-angular-client/browser" ]; then (cd "$NG" && npx ng build); fi
mkdir -p .run
./bin/das-edge -listen "127.0.0.1:$PORT" -web "$NG/dist/das-04-angular-client/browser" -log text > .run/e2e-edge.log 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT INT TERM
for _ in $(seq 1 50); do
  if curl -fs "http://127.0.0.1:$PORT/api/heartbeat" > /dev/null 2>&1; then break; fi
  sleep 0.1
done
cd "$NG" && E2E_BASE_URL="http://127.0.0.1:$PORT" npx playwright test
