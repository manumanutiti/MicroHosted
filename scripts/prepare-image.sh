#!/usr/bin/env bash
# Prepara una plantilla (rootfs dorado) para usarse con la plataforma. Deja
# listas las dos vías de acceso a las VMs clonadas de esa imagen — cuál usar
# en cada caso es cosa del operador, no de este script:
#
#   - vsock (POST /v1/vms/{id}/exec): programático, no necesita red ni claves,
#     funciona incluso en VMs creadas con no_network:true.
#   - SSH: shell interactiva de verdad, necesita que la VM tenga red.
#
# Es el ÚNICO paso de preparación de imagen — se corre una vez por rootfs
# dorado (internal/storage.CloneRootfs copia lo que haya en él a cada VM), no
# una vez por VM.
#
# Uso: sudo ./scripts/prepare-image.sh <rootfs.ext4> [--no-ssh] [--no-vsock] [clave-publica.pub]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KEY_DIR="${SCRIPT_DIR}/../images/keys"
DEFAULT_PRIVATE_KEY="${KEY_DIR}/microhosted_ed25519"
DEFAULT_PUBKEY="${DEFAULT_PRIVATE_KEY}.pub"
AGENT_PORT=52

ROOTFS=""
PUBKEY="$DEFAULT_PUBKEY"
DO_SSH=1
DO_VSOCK=1

for arg in "$@"; do
  case "$arg" in
    --no-ssh) DO_SSH=0 ;;
    --no-vsock) DO_VSOCK=0 ;;
    *)
      if [[ -z "$ROOTFS" ]]; then
        ROOTFS="$arg"
      else
        PUBKEY="$arg"
      fi
      ;;
  esac
done

if [[ -z "$ROOTFS" ]]; then
  echo "uso: sudo ./scripts/prepare-image.sh <rootfs.ext4> [--no-ssh] [--no-vsock] [clave-publica.pub]"
  exit 1
fi

if [[ ! -f "$ROOTFS" ]]; then
  echo "ERROR: no existe $ROOTFS"
  exit 1
fi

MOUNT_DIR="$(mktemp -d)"
cleanup() {
  sudo umount "$MOUNT_DIR" 2>/dev/null || true
  rmdir "$MOUNT_DIR" 2>/dev/null || true
}
trap cleanup EXIT

echo "==> Montando ${ROOTFS}..."
sudo mount -o loop "$ROOTFS" "$MOUNT_DIR"

if [[ "$DO_VSOCK" -eq 1 ]]; then
  if [[ ! -x "$MOUNT_DIR/usr/bin/socat" ]]; then
    echo "ERROR: esta imagen no tiene socat instalado (necesario para el canal vsock)."
    echo "       instálalo dentro de la imagen o vuelve a correr con --no-vsock."
    exit 1
  fi

  echo "==> Instalando el listener vsock (microhosted-exec, puerto ${AGENT_PORT})..."
  sudo tee "$MOUNT_DIR/usr/local/bin/microhosted-exec" >/dev/null <<'AGENT'
#!/bin/sh
read -r cmd
sh -c "$cmd" 2>&1
echo "___MICROHOSTED_EXIT___:$?"
AGENT
  sudo chmod 0755 "$MOUNT_DIR/usr/local/bin/microhosted-exec"

  sudo tee "$MOUNT_DIR/etc/systemd/system/microhosted-exec.service" >/dev/null <<UNIT
[Unit]
Description=MicroHosted vsock exec listener

[Service]
ExecStart=/usr/bin/socat VSOCK-LISTEN:${AGENT_PORT},fork,reuseaddr EXEC:/usr/local/bin/microhosted-exec
Restart=always

[Install]
WantedBy=multi-user.target
UNIT

  sudo systemctl --root="$MOUNT_DIR" enable microhosted-exec.service
  echo "    OK: acceso programático (vsock) listo."
fi

if [[ "$DO_SSH" -eq 1 ]]; then
  if [[ "$PUBKEY" == "$DEFAULT_PUBKEY" && ! -f "$DEFAULT_PUBKEY" ]]; then
    echo "==> No hay todavía una clave del proyecto, generando ${DEFAULT_PRIVATE_KEY}..."
    mkdir -p "$KEY_DIR"
    ssh-keygen -t ed25519 -f "$DEFAULT_PRIVATE_KEY" -N '' -C "microhosted (generada localmente, no commitear)"
  fi

  if [[ ! -f "$PUBKEY" ]]; then
    echo "ERROR: no existe $PUBKEY"
    exit 1
  fi

  echo "==> Añadiendo ${PUBKEY} a /root/.ssh/authorized_keys..."
  sudo mkdir -p "$MOUNT_DIR/root/.ssh"
  sudo bash -c "cat '$PUBKEY' >> '$MOUNT_DIR/root/.ssh/authorized_keys'"
  sudo chmod 700 "$MOUNT_DIR/root/.ssh"
  sudo chmod 600 "$MOUNT_DIR/root/.ssh/authorized_keys"
  sudo chown -R 0:0 "$MOUNT_DIR/root/.ssh"
  echo "    OK: acceso interactivo (SSH) listo."
fi

echo ""
echo "OK: ${ROOTFS} preparado."
echo "    Crea una VM NUEVA (las que ya existían se clonaron antes de este cambio) y:"
if [[ "$DO_VSOCK" -eq 1 ]]; then
  echo "    - ejecuta comandos con: curl -X POST localhost:8080/v1/vms/<id>/exec -d '{\"cmd\":\"...\"}'"
fi
if [[ "$DO_SSH" -eq 1 ]]; then
  echo "    - entra por shell con: ssh -i ${DEFAULT_PRIVATE_KEY} root@<guest_ip>"
fi
