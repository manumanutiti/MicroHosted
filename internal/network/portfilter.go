package network

import (
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
)

// portTable is the bridge-family table that pins each VM's TAP to its own
// addresses. It is separate from nftTable because the inet family never sees
// frames switched inside one bridge; only the bridge family does.
const portTable = "bridge microhosted"

// Port is one VM's bridge port and the only source addresses it may use: the
// MAC Firecracker gives its NIC and the IP its network leased it.
type Port struct {
	Tap string
	MAC string
	IP  string
}

// GuestMAC is the MAC a VM's NIC gets for a guest IP: a locally-administered,
// unicast 02:00 + the four IPv4 octets (172.16.1.2 → 02:00:ac:10:01:02).
// Unique per address, stable across reboots, and — because the port filter
// pins the pair — the only MAC that VM's frames may carry. "" if ip is not
// IPv4.
func GuestMAC(ip net.IP) string {
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	return fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", v4[0], v4[1], v4[2], v4[3])
}

// ApplyPortFilter re-renders the whole port table for ports and installs it
// atomically, like ApplyNftables.
func ApplyPortFilter(ports []Port) error {
	script, err := renderPortFilter(ports)
	if err != nil {
		return err
	}
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("applying port filter: %w\n--- ruleset ---\n%s\n--- nft said ---\n%s", err, script, strings.TrimSpace(string(out)))
	}
	return nil
}

// renderPortFilter produces the `nft -f` script for the port table.
//
// Every port of one of our bridges is a VM's TAP, and a frame entering the
// bridge from it is accepted only if it carries that VM's own addresses:
//
//   - IPv4: source MAC and source IP both the port's;
//   - ARP (Ethernet/IPv4 only): the frame's source MAC, the sender hardware
//     address and the sender IP all the port's — an ARP reply claiming another
//     address is how a VM would steal that address's traffic, host's included;
//   - anything else (IPv6, VLAN tags, other ethertypes) is dropped: networks
//     are IPv4-only and nothing else has a use on them.
//
// A port the table does not list (a TAP enslaved before its lease reached the
// table, or by hand) matches nothing and is dropped whole: unknown is closed,
// as with unknown bridges in the inet table. Bridges that are not ours are
// left alone.
//
// The table is rendered even with no ports, so the closing rule is always in
// force.
func renderPortFilter(ports []Port) (string, error) {
	elems := make([]string, 0, len(ports))
	for _, p := range ports {
		ip := net.ParseIP(p.IP).To4()
		mac, err := net.ParseMAC(p.MAC)
		if ip == nil || err != nil || len(mac) != 6 {
			return "", fmt.Errorf("port %s: invalid address pair %q / %q", p.Tap, p.MAC, p.IP)
		}
		if err := ValidateIfaceName(p.Tap); err != nil {
			return "", fmt.Errorf("port %s: %w", p.Tap, err)
		}
		elems = append(elems, fmt.Sprintf("%q . %s . %s", p.Tap, mac, ip))
	}
	sort.Strings(elems)

	var b strings.Builder
	fmt.Fprintf(&b, "table %s {}\n", portTable)
	fmt.Fprintf(&b, "delete table %s\n", portTable)
	fmt.Fprintf(&b, "table %s {\n", portTable)
	b.WriteString("\tset ports {\n\t\ttype ifname . ether_addr . ipv4_addr\n")
	if len(elems) > 0 {
		fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(elems, ", "))
	}
	b.WriteString("\t}\n")
	b.WriteString("\tchain prerouting {\n")
	b.WriteString("\t\ttype filter hook prerouting priority filter; policy accept;\n")
	fmt.Fprintf(&b, "\t\tmeta ibrname != \"%s*\" accept\n", BridgePrefix)
	b.WriteString("\t\tether type ip iifname . ether saddr . ip saddr @ports accept\n")
	b.WriteString("\t\tether type arp arp htype 1 arp ptype ip arp hlen 6 arp plen 4 " +
		"iifname . ether saddr . arp saddr ip @ports " +
		"iifname . arp saddr ether . arp saddr ip @ports accept\n")
	b.WriteString("\t\tcounter drop\n")
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String(), nil
}
