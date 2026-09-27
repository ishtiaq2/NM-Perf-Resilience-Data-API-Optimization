#!/bin/sh
# The "multi-service API gateway" path, end to end: the das-02 nginx gateway stays
# the single origin (TLS, HTTP/2, static UI files) and das-engtools (Rust) REPLACES
# das-02's Node.js spectrum-service on the same Unix socket. nginx gets two extra
# locations (deploy/nginx/das02-engtools-locations.conf): /api/dtf, and
# /api/capabilities so the frontend discovers distance-to-fault. core-api and
# telemetry-service are untouched; core-api's health probe (/internal/health) and
# aggregated metrics keep working. Then the das-v1 conformance suite runs through nginx.
#
#   sh scripts/drop-in-das02.sh     (needs ../das-02-gateway-multiservice with npm ci done, nginx)
set -eu
cd "$(dirname "$0")/.."
ROOT=$PWD
GW=${GW_DIR:-../das-02-gateway-multiservice}
GW=$(cd "$GW" && pwd)
BIN=${BIN:-$ROOT/target/release/das-engtools}
[ -x "$BIN" ] || { echo "build first: cargo build --release"; exit 1; }
mkdir -p .run
(cd "$GW" && sh scripts/run-local.sh > "$ROOT/.run/gateway.log" 2>&1 &)
cleanup() { kill ${RUST:-} 2>/dev/null || true; (cd "$GW" && sh scripts/run-local.sh stop >/dev/null 2>&1) || true; }
trap cleanup EXIT INT TERM
for _ in $(seq 1 100); do
  curl -fs http://127.0.0.1:8080/api/heartbeat >/dev/null 2>&1 && break
  sleep 0.2
done
CONF="$GW/.run/nginx"
SOCK=$(grep -ho 'unix:[^;]*spectrum.sock' "$CONF"/*.conf | head -1 | sed 's/.*unix://')
CORE=$(grep -ho 'unix:[^;]*core-api.sock' "$CONF"/*.conf | head -1)
[ -n "$SOCK" ] && [ -n "$CORE" ] || { echo "sockets not found in the gateway config"; exit 1; }

# 1. das-engtools replaces the Node.js spectrum-service on its socket.
kill "$(cat "$GW/.run/spectrum.pid")"
sleep 0.5
"$BIN" --listen "unix:$SOCK" --sidecar --legacy "$CORE" --log-format text > .run/drop-in.log 2>&1 &
RUST=$!
for _ in $(seq 1 50); do [ -S "$SOCK" ] && break; sleep 0.1; done

# 2. Two extra nginx locations, then a graceful reload.
if ! grep -q 'location /api/dtf' "$CONF/das-locations.conf"; then
  cat deploy/nginx/das02-engtools-locations.conf >> "$CONF/das-locations.conf"
fi
kill -HUP "$(cat "$GW/.run/nginx.pid")"
sleep 2.5                                        # reload done; core-api's health probe sees the new service

echo "== das-02 gateway with das-engtools (Rust) as the engineering-tools service ($SOCK)"
printf 'heartbeat (core-api):         '; curl -s http://127.0.0.1:8080/api/heartbeat; echo
printf 'capabilities (merged):        '; curl -s http://127.0.0.1:8080/api/capabilities; echo
printf 'distance-to-fault, node 3/1:  '
curl -s 'http://127.0.0.1:8080/api/dtf?nodeId=3&port=1&maxDistanceM=60' | node -e '
  let s = ""; process.stdin.on("data", (d) => (s += d)).on("end", () => {
    const j = JSON.parse(s);
    console.log(`${j.points} points, range ${j.maxRangeM} m, resolution ${j.resolutionM} m`);
    for (const e of j.events) console.log(`    ${e.distanceM.toFixed(2).padStart(7)} m  RL ${e.returnLossDb.toFixed(1).padStart(5)} dB  VSWR ${e.vswr.toFixed(2)}${e.fault ? "  FAULT" : ""}`);
  });'
node conformance/run.js --base http://127.0.0.1:8080
