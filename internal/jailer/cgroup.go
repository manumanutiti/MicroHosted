package jailer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Per-VM resource limits, written into the cgroup v2 directory Jailer creates
// for each microVM. Without them a guest can DoS the host from inside its
// jail: spin every vCPU at 100% (Firecracker's vCPU threads are ordinary host
// threads with no quota), balloon the VMM's memory, or — cheapest of all —
// fork-bomb the host's PID space. Jailer itself only writes the cgroup values
// it's told on the command line, and firecracker-go-sdk v1.0.0 exposes no way
// to pass extra --cgroup flags (it only emits the NUMA cpuset pair), so the
// daemon writes the limits directly after launch instead.
//
// Layout: with --cgroup-version 2, Jailer creates
// <mount>/<parent_cgroup>/<id> where parent_cgroup defaults to the exec-file
// basename — the exact mirror of the chroot's InstanceDir formula. Jailer
// moves the Firecracker process in there before exec'ing it, but only when it
// got at least one --cgroup flag, so ApplyLimits both writes the limit files
// AND enrolls the PID itself — correct whether or not Jailer created the
// group first.

// cgroupMountPoint is where the unified cgroup2 hierarchy is mounted. A var,
// not a const, so tests can point it at a temp directory and exercise the
// writes without root or a real cgroupfs.
var cgroupMountPoint = "/sys/fs/cgroup"

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

// CgroupDir returns the host-side path of a VM's cgroup v2 directory:
// <mount>/<basename(ExecFile)>/<vmID> — Jailer's default --parent-cgroup is
// the exec-file basename, same convention as the chroot layout.
func CgroupDir(d Defaults, vmID string) string {
	return filepath.Join(cgroupMountPoint, filepath.Base(d.ExecFile), vmID)
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
	// MkdirAll, not a stat check: Jailer only creates the group when given a
	// --cgroup flag (the SDK's NUMA cpuset pair — present on normal hosts but
	// not guaranteed). Creating it here makes the limits hold either way.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating cgroup %s: %w", dir, err)
	}

	// A child only gets a controller's limit files if every ancestor enables
	// it in cgroup.subtree_control. Jailer enables only what it writes
	// (cpuset), so cpu/memory/pids must be switched on here — root first,
	// then the parent. Re-enabling an already-enabled controller is a no-op.
	for _, anc := range []string{cgroupMountPoint, filepath.Dir(dir)} {
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

	// Enroll the process. If Jailer already moved it here this re-attach is a
	// no-op; if it didn't (no --cgroup flags reached it), this is what makes
	// the limits above actually bind. Root can always migrate.
	if err := writeCgroupFile(filepath.Join(dir, "cgroup.procs"), strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("enrolling pid %d: %w", pid, err)
	}
	return nil
}

// RemoveCgroup deletes a VM's cgroup directory once its process is gone.
// Jailer never cleans these up, so without it every create/destroy cycle
// leaks an empty cgroup. The kernel only allows rmdir on an empty group, and
// process exit is asynchronous with the SIGTERM that caused it, so EBUSY is
// retried briefly. Missing directory (v1 host, VM that never launched) is
// success.
func RemoveCgroup(d Defaults, vmID string) error {
	if d.CgroupVersion != "2" {
		return nil
	}
	dir := CgroupDir(d, vmID)
	var err error
	for i := 0; i < 40; i++ {
		err = os.Remove(dir)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("removing cgroup %s: %w", dir, err)
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
