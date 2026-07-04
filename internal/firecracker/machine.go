package firecracker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

// BuildRestoreConfig assembles the minimal fc.Config for a snapshot restore.
// No kernel, no NICs, no vsock: the restored VM's whole device tree comes from
// the snapshot's vmstate, and PUTting devices after a restore is invalid. The
// root drive entry is bookkeeping only — it satisfies the SDK's jailer
// validation (which insists on a root drive) and is never sent to Firecracker,
// since withRestore drops the bootstrap handlers that would PUT it.
func BuildRestoreConfig(vmID, diskPath string, jcfg fc.JailerConfig) fc.Config {
	return fc.Config{
		VMID: vmID,
		// Same reasoning as BuildConfig: VMs outlive the daemon; never forward
		// the daemon's signals to the Firecracker child.
		ForwardSignals: []os.Signal{},
		Drives: []models.Drive{{
			DriveID:      fc.String("rootfs"),
			PathOnHost:   fc.String(diskPath),
			IsRootDevice: fc.Bool(true),
			IsReadOnly:   fc.Bool(false),
		}},
		JailerCfg: &jcfg,
	}
}

// Chroot-relative filenames the restore handler links a snapshot's artifacts
// under. Fixed names: each VM has its own chroot, so they can't collide.
const (
	snapshotStateBase = "snap.vmstate"
	snapshotMemBase   = "snap.mem"
)

// RestoreSpec carries everything LaunchFromSnapshot needs beyond the base
// fc.Config: where the snapshot's artifacts live on the host, what the drive
// must be called inside the chroot, and which TAP the restored NIC lands on.
type RestoreSpec struct {
	// StatePath/MemPath are the host-side snapshot files (vmstate + guest
	// memory). Never written to by a restore: the mem file is mapped
	// copy-on-write, so many VMs can restore from the same pair concurrently.
	StatePath string
	MemPath   string

	// DiskPath is the host-side rootfs for the NEW VM — a private
	// copy-on-write clone of the snapshot's disk, already chowned to the
	// jailer uid/gid. DriveBase is the filename the snapshot's vmstate
	// recorded for the drive (the ORIGIN VM's "<id>.ext4"), so the clone must
	// appear inside the chroot under that exact name, whatever the new VM's
	// own ID is.
	DiskPath  string
	DriveBase string

	// ChrootDir is the host-side path of the new VM's chroot root
	// (jailer.WorkspaceRoot) — where the artifacts above get hardlinked. All
	// of them live on the same filesystem as the chroot by the store's
	// invariant, or these links would fail with EXDEV.
	ChrootDir string

	// TapDevice is the host TAP backing the restored NIC; SnapshotTap is the
	// TAP name the vmstate recorded at snapshot time. When they match (the
	// in-place restore case, or a fork that grabbed the original name) no
	// network_overrides is sent — Firecracker rebinds the recorded name on its
	// own, and the field doesn't even exist before Firecracker 1.12. Only when
	// they differ is an override emitted, which then requires FC >= 1.12.
	// TapDevice empty = the snapshot had no network device.
	TapDevice   string
	SnapshotTap string
}

// LaunchFromSnapshot starts a Firecracker process through Jailer and, instead
// of booting a kernel, restores it from a snapshot and resumes it. The SDK's
// own snapshot support can't do this under Jailer with a renamed TAP (its
// models predate network_overrides), so the FcInit pipeline is replaced with
// just StartVMM plus a custom handler that links the snapshot's files into
// the freshly created chroot and drives PUT /snapshot/load by hand.
func LaunchFromSnapshot(ctx context.Context, cfg fc.Config, spec RestoreSpec) (*fc.Machine, error) {
	m, err := fc.NewMachine(ctx, cfg, withRestore(spec))
	if err != nil {
		return nil, fmt.Errorf("creating machine for restore: %w", err)
	}

	if err := m.Start(ctx); err != nil {
		return nil, fmt.Errorf("restoring machine from snapshot: %w", err)
	}

	return m, nil
}

// withRestore reshapes a jailed Machine for snapshot restore. It must run as
// an Opt (after jail() has built the command and handler list) so it can
// discard the boot-oriented FcInit pipeline the chroot strategy installed.
func withRestore(spec RestoreSpec) fc.Opt {
	return func(m *fc.Machine) {
		// Host-side paths, deliberately: hasSnapshot() turning true is what
		// suppresses the SDK's InstanceStart, and the SDK validates these with
		// os.Stat on the host. The chroot-relative paths Firecracker sees are
		// built by the restore handler below.
		m.Cfg.Snapshot = fc.SnapshotConfig{
			MemFilePath:  spec.MemPath,
			SnapshotPath: spec.StatePath,
		}

		// StartVMM launches jailer (which builds the chroot) and waits for the
		// API socket; everything a boot would do next (boot-source, drives,
		// NICs, vsock PUTs) is invalid on a restored VM — its device tree
		// comes from the vmstate — so the restore handler is the only step.
		m.Handlers.FcInit = fc.HandlerList{}.Append(
			fc.StartVMMHandler,
			restoreHandler(spec),
		)
	}
}

// restoreHandler links the snapshot's artifacts into the (just created)
// chroot and asks Firecracker to load + resume. Runs after StartVMMHandler,
// so the chroot exists and the API socket is up.
func restoreHandler(spec RestoreSpec) fc.Handler {
	return fc.Handler{
		Name: "microhosted.RestoreSnapshot",
		Fn: func(ctx context.Context, m *fc.Machine) error {
			links := []struct{ src, base string }{
				{spec.StatePath, snapshotStateBase},
				{spec.MemPath, snapshotMemBase},
				{spec.DiskPath, spec.DriveBase},
			}
			for _, l := range links {
				if err := os.Link(l.src, filepath.Join(spec.ChrootDir, l.base)); err != nil {
					return fmt.Errorf("linking %s into chroot: %w", l.src, err)
				}
			}

			var overrides []NetworkOverride
			if spec.TapDevice != "" && spec.TapDevice != spec.SnapshotTap {
				// The SDK numbers boot-time interfaces from "1", so a
				// single-NIC snapshot always restores as iface "1".
				overrides = append(overrides, NetworkOverride{IfaceID: "1", HostDevName: spec.TapDevice})
			}

			// m.Cfg.SocketPath is the host-side socket path (jail() rewrote
			// it); the snapshot paths are what chrooted Firecracker resolves.
			return SnapshotLoad(ctx, m.Cfg.SocketPath,
				"/"+snapshotStateBase, "/"+snapshotMemBase, overrides)
		},
	}
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
