package vm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"microhosted/internal/jailer"
	"microhosted/pkg/types"
)

// Liveness is decided by the VM's cgroup, not by its command line: a
// compromised VMM can rewrite its own argv (it is just memory) and used to be
// reaped as dead — TAP deleted, volumes handed on — while it kept running.
// These tests run against a fake cgroupfs in a temp dir (jailer.Defaults.
// CgroupMount): cgroup.procs is written by the test, and a fake "kernel"
// goroutine honours cgroup.kill. What a directory can't reproduce — the real
// kernel's cgroup.kill and populated semantics — needs a real host.

const livenessID = "ab12cd34"

// cgroupManager is newStoreManager on a cgroup v2 host whose hierarchy is a
// temp dir.
func cgroupManager(t *testing.T) *Manager {
	t.Helper()
	m := newStoreManager(t)
	m.jailerCfg.CgroupVersion = "2"
	m.jailerCfg.CgroupMount = t.TempDir()
	return m
}

// argvlessProcess starts a process whose command line carries no VM ID at all
// — what a VMM looks like after overwriting its argv. done closes once it has
// exited (and been reaped).
func argvlessProcess(t *testing.T) (pid int, done <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ch := make(chan struct{})
	go func() { _ = cmd.Wait(); close(ch) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-ch })
	return cmd.Process.Pid, ch
}

// enroll puts pid in the VM's fake limits cgroup, as ApplyLimits would.
func enroll(t *testing.T, m *Manager, id string, pid int) string {
	t.Helper()
	dir := jailer.CgroupDir(m.jailerCfg, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "cgroup.procs"), strconv.Itoa(pid)+"\n")
	writeFile(t, filepath.Join(dir, "cgroup.events"), "populated 1\nfrozen 0\n")
	writeFile(t, filepath.Join(dir, "cgroup.kill"), "")
	return dir
}

// fakeKernel acts on cgroup.kill the way the kernel does: SIGKILL every
// listed process, and the group is empty.
func fakeKernel(t *testing.T, dir string) {
	t.Helper()
	stop := make(chan struct{})
	finished := make(chan struct{})
	t.Cleanup(func() { close(stop); <-finished })
	go func() {
		defer close(finished)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if b, err := os.ReadFile(filepath.Join(dir, "cgroup.kill")); err != nil || strings.TrimSpace(string(b)) != "1" {
				continue
			}
			procs, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
			for _, f := range strings.Fields(string(procs)) {
				if pid, err := strconv.Atoi(f); err == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
			// Removed outright, standing in for the empty group: on a real
			// cgroupfs RemoveCgroup's rmdir then succeeds despite the
			// interface files, which it can't on a plain directory.
			_ = os.RemoveAll(dir)
			return
		}
	}()
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// trackVM registers a VM with one attached volume, in memory and in the store.
func trackVM(t *testing.T, m *Manager, id string, state types.VMState, pid int) *types.VM {
	t.Helper()
	rec := &types.VM{
		Config: types.VMConfig{ID: id, Volumes: []types.VolumeMount{{VolumeID: "vol1"}}},
		State:  state,
		PID:    pid,
	}
	m.vms[id] = rec
	m.run[id] = &running{}
	m.vols["vol1"] = &types.Volume{ID: "vol1", Name: "data", Path: filepath.Join(t.TempDir(), "vol1.ext4"), AttachedTo: id}
	if err := m.store.SaveVM(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: process still alive", what)
	}
}

// A process in the VM's cgroup is the VM's process, whatever its argv says:
// the monitor must not reap it, and Destroy must leave nothing running.
func TestArgvRewriteDoesNotEscapeLiveness(t *testing.T) {
	m := cgroupManager(t)
	pid, done := argvlessProcess(t)
	dir := enroll(t, m, livenessID, pid)
	rec := trackVM(t, m, livenessID, types.VMStateRunning, pid)

	if !m.processAlive(pid, livenessID) {
		t.Fatal("process in the VM's cgroup reported dead because its argv lacks the VM id")
	}
	m.reapDead()
	if rec.State != types.VMStateRunning || rec.PID != pid {
		t.Fatalf("monitor reaped a live VM: state %s pid %d", rec.State, rec.PID)
	}
	if m.vols["vol1"].AttachedTo != livenessID {
		t.Fatal("monitor released the volume of a live VM")
	}

	fakeKernel(t, dir)
	if err := m.Destroy(context.Background(), livenessID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	waitDone(t, done, "after Destroy")
	if _, ok := m.Get(livenessID); ok {
		t.Error("record kept after Destroy")
	}
}

// The state the old argv check left behind: a record already "stopped" with
// PID 0 while its VMM still runs. Destroy has no PID to signal, so the cgroup
// is what ends it — and only then are the volumes released.
func TestDestroyKillsWhatIsLeftInTheCgroup(t *testing.T) {
	m := cgroupManager(t)
	pid, done := argvlessProcess(t)
	fakeKernel(t, enroll(t, m, livenessID, pid))
	trackVM(t, m, livenessID, types.VMStateStopped, 0)

	if err := m.Destroy(context.Background(), livenessID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	waitDone(t, done, "after Destroy of a PID-less record")
	if got := m.vols["vol1"].AttachedTo; got != "" {
		t.Errorf("volume still attached to %q after a clean Destroy", got)
	}
}

// A cgroup the kernel won't empty means the VM may still hold its volumes
// read-write: Destroy fails and releases nothing — record, volume claim and
// all stay as they were.
func TestDestroyKeepsVMWhoseCgroupWontDrain(t *testing.T) {
	m := cgroupManager(t)
	pid, _ := argvlessProcess(t)
	enroll(t, m, livenessID, pid) // no fake kernel: cgroup.kill is ignored
	trackVM(t, m, livenessID, types.VMStateStopped, 0)

	err := m.Destroy(context.Background(), livenessID)
	if !errors.Is(err, jailer.ErrCgroupNotDrained) {
		t.Fatalf("Destroy = %v, want ErrCgroupNotDrained", err)
	}
	if _, ok := m.Get(livenessID); !ok {
		t.Error("record forgotten although its cgroup is still populated")
	}
	if recs, _ := m.store.ListVMs(); len(recs) != 1 {
		t.Errorf("stored records = %d, want 1", len(recs))
	}
	if got := m.vols["vol1"].AttachedTo; got != livenessID {
		t.Errorf("volume AttachedTo = %q, want still %q", got, livenessID)
	}
}

// The recorded PID is gone but something else is still in the group (the
// record lost track of it): the monitor kills it before marking the VM
// stopped.
func TestReapKillsTheRestOfTheCgroup(t *testing.T) {
	m := cgroupManager(t)
	pid, done := argvlessProcess(t)
	fakeKernel(t, enroll(t, m, livenessID, pid))
	rec := trackVM(t, m, livenessID, types.VMStateRunning, 1<<22) // a PID not in the group

	m.reapDead()
	waitDone(t, done, "after reap")
	if rec.State != types.VMStateStopped || rec.PID != 0 {
		t.Errorf("state %s pid %d, want stopped with no pid", rec.State, rec.PID)
	}
}

// A local user's process that names itself firecracker and carries Jailer's
// flags is not ours: the command-line mark only counts for a process still
// running as root (Jailer before its exec).
func TestOwnsProcessRefusesUnprivilegedLookalike(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs a non-root process to impersonate Jailer")
	}
	m := cgroupManager(t)
	cmdline := []byte("/usr/bin/jailer\x00--id\x00" + livenessID + "\x00--chroot-base-dir\x00" + m.jailerCfg.ChrootBaseDir + "\x00")
	if m.ownsProcess(os.Getpid(), livenessID, cmdline) {
		t.Fatal("an unprivileged process was taken for this daemon's Jailer by its argv")
	}
}

// The reverse: a stranger that inherited a VM's recycled PID, with the VM id
// in its own argv, is not the VM — it was never in the cgroup.
func TestRecycledPIDIsNotAdopted(t *testing.T) {
	m := cgroupManager(t)
	cmd := exec.Command("sh", "-c", "sleep 30", livenessID)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })

	// The VM's group outlives its process (only the daemon removes it); here
	// it lists a PID that is not the stranger's.
	enroll(t, m, livenessID, 1<<22)
	if m.processAlive(cmd.Process.Pid, livenessID) {
		t.Fatal("a process outside the VM's cgroup was taken for the VM")
	}
}

// A VM launched before per-VM limits existed has no limits group at all; it
// is tracked by its command line, as before, instead of being reaped (and its
// volumes handed on) while it still runs.
func TestVMWithoutLimitsGroupKeepsLegacyLiveness(t *testing.T) {
	m := cgroupManager(t)
	cmd := exec.Command("sh", "-c", "sleep 30; :", "--id", livenessID)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })

	// exec has returned, but the new image's argv shows up a moment later.
	cmdline := "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/cmdline"
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if b, _ := os.ReadFile(cmdline); strings.Contains(string(b), livenessID) {
			break
		}
	}
	if !m.processAlive(cmd.Process.Pid, livenessID) {
		t.Fatal("a pre-limits VM with no limits group was taken for dead")
	}
	enroll(t, m, livenessID, 1<<22)
	if m.processAlive(cmd.Process.Pid, livenessID) {
		t.Fatal("once the VM has a limits group, its command line must not decide")
	}
}
