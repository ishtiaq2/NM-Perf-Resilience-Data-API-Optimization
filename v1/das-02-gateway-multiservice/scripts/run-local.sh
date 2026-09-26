#!/usr/bin/env sh
# Run the whole gateway stack locally, like on the device:
# nginx on :8080 (HTTP) and :8443 (HTTPS + HTTP/2), three Node services on Unix sockets.
#
#   sh scripts/run-local.sh              # start (Ctrl-C stops everything)
#   sh scripts/run-local.sh stop         # stop a stack started in the background
#
# Environment:
#   WEB_ROOT=/path/to/angular/dist/browser   serve a real frontend build (default: ./web)
#   PIN_CPU=0                                 pin all device processes to one core (Linux, taskset)
#   DAS_CPU_SLOWDOWN=8                        emulate an embedded CPU on a fast PC
#   DAS_NODES=300                             number of simulated Remote Nodes
# Requires: node >= 12.16, nginx >= 1.18, openssl. On Windows use WSL.
set -e
cd "$(dirname "$0")/.."
ROOT="$PWD"
RUN="$ROOT/.run"

stop() {
  for p in nginx core-api spectrum telemetry; do
    if [ -f "$RUN/$p.pid" ]; then kill "$(cat "$RUN/$p.pid")" 2>/dev/null || true; rm -f "$RUN/$p.pid"; fi
  done
}
if [ "$1" = "stop" ]; then stop; echo "stopped"; exit 0; fi

command -v nginx >/dev/null 2>&1 || { echo "nginx not found (apt install nginx-light | brew install nginx)"; exit 1; }
stop
mkdir -p "$RUN/log"
# Unix socket paths are limited to ~107 bytes: use a short private directory when the
# project lives in a deep path.
SOCK="$RUN"
if [ ${#RUN} -gt 80 ]; then
  SOCK="${TMPDIR:-/tmp}/das-sock-$(id -u)-$(printf '%s' "$ROOT" | cksum | cut -d' ' -f1)"
  mkdir -p "$SOCK"
  chmod 700 "$SOCK"
fi
[ -f "$RUN/tls/device.crt" ] || sh scripts/gen-selfsigned.sh "$RUN/tls" >/dev/null
node scripts/render-nginx.js --profile local --out "$RUN/nginx" --web-root "${WEB_ROOT:-$ROOT/web}" --sock-dir "$SOCK"

PIN=""
if [ -n "$PIN_CPU" ] && command -v taskset >/dev/null 2>&1; then PIN="taskset -c $PIN_CPU"; fi

export DAS_UPSTREAM_SPECTRUM="unix:$SOCK/spectrum.sock"
export DAS_UPSTREAM_TELEMETRY="unix:$SOCK/telemetry.sock"
LISTEN="unix:$SOCK/telemetry.sock" $PIN node services/telemetry-service/server.js 2>>"$RUN/log/telemetry.log" &
echo $! > "$RUN/telemetry.pid"
LISTEN="unix:$SOCK/spectrum.sock" DAS_DEMO=1 $PIN node services/spectrum-service/server.js 2>>"$RUN/log/spectrum.log" &
echo $! > "$RUN/spectrum.pid"
LISTEN="unix:$SOCK/core-api.sock" $PIN node services/core-api/server.js 2>>"$RUN/log/core-api.log" &
echo $! > "$RUN/core-api.pid"

for i in $(seq 1 100); do
  [ -S "$SOCK/core-api.sock" ] && [ -S "$SOCK/spectrum.sock" ] && [ -S "$SOCK/telemetry.sock" ] && break
  sleep 0.1
done

$PIN nginx -p "$RUN/nginx" -c "$RUN/nginx/nginx.conf" -g 'daemon off;' &
echo $! > "$RUN/nginx.pid"

trap 'stop; exit 0' INT TERM
echo "DAS gateway stack running"
echo "  http://127.0.0.1:8080    (HTTP/1.1)"
echo "  https://127.0.0.1:8443   (HTTP/2, self-signed certificate)"
echo "  logs: $RUN/log/   stop: Ctrl-C (or sh scripts/run-local.sh stop)"
wait
