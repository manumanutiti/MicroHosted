# MicroHosted — Orquestador propio de microVMs

## Por qué existe

Necesidad real sin cubrir: **aislamiento fuerte y auto-alojado** para cargas efímeras
(agentes de IA, CI, entornos de prueba) sin depender de un tercero (E2B, Daytona,
Modal...) y sin la sobrecarga de VMs tradicionales.

Proxmox lleva desde 2020 con peticiones abiertas de soporte de microVMs sin plan de
implementarlo. El único intento reciente (`pve-microvm`) es un parche experimental sobre
QEMU, no una herramienta nativa de Firecracker+Jailer.

**El hueco**: herramienta pequeña y propia, con el modelo de seguridad real de Jailer
(chroot + cgroups + seccomp), pensada desde cero para cargas efímeras aisladas.

No compite con Nomad, Kubernetes ni startups grandes. Alcance controlado, problema
concreto, puede crecer si demuestra que funciona.

---

## Alcance de la primera etapa

**Solo viabilidad del núcleo técnico**: crear, usar y destruir microVMs aisladas de
forma programática y repetible.

Fuera de alcance por ahora: interfaz web, multi-tenencia seria, alta disponibilidad.

---

## Stack técnico

| Componente | Elección | Razón |
|---|---|---|
| Lenguaje | Go | `firecracker-go-sdk` oficial evita escribir el cliente HTTP desde cero |
| Hipervisor | Firecracker | Arranque < 125ms, footprint mínimo, modelo de seguridad serio |
| Aislamiento | Jailer | chroot + cgroups v2 + seccomp — el modelo real, no un workaround |
| Red (etapa 1) | TAP + IP estática | Sin CNI por ahora, sin overhead |
| Comunicación host↔guest | vsock | Patrón de toda plataforma de sandboxing seria |
| Estado | SQLite → Postgres | SQLite primero, migrar cuando haya multi-host |

---

## Tres conceptos fundamentales (entender antes de escribir código)

**1. API de Firecracker**

Firecracker se controla con una API REST sobre un socket Unix. No hay CLI de "arrancar
VM". Todo es `PUT`/`PATCH` a endpoints:

```
PUT /boot-source      # kernel + cmdline
PUT /drives/rootfs    # disco raíz
PUT /machine-config   # vCPUs, memoria
PUT /network-interfaces/eth0  # red
PUT /actions          # InstanceStart / SendCtrlAltDel / FlushMetrics
```

**2. Layout de Jailer**

```
/srv/jailer/<exec-file>/<id>/root/
```

- `jailer` y `firecracker` deben ser binarios de la **misma versión exacta**
- El socket de la API vive dentro del chroot
- Jailer monta `/dev/kvm`, el socket, etc. dentro del chroot antes de ejecutar

**3. Comunicación host↔guest**

Para todo lo que no sea red (mandar comandos a ejecutar, leer resultados) se usa
**vsock**, no red normal. Es el patrón que usan E2B, Kata Containers,
firecracker-containerd. El guest escucha en un puerto vsock; el host conecta al CID
de la VM.

---

## Requisitos del host

- Linux con `/dev/kvm` accesible (`ls -la /dev/kvm`)
- Kernel 5.10+ (recomendado 6.x)
- cgroups v2 montados (`mount | grep cgroup2`)
- Capacidad de crear TAP devices (`ip tuntap`)
- Go 1.22+
- `firecracker` y `jailer` del mismo release (instalados por `scripts/install-fc.sh`)

Para verificar KVM:
```bash
[ -r /dev/kvm ] && [ -w /dev/kvm ] && echo "OK" || echo "Falta permisos o KVM"
```

---

## Estructura del repositorio

```
MicroHosted/
├── cmd/
│   └── microhosted/       # Punto de entrada del binario principal
│       └── main.go
├── internal/
│   ├── firecracker/       # Wrapper del SDK + llamadas al API REST
│   ├── jailer/            # Configuración del chroot y lanzamiento con Jailer
│   ├── network/           # TAP devices, asignación de IPs, limpieza de red
│   ├── storage/           # Gestión de kernels y rootfs
│   ├── vm/                # Ciclo de vida de VMs (create/destroy/snapshot)
│   └── api/               # Servidor HTTP/gRPC expuesto hacia afuera
├── pkg/
│   └── types/             # Tipos compartidos (VMConfig, VMState, etc.)
├── scripts/
│   ├── install-fc.sh      # Descarga firecracker + jailer (misma versión)
│   ├── build-kernel.sh    # Compila kernel mínimo para microVMs
│   ├── build-rootfs.sh    # Genera rootfs.ext4 desde una definición
│   └── setup-host.sh      # Configura el host (cgroups, permisos, TAP, etc.)
├── images/
│   ├── kernels/           # vmlinux versionados (.gitignore los binarios)
│   └── rootfs/            # rootfs.ext4 base (.gitignore las imágenes)
├── sessions/              # Notas técnicas por sesión de trabajo
│   └── session-00.md
├── docs/
│   └── architecture.md
├── PROJECT.md             # Este archivo
├── SESSIONS.md            # Registro de avance por sesión
├── Makefile
├── .gitignore
├── go.mod
└── go.sum
```

> **Nota sobre imágenes**: Los binarios grandes (kernels, rootfs) no se commitean.
> Se descargan o construyen localmente con los scripts de `scripts/`.

---

## Roadmap por sesiones

### Sesión 0 — Arranque manual, sin código
Conseguir `firecracker` y `jailer` (misma versión). Descargar kernel mínimo y rootfs
de ejemplo. Arrancar una microVM **a mano** con `curl` contra el socket Unix.

**Criterio de éxito**: entrar por consola serie a una microVM levantada a mano y
apagarla limpio. Si no se puede hacer esto sin fallos, no seguir — depurar aquí.

---

### Sesión 1 — Cliente propio del API de Firecracker (sin Jailer)
Primer código: programa que reemplaza los `curl` de la Sesión 0 usando
`firecracker-go-sdk`. Parámetros: ruta del kernel, rootfs, vCPUs, memoria.

**Criterio de éxito**: `./microhosted create --kernel ... --rootfs ...` levanta VM;
`./microhosted destroy <id>` la apaga y limpia.

---

### Sesión 2 — Envolver con Jailer
Agregar la capa de aislamiento real: chroot, cgroups, lanzar Firecracker vía Jailer.

**Criterio de éxito**: `ps`/`nsenter` confirma que el proceso de Firecracker no puede
ver el resto del filesystem del host.

---

### Sesión 3 — Red para una sola microVM
TAP device + configuración de IP estática (sin CNI todavía).

**Criterio de éxito**: `ping`/SSH desde el host a la microVM.

---

### Sesión 4 — Concurrencia: varias microVMs simultáneas
Generalizar sesiones 1-3: sockets únicos, IDs únicos, IPs únicas, cgroups únicos,
limpieza correcta al destruir.

**Criterio de éxito**: 5 microVMs simultáneas, cada una accesible por separado,
destruir una no afecta a las demás ni deja recursos huérfanos.

---

### Sesión 5 — Pipeline de imágenes reproducible
Automatizar construcción de kernel mínimo y rootfs propios (no los de ejemplo de AWS),
con plantillas versionadas.

**Criterio de éxito**: script que, a partir de una definición simple (paquetes,
comandos de setup), genera `rootfs.ext4` + `vmlinux` listos para usar.

---

### Sesión 6 — Snapshot / restore
Implementar pausa + snapshot + restauración para arranques casi instantáneos.

**Criterio de éxito**: medir y comparar tiempo desde snapshot frente a arranque en
frío. Referencia de la industria: ~150-200ms restaurando snapshot.

---

### Sesión 7 — Agente de host con API propia
Envolver todo detrás de una API HTTP/gRPC local: `POST /sandboxes`,
`DELETE /sandboxes/:id`, `GET /sandboxes`. Primer componente que pasa de "script"
a "servicio".

**Criterio de éxito**: crear, listar y destruir sandboxes solo hablando con la API,
sin invocar Firecracker/Jailer directamente desde afuera.

---

### Sesión 8 — Estado persistente
Guardar estado en SQLite (luego Postgres), no en memoria.

**Criterio de éxito**: reiniciar el proceso y recuperar correctamente qué sandboxes
existían, sin perder ni duplicar estado.

---

### Sesión 9 — Multi-host y selector simple
Extender a 2+ hosts físicos. Selector simple: "el host con más capacidad libre".

**Criterio de éxito**: con 2 hosts, el sistema coloca sandboxes en el que tiene
capacidad y detecta si un host deja de responder.

---

### Sesión 10 — Cierre de etapa: prueba de resistencia
Bucle de creación/destrucción repetida, medición de fugas (cgroups, TAP devices,
sockets, memoria) y tiempos reales.

**Criterio de éxito**: N ciclos de creación/destrucción sin fugas ni caídas.
Aquí termina la etapa de "viabilidad del core".

---

## Después de esta etapa (referencia futura)

Interfaz web tipo panel, multi-tenencia y aislamiento de red entre clientes,
observabilidad/métricas, hardening de seguridad más profundo, y — si todo funciona
bien — evaluar si darle forma de proyecto open source público.

---

## Dirección refinada (2026-07-01)

Tras validar el núcleo (sesiones 1-3 + acceso vsock/SSH), se afina el rumbo y el
público objetivo.

### Dos casos de uso, un mismo núcleo
- **Sandboxing de ciberseguridad / forense**: detonar binarios no confiables,
  análisis de malware, ejecutar Volatility u otras herramientas sobre volcados,
  recoger artefactos (pcaps, dumps de memoria) — todo con aislamiento de
  hipervisor real y revert a estado limpio entre muestras.
- **Sandboxing para agentes de IA**: ejecución de código aislada y efímera,
  auto-alojada, sin mandar código/datos a un tercero (E2B/Daytona/Modal).

Ambos comparten el mismo requisito: correr algo no confiable sin que toque el
host. Endurecer para malware da gratis el aislamiento que el caso IA también
quiere. (Cuotas de GPU para IA y un agente de prueba tipo ONNX quedan aparcados
para más adelante — primero el core.)

### Refinamiento del posicionamiento (2026-07-02)

El encuadre madura de "dos casos de uso" a un **posicionamiento único**:
**plataforma de sandboxing de seguridad AUTOHOSTED**. Separar en dos capas:

- **El motor** (ciclo de vida de microVMs + aislamiento + snapshots): general y
  neutral, agnóstico al caso de uso. La calidad se mide en correctitud y
  garantías.
- **El producto**: una **librería curada de imágenes desechables** para distintos
  usos defensivos (detonación de malware, honeypots/deception, bancos DFIR,
  rangos blue-team, labs/CTF; el sandbox de agentes IA es un segundo acto sobre
  el mismo motor).

Claves estratégicas:
- **Foso = soberanía del dato.** El autohosted es el eje donde los sandboxes
  cloud (ANY.RUN, Joe Sandbox, e2b) no pueden competir por estructura: no mandas
  la muestra/el dato a un tercero. Para banca/defensa/sanidad/air-gap es un "no"
  rotundo, no una preferencia.
- **Incumbente a desplazar = CAPEv2/Cuckoo** (sandbox de malware self-hosted
  clásico, QEMU pesado, doloroso de operar). Ángulo: el sucesor moderno en
  microVMs Firecracker, API-first, que sí se instala.
- **Disciplina: motor general, primer workflow afilado.** "Biblioteca para
  distintos usos" es la promesa, no el lanzamiento. Se lanza con UN workflow
  hondo (detonación: muestra → VM aislada sin egress → corre → captura artefactos
  → reset a limpio) y se expande desde ahí. Amplitud = promesa; profundidad-de-uno
  = prueba.
- **Consecuencias**: el catálogo/sistema de imágenes sube a activo de primera
  clase (templates versionadas, manifests, builds reproducibles, firma) — enlaza
  con las imágenes ultra-optimizadas pendientes (Alpine/Rocky). **Snapshots** son
  lo más estratégico (reset-a-limpio, bifurcar en el punto de infección), por
  encima de multi-host/HA. **Modelo de amenaza escrito + tests adversariales**
  pasa a ser argumento de venta, no higiene.
- **Orden**: control plane, colas, HA y multi-host son etapas posteriores; su
  orden lo dicta un caso de uso que tira, no una checklist de escalar. Siguiente:
  soak test (robustez en operación) → snapshots → workflow vertical de detonación.

### Decisiones de arquitectura fijadas
- **Red segmentada por nombre** (no el `/30` punto-a-punto actual, donde el host
  es gateway de todas las VMs y dos VMs no pueden verse). Bridge por red nombrada;
  las VMs se unen por nombre; `nftables` fuerza por defecto: `guest→host`
  bloqueado, cross-segment bloqueado, salida a internet como flag por red (deny
  por defecto = malware-safe; opt-in NAT). Un solo mecanismo cubre inter-VM Y
  aislamiento.
- **Persistencia SQLite** desde ya: un backend que olvida su estado al reiniciar
  no es "funcional". Reemplaza el estado en memoria del `Manager` y el `Allocator`.

### Plan del backend funcional (por dependencias)

**Fase 0 — Fundamento: limpieza total + persistencia**
- Auditar `Destroy`: hoy NO borra el chroot de Jailer
  (`/srv/jailer/<exec>/<id>/`) — fuga de disco en bucles crear/destruir. Cerrarla
  y auditar todos los caminos de fallo.
- SQLite: persistir VMs/redes/volúmenes; reconciliar procesos vivos al arrancar.
- Límites cgroup por VM (CPU/mem/pids): evita fork-bomb / agotar el host.

**Fase 1 — Red segmentada** (rediseño de `internal/network`)
- Entidad `Network` (nombre, subred, `egress`) + CRUD `/v1/networks`.
- Bridge por red; TAP de cada VM enslavado al bridge; IPAM por-red.
- `nftables`: drop guest→host, drop cross-segment, NAT condicional.
- `NoNetwork` sigue válido para el sandbox más hermético.

**Fase 2 — Almacenamiento**
- *(hecho, validado en hardware 2026-07-02)* **Tamaño de disco configurable**
  (`disk_mb` en template/request; el clon se agranda con `resize2fs`) + **store
  copy-on-write** (loopback btrfs, agnóstico al host, provisionado por
  `setup-host.sh`). Todo lo que Jailer clona/hardlinka (golden, kernel, chroot)
  vive en el mismo btrfs por invariante — reflink y hardlink no cruzan FS. Ver
  `SESSIONS.md` (Fase 2 parte 1) y `docs/layers.md` L3.
- *(hecho)* Entidad `Volume` (nombre, tamaño, ext4 persistente que sobrevive
  al destroy) + CRUD `/v1/volumes`; attach como drive extra (Firecracker
  `Drives`), read-only o writable, auto-montado en el guest por vsock. Ciber:
  muestra montada read-only + volumen de salida writable para artefactos. Una VM
  con volúmenes no puede snapshotear/fork/restore en v1 (409). Ver
  `docs/volumes.md`.

**Fase 3 — Completar el CRUD**
- *(hecho)* Update: inyectar/extraer archivos a una VM o volumen. Vía vsock si
  la VM está viva (`PUT/GET /v1/vms/{id}/files`, canal de datos en volumen sin
  red); vía `debugfs` **sin montar** si está parada o el volumen está suelto
  (`/v1/volumes/{id}/files`). Regla de seguridad: el host NUNCA monta un fs del
  guest — `mount(2)` de una imagen no confiable expone el parser ext4 del kernel
  del host. Ver `docs/volumes.md`.
- Read estilo `docker ps`: estado, imagen base, red, volúmenes.

**Fase 4 — Endurecimiento + prueba de resistencia**
- Verificar seccomp, caps cgroup, egress denegado por defecto.
- Test de escape (no ve FS del host, no alcanza host ni otra red, fork-bomb no
  tumba el host) + N ciclos crear/destruir sin fugas.

---

## Referencias clave

- [Firecracker GitHub](https://github.com/firecracker-microvm/firecracker)
- [firecracker-go-sdk](https://github.com/firecracker-microvm/firecracker-go-sdk)
- [Firecracker Getting Started](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md)
- [Jailer documentation](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md)
- [firecracker-containerd](https://github.com/firecracker-microvm/firecracker-containerd) (referencia arquitectural)
