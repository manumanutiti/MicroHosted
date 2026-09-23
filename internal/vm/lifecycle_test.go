package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"microhosted/internal/jailer"
	"microhosted/internal/network"
	"microhosted/internal/store"
	"microhosted/pkg/types"
)

// newStoreManager is newTestManager with a real instances dir and chroot
// base, for the paths that touch per-VM files.
func newStoreManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	jcfg := jailer.Defaults{ChrootBaseDir: t.TempDir(), ExecFile: "/usr/bin/firecracker", JailerBinary: "/usr/bin/jailer"}
	return NewManager(nil, jcfg, t.TempDir(), st, network.NewManager(st, nil))
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The daemon died in the middle of a create: the record says "creating" and
// the disk, log and jail dir exist. Startup must undo all of it — the VM was
// never handed to anyone — and forget the record.
func TestReconcileUndoesInterruptedCreate(t *testing.T) {
	m := newStoreManager(t)
	id := "c0ffee00"
	clone := filepath.Join(m.instancesDir, id+".ext4")
	logPath := filepath.Join(m.instancesDir, id+".log")
	touch(t, clone)
	touch(t, logPath)
	touch(t, filepath.Join(jailer.WorkspaceRoot(m.jailerCfg, id), "rootfs.ext4"))

	rec := &types.VM{Config: types.VMConfig{ID: id, Rootfs: clone}, State: types.VMStateCreating, LogPath: logPath}
	if err := m.store.SaveVM(rec); err != nil {
		t.Fatal(err)
	}
	m.Reconcile([]*types.VM{rec})

	if _, ok := m.Get(id); ok {
		t.Error("an interrupted create was adopted")
	}
	for _, p := range []string{clone, logPath, jailer.InstanceDir(m.jailerCfg, id)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the undo (err %v)", p, err)
		}
	}
	if recs, _ := m.store.ListVMs(); len(recs) != 0 {
		t.Errorf("record kept: %+v", recs)
	}
}

func TestAdmitMemoryReserve(t *testing.T) {
	m := newTestManager(t)
	m.SetLimits(Limits{MemReserveMB: 500, MaxParallelBoots: 4})
	m.memAvailable = func() (int64, error) { return 1000, nil }

	rel, err := m.admit(context.Background(), "aaaa0001", 300) // leaves 700
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	// 1000 - 300 in flight - 300 = 400 < 500: the in-flight launch counts
	// although MemAvailable does not show it yet.
	if _, err := m.admit(context.Background(), "aaaa0002", 300); !errors.Is(err, ErrCapacity) {
		t.Errorf("second admit = %v, want ErrCapacity", err)
	}
	rel()
	rel2, err := m.admit(context.Background(), "aaaa0002", 300)
	if err != nil {
		t.Errorf("admit after release: %v", err)
	} else {
		rel2()
	}

	m.memAvailable = func() (int64, error) { return 0, errors.New("no /proc") }
	if _, err := m.admit(context.Background(), "aaaa0003", 1); !errors.Is(err, ErrCapacity) {
		t.Errorf("admit without a memory reading = %v, want ErrCapacity (fail closed)", err)
	}
}

func TestAdmitMaxVMs(t *testing.T) {
	m := newTestManager(t)
	m.SetLimits(Limits{MaxVMs: 2})
	m.memAvailable = func() (int64, error) { return 1 << 20, nil }
	m.vms["run00001"] = &types.VM{Config: types.VMConfig{ID: "run00001"}, State: types.VMStateRunning}
	m.vms["a0000001"] = &types.VM{Config: types.VMConfig{ID: "a0000001"}, State: types.VMStateStopped}

	rel, err := m.admit(context.Background(), "new00001", 64)
	if err != nil {
		t.Fatalf("admit under the cap: %v", err)
	}
	defer rel()
	if _, err := m.admit(context.Background(), "new00002", 64); !errors.Is(err, ErrCapacity) {
		t.Errorf("admit over the cap = %v, want ErrCapacity", err)
	}
}

// A launch waits for a boot slot, and gives up when its request is cancelled
// instead of queueing forever.
func TestAdmitWaitsForSlot(t *testing.T) {
	m := newTestManager(t)
	m.SetLimits(Limits{MaxParallelBoots: 1})
	m.memAvailable = func() (int64, error) { return 1 << 20, nil }

	rel, err := m.admit(context.Background(), "slot0001", 64)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := m.admit(ctx, "slot0002", 64); !errors.Is(err, ErrCapacity) {
		t.Errorf("admit with every slot taken = %v, want ErrCapacity after the context ends", err)
	}
	rel()
	rel2, err := m.admit(context.Background(), "slot0002", 64)
	if err != nil {
		t.Errorf("admit after the slot freed: %v", err)
	} else {
		rel2()
	}
}

// One lifecycle operation per VM at a time: a second one is a 409, not a race.
func TestBusyVMRejectsSecondOperation(t *testing.T) {
	m := newTestManager(t)
	m.Reconcile([]*types.VM{{Config: types.VMConfig{ID: "deadbeef"}, State: types.VMStateStopped}})
	if err := m.begin("deadbeef", "start"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(context.Background(), "deadbeef"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "start in progress") {
		t.Errorf("Start on a busy VM = %v, want ErrConflict naming the operation", err)
	}
	if err := m.Destroy(context.Background(), "deadbeef"); !errors.Is(err, ErrConflict) {
		t.Errorf("Destroy on a busy VM = %v, want ErrConflict", err)
	}
	m.end("deadbeef")
	if err := m.Destroy(context.Background(), "deadbeef"); err != nil {
		t.Errorf("Destroy once idle: %v", err)
	}
}

// A VM whose process died while the daemon is up is marked stopped, with the
// reason, and persisted that way. It is not restarted.
func TestMonitorReapsDeadVM(t *testing.T) {
	m := newStoreManager(t)
	rec := &types.VM{
		Config:     types.VMConfig{ID: "dead0001", Autostart: true},
		State:      types.VMStateRunning,
		PID:        0, // no process: what the OOM killer leaves
		SocketPath: "/nonexistent",
		VsockPath:  "/nonexistent",
	}
	m.vms[rec.Config.ID] = rec
	m.run[rec.Config.ID] = &running{}

	m.reapDead()

	got, _ := m.Get("dead0001")
	if got.State != types.VMStateStopped || got.PID != 0 || got.VsockPath != "" {
		t.Errorf("after reap: state %q pid %d vsock %q, want stopped with runtime fields cleared", got.State, got.PID, got.VsockPath)
	}
	if got.LastExit == nil || got.LastExit.Reason == "" {
		t.Errorf("LastExit = %+v, want the death recorded", got.LastExit)
	}
	recs, _ := m.store.ListVMs()
	if len(recs) != 1 || recs[0].State != types.VMStateStopped || recs[0].LastExit == nil {
		t.Errorf("persisted = %+v, want stopped with LastExit", recs)
	}
}

// The monitor leaves a VM alone while an operation is acting on it: a Stop
// in progress has a dying process by design.
func TestMonitorSkipsBusyVM(t *testing.T) {
	m := newStoreManager(t)
	m.vms["d0000001"] = &types.VM{Config: types.VMConfig{ID: "d0000001"}, State: types.VMStateRunning}
	m.busy["d0000001"] = "stop"
	m.reapDead()
	if m.vms["d0000001"].State != types.VMStateRunning {
		t.Error("the monitor reaped a VM with an operation in progress")
	}
}

func TestSweepResidueReleasesOrphanVolumeClaims(t *testing.T) {
	m := newStoreManager(t)
	vol := &types.Volume{ID: "vol00001", Name: "data", Path: "/nope.ext4", AttachedTo: "gone0001"}
	m.vols[vol.ID] = vol
	m.SweepResidue()
	if vol.AttachedTo != "" {
		t.Errorf("volume still claimed by a VM that does not exist: %q", vol.AttachedTo)
	}
}

func TestSweepResidueRemovesJailDirsOfStoppedVMs(t *testing.T) {
	m := newStoreManager(t)
	m.vms["a0000001"] = &types.VM{Config: types.VMConfig{ID: "a0000001"}, State: types.VMStateStopped}
	touch(t, filepath.Join(jailer.WorkspaceRoot(m.jailerCfg, "a0000001"), "x"))
	touch(t, filepath.Join(jailer.WorkspaceRoot(m.jailerCfg, "b0000001"), "x"))
	// Not an ID of ours: never touched.
	touch(t, filepath.Join(filepath.Dir(jailer.InstanceDir(m.jailerCfg, "x")), "keep-me", "x"))

	m.SweepResidue()

	for _, id := range []string{"a0000001", "b0000001"} {
		if _, err := os.Stat(jailer.InstanceDir(m.jailerCfg, id)); !os.IsNotExist(err) {
			t.Errorf("jail dir of %s survived", id)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(jailer.InstanceDir(m.jailerCfg, "x")), "keep-me")); err != nil {
		t.Errorf("a directory that is not a VM ID was removed: %v", err)
	}
}

func TestDoctorFindsResidue(t *testing.T) {
	m := newStoreManager(t)
	// A tracked stopped VM with its disk: owned, not residue.
	disk := filepath.Join(m.instancesDir, "a0000001.ext4")
	touch(t, disk)
	m.vms["a0000001"] = &types.VM{Config: types.VMConfig{ID: "a0000001", Rootfs: disk}, State: types.VMStateStopped}
	// Residue.
	touch(t, filepath.Join(m.instancesDir, "b0000001.ext4"))
	touch(t, filepath.Join(m.instancesDir, "b0000002.log"))
	touch(t, filepath.Join(m.instancesDir, "snapshots", "c0000001", "mem"))
	touch(t, filepath.Join(jailer.WorkspaceRoot(m.jailerCfg, "b0000003"), "x"))
	m.vols["vol00001"] = &types.Volume{ID: "vol00001", Name: "data", AttachedTo: "gone0001"}
	// An interrupted create, in the store only.
	if err := m.store.SaveVM(&types.VM{Config: types.VMConfig{ID: "half0001"}, State: types.VMStateCreating}); err != nil {
		t.Fatal(err)
	}
	// An in-flight operation's files are not residue.
	m.busy["d0000001"] = "create"
	touch(t, filepath.Join(m.instancesDir, "d0000001.ext4"))

	rep := m.Doctor()
	if rep.Clean {
		t.Fatal("doctor reported clean over residue")
	}
	kinds := make(map[string]string)
	for _, f := range rep.Findings {
		kinds[f.Kind] += f.Object + " "
	}
	for kind, obj := range map[string]string{
		"clone_orphan":        "b0000001.ext4",
		"log_orphan":          "b0000002.log",
		"snapshot_dir_orphan": "c0000001",
		"jail_orphan":         "b0000003",
		"volume_claim_orphan": "data",
		"record_interrupted":  "half0001",
	} {
		if !strings.Contains(kinds[kind], obj) {
			t.Errorf("missing %s for %s; findings: %+v", kind, obj, rep.Findings)
		}
	}
	for _, f := range rep.Findings {
		if strings.Contains(f.Object, "a0000001") || strings.Contains(f.Object, "d0000001") {
			t.Errorf("owned or in-flight resource reported: %+v", f)
		}
	}
	if len(rep.InFlight) != 1 {
		t.Errorf("InFlight = %v, want the busy create", rep.InFlight)
	}
}

func TestParseVMProcess(t *testing.T) {
	names := map[string]bool{"firecracker": true, "jailer": true}
	for cmd, want := range map[string]string{
		"/firecracker\x00--id\x009eb5f62a\x00--api-sock\x00/run/firecracker.socket\x00":  "9eb5f62a",
		"/usr/bin/jailer\x00--id\x00deadbeef\x00--exec-file\x00/usr/bin/firecracker\x00": "deadbeef",
		"/usr/bin/qemu\x00--id\x00deadbeef\x00":                                          "",
		"/firecracker\x00--id\x00not-ours\x00":                                           "",
	} {
		got, ok := parseVMProcess([]byte(cmd), names)
		if got != want || ok != (want != "") {
			t.Errorf("parseVMProcess(%q) = %q, %v; want %q", cmd, got, ok, want)
		}
	}
}
