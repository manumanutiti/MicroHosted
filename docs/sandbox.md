# mh sandbox — run code you do not trust, and see what it did

One command: a fresh VM, the code inside, decoy credentials around it, the
network cut before the code runs, and a report of what it did — from inside
(decoys read, ways it looked for a VM, files changed, processes left) and from
the host (every connection it tried).

```bash
mh sandbox ./install.sh                                   # a file: runs it
mh sandbox https://astral.sh/uv/install.sh                # curl | sh, watched
mh sandbox npm:@modelcontextprotocol/server-filesystem    # a package: install, import, --help
mh sandbox pypi:httpie 'http --version'
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
| `https://…` | a git repository: `git clone --depth 1` into `~/work`. Anything else — `sh.rustup.rs`, an `install.sh` — downloaded as `~/work/NAME` (its own name, or `download`), as `curl \| sh` would; with no COMMAND it is run: `./NAME` when it starts with `#!` or is a program, `sh NAME` otherwise. Inside the VM, with the fetch network |
| `npm:NAME[@VERSION]` | `npm install --ignore-scripts` into `~/work` (Node 24 LTS in the image): nothing of it runs while the network is open |
| `pypi:NAME[==VERSION]` | `pip install --only-binary=:all:` into the venv `~/work/.v`: wheels only, since building an sdist runs its `setup.py` — a package with one fails the fetch (then: a directory, and `--fetch` that downloads it, built in the run) |

COMMAND is one shell line, run as the sandbox's user in `~/work`.

A package with no COMMAND is used the ways it can act — `mh-sandbox-try`, in
the image: npm's install scripts (`npm rebuild`), `import(NAME)`, each of its
`bin` with `--help`; for pypi, each top-level module imported and each console
script with `--help`, 30 s each. Code that does something on install, on
import or on start does it there. An MCP server — `mcp` in its name, or the
MCP SDK among its dependencies — is also spoken to over stdin: `initialize`,
then `tools/list`, `prompts/list`, `resources/list` (given `~/work` as an
argument if it answers nothing without one). Its tools' descriptions, what a
model reads and where a poisoned server hides instructions for it, are then
in the output, which is scanned for text addressed to an agent. With a COMMAND, its commands are on the PATH
(`node_modules/.bin`, `.v/bin`), after npm's install scripts; a COMMAND that
starts an MCP server — `mcp-server-filesystem /tmp`, as the user's MCP config
would — is spoken to the same way, with its arguments as given, instead of
waiting on a stdin nobody writes to:

```bash
mh sandbox npm:cowsay 'cowsay hi'
mh sandbox npm:@modelcontextprotocol/server-filesystem@2025.8.21 'mcp-server-filesystem /tmp'
```

| Flag | |
|---|---|
| `--fetch 'CMD'` | before the code runs, with a network, after a package's or URL's own fetch: CMD must run nothing of the code (`npm ci --ignore-scripts`, `pip download --only-binary=:all: -d wheels -r requirements.txt`, `go mod download`) |
| `--apt PKG` | Ubuntu packages, installed by root before the code runs (repeatable, or comma-separated) |
| `--image IMAGE` | default: the newest `sandbox:N` |
| `--iface IFACE` | the fetch network's way out (default: the host's default route) |
| `--timeout D` | for the fetch and the run each (default 5m, at most 10m) |
| `--cpus N`, `--mem SIZE` | the VM's size (default 2 vCPUs, 2G): a build — `go build`, `cargo`, webpack — outgrows less. Out of memory, the kernel kills the code first, never the agent or audit: a run that outgrows the VM fails as itself, the report intact |
| `--keep` | keep the VM (and its network) afterwards, to look inside |
| `--json` | the report as JSON on stdout (schema below), everything in it |
| `-o`, `--output` | print the code's own output too (its last 8 KiB, every line prefixed with `\|`); by default only what it did is reported |
| `-v`, `--verbose` | the report in full — every finding under its kind, every probe by every program, every command the code ran, what changed in `~/work`. Without it, what the findings amount to: probes by what they go for, the same command on many directories as one line, at most 8 lines a kind |
| `--live` | each finding as it happens, every 2 seconds, while the code runs (below); the report follows |
| `--no-sinkhole` | answer no name the code looks up, as with no network at all (below, "Its network"); by default the sinkhole answers |
| `--rules FILE` | rules of your own (below), from any file, in order (repeatable); none is read without it |

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
5. Prepare: decoys planted, the code's network started inside the VM (its
   resolver and sinkhole, below), audit on, the clock started.
6. The input scanned for text addressed to an AI agent (below).
7. The command, as the sandbox's user, no way to gain privileges, no way out
   of the VM.
8. The report from inside, the refused connections from the host, the VM and
   its network removed.

## What it reports

| | Means |
|---|---|
| decoys | a fake credential read (`READ`) or written (`TAMPERED`), and the programs that opened it (`by`). Each says who reads that file legitimately; read only by that tool (npm reading `~/.npmrc`), it is `info`, not `high` |
| vm_probes | paths that tell a VM apart the code asked for — DMI, `/proc/cpuinfo`, PCI, modules, `/.dockerenv`, `systemd-detect-virt` — `absent` ones included: asking is the tell |
| privesc | what a privilege escalation looks for — `/etc/shadow`, `/etc/sudoers`, cron, root's home, container sockets (docker, lxd), the kernel's hardening switches, `/proc/PID/mem`; `sudo`, `su`, `pkexec`, `doas`, `getcap` looked up or run (by name); `find -perm` searching for setuid (`-4000`) or writable (`-o=w`) files — `absent` ones included. Not perl's `getpw*`, which reads `/etc/shadow` right after `/etc/passwd` for the shadow password (glibc's `getspnam`) |
| commands | every command line the code ran (from audit's `execve`), in the order it first ran it, with a count; the first 2000. The first lines are `mh-sandbox-run` starting it. `-v` prints them |
| alerts | what code does when it means harm, each with a kind, a severity (`high`: what malicious code does; `warn`: unusual for ordinary code; `info`: worth knowing), what it did and the program that did it — table below |
| changed | files and directories created or changed: outside `~/work` one by one, inside it one line per entry with a count. The report groups them (`~/.cache/go-build/ (1515)`) and grades caches (`~/.cache`, `~/.npm`, `~/go`…), a tool's settings in `~/.config` and temporary files `info` |
| processes | still running as the sandbox's user |
| listening | sockets it opened |
| dns | every name the code looked up, with its type and count — `github.com`, `pastebin.com`, `x7f3.evil.example`. A name sent to another resolver directly (`dig @8.8.8.8`) shows only as a connection |
| requests | what it sent the sinkhole: method, URL, size, and whether this run's decoy token was in it (as is, URL- or base64-encoded, gzipped) — `POST https://evil.example/collect` carrying the fake AWS key. `tls_refused`: HTTPS clients that refused the sinkhole's certificate (a list of authorities of their own): the name only |
| connections | what it tried that the network refused (the host's view); the report names the program that tried each (audit's `connect`, inside) |
| addresses_an_agent | text in the input or the output that speaks to an AI agent: "ignore previous instructions", "note to the AI:", chat-template tokens (a phrase is `warn`: skills, prompts and their tests are full of them); Unicode that hides text — tag characters, a run of zero-width ones, bidirectional controls in code or a file's name (not one zero-width joiner, which emoji and names have, nor a right-to-left override in pip's `AUTHORS.txt`) |

### Reading it

A verdict first — `SUSPICIOUS` (something high), `REVIEW` (something warn),
`NOTHING SUSPICIOUS SEEN`, `DID NOT RUN` (the command never started) — then one block per kind of finding, the most
serious first, each line what the findings amount to: the privesc probes by
what they go for, the VM probes by what they tell apart, the same command run
on fifteen directories as one line, every connection with the program that
tried it. Kinds that are only `info` share one line. Then what was checked
and found clean, so an empty category never reads as an unchecked one; the
last line, the other views.

```
XX SUSPICIOUS -- it read the decoy credentials, searched for setuid binaries and searched for
   credentials
   ./linpeas.sh, sandbox:11, exit 0, 2m44s, 2342 commands
   !! it looked for a VM: what it did not do here proves nothing

[HIGH] decoy credentials read (17)  fake secrets planted for this run
  !  SSH                  READ  ~/.ssh/{id_ed25519, config}  by cat
  !  cloud: AWS, k8s      READ  ~/{.aws/credentials, .kube/config}  by cat
  ...

[HIGH] looked for a way to become root (36)  absent ones count: asking is the tell
  !  password files    5  /etc/{gshadow, gshadow-, shadow, shadow-, s...  by newgrp, cat, linpeas.sh
  !  sudo, doas rules  3  /etc/{sudoers, sudoers.d, sudoers.d/*}  by linpeas.sh, grep
  !  setuid search     6  find -perm {-2000, -4000, -g=w, -o=r, -o=w, -u=x}  by find
  *  cron              9  /etc/anacrontab, /etc/cron.d, /etc/cron.d/...  by grep, cat, linpeas.sh, …
  *  kernel hardening  5  /proc/sys/kernel/{kptr_restrict, randomize_va_space...  by cat, linpeas.sh

[HIGH] searched for credentials (30)
  !  x12  find -type d -name .bluemix -o -name doctl -o -name .clau...  in /.cache /applications ...
  !  x10  grep -R -H -i pwd\|passw  in /var/log/alternatives.log /var/log/apt/eipp.log.xz /var/lo...
  !  x4   grep -rl \-\-\-\-\-BEGIN .* PRIVATE KEY\-\-\-\-\-  in /etc /home/ /Users/ /root/ … 7 pl...

[WARN] looked for a VM (48)  absent: not in this VM; asking is the tell
  *  DMI, BIOS          6   /sys/{class/dmi/id/bios_vendor, class/dmi/id/...  by systemd-detect-virt
  *  container markers  4   /.dockerenv, /proc/1/cgroup, /proc/self...  by grep, linpeas.sh, syst...
  *  kernel modules     25  /proc/modules, /sys/module/act_gact, /sys/module...  by linpeas.sh, grep
  *  ACPI tables        4   /sys/firmware/acpi/tables/{APIC, DSDT, FACP, MCFG}  by linpeas.sh

[WARN] tried to reach the network (6)  refused by the host: nothing got out
  *  udp  1.1.1.1:53          x6  DNS; by curl, bash
  *  tcp  169.254.169.254:80  x4  cloud metadata: an instance's credentials; by curl
  *  tcp  104.18.74.230:443   x7  by bash
  ...

ok clean: no sockets, no text addressed to an agent
! audit: the kernel held the code back 4756 ms for audit to keep up: some of what it did may be
   missing

-v every finding, --live as it happens, -o its output, --json for programs
```

That is the plain form, for a pipe or a file (or `NO_COLOR`); on a terminal
the same in color, with symbols. When the report from inside the VM failed,
the verdict is `INCOMPLETE`, and what is inside is unknown, not clean. When
the shell could not find the command or run it (exit 127, 126: `npm` not in
the image, a path that is not there), it is `DID NOT RUN`: the code was not
seen, so nothing in the report says what it does — fix the call. Any other
failure is a warning: what the code would have done past it is not there.
`-v`
prints every finding under its kind, with what it means:

```
[HIGH] decoy credentials read (1)  fake secrets planted for this run
  !  READ  ~/.aws/credentials  by cat ×1; legitimately, the AWS CLI and SDKs read it

[INFO] decoys read by their own tool (1)  each by the program it is for (npm ~/.npmrc, git ~/.netrc): expected
  -  READ  ~/.npmrc  by npm ×2
```

The verdict is in the JSON too (`verdict`: `suspicious`, `review`, `clean`,
`incomplete`, `did_not_run`), with the summary lines counted by grade (`summary.high`,
`.warn`, `.info`).

| Grade | |
|---|---|
| high | a decoy tampered with or deleted, or read by anything but its own tool (or by a program audit did not see); text spelled in Unicode tag characters (a model reads it, a person sees nothing; not a flag's, 🏴 then a region's letters, which emoji use); a request to the sinkhole, or a name looked up, that carries the run's decoy token (a secret sent out); a privesc probe an escalation goes for: `find -perm` for setuid or setgid files, `/etc/shadow`, `/etc/gshadow`, `/etc/sudoers*`, a container runtime's socket, `/proc/PID/mem` |
| warn | a name looked up; a request to the sinkhole; an HTTPS client that refused its certificate; a phrase addressed to an AI agent; a run of zero-width characters, bidirectional controls in code (they deceive a person reading it; the tests of terminals and editors have them); a name looked up; every other privesc probe (cron, root's home, the kernel's switches, `find -perm` for writable files); a VM probe; a connection refused; a file changed outside `~/work` that is not a cache, a tool's settings or a temporary file (`~/.local/bin`, `~/.ssh`, the system); a process left; a listening socket |
| info | a decoy read only by its own tool; `sudo`, `su`, `pkexec`… looked up by name (installers check for sudo); a VM probe ordinary programs make too (`/proc/cpuinfo`, `/proc/self/cgroup`); a cache, a temporary file, a directory whose entries changed |

An alert (`alerts` in the JSON) is graded by its kind. The grades are a
reading aid, not a verdict on the code: the report is what it did.

### What a probe line means

A path in `vm_probes` and `privesc` is the one the code asked for, made
absolute against its working directory (`cd /sys/class/dmi; cat
id/product_name` is `/sys/class/dmi/id/product_name`) and without a trailing
`/`. `absent` means the lookup failed: the path is not there, it is hidden
from the sandbox's user (`/proc/1/cgroup`: other users' processes are hidden),
or a file was asked for as a directory (`/proc/cmdline/`). The program is
named by its file when its process name was cut at 15 characters
(`systemd-detect-virt`, not `systemd-detect-`), and by what its process
started as, not by the thread that did it: threads name themselves (Node's
`libuv-worker`, `MainThread`), and so does code that wants to look like
`kworker`.

| Alert | Severity | When |
|---|---|---|
| `privesc_attempt` | high | a syscall that changes who the process is refused (`setuid(0)`, `setresgid`, `capset`, `setgroups`), or one an ordinary program never makes as a user: `mount`, `chroot`, `pivot_root`, `bpf`, a kernel module, `keyctl`, `userfaultfd`, `perf_event_open`, `personality` turning ASLR off |
| `namespace` | warn | `unshare`, `setns` — browsers' sandboxes (Playwright) do it too |
| `ptrace` | warn | `ptrace` — debuggers do it too |
| `persistence` | high | a write (refused ones too) to `~/.ssh/authorized_keys`, a systemd user unit, `~/.config/autostart`, cron, `/etc`'s startup files; `crontab`, `at`, `systemctl --user enable` run |
| `shell_rc` | warn | a write to `~/.bashrc`, `~/.profile`… — installers (nvm, rustup) do it too |
| `system_write` | warn | any other write to `/etc` |
| `reverse_shell` | high | a shell joined to a connection: `bash -i >& /dev/tcp/…`, `nc -e`, `socat … exec:`, `mkfifo … \| sh -i \| nc`, a Python/Perl socket with a shell, `pty.spawn` |
| `dev_tcp` | warn | bash's `/dev/tcp` or `/dev/udp` otherwise (a connectivity check, a hand-made request); in a script, where no command line shows it, bash itself connecting |
| `pipe_to_shell` | warn | `curl … \| sh`, `sh -c "$(curl …)"` — installers do it too |
| `obfuscated_exec` | high | decoded and run at once: `base64 -d \| sh`, `xxd -r \| sh`, `exec(base64.b64decode(…))` |
| `dropper` | high | a binary written after the sandbox was prepared, by anything but a compiler or linker, in `/tmp`, `/var/tmp` or `/dev/shm`, then run |
| `dropped_exec` | warn | the same elsewhere (`~/work`) |
| `dropped_script` | info | a script written in a temporary directory and run: test suites (pytest's `tmp_path`) and git hooks do it, and what it runs is recorded command by command |
| `antiforensics` | high | history off or cleared (`HISTFILE=/dev/null`, `history -c`), `touch -d/-r` outside `~/work`, `shred`, logs or a dropped program removed |
| `miner` | high | `xmrig`, `stratum+tcp://`, `--donate-level`, a mining pool's name |
| `credential_search` | high | `grep -r`, `find -name`, `locate` for passwords, keys, tokens (`id_rsa`, `.pem`, `AKIA`…) outside `~/work` |
| `io_uring` | warn | `io_uring_setup`: files opened and read through io_uring are not in audit's record. Node would use it (libuv ≥ 1.45); the sandbox turns it off for Node (`UV_USE_IO_URING=0`), so little else does |
| `io_uring_epoll` | info | Node's own ring of 256, which libuv sets up for `epoll_ctl` with io_uring off for files: a native module could still use it unseen |
| `connect` | info | which program tried to reach which address (`:53 (dns)`: a name lookup); loopback left out |
| `audit_health` | warn | audit may have missed some: the disk nearly full (audit stops recording below `admin_space_left`, 150 MB), auditd stopped, the kernel holding the code back for audit to keep up |

Shell builtins (`history -c`, `export HISTFILE=…`) and pipes (`curl … | sh`,
`base64 -d | sh`) show only when they are in a command line run (`bash -c
'…'`), not inside a script file: audit sees programs started, not what a
shell does within. A `/dev/tcp` redirection in a script shows by its effect:
bash connecting. The commands that make alerts are matched on their arguments: code
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

### Live, with `--live`

While the code runs, `mh sandbox --live` looks every 2 seconds and prints each
finding once, the moment it shows: a decoy read, a probe for a VM or for a
way to root, a connection the host refused.

```
== live: what it does, as it happens
    +1s  [WARN]  privesc     find -perm -o=w found  (by find)
    +9s  [HIGH]  privesc     /etc/shadow found  (by newgrp)
   +42s  [HIGH]  decoy       opened /home/dev/.aws/credentials  (by cat)
   +60s  [WARN]  connection  udp 1.1.1.1:53 refused  (egress)
```

Alerts from the image show there too, under their kind. A decoy shows with
the program that opened it (`info` when that is its own tool), or as `READ`
when audit did not see who.

The guest's side is the image's `mh-sandbox-watch`, which reads only what
audit's log gained since the last look. The report at the end is the whole
account; the live view can miss what the log wrote and rotated between two
looks.

If the code is still running when the timeout comes, it is stopped
(SIGSTOP) where it is and looked at: what it left running is still listed.

If the report from inside the VM fails, the rest is still printed — the
output, the exit code, the host's connections — with `"complete": false`
and a warning: everything from inside is then unknown, not empty.

## Its network

The code never reaches the internet; what it reaches for is answered inside
the VM, by the image's `mh-sandbox-net`, and written down:

- **The resolver** (`/etc/resolv.conf` points at it) writes down every name
  looked up and answers an `A` query with an address of its own for each
  name, in `198.18.0.0/15` — a range the VM holds itself (a local route): a
  connection there never reaches the host. Any other type has no answer.
- **The sinkhole**, on ports 80 and 443 of those addresses, answers HTTP
  `200`, empty, and writes down the method, the URL, the size and whether
  this run's decoy token is in it. HTTPS gets a certificate for the name,
  signed by a CA made for this run that the VM trusts (Node and pip are
  pointed at the system's list); a client with a list of its own (Python's
  `certifi`, a pinned key) refuses it, and only the name it asked for shows.
- **Any other port** of a sinkhole address refuses the connection; audit
  says which program tried it, and the report names it by the name it
  looked up (`evil.example:4444`).

It runs as a user of its own (`mhsink`), neither root nor the code's: it
parses what the code sends, and a mistake there must not give the code
root. Code can tell it apart — every name resolves, which on a machine with
no network none would — so evasive code may behave; `--no-sinkhole` answers
no name, as before, and the names are still written down.

## JSON

```json
{
  "target": "./repo",              // as given
  "image": "sandbox:3@sha256:…",
  "vm": "3878e944",
  "kept": false,
  "exit_code": 0,                  // the command's; 124 if it timed out
  "timed_out": false,
  "duration_ms": 4810,             // the command's start to its end
  "complete": true,                // false: the VM's side could not be read
  "verdict": "suspicious",         // suspicious, review, clean, incomplete, did_not_run
  "summary": {
    "high": 2, "warn": 3, "info": 1, // the report's lines, by grade
    "decoys_read": 1,                // touched, by any program
    "vm_probes": 2,
    "evasion_suspected": true,
    "privesc": 3,
    "commands": 41,                // distinct command lines
    "alerts": 1,
    "changed_outside_work": 2,
    "processes_left": 0,
    "listening": 0,
    "connections_refused": 8,
    "dns_names": 2,                // distinct names looked up
    "requests": 1,                 // to the sinkhole, and HTTPS refused
    "secrets_sent": 1,             // requests and names that carried the decoy token
    "addresses_an_agent": 0
  },
  "decoys":      [{"path": "/home/dev/.netrc", "state": "READ", "legitimately": "curl -n, …",
                   "by": ["cat ×1"], "by_its_tool": false, "severity": "high"}],
  "vm_probes":   [{"path": "/sys/class/dmi/id/product_name", "found": false, "count": 1, "by": "python"}],
  "privesc":     [{"path": "find -perm -4000", "found": true, "count": 1, "by": "find"}],
  "commands":    [{"count": 3, "args": "find / -perm -4000"}],
  "alerts":      [{"kind": "shell_rc", "severity": "warn", "count": 1, "what": "/home/dev/.bashrc", "by": "bash"}],
  "changed":     {"files": ["/home/dev/.bashrc"], "dirs": ["/tmp"],
                  "work_files": [{"entry": ".v/", "count": 203}], "work_dirs": [{"entry": ".v/", "count": 33}]},
  "processes":   [{"pid": 123, "args": "…"}],
  "listening":   ["tcp 0.0.0.0:8080"],
  "connections": [{"protocol": "udp", "dst": "1.1.1.1", "dst_port": 53, "count": 4, "reason": "egress"}],
  "net":         "sinkhole",       // sinkhole, servfail (--no-sinkhole), off (an image without it)
  "dns":         [{"count": 4, "type": "A", "name": "evil.example"}],
  "requests":    [{"count": 1, "scheme": "https", "method": "POST", "url": "https://evil.example/collect",
                   "port": 443, "bytes": 2048, "carries_token": true}],
  "tls_refused": [{"count": 1, "name": "pypi.org"}],
  "addresses_an_agent": [{"where": "input", "file": "README.md", "line": 12, "text": "ignore previous instructions", "severity": "warn"}],  // where: input, output, created (a file's name)
  "output": "…",                   // the command's, last 8 KiB
  "warnings": ["audit lost 3 events: vm_probes may be incomplete"],
  "rules":    ["sandbox/rules/example.yml"],                // the --rules files read
  "accepted": [{"kind": "vm_probe", "what": "/sys/firmware/dmi/tables/DMI", "by": "lscpu",
                "count": 1, "why": "our CI prints lscpu"}]  // out of the lists above
}
```

A decoy an accept rule matched stays in `decoys`, graded `info`, with
`"accepted": WHY`. A rule's alert has the kind `rule:NAME`.

Untrusted (the code's choice): `decoys[].by`, `changed.*`, `dns[].name`, `requests[].method` and `.url`, `tls_refused[].name`, `vm_probes[].path` and `.by`,
`privesc[].path` and `.by`, `commands[].args`, `alerts[].what` and `.by`,
`processes[].args`, `addresses_an_agent[].file` and `.text`, `output`,
`accepted[].what` and `.by`.

## Your own rules

What the sandbox looks for can be added to, and what you have checked can be
accepted, in YAML files of yours, named on the command line — from anywhere,
as many as you like, read in order:

```bash
mh sandbox ./install.sh --rules sandbox/rules/example.yml
mh sandbox ./repo 'npm test' --rules ~/team.yml --rules ./this-project.yml
```

No file is read without `--rules`: nothing left in a directory can slip
rules into a run. `sandbox/rules/example.yml` is a starting point.

```yaml
detect:                                  # an alert of your own, titled "your rule NAME"
  - name: brave-passwords                # a-z 0-9 - _ . (at most 40), one per rule
    severity: high                       # high, warn (default) or info
    means: Brave's saved passwords       # optional: shown with it
    path: ~/.config/BraveSoftware/       # opened, run or asked for (access); / at the end: what is inside too
  - name: solana-transfer
    command: 'solana .*transfer'         # a POSIX extended regex (grep -E: no \d, no \b), on every command line
  - name: known-c2
    connect: 203.0.113.0/24              # 1.2.3.4, 10.0.0.0/8, :4444, 1.2.3.4:4444, [2001:db8::1]:443
accept:                                  # findings you checked: graded info, never hidden
  - kind: vm_probe                       # an alert's kind (shell_rc, dropped_exec…), rule:NAME, or
    what: ^/sys/firmware/dmi/            #   decoy, vm_probe, privesc, changed, connection, process, listening
    by: lscpu                            # the program, exactly as the report names it
    why: our CI prints lscpu             # required: whoever reads the report sees it
```

- A rule is a `path`, a `command` or a `connect`, or several of them. A
  `path` is absolute or starts with `~/` (the sandbox user's home); a stat
  alone is not a match, since listing its directory does that.
- An accept rule needs a `what` (a regex, Go's syntax, on what the report
  says with `-v`, `~` for the home) or a `by`, or both: accepting every
  decoy read would accept what the rule is not about. A decoy's `by` is its
  reader when it had only one.
- What an accept rule matches moves under "accepted by your rules", graded
  `info`, with its `why`. It is not dropped, and the verdict no longer
  counts it.
- The rules are checked before anything is created: a mistake costs no VM.
  The report names the files it read.
- They come from the command line only, never from the code under test,
  which could otherwise ship its own accept rules: a `--rules` file inside
  the directory or file under test is refused. In the VM they are root's
  (`/var/lib/mh-sandbox/rules`): the code cannot read them.
- `path` and `command` rules are applied inside the VM, by the image: an
  image older than them says so in a warning (rebuild it). `connect` and
  `accept` rules are the CLI's and work with any image. `--live` shows
  your `detect` rules as they match; it shows findings before your
  `accept` rules, which apply to the report.

## Out of scope

- **Running an agent inside** (to audit a skill by watching an agent use it):
  it needs the model's API reachable and a key inside the VM. Not done.
  Auditing a skill is its text through the scan and its scripts through
  `mh sandbox`.
- **The internet while the code runs**: never — the host's address would be
  the one scanning, spamming or talking to a real command server. What the
  code reached for is in `dns`, `requests` and `connections`.
