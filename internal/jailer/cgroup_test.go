package jailer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests exercise the limit maths and file layout against a fake cgroup
// root (cgroupMountPoint pointed at a temp dir) — no root, no real cgroupfs.
// What a plain directory can't reproduce is the kernel's side of the
// contract (controller availability, the no-internal-process rule, pid
// migration), which is hardware-validation territory.

// withFakeCgroupRoot swaps cgroupMountPoint for a temp dir for one test.
func withFakeCgroupRoot(t *testing.T) (string, Defaults) {
	t.Helper()
	root := t.TempDir()
	old := cgroupMountPoint
	cgroupMountPoint = root
	t.Cleanup(func() { cgroupMountPoint = old })
	return root, Defaults{ExecFile: "/usr/local/bin/firecracker", CgroupVersion: "2"}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func TestApplyLimitsWritesSizedValues(t *testing.T) {
	root, d := withFakeCgroupRoot(t)

	if err := ApplyLimits(d, "vm1", 4242, 2, 512); err != nil {
		t.Fatalf("ApplyLimits: %v", err)
	}

	// The limits tree is the daemon's own (<mount>/microhosted/<id>), NOT
	// Jailer's parent cgroup: enabling controllers there would trip cgroup
	// v2's no-internal-process rule on hosts where Jailer attaches the
	// process to its parent directly (no NUMA sysfs → no --cgroup flags).
	dir := filepath.Join(root, "microhosted", "vm1")
	if got := CgroupDir(d, "vm1"); got != dir {
		t.Fatalf("CgroupDir = %s, want %s", got, dir)
	}

	for file, want := range map[string]string{
		"cpu.max":         "200000 100000", // 2 vCPUs × full period
		"memory.max":      "603979776",     // (512 + 64 overhead) MiB
		"memory.swap.max": "0",
		"pids.max":        "18", // 2 vCPUs + 16 headroom
		"cgroup.procs":    "4242",
	} {
		if got := readFile(t, filepath.Join(dir, file)); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}

	// Ancestors must have cpu/memory/pids enabled or the leaf files above
	// wouldn't exist on a real cgroupfs. The fake root records only the last
	// single-controller write; presence of the file is what's checkable here.
	for _, anc := range []string{root, filepath.Join(root, "microhosted")} {
		if _, err := os.Stat(filepath.Join(anc, "cgroup.subtree_control")); err != nil {
			t.Errorf("subtree_control not written in %s: %v", anc, err)
		}
	}

	// Jailer's own parent cgroup must stay untouched — writing its
	// subtree_control is exactly the poison this layout exists to avoid.
	if _, err := os.Stat(filepath.Join(root, "firecracker")); !os.IsNotExist(err) {
		t.Errorf("ApplyLimits touched jailer's parent cgroup: stat = %v", err)
	}
}

func TestApplyLimitsRejectsCgroupV1(t *testing.T) {
	_, d := withFakeCgroupRoot(t)
	d.CgroupVersion = "1"
	if err := ApplyLimits(d, "vm1", 1, 1, 128); !errors.Is(err, ErrCgroupV1) {
		t.Fatalf("ApplyLimits on v1 = %v, want ErrCgroupV1", err)
	}
}

func TestApplyLimitsRejectsInvalidSizing(t *testing.T) {
	_, d := withFakeCgroupRoot(t)
	if err := ApplyLimits(d, "vm1", 1, 0, 128); err == nil {
		t.Error("ApplyLimits with 0 vcpus succeeded, want error")
	}
	if err := ApplyLimits(d, "vm1", 1, 1, 0); err == nil {
		t.Error("ApplyLimits with 0 memMB succeeded, want error")
	}
}

func TestRemoveCgroup(t *testing.T) {
	root, d := withFakeCgroupRoot(t)
	// Both per-VM groups can exist: the daemon's limits group always, and
	// Jailer's own child on hosts where it got --cgroup flags.
	limitsDir := filepath.Join(root, "microhosted", "vm1")
	jailerDir := filepath.Join(root, "firecracker", "vm1")
	for _, dir := range []string{limitsDir, jailerDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveCgroup(d, "vm1"); err != nil {
		t.Fatalf("RemoveCgroup: %v", err)
	}
	for _, dir := range []string{limitsDir, jailerDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("cgroup dir %s still present after RemoveCgroup: %v", dir, err)
		}
	}
	// Absent dirs (never launched, a v1 host, or no NUMA sysfs so Jailer
	// never built its child) are success, not an error.
	if err := RemoveCgroup(d, "vm1"); err != nil {
		t.Fatalf("RemoveCgroup on missing dirs: %v", err)
	}
}

func TestInCgroup(t *testing.T) {
	root, d := withFakeCgroupRoot(t)
	dir := filepath.Join(root, "microhosted", "82970554")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("41\n4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for pid, want := range map[int]bool{4242: true, 41: true, 424: false, 0: false} {
		if got := InCgroup(d, "82970554", pid); got != want {
			t.Errorf("InCgroup(%d) = %v, want %v", pid, got, want)
		}
	}
	if InCgroup(d, "06364720", 4242) {
		t.Error("pid counted as member of another VM's cgroup")
	}
	d.CgroupVersion = "1"
	if InCgroup(d, "82970554", 4242) {
		t.Error("cgroup v1 host: InCgroup = true")
	}
}

// A fake group: cgroup.events says what the test wants, cgroup.kill exists
// empty (a 5.14+ kernel), and nothing acts on it unless the test does.
func fakeGroup(t *testing.T, dir, events string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, data := range map[string]string{"cgroup.events": events, "cgroup.kill": ""} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKillCgroup(t *testing.T) {
	root, d := withFakeCgroupRoot(t)
	old := cgroupKillTimeout
	cgroupKillTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cgroupKillTimeout = old })

	// Nothing there (never launched, already removed), or a v1 host: success.
	if err := KillCgroup(d, "82970554"); err != nil {
		t.Fatalf("KillCgroup on missing groups: %v", err)
	}

	// Already empty: no kill needed.
	limits := filepath.Join(root, "microhosted", "82970554")
	fakeGroup(t, limits, "populated 0\nfrozen 0\n")
	if err := KillCgroup(d, "82970554"); err != nil {
		t.Fatalf("KillCgroup on an empty group: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(limits, "cgroup.kill")); len(b) != 0 {
		t.Error("cgroup.kill written for an empty group")
	}

	// Populated, and the kernel empties it once told to.
	fakeGroup(t, limits, "populated 1\nfrozen 0\n")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if b, _ := os.ReadFile(filepath.Join(limits, "cgroup.kill")); string(b) == "1" {
				_ = os.WriteFile(filepath.Join(limits, "cgroup.events"), []byte("populated 0\nfrozen 0\n"), 0o644)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if err := KillCgroup(d, "82970554"); err != nil {
		t.Fatalf("KillCgroup: %v", err)
	}
	<-done

	// Populated and staying so — or unreadable — counts as still running.
	fakeGroup(t, limits, "populated 1\nfrozen 0\n")
	if err := KillCgroup(d, "82970554"); !errors.Is(err, ErrCgroupNotDrained) {
		t.Fatalf("KillCgroup on a group that won't drain = %v, want ErrCgroupNotDrained", err)
	}
	if b, _ := os.ReadFile(filepath.Join(limits, "cgroup.kill")); string(b) != "1" {
		t.Errorf("cgroup.kill = %q, want \"1\"", b)
	}
	fakeGroup(t, limits, "frozen 0\n")
	if err := KillCgroup(d, "82970554"); !errors.Is(err, ErrCgroupNotDrained) {
		t.Fatalf("KillCgroup with no populated key = %v, want ErrCgroupNotDrained", err)
	}

	// Jailer's per-VM child is killed too; its shared parent never is.
	_ = os.RemoveAll(limits)
	jailerDir := filepath.Join(root, "firecracker", "82970554")
	fakeGroup(t, jailerDir, "populated 1\n")
	fakeGroup(t, filepath.Join(root, "firecracker"), "populated 1\n")
	if err := KillCgroup(d, "82970554"); !errors.Is(err, ErrCgroupNotDrained) {
		t.Fatalf("KillCgroup with a populated jailer child = %v, want ErrCgroupNotDrained", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "firecracker", "cgroup.kill")); len(b) != 0 {
		t.Error("cgroup.kill written in Jailer's shared parent")
	}
	if b, _ := os.ReadFile(filepath.Join(jailerDir, "cgroup.kill")); string(b) != "1" {
		t.Errorf("Jailer's per-VM child: cgroup.kill = %q, want \"1\"", b)
	}

	d.CgroupVersion = "1"
	if err := KillCgroup(d, "82970554"); err != nil {
		t.Fatalf("KillCgroup on a v1 host: %v", err)
	}
}

func TestProcessCgroupOwner(t *testing.T) {
	_, d := withFakeCgroupRoot(t)
	for cg, want := range map[string]CgroupOwner{
		"0::/microhosted/82970554\n":      CgroupLimits,
		"0::/firecracker/82970554\n":      CgroupJailer,
		"0::/firecracker\n":               CgroupJailer,
		"0::/microhosted/06364720\n":      CgroupNone, // another VM's
		"0::/system.slice/foo.service\n":  CgroupNone,
		"12:pids:/microhosted/82970554\n": CgroupNone, // v1 line
		"":                                CgroupNone,
	} {
		if got := ProcessCgroupOwner(d, []byte(cg), "82970554"); got != want {
			t.Errorf("ProcessCgroupOwner(%q) = %v, want %v", cg, got, want)
		}
	}
	d.CgroupVersion = "1"
	if got := ProcessCgroupOwner(d, []byte("0::/microhosted/82970554\n"), "82970554"); got != CgroupNone {
		t.Errorf("cgroup v1 host: got %v, want CgroupNone", got)
	}
}
