package network

import "testing"

func TestSubnetGatewayAndCIDR(t *testing.T) {
	s, err := ParseSubnet("172.16.0.0/24")
	if err != nil {
		t.Fatalf("ParseSubnet: %v", err)
	}
	if s.Gateway() != "172.16.0.1" {
		t.Fatalf("gateway = %s, want 172.16.0.1", s.Gateway())
	}
	if s.GatewayCIDR() != "172.16.0.1/24" {
		t.Fatalf("gatewayCIDR = %s, want 172.16.0.1/24", s.GatewayCIDR())
	}
}

func TestSubnetAllocateSkipsReserved(t *testing.T) {
	s, _ := ParseSubnet("172.16.0.0/24")
	first, err := s.Allocate("vm1")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	// .0 network, .1 gateway, .255 broadcast are never handed out.
	if first != "172.16.0.2" {
		t.Fatalf("first allocation = %s, want 172.16.0.2", first)
	}
	second, _ := s.Allocate("vm2")
	if second == first {
		t.Fatalf("Allocate handed out %s twice", first)
	}
}

func TestSubnetNoDuplicates(t *testing.T) {
	s, _ := ParseSubnet("172.16.0.0/24")
	seen := map[string]bool{}
	// /24 minus network/gateway/broadcast = 253 usable.
	for i := 0; i < 253; i++ {
		ip, err := s.Allocate("vm")
		if err != nil {
			t.Fatalf("Allocate #%d: %v", i, err)
		}
		if seen[ip] {
			t.Fatalf("duplicate IP %s", ip)
		}
		seen[ip] = true
	}
	if _, err := s.Allocate("overflow"); err == nil {
		t.Fatalf("expected exhaustion error after filling the subnet")
	}
}

func TestSubnetReserveThenAllocateNoCollision(t *testing.T) {
	s, _ := ParseSubnet("172.16.0.0/24")
	if err := s.Reserve("adopted", "172.16.0.2"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// A fresh allocation must not reuse the reserved .2.
	for i := 0; i < 10; i++ {
		ip, err := s.Allocate("vm")
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		if ip == "172.16.0.2" {
			t.Fatalf("Allocate reused reserved address 172.16.0.2")
		}
	}
}

func TestSubnetReleaseFreesAddress(t *testing.T) {
	s, _ := ParseSubnet("172.16.0.0/30") // .0 net, .1 gw, .2 usable, .3 bcast
	ip, err := s.Allocate("vm1")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if ip != "172.16.0.2" {
		t.Fatalf("got %s, want 172.16.0.2", ip)
	}
	if _, err := s.Allocate("vm2"); err == nil {
		t.Fatalf("expected /30 to be exhausted after one host")
	}
	s.Release("vm1")
	if _, err := s.Allocate("vm2"); err != nil {
		t.Fatalf("Allocate after Release: %v", err)
	}
}

func TestParseSubnetRejects(t *testing.T) {
	for _, cidr := range []string{"nonsense", "172.16.0.0/31", "::1/64", "10.0.0.0/33"} {
		if _, err := ParseSubnet(cidr); err == nil {
			t.Fatalf("expected error parsing %q", cidr)
		}
	}
}
