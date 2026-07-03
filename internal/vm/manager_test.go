package vm

import (
	"path/filepath"
	"testing"
	"time"

	"microhosted/internal/jailer"
	"microhosted/internal/network"
	"microhosted/internal/store"
	"microhosted/pkg/types"
)

// newTestManager builds a Manager wired only to the pieces the reconcile path
// touches — a real store and network manager. Catalog/jailer/instancesDir are
// unused by the code under test, so their zero values are fine.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewManager(nil, jailer.Defaults{}, "", st, network.NewManager(st))
}

// A VM stopped on purpose (poweroff) has no live process by design. Reconcile
// must keep tracking it as stopped — not sweep it like a crashed VM would be —
// so its disk and IP survive a daemon restart and Start can bring it back.
func TestReconcileKeepsStoppedVM(t *testing.T) {
	m := newTestManager(t)

	rec := &types.VM{
		Config:    types.VMConfig{ID: "deadbeef", GuestIP: "172.16.0.2", NetworkName: "default"},
		State:     types.VMStateStopped,
		PID:       0, // powered off — no process
		CreatedAt: time.Now(),
	}

	keepTaps := m.Reconcile([]*types.VM{rec})

	got, ok := m.Get("deadbeef")
	if !ok {
		t.Fatal("stopped VM was swept by Reconcile; it must be kept")
	}
	if got.State != types.VMStateStopped {
		t.Errorf("state = %q, want %q", got.State, types.VMStateStopped)
	}
	// A stopped VM released its TAP at Stop, so it contributes nothing to keep.
	if len(keepTaps) != 0 {
		t.Errorf("keepTaps = %v, want empty for a stopped VM", keepTaps)
	}
}

// A VM whose record says running but whose process is gone (crashed / host
// reboot while the daemon was down) is the opposite case: it must be swept and
// forgotten, not adopted. Guards the reconcile branch from over-keeping.
func TestReconcileSweepsDeadRunningVM(t *testing.T) {
	m := newTestManager(t)

	rec := &types.VM{
		Config:    types.VMConfig{ID: "cafebabe", NetworkName: "default"},
		State:     types.VMStateRunning,
		PID:       0, // no live process backing the "running" claim
		CreatedAt: time.Now(),
	}

	m.Reconcile([]*types.VM{rec})

	if _, ok := m.Get("cafebabe"); ok {
		t.Fatal("dead running VM was kept; it must be swept")
	}
}
