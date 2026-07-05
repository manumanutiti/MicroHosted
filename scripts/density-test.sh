#!/usr/bin/env bash
# Test de densidad: ¿cuántos dispositivos aguanta este host, y dónde deja de
# ser razonable desplegar más?
#
# Dos modos:
#
#   --mode fleet (DEFAULT): simula una FLOTA REAL con la topología
#     red-por-VM — por cada "dispositivo" crea su red con egress fino hacia
#     su sensor (allowed_egress), levanta una VM de la plantilla en ella y
#     verifica que el agente vsock responde. Ejercita el camino entero de
#     despliegue: subnet pool, bridge, regla nftables agregada, boot, agente.
#     El número que sale es la capacidad de producción.
#
#   --mode fork: techo teórico de RAM — forks en cuarentena de un snapshot
#     (comparten páginas limpias CoW, sin red). Más rápido y más optimista.
#
# Criterios de parada (el primero que se cumpla, y el resumen dice cuál):
#   - un despliegue FALLA (API, boot o agente mudo) → eso es un hallazgo
#   - RAM disponible < --ram-floor-mb (default 500)
#   - un despliegue tarda > --slow-sec (default 15 s): ya no es óptimo seguir
#   - --max alcanzado (default 200)
#
# Limpia todo lo que creó al salir (también con Ctrl-C); --keep lo deja vivo.
# Como root mide además el PSS real por VM. Duración típica en modo fleet:
# ~2-4 s por dispositivo.
#
# Uso: ./scripts/density-test.sh [--mode fleet|fork] [--template alpine-py]
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
    *) echo "uso: $0 [--mode fleet|fork] [--template T] [--max N] [--ram-floor-mb MB] [--slow-sec S] [--sensor-ip IP] [--keep]" >&2; exit 1 ;;
  esac
done
[[ "$MODE" == "fleet" || "$MODE" == "fork" ]] \
  || { echo "ERROR: --mode debe ser fleet o fork" >&2; exit 1; }

command -v jq >/dev/null || { echo "ERROR: hace falta jq" >&2; exit 1; }
curl -sf "http://$API/v1/health" >/dev/null \
  || { echo "ERROR: la API no responde en $API" >&2; exit 1; }

# Registro de lo creado, una línea por dispositivo: "<vm-id> <red|->".
CREATED_FILE="$(mktemp /tmp/mh-density-XXXXXX.txt)"
BASE_ID=""
SNAP_ID=""

avail_mb() { free -m | awk '/^Mem/{print $7}'; }
now_ms()   { date +%s%3N; }

# Espera al agente vsock de una VM (el boot devuelve antes de que escuche).
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
  [[ "$KEEP" -eq 1 ]] && { echo ""; echo "--keep: flota viva. Registro en $CREATED_FILE"; return; }
  echo ""
  echo "==> Limpiando..."
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
    echo "    OJO: $fails borrados fallaron — reintenta con $CREATED_FILE"
  else
    rm -f "$CREATED_FILE"
    echo "    OK: todo lo creado por el test está borrado."
  fi
}
trap cleanup EXIT

# --- Preparación del modo fork: VM base + snapshot -------------------------
if [[ "$MODE" == "fork" ]]; then
  echo "==> [fork] VM base de '$TEMPLATE' + snapshot..."
  BASE_ID=$(curl -s -X POST "http://$API/v1/vms" -d "{\"template\":\"$TEMPLATE\"}" | jq -r '.id // empty')
  [[ -n "$BASE_ID" ]] || { echo "ERROR creando la VM base" >&2; exit 1; }
  agent_ok "$BASE_ID" 30 || { echo "ERROR: agente de la base mudo" >&2; exit 1; }
  SNAP_ID=$(curl -s -X POST "http://$API/v1/vms/$BASE_ID/snapshot" -d '{"name":"density-test"}' | jq -r '.id // empty')
  [[ -n "$SNAP_ID" ]] || { echo "ERROR creando el snapshot" >&2; exit 1; }
fi

# --- Bucle de despliegue ----------------------------------------------------
RAM_START=$(avail_mb)
echo "==> Desplegando (modo $MODE, máx $MAX, suelo ${RAM_FLOOR_MB} MB, lento >${SLOW_SEC}s; RAM: ${RAM_START} MB)"
N=0
FIRST_MS=0
LAST_MS=0
LAST_VM=""
STOP="máximo alcanzado ($MAX)"
while [[ "$N" -lt "$MAX" ]]; do
  i=$((N + 1))
  t0=$(now_ms)

  if [[ "$MODE" == "fleet" ]]; then
    net="fleet-$i"
    resp=$(curl -s -X POST "http://$API/v1/networks" \
      -d "{\"name\":\"$net\",\"allowed_egress\":[{\"ip\":\"$SENSOR_IP\",\"protocol\":\"tcp\",\"port\":1883}]}")
    if [[ -z "$(echo "$resp" | jq -r '.name // empty')" ]]; then
      STOP="red $i rechazada: $(echo "$resp" | head -c 200)"; break
    fi
    resp=$(curl -s -X POST "http://$API/v1/vms" -d "{\"template\":\"$TEMPLATE\",\"network\":\"$net\"}")
    vm=$(echo "$resp" | jq -r '.id // empty')
    if [[ -z "$vm" ]]; then
      curl -s -o /dev/null -X DELETE "http://$API/v1/networks/$net"
      STOP="VM $i rechazada: $(echo "$resp" | head -c 200)"; break
    fi
    echo "$vm $net" >> "$CREATED_FILE"
    if ! agent_ok "$vm" 20; then
      STOP="VM $i ($vm) arrancó pero el agente no responde"; break
    fi
  else
    resp=$(curl -s -X POST "http://$API/v1/snapshots/$SNAP_ID/fork" -d '{"quarantine":true}')
    vm=$(echo "$resp" | jq -r '.id // empty')
    if [[ -z "$vm" ]]; then
      STOP="fork $i rechazado: $(echo "$resp" | head -c 200)"; break
    fi
    echo "$vm -" >> "$CREATED_FILE"
  fi

  LAST_VM="$vm"
  N=$i
  LAST_MS=$(( $(now_ms) - t0 ))
  [[ "$N" -eq 1 ]] && FIRST_MS=$LAST_MS
  avail=$(avail_mb)
  printf '%s %3d → %s | %5d ms | RAM: %5d MB\n' \
    "$([[ "$MODE" == fleet ]] && echo dispositivo || echo fork)" "$N" "$vm" "$LAST_MS" "$avail"

  if [[ "$avail" -lt "$RAM_FLOOR_MB" ]]; then
    STOP="suelo de RAM (${avail} MB < ${RAM_FLOOR_MB} MB)"; break
  fi
  if [[ "$LAST_MS" -gt $((SLOW_SEC * 1000)) ]]; then
    STOP="despliegue $N tardó ${LAST_MS} ms (> ${SLOW_SEC}s): dejar de desplegar aquí"; break
  fi
done

# --- Verificación final: la flota entera sigue viva? Muestreo 1º/medio/último
ALIVE="n/a"
if [[ "$N" -gt 0 && "$MODE" == "fleet" ]]; then
  ALIVE="sí"
  mid=$(( (N + 1) / 2 ))
  for vm in $(awk -v m="$mid" 'NR==1||NR==m{print $1}' "$CREATED_FILE") "$LAST_VM"; do
    agent_ok "$vm" 5 || ALIVE="NO ($vm no responde)"
  done
elif [[ "$N" -gt 0 ]]; then
  ALIVE="sí"
  for vm in "$(awk 'NR==1{print $1}' "$CREATED_FILE")" "$LAST_VM"; do
    agent_ok "$vm" 5 || ALIVE="NO ($vm no responde)"
  done
fi

PSS="(ejecuta como root para medir PSS)"
if [[ "$N" -gt 0 ]]; then
  pid=$(curl -s "http://$API/v1/vms/$LAST_VM" | jq -r '.pid // empty')
  [[ -n "$pid" && -r "/proc/$pid/smaps_rollup" ]] \
    && PSS="$(awk '/^Pss:/{print $2 " kB"}' "/proc/$pid/smaps_rollup")"
fi

RAM_END=$(avail_mb)
HEALTH=$(curl -s "http://$API/v1/health" | jq -c '.')

echo ""
echo "================== RESULTADO ($MODE) =================="
echo "Desplegados:            $N"
echo "Motivo de parada:       $STOP"
echo "Vivos (muestra):        $ALIVE"
echo "Despliegue 1º vs último: ${FIRST_MS} ms → ${LAST_MS} ms"
echo "PSS última VM:          $PSS"
echo "RAM: ${RAM_START} MB → ${RAM_END} MB"
[[ "$N" -gt 0 ]] && echo "Coste medio por unidad:  $(( (RAM_START - RAM_END) / N )) MB"
echo "Health: $HEALTH"
echo "======================================================="
