# `mh` — the command-line client

`mh` drives the daemon the way `docker` drives dockerd: one short command per
operation instead of a `curl` against the Unix socket with a hand-written JSON
body. It is a thin client over the [HTTP API](api.md) — every command maps to
one or two API calls and shows the daemon's own error message when one fails.

It is built with the daemon (`make build` → `build/mh`) and installed with it
(`make install-service`, and so `make full-install` → `/usr/local/bin/mh`, with
`mh-orchestrator`); `make install-cli` installs just the clients.

## Shape of a command

Same as docker: **object first, then the verb**.

```bash
mh network create lab --intra
mh volume ls
mh vm inspect a1b2
```

The VM verbs you use most also exist at the top level, like `docker ps`,
`docker run` and `docker exec`:

| shortcut | same as | | shortcut | same as |
|---|---|---|---|---|
| `mh ps` | `mh vm ls` | | `mh logs` | `mh vm logs` |
| `mh run` | `mh vm create` | | `mh fork` | `mh vm fork` |
| `mh exec` | `mh vm exec` | | `mh restore` | `mh vm restore` |
| `mh stop` / `mh start` | `mh vm stop` / `start` | | `mh inspect` | `mh vm inspect` |
| `mh rm` | `mh vm rm` | | `mh images` | `mh template ls` |
| `mh cp` | `mh vm cp` | | `mh health` / `mh info` | `mh system health` / `info` |
| `mh flows` | `mh vm flows` | | | |

**The verb may also come first**: `mh create vm base-alpine`,
`mh list network` and `mh change network lab --intra` are rewritten to the
object-first form, so both spellings behave exactly the same. Verbs have
aliases: `ls`/`list`, `rm`/`remove`/`delete`, `inspect`/`show`,
`update`/`change`/`set`, `create`/`new`. Objects too: `net`, `vol`, `snap`,
`tpl`, plurals.

Flags work anywhere on the line (`mh run base-alpine --net lab` and
`mh run --net lab base-alpine` are the same), except for `exec`, where
everything after the VM is the guest command.

## Referring to things

- **VMs**: full ID, any **unique prefix** — `mh stop a1b` is enough — **or
  name** (`mh stop ts01-a`). An ambiguous prefix is refused with the list of
  matches rather than guessed.
- **Volumes and snapshots**: ID, unique ID prefix, **or name**
  (`mh volume rm output`, `mh restore a1b2 clean`). On `restore`, a snapshot
  name is looked up among that VM's own snapshots, so every VM can have its
  own `clean`.
- **Networks** and **templates**: by name.

## Where it connects

`/run/microhosted.sock` by default. Override with `-H` or an env var, in this
order: `-H`, `$MICROHOSTED_HOST`, `$MICROHOSTED_SOCKET`.

```bash
mh -H /run/other.sock ps
MICROHOSTED_HOST=tcp://127.0.0.1:8080 mh ps     # daemon started with --addr
```

Access is the socket's file permissions (see [api.md](api.md#calling-the-api-and-who-may)):
be in the socket's group or use `sudo mh …`. `mh` says which one is the problem:
*permission denied* (not in the group) is reported differently from *the daemon
is not running*. `curl` shows both as "Could not connect".

## Command reference

`mh --help`, `mh <object> --help` and `mh <command> --help` list every flag.

### VMs

```bash
mh images                                   # templates you can create from
mh run base-alpine                          # prints the new ID on stdout
mh run base-alpine --net lab -c 2 -m 512M --disk 2G
mh run base-alpine --no-net -v sample:/mnt/sample:ro -v output
mh run alpine-py --name ts01-a -l sensor=ts-01 -l managed-by=ot
mh run base-alpine --net ing53 --ip 172.16.20.3   # exact address: a replacement's, or an ingress to_ip
VM=$(mh run base-alpine)                    # ID only on stdout; details go to stderr
VM=$(mh run base-alpine --wait)             # return once its agent answers: exec/cp work at once
mh run base-alpine --wait --wait-timeout 5s # (also on start, fork, snapshot fork, replace, restore)

mh ps                                       # running VMs
mh ps -a                                    # all, stopped included
mh ps -q                                    # IDs only
mh ps --net lab --json
mh ps -l sensor=ts-01                       # only VMs with that label (repeat -l: all must match)
mh ps -L                                    # --show-labels: add a LABELS column
mh inspect a1b2 | jq .guest_ip

mh exec a1b2 uname -a                       # exit code = the guest command's
mh exec a1b2 'ps | grep socat'              # one argument = a shell line (pipes work)
mh exec a1b2 -- sh -c 'echo $HOME'
mh exec -t 5s a1b2 /opt/probe               # give up after 5 s (default 30s, max 10m); mh flags go before the VM

mh cp ./sample.bin a1b2:/root/              # upload (trailing / keeps the name)
mh cp a1b2:/var/log/messages .              # download
mh cp a1b2:/etc/os-release -                # to stdout

mh logs a1b2 -n 50                          # console log (through the API)
mh logs -f a1b2                             # follow (read from the host's disk)
mh flows a1b2                               # connections it tried that its network refused
mh flows a1b2 --json                        # with "recording": false if the host is not recording

mh stop a1b2 && mh start a1b2               # power off keeping disk + IP, boot again
mh vm update a1b2 --autostart               # boot it again on its own after a host reboot
mh run base-alpine --disk-mib-s 20 --net-mbit 10   # below the daemon's ceiling (never above)
mh label ts01-a sensor=ts-02 role-          # set sensor, remove role; the rest untouched
mh replace ts01-a --old stop                # new VM takes its IP + labels; old cut off and powered off
mh replace ts01-a -s clean --old destroy    # replacement forked from snapshot "clean"
mh run parser:1.2 -f /etc/parser.conf=./ts01.conf,mode=0640 --secret /etc/parser.key=./ts01.key
                                            # written into the disk before boot; local files read by mh, not the daemon
mh replace ts01-a -f /etc/parser.conf=./ts01.conf --secret /etc/parser.key=./ts01.key
mh replace ts01-a --no-files                # a VM created with files needs them again, or --no-files
mh quarantine ts01-a                        # cut it off its network in place: IP freed, still running, exec/cp work
mh rm a1b2 e5f6                             # destroy (disk included)
mh rm --all                                 # destroy EVERY VM
```

`--name` is an alias for that one VM (unique, fixed for its life); what should
survive replacing the VM — the sensor it serves, who owns it — goes in labels
(see [api.md](api.md#post-v1vms--create)). `-c`/`-m` are also the VM's host limits:
its cgroup caps it at that many cores and that memory (+64 MiB for Firecracker).

`-v` takes `NAME[:GUEST_PATH][:ro]` — the volume is mounted at `/vol/NAME` unless
you give a path. Sizes accept MiB (`512`) or a suffix (`512M`, `2G`).

`mh logs` reads the end of the console through the API
(`GET /v1/vms/{id}/console`, the last 4 MiB — all a VM keeps), so it needs
neither root nor the daemon's host. `mh logs -f` follows the file at `log_path`
on the local disk instead: it only works on the daemon's host, and the file may
need `sudo`.

### Networks

A network's policy is named by **who opens the connection**:

| | who opens it | where to |
|---|---|---|
| **OUT** | the VM | the internet, or a device behind a managed interface (`@IFACE`); `--internet IFACE` opens ALL outbound through that one host interface |
| **IN** | a device behind a managed interface | one VM of the network |

Everything not listed is dropped, both ways. (The API calls these
`allowed_egress` and `allowed_ingress`.)

```bash
mh network create lab                       # isolated: nothing in or out, VMs can't see each other
mh network create build --internet eth0     # the internet, through eth0 only (NAT); not the LAN
mh network create dev --internet eth0 --private   # the LAN behind eth0 too
mh network create fetch --internet eth0 --ports tcp:80,tcp:443,udp:53   # the internet on these ports only
mh network create lab2 --intra --subnet 10.10.0.0/24
mh network create iot --out tcp:203.0.113.7:8883 --out icmp:203.0.113.7
mh network create ot-52 --out tcp:192.168.50.52:502@wlan0
mh network create mqtt-60 --subnet 172.16.9.0/24 --in tcp:192.168.50.60:1883@wlan0=172.16.9.2

mh network create ts-01 -l managed-by=ot -l zone=north   # labels, as on VMs

mh network ls
mh network ls -l managed-by=ot -L           # only networks with that label; -L adds a LABELS column
mh network inspect lab
mh network label ts-01 zone=south owner-    # set zone, remove owner

# Change a live network (VMs stay up)
mh network update lab --intra                         # VM↔VM on
mh network update lab --no-intra
mh network update lab --out udp:10.0.0.53:53          # add an OUT rule
mh network update lab --rm-out icmp:203.0.113.7       # remove one
mh network update lab --set-out tcp:1.2.3.4:443       # REPLACE all OUT rules with these
mh network update lab --internet eth0                 # ALL outbound, through eth0 only
mh network update lab --no-out                        # close all outbound
mh network update mqtt-60 --in tcp:192.168.50.61:1883@wlan0=172.16.9.3      # add an IN rule
mh network update mqtt-60 --rm-in tcp:192.168.50.61:1883@wlan0=172.16.9.3   # remove one
mh network update mqtt-60 --no-in                     # close all inbound
mh network update lab --out tcp:1.2.3.4:80 --no-intra # OUT + intra in one go

mh network rm lab                           # refuses while VMs are attached
mh network rm -f lab                        # destroys its VMs first
```

`--internet` needs the interface: "all outbound" without one used to mean
everything that is not one of our bridges — the LAN behind a second NIC, a VPN,
the Docker networks. A network created before this rule was pinned to the
default route's interface; `mh network ls` shows `internet@eth0`, or `CLOSED`
when the daemon found no usable one (set it with `--internet IFACE`).

**OUT rules** are `PROTO:DEST[:PORT][@IFACE]`:

| rule | meaning |
|---|---|
| `tcp:203.0.113.7:8883` | TCP to that host and port, on the **internet** (no `@IFACE`) |
| `udp:10.0.0.0/24:53` | UDP to a whole CIDR, port 53 |
| `icmp:203.0.113.7` | ping (no port) |
| `tcp:192.168.50.52:502@wlan0` | to a device behind the **managed interface** `wlan0` (see [networking.md](networking.md)) |

Without `@IFACE` a rule means the internet, even if the address happens to sit
on a managed segment: `icmp:192.168.50.52` does **not** reach the device behind
`wlan0`, `icmp:192.168.50.52@wlan0` does.

**IN rules** are `PROTO:SRC:PORT@IFACE=VM_IP`, e.g.
`tcp:192.168.50.60:1883@wlan0=172.16.9.2`. The device `192.168.50.60` connects
to **this host's address** on `wlan0`, port 1883, and lands on the VM
`172.16.9.2`, same port. The host itself listens on nothing (see
[networking.md](networking.md) § Ingress). `@IFACE` is required, and at create
time `--in` needs `--subnet`.

The API replaces the whole policy on every update. `--out`/`--rm-out` and
`--in`/`--rm-in` are the client reading the current rules and sending the
merged list. Removing a rule that isn't there is an error, so a typo can't look
like a closed hole. The daemon validates each rule (canonical CIDR, port range,
managed interface, VM_IP inside the subnet) and its message comes back as is.

The old spellings (`--allow`, `--add-allow`, `--rm-allow`, `--egress`,
`--no-egress`, `--ingress`, `--add-ingress`, `--rm-ingress`, `--no-ingress`)
still work but are no longer shown in `--help`. In `update`, `--allow` and
`--ingress` REPLACE (like `--set-out`/`--set-in`); `--add-*` add.

### Volumes

```bash
mh volume create output --size 512M         # prints the ID
mh volume create dataset -s 8G
mh volume ls
mh volume cp ./sample.bin sample:/sample.bin    # offline, volume must be DETACHED
mh volume cp output:/result.txt .
mh volume rm output                         # by name or ID
```

To write into a volume that a running VM has attached, go through the VM:
`mh cp file VM:/vol/output/`.

### Snapshots and forks

```bash
mh vm snapshot a1b2 --name clean            # or: mh snapshot create a1b2 --name clean
mh snapshot ls [--vm a1b2]
mh restore a1b2 clean                       # rewind in place (same ID, IP)
mh snapshot fork clean --quarantine         # new VM from a snapshot, no network
mh snapshot fork clean --name ts01-b -l sensor=ts-01   # a fork inherits no name or labels
mh fork a1b2 --quarantine                   # direct clone of a running VM
mh snapshot rm clean
```

### Images

```bash
# copy a golden into the content-addressed store (once; prints NAME:VERSION@DIGEST)
mh image import parser:1.0 \
  --kernel /var/lib/microhosted/store/kernels/vmlinux-6.1.102 \
  --rootfs /var/lib/microhosted/store/rootfs/alpine-py.ext4 --mem 128 --disk 512

mh image ls                                  # tags, short digest, defaults, size
mh image ls -q                               # pinned references, for scripts and specs
mh image ls --no-trunc                       # the full digest in the table
mh run parser:1.0                            # a reference with ':' is an image; its command, if it declares one, is started
mh run --no-command parser:1.0               # boot it without starting the image's command
mh run parser:1.0@sha256:…                   # pinned: refused if the tag says otherwise
mh image verify parser:1.0                   # re-hash its files (slow, explicit)
mh image rm parser:stable                    # the image has other tags: removes only this one
mh image rm parser:1.0                       # its last tag: deletes the image; refused while a VM or snapshot uses it
mh image rm sha256:…                         # a digest: deletes the image with all its tags
mh replace ts01-a                            # a VM from an image is replaced from ITS digest
```

Files are imported from the daemon's store directory only. A tag is bound once:
rebuilding under the same `name:version` is refused — import it as a new
version. `--command` and `--health-cmd`/`--health-interval`/`--health-timeout`/
`--health-retries` record the image's default command and health check, as
`mh build` does from a spec.

### Building an image: `mh build`

`mh build` (also `mh image build`) is to a `build.yml` what `docker build` is to
a Dockerfile: it builds the image, imports it into the store and prints its
pinned reference. That reference is the **only** thing it prints on stdout —
progress goes to stderr, and `-q` hides it.

```bash
mh build                                     # ./build.yml, context .
mh build web/                                # web/build.yml, context web/
mh build -q -f web/build.yml                 # just the reference: web:sha-3f2a9c1e7b04@sha256:…
mh build -t web:1.0 web/                     # an explicit tag
mh build --no-cache web/                     # every step again, even if nothing changed (newer packages)
mh build --no-build web/                     # only look: the reference, or exit 3 if not built
mh build --adopt web/                        # trust a store image of these inputs you have no record of building
```

**Naming and the build cache.** Without `-t`, the image is named by the spec's
`name:` (else the context directory's name) and versioned by the
**fingerprint** of everything the build takes in: the parsed spec (not its
comments), the pins its base and kernel resolve to, the guest agent, and every
file it copies — paths, modes, contents. When the store already has that tag
**and this user's record** (`$XDG_STATE_HOME/microhosted/builds/NAME/VERSION`,
written by the build that imported it) names the same digest, nothing is built
and no `sudo` is asked: the existing reference is printed. A tag is not proof on
its own — once removed, anyone with the API can import other bytes under it —
so a tag whose digest differs from the record is refused, and one with no
record (built by another user, or the record lost) is refused until
`mh build --adopt` takes it after you checked it. Change a copied file, or a
pin in the code, and the next build is a new version. The fingerprint cannot see what the package repositories serve:
unpinned packages stay as they were built until `--no-cache`.

**Layers.** A build that does run is a chain of layers, as Docker's:
`FROM` (the base laid down), `PREPARE` (the base upgraded), `PACKAGES`,
`AGENT`, then one `COPY` per file and one `RUN` per command, in that order.
Each is kept for the next build under a key made of its parent layer and all
it takes in (its scripts, their arguments, a copied source's contents); a
build takes every layer whose key it finds (`---> Using cache`) and runs the
rest. So a change reruns its step and those after it, nothing before: a new
line in a copied file copies that file and reruns the `run:` steps, it does
not lay down or upgrade the base again, and a change to `command`, `health`
or `mem_mb` — which are not in the tree — only writes the ext4 again.

`files:` and `run:` copy every file before the first command, so a change to
any file reruns every command. `steps:` lists copies and commands in the
order they run, as a Dockerfile's lines do: copy the dependency list, install
it, then copy the code, and a change to the code leaves the install in the
cache (`orchestrator/examples/stack/api/build.yml`). A spec has either
`steps:` or `files:`/`run:`; a step is `copy:` (one or more guest path ←
source entries, with the same rules as `files:`) or `run:`, never both.

- Each step runs on an overlayfs of the layers below it, chrooted, in mount,
  PID, UTS and IPC namespaces of its own: what it mounts and every process it
  starts end with it, and it does not see the host's processes. The network
  is the host's. `/proc`, `/dev`, `/etc/resolv.conf` and the package cache are
  the build's own, mounted over directories that hide whatever the image has
  at those paths, and are not in any layer.
- The layers, and the package caches (apk's and apt's downloads, and
  debootstrap's), are root's: `STORE/build/layers` and `STORE/build/cache`,
  `0700`. apk and apt check every cached index and package against the
  archive's signature before using it. A layer is written once, by its step,
  and only mounted read-only after that.
- A step takes the network's state as it was when it ran — the base's
  upgrade, unpinned packages, a `run:` step's downloads — so a layer is
  reused for **7 days** at most; after that its step runs again, and every
  step above it (layers chain on their parent's identity, not its key).
  `--no-cache` runs every step now and replaces the kept layers.
- `mh builder prune` removes the layers no build took for a week (or
  `--older-than`) and those a newer layer replaced; `--all` removes every
  layer and the package caches.
- The store's filesystem must support overlayfs as an upper layer (ext4,
  xfs, btrfs do; an overlay itself, as in a container, does not), and its
  path must be letters, digits and `. _ - /`.

**From a plant spec.** A function says `build: DIR` (the directory holding a
`build.yml`) or `build: path/to/build.yml` instead of `image:`, and
`mh-orchestrator plan`/`apply`/`run` run `mh build` on it — building only what
changed — and pin the reference it prints. `validate`, `status`, `down` and a
running orchestrator's reload never build. The other way, when the image comes
from elsewhere, is `image:` with a pinned reference, typed or through
`${VAR}` from `.env`:

```bash
echo "WEB=$(mh build -q web/)" >> .env       # microse.yml: image: ${WEB}
```

The image spec, one YAML document parsed strictly (an unknown field is an
error). Commented, buildable examples, each next to a `microse.yml` that runs
it: `orchestrator/examples/alpine-nginx` and `orchestrator/examples/app-ubuntu`;
`orchestrator/examples/stack` builds three (two Ubuntu, one Alpine) and runs
them together.

```yaml
name: web                    # optional; default: the build context directory's name
base: alpine:3.22            # pinned Alpine minirootfs (alpine:3.22.0), or ubuntu:24.04 (ubuntu:noble)
kernel: 6.1.102              # optional; pinned Firecracker CI kernel
packages: [nginx]            # apk (Alpine) or apt (Ubuntu), optionally pinned: nginx=1.28.0-r3
files:                       # COPY: guest path ← source in the build context
  /srv/www/: out/            #   a directory: its contents, into the path (write it with a /)
  /etc/nginx/http.d/default.conf: nginx.conf   # a file (into the path when it ends in /)
run:                         # RUN: shell commands in the image, after packages and files
  - mkdir -p /run/nginx
# …or steps:, instead of files: and run:, in the order they run (Dockerfile order):
# steps:
#   - copy: {/opt/app/requirements.txt: app/requirements.txt}
#   - run: pip install -r /opt/app/requirements.txt   # kept while requirements.txt is unchanged
#   - copy: {/opt/app/: app/}                         # a code change reruns only this and after
# Defaults of its VMs. command and health are for whoever runs them — mh run
# starts the command (docker run's CMD), the orchestrator uses both when a plant
# spec leaves them out; the engine itself runs neither.
command: nginx -g 'daemon off;'
health: { command: "wget -qO- -T 2 http://127.0.0.1/", every: 10s, timeout: 2s, failures: 3 }
vcpus: 1                     # default 1
mem_mb: 64                   # default 128
disk_mb: 0                   # disk its VMs are grown to; 0: the rootfs's size
size_mb: 0                   # rootfs size; 0: its content plus a quarter and 32 MB
```

- **Where it runs.** On the engine's host: it writes the root filesystem under
  the daemon's store (`GET /v1/system` tells where) and imports it from there,
  then removes its working files. It works as root through `sudo` (it asks
  once, up front): unpacking the base, `apk add`, the copies and the `run`
  steps happen in a chroot of the image. A chroot does not confine root —
  build only specs you trust, as with any Dockerfile.
- **Bases.** `alpine:3.22`: the minirootfs, upgraded to the branch's current
  packages (`apk upgrade`), busybox init — a ~10 MB image idling at ~10 MB of
  RAM. `ubuntu:24.04`: `debootstrap --variant=minbase` (needs `debootstrap`
  and `ubuntu-keyring` on the build host) with systemd and udev, `apt` with the
  `-updates` and `-security` pockets, systemd starting the agent — for
  software that only exists as `.deb` or expects glibc. Minbase is Ubuntu's
  essential packages and apt, not the default variant's netplan, rsyslog,
  cron and the like: `packages:` adds what the image needs. No SSH in either:
  access is `mh exec` (add `openssh-server` to `packages` if you want it).
  Ubuntu's packages are verified by the archive's signature, not pinned by
  hash.
- **What goes in.** The Alpine base and the kernel are downloaded once to
  `~/.cache/microhosted` and refused unless their SHA-256 matches the pins in
  `scripts/checksums.sha256` (kept equal to `internal/build/pins.go` by a test).
  Every image gets the engine's agent (vsock exec on port 52) and busybox init,
  as `make prepare-image` builds them. File sources cannot leave the build
  context, through a symlink either; every write into the image is resolved
  inside the chroot, so a symlink in the image never points a write at the
  host.
- **Tags never move.** `-t NAME:VERSION` of a tag that exists is refused
  before building; the fingerprint tag is reused, never rebuilt; `--no-cache`
  tags with the build time.
- **Not bit-reproducible yet.** Two hosts building the same `build.yml` get the
  same *tag* (the fingerprint is of the inputs) but not the same *digest*:
  unpinned packages may differ, and ext4 timestamps and UUIDs do. Share the
  `build.yml` and its files — everyone builds the image with one command, or
  lets `build:` do it — rather than the digest, or share the image itself
  (`docs/roadmap.md`, "a plant spec that works on another host").

### Projects: `mh up`, `mh down`

A plant spec (`microse.yml`, see `orchestrator/README.md`) is run with docker
compose's verbs, on `./microse.yml` unless `-f FILE` says otherwise. `mh` hands
them to `mh-orchestrator` (installed with it by `make install-service` and
`make install-cli`), a separate
program with the same API access.

```bash
mh up                  # build what is missing, apply, keep it running in this terminal
mh up -d               # the same in the background: "Project web is up: pid …, logs: …"
mh plan                # what would change (builds missing images first)
mh apply               # converge once, or hand changes to the project's running up
mh status              # the project's functions, VMs and health
mh failures [FUNCTION] # why its VMs failed
mh down                # stop its up, remove its VMs and networks (asks; -y does not)
mh validate            # the file on its own
```

Each spec is a **project** — its `name:`, or its directory's name — and sees
only its own VMs and networks: `mh up` in two directories runs both. Its VMs
are named `<project>-<function>-<n>`.

### Untrusted code: `mh sandbox`

Runs a directory, a file, an archive or an https git URL in a fresh VM built
from `sandbox/`, with decoy credentials around it and the network cut before
it runs, and reports what it did. docs/sandbox.md has the whole of it.

```bash
mh build -t sandbox:1 sandbox                                    # once
mh sandbox ./install.sh                                          # a file: runs it; a verdict, then what it found
mh sandbox ./install.sh -o                                       # its own output too
mh sandbox ./repo 'npm test' --fetch 'npm ci --ignore-scripts'   # dependencies first, with a network
mh sandbox https://github.com/x/y 'make test' --apt build-essential
mh sandbox ./release.tgz 'bash setup.sh' --json                  # for an agent
mh sandbox ./linpeas.sh --live                                   # each finding as it happens
mh sandbox ./install.sh --rules sandbox/rules/example.yml       # rules of your own (any file, repeatable)
mh sandbox ./linpeas.sh -v                                       # every finding
```

### Platform

```bash
mh health          # checks table; exit 1 when degraded (usable in scripts/monitors)
mh info            # host, memory, store, fleet, checks, store usage, paths
mh info --json     # the raw GET /v1/system
mh doctor          # drift between the daemon and the host; exit 1 if any
mh events                         # follow what happens from now on (survives daemon restarts)
mh events -l sensor=ts-01 -t vm.died -t vm.replace_failed
mh events --all --no-follow       # every event the daemon still keeps, then exit
mh events --json                  # one JSON event per line; its epoch:seq resumes with --since
```

`mh doctor` lists everything the daemon and the host disagree on: Firecracker
processes, TAPs, bridges, jail dirs and cgroups nobody owns, disks and logs no
record owns, running VMs whose process or TAP is gone, IP leases and volume
claims of VMs that do not exist, interrupted creates, and a firewall ruleset
that failed to apply. It changes nothing; restarting the daemon cleans all of
it except disks, which it only reports. `scripts/fault-test.sh` uses it as the
pass/fail criterion.

A VM whose process dies on its own (OOM killer, crash, guest reboot) shows as
`stopped` within seconds, and `mh inspect VM` has `last_exit` with when and
why. It is not restarted.

## Output and exit codes

- Listings are tables; `-q` prints only IDs/names (for `$(...)` and `xargs`),
  `--json` prints the API's JSON unchanged.
- `inspect` prints JSON: a bare object for one argument (`| jq .field`), an
  array for several.
- Commands that create something print its ID/name on stdout.
- Exit codes: `0` ok · `1` the operation failed (or `health` is degraded, or
  `doctor` found something) ·
  `2` bad command line · for `exec`, the guest command's own exit code.
- Commands over several targets (`rm a b c`, `stop a b`) keep going past a
  failure, report each one, and exit `1` if any failed.
