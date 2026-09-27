#!/bin/sh
# Install das-engtools on a Master Unit (run as root). Idempotent.
#
#   sh deploy/install-device.sh gateway  target/aarch64-unknown-linux-musl/release/das-engtools
#   sh deploy/install-device.sh front    target/aarch64-unknown-linux-musl/release/das-engtools
#
# gateway: next to the das-02 stack (/etc/das/nginx exists). Replaces the Node.js
#          spectrum service, adds the /api/dtf and /api/capabilities locations to
#          nginx, reloads nginx. Rollback: systemctl disable --now das-engtools &&
#          systemctl enable --now das-spectrum (and remove the two locations).
# front:   no gateway. Installs the unit with profile B; move the Node.js app to
#          127.0.0.1:8081 before starting it (see docs/INTEGRATION.md).
set -eu
MODE=${1:?usage: install-device.sh gateway|front <binary>}
BIN=${2:?path to the das-engtools binary for this CPU}
SRC=$(cd "$(dirname "$0")/.." && pwd)
DEST=/opt/das-engtools

id das >/dev/null 2>&1 || adduser --system --no-create-home --group das 2>/dev/null || adduser -S -H -G das das
mkdir -p "$DEST"
install -m 0755 "$BIN" "$DEST/das-engtools.new" && mv "$DEST/das-engtools.new" "$DEST/das-engtools"
cp "$SRC/README.md" "$DEST/"
[ -f /etc/default/das-engtools ] || cp "$SRC/deploy/systemd/das-engtools.env" /etc/default/das-engtools
cp "$SRC/deploy/systemd/das-engtools.service" /etc/systemd/system/

case "$MODE" in
  gateway)
    [ -d /etc/das/nginx ] || { echo "das-02 gateway not found (/etc/das/nginx)"; exit 1; }
    mkdir -p /etc/systemd/system/das.target.d
    cp "$SRC/deploy/systemd/das.target.d/engtools.conf" /etc/systemd/system/das.target.d/
    if ! grep -q 'location /api/dtf' /etc/das/nginx/das-locations.conf; then
      cp /etc/das/nginx/das-locations.conf /etc/das/nginx/das-locations.conf.before-engtools
      cat "$SRC/deploy/nginx/das02-engtools-locations.conf" >> /etc/das/nginx/das-locations.conf
    fi
    nginx -t
    systemctl daemon-reload
    systemctl disable --now das-spectrum.service 2>/dev/null || true
    systemctl enable --now das-engtools.service
    systemctl reload das-gateway.service 2>/dev/null || nginx -s reload
    ;;
  front)
    sed -i -e 's|^DAS_LISTEN=unix:.*|#&|' -e 's|^DAS_SIDECAR=1|#&|' -e 's|^DAS_LEGACY=unix:.*|#&|' \
           -e 's|^#DAS_LISTEN=0.0.0.0:80|DAS_LISTEN=0.0.0.0:80|' -e 's|^#DAS_LEGACY=http://127.0.0.1:8081|DAS_LEGACY=http://127.0.0.1:8081|' \
           /etc/default/das-engtools
    sed -i 's|^WantedBy=das.target|WantedBy=multi-user.target|' /etc/systemd/system/das-engtools.service
    sed -i -e 's|^PartOf=das.target|#&|' -e 's|^Slice=das.slice|#&|' /etc/systemd/system/das-engtools.service
    systemctl daemon-reload
    echo "installed (front mode). Move the Node.js app to 127.0.0.1:8081, then: systemctl enable --now das-engtools"
    exit 0
    ;;
  *) echo "mode must be gateway or front"; exit 2 ;;
esac
sleep 1
systemctl --no-pager --lines=0 status das-engtools.service || true
echo "installed (gateway mode): the UI discovers distance-to-fault via /api/capabilities"
