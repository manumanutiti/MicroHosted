# MicroHosted

A self-hosted microVM orchestrator built on **Firecracker + Jailer**, written
in Go. Strong, self-hosted isolation for ephemeral workloads and, as its
current niche, an **IoT/OT isolation gateway at the edge** (x86_64 and aarch64
— Raspberry Pi 4/5, Jetson, ARM gateways).

Every microVM boots in milliseconds with Jailer's real security model
(chroot + cgroups v2 + seccomp), segmented networking with nftables, host↔guest
communication over vsock, and CoW image cloning on btrfs. Everything is driven
through an HTTP API.

## Getting started

- **[quick-setup.md](quick-setup.md)** — from clone to your first microVM in 3
  steps (`make full-install` + `make prepare-image` + `curl`). The `.ext4`
  images and kernels are **not tracked in git**: every machine builds them
  locally.

## Documentation

| Document | What it covers |
|---|---|
| [PROJECT.md](PROJECT.md) | Why it exists, scope, stack, and design decisions |
| [docs/architecture.md](docs/architecture.md) | Architecture and the lifecycle of a microVM |
| [docs/layers.md](docs/layers.md) | Layer map, bottom to top |
| [docs/api.md](docs/api.md) | Complete HTTP API reference (VMs, networks, exec, snapshots/forks, volumes) |
| [docs/networking.md](docs/networking.md) | Segmented networking: bridges, IPAM, egress, quarantine |
| [docs/volumes.md](docs/volumes.md) | Volumes and secure host↔VM data transfer |
| [docs/deploy.md](docs/deploy.md) | Deployment as a systemd service |
| [docs/iot-edge.md](docs/iot-edge.md) | IoT/OT isolation gateway design (current niche) |

## Examples

- [examples/modbus-pull/](examples/modbus-pull/README.md) — a Modbus parser
  isolated inside a microVM (IoT/OT use case).

## Development

```bash
make check     # verify the environment (KVM, cgroups, firecracker, Go)
make build     # build the daemon (CGO_ENABLED=0, cross-compile with ARCH=aarch64)
make test      # go test ./...
make lint      # golangci-lint
```

## License

Licensed under the [Apache License 2.0](LICENSE). See [NOTICE](NOTICE) for
third-party attributions.
