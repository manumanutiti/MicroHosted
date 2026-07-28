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

	// AllowedEgress punches specific holes in a non-egress network's WAN drop:
	// only the listed destination/protocol/port flows are forwarded (and
	// masqueraded); everything else to the WAN is still dropped. Empty → no
	// egress at all. Mutually exclusive with Egress (which already allows all).
	AllowedEgress []EgressRule

	// Intra controls VM↔VM reachability WITHIN the network. False (default,
	// deny-by-default): every VM's TAP is enslaved as an ISOLATED bridge port,
	// so VMs reach the gateway (and whatever the egress policy allows) but
	// never each other. True: plain L2 segment, VMs on the bridge see each
	// other — the "networks between machines" case, now explicit opt-in.
	Intra bool

	CreatedAt time.Time
}

// EgressRule permits one outbound flow from a network whose WAN egress is
// otherwise dropped: a destination (IP or CIDR), a protocol, and — for TCP and
// UDP — a destination port. The typical IoT case: a parser VM allowed to reach
// only its MQTT broker at 203.0.113.7:8883/tcp.
type EgressRule struct {
	// IP is the destination: a single IPv4 address ("203.0.113.7") or an IPv4
	// CIDR ("203.0.113.0/28").
	IP string `json:"ip"`
	// Protocol is "tcp", "udp" or "icmp".
	Protocol string `json:"protocol"`
	// Port is the destination port (1–65535). Required for tcp/udp; must be
	// omitted for icmp.
	Port int `json:"port,omitempty"`
}

// CreateNetworkRequest is the payload accepted by POST /v1/networks.
type CreateNetworkRequest struct {
	Name string `json:"name"`
	// Subnet is optional: if empty, a free /24 is auto-allocated from the pool.
	Subnet string `json:"subnet,omitempty"`
	Egress bool   `json:"egress,omitempty"`
	// AllowedEgress lists the only WAN flows this network may open. Requires
	// Egress to be false/omitted.
	AllowedEgress []EgressRule `json:"allowed_egress,omitempty"`
	// Intra opts in to VM↔VM connectivity within the network (off by default).
	Intra bool `json:"intra,omitempty"`
}

// UpdateNetworkEgressRequest is the payload accepted by
// PUT /v1/networks/{name}/egress: it REPLACES the network's whole egress
// policy (both fields) — it does not merge with the existing rules. Same
// semantics as at create time: Egress and AllowedEgress are mutually
// exclusive, omitting both means "no egress at all".
type UpdateNetworkEgressRequest struct {
	Egress        bool         `json:"egress,omitempty"`
	AllowedEgress []EgressRule `json:"allowed_egress,omitempty"`
}

// UpdateNetworkIntraRequest is the payload accepted by
// PUT /v1/networks/{name}/intra: flips VM↔VM reachability on a live network.
type UpdateNetworkIntraRequest struct {
	Intra bool `json:"intra"`
}

// NetworkResponse is the JSON representation of a Network returned by the API.
type NetworkResponse struct {
	Name          string       `json:"name"`
	Bridge        string       `json:"bridge"`
	Subnet        string       `json:"subnet"`
	Gateway       string       `json:"gateway"`
	Egress        bool         `json:"egress"`
	AllowedEgress []EgressRule `json:"allowed_egress,omitempty"`
	Intra         bool         `json:"intra"`
	CreatedAt     string       `json:"created_at"`
}

// NewNetworkResponse builds the API DTO from an internal Network record.
func NewNetworkResponse(n *Network) NetworkResponse {
	return NetworkResponse{
		Name:          n.Name,
		Bridge:        n.Bridge,
		Subnet:        n.Subnet,
		Gateway:       n.Gateway,
		Egress:        n.Egress,
		AllowedEgress: n.AllowedEgress,
		Intra:         n.Intra,
		CreatedAt:     n.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}
