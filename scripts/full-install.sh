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
# Supports x86_64 and aarch64 (ARM64 boards with a 64-bit kernel, Jetson, ARM gateways).
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
# No ADDR by default: the API serves on a Unix socket whose permissions are its
# authorization. Set ADDR=host:port to put it on an unauthenticated port instead.
ADDR="${ADDR:-}"
SOCKET="${SOCKET:-/run/microhosted.sock}"
SOCKET_GROUP="${SOCKET_GROUP:-}"
MANAGED_IFACE="${MANAGED_IFACE:-}"
MANAGED_HOST_ALLOW="${MANAGED_HOST_ALLOW:-}"
if [[ -n "$ADDR" ]]; then
  API_URL="http://${ADDR}"
  [[ "$ADDR" == :* ]] && API_URL="http://localhost${ADDR}"
  API_CURL=(curl -sS)
else
  API_URL="http://localhost"
  API_CURL=(sudo curl -sS --unix-socket "$SOCKET")
fi

echo "=============================================="
echo " MicroHosted — full installation (${ARCH})"
echo "=============================================="

# --- [1/6] Checks -----------------------------------------------------------
echo ""
echo "==> [1/6] Pre-flight checks..."

if [[ ! -e /dev/kvm ]]; then
  echo "ERROR: /dev/kvm doesn't exist — no KVM means no microVMs." >&2
  if [[ "$ARCH" == "aarch64" ]]; then
    echo "  On ARM64 boards: use a 64-bit OS (e.g. Ubuntu Server arm64) with a" >&2
    echo "  kernel built with KVM; most vendor arm64 kernels ship it." >&2
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
CGO_ENABLED=0 go build -o build/mh ./cmd/mh
echo "  build/microhosted, build/mh: OK"

# --- [5/6] systemd service ---------------------------------------------------
echo ""
echo "==> [5/6] Installing the systemd service..."
sudo SOCKET="$SOCKET" SOCKET_GROUP="$SOCKET_GROUP" MANAGED_IFACE="$MANAGED_IFACE" MANAGED_HOST_ALLOW="$MANAGED_HOST_ALLOW" ./scripts/install-service.sh "$ADDR"
sudo systemctl enable --now microhosted
sudo systemctl restart microhosted   # if it was already running, pick up the new binary

# --- [6/6] Verification ------------------------------------------------------
echo ""
echo "==> [6/6] Checking API health..."
# /v1/health answers 503 when any check fails, so this must NOT use curl -f: a
# degraded daemon is up and its body NAMES what is wrong (a store without CoW, a
# store below the free-space floor, egress rules it cannot enforce). Reading that
# as "the API isn't responding" sends an operator to journalctl for a problem
# already spelled out in the payload — and on a fresh device with a small disk
# it's the first thing they'd see.
HEALTH_BODY=""
HEALTH_CODE="000"
for _ in $(seq 1 15); do
  RAW="$("${API_CURL[@]}" -w '\n%{http_code}' "${API_URL}/v1/health" 2>/dev/null || true)"
  HEALTH_CODE="$(printf '%s' "$RAW" | tail -n1)"
  if [[ "$HEALTH_CODE" == "200" || "$HEALTH_CODE" == "503" ]]; then
    HEALTH_BODY="$(printf '%s' "$RAW" | sed '$d')"
    break
  fi
  sleep 1
done

DEGRADED=0
case "$HEALTH_CODE" in
  200)
    echo "  GET /v1/health: ok (every check passed)"
    ;;
  503)
    DEGRADED=1
    echo "  GET /v1/health: DEGRADED — the daemon is UP, but these checks failed:" >&2
    if ! printf '%s' "$HEALTH_BODY" | python3 -c '
import json, sys
try:
    checks = json.load(sys.stdin).get("checks", [])
except Exception:
    sys.exit(1)
for c in checks:
    if not c.get("ok"):
        print("    %-16s %s" % (c.get("name", "?"), c.get("detail", "")))
' 2>/dev/null; then
      printf '    %s\n' "$HEALTH_BODY"
    fi
    ;;
  *)
    echo "  WARN: the API isn't responding at ${API_URL}/v1/health" >&2
    echo "        check the log: journalctl -u microhosted -n 50" >&2
    ;;
esac
echo ""

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
if [[ -n "$ADDR" ]]; then
  echo "  API:      ${API_URL}  (tcp, UNAUTHENTICATED — root-equivalent)"
  echo "  status:   curl -s ${API_URL}/v1/system | python3 -m json.tool"
else
  echo "  API:      unix ${SOCKET}  (file permissions are the authorization)"
  echo "  status:   sudo curl -s --unix-socket ${SOCKET} ${API_URL}/v1/system | python3 -m json.tool"
fi
echo "  logs:     journalctl -u microhosted -f"
if [[ "$DEGRADED" -eq 1 ]]; then
  echo ""
  echo "  ATTENTION: the daemon is running but health is DEGRADED (detail above)."
  echo "    mh health                 # re-check at any time"
fi
if [[ "$MISSING" != "ok" ]]; then
  echo ""
  echo "  NEXT STEP — there's no template ready yet:"
  echo "    make prepare-image        # kernel + rootfs + preparation + catalog"
fi
echo ""
echo "  Create the first VM:"
echo "    mh vm create base-alpine"
