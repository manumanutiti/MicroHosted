package network

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"microhosted/internal/labels"
	"microhosted/pkg/types"
)

func strp(s string) *string { return &s }

// Labels given at create are kept in memory and in the store; a patch merges
// into them and is persisted before it is reported.
func TestNetworkLabels(t *testing.T) {
	withFakeHost(t)
	m, st := newTestNetManager(t)
	own := map[string]string{"managed-by": "mh-orchestrator", "function": "ts-01"}
	if _, err := m.Create(types.CreateNetworkRequest{Name: "ts-01", Labels: own}); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.Get("ts-01"); !reflect.DeepEqual(n.Labels, own) {
		t.Errorf("labels after create = %v, want %v", n.Labels, own)
	}

	n, err := m.SetLabels("ts-01", map[string]*string{"function": nil, "zone": strp("north")})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"managed-by": "mh-orchestrator", "zone": "north"}
	if !reflect.DeepEqual(n.Labels, want) {
		t.Errorf("labels after patch = %v, want %v", n.Labels, want)
	}
	recs, _ := st.ListNetworks()
	if len(recs) != 1 || !reflect.DeepEqual(recs[0].Labels, want) {
		t.Errorf("stored labels = %+v, want %v", recs, want)
	}
	if own["function"] != "ts-01" {
		t.Error("the patch wrote into the caller's map")
	}

	if _, err := m.SetLabels("ts-01", map[string]*string{"zone": strp("bad value")}); !errors.Is(err, labels.ErrInvalid) {
		t.Errorf("bad value = %v, want ErrInvalid", err)
	}
	if n, _ := m.Get("ts-01"); !reflect.DeepEqual(n.Labels, want) {
		t.Errorf("a refused patch changed the labels: %v", n.Labels)
	}
	if _, err := m.SetLabels("nope", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown network = %v, want ErrNotFound", err)
	}
}

// A bad name or label is refused before anything is created on the host.
func TestCreateNetworkRejectsBadIdentity(t *testing.T) {
	h := withFakeHost(t)
	m, _ := newTestNetManager(t)
	for _, req := range []types.CreateNetworkRequest{
		{Name: ""},
		{Name: "Lab"},
		{Name: "a/b"},
		{Name: "lab", Labels: map[string]string{"Bad": "x"}},
		{Name: "lab", Labels: map[string]string{"k": "has space"}},
	} {
		if _, err := m.Create(req); !errors.Is(err, labels.ErrInvalid) {
			t.Errorf("Create(%+v) = %v, want ErrInvalid", req, err)
		}
	}
	if len(h.bridges) != 0 || len(h.applied) != 0 {
		t.Errorf("a refused create touched the host: bridges %v, %d applies", h.bridges, len(h.applied))
	}
}

func TestNetworkErrorKinds(t *testing.T) {
	withFakeHost(t)
	m, _ := newTestNetManager(t)
	if _, err := m.Create(types.CreateNetworkRequest{Name: "lab"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(types.CreateNetworkRequest{Name: "lab"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate name = %v, want ErrExists", err)
	}
	if _, _, _, _, err := m.AttachVM("lab", "deadbeef", "tapdeadbeef"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("lab"); !errors.Is(err, ErrInUse) {
		t.Errorf("delete with a VM attached = %v, want ErrInUse", err)
	}
	if err := m.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete unknown = %v, want ErrNotFound", err)
	}
}

// A policy update works on a copy taken before its apply and installs it
// afterwards. A label or intra change landing during that apply used to be
// overwritten by the copy — in memory and in the store. Now it waits.
func TestRecordUpdateNotLostDuringPolicyApply(t *testing.T) {
	h := withFakeHost(t)
	m, st := newTestNetManager(t)
	if _, err := m.Create(types.CreateNetworkRequest{Name: "lab", Intra: true}); err != nil {
		t.Fatal(err)
	}

	inApply, release := make(chan struct{}), make(chan struct{})
	applyNftables = func(nets []types.Network, _ []ManagedIface) error {
		close(inApply)
		<-release
		h.applied = append(h.applied, nets)
		return nil
	}
	policyDone := make(chan error)
	go func() {
		_, err := m.UpdateEgress("lab", types.UpdateNetworkEgressRequest{Egress: true, EgressIface: "eth0"})
		policyDone <- err
	}()
	<-inApply

	recordDone := make(chan error, 2)
	go func() {
		_, err := m.UpdateIntra("lab", false)
		recordDone <- err
	}()
	go func() {
		_, err := m.SetLabels("lab", map[string]*string{"zone": strp("north")})
		recordDone <- err
	}()
	// Give both a chance to land inside the apply, as they did before
	// updateRecord took applyMu; now they wait for it.
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-policyDone; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-recordDone; err != nil {
			t.Fatal(err)
		}
	}

	n, _ := m.Get("lab")
	recs, _ := st.ListNetworks()
	for where, got := range map[string]*types.Network{"memory": n, "store": recs[0]} {
		if !got.Egress || got.Intra || got.Labels["zone"] != "north" {
			t.Errorf("%s: egress %v intra %v labels %v; want egress on, intra off, zone=north", where, got.Egress, got.Intra, got.Labels)
		}
	}
}
