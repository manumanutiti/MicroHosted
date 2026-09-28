# Quick setup — from clone to your first microVM

Minimal guide to get MicroHosted running on a fresh machine. Everything heavy
(`vmlinux` kernels, `.ext4` rootfs images, keys, database) is **not tracked in
git**: every machine builds it locally with the scripts in this repo. That's
why cloning is lightweight, and why you need to run the steps below.

## Requirements

- Linux x86_64 or aarch64 (ARM64 boards with a **64-bit** OS, Jetson, ARM
  gateways) with kernel 5.10+ and cgroups v2.
- **KVM**: `/dev/kvm` must exist. On a PC, enable VT-x/AMD-V in the BIOS (or
  nested virtualization if you work inside a VM). On ARM64 boards it
  usually ships with the vendor's 64-bit kernel.
- **Go 1.25+** (`go.mod` requires `go 1.25.0`).
- `make`, `git`, `sudo`.

Quick check of all of the above:

```bash
make check
```

## 1. Full installation (one step)

```bash
git clone <repo-url> && cd MicroHosted
make full-install
```

This does, in order: checks (architecture, KVM, Go), host configuration
(cgroups, nftables, btrfs CoW store), installation of `firecracker` +
`jailer` (pinned version `v1.16.1`, validated against this platform),
building the daemon, registration as a systemd service (`enable --now`), and a
health check through the API.

Useful variables: `make full-install ADDR=127.0.0.1:9000` (API address,
no `ADDR` means the API serves on a Unix socket, whose
permissions are its authorization; `SOCKET_GROUP=microhosted` lets that group
drive it without `sudo`) and `FC_VERSION=vX.Y.Z` (changing the Firecracker version is a
conscious decision: snapshots are tied to the version that created them).

It is **idempotent**: re-running it updates the binary/service without touching
live VMs. It must run **on the target machine** (KVM, cgroups, and the store
are local); to ship only the binary to another machine: `make build ARCH=aarch64`.

## 2. Build your first image

The `.ext4` images are not distributed via git — they are built:

```bash
make prepare-image
```

By default it builds the **ultra-minimal Alpine** (`base-alpine`, ~10 MB
real, busybox init without systemd, access via vsock exec) — the one designed
for density at the edge (it decides how many microVMs fit on a host). It ends
with two things built from the same files:

- a **template** `base-alpine` in the catalog
  (`/var/lib/microhosted/catalog.json`, root-owned; `images/catalog.json` in the
  repo is only the seed a fresh install starts from): a name for the two file
  paths, with no digest — a rebuild changes what it boots;
- an **image** `base-alpine:<version>` in the engine's image store: a hashed,
  read-only copy under a tag that never moves. The last lines print its pinned
  reference:

```
  For a plant spec (the orchestrator needs the digest):
    image: base-alpine:20260928-190412@sha256:59ddee22…
```

That line is what goes into an orchestrator spec's `image:`; `mh image ls -q`
prints it again at any time. The version defaults to the build time; name it
with `IMAGE_VERSION=1.0`. A tag names one build forever, so a rebuild needs a
new version (the same version with different bytes is refused). `IMPORT=0`
stops at the template. The difference is explained in
[docs/engine.md](docs/engine.md#template-or-image).

Variants:

```bash
make prepare-image EXTRA_PKGS=python3 IMAGE_NAME=alpine-py IMAGE_VERSION=1.0   # Alpine + apk packages
make prepare-image FLAVOR=ubuntu     # Ubuntu noble (~1 GiB, systemd + SSH): base-ubuntu-noble
make prepare-image FLAVOR=ubuntu-docker  # noble + Docker Engine, dev workstation: dev-ubuntu
make prepare-image ARCH=aarch64      # image for ARM (cross-compile with qemu-user-static)
make prepare-image ARCH=x86_64       # the reverse, from an ARM machine
```

Both flavors support x86_64 and aarch64, native or cross. Watch out with
cross-building: the resulting image is **neither registered in the local catalog
nor imported** (it's for another CPU) — the script leaves the kernel and rootfs with an
architecture suffix so you can copy them to the target machine's store. The
usual approach is to run `make prepare-image` natively on each machine (x86_64
and ARM64).

The project SSH key is generated locally in `images/keys/` (git-ignored; never
shared).

## 3. Your first microVM

`full-install` also installs `mh`, the docker-style CLI. Use `sudo mh …`
until you are in the socket's group (see [docs/api.md](docs/api.md#calling-the-api-and-who-may)).

```bash
mh health                          # daemon health
mh template ls                     # templates (alias: mh images)
mh image ls                        # images: tag, digest, defaults
VM=$(mh run base-alpine)           # create a VM from the template
VM=$(mh run base-alpine:<version>) # …or from the image (a ':' makes it one)
mh ps                              # list
mh exec $VM uname -a               # run a command inside it (vsock)
mh rm $VM
```

Every command is in [docs/cli.md](docs/cli.md) (networks, egress rules,
volumes, snapshots/forks, quarantine). The raw HTTP API is in
[docs/api.md](docs/api.md).

## Day-to-day operation

```bash
make service-logs      # daemon logs (journalctl -f)
make test              # Go tests
make uninstall         # inverse of full-install (DRY_RUN=1 to simulate)
```

## Common problems

- **`/dev/kvm` doesn't exist** — no KVM means no microVMs. PC: enable
  virtualization in the BIOS. ARM64 boards: you need a 64-bit OS
  with KVM in its kernel (`zgrep KVM /proc/config.gz` to check).
- **Permissions on `/dev/kvm`** — `setup-host.sh` adds you to the `kvm` group;
  you may need to re-login or run `newgrp kvm`.
- **`no pinned SHA-256 for …` / `… is not the pinned …`** — every download
  (Firecracker, the guest kernel, the Alpine base) is checked against
  `scripts/checksums.sha256`. The first means the version you asked for (e.g.
  `FC_VERSION=latest` resolving to a new release) is not pinned yet: add it
  there by hand after checking it against its publisher, as the file's header
  describes. The second means the file is not what was pinned — do not work
  around it: delete the cached copy (`images/cache/`, `images/kernels/`) and
  download again; if it still differs, find out why.
  ([docs/threat-model.md](docs/threat-model.md), Layer 11.)
- **The daemon in the foreground** — don't run it in an interactive terminal;
  use the systemd service (see [docs/deploy.md](docs/deploy.md)).
