# Despliegue como servicio systemd

El daemon **no debe correrse en primer plano** en una terminal interactiva:
un Ctrl-C manda SIGINT a todo el grupo de procesos de la terminal, lo que mata
también los procesos Firecracker hijos y tira las microVMs. Como servicio
systemd no hay terminal de control, y la unit está configurada para que las
VMs sobrevivan a los reinicios del daemon.

## Instalación en un paso (recomendada)

```bash
make full-install       # host (cgroups/nftables/store CoW) + firecracker/jailer
                        # + compila + servicio systemd + verificación de salud
make prepare-image      # kernel + rootfs (debootstrap) + vsock/SSH/DNS
                        # + golden al store + alta en el catálogo
```

Con eso el sistema queda funcionando de cero: tras `prepare-image` ya se puede
crear la primera VM (`POST /v1/vms {"template":"base-ubuntu-noble"}`).

Funciona en **x86_64 y aarch64** (Raspberry Pi 4/5 de 64 bits, Jetson,
gateways ARM): la arquitectura se autodetecta y todos los pasos la respetan
(binarios de Firecracker, kernel del bucket de CI, mirror de Ubuntu — arm64
vive en `ports.ubuntu.com`, no en `archive.ubuntu.com`). Variables útiles:

```bash
make full-install FC_VERSION=v1.16.1 ADDR=127.0.0.1:9000
make prepare-image IMAGE_NAME=sensor-base IMAGE_SIZE_MB=512
```

`full-install` debe ejecutarse **en la máquina objetivo** (KVM, cgroups y el
store son locales) y es idempotente: reejecutarlo actualiza binario/servicio
sin tocar las VMs vivas. La versión de Firecracker va **fijada** en el
Makefile (`v1.16.1`, la validada en hardware): así toda instalación es
reproducible. Actualizarla es una decisión consciente
(`make full-install FC_VERSION=vX.Y.Z`) — los snapshots existentes van
ligados a la versión que los creó y habrá que recrearlos.

Para preparar piezas de ARM desde el PC x86 (útil antes de tener la Pi a mano):

```bash
make build ARCH=aarch64          # cross-compila solo el binario (Go puro, estático)
make prepare-image ARCH=aarch64  # imagen arm64 vía qemu-user-static; los ficheros
                                 # se copian al store de la Pi a mano (no se dan
                                 # de alta en el catálogo local)
```

## Instalar (paso a paso, si prefieres control fino)

```bash
make setup-host                      # cgroups, nftables, store CoW
make install-fc                      # firecracker + jailer (misma versión)
make install-service                 # compila (como tu usuario) e instala la unit
sudo systemctl enable --now microhosted
systemctl status microhosted
```

Dirección de escucha distinta:

```bash
make install-service ADDR=127.0.0.1:9000
```

## Operar

```bash
journalctl -u microhosted -f         # logs en vivo
sudo systemctl restart microhosted   # reinicia el daemon SIN matar las VMs vivas
sudo systemctl stop microhosted      # para el daemon; las VMs siguen corriendo
```

Tras un `restart` o un arranque, en el log verás `reconcile: adopted running
vm <id>` por cada VM que seguía viva, o `reconcile: swept dead vm <id>` por las
que hubieran muerto mientras el daemon estaba parado.

## Por qué la unit es como es

- **`KillMode=process`** — systemd solo señala al proceso principal (el
  orquestador), nunca a los Firecracker hijos. Es lo que permite que
  `Manager.Reconcile` readopte las VMs por PID al volver a arrancar.
- **`Delegate=yes`** — Jailer crea un cgroup por VM; delegar el subárbol de
  cgroup al servicio evita chocar con la gestión de systemd.
- **`AssertPathExists=/dev/kvm`** — sin KVM el servicio falla claro al arrancar,
  no más tarde con un error oscuro.
- Corre como **root**: Jailer necesita crear chroots/cgroups/tap y abrir
  `/dev/kvm`.

## Almacenamiento (store copy-on-write)

`scripts/setup-host.sh` provisiona el **store de discos** antes de instalar el
servicio. Si `/var/lib/microhosted/store` no está ya sobre un filesystem con reflink,
monta ahí un **loopback btrfs** (`/var/lib/microhosted/instances.btrfs`,
persistido en `/etc/fstab`) y crea `rootfs/`, `kernels/` y `jailer/` dentro. Así
los clones de VM son copy-on-write (cada VM cuesta sus deltas, no el rootfs
entero) y funciona en cualquier host Ubuntu sin reparticionar.

Requisito: goldens, kernels y el chroot del Jailer viven todos en ese store — el
daemon deriva `--chroot-base` de `--instances-dir` justamente para respetarlo
(reflink y hardlink no cruzan filesystems). Al arrancar, si el store no es CoW el
daemon lo avisa en el log. El store vive FUERA del repo a propósito: son datos de
runtime propiedad de root (incluidos los jail dirs), y tenerlos en el árbol de
código rompe herramientas como `go build ./...`. Detalle en `docs/layers.md`
(L3) y `docs/architecture.md`.

### Mover el store (migración)

`setup-host.sh` es idempotente: si el btrfs del store ya está montado en otro
punto (p.ej. una instalación vieja en `<repo>/images/instances`), lo detecta con
`losetup -j`/`findmnt`, lo desmonta, limpia su línea de `/etc/fstab` y lo remonta
en `/var/lib/microhosted/store` — sin copiar datos (es el mismo subvolumen, los
goldens/kernels vienen solos).

**Gotcha**: `KillMode=process` hace que las VMs **sobrevivan** al `systemctl
stop`, así que un `umount` del store dará `target is busy` mientras queden
procesos `firecracker` vivos. Antes de migrar hay que apagarlos:

```bash
curl -s -X DELETE localhost:8080/v1/vms   # destruye las VMs (con el daemon vivo)
sudo systemctl stop microhosted
sudo pkill -9 -f '/firecracker --id'      # mata cualquier VM huérfana que sobreviva
sudo ./scripts/setup-host.sh              # remonta el store en el nuevo path
make install-service && sudo systemctl start microhosted
```

## Desinstalar

```bash
make uninstall-service               # para y quita el servicio (no toca las VMs)
```
