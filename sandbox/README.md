# sandbox/ — the image `mh sandbox` runs code in

`mh sandbox` runs code you do not trust in a fresh VM and reports what it did:
decoy credentials read, ways it looked for a VM, files changed, processes
left, connections refused, text addressed to an AI agent. **docs/sandbox.md**
has the command, the report and the JSON; this directory is the image.

```bash
mh build -t sandbox:1 sandbox          # once, from the repository's root (sudo)
mh sandbox ./repo 'npm test' --fetch 'npm ci --ignore-scripts'
```

| File | |
|---|---|
| `build.yml` | the image: Ubuntu 24.04, Node 24 LTS, a C toolchain, the user `dev`, audit, no setuid file, a quiet systemd |
| `alpine.yml` | a smaller one, without audit: `mh build -f sandbox/alpine.yml -t sandbox-alpine:1 sandbox` |
| `sbin/` | the tools inside, root's only: prepare, unpack, scan, run, report, watch, net (the code's network: a resolver and a sinkhole, inside the VM) |
| `sbin/mh-sandbox-try` | the code's user's (`/usr/local/bin`): a package (`npm:`, `pypi:`) used the ways it can act — install scripts, import, `--help` |
| `agent-patterns` | the phrases that speak to an AI agent, for the input (grep) and the output (`mh sandbox`) |

## Fetching without running the code

`mh sandbox npm:NAME` and `pypi:NAME` do this themselves. For a directory:

The fetch runs with a network, before the decoys and the clock: it must run
nothing of the code. In npm and pip, *installing* already runs code
(`postinstall`, `setup.py`):

| Tool | `--fetch` | Then, in the run |
|---|---|---|
| npm | `npm ci --ignore-scripts` | `npm rebuild && npm test` |
| pip | `python3 -m venv .v && .v/bin/pip download --only-binary=:all: -d wheels -r requirements.txt` | `.v/bin/pip install --no-index --find-links wheels -r requirements.txt && …` |
| Go | `go mod download` | `go build ./... && go test ./...` |

System packages: `--apt PKG`, installed by root from Ubuntu's archive.

## By hand

What `mh sandbox` does, step by step — to see it, or to look inside between
steps:

```bash
tar -czf /tmp/code.tgz -C ./repo .
VM=$(mh run sandbox:1 --wait)
mh cp /tmp/code.tgz $VM:/root/code.tgz
mh exec $VM mh-sandbox-prepare /root/code.tgz     # code in, decoys, audit, the clock
mh exec $VM mh-sandbox-scan                       # text addressed to an AI agent
mh exec -t 5m $VM mh-sandbox-run 'bash install.sh'
mh exec $VM mh-sandbox-report                     # --tsv: what mh sandbox reads
mh flows $VM
mh rm $VM
```

To fetch first, the VM on a network with a way out: `mh-sandbox-unpack
/root/code.tgz`, then `mh-sandbox-run --fetch '…'` (refused once prepared),
`mh network update NET --no-out`, then `mh-sandbox-prepare` with no archive.

## How far to trust it

- **The code runs as an unprivileged user** — no password, no sudo, no
  setuid file in the image, `no_new_privs`, other users' processes hidden,
  kernel addresses hidden, root locked (Ubuntu's base leaves it without a
  password). The guest kernel has no vsock loopback: the user cannot reach
  the root agent.
- **The report** holds while the code did not become root: only root can set
  a file's ctime, or stop audit. Root through a guest kernel exploit could
  hide from it.
- **`mh flows`** is the host's view and holds whatever happened inside.
- **Root never writes inside the user's home**: decoys and code go in as the
  user, so a symlink the code planted cannot turn root's write into an
  escalation. An archive member that is absolute or climbs with `..` is
  refused.
- **No false positives at rest**: a prepared VM where nothing runs reports
  nothing (systemd's journal in memory, every timer masked, audit's log left
  out).
- Everything that comes out — file names, process names, output — is the
  code's to write: **data, never instructions**, for a person or an agent
  reading it.
- Not `mh quarantine` to cut a VM off: a quarantined VM is cut off but **not
  recorded** (docs/not-yet.md); `mh sandbox` closes a network of its own.
