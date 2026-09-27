#!/usr/bin/env bash
# Network labels on the real host — docs/orchestrator.md §15, E1.
#
# Walks a throwaway network through what the orchestrator will rely on to own
# networks, against the running daemon:
#
#   1. create     labels given at create are returned and shown
#   2. select     ls -l keeps networks carrying the label, drops the others
#   3. patch      label KEY=VALUE sets, KEY- removes, the rest stays
#   4. refused    a malformed label or name is refused and changes nothing;
#                 a taken name and an unknown network are refused too
#   5. restart    (root only) labels survive a daemon restart
#   6. doctor     `mh doctor` clean
#
# Not disruptive: it only touches the network it creates (removed at the end).
# Needs the installed daemon with network labels, `mh` and jq.
#
#   scripts/netlabels-test.sh          # or with sudo for the restart step
#
# Environment:
#   MH        the CLI (default mh)
set -uo pipefail

MH="${MH:-mh}"
NET="nl-net"

passes=0
failures=0
pass() { echo "PASS  $*"; passes=$((passes + 1)); }
fail() { echo "FAIL  $*"; failures=$((failures + 1)); }
check() { # check LABEL COMMAND...: PASS if the command succeeds
  local label="$1"; shift
  if "$@" >/dev/null 2>&1; then pass "$label"; else fail "$label"; fi
}
refused() { # refused LABEL COMMAND...: PASS if the command fails
  local label="$1"; shift
  if "$@" >/dev/null 2>&1; then fail "$label"; else pass "$label"; fi
}
log() { printf '\n== %s\n' "$*"; }
labels() { "$MH" network inspect "$NET" 2>/dev/null | jq -cS '.labels // {}'; }
listed() { "$MH" network ls -q "$@" 2>/dev/null | grep -qx "$NET"; }

cleanup() { "$MH" network rm -f "$NET" >/dev/null 2>&1 || true; }
trap cleanup EXIT

command -v jq >/dev/null || { echo "needs jq" >&2; exit 1; }
cleanup

log "create"
if ! "$MH" network create "$NET" -l managed-by=nl-test -l fn=nl >/dev/null; then
  echo "cannot create the test network" >&2
  exit 1
fi
want='{"fn":"nl","managed-by":"nl-test"}'
if [[ "$(labels)" == "$want" ]]; then pass "labels given at create are returned"; else fail "labels after create: $(labels), want $want"; fi
if "$MH" network ls -L 2>/dev/null | grep -q "managed-by=nl-test"; then pass "ls -L shows them"; else fail "ls -L does not show the labels"; fi

log "select"
check "ls -l managed-by=nl-test lists it" listed -l managed-by=nl-test
check "two terms, both held, list it" listed -l managed-by=nl-test -l fn=nl
if listed -l managed-by=someone-else; then fail "ls -l with another value lists it"; else pass "ls -l with another value leaves it out"; fi
if "$MH" network ls -q -l managed-by=nl-test 2>/dev/null | grep -qx default; then fail "the default network matched a label it does not carry"; else pass "unlabelled networks are left out"; fi

log "patch"
check "label zone=north fn-" "$MH" network label "$NET" zone=north fn-
want='{"managed-by":"nl-test","zone":"north"}'
if [[ "$(labels)" == "$want" ]]; then pass "patch merged (set one, removed one, kept one)"; else fail "labels after patch: $(labels), want $want"; fi

log "refused"
refused "a malformed label value is refused" "$MH" network label "$NET" zone=has.bad-
refused "a malformed label key is refused" "$MH" network label "$NET" Zone=x
if [[ "$(labels)" == "$want" ]]; then pass "a refused patch changes nothing"; else fail "labels after a refused patch: $(labels)"; fi
refused "a name that is not a DNS label is refused" "$MH" network create Bad_Name
if "$MH" network ls -q 2>/dev/null | grep -qx Bad_Name; then fail "Bad_Name was created"; else pass "and nothing was created"; fi
err=$("$MH" network create "$NET" 2>&1 >/dev/null)
if [[ $? -ne 0 && "$err" == *"already exists"* ]]; then pass "a taken name is refused"; else fail "duplicate create: $err"; fi
err=$("$MH" network label nl-missing k=v 2>&1 >/dev/null)
if [[ $? -ne 0 && "$err" == *"not found"* ]]; then pass "labelling an unknown network is refused"; else fail "label unknown: $err"; fi

if [[ $EUID -eq 0 ]]; then
  log "daemon restart (root)"
  systemctl restart microhosted
  for _ in $(seq 1 100); do "$MH" network ls >/dev/null 2>&1 && break; sleep 0.2; done
  if [[ "$(labels)" == "$want" ]]; then pass "labels survive a daemon restart"; else fail "labels after restart: $(labels), want $want"; fi
fi

log "doctor"
cleanup
check "mh doctor clean" "$MH" doctor

log "result: $passes passed, $failures failed"
[[ $failures -eq 0 ]]
