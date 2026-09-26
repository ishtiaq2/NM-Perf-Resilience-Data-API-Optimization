#!/usr/bin/env sh
# Self-signed certificate for the device (or the local demo). On a real product,
# provision a per-device certificate at manufacturing time or via the customer's PKI.
#   sh scripts/gen-selfsigned.sh [out-dir] [common-name]
set -e
OUT="${1:-.run/tls}"
CN="${2:-das-master-node.local}"
mkdir -p "$OUT"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 825 \
  -keyout "$OUT/device.key" -out "$OUT/device.crt" -subj "/CN=$CN" \
  -addext "subjectAltName=DNS:$CN,DNS:localhost,IP:127.0.0.1" >/dev/null 2>&1
chmod 600 "$OUT/device.key"
echo "certificate: $OUT/device.crt (CN=$CN, ECDSA P-256)"
