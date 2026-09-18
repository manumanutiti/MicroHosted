package network

import (
	"reflect"
	"testing"

	"microhosted/pkg/types"
)

// TestUnenforcedRules: the list the egress_policy health check and the startup
// warning are built from. Only interface-scoped rules for an interface the
// daemon does not manage belong in it — WAN rules and rules on a managed
// interface are enforced.
func TestUnenforcedRules(t *testing.T) {
	m := NewManager(nil, managedWlan())
	m.nets["ot"] = &managedNet{net: &types.Network{Name: "ot", AllowedEgress: []types.EgressRule{
		{Iface: "wlan0", IP: "192.168.50.52", Protocol: "tcp", Port: 502},
		{Iface: "eth1", IP: "10.0.0.0/24", Protocol: "icmp"},
		{IP: "203.0.113.7", Protocol: "tcp", Port: 8883},
	}}}
	m.nets["lab"] = &managedNet{net: &types.Network{Name: "lab"}}

	want := []string{"network ot: icmp:10.0.0.0/24@eth1"}
	if got := m.UnenforcedRules(); !reflect.DeepEqual(got, want) {
		t.Errorf("UnenforcedRules() = %q, want %q", got, want)
	}

	// Restarted with no --managed-iface at all: the wlan0 rule joins the list.
	m.managed = nil
	want = []string{"network ot: icmp:10.0.0.0/24@eth1", "network ot: tcp:192.168.50.52:502@wlan0"}
	if got := m.UnenforcedRules(); !reflect.DeepEqual(got, want) {
		t.Errorf("with nothing managed: %q, want %q", got, want)
	}
}
