# OT orchestrator — design

> Status: **design, nothing implemented.** Started 2026-09-24; final design
> review 2026-09-25 (see "Review log" at the end). Every decision is marked
> **Decided** (agreed), **Proposed** (recommended, awaiting agreement) or
> **Open**.
> Vocabulary follows `docs/roadmap.md`: the **engine** is MicroHosted (VMs,
> network, storage, host-initiated vsock, events); the **OT orchestrator** is its
> first consumer and sits strictly above it. This document is the control half
> of roadmap Phase 2; the data half (how readings leave the VMs) is
> `docs/ingestion.md`.

## Contents

1. Why an orchestrator
2. Three layers — build, deploy, operate
3. Process model
4. Images
5. Plant spec
6. Objects, names and ownership
7. Functions: lifetimes and the task contract
8. Health and failure
9. Scheduler and work queue
10. Redundancy
11. Bounded by construction
12. Reconciliation, restarts and authority
13. Updates
14. External triggers
15. Engine prerequisites
16. Milestones
17. Decisions and open questions
18. Review log

## 1. Why an orchestrator

A configuration-management tool (Ansible, Salt, …) runs when an operator runs
it, converges the host once and exits. That is the right shape for **installing
and provisioning a gateway**, and it stays the recommended way to do that across
a fleet: it installs the engine, ships images and places the plant spec on each
gateway. A thin module wrapping `mh apply` is a natural later addition.

It is the wrong shape for what the plant needs at runtime:

| Requirement | One-shot convergence | Orchestrator |
|---|---|---|
| A function is never left empty: replace on death, unhealthy or suspected compromise | no — nothing runs between runs | event loop over `GET /v1/events` |
| Lifetimes: boot → read → destroy every 30 s | no | scheduler |
| Back-off, *degraded* after N failures | no | per-function state machine |
| Keeps working with the uplink cut | no — the controller is outside the plant | runs on the gateway |

The orchestrator holds a desired state and reconciles toward it. It is **not** a
workflow engine (no task graphs) and **not** a scripting language (no steps).

## 2. Three layers — build, deploy, operate

**Decided (2026-09-24).**

| Layer | Artefact | Tool | Nature |
|---|---|---|---|
| Build | image spec → image (rootfs + kernel, content-addressed) | `mh build` | reproducible, runs **outside the plant** |
| Deploy | plant spec | orchestrator | declarative, reconcilable |
| Operate | — | `mh cp`, `mh exec`, `mh events`, `mh quarantine` | imperative, for humans |

Imperative operations never appear in the plant spec: a spec that says "copy
this, then run that" cannot be diffed, cannot be resumed after a crash and is
not idempotent. `mh cp` and `mh exec` remain the diagnostic tools.

## 3. Process model

**Proposed.**

- A separate binary, `cmd/mh-orchestrator`, run as its own systemd unit. It is a
  **client of the engine API** over the Unix socket and imports nothing from
  `internal/`. That satisfies roadmap Phase 1's exit criterion ("the OT
  orchestrator compiles without importing `internal/vm`") by construction, and a
  crash of one process never takes the other down.
- The desired state is one file on the gateway (default
  `/etc/microhosted/plant.yaml`). The orchestrator is the **only writer** to the
  engine for the objects it owns.
- `mh apply -f plant.yaml` validates the file, prints the plan and, when
  confirmed, installs it atomically as the desired state (write + rename) and
  signals the orchestrator. It never talks to the engine directly once the
  orchestrator exists; in milestone O1, before the loop exists, the same code
  runs one reconcile pass in-process. A file lock prevents two concurrent
  applies.
- `mh orch status | plan | reset FUNCTION` for operators (names not final).
- **Privilege.** The engine socket is root-equivalent (`/exec` is a root shell
  in every guest), so the orchestrator is as trusted as root. It runs on the
  gateway only, has no listening port in v1, and its only inputs are the plant
  spec and the engine's events.

## 4. Images

**Decided (2026-09-24):** images are built **outside the plant**; the plant spec
references them as `name:version@sha256:<digest>` and the digest is
**mandatory**; image spec and plant spec are **two separate files**. A
replacement must boot from exactly the image that was validated — never a newer
build, never a post-incident snapshot.

**Decided (2026-09-25) — the engine enforces the digest, not the orchestrator.**
A template is a pair of file paths, and a golden can be overwritten in place
under the same path (it has happened on the test hardware). A digest checked by
the orchestrator and then booted by path is a time-of-check/time-of-use gap.
Hence an engine-side image store (E2, §15; API in `docs/api.md` § Images):

- `mh image import parser-modbus:1.2 --kernel K --rootfs R` copies both files
  into the store, hashes the store's copies once, keeps them 0444 in a
  root-only directory, and binds the tag to the digest. Tags never move.
- The image digest covers the two files' digests and the VM defaults. A create
  names the image by tag, digest or both (`name:version@sha256:…`, refused if
  they disagree); a replacement reuses the old VM's digest.
- Hashing happens once at import (seconds for a 128–512 MB image), never per
  boot: a create by image is a map lookup, the same cost as a template.
- Imports read only files under the daemon's store directory, so the import
  endpoint cannot be used to read arbitrary host files.

Image spec (sketch, field names not final):

```yaml
# parser-modbus.image.yaml  →  mh build  →  parser-modbus:1.2@sha256:…
base: alpine:3.22@sha256:…
packages: [python3, py3-pymodbus]
files:
  /opt/parser/: ./src/
task: /opt/parser/read-once        # one-shot entry point (transaction mode)
service: /opt/parser/serve         # long-running entry point (persistent/window)
health: /opt/parser/healthcheck    # optional; exit 0 = healthy
vcpus: 1
mem_mb: 64
```

The image declares **what** it can do (`task`, `service`, `health`); the plant
spec only declares **when** and **where**. `mh build` formalises what
`scripts/build-rootfs-alpine.sh --add` does today.

## 5. Plant spec

**Decided (2026-09-19):** YAML, parsed in **strict mode** (unknown field →
error) and validated in full before anything is applied. A mistyped policy line
must never degrade silently into "no rule".

**Proposed — network blocks reuse the engine's types verbatim**
(`pkg/types.CreateNetworkRequest`: `allowed_egress[].ip/protocol/port`,
`allowed_ingress[].iface/src_ip/protocol/port/to_ip`, `egress_iface`, `intra`).
No second vocabulary to translate, and no translation bug between the spec and
the ruleset.

```yaml
version: 1
budget:
  max_vms: 120
  max_mem_mb: 6000
  workers: 4
networks:
  ts-net:
    subnet: 172.16.10.0/24
    allowed_ingress:
      - { iface: wlan0, src_ip: 192.168.50.52, protocol: tcp, port: 8080, to_ip: 172.16.10.2 }
    allowed_egress:
      - { ip: 192.168.50.52, protocol: tcp, port: 80 }
functions:
  ts-01:
    image: parser-modbus:1.2@sha256:ab12…
    network: ts-net
    ip: 172.16.10.2
    labels: { sensor: ts-01 }
    files:
      /etc/parser.conf: { from: conf/ts-01.conf }
    secrets:
      /etc/parser.key: { from: /etc/microhosted/secrets/ts-01.key }
    lifecycle: { mode: transaction, every: 30s, timeout: 5s }
    on_failure: { backoff: 5s..5m, degraded_after: 5, old: quarantine }
```

### Validation rules (all checked before any change)

- Every `image` has a digest and that digest exists in the engine's image store.
- Every `network` referenced exists in the spec; subnets do not overlap each
  other or existing engine networks.
- `ip` lies inside the function's subnet and is unique across functions.
- If a network has an ingress rule with `to_ip = X`, exactly one function on that
  network declares `ip: X` (the address belongs to the function; automatic
  allocation never hands it out).
- `timeout < every` for `transaction`; `duration < every` for `window`.
- File sources exist and are regular files; secret sources are owned by root and
  not group/world-readable.
- The static budget (§11) fits, and fits the host (§11).

### Per-function files and secrets

**Decided:** per-function configuration is declared under `files:`; code and
packages belong in the image.

**Injected offline into the VM's disk before first boot** — the engine side is
implemented (E4, `files` on create and replace; see `docs/api.md`). The VM is
born configured, a replacement is born identical, no agent is involved, and
there is no window in which a VM runs unconfigured. `secrets:` uses the same
path with mode `0400` and sources restricted to a root-only directory, and is
**never printed** in a plan or log. The engine records a file's SHA-256 so the
diff can tell whether a VM has the declared content, but not a secret's: the
orchestrator keeps the hash of the secrets it applied in its own state.

## 6. Objects, names and ownership

**Decided (2026-09-19):** the orchestrator only touches objects carrying its
ownership label and never an object it did not create.

**Proposed:**

- Reserved labels, set by the orchestrator and rejected in user `labels:`:
  - `managed-by=mh-orchestrator`
  - `function=<name>`
  - `generation=<n>` (increments on every new instance of the function)
  - `spec=<short digest of the function's resolved spec>` — how the reconciler
    knows an instance is out of date.
- VM names: `<function>-<generation>` (e.g. `ts-01-17`). Names are unique and
  immutable in the engine, so every instance gets a fresh one.
- Networks carry labels with the same rules as VMs (E1, §15): set at create,
  filtered with `?label=`, changed with a merge patch. New network names are
  DNS labels, so a function name maps onto a network name unchanged.
- Engine `autostart` is always `false` on owned VMs (§12). `replace` copies it
  from the old VM, so creating with `false` is enough.

## 7. Functions: lifetimes and the task contract

| `mode` | VM lifetime | Uses | Parameters |
|---|---|---|---|
| `transaction` | boot → run `task` once → destroy | image `task` | `every`, `timeout` |
| `window` | boot → run `service` for a bounded time → destroy | image `service` | `every`, `duration` |
| `persistent` | always on; recycled on a schedule and on failure | image `service` | `recycle` (e.g. `6h`) |

**Proposed — cold boot, not snapshot restore, in v1.** A snapshot freezes the
guest's IP and MAC, so a "restore per cycle" design needs one snapshot per
function, each carrying a full memory file (128 MB × 200 functions ≈ 25 GB of
disk) and rebuilt whenever the image or the files change. A measured cold
deploy is ~0.8 s, flat up to 164 devices — well inside a 30 s interval.
Per-function snapshots stay a later optimisation for sub-second cadences.

**Proposed — the task contract** (transaction mode):

1. Boot the VM; wait for the agent (bounded by `timeout`).
2. Run the image's `task` over the engine's exec channel.
3. Exit code `0` = success; stdout (bounded by the engine's response cap) = the
   result.
4. Destroy the VM. On timeout or non-zero exit the cycle is a **failure** (§8).
5. Results go to an append-only local journal, one JSON line per cycle
   (`function`, `generation`, `started`, `duration`, `exit`, `output`). This is
   the v1 sink; `docs/ingestion.md`'s data stream replaces the exec path later
   and the journal stays as the durable layer.

The whole cycle, boot included, must fit in `timeout`; the engine must be able
to abort an exec at that deadline (§15).

`persistent` recycling is a replace with `old: destroy` — hygiene, not
suspicion, so it does not consume quarantine.

## 8. Health and failure

**Decided (2026-09-23):** detection belongs to the orchestrator; every cause of
failure ends in the same action — a replacement from the pinned image takes the
function's place. If the replacement fails, the suspect is **never**
reconnected.

**Proposed — health is checked over vsock only.** The host cannot reach guest
networks by design, so there is no TCP/HTTP probe. Two levels:

- Liveness: the agent answers a trivial exec within the timeout.
- Readiness: the image's `health` command exits `0`, if the image declares one.

Failure sources and what they mean:

| Source | Applies to |
|---|---|
| `vm.died` event (incl. OOM) | all modes |
| health check fails `health.failures` times in a row (default 3) | `persistent`, `window` |
| task timeout or non-zero exit | `transaction` |
| `vm.autostart_failed`, create/replace error | all modes |

Per function, a state machine: `healthy → failing (back-off: 5 s, 10 s, … up to
5 m) → degraded` after `degraded_after` consecutive failures. A degraded
function gets no more attempts until `mh orch reset <function>` or a change to
that function in the spec. Entering and leaving *degraded* is logged and
surfaced by `mh orch status`; *where alerts go* is open (§17).

**Proposed — failure state is persisted.** The orchestrator owns no objects
state (§12), but back-off counters and *degraded* flags must survive its own
restarts: otherwise an orchestrator in a crash loop resets every back-off to
zero and becomes the flood it exists to prevent. A small state file
(`/var/lib/microhosted/orchestrator/state.json`, atomic write + rename) holds
only per-function failure counters and flags, counted in cycles rather than
wall-clock timestamps (§9). A missing or unreadable file starts every function
at the first back-off step, not at zero.

## 9. Scheduler and work queue

**Decided (2026-09-25): intervals only** — monotonic, never wall-clock. Boards
without a battery-backed RTC boot with a wrong clock until NTP converges; a cron
expression would then fire everything at once or freeze.

**Proposed:**

1. **Deterministic jitter.** Each function's phase within its interval is
   derived from a hash of its name, so 200 functions at `every: 30s` spread over
   the 30 s.
2. **Workers hold VMs, the queue holds intentions.** At most `budget.workers`
   cycles, replacements and recycles are in progress at once across all
   functions; everything else waits in the queue, which costs bytes, not VMs.
3. **Coalescing.** A function has at most one pending entry. A tick that arrives
   while one is pending, or while its cycle is still running, is merged and
   counted as *coalesced*. Hence no overlap, and the queue length is bounded by
   the number of functions.
4. **No catch-up.** After downtime one cycle runs and the cadence resumes.
5. **Freshness.** A pending entry older than `max_age` (default: one interval) is
   dropped and counted — a reading taken five minutes late is not the one that
   was asked for.
6. **Admission.** An engine 503 puts the entry back with back-off; it is not a
   function failure.
7. **FIFO** within the queue; failure handling (§8) takes precedence over
   scheduled cycles.
8. **In memory.** The queue is rebuilt from the spec after a restart.

With coalescing, a fixed rate can never pile up, so a separate "fixed delay"
(`after_completion`) mode is **not needed in v1**.

Start-up order: `depends_on: [function]` delays a function until the named ones
are healthy. Completion-triggered work ("run B when A finishes") is out of scope.

## 10. Redundancy

Container orchestrators replicate identical pods behind a load balancer across
nodes. Little of that transfers: one gateway is one failure domain, many Modbus
devices accept only a handful of connections, the function's address reaches
exactly one VM, and every copy costs RAM.

| `redundancy` | Meaning | Cost | Status |
|---|---|---|---|
| `single` (default) | one VM; replace on failure | 1×, ~1 s gap | v1 |
| `standby: N` | one active + N booted spares without the function's IP; failover is quarantine + claim | (1+N)× | O5 |
| `voting: 3` | three parsers process the same raw reading, outputs compared 2-of-3; divergence is a compromise signal | 3× | needs the data stream; later |

## 11. Bounded by construction

The central property: **the orchestrator can never flood the host with VMs**,
and this is proven when the spec is applied, not observed afterwards.

### Static budget

```
peak_vms = Σ persistent/window functions × (1 + standby)
         + workers                        # transaction cycles, replacements, recycles
         + Σ keep_quarantined             # suspects kept alive for investigation
         + keep_quarantined_orphans       # suspects of functions removed from the spec
```

`peak_mem` is the same sum weighted by each image's `mem_mb` plus a fixed VMM
overhead per VM. The configured `mem_mb`, not the measured working set, is used:
a guest may touch all of it.

`mh apply` rejects the spec if `peak_vms > budget.max_vms`, if
`peak_mem > budget.max_mem_mb`, or if the budget exceeds what the host can admit
(`MemTotal − --mem-reserve-mb`, `--max-vms`, read from the engine). A spec that
is accepted is a spec whose worst case fits the host — which is also why v1
needs no priority classes: nothing is ever left waiting for RAM.

### Quarantine retention

A quarantined VM keeps its RAM. **Proposed:** `keep_quarantined` per function,
default 1. When a new suspect exceeds it, the oldest is powered off with its
disk kept for forensics (`old: stop` semantics) and then destroyed once a
disk-retention limit is reached. Suspects are never deleted silently: every
removal is logged.

### Runtime brakes, outermost first

1. Engine admission: `--mem-reserve-mb`, `--max-vms`, `--max-parallel-boots` (503).
2. Per-orchestrator quota counted by `managed-by` (`--quota`, 429). `mh apply`
   also checks the static budget against this quota, read from the engine.
3. The static budget above.
4. The engine refuses a second VM serving the same network + address (409).
5. Workers + coalescing (§9).
6. Per-function back-off and *degraded* (§8), persisted across restarts.

## 12. Reconciliation, restarts and authority

- **No object database.** The engine is the source of truth for what exists.
  On start, the orchestrator reads the spec, lists owned VMs and networks by
  label, reads its failure state, subscribes to events and reconciles.
- **Plan.** Each difference is one of: create, update in place (network rules,
  labels), recreate (image digest, files, memory), destroy. Changing a network's
  subnet recreates the network and therefore every VM on it; the plan marks such
  changes **destructive** and `mh apply` refuses them without
  `--allow-destructive`.
- **Host reboot.** Owned VMs have `autostart=false`; the engine finds them dead
  and the orchestrator destroys the dead records and creates fresh instances in
  `depends_on` order. One authority per object.
- **Removed functions.** Serving instances are destroyed; quarantined ones are
  kept (evidence does not disappear because configuration changed) and counted
  in `keep_quarantined_orphans`.
- **Objects it does not own** are never modified, even if they collide by name;
  a collision is a validation error.

## 13. Updates

A changed image digest (or files, or memory) recreates the affected functions
**one at a time**: new instance, wait for a healthy result (or one successful
transaction), continue. At the first failure the roll-out **halts and alerts**;
already-updated functions stay on the new digest and the rest on the old one.
**Proposed:** no automatic rollback — in a plant, reverting is itself a change
an operator decides.

## 14. External triggers

**Not in v1.** Every inbound channel is attack surface on an OT gateway.
Whatever the transport:

1. A trigger names a **declared function** and nothing else — no image, command,
   file or rule. Arbitrary job submission is out of scope by design.
2. It enters the **same queue** (coalescing, budget, admission apply).
3. It carries an idempotency key and is rate-limited per source and function.

Transports in order of preference: local Unix socket (`mh trigger ts-01`);
**outbound** MQTT subscription to an upstream broker (no listening port on the
gateway); inbound HTTP webhook as a last resort (TLS, authentication, a port).

## 15. Engine prerequisites

Found in the review; each blocks the milestone shown.

| # | Engine change | Why | Blocks |
|---|---|---|---|
| E1 | Labels on networks (create, list/filter, patch) — **done**, validated on x86_64 hardware (2026-09-27, `scripts/netlabels-test.sh` 17/17 as root) | ownership rule and pruning of networks | O1 |
| E2 | Content-addressed, read-only image store; create by digest — **done**, validated on x86_64 hardware (2026-09-27, `scripts/image-test.sh` 17/17; the run caught and fixed a create-by-image refusal in the API) | a digest checked by the orchestrator and booted by path is TOCTOU; goldens are mutable today | O1 |
| E3 | Per-request exec timeout (`timeout_ms`), abort at the deadline — **done**: 1 ms–10 min, 504 at the deadline, partial output discarded, abandoned if the client disconnects | transaction `timeout` and health timeouts are shorter than the fixed 30 s | O2 |
| E4 | Offline file injection into a VM's disk at create — **done**: `files` on create and replace (content in the request, ≤ 64 files / 512 KiB, written with `debugfs` as the VM's identity and read back; record keeps SHA-256 except for secrets; replace of a VM with files requires them again) | per-function files and secrets | O4 (O1 may ship without `files:`) |
| E5 | Per-orchestrator quota by `managed-by` — **done**: `--quota CONSUMER=vms:N,mem:MB` and `--quota-default` on the daemon (operator-owned, not settable through the API), 429 over it, usage in `GET /v1/system`; `managed-by` fixed at create | roadmap Phase 1 item 3 | O3 |
| E6 | Readiness published by the engine — **done** (2026-09-27): after every boot the daemon probes the guest agent (host-dialed `CONNECT`, no command) and publishes `vm.ready` / `vm.agent_unready`, `agent_ready_at` on the VM, and `GET /v1/vms/{id}/ready` (blocking); exec and file transfers wait for it within their deadline | a function's first exec (health, task) right after a run or replace failed its handshake for ~100 ms and would read as "down"; each client would otherwise poll | O1 |

Pending engine validations, carried over: daemon restart with a quarantined VM,
and `nft -c` of the generated ruleset as root.

## 16. Milestones

Each slice is usable on its own and validated on test hardware before the next.

| # | Slice | Exit criterion |
|---|---|---|
| O0 | Engine prerequisites E1, E2 | networks carry labels; a VM boots only from a digest in the store |
| O1 | `mh apply` + one reconcile pass: strict parse, validation, budget, plan, apply networks and `persistent` functions, prune owned objects | apply twice → empty plan; remove a function → fully cleaned; over-budget or destructive spec refused before any change |
| O2 | `mh-orchestrator` loop: events, health, replace, back-off, *degraded*, persisted failure state (E3) | kill a VM, kill the orchestrator, reboot the host: every function back, zero orphans, back-off preserved |
| O3 | Scheduler + work queue: `transaction`, `window`, jitter, coalescing, `max_age`, workers, journal (E5) | 200 functions at 30 s for 72 h unattended: flat boot rate, peak VMs ≤ computed peak — roadmap Phase 2 exit |
| O4 | `mh build` + image spec + `files`/`secrets` (E4) | same spec → same digest; a replacement boots configured |
| O5 | `standby`, quarantine retention limits, rolling updates | failover without a gap; a failing function never exceeds its peak |
| O6 | Triggers: local socket, then outbound MQTT | a flood of triggers changes neither the peak nor the budget |

## 17. Decisions and open questions

Decided: YAML + strict parsing; ownership by label; three layers; build outside
the plant; digest mandatory; two files; intervals only.

Proposed in the review, awaiting agreement:

1. Separate `mh-orchestrator` binary, engine API client (§3).
2. Engine-enforced digests via an image store (§4, E2) — **decided 2026-09-25**, implemented.
3. Image declares `task` / `service` / `health`; plant spec declares when/where (§4).
4. Network blocks reuse the engine's field names (§5).
5. Cold boot in v1, snapshots later (§7).
6. Task contract + local JSON-lines journal as the v1 sink (§7).
7. Health over vsock only (§8).
8. Persisted failure state (§8).
9. No `after_completion` in v1 (§9).
10. Budget formula with `workers`; no priority classes in v1 (§11).
11. Quarantined VMs survive the removal of their function (§12).
12. Roll-outs halt and alert; no automatic rollback (§13).

Open:

1. Where *degraded* and other alerts are delivered (log only, an orchestrator
   event stream, upstream MQTT).
2. `keep_quarantined` default and the forensic disk-retention limit.
3. `health.failures` and `max_age` defaults.

## 18. Review log

**2026-09-25 — final design review before code.** Checked against the engine
code. Changes from the first draft:

- The draft's network sketch used invented field names; aligned with
  `pkg/types` (§5).
- The budget summed a replace slot per function while the queue said workers
  bound transient VMs; unified into one formula (§11).
- "Stateless orchestrator" contradicted "degraded until an operator acts": a
  restart would reset back-off. Failure state is now persisted (§8).
- "Restore per cycle" does not fit snapshots that freeze IP/MAC per VM; v1 uses
  cold boot (§7).
- The task contract, the result sink and the health mechanism were undefined
  (§7, §8).
- Networks have no labels, goldens are mutable files addressed by path, and exec
  has a fixed 30 s timeout: listed as engine prerequisites (§15).
- `after_completion` and priority classes dropped from v1: coalescing and the
  host-checked budget make them unnecessary (§9, §11).
- Destructive changes, name collisions, prune of quarantined VMs and the single
  writer to the engine made explicit (§3, §12).
