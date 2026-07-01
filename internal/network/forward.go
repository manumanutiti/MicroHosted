package network

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

const ipForwardPath = "/proc/sys/net/ipv4/ip_forward"

// EnsureIPForward turns on IPv4 forwarding on the host. Egress networks NAT the
// guest subnet out to the WAN (see the masquerade rule in ApplyNftables), but
// the masquerade only fires on traffic the host actually routes — with
// ip_forward=0 the kernel drops guest→WAN packets before nftables' nat hook
// ever sees them. Enabling it is what makes an egress network reach the
// internet. Done from the daemon (integrated), not left as a host-prep step the
// operator has to remember, and only when an egress network exists.
//
// It's a no-op if forwarding is already on, and it never turns forwarding OFF:
// a host may have it enabled for its own reasons, and a network delete
// shouldn't silently break that.
func EnsureIPForward() error {
	current, err := os.ReadFile(ipForwardPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", ipForwardPath, err)
	}
	if strings.TrimSpace(string(current)) == "1" {
		return nil
	}
	if err := os.WriteFile(ipForwardPath, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enabling ip_forward: %w", err)
	}
	return nil
}

// EnsureDockerForwarding makes egress work on hosts running Docker. Docker
// installs a FORWARD chain in the iptables-managed `ip filter` table with
// `policy drop`; since every base chain on a hook is evaluated and a drop is
// final, that policy kills our forwarded egress traffic even though our own
// `inet microhosted` table accepts it. Docker provides the DOCKER-USER chain
// (jumped to first, before its own drop) precisely so external tools can allow
// their own traffic — the same coexistence pattern libvirt uses.
//
// We add a blanket accept for our bridges (mhbr+) there. This does NOT weaken
// isolation: our inet forward chain still runs and its drops (guest→host,
// cross-segment, no-egress→WAN) still win, because a drop in any base chain is
// final. DOCKER-USER's accept only stops Docker's blanket drop from pre-empting
// our finer policy. No-op on hosts without Docker.
//
// Uses the `iptables` frontend (iptables-nft on modern hosts) because that's
// what Docker manages DOCKER-USER with; if Docker is present so is iptables.
func EnsureDockerForwarding() error {
	if exec.Command("iptables", "-t", "filter", "-L", "DOCKER-USER").Run() != nil {
		return nil // no DOCKER-USER chain → Docker not managing FORWARD here.
	}

	// mhbr+ is iptables' wildcard for "any interface named mhbr...", so these
	// two static rules cover every current and future bridge — no per-network
	// bookkeeping. -C checks presence so re-applying never duplicates them.
	// The table selector (-t filter) must precede the command (-C/-I) or
	// iptables misparses the following token as a rule number.
	for _, dir := range []string{"-i", "-o"} {
		rule := []string{"DOCKER-USER", dir, "mhbr+", "-j", "ACCEPT"}
		if exec.Command("iptables", append([]string{"-t", "filter", "-C"}, rule...)...).Run() == nil {
			continue // already present
		}
		if out, err := exec.Command("iptables", append([]string{"-t", "filter", "-I"}, rule...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("adding DOCKER-USER %s accept: %w (%s)", dir, err, strings.TrimSpace(string(out)))
		}
		log.Printf("network: added DOCKER-USER accept for mhbr+ (%s) to coexist with Docker's FORWARD drop", dir)
	}
	return nil
}
