#!/bin/sh
# Scale test: 5 000 simulated Master Units + 26 dashboards against one NOC process.
#
#   sh bench/scale.sh                  # 5 000 sites, 5 minutes, storm at 120 s, NOC restart at 200 s
#   SITES=10000 DURATION=240s STORM_AT=150s RESTART_AT=0 CHECKS=120s,230s sh bench/scale.sh
#
# The NOC runs on ONE core (taskset -c 0), the simulator on another. At RESTART_AT
# the NOC gets SIGTERM (it closes every uplink with "service restart") and a new
# process starts immediately with the same data directory; the simulator measures
# how long the fleet takes to come back and checks that the NOC's view matches
# every device again. Results: bench/results/scale-<stamp>.json.
set -eu
cd "$(dirname "$0")/.."
SITES=${SITES:-5000}
DURATION=${DURATION:-300s}
STORM_AT=${STORM_AT:-120s}
RESTART_AT=${RESTART_AT:-200}
CHECKS=${CHECKS:-100s,280s}
PORT=${PORT:-18280}
IPORT=$((PORT + 1000))
export DAS_NOC_SECRET=${DAS_NOC_SECRET:-scale-test-secret-0123456789}
mkdir -p bin .run bench/results
go build -trimpath -ldflags '-s -w' -o bin/noc ./cmd/noc
go build -trimpath -ldflags '-s -w' -o bin/fleetsim ./cmd/fleetsim
PIN_NOC="" PIN_SIM=""
if command -v taskset >/dev/null 2>&1 && [ "$(nproc)" -ge 2 ]; then PIN_NOC="taskset -c 0"; PIN_SIM="taskset -c 1"; fi
rm -rf .run/scale-data
start_noc() {
  $PIN_NOC ./bin/noc -listen 127.0.0.1:$PORT -internal-listen 127.0.0.1:$IPORT -data-dir .run/scale-data -log json >> .run/scale-noc.log 2>&1 &
  echo $! > .run/scale-noc.pid
}
: > .run/scale-noc.log
start_noc
cleanup() { kill "$(cat .run/scale-noc.pid 2>/dev/null)" ${SIM_PID:-} 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT INT TERM
for _ in $(seq 1 50); do curl -fs "http://127.0.0.1:$IPORT/healthz" >/dev/null 2>&1 && break; sleep 0.1; done

STAMP=$(date -u +%Y-%m-%dT%H-%M-%S)
$PIN_SIM ./bin/fleetsim -url http://127.0.0.1:$PORT -metrics "http://127.0.0.1:$IPORT/metrics?format=json" \
  -sites "$SITES" -dashboards 20 -tenant-dashboards 4 -drilldowns 2 -duration "$DURATION" \
  -storm-at "$STORM_AT" -storm-sites $((SITES / 5)) -storm-alarms 20 -storm-clear-after 45s -checks "$CHECKS" \
  -report "bench/results/scale-$STAMP.json" &
SIM_PID=$!

if [ "$RESTART_AT" -gt 0 ]; then
  sleep "$RESTART_AT"
  echo "== restarting the NOC (SIGTERM, then a new process with the same data directory)"
  kill -TERM "$(cat .run/scale-noc.pid)"
  wait "$(cat .run/scale-noc.pid)" 2>/dev/null || true
  start_noc
fi
wait $SIM_PID
cp "bench/results/scale-$STAMP.json" bench/results/latest.json
echo "report: bench/results/scale-$STAMP.json"
