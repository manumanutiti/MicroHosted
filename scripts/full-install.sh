#!/usr/bin/env bash
# Full installation of MicroHosted on THIS machine, in a single step:
#
#   1. checks (architecture, KVM, Go)
#   2. host configuration (cgroups, nftables, btrfs CoW store)
#   3. firecracker + jailer (same version, architecture binaries)
#   4. building the daemon
#   5. systemd service (enable --now)
#   6. health check through the API
#
# Supports x86_64 and aarch64 (64-bit Raspberry Pi 4/5, Jetson, ARM gateways).
# Normal entry point: `make full-install` (accepts ARCH=, FC_VERSION=, ADDR=).
#
# Idempotent: re-running it updates the binary/service and doesn't touch live VMs
# (KillMode=process + reconcile). NOTE: with FC_VERSION=latest it may upgrade
# Firecracker — snapshots are tied to the version that created them.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
  *) echo "ERROR: unsupported architecture: $ARCH (use x86_64 or aarch64)" >&2; exit 1 ;;
esac
NATIVE="$(uname -m)"
if [[ "$ARCH" != "$NATIVE" ]]; then
  echo "ERROR: full-install must run ON the target machine ($ARCH)," >&2
  echo "       because KVM, cgroups, nftables, and the store are local." >&2
  echo "       To cross-compile only the binary:  make build ARCH=$ARCH" >&2
  echo "       To cross-build an image:            make prepare-image ARCH=$ARCH" >&2
  exit 1
fi

FC_VERSION="${FC_VERSION:-latest}"
ADDR="${ADDR:-:8080}"

echo "=============================================="
echo " MicroHosted — full installation (${ARCH})"
echo "=============================================="

# --- [1/6] Checks -----------------------------------------------------------
echo ""
echo "==> [1/6] Pre-flight checks..."

if [[ ! -e /dev/kvm ]]; then
  echo "ERROR: /dev/kvm doesn't exist — no KVM means no microVMs." >&2
  if [[ "$ARCH" == "aarch64" ]]; then
    echo "  On Raspberry Pi: use a 64-bit OS (Raspberry Pi OS 64-bit or" >&2
    echo "  Ubuntu Server arm64); on Pi 4/5, KVM ships in the stock kernel." >&2
    echo "  Check: ls /dev/kvm ; zgrep KVM /proc/config.gz" >&2
  else
    echo "  Enable VT-x/AMD-V in the BIOS (or nested virtualization if it's a VM)." >&2
  fi
  exit 1
fi
echo "  /dev/kvm: OK"

if ! command -v go &>/dev/null; then
  echo "ERROR: Go (1.22+) is missing. Install it: https://go.dev/dl/ or 'sudo snap install go --classic'" >&2
  exit 1
fi
echo "  $(go version): OK"

# Ask for sudo once at the start, not halfway through the installation.
echo "  (sudo is needed to configure the host, binaries, and service)"
sudo -v

# --- [2/6] Host: cgroups, nftables, CoW store -------------------------------
echo ""
echo "==> [2/6] Configuring the host (setup-host.sh)..."
sudo ./scripts/setup-host.sh

# --- [3/6] Firecracker + Jailer ----------------------------------------------
echo ""
echo "==> [3/6] Installing firecracker + jailer (${FC_VERSION})..."
if command -v firecracker &>/dev/null; then
  echo "  current version: $(firecracker --version | head -1)"
fi
ARCH="$ARCH" ./scripts/install-fc.sh "$FC_VERSION"

# --- [4/6] Build the daemon --------------------------------------------------
echo ""
echo "==> [4/6] Building microhosted..."
mkdir -p build
CGO_ENABLED=0 go build -o build/microhosted ./cmd/microhosted
echo "  build/microhosted: OK"

# --- [5/6] systemd service ---------------------------------------------------
echo ""
echo "==> [5/6] Installing the systemd service..."
sudo ./scripts/install-service.sh "$ADDR"
sudo systemctl enable --now microhosted
sudo systemctl restart microhosted   # if it was already running, pick up the new binary

# --- [6/6] Verification ------------------------------------------------------
echo ""
echo "==> [6/6] Checking API health..."
HEALTH_HOST="${ADDR}"
[[ "$HEALTH_HOST" == :* ]] && HEALTH_HOST="localhost${HEALTH_HOST}"
HEALTH_OK=0
for _ in $(seq 1 15); do
  if curl -fsS "http://${HEALTH_HOST}/v1/health" >/dev/null 2>&1; then
    HEALTH_OK=1
    break
  fi
  sleep 1
done
if [[ "$HEALTH_OK" -eq 1 ]]; then
  echo "  GET /v1/health: OK"
  curl -fsS "http://${HEALTH_HOST}/v1/health" 2>/dev/null || true
  echo ""
else
  echo "  WARN: the API isn't responding yet at http://${HEALTH_HOST}/v1/health" >&2
  echo "        check the log: journalctl -u microhosted -n 50" >&2
fi

# Is there any usable template? (a catalog with existing kernel+rootfs)
MISSING="$(python3 - images/catalog.json <<'PY' 2>/dev/null || true
import json, os, sys
try:
    cat = json.load(open(sys.argv[1]))
except Exception:
    cat = []
usable = [t for t in cat
          if os.path.exists(t.get("kernel_path","")) and os.path.exists(t.get("rootfs_path",""))]
print("ok" if usable else "none")
PY
)"

echo ""
echo "=============================================="
echo " Installation complete (${ARCH})"
echo "=============================================="
echo "  API:      http://${HEALTH_HOST}"
echo "  logs:     journalctl -u microhosted -f"
echo "  status:   curl -s http://${HEALTH_HOST}/v1/system | python3 -m json.tool"
if [[ "$MISSING" != "ok" ]]; then
  echo ""
  echo "  NEXT STEP — there's no template ready yet:"
  echo "    make prepare-image        # kernel + rootfs + preparation + catalog"
fi
echo ""
echo "  Create the first VM:"
echo "    curl -s -X POST http://${HEALTH_HOST}/v1/vms -d '{\"template\":\"base-alpine\"}'"
