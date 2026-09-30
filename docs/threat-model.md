# Threat model and blast radius

What MicroHosted protects, against whom, how far damage can spread when
something is compromised, and the layers that stop it — each with what it
defends against, how it works, where it lives in the code, how it was verified,
and what it does **not** stop.

It starts simple and gets more precise section by section. Known gaps are listed
at the end, not hidden in the middle.

---

## 1. In one paragraph

MicroHosted runs code it does not trust — a protocol parser that talks to a
sensor, a sample under analysis, an agent's tool call — inside microVMs. The
working assumption is that **any VM can be fully compromised at any moment**.
The design goal is that such a compromise stays inside that VM: it cannot reach
the host, cannot reach other VMs, cannot reach any network the operator did not
declare for it, cannot exhaust the host, and — once detected — can be cut off and
replaced by a clean VM while it is kept for forensics. Every one of those
properties is enforced by a mechanism below the guest's control, and most of them
by more than one.

---

## 2. What is protected, and from whom

### Assets

| Asset | Why it matters |
|---|---|
| **The host** (kernel, root, the daemon, its state DB) | owning it owns every VM and every network policy |
| **Other VMs** and their data | one compromised workload must not become all of them |
| **Trusted networks** behind the host (plant LAN, office, VPN, management) | an edge gateway usually sits between an untrusted device segment and a trusted one; being the pivot between them is the worst outcome |
| **Devices** on the untrusted segment (sensors, PLCs) | a compromised VM must not be able to attack them beyond what its policy already allows |
| **Integrity of what a function reports** | a compromised parser can lie; that must be detectable and recoverable |
| **Availability** of the other functions | one VM must not be able to starve the rest |

### Adversaries

| Adversary | Starting position | Considered |
|---|---|---|
| **A compromised guest** | root inside one VM (e.g. a parser exploited by a malicious device) | **primary** — every layer below is designed against it |
| **A malicious device** on a managed segment | can send any packet on that segment, spoof its source address | yes |
| **A local unprivileged user** on the host | a shell without root, not in the API socket's group | yes |
| **A network attacker** on the host's other networks | can reach the host's IPs | yes |
| **The operator's own mistakes** | a typo in a rule, a half-applied change, a crash mid-operation | yes — a policy that silently differs from the declared one is treated as a security failure |
| **A tampered download**: a compromised mirror or CDN, an altered cache, a man in the middle of the build host | serves other bytes under a pinned name (Firecracker, the guest kernel, the Alpine base, Ubuntu packages) | yes — Layer 11 |
| **Root on the host**, physical access, a malicious upstream (Firecracker, the kernel, Alpine or Ubuntu signing a bad release) | — | **out of scope**: at that point there is nothing left to defend; pinning only stops *other* bytes, not bad ones that were pinned |

---

## 3. Blast radius, simply

"If X is compromised, what can it reach?"

| Compromised | Can reach | Cannot reach |
|---|---|---|
| **A guest VM** | its own disk and memory; exactly the flows its network declares (OUT rules, replies to IN rules); with `intra: true`, the other VMs of its network; the host's services **only** through replies to what the host asks over vsock | the host (no route, no listening socket, no filesystem shared); VMs of other networks; VMs of its own network by default; any interface or destination not declared; the API |
| **A device** on a managed segment | the host's address on that segment, **only** on the ports its IN rules name, which lead straight into one VM; the host services explicitly allowed (`--managed-host-allow`, default DHCP only) | the API, SSH, anything else on the host; any VM its rules do not target; the trusted networks |
| **A member of the API socket's group** | everything the API offers: create/destroy VMs, change network policy, exec into any VM | — treat membership as root-equivalent |
| **The daemon process** | everything (it runs as root) | — it is the trust anchor; the layers below exist so that the guest never reaches it |

The rest of this document explains why the first two rows hold.

---

## 4. The layers

Ordered from the guest outwards: what an attacker inside a VM meets first.

### Layer 1 — Hardware virtualization (KVM)

- **Stops:** the guest reading or writing host memory, running host code, seeing
  host processes. The guest has its own kernel; a kernel exploit inside it owns
  the guest, not the host.
- **How:** each VM is a KVM virtual machine (Intel VT-x / AMD-V on x86_64,
  virtualization extensions on ARM64). This is the primary isolation boundary;
  every other layer assumes it holds and limits the damage if it does not.
- **Does not stop:** a vulnerability in KVM itself or in the CPU (speculative
  execution side channels between VMs sharing cores). See §7.

### Layer 2 — A minimal VMM (Firecracker)

- **Stops:** attacks through emulated hardware, the classic VM-escape surface.
- **How:** Firecracker emulates a handful of devices — virtio-net,
  virtio-block, virtio-vsock, a serial console and the minimum needed to reset
  the machine — and nothing else: no USB, no PCI passthrough, no graphics. It is written in Rust
  and applies its **built-in seccomp filters** per thread, restricting the host
  syscalls the VMM can make.
- **Where:** `internal/firecracker` (`seccomp.go`).
- **Does not stop:** a bug in those devices. That is what Layer 3 is for.
- **What the filter allows** (Firecracker v1.16.1 defaults, per thread): new
  sockets only `AF_UNIX` stream — no IP, packet or netlink sockets, so no path
  into the host's network stack; `kill` not at all, `tkill` only `SIGABRT` and
  the vCPU kick signal (and only to threads its uid may signal); `openat`
  unrestricted, within the chroot. `connect` is unrestricted on the VMM thread
  (vsock needs it), which leaves abstract Unix sockets reachable — see §7.
- **Verification status:** checked on the running process at every boot and
  restore: every thread of the VMM must be in seccomp mode 2, or the VM is
  killed before it runs (fail-closed); `mh doctor` reports a running VM
  without filters (`seccomp_off`), and `scripts/security-test.sh` asserts it
  for every running VM. This check exists because the filters were **off**
  until 2026-09: the Firecracker Go SDK passes `--no-seccomp` unless seccomp is
  enabled explicitly, which the daemon did not do, while this document said
  otherwise. A VM started before the fix runs unfiltered until it boots again.

### Layer 3 — The jail around the VMM (Jailer)

Assume the guest has escaped into the Firecracker process. It is then:

- **chrooted** (pivot_root) into `<store>/jailer/firecracker/<id>/root/`, which
  holds only that VM's kernel, disk, API socket and vsock socket (hard links on the
  store) — no host filesystem, no other VM's files;
- running as an **unprivileged identity of its own** — a uid and gid from a
  range reserved for the daemon (default 1900000000+), never shared with another
  VM, a volume or any host account (the daemon refuses to start on a range that
  overlaps `/etc/passwd`, `/etc/group` or `/etc/sub[ug]id`). It owns that VM's
  disk and attached volumes and nothing else: not another VM's files, not their
  processes, not their TAPs (each TAP is created with its VM's identity as
  owner, so no other uid can attach to it). An identity returns to the pool only
  once its VM is gone and is never reissued while any process runs as it;
- inside a **per-VM cgroup** with hard limits written by the daemon on every
  boot: `cpu.max` = its vCPUs, `memory.max` = its memory + 64 MiB of VMM
  overhead with **swap disabled**, `pids.max` = vCPUs + small headroom
  (Firecracker never forks). A VM whose limits cannot be applied **does not
  boot** (fail-closed, cgroup v2).
- given `oom_score_adj = 0` while the daemon has `-900`: under host memory
  pressure the kernel kills a VM, never the daemon that holds all of them.

The chroot belongs to the VM's identity, so everything in it — including the
API and vsock sockets the daemon connects to — is under the escaped process's
control. The daemon (root) therefore never connects to a path there as given:

- the jail directory above the chroot must be root-owned and not group- or
  world-writable;
- the path inside the chroot is resolved with `openat2` refusing every symlink,
  magic link, mount crossing and escape from the jail;
- the result must be a socket **owned by that VM's identity** — which also rules
  out a hard link to a host socket;
- the connect goes through that very inode (`/proc/self/fd/N`), so nothing can
  be swapped in between the check and the connect.

A VMM that plants a link to a host socket (the daemon's API, systemd, a
container runtime) gets a refused connection, not root talking to that socket.

- **Where:** `internal/jailer/config.go`, `internal/jailer/cgroup.go`,
  `internal/jailer/sockdial.go`, `internal/firecracker/ratelimit.go`,
  `internal/vm/manager.go` (`applyLimits`), `internal/vm/iolimits.go`.
- **Stops:** reading host files, acting as root, pinning every core, ballooning
  memory, fork-bombing the host, saturating the storage device or the network
  for the other VMs, redirecting the daemon's connections to a host
  socket.
- Disk and network **throughput** are capped by Firecracker's rate limiters:
  every drive and both directions of the NIC, at the lower of the VM's own
  `io_limits` and the daemon's ceiling (on by default: 100 MiB/s and 4000 IOPS
  per drive, 100 Mbit/s per direction). A VM can lower its limits, never raise
  them; a restored VM gets its limits set before it runs a single instruction.
- **Does not stop:** the jailed process is not given a network or PID namespace of its own (Jailer's
  `--netns` / `--new-pid-ns` are not used); what it can do in the host's
  namespaces is bounded by its uid, its chroot and the seccomp filter. In the
  PID namespace that leaves nothing: it cannot `kill`, and `tkill` reaches only
  threads its uid may signal. In the network namespace it leaves abstract Unix
  sockets (see §7).

### Layer 4 — The network

This is the layer a compromised guest will actually push against, because it is
the only one it is *supposed* to use.

**4a. Where a VM's packets can physically go.** Each VM's only NIC is a TAP on
the host:

- attached to **its network's bridge**, as an **isolated bridge port** unless the
  network opted into `intra`: isolated ports exchange no frames with each other,
  only with the bridge — so two VMs on the same network cannot talk, at L2,
  before any firewall is involved. (Same-bridge traffic never reaches nftables;
  this is the only layer that can separate it.)
- or attached to **nothing**, for a quarantined VM: every frame dies at the TAP.
  Off a bridge a TAP is an L3 interface of the host, so that takes two things:
  the TAP never carries a host address (`addrgenmode none` from creation, IPv6
  flushed on detach — no `fe80::` for the guest to reach `[::]`-bound services
  through), and the ruleset drops everything arriving on or leaving through a
  `tap*` interface (4b). `tap*` is reserved: the daemon refuses it as a managed
  interface or an `egress_iface`.
- or absent: a VM created with `no_network` has no NIC at all.

Each bridge port is **pinned to its VM's addresses** (a `bridge microhosted`
nftables table, separate from the one below because only the bridge family sees
frames switched inside one bridge). A frame entering one of our bridges is
accepted only if it is:

- IPv4 with the port's own source MAC **and** source IP (the IP its network
  leased, the MAC derived from it), or
- Ethernet/IPv4 ARP whose frame source, sender hardware address and sender IP
  are all the port's own.

Everything else — another address, a forged ARP reply, IPv6, VLAN-tagged or
any other ethertype — is dropped, and so is every frame from a port the table
does not list. A lease is pinned **before** its TAP is created: if the filter
cannot be installed, the attach fails and the address is released; a stopped
VM does not rejoin its bridge until the filter is in force.

**4b. What the host lets through** — one nftables table, `inet microhosted`,
rendered in full from the declared state and applied atomically (`nft -f`):

| Rule | Stops |
|---|---|
| guest → host (`input`): **drop** from every `mhbr*` bridge | the VM reaching the API, SSH, any host service, even its own gateway address |
| quarantine (`input`, `forward`): **drop** anything on a `tap*` interface, in `input` ahead of the established accept | a quarantined VM, whose TAP is off every bridge, reaching host services or being routed anywhere — including a flow it had open before the cut. On a bridge the IP hooks see the bridge, never the TAP, so this touches no bridged VM |
| cross-segment (`forward`): **drop** between our bridges (one aggregated rule over sets, O(N)) | VM ↔ VM across networks |
| **unknown bridge**: drop both ways, for any `mhbr*` the ruleset does not list | a bridge left by a crash, a network mid-create, a network whose apply failed |
| no egress (default): **drop** towards anything that is not our bridges | calling home, scanning, pivoting to the LAN |
| `allowed_egress`: accept only the listed `(destination, protocol, port)`, then drop | everything else outbound |
| `egress: true` requires `egress_iface`: out through **that interface only** | "internet" silently meaning "the plant LAN / the VPN / Docker networks" behind another interface |
| `egress: true` drops private and special destinations (`@mhprivate`: RFC 1918, CGNAT, link-local, loopback, multicast, reserved) unless `egress_private` | "internet" meaning the LAN on the **same** interface: on a one-NIC host, the router and every device next to it, and a cloud's metadata service (found 2026-09-29: a CI runner VM reached the home router's admin page) |
| return legs of managed-interface rules match `ct direction reply` | the device (or the VM) using the rule's port as a *source* port to open connections to any port on the other side |
| the `forward` chain is **stateless** except for those direction matches | a flow opened under an old policy surviving a tightening: it dies on its next packet |

**4c. Managed interfaces** — the interface facing the untrusted device segment
(`--managed-iface wlan0`) has its *entire* policy owned by the daemon: host
services denied except those listed (default: DHCP), everything forwarded
through it denied except the declared OUT/IN rules, and — crucially — that drop
comes **before** every WAN rule, so no internet-scoped rule can leak packets onto
the device segment. Declaring the interface here, rather than in a separate
firewall file, is itself a security property: two base chains on one hook are
both evaluated, and a hand-written drop would silently override the declared
policy.

**4d. Ingress without the host touching the data.** An IN rule is a DNAT in
`prerouting`: the device connects to the host's address, the kernel rewrites the
destination to the VM's address and forwards the packet. The host **listens on
nothing** and parses nothing — the attack surface a device can reach on the host
is the kernel's IP stack, not a userspace broker.

**4e. Fail-closed application.**

- A network is not attachable until the ruleset listing it is **in force**; if
  `nft -f` fails, the network is rolled back entirely.
- A policy update that cannot be applied is **not persisted**, and the previous
  policy is re-applied.
- Every policy mutation holds one lock through its apply, so two concurrent
  changes cannot install an older snapshot last.
- A failed apply raises `network.ruleset_failed` (event) and `ruleset_failed`
  (doctor).

**4f. Addresses belong to functions.** An IN rule targets an **address**, not a
VM. Those addresses are **pinned**: automatic allocation never hands one out, so
when the VM holding it is destroyed or quarantined, an unrelated new VM cannot
silently start receiving the traffic meant for the function. Only an explicit
claim (a replacement) takes it.

- **Where:** `internal/network/nftables.go`, `portfilter.go`, `manager.go`,
  `bridge.go`, `ipam.go`.
- **Verification:** isolation between VMs, between networks, guest→host, egress
  on/off and fine-grained, managed interfaces and ingress were validated on test
  hardware; the fail-closed paths were fault-injected (`scripts/fault-test.sh`),
  pinning by `scripts/replace-test.sh`.
- **Does not stop:** see §7 — a device's spoofed `src_ip`, and traffic between
  two devices on the same segment (which never crosses the host).

### Layer 5 — The control channel (vsock)

The host must be able to run commands and move files in a VM without a network.
That channel is the most direct link between a guest and the daemon, so it is
one-directional by construction:

- **Only the host dials.** The daemon has no vsock listener (there is no
  `net.Listen` on vsock anywhere in it). A guest cannot open a channel to the
  host; it can only answer when asked. That includes readiness: the daemon
  learns that a guest's agent is up by probing it (a `CONNECT` handshake, no
  command), never from the guest announcing itself — a guest's claim would be
  worth nothing anyway, and hearing it would need a listener.
- **Answers are bounded.** One exec response is capped at 8 MiB; commands time
  out at 30 s, file transfers at 10 min. A guest that answers forever, or never,
  costs the daemon bounded memory and time.
- **Paths are validated** before any file operation (absolute, no `..`, no control
  characters or quotes), identically for the vsock and the offline channel.
- **Where:** `internal/vsock/exec.go`, `internal/storage/offline.go`.
- **Does not stop:** a guest lying in its answers — the channel guarantees who
  speaks, not that the guest tells the truth. Detection is the orchestrator's job.

### Layer 6 — Data in and out (disks and volumes)

- **The host never mounts a guest filesystem.** Mounting an attacker-controlled
  ext4 image exposes the host kernel's filesystem parser. Files are copied into
  and out of a stopped VM or a volume with `debugfs`, a userspace tool, run as
  the identity that owns that image (the VM's, or the detached volume's own): a
  malformed image can at worst compromise a process holding that one image's
  permissions.
- **Configuration goes in before the first boot** (`files` on create and
  replace), through the same `debugfs` path as the VM's identity, and is read
  back byte by byte before the VM boots: no agent inside the guest receives
  it, and no VM runs half-configured. File contents are never kept in the
  VM's record or logged; the record has path, mode, owner and size, plus a
  SHA-256 for files that are not secrets. They are staged on the store (0600, removed at once)
  and end up in the VM's disk, which is as private as any disk (0600, its VM's
  identity).
- **Read-only volumes are read-only at the block device** (`is_read_only` in
  Firecracker), not by a mount option the guest could change.
- **Copy-on-write clones** mean no VM writes into a golden image or another VM's
  disk. A clone is mode 0600, owned by its VM's identity: its VM's alone. A
  volume belongs to the VM it is attached to, and to its own identity while
  detached.
- **The store is private.** Its directories are 0711 (traversable by the jailer
  identity, listable by no one); disks, volumes and console logs are 0600. The
  daemon enforces these modes at every start, so stores created by older
  versions are migrated.
- **The daemon's own state is root's alone.** The database and the template
  catalog decide which paths the root daemon truncates, deletes and boots, so
  they live in `/var/lib/microhosted`, owned by root (database 0600). The daemon
  refuses to start if either file, or any directory above it, is owned by
  anyone else or writable by group or others.
- **Snapshots are sealed.** Memory image and vmstate are copied out of the
  source VM's chroot into fresh root-only inodes (0400) — never renamed, so no
  descriptor Firecracker kept open reaches them — in a 0700 directory. Every
  restore or fork maps that same sealed inode (so forks share the page cache of
  untouched memory), hard-linked into its chroot. The restoring VM's group gets
  a read-only ACL entry only while Firecracker loads the snapshot; it is revoked
  as soon as the load returns, and at every daemon start. No VMM can write the
  snapshot, none keeps a standing grant, and a reissued identity inherits none.
  On a filesystem without POSIX ACLs each restore gets a private copy instead.
  A snapshot that fails these checks at startup is not offered for forks.
- **Output from the host tools that parse guest images is bounded.** `debugfs`,
  `e2fsck` and `resize2fs` output is capped at 1 MiB in the daemon's memory, as
  are the vsock control lines (4 KiB) and exec responses (8 MiB).
- **Images are content-addressed and immutable.** A store image's kernel and
  rootfs are hashed once at import, over the store's own copy, and kept 0444
  in a root-only directory; a new build is a new digest and a tag never moves.
  A VM created by digest — and its replacement, which reuses the digest —
  boots exactly the bytes that were validated, not whatever a path holds now.
  Imports read only files under the daemon's store directory, so the API
  cannot be used to copy an arbitrary host file into a VM. `mh doctor` flags a
  store file gone missing or writable; `mh image verify` re-hashes on demand.
- **Where:** `internal/storage`, `internal/images`.

### Layer 7 — The API

The API can do everything, so who can reach it is the question.

- It serves on a **Unix socket** (`/run/microhosted.sock`), mode `0600` (root) or
  `0660` with a named group. The file permissions *are* the authorization: no
  secret to distribute, leak or rotate, and an access list the host already
  audits.
- It never listens on a network by default. `--addr` opts into TCP, with a
  warning on every start: there is **no authentication** on it — use it only on
  loopback or behind a tunnel, never on a segment a workload or device can reach.
- VMs cannot reach it: the guest → host drop (Layer 4) covers every host address,
  and vsock is host-initiated only (Layer 5).
- **Input to the firewall is validated as a security boundary**: rule fields are
  interpolated into an `nft` script, so anything that is not a canonical IPv4
  address or CIDR, a known protocol, a numeric port and a managed interface is
  rejected before rendering (ruleset injection).
- **Request bodies are parsed strictly**: unknown fields, a key given twice
  (also with different case), anything but one JSON object, and bodies over
  1 MiB are refused. A misspelt policy field cannot silently become "no rule",
  and two readers of one body cannot disagree on what it says.
- **Where:** `internal/api/listen.go`, `internal/api/decode.go`,
  `internal/network/nftables.go` (`ValidateEgressRules`, `ValidateIngressRules`).

### Layer 8 — Host resources

- **Admission:** a launch is refused (503) when it would leave less than
  `--mem-reserve-mb` (512 MB) of the host's available memory, or exceed
  `--max-vms`; at most `--max-parallel-boots` launches run at once. A flood of
  create requests cannot push the host into its OOM killer.
- **Per-consumer quotas:** `--quota CONSUMER=vms:N,mem:MB` caps what the VMs
  labelled `managed-by=CONSUMER` hold at once (429 over it); the label is fixed
  at create, so a VM cannot leave its quota. Set on the daemon only. Any API
  client is root-equivalent, so this contains one consumer's bugs and floods,
  not a malicious consumer.
- **Store space:** disks are thin, so the store keeps `--disk-reserve-mb`
  (1024 MB) free: a launch, snapshot, volume, image import or upload that would
  leave less is refused (503), and `host.disk_low` is raised while the store is
  under it. This bounds what the operator's actions can take, not what running
  guests write (see §7).
- **Per-VM caps** (Layer 3) bound what each running VM can take.
- **The daemon survives memory pressure** (`OOMScoreAdjust=-900`), and systemd
  restarts it forever with a growing delay if it crashes — the VMs keep running
  meanwhile (`KillMode=process`) and are re-adopted.

### Layer 9 — Containment after detection

The layers above limit what a compromised VM can do. This one limits **how long**
it does it, once the orchestrator suspects it:

- **Quarantine in place:** the VM's TAP leaves the bridge, its address is
  released, it is labelled `lease=quarantined` — and it keeps running, reachable
  over vsock, so its memory, processes and disk can be examined. Fail-closed
  order: the network is cut first; nothing ever reconnects it.
- **Replace:** a clean VM (from a template, or a snapshot taken at the function's
  address while it was known-good) takes over the address and labels. If the
  replacement fails, the suspect **stays quarantined** — a function without data
  is preferred to a compromised function with data. A second replace of the same
  VM is refused, so a looping controller cannot flood the host.
- **Events** tell the orchestrator what happened (`vm.died` with the OOM reason,
  `vm.replace_failed`, `network.ruleset_failed`) without polling.
- **Where:** `internal/vm/quarantine.go`, `internal/vm/replace.go`,
  `internal/events`. **Verification:** `scripts/replace-test.sh`,
  `scripts/events-test.sh`.

### Layer 10 — Consistency

A policy the daemon believes in but the host does not enforce is a silent hole.

- **All-or-nothing operations:** creates, forks and network creates are recorded
  before their first side effect and undone completely if they fail — including
  the daemon being killed halfway. Fault-injected at every step
  (`scripts/fault-test.sh`, 21/21).
- **Startup reconciliation:** VMs are re-adopted by PID, checked against their
  command line (PID reuse) and their cgroup (ownership); orphan Firecracker processes, TAPs, jail dirs and cgroups are removed;
  every adopted TAP is converged to its record (on its bridge, or detached if
  quarantined).
- **Drift detection:** `mh doctor` compares the daemon's belief with the host's
  reality (processes, TAPs, bridges, cgroups, disks, leases, the last ruleset
  apply) and reports every difference.

### Layer 11 — What boots: the supply chain and the image store

Every layer above assumes that Firecracker, the jailer and the guest kernel are
the builds they claim to be. They are downloaded, and so is the base of every
guest; a download that could be swapped would make the rest moot.

**Every download is pinned.** `scripts/checksums.sha256` lists the SHA-256 of
each file the host and image pipelines fetch; `scripts/lib/pinned.sh` checks it
before the file is used, and a file whose hash is **not listed** is refused like
a mismatch, before it is even fetched. Changing a version is a reviewed change
to that file, never a side effect of what a server returns that day.

| Download | Used as | How its pin was established | Checked |
|---|---|---|---|
| Firecracker + jailer release tarball (GitHub) | the VMM and the jailer, which runs as **root** | the release's `.sha256.txt`, and the binaries validated on the project's hardware | at install (`install-fc.sh`); `FC_VERSION=latest` only installs if it resolves to a pinned version |
| Guest kernel `vmlinux` (Firecracker CI, S3) | every VM's kernel | **trust on first use**: no checksum or signature is published (gap 7) | at download and on every image build, including the copy already in the store |
| Alpine minirootfs | the base of every Alpine guest, **including `/etc/apk/keys`** | its detached GPG signature, verified with Alpine's release key (fingerprint `0482 D840 22F5 2DF1 C4E7 CD43 293A CD09 07D9 495A`) | at download and on every build, the cached copy too |
| apk packages (`EXTRA_PKGS`, `socat`) | added to an Alpine guest | signed by Alpine; apk verifies each against the keys of the pinned base, and nothing uses `--allow-untrusted` | by apk, at build |
| Ubuntu packages (debootstrap, apt) | Ubuntu guests | the archive's `Release` signature against `ubuntu-archive-keyring.gpg`, named explicitly: a host without it **fails** instead of debootstrap warning and going on unverified; the mirror may be plain http | by debootstrap/apt, at build |

**What boots is what was imported.** The image store (`mh image import`, run by
`make prepare-image` as its last step) copies the kernel and the rootfs into a
root-only directory, keeps them 0444 and hashes them once. An image's digest
covers both files' digests and its default shape; a VM created from
`name:version@sha256:…` boots those exact bytes, and a tag that disagrees with
the digest is refused by the engine itself, not by its client — no gap between
checking a file and booting it by path. A replacement reuses the old VM's
digest. The orchestrator accepts only pinned images. `mh image verify` re-hashes
on demand, and `mh doctor` reports an image file made writable or removed.

**Catalog templates are outside this guarantee:** a template is a name for two
paths, rebuilt in place, and `mh run TEMPLATE` boots whatever is at those paths
now. Fine by hand on a development host; production functions boot images.

**What a digest proves, and what it does not.** It proves that a VM runs the
bytes that were imported — the ones that were validated. It does not prove that
those bytes were trustworthy when they were built: that is what the pins above
are for, and where they stop is listed in gaps 7 and 8.

---

## 5. A worked attack

A sensor on the device segment sends a malformed frame; the Modbus parser in its
VM has a memory-safety bug; the attacker now has root in that VM. What next?

| Attacker tries | Result | Stopped by |
|---|---|---|
| scan the plant LAN / reach the internet | packets dropped in `forward` | L4b — no egress declared |
| reach other sensors on the segment | only the one destination/port the network's OUT rule names | L4b/4c |
| open a connection to the host (API, SSH) | dropped in `input` | L4b — guest → host |
| reach the other VMs | no frames between isolated ports; cross-network dropped | L4a, L4b |
| use the IN rule backwards (bind 1883 as a source port) | dropped: return leg only carries replies | L4b — `ct direction reply` |
| exhaust the host (fork bomb, memory, CPU) | capped by its cgroup; its OOM kill is reported as `vm.died` | L3, L8 |
| flood the daemon through vsock | answers capped at 8 MiB and 30 s; the guest cannot open the channel | L5 |
| escape the VM through a Firecracker device bug | lands as that VM's own uid in a chroot with its own files only, capped by its cgroup, inside a seccomp filter | L2, L3 |
| send forged readings | **not stopped by the engine** — the orchestrator's validation must notice; then quarantine + replace within a VM boot time, suspect kept for forensics | L9 |

The last row is the honest one: isolation contains the attacker, it does not make
the data trustworthy. Detection is the orchestrator's responsibility, recovery is
the engine's.

---

## 6. Operator responsibilities

The engine can only enforce what it is told and what it controls. These are on
the operator:

- **Access layer isolation.** Two devices on the same Wi-Fi BSS or switch reach
  each other without crossing the host (inside the access point's radio stack, or
  in the switch). Enable client isolation (`ap_isolate=1` on an AP, private VLANs
  or port isolation on a switch).
- **Per-device credentials.** An IN rule's `src_ip` can be spoofed on a flat
  segment. Pair it with authentication inside the VM (MQTT credentials or TLS
  client certificates per device).
- **Who is in the socket's group** — root-equivalent.
- **Never expose `--addr`** on a network a device or workload can reach.
- **Least privilege per network:** one function per network where possible; no
  `intra` unless the VMs must talk; `allowed_egress` instead of `egress: true`.
- **Clean sources for replacement:** snapshots used as replacement sources must
  be taken from known-good VMs.
- **Keep Firecracker, the host kernel and the guest images up to date.**

---

## 7. Residual risks and known gaps

Stated plainly, most important first.

1. **VMs are not pinned while the port filter is down.** Each TAP is pinned to
   its VM's MAC and IP (Layer 4), so a compromised VM cannot use another address
   or answer ARP for one. If the filter cannot be installed at startup, the
   daemon still starts; VMs already running keep the filter the previous run left
   in the kernel, and no VM joins a bridge until it is in force (`port_filter_failed`
   in `mh doctor`). After a host reboot there is no previous filter, so nothing
   joins until the install succeeds.
2. **Throughput limits are per device and not shared.** Firecracker has no
   budget across devices: a VM with volumes gets the disk limit on each drive,
   and N VMs together get N times the limit — the ceiling bounds one VM's share,
   not the host's total. Size it for the storage and uplink in use, and lift a
   limit (`0`) only knowingly: the daemon logs it. VMs keep the limits they
   booted with until their next boot.
3. **Running guests can fill the store.** Disks are thin: a clone or volume
   takes space only as its guest writes, and together they may be larger than
   the store. Admission keeps `--disk-reserve-mb` free against new work, but a
   running guest can still write into that reserve, at most at its disk
   throughput limit. If the store fills anyway, every VM on it gets I/O errors.
   `host.disk_low` (event) and `disk_low` (`mh doctor`) fire when the reserve
   is breached; size the reserve to cover the time needed to react, and keep
   each VM's `disk_mb` no larger than it needs.
4. **Console output is capped, not rate-limited.** Firecracker writes the
   guest's serial console to a pipe the daemon drains into `<store>/<id>.log`
   (mode 0600), rotated to `<id>.log.1` at 2 MiB: a VM costs at most 4 MiB of
   console log however much it prints. Output printed while the daemon is
   restarting is dropped; a VM started by an older daemon writes its log
   directly and uncapped until its next boot.
5. **The daemon runs as root.** It needs to create TAPs, bridges, cgroups and
   nftables rules. The layers above exist to keep the guest away from it; a bug
   in the daemon reachable from the guest's answers (vsock) would be serious —
   which is why that input is bounded and parsed minimally.
6. **Side channels.** VMs sharing physical cores may leak data through
   speculative-execution side channels. Mitigate with up-to-date microcode and
   kernel mitigations and, where it matters, by disabling SMT or pinning VMs to
   dedicated cores. Not managed by the engine.
7. **The guest kernel is pinned on trust on first use.** Firecracker CI
   publishes no checksum or signature for its kernels, so the pin records what
   was downloaded (cross-checked against an earlier install and the object's
   unchanged date), not a publisher's statement. Building the kernel from
   source with a reviewed config would close it (`docs/architecture.md`).
8. **Image builds are not reproducible.** apk and apt install each package's
   current version, verified by signature but not pinned: two builds of the same
   spec differ, and a package that was bad but correctly signed is not stopped.
   The image digest pins the result — test that result, then deploy that digest;
   a rebuild is a new version to test again. `mh build` narrows it (an image
   spec may pin `pkg=version`) without closing it. Its `run:` steps execute as
   root on the build host inside a chroot, which does not confine root: an
   image spec is code the operator runs, like a Dockerfile — build only specs
   you trust. Its copies are resolved inside the chroot, so a symlink in the
   image cannot aim a write at the host, and file sources cannot leave the
   build context. `plan`, `apply`, `run` and `up` build what a `build:`
   lacks, so they run those steps too. Downloads are hashed in the user's
   cache and again, by root, in a copy under the build's root-only directory;
   root reads only that copy.
9. **A removed tag can be bound again.** Tags never move while they exist, but
   once `mh image rm` removes a tag (or its image), the same `name:version` can
   be imported with other bytes. A pinned reference is refused if its tag
   disagrees, so a spec with `image: …@sha256:…` is not affected; a bare
   `mh run name:version` boots the new bytes. Pin the digest wherever it
   matters. `build:` gets its pin from `mh build`, which would otherwise take a
   fingerprint tag at its word: it checks the tag against the digest this
   user's build recorded for it (`$XDG_STATE_HOME/microhosted/builds`) and
   refuses a mismatch or an unrecorded tag until `mh build --adopt`.
10. **Abstract Unix sockets on the host are reachable from an escaped VMM.**
   They belong to the network namespace, not to the filesystem, so the chroot
   does not hide them, and the VMM shares the host's namespace; its seccomp
   filter allows `AF_UNIX` sockets and `connect`. A compromised VMM could reach
   any service listening on one (`ss -xl | grep @`), as its own uid. Keep such
   services off the host — notably containerd shims, the subject of
   CVE-2020-15257 — until each VMM gets its own network namespace (Jailer
   `--netns`; roadmap Phase 3).
11. **Public DNS resolvers** are configured in guests; on networks without egress
   they are unreachable (DNS fails closed), with egress they are reached through
   the same NAT.
12. **Projects are not a security boundary.** They keep plant specs from
    removing each other's objects by mistake; anyone with the engine's API
    can change any of them. A project's lock and failure records live in its
    state directory (0700, the user's), never in a shared directory: the lock
    names the pid `down` signals, which must be that user's `mh-orchestrator`.
13. **Events are in memory.** They are a notification channel, not an audit log; a
   daemon restart starts a new epoch. Durable state is the VM records.
14. **Availability, not isolation:** restarting Docker removes the `DOCKER-USER`
   rules that let VM egress through Docker's forward drop; egress fails (closed)
   until the next network change or daemon restart.

---

## 8. How to verify

| What | How |
|---|---|
| crash consistency, fail-closed firewall | `sudo scripts/fault-test.sh` — `mh doctor` must be clean after every round |
| pinning, quarantine, replace | `scripts/replace-test.sh` |
| event delivery, resume, reset | `sudo scripts/events-test.sh` |
| drift right now | `mh doctor` |
| an image still has the bytes it was imported with | `mh image verify NAME:VERSION` |
| the pinned downloads | a file altered in `images/cache/` or the store makes the next `make prepare-image` stop with `is not the pinned …`; an unlisted version stops with `no pinned SHA-256` |
| the ruleset in force | `sudo nft list table inet microhosted` |
| every invariant of this document on the live host: daemon, store, every running VM's identity, capabilities, seccomp, cgroup, jail and descriptors, and isolation seen from inside test guests | `sudo scripts/security-test.sh` (without root the jail, descriptor and nftables checks are skipped) |
| a VM's limits | `cat /sys/fs/cgroup/microhosted/<id>/{cpu.max,memory.max,memory.swap.max,pids.max}` |
| jail sockets: symlinks and foreign owners refused | `sudo go test ./internal/jailer -run DialSocket -v` |
| nothing listening for devices | `ss -ltnup` on the host shows no ingress port |

`scripts/security-test.sh` asserts the invariants; it does not attack. Planned
(roadmap Phase 3): an adversarial suite that runs a deliberately vulnerable
parser, exploits it from the device side, and asserts every row of §5
automatically.
