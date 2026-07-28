#!/usr/bin/env bash
# Prepares a template (golden rootfs) for use with the platform. It sets up the
# two access paths to the VMs cloned from that image — which one to use in each
# case is the operator's call, not this script's:
#
#   - vsock (POST /v1/vms/{id}/exec): programmatic, needs no network or keys,
#     works even on VMs created with no_network:true.
#   - SSH: a real interactive shell, needs the VM to have a network.
#
# It's the ONLY image-preparation step — run once per golden rootfs
# (internal/storage.CloneRootfs copies whatever is in it to each VM), not once
# per VM.
#
# Usage: sudo ./scripts/prepare-image.sh <rootfs.ext4> [--no-ssh] [--no-vsock] [public-key.pub]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KEY_DIR="${SCRIPT_DIR}/../images/keys"
DEFAULT_PRIVATE_KEY="${KEY_DIR}/microhosted_ed25519"
DEFAULT_PUBKEY="${DEFAULT_PRIVATE_KEY}.pub"
AGENT_PORT=52

ROOTFS=""
PUBKEY="$DEFAULT_PUBKEY"
DO_SSH=1
DO_VSOCK=1

for arg in "$@"; do
  case "$arg" in
    --no-ssh) DO_SSH=0 ;;
    --no-vsock) DO_VSOCK=0 ;;
    *)
      if [[ -z "$ROOTFS" ]]; then
        ROOTFS="$arg"
      else
        PUBKEY="$arg"
      fi
      ;;
  esac
done

if [[ -z "$ROOTFS" ]]; then
  echo "usage: sudo ./scripts/prepare-image.sh <rootfs.ext4> [--no-ssh] [--no-vsock] [public-key.pub]"
  exit 1
fi

if [[ ! -f "$ROOTFS" ]]; then
  echo "ERROR: $ROOTFS doesn't exist"
  exit 1
fi

MOUNT_DIR="$(mktemp -d)"
cleanup() {
  sudo umount "$MOUNT_DIR" 2>/dev/null || true
  rmdir "$MOUNT_DIR" 2>/dev/null || true
}
trap cleanup EXIT

echo "==> Mounting ${ROOTFS}..."
sudo mount -o loop "$ROOTFS" "$MOUNT_DIR"

# DNS: the IP/gateway/nameservers we pass through firecracker-go-sdk
# (IPConfiguration) don't reach the guest as normal network config — the kernel
# writes them into /proc/net/pnp (a mechanism inherited from nfsroot/netboot; see
# the IPConfiguration comment in the SDK). For the guest to use them,
# /etc/resolv.conf has to be a symlink to /proc/net/pnp. Many images
# (systemd-resolved, netplan, cloud-init) leave it as a regular file or a symlink
# to their own stub — without this step, `ping`/`curl` to an IP work but
# resolving names (google.com) fails with "Temporary failure in name resolution"
# even though the VM has egress and real internet. Always done, regardless of
# --no-ssh/--no-vsock: it's basic network functionality, not part of access.
echo "==> Configuring guest DNS (/etc/resolv.conf -> /proc/net/pnp)..."
if [[ -e "$MOUNT_DIR/etc/resolv.conf" && ! -L "$MOUNT_DIR/etc/resolv.conf" ]]; then
  sudo mv "$MOUNT_DIR/etc/resolv.conf" "$MOUNT_DIR/etc/resolv.conf.microhosted-orig"
fi
sudo ln -sf /proc/net/pnp "$MOUNT_DIR/etc/resolv.conf"
echo "    OK: DNS resolution ready (uses the nameservers microhosted assigns to each VM)."

if [[ "$DO_VSOCK" -eq 1 ]]; then
  if [[ ! -x "$MOUNT_DIR/usr/bin/socat" ]]; then
    echo "ERROR: this image doesn't have socat installed (needed for the vsock channel)."
    echo "       install it inside the image or re-run with --no-vsock."
    exit 1
  fi

  echo "==> Installing the vsock listener (microhosted-exec, port ${AGENT_PORT})..."
  # The agent multiplexes three verbs over the same connection, based on the first
  # line: a bare command (exec, the historical contract), PUT (upload a file), and
  # GET (download a file). PUT/GET are the volume data channel: inject a sample or
  # pull artifacts with no network. It reads the whole first line with
  # `IFS= read -r` so as not to touch the command's spaces; the rest of stdin (a
  # PUT's bytes) is consumed by `head -c` byte by byte, so read doesn't eat it first.
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

  sudo tee "$MOUNT_DIR/etc/systemd/system/microhosted-exec.service" >/dev/null <<UNIT
[Unit]
Description=MicroHosted vsock exec listener

[Service]
ExecStart=/usr/bin/socat VSOCK-LISTEN:${AGENT_PORT},fork,reuseaddr EXEC:/usr/local/bin/microhosted-exec
Restart=always

[Install]
WantedBy=multi-user.target
UNIT

  sudo systemctl --root="$MOUNT_DIR" enable microhosted-exec.service
  echo "    OK: programmatic access (vsock) ready."
fi

if [[ "$DO_SSH" -eq 1 ]]; then
  if [[ "$PUBKEY" == "$DEFAULT_PUBKEY" && ! -f "$DEFAULT_PUBKEY" ]]; then
    echo "==> No project key yet, generating ${DEFAULT_PRIVATE_KEY}..."
    mkdir -p "$KEY_DIR"
    ssh-keygen -t ed25519 -f "$DEFAULT_PRIVATE_KEY" -N '' -C "microhosted (generated locally, do not commit)"
  fi

  if [[ ! -f "$PUBKEY" ]]; then
    echo "ERROR: $PUBKEY doesn't exist"
    exit 1
  fi

  echo "==> Adding ${PUBKEY} to /root/.ssh/authorized_keys..."
  sudo mkdir -p "$MOUNT_DIR/root/.ssh"
  sudo bash -c "cat '$PUBKEY' >> '$MOUNT_DIR/root/.ssh/authorized_keys'"
  sudo chmod 700 "$MOUNT_DIR/root/.ssh"
  sudo chmod 600 "$MOUNT_DIR/root/.ssh/authorized_keys"
  sudo chown -R 0:0 "$MOUNT_DIR/root/.ssh"
  echo "    OK: interactive access (SSH) ready."
fi

echo ""
echo "OK: ${ROOTFS} prepared."
echo "    Create a NEW VM (existing ones were cloned before this change) and:"
if [[ "$DO_VSOCK" -eq 1 ]]; then
  echo "    - run commands with: curl -X POST localhost:8080/v1/vms/<id>/exec -d '{\"cmd\":\"...\"}'"
fi
if [[ "$DO_SSH" -eq 1 ]]; then
  echo "    - get a shell with: ssh -i ${DEFAULT_PRIVATE_KEY} root@<guest_ip>"
fi
