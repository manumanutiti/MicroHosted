package vm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"microhosted/internal/events"
	"microhosted/pkg/types"
)

// fakeLauncher stands in for Fork/Create: it records what Replace asked for,
// what state the old VM was in at that moment, and registers the new VM the
// way a real launch would.
type fakeLauncher struct {
	m        *Manager
	oldID    string
	calls    []replacement
	oldAtRun []types.VMConfig
	fail     error
	n        int
}

func (f *fakeLauncher) launch(_ context.Context, r replacement) (*types.VM, error) {
	f.calls = append(f.calls, r)
	if old, ok := f.m.Get(f.oldID); ok {
		f.oldAtRun = append(f.oldAtRun, old.Config)
	}
	if f.fail != nil {
		return nil, f.fail
	}
	f.n++
	id := "e000000" + string(rune('0'+f.n))
	rec := &types.VM{
		Config:    types.VMConfig{ID: id, Name: r.name, Labels: r.labels, NetworkName: r.network, GuestIP: r.ip, TemplateName: r.template},
		State:     types.VMStateRunning,
		CreatedAt: time.Now(),
	}
	f.m.mu.Lock()
	f.m.vms[id] = rec
	f.m.run[id] = &running{}
	f.m.mu.Unlock()
	cp := *rec
	return &cp, nil
}

// newReplaceManager tracks one stopped VM serving a function (sensor ts-01 at
// 172.16.9.5 on "plant"). Stopped, so nothing here touches a TAP or a process.
func newReplaceManager(t *testing.T, cfg types.VMConfig) (*Manager, *fakeLauncher) {
	t.Helper()
	m := newTestManager(t)
	m.instancesDir = t.TempDir()
	m.jailerCfg.ChrootBaseDir = t.TempDir()
	m.Reconcile([]*types.VM{{Config: cfg, State: types.VMStateStopped, CreatedAt: time.Now()}})
	f := &fakeLauncher{m: m, oldID: cfg.ID}
	m.replaceLaunch = f.launch
	return m, f
}

func sensorVM() types.VMConfig {
	return types.VMConfig{
		ID: "a0000001", Name: "ts01-a", TemplateName: "alpine-py",
		Labels:      map[string]string{"sensor": "ts-01", "managed-by": "ot"},
		NetworkName: "plant", GuestIP: "172.16.9.5",
		VCPUs: 1, MemMB: 128, DiskMB: 512, Autostart: true,
	}
}

func TestReplaceHandsOverTheFunction(t *testing.T) {
	m, f := newReplaceManager(t, sensorVM())

	nv, old, err := m.Replace(context.Background(), "a0000001", types.ReplaceVMRequest{Name: "ts01-b", Labels: map[string]string{"gen": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("launched %d replacements, want 1", len(f.calls))
	}
	got := f.calls[0]
	want := replacement{
		template: "alpine-py", network: "plant", ip: "172.16.9.5", name: "ts01-b",
		labels: map[string]string{"sensor": "ts-01", "managed-by": "ot", "gen": "2"},
		vcpus:  1, memMB: 128, diskMB: 512,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replacement asked for\n  %+v\nwant\n  %+v", got, want)
	}
	// The order that makes the handover safe: the old VM was already off the
	// network (its address free) when the replacement launched.
	if at := f.oldAtRun[0]; !at.Quarantine || at.NetworkName != "" {
		t.Errorf("old VM at launch time: quarantine=%v network=%q; it must be cut off first", at.Quarantine, at.NetworkName)
	}

	if nv.Config.Replaces != "a0000001" || !nv.Config.Autostart {
		t.Errorf("replacement: replaces=%q autostart=%v, want a0000001 and the old VM's autostart", nv.Config.Replaces, nv.Config.Autostart)
	}
	if old == nil || !old.Config.Quarantine || old.Config.QuarantinedFrom != "plant" || old.Config.Labels[LeaseLabel] != LeaseQuarantined {
		t.Fatalf("old VM not left quarantined from plant: %+v", old)
	}
	if old.Config.Name != "ts01-a" {
		t.Errorf("old VM lost its name: %q", old.Config.Name)
	}
	stored, _ := m.store.ListVMs()
	for _, s := range stored {
		if s.Config.ID == nv.Config.ID && s.Config.Replaces != "a0000001" {
			t.Errorf("lineage not persisted: %+v", s.Config)
		}
	}

	// A second replace of the same VM would put two VMs on one function.
	_, _, err = m.Replace(context.Background(), "a0000001", types.ReplaceVMRequest{})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), nv.Config.ID) {
		t.Errorf("replacing an already replaced VM = %v, want ErrConflict naming %s", err, nv.Config.ID)
	}
	if len(f.calls) != 1 {
		t.Errorf("the refused replace still launched a VM (%d launches)", len(f.calls))
	}
}

func TestReplaceDestroysOldOnlyOnSuccess(t *testing.T) {
	m, f := newReplaceManager(t, sensorVM())
	f.fail = ErrCapacity

	_, old, err := m.Replace(context.Background(), "a0000001", types.ReplaceVMRequest{Old: types.ReplaceOldDestroy})
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("failed launch = %v, want the launch's own error (ErrCapacity) wrapped", err)
	}
	// Failed replacement: the suspect is kept, cut off, never reconnected.
	if old == nil || !old.Config.Quarantine || old.Config.NetworkName != "" {
		t.Fatalf("after a failed replace the old VM must stay quarantined: %+v", old)
	}

	// The same replace is retryable: the quarantined VM remembers its function.
	f.fail = nil
	nv, old, err := m.Replace(context.Background(), "a0000001", types.ReplaceVMRequest{Old: types.ReplaceOldDestroy})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if last := f.calls[len(f.calls)-1]; last.network != "plant" || last.ip != "172.16.9.5" {
		t.Errorf("retry launched at %s on %q, want the function's 172.16.9.5 on plant", last.ip, last.network)
	}
	if old != nil {
		t.Errorf("old = %+v, want nil (destroyed)", old)
	}
	if _, ok := m.Get("a0000001"); ok {
		t.Error("old VM still tracked after old=destroy")
	}
	if nv.Config.Replaces != "a0000001" {
		t.Errorf("replaces = %q", nv.Config.Replaces)
	}
	// Its name is free again; the replacement did not take it.
	if err := m.reserveName("ts01-a", "f0000001"); err != nil {
		t.Errorf("destroyed VM's name still taken: %v", err)
	}
}

func TestReplaceRefusesBeforeTouchingAnything(t *testing.T) {
	noNet := sensorVM()
	noNet.ID, noNet.Name, noNet.NetworkName, noNet.GuestIP = "b0000001", "", "", ""
	withVol := sensorVM()
	withVol.ID, withVol.Name = "c0000001", ""
	withVol.Volumes = []types.VolumeMount{{VolumeID: "v1"}}

	for _, tc := range []struct {
		name string
		cfg  types.VMConfig
		req  types.ReplaceVMRequest
		want error
	}{
		{"unknown disposition", sensorVM(), types.ReplaceVMRequest{Old: "keep"}, ErrInvalid},
		{"snapshot and template", sensorVM(), types.ReplaceVMRequest{Snapshot: "s", Template: "t"}, ErrInvalid},
		{"bad label", sensorVM(), types.ReplaceVMRequest{Labels: map[string]string{"k": "bad value"}}, ErrInvalid},
		{"unknown snapshot", sensorVM(), types.ReplaceVMRequest{Snapshot: "nope"}, ErrSnapshotNotFound},
		{"no network", noNet, types.ReplaceVMRequest{}, ErrConflict},
		{"volumes", withVol, types.ReplaceVMRequest{}, ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newReplaceManager(t, tc.cfg)
			if _, _, err := m.Replace(context.Background(), tc.cfg.ID, tc.req); !errors.Is(err, tc.want) {
				t.Errorf("= %v, want %v", err, tc.want)
			}
			if len(f.calls) != 0 {
				t.Error("a refused replace launched a VM")
			}
			if got, _ := m.Get(tc.cfg.ID); got.Config.Quarantine {
				t.Error("a refused replace quarantined the old VM")
			}
		})
	}

	m, _ := newReplaceManager(t, sensorVM())
	if _, _, err := m.Replace(context.Background(), "nope0000", types.ReplaceVMRequest{}); !errors.Is(err, ErrVMNotFound) {
		t.Errorf("unknown VM = %v, want ErrVMNotFound", err)
	}
}

// A snapshot taken anywhere but at the function's address would boot a
// replacement that does not serve the function: refused, old VM untouched.
func TestReplaceSnapshotMustBeAtTheFunctionsAddress(t *testing.T) {
	m, f := newReplaceManager(t, sensorVM())
	m.snaps["s1"] = &types.Snapshot{ID: "s1", NetworkName: "plant", GuestIP: "172.16.9.7"}
	m.snaps["s2"] = &types.Snapshot{ID: "s2", NetworkName: "plant", GuestIP: "172.16.9.5"}

	if _, _, err := m.Replace(context.Background(), "a0000001", types.ReplaceVMRequest{Snapshot: "s1"}); !errors.Is(err, ErrConflict) {
		t.Errorf("snapshot at another address = %v, want ErrConflict", err)
	}
	if got, _ := m.Get("a0000001"); got.Config.Quarantine || len(f.calls) != 0 {
		t.Error("the refused replace changed something")
	}
	if _, _, err := m.Replace(context.Background(), "a0000001", types.ReplaceVMRequest{Snapshot: "s2"}); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].snapshot != "s2" || f.calls[0].template != "" {
		t.Errorf("launch = %+v, want a fork of s2", f.calls[0])
	}
}

// What an orchestrator sees of a replace: the cut, where the function went,
// then the old VM's end — in that order, all on the function's network.
func TestReplaceEvents(t *testing.T) {
	m, f := newReplaceManager(t, sensorVM())
	bus := events.NewBus(0)
	m.SetEvents(bus)

	f.fail = ErrCapacity
	_, _, _ = m.Replace(context.Background(), "a0000001", types.ReplaceVMRequest{Old: types.ReplaceOldDestroy})
	f.fail = nil
	nv, _, err := m.Replace(context.Background(), "a0000001", types.ReplaceVMRequest{Old: types.ReplaceOldDestroy})
	if err != nil {
		t.Fatal(err)
	}

	backlog, _, _, sub := bus.Subscribe("", 0, true)
	sub.Close()
	var got []string
	for _, e := range backlog {
		got = append(got, e.Type)
		if e.VM != "a0000001" || e.Network != "plant" || e.Labels["sensor"] != "ts-01" {
			t.Errorf("%s: vm %q network %q labels %v; want the old VM on plant with its labels", e.Type, e.VM, e.Network, e.Labels)
		}
	}
	want := []string{types.EventVMQuarantined, types.EventVMReplaceFailed, types.EventVMReplaced, types.EventVMDestroyed}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if r := backlog[1].Reason; r == "" {
		t.Error("replace_failed carries no reason")
	}
	if d := backlog[2].Data; d["replacement"] != nv.Config.ID || d["old"] != types.ReplaceOldDestroy {
		t.Errorf("replaced data = %v", d)
	}
}
