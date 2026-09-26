# Deployment as a systemd service

The daemon **must not be run in the foreground** in an interactive terminal: a
Ctrl-C sends SIGINT to the terminal's entire process group, which also kills the
child Firecracker processes and takes down the microVMs. As a systemd service
there is no controlling terminal, and the unit is configured so that VMs survive
daemon restarts.

## One-step installation (recommended)

```bash
make full-install       # host (cgroups/nftables/CoW store) + firecracker/jailer
                        # + build + systemd service + health check
make prepare-image      # kernel + ultra-minimal Alpine rootfs (default)
                        # + golden into the store + registration in the catalog
```

With that the system is up from scratch: after `prepare-image` you can already
create the first VM (`POST /v1/vms {"template":"base-alpine"}`). For the classic
Ubuntu image (systemd + SSH): `make prepare-image FLAVOR=ubuntu` (template
`base-ubuntu-noble`). For an isolated development workstation with Docker
Engine inside the guest: `make prepare-image FLAVOR=ubuntu-docker` (template
`dev-ubuntu`: 2 vCPUs, 1 GiB RAM, 8 GiB disk by default). Containers run
inside the microVM, so the VM stays the isolation boundary; the guest uses
the legacy iptables backend because the guest kernel has no nf_tables, and
`docker pull` needs `egress` towards the registries.

It works on **x86_64 and aarch64** (ARM64 boards with a 64-bit kernel, Jetson, ARM
gateways): the architecture is auto-detected and every step respects it
(Firecracker binaries, kernel from the CI bucket, Ubuntu mirror — arm64 lives on
`ports.ubuntu.com`, not `archive.ubuntu.com`). Useful variables:

```bash
make full-install FC_VERSION=v1.16.1 ADDR=127.0.0.1:9000
make prepare-image EXTRA_PKGS=python3 IMAGE_NAME=alpine-py   # Alpine + extra apk
make prepare-image FLAVOR=ubuntu IMAGE_NAME=sensor-base IMAGE_SIZE_MB=512
```

`full-install` must run **on the target machine** (KVM, cgroups, and the store
are local) and is idempotent: re-running it updates the binary/service without
touching live VMs. The Firecracker version is **pinned** in the Makefile
(`v1.16.1`, the one validated on hardware): this way every install is
reproducible. Updating it is a conscious decision
(`make full-install FC_VERSION=vX.Y.Z`) — existing snapshots are tied to the
version that created them and will need to be recreated.

To prepare ARM pieces from an x86 PC (useful before the target board is at hand):

```bash
make build ARCH=aarch64          # cross-compiles only the binary (pure Go, static)
make prepare-image ARCH=aarch64  # arm64 image via qemu-user-static; the files
                                 # are copied to the target's store by hand (they are
                                 # not registered in the local catalog)
```

## Install (step by step, if you prefer fine control)

```bash
make setup-host                      # cgroups, nftables, CoW store
make install-fc                      # firecracker + jailer (same version)
make install-service                 # builds (as your user) and installs the unit
sudo systemctl enable --now microhosted
systemctl status microhosted
```

Different listen address:

```bash
make install-service ADDR=127.0.0.1:9000
```

## Operate

```bash
journalctl -u microhosted -f         # live logs
sudo systemctl restart microhosted   # restarts the daemon WITHOUT killing live VMs
sudo systemctl stop microhosted      # stops the daemon; the VMs keep running
```

After a `restart` or a boot, the log will show `reconcile: adopted running vm
<id>` for each VM that was still alive, or `reconcile: vm <id> died while the
daemon was down (pid N); kept as stopped` for any that had died meanwhile — after
a host reboot, that is every VM. Their disks and IPs survive: `mh start` brings
them back, and the ones created with `--autostart` (or set with
`mh vm update --autostart`) are booted on their own, logged as
`autostart: started vm <id>`. Only a dead VM whose disk is gone too is
`swept` and forgotten.

Startup also cleans what a previous run left when it died mid-operation:
`reconcile: vm <id> was being created when the daemon stopped; undoing it`,
`sweep: killed orphan firecracker ...`, `sweep: removed jail dir ...`,
`network reconcile: removed orphan bridge ...`. While running, a VM that dies on
its own is logged as `monitor: vm <id> died (pid N): <reason>; marked stopped`.
`mh doctor` tells you whether anything is still out of step.

### Admission limits

The daemon refuses a launch (create, fork, start) with a 503 rather than push the
host into its OOM killer. Defaults, overridable on the daemon's command line:

| flag | default | meaning |
|---|---|---|
| `--mem-reserve-mb` | 512 | host memory a launch must leave available (judged on `MemAvailable`, not on the sum of `mem_mb`: guest RAM is lazy) |
| `--max-vms` | 0 (off) | cap on running VMs plus launches in progress |
| `--max-parallel-boots` | 4 | launches running at once; the rest wait |
| `--quota` | none | `CONSUMER=vms:N,mem:MB` (repeatable): cap on what the VMs labelled `managed-by=CONSUMER` may hold at once (running + launching, their `mem_mb`); over it a launch is 429 |
| `--quota-default` | none | `vms:N,mem:MB`: the quota of every labelled consumer without its own `--quota` |
| `--disk-reserve-mb` | 1024 | store space kept free: a launch, snapshot, volume, image import or upload that would leave less is refused (503), and `host.disk_low` is raised while the store is under it |

Every VM's disk and network throughput has a ceiling too. A VM may ask for
less (`io_limits` on create or fork), never more; `0` lifts a limit, and the
daemon logs a warning when one is lifted:

| flag | default | meaning |
|---|---|---|
| `--vm-disk-mib-s` | 100 | throughput of each VM drive (rootfs and every volume), MiB/s; the first 128 MiB after a boot are not throttled |
| `--vm-disk-iops` | 4000 | operations per second of each VM drive |
| `--vm-net-mbit` | 100 | throughput of each VM's network, per direction, Mbit/s |

A lowered ceiling reaches each VM at its next boot (start, restore, fork);
VMs running now keep what they booted with.

### Fault injection

`sudo scripts/fault-test.sh` restarts the daemon with each failpoint armed
(`MICROHOSTED_FAULTS`, see `internal/faults`), fires bursts of creates, forks and
network creates, kills the daemon mid-burst, and requires `mh doctor` to be
clean after every round; an error-mode round also fails if no operation
returned the injected error (a failpoint that never fired proves nothing). It
is disruptive (many restarts, each preceded by `systemctl reset-failed` so the
restart back-off starts from 2 s again; running VMs survive them) —
run it on a test host. Validated 21/21 on test hardware on 2026-09-22. The failpoints are inert unless that variable is
set, and the daemon logs a warning when it is.

## Why the unit is the way it is

- **`KillMode=process`** — systemd only signals the main process (the
  orchestrator), never the child Firecrackers. This is what lets
  `Manager.Reconcile` re-adopt the VMs by PID on restart.
- **`Delegate=yes`** — Jailer creates one cgroup per VM; delegating the cgroup
  subtree to the service avoids clashing with systemd's management.
- **`OOMScoreAdjust=-900`** — under host memory pressure the kernel must kill a
  VM, never the daemon holding all of them. Children inherit the score, so the
  daemon resets each Firecracker to 0 right after launching it; the dead VM then
  shows as `stopped` with `last_exit`.
- **`StartLimitIntervalSec=0` + `RestartSteps=5` / `RestartMaxDelaySec=60`** —
  systemd never gives up restarting the daemon. Its default start limit (5
  starts in 10 s) would leave an unattended gateway with a dead daemon until
  someone ran `systemctl reset-failed`; instead each automatic restart waits
  longer (2 s, ~4, ~8, ~15, ~30, then 60 s). The count goes back to zero only on
  a manual `start`/`restart` or `reset-failed`, not after a long healthy run.
  The VMs keep running while the daemon is down. Needs systemd ≥ 254.
- **`AssertPathExists=/dev/kvm`** — without KVM the service fails clearly at
  startup, not later with an obscure error.
- Runs as **root**: Jailer needs to create chroots/cgroups/tap and open
  `/dev/kvm`.

## Daemon state (database and catalog)

The SQLite database and the template catalog live in `/var/lib/microhosted`
(`microhosted.db`, `catalog.json`), owned by root, never in the source checkout.
They are trusted input for a root daemon: a VM record names the files it
truncates (the console log), clones and deletes (the disk), and a template names
the kernel and disk it boots. Whoever can write either file, or replace it
through a writable directory above it, could aim those operations at any file
on the host.

The daemon enforces this. Running as root, it **refuses to start** if `--db`
or `--catalog`, or any directory leading to them, is owned by anyone but root
or writable by group or others. It keeps the database and its SQLite side files
at mode 0600. `/var/lib/microhosted` itself is 0711: the per-VM identities
traverse it to reach the store, but nobody can list it.

- `images/catalog.json` in the repository is only the **seed**:
  `install-service.sh` copies it to `/var/lib/microhosted/catalog.json` when no
  catalog is installed yet, and never overwrites an existing one.
  `make prepare-image` registers templates in the installed catalog (as root).
- **Migration from older installs**, which kept both files under `images/`:
  `make install-service` stops the daemon (the VMs keep running), moves the
  database to `/var/lib/microhosted/microhosted.db` as root 0600, starts the
  daemon again (it re-adopts the VMs), and leaves the old copy as
  `images/microhosted.db.migrated`. Delete that copy once the daemon is
  confirmed healthy.

## Storage (copy-on-write store)

`scripts/setup-host.sh` provisions the **disk store** before installing the
service. If `/var/lib/microhosted/store` isn't already on a filesystem with
reflink, it mounts a **btrfs loopback** there
(`/var/lib/microhosted/instances.btrfs`, persisted in `/etc/fstab`) and creates
`rootfs/`, `kernels/`, and `jailer/` inside it. This makes VM clones
copy-on-write (each VM costs its deltas, not the whole rootfs) and works on any
Ubuntu host without repartitioning.

Requirement: goldens, kernels, and the Jailer chroot all live in that store — the
daemon derives `--chroot-base` from `--instances-dir` precisely to respect it
(reflink and hardlink don't cross filesystems). At startup, if the store isn't
CoW the daemon warns in the log. The store lives OUTSIDE the repo on purpose:
it's root-owned runtime data (including the jail dirs), and keeping it in the code
tree breaks tools like `go build ./...`. Detail in `docs/layers.md` (L3) and
`docs/architecture.md`.

### Move the store (migration)

`setup-host.sh` is idempotent: if the store's btrfs is already mounted at another
point (e.g. an old install at `<repo>/images/instances`), it detects it with
`losetup -j`/`findmnt`, unmounts it, cleans its `/etc/fstab` line, and remounts it
at `/var/lib/microhosted/store` — without copying data (it's the same subvolume;
the goldens/kernels come along for free).

**Gotcha**: `KillMode=process` makes VMs **survive** a `systemctl stop`, so a
`umount` of the store will give `target is busy` while any live `firecracker`
processes remain. Before migrating you have to shut them down:

```bash
sudo mh rm --all                          # destroys the VMs (with the daemon alive)
sudo systemctl stop microhosted
sudo pkill -9 -f '/firecracker --id'      # kills any orphaned VM that survives
sudo ./scripts/setup-host.sh              # remounts the store at the new path
make install-service && sudo systemctl start microhosted
```

## Uninstall

```bash
make uninstall-service               # stops and removes the service (doesn't touch the VMs)
```
