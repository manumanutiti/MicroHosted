package network

import (
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
	// Cross-segment: both directions between the two bridges dropped.
	for _, want := range []string{
		`iifname "mhbraaaa" oifname "mhbrbbbb" drop`,
		`iifname "mhbrbbbb" oifname "mhbraaaa" drop`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing cross-segment rule %q:\n%s", want, got)
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
