#!/bin/sh
# The real data path: das-edge WITHOUT its internal simulator; 300 Remote Nodes
# stream their state over das.v1.NodeIngestService (Connect, h2c) from das-nodesim.
# Open http://127.0.0.1:8080 while it runs; Ctrl-C stops both.
set -eu
cd "$(dirname "$0")/.."
NODES=${NODES:-300}
mkdir -p .run
./bin/das-edge -listen 127.0.0.1:8080 -nodes 0 -ingest 127.0.0.1:9090 -log text > .run/demo-edge.log 2>&1 &
EDGE=$!
trap 'kill $EDGE 2>/dev/null || true' EXIT INT TERM
sleep 0.5
echo "edge: http://127.0.0.1:8080  (ingest h2c :9090, log .run/demo-edge.log)"
./bin/das-nodesim -master http://127.0.0.1:9090 -nodes "$NODES" -stats 10s ${DURATION:+-duration "$DURATION"}
