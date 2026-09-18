# HTTP API — reference

Served by `internal/api` (`cmd/microhosted`). It listens on a **Unix socket**,
`/run/microhosted.sock` by default (`--socket`). All bodies are JSON.

## Calling the API, and who may

The socket's file permissions **are** the authorization. That is the whole
access-control story, and it is deliberate: this API creates VMs, runs commands
inside them and rewrites each network's egress policy, from a daemon running as
root — so "who may call it" is the same question as "who may open this file",
which the host already knows how to express, audit with `ls`, and revoke. There
is no token to distribute, rotate or leak into a shell history.

The socket is root-only unless it was installed with an owning group
(`sudo SOCKET_GROUP=microhosted ./scripts/install-service.sh`, mode 0660). To
use it without `sudo`, join that group and **start a new login session** —
`usermod` does not touch sessions that are already open:

```bash
sudo usermod -aG microhosted $USER
# log out and back in, then confirm:
id -nG | tr ' ' '\n' | grep microhosted
```

### Day to day: the `mh` CLI

For operating the platform by hand, use **[`mh`](cli.md)**, the docker-style
client installed with the daemon (`mh ps`, `mh run base-alpine`,
`mh network update lab --intra`). This page is the raw HTTP reference: what a
panel, a script in another language, or `mh` itself sends.

### The `mhcurl` helper used by every example below

`mhcurl` is **a shell function you define yourself**, nothing in this repo
installs it. Define it once (or drop it in `~/.bashrc`). It used to be called
`mh`, which is now the CLI binary, so a leftover `mh()` function in your shell
would hide the binary. Rename it.

```bash
mhcurl() { curl -sS --unix-socket "${MICROHOSTED_SOCKET:-/run/microhosted.sock}" "$@"; }

mhcurl http://localhost/v1/health
```

Being a shell function, it does **not** survive `sudo`. If you are not in the
socket's group yet, spell the call out instead:

```bash
sudo curl -sS --unix-socket /run/microhosted.sock http://localhost/v1/health
```

> Two traps worth knowing, because curl hides both. `curl -s` silences transport
> errors, so a daemon that is down or a socket you may not open both come back as
> empty output rather than a message — use `-sS`, as the helper does. And curl
> reports "permission denied" on a Unix socket as `Could not connect to server`,
> which reads like the daemon is dead when it is only that you are not in the
> group. (`mh` tells the two apart.)

`--addr host:port` serves on a TCP port **instead** of the socket. There is no
authentication in front of that port, so anyone who can reach it has
root-equivalent control of the host: it is for a loopback-only dashboard or a
tunnel, never for a segment where the workloads themselves live.

Status: covers create/read/delete + stop/start + exec + snapshots/fork +
volumes/files + observability (`/v1/system`, `/v1/health`). The full CRUD
(including "update" and create/delete nuances) is in progress.

**Route convention** (the whole API follows this rule):

- **Resources = nouns with pure CRUD**: `POST/GET /v1/<resource>` and
  `GET/DELETE /v1/<resource>/{id}` — `vms`, `snapshots`, `networks`, `templates`.
- **Actions = verbs**: `POST /v1/<resource>/{id}/<verb>` — `stop`, `start`,
  `snapshot`, `fork`, `restore`, `exec`. The same verb means the same thing
  wherever it hangs (e.g. `fork` exists on `/vms/{id}` and on `/snapshots/{id}`
  with the same body and the same response; only the origin changes). What an
  action creates then lives in its collection: `snapshot` creates in
  `/v1/snapshots`, `fork` creates in `/v1/vms`.
- **Read-only singletons**: `GET /v1/system` and `GET /v1/health` — unique
  resources (there's only one host), with no `{id}` (see `## Observability`).

---

## Templates (catalog)

### `GET /v1/templates`

Lists the templates ("micro-machines") available to clone.

**200 response** — an array of `Template`:

```json
[
  {
    "name": "base-ubuntu",
    "description": "Ubuntu 22.04 (official Firecracker CI rootfs)",
    "kernel_path": "images/kernels/vmlinux-6.1.102",
    "rootfs_path": "images/rootfs/ubuntu-22.04.ext4",
    "vcpus": 1,
    "mem_mb": 128
  }
]
```

Source: `images/catalog.json`, loaded once when the daemon starts
(`storage.LoadCatalog`).

---

## Networks

Named L2 segments (bridge + subnet + nftables policy). Model detail in
`docs/networking.md`. On startup a `default` network always exists
(`172.16.0.0/24`, no internet egress).

### `POST /v1/networks` — create

**Body** (`CreateNetworkRequest`):

| field            | type   | required | description                                                        |
|------------------|--------|----------|---------------------------------------------------------------------|
| `name`           | string | yes      | unique network name                                                |
| `subnet`         | string | no       | CIDR (e.g. `10.10.0.0/24`); if omitted, a free `/24` is assigned   |
| `egress`         | bool   | no       | if `true`, the subnet goes to the internet via NAT; `false` by default |
| `allowed_egress` | array  | no       | fine-grained egress: only these flows reach the WAN; requires `egress` = `false`. See `docs/networking.md` |
| `allowed_ingress` | array | no       | devices on a managed interface allowed to connect into one guest address (DNAT); requires `subnet`. See `docs/networking.md` § Ingress |
| `intra`          | bool   | no       | if `true`, the network's VMs see each other (L2); `false` by default: each TAP is an isolated bridge port |

Each element of `allowed_egress` is `{"ip", "protocol", "port"}`: `ip` an IPv4 or
canonical IPv4 CIDR, `protocol` ∈ `tcp`/`udp`/`icmp` (lowercase), `port` required
for tcp/udp and forbidden for icmp.

```bash
mhcurl -X POST http://localhost/v1/networks -d '{"name":"lab"}'
mhcurl -X POST http://localhost/v1/networks -d '{"name":"build","egress":true}'
mhcurl -X POST http://localhost/v1/networks -d '{"name":"iot","allowed_egress":[
  {"ip":"203.0.113.7","protocol":"tcp","port":8883},
  {"ip":"203.0.113.7","protocol":"icmp"}]}'
```

Each rule also takes an optional **`iface`**. Empty (the usual case) means the
WAN: anywhere that is not one of our bridges. Set, it must name an interface the
daemon was told to manage (`--managed-iface`) — one whose whole policy it owns,
denied in both directions by default — and the rule then also emits the matching
return rule. `ip` may be a host or a CIDR in either case. See
`docs/networking.md` § Managed interfaces.

```bash
mhcurl -X POST http://localhost/v1/networks -d '{"name":"ot-52","allowed_egress":[
  {"iface":"wlan0","ip":"192.168.50.52","protocol":"tcp","port":502},
  {"ip":"203.0.113.7","protocol":"tcp","port":8883}
]}'
```

Each element of `allowed_ingress` is `{"iface", "src_ip", "protocol", "port",
"to_ip"}`: `iface` a managed interface (required), `src_ip` an IPv4 or canonical
IPv4 CIDR, `protocol` `tcp`/`udp`, `port` 1–65535 (the same on the host and in
the guest), `to_ip` a guest address of the network's subnet.

```bash
mhcurl -X POST http://localhost/v1/networks -d '{"name":"mqtt-60","subnet":"172.16.9.0/24","allowed_ingress":[
  {"iface":"wlan0","src_ip":"192.168.50.60","protocol":"tcp","port":1883,"to_ip":"172.16.9.2"}
]}'
```

**201 response** (`NetworkResponse`) · **400** if `name` is missing, if an
`allowed_egress` or `allowed_ingress` rule is invalid (including an ingress rule
without `subnet`, with `to_ip` outside it, or clashing with another network's),
or if `egress: true` and `allowed_egress` are combined · **500** if the name already exists, the subnet is invalid, or bridge
creation fails.

### `GET /v1/networks` · `GET /v1/networks/{name}`

Lists all networks / detail of one. **Shape of `NetworkResponse`:**

| field        | description                                             |
|--------------|----------------------------------------------------------|
| `name`       | network name                                            |
| `bridge`     | Linux bridge backing it (`mhbr<id>`)                    |
| `subnet`     | the network's CIDR                                      |
| `gateway`    | the host's IP on the bridge (the `.1`, the guests' route) |
| `egress`     | whether it has internet egress                          |
| `allowed_egress` | fine-grained egress rules, if any (omitted if empty) |
| `allowed_ingress` | ingress rules, if any (omitted if empty) |
| `intra`      | whether the network's VMs can see each other           |
| `created_at` | RFC3339 timestamp                                       |

### `PUT /v1/networks/{name}/egress` — update the egress policy live

Replaces the **whole** egress policy (no merge) without touching the connected
VMs. Same fields and validation as on creation: `egress` (bool) and
`allowed_egress` (array), mutually exclusive.

```bash
mhcurl -X PUT http://localhost/v1/networks/pingtest/egress -d '{"allowed_egress":[
  {"ip":"192.168.0.15","protocol":"icmp"},
  {"ip":"192.168.0.15","protocol":"tcp","port":6565}]}'

# remove all egress: empty body
mhcurl -X PUT http://localhost/v1/networks/pingtest/egress -d '{}'
```

Flows opened under the previous policy are cut immediately when hardening (see
`docs/networking.md`, "Hot update").

**200 response** (updated `NetworkResponse`) · **400** if a rule is invalid or
`egress: true` and `allowed_egress` are combined · **404** if the network doesn't
exist.

### `PUT /v1/networks/{name}/ingress` — update the ingress policy live

Replaces the **whole** ingress policy (no merge), same validation as on
creation. An empty or missing list closes every inbound hole.

```bash
mhcurl -X PUT http://localhost/v1/networks/mqtt-60/ingress -d '{"allowed_ingress":[
  {"iface":"wlan0","src_ip":"192.168.50.60","protocol":"tcp","port":1883,"to_ip":"172.16.9.2"}]}'

# close it
mhcurl -X PUT http://localhost/v1/networks/mqtt-60/ingress -d '{"allowed_ingress":[]}'
```

A removed rule cuts its live flows on their next packet.

**200 response** (updated `NetworkResponse`) · **400** if a rule is invalid,
`to_ip` is outside the subnet, or a rule clashes with another network's · **404**
if the network doesn't exist.

### `PUT /v1/networks/{name}/intra` — VM↔VM connectivity live

Enables or disables traffic between the network's VMs without recreating it:
persists the flag and re-applies port isolation to all the network's live TAPs.
Stopped VMs pick it up on boot.

```bash
mhcurl -X PUT http://localhost/v1/networks/pingtest/intra -d '{"intra":true}'
mhcurl -X PUT http://localhost/v1/networks/pingtest/intra -d '{"intra":false}'
```

**200 response** (updated `NetworkResponse`) · **400** invalid body · **404** if
the network doesn't exist · **500** if the flag was saved but some live TAP didn't
converge (the message says which — re-issue the PUT).

### `DELETE /v1/networks/{name}`

```bash
mhcurl -X DELETE http://localhost/v1/networks/lab
```

**204 response** · **404** if it doesn't exist · **409** if it still has connected
VMs (destroy them first, with the endpoint below).

### `DELETE /v1/networks/{name}/vms` — delete all VMs of a network

The typical step before `DELETE /v1/networks/{name}` when it still has VMs.

```bash
mhcurl -X DELETE http://localhost/v1/networks/lab/vms
```

**200 response** (`BulkDeleteResponse`, shape further below) · **404** if the
network doesn't exist.

---

## VMs

### `POST /v1/vms` — create

**Body** (`CreateVMRequest`):

| field         | type   | required | description                                                                 |
|---------------|--------|----------|------------------------------------------------------------------------------|
| `template`    | string | yes      | name of a catalog template                                                  |
| `vcpus`       | int    | no       | overrides the template's `vcpus`                                            |
| `mem_mb`      | int    | no       | overrides the template's `mem_mb`                                           |
| `disk_mb`     | int    | no       | overrides the template's `disk_mb`; only grows (never shrinks)             |
| `network`     | string | no       | segmented network to connect the VM to (see `## Networks`); empty = `default` |
| `no_network`  | bool   | no       | if `true`, no TAP/IP is created — the VM is only reachable over vsock (`/exec`) |
| `volumes`     | array  | no       | volumes to attach at boot (see `## Volumes`); each one `{name, read_only?, guest_path?}` |

```bash
mhcurl -X POST http://localhost/v1/vms -d '{"template":"base-ubuntu"}'
mhcurl -X POST http://localhost/v1/vms -d '{"template":"base-ubuntu","network":"lab"}'
mhcurl -X POST http://localhost/v1/vms -d '{"template":"base-ubuntu","no_network":true}'
# read-only mounted sample + writable output volume
mhcurl -X POST http://localhost/v1/vms -d '{"template":"base-ubuntu","no_network":true,
  "volumes":[{"name":"sample","read_only":true,"guest_path":"/mnt/sample"},
             {"name":"output"}]}'
```

**201 response** (`VMResponse`, see below) · **400** if `template` is missing or
the JSON is invalid · **500** if clone/network/boot fails (the error message
includes which step failed).

Each `POST` clones the template's rootfs from scratch
(`internal/storage.CloneRootfs`, copy-on-write via `cp --reflink=auto`) and grows
it to `disk_mb` with `resize2fs` so the guest has free space (a golden rootfs
ships nearly full; without this, an `apt install` runs out of space). The CoW is
only real on a filesystem with reflink (btrfs / XFS-reflink); on plain ext4 `cp`
falls back to a full copy and each VM takes the whole disk — the daemon warns
about this on startup and `scripts/setup-host.sh` provisions a CoW store. There's
no way today to reboot on top of an already-modified disk from a previous VM; that's
part of the pending work.

---

### `GET /v1/vms` — list

```bash
mhcurl http://localhost/v1/vms
```

**200 response** — an array of `VMResponse`. Intended as the basis of the future
"`docker ps` of microVMs" — today it's a flat list, with no filters or status
columns beyond `state`.

---

### `GET /v1/vms/{id}` — detail

```bash
mhcurl http://localhost/v1/vms/a1b2c3d4
```

**200 response** (`VMResponse`) · **404** if it doesn't exist.

**Shape of `VMResponse`:**

| field         | description                                                                 |
|---------------|------------------------------------------------------------------------------|
| `id`          | short ID (8 hex) of the VM                                                   |
| `template`    | name of the template it was cloned from                                     |
| `state`       | `creating`\|`running`\|`paused`\|`stopped`\|`failed` (`running` and `stopped` are used) |
| `pid`         | PID of the (jailed) Firecracker process                                     |
| `vcpus` / `mem_mb` / `disk_mb` | shape promised to the VM at creation                       |
| `uptime_seconds` | `running` only: seconds since the Firecracker process started (boot/restore, not `created_at`) |
| `mem_rss_mb`  | `running` only: the process's REAL resident RAM — what the VM costs the host right now (guest RAM is paged in on demand: usually well below `mem_mb`) |
| `cpu_seconds` | `running` only: the process's accumulated CPU; for a usage rate, difference it between two polls |
| `network`     | name of the segmented network it's connected to (empty if `no_network`)     |
| `guest_ip`    | the guest's IP in its network's subnet (empty if `no_network`)              |
| `tap_device`  | the TAP's name, enslaved to the network's bridge (empty if `no_network`)    |
| `rootfs_path` | the VM's disk (rootfs clone) on the host                                    |
| `log_path`    | the file with this VM's serial console + Jailer/Firecracker logs            |
| `created_at`  | RFC3339 timestamp                                                            |

Note: `VMResponse` doesn't currently include `socket_path` or `vsock_path` (they
exist in the internal `types.VM` type but aren't serialized) — pending a decision
on whether to expose them.

---

### `DELETE /v1/vms/{id}` — destroy

```bash
mhcurl -X DELETE http://localhost/v1/vms/a1b2c3d4
```

**204 response** · **404** if it doesn't exist or if stop/cleanup fails (the error
detail is in the body).

Stops the machine (`firecracker.Stop`: ACPI graceful + SIGTERM backup, or a signal
by PID if it's a VM adopted after a restart), deletes the TAP, frees the IP in its
network, deletes the rootfs clone + `.log`, the Jailer chroot directory, and the
persisted record. Cleanup accumulates errors (`errors.Join`): a failure in one
step doesn't skip the others.

**Destroy (`DELETE`) vs. power off (`stop`)**: `DELETE` deletes *everything*,
including the microVM's ext4 (the disk). If you only want to free CPU/RAM and keep
the disk, use `stop` (below).

---

### `POST /v1/vms/{id}/stop` — power off (poweroff, keeps the disk)

```bash
mhcurl -X POST http://localhost/v1/vms/a1b2c3d4/stop
```

**200 response** with the `VMResponse` (now `state: "stopped"`, `pid` omitted) ·
**404** if it doesn't exist · **409** if it was already stopped.

Powers off the Firecracker process (frees CPU/RAM) and releases the TAP and the
Jailer chroot directory, but **keeps the rootfs clone** (the disk, with everything
the guest wrote) and **keeps the IP reserved** in its network. It survives a daemon
restart: `Reconcile` doesn't sweep it, it leaves it stopped and re-reserves its IP.
You can't `exec` on a stopped VM (**409**).

### `POST /v1/vms/{id}/start` — start a stopped VM

```bash
mhcurl -X POST http://localhost/v1/vms/a1b2c3d4/start
```

**200 response** with the `VMResponse` (`state: "running"`, new `pid`) · **404** if
it doesn't exist · **409** if it isn't stopped.

Recreates the TAP (which was released on stop) and relaunches Firecracker on the
**same** ext4 and the **same** IP it had. The network's bridge is still up (stop
doesn't touch it), so it cold-boots with the disk and addressing intact.

---

### `DELETE /v1/vms` — delete all VMs

Resets the environment (useful between test batches) without going one by one.

```bash
mhcurl -X DELETE http://localhost/v1/vms
```

**200 response** always — destroying many independent VMs isn't all-or-nothing; the
result is in the body, not the HTTP code.

**Shape of `BulkDeleteResponse`** (also used by `DELETE /v1/networks/{name}/vms`):

| field     | description                                                        |
|-----------|----------------------------------------------------------------------|
| `deleted` | array of IDs destroyed successfully                                 |
| `failed`  | object `{id: error message}` — only present if something failed     |

```json
{"deleted": ["a1b2c3d4", "e5f6a7b8"], "failed": {"c9d0e1f2": "vm not found"}}
```

---

## Snapshots and forking

A snapshot freezes a **running** VM as a restorable point: guest memory + device
state (Firecracker's vmstate/mem) + a copy-on-write clone of its disk, all captured
at the same instant (the VM pauses <1s and resumes on its own). The snapshot is an
independent entity: **it survives its source VM being stopped or destroyed** —
that's the point: "detonate and go back to clean" requires the clean state to
outlive whatever happens afterward.

Two ways to return to a snapshot:

- **In-place restore** (`POST /v1/vms/{id}/restore`): rewinds THAT VM — same ID,
  same IP, same TAP; only memory and disk go back. The "reset to clean between
  samples" primitive.
- **Fork** (`POST /v1/snapshots/{id}/fork`): creates a **new** VM from the snapshot
  — the guest wakes up mid-execution exactly where it was frozen.

And a shortcut that doesn't require managing snapshots:

- **Direct fork** (`POST /v1/vms/{id}/fork`): forks a **running** VM in a single
  call — the daemon takes an ephemeral snapshot, forks from it, and deletes it. For
  "give me a copy of this machine as it is right now".

**The network identity is frozen in memory.** The restored guest thinks it has the
IP/MAC from the moment of the snapshot, and that can't be changed on restore. Hence
the two fork modes:

| mode | what it does | when |
|---|---|---|
| normal (default) | the fork joins the origin network with the snapshot's IP; **409** if that IP is taken (e.g. the original VM is still alive) | recover a known state as a full VM |
| `quarantine: true` | a TAP is created but enslaved to **nothing**: the guest thinks it has a network but every packet dies at the host; only reachable over vsock (`/exec`) | fork the point of infection and examine it without letting it talk to anyone; allows **N simultaneous forks** of the same snapshot |

### `POST /v1/vms/{id}/snapshot` — create a snapshot

**Body** (optional): `{"name": "clean"}` — a free label.

```bash
mhcurl -X POST http://localhost/v1/vms/a1b2c3d4/snapshot -d '{"name":"clean"}'
```

**201 response** (`SnapshotResponse`) · **404** if the VM doesn't exist · **409**
if it isn't running (you can only snapshot a `running` VM).

**Shape of `SnapshotResponse`:**

| field | description |
|---|---|
| `id` | short ID of the snapshot |
| `name` | optional label |
| `source_vm` | the VM it was taken from |
| `template` | the source VM's template |
| `vcpus` / `mem_mb` / `disk_mb` | shape of the frozen machine (fixed: restore comes back exactly like this) |
| `network` / `guest_ip` | network identity frozen in the guest's memory |
| `created_at` | RFC3339 timestamp |

### `GET /v1/snapshots` · `GET /v1/snapshots/{id}` · `DELETE /v1/snapshots/{id}`

List, detail, and delete. Deleting a snapshot is safe even if there are VMs
restored from it running (they have their own copies/hardlinks). **204** on delete
· **404** if it doesn't exist.

### `POST /v1/snapshots/{id}/fork` — fork

**Body** (`ForkVMRequest`, optional): `{"quarantine": true}`.

```bash
# Normal fork: requires the snapshot's IP free in its origin network
mhcurl -X POST http://localhost/v1/snapshots/f00dcafe/fork

# Quarantine fork: no real network, vsock only
mhcurl -X POST http://localhost/v1/snapshots/f00dcafe/fork -d '{"quarantine":true}'
```

**201 response** (`VMResponse`; forks carry `restored_from` and, where applicable,
`quarantine: true`; in quarantine `guest_ip` is the IP the guest *thinks* it has,
not a real reservation) · **404** if the snapshot doesn't exist · **409** if the
snapshot's IP is taken in the origin network (destroy the VM that has it or use
`quarantine`).

**Version requirement for simultaneous forks**: `network_overrides` (remapping the
frozen NIC to another TAP) exists from **Firecracker v1.12.0**. With an earlier FC
(the daemon detects it on its own), the fork reuses the snapshot's original TAP
name — it works if the source VM is destroyed or stopped, but **with the original
(or another fork) running, any fork returns 409**, including `quarantine`,
indicating you need to upgrade (`scripts/install-fc.sh`). When upgrading FC,
existing snapshots must be recreated (their format is tied to the version).

### `POST /v1/vms/{id}/fork` — direct fork of a running VM

**Body** (`ForkVMRequest`, optional): `{"quarantine": true}` — the same as the fork
from a snapshot.

```bash
# Quarantine copy of a live VM, in one call
mhcurl -X POST http://localhost/v1/vms/a1b2c3d4/fork -d '{"quarantine":true}'
```

Equivalent to snapshot → fork → delete the snapshot, without the ephemeral snapshot
being registered. The source VM only pauses <1s (same as snapshotting) and keeps
running. If you want to keep the frozen point for later restores, use the explicit
flow (`/snapshots` + fork).

**Network semantics**: those of fork, with one practical consequence — the source
VM is still alive occupying its IP, so a direct fork **without** `quarantine`
always returns 409 on a VM with a network (the frozen IP is in use by definition).
This endpoint's natural mode is `quarantine: true` (or `no_network` VMs).

**201 response** (`VMResponse`, like a normal fork) · **404** nonexistent VM ·
**409** VM not `running`, or the corresponding network/TAP conflict (on FC < 1.12
the original TAP is always in use by the source VM itself, so this endpoint in
practice requires **Firecracker ≥ 1.12**).

### `POST /v1/vms/{id}/restore` — rewind in-place

**Body** (`RestoreVMRequest`): `{"snapshot": "f00dcafe"}`. Only accepts snapshots
taken **from that same VM** (to restore another VM's snapshot there's fork, which
handles identity collisions honestly).

```bash
mhcurl -X POST http://localhost/v1/vms/a1b2c3d4/restore -d '{"snapshot":"f00dcafe"}'
```

Works on a `running` VM (it's stopped first) or a `stopped` one. **200 response**
(`VMResponse`, `state: "running"`) · **404** nonexistent VM or snapshot · **409** if
the snapshot is from another VM or the VM is in an incompatible state.

**Note (guest clock)**: after any restore the guest's clock stays at the snapshot's
time; for analysis where the timestamp matters, resync via `/exec` (e.g. `date -s`
or chrony). It's Firecracker's documented behavior.

---

### `POST /v1/vms/{id}/exec` — run a command (vsock)

**Body** (`ExecRequest`):

| field | type   | required | description                          |
|-------|--------|----------|----------------------------------------|
| `cmd` | string | yes      | command to run with `sh -c` in the guest |

```bash
mhcurl -X POST http://localhost/v1/vms/a1b2c3d4/exec -d '{"cmd":"whoami && uname -a"}'
```

**200 response** (`ExecResponse`):

```json
{"output": "root\nLinux ubuntu-fc-uvm 6.1.102 ...\n", "exit_code": 0}
```

`output` is stdout+stderr combined. It works with or without a network
(`no_network` doesn't affect this endpoint) — it requires the template to have been
prepared with `scripts/prepare-image.sh` (which installs the `socat`+vsock listener
in the golden rootfs). If the template isn't prepared, or if you cloned it before
preparing it, it returns **500** with an error indicating the agent's exit marker is
missing.

**400** if `cmd` is missing · **404**/**500** if the VM doesn't exist or the vsock
doesn't respond.

---

## Volumes

Persistent ext4 disks that live apart from the VMs and survive their destruction —
the data plane: a read-only mounted sample, or a writable disk that collects
artifacts and is read afterward. Full detail (vsock vs `debugfs` channel, streaming,
security) in `docs/volumes.md`.

**Security rule**: the host **never mounts** the guest's filesystem; all host-side
I/O goes through `debugfs` (userspace, no `mount`).

### `POST /v1/volumes` — create

**Body** (`CreateVolumeRequest`):

| field     | type   | required | description                          |
|-----------|--------|----------|----------------------------------------|
| `name`    | string | yes      | unique volume name                    |
| `size_mb` | int    | yes      | ext4 size in MiB (fixed at creation)  |

```bash
mhcurl -X POST http://localhost/v1/volumes -d '{"name":"sample","size_mb":64}'
mhcurl -X POST http://localhost/v1/volumes -d '{"name":"dataset","size_mb":8192}'
```

**201 response** (`VolumeResponse`) · **400** if `name` is missing / `size_mb` ≤ 0 ·
**409** if a volume with that name already exists.

### `GET /v1/volumes` · `GET /v1/volumes/{id}`

List all / detail of one. **Shape of `VolumeResponse`:**

| field         | description                                             |
|---------------|----------------------------------------------------------|
| `id`          | the volume's id                                         |
| `name`        | name                                                    |
| `size_mb`     | size in MiB                                             |
| `attached_to` | id of the VM that has it attached, or absent if free    |
| `created_at`  | RFC3339 timestamp                                       |

### `DELETE /v1/volumes/{id}`

```bash
mhcurl -X DELETE http://localhost/v1/volumes/VOLID
```

**204 response** · **404** if it doesn't exist · **409** if it's attached to a VM
(destroy that VM first — the volume survives its destruction).

### Attach a volume to a VM

**There's no "attach" endpoint**: Firecracker doesn't allow plugging a disk into an
already-booted VM (no hot-plug). A volume is attached **when creating the VM**, in
the `volumes[]` field of `POST /v1/vms`. Each element:

| field        | type   | required | description                                                        |
|--------------|--------|----------|---------------------------------------------------------------------|
| `name`       | string | yes      | name of an existing volume (created with `POST /v1/volumes`)        |
| `read_only`  | bool   | no       | attach as a read-only device (a sample the guest must not alter)    |
| `guest_path` | string | no       | where to mount it inside the guest; defaults to `/vol/<name>`       |

After booting, the daemon mounts each volume at its `guest_path` over vsock (with
`-o ro` if `read_only`). Firecracker exposes them as `/dev/vdb`, `/dev/vdc`… in the
array's order.

```bash
# 1) create the volume
mhcurl -X POST http://localhost/v1/volumes -d '{"name":"output","size_mb":512}'

# 2) (optional) pre-fill it offline without booting anything
mhcurl -X PUT "http://localhost/v1/volumes/VOLID/files?path=/sample.bin" --data-binary @sample.bin

# 3) create the VM with the volume attached
mhcurl -X POST http://localhost/v1/vms -d '{
  "template":"base-ubuntu","no_network":true,
  "volumes":[{"name":"output","guest_path":"/mnt/out"}]}'
```

A volume is attached **to at most one VM at a time** — attaching it to a second one
while the first has it returns **409**. When the VM is destroyed the volume is
detached (becomes free) but **not deleted**. If you need to write to a volume
already attached to a live VM, do it through the VM's channel
(`PUT /v1/vms/{id}/files`, below), not through the volume's offline endpoint.

### `PUT`/`GET /v1/volumes/{id}/files?path=…` — put/get data offline

Writes or reads a file inside the volume **without booting any VM**, with `debugfs`
(no mounting). It's the way to **prepare** a volume: you fill it and then attach it
when creating the VM. Streamed — constant memory whatever the size (a several-GB
dataset doesn't load the host's RAM).

Only valid with the volume **free** (`attached_to` empty): writing underneath a
guest that has it mounted would corrupt it.

```bash
# put a sample into the volume (the body is the raw file)
mhcurl -X PUT "http://localhost/v1/volumes/VOLID/files?path=/sample.bin" --data-binary @sample.bin
# get an artifact
mhcurl "http://localhost/v1/volumes/VOLID/files?path=/output/result.txt" -o result.txt
```

**204 response** (PUT) / **200** with the file (GET) · **400** if `path` is missing
or invalid (see [path rules](#path-rules)) · **404** if the volume or the file
doesn't exist · **409** if the volume is attached to a VM.

> **Large data**: Firecracker has no disk hot-plug, so you don't attach a volume to
> an already-booted VM. The model is **prepare and attach**: create the volume of the
> size you need, fill it here (offline, streamed), and create the VM with it in
> `volumes[]`.

---

## VM files

### `PUT`/`GET /v1/vms/{id}/files?path=…` — upload/download a file

Transfers a file to/from a VM. **Transparent to state**: if the VM is **running** it
goes over vsock (works even with no network); if it's **stopped** it reads/writes
its disk offline with `debugfs` (the *post-mortem* path: pull evidence without
booting). Streamed end-to-end — constant memory at any size.

```bash
# upload (automatic Content-Length with --data-binary @file)
mhcurl -X PUT "http://localhost/v1/vms/VMID/files?path=/root/input.bin" --data-binary @input.bin
# download
mhcurl "http://localhost/v1/vms/VMID/files?path=/root/output.bin" -o output.bin
```

**204 response** (PUT) / **200** with the file (GET) · **400** if `path` is missing
or invalid (see below) · **404** if the VM doesn't exist · **409** if the VM is
neither `running` nor `stopped`.

Watch the root disk's size (`disk_mb`) if you upload a large file there; for large
data use a sized **volume**, not the rootfs.

### Path rules

The same for every file endpoint (VM and volume, running or stopped), so a path
never works on one channel and fails on the other:

- absolute (`/root/x.bin`), not the root itself, no `..` components;
- **no control characters** (newline, CR, tab, NUL…) **and no `"`**. Spaces and
  any other character are fine (`/out dir/my file.txt`).

The control-character rule is a security boundary. The offline channel drives
`debugfs` with a script of one command per line, so a newline in a path would
add a command of its own, such as `dump` (write a host file) or `write` (read
one), running as the jailer uid, which owns every VM's disk and every volume.
Paths often come from the guest itself (listing a sample's output and
downloading each file), so it is the guest that would choose them.
`storage.ValidateGuestPath` refuses them at the API, again in the manager, and
every `debugfs` argument is also double-quoted and checked when the script is
built.

---

## Observability

Two read-only singletons, meant for two different consumers:

- **`GET /v1/health`** — for a **monitor** (systemd, uptime-checker, load balancer):
  cheap, responds via HTTP code (**200** `ok` / **503** `degraded`), no need to
  parse the body.
- **`GET /v1/system`** — for a **dashboard/operator**: the full report in one call
  (always **200**; health is inside). Everything is computed at request time from
  `/proc`, `statfs`, and the manager's in-memory state — no background collectors or
  history (history is the client's: poll and difference).

### `GET /v1/health` — health probe

```bash
mhcurl http://localhost/v1/health
```

**200/503 response** (`HealthResponse`): `{"status": "ok"|"degraded", "checks": [...]}`.
`status` is `degraded` as soon as ONE check fails. Each check carries `name`, `ok`,
and `detail` (the reason on failure; on `disk_space`, the figures always):

| check            | what it verifies                                                          |
|------------------|----------------------------------------------------------------------------|
| `kvm`            | `/dev/kvm` present — without it no VM boots                                |
| `database`       | the state SQLite responds (a real query, not just a ping)                 |
| `store_writable` | a file can be created in the store (where clones/volumes go)              |
| `disk_space`     | store free space ≥ max(5% of the total, 1 GiB) — a full store fails every create/snapshot/upload in worse ways |
| `store_cow`      | the store supports reflink; without CoW each VM is a full copy of the rootfs (the same warning the daemon logs on startup, made probeable) |
| `firecracker`    | the Firecracker binary exists and responds to `--version`                 |
| `egress_policy`  | every stored egress rule is enforceable. A rule naming an interface the daemon does not manage (restarted without the `--managed-iface` it had) is **not applied**, because its hole is only safe inside that interface's deny-both-ways policy. This check lists those rules, so the policy the API reports is never silently different from the one in force |

### `GET /v1/system` — full report

```bash
mhcurl http://localhost/v1/system | jq .
```

**200 response** (`SystemResponse`), five blocks:

- **`status` + `checks`** — the same as `/v1/health`, embedded so a dashboard needs a
  single call.
- **`daemon`** — the process and the "where everything is" map: `pid`, `started_at`,
  `uptime_seconds`, `firecracker_version`, `network_overrides` (FC ≥ 1.12:
  simultaneous forks possible — its absence explains the fork 409s), and `paths` with
  ALL the host paths an operator may need to inspect or back up: `store` (clones +
  console logs at its root), `goldens`, `kernels`, `snapshots`, `volumes`,
  `chroot_base` (Jailer jails), `database`, `catalog`, `firecracker`, `jailer`.
- **`host`** — live resources of the physical host: `hostname`, `kernel`, `cpus`,
  `load1/5/15` (judge them against `cpus`), and `memory{total_mb, used_mb,
  available_mb}` (`used` = total−available: reclaimable cache doesn't count as used;
  `available_mb` is what new VMs can claim).
- **`storage`** — store capacity: `path`, `fs_type`, `cow`,
  `total_mb/used_mb/free_mb` (filesystem statfs) and `breakdown` by category
  (`vm_disks_and_logs`, `goldens`, `kernels`, `snapshots`, `volumes`,
  `jailer_chroots`), each with its path and ALLOCATED size (`du`-style, real blocks —
  sparse files don't fool it). **Note in CoW**: extents shared by reflink are counted
  once per file, so the categories may add up to more than `used_mb` — each figure
  answers "how much would deleting this free at most", not "how much it owns
  exclusively" (`btrfs filesystem du` is the exact reference).
- **`fleet`** — what the daemon manages: `vms{total,running,stopped}`,
  `allocated{vcpus,mem_mb}` (what's PROMISED in aggregate to the `running` VMs —
  overcommit visibility against `host`; real per-VM consumption is in each
  `VMResponse`'s `mem_rss_mb`), `networks`, `snapshots`, `volumes{total,attached}`,
  `templates`.

```json
{
  "status": "ok",
  "checks": [{"name":"kvm","ok":true}, {"name":"disk_space","ok":true,"detail":"18432 MiB free of 20480 MiB"}],
  "daemon": {
    "pid": 95974, "started_at": "2026-07-05T12:00:00+02:00", "uptime_seconds": 3600,
    "firecracker_version": "v1.10.1", "network_overrides": false,
    "paths": {
      "store": "/var/lib/microhosted/store",
      "goldens": "/var/lib/microhosted/store/rootfs",
      "kernels": "/var/lib/microhosted/store/kernels",
      "snapshots": "/var/lib/microhosted/store/snapshots",
      "volumes": "/var/lib/microhosted/store/volumes",
      "chroot_base": "/var/lib/microhosted/store/jailer",
      "database": "/home/user/MicroHosted/images/microhosted.db",
      "catalog": "/home/user/MicroHosted/images/catalog.json",
      "firecracker": "/usr/local/bin/firecracker",
      "jailer": "/usr/local/bin/jailer"
    }
  },
  "host": {
    "hostname": "lab", "kernel": "6.17.0-35-generic", "cpus": 8,
    "load1": 0.42, "load5": 0.31, "load15": 0.25,
    "memory": {"total_mb": 31855, "used_mb": 7200, "available_mb": 24655}
  },
  "storage": {
    "path": "/var/lib/microhosted/store", "fs_type": "btrfs", "cow": true,
    "total_mb": 20480, "used_mb": 2048, "free_mb": 18432,
    "breakdown": [
      {"what": "vm_disks_and_logs", "path": "/var/lib/microhosted/store", "size_mb": 610},
      {"what": "goldens", "path": "/var/lib/microhosted/store/rootfs", "size_mb": 300},
      {"what": "snapshots", "path": "/var/lib/microhosted/store/snapshots", "size_mb": 450}
    ]
  },
  "fleet": {
    "vms": {"total": 3, "running": 2, "stopped": 1},
    "allocated": {"vcpus": 2, "mem_mb": 256},
    "networks": 2, "snapshots": 1,
    "volumes": {"total": 2, "attached": 1}, "templates": 1
  }
}
```

Per-VM consumption (real resident RAM, accumulated CPU, uptime) doesn't live here
but in each `VMResponse` of `GET /v1/vms` — see its table above. With that,
`GET /v1/vms` is already the "`docker ps`" of microVMs: each one's state, shape, real
consumption, network, and paths.

---

## What's missing

- Being able to create from an already-used disk (not only from the golden template).
- `GET /v1/vms`: filters/sorting for dashboards (uptime/consumption/paths are already
  in `VMResponse` since the observability work).
- An audit that `DELETE` leaves no orphans on any failure path.
