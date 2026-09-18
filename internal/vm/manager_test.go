package vm

import (
	"context"
	"errors"
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
	return NewManager(nil, jailer.Defaults{}, "", st, network.NewManager(st, nil))
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

// Guards the error contract of the snapshot/fork/restore surface — the paths
// that don't need a live Firecracker to be exercised.
func TestSnapshotErrorPaths(t *testing.T) {
	m := newTestManager(t)

	if _, err := m.Snapshot(context.Background(), "nope", ""); !errors.Is(err, ErrVMNotFound) {
		t.Errorf("Snapshot of unknown vm: err = %v, want ErrVMNotFound", err)
	}

	// A stopped VM has no live Firecracker process to pause and snapshot.
	stopped := &types.VM{
		Config: types.VMConfig{ID: "deadbeef"},
		State:  types.VMStateStopped,
	}
	m.Reconcile([]*types.VM{stopped})
	if _, err := m.Snapshot(context.Background(), "deadbeef", ""); !errors.Is(err, ErrVMState) {
		t.Errorf("Snapshot of stopped vm: err = %v, want ErrVMState", err)
	}
}

// Direct fork rides on Snapshot, so it inherits its preconditions: the source
// VM must exist and be running (there's no memory to freeze otherwise).
func TestForkVMErrorPaths(t *testing.T) {
	m := newTestManager(t)

	if _, err := m.ForkVM(context.Background(), "nope", true); !errors.Is(err, ErrVMNotFound) {
		t.Errorf("ForkVM of unknown vm: err = %v, want ErrVMNotFound", err)
	}

	stopped := &types.VM{
		Config: types.VMConfig{ID: "deadbeef"},
		State:  types.VMStateStopped,
	}
	m.Reconcile([]*types.VM{stopped})
	if _, err := m.ForkVM(context.Background(), "deadbeef", true); !errors.Is(err, ErrVMState) {
		t.Errorf("ForkVM of stopped vm: err = %v, want ErrVMState", err)
	}
}

func TestForkUnknownSnapshot(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Fork(context.Background(), "nope", false); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("Fork of unknown snapshot: err = %v, want ErrSnapshotNotFound", err)
	}
}

// In-place restore is restricted to the VM's own snapshots: the guest identity
// (IP, MAC, drive name) frozen in another VM's snapshot would silently replace
// this VM's. That cross-VM operation is Fork, and Restore must refuse it.
func TestRestoreRejectsForeignSnapshot(t *testing.T) {
	m := newTestManager(t)

	vm := &types.VM{Config: types.VMConfig{ID: "deadbeef"}, State: types.VMStateStopped}
	m.Reconcile([]*types.VM{vm})

	snapDir := t.TempDir() // exists, so LoadSnapshots keeps it
	m.LoadSnapshots([]*types.Snapshot{{ID: "snap1234", SourceVMID: "otro-vm1", Dir: snapDir}})

	if _, err := m.Restore(context.Background(), "deadbeef", "snap1234"); !errors.Is(err, ErrConflict) {
		t.Errorf("Restore with foreign snapshot: err = %v, want ErrConflict", err)
	}
	if _, err := m.Restore(context.Background(), "deadbeef", "nope"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("Restore with unknown snapshot: err = %v, want ErrSnapshotNotFound", err)
	}
}

// A snapshot whose directory was removed out-of-band can only produce failing
// restores — LoadSnapshots must drop it (and its record) instead of indexing it.
func TestLoadSnapshotsDropsMissingDir(t *testing.T) {
	m := newTestManager(t)

	alive := t.TempDir()
	m.LoadSnapshots([]*types.Snapshot{
		{ID: "kept1234", SourceVMID: "a", Dir: alive},
		{ID: "gone1234", SourceVMID: "a", Dir: filepath.Join(alive, "does-not-exist")},
	})

	if _, ok := m.GetSnapshot("kept1234"); !ok {
		t.Error("snapshot with existing dir was dropped")
	}
	if _, ok := m.GetSnapshot("gone1234"); ok {
		t.Error("snapshot with missing dir was kept")
	}
}
