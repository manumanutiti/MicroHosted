#!/usr/bin/env bash
# Files written into a VM's disk before its first boot, on the real host —
# docs/orchestrator.md, engine prerequisite E4.
#
#   1. create     --file and --secret land with their content, mode and owner,
#                 read from inside the guest over exec
#   2. record     inspect lists them: sha256 for a file, none for a secret
#   3. refused    a setuid mode is a 400 and leaves no VM behind
#   4. replace    without files is a 400 and the old VM is untouched; with
#                 them the replacement is born configured; --no-files works
#   5. fork       a fork of a snapshot keeps the files and their record
#   6. latency    create time with and without files (median of 3)
#   7. doctor     `mh doctor` clean
#
# Not disruptive: it only touches the VMs it creates (label ft=1, removed at
# the end). Needs the installed daemon with E4, `mh`, jq and sha256sum.
#
#   scripts/files-test.sh
#
# Environment:
#   TEMPLATE  template for the test VMs (default base-alpine)
#   MH        the CLI (default mh)
set -uo pipefail

TEMPLATE="${TEMPLATE:-base-alpine}"
MH="${MH:-mh}"

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
test_vms() { "$MH" ps -a -q -l ft=1 2>/dev/null; }
# The guest agent answers a moment after create returns; wait for it (up to
# 30 s) before reading anything, or an early exec fails with no output.
agent_up() {
  local i
  for i in $(seq 1 300); do
    "$MH" exec "$1" true >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  return 1
}
gexec() { agent_up "$1" && "$MH" exec "$1" "$2" 2>/dev/null | tr -d '\r'; }

WORK=$(mktemp -d)
S=""
cleanup() {
  for v in $(test_vms); do "$MH" rm "$v" >/dev/null 2>&1 || true; done
  [[ -n "$S" ]] && "$MH" snapshot rm "$S" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

command -v jq >/dev/null || { echo "needs jq" >&2; exit 1; }
printf 'port=502\nunit=1\n' >"$WORK/parser.conf"
head -c 32 /dev/urandom | base64 >"$WORK/key"
CONF_SUM=$(sha256sum "$WORK/parser.conf" | cut -d' ' -f1)

log "1. create with a file and a secret"
A=$("$MH" run "$TEMPLATE" -l ft=1 \
  -f "/etc/ft/parser.conf=$WORK/parser.conf,mode=0640,uid=10,gid=20" \
  --secret "/etc/ft/key=$WORK/key" 2>/dev/null)
if [[ -z "$A" ]]; then
  fail "create with files"
  echo; echo "$passes passed, $failures failed"; exit 1
fi
pass "create with files ($A)"
check "the file's content is in the guest" eq "$(gexec "$A" 'cat /etc/ft/parser.conf')" "$(cat "$WORK/parser.conf")"
check "the file is 0640 owned 10:20" eq "$(gexec "$A" 'stat -c "%a %u %g" /etc/ft/parser.conf')" "640 10 20"
check "the secret's content is in the guest" eq "$(gexec "$A" 'cat /etc/ft/key')" "$(cat "$WORK/key")"
check "the secret is 0400 owned by root" eq "$(gexec "$A" 'stat -c "%a %u %g" /etc/ft/key')" "400 0 0"

log "2. the record"
check "inspect lists both files" eq "$(field "$A" '.files | length')" "2"
check "the file's sha256 matches" eq "$(field "$A" '.files[] | select(.path=="/etc/ft/parser.conf") | .sha256')" "$CONF_SUM"
check "the secret has no hash" eq "$(field "$A" '.files[] | select(.path=="/etc/ft/key") | .sha256 // "none"')" "none"
if "$MH" inspect "$A" 2>/dev/null | grep -qF "$(cat "$WORK/key")"; then fail "inspect leaks the secret"; else pass "inspect does not carry the secret"; fi

log "3. refused before anything is made"
before=$(test_vms | wc -l)
if "$MH" run "$TEMPLATE" -l ft=1 -f "/usr/bin/x=$WORK/parser.conf,mode=4755" >/dev/null 2>"$WORK/err"; then
  fail "a setuid mode was accepted"
else
  check "a setuid mode is refused as a bad request" grep -q "setuid" "$WORK/err"
fi
check "the refused create left no VM" eq "$(test_vms | wc -l)" "$before"

log "4. replace"
if "$MH" replace "$A" >/dev/null 2>"$WORK/err"; then
  fail "replace without files was accepted"
else
  check "replace without files names them" grep -q "/etc/ft/parser.conf" "$WORK/err"
fi
check "the old VM was not cut off" eq "$(field "$A" '.quarantine // false')" "false"
B=$("$MH" replace "$A" --old destroy -f "/etc/ft/parser.conf=$WORK/parser.conf" --secret "/etc/ft/key=$WORK/key" 2>/dev/null)
check "replace with files boots a replacement" test -n "$B"
check "the replacement is born configured" eq "$(gexec "$B" 'cat /etc/ft/parser.conf')" "$(cat "$WORK/parser.conf")"
C=$("$MH" replace "$B" --old destroy --no-files 2>/dev/null)
check "replace --no-files boots a replacement" test -n "$C"
check "the --no-files replacement has no file record" eq "$(field "$C" '.files | length')" "0"
check "and not the file either" eq "$(gexec "$C" 'test -e /etc/ft/parser.conf && echo yes || echo no')" "no"

log "5. snapshot and fork"
D=$("$MH" run "$TEMPLATE" -l ft=1 --no-net -f "/etc/ft/parser.conf=$WORK/parser.conf" 2>/dev/null)
S=$("$MH" snapshot create "$D" --name ft-snap 2>/dev/null | tail -1)
F=$("$MH" snapshot fork "$S" --quarantine -l ft=1 2>/dev/null)
if [[ -n "$F" ]]; then
  check "the fork has the file" eq "$(gexec "$F" 'cat /etc/ft/parser.conf')" "$(cat "$WORK/parser.conf")"
  check "the fork keeps the record" eq "$(field "$F" '.files[0].sha256')" "$CONF_SUM"
else
  fail "fork of the snapshot ($S)"
fi

log "6. create latency (median of 3)"
median() { sort -n | sed -n 2p; }
time_run() { # time_run ARGS...: ms for one mh run
  local t0 t1 v
  t0=$(date +%s%N)
  v=$("$MH" run "$TEMPLATE" -l ft=1 --no-net "$@" 2>/dev/null)
  t1=$(date +%s%N)
  "$MH" rm "$v" >/dev/null 2>&1
  echo $(((t1 - t0) / 1000000))
}
plain=$(for _ in 1 2 3; do time_run; done | median)
withf=$(for _ in 1 2 3; do time_run -f "/etc/ft/parser.conf=$WORK/parser.conf" --secret "/etc/ft/key=$WORK/key"; done | median)
echo "      create: ${plain} ms plain, ${withf} ms with 2 files (+$((withf - plain)) ms)"

log "7. doctor"
for v in $(test_vms); do "$MH" rm "$v" >/dev/null 2>&1; done
check "mh doctor is clean" "$MH" doctor

echo
echo "$passes passed, $failures failed"
[[ $failures -eq 0 ]]
