package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"microhosted/internal/firecracker"
	"microhosted/internal/hostinfo"
	"microhosted/internal/jailer"
	"microhosted/internal/network"
	"microhosted/internal/storage"
	"microhosted/pkg/types"
)

// The engine under failure (docs/roadmap.md, Phase 0b): what keeps the
// manager's view and the host's reality the same thing when an operation
// breaks halfway, when the daemon dies mid-operation, and when a VM dies on
// its own.

// ErrCapacity is a create/fork/start refused because the host cannot take one
// more VM right now. The API answers 503: nothing is wrong with the request,
// and it may succeed once something else stops.
var ErrCapacity = errors.New("insufficient host capacity")

// Limits bound what the manager admits. The zero value admits by memory only,
// with the default reserve.
type Limits struct {
	// MemReserveMB is the host memory kept free for everything that is not a
	// VM (the daemon, sshd, the kernel's own needs). A launch is refused when
	// MemAvailable minus what in-flight launches will take, minus the new
	// VM's mem_mb, would drop below it. Guest memory is allocated lazily, so
	// this is judged against what the host has now, not against the sum of
	// mem_mb promised — that sum would cap an 8 GB host at ~40 VMs of 128 MB
	// that each use ~34 MB.
	MemReserveMB int64
	// MaxVMs caps running VMs plus in-flight launches. 0 means no cap.
	MaxVMs int
	// MaxParallelBoots is how many launches (create, fork, start, restore
	// from stopped) run at once; the rest wait their turn, or give up when
	// their request is cancelled.
	MaxParallelBoots int
}

// Defaults for Limits fields left at zero.
const (
	DefaultMemReserveMB     = 512
	DefaultMaxParallelBoots = 4
)

// SetLimits installs the admission limits. Call once, before serving.
func (m *Manager) SetLimits(l Limits) {
	if l.MemReserveMB <= 0 {
		l.MemReserveMB = DefaultMemReserveMB
	}
	if l.MaxParallelBoots <= 0 {
		l.MaxParallelBoots = DefaultMaxParallelBoots
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limits = l
	m.bootSlots = make(chan struct{}, l.MaxParallelBoots)
}

// Limits returns the admission limits in force.
func (m *Manager) Limits() Limits {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limits
}

// begin marks a VM as having a lifecycle operation in progress, refusing a
// second one: two Starts on the same VM would launch two Firecrackers for one
// disk, a Stop racing a Snapshot would kill it mid-pause. The monitor and the
// doctor skip busy VMs — their state is in the middle of changing.
func (m *Manager) begin(id, op string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.busy[id]; ok {
		return fmt.Errorf("%w: vm %s is busy (%s in progress)", ErrConflict, id, cur)
	}
	m.busy[id] = op
	return nil
}

func (m *Manager) end(id string) {
	m.mu.Lock()
	delete(m.busy, id)
	m.mu.Unlock()
}

// admit waits for a boot slot and reserves room for a VM of memMB about to
// launch. The returned release must be called once the launch is over,
// whether it worked or not.
func (m *Manager) admit(ctx context.Context, id string, memMB int64) (release func(), err error) {
	m.mu.Lock()
	slots := m.bootSlots
	m.mu.Unlock()

	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: gave up waiting for a boot slot: %v", ErrCapacity, ctx.Err())
	}

	avail, memErr := m.memAvailable()

	m.mu.Lock()
	defer m.mu.Unlock()
	refuse := func(err error) (func(), error) {
		<-slots
		return nil, err
	}
	if m.limits.MaxVMs > 0 {
		live := len(m.inflight)
		for vid, v := range m.vms {
			if v.State == types.VMStateRunning && !m.inflight[vid] {
				live++
			}
		}
		if live >= m.limits.MaxVMs {
			return refuse(fmt.Errorf("%w: %d VMs running or launching, the cap is %d (--max-vms)", ErrCapacity, live, m.limits.MaxVMs))
		}
	}
	if memErr != nil {
		// Admission that cannot see the host's memory admits nothing: the
		// failure it prevents is the host OOM-killing a VM, or the daemon.
		return refuse(fmt.Errorf("%w: reading host memory: %v", ErrCapacity, memErr))
	}
	if left := avail - m.inflightMB - memMB; left < m.limits.MemReserveMB {
		return refuse(fmt.Errorf("%w: %d MB available, %d MB promised to launches in progress; a %d MB VM would leave %d MB, under the %d MB reserve (--mem-reserve-mb)",
			ErrCapacity, avail, m.inflightMB, memMB, left, m.limits.MemReserveMB))
	}
	m.inflightMB += memMB
	m.inflight[id] = true

	return func() {
		m.mu.Lock()
		m.inflightMB -= memMB
		delete(m.inflight, id)
		m.mu.Unlock()
		<-slots
	}, nil
}

// hostMemAvailable is the production memAvailable: MemAvailable from
// /proc/meminfo, the kernel's own estimate of what can be allocated without
// swapping.
func hostMemAvailable() (int64, error) {
	mem, err := hostinfo.ReadMemory()
	if err != nil {
		return 0, err
	}
	return mem.AvailableMB, nil
}

// undoCreate removes every trace of a VM whose create or fork did not
// complete: its process, TAP, IP lease, volume claims, jail dir and cgroup,
// disk clone and console log, and finally its record. Each step tolerates the
// resource never having been made, so the same function serves a rollback at
// any point of Create/Fork and, at startup, a create the daemon died in the
// middle of. If anything cannot be removed the record is left as it is —
// still "creating" — so the next daemon start tries again, instead of
// forgetting the residue it points to.
func (m *Manager) undoCreate(rec *types.VM) {
	id := rec.Config.ID

	m.mu.Lock()
	r := m.run[id]
	delete(m.vms, id)
	delete(m.run, id)
	m.mu.Unlock()

	var errs []error
	if r != nil && r.machine != nil {
		errs = append(errs, wrapErr("stopping vm %s", id, firecracker.Kill(context.Background(), r.machine)))
	}
	if r != nil && r.logFile != nil {
		_ = r.logFile.Close()
	}
	// The SDK handle may never have been kept (crash, or a launch that died
	// between spawning the process and returning), so look for the process
	// itself too.
	if pid, ok := m.findVMProcesses()[id]; ok {
		errs = append(errs, wrapErr("stopping vm %s", id, stopByPID(pid)))
	}
	if rec.Config.TapDevice != "" {
		errs = append(errs, network.DeleteTap(rec.Config.TapDevice))
	}
	if rec.Config.NetworkName != "" {
		m.netmgr.DetachVM(rec.Config.NetworkName, id)
	}
	m.releaseVolumes(id, rec.Config.Volumes)
	errs = append(errs,
		wrapErr("removing jail dir for vm %s", id, jailer.RemoveInstanceDir(m.jailerCfg, id)),
		wrapErr("removing rootfs clone for vm %s", id, storage.DeleteClone(m.instancesDir, id)),
		wrapErr("removing console log for vm %s", id, removeIfExists(rec.LogPath)),
	)
	if err := errors.Join(errs...); err != nil {
		log.Printf("vm %s: undoing a failed create left residue; its record stays \"creating\" so the next daemon start retries: %v", id, err)
		return
	}
	if err := m.store.DeleteVM(id); err != nil {
		log.Printf("vm %s: dropping the record of a failed create: %v", id, err)
	}
	m.releaseName(rec.Config.Name, id)
}

// Monitor watches running VMs until ctx ends and reaps the ones whose process
// died on its own: the OOM killer, a Firecracker crash, a guest that rebooted
// (Firecracker exits on a guest reboot). Without it such a VM stays "running"
// until the next daemon restart — every exec fails and nothing says why.
func (m *Manager) Monitor(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reapDead()
		}
	}
}

func (m *Manager) reapDead() {
	m.mu.Lock()
	var dead []string
	for id, v := range m.vms {
		if v.State == types.VMStateRunning && m.busy[id] == "" && !processAlive(v.PID, id) {
			dead = append(dead, id)
		}
	}
	m.mu.Unlock()
	for _, id := range dead {
		m.reap(id)
	}
}

// reap turns a VM whose process is gone into a stopped one — the same end
// state Stop leaves: disk, IP and volumes kept, TAP and jail dir released —
// and records why it died. It does not restart it (see VM.LastExit).
func (m *Manager) reap(id string) {
	if m.begin(id, "reap") != nil {
		return // someone is acting on it; the next tick looks again
	}
	defer m.end(id)

	m.mu.Lock()
	rec, ok := m.vms[id]
	r := m.run[id]
	m.mu.Unlock()
	if !ok || rec.State != types.VMStateRunning || processAlive(rec.PID, id) {
		return
	}

	// Read before the cgroup goes away with the jail dir.
	reason := "process exited"
	if oom, kills := jailer.OOMEvents(m.jailerCfg, id); kills > 0 {
		reason = "killed by the host's OOM killer (host out of memory)"
		if oom > 0 {
			reason = "killed by the OOM killer (reached its cgroup memory.max)"
		}
	}

	if r != nil && r.logFile != nil {
		_ = r.logFile.Close()
	}
	if rec.Config.TapDevice != "" {
		if err := network.DeleteTap(rec.Config.TapDevice); err != nil {
			log.Printf("monitor: vm %s: %v", id, err)
		}
	}
	if err := jailer.RemoveInstanceDir(m.jailerCfg, id); err != nil {
		log.Printf("monitor: vm %s: %v", id, err)
	}

	deadPID := rec.PID
	m.mu.Lock()
	rec.State = types.VMStateStopped
	rec.PID = 0
	rec.SocketPath = ""
	rec.VsockPath = ""
	rec.LastExit = &types.VMExit{At: time.Now(), Reason: reason}
	m.run[id] = &running{}
	m.mu.Unlock()
	if err := m.store.SaveVM(rec); err != nil {
		log.Printf("monitor: persisting vm %s as stopped: %v", id, err)
	}
	log.Printf("monitor: vm %s died (pid %d): %s; marked stopped", id, deadPID, reason)
	m.emit(types.EventVMDied, rec, reason, nil)
}

// SweepResidue removes the per-VM host resources that belong to no running
// VM: Firecracker processes nobody adopted (a create or start the previous
// run died in the middle of — KillMode=process keeps them alive), and the
// jail dirs and cgroups left behind. Call after Reconcile and before serving,
// when no operation can be in flight. It never touches a disk: clones and
// snapshots are data, and only the records they belong to may decide their
// fate (see undoCreate); the doctor reports the ones nothing references.
func (m *Manager) SweepResidue() {
	m.mu.Lock()
	running := make(map[string]int)
	for id, v := range m.vms {
		if v.State == types.VMStateRunning {
			running[id] = v.PID
		}
	}
	m.mu.Unlock()

	for id, pid := range m.findVMProcesses() {
		if running[id] == pid {
			continue
		}
		if err := stopByPID(pid); err != nil {
			log.Printf("sweep: killing orphan firecracker %d (vm %s): %v", pid, id, err)
			continue
		}
		log.Printf("sweep: killed orphan firecracker pid %d (vm %s is not running)", pid, id)
	}
	for _, id := range jailer.InstanceIDs(m.jailerCfg) {
		if _, ok := running[id]; ok {
			continue
		}
		if err := jailer.RemoveInstanceDir(m.jailerCfg, id); err != nil {
			log.Printf("sweep: removing jail dir of vm %s: %v", id, err)
			continue
		}
		log.Printf("sweep: removed jail dir of vm %s (not running)", id)
	}
	for _, id := range jailer.CgroupIDs(m.jailerCfg) {
		if _, ok := running[id]; ok {
			continue
		}
		if err := jailer.RemoveCgroup(m.jailerCfg, id); err != nil {
			log.Printf("sweep: removing cgroup of vm %s: %v", id, err)
			continue
		}
		log.Printf("sweep: removed cgroup of vm %s (not running)", id)
	}

	// A volume claimed by a VM that no longer exists (its create was undone,
	// or its record dropped) would stay unattachable forever.
	m.mu.Lock()
	var orphaned []*types.Volume
	for _, vol := range m.vols {
		if vol.AttachedTo != "" && m.vms[vol.AttachedTo] == nil {
			log.Printf("sweep: releasing volume %s (%s), claimed by vm %s which no longer exists", vol.ID, vol.Name, vol.AttachedTo)
			vol.AttachedTo = ""
			orphaned = append(orphaned, vol)
		}
	}
	m.mu.Unlock()
	for _, vol := range orphaned {
		if err := m.store.SaveVolume(vol); err != nil {
			log.Printf("sweep: persisting released volume %s: %v", vol.ID, err)
		}
	}
}

// findVMProcesses returns the Firecracker (or Jailer, before it execs)
// processes that belong to this daemon, by VM ID. A process is ours when its
// command line carries `--id <vm id>` and it sits in that VM's cgroup, runs
// inside this daemon's chroot base, or — still in Jailer — was told to use it
// (see ownsProcess). Anything else on the host named firecracker is left alone.
func (m *Manager) findVMProcesses() map[string]int {
	out := make(map[string]int)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	names := map[string]bool{
		filepath.Base(m.jailerCfg.ExecFile):     true,
		filepath.Base(m.jailerCfg.JailerBinary): true,
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(cmdline) == 0 {
			continue
		}
		id, ok := parseVMProcess(cmdline, names)
		if !ok {
			continue
		}
		if !m.ownsProcess(pid, id, cmdline) {
			continue
		}
		out[id] = pid
	}
	return out
}

// parseVMProcess reads a NUL-separated command line and returns the VM ID if
// it is one of names with an `--id` of the daemon's shape.
func parseVMProcess(cmdline []byte, names map[string]bool) (string, bool) {
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	if len(args) == 0 || !names[filepath.Base(args[0])] {
		return "", false
	}
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "--id" && jailer.IsVMID(args[i+1]) {
			return args[i+1], true
		}
	}
	return "", false
}

func (m *Manager) ownsProcess(pid int, id string, cmdline []byte) bool {
	// A booted Firecracker carries neither of the marks further down: Jailer
	// pivot_root's into its own mount namespace, so /proc/<pid>/root does not
	// read as the chroot from here, and the exec drops --chroot-base-dir. What
	// it does carry is its cgroup. The limits one is ours alone; Jailer's
	// parent is shared with any other Jailer on the host, so it only counts
	// when the jail dir for that ID is ours too (the short window between the
	// exec and ApplyLimits migrating the PID).
	if cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid)); err == nil {
		switch jailer.ProcessCgroupOwner(m.jailerCfg, cg, id) {
		case jailer.CgroupLimits:
			return true
		case jailer.CgroupJailer:
			if _, err := os.Stat(jailer.InstanceDir(m.jailerCfg, id)); err == nil {
				return true
			}
		}
	}
	if root, err := os.Readlink(fmt.Sprintf("/proc/%d/root", pid)); err == nil {
		if root == jailer.WorkspaceRoot(m.jailerCfg, id) {
			return true
		}
	}
	return m.jailerCfg.ChrootBaseDir != "" &&
		bytes.Contains(cmdline, []byte("--chroot-base-dir\x00"+m.jailerCfg.ChrootBaseDir+"\x00"))
}

// snapshotVM returns a copy of rec taken under the lock, so callers outside
// the manager never read a record while an operation or the monitor writes it.
func (m *Manager) snapshotVM(rec *types.VM) *types.VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *rec
	return &cp
}
