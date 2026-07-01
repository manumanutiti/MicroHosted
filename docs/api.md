# API HTTP — referencia

Servida por `internal/api` (`cmd/microhosted`, flag `--addr`, por defecto
`:8080`). Todos los cuerpos son JSON. No hay autenticación todavía — pensada
para correr detrás de un panel/backend propio, no expuesta directamente.

Estado: cubre create/read/delete + exec. El CRUD completo (incluyendo
"update" y matices de create/delete) está en marcha — ver `SESSIONS.md`,
sección "Próxima sesión".

---

## Plantillas (catálogo)

### `GET /v1/templates`

Lista las plantillas ("micromáquinas") disponibles para clonar.

**Respuesta 200** — array de `Template`:

```json
[
  {
    "name": "base-ubuntu",
    "description": "Ubuntu 22.04 (rootfs oficial de Firecracker CI)",
    "kernel_path": "images/kernels/vmlinux-6.1.102",
    "rootfs_path": "images/rootfs/ubuntu-22.04.ext4",
    "vcpus": 1,
    "mem_mb": 128
  }
]
```

Fuente: `images/catalog.json`, cargado una vez al arrancar el daemon
(`storage.LoadCatalog`).

---

## VMs

### `POST /v1/vms` — crear

**Body** (`CreateVMRequest`):

| campo         | tipo   | requerido | descripción                                                                 |
|---------------|--------|-----------|------------------------------------------------------------------------------|
| `template`    | string | sí        | nombre de una plantilla del catálogo                                        |
| `vcpus`       | int    | no        | overridea el `vcpus` de la plantilla                                        |
| `mem_mb`      | int    | no        | overridea el `mem_mb` de la plantilla                                       |
| `no_network`  | bool   | no        | si `true`, no crea TAP/IP — la VM solo es accesible por vsock (`/exec`)     |

```bash
curl -X POST localhost:8080/v1/vms -d '{"template":"base-ubuntu"}'
curl -X POST localhost:8080/v1/vms -d '{"template":"base-ubuntu","no_network":true}'
```

**Respuesta 201** (`VMResponse`, ver más abajo) · **400** si falta `template`
o el JSON es inválido · **500** si falla el clonado/red/arranque (el mensaje
de error incluye en qué paso falló).

Cada `POST` clona el rootfs de la plantilla desde cero
(`internal/storage.CloneRootfs`, copy-on-write vía `cp --reflink=auto`) — hoy
no hay forma de re-arrancar sobre un disco ya modificado de una VM anterior;
eso es parte del trabajo pendiente (ver `SESSIONS.md`).

---

### `GET /v1/vms` — listar

```bash
curl localhost:8080/v1/vms
```

**Respuesta 200** — array de `VMResponse`. Pensado como base del futuro
"`docker ps` de microVMs" (ver `SESSIONS.md`) — hoy es una lista plana, sin
filtros ni columnas de estado más allá de `state`.

---

### `GET /v1/vms/{id}` — detalle

```bash
curl localhost:8080/v1/vms/a1b2c3d4
```

**Respuesta 200** (`VMResponse`) · **404** si no existe.

**Forma de `VMResponse`:**

| campo         | descripción                                                                 |
|---------------|------------------------------------------------------------------------------|
| `id`          | ID corto (8 hex) de la VM                                                    |
| `template`    | nombre de la plantilla de la que se clonó                                   |
| `state`       | `creating`\|`running`\|`paused`\|`stopped`\|`failed` (hoy solo se usa `running`) |
| `pid`         | PID del proceso Firecracker (jailed)                                        |
| `guest_ip`    | IP del guest (vacío si `no_network`)                                        |
| `host_ip`     | IP del host en el enlace punto a punto (vacío si `no_network`)              |
| `tap_device`  | nombre del TAP (vacío si `no_network`)                                      |
| `log_path`    | archivo con la consola serie + logs de Jailer/Firecracker de esta VM        |
| `created_at`  | timestamp RFC3339                                                            |

Nota: `VMResponse` no incluye hoy `socket_path` ni `vsock_path` (existen en el
tipo interno `types.VM` pero no se serializan) — pendiente decidir si exponerlos.

---

### `DELETE /v1/vms/{id}` — destruir

```bash
curl -X DELETE localhost:8080/v1/vms/a1b2c3d4
```

**Respuesta 204** · **404** si no existe o si falla al parar/limpiar (el
detalle del error queda en el body).

Para a la máquina (`firecracker.Stop`: ACPI graceful + SIGTERM de respaldo),
borra el TAP device, libera el bloque de IP, y borra el clon del rootfs
(`images/instances/<id>.ext4` y `.log`). Pendiente de auditar que no queden
huérfanos en todos los casos de fallo parcial — ver `SESSIONS.md`.

---

### `POST /v1/vms/{id}/exec` — ejecutar un comando (vsock)

**Body** (`ExecRequest`):

| campo | tipo   | requerido | descripción                          |
|-------|--------|-----------|----------------------------------------|
| `cmd` | string | sí        | comando a ejecutar con `sh -c` en el guest |

```bash
curl -X POST localhost:8080/v1/vms/a1b2c3d4/exec -d '{"cmd":"whoami && uname -a"}'
```

**Respuesta 200** (`ExecResponse`):

```json
{"output": "root\nLinux ubuntu-fc-uvm 6.1.102 ...\n", "exit_code": 0}
```

`output` es stdout+stderr combinados. Funciona con o sin red (`no_network`
no afecta a este endpoint) — requiere que la plantilla se haya preparado con
`scripts/prepare-image.sh` (instala el listener `socat`+vsock en el rootfs
dorado). Si la plantilla no está preparada, o si la clonaste antes de
prepararla, da **500** con un error indicando que falta el marcador de salida
del agente.

**400** si falta `cmd` · **404**/**500** si la VM no existe o el vsock no
responde.

---

## Lo que falta (ver `SESSIONS.md` → "Próxima sesión")

- `PUT`/`PATCH` para "actualizar" una VM: subir un archivo/playbook a su
  disco, o parametrizar con qué contenido nace.
- Poder crear a partir de un disco ya usado (no solo de la plantilla dorada).
- `GET /v1/vms` con más forma de "`docker ps`" (uptime, columnas pensadas
  para un panel web).
- Auditoría de que `DELETE` no deja huérfanos en ningún camino de fallo.
