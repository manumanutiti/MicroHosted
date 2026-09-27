#!/usr/bin/env bash
# Image store on the real host — docs/orchestrator.md §15, E2.
#
# Imports the template's golden into the content-addressed store and checks,
# against the running daemon, what the unit tests cannot: real clones from a
# read-only store file, the kernel hard-linked from the store into the jail,
# and that booting by image costs no more than booting by template.
#
#   1. import    the golden becomes NAME:VERSION@sha256:…, listed and ready
#   2. speed     N creates by template vs N by image: the image median must
#                not be slower (the store adds a lookup, no I/O)
#   3. boot      a VM by image records its digest, answers exec, survives
#                stop/start (kernel from the store)
#   4. refused   a pinned reference with another digest, an import from
#                outside the store dir, deleting an image in use
#   5. replace   a VM from an image is replaced from the same digest
#   6. verify    the stored files still hash to their digests
#   7. cleanup   image removed, `mh doctor` clean
#
# Not disruptive: it only touches the VMs and the image it creates (removed at
# the end). Needs the installed daemon with the image store, `mh` and jq.
#
#   scripts/image-test.sh
#
# Environment:
#   TEMPLATE  template whose golden is imported (default base-alpine)
#   N         creates per source in the speed round (default 5)
#   MH        the CLI (default mh)
set -uo pipefail

TEMPLATE="${TEMPLATE:-base-alpine}"
N="${N:-5}"
MH="${MH:-mh}"
TAG="img-test:1"

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
field() { "$MH" inspect "$1" 2>/dev/null | jq -r "$2"; }
now_ms() { date +%s%3N; }
median() { sort -n | awk '{a[NR]=$1} END {print (NR%2 ? a[(NR+1)/2] : int((a[NR/2]+a[NR/2+1])/2))}'; }
# A create returns once the VMM is up, before the guest's agent listens.
agent_up() {
  for _ in $(seq 1 300); do
    "$MH" exec "$1" true >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  return 1
}
test_vms() { "$MH" ps -a -q -l img-test=1 2>/dev/null; }

cleanup() {
  for v in $(test_vms); do "$MH" rm "$v" >/dev/null 2>&1 || true; done
  "$MH" image rm "$TAG" >/dev/null 2>&1 || true
}
trap cleanup EXIT

command -v jq >/dev/null || { echo "needs jq" >&2; exit 1; }
cleanup

log "import the golden of $TEMPLATE as $TAG"
tpl=$("$MH" images --json 2>/dev/null | jq -c --arg t "$TEMPLATE" '.[] | select(.name == $t)')
if [[ -z "$tpl" ]]; then echo "template $TEMPLATE not in the catalog" >&2; exit 1; fi
kernel=$(jq -r .kernel_path <<<"$tpl"); rootfs=$(jq -r .rootfs_path <<<"$tpl")
mem=$(jq -r .mem_mb <<<"$tpl"); disk=$(jq -r .disk_mb <<<"$tpl")
t0=$(now_ms)
ref=$("$MH" image import "$TAG" --kernel "$kernel" --rootfs "$rootfs" --mem "$mem" --disk "$disk" 2>&1)
t1=$(now_ms)
if [[ "$ref" == "$TAG@sha256:"* ]]; then pass "import printed the pinned reference ($((t1 - t0)) ms, paid once)"; else
  fail "import: $ref"; exit 1
fi
digest="${ref#*@}"
check "a second import of the same files is a no-op" "$MH" image import "$TAG" --kernel "$kernel" --rootfs "$rootfs" --mem "$mem" --disk "$disk"
if "$MH" image ls 2>/dev/null | grep "$TAG" | grep -q ready; then pass "listed and ready"; else fail "not listed as ready"; fi

log "speed: $N creates by template vs $N by image"
round() { # round SOURCE [COUNT]: prints one create time in ms per line
  for _ in $(seq 1 "${2:-$N}"); do
    local s e id
    s=$(now_ms)
    id=$("$MH" run "$1" --no-net -l img-test=1 2>/dev/null) || { echo "create from $1 failed" >&2; continue; }
    e=$(now_ms)
    echo $((e - s))
    "$MH" rm "$id" >/dev/null 2>&1
  done
}
# One warm-up each: the first create of a size builds its pre-grown copy.
round "$TEMPLATE" 1 >/dev/null
round "$TAG" 1 >/dev/null
t_tpl=$(round "$TEMPLATE" | median)
t_img=$(round "$TAG" | median)
echo "      median create: template ${t_tpl} ms, image ${t_img} ms"
# The noise of a single boot is tens of ms; the store adds none by design.
# median prints 0 for an empty round, so every create by image failing would
# read as "faster": the comparison only counts when both rounds booted.
if [[ -n "$t_tpl" && -n "$t_img" ]] && (( t_tpl > 0 && t_img > 0 && t_img <= t_tpl * 110 / 100 + 30 )); then
  pass "booting by image is not slower than by template"
else
  fail "booting by image is slower: ${t_img} ms vs ${t_tpl} ms"
fi

log "boot a VM by image"
VM=$("$MH" run "$ref" -l img-test=1 2>/dev/null)
if [[ -n "$VM" ]]; then pass "created from the pinned reference"; else fail "create by image"; exit 1; fi
if [[ "$(field "$VM" .image)" == "$digest" ]]; then pass "the VM records the image digest"; else fail "VM image: $(field "$VM" .image)"; fi
check "exec answers" agent_up "$VM"
check "stop" "$MH" stop "$VM"
check "start again (kernel from the store)" "$MH" start "$VM"
check "exec answers after the restart" agent_up "$VM"

log "refused"
refused "a tag pinned to another digest" "$MH" run "$TAG@sha256:$(printf '0%.0s' $(seq 1 64))" -l img-test=1
refused "an import from outside the store dir" "$MH" image import leak:1 --kernel /etc/hostname --rootfs /etc/hostname
refused "deleting an image a VM uses" "$MH" image rm "$TAG"

log "replace from the same digest"
NEW=$("$MH" replace "$VM" --old destroy 2>/dev/null)
if [[ -n "$NEW" && "$(field "$NEW" .image)" == "$digest" ]]; then pass "the replacement boots the same digest"; else fail "replacement: ${NEW:-none}, image $(field "${NEW:-x}" .image)"; fi

log "verify"
check "stored files still match their digests" "$MH" image verify "$TAG"

log "cleanup"
for v in $(test_vms); do "$MH" rm "$v" >/dev/null 2>&1; done
check "image removed once unused" "$MH" image rm "$TAG"
check "mh doctor clean" "$MH" doctor

log "result: $passes passed, $failures failed"
[[ $failures -eq 0 ]]
