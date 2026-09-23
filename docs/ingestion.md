# Ingestion data path — design

> Status: **design, nothing implemented.** Written 2026-09-18 for future work.
> Vocabulary follows `docs/roadmap.md`: the **engine** is MicroHosted (VMs,
> network, storage, host-initiated vsock); the **OT orchestrator** sits above it.
> This document specifies the orchestrator's data path and the few engine
> primitives it needs. It extends the gateway loop of `docs/iot-edge.md` and is
> the data half of roadmap Phase 2.
>
> Origin: a Prometheus/Grafana prototype (`~/prom-graf`, see "The prototype"
> below) that polls one ESP32 through `/exec`. It works for one sensor and
> showed exactly what does not survive 100–200.

## The problem at 100–200 sensors

Volume is not the problem: 200 sensors × 1 reading/s × ~200 B ≈ 40 KB/s. What
breaks is **how each reading travels** and **how 200 sensor↔VM pairs are kept
consistent**.

Today a reading is: HTTP call to the daemon → new vsock connection → `socat`
forks → `sh` → `wget` → `awk`. Measured ~0.1 s and ~4 process spawns in the
guest per reading; at 200 sensors × 1 Hz that is ~800 spawns/s to move numbers.
Worse than the cost: **the data plane is a root shell into every VM**.

`/exec` stays — it is the right tool to diagnose one machine. It is not a data
plane.

## Roles

```
┌─ sensor VM (untrusted) ─────────────┐
│ speaks the sensor protocol (pull or │
│ push), parses, quality-validates,   │
│ emits canonical records on DataPort │
└──────────────┬──────────────────────┘
               │ vsock, host-initiated, DataPort only
┌─ engine (MicroHosted, root) ────────┴───────────────────────────────┐
│ dials CONNECT <DataPort>, hands the connected fd out (SCM_RIGHTS)   │
│ typed events (VM died, vsock timeout, cgroup ceiling)               │
└──────┬──────────────────────────────────────────────▲───────────────┘
       │ fd                                           │ lifecycle calls
┌─ OT orchestrator ─────────────────────────────────────────────────────┐
│ ┌─ ingest (no engine access) ─┐  events  ┌─ controller ─────────────┐ │
│ │ limits, revalidation,       │ ───────▶ │ registry, reconciler,    │ │
│ │ identity, envelope, journal │          │ lifecycle policy, breaker│ │
│ └──────────────┬──────────────┘          └──────────────────────────┘ │
└────────────────┼──────────────────────────────────────────────────────┘
                 ▼
        local journal (durable) ──▶ consumers: upstream forwarder, TSDB,
                                    Prometheus exporter, local rules
```

| Piece | Owns | Privilege |
|---|---|---|
| Sensor VM | sensor protocol, parsing, data quality, canonical records | none toward the host |
| Engine | vsock, lifecycle primitives, events | root |
| Controller | sensor registry, reconcile, lifetime mode, circuit breaker | engine API (scoped to its VMs) |
| Ingest | stream limits, revalidation, identity, journal, events | only the fds it is handed |
| Consumers | forwarding, storage, dashboards, rules | read the journal |

**Why ingest and controller are separate processes.** Ingest is the only host
code that interprets bytes a compromised guest controls. It must not also hold
the credentials that restore, fork or re-network VMs. It asks for nothing and
acts on nothing: it reports, the controller decides.

**Why the engine hands out a connected fd instead of the vsock UDS.** Whoever
can connect to a VM's Firecracker UDS chooses the port in `CONNECT <port>`,
including `vsock.AgentPort` (52), a root shell in the guest. Passing an fd
already connected to DataPort means ingest never sees the UDS path, the port, or
the API.

## Guest side

- **Listens on a DataPort** (proposal: 1024), distinct from `AgentPort`. The
  data agent is part of the class's golden image, not injected at runtime.
- **Pull** (Modbus, OPC-UA, HTTP): the guest polls *its* sensor on its own
  schedule. **Push** (MQTT via `allowed_ingress`): the broker-VM translates.
  From the stream upward, both are identical.
- **Quality validation** against its class schema, then canonical records. This
  is for data quality only; the host re-validates (see below), because a
  compromised guest emits whatever it likes.
- **Ring buffer** of the last N records so a reconnect can resume without loss.
- **Clock is not trusted.** After a restore the guest clock is the snapshot's.
- **Golden snapshots must be taken with an empty buffer and `seq = 0`.**
  Otherwise every restored VM replays the snapshot's stale records.
- **Polling jitter must be seeded after restore.** Every VM restored from the
  same snapshot has the same PRNG state; left alone, 200 VMs poll in lockstep.
  The seed arrives in the handshake (below).

## Stream protocol (v0, draft)

Transport: Firecracker hybrid vsock. The engine connects to the VM's UDS, sends
`CONNECT <DataPort>\n`, reads `OK <port>\n` (same handshake as `internal/vsock`
`dial`), then hands the fd to ingest.

Framing: NDJSON, **max 4 KiB per line**. A longer line, invalid JSON or an
unknown message type is a protocol violation: close + event.

```
ingest → guest   {"t":"hello","proto":1,"resume_from":1042,"seed":"<random>","credit":64}
guest  → ingest  {"t":"hello","proto":1,"class":"dht-http","seq_head":1050}
guest  → ingest  {"t":"rec","seq":1042,"fields":{"temperature_c":27.9,"humidity_pct":51}}
guest  → ingest  {"t":"gap","from":1000,"to":1041}        # evicted from the ring buffer
guest  → ingest  {"t":"hb","seq_head":1050}               # idle heartbeat, every H seconds
ingest → guest   {"t":"credit","n":64}
```

- `class` from the guest is advisory; a mismatch with the registry is an event.
- `resume_from` = last journaled `seq` + 1. `seq` going backwards means the VM
  was reset; ingest opens a new epoch, it does not reject.
- **Credit-based flow control**: the guest may send at most the granted number
  of records. A flood is structurally impossible, not merely rate-limited.
- Heartbeats make silence meaningful: no record and no `hb` within the timeout =
  `silent`.
- Ingest writes only these fixed control messages; nothing from the host is
  derived from guest data.

## Ingest (host)

Per stream, in order:

1. **Limits**: line size, credit, idle timeout, bounded buffer (drop oldest).
2. **Revalidation** with the class schema: whitelisted fields only, all required
   present, numbers only (no bools), finite, within physical range.
3. **Envelope**:
   `{gateway_id, sensor_id, class, epoch, seq, received_at, fields}` —
   `sensor_id` comes from which fd the record arrived on (the controller's
   mapping), **never from the payload**; `received_at` is the host clock.
4. **Journal append.** This is the acceptance point: from here on nothing
   depends on the WAN or on any consumer.

Error kinds (carried over from the prototype, extended) and what they mean:

| Kind | Meaning | Signal |
|---|---|---|
| `missing_field` | sensor unreachable or partial read | liveness |
| `out_of_range` | physically implausible value | quality |
| `silent` | no record nor heartbeat within timeout | liveness |
| `bad_output` | output the guest's own extractor cannot produce | **security** |
| `protocol_violation` | oversize line, bad framing, unknown type, sent without credit | **security** |
| `class_mismatch` | guest claims a different class | **security** |

## Journal

Durable, local, append-only, with **one cursor per consumer**. Retention sized
for the longest WAN outage to survive (time and size bounded).

Candidates, decision open: NATS JetStream (file store), SQLite in WAL mode, a
plain segment log. Criteria: single static binary on ARM64, per-consumer
cursors, bounded retention, crash-safe append.

## Consumers

Each reads the journal from its own cursor; one failing never blocks ingestion
or the others, and it resumes where it stopped.

- **Upstream forwarder**: batching, compression, retry with backoff —
  store-and-forward for WAN outages. Global identity `gateway_id/sensor_id`.
- **Local TSDB** (optional): every reading, with its timestamp.
- **Prometheus exporter**: last value per sensor. A convenience view, not the
  ingestion sink — scraping loses readings between scrapes and stamps the scrape
  time.
- **Local rules**: act at the edge even with the WAN down.

## Control loop: ingest reports, the controller acts

Ingest emits events (`silent`, a streak of security-signal errors, credit
violations, `seq` regressions). The controller applies the policy of
`docs/iot-edge.md`:

- **Circuit breaker**: N security signals in a row → `Fork(quarantine=true)` for
  forensics, restore the sensor's VM from its clean snapshot, alert.
- **Liveness**: `silent` → check engine events (VM died? cgroup ceiling?) →
  restart or restore.
- **Aggregation**: many sensors silent at once is an access-network problem;
  one aggregated alert inhibits the individual ones.

Ingest never calls the engine.

## Declarative registry

The controller reconciles a desired state; VM ids are *actual* state, never
configuration.

```yaml
gateway_id: plant1-gw01
classes:                      # a handful of device classes, not 200 configs
  dht-http:
    snapshot: golden-dht-http
    schema:
      temperature_c: {min: -10, max: 60}
      humidity_pct:  {min: 0,   max: 100}
    lifetime: anomaly         # transaction | window | anomaly (docs/iot-edge.md)
    reset_every: 6h
sensors:
  - {id: hall1-t01, class: dht-http, address: 10.20.0.51:80}
  # … ×200
```

Per sensor, the reconciler ensures: its network (network-per-VM, per the
roadmap) with `allowed_egress` = exactly `address`; a VM restored from the
class snapshot; a data stream open and handed to ingest under that `sensor_id`.

## Lifetimes over the stream

| Mode | Stream use |
|---|---|
| Anomaly (persistent) | long-lived stream, scheduled or reactive reset |
| Window | stream open for the window, then dispose |
| Transaction | restore → connect → one `rec` → close → dispose |

All three use the stream; none needs `/exec`.

## Scaling notes

- 200 open fds and goroutines are nothing. Polling schedules move into the
  guests, so the host has no thundering herd — provided the jitter seed above.
- Horizontal scale is per gateway: each ingests its own sensors independently
  and forwards only the clean stream. No cross-gateway coordination.
- The ceiling per gateway is guest RAM (`docs/iot-edge.md` → Density), not the
  data path.

## Engine changes required

1. **DataPort** convention and a data agent in the class images
   (`scripts/build-rootfs-alpine.sh`).
2. **`OpenDataStream(vm) → fd`** over a narrow socket that offers only this
   operation, only for VMs owned by the calling orchestrator. `internal/vsock`
   `dial` already does the handshake; it needs to take the port and return the
   conn instead of speaking the agent protocol.
3. **Typed events** — roadmap Phase 1 item 2.
4. **Optional: `/exec` disabled by policy** on production VMs; diagnostic
   templates keep the agent.

## Open questions

- Journal technology (see above).
- Schema versioning of `fields` per class, and how a class schema change rolls
  out to running VMs (new golden snapshot + rolling restore?).
- A reset loses unsent ring-buffer records: accept, or drain before reset?
- `sensor_ts` for sensors with their own RTC: carry alongside `received_at`?
- Credit only, or credit plus a per-class rate ceiling?
- Push mode: does the broker-VM speak the same stream protocol unchanged?

## Phased plan

| Step | Deliverable | Exit criterion |
|---|---|---|
| S1 | Data agent + DataPort in the Alpine template; ingest prototype dialing DataPort directly | the prom-graf prototype runs with no `/exec` in the data path |
| S2 | `OpenDataStream` + fd passing over a narrow socket | ingest runs with no access to the API socket or any UDS |
| S3 | Journal + Prometheus exporter as a journal consumer | kill ingest mid-stream: zero lost or duplicated `seq` after restart |
| S4 | Registry + reconciler (controller) | 200 sensor entries → 200 VMs/networks/streams; delete one entry → fully cleaned up |
| S5 | Events + circuit breaker → quarantine + restore | a VM emitting `bad_output` is quarantined and replaced, alert fired |
| S6 | Upstream forwarder with store-and-forward | WAN cut for the full retention window: no loss on recovery |

S1 and S2 come first: the protocol and the engine↔orchestrator contract are
what everything else stands on.

## The prototype (`~/prom-graf`)

What exists today, as a stopgap to see data:

- `ingestor/ingestor.py` polls each VM via `POST /v1/vms/{id}/exec`, runs
  `wget | awk` in the guest, re-validates the JSON (whitelist, finite, ranges)
  and serves `/metrics`.
- Prometheus + Alertmanager + Grafana in Docker Compose, bound to the LAN
  interface.

Known stopgaps, all resolved by this design:

| Stopgap | Resolved by |
|---|---|
| `/exec` (root shell, ~4 spawns) per reading | DataPort stream (S1) |
| Container holds `/run/microhosted.sock` — root-equivalent: exec in any VM, rewrite egress, destroy | fd passing (S2) |
| Sequential polling loop | guest-side scheduling (S1) |
| `sensors.json` with hand-kept VM ids | registry + reconciler (S4) |
| Prometheus as the only sink | journal + consumers (S3, S6) |

What carries over: the validation rules, the error kinds, and the alert rules
(`prometheus/alerts.yml`).
