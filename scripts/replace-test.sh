#!/usr/bin/env bash
# Replace on the real host — docs/roadmap.md, Phase 1 (lease).
#
# Builds a throwaway network with one ingress rule (so its to_ip is pinned),
# puts a VM on the function's address, and walks it through every replace
# path against the running daemon, checking what the unit tests cannot: real
# TAPs, real IPAM, real boots.
#
#   1. pinning     a plain `mh run` never gets the ingress to_ip
#   2. quarantine  replace (default): new VM at the same IP, same labels,
#                  answers ping; the old one alive, cut off, exec still works
#   3. no flood    replacing the old VM again is a 409 and boots nothing
#   4. stop        --old stop: old powered off and quarantined, new one serves
#   5. bad source  replace from a missing template: 400, the serving VM untouched
#   6. snapshot    --snapshot S --old destroy: fork at the function's address,
#                  old VM gone
#   7. doctor      `mh doctor` clean, and exactly the expected VMs exist
#
# Not disruptive: it only touches the network and VMs it creates (removed at
# the end). Needs the installed daemon with replace, `mh` and jq. Root is not
# needed if your user can reach the daemon.
#
#   scripts/replace-test.sh
#
# Environment:
#   TEMPLATE  template for the test VMs (default base-alpine)
#   SUBNET    test network subnet (default 172.16.77.0/24; must be free)
#   IFACE     a managed interface for the ingress rule (default wlan0)
#   MH        the CLI (default mh)
set -uo pipefail

TEMPLATE="${TEMPLATE:-base-alpine}"
SUBNET="${SUBNET:-172.16.77.0/24}"
IFACE="${IFACE:-wlan0}"
MH="${MH:-mh}"
NET="rt-net"
PREFIX="${SUBNET%.*}"         # 172.16.77
FN_IP="${PREFIX}.2"           # the function's address = the ingress to_ip
# 192.0.2.1 is TEST-NET-1: the rule is real but nothing will ever match it.
RULE="tcp:192.0.2.1:1883@${IFACE}=${FN_IP}"

passes=0
failures=0
pass() { echo "PASS  $*"; passes=$((passes + 1)); }
fail() { echo "FAIL  $*"; failures=$((failures + 1)); }
check() { # check LABEL COMMAND...: PASS if the command succeeds
  local label="$1"; shift
  if "$@" >/dev/null 2>&1; then pass "$label"; else fail "$label"; fi
}
log() { printf '\n== %s\n' "$*"; }
field() { "$MH" inspect "$1" 2>/dev/null | jq -r "$2"; }
eq() { [[ "$1" == "$2" ]]; }
fn_vms() { "$MH" ps -a -q -l fn=rt 2>/dev/null | sort; }

cleanup() {
  for v in $(fn_vms) $(tmp_vms); do "$MH" rm "$v" >/dev/null 2>&1 || true; done
  "$MH" network rm -f "$NET" >/dev/null 2>&1 || true
}
tmp_vms() { "$MH" ps -a -q -l rt-tmp=1 2>/dev/null; }
trap cleanup EXIT

command -v jq >/dev/null || { echo "needs jq" >&2; exit 1; }
cleanup

log "setup: network $NET ($SUBNET) with ingress $RULE"
if ! "$MH" network create "$NET" --subnet "$SUBNET" --in "$RULE" >/dev/null; then
  echo "cannot create the test network (is $IFACE managed? set IFACE=...)" >&2
  exit 1
fi

log "1. pinning"
TMP=$("$MH" run "$TEMPLATE" --net "$NET" -l rt-tmp=1 2>/dev/null)
TMP_IP=$(field "$TMP" .guest_ip)
if [[ -n "$TMP" && "$TMP_IP" != "$FN_IP" ]]; then pass "plain run skips the pinned $FN_IP (got $TMP_IP)"; else fail "plain run got $TMP_IP, the pinned address"; fi
"$MH" rm "$TMP" >/dev/null 2>&1

A=$("$MH" run "$TEMPLATE" --net "$NET" --ip "$FN_IP" --name rt-a -l fn=rt 2>/dev/null)
check "run --ip takes the pinned address" eq "$(field "$A" .guest_ip)" "$FN_IP"

log "2. replace, old quarantined"
B=$("$MH" replace "$A" --name rt-b 2>/dev/null)
if [[ -z "$B" ]]; then fail "replace $A"; exit 1; fi
check "replacement is at the function's IP"      eq "$(field "$B" .guest_ip)" "$FN_IP"
check "replacement is on the function's network" eq "$(field "$B" .network)" "$NET"
check "replacement inherits fn=rt"               eq "$(field "$B" .labels.fn)" "rt"
check "replacement records replaces=$A"          eq "$(field "$B" .replaces)" "$A"
check "replacement answers on $FN_IP"            ping -c 2 -W 2 "$FN_IP"
check "replacement's guest holds $FN_IP"         "$MH" exec "$B" -- sh -c "ip -4 addr | grep -q '$FN_IP/'"
check "old VM still running"                     eq "$(field "$A" .state)" "running"
check "old VM quarantined"                       eq "$(field "$A" .quarantine)" "true"
check "old VM labelled lease=quarantined"        eq "$(field "$A" .labels.lease)" "quarantined"
check "old VM remembers its network"             eq "$(field "$A" .quarantined_from)" "$NET"
check "old VM reachable over vsock"              "$MH" exec "$A" -- true
check "old VM cannot reach its gateway"          bash -c "! '$MH' exec '$A' -- ping -c 1 -W 2 ${PREFIX}.1"

log "3. no flood"
before=$(fn_vms | wc -l)
if "$MH" replace "$A" >/dev/null 2>&1; then fail "replacing $A again was accepted"; else pass "replacing $A again is refused"; fi
check "the refused replace booted nothing" eq "$(fn_vms | wc -l)" "$before"

log "4. replace, old stopped"
C=$("$MH" replace "$B" --old stop --name rt-c 2>/dev/null)
if [[ -z "$C" ]]; then fail "replace $B --old stop"; exit 1; fi
check "old VM stopped"            eq "$(field "$B" .state)" "stopped"
check "old VM quarantined"        eq "$(field "$B" .quarantine)" "true"
check "replacement at $FN_IP"     eq "$(field "$C" .guest_ip)" "$FN_IP"
check "replacement answers"       ping -c 2 -W 2 "$FN_IP"

log "5. bad source"
if "$MH" replace "$C" --template no-such-template >/dev/null 2>&1; then fail "replace from a missing template was accepted"; else pass "replace from a missing template is refused"; fi
check "serving VM left on its network" eq "$(field "$C" .network)" "$NET"
check "serving VM not quarantined"     eq "$(field "$C" '.quarantine // false')" "false"
check "serving VM still answers"       ping -c 2 -W 2 "$FN_IP"

log "6. replace from a snapshot, old destroyed"
SNAP=$("$MH" vm snapshot "$C" --name rt-clean 2>/dev/null)
D=$("$MH" replace "$C" --snapshot "$SNAP" --old destroy --name rt-d 2>/dev/null)
if [[ -z "$D" ]]; then fail "replace $C --snapshot --old destroy"; else
  check "old VM destroyed"          bash -c "! '$MH' inspect '$C'"
  check "fork at $FN_IP"            eq "$(field "$D" .guest_ip)" "$FN_IP"
  check "fork answers"              ping -c 2 -W 2 "$FN_IP"
  check "fork records replaces=$C"  eq "$(field "$D" .replaces)" "$C"
fi
"$MH" snapshot rm "$SNAP" >/dev/null 2>&1

log "7. what is left"
expected=$(printf '%s\n' "$A" "$B" "$D" | sort)
check "exactly $A (quarantined), $B (stopped), $D (serving)" eq "$(fn_vms)" "$expected"
serving=$("$MH" ps -a -l fn=rt --json | jq -r --arg ip "$FN_IP" '[.[] | select(.quarantine != true and .guest_ip == $ip)] | length')
check "one VM serves the function" eq "$serving" "1"
check "mh doctor clean" "$MH" doctor

log "result: $passes passed, $failures failed"
[[ $failures -eq 0 ]]
