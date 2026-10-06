#!/usr/bin/env bash
# Full installation of MicroHosted on THIS machine, in a single step:
#
#   1. checks (architecture, KVM, Go)
#   2. host configuration (cgroups, nftables, btrfs CoW store)
#   3. firecracker + jailer (same version, architecture binaries)
#   4. building the daemon and its clients (microhosted, mh, mh-orchestrator)
#   5. systemd service (enable --now); the socket's group, microhosted, with
#      you in it on a first install
#   6. health check through the API
#
# Supports x86_64 and aarch64 (ARM64 boards with a 64-bit kernel, Jetson, ARM gateways).
# Normal entry point: `make full-install` (accepts ARCH=, FC_VERSION=, ADDR=).
#
# Idempotent: re-running it updates the binary/service and doesn't touch live VMs
# (KillMode=process + reconcile). NOTE: with FC_VERSION=latest it may upgrade
# Firecracker — snapshots are tied to the version that created them.
#
# Afterwards nothing else is needed to run a project: mh up builds its images
# (the pinned kernel included) the first time.
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

FC_VERSION="${FC_VERSION:-v1.16.1}"   # the pinned one; the Makefile passes the same
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

# go.mod asks for 1.25; any go from 1.21 on fetches that toolchain by itself
# (GOTOOLCHAIN=auto), so 1.21 is the floor here.
GO_WANT="$(awk '$1 == "go" { print $2; exit }' go.mod)"
if ! command -v go &>/dev/null; then
  echo "ERROR: Go is missing (the build needs ${GO_WANT}; any Go from 1.21 fetches it by itself)." >&2
  echo "  Install it: sudo snap install go --classic   or   https://go.dev/dl/" >&2
  exit 1
fi
GO_HAVE="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
if [[ "$(printf '%s\n' 1.21 "$GO_HAVE" | sort -V | head -1)" != 1.21 ]]; then
  echo "ERROR: Go ${GO_HAVE} is too old to fetch the ${GO_WANT} the build needs: install 1.21 or later." >&2
  echo "  sudo snap install go --classic   or   https://go.dev/dl/" >&2
  exit 1
fi
echo "  go ${GO_HAVE}: OK (builds with ${GO_WANT})"

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
echo "==> [4/6] Building microhosted, mh and mh-orchestrator..."
mkdir -p build
CGO_ENABLED=0 go build -o build/microhosted ./cmd/microhosted
CGO_ENABLED=0 go build -o build/mh ./cmd/mh
# mh up, down, plan… run it: without it the first project fails.
CGO_ENABLED=0 go build -o build/mh-orchestrator ./orchestrator/cmd/mh-orchestrator
echo "  build/microhosted, build/mh, build/mh-orchestrator: OK"

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

# Can this user drive the daemon yet? A group just joined applies from the
# next login; until then this terminal needs newgrp (or sudo mh).
SOCK_GROUP=""
if [[ -z "$ADDR" && -S "$SOCKET" ]]; then
  SOCK_GROUP="$(stat -c %G "$SOCKET")"
  [[ "$SOCK_GROUP" == root ]] && SOCK_GROUP=""
fi
in_group() { tr ' ' '\n' | grep -qx "$1"; }

echo ""
echo "=============================================="
echo " Installation complete (${ARCH})"
echo "=============================================="
if [[ -n "$ADDR" ]]; then
  echo "  API:      ${API_URL}  (tcp, UNAUTHENTICATED — root-equivalent)"
elif [[ -n "$SOCK_GROUP" ]]; then
  echo "  API:      unix ${SOCKET}  (group ${SOCK_GROUP}: its members use mh without sudo)"
else
  echo "  API:      unix ${SOCKET}  (root-only: sudo mh …)"
fi
echo "  logs:     journalctl -u microhosted -f"
if [[ "$DEGRADED" -eq 1 ]]; then
  echo ""
  echo "  ATTENTION: the daemon is running but health is DEGRADED (detail above)."
  echo "    mh health                 # re-check at any time"
fi
echo ""
echo "  Try it — an example project, built and run:"
if [[ -n "$SOCK_GROUP" ]] && ! id -nG | in_group "$SOCK_GROUP"; then
  if id -nG "$USER" | in_group "$SOCK_GROUP"; then
    echo "    newgrp ${SOCK_GROUP}            # once: your new group, in this terminal (or log in again)"
  else
    echo "    (you are not in ${SOCK_GROUP}: use sudo mh, or sudo usermod -aG ${SOCK_GROUP} \$USER)"
  fi
fi
echo "    cd orchestrator/examples/hello"
echo "    mh up                       # builds its image the first time (asks for sudo), then runs it"
echo "    mh status                   # in another terminal; mh down removes it all"
echo ""
echo "  More examples: orchestrator/examples/README.md — every command: docs/cli.md"
