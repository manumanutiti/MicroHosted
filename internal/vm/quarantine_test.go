package vm

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"microhosted/pkg/types"
)

// Quarantine on a stopped VM touches no device (it holds no TAP), so the
// bookkeeping half can be checked without root: the record leaves its network,
// is marked, labelled, persisted, and keeps its name and other labels.
func TestQuarantineMarksAndPersists(t *testing.T) {
	m := newTestManager(t)
	rec := &types.VM{
		Config: types.VMConfig{
			ID: "a0000001", Name: "ts01-a", Labels: map[string]string{"sensor": "ts-01"},
			NetworkName: "plant", Bridge: "mhbr0", TapDevice: "tapa0000001", GuestIP: "10.0.0.5",
		},
		State:     types.VMStateStopped,
		CreatedAt: time.Now(),
	}
	m.Reconcile([]*types.VM{rec})
	before, _ := m.Get("a0000001")

	got, err := m.Quarantine("a0000001")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Config.Quarantine || got.Config.NetworkName != "" || got.Config.Bridge != "" {
		t.Errorf("quarantine=%v network=%q bridge=%q, want true, empty, empty", got.Config.Quarantine, got.Config.NetworkName, got.Config.Bridge)
	}
	if got.Config.GuestIP != "10.0.0.5" || got.Config.TapDevice != "tapa0000001" || got.Config.Name != "ts01-a" {
		t.Errorf("guest ip %q, tap %q, name %q: what the guest believes, its TAP and its name must be kept", got.Config.GuestIP, got.Config.TapDevice, got.Config.Name)
	}
	want := map[string]string{"sensor": "ts-01", LeaseLabel: LeaseQuarantined}
	if !reflect.DeepEqual(got.Config.Labels, want) {
		t.Errorf("labels = %v, want %v", got.Config.Labels, want)
	}
	if before.Config.Quarantine || before.Config.Labels[LeaseLabel] != "" {
		t.Errorf("an earlier copy changed under its reader: %+v", before.Config)
	}

	stored, err := m.store.ListVMs()
	if err != nil || len(stored) != 1 {
		t.Fatalf("store: %v, %d records", err, len(stored))
	}
	if !stored[0].Config.Quarantine || stored[0].Config.NetworkName != "" || stored[0].Config.Labels[LeaseLabel] != LeaseQuarantined {
		t.Errorf("persisted record not quarantined: %+v", stored[0].Config)
	}

	// One way only.
	if _, err := m.Quarantine("a0000001"); !errors.Is(err, ErrVMState) {
		t.Errorf("second quarantine = %v, want ErrVMState", err)
	}
}

func TestQuarantineRefusals(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Quarantine("nope0000"); !errors.Is(err, ErrVMNotFound) {
		t.Errorf("unknown VM = %v, want ErrVMNotFound", err)
	}

	full := make(map[string]string, maxLabels)
	for i := 0; i < maxLabels; i++ {
		full[string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	rec := &types.VM{
		Config:    types.VMConfig{ID: "b0000001", NetworkName: "plant", Labels: full},
		State:     types.VMStateStopped,
		CreatedAt: time.Now(),
	}
	m.Reconcile([]*types.VM{rec})

	// Another operation in flight on the VM.
	if err := m.begin("b0000001", "start"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Quarantine("b0000001"); !errors.Is(err, ErrConflict) {
		t.Errorf("busy VM = %v, want ErrConflict", err)
	}
	m.end("b0000001")

	// No room for the lease label: refused before anything changes.
	if _, err := m.Quarantine("b0000001"); !errors.Is(err, ErrInvalid) {
		t.Errorf("VM at the label cap = %v, want ErrInvalid", err)
	}
	if got, _ := m.Get("b0000001"); got.Config.Quarantine || got.Config.NetworkName != "plant" {
		t.Errorf("a refused quarantine changed the VM: %+v", got.Config)
	}
}
