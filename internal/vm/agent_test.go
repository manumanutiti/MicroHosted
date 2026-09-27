package vm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"microhosted/internal/events"
	"microhosted/internal/vsock"
	"microhosted/pkg/types"
)

// fakeAgent stands in for the guest agent: it refuses the probe until up is
// set, like a guest whose init has not started the listener yet.
type fakeAgent struct {
	up     atomic.Bool
	probes atomic.Int64
}

func withFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	f := &fakeAgent{}
	old := agentProbe
	agentProbe = func(context.Context, string, int, time.Duration) error {
		f.probes.Add(1)
		if f.up.Load() {
			return nil
		}
		return errors.New("reading CONNECT ack: EOF")
	}
	t.Cleanup(func() { agentProbe = old })
	return f
}

func shortAgentBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := agentReadyBudget
	agentReadyBudget = d
	t.Cleanup(func() { agentReadyBudget = old })
}

// runningVM registers a VM as running, as a boot leaves it.
func runningVM(m *Manager, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vms[id] = &types.VM{Config: types.VMConfig{ID: id}, State: types.VMStateRunning, VsockPath: "/nonexistent/v.sock"}
	m.run[id] = &running{}
}

func eventTypes(bus *events.Bus) []string {
	backlog, _, _, sub := bus.Subscribe("", 0, true)
	sub.Close()
	var got []string
	for _, e := range backlog {
		got = append(got, e.Type)
	}
	return got
}

// The agent comes up a moment after the boot: the watch notices, records when,
// and publishes vm.ready; a waiter is released by it.
func TestAgentReadyIsPublished(t *testing.T) {
	m := newTestManager(t)
	bus := events.NewBus(0)
	m.SetEvents(bus)
	f := withFakeAgent(t)
	runningVM(m, "a0000001")

	m.watchAgent("a0000001")
	time.AfterFunc(100*time.Millisecond, func() { f.up.Store(true) })
	vm, err := m.WaitReady(context.Background(), "a0000001", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if vm.AgentReadyAt == nil {
		t.Fatal("agent_ready_at not set on a ready VM")
	}
	if got := eventTypes(bus); len(got) != 1 || got[0] != types.EventVMReady {
		t.Fatalf("events %v, want one vm.ready", got)
	}
	if f.probes.Load() < 2 {
		t.Errorf("%d probes: the watch did not retry", f.probes.Load())
	}
}

// An exec issued right after a boot waits for the agent instead of failing
// its handshake — and gives up at its own deadline, as a timeout.
func TestAwaitAgentWaitsWithinTheDeadline(t *testing.T) {
	m := newTestManager(t)
	f := withFakeAgent(t)
	runningVM(m, "a0000001")
	m.watchAgent("a0000001")

	_, err := m.awaitAgent(context.Background(), "a0000001", time.Now().Add(100*time.Millisecond))
	if !errors.Is(err, vsock.ErrTimeout) {
		t.Fatalf("err = %v, want a timeout while the agent is down", err)
	}
	f.up.Store(true)
	ready, err := m.awaitAgent(context.Background(), "a0000001", time.Now().Add(5*time.Second))
	if err != nil || !ready {
		t.Fatalf("ready=%v err=%v, want ready once the agent answers", ready, err)
	}
}

// No answer within the budget: vm.agent_unready with a reason, and a waiter
// is told so (502), not left hanging. An agent that comes up late is found by
// the next wait, which publishes vm.ready then.
func TestAgentUnreadyThenLate(t *testing.T) {
	m := newTestManager(t)
	bus := events.NewBus(0)
	m.SetEvents(bus)
	shortAgentBudget(t, 150*time.Millisecond)
	f := withFakeAgent(t)
	runningVM(m, "a0000001")
	m.watchAgent("a0000001")

	if _, err := m.WaitReady(context.Background(), "a0000001", 5*time.Second); !errors.Is(err, ErrAgentUnready) {
		t.Fatalf("err = %v, want ErrAgentUnready", err)
	}
	backlog, _, _, sub := bus.Subscribe("", 0, true)
	sub.Close()
	if len(backlog) != 1 || backlog[0].Type != types.EventVMAgentUnready || backlog[0].Reason == "" {
		t.Fatalf("events %+v, want one vm.agent_unready with a reason", backlog)
	}

	f.up.Store(true)
	vm, err := m.WaitReady(context.Background(), "a0000001", 5*time.Second)
	if err != nil || vm.AgentReadyAt == nil {
		t.Fatalf("late agent: vm=%+v err=%v, want ready", vm, err)
	}
	if got := eventTypes(bus); len(got) != 2 || got[1] != types.EventVMReady {
		t.Fatalf("events %v, want vm.agent_unready then vm.ready", got)
	}
}

// A watch belongs to one incarnation: once the VM is stopped or booted again,
// the old watch stops and publishes nothing about the new one.
func TestAgentWatchEndsWithItsIncarnation(t *testing.T) {
	m := newTestManager(t)
	bus := events.NewBus(0)
	m.SetEvents(bus)
	f := withFakeAgent(t)
	runningVM(m, "a0000001")
	m.watchAgent("a0000001")
	w := m.agentWatchOf("a0000001")

	m.mu.Lock()
	m.run["a0000001"] = &running{} // what a stop, a restore or a new boot does
	m.mu.Unlock()
	f.up.Store(true)
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the old watch did not stop")
	}
	if w.ready {
		t.Error("the old incarnation's watch reported ready")
	}
	if got := eventTypes(bus); len(got) != 0 {
		t.Fatalf("events %v, want none from a superseded watch", got)
	}
	if vm, _ := m.Get("a0000001"); vm.AgentReadyAt != nil {
		t.Error("agent_ready_at set by a superseded watch")
	}
}

// Waiting on a VM that is not running is a state error (409), not a timeout.
func TestWaitReadyNotRunning(t *testing.T) {
	m := newTestManager(t)
	withFakeAgent(t)
	runningVM(m, "a0000001")
	m.mu.Lock()
	m.vms["a0000001"].State = types.VMStateStopped
	m.mu.Unlock()
	if _, err := m.WaitReady(context.Background(), "a0000001", time.Second); !errors.Is(err, ErrVMState) {
		t.Fatalf("err = %v, want ErrVMState", err)
	}
	if _, err := m.WaitReady(context.Background(), "nope", time.Second); !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("err = %v, want ErrVMNotFound", err)
	}
}
