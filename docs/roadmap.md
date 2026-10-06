# Roadmap — a general engine, orchestrators on top

> Written 2026-09-18. Supersedes the priority list in `PROJECT.md` → "Needs and
> session plan" and the gap list in `docs/iot-edge.md`: several of those items
> are already closed (see "Where we actually are"), and the ones that remain
> need a different order than the one originally written.

## The thesis

**MicroHosted is an engine; the OT gateway is its first orchestrator.** The
engine's job is to hand out isolated execution units with a lifecycle, a network
policy, copy-on-write storage and a host-initiated control channel. What you do
with them — poll industrial sensors, run untrusted agent code, detonate a
sample — belongs above the line.

The corollary governs the whole order below: **generality is not designed, it is
demonstrated by the second consumer.** Abstracting now would mean inventing an
engine API against a single imaginary orchestrator, and it would be wrong. The
path is: draw the boundary, write the OT orchestrator strictly above it, write a
thin second orchestrator, then delete whatever did not generalise.

## Where we actually are (2026-09-18)

Closed, hardware-validated — **`docs/iot-edge.md` still lists some of these as
gaps and is stale on those rows**:

| Item | Evidence |
|---|---|
| ARM64 | the daemon runs on ARM64 hardware as well as x86_64; this was the existential risk |
| Fine-grained egress (`allowed_egress`) | `internal/network/nftables.go`, `ValidateEgressRules` + tests — listed as gap #1, it is done |
| Ultra-minimal image | `scripts/build-rootfs-alpine.sh`, template `alpine-py`: 42 MB PSS vs 123 MB on Ubuntu Noble |
| Density on target hardware | 174 devices (network + egress + VM each) on an 8 GB ARM64 host, **flat** deploy latency 1783→1856 ms, 34 MB RAM/device, ~0 disk per clone |
| Network-per-VM topology | chosen deliberately; per-source-IP egress on a shared network was rejected (would need saddr + per-tap anti-spoofing) |
| Guest cannot open a channel to the host | `internal/vsock` always dials; there is no `net.Listen` in the daemon |
| Reset to clean + forensics | restore 109 ms, fork 113 ms, `Fork(quarantine=true)` gives a bridge-less TAP |

> Update 2026-09-23: Phases 0 and 0b are closed — 0b validated on test hardware by
> `scripts/fault-test.sh` (21/21) and a real host reboot with `autostart`. VMs
> now carry an optional unique `name` and mutable `labels` (see Phase 1).

Open, and the subject of this document: **there is no orchestrator at all.**
Everything above is primitives. A VM is created and lives forever; nothing
resets it, nothing notices it has been compromised for twelve hours, nothing
caps how many of them exist.

## Phase 0 — Close what is verified open

Not a product phase: debt with a name and a line number. Each item below was
confirmed by reading the code or the live host, not inferred.

1. **Bound the agent response.** `internal/vsock/exec.go` accumulated a guest's
   reply in an unbounded `strings.Builder`. The guest is untrusted by design, so
   this was a path from a compromised VM into the daemon's heap: answer an exec
   with an endless stream and the host OOMs while holding every other VM. It is
   the only route we found from inside a VM back to the host.
2. **The API is the trust anchor, so treat it as one.** It bound `0.0.0.0` with
   no authentication, while the daemon runs as root and exposes
   `POST /v1/vms/{id}/exec` and `POST /v1/networks`. Whoever reaches it *defines
   the network policy*. It now serves on a Unix socket whose file permissions
   are the authorization (`internal/api/listen.go`): no secret to distribute,
   rotate or leak, and an access list the host already audits and revokes.
   `--addr` still opts into a TCP port, loudly and for a tunnel or a loopback
   dashboard — never for a segment the workloads can reach.
3. **One authority over the netfilter hook.** The device-facing interface was
   governed by a hand-written table the daemon could not see, and the two
   contradicted each other in silence: network `ot` declared `allowed_egress` to
   `192.168.50.0/24:502/tcp`, the daemon rendered the accept correctly, and a
   separate `oifname "wlan0" drop` annulled it. A policy declared through the API
   that is not the policy in force is worse than no policy. Done:
   `--managed-iface` hands the daemon an interface's whole policy, and an egress
   rule naming that interface is the only way through it, in either direction
   (`types.EgressRule.Iface`, `internal/network/nftables.go`), `input` chain
   included. The old table is removed at deploy time — two authorities is the
   bug, so one must go.
4. **`egress: true` must name a destination, or go.** It does not mean
   "internet": it means everything that is not one of our bridges — the plant
   LAN, the VPN, the Docker networks. In an OT gateway that is the pivot to the
   trusted side. Decided 2026-09-19: it must name its **exit interface**
   (`egress_iface`, e.g. `eth0`) and leaves through that one only — never a
   VPN, a Docker bridge or a managed interface. Built in Phase 0b.

Exit criterion: no known path from a compromised guest to the host, or to any
network the operator did not declare.

## Phase 0b — The engine under failure

> Added 2026-09-19. Moved ahead of the orchestrator on purpose: the density
> tests measured the path where everything works. An orchestrator does
> thousands of creates a day, and what it inherits from the engine is how the
> engine behaves when one of them breaks. The host reboot that wiped every VM
> (`Reconcile` had never been exercised) is the proof that this path is not
> tested.

Every item was found by reading the code, not by an incident — except the
reboot, which was one.

1. **An orphan detector first** (`GET /v1/doctor`, `mh doctor`). It compares
   what the daemon believes exists against what the host actually has:
   Firecracker processes, TAPs, bridges, jail dirs, cgroups, clones, console
   logs, IP leases, and whether the last ruleset applied. It changes nothing.
   It goes first because it is how every item below is verified, and it is the
   "zero orphans" instrument Phase 2's exit criterion already asks for.
2. **The firewall fails closed.** `network.Manager.Create` persisted the network
   and brought its bridge up *before* applying the ruleset. When `nft -f`
   failed, the network stayed — and since every chain is `policy accept` with
   drops keyed on `@mhbridges`, a bridge missing from the ruleset in force was
   **open**: its VMs reached the host and the LAN. Now: a network is not
   attachable until its rules are in force, a failed apply rolls it back, a
   policy update that cannot be applied is not persisted, and the ruleset drops
   any `mhbr*` bridge it does not know, in both `input` and `forward`. Every
   mutation holds one lock through its apply, so two concurrent creates can no
   longer land an older snapshot of the networks last.
   `egress: true` names its interface here (Phase 0, item 4).
3. **Creation survives the daemon dying halfway.** A VM used to be recorded
   only at the end of `Create`/`Fork`; a crash in between left a clone, a TAP,
   a jail dir, a cgroup and — with `KillMode=process` — a live Firecracker
   nobody knew about. Now the record is written as `creating` before the first
   side effect and startup undoes whatever is still `creating`. Startup also
   kills any Firecracker whose ID it did not adopt, and removes jail dirs and
   cgroups of VMs that are not running. Rollbacks log what they fail to clean
   instead of discarding it.
4. **The state tells the truth.** Nothing watched a VM after boot: one killed
   by the OOM killer stayed `running` until the next daemon restart. A monitor
   notices the death, releases what a powered-off VM does not hold, and marks
   it `stopped` with `last_exit` (when, and whether the cgroup OOM-killed it).
   It does **not** restart it — that is policy, and policy belongs to the
   orchestrator (same reasoning as `autostart` not being a supervisor). One
   lifecycle operation per VM at a time: a second one gets a 409 instead of
   racing the first.
5. **Admission control.** A create, fork, start or restore is refused when the
   host's `MemAvailable`, minus what in-flight boots will take, would drop below
   a reserve (`--mem-reserve-mb`, default 512). Optional hard cap
   `--max-vms`. At most `--max-parallel-boots` launches run at once (default 4);
   the rest queue. Accounting by `mem_mb` was rejected: guest memory is
   allocated lazily, so a 128 MB VM costs ~34 MB and a strict commit limit
   would cap an 8 GB host at ~40 VMs instead of the measured 174. The daemon gets
   `OOMScoreAdjust=-900`, and each Firecracker is reset to 0, so that under
   global memory pressure the kernel kills a VM (which the monitor reports),
   never the daemon holding every VM.
6. **Fault injection.** Failpoints at each step of create/fork
   (`MICROHOSTED_FAULTS=point:error|crash`, read once at startup, inert
   otherwise) and `scripts/fault-test.sh`: bursts of creates with a fault at
   each point, `kill -9` of the daemon mid-burst, a restart — and `mh doctor`
   clean after every round. The real reboot with `autostart` is one more case.

Exit criterion: every injected fault at every step ends in either a working VM
or nothing at all, `mh doctor` is clean after each round, and no
failure leaves a network reachable that the policy says is closed.

> Validated 2026-09-22 on test hardware: `scripts/fault-test.sh` 21/21 (9 failpoints ×
> error/crash, plus SIGKILL mid-burst at three delays). The first run found a
> real bug: a booted Firecracker was not recognised as the daemon's own (Jailer's
> `pivot_root` hides the chroot from `/proc/<pid>/root`), so undo, sweep and
> doctor could not see it. Ownership is now read from its cgroup.

Phase 5 keeps what this phase does not cover: the store filling up, a VM that
refuses to die, the guest agent going silent, Docker taking `DOCKER-USER` down,
and the long soak.

## Phase 1 — The engine/orchestrator line

Today the only consumer of `vm.Manager` is the HTTP API, and its surface is
CRUD-shaped (`Create`/`Destroy`/`Fork`/`Snapshot`/`Exec`).

**Done (2026-09-23): identity.** A VM has an optional `name` — an alias of *that
instance*, unique while it exists and never changed — and `labels`, mutable and
selectable (`GET /v1/vms?label=k=v`, `mh ps -l`). They split two things a lease
must not confuse: the name says *which instance*, a label says *what it serves*
(`sensor=ts-01`) and *who owns it* (`managed-by=…`). A replacement gets a new
name and the same labels; a fork inherits neither, so a quarantined copy taken
for forensics never claims the sensor. A pipeline therefore selects by label,
not by a fixed name, which would 409 while the previous instance still exists.

What is still missing before an orchestrator can be written without reaching
into the engine:

1. **Lease over a pool.** *"Give me a clean instance of template T, I hold it for
   N seconds, then it is destroyed or quarantined."* That sentence is identical
   for the OT pull loop and for an AI sandbox, which is why it is the primitive
   worth extracting first. It builds on `Restore`/`Fork`, both already validated.
   Decided (2026-09-19): the replacement **inherits the IP** of the VM it
   replaces, so `allowed_ingress` rules and everything outside that points at
   the address stay valid, and it inherits the sensor's label. The order is
   fixed: quarantine first (the suspect releases its IPAM reservation), claim
   after — `ClaimVM` reserves exclusively, so the reverse fails.
   **Done and validated on test hardware (2026-09-23):** quarantine of a *running* VM in
   place (`POST /v1/vms/{id}/quarantine`, `mh quarantine`): TAP off the bridge,
   IP released, `quarantine` + `lease=quarantined`, VM alive and reachable over
   vsock. What is left of the lease is the lease itself.
   **Done and validated on test hardware (2026-09-23):** ingress addresses are pinned —
   automatic allocation never hands out a `to_ip`, only an explicit claim does
   (`guest_ip` on create / `mh run --ip`, or a fork). The address belongs to the
   function, not to the VM, so the gap between quarantine and claim can no
   longer give the function's traffic to an unrelated VM.
   **Done and validated on test hardware (2026-09-23):** atomic replace
   (`POST /v1/vms/{id}/replace`, `mh replace`), old → quarantine | stop |
   destroy; `scripts/replace-test.sh` 31/31 (pinning, all three dispositions,
   snapshot source, no-flood 409, bad source untouched, doctor clean).
   Decided (2026-09-23): **detection is the orchestrator's** (health checks,
   anomalous data); a compromise is one more cause of failure and ends in the
   same action — a replacement takes the function's place. The engine offers
   the safe primitives: pinned addresses, an atomic `replace` (old VM →
   quarantine | destroy, never reconnected if the replacement fails), typed
   events. **A persistent failure must not flood the host with VMs:** growing
   back-off between replacements and, after N failures, the function goes
   *degraded* with an alert instead of retrying forever.
2. **Typed events.** **Done and validated on test hardware (2026-09-23):**
   `GET /v1/events` (SSE) + `mh events`: created/started/stopped/died (with the
   OOM reason)/restored/destroyed/quarantined/replaced/replace_failed/
   autostart_failed and `network.ruleset_failed`. Resumable by `epoch:seq`
   (ring of 1024), `reset` when the gap cannot be filled, slow subscribers are
   dropped, never waited for. `scripts/events-test.sh` 10/10. Not yet: vsock
   exec timeouts and cgroup memory pressure short of an OOM kill.
3. **Per-orchestrator quota.** The global half of admission control landed in
   Phase 0b (`--mem-reserve-mb`, `--max-vms`, `--max-parallel-boots`); what is
   left is a quota per consumer — labels give it something to count by
   (`managed-by`) — so one orchestrator cannot starve another, and in push mode
   that is exactly the DoS `docs/iot-edge.md` already anticipates.
   **Implemented (2026-09-26, not yet validated on test hardware):**
   `--quota CONSUMER=vms:N,mem:MB` and `--quota-default` on the daemon, 429
   over it, counted on running + launching VMs (quarantined included) and
   their `mem_mb`; `managed-by` is fixed at create; usage in `GET /v1/system`
   and `mh info`.

Then the cut: `internal/engine` (what any orchestrator may use) against
`internal/orchestrator/*`. The HTTP API becomes **one more consumer**, not the
front door.

Exit criterion: the OT orchestrator compiles without importing `internal/vm`.

## Phase 2 — OT pull orchestrator v1 (the product)

`restore → interrogate → validate → extract → dispose`, with the three lifetimes
already defined in `docs/iot-edge.md` (per transaction / per window / per
anomaly). Watchdog plus circuit breaker, and the breaker's action is
`quarantine=true`: the suspect VM is **not** destroyed, it is frozen without a
network so it can be investigated while a clean one takes over the sensor. That
is not a robustness feature, it is the compromise detector.

The real workload already exists (`examples/modbus-pull`: a C parser and a
stdlib simulator) and the ceiling is measured (174 devices). One VM per sensor
holds for a real plant — that is no longer an estimate.

The data half of this phase — how readings leave the VMs at 100–200 sensors
without `/exec` in the data path (vsock DataPort stream, fd passing, journal,
declarative registry) — is designed in `docs/ingestion.md`. The control half —
project spec, lifetimes, redundancy and the VM budget — is being designed in
`docs/orchestrator.md`.

Exit criterion: N sensors unattended for 72 h on the target hardware, zero orphans (VM,
network, tap, cgroup, chroot), and the loop survives killing the daemon
mid-cycle — `Reconcile` exists, it has never been exercised under an
orchestrator.

### Open: a project spec that works on another host (noted 2026-09-28)

**The problem, as a newcomer meets it.** Someone clones the repo and tries
`orchestrator/examples/website/microse.yml`. Its `image:` carries the digest of the
author's build, which no other host has, and the first thing they see is

```
functions.site: image alpine-py:1.0@sha256:cf6c…: engine 409: image conflict:
alpine-py:1.0 is sha256:fe7f…, not sha256:cf6c…
```

(or *not found*), with nothing saying what to do. The same happens to any spec
moved between two plant hosts.

**The cause is not the digest.** A digest covers only content — the kernel's
and rootfs's hashes and the default shape, never the host, path or import time
— so the same files imported anywhere give the same digest. What breaks it:

- every host **builds** its images (`quick-setup.md`), and builds are not
  reproducible (`mkfs.ext4` UUID and timestamps, apk installing current
  package versions: threat-model gap 8), so every host gets another digest;
- there is **no way to move an image** between hosts: only copying the exact
  `.ext4` and kernel by hand and importing them with the same shape;
- a digest is per **architecture**: an x86_64 spec cannot run on aarch64.

Dropping the digest is not the fix: it is what makes the same spec mean the
same bytes everywhere — without it `alpine-py:1.0` would silently be different
software on every host. The design already says images are built **once,
outside the plant** (`docs/orchestrator.md` §4); the missing piece is carrying
them.

**Plan, most frustration removed first:**

1. **Examples and errors that say what to do.** **Examples done (2026-09-28):**
   they carry no digest any more — `build: images/alpine` — so a fresh clone
   runs them with `mh-orchestrator run`: the first plan builds the image
   (`sudo` once) and later ones reuse it. Still open: `plan` answering an unknown or mismatched
   image with the fix instead of a bare engine 409.
2. **`mh image export NAME:VERSION -o FILE.tar` / `mh image load FILE.tar`**: a
   bundle of kernel + rootfs + manifest; `load` re-hashes and refuses a bundle
   whose content does not match the digest it declares. Build once, ship the
   file, and the spec works unchanged on every host of that architecture.
3. **Published example images**: the example images built once and attached to
   a release (per architecture), so the digests in `orchestrator/examples/` are
   real everywhere: `mh image load` + `mh-orchestrator run` works on a fresh
   clone.
4. **Multi-architecture references**: an index binding `name:version` to one
   digest per architecture (as OCI image indexes do), so one spec serves x86_64
   and aarch64 gateways.
5. **Reproducible builds** (optional once 2 exists): pinned apk versions, a fixed
   filesystem UUID and hash seed, `SOURCE_DATE_EPOCH`. It lets anyone rebuild
   and check a published digest, rather than being what makes specs portable.
   `mh build` is where it lands (it already pins the base and kernel, and a
   spec may pin package versions); ext4 inode times and the readdir order
   `mkfs.ext4 -d` copies in are what is left.

Exit criterion: on a fresh clone of the repo, the website example runs on
x86_64 and aarch64 with no edit to its YAML.

### Open: build and projects, where they stand (noted 2026-09-28)

Done on 2026-09-28, uncommitted at the end of the session: `mh build` (Alpine
and Ubuntu bases, `build.yml`, fingerprint cache, `--no-cache`/`--no-build`),
image defaults `command`/`health` (E8), `mh run` starting an image's command,
`${VAR}`/`.env` and `build:` in project specs, projects (`project=` label,
adoption of older objects) and the compose verbs `mh up [-d]`, `down`, `plan`,
`apply`, `status`, `failures`, `validate`; examples moved to `build.yml` +
`microse.yml`; `docs/uses.md` (a static website, with measured capacity).

Validated on the x86_64 host: an Alpine + nginx image built with `mh build`,
run by the orchestrator (image defaults, ~1 s to serving), load-tested; an
existing deployment adopted as a project with labels only; two projects side
by side; `mh down` of one leaving the other intact.

Still open, in the order they matter:

1. **Validate on hardware what only tests cover:** an Ubuntu build
   (`orchestrator/examples/app-ubuntu`: debootstrap, apt, systemd agent); the
   fingerprint cache end to end (`mh build` twice → the second builds nothing,
   no sudo); `build:` through `mh plan`/`mh up` with a real build; a changed
   build context rolled out by `mh apply` to a running `mh up -d`.
2. **`replicas: N`** for a persistent function (compose's `deploy.replicas`):
   N VMs `<project>-<function>-<i>`, addresses automatic or `ips: [...]`, kept
   at N by the supervisor, updated one at a time so one always serves. Traffic
   is not spread by having N VMs: document a host proxy `upstream` over their
   addresses; an ingress rule to several `to_ip` is an engine change, later.
   Today: two functions with the same `build:` do the same by hand.
3. **`mh up -d` across reboots:** a systemd unit per project (or one that
   starts every project's `up`), so a background `up` survives a host restart.
4. **Reproducible builds** (see "a project spec that works on another host", 5):
   the fingerprint already gives the same *tag* on every host; the same
   *digest* needs fixed ext4 inode times, readdir order and pinned packages.
5. **Default nginx settings worth a build-time warning:** Alpine's nginx ships
   `gzip` off and an `access_log` on the root disk; on a 64 MB VM, ~100
   concurrent large responses exhaust guest TCP memory (`TCPAbortOnMemory`).
   Measured and documented in `docs/uses.md`; the examples set gzip on, the
   log off and 128 MB.

## Phase 3 — Demonstrability

In OT the guarantee *is* the product, and right now it is arguable but not
demonstrable. Two deliverables:

- **A written threat model** — **done (2026-09-23): [threat-model.md](threat-model.md)**:
  what is contained, what is not, and explicitly that the access layer is not ours. Two sensors on the same Wi-Fi BSS reach
  each other inside `mac80211`, below netfilter, exactly as two ports on the
  same bridge do — which is why we isolate bridge ports at L2 and why the AP
  needs `ap_isolate`. No nftables rule can substitute for either.
- **An adversarial suite in CI**: a deliberately vulnerable parser, exploited
  from the sensor side, asserting that it reaches neither the host, nor another
  VM, nor an undeclared network, that it cannot exhaust the daemon, and that a
  clean VM is serving ~100 ms later.

The hardening left open in `SESSIONS.md` is done: validating seccomp exposed
that it was **off** in every VM (the Go SDK passes `--no-seccomp` by default);
it is now on, checked at every boot (fail-closed) and by `mh doctor`, and
`scripts/security-test.sh` asserts it together with the actual values under
`/sys/fs/cgroup/microhosted/<id>`, identities, capabilities, jail contents,
open descriptors and network isolation from inside guests. Still open here:
a network namespace per VMM (Jailer `--netns`), which closes abstract Unix
sockets (threat-model §7).

Exit criterion: an external auditor reproduces it with a `make` target.

## Phase 4 — Second orchestrator, deliberately thin (AI sandboxing)

Run untrusted agent code: no network or a narrow `allowed_egress`, artifacts out
over vsock, reset between tasks. **This is not a product yet** — it is the test
of the Phase 1 boundary. Everything it needs that `internal/engine` does not
offer is, precisely, the list of what was not general.

Exit criterion: it is written only against `internal/engine`, and every missing
capability is written down before it is added.

## Phase 5 — Resilience under failure, not under load

Load is measured. What is untested is behaviour when something breaks: `nft -f`
fails halfway, the store fills, a VM refuses to die, the guest agent stops
answering, Docker restarts and takes the `DOCKER-USER` rules with it (already
documented as a real risk in `docs/networking.md`). Fault injection plus the
soak test still open as Stage 10.

A noisy neighbour is a failure too. CPU, memory and PIDs are capped per VM
(cgroup), and since 2026-09-25 so are disk and network throughput
(Firecracker's rate limiters per drive and per NIC direction, under a
daemon-wide ceiling a VM may only lower). Two
more found while writing the threat model (2026-09-23): the guest's console log
on the host has no size cap (a guest printing forever fills the store), and a VM
can claim another address of its own network towards the host (no per-TAP
source filtering) — see [threat-model.md § 7](threat-model.md#7-residual-risks-and-known-gaps).

Exit criterion: every failure mode has defined, tested behaviour, under one hard
rule — **if the network policy cannot be applied, the VM does not start.**

## Phase 6 — What a plant will ask for

Push mode (DNAT + NFQUEUE trigger + anti-DoS, which now has something to stand
on thanks to Phase 1's admission control), the blind RS-485→vsock serial bridge,
and a mass pool from a shared snapshot as a first-class operation.

## Cross-cutting — Installation and usage experience (noted 2026-09-28)

The engine's guarantees hold, but getting to a first working result takes
knowing things the tools do not say. Each friction point below was hit in a
real session, trying to serve a page from a microVM to the LAN. The rule for
all of them: **an error or a missing step says what to do next**, and the
secure path is the easy one; no security property is traded for convenience.

**Installation**

- `mh-orchestrator` (which `mh up`/`down`/`plan`… run) is installed with `mh`
  by `make install-cli` (2026-09-28) and by `make install-service`/`full-install`
  (2026-09-30), and `mh up -d` keeps a project running in the background — but
  not across a reboot: there is no systemd unit yet. *Done 2026-10-06:*
  `full-install` builds and installs it too (it built `mh` only, so the first
  `mh up` on a fresh host failed).
- *Done 2026-10-06:* a first install gives the socket the group `microhosted`
  and adds the installing user, as Docker's `docker` group; the installer
  ends with `newgrp microhosted` and `mh up` in an example. `SOCKET_GROUP=none`
  keeps it root-only; a reinstall never widens access.
- *Done 2026-10-06:* `setup-host` installs every host package in one apt run,
  debootstrap and ubuntu-keyring included (an Ubuntu-based example failed
  without them); `full-install` checks Go ≥ 1.21, which fetches the 1.25 the
  build needs; `make check` lists the host tools.
- `make prepare-image` ends with a template **and** an image (done 2026-09-28),
  but the catalog seed still lists templates that are never built on a fresh
  host (`base-ubuntu`, …), shown as `NOT BUILT`. List only what exists, or say
  in one line how to build each.
- One command from clone to a running example. Now two — `make full-install`,
  then `mh up` in `orchestrator/examples/hello`, which builds its image —
  plus a `newgrp` on the first install. Still open: prebuilt binaries (no Go
  on the host), an install that is not apt-only, and a first build without a
  second `sudo` prompt.
- Version bumps (Firecracker, kernel, Alpine) now stop at `no pinned SHA-256`
  (threat-model Layer 11). Right, but the steps to add a pin are in a file
  header: a `make pin` helper that fetches, verifies (signature where the
  publisher offers one) and prints the line to review.

**Usage**

- **Templates vs images** is the main confusion: two stores, near-identical
  names (`alpine-py` / `alpine-py:1.0`), and `mh images` is an alias for
  `mh template ls`. Documented (`docs/engine.md`, "Template or image?"); still
  to decide: rename or drop the alias, and whether templates stay user-facing.
- **Examples that run on a fresh clone** — see "a project spec that works on
  another host" (Phase 2): today they fail with a bare `engine 409`.
- **Errors that name the fix.** Engine refusals reach the user raw (`engine
  409: image conflict: …`). The orchestrator and `mh` should translate the
  common ones: image missing or mismatched → how to build/load it and where to
  paste `mh image ls -q`; name rules → the rule and a valid example.
- **Subnet collisions with the host.** The orchestrator checks a spec's
  subnets against each other and the engine's networks, not the host's routes:
  `website.yaml`'s `172.30.30.0/24` sits inside a Docker network
  (`172.30.0.0/16`) on a typical developer host, silently. `plan` should warn
  about any overlap with a host route.
- **Reaching a service from the LAN.** Ingress rules only exist on a managed
  interface, which the engine then owns entirely (deny-by-default). On a PC
  whose LAN interface is not managed, the only way today is a reverse proxy on
  the host (e.g. nginx with host networking to the VM's address). Document that
  path next to the managed-interface one, with the trade-off: the proxy is a
  listener on the host, which ingress rules deliberately avoid.
- **Listing what a spec needs.** `mh image ls` shows a short digest; the hint
  towards `-q` and `--no-trunc` exist (2026-09-28). Same review for every
  command a spec author uses: each should print, or point to, the exact value
  the YAML takes.
- **Transient states read as errors.** An import racing a build still being
  written failed with `lstat …: no such file or directory`; an operation during
  a daemon restart fails with a socket error. Say "not ready yet" and, where
  safe, wait.

Exit criterion: a person new to the project goes from `git clone` to the
website example served on their LAN following `quick-setup.md` only, without
`sudo` after install, without editing a YAML, and every error they hit on the
way names its fix.

## Deliberately not yet

Multi-host and a scheduler (Stage 9) until a single host is solid under failure.
A GUI. And above all, no abstracting the engine before Phase 4: the boundary is
validated by writing the second consumer, not by imagining it.

## Why this order

Phase 1 precedes Phase 2 because an orchestrator written first fuses with the
engine and then there is no second one. Phase 3 follows Phase 2 closely because
a guarantee that cannot be demonstrated does not sell in OT. Phase 0 precedes
everything because those are not risks, they are open doors we have already
walked through.

Phase 0b follows it because an orchestrator's reliability is bounded by the
engine's behaviour when something breaks: building one on an engine that leaks
on failure would hide the leak behind a controller that keeps retrying.
