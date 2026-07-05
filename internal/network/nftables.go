package network

import (
	"fmt"
	"net"
	"os/exec"
	"strings"

	"microhosted/pkg/types"
)

// nftTable is the single table this daemon owns. Living in its own `inet`
// table (not editing the host's filter tables) means our base chains coexist
// with whatever firewall the host already runs — we add rules, we never
// clobber theirs.
const nftTable = "inet microhosted"

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
func ApplyNftables(networks []types.Network) error {
	script := renderNftables(networks)
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
func renderNftables(networks []types.Network) string {
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
	b.WriteString("\t}\n")

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
func ValidateEgressRules(rules []types.EgressRule) error {
	for i, r := range rules {
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
