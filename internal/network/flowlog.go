package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"microhosted/pkg/types"
)

// FlowLimits bounds what the flow log holds, whatever the guests send. The
// ruleset already limits records per guest (flowLogLimit); these bound the
// memory they end up in.
type FlowLimits struct {
	// PerSource is how many distinct destinations are kept per guest. Past
	// it the guest's new destinations are counted (Omitted), not kept: the
	// first ones stay, so trying many more cannot push them out.
	PerSource int
	// Sources is how many guests are kept. Past it the one heard from
	// longest ago is forgotten — typically a VM destroyed long since.
	Sources int
}

// DefaultFlowLimits holds about 1M flows at most, ~200 bytes each.
var DefaultFlowLimits = FlowLimits{PerSource: 256, Sources: 4096}

// FlowResolver names the network and the VM behind a record, from the device
// it came in by and its source address, at the time it arrives — a VM
// destroyed later keeps its records, and the next holder of its address does
// not inherit them. Empty strings when it cannot tell.
type FlowResolver func(iface, src string) (network, vm string)

// FlowLog keeps, per guest, what the ruleset recorded it trying (see
// writeFlowLog): read from NFLOG by Run, aggregated by destination, bounded
// by FlowLimits, and kept in memory only — it starts empty with the daemon.
type FlowLog struct {
	limits  FlowLimits
	resolve FlowResolver
	ifname  func(index uint32) string
	now     func() time.Time

	mu       sync.Mutex
	sources  map[string]*flowSource
	overruns uint64
}

// flowSource is one guest's flows, in the order first seen.
type flowSource struct {
	flows   map[flowKey]*types.Flow
	order   []flowKey
	omitted uint64
	last    time.Time
}

type flowKey struct {
	iface, verdict, reason, proto, dst string
	port                               int
}

// NewFlowLog makes an empty flow log; Run fills it.
func NewFlowLog(limits FlowLimits, resolve FlowResolver) *FlowLog {
	return &FlowLog{
		limits:  limits,
		resolve: resolve,
		ifname:  ifaceName,
		now:     time.Now,
		sources: make(map[string]*flowSource),
	}
}

// Run binds FlowLogGroup and records what arrives until ctx is done. It fails
// only if it cannot bind, or the socket breaks; the policy does not depend on
// it either way.
func (f *FlowLog) Run(ctx context.Context) error {
	c, err := openNFLOG(FlowLogGroup)
	if err != nil {
		return err
	}
	defer c.close()
	buf := make([]byte, 1<<16)
	for ctx.Err() == nil {
		pkts, err := c.read(buf)
		switch {
		case errors.Is(err, errNFLOGTimeout):
			continue
		case errors.Is(err, unix.ENOBUFS):
			f.mu.Lock()
			f.overruns++
			f.mu.Unlock()
			continue
		case err != nil:
			return fmt.Errorf("reading NFLOG group %d: %w", FlowLogGroup, err)
		}
		for _, p := range pkts {
			f.record(p)
		}
	}
	return nil
}

// record adds one packet to its guest's flows. Anything that is not one of
// our records — another prefix, not IPv4 — is ignored.
func (f *FlowLog) record(p nflogPacket) {
	fields := strings.Fields(p.prefix) // "mh <verdict> <reason>"
	if len(fields) != 3 || fields[0] != "mh" {
		return
	}
	verdict, reason := fields[1], fields[2]
	ip, ok := parseIPv4(p.payload)
	if !ok {
		return
	}
	iface := f.ifname(p.indev)
	network, vm := "", ""
	if f.resolve != nil {
		network, vm = f.resolve(iface, ip.src)
	}
	// A guest is its VM; without one (its address no longer held), the
	// address on that device.
	sk := vm
	if sk == "" {
		sk = iface + "/" + ip.src
	}
	key := flowKey{iface: iface, verdict: verdict, reason: reason, proto: ip.proto, dst: ip.dst, port: ip.dstPort}
	now := f.now()

	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sources[sk]
	if !ok {
		if len(f.sources) >= f.limits.Sources {
			f.forgetOldestSource()
		}
		s = &flowSource{flows: make(map[flowKey]*types.Flow)}
		f.sources[sk] = s
	}
	s.last = now
	if fl, ok := s.flows[key]; ok {
		fl.Count++
		fl.Last = now
		return
	}
	if len(s.order) >= f.limits.PerSource {
		s.omitted++
		return
	}
	s.flows[key] = &types.Flow{
		VM: vm, Network: network, Iface: iface,
		Verdict: verdict, Reason: reason,
		Protocol: ip.proto, Src: ip.src, Dst: ip.dst, DstPort: ip.dstPort,
		Count: 1, First: now, Last: now,
	}
	s.order = append(s.order, key)
}

// forgetOldestSource drops the guest heard from longest ago. Linear, but only
// when a new guest arrives with the log full.
func (f *FlowLog) forgetOldestSource() {
	var oldest string
	var at time.Time
	for k, s := range f.sources {
		if oldest == "" || s.last.Before(at) {
			oldest, at = k, s.last
		}
	}
	delete(f.sources, oldest)
}

// ByVM returns what one VM tried.
func (f *FlowLog) ByVM(id string) types.FlowList {
	return f.list(func(fl *types.Flow) bool { return fl.VM == id })
}

// ByNetwork returns what the guests of one network tried, VMs since
// destroyed included.
func (f *FlowLog) ByNetwork(name string) types.FlowList {
	return f.list(func(fl *types.Flow) bool { return fl.Network == name })
}

// list copies the flows that match, oldest first, with the omissions of the
// guests they belong to.
func (f *FlowLog) list(match func(*types.Flow) bool) types.FlowList {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := types.FlowList{Flows: []types.Flow{}, Overruns: f.overruns}
	for _, s := range f.sources {
		matched := false
		for _, k := range s.order {
			if fl := s.flows[k]; match(fl) {
				out.Flows = append(out.Flows, *fl)
				matched = true
			}
		}
		if matched {
			out.Omitted += s.omitted
		}
	}
	slices.SortStableFunc(out.Flows, func(a, b types.Flow) int { return a.First.Compare(b.First) })
	return out
}

// ifaceName names an interface by index, as it is now: a TAP deleted since
// the packet was sent shows as its number.
func ifaceName(index uint32) string {
	if ifi, err := net.InterfaceByIndex(int(index)); err == nil {
		return ifi.Name
	}
	return fmt.Sprintf("if%d", index)
}
