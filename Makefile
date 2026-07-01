BINARY   := microhosted
CMD_DIR  := ./cmd/microhosted
BUILD_DIR := ./build

FC_VERSION ?= v1.10.1
ADDR ?= :8080

.PHONY: all build clean install-fc setup-host kernel rootfs lint test \
        install-service uninstall-service service-logs

all: build

build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY) $(CMD_DIR)

clean:
	rm -rf $(BUILD_DIR)

install-fc:
	chmod +x scripts/install-fc.sh
	./scripts/install-fc.sh $(FC_VERSION)

setup-host:
	chmod +x scripts/setup-host.sh
	sudo ./scripts/setup-host.sh

kernel:
	chmod +x scripts/build-kernel.sh
	./scripts/build-kernel.sh

rootfs:
	chmod +x scripts/build-rootfs.sh
	sudo ./scripts/build-rootfs.sh

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

# Verifica el entorno antes de empezar la Sesión 0
check:
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
