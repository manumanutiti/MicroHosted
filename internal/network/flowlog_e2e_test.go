package network

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"microhosted/pkg/types"
)

// TestFlowLogEndToEnd drives the real kernel: a bridge of ours, a "guest" in
// its own network namespace on it, the ruleset with the flow log, and the
// reader. The guest tries the host, an outside address it may not reach and
// one it may; the first two must be dropped and recorded with their reason,
// the third let through unrecorded. It changes the host's networking (a bridge, a netns, the
// microhosted table), so it runs only when asked, as root, in a throwaway
// namespace or container:
//
//	MH_NET_E2E=1 go test ./internal/network -run EndToEnd
func TestFlowLogEndToEnd(t *testing.T) {
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

	const bridge, guestIP = "mhbre2e0", "10.250.0.2"
	sh("ip link add " + bridge + " type bridge && ip addr add 10.250.0.1/24 dev " + bridge + " && ip link set " + bridge + " up")
	sh("ip netns add mhguest && ip link add mhv0 type veth peer name mhv1 && ip link set mhv1 netns mhguest")
	sh("ip link set mhv0 master " + bridge + " up")
	sh("ip -n mhguest addr add " + guestIP + "/24 dev mhv1 && ip -n mhguest link set mhv1 up && ip -n mhguest link set lo up")
	sh("ip -n mhguest route add default via 10.250.0.1")
	sh("sysctl -qw net.ipv4.ip_forward=1")
	t.Cleanup(func() {
		try("nft delete table " + nftTable)
		try("ip netns del mhguest")
		try("ip link del " + bridge)
	})

	nets := []types.Network{{Name: "e2e", Bridge: bridge, Subnet: "10.250.0.0/24", AllowedEgress: []types.EgressRule{
		{IP: "203.0.113.10", Protocol: "tcp", Port: 80},
	}}}
	if !flowLogAvailable() {
		t.Fatal("the kernel refused the flow-log rules")
	}
	if err := ApplyNftables(nets, nil); err != nil {
		t.Fatal(err)
	}

	f := NewFlowLog(DefaultFlowLimits, func(iface, src string) (string, string) {
		if iface == bridge && src == guestIP {
			return "e2e", "guest"
		}
		return "", ""
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	time.Sleep(300 * time.Millisecond) // bound before the guest sends

	// Each attempt is one SYN (and its retries) that the ruleset drops.
	try("ip netns exec mhguest timeout 2 sh -c 'echo > /dev/tcp/10.250.0.1/22' 2>/dev/null || ip netns exec mhguest timeout 2 nc -w1 10.250.0.1 22")
	try("ip netns exec mhguest timeout 2 sh -c 'echo > /dev/tcp/203.0.113.9/443' 2>/dev/null || ip netns exec mhguest timeout 2 nc -w1 203.0.113.9 443")
	try("ip netns exec mhguest timeout 2 sh -c 'echo > /dev/tcp/203.0.113.10/80' 2>/dev/null || ip netns exec mhguest timeout 2 nc -w1 203.0.113.10 80")

	deadline := time.Now().Add(5 * time.Second)
	var got types.FlowList
	for time.Now().Before(deadline) {
		if got = f.ByVM("guest"); len(got.Flows) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	seen := map[string]types.Flow{}
	for _, fl := range got.Flows {
		seen[fl.Reason+" "+fl.Dst] = fl
	}
	for _, want := range []string{"host 10.250.0.1", "egress 203.0.113.9"} {
		fl, ok := seen[want]
		if !ok {
			t.Errorf("no record %q in %+v", want, got.Flows)
			continue
		}
		if fl.Verdict != "drop" || fl.Protocol != "tcp" || fl.Src != guestIP || fl.Network != "e2e" || fl.Iface != bridge {
			t.Errorf("record %q: %+v", want, fl)
		}
	}
	if fl := seen["host 10.250.0.1"]; fl.DstPort != 22 {
		t.Errorf("host record port %d, want 22", fl.DstPort)
	}
	if fl := seen["egress 203.0.113.9"]; fl.DstPort != 443 {
		t.Errorf("egress record port %d, want 443", fl.DstPort)
	}
	if len(got.Flows) != 2 {
		t.Errorf("want exactly the two attempts, got %+v", got.Flows)
	}
	for _, fl := range got.Flows {
		t.Logf("recorded: %s %s %s:%d ×%d", fl.Reason, fl.Protocol, fl.Dst, fl.DstPort, fl.Count)
	}
}
