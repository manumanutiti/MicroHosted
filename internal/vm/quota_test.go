package vm

import (
	"context"
	"errors"
	"testing"

	"microhosted/pkg/types"
)

func TestParseQuota(t *testing.T) {
	c, q, err := ParseConsumerQuota("ot=vms:50,mem:4096")
	if err != nil || c != "ot" || q != (Quota{MaxVMs: 50, MaxMemMB: 4096}) {
		t.Errorf("= %q %+v %v", c, q, err)
	}
	if _, q, err := ParseConsumerQuota("lab=mem:512"); err != nil || q != (Quota{MaxMemMB: 512}) {
		t.Errorf("mem only = %+v %v", q, err)
	}
	for _, bad := range []string{"", "ot", "=vms:1", "ot=", "ot=vms:0", "ot=vms:-1", "ot=cpus:2", "ot=vms", "bad value=vms:1"} {
		if _, _, err := ParseConsumerQuota(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A consumer is capped by its own quota — running VMs (quarantined included)
// and launches in progress — independently of other consumers and of VMs
// with no consumer; the default quota covers the consumers not named.
func TestAdmitQuota(t *testing.T) {
	m := newTestManager(t)
	def := Quota{MaxVMs: 1}
	m.SetLimits(Limits{Quotas: map[string]Quota{"ot": {MaxVMs: 3, MaxMemMB: 512}}, DefaultQuota: &def})
	m.memAvailable = func() (int64, error) { return 1 << 20, nil }
	run := func(id, owner string, memMB int64, quarantined bool) {
		m.vms[id] = &types.VM{Config: types.VMConfig{ID: id, MemMB: memMB, Labels: map[string]string{"managed-by": owner}, Quarantine: quarantined}, State: types.VMStateRunning}
	}
	run("ot000001", "ot", 128, false)
	run("ot000002", "ot", 128, true) // quarantined: still holds its RAM
	m.vms["ot000003"] = &types.VM{Config: types.VMConfig{ID: "ot000003", MemMB: 128, Labels: map[string]string{"managed-by": "ot"}}, State: types.VMStateStopped}

	// 256 MB held + 256 = 512: at the cap, admitted.
	rel, err := m.admit(context.Background(), "ot000004", "ot", 256, 0)
	if err != nil {
		t.Fatalf("up to the quota: %v", err)
	}
	// The launch in progress counts: 3 VMs held.
	if _, err := m.admit(context.Background(), "ot000005", "ot", 1, 0); !errors.Is(err, ErrQuota) {
		t.Errorf("over max vms = %v, want ErrQuota", err)
	}
	rel()
	if _, err := m.admit(context.Background(), "ot000005", "ot", 257, 0); !errors.Is(err, ErrQuota) {
		t.Errorf("over max mem = %v, want ErrQuota", err)
	}
	if _, err := m.admit(context.Background(), "ot000005", "ot", 257, 0); errors.Is(err, ErrCapacity) {
		t.Error("a quota refusal must not read as host capacity")
	}

	// Other consumers: the default quota; no consumer: host admission only.
	run("lab00001", "lab", 64, false)
	if _, err := m.admit(context.Background(), "lab00002", "lab", 64, 0); !errors.Is(err, ErrQuota) {
		t.Errorf("default quota = %v, want ErrQuota", err)
	}
	// Unlabelled VMs running beyond any quota: still admitted.
	for i := range 5 {
		id := "free000" + string(rune('0'+i))
		m.vms[id] = &types.VM{Config: types.VMConfig{ID: id, MemMB: 64}, State: types.VMStateRunning}
	}
	r, err := m.admit(context.Background(), "free0009", "", 64, 0)
	if err != nil {
		t.Fatalf("no consumer: %v", err)
	}
	r()

	got := map[string]types.QuotaUsage{}
	for _, u := range m.QuotaUsage() {
		got[u.Consumer] = u
	}
	if u := got["ot"]; u.VMs != 2 || u.MemMB != 256 || u.MaxVMs != 3 || u.MaxMemMB != 512 {
		t.Errorf("ot usage %+v", u)
	}
	if u := got["lab"]; u.VMs != 1 || u.MaxVMs != 1 {
		t.Errorf("lab usage %+v", u)
	}
	if _, ok := got[""]; ok {
		t.Error("VMs without a consumer reported as one")
	}
}

// managed-by is fixed at create: a patch cannot add, change or remove it,
// and a replace cannot give the replacement another owner.
func TestManagedByIsFixed(t *testing.T) {
	cfg := sensorVM() // managed-by=ot
	m, f := newReplaceManager(t, cfg)
	str := func(s string) *string { return &s }
	for name, patch := range map[string]map[string]*string{
		"change": {"managed-by": str("other")},
		"remove": {"managed-by": nil},
	} {
		if _, err := m.SetLabels(cfg.ID, patch); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := m.SetLabels(cfg.ID, map[string]*string{"managed-by": str("ot"), "role": str("x")}); err != nil {
		t.Errorf("same value is a no-op: %v", err)
	}
	if _, _, err := m.Replace(context.Background(), cfg.ID, types.ReplaceVMRequest{Labels: map[string]string{"managed-by": "other"}}); !errors.Is(err, ErrInvalid) || len(f.calls) != 0 {
		t.Errorf("replace to another owner = %v (launched %d)", err, len(f.calls))
	}

	unowned := sensorVM()
	unowned.ID, unowned.Name, unowned.Labels = "b0000002", "", nil
	m, _ = newReplaceManager(t, unowned)
	if _, err := m.SetLabels(unowned.ID, map[string]*string{"managed-by": str("ot")}); !errors.Is(err, ErrInvalid) {
		t.Errorf("adding it later = %v, want ErrInvalid", err)
	}
}
