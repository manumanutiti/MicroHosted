#!/usr/bin/env bash
# Density and pressure test: how many devices can this host run, and how does
# the host behave as it fills up?
#
# Two modes:
#
#   --mode fleet (DEFAULT): a REAL fleet with the network-per-VM topology — for
#     each "device" it creates its own network (with fine-grained egress towards
#     its sensor), boots a VM from the template on it, and waits for the vsock
#     agent. It exercises the whole deploy path: subnet pool, bridge, nftables
#     re-render, clone, boot, agent. The number it produces is the production
#     capacity.
#
#   --mode fork: theoretical RAM ceiling — quarantined forks of one snapshot
#     (clean pages shared copy-on-write, no network). Faster, more optimistic.
#
# For every device it records, and writes to a CSV:
#   - time to create the network, to boot the VM, and for its agent to answer
#   - host available memory, and the kernel's pressure-stall information (PSI:
#     % of the last 10 s some task was stalled on memory, CPU and I/O)
#   - load average and the daemon's own resident memory
# and it follows the event stream to count VMs that die while it runs.
#
# Stop criteria (the first one met; the summary says which):
#   - the daemon refuses a launch for capacity (503): admission control is doing
#     its job — this is the expected end on a host that is not overcommitted
#   - any other failure (API, boot, silent agent) → a finding
#   - available memory < --ram-floor-mb (default 500)
#   - memory pressure (PSI some avg10) > --psi-max (default 20 %)
#   - one deploy takes > --slow-sec (default 15 s)
#   - --max reached (default 200)
#
# Everything it creates carries the label density-run=<run id> and networks are
# named dt<run id>-<n>, so cleanup finds them even if this script is killed:
# it runs on exit (Ctrl-C included) unless --keep, and ends with the daemon's
# drift check. --cleanup-leftovers removes what any earlier run left behind.
#
# Usage: scripts/density-test.sh [--mode fleet|fork] [--template base-alpine]
#          [--max 200] [--ram-floor-mb 500] [--psi-max 20] [--slow-sec 15]
#          [--sensor-ip 192.168.0.15] [--csv FILE] [--keep]
#        scripts/density-test.sh --cleanup-leftovers
#
# Env:   SOCKET=/run/microhosted.sock   the daemon's socket
#        API=host:port                  drive a TCP listener instead
# As root it also measures the real PSS of the last VM.

set -uo pipefail

SOCKET="${SOCKET:-/run/microhosted.sock}"
API="${API:-}"
if [[ -n "$API" ]]; then
  CURL=(curl -s)
  BASE="http://${API}"
  API_DESC="${API}"
else
  CURL=(curl -s --unix-socket "$SOCKET")
  BASE="http://localhost"
  API_DESC="unix ${SOCKET}"
fi
MODE="fleet"
TEMPLATE="base-alpine"
MAX=200
RAM_FLOOR_MB=500
PSI_MAX=20
SLOW_SEC=15
SENSOR_IP="192.168.0.15"
KEEP=0
CSV=""
LEFTOVERS=0

usage() {
  echo "usage: $0 [--mode fleet|fork] [--template T] [--max N] [--ram-floor-mb MB] [--psi-max PCT]" >&2
  echo "          [--slow-sec S] [--sensor-ip IP] [--csv FILE] [--keep] | --cleanup-leftovers" >&2
  exit 1
}
while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)              MODE="$2"; shift 2 ;;
    --template)          TEMPLATE="$2"; shift 2 ;;
    --max)               MAX="$2"; shift 2 ;;
    --ram-floor-mb)      RAM_FLOOR_MB="$2"; shift 2 ;;
    --psi-max)           PSI_MAX="$2"; shift 2 ;;
    --slow-sec)          SLOW_SEC="$2"; shift 2 ;;
    --sensor-ip)         SENSOR_IP="$2"; shift 2 ;;
    --csv)               CSV="$2"; shift 2 ;;
    --keep)              KEEP=1; shift ;;
    --cleanup-leftovers) LEFTOVERS=1; shift ;;
    *) usage ;;
  esac
done
[[ "$MODE" == "fleet" || "$MODE" == "fork" ]] || { echo "ERROR: --mode must be fleet or fork" >&2; exit 1; }
command -v jq >/dev/null || { echo "ERROR: jq is required" >&2; exit 1; }
"${CURL[@]}" -f "$BASE/v1/health" >/dev/null \
  || { echo "ERROR: the API isn't responding at $API_DESC" >&2; exit 1; }

# api METHOD PATH [BODY]: sets RESP (body) and CODE (HTTP status).
api() {
  local out
  if [[ $# -ge 3 ]]; then
    out=$("${CURL[@]}" -w '\n%{http_code}' -X "$1" "$BASE$2" -d "$3")
  else
    out=$("${CURL[@]}" -w '\n%{http_code}' -X "$1" "$BASE$2")
  fi
  CODE="${out##*$'\n'}"
  RESP="${out%$'\n'*}"
}
errmsg() { jq -r '.error // empty' <<<"$RESP" 2>/dev/null | head -c 200; }

# --- Cleanup ------------------------------------------------------------------

# sweep SELECTOR NET_REGEX: destroy every VM matching the jq selector over
# GET /v1/vms, then every network whose name matches the regex. Prints what
# failed; returns the number of failures.
sweep() {
  local sel="$1" netre="$2" fails=0 id name
  api GET /v1/vms
  for id in $(jq -r ".[] | select($sel) | .id" <<<"$RESP"); do
    api DELETE "/v1/vms/$id"
    [[ "$CODE" == 2* ]] || { echo "    could not delete vm $id: $(errmsg)"; fails=$((fails + 1)); }
  done
  api GET /v1/snapshots
  for id in $(jq -r '.[] | select((.name // "") | test("^density-")) | .id' <<<"$RESP"); do
    api DELETE "/v1/snapshots/$id"
  done
  api GET /v1/networks
  for name in $(jq -r --arg re "$netre" '.[] | select(.name | test($re)) | .name' <<<"$RESP"); do
    api DELETE "/v1/networks/$name"
    [[ "$CODE" == 2* ]] || { echo "    could not delete network $name: $(errmsg)"; fails=$((fails + 1)); }
  done
  return "$fails"
}

doctor() {
  api GET /v1/doctor
  if [[ "$(jq -r .clean <<<"$RESP")" == "true" ]]; then
    echo "    doctor: clean — the daemon and the host agree"
  else
    echo "    doctor: NOT clean:"
    jq -r '.findings[] | "      \(.kind)  \(.object)  \(.detail)"' <<<"$RESP"
  fi
}

if [[ "$LEFTOVERS" -eq 1 ]]; then
  echo "==> Removing what earlier density runs left behind..."
  sweep '.labels["density-run"] != null' '^dt[0-9]+-[0-9]+$'
  echo "    $? failure(s)"
  doctor
  exit 0
fi

RUN="$(date +%H%M%S)"
LABEL="density-run=$RUN"
NETPREFIX="dt${RUN}-"
CSV="${CSV:-/tmp/density-$RUN.csv}"
EVENTS_FILE="$(mktemp /tmp/density-events-XXXXXX)"
EVENTS_PID=""
BASE_ID=""

cleanup() {
  [[ -n "$EVENTS_PID" ]] && kill "$EVENTS_PID" 2>/dev/null
  if [[ "$KEEP" -eq 1 ]]; then
    echo ""
    echo "--keep: left alive. Remove later with: $0 --cleanup-leftovers"
    return
  fi
  echo ""
  echo "==> Cleaning up run $RUN..."
  local t0=$SECONDS
  if sweep ".labels[\"density-run\"] == \"$RUN\"" "^${NETPREFIX}[0-9]+\$"; then
    echo "    everything the test created is deleted ($((SECONDS - t0)) s)"
  else
    echo "    some deletions failed — retry with: $0 --cleanup-leftovers"
  fi
  doctor
  rm -f "$EVENTS_FILE"
}
trap cleanup EXIT

# --- Measurements -------------------------------------------------------------

avail_mb() { awk '/^MemAvailable:/{print int($2/1024)}' /proc/meminfo; }
psi() { awk '/^some/{split($2,a,"="); print a[2]}' "/proc/pressure/$1" 2>/dev/null || echo "-"; }
load1() { cut -d' ' -f1 /proc/loadavg; }
DAEMON_PID="$(systemctl show -p MainPID --value microhosted 2>/dev/null || true)"
daemon_rss_mb() {
  [[ -n "$DAEMON_PID" && "$DAEMON_PID" != 0 ]] || { echo "-"; return; }
  awk '/^VmRSS:/{print int($2/1024)}' "/proc/$DAEMON_PID/status" 2>/dev/null || echo "-"
}
now_ms() { date +%s%3N; }
deaths() { grep -c '^event: vm.died' "$EVENTS_FILE" 2>/dev/null || true; }

# agent_ok VM TIMEOUT_S: wait up to TIMEOUT_S seconds for the vsock agent
# (create returns before it listens). Polls every 20 ms: the guest is usually
# up in ~200 ms, so a coarse interval would dominate the measured agent time.
agent_ok() {
  local out deadline=$(( $(now_ms) + $2 * 1000 ))
  while [[ "$(now_ms)" -lt "$deadline" ]]; do
    out=$("${CURL[@]}" -X POST "$BASE/v1/vms/$1/exec" -d '{"cmd":"echo ok"}' | jq -r '.output // empty' 2>/dev/null)
    [[ "$out" == ok* ]] && return 0
    sleep 0.02
  done
  return 1
}

# Deaths of our VMs while the test runs (OOM kills, crashes).
"${CURL[@]}" -N "$BASE/v1/events?since=now&type=vm.died&label=$LABEL" >"$EVENTS_FILE" 2>/dev/null &
EVENTS_PID=$!

# --- Fork mode preparation: base VM + snapshot ---------------------------------
SNAP_ID=""
if [[ "$MODE" == "fork" ]]; then
  echo "==> [fork] base VM from '$TEMPLATE' + snapshot..."
  api POST /v1/vms "{\"template\":\"$TEMPLATE\",\"labels\":{\"density-run\":\"$RUN\"}}"
  BASE_ID=$(jq -r '.id // empty' <<<"$RESP")
  [[ -n "$BASE_ID" ]] || { echo "ERROR creating the base VM: $(errmsg)" >&2; exit 1; }
  agent_ok "$BASE_ID" 30 || { echo "ERROR: the base VM's agent is silent" >&2; exit 1; }
  api POST "/v1/vms/$BASE_ID/snapshot" "{\"name\":\"density-$RUN\"}"
  SNAP_ID=$(jq -r '.id // empty' <<<"$RESP")
  [[ -n "$SNAP_ID" ]] || { echo "ERROR creating the snapshot: $(errmsg)" >&2; exit 1; }
fi

# --- Deploy loop ----------------------------------------------------------------
RAM_START=$(avail_mb)
echo "n,vm,net_ms,vm_ms,agent_ms,total_ms,avail_mb,psi_mem,psi_cpu,psi_io,load1,daemon_rss_mb" >"$CSV"
echo "==> Run $RUN: mode $MODE, template $TEMPLATE, max $MAX"
echo "    stops at: capacity 503 | avail < ${RAM_FLOOR_MB} MB | PSI mem > ${PSI_MAX}% | deploy > ${SLOW_SEC}s"
echo "    host: $(nproc) CPUs, ${RAM_START} MB available, PSI mem/cpu/io $(psi memory)/$(psi cpu)/$(psi io)%"
echo ""
printf '%-5s %-9s %7s %7s %7s %8s %7s %6s %6s %6s %5s %6s\n' \
  "#" "vm" "net" "vm" "agent" "total" "avail" "psiM" "psiC" "psiIO" "load" "dRSS"

N=0
FIRST_MS=0
LAST_MS=0
LAST_VM=""
STOP="max reached ($MAX)"
while [[ "$N" -lt "$MAX" ]]; do
  i=$((N + 1))
  t0=$(now_ms)
  net_ms=0

  if [[ "$MODE" == "fleet" ]]; then
    net="${NETPREFIX}${i}"
    api POST /v1/networks "{\"name\":\"$net\",\"allowed_egress\":[{\"ip\":\"$SENSOR_IP\",\"protocol\":\"tcp\",\"port\":1883}]}"
    if [[ "$CODE" != 2* ]]; then STOP="network $i refused ($CODE): $(errmsg)"; break; fi
    t1=$(now_ms); net_ms=$((t1 - t0))
    api POST /v1/vms "{\"template\":\"$TEMPLATE\",\"network\":\"$net\",\"labels\":{\"density-run\":\"$RUN\"}}"
  else
    t1=$(now_ms)
    api POST "/v1/snapshots/$SNAP_ID/fork" "{\"quarantine\":true,\"labels\":{\"density-run\":\"$RUN\"}}"
  fi
  vm=$(jq -r '.id // empty' <<<"$RESP")
  if [[ -z "$vm" ]]; then
    if [[ "$CODE" == 503 ]]; then
      STOP="admission control refused device $i (503): $(errmsg)"
    else
      STOP="device $i refused ($CODE): $(errmsg)"
    fi
    break
  fi
  t2=$(now_ms); vm_ms=$((t2 - t1))
  agent_ms=0
  if [[ "$MODE" == "fleet" ]]; then
    if ! agent_ok "$vm" 20; then STOP="device $i ($vm) booted but its agent is silent"; break; fi
    agent_ms=$(( $(now_ms) - t2 ))
  fi

  LAST_VM="$vm"
  N=$i
  LAST_MS=$(( $(now_ms) - t0 ))
  [[ "$N" -eq 1 ]] && FIRST_MS=$LAST_MS
  avail=$(avail_mb); pm=$(psi memory); pc=$(psi cpu); pio=$(psi io); ld=$(load1); drss=$(daemon_rss_mb)
  echo "$N,$vm,$net_ms,$vm_ms,$agent_ms,$LAST_MS,$avail,$pm,$pc,$pio,$ld,$drss" >>"$CSV"
  printf '%-5s %-9s %5sms %5sms %5sms %6sms %5sMB %5s%% %5s%% %5s%% %5s %4sMB\n' \
    "$N" "$vm" "$net_ms" "$vm_ms" "$agent_ms" "$LAST_MS" "$avail" "$pm" "$pc" "$pio" "$ld" "$drss"

  if [[ "$avail" -lt "$RAM_FLOOR_MB" ]]; then STOP="memory floor (${avail} MB < ${RAM_FLOOR_MB} MB)"; break; fi
  if awk -v p="$pm" -v m="$PSI_MAX" 'BEGIN{exit !(p+0 > m+0)}'; then STOP="memory pressure (PSI ${pm}% > ${PSI_MAX}%)"; break; fi
  if [[ "$LAST_MS" -gt $((SLOW_SEC * 1000)) ]]; then STOP="device $N took ${LAST_MS} ms (> ${SLOW_SEC}s)"; break; fi
done

# --- Is the whole fleet still alive? Sample first / middle / last ---------------
ALIVE="n/a"
if [[ "$N" -gt 0 ]]; then
  ALIVE="yes"
  api GET "/v1/vms?label=$LABEL"
  running=$(jq '[.[] | select(.state == "running")] | length' <<<"$RESP")
  mid=$(( (N + 1) / 2 ))
  for vm in $(awk -F, -v m="$mid" 'NR==2||NR==m+1{print $2}' "$CSV") "$LAST_VM"; do
    agent_ok "$vm" 5 || ALIVE="NO ($vm does not answer)"
  done
fi

PSS="(run as root to measure)"
if [[ -n "$LAST_VM" ]]; then
  api GET "/v1/vms/$LAST_VM"
  pid=$(jq -r '.pid // empty' <<<"$RESP")
  if [[ -n "$pid" ]] && pss=$(awk '/^Pss:/{print int($2/1024) " MB"}' "/proc/$pid/smaps_rollup" 2>/dev/null) && [[ -n "$pss" ]]; then
    PSS="$pss"
  fi
fi

sleep 1
RAM_END=$(avail_mb)
api GET /v1/health
HEALTH=$(jq -r .status <<<"$RESP")

echo ""
echo "===================== RESULT (run $RUN, $MODE) ====================="
echo "Deployed:                $N"
echo "Stop reason:             $STOP"
echo "Running now / sampled:   ${running:-0} running; sample alive: $ALIVE"
echo "Died during the run:     $(deaths)  (vm.died events)"
echo "Deploy time 1st → last:  ${FIRST_MS} ms → ${LAST_MS} ms"
if [[ "$N" -gt 0 ]]; then
  awk -F, 'NR>1{n++; net+=$3; vm+=$4; ag+=$5} END{if(n) printf "Average per device:      network %d ms, VM %d ms, agent %d ms\n", net/n, vm/n, ag/n}' "$CSV"
  echo "Memory available:        ${RAM_START} MB → ${RAM_END} MB  (~$(( (RAM_START - RAM_END) / N )) MB per device)"
  awk -F, 'NR>1{if($8+0>m)m=$8+0; if($9+0>c)c=$9+0; if($10+0>io)io=$10+0} END{printf "Peak PSI (avg10):        memory %.2f%%, cpu %.2f%%, io %.2f%%\n", m, c, io}' "$CSV"
  echo "Daemon RSS:              $(awk -F, 'NR==2{print $12}' "$CSV") MB → $(daemon_rss_mb) MB"
fi
echo "Last VM PSS:             $PSS"
echo "Health:                  $HEALTH"
echo "Per-device data:         $CSV"
echo "===================================================================="
