package firecracker

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"

	"microhosted/internal/network"
	"microhosted/internal/storage"
	"microhosted/pkg/types"
)

// defaultKernelArgs keeps the serial console (the VM's console log and its
// getty) but boots it quiet: every character the kernel prints to the emulated
// UART traps into the VMM, and its ~200 boot lines cost ~80 ms of an ~170 ms
// boot to agent-ready (measured with scripts/boot-args-bench.py). Warnings and
// errors still reach the console log; the full boot log stays in the guest's
// dmesg.
const defaultKernelArgs = "console=ttyS0 reboot=k panic=1 pci=off quiet"

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

// rootDriveID is the root drive's ID, in the boot config and so in every
// vmstate taken from it — which is how a restore addresses it.
const rootDriveID = "rootfs"

// vsockGuestCID is the guest-side CID Firecracker's vsock device presents.
// It's local to the VM's own vsock namespace (isolation on the host side
// comes from which UDS path you dial, not from CID uniqueness), so every VM
// can safely use the same value — this is Firecracker's own convention.
const vsockGuestCID = 3

// BuildConfig assembles the firecracker.Config for one VM: boot source, root
// drive, machine sizing, network interface (if a TAP device was assigned)
// and the jailer configuration that will actually launch it.
// io is the VM's effective throughput limits (vm.EffectiveIO): every drive
// and both directions of the NIC get a Firecracker rate limiter.
func BuildConfig(vm types.VMConfig, io types.IOLimits, jcfg fc.JailerConfig) (fc.Config, error) {
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
		Seccomp:        seccomp,
		Drives:         buildDrives(vm, DiskRateLimiter(io)),
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
			InRateLimiter:  NetRateLimiter(io),
			OutRateLimiter: NetRateLimiter(io),
		}}
	}

	return cfg, nil
}

// buildDrives assembles the drive list for a VM: the root device first, then one
// secondary drive per attached volume. Firecracker exposes secondary drives as
// /dev/vdb, /dev/vdc… in this order, which is why VMConfig.Volumes preserves the
// order the volumes were requested — the manager's auto-mount depends on it. A
// read-only volume is a read-only block device (is_read_only), so a sample
// attached that way can't be altered by the guest inspecting it, not merely
// mounted with -o ro.
func buildDrives(vm types.VMConfig, rl *models.RateLimiter) []models.Drive {
	drives := []models.Drive{{
		DriveID:      fc.String(rootDriveID),
		PathOnHost:   fc.String(vm.Rootfs),
		IsRootDevice: fc.Bool(true),
		IsReadOnly:   fc.Bool(false),
		RateLimiter:  rl,
	}}
	for _, vol := range vm.Volumes {
		drives = append(drives, models.Drive{
			DriveID:      fc.String(vol.DriveID),
			PathOnHost:   fc.String(vol.HostPath),
			IsRootDevice: fc.Bool(false),
			IsReadOnly:   fc.Bool(vol.ReadOnly),
			// Per drive: Firecracker has no budget shared across devices,
			// so a VM with volumes gets this limit on each of them.
			RateLimiter: rl,
		})
	}
	return drives
}

// deriveMAC is the NIC's MAC for a guest IP — network.GuestMAC, the same pair
// the port filter pins the VM's TAP to. Falls back to a fixed local MAC if ip
// isn't a valid IPv4 (shouldn't happen — the caller only reaches here with a
// TAP configured, and the filter then passes nothing from it).
func deriveMAC(ip net.IP) string {
	if mac := network.GuestMAC(ip); mac != "" {
		return mac
	}
	return "02:00:00:00:00:01"
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
		Seccomp:        seccomp,
		Drives: []models.Drive{{
			DriveID:      fc.String(rootDriveID),
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
	// memory), sealed root-only. Hard-linked into the chroot with a read
	// grant for JailGID that lasts only for the load (storage.ShareSnapshotFile).
	StatePath string
	MemPath   string
	// JailGID is the new VM's identity (its gid; see jailer/identity.go).
	JailGID int

	// DiskPath is the host-side rootfs for the NEW VM — a private
	// copy-on-write clone of the snapshot's disk, already owned by the VM's
	// identity. DriveBase is the filename the snapshot's vmstate
	// recorded for the drive (the ORIGIN VM's "<id>.ext4"), so the clone must
	// appear inside the chroot under that exact name, whatever the new VM's
	// own ID is.
	DiskPath  string
	DriveBase string

	// ChrootDir is the host-side path of the new VM's chroot root
	// (jailer.WorkspaceRoot) — where the disk, state and memory get
	// hardlinked. All of them live on the same filesystem as the chroot by
	// the store's invariant, or the links fail with EXDEV.
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

	// IO is the restored VM's throughput limits (vm.EffectiveIO), set on its
	// devices between the load and the resume.
	IO types.IOLimits
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
			// State and memory: the snapshot's sealed inodes, shared by every
			// restore so the page cache of untouched memory is shared too,
			// with a read grant for this VM's group that lasts only until the
			// load below has opened them (see storage.ShareSnapshotFile).
			// Revoked on every path out of this handler, success or not.
			shared := []struct{ src, base string }{
				{spec.StatePath, snapshotStateBase},
				{spec.MemPath, snapshotMemBase},
			}
			var revokes []func() error
			defer func() {
				for _, revoke := range revokes {
					if err := revoke(); err != nil {
						// Fail-safe either way: the grant is read-only, and
						// the next daemon start clears every snapshot ACL.
						log.Printf("restore: %v", err)
					}
				}
			}()
			for _, s := range shared {
				revoke, err := storage.ShareSnapshotFile(s.src, filepath.Join(spec.ChrootDir, s.base), spec.JailGID)
				revokes = append(revokes, revoke)
				if err != nil {
					return fmt.Errorf("sharing %s into chroot: %w", s.src, err)
				}
			}
			// The disk is already this VM's own clone.
			if err := os.Link(spec.DiskPath, filepath.Join(spec.ChrootDir, spec.DriveBase)); err != nil {
				return fmt.Errorf("linking %s into chroot: %w", spec.DiskPath, err)
			}

			var overrides []NetworkOverride
			if spec.TapDevice != "" && spec.TapDevice != spec.SnapshotTap {
				// The SDK numbers boot-time interfaces from "1", so a
				// single-NIC snapshot always restores as iface "1".
				overrides = append(overrides, NetworkOverride{IfaceID: "1", HostDevName: spec.TapDevice})
			}

			// m.Cfg.SocketPath is the host-side socket path (jail() rewrote
			// it); the snapshot paths are what chrooted Firecracker resolves.
			sock, uid := m.Cfg.SocketPath, JailUID(m)
			if err := SnapshotLoad(ctx, sock, uid,
				"/"+snapshotStateBase, "/"+snapshotMemBase, overrides); err != nil {
				return err
			}
			// The vmstate carries the limiters of whatever VM was snapshotted
			// — none, for a snapshot older than them. The guest runs no
			// instruction before this VM's own are in place.
			if rl := DiskRateLimiter(spec.IO); rl != nil {
				if err := PatchDriveRateLimiter(ctx, sock, uid, rootDriveID, rl); err != nil {
					return fmt.Errorf("limiting restored disk: %w", err)
				}
			}
			if rl := NetRateLimiter(spec.IO); rl != nil && spec.TapDevice != "" {
				if err := PatchNetRateLimiter(ctx, sock, uid, "1", rl); err != nil {
					return fmt.Errorf("limiting restored network: %w", err)
				}
			}
			return ResumeVM(ctx, sock, uid)
		},
	}
}

// JailUID is the uid a machine's Firecracker was jailed as, i.e. the owner its
// sockets must have. -1 (which jailer.DialSocket refuses) if it isn't jailed.
func JailUID(m *fc.Machine) int {
	if m.Cfg.JailerCfg == nil || m.Cfg.JailerCfg.UID == nil {
		return -1
	}
	return *m.Cfg.JailerCfg.UID
}

// Stop shuts a machine down. It attempts a graceful ACPI power-off first and
// gives the guest a short window to take it, but always follows up with
// StopVMM (SIGTERM to the jailed process) so the caller is guaranteed the
// process is gone and its resources (chroot, socket) can be cleaned up.
func Stop(ctx context.Context, m *fc.Machine) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Not m.Shutdown: the SDK's client would dial the socket path as is,
	// following whatever the jailed process put there.
	_ = SendCtrlAltDel(shutdownCtx, m.Cfg.SocketPath, JailUID(m))
	_ = m.Wait(shutdownCtx)

	return m.StopVMM()
}

// Kill terminates a machine without offering the guest a graceful power-off.
// This is for Destroy, where the disk is deleted right afterwards — an orderly
// guest shutdown buys nothing there, and waiting for one (which most minimal
// guests ignore anyway) costs Stop's full 5-second window per VM. StopVMM only
// sends SIGTERM and returns, so wait for the process to actually exit before
// the caller removes the jail dir out from under it; the VMM dies in
// milliseconds, the timeout is just a backstop against a wedged process.
func Kill(ctx context.Context, m *fc.Machine) error {
	err := m.StopVMM()

	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = m.Wait(waitCtx)

	return err
}
