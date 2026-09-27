# Development environments

Status: **exploratory**. Docker Engine inside a microVM and a persistent
Docker data volume are validated on ARM64; turning them into a finished
feature is the pending work listed at the end.

A microVM makes a good disposable workstation. It boots in seconds, runs a
full Ubuntu with systemd and Docker, and anything done inside it (packages,
builds, containers, third-party code) stays behind the VM boundary: its own
kernel, the jailer, a per-VM uid, seccomp and a per-TAP anti-spoofing filter.
Running Docker as root *inside* the guest does not weaken host isolation; the
isolation boundary stays the microVM.

## The `dev-ubuntu` template

```bash
make prepare-image FLAVOR=ubuntu-docker
```

This builds the template `dev-ubuntu`: Ubuntu noble (debootstrap) plus
`docker.io`, `containerd`, `runc`, `iptables`, `git`, `curl` and
`ca-certificates`. Defaults are sized for a workstation rather than a service:
2 vCPUs, 1 GiB RAM, a 2 GiB golden image grown to 8 GiB per clone
(`VCPUS`, `MEM_MB`, `SIZE_MB`, `DISK_MB` override them).

Implementation (`scripts/build-rootfs.sh`, `WITH_DOCKER=1`):

- debootstrap runs with `--components=main,universe` (`docker.io` lives in
  universe);
- packages are installed in the chroot with `/proc`, `/sys` and `/dev` mounted
  and a temporary `policy-rc.d` so no daemon starts on the build host;
- `iptables`/`ip6tables` are switched to the **legacy** backend: the guest
  kernel (Firecracker CI `vmlinux-6.1.102`) ships the xtables modules but not
  `nf_tables`, so the default `iptables-nft` would fail with
  `Failed to initialize nft: Protocol not supported` and dockerd could not set
  up its bridge NAT;
- `containerd.service` and `docker.service` are enabled.

Validated with the guest kernel as is:

| Check | Result |
|---|---|
| Storage driver | `overlay2` on extfs, `d_type` supported |
| Cgroups | v2, systemd driver |
| Security options | seccomp (builtin profile), cgroupns |
| `docker run hello-world` | OK (arm64v8 image) |
| `docker run -p 80:80 nginx` + `curl localhost` | OK |

## Using it

```bash
mh network create devnet --internet eth0      # docker pull needs egress
mh vm create dev-ubuntu --name dev1 --net devnet
ssh -i images/keys/microhosted_ed25519 root@<vm-ip>
```

- **Access.** SSH is key-only: `prepare-image.sh` installs the project key
  (`images/keys/microhosted_ed25519`) into `/root/.ssh/authorized_keys`. The
  root password is empty for the serial console only; sshd rejects empty
  passwords. `mh exec` works over vsock with no network or key.
- **Egress.** Pulling images needs a network with full egress
  (`--internet IFACE`). Registries sit behind CDNs, so `allowed_egress` rules
  are impractical. Once images are pulled, the VM can be moved to a network
  without egress so untrusted dependencies cannot exfiltrate anything.
- **Viewing a service.** Ports published with `docker run -p` are reachable on
  the VM's address from the host (`curl http://<vm-ip>`), never from the LAN
  by default. Ingress rules only apply to managed interfaces
  (`--managed-iface`), so for a developer's browser the intended path is an
  SSH tunnel through the host, which opens nothing in the firewall:

  ```bash
  ssh -L 8080:<vm-ip>:80 user@<host>     # then http://localhost:8080
  ```

### Troubleshooting: DNS times out but HTTPS works

If `docker pull` fails with `Client.Timeout exceeded` while
`curl https://1.1.1.1` succeeds from the guest, look for host-level rules that
rewrite port 53 **without an interface match**, e.g. a leftover

```
-A PREROUTING -p udp --dport 53 -j REDIRECT --to-ports 5353
```

in `iptables -t nat -S PREROUTING`. Such a rule captures every routed DNS
query, including the VMs' (they enter through the bridge), and redirects it to
a host port. It is not a MicroHosted rule: MicroHosted's full-egress policy
does not match on ports. Remove it (and from `/etc/iptables/rules.v4` if it is
persisted) or restrict it with `-i <iface>`.

## Persisting Docker data on a volume

Goal: images, layers and container state survive `vm rm`, `replace` or a
template rebuild, so the VM becomes truly disposable and only the volume is
kept.

### The ordering problem

The daemon mounts volumes **over vsock after boot**
(`internal/vm/manager.go`, `mountVolumes`), at `guest_path` (default
`/vol/<name>`). By then `dockerd` is already running on the rootfs, so
mounting a volume over `/var/lib/docker` at that point would pull the
directory out from under a live daemon.

### Image-only solution (validated)

The guest mounts the volume itself, early, from `fstab`, and Docker waits for
that mount:

```
# /etc/fstab
/dev/vdb /var/lib/docker ext4 defaults,nofail,x-systemd.device-timeout=5s 0 2
```

```ini
# /etc/systemd/system/docker.service.d/10-volume.conf
[Unit]
Wants=var-lib-docker.mount
After=var-lib-docker.mount
```

- The daemon later mounts the same device at `/vol/<name>` too. Mounting one
  ext4 twice in the same kernel is a second view of the same superblock, not
  a second filesystem instance, so it is safe.
- `nofail` + a 5 s device timeout + `Wants=` (not `Requires=`): a VM created
  **without** a volume boots normally and Docker falls back to the rootfs.
- Convention: the Docker volume must be the **first** attached volume
  (`/dev/vdb`). Firecracker exposes secondary drives in attach order.

Validation:

```bash
mh volume create dockerdata --size 8G
mh vm create dev-ubuntu --name dev2 --net devnet -v dockerdata:/vol/docker
# inside: docker pull nginx → layers land on the volume (~187 MB)
mh vm rm dev2
mh vm create dev-ubuntu --name dev3 --net devnet -v dockerdata:/vol/docker
# with the fstab + drop-in above: nginx:latest is present,
# docker run --pull never nginx works, nothing is re-downloaded
```

Boot order observed: `var-lib-docker.mount` active at 0.94 s,
`dockerd` started at 1.12 s (monotonic).

## Pending work

In priority order.

### A. Bake the volume support into the template

Add the `fstab` line and the `docker.service` drop-in to
`scripts/build-rootfs.sh` under `WITH_DOCKER=1`, then rebuild `dev-ubuntu`.
Today it was only applied by hand inside one VM. Document the "first volume
is Docker's" convention in `docs/volumes.md`.

### B. Graceful stop on ARM64 — done (2026-09-27)

Implemented as proposed below, for both architectures: `stop` asks the guest
agent for `sync` + `reboot` over vsock and waits up to 10 s
(`internal/vm/shutdown.go`). It turned out x86_64 needed it too: the Firecracker
CI kernels have no i8042 driver, so `SendCtrlAltDel` was never delivered there
either and every stop was a hard kill after 5 s. Destroy, restore and
`replace --old destroy` kill at once.

`Machine.Stop` (`internal/firecracker/machine.go`) sends `SendCtrlAltDel`,
waits 5 s, then kills. Firecracker implements `SendCtrlAltDel` **only on
x86_64**; on ARM64 the call fails, the error is discarded (`_ =`), and every
`mh vm stop` is a hard kill. Unsynced writes are lost: during validation a
file appended without `sync` came back after `stop`/`start` with its new size
but its new bytes zeroed (78 NUL bytes in `/etc/fstab`). With Docker on a
volume, a stop in the middle of a `docker pull` risks corrupting layers.

Proposal: on ARM64 (or whenever `SendCtrlAltDel` fails), ask the guest over
vsock for `sync` and `poweroff`, wait for the process to exit within the
existing timeout, and only then kill. This changes how the guest is powered
off, not any isolation property. Consider also a longer timeout for systemd
guests, which take longer than 5 s to shut down cleanly.

### C. Multi-line `exec`

The guest agent (`microhosted-exec`, installed by `prepare-image.sh`) reads
only the **first line** of the command (`IFS= read -r line`); anything after
the first newline is silently dropped, so a `sync` on a second line never
runs. Either reject commands containing a newline with a clear error (in the
CLI and the API) or change the framing so the full command is transmitted.

### D. Raw volume attachment

`types.VolumeMount.GuestPath` documents "empty means the volume is attached
as a raw block device and the guest mounts it itself", and
`docs/volumes.md` says the same, but `attachVolumes` always defaults it to
`/vol/<name>`, so that mode does not exist. Either implement it (a raw attach
would be the clean way to hand the Docker volume to the guest's `fstab`) or
fix the comment and the doc.

## Ideas for later

- **Checkpoints.** Snapshot a configured environment before a risky change and
  restore it in seconds. Forks give one identical environment per branch or
  experiment (the snapshot fork/replace path has an open ARM64 bug; validate
  on x86_64 first).
- **Code on a second volume** (`/vol/src`), separate from Docker data, so the
  workspace outlives both the VM and the Docker cache.
- **Per-stack templates** (`dev-node`, `dev-python`, …) built the same way,
  so a new environment starts with its toolchain.
- **Offline mode by default.** Pull on a network with egress, then work on a
  network without it.
