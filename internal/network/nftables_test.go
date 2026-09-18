package network

import (
	"fmt"
	"strings"
	"testing"

	"microhosted/pkg/types"
)

func TestRenderNftablesEmpty(t *testing.T) {
	// No networks → the table is deleted, not left with dangling rules.
	got := renderNftables(nil, nil)
	if !strings.Contains(got, "delete table inet microhosted") {
		t.Fatalf("empty render should delete the table, got:\n%s", got)
	}
	if strings.Contains(got, "chain forward") {
		t.Fatalf("empty render should not define chains, got:\n%s", got)
	}
}

func TestRenderNftablesIsolationAndEgress(t *testing.T) {
	nets := []types.Network{
		{Name: "lab", Bridge: "mhbraaaa", Subnet: "172.16.0.0/24", Egress: false},
		{Name: "build", Bridge: "mhbrbbbb", Subnet: "172.17.0.0/24", Egress: true},
	}
	got := renderNftables(nets, nil)

	// guest → host is always dropped.
	if !strings.Contains(got, "iifname @mhbridges drop") {
		t.Fatalf("missing guest→host drop:\n%s", got)
	}
	// Cross-segment: one aggregate drop covers every bridge pair, with the
	// same-bridge pairs exempted via @mhsame (br_netfilter safety).
	if !strings.Contains(got, "iifname @mhbridges oifname @mhbridges iifname . oifname != @mhsame drop") {
		t.Fatalf("missing aggregate cross-segment drop:\n%s", got)
	}
	for _, want := range []string{
		`"mhbraaaa" . "mhbraaaa"`,
		`"mhbrbbbb" . "mhbrbbbb"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing same-bridge pair %q in @mhsame:\n%s", want, got)
		}
	}
	// No-egress network blocks traffic to the WAN...
	if !strings.Contains(got, `iifname "mhbraaaa" oifname != @mhbridges drop`) {
		t.Fatalf("no-egress network should block WAN egress:\n%s", got)
	}
	// ...while the egress network does NOT get that drop and DOES get NAT.
	if strings.Contains(got, `iifname "mhbrbbbb" oifname != @mhbridges drop`) {
		t.Fatalf("egress network must not have its WAN egress dropped:\n%s", got)
	}
	if !strings.Contains(got, "ip saddr 172.17.0.0/24 oifname != @mhbridges masquerade") {
		t.Fatalf("egress network should be masqueraded:\n%s", got)
	}
	// The no-egress subnet must never be masqueraded.
	if strings.Contains(got, "172.16.0.0/24 oifname != @mhbridges masquerade") {
		t.Fatalf("no-egress subnet must not be masqueraded:\n%s", got)
	}
}

func TestRenderNftablesAllowedEgress(t *testing.T) {
	nets := []types.Network{
		{Name: "iot", Bridge: "mhbrcccc", Subnet: "172.18.0.0/24", AllowedEgress: []types.EgressRule{
			{IP: "203.0.113.7", Protocol: "tcp", Port: 8883},
			{IP: "198.51.100.0/28", Protocol: "udp", Port: 123},
			{IP: "203.0.113.7", Protocol: "icmp"},
		}},
	}
	got := renderNftables(nets, nil)

	// Each allowed flow gets an accept scoped to the bridge and the WAN.
	for _, want := range []string{
		`iifname "mhbrcccc" oifname != @mhbridges ip daddr 203.0.113.7 tcp dport 8883 accept`,
		`iifname "mhbrcccc" oifname != @mhbridges ip daddr 198.51.100.0/28 udp dport 123 accept`,
		`iifname "mhbrcccc" oifname != @mhbridges ip daddr 203.0.113.7 meta l4proto icmp accept`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing allowed-egress accept %q:\n%s", want, got)
		}
	}
	// The catch-all WAN drop is still there, and AFTER the accepts.
	drop := `iifname "mhbrcccc" oifname != @mhbridges drop`
	if !strings.Contains(got, drop) {
		t.Fatalf("restricted network must keep its WAN drop:\n%s", got)
	}
	if strings.Index(got, drop) < strings.Index(got, "dport 8883 accept") {
		t.Fatalf("WAN drop must come after the allowed-egress accepts:\n%s", got)
	}
	// Allowed flows need NAT: the subnet is masqueraded.
	if !strings.Contains(got, "ip saddr 172.18.0.0/24 oifname != @mhbridges masquerade") {
		t.Fatalf("restricted network's subnet should be masqueraded:\n%s", got)
	}
}

func TestRenderNftablesScalesLinearly(t *testing.T) {
	// Cross-segment isolation must stay a single aggregate rule no matter how
	// many networks exist (the network-per-VM topology means one network per
	// device, so N gets big). A per-pair regression would mean O(N²) rules.
	nets := make([]types.Network, 150)
	for i := range nets {
		nets[i] = types.Network{
			Name:   fmt.Sprintf("n%d", i),
			Bridge: fmt.Sprintf("mhbr%04x", i),
			Subnet: fmt.Sprintf("172.16.%d.0/24", i%256),
		}
	}
	got := renderNftables(nets, nil)

	if n := strings.Count(got, "oifname @mhbridges iifname . oifname != @mhsame drop"); n != 1 {
		t.Fatalf("want exactly 1 aggregate cross-segment drop, got %d", n)
	}
	// Nothing else should scale per-pair: total drop rules stay O(N).
	if n := strings.Count(got, "drop"); n > len(nets)+5 {
		t.Fatalf("drop rule count %d suggests per-pair rendering came back", n)
	}
}

func TestForwardChainMatchesStatelessly(t *testing.T) {
	// The forward chain must NOT blanket-accept established flows: that would
	// let a connection opened under a looser policy survive an UpdateEgress
	// tightening (stale conntrack entry riding past the new drop). The input
	// chain keeps its established accept (host→guest replies need it).
	nets := []types.Network{
		{Name: "iot", Bridge: "mhbrcccc", Subnet: "172.18.0.0/24", AllowedEgress: []types.EgressRule{
			{IP: "203.0.113.7", Protocol: "tcp", Port: 8883},
		}},
	}
	got := renderNftables(nets, nil)

	fwdStart := strings.Index(got, "chain forward {")
	fwdEnd := strings.Index(got[fwdStart:], "}")
	forward := got[fwdStart : fwdStart+fwdEnd]
	if strings.Contains(forward, "ct state") {
		t.Fatalf("forward chain must not have ct-state accepts:\n%s", forward)
	}

	inStart := strings.Index(got, "chain input {")
	inEnd := strings.Index(got[inStart:], "}")
	input := got[inStart : inStart+inEnd]
	if !strings.Contains(input, "ct state established,related accept") {
		t.Fatalf("input chain must keep its established accept:\n%s", input)
	}
}

func TestValidateEgressRules(t *testing.T) {
	valid := []types.EgressRule{
		{IP: "203.0.113.7", Protocol: "tcp", Port: 8883},
		{IP: "198.51.100.0/28", Protocol: "udp", Port: 123},
		{IP: "203.0.113.7", Protocol: "icmp"},
	}
	if err := ValidateEgressRules(valid, nil); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}

	bad := map[string]types.EgressRule{
		"hostname":           {IP: "evil.example.com", Protocol: "tcp", Port: 80},
		"script injection":   {IP: "1.2.3.4 accept; ip daddr 0.0.0.0/0", Protocol: "tcp", Port: 80},
		"ipv6":               {IP: "2001:db8::1", Protocol: "tcp", Port: 80},
		"non-canonical cidr": {IP: "1.2.3.4/24", Protocol: "tcp", Port: 80},
		"unknown protocol":   {IP: "1.2.3.4", Protocol: "gre"},
		"protocol case":      {IP: "1.2.3.4", Protocol: "TCP", Port: 80},
		"tcp without port":   {IP: "1.2.3.4", Protocol: "tcp"},
		"port out of range":  {IP: "1.2.3.4", Protocol: "udp", Port: 70000},
		"icmp with port":     {IP: "1.2.3.4", Protocol: "icmp", Port: 80},
	}
	for name, r := range bad {
		if err := ValidateEgressRules([]types.EgressRule{r}, nil); err == nil {
			t.Errorf("%s: rule %+v should have been rejected", name, r)
		}
	}
}

// wlan0 stands in for any managed interface; nothing in the engine knows or
// cares what kind of link it is.
func managedWlan() []ManagedIface {
	return []ManagedIface{{Name: "wlan0", HostAllow: []HostService{{Protocol: "udp", Port: 67}}}}
}

// mustIndex returns where needle appears, failing the test if it doesn't. Rule
// ORDER is the whole correctness argument in a chain where the first match
// wins, so the tests below assert positions, not just presence.
func mustIndex(t *testing.T, script, needle string) int {
	t.Helper()
	i := strings.Index(script, needle)
	if i < 0 {
		t.Fatalf("missing rule %q in:\n%s", needle, script)
	}
	return i
}

// TestRenderNftablesManagedIfaceDeniedByDefault: declaring an interface hands
// the daemon its whole policy. With no network claiming anything on it, nothing
// crosses it in either direction and nothing reaches the host through it beyond
// the services the operator listed.
func TestRenderNftablesManagedIfaceDeniedByDefault(t *testing.T) {
	nets := []types.Network{{Name: "lab", Bridge: "mhbraaaa", Subnet: "172.16.0.0/24"}}
	got := renderNftables(nets, managedWlan())

	for _, want := range []string{
		`iifname "wlan0" udp dport 67 accept`,
		`iifname "wlan0" drop`,
		`oifname "wlan0" drop`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	// A host service has to precede the input drop or it is never reachable.
	inputChain := got[mustIndex(t, got, "chain input"):mustIndex(t, got, "chain forward")]
	if strings.Index(inputChain, "udp dport 67 accept") > strings.Index(inputChain, `iifname "wlan0" drop`) {
		t.Fatalf("the host-service accept comes after the input drop:\n%s", inputChain)
	}
	if strings.Contains(got, `oifname "wlan0" ip daddr`) {
		t.Fatalf("an accept appeared for a network that declared none:\n%s", got)
	}
}

// TestRenderNftablesManagedHostAllowIsNotAssumed: an operator whose segment is
// addressed by something else denies every host service, DHCP included.
func TestRenderNftablesManagedHostAllowIsNotAssumed(t *testing.T) {
	got := renderNftables(nil, []ManagedIface{{Name: "eth1"}})
	if strings.Contains(got, "dport 67") {
		t.Fatalf("DHCP was opened on an interface that asked for no host service:\n%s", got)
	}
}

// TestRenderNftablesIfaceRuleOrdering is the core correctness test: both legs of
// an interface-scoped rule must be accepted BEFORE every drop that would
// otherwise catch them — the network's own egress drop (a managed interface is
// not one of our bridges) and the blanket drops that close the chain.
func TestRenderNftablesIfaceRuleOrdering(t *testing.T) {
	nets := []types.Network{{
		Name: "ot", Bridge: "mhbrcccc", Subnet: "172.16.9.0/24",
		AllowedEgress: []types.EgressRule{
			{Iface: "wlan0", IP: "192.168.50.52", Protocol: "tcp", Port: 502},
		},
	}}
	got := renderNftables(nets, managedWlan())

	out := mustIndex(t, got, `iifname "mhbrcccc" oifname "wlan0" ip daddr 192.168.50.52 tcp dport 502 accept`)
	back := mustIndex(t, got, `iifname "wlan0" oifname "mhbrcccc" ip saddr 192.168.50.52 ip daddr 172.16.9.0/24 tcp sport 502 accept`)
	egressDrop := mustIndex(t, got, `iifname "mhbrcccc" oifname != @mhbridges drop`)
	blanketIn := strings.LastIndex(got, `iifname "wlan0" drop`)
	blanketOut := strings.LastIndex(got, `oifname "wlan0" drop`)

	if out > egressDrop {
		t.Fatalf("the outbound accept is after the network's egress drop — it never leaves:\n%s", got)
	}
	if out > blanketIn || out > blanketOut || back > blanketIn || back > blanketOut {
		t.Fatalf("an interface-scoped accept is after the blanket drops:\n%s", got)
	}
	// An interface-scoped rule must NOT also be emitted as a WAN rule, or the
	// flow would be allowed towards anything that is not one of our bridges.
	if strings.Contains(got, `oifname != @mhbridges ip daddr 192.168.50.52`) {
		t.Fatalf("an interface-scoped rule leaked into the WAN rules:\n%s", got)
	}
}

// TestRenderNftablesWholeRangeOnManagedIface: a range is a legitimate
// destination — "this network polls that whole segment" is a real deployment,
// and the engine must be able to say it.
func TestRenderNftablesWholeRangeOnManagedIface(t *testing.T) {
	nets := []types.Network{{
		Name: "ot", Bridge: "mhbrcccc", Subnet: "172.16.9.0/24",
		AllowedEgress: []types.EgressRule{
			{Iface: "wlan0", IP: "192.168.50.0/24", Protocol: "icmp"},
		},
	}}
	got := renderNftables(nets, managedWlan())
	for _, want := range []string{
		`iifname "mhbrcccc" oifname "wlan0" ip daddr 192.168.50.0/24 meta l4proto icmp accept`,
		`iifname "wlan0" oifname "mhbrcccc" ip saddr 192.168.50.0/24 ip daddr 172.16.9.0/24 meta l4proto icmp accept`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

// TestRenderNftablesMixedDestinations: one list, two kinds of destination — the
// WAN broker keeps its old rule, the local device gets an interface-scoped pair.
func TestRenderNftablesMixedDestinations(t *testing.T) {
	nets := []types.Network{{
		Name: "ot", Bridge: "mhbrcccc", Subnet: "172.16.9.0/24",
		AllowedEgress: []types.EgressRule{
			{Iface: "wlan0", IP: "192.168.50.52", Protocol: "tcp", Port: 502},
			{IP: "203.0.113.7", Protocol: "tcp", Port: 8883},
		},
	}}
	got := renderNftables(nets, managedWlan())

	if !strings.Contains(got, `iifname "mhbrcccc" oifname != @mhbridges ip daddr 203.0.113.7 tcp dport 8883 accept`) {
		t.Fatalf("the WAN rule lost its old shape:\n%s", got)
	}
	if !strings.Contains(got, `iifname "mhbrcccc" oifname "wlan0" ip daddr 192.168.50.52 tcp dport 502 accept`) {
		t.Fatalf("the interface-scoped rule is missing:\n%s", got)
	}
	// One masquerade covers both: a managed interface is not one of our bridges.
	if n := strings.Count(got, "ip saddr 172.16.9.0/24"); n != 1 {
		t.Fatalf("expected exactly one masquerade for the subnet, got %d:\n%s", n, got)
	}
}

// TestRenderNftablesSkipsRulesForUnmanagedIface is the restart-without-the-flag
// case: a stored rule names wlan0, but the daemon came back without
// --managed-iface wlan0. Rendering its accept would open a hole with no
// deny-both-ways policy around it — every egress network could reach that
// segment and nothing would stop it coming back in — so it must not appear,
// while the rest of the network's policy still does.
func TestRenderNftablesSkipsRulesForUnmanagedIface(t *testing.T) {
	nets := []types.Network{{
		Name: "ot", Bridge: "mhbrcccc", Subnet: "172.16.9.0/24",
		AllowedEgress: []types.EgressRule{
			{Iface: "wlan0", IP: "192.168.50.52", Protocol: "tcp", Port: 502},
			{IP: "203.0.113.7", Protocol: "tcp", Port: 8883},
		},
	}}
	for name, managed := range map[string][]ManagedIface{
		"no managed interface":    nil,
		"a different one managed": {{Name: "eth1"}},
	} {
		got := renderNftables(nets, managed)
		if strings.Contains(got, `"wlan0"`) {
			t.Errorf("%s: a rule for unmanaged wlan0 was rendered:\n%s", name, got)
		}
		if !strings.Contains(got, `iifname "mhbrcccc" oifname != @mhbridges ip daddr 203.0.113.7 tcp dport 8883 accept`) {
			t.Errorf("%s: the network's WAN rule went missing too:\n%s", name, got)
		}
		if !strings.Contains(got, `iifname "mhbrcccc" oifname != @mhbridges drop`) {
			t.Errorf("%s: the network's egress drop went missing:\n%s", name, got)
		}
	}
}

// TestRenderNftablesNoManagedIfaceNoPolicy: an operator who declared nothing
// gets exactly the ruleset they had before this feature existed.
func TestRenderNftablesNoManagedIfaceNoPolicy(t *testing.T) {
	nets := []types.Network{{Name: "lab", Bridge: "mhbraaaa", Subnet: "172.16.0.0/24"}}
	if strings.Contains(renderNftables(nets, nil), "wlan0") {
		t.Fatal("rules rendered for an interface nobody declared")
	}
}

func TestValidateEgressRulesIface(t *testing.T) {
	managed := []string{"wlan0"}
	ok := []types.EgressRule{{Iface: "wlan0", IP: "192.168.50.0/24", Protocol: "tcp", Port: 502}}
	if err := ValidateEgressRules(ok, managed); err != nil {
		t.Fatalf("rejected a valid interface-scoped rule: %v", err)
	}
	// An interface the daemon was not told to manage has no deny-by-default
	// around it, so a hole through it would be a hole with no policy.
	bad := []types.EgressRule{{Iface: "eth0", IP: "192.168.0.15", Protocol: "tcp", Port: 502}}
	if err := ValidateEgressRules(bad, managed); err == nil {
		t.Fatal("accepted a rule naming an unmanaged interface")
	}
	if err := ValidateEgressRules(ok, nil); err == nil {
		t.Fatal("accepted an interface-scoped rule on a daemon that manages none")
	}
	// Without an iface the rule is a plain WAN rule and needs no declaration.
	wan := []types.EgressRule{{IP: "203.0.113.7", Protocol: "tcp", Port: 8883}}
	if err := ValidateEgressRules(wan, nil); err != nil {
		t.Fatalf("a WAN rule must not require a managed interface: %v", err)
	}
}

func TestValidateIfaceName(t *testing.T) {
	for _, ok := range []string{"wlan0", "eth0", "br-lan", "en_p1", "veth.1"} {
		if err := ValidateIfaceName(ok); err != nil {
			t.Errorf("rejected valid interface %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "this-name-is-far-too-long", "wlan0\naccept", "wlan 0", `wlan0"`} {
		if err := ValidateIfaceName(bad); err == nil {
			t.Errorf("accepted invalid interface %q", bad)
		}
	}
}
