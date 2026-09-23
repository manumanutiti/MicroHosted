# IoT/OT isolation gateway

> Design document for the niche pivot (2026-07-05). Strategic context in
> `PROJECT.md` → "Niche pivot". This document fixes the pattern, maps what
> already exists in the code, enumerates the gaps, and defines the session plan
> with success criteria.

## The problem

In a traditional edge gateway (the local concentrator for dozens of sensors:
ESP32, cameras, PLCs), the ingestion surface lives on the host:

- The broker/parser (MQTT, HTTP, CoAP, Modbus) runs in the host's userspace. A
  malicious frame that exploits the parser takes the whole gateway — and from
  there, the plant.
- Containers (Greengrass, balena, KubeEdge) don't fix this: they share a kernel.
  A kernel privilege escalation compromises everything.
- Traditional VMs (QEMU) don't fit: too much RAM/disk for a 2-8GB gateway.

## The pattern: isolated telemetry proxy

One disposable parser-microVM per sensor (or per small group of sensors). The
host **exposes no ports toward the sensors and parses no protocol**; the
already-validated data leaves the VM over vsock.

```
[ Physical sensors / network ]
       │            │
       ▼            ▼
  ┌────────────────────────────────────────────────────┐
  │ EDGE GATEWAY (host)                                │
  │                                                    │
  │  ┌───────────────┐  ┌───────────────┐              │
  │  │ microVM 1     │  │ microVM 2     │   … × N      │
  │  │ MQTT parser   │  │ Modbus parser │              │
  │  └───────┬───────┘  └───────┬───────┘              │
  │          │ vsock            │ vsock                │
  │          ▼                  ▼                      │
  │  ┌──────────────────────────────────────────────┐  │
  │  │ microhosted daemon (no listeners toward OT)  │  │
  │  └──────────────────────┬───────────────────────┘  │
  └─────────────────────────┼──────────────────────────┘
                            ▼
                   [ local DB / cloud ]
```

Guarantee: if a compromised sensor exploits its parser, it compromises a ~32MB
VM, with no network to the host (guest→host is already DROP by design), which
regenerates from a snapshot in ~100ms (measured: restore 109ms, fork 113ms).

## Ingestion modes

### Pull mode (first) — the OT-native one

The VM interrogates the sensor; the sensor never initiates anything. **Modbus,
OPC-UA, M-Bus, and most industrial protocols are already master/slave with the
gateway as master** — we're not imposing a strange model, we're putting a
hypervisor underneath the flow that plants already use.

Cycle: `restore from snapshot → the VM interrogates ITS sensor → validate/parse
→ deliver the result over vsock → destroy`.

- There's no inbound path at any moment: no listener, no DNAT, nothing.
- It requires **fine-grained egress**: the VM can only reach `sensor_IP:port`,
  nothing else (today egress is a per-network boolean — gap #1).
- Extraction uses the existing channel (`vsock.Exec`/`GetFileStream`,
  host→guest): the host *collects* the result, the guest can't initiate anything
  toward the host. Consistent with the current security model.

### Push mode (planned, IoT-6) — for MQTT/HTTP

> Status (2026-09-18): the network primitive is built and **validated on
> hardware**, `allowed_ingress` (`docs/networking.md` § Ingress): a device on
> `wlan0` reached a VM through the host's address with nothing listening on the
> host, and the VM saw the device's real source IP. The broker-VM template and
> the collection side are not built. The only data flow validated end to end is
> Modbus pull from a real ESP32+DHT11 over a managed interface.

In MQTT the sensor is the **client**: it opens the connection toward a broker
and listens on nothing, so it cannot be polled. The gateway has to accept an
inbound connection without the host listening or parsing a single byte. A
managed interface drops everything inbound (`iifname <iface> drop` in `input`
and `forward`). Its only inbound holes are the return legs of VM-initiated flows
and the `allowed_ingress` rules below.

#### The pattern: a dirty broker per sensor, a clean broker upstream

```
MQTT sensor 192.168.50.60 ──publish──▶ gateway:1883 on the managed iface
        prerouting DNAT keyed on source IP (kernel only, L3/L4 headers)
                    ▼
  broker-VM-60: Mosquitto + translator, NO egress at all
                    ▲ vsock (host-initiated, exactly as in pull mode)
  ingestor: strict validation → topic from the VM's identity → clean broker
                    ▼
          dashboards / DB / cloud
```

- **Push at the sensor edge, pull at the host edge.** From vsock upward the flow
  is identical to pull mode: same ingestor, same validation, same clean broker.
  Only who opens the sensor↔VM connection changes.
- **The VM translates** the vendor payload (custom JSON, Sparkplug B…) into the
  canonical format. The dangerous parsing stays inside the VM.
- **One broker-VM per sensor, not a shared one.** A shared broker lets one sensor
  that exploits Mosquitto forge every other sensor's data. Per-sensor keeps
  blast radius and identity 1:1, and the measured density (174 devices on an
  8 GB ARM64 host at ~34MB each) makes it affordable.
- **The broker-VM has no egress.** Compromised, it can't reach the internet,
  other sensors, or the host (guest→host DROP, vsock is host-initiated only).
  A reset to snapshot wipes it.
- **Source IP is spoofable** on Wi-Fi or a flat segment: per-sensor MQTT
  credentials in each broker-VM, plus `ap_isolate=1` / port isolation at the
  access layer.
- **The clean broker never receives sensor bytes**, only the ingestor's
  re-serialized output. Where it runs is a deployment choice, not a security one.

#### Lifetime depends on how the sensor is powered

| Sensor | Behaviour | Broker-VM lifetime |
|---|---|---|
| Mains-powered | Persistent TCP session, PINGREQ every keepalive (e.g. 60s); the broker drops it after 1.5× keepalive of silence | Always on, "per anomaly" mode (scheduled/reactive reset; the sensor's MQTT client reconnects by itself) |
| Battery (deep sleep) | Wake → connect → publish → disconnect → sleep 5–15 min | Idle ~99% of the time → candidate for on-demand |

Start with **always-on** broker-VMs: an idle Mosquitto costs ~0 CPU and only RAM.
On-demand is an optimization for when RAM runs short, and only for battery
sensors.

#### On-demand (later): hold the SYN, don't drop it

1. nftables sends the first SYN toward the ingestion port to the daemon via
   NFQUEUE (kernel L3/L4 metadata, zero payload).
2. The daemon restores the sensor's VM from its snapshot (~100ms measured) and
   installs the DNAT.
3. The daemon **reinjects the held SYN** with `NF_ACCEPT` instead of letting it
   drop and waiting for the sensor's TCP retransmission (1–3s depending on the
   sensor's stack — battery time on an ESP32). If the queue hook sits after
   conntrack (priority -200) and before NAT (-100), the reinjected packet is
   still the flow's first packet when it reaches `nat prerouting` and picks up
   the freshly installed DNAT: cold start ≈ restore latency. **To validate on
   hardware.** Retransmitted SYNs arriving while held must not trigger a second
   restore (dedupe per source).
4. No `bypass` on the queue: if the daemon isn't consuming it, SYNs drop
   (fail-closed).

Note: ~100ms is **restore from snapshot**, not cold creation (a full
network+egress+VM create measured ~1.8s in the density test). On-demand only
works from a snapshot.

**Anti-DoS mandatory** for on-demand: an attacker spamming SYNs triggers a VM
storm. A global cap on concurrent VMs + per-source-IP rate limit + per-sensor
circuit breaker (if its VM dies N times in a row, quarantine and alert — that IS
the compromise detection).

#### Residual risks (stated, not hidden)

- **VM escape** (Firecracker + jailer) is the root risk of the whole product.
  The adversarial escape tests of Phase 4 are pending; the guarantee is not
  advertised before they pass.
- The host kernel's TCP/IP stack and netfilter/conntrack process the headers —
  the same surface every packet arriving on the managed interface already hits
  today, not a new one.
- The **ingestor** becomes the most sensitive host component: small size cap,
  strict schema, physical ranges, re-serialize; never forward the VM's bytes.
- A compromised broker-VM can lie about its **own** sensor's data — which that
  sensor already controlled.

### Physical wire (RS-485 / Modbus RTU / USB serial)

Firecracker doesn't do character-device passthrough. Solution: a **blind serial
bridge** — a host daemon that copies raw bytes `/dev/ttyUSB0 ↔ vsock` of the
corresponding VM, with no protocol logic whatsoever (zero parsing surface on the
host). The Modbus parser runs inside the VM. The `dial()` + CONNECT handshake of
`internal/vsock` is already half the work.

## Transactional lifecycle: the product's dial

The three modes use the same machinery (snapshot/restore/kill already validated
on hardware); the operator picks the point on the security/cost-per-sensor
curve:

| Mode | VM lifetime | When |
|---|---|---|
| Per transaction | restore → 1 read → destroy | critical or slow data (≥30s between reads) |
| Per window | lives N seconds, processes the batch, destroy | frequent telemetry |
| Per anomaly | persistent; reset to snapshot scheduled (hygiene: every N hours) or reactive (watchdog) | high frequency / low latency |

Sizing rule: at 1 read/s × 100 sensors, "per transaction" is 100 restores/s —
absurd churn; there you use window or anomaly. The product framing: **the parser
is disposable; the compromise can't persist** — no current container or
edge-stack regenerates isolation, they only maintain it.

The reactive mode's watchdog closes the "isolate behaviors" loop: a VM that
doesn't respond to the vsock poll / exceeds its cgroup / behaves strangely →
`Kill` + restore to clean + alert event. All the material exists; the controller
is missing.

## Density: the honest arithmetic

The ceiling isn't CPU (sensors are almost always idle + `cpu.max` per VM already
implemented) or disk (reflink: 236KiB exclusive measured per clone). It's
**guest RAM**. And note: Firecracker's "<5MB per microVM" is the VMM overhead,
not the guest RAM.

| Engineering level | Incremental RAM/VM | 8 GB ARM64 host (~7GB usable) |
|---|---|---|
| Current image (ubuntu, 128MB) | ~130-190MB | ~40-50 |
| tinyconfig kernel + static init + parser | ~20-32MB | ~200-300 |
| + mass restore from a shared snapshot | ~5-15MB dirty | ~300-500 |

The third level's lever is **already implemented**: the snapshot's memory file
is restored with the File backend = copy-on-write mapping
(`internal/firecracker/machine.go`) — N VMs restored from the same snapshot
share the unwritten pages in page cache; each pays only for what it dirties. It's
the same density mechanism as E2B/Lambda.

Known bottlenecks of the cold-creation path (they don't apply to the snapshot
path): `growRootfs` runs `e2fsck -fy` + `resize2fs` per clone
(`internal/storage/clone.go`) — it's already a no-op if `disk_mb` doesn't grow,
so sensor templates should ship pre-sized.

All the figures in the table are **estimates to be validated on hardware**
(session IoT-5); they aren't promised publicly until measured.

## What already exists (code mapping)

| Pattern need | Status | Where |
|---|---|---|
| Hypervisor isolation + chroot/seccomp/cgroups | ✅ HW-validated | the whole engine; per-VM limits in `internal/jailer/cgroup.go` |
| VM with no network, vsock only | ✅ HW-validated | `no_network:true`; unconditional vsock (`internal/firecracker/machine.go`) |
| Host interrogates VMs over vsock (the gateway loop) | ✅ HW-validated | `internal/vsock` Exec/PutFile/GetFileStream |
| guest→host DROP, cross-segment DROP, egress deny by default | ✅ HW-validated | `internal/network/nftables.go` |
| Per-segment network/bridge/IPAM, unique MAC/IP | ✅ HW-validated | `internal/network`, `deriveMAC` |
| Reset-to-clean in ~100ms + fork | ✅ HW-validated | snapshots (restore 109ms, fork 113ms) |
| CoW memory shared across restores | ✅ implemented | File backend on restore |
| Disk clones at ~0 cost | ✅ HW-validated | btrfs store + reflink |
| Persistence + reconcile (VMs survive the daemon) | ✅ HW-validated for daemon restarts; a **host reboot** wiped every VM until 2026-09-19 (fixed: kept as stopped + `autostart`, HW validation pending). Crash-safe creates, residue sweep and `mh doctor`: roadmap Phase 0b | `internal/store`, `Manager.Reconcile`, `internal/vm/lifecycle.go` |
| Observability (health, capacity, per-VM RSS) | ✅ code | `/v1/health`, `/v1/system` |
| Fine-grained egress, incl. through a managed interface | ✅ HW-validated | `allowed_egress`, `ValidateEgressRules` (`internal/network/nftables.go`) |
| Inbound DNAT to one guest address (push mode) | ✅ HW-validated | `allowed_ingress`, `ValidateIngressRules` + `Manager.checkIngress` |

## Gaps (what needs to be built)

1. ~~**Fine-grained egress**~~ — done: `allowed_egress` (see the table above).
2. **Transactional orchestrator** — the restore→poll→validate→extract→destroy
   loop with the 3 lifetime modes, watchdog, circuit breaker, and global VM cap.
   It's the product; the engine is primitives. Its data path (stream protocol,
   ingest, journal, registry) is designed in `docs/ingestion.md`.
3. ~~**ARM64**~~ — done: the daemon runs on ARM64 hardware (see
   `docs/roadmap.md` § Where we actually are). Original text: the install and image pipeline is already multi-arch
   (`make full-install` / `make prepare-image` detect or accept `ARCH=aarch64`:
   FC binaries, kernel from the CI bucket, arm64 debootstrap with the ports
   mirror, cross-build via qemu-user-static). What's missing is the **real
   validation**: nothing had been tested on ARM64 KVM. Existential risk of the
   target hardware.
4. ~~**Ultra-minimal sensor image**~~ — done as `alpine-py` (42 MB PSS; see
   `docs/roadmap.md`). Original text: tinyconfig kernel without modules + static
   init + the parser runtime. Target: a functional VM with `mem_mb: 24-32`.
   (Ties into the already-pending ultra-optimized images.)
5. **Push mode** — `allowed_ingress` is built and HW-validated. Still
   missing: the broker-VM template and the collection side. The NFQUEUE on-demand
   trigger + anti-DoS is a later optimization, not a prerequisite.
6. **Blind serial bridge** — a `tty↔vsock` daemon with no parser.
7. **Mass deployment from snapshot** — "spin up N workers of this template" as a
   first-class operation (today it's N calls to fork).

## Gateway hardware requirements

- A CPU with hardware virtualization: x86_64 or **ARM Cortex-A with KVM** (ARM64
  boards with a 64-bit kernel, Jetson, i.MX8). Microcontrollers (ESP32, Cortex-M) don't
  run microVMs — they're the sensors that talk to the gateway.
- Kernel 5.10+ with KVM, cgroups v2, and btrfs (or the btrfs loopback that
  `setup-host.sh` provisions — already host-agnostic).
- For high density on a small ARM board: NVMe/USB SSD storage, not an SD card (mass restore
  reads the snapshot; the SD turns it into a bottleneck).
- Direct GPIO/I2C/SPI: the microVMs don't see them; always via the blind bridge.

## Session plan

The order mixes risk (ARM first: it validates or kills the target hardware) and
dependencies (fine-grained egress before pull). IoT-2/3/4 are developed on x86 —
they don't wait for the ARM spike.

### IoT-1 — ARM64 spike (existential risk)
The tooling is already ready (`make full-install` and `make prepare-image` are
multi-arch); the spike is actually running it on an ARM64 board and hunting the
x86 assumptions that only appear on hardware (aarch64 kernel args, KVM behavior
on ARM64, store performance on SD/NVMe).

**Success criterion**: `make full-install && make prepare-image` on an ARM64 board leave
the whole system working: `create` + `exec` over vsock + `destroy`, with jailer
and cgroup limits active. If KVM on the board turns out to be unviable, pivot the
target hardware to industrial ARM/x86 gateways — a decision, not a defeat.

### IoT-2 — Fine-grained egress
`allowed_egress: [{ip, port, proto}]` per network (or per VM), rendered into
nftables. `egress:true/false` keeps working as before.

**Success criterion**: a VM reaches `sensor_IP:1883` and NOTHING else (no other
IP, no other port, no DNS); guest→host and cross-segment intact. Validated on HW.

### IoT-3 — Transactional pull orchestrator v1 (the MVP)
An `Ingestor` entity (sensor, template/snapshot, lifetime mode, schedule):
restore → the VM interrogates → result over vsock → destroy. The 3 lifetime
modes. Watchdog (VM doesn't respond → kill+restore+event). Global VM cap. A demo
with a simulated sensor (another microVM acting as a Modbus/MQTT slave —
dogfooding of digital twins).

**Success criterion**: an end-to-end demo on x86 — simulated sensor →
transactional cycle → validated datum on the host; killing the parser inside the
VM mid-transaction produces a clean reset + event, without intervention.

### IoT-4 — Ultra-minimal sensor image
tinyconfig kernel + static init + parser (start with Modbus TCP or MQTT). Measure
real RAM (VMM RSS + working set).

**Success criterion**: the IoT-3 demo runs with `mem_mb ≤ 32` and boots from a
snapshot in <200ms.

### IoT-5 — Density: measure for real
Mass deployment from snapshot as a first-class operation. A density battery: how
many idle+polling sensor-VMs fit on (a) the spike's ARM64 board, (b) a reference x86,
measuring real incremental RSS, restore latency under load, and store behavior.

**Success criterion**: a publishable density table with measured numbers, not
estimates. Internal goal: ≥100 sensor-VMs on an 8 GB ARM64 host.

### IoT-6 — Push mode (MQTT/HTTP)
Design in "Push mode" above. Prerequisite: the ingestor of IoT-3 (the clean
side is shared with pull mode). Build order:

1. **`allowed_ingress`** on a network — **built and HW-validated (2026-09-18).**
   `{iface, src_ip, protocol, port, to_ip}`, rendered as a `prerouting` DNAT
   plus both `forward` legs, inside the managed interface's deny-both-ways
   policy. The return leg only carries replies (`ct direction reply`). Sensors
   address the gateway's IP on the managed interface; one port (1883) for all,
   distinguished by source IP. The target is named **by guest address
   (`to_ip`)**, not by VM. It survives a reset because the restore keeps the IP.
   Keeping addresses and VMs paired on shared networks (reassignment) is left
   to the orchestrator. Detail in `docs/networking.md` § Ingress.
2. **Broker-VM template** — Alpine + Mosquitto + translator to the canonical
   format, per-sensor credentials, no egress.
3. **Collection** — the ingestor reads the broker-VM over vsock (retained last
   message, or a local spool file fetched with GET), same validation as pull.
4. **Later: on-demand** — NFQUEUE hold-and-reinject + anti-DoS, only for battery
   sensors and only if RAM runs short.

**Success criterion (also the blast-radius demo)**: a real ESP32 publishes MQTT
→ the data reaches the clean broker with nothing listening on 1883 on the host
(`ss -ltn`); a simulated compromise of broker-VM A (exec a payload inside it)
can't reach the internet, the host, or sensor B (attempts verified to fail),
sensor B's data keeps flowing, and A is reset from snapshot in ~100ms. For
on-demand: a SYN flood doesn't exhaust the host (the cap and rate limit contain
it).

### IoT-7 — Blind serial bridge
A `tty↔vsock` daemon with no parser (RS-485/Modbus RTU/USB).

**Success criterion**: a serial sensor (real or simulated with `socat pty`) → a
Modbus RTU parser inside the VM → a validated datum on the host; the bridge
daemon contains no protocol logic (auditable by eye).

### Cross-cutting (not its own session, doesn't get dropped)
The pending Phase 4 (hardware validation of cgroups/seccomp, soak test, escape
tests) rises in priority: in OT the product IS the isolation guarantee, and it's
demonstrated with a written threat model + adversarial tests. The soak test of N
create/destroy cycles moves from hygiene to a requirement of the transactional
cycle (thousands of restores/day by design).
