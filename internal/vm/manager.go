package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/google/uuid"

	"microhosted/internal/events"
	"microhosted/internal/faults"
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
// the single seam meant for future extension — persistence (Stage 8),
// multi-host placement (Stage 9) and snapshot/restore (Stage 6) all plug
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

	mu  sync.Mutex
	vms map[string]*types.VM
	// names indexes VM names to IDs. A name is claimed here before its VM's
	// record exists (see reserveNameLocked), so it can hold names m.vms does
	// not have yet.
	names map[string]string
	run   map[string]*running
	snaps map[string]*types.Snapshot
	vols  map[string]*types.Volume
	// volIO/vmIO mark a volume or a VM's disk as busy with an offline debugfs
	// operation. debugfs runs unlocked (a multi-GB transfer can't hold the
	// manager lock), so these guard against a concurrent attach/boot writing
	// the same ext4 out from under it — writing a mounted filesystem corrupts
	// it. Both are set/cleared under mu.
	volIO map[string]bool
	vmIO  map[string]bool

	// busy is the lifecycle operation in progress per VM (see begin); a VM
	// being created or forked is in it before it is in vms.
	busy map[string]string
	// Admission (see admit): the limits, one token per launch allowed to run
	// at once, and the launches admitted but not finished — their memory is
	// not visible in MemAvailable yet.
	limits     Limits
	bootSlots  chan struct{}
	inflight   map[string]bool
	inflightMB int64
	// memAvailable reads the host's available memory in MB; a variable so
	// tests can set the host they need.
	memAvailable func() (int64, error)
	// replaceLaunch boots Replace's new VM; nil means launchReplacement. A
	// variable so tests can drive Replace without Firecracker.
	replaceLaunch func(context.Context, replacement) (*types.VM, error)
	// events receives the lifecycle events (see SetEvents); nil drops them.
	events *events.Bus
}

// NewManager wires a Manager to its template catalog, jailer defaults, the
// directory where per-VM rootfs clones and console logs are stored, the network
// manager it attaches VMs through, and the store that persists VM records
// across daemon restarts.
func NewManager(catalog *storage.Catalog, jailerCfg jailer.Defaults, instancesDir string, st *store.Store, netmgr *network.Manager) *Manager {
	// Sweep any staging temps a previous run left mid-transfer (a crash between
	// stageFile and its deferred Remove). Safe at startup: no transfer is live
	// yet. Best-effort — a failure here just means a few stale files, not a
	// reason to refuse to boot.
	_ = os.RemoveAll(filepath.Join(instancesDir, "staging"))

	return &Manager{
		catalog:        catalog,
		jailerCfg:      jailerCfg,
		netmgr:         netmgr,
		instancesDir:   instancesDir,
		store:          st,
		netOverridesOK: firecracker.SupportsNetworkOverrides(jailerCfg.ExecFile),
		vms:            make(map[string]*types.VM),
		names:          make(map[string]string),
		run:            make(map[string]*running),
		snaps:          make(map[string]*types.Snapshot),
		vols:           make(map[string]*types.Volume),
		volIO:          make(map[string]bool),
		vmIO:           make(map[string]bool),
		busy:           make(map[string]string),
		inflight:       make(map[string]bool),
		limits:         Limits{MemReserveMB: DefaultMemReserveMB, MaxParallelBoots: DefaultMaxParallelBoots},
		bootSlots:      make(chan struct{}, DefaultMaxParallelBoots),
		memAvailable:   hostMemAvailable,
	}
}

// LoadVolumes seeds the manager's volume index from persisted records at
// startup. Like snapshots there's no process liveness to reconcile, but a
// volume whose backing file vanished (removed out-of-band) is dropped rather
// than offered for an attach that can only fail. Must run before Reconcile so
// the sweep of dead VMs can find and release their volumes.
func (m *Manager) LoadVolumes(records []*types.Volume) {
	for _, vol := range records {
		if _, err := os.Stat(vol.Path); err != nil {
			log.Printf("reconcile: dropping volume %s (%s): file %s missing", vol.ID, vol.Name, vol.Path)
			_ = m.store.DeleteVolume(vol.ID)
			continue
		}
		m.mu.Lock()
		m.vols[vol.ID] = vol
		m.mu.Unlock()
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
//   - a VM whose process is gone died while the daemon was down — a host
//     reboot kills every Firecracker process, so after one this is every VM.
//     Its disk is intact, so it is kept as stopped, exactly as if it had been
//     powered off: disk, IP reservation and volumes survive and Start brings it
//     back. Only a VM whose disk is gone too is swept and forgotten.
//   - a VM stopped on purpose is kept stopped.
//
// It returns the set of tap device names belonging to adopted VMs so the
// caller can exclude them from network.SweepOrphans — they're live, not
// orphans — and the IDs of dead VMs that asked to come back on their own
// (VMConfig.Autostart), for AutostartVMs. A VM stopped on purpose is never in
// that list. Must be called before the API server starts serving.
func (m *Manager) Reconcile(records []*types.VM) (keepTaps map[string]bool, autostart []string) {
	keepTaps = make(map[string]bool)

	for _, rec := range records {
		id := rec.Config.ID

		// The daemon died while this VM was being created or forked: it was
		// never handed to anyone, so it is undone, not recovered.
		if rec.State == types.VMStateCreating {
			log.Printf("reconcile: vm %s was being created when the daemon stopped; undoing it", id)
			m.undoCreate(rec)
			continue
		}

		if rec.State != types.VMStateStopped {
			if m.adoptIfAlive(rec, keepTaps) {
				m.adoptName(rec)
				continue
			}
			if !m.keepDead(rec) {
				continue
			}
			if rec.Config.Autostart {
				autostart = append(autostart, id)
			}
		} else {
			log.Printf("reconcile: kept stopped vm %s", id)
		}

		// A stopped VM (on purpose, or dead and just converted) has no live
		// process by design. Keep it tracked as stopped and re-reserve its IP so
		// a freshly created VM can't grab the address it will reclaim on Start.
		// It has no TAP (Stop / keepDead released it), so there's nothing to add
		// to keepTaps.
		m.mu.Lock()
		m.vms[id] = rec
		m.run[id] = &running{}
		m.mu.Unlock()
		m.adoptName(rec)
		// Quarantined forks hold no reservation: their GuestIP is only what the
		// restored guest believes it has, not an IPAM lease.
		if rec.Config.GuestIP != "" && rec.Config.NetworkName != "" {
			m.netmgr.ReserveVM(rec.Config.NetworkName, id, rec.Config.GuestIP)
		}
	}

	return keepTaps, autostart
}

// adoptIfAlive tracks a VM whose Firecracker process survived the daemon's
// restart, adding its TAP to keepTaps. Reports false if the process is gone.
func (m *Manager) adoptIfAlive(rec *types.VM, keepTaps map[string]bool) bool {
	id := rec.Config.ID
	if !processAlive(rec.PID, id) {
		return false
	}
	m.mu.Lock()
	m.vms[id] = rec
	m.run[id] = &running{} // no SDK handle / log file for adopted VMs
	m.mu.Unlock()

	if tap := rec.Config.TapDevice; tap != "" {
		keepTaps[tap] = true
		// A quarantined VM's TAP is live and must be kept, but there is no
		// network to re-reserve on. Detaching again is a no-op for a TAP
		// already off its bridge, and fail-closed for one that somehow isn't.
		if rec.Config.Quarantine {
			if err := network.DetachTap(tap); err != nil {
				log.Printf("reconcile: vm %s: %v", id, err)
			}
		}
		if rec.Config.NetworkName != "" {
			m.netmgr.ReserveVM(rec.Config.NetworkName, id, rec.Config.GuestIP)
			// The TAP predates this daemon run — converge it to the record
			// (on its bridge: a quarantine the daemon died in the middle of
			// was never acknowledged, so it is undone) and to the network's
			// current intra policy. Best-effort: a failure leaves stale
			// reachability or a VM cut off, logged, not a daemon that won't
			// start.
			isolated := !m.netmgr.Intra(rec.Config.NetworkName)
			var err error
			if rec.Config.Bridge != "" {
				err = network.EnslaveTap(tap, rec.Config.Bridge, isolated)
			} else {
				err = network.SetTapIsolation(tap, isolated)
			}
			if err != nil {
				log.Printf("reconcile: vm %s: %v", id, err)
			}
		}
	}
	log.Printf("reconcile: adopted running vm %s (pid %d)", id, rec.PID)
	return true
}

// keepDead turns the record of a VM that died while the daemon was down into a
// stopped one: it releases what a powered-off VM doesn't hold (the TAP, if it
// outlived the process, and the jail dir) and persists the new state — the same
// end state Stop leaves. The IP reservation and volume claims are kept.
//
// A VM whose disk is gone has nothing left to boot: that one is swept instead —
// every leftover released and its record dropped — and keepDead reports false.
func (m *Manager) keepDead(rec *types.VM) bool {
	id := rec.Config.ID
	if _, err := os.Stat(rec.Config.Rootfs); err != nil {
		m.cleanupNetwork(rec.Config.NetworkName, rec.Config.TapDevice, id)
		m.releaseVolumes(id, rec.Config.Volumes)
		_ = storage.DeleteClone(m.instancesDir, id)
		_ = jailer.RemoveInstanceDir(m.jailerCfg, id)
		_ = removeIfExists(rec.LogPath)
		if err := m.store.DeleteVM(id); err != nil {
			log.Printf("reconcile: dropping dead vm record %s: %v", id, err)
		}
		log.Printf("reconcile: swept dead vm %s (pid %d no longer running, disk %q missing)", id, rec.PID, rec.Config.Rootfs)
		return false
	}

	if rec.Config.TapDevice != "" {
		_ = network.DeleteTap(rec.Config.TapDevice)
	}
	_ = jailer.RemoveInstanceDir(m.jailerCfg, id)
	deadPID := rec.PID
	rec.State = types.VMStateStopped
	rec.PID = 0
	rec.SocketPath = ""
	rec.VsockPath = ""
	// Best-effort: if this write fails the record still says running, and the
	// next startup lands here again with the same outcome.
	if err := m.store.SaveVM(rec); err != nil {
		log.Printf("reconcile: persisting vm %s as stopped: %v", id, err)
	}
	log.Printf("reconcile: vm %s died while the daemon was down (pid %d); kept as stopped", id, deadPID)
	m.emit(types.EventVMDied, rec, "died while the daemon was down (host reboot, or killed meanwhile)", nil)
	return true
}

// AutostartVMs boots the dead VMs Reconcile found with Autostart set. It must
// run after network.SweepOrphans — the sweep would delete the TAPs Start
// creates. One at a time, so a host coming back from a reboot doesn't launch
// its whole fleet at once; a failure is logged and leaves that VM stopped, to
// be started by hand.
func (m *Manager) AutostartVMs(ids []string) {
	for _, id := range ids {
		if _, err := m.Start(context.Background(), id); err != nil {
			log.Printf("autostart: vm %s: %v", id, err)
			if rec, ok := m.Get(id); ok {
				m.emit(types.EventVMAutostartFail, rec, err.Error(), nil)
			}
			continue
		}
		log.Printf("autostart: started vm %s", id)
	}
}

// SetAutostart flips whether a VM boots again on its own after a host reboot
// (see VMConfig.Autostart). Persisted before it is reported: on a store failure
// the in-memory flag is rolled back.
func (m *Manager) SetAutostart(id string, on bool) (*types.VM, error) {
	m.mu.Lock()
	record, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	prev := record.Config.Autostart
	record.Config.Autostart = on
	cp := *record
	m.mu.Unlock()

	if err := m.store.SaveVM(&cp); err != nil {
		m.mu.Lock()
		record.Config.Autostart = prev
		m.mu.Unlock()
		return nil, fmt.Errorf("persisting vm %s: %w", id, err)
	}
	return &cp, nil
}

// CreateVolume provisions a new persistent volume: a fresh ext4 image on the
// CoW store plus its record. Volumes exist independently of VMs — they're the
// data plane (a read-only sample, a writable artifact scratch disk) and survive
// a VM's destruction.
func (m *Manager) CreateVolume(req types.CreateVolumeRequest) (*types.Volume, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("volume name is required")
	}
	m.mu.Lock()
	for _, v := range m.vols {
		if v.Name == req.Name {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: a volume named %q already exists", ErrConflict, req.Name)
		}
	}
	m.mu.Unlock()

	id := uuid.NewString()[:8]
	path, err := storage.CreateVolume(m.instancesDir, id, req.SizeMB, m.jailerCfg.UID, m.jailerCfg.GID)
	if err != nil {
		return nil, err
	}

	vol := &types.Volume{
		ID:        id,
		Name:      req.Name,
		SizeMB:    req.SizeMB,
		Path:      path,
		CreatedAt: time.Now(),
	}
	if err := m.store.SaveVolume(vol); err != nil {
		_ = storage.DeleteVolume(m.instancesDir, id)
		return nil, fmt.Errorf("persisting volume %s: %w", req.Name, err)
	}

	m.mu.Lock()
	m.vols[id] = vol
	m.mu.Unlock()
	return vol, nil
}

// DeleteVolume removes a volume's image and record. Refuses while the volume is
// attached to a VM — the data would vanish from under a running guest, and the
// VM's config still references it. Detach by destroying that VM first.
func (m *Manager) DeleteVolume(id string) error {
	m.mu.Lock()
	vol, ok := m.vols[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: volume %s", ErrVMNotFound, id)
	}
	if vol.AttachedTo != "" {
		m.mu.Unlock()
		return fmt.Errorf("%w: volume %s is attached to vm %s — destroy it first", ErrConflict, id, vol.AttachedTo)
	}
	delete(m.vols, id)
	m.mu.Unlock()

	fileErr := storage.DeleteVolume(m.instancesDir, id)
	storeErr := m.store.DeleteVolume(id)
	return errors.Join(
		wrapErr("removing volume image %s", id, fileErr),
		wrapErr("removing volume record %s", id, storeErr),
	)
}

// GetVolume returns a single volume by ID.
func (m *Manager) GetVolume(id string) (*types.Volume, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vols[id]
	return v, ok
}

// Volumes returns every volume currently tracked.
func (m *Manager) Volumes() []*types.Volume {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]*types.Volume, 0, len(m.vols))
	for _, v := range m.vols {
		list = append(list, v)
	}
	return list
}

// attachVolumes resolves each attach request by volume name, claims the volume
// for vmID (rejecting one already held by another VM, or named twice in one
// request), and returns the VolumeMounts to record on the VM. The claim is taken
// under the lock so two concurrent Creates can't grab the same volume; on any
// failure every claim made so far is released before returning.
func (m *Manager) attachVolumes(vmID string, reqs []types.VolumeAttachRequest) ([]types.VolumeMount, error) {
	if len(reqs) == 0 {
		return nil, nil
	}

	m.mu.Lock()
	byName := make(map[string]*types.Volume, len(m.vols))
	for _, v := range m.vols {
		byName[v.Name] = v
	}

	mounts := make([]types.VolumeMount, 0, len(reqs))
	claimed := make([]*types.Volume, 0, len(reqs))
	seen := make(map[string]bool, len(reqs))
	for _, req := range reqs {
		vol, ok := byName[req.Name]
		if !ok {
			m.releaseClaimsLocked(claimed)
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: volume %q", ErrVMNotFound, req.Name)
		}
		if seen[vol.ID] {
			m.releaseClaimsLocked(claimed)
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: volume %q attached more than once", ErrConflict, req.Name)
		}
		if vol.AttachedTo != "" {
			m.releaseClaimsLocked(claimed)
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: volume %q is already attached to vm %s", ErrConflict, req.Name, vol.AttachedTo)
		}
		if m.volIO[vol.ID] {
			m.releaseClaimsLocked(claimed)
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: volume %q is busy with an offline file operation", ErrConflict, req.Name)
		}
		vol.AttachedTo = vmID
		seen[vol.ID] = true
		claimed = append(claimed, vol)

		guestPath := req.GuestPath
		if guestPath == "" {
			guestPath = "/vol/" + vol.Name
		}
		mounts = append(mounts, types.VolumeMount{
			VolumeID:   vol.ID,
			VolumeName: vol.Name,
			ReadOnly:   req.ReadOnly,
			GuestPath:  guestPath,
			// Underscore, not hyphen: Firecracker rejects a drive_id with any
			// non-alphanumeric/underscore character ("API Resource IDs can only
			// contain alphanumeric characters and underscores").
			DriveID:  "vol_" + vol.ID,
			HostPath: vol.Path,
		})
	}
	m.mu.Unlock()

	// Persist the claims so a restart still sees them attached. A persist failure
	// rolls every claim back — a volume marked attached in memory but not on disk
	// would desync after a restart.
	for _, vol := range claimed {
		if err := m.store.SaveVolume(vol); err != nil {
			m.releaseVolumes(vmID, mounts)
			return nil, fmt.Errorf("persisting volume attachment %s: %w", vol.Name, err)
		}
	}
	return mounts, nil
}

// releaseClaimsLocked clears AttachedTo on volumes claimed earlier in the same
// attach attempt. Caller must hold m.mu. In-memory only — these claims were
// never persisted (attachVolumes persists after the whole loop succeeds).
func (m *Manager) releaseClaimsLocked(claimed []*types.Volume) {
	for _, vol := range claimed {
		vol.AttachedTo = ""
	}
}

// releaseVolumes detaches vmID's volumes: clears AttachedTo and persists, so the
// volumes become free for another VM. The image files are left untouched — a
// volume's whole point is to outlive the VM. Errors are logged, not returned:
// a detach is part of teardown and shouldn't fail the caller.
func (m *Manager) releaseVolumes(vmID string, mounts []types.VolumeMount) {
	if len(mounts) == 0 {
		return
	}
	m.mu.Lock()
	toSave := make([]*types.Volume, 0, len(mounts))
	for _, mt := range mounts {
		if vol, ok := m.vols[mt.VolumeID]; ok && vol.AttachedTo == vmID {
			vol.AttachedTo = ""
			toSave = append(toSave, vol)
		}
	}
	m.mu.Unlock()
	for _, vol := range toSave {
		if err := m.store.SaveVolume(vol); err != nil {
			log.Printf("releasing volume %s from vm %s: %v", vol.ID, vmID, err)
		}
	}
}

// mountVolumes waits for the guest's vsock agent to come up, then mounts each
// attached volume inside the guest. Firecracker exposes the secondary drives as
// /dev/vdb, /dev/vdc… in VMConfig.Volumes order, so index i maps to /dev/vd{b+i}.
// A read-only attachment is mounted -o ro. A volume with no GuestPath is left as
// a raw device for the guest to mount itself.
func (m *Manager) mountVolumes(record *types.VM) error {
	if len(record.Config.Volumes) == 0 {
		return nil
	}
	if err := m.waitAgentReady(record.VsockPath); err != nil {
		return err
	}
	for i, mt := range record.Config.Volumes {
		if mt.GuestPath == "" {
			continue
		}
		device := "/dev/vd" + string(rune('b'+i))
		opts := ""
		if mt.ReadOnly {
			opts = "-o ro "
		}
		cmd := fmt.Sprintf("mkdir -p %s && mount %s%s %s", mt.GuestPath, opts, device, mt.GuestPath)
		out, code, err := vsock.Exec(record.VsockPath, cmd)
		if err != nil {
			return fmt.Errorf("mounting volume %s at %s: %w", mt.VolumeName, mt.GuestPath, err)
		}
		if code != 0 {
			return fmt.Errorf("mounting volume %s at %s failed (exit %d): %s", mt.VolumeName, mt.GuestPath, code, strings.TrimSpace(out))
		}
	}
	return nil
}

// waitAgentReady polls the guest's vsock exec agent until it answers, so an
// auto-mount issued right after boot doesn't race the guest still coming up.
func (m *Manager) waitAgentReady(vsockPath string) error {
	const (
		budget   = 30 * time.Second
		interval = 500 * time.Millisecond
	)
	deadline := time.Now().Add(budget)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, _, err := vsock.Exec(vsockPath, "true"); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("guest vsock agent not ready after %s: %w", budget, lastErr)
}

// Create clones a template's disk, wires up networking, and launches a new
// microVM through Jailer. The record is written as "creating" before the first
// side effect, so any failure — here, or the daemon dying in the middle — is
// undone completely (see undoCreate): the result is a running VM or nothing.
func (m *Manager) Create(ctx context.Context, req types.CreateVMRequest) (*types.VM, error) {
	if err := errors.Join(validateShape(req.VCPUs, req.MemMB, req.DiskMB), ValidateName(req.Name), ValidateLabels(req.Labels)); err != nil {
		return nil, err
	}
	if req.GuestIP != "" && req.NoNetwork {
		return nil, fmt.Errorf("%w: guest_ip needs a network, and no_network is set", ErrInvalid)
	}
	tpl, err := m.catalog.Get(req.Template)
	if err != nil {
		return nil, err
	}
	if err := checkTemplateGoldens(tpl); err != nil {
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
	if err := m.begin(id, "create"); err != nil {
		return nil, err
	}
	defer m.end(id)
	release, err := m.admit(ctx, id, memMB)
	if err != nil {
		return nil, err
	}
	defer release()

	record := &types.VM{
		Config: types.VMConfig{
			ID:           id,
			Name:         req.Name,
			Labels:       maps.Clone(req.Labels),
			TemplateName: tpl.Name,
			Kernel:       tpl.KernelPath,
			VCPUs:        vcpus,
			MemMB:        memMB,
			DiskMB:       diskMB,
			Autostart:    req.Autostart,
		},
		State:     types.VMStateCreating,
		LogPath:   filepath.Join(m.instancesDir, id+".log"),
		CreatedAt: time.Now(),
	}
	if err := m.reserveName(req.Name, id); err != nil {
		return nil, err
	}
	if err := m.store.SaveVM(record); err != nil {
		m.releaseName(req.Name, id)
		return nil, fmt.Errorf("recording vm %s before creating it: %w", id, err)
	}
	fail := func(err error) (*types.VM, error) {
		m.undoCreate(record)
		return nil, err
	}
	if err := faults.Check("vm.create.after-record"); err != nil {
		return fail(err)
	}

	rootfsPath, err := storage.CloneRootfs(tpl, id, m.instancesDir, m.jailerCfg.UID, m.jailerCfg.GID, diskMB)
	if err != nil {
		return fail(fmt.Errorf("cloning rootfs: %w", err))
	}
	record.Config.Rootfs = rootfsPath
	if err := faults.Check("vm.create.after-clone"); err != nil {
		return fail(err)
	}

	// Network is opt-out, not mandatory: sandboxed/ephemeral workloads often
	// shouldn't have any path to the host at all (see NoNetwork's doc comment).
	// When on, the VM joins a segmented network (the default one if unspecified):
	// its TAP is enslaved to that network's bridge and it gets an IP from the
	// network's subnet. Isolated across networks always; within a network VMs
	// see each other only if the network opted in (intra) — otherwise the TAP
	// is an isolated bridge port.
	if !req.NoNetwork {
		networkName := req.Network
		if networkName == "" {
			networkName = network.DefaultNetworkName
		}
		var ip, gw, br string
		var prefix int
		if req.GuestIP != "" {
			ip = req.GuestIP
			gw, br, prefix, err = m.netmgr.ClaimVM(networkName, id, ip)
			switch {
			case errors.Is(err, network.ErrAddressInUse):
				return fail(fmt.Errorf("%w: network %q: %v", ErrConflict, networkName, err))
			case errors.Is(err, network.ErrBadAddress):
				return fail(fmt.Errorf("%w: network %q: %v", ErrInvalid, networkName, err))
			}
		} else {
			ip, gw, br, prefix, err = m.netmgr.AttachVM(networkName, id)
		}
		if err != nil {
			return fail(fmt.Errorf("attaching to network %q: %w", networkName, err))
		}
		record.Config.NetworkName = networkName
		record.Config.Bridge = br
		record.Config.GuestIP = ip
		record.Config.GatewayIP = gw
		record.Config.PrefixLen = prefix

		tapName := "tap" + id
		if err := network.CreateTapEnslaved(tapName, br, !m.netmgr.Intra(networkName)); err != nil {
			return fail(fmt.Errorf("creating tap device: %w", err))
		}
		record.Config.TapDevice = tapName
	}
	if err := faults.Check("vm.create.after-network"); err != nil {
		return fail(err)
	}

	// Attach any requested volumes before boot: their drives must be in the
	// Firecracker config (built inside boot).
	mounts, err := m.attachVolumes(id, req.Volumes)
	if err != nil {
		return fail(fmt.Errorf("attaching volumes: %w", err))
	}
	record.Config.Volumes = mounts

	// boot opens the console log and launches Firecracker, filling in the
	// runtime fields.
	if err := m.boot(record); err != nil {
		return fail(err)
	}
	if err := faults.Check("vm.create.after-boot"); err != nil {
		return fail(err)
	}

	m.mu.Lock()
	m.vms[id] = record
	m.mu.Unlock()

	// Mount attached volumes inside the guest over vsock, now that it's up. A
	// half-mounted VM isn't what the caller asked for.
	if err := m.mountVolumes(record); err != nil {
		return fail(fmt.Errorf("mounting volumes: %w", err))
	}
	if err := faults.Check("vm.create.before-save"); err != nil {
		return fail(err)
	}

	// Persisting the running state is what ends the create: until this
	// write, a restart treats the VM as never having existed.
	if err := m.store.SaveVM(record); err != nil {
		return fail(fmt.Errorf("persisting vm record %s: %w", id, err))
	}

	m.emit(types.EventVMCreated, record, "", map[string]string{"template": tpl.Name})
	return m.snapshotVM(record), nil
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
	if err := m.applyLimits(id, pid, record.Config); err != nil {
		_ = firecracker.Kill(context.Background(), machine)
		_ = logFile.Close()
		return err
	}

	m.mu.Lock()
	record.PID = pid
	// machine.Cfg.SocketPath (not fcCfg.SocketPath) because Jailer rewrites it
	// to the absolute path inside the chroot once launched.
	record.SocketPath = machine.Cfg.SocketPath
	// Unlike SocketPath, the SDK does NOT rewrite VsockDevice.Path into the
	// chroot for us — build the host-side path ourselves using the same
	// convention jailer.WorkspaceRoot encodes.
	record.VsockPath = filepath.Join(jailer.WorkspaceRoot(m.jailerCfg, id), firecracker.VsockDevicePath)
	record.State = types.VMStateRunning
	m.run[id] = &running{machine: machine, logFile: logFile}
	m.mu.Unlock()

	return nil
}

// applyLimits caps a just-launched VM's host resources by writing cgroup v2
// limits (CPU, memory, PIDs — sized from its own config) into its Jailer
// cgroup. Fail-closed: a VM the host can't cap is exactly the DoS vector the
// limits exist to prevent, so an error here aborts the boot and the caller
// kills the process. The one tolerated failure is a cgroup v1 host, where
// limits simply aren't supported — those hosts predate this feature and keep
// working, with a loud warning per boot.
func (m *Manager) applyLimits(id string, pid int, cfg types.VMConfig) error {
	// The daemon runs with a strongly negative oom_score_adj (see
	// deploy/microhosted.service.in) so that, under host memory pressure, the
	// kernel kills a VM — which the monitor reports — and never the process
	// holding every VM. Children inherit it; Firecracker must not.
	if err := os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), []byte("0"), 0o644); err != nil {
		log.Printf("vm %s: resetting oom_score_adj of pid %d: %v", id, pid, err)
	}
	err := jailer.ApplyLimits(m.jailerCfg, id, pid, cfg.VCPUs, cfg.MemMB)
	if err == nil {
		return nil
	}
	if errors.Is(err, jailer.ErrCgroupV1) {
		log.Printf("vm %s: host on cgroup v1 — CPU/memory/PID limits NOT applied (guest can contend for host resources)", id)
		return nil
	}
	return fmt.Errorf("applying resource limits to vm %s: %w", id, err)
}

// powerOff stops a VM's running process and releases its TAP + jail dir, leaving
// the disk and IP intact — the shared teardown behind Stop and behind a failed
// volume auto-mount on Start. It does NOT touch the store or the VM's tracked
// state; the caller decides what state to record.
func (m *Manager) powerOff(ctx context.Context, record *types.VM, r *running) error {
	id := record.Config.ID
	var stopErr error
	switch {
	case r != nil && r.machine != nil:
		stopErr = firecracker.Stop(ctx, r.machine)
	case record.PID > 0:
		stopErr = stopByPID(record.PID)
	}
	if r != nil && r.logFile != nil {
		_ = r.logFile.Close()
	}
	if record.Config.TapDevice != "" {
		_ = network.DeleteTap(record.Config.TapDevice)
	}
	jailErr := jailer.RemoveInstanceDir(m.jailerCfg, id)
	return errors.Join(
		wrapErr("stopping vm %s", id, stopErr),
		wrapErr("removing jail dir for vm %s", id, jailErr),
	)
}

// Destroy stops the machine and releases every resource associated with it.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	if err := m.begin(id, "destroy"); err != nil {
		return err
	}
	defer m.end(id)
	return m.destroy(ctx, id)
}

// destroy is Destroy for a caller that already holds the VM's busy mark.
func (m *Manager) destroy(ctx context.Context, id string) error {
	m.mu.Lock()
	record, ok := m.vms[id]
	r := m.run[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}

	var stopErr error
	switch {
	case r != nil && r.machine != nil:
		// Normal path: we own a live SDK handle from this daemon's lifetime.
		// Kill, not Stop: the disk is deleted below, so there's nothing for a
		// graceful guest power-off to protect, and skipping its 5s window is
		// what keeps Destroy fast.
		stopErr = firecracker.Kill(ctx, r.machine)
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
	// Detach volumes (clear their AttachedTo) but keep their image files: a
	// volume is persistent by definition and survives the VM it was attached to.
	m.releaseVolumes(id, record.Config.Volumes)
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
	m.releaseName(record.Config.Name, id)
	m.emit(types.EventVMDestroyed, record, "", nil)

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
	if err := m.begin(id, "stop"); err != nil {
		return nil, err
	}
	defer m.end(id)
	return m.stop(ctx, id)
}

// stop is Stop for a caller that already holds the VM's busy mark.
func (m *Manager) stop(ctx context.Context, id string) (*types.VM, error) {
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
	m.emit(types.EventVMStopped, record, "", nil)

	return m.snapshotVM(record), errors.Join(
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
	if err := m.begin(id, "start"); err != nil {
		return nil, err
	}
	defer m.end(id)
	m.mu.Lock()
	record, ok := m.vms[id]
	busyIO := m.vmIO[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	// An offline file op is writing this VM's disk right now; booting Firecracker
	// on it mid-write would corrupt it. Make the caller retry once the op is done.
	if busyIO {
		return nil, fmt.Errorf("%w: vm %s is busy with an offline file operation", ErrConflict, id)
	}
	if record.State != types.VMStateStopped {
		return nil, fmt.Errorf("%w: vm %s is not stopped (state %s)", ErrVMState, id, record.State)
	}
	release, err := m.admit(ctx, id, record.Config.MemMB)
	if err != nil {
		return nil, err
	}
	defer release()

	// Recreate the TAP and re-enslave it to its network's bridge. No AttachVM:
	// the IP is still reserved from before, so we reuse record.Config.GuestIP.
	// A quarantined fork gets its bridge-less TAP back instead — starting it
	// cold must not quietly connect it to a network its whole point is to be
	// off of.
	if record.Config.TapDevice != "" {
		if err := m.recreateTap(record.Config); err != nil {
			return nil, fmt.Errorf("recreating tap for vm %s: %w", id, err)
		}
	}

	if err := m.boot(record); err != nil {
		if record.Config.TapDevice != "" {
			_ = network.DeleteTap(record.Config.TapDevice)
		}
		return nil, err
	}

	// Re-mount the VM's volumes (the block devices are back with the fresh boot).
	// On failure, power the VM back off and leave it stopped — the volumes stay
	// attached and the disk is intact, so Start is retryable; unlike Create, a
	// failed Start must not destroy an existing VM.
	if err := m.mountVolumes(record); err != nil {
		m.mu.Lock()
		r := m.run[id]
		m.mu.Unlock()
		_ = m.powerOff(ctx, record, r)
		m.mu.Lock()
		record.State = types.VMStateStopped
		record.PID = 0
		record.SocketPath = ""
		record.VsockPath = ""
		m.run[id] = &running{}
		m.mu.Unlock()
		_ = m.store.SaveVM(record)
		return nil, fmt.Errorf("mounting volumes for vm %s: %w", id, err)
	}

	if err := m.store.SaveVM(record); err != nil {
		_ = m.destroy(context.Background(), id)
		return nil, fmt.Errorf("persisting restarted vm %s: %w", id, err)
	}

	m.emit(types.EventVMStarted, record, "", nil)
	return m.snapshotVM(record), nil
}

// SyncTapIsolation re-applies the isolated-port flag to every live TAP on the
// named network — the enforcement half of a hot intra toggle (the flag itself
// is persisted by network.Manager.UpdateIntra). Stopped VMs have no TAP and
// pick the current flag up at Start; quarantined forks carry no network name
// and are never touched. Partial failure returns an error naming what's left
// unconverged — on a tightening, a tap that kept its old reachability is a
// security hole the caller must surface, not a footnote for the log.
func (m *Manager) SyncTapIsolation(networkName string, isolated bool) error {
	m.mu.Lock()
	var taps []string
	for _, rec := range m.vms {
		if rec.Config.NetworkName == networkName && rec.Config.TapDevice != "" && rec.State != types.VMStateStopped {
			taps = append(taps, rec.Config.TapDevice)
		}
	}
	m.mu.Unlock()

	var errs []error
	for _, tap := range taps {
		if err := network.SetTapIsolation(tap, isolated); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recreateTap rebuilds a VM's TAP device according to its config: enslaved to
// its network's bridge, or deliberately bridge-less for a quarantined fork.
// Isolation is re-derived from the network's CURRENT intra flag, not a stored
// copy, so a restarted VM always lands on the network's present policy.
func (m *Manager) recreateTap(cfg types.VMConfig) error {
	if cfg.Quarantine {
		return network.CreateTapQuarantined(cfg.TapDevice)
	}
	return network.CreateTapEnslaved(cfg.TapDevice, cfg.Bridge, !m.netmgr.Intra(cfg.NetworkName))
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
	if err := m.begin(vmID, "snapshot"); err != nil {
		return nil, err
	}
	defer m.end(vmID)
	m.mu.Lock()
	record, ok := m.vms[vmID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	if record.State != types.VMStateRunning {
		return nil, fmt.Errorf("%w: vm %s is not running (state %s)", ErrVMState, vmID, record.State)
	}
	if len(record.Config.Volumes) > 0 {
		return nil, errVolumesAttached(vmID)
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
func (m *Manager) Fork(ctx context.Context, snapID string, req types.ForkVMRequest) (*types.VM, error) {
	if err := errors.Join(ValidateName(req.Name), ValidateLabels(req.Labels)); err != nil {
		return nil, err
	}
	quarantine := req.Quarantine
	m.mu.Lock()
	snap, ok := m.snaps[snapID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapID)
	}

	id := uuid.NewString()[:8]
	if err := m.begin(id, "fork"); err != nil {
		return nil, err
	}
	defer m.end(id)
	release, err := m.admit(ctx, id, snap.MemMB)
	if err != nil {
		return nil, err
	}
	defer release()

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

	// Recorded before the first side effect, same contract as Create.
	record := &types.VM{
		Config: types.VMConfig{
			ID:           id,
			Name:         req.Name,
			Labels:       maps.Clone(req.Labels),
			TemplateName: snap.TemplateName,
			Kernel:       kernel,
			VCPUs:        snap.VCPUs,
			MemMB:        snap.MemMB,
			DiskMB:       snap.DiskMB,
			RestoredFrom: snapID,
		},
		State:     types.VMStateCreating,
		LogPath:   filepath.Join(m.instancesDir, id+".log"),
		CreatedAt: time.Now(),
	}
	if err := m.reserveName(req.Name, id); err != nil {
		return nil, err
	}
	if err := m.store.SaveVM(record); err != nil {
		m.releaseName(req.Name, id)
		return nil, fmt.Errorf("recording vm %s before forking it: %w", id, err)
	}
	fail := func(err error) (*types.VM, error) {
		m.undoCreate(record)
		return nil, err
	}

	rootfs, err := storage.CloneFromSnapshot(snap.Dir, id, m.instancesDir, m.jailerCfg.UID, m.jailerCfg.GID)
	if err != nil {
		return fail(err)
	}
	record.Config.Rootfs = rootfs
	if err := faults.Check("vm.fork.after-clone"); err != nil {
		return fail(err)
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
				return fail(fmt.Errorf("%w: this host's firecracker predates network_overrides (needs >= 1.12), so the fork must reuse the snapshot's TAP %q, which is in use — destroy/stop the VM holding it, or upgrade firecracker (scripts/install-fc.sh) for simultaneous forks", ErrConflict, snapTap))
			}
			tap = snapTap
		}
		if quarantine {
			if err := network.CreateTapQuarantined(tap); err != nil {
				return fail(fmt.Errorf("creating quarantined tap: %w", err))
			}
			// GuestIP/gateway are what the restored guest BELIEVES it has —
			// kept for visibility. NetworkName stays empty: no reservation.
			record.Config.TapDevice = tap
			record.Config.GuestIP = snap.GuestIP
			record.Config.GatewayIP = snap.GatewayIP
			record.Config.PrefixLen = snap.PrefixLen
			record.Config.Quarantine = true
		} else {
			gateway, bridge, prefix, err := m.netmgr.ClaimVM(snap.NetworkName, id, snap.GuestIP)
			if err != nil {
				return fail(fmt.Errorf("%w: fork needs the snapshot's address on network %q (%s): %v — destroy the VM holding it, or fork with quarantine=true", ErrConflict, snap.NetworkName, snap.GuestIP, err))
			}
			record.Config.NetworkName = snap.NetworkName
			record.Config.Bridge = bridge
			record.Config.GuestIP = snap.GuestIP
			record.Config.GatewayIP = gateway
			record.Config.PrefixLen = prefix
			if err := network.CreateTapEnslaved(tap, bridge, !m.netmgr.Intra(snap.NetworkName)); err != nil {
				return fail(fmt.Errorf("creating tap device: %w", err))
			}
			record.Config.TapDevice = tap
		}
	}

	if err := m.bootFromSnapshot(record, snap); err != nil {
		return fail(err)
	}
	if err := faults.Check("vm.fork.after-boot"); err != nil {
		return fail(err)
	}

	m.mu.Lock()
	m.vms[id] = record
	m.mu.Unlock()

	// Persist last, same contract as Create: until this write, a restart
	// treats the fork as never having existed.
	if err := m.store.SaveVM(record); err != nil {
		return fail(fmt.Errorf("persisting vm record %s: %w", id, err))
	}
	m.emit(types.EventVMCreated, record, "", map[string]string{"snapshot": snapID})

	return m.snapshotVM(record), nil
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
func (m *Manager) ForkVM(ctx context.Context, vmID string, req types.ForkVMRequest) (*types.VM, error) {
	m.mu.Lock()
	record, ok := m.vms[vmID]
	m.mu.Unlock()
	if ok && len(record.Config.Volumes) > 0 {
		return nil, errVolumesAttached(vmID)
	}

	snap, err := m.Snapshot(ctx, vmID, "fork-ephemeral")
	if err != nil {
		return nil, err
	}

	record, forkErr := m.Fork(ctx, snap.ID, req)

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
	if err := m.begin(vmID, "restore"); err != nil {
		return nil, err
	}
	defer m.end(vmID)
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
	if len(record.Config.Volumes) > 0 {
		return nil, errVolumesAttached(vmID)
	}
	// A running VM gives its memory back before the restored one takes it; a
	// stopped one is a new launch and is admitted like one.
	if record.State == types.VMStateStopped {
		release, err := m.admit(ctx, vmID, record.Config.MemMB)
		if err != nil {
			return nil, err
		}
		defer release()
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
	m.mu.Lock()
	record.Config.Rootfs = rootfs
	m.mu.Unlock()

	if record.Config.TapDevice != "" {
		if err := m.recreateTap(record.Config); err != nil {
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
	m.mu.Lock()
	record.Config.RestoredFrom = snapID
	m.mu.Unlock()

	if err := m.store.SaveVM(record); err != nil {
		_ = m.destroy(context.Background(), vmID)
		return nil, fmt.Errorf("persisting restored vm %s: %w", vmID, err)
	}

	m.emit(types.EventVMRestored, record, "", map[string]string{"snapshot": snapID})
	return m.snapshotVM(record), nil
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
	if err := m.applyLimits(id, pid, record.Config); err != nil {
		_ = firecracker.Kill(context.Background(), machine)
		_ = logFile.Close()
		return err
	}
	m.mu.Lock()
	record.PID = pid
	record.SocketPath = machine.Cfg.SocketPath
	record.VsockPath = filepath.Join(jailer.WorkspaceRoot(m.jailerCfg, id), firecracker.VsockDevicePath)
	record.State = types.VMStateRunning
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

// errVolumesAttached is the ErrConflict returned when a snapshot/fork/restore is
// attempted on a VM with volumes attached. In v1 that combination isn't
// supported: the snapshotted RAM holds the volume mounted (page cache, journal),
// and restoring over a volume that changed since would corrupt it.
func errVolumesAttached(vmID string) error {
	return fmt.Errorf("%w: vm %s has volumes attached — snapshot/fork/restore of a VM with volumes is not supported; destroy it (volumes persist) and recreate without them first", ErrConflict, vmID)
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

	// Destroys run in parallel: each one can spend seconds blocked on process
	// shutdown, and those waits are independent, so serially a bulk delete
	// costs their sum where in parallel it costs roughly the slowest one.
	// Per-VM state is disjoint (each has its own tap/clone/jail dir/store
	// row) and the shared structures (manager maps, IPAM, store) take their
	// own locks. Concurrency is capped so a large fleet doesn't stampede
	// netlink and the filesystem with hundreds of simultaneous teardowns.
	const maxConcurrent = 8
	sem := make(chan struct{}, maxConcurrent)
	var (
		wg    sync.WaitGroup
		resMu sync.Mutex
	)
	failed = make(map[string]error)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			err := m.Destroy(ctx, id)

			resMu.Lock()
			defer resMu.Unlock()
			if err != nil {
				failed[id] = err
				return
			}
			deleted = append(deleted, id)
		}()
	}
	wg.Wait()
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

// stagingDir is where offline debugfs I/O stages its temp files: on the store,
// never /tmp (which is often tmpfs = RAM — a 4 GB inject there would blow up
// memory, defeating the whole streaming design). The store is a real on-disk
// filesystem, so a staged file costs disk, not RAM.
func (m *Manager) stagingDir() string {
	return filepath.Join(m.instancesDir, "staging")
}

// offlineIO builds the context for offline debugfs operations: staging on the
// store, and debugfs dropped to the jailer uid/gid — the untrusted-ext4 parser
// runs with the same unprivileged identity as the VMs themselves, never as the
// daemon's root (see internal/storage/offline.go).
func (m *Manager) offlineIO() storage.OfflineIO {
	return storage.OfflineIO{
		StagingDir: m.stagingDir(),
		UID:        m.jailerCfg.UID,
		GID:        m.jailerCfg.GID,
	}
}

// PutFile writes data into a VM at guestPath, in constant memory whatever the
// size. For a running VM it streams over vsock (works even with no network); for
// a stopped VM it streams into the VM's disk offline with debugfs — never
// mounting the guest filesystem on the host. Any other state is rejected.
//
// size is the byte length when known (from Content-Length), or negative when
// unknown. The vsock protocol needs the length up front, so an unknown-length
// upload to a running VM is first staged to a file on the store to measure it,
// then streamed — still constant memory, just via disk. The debugfs path stages
// regardless (debugfs can't read a stream), so size is irrelevant there.
func (m *Manager) PutFile(id, guestPath string, data io.Reader, size int64) error {
	// Same rules for both channels (see storage.ValidateGuestPath): the vsock
	// agent would take a newline as the end of its header line.
	guestPath, err := storage.ValidateGuestPath(guestPath)
	if err != nil {
		return err
	}
	m.mu.Lock()
	record, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	state := record.State
	vsockPath := record.VsockPath
	rootfs := record.Config.Rootfs
	// Reserve the disk against a concurrent Start (which would boot Firecracker
	// on this rootfs while debugfs is writing it — corruption). Only the offline
	// path needs it; the running path talks to the live guest over vsock.
	if state == types.VMStateStopped {
		if m.vmIO[id] {
			m.mu.Unlock()
			return fmt.Errorf("%w: vm %s is busy with another file operation", ErrConflict, id)
		}
		m.vmIO[id] = true
	}
	m.mu.Unlock()

	switch state {
	case types.VMStateRunning:
		if size >= 0 {
			return vsock.PutFile(vsockPath, guestPath, data, size)
		}
		staged, n, err := m.stageReader(data)
		if err != nil {
			return err
		}
		defer os.Remove(staged)
		f, err := os.Open(staged)
		if err != nil {
			return fmt.Errorf("reopening staged upload: %w", err)
		}
		defer f.Close()
		return vsock.PutFile(vsockPath, guestPath, f, n)
	case types.VMStateStopped:
		defer m.endVMDiskIO(id)
		return m.offlineIO().InjectFile(rootfs, guestPath, data)
	default:
		return fmt.Errorf("%w: vm %s is %s (files need it running or stopped)", ErrVMState, id, state)
	}
}

func (m *Manager) endVMDiskIO(id string) {
	m.mu.Lock()
	delete(m.vmIO, id)
	m.mu.Unlock()
}

// stageReader streams data to a temp file on the store and returns its path and
// size — constant memory, on disk not RAM. Used when the vsock path needs a
// length it wasn't given up front.
func (m *Manager) stageReader(data io.Reader) (string, int64, error) {
	dir := m.stagingDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("creating staging dir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		return "", 0, fmt.Errorf("staging upload: %w", err)
	}
	n, err := io.Copy(f, data)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", 0, fmt.Errorf("staging upload: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", 0, err
	}
	return f.Name(), n, nil
}

// GetFileStream reads guestPath out of a VM as a streaming reader plus its size,
// in constant memory. Running → over vsock; stopped → off the disk offline with
// debugfs (no host mount). This is the post-mortem artifact path: stop a
// detonation VM and pull files straight off its disk. The caller must Close the
// returned reader.
func (m *Manager) GetFileStream(id, guestPath string) (io.ReadCloser, int64, error) {
	guestPath, err := storage.ValidateGuestPath(guestPath)
	if err != nil {
		return nil, 0, err
	}
	m.mu.Lock()
	record, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return nil, 0, fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	state := record.State
	vsockPath := record.VsockPath
	rootfs := record.Config.Rootfs
	if state == types.VMStateStopped {
		if m.vmIO[id] {
			m.mu.Unlock()
			return nil, 0, fmt.Errorf("%w: vm %s is busy with another file operation", ErrConflict, id)
		}
		m.vmIO[id] = true
	}
	m.mu.Unlock()

	switch state {
	case types.VMStateRunning:
		return vsock.GetFileStream(vsockPath, guestPath)
	case types.VMStateStopped:
		// The debugfs read finishes inside ExtractFileStream (into an unlinked
		// temp the reader wraps), so releasing the reservation here is safe even
		// though the caller reads the stream afterwards.
		defer m.endVMDiskIO(id)
		f, size, err := m.offlineIO().ExtractFileStream(rootfs, guestPath)
		if err != nil {
			return nil, 0, err
		}
		return f, size, nil
	default:
		return nil, 0, fmt.Errorf("%w: vm %s is %s (files need it running or stopped)", ErrVMState, id, state)
	}
}

// beginVolumeIO reserves a detached volume for an offline debugfs operation,
// returning its path. It fails if the volume is attached to a VM or already
// busy with another file operation — either would mean two writers on one ext4.
// Pair every success with endVolumeIO.
func (m *Manager) beginVolumeIO(volID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vol, ok := m.vols[volID]
	if !ok {
		return "", fmt.Errorf("%w: volume %s", ErrVMNotFound, volID)
	}
	if vol.AttachedTo != "" {
		return "", fmt.Errorf("%w: volume %s is attached to vm %s — write/read through that VM instead", ErrConflict, volID, vol.AttachedTo)
	}
	if m.volIO[volID] {
		return "", fmt.Errorf("%w: volume %s is busy with another file operation", ErrConflict, volID)
	}
	m.volIO[volID] = true
	return vol.Path, nil
}

func (m *Manager) endVolumeIO(volID string) {
	m.mu.Lock()
	delete(m.volIO, volID)
	m.mu.Unlock()
}

// InjectToVolume streams data into a detached volume at guestPath, offline with
// debugfs (no host mount), in constant memory. Refuses while the volume is
// attached to a VM or busy with another file op — a write underneath a running
// guest's mounted filesystem (or a concurrent debugfs write) corrupts it; use
// the VM's live channel (PutFile) for an attached volume. This is the path for
// filling a large volume (e.g. a multi-GB dataset) before attaching it at VM
// create time — Firecracker has no disk hot-plug, so prepare-then-attach is the
// clean model, not attaching to an already-running VM.
func (m *Manager) InjectToVolume(volID, guestPath string, data io.Reader) error {
	path, err := m.beginVolumeIO(volID)
	if err != nil {
		return err
	}
	defer m.endVolumeIO(volID)
	return m.offlineIO().InjectFile(path, guestPath, data)
}

// ExtractFromVolumeStream reads guestPath out of a detached volume as a
// streaming reader plus its size, offline with debugfs. Same guard as
// InjectToVolume. The debugfs read completes before this returns (into an
// unlinked temp the reader wraps), so the busy reservation is released here even
// though the caller consumes the stream afterwards. The caller must Close it.
func (m *Manager) ExtractFromVolumeStream(volID, guestPath string) (io.ReadCloser, int64, error) {
	path, err := m.beginVolumeIO(volID)
	if err != nil {
		return nil, 0, err
	}
	defer m.endVolumeIO(volID)
	f, size, err := m.offlineIO().ExtractFileStream(path, guestPath)
	if err != nil {
		return nil, 0, err
	}
	return f, size, nil
}

// Get returns a single VM by ID.
// The record is a copy: the monitor and in-flight operations write the live
// one under the lock, and a caller reading fields outside it would race them.
func (m *Manager) Get(id string) (*types.VM, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return nil, false
	}
	cp := *v
	return &cp, true
}

// List returns every VM currently tracked by the manager, as copies (see Get).
func (m *Manager) List() []*types.VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]*types.VM, 0, len(m.vms))
	for _, v := range m.vms {
		cp := *v
		list = append(list, &cp)
	}
	return list
}

// Templates exposes the catalog for the API layer, each entry marked with
// whether its goldens are on this host (see types.Template.Ready). Listing a
// template the store cannot boot as if it were usable is what sends an operator
// into `mh run` only to get a cp error from inside the clone.
func (m *Manager) Templates() []types.Template {
	list := m.catalog.List()
	for i := range list {
		if missing := missingGolden(list[i]); missing != "" {
			list[i].Missing = missing
			continue
		}
		list[i].Ready = true
	}
	return list
}

// missingGolden returns the path of the template's first golden file that is not
// on this host, or "" when both are present.
func missingGolden(tpl types.Template) string {
	for _, p := range []string{tpl.RootfsPath, tpl.KernelPath} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			return p
		}
	}
	return ""
}

// checkTemplateGoldens rejects a template whose golden files this host does not
// have, BEFORE any side effect. The catalog advertises every template the
// project knows, so "in the catalog" is not "bootable here": without this the
// first sign is a raw `cp`/open error from deep inside CloneRootfs, after the
// record, the admission and the name reservation were taken and rolled back —
// an error that names a path but not what to do about it.
func checkTemplateGoldens(tpl types.Template) error {
	missing := missingGolden(tpl)
	if missing == "" {
		return nil
	}
	return fmt.Errorf("%w: template %q is in the catalog but %s is not on this host: build its golden with 'make prepare-image' (see 'mh images' for what is ready)",
		ErrInvalid, tpl.Name, missing)
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
