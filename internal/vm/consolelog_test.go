package vm

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"microhosted/pkg/types"
)

// TestConsoleLogIsCapped: however much a VM prints, the store holds at most
// two segments of consoleLogMax.
func TestConsoleLogIsCapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vm.log")
	c, err := openConsoleLog(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for i := 0; i < 5*consoleLogMax/len(chunk); i++ {
		if n, err := c.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v; want full success", n, err)
		}
	}
	// An oversized single write is trimmed to its tail, not stored whole.
	if _, err := c.Write(bytes.Repeat([]byte("y"), 3*consoleLogMax)); err != nil {
		t.Fatal(err)
	}
	c.Close()
	var total int64
	for _, p := range []string{path, consoleLogRotated(path)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > consoleLogMax {
			t.Fatalf("%s is %d bytes, cap is %d", p, fi.Size(), consoleLogMax)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v, want 0600", p, fi.Mode().Perm())
		}
		total += fi.Size()
	}
	if total > 2*consoleLogMax {
		t.Fatalf("console log uses %d bytes", total)
	}
	// Writes after Close are dropped, never an error.
	if n, err := c.Write([]byte("late")); err != nil || n != 4 {
		t.Fatalf("Write after Close = %d, %v", n, err)
	}
}

// TestConsoleLogTightensOldMode: a log an older daemon left 0644 becomes 0600
// on the next boot, and a stale rotated segment is removed.
func TestConsoleLogTightensOldMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vm.log")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(consoleLogRotated(path), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := openConsoleLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 || fi.Size() != 0 {
		t.Fatalf("mode %v size %d, want 0600 and empty", fi.Mode().Perm(), fi.Size())
	}
	if _, err := os.Stat(consoleLogRotated(path)); !os.IsNotExist(err) {
		t.Fatalf("stale rotated segment kept: %v", err)
	}
}

// TestConsoleLogRefusesSymlink: the log path must not be followed to another
// file.
func TestConsoleLogRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "vm.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if c, err := openConsoleLog(path); err == nil {
		c.Close()
		t.Fatal("openConsoleLog followed a symlink")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("symlink target modified: %q", b)
	}
}

// TestAttachConsole reproduces a daemon restart: a process keeps writing to a
// pipe whose original reader is gone; attachConsole must pick the output up
// again from /proc/<pid>/fd/1 into the same log.
func TestAttachConsole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vm.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "sleep 0.5; echo after")
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	// Keep our read end open until attached, so the child's write can't
	// EPIPE in between; then drop it like a dying daemon would.
	lf, err := attachConsole(cmd.Process.Pid, path)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	_ = cmd.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(path)
		if string(b) == "before\nafter\n" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log = %q", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAttachConsoleRefusesFile: a process whose stdout is a regular file (an
// older daemon's VM) has no pipe to attach to.
func TestAttachConsoleRefusesFile(t *testing.T) {
	dir := t.TempDir()
	out, err := os.Create(filepath.Join(dir, "direct.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.Command("sleep", "2")
	cmd.Stdout = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if lf, err := attachConsole(cmd.Process.Pid, filepath.Join(dir, "vm.log")); err == nil {
		lf.Close()
		t.Fatal("attached to a non-pipe stdout")
	}
}

// ConsoleTail reads the end of the log across a rotation, refuses a symlink
// planted in its place, and answers for an unknown VM with ErrVMNotFound.
func TestConsoleTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vm1.log")
	if err := os.WriteFile(consoleLogRotated(path), []byte("old-segment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &types.VM{LogPath: path, State: types.VMStateStopped}
	m := &Manager{vms: map[string]*types.VM{"vm1": rec}}

	for n, want := range map[int64]string{3: "ve\n", 5: "live\n", 9: "ent\nlive\n", 1000: "old-segment\nlive\n"} {
		got, err := m.ConsoleTail("vm1", n)
		if err != nil || string(got) != want {
			t.Errorf("tail %d = %q, %v; want %q", n, got, err, want)
		}
	}
	for _, n := range []int64{0, -1, ConsoleTailMax + 1} {
		if _, err := m.ConsoleTail("vm1", n); !errors.Is(err, ErrInvalid) {
			t.Errorf("tail %d: %v, want ErrInvalid", n, err)
		}
	}
	if _, err := m.ConsoleTail("nope", 10); !errors.Is(err, ErrVMNotFound) {
		t.Errorf("unknown VM: %v", err)
	}

	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, path); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ConsoleTail("vm1", 100); err == nil {
		t.Errorf("read through a symlink: %q", got)
	}
}
