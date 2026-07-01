package network

import (
	"fmt"
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

	// guest → host: drop new connections coming in from any bridge, but let
	// established/related through so host-initiated flows (e.g. SSH into a VM)
	// still get their replies.
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority 0; policy accept;\n")
	b.WriteString("\t\tct state established,related accept\n")
	b.WriteString("\t\tiifname @mhbridges drop\n")
	b.WriteString("\t}\n")

	b.WriteString("\tchain forward {\n")
	b.WriteString("\t\ttype filter hook forward priority 0; policy accept;\n")
	b.WriteString("\t\tct state established,related accept\n")
	// Cross-segment isolation: drop forwarding between two DIFFERENT bridges.
	// Done per ordered pair (not `iifname @s oifname @s`) so it never touches
	// same-bridge traffic, which must stay reachable even if br_netfilter is on.
	for _, a := range networks {
		for _, c := range networks {
			if a.Bridge != c.Bridge {
				fmt.Fprintf(&b, "\t\tiifname \"%s\" oifname \"%s\" drop\n", a.Bridge, c.Bridge)
			}
		}
	}
	// No-egress networks: drop anything leaving the bridge towards the WAN
	// (oifname not one of our bridges). Same-bridge and cross-segment aren't
	// matched here (their oifname IS a bridge); cross-segment is already
	// dropped above.
	for _, n := range networks {
		if !n.Egress {
			fmt.Fprintf(&b, "\t\tiifname \"%s\" oifname != @mhbridges drop\n", n.Bridge)
		}
	}
	b.WriteString("\t}\n")

	// NAT: masquerade egress networks' subnets when leaving towards the WAN.
	b.WriteString("\tchain postrouting {\n")
	b.WriteString("\t\ttype nat hook postrouting priority 100; policy accept;\n")
	for _, n := range networks {
		if n.Egress {
			fmt.Fprintf(&b, "\t\tip saddr %s oifname != @mhbridges masquerade\n", n.Subnet)
		}
	}
	b.WriteString("\t}\n")

	b.WriteString("}\n")
	return b.String()
}
