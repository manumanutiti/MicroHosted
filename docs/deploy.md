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
`base-ubuntu-noble`).

It works on **x86_64 and aarch64** (64-bit Raspberry Pi 4/5, Jetson, ARM
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

To prepare ARM pieces from an x86 PC (useful before you have the Pi at hand):

```bash
make build ARCH=aarch64          # cross-compiles only the binary (pure Go, static)
make prepare-image ARCH=aarch64  # arm64 image via qemu-user-static; the files
                                 # are copied to the Pi's store by hand (they are
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
<id>` for each VM that was still alive, or `reconcile: swept dead vm <id>` for any
that had died while the daemon was stopped.

## Why the unit is the way it is

- **`KillMode=process`** — systemd only signals the main process (the
  orchestrator), never the child Firecrackers. This is what lets
  `Manager.Reconcile` re-adopt the VMs by PID on restart.
- **`Delegate=yes`** — Jailer creates one cgroup per VM; delegating the cgroup
  subtree to the service avoids clashing with systemd's management.
- **`AssertPathExists=/dev/kvm`** — without KVM the service fails clearly at
  startup, not later with an obscure error.
- Runs as **root**: Jailer needs to create chroots/cgroups/tap and open
  `/dev/kvm`.

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
