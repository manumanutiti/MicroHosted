# Quick setup — from clone to your first project

Two commands: install, then `mh up` in an example. Everything heavy (kernels,
images, the database) is **not tracked in git**: every machine downloads or
builds its own — pinned and checked — the first time it needs it.

```bash
git clone <repo-url> && cd MicroHosted
make full-install                         # host, Firecracker, daemon, mh

cd orchestrator/examples/hello
mh up                                     # builds its image the first time, then runs it
```

## Requirements

- Linux x86_64 or aarch64 (ARM64 boards with a **64-bit** OS, Jetson, ARM
  gateways) with kernel 5.10+ and cgroups v2.
- **KVM**: `/dev/kvm` must exist. On a PC, enable VT-x/AMD-V in the BIOS (or
  nested virtualization if you work inside a VM). On ARM64 boards it
  usually ships with the vendor's 64-bit kernel.
- **Go 1.21+**: the build needs 1.25 (`go.mod`), and any Go from 1.21 fetches
  it by itself. `sudo snap install go --classic` or <https://go.dev/dl/>.
- `make`, `git`, `sudo`. The rest (nftables, iproute2, curl, e2fsprogs,
  debootstrap, ubuntu-keyring, btrfs-progs) `make full-install` installs with
  apt; on another distribution, install them first.

Quick check of all of the above:

```bash
make check
```

## 1. Install (one step)

```bash
git clone <repo-url> && cd MicroHosted
make full-install
```

In order: checks (architecture, KVM, Go), host packages and configuration
(cgroups, nftables, a btrfs copy-on-write store), `firecracker` + `jailer`
(pinned `v1.16.1`, checked against its hash), the three programs —
`microhosted`, `mh`, `mh-orchestrator` — and the daemon as a systemd service,
then a health check through the API. It asks for `sudo` once, at the start.

**Who may use it.** The API is a Unix socket whose permissions are its
authorization. A first install gives it the group `microhosted` and adds you
to it, as Docker does with `docker`: `mh` works without `sudo`. Membership is
root-equivalent on this host (a member can exec as root in any VM and set any
network's policy), so add only people you would give root. The group applies
from your next login; in the terminal you installed from:

```bash
newgrp microhosted
mh health
```

Variables: `SOCKET_GROUP=none` keeps the socket root-only (then `sudo mh …`),
`SOCKET_GROUP=NAME` uses a group of your own, `ADDR=127.0.0.1:9000` serves the
API on a TCP port instead — with no authentication in front of it — and
`FC_VERSION=vX.Y.Z` changes Firecracker (a conscious decision: snapshots are
tied to the version that created them).

It is **idempotent**: re-running it updates the binaries and the service
without touching live VMs, and keeps the socket's group as it is. It must run
**on the target machine** (KVM, cgroups and the store are local); to ship only
the binaries elsewhere: `make build ARCH=aarch64`.

## 2. Your first project

```bash
cd orchestrator/examples/hello
mh up              # Ctrl-C to stop; -d to run it in the background
```

The first `mh up` builds the project's image from its `build.yml`: it
downloads the pinned kernel and the Alpine base, checks both, and asks for
`sudo` (a build runs as root). Later runs reuse it — an unchanged build builds
nothing. Then, in another terminal:

```bash
mh status          # functions, VMs, health
mh ps              # the VMs themselves
mh down            # remove everything the project created
```

[orchestrator/examples/](orchestrator/examples/README.md) has the rest — a
website, a three-tier stack, CI runners, labs — each a directory with its
`microse.yml`. Ubuntu-based ones take a few minutes to build the first time.

## 3. Single VMs, by hand

`mh run` boots one VM from an image or a catalog template, as `docker run`:

```bash
mh image ls                        # images: tag, digest, defaults
VM=$(mh run hello:<version>)       # from an image (a ':' makes it one)
mh exec $VM uname -a               # run a command inside it (vsock)
mh rm $VM
```

**Catalog templates** (`mh run base-alpine`, no `:`) are built separately,
with `make prepare-image` — a kernel and rootfs registered by name, with no
digest, for quick experiments:

```bash
make prepare-image                                   # base-alpine: ~10 MB, busybox, vsock exec
make prepare-image EXTRA_PKGS=python3 IMAGE_NAME=alpine-py
make prepare-image FLAVOR=ubuntu                     # base-ubuntu-noble: systemd + SSH
make prepare-image FLAVOR=ubuntu-docker              # dev-ubuntu: noble + Docker Engine
make prepare-image ARCH=aarch64                      # for another CPU (qemu-user-static); copy it there
```

It also imports the result as an image and prints its pinned reference. The
difference between the two is in
[docs/engine.md](docs/engine.md#template-or-image). Every command is in
[docs/cli.md](docs/cli.md); the raw HTTP API in [docs/api.md](docs/api.md).

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
- **`permission denied` on `/run/microhosted.sock`** — your session is not in
  the socket's group yet: `newgrp microhosted` (or log in again). `mh` says
  which case it is: not in the group, or a root-only socket (`sudo mh …`).
- **`mh up … mh-orchestrator, which is not installed`** — an install from
  before it was built with the rest: `make full-install` again (or
  `make install-cli`).
- **`base ubuntu:… is built with debootstrap, which this host lacks`** —
  `sudo apt-get install debootstrap ubuntu-keyring`, or re-run
  `make full-install`, which installs them.
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
