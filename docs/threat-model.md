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
| **Root on the host**, physical access, a malicious Firecracker/kernel build | — | **out of scope**: at that point there is nothing left to defend |

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
  syscalls the VMM can make; the daemon never disables them.
- **Where:** `internal/firecracker`.
- **Does not stop:** a bug in those devices. That is what Layer 3 is for.
- **Verification status:** the seccomp filters are Firecracker's defaults and are
  active unless `--no-seccomp` is passed (it never is); they have not yet been
  independently verified on the running process (roadmap Phase 3).

### Layer 3 — The jail around the VMM (Jailer)

Assume the guest has escaped into the Firecracker process. It is then:

- **chrooted** (pivot_root) into `<store>/jailer/firecracker/<id>/root/`, which
  holds only that VM's kernel, disk, API socket and vsock socket (hard links on the
  store) — no host filesystem, no other VM's files;
- running as an **unprivileged user** (uid 123, gid 100), not root;
- inside a **per-VM cgroup** with hard limits written by the daemon on every
  boot: `cpu.max` = its vCPUs, `memory.max` = its memory + 64 MiB of VMM
  overhead with **swap disabled**, `pids.max` = vCPUs + small headroom
  (Firecracker never forks). A VM whose limits cannot be applied **does not
  boot** (fail-closed, cgroup v2).
- given `oom_score_adj = 0` while the daemon has `-900`: under host memory
  pressure the kernel kills a VM, never the daemon that holds all of them.

- **Where:** `internal/jailer/config.go`, `internal/jailer/cgroup.go`,
  `internal/vm/manager.go` (`applyLimits`).
- **Stops:** reading host files, acting as root, pinning every core, ballooning
  memory, fork-bombing the host.
- **Does not stop:** disk and network **throughput** (no I/O rate limits yet).
  The jailed process is not given a network or PID namespace of its own (Jailer's
  `--netns` / `--new-pid-ns` are not used); what it can do in the host's
  namespaces is bounded by its uid, its chroot and the seccomp filter.

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
- or absent: a VM created with `no_network` has no NIC at all.

**4b. What the host lets through** — one nftables table, `inet microhosted`,
rendered in full from the declared state and applied atomically (`nft -f`):

| Rule | Stops |
|---|---|
| guest → host (`input`): **drop** from every `mhbr*` bridge | the VM reaching the API, SSH, any host service, even its own gateway address |
| cross-segment (`forward`): **drop** between our bridges (one aggregated rule over sets, O(N)) | VM ↔ VM across networks |
| **unknown bridge**: drop both ways, for any `mhbr*` the ruleset does not list | a bridge left by a crash, a network mid-create, a network whose apply failed |
| no egress (default): **drop** towards anything that is not our bridges | calling home, scanning, pivoting to the LAN |
| `allowed_egress`: accept only the listed `(destination, protocol, port)`, then drop | everything else outbound |
| `egress: true` requires `egress_iface`: out through **that interface only** | "internet" silently meaning "the plant LAN / the VPN / Docker networks" |
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

- **Where:** `internal/network/nftables.go`, `manager.go`, `bridge.go`, `ipam.go`.
- **Verification:** isolation between VMs, between networks, guest→host, egress
  on/off and fine-grained, managed interfaces and ingress were validated on test
  hardware; the fail-closed paths were fault-injected (`scripts/fault-test.sh`),
  pinning by `scripts/replace-test.sh`.
- **Does not stop:** see §7 — address spoofing *inside* one network, a device's
  spoofed `src_ip`, and traffic between two devices on the same segment (which
  never crosses the host).

### Layer 5 — The control channel (vsock)

The host must be able to run commands and move files in a VM without a network.
That channel is the most direct link between a guest and the daemon, so it is
one-directional by construction:

- **Only the host dials.** The daemon has no vsock listener (there is no
  `net.Listen` on vsock anywhere in it). A guest cannot open a channel to the
  host; it can only answer when asked.
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
  the jailer's unprivileged uid: a malformed image can at worst crash that
  process.
- **Read-only volumes are read-only at the block device** (`is_read_only` in
  Firecracker), not by a mount option the guest could change.
- **Copy-on-write clones** mean no VM writes into a golden image or another VM's
  disk.
- **Where:** `internal/storage`.

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
- **Where:** `internal/api/listen.go`, `internal/network/nftables.go`
  (`ValidateEgressRules`, `ValidateIngressRules`).

### Layer 8 — Host resources

- **Admission:** a launch is refused (503) when it would leave less than
  `--mem-reserve-mb` (512 MB) of the host's available memory, or exceed
  `--max-vms`; at most `--max-parallel-boots` launches run at once. A flood of
  create requests cannot push the host into its OOM killer.
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
| escape the VM through a Firecracker device bug | lands as uid 123 in a chroot with its own files only, capped by its cgroup, inside a seccomp filter | L2, L3 |
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

1. **Address spoofing inside one network.** The engine does not pin a VM's source
   IP or MAC to its TAP. A compromised VM can configure another address of its own
   subnet and answer ARP for it; the host's neighbor cache may then send it
   traffic meant for that address — including traffic from an IN rule. Isolated
   ports stop VM↔VM frames, not VM↔host ARP. **Mitigation today:** one function
   per network (the recommended topology), so there is no other address worth
   stealing. **Planned:** per-TAP source filtering.
2. **No disk or network throughput limits per VM.** CPU, memory and PIDs are
   capped; I/O is not. A compromised VM can saturate the storage device or its
   bridge and degrade the other VMs. Firecracker's per-drive and per-interface
   rate limiters are not wired yet.
3. **The console log has no size limit.** A guest's serial console is written to
   `<store>/<id>.log` on the host (truncated at each boot). A guest printing
   endlessly to its console grows that file until the store is full.
   **Planned:** a cap.
4. **The daemon runs as root.** It needs to create TAPs, bridges, cgroups and
   nftables rules. The layers above exist to keep the guest away from it; a bug
   in the daemon reachable from the guest's answers (vsock) would be serious —
   which is why that input is bounded and parsed minimally.
5. **Side channels.** VMs sharing physical cores may leak data through
   speculative-execution side channels. Mitigate with up-to-date microcode and
   kernel mitigations and, where it matters, by disabling SMT or pinning VMs to
   dedicated cores. Not managed by the engine.
6. **Seccomp and cgroup values not independently audited** on a running process;
   they are Firecracker's defaults and the daemon's written limits, respectively
   (roadmap Phase 3: an adversarial suite that asserts them).
7. **Public DNS resolvers** are configured in guests; on networks without egress
   they are unreachable (DNS fails closed), with egress they are reached through
   the same NAT.
8. **Events are in memory.** They are a notification channel, not an audit log; a
   daemon restart starts a new epoch. Durable state is the VM records.
9. **Availability, not isolation:** restarting Docker removes the `DOCKER-USER`
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
| the ruleset in force | `sudo nft list table inet microhosted` |
| a VM's limits | `cat /sys/fs/cgroup/firecracker/<id>/{cpu.max,memory.max,pids.max}` |
| nothing listening for devices | `ss -ltnup` on the host shows no ingress port |

Planned (roadmap Phase 3): an adversarial suite that runs a deliberately
vulnerable parser, exploits it from the device side, and asserts every row of §5
automatically.
