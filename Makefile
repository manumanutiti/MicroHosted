BINARY   := microhosted
CMD_DIR  := ./cmd/microhosted
# mh: the docker-style CLI client (internal/cli, docs/cli.md).
CLI      := mh
CLI_DIR  := ./cmd/mh
# mh-orchestrator: the declarative orchestrator, an engine API client
# (orchestrator/, docs/orchestrator.md). make install-cli installs it with mh.
ORCH     := mh-orchestrator
ORCH_DIR := ./orchestrator/cmd/mh-orchestrator
BUILD_DIR := ./build

# PINNED Firecracker/Jailer version: the one validated on hardware with this
# platform (snapshots are tied to the version that created them, and >=1.12 is
# needed for network_overrides / simultaneous forks). Changing it is a conscious
# decision: make full-install FC_VERSION=vX.Y.Z (or =latest).
FC_VERSION ?= v1.16.1
# Empty ADDR: the API serves on a Unix socket (SOCKET), whose file permissions
# ARE its authorization — the API grants root-equivalent control of the host
# (create a VM, exec in it, set a network's egress), so putting it on a port is
# a conscious decision: make install-service ADDR=127.0.0.1:8080
ADDR ?=
SOCKET ?= /run/microhosted.sock
# Group allowed to drive the daemon without sudo. Empty: a first install
# creates microhosted and adds you to it (root-equivalent, as the docker
# group); a reinstall keeps what the unit has. none: root-only.
SOCKET_GROUP ?=
# Interfaces whose whole nftables policy the daemon owns (comma-separated).
# Declaring one denies it in both directions except for the egress and ingress
# rules that name it — see docs/networking.md § Managed interfaces — so remove any other
# ruleset covering it first. MANAGED_HOST_ALLOW picks which host services stay
# reachable from them (default udp/67 for DHCP; "none" denies every one).
MANAGED_IFACE ?=
MANAGED_HOST_ALLOW ?=
# Size in GB of the btrfs copy-on-write store a first install creates (a sparse
# file: it takes on disk only what is written). Empty: setup-host.sh's default,
# 20. An existing store keeps its size.
COW_SIZE_GB ?=

# ---------------------------------------------------------------------------
# Target architecture. Defaults to this machine's; can be forced with
# ARCH=x86_64 | aarch64 (accepts the aliases amd64/x86 and arm64/arm). The full
# installation (full-install) only makes sense on the target machine (KVM,
# cgroups, and the store are local); to carry a binary to another machine you
# can cross-compile just it with `make build ARCH=aarch64`.
# ---------------------------------------------------------------------------
ARCH ?= $(shell uname -m)
ifeq ($(ARCH),amd64)
  override ARCH := x86_64
endif
ifeq ($(ARCH),x86)
  override ARCH := x86_64
endif
ifeq ($(ARCH),arm64)
  override ARCH := aarch64
endif
ifeq ($(ARCH),arm)
  override ARCH := aarch64
endif

GOARCH_x86_64  := amd64
GOARCH_aarch64 := arm64
GOARCH := $(GOARCH_$(ARCH))
ifeq ($(GOARCH),)
  $(error Unsupported ARCH: $(ARCH) — use x86_64 or aarch64)
endif

# Image pipeline parameters (make prepare-image).
# FLAVOR: alpine (ultra-minimal, default) | ubuntu (noble, systemd + SSH) |
#         ubuntu-docker (noble + Docker Engine, development template dev-ubuntu).
# Empty IMAGE_NAME/IMAGE_SIZE_MB → the flavor's default (base-alpine 128MB /
# base-ubuntu-noble 1024MB), resolved by build-image.sh.
# IMAGE_VERSION: the image tag's version; empty → the build time. IMPORT=0
# stops at the template (no image, no digest).
FLAVOR         ?= alpine
IMAGE_NAME     ?=
IMAGE_VERSION  ?=
IMPORT         ?= 1
IMAGE_SIZE_MB  ?=
KERNEL_VERSION ?= 6.1.102
EXTRA_PKGS     ?=

.PHONY: all build clean install-cli install-fc setup-host kernel rootfs lint test \
        install-service uninstall-service service-logs full-install \
        uninstall prepare-image check

all: build

# CGO_ENABLED=0: the whole tree is pure Go (sqlite is modernc, no C), so the
# binary is static and cross-compiling to ARM needs no C toolchain.
build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOARCH=$(GOARCH) go build -o $(BUILD_DIR)/$(BINARY) $(CMD_DIR)
	CGO_ENABLED=0 GOARCH=$(GOARCH) go build -o $(BUILD_DIR)/$(CLI) $(CLI_DIR)
	CGO_ENABLED=0 GOARCH=$(GOARCH) go build -o $(BUILD_DIR)/$(ORCH) $(ORCH_DIR)

clean:
	rm -rf $(BUILD_DIR)

# Installs the clients: mh, and mh-orchestrator, which mh up/down/plan… run
# (install-service, and so full-install, installs both too).
install-cli: build
	sudo install -o root -g root -m 0755 $(BUILD_DIR)/$(CLI) /usr/local/bin/$(CLI)
	sudo install -o root -g root -m 0755 $(BUILD_DIR)/$(ORCH) /usr/local/bin/$(ORCH)

# ---------------------------------------------------------------------------
# One-step full installation (x86_64 or aarch64, auto-detected):
#   make full-install                      # everything: host + firecracker + daemon
#   make full-install FC_VERSION=v1.16.1   # pin the Firecracker version
#   make full-install SOCKET_GROUP=none      # socket root-only (default: group microhosted, you in it)
#   make full-install ADDR=127.0.0.1:8080      # serve on a port instead
#   make full-install COW_SIZE_GB=40           # a 40 GB store (default 20; first install only)
# Afterward: cd orchestrator/examples/hello && mh up
# (make prepare-image only for catalog templates: mh run base-alpine)
# ---------------------------------------------------------------------------
full-install:
	chmod +x scripts/*.sh
	ARCH=$(ARCH) FC_VERSION=$(FC_VERSION) ADDR=$(ADDR) SOCKET=$(SOCKET) SOCKET_GROUP=$(SOCKET_GROUP) MANAGED_IFACE=$(MANAGED_IFACE) MANAGED_HOST_ALLOW=$(MANAGED_HOST_ALLOW) COW_SIZE_GB=$(COW_SIZE_GB) ./scripts/full-install.sh

# ---------------------------------------------------------------------------
# Full uninstallation: the inverse of full-install. Kills the live VMs, removes
# the service, nftables, bridges/taps, unmounts and deletes the CoW store
# (btrfs image + fstab), and removes the binaries and the state DB.
#   make uninstall
#   make uninstall DRY_RUN=1   # only show what it would do
#   make uninstall KEEP_FC=1   # keep firecracker/jailer
#   make uninstall PURGE=1     # also delete images/{kernels,rootfs,...}
# ---------------------------------------------------------------------------
uninstall:
	chmod +x scripts/uninstall.sh
	sudo DRY_RUN=$(DRY_RUN) KEEP_FC=$(KEEP_FC) PURGE=$(PURGE) ./scripts/uninstall.sh

# ---------------------------------------------------------------------------
# Complete image pipeline: kernel + rootfs (correct arch, cross with qemu) +
# preparation (vsock/SSH/DNS) + installation into the CoW store + registration
# in the catalog + import into the image store, which prints the pinned
# reference (name:version@sha256:…) a project spec needs. Requires the host already configured (make full-install or
# setup-host). By default it builds the ultra-minimal Alpine (base-alpine,
# ~10 MB, vsock exec, no systemd); FLAVOR=ubuntu for the classic noble.
#   make prepare-image
#   make prepare-image EXTRA_PKGS=python3 IMAGE_NAME=alpine-py IMAGE_VERSION=1.1
#   make prepare-image FLAVOR=ubuntu         # base-ubuntu-noble (systemd+SSH)
#   make prepare-image FLAVOR=ubuntu-docker  # dev-ubuntu (noble + Docker)
#   make prepare-image ARCH=aarch64          # image for ARM (cross with qemu)
# ---------------------------------------------------------------------------
prepare-image:
	chmod +x scripts/*.sh
	ARCH=$(ARCH) FLAVOR=$(FLAVOR) IMAGE_NAME=$(IMAGE_NAME) SIZE_MB=$(IMAGE_SIZE_MB) \
	IMAGE_VERSION=$(IMAGE_VERSION) IMPORT=$(IMPORT) KERNEL_VERSION=$(KERNEL_VERSION) EXTRA_PKGS="$(EXTRA_PKGS)" ./scripts/build-image.sh

install-fc:
	chmod +x scripts/install-fc.sh
	ARCH=$(ARCH) ./scripts/install-fc.sh $(FC_VERSION)

setup-host:
	chmod +x scripts/setup-host.sh
	sudo COW_SIZE_GB=$(COW_SIZE_GB) ./scripts/setup-host.sh

kernel:
	chmod +x scripts/build-kernel.sh
	ARCH=$(ARCH) ./scripts/build-kernel.sh $(KERNEL_VERSION)

rootfs:
	chmod +x scripts/build-rootfs.sh
	sudo ARCH=$(ARCH) ./scripts/build-rootfs.sh

# Installs/updates microhosted as a systemd service. Builds as your user (build)
# and only the install step asks for sudo, so as not to build as root.
install-service: build
	sudo SOCKET=$(SOCKET) SOCKET_GROUP=$(SOCKET_GROUP) MANAGED_IFACE=$(MANAGED_IFACE) MANAGED_HOST_ALLOW=$(MANAGED_HOST_ALLOW) ./scripts/install-service.sh $(ADDR)

# Uninstalls the service. Live VMs aren't touched (KillMode=process); if you want
# to power them off, destroy them via the API first.
uninstall-service:
	-sudo systemctl disable --now microhosted
	sudo rm -f /etc/systemd/system/microhosted.service
	sudo systemctl daemon-reload

service-logs:
	journalctl -u microhosted -f

lint:
	golangci-lint run ./...

test:
	go test -v ./...
	# the sandbox's network (sandbox/net), the image's Python
	python3 -m unittest discover -s sandbox/tests/net

# Verify the environment before installing
check:
	@echo "==> Architecture: $(ARCH) (GOARCH=$(GOARCH))"
	@echo "==> KVM:"
	@ls -la /dev/kvm 2>/dev/null && echo "  OK" || echo "  MISSING"
	@echo "==> cgroup2:"
	@mount | grep cgroup2 && echo "  OK" || echo "  MISSING"
	@echo "==> firecracker:"
	@command -v firecracker && firecracker --version || echo "  MISSING (make install-fc)"
	@echo "==> jailer:"
	@command -v jailer && jailer --version || echo "  MISSING (make install-fc)"
	@echo "==> Go (any from 1.21 fetches the $$(awk '$$1 == "go" { print $$2; exit }' go.mod) the build needs):"
	@go version || echo "  MISSING (sudo snap install go --classic, or https://go.dev/dl/)"
	@echo "==> Host tools (make full-install installs the missing ones with apt):"
	@for c in ip nft curl mkfs.ext4 python3 debootstrap; do \
	  if command -v $$c >/dev/null || [ -x /usr/sbin/$$c ] || [ -x /sbin/$$c ]; then echo "  $$c: OK"; else echo "  $$c: MISSING"; fi; \
	done
	@[ -e /usr/share/keyrings/ubuntu-archive-keyring.gpg ] && echo "  ubuntu-keyring: OK" || echo "  ubuntu-keyring: MISSING (only for Ubuntu-based images)"
