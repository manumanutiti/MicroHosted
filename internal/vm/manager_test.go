package vm

import (
	"context"
	"errors"
	"os"
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
// so its disk and IP survive a daemon restart and Start can bring it back. And
// it stays off: autostart is for involuntary deaths, not for a VM the operator
// powered off.
func TestReconcileKeepsStoppedVM(t *testing.T) {
	m := newTestManager(t)

	rec := &types.VM{
		Config:    types.VMConfig{ID: "deadbeef", GuestIP: "172.16.0.2", NetworkName: "default", Autostart: true},
		State:     types.VMStateStopped,
		PID:       0, // powered off — no process
		CreatedAt: time.Now(),
	}

	keepTaps, autostart := m.Reconcile([]*types.VM{rec})

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
	if len(autostart) != 0 {
		t.Errorf("autostart = %v, want empty: a VM stopped on purpose stays off", autostart)
	}
}

// deadRunningVM is a record that claims running but has no process behind it —
// what every VM looks like to the daemon after a host reboot. rootfs is its
// disk path.
func deadRunningVM(id, rootfs string, autostart bool) *types.VM {
	return &types.VM{
		Config: types.VMConfig{
			ID: id, Rootfs: rootfs, NetworkName: "default", GuestIP: "172.16.0.9",
			TapDevice: "tap" + id, Autostart: autostart,
		},
		State:      types.VMStateRunning,
		PID:        0, // no live process backing the "running" claim
		SocketPath: "/nonexistent/firecracker.socket",
		VsockPath:  "/nonexistent/v.sock",
		CreatedAt:  time.Now(),
	}
}

// A host reboot kills every Firecracker process, but the disks survive. A dead
// VM whose disk is still there must be kept as stopped — tracked, persisted as
// stopped, runtime fields cleared — so Start brings it back with its data. The
// old behaviour swept it, disk included, turning every reboot into data loss.
func TestReconcileKeepsDeadVMWithDisk(t *testing.T) {
	m := newTestManager(t)
	disk := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := deadRunningVM("cafebabe", disk, false)
	if err := m.store.SaveVM(rec); err != nil {
		t.Fatal(err)
	}

	keepTaps, autostart := m.Reconcile([]*types.VM{rec})

	got, ok := m.Get("cafebabe")
	if !ok {
		t.Fatal("dead VM with its disk intact was swept; it must be kept as stopped")
	}
	if got.State != types.VMStateStopped || got.PID != 0 || got.SocketPath != "" || got.VsockPath != "" {
		t.Errorf("record = state %q pid %d socket %q vsock %q, want stopped with runtime fields cleared",
			got.State, got.PID, got.SocketPath, got.VsockPath)
	}
	if _, err := os.Stat(disk); err != nil {
		t.Errorf("disk was removed: %v", err)
	}
	// Its TAP died with the process — nothing live to keep from the sweep.
	if len(keepTaps) != 0 {
		t.Errorf("keepTaps = %v, want empty for a dead VM", keepTaps)
	}
	if len(autostart) != 0 {
		t.Errorf("autostart = %v, want empty without Autostart", autostart)
	}

	// Persisted as stopped: the next startup must see it that way too.
	recs, err := m.store.ListVMs()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].State != types.VMStateStopped {
		t.Errorf("persisted records = %+v, want one stopped VM", recs)
	}
}

// A dead VM that asked for autostart is handed back to the caller to boot.
func TestReconcileAutostartsDeadVM(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	disk := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("disk"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	_, autostart := m.Reconcile([]*types.VM{
		deadRunningVM("aaaa1111", disk("a.ext4"), true),
		deadRunningVM("bbbb2222", disk("b.ext4"), false),
	})

	if len(autostart) != 1 || autostart[0] != "aaaa1111" {
		t.Errorf("autostart = %v, want [aaaa1111]", autostart)
	}
}

// A dead VM whose disk is gone too has nothing left to boot: it is swept and
// forgotten, record included. Guards the reconcile branch from over-keeping.
func TestReconcileSweepsDeadVMWithoutDisk(t *testing.T) {
	m := newTestManager(t)
	rec := deadRunningVM("cafebabe", filepath.Join(t.TempDir(), "missing.ext4"), true)
	if err := m.store.SaveVM(rec); err != nil {
		t.Fatal(err)
	}

	_, autostart := m.Reconcile([]*types.VM{rec})

	if _, ok := m.Get("cafebabe"); ok {
		t.Fatal("dead VM without a disk was kept; it must be swept")
	}
	if len(autostart) != 0 {
		t.Errorf("autostart = %v, want empty for a swept VM", autostart)
	}
	if recs, _ := m.store.ListVMs(); len(recs) != 0 {
		t.Errorf("persisted records = %d, want the swept VM's record dropped", len(recs))
	}
}

// SetAutostart persists the flag, so it survives the restart it exists for.
func TestSetAutostart(t *testing.T) {
	m := newTestManager(t)
	m.Reconcile([]*types.VM{{Config: types.VMConfig{ID: "deadbeef"}, State: types.VMStateStopped}})

	if _, err := m.SetAutostart("nope", true); !errors.Is(err, ErrVMNotFound) {
		t.Errorf("SetAutostart of unknown vm: err = %v, want ErrVMNotFound", err)
	}
	got, err := m.SetAutostart("deadbeef", true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Config.Autostart {
		t.Error("returned record has Autostart = false")
	}
	recs, err := m.store.ListVMs()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || !recs[0].Config.Autostart {
		t.Errorf("persisted records = %+v, want Autostart = true", recs)
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

	if _, err := m.ForkVM(context.Background(), "nope", types.ForkVMRequest{Quarantine: true}); !errors.Is(err, ErrVMNotFound) {
		t.Errorf("ForkVM of unknown vm: err = %v, want ErrVMNotFound", err)
	}

	stopped := &types.VM{
		Config: types.VMConfig{ID: "deadbeef"},
		State:  types.VMStateStopped,
	}
	m.Reconcile([]*types.VM{stopped})
	if _, err := m.ForkVM(context.Background(), "deadbeef", types.ForkVMRequest{Quarantine: true}); !errors.Is(err, ErrVMState) {
		t.Errorf("ForkVM of stopped vm: err = %v, want ErrVMState", err)
	}
}

func TestForkUnknownSnapshot(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Fork(context.Background(), "nope", types.ForkVMRequest{}); !errors.Is(err, ErrSnapshotNotFound) {
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
