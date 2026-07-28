#!/usr/bin/env bash
# Configures the host to run microVMs with Firecracker + Jailer.
# Verifies requirements, adjusts permissions, and prepares cgroups v2.
# Run with sudo or as root.

set -euo pipefail

echo "==> Checking the kernel..."
KERNEL_VERSION="$(uname -r)"
KERNEL_MAJOR="$(echo "$KERNEL_VERSION" | cut -d. -f1)"
KERNEL_MINOR="$(echo "$KERNEL_VERSION" | cut -d. -f2)"
if [[ "$KERNEL_MAJOR" -lt 5 || ("$KERNEL_MAJOR" -eq 5 && "$KERNEL_MINOR" -lt 10) ]]; then
  echo "WARN: kernel ${KERNEL_VERSION} — 5.10+ is recommended. There may be issues."
else
  echo "  kernel ${KERNEL_VERSION}: OK"
fi

echo "==> Checking KVM..."
if [[ ! -e /dev/kvm ]]; then
  echo "ERROR: /dev/kvm doesn't exist. Enable virtualization in the BIOS or turn on nested virt."
  exit 1
fi
if [[ ! -r /dev/kvm || ! -w /dev/kvm ]]; then
  echo "  Adjusting permissions on /dev/kvm for the kvm group..."
  sudo chown root:kvm /dev/kvm
  sudo chmod 0660 /dev/kvm
  sudo usermod -aG kvm "$USER"
  echo "  NOTE: re-login or run: newgrp kvm"
else
  echo "  /dev/kvm: OK"
fi

echo "==> Checking cgroups v2..."
if ! mount | grep -q "type cgroup2"; then
  echo "  Mounting cgroup2 at /sys/fs/cgroup..."
  sudo mount -t cgroup2 none /sys/fs/cgroup || true
fi
if mount | grep -q "type cgroup2"; then
  echo "  cgroup2: OK"
else
  echo "WARN: cgroup2 not mounted. Jailer may fail."
fi

echo "==> Checking iproute2 (ip tuntap)..."
if ! command -v ip &>/dev/null; then
  echo "  Installing iproute2..."
  sudo apt-get install -y iproute2
fi
echo "  iproute2: OK"

echo "==> Checking nftables (nft)..."
if ! command -v nft &>/dev/null; then
  echo "  Installing nftables..."
  sudo apt-get install -y nftables
fi
# The daemon needs it at startup: it applies the segmented-network policy
# (drop guest→host, isolation between networks, egress NAT) in its own table
# 'inet microhosted'. Without nft, network reconciliation fails at startup.
echo "  nftables: OK"

# The Jailer working directory (the chroot) NO LONGER goes in /srv/jailer: it has
# to be on the SAME filesystem as the rootfs clones, because Jailer hardlinks the
# rootfs/kernel into the chroot and a hardlink doesn't cross devices. With the CoW
# store that's the btrfs at <instances>/. That's why the chroot (and the kernel)
# live inside the store — they're created below, after mounting it.

# ---------------------------------------------------------------------------
# Copy-on-write (CoW) instances store.
#
# Per-VM clones are made with `cp --reflink=auto`: on a filesystem with reflink
# (btrfs, XFS with reflink) they're instant CoW — the golden rootfs is stored
# ONCE and each VM only costs what it writes. On plain ext4 `cp` falls back to a
# FULL copy: each VM = a whole copy of the rootfs, and with 1GB+ images the disk
# fills up in an instant.
#
# So it works on ANY Ubuntu host without repartitioning, if the instances
# directory isn't already on a reflink-capable FS, we mount a btrfs loopback
# there (a sparse image file). btrfs ships in every Ubuntu kernel; only
# btrfs-progs is needed to format it.
#
# IMPORTANT: reflink does NOT cross filesystems. For the clone to be CoW, the
# golden rootfs has to live on the SAME filesystem as the clones — that's why the
# goldens go in <store>/rootfs/, not in a separate images/rootfs on the host's
# ext4. If the golden is on another FS, `cp` falls back to a full copy even if the
# store is btrfs. The catalog points to <store>/rootfs.
#
# The store lives OUTSIDE the repo (default /var/lib/microhosted/store): it's
# root-owned runtime data (including the Jailer chroot), not sources — keeping it
# inside the code tree breaks tools like `go build ./...`.
# ---------------------------------------------------------------------------
echo "==> Checking the disk store (copy-on-write)..."
INSTANCES_DIR="${INSTANCES_DIR:-/var/lib/microhosted/store}"
COW_IMG="${COW_IMG:-/var/lib/microhosted/instances.btrfs}"
COW_SIZE_GB="${COW_SIZE_GB:-20}"

# Migration: if the store's btrfs is already mounted at ANOTHER point (e.g. the
# historical images/instances inside the repo), unmount it and remove its fstab
# line to remount it at INSTANCES_DIR. The service must be stopped with no live
# VMs (otherwise umount gives "target is busy").
LOOPDEV="$(sudo losetup -j "$COW_IMG" 2>/dev/null | cut -d: -f1 | head -1 || true)"
if [[ -n "$LOOPDEV" ]]; then
  OLD_MNT="$(findmnt -n -o TARGET --source "$LOOPDEV" 2>/dev/null | head -1 || true)"
  if [[ -n "$OLD_MNT" && "$OLD_MNT" != "$INSTANCES_DIR" ]]; then
    echo "  migrating store from $OLD_MNT to $INSTANCES_DIR..."
    if ! sudo umount "$OLD_MNT"; then
      echo "  ERROR: couldn't unmount $OLD_MNT — stop the service and destroy the VMs first:" >&2
      echo "         sudo systemctl stop microhosted" >&2
      exit 1
    fi
    sudo sed -i "\#[[:space:]]${OLD_MNT}[[:space:]]#d" /etc/fstab
  fi
fi

sudo mkdir -p "$INSTANCES_DIR"

# does the directory already support reflink? (a real test: it depends on the FS)
probe_reflink() {
  local dir="$1" src dst
  src="$(sudo mktemp "$dir/.reflink-probe.XXXXXX")" || return 1
  dst="${src}.clone"
  sudo dd if=/dev/zero of="$src" bs=4k count=1 status=none 2>/dev/null || true
  if sudo cp --reflink=always "$src" "$dst" 2>/dev/null; then
    sudo rm -f "$src" "$dst"; return 0
  fi
  sudo rm -f "$src" "$dst"; return 1
}

if probe_reflink "$INSTANCES_DIR"; then
  echo "  $INSTANCES_DIR already supports CoW (reflink): OK"
elif mountpoint -q "$INSTANCES_DIR"; then
  echo "  WARN: $INSTANCES_DIR is mounted but without reflink; switch it to btrfs/XFS-reflink"
  echo "        or the clones will be full copies."
else
  echo "  $INSTANCES_DIR doesn't support CoW; provisioning a btrfs loopback (${COW_SIZE_GB}GB)..."
  if ! command -v mkfs.btrfs &>/dev/null; then
    echo "  Installing btrfs-progs..."
    sudo apt-get install -y btrfs-progs
  fi
  sudo mkdir -p "$(dirname "$COW_IMG")"
  if [[ ! -f "$COW_IMG" ]]; then
    sudo truncate -s "${COW_SIZE_GB}G" "$COW_IMG"   # sparse: doesn't take GB until used
    sudo mkfs.btrfs -q "$COW_IMG"
  fi
  sudo mount -o loop,compress=zstd "$COW_IMG" "$INSTANCES_DIR"
  # Persist it so the mount survives reboots.
  FSTAB_LINE="$COW_IMG $INSTANCES_DIR btrfs loop,compress=zstd 0 0"
  if ! grep -qF "$COW_IMG" /etc/fstab; then
    echo "$FSTAB_LINE" | sudo tee -a /etc/fstab >/dev/null
  fi
  echo "  btrfs loopback mounted at $INSTANCES_DIR: OK"
fi

# Everything Jailer hardlinks or clones has to share a filesystem with the
# clones, so goldens, kernels, and the Jailer chroot live INSIDE the store:
#   <instances>/rootfs   -> golden rootfs (reflink CoW source)
#   <instances>/kernels  -> kernels (Jailer hardlinks them into the chroot)
#   <instances>/jailer   -> Jailer chroot base (hardlink destination)
# The catalog points to rootfs/ and kernels/; the daemon uses <instances>/jailer
# as the default chroot-base. Placing the golden rootfs/kernels is the image
# preparation's job.
sudo mkdir -p "$INSTANCES_DIR/rootfs" "$INSTANCES_DIR/kernels" "$INSTANCES_DIR/jailer"
sudo chown root:root "$INSTANCES_DIR/jailer"
sudo chmod 0755 "$INSTANCES_DIR/jailer"
echo "  store with rootfs/ kernels/ jailer/ (same FS: reflink CoW + hardlinks OK)"

echo ""
echo "Host configured. Next step: scripts/install-fc.sh"
