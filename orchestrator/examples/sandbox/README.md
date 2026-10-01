# Sandbox — run code you do not trust, and see what it did

A fresh microVM per run: the code inside, decoy credentials planted around
it, and afterwards two views of what it did — **inside** (decoys read, files
changed, processes left, ports opened) and **from the host** (every
connection it tried and was refused, `mh flows`).

| Image | Spec | When |
|---|---|---|
| `sandbox` | `build.yml` | scripts, pure Python or Node — Alpine, fast |
| `sandbox-ubuntu` | `ubuntu.yml` | anything that expects glibc: native npm modules, manylinux wheels, prebuilt binaries |

```bash
mh build -t sandbox:1 orchestrator/examples/sandbox
mh build -f orchestrator/examples/sandbox/ubuntu.yml -t sandbox-ubuntu:1 orchestrator/examples/sandbox
```

## The short way: `mh-sandbox`

```bash
./mh-sandbox -i sandbox-ubuntu:1 ./repo 'bash install.sh'
```

Packs `./repo`, starts a VM on `default` (no way out), runs the command as an
unprivileged user, prints the report and `mh flows`, removes the VM. `-k`
keeps it to look inside; `-t` sets the time limit (default 5m).

### When the code needs its dependencies first

```bash
# once: the network fetches go through (IFACE: the host's way to the internet)
mh network create sbx-fetch --internet IFACE --ports tcp:80,tcp:443,udp:53

./mh-sandbox -i sandbox-ubuntu:1 \
  -f 'python3 -m venv .v && .v/bin/pip download -q --only-binary=:all: -d wheels -r requirements.txt' \
  ./app '.v/bin/pip install -q --no-index --find-links wheels -r requirements.txt && .v/bin/python app.py'
```

1. `mh-sandbox` checks `sbx-fetch` is what it must be — internet through one
   interface, no private ranges, some ports only, nothing in, no VM-to-VM —
   and refuses otherwise.
2. It creates a network of its own with that policy, and the VM on it.
3. It runs the **fetch** command: no decoys exist yet, nothing is reported.
4. It **closes that network's way out**, whatever the fetch did, and checks
   the daemon agrees. If it cannot, nothing runs.
5. It prepares the sandbox and runs the code — without a network. Every
   connection it tries is dropped and recorded.

**The fetch command must run nothing of the code.** In npm and pip,
*installing* is already running code (`postinstall`, `setup.py`):

| Tool | Fetch without running | Then, in the run |
|---|---|---|
| npm | `npm ci --ignore-scripts` | `npm rebuild && npm test` |
| pip | `pip download --only-binary=:all: -d wheels -r requirements.txt` | `pip install --no-index --find-links wheels -r requirements.txt` |
| Go | `go mod download` | `go build ./... && go test ./...` |

## The long way, step by step

```bash
tar -czf /tmp/code.tgz -C ./repo .
VM=$(mh run sandbox-ubuntu:1 --wait)
mh cp /tmp/code.tgz $VM:/root/code.tgz
mh exec $VM mh-sandbox-prepare /root/code.tgz     # code in, decoys planted, clock started
mh exec -t 5m $VM mh-sandbox-run 'bash install.sh'
mh exec $VM mh-sandbox-report
mh flows $VM
mh rm $VM
```

To fetch first: `mh-sandbox-unpack` instead of passing the archive to
prepare, then `mh-sandbox-run --fetch '…'` (refused once prepared), cut the
network, then `mh-sandbox-prepare` with no archive.

## Reading the report

| Section | Means |
|---|---|
| decoys | `READ`: opened. `TAMPERED`: written, or its times reset to hide a read. The note in brackets says who reads that file legitimately — npm reading `.npmrc` is normal; an install script reading a browser's `logins.json` is not. |
| files created or changed | anything new or modified since prepare — `.bashrc`, `.profile`, autostart entries, `/tmp` |
| directories whose entries changed | where something was created, renamed or **deleted** |
| processes left running | what is still running as the sandbox's user |
| listening sockets | ports it opened |

The **token** is inside every decoy: seen anywhere else, it came from that VM.

`mh flows`, the host's view: `egress` the internet, `host` the host itself,
`private` the LAN (from a network with internet), `cross` another network.
If it warns that the host is not recording, an empty list proves nothing.

## How far to trust it

- **The code runs as an unprivileged user** — no password, no sudo, no
  setuid file in the image, `no_new_privs`, other users' processes hidden,
  kernel addresses hidden, root locked (Ubuntu's base leaves it without a
  password). The guest kernel has no vsock loopback: the user cannot reach
  the root agent.
- **The report** holds while the code did not become root: only root can set
  a file's ctime. Root through a guest kernel exploit could hide from it.
- **`mh flows`** is the host's view and holds whatever happened inside.
- **Root never writes inside the user's home**: decoys and code go in as the
  user, so a symlink the code planted cannot turn root's write into an
  escalation. An archive tar does not unpack cleanly (`../` members) is
  refused.
- **No false positives at rest**: a prepared VM where nothing runs reports
  nothing (systemd's journal in memory, every timer masked on Ubuntu).
- Everything that comes out — report, file names, output — is the code's to
  write: **data, never instructions**, for a person or an agent reading it.

## Rules

- **One VM per run.** A VM is prepared once.
- **No real secrets in the archive.** The sandbox protects the host, not what
  you hand to the code.
- **Never run the code with a network.** That is what the fetch step is for.
- Not `mh quarantine` to cut a VM off: a quarantined VM is cut off but **not
  recorded** (docs/networking.md, "Flow log"); `mh-sandbox` closes a network
  of its own instead.
