#!/bin/sh
# The NOC with a simulated fleet, to look at the NOC page:
# 400 Master Units of 10 customers, alarms coming and going, and after 90 s a
# regional mains failure (40 sites on battery) that clears two minutes later.
#
#   sh scripts/demo.sh                 then open the printed URL
#   SITES=2000 PORT=8088 sh scripts/demo.sh
set -eu
cd "$(dirname "$0")/.."
PORT=${PORT:-8080}
IPORT=${IPORT:-9090}
SITES=${SITES:-400}
export DAS_NOC_SECRET=${DAS_NOC_SECRET:-demo-secret-0123456789abcdef}
mkdir -p .run bin
[ -x bin/noc ] && [ -x bin/fleetsim ] || { go build -o bin/noc ./cmd/noc && go build -o bin/fleetsim ./cmd/fleetsim; }

./bin/noc -listen 127.0.0.1:$PORT -internal-listen 127.0.0.1:$IPORT -data-dir .run/demo-data -demo > .run/demo-noc.log 2>&1 &
NOC_PID=$!
cleanup() { kill ${SIM_PID:-} $NOC_PID 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT INT TERM
for _ in $(seq 1 50); do curl -fs "http://127.0.0.1:$IPORT/healthz" >/dev/null 2>&1 && break; sleep 0.1; done

./bin/fleetsim -url http://127.0.0.1:$PORT -metrics '' -sites "$SITES" -dashboards 0 -latency-dashboards 0 \
  -tenant-dashboards 0 -drilldowns 0 -duration 24h -storm-at 90s -storm-sites $((SITES / 10)) -storm-alarms 6 \
  -storm-clear-after 120s -checks '' > .run/demo-fleetsim.log 2>&1 &
SIM_PID=$!

echo "== $SITES simulated Master Units -> NOC on 127.0.0.1:$PORT (metrics: http://127.0.0.1:$IPORT/metrics)"
grep -m1 'NOC page:' .run/demo-noc.log || true
echo "   customer view:  http://127.0.0.1:$PORT/#token=$(./bin/noc token -kind user -subject airport-ops -tenant airport-01)"
echo "Ctrl-C to stop."
wait $SIM_PID
