#!/usr/bin/env sh
# Scan a Node.js code base for patterns that block the event loop.
#
#   sh scripts/find-blocking-calls.sh /path/to/backend/src
#
# A hit is not automatically a bug (readFileSync at start-up is fine), but
# every hit inside a request handler, timer or message callback on the
# Master Node is a candidate for the "server is dead" symptom.

ROOT="${1:-.}"
INC="--include=*.js --include=*.cjs --include=*.mjs --include=*.ts"
EXC="--exclude-dir=node_modules --exclude-dir=dist --exclude-dir=build --exclude-dir=.git --exclude-dir=coverage"

section() {
  title="$1"; pattern="$2"; advice="$3"
  # shellcheck disable=SC2086
  hits=$(grep -rnE $INC $EXC "$pattern" "$ROOT" 2>/dev/null)
  n=$(printf '%s' "$hits" | grep -c . || true)
  printf '\n== %s  (%s hits)\n   %s\n' "$title" "$n" "$advice"
  [ -n "$hits" ] && printf '%s\n' "$hits" | head -n 40 | sed 's/^/   /'
  [ "$n" -gt 40 ] && printf '   ... %s more\n' "$((n - 40))"
}

echo "Event-loop blocking scan of: $ROOT"

section "Synchronous child processes" \
  '(execSync|spawnSync|execFileSync)\(' \
  "Blocks until the child exits (e.g. a CLI that talks to the RF hardware). Use spawn/execFile with callbacks or move it into a worker."

section "Synchronous file system calls" \
  'fs\.[a-zA-Z]+Sync\(|(readFileSync|writeFileSync|existsSync|statSync|readdirSync|appendFileSync)\(' \
  "Fine at start-up; in handlers use fs.promises. On flash storage a sync write can stall for 100s of ms."

section "Synchronous compression / crypto" \
  'zlib\.[a-zA-Z]+Sync\(|(gzipSync|gunzipSync|deflateSync|inflateSync|brotliCompressSync)\(|(pbkdf2Sync|scryptSync|generateKeyPairSync|randomFillSync)\(' \
  "CPU-bound on the event loop. Use the async variants (they run in the libuv pool) or a worker."

section "JSON.parse / JSON.stringify" \
  'JSON\.(parse|stringify)\(' \
  "Harmless for small objects; the root cause for large ones (spectrum sweeps, volatile_data). Check which ones touch payloads > 100 kB."

section "Pretty-printed JSON" \
  'JSON\.stringify\([^)]*,[^)]*,[ ]*[0-9"'"'"']' \
  "Indentation makes serialisation slower and responses larger. Never pretty-print API payloads."

section "Deep clone / merge / compare of big objects" \
  '(cloneDeep|_\.merge|merge\(|isEqual\(|structuredClone\(|JSON\.parse\(JSON\.stringify)' \
  "O(size of the object) on every call. Merging volatile_data this way on each report or request adds up quickly."

section "Console logging" \
  'console\.(log|info|warn|error|debug|dir|trace)\(' \
  "process.stdout/stderr writes are SYNCHRONOUS for files and TTYs. On a serial console (115200 baud) a 10 kB line blocks ~0.9 s. Log via an async logger, keep lines short and never log payloads."

section "Busy waits / tight timers" \
  'while[ ]*\([ ]*true[ ]*\)|Atomics\.wait|setInterval\([^,]+,[ ]*[0-9]{1,2}[ ]*\)' \
  "Spinning or sub-100 ms intervals starve the loop on a small CPU."

section "Large synchronous loops over nodes/points in handlers (review)" \
  'for[ ]*\(.*(points|nodes|sweep|samples|data)\.length' \
  "Loops over thousands of points belong in a worker, or should run once per change rather than once per request."

echo
echo "Next step: run the backend with --cpu-prof (or 'node --prof') while the spectrum analyzer is used,"
echo "and look for these call sites at the top of the profile. The hotfix's blocked-loop detector logs"
echo "'event_loop_blocked' with the suspect request URL in the field."
