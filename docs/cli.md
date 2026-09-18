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

- **VMs**: full ID or any **unique prefix** — `mh stop a1b` is enough. An
  ambiguous prefix is refused with the list of matches rather than guessed.
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
VM=$(mh run base-alpine)                    # ID only on stdout; details go to stderr

mh ps                                       # running VMs
mh ps -a                                    # all, stopped included
mh ps -q                                    # IDs only
mh ps --net lab --json
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
mh rm a1b2 e5f6                             # destroy (disk included)
mh rm --all                                 # destroy EVERY VM
```

`-v` takes `NAME[:GUEST_PATH][:ro]` — the volume is mounted at `/vol/NAME` unless
you give a path. Sizes accept MiB (`512`) or a suffix (`512M`, `2G`).

`mh logs` reads the VM's `log_path` from the local disk (the API reports where
the log is, not its contents), so it only works on the daemon's host, and the
file may need `sudo`.

### Networks

```bash
mh network create lab                       # isolated: no egress, VMs can't see each other
mh network create build --egress            # full internet (NAT)
mh network create lab2 --intra --subnet 10.10.0.0/24
mh network create iot --allow tcp:203.0.113.7:8883 --allow icmp:203.0.113.7
mh network create ot-52 --allow tcp:192.168.50.52:502@wlan0

mh network ls
mh network inspect lab

# Change a live network (VMs stay up)
mh network update lab --intra                         # VM↔VM on
mh network update lab --no-intra
mh network update lab --egress                        # full internet
mh network update lab --no-egress                     # cut everything
mh network update lab --allow tcp:1.2.3.4:443         # REPLACE the rules with these
mh network update lab --add-allow udp:10.0.0.53:53    # add to the current rules
mh network update lab --rm-allow icmp:203.0.113.7     # remove one
mh network update lab --add-allow tcp:1.2.3.4:80 --no-intra   # egress + intra in one go

mh network rm lab                           # refuses while VMs are attached
mh network rm -f lab                        # destroys its VMs first
```

**Egress rules** are `PROTO:IP[:PORT][@IFACE]`:

| rule | meaning |
|---|---|
| `tcp:203.0.113.7:8883` | TCP to that host and port, out to the WAN |
| `udp:10.0.0.0/24:53` | UDP to a whole CIDR, port 53 |
| `icmp:203.0.113.7` | ping (no port) |
| `tcp:192.168.50.52:502@wlan0` | through a **managed interface** (see [networking.md](networking.md)) |

The API replaces the whole egress policy on every update. `--add-allow` and
`--rm-allow` are the client reading the current rules and sending the merged
list. `--rm-allow` of a rule that isn't there is an error, so a typo can't
look like a closed hole. The daemon validates each rule (canonical CIDR, port
range, managed interface) and its message comes back as is.

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
mh fork a1b2 --quarantine                   # direct clone of a running VM
mh snapshot rm clean
```

### Platform

```bash
mh health          # checks table; exit 1 when degraded (usable in scripts/monitors)
mh info            # host, memory, store, fleet, checks, store usage, paths
mh info --json     # the raw GET /v1/system
```

## Output and exit codes

- Listings are tables; `-q` prints only IDs/names (for `$(...)` and `xargs`),
  `--json` prints the API's JSON unchanged.
- `inspect` prints JSON: a bare object for one argument (`| jq .field`), an
  array for several.
- Commands that create something print its ID/name on stdout.
- Exit codes: `0` ok · `1` the operation failed (or `health` is degraded) ·
  `2` bad command line · for `exec`, the guest command's own exit code.
- Commands over several targets (`rm a b c`, `stop a b`) keep going past a
  failure, report each one, and exit `1` if any failed.
