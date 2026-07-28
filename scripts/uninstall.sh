#!/usr/bin/env bash
# COMPLETE uninstallation of MicroHosted on this machine — the inverse of
# full-install.sh. Returns the system to its original state:
#
#   1. kills the live VMs (KillMode=process leaves them running when the
#      service stops, so they have to be killed explicitly)
#   2. stops, disables, and removes the systemd service
#   3. deletes the nftables table 'inet microhosted' (our only table;
#      doesn't touch the host's tables or Docker's)
#   4. deletes the mhbr* bridges and their taps (only ours: the mhbr prefix
#      is our own; docker0/br-* are left intact)
#   5. deletes the daemon's cgroups (/sys/fs/cgroup/microhosted and the
#      Jailer's /sys/fs/cgroup/firecracker)
#   6. unmounts the CoW store, removes its /etc/fstab line, and deletes the
#      btrfs image file and all of /var/lib/microhosted
#   7. deletes the installed binaries (microhosted; firecracker and jailer
#      unless KEEP_FC=1) and the repo's state DB
#
# Idempotent: it can be re-run over a partial installation without failing.
#
# Usage:  sudo ./scripts/uninstall.sh
#         DRY_RUN=1  only shows what it would do, touching nothing
#         KEEP_FC=1  keeps firecracker/jailer in /usr/local/bin
#         PURGE=1    also deletes the repo's image artifacts
#                    (images/kernels, rootfs, instances, keys)
#
# NOT reverted (warned about at the end): apt packages (nftables, btrfs-progs,
# iproute2), net.ipv4.ip_forward, /dev/kvm permissions and the kvm group.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTANCES_DIR="${INSTANCES_DIR:-/var/lib/microhosted/store}"
STATE_ROOT="$(dirname "$INSTANCES_DIR")"                 # /var/lib/microhosted
COW_IMG="${COW_IMG:-$STATE_ROOT/instances.btrfs}"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"
UNIT="/etc/systemd/system/microhosted.service"

if [[ $EUID -ne 0 ]]; then
  echo "ERROR: run with sudo or as root." >&2
  exit 1
fi

DRY_RUN="${DRY_RUN:-}"
run() {
  if [[ -n "$DRY_RUN" ]]; then
    echo "  [dry-run] $*"
  else
    "$@"
  fi
}

echo "=============================================="
echo " MicroHosted — complete uninstallation"
[[ -n "$DRY_RUN" ]] && echo " (DRY_RUN: nothing is touched)" || true
echo "=============================================="

# --- [1/7] Live VMs ----------------------------------------------------------
# Stop the service BEFORE killing the VMs: otherwise Restart=on-failure +
# reconcile could re-adopt them or relaunch the daemon mid-cleanup.
echo ""
echo "==> [1/7] Stopping the service and killing live VMs..."
if systemctl list-unit-files microhosted.service &>/dev/null && \
   systemctl is-active --quiet microhosted 2>/dev/null; then
  run systemctl stop microhosted
fi

# The VMs are firecracker processes whose root (the Jailer chroot) lives inside
# the store — that check avoids killing an unrelated firecracker. Jailers still
# without an exec are also included (they're ephemeral and always ours).
VM_PIDS=()
for pid in $(pgrep -x firecracker 2>/dev/null || true); do
  root="$(readlink "/proc/$pid/root" 2>/dev/null || true)"
  if [[ "$root" == "$INSTANCES_DIR"/* ]]; then VM_PIDS+=("$pid"); fi
done
for pid in $(pgrep -x jailer 2>/dev/null || true); do
  VM_PIDS+=("$pid")
done

if [[ ${#VM_PIDS[@]} -gt 0 ]]; then
  echo "  killing ${#VM_PIDS[@]} VM(s): ${VM_PIDS[*]}"
  run kill -TERM "${VM_PIDS[@]}" 2>/dev/null || true
  if [[ -z "$DRY_RUN" ]]; then
    for _ in $(seq 1 10); do
      alive=0
      for pid in "${VM_PIDS[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then alive=1; fi
      done
      if [[ "$alive" -eq 0 ]]; then break; fi
      sleep 1
    done
    for pid in "${VM_PIDS[@]}"; do kill -KILL "$pid" 2>/dev/null || true; done
  fi
else
  echo "  no live VMs"
fi

# --- [2/7] systemd service ----------------------------------------------------
echo ""
echo "==> [2/7] Removing the systemd service..."
if [[ -f "$UNIT" ]]; then
  run systemctl disable --now microhosted 2>/dev/null || true
  run rm -f "$UNIT"
  run systemctl daemon-reload
  run systemctl reset-failed microhosted 2>/dev/null || true
  echo "  $UNIT: removed"
else
  echo "  wasn't installed"
fi

# --- [3/7] nftables --------------------------------------------------------------
echo ""
echo "==> [3/7] Removing the nftables table 'inet microhosted'..."
if command -v nft &>/dev/null && nft list table inet microhosted &>/dev/null; then
  run nft delete table inet microhosted
  echo "  table removed"
else
  echo "  didn't exist"
fi

# --- [4/7] Bridges and taps ---------------------------------------------------------
echo ""
echo "==> [4/7] Removing mhbr* bridges and their taps..."
BRIDGES="$(ip -o link show type bridge 2>/dev/null | awk -F': ' '{print $2}' | grep '^mhbr' || true)"
if [[ -n "$BRIDGES" ]]; then
  for br in $BRIDGES; do
    # The taps are persistent (they outlive the process): delete them explicitly.
    for tap in $(ip -o link show master "$br" 2>/dev/null | awk -F': ' '{print $2}' | cut -d@ -f1); do
      run ip link del "$tap" 2>/dev/null || true
    done
    run ip link del "$br"
    echo "  $br: removed"
  done
else
  echo "  no mhbr* bridges"
fi

# --- [5/7] cgroups -----------------------------------------------------------------
echo ""
echo "==> [5/7] Removing cgroups..."
for cgparent in /sys/fs/cgroup/microhosted /sys/fs/cgroup/firecracker; do
  if [[ -d "$cgparent" ]]; then
    for d in "$cgparent"/*/; do
      if [[ -d "$d" ]]; then run rmdir "$d" 2>/dev/null || true; fi
    done
    run rmdir "$cgparent" 2>/dev/null || true
    echo "  $cgparent: removed"
  fi
done

# --- [6/7] CoW store ---------------------------------------------------------------
echo ""
echo "==> [6/7] Unmounting and deleting the CoW store..."
if mountpoint -q "$INSTANCES_DIR" 2>/dev/null; then
  UMOUNT_OK=0
  for _ in $(seq 1 5); do
    if run umount "$INSTANCES_DIR" 2>/dev/null; then UMOUNT_OK=1; break; fi
    sleep 1
  done
  if [[ -n "$DRY_RUN" ]]; then UMOUNT_OK=1; fi
  if [[ "$UMOUNT_OK" -ne 1 ]]; then
    echo "ERROR: couldn't unmount $INSTANCES_DIR (target is busy)." >&2
    echo "       Check what's using it:  sudo lsof +f -- $INSTANCES_DIR" >&2
    echo "       and re-run the uninstall." >&2
    exit 1
  fi
  echo "  $INSTANCES_DIR: unmounted"
fi

# Remove the loopback's fstab line (by image, so it also covers historical mounts
# at another point) and refresh the generated .mount units.
if grep -qF "$COW_IMG" /etc/fstab 2>/dev/null; then
  run sed -i "\#${COW_IMG}#d" /etc/fstab
  run systemctl daemon-reload
  echo "  /etc/fstab line: removed"
fi

# If a loop device is left hanging off the image, detach it before deleting it.
for loopdev in $(losetup -j "$COW_IMG" 2>/dev/null | cut -d: -f1); do
  run losetup -d "$loopdev" 2>/dev/null || true
done

if [[ -d "$STATE_ROOT" ]]; then
  # Double safety: never delete the tree with something still mounted inside.
  # (findmnt -R won't do: it only checks whether the target ITSELF is a
  # mountpoint, not its children.)
  if [[ -z "$DRY_RUN" ]] && findmnt -rn -o TARGET | grep -Eq "^${STATE_ROOT}(/|\$)"; then
    echo "ERROR: there's still a filesystem mounted under $STATE_ROOT — deleting nothing." >&2
    exit 1
  fi
  run rm -rf "$STATE_ROOT"
  echo "  $STATE_ROOT: deleted (btrfs image included)"
else
  echo "  $STATE_ROOT didn't exist"
fi

# --- [7/7] Repo binaries and state ------------------------------------------------
echo ""
echo "==> [7/7] Removing binaries and the state DB..."
run rm -f "$BIN_DIR/microhosted"
echo "  $BIN_DIR/microhosted: removed"
if [[ -n "${KEEP_FC:-}" ]]; then
  echo "  firecracker/jailer: kept (KEEP_FC=1)"
else
  run rm -f "$BIN_DIR/firecracker" "$BIN_DIR/jailer"
  echo "  $BIN_DIR/{firecracker,jailer}: removed"
fi

# The DB points to VMs/clones in the just-deleted store: leaving it only poisons
# a future reinstall with phantom state.
run rm -f "$REPO_ROOT/images/microhosted.db"
echo "  images/microhosted.db: removed"

if [[ -n "${PURGE:-}" ]]; then
  run rm -rf "$REPO_ROOT/images/kernels" "$REPO_ROOT/images/rootfs" \
             "$REPO_ROOT/images/instances" "$REPO_ROOT/images/keys"
  echo "  PURGE: images/{kernels,rootfs,instances,keys} removed"
fi

echo ""
echo "=============================================="
echo " Uninstallation complete"
echo "=============================================="
echo "  Left on the system (revert by hand if you want):"
echo "    - apt packages: nftables, btrfs-progs, iproute2"
echo "    - net.ipv4.ip_forward=1 (if no other workloads use it:"
echo "      sudo sysctl -w net.ipv4.ip_forward=0)"
echo "    - /dev/kvm permissions and kvm group membership"
if [[ -z "${PURGE:-}" ]]; then
  echo "  The image artifacts remain in the repo (images/); PURGE=1 deletes them."
fi
echo "  The catalog (images/catalog.json) is versioned: git checkout restores it."
