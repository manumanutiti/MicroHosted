package network

import (
	"fmt"
	"testing"
)

func TestAllocatorNoOverlap(t *testing.T) {
	a := NewAllocator()
	seen := make(map[string]bool)
	for i := 0; i < 500; i++ {
		vmID := fmt.Sprintf("vm-%d", i)
		block, err := a.Allocate(vmID)
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		if seen[block.HostIP] || seen[block.GuestIP] {
			t.Fatalf("duplicate IP allocated: %+v", block)
		}
		seen[block.HostIP] = true
		seen[block.GuestIP] = true
		if block.HostIP == block.GuestIP {
			t.Fatalf("host and guest IP must differ: %+v", block)
		}
	}
}

// Reserve recovers a block index from its host IP; it must be the exact
// inverse of the layout Allocate encodes, or a restart would re-reserve the
// wrong range and later collide.
func TestAllocatorReserveRoundTrip(t *testing.T) {
	a := NewAllocator()
	for i := 0; i < 500; i++ {
		vmID := fmt.Sprintf("vm-%d", i)
		block, err := a.Allocate(vmID)
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}

		// A fresh allocator (as after a restart) must land on the same index.
		fresh := NewAllocator()
		if err := fresh.Reserve(vmID, block.HostIP); err != nil {
			t.Fatalf("Reserve(%s): %v", block.HostIP, err)
		}
		if got, want := fresh.used[vmID], a.used[vmID]; got != want {
			t.Fatalf("Reserve recovered index %d, Allocate used %d for host %s", got, want, block.HostIP)
		}
	}
}

// After reserving a live VM's block, the next fresh allocation must not reuse
// that range — the whole point of reconciliation.
func TestAllocatorReserveThenAllocateNoCollision(t *testing.T) {
	a := NewAllocator()
	if err := a.Reserve("adopted", "172.16.0.13"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	block, err := a.Allocate("fresh")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if block.HostIP == "172.16.0.13" {
		t.Fatalf("Allocate reused a reserved block: %+v", block)
	}
}

func TestAllocatorReserveRejectsBadIP(t *testing.T) {
	a := NewAllocator()
	if err := a.Reserve("x", "10.0.0.1"); err == nil {
		t.Fatalf("expected error reserving IP outside the pool")
	}
	if err := a.Reserve("x", "not-an-ip"); err == nil {
		t.Fatalf("expected error reserving invalid IP")
	}
}

func TestAllocatorRelease(t *testing.T) {
	a := NewAllocator()
	if _, err := a.Allocate("vm-1"); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	a.Release("vm-1")
	if len(a.used) != 0 {
		t.Fatalf("expected used map to be empty after release")
	}
}
