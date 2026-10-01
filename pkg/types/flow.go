package types

import "time"

// Flow is what a guest tried, aggregated by destination: every packet the
// ruleset recorded with the same verdict, reason, protocol and destination
// adds to Count. The source port is not part of it — each retry of the same
// connection has a new one.
type Flow struct {
	// VM is the guest that sent it: the holder of Src on Network when the
	// record arrived, or the VM behind a quarantined TAP. Empty when no VM
	// held the address (one destroyed in between).
	VM      string `json:"vm,omitempty"`
	Network string `json:"network,omitempty"`
	// Iface is the device the packet came in by: the network's bridge, or a
	// quarantined VM's TAP.
	Iface string `json:"iface"`

	Verdict string `json:"verdict"` // drop
	// Reason is why: host, quarantine, egress, iface, private, managed,
	// cross, unlisted (see docs/networking.md, "Flow log").
	Reason   string `json:"reason"`
	Protocol string `json:"protocol"`           // tcp, udp, icmp, or the IP protocol number
	Src      string `json:"src"`                // the guest's address
	Dst      string `json:"dst"`                // where it tried to go
	DstPort  int    `json:"dst_port,omitempty"` // tcp and udp

	Count uint64    `json:"count"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// FlowList is the flows of one VM or network, oldest first. Omitted counts
// records for destinations the list had no room for: the first ones a guest
// tried are kept, so a guest cannot push its earliest attempts out by trying
// many more. Overruns counts, host-wide since the daemon started, the times
// the kernel discarded records because the daemon's socket was full: each
// hides one record or more, and the kernel does not say how many.
type FlowList struct {
	Flows    []Flow `json:"flows"`
	Omitted  uint64 `json:"omitted,omitempty"`
	Overruns uint64 `json:"overruns,omitempty"`
}
