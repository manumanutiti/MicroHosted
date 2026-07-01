#!/usr/bin/env bash
# Instala microhosted como servicio systemd. Idempotente: se puede reejecutar
# para actualizar el binario o la unit.
#
# NO compila: usa el binario ya construido en build/microhosted (corre
# `make build` como tu usuario antes, para no compilar como root). El target
# `make install-service` encadena ambos pasos por ti.
#
# Uso:  sudo ./scripts/install-service.sh [ADDR]
#       ADDR es la dirección de escucha de la API (por defecto :8080).

set -euo pipefail

ADDR="${1:-:8080}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_SRC="$REPO_ROOT/build/microhosted"
BIN_DST="/usr/local/bin/microhosted"
UNIT_SRC="$REPO_ROOT/deploy/microhosted.service.in"
UNIT_DST="/etc/systemd/system/microhosted.service"

if [[ $EUID -ne 0 ]]; then
  echo "ERROR: ejecutar con sudo o como root." >&2
  exit 1
fi

if [[ ! -x "$BIN_SRC" ]]; then
  echo "ERROR: no existe $BIN_SRC — compila primero con 'make build'." >&2
  exit 1
fi

echo "==> Instalando binario en $BIN_DST..."
install -o root -g root -m 0755 "$BIN_SRC" "$BIN_DST"

echo "==> Renderizando unit en $UNIT_DST (workdir=$REPO_ROOT, addr=$ADDR)..."
sed -e "s#@BINARY@#${BIN_DST}#g" \
    -e "s#@WORKDIR@#${REPO_ROOT}#g" \
    -e "s#@ADDR@#${ADDR}#g" \
    "$UNIT_SRC" > "$UNIT_DST"

echo "==> systemctl daemon-reload..."
systemctl daemon-reload

echo ""
echo "OK. Servicio instalado. Siguientes pasos:"
echo "  sudo systemctl enable --now microhosted    # arranca ahora + al boot"
echo "  systemctl status microhosted"
echo "  journalctl -u microhosted -f               # seguir logs"
echo ""
echo "Reiniciar sin matar las VMs vivas:"
echo "  sudo systemctl restart microhosted         # reconcile las readopta"
