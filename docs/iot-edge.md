# Gateway de aislamiento IoT/OT

> Documento de diseño del pivote de nicho (2026-07-05). Contexto estratégico en
> `PROJECT.md` → "Pivote de nicho". Este documento fija el patrón, mapea qué
> existe ya en el código, enumera los huecos y define el plan de sesiones con
> criterios de éxito.

## El problema

En un gateway edge tradicional (el concentrador local de decenas de sensores:
ESP32, cámaras, PLCs), la superficie de ingesta vive en el host:

- El broker/parser (MQTT, HTTP, CoAP, Modbus) corre en espacio de usuario del
  host. Una trama maliciosa que explote el parser toma el gateway entero — y
  desde ahí, la planta.
- Los contenedores (Greengrass, balena, KubeEdge) no arreglan esto: comparten
  kernel. Una escalada de privilegios de kernel compromete todo.
- Las VMs tradicionales (QEMU) no caben: demasiada RAM/disco para un gateway
  de 2-8GB.

## El patrón: proxy de telemetría aislado

Una microVM-parser desechable por sensor (o por grupo pequeño de sensores).
El host **no expone puertos hacia los sensores y no parsea ningún protocolo**;
los datos ya validados salen de la VM por vsock.

```
[ Sensores físicos / red ]
       │            │
       ▼            ▼
  ┌────────────────────────────────────────────────────┐
  │ EDGE GATEWAY (host)                                │
  │                                                    │
  │  ┌───────────────┐  ┌───────────────┐              │
  │  │ microVM 1     │  │ microVM 2     │   … × N      │
  │  │ parser MQTT   │  │ parser Modbus │              │
  │  └───────┬───────┘  └───────┬───────┘              │
  │          │ vsock            │ vsock                │
  │          ▼                  ▼                      │
  │  ┌──────────────────────────────────────────────┐  │
  │  │ microhosted daemon (sin listeners hacia OT)  │  │
  │  └──────────────────────┬───────────────────────┘  │
  └─────────────────────────┼──────────────────────────┘
                            ▼
                   [ BD local / nube ]
```

Garantía: si un sensor comprometido explota su parser, compromete una VM de
~32MB, sin red hacia el host (guest→host ya es DROP por diseño), que se
regenera desde snapshot en ~100ms (medido: restore 109ms, fork 113ms).

## Modos de ingesta

### Modo pull (primero) — el nativo de OT

La VM interroga al sensor; el sensor nunca inicia nada. **Modbus, OPC-UA,
M-Bus y la mayoría de protocolos industriales ya son maestro/esclavo con el
gateway como maestro** — no imponemos un modelo raro, ponemos hipervisor
debajo del flujo que las plantas ya usan.

Ciclo: `restore desde snapshot → la VM interroga a SU sensor → valida/parsea
→ entrega resultado por vsock → destroy`.

- No existe camino de entrada en ningún momento: ni listener, ni DNAT, nada.
- Requiere **egress de grano fino**: la VM solo puede alcanzar
  `IP_sensor:puerto`, nada más (hoy egress es un booleano por red — hueco #1).
- La extracción usa el canal ya existente (`vsock.Exec`/`GetFileStream`,
  host→guest): el host *recoge* el resultado, el guest no puede iniciar nada
  hacia el host. Coherente con el modelo de seguridad actual.

### Modo push (después) — para MQTT/HTTP

El sensor inicia la conexión y la VM debe existir para recibirla. El problema:
algo tiene que ver llegar la conexión antes de que la VM exista, **sin parsear
un solo byte**:

1. nftables marca el primer SYN hacia el puerto de ingesta y lo entrega al
   daemon vía NFQUEUE/NFLOG (metadatos L3/L4 del kernel, cero payload).
2. El daemon restaura la VM del sensor (por IP origen) e instala el DNAT.
3. El SYN retransmitido del sensor (~1s, automático en TCP) aterriza ya dentro
   de la VM. Los bytes del payload nunca tocan espacio de usuario del host.

Latencia efectiva ~1s en frío — irrelevante para telemetría cada 30s. Para
sensores de alta frecuencia: VM caliente por sensor (modo "por anomalía").

**Anti-DoS obligatorio**: un atacante que spamea SYNs provoca tormenta de VMs.
Tope global de VMs concurrentes + rate-limit por IP origen + circuit breaker
por sensor (si su VM muere N veces seguidas, cuarentena y alerta — eso ES la
detección de compromiso).

### Cable físico (RS-485 / Modbus RTU / USB serie)

Firecracker no hace passthrough de dispositivos de caracteres. Solución:
**puente serie ciego** — daemon del host que copia bytes crudos
`/dev/ttyUSB0 ↔ vsock` de la VM correspondiente, sin ninguna lógica de
protocolo (cero superficie de parsing en el host). El parser Modbus corre
dentro de la VM. El `dial()` + handshake CONNECT de `internal/vsock` ya es la
mitad del trabajo.

## Ciclo de vida transaccional: el dial del producto

Los tres modos usan la misma maquinaria (snapshot/restore/kill ya validada en
hardware); el operador elige el punto de la curva seguridad/coste por sensor:

| Modo | Vida de la VM | Cuándo |
|---|---|---|
| Por transacción | restore → 1 lectura → destroy | datos críticos o lentos (≥30s entre lecturas) |
| Por ventana | vive N segundos, procesa el lote, destroy | telemetría frecuente |
| Por anomalía | persistente; reset a snapshot programado (higiene: cada N horas) o reactivo (watchdog) | alta frecuencia / baja latencia |

Regla de dimensionado: a 1 lectura/s × 100 sensores, "por transacción" son 100
restores/s — churn absurdo; ahí se usa ventana o anomalía. El framing de
producto: **el parser es desechable; el compromiso no puede persistir** —
ningún contenedor ni edge-stack actual regenera el aislamiento, solo lo
mantienen.

El watchdog del modo reactivo cierra el bucle "aislar comportamientos":
VM que no responde al poll vsock / excede su cgroup / se comporta raro →
`Kill` + restore a limpio + evento de alerta. Todo el material existe; falta
el controlador.

## Densidad: la aritmética honesta

El techo no es CPU (sensores casi siempre idle + `cpu.max` por VM ya
implementado) ni disco (reflink: 236KiB exclusivos medidos por clon). Es
**RAM del guest**. Y ojo: los "<5MB por microVM" de Firecracker son el
overhead del VMM, no la RAM del guest.

| Nivel de ingeniería | RAM incremental/VM | Pi 5 8GB (~7GB útiles) |
|---|---|---|
| Imagen actual (ubuntu, 128MB) | ~130-190MB | ~40-50 |
| Kernel tinyconfig + init estático + parser | ~20-32MB | ~200-300 |
| + restore masivo desde snapshot compartido | ~5-15MB sucios | ~300-500 |

La palanca del tercer nivel **ya está implementada**: el fichero de memoria
del snapshot se restaura con backend File = mapeo copy-on-write
(`internal/firecracker/machine.go`) — N VMs restauradas del mismo snapshot
comparten las páginas no escritas en page cache; cada una paga solo lo que
ensucia. Es el mismo mecanismo de densidad de E2B/Lambda.

Cuellos de botella conocidos del camino de creación en frío (no aplican al
camino snapshot): `growRootfs` corre `e2fsck -fy` + `resize2fs` por clon
(`internal/storage/clone.go`) — ya es no-op si `disk_mb` no crece, así que las
plantillas sensor deben venir pre-dimensionadas.

Todas las cifras de la tabla son **estimaciones a validar en hardware**
(sesión IoT-5); no se prometen públicamente hasta medirlas.

## Qué existe ya (mapeo al código)

| Necesidad del patrón | Estado | Dónde |
|---|---|---|
| Aislamiento hipervisor + chroot/seccomp/cgroups | ✅ validado HW | motor entero; límites por VM en `internal/jailer/cgroup.go` |
| VM sin red, solo vsock | ✅ validado HW | `no_network:true`; vsock incondicional (`internal/firecracker/machine.go`) |
| Host interroga VMs por vsock (el bucle del gateway) | ✅ validado HW | `internal/vsock` Exec/PutFile/GetFileStream |
| guest→host DROP, cross-segment DROP, egress deny por defecto | ✅ validado HW | `internal/network/nftables.go` |
| Red/bridge/IPAM por segmento, MAC/IP únicas | ✅ validado HW | `internal/network`, `deriveMAC` |
| Reset-a-limpio en ~100ms + fork | ✅ validado HW | snapshots (restore 109ms, fork 113ms) |
| Memoria CoW compartida entre restores | ✅ implementado | backend File en restore |
| Clones de disco a coste ~0 | ✅ validado HW | store btrfs + reflink |
| Persistencia + reconcile (VMs sobreviven al daemon) | ✅ validado HW | `internal/store`, `Manager.Reconcile` |
| Observabilidad (health, capacidad, RSS por VM) | ✅ código | `/v1/health`, `/v1/system` |

## Huecos (lo que hay que construir)

1. **Egress de grano fino** — hoy `egress` es booleano por red. Falta: destino
   permitido (`IP:puerto`) por red o por VM, renderizado en la tabla
   `inet microhosted` (el diseño declarativo de `ApplyNftables` lo absorbe
   limpio). Prerequisito del modo pull.
2. **Orquestador transaccional** — el bucle restore→poll→validar→extraer→
   destruir con los 3 modos de vida, watchdog, circuit breaker y tope global
   de VMs. Es el producto; el motor son primitivas.
3. **ARM64** — Firecracker soporta aarch64 e `install-fc.sh` lo contempla,
   pero: `build-rootfs.sh` tiene `--arch=amd64` hardcodeado, el pipeline de
   kernel es x86, y nada se ha probado sobre KVM de una Pi. Riesgo existencial
   del hardware objetivo.
4. **Imagen sensor ultra-mínima** — kernel tinyconfig sin módulos + init
   estático + runtime del parser. Objetivo: VM funcional con `mem_mb: 24-32`.
   (Enlaza con las imágenes ultra-optimizadas ya pendientes.)
5. **Modo push** — cadena `prerouting` DNAT (hoy no existe) + trigger
   NFQUEUE/NFLOG + anti-DoS.
6. **Puente serie ciego** — daemon `tty↔vsock` sin parser.
7. **Despliegue masivo desde snapshot** — "levanta N workers de esta
   plantilla" como operación de primera clase (hoy es N llamadas a fork).

## Requisitos de hardware del gateway

- CPU con virtualización por hardware: x86_64 o **ARM Cortex-A con KVM**
  (Pi 4/5 con kernel 64-bit, Jetson, i.MX8). Los microcontroladores (ESP32,
  Cortex-M) no ejecutan microVMs — son los sensores que hablan con el gateway.
- Kernel 5.10+ con KVM, cgroups v2 y btrfs (o el loopback btrfs que
  provisiona `setup-host.sh` — ya agnóstico al host).
- Para densidad alta en Pi: almacenamiento en NVMe/SSD USB, no SD (el restore
  masivo lee el snapshot; la SD lo convierte en cuello de botella).
- GPIO/I2C/SPI directos: las microVMs no los ven; siempre vía puente ciego.

## Plan de sesiones

El orden mezcla riesgo (ARM primero: valida o mata el hardware objetivo) y
dependencias (egress fino antes que pull). IoT-2/3/4 se desarrollan en x86 —
no esperan al spike ARM.

### IoT-1 — Spike ARM64 (riesgo existencial)
Compilar el daemon (`GOARCH=arm64`), kernel aarch64 mínimo, rootfs arm64
(`build-rootfs.sh` parametrizado), revisar supuestos x86 (kernel args,
scripts). Arrancar una microVM en una Raspberry Pi 5.

**Criterio de éxito**: `create` + `exec` por vsock + `destroy` en una Pi 5,
con jailer y límites cgroup activos. Si KVM en Pi resulta inviable, pivotar el
hardware objetivo a gateways industriales ARM/x86 — decisión, no derrota.

### IoT-2 — Egress de grano fino
`allowed_egress: [{ip, port, proto}]` por red (o por VM), renderizado en
nftables. `egress:true/false` sigue funcionando como hasta ahora.

**Criterio de éxito**: una VM alcanza `IP_sensor:1883` y NADA más (ni otra IP,
ni otro puerto, ni DNS); guest→host y cross-segment intactos. Validado en HW.

### IoT-3 — Orquestador transaccional pull v1 (el MVP)
Entidad `Ingestor` (sensor, plantilla/snapshot, modo de vida, schedule):
restore → la VM interroga → resultado por vsock → destroy. Los 3 modos de
vida. Watchdog (VM no responde → kill+restore+evento). Tope global de VMs.
Demo con un sensor simulado (otra microVM haciendo de Modbus/MQTT slave —
dogfooding de gemelos digitales).

**Criterio de éxito**: demo end-to-end en x86 — sensor simulado → ciclo
transaccional → dato validado en el host; matar el parser dentro de la VM a
mitad de transacción produce reset limpio + evento, sin intervención.

### IoT-4 — Imagen sensor ultra-mínima
Kernel tinyconfig + init estático + parser (empezar por Modbus TCP o MQTT).
Medir RAM real (RSS del VMM + working set).

**Criterio de éxito**: la demo de IoT-3 corre con `mem_mb ≤ 32` y arranca
desde snapshot en <200ms.

### IoT-5 — Densidad: medir de verdad
Despliegue masivo desde snapshot como operación de primera clase. Batería de
densidad: cuántas VMs-sensor idle+polling caben en (a) la Pi del spike,
(b) un x86 de referencia, midiendo RSS incremental real, latencia de restore
bajo carga y comportamiento del store.

**Criterio de éxito**: tabla de densidad publicable con números medidos, no
estimados. Meta interna: ≥100 VMs-sensor en Pi 5 8GB.

### IoT-6 — Modo push (MQTT/HTTP)
Cadena prerouting DNAT + trigger NFQUEUE + anti-DoS (rate-limit por origen,
circuit breaker por sensor).

**Criterio de éxito**: sensor MQTT real publica → VM se materializa y recibe
la conexión sin listener en el host; un flood de SYNs no agota el host (el
tope y el rate-limit contienen).

### IoT-7 — Puente serie ciego
Daemon `tty↔vsock` sin parser (RS-485/Modbus RTU/USB).

**Criterio de éxito**: sensor serie (real o simulado con `socat pty`) →
parser Modbus RTU dentro de la VM → dato validado en el host; el daemon
puente no contiene ninguna lógica de protocolo (auditable a ojo).

### Transversal (no sesión propia, no se cae)
La Fase 4 pendiente (validación hardware de cgroups/seccomp, soak test, tests
de escape) sube de prioridad: en OT el producto ES la garantía de aislamiento,
y se demuestra con modelo de amenaza escrito + tests adversariales. El soak
test de N ciclos crear/destruir pasa de higiene a requisito del ciclo
transaccional (miles de restores/día por diseño).
