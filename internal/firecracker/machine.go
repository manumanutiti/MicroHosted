package firecracker

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"

	"microhosted/pkg/types"
)

const defaultKernelArgs = "console=ttyS0 reboot=k panic=1 pci=off"

// defaultNameservers are handed to every networked guest via the SDK's
// IPConfiguration.Nameservers (see BuildConfig). Public resolvers, not the
// host's own — the host's resolver (e.g. systemd-resolved's 127.0.0.53 stub)
// would be unreachable anyway, since guest→host is dropped by design (see
// docs/networking.md). These only resolve anything on a network with
// egress:true; on a no-egress network DNS queries just time out the same way
// any other WAN traffic does, which is correct.
var defaultNameservers = []string{"1.1.1.1", "8.8.8.8"}

// VsockDevicePath is where Firecracker creates the vsock UDS, relative to
// its own (chrooted) view of the filesystem. internal/vsock connects to it
// from the host by joining this with jailer.WorkspaceRoot(id) — the SDK
// doesn't rewrite VsockDevice.Path into the chroot for us the way it does
// for the API socket.
const VsockDevicePath = "v.sock"

// vsockGuestCID is the guest-side CID Firecracker's vsock device presents.
// It's local to the VM's own vsock namespace (isolation on the host side
// comes from which UDS path you dial, not from CID uniqueness), so every VM
// can safely use the same value — this is Firecracker's own convention.
const vsockGuestCID = 3

// BuildConfig assembles the firecracker.Config for one VM: boot source, root
// drive, machine sizing, network interface (if a TAP device was assigned)
// and the jailer configuration that will actually launch it.
func BuildConfig(vm types.VMConfig, jcfg fc.JailerConfig) (fc.Config, error) {
	cfg := fc.Config{
		VMID:            vm.ID,
		KernelImagePath: vm.Kernel,
		KernelArgs:      defaultKernelArgs,
		// Do NOT forward signals from the daemon to the VM. The SDK's default
		// (nil) installs a handler that forwards SIGINT/SIGTERM/SIGHUP/... from
		// this process straight to the Firecracker child — so a `systemctl
		// stop`/`restart` (SIGTERM to the daemon) would kill every VM, and the
		// whole point of persistence+reconcile is that VMs OUTLIVE the daemon.
		// An empty (non-nil) slice disables forwarding entirely; the VM's
		// lifetime is the manager's, ended only by an explicit Destroy.
		ForwardSignals: []os.Signal{},
		Drives: []models.Drive{{
			DriveID:      fc.String("rootfs"),
			PathOnHost:   fc.String(vm.Rootfs),
			IsRootDevice: fc.Bool(true),
			IsReadOnly:   fc.Bool(false),
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  fc.Int64(vm.VCPUs),
			MemSizeMib: fc.Int64(vm.MemMB),
		},
		JailerCfg: &jcfg,
		// Always present, regardless of NoNetwork/TapDevice: this is the
		// programmatic exec channel (internal/vsock) and doesn't need a
		// network interface, IP, or TAP device to work.
		VsockDevices: []fc.VsockDevice{{
			ID:   "exec",
			Path: VsockDevicePath,
			CID:  vsockGuestCID,
		}},
	}

	if vm.TapDevice != "" {
		guestIP := net.ParseIP(vm.GuestIP)
		gatewayIP := net.ParseIP(vm.GatewayIP)
		if guestIP == nil || gatewayIP == nil {
			return fc.Config{}, fmt.Errorf("invalid guest/gateway IP for VM %s: guest=%q gateway=%q", vm.ID, vm.GuestIP, vm.GatewayIP)
		}

		// The SDK configures this statically inside the guest at boot
		// (no DHCP, no in-guest agent needed) — same pattern as Firecracker's
		// own getting-started network guide.
		// Mask must be the network's real prefix, not a fixed /30: with the wrong
		// prefix the guest miscomputes its subnet (e.g. treats a same-network
		// peer as its broadcast address) and unicast between VMs breaks.
		prefix := vm.PrefixLen
		if prefix == 0 {
			prefix = 24
		}
		cfg.NetworkInterfaces = fc.NetworkInterfaces{{
			StaticConfiguration: &fc.StaticNetworkConfiguration{
				HostDevName: vm.TapDevice,
				// A unique, stable MAC per VM. Without it Firecracker leaves the
				// guest to pick one, and on a shared bridge two VMs can collide
				// on the same MAC — the bridge then can't tell them apart and
				// L2/ARP between them fails ("Destination Host Unreachable").
				// Derived from the guest IP (locally-administered 02: prefix),
				// so it's deterministic across reboots and never duplicated
				// within a subnet.
				MacAddress: deriveMAC(guestIP),
				IPConfiguration: &fc.IPConfiguration{
					IPAddr:      net.IPNet{IP: guestIP, Mask: net.CIDRMask(prefix, 32)},
					Gateway:     gatewayIP,
					Nameservers: defaultNameservers,
				},
			},
		}}
	}

	return cfg, nil
}

// deriveMAC builds a locally-administered, unicast MAC from an IPv4 address:
// 02:00 + the four IP octets (e.g. 172.16.1.2 -> 02:00:ac:10:01:02). Unique
// per address within a deployment and stable across reboots. Falls back to a
// fixed local MAC if ip isn't a valid IPv4 (shouldn't happen — the caller only
// reaches here with a TAP configured).
func deriveMAC(ip net.IP) string {
	v4 := ip.To4()
	if v4 == nil {
		return "02:00:00:00:00:01"
	}
	return fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", v4[0], v4[1], v4[2], v4[3])
}

// Launch starts a new Firecracker microVM through Jailer (cfg.JailerCfg must
// be set) and blocks until the machine has accepted its configuration and
// issued InstanceStart.
func Launch(ctx context.Context, cfg fc.Config) (*fc.Machine, error) {
	m, err := fc.NewMachine(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating machine: %w", err)
	}

	if err := m.Start(ctx); err != nil {
		return nil, fmt.Errorf("starting machine: %w", err)
	}

	return m, nil
}

// Stop shuts a machine down. It attempts a graceful ACPI power-off first and
// gives the guest a short window to take it, but always follows up with
// StopVMM (SIGTERM to the jailed process) so the caller is guaranteed the
// process is gone and its resources (chroot, socket) can be cleaned up.
func Stop(ctx context.Context, m *fc.Machine) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_ = m.Shutdown(shutdownCtx)
	_ = m.Wait(shutdownCtx)

	return m.StopVMM()
}
