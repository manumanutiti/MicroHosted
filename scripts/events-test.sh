#!/usr/bin/env bash
# Events on the real host — docs/roadmap.md, Phase 1 (typed events).
#
# Follows `mh events` while it drives one VM through its lifecycle on a
# throwaway network, and checks that the stream says what happened, in order:
#
#   run → stop → start → quarantine → replace --old destroy → rm
#
# then that a subscriber can resume from an id and that a position from
# another daemon run gets a reset. As root it also kills a VM's Firecracker
# (vm.died with a reason) and restarts the daemon under the follower, which
# must survive it, receive a reset and keep following.
#
#   scripts/events-test.sh           # lifecycle, resume, reset
#   sudo scripts/events-test.sh      # + vm.died + daemon restart
#
# Environment: TEMPLATE (base-alpine), SUBNET (172.16.78.0/24), MH (mh).
set -uo pipefail

TEMPLATE="${TEMPLATE:-base-alpine}"
SUBNET="${SUBNET:-172.16.78.0/24}"
MH="${MH:-mh}"
NET="ev-net"
OUT="$(mktemp)"
FOLLOWER=""

passes=0
failures=0
pass() { echo "PASS  $*"; passes=$((passes + 1)); }
fail() { echo "FAIL  $*"; failures=$((failures + 1)); }
log() { printf '\n== %s\n' "$*"; }
# seen: the "type:vm" of every event the follower printed, one per line.
seen() { jq -r '.type + ":" + .vm' "$OUT"; }
# wait_for PATTERN: up to 10 s for a line of seen() to match.
wait_for() {
  for _ in $(seq 1 50); do
    seen | grep -qx "$1" && return 0
    sleep 0.2
  done
  return 1
}

cleanup() {
  [[ -n "$FOLLOWER" ]] && kill "$FOLLOWER" 2>/dev/null
  for v in $("$MH" ps -a -q -l fn=ev 2>/dev/null); do "$MH" rm "$v" >/dev/null 2>&1 || true; done
  "$MH" network rm -f "$NET" >/dev/null 2>&1 || true
  rm -f "$OUT"
}
trap cleanup EXIT

command -v jq >/dev/null || { echo "needs jq" >&2; exit 1; }
cleanup
OUT="$(mktemp)"
"$MH" network create "$NET" --subnet "$SUBNET" >/dev/null || { echo "cannot create $NET" >&2; exit 1; }

log "follower: mh events --json -l fn=ev"
"$MH" events --json -l fn=ev >"$OUT" 2>/dev/null &
FOLLOWER=$!
sleep 1

log "lifecycle"
A=$("$MH" run "$TEMPLATE" --net "$NET" --name ev-a -l fn=ev 2>/dev/null)
"$MH" stop "$A" >/dev/null
"$MH" start "$A" >/dev/null
"$MH" quarantine "$A" >/dev/null
B=$("$MH" replace "$A" --old destroy --name ev-b 2>/dev/null)
wait_for "vm.destroyed:$A" || true

want=$(printf '%s\n' \
  "vm.created:$A" "vm.stopped:$A" "vm.started:$A" "vm.quarantined:$A" \
  "vm.stopped:$A" "vm.created:$B" "vm.replaced:$A" "vm.destroyed:$A")
got=$(seen | grep -v '^network\.')
if [[ "$got" == "$want" ]]; then pass "lifecycle events, in order"; else
  fail "lifecycle events"; diff <(echo "$want") <(echo "$got") | sed 's/^/      /'
fi
if jq -e --arg a "$A" --arg b "$B" 'select(.type=="vm.replaced" and .vm==$a) | .data.replacement==$b and .data.old=="destroy" and .network=="ev-net"' "$OUT" >/dev/null; then
  pass "vm.replaced names the replacement and the function's network"
else fail "vm.replaced data"; fi
if jq -e --arg a "$A" 'select(.type=="vm.quarantined" and .vm==$a) | .network=="ev-net" and .labels.lease=="quarantined"' "$OUT" >/dev/null; then
  pass "vm.quarantined carries the network it left and lease=quarantined"
else fail "vm.quarantined fields"; fi

if [[ $EUID -eq 0 ]]; then
  log "vm.died (root)"
  pid=$("$MH" inspect "$B" | jq -r .pid)
  kill -9 "$pid"
  if wait_for "vm.died:$B"; then
    reason=$(jq -r --arg b "$B" 'select(.type=="vm.died" and .vm==$b) | .reason' "$OUT")
    if [[ -n "$reason" ]]; then pass "vm.died within seconds ($reason)"; else fail "vm.died has no reason"; fi
  else fail "no vm.died after SIGKILL of $B's firecracker"; fi
fi

"$MH" rm "$B" >/dev/null
if wait_for "vm.destroyed:$B"; then pass "rm → vm.destroyed"; else fail "no vm.destroyed for $B"; fi

log "resume and reset"
third=$(jq -r '"\(.epoch):\(.seq)"' "$OUT" | sed -n 3p)
resumed=$("$MH" events --since "$third" --no-follow --json -l fn=ev | jq -r '.type + ":" + .vm' | grep -v '^network\.')
expected=$(seen | grep -v '^network\.' | tail -n +4)
if [[ "$resumed" == "$expected" ]]; then pass "--since resumes right after the given event"; else
  fail "--since $third"; diff <(echo "$expected") <(echo "$resumed") | sed 's/^/      /'
fi
first=$("$MH" events --since 0badc0de:1 --no-follow --json | head -1 | jq -r .type)
if [[ "$first" == "reset" ]]; then pass "a position from another daemon run gets a reset"; else fail "stale position: first event $first, want reset"; fi

if [[ $EUID -eq 0 ]]; then
  log "daemon restart under the follower (root)"
  systemctl restart microhosted
  for _ in $(seq 1 100); do "$MH" network ls >/dev/null 2>&1 && break; sleep 0.2; done
  if wait_for "reset:"; then pass "follower got a reset after the restart"; else fail "no reset after the restart"; fi
  C=$("$MH" run "$TEMPLATE" --net "$NET" -l fn=ev 2>/dev/null)
  if wait_for "vm.created:$C"; then pass "follower keeps following after the restart"; else fail "follower stopped after the restart"; fi
  if kill -0 "$FOLLOWER" 2>/dev/null; then pass "follower process still alive"; else fail "follower exited"; fi
fi

log "result: $passes passed, $failures failed"
[[ $failures -eq 0 ]]
