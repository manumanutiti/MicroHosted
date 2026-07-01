package network

import (
	"fmt"
	"net"
	"sync"
)

// Block is a point-to-point /30 pair handed out to one VM: host and guest
// each get an address, reachable over that VM's own TAP device.
type Block struct {
	HostIP    string
	GuestIP   string
	GatewayIP string // same as HostIP — the guest routes through the host
	PrefixLen int
}

const (
	poolA     = 172
	poolB     = 16
	maxBlocks = 16384 // 172.16.0.0/16 sliced into /30s
)

// Allocator hands out /30 blocks from a fixed 172.16.0.0/16 pool.
//
// This is intentionally simplistic — sequential counter, in-memory, no
// reuse of released blocks, no persistence across restarts. Sesión 9
// replaces it with a real IPAM once there's more than one host to
// coordinate across.
type Allocator struct {
	mu   sync.Mutex
	next int
	used map[string]int // vmID -> block index
}

// NewAllocator creates an empty allocator.
func NewAllocator() *Allocator {
	return &Allocator{used: make(map[string]int)}
}

// Allocate returns the next free /30 block for the given VM ID.
func (a *Allocator) Allocate(vmID string) (Block, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.next >= maxBlocks {
		return Block{}, fmt.Errorf("no /30 blocks left in %d.%d.0.0/16", poolA, poolB)
	}

	idx := a.next
	a.next++
	a.used[vmID] = idx

	base := idx * 4
	c := byte((base / 256) % 256)
	d := byte(base % 256)

	host := net.IPv4(poolA, poolB, c, d+1).String()
	guest := net.IPv4(poolA, poolB, c, d+2).String()

	return Block{HostIP: host, GuestIP: guest, GatewayIP: host, PrefixLen: 30}, nil
}

// Reserve re-registers a block that a still-running VM already holds, so a
// restart of the daemon can adopt live VMs (see vm.Manager.Reconcile) without
// the allocator later handing their IP range to a new VM. hostIP is the
// block's host-side address, from which the block index is recovered — the
// inverse of the layout Allocate encodes. next is advanced past the reserved
// index so sequential allocation never collides with it.
func (a *Allocator) Reserve(vmID, hostIP string) error {
	ip := net.ParseIP(hostIP).To4()
	if ip == nil {
		return fmt.Errorf("reserving block for %s: invalid host IP %q", vmID, hostIP)
	}
	if ip[0] != poolA || ip[1] != poolB {
		return fmt.Errorf("reserving block for %s: host IP %q outside %d.%d.0.0/16", vmID, hostIP, poolA, poolB)
	}

	// Allocate lays out host = base+1 with base = idx*4, so idx = (base)/4.
	base := int(ip[2])*256 + int(ip[3]) - 1
	idx := base / 4

	a.mu.Lock()
	defer a.mu.Unlock()
	a.used[vmID] = idx
	if idx >= a.next {
		a.next = idx + 1
	}
	return nil
}

// Release forgets the block held by vmID. It does not currently return the
// block to the free pool (see the simplicity note above).
func (a *Allocator) Release(vmID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.used, vmID)
}
