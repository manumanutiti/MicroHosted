# MicroHosted — A self-hosted microVM orchestrator

## Why it exists

A real, unmet need: **strong, self-hosted isolation** for ephemeral workloads
(AI agents, CI, test environments) without depending on a third party (E2B,
Daytona, Modal...) and without the overhead of traditional VMs.

Proxmox has had open requests for microVM support since 2020, with no plan to
implement it. The only recent attempt (`pve-microvm`) is an experimental patch
on top of QEMU, not a native Firecracker+Jailer tool.

**The gap**: a small, self-owned tool with Jailer's real security model
(chroot + cgroups + seccomp), designed from scratch for isolated ephemeral
workloads.

It doesn't compete with Nomad, Kubernetes, or large startups. Controlled scope,
a concrete problem, room to grow if it proves itself.

---

## Scope of the first stage

**Core technical viability only**: create, use, and destroy isolated microVMs
programmatically and repeatably.

Out of scope for now: web interface, serious multi-tenancy, high availability.

---

## Technical stack

| Component | Choice | Reason |
|---|---|---|
| Language | Go | The official `firecracker-go-sdk` avoids writing the HTTP client from scratch |
| Hypervisor | Firecracker | Boot < 125ms, minimal footprint, serious security model |
| Isolation | Jailer | chroot + cgroups v2 + seccomp — the real model, not a workaround |
| Networking (stage 1) | TAP + static IP | No CNI for now, no overhead |
| Host↔guest communication | vsock | The pattern used by every serious sandboxing platform |
| State | SQLite → Postgres | SQLite first, migrate once there's multi-host |

---

## Three fundamental concepts (understand before writing code)

**1. The Firecracker API**

Firecracker is controlled through a REST API over a Unix socket. There's no
"start VM" CLI. Everything is a `PUT`/`PATCH` to endpoints:

```
PUT /boot-source      # kernel + cmdline
PUT /drives/rootfs    # root disk
PUT /machine-config   # vCPUs, memory
PUT /network-interfaces/eth0  # networking
PUT /actions          # InstanceStart / SendCtrlAltDel / FlushMetrics
```

**2. Jailer layout**

```
/srv/jailer/<exec-file>/<id>/root/
```

- `jailer` and `firecracker` must be binaries of the **exact same version**
- The API socket lives inside the chroot
- Jailer mounts `/dev/kvm`, the socket, etc. inside the chroot before executing

**3. Host↔guest communication**

For everything that isn't networking (sending commands to run, reading results),
**vsock** is used, not regular networking. It's the pattern used by E2B, Kata
Containers, and firecracker-containerd. The guest listens on a vsock port; the
host connects to the VM's CID.

---

## Host requirements

- Linux with `/dev/kvm` accessible (`ls -la /dev/kvm`)
- Kernel 5.10+ (6.x recommended)
- cgroups v2 mounted (`mount | grep cgroup2`)
- Ability to create TAP devices (`ip tuntap`)
- Go 1.22+
- `firecracker` and `jailer` from the same release (installed by `scripts/install-fc.sh`)

To verify KVM:
```bash
[ -r /dev/kvm ] && [ -w /dev/kvm ] && echo "OK" || echo "Missing permissions or KVM"
```

---

## Repository structure

```
MicroHosted/
├── cmd/
│   └── microhosted/       # Entry point of the main binary
│       └── main.go
├── internal/
│   ├── firecracker/       # SDK wrapper + REST API calls
│   ├── jailer/            # chroot configuration and launch via Jailer
│   ├── network/           # TAP devices, IP allocation, network cleanup
│   ├── storage/           # Kernel and rootfs management
│   ├── vm/                # VM lifecycle (create/destroy/snapshot)
│   └── api/               # HTTP/gRPC server exposed outward
├── pkg/
│   └── types/             # Shared types (VMConfig, VMState, etc.)
├── scripts/
│   ├── install-fc.sh      # Downloads firecracker + jailer (same version)
│   ├── build-kernel.sh    # Builds a minimal kernel for microVMs
│   ├── build-rootfs.sh    # Generates rootfs.ext4 from a definition
│   └── setup-host.sh      # Configures the host (cgroups, permissions, TAP, etc.)
├── images/
│   ├── kernels/           # Versioned vmlinux (binaries are git-ignored)
│   └── rootfs/            # Base rootfs.ext4 (images are git-ignored)
├── docs/
│   └── architecture.md
├── PROJECT.md             # This file
├── Makefile
├── .gitignore
├── go.mod
└── go.sum
```

> **Note on images**: Large binaries (kernels, rootfs) are not committed. They
> are downloaded or built locally with the scripts in `scripts/`.

---

## Roadmap by stages

### Stage 0 — Manual boot, no code
Obtain `firecracker` and `jailer` (same version). Download a minimal kernel and
example rootfs. Boot a microVM **by hand** with `curl` against the Unix socket.

**Success criterion**: get a serial console into a manually booted microVM and
shut it down cleanly. If this can't be done without failures, don't proceed —
debug here.

---

### Stage 1 — Own Firecracker API client (no Jailer)
First code: a program that replaces the `curl` calls from Stage 0 using
`firecracker-go-sdk`. Parameters: kernel path, rootfs, vCPUs, memory.

**Success criterion**: `./microhosted create --kernel ... --rootfs ...` boots a
VM; `./microhosted destroy <id>` shuts it down and cleans up.

---

### Stage 2 — Wrap with Jailer
Add the real isolation layer: chroot, cgroups, launch Firecracker via Jailer.

**Success criterion**: `ps`/`nsenter` confirms the Firecracker process can't see
the rest of the host filesystem.

---

### Stage 3 — Networking for a single microVM
TAP device + static IP configuration (no CNI yet).

**Success criterion**: `ping`/SSH from the host to the microVM.

---

### Stage 4 — Concurrency: several simultaneous microVMs
Generalize stages 1-3: unique sockets, unique IDs, unique IPs, unique cgroups,
correct cleanup on destroy.

**Success criterion**: 5 simultaneous microVMs, each accessible separately;
destroying one doesn't affect the others or leave orphaned resources.

---

### Stage 5 — Reproducible image pipeline
Automate building a minimal kernel and custom rootfs (not the AWS examples),
with versioned templates.

**Success criterion**: a script that, from a simple definition (packages, setup
commands), generates `rootfs.ext4` + `vmlinux` ready to use.

---

### Stage 6 — Snapshot / restore
Implement pause + snapshot + restore for near-instant boots.

**Success criterion**: measure and compare snapshot restore time versus cold
boot. Industry reference: ~150-200ms restoring a snapshot.

---

### Stage 7 — Host agent with its own API
Wrap everything behind a local HTTP/gRPC API: `POST /sandboxes`,
`DELETE /sandboxes/:id`, `GET /sandboxes`. The first component to move from
"script" to "service".

**Success criterion**: create, list, and destroy sandboxes by talking only to
the API, without invoking Firecracker/Jailer directly from outside.

---

### Stage 8 — Persistent state
Store state in SQLite (later Postgres), not in memory.

**Success criterion**: restart the process and correctly recover which sandboxes
existed, without losing or duplicating state.

---

### Stage 9 — Multi-host and a simple scheduler
Extend to 2+ physical hosts. Simple scheduler: "the host with the most free
capacity".

**Success criterion**: with 2 hosts, the system places sandboxes on the one with
capacity and detects when a host stops responding.

---

### Stage 10 — Stage close: stress test
A repeated create/destroy loop, measuring leaks (cgroups, TAP devices, sockets,
memory) and real timings.

**Success criterion**: N create/destroy cycles without leaks or crashes. This is
where the "core viability" stage ends.

---

## After this stage (future reference)

A dashboard-style web interface, multi-tenancy and network isolation between
clients, observability/metrics, deeper security hardening, and — if everything
works well — evaluating whether to shape it into a public open-source project.

---

## Refined direction (2026-07-01)

After validating the core (stages 1-3 + vsock/SSH access), the direction and
target audience are sharpened.

### Two use cases, one core
- **Cybersecurity / forensics sandboxing**: detonate untrusted binaries, malware
  analysis, run Volatility or other tools over dumps, collect artifacts (pcaps,
  memory dumps) — all with real hypervisor isolation and revert to a clean state
  between samples.
- **Sandboxing for AI agents**: isolated, ephemeral, self-hosted code execution,
  without sending code/data to a third party (E2B/Daytona/Modal).

Both share the same requirement: run something untrusted without letting it
touch the host. Hardening for malware gives, for free, the isolation the AI case
also wants. (GPU quotas for AI and an ONNX-style test agent are parked for later
— core first.)

### Positioning refinement (2026-07-02)

The framing matures from "two use cases" to a **single positioning**: a
**SELF-HOSTED security sandboxing platform**. Split into two layers:

- **The engine** (microVM lifecycle + isolation + snapshots): general and
  neutral, agnostic to the use case. Quality is measured in correctness and
  guarantees.
- **The product**: a **curated library of disposable images** for various
  defensive uses (malware detonation, honeypots/deception, DFIR benches,
  blue-team ranges, labs/CTF; the AI agent sandbox is a second act on the same
  engine).

Strategic keys:
- **Moat = data sovereignty.** Self-hosting is the axis where cloud sandboxes
  (ANY.RUN, Joe Sandbox, e2b) structurally can't compete: you don't send the
  sample/data to a third party. For banking/defense/healthcare/air-gap it's a
  hard "no", not a preference.
- **Incumbent to displace = CAPEv2/Cuckoo** (the classic self-hosted malware
  sandbox, heavy QEMU, painful to operate). Angle: the modern successor on
  Firecracker microVMs, API-first, that actually installs.
- **Discipline: general engine, first workflow sharp.** "A library for various
  uses" is the promise, not the launch. Launch with ONE deep workflow
  (detonation: sample → isolated VM with no egress → run → capture artifacts →
  reset to clean) and expand from there. Breadth = promise; depth-of-one = proof.
- **Consequences**: the catalog/image system becomes a first-class asset
  (versioned templates, manifests, reproducible builds, signing) — it ties into
  the pending ultra-optimized images (Alpine/Rocky). **Snapshots** are the most
  strategic piece (reset-to-clean, fork at the point of infection), above
  multi-host/HA. **A written threat model + adversarial tests** become a selling
  point, not hygiene.
- **Order**: control plane, queues, HA, and multi-host are later stages; their
  order is dictated by a use case that pulls, not a scaling checklist. Next: soak
  test (operational robustness) → snapshots → vertical detonation workflow.

### Fixed architecture decisions
- **Networking segmented by name** (not the current point-to-point `/30`, where
  the host is the gateway for every VM and two VMs can't see each other). One
  bridge per named network; VMs join by name; `nftables` enforces by default:
  `guest→host` blocked, cross-segment blocked, internet egress as a per-network
  flag (deny by default = malware-safe; opt-in NAT). A single mechanism covers
  both inter-VM AND isolation.
- **SQLite persistence** from now on: a backend that forgets its state on restart
  isn't "functional". It replaces the in-memory state of the `Manager` and the
  `Allocator`.

### Functional backend plan (by dependencies)

**Phase 0 — Foundation: full cleanup + persistence**
- Audit `Destroy`: today it does NOT delete the Jailer chroot
  (`/srv/jailer/<exec>/<id>/`) — a disk leak in create/destroy loops. Close it
  and audit every failure path.
- SQLite: persist VMs/networks/volumes; reconcile live processes on startup.
- Per-VM cgroup limits (CPU/mem/pids): prevents fork-bombs / exhausting the host.

**Phase 1 — Segmented networking** (redesign of `internal/network`)
- A `Network` entity (name, subnet, `egress`) + CRUD `/v1/networks`.
- One bridge per network; each VM's TAP enslaved to the bridge; per-network IPAM.
- `nftables`: drop guest→host, drop cross-segment, conditional NAT.
- `NoNetwork` remains valid for the most hermetic sandbox.

**Phase 2 — Storage**
- *(done, validated on hardware 2026-07-02)* **Configurable disk size**
  (`disk_mb` in template/request; the clone is grown with `resize2fs`) + a
  **copy-on-write store** (btrfs loopback, host-agnostic, provisioned by
  `setup-host.sh`). Everything Jailer clones/hardlinks (golden, kernel, chroot)
  lives on the same btrfs by invariant — reflink and hardlink don't cross
  filesystems. See `docs/layers.md` L3.
- *(done)* A `Volume` entity (name, size, persistent ext4 that survives destroy)
  + CRUD `/v1/volumes`; attach as an extra drive (Firecracker `Drives`),
  read-only or writable, auto-mounted in the guest over vsock. Security: a sample
  mounted read-only + a writable output volume for artifacts. A VM with volumes
  can't snapshot/fork/restore in v1 (409). See `docs/volumes.md`.

**Phase 3 — Complete the CRUD**
- *(done)* Update: inject/extract files to a VM or volume. Via vsock if the VM is
  alive (`PUT/GET /v1/vms/{id}/files`, data channel on a volume with no network);
  via `debugfs` **without mounting** if it's stopped or the volume is detached
  (`/v1/volumes/{id}/files`). Security rule: the host NEVER mounts a guest fs —
  `mount(2)` of an untrusted image exposes the host kernel's ext4 parser. See
  `docs/volumes.md`.
- `docker ps`-style read: state, base image, network, volumes.

**Phase 4 — Hardening + stress test**
- Verify seccomp, cgroup caps, egress denied by default.
- Escape test (can't see the host FS, can't reach the host or another network,
  fork-bomb doesn't take down the host) + N create/destroy cycles without leaks.

---

## Niche pivot (2026-07-05) — IoT/OT isolation gateway

Direction decision: **the project specializes in the IoT/OT edge niche.** The
engine (Firecracker+Jailer microVMs, nftables segmented networks, btrfs CoW,
snapshots/fork, vsock, volumes, cgroups, observability) stays as is — it's
advanced enough to support a real vertical, and this vertical doesn't require
redesigning it, only extending it.

### Why IoT/OT and not detonation first

- The AI sandbox market is saturated (confirmed 2026-07-03). The detonation one
  has room, but it demands competing against habits (CAPEv2) and an analyst
  adoption cycle.
- In IoT/OT the gap is **structural**: today's edge gateways (Greengrass, balena,
  KubeEdge, EdgeX) use containers = shared kernel; an exploit in a sensor's
  parser compromises the plant. Nobody offers "hypervisor isolation per sensor,
  at the gateway, installable in an afternoon". EVE-OS is the closest neighbor
  and it's heavy generic orchestration, not this pattern.
- The selling point is the same old moat (process untrusted data without letting
  it touch the host) applied to sensor frames instead of malware samples.
  Detonation becomes a **second act** on the same engine; nothing is thrown away.

### The product

An **isolation gateway**: each sensor (or a small group of sensors) has its own
disposable parser-microVM. The host exposes no port toward the sensors and
parses no protocol; validated data leaves the VM over vsock. If a compromised
sensor exploits its parser, it compromises a ~32MB VM with no network to the
host, which regenerates from a snapshot in ~100ms.

Two ingestion modes, in this order:

1. **Pull (first)** — the VM boots (restore from snapshot), interrogates its
   sensor (egress restricted to that IP:port), validates, delivers over vsock,
   and is destroyed. It's the **native OT model** (Modbus/OPC-UA are polling),
   the simplest and safest: there is no inbound path at any moment.
2. **Push (later)** — for MQTT/HTTP: the kernel detects the incoming connection
   (nftables/NFQUEUE), the VM is restored, DNAT toward it, and the sensor's
   retried SYN lands inside the VM. It requires anti-DoS (a global VM cap +
   per-source rate limiting).

**Transactional lifecycle** as the product's dial: a VM *per transaction* (clean
state per datum), *per window* (lives N seconds, processes the batch), or *per
anomaly* (persistent with a scheduled/reactive reset — an implant can't persist).
All three use the same snapshot/restore/destroy machinery.

**Digital twins** as a secondary use case of the same engine: CoW clones +
unique MAC/IP per VM = simulate device fleets for broker stress tests, OTA, and
targeted attacks.

### Expectation corrections (so as not to oversell)

- Firecracker's "<5MB per microVM" is the *VMM overhead*, not the guest RAM. A
  real minimal Linux needs 20-50MB. High density comes from (a) an ultra-minimal
  image and (b) mass restore from a shared snapshot (memory is mapped
  copy-on-write — already implemented): each VM pays only for its dirty pages.
- Realistic target: **dozens of VMs on a 4GB ARM64 board, 100-300 on an 8/16GB one (via
  snapshot + minimal image), 1000+ on an industrial x86 gateway.** Figures to be
  validated on hardware, not promises.

### Needs and session plan

The full design, gaps with code mapping, and success criteria per session are in
**`docs/iot-edge.md`**. Priority summary:

1. **ARM64 spike** (existential risk: until a microVM boots on an ARM64 board,
   the target hardware is theory).
2. **Fine-grained egress** (a VM can only talk to its sensor IP:port) —
   prerequisite of the pull mode.
3. **Transactional pull orchestrator v1** (restore→poll→validate→extract→destroy,
   with the 3 lifecycle modes) — the product MVP, developable on x86.
4. **Ultra-minimal sensor image** (tinyconfig kernel + static parser; target
   `mem_mb ≤ 32`).
5. **Density**: mass pool from snapshot + honest measurement on ARM64 and x86.
6. **Push mode** (DNAT + NFQUEUE trigger + anti-DoS).
7. **Blind serial bridge** (RS-485/Modbus RTU → vsock, a daemon with no parser).

The pending hardening (Phase 4: cgroups/seccomp validation, soak test) **remains
in force and rises in importance** — in OT the selling point is the isolation
guarantee, and that's demonstrated with the threat model + adversarial tests
already planned.

---

## Key references

- [Firecracker GitHub](https://github.com/firecracker-microvm/firecracker)
- [firecracker-go-sdk](https://github.com/firecracker-microvm/firecracker-go-sdk)
- [Firecracker Getting Started](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md)
- [Jailer documentation](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md)
- [firecracker-containerd](https://github.com/firecracker-microvm/firecracker-containerd) (architectural reference)
