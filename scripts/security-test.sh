#!/usr/bin/env bash
# Security invariants, checked on the live host — docs/threat-model.md.
#
# Every guarantee the threat model states is asserted here against the running
# system, not against the code: the seccomp filters were documented as active
# for months while the SDK was passing --no-seccomp. A property that is not in
# this script is a property nobody has checked.
#
#   host   the daemon (root, oom_score_adj -900, API socket 0660, no TCP
#          listener), the store and database modes, no guest image ever
#          mounted or loop-attached, no AF_VSOCK listener, nftables tables
#   vm     for EVERY running VM (not only the test ones): its own uid in the
#          reserved range, no groups, no capabilities, no_new_privs, seccomp
#          on every thread, cgroup limits exactly as configured, oom_score_adj,
#          TAP and disk owned by it; as root, the chroot holds nothing
#          unexpected and no open file descriptor points outside it
#   guest  from inside test VMs: the host (gateway, LAN addresses, ssh), VMs
#          of the same network without intra and of other networks, and the
#          internet without egress are unreachable; IPv6 is dead; a spoofed
#          IP or MAC is dropped. Each has a positive control, so a broken
#          tool cannot pass for a blocked path.
#
# Not disruptive: creates its own networks and VMs (label sectest=1), removed
# at the end. Checks that need root are SKIPPED without it — run with sudo for
# the full suite.
#
#   sudo scripts/security-test.sh
#
# Environment:
#   TEMPLATE    template for the test VMs (default base-alpine)
#   MH          the CLI (default mh)
#   UNIT        systemd unit of the daemon (default microhosted)
#   STATE_DIR   daemon state dir (default /var/lib/microhosted)
#   STORE       instances dir (default $STATE_DIR/store)
#   SOCKET      API socket (default /run/microhosted.sock)
#   ID_BASE     start of the reserved uid range (default 1900000000)
#   ID_COUNT    size of the range (default 65536)
set -uo pipefail

TEMPLATE="${TEMPLATE:-base-alpine}"
MH="${MH:-mh}"
UNIT="${UNIT:-microhosted}"
STATE_DIR="${STATE_DIR:-/var/lib/microhosted}"
STORE="${STORE:-$STATE_DIR/store}"
SOCKET="${SOCKET:-/run/microhosted.sock}"
ID_BASE="${ID_BASE:-1900000000}"
ID_COUNT="${ID_COUNT:-65536}"
# From internal/jailer/cgroup.go.
CPU_PERIOD=100000
MEM_OVERHEAD_MIB=64
PIDS_HEADROOM=16

passes=0
failures=0
skips=0
pass() { echo "PASS  $*"; passes=$((passes + 1)); }
fail() { echo "FAIL  $*"; failures=$((failures + 1)); }
skip() { echo "SKIP  $*"; skips=$((skips + 1)); }
expect() { # expect LABEL GOT WANT
  if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi
}
log() { printf '\n== %s\n' "$*"; }
root() { [[ $EUID -eq 0 ]]; }
status_field() { awk -v k="$2:" '$1 == k { $1 = ""; sub(/^ +/, ""); print; exit }' "/proc/$1/status" 2>/dev/null; }

cleanup() {
  for v in $("$MH" ps -a -q -l sectest=1 2>/dev/null); do "$MH" rm "$v" >/dev/null 2>&1 || true; done
  for n in sect-a sect-b sect-i; do "$MH" network rm -f "$n" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT
command -v jq >/dev/null || { echo "needs jq" >&2; exit 1; }
root || echo "(not root: the checks that need it are skipped — run with sudo for the full suite)"

# ---------------------------------------------------------------- host
log "host"
DPID=$(systemctl show "$UNIT" -p MainPID --value 2>/dev/null)
if [[ -n "$DPID" && "$DPID" != 0 ]]; then
  expect "daemon runs as root" "$(status_field "$DPID" Uid | awk '{print $2}')" "0"
  expect "daemon oom_score_adj" "$(cat /proc/"$DPID"/oom_score_adj)" "-900"
else
  fail "daemon pid not found (unit $UNIT)"
fi
expect "API socket mode and owner" "$(stat -c '%a %U' "$SOCKET")" "660 root"
expect "state dir 0711 root" "$(stat -c '%a %U' "$STATE_DIR")" "711 root"
expect "store 0711 root" "$(stat -c '%a %U' "$STORE")" "711 root"
expect "database 0600 root" "$(stat -c '%a %U' "$STATE_DIR/microhosted.db")" "600 root"
cat_mode=$(stat -c '%a %U' "$STATE_DIR/catalog.json")
[[ "$cat_mode" =~ ^6[04][04]\ root$ ]] && pass "catalog root-owned, not group/world-writable ($cat_mode)" || fail "catalog: $cat_mode"

mounted=$(awk -v s="$STORE/" 'index($1, s) == 1 || index($2, s) == 1' /proc/mounts)
[[ -z "$mounted" ]] && pass "no mount from or into the store" || fail "mounts under the store: $mounted"
loops=$(losetup -l -n -O BACK-FILE 2>/dev/null | awk -v s="$STORE/" 'index($1, s) == 1')
[[ -z "$loops" ]] && pass "no store file attached as a loop device" || fail "loop devices on store files: $loops"

vsock_listeners=$(ss -l --vsock -H 2>/dev/null | wc -l)
expect "no AF_VSOCK listener on the host" "$vsock_listeners" "0"

if root; then
  tcp=$(ss -ltnpH 2>/dev/null | grep -c '"microhosted"')
  expect "daemon has no TCP listener" "$tcp" "0"
  tables=$(nft list tables 2>/dev/null)
  grep -q "inet microhosted" <<<"$tables" && pass "nft table inet microhosted present" || fail "nft table inet microhosted missing"
  grep -q "bridge microhosted" <<<"$tables" && pass "nft table bridge microhosted present" || fail "nft table bridge microhosted missing"
else
  skip "daemon TCP listeners, nftables tables (need root)"
fi

# ---------------------------------------------------------------- test VMs
log "setup: test networks and VMs"
"$MH" network create sect-a -l sectest=1 >/dev/null || { echo "cannot create sect-a" >&2; exit 1; }
"$MH" network create sect-b -l sectest=1 >/dev/null || { echo "cannot create sect-b" >&2; exit 1; }
"$MH" network create sect-i -l sectest=1 --intra >/dev/null || { echo "cannot create sect-i" >&2; exit 1; }
mk() { "$MH" run "$TEMPLATE" -l sectest=1 "$@" 2>/dev/null; }
A1=$(mk --net sect-a); A2=$(mk --net sect-a); B1=$(mk --net sect-b)
I1=$(mk --net sect-i); I2=$(mk --net sect-i)
for v in "$A1" "$A2" "$B1" "$I1" "$I2"; do
  [[ -n "$v" ]] || { echo "a test VM did not start" >&2; exit 1; }
done
agent_up() {
  for _ in $(seq 1 300); do
    "$MH" exec "$1" true >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  return 1
}
for v in "$A1" "$A2" "$B1" "$I1" "$I2"; do agent_up "$v" || { echo "agent of $v silent" >&2; exit 1; }; done
echo "      sect-a: $A1 $A2 · sect-b: $B1 · sect-i (intra): $I1 $I2"

# ---------------------------------------------------------------- every VM
log "every running VM"
declare -A seen_uid
for id in $("$MH" ps -q 2>/dev/null); do
  j=$("$MH" inspect "$id" 2>/dev/null) || continue
  pid=$(jq -r '.pid // 0' <<<"$j")
  [[ "$pid" -gt 0 && -d /proc/$pid ]] || { fail "$id: no live process (pid $pid)"; continue; }
  vcpus=$(jq -r .vcpus <<<"$j"); mem=$(jq -r .mem_mb <<<"$j")
  tap=$(jq -r '.tap_device // empty' <<<"$j"); net=$(jq -r '.network // empty' <<<"$j")
  disk=$(jq -r .rootfs_path <<<"$j")
  bad=()

  uid=$(status_field "$pid" Uid | awk '{print $1}')
  read -r ruid euid suid fsuid <<<"$(status_field "$pid" Uid)"
  read -r rgid egid sgid fsgid <<<"$(status_field "$pid" Gid)"
  if [[ "$ruid$euid$suid$fsuid" != "$uid$uid$uid$uid" || "$rgid$egid$sgid$fsgid" != "$uid$uid$uid$uid" ]]; then
    bad+=("uids/gids not all $uid")
  fi
  ((uid >= ID_BASE && uid < ID_BASE + ID_COUNT)) || bad+=("uid $uid outside the reserved range")
  [[ -n "${seen_uid[$uid]:-}" ]] && bad+=("uid $uid shared with ${seen_uid[$uid]}")
  seen_uid[$uid]=$id
  [[ -z "$(status_field "$pid" Groups)" ]] || bad+=("supplementary groups: $(status_field "$pid" Groups)")
  for c in CapInh CapPrm CapEff CapAmb; do
    [[ "$(status_field "$pid" $c)" == 0000000000000000 ]] || bad+=("$c=$(status_field "$pid" $c)")
  done
  [[ "$(status_field "$pid" NoNewPrivs)" == 1 ]] || bad+=("no_new_privs off")
  for t in /proc/"$pid"/task/*; do
    m=$(awk '$1 == "Seccomp:" {print $2}' "$t/status" 2>/dev/null)
    [[ "$m" == 2 ]] || bad+=("thread $(cat "$t/comm" 2>/dev/null) seccomp mode $m")
  done
  grep -q -- --no-seccomp /proc/"$pid"/cmdline 2>/dev/null && bad+=("launched with --no-seccomp")
  [[ "$(cat /proc/"$pid"/oom_score_adj)" == 0 ]] || bad+=("oom_score_adj $(cat /proc/"$pid"/oom_score_adj)")

  cg=$(cat /proc/"$pid"/cgroup)
  [[ "$cg" == "0::/microhosted/$id" ]] || bad+=("cgroup $cg")
  cgd=/sys/fs/cgroup/microhosted/$id
  [[ "$(cat $cgd/cpu.max 2>/dev/null)" == "$((vcpus * CPU_PERIOD)) $CPU_PERIOD" ]] || bad+=("cpu.max $(cat $cgd/cpu.max 2>/dev/null)")
  [[ "$(cat $cgd/memory.max 2>/dev/null)" == "$(((mem + MEM_OVERHEAD_MIB) << 20))" ]] || bad+=("memory.max $(cat $cgd/memory.max 2>/dev/null)")
  [[ "$(cat $cgd/memory.swap.max 2>/dev/null)" == 0 ]] || bad+=("memory.swap.max $(cat $cgd/memory.swap.max 2>/dev/null)")
  [[ "$(cat $cgd/pids.max 2>/dev/null)" == "$((vcpus + PIDS_HEADROOM))" ]] || bad+=("pids.max $(cat $cgd/pids.max 2>/dev/null)")

  [[ "$(stat -c '%a %u' "$disk" 2>/dev/null)" == "600 $uid" ]] || bad+=("disk $(stat -c '%a %u' "$disk" 2>/dev/null)")
  if [[ -n "$tap" ]]; then
    tapinfo=$(ip -d link show "$tap" 2>/dev/null)
    grep -q "user $uid group $uid" <<<"$tapinfo" || bad+=("TAP $tap not owned by $uid")
    if grep -q " master " <<<"$tapinfo"; then
      intra=$("$MH" network inspect "$net" 2>/dev/null | jq -r .intra)
      want="isolated on"; [[ "$intra" == true ]] && want="isolated off"
      grep -q "$want" <<<"$tapinfo" || bad+=("TAP $tap not '$want' (intra=$intra)")
    fi
  fi

  if root; then
    chroot=$(readlink /proc/"$pid"/root)
    [[ "$chroot" == */"$id"/root ]] || bad+=("root is $chroot, not its jail")
    while IFS= read -r -d '' f; do
      rel=${f#"$chroot"}
      case "$(stat -c %F "$f")" in
      "symbolic link") bad+=("symlink in jail: $rel") ;;
      "character special file")
        case "$rel" in /dev/kvm | /dev/net/tun | /dev/userfaultfd) ;; *) bad+=("device in jail: $rel") ;; esac ;;
      "block special file") bad+=("block device in jail: $rel") ;;
      esac
      perm=$(stat -c %a "$f")
      ((8#$perm & 8#6000)) && bad+=("setuid/setgid in jail: $rel")
      [[ "$(stat -c %F "$f")" != "socket" ]] && ((8#$perm & 8#0002)) && bad+=("world-writable in jail: $rel")
      owner=$(stat -c %u "$f")
      [[ "$owner" == 0 || "$owner" == "$uid" ]] || bad+=("jail file $rel owned by $owner")
    done < <(find "$chroot" -mindepth 1 -print0 2>/dev/null)
    grep -q " $chroot" /proc/mounts && bad+=("something is mounted inside the jail")
    for fd in /proc/"$pid"/fd/*; do
      tgt=$(readlink "$fd" 2>/dev/null) || continue
      case "$tgt" in
      "$chroot"/* | socket:* | pipe:* | anon_inode:* | /dev/null) ;;
      *) bad+=("fd ${fd##*/} -> $tgt (outside the jail)") ;;
      esac
    done
    envs=$(tr '\0' '\n' </proc/"$pid"/environ | grep -ciE 'pass|secret|token|key|credential')
    [[ "$envs" == 0 ]] || bad+=("environment carries $envs secret-looking variables")
  fi

  if ((${#bad[@]} == 0)); then
    pass "vm $id (pid $pid, uid $uid)"
  else
    fail "vm $id (pid $pid): $(IFS='; '; echo "${bad[*]}")"
  fi
done
root || skip "jail contents, open descriptors, environment (need root)"

# ---------------------------------------------------------------- guest
log "from inside the guest"
gx() { "$MH" exec "$1" "$2" >/dev/null 2>&1; } # gx VM CMD: true if CMD succeeds in the guest
ip_of() { "$MH" inspect "$1" | jq -r .guest_ip; }
gw_a=$("$MH" network inspect sect-a | jq -r .gateway)
reach() { # reach LABEL VM CMD: PASS if CMD fails in the guest (the path is closed)
  if gx "$2" "$3"; then fail "$1 (reachable)"; else pass "$1"; fi
}

# Positive controls: the tools work and the network is up.
if gx "$I1" "ping -c 2 -W 2 $(ip_of "$I2")"; then pass "control: ping between VMs of an intra network"; else fail "control: intra ping failed — the guest checks below prove nothing"; fi
gx "$A1" "ping -c 1 -W 1 127.0.0.1" && pass "control: ping on loopback" || fail "control: ping on loopback"

reach "gateway does not answer ping" "$A1" "ping -c 2 -W 2 $gw_a"
reach "gateway ssh (22) closed" "$A1" "nc -z -w 3 $gw_a 22"
for hostip in $(ip -4 -o addr show scope global | awk '$2 !~ /^(mhbr|docker|br-|veth|tap)/ {split($4, a, "/"); print a[1]}'); do
  reach "host address $hostip ssh (22) closed" "$A1" "nc -z -w 3 $hostip 22"
  reach "host address $hostip does not answer ping" "$A1" "ping -c 2 -W 2 $hostip"
done
reach "host loopback-only services unreachable (5432 via gateway)" "$A1" "nc -z -w 3 $gw_a 5432"
reach "same network, intra off: no ping" "$A1" "ping -c 2 -W 2 $(ip_of "$A2")"
reach "same network, intra off: no TCP" "$A1" "nc -z -w 3 $(ip_of "$A2") 22"
reach "another network: no ping" "$A1" "ping -c 2 -W 2 $(ip_of "$B1")"
reach "another network (intra): no ping" "$A1" "ping -c 2 -W 2 $(ip_of "$I1")"
reach "no egress: internet unreachable (1.1.1.1 ping)" "$A1" "ping -c 2 -W 2 1.1.1.1"
reach "no egress: internet unreachable (1.1.1.1:53)" "$A1" "nc -z -w 3 1.1.1.1 53"

ll2=$("$MH" exec "$I2" "ip -6 addr show dev eth0 scope link" 2>/dev/null | awk '/inet6/ {split($2, a, "/"); print a[1]; exit}')
if [[ -n "$ll2" ]]; then
  gx "$I2" "ping -6 -c 1 -W 2 $ll2%eth0" && pass "control: IPv6 ping works inside the guest" || fail "control: IPv6 ping does not work — the IPv6 check proves nothing"
  reach "IPv6 between VMs of an intra network dropped" "$I1" "ping -6 -c 2 -W 2 $ll2%eth0"
else
  skip "IPv6: the guest has no link-local address"
fi

# Spoofing, on the intra network where the control proved traffic flows.
i2=$(ip_of "$I2")
spoof=$(awk -F. '{print $1"."$2"."$3".250"}' <<<"$i2")
reach "spoofed source IP dropped" "$I1" "ip addr add $spoof/24 dev eth0 && ping -c 2 -W 2 -I $spoof $i2"
"$MH" exec "$I1" "ip addr del $spoof/24 dev eth0" >/dev/null 2>&1
gx "$I1" "ping -c 2 -W 2 $i2" && pass "control: real address still works after the spoof attempt" || fail "control: real address stopped working"
reach "spoofed MAC dropped" "$I1" "ip link set eth0 address 02:00:00:de:ad:01 && ping -c 2 -W 2 $i2"

echo
echo "$passes passed, $failures failed, $skips skipped"
[[ $failures -eq 0 ]]
