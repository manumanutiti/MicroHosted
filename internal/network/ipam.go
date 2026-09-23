package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
)

// Subnet is the per-network IP allocator (IPAM) for one segmented network. It
// hands out guest addresses from that network's CIDR, reserving the first host
// (.1) as the gateway that lives on the bridge. It replaces the old global /30
// Allocator: there's one Subnet per Network now, not one pool for the whole
// host.
//
// In-memory like the old allocator; on a daemon restart the network manager
// rebuilds it from persisted VM records via Reserve (same pattern the /30
// Allocator uses for adoption).
type Subnet struct {
	mu      sync.Mutex
	base    uint32 // network address
	gateway uint32 // base+1, held for the host side on the bridge
	bcast   uint32 // broadcast address (last in range)
	prefix  int
	used    map[string]uint32 // vmID -> ip
	taken   map[uint32]bool
}

// ParseSubnet builds a Subnet from a CIDR (e.g. "172.16.0.0/24"). The gateway
// is the first usable host (.1). A prefix leaving no usable hosts is rejected.
func ParseSubnet(cidr string) (*Subnet, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parsing subnet %q: %w", cidr, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("subnet %q is not IPv4", cidr)
	}
	prefix, bits := ipnet.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("subnet %q is not IPv4", cidr)
	}
	if prefix > 30 {
		return nil, fmt.Errorf("subnet %q too small: need at least a /30", cidr)
	}

	base := binary.BigEndian.Uint32(ipnet.IP.To4())
	size := uint32(1) << uint(32-prefix)
	bcast := base + size - 1
	gateway := base + 1

	return &Subnet{
		base:    base,
		gateway: gateway,
		bcast:   bcast,
		prefix:  prefix,
		used:    make(map[string]uint32),
		taken:   map[uint32]bool{base: true, gateway: true, bcast: true},
	}, nil
}

// Gateway returns the host-side gateway IP (the .1) as a string.
func (s *Subnet) Gateway() string {
	return uint32ToIP(s.gateway).String()
}

// GatewayCIDR returns the gateway with the subnet prefix (e.g. "172.16.0.1/24"),
// the form used to assign the address to the bridge device.
func (s *Subnet) GatewayCIDR() string {
	return fmt.Sprintf("%s/%d", uint32ToIP(s.gateway).String(), s.prefix)
}

// Prefix returns the subnet prefix length (e.g. 24).
func (s *Subnet) Prefix() int { return s.prefix }

// CheckGuestIP reports whether ipStr is an address a guest on this subnet can
// hold: inside it, and neither the network, gateway nor broadcast address.
// Whether a VM holds it right now is deliberately not asked — a rule naming
// it must survive that VM being destroyed and restored.
func (s *Subnet) CheckGuestIP(ipStr string) error {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return fmt.Errorf("%q is not an IPv4 address", ipStr)
	}
	v := binary.BigEndian.Uint32(ip)
	if v <= s.gateway || v >= s.bcast {
		return fmt.Errorf("%s is not a guest address of %s (first guest is %s)",
			ipStr, fmt.Sprintf("%s/%d", uint32ToIP(s.base), s.prefix), uint32ToIP(s.gateway+1))
	}
	return nil
}

// ErrAddressInUse is a specific address someone else already holds;
// ErrBadAddress one no guest of the subnet can hold at all.
var (
	ErrAddressInUse = errors.New("address already in use")
	ErrBadAddress   = errors.New("not a guest address")
)

// Allocate returns the next free guest IP for vmID.
func (s *Subnet) Allocate(vmID string) (string, error) {
	return s.AllocateAvoiding(vmID, nil)
}

// AllocateAvoiding is Allocate that never hands out an address in avoid, even
// a free one. Those are pinned (the to_ip of an ingress rule): traffic is
// addressed to them from outside, so only an explicit claim may take one —
// never whichever VM happens to be created next.
func (s *Subnet) AllocateAvoiding(vmID string, avoid map[string]bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Scan the whole usable range (gateway+1 .. bcast-1) and take the first
	// free address. No high-water-mark shortcut: it would skip addresses freed
	// by Release that sit below the mark.
	for ip := s.gateway + 1; ip < s.bcast; ip++ {
		if s.taken[ip] || avoid[uint32ToIP(ip).String()] {
			continue
		}
		s.taken[ip] = true
		s.used[vmID] = ip
		return uint32ToIP(ip).String(), nil
	}
	return "", fmt.Errorf("no free addresses left in /%d subnet", s.prefix)
}

// Reserve re-registers a guest IP a still-running VM already holds, so a
// restart can adopt VMs without later handing their address to a new VM.
func (s *Subnet) Reserve(vmID, ipStr string) error {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return fmt.Errorf("reserving %q for %s: invalid IPv4", ipStr, vmID)
	}
	v := binary.BigEndian.Uint32(ip)
	if v <= s.base || v >= s.bcast {
		return fmt.Errorf("reserving %q for %s: outside subnet", ipStr, vmID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.taken[v] = true
	s.used[vmID] = v
	return nil
}

// ReserveExclusive claims a specific IP for vmID, failing if the address is
// already held. This is the fork-from-snapshot path: the restored guest wakes
// up with the snapshot's IP frozen in its memory, so either that exact address
// is free on the network or the fork must not join it at all. Silently
// double-booking (what Reserve does, deliberately, for adopt-on-restart where
// the address was already ours) would put two VMs with the same IP *and* the
// same derived MAC on one bridge.
func (s *Subnet) ReserveExclusive(vmID, ipStr string) error {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return fmt.Errorf("claiming %q for %s: invalid IPv4", ipStr, vmID)
	}
	v := binary.BigEndian.Uint32(ip)
	if v <= s.base || v >= s.bcast {
		return fmt.Errorf("claiming %q for %s: outside subnet", ipStr, vmID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.taken[v] {
		return fmt.Errorf("%w on this network: %s", ErrAddressInUse, ipStr)
	}
	s.taken[v] = true
	s.used[vmID] = v
	return nil
}

// Release frees the address held by vmID.
func (s *Subnet) Release(vmID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ip, ok := s.used[vmID]; ok {
		delete(s.taken, ip)
		delete(s.used, vmID)
	}
}

func uint32ToIP(v uint32) net.IP {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return net.IP(b)
}

// Overlaps reports whether two subnets share any address.
func (s *Subnet) Overlaps(o *Subnet) bool {
	return s.base <= o.bcast && o.base <= s.bcast
}
