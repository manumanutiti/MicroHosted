#!/usr/bin/env bash
# The store's free-space reserve on the real host — threat-model §7, audit M4.
#
# Restarts the daemon with a --disk-reserve-mb larger than the store (through
# a temporary systemd drop-in: the current ExecStart plus the flag), so the
# store is "under its reserve" without filling anything, then puts the normal
# unit back. Running VMs survive both restarts (KillMode=process).
#
#   1. refused   a create and a volume create are 503, naming the reserve
#   2. watch     host.disk_low is published, mh doctor reports disk_low,
#                mh health fails its disk_space check
#   3. running   a VM that was already running keeps running and answering
#   4. restore   original unit back: create works, mh doctor clean
#
# Needs root (systemctl), the installed daemon and `mh`.
#
#   sudo scripts/disk-reserve-test.sh
#
# Environment:
#   TEMPLATE  template for the test VMs (default base-alpine)
#   MH        the CLI (default mh)
set -uo pipefail

TEMPLATE="${TEMPLATE:-base-alpine}"
MH="${MH:-mh}"
UNIT=microhosted
DROPIN_DIR=/etc/systemd/system/${UNIT}.service.d
DROPIN=${DROPIN_DIR}/zz-disk-reserve-test.conf
HUGE=100000000 # 100 TB: more than any store has free

passes=0
failures=0
pass() { echo "PASS  $*"; passes=$((passes + 1)); }
fail() { echo "FAIL  $*"; failures=$((failures + 1)); }
check() {
  local label="$1"; shift
  if "$@" >/dev/null 2>&1; then pass "$label"; else fail "$label"; fi
}
refused() { # refused LABEL PATTERN COMMAND...
  local label="$1" pattern="$2"; shift 2
  local err
  if err=$("$@" 2>&1 >/dev/null); then
    fail "$label (accepted)"
  elif grep -qi -- "$pattern" <<<"$err"; then
    pass "$label"
  else
    fail "$label (refused, but: $err)"
  fi
}
log() { printf '\n== %s\n' "$*"; }
wait_ready() {
  for _ in $(seq 1 60); do
    "$MH" ps >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}
restart() {
  systemctl daemon-reload && systemctl reset-failed "$UNIT" 2>/dev/null
  systemctl restart "$UNIT" && wait_ready
}
agent_up() {
  for _ in $(seq 1 300); do
    "$MH" exec "$1" true >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  return 1
}

cleanup() {
  for v in $("$MH" ps -a -q -l drtest=1 2>/dev/null); do "$MH" rm "$v" >/dev/null 2>&1 || true; done
  "$MH" volume rm drtest-vol >/dev/null 2>&1 || true
  if [[ -f "$DROPIN" ]]; then
    rm -f "$DROPIN"
    restart || echo "WARNING: the daemon did not come back after removing $DROPIN" >&2
  fi
}
trap cleanup EXIT

[[ $EUID -eq 0 ]] || { echo "needs root (restarts $UNIT with a test reserve)" >&2; exit 1; }
[[ -e "$DROPIN" ]] && { echo "$DROPIN exists: a previous run did not clean up; remove it first" >&2; exit 1; }
EXEC=$(systemctl show "$UNIT" -p ExecStart --value | sed -n 's/.*argv\[\]=\([^;]*\) ;.*/\1/p' | head -1)
[[ -n "$EXEC" ]] || { echo "cannot read $UNIT's ExecStart" >&2; exit 1; }

log "setup: a VM running before the reserve is breached"
R=$("$MH" run "$TEMPLATE" -l drtest=1 2>/dev/null)
[[ -n "$R" ]] && agent_up "$R" || { echo "cannot start the pre-existing VM" >&2; exit 1; }

log "setup: restart with --disk-reserve-mb $HUGE"
mkdir -p "$DROPIN_DIR"
printf '[Service]\nExecStart=\nExecStart=%s --disk-reserve-mb %s\n' "$EXEC" "$HUGE" >"$DROPIN"
restart || { echo "the daemon did not start with the test reserve (journalctl -u $UNIT)" >&2; exit 1; }

log "1. refused"
refused "create under the reserve" "disk-reserve-mb" "$MH" run "$TEMPLATE" -l drtest=1
refused "volume create under the reserve" "disk-reserve-mb" "$MH" volume create drtest-vol --size 64

log "2. watch"
sleep 3 # the monitor ticks every 2 s
check "host.disk_low published" bash -c "'$MH' events --all --no-follow -t host.disk_low | grep -q disk_low"
check "mh doctor reports disk_low" bash -c "'$MH' doctor 2>&1 | grep -q disk_low"
check "mh health fails disk_space" bash -c "! '$MH' health >/dev/null 2>&1"

log "3. running VMs are left alone"
check "the pre-existing VM still answers" "$MH" exec "$R" true

log "4. restore"
rm -f "$DROPIN"
check "daemon back on its own unit" restart
N=$("$MH" run "$TEMPLATE" -l drtest=1 2>/dev/null)
check "create works again" test -n "$N"
for v in $("$MH" ps -a -q -l drtest=1 2>/dev/null); do "$MH" rm "$v" >/dev/null 2>&1; done
check "mh doctor is clean" "$MH" doctor

echo
echo "$passes passed, $failures failed"
[[ $failures -eq 0 ]]
