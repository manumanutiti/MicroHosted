# Orchestrator — design

> Status: **implemented in part** — what runs today, and how to use it, is in
> [`orchestrator/README.md`](../orchestrator/README.md) and
> [`docs/cli.md`](cli.md#projects-mh-up-mh-down); §16 tracks the milestones.
> This document is the design record: started 2026-09-24, final review
> 2026-09-25, second round of decisions 2026-09-28 (see "Review log" at the
> end). Every decision is marked **Decided** (agreed), **Proposed**
> (recommended, awaiting agreement) or **Open**.
>
> **Scope.** It was designed as the orchestrator of an IoT/OT isolation
> gateway, and much of the text keeps that vocabulary and its examples
> (sensors, readings, a plant). The orchestrator that came out of it is
> general: Docker Compose for microVMs — `mh up`, `mh down`, a project per
> directory. Read the terms accordingly:
>
> | Here | In the tool |
> |---|---|
> | `plant.yaml`, one file per host (§3) | `microse.yml`, one per project |
> | the plant | the project, or the host's workloads |
> | the gateway | the host running the engine |
> | `mh orch …` | `mh up`, `mh status`, `mh failures`, … (`mh-orchestrator VERB`) |
>
> Vocabulary follows `docs/roadmap.md`: the **engine** is MicroHosted (VMs,
> network, storage, host-initiated vsock, events); the orchestrator is its first
> consumer and sits strictly above it. This document is the control half of
> roadmap Phase 2; the data half for the edge use case (how readings leave the
> VMs) is `docs/ingestion.md`.

## Contents

1. Why an orchestrator
2. Three layers — build, deploy, operate
3. Process model
4. Images
5. Project spec (`microse.yml`)
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
and provisioning a host**, and it stays the recommended way to do that across
a fleet: it installs the engine, ships images and places the project specs on
each host. A thin module wrapping `mh apply` is a natural later addition.

It is the wrong shape for what the workloads need at runtime (the table was
written for an edge gateway; the first three rows hold for any project):

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
| Deploy | project spec | orchestrator | declarative, reconcilable |
| Operate | — | `mh cp`, `mh exec`, `mh events`, `mh quarantine` | imperative, for humans |

Imperative operations never appear in the project spec: a spec that says "copy
this, then run that" cannot be diffed, cannot be resumed after a crash and is
not idempotent. `mh cp` and `mh exec` remain the diagnostic tools.

## 3. Process model

**Decided (2026-09-28).** *As built:* `mh-orchestrator` runs per project — in
the foreground (`mh up`) or in the background (`mh up -d`) — on the
`microse.yml` of the current directory, not as one systemd unit over
`/etc/microhosted/plant.yaml`; `mh apply` hands changes to a running `up`.
Locks and state are per project (`orchestrator/README.md`). The rest of this
section is the original decision.

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
  gateway only, has no listening port in v1, and its only inputs are the project
  spec and the engine's events.

## 4. Images

**Decided (2026-09-24):** images are built **outside the plant**; the project spec
references them as `name:version@sha256:<digest>` and the digest is
**mandatory**; image spec and project spec are **two separate files**. A
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

**Implemented (2026-09-28) — the image spec and `mh build`.** Format and
behaviour in `docs/cli.md` § Building an image; the essentials:

```yaml
# parser-modbus/build.yml  →  mh build parser-modbus/  →  parser-modbus:sha-3f2a9c1e7b04@sha256:…
base: alpine:3.22                  # pinned minirootfs
packages: [python3, py3-pymodbus]
files:
  /opt/parser/: src/               # a directory's contents (Docker's COPY)
run: ["adduser -D parser"]         # Docker's RUN, in a chroot of the image
command: /opt/parser/read-once     # optional default command
health: { command: /opt/parser/healthcheck, every: 30s }   # optional default health check
vcpus: 1
mem_mb: 64
```

Bases: `alpine:3.22` (minirootfs, apk, busybox init) and `ubuntu:24.04`
(debootstrap, apt, systemd). It follows Docker where Docker has an answer: `-t`, `-f`, a build context that
file sources cannot leave, `-q` printing only the reference, `COPY`/`RUN`
semantics. It runs on the engine's host as root through `sudo` (the rootfs is
written into the daemon's store and imported from there), with the base and
kernel pinned by SHA-256. The build is **not bit-reproducible** yet (ext4
timestamps and UUIDs; unpinned package versions): the O4 criterion "same spec →
same digest" is still open — see `docs/roadmap.md`, reproducible builds.

**How a project spec gets the reference: compose's interpolation (2026-09-28).**
`${NAME}` in a project spec's values comes from the environment, else from the
`.env` file next to the spec (`${NAME:-default}`, `${NAME:?message}`; a bare
`$NAME` is left alone, so guest shell commands need no escaping). The digest
stays mandatory — it is checked after interpolation — but it no longer has to
be pasted:

```bash
echo "PARSER=$(mh build -q -t parser-modbus parser-modbus/)" >> .env   # image: ${PARSER}
mh-orchestrator apply -f plant.yaml
```

A running orchestrator reloads the file with its own environment, so `apply`
refuses to hand it a spec whose variables only the caller's shell holds:
they belong in `.env`.

**Or the project spec builds it (2026-09-28): `build:`, compose's `build:`.** A
function names a `build.yml` (or its directory) instead of an image;
`plan`/`apply`/`run` run `mh build` on it and pin the reference printed. The
build is cached by fingerprint — `mh build` versions an image
`name:sha-<fingerprint of the spec and its files>` and reuses a tag the store
holds only while it still names the digest this user's build imported under
it (a per-user record; a rebound tag is refused) — so an unchanged context builds nothing and a changed one is a new
digest, rolled out as any change. Pinning still holds: what boots is the
digest `mh build` printed. The orchestrator stays an API client: it runs the
`mh` binary. File names follow compose: `mh build` reads `./build.yml`,
`mh-orchestrator` reads `./microse.yml` without `-f`.

**Projects and `mh up` (2026-09-28), compose's model.** The desired state is no
longer one file per host: each project spec is a project (its `name:`, else its
directory), everything created carries `project=<name>`, and a spec sees and
prunes only its own project — several run side by side, each with its own
lock, state and background `run`. VMs are `<project>-<function>-<n>`; network
names stay host-wide (a clash is refused, naming the owning project). Objects
from before projects are adopted by the spec that declares them (labels
patched, nothing recreated). `mh up [-d]`, `down`, `plan`, `apply`, `status`,
`failures`, `validate` hand the verb to `mh-orchestrator` (`up` is `run`;
`up -d` applies, then leaves a `run` in the background; `down` stops it first),
which stays a separate program (§3).

**Decided (2026-09-28) — the command is configuration, like Docker's `CMD` and
compose's `command:`.** An image *may* declare a default `command` and a default
`health`; the project spec may set or override either per function. There are no
named entry points: an operator must be able to use an image without knowing
anything about it. Allowing a free command in the project spec does not weaken the
host:

- The project spec is written by the operator, who already holds root-equivalent
  access (the engine socket; `/exec` is a root shell in every guest).
- The guest is untrusted by design. What protects the host and the network is
  the Firecracker boundary (jailer, seccomp, per-VM uid) and the nftables rules,
  none of which depend on what runs inside.
- The digest still pins the software exactly; the command is versioned with the
  project spec.

A command appears in plans and logs, so it must never carry a secret; secrets go
through `secrets:` (§5).

The image defaults live in the image manifest, covered by the digest (E8, §15).
A function that sets no `command` takes the image's; a persistent function
without `health` takes the image's check (a check's missing `every`, `timeout`
or `failures` get the project spec's defaults). `plan` says which functions run
what the image declared. A cycle function with no command in either is refused
at plan time.

## 5. Project spec (`microse.yml`)

**Decided (2026-09-19):** YAML, parsed in **strict mode** (unknown field →
error) and validated in full before anything is applied. A mistyped policy line
must never degrade silently into "no rule".

**Decided (2026-09-28) — network blocks reuse the engine's types verbatim**
(`pkg/types.CreateNetworkRequest`: `allowed_egress[].ip/protocol/port`,
`allowed_ingress[].iface/src_ip/protocol/port/to_ip`, `egress_iface`,
`egress_private`, `intra`).
No second vocabulary to translate, and no translation bug between the spec and
the ruleset.

```yaml
version: 1

budget:   { max_vms: 120, max_mem_mb: 6000, workers: 4 }
limits:   { max_quarantined: 5, quarantine_disk_mb: 4096 }
journal:  { max_mb: 256, max_age: 7d }

defaults:
  on_failure: { backoff: 5s..5m, degraded_after: 5, old: destroy }
  keep_quarantined: 1

networks:
  backend:
    subnet: 172.16.10.0/24
    allowed_egress:
      - { ip: 10.0.0.5, protocol: tcp, port: 5432 }
  ingest:
    subnet: 172.16.20.0/24
    allowed_ingress:
      - { iface: wlan0, src_ip: 192.168.50.52, protocol: tcp, port: 1883, to_ip: 172.16.20.2 }

functions:
  nightly-report:                      # disposable: one VM per run
    image: tools:1.0@sha256:…
    network: backend
    command: /opt/report/run --since 24h
    lifecycle: { mode: transaction, every: 24h, timeout: 10m }

  web-up:                              # periodic check
    image: netcheck:1.0@sha256:…
    network: backend
    command: curl -fsS http://10.0.0.5:8080/health
    lifecycle: { mode: transaction, every: 1m, timeout: 10s }

  sampler:                             # runs for a bounded time, then goes
    image: sampler:0.4@sha256:…
    network: backend
    command: /opt/sampler/run --hz 1000
    lifecycle: { mode: window, every: 15m, duration: 60s }

  broker:                              # static: always on, never recycled
    image: mosquitto:2.0@sha256:…
    network: ingest
    ip: 172.16.20.2                    # the ingress rule's to_ip
    health: { command: "pgrep mosquitto", every: 10s, timeout: 2s, failures: 3 }
    lifecycle: { mode: persistent, recycle: never }

  api:
    image: api:3.2@sha256:…
    network: backend
    command: /opt/api/serve
    health: { command: "wget -qO- localhost:8080/health", every: 10s, timeout: 2s, failures: 3 }
    lifecycle: { mode: persistent, recycle: 6h }
    resources: { vcpus: 1, mem_mb: 256, disk_mib_s: 20, net_mbit: 10 }
    labels: { tier: api }
    files:
      /etc/api.conf: { from: conf/api.conf }
    secrets:
      /etc/api.token: { from: /etc/microhosted/secrets/api.token }
    on_failure: { old: quarantine }    # this one is worth investigating
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
- Every `transaction` and `window` function has a command: its own or the
  image's default.
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

**A secret minted per VM (2026-09-29): `command:` instead of `from:`.** A
secret may name a shell command, run on the orchestrator's host in the spec's
directory each time one of the function's VMs is created (a replacement and
every cycle's VM included), with `MH_PROJECT`, `MH_FUNCTION` and `MH_VM_NAME`
in its environment. Its standard output (at most 512 KiB, not empty) is the
secret; a failure fails that create — a start failure like any other, with
back-off — and reports the command's last line of stderr, so a command must
not print the secret there. It runs with the orchestrator's user and
environment, as code the operator wrote: project specs are trusted like image
specs. Because every VM gets different content, the spec hash covers the
command, not its output: a fresh credential is not a spec change. The use is a
credential that must not outlive the VM — a GitHub runner's just-in-time
config (`orchestrator/examples/github-runner`): the long-lived token that
mints it stays on the host, and the VM holds only what is worthless after it.

```yaml
    secrets:
      /var/lib/mh-runner/jit:
        command: ./jit-config.py '${GITHUB_SCOPE}'
```

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

**Decided (2026-09-28).** A VM is never reused or restarted: whenever one is
needed, a fresh one is created from the pinned digest. The modes differ only in
how long each VM lives.

| `mode` | VM lifetime | Runs | Parameters |
|---|---|---|---|
| `transaction` | boot → run `command` once → destroy; the next cycle gets a new VM | `command` over the exec channel | `every`, `timeout` |
| `window` | boot → run `command` for a bounded time → destroy | `command` | `every`, `duration` |
| `persistent` | always on; a fresh VM takes its place on schedule and on failure | the image's init, or `command` if set (the VM is kept while it runs) | `recycle` (`6h`, … or `never`) |

`persistent` with `recycle: never` is the static case: replaced only on failure.

**`on_exit: replace` (persistent, 2026-09-29): a VM that does one piece of
work.** By default a persistent command should never end, and its exit is a
failure (recorded, with back-off). With `on_exit: replace` an exit **0** is the
work done: the orchestrator checks the command on every supervision pass (2 s,
not the health schedule), destroys the VM and starts a fresh one at once, with
nothing recorded; any other code is still a failure. The command's exit code is
written in the guest (`/run/mh-function.exit`) by the shell that launches it,
also when the command execs. Made for a CI runner that takes one job and exits
(`orchestrator/examples/github-runner`). Opt-in because it has no back-off on
exit 0: a server that exits 0 by mistake would be replaced in a loop.

**Proposed — cold boot, not snapshot restore, in v1.** A snapshot freezes the
guest's IP and MAC, so a "restore per cycle" design needs one snapshot per
function, each carrying a full memory file (128 MB × 200 functions ≈ 25 GB of
disk) and rebuilt whenever the image or the files change. A measured cold
deploy is ~0.8 s, flat up to 164 devices — well inside a 30 s interval.
Per-function snapshots stay a later optimisation for sub-second cadences.

**Proposed — the task contract** (transaction mode):

1. Boot the VM; wait for the agent (bounded by `timeout`).
2. Run the function's `command` over the engine's exec channel.
3. Exit code `0` = success; stdout (bounded by the engine's response cap) = the
   result.
4. Destroy the VM. On timeout or non-zero exit the cycle is a **failure** (§8).
5. Results go to an append-only local journal, one JSON line per cycle
   (`function`, `generation`, `started`, `duration`, `exit`, `output`), bounded
   by `journal.max_mb` and `journal.max_age`. This is the v1 sink.

**Decided (2026-09-28) — the vsock data stream (`docs/ingestion.md`) is a
reference for later, not part of this design.** v1 moves results only as a
command's output.

The whole cycle, boot included, must fit in `timeout`; the engine must be able
to abort an exec at that deadline (§15).

`persistent` recycling is a replace with `old: destroy` — hygiene, not
suspicion, so it does not consume quarantine.

## 8. Health and failure

**Decided (2026-09-23):** detection belongs to the orchestrator; every cause of
failure ends in the same action — a replacement from the pinned image takes the
function's place. If the replacement fails, the suspect is **never**
reconnected.

**Decided (2026-09-28) — failure is a process signal, never a judgement on
data.** The orchestrator is generic: it does not know what a function's output
means, and the project spec holds no data schemas. A function that wants to fail
on bad data exits non-zero.

**Proposed — health is checked over vsock only.** The host cannot reach guest
networks by design, so there is no TCP/HTTP probe. Two levels:

- Liveness: the agent answers a trivial exec within the timeout.
- Readiness: the function's `health` command (its own or the image's default)
  exits `0`, if there is one.

Failure sources and what they mean:

| Source | Applies to |
|---|---|
| `vm.died` event (incl. OOM) | all modes |
| health check fails `health.failures` times in a row (default 3) | `persistent`, `window` |
| `command` exits non-zero or passes `timeout` | `transaction` |
| `command` ends (with any code) before `duration` | `window`; `persistent` with a `command` |
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
only, per function, failure counters and flags (counted in cycles rather than
wall-clock timestamps, §9), the last failure records (below) and, for updates,
the last spec that ran successfully and the *held* flag (§13). A missing or
unreadable file starts every function at the first back-off step, not at zero;
a held function whose flag was lost gets one more update attempt, which reverts
it again if it fails.

**Decided (2026-09-28) — a failed VM is destroyed by default** (`on_failure.old:
destroy`). A non-zero exit is almost always a bug or a dependency being down,
not a suspected compromise; quarantining every failure would fill the host with
suspects. Quarantine is opt-in per function (`old: quarantine`) or manual
(`mh quarantine`); nothing in v1 quarantines on its own judgement. `old: stop`
keeps the powered-off disk for a post-mortem, within the retention limits
(§11).

**Decided and implemented (2026-09-28) — why it failed survives the VM.**
Destroying a failed VM deletes
its disk and its console log, so the orchestrator captures the evidence
**before** it asks for the destroy, and keeps it as a *failure record*:

| Field | Source |
|---|---|
| `cause` | `exit` (with the code), `timeout`, `health`, `died` (with the engine's reason: OOM, process exited), `create` (the engine's error) |
| `output` | the last 4 KiB of the command's stdout+stderr, when there is a command |
| `console` | the last 16 KiB of the VM's serial console (kernel, init, the service's own messages) — E7, §15 |
| `function`, `generation`, `vm`, `image`, `at`, `after` | identity and timing: `after` is the time from boot to failure, so "fails right after starting" is visible at a glance |

Records are bounded: the last 5 per function, each field capped (about 20 KiB
per record). They are kept in the orchestrator's state directory (one private
file per function, rewritten atomically), so `mh-orchestrator status` shows the
last failure of each function and `mh-orchestrator failures [FUNCTION]` the
detail, even after the VM is gone and after an orchestrator restart. The state
of a function removed from the spec is removed with it. Console text comes from
the guest and is untrusted: it is stored and printed as data — control
characters escaped, so a guest cannot drive the operator's terminal — never
interpreted.

Captured on: a cycle that fails (not one skipped because the host is full, nor
one interrupted by the orchestrator stopping); a persistent VM that fails to
start, dies, or fails its health threshold; a dead VM found by an apply.

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

**Decided (2026-09-28): no `depends_on` in v1.** Functions start in any order
and a function whose dependency is not up yet fails and backs off like any
other. Completion-triggered work ("run B when A finishes") is out of scope.

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

A quarantined VM keeps its RAM. **Decided (2026-09-28):** `keep_quarantined` per
function (default 1) and `limits.max_quarantined` for the whole plant. When a
new suspect exceeds either, the oldest is powered off with its disk kept for
forensics (`old: stop` semantics); powered-off disks are destroyed, oldest
first, once they exceed `limits.quarantine_disk_mb`. Suspects are never deleted
silently: every removal is logged.

### No residue

**Decided (2026-09-28).** Everything the orchestrator creates carries its
ownership label, is accounted for by the project spec, and has a bound. Whatever
does not fit is removed on the next pass.

| Possible residue | What removes or bounds it |
|---|---|
| a cycle's VM left behind by an orchestrator crash | on start, an owned `transaction`/`window` VM that is not in progress is destroyed |
| VMs of a removed function | pruned on the next pass; quarantined ones: `mh apply` asks (§12) |
| VMs of an older generation after an update | `generation` label mismatch → destroyed |
| quarantined VMs | `keep_quarantined`, `limits.max_quarantined`, counted in the budget |
| powered-off disks (`old: stop`) | `limits.quarantine_disk_mb` |
| networks of removed functions | ownership label → pruned |
| results and failure records | `journal.max_mb` / `journal.max_age`; 5 failure records per function, capped fields |
| failure state of removed functions | pruned with the function |
| a bug in the orchestrator itself | the engine's quota for its `managed-by` (E5): past it the engine answers 429, whatever the orchestrator asks |

`mh orch status` reports these counts; "orphans: 0" is part of the O2 exit
criterion.

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
  and the orchestrator destroys the dead records and creates fresh instances.
  One authority per object.
- **Removed functions.** Serving instances are destroyed. **Decided
  (2026-09-28):** for quarantined ones, `mh apply` lists them in the plan and
  asks whether to keep or destroy them — evidence never disappears silently
  because configuration changed, and never piles up without a decision. Kept
  ones count in `keep_quarantined_orphans`. **Proposed:** without a terminal
  (Ansible, scripts) the choice must be given as `--quarantined=keep|destroy`;
  without it the whole apply is refused before any change.
- **Objects it does not own** are never modified, even if they collide by name;
  a collision is a validation error.

## 13. Updates

**Decided (2026-09-28).**

What a change in the project spec does:

| Change | Effect |
|---|---|
| image digest, `command`, `health`, `files` / `secrets`, `resources` | the function's VM is recreated — one function at a time |
| `labels` | patched in place |
| a network's egress / ingress rules | updated in place |
| a network's `subnet` | recreates the network and every VM on it: destructive, refused without `--allow-destructive` (§12) |

**One at a time**, in the order the functions appear in the spec. Each update
must succeed before the next one starts:

| Mode | How it is updated | Success |
|---|---|---|
| `transaction`, `window` | nothing to replace: the next cycle runs the new spec | that cycle succeeds (exit `0`, within `timeout` / `duration`) |
| `persistent` | engine `replace` with `old: destroy` — the new VM takes over the function's address | the `health` check passes; without one, the VM stays up for 30 s |

A `persistent` function with a fixed address is down for about a second during
its replace: the engine never lets two VMs serve one address. Accepted for v1;
`standby` (O5, §10) removes the gap.

**At the first failure, that function goes back to its previous version and
the roll-out halts.** The orchestrator keeps, per function, the last spec that
ran successfully (in its failure state, §8). Going back is another `replace`
(or, for a cycle, the next cycle) with that spec: a fresh VM from the previous
digest, about a second, never the failed VM reconnected. The function is then
*held*:

- it keeps running its previous version, and is not counted as failing — the
  new version failed, not the function;
- the failed attempt leaves a failure record (§8), so the operator sees why;
- functions already updated stay on the new version; functions not reached yet
  stay on the old one;
- `mh orch status` and every plan show the roll-out as **not converged**: which
  function is held, on which digest, and what the spec asks for;
- the loop never retries a held function by itself, so a bad version cannot
  turn into a boot loop.

**Resuming is `mh apply`.** Applying the same file again retries the held
function once and, if it succeeds, continues the roll-out; applying a corrected
file converges to that one instead. There is no separate resume command.

**Implemented (2026-09-28) and validated on test hardware** with the intranet
example. `mh-orchestrator apply` on the file a `run` is using validates and plans
it, shows it, and hands it over with SIGHUP (the lock file records the running
orchestrator's pid and spec); `run` plans it again and refuses it whole if it
does not plan. A cycle function is verified by one cycle run at once (not by
waiting for its next scheduled one, which could be a day away); its scheduled
cycles are paused meanwhile, so two versions never run side by side. A new
persistent version whose command exits before its health check passes fails at
once rather than after the whole health wait. Measured: a new app version rolled
out in ~7 s; a broken one (a syntax error) detected in 2 s and the previous
version back on a fresh VM 6 s later; a broken cycle version detected by its
verification cycle in 1 s.

**Known gap: held is not kept across an orchestrator restart.** The held flag
and the previous version live in the running orchestrator; a restart converges
to the file. With a broken file, a persistent function is then retried on the
broken version (back-off, then *degraded*) instead of staying on its previous
one. Keeping the last good version of each function in the state directory
closes it (to be decided: that includes file and secret contents).

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
| E7 | Console tail over the API — **done**, validated on ARM64 test hardware (2026-09-28: failure records carry the console): `GET /v1/vms/{id}/console?tail=N` (default 64 KiB, at most 4 MiB), any state until destroy, raw bytes (guest-written, untrusted), no symlinks followed; `mh logs` uses it | failure records must say why a VM failed before it is destroyed; the orchestrator reads nothing from the host's filesystem | O2 |
| E8 | Image defaults — **done** (2026-09-28): optional `command` and `health` in the image manifest, covered by the digest (absent fields leave older digests unchanged), durations stored canonical; `mh image import --command/--health-*` and `mh build` set them | an image usable without knowing it | O4 |

Pending engine validations, carried over: daemon restart with a quarantined VM,
and `nft -c` of the generated ruleset as root.

## 16. Milestones

Each slice is usable on its own and validated on test hardware before the next.

**Status (2026-09-28):** a first cut spanning O1 and the basics of O2/O3 is in
`orchestrator/` (`mh-orchestrator validate | plan | apply | run | status |
down`, first example in `orchestrator/examples/hello/microse.yml`). Unit-tested against
an in-memory engine, and run on ARM64 test hardware (2026-09-28) with the first
example: all three modes answered correctly (a transaction cycle, boot to
result, in ~170 ms; a VM reaching the persistent function over its network),
`down` left nothing behind. Failure round on the same hardware: a persistent VM
destroyed from outside was replaced in 0.6 s, one that died (guest reboot) in
4.6 s, one whose service stopped after 3 failed health checks (~25 s); a
transaction exiting non-zero was reported with its output and its VM destroyed;
a persistent function that never starts backed off (5, 10, 20, 40 s) and went
*degraded* after 5 attempts; stopping the orchestrator mid-window destroyed the
window's VM and reported the cycle as interrupted; a leftover cycle VM was
removed by the next apply; a quarantined VM of a removed function was kept.
Then `orchestrator/examples/intranet/microse.yml` (files: added to the orchestrator for
it): three Python services on one intra segment plus clients; destroying the
database (kv) was healed in ~6 s while the app answered 503 without failing;
killing the app's process was healed after 3 failed checks, with a failure
record carrying output and console; quarantining the log collector kept it
running, isolated and inspectable over vsock while a fresh one took its
address; the guest network never reached the office segment. What it does not do yet is listed in
`orchestrator/README.md`.

| # | Slice | Exit criterion |
|---|---|---|
| O0 | Engine prerequisites E1, E2 | networks carry labels; a VM boots only from a digest in the store |
| O1 | `mh apply` + one reconcile pass: strict parse, validation, budget, plan, apply networks and `persistent` functions, prune owned objects | apply twice → empty plan; remove a function → fully cleaned; over-budget or destructive spec refused before any change |
| O2 | `mh-orchestrator` loop: events, health, replace, back-off, *degraded*, persisted failure state and failure records (E3, E7) | kill a VM, kill the orchestrator, reboot the host: every function back, zero orphans, back-off preserved; `mh orch status` shows why each failure happened |
| O3 | Scheduler + work queue: `transaction`, `window`, jitter, coalescing, `max_age`, workers, journal (E5) | 200 functions at 30 s for 72 h unattended: flat boot rate, peak VMs ≤ computed peak — roadmap Phase 2 exit |
| O4 | `mh build` + image spec + `files`/`secrets` (E4) — **in progress**: `mh build` (fingerprint cache), `build:` in project specs, image defaults (E8), `${VAR}`/`.env`, `files`/`secrets` done; not yet validated on hardware, builds not reproducible | same spec → same digest; a replacement boots configured |
| O5 | `standby`, quarantine retention limits, rolling updates | failover without a gap; a failing function never exceeds its peak |
| O6 | Triggers: local socket, then outbound MQTT | a flood of triggers changes neither the peak nor the budget |

## 17. Decisions and open questions

Decided: YAML + strict parsing; ownership by label; three layers; build outside
the plant; digest mandatory; two files; intervals only. 2026-09-28: separate
binary (§3); `command:` in the project spec, optional image defaults, no named
entry points (§4); engine field names for networks (§5); the three modes, with
`recycle: never` (§7); failure is a process signal, no data schemas; `old:
destroy` by default, quarantine opt-in (§8); no `depends_on` in v1 (§9);
quarantine limits and the no-residue rule (§11); `mh apply` asks about
quarantined VMs of removed functions (§12); updates one at a time, a failed
update reverts that function and halts the roll-out, ~1 s gap accepted, resume
with `mh apply` (§13); the data stream stays a reference (§7).

Proposed in the review, awaiting agreement:

1. ~~Separate `mh-orchestrator` binary, engine API client (§3).~~ **Decided 2026-09-28.**
2. Engine-enforced digests via an image store (§4, E2) — **decided 2026-09-25**, implemented.
3. ~~Image declares `task` / `service` / `health`~~ — replaced 2026-09-28: `command:` in the project spec, optional image defaults (§4).
4. ~~Network blocks reuse the engine's field names (§5).~~ **Decided 2026-09-28.**
5. Cold boot in v1, snapshots later (§7).
6. Task contract + local JSON-lines journal as the v1 sink (§7).
7. Health over vsock only (§8).
8. Persisted failure state (§8).
9. No `after_completion` in v1 (§9).
10. Budget formula with `workers`; no priority classes in v1 (§11).
11. ~~Quarantined VMs survive the removal of their function (§12).~~ — replaced 2026-09-28: `mh apply` asks (§12).
12. ~~Roll-outs halt and alert; no automatic rollback (§13).~~ — replaced
    2026-09-28: the failed function reverts to its previous version, the
    roll-out halts (§13).

Open:

1. Where *degraded* and other alerts are delivered (log only, an orchestrator
   event stream, upstream MQTT).
2. ~~`keep_quarantined` default and the forensic disk-retention limit.~~ Default
   1 (decided 2026-09-28); the default of `limits.quarantine_disk_mb` is open.
3. `health.failures` and `max_age` defaults.
4. The 30-second settle time for a `persistent` function without a health check
   (§13): fixed, or configurable per function.

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

**2026-09-28 — second round, from a generic workload's point of view.**

- Named entry points declared by the image were too indirect for an operator
  who does not know the image: replaced by `command:` in the project spec with
  optional image defaults. The host's isolation does not depend on the guest's
  command (§4).
- Data schemas removed from the project spec: the orchestrator is generic, so
  failure is an exit code, a timeout, a failed health check or a death (§8).
- The default `old: quarantine` would have turned every ordinary failure into a
  suspect and filled the host: the default is now `destroy`, quarantine is
  opt-in and capped plant-wide (§8, §11).
- With `destroy` the evidence went with the VM: failure records capture the
  cause, the output and the console tail first (§8, E7).
- Added the no-residue table (§11); dropped `depends_on` from v1 (§9).
- Updates: "halt, no rollback" left a function down after a bad version. Now
  the function that failed reverts to its last good spec with a fresh VM, the
  roll-out halts and is resumed with `mh apply` (§13).
