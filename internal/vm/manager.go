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

	mu  sync.Mutex
	vms map[string]*types.VM
	run map[string]*running
}

// NewManager wires a Manager to its template catalog, jailer defaults, the
// directory where per-VM rootfs clones and console logs are stored, the network
// manager it attaches VMs through, and the store that persists VM records
// across daemon restarts.
func NewManager(catalog *storage.Catalog, jailerCfg jailer.Defaults, instancesDir string, st *store.Store, netmgr *network.Manager) *Manager {
	return &Manager{
		catalog:      catalog,
		jailerCfg:    jailerCfg,
		netmgr:       netmgr,
		instancesDir: instancesDir,
		store:        st,
		vms:          make(map[string]*types.VM),
		run:          make(map[string]*running),
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
		if processAlive(rec.PID, id) {
			m.mu.Lock()
			m.vms[id] = rec
			m.run[id] = &running{} // no SDK handle / log file for adopted VMs
			m.mu.Unlock()

			if tap := rec.Config.TapDevice; tap != "" {
				keepTaps[tap] = true
				m.netmgr.ReserveVM(rec.Config.NetworkName, id, rec.Config.GuestIP)
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

	// Kept short (8 hex chars) because it's reused as part of the tap
	// device name, which Linux caps at 15 characters (IFNAMSIZ).
	id := uuid.NewString()[:8]

	rootfsPath, err := storage.CloneRootfs(tpl, id, m.instancesDir, m.jailerCfg.UID, m.jailerCfg.GID)
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

	logPath := filepath.Join(m.instancesDir, id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		m.cleanupNetwork(networkName, tapName, id)
		_ = storage.DeleteClone(m.instancesDir, id)
		return nil, fmt.Errorf("opening console log %s: %w", logPath, err)
	}

	vmCfg := types.VMConfig{
		ID:           id,
		TemplateName: tpl.Name,
		Kernel:       tpl.KernelPath,
		Rootfs:       rootfsPath,
		VCPUs:        vcpus,
		MemMB:        memMB,
		NetworkName:  networkName,
		Bridge:       bridge,
		TapDevice:    tapName,
		GuestIP:      guestIP,
		GatewayIP:    gatewayIP,
		PrefixLen:    prefixLen,
	}

	// stdout/stderr point at the VM's own log file, never at the daemon's
	// terminal — see the comment on jailer.Build for why.
	jcfg := jailer.Build(id, tpl.KernelPath, m.jailerCfg, logFile, logFile)
	fcCfg, err := firecracker.BuildConfig(vmCfg, jcfg)
	if err != nil {
		_ = logFile.Close()
		m.cleanupNetwork(networkName, tapName, id)
		_ = storage.DeleteClone(m.instancesDir, id)
		return nil, err
	}

	// Deliberately not `ctx`: the SDK ties the Firecracker process's lifetime
	// to whatever context it's launched with (exec.CommandContext under the
	// hood). ctx here is request-scoped and dies the moment this HTTP call
	// returns, which would kill the VM right after it finished booting. The
	// VM's lifetime is the manager's, not any single API request's.
	machine, err := firecracker.Launch(context.Background(), fcCfg)
	if err != nil {
		_ = logFile.Close()
		m.cleanupNetwork(networkName, tapName, id)
		_ = storage.DeleteClone(m.instancesDir, id)
		return nil, fmt.Errorf("launching VM: %w", err)
	}

	pid, _ := machine.PID()

	record := &types.VM{
		Config: vmCfg,
		State:  types.VMStateRunning,
		PID:    pid,
		// machine.Cfg.SocketPath (not fcCfg.SocketPath) because Jailer
		// rewrites it to the absolute path inside the chroot once launched.
		SocketPath: machine.Cfg.SocketPath,
		LogPath:    logPath,
		// Unlike SocketPath, the SDK does NOT rewrite VsockDevice.Path into
		// the chroot for us — build the host-side path ourselves using the
		// same convention jailer.WorkspaceRoot encodes.
		VsockPath: filepath.Join(jailer.WorkspaceRoot(m.jailerCfg, id), firecracker.VsockDevicePath),
		CreatedAt: time.Now(),
	}

	m.mu.Lock()
	m.vms[id] = record
	m.run[id] = &running{machine: machine, logFile: logFile}
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
