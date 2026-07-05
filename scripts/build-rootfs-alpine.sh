#!/usr/bin/env bash
# Genera un rootfs.ext4 ULTRA-MÍNIMO para microVMs usando Alpine minirootfs
# (~3 MB descargados, ~10 MB instalados vs ~1 GiB del Ubuntu de
# build-rootfs.sh). Sin systemd: busybox init + inittab. El objetivo es
# densidad — el working set sucio de un guest ocioso baja de ~100 MB
# (systemd) a ~10 MB, que es lo que decide cuántas microVMs caben en la Pi.
#
# La imagen sale LISTA para usar: incluye lo que prepare-image.sh añade a las
# imágenes Ubuntu (listener vsock puerto 52, resolv.conf → /proc/net/pnp), así
# que NO hace falta pasarle prepare-image.sh (que además asume systemd).
#
# Variantes por uso ("añadirle un python"): --add instala paquetes apk extra
# en el golden. Cada variante es un golden distinto; los clones por VM siguen
# siendo reflinks CoW, así que 50 VMs de la variante python comparten el disco.
#
# Uso: sudo ./scripts/build-rootfs-alpine.sh [OUTPUT] [SIZE_MB] [--add pkg1,pkg2] [--ssh clave.pub]
#      sudo ./scripts/build-rootfs-alpine.sh images/rootfs/alpine.ext4
#      sudo ./scripts/build-rootfs-alpine.sh images/rootfs/alpine-py.ext4 512 --add python3
#
# Igual que los goldens Ubuntu: ext4 sobre el fichero entero, sin tabla de
# particiones (CloneRootfs los agranda por VM con resize2fs offline).

set -euo pipefail

OUTPUT="images/rootfs/alpine.ext4"
SIZE_MB=256
EXTRA_PKGS=""
SSH_PUBKEY=""
AGENT_PORT=52
ALPINE_BRANCH="${ALPINE_BRANCH:-v3.22}"
ALPINE_VER="${ALPINE_VER:-3.22.0}"

positional=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --add) EXTRA_PKGS="${2//,/ }"; shift 2 ;;
    --ssh) SSH_PUBKEY="$2"; shift 2 ;;
    *)
      case "$positional" in
        0) OUTPUT="$1" ;;
        1) SIZE_MB="$1" ;;
        *) echo "ERROR: argumento inesperado: $1" >&2; exit 1 ;;
      esac
      positional=$((positional + 1)); shift ;;
  esac
done

ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
  *) echo "ERROR: arquitectura no soportada: $ARCH (usa x86_64 o aarch64)" >&2; exit 1 ;;
esac
if [[ "$ARCH" != "$(uname -m)" ]]; then
  echo "ERROR: build cross-arch no soportado aquí (el apk del chroot es de otra CPU)." >&2
  echo "       Constrúyelo en un host $ARCH, o usa build-rootfs.sh (debootstrap+qemu)." >&2
  exit 1
fi

TARBALL="alpine-minirootfs-${ALPINE_VER}-${ARCH}.tar.gz"
URL="${MINIROOTFS_URL:-https://dl-cdn.alpinelinux.org/alpine/${ALPINE_BRANCH}/releases/${ARCH}/${TARBALL}}"
CACHE_DIR="images/cache"
MOUNT_DIR="$(mktemp -d /tmp/mh-alpine-XXXXXX)"

cleanup() {
  sudo umount "$MOUNT_DIR" 2>/dev/null || true
  rmdir "$MOUNT_DIR" 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p "$(dirname "$OUTPUT")" "$CACHE_DIR"

if [[ ! -f "$CACHE_DIR/$TARBALL" ]]; then
  echo "==> Descargando $TARBALL..."
  curl -fL -o "$CACHE_DIR/$TARBALL.tmp" "$URL"
  mv "$CACHE_DIR/$TARBALL.tmp" "$CACHE_DIR/$TARBALL"
fi

echo "==> Creando ext4 de ${SIZE_MB} MB en $OUTPUT..."
rm -f "$OUTPUT"
truncate -s "${SIZE_MB}M" "$OUTPUT"
mkfs.ext4 -F -q -L microhosted-alpine "$OUTPUT"
sudo mount -o loop "$OUTPUT" "$MOUNT_DIR"

echo "==> Extrayendo minirootfs..."
sudo tar -xzf "$CACHE_DIR/$TARBALL" -C "$MOUNT_DIR"

# apk necesita red dentro del chroot: resolv.conf del host, solo durante el
# build (al final se reemplaza por el symlink a /proc/net/pnp).
sudo cp /etc/resolv.conf "$MOUNT_DIR/etc/resolv.conf"

echo "==> Instalando paquetes (socat${EXTRA_PKGS:+ $EXTRA_PKGS}${SSH_PUBKEY:+ openssh})..."
sudo chroot "$MOUNT_DIR" /sbin/apk add --no-cache socat ${EXTRA_PKGS} \
  ${SSH_PUBKEY:+openssh}

# El listener vsock: MISMO agente y contrato que instala prepare-image.sh en
# las imágenes Ubuntu (multiplexa exec/PUT/GET sobre la primera línea). POSIX
# sh puro — corre igual en busybox ash.
echo "==> Instalando el listener vsock (microhosted-exec, puerto ${AGENT_PORT})..."
sudo tee "$MOUNT_DIR/usr/local/bin/microhosted-exec" >/dev/null <<'AGENT'
#!/bin/sh
IFS= read -r line
verb=${line%% *}
case "$verb" in
  PUT)
    rest=${line#PUT }
    path=${rest% *}
    len=${rest##* }
    mkdir -p "$(dirname "$path")" 2>/dev/null
    head -c "$len" > "$path"
    echo "___MICROHOSTED_EXIT___:$?"
    ;;
  GET)
    path=${line#GET }
    if [ -f "$path" ]; then
      len=$(wc -c < "$path")
      printf 'OK %s\n' "$len"
      cat "$path"
    else
      printf 'ERR no such file: %s\n' "$path"
    fi
    ;;
  *)
    sh -c "$line" 2>&1
    echo "___MICROHOSTED_EXIT___:$?"
    ;;
esac
AGENT
sudo chmod 0755 "$MOUNT_DIR/usr/local/bin/microhosted-exec"

# Init: busybox directamente, sin OpenRC — un microVM no necesita gestor de
# servicios. sysinit monta los pseudo-fs (idempotente: si el kernel ya montó
# devtmpfs, el mount falla y init sigue), respawn mantiene vivo el agente.
# ctrlaltdel→reboot es cómo Firecracker apaga el guest (SendCtrlAltDel; en
# microVM el reboot termina el proceso, no reinicia).
echo "==> Configurando busybox init (inittab)..."
sudo tee "$MOUNT_DIR/etc/inittab" >/dev/null <<INITTAB
::sysinit:/bin/mount -t proc proc /proc
::sysinit:/bin/mount -t sysfs sysfs /sys
::sysinit:/bin/mount -t devtmpfs devtmpfs /dev
::sysinit:/bin/mkdir -p /dev/pts
::sysinit:/bin/mount -t devpts devpts /dev/pts
::respawn:/usr/bin/socat VSOCK-LISTEN:${AGENT_PORT},fork,reuseaddr EXEC:/usr/local/bin/microhosted-exec
ttyS0::respawn:/sbin/getty -L 115200 ttyS0 vt100
::ctrlaltdel:/sbin/reboot
::shutdown:/bin/umount -a -r
INITTAB

if [[ -n "$SSH_PUBKEY" ]]; then
  echo "==> Configurando SSH (clave $SSH_PUBKEY)..."
  sudo chroot "$MOUNT_DIR" /usr/bin/ssh-keygen -A
  sudo mkdir -p "$MOUNT_DIR/root/.ssh"
  sudo cp "$SSH_PUBKEY" "$MOUNT_DIR/root/.ssh/authorized_keys"
  sudo chmod 700 "$MOUNT_DIR/root/.ssh"
  sudo chmod 600 "$MOUNT_DIR/root/.ssh/authorized_keys"
  echo "::respawn:/usr/sbin/sshd -D -e" | sudo tee -a "$MOUNT_DIR/etc/inittab" >/dev/null
fi

# DNS: mismo mecanismo que prepare-image.sh — el kernel escribe los
# nameservers que asigna microhosted en /proc/net/pnp (vía ip= del SDK).
sudo ln -sf /proc/net/pnp "$MOUNT_DIR/etc/resolv.conf"

sudo umount "$MOUNT_DIR"

USED_MB=$(du -m "$OUTPUT" | cut -f1)
echo ""
echo "OK: $OUTPUT (${SIZE_MB} MB, ~${USED_MB} MB reales)."
echo "    Lista para usar como golden — NO necesita prepare-image.sh."
echo "    Acceso: vsock exec (puerto ${AGENT_PORT})${SSH_PUBKEY:+ + SSH root}."
