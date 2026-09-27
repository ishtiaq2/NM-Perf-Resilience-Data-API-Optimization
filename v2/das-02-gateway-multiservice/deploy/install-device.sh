#!/usr/bin/env sh
# Install the gateway stack on a Master Node (run as root). Idempotent.
#   sh deploy/install-device.sh /opt/das-web
set -e
DEST="${1:-/opt/das-web}"
SRC="$(cd "$(dirname "$0")/.." && pwd)"
id das >/dev/null 2>&1 || adduser --system --no-create-home --group das 2>/dev/null || adduser -S -H -G das das
id dasgw >/dev/null 2>&1 || adduser --system --no-create-home --ingroup das dasgw 2>/dev/null || adduser -S -H -G das dasgw
mkdir -p "$DEST" /etc/das/nginx /etc/das/tls
cp -r "$SRC/lib" "$SRC/services" "$SRC/node_modules" "$SRC/package.json" "$DEST/"
node "$SRC/scripts/render-nginx.js" --profile device --out /etc/das/nginx
[ -f /etc/das/tls/device.crt ] || sh "$SRC/scripts/gen-selfsigned.sh" /etc/das/tls "$(hostname).local"
cp "$SRC/deploy/tmpfiles/das.conf" /etc/tmpfiles.d/das.conf && systemd-tmpfiles --create /etc/tmpfiles.d/das.conf
cp "$SRC"/deploy/systemd/das* /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now das.target
echo "installed; UI: http://$(hostname) and https://$(hostname) (HTTP/2)"
