# Arquitectura — MicroHosted

## Flujo de una microVM (etapa 1)

```
CLI / API
    │
    ▼
vm.Manager
    │
    ├─► storage.CloneRootfs # clona el golden (reflink CoW) y lo agranda a disk_mb
    │       └─► /var/lib/microhosted/store/<id>.ext4
    │
    ├─► jailer.Runner      # crea el chroot, lanza jailer → firecracker
    │       └─► /var/lib/microhosted/store/jailer/firecracker/<id>/root/
    │
    ├─► firecracker.Client # habla con el socket Unix dentro del chroot
    │       PUT /boot-source
    │       PUT /drives/rootfs
    │       PUT /machine-config
    │       PUT /network-interfaces/eth0
    │       PUT /actions { InstanceStart }
    │
    └─► network.Manager    # red segmentada: bridge por red + TAP enslavado
            ip tuntap add tapN mode tap        # TAP sin IP
            ip link set tapN master mhbr<red>  # enslavado al bridge de la red
            ip link set tapN up
            # IP del guest la fija Firecracker (estática, vía kernel); nftables
            # aísla: drop guest→host, drop cross-segment, egress NAT opcional.
```

(El modelo `/30` punto-a-punto original, con el host como gateway de cada VM, se
retiró en la Fase 1 — ver `docs/networking.md`.)

## Comunicación host ↔ guest

- **Red**: TAP device → IP estática → SSH/HTTP normal
- **Comandos/resultados**: vsock (virtio-vsock)
  - Guest expone un servidor en `VSOCK_PORT`
  - Host conecta al CID de la VM
  - No usa red IP — funciona aunque no haya red configurada

## Estado de una VM

```
creating → running → paused → running
                  └──────────────────→ stopped
```

## Almacenamiento: store copy-on-write

Todos los discos de VM viven en un **store btrfs con copy-on-write** montado en
`/var/lib/microhosted/store` (un loopback btrfs que `setup-host.sh` provisiona en cualquier
host, sin reparticionar). Clonar un golden es un reflink instantáneo: cada VM
comparte los bloques del golden y solo cuesta lo que escribe (medido: ~236 KiB
por clon de 1GB, no 300MB).

`storage.CloneRootfs` clona el golden y lo agranda al `disk_mb` del template con
`resize2fs` (offline: los goldens son ext4 sobre el dispositivo entero, sin tabla
de particiones, así que basta `truncate` + `resize2fs`), para que el guest tenga
espacio libre — un golden va casi lleno y sin esto un `apt install` se queda sin
disco.

**Invariante clave**: todo lo que el daemon clona o hardlinka comparte este mismo
filesystem, porque ni el reflink (`cp --reflink`) ni el hardlink cruzan
filesystems en Linux. Por eso goldens, kernels y el chroot del Jailer viven todos
bajo el store:

```
/var/lib/microhosted/store/            (btrfs, CoW)
├── rootfs/                   # goldens (fuente del reflink)
├── kernels/                  # kernels (Jailer los hardlinka al chroot)
├── jailer/                   # base del chroot del Jailer (chroot-base derivado)
│   └── firecracker/<vm-id>/root/
│       ├── firecracker          # hard link al binario
│       ├── firecracker.socket   # socket de la API REST
│       ├── vmlinux              # hard link al kernel del store
│       └── <id>.ext4            # hard link al clon (MISMO inodo → Stop conserva disco)
├── snapshots/                # snapshots (memoria+estado+disco congelados)
│   └── <snap-id>/
│       ├── vmstate              # estado de dispositivos/vCPUs (Firecracker)
│       ├── mem                  # memoria del guest (los restores la mapean CoW)
│       └── disk.ext4            # reflink del disco tomado con la VM pausada
└── <id>.ext4                 # clon de la VM (reflink del golden, agrandado)
```

Jailer hace chroot a `root/` antes de ejecutar Firecracker; el proceso resultante
no ve nada fuera de ese directorio. El `--chroot-base` del daemon **deriva de
`--instances-dir`** (`<instances>/jailer`) precisamente para garantizar el mismo
FS. Ver `docs/layers.md` (L3) para el detalle.

## Snapshots y bifurcación

**Crear** (`vm.Manager.Snapshot`): pausa la VM (`PATCH /vm`), le pide a
Firecracker un snapshot Full (`PUT /snapshot/create` — el proceso está
chrooteado, así que escribe vmstate+mem dentro de su propio chroot), el daemon
los mueve con `os.Rename` (mismo FS ⇒ gratis) a `snapshots/<sid>/`, reflinka el
disco **aún en pausa** (memoria y disco quedan mutuamente consistentes) y
reanuda. Estas llamadas van directas al socket UDS (`internal/firecracker/rawapi.go`),
no por el SDK: así funcionan igual sobre VMs adoptadas tras un reinicio del
daemon (sin handle del SDK) y dan acceso a campos que el SDK v1.0.0 no conoce.

**Restaurar/bifurcar** (`vm.Manager.Fork` / `Restore`): se lanza un Firecracker
nuevo vía Jailer **sin boot** — se sustituye el pipeline de arranque del SDK por
StartVMM + un handler propio (`internal/firecracker.LaunchFromSnapshot`) que
hardlinka vmstate/mem/disco al chroot recién creado y hace `PUT /snapshot/load`
con `resume_vm`. El mem se mapea copy-on-write: N VMs pueden restaurar del
mismo snapshot a la vez sin copiarlo.

**TAP y versión de Firecracker**: el vmstate recuerda el nombre del TAP
original, y `network_overrides` (el campo de `/snapshot/load` que permite
remapear la NIC a otro TAP) **solo existe desde Firecracker v1.12.0**. El
daemon sondea la versión del binario al arrancar
(`firecracker.SupportsNetworkOverrides`) y adapta la estrategia:
- **Restore in-place**: recrea el TAP con su nombre original → nunca necesita
  override → funciona en cualquier versión.
- **Fork en FC ≥1.12**: TAP propio (`tap<nuevo-id>`) + override. Sin
  restricciones.
- **Fork en FC <1.12**: reutiliza el nombre de TAP original del snapshot si
  está libre (original destruida/parada); si está ocupado → 409 explicando que
  los forks simultáneos requieren actualizar Firecracker. Ojo al actualizar:
  el formato de snapshot va ligado a la versión de FC — los snapshots
  existentes hay que recrearlos.

Medido en hardware (2026-07-04, host FC 1.10.1): snapshot 271ms, restore
in-place 109ms, fork 113ms (endpoint completo, VM de 128MB).

**Identidad congelada**: la IP/MAC del guest viven en la memoria snapshoteada y
no se pueden cambiar al restaurar. Por eso el fork tiene dos modos: unirse a la
red de origen reclamando la IP exacta del snapshot (reserva estricta,
`ClaimVM` — 409 si está ocupada), o `quarantine` (TAP sin bridge: el guest cree
tener red, todo muere en el host, acceso solo por vsock — N forks simultáneos).

## Decisiones de diseño

| Decisión | Alternativa descartada | Razón |
|---|---|---|
| Go + firecracker-go-sdk | Python / Rust desde cero | SDK oficial mantenido, evita escribir el cliente HTTP |
| SQLite primero | Postgres desde el inicio | Un solo host no necesita Postgres; migrar cuando llegue multi-host |
| IP estática (etapa 1) | CNI / IPAM | CNI añade complejidad sin aportar nada en un solo host |
| vsock para host↔guest | SSH para todo | vsock no necesita IP configurada, latencia menor, patrón de la industria |
| Store btrfs CoW (loopback) | copia completa en ext4 | clones = deltas, no rootfs enteros; loopback = agnóstico al host, sin reparticionar |
| Golden/kernel/chroot en el store | rutas dispersas (/srv/jailer, images/kernels) | reflink y hardlink no cruzan FS; todo en un FS o el clon/arranque fallan |
