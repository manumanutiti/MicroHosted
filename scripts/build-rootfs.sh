#!/usr/bin/env bash
# Generates a minimal rootfs.ext4 for microVMs using debootstrap.
# Supports x86_64 and aarch64 (native or cross with qemu-user-static).
#
# Usage: sudo ./scripts/build-rootfs.sh [OUTPUT] [SIZE_MB]
#      ARCH=aarch64 sudo -E ./scripts/build-rootfs.sh images/rootfs/pi.ext4 1024
#
# ALWAYS includes socat (required by the vsock listener that prepare-image.sh
# installs) and openssh-server (the interactive access path).
#
# WITH_DOCKER=1 also installs Docker Engine (docker.io from universe) for the
# development template (FLAVOR=ubuntu-docker in build-image.sh).

set -euo pipefail

OUTPUT="${1:-images/rootfs/base.ext4}"
SIZE_MB="${2:-1024}"
DISTRO="${DISTRO:-noble}"   # Ubuntu 24.04
INCLUDE="socat,openssh-server"
WITH_DOCKER="${WITH_DOCKER:-0}"

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
  sudo umount -R "$MOUNT_DIR" 2>/dev/null || true
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
    --components=main,universe "$DISTRO" "$MOUNT_DIR" "$MIRROR"
else
  echo "==> debootstrap ${DISTRO}/${DEBARCH} (cross, two stages with qemu)..."
  sudo debootstrap --foreign --arch="$DEBARCH" --include="$INCLUDE" \
    --components=main,universe "$DISTRO" "$MOUNT_DIR" "$MIRROR"
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

if [[ "$WITH_DOCKER" == "1" ]]; then
  echo "==> Installing Docker Engine (docker.io)..."
  # apt in the chroot needs /proc, /sys and /dev; policy-rc.d keeps the
  # postinst scripts from trying to start daemons on the build host.
  sudo mount -t proc proc "$MOUNT_DIR/proc"
  sudo mount -t sysfs sysfs "$MOUNT_DIR/sys"
  sudo mount --bind /dev "$MOUNT_DIR/dev"
  printf '#!/bin/sh\nexit 101\n' | sudo tee "$MOUNT_DIR/usr/sbin/policy-rc.d" >/dev/null
  sudo chmod 0755 "$MOUNT_DIR/usr/sbin/policy-rc.d"

  sudo chroot "$MOUNT_DIR" /bin/bash -euo pipefail <<'CHROOT'
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y --no-install-recommends \
      docker.io containerd runc iptables ca-certificates curl git
    # The Firecracker guest kernel ships the legacy xtables modules but not
    # nf_tables: noble's default iptables-nft backend would make dockerd fail
    # to set up its bridge NAT.
    update-alternatives --set iptables /usr/sbin/iptables-legacy
    update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy
    apt-get clean
    rm -rf /var/lib/apt/lists/*
CHROOT

  sudo rm -f "$MOUNT_DIR/usr/sbin/policy-rc.d"
  sudo umount "$MOUNT_DIR/dev" "$MOUNT_DIR/sys" "$MOUNT_DIR/proc"
  sudo systemctl --root="$MOUNT_DIR" enable containerd.service docker.service 2>/dev/null || true
fi

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
