package network

import (
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strings"

	"microhosted/pkg/types"
)

// nftTable is the single table this daemon owns. Living in its own `inet`
// table (not editing the host's filter tables) means our base chains coexist
// with whatever firewall the host already runs — we add rules, we never
// clobber theirs.
const nftTable = "inet microhosted"

// ManagedIface is a host interface whose ENTIRE nftables policy this daemon
// owns: denied in both directions, with each network's interface-scoped egress
// rules and its ingress rules as the only holes.
//
// It exists because an interface needs exactly one author. Several base chains
// on one hook are all evaluated and a `drop` in any of them is final, so a
// hand-written ruleset beside this one does not conflict with it — it silently
// wins. The visible result is a policy declared through the API, reported back
// by the API, and not the policy in force. Declaring the interface here is what
// makes the API's answer true.
type ManagedIface struct {
	Name string
	// HostAllow are the host's own services that stay reachable from this
	// interface. Nothing is assumed: the daemon has no way to know whether the
	// host addresses that segment, serves it time, or offers it nothing at all,
	// and guessing wrong in either direction is bad (a silent drop bricks the
	// segment; a silent accept is surface nobody asked for). The operator says.
	HostAllow []HostService
}

// HostService is one host-side port left reachable from a managed interface,
// e.g. udp/67 when the host runs that segment's DHCP.
type HostService struct {
	Protocol string // "tcp" or "udp"
	Port     int
}

// ApplyNftables re-renders the ENTIRE ruleset for the given networks and
// installs it atomically. It's declarative on purpose: instead of tracking rule
// handles and adding/deleting individual rules as networks come and go (fiddly,
// easy to leak a stale rule), every change regenerates the whole table and
// swaps it in one `nft -f` transaction. Idempotent.
//
// Policy per network (see docs/networking.md):
//   - guest→host: dropped (except replies to host-initiated conns, so
//     host→guest SSH still works) — a VM can't reach host services.
//   - cross-segment: dropped — VMs on different networks can't see each other.
//   - egress: masqueraded out when the network allows it; dropped otherwise
//     (default), so a sample can't phone home unless explicitly permitted.
//   - managed interfaces: dropped in BOTH directions, except the egress rules
//     that name them. Rendering them here rather than leaving them to a
//     hand-written table beside us is the whole point — two base chains on one
//     hook are both evaluated and a drop anywhere wins, so a second author
//     silently overrides the policy this daemon reports through its API.
//   - ingress: a managed interface's only inbound holes are the networks'
//     AllowedIngress rules, each a prerouting DNAT to one guest address plus
//     the two forward legs of that DNATed flow. The host never listens.
func ApplyNftables(networks []types.Network, managed []ManagedIface) error {
	script := renderNftables(networks, managed)
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("applying nftables: %w\n--- ruleset ---\n%s\n--- nft said ---\n%s", err, script, strings.TrimSpace(string(out)))
	}
	return nil
}

// renderNftables produces the `nft -f` script. The leading
// `table {}` + `delete table` idiom guarantees the delete never fails on a
// missing table, so the whole thing is a clean atomic replace.
func renderNftables(networks []types.Network, managed []ManagedIface) string {
	var b strings.Builder

	// Ensure-then-delete so the recreate below is an atomic replace.
	fmt.Fprintf(&b, "table %s {}\n", nftTable)
	fmt.Fprintf(&b, "delete table %s\n", nftTable)

	if len(networks) == 0 {
		return b.String() // nothing to protect; leave the table gone.
	}

	fmt.Fprintf(&b, "table %s {\n", nftTable)

	// Set of all our bridge ifnames, used to express "to the WAN" as
	// "oifname != @mhbridges" (anything not one of our own bridges).
	b.WriteString("\tset mhbridges {\n\t\ttype ifname\n\t\telements = { ")
	names := make([]string, 0, len(networks))
	for _, n := range networks {
		names = append(names, `"`+n.Bridge+`"`)
	}
	b.WriteString(strings.Join(names, ", "))
	b.WriteString(" }\n\t}\n")

	// Same-bridge pairs, to exempt intra-bridge traffic from the cross-segment
	// drop below. Needed because br_netfilter (if loaded) pushes same-bridge
	// traffic through the host's forward hook, and nft cannot express
	// `iifname != oifname` directly (no expression-vs-expression compare).
	b.WriteString("\tset mhsame {\n\t\ttype ifname . ifname\n\t\telements = { ")
	pairs := make([]string, 0, len(networks))
	for _, n := range networks {
		pairs = append(pairs, `"`+n.Bridge+`" . "`+n.Bridge+`"`)
	}
	b.WriteString(strings.Join(pairs, ", "))
	b.WriteString(" }\n\t}\n")

	// guest → host: drop new connections coming in from any bridge, but let
	// established/related through so host-initiated flows (e.g. SSH into a VM)
	// still get their replies.
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority 0; policy accept;\n")
	b.WriteString("\t\tct state established,related accept\n")
	b.WriteString("\t\tiifname @mhbridges drop\n")
	// A managed interface reaches only the host services the operator listed
	// (typically udp/67 when the host runs that segment's DHCP), and nothing
	// else — not the API, not SSH.
	for _, m := range managed {
		for _, h := range m.HostAllow {
			fmt.Fprintf(&b, "\t\tiifname %q %s dport %d accept\n", m.Name, h.Protocol, h.Port)
		}
		fmt.Fprintf(&b, "\t\tiifname %q drop\n", m.Name)
	}
	b.WriteString("\t}\n")

	// Ingress: rewrite the destination of each allowed inbound flow to its
	// guest, in the kernel, before routing. The packet then goes through
	// `forward`, never `input` — nothing on the host listens on the port.
	// `fib daddr type local` limits it to traffic addressed to the host
	// itself, so transit traffic crossing the interface is never captured.
	// Only rules whose interface is managed are rendered, for the same
	// reason as the egress rules below.
	var dnats []string
	for _, n := range networks {
		for _, r := range n.AllowedIngress {
			if !isManaged(managed, r.Iface) {
				continue
			}
			dnats = append(dnats, fmt.Sprintf("\t\tiifname %q ip saddr %s fib daddr type local %s dport %d dnat ip to %s:%d\n",
				r.Iface, r.SrcIP, r.Protocol, r.Port, r.ToIP, r.Port))
		}
	}
	if len(dnats) > 0 {
		b.WriteString("\tchain prerouting {\n")
		b.WriteString("\t\ttype nat hook prerouting priority -100; policy accept;\n")
		for _, d := range dnats {
			b.WriteString(d)
		}
		b.WriteString("\t}\n")
	}

	// The forward chain matches STATELESSLY on purpose: no blanket
	// `ct state established,related accept` here (unlike input). Every drop
	// below is scoped by iifname/daddr, so it doesn't touch return traffic
	// (replies arrive from the WAN side and pass via policy accept), and every
	// packet of a guest-initiated flow re-matches the policy on every pass.
	// That's what makes UpdateEgress safe: tightening the rules cuts flows that
	// were established under the old policy IMMEDIATELY — a stale conntrack
	// entry can't ride an established-accept past the new rules, and we don't
	// need the conntrack(8) binary to flush anything.
	b.WriteString("\tchain forward {\n")
	b.WriteString("\t\ttype filter hook forward priority 0; policy accept;\n")
	// Cross-segment isolation: drop forwarding between two DIFFERENT bridges.
	// One aggregate rule instead of a rule per ordered pair (which grows
	// O(N²) — 150 networks would mean 22350 rules, each traversed by every
	// forwarded packet). The @mhsame exemption keeps same-bridge traffic
	// reachable even if br_netfilter is on.
	b.WriteString("\t\tiifname @mhbridges oifname @mhbridges iifname . oifname != @mhsame drop\n")
	// Rules naming a managed interface come first, ahead of every drop that
	// would otherwise catch them: the per-network egress drop below (a managed
	// interface is not one of our bridges) and the blanket drops that close the
	// chain.
	//
	// Each emits BOTH legs. The return leg is matched on the stateless tuple
	// like the rest of this chain — by the destination's own address and its
	// source port — so tightening the policy cuts a live flow on its next
	// packet instead of letting a conntrack entry ride through. On top of the
	// tuple it requires `ct direction reply`: the tuple alone would let the
	// DEVICE open connections into the guest, on any port, just by using the
	// rule's port as its source port (or, for icmp, by pinging it). The reply
	// arrives already un-masqueraded: conntrack reverses the source NAT in
	// prerouting, which runs before this hook, so `ip daddr` here is the
	// guest's real address.
	for _, n := range networks {
		for _, r := range n.AllowedEgress {
			if r.Iface == "" {
				continue // towards the WAN; handled by the loop below
			}
			// A stored rule for an interface this daemon no longer manages (it
			// was restarted without that --managed-iface) is skipped: its accept
			// was only ever safe inside the deny-both-ways drops rendered for a
			// managed interface, and those are gone. Enforcing less than the API
			// reports is the recoverable direction; Manager.UnenforcedRules and
			// the egress_policy health check say so out loud.
			if !isManaged(managed, r.Iface) {
				continue
			}
			switch r.Protocol {
			case "tcp", "udp":
				fmt.Fprintf(&b, "\t\tiifname %q oifname %q ip daddr %s %s dport %d accept\n", n.Bridge, r.Iface, r.IP, r.Protocol, r.Port)
				fmt.Fprintf(&b, "\t\tiifname %q oifname %q ip saddr %s ip daddr %s %s sport %d ct direction reply accept\n", r.Iface, n.Bridge, r.IP, n.Subnet, r.Protocol, r.Port)
			case "icmp":
				fmt.Fprintf(&b, "\t\tiifname %q oifname %q ip daddr %s meta l4proto icmp accept\n", n.Bridge, r.Iface, r.IP)
				fmt.Fprintf(&b, "\t\tiifname %q oifname %q ip saddr %s ip daddr %s meta l4proto icmp ct direction reply accept\n", r.Iface, n.Bridge, r.IP, n.Subnet)
			}
		}
	}
	// Ingress legs, also ahead of every drop (the return leg leaves the bridge
	// towards a non-bridge, which the network's egress drop would eat).
	//
	// These add conntrack matches on TOP of the stateless tuple — never a
	// blanket established-accept, so the property above holds: remove the
	// rule and the next packet of a live flow meets the managed interface's
	// drop. What conntrack adds is direction. `ct status dnat` admits only
	// flows that went through the DNAT above (not a device that routes
	// straight at the guest subnet), and `ct direction reply` on the return
	// leg means the VM can only answer: without it, a compromised VM could
	// bind the ingress port as its SOURCE port and open connections to any
	// port on the device.
	for _, n := range networks {
		for _, r := range n.AllowedIngress {
			if !isManaged(managed, r.Iface) {
				continue
			}
			fmt.Fprintf(&b, "\t\tiifname %q oifname %q ip saddr %s ip daddr %s %s dport %d ct status dnat ct direction original accept\n",
				r.Iface, n.Bridge, r.SrcIP, r.ToIP, r.Protocol, r.Port)
			fmt.Fprintf(&b, "\t\tiifname %q oifname %q ip saddr %s %s sport %d ip daddr %s ct status dnat ct direction reply accept\n",
				n.Bridge, r.Iface, r.ToIP, r.Protocol, r.Port, r.SrcIP)
		}
	}
	// Everything else crossing a managed interface dies here, both directions,
	// right after its only holes (above) and BEFORE the WAN rules below. To
	// those rules a managed interface is simply "not one of our bridges", so
	// if they ran first a WAN-scoped accept whose destination sits on that
	// segment (`icmp:192.168.50.52` without @wlan0) would send the packet out
	// through it — one-way, since the reply dies here, but still a VM putting
	// packets on the segment through a rule that never named it. Outbound too,
	// not just inbound: an `egress: true` network would otherwise reach the
	// whole segment.
	for _, m := range managed {
		fmt.Fprintf(&b, "\t\tiifname %q drop\n", m.Name)
		fmt.Fprintf(&b, "\t\toifname %q drop\n", m.Name)
	}
	// No-egress networks: drop anything leaving the bridge towards the WAN
	// (oifname not one of our bridges). Same-bridge and cross-segment aren't
	// matched here (their oifname IS a bridge); cross-segment is already
	// dropped above. Fine-grained holes (AllowedEgress) are accepted first, so
	// only the listed destination/protocol/port flows survive the drop.
	for _, n := range networks {
		if n.Egress {
			continue
		}
		for _, r := range n.AllowedEgress {
			if r.Iface != "" {
				continue // already emitted above, scoped to its interface
			}
			switch r.Protocol {
			case "tcp", "udp":
				fmt.Fprintf(&b, "\t\tiifname \"%s\" oifname != @mhbridges ip daddr %s %s dport %d accept\n", n.Bridge, r.IP, r.Protocol, r.Port)
			case "icmp":
				fmt.Fprintf(&b, "\t\tiifname \"%s\" oifname != @mhbridges ip daddr %s meta l4proto icmp accept\n", n.Bridge, r.IP)
			}
		}
		fmt.Fprintf(&b, "\t\tiifname \"%s\" oifname != @mhbridges drop\n", n.Bridge)
	}
	b.WriteString("\t}\n")

	// NAT: masquerade towards the WAN for full-egress networks and for
	// restricted ones (whose allowed flows still need NAT to get replies back).
	// Safe to masquerade a restricted network's whole subnet: postrouting only
	// sees packets the forward chain already accepted.
	b.WriteString("\tchain postrouting {\n")
	b.WriteString("\t\ttype nat hook postrouting priority 100; policy accept;\n")
	// One rule per network covers both destinations: a managed interface is not
	// one of our bridges either, so `oifname != @mhbridges` already masquerades
	// towards it. That NAT is not a convenience there — devices on such a
	// segment are typically handed no default route at all, so a reply addressed
	// to 172.16.x.y would have nowhere to go.
	for _, n := range networks {
		if n.Egress || len(n.AllowedEgress) > 0 {
			fmt.Fprintf(&b, "\t\tip saddr %s oifname != @mhbridges masquerade\n", n.Subnet)
		}
	}
	b.WriteString("\t}\n")

	b.WriteString("}\n")
	return b.String()
}

// ValidateEgressRules vets user-supplied AllowedEgress rules BEFORE they are
// ever rendered. This is a security boundary, not a convenience check: rule
// fields are interpolated verbatim into the `nft -f` script, so anything that
// isn't strictly an IPv4 address/CIDR + known protocol + numeric port must be
// rejected here or it becomes ruleset injection.
func ValidateEgressRules(rules []types.EgressRule, managed []string) error {
	for i, r := range rules {
		if r.Iface != "" && !slices.Contains(managed, r.Iface) {
			if len(managed) == 0 {
				return fmt.Errorf("egress rule %d: iface %q — this daemon manages no interface (start it with --managed-iface)", i, r.Iface)
			}
			return fmt.Errorf("egress rule %d: iface %q is not one this daemon manages (declared: %s)", i, r.Iface, strings.Join(managed, ", "))
		}
		if !isIPv4OrCIDR(r.IP) {
			return fmt.Errorf("egress rule %d: ip %q must be an IPv4 address or CIDR", i, r.IP)
		}
		switch r.Protocol {
		case "tcp", "udp":
			if r.Port < 1 || r.Port > 65535 {
				return fmt.Errorf("egress rule %d: %s needs a port in 1-65535, got %d", i, r.Protocol, r.Port)
			}
		case "icmp":
			if r.Port != 0 {
				return fmt.Errorf("egress rule %d: icmp takes no port", i)
			}
		default:
			return fmt.Errorf("egress rule %d: protocol %q must be tcp, udp or icmp", i, r.Protocol)
		}
	}
	return nil
}

// ValidateIngressRules vets user-supplied AllowedIngress rules before they are
// rendered, for the same reason as ValidateEgressRules: every field lands
// verbatim in the `nft -f` script. It checks each rule on its own and the list
// against itself; where to_ip must fall (the network's subnet) and clashes with
// other networks' rules are the Manager's to check, since only it knows them.
//
// Two rules clash when a packet could match both — same interface, protocol
// and port with overlapping sources. The first DNAT would silently win, so the
// second rule would be reported by the API and never be in force.
func ValidateIngressRules(rules []types.IngressRule, managed []string) error {
	for i, r := range rules {
		if r.Iface == "" {
			return fmt.Errorf("ingress rule %d: iface is required (a managed interface the device sits behind)", i)
		}
		if !slices.Contains(managed, r.Iface) {
			if len(managed) == 0 {
				return fmt.Errorf("ingress rule %d: iface %q — this daemon manages no interface (start it with --managed-iface)", i, r.Iface)
			}
			return fmt.Errorf("ingress rule %d: iface %q is not one this daemon manages (declared: %s)", i, r.Iface, strings.Join(managed, ", "))
		}
		if !isIPv4OrCIDR(r.SrcIP) {
			return fmt.Errorf("ingress rule %d: src_ip %q must be an IPv4 address or CIDR", i, r.SrcIP)
		}
		if ip := net.ParseIP(r.ToIP); ip == nil || ip.To4() == nil || ip.String() != r.ToIP {
			return fmt.Errorf("ingress rule %d: to_ip %q must be a single IPv4 address", i, r.ToIP)
		}
		if r.Protocol != "tcp" && r.Protocol != "udp" {
			return fmt.Errorf("ingress rule %d: protocol %q must be tcp or udp", i, r.Protocol)
		}
		if r.Port < 1 || r.Port > 65535 {
			return fmt.Errorf("ingress rule %d: port must be in 1-65535, got %d", i, r.Port)
		}
		for j := range i {
			if IngressClash(rules[j], r) {
				return fmt.Errorf("ingress rule %d clashes with rule %d: same %s/%d on %s from overlapping sources", i, j, r.Protocol, r.Port, r.Iface)
			}
		}
	}
	return nil
}

// IngressClash reports whether one inbound packet could match both rules.
// Both must already be valid.
func IngressClash(a, b types.IngressRule) bool {
	if a.Iface != b.Iface || a.Protocol != b.Protocol || a.Port != b.Port {
		return false
	}
	na, nb := asNet(a.SrcIP), asNet(b.SrcIP)
	return na.Contains(nb.IP) || nb.Contains(na.IP)
}

// asNet reads a validated address-or-CIDR as a network (an address is a /32).
func asNet(s string) *net.IPNet {
	if _, n, err := net.ParseCIDR(s); err == nil {
		return n
	}
	return &net.IPNet{IP: net.ParseIP(s).To4(), Mask: net.CIDRMask(32, 32)}
}

// ValidateIfaceName vets an operator-supplied interface name at startup, before
// it can reach the nft script. The operator is trusted; a typo that happens to
// contain a newline is not.
func ValidateIfaceName(name string) error {
	if name == "" || len(name) > 15 { // IFNAMSIZ - 1
		return fmt.Errorf("interface name %q must be 1-15 characters", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("interface name %q may only contain letters, digits, '-', '_' and '.'", name)
		}
	}
	return nil
}

// isIPv4OrCIDR reports whether s is exactly an IPv4 address or IPv4 CIDR in
// canonical form (what net re-renders it as), so nothing extra can ride along
// into the nft script.
func isIPv4OrCIDR(s string) bool {
	if ip := net.ParseIP(s); ip != nil {
		return ip.To4() != nil && ip.String() == s
	}
	ip, ipnet, err := net.ParseCIDR(s)
	if err != nil || ip.To4() == nil {
		return false
	}
	return ipnet.String() == s
}
