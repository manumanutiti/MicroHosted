#!/usr/bin/env bash
# mh build's layers on the real host — what the unit tests cannot run: the
# overlays, the namespaces, apk/apt against the real mirrors, and the
# images booting.
#
#   1. first      the example builds and boots; its image holds nothing of
#                 the build (/.mh-cache, /.mh-build, apk's cache link, apt's
#                 build config) and resolves DNS through /proc/net/pnp
#   2. same       unchanged, nothing is built
#   3. one line   a changed site file: FROM, PREPARE, PACKAGES, AGENT and the
#                 other COPY are taken from the cache; the new image serves
#                 the new file
#   4. defaults   a new mem_mb: every layer from the cache
#   5. store      layers and caches are root's 0700 (the package cache's
#                 0755 inside them), no mount and no working directory left
#   6. ubuntu     (UBUNTU=1) the same on ubuntu:24.04 minbase: boots with
#                 systemd, the agent answers, a code change reuses debootstrap
#                 and apt
#
# Not disruptive: it builds under names of its own (removed at the end) and
# leaves the layer cache as the builds made it. Needs the installed daemon,
# mh and jq; checks in the guests go through mh exec. mh build asks for sudo
# when it builds, and step 5 to look at root's directories.
#
#   scripts/build-cache-test.sh
#   UBUNTU=1 scripts/build-cache-test.sh
#
# Environment:
#   MH      the CLI (default mh)
#   UBUNTU  1 to test the Ubuntu base too (needs debootstrap, ubuntu-keyring)
set -uo pipefail

MH="${MH:-mh}"
UBUNTU="${UBUNTU:-0}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

passes=0
failures=0
pass() { echo "PASS  $*"; passes=$((passes + 1)); }
fail() { echo "FAIL  $*"; failures=$((failures + 1)); }
check() { # check LABEL COMMAND...: PASS if the command succeeds
  local label=$1
  shift
  if "$@" >/dev/null 2>&1; then pass "$label"; else fail "$label"; fi
}
log() { echo; echo "== $*"; }

WORK="$(mktemp -d)"
VMS=()
IMAGES=()
cleanup() {
  for v in "${VMS[@]}"; do "$MH" rm "$v" >/dev/null 2>&1; done
  for i in "${IMAGES[@]}"; do "$MH" image rm "$i" >/dev/null 2>&1; done
  rm -rf "$WORK"
}
trap cleanup EXIT

# build DIR LOG: mh build, its progress in LOG; the reference in $OUT. Not
# called in $(…): what it adds to IMAGES must reach the cleanup.
build() {
  OUT=$("$MH" build "$1" 2>"$2") || { cat "$2" >&2; return 1; }
  IMAGES+=("${OUT%@*}")
}
# boot REF: a VM from the image, agent up; its ID in $OUT.
boot() {
  OUT=$("$MH" run "$1" --no-net --no-command --wait --wait-timeout 2m 2>/dev/null) || return 1
  VMS+=("$OUT")
}
gexec() { "$MH" exec "$1" sh -c "$2"; }
cached() { grep -c -- '---> Using cache' "$1"; }
ran() { grep -c -- '^ ---> [0-9a-f]\{12\}$' "$1"; }

# An mh without layered builds would pass or fail this for the wrong reasons.
if ! "$MH" builder prune --help >/dev/null 2>&1; then
  echo "$MH has no layered build (mh builder): make build && MH=build/mh $0, or install it" >&2
  exit 1
fi
STORE=$("$MH" system info --json | jq -r '.daemon.paths.store // empty')
[ -n "$STORE" ] || { echo "no store directory from mh system info --json" >&2; exit 1; }
echo "store: $STORE"

log "1. first build"
CTX="$WORK/nginx"
cp -a "$ROOT/orchestrator/examples/alpine-nginx" "$CTX"
sed -i 's/^name: .*/name: build-cache-test/' "$CTX/build.yml"
t0=$(date +%s)
if build "$CTX" "$WORK/1.log"; then
  REF=$OUT
  pass "built $REF in $(($(date +%s) - t0)) s ($(cached "$WORK/1.log") layers from the cache)"
else
  fail "the first build"
  exit 1
fi
if boot "$REF"; then
  V=$OUT
  pass "booted $V"
  check "nginx's configuration is valid in the VM" gexec "$V" 'nginx -t'
  check "no build scaffolding in the image" gexec "$V" '! ls -d /.mh-cache /.mh-build /etc/apk/cache 2>/dev/null | grep .'
  check "resolv.conf is the engine's" [ "$(gexec "$V" 'readlink /etc/resolv.conf')" = /proc/net/pnp ]
  check "/dev holds the kernel's devtmpfs, not build nodes" gexec "$V" 'mount | grep -q "on /dev type devtmpfs"'
else
  fail "booting $REF"
fi

log "2. unchanged"
t0=$(date +%s)
build "$CTX" "$WORK/2.log"
REF2=$OUT
check "the same reference" [ "$REF2" = "$REF" ]
check "nothing built ($(($(date +%s) - t0)) s)" grep -q "already built" "$WORK/2.log"

log "3. one line changed"
echo "<p>changed $(date +%s)</p>" >>"$CTX/site/index.html"
t0=$(date +%s)
if build "$CTX" "$WORK/3.log"; then
  REF3=$OUT
  secs=$(($(date +%s) - t0))
  check "a new reference" [ "$REF3" != "$REF" ]
  check "FROM PREPARE PACKAGES AGENT and the config's COPY from the cache ($(cached "$WORK/3.log"), in $secs s)" [ "$(cached "$WORK/3.log")" -eq 5 ]
  check "the changed COPY and the 2 run steps ran" [ "$(ran "$WORK/3.log")" -eq 3 ]
  check "apk did not run" bash -c "! grep -q 'fetch https://' '$WORK/3.log'"
  if boot "$REF3"; then
    V3=$OUT
    check "the new image has the new line" gexec "$V3" 'grep -q changed /srv/www/index.html'
  else
    fail "booting $REF3"
  fi
else
  fail "the rebuild"
fi

log "4. VM defaults only"
sed -i 's/^mem_mb: .*/mem_mb: 160/' "$CTX/build.yml"
if build "$CTX" "$WORK/4.log"; then
  check "every layer from the cache ($(cached "$WORK/4.log"))" [ "$(ran "$WORK/4.log")" -eq 0 ]
else
  fail "the build with a new mem_mb"
fi

log "5. the store"
echo "      (sudo to look at root's directories)"
check "layers are root's, 0700" [ "$(sudo stat -c '%U %a' "$STORE/build/layers")" = "root 700" ]
check "caches are root's, 0700" [ "$(sudo stat -c '%U %a' "$STORE/build/cache")" = "root 700" ]
check "every layer 0700" bash -c "[ -z \"\$(sudo find '$STORE/build/layers' -mindepth 1 -maxdepth 1 ! -perm 0700)\" ]"
check "no mount left under the build directory" bash -c "! grep -q ' $STORE/build' /proc/self/mounts"
check "no working directory left" bash -c "[ -z \"\$(sudo find '$STORE/build' -maxdepth 1 -name 'mh-build-*')\" ]"
# [m]: the pattern does not match pgrep's own command line.
check "no step process left" bash -c "! pgrep -f '[m]h-build-' >/dev/null"

if [ "$UBUNTU" = 1 ]; then
  log "6. ubuntu:24.04"
  UCTX="$WORK/ubuntu"
  cp -a "$ROOT/orchestrator/examples/app-ubuntu" "$UCTX"
  sed -i 's/^name: .*/name: build-cache-test-ubuntu/' "$UCTX/build.yml"
  t0=$(date +%s)
  if build "$UCTX" "$WORK/u1.log"; then
    UREF=$OUT
    pass "built $UREF in $(($(date +%s) - t0)) s"
    if boot "$UREF"; then
      UV=$OUT
      pass "booted $UV"
      # --wait: the agent answers before systemd has finished booting.
      check "systemd is up" gexec "$UV" 'state=$(timeout 60 systemctl is-system-running --wait); [ "$state" = running ]'
      check "no failed units" gexec "$UV" '[ -z "$(systemctl --failed --no-legend)" ]'
      check "python3 runs the app" gexec "$UV" 'python3 -c "import http.server"'
      check "no build config left in the image" gexec "$UV" '! ls /usr/sbin/policy-rc.d /etc/dpkg/dpkg.cfg.d/00mh-build /etc/apt/apt.conf.d/00mh-build /.mh-cache 2>/dev/null | grep .'
      check "no apt lists in the image" gexec "$UV" '[ -z "$(ls /var/lib/apt/lists | grep -v -e lock -e partial -e auxfiles)" ]'
      echo "      size: $(gexec "$UV" 'du -sxm / | cut -f1') MB, $(gexec "$UV" 'dpkg -l | grep -c ^ii') packages"
    else
      fail "booting $UREF"
    fi
    echo "# $(date +%s)" >>"$UCTX/app/server.py"
    t0=$(date +%s)
    if build "$UCTX" "$WORK/u2.log"; then
      check "a code change reuses debootstrap, the upgrade and apt ($(cached "$WORK/u2.log") from the cache, $(($(date +%s) - t0)) s)" [ "$(cached "$WORK/u2.log")" -eq 4 ]
    else
      fail "the Ubuntu rebuild"
    fi
  else
    fail "the Ubuntu build"
  fi
fi

echo
echo "$passes passed, $failures failed"
[ "$failures" -eq 0 ]
