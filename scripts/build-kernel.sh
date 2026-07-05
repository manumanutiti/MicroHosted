#!/usr/bin/env bash
# Descarga el kernel mínimo que recomienda Firecracker (sin compilar desde cero).
# Para compilar desde fuente, ver docs/architecture.md.
# Uso: ./scripts/build-kernel.sh [VERSION] [OUTPUT_DIR]

set -euo pipefail

# Versión del kernel mantenida por Firecracker CI (sin prefijo "v" en el nombre de archivo)
FC_KERNEL_VERSION="${1:-6.1.102}"
OUTPUT_DIR="${2:-images/kernels}"

# Arquitectura del kernel: la de la máquina, o forzada con ARCH= (el bucket de
# Firecracker CI publica x86_64 y aarch64 con el mismo layout).
ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
  *) echo "ERROR: arquitectura no soportada: $ARCH (usa x86_64 o aarch64)" >&2; exit 1 ;;
esac

mkdir -p "$OUTPUT_DIR"

KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/${ARCH}/vmlinux-${FC_KERNEL_VERSION}"
OUTPUT_FILE="${OUTPUT_DIR}/vmlinux-${FC_KERNEL_VERSION}"

if [[ -f "$OUTPUT_FILE" ]]; then
  echo "Kernel ya descargado: ${OUTPUT_FILE}"
  exit 0
fi

echo "==> Descargando kernel ${FC_KERNEL_VERSION} para ${ARCH}..."
curl -fL "$KERNEL_URL" -o "$OUTPUT_FILE"
chmod 0644 "$OUTPUT_FILE"

echo ""
echo "OK: kernel en ${OUTPUT_FILE}"
echo "    Usar con: --kernel ${OUTPUT_FILE}"
