# MicroHosted

**Docker and Docker Compose, but every container is a microVM.**

MicroHosted is a self-hosted microVM engine and orchestrator built on
**Firecracker + Jailer**, written in Go. You describe an image in a
`build.yml`, a set of services in a `microse.yml`, and drive them with the
verbs you already know — `mh build`, `mh run`, `mh exec`, `mh up`, `mh down` —
except that each workload gets its own kernel instead of a shared one.

Every microVM boots in milliseconds inside Jailer's real security model
(chroot + cgroups v2 + seccomp), on segmented networks enforced with nftables
(nothing reaches anything unless the network says so), talks to the host over
host-initiated vsock, and is cloned copy-on-write on btrfs. It runs on x86_64
and aarch64 — from a workstation to an ARM64 board.

## What it looks like

An image, `build.yml` (Alpine or Ubuntu base, packages, files, commands — built
in cached layers like a Dockerfile):

```yaml
name: web
base: alpine:3.22
packages: [nginx]
files:
  /srv/www/: site/
  /etc/nginx/http.d/default.conf: nginx.conf
run:
  - nginx -t
command: nginx -g 'daemon off;'
health: { command: "wget -qO- http://127.0.0.1:8080/", every: 10s }
```

A project, `microse.yml` (networks and services, like a compose file):

```yaml
version: 1
budget: { max_vms: 2, max_mem_mb: 512 }   # a hard cap: room for a replacement during an update
networks:
  web-demo: { subnet: 172.30.30.0/24 }
functions:
  site:
    build: .                     # built by mh build, pinned by digest
    network: web-demo
    ip: 172.30.30.10
    lifecycle: { mode: persistent }
```

```bash
mh up -d          # build what is missing, create, keep it running
mh status         # services, VMs, health
mh apply          # roll out a change, one service at a time, with rollback
mh down           # remove everything the project created
```

## What you can do with it

- **Multi-service stacks** — a database, an API and a front end, each in its
  own VM on a private network ([stack](orchestrator/examples/stack/microse.yml)).
- **Labs and test environments** — a control node and five machines to test an
  Ansible playbook, a school network with DNS, FTP, MQTT and NTP
  ([examples](orchestrator/examples/README.md)).
- **CI runners** — one GitHub Actions job per fresh VM, Docker included
  ([github-runner](orchestrator/examples/github-runner/microse.yml)).
- **Untrusted code** — `mh sandbox` runs an installer, a package, a repository
  or an MCP server in a throwaway VM with decoy credentials and the network cut,
  and reports what it did ([docs/sandbox.md](docs/sandbox.md)). There is also an
  [agent skill](skills/mh-sandbox/SKILL.md) for it.
- **Ephemeral and scheduled work** — besides always-on services, a service can
  be a fresh VM every N seconds that runs one command and is destroyed
  (`transaction`), or runs for a fixed window (`window`).
- **Isolation at the edge** — e.g. a protocol parser for industrial devices
  kept in a disposable VM instead of on the gateway
  ([modbus-pull](examples/modbus-pull/README.md), [docs/iot-edge.md](docs/iot-edge.md)).

## Getting started

```bash
git clone <repo-url> && cd MicroHosted
make full-install                 # host packages, Firecracker, the daemon as a systemd service, mh
newgrp microhosted                # once: the group the installer added you to (or log in again)

cd orchestrator/examples/hello
mh up                             # builds the image the first time (sudo), then runs; Ctrl-C to stop
```

Requirements: Linux x86_64 or aarch64 with KVM (`/dev/kvm`), cgroups v2,
Go 1.21+ (it fetches the 1.25 the build needs). `make check` verifies them;
`make full-install` installs the rest with apt. Images and kernels are not in git: every
machine builds its own. **[quick-setup.md](quick-setup.md)** walks through it
step by step, and **[orchestrator/examples/](orchestrator/examples/README.md)**
lists every example with what it shows.

## How it is put together

| Piece | Role | Docker equivalent |
|---|---|---|
| `microhosted` | the engine: VMs, networks, volumes, images, snapshots, events, over an HTTP API on a Unix socket | `dockerd` |
| `mh` | the command-line client | `docker` |
| `mh build` + `build.yml` | builds an image in cached layers and pins it by digest | `docker build` + Dockerfile |
| `mh up` + `microse.yml` | keeps a project in the declared state: creates, health-checks, replaces, rolls out updates | `docker compose` |
| `mh sandbox` | runs code you do not trust and reports its behaviour | — |

## Documentation

| Document | What it covers |
|---|---|
| **[docs/engine.md](docs/engine.md)** | **Start here.** The engine by example: commands with real output, addresses, quarantine, replace, events |
| [docs/cli.md](docs/cli.md) | `mh`: every command and flag, `mh build`, `mh up`, `mh sandbox` |
| [orchestrator/README.md](orchestrator/README.md) | Projects: `microse.yml`, images, lifecycle modes, updates |
| [docs/uses.md](docs/uses.md) | Uses, step by step with measured results (a static website behind nginx) |
| [docs/sandbox.md](docs/sandbox.md) | `mh sandbox`: targets, the report, the JSON |
| [docs/threat-model.md](docs/threat-model.md) | What is protected, from whom, the security layers and their known gaps |
| [docs/architecture.md](docs/architecture.md) | Architecture and the lifecycle of a microVM |
| [docs/layers.md](docs/layers.md) | Layer map, bottom to top |
| [docs/api.md](docs/api.md) | The HTTP API (VMs, networks, exec, snapshots/forks, volumes, images) |
| [docs/networking.md](docs/networking.md) | Segmented networking: bridges, IPAM, ingress/egress, quarantine |
| [docs/volumes.md](docs/volumes.md) | Volumes and host↔VM data transfer |
| [docs/development-environments.md](docs/development-environments.md) | Disposable workstations: Docker inside a microVM (exploratory) |
| [docs/deploy.md](docs/deploy.md) | Deployment as a systemd service |
| [docs/orchestrator.md](docs/orchestrator.md) | Orchestrator design: lifetimes, redundancy, bounded VM budget |
| [docs/roadmap.md](docs/roadmap.md) | Where the project stands and what comes next |
| [docs/not-yet.md](docs/not-yet.md) | Features weighed and deferred, with the reasoning |
| [docs/iot-edge.md](docs/iot-edge.md), [docs/ingestion.md](docs/ingestion.md) | The IoT/OT edge gateway use case |
| [PROJECT.md](PROJECT.md) | Origin, stack and early design decisions |

## Development

```bash
make check     # verify the environment (KVM, cgroups, firecracker, Go)
make build     # build microhosted, mh and mh-orchestrator (CGO_ENABLED=0, cross-compile with ARCH=aarch64)
make test      # go test ./...
make lint      # golangci-lint
```

## License

Licensed under the [Apache License 2.0](LICENSE). See [NOTICE](NOTICE) for
third-party attributions.
