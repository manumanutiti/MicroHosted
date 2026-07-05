package network

import (
	"fmt"
	"strings"
	"testing"

	"microhosted/pkg/types"
)

func TestRenderNftablesEmpty(t *testing.T) {
	// No networks → the table is deleted, not left with dangling rules.
	got := renderNftables(nil)
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
	got := renderNftables(nets)

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
	got := renderNftables(nets)

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
	// many networks exist (the red-por-VM topology means one network per
	// device, so N gets big). A per-pair regression would mean O(N²) rules.
	nets := make([]types.Network, 150)
	for i := range nets {
		nets[i] = types.Network{
			Name:   fmt.Sprintf("n%d", i),
			Bridge: fmt.Sprintf("mhbr%04x", i),
			Subnet: fmt.Sprintf("172.16.%d.0/24", i%256),
		}
	}
	got := renderNftables(nets)

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
	got := renderNftables(nets)

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
	if err := ValidateEgressRules(valid); err != nil {
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
		if err := ValidateEgressRules([]types.EgressRule{r}); err == nil {
			t.Errorf("%s: rule %+v should have been rejected", name, r)
		}
	}
}
