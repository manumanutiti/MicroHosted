#!/usr/bin/env bash
# Downloads the minimal kernel Firecracker recommends (no building from scratch).
# To build from source, see docs/architecture.md.
# Usage: ./scripts/build-kernel.sh [VERSION] [OUTPUT_DIR]

set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib/pinned.sh"

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

PIN_KEY="kernel/${ARCH}/vmlinux-${FC_KERNEL_VERSION}"

if [[ -f "$OUTPUT_FILE" ]]; then
  echo "Kernel already downloaded: ${OUTPUT_FILE}"
  verify_pinned "$PIN_KEY" "$OUTPUT_FILE"
  exit 0
fi

echo "==> Downloading kernel ${FC_KERNEL_VERSION} for ${ARCH}..."
# Refused before it is fetched when the version is not pinned, and kept under
# its final name only once it matches.
[[ -n "$(pinned_sha256 "$PIN_KEY")" ]] || { verify_pinned "$PIN_KEY" /dev/null; exit 1; }
curl -fL "$KERNEL_URL" -o "$OUTPUT_FILE.tmp"
if ! verify_pinned "$PIN_KEY" "$OUTPUT_FILE.tmp"; then
  rm -f "$OUTPUT_FILE.tmp"
  exit 1
fi
chmod 0644 "$OUTPUT_FILE.tmp"
mv "$OUTPUT_FILE.tmp" "$OUTPUT_FILE"

echo ""
echo "OK: kernel at ${OUTPUT_FILE}"
echo "    Use with: --kernel ${OUTPUT_FILE}"
