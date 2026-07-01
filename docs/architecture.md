# Arquitectura — MicroHosted

## Flujo de una microVM (etapa 1)

```
CLI / API
    │
    ▼
vm.Manager
    │
    ├─► jailer.Runner      # crea el chroot, lanza jailer → firecracker
    │       └─► /srv/jailer/firecracker/<id>/root/
    │
    ├─► firecracker.Client # habla con el socket Unix dentro del chroot
    │       PUT /boot-source
    │       PUT /drives/rootfs
    │       PUT /machine-config
    │       PUT /network-interfaces/eth0
    │       PUT /actions { InstanceStart }
    │
    └─► network.Manager    # crea TAP, configura masquerade
            ip tuntap add tapN mode tap
            ip addr add hostIP/30 dev tapN
            ip link set tapN up
```

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

## Jailer: layout de directorios

```
/srv/jailer/
└── firecracker/
    └── <vm-id>/
        └── root/
            ├── firecracker          # hard link al binario
            ├── firecracker.socket   # socket de la API REST
            ├── vmlinux              # kernel (hard link o bind mount)
            └── rootfs.ext4          # disco raíz (hard link o bind mount)
```

Jailer hace chroot a `root/` antes de ejecutar Firecracker. El proceso resultante
no puede ver nada fuera de ese directorio.

## Decisiones de diseño

| Decisión | Alternativa descartada | Razón |
|---|---|---|
| Go + firecracker-go-sdk | Python / Rust desde cero | SDK oficial mantenido, evita escribir el cliente HTTP |
| SQLite primero | Postgres desde el inicio | Un solo host no necesita Postgres; migrar cuando llegue multi-host |
| IP estática (etapa 1) | CNI / IPAM | CNI añade complejidad sin aportar nada en un solo host |
| vsock para host↔guest | SSH para todo | vsock no necesita IP configurada, latencia menor, patrón de la industria |
