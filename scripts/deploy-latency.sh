#!/usr/bin/env bash
# Deploy latency: how long does ONE device take from API call to a working
# agent, and where does that time go?
#
# Complements density-test.sh (behaviour as the host fills up): here every
# device is created and destroyed before the next one, on an otherwise quiet
# host, and the agent is polled every 10 ms so the measurement is not quantised
# by the poll interval. For each run it records:
#   - net:    POST /v1/networks (own network, fine-grained egress, as in fleet)
#   - create: POST /v1/vms until it returns (clone, TAP, VMM launch)
#   - agent:  from create returning to the vsock agent answering
#   - uptime: the guest's own /proc/uptime at that moment (its boot time)
# The daemon's journal has the per-phase split of each create
# ("vm <id> created in ..."), printed at the end when readable.
#
# Everything it creates carries the label latency-run=<run id> and networks are
# named lt<run id>-<n>; it is all removed on exit (Ctrl-C included).
#
# Usage: scripts/deploy-latency.sh [--runs 10] [--template base-alpine]
#          [--sensor-ip 192.168.0.15] [--no-network]
#
# Env:   SOCKET=/run/microhosted.sock   the daemon's socket
#        API=host:port                  drive a TCP listener instead

set -uo pipefail

SOCKET="${SOCKET:-/run/microhosted.sock}"
API="${API:-}"
if [[ -n "$API" ]]; then
  CURL=(curl -s)
  BASE="http://${API}"
else
  CURL=(curl -s --unix-socket "$SOCKET")
  BASE="http://localhost"
fi
RUNS=10
TEMPLATE="base-alpine"
SENSOR_IP="192.168.0.15"
NO_NETWORK=0

usage() {
  echo "usage: $0 [--runs N] [--template T] [--sensor-ip IP] [--no-network]" >&2
  exit 2
}
while [[ $# -gt 0 ]]; do
  case "$1" in
    --runs)       RUNS="$2"; shift 2 ;;
    --template)   TEMPLATE="$2"; shift 2 ;;
    --sensor-ip)  SENSOR_IP="$2"; shift 2 ;;
    --no-network) NO_NETWORK=1; shift ;;
    *) usage ;;
  esac
done
[[ "$RUNS" =~ ^[0-9]+$ && "$RUNS" -gt 0 ]] || usage
for bin in curl jq; do
  command -v "$bin" >/dev/null || { echo "ERROR: $bin is required" >&2; exit 1; }
done

# api METHOD PATH [BODY]: sets CODE and RESP.
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
now_ms() { date +%s%3N; }

RUN="$(date +%H%M%S)"
NETPREFIX="lt${RUN}-"
VM=""
NET=""
IDS=()

# teardown: remove the current run's VM and network, if any.
teardown() {
  [[ -n "$VM" ]] && api DELETE "/v1/vms/$VM"
  [[ -n "$NET" ]] && api DELETE "/v1/networks/$NET"
  VM=""
  NET=""
}
trap teardown EXIT

# agent_wait VM: polls the agent every 10 ms for up to 20 s.
agent_wait() {
  local out deadline=$(( $(now_ms) + 20000 ))
  while [[ "$(now_ms)" -lt "$deadline" ]]; do
    out=$("${CURL[@]}" -X POST "$BASE/v1/vms/$1/exec" -d '{"cmd":"echo ok"}' | jq -r '.output // empty' 2>/dev/null)
    [[ "$out" == ok* ]] && return 0
    sleep 0.01
  done
  return 1
}

# stats NAME VALUES...: prints median, p95 and min/max.
stats() {
  local name="$1"; shift
  printf '%s\n' "$@" | sort -n | awk -v name="$name" '
    { v[NR] = $1 }
    END {
      m = (NR % 2) ? v[(NR + 1) / 2] : (v[NR / 2] + v[NR / 2 + 1]) / 2
      p = int(NR * 0.95); if (p < 1) p = 1; if (p > NR) p = NR
      printf "  %-8s median %6.0f ms   p95 %6.0f ms   min %6.0f   max %6.0f\n", name, m, v[p], v[1], v[NR]
    }'
}

echo "==> Run $RUN: $RUNS deploys of $TEMPLATE, $([[ $NO_NETWORK -eq 1 ]] && echo "no network" || echo "own network each")"
printf '%-4s %-9s %6s %7s %6s %7s %7s\n' "#" "vm" "net" "create" "agent" "total" "uptime"
NETS=() CREATES=() AGENTS=() TOTALS=()
for i in $(seq 1 "$RUNS"); do
  t0=$(now_ms)
  body="{\"template\":\"$TEMPLATE\",\"labels\":{\"latency-run\":\"$RUN\"}"
  if [[ "$NO_NETWORK" -eq 1 ]]; then
    body="$body,\"no_network\":true}"
  else
    NET="${NETPREFIX}${i}"
    api POST /v1/networks "{\"name\":\"$NET\",\"allowed_egress\":[{\"ip\":\"$SENSOR_IP\",\"protocol\":\"tcp\",\"port\":1883}]}"
    [[ "$CODE" == 2* ]] || { echo "ERROR: network refused ($CODE): $(errmsg)" >&2; NET=""; exit 1; }
    body="$body,\"network\":\"$NET\"}"
  fi
  t1=$(now_ms)
  api POST /v1/vms "$body"
  VM=$(jq -r '.id // empty' <<<"$RESP")
  [[ -n "$VM" ]] || { echo "ERROR: create refused ($CODE): $(errmsg)" >&2; exit 1; }
  t2=$(now_ms)
  agent_wait "$VM" || { echo "ERROR: vm $VM booted but its agent is silent" >&2; exit 1; }
  t3=$(now_ms)
  api POST "/v1/vms/$VM/exec" '{"cmd":"cut -d\" \" -f1 /proc/uptime"}'
  uptime=$(jq -r '.output // "-"' <<<"$RESP" | tr -d '\n')

  printf '%-4s %-9s %4sms %5sms %4sms %5sms %6ss\n' "$i" "$VM" "$((t1 - t0))" "$((t2 - t1))" "$((t3 - t2))" "$((t3 - t0))" "$uptime"
  NETS+=("$((t1 - t0))") CREATES+=("$((t2 - t1))") AGENTS+=("$((t3 - t2))") TOTALS+=("$((t3 - t0))")
  IDS+=("$VM")
  teardown
done

echo ""
echo "==> Summary ($RUNS deploys)"
[[ "$NO_NETWORK" -eq 1 ]] || stats net "${NETS[@]}"
stats create "${CREATES[@]}"
stats agent "${AGENTS[@]}"
stats total "${TOTALS[@]}"

# Per-phase split of each create, from the daemon's own log line.
if lines=$(journalctl -u microhosted --since "-10min" --no-pager -o cat 2>/dev/null | grep -E "vm ($(IFS='|'; echo "${IDS[*]}")) created in"); then
  echo ""
  echo "==> Daemon-side split of each create"
  sed 's/^/  /' <<<"$lines"
fi
