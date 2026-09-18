package network

import (
	"errors"
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

// Ingress rules follow the same rule: listed only when their interface is not
// managed, because then they are not rendered at all.
func TestUnenforcedRulesIngress(t *testing.T) {
	m := NewManager(nil, managedWlan())
	m.nets["mqtt"] = &managedNet{net: &types.Network{Name: "mqtt", AllowedIngress: []types.IngressRule{
		{Iface: "wlan0", SrcIP: "192.168.50.60", Protocol: "tcp", Port: 1883, ToIP: "172.16.9.2"},
	}}}
	if got := m.UnenforcedRules(); len(got) != 0 {
		t.Errorf("a rule on a managed interface was reported unenforced: %q", got)
	}
	m.managed = nil
	want := []string{"network mqtt: ingress tcp:192.168.50.60:1883@wlan0=172.16.9.2"}
	if got := m.UnenforcedRules(); !reflect.DeepEqual(got, want) {
		t.Errorf("UnenforcedRules() = %q, want %q", got, want)
	}
}

// checkIngress is where what only the Manager knows is enforced: the target
// network's subnet and every other network's rules (prerouting is shared).
func TestCheckIngress(t *testing.T) {
	m := NewManager(nil, managedWlan())
	own, _ := ParseSubnet("172.16.9.0/24")
	other, _ := ParseSubnet("172.16.10.0/24")
	sensor60 := types.IngressRule{Iface: "wlan0", SrcIP: "192.168.50.60", Protocol: "tcp", Port: 1883, ToIP: "172.16.10.2"}
	m.nets["mqtt-60"] = &managedNet{net: &types.Network{Name: "mqtt-60", AllowedIngress: []types.IngressRule{sensor60}}, subnet: other}
	m.nets["mqtt-61"] = &managedNet{net: &types.Network{Name: "mqtt-61"}, subnet: own}

	rule := func(src, to string) []types.IngressRule {
		return []types.IngressRule{{Iface: "wlan0", SrcIP: src, Protocol: "tcp", Port: 1883, ToIP: to}}
	}
	if err := m.checkIngress("mqtt-61", own, rule("192.168.50.61", "172.16.9.2")); err != nil {
		t.Fatalf("a valid rule was rejected: %v", err)
	}
	for name, rules := range map[string][]types.IngressRule{
		"to_ip in another subnet":  rule("192.168.50.61", "172.16.10.3"),
		"to_ip is the gateway":     rule("192.168.50.61", "172.16.9.1"),
		"to_ip is the broadcast":   rule("192.168.50.61", "172.16.9.255"),
		"clash with mqtt-60":       rule("192.168.50.60", "172.16.9.2"),
		"range swallowing mqtt-60": rule("192.168.50.0/24", "172.16.9.2"),
	} {
		err := m.checkIngress("mqtt-61", own, rules)
		if !errors.Is(err, ErrInvalidIngress) {
			t.Errorf("%s: got %v, want an ErrInvalidIngress", name, err)
		}
	}
	// Replacing a network's own rules must not clash with the old copy of them.
	if err := m.checkIngress("mqtt-60", other, []types.IngressRule{sensor60}); err != nil {
		t.Errorf("a network's rule clashed with itself: %v", err)
	}
}
