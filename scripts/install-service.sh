#!/usr/bin/env bash
# Installs microhosted as a systemd service. Idempotent: it can be re-run to
# update the binary or the unit.
#
# It does NOT build: it uses the binary already built at build/microhosted (run
# `make build` as your user first, so as not to build as root). The
# `make install-service` target chains both steps for you.
#
# Usage:  sudo ./scripts/install-service.sh [ADDR]
#         ADDR is the API's listen address (default :8080).

set -euo pipefail

ADDR="${1:-:8080}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_SRC="$REPO_ROOT/build/microhosted"
BIN_DST="/usr/local/bin/microhosted"
UNIT_SRC="$REPO_ROOT/deploy/microhosted.service.in"
UNIT_DST="/etc/systemd/system/microhosted.service"

if [[ $EUID -ne 0 ]]; then
  echo "ERROR: run with sudo or as root." >&2
  exit 1
fi

if [[ ! -x "$BIN_SRC" ]]; then
  echo "ERROR: $BIN_SRC doesn't exist — build it first with 'make build'." >&2
  exit 1
fi

echo "==> Installing the binary to $BIN_DST..."
install -o root -g root -m 0755 "$BIN_SRC" "$BIN_DST"

echo "==> Rendering the unit to $UNIT_DST (workdir=$REPO_ROOT, addr=$ADDR)..."
sed -e "s#@BINARY@#${BIN_DST}#g" \
    -e "s#@WORKDIR@#${REPO_ROOT}#g" \
    -e "s#@ADDR@#${ADDR}#g" \
    "$UNIT_SRC" > "$UNIT_DST"

echo "==> systemctl daemon-reload..."
systemctl daemon-reload

echo ""
echo "OK. Service installed. Next steps:"
echo "  sudo systemctl enable --now microhosted    # start now + at boot"
echo "  systemctl status microhosted"
echo "  journalctl -u microhosted -f               # follow logs"
echo ""
echo "Restart without killing live VMs:"
echo "  sudo systemctl restart microhosted         # reconcile re-adopts them"
