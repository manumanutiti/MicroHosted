# Layers — what sits on top of what

A simple, bottom-to-top map of MicroHosted. Each layer only works if the layer
below it is in place. Read it once and the whole system stops feeling like
magic: it's just seven layers, from "what the machine must have" to "a microVM
you can talk to".

For each layer: **Needs** (what must already be true), **You install / it
creates** (what gets put on disk), **Running after** (what exists once the layer
is up).

```
L7  A running microVM  ......... clone → grow → jail → boot → vsock/SSH
L6  Networking  ................ bridges, TAPs, nftables, egress
L5  The daemon (microhosted) ... API + Manager + SQLite state
L4  Images  ..................... kernel + golden rootfs + catalog
L3  Storage (CoW store)  ........ btrfs store: rootfs/ kernels/ jailer/ clones
L2  Firecracker + Jailer  ....... the VMM and its sandbox launcher
L1  Host packages  .............. iproute2, nftables, btrfs-progs, e2fsprogs
L0  Host requirements  .......... KVM, cgroup v2, Linux 5.10+
```

---

## L0 — Host requirements

The bare minimum the physical/virtual machine must provide. Nothing to build
here; it's either present or the platform can't run.

- **Needs:** a Linux kernel 5.10+ with **KVM** (`/dev/kvm`) and **cgroup v2**
  mounted. On a cloud VM this means nested virtualization must be enabled.
- **You install / it creates:** nothing — `scripts/setup-host.sh` only *checks*
  these and fixes `/dev/kvm` permissions (group `kvm`).
- **Running after:** a host that can actually launch a hardware-isolated VM.

Why it matters: KVM is the real isolation boundary. Everything above trusts that
a guest cannot escape the virtual machine.

---

## L1 — Host packages

The command-line tools the daemon shells out to.

- **Needs:** L0, plus `apt` to install.
- **You install / it creates** (via `scripts/setup-host.sh`):
  - `iproute2` — create/manage TAP devices and bridges.
  - `nftables` — the firewall rules that isolate guests (a hard dependency: the
    daemon applies its ruleset at startup and won't boot without `nft`).
  - `btrfs-progs` — format the copy-on-write store (L3).
  - `e2fsprogs` — grow a VM's disk (`resize2fs`, `e2fsck`) at clone time.
- **Running after:** the host has every tool the daemon needs; no daemon yet.

---

## L2 — Firecracker + Jailer

The engine. **Firecracker** is the microVM monitor (it runs the guest).
**Jailer** is a wrapper that drops Firecracker into a locked-down chroot +
cgroup before it starts, so a compromised VMM still can't see the host.

Jailer creates the per-VM cgroup but writes no limits into it on its own; the
daemon does, right after launch: `cpu.max` (the VM's vCPU count × one full
period), `memory.max` (guest memory + a fixed VMM overhead margin, swap
disabled) and `pids.max` (vCPUs + a small headroom — Firecracker never forks)
go into `/sys/fs/cgroup/firecracker/<vm-id>` on every boot. Without them a
guest could DoS the host from inside its jail: pin every core, balloon the
VMM, or fork-bomb the host's PID space. Fail-closed: a VM whose limits can't
be applied doesn't boot (cgroup v2 hosts; on v1 the daemon warns and boots
uncapped, the pre-limits behavior).

- **Needs:** L0 (KVM) and L1.
- **You install / it creates** (via `scripts/install-fc.sh`): the `firecracker`
  and `jailer` binaries in `/usr/local/bin` (both from the exact same release —
  they must match).
- **Running after:** nothing runs yet, but the host can now *launch* a jailed
  microVM if asked.

---

## L3 — Storage: the copy-on-write store

Where every VM disk lives. This layer is what keeps the platform from filling
the host: instead of copying a full rootfs per VM, clones share the golden
image's blocks and only cost what the guest actually writes.

- **Needs:** L1 (`btrfs-progs`).
- **You install / it creates** (via `scripts/setup-host.sh`):
  - A **btrfs** filesystem. If the target directory isn't already on a
    reflink-capable filesystem, the script provisions a **loopback btrfs** — a
    sparse image file (`/var/lib/microhosted/instances.btrfs`) mounted at
    `/var/lib/microhosted/store` and persisted in `/etc/fstab`. btrfs ships in every
    Ubuntu kernel, so this works on any host without repartitioning.
  - Three subdirectories on that store: `rootfs/` (golden images), `kernels/`
    (guest kernels), `jailer/` (the Jailer chroot base).
- **Running after:** a single filesystem that can make instant CoW clones.

**The one rule of this layer:** everything the daemon clones or hard-links —
golden rootfs, kernel, and the jail chroot — must live on this *same*
filesystem. Two reasons, both hard limits of Linux:
- `cp --reflink` (CoW) **cannot cross filesystems** → a golden on another disk
  would be a full copy, not a clone.
- Jailer **hard-links** the rootfs and kernel into the chroot, and a hard link
  **cannot cross filesystems** → a chroot on another disk fails to launch with
  `invalid cross-device link`.

That's why goldens, kernels, and the jail dir all sit under `/var/lib/microhosted/store/`.

---

## L4 — Images

The actual bits a VM boots: a **kernel** and a **golden rootfs** (a read-only
template disk). Plus the **catalog** that names them.

- **Needs:** L3 (they must be placed *on the store*).
- **You install / it creates:**
  - A kernel at `/var/lib/microhosted/store/kernels/…` and a golden rootfs at
    `/var/lib/microhosted/store/rootfs/…`.
  - The golden is prepared once with `scripts/prepare-image.sh`, which bakes in:
    a **vsock exec listener** (so the platform can run commands without a
    network), a **DNS fix** (`/etc/resolv.conf` → `/proc/net/pnp`), and an
    **SSH key** for interactive access.
  - `images/catalog.json` — the template library. Each entry names a kernel +
    rootfs and its defaults: `vcpus`, `mem_mb`, and `disk_mb` (the size clones
    are grown to).
- **Running after:** the daemon has something to clone from. Still no VM.

Think of L4 as the "image library" — the surface that will hold many
purpose-built images (a detonation sandbox, a honeypot, an analysis bench…),
each stored once and cloned cheaply thanks to L3.

---

## L5 — The daemon (`microhosted`)

The orchestrator: one Go binary that serves the HTTP API and owns every VM's
lifecycle.

- **Needs:** L2, L3, L4.
- **You install / it creates** (via `make install-service`): the binary in
  `/usr/local/bin`, a **systemd** unit, and a **SQLite** state file
  (`images/microhosted.db`). The unit uses `KillMode=process` so restarting the
  daemon does **not** kill running VMs — on restart, `Reconcile` re-adopts them.
- **Running after:** the API is live (`unix /run/microhosted.sock`). It loads the catalog, opens the
  state DB, warns if the store isn't CoW-capable, and derives the Jailer chroot
  from the store directory (so the L3 same-filesystem rule holds automatically).

Key idea: **VMs outlive the daemon.** Their state is in SQLite and their
processes are jailed children that systemd leaves alone; the daemon can restart
and pick up exactly where it was.

---

## L6 — Networking

How VMs get (or are denied) a network. Off this layer, a VM is fully sealed and
only reachable over vsock.

- **Needs:** L1 (`iproute2`, `nftables`) and L5.
- **You install / it creates:** nothing to install — the daemon builds it live.
  Networks are **named segments**: one Linux **bridge** per network, each VM's
  **TAP** enslaved to it, an **IP** from that network's subnet, and an
  **nftables** ruleset that (by default) blocks guest→host, blocks traffic
  between segments, and only allows internet egress when a network opts in.
- **Running after:** VMs on the same network can see each other; different
  networks are isolated; no guest can reach the host. A `default` network exists
  out of the box.

Isolation lives here: the firewall posture is the product's security guarantee,
not an afterthought.

---

## L7 — A running microVM

The top. What actually happens on `POST /v1/vms`.

- **Needs:** everything below.
- **The daemon then:**
  1. **Clones** the golden rootfs on the store — a CoW reflink (L3), near-zero
     cost.
  2. **Grows** the clone to `disk_mb` with `resize2fs` so the guest has free
     space (a golden is near-full; without this, `apt install` fails with
     *No space left*).
  3. **Attaches networking** (L6) unless `no_network` is set: TAP, bridge, IP.
  4. **Jails and boots** (L2): Jailer hard-links the clone + kernel into a
     chroot on the store, drops privileges, and launches Firecracker.
  5. **Records** the VM in SQLite (L5).
- **Running after:** an isolated microVM you can reach two ways — **vsock exec**
  (`POST /v1/vms/{id}/exec`, works even with no network) or **SSH** (if it has
  one). `stop`/`start` power it off and back on keeping its disk and IP;
  `delete` frees everything.

**Data in and out.** Beyond a VM's own disk, the platform has a **data plane**
(see `docs/volumes.md`): persistent **volumes** (ext4 disks that outlive VMs,
attached read-only for a sample or writable for artifacts) and file transfer
that adapts to the VM's state — over **vsock** while running (even with no
network), or **offline via `debugfs`** while stopped. The one rule mirrors L2's:
the host **never mounts a guest filesystem** — `debugfs` reads/writes the ext4
in userspace, so a malicious image can't reach the host kernel's fs parser.

---

## One-line summary

> A host with KVM (L0) and a few packages (L1) runs Firecracker+Jailer (L2)
> against a copy-on-write store (L3) full of cloneable images (L4); one daemon
> (L5) wires up isolated networks (L6) and turns each request into a cheap,
> sandboxed, throwaway microVM (L7).
