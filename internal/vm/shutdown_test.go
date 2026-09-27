package vm

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"microhosted/pkg/types"
)

// adoptedVM starts a real process standing in for an adopted VM's Firecracker:
// its cmdline carries the VM id, which is what processAlive checks. ignoreTerm
// makes it survive SIGTERM, like a wedged VMM.
func adoptedVM(t *testing.T, id string, ignoreTerm bool) (*types.VM, *exec.Cmd) {
	t.Helper()
	script := "sleep 30"
	if ignoreTerm {
		script = "trap '' TERM; sleep 30"
	}
	cmd := exec.Command("sh", "-c", script, id)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }() // reap, so the pid does not linger as a zombie
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	// Until the child has exec'd, its cmdline is the test binary's.
	for deadline := time.Now().Add(2 * time.Second); !processAlive(cmd.Process.Pid, id); {
		if time.Now().After(deadline) {
			t.Fatal("stand-in process never came up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec := &types.VM{PID: cmd.Process.Pid, VsockPath: "/nonexistent/v.sock"}
	rec.Config.ID = id
	return rec, cmd
}

func withAgent(t *testing.T, fn func(cmd string) (int, error)) *[]string {
	t.Helper()
	var calls []string
	old := agentExec
	agentExec = func(_ context.Context, _ string, _ int, cmd string, _ time.Duration) (string, int, error) {
		calls = append(calls, cmd)
		code, err := fn(cmd)
		return "", code, err
	}
	t.Cleanup(func() { agentExec = old })
	return &calls
}

func exited(pid int, id string) bool { return !processAlive(pid, id) }

// The agent takes the request and the guest goes down: halt returns once the
// process is gone, without signalling it.
func TestHaltGracefulThroughAgent(t *testing.T) {
	rec, cmd := adoptedVM(t, "haltagent1", true)
	calls := withAgent(t, func(string) (int, error) {
		_ = cmd.Process.Signal(syscall.SIGKILL) // the guest rebooting: Firecracker exits
		return 0, nil
	})
	m := &Manager{}
	start := time.Now()
	if err := m.halt(context.Background(), rec, nil, true); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0] != guestPowerOff {
		t.Fatalf("agent calls = %q, want one %q", *calls, guestPowerOff)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("halt took %s after the guest was already gone", d)
	}
	if !exited(rec.PID, rec.Config.ID) {
		t.Fatal("process still alive")
	}
}

// No agent: fall back to signalling the process at once, no window waited.
func TestHaltNoAgentFallsBack(t *testing.T) {
	rec, _ := adoptedVM(t, "haltagent2", false)
	withAgent(t, func(string) (int, error) { return 0, errors.New("connection refused") })
	m := &Manager{}
	start := time.Now()
	if err := m.halt(context.Background(), rec, nil, true); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("fallback took %s", d)
	}
	if !exited(rec.PID, rec.Config.ID) {
		t.Fatal("process still alive")
	}
}

// The guest (untrusted) accepts the request and never goes down: the stop is
// bounded by the window, then the process is terminated anyway.
func TestHaltGuestIgnoresRequestIsBounded(t *testing.T) {
	old := gracefulWindow
	gracefulWindow = 300 * time.Millisecond
	t.Cleanup(func() { gracefulWindow = old })
	rec, _ := adoptedVM(t, "haltagent3", false)
	withAgent(t, func(string) (int, error) { return 0, nil })
	m := &Manager{}
	start := time.Now()
	if err := m.halt(context.Background(), rec, nil, true); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < gracefulWindow || d > gracefulWindow+2*time.Second {
		t.Fatalf("halt took %s, want about the %s window", d, gracefulWindow)
	}
	if !exited(rec.PID, rec.Config.ID) {
		t.Fatal("process still alive")
	}
}

// Not graceful (destroy, restore, replace --old destroy): the agent is never
// asked.
func TestHaltForcedSkipsAgent(t *testing.T) {
	rec, _ := adoptedVM(t, "haltagent4", false)
	calls := withAgent(t, func(string) (int, error) { return 0, nil })
	m := &Manager{}
	if err := m.halt(context.Background(), rec, nil, false); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("agent asked %q on a forced halt", *calls)
	}
	if !exited(rec.PID, rec.Config.ID) {
		t.Fatal("process still alive")
	}
}
