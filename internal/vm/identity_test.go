package vm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"microhosted/pkg/types"
)

func TestValidateName(t *testing.T) {
	for name, ok := range map[string]bool{
		"":                      true,
		"ts01-a":                true,
		"a":                     true,
		"parser-7":              true,
		"TS01":                  false, // lowercase only
		"-ts01":                 false,
		"ts01-":                 false,
		"ts_01":                 false,
		"ts.01":                 false,
		"deadbeef":              false, // the shape of a VM ID
		"deadbeef-1":            true,
		strings.Repeat("a", 64): false,
	} {
		if err := ValidateName(name); (err == nil) != ok {
			t.Errorf("ValidateName(%q) = %v, want ok=%v", name, err, ok)
		} else if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateName(%q) = %v, want ErrInvalid", name, err)
		}
	}
}

func TestValidateLabels(t *testing.T) {
	for _, tc := range []struct {
		labels map[string]string
		ok     bool
	}{
		{nil, true},
		{map[string]string{"sensor": "ts-01"}, true},
		{map[string]string{"ot.plant/managed-by": "ot-orch"}, true},
		{map[string]string{"role": ""}, true}, // present, empty value
		{map[string]string{"Sensor": "x"}, false},
		{map[string]string{"sensor": "has space"}, false},
		{map[string]string{"sensor": "-x"}, false},
		{map[string]string{"": "x"}, false},
	} {
		if err := ValidateLabels(tc.labels); (err == nil) != tc.ok {
			t.Errorf("ValidateLabels(%v) = %v, want ok=%v", tc.labels, err, tc.ok)
		}
	}
	many := make(map[string]string)
	for i := 0; i <= maxLabels; i++ {
		many[string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	if err := ValidateLabels(many); err == nil {
		t.Errorf("%d labels accepted, the cap is %d", len(many), maxLabels)
	}
}

// The shape is checked before anything else: a VM with -1 vCPUs must be a 400,
// not a Firecracker failure after a clone and a TAP were made.
func TestCreateRejectsInvalidRequestFirst(t *testing.T) {
	m := newTestManager(t) // no catalog: reaching it would panic
	for _, req := range []types.CreateVMRequest{
		{Template: "t", VCPUs: -1},
		{Template: "t", VCPUs: maxVCPUs + 1},
		{Template: "t", MemMB: -5},
		{Template: "t", MemMB: 8},
		{Template: "t", DiskMB: -1},
		{Template: "t", Name: "Bad Name"},
		{Template: "t", Labels: map[string]string{"k": "bad value"}},
		{Template: "t", NoNetwork: true, GuestIP: "172.16.0.9"},
	} {
		if _, err := m.Create(context.Background(), req); !errors.Is(err, ErrInvalid) {
			t.Errorf("Create(%+v) = %v, want ErrInvalid", req, err)
		}
	}
	if _, err := m.Fork(context.Background(), "snap", types.ForkVMRequest{Name: "deadbeef"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Fork with an ID-shaped name = %v, want ErrInvalid", err)
	}
}

func TestNameReservation(t *testing.T) {
	m := newTestManager(t)
	if err := m.reserveName("ts01-a", "aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	// Idempotent for its owner, refused to anyone else.
	if err := m.reserveName("ts01-a", "aaaaaaaa"); err != nil {
		t.Errorf("owner re-reserving its own name: %v", err)
	}
	if err := m.reserveName("ts01-a", "bbbbbbbb"); !errors.Is(err, ErrConflict) {
		t.Errorf("second VM taking a name = %v, want ErrConflict", err)
	}
	// Only the owner's release frees it.
	m.releaseName("ts01-a", "bbbbbbbb")
	if err := m.reserveName("ts01-a", "bbbbbbbb"); !errors.Is(err, ErrConflict) {
		t.Error("a release by a VM that does not own the name freed it")
	}
	m.releaseName("ts01-a", "aaaaaaaa")
	if err := m.reserveName("ts01-a", "bbbbbbbb"); err != nil {
		t.Errorf("name not free after its owner released it: %v", err)
	}
}

// A failed create gives its name back: the retry under the same name must not
// hit a conflict with a VM that never existed.
func TestUndoCreateReleasesName(t *testing.T) {
	m := newTestManager(t)
	rec := &types.VM{Config: types.VMConfig{ID: "c0000001", Name: "ts01-a"}, State: types.VMStateCreating}
	if err := m.reserveName("ts01-a", "c0000001"); err != nil {
		t.Fatal(err)
	}
	m.undoCreate(rec)
	if err := m.reserveName("ts01-a", "c0000002"); err != nil {
		t.Errorf("name still taken after undoCreate: %v", err)
	}
}

// Reconcile rebuilds the name index from the store. Two records with one name
// (a store edited by hand) must not stop the daemon: the first keeps it.
func TestReconcileAdoptsNames(t *testing.T) {
	m := newTestManager(t)
	a := &types.VM{Config: types.VMConfig{ID: "a0000001", Name: "ts01-a"}, State: types.VMStateStopped, CreatedAt: time.Now()}
	b := &types.VM{Config: types.VMConfig{ID: "b0000001", Name: "ts01-a"}, State: types.VMStateStopped, CreatedAt: time.Now()}
	m.Reconcile([]*types.VM{a, b})

	if err := m.reserveName("ts01-a", "c0000001"); !errors.Is(err, ErrConflict) {
		t.Errorf("name of a reconciled VM is free: %v", err)
	}
	got, _ := m.Get("b0000001")
	if got.Config.Name != "" {
		t.Errorf("duplicate kept its name %q; the second record must lose it", got.Config.Name)
	}
	if got, _ := m.Get("a0000001"); got.Config.Name != "ts01-a" {
		t.Errorf("first record lost its name: %q", got.Config.Name)
	}
}

func TestSetLabelsMergePatch(t *testing.T) {
	m := newTestManager(t)
	rec := &types.VM{
		Config:    types.VMConfig{ID: "a0000001", Labels: map[string]string{"sensor": "ts-01", "role": "parser"}},
		State:     types.VMStateStopped,
		CreatedAt: time.Now(),
	}
	m.Reconcile([]*types.VM{rec})
	before, _ := m.Get("a0000001")

	s := func(v string) *string { return &v }
	got, err := m.SetLabels("a0000001", map[string]*string{"sensor": s("ts-02"), "role": nil, "owner": s("ot")})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"sensor": "ts-02", "owner": "ot"}
	if !reflect.DeepEqual(got.Config.Labels, want) {
		t.Errorf("labels = %v, want %v", got.Config.Labels, want)
	}
	// A copy handed out before the patch must not see it.
	if before.Config.Labels["sensor"] != "ts-01" || before.Config.Labels["role"] != "parser" {
		t.Errorf("an earlier copy changed under its reader: %v", before.Config.Labels)
	}

	// An invalid patch changes nothing.
	if _, err := m.SetLabels("a0000001", map[string]*string{"sensor": s("bad value")}); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid value = %v, want ErrInvalid", err)
	}
	if now, _ := m.Get("a0000001"); !reflect.DeepEqual(now.Config.Labels, want) {
		t.Errorf("a refused patch changed the labels: %v", now.Config.Labels)
	}

	// Removing the last label leaves none, not an empty map in the JSON.
	if got, err = m.SetLabels("a0000001", map[string]*string{"sensor": nil, "owner": nil}); err != nil || got.Config.Labels != nil {
		t.Errorf("removing every label: labels %v, err %v", got.Config.Labels, err)
	}

	if _, err := m.SetLabels("nope0000", nil); !errors.Is(err, ErrVMNotFound) {
		t.Errorf("unknown VM = %v, want ErrVMNotFound", err)
	}
}

func TestLabelSelector(t *testing.T) {
	sel, err := ParseLabelSelector([]string{"sensor=ts-01", "role=parser,owner=ot"})
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches(map[string]string{"sensor": "ts-01", "role": "parser", "owner": "ot", "extra": "x"}) {
		t.Error("a VM with every label (and more) must match")
	}
	if sel.Matches(map[string]string{"sensor": "ts-01", "role": "parser"}) {
		t.Error("a VM missing one label must not match")
	}
	if empty, _ := ParseLabelSelector(nil); !empty.Matches(nil) {
		t.Error("an empty selector selects everything")
	}
	for _, bad := range []string{"sensor", "Sensor=x", "sensor=a b"} {
		if _, err := ParseLabelSelector([]string{bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("selector %q = %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := ParseLabelSelector([]string{"sensor=a", "sensor=b"}); err == nil {
		t.Error("a selector asking one key for two values can match nothing; refuse it")
	}
}
