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

echo "==> Creando directorio de trabajo de Jailer..."
sudo mkdir -p /srv/jailer
sudo chown root:root /srv/jailer
sudo chmod 0755 /srv/jailer
echo "  /srv/jailer: OK"

echo ""
echo "Host configurado. Siguiente paso: scripts/install-fc.sh"
