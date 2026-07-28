#!/usr/bin/env bash
# Downloads the minimal kernel Firecracker recommends (no building from scratch).
# To build from source, see docs/architecture.md.
# Usage: ./scripts/build-kernel.sh [VERSION] [OUTPUT_DIR]

set -euo pipefail

# Kernel version maintained by Firecracker CI (no "v" prefix in the filename)
FC_KERNEL_VERSION="${1:-6.1.102}"
OUTPUT_DIR="${2:-images/kernels}"

# Kernel architecture: this machine's, or forced with ARCH= (the Firecracker CI
# bucket publishes x86_64 and aarch64 with the same layout).
ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
  *) echo "ERROR: unsupported architecture: $ARCH (use x86_64 or aarch64)" >&2; exit 1 ;;
esac

mkdir -p "$OUTPUT_DIR"

KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/${ARCH}/vmlinux-${FC_KERNEL_VERSION}"
OUTPUT_FILE="${OUTPUT_DIR}/vmlinux-${FC_KERNEL_VERSION}"

if [[ -f "$OUTPUT_FILE" ]]; then
  echo "Kernel already downloaded: ${OUTPUT_FILE}"
  exit 0
fi

echo "==> Downloading kernel ${FC_KERNEL_VERSION} for ${ARCH}..."
curl -fL "$KERNEL_URL" -o "$OUTPUT_FILE"
chmod 0644 "$OUTPUT_FILE"

echo ""
echo "OK: kernel at ${OUTPUT_FILE}"
echo "    Use with: --kernel ${OUTPUT_FILE}"
