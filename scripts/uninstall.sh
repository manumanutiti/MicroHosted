#!/usr/bin/env bash
# Desinstalación COMPLETA de MicroHosted en esta máquina — el inverso de
# full-install.sh. Devuelve el sistema a su estado original:
#
#   1. mata las VMs vivas (KillMode=process las deja corriendo al parar el
#      servicio, así que hay que matarlas explícitamente)
#   2. para, deshabilita y borra el servicio systemd
#   3. borra la tabla nftables 'inet microhosted' (única tabla nuestra;
#      no toca las tablas del host ni las de Docker)
#   4. borra los bridges mhbr* y sus taps (solo los nuestros: el prefijo
#      mhbr es propio; docker0/br-* quedan intactos)
#   5. borra los cgroups del daemon (/sys/fs/cgroup/microhosted y el
#      /sys/fs/cgroup/firecracker del Jailer)
#   6. desmonta el store CoW, quita su línea de /etc/fstab y borra el
#      fichero-imagen btrfs y /var/lib/microhosted entero
#   7. borra los binarios instalados (microhosted; firecracker y jailer
#      salvo KEEP_FC=1) y la DB de estado del repo
#
# Idempotente: se puede reejecutar sobre una instalación parcial sin fallar.
#
# Uso:  sudo ./scripts/uninstall.sh
#       DRY_RUN=1  solo muestra lo que haría, sin tocar nada
#       KEEP_FC=1  conserva firecracker/jailer en /usr/local/bin
#       PURGE=1    borra también los artefactos de imagen del repo
#                  (images/kernels, rootfs, instances, keys)
#
# NO se revierte (se avisa al final): paquetes apt (nftables, btrfs-progs,
# iproute2), net.ipv4.ip_forward, permisos de /dev/kvm y grupo kvm.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTANCES_DIR="${INSTANCES_DIR:-/var/lib/microhosted/store}"
STATE_ROOT="$(dirname "$INSTANCES_DIR")"                 # /var/lib/microhosted
COW_IMG="${COW_IMG:-$STATE_ROOT/instances.btrfs}"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"
UNIT="/etc/systemd/system/microhosted.service"

if [[ $EUID -ne 0 ]]; then
  echo "ERROR: ejecutar con sudo o como root." >&2
  exit 1
fi

DRY_RUN="${DRY_RUN:-}"
run() {
  if [[ -n "$DRY_RUN" ]]; then
    echo "  [dry-run] $*"
  else
    "$@"
  fi
}

echo "=============================================="
echo " MicroHosted — desinstalación completa"
[[ -n "$DRY_RUN" ]] && echo " (DRY_RUN: no se toca nada)" || true
echo "=============================================="

# --- [1/7] VMs vivas ----------------------------------------------------------
# Parar el servicio ANTES de matar las VMs: si no, Restart=on-failure +
# reconcile podrían readoptarlas o relanzar el daemon a mitad de limpieza.
echo ""
echo "==> [1/7] Parando servicio y matando VMs vivas..."
if systemctl list-unit-files microhosted.service &>/dev/null && \
   systemctl is-active --quiet microhosted 2>/dev/null; then
  run systemctl stop microhosted
fi

# Las VMs son procesos firecracker cuyo root (chroot del Jailer) vive dentro
# del store — esa comprobación evita matar un firecracker ajeno. Los jailer
# aún sin exec también se incluyen (son efímeros y siempre nuestros).
VM_PIDS=()
for pid in $(pgrep -x firecracker 2>/dev/null || true); do
  root="$(readlink "/proc/$pid/root" 2>/dev/null || true)"
  if [[ "$root" == "$INSTANCES_DIR"/* ]]; then VM_PIDS+=("$pid"); fi
done
for pid in $(pgrep -x jailer 2>/dev/null || true); do
  VM_PIDS+=("$pid")
done

if [[ ${#VM_PIDS[@]} -gt 0 ]]; then
  echo "  matando ${#VM_PIDS[@]} VM(s): ${VM_PIDS[*]}"
  run kill -TERM "${VM_PIDS[@]}" 2>/dev/null || true
  if [[ -z "$DRY_RUN" ]]; then
    for _ in $(seq 1 10); do
      alive=0
      for pid in "${VM_PIDS[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then alive=1; fi
      done
      if [[ "$alive" -eq 0 ]]; then break; fi
      sleep 1
    done
    for pid in "${VM_PIDS[@]}"; do kill -KILL "$pid" 2>/dev/null || true; done
  fi
else
  echo "  no hay VMs vivas"
fi

# --- [2/7] Servicio systemd -----------------------------------------------------
echo ""
echo "==> [2/7] Eliminando el servicio systemd..."
if [[ -f "$UNIT" ]]; then
  run systemctl disable --now microhosted 2>/dev/null || true
  run rm -f "$UNIT"
  run systemctl daemon-reload
  run systemctl reset-failed microhosted 2>/dev/null || true
  echo "  $UNIT: eliminado"
else
  echo "  no estaba instalado"
fi

# --- [3/7] nftables --------------------------------------------------------------
echo ""
echo "==> [3/7] Eliminando la tabla nftables 'inet microhosted'..."
if command -v nft &>/dev/null && nft list table inet microhosted &>/dev/null; then
  run nft delete table inet microhosted
  echo "  tabla eliminada"
else
  echo "  no existía"
fi

# --- [4/7] Bridges y taps ---------------------------------------------------------
echo ""
echo "==> [4/7] Eliminando bridges mhbr* y sus taps..."
BRIDGES="$(ip -o link show type bridge 2>/dev/null | awk -F': ' '{print $2}' | grep '^mhbr' || true)"
if [[ -n "$BRIDGES" ]]; then
  for br in $BRIDGES; do
    # Los taps son persistentes (sobreviven al proceso): borrarlos explícitamente.
    for tap in $(ip -o link show master "$br" 2>/dev/null | awk -F': ' '{print $2}' | cut -d@ -f1); do
      run ip link del "$tap" 2>/dev/null || true
    done
    run ip link del "$br"
    echo "  $br: eliminado"
  done
else
  echo "  no hay bridges mhbr*"
fi

# --- [5/7] cgroups -----------------------------------------------------------------
echo ""
echo "==> [5/7] Eliminando cgroups..."
for cgparent in /sys/fs/cgroup/microhosted /sys/fs/cgroup/firecracker; do
  if [[ -d "$cgparent" ]]; then
    for d in "$cgparent"/*/; do
      if [[ -d "$d" ]]; then run rmdir "$d" 2>/dev/null || true; fi
    done
    run rmdir "$cgparent" 2>/dev/null || true
    echo "  $cgparent: eliminado"
  fi
done

# --- [6/7] Store CoW ---------------------------------------------------------------
echo ""
echo "==> [6/7] Desmontando y borrando el store CoW..."
if mountpoint -q "$INSTANCES_DIR" 2>/dev/null; then
  UMOUNT_OK=0
  for _ in $(seq 1 5); do
    if run umount "$INSTANCES_DIR" 2>/dev/null; then UMOUNT_OK=1; break; fi
    sleep 1
  done
  if [[ -n "$DRY_RUN" ]]; then UMOUNT_OK=1; fi
  if [[ "$UMOUNT_OK" -ne 1 ]]; then
    echo "ERROR: no se pudo desmontar $INSTANCES_DIR (target is busy)." >&2
    echo "       Mira qué lo usa:  sudo lsof +f -- $INSTANCES_DIR" >&2
    echo "       y reejecuta el uninstall." >&2
    exit 1
  fi
  echo "  $INSTANCES_DIR: desmontado"
fi

# Quitar la línea de fstab del loopback (por imagen, cubre también montajes
# históricos en otro punto) y refrescar las unidades .mount generadas.
if grep -qF "$COW_IMG" /etc/fstab 2>/dev/null; then
  run sed -i "\#${COW_IMG}#d" /etc/fstab
  run systemctl daemon-reload
  echo "  línea de /etc/fstab: eliminada"
fi

# Si quedó algún loop device colgando de la imagen, soltarlo antes de borrarla.
for loopdev in $(losetup -j "$COW_IMG" 2>/dev/null | cut -d: -f1); do
  run losetup -d "$loopdev" 2>/dev/null || true
done

if [[ -d "$STATE_ROOT" ]]; then
  # Doble seguro: jamás borrar el árbol con algo aún montado dentro. (findmnt -R
  # no vale: solo mira si el target MISMO es mountpoint, no sus hijos.)
  if [[ -z "$DRY_RUN" ]] && findmnt -rn -o TARGET | grep -Eq "^${STATE_ROOT}(/|\$)"; then
    echo "ERROR: sigue habiendo un filesystem montado bajo $STATE_ROOT — no borro nada." >&2
    exit 1
  fi
  run rm -rf "$STATE_ROOT"
  echo "  $STATE_ROOT: eliminado (imagen btrfs incluida)"
else
  echo "  $STATE_ROOT no existía"
fi

# --- [7/7] Binarios y estado del repo ------------------------------------------------
echo ""
echo "==> [7/7] Eliminando binarios y DB de estado..."
run rm -f "$BIN_DIR/microhosted"
echo "  $BIN_DIR/microhosted: eliminado"
if [[ -n "${KEEP_FC:-}" ]]; then
  echo "  firecracker/jailer: conservados (KEEP_FC=1)"
else
  run rm -f "$BIN_DIR/firecracker" "$BIN_DIR/jailer"
  echo "  $BIN_DIR/{firecracker,jailer}: eliminados"
fi

# La DB apunta a VMs/clones del store recién borrado: dejarla solo envenena
# una futura reinstalación con estado fantasma.
run rm -f "$REPO_ROOT/images/microhosted.db"
echo "  images/microhosted.db: eliminada"

if [[ -n "${PURGE:-}" ]]; then
  run rm -rf "$REPO_ROOT/images/kernels" "$REPO_ROOT/images/rootfs" \
             "$REPO_ROOT/images/instances" "$REPO_ROOT/images/keys"
  echo "  PURGE: images/{kernels,rootfs,instances,keys} eliminados"
fi

echo ""
echo "=============================================="
echo " Desinstalación completada"
echo "=============================================="
echo "  Queda en el sistema (revertir a mano si se quiere):"
echo "    - paquetes apt: nftables, btrfs-progs, iproute2"
echo "    - net.ipv4.ip_forward=1 (si otras cargas no lo usan:"
echo "      sudo sysctl -w net.ipv4.ip_forward=0)"
echo "    - permisos de /dev/kvm y pertenencia al grupo kvm"
if [[ -z "${PURGE:-}" ]]; then
  echo "  En el repo quedan los artefactos de imagen (images/); PURGE=1 los borra."
fi
echo "  El catálogo (images/catalog.json) está versionado: git checkout lo restaura."
