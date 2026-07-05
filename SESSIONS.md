# SESSIONS — Registro de trabajo por sesión

Este archivo documenta qué se hizo, qué decisiones se tomaron y qué problemas se
encontraron en cada sesión. Se actualiza al final de cada sesión de trabajo.

---

## Sesión 0 — Arranque manual (pendiente)

**Objetivo**: levantar una microVM a mano con `curl` sin escribir código.

**Estado**: pendiente

**Tareas**:
- [ ] Instalar `firecracker` y `jailer` (misma versión) con `scripts/install-fc.sh`
- [ ] Verificar `/dev/kvm` accesible
- [ ] Descargar kernel mínimo (`vmlinux`) y rootfs de ejemplo de AWS
- [ ] Configurar socket Unix y lanzar `firecracker` directo (sin Jailer todavía)
- [ ] Secuencia de `curl` para: boot-source → drives → machine-config → InstanceStart
- [ ] Conectar por consola serie y verificar que el guest responde
- [ ] Apagar limpio y verificar que no quedan procesos ni sockets huérfanos

**Criterio de éxito**: consola serie funcional en microVM levantada a mano, apagado
limpio verificado.

---

## Sesión 1 — Cliente Go (validado contra KVM real)

**Objetivo**: reemplazar los `curl` manuales con código Go usando `firecracker-go-sdk`.

**Estado**: hecho y probado en este host — no solo código, se creó/arrancó/destruyó
una microVM real de punta a punta. Se fusionó con la Sesión 2 y adelantó parte de
la 5/7 a petición del usuario: wrapper/API pensado para un panel web futuro,
catálogo de plantillas y clonado de disco, en vez de un cliente CLI mínimo.

**Qué se hizo**:
- `internal/firecracker/machine.go`: `BuildConfig`/`Launch`/`Stop` sobre
  `firecracker-go-sdk` (boot-source, drive raíz, machine-config, red estática, vsock).
- `pkg/types`: `Template`, `CreateVMRequest`/`VMResponse`, `VMConfig.TemplateName`.

---

## Sesión 2 — Integración con Jailer (validado contra KVM real)

**Objetivo**: agregar aislamiento real con Jailer (chroot + cgroups + seccomp).

**Estado**: hecho. Se usa el soporte de Jailer ya integrado en
`firecracker-go-sdk` (`JailerConfig` + `NaiveChrootStrategy`) en vez de reimplementar
el chroot a mano — el SDK arma `<chroot-base>/<exec-file>/<id>/root/`, hace hardlink
del kernel/rootfs/binario y lanza `jailer` por nosotros. Confirmado con
`ls /proc/<pid>/root/` (con barra final — sin ella `ls` no sigue el symlink) que el
proceso de Firecracker solo ve el contenido del jail, no el filesystem del host.

**Qué se hizo**:
- `internal/jailer/config.go`: `Defaults` (uid/gid/paths, `CgroupVersion`
  autodetectada con `DetectCgroupVersion`) + `Build`/`WorkspaceRoot`.
- `internal/vm/manager.go`: orquesta clonado de disco → red (opcional) → Jailer →
  Firecracker, con rollback si algo falla a mitad de camino.
- `internal/api/server.go` + `cmd/microhosted/main.go`: API HTTP
  (`POST/GET/DELETE /v1/vms`, `GET /v1/templates`, `POST /v1/vms/{id}/exec`).

**Bugs reales encontrados y corregidos depurando contra hardware** (quedan aquí
para no repetirlos): Jailer asume cgroup v1 si no se le dice lo contrario y este
host solo monta cgroup2 (`Hierarchy not found`, ahora autodetectado); el rootfs
clonado hay que chownearlo al uid/gid de Jailer o Firecracker no puede abrirlo en
escritura; la consola serie del guest se engancha a stdin/stdout si estos apuntan
a un TTY (por eso el log de cada VM va a su propio archivo, nunca a la terminal
del daemon); el contexto de la petición HTTP no puede ser el mismo que lanza el
proceso de Firecracker (`exec.CommandContext` lo mataría al terminar la request).

---

## Sesión 3 — Red (cubierta, opcional por VM)

**Objetivo**: TAP device e IP estática, SSH/ping desde host a guest.

**Estado**: hecho y confirmado con `ping` real. `internal/network` crea el TAP
device y asigna un bloque `/30` por VM (`172.16.0.0/16`, simplista/en memoria —
IPAM real queda para la Sesión 9, y un barrido de huérfanos al arrancar
(`network.SweepOrphans`) evita que reinicios del daemon choquen con TAPs de una
ejecución anterior); la IP del guest se configura vía `IPConfiguration` del SDK
(equivalente al parámetro de kernel `ip=`, sin agente ni DHCP).

La red es **opt-out, no obligatoria**: `POST /v1/vms` acepta `"no_network": true`
para lanzar una VM sin TAP/IP en absoluto — pensado para sandboxes efímeros que no
necesitan ninguna ruta al host (ver acceso por vsock más abajo). No implementado:
NAT/salida a internet desde el guest (no era el criterio de éxito de esta sesión).

---

## Acceso a las VMs — vsock (programático) + SSH (interactivo), ambos de igual nivel

No es una sesión numerada del roadmap original, pero fue necesario resolverlo
como pieza propia de la plataforma en vez de ir parcheando: el objetivo es poder
tanto **entrar** a una VM (shell real) como **ejecutar comandos desde código**
(sandboxing), y ninguna de las dos vías debe depender de scripts sueltos que haya
que recordar ejecutar.

- `internal/vsock` + `POST /v1/vms/{id}/exec`: canal programático. No usa IP ni
  TAP — funciona igual con `no_network: true`. En el guest lo atiende
  `socat VSOCK-LISTEN` (ya viene instalado en el rootfs de Firecracker CI), sin
  necesidad de compilar ni mantener un agente propio.
- SSH: canal interactivo para humanos, sobre la red normal (necesita que la VM
  tenga TAP/IP).
- `scripts/prepare-image.sh` (sustituye a un script anterior de solo-SSH que se
  quedó corto): un único paso que prepara **ambas** vías sobre el rootfs dorado,
  con flags `--no-ssh`/`--no-vsock` si una plantilla concreta solo necesita una.
  Usa (y genera si hace falta) una clave propia del proyecto en `images/keys/`
  — nunca las claves personales del operador.

---

## Plan activo — Backend funcional (dirección refinada, 2026-07-01)

Se afinó el rumbo (ver `PROJECT.md` → "Dirección refinada"): dos casos de uso
sobre el mismo núcleo — **sandboxing de ciberseguridad/forense** y **sandboxing
para agentes de IA** — y dos decisiones de arquitectura fijadas con el usuario:
**red segmentada por nombre** (no el `/30` con host-como-gateway actual) y
**persistencia SQLite** desde ya. Detalle completo del plan por fases en
`PROJECT.md`. Estado por fases:

- **Fase 0 — Fundamento (CERRADA, validada en hardware)**: limpieza total en
  `Destroy` (chroot de Jailer incluido), persistencia SQLite + reconciliación
  por PID, despliegue systemd (ver abajo). Validado end-to-end en el host:
  crear VM → `systemctl restart` → misma PID readoptada → `exec` devuelve
  `root`. Los límites cgroup se reencuadraron a Fase 4 (multi-tenencia): el
  guest ya está contenido por KVM+Jailer, así que el cgroup aporta política de
  overcommit, no aislamiento anti-malicioso (eso lo dan KVM/Jailer + la red de
  Fase 1).

  **Despliegue systemd (hecho)**: `deploy/microhosted.service.in` +
  `scripts/install-service.sh` + targets `make install-service` /
  `uninstall-service` / `service-logs`, documentado en `docs/deploy.md`. La
  clave es `KillMode=process`: al reiniciar el daemon systemd NO mata los
  Firecracker hijos, así que las VMs sobreviven y `Manager.Reconcile` las
  readopta por PID. Esto resuelve de raíz que un Ctrl-C en primer plano
  matara las VMs (SIGINT al grupo de procesos de la terminal). También
  `Delegate=yes` (cgroups de Jailer) y `AssertPathExists=/dev/kvm`.

  **Bug real encontrado depurando esto (importante)**: con systemd bien
  configurado, un `systemctl stop/restart` SEGUÍA matando las VMs, aunque
  Firecracker vivía en su propio cgroup `/firecracker/<id>` (fuera del cgroup
  del servicio, confirmado con `/proc/<pid>/cgroup`). La causa NO era systemd
  ni cgroups: el `firecracker-go-sdk` por defecto (`ForwardSignals == nil`)
  instala en NUESTRO daemon un manejador que **reenvía** SIGINT/SIGTERM/SIGHUP/
  SIGQUIT/SIGABRT al proceso Firecracker hijo (`machine.go` `setupSignals`).
  Así, el SIGTERM que systemd manda al daemon se propagaba a las VMs. Arreglo:
  `firecracker.BuildConfig` fija `ForwardSignals: []os.Signal{}` (slice vacío,
  no nil) para desactivar el reenvío — la vida de la VM es del Manager, solo
  la termina un Destroy explícito.
- **Fase 1 — Red segmentada (CERRADA, validada en hardware 2026-07-02)**:
  reemplaza el `/30` punto-a-punto por redes con nombre (bridge por red, TAP
  enslavado, IPAM por-red, nftables).
  Diseño completo en `docs/networking.md`. Hecho hasta ahora (compila + tests):
  - Modelo `types.Network` + `network` en `CreateVMRequest`/`VMConfig`/`VMResponse`.
  - Persistencia: tabla `networks` (name UNIQUE) + `SaveNetwork`/`DeleteNetwork`/
    `ListNetworks` en `store`.
  - IPAM por-red: `network.Subnet` (`ParseSubnet`/`Allocate`/`Reserve`/`Release`,
    reserva .1 como gateway), con tests. Bug cazado por el test: el atajo de
    marca de agua (`next`) se saltaba IPs liberadas por debajo; ahora escanea
    el rango entero.
  - Primitivas de data plane: `CreateBridge`/`DeleteBridge`/`BridgeExists` +
    `CreateTapEnslaved` (TAP sin IP, enslavado al bridge).
  - `network.ApplyNftables`: ruleset declarativo (re-render entero + `nft -f -`
    atómico) — drop guest→host (salvo established, para SSH host→guest),
    drop cross-segment por pares, egress NAT condicional. Con test del render.
  - `network.Manager`: CRUD de redes, pool de `/24` desde `172.16.0.0/12`,
    attach/detach VM, reconcile de bridges al arrancar + red `default` auto.
  - `vm.Manager` **movido del `/30` a bridges**: `AttachVM` + `CreateTapEnslaved`
    en create, `DetachVM` en destroy/reconcile. Modelo `/30` retirado
    (`alloc.go`/`CreateTap` borrados).
  - Endpoints `/v1/networks` (POST/GET/GET{name}/DELETE) + `network` en el
    create de VM. `setup-host.sh` instala `nftables`.
  - Egress: `EnsureIPForward` (ip_forward=1) + `EnsureDockerForwarding`
    (coexistencia con Docker vía `DOCKER-USER`, ver `docs/networking.md` →
    "Egress y coexistencia con el firewall del host"). Best-effort: si fallan,
    warning y el daemon sigue vivo; la tabla `inet microhosted` es autoritativa.
  - **Validada en hardware**: 4 bugs cazados y arreglados en el proceso (máscara
    `/30` → broadcast; sin MAC → colisión en bridge; ip_forward; Docker FORWARD
    drop). inter-VM OK, aislamiento entre redes OK, guest↛host OK, egress
    false/true OK sobre host con Docker. Ver `docs/networking.md`.
  - **Bug #5, encontrado después de cerrar la fase**: DNS no resolvía
    (`ping 8.8.8.8` OK, `ping google.com` fallaba). Dos causas, ninguna de
    firewall: (a) nunca pasábamos `Nameservers` al SDK — arreglado con
    `defaultNameservers = [1.1.1.1, 8.8.8.8]` en `firecracker.BuildConfig`;
    (b) aunque el kernel los vuelca en `/proc/net/pnp`, el guest no lee de ahí
    salvo que `/etc/resolv.conf` sea symlink a ese archivo — arreglado en
    `prepare-image.sh` (paso incondicional, no ligado a `--no-ssh`/`--no-vsock`).
    A diferencia del bug de Docker, este es de la IMAGEN, no del host — mismo
    fix en cualquier host. Documentado a fondo en `docs/networking.md` → "DNS
    en el guest". Requiere: volver a correr `prepare-image.sh` sobre la
    plantilla dorada + crear una VM NUEVA (las clonadas antes no cambian).

  **Bulk delete (a petición del usuario)**: `DELETE /v1/vms` (borra todas las
  VMs) y `DELETE /v1/networks/{name}/vms` (borra las VMs de una red — paso
  previo a poder borrar esa red, que hoy rechaza si tiene VMs conectadas).
  `vm.Manager.destroyMatching` es el helper compartido: destruye cada VM de
  forma independiente (un fallo no frena a las demás) y devuelve
  `(deleted []string, failed map[string]error)`. Respuesta siempre 200
  (`BulkDeleteResponse{deleted, failed}`) — no es todo-o-nada.
- **Fase 2 — Almacenamiento (EN CURSO)**: tamaño de disco configurable + store
  copy-on-write (hecho y validado, ver bloque abajo); volúmenes extra (`Volume`
  + CRUD, muestra RO + salida writable para artefactos) pendientes.
- **Fase 3 — Completar CRUD**: Update (inyectar archivo/playbook) + Read estilo
  `docker ps`.
- **Fase 4 — Endurecimiento + prueba de resistencia**.

`docs/api.md` se mantiene al día según se añaden/cambian endpoints.

---

## Fase 2 (parte 1) — Tamaño de disco + store copy-on-write (2026-07-02, validada en hardware)

Disparador real: dentro de una microVM, `apt update`/`install` fallaba con
`No space left on device` y dejaba `/var/lib/dpkg/status` corrupto. Causa: el
rootfs dorado es un ext4 de tamaño fijo casi lleno, y el clon lo copiaba tal
cual — cada VM nacía sin espacio libre.

**Tamaño de disco configurable.** Nuevo campo `disk_mb` en `types.Template`,
`CreateVMRequest` y `VMConfig`. `storage.CloneRootfs` recibe `diskMB` y, tras
clonar, agranda el clon con `truncate` + `e2fsck -fy` + `resize2fs` (offline,
sin nada dentro del guest: los goldens son ext4 sobre el dispositivo entero, sin
tabla de particiones, así que un resize offline basta). Solo crece, nunca encoge.
`base-ubuntu` → `disk_mb: 1024`. Tests: `TestCloneRootfsGrows` (mkfs.ext4 real →
grow → asserta tamaño) + `TestCloneRootfsNeverShrinks`.

**Store copy-on-write, agnóstico al host.** El host es ext4 **sin reflink**
(comprobado), así que `cp --reflink=auto` caía a copia completa: cada VM = copia
entera del rootfs → con imágenes de 1GB el disco se llena en un instante. Arreglo
en `scripts/setup-host.sh`: si el directorio de instancias no está ya sobre un FS
con reflink, provisiona un **loopback btrfs** (fichero sparse + `mkfs.btrfs` +
mount + fstab; btrfs va en el kernel de cualquier Ubuntu, sin reparticionar).
Guardrail en el daemon: `storage.SupportsReflink` sondea reflink real al arrancar
y suelta un AVISO visible si el store no es CoW (nunca duplicar en silencio).

**Bug #6 — reflink no cruza filesystems.** Con el store btrfs montado, los clones
seguían costando 300MB completos. Los goldens vivían en `images/rootfs` (ext4 del
host) y los clones en `images/instances` (btrfs): `cp --reflink` entre FS
distintos = copia completa. Arreglo: los goldens viven en el MISMO btrfs, en
`images/instances/rootfs/`; el catálogo apunta ahí. Verificado con
`btrfs filesystem du -s`: clon reflink = **0 B Exclusive** / 300MiB shared; clon
completo de plataforma (reflink + grow 1GB + resize2fs) = **236 KiB Exclusive**.
Cada VM cuesta sus deltas, no 1GB. (Nota: el `df` de btrfs NO sirve para medir
CoW — salta por asignación de chunks de metadata; usar `btrfs fi du`.)

**Bug #7 — hardlink no cruza filesystems (`invalid cross-device link`).** El
primer create tras el cambio falló al arrancar: `NaiveChrootStrategy` del SDK
**hardlinka rootfs Y kernel** dentro del chroot, y el chroot (`/srv/jailer`) +
el kernel (`images/kernels`) estaban en el ext4 del host mientras el clon estaba
en el btrfs → EXDEV. No es solo eficiencia: Stop/Start depende de que clon y
rootfs del chroot sean el MISMO inodo (hardlink). Arreglo — todo lo que Jailer
hardlinka/clona comparte el FS del store: (a) `cmd/microhosted/main.go` **deriva
`--chroot-base` de `--instances-dir`** por defecto (`<instances>/jailer`, mismo
FS) en vez del histórico `/srv/jailer`, resuelve a ruta absoluta y crea el dir;
(b) kernel movido a `images/instances/kernels/` y catálogo repuntado; (c) golden
ya en `images/instances/rootfs/`; (d) `setup-host.sh` crea
`<instances>/{rootfs,kernels,jailer}` en el store con la nota del invariante
"hardlink/reflink = mismo FS". Layout final del store (un solo btrfs):
`images/instances/{rootfs/, kernels/, jailer/, <id>.ext4}`. Ver `docs/layers.md`
(L3) y `docs/architecture.md`.

**VALIDADO en hardware**: tras `make install-service`, crear una VM `base-ubuntu`
en una red con egress arranca, tiene espacio (`apt install` funciona) y el clon
es CoW. El usuario confirmó "todo funciona".

Esto fue un cambio **estructural**, no un parche: se cazaron dos invariantes
latentes (reflink y hardlink exigen mismo FS) que habrían roto en CUALQUIER host
donde el store del disco no coincidiera con el FS de jailer/kernel, y se
**codificaron** (el chroot deriva del store; el daemon avisa si no hay CoW). Más
correcto y más resistente a mala configuración; falta probar la robustez en
operación (soak test con muchas VMs + reinicios).

**Bug #8 — store dentro del repo rompía el tooling.** El store estaba montado en
`images/instances` (dentro del repo), así que los jail dirs de Jailer (propiedad
de root) vivían en el árbol de fuentes: `go build ./...` fallaba con
`permission denied` mientras hubiera una VM viva (Go recorre el chroot de root).
Smell real: datos de runtime no van en el código. Arreglo: el store se movió a
**`/var/lib/microhosted/store`** (absoluto, fuera del repo; el fichero btrfs ya
estaba en `/var/lib/microhosted/instances.btrfs`, solo mal montado). Cambios:
default de `--instances-dir` + unit + catálogo (`rootfs_path`/`kernel_path`
absolutos) → nuevo path; `setup-host.sh` con bloque de **migración** (`losetup -j`
+ `findmnt` detectan el btrfs montado en otro sitio, lo desmontan, limpian fstab
y remontan — sin copiar datos, es el mismo subvolumen). Gotcha operativo al
migrar: `KillMode=process` hace que las VMs sobrevivan al stop del servicio, así
que el `umount` da "target is busy" hasta matar los `firecracker` huérfanos
(`sudo pkill -9 -f '/firecracker --id'`). Validado: tras migrar, `go build ./...`
limpio y crear VM funciona.

**Estado: Fase 2 parte 1 CERRADA y validada en hardware.** El usuario confirmó
"ahora funciona todo". Siguiente: soak test (robustez en operación) o volúmenes
extra (Fase 2 parte 2). Ver `docs/layers.md` para el mapa completo por capas.

---

## Matices de la idea de producto (2026-07-02)

Afinado con el usuario tras cerrar el store CoW (detalle en `PROJECT.md` →
"Dirección refinada"). El encuadre pasa de "dos casos de uso" a un
**posicionamiento**: **plataforma de sandboxing de seguridad autohosted**, con un
**motor neutral** (ciclo de vida + aislamiento + snapshots) y una **librería
curada de imágenes desechables** para distintos usos defensivos (detonación de
malware, honeypots, bancos DFIR, rangos blue-team, y — segundo acto, mismo motor
— sandbox de agentes IA).

- **El foso es la soberanía del dato.** El autohosted es lo que los sandboxes
  cloud (ANY.RUN, Joe, e2b) no pueden igualar por estructura: no mandas la
  muestra/el dato a un tercero. Para banca/defensa/sanidad/air-gap eso es un "no"
  rotundo, no una preferencia.
- **Incumbente a desplazar: CAPEv2/Cuckoo** (el sandbox de malware self-hosted
  clásico, QEMU pesado y doloroso de operar). Ángulo: el sucesor moderno en
  microVMs Firecracker, API-first, que sí se instala.
- **Disciplina clave: motor general, primer workflow afilado.** "Biblioteca para
  distintos usos" es la promesa/superficie, no el lanzamiento. Se lanza con UN
  workflow hondo (detonación: muestra → VM aislada sin egress → corre → captura
  artefactos → reset a limpio), no con la biblioteca entera. Amplitud = promesa;
  profundidad-de-uno = prueba.
- **Consecuencias de roadmap**: el sistema de imágenes/catálogo sube a activo de
  primera clase (templates versionadas, manifests, builds reproducibles,
  imágenes firmadas) — conecta con las imágenes ultra-optimizadas (Alpine/Rocky)
  pendientes. **Snapshots** siguen siendo lo más estratégico (reset-a-limpio,
  bifurcar en el punto de infección) — más que el multi-host/HA. El **modelo de
  amenaza escrito + tests adversariales** deja de ser opcional: en defensivo,
  "aquí está el modelo de amenaza y los tests que lo verifican" ES el argumento
  de venta.
- **Orden**: control plane / colas / HA / multi-host son etapas posteriores, y
  su orden lo dicta un usuario/caso que tira, no una checklist genérica de
  escalar. Primero: soak test (robustez en operación) → snapshots → workflow
  vertical de detonación.

---

## Sesión 4 — Concurrencia (pendiente)

**Objetivo**: 5 microVMs simultáneas sin colisiones ni fugas.

**Estado**: pendiente

---

## Sesión 5 — Pipeline de imágenes (pendiente)

**Objetivo**: scripts reproducibles para construir kernel y rootfs propios.

**Estado**: pendiente

---

## Sesión 6 — Snapshot/restore + bifurcación (implementada 2026-07-03, VALIDADA EN HARDWARE 2026-07-04)

**Objetivo**: arranque desde snapshot < 200ms + bifurcar (fork) desde un punto congelado.

**Estado**: VALIDADA end-to-end contra KVM real. Medido (VM 128MB, endpoint
HTTP completo): **snapshot 271ms · restore in-place 109ms · fork 113ms** — el
criterio de <200ms de la sesión se cumple con margen. Batería completa
superada: rebobinado real de memoria+disco (contador en memoria reanuda en el
valor congelado, no en 0 ni en el sucio; fichero rebobinado), fork despierta
con la IP del snapshot reclamada y respondiendo, cuarentena aislada de verdad
(guest cree tener red, no alcanza ni al gateway, vsock OK), 409s honestos,
persistencia tras restart del daemon (snapshot sobre VM adoptada incluida, con
mismo PID), borrado de snapshot con fork vivo inocuo, y cero fugas al final
(TAPs/clones/jail dirs/snapshots).

**Bug real cazado EN la validación**: asumí `network_overrides` desde FC v1.8;
en realidad es de **v1.12.0** y el host tiene 1.10.1 → el primer restore dio
400. Arreglo (validado): el daemon sondea la versión del binario al arrancar;
el restore in-place nunca envía override (recrea el TAP con su nombre
original); el fork en FC <1.12 reutiliza el nombre de TAP original si está
libre y si no da 409 explicando la actualización; en FC ≥1.12 cada fork lleva
TAP propio + override. El fallo de restore dejó la VM `stopped` con disco
rebobinado y reintentable — el camino de fallo diseñado funcionó tal cual.
Pendiente opcional del host: `sudo ./scripts/install-fc.sh v1.16.1` para forks
simultáneos (recrear snapshots después: su formato va ligado a la versión).

**Qué hay**:
- `POST /v1/vms/{id}/snapshot` — pausa (<1s) → vmstate+mem → reflink del disco
  en pausa (consistencia memoria↔disco) → resume. Los artefactos van a
  `<store>/snapshots/<sid>/`. Funciona también sobre VMs adoptadas tras un
  reinicio (llamadas crudas al socket UDS, sin handle del SDK).
- `POST /v1/vms/{id}/restore` — rebobinado in-place (mismo ID/IP/TAP): el
  primitivo "reset a limpio entre muestras". Solo snapshots de esa misma VM.
- `POST /v1/snapshots/{id}/fork` — fork a VM nueva. Dos modos por la identidad
  de red congelada en la memoria: normal (reclama la IP del snapshot en la red
  de origen, reserva estricta, 409 si ocupada) y `quarantine` (TAP sin bridge,
  solo vsock, N forks simultáneos).
- CRUD de snapshots (`GET/DELETE /v1/snapshots[/{id}]`), persistidos en SQLite
  y re-indexados al arrancar; un snapshot sobrevive al destroy de su VM.

**Decisiones técnicas** (detalle en `docs/architecture.md`):
- El SDK v1.0.0 no conoce `network_overrides` (imprescindible para que el fork
  use su propio TAP; Firecracker ≥1.8 lo soporta, tenemos 1.10.1) → el load se
  hace con una llamada cruda `PUT /snapshot/load` enganchada como handler
  propio en el pipeline del SDK (`LaunchFromSnapshot`), manteniendo Jailer y
  la gestión de proceso del SDK.
- El invariante "todo en el mismo btrfs" paga de nuevo: mover el snapshot fuera
  del chroot es un `rename`, capturar/estampar discos son reflinks, y montar el
  chroot del restore son hardlinks.
- El mem se restaura con backend File (mapeo copy-on-write): N restores
  comparten el mismo fichero de memoria sin copiarlo ni escribirlo.
- Borrar un snapshot con forks vivos es seguro (tienen inodos/copias propios).

**Refinado post-validación (2026-07-04)** — dos asperezas de la validación,
resueltas:
- `scripts/install-fc.sh` instalaba v1.10.1 por defecto (el usuario lo corrió
  esperando la nueva y obtuvo la vieja). Ahora sin argumentos resuelve e
  instala la **última release** (siguiendo el redirect de
  `releases/latest`, sin depender de la API de GitHub), y al terminar
  recuerda: reiniciar el daemon (la capacidad `network_overrides` se sondea
  al arrancar) y recrear los snapshots (formato ligado a versión).
- **Fork directo** `POST /v1/vms/{id}/fork` (body `{"quarantine":true}`
  opcional): bifurcar una VM viva ya no exige gestionar un snapshot —
  `vm.Manager.ForkVM` compone snapshot efímero → `Fork` → `DeleteSnapshot`
  (borrar bajo un fork vivo ya estaba validado como seguro: disco reflink +
  hardlinks propios). Sin `quarantine` sigue dando 409 con la origen viva
  (su IP congelada está en uso por definición) — el modo natural del
  endpoint es cuarentena. En FC <1.12 requiere en la práctica actualizar
  (la origen ocupa el TAP original). PENDIENTE de validar en hardware con
  FC ≥ 1.12.
- **Rutas unificadas** (a petición del usuario — convivían dos estilos):
  regla única "sustantivo = recurso CRUD, verbo = acción
  (`POST /v1/<recurso>/{id}/<verbo>`)", documentada al inicio de
  `docs/api.md`. Renombres (ruptura limpia, sin alias — API pre-release):
  `POST /v1/vms/{id}/snapshots` → `/v1/vms/{id}/snapshot` y
  `POST /v1/snapshots/{id}/vms` → `/v1/snapshots/{id}/fork` (mismo verbo y
  body que el fork directo `/v1/vms/{id}/fork`; solo cambia el origen).

**Batería de validación ejecutada (2026-07-04, todas OK)**:
1. VM + estado (fichero en disco, contador vivo en memoria) → snapshot 271ms; la VM siguió corriendo.
2. estado ensuciado → `restore` in-place 109ms → fichero rebobinado, contador reanudó en el valor congelado y siguió, misma IP respondiendo.
3. original viva → fork normal Y quarantine dan 409 (límite FC 1.10.1, mensaje con la solución).
4. original destruida → fork normal 113ms → IP del snapshot reclamada en `default`, ping OK, estado congelado presente.
5. fork parado (libera TAP) → fork `quarantine` → eth0 UP con la IP "fantasma", ni el gateway alcanzable, vsock OK.
6. restart del daemon → cuarentena adoptada (mismo PID), parada conservada, snapshot listado; snapshot sobre la VM adoptada OK.
7. `DELETE` del snapshot con su fork corriendo → 204 y el fork siguió vivo.
8. limpieza → cero huérfanos (taps, clones, jail dirs, snapshots).

---

## Fase 2 (parte 2) — Volúmenes + plano de datos seguro (2026-07-04, implementada + tests; arranque con volumen confirmado en hardware, batería completa pendiente)

**Objetivo**: meter y sacar datos de las VMs de forma segura, sin que el host se
vea afectado. Volúmenes persistentes (muestra RO + salida escribible para
artefactos) y transferencia de ficheros. Es la Fase 2 (volúmenes) + el "Update"
de Fase 3 del plan.

**Regla de seguridad rectora — el host NUNCA monta un filesystem del guest.**
`mount(2)` de un ext4 no confiable corre el parser ext4 del *kernel del host*
sobre bytes del atacante (la superficie de escape clásica). Todo el I/O
host-side va por **`debugfs`** (espacio de usuario, sin montar). Verificado con
grep: cero `mount` de imágenes del guest.

**Qué hay** (detalle en `docs/volumes.md` + `docs/api.md`):
- **Entidad `Volume`**: ext4 persistente en el store (`<store>/volumes/<id>.ext4`,
  `mkfs.ext4 -F` sin tabla de particiones como los goldens), sobrevive al destroy.
  CRUD `POST/GET/DELETE /v1/volumes`. Un volumen está adjunto a lo sumo a una VM
  (`AttachedTo`).
- **Adjuntar** = al crear la VM, campo `volumes:[{name,read_only,guest_path}]` de
  `POST /v1/vms` (Firecracker NO tiene hot-plug de discos → no hay attach a una
  VM viva; modelo prepare-then-attach). Se añade un `Drive` secundario por
  volumen (RO → `is_read_only`, el bloque rechaza escrituras de verdad) y el
  daemon lo **auto-monta por vsock** tras el boot en `/vol/<name>` (o `guest_path`).
- **Transferencia de ficheros** `PUT/GET /v1/vms/{id}/files?path=` — transparente
  al estado: VM viva → vsock (funciona sin red); VM parada → `debugfs` sobre el
  disco (ruta post-mortem, sin arrancar).
- **I/O offline en volumen suelto** `PUT/GET /v1/volumes/{id}/files?path=` — con
  `debugfs`, para prellenar una muestra antes de adjuntar o sacar artefactos tras
  el destroy. Solo con el volumen libre.
- Agente guest (`prepare-image.sh`) multiplexa 3 verbos en el puerto vsock 52:
  comando pelado (exec, contrato intacto), `PUT <path> <len>`, `GET <path>`.
- Snapshot/fork/restore **rechazan (409)** una VM con volúmenes: la RAM del
  snapshot tiene el volumen montado; restaurar sobre un volumen mutado lo
  corrompe (límite honesto de v1).

**Plano de datos en STREAMING de punta a punta (RAM constante)** — el primer
corte buffeaba el fichero entero (`io.ReadAll`, `[]byte`) → 4 GB = 4 GB de RAM
del host; y el staging de `debugfs` caía en `/tmp` (tmpfs = RAM). Reescrito:
subida/bajada streamean con búfer fijo, el staging de `debugfs` va al **store**
(disco real, nunca `/tmp`) y se barre al arrancar. Un dataset de varios GB no
carga la RAM. Test de regresión con payload de 40 MiB por hash.

**Bug de runtime cazado EN hardware** (no salta en build/vet/tests): el
`drive_id` de los volúmenes llevaba guion (`vol-<id>`) y Firecracker solo acepta
alfanuméricos y guion bajo → `PUT /drives` daba 400. Arreglado a `vol_<id>`.
Tras el arreglo, VM con volumen arranca y monta en `/vol/datos` OK (medido: ~3.3s
de create, dominado por esperar a que el agente vsock del guest esté listo para
auto-montar — inherente al diseño, solo afecta a VMs con volumen).

**Auditoría de seguridad** (a petición del usuario: que nada nuevo reviente el
air-gap ni el host):
- **Air-gap intacto**: los volúmenes son dispositivos de bloque (sin red); el
  vsock es control host→guest (el daemon solo *dial*ea el puerto 52 del guest,
  nunca escucha → guest→host por vsock no tiene dónde aterrizar; vsock no es
  salida de red).
- **Host intacto**: sin `mount(2)`, `debugfs` en espacio de usuario, sin
  hot-plug falso, staging en el store no en RAM.
- **Bug real encontrado + arreglado (TOCTOU introducido en el primer corte)**:
  el I/O offline comprobaba "volumen libre" bajo lock, lo soltaba y LUEGO corría
  `debugfs`; un `Create`/`Start` concurrente podía adjuntar+montar (o arrancar
  sobre el disco) en medio → `debugfs` escribiría un ext4 montado y lo
  corrompería. Cerrado con guards de "ocupado" por recurso (`volIO`/`vmIO` bajo
  el lock): attach y Start rechazan (409) mientras hay un `debugfs` en curso.
  Tests en `internal/vm/volume_guard_test.go`.
- **Riesgo residual señalado (no mitigado, recomendado para Fase 4)**: `debugfs`
  corre como **root** sobre ext4 controlado por el atacante (rootfs de una VM
  parada) — superficie mucho menor que `mount(2)` del kernel, pero un RCE en
  debugfs/libext2fs sería root en el host; recomendación: bajar `debugfs` al
  uid/gid del jailer (necesita permisos de staging + validación en hardware).
- Notas de modelo de amenaza (no bugs): un volumen escribible reusado entre VMs
  es un canal de datos intencionado entre ejecuciones; una subida enorme puede
  llenar el disco del store (DoS de almacenamiento, no escape).

**Ficheros clave**: `pkg/types/volume.go`, `internal/storage/volume.go` +
`offline.go` (debugfs, streaming), `internal/store` (tabla volumes),
`internal/vsock/exec.go` (PutFile/GetFileStream), `internal/vm/manager.go`
(CRUD, attach, auto-mount, guards, streaming), `internal/firecracker/machine.go`
(drives extra), `internal/api/server.go` (rutas), `scripts/prepare-image.sh`
(agente multi-verbo).

**PENDIENTE de validar en hardware** (batería completa): muestra RO inyectada
por debugfs → montada RO rechaza escritura; escribir en volumen RW desde el
guest → destroy → el volumen persiste y se lee de vuelta (probado el ciclo
destroy→re-attach conservando datos); fichero grande (~GB) por vsock con
checksum y RSS del daemon plana; GET post-mortem por debugfs sobre VM parada;
409 en snapshot-con-volúmenes.

**Decisión de ritmo (usuario)**: los ~3.3s del create-con-volumen se dejan como
están de momento (aceptable para el workflow de detonación). Palancas futuras si
molesta: sondeo del agente más fino (500ms→200ms) y/o adelantar el arranque del
servicio vsock en el guest.

---

## Observabilidad — `/v1/system` + `/v1/health` + consumo por VM (2026-07-05, implementada + tests; PENDIENTE validar en hardware)

**Objetivo (usuario)**: endpoint(s) para saber la salud general, el estado del
almacenamiento del host, la ubicación de todo lo importante (VMs, rutas) y el
consumo de RAM/CPU — pensado para usabilidad y funcionalidad.

**Diseño — dos consumidores, dos endpoints** (singletons de solo lectura, se
suman a la convención de rutas en `docs/api.md`):
- `GET /v1/health` — para un **monitor**: 6 checks (`kvm`, `database`,
  `store_writable`, `disk_space` con suelo max(5%, 1 GiB), `store_cow`,
  `firecracker`) y responde por **código HTTP** (200 `ok` / 503 `degraded`)
  para que systemd/uptime-checkers no parseen JSON. `degraded` en cuanto
  falla un check; cada check lleva `detail` con el motivo.
- `GET /v1/system` — para un **panel/operador**: informe completo en una
  llamada (siempre 200; la salud va embebida). Bloques: `daemon` (pid,
  uptime, versión de Firecracker, capacidad `network_overrides`, y `paths`
  con TODAS las rutas del host: store/goldens/kernels/snapshots/volúmenes/
  chroots/db/catálogo/binarios), `host` (cpus, load 1/5/15, RAM
  total/usada/disponible de `/proc/meminfo` — used=total−available),
  `storage` (statfs del store: fs_type/cow/total/usado/libre + breakdown
  estilo `du` por categoría con su ruta) y `fleet` (VMs por estado,
  `allocated{vcpus,mem_mb}` de las running = visibilidad de overcommit
  contra `host`, redes/snapshots/volúmenes attached/templates).
- **Consumo por VM** en cada `VMResponse` (esto convierte `GET /v1/vms` en el
  "`docker ps`" pendiente de Fase 3): `vcpus/mem_mb/disk_mb` (forma),
  `rootfs_path` (dónde está su disco) y, solo en running, `uptime_seconds`
  (desde el arranque del PROCESO, no created_at), `mem_rss_mb` (RAM residente
  real — lo que la VM cuesta al host AHORA; suele ir muy por debajo de
  `mem_mb` porque el guest paginia bajo demanda) y `cpu_seconds` (acumulada;
  un panel la diferencia entre sondeos para sacar %).

**Decisiones técnicas**:
- **Sin recolectores de fondo ni histórico**: todo se calcula al momento de la
  petición desde `/proc`, `statfs` y el estado en memoria del manager. El
  histórico es del cliente (sondea y diferencia) — el daemon responde "ahora"
  barato y honesto. Nuevo paquete `internal/hostinfo` (meminfo, loadavg,
  statfs con nombres de FS por magic, tamaños du-style por `st_blocks` — los
  sparse no engañan —, stats por PID de `/proc/<pid>/stat|statm` con parseo
  anclado al último `)` del comm).
- El breakdown de storage cuenta extents reflink una vez POR FICHERO: las
  categorías pueden sumar más que `used_mb`; cada cifra = "cuánto liberaría
  borrar esto como máximo" (documentado; `btrfs fi du` es la referencia).
- `store.Ping()` hace `SELECT 1` real (el Ping de database/sql puede pasar
  sobre un fichero roto: las conexiones son perezosas).
- `firecracker.Version()` (string completa) separado de
  `SupportsNetworkOverrides` (capacidad); versión sondeada una vez al montar
  las rutas, capacidad expuesta en `daemon.network_overrides` porque su
  ausencia explica 409s de fork que de otro modo desconciertan.
- El informe host degrada con elegancia: una lectura de `/proc` que falle
  deja ceros en su bloque, nunca un 500 — el endpoint para diagnosticar
  problemas no puede caerse por el problema.
- `/v1/vms` enriquece con stats vivas vía `liveVMResponse` en la capa API
  (best-effort: si el proceso murió entre listar y leer, la VM sale sin
  stats); `NewVMResponse` se mantiene puro (sin I/O en `pkg/types`).

**Ficheros**: `internal/hostinfo/` (nuevo, con tests de parseo), 
`pkg/types/system.go` (DTOs), `internal/api/system.go` (rutas + checks +
informe), `internal/vm/system.go` (`Facts`/`FleetStats`/`CheckDB`/
`StoreIsCoW`), `store.Ping`, `firecracker.Version`, `VMResponse` ampliado,
`NewServer` recibe `api.SystemConfig{DBPath,CatalogPath,StartedAt}`.
Tests: `internal/api/system_test.go` levanta el mux real con manager real
(store/catálogo temporales, sin root) y verifica la forma del informe y que
sin binario de Firecracker el estado es `degraded` con 503 en `/v1/health`.
`docs/api.md` → sección "Observabilidad" completa con ejemplo.

**PENDIENTE validar en hardware** (el binario nuevo no se pudo instalar en la
sesión — `make install-service` pide sudo interactivo): tras reinstalar,
`curl /v1/health` (200 ok con checks verdes), `curl /v1/system | jq` (rutas y
breakdown coherentes con el store real, `cow: true`, fleet cuadra con
`/v1/vms`), y en `GET /v1/vms` de una VM corriendo: `mem_rss_mb` < `mem_mb`,
`uptime_seconds` razonable, `cpu_seconds` creciendo entre sondeos.

---

## Hardening — límites cgroup v2 por VM + debugfs sin root (2026-07-05, implementada + tests; PENDIENTE validar en hardware)

**Objetivo (usuario)**: (1) escribir límites de CPU, memoria y PIDs en el
cgroup de Jailer de cada VM para que un guest no pueda hacer DoS al host;
(2) bajar los privilegios del `debugfs` host-side al uid/gid del jailer para
mitigar escapes a nivel de parser ext4.

**Límites cgroup v2** (`internal/jailer/cgroup.go`):
- El SDK v1.0.0 no permite pasar `--cgroup` extra al jailer (solo emite el
  par cpuset de NUMA), así que los escribe el **daemon** justo tras el launch
  (con el PID ya conocido), en `/sys/fs/cgroup/firecracker/<vm-id>` — el
  layout de Jailer con `--cgroup-version 2` (parent-cgroup = basename del
  exec-file, espejo de la fórmula del chroot).
  **NOTA (2026-07-05, mismo día por la tarde): este layout resultó ser el
  origen de un bug grave en ARM y se cambió a `/sys/fs/cgroup/microhosted/<id>`
  — ver la sesión "Portabilidad ARM" más abajo.**
- Valores dimensionados por la config de la propia VM: `cpu.max` =
  vCPUs × período completo (100 ms), `memory.max` = MemMB + 64 MiB de margen
  VMM (heap/virtio/page tables; el overhead documentado de FC es <5 MiB),
  `memory.swap.max=0` (una VM capada debe chocar con su límite, no empujar
  al host a swap), `pids.max` = vCPUs + 16 (el controller cuenta threads;
  FC corre 1 por vCPU + VMM + API y nunca forkea → un fork bomb del VMM
  comprometido muere en EAGAIN).
- Robusto ante que Jailer no cree el grupo (solo lo hace si recibe algún
  `--cgroup`): `ApplyLimits` hace MkdirAll + habilita `+cpu +memory +pids`
  en `subtree_control` de root y del padre (Jailer solo habilita lo que
  escribe: cpuset) + inscribe el PID en `cgroup.procs` él mismo (re-attach
  al mismo grupo es no-op si Jailer ya lo movió).
- **Fail-closed**: si los límites no se pueden aplicar, el boot se aborta y
  el proceso se mata (una VM sin capar es exactamente el DoS que esto
  cierra). Única excepción: host con cgroups v1 (`ErrCgroupV1`) → warning
  ruidoso por boot y arranca sin límites (comportamiento pre-feature).
- Aplica en los dos caminos de arranque (`boot` y `bootFromSnapshot` — los
  forks/restores heredan el sizing del snapshot).
- **Limpieza**: Jailer nunca borra su cgroup → `RemoveInstanceDir` ahora
  también hace `RemoveCgroup` (rmdir con reintentos ante EBUSY: la salida
  del proceso es asíncrona al SIGTERM), así todos los caminos de teardown
  (Destroy/Stop/rollbacks/sweep de Reconcile) lo heredan y se cierra una
  fuga que ya existía.

**debugfs sin root** (`internal/storage/offline.go`):
- `InjectFile/ExtractFileStream/ExtractDir` pasan a métodos de
  `OfflineIO{StagingDir, UID, GID}`; cuando el daemon corre como root, cada
  `debugfs` corre con `Credential{uid,gid}` del jailer y `Groups: []` (sin
  los grupos suplementarios de root). Un exploit del parser ext4 aterriza en
  un proceso sin privilegios, no en root. Es **mitigación, no jaula**
  (comparte namespaces del host): elimina el premio de shell root, no la
  superficie.
- Funciona porque las imágenes ya son de ese uid (clones y volúmenes se
  chownean al crearse); los temporales del staging (fichero de comandos,
  fuente de inject, destino de dump/rdump) se chownean al uid en
  `stageFile`/`grant` manteniendo 0600. En ejecuciones sin root (tests) no
  hay drop: debugfs corre como el usuario del daemon, que ya es dueño de lo
  que crea.
- El manager construye el contexto en `offlineIO()` (staging del store +
  uid/gid del jailer) — los 4 call sites (PutFile/GetFileStream offline,
  Inject/Extract de volúmenes) lo usan.

**Ficheros**: `internal/jailer/cgroup.go` (nuevo) + `cgroup_test.go` (nuevo;
root de cgroup falseado con temp dir — la parte kernel es territorio de
validación en hardware), `internal/jailer/config.go` (`RemoveInstanceDir`
integra `RemoveCgroup`), `internal/storage/offline.go` (refactor a
`OfflineIO` + drop de privilegios), `internal/vm/manager.go` (`applyLimits`
fail-closed en ambos boots, `offlineIO()`), tests de storage adaptados.
`docs/layers.md` (L2: límites) y `docs/volumes.md` (drop de debugfs)
actualizados.

**PENDIENTE validar en hardware** (los tests unitarios no ejercitan el
kernel): tras reinstalar el binario —
- Crear una VM y comprobar `cat /sys/fs/cgroup/microhosted/<id>/{cpu.max,memory.max,memory.swap.max,pids.max,cgroup.procs}`
  (valores dimensionados y el PID de firecracker inscrito; ruta actualizada
  tras el fix de la sesión "Portabilidad ARM").
- Dentro del guest: `yes > /dev/null &` × N no debe pasar del % de CPU de sus
  vCPUs en el host (`top`); un `stress` de memoria debe morir por OOM del
  cgroup sin tocar el swap del host.
- Destroy/Stop → el directorio del cgroup desaparece (no se acumulan).
- `PUT`/`GET` de ficheros con VM parada y con volumen suelto: `ps -o user`
  del proceso debugfs durante una transferencia grande = uid del jailer, y
  los round-trips siguen funcionando (permisos del staging correctos).
- Host cgroups v1 (si hay alguno): la VM arranca con el warning en el log.

---

## Portabilidad ARM — Bug #9: "la primera VM arranca, todas las siguientes fallan" (2026-07-05, VALIDADO EN HARDWARE — Raspberry Pi, kernel 6.17 raspi)

**Contexto**: primera prueba de la plataforma completa en aarch64 (Raspberry
Pi). En x86 todo iba bien. `make full-install` + `make prepare-image`
funcionaron; la **primera** microVM arrancó, respondió a `exec` por vsock…
y a partir de ahí **todo `POST /v1/vms` fallaba** con
`Firecracker did not create API socket … exit status 1`, sobreviviendo a
reinicios del daemon. Solo un reboot del host devolvía exactamente UNA VM más.

**Cómo se encontró**: el journal solo decía "exit status 1"; el stderr real
del jailer estaba en el log por-VM (`/var/lib/microhosted/store/<id>.log`):
`CgroupMove("/sys/fs/cgroup/firecracker") Resource busy (EBUSY)`.

**Cadena de causas (las cuatro piezas)**:
1. El kernel de la Pi no tiene NUMA → no existe `/sys/devices/system/node`.
   El firecracker-go-sdk solo emite sus flags `--cgroup cpuset.*` si puede
   leer `node0/cpulist` → **en ARM el jailer arranca sin ningún `--cgroup`**.
   (En x86 el fichero existe: por eso ahí nunca se manifestó.)
2. Sin flags `--cgroup`, el jailer (v1.16.1) no crea el cgroup hijo por VM:
   mete el proceso **directamente en el padre** `/sys/fs/cgroup/firecracker`.
3. `ApplyLimits` (hardening de esta misma mañana) habilitaba `+cpu +memory
   +pids` en el `subtree_control` de ese MISMO padre para poder poner límites
   en el hijo.
4. Regla cgroup v2 de **"no internal processes"**: un cgroup con
   controladores delegados en `subtree_control` no admite procesos directos.
   La primera VM entra con el padre limpio; `ApplyLimits` lo "envenena"; cada
   jailer posterior recibe EBUSY y muere antes de crear el socket. El estado
   persiste hasta el reboot — por eso reiniciar el daemon no ayudaba.

**El fix (general, no un parche ARM)**: el árbol de límites se muda a un
cgroup propiedad del daemon, **`/sys/fs/cgroup/microhosted/<id>`**, en vez de
imitar la convención interna del jailer. Así el `subtree_control` de
`/sys/fs/cgroup/firecracker` no se toca jamás → el attach del jailer al padre
siempre funciona, con 1 o con 50 VMs; `ApplyLimits` migra el PID desde donde
lo dejara el jailer (root siempre puede migrar). Un solo camino de código para
x86 y ARM: se elimina la dependencia oculta de "existe NUMA sysfs". Costes
asumidos y documentados: en hosts NUMA la migración pierde el pinning cpuset
del jailer (no-op en máquinas de un solo nodo, que son todos los objetivos), y
`RemoveCgroup` ahora limpia DOS directorios (el de límites + el hijo
`firecracker/<id>` que el jailer sí crea en x86, que si no se fugaría).

**Ficheros**: `internal/jailer/cgroup.go` (layout nuevo + `jailerCgroupDir` +
`RemoveCgroup` doble, con el porqué completo en el comentario de paquete),
`cgroup_test.go` (layout nuevo + assert explícito de que `ApplyLimits` NO toca
el padre del jailer — el test codifica el invariante anti-regresión).

**Operativo**: el fix necesita una limpieza única del estado envenenado de la
sesión en curso (`echo -cpu/-memory/-pids > .../firecracker/cgroup.subtree_control`
tras borrar hijos huérfanos) o un reboot; después ya no puede reproducirse.

**VALIDADO en hardware**: tras instalar el binario y limpiar, múltiples VMs
consecutivas arrancan en la Pi (con los límites fail-closed activos — si
`ApplyLimits` fallara, el boot abortaría, así que el camino nuevo de cgroups
queda ejercitado en cada create). El usuario confirmó "ya funciona".
Queda pendiente (heredado de la sesión de Hardening) verificar los VALORES de
los límites en `/sys/fs/cgroup/microhosted/<id>` y el resto de su batería.

**Lección para el registro**: `CgroupDir` funcionaba en x86 *por
coincidencia* — replicaba la convención de nombres del jailer y asumía un
comportamiento (crear el hijo) que depende de flags que el SDK solo emite a
veces. Cuando un componente externo es dueño de un recurso (el cgroup padre
del jailer), no se le comparte el `subtree_control`: árbol propio y migrar.

---

## Sesión 7 — API propia (pendiente)

**Objetivo**: HTTP/gRPC local para gestionar sandboxes.

**Estado**: pendiente

---

## Sesión 8 — Estado persistente (pendiente)

**Objetivo**: SQLite para sobrevivir reinicios del proceso.

**Estado**: pendiente

---

## Sesión 9 — Multi-host (pendiente)

**Objetivo**: 2+ hosts, selector de capacidad, detección de host caído.

**Estado**: pendiente

---

## Sesión 10 — Prueba de resistencia (pendiente)

**Objetivo**: N ciclos sin fugas. Cierre de etapa de viabilidad del core.

**Estado**: pendiente
