#!/usr/bin/env sh
# Collect the facts that decide how to tune the hotfix (and which long-term
# option fits) on a Master Node. Read-only; safe to run on a customer unit.
#
#   sh device-profile.sh > profile.txt

line() { printf '%-28s %s\n' "$1" "$2"; }
have() { command -v "$1" >/dev/null 2>&1 && echo yes || echo no; }

echo "== DAS Master Node profile ($(date -u +%Y-%m-%dT%H:%M:%SZ))"
line "Kernel" "$(uname -srm)"
[ -r /etc/os-release ] && line "OS" "$(. /etc/os-release; echo "${PRETTY_NAME:-$NAME}")"
line "CPU model" "$(grep -m1 -E 'model name|Processor|cpu model' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //')"
line "CPU part (ARM)" "$(grep -m1 'CPU part' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //')"
line "CPU cores (online)" "$(getconf _NPROCESSORS_ONLN 2>/dev/null || grep -c ^processor /proc/cpuinfo)"
line "BogoMIPS (per core)" "$(grep -m1 -i bogomips /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //')"
line "Memory total" "$(grep MemTotal /proc/meminfo | awk '{printf "%d MB", $2/1024}')"
line "Memory available" "$(grep MemAvailable /proc/meminfo | awk '{printf "%d MB", $2/1024}')"
line "Load average" "$(cut -d' ' -f1-3 /proc/loadavg)"
line "Init system (PID 1)" "$(cat /proc/1/comm 2>/dev/null)"
line "cgroup v2" "$( [ -f /sys/fs/cgroup/cgroup.controllers ] && echo yes || echo no)"
line "Root FS free" "$(df -h / 2>/dev/null | awk 'NR==2{print $4 " of " $2}')"
line "Open files limit" "$(ulimit -n)"
echo
echo "== Runtimes and servers available"
if command -v node >/dev/null 2>&1; then
  line "node" "$(node -v) arch=$(node -p process.arch) v8=$(node -p process.versions.v8)"
  line "worker_threads" "$(node -e "try{require('worker_threads');console.log('yes')}catch(e){console.log('no')}")"
  line "UV_THREADPOOL_SIZE" "${UV_THREADPOOL_SIZE:-default (4)}"
else
  line "node" "not found in PATH"
fi
for b in nginx lighttpd haproxy mosquitto systemd-run taskset chrt; do line "$b" "$(have $b)"; done
echo
echo "== Node processes (RSS)"
ps -eo pid,nice,rss,etime,args 2>/dev/null | awk 'NR==1 || /[n]ode /' | cut -c1-160
echo
echo "== Serial console (synchronous logging risk)"
line "console= on kernel cmdline" "$(tr ' ' '\n' < /proc/cmdline | grep '^console=' | tr '\n' ' ')"
