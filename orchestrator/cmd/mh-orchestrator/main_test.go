package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// A lock names a pid for down to SIGTERM and apply to SIGHUP: it is only
// believed when it is this user's file, and the pid an mh-orchestrator.
func TestLockHolderTrustsOnlyOrchestrators(t *testing.T) {
	dir := t.TempDir()
	unlock, h, err := lock("p", dir, "microse.yml")
	if err != nil || h != nil {
		t.Fatalf("lock: %v, %v", err, h)
	}
	defer unlock()
	if h := lockHolder(dir); h != nil {
		t.Errorf("the test binary taken for an orchestrator: %+v", h)
	}

	// Held, but naming another process of this user's.
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Skip(err)
	}
	defer sleep.Process.Kill()
	if err := os.WriteFile(lockPath(dir), []byte(fmt.Sprintf("%d\n/x/microse.yml\n", sleep.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if h := lockHolder(dir); h != nil {
		t.Errorf("a sleep taken for an orchestrator: %+v", h)
	}
	if err := stopRunner("p", dir); err != nil {
		t.Fatal(err)
	}
	if sleep.ProcessState != nil || sleep.Process.Signal(syscall.Signal(0)) != nil {
		t.Error("stopRunner signalled a process that is not an orchestrator")
	}
}

func TestLockRefusesUnsafeFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, lockPath(dir)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lock("p", dir, "microse.yml"); err == nil {
		t.Error("a lock through a symlink was taken")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Error("the symlink's target was created")
	}

	dir = t.TempDir()
	if err := os.WriteFile(lockPath(dir), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockPath(dir), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lock("p", dir, "microse.yml"); err == nil {
		t.Error("a world-writable lock was taken")
	}
}
