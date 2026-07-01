#!/usr/bin/env bash
# Descarga firecracker y jailer de la misma versión desde GitHub Releases.
# Uso: ./scripts/install-fc.sh [VERSION] [INSTALL_DIR]
# Ejemplo: ./scripts/install-fc.sh v1.10.1 /usr/local/bin

set -euo pipefail

FC_VERSION="${1:-v1.10.1}"
INSTALL_DIR="${2:-/usr/local/bin}"
ARCH="$(uname -m)"

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

echo "==> Verificando versiones:"
firecracker --version
jailer --version

echo ""
echo "OK: firecracker y jailer instalados en ${INSTALL_DIR}"
echo "    Ambos binarios son version ${FC_VERSION} — requerido para que Jailer funcione."
