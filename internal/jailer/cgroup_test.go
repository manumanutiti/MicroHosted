package jailer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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

	dir := filepath.Join(root, "firecracker", "vm1")
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
	for _, anc := range []string{root, filepath.Join(root, "firecracker")} {
		if _, err := os.Stat(filepath.Join(anc, "cgroup.subtree_control")); err != nil {
			t.Errorf("subtree_control not written in %s: %v", anc, err)
		}
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
	dir := filepath.Join(root, "firecracker", "vm1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCgroup(d, "vm1"); err != nil {
		t.Fatalf("RemoveCgroup: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cgroup dir still present after RemoveCgroup: %v", err)
	}
	// Absent dir (never launched, or a v1 host) is success, not an error.
	if err := RemoveCgroup(d, "vm1"); err != nil {
		t.Fatalf("RemoveCgroup on missing dir: %v", err)
	}
}
