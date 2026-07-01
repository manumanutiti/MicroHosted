package types

import "time"

// Network is a named L2 segment: a Linux bridge + a subnet, that VMs attach to.
// VMs on the same network see each other (same bridge); VMs on different
// networks are isolated (separate bridges + nftables). See docs/networking.md.
type Network struct {
	// ID is an internal 8-hex handle used to derive device names (bridge
	// mhbr<ID>) that fit within IFNAMSIZ; Name is the user-facing key.
	ID   string
	Name string

	// Bridge is the Linux bridge device backing this network (mhbr<ID>).
	Bridge string

	// Subnet is the network's CIDR (e.g. 172.16.0.0/24); Gateway is the host's
	// IP on the bridge (the .1 of the subnet), which guests use as their route.
	Subnet  string
	Gateway string

	// Egress controls internet reachability: true → the subnet is MASQUERADEd
	// out the host's default route; false (default, malware-safe) → no NAT and
	// forwarding to the WAN is dropped.
	Egress bool

	CreatedAt time.Time
}

// CreateNetworkRequest is the payload accepted by POST /v1/networks.
type CreateNetworkRequest struct {
	Name string `json:"name"`
	// Subnet is optional: if empty, a free /24 is auto-allocated from the pool.
	Subnet string `json:"subnet,omitempty"`
	Egress bool   `json:"egress,omitempty"`
}

// NetworkResponse is the JSON representation of a Network returned by the API.
type NetworkResponse struct {
	Name      string `json:"name"`
	Bridge    string `json:"bridge"`
	Subnet    string `json:"subnet"`
	Gateway   string `json:"gateway"`
	Egress    bool   `json:"egress"`
	CreatedAt string `json:"created_at"`
}

// NewNetworkResponse builds the API DTO from an internal Network record.
func NewNetworkResponse(n *Network) NetworkResponse {
	return NetworkResponse{
		Name:      n.Name,
		Bridge:    n.Bridge,
		Subnet:    n.Subnet,
		Gateway:   n.Gateway,
		Egress:    n.Egress,
		CreatedAt: n.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}
