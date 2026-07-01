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

- **Fase 0 — Fundamento (EN CURSO)**: limpieza total en `Destroy` (chroot de
  Jailer incluido — hecho), persistencia SQLite + reconciliación por PID
  (hecho, unit-tested), despliegue systemd (hecho — ver abajo), y límites
  cgroup por VM (pendiente, necesita la ruta real del cgroup en el host).

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
- **Fase 1 — Red segmentada**: `Network` + CRUD, bridge por red, IPAM por-red,
  `nftables` (drop guest→host, drop cross-segment, NAT condicional).
- **Fase 2 — Volúmenes**: `Volume` + CRUD, attach como drive extra; muestra RO +
  salida writable para artefactos.
- **Fase 3 — Completar CRUD**: Update (inyectar archivo/playbook) + Read estilo
  `docker ps`.
- **Fase 4 — Endurecimiento + prueba de resistencia**.

`docs/api.md` se mantiene al día según se añaden/cambian endpoints.

---

## Sesión 4 — Concurrencia (pendiente)

**Objetivo**: 5 microVMs simultáneas sin colisiones ni fugas.

**Estado**: pendiente

---

## Sesión 5 — Pipeline de imágenes (pendiente)

**Objetivo**: scripts reproducibles para construir kernel y rootfs propios.

**Estado**: pendiente

---

## Sesión 6 — Snapshot/restore (pendiente)

**Objetivo**: arranque desde snapshot < 200ms.

**Estado**: pendiente

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
