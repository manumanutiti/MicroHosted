#!/usr/bin/env bash
# Downloads firecracker and jailer of the same version from GitHub Releases.
# With no arguments it installs the platform's PINNED version (the one validated
# on hardware; >= 1.12 is needed for network_overrides / simultaneous forks).
# "latest" installs the newest published release — a conscious decision: existing
# snapshots are tied to the version that created them.
# Usage: ./scripts/install-fc.sh [VERSION|latest] [INSTALL_DIR]
# Example: ./scripts/install-fc.sh v1.16.1 /usr/local/bin

set -euo pipefail

FC_VERSION="${1:-v1.16.1}"
INSTALL_DIR="${2:-/usr/local/bin}"

# Architecture: this machine's (the binaries run HERE; installing another
# architecture's only makes sense to copy them to another box).
ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
esac

if [[ "$FC_VERSION" == "latest" ]]; then
  # /releases/latest redirects to /releases/tag/vX.Y.Z — resolving the version by
  # following the redirect avoids depending on the GitHub API (and its limits).
  TAG_URL="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    https://github.com/firecracker-microvm/firecracker/releases/latest)"
  FC_VERSION="${TAG_URL##*/}"
  if [[ ! "$FC_VERSION" =~ ^v[0-9]+\.[0-9]+ ]]; then
    echo "ERROR: couldn't resolve the latest version (got: ${TAG_URL})"
    echo "       Pass an explicit version: $0 v1.16.1"
    exit 1
  fi
  echo "==> Latest release: ${FC_VERSION}"
fi

# Firecracker only supports x86_64 and aarch64
if [[ "$ARCH" != "x86_64" && "$ARCH" != "aarch64" ]]; then
  echo "ERROR: unsupported architecture: $ARCH"
  exit 1
fi

RELEASE_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}"
TARBALL="firecracker-${FC_VERSION}-${ARCH}.tgz"

echo "==> Downloading Firecracker ${FC_VERSION} for ${ARCH}..."
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

curl -fsSL "${RELEASE_URL}/${TARBALL}" -o "${TMP_DIR}/${TARBALL}"
tar -xzf "${TMP_DIR}/${TARBALL}" -C "$TMP_DIR"

# The tarball contains release-v*/firecracker-v*-x86_64 and release-v*/jailer-v*-x86_64
FC_BIN="$(find "$TMP_DIR" -name "firecracker-${FC_VERSION}-${ARCH}" | head -1)"
JAILER_BIN="$(find "$TMP_DIR" -name "jailer-${FC_VERSION}-${ARCH}" | head -1)"

if [[ -z "$FC_BIN" || -z "$JAILER_BIN" ]]; then
  echo "ERROR: the binaries weren't found in the tarball. Contents:"
  find "$TMP_DIR" -type f
  exit 1
fi

echo "==> Installing to ${INSTALL_DIR}..."
sudo install -o root -g root -m 0755 "$FC_BIN"     "${INSTALL_DIR}/firecracker"
sudo install -o root -g root -m 0755 "$JAILER_BIN" "${INSTALL_DIR}/jailer"

if [[ "$ARCH" == "$(uname -m)" ]]; then
  echo "==> Verifying versions:"
  firecracker --version
  jailer --version
else
  echo "==> WARNING: ${ARCH} binaries installed on a $(uname -m) machine — they"
  echo "    can't run here; copy them to the target machine."
fi

echo ""
echo "OK: firecracker and jailer installed in ${INSTALL_DIR}"
echo "    Both binaries are version ${FC_VERSION} — required for Jailer to work."
echo ""
echo "REMEMBER:"
echo "  - Restart the daemon (sudo systemctl restart microhosted) so it"
echo "    detects the new version (capabilities like network_overrides are"
echo "    probed at startup)."
echo "  - Existing snapshots are tied to the Firecracker version that created"
echo "    them: recreate them after upgrading."
