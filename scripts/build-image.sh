#!/usr/bin/env bash
# COMPLETE image pipeline, from zero to a usable template:
#
#   1. kernel  — downloads the Firecracker CI vmlinux for the architecture
#   2. rootfs  — FLAVOR=alpine (default: busybox, ~10 MB), FLAVOR=ubuntu
#                (debootstrap noble, ~1 GiB) or FLAVOR=ubuntu-docker (noble +
#                Docker Engine, a development template); correct arch, cross
#                with qemu
#   3. prepare — vsock exec listener + SSH + DNS (prepare-image.sh; ubuntu
#                only — the Alpine build already ships with vsock + DNS ready)
#   4. store   — installs golden + kernel into the CoW store (same FS: reflink)
#   5. catalog — registers/updates the template in /var/lib/microhosted/catalog.json
#
# Normal entry point: `make prepare-image` (accepts ARCH=, FLAVOR=, IMAGE_NAME=,
# SIZE_MB=, KERNEL_VERSION=, EXTRA_PKGS=). Requires the host already configured
# (make full-install or make setup-host): the store must exist.
#
# Cross-arch (e.g. building the aarch64 image on the x86 PC to carry to an ARM64 target):
# works via qemu-user-static, but the resulting kernel/rootfs have to be copied
# to the target machine's store by hand.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
  *) echo "ERROR: unsupported architecture: $ARCH (use x86_64 or aarch64)" >&2; exit 1 ;;
esac

# Rootfs flavor. The name/size defaults depend on it, so empty
# IMAGE_NAME/SIZE_MB/DISK_MB take the default of the chosen flavor.
FLAVOR="${FLAVOR:-alpine}"
case "$FLAVOR" in
  alpine)
    IMAGE_NAME="${IMAGE_NAME:-base-alpine}"
    SIZE_MB="${SIZE_MB:-128}"
    DISK_MB="${DISK_MB:-512}"
    VCPUS="${VCPUS:-1}"
    MEM_MB="${MEM_MB:-128}"
    DESCRIPTION="Ultra-minimal Alpine, busybox init + vsock exec (make prepare-image)"
    ;;
  ubuntu)
    IMAGE_NAME="${IMAGE_NAME:-base-ubuntu-noble}"
    SIZE_MB="${SIZE_MB:-1024}"
    DISK_MB="${DISK_MB:-1024}"
    VCPUS="${VCPUS:-1}"
    MEM_MB="${MEM_MB:-128}"
    DESCRIPTION="Ubuntu noble (debootstrap, make prepare-image FLAVOR=ubuntu)"
    ;;
  ubuntu-docker)
    # Development template: dockerd + containerd idle at ~150 MB and images
    # pile up quickly, so the defaults are sized for a workstation, not a
    # service. The golden stays small; each clone grows to DISK_MB.
    IMAGE_NAME="${IMAGE_NAME:-dev-ubuntu}"
    SIZE_MB="${SIZE_MB:-2048}"
    DISK_MB="${DISK_MB:-8192}"
    VCPUS="${VCPUS:-2}"
    MEM_MB="${MEM_MB:-1024}"
    DESCRIPTION="Ubuntu noble + Docker Engine, development (make prepare-image FLAVOR=ubuntu-docker)"
    ;;
  *) echo "ERROR: unsupported FLAVOR: $FLAVOR (use alpine, ubuntu or ubuntu-docker)" >&2; exit 1 ;;
esac

KERNEL_VERSION="${KERNEL_VERSION:-6.1.102}"
STORE="${INSTANCES_DIR:-/var/lib/microhosted/store}"
# The daemon's catalog: root-owned, as the daemon requires (see
# scripts/install-service.sh). Written with sudo below.
CATALOG="${CATALOG:-/var/lib/microhosted/catalog.json}"
EXTRA_PKGS="${EXTRA_PKGS:-}"   # alpine only: extra apk packages in the golden
SSH_PUBKEY="${SSH_PUBKEY:-}"   # alpine only: add sshd with this key

if [[ ! -d "$STORE/rootfs" || ! -d "$STORE/kernels" ]]; then
  echo "ERROR: the store $STORE isn't prepared (rootfs/ and kernels/ are missing)." >&2
  echo "       Run first: make full-install   (or make setup-host)" >&2
  exit 1
fi

echo "=============================================="
echo " Image '${IMAGE_NAME}' (${FLAVOR}, ${ARCH}, ${SIZE_MB}MB)"
echo "=============================================="
sudo -v

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# --- [1/5] Kernel -------------------------------------------------------------
echo ""
echo "==> [1/5] Kernel vmlinux-${KERNEL_VERSION} (${ARCH})..."
KERNEL_DST="$STORE/kernels/vmlinux-${KERNEL_VERSION}"
if [[ "$ARCH" != "$(uname -m)" ]]; then
  # cross: don't pollute the local store with another architecture's kernel
  KERNEL_DST="$STORE/kernels/vmlinux-${KERNEL_VERSION}-${ARCH}"
fi
if sudo test -f "$KERNEL_DST"; then
  echo "  already exists: $KERNEL_DST"
else
  ARCH="$ARCH" ./scripts/build-kernel.sh "$KERNEL_VERSION" "$TMP_DIR/kernels"
  sudo install -o root -g root -m 0644 \
    "$TMP_DIR/kernels/vmlinux-${KERNEL_VERSION}" "$KERNEL_DST"
  echo "  installed: $KERNEL_DST"
fi

# --- [2/5] Rootfs ---------------------------------------------------------------
echo ""
ROOTFS_TMP="$TMP_DIR/${IMAGE_NAME}.ext4"
if [[ "$FLAVOR" == "alpine" ]]; then
  echo "==> [2/5] Alpine rootfs (minirootfs, ${ARCH})..."
  sudo ARCH="$ARCH" ./scripts/build-rootfs-alpine.sh "$ROOTFS_TMP" "$SIZE_MB" \
    ${EXTRA_PKGS:+--add "$EXTRA_PKGS"} ${SSH_PUBKEY:+--ssh "$SSH_PUBKEY"}
else
  echo "==> [2/5] Ubuntu rootfs (debootstrap, ${ARCH})..."
  WITH_DOCKER=0
  [[ "$FLAVOR" == "ubuntu-docker" ]] && WITH_DOCKER=1
  sudo ARCH="$ARCH" WITH_DOCKER="$WITH_DOCKER" ./scripts/build-rootfs.sh "$ROOTFS_TMP" "$SIZE_MB"
fi

# --- [3/5] Preparation (vsock + SSH + DNS) --------------------------------------
echo ""
if [[ "$FLAVOR" == "alpine" ]]; then
  echo "==> [3/5] Preparation: included in the Alpine build (vsock + DNS) — nothing to do."
else
  echo "==> [3/5] Preparing the image (vsock exec + SSH + DNS)..."
  sudo ./scripts/prepare-image.sh "$ROOTFS_TMP"
fi

# --- [4/5] To the CoW store ----------------------------------------------------
echo ""
echo "==> [4/5] Installing the golden into the store..."
ROOTFS_DST="$STORE/rootfs/${IMAGE_NAME}.ext4"
sudo install -o root -g root -m 0644 "$ROOTFS_TMP" "$ROOTFS_DST"
echo "  installed: $ROOTFS_DST"

# --- [5/5] Catalog ---------------------------------------------------------------
echo ""
echo "==> [5/5] Updating the catalog (${CATALOG})..."
if [[ "$ARCH" != "$(uname -m)" ]]; then
  echo "  NOTE: cross-arch image (${ARCH}) — NOT registered in the local catalog."
  echo "  Copy these files to the ${ARCH} machine's store and add the entry there:"
  echo "    $KERNEL_DST"
  echo "    $ROOTFS_DST"
else
  sudo python3 - "$CATALOG" "$IMAGE_NAME" "$KERNEL_DST" "$ROOTFS_DST" \
             "$DESCRIPTION" "$VCPUS" "$MEM_MB" "$DISK_MB" <<'PY'
import json, os, sys

path, name, kernel, rootfs, description = sys.argv[1:6]
vcpus, mem_mb, disk_mb = (int(x) for x in sys.argv[6:9])

catalog = []
if os.path.exists(path):
    with open(path) as f:
        catalog = json.load(f)

entry = {
    "name": name,
    "description": description,
    "kernel_path": kernel,
    "rootfs_path": rootfs,
    "vcpus": vcpus,
    "mem_mb": mem_mb,
    "disk_mb": disk_mb,
}
catalog = [t for t in catalog if t.get("name") != name]
catalog.append(entry)

# Written to a temp file and renamed: the daemon never sees a half-written
# catalog, and the result is root:root 0644 whatever the umask.
tmp = path + ".tmp"
with open(tmp, "w") as f:
    json.dump(catalog, f, indent=2, ensure_ascii=False)
    f.write("\n")
os.chown(tmp, 0, 0)
os.chmod(tmp, 0o644)
os.replace(tmp, path)
print(f"  template '{name}' registered (vcpus={vcpus}, mem={mem_mb}MB, disk={disk_mb}MB)")
PY

  # The daemon reads the catalog at startup: restart it so it sees the new
  # template (live VMs survive: KillMode=process + reconcile).
  if systemctl is-active --quiet microhosted 2>/dev/null; then
    echo "  restarting the daemon to reload the catalog..."
    sudo systemctl restart microhosted
  fi
fi

echo ""
echo "=============================================="
echo " Image ready"
echo "=============================================="
if [[ "$ARCH" == "$(uname -m)" ]]; then
  echo "  Test it:"
  echo "    sudo curl -s --unix-socket /run/microhosted.sock -X POST http://localhost/v1/vms -d '{\"template\":\"${IMAGE_NAME}\"}'"
  echo "    sudo curl -s --unix-socket /run/microhosted.sock -X POST http://localhost/v1/vms/<id>/exec -d '{\"cmd\":\"uname -a\"}'"
fi
