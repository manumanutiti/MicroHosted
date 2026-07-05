# API HTTP — referencia

Servida por `internal/api` (`cmd/microhosted`, flag `--addr`, por defecto
`:8080`). Todos los cuerpos son JSON. No hay autenticación todavía — pensada
para correr detrás de un panel/backend propio, no expuesta directamente.

Estado: cubre create/read/delete + stop/start + exec. El CRUD completo (incluyendo
"update" y matices de create/delete) está en marcha — ver `SESSIONS.md`,
sección "Próxima sesión".

**Convención de rutas** (toda la API sigue esta regla):

- **Recursos = sustantivos con CRUD puro**: `POST/GET /v1/<recurso>` y
  `GET/DELETE /v1/<recurso>/{id}` — `vms`, `snapshots`, `networks`, `templates`.
- **Acciones = verbos**: `POST /v1/<recurso>/{id}/<verbo>` — `stop`, `start`,
  `snapshot`, `fork`, `restore`, `exec`. El mismo verbo significa lo mismo
  cuelgue de donde cuelgue (p. ej. `fork` existe en `/vms/{id}` y en
  `/snapshots/{id}` con el mismo body y la misma respuesta; solo cambia el
  origen). Lo que una acción crea vive después en su colección: `snapshot`
  crea en `/v1/snapshots`, `fork` crea en `/v1/vms`.

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

## Redes

Segmentos L2 con nombre (bridge + subred + política nftables). Detalle del
modelo en `docs/networking.md`. Al arrancar existe siempre una red `default`
(`172.16.0.0/24`, sin salida a internet).

### `POST /v1/networks` — crear

**Body** (`CreateNetworkRequest`):

| campo    | tipo   | requerido | descripción                                                        |
|----------|--------|-----------|---------------------------------------------------------------------|
| `name`   | string | sí        | nombre único de la red                                             |
| `subnet` | string | no        | CIDR (p.ej. `10.10.0.0/24`); si se omite, se asigna un `/24` libre |
| `egress` | bool   | no        | si `true`, la subred sale a internet vía NAT; `false` por defecto  |

```bash
curl -X POST localhost:8080/v1/networks -d '{"name":"lab"}'
curl -X POST localhost:8080/v1/networks -d '{"name":"build","egress":true}'
```

**Respuesta 201** (`NetworkResponse`) · **400** si falta `name` · **500** si el
nombre ya existe, la subred es inválida o falla la creación del bridge.

### `GET /v1/networks` · `GET /v1/networks/{name}`

Lista todas las redes / detalle de una. **Forma de `NetworkResponse`:**

| campo        | descripción                                             |
|--------------|----------------------------------------------------------|
| `name`       | nombre de la red                                        |
| `bridge`     | bridge Linux que la respalda (`mhbr<id>`)               |
| `subnet`     | CIDR de la red                                          |
| `gateway`    | IP del host en el bridge (la `.1`, ruta de los guests)  |
| `egress`     | si tiene salida a internet                              |
| `created_at` | timestamp RFC3339                                       |

### `DELETE /v1/networks/{name}`

```bash
curl -X DELETE localhost:8080/v1/networks/lab
```

**Respuesta 204** · **404** si no existe · **409** si aún tiene VMs conectadas
(destrúyelas primero, con el endpoint de abajo).

### `DELETE /v1/networks/{name}/vms` — borrar todas las VMs de una red

Paso previo típico a `DELETE /v1/networks/{name}` cuando aún tiene VMs.

```bash
curl -X DELETE localhost:8080/v1/networks/lab/vms
```

**Respuesta 200** (`BulkDeleteResponse`, ver forma más abajo) · **404** si la
red no existe.

---

## VMs

### `POST /v1/vms` — crear

**Body** (`CreateVMRequest`):

| campo         | tipo   | requerido | descripción                                                                 |
|---------------|--------|-----------|------------------------------------------------------------------------------|
| `template`    | string | sí        | nombre de una plantilla del catálogo                                        |
| `vcpus`       | int    | no        | overridea el `vcpus` de la plantilla                                        |
| `mem_mb`      | int    | no        | overridea el `mem_mb` de la plantilla                                       |
| `disk_mb`     | int    | no        | overridea el `disk_mb` de la plantilla; solo agranda (nunca encoge)        |
| `network`     | string | no        | red segmentada a la que conectar la VM (ver `## Redes`); vacío = `default`  |
| `no_network`  | bool   | no        | si `true`, no crea TAP/IP — la VM solo es accesible por vsock (`/exec`)     |
| `volumes`     | array  | no        | volúmenes a adjuntar al arrancar (ver `## Volúmenes`); cada uno `{name, read_only?, guest_path?}` |

```bash
curl -X POST localhost:8080/v1/vms -d '{"template":"base-ubuntu"}'
curl -X POST localhost:8080/v1/vms -d '{"template":"base-ubuntu","network":"lab"}'
curl -X POST localhost:8080/v1/vms -d '{"template":"base-ubuntu","no_network":true}'
# muestra montada de solo lectura + volumen de salida escribible
curl -X POST localhost:8080/v1/vms -d '{"template":"base-ubuntu","no_network":true,
  "volumes":[{"name":"muestra","read_only":true,"guest_path":"/mnt/sample"},
             {"name":"salida"}]}'
```

**Respuesta 201** (`VMResponse`, ver más abajo) · **400** si falta `template`
o el JSON es inválido · **500** si falla el clonado/red/arranque (el mensaje
de error incluye en qué paso falló).

Cada `POST` clona el rootfs de la plantilla desde cero
(`internal/storage.CloneRootfs`, copy-on-write vía `cp --reflink=auto`) y lo
agranda a `disk_mb` con `resize2fs` para que el guest tenga espacio libre (un
rootfs dorado va casi lleno; sin esto un `apt install` se queda sin espacio).
El CoW solo es real sobre un FS con reflink (btrfs / XFS-reflink); en ext4
normal `cp` cae a copia completa y cada VM ocupa el disco entero — el daemon
avisa de esto al arrancar y `scripts/setup-host.sh` provisiona un store CoW.
Hoy no hay forma de re-arrancar sobre un disco ya modificado de una VM anterior;
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
| `state`       | `creating`\|`running`\|`paused`\|`stopped`\|`failed` (se usan `running` y `stopped`) |
| `pid`         | PID del proceso Firecracker (jailed)                                        |
| `network`     | nombre de la red segmentada a la que está conectada (vacío si `no_network`) |
| `guest_ip`    | IP del guest en la subred de su red (vacío si `no_network`)                 |
| `tap_device`  | nombre del TAP, enslavado al bridge de la red (vacío si `no_network`)       |
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

Para a la máquina (`firecracker.Stop`: ACPI graceful + SIGTERM de respaldo, o
señal por PID si es una VM adoptada tras un reinicio), borra el TAP, libera la
IP en su red, borra el clon del rootfs + `.log`, el directorio de chroot de
Jailer y el registro persistido. La limpieza acumula errores (`errors.Join`):
un fallo en un paso no salta los demás.

**Destruir (`DELETE`) vs. apagar (`stop`)**: `DELETE` borra *todo*, incluido el
ext4 de la microVM (el disco). Si solo quieres liberar CPU/RAM y conservar el
disco, usa `stop` (abajo).

---

### `POST /v1/vms/{id}/stop` — apagar (poweroff, conserva el disco)

```bash
curl -X POST localhost:8080/v1/vms/a1b2c3d4/stop
```

**Respuesta 200** con el `VMResponse` (ahora `state: "stopped"`, `pid` omitido) ·
**404** si no existe · **409** si ya estaba parada.

Apaga el proceso Firecracker (libera CPU/RAM) y suelta el TAP y el directorio de
chroot de Jailer, pero **conserva el clon del rootfs** (el disco, con todo lo que
el guest haya escrito) y **mantiene la IP reservada** en su red. Sobrevive a un
reinicio del daemon: `Reconcile` no la barre, la deja parada y re-reserva su IP.
No se puede hacer `exec` sobre una VM parada (**409**).

### `POST /v1/vms/{id}/start` — arrancar una VM parada

```bash
curl -X POST localhost:8080/v1/vms/a1b2c3d4/start
```

**Respuesta 200** con el `VMResponse` (`state: "running"`, nuevo `pid`) ·
**404** si no existe · **409** si no está parada.

Recrea el TAP (que se soltó al parar) y relanza Firecracker sobre el **mismo**
ext4 y la **misma** IP que tenía. El bridge de la red sigue en pie (parar no lo
toca), así que arranca en frío con el disco y el direccionamiento intactos.

---

### `DELETE /v1/vms` — borrar todas las VMs

Resetea el entorno (útil entre tandas de pruebas) sin ir una a una.

```bash
curl -X DELETE localhost:8080/v1/vms
```

**Respuesta 200** siempre — destruir muchas VMs independientes no es
todo-o-nada; el resultado va en el body, no en el código HTTP.

**Forma de `BulkDeleteResponse`** (también la usa `DELETE /v1/networks/{name}/vms`):

| campo     | descripción                                                        |
|-----------|----------------------------------------------------------------------|
| `deleted` | array de IDs destruidos con éxito                                   |
| `failed`  | objeto `{id: mensaje de error}` — solo presente si algo falló       |

```json
{"deleted": ["a1b2c3d4", "e5f6a7b8"], "failed": {"c9d0e1f2": "vm not found"}}
```

---

## Snapshots y bifurcación

Un snapshot congela una VM **en marcha** como punto restaurable: memoria del
guest + estado de dispositivos (vmstate/mem de Firecracker) + un clon
copy-on-write de su disco, capturado todo en el mismo instante (la VM se pausa
<1s y se reanuda sola). El snapshot es una entidad independiente: **sobrevive
a que su VM de origen se pare o destruya** — ese es el punto: "detonar y
volver a limpio" exige que el estado limpio viva más que lo que pase después.

Dos formas de volver a un snapshot:

- **Restore in-place** (`POST /v1/vms/{id}/restore`): rebobina ESA VM — mismo
  ID, misma IP, mismo TAP; solo memoria y disco vuelven atrás. El primitivo
  "reset a limpio entre muestras".
- **Fork** (`POST /v1/snapshots/{id}/fork`): crea una VM **nueva** desde el
  snapshot — el guest despierta a mitad de ejecución justo donde se congeló.

Y un atajo que no requiere gestionar snapshots:

- **Fork directo** (`POST /v1/vms/{id}/fork`): bifurca una VM **en marcha** en
  una sola llamada — el daemon toma un snapshot efímero, forkea desde él y lo
  borra. Para "dame una copia de esta máquina tal y como está ahora".

**La identidad de red va congelada en la memoria.** El guest restaurado cree
tener la IP/MAC del momento del snapshot y eso no se puede cambiar al
restaurar. De ahí los dos modos de fork:

| modo | qué hace | cuándo |
|---|---|---|
| normal (por defecto) | el fork se une a la red de origen con la IP del snapshot; **409** si esa IP está ocupada (p. ej. la VM original sigue viva) | recuperar un estado conocido como VM plena |
| `quarantine: true` | TAP creado pero enslavado a **nada**: el guest cree tener red pero cada paquete muere en el host; solo accesible por vsock (`/exec`) | bifurcar el punto de infección y examinarlo sin que hable con nadie; permite **N forks simultáneos** del mismo snapshot |

### `POST /v1/vms/{id}/snapshot` — crear snapshot

**Body** (opcional): `{"name": "clean"}` — etiqueta libre.

```bash
curl -X POST localhost:8080/v1/vms/a1b2c3d4/snapshot -d '{"name":"clean"}'
```

**Respuesta 201** (`SnapshotResponse`) · **404** si la VM no existe · **409**
si no está en marcha (solo se puede snapshotear una VM `running`).

**Forma de `SnapshotResponse`:**

| campo | descripción |
|---|---|
| `id` | ID corto del snapshot |
| `name` | etiqueta opcional |
| `source_vm` | VM de la que se tomó |
| `template` | plantilla de la VM de origen |
| `vcpus` / `mem_mb` / `disk_mb` | forma de la máquina congelada (fija: la restauración vuelve exactamente así) |
| `network` / `guest_ip` | identidad de red congelada en la memoria del guest |
| `created_at` | timestamp RFC3339 |

### `GET /v1/snapshots` · `GET /v1/snapshots/{id}` · `DELETE /v1/snapshots/{id}`

Listado, detalle y borrado. Borrar un snapshot es seguro aunque haya VMs
restauradas desde él corriendo (tienen copias/hardlinks propios). **204** al
borrar · **404** si no existe.

### `POST /v1/snapshots/{id}/fork` — bifurcar (fork)

**Body** (`ForkVMRequest`, opcional): `{"quarantine": true}`.

```bash
# Fork normal: exige la IP del snapshot libre en su red de origen
curl -X POST localhost:8080/v1/snapshots/f00dcafe/fork

# Fork en cuarentena: sin red real, solo vsock
curl -X POST localhost:8080/v1/snapshots/f00dcafe/fork -d '{"quarantine":true}'
```

**Respuesta 201** (`VMResponse`; los forks llevan `restored_from` y, en su
caso, `quarantine: true`; en cuarentena `guest_ip` es la IP que el guest
*cree* tener, no una reserva real) · **404** si el snapshot no existe ·
**409** si la IP del snapshot está ocupada en la red de origen (destruye la
VM que la tiene o usa `quarantine`).

**Requisito de versión para forks simultáneos**: `network_overrides` (remapear
la NIC congelada a otro TAP) existe desde **Firecracker v1.12.0**. Con un FC
anterior (el daemon lo detecta solo), el fork reutiliza el nombre de TAP
original del snapshot — funciona si la VM de origen está destruida o parada,
pero **con la original (u otro fork) corriendo cualquier fork da 409**,
incluido `quarantine`, indicando que hay que actualizar
(`scripts/install-fc.sh`). Al actualizar FC, los snapshots existentes deben
recrearse (su formato va ligado a la versión).

### `POST /v1/vms/{id}/fork` — fork directo de una VM en marcha

**Body** (`ForkVMRequest`, opcional): `{"quarantine": true}` — el mismo que el
fork desde snapshot.

```bash
# Copia en cuarentena de una VM viva, en una llamada
curl -X POST localhost:8080/v1/vms/a1b2c3d4/fork -d '{"quarantine":true}'
```

Equivale a snapshot → fork → borrar el snapshot, sin que el snapshot efímero
quede registrado. La VM de origen solo se pausa <1s (igual que al snapshotear)
y sigue corriendo. Si quieres conservar el punto congelado para restores
posteriores, usa el flujo explícito (`/snapshots` + fork).

**Semántica de red**: la de fork, con una consecuencia práctica — la VM de
origen sigue viva ocupando su IP, así que el fork directo **sin** `quarantine`
siempre da 409 en una VM con red (la IP congelada está en uso por definición).
El modo natural de este endpoint es `quarantine: true` (o VMs `no_network`).

**Respuesta 201** (`VMResponse`, como el fork normal) · **404** VM inexistente
· **409** VM no `running`, o el conflicto de red/TAP correspondiente (en FC
< 1.12 el TAP original está siempre en uso por la propia VM de origen, así que
este endpoint requiere en la práctica **Firecracker ≥ 1.12**).

### `POST /v1/vms/{id}/restore` — rebobinar in-place

**Body** (`RestoreVMRequest`): `{"snapshot": "f00dcafe"}`. Solo acepta
snapshots tomados **de esa misma VM** (para restaurar el snapshot de otra VM
está el fork, que gestiona las colisiones de identidad honestamente).

```bash
curl -X POST localhost:8080/v1/vms/a1b2c3d4/restore -d '{"snapshot":"f00dcafe"}'
```

Vale sobre una VM `running` (se para primero) o `stopped`. **Respuesta 200**
(`VMResponse`, `state: "running"`) · **404** VM o snapshot inexistentes ·
**409** si el snapshot es de otra VM o la VM está en un estado incompatible.

**Nota (reloj del guest)**: tras cualquier restauración el reloj del guest
sigue en la hora del snapshot; para análisis donde importe el timestamp,
resincroniza vía `/exec` (p. ej. `date -s` o chrony). Es el comportamiento
documentado de Firecracker.

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

## Volúmenes

Discos ext4 persistentes que viven aparte de las VMs y sobreviven a su
destrucción — el plano de datos: una muestra montada de solo lectura, o un
disco escribible que recoge artefactos y se lee después. Detalle completo (canal
vsock vs `debugfs`, streaming, seguridad) en `docs/volumes.md`.

**Regla de seguridad**: el host **nunca monta** el filesystem del guest;
todo el I/O host-side va por `debugfs` (espacio de usuario, sin `mount`).

### `POST /v1/volumes` — crear

**Body** (`CreateVolumeRequest`):

| campo     | tipo   | requerido | descripción                          |
|-----------|--------|-----------|----------------------------------------|
| `name`    | string | sí        | nombre único del volumen              |
| `size_mb` | int    | sí        | tamaño del ext4 en MiB (fijo al crear) |

```bash
curl -X POST localhost:8080/v1/volumes -d '{"name":"muestra","size_mb":64}'
curl -X POST localhost:8080/v1/volumes -d '{"name":"dataset","size_mb":8192}'
```

**Respuesta 201** (`VolumeResponse`) · **400** si falta `name`/`size_mb` ≤ 0 ·
**409** si ya existe un volumen con ese nombre.

### `GET /v1/volumes` · `GET /v1/volumes/{id}`

Lista todos / detalle de uno. **Forma de `VolumeResponse`:**

| campo         | descripción                                             |
|---------------|----------------------------------------------------------|
| `id`          | id del volumen                                          |
| `name`        | nombre                                                  |
| `size_mb`     | tamaño en MiB                                           |
| `attached_to` | id de la VM que lo tiene adjunto, o ausente si libre    |
| `created_at`  | timestamp RFC3339                                       |

### `DELETE /v1/volumes/{id}`

```bash
curl -X DELETE localhost:8080/v1/volumes/VOLID
```

**Respuesta 204** · **404** si no existe · **409** si está adjunto a una VM
(destruye esa VM primero — el volumen persiste al destruirla).

### Adjuntar un volumen a una VM

**No hay endpoint de "attach"**: Firecracker no permite enchufar un disco a una
VM ya arrancada (no hay hot-plug). Un volumen se adjunta **al crear la VM**, en
el campo `volumes[]` de `POST /v1/vms`. Cada elemento:

| campo        | tipo   | requerido | descripción                                                        |
|--------------|--------|-----------|---------------------------------------------------------------------|
| `name`       | string | sí        | nombre de un volumen existente (creado con `POST /v1/volumes`)      |
| `read_only`  | bool   | no        | adjuntar como dispositivo de solo lectura (una muestra que el guest no debe alterar) |
| `guest_path` | string | no        | dónde montarlo dentro del guest; por defecto `/vol/<name>`          |

Tras arrancar, el daemon monta cada volumen en su `guest_path` por vsock (con
`-o ro` si es `read_only`). Firecracker los expone como `/dev/vdb`, `/dev/vdc`…
en el orden del array.

```bash
# 1) crear el volumen
curl -X POST localhost:8080/v1/volumes -d '{"name":"salida","size_mb":512}'

# 2) (opcional) prellenarlo offline sin arrancar nada
curl -X PUT "localhost:8080/v1/volumes/VOLID/files?path=/muestra.bin" --data-binary @muestra.bin

# 3) crear la VM con el volumen adjunto
curl -X POST localhost:8080/v1/vms -d '{
  "template":"base-ubuntu","no_network":true,
  "volumes":[{"name":"salida","guest_path":"/mnt/out"}]}'
```

Un volumen está adjunto **a lo sumo a una VM a la vez** — adjuntarlo a una
segunda mientras la primera lo tiene da **409**. Al destruir la VM el volumen se
suelta (queda libre) pero **no se borra**. Si necesitas escribir en un volumen
ya adjunto a una VM viva, hazlo por el canal de la VM
(`PUT /v1/vms/{id}/files`, más abajo), no por el endpoint offline del volumen.

### `PUT`/`GET /v1/volumes/{id}/files?path=…` — meter/sacar datos offline

Escribe o lee un fichero dentro del volumen **sin arrancar ninguna VM**, con
`debugfs` (sin montar). Es la vía para **preparar** un volumen: lo llenas y
luego lo adjuntas al crear la VM. En streaming — memoria constante sea cual sea
el tamaño (un dataset de varios GB no carga la RAM del host).

Solo válido con el volumen **libre** (`attached_to` vacío): escribir por debajo
de un guest que lo tiene montado lo corrompería.

```bash
# meter una muestra en el volumen (el cuerpo es el fichero crudo)
curl -X PUT "localhost:8080/v1/volumes/VOLID/files?path=/muestra.bin" --data-binary @muestra.bin
# sacar un artefacto
curl "localhost:8080/v1/volumes/VOLID/files?path=/salida/resultado.txt" -o resultado.txt
```

**Respuesta 204** (PUT) / **200** con el fichero (GET) · **400** si falta
`path` o no es una ruta absoluta válida · **404** si el volumen o el fichero no
existe · **409** si el volumen está adjunto a una VM.

> **Datos grandes**: Firecracker no tiene hot-plug de discos, así que no se
> adjunta un volumen a una VM ya arrancada. El modelo es **preparar y adjuntar**:
> creas el volumen del tamaño que necesites, lo llenas aquí (offline, streaming)
> y creas la VM con él en `volumes[]`.

---

## Ficheros de una VM

### `PUT`/`GET /v1/vms/{id}/files?path=…` — subir/bajar un fichero

Transfiere un fichero a/desde una VM. **Transparente al estado**: si la VM está
**running** va por vsock (funciona incluso sin red); si está **stopped** lee/
escribe su disco offline con `debugfs` (ruta *post-mortem*: sacar evidencias sin
arrancar). En streaming de punta a punta — memoria constante a cualquier tamaño.

```bash
# subir (Content-Length automático con --data-binary @fichero)
curl -X PUT "localhost:8080/v1/vms/VMID/files?path=/root/entrada.bin" --data-binary @entrada.bin
# bajar
curl "localhost:8080/v1/vms/VMID/files?path=/root/salida.bin" -o salida.bin
```

**Respuesta 204** (PUT) / **200** con el fichero (GET) · **400** si falta
`path` · **404** si la VM no existe · **409** si la VM no está `running` ni
`stopped`.

Ojo con el tamaño del disco raíz (`disk_mb`) si subes un fichero grande ahí;
para datos grandes usa un **volumen** dimensionado, no el rootfs.

---

## Lo que falta (ver `SESSIONS.md` → "Próxima sesión")

- Poder crear a partir de un disco ya usado (no solo de la plantilla dorada).
- `GET /v1/vms` con más forma de "`docker ps`" (uptime, columnas pensadas
  para un panel web).
- Auditoría de que `DELETE` no deja huérfanos en ningún camino de fallo.
