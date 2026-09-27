package vm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"microhosted/internal/vsock"
	"microhosted/pkg/types"
)

// Readiness of a VM's guest agent. A create (or start, fork, restore) returns
// as soon as Firecracker runs, well before the guest's init has the vsock agent
// listening, so the daemon itself watches for the agent and publishes the
// answer: vm.ready (or vm.agent_unready) on the event stream, agent_ready_at on
// the VM. An exec that arrives early waits for it inside its own deadline.
//
// The HOST probes. The guest never announces itself: that would need a
// listener on the host side of vsock, a channel an untrusted guest could open
// at will — which the threat model rules out (Layer 5: only the host dials).
// A guest's claim to be ready would be worth nothing anyway; a completed
// handshake is the proof.

// Vars, not consts, so tests can shorten them.
var (
	// agentReadyBudget is how long after a boot the agent may take to answer
	// before vm.agent_unready. Alpine answers in ~100 ms, systemd guests in a
	// second or two; this leaves room for a slow first boot (an fsck).
	agentReadyBudget = 30 * time.Second
	// The probe is fast while a guest is expected to be coming up and backs
	// off after, so a fleet of guests without an agent costs little.
	agentProbeFast    = 20 * time.Millisecond
	agentProbeSlow    = 250 * time.Millisecond
	agentProbeFastFor = 2 * time.Second
	// agentProbeTimeout bounds a single handshake.
	agentProbeTimeout = time.Second
)

// ErrAgentUnready reports a VM whose guest agent did not answer within
// agentReadyBudget of its boot (and still does not).
var ErrAgentUnready = errors.New("guest agent not answering")

// agentProbe checks a VM's agent; tests replace it.
var agentProbe = vsock.Probe

// agentWatch is one incarnation's readiness watch. ready is written before
// done is closed and read only after, so the close orders the two.
type agentWatch struct {
	started time.Time
	done    chan struct{}
	ready   bool
}

// watchAgent starts watching the agent of a running VM's current incarnation.
// It is called once that incarnation is published (after vm.created,
// vm.started or vm.restored, and for VMs adopted at startup), so vm.ready
// never precedes them. Idempotent per incarnation.
func (m *Manager) watchAgent(id string) {
	m.mu.Lock()
	rec, ok := m.vms[id]
	r := m.run[id]
	if !ok || r == nil || r.agent != nil || rec.State != types.VMStateRunning || rec.VsockPath == "" {
		m.mu.Unlock()
		return
	}
	w := &agentWatch{started: time.Now(), done: make(chan struct{})}
	r.agent = w
	rec.AgentReadyAt = nil
	sock, uid := rec.VsockPath, rec.Config.JailUID
	m.mu.Unlock()
	go m.probeAgent(id, r, w, sock, uid)
}

func (m *Manager) probeAgent(id string, r *running, w *agentWatch, sock string, uid int) {
	defer close(w.done)
	deadline := w.started.Add(agentReadyBudget)
	for {
		if !m.isIncarnation(id, r) {
			return // stopped, destroyed or booted again meanwhile
		}
		err := agentProbe(context.Background(), sock, uid, agentProbeTimeout)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			if rec := m.settleAgent(id, r, w, false); rec != nil {
				log.Printf("vm %s: guest agent not answering %s after boot: %v", id, agentReadyBudget, err)
				m.emit(types.EventVMAgentUnready, rec, fmt.Sprintf("no answer from the guest agent within %s of boot: %v", agentReadyBudget, err), nil)
			}
			return
		}
		if time.Since(w.started) < agentProbeFastFor {
			time.Sleep(agentProbeFast)
		} else {
			time.Sleep(agentProbeSlow)
		}
	}
	after := time.Since(w.started)
	if rec := m.settleAgent(id, r, w, true); rec != nil {
		m.emit(types.EventVMReady, rec, "", map[string]string{"after_ms": strconv.FormatInt(after.Milliseconds(), 10)})
	}
}

// isIncarnation reports whether r is still the VM's running incarnation: every
// boot, restore, stop and destroy replaces or drops m.run[id].
func (m *Manager) isIncarnation(id string, r *running) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.run[id] == r
}

// settleAgent records a watch's outcome on the VM, if r is still its
// incarnation, and returns a copy of the record to publish (nil otherwise). w
// is the watch being settled, nil for a late answer after it closed (its ready
// flag is read without the lock once done is closed, so it is never written
// again).
func (m *Manager) settleAgent(id string, r *running, w *agentWatch, ready bool) *types.VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w != nil {
		w.ready = ready
	}
	rec, ok := m.vms[id]
	if !ok || m.run[id] != r {
		return nil
	}
	if ready {
		now := time.Now()
		rec.AgentReadyAt = &now
	}
	cp := *rec
	return &cp
}

// agentWatchOf returns the running incarnation's watch, or nil.
func (m *Manager) agentWatchOf(id string) *agentWatch {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.run[id]; r != nil {
		return r.agent
	}
	return nil
}

// awaitAgent waits, until deadline, for the VM's readiness watch to settle, so
// an exec or a file transfer issued right after a boot reaches a listening
// agent instead of failing its handshake. It returns whether the agent was
// found ready; false with a nil error means the watch gave up (or there is
// none) and the caller should just try. Past deadline the error wraps
// vsock.ErrTimeout.
func (m *Manager) awaitAgent(ctx context.Context, id string, deadline time.Time) (bool, error) {
	w := m.agentWatchOf(id)
	if w == nil {
		return false, nil
	}
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-w.done:
		return w.ready, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-t.C:
		return false, fmt.Errorf("%w: the guest agent of vm %s is not answering yet, %s after boot", vsock.ErrTimeout, id, time.Since(w.started).Round(time.Millisecond))
	}
}

// WaitReady blocks until a running VM's guest agent answers, or timeout. A VM
// whose watch gave up is probed once more — an agent that came up late is
// ready now — before it is reported ErrAgentUnready.
func (m *Manager) WaitReady(ctx context.Context, id string, timeout time.Duration) (*types.VM, error) {
	if timeout <= 0 || timeout > vsock.MaxExecTimeout {
		return nil, fmt.Errorf("%w: wait timeout %s is outside (0, %s]", ErrInvalid, timeout, vsock.MaxExecTimeout)
	}
	m.mu.Lock()
	rec, ok := m.vms[id]
	var state types.VMState
	if ok {
		state = rec.State
	}
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	if state != types.VMStateRunning {
		return nil, fmt.Errorf("%w: vm %s is not running (state %s)", ErrVMState, id, state)
	}
	m.watchAgent(id) // no-op if the incarnation already has one
	w := m.agentWatchOf(id)
	if w == nil {
		return nil, fmt.Errorf("%w: vm %s is not running", ErrVMState, id)
	}
	ready, err := m.awaitAgent(ctx, id, time.Now().Add(timeout))
	if err != nil {
		return nil, err
	}
	if !ready {
		m.mu.Lock()
		r, rec := m.run[id], m.vms[id]
		var sock string
		var uid int
		if rec != nil {
			sock, uid = rec.VsockPath, rec.Config.JailUID
		}
		m.mu.Unlock()
		if rec == nil || r == nil || r.agent != w {
			return nil, fmt.Errorf("%w: vm %s stopped or rebooted while waiting", ErrVMState, id)
		}
		if err := agentProbe(ctx, sock, uid, agentProbeTimeout); err != nil {
			return nil, fmt.Errorf("%w: vm %s, %s after boot: %v", ErrAgentUnready, id, time.Since(w.started).Round(time.Millisecond), err)
		}
		if late := m.settleAgent(id, r, nil, true); late != nil {
			m.emit(types.EventVMReady, late, "", map[string]string{"after_ms": strconv.FormatInt(time.Since(w.started).Milliseconds(), 10)})
		}
	}
	vm, ok := m.Get(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	return vm, nil
}
