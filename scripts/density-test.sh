#!/usr/bin/env bash
# Density test: how many devices can this host handle, and where does it stop
# being reasonable to deploy more?
#
# Two modes:
#
#   --mode fleet (DEFAULT): simulates a REAL FLEET with the network-per-VM
#     topology — for each "device" it creates its network with fine-grained
#     egress toward its sensor (allowed_egress), boots a VM from the template on
#     it, and verifies the vsock agent responds. It exercises the whole deploy
#     path: subnet pool, bridge, aggregate nftables rule, boot, agent. The number
#     it produces is the production capacity.
#
#   --mode fork: theoretical RAM ceiling — quarantined forks of a snapshot
#     (they share clean CoW pages, no network). Faster and more optimistic.
#
# Stop criteria (the first one met, and the summary says which):
#   - a deploy FAILS (API, boot, or mute agent) → that's a finding
#   - available RAM < --ram-floor-mb (default 500)
#   - a deploy takes > --slow-sec (default 15 s): it's no longer optimal to go on
#   - --max reached (default 200)
#
# It cleans up everything it created on exit (including on Ctrl-C); --keep leaves
# it alive. As root it also measures the real PSS per VM. Typical duration in
# fleet mode: ~2-4 s per device.
#
# Usage: ./scripts/density-test.sh [--mode fleet|fork] [--template alpine-py]
#        [--max 200] [--ram-floor-mb 500] [--slow-sec 15]
#        [--sensor-ip 192.168.0.15] [--keep]

set -euo pipefail

API="${API:-localhost:8080}"
MODE="fleet"
TEMPLATE="alpine-py"
MAX=200
RAM_FLOOR_MB=500
SLOW_SEC=15
SENSOR_IP="192.168.0.15"
KEEP=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)         MODE="$2"; shift 2 ;;
    --template)     TEMPLATE="$2"; shift 2 ;;
    --max)          MAX="$2"; shift 2 ;;
    --ram-floor-mb) RAM_FLOOR_MB="$2"; shift 2 ;;
    --slow-sec)     SLOW_SEC="$2"; shift 2 ;;
    --sensor-ip)    SENSOR_IP="$2"; shift 2 ;;
    --keep)         KEEP=1; shift ;;
    *) echo "usage: $0 [--mode fleet|fork] [--template T] [--max N] [--ram-floor-mb MB] [--slow-sec S] [--sensor-ip IP] [--keep]" >&2; exit 1 ;;
  esac
done
[[ "$MODE" == "fleet" || "$MODE" == "fork" ]] \
  || { echo "ERROR: --mode must be fleet or fork" >&2; exit 1; }

command -v jq >/dev/null || { echo "ERROR: jq is required" >&2; exit 1; }
curl -sf "http://$API/v1/health" >/dev/null \
  || { echo "ERROR: the API isn't responding at $API" >&2; exit 1; }

# Record of what was created, one line per device: "<vm-id> <net|->".
CREATED_FILE="$(mktemp /tmp/mh-density-XXXXXX.txt)"
BASE_ID=""
SNAP_ID=""

avail_mb() { free -m | awk '/^Mem/{print $7}'; }
now_ms()   { date +%s%3N; }

# Wait for a VM's vsock agent (boot returns before it's listening).
agent_ok() { # $1=vm-id $2=timeout-s
  local out=""
  for _ in $(seq 1 "$2"); do
    out=$(curl -s -X POST "http://$API/v1/vms/$1/exec" -d '{"cmd":"echo ok"}' | jq -r '.output // empty')
    [[ "$out" == ok* ]] && return 0
    sleep 1
  done
  return 1
}

cleanup() {
  [[ "$KEEP" -eq 1 ]] && { echo ""; echo "--keep: fleet left alive. Record in $CREATED_FILE"; return; }
  echo ""
  echo "==> Cleaning up..."
  local fails=0 vm net code
  while read -r vm net; do
    code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "http://$API/v1/vms/$vm")
    [[ "$code" == 2* ]] || fails=$((fails + 1))
    if [[ "$net" != "-" ]]; then
      code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "http://$API/v1/networks/$net")
      [[ "$code" == 2* ]] || fails=$((fails + 1))
    fi
  done < "$CREATED_FILE"
  [[ -n "$SNAP_ID" ]] && curl -s -o /dev/null -X DELETE "http://$API/v1/snapshots/$SNAP_ID"
  [[ -n "$BASE_ID" ]] && curl -s -o /dev/null -X DELETE "http://$API/v1/vms/$BASE_ID"
  if [[ "$fails" -gt 0 ]]; then
    echo "    NOTE: $fails deletions failed — retry with $CREATED_FILE"
  else
    rm -f "$CREATED_FILE"
    echo "    OK: everything the test created is deleted."
  fi
}
trap cleanup EXIT

# --- Fork mode preparation: base VM + snapshot -----------------------------
if [[ "$MODE" == "fork" ]]; then
  echo "==> [fork] base VM from '$TEMPLATE' + snapshot..."
  BASE_ID=$(curl -s -X POST "http://$API/v1/vms" -d "{\"template\":\"$TEMPLATE\"}" | jq -r '.id // empty')
  [[ -n "$BASE_ID" ]] || { echo "ERROR creating the base VM" >&2; exit 1; }
  agent_ok "$BASE_ID" 30 || { echo "ERROR: base agent is mute" >&2; exit 1; }
  SNAP_ID=$(curl -s -X POST "http://$API/v1/vms/$BASE_ID/snapshot" -d '{"name":"density-test"}' | jq -r '.id // empty')
  [[ -n "$SNAP_ID" ]] || { echo "ERROR creating the snapshot" >&2; exit 1; }
fi

# --- Deploy loop ------------------------------------------------------------
RAM_START=$(avail_mb)
echo "==> Deploying (mode $MODE, max $MAX, floor ${RAM_FLOOR_MB} MB, slow >${SLOW_SEC}s; RAM: ${RAM_START} MB)"
N=0
FIRST_MS=0
LAST_MS=0
LAST_VM=""
STOP="max reached ($MAX)"
while [[ "$N" -lt "$MAX" ]]; do
  i=$((N + 1))
  t0=$(now_ms)

  if [[ "$MODE" == "fleet" ]]; then
    net="fleet-$i"
    resp=$(curl -s -X POST "http://$API/v1/networks" \
      -d "{\"name\":\"$net\",\"allowed_egress\":[{\"ip\":\"$SENSOR_IP\",\"protocol\":\"tcp\",\"port\":1883}]}")
    if [[ -z "$(echo "$resp" | jq -r '.name // empty')" ]]; then
      STOP="network $i rejected: $(echo "$resp" | head -c 200)"; break
    fi
    resp=$(curl -s -X POST "http://$API/v1/vms" -d "{\"template\":\"$TEMPLATE\",\"network\":\"$net\"}")
    vm=$(echo "$resp" | jq -r '.id // empty')
    if [[ -z "$vm" ]]; then
      curl -s -o /dev/null -X DELETE "http://$API/v1/networks/$net"
      STOP="VM $i rejected: $(echo "$resp" | head -c 200)"; break
    fi
    echo "$vm $net" >> "$CREATED_FILE"
    if ! agent_ok "$vm" 20; then
      STOP="VM $i ($vm) booted but the agent doesn't respond"; break
    fi
  else
    resp=$(curl -s -X POST "http://$API/v1/snapshots/$SNAP_ID/fork" -d '{"quarantine":true}')
    vm=$(echo "$resp" | jq -r '.id // empty')
    if [[ -z "$vm" ]]; then
      STOP="fork $i rejected: $(echo "$resp" | head -c 200)"; break
    fi
    echo "$vm -" >> "$CREATED_FILE"
  fi

  LAST_VM="$vm"
  N=$i
  LAST_MS=$(( $(now_ms) - t0 ))
  [[ "$N" -eq 1 ]] && FIRST_MS=$LAST_MS
  avail=$(avail_mb)
  printf '%s %3d → %s | %5d ms | RAM: %5d MB\n' \
    "$([[ "$MODE" == fleet ]] && echo device || echo fork)" "$N" "$vm" "$LAST_MS" "$avail"

  if [[ "$avail" -lt "$RAM_FLOOR_MB" ]]; then
    STOP="RAM floor (${avail} MB < ${RAM_FLOOR_MB} MB)"; break
  fi
  if [[ "$LAST_MS" -gt $((SLOW_SEC * 1000)) ]]; then
    STOP="deploy $N took ${LAST_MS} ms (> ${SLOW_SEC}s): stop deploying here"; break
  fi
done

# --- Final check: is the whole fleet still alive? Sample 1st/middle/last
ALIVE="n/a"
if [[ "$N" -gt 0 && "$MODE" == "fleet" ]]; then
  ALIVE="yes"
  mid=$(( (N + 1) / 2 ))
  for vm in $(awk -v m="$mid" 'NR==1||NR==m{print $1}' "$CREATED_FILE") "$LAST_VM"; do
    agent_ok "$vm" 5 || ALIVE="NO ($vm doesn't respond)"
  done
elif [[ "$N" -gt 0 ]]; then
  ALIVE="yes"
  for vm in "$(awk 'NR==1{print $1}' "$CREATED_FILE")" "$LAST_VM"; do
    agent_ok "$vm" 5 || ALIVE="NO ($vm doesn't respond)"
  done
fi

PSS="(run as root to measure PSS)"
if [[ "$N" -gt 0 ]]; then
  pid=$(curl -s "http://$API/v1/vms/$LAST_VM" | jq -r '.pid // empty')
  [[ -n "$pid" && -r "/proc/$pid/smaps_rollup" ]] \
    && PSS="$(awk '/^Pss:/{print $2 " kB"}' "/proc/$pid/smaps_rollup")"
fi

RAM_END=$(avail_mb)
HEALTH=$(curl -s "http://$API/v1/health" | jq -c '.')

echo ""
echo "================== RESULT ($MODE) =================="
echo "Deployed:               $N"
echo "Stop reason:            $STOP"
echo "Alive (sample):         $ALIVE"
echo "Deploy 1st vs last:     ${FIRST_MS} ms → ${LAST_MS} ms"
echo "Last VM PSS:            $PSS"
echo "RAM: ${RAM_START} MB → ${RAM_END} MB"
[[ "$N" -gt 0 ]] && echo "Average cost per unit:  $(( (RAM_START - RAM_END) / N )) MB"
echo "Health: $HEALTH"
echo "==================================================="
