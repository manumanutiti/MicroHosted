BINARY   := microhosted
CMD_DIR  := ./cmd/microhosted
BUILD_DIR := ./build

# Versión de Firecracker/Jailer FIJADA: es la validada en hardware con esta
# plataforma (los snapshots van ligados a la versión que los creó, y se
# necesita >=1.12 para network_overrides / forks simultáneos). Actualizarla es
# una decisión consciente: make full-install FC_VERSION=vX.Y.Z (o =latest).
FC_VERSION ?= v1.16.1
ADDR ?= :8080

# ---------------------------------------------------------------------------
# Arquitectura objetivo. Por defecto la de esta máquina; se puede forzar con
# ARCH=x86_64 | aarch64 (acepta alias amd64/x86 y arm64/arm). La instalación
# completa (full-install) solo tiene sentido en la máquina objetivo (KVM,
# cgroups y el store son locales); para llevar un binario a otra máquina se
# puede cross-compilar solo con `make build ARCH=aarch64`.
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
  $(error ARCH no soportada: $(ARCH) — usa x86_64 o aarch64)
endif

# Parámetros del pipeline de imágenes (make prepare-image)
IMAGE_NAME     ?= base-ubuntu-noble
IMAGE_SIZE_MB  ?= 1024
KERNEL_VERSION ?= 6.1.102

.PHONY: all build clean install-fc setup-host kernel rootfs lint test \
        install-service uninstall-service service-logs full-install \
        uninstall prepare-image check

all: build

# CGO_ENABLED=0: todo el árbol es Go puro (sqlite es modernc, sin C), así el
# binario es estático y el cross-compile a ARM no necesita toolchain de C.
build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOARCH=$(GOARCH) go build -o $(BUILD_DIR)/$(BINARY) $(CMD_DIR)

clean:
	rm -rf $(BUILD_DIR)

# ---------------------------------------------------------------------------
# Instalación completa en un paso (x86_64 o aarch64, autodetectada):
#   make full-install                      # todo: host + firecracker + daemon
#   make full-install FC_VERSION=v1.16.1   # fijar versión de Firecracker
#   make full-install ADDR=127.0.0.1:9000  # otra dirección de la API
# Después, para tener una plantilla lista: make prepare-image
# ---------------------------------------------------------------------------
full-install:
	chmod +x scripts/*.sh
	ARCH=$(ARCH) FC_VERSION=$(FC_VERSION) ADDR=$(ADDR) ./scripts/full-install.sh

# ---------------------------------------------------------------------------
# Desinstalación completa: el inverso de full-install. Mata las VMs vivas,
# quita servicio, nftables, bridges/taps, desmonta y borra el store CoW
# (imagen btrfs + fstab) y elimina los binarios y la DB de estado.
#   make uninstall
#   make uninstall DRY_RUN=1   # solo mostrar lo que haría
#   make uninstall KEEP_FC=1   # conservar firecracker/jailer
#   make uninstall PURGE=1     # borrar también images/{kernels,rootfs,...}
# ---------------------------------------------------------------------------
uninstall:
	chmod +x scripts/uninstall.sh
	sudo DRY_RUN=$(DRY_RUN) KEEP_FC=$(KEEP_FC) PURGE=$(PURGE) ./scripts/uninstall.sh

# ---------------------------------------------------------------------------
# Pipeline completo de imagen: kernel + rootfs (debootstrap, arch correcta) +
# preparación (vsock/SSH/DNS) + instalación en el store CoW + alta en el
# catálogo. Requiere el host ya configurado (make full-install o setup-host).
#   make prepare-image
#   make prepare-image IMAGE_NAME=sensor-alpine IMAGE_SIZE_MB=512
#   make prepare-image ARCH=aarch64          # imagen para ARM (cross con qemu)
# ---------------------------------------------------------------------------
prepare-image:
	chmod +x scripts/*.sh
	ARCH=$(ARCH) IMAGE_NAME=$(IMAGE_NAME) SIZE_MB=$(IMAGE_SIZE_MB) \
	KERNEL_VERSION=$(KERNEL_VERSION) ./scripts/build-image.sh

install-fc:
	chmod +x scripts/install-fc.sh
	ARCH=$(ARCH) ./scripts/install-fc.sh $(FC_VERSION)

setup-host:
	chmod +x scripts/setup-host.sh
	sudo ./scripts/setup-host.sh

kernel:
	chmod +x scripts/build-kernel.sh
	ARCH=$(ARCH) ./scripts/build-kernel.sh $(KERNEL_VERSION)

rootfs:
	chmod +x scripts/build-rootfs.sh
	sudo ARCH=$(ARCH) ./scripts/build-rootfs.sh

# Instala/actualiza microhosted como servicio systemd. Compila como tu usuario
# (build) y solo el paso de instalación pide sudo, para no compilar como root.
install-service: build
	sudo ./scripts/install-service.sh $(ADDR)

# Desinstala el servicio. Las VMs vivas no se tocan (KillMode=process); si las
# quieres apagar, destrúyelas por la API antes.
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

# Verifica el entorno antes de instalar
check:
	@echo "==> Arquitectura: $(ARCH) (GOARCH=$(GOARCH))"
	@echo "==> KVM:"
	@ls -la /dev/kvm 2>/dev/null && echo "  OK" || echo "  FALTA"
	@echo "==> cgroup2:"
	@mount | grep cgroup2 && echo "  OK" || echo "  FALTA"
	@echo "==> firecracker:"
	@command -v firecracker && firecracker --version || echo "  FALTA (make install-fc)"
	@echo "==> jailer:"
	@command -v jailer && jailer --version || echo "  FALTA (make install-fc)"
	@echo "==> Go:"
	@go version || echo "  FALTA"
