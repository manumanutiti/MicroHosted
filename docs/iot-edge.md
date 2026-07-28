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

### Push mode (later) — for MQTT/HTTP

The sensor initiates the connection and the VM must exist to receive it. The
problem: something has to see the connection arrive before the VM exists,
**without parsing a single byte**:

1. nftables marks the first SYN toward the ingestion port and hands it to the
   daemon via NFQUEUE/NFLOG (kernel L3/L4 metadata, zero payload).
2. The daemon restores the sensor's VM (by source IP) and installs the DNAT.
3. The sensor's retransmitted SYN (~1s, automatic in TCP) lands inside the VM.
   The payload bytes never touch the host's userspace.

Effective latency ~1s when cold — irrelevant for telemetry every 30s. For
high-frequency sensors: a hot VM per sensor ("per anomaly" mode).

**Anti-DoS mandatory**: an attacker spamming SYNs triggers a VM storm. A global
cap on concurrent VMs + per-source-IP rate limit + per-sensor circuit breaker
(if its VM dies N times in a row, quarantine and alert — that IS the compromise
detection).

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

| Engineering level | Incremental RAM/VM | Pi 5 8GB (~7GB usable) |
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
| Persistence + reconcile (VMs survive the daemon) | ✅ HW-validated | `internal/store`, `Manager.Reconcile` |
| Observability (health, capacity, per-VM RSS) | ✅ code | `/v1/health`, `/v1/system` |

## Gaps (what needs to be built)

1. **Fine-grained egress** — today `egress` is a per-network boolean. Missing: an
   allowed destination (`IP:port`) per network or per VM, rendered into the
   `inet microhosted` table (the declarative design of `ApplyNftables` absorbs it
   cleanly). Prerequisite of pull mode.
2. **Transactional orchestrator** — the restore→poll→validate→extract→destroy
   loop with the 3 lifetime modes, watchdog, circuit breaker, and global VM cap.
   It's the product; the engine is primitives.
3. **ARM64** — the install and image pipeline is already multi-arch
   (`make full-install` / `make prepare-image` detect or accept `ARCH=aarch64`:
   FC binaries, kernel from the CI bucket, arm64 debootstrap with the ports
   mirror, cross-build via qemu-user-static). What's missing is the **real
   validation**: nothing has been tested on a Pi's KVM. Existential risk of the
   target hardware.
4. **Ultra-minimal sensor image** — tinyconfig kernel without modules + static
   init + the parser runtime. Target: a functional VM with `mem_mb: 24-32`.
   (Ties into the already-pending ultra-optimized images.)
5. **Push mode** — a `prerouting` DNAT chain (doesn't exist today) + NFQUEUE/NFLOG
   trigger + anti-DoS.
6. **Blind serial bridge** — a `tty↔vsock` daemon with no parser.
7. **Mass deployment from snapshot** — "spin up N workers of this template" as a
   first-class operation (today it's N calls to fork).

## Gateway hardware requirements

- A CPU with hardware virtualization: x86_64 or **ARM Cortex-A with KVM** (Pi 4/5
  with a 64-bit kernel, Jetson, i.MX8). Microcontrollers (ESP32, Cortex-M) don't
  run microVMs — they're the sensors that talk to the gateway.
- Kernel 5.10+ with KVM, cgroups v2, and btrfs (or the btrfs loopback that
  `setup-host.sh` provisions — already host-agnostic).
- For high density on a Pi: NVMe/USB SSD storage, not an SD card (mass restore
  reads the snapshot; the SD turns it into a bottleneck).
- Direct GPIO/I2C/SPI: the microVMs don't see them; always via the blind bridge.

## Session plan

The order mixes risk (ARM first: it validates or kills the target hardware) and
dependencies (fine-grained egress before pull). IoT-2/3/4 are developed on x86 —
they don't wait for the ARM spike.

### IoT-1 — ARM64 spike (existential risk)
The tooling is already ready (`make full-install` and `make prepare-image` are
multi-arch); the spike is actually running it on a Raspberry Pi 5 and hunting the
x86 assumptions that only appear on hardware (aarch64 kernel args, KVM behavior
on the Pi, store performance on SD/NVMe).

**Success criterion**: `make full-install && make prepare-image` on a Pi 5 leave
the whole system working: `create` + `exec` over vsock + `destroy`, with jailer
and cgroup limits active. If KVM on the Pi turns out to be unviable, pivot the
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
many idle+polling sensor-VMs fit on (a) the spike's Pi, (b) a reference x86,
measuring real incremental RSS, restore latency under load, and store behavior.

**Success criterion**: a publishable density table with measured numbers, not
estimates. Internal goal: ≥100 sensor-VMs on a Pi 5 8GB.

### IoT-6 — Push mode (MQTT/HTTP)
A prerouting DNAT chain + NFQUEUE trigger + anti-DoS (per-source rate limit,
per-sensor circuit breaker).

**Success criterion**: a real MQTT sensor publishes → the VM materializes and
receives the connection with no listener on the host; a SYN flood doesn't exhaust
the host (the cap and rate limit contain it).

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
