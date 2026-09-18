#!/usr/bin/env bash
# Installs microhosted as a systemd service. Idempotent: it can be re-run to
# update the binary or the unit.
#
# It does NOT build: it uses the binary already built at build/microhosted (run
# `make build` as your user first, so as not to build as root). The
# `make install-service` target chains both steps for you.
#
# Usage:  sudo ./scripts/install-service.sh [ADDR]
#         With no argument the API serves on a Unix socket (default
#         /run/microhosted.sock), whose file permissions ARE its authorization —
#         root-only unless SOCKET_GROUP names a group. Pass an ADDR
#         (e.g. 127.0.0.1:8080) to serve on a TCP port instead: that API grants
#         root-equivalent control and has no authentication in front of it.
#
#         Env:  SOCKET=/run/microhosted.sock   socket path
#               SOCKET_GROUP=microhosted       group allowed to use it (0660)
#               MANAGED_IFACE=wlan0            interface(s) whose whole nftables
#                                              policy the daemon owns, comma-
#                                              separated: denied both ways
#                                              except each network's egress
#                                              and ingress rules that name
#                                              it. Remove any
#                                              OTHER ruleset covering it first —
#                                              two authors on one hook means a
#                                              drop you cannot see wins.
#               MANAGED_HOST_ALLOW=udp/67      host services still reachable
#                                              from those interfaces (default
#                                              udp/67 for DHCP; "none" denies
#                                              every one).
#
#         Re-running KEEPS what the installed unit already has: an empty
#         SOCKET_GROUP, MANAGED_IFACE or MANAGED_HOST_ALLOW means "unchanged",
#         not "remove". `make install-service` passes all three, empty when
#         unset, so a plain reinstall to ship a new binary must not silently
#         drop the socket's group (every operator locked out) or a managed
#         interface (its deny-both-ways policy gone while the API still reports
#         the rules that name it). To remove one on purpose:
#         SOCKET_GROUP=none, MANAGED_IFACE=none. (MANAGED_HOST_ALLOW=none keeps
#         its meaning: no host service at all.)

set -euo pipefail

ADDR="${1:-}"
SOCKET="${SOCKET:-/run/microhosted.sock}"
SOCKET_GROUP="${SOCKET_GROUP:-}"
MANAGED_IFACE="${MANAGED_IFACE:-}"
MANAGED_HOST_ALLOW="${MANAGED_HOST_ALLOW:-}"
UNIT_DST="/etc/systemd/system/microhosted.service"

# installed_flag NAME prints the value the installed unit gives --NAME and
# succeeds; fails when the flag is absent. The `--NAME=` form (an explicitly
# empty value) succeeds printing nothing.
installed_flag() {
  local line re_eq re_sp
  line="$(grep -m1 '^ExecStart=' "$UNIT_DST" 2>/dev/null || true)"
  re_eq="--$1=([^[:space:]]*)"
  re_sp="--$1[[:space:]]+([^-[:space:]][^[:space:]]*)"
  if [[ "$line" =~ $re_eq ]] || [[ "$line" =~ $re_sp ]]; then
    printf '%s' "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

INHERITED=()
if [[ -f "$UNIT_DST" ]]; then
  if [[ -z "$SOCKET_GROUP" ]] && old="$(installed_flag socket-group)" && [[ -n "$old" ]]; then
    SOCKET_GROUP="$old"
    INHERITED+=("SOCKET_GROUP=$old")
  fi
  if [[ -z "$MANAGED_IFACE" ]] && old="$(installed_flag managed-iface)" && [[ -n "$old" ]]; then
    MANAGED_IFACE="$old"
    INHERITED+=("MANAGED_IFACE=$old")
  fi
  if [[ -z "$MANAGED_HOST_ALLOW" ]] && old="$(installed_flag managed-host-allow)"; then
    MANAGED_HOST_ALLOW="${old:-none}"
    INHERITED+=("MANAGED_HOST_ALLOW=${old:-none}")
  fi
fi
if [[ "$SOCKET_GROUP" == "none" ]]; then SOCKET_GROUP=""; fi
if [[ "$MANAGED_IFACE" == "none" ]]; then MANAGED_IFACE=""; fi

if [[ -n "$ADDR" ]]; then
  LISTEN="--addr ${ADDR}"
else
  LISTEN="--socket ${SOCKET}"
  [[ -n "$SOCKET_GROUP" ]] && LISTEN="${LISTEN} --socket-group ${SOCKET_GROUP}"
fi

ARGS="$LISTEN"
if [[ -n "$MANAGED_IFACE" ]]; then
  ARGS="${ARGS} --managed-iface ${MANAGED_IFACE}"
  # "none" is how an operator says "no host service at all", which an empty
  # value cannot express (empty means "leave the daemon's default").
  if [[ "$MANAGED_HOST_ALLOW" == "none" ]]; then
    ARGS="${ARGS} --managed-host-allow="
  elif [[ -n "$MANAGED_HOST_ALLOW" ]]; then
    ARGS="${ARGS} --managed-host-allow ${MANAGED_HOST_ALLOW}"
  fi
fi
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_SRC="$REPO_ROOT/build/microhosted"
BIN_DST="/usr/local/bin/microhosted"
UNIT_SRC="$REPO_ROOT/deploy/microhosted.service.in"

if [[ $EUID -ne 0 ]]; then
  echo "ERROR: run with sudo or as root." >&2
  exit 1
fi

# Validate the group HERE rather than letting the daemon discover it at boot:
# it refuses to start on an unknown group (silently falling back to root-only
# would read as "the daemon is broken" and get "fixed" with chmod), so rendering
# a unit that names a nonexistent group just turns this into a crash loop.
if [[ -n "$SOCKET_GROUP" ]] && ! getent group "$SOCKET_GROUP" >/dev/null; then
  echo "ERROR: group '$SOCKET_GROUP' does not exist, and the daemon refuses to" >&2
  echo "       start rather than quietly leave the socket root-only. Create it:" >&2
  echo "         sudo groupadd -f $SOCKET_GROUP" >&2
  echo "         sudo usermod -aG $SOCKET_GROUP \$USER   # then log out and back in" >&2
  exit 1
fi

if [[ ! -x "$BIN_SRC" ]]; then
  echo "ERROR: $BIN_SRC doesn't exist — build it first with 'make build'." >&2
  exit 1
fi

echo "==> Installing the binary to $BIN_DST..."
install -o root -g root -m 0755 "$BIN_SRC" "$BIN_DST"

# The mh client talks to this daemon's socket; installing both together keeps
# the CLI in step with the API it speaks. Optional so an older build dir that
# predates cmd/mh still installs the daemon.
CLI_SRC="$REPO_ROOT/build/mh"
if [[ -x "$CLI_SRC" ]]; then
  echo "==> Installing the mh client to /usr/local/bin/mh..."
  install -o root -g root -m 0755 "$CLI_SRC" /usr/local/bin/mh
fi

if [[ ${#INHERITED[@]} -gt 0 ]]; then
  echo "==> Kept from the installed unit: ${INHERITED[*]}"
  echo "    (pass a new value to change one, or =none to remove it)"
fi
echo "==> Rendering the unit to $UNIT_DST (workdir=$REPO_ROOT, args=$ARGS)..."
sed -e "s#@BINARY@#${BIN_DST}#g" \
    -e "s#@WORKDIR@#${REPO_ROOT}#g" \
    -e "s#@ARGS@#${ARGS}#g" \
    "$UNIT_SRC" > "$UNIT_DST"

echo "==> systemctl daemon-reload..."
systemctl daemon-reload

if [[ -n "$MANAGED_IFACE" ]]; then
  echo ""
  echo "NOTE: this daemon now owns the whole policy on: ${MANAGED_IFACE}"
  echo "      Any OTHER nftables table covering it still applies, and a drop in"
  echo "      any table wins — so the policy the API reports would not be the one"
  echo "      in force. Check what else is there, and remove it:"
  echo "        sudo nft list ruleset | grep -n ${MANAGED_IFACE%%,*}"
fi

if [[ -n "$ADDR" ]]; then
  echo ""
  echo "WARNING: the API will serve on tcp ${ADDR} with NO authentication."
  echo "         Anyone who can reach it has root-equivalent control of this host."
elif [[ -z "$SOCKET_GROUP" ]]; then
  echo ""
  echo "NOTE: ${SOCKET} will be root-only. To let a group drive it without sudo:"
  echo "      sudo groupadd -f microhosted && sudo usermod -aG microhosted \$USER"
  echo "      sudo SOCKET_GROUP=microhosted ./scripts/install-service.sh"
fi

echo ""
echo "OK. Service installed. Next steps:"
echo "  sudo systemctl enable --now microhosted    # start now + at boot"
echo "  systemctl status microhosted"
echo "  journalctl -u microhosted -f               # follow logs"
echo "  mh health                                  # talk to it (mh --help)"
echo ""
echo "Restart without killing live VMs:"
echo "  sudo systemctl restart microhosted         # reconcile re-adopts them"
