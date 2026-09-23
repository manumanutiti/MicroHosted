package network

import (
	"fmt"
	"sync"
)

// subnetPool hands out /24 subnets for networks that don't specify one, from
// the 172.16.0.0/12 private range (172.16.0.0 – 172.31.255.255 = 4096 /24s).
// Operator-specified subnets bypass the pool but are marked reserved so the
// pool never auto-hands-out something that overlaps.
type subnetPool struct {
	mu   sync.Mutex
	used map[string]bool // cidr -> true
}

func newSubnetPool() *subnetPool {
	return &subnetPool{used: make(map[string]bool)}
}

// allocate returns the next free /24 in 172.16–31.x.0/24 order.
func (p *subnetPool) allocate() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < 4096; i++ {
		cidr := fmt.Sprintf("172.%d.%d.0/24", 16+i/256, i%256)
		if !p.used[cidr] {
			p.used[cidr] = true
			return cidr, nil
		}
	}
	return "", fmt.Errorf("no free /24 subnets left in 172.16.0.0/12")
}

// reserve marks a subnet as taken (used at reconcile and for operator-chosen
// subnets) so allocate never overlaps it.
func (p *subnetPool) reserve(cidr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.used[cidr] = true
}

// release returns a subnet to the pool when its network is deleted (or its
// creation rolled back), so churn doesn't exhaust the range until a restart.
func (p *subnetPool) release(cidr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used, cidr)
}
