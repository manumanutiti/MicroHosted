package vm

import (
	"context"
	"errors"
	"log"
	"syscall"
	"time"

	"microhosted/internal/firecracker"
	"microhosted/internal/jailer"
	"microhosted/internal/vsock"
	"microhosted/pkg/types"
)

// gracefulWindow bounds how long a stop waits for a guest that accepted the
// power-off request to finish it. busybox init takes ~2-3 s (SIGTERM to every
// process, a grace second, SIGKILL, unmount); a systemd guest stopping its
// units takes longer, so this is above the old 5-second ACPI window.
// A var so tests can shorten it.
var gracefulWindow = 10 * time.Second

// agentRequestTimeout bounds the power-off request itself: the agent answers
// as soon as the guest's init has the reboot signal, well before it acts on it.
const agentRequestTimeout = 3 * time.Second

// guestPowerOff is what the agent runs. sync first, in the foreground, so the
// guest's dirty pages reach the disk even if the orderly shutdown never
// completes. reboot, not poweroff, because Firecracker exits on a guest reboot
// on both architectures while a poweroff can leave a halted guest in a live
// process. The reboot is detached and slightly delayed: run inline, init's
// SIGTERM to every process kills the agent before it sends its exit marker,
// and the request would read as refused.
const guestPowerOff = "sync; (sleep 0.2; reboot) </dev/null >/dev/null 2>&1 &"

// agentExec runs a command through a VM's guest agent; tests replace it.
var agentExec = vsock.ExecContext

// The ways to end a VM this daemon launched; tests replace them.
var (
	vmmStop = firecracker.Stop
	vmmKill = firecracker.Kill
)

// halt ends a VM's Firecracker process. graceful asks the guest to shut itself
// down first, over its vsock agent: the only path that works on every guest
// Firecracker runs — SendCtrlAltDel needs an i8042 keyboard the CI kernels do
// not build and is not implemented on aarch64 at all. A guest without an agent
// (or one that refuses) falls back to that ACPI path; a guest that accepts but
// does not go down within gracefulWindow is killed — not handed to the ACPI
// path, whose own window would only extend the stop. The guest is untrusted, so
// nothing it answers can make a stop take longer than those bounds.
//
// graceful false kills outright, for a caller that discards the guest's state
// anyway (a destroy, a restore, a replace --old destroy).
//
// Whatever that did — or skipped, for a record with no live PID — the VM's
// cgroup is emptied last (jailer.KillCgroup): the record's PID and the SDK
// handle are the daemon's view, the cgroup is the kernel's. An error wrapping
// jailer.ErrCgroupNotDrained means something of the VM may still run: the
// caller must keep it counted as alive and release nothing it holds.
func (m *Manager) halt(ctx context.Context, record *types.VM, r *running, graceful bool) error {
	err := m.endProcess(ctx, record, r, graceful)
	return errors.Join(err, jailer.KillCgroup(m.jailerCfg, record.Config.ID))
}

func (m *Manager) endProcess(ctx context.Context, record *types.VM, r *running, graceful bool) error {
	owned := r != nil && r.machine != nil
	// An adopted VM is signalled by PID, so the PID must still be the VM's: a
	// recycled one belongs to a stranger (root would SIGKILL it).
	if !owned && !m.processAlive(record.PID, record.Config.ID) {
		return nil // no process to end
	}
	if graceful && record.VsockPath != "" {
		start := time.Now()
		if m.requestGuestPowerOff(ctx, record) {
			if m.waitExit(ctx, record, r, gracefulWindow) {
				log.Printf("vm %s: guest powered off cleanly in %s", record.Config.ID, time.Since(start).Round(time.Millisecond))
				if owned {
					// Already exited: StopVMM finds the process finished and
					// returns nil; this only settles the SDK's handle.
					return vmmKill(ctx, r.machine)
				}
				return nil
			}
			// The guest took the request and stayed up: it had its chance
			// at a clean shutdown, so no ACPI window on top of this one.
			log.Printf("vm %s: guest accepted the power-off but was still up after %s; killing it", record.Config.ID, gracefulWindow)
			graceful = false
		}
	}
	switch {
	case owned && graceful:
		return vmmStop(ctx, r.machine)
	case owned:
		return vmmKill(ctx, r.machine)
	default:
		// Adopted VM: no SDK handle, signal the process directly.
		return stopByPID(record.PID)
	}
}

// requestGuestPowerOff asks the guest agent to sync and reboot. true means the
// agent confirmed the request; the guest may still ignore it, which waitExit's
// bound covers.
func (m *Manager) requestGuestPowerOff(ctx context.Context, record *types.VM) bool {
	_, code, err := agentExec(ctx, record.VsockPath, record.Config.JailUID, guestPowerOff, agentRequestTimeout)
	if err != nil {
		log.Printf("vm %s: guest agent did not take the power-off request (%v); falling back to ACPI", record.Config.ID, err)
		return false
	}
	if code != 0 {
		log.Printf("vm %s: guest power-off request exited %d; falling back to ACPI", record.Config.ID, code)
		return false
	}
	return true
}

// waitExit waits up to window for the VM's Firecracker process to exit, and
// reports whether it did.
func (m *Manager) waitExit(ctx context.Context, record *types.VM, r *running, window time.Duration) bool {
	wctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	if r != nil && r.machine != nil {
		_ = r.machine.Wait(wctx)
		return wctx.Err() == nil
	}
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if syscall.Kill(record.PID, 0) == syscall.ESRCH || !m.processAlive(record.PID, record.Config.ID) {
			return true
		}
		select {
		case <-wctx.Done():
			return false
		case <-t.C:
		}
	}
}
