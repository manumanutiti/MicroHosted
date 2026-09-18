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

	// AllowedIngress lets specific devices on a managed interface open
	// connections INTO one of this network's VMs: each rule DNATs a
	// (source, protocol, port) arriving at the host to a guest address. The
	// host never listens on the port. Independent of the egress fields.
	AllowedIngress []IngressRule

	// Intra controls VM↔VM reachability WITHIN the network. False (default,
	// deny-by-default): every VM's TAP is enslaved as an ISOLATED bridge port,
	// so VMs reach the gateway (and whatever the egress policy allows) but
	// never each other. True: plain L2 segment, VMs on the bridge see each
	// other — the "networks between machines" case, now explicit opt-in.
	Intra bool

	CreatedAt time.Time
}

// EgressRule permits one outbound flow from a network whose egress is otherwise
// dropped: a destination (IP or CIDR), a protocol, and — for TCP and UDP — a
// destination port. The typical IoT case: a parser VM allowed to reach only its
// MQTT broker at 203.0.113.7:8883/tcp.
type EgressRule struct {
	// Iface picks WHERE the destination is, and it changes the rule's meaning:
	//
	//   - empty (the usual case): out towards the WAN, i.e. anywhere that is not
	//     one of our own bridges. Replies return on their own — nothing in the
	//     ruleset drops them.
	//
	//   - a MANAGED interface (see the daemon's --managed-iface): a host
	//     interface whose whole policy this daemon owns, denied in both
	//     directions by default. A rule naming one also emits the matching
	//     return rule, because otherwise that blanket deny would eat the reply.
	//
	// Naming an interface the daemon was not told to manage is rejected: the
	// deny-by-default that makes such a rule meaningful only exists for declared
	// interfaces, so a hole through an undeclared one would be a hole with no
	// policy around it.
	Iface string `json:"iface,omitempty"`
	// IP is the destination: a single IPv4 address ("203.0.113.7") or an IPv4
	// CIDR ("203.0.113.0/28"). A range is legitimate — "this network may poll
	// that whole segment" is a real deployment. Narrowing it to one device per
	// network is a policy an orchestrator imposes, not something the engine
	// decides.
	IP string `json:"ip"`
	// Protocol is "tcp", "udp" or "icmp".
	Protocol string `json:"protocol"`
	// Port is the destination port (1–65535). Required for tcp/udp; must be
	// omitted for icmp.
	Port int `json:"port,omitempty"`
}

// IngressRule lets one device on a managed interface open connections into
// one VM of the network — the push case, where a sensor is the client (MQTT,
// HTTP) and cannot be polled. The device addresses the HOST on that interface
// (e.g. gateway:1883); the kernel rewrites the destination to ToIP in
// prerouting, so the host has no listener and never touches the payload.
// Only replies of that DNATed flow come back out; the VM still cannot open
// anything towards the device.
type IngressRule struct {
	// Iface is the managed interface the device sits behind. Required: the
	// deny-both-ways policy of a managed interface is what makes a hole in
	// it safe, and there is no such policy on any other interface.
	Iface string `json:"iface"`
	// SrcIP is the device allowed in: an IPv4 address or CIDR. It is spoofable
	// on a flat segment, so pair it with per-device credentials inside the VM
	// and port isolation at the access layer (see docs/iot-edge.md).
	SrcIP string `json:"src_ip"`
	// Protocol is "tcp" or "udp".
	Protocol string `json:"protocol"`
	// Port is both the port the device connects to on the host and the port
	// the VM listens on (no remapping).
	Port int `json:"port"`
	// ToIP is the guest address inside this network's subnet that receives
	// the flow. An address, not a VM name, so the rule survives a VM being
	// reset from its snapshot (the restored guest keeps the snapshot's IP).
	// If no VM holds it, the flow goes nowhere.
	ToIP string `json:"to_ip"`
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
	// AllowedIngress lists the only inbound flows from managed interfaces.
	// Needs an explicit Subnet: every rule's to_ip must fall inside it.
	AllowedIngress []IngressRule `json:"allowed_ingress,omitempty"`
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

// UpdateNetworkIngressRequest is the payload accepted by
// PUT /v1/networks/{name}/ingress: it REPLACES the network's whole ingress
// policy. An empty list closes every inbound hole.
type UpdateNetworkIngressRequest struct {
	AllowedIngress []IngressRule `json:"allowed_ingress"`
}

// UpdateNetworkIntraRequest is the payload accepted by
// PUT /v1/networks/{name}/intra: flips VM↔VM reachability on a live network.
type UpdateNetworkIntraRequest struct {
	Intra bool `json:"intra"`
}

// NetworkResponse is the JSON representation of a Network returned by the API.
type NetworkResponse struct {
	Name           string        `json:"name"`
	Bridge         string        `json:"bridge"`
	Subnet         string        `json:"subnet"`
	Gateway        string        `json:"gateway"`
	Egress         bool          `json:"egress"`
	AllowedEgress  []EgressRule  `json:"allowed_egress,omitempty"`
	AllowedIngress []IngressRule `json:"allowed_ingress,omitempty"`
	Intra          bool          `json:"intra"`
	CreatedAt      string        `json:"created_at"`
}

// NewNetworkResponse builds the API DTO from an internal Network record.
func NewNetworkResponse(n *Network) NetworkResponse {
	return NetworkResponse{
		Name:           n.Name,
		Bridge:         n.Bridge,
		Subnet:         n.Subnet,
		Gateway:        n.Gateway,
		Egress:         n.Egress,
		AllowedEgress:  n.AllowedEgress,
		AllowedIngress: n.AllowedIngress,
		Intra:          n.Intra,
		CreatedAt:      n.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}
