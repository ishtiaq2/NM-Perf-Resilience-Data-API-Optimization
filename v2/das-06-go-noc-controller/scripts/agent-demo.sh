#!/bin/sh
# End to end with a REAL Master Unit backend: the das-01 Node.js app (simulated
# Remote Nodes, the shipped volatile_data API) + agent/noc-agent.js as a sidecar
# + the Go NOC. Checks that the NOC's view matches the device: summary hash
# (computed independently in JavaScript and Go), alarms, and the node table on
# drill-down.
#   sh scripts/agent-demo.sh        (needs ../das-01-hotfix-node and Node.js 22+)
set -eu
cd "$(dirname "$0")/.."
ROOT=$PWD
APP=${APP_DIR:-../das-01-hotfix-node}
NOC_PORT=${NOC_PORT:-18180}
APP_PORT=${APP_PORT:-18181}
export DAS_NOC_SECRET=${DAS_NOC_SECRET:-agent-demo-secret-0123456789}
mkdir -p .run bin
go build -trimpath -ldflags '-s -w' -o bin/noc ./cmd/noc

./bin/noc -listen 127.0.0.1:$NOC_PORT -internal-listen 127.0.0.1:$((NOC_PORT + 1000)) -log json > .run/agent-demo-noc.log 2>&1 &
NOC_PID=$!
( cd "$APP" && DAS_MODE=${APP_MODE:-hotfix} HOST=127.0.0.1 PORT=$APP_PORT exec node src/server.js ) > .run/agent-demo-app.log 2>&1 &
APP_PID=$!
cleanup() { kill ${AGENT_PID:-} $APP_PID $NOC_PID 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT INT TERM
for _ in $(seq 1 100); do curl -fs "http://127.0.0.1:$APP_PORT/api/heartbeat" >/dev/null 2>&1 && break; sleep 0.1; done

SITE_TOKEN=$(./bin/noc token -kind site -subject S90001 -tenant demo-venue)
ADMIN=$(./bin/noc token -kind user -subject demo-admin -tenant '*')
NOC_URL=ws://127.0.0.1:$NOC_PORT/uplink/v1 NOC_TOKEN=$SITE_TOKEN LOCAL_API=http://127.0.0.1:$APP_PORT \
  SITE_NAME="Demo Master Unit" SITE_VENUE="das-01 simulator" SITE_REGION=lab SITE_FW=1.1.0 POLL_MS=2000 \
  node agent/noc-agent.js > .run/agent-demo-agent.log 2>&1 &
AGENT_PID=$!
sleep 8

echo "== das-01 (${APP_MODE:-hotfix}) -> noc-agent.js -> Go NOC"
ADMIN=$ADMIN NOC=http://127.0.0.1:$NOC_PORT METRICS=http://127.0.0.1:$((NOC_PORT + 1000))/metrics?format=json node -e '
const h = { Authorization: "Bearer " + process.env.ADMIN };
const get = async (u, hdr) => (await fetch(u, { headers: hdr || h })).json();
(async () => {
  const s = await get(process.env.NOC + "/api/sites/S90001");
  const m = await get(process.env.METRICS, {});
  console.log("site S90001:", { connected: s.connected, status: s.status, nodes: s.nodes, online: s.online, offline: s.offline,
    degraded: s.degraded, maxTempC: s.maxTempC, minRxDbm: s.minRxDbm, maxVswr: s.maxVswr, alarms: s.alarms, rev: s.rev, hash: s.hash });
  console.log("NOC: summaries", m.fleet.summaries, "deltas", m.fleet.deltas, "alarm events", m.fleet.alarmEvents, "hash mismatches", m.fleet.hashMismatch);
  const d = await get(process.env.NOC + "/api/sites/S90001/detail?waitMs=5000");
  console.log("drill-down: node table with", d.count, "Remote Nodes, e.g.", JSON.stringify(d.nodes && d.nodes[0]));
  const ok = s.connected && s.nodes === 300 && m.fleet.hashMismatch === 0 && m.fleet.deltas > 0 && d.count === 300;
  console.log(ok ? "PASS: the NOC matches the Master Unit (hashes computed in JavaScript and Go agree)" : "FAIL");
  process.exit(ok ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });'
