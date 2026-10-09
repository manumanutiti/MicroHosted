# mh sandbox — what could come next

Written 2026-10-08. Every improvement to `mh sandbox` weighed so far, with
what it would take and what it would show. Nothing here is built; the order
at the end is a proposal. What `mh sandbox` does today is
`docs/sandbox.md`.

The measure behind all of it: real supply-chain malware run through the
sandbox (`~/own/maldata`, a bench of a few samples per family, each with the
verdict it should get). A sample graded `clean` is a false negative, and a
reason to change something here.

## 1. A security review of the sandbox

**Planned.** Before more is built on it, a review of the sandbox's own
attack surface — the code under test is hostile by definition, and some of
the sandbox reads what it chose:

- `mh-sandbox-net`: parses DNS packets and HTTP requests from the code, as
  `mhsink`; a mistake there is code execution next to the sinkhole's CA key.
- The report's path into the host: `mh-sandbox-report --tsv` and
  `parseSandboxReport` read strings the code chose (names, command lines,
  URLs); their bounds, escaping, and what an agent reading the JSON does
  with them.
- The fetch phase: it runs with the network open and the target already in
  the VM (`--fetch`): a fetch that runs code (a `pip download` of an sdist
  runs `setup.py`) does so with the host's address. Option: fetch in a
  separate VM.
- The shells' trace (`sh.c`, `trace.bash`, the FIFO and its reader): the code
  can write to it; it must not be able to read it back, block it, or forge
  more than extra lines.
- `mh-sandbox-try`, `mh-sandbox-unpack`, `mh-sandbox-sdist`: archives and
  package metadata from the code, unpacked and read as root or as the user.
- The user's rules (`--rules`): regexes run over the code's text.
- What stays of the guest → host channel: the vsock exec reply is bounded
  (roadmap, phase 0); check it still is for every sandbox call.

## 2. Evasion: making the code act as it would on a real machine

Code that finds a sandbox, or waits for the right moment, shows nothing.
Hiding the VM perfectly is not possible; making the cheap tells go away is,
and running twice is the strongest answer (2.6).

### 2.1 The date: `--date`

Time bombs keyed to a date ("after 1 December", "a week after publishing").
Set the VM's clock forward as root before the run (`date -s`), e.g.
`--date +30d` or `--date 2026-12-01`; several runs at several offsets. It
works for every program, static binaries included, since it moves the
system clock. The sinkhole's per-run CA and certificates must be valid
across the offset.

### 2.2 Who and where: environment profiles

Code that acts only in CI, only for some users, only outside some locales:

- `--ci`: `CI=true`, `GITHUB_ACTIONS`, a runner's paths and user, and decoy
  tokens in the environment (`GITHUB_TOKEN`, `NPM_TOKEN`, AWS keys) — opt-in,
  since every program sees the environment.
- Locale and time zone (`LANG`, `TZ`): some code spares some countries.
- User and host names: today `dev`; a profile can vary them.

### 2.3 A home that looks lived in

An empty home is a tell, and gives the decoys little cover: shell history,
a `.gitconfig`, a couple of repositories with `.git`, `known_hosts`, recent
files, editor settings. Content fixed per image, never the user's own.

### 2.4 What the machine looks like

| Tell | What can be done |
|---|---|
| Few CPUs (`nproc`, `os.cpus()`) | Give more vCPUs and keep the host's cost: a vCPU is a host thread, idle when unused, and the VM's `cpu.max` (`internal/jailer/cgroup.go`) can stay at what the sandbox wants to spend. The guest sees 4 or 8 cores; the host spends at most the quota. |
| Little memory (`free`, `os.totalmem()`) | Give more guest memory: Firecracker maps it lazily, so the host pays only for the pages the guest touches. The bound is `memory.max`, today guest memory + 64 MiB: decoupling them (guest 8 GiB, cap 2–3 GiB) means code that really fills memory has its VM killed — report lost — instead of the guest OOM killer acting. Weigh that, or keep the cap and rely on the guest's own OOM ordering (`oom_score_adj`, already set). |
| Short uptime (`/proc/uptime`, `os.uptime()`) | A time namespace: since Linux 5.6, `unshare --time --boottime N` offsets CLOCK_BOOTTIME and CLOCK_MONOTONIC for what runs inside, and `/proc/uptime` and `sysinfo()` follow it. No file mounted over (which `/proc/self/mountinfo` would show). The wall clock is not offset by it: that stays `--date`. To check: the guest kernel has `CONFIG_TIME_NS`, and setpriv/asuser can enter it. |
| MAC of a VM vendor, or locally administered | A MAC with a real NIC vendor's prefix (Intel, Realtek), random per run. |
| Hypervisor bit in CPUID (`/proc/cpuinfo` `hypervisor`, `systemd-detect-virt`) | Firecracker's custom CPU templates can change CPUID. To test: whether the guest kernel still finds kvm-clock without the bit. Experimental. |
| No DMI/SMBIOS (`/sys/class/dmi/id/*`) | Firecracker exposes none (as far as known). Files mounted over show in `mountinfo`. Not planned; the absence stays a tell. |
| `vda` disk (virtio) | Not hideable without another device model. Not planned. |

Faking `/proc/cpuinfo` or `/proc/meminfo` with files mounted over them (as
LXCFS does for containers) is weaker than the above: the mounts show, and
`sched_getaffinity`, `sysconf` and `sysinfo()` still tell the truth.

### 2.5 Waits, said in the report

Code that sleeps past the timeout shows nothing after. Accelerating time is
**not wanted for now** (decided 2026-10-08). The report can at least say
it: a `sleep N` command, or a long `nanosleep`/`clock_nanosleep` audit sees,
as an evasion signal — "it waited 10 minutes: what comes after was not
seen". Node's `setTimeout` waits in `epoll_wait` and does not show this way.

### 2.6 Differential runs

The same sample under variants — today's date and +30 days, plain and
`--ci`, empty home and lived-in — and the timelines compared. Behaviour that
depends on the environment is strong evidence on its own: a legitimate
package does not read credentials only on a given date or only in CI. The
report's times and process tree (`first_ms`, `pid`, since sandbox:30) are
what makes two runs comparable step by step; maldata keeps the variant as a
column.

## 3. Network

- **What the code downloads (second stages, C2), in the same run** —
  design being polished (2026-10-09), not to build before the security
  review (1): its `vsock` and nftables `forward` candidates bear on it.
  Why: Shai-Hulud 2.0 stops without Bun (`bun.sh/install`), tinycolor asks
  for trufflehog's release; most malware's next stage is a download from
  its C2, so no per-tool case (Bun in the image) scales. Rejected: giving
  the VM egress, even through a VPN (Mullvad) — a bot's traffic from our
  connection, real exfiltration channels, the sandbox seen; passing the
  file through the host (`mh cp`: the host parses the guest's framing,
  and a stopped VM's disk with debugfs). The design so far:
  1. The code's GET to a URL the sinkhole does not answer: the sinkhole
     holds the connection and queues the URL. Checked: http(s) only, no
     private or metadata address, not carrying the run's token, at most N
     a run.
  2. A clean, minimal VM (Alpine, a Python server), on the sandbox's
     network, with egress: our curl downloads it — GET, no body, no
     cookies or headers of the code's, a fixed user agent, size and time
     limits. It never runs the code.
  3. Its egress cut, and confirmed (as the fetch phase: unconfirmed,
     nothing goes on).
  4. Only then nft opens one way: the sandbox to its port, nothing else
     (it starts no connection; still no way out).
  5. The sinkhole — mhsink, not the code — fetches it from there and
     serves it on the held connection; its sha256 to the report and
     maldata. The downloader VM dies.
  Still open: the flag (explicit, per run, never by default), N, the
  sizes, how the host learns of a queued URL, clients that time out while
  the downloader boots, a stage that downloads a third. What still gets
  out: the URL and when (the C2 learns something ran it; the data is the
  decoys'), which over time can burn the sandbox for that attacker.
  Supersedes the earlier idea of a replay (`--allow`: run 1 records the
  GETs, run 2's fetch downloads them).
- `--live` shows the sinkhole's requests as they happen (today only
  connections, decoys, probes and alerts).
- Pinned TLS (certifi, a client with its own CA list): only the name is
  seen. Known gap; no plan.

### 3.1 Found by elementary-data 0.23.3 (2026-10-09), for next

- **Done (sandbox:38): a grace period** (`--linger D`, 30 s by default): when the
  command ends with the code's processes still running, keep watching
  until they end or D, then stop them and report. elementary-data's `.pth`
  stealer was still at work (`grep -r` for webhooks) when `run.sh` ended:
  its `trin.tar.gz`, and where it sends it, were never seen. Nothing more
  leaves the VM: the network stays the sinkhole's. Detached payloads
  (`nohup`, `setsid`, `&`) in npm too. Part of 2.5.
- `169.254.170.2` (ECS task credentials) noted as the cloud's metadata,
  as `169.254.169.254` is (`dstNote`).
- "commands it ran": a multi-line `python -c` shows each of its lines as
  a command of its own (the hex blob); one command, its lines together.
- `obfuscated_exec` sees base64 and eval, not a home-made cipher (XOR
  with an md5 keystream, decoded and run by `python -`): the `.pth`
  caught it here; without one it would pass.

## 4. Detection

- **Honeypot CLIs** (level 2 of the AI agent decoys): fake `claude`,
  `gemini`, `codex`, `q`, `gh` on the PATH that log their arguments; an
  `ai_cli_abuse` alert (high) on `--dangerously-skip-permissions`, `--yolo`
  and the like; `gh auth token` answering with a decoy token. s1ngularity
  drove the victim's AI CLIs this way.
- The malware bench as regression tests: every image rebuilt is run over it
  and must keep each sample's verdict.

## 5. The report, for data

- Per-command duration and exit code (audit's `exit_group`); today
  `last_ms` is when a command line last started, not how long it ran.
- Each command's origin — the sandbox, the fetch, the code — so consumers do
  not need patterns to drop the sandbox's own lines.
- Times for what is read after the run: changed files (their ctime), lines
  added to shell startup files, `.pth` hooks.
- The target's sha256 and a `schema_version` in the JSON.
- Times in the person's view (`-v`).

## 6. Targets not yet tried

PyPI sdists (`setup.py`), Go and Rust builds, a skill or plugin repository,
a `curl | sh` that pipes a second stage (needs 3, what the code downloads).

## Proposed order

1. The security review (1).
2. `--date` (2.1) and the lived-in home (2.3).
3. More vCPUs and memory shown than spent, uptime by time namespace, a
   vendor MAC (2.4).
4. `--ci` and the other profiles (2.2); differential runs (2.6).
5. Waits in the report (2.5); the report's data items (5).
6. Honeypot CLIs (4), what the code downloads (3).

Not now: accelerating time (2.5).
