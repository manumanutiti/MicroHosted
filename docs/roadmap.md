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
declarative registry) — is designed in `docs/ingestion.md`.

Exit criterion: N sensors unattended for 72 h on the target hardware, zero orphans (VM,
network, tap, cgroup, chroot), and the loop survives killing the daemon
mid-cycle — `Reconcile` exists, it has never been exercised under an
orchestrator.

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

This also absorbs the hardening left open in `SESSIONS.md`: validating seccomp
and the actual *values* under `/sys/fs/cgroup/microhosted/<id>`.

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
(cgroup), but disk and network throughput are not: one compromised VM can
saturate the storage device or its bridge for all the others. Firecracker has
rate limiters for both (per drive, per interface); they are not wired yet. Two
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
