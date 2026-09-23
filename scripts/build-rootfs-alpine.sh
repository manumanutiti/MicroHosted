#!/usr/bin/env bash
# Generates an ULTRA-MINIMAL rootfs.ext4 for microVMs using the Alpine
# minirootfs (~3 MB downloaded, ~10 MB installed vs ~1 GiB for the Ubuntu of
# build-rootfs.sh). No systemd: busybox init + inittab. The goal is density —
# the dirty working set of an idle guest drops from ~100 MB (systemd) to ~10 MB,
# which is what decides how many microVMs fit on a host.
#
# The image comes out READY to use: it includes what prepare-image.sh adds to
# the Ubuntu images (vsock listener on port 52, resolv.conf → /proc/net/pnp), so
# there's NO need to run prepare-image.sh on it (which also assumes systemd).
#
# Per-use variants ("add a python to it"): --add installs extra apk packages in
# the golden. Each variant is a distinct golden; the per-VM clones are still CoW
# reflinks, so 50 VMs of the python variant share the disk.
#
# Supports x86_64 and aarch64, native or cross (qemu-user-static + binfmt, like
# build-rootfs.sh). Normal entry point: `make prepare-image` (FLAVOR=alpine, the
# default flavor, via build-image.sh).
#
# Usage: sudo ./scripts/build-rootfs-alpine.sh [OUTPUT] [SIZE_MB] [--add pkg1,pkg2] [--ssh key.pub]
#      sudo ./scripts/build-rootfs-alpine.sh images/rootfs/alpine.ext4
#      sudo ./scripts/build-rootfs-alpine.sh images/rootfs/alpine-py.ext4 512 --add python3
#      ARCH=aarch64 sudo -E ./scripts/build-rootfs-alpine.sh images/rootfs/pi.ext4
#
# Like the Ubuntu goldens: ext4 over the whole file, no partition table
# (CloneRootfs grows them per VM with offline resize2fs).

set -euo pipefail

OUTPUT="images/rootfs/alpine.ext4"
SIZE_MB=128
EXTRA_PKGS=""
SSH_PUBKEY=""
AGENT_PORT=52
ALPINE_BRANCH="${ALPINE_BRANCH:-v3.22}"
ALPINE_VER="${ALPINE_VER:-3.22.0}"

positional=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --add) EXTRA_PKGS="${2//,/ }"; shift 2 ;;
    --ssh) SSH_PUBKEY="$2"; shift 2 ;;
    *)
      case "$positional" in
        0) OUTPUT="$1" ;;
        1) SIZE_MB="$1" ;;
        *) echo "ERROR: unexpected argument: $1" >&2; exit 1 ;;
      esac
      positional=$((positional + 1)); shift ;;
  esac
done

ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
  *) echo "ERROR: unsupported architecture: $ARCH (use x86_64 or aarch64)" >&2; exit 1 ;;
esac
# Cross-arch: the chroot binaries (apk, ssh-keygen) are for another CPU —
# qemu-user-static + binfmt is needed so the kernel runs them transparently
# (same mechanism as build-rootfs.sh).
CROSS=0
QEMU_BIN=""
if [[ "$ARCH" != "$(uname -m)" ]]; then
  CROSS=1
  case "$ARCH" in
    aarch64) QEMU_BIN=/usr/bin/qemu-aarch64-static ;;
    x86_64)  QEMU_BIN=/usr/bin/qemu-x86_64-static ;;
  esac
  if [[ ! -x "$QEMU_BIN" ]]; then
    echo "==> Installing qemu-user-static (cross $(uname -m) → ${ARCH})..."
    sudo apt-get install -y qemu-user-static binfmt-support
  fi
fi

TARBALL="alpine-minirootfs-${ALPINE_VER}-${ARCH}.tar.gz"
URL="${MINIROOTFS_URL:-https://dl-cdn.alpinelinux.org/alpine/${ALPINE_BRANCH}/releases/${ARCH}/${TARBALL}}"
CACHE_DIR="images/cache"
MOUNT_DIR="$(mktemp -d /tmp/mh-alpine-XXXXXX)"

cleanup() {
  sudo umount "$MOUNT_DIR" 2>/dev/null || true
  rmdir "$MOUNT_DIR" 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p "$(dirname "$OUTPUT")" "$CACHE_DIR"

if [[ ! -f "$CACHE_DIR/$TARBALL" ]]; then
  echo "==> Downloading $TARBALL..."
  curl -fL -o "$CACHE_DIR/$TARBALL.tmp" "$URL"
  mv "$CACHE_DIR/$TARBALL.tmp" "$CACHE_DIR/$TARBALL"
fi

echo "==> Creating a ${SIZE_MB} MB ext4 at $OUTPUT..."
rm -f "$OUTPUT"
truncate -s "${SIZE_MB}M" "$OUTPUT"
mkfs.ext4 -F -q -L microhosted-alpine "$OUTPUT"
sudo mount -o loop "$OUTPUT" "$MOUNT_DIR"

echo "==> Extracting the minirootfs..."
sudo tar -xzf "$CACHE_DIR/$TARBALL" -C "$MOUNT_DIR"

# Cross: the qemu interpreter has to exist INSIDE the chroot; it's removed at
# the end so the golden stays clean.
if [[ "$CROSS" -eq 1 ]]; then
  sudo cp "$QEMU_BIN" "$MOUNT_DIR/usr/bin/"
fi

# apk needs networking inside the chroot: the host's resolv.conf, only during the
# build (at the end it's replaced by the symlink to /proc/net/pnp).
sudo cp /etc/resolv.conf "$MOUNT_DIR/etc/resolv.conf"

echo "==> Installing packages (socat${EXTRA_PKGS:+ $EXTRA_PKGS}${SSH_PUBKEY:+ openssh})..."
sudo chroot "$MOUNT_DIR" /sbin/apk add --no-cache socat ${EXTRA_PKGS} \
  ${SSH_PUBKEY:+openssh}

# The vsock listener: the SAME agent and contract that prepare-image.sh installs
# on the Ubuntu images (multiplexes exec/PUT/GET over the first line). Pure POSIX
# sh — runs the same in busybox ash.
echo "==> Installing the vsock listener (microhosted-exec, port ${AGENT_PORT})..."
sudo tee "$MOUNT_DIR/usr/local/bin/microhosted-exec" >/dev/null <<'AGENT'
#!/bin/sh
IFS= read -r line
verb=${line%% *}
case "$verb" in
  PUT)
    rest=${line#PUT }
    path=${rest% *}
    len=${rest##* }
    mkdir -p "$(dirname "$path")" 2>/dev/null
    head -c "$len" > "$path"
    echo "___MICROHOSTED_EXIT___:$?"
    ;;
  GET)
    path=${line#GET }
    if [ -f "$path" ]; then
      len=$(wc -c < "$path")
      printf 'OK %s\n' "$len"
      cat "$path"
    else
      printf 'ERR no such file: %s\n' "$path"
    fi
    ;;
  *)
    sh -c "$line" 2>&1
    echo "___MICROHOSTED_EXIT___:$?"
    ;;
esac
AGENT
sudo chmod 0755 "$MOUNT_DIR/usr/local/bin/microhosted-exec"

# Init: busybox directly, no OpenRC — a microVM doesn't need a service manager.
# sysinit mounts the pseudo-fs (idempotent: if the kernel already mounted
# devtmpfs, the mount fails and init continues), respawn keeps the agent alive.
# ctrlaltdel→reboot is how Firecracker powers off the guest (SendCtrlAltDel; in a
# microVM, reboot terminates the process, it doesn't restart).
echo "==> Configuring busybox init (inittab)..."
sudo tee "$MOUNT_DIR/etc/inittab" >/dev/null <<INITTAB
::sysinit:/bin/mount -t proc proc /proc
::sysinit:/bin/mount -t sysfs sysfs /sys
::sysinit:/bin/mount -t devtmpfs devtmpfs /dev
::sysinit:/bin/mkdir -p /dev/pts
::sysinit:/bin/mount -t devpts devpts /dev/pts
::respawn:/usr/bin/socat VSOCK-LISTEN:${AGENT_PORT},fork,reuseaddr EXEC:/usr/local/bin/microhosted-exec
ttyS0::respawn:/sbin/getty -L 115200 ttyS0 vt100
::ctrlaltdel:/sbin/reboot
::shutdown:/bin/umount -a -r
INITTAB

if [[ -n "$SSH_PUBKEY" ]]; then
  echo "==> Configuring SSH (key $SSH_PUBKEY)..."
  sudo chroot "$MOUNT_DIR" /usr/bin/ssh-keygen -A
  sudo mkdir -p "$MOUNT_DIR/root/.ssh"
  sudo cp "$SSH_PUBKEY" "$MOUNT_DIR/root/.ssh/authorized_keys"
  sudo chmod 700 "$MOUNT_DIR/root/.ssh"
  sudo chmod 600 "$MOUNT_DIR/root/.ssh/authorized_keys"
  echo "::respawn:/usr/sbin/sshd -D -e" | sudo tee -a "$MOUNT_DIR/etc/inittab" >/dev/null
fi

# DNS: same mechanism as prepare-image.sh — the kernel writes the nameservers
# microhosted assigns into /proc/net/pnp (via the SDK's ip=).
sudo ln -sf /proc/net/pnp "$MOUNT_DIR/etc/resolv.conf"

if [[ "$CROSS" -eq 1 ]]; then
  sudo rm -f "$MOUNT_DIR/usr/bin/$(basename "$QEMU_BIN")"
fi

sudo umount "$MOUNT_DIR"

USED_MB=$(du -m "$OUTPUT" | cut -f1)
echo ""
echo "OK: $OUTPUT (${SIZE_MB} MB, ~${USED_MB} MB real)."
echo "    Ready to use as a golden — does NOT need prepare-image.sh."
echo "    Access: vsock exec (port ${AGENT_PORT})${SSH_PUBKEY:+ + SSH root}."
