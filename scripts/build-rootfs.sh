#!/usr/bin/env bash
# Genera un rootfs.ext4 mínimo para microVMs usando debootstrap.
# Uso: ./scripts/build-rootfs.sh [OUTPUT] [SIZE_MB]
# Ejemplo: ./scripts/build-rootfs.sh images/rootfs/ubuntu-noble.ext4 500

set -euo pipefail

OUTPUT="${1:-images/rootfs/base.ext4}"
SIZE_MB="${2:-500}"
DISTRO="noble"   # Ubuntu 24.04

if ! command -v debootstrap &>/dev/null; then
  echo "==> Instalando debootstrap..."
  sudo apt-get install -y debootstrap
fi

TMP_DIR="$(mktemp -d)"
trap 'sudo rm -rf "$TMP_DIR"' EXIT

echo "==> Creando rootfs de ${SIZE_MB}MB en ${OUTPUT}..."
dd if=/dev/zero of="$OUTPUT" bs=1M count="$SIZE_MB" status=progress
mkfs.ext4 -F "$OUTPUT"

echo "==> Montando imagen..."
MOUNT_DIR="${TMP_DIR}/rootfs"
mkdir -p "$MOUNT_DIR"
sudo mount -o loop "$OUTPUT" "$MOUNT_DIR"

echo "==> Ejecutando debootstrap (${DISTRO})..."
sudo debootstrap --arch=amd64 "$DISTRO" "$MOUNT_DIR" http://archive.ubuntu.com/ubuntu

echo "==> Configurando guest mínimo..."
sudo chroot "$MOUNT_DIR" /bin/bash -euo pipefail <<'CHROOT'
  # Contraseña root vacía para consola serie
  passwd -d root

  # Hostname
  echo "microvm" > /etc/hostname

  # /etc/fstab mínimo
  echo "LABEL=rootfs / ext4 defaults,noatime 0 1" > /etc/fstab

  # Deshabilitar servicios innecesarios
  systemctl disable --now snapd.service 2>/dev/null || true
  systemctl disable --now apt-daily.service 2>/dev/null || true
CHROOT

echo "==> Desmontando..."
sudo umount "$MOUNT_DIR"
sudo e2label "$OUTPUT" rootfs

echo ""
echo "OK: rootfs generado en ${OUTPUT} (${SIZE_MB}MB)"
echo "    Usar con: --rootfs ${OUTPUT}"
