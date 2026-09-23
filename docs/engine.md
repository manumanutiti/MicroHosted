# The engine — a working guide

What the engine does, command by command, with real output, the exceptions you will meet, and how the parts that are not obvious
work inside: where an address points, how a replacement takes a VM's place, and
what keeps a function alive when something breaks.

This is the guide. The references are [api.md](api.md) (every endpoint and field)
and [cli.md](cli.md) (every flag). The security side has its own document:
[threat-model.md](threat-model.md).

> Every transcript here was captured on a live host (Firecracker via Jailer) on
> 2026-09-23. The engine runs on x86_64 and ARM64 alike; IDs, times and memory
> figures will differ on yours.

---

## 1. What the engine is — and what it is not

MicroHosted is split along one line:

- **The engine** (this daemon) hands out isolated execution units — microVMs — and
  gives each a lifecycle, a network policy, copy-on-write storage and a control
  channel that only the host can open. It keeps them in the state it was asked
  for, tells you when reality diverges (a VM died, a firewall apply failed), and
  offers the primitives to recover: quarantine, replace, restore.
- **An orchestrator** (not written yet) decides *policy*: which VMs should exist,
  when one is unhealthy or compromised, when to replace it, how often to retry,
  when to give up and raise an alarm.

The engine **never decides on its own that a VM should be restarted or replaced.**
That is on purpose: a VM that died because its parser was exploited must not be
brought back automatically. The engine reports; the orchestrator decides.

### The objects

| Object | What it is | Identity |
|---|---|---|
| **Template** | a kernel + a golden rootfs + default shape (`vcpus`, `mem_mb`, `disk_mb`) | name (`base-alpine`) |
| **VM** | one microVM: a CoW clone of a template's disk, a Firecracker process, maybe a TAP on a network | 8-hex ID (`b5195576`), optional unique **name**, free **labels** |
| **Network** | an L2 segment: bridge + subnet + IPAM + a firewall policy | name (`doc-demo`) |
| **Snapshot** | a frozen VM: memory + device state + disk at one instant | ID, optional name |
| **Volume** | a persistent ext4 disk that outlives the VMs it is attached to | ID, name |
| **Function** | not an object in the API: *the job a VM does*, which is its **network address** plus its **labels** (and the firewall rules that point at the address). It is what survives when the VM serving it is replaced. | address + labels |
| **Event** | something that happened, pushed to subscribers | `epoch:seq` |

Everything is driven through the HTTP API on `/run/microhosted.sock`; `mh` is a
thin client over it. Every `mh` example below has an API equivalent in
[api.md](api.md).

---

## 2. A VM's life

### States

```
               create / fork                stop                  start
   (nothing) ─────────────────► running ─────────────► stopped ─────────────► running
                  │                │  ▲                   ▲
                  │                │  └── restore ────────┤ (rewinds disk + memory in place)
     "creating"   │                │                      │
     while in     │                └─── process dies ─────┘  → vm.died, last_exit = when + why
     progress     │                     on its own              (NOT restarted: the orchestrator decides)
                  ▼
     destroy from any state ──► (nothing)
```

`quarantine` is not a state, it is a **flag** a VM of any state can carry: the VM
keeps running (or stays stopped) but is off every network.

`creating` is only ever seen internally: the record is written before the first
side effect so that a daemon crash in the middle is **undone** at the next start,
never half-kept (see §9).

### What each operation keeps and frees

| | process | TAP | IP reservation | disk | name | labels |
|---|---|---|---|---|---|---|
| `stop` | killed (graceful, ≤5 s) | removed | **kept** | kept | kept | kept |
| `start` | new | recreated | (still held) | same | same | same |
| `quarantine` | **kept running** | kept, **off the bridge** | **released** | kept | kept | + `lease=quarantined` |
| `destroy` (`rm`) | killed | removed | released | **deleted** | released | gone |
| process dies on its own | — | removed | kept | kept | kept | kept (+ `last_exit`) |

So `stop` + `start` brings a VM back **at the same address with the same disk**;
`destroy` is the only thing that deletes data.

### Create, look, run a command

```console
$ mh run base-alpine --net doc-demo --ip 172.16.79.2 --name broker-a -l role=broker -l managed-by=doc
b5195576
created b5195576 (broker-a): base-alpine, doc-demo 172.16.79.2

$ mh run base-alpine --net doc-demo --name scratch -l managed-by=doc
62cf6b3c
created 62cf6b3c (scratch): base-alpine, doc-demo 172.16.79.3

$ mh ps -L -l managed-by=doc
VM ID      NAME       TEMPLATE      STATE     NETWORK    IP            VCPU   MEM    RSS   UPTIME   CREATED   LABELS
62cf6b3c   scratch    base-alpine   running   doc-demo   172.16.79.3   1      128M   20M   0s       0s ago    managed-by=doc
b5195576   broker-a   base-alpine   running   doc-demo   172.16.79.2   1      128M   25M   1s       1s ago    managed-by=doc,role=broker
```

The ID goes to stdout (so `VM=$(mh run …)` works); the human line goes to stderr.
`RSS` is what the VM really costs the host right now: guest memory is allocated
lazily, so a 128 MB VM idles at 20–35 MB.

```console
$ mh exec t2 'ip -4 addr show eth0 | grep inet'
    inet 172.16.0.2/24 brd 172.16.0.255 scope global eth0
```

`exec` runs over **vsock**, not the network: it works on a VM with no network at
all, and on a quarantined one. Its exit code is the guest command's.

> **Exception — exec right after create.** `create` returns when Firecracker has
> booted the guest, not when the guest's vsock agent is listening (except for a
> VM with volumes, whose create waits for the agent to mount them). An `exec`
> in the first second or so can fail with `reading CONNECT ack: EOF`. Retry after
> a moment. (Open item: see §10.)

---

## 3. Identity: ID, name, labels

Three different things, deliberately:

- **ID** — generated, 8 hex characters, never reused while the VM exists. It is
  also in the TAP name (`tap<id>`) and the jail path.
- **name** — optional, an alias for **this instance**: unique while it exists,
  never changed. `broker-a` is one VM; its replacement is `broker-b`. Any command
  that takes a VM takes its name, its ID or a unique ID prefix.
- **labels** — free `key=value` pairs, changeable at any time
  (`mh label VM k=v k-`), selectable (`mh ps -l k=v`, `GET /v1/vms?label=k=v`).
  They carry what must **outlive** the instance: *what it serves*
  (`role=broker`, `sensor=ts-01`) and *who owns it* (`managed-by=…`).

A replacement inherits the labels, not the name. A fork inherits neither — so a
copy taken for forensics never claims to be the sensor. **Select by label, not
by name**: a pipeline that looks for "the VM called broker" breaks the moment the
broker is replaced; one that looks for `role=broker` does not.

---

## 4. Networks and addresses

### The default is nothing

A new network lets nothing in or out, and its VMs cannot see each other:

```console
$ mh network create doc-demo --subnet 172.16.79.0/24 --in tcp:192.0.2.10:1883@wlan0=172.16.79.2
NAME       SUBNET           GATEWAY       BRIDGE         INTRA   OUT    IN
doc-demo   172.16.79.0/24   172.16.79.1   mhbr33afad30   off     none   tcp:192.0.2.10:1883@wlan0=172.16.79.2
```

What is open is listed, by **who opens the connection**:

- **OUT** (`allowed_egress`): flows a VM may open — to an internet host, or to a
  device behind a *managed interface* (`@wlan0`). `--internet eth0` opens all
  outbound, through that one interface only.
- **IN** (`allowed_ingress`): flows a device behind a managed interface may open
  *into* one VM.
- **intra**: VM↔VM inside the network (off by default — each TAP is an isolated
  bridge port).

The mechanics are in [networking.md](networking.md). Here is the one that matters
most for keeping a function alive.

### How an IN rule and its `to_ip` work

```
IN rule:  tcp:192.0.2.10:1883@wlan0=172.16.79.2
          │   │          │    │     └─ to_ip: the VM address that receives it
          │   │          │    └─ the managed interface it arrives on
          │   │          └─ the port the device connects to (on the HOST's address)
          │   └─ the only source allowed
          └─ protocol
```

The device does not know the VMs exist. It connects to **the host's own address**
on `wlan0`, port 1883. Inside the host:

```
device 192.0.2.10
   │  TCP → <host's wlan0 address>:1883
   ▼
wlan0 ── nftables prerouting: does it match the rule? yes
         rewrite destination → 172.16.79.2:1883        (DNAT, in the kernel)
   │
   ▼
bridge of doc-demo ──► whichever VM holds 172.16.79.2 right now
```

The host listens on nothing (`ss -ltn` shows no 1883) and never parses a byte:
the kernel rewrites the destination and forwards the packet. Replies go back the
same way.

**The rule names an address, not a VM.** It reaches whoever holds `172.16.79.2`.
That is exactly what lets a function survive its VM: replace the VM, give the
replacement the same address, and the rule, the device and everything outside
keep working untouched.

It is also a hazard, which is why these addresses are **pinned**:

| Situation | Where the device's traffic goes |
|---|---|
| VM A holds `.2` | A |
| A is quarantined or destroyed | nowhere (the device retries) |
| someone runs `mh run --net doc-demo` | **not** to that VM: automatic allocation never hands out an address an IN rule points at |
| the replacement takes `.2` explicitly | the replacement — rule untouched |

Automatic allocation skips pinned addresses, even free ones:

```console
$ mh run base-alpine --net doc-demo --name scratch -l managed-by=doc
created 62cf6b3c (scratch): base-alpine, doc-demo 172.16.79.3        ← .2 is pinned, skipped
```

Only an **explicit claim** takes a pinned address:

- `mh run --ip 172.16.79.2` (API: `guest_ip` on create), or
- a fork of a snapshot taken at that address (the guest's address is frozen in
  its memory), or
- `mh replace`, which does one of those two for you.

Exceptions:

```console
$ mh run base-alpine --net doc-demo --ip 172.16.79.2        # while another VM holds it
mh: conflicting resources: network "doc-demo": address already in use on this network: 172.16.79.2
$ mh run base-alpine --no-net --ip 172.16.79.9
mh: invalid request: guest_ip needs a network, and no_network is set
```

(409 and 400 in the API.) An address outside the subnet, or the network, gateway
or broadcast address, is a 400.

---

## 5. Keeping a function alive

The goal: **the job a VM does is never left unserved for longer than it takes to
boot a replacement, and a failure that keeps happening does not fill the host
with VMs.** No single mechanism does that; it is layered, from the bottom up.

| What breaks | What notices | What happens | Who acts |
|---|---|---|---|
| the daemon crashes | systemd | restarted with a growing delay (2 s → 60 s), never given up on. **VMs keep running meanwhile** (`KillMode=process`); on start, `Reconcile` re-adopts them by PID | engine |
| the host reboots | the daemon, at start | every VM is found dead and **kept as `stopped`** (disk and address intact); those with `autostart` are booted again, one by one | engine |
| a VM's process dies (OOM, crash, guest reboot) | the monitor, within ~2 s | marked `stopped` with `last_exit` (when, and whether the cgroup's OOM killer did it); `vm.died` event. **Not restarted** | orchestrator |
| a VM misbehaves or looks compromised | only the orchestrator can tell | `quarantine` (cut off, kept for forensics) and/or `replace` (a clean VM takes the function) | orchestrator |
| the replacement fails to boot | the engine | old VM stays quarantined, never reconnected; `vm.replace_failed` event; the function is down until a retry | orchestrator (back-off, then *degraded*) |

The engine's part is to make the orchestrator's actions **safe and cheap** —
atomic, all-or-nothing, impossible to double — and to **tell** it what happened.

### Quarantine: cut a VM off, keep it alive

```console
$ mh quarantine 149d230a
149d230a
$ mh ps
VM ID      NAME   TEMPLATE      STATE     NETWORK        IP             …
149d230a   t1     base-alpine   running   (quarantine)   (172.16.0.2)   …
$ ping 172.16.0.2
From 172.16.0.1 icmp_seq=1 Destination Host Unreachable
$ mh exec 149d230a whoami
root
```

What happens, in this order (fail-closed: each step only moves the VM *away* from
the network):

1. its TAP leaves the bridge (`ip link set tapX nomaster`) — still up, attached to
   nothing, so every frame the guest sends dies at the host;
2. the record is persisted: `quarantine: true`, `quarantined_from: <network>`,
   label `lease=quarantined`;
3. the IP reservation is released.

The IP in parentheses is what the guest **still believes** it has; the network no
longer reserves it for this VM. If step 2 fails the TAP is put back and nothing
has changed. If the daemon dies between 1 and 2, the next start re-attaches the
TAP (the quarantine was never acknowledged, so it is undone).

There is no un-quarantine: a quarantined VM returns to service only as a new VM.

### Replace: a clean VM takes the function

```console
$ mh replace b5195576 --old stop --name broker-b
10ba08e7
replaced b5195576 with 10ba08e7 (broker-b): base-alpine, doc-demo 172.16.79.2
b5195576 left stopped, quarantined

$ mh ps -a -L -l managed-by=doc
VM ID      NAME       TEMPLATE      STATE     NETWORK        IP              …   LABELS
10ba08e7   broker-b   base-alpine   running   doc-demo       172.16.79.2     …   managed-by=doc,role=broker
62cf6b3c   scratch    base-alpine   running   doc-demo       172.16.79.3     …   managed-by=doc
b5195576   broker-a   base-alpine   stopped   (quarantine)   (172.16.79.2)   …   lease=quarantined,managed-by=doc,role=broker

$ mh inspect 10ba08e7 | jq '{id,name,network,guest_ip,labels,replaces}'
{
  "id": "10ba08e7",
  "name": "broker-b",
  "network": "doc-demo",
  "guest_ip": "172.16.79.2",
  "labels": { "managed-by": "doc", "role": "broker" },
  "replaces": "b5195576"
}
```

The replacement has the old VM's address, network and labels (minus `lease`), a
name of its own, and `replaces` pointing back for forensics. The IN rule for
`.2` now reaches `broker-b` without anyone touching it.

**Where the replacement comes from:**

| flag | source | notes |
|---|---|---|
| *(none)* | the old VM's template, same vcpus/mem/disk | a cold boot: whatever the function needs must be in the template |
| `-t TEMPLATE` | another template | same shape as the old VM |
| `-s SNAPSHOT` | a fork of that snapshot | must have been taken **at the function's address** (409 otherwise): the guest's address is frozen in its memory. Take it from a known-clean VM, never from the suspect one after the fact |

**What happens to the old VM** (`--old`):

| value | old VM ends | why pick it |
|---|---|---|
| `quarantine` (default) | running, cut off, reachable over vsock | live forensics: inspect memory, processes, connections |
| `stop` | quarantined **and powered off**, disk kept | keeps the evidence on disk but frees its RAM *before* the replacement boots — on a small host that is often the difference between a replacement and a 503. The graceful power-off can take up to ~5 s |
| `destroy` | deleted, once the replacement is up | nothing to keep (a plain crash, not a suspicion) |

**The order, and why:**

```
 1. check everything                      nothing has changed yet: any refusal here is a clean 4xx
    ├─ the VM exists, serves on a network (else 409), carries no volumes (else 409)
    ├─ the source exists: template in the catalog (else 400), snapshot at the function's address (else 409)
    └─ NO OTHER VM ALREADY SERVES THIS FUNCTION (else 409)        ← the anti-flood guard
 2. cut the old VM off (quarantine)       frees the address
 3. old=stop|destroy → power it off       frees its RAM for step 4
 4. boot the replacement, claiming the function's address
    ├─ ok   → inherit labels + autostart, record replaces=<old>
    └─ fail → STOP HERE: old VM stays quarantined, vm.replace_failed, error to the caller
 5. old=destroy → delete the old VM       only now that the function is served again
```

Three properties follow:

- **A suspect is never reconnected.** If step 4 fails, the old VM stays
  quarantined. A sensor without data is better than a compromised sensor with
  data. The error says the function is down, and the same `replace` can simply be
  retried: a quarantined VM remembers its function's network (`quarantined_from`)
  and address.
- **It cannot double.** Replacing the same VM again is refused, and boots nothing:

  ```console
  $ mh replace b5195576
  mh: conflicting resources: the function of vm b5195576 (172.16.79.2 on doc-demo) is already served by vm 10ba08e7
  ```

  A looping or confused caller gets 409s, not VMs. That is the engine's half of
  "a persistent failure must not flood the host". The other half — waiting longer
  between attempts, and giving up after N — is the orchestrator's (§6).
- **A bad request changes nothing.**

  ```console
  $ mh replace 10ba08e7 --template nope
  mh: invalid request: template "nope" not found in catalog
  ```

  `10ba08e7` is still serving, untouched: every check runs before step 2.

**Limits today:** a VM with volumes attached cannot be replaced (409): volumes are
not handed over yet. A crash of the daemon in the middle leaves the same state as
a failed step 4 (the half-created replacement is undone at start; the old VM
stays quarantined) — retry.

### What the engine does not do here

It does not health-check a guest, decide that a VM is compromised, retry a failed
replace, or back off. Those need knowledge of the workload (what a healthy
response is, what data is anomalous), and they are policy. The events below are
how an orchestrator learns what to act on.

---

## 6. Events

### What they look like

Live, as a person reads them:

```console
$ mh events
2026-09-23 02:53:01  vm.created              b5195576 (broker-a)  net=doc-demo  template=base-alpine
2026-09-23 02:53:01  vm.created              62cf6b3c (scratch)  net=doc-demo  template=base-alpine
2026-09-23 02:53:01  vm.quarantined          b5195576 (broker-a)  net=doc-demo
2026-09-23 02:53:06  vm.stopped              b5195576 (broker-a)  net=doc-demo
2026-09-23 02:53:06  vm.created              10ba08e7 (broker-b)  net=doc-demo  template=base-alpine
2026-09-23 02:53:06  vm.replaced             b5195576 (broker-a)  net=doc-demo  old=stop  replacement=10ba08e7
2026-09-23 02:53:07  vm.destroyed            10ba08e7 (broker-b)  net=doc-demo
```

That is the `replace --old stop` above, as it happened: the old VM cut off, then
powered off (5 s of graceful shutdown), the replacement created, and the handover
recorded.

As a program reads them (`mh events --json`, one per line):

```json
{"epoch":"540b7354","seq":5,"time":"2026-09-23T00:53:01.166384087Z","type":"vm.quarantined","vm":"b5195576","name":"broker-a","labels":{"lease":"quarantined","managed-by":"doc","role":"broker"},"network":"doc-demo"}
```

And on the wire (`GET /v1/events`, Server-Sent Events):

```
id: 540b7354:5
event: vm.quarantined
data: {"epoch":"540b7354","seq":5,…}

```

A `vm.*` event describes the VM as it was at that moment: its name, labels and
network (for a quarantined VM, the network it served on). The full list of types
is in [api.md § Events](api.md#events); the ones an orchestrator lives on:

| event | means | typical reaction |
|---|---|---|
| `vm.died` | a VM's process died; `reason` says why if the host can tell (e.g. its cgroup OOM-killed it) | replace it (or start it, if it was a plain crash) |
| `vm.replace_failed` | **the function is down**: old VM cut off, replacement did not boot | retry with back-off; after N failures mark the function degraded and alert |
| `vm.replaced` | the function moved; `data.replacement` is the new VM | update your map from function to VM |
| `network.ruleset_failed` | `nft -f` refused a ruleset: the policy in force may not be the declared one | alert: this is a security event |
| `reset` | you missed events you can no longer get | re-read the state (`GET /v1/vms?label=…`), then carry on |

### How to consume them

**From the shell:**

```bash
mh events                                        # from now on
mh events -l role=broker -t vm.died -t vm.replace_failed
mh events --all --no-follow                      # everything still kept, then exit
curl -N --unix-socket /run/microhosted.sock http://localhost/v1/events?since=now
```

`mh events` keeps following across daemon restarts: it reconnects on its own and
resumes where it was.

**From a program** — the pattern an orchestrator follows:

```
position := load()                     # last "epoch:seq" handled, or empty
loop:
  open GET /v1/events?label=managed-by=me&since=<position>   (or Last-Event-ID)
  for each event:
    if type == reset:  resync()        # re-read GET /v1/vms?label=managed-by=me, rebuild the picture
    else:              handle(event)
    position = event id; save(position)
  on disconnect: wait a little, reopen with the same position
```

A minimal but real one, in bash — replace any of *my* VMs that dies, at most
once per 30 s per function (a crude back-off):

```bash
declare -A last
mh events --json -l managed-by=me -t vm.died | while read -r ev; do
  vm=$(jq -r .vm <<<"$ev"); fn=$(jq -r '.labels.role' <<<"$ev")
  now=$(date +%s)
  if (( now - ${last[$fn]:-0} < 30 )); then echo "backing off $fn"; continue; fi
  last[$fn]=$now
  mh replace "$vm" --old destroy || echo "replace of $vm failed; function $fn is down"
done
```

It is a sketch — a real orchestrator persists its position, grows the back-off,
counts failures to declare the function degraded, and health-checks VMs that are
alive but wrong (no event can tell it that).

### The guarantees

- **Order.** Within one run of the daemon (one `epoch`) every subscriber sees the
  same events in the same order; `seq` grows by one per event.
- **Nothing is lost without telling you.** The daemon keeps the last 1024 events.
  Reconnect with your last id and you get what you missed first. If that is
  impossible — the daemon restarted (new epoch), or you come back from further
  than 1024 events ago — the stream **starts with `reset`**, and you resync from
  the state. After the backlog the stream sends your position as a bare `id:`
  line, so even a subscriber that received nothing (quiet host, strict filters)
  resumes from the right place.
- **The engine never waits for you.** A subscriber that falls 256 events behind
  is disconnected, not waited for; it reconnects and catches up (or gets a
  `reset`). A slow orchestrator cannot slow VM operations down.
- **Not an audit log.** Events live in memory: a daemon restart starts a new
  epoch. What is durable is the state (`GET /v1/vms`), which is exactly what a
  `reset` sends you back to.
- **Not everything is an event yet**: a vsock exec that times out, and cgroup
  memory pressure short of an OOM kill, are not reported (§10).

---

## 7. Snapshots, forks, restore

```bash
mh vm snapshot a1b2 --name clean       # freeze memory + disk (VM paused ~0.3 s)
mh restore a1b2 clean                  # rewind a1b2 in place: same ID, same IP
mh snapshot fork clean --quarantine    # a new VM from it, on no network
mh fork a1b2 --quarantine              # snapshot + fork in one go
```

The one thing to know: **a guest's IP and MAC are frozen in its snapshot's
memory** and cannot be changed on restore. So a fork either rejoins the original
network **at the snapshot's address** (and is refused with 409 while someone
holds it), or goes to quarantine (no network, any number of copies). That is why
a replace from a snapshot requires one taken at the function's address. A VM with
volumes attached cannot be snapshotted (its memory holds them mounted).

Details and timings: [architecture.md § Snapshots and forking](architecture.md#snapshots-and-forking).

## 8. Files and volumes

```bash
mh cp ./sample.bin a1b2:/root/         # running VM: over vsock; stopped VM: offline (debugfs)
mh volume create output --size 512M
mh run base-alpine --no-net -v sample:/mnt/sample:ro -v output
```

The host never mounts a guest filesystem; see [volumes.md](volumes.md).

---

## 9. When things go wrong

### Exceptions you will meet

| HTTP | `mh` says (real messages) | means | what to do |
|---|---|---|---|
| 400 | `invalid request: guest_ip needs a network, and no_network is set` | the request is wrong on its own terms (bad name, label, size, address, unknown template in a replace) — **nothing was touched** | fix the request |
| 404 | `vm not found: …` | no such VM / snapshot / network | |
| 409 | `conflicting resources: network "doc-demo": address already in use on this network: 172.16.79.2` | the request is valid but collides with what exists: an address taken, a name taken, a function already served, a snapshot at the wrong address, volumes attached | free the resource, or choose another |
| 409 | `vm in incompatible state: vm b5195576 is not running (state stopped)` | wrong state for the operation (exec on a stopped VM, start on a running one, quarantine twice) | |
| 409 | `conflicting resources: vm … is busy (replace in progress)` | another operation on the same VM is running: **one lifecycle operation per VM at a time** | retry after it |
| 503 | `insufficient host capacity: …` | admission refused a launch: it would leave less than `--mem-reserve-mb` (512 MB) of host memory available, or exceed `--max-vms` | nothing is wrong with the request; retry once something stops |
| 500 | the step that failed (`cloning rootfs: …`, `creating tap device: …`) | the host failed underneath — **and everything done so far was undone** | look at the journal; `mh doctor` |

`mh` exits 1 when the operation failed, 2 for a bad command line, and for `exec`
with the guest command's own code.

### All-or-nothing

Every create, fork, network create and quarantine ends in one of two states: done,
or as if it had never been asked. That includes the daemon being `kill -9`'d in
the middle: the record is written as `creating` before the first side effect, and
the next start undoes it — clone, TAP, jail dir, cgroup and orphan Firecracker
included. This was fault-injected at every step on test hardware
(`scripts/fault-test.sh`, 21/21).

The firewall fails closed: a network is not usable until the ruleset listing it
is in force; a ruleset that cannot be applied rolls the network back; and any
bridge of ours the ruleset does not know is dropped both ways.

### The drift detector

```console
$ mh doctor
clean: the daemon and the host agree
```

It compares what the daemon believes with what the host has — Firecracker
processes, TAPs, bridges, jail dirs, cgroups, disks, logs, IP leases, volume
claims, the last ruleset apply — and changes nothing. Restarting the daemon
cleans everything it reports except disks.

---

## 10. Engine status

Validated on test hardware: lifecycle, networks and firewall (including managed
interfaces, IN/OUT rules), snapshots/forks/restore, volumes, admission, crash
consistency (`fault-test.sh` 21/21), host reboot with autostart, quarantine in
place, pinned addresses and replace (`replace-test.sh` 31/31), events
(`events-test.sh` 10/10).

Open, and known:

- **exec right after create** can fail until the guest agent is up (§2). Either
  `create` waits for the agent, or callers retry.
- **Replace does not hand over volumes** (409 for a VM with volumes).
- **No per-consumer quota**: admission is host-wide; one orchestrator can starve
  another.
- **Events do not cover** vsock exec timeouts or cgroup memory pressure short of
  an OOM kill.
- **No disk or network rate limits per VM**: CPU, memory and PIDs are capped, but
  one VM can saturate the SD card or its bridge for the others.
- **`mh doctor` does not check** that each TAP is on the bridge its record says
  (a quarantine whose rollback failed would show only after a daemon restart
  fixes it).
- **The engine/orchestrator cut is not in the code yet**: everything is in
  `internal/vm`; `internal/engine` as the only thing an orchestrator may import
  is Phase 1's exit criterion ([roadmap.md](roadmap.md)).
