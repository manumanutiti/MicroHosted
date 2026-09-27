#!/usr/bin/env bash
# Per-consumer quotas on the real host — docs/roadmap.md, Phase 1 item 3 (E5).
#
# Restarts the daemon with test quotas through a temporary systemd drop-in
# (the current ExecStart plus --quota flags), removed at the end with a second
# restart. Running VMs survive both restarts (KillMode=process).
#
#   1. vms cap     qt=vms:2: two launches pass, the third is refused (429)
#   2. others      a VM of another consumer, and one with no managed-by, pass
#   3. mem cap     qm=mem:200: one 128 MB VM passes, the second is refused
#   4. fixed label managed-by cannot be changed, removed or added (400)
#   5. quarantine  a quarantined VM still counts; destroying one frees room
#   6. report      mh info shows the quotas and their use
#   7. restore     original unit back, mh doctor clean
#
# Needs root (systemctl), the installed daemon, `mh` and jq.
#
#   sudo scripts/quota-test.sh
#
# Environment:
#   TEMPLATE  template for the test VMs (default base-alpine, 128 MB)
#   MH        the CLI (default mh)
set -uo pipefail

TEMPLATE="${TEMPLATE:-base-alpine}"
MH="${MH:-mh}"
UNIT=microhosted
DROPIN_DIR=/etc/systemd/system/${UNIT}.service.d
DROPIN=${DROPIN_DIR}/zz-quota-test.conf

passes=0
failures=0
pass() { echo "PASS  $*"; passes=$((passes + 1)); }
fail() { echo "FAIL  $*"; failures=$((failures + 1)); }
check() { # check LABEL COMMAND...: PASS if the command succeeds
  local label="$1"; shift
  if "$@" >/dev/null 2>&1; then pass "$label"; else fail "$label"; fi
}
refused() { # refused LABEL PATTERN COMMAND...: PASS if it fails with PATTERN on stderr
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
run() { "$MH" run "$TEMPLATE" "$@" 2>/dev/null; }

wait_ready() {
  for _ in $(seq 1 60); do
    "$MH" ps >/dev/null 2>&1 && return 0  # the API answering; health may be degraded for unrelated reasons
    sleep 0.5
  done
  return 1
}
restart() {
  systemctl daemon-reload && systemctl reset-failed "$UNIT" 2>/dev/null
  systemctl restart "$UNIT" && wait_ready
}

cleanup() {
  # The drop-in goes first and nothing may interrupt the cleanup: left in
  # place it keeps the daemon misconfigured after the test. A second Ctrl-C,
  # or a `| tee` that died with the first one (SIGPIPE on the next echo),
  # used to kill it before the rm.
  trap '' INT TERM PIPE
  if [[ -f "$DROPIN" ]]; then
    rm -f "$DROPIN"
    restart || echo "WARNING: the daemon did not come back after removing $DROPIN" >&2
  fi
  for v in $("$MH" ps -a -q -l qtest=1 2>/dev/null); do "$MH" rm "$v" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT

[[ $EUID -eq 0 ]] || { echo "needs root (restarts $UNIT with test quotas)" >&2; exit 1; }
command -v jq >/dev/null || { echo "needs jq" >&2; exit 1; }
[[ -e "$DROPIN" ]] && { echo "$DROPIN exists: a previous run did not clean up; remove it first" >&2; exit 1; }

# The running command line, from systemd: "... argv[]=/usr/local/bin/microhosted --socket ... ; ..."
EXEC=$(systemctl show "$UNIT" -p ExecStart --value | sed -n 's/.*argv\[\]=\([^;]*\) ;.*/\1/p' | head -1)
[[ -n "$EXEC" ]] || { echo "cannot read $UNIT's ExecStart" >&2; exit 1; }

log "setup: restart with --quota qt=vms:2 --quota qm=mem:200"
mkdir -p "$DROPIN_DIR"
printf '[Service]\nExecStart=\nExecStart=%s --quota qt=vms:2 --quota qm=mem:200\n' "$EXEC" >"$DROPIN"
if ! restart; then
  echo "the daemon did not start with the test quotas (journalctl -u $UNIT)" >&2
  exit 1
fi

log "1. vms cap"
Q1=$(run -l qtest=1 -l managed-by=qt); check "first qt VM" test -n "$Q1"
Q2=$(run -l qtest=1 -l managed-by=qt); check "second qt VM" test -n "$Q2"
refused "third qt VM refused by quota" "quota" "$MH" run "$TEMPLATE" -l qtest=1 -l managed-by=qt

log "2. other consumers"
O=$(run -l qtest=1 -l managed-by=other); check "a VM of another consumer" test -n "$O"
N=$(run -l qtest=1); check "a VM with no managed-by" test -n "$N"

log "3. mem cap"
M1=$(run -l qtest=1 -l managed-by=qm); check "first qm VM (128 MB of 200)" test -n "$M1"
refused "second qm VM refused by quota" "quota" "$MH" run "$TEMPLATE" -l qtest=1 -l managed-by=qm

log "4. managed-by is fixed"
refused "changing managed-by" "managed-by" "$MH" label "$Q1" managed-by=other
refused "removing managed-by" "managed-by" "$MH" label "$Q1" managed-by-
refused "adding managed-by" "managed-by" "$MH" label "$N" managed-by=qt
check "other labels still change" "$MH" label "$Q1" role=probe

log "5. quarantine and destroy"
check "quarantine a qt VM" "$MH" quarantine "$Q1"
refused "a quarantined VM still counts" "quota" "$MH" run "$TEMPLATE" -l qtest=1 -l managed-by=qt
"$MH" rm "$Q1" >/dev/null 2>&1
Q3=$(run -l qtest=1 -l managed-by=qt); check "destroying one frees room" test -n "$Q3"

log "6. report"
INFO=$("$MH" info 2>/dev/null)
check "mh info shows qt at 2 of 2" grep -q "Quota qt:.*2 of 2 VMs" <<<"$INFO"
check "mh info shows qm's memory cap" grep -q "Quota qm:" <<<"$INFO"
echo "$INFO" | grep "Quota" | sed 's/^/      /'

log "7. restore"
for v in $("$MH" ps -a -q -l qtest=1 2>/dev/null); do "$MH" rm "$v" >/dev/null 2>&1; done
rm -f "$DROPIN"
check "daemon back on its own unit" restart
check "mh doctor is clean" "$MH" doctor

echo
echo "$passes passed, $failures failed"
[[ $failures -eq 0 ]]
