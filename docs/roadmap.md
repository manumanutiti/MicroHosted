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
| ARM64 / Raspberry Pi 5 | the daemon runs on the Pi; this was the existential risk |
| Fine-grained egress (`allowed_egress`) | `internal/network/nftables.go`, `ValidateEgressRules` + tests — listed as gap #1, it is done |
| Ultra-minimal image | `scripts/build-rootfs-alpine.sh`, template `alpine-py`: 42 MB PSS vs 123 MB on Ubuntu Noble |
| Density on target hardware | 174 devices (network + egress + VM each) on the 8 GB Pi, **flat** deploy latency 1783→1856 ms, 34 MB RAM/device, ~0 disk per clone |
| Network-per-VM topology | chosen deliberately; per-source-IP egress on a shared network was rejected (would need saddr + per-tap anti-spoofing) |
| Guest cannot open a channel to the host | `internal/vsock` always dials; there is no `net.Listen` in the daemon |
| Reset to clean + forensics | restore 109 ms, fork 113 ms, `Fork(quarantine=true)` gives a bridge-less TAP |

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
   trusted side.

Exit criterion: no known path from a compromised guest to the host, or to any
network the operator did not declare.

## Phase 1 — The engine/orchestrator line

Today the only consumer of `vm.Manager` is the HTTP API, and its surface is
CRUD-shaped (`Create`/`Destroy`/`Fork`/`Snapshot`/`Exec`). Three things are
missing before an orchestrator can be written without reaching into the engine:

1. **Lease over a pool.** *"Give me a clean instance of template T, I hold it for
   N seconds, then it is destroyed or quarantined."* That sentence is identical
   for the OT pull loop and for an AI sandbox, which is why it is the primitive
   worth extracting first. It builds on `Restore`/`Fork`, both already validated.
2. **Typed events.** The VM died, it hit its cgroup ceiling, the vsock channel
   timed out, `nft -f` failed. Today these are journal lines. An orchestrator
   needs to subscribe, not poll.
3. **Admission control.** A global VM cap and a per-orchestrator quota. There is
   none, so any consumer can take the host down — and in push mode that is
   exactly the DoS `docs/iot-edge.md` already anticipates.

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

Exit criterion: N sensors unattended for 72 h on the Pi, zero orphans (VM,
network, tap, cgroup, chroot), and the loop survives killing the daemon
mid-cycle — `Reconcile` exists, it has never been exercised under an
orchestrator.

## Phase 3 — Demonstrability

In OT the guarantee *is* the product, and right now it is arguable but not
demonstrable. Two deliverables:

- **A written threat model**: what is contained, what is not, and explicitly
  that the access layer is not ours. Two sensors on the same Wi-Fi BSS reach
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
