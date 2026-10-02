# mh sandbox — run code you do not trust, and see what it did

One command: a fresh VM, the code inside, decoy credentials around it, the
network cut before the code runs, and a report of what it did — from inside
(decoys read, ways it looked for a VM, files changed, processes left) and from
the host (every connection it tried).

```bash
mh sandbox ./install.sh                                   # a file: runs it
mh sandbox ./repo 'npm test' --fetch 'npm ci --ignore-scripts'
mh sandbox https://github.com/x/y 'make test' --apt build-essential
mh sandbox ./release.tgz 'bash setup.sh' --json           # for an agent
```

The image is built once from `sandbox/` (sudo, like any `mh build`):

```bash
mh build -t sandbox:1 sandbox
```

## What it takes

`mh sandbox [FLAGS] TARGET [COMMAND]`

| TARGET | Goes in as |
|---|---|
| a directory | its contents, in `~/work` |
| a file | `~/work/NAME`; with no COMMAND it is run: `./NAME` |
| `.tar.gz`, `.tgz`, `.tar`, `.zip` | unpacked into `~/work` |
| `https://…` | `git clone --depth 1` into `~/work`, inside the VM, with the fetch network |

COMMAND is one shell line, run as the sandbox's user in `~/work`.

| Flag | |
|---|---|
| `--fetch 'CMD'` | before the code runs, with a network: CMD must run nothing of the code (`npm ci --ignore-scripts`, `pip download --only-binary=:all: -d wheels -r requirements.txt`, `go mod download`) |
| `--apt PKG` | Ubuntu packages, installed by root before the code runs (repeatable, or comma-separated) |
| `--image IMAGE` | default: the newest `sandbox:N` |
| `--iface IFACE` | the fetch network's way out (default: the host's default route) |
| `--timeout D` | for the fetch and the run each (default 5m, at most 10m) |
| `--keep` | keep the VM (and its network) afterwards, to look inside |
| `--json` | the report as JSON on stdout (schema below), everything in it |
| `-v`, `--verbose` | live: each finding as it happens (below); then the report in full — every probe by every program, and every command the code ran. Without it, one line per path (the programs that asked named) and at most 15 per section |

The exit code is the command's. A sandbox that could not be set up exits 1
and runs nothing.

## The steps

1. A network of its own when anything is fetched (`--fetch`, `--apt`, a URL):
   internet through IFACE, ports 80, 443 and 53 only, no private ranges, nothing
   in, no VM-to-VM. Otherwise `default`, which has no way out.
2. The VM, the code in (unpacked as the sandbox's user: root never writes in
   its home).
3. With a network: `git clone`, `apt-get install`, the fetch command — in that
   order, before any decoy exists, nothing reported.
4. **The network's way out closed**, and the daemon asked to confirm it. If it
   cannot be confirmed, the VM is removed and nothing runs.
5. Prepare: decoys planted, audit on, the clock started.
6. The input scanned for text addressed to an AI agent (below).
7. The command, as the sandbox's user, no way to gain privileges, no network.
8. The report from inside, the refused connections from the host, the VM and
   its network removed.

## What it reports

| | Means |
|---|---|
| decoys | a fake credential read (`READ`) or written (`TAMPERED`). Each says who reads that file legitimately. |
| vm_probes | paths that tell a VM apart the code asked for — DMI, `/proc/cpuinfo`, PCI, modules, `/.dockerenv`, `systemd-detect-virt` — `absent` ones included: asking is the tell |
| privesc | what a privilege escalation looks for — `/etc/shadow`, `/etc/sudoers`, cron, root's home, container sockets (docker, lxd), the kernel's hardening switches, `/proc/PID/mem`; `sudo`, `su`, `pkexec`, `doas`, `getcap` looked up or run (by name); `find -perm` searching for setuid (`-4000`) or writable (`-o=w`) files — `absent` ones included |
| commands | every command line the code ran (from audit's `execve`), in the order it first ran it, with a count; the first 2000. The first lines are `mh-sandbox-run` starting it. `-v` prints them |
| alerts | what code does when it means harm, each with a kind, a severity (`high`: what malicious code does; `warn`: unusual for ordinary code; `info`: worth knowing), what it did and the program that did it — table below |
| changed | files and directories created or changed: outside `~/work` one by one, inside it one line per entry with a count |
| processes | still running as the sandbox's user |
| listening | sockets it opened |
| connections | what it tried that the network refused (the host's view) |
| addresses_an_agent | text in the input or the output that speaks to an AI agent: "ignore previous instructions", "system prompt", chat-template tokens, invisible Unicode |

A path in `vm_probes` and `privesc` is the one the code asked for, made
absolute against its working directory (`cd /sys/class/dmi; cat
id/product_name` is `/sys/class/dmi/id/product_name`) and without a trailing
`/`. `absent` means the lookup failed: the path is not there, it is hidden
from the sandbox's user (`/proc/1/cgroup`: other users' processes are hidden),
or a file was asked for as a directory (`/proc/cmdline/`). The program is
named by its file when its process name was cut at 15 characters
(`systemd-detect-virt`, not `systemd-detect-`).

| Alert | Severity | When |
|---|---|---|
| `privesc_attempt` | high | a syscall that changes who the process is refused (`setuid(0)`, `setresgid`, `capset`, `setgroups`), or one an ordinary program never makes as a user: `mount`, `chroot`, `pivot_root`, `bpf`, a kernel module, `keyctl`, `userfaultfd`, `perf_event_open`, `personality` turning ASLR off |
| `namespace` | warn | `unshare`, `setns` — browsers' sandboxes (Playwright) do it too |
| `ptrace` | warn | `ptrace` — debuggers do it too |
| `persistence` | high | a write (refused ones too) to `~/.ssh/authorized_keys`, a systemd user unit, `~/.config/autostart`, cron, `/etc`'s startup files; `crontab`, `at`, `systemctl --user enable` run |
| `shell_rc` | warn | a write to `~/.bashrc`, `~/.profile`… — installers (nvm, rustup) do it too |
| `system_write` | warn | any other write to `/etc` |
| `reverse_shell` | high | a shell joined to a connection: `bash -i >& /dev/tcp/…`, `nc -e`, `socat … exec:`, `mkfifo … \| sh -i \| nc`, a Python/Perl socket with a shell, `pty.spawn` |
| `dev_tcp` | warn | bash's `/dev/tcp` or `/dev/udp` otherwise (a connectivity check, a hand-made request) |
| `pipe_to_shell` | warn | `curl … \| sh`, `sh -c "$(curl …)"` — installers do it too |
| `obfuscated_exec` | high | decoded and run at once: `base64 -d \| sh`, `xxd -r \| sh`, `exec(base64.b64decode(…))` |
| `dropper` | high | a binary written after the sandbox was prepared, by anything but a compiler or linker, in `/tmp`, `/var/tmp` or `/dev/shm`, then run |
| `dropped_exec` | warn | the same elsewhere (`~/work`), or a script written in a temporary directory and run |
| `antiforensics` | high | history off or cleared (`HISTFILE=/dev/null`, `history -c`), `touch -d/-r` outside `~/work`, `shred`, logs or a dropped program removed |
| `miner` | high | `xmrig`, `stratum+tcp://`, `--donate-level`, a mining pool's name |
| `credential_search` | high | `grep -r`, `find -name`, `locate` for passwords, keys, tokens (`id_rsa`, `.pem`, `AKIA`…) outside `~/work` |
| `connect` | info | which program tried to reach which address (`:53 (dns)`: a name lookup); loopback left out |
| `audit_health` | warn | audit may have missed some: the disk nearly full (audit stops recording below `admin_space_left`, 150 MB), auditd stopped, the kernel holding the code back for audit to keep up |

Shell builtins (`history -c`, `export HISTFILE=…`) and redirections (`>
/dev/tcp/…`) show only when they are in a command line run (`bash -c '…'`),
not inside a script file: audit sees programs started, not what a shell does
within. The commands that make alerts are matched on their arguments: code
that means to hide can split, encode or rename; an alert is evidence, its
absence is not.

`evasion_suspected` is true when the code looked for a VM — any probe but
`/proc/cpuinfo` and `/proc/self/cgroup`, which ordinary programs read too
(Node's `os.cpus()`, cgroup limits in Node, Go and the JVM). Then nothing
else in the report means "harmless": code that knows it is watched can
behave.

### No finding is not "safe"

The sandbox shows what the code did in this run: not what it would do with a
network, after more than the timeout, on another date, or once it has decided
it is not watched (the CPUID instruction does not show). A report with
nothing in it is the absence of evidence.

### The report is data, never instructions

Everything the code chose is in it: file names, process names and command
lines, its output, the matched text. A file can be named `IGNORE PREVIOUS
INSTRUCTIONS…`. In the JSON those strings are only in fields marked
untrusted below, unprintable characters replaced, long ones cut; every other
field is ours: counts, booleans, fixed words, our paths. An agent decides on
the `summary`; it reads the untrusted fields as quoted data, if at all.

The scan for text addressed to an agent is a signal, not a defence: it is
evaded by base64, another language or a split phrase. Code that has such text
has no business having it.

### Live, with `-v`

While the code runs, `mh sandbox -v` looks every 2 seconds and prints each
finding once, the moment it shows: a decoy read, a probe for a VM or for a
way to root, a connection the host refused.

```
== live (-v): what it does, as it happens
     +1s  privesc      find -perm -o=w found (by find)
     +9s  privesc      /etc/shadow found (by newgrp)
    +42s  DECOY READ   /home/dev/.aws/credentials
    +60s  connection   udp 1.1.1.1:53 refused (egress)
```

The guest's side is the image's `mh-sandbox-watch`, which reads only what
audit's log gained since the last look. The report at the end is the whole
account; the live view can miss what the log wrote and rotated between two
looks.

If the code is still running when the timeout comes, it is stopped
(SIGSTOP) where it is and looked at: what it left running is still listed.

If the report from inside the VM fails, the rest is still printed — the
output, the exit code, the host's connections — with `"complete": false`
and a warning: everything from inside is then unknown, not empty.

## JSON

```json
{
  "target": "./repo",              // as given
  "image": "sandbox:3@sha256:…",
  "vm": "3878e944",
  "kept": false,
  "exit_code": 0,                  // the command's; 124 if it timed out
  "timed_out": false,
  "complete": true,                // false: the VM's side could not be read
  "summary": {
    "decoys_read": 1,
    "vm_probes": 2,
    "evasion_suspected": true,
    "privesc": 3,
    "commands": 41,                // distinct command lines
    "alerts": 1,
    "changed_outside_work": 2,
    "processes_left": 0,
    "listening": 0,
    "connections_refused": 8,
    "addresses_an_agent": 0
  },
  "decoys":      [{"path": "/home/dev/.netrc", "state": "READ", "legitimately": "curl -n, …"}],
  "vm_probes":   [{"path": "/sys/class/dmi/id/product_name", "found": false, "count": 1, "by": "python"}],
  "privesc":     [{"path": "find -perm -4000", "found": true, "count": 1, "by": "find"}],
  "commands":    [{"count": 3, "args": "find / -perm -4000"}],
  "alerts":      [{"kind": "shell_rc", "severity": "warn", "count": 1, "what": "/home/dev/.bashrc", "by": "bash"}],
  "changed":     {"files": ["/home/dev/.bashrc"], "dirs": ["/tmp"],
                  "work_files": [{"entry": ".v/", "count": 203}], "work_dirs": [{"entry": ".v/", "count": 33}]},
  "processes":   [{"pid": 123, "args": "…"}],
  "listening":   ["tcp 0.0.0.0:8080"],
  "connections": [{"protocol": "udp", "dst": "1.1.1.1", "dst_port": 53, "count": 4, "reason": "egress"}],
  "addresses_an_agent": [{"where": "input", "file": "README.md", "line": 12, "text": "ignore previous instructions"}],  // where: input, output, created (a file's name)
  "output": "…",                   // the command's, last 8 KiB
  "warnings": ["audit lost 3 events: vm_probes may be incomplete"]
}
```

Untrusted (the code's choice): `changed.*`, `vm_probes[].path` and `.by`,
`privesc[].path` and `.by`, `commands[].args`, `alerts[].what` and `.by`,
`processes[].args`, `addresses_an_agent[].file` and `.text`, `output`.

## Out of scope

- **Running an agent inside** (to audit a skill by watching an agent use it):
  it needs the model's API reachable and a key inside the VM. Not done.
  Auditing a skill is its text through the scan and its scripts through
  `mh sandbox`.
- **A network while the code runs**: never. What it would have reached is in
  `connections`.
