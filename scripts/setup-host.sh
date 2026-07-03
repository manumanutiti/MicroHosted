#!/usr/bin/env bash
# Configura el host para ejecutar microVMs con Firecracker + Jailer.
# Verifica requisitos, ajusta permisos y prepara cgroups v2.
# Ejecutar con sudo o como root.

set -euo pipefail

echo "==> Verificando kernel..."
KERNEL_VERSION="$(uname -r)"
KERNEL_MAJOR="$(echo "$KERNEL_VERSION" | cut -d. -f1)"
KERNEL_MINOR="$(echo "$KERNEL_VERSION" | cut -d. -f2)"
if [[ "$KERNEL_MAJOR" -lt 5 || ("$KERNEL_MAJOR" -eq 5 && "$KERNEL_MINOR" -lt 10) ]]; then
  echo "WARN: kernel ${KERNEL_VERSION} — se recomienda 5.10+. Puede haber problemas."
else
  echo "  kernel ${KERNEL_VERSION}: OK"
fi

echo "==> Verificando KVM..."
if [[ ! -e /dev/kvm ]]; then
  echo "ERROR: /dev/kvm no existe. Habilita virtualización en BIOS o activa nested virt."
  exit 1
fi
if [[ ! -r /dev/kvm || ! -w /dev/kvm ]]; then
  echo "  Ajustando permisos en /dev/kvm para el grupo kvm..."
  sudo chown root:kvm /dev/kvm
  sudo chmod 0660 /dev/kvm
  sudo usermod -aG kvm "$USER"
  echo "  AVISO: sesión a reiniciar o ejecutar: newgrp kvm"
else
  echo "  /dev/kvm: OK"
fi

echo "==> Verificando cgroups v2..."
if ! mount | grep -q "type cgroup2"; then
  echo "  Montando cgroup2 en /sys/fs/cgroup..."
  sudo mount -t cgroup2 none /sys/fs/cgroup || true
fi
if mount | grep -q "type cgroup2"; then
  echo "  cgroup2: OK"
else
  echo "WARN: cgroup2 no montado. Jailer puede fallar."
fi

echo "==> Verificando iproute2 (ip tuntap)..."
if ! command -v ip &>/dev/null; then
  echo "  Instalando iproute2..."
  sudo apt-get install -y iproute2
fi
echo "  iproute2: OK"

echo "==> Verificando nftables (nft)..."
if ! command -v nft &>/dev/null; then
  echo "  Instalando nftables..."
  sudo apt-get install -y nftables
fi
# El daemon lo necesita al arrancar: aplica la política de red segmentada
# (drop guest→host, aislamiento entre redes, NAT de egress) en la tabla propia
# 'inet microhosted'. Sin nft, la reconciliación de redes falla al arrancar.
echo "  nftables: OK"

# El directorio de trabajo del Jailer (el chroot) ya NO va en /srv/jailer: tiene
# que estar en el MISMO filesystem que los clones de rootfs, porque Jailer
# hardlinka el rootfs/kernel dentro del chroot y un hardlink no cruza
# dispositivos. Con el store CoW eso es el btrfs de <instances>/. Por eso el
# chroot (y el kernel) viven dentro del store — se crean más abajo, tras montarlo.

# ---------------------------------------------------------------------------
# Store de instancias con copy-on-write (CoW).
#
# Los clones por VM se hacen con `cp --reflink=auto`: en un FS con reflink
# (btrfs, XFS con reflink) son CoW instantáneos — el rootfs dorado se guarda
# UNA vez y cada VM solo cuesta lo que escribe. En ext4 normal `cp` cae a copia
# COMPLETA: cada VM = una copia entera del rootfs, y con imágenes de 1GB+ el
# disco se llena en un instante.
#
# Para que funcione en CUALQUIER host Ubuntu sin reparticionar, si el directorio
# de instancias no está ya sobre un FS con reflink, montamos ahí un loopback
# btrfs (un fichero-imagen sparse). btrfs va en el kernel de todo Ubuntu; solo
# hace falta btrfs-progs para formatearlo.
#
# IMPORTANTE: reflink NO cruza filesystems. Para que el clon sea CoW, el rootfs
# dorado tiene que vivir en el MISMO filesystem que los clones — por eso los
# goldens van en <store>/rootfs/, no en un images/rootfs aparte sobre el ext4 del
# host. Si el golden está en otro FS, `cp` cae a copia completa aunque el store
# sea btrfs. El catálogo apunta a <store>/rootfs.
#
# El store vive FUERA del repo (por defecto /var/lib/microhosted/store): son
# datos de runtime propiedad de root (incluido el chroot del Jailer), no fuentes
# — tenerlos dentro del árbol de código rompe herramientas como `go build ./...`.
# ---------------------------------------------------------------------------
echo "==> Verificando store de discos (copy-on-write)..."
INSTANCES_DIR="${INSTANCES_DIR:-/var/lib/microhosted/store}"
COW_IMG="${COW_IMG:-/var/lib/microhosted/instances.btrfs}"
COW_SIZE_GB="${COW_SIZE_GB:-20}"

# Migración: si el btrfs del store ya está montado en OTRO punto (p.ej. el
# histórico images/instances dentro del repo), desmontarlo y quitar su línea de
# fstab para remontarlo en INSTANCES_DIR. El servicio debe estar parado y sin VMs
# vivas (si no, umount da "target is busy").
LOOPDEV="$(sudo losetup -j "$COW_IMG" 2>/dev/null | cut -d: -f1 | head -1 || true)"
if [[ -n "$LOOPDEV" ]]; then
  OLD_MNT="$(findmnt -n -o TARGET --source "$LOOPDEV" 2>/dev/null | head -1 || true)"
  if [[ -n "$OLD_MNT" && "$OLD_MNT" != "$INSTANCES_DIR" ]]; then
    echo "  migrando store de $OLD_MNT a $INSTANCES_DIR..."
    if ! sudo umount "$OLD_MNT"; then
      echo "  ERROR: no se pudo desmontar $OLD_MNT — para el servicio y destruye las VMs primero:" >&2
      echo "         sudo systemctl stop microhosted" >&2
      exit 1
    fi
    sudo sed -i "\#[[:space:]]${OLD_MNT}[[:space:]]#d" /etc/fstab
  fi
fi

sudo mkdir -p "$INSTANCES_DIR"

# ¿el directorio ya soporta reflink? (prueba real: depende del FS concreto)
probe_reflink() {
  local dir="$1" src dst
  src="$(sudo mktemp "$dir/.reflink-probe.XXXXXX")" || return 1
  dst="${src}.clone"
  sudo dd if=/dev/zero of="$src" bs=4k count=1 status=none 2>/dev/null || true
  if sudo cp --reflink=always "$src" "$dst" 2>/dev/null; then
    sudo rm -f "$src" "$dst"; return 0
  fi
  sudo rm -f "$src" "$dst"; return 1
}

if probe_reflink "$INSTANCES_DIR"; then
  echo "  $INSTANCES_DIR ya soporta CoW (reflink): OK"
elif mountpoint -q "$INSTANCES_DIR"; then
  echo "  WARN: $INSTANCES_DIR está montado pero sin reflink; cámbialo a btrfs/XFS-reflink"
  echo "        o los clones serán copias completas."
else
  echo "  $INSTANCES_DIR no soporta CoW; provisionando loopback btrfs (${COW_SIZE_GB}GB)..."
  if ! command -v mkfs.btrfs &>/dev/null; then
    echo "  Instalando btrfs-progs..."
    sudo apt-get install -y btrfs-progs
  fi
  sudo mkdir -p "$(dirname "$COW_IMG")"
  if [[ ! -f "$COW_IMG" ]]; then
    sudo truncate -s "${COW_SIZE_GB}G" "$COW_IMG"   # sparse: no ocupa GB hasta usarse
    sudo mkfs.btrfs -q "$COW_IMG"
  fi
  sudo mount -o loop,compress=zstd "$COW_IMG" "$INSTANCES_DIR"
  # Persistir para que el mount sobreviva a reboots.
  FSTAB_LINE="$COW_IMG $INSTANCES_DIR btrfs loop,compress=zstd 0 0"
  if ! grep -qF "$COW_IMG" /etc/fstab; then
    echo "$FSTAB_LINE" | sudo tee -a /etc/fstab >/dev/null
  fi
  echo "  loopback btrfs montado en $INSTANCES_DIR: OK"
fi

# Todo lo que Jailer hardlinka o clona tiene que compartir filesystem con los
# clones, así que goldens, kernels y el chroot del Jailer viven DENTRO del store:
#   <instances>/rootfs   -> rootfs dorados (fuente del reflink CoW)
#   <instances>/kernels  -> kernels (Jailer los hardlinka al chroot)
#   <instances>/jailer   -> base del chroot del Jailer (destino del hardlink)
# El catálogo apunta a rootfs/ y kernels/; el daemon usa <instances>/jailer como
# chroot-base por defecto. Meter los rootfs/kernels dorados es cosa de la
# preparación de imágenes.
sudo mkdir -p "$INSTANCES_DIR/rootfs" "$INSTANCES_DIR/kernels" "$INSTANCES_DIR/jailer"
sudo chown root:root "$INSTANCES_DIR/jailer"
sudo chmod 0755 "$INSTANCES_DIR/jailer"
echo "  store con rootfs/ kernels/ jailer/ (mismo FS: reflink CoW + hardlinks OK)"

echo ""
echo "Host configurado. Siguiente paso: scripts/install-fc.sh"
