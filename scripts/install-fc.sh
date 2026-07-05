#!/usr/bin/env bash
# Descarga firecracker y jailer de la misma versión desde GitHub Releases.
# Sin argumentos instala la versión FIJADA de la plataforma (la validada en
# hardware; se necesita >= 1.12 para network_overrides / forks simultáneos).
# "latest" instala la última release publicada — decisión consciente: los
# snapshots existentes van ligados a la versión que los creó.
# Uso: ./scripts/install-fc.sh [VERSION|latest] [INSTALL_DIR]
# Ejemplo: ./scripts/install-fc.sh v1.16.1 /usr/local/bin

set -euo pipefail

FC_VERSION="${1:-v1.16.1}"
INSTALL_DIR="${2:-/usr/local/bin}"

# Arquitectura: la de la máquina (los binarios se ejecutan AQUÍ; instalar los
# de otra arquitectura solo tiene sentido para copiarlos a otro equipo).
ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
esac

if [[ "$FC_VERSION" == "latest" ]]; then
  # /releases/latest redirige a /releases/tag/vX.Y.Z — resolver la versión
  # siguiendo el redirect evita depender de la API de GitHub (y sus límites).
  TAG_URL="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    https://github.com/firecracker-microvm/firecracker/releases/latest)"
  FC_VERSION="${TAG_URL##*/}"
  if [[ ! "$FC_VERSION" =~ ^v[0-9]+\.[0-9]+ ]]; then
    echo "ERROR: no pude resolver la última versión (obtuve: ${TAG_URL})"
    echo "       Pasa una versión explícita: $0 v1.16.1"
    exit 1
  fi
  echo "==> Última release: ${FC_VERSION}"
fi

# Firecracker solo soporta x86_64 y aarch64
if [[ "$ARCH" != "x86_64" && "$ARCH" != "aarch64" ]]; then
  echo "ERROR: arquitectura no soportada: $ARCH"
  exit 1
fi

RELEASE_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}"
TARBALL="firecracker-${FC_VERSION}-${ARCH}.tgz"

echo "==> Descargando Firecracker ${FC_VERSION} para ${ARCH}..."
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

curl -fsSL "${RELEASE_URL}/${TARBALL}" -o "${TMP_DIR}/${TARBALL}"
tar -xzf "${TMP_DIR}/${TARBALL}" -C "$TMP_DIR"

# El tarball contiene release-v*/firecracker-v*-x86_64 y release-v*/jailer-v*-x86_64
FC_BIN="$(find "$TMP_DIR" -name "firecracker-${FC_VERSION}-${ARCH}" | head -1)"
JAILER_BIN="$(find "$TMP_DIR" -name "jailer-${FC_VERSION}-${ARCH}" | head -1)"

if [[ -z "$FC_BIN" || -z "$JAILER_BIN" ]]; then
  echo "ERROR: no se encontraron los binarios en el tarball. Contenido:"
  find "$TMP_DIR" -type f
  exit 1
fi

echo "==> Instalando en ${INSTALL_DIR}..."
sudo install -o root -g root -m 0755 "$FC_BIN"     "${INSTALL_DIR}/firecracker"
sudo install -o root -g root -m 0755 "$JAILER_BIN" "${INSTALL_DIR}/jailer"

if [[ "$ARCH" == "$(uname -m)" ]]; then
  echo "==> Verificando versiones:"
  firecracker --version
  jailer --version
else
  echo "==> AVISO: binarios ${ARCH} instalados en una máquina $(uname -m) — no se"
  echo "    pueden ejecutar aquí; cópialos a la máquina destino."
fi

echo ""
echo "OK: firecracker y jailer instalados en ${INSTALL_DIR}"
echo "    Ambos binarios son version ${FC_VERSION} — requerido para que Jailer funcione."
echo ""
echo "RECUERDA:"
echo "  - Reinicia el daemon (sudo systemctl restart microhosted) para que"
echo "    detecte la nueva versión (capacidades como network_overrides se"
echo "    sondean al arrancar)."
echo "  - Los snapshots existentes van ligados a la versión de Firecracker"
echo "    que los creó: recréalos tras actualizar."
