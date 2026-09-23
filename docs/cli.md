# `mh` — the command-line client

`mh` drives the daemon the way `docker` drives dockerd: one short command per
operation instead of a `curl` against the Unix socket with a hand-written JSON
body. It is a thin client over the [HTTP API](api.md) — every command maps to
one or two API calls and shows the daemon's own error message when one fails.

It is built with the daemon (`make build` → `build/mh`) and installed with it
(`make install-service` → `/usr/local/bin/mh`); `make install-cli` installs just
the client.

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

mh cp ./sample.bin a1b2:/root/              # upload (trailing / keeps the name)
mh cp a1b2:/var/log/messages .              # download
mh cp a1b2:/etc/os-release -                # to stdout

mh logs a1b2 -n 50                          # console log (read from the host's disk)
mh logs -f a1b2

mh stop a1b2 && mh start a1b2               # power off keeping disk + IP, boot again
mh vm update a1b2 --autostart               # boot it again on its own after a host reboot
mh label ts01-a sensor=ts-02 role-          # set sensor, remove role; the rest untouched
mh replace ts01-a --old stop                # new VM takes its IP + labels; old cut off and powered off
mh replace ts01-a -s clean --old destroy    # replacement forked from snapshot "clean"
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

`mh logs` reads the VM's `log_path` from the local disk (the API reports where
the log is, not its contents), so it only works on the daemon's host, and the
file may need `sudo`.

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
mh network create build --internet eth0     # ALL outbound, through eth0 only (NAT)
mh network create lab2 --intra --subnet 10.10.0.0/24
mh network create iot --out tcp:203.0.113.7:8883 --out icmp:203.0.113.7
mh network create ot-52 --out tcp:192.168.50.52:502@wlan0
mh network create mqtt-60 --subnet 172.16.9.0/24 --in tcp:192.168.50.60:1883@wlan0=172.16.9.2

mh network ls
mh network inspect lab

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
