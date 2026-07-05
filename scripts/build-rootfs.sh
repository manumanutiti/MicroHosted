#!/usr/bin/env bash
# Genera un rootfs.ext4 mínimo para microVMs usando debootstrap.
# Soporta x86_64 y aarch64 (nativo o cross con qemu-user-static).
#
# Uso: sudo ./scripts/build-rootfs.sh [OUTPUT] [SIZE_MB]
#      ARCH=aarch64 sudo -E ./scripts/build-rootfs.sh images/rootfs/pi.ext4 1024
#
# Incluye SIEMPRE socat (lo exige el listener vsock que instala
# prepare-image.sh) y openssh-server (la vía de acceso interactiva).

set -euo pipefail

OUTPUT="${1:-images/rootfs/base.ext4}"
SIZE_MB="${2:-1024}"
DISTRO="${DISTRO:-noble}"   # Ubuntu 24.04
INCLUDE="socat,openssh-server"

ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
  *) echo "ERROR: arquitectura no soportada: $ARCH (usa x86_64 o aarch64)" >&2; exit 1 ;;
esac

# Arquitectura Debian + mirror. OJO: archive.ubuntu.com solo sirve amd64/i386;
# arm64 vive en ports.ubuntu.com — con el mirror equivocado debootstrap falla
# con un críptico "no packages found".
if [[ "$ARCH" == "x86_64" ]]; then
  DEBARCH=amd64
  MIRROR="${MIRROR:-http://archive.ubuntu.com/ubuntu}"
else
  DEBARCH=arm64
  MIRROR="${MIRROR:-http://ports.ubuntu.com/ubuntu-ports}"
fi

case "$(uname -m)" in
  x86_64) NATIVE_DEBARCH=amd64 ;;
  aarch64) NATIVE_DEBARCH=arm64 ;;
  *) NATIVE_DEBARCH="$(uname -m)" ;;
esac
CROSS=0
[[ "$DEBARCH" != "$NATIVE_DEBARCH" ]] && CROSS=1

if ! command -v debootstrap &>/dev/null; then
  echo "==> Instalando debootstrap..."
  sudo apt-get install -y debootstrap
fi

# Cross-arch: los binarios del rootfs (postinst de los .deb, el chroot de
# configuración) son de otra CPU — hace falta qemu-user-static + binfmt para
# que el kernel los ejecute de forma transparente.
QEMU_BIN=""
if [[ "$CROSS" -eq 1 ]]; then
  case "$DEBARCH" in
    arm64) QEMU_BIN=/usr/bin/qemu-aarch64-static ;;
    amd64) QEMU_BIN=/usr/bin/qemu-x86_64-static ;;
  esac
  if [[ ! -x "$QEMU_BIN" ]]; then
    echo "==> Instalando qemu-user-static (cross ${NATIVE_DEBARCH} → ${DEBARCH})..."
    sudo apt-get install -y qemu-user-static binfmt-support
  fi
fi

TMP_DIR="$(mktemp -d)"
MOUNT_DIR="${TMP_DIR}/rootfs"
cleanup() {
  sudo umount "$MOUNT_DIR" 2>/dev/null || true
  sudo rm -rf "$TMP_DIR"
}
trap cleanup EXIT

echo "==> Creando rootfs ${DEBARCH} de ${SIZE_MB}MB en ${OUTPUT}..."
mkdir -p "$(dirname "$OUTPUT")"
truncate -s "${SIZE_MB}M" "$OUTPUT"   # sparse: solo ocupa lo que se escriba
mkfs.ext4 -q -F "$OUTPUT"

echo "==> Montando imagen..."
mkdir -p "$MOUNT_DIR"
sudo mount -o loop "$OUTPUT" "$MOUNT_DIR"

if [[ "$CROSS" -eq 0 ]]; then
  echo "==> debootstrap ${DISTRO}/${DEBARCH} (nativo)..."
  sudo debootstrap --arch="$DEBARCH" --include="$INCLUDE" \
    "$DISTRO" "$MOUNT_DIR" "$MIRROR"
else
  echo "==> debootstrap ${DISTRO}/${DEBARCH} (cross, dos etapas con qemu)..."
  sudo debootstrap --foreign --arch="$DEBARCH" --include="$INCLUDE" \
    "$DISTRO" "$MOUNT_DIR" "$MIRROR"
  sudo cp "$QEMU_BIN" "$MOUNT_DIR/usr/bin/"
  sudo chroot "$MOUNT_DIR" /debootstrap/debootstrap --second-stage
fi

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

# SSH activo al primer arranque (enlace de unit offline; no ejecuta nada del guest)
sudo systemctl --root="$MOUNT_DIR" enable ssh.service 2>/dev/null || true

if [[ "$CROSS" -eq 1 ]]; then
  sudo rm -f "$MOUNT_DIR/usr/bin/$(basename "$QEMU_BIN")"
fi

echo "==> Desmontando..."
sudo umount "$MOUNT_DIR"
sudo e2label "$OUTPUT" rootfs

echo ""
echo "OK: rootfs ${DEBARCH} generado en ${OUTPUT} (${SIZE_MB}MB)"
echo "    Siguiente paso: sudo ./scripts/prepare-image.sh ${OUTPUT}"
