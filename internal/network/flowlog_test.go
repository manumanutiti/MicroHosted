package network

import (
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"microhosted/pkg/types"
)

// ipv4Packet builds the start of an IPv4 packet as the kernel copies it.
func ipv4Packet(proto byte, src, dst string, dport int) []byte {
	b := make([]byte, 24)
	b[0] = 0x45 // version 4, 20-byte header
	b[9] = proto
	copy(b[12:16], net.ParseIP(src).To4())
	copy(b[16:20], net.ParseIP(dst).To4())
	binary.BigEndian.PutUint16(b[20:22], 40000) // source port
	binary.BigEndian.PutUint16(b[22:24], uint16(dport))
	return b
}

// nflogDatagram builds what recv returns for one NFLOG record.
func nflogDatagram(prefix string, indev uint32, payload []byte) []byte {
	idx := make([]byte, 4)
	binary.BigEndian.PutUint32(idx, indev)
	var attrs []byte
	attrs = append(attrs, nlAttr(nfulaPrefix, append([]byte(prefix), 0))...)
	attrs = append(attrs, nlAttr(nfulaIfindexIndev|0x4000, idx)...) // with NLA_F_NET_BYTEORDER
	attrs = append(attrs, nlAttr(nfulaPayload, payload)...)
	return nfnlMessage(nfnlSubsysULOG<<8|nfulnlMsgPacket, 0, 0, unix.AF_INET, FlowLogGroup, attrs)
}

func TestParseNFLOGDatagram(t *testing.T) {
	pkt := ipv4Packet(unix.IPPROTO_TCP, "172.16.9.2", "45.142.1.1", 443)
	dgram := append(nflogDatagram("mh drop egress", 7, pkt), nflogDatagram("mh drop host", 8, pkt)...)

	msgs := splitNetlink(dgram)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	p, ok := parseNFLOGPacket(msgs[0].data)
	if !ok || p.prefix != "mh drop egress" || p.indev != 7 || string(p.payload) != string(pkt) {
		t.Fatalf("parsed %+v, %v", p, ok)
	}
	if p, _ := parseNFLOGPacket(msgs[1].data); p.prefix != "mh drop host" || p.indev != 8 {
		t.Fatalf("second message parsed %+v", p)
	}
}

func TestParseIPv4(t *testing.T) {
	frag := ipv4Packet(unix.IPPROTO_UDP, "10.0.0.2", "10.0.0.9", 53)
	binary.BigEndian.PutUint16(frag[6:8], 185) // fragment offset: no port in it
	opts := append(ipv4Packet(unix.IPPROTO_TCP, "10.0.0.2", "10.0.0.9", 0)[:20:20], 1, 1, 1, 1, 0x9c, 0x40, 0x01, 0xbb)
	opts[0] = 0x46 // 24-byte header: the port comes after the options

	for name, c := range map[string]struct {
		pkt  []byte
		want ipv4Summary
		ok   bool
	}{
		"tcp":      {ipv4Packet(unix.IPPROTO_TCP, "10.0.0.2", "1.2.3.4", 443), ipv4Summary{"tcp", "10.0.0.2", "1.2.3.4", 443}, true},
		"udp":      {ipv4Packet(unix.IPPROTO_UDP, "10.0.0.2", "8.8.8.8", 53), ipv4Summary{"udp", "10.0.0.2", "8.8.8.8", 53}, true},
		"icmp":     {ipv4Packet(unix.IPPROTO_ICMP, "10.0.0.2", "1.1.1.1", 0)[:20], ipv4Summary{"icmp", "10.0.0.2", "1.1.1.1", 0}, true},
		"other":    {ipv4Packet(47, "10.0.0.2", "1.1.1.1", 0)[:20], ipv4Summary{"47", "10.0.0.2", "1.1.1.1", 0}, true},
		"fragment": {frag, ipv4Summary{"udp", "10.0.0.2", "10.0.0.9", 0}, true},
		"options":  {opts, ipv4Summary{"tcp", "10.0.0.2", "10.0.0.9", 443}, true},
		"no port":  {ipv4Packet(unix.IPPROTO_TCP, "10.0.0.2", "1.2.3.4", 443)[:21], ipv4Summary{"tcp", "10.0.0.2", "1.2.3.4", 0}, true},
		"short":    {make([]byte, 19), ipv4Summary{}, false},
		"ipv6":     {append([]byte{0x60}, make([]byte, 39)...), ipv4Summary{}, false},
		"bad ihl":  {append([]byte{0x4f}, make([]byte, 23)...), ipv4Summary{}, false},
	} {
		got, ok := parseIPv4(c.pkt)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v, %v", name, got, ok, c.want, c.ok)
		}
	}
}

// What the kernel hands over is trusted framing, but the payload is the
// guest's: no input may panic the parsers.
func FuzzParseNFLOG(f *testing.F) {
	f.Add(nflogDatagram("mh drop egress", 7, ipv4Packet(unix.IPPROTO_TCP, "10.0.0.2", "1.2.3.4", 443)))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0x10, 0, 0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, m := range splitNetlink(b) {
			if p, ok := parseNFLOGPacket(m.data); ok {
				parseIPv4(p.payload)
			}
		}
		parseNFLOGPacket(b)
		parseIPv4(b)
	})
}

// testFlowLog is a flow log with a fixed clock, interfaces named by number,
// and a resolver from a table keyed "iface src".
func testFlowLog(limits FlowLimits, owners map[string][2]string) (*FlowLog, *time.Time) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	f := NewFlowLog(limits, func(iface, src string) (string, string) {
		o := owners[iface+" "+src]
		return o[0], o[1]
	})
	f.ifname = func(i uint32) string { return fmt.Sprintf("mhbr%04d", i) }
	f.now = func() time.Time { return now }
	return f, &now
}

func drop(reason string, indev uint32, proto byte, src, dst string, port int) nflogPacket {
	return nflogPacket{prefix: "mh drop " + reason, indev: indev, payload: ipv4Packet(proto, src, dst, port)}
}

func TestFlowLogAggregatesByDestination(t *testing.T) {
	f, now := testFlowLog(DefaultFlowLimits, map[string][2]string{"mhbr0001 172.16.9.2": {"lab", "vm-a"}})
	first := *now
	for range 3 {
		f.record(drop("egress", 1, unix.IPPROTO_TCP, "172.16.9.2", "45.142.1.1", 443))
		*now = now.Add(time.Second)
	}
	f.record(drop("host", 1, unix.IPPROTO_TCP, "172.16.9.2", "172.16.9.1", 22))

	got := f.ByVM("vm-a")
	if len(got.Flows) != 2 {
		t.Fatalf("got %d flows, want 2: %+v", len(got.Flows), got.Flows)
	}
	want := types.Flow{
		VM: "vm-a", Network: "lab", Iface: "mhbr0001", Verdict: "drop", Reason: "egress",
		Protocol: "tcp", Src: "172.16.9.2", Dst: "45.142.1.1", DstPort: 443,
		Count: 3, First: first, Last: first.Add(2 * time.Second),
	}
	if got.Flows[0] != want {
		t.Errorf("first flow = %+v\nwant          %+v", got.Flows[0], want)
	}
	if got.Flows[1].Reason != "host" || got.Flows[1].Count != 1 {
		t.Errorf("second flow = %+v", got.Flows[1])
	}
	if n := f.ByNetwork("lab"); len(n.Flows) != 2 {
		t.Errorf("by network: %+v", n)
	}
	if n := f.ByVM("other"); n.Flows == nil || len(n.Flows) != 0 {
		t.Errorf("a VM with no flows must list none, as an empty list: %#v", n.Flows)
	}
}

// Past PerSource the first destinations stay and the rest are counted: a
// guest cannot erase its earliest attempts by trying many more.
func TestFlowLogKeepsFirstDestinations(t *testing.T) {
	f, _ := testFlowLog(FlowLimits{PerSource: 3, Sources: 10}, map[string][2]string{"mhbr0001 10.0.0.2": {"lab", "vm-a"}})
	for i := range 10 {
		f.record(drop("egress", 1, unix.IPPROTO_TCP, "10.0.0.2", fmt.Sprintf("1.1.1.%d", i), 443))
	}
	f.record(drop("egress", 1, unix.IPPROTO_TCP, "10.0.0.2", "1.1.1.0", 443)) // known: still counted

	got := f.ByVM("vm-a")
	if len(got.Flows) != 3 || got.Omitted != 7 {
		t.Fatalf("got %d flows, %d omitted; want 3, 7", len(got.Flows), got.Omitted)
	}
	for i, fl := range got.Flows {
		if fl.Dst != fmt.Sprintf("1.1.1.%d", i) {
			t.Errorf("flow %d is %s: the first destinations must be the ones kept", i, fl.Dst)
		}
	}
	if got.Flows[0].Count != 2 {
		t.Errorf("a kept destination stopped counting: %+v", got.Flows[0])
	}
}

// Past Sources the guest heard from longest ago goes.
func TestFlowLogForgetsOldestSource(t *testing.T) {
	f, now := testFlowLog(FlowLimits{PerSource: 10, Sources: 2}, map[string][2]string{
		"mhbr0001 10.0.0.2": {"lab", "vm-a"},
		"mhbr0001 10.0.0.3": {"lab", "vm-b"},
		"mhbr0001 10.0.0.4": {"lab", "vm-c"},
	})
	f.record(drop("egress", 1, unix.IPPROTO_TCP, "10.0.0.2", "1.1.1.1", 443))
	*now = now.Add(time.Second)
	f.record(drop("egress", 1, unix.IPPROTO_TCP, "10.0.0.3", "1.1.1.1", 443))
	*now = now.Add(time.Second)
	f.record(drop("egress", 1, unix.IPPROTO_TCP, "10.0.0.2", "1.1.1.1", 443)) // vm-a heard again
	*now = now.Add(time.Second)
	f.record(drop("egress", 1, unix.IPPROTO_TCP, "10.0.0.4", "1.1.1.1", 443))

	if len(f.ByVM("vm-b").Flows) != 0 {
		t.Error("vm-b, heard from longest ago, should have been forgotten")
	}
	if len(f.ByVM("vm-a").Flows) != 1 || len(f.ByVM("vm-c").Flows) != 1 {
		t.Error("the guests heard from recently must be kept")
	}
}

// A record is attributed when it arrives: one from an address no VM holds is
// kept apart, and is not handed to the VM that gets the address later.
func TestFlowLogAttributesAtArrival(t *testing.T) {
	owners := map[string][2]string{"mhbr0001 10.0.0.2": {"lab", ""}}
	f, _ := testFlowLog(DefaultFlowLimits, owners)
	f.record(drop("egress", 1, unix.IPPROTO_TCP, "10.0.0.2", "1.1.1.1", 443))
	owners["mhbr0001 10.0.0.2"] = [2]string{"lab", "vm-new"}
	f.record(drop("egress", 1, unix.IPPROTO_TCP, "10.0.0.2", "1.1.1.1", 443))

	if got := f.ByVM("vm-new").Flows; len(got) != 1 || got[0].Count != 1 {
		t.Errorf("the new holder inherited the old record: %+v", got)
	}
	if got := f.ByNetwork("lab").Flows; len(got) != 2 || got[0].VM != "" {
		t.Errorf("the unattributed record must be kept on its network: %+v", got)
	}
}

func TestFlowLogIgnoresForeignRecords(t *testing.T) {
	f, _ := testFlowLog(DefaultFlowLimits, map[string][2]string{"mhbr0001 10.0.0.2": {"lab", "vm-a"}})
	pkt := ipv4Packet(unix.IPPROTO_TCP, "10.0.0.2", "1.1.1.1", 443)
	for _, p := range []nflogPacket{
		{prefix: "ufw block", indev: 1, payload: pkt},
		{prefix: "mh drop", indev: 1, payload: pkt},
		{prefix: "mh drop egress extra", indev: 1, payload: pkt},
		{prefix: "mh drop egress", indev: 1, payload: []byte{0x60, 0, 0}},
	} {
		f.record(p)
	}
	if got := f.ByNetwork("lab").Flows; len(got) != 0 {
		t.Errorf("foreign or malformed records were kept: %+v", got)
	}
}

func TestResolveFlow(t *testing.T) {
	withFakeHost(t)
	m, _ := newTestNetManager(t)
	n, err := m.Create(types.CreateNetworkRequest{Name: "lab", Subnet: "10.9.0.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	ip, _, _, _, err := m.AttachVM("lab", "deadbeef", "tapdeadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if nw, vm := m.ResolveFlow(n.Bridge, ip); nw != "lab" || vm != "deadbeef" {
		t.Errorf("ResolveFlow(bridge, lease) = %q, %q", nw, vm)
	}
	if nw, vm := m.ResolveFlow(n.Bridge, "10.9.0.200"); nw != "lab" || vm != "" {
		t.Errorf("ResolveFlow(bridge, free address) = %q, %q", nw, vm)
	}
	if nw, vm := m.ResolveFlow("tapdeadbeef", ip); nw != "" || vm != "" {
		t.Errorf("a TAP is not a network's to name: %q, %q", nw, vm)
	}
}
