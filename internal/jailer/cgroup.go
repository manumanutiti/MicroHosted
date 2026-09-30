package jailer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Per-VM resource limits, written into a cgroup v2 directory owned by this
// daemon. Without them a guest can DoS the host from inside its jail: spin
// every vCPU at 100% (Firecracker's vCPU threads are ordinary host threads
// with no quota), balloon the VMM's memory, or — cheapest of all — fork-bomb
// the host's PID space. Jailer itself only writes the cgroup values it's told
// on the command line, and firecracker-go-sdk v1.0.0 exposes no way to pass
// extra --cgroup flags (it only emits the NUMA cpuset pair), so the daemon
// writes the limits directly after launch instead.
//
// Layout: the limits live in the daemon's own tree, <mount>/microhosted/<id>
// — deliberately NOT under Jailer's parent cgroup (<mount>/firecracker).
// Jailer's cgroup behaviour depends on the flags the SDK gives it, and the
// SDK only emits its cpuset --cgroup pair when the host exposes NUMA sysfs
// (/sys/devices/system/node). On hosts without it (common on ARM64
// single-board kernels), Jailer gets no --cgroup flags and attaches the Firecracker
// process directly to <mount>/firecracker itself. If ApplyLimits enabled
// controllers in that cgroup's subtree_control — as it must for any child
// under it to have limit files — cgroup v2's no-internal-process rule makes
// every later attach fail with EBUSY: the first VM boots, every one after it
// dies before creating its API socket, until reboot. Keeping the limits tree
// out of Jailer's parent means Jailer's attach always succeeds and
// ApplyLimits just migrates the PID over afterwards (root can always
// migrate). The cost: on NUMA hosts the migration drops the cpuset pinning
// Jailer applied — a no-op on single-node machines, which is every host this
// targets.

// cgroupMountPoint is where the unified cgroup2 hierarchy is mounted. A var,
// not a const, so tests can point it at a temp directory and exercise the
// writes without root or a real cgroupfs.
var cgroupMountPoint = "/sys/fs/cgroup"

// mountPoint is the cgroup2 mount for d: Defaults.CgroupMount when set (tests
// of other packages point it at a fake tree), cgroupMountPoint otherwise.
func mountPoint(d Defaults) string {
	if d.CgroupMount != "" {
		return d.CgroupMount
	}
	return cgroupMountPoint
}

// ErrCgroupV1 is returned by ApplyLimits on hosts still on cgroup v1, where
// this per-VM limit scheme doesn't apply (jailer v1 layout is per-controller
// trees). The caller decides whether that's fatal; the manager logs a loud
// warning and boots anyway, since v1 hosts were already supported before
// limits existed.
var ErrCgroupV1 = errors.New("host uses cgroup v1: per-VM resource limits need cgroup v2")

const (
	// cpuPeriodUsec is the cpu.max accounting period. 100ms is the kernel
	// default; the quota scales with the VM's vCPU count so a VM can use at
	// most its allotted cores' worth of host CPU time.
	cpuPeriodUsec = 100_000

	// memoryOverheadMiB is added on top of the guest's MemMB for memory.max.
	// The cgroup charges the whole Firecracker process, not just guest RAM:
	// the VMM's own heap, virtio queues and page tables ride on top of the
	// guest mapping. Firecracker's documented VMM overhead is < 5 MiB; 64
	// leaves honest margin without weakening the bound meaningfully.
	memoryOverheadMiB = 64

	// pidsHeadroom is added to the vCPU count for pids.max. The pids
	// controller counts tasks (threads included): Firecracker runs one thread
	// per vCPU plus the VMM main and API threads. Firecracker never forks, so
	// a tight cap costs nothing and turns a VMM compromise's fork bomb into
	// an immediate EAGAIN.
	pidsHeadroom = 16
)

// limitsParent is the daemon-owned cgroup all per-VM limit groups live under.
// Its subtree_control is safe to populate precisely because nothing but
// ApplyLimits ever attaches processes here — see the package comment for why
// Jailer's own parent cgroup can't play that role.
const limitsParent = "microhosted"

// CgroupDir returns the host-side path of a VM's limits cgroup:
// <mount>/microhosted/<vmID>. This is the daemon's tree, not the one Jailer
// touches (see jailerCgroupDir).
func CgroupDir(d Defaults, vmID string) string {
	return filepath.Join(mountPoint(d), limitsParent, vmID)
}

// jailerCgroupDir returns the cgroup Jailer itself creates when it got at
// least one --cgroup flag: <mount>/<basename(ExecFile)>/<vmID> (its default
// --parent-cgroup is the exec-file basename, same convention as the chroot
// layout). The daemon never writes into it — ApplyLimits migrates the PID out
// — but it's per-VM residue Jailer never removes, so RemoveCgroup must.
func jailerCgroupDir(d Defaults, vmID string) string {
	return filepath.Join(mountPoint(d), filepath.Base(d.ExecFile), vmID)
}

// ApplyLimits caps a freshly launched VM's host resources by writing cpu,
// memory and pids limits into its Jailer cgroup and making sure pid is
// enrolled in it. Sized from the VM's own config: cpu.max = vcpus × one full
// period, memory.max = guest memory + fixed VMM overhead (swap disabled — a
// capped VM must hit its limit, not push the host into swap), pids.max =
// vcpus + a small fixed headroom.
//
// Call it as soon as the PID is known. There is a sub-second window where the
// process runs uncapped (jailer applies no limits of its own unless flagged),
// but nothing attacker-controlled executes before the guest kernel is up.
func ApplyLimits(d Defaults, vmID string, pid int, vcpus, memMB int64) error {
	if d.CgroupVersion != "2" {
		return ErrCgroupV1
	}
	if vcpus <= 0 || memMB <= 0 {
		return fmt.Errorf("cgroup limits for vm %s: invalid sizing vcpus=%d memMB=%d", vmID, vcpus, memMB)
	}

	dir := CgroupDir(d, vmID)
	// This tree is the daemon's own — Jailer never creates anything under it,
	// so the whole path has to be made here.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating cgroup %s: %w", dir, err)
	}

	// A child only gets a controller's limit files if every ancestor enables
	// it in cgroup.subtree_control — root first, then the parent. Re-enabling
	// an already-enabled controller is a no-op. Safe here (and only here)
	// because no process is ever attached to <mount>/microhosted itself; see
	// the package comment.
	for _, anc := range []string{mountPoint(d), filepath.Dir(dir)} {
		if err := enableControllers(anc); err != nil {
			return err
		}
	}

	limits := []struct{ file, value string }{
		{"cpu.max", fmt.Sprintf("%d %d", vcpus*cpuPeriodUsec, cpuPeriodUsec)},
		{"memory.max", strconv.FormatInt((memMB+memoryOverheadMiB)<<20, 10)},
		{"memory.swap.max", "0"},
		{"pids.max", strconv.FormatInt(vcpus+pidsHeadroom, 10)},
	}
	for _, l := range limits {
		if err := writeCgroupFile(filepath.Join(dir, l.file), l.value); err != nil {
			return err
		}
	}

	// Migrate the process in from wherever Jailer left it — its own cpuset
	// child on NUMA hosts, its parent cgroup (or nowhere) on hosts without
	// NUMA sysfs. Root can always migrate; this write is what makes the
	// limits above actually bind.
	if err := writeCgroupFile(filepath.Join(dir, "cgroup.procs"), strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("enrolling pid %d: %w", pid, err)
	}
	return nil
}

// RemoveCgroup deletes a VM's per-VM cgroup directories once its process is
// gone: the daemon's limits group (CgroupDir) and, on hosts where Jailer got
// --cgroup flags and built its own child, Jailer's leftover group too.
// Neither is ever cleaned up by anyone else, so without this every
// create/destroy cycle leaks empty cgroups. The kernel only allows rmdir on
// an empty group, and process exit is asynchronous with the SIGTERM that
// caused it, so EBUSY is retried briefly. Missing directories (v1 host, VM
// that never launched, no NUMA sysfs) are success.
func RemoveCgroup(d Defaults, vmID string) error {
	if d.CgroupVersion != "2" {
		return nil
	}
	var errs []error
	for _, dir := range []string{CgroupDir(d, vmID), jailerCgroupDir(d, vmID)} {
		var err error
		for i := 0; i < 40; i++ {
			err = os.Remove(dir)
			if err == nil || os.IsNotExist(err) {
				err = nil
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("removing cgroup %s: %w", dir, err))
		}
	}
	return errors.Join(errs...)
}

// ErrCgroupNotDrained is returned by KillCgroup when a VM's cgroup still has
// processes after the kill and its timeout. The VM must then keep counting as
// alive: whatever it holds (volumes, identity, name) stays held.
var ErrCgroupNotDrained = errors.New("vm cgroup still populated after kill")

// cgroupKillTimeout bounds how long KillCgroup waits for the kernel to empty
// a group after cgroup.kill. SIGKILL is not deferrable, so this only covers
// exit latency (unmapping a large guest). A var so tests can shorten it.
var cgroupKillTimeout = 5 * time.Second

// InCgroup reports whether pid is a member of vmID's limits cgroup, per the
// kernel's own cgroup.procs. This — not the process's command line, which the
// process can rewrite from its own memory — is how the daemon decides that a
// VM's Firecracker is alive: only root can move a process into or out of that
// group, so a compromised VMM can't leave it, and a stranger that inherited a
// recycled PID was never in it. False on a cgroup v1 host, where there is no
// such group; the caller has to fall back to something weaker there.
func InCgroup(d Defaults, vmID string, pid int) bool {
	if d.CgroupVersion != "2" || pid <= 0 {
		return false
	}
	data, err := os.ReadFile(filepath.Join(CgroupDir(d, vmID), "cgroup.procs"))
	if err != nil {
		return false
	}
	want := strconv.Itoa(pid)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// KillCgroup kills every process in vmID's per-VM cgroups — the daemon's
// limits group and, where Jailer built one, its own per-VM child (never
// Jailer's shared parent) — and waits until the kernel reports them empty.
// It is the teardown step that doesn't depend on knowing the right PID: a VMM
// that rewrote its argv, or a record whose PID is stale, still dies here.
// cgroup.kill needs Linux 5.14; on older kernels each listed process is
// SIGKILLed instead. A missing group (never launched, already removed) and a
// cgroup v1 host are success. Every error wraps ErrCgroupNotDrained: a group
// whose emptiness can't be confirmed — still populated after
// cgroupKillTimeout, or unreadable — counts as still running.
func KillCgroup(d Defaults, vmID string) error {
	if d.CgroupVersion != "2" {
		return nil
	}
	var errs []error
	for _, dir := range []string{CgroupDir(d, vmID), jailerCgroupDir(d, vmID)} {
		if err := killCgroupDir(dir); err != nil {
			errs = append(errs, fmt.Errorf("%w: %s: %w", ErrCgroupNotDrained, dir, err))
		}
	}
	return errors.Join(errs...)
}

func killCgroupDir(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	deadline := time.Now().Add(cgroupKillTimeout)
	for {
		populated, err := cgroupPopulated(dir)
		if err != nil {
			return err
		}
		if !populated {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still populated after %s", cgroupKillTimeout)
		}
		// Re-sent every round: cheap, idempotent, and it also catches a task
		// that raced into the group after the first write.
		if err := writeKill(dir); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("writing cgroup.kill: %w", err)
			}
			killListed(dir) // pre-5.14 kernel: no cgroup.kill
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeKill writes "1" to dir's cgroup.kill without O_CREAT: on a kernel
// without the file, cgroupfs would refuse the create with EACCES, hiding the
// ENOENT the fallback keys on.
func writeKill(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, "cgroup.kill"), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString("1")
	return errors.Join(err, f.Close())
}

// killListed SIGKILLs every process cgroup.procs lists — KillCgroup's fallback
// on kernels without cgroup.kill. Not atomic against a process that forks
// meanwhile, which the retry loop and the group's tight pids.max cover.
func killListed(dir string) {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// cgroupPopulated reads the "populated" key of dir's cgroup.events, which the
// kernel clears once no process is left in the group or below it. A group
// that vanished meanwhile is unpopulated; any other read failure is an error,
// so a caller never mistakes "couldn't tell" for "empty".
func cgroupPopulated(dir string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		if _, serr := os.Stat(dir); os.IsNotExist(serr) {
			return false, nil
		}
		return false, fmt.Errorf("reading cgroup.events: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if key, val, ok := strings.Cut(line, " "); ok && key == "populated" {
			return strings.TrimSpace(val) != "0", nil
		}
	}
	return false, errors.New("cgroup.events has no populated key")
}

// enableControllers turns on the cpu, memory and pids controllers for dir's
// children. One write per controller: the kernel rejects a compound write
// wholesale if any token is invalid, and per-controller writes name the
// culprit in the error.
func enableControllers(dir string) error {
	ctl := filepath.Join(dir, "cgroup.subtree_control")
	for _, c := range []string{"+cpu", "+memory", "+pids"} {
		if err := os.WriteFile(ctl, []byte(c), 0o644); err != nil {
			return fmt.Errorf("enabling controller %s in %s: %w", c, dir, err)
		}
	}
	return nil
}

// writeCgroupFile writes one value into one cgroup interface file, with the
// value in the error so a rejected limit is diagnosable from the log alone.
func writeCgroupFile(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("writing %q to %s: %w", value, path, err)
	}
	return nil
}

// OOMEvents reads a VM's memory.events counters: how many times it hit its
// memory.max (oom) and how many of its processes an OOM killer took
// (oom_kill — any OOM killer, the host's included). Zeros when the group is
// gone or the host has no cgroup v2.
func OOMEvents(d Defaults, vmID string) (oom, oomKill int) {
	data, err := os.ReadFile(filepath.Join(CgroupDir(d, vmID), "memory.events"))
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(strings.TrimSpace(val))
		switch key {
		case "oom":
			oom = n
		case "oom_kill":
			oomKill = n
		}
	}
	return oom, oomKill
}

// CgroupIDs lists the VM IDs that have a per-VM cgroup on this host, in the
// daemon's limits tree or in Jailer's — the residue startup and the doctor
// compare against the VMs actually running.
func CgroupIDs(d Defaults) []string {
	if d.CgroupVersion != "2" {
		return nil
	}
	seen := make(map[string]bool)
	var ids []string
	for _, parent := range []string{filepath.Dir(CgroupDir(d, "x")), filepath.Dir(jailerCgroupDir(d, "x"))} {
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() && IsVMID(e.Name()) && !seen[e.Name()] {
				seen[e.Name()] = true
				ids = append(ids, e.Name())
			}
		}
	}
	return ids
}

// CgroupOwner says whose per-VM cgroup a process sits in.
type CgroupOwner int

const (
	CgroupNone   CgroupOwner = iota
	CgroupLimits             // <mount>/microhosted/<id>: only ApplyLimits attaches here
	CgroupJailer             // Jailer's parent, shared with any other Jailer on the host
)

// ProcessCgroupOwner classifies the cgroup v2 path of a process — the "0::"
// line of /proc/<pid>/cgroup, e.g. "0::/microhosted/1a2b3c4d" — against the
// cgroups this daemon and Jailer create for vmID. It is how a running
// Firecracker is recognised as ours: once Jailer has pivot_root'ed into its own
// mount namespace, /proc/<pid>/root no longer reads as the chroot path from the
// host, and after the exec the command line no longer names the chroot base.
func ProcessCgroupOwner(d Defaults, procCgroup []byte, vmID string) CgroupOwner {
	if d.CgroupVersion != "2" {
		return CgroupNone
	}
	var path string
	for _, line := range strings.Split(string(procCgroup), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			path = rest
			break
		}
	}
	if path == "" {
		return CgroupNone
	}
	rel := func(dir string) string { return strings.TrimPrefix(dir, mountPoint(d)) }
	switch path {
	case rel(CgroupDir(d, vmID)):
		return CgroupLimits
	case rel(jailerCgroupDir(d, vmID)), rel(filepath.Dir(jailerCgroupDir(d, vmID))):
		// Without --cgroup flags (no NUMA sysfs, common on ARM64 boards) Jailer attaches
		// straight to its parent, with no per-VM child.
		return CgroupJailer
	}
	return CgroupNone
}

// InstanceIDs lists the VM IDs that have a jail directory under the chroot
// base.
func InstanceIDs(d Defaults) []string {
	entries, err := os.ReadDir(filepath.Dir(InstanceDir(d, "x")))
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && IsVMID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

// IsVMID reports whether s has the shape of a VM ID this daemon hands out
// (8 lowercase hex characters). Residue sweeps only ever touch names of that
// shape, so nothing else that happens to live beside them is at risk.
func IsVMID(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
