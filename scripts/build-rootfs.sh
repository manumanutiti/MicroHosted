#!/usr/bin/env bash
# Generates a minimal rootfs.ext4 for microVMs using debootstrap.
# Supports x86_64 and aarch64 (native or cross with qemu-user-static).
#
# Usage: sudo ./scripts/build-rootfs.sh [OUTPUT] [SIZE_MB]
#      ARCH=aarch64 sudo -E ./scripts/build-rootfs.sh images/rootfs/pi.ext4 1024
#
# ALWAYS includes socat (required by the vsock listener that prepare-image.sh
# installs) and openssh-server (the interactive access path).

set -euo pipefail

OUTPUT="${1:-images/rootfs/base.ext4}"
SIZE_MB="${2:-1024}"
DISTRO="${DISTRO:-noble}"   # Ubuntu 24.04
INCLUDE="socat,openssh-server"

ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
  *) echo "ERROR: unsupported architecture: $ARCH (use x86_64 or aarch64)" >&2; exit 1 ;;
esac

# Debian architecture + mirror. NOTE: archive.ubuntu.com only serves amd64/i386;
# arm64 lives on ports.ubuntu.com — with the wrong mirror debootstrap fails with
# a cryptic "no packages found".
if [[ "$ARCH" == "x86_64" ]]; then
  DEBARCH=amd64
  MIRROR="${MIRROR:-http://archive.ubuntu.com/ubuntu}"
else
  DEBARCH=arm64
  MIRROR="${MIRROR:-http://ports.ubuntu.com/ubuntu-ports}"
fi

case "$(uname -m)" in
  x86_64) NATIVE_DEBARCH=amd64 ;;
  aarch64) NATIVE_DEBARCH=arm64 ;;
  *) NATIVE_DEBARCH="$(uname -m)" ;;
esac
CROSS=0
[[ "$DEBARCH" != "$NATIVE_DEBARCH" ]] && CROSS=1

if ! command -v debootstrap &>/dev/null; then
  echo "==> Installing debootstrap..."
  sudo apt-get install -y debootstrap
fi

# Cross-arch: the rootfs binaries (the .deb postinst, the configuration chroot)
# are for another CPU — qemu-user-static + binfmt is needed so the kernel runs
# them transparently.
QEMU_BIN=""
if [[ "$CROSS" -eq 1 ]]; then
  case "$DEBARCH" in
    arm64) QEMU_BIN=/usr/bin/qemu-aarch64-static ;;
    amd64) QEMU_BIN=/usr/bin/qemu-x86_64-static ;;
  esac
  if [[ ! -x "$QEMU_BIN" ]]; then
    echo "==> Installing qemu-user-static (cross ${NATIVE_DEBARCH} → ${DEBARCH})..."
    sudo apt-get install -y qemu-user-static binfmt-support
  fi
fi

TMP_DIR="$(mktemp -d)"
MOUNT_DIR="${TMP_DIR}/rootfs"
cleanup() {
  sudo umount "$MOUNT_DIR" 2>/dev/null || true
  sudo rm -rf "$TMP_DIR"
}
trap cleanup EXIT

echo "==> Creating a ${SIZE_MB}MB ${DEBARCH} rootfs at ${OUTPUT}..."
mkdir -p "$(dirname "$OUTPUT")"
truncate -s "${SIZE_MB}M" "$OUTPUT"   # sparse: only takes up what gets written
mkfs.ext4 -q -F "$OUTPUT"

echo "==> Mounting the image..."
mkdir -p "$MOUNT_DIR"
sudo mount -o loop "$OUTPUT" "$MOUNT_DIR"

if [[ "$CROSS" -eq 0 ]]; then
  echo "==> debootstrap ${DISTRO}/${DEBARCH} (native)..."
  sudo debootstrap --arch="$DEBARCH" --include="$INCLUDE" \
    "$DISTRO" "$MOUNT_DIR" "$MIRROR"
else
  echo "==> debootstrap ${DISTRO}/${DEBARCH} (cross, two stages with qemu)..."
  sudo debootstrap --foreign --arch="$DEBARCH" --include="$INCLUDE" \
    "$DISTRO" "$MOUNT_DIR" "$MIRROR"
  sudo cp "$QEMU_BIN" "$MOUNT_DIR/usr/bin/"
  sudo chroot "$MOUNT_DIR" /debootstrap/debootstrap --second-stage
fi

echo "==> Configuring a minimal guest..."
sudo chroot "$MOUNT_DIR" /bin/bash -euo pipefail <<'CHROOT'
  # Empty root password for the serial console
  passwd -d root

  # Hostname
  echo "microvm" > /etc/hostname

  # Minimal /etc/fstab
  echo "LABEL=rootfs / ext4 defaults,noatime 0 1" > /etc/fstab

  # Disable unnecessary services
  systemctl disable --now snapd.service 2>/dev/null || true
  systemctl disable --now apt-daily.service 2>/dev/null || true
CHROOT

# SSH enabled on first boot (offline unit link; runs nothing from the guest)
sudo systemctl --root="$MOUNT_DIR" enable ssh.service 2>/dev/null || true

if [[ "$CROSS" -eq 1 ]]; then
  sudo rm -f "$MOUNT_DIR/usr/bin/$(basename "$QEMU_BIN")"
fi

echo "==> Unmounting..."
sudo umount "$MOUNT_DIR"
sudo e2label "$OUTPUT" rootfs

echo ""
echo "OK: ${DEBARCH} rootfs generated at ${OUTPUT} (${SIZE_MB}MB)"
echo "    Next step: sudo ./scripts/prepare-image.sh ${OUTPUT}"
