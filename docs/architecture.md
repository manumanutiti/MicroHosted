# Architecture — MicroHosted

## Flow of a microVM (stage 1)

```
CLI / API
    │
    ▼
vm.Manager
    │
    ├─► storage.CloneRootfs # clones the golden (reflink CoW) and grows it to disk_mb
    │       └─► /var/lib/microhosted/store/<id>.ext4
    │
    ├─► jailer.Runner      # creates the chroot, launches jailer → firecracker
    │       └─► /var/lib/microhosted/store/jailer/firecracker/<id>/root/
    │
    ├─► firecracker.Client # talks to the Unix socket inside the chroot
    │       PUT /boot-source
    │       PUT /drives/rootfs
    │       PUT /machine-config
    │       PUT /network-interfaces/eth0
    │       PUT /actions { InstanceStart }
    │
    └─► network.Manager    # segmented networking: one bridge per network + enslaved TAP
            ip tuntap add tapN mode tap        # TAP without IP
            ip link set tapN master mhbr<net>  # enslaved to the network's bridge
            ip link set tapN up
            # The guest IP is set by Firecracker (static, via kernel); nftables
            # isolates: drop guest→host, drop cross-segment, optional egress NAT.
```

(The original point-to-point `/30` model, with the host as the gateway for every
VM, was retired in Phase 1 — see `docs/networking.md`.)

## Host ↔ guest communication

- **Networking**: TAP device → static IP → regular SSH/HTTP
- **Commands/results**: vsock (virtio-vsock)
  - The guest exposes a server on `VSOCK_PORT`
  - The host connects to the VM's CID
  - It doesn't use IP networking — it works even with no network configured

## VM state

```
creating → running ⇄ stopped        (quarantine is a flag, not a state)
```

`creating` is internal (undone if the daemon dies mid-create); a snapshot pauses a
VM for a fraction of a second without surfacing a state. The full lifecycle — what
each operation keeps and frees, quarantine, replace — is in
[engine.md](engine.md#2-a-vms-life).

## Storage: copy-on-write store

Every VM disk lives in a **btrfs copy-on-write store** mounted at
`/var/lib/microhosted/store` (a btrfs loopback that `setup-host.sh` provisions on
any host, without repartitioning). Cloning a golden is an instant reflink: each
VM shares the golden's blocks and only costs what it writes (measured: ~236 KiB
per 1GB clone, not 300MB).

`storage.CloneRootfs` clones the golden and grows it to the template's `disk_mb`
with `resize2fs` (offline: goldens are ext4 over the whole device, with no
partition table, so `truncate` + `resize2fs` is enough), so the guest has free
space — a golden ships nearly full and, without this, an `apt install` runs out
of disk. The grow happens once per golden and size: the result is cached as
`<store>/sized/<golden>-<size>m-<key>.ext4`, where the key identifies the golden
file as it is now, and every later clone reflinks it. The guest gets the same
bytes it would from growing its own clone. On startup the daemon removes copies
that match no current template golden (template removed from the catalog, or
golden rebuilt or deleted); the next create that needs one rebuilds it.

**Key invariant**: everything the daemon clones or hardlinks shares this same
filesystem, because neither reflink (`cp --reflink`) nor hardlink cross
filesystems on Linux. That's why goldens, kernels, and the Jailer chroot all
live under the store:

```
/var/lib/microhosted/store/            (btrfs, CoW)
├── rootfs/                   # goldens (reflink source)
├── kernels/                  # kernels (Jailer hardlinks them into the chroot)
├── jailer/                   # Jailer chroot base (derived chroot-base)
│   └── firecracker/<vm-id>/root/
│       ├── firecracker          # hard link to the binary
│       ├── firecracker.socket   # REST API socket
│       ├── vmlinux              # hard link to the store kernel
│       └── <id>.ext4            # hard link to the clone (SAME inode → Stop keeps the disk)
├── snapshots/                # snapshots (frozen memory+state+disk)
│   └── <snap-id>/
│       ├── vmstate              # device/vCPU state (Firecracker)
│       ├── mem                  # guest memory (restores map it CoW)
│       └── disk.ext4            # reflink of the disk taken with the VM paused
└── <id>.ext4                 # VM clone (reflink of the golden, grown)
```

Jailer chroots into `root/` before executing Firecracker; the resulting process
sees nothing outside that directory. The daemon's `--chroot-base` **derives from
`--instances-dir`** (`<instances>/jailer`) precisely to guarantee the same FS.
See `docs/layers.md` (L3) for the detail.

## Snapshots and forking

**Create** (`vm.Manager.Snapshot`): pauses the VM (`PATCH /vm`), asks Firecracker
for a Full snapshot (`PUT /snapshot/create` — the process is chrooted, so it
writes vmstate+mem inside its own chroot), the daemon moves them with `os.Rename`
(same FS ⇒ free) to `snapshots/<sid>/`, reflinks the disk **while still paused**
(memory and disk stay mutually consistent), and resumes. These calls go straight
to the UDS socket (`internal/firecracker/rawapi.go`), not through the SDK: this
way they work the same on VMs adopted after a daemon restart (no SDK handle) and
give access to fields the SDK v1.0.0 doesn't know about.

**Restore/fork** (`vm.Manager.Fork` / `Restore`): a new Firecracker is launched
via Jailer **without boot** — the SDK's boot pipeline is replaced by StartVMM + a
custom handler (`internal/firecracker.LaunchFromSnapshot`) that hardlinks
vmstate/mem/disk into the freshly created chroot and does `PUT /snapshot/load`
with `resume_vm`. The mem is mapped copy-on-write: N VMs can restore from the
same snapshot at once without copying it.

**TAP and Firecracker version**: the vmstate remembers the original TAP's name,
and `network_overrides` (the `/snapshot/load` field that lets you remap the NIC
to another TAP) **only exists from Firecracker v1.12.0 onward**. The daemon
probes the binary's version at startup (`firecracker.SupportsNetworkOverrides`)
and adapts the strategy:
- **In-place restore**: recreates the TAP with its original name → never needs an
  override → works on any version.
- **Fork on FC ≥1.12**: its own TAP (`tap<new-id>`) + override. No restrictions.
- **Fork on FC <1.12**: reuses the snapshot's original TAP name if it's free
  (original destroyed/stopped); if it's taken → 409 explaining that simultaneous
  forks require upgrading Firecracker. Careful when upgrading: the snapshot format
  is tied to the FC version — existing snapshots have to be recreated.

Measured on hardware (2026-07-04, host FC 1.10.1): snapshot 271ms, in-place
restore 109ms, fork 113ms (full endpoint, 128MB VM).

**Frozen identity**: the guest's IP/MAC live in the snapshotted memory and can't
be changed on restore. That's why a fork has two modes: join the origin network
by reclaiming the snapshot's exact IP (strict reservation, `ClaimVM` — 409 if
taken), or `quarantine` (a TAP with no bridge: the guest thinks it has a network,
everything dies at the host, access only over vsock — N simultaneous forks).

## Design decisions

| Decision | Rejected alternative | Reason |
|---|---|---|
| Go + firecracker-go-sdk | Python / Rust from scratch | Maintained official SDK, avoids writing the HTTP client |
| SQLite first | Postgres from the start | A single host doesn't need Postgres; migrate once multi-host arrives |
| Static IP (stage 1) | CNI / IPAM | CNI adds complexity without contributing anything on a single host |
| vsock for host↔guest | SSH for everything | vsock needs no configured IP, lower latency, industry pattern |
| btrfs CoW store (loopback) | full copy on ext4 | clones = deltas, not whole rootfs; loopback = host-agnostic, no repartitioning |
| Golden/kernel/chroot in the store | scattered paths (/srv/jailer, images/kernels) | reflink and hardlink don't cross FS; everything on one FS or the clone/boot fails |
