#!/usr/bin/env bash
# Fault injection on the real host — docs/roadmap.md, Phase 0b item 6.
#
# For every failpoint compiled into the daemon (internal/faults), in both
# modes — "error" (the operation fails there) and "crash" (the daemon is
# SIGKILLed there) — it restarts the daemon with the fault armed, fires a
# burst of operations at it, restarts it clean, and requires `mh doctor` to
# report nothing: every failure must end in a working VM or in nothing at all.
# Then it kills the daemon with SIGKILL in the middle of a burst of creates,
# with no failpoint, and checks the same.
#
# DISRUPTIVE: restarts microhosted.service many times. Running VMs survive
# (KillMode=process + reconcile), but run it on a test host, not in the middle
# of something that matters. Needs root, the installed service and `mh`.
#
#   sudo scripts/fault-test.sh                 # everything
#   sudo POINTS="vm.create.after-boot" MODES=crash scripts/fault-test.sh
#
# Environment:
#   TEMPLATE  template for the test VMs (default base-alpine)
#   BURST     operations per round (default 4)
#   POINTS    space-separated failpoints (default: all of them)
#   MODES     "error crash" (default both)
#   MH        the CLI (default mh)
set -euo pipefail

TEMPLATE="${TEMPLATE:-base-alpine}"
BURST="${BURST:-4}"
MODES="${MODES:-error crash}"
MH="${MH:-mh}"
NET="faulttest"
UNIT="microhosted.service"
DROPIN_DIR="/run/systemd/system/${UNIT}.d"
DROPIN="${DROPIN_DIR}/faults.conf"
ALL_POINTS="vm.create.after-record vm.create.after-clone vm.create.after-network vm.create.after-boot vm.create.before-save vm.fork.after-clone vm.fork.after-boot network.create.after-bridge network.create.after-apply"
POINTS="${POINTS:-$ALL_POINTS}"

if [[ $EUID -ne 0 ]]; then
  echo "run as root: sudo $0" >&2
  exit 1
fi

failures=0
passes=0
# What the burst clients printed on stderr: an error-mode round must show the
# injected error, or the failpoint never fired and its PASS proves nothing.
ERRS="$(mktemp)"

log() { printf '\n== %s\n' "$*"; }

wait_up() {
  for _ in $(seq 1 100); do
    if "$MH" network ls >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  echo "daemon did not come back" >&2
  systemctl status "$UNIT" --no-pager | tail -20 >&2
  exit 1
}

# restart_with SPEC: restart the daemon with MICROHOSTED_FAULTS=SPEC, or with
# no failpoint at all when SPEC is empty. A runtime drop-in, so nothing
# survives a reboot even if this script is interrupted.
restart_with() {
  if [[ -n "$1" ]]; then
    mkdir -p "$DROPIN_DIR"
    printf '[Service]\nEnvironment=MICROHOSTED_FAULTS=%s\n' "$1" >"$DROPIN"
  else
    rm -f "$DROPIN"
  fi
  systemctl daemon-reload
  # The unit has no start limit any more (StartLimitIntervalSec=0), but its
  # restart delay grows with every automatic restart (RestartSteps) and only
  # reset-failed / a manual start zeroes it: without this, the crash rounds
  # would wait up to 60 s for systemd to bring the daemon back.
  systemctl reset-failed "$UNIT"
  systemctl restart "$UNIT"
  wait_up
}

# A crash-mode failpoint SIGKILLs the daemon; systemd restarts it (still
# armed). Wait for that before disarming, so the restart below is ours.
settle() {
  sleep 1
  wait_up
}

check_clean() {
  local label="$1"
  if out="$("$MH" doctor 2>&1)"; then
    echo "PASS  $label"
    passes=$((passes + 1))
  else
    echo "FAIL  $label"
    echo "$out" | sed 's/^/      /'
    failures=$((failures + 1))
  fi
}

teardown() {
  "$MH" network rm -f "$NET" >/dev/null 2>&1 || true
  for n in $("$MH" network ls 2>/dev/null | awk 'NR>1 && $1 ~ /^ft-/ {print $1}'); do
    "$MH" network rm -f "$n" >/dev/null 2>&1 || true
  done
  "$MH" snapshot ls 2>/dev/null | awk 'NR>1 && /faulttest/ {print $1}' | while read -r s; do
    "$MH" snapshot rm "$s" >/dev/null 2>&1 || true
  done
}

cleanup() {
  rm -f "$DROPIN" "$ERRS"
  systemctl daemon-reload
  systemctl reset-failed "$UNIT" || true
  systemctl restart "$UNIT" || true
  wait_up || true
  teardown
}
trap cleanup EXIT

# Whatever an operation did manage to create is legitimate — the test is about
# what failures leave behind — but it is removed before the next round. Only
# VMs that did not exist when the test started are touched.
remove_new_vms() {
  local id
  for id in $("$MH" ps -a -q); do
    if ! grep -qx "$id" <<<"$PREEXISTING"; then
      "$MH" rm "$id" >/dev/null 2>&1 || true
    fi
  done
}

burst_create() {
  for _ in $(seq 1 "$BURST"); do
    "$MH" run "$TEMPLATE" --net "$NET" >/dev/null 2>>"$ERRS" &
  done
  wait || true
}

burst_fork() {
  local snap="$1"
  for _ in $(seq 1 "$BURST"); do
    "$MH" snapshot fork "$snap" --quarantine >/dev/null 2>>"$ERRS" &
  done
  wait || true
}

burst_network() {
  for i in $(seq 1 "$BURST"); do
    "$MH" network create "ft-$i" >/dev/null 2>>"$ERRS" &
  done
  wait || true
}

log "baseline"
restart_with ""
teardown
if ! "$MH" doctor; then
  echo "the host is not clean BEFORE injecting anything; fix that first (restart the daemon, see mh doctor)" >&2
  exit 1
fi
PREEXISTING="$("$MH" ps -a -q)"
"$MH" network create "$NET" >/dev/null

# A snapshot to fork from, taken before any fault is armed.
base="$("$MH" run "$TEMPLATE" --net "$NET")"
snap="$("$MH" snapshot create "$base" --name faulttest)"
"$MH" rm "$base" >/dev/null

for point in $POINTS; do
  for mode in $MODES; do
    label="$point:$mode"
    log "$label"
    restart_with "$label"
    : >"$ERRS"
    case "$point" in
      vm.create.*) burst_create ;;
      vm.fork.*) burst_fork "$snap" ;;
      network.create.*) burst_network ;;
    esac
    if [[ "$mode" == crash ]]; then settle; fi
    restart_with ""
    if [[ "$mode" == error ]] && ! grep -q "failpoint $point: injected error" "$ERRS"; then
      echo "FAIL  $label: no operation returned the injected error (the failpoint never fired)"
      failures=$((failures + 1))
    fi
    check_clean "$label"
    remove_new_vms
    for n in $("$MH" network ls | awk 'NR>1 && $1 ~ /^ft-/ {print $1}'); do "$MH" network rm -f "$n" >/dev/null 2>&1 || true; done
  done
done

log "SIGKILL mid-burst, no failpoint"
restart_with ""
for delay in 0.3 0.8 1.5; do
  for _ in $(seq 1 "$BURST"); do
    "$MH" run "$TEMPLATE" --net "$NET" >/dev/null 2>&1 &
  done
  sleep "$delay"
  kill -9 "$(systemctl show -p MainPID --value "$UNIT")"
  wait || true
  settle
  restart_with ""
  check_clean "sigkill after ${delay}s"
  remove_new_vms
done

log "result: $passes passed, $failures failed"
[[ $failures -eq 0 ]]
