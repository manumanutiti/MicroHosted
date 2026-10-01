package network

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"microhosted/pkg/types"
)

// TestEgressPortsEndToEnd drives the real kernel: a guest on a full-egress
// network narrowed to tcp:443 tries 443 (forwarded), 22, udp 53 and ICMP to
// the outside, and the LAN on 443 — the four must be dropped, each recorded
// with its reason. Like TestFlowLogEndToEnd it changes the host's networking,
// so it runs only when asked, as root, in a throwaway network namespace whose
// way out is eth0:
//
//	MH_NET_E2E=1 go test ./internal/network -run EgressPortsEndToEnd
func TestEgressPortsEndToEnd(t *testing.T) {
	if os.Getenv("MH_NET_E2E") != "1" || os.Geteuid() != 0 {
		t.Skip("set MH_NET_E2E=1 and run as root, in a throwaway network namespace")
	}
	sh := func(cmd string) {
		t.Helper()
		if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
	}
	try := func(cmd string) { _ = exec.Command("sh", "-c", cmd).Run() }

	const bridge, guestIP = "mhbre2e1", "10.251.0.2"
	sh("ip link add " + bridge + " type bridge && ip addr add 10.251.0.1/24 dev " + bridge + " && ip link set " + bridge + " up")
	sh("ip netns add mhguest2 && ip link add mhw0 type veth peer name mhw1 && ip link set mhw1 netns mhguest2")
	sh("ip link set mhw0 master " + bridge + " up")
	sh("ip -n mhguest2 addr add " + guestIP + "/24 dev mhw1 && ip -n mhguest2 link set mhw1 up && ip -n mhguest2 route add default via 10.251.0.1")
	sh("sysctl -qw net.ipv4.ip_forward=1")
	t.Cleanup(func() {
		try("nft delete table " + nftTable)
		try("ip netns del mhguest2")
		try("ip link del " + bridge)
	})

	nets := []types.Network{{Name: "fetch", Bridge: bridge, Subnet: "10.251.0.0/24", Egress: true, EgressIface: "eth0",
		EgressPorts: []types.PortRule{{Protocol: "tcp", Port: 443}}}}
	if err := ApplyNftables(nets, nil); err != nil {
		t.Fatal(err)
	}
	f := NewFlowLog(DefaultFlowLimits, func(iface, src string) (string, string) { return "fetch", "guest" })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	g := "ip netns exec mhguest2 "
	try(g + "timeout 2 nc -w1 203.0.113.9 443") // allowed: leaves (and gets no answer)
	try(g + "timeout 2 nc -w1 203.0.113.9 22")  // port
	try(g + "timeout 2 nc -u -w1 203.0.113.9 53 </dev/null; echo x | " + g + "timeout 2 nc -u -w1 203.0.113.9 53")
	try(g + "ping -c1 -W1 203.0.113.9")         // icmp
	try(g + "timeout 2 nc -w1 192.168.0.1 443") // the LAN, on an allowed port
	time.Sleep(500 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	seen := map[string]bool{}
	for _, fl := range f.ByVM("guest").Flows {
		seen[fl.Reason+" "+fl.Protocol+" "+fl.Dst] = true
		t.Logf("recorded: %s %s %s:%d ×%d", fl.Reason, fl.Protocol, fl.Dst, fl.DstPort, fl.Count)
		if fl.Protocol == "tcp" && fl.Dst == "203.0.113.9" && fl.DstPort == 443 {
			t.Errorf("tcp:443 to the internet was dropped: %+v", fl)
		}
	}
	for _, want := range []string{"port tcp 203.0.113.9", "port udp 203.0.113.9", "port icmp 203.0.113.9", "private tcp 192.168.0.1"} {
		if !seen[want] {
			t.Errorf("no record %q", want)
		}
	}
}
