package network

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"microhosted/internal/store"
	"microhosted/pkg/types"
)

// fakeHost replaces the kernel-facing operations for one test: bridges live in
// a map, and every apply records the networks it was handed. failApply makes
// the next applies fail.
type fakeHost struct {
	mu        sync.Mutex
	bridges   map[string]bool
	applied   [][]types.Network
	failApply bool
}

func withFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{bridges: make(map[string]bool)}
	oldCreate, oldDelete, oldApply, oldFwd := createBridge, deleteBridge, applyNftables, ensureForwarding
	ensureForwarding = func() {}
	createBridge = func(name, _ string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.bridges[name] = true
		return nil
	}
	deleteBridge = func(name string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.bridges, name)
		return nil
	}
	applyNftables = func(nets []types.Network, _ []ManagedIface) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.failApply {
			return errors.New("nft: simulated failure")
		}
		h.applied = append(h.applied, nets)
		return nil
	}
	t.Cleanup(func() {
		createBridge, deleteBridge, applyNftables, ensureForwarding = oldCreate, oldDelete, oldApply, oldFwd
	})
	return h
}

func newTestNetManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "net.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewManager(st, managedWlan()), st
}

// If the ruleset cannot be applied, the network must not exist afterwards in
// any form: not in memory, not in the store, no bridge, subnet free again.
// Before, it stayed persisted with its bridge up and outside the policy.
func TestCreateRollsBackWhenApplyFails(t *testing.T) {
	h := withFakeHost(t)
	m, st := newTestNetManager(t)
	h.failApply = true

	if _, err := m.Create(types.CreateNetworkRequest{Name: "lab", Subnet: "10.9.0.0/24"}); err == nil {
		t.Fatal("Create succeeded although the ruleset could not be applied")
	}
	if _, ok := m.Get("lab"); ok {
		t.Error("network still listed after a failed apply")
	}
	if recs, _ := st.ListNetworks(); len(recs) != 0 {
		t.Errorf("network persisted after a failed apply: %+v", recs)
	}
	if len(h.bridges) != 0 {
		t.Errorf("bridge left behind: %v", h.bridges)
	}
	if _, _, _, _, err := m.AttachVM("lab", "deadbeef"); err == nil {
		t.Error("a VM could attach to a network whose rules never applied")
	}

	// The same name and subnet are free again.
	h.failApply = false
	if _, err := m.Create(types.CreateNetworkRequest{Name: "lab", Subnet: "10.9.0.0/24"}); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}

// A network is not attachable until its rules are in force: the ruleset that
// was being installed while it was pending must already list its bridge.
func TestCreateInstallsRulesBeforeAttachable(t *testing.T) {
	h := withFakeHost(t)
	m, _ := newTestNetManager(t)

	var sawPendingAttach bool
	applyNftables = func(nets []types.Network, _ []ManagedIface) error {
		if _, _, _, _, err := m.AttachVM("lab", "deadbeef"); err == nil {
			sawPendingAttach = true
		}
		h.applied = append(h.applied, nets)
		return nil
	}
	n, err := m.Create(types.CreateNetworkRequest{Name: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	if sawPendingAttach {
		t.Error("a VM attached to the network before its rules were applied")
	}
	last := h.applied[len(h.applied)-1]
	if len(last) != 1 || last[0].Bridge != n.Bridge {
		t.Errorf("the applied ruleset does not list the new bridge: %+v", last)
	}
	if _, _, _, _, err := m.AttachVM("lab", "deadbeef"); err != nil {
		t.Errorf("attach after create: %v", err)
	}
}

// A policy update that cannot be applied must leave the old policy both in
// memory and in the store: the API reports what is in force.
func TestUpdateEgressNotPersistedWhenApplyFails(t *testing.T) {
	h := withFakeHost(t)
	m, st := newTestNetManager(t)
	if _, err := m.Create(types.CreateNetworkRequest{Name: "lab"}); err != nil {
		t.Fatal(err)
	}

	h.failApply = true
	_, err := m.UpdateEgress("lab", types.UpdateNetworkEgressRequest{Egress: true, EgressIface: "eth0"})
	if err == nil {
		t.Fatal("UpdateEgress succeeded although the ruleset could not be applied")
	}
	if n, _ := m.Get("lab"); n.Egress {
		t.Error("in-memory policy changed although it is not in force")
	}
	recs, _ := st.ListNetworks()
	if len(recs) != 1 || recs[0].Egress {
		t.Errorf("stored policy changed although it is not in force: %+v", recs)
	}
	if st := m.Rules(); st.OK || !strings.Contains(st.Err, "simulated") {
		t.Errorf("Rules() = %+v, want the failed apply reported", st)
	}
}

// Concurrent creates each install a ruleset; the one installed last must list
// every network, or a network created in between would be missing from the
// policy in force (and dark, or worse, before the catch-all existed).
func TestConcurrentCreatesInstallNewestRuleset(t *testing.T) {
	h := withFakeHost(t)
	m, _ := newTestNetManager(t)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Create(types.CreateNetworkRequest{Name: "n" + string(rune('a'+i))}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	last := h.applied[len(h.applied)-1]
	if len(last) != 20 {
		t.Errorf("last ruleset installed lists %d networks, want 20", len(last))
	}
}

func TestCreateRejectsOverlappingSubnet(t *testing.T) {
	withFakeHost(t)
	m, _ := newTestNetManager(t)
	if _, err := m.Create(types.CreateNetworkRequest{Name: "a", Subnet: "10.9.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(types.CreateNetworkRequest{Name: "b", Subnet: "10.9.3.0/24"}); err == nil {
		t.Error("an overlapping subnet was accepted")
	}
	// The refused request must not have freed the subnet of the network it
	// collided with: the pool would then hand it out again.
	if _, err := m.Create(types.CreateNetworkRequest{Name: "c", Subnet: "172.16.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(types.CreateNetworkRequest{Name: "d", Subnet: "172.16.0.0/24"}); err == nil {
		t.Fatal("a duplicate subnet was accepted")
	}
	if n, err := m.Create(types.CreateNetworkRequest{Name: "e"}); err != nil || n.Subnet == "172.16.0.0/24" {
		t.Errorf("auto-allocation handed out a subnet in use: %+v, %v", n, err)
	}
}

// Deleting a network gives its auto-allocated subnet back.
func TestDeleteReleasesSubnet(t *testing.T) {
	withFakeHost(t)
	m, _ := newTestNetManager(t)
	a, _ := m.Create(types.CreateNetworkRequest{Name: "a"})
	if err := m.Delete("a"); err != nil {
		t.Fatal(err)
	}
	b, _ := m.Create(types.CreateNetworkRequest{Name: "b"})
	if a.Subnet != b.Subnet {
		t.Errorf("subnet %s not reused after delete (got %s)", a.Subnet, b.Subnet)
	}
}

func TestParseDefaultRoute(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"wlan0\t00000000\t0100A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0\t0000A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n"
	if got, err := parseDefaultRoute(table); err != nil || got != "eth0" {
		t.Errorf("parseDefaultRoute = %q, %v; want eth0 (lowest metric)", got, err)
	}
	if _, err := parseDefaultRoute("Iface\tDestination\n"); err == nil {
		t.Error("no default route must be an error")
	}
}

func TestParseLinkNames(t *testing.T) {
	out := "1: lo: <LOOPBACK> mtu 65536\n" +
		"5: mhbr41d35718: <BROADCAST> mtu 1500\n" +
		"9: tapdeadbeef@mhbr41d35718: <BROADCAST> mtu 1500\n"
	if got := parseLinkNames(out, "mhbr"); len(got) != 1 || got[0] != "mhbr41d35718" {
		t.Errorf("bridges = %v", got)
	}
	if got := parseLinkNames(out, "tap"); len(got) != 1 || got[0] != "tapdeadbeef" {
		t.Errorf("taps = %v", got)
	}
}

// The rendered rulesets must parse. nft only checks syntax with CAP_NET_ADMIN,
// so this runs as root only: sudo go test ./internal/network -run Parses
func TestRenderedRulesetParses(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for nft -c")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	cases := map[string][]types.Network{
		"empty": nil,
		"mixed": {
			{Name: "lab", Bridge: "mhbraaaa", Subnet: "172.16.0.0/24"},
			{Name: "build", Bridge: "mhbrbbbb", Subnet: "172.17.0.0/24", Egress: true, EgressIface: "eth0"},
			{Name: "old", Bridge: "mhbrdddd", Subnet: "172.18.0.0/24", Egress: true},
			{Name: "ot", Bridge: "mhbrcccc", Subnet: "172.16.9.0/24", AllowedEgress: []types.EgressRule{
				{Iface: "wlan0", IP: "192.168.50.52", Protocol: "tcp", Port: 502},
				{IP: "203.0.113.7", Protocol: "icmp"},
			}, AllowedIngress: []types.IngressRule{
				{Iface: "wlan0", SrcIP: "192.168.50.60", Protocol: "tcp", Port: 1883, ToIP: "172.16.9.2"},
			}},
		},
	}
	for name, nets := range cases {
		cmd := exec.Command("nft", "-c", "-f", "-")
		cmd.Stdin = strings.NewReader(renderNftables(nets, managedWlan()))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: nft -c rejected the ruleset: %v\n%s", name, err, out)
		}
	}
}
