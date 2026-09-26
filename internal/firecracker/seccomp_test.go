package firecracker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"

	"microhosted/pkg/types"
)

// Regression: the SDK's zero value passes --no-seccomp. Both launch paths
// must ask for Firecracker's filters.
func TestConfigsEnableSeccomp(t *testing.T) {
	boot, err := BuildConfig(types.VMConfig{ID: "a1b2c3d4", VCPUs: 1, MemMB: 128, Rootfs: "/r", Kernel: "/k"}, types.IOLimits{}, fc.JailerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	restore := BuildRestoreConfig("a1b2c3d4", "/r", fc.JailerConfig{})
	for name, cfg := range map[string]fc.Config{"boot": boot, "restore": restore} {
		if !cfg.Seccomp.Enabled || cfg.Seccomp.Filter != "" {
			t.Errorf("%s: seccomp %+v, want Firecracker's built-in filters", name, cfg.Seccomp)
		}
	}
}

func fakeTask(t *testing.T, root, pid, tid, comm, mode string) {
	t.Helper()
	dir := filepath.Join(root, pid, "task", tid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\t"+comm+"\nNoNewPrivs:\t1\nSeccomp:\t"+mode+"\nSeccomp_filters:\t1\n"), 0o644)
}

func TestCheckSeccomp(t *testing.T) {
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })

	fakeTask(t, root, "100", "100", "firecracker", "2")
	fakeTask(t, root, "100", "101", "fc_api", "2")
	fakeTask(t, root, "100", "102", "fc_vcpu 0", "2")
	if err := CheckSeccomp(100); err != nil {
		t.Errorf("all filtered: %v", err)
	}

	fakeTask(t, root, "100", "103", "fc_vcpu 1", "0")
	if err := CheckSeccomp(100); err == nil || !strings.Contains(err.Error(), "fc_vcpu 1") {
		t.Errorf("one thread unfiltered: %v", err)
	}
	if err := CheckSeccomp(999); err == nil {
		t.Error("a missing process passed")
	}
	// The test process itself runs without seccomp.
	procRoot = "/proc"
	if err := CheckSeccomp(os.Getpid()); err == nil && !seccompOnSelf() {
		t.Error("an unfiltered live process passed")
	}
}

func seccompOnSelf() bool {
	b, _ := os.ReadFile("/proc/self/status")
	return strings.Contains(string(b), "Seccomp:\t2")
}
