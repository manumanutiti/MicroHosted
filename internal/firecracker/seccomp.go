package firecracker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
)

// seccomp turns on Firecracker's built-in per-thread seccomp filters, the
// ones its maintainers ship for production. It must be set explicitly: the
// SDK's zero value (Enabled: false) makes it pass --no-seccomp, so every VM
// ran with no syscall filter until this was noticed on a live process.
var seccomp = fc.SeccompConfig{Enabled: true}

// procRoot is /proc; a variable so tests can point it at a fake tree.
var procRoot = "/proc"

// CheckSeccomp verifies that every thread of the Firecracker process pid runs
// under a seccomp filter (mode 2). Firecracker installs one per thread before
// that thread runs guest-facing code, so once a boot or restore has returned,
// a thread without one means the filters are off — the caller must not let
// the VM run.
func CheckSeccomp(pid int) error {
	tasks, err := os.ReadDir(filepath.Join(procRoot, fmt.Sprint(pid), "task"))
	if err != nil {
		return fmt.Errorf("checking seccomp of pid %d: %w", pid, err)
	}
	if len(tasks) == 0 {
		return fmt.Errorf("checking seccomp of pid %d: no threads", pid)
	}
	for _, t := range tasks {
		dir := filepath.Join(procRoot, fmt.Sprint(pid), "task", t.Name())
		status, err := os.ReadFile(filepath.Join(dir, "status"))
		if err != nil {
			return fmt.Errorf("checking seccomp of pid %d thread %s: %w", pid, t.Name(), err)
		}
		mode := ""
		for _, line := range strings.Split(string(status), "\n") {
			if v, ok := strings.CutPrefix(line, "Seccomp:"); ok {
				mode = strings.TrimSpace(v)
				break
			}
		}
		if mode != "2" {
			comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
			return fmt.Errorf("firecracker pid %d thread %s (%s) runs without a seccomp filter (mode %q)", pid, t.Name(), strings.TrimSpace(string(comm)), mode)
		}
	}
	return nil
}
