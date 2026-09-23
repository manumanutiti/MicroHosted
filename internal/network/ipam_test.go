package network

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"microhosted/pkg/types"
)

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

// ReserveExclusive is the fork path: the snapshot's IP either is free or the
// fork must not join the network. Unlike Reserve (adopt-on-restart), a taken
// address must be an error, never a silent double-booking.
func TestSubnetReserveExclusiveRejectsTakenAddress(t *testing.T) {
	s, err := ParseSubnet("172.16.0.0/24")
	if err != nil {
		t.Fatalf("ParseSubnet: %v", err)
	}

	ip, err := s.Allocate("origin-vm")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	if err := s.ReserveExclusive("fork-vm", ip); err == nil {
		t.Fatalf("ReserveExclusive(%s) succeeded while origin-vm holds it", ip)
	}

	// Once the origin releases the address, the fork can claim it...
	s.Release("origin-vm")
	if err := s.ReserveExclusive("fork-vm", ip); err != nil {
		t.Fatalf("ReserveExclusive after release: %v", err)
	}
	// ...and a second fork can't.
	if err := s.ReserveExclusive("fork-vm-2", ip); err == nil {
		t.Fatal("second ReserveExclusive on same address must fail")
	}
}

func TestSubnetReserveExclusiveRejectsOutsideSubnet(t *testing.T) {
	s, err := ParseSubnet("172.16.0.0/24")
	if err != nil {
		t.Fatalf("ParseSubnet: %v", err)
	}
	if err := s.ReserveExclusive("vm", "10.0.0.5"); err == nil {
		t.Fatal("ReserveExclusive outside the subnet must fail")
	}
	if err := s.ReserveExclusive("vm", "not-an-ip"); err == nil {
		t.Fatal("ReserveExclusive with a bogus IP must fail")
	}
}

// A pinned address (an ingress rule's to_ip) is never handed out by Allocate,
// even when free; an explicit claim still takes it.
func TestAllocateAvoidsPinned(t *testing.T) {
	s, err := ParseSubnet("172.16.0.0/29") // guests .2 .. .6
	if err != nil {
		t.Fatal(err)
	}
	pinned := pinnedIPs(&types.Network{AllowedIngress: []types.IngressRule{{ToIP: "172.16.0.2"}, {ToIP: "172.16.0.4"}}})
	var got []string
	for i := 0; i < 3; i++ {
		ip, err := s.AllocateAvoiding(fmt.Sprintf("vm%d", i), pinned)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ip)
	}
	if want := []string{"172.16.0.3", "172.16.0.5", "172.16.0.6"}; !reflect.DeepEqual(got, want) {
		t.Errorf("allocated %v, want %v (skipping the pinned .2 and .4)", got, want)
	}
	if _, err := s.AllocateAvoiding("vm3", pinned); err == nil {
		t.Error("only pinned addresses left, yet one was allocated")
	}
	if err := s.ReserveExclusive("repl", "172.16.0.2"); err != nil {
		t.Errorf("claiming a free pinned address: %v", err)
	}
	if err := s.ReserveExclusive("other", "172.16.0.2"); !errors.Is(err, ErrAddressInUse) {
		t.Errorf("claiming a held address = %v, want ErrAddressInUse", err)
	}
	if pinnedIPs(&types.Network{}) != nil {
		t.Error("a network without ingress rules pins nothing")
	}
}
