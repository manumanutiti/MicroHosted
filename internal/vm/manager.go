package vm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/google/uuid"

	"microhosted/internal/firecracker"
	"microhosted/internal/jailer"
	"microhosted/internal/network"
	"microhosted/internal/storage"
	"microhosted/internal/store"
	"microhosted/internal/vsock"
	"microhosted/pkg/types"
)

// Lifecycle errors the API layer maps to HTTP status codes: ErrVMNotFound /
// ErrSnapshotNotFound → 404, ErrVMState → 409 (an operation invalid for the
// VM's current state, e.g. Stop on a stopped VM or Start on a running one),
// ErrConflict → 409 (the operation is valid but collides with current
// resources, e.g. forking onto a network where the snapshot's IP is taken).
var (
	ErrVMNotFound       = errors.New("vm not found")
	ErrVMState          = errors.New("vm in incompatible state")
	ErrSnapshotNotFound = errors.New("snapshot not found")
	ErrConflict         = errors.New("conflicting resources")
)

// running couples a live Machine with the log file its console/Jailer
// output was redirected to, so Destroy can close the file handle cleanly.
type running struct {
	machine *fc.Machine
	logFile *os.File
}

// Manager owns the whole lifecycle of a VM: template lookup, disk cloning,
// network setup, and launching/stopping through Jailer + Firecracker. It's
// the single seam meant for future extension — persistence (Sesión 8),
// multi-host placement (Sesión 9) and snapshot/restore (Sesión 6) all plug
// in here without the API layer changing.
//
// State is kept in memory only for now; a restart of the daemon forgets
// every VM it was tracking (their processes keep running, but the manager
// loses its bookkeeping).
type Manager struct {
	catalog      *storage.Catalog
	jailerCfg    jailer.Defaults
	netmgr       *network.Manager
	instancesDir string
	store        *store.Store
	// netOverridesOK: the host's firecracker binary accepts network_overrides
	// in snapshot load (>= 1.12). Without it a snapshot can only be restored
	// onto a TAP with the exact name recorded in its vmstate, which constrains
	// what Fork can do (see there). Probed once at construction.
	netOverridesOK bool

	mu    sync.Mutex
	vms   map[string]*types.VM
	run   map[string]*running
	snaps map[string]*types.Snapshot
}

// NewManager wires a Manager to its template catalog, jailer defaults, the
// directory where per-VM rootfs clones and console logs are stored, the network
// manager it attaches VMs through, and the store that persists VM records
// across daemon restarts.
func NewManager(catalog *storage.Catalog, jailerCfg jailer.Defaults, instancesDir string, st *store.Store, netmgr *network.Manager) *Manager {
	return &Manager{
		catalog:        catalog,
		jailerCfg:      jailerCfg,
		netmgr:         netmgr,
		instancesDir:   instancesDir,
		store:          st,
		netOverridesOK: firecracker.SupportsNetworkOverrides(jailerCfg.ExecFile),
		vms:            make(map[string]*types.VM),
		run:            make(map[string]*running),
		snaps:          make(map[string]*types.Snapshot),
	}
}

// LoadSnapshots seeds the manager's snapshot index from persisted records at
// startup. Unlike VMs there's no liveness to reconcile — a snapshot is inert
// files plus this record — but one whose directory vanished (operator deleted
// it by hand) is dropped rather than offered for restores that can only fail.
func (m *Manager) LoadSnapshots(records []*types.Snapshot) {
	for _, snap := range records {
		if _, err := os.Stat(snap.Dir); err != nil {
			log.Printf("reconcile: dropping snapshot %s: dir %s missing", snap.ID, snap.Dir)
			_ = m.store.DeleteSnapshot(snap.ID)
			continue
		}
		m.mu.Lock()
		m.snaps[snap.ID] = snap
		m.mu.Unlock()
	}
}

// Reconcile rebuilds the manager's in-memory view from persisted records at
// startup, comparing each against reality:
//
//   - a VM whose process is still alive is adopted — tracked again so it can be
//     listed, exec'd into and destroyed, and its IP block re-reserved — but
//     without an SDK machine handle, since the SDK can't re-attach to a process
//     it didn't spawn (Destroy signals it by PID instead).
//   - a VM whose process is gone died while the daemon was down: its leftover
//     resources are swept and its record dropped.
//
// It returns the set of tap device names belonging to adopted VMs so the
// caller can exclude them from network.SweepOrphans — they're live, not
// orphans. Must be called before the API server starts serving.
func (m *Manager) Reconcile(records []*types.VM) map[string]bool {
	keepTaps := make(map[string]bool)

	for _, rec := range records {
		id := rec.Config.ID

		// A VM stopped on purpose (poweroff — disk kept, IP reserved) has no
		// live process by design, so it must NOT be swept like a crashed one.
		// Keep it tracked as stopped and re-reserve its IP so a freshly created
		// VM can't grab the address it will reclaim on Start. It has no TAP
		// (Stop released it), so there's nothing to add to keepTaps.
		if rec.State == types.VMStateStopped {
			m.mu.Lock()
			m.vms[id] = rec
			m.run[id] = &running{}
			m.mu.Unlock()
			// Quarantined forks hold no reservation: their GuestIP is only
			// what the restored guest believes it has, not an IPAM lease.
			if rec.Config.GuestIP != "" && rec.Config.NetworkName != "" {
				m.netmgr.ReserveVM(rec.Config.NetworkName, id, rec.Config.GuestIP)
			}
			log.Printf("reconcile: kept stopped vm %s", id)
			continue
		}

		if processAlive(rec.PID, id) {
			m.mu.Lock()
			m.vms[id] = rec
			m.run[id] = &running{} // no SDK handle / log file for adopted VMs
			m.mu.Unlock()

			if tap := rec.Config.TapDevice; tap != "" {
				keepTaps[tap] = true
				// See the stopped branch: a quarantined fork's TAP is live and
				// must be kept, but there is no network to re-reserve on.
				if rec.Config.NetworkName != "" {
					m.netmgr.ReserveVM(rec.Config.NetworkName, id, rec.Config.GuestIP)
				}
			}
			log.Printf("reconcile: adopted running vm %s (pid %d)", id, rec.PID)
			continue
		}

		// Dead while we were down — sweep whatever it left behind and forget it.
		m.cleanupNetwork(rec.Config.NetworkName, rec.Config.TapDevice, id)
		_ = storage.DeleteClone(m.instancesDir, id)
		_ = jailer.RemoveInstanceDir(m.jailerCfg, id)
		_ = removeIfExists(rec.LogPath)
		if err := m.store.DeleteVM(id); err != nil {
			log.Printf("reconcile: dropping dead vm record %s: %v", id, err)
		}
		log.Printf("reconcile: swept dead vm %s (pid %d no longer running)", id, rec.PID)
	}

	return keepTaps
}

// Create clones a template's disk, wires up networking, and launches a new
// microVM through Jailer. Any failure partway through is rolled back in
// reverse order so no orphaned tap devices or clones are left behind.
func (m *Manager) Create(ctx context.Context, req types.CreateVMRequest) (*types.VM, error) {
	tpl, err := m.catalog.Get(req.Template)
	if err != nil {
		return nil, err
	}

	vcpus := req.VCPUs
	if vcpus == 0 {
		vcpus = tpl.VCPUs
	}
	memMB := req.MemMB
	if memMB == 0 {
		memMB = tpl.MemMB
	}
	diskMB := req.DiskMB
	if diskMB == 0 {
		diskMB = tpl.DiskMB
	}

	// Kept short (8 hex chars) because it's reused as part of the tap
	// device name, which Linux caps at 15 characters (IFNAMSIZ).
	id := uuid.NewString()[:8]

	rootfsPath, err := storage.CloneRootfs(tpl, id, m.instancesDir, m.jailerCfg.UID, m.jailerCfg.GID, diskMB)
	if err != nil {
		return nil, fmt.Errorf("cloning rootfs: %w", err)
	}

	// Network is opt-out, not mandatory: sandboxed/ephemeral workloads often
	// shouldn't have any path to the host at all (see NoNetwork's doc comment).
	// When on, the VM joins a segmented network (the default one if unspecified):
	// its TAP is enslaved to that network's bridge and it gets an IP from the
	// network's subnet — VMs share L2 within a network, isolated across networks.
	var tapName, networkName, bridge, guestIP, gatewayIP string
	var prefixLen int
	if !req.NoNetwork {
		networkName = req.Network
		if networkName == "" {
			networkName = network.DefaultNetworkName
		}
		ip, gw, br, prefix, err := m.netmgr.AttachVM(networkName, id)
		if err != nil {
			_ = storage.DeleteClone(m.instancesDir, id)
			return nil, fmt.Errorf("attaching to network %q: %w", networkName, err)
		}

		tapName = "tap" + id
		if err := network.CreateTapEnslaved(tapName, br); err != nil {
			m.netmgr.DetachVM(networkName, id)
			_ = storage.DeleteClone(m.instancesDir, id)
			return nil, fmt.Errorf("creating tap device: %w", err)
		}
		guestIP, gatewayIP, bridge, prefixLen = ip, gw, br, prefix
	}

	vmCfg := types.VMConfig{
		ID:           id,
		TemplateName: tpl.Name,
		Kernel:       tpl.KernelPath,
		Rootfs:       rootfsPath,
		VCPUs:        vcpus,
		MemMB:        memMB,
		DiskMB:       diskMB,
		NetworkName:  networkName,
		Bridge:       bridge,
		TapDevice:    tapName,
		GuestIP:      guestIP,
		GatewayIP:    gatewayIP,
		PrefixLen:    prefixLen,
	}

	record := &types.VM{
		Config:    vmCfg,
		LogPath:   filepath.Join(m.instancesDir, id+".log"),
		CreatedAt: time.Now(),
	}

	// boot opens the console log and launches Firecracker, filling in the
	// runtime fields. Any failure here rolls back the network attachment and
	// rootfs clone this Create made (boot cleans up only the log it opened).
	// The jail dir too: the SDK stops the VMM on a failed start but never
	// removes Jailer's directory, and a leftover would break a retried launch
	// under the same ID and leak disk otherwise.
	if err := m.boot(record); err != nil {
		m.cleanupNetwork(networkName, tapName, id)
		_ = jailer.RemoveInstanceDir(m.jailerCfg, id)
		_ = storage.DeleteClone(m.instancesDir, id)
		return nil, err
	}

	m.mu.Lock()
	m.vms[id] = record
	m.mu.Unlock()

	// Persist last, once the VM is fully up and tracked. A record we can't
	// persist is exactly the orphan the store exists to prevent, so a failure
	// here rolls the whole VM back rather than leaving it running-but-unknown.
	if err := m.store.SaveVM(record); err != nil {
		_ = m.Destroy(context.Background(), id)
		return nil, fmt.Errorf("persisting vm record %s: %w", id, err)
	}

	return record, nil
}

// boot opens the VM's console log and launches Firecracker+Jailer for the
// already-populated record (rootfs cloned, network attached). On success it
// fills in the runtime fields — PID, socket path, vsock path, running state —
// and tracks the live handle in m.run. On failure it closes only the log file
// it opened; rolling back the clone/network is the caller's job, since boot
// can't know whether they should survive (Start reuses them, Create undoes
// them). Shared by Create (first boot) and Start (boot from a stopped VM).
func (m *Manager) boot(record *types.VM) error {
	id := record.Config.ID

	logFile, err := os.OpenFile(record.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("opening console log %s: %w", record.LogPath, err)
	}

	// stdout/stderr point at the VM's own log file, never at the daemon's
	// terminal — see the comment on jailer.Build for why.
	jcfg := jailer.Build(id, record.Config.Kernel, m.jailerCfg, logFile, logFile)
	fcCfg, err := firecracker.BuildConfig(record.Config, jcfg)
	if err != nil {
		_ = logFile.Close()
		return err
	}

	// Deliberately not a request context: the SDK ties the Firecracker
	// process's lifetime to whatever context it's launched with
	// (exec.CommandContext under the hood). A request-scoped ctx dies the
	// moment the HTTP call returns, which would kill the VM right after it
	// booted. The VM's lifetime is the manager's, not any single request's.
	machine, err := firecracker.Launch(context.Background(), fcCfg)
	if err != nil {
		_ = logFile.Close()
		return fmt.Errorf("launching VM: %w", err)
	}

	pid, _ := machine.PID()
	record.PID = pid
	// machine.Cfg.SocketPath (not fcCfg.SocketPath) because Jailer rewrites it
	// to the absolute path inside the chroot once launched.
	record.SocketPath = machine.Cfg.SocketPath
	// Unlike SocketPath, the SDK does NOT rewrite VsockDevice.Path into the
	// chroot for us — build the host-side path ourselves using the same
	// convention jailer.WorkspaceRoot encodes.
	record.VsockPath = filepath.Join(jailer.WorkspaceRoot(m.jailerCfg, id), firecracker.VsockDevicePath)
	record.State = types.VMStateRunning

	m.mu.Lock()
	m.run[id] = &running{machine: machine, logFile: logFile}
	m.mu.Unlock()

	return nil
}

// Destroy stops the machine and releases every resource associated with it.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	m.mu.Lock()
	record, ok := m.vms[id]
	r := m.run[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("vm %q not found", id)
	}

	var stopErr error
	switch {
	case r != nil && r.machine != nil:
		// Normal path: we own a live SDK handle from this daemon's lifetime.
		stopErr = firecracker.Stop(ctx, r.machine)
	case record.PID > 0:
		// Adopted VM: recovered from the store after a restart, so there's no
		// SDK handle to drive a graceful shutdown — signal the process directly.
		stopErr = stopByPID(record.PID)
	}
	if r != nil && r.logFile != nil {
		_ = r.logFile.Close()
	}

	// Every resource is cleaned up regardless of earlier failures — a VM that
	// fails to stop cleanly must still have its tap/IP/clone/jail dir released,
	// or the leak compounds on every crash. Errors are collected, not returned
	// early, so one failed step never skips the rest.
	m.cleanupNetwork(record.Config.NetworkName, record.Config.TapDevice, id)
	cloneErr := storage.DeleteClone(m.instancesDir, id)
	// Jailer never removes its own per-VM directory; without this the chroot
	// (rootfs/kernel hardlinks, api socket, cgroup leftovers) accumulates under
	// ChrootBaseDir on every create/destroy cycle. Safe now that the process
	// is stopped above.
	jailErr := jailer.RemoveInstanceDir(m.jailerCfg, id)
	logErr := removeIfExists(record.LogPath)
	storeErr := m.store.DeleteVM(id)

	m.mu.Lock()
	delete(m.vms, id)
	delete(m.run, id)
	m.mu.Unlock()

	return errors.Join(
		wrapErr("stopping vm %s", id, stopErr),
		wrapErr("removing rootfs clone for vm %s", id, cloneErr),
		wrapErr("removing jail dir for vm %s", id, jailErr),
		wrapErr("removing console log for vm %s", id, logErr),
		wrapErr("removing vm record %s", id, storeErr),
	)
}

// Stop powers a VM off but keeps it around. It halts the Firecracker process
// (freeing its CPU/RAM) and releases the host-side TAP device and jail dir, but
// deliberately preserves the rootfs clone (the disk, with everything the guest
// wrote — e.g. a file it touched) and the VM's IP reservation. Start later
// brings the same VM back at the same address, disk intact. This is the whole
// difference from Destroy, which additionally erases the disk and frees the IP.
func (m *Manager) Stop(ctx context.Context, id string) (*types.VM, error) {
	m.mu.Lock()
	record, ok := m.vms[id]
	r := m.run[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	if record.State == types.VMStateStopped {
		return nil, fmt.Errorf("%w: vm %s is already stopped", ErrVMState, id)
	}

	var stopErr error
	switch {
	case r != nil && r.machine != nil:
		// Normal path: we own a live SDK handle from this daemon's lifetime.
		stopErr = firecracker.Stop(ctx, r.machine)
	case record.PID > 0:
		// Adopted VM (recovered after a restart): no SDK handle to drive a
		// graceful shutdown — signal the process directly.
		stopErr = stopByPID(record.PID)
	}
	if r != nil && r.logFile != nil {
		_ = r.logFile.Close()
	}

	// Release only what a powered-off VM doesn't need: the TAP device and the
	// jail dir. Removing the jail dir is safe for the data — the rootfs clone
	// under instancesDir is a separate hardlink to the same inode, so the disk
	// survives. Deliberately NOT DetachVM (that would release the IP) and NOT
	// DeleteClone (that would erase the disk) — those belong to Destroy.
	if record.Config.TapDevice != "" {
		_ = network.DeleteTap(record.Config.TapDevice)
	}
	jailErr := jailer.RemoveInstanceDir(m.jailerCfg, id)

	m.mu.Lock()
	record.State = types.VMStateStopped
	record.PID = 0 // no live process; also keeps Destroy from signalling a dead pid
	record.SocketPath = ""
	record.VsockPath = ""
	m.run[id] = &running{} // drop the (now closed) SDK handle + log file
	m.mu.Unlock()

	storeErr := m.store.SaveVM(record)

	return record, errors.Join(
		wrapErr("stopping vm %s", id, stopErr),
		wrapErr("removing jail dir for vm %s", id, jailErr),
		wrapErr("persisting stopped vm %s", id, storeErr),
	)
}

// Start boots a stopped VM back up. Stop kept its rootfs clone and IP
// reservation, so it returns at the same address with the same disk; all Start
// rebuilds is the TAP device (released at Stop) and the Firecracker process.
// The network's bridge is still up — Stop never touched it.
func (m *Manager) Start(ctx context.Context, id string) (*types.VM, error) {
	m.mu.Lock()
	record, ok := m.vms[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	if record.State != types.VMStateStopped {
		return nil, fmt.Errorf("%w: vm %s is not stopped (state %s)", ErrVMState, id, record.State)
	}

	// Recreate the TAP and re-enslave it to its network's bridge. No AttachVM:
	// the IP is still reserved from before, so we reuse record.Config.GuestIP.
	// A quarantined fork gets its bridge-less TAP back instead — starting it
	// cold must not quietly connect it to a network its whole point is to be
	// off of.
	if record.Config.TapDevice != "" {
		if err := recreateTap(record.Config); err != nil {
			return nil, fmt.Errorf("recreating tap for vm %s: %w", id, err)
		}
	}

	if err := m.boot(record); err != nil {
		if record.Config.TapDevice != "" {
			_ = network.DeleteTap(record.Config.TapDevice)
		}
		return nil, err
	}

	if err := m.store.SaveVM(record); err != nil {
		_ = m.Destroy(context.Background(), id)
		return nil, fmt.Errorf("persisting restarted vm %s: %w", id, err)
	}

	return record, nil
}

// recreateTap rebuilds a VM's TAP device according to its config: enslaved to
// its network's bridge, or deliberately bridge-less for a quarantined fork.
func recreateTap(cfg types.VMConfig) error {
	if cfg.Quarantine {
		return network.CreateTapQuarantined(cfg.TapDevice)
	}
	return network.CreateTapEnslaved(cfg.TapDevice, cfg.Bridge)
}

// Snapshot captures a running VM's full state — guest memory, device state,
// and a copy-on-write clone of its disk — as a restorable point in time. The
// VM is paused for the duration (memory and disk must be captured at the same
// instant to be mutually consistent) and resumed before returning; the pause
// is not a state transition the API surfaces, just a sub-second freeze.
//
// Works on adopted VMs too (no SDK handle after a daemon restart): all three
// steps speak to Firecracker's API socket directly.
func (m *Manager) Snapshot(ctx context.Context, vmID, name string) (*types.Snapshot, error) {
	m.mu.Lock()
	record, ok := m.vms[vmID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	if record.State != types.VMStateRunning {
		return nil, fmt.Errorf("%w: vm %s is not running (state %s)", ErrVMState, vmID, record.State)
	}

	sid := uuid.NewString()[:8]
	dir, err := storage.CreateSnapshotDir(m.instancesDir, sid)
	if err != nil {
		return nil, err
	}
	fail := func(step string, err error) (*types.Snapshot, error) {
		_ = storage.DeleteSnapshotDir(m.instancesDir, sid)
		return nil, fmt.Errorf("%s for vm %s: %w", step, vmID, err)
	}

	socket := record.SocketPath
	if err := firecracker.PauseVM(ctx, socket); err != nil {
		return fail("pausing vm", err)
	}
	// From here on the VM must be resumed no matter what fails — a VM left
	// frozen because its snapshot failed would be strictly worse than no
	// snapshot. Background context: the resume must happen even if the
	// request's ctx is already dead.
	paused := true
	defer func() {
		if paused {
			if err := firecracker.ResumeVM(context.Background(), socket); err != nil {
				log.Printf("snapshot: resuming vm %s after failure: %v", vmID, err)
			}
		}
	}()

	// Chroot-relative paths: Firecracker writes these inside its jail, and the
	// daemon then moves them (same-FS rename, free) into the snapshot dir.
	stateBase := "snap_" + sid + ".vmstate"
	memBase := "snap_" + sid + ".mem"
	if err := firecracker.SnapshotCreate(ctx, socket, "/"+stateBase, "/"+memBase); err != nil {
		return fail("creating snapshot", err)
	}

	ws := jailer.WorkspaceRoot(m.jailerCfg, vmID)
	if err := os.Rename(filepath.Join(ws, stateBase), filepath.Join(dir, storage.SnapshotStateFile)); err != nil {
		return fail("collecting vmstate", err)
	}
	if err := os.Rename(filepath.Join(ws, memBase), filepath.Join(dir, storage.SnapshotMemFile)); err != nil {
		return fail("collecting memory file", err)
	}

	// Disk capture happens while still paused, so it matches the memory image
	// exactly. Reflink: instant and shares blocks, no matter the disk size.
	if err := storage.ReflinkFile(record.Config.Rootfs, filepath.Join(dir, storage.SnapshotDiskFile)); err != nil {
		return fail("capturing disk", err)
	}

	paused = false
	if err := firecracker.ResumeVM(ctx, socket); err != nil {
		// The snapshot itself is complete and usable; what failed is bringing
		// the SOURCE back. Keep the snapshot, surface the resume failure.
		return nil, fmt.Errorf("snapshot %s created, but resuming vm %s failed: %w", sid, vmID, err)
	}

	snap := &types.Snapshot{
		ID:           sid,
		Name:         name,
		SourceVMID:   vmID,
		TemplateName: record.Config.TemplateName,
		VCPUs:        record.Config.VCPUs,
		MemMB:        record.Config.MemMB,
		DiskMB:       record.Config.DiskMB,
		NetworkName:  record.Config.NetworkName,
		GuestIP:      record.Config.GuestIP,
		GatewayIP:    record.Config.GatewayIP,
		PrefixLen:    record.Config.PrefixLen,
		HadNetwork:   record.Config.TapDevice != "",
		TapDevice:    record.Config.TapDevice,
		DriveBase:    filepath.Base(record.Config.Rootfs),
		Dir:          dir,
		CreatedAt:    time.Now(),
	}

	if err := m.store.SaveSnapshot(snap); err != nil {
		_ = storage.DeleteSnapshotDir(m.instancesDir, sid)
		return nil, fmt.Errorf("persisting snapshot %s: %w", sid, err)
	}
	m.mu.Lock()
	m.snaps[sid] = snap
	m.mu.Unlock()

	return snap, nil
}

// Fork creates a brand-new VM from a snapshot: fresh ID, its own CoW disk
// stamped from the snapshot's, and the snapshot's memory restored — the guest
// resumes mid-execution exactly where the snapshot froze it.
//
// The guest's network identity (IP, MAC) is baked into that memory and cannot
// be changed at restore time, which forces the two modes:
//
//   - default: the fork rejoins the snapshot's origin network at the
//     snapshot's IP — valid only while that address is free (origin VM
//     destroyed, or never on that network since). This is "reset to clean /
//     resume from a known state as a real VM".
//   - quarantine: the fork's TAP is enslaved to nothing; the guest wakes up
//     believing it's networked while every packet dies on the host. Reachable
//     via vsock exec only. Any number of quarantined forks of one snapshot
//     can run simultaneously.
func (m *Manager) Fork(ctx context.Context, snapID string, quarantine bool) (*types.VM, error) {
	m.mu.Lock()
	snap, ok := m.snaps[snapID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapID)
	}

	id := uuid.NewString()[:8]

	rootfs, err := storage.CloneFromSnapshot(snap.Dir, id, m.instancesDir, m.jailerCfg.UID, m.jailerCfg.GID)
	if err != nil {
		return nil, err
	}

	// Kernel is irrelevant to the restore itself (the guest's kernel lives in
	// the snapshotted memory) but is recorded so a later Stop+Start — a cold
	// boot of the fork's disk — still knows what to boot. A template deleted
	// from the catalog since the snapshot just leaves it blank.
	var kernel string
	if m.catalog != nil {
		if tpl, err := m.catalog.Get(snap.TemplateName); err == nil {
			kernel = tpl.KernelPath
		}
	}

	vmCfg := types.VMConfig{
		ID:           id,
		TemplateName: snap.TemplateName,
		Kernel:       kernel,
		Rootfs:       rootfs,
		VCPUs:        snap.VCPUs,
		MemMB:        snap.MemMB,
		DiskMB:       snap.DiskMB,
		RestoredFrom: snapID,
	}

	if snap.HadNetwork {
		// TAP naming is constrained by the host's Firecracker: before 1.12
		// there is no network_overrides, so a snapshot can ONLY be restored
		// onto a TAP named exactly what its vmstate recorded. On such hosts
		// the fork takes the original name if it's free (origin destroyed or
		// stopped) and refuses otherwise — the honest limit, with the upgrade
		// path spelled out. On >= 1.12 every fork gets its own name and the
		// restore handler emits the override.
		tap := "tap" + id
		if !m.netOverridesOK {
			snapTap := snapshotTapName(snap)
			if network.TapExists(snapTap) {
				_ = storage.DeleteClone(m.instancesDir, id)
				return nil, fmt.Errorf("%w: this host's firecracker predates network_overrides (needs >= 1.12), so the fork must reuse the snapshot's TAP %q, which is in use — destroy/stop the VM holding it, or upgrade firecracker (scripts/install-fc.sh) for simultaneous forks", ErrConflict, snapTap)
			}
			tap = snapTap
		}
		if quarantine {
			if err := network.CreateTapQuarantined(tap); err != nil {
				_ = storage.DeleteClone(m.instancesDir, id)
				return nil, fmt.Errorf("creating quarantined tap: %w", err)
			}
			// GuestIP/gateway are what the restored guest BELIEVES it has —
			// kept for visibility. NetworkName stays empty: no reservation.
			vmCfg.TapDevice = tap
			vmCfg.GuestIP = snap.GuestIP
			vmCfg.GatewayIP = snap.GatewayIP
			vmCfg.PrefixLen = snap.PrefixLen
			vmCfg.Quarantine = true
		} else {
			gateway, bridge, prefix, err := m.netmgr.ClaimVM(snap.NetworkName, id, snap.GuestIP)
			if err != nil {
				_ = storage.DeleteClone(m.instancesDir, id)
				return nil, fmt.Errorf("%w: fork needs the snapshot's address on network %q (%s): %v — destroy the VM holding it, or fork with quarantine=true", ErrConflict, snap.NetworkName, snap.GuestIP, err)
			}
			if err := network.CreateTapEnslaved(tap, bridge); err != nil {
				m.netmgr.DetachVM(snap.NetworkName, id)
				_ = storage.DeleteClone(m.instancesDir, id)
				return nil, fmt.Errorf("creating tap device: %w", err)
			}
			vmCfg.NetworkName = snap.NetworkName
			vmCfg.Bridge = bridge
			vmCfg.TapDevice = tap
			vmCfg.GuestIP = snap.GuestIP
			vmCfg.GatewayIP = gateway
			vmCfg.PrefixLen = prefix
		}
	}

	record := &types.VM{
		Config:    vmCfg,
		LogPath:   filepath.Join(m.instancesDir, id+".log"),
		CreatedAt: time.Now(),
	}

	if err := m.bootFromSnapshot(record, snap); err != nil {
		m.cleanupNetwork(vmCfg.NetworkName, vmCfg.TapDevice, id)
		_ = jailer.RemoveInstanceDir(m.jailerCfg, id)
		_ = storage.DeleteClone(m.instancesDir, id)
		return nil, err
	}

	m.mu.Lock()
	m.vms[id] = record
	m.mu.Unlock()

	// Persist last, same contract as Create: a fork we can't persist is
	// rolled back rather than left running-but-unknown.
	if err := m.store.SaveVM(record); err != nil {
		_ = m.Destroy(context.Background(), id)
		return nil, fmt.Errorf("persisting vm record %s: %w", id, err)
	}

	return record, nil
}

// ForkVM forks a RUNNING VM directly, without the caller managing a snapshot:
// it takes an ephemeral snapshot (the source VM is paused sub-second and
// resumed, exactly like Snapshot), forks a new VM from it, and deletes the
// snapshot. Deleting it under a live fork is safe by design — the fork's disk
// is a private reflink copy and its chroot hardlinks the mem/vmstate inodes
// (see DeleteSnapshot).
//
// This is the one-call path for "give me a copy of this machine as it is right
// now". The network constraints are Fork's: the source VM keeps its IP, so a
// non-quarantine fork of a live VM always collides with it — direct forks are
// therefore mostly useful with quarantine=true (or on a VM with no network).
// Callers who want to keep the frozen point for later restores should use
// Snapshot + Fork instead.
func (m *Manager) ForkVM(ctx context.Context, vmID string, quarantine bool) (*types.VM, error) {
	snap, err := m.Snapshot(ctx, vmID, "fork-ephemeral")
	if err != nil {
		return nil, err
	}

	record, forkErr := m.Fork(ctx, snap.ID, quarantine)

	// The ephemeral snapshot goes away whether the fork worked or not; a
	// deletion failure is a leak to log, not a reason to fail a good fork.
	if err := m.DeleteSnapshot(snap.ID); err != nil {
		log.Printf("forkvm: deleting ephemeral snapshot %s: %v", snap.ID, err)
	}

	return record, forkErr
}

// Restore rolls a VM back, in place, to a snapshot previously taken FROM THAT
// VM: same ID, same network identity, same TAP — only the disk and memory are
// rewound. This is the "reset to clean between samples" primitive: detonate,
// restore, detonate the next sample on a pristine machine in milliseconds.
//
// Restricted to the VM's own snapshots because the guest's identity (IP, MAC,
// in-chroot drive name) is frozen inside the snapshot — restoring another
// VM's snapshot here would silently swap the machine's identity; that
// operation is Fork, which handles the collisions honestly.
func (m *Manager) Restore(ctx context.Context, vmID, snapID string) (*types.VM, error) {
	m.mu.Lock()
	record, okVM := m.vms[vmID]
	snap, okSnap := m.snaps[snapID]
	r := m.run[vmID]
	m.mu.Unlock()
	if !okVM {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	if !okSnap {
		return nil, fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapID)
	}
	if snap.SourceVMID != vmID {
		return nil, fmt.Errorf("%w: snapshot %s was taken from vm %s, not %s — use fork to create a new vm from it", ErrConflict, snapID, snap.SourceVMID, vmID)
	}
	if record.State != types.VMStateRunning && record.State != types.VMStateStopped {
		return nil, fmt.Errorf("%w: vm %s is %s", ErrVMState, vmID, record.State)
	}

	// Tear down the current incarnation the way Stop does — process, jail dir,
	// TAP — but keep the IP reservation and VM record: the restored guest is
	// the same machine at the same address.
	if record.State == types.VMStateRunning {
		var stopErr error
		switch {
		case r != nil && r.machine != nil:
			stopErr = firecracker.Stop(ctx, r.machine)
		case record.PID > 0:
			stopErr = stopByPID(record.PID)
		}
		if stopErr != nil {
			return nil, fmt.Errorf("stopping vm %s before restore: %w", vmID, stopErr)
		}
		if r != nil && r.logFile != nil {
			_ = r.logFile.Close()
		}
		if record.Config.TapDevice != "" {
			_ = network.DeleteTap(record.Config.TapDevice)
		}
		if err := jailer.RemoveInstanceDir(m.jailerCfg, vmID); err != nil {
			return nil, fmt.Errorf("removing jail dir for vm %s: %w", vmID, err)
		}
		m.mu.Lock()
		record.State = types.VMStateStopped
		record.PID = 0
		record.SocketPath = ""
		record.VsockPath = ""
		m.run[vmID] = &running{}
		m.mu.Unlock()
	}

	// Rewind the disk: drop the current clone, stamp a fresh one from the
	// snapshot. Same filename, so the vmstate's recorded drive path matches.
	if err := storage.DeleteClone(m.instancesDir, vmID); err != nil {
		return nil, err
	}
	rootfs, err := storage.CloneFromSnapshot(snap.Dir, vmID, m.instancesDir, m.jailerCfg.UID, m.jailerCfg.GID)
	if err != nil {
		return nil, err
	}
	record.Config.Rootfs = rootfs

	if record.Config.TapDevice != "" {
		if err := recreateTap(record.Config); err != nil {
			return nil, fmt.Errorf("recreating tap for vm %s: %w", vmID, err)
		}
	}

	if err := m.bootFromSnapshot(record, snap); err != nil {
		if record.Config.TapDevice != "" {
			_ = network.DeleteTap(record.Config.TapDevice)
		}
		// Remove the jail dir so a retried restore doesn't trip over the
		// files this attempt linked into the chroot. Disk is already rewound;
		// the VM stays stopped with a clean disk — a retryable state, not a
		// leak.
		_ = jailer.RemoveInstanceDir(m.jailerCfg, vmID)
		_ = m.store.SaveVM(record)
		return nil, err
	}
	record.Config.RestoredFrom = snapID

	if err := m.store.SaveVM(record); err != nil {
		_ = m.Destroy(context.Background(), vmID)
		return nil, fmt.Errorf("persisting restored vm %s: %w", vmID, err)
	}

	return record, nil
}

// bootFromSnapshot is boot()'s sibling for restores: same log file and jailer
// plumbing, but the Firecracker process is brought up via snapshot load —
// no kernel boot, the guest resumes where the snapshot froze it.
func (m *Manager) bootFromSnapshot(record *types.VM, snap *types.Snapshot) error {
	id := record.Config.ID

	logFile, err := os.OpenFile(record.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("opening console log %s: %w", record.LogPath, err)
	}

	jcfg := jailer.Build(id, record.Config.Kernel, m.jailerCfg, logFile, logFile)
	fcCfg := firecracker.BuildRestoreConfig(id, record.Config.Rootfs, jcfg)

	spec := firecracker.RestoreSpec{
		StatePath:   filepath.Join(snap.Dir, storage.SnapshotStateFile),
		MemPath:     filepath.Join(snap.Dir, storage.SnapshotMemFile),
		DiskPath:    record.Config.Rootfs,
		DriveBase:   snap.DriveBase,
		ChrootDir:   jailer.WorkspaceRoot(m.jailerCfg, id),
		TapDevice:   record.Config.TapDevice,
		SnapshotTap: snapshotTapName(snap),
	}

	// Background context for the same reason as boot(): the SDK ties the
	// Firecracker process's lifetime to this context.
	machine, err := firecracker.LaunchFromSnapshot(context.Background(), fcCfg, spec)
	if err != nil {
		_ = logFile.Close()
		return fmt.Errorf("restoring VM from snapshot %s: %w", snap.ID, err)
	}

	pid, _ := machine.PID()
	record.PID = pid
	record.SocketPath = machine.Cfg.SocketPath
	record.VsockPath = filepath.Join(jailer.WorkspaceRoot(m.jailerCfg, id), firecracker.VsockDevicePath)
	record.State = types.VMStateRunning

	m.mu.Lock()
	m.run[id] = &running{machine: machine, logFile: logFile}
	m.mu.Unlock()

	return nil
}

// snapshotTapName returns the TAP name recorded in a snapshot's vmstate,
// deriving it from the source VM's ID for records persisted before the
// TapDevice field existed (the daemon has always named TAPs "tap<vm-id>").
func snapshotTapName(snap *types.Snapshot) string {
	if snap.TapDevice != "" {
		return snap.TapDevice
	}
	if !snap.HadNetwork {
		return ""
	}
	return "tap" + snap.SourceVMID
}

// GetSnapshot returns a single snapshot by ID.
func (m *Manager) GetSnapshot(id string) (*types.Snapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.snaps[id]
	return s, ok
}

// Snapshots returns every snapshot currently tracked.
func (m *Manager) Snapshots() []*types.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]*types.Snapshot, 0, len(m.snaps))
	for _, s := range m.snaps {
		list = append(list, s)
	}
	return list
}

// DeleteSnapshot removes a snapshot's files and record. Safe while VMs
// restored from it are running: their disks are private reflink copies and
// their chroots hold their own hardlinks to the mem/vmstate inodes, so
// nothing they depend on disappears with the snapshot directory.
func (m *Manager) DeleteSnapshot(id string) error {
	m.mu.Lock()
	_, ok := m.snaps[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrSnapshotNotFound, id)
	}

	dirErr := storage.DeleteSnapshotDir(m.instancesDir, id)
	storeErr := m.store.DeleteSnapshot(id)

	m.mu.Lock()
	delete(m.snaps, id)
	m.mu.Unlock()

	return errors.Join(
		wrapErr("removing snapshot dir %s", id, dirErr),
		wrapErr("removing snapshot record %s", id, storeErr),
	)
}

// wrapErr annotates err with a formatted context prefix, or returns nil if
// err is nil so it drops out of an errors.Join cleanly.
func wrapErr(format, id string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(format+": %w", id, err)
}

// DestroyAll destroys every VM currently tracked. Each VM is destroyed
// independently — one failing (e.g. a stuck stop) doesn't stop the rest from
// being cleaned up, so a partial batch still makes maximum progress instead of
// aborting on the first error. Returns the IDs destroyed and a per-ID error for
// any that failed.
func (m *Manager) DestroyAll(ctx context.Context) (deleted []string, failed map[string]error) {
	return m.destroyMatching(ctx, func(*types.VM) bool { return true })
}

// DestroyByNetwork destroys every VM attached to the named network — the
// bulk-cleanup step before deleting the network itself, which otherwise
// refuses while any VM is still attached (see network.Manager.Delete). Same
// independent-failure semantics as DestroyAll.
func (m *Manager) DestroyByNetwork(ctx context.Context, networkName string) (deleted []string, failed map[string]error) {
	return m.destroyMatching(ctx, func(v *types.VM) bool { return v.Config.NetworkName == networkName })
}

func (m *Manager) destroyMatching(ctx context.Context, match func(*types.VM) bool) (deleted []string, failed map[string]error) {
	m.mu.Lock()
	var ids []string
	for id, v := range m.vms {
		if match(v) {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()

	failed = make(map[string]error)
	for _, id := range ids {
		if err := m.Destroy(ctx, id); err != nil {
			failed[id] = err
			continue
		}
		deleted = append(deleted, id)
	}
	return deleted, failed
}

// Exec runs cmd inside a VM over its vsock channel — no SSH key, no IP, no
// network interface needed, works even on a VM created with NoNetwork.
func (m *Manager) Exec(id, cmd string) (string, int, error) {
	m.mu.Lock()
	record, ok := m.vms[id]
	m.mu.Unlock()
	if !ok {
		return "", 0, fmt.Errorf("vm %q not found", id)
	}
	if record.State != types.VMStateRunning {
		return "", 0, fmt.Errorf("%w: vm %s is not running (state %s)", ErrVMState, id, record.State)
	}

	return vsock.Exec(record.VsockPath, cmd)
}

// Get returns a single VM by ID.
func (m *Manager) Get(id string) (*types.VM, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	return v, ok
}

// List returns every VM currently tracked by the manager.
func (m *Manager) List() []*types.VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]*types.VM, 0, len(m.vms))
	for _, v := range m.vms {
		list = append(list, v)
	}
	return list
}

// Templates exposes the catalog for the API layer.
func (m *Manager) Templates() []types.Template {
	return m.catalog.List()
}

func (m *Manager) cleanupNetwork(networkName, tapName, vmID string) {
	if tapName != "" {
		_ = network.DeleteTap(tapName)
	}
	if networkName != "" {
		m.netmgr.DetachVM(networkName, vmID)
	}
}

// processAlive reports whether pid is a live Firecracker process for vmID. The
// vmID check guards against PID reuse — by the time we reconcile, the original
// pid may have been recycled by an unrelated process; Firecracker's own
// command line always carries `--id <vmID>`, so requiring it there means we
// only ever adopt the real VM, never a stranger that inherited its pid.
func processAlive(pid int, vmID string) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), vmID)
}

// stopByPID terminates an adopted VM's process (one this daemon didn't spawn,
// so there's no SDK handle for a graceful API shutdown). It asks politely with
// SIGTERM, waits a few seconds for Firecracker to exit, and escalates to
// SIGKILL only if it's still around. A process that's already gone is success.
func stopByPID(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("sending SIGTERM to pid %d: %w", pid, err)
	}
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return nil
}

// removeIfExists deletes path, treating an already-absent file as success.
func removeIfExists(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
