# Sesión 0 — Arranque manual, sin código

**Estado**: pendiente  
**Objetivo**: levantar una microVM con `curl` sin escribir código.

---

## Checklist

- [ ] `make check` — verificar entorno (KVM, cgroup2, Go)
- [ ] `make install-fc` — instalar firecracker y jailer
- [ ] `make kernel` — descargar vmlinux mínimo
- [ ] Descargar rootfs de ejemplo de AWS
- [ ] Crear socket y lanzar `firecracker` directo (sin Jailer)
- [ ] Secuencia de `curl` para arrancar la VM
- [ ] Conectar por consola serie
- [ ] Apagar limpio

---

## Secuencia de arranque manual (referencia)

```bash
# 1. Lanzar firecracker en background apuntando al socket
rm -f /tmp/fc.socket
./firecracker --api-sock /tmp/fc.socket &

# 2. Configurar kernel
curl -s -X PUT 'http://localhost/boot-source' \
  --unix-socket /tmp/fc.socket \
  -H 'Content-Type: application/json' \
  -d '{
    "kernel_image_path": "/path/to/vmlinux",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off"
  }'

# 3. Configurar disco
curl -s -X PUT 'http://localhost/drives/rootfs' \
  --unix-socket /tmp/fc.socket \
  -H 'Content-Type: application/json' \
  -d '{
    "drive_id": "rootfs",
    "path_on_host": "/path/to/rootfs.ext4",
    "is_root_device": true,
    "is_read_only": false
  }'

# 4. Configurar máquina
curl -s -X PUT 'http://localhost/machine-config' \
  --unix-socket /tmp/fc.socket \
  -H 'Content-Type: application/json' \
  -d '{"vcpu_count": 1, "mem_size_mib": 128}'

# 5. Arrancar
curl -s -X PUT 'http://localhost/actions' \
  --unix-socket /tmp/fc.socket \
  -H 'Content-Type: application/json' \
  -d '{"action_type": "InstanceStart"}'
```

---

## Notas / problemas encontrados

_(rellenar durante la sesión)_
