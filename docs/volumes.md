# Volúmenes y transferencia segura de datos

Cómo MicroHosted mete y saca datos de las microVMs sin exponer el host. Es el
plano de datos de la plataforma: en el workflow de detonación es lo que lleva la
muestra a la VM aislada y lo que recoge los artefactos después.

## Principio rector: el host NUNCA monta un filesystem del guest

`mount(2)` de una imagen ext4 no confiable ejecuta el parser ext4 del **kernel
del host** sobre bytes controlados por el atacante — la superficie de escape
clásica (histórico largo de CVEs de ext4/journal). Por eso **todo** el I/O
host-side va por **`debugfs`** (de `e2fsprogs`), una herramienta de espacio de
usuario que lee/escribe ext4 **sin montar**: una imagen maliciosa como mucho hace
fallar al propio proceso `debugfs`, nunca toca el kernel del host.

Hay dos canales de datos, según dónde esté la VM:

| Canal | Cuándo | Cómo |
|---|---|---|
| **vsock** | VM viva | Streaming por el puerto vsock 52 (mismo canal que `exec`). Funciona sin red (`no_network`, quarantine). |
| **debugfs** | VM parada / volumen suelto | Lectura/escritura offline sobre el fichero ext4, sin montar. |

Los endpoints de ficheros eligen el canal solos según el estado de la VM.

### Memoria constante (streaming de punta a punta)

Toda transferencia va en **streaming**: ni subida ni bajada cargan el fichero
entero en RAM, así que da igual que sean 4 KB o 40 GB — el coste en memoria del
daemon es un búfer fijo. Como `debugfs` no lee/escribe un stream (sus verbos
`write`/`dump` toman ficheros reales del host), el canal offline **stagea** en un
temporal **en el store** (`<store>/staging/`, disco real), **nunca en `/tmp`**
(que suele ser tmpfs = RAM). Los temporales de extracción se borran del
directorio nada más abrirlos (siguen válidos por el descriptor abierto), y un
crash a medias se barre al reiniciar.

Si subes por el endpoint de VM (`PUT /v1/vms/{id}/files`) sin `Content-Length`
(cuerpo *chunked*) y la VM está viva, el daemon stagea primero en el store para
saber el tamaño que el protocolo vsock necesita — sigue siendo memoria
constante, solo que pasando por disco. `curl --data-binary @fichero` manda
`Content-Length`, así que ese caso streamea directo sin stage.

## Volúmenes

Un **volumen** es un disco ext4 persistente que vive independiente de las VMs
(`internal/storage/volume.go`, bajo `<store>/volumes/<id>.ext4`). Sobrevive al
`destroy` de la VM — es donde persisten los datos. Dos usos canónicos:

- **Muestra read-only**: se adjunta con `read_only:true`; es un dispositivo de
  bloque de solo lectura, así que el guest que la analiza no puede alterarla (lo
  rechaza el propio bloque, no solo una opción de mount).
- **Volumen de salida writable**: recoge artefactos (pcaps, dumps, resultados)
  que se leen después de que la VM ya no exista.

Un volumen está adjunto **a lo sumo a una VM a la vez** (`AttachedTo`): dos
guests escribiendo el mismo ext4 lo corromperían.

### Datos grandes: preparar y luego adjuntar

Firecracker **no tiene hot-plug de discos**: no se puede enchufar un volumen
nuevo a una VM que ya arrancó. El modelo correcto para un dataset grande
(varios GB) es **preparar el volumen y adjuntarlo al crear la VM**:

1. `POST /v1/volumes` con el `size_mb` que necesites (el volumen es un ext4 de
   tamaño fijo; ponle los GB que hagan falta — es el sitio para datos grandes,
   no el disco raíz de la VM).
2. `PUT /v1/volumes/{id}/files?path=…` para llenarlo **offline por debugfs, en
   streaming** (sin arrancar VM, memoria constante).
3. `POST /v1/vms` con ese volumen en `volumes[]` → la VM arranca con el dato ya
   dentro, montado en `guest_path`.

Si la VM ya existe y ya tiene un volumen adjunto, escribes en él **por vsock en
streaming** con `PUT /v1/vms/{id}/files` apuntando a una ruta dentro de su
`guest_path`. Lo que no hay (a propósito, porque Firecracker no lo soporta) es
adjuntar un volumen a una VM en marcha.

### Ciclo de vida

```
POST   /v1/volumes                 {name, size_mb}   -> crea (mkfs.ext4)
GET    /v1/volumes                                   -> lista
GET    /v1/volumes/{id}                              -> detalle (incl. attached_to)
DELETE /v1/volumes/{id}                              -> borra (409 si adjunto)
```

### Adjuntar a una VM

En `POST /v1/vms`, campo `volumes`:

```json
{
  "template": "detonation",
  "no_network": true,
  "volumes": [
    {"name": "sample-in", "read_only": true, "guest_path": "/mnt/sample"},
    {"name": "artifacts-out"}
  ]
}
```

Tras el boot, el daemon monta cada volumen **por vsock** en `guest_path` (por
defecto `/vol/<nombre>`), con `-o ro` si es read-only. Firecracker expone los
discos secundarios como `/dev/vdb`, `/dev/vdc`… en el orden de la lista, que es
el orden en que se montan. Si el mount falla, el `Create` falla y hace rollback.
Un volumen sin `guest_path` se deja como dispositivo crudo para que lo monte el
guest.

Al hacer `destroy` de la VM, los volúmenes se **sueltan** (`AttachedTo` a vacío)
pero sus ficheros **no se borran**.

> **Snapshots + volúmenes**: no soportado en v1. `snapshot`/`fork`/`restore`
> sobre una VM con volúmenes adjuntos devuelven **409**. La RAM del snapshot
> tiene el volumen montado (page cache, journal); restaurar sobre un volumen que
> cambió desde entonces lo corrompe. Destruye la VM (los volúmenes persisten) y
> recréala sin ellos.

## Transferencia de ficheros

### Con la VM (viva → vsock, parada → debugfs)

```
PUT /v1/vms/{id}/files?path=/ruta/en/guest      cuerpo = bytes
GET /v1/vms/{id}/files?path=/ruta/en/guest      respuesta = bytes
```

Transparente al estado: con la VM **running** va por vsock (incluso sin red);
con la VM **stopped** lee/escribe el disco offline con `debugfs`. Este segundo
caso es la ruta **post-mortem**: paras una VM de detonación y sacas ficheros
directamente de su disco sin arrancarla ni montarla. Ambos en streaming; ojo con
el tamaño del disco raíz de la VM (`disk_mb`) si subes un fichero grande ahí —
para datos grandes usa un volumen dimensionado, no el rootfs.

### Con un volumen suelto (offline, debugfs)

```
PUT /v1/volumes/{id}/files?path=/ruta      cuerpo = bytes   (inyecta una muestra antes de adjuntar)
GET /v1/volumes/{id}/files?path=/ruta      respuesta = bytes (extrae resultados tras el destroy)
```

Solo válido con el volumen **no adjunto**: escribir por debajo de un guest que lo
tiene montado lo corrompe. Si está adjunto y la VM viva, usa el canal de la VM.

## El agente guest

`scripts/prepare-image.sh` instala `microhosted-exec`, que multiplexa tres verbos
sobre el puerto vsock según la primera línea de cada conexión:

- comando pelado (sin verbo) → `exec` (contrato histórico intacto)
- `PUT <path> <len>` + `<len>` bytes → escribe el fichero
- `GET <path>` → responde `OK <len>` + bytes, o `ERR <msg>`

Requiere `socat` en la imagen (ya necesario para `exec`).
