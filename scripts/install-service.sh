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
#         /run/microhosted.sock), whose file permissions ARE its authorization.
#         A first install gives it the group microhosted, created if missing,
#         and adds the user who ran sudo to it — as Docker's docker group:
#         membership is root-equivalent. SOCKET_GROUP=none keeps it root-only,
#         SOCKET_GROUP=NAME names an existing group instead. Pass an ADDR
#         (e.g. 127.0.0.1:8080) to serve on a TCP port instead: that API grants
#         root-equivalent control and has no authentication in front of it.
#
#         Env:  SOCKET=/run/microhosted.sock   socket path
#               SOCKET_GROUP=microhosted       group allowed to use it (0660);
#                                              the default on a first install
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
# A first install (no unit yet) with no SOCKET_GROUP gets the default group,
# created below, with the user who ran sudo in it. A reinstall keeps what the
# unit has, root-only included: access is never widened behind an operator.
DEFAULT_GROUP=0
if [[ ! -f "$UNIT_DST" && -z "$SOCKET_GROUP" && -z "$ADDR" ]]; then
  SOCKET_GROUP=microhosted
  DEFAULT_GROUP=1
fi
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
if [[ "$DEFAULT_GROUP" -eq 1 ]]; then
  if ! getent group "$SOCKET_GROUP" >/dev/null; then
    echo "==> Creating the group $SOCKET_GROUP (who may drive the daemon)..."
    groupadd "$SOCKET_GROUP"
  fi
  # The user who ran sudo, as Docker's post-install adds them to docker. Root
  # needs no group; a sudo from root's own shell has no one to add.
  if [[ -n "${SUDO_USER:-}" && "$SUDO_USER" != root ]] && ! id -nG "$SUDO_USER" | tr ' ' '\n' | grep -qx "$SOCKET_GROUP"; then
    echo "==> Adding $SUDO_USER to $SOCKET_GROUP: mh without sudo (root-equivalent, like the docker group)..."
    usermod -aG "$SOCKET_GROUP" "$SUDO_USER"
    ADDED_USER="$SUDO_USER"
  fi
fi
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

# The clients talk to this daemon's socket; installing them together keeps
# them in step with the API they speak: mh, and mh-orchestrator, which mh
# up/down/plan run (and which runs mh build). Optional so an older build dir
# that predates them still installs the daemon.
for client in mh mh-orchestrator; do
  if [[ -x "$REPO_ROOT/build/$client" ]]; then
    echo "==> Installing $client to /usr/local/bin/$client..."
    install -o root -g root -m 0755 "$REPO_ROOT/build/$client" "/usr/local/bin/$client"
  fi
done

# State lives under /var/lib/microhosted, owned by root: the database and the
# catalog name the paths this root daemon truncates, deletes and boots, so
# whoever can write them controls those operations — and the daemon refuses to
# start if anyone but root can. Older installs kept both in the checkout, owned
# by the user who cloned it; move them. 0711: the per-VM identities must still
# traverse it to reach the store, but no one lists it.
STATE_DIR="/var/lib/microhosted"
OLD_DB="$REPO_ROOT/images/microhosted.db"
NEW_DB="$STATE_DIR/microhosted.db"
SEED_CATALOG="$REPO_ROOT/images/catalog.json"
NEW_CATALOG="$STATE_DIR/catalog.json"
RESTART_AFTER=0

echo "==> State directory $STATE_DIR (root, 0711)..."
install -d -o root -g root -m 0711 "$STATE_DIR"
chown root:root "$STATE_DIR"
chmod 0711 "$STATE_DIR"

if [[ -f "$OLD_DB" ]]; then
  if [[ -e "$NEW_DB" ]]; then
    echo "  NOTE: $OLD_DB is ignored — the daemon uses $NEW_DB. Delete the old one once you no longer need it."
  else
    # Stop the daemon so the copy is consistent. Its VMs keep running
    # (KillMode=process) and are re-adopted when it starts again below.
    if systemctl is-active --quiet microhosted 2>/dev/null; then
      echo "  stopping the daemon to move its database (VMs keep running)..."
      systemctl stop microhosted
      RESTART_AFTER=1
    fi
    for suffix in "" -wal -shm -journal; do
      if [[ -f "${OLD_DB}${suffix}" ]]; then
        install -o root -g root -m 0600 "${OLD_DB}${suffix}" "${NEW_DB}${suffix}"
        mv "${OLD_DB}${suffix}" "${OLD_DB}${suffix}.migrated"
      fi
    done
    echo "  database moved to $NEW_DB (the old copy is kept as ${OLD_DB}.migrated; delete it once the daemon is confirmed healthy)"
  fi
fi

if [[ -e "$NEW_CATALOG" ]]; then
  chown root:root "$NEW_CATALOG"
  chmod 0644 "$NEW_CATALOG"
  if [[ -f "$SEED_CATALOG" ]] && ! cmp -s "$SEED_CATALOG" "$NEW_CATALOG"; then
    echo "  NOTE: $SEED_CATALOG differs from the catalog in use ($NEW_CATALOG); the repo copy is only the seed for new installs."
  fi
elif [[ -f "$SEED_CATALOG" ]]; then
  install -o root -g root -m 0644 "$SEED_CATALOG" "$NEW_CATALOG"
  echo "  catalog installed at $NEW_CATALOG (from $SEED_CATALOG)"
else
  echo "[]" > "$NEW_CATALOG"
  chown root:root "$NEW_CATALOG"
  chmod 0644 "$NEW_CATALOG"
  echo "  empty catalog created at $NEW_CATALOG (make prepare-image registers templates in it)"
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

if [[ "$RESTART_AFTER" -eq 1 ]]; then
  echo "==> Starting the daemon again (it re-adopts the running VMs)..."
  systemctl start microhosted
fi

if [[ -n "$MANAGED_IFACE" ]]; then
  echo ""
  echo "NOTE: this daemon now owns the whole policy on: ${MANAGED_IFACE}"
  echo "      Any OTHER nftables table covering it still applies, and a drop in"
  echo "      any table wins — so the policy the API reports would not be the one"
  echo "      in force. Check what else is there, and remove it:"
  echo "        sudo nft list ruleset | grep -n ${MANAGED_IFACE%%,*}"

  # Managing the interface the host routes out through is legitimate (that is the
  # point on a single-NIC gateway), but it rules out full egress entirely: a
  # network's --internet IFACE is refused for a managed interface, because a
  # blanket masquerade would punch straight through the deny-both-ways policy
  # that declaring it installs. Say so here rather than let an operator discover
  # it as a network that reports CLOSED and cannot be opened.
  DEFAULT_IFACE="$(ip -4 route show default 2>/dev/null | awk '{for(i=1;i<NF;i++)if($i=="dev"){print $(i+1);exit}}')"
  if [[ -n "$DEFAULT_IFACE" ]] && [[ ",${MANAGED_IFACE}," == *",${DEFAULT_IFACE},"* ]]; then
    echo ""
    echo "NOTE: ${DEFAULT_IFACE} is both managed and the host's default route, so NO network"
    echo "      can get full egress through it — 'mh network update NAME --internet ${DEFAULT_IFACE}'"
    echo "      is refused by design. Give guests what they need with per-rule holes:"
    echo "        mh network update NAME --out tcp:IP:PORT@${DEFAULT_IFACE}"
    echo "      Full egress needs a SEPARATE, unmanaged exit interface."
  fi
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
else
  echo ""
  echo "NOTE: members of ${SOCKET_GROUP} drive the daemon without sudo — root-equivalent"
  echo "      on this host, as the docker group is. SOCKET_GROUP=none makes it root-only."
  if [[ -n "${ADDED_USER:-}" ]]; then
    echo "      ${ADDED_USER} was added: it applies from the next login, or now in a"
    echo "      shell started with: newgrp ${SOCKET_GROUP}"
  fi
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
