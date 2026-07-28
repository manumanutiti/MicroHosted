# Volumes and secure data transfer

How MicroHosted moves data in and out of microVMs without exposing the host.
It's the platform's data plane: in the detonation workflow it's what carries the
sample into the isolated VM and what collects the artifacts afterward.

## Guiding principle: the host NEVER mounts a guest filesystem

`mount(2)` of an untrusted ext4 image runs the **host kernel's** ext4 parser
over attacker-controlled bytes — the classic escape surface (a long history of
ext4/journal CVEs). That's why **all** host-side I/O goes through **`debugfs`**
(from `e2fsprogs`), a userspace tool that reads/writes ext4 **without mounting**:
a malicious image, at worst, crashes the `debugfs` process itself, never touches
the host kernel.

And that process **doesn't run as root**: when the daemon runs as root, each
`debugfs` invocation drops to the jailer's uid/gid (`--jailer-uid/-gid`, the same
unprivileged identity the jailed Firecracker runs as, and the owner of every
image in the store). An exploit of the `debugfs` parser triggered by a malicious
image thus lands in a process with no privileges or capabilities — it can scrawl
over store images (which it already could: it *is* the parser that writes them),
but not the host. It's mitigation, not a jail (it shares the host namespaces);
what it removes is the root-shell prize from the ext4 parser's attack surface.
The staging temporaries are chowned to that uid so the degraded process can
read/write them.

There are two data channels, depending on where the VM is:

| Channel | When | How |
|---|---|---|
| **vsock** | VM alive | Streaming over vsock port 52 (same channel as `exec`). Works without a network (`no_network`, quarantine). |
| **debugfs** | VM stopped / volume detached | Offline read/write over the ext4 file, without mounting. |

The file endpoints pick the channel on their own based on the VM's state.

### Constant memory (end-to-end streaming)

Every transfer is **streamed**: neither upload nor download loads the whole file
into RAM, so it makes no difference whether it's 4 KB or 40 GB — the daemon's
memory cost is a fixed buffer. Since `debugfs` doesn't read/write a stream (its
`write`/`dump` verbs take real host files), the offline channel **stages** in a
temporary **on the store** (`<store>/staging/`, real disk), **never in `/tmp`**
(which is usually tmpfs = RAM). Extraction temporaries are deleted from the
directory as soon as they're opened (they stay valid via the open descriptor),
and a half-finished crash is swept on restart.

If you upload via the VM endpoint (`PUT /v1/vms/{id}/files`) without a
`Content-Length` (a *chunked* body) and the VM is alive, the daemon stages on the
store first to learn the size the vsock protocol needs — still constant memory,
just going through disk. `curl --data-binary @file` sends `Content-Length`, so
that case streams directly without staging.

## Volumes

A **volume** is a persistent ext4 disk that lives independently of the VMs
(`internal/storage/volume.go`, under `<store>/volumes/<id>.ext4`). It survives the
VM's `destroy` — it's where data persists. Two canonical uses:

- **Read-only sample**: attached with `read_only:true`; it's a read-only block
  device, so the guest analyzing it can't alter it (the block layer itself rejects
  it, not just a mount option).
- **Writable output volume**: collects artifacts (pcaps, dumps, results) that are
  read after the VM no longer exists.

A volume is attached **to at most one VM at a time** (`AttachedTo`): two guests
writing the same ext4 would corrupt it.

### Large data: prepare, then attach

Firecracker has **no disk hot-plug**: you can't plug a new volume into a VM that
has already booted. The correct model for a large dataset (several GB) is to
**prepare the volume and attach it when creating the VM**:

1. `POST /v1/volumes` with the `size_mb` you need (the volume is a fixed-size
   ext4; give it as many GB as required — it's the place for large data, not the
   VM's root disk).
2. `PUT /v1/volumes/{id}/files?path=…` to fill it **offline via debugfs,
   streamed** (no VM booted, constant memory).
3. `POST /v1/vms` with that volume in `volumes[]` → the VM boots with the data
   already inside, mounted at `guest_path`.

If the VM already exists and already has a volume attached, you write to it **over
vsock, streamed** with `PUT /v1/vms/{id}/files` pointing at a path inside its
`guest_path`. What you can't do (on purpose, because Firecracker doesn't support
it) is attach a volume to a running VM.

### Lifecycle

```
POST   /v1/volumes                 {name, size_mb}   -> create (mkfs.ext4)
GET    /v1/volumes                                   -> list
GET    /v1/volumes/{id}                              -> detail (incl. attached_to)
DELETE /v1/volumes/{id}                              -> delete (409 if attached)
```

### Attach to a VM

In `POST /v1/vms`, the `volumes` field:

```json
{
  "template": "detonation",
  "no_network": true,
  "volumes": [
    {"name": "sample-in", "read_only": true, "guest_path": "/mnt/sample"},
    {"name": "artifacts-out"}
  ]
}
```

After boot, the daemon mounts each volume **over vsock** at `guest_path` (default
`/vol/<name>`), with `-o ro` if it's read-only. Firecracker exposes secondary
disks as `/dev/vdb`, `/dev/vdc`… in list order, which is the order they're mounted
in. If the mount fails, the `Create` fails and rolls back. A volume with no
`guest_path` is left as a raw device for the guest to mount.

When the VM is `destroy`ed, the volumes are **detached** (`AttachedTo` cleared)
but their files are **not deleted**.

> **Snapshots + volumes**: not supported in v1. `snapshot`/`fork`/`restore` on a
> VM with attached volumes return **409**. The snapshot's RAM has the volume
> mounted (page cache, journal); restoring over a volume that changed since then
> corrupts it. Destroy the VM (the volumes persist) and recreate it without them.

## File transfer

### With the VM (alive → vsock, stopped → debugfs)

```
PUT /v1/vms/{id}/files?path=/path/in/guest      body = bytes
GET /v1/vms/{id}/files?path=/path/in/guest      response = bytes
```

Transparent to state: with the VM **running** it goes over vsock (even with no
network); with the VM **stopped** it reads/writes the disk offline with
`debugfs`. This second case is the **post-mortem** path: you stop a detonation VM
and pull files straight from its disk without booting or mounting it. Both are
streamed; watch the size of the VM's root disk (`disk_mb`) if you upload a large
file there — for large data use a sized volume, not the rootfs.

### With a detached volume (offline, debugfs)

```
PUT /v1/volumes/{id}/files?path=/path      body = bytes   (inject a sample before attaching)
GET /v1/volumes/{id}/files?path=/path      response = bytes (extract results after destroy)
```

Only valid with the volume **detached**: writing underneath a guest that has it
mounted corrupts it. If it's attached and the VM alive, use the VM's channel.

## The guest agent

`scripts/prepare-image.sh` installs `microhosted-exec`, which multiplexes three
verbs over the vsock port based on the first line of each connection:

- a bare command (no verb) → `exec` (the historical contract, intact)
- `PUT <path> <len>` + `<len>` bytes → writes the file
- `GET <path>` → responds `OK <len>` + bytes, or `ERR <msg>`

It requires `socat` in the image (already needed for `exec`).
