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
	// Ingress legs carry conntrack matches (direction, DNAT status), but only
	// on top of their stateless tuple — never a `ct state` accept.
	nets := append(mqttNet(), types.Network{
		Name: "iot", Bridge: "mhbrcccc", Subnet: "172.18.0.0/24", AllowedEgress: []types.EgressRule{
			{IP: "203.0.113.7", Protocol: "tcp", Port: 8883},
		}})
	got := renderNftables(nets, managedWlan())

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
	back := mustIndex(t, got, `iifname "wlan0" oifname "mhbrcccc" ip saddr 192.168.50.52 ip daddr 172.16.9.0/24 tcp sport 502 ct direction reply accept`)
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

// TestRenderNftablesIfaceReturnLegIsReplyOnly: every device→guest accept of an
// egress rule must be restricted to replies. The stateless tuple alone lets the
// device open connections into the guest on any port by using the rule's port
// as its source port, or ping it through an icmp rule.
func TestRenderNftablesIfaceReturnLegIsReplyOnly(t *testing.T) {
	nets := []types.Network{{
		Name: "ot", Bridge: "mhbrcccc", Subnet: "172.16.9.0/24",
		AllowedEgress: []types.EgressRule{
			{Iface: "wlan0", IP: "192.168.50.52", Protocol: "tcp", Port: 502},
			{Iface: "wlan0", IP: "192.168.50.53", Protocol: "udp", Port: 47808},
			{Iface: "wlan0", IP: "192.168.50.0/24", Protocol: "icmp"},
		},
	}}
	got := renderNftables(nets, managedWlan())
	n := 0
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, `iifname "wlan0" oifname "mhbrcccc"`) {
			continue
		}
		n++
		if !strings.Contains(line, "ct direction reply accept") {
			t.Errorf("a device→guest accept is not restricted to replies: %q", line)
		}
	}
	if n != 3 {
		t.Fatalf("want 3 return legs, got %d:\n%s", n, got)
	}
}

// TestRenderNftablesWANRuleCannotReachManagedSegment: a rule WITHOUT an iface
// means the WAN. If its destination happens to sit on a managed segment, the
// WAN accept must not carry the packet out through that interface — the
// blanket drop has to come first. (Found on hardware: `icmp:192.168.50.52`
// without @wlan0 sent the echo request out wlan0; only the reply died.)
func TestRenderNftablesWANRuleCannotReachManagedSegment(t *testing.T) {
	nets := []types.Network{
		{Name: "ot", Bridge: "mhbrcccc", Subnet: "172.16.9.0/24", AllowedEgress: []types.EgressRule{
			{IP: "192.168.50.52", Protocol: "icmp"},
			{IP: "192.168.50.0/24", Protocol: "tcp", Port: 502},
		}},
		{Name: "build", Bridge: "mhbrbbbb", Subnet: "172.17.0.0/24", Egress: true},
	}
	got := renderNftables(nets, managedWlan())
	blanketOut := mustIndex(t, got, `oifname "wlan0" drop`)
	blanketIn := mustIndex(t, got, `iifname "wlan0" drop`+"\n\t\toifname")
	for _, wan := range []string{
		`iifname "mhbrcccc" oifname != @mhbridges ip daddr 192.168.50.52 meta l4proto icmp accept`,
		`iifname "mhbrcccc" oifname != @mhbridges ip daddr 192.168.50.0/24 tcp dport 502 accept`,
	} {
		if i := mustIndex(t, got, wan); i < blanketOut || i < blanketIn {
			t.Errorf("WAN accept %q comes before the managed interface's drops:\n%s", wan, got)
		}
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
		`iifname "wlan0" oifname "mhbrcccc" ip saddr 192.168.50.0/24 ip daddr 172.16.9.0/24 meta l4proto icmp ct direction reply accept`,
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

// mqttNet is the push-mode shape: one sensor behind wlan0 publishes to the
// host's port 1883 and lands on the network's broker VM.
func mqttNet() []types.Network {
	return []types.Network{{
		Name: "mqtt-60", Bridge: "mhbrdddd", Subnet: "172.16.9.0/24",
		AllowedIngress: []types.IngressRule{
			{Iface: "wlan0", SrcIP: "192.168.50.60", Protocol: "tcp", Port: 1883, ToIP: "172.16.9.2"},
		},
	}}
}

// TestRenderNftablesIngress: the DNAT and both forward legs are there, and the
// host itself never opens the port — the flow is routed to the guest, it never
// reaches `input`.
func TestRenderNftablesIngress(t *testing.T) {
	got := renderNftables(mqttNet(), managedWlan())

	pre := mustIndex(t, got, "chain prerouting {")
	mustIndex(t, got, "type nat hook prerouting priority -100; policy accept;")
	dnat := mustIndex(t, got, `iifname "wlan0" ip saddr 192.168.50.60 fib daddr type local tcp dport 1883 dnat ip to 172.16.9.2:1883`)
	if dnat < pre || dnat > mustIndex(t, got, "chain forward {") {
		t.Fatalf("the DNAT is not inside the prerouting chain:\n%s", got)
	}

	in := mustIndex(t, got, `iifname "wlan0" oifname "mhbrdddd" ip saddr 192.168.50.60 ip daddr 172.16.9.2 tcp dport 1883 ct status dnat ct direction original accept`)
	back := mustIndex(t, got, `iifname "mhbrdddd" oifname "wlan0" ip saddr 172.16.9.2 tcp sport 1883 ip daddr 192.168.50.60 ct status dnat ct direction reply accept`)
	egressDrop := mustIndex(t, got, `iifname "mhbrdddd" oifname != @mhbridges drop`)
	blanketIn := strings.LastIndex(got, `iifname "wlan0" drop`)
	blanketOut := strings.LastIndex(got, `oifname "wlan0" drop`)
	if back > egressDrop {
		t.Fatalf("the return leg is after the network's egress drop — replies never leave:\n%s", got)
	}
	if in > blanketIn || in > blanketOut || back > blanketIn || back > blanketOut {
		t.Fatalf("an ingress leg is after the managed interface's blanket drops:\n%s", got)
	}

	inputChain := got[mustIndex(t, got, "chain input"):pre]
	if strings.Contains(inputChain, "1883") {
		t.Fatalf("the host opened the ingress port itself:\n%s", inputChain)
	}
	// Ingress is not egress: the VM gains no outbound flow towards the device.
	if strings.Contains(got, `oifname "wlan0" ip daddr 192.168.50.60 tcp dport`) {
		t.Fatalf("an ingress rule leaked an outbound accept:\n%s", got)
	}
	// The DNATed flow keeps the device's real source (the VM sees who it is):
	// this network has no egress, so nothing masquerades it.
	if strings.Contains(got, "masquerade") {
		t.Fatalf("ingress alone must not masquerade:\n%s", got)
	}
}

// TestRenderNftablesIngressReturnLegIsReplyOnly: without the conntrack
// direction on the return leg, a compromised VM could bind the ingress port as
// its SOURCE port and open connections to any port on the device.
func TestRenderNftablesIngressReturnLegIsReplyOnly(t *testing.T) {
	got := renderNftables(mqttNet(), managedWlan())
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, `iifname "mhbrdddd" oifname "wlan0"`) && !strings.Contains(line, "ct direction reply") {
			t.Fatalf("a bridge→device accept is not restricted to replies: %q", line)
		}
	}
}

// TestRenderNftablesNoIngressNoPrerouting: networks without ingress rules get
// exactly the ruleset they had before the feature existed.
func TestRenderNftablesNoIngressNoPrerouting(t *testing.T) {
	nets := []types.Network{{Name: "lab", Bridge: "mhbraaaa", Subnet: "172.16.0.0/24"}}
	if got := renderNftables(nets, managedWlan()); strings.Contains(got, "prerouting") || strings.Contains(got, "dnat") {
		t.Fatalf("a prerouting chain appeared with no ingress rules:\n%s", got)
	}
}

// TestRenderNftablesSkipsIngressForUnmanagedIface: the restart-without-the-flag
// case again. A DNAT through an interface with no deny-both-ways around it is
// a hole with no policy, so none of the rule's three lines may appear.
func TestRenderNftablesSkipsIngressForUnmanagedIface(t *testing.T) {
	for name, managed := range map[string][]ManagedIface{
		"no managed interface":    nil,
		"a different one managed": {{Name: "eth1"}},
	} {
		got := renderNftables(mqttNet(), managed)
		if strings.Contains(got, "1883") || strings.Contains(got, "prerouting") {
			t.Errorf("%s: an ingress rule for unmanaged wlan0 was rendered:\n%s", name, got)
		}
	}
}

func TestValidateIngressRules(t *testing.T) {
	managed := []string{"wlan0"}
	ok := types.IngressRule{Iface: "wlan0", SrcIP: "192.168.50.60", Protocol: "tcp", Port: 1883, ToIP: "172.16.9.2"}
	valid := []types.IngressRule{
		ok,
		{Iface: "wlan0", SrcIP: "192.168.50.61", Protocol: "tcp", Port: 1883, ToIP: "172.16.9.3"},
		{Iface: "wlan0", SrcIP: "192.168.60.0/24", Protocol: "udp", Port: 5683, ToIP: "172.16.9.4"},
	}
	if err := ValidateIngressRules(valid, managed); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}

	with := func(f func(*types.IngressRule)) types.IngressRule { r := ok; f(&r); return r }
	bad := map[string]types.IngressRule{
		"no iface":             with(func(r *types.IngressRule) { r.Iface = "" }),
		"unmanaged iface":      with(func(r *types.IngressRule) { r.Iface = "eth0" }),
		"hostname source":      with(func(r *types.IngressRule) { r.SrcIP = "sensor.local" }),
		"source injection":     with(func(r *types.IngressRule) { r.SrcIP = "1.2.3.4 accept; ip saddr 0.0.0.0/0" }),
		"non-canonical source": with(func(r *types.IngressRule) { r.SrcIP = "192.168.50.60/24" }),
		"ipv6 source":          with(func(r *types.IngressRule) { r.SrcIP = "2001:db8::1" }),
		"cidr target":          with(func(r *types.IngressRule) { r.ToIP = "172.16.9.0/24" }),
		"target injection":     with(func(r *types.IngressRule) { r.ToIP = "172.16.9.2:22 accept" }),
		"icmp":                 with(func(r *types.IngressRule) { r.Protocol = "icmp"; r.Port = 0 }),
		"protocol case":        with(func(r *types.IngressRule) { r.Protocol = "TCP" }),
		"no port":              with(func(r *types.IngressRule) { r.Port = 0 }),
		"port out of range":    with(func(r *types.IngressRule) { r.Port = 70000 }),
	}
	for name, r := range bad {
		if err := ValidateIngressRules([]types.IngressRule{r}, managed); err == nil {
			t.Errorf("%s: rule %+v should have been rejected", name, r)
		}
	}
	if err := ValidateIngressRules([]types.IngressRule{ok}, nil); err == nil {
		t.Error("accepted an ingress rule on a daemon that manages no interface")
	}
	// Two DNATs one packet could match: the second would never be in force.
	clash := []types.IngressRule{ok, with(func(r *types.IngressRule) { r.SrcIP = "192.168.50.0/24"; r.ToIP = "172.16.9.3" })}
	if err := ValidateIngressRules(clash, managed); err == nil {
		t.Error("accepted two rules whose sources overlap on the same port")
	}
}

func TestIngressClash(t *testing.T) {
	base := types.IngressRule{Iface: "wlan0", SrcIP: "192.168.50.60", Protocol: "tcp", Port: 1883, ToIP: "172.16.9.2"}
	with := func(f func(*types.IngressRule)) types.IngressRule { r := base; f(&r); return r }
	cases := map[string]struct {
		other types.IngressRule
		want  bool
	}{
		"identical":         {base, true},
		"other target only": {with(func(r *types.IngressRule) { r.ToIP = "172.16.10.2" }), true},
		"range holds it":    {with(func(r *types.IngressRule) { r.SrcIP = "192.168.50.0/24" }), true},
		"other source":      {with(func(r *types.IngressRule) { r.SrcIP = "192.168.50.61" }), false},
		"disjoint range":    {with(func(r *types.IngressRule) { r.SrcIP = "192.168.51.0/24" }), false},
		"other port":        {with(func(r *types.IngressRule) { r.Port = 8883 }), false},
		"other protocol":    {with(func(r *types.IngressRule) { r.Protocol = "udp" }), false},
		"other interface":   {with(func(r *types.IngressRule) { r.Iface = "eth1" }), false},
	}
	for name, c := range cases {
		if got := IngressClash(base, c.other); got != c.want {
			t.Errorf("%s: IngressClash = %v, want %v", name, got, c.want)
		}
		if got := IngressClash(c.other, base); got != c.want {
			t.Errorf("%s (swapped): IngressClash = %v, want %v", name, got, c.want)
		}
	}
}
