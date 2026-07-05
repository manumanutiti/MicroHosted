#!/usr/bin/env bash
# Pipeline COMPLETO de imagen, de cero a plantilla usable:
#
#   1. kernel  — descarga el vmlinux de Firecracker CI para la arquitectura
#   2. rootfs  — debootstrap Ubuntu (arch correcta; cross con qemu-user-static)
#   3. prepara — vsock exec listener + SSH + DNS (prepare-image.sh)
#   4. store   — instala golden + kernel en el store CoW (mismo FS: reflink)
#   5. catálogo — alta/actualización de la plantilla en images/catalog.json
#
# Entrada normal: `make prepare-image` (acepta ARCH=, IMAGE_NAME=, SIZE_MB=,
# KERNEL_VERSION=). Requiere el host ya configurado (make full-install o
# make setup-host): el store debe existir.
#
# Cross-arch (p.ej. construir la imagen aarch64 en el PC x86 para llevarla a
# una Pi): funciona vía qemu-user-static, pero el kernel/rootfs resultantes
# hay que copiarlos al store de la máquina destino a mano.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  amd64|x86|x86_64)  ARCH=x86_64 ;;
  arm|arm64|aarch64) ARCH=aarch64 ;;
  *) echo "ERROR: arquitectura no soportada: $ARCH (usa x86_64 o aarch64)" >&2; exit 1 ;;
esac

IMAGE_NAME="${IMAGE_NAME:-base-ubuntu-noble}"
SIZE_MB="${SIZE_MB:-1024}"
KERNEL_VERSION="${KERNEL_VERSION:-6.1.102}"
STORE="${INSTANCES_DIR:-/var/lib/microhosted/store}"
CATALOG="${CATALOG:-images/catalog.json}"
VCPUS="${VCPUS:-1}"
MEM_MB="${MEM_MB:-128}"
DISK_MB="${DISK_MB:-1024}"

if [[ ! -d "$STORE/rootfs" || ! -d "$STORE/kernels" ]]; then
  echo "ERROR: el store $STORE no está preparado (faltan rootfs/ y kernels/)." >&2
  echo "       Corre primero: make full-install   (o make setup-host)" >&2
  exit 1
fi

echo "=============================================="
echo " Imagen '${IMAGE_NAME}' (${ARCH}, ${SIZE_MB}MB)"
echo "=============================================="
sudo -v

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# --- [1/5] Kernel -------------------------------------------------------------
echo ""
echo "==> [1/5] Kernel vmlinux-${KERNEL_VERSION} (${ARCH})..."
KERNEL_DST="$STORE/kernels/vmlinux-${KERNEL_VERSION}"
if [[ "$ARCH" != "$(uname -m)" ]]; then
  # cross: no contaminar el store local con un kernel de otra arquitectura
  KERNEL_DST="$STORE/kernels/vmlinux-${KERNEL_VERSION}-${ARCH}"
fi
if sudo test -f "$KERNEL_DST"; then
  echo "  ya existe: $KERNEL_DST"
else
  ARCH="$ARCH" ./scripts/build-kernel.sh "$KERNEL_VERSION" "$TMP_DIR/kernels"
  sudo install -o root -g root -m 0644 \
    "$TMP_DIR/kernels/vmlinux-${KERNEL_VERSION}" "$KERNEL_DST"
  echo "  instalado: $KERNEL_DST"
fi

# --- [2/5] Rootfs (debootstrap) ------------------------------------------------
echo ""
echo "==> [2/5] Rootfs Ubuntu (debootstrap, ${ARCH})..."
ROOTFS_TMP="$TMP_DIR/${IMAGE_NAME}.ext4"
sudo ARCH="$ARCH" ./scripts/build-rootfs.sh "$ROOTFS_TMP" "$SIZE_MB"

# --- [3/5] Preparación (vsock + SSH + DNS) --------------------------------------
echo ""
echo "==> [3/5] Preparando la imagen (vsock exec + SSH + DNS)..."
sudo ./scripts/prepare-image.sh "$ROOTFS_TMP"

# --- [4/5] Al store CoW ----------------------------------------------------------
echo ""
echo "==> [4/5] Instalando el golden en el store..."
ROOTFS_DST="$STORE/rootfs/${IMAGE_NAME}.ext4"
sudo install -o root -g root -m 0644 "$ROOTFS_TMP" "$ROOTFS_DST"
echo "  instalado: $ROOTFS_DST"

# --- [5/5] Catálogo ---------------------------------------------------------------
echo ""
echo "==> [5/5] Actualizando el catálogo (${CATALOG})..."
if [[ "$ARCH" != "$(uname -m)" ]]; then
  echo "  AVISO: imagen cross-arch (${ARCH}) — NO se da de alta en el catálogo local."
  echo "  Copia estos ficheros al store de la máquina ${ARCH} y añade la entrada allí:"
  echo "    $KERNEL_DST"
  echo "    $ROOTFS_DST"
else
  python3 - "$CATALOG" "$IMAGE_NAME" "$KERNEL_DST" "$ROOTFS_DST" \
             "$VCPUS" "$MEM_MB" "$DISK_MB" <<'PY'
import json, os, sys

path, name, kernel, rootfs = sys.argv[1:5]
vcpus, mem_mb, disk_mb = (int(x) for x in sys.argv[5:8])

catalog = []
if os.path.exists(path):
    with open(path) as f:
        catalog = json.load(f)

entry = {
    "name": name,
    "description": f"Ubuntu noble (debootstrap, make prepare-image)",
    "kernel_path": kernel,
    "rootfs_path": rootfs,
    "vcpus": vcpus,
    "mem_mb": mem_mb,
    "disk_mb": disk_mb,
}
catalog = [t for t in catalog if t.get("name") != name]
catalog.append(entry)

with open(path, "w") as f:
    json.dump(catalog, f, indent=2, ensure_ascii=False)
    f.write("\n")
print(f"  plantilla '{name}' registrada (vcpus={vcpus}, mem={mem_mb}MB, disk={disk_mb}MB)")
PY

  # El daemon lee el catálogo al arrancar: reiniciarlo para que vea la
  # plantilla nueva (las VMs vivas sobreviven: KillMode=process + reconcile).
  if systemctl is-active --quiet microhosted 2>/dev/null; then
    echo "  reiniciando el daemon para recargar el catálogo..."
    sudo systemctl restart microhosted
  fi
fi

echo ""
echo "=============================================="
echo " Imagen lista"
echo "=============================================="
if [[ "$ARCH" == "$(uname -m)" ]]; then
  echo "  Probar:"
  echo "    curl -s -X POST localhost:8080/v1/vms -d '{\"template\":\"${IMAGE_NAME}\"}'"
  echo "    curl -s -X POST localhost:8080/v1/vms/<id>/exec -d '{\"cmd\":\"uname -a\"}'"
fi
