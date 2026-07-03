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
└── <id>.ext4                 # clon de la VM (reflink del golden, agrandado)
```

Jailer hace chroot a `root/` antes de ejecutar Firecracker; el proceso resultante
no ve nada fuera de ese directorio. El `--chroot-base` del daemon **deriva de
`--instances-dir`** (`<instances>/jailer`) precisamente para garantizar el mismo
FS. Ver `docs/layers.md` (L3) para el detalle.

## Decisiones de diseño

| Decisión | Alternativa descartada | Razón |
|---|---|---|
| Go + firecracker-go-sdk | Python / Rust desde cero | SDK oficial mantenido, evita escribir el cliente HTTP |
| SQLite primero | Postgres desde el inicio | Un solo host no necesita Postgres; migrar cuando llegue multi-host |
| IP estática (etapa 1) | CNI / IPAM | CNI añade complejidad sin aportar nada en un solo host |
| vsock para host↔guest | SSH para todo | vsock no necesita IP configurada, latencia menor, patrón de la industria |
| Store btrfs CoW (loopback) | copia completa en ext4 | clones = deltas, no rootfs enteros; loopback = agnóstico al host, sin reparticionar |
| Golden/kernel/chroot en el store | rutas dispersas (/srv/jailer, images/kernels) | reflink y hardlink no cruzan FS; todo en un FS o el clon/arranque fallan |
