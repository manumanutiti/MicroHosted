#!/usr/bin/env bash
# Instalación completa de MicroHosted en ESTA máquina, en un solo paso:
#
#   1. comprobaciones (arquitectura, KVM, Go)
#   2. configuración del host (cgroups, nftables, store CoW btrfs)
#   3. firecracker + jailer (misma versión, binarios de la arquitectura)
#   4. compilación del daemon
#   5. servicio systemd (enable --now)
#   6. verificación de salud por la API
#
# Soporta x86_64 y aarch64 (Raspberry Pi 4/5 64-bit, Jetson, gateways ARM).
# Entrada normal: `make full-install` (acepta ARCH=, FC_VERSION=, ADDR=).
#
# Idempotente: reejecutarlo actualiza binario/servicio y no toca las VMs vivas
# (KillMode=process + reconcile). OJO: con FC_VERSION=latest puede actualizar
# Firecracker — los snapshots van ligados a la versión que los creó.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
  *) echo "ERROR: arquitectura no soportada: $ARCH (usa x86_64 o aarch64)" >&2; exit 1 ;;
esac
NATIVE="$(uname -m)"
if [[ "$ARCH" != "$NATIVE" ]]; then
  echo "ERROR: full-install debe ejecutarse EN la máquina objetivo ($ARCH)," >&2
  echo "       porque KVM, cgroups, nftables y el store son locales." >&2
  echo "       Para cross-compilar solo el binario:  make build ARCH=$ARCH" >&2
  echo "       Para cross-construir una imagen:      make prepare-image ARCH=$ARCH" >&2
  exit 1
fi

FC_VERSION="${FC_VERSION:-latest}"
ADDR="${ADDR:-:8080}"

echo "=============================================="
echo " MicroHosted — instalación completa (${ARCH})"
echo "=============================================="

# --- [1/6] Comprobaciones ---------------------------------------------------
echo ""
echo "==> [1/6] Comprobaciones previas..."

if [[ ! -e /dev/kvm ]]; then
  echo "ERROR: /dev/kvm no existe — sin KVM no hay microVMs." >&2
  if [[ "$ARCH" == "aarch64" ]]; then
    echo "  En Raspberry Pi: usa un SO de 64 bits (Raspberry Pi OS 64-bit o" >&2
    echo "  Ubuntu Server arm64); en Pi 4/5 KVM viene en el kernel de serie." >&2
    echo "  Comprueba: ls /dev/kvm ; zgrep KVM /proc/config.gz" >&2
  else
    echo "  Activa VT-x/AMD-V en la BIOS (o virtualización anidada si es una VM)." >&2
  fi
  exit 1
fi
echo "  /dev/kvm: OK"

if ! command -v go &>/dev/null; then
  echo "ERROR: falta Go (1.22+). Instálalo: https://go.dev/dl/ o 'sudo snap install go --classic'" >&2
  exit 1
fi
echo "  $(go version): OK"

# Pedir sudo una vez al principio, no a mitad de instalación.
echo "  (se necesita sudo para configurar host, binarios y servicio)"
sudo -v

# --- [2/6] Host: cgroups, nftables, store CoW -------------------------------
echo ""
echo "==> [2/6] Configurando el host (setup-host.sh)..."
sudo ./scripts/setup-host.sh

# --- [3/6] Firecracker + Jailer ----------------------------------------------
echo ""
echo "==> [3/6] Instalando firecracker + jailer (${FC_VERSION})..."
if command -v firecracker &>/dev/null; then
  echo "  versión actual: $(firecracker --version | head -1)"
fi
ARCH="$ARCH" ./scripts/install-fc.sh "$FC_VERSION"

# --- [4/6] Compilar el daemon -------------------------------------------------
echo ""
echo "==> [4/6] Compilando microhosted..."
mkdir -p build
CGO_ENABLED=0 go build -o build/microhosted ./cmd/microhosted
echo "  build/microhosted: OK"

# --- [5/6] Servicio systemd ---------------------------------------------------
echo ""
echo "==> [5/6] Instalando el servicio systemd..."
sudo ./scripts/install-service.sh "$ADDR"
sudo systemctl enable --now microhosted
sudo systemctl restart microhosted   # si ya corría, que coja el binario nuevo

# --- [6/6] Verificación --------------------------------------------------------
echo ""
echo "==> [6/6] Verificando salud de la API..."
HEALTH_HOST="${ADDR}"
[[ "$HEALTH_HOST" == :* ]] && HEALTH_HOST="localhost${HEALTH_HOST}"
HEALTH_OK=0
for _ in $(seq 1 15); do
  if curl -fsS "http://${HEALTH_HOST}/v1/health" >/dev/null 2>&1; then
    HEALTH_OK=1
    break
  fi
  sleep 1
done
if [[ "$HEALTH_OK" -eq 1 ]]; then
  echo "  GET /v1/health: OK"
  curl -fsS "http://${HEALTH_HOST}/v1/health" 2>/dev/null || true
  echo ""
else
  echo "  WARN: la API no responde aún en http://${HEALTH_HOST}/v1/health" >&2
  echo "        mira el log: journalctl -u microhosted -n 50" >&2
fi

# ¿Hay alguna plantilla usable? (catálogo con kernel+rootfs existentes)
MISSING="$(python3 - images/catalog.json <<'PY' 2>/dev/null || true
import json, os, sys
try:
    cat = json.load(open(sys.argv[1]))
except Exception:
    cat = []
usable = [t for t in cat
          if os.path.exists(t.get("kernel_path","")) and os.path.exists(t.get("rootfs_path",""))]
print("ok" if usable else "none")
PY
)"

echo ""
echo "=============================================="
echo " Instalación completada (${ARCH})"
echo "=============================================="
echo "  API:      http://${HEALTH_HOST}"
echo "  logs:     journalctl -u microhosted -f"
echo "  estado:   curl -s http://${HEALTH_HOST}/v1/system | python3 -m json.tool"
if [[ "$MISSING" != "ok" ]]; then
  echo ""
  echo "  SIGUIENTE PASO — no hay ninguna plantilla lista todavía:"
  echo "    make prepare-image        # kernel + rootfs + preparación + catálogo"
fi
echo ""
echo "  Crear la primera VM:"
echo "    curl -s -X POST http://${HEALTH_HOST}/v1/vms -d '{\"template\":\"base-ubuntu-noble\"}'"
