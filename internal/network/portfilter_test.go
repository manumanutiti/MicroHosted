package network

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"microhosted/pkg/types"
)

func TestGuestMAC(t *testing.T) {
	if got := GuestMAC(net.ParseIP("172.16.1.2")); got != "02:00:ac:10:01:02" {
		t.Errorf("GuestMAC = %q", got)
	}
	if got := GuestMAC(net.ParseIP("fe80::1")); got != "" {
		t.Errorf("GuestMAC(IPv6) = %q, want empty", got)
	}
}

func TestRenderPortFilter(t *testing.T) {
	script, err := renderPortFilter([]Port{
		{Tap: "tapbbbbbbbb", MAC: "02:00:ac:10:00:03", IP: "172.16.0.3"},
		{Tap: "tapaaaaaaaa", MAC: "02:00:ac:10:00:02", IP: "172.16.0.2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"table bridge microhosted {}\ndelete table bridge microhosted\n",
		`elements = { "tapaaaaaaaa" . 02:00:ac:10:00:02 . 172.16.0.2, "tapbbbbbbbb" . 02:00:ac:10:00:03 . 172.16.0.3 }`,
		`meta ibrname != "mhbr*" accept`,
		"ether type ip iifname . ether saddr . ip saddr @ports accept",
		"iifname . ether saddr . arp saddr ip @ports iifname . arp saddr ether . arp saddr ip @ports accept",
		"counter drop",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	// The closing drop is the last rule: nothing from a port of ours may fall
	// through to the chain's accept policy.
	if i, j := strings.Index(script, "counter drop"), strings.LastIndex(script, "accept\n"); i < j {
		t.Errorf("a rule follows the closing drop:\n%s", script)
	}
}

// With no VMs the table (and its closing drop) is still installed, with an
// empty set rather than an invalid empty element list.
func TestRenderPortFilterEmpty(t *testing.T) {
	script, err := renderPortFilter(nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "elements") || !strings.Contains(script, "counter drop") {
		t.Errorf("unexpected empty-port script:\n%s", script)
	}
}

// Nothing that could break out of the nft script (or pin a nonsense pair)
// reaches it.
func TestRenderPortFilterRejectsBadPorts(t *testing.T) {
	for _, p := range []Port{
		{Tap: "tap\"; flush ruleset", MAC: "02:00:ac:10:00:02", IP: "172.16.0.2"},
		{Tap: "tapaaaaaaaa", MAC: "", IP: "172.16.0.2"},
		{Tap: "tapaaaaaaaa", MAC: "02:00:ac:10:00:02", IP: "fe80::1"},
		{Tap: "tapaaaaaaaa", MAC: "02:00:ac:10:00:02:00:00", IP: "172.16.0.2"},
	} {
		if _, err := renderPortFilter([]Port{p}); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
}

// A lease is pinned before AttachVM returns, and unpinned by DetachVM.
func TestAttachPinsPort(t *testing.T) {
	h := withFakeHost(t)
	m, _ := newTestNetManager(t)
	if _, err := m.Create(types.CreateNetworkRequest{Name: "lab", Subnet: "10.9.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	ip, _, _, _, err := m.AttachVM("lab", "deadbeef", "tapdeadbeef")
	if err != nil {
		t.Fatal(err)
	}
	last := h.ports[len(h.ports)-1]
	want := Port{Tap: "tapdeadbeef", MAC: GuestMAC(net.ParseIP(ip)), IP: ip}
	if len(last) != 1 || last[0] != want {
		t.Fatalf("installed ports = %+v, want [%+v]", last, want)
	}
	m.DetachVM("lab", "deadbeef")
	if last := h.ports[len(h.ports)-1]; len(last) != 0 {
		t.Errorf("ports after detach = %+v, want none", last)
	}
}

// If the filter cannot pin the port, the attach fails and leaves nothing:
// the address is free again and the network is not held.
func TestAttachUndoneWhenPortFilterFails(t *testing.T) {
	h := withFakeHost(t)
	m, _ := newTestNetManager(t)
	if _, err := m.Create(types.CreateNetworkRequest{Name: "lab", Subnet: "10.9.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	h.failPorts = true
	if _, _, _, _, err := m.AttachVM("lab", "deadbeef", "tapdeadbeef"); err == nil {
		t.Fatal("attach succeeded without the port filter")
	}
	if _, _, _, err := m.ClaimVM("lab", "cafebabe", "10.9.0.7", "tapcafebabe"); err == nil {
		t.Fatal("claim succeeded without the port filter")
	}
	if leases := m.Leases()["lab"]; len(leases) != 0 {
		t.Errorf("leases left after failed attaches: %v", leases)
	}
	if err := m.RequirePorts(); err == nil {
		t.Error("RequirePorts passed with the filter failing")
	}

	h.failPorts = false
	if err := m.RequirePorts(); err != nil {
		t.Errorf("RequirePorts did not retry the install: %v", err)
	}
	if _, _, _, err := m.ClaimVM("lab", "cafebabe", "10.9.0.7", "tapcafebabe"); err != nil {
		t.Errorf("the address was not released by the failed claim: %v", err)
	}
}

// The rendered port filter must parse (same constraint as
// TestRenderedRulesetParses): sudo go test ./internal/network -run Parses
func TestRenderedPortFilterParses(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for nft -c")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	for name, ports := range map[string][]Port{
		"empty": nil,
		"two": {
			{Tap: "tapaaaaaaaa", MAC: "02:00:ac:10:00:02", IP: "172.16.0.2"},
			{Tap: "tapbbbbbbbb", MAC: "02:00:ac:10:09:02", IP: "172.16.9.2"},
		},
	} {
		script, err := renderPortFilter(ports)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("nft", "-c", "-f", "-")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: nft -c rejected the port filter: %v\n%s", name, err, out)
		}
	}
}
