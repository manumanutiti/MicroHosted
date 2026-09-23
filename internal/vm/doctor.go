package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"microhosted/internal/jailer"
	"microhosted/internal/network"
	"microhosted/pkg/types"
)

// Doctor compares what the manager believes exists against what the host
// has, and reports every disagreement: residue nothing owns (an orphan) and
// resources a tracked VM or network should have and does not (missing). It
// changes nothing — startup's Reconcile and SweepResidue are what clean up —
// so it is safe to run at any time, and it is how a fault test proves a
// failure left the host exactly as it found it.
func (m *Manager) Doctor() types.DoctorReport {
	rep := types.DoctorReport{Findings: []types.DoctorFinding{}}
	add := func(kind, object, format string, args ...any) {
		rep.Findings = append(rep.Findings, types.DoctorFinding{Kind: kind, Object: object, Detail: fmt.Sprintf(format, args...)})
	}

	// One consistent view of what the manager tracks.
	m.mu.Lock()
	busy := make(map[string]bool, len(m.busy))
	for id, op := range m.busy {
		busy[id] = true
		rep.InFlight = append(rep.InFlight, id+": "+op)
	}
	vms := make(map[string]types.VM, len(m.vms))
	for id, v := range m.vms {
		if !busy[id] {
			vms[id] = *v
		}
	}
	snaps := make(map[string]bool, len(m.snaps))
	for id := range m.snaps {
		snaps[id] = true
	}
	var claims []types.Volume
	for _, vol := range m.vols {
		if vol.AttachedTo != "" {
			claims = append(claims, *vol)
		}
	}
	m.mu.Unlock()
	sort.Strings(rep.InFlight)

	running := make(map[string]types.VM)
	for id, v := range vms {
		if v.State == types.VMStateRunning {
			running[id] = v
		}
	}
	// Anything with a record — in memory or, for an interrupted create, only
	// in the store — owns its disk and log; only the rest is orphaned.
	owned := func(id string) bool {
		_, ok := vms[id]
		return ok || busy[id]
	}

	// Records the store holds that the manager does not: an interrupted
	// create the next start will undo, or a record that failed to load.
	if recs, err := m.store.ListVMs(); err != nil {
		add("store_unreadable", "vms", "listing VM records: %v", err)
	} else {
		for _, r := range recs {
			id := r.Config.ID
			if busy[id] {
				continue
			}
			if _, ok := vms[id]; !ok {
				if r.State == types.VMStateCreating {
					add("record_interrupted", id, "create never finished and was not undone; the next daemon start undoes it")
				} else {
					add("record_untracked", id, "stored as %s but not tracked by the running daemon", r.State)
				}
				vms[id] = *r // its disk and log are accounted for below
			}
		}
	}

	// Processes.
	procs := m.findVMProcesses()
	for id, pid := range procs {
		if busy[id] {
			continue
		}
		v, ok := running[id]
		switch {
		case !ok:
			add("process_orphan", id, "firecracker pid %d runs for a VM that is not running", pid)
		case v.PID != pid:
			add("process_orphan", id, "firecracker pid %d runs, the VM record says pid %d", pid, v.PID)
		}
	}
	for id, v := range running {
		if _, ok := procs[id]; !ok && !processAlive(v.PID, id) {
			add("process_missing", id, "recorded as running (pid %d) but no process exists", v.PID)
		}
	}

	// TAPs: only a running VM holds one.
	wantTaps := make(map[string]string)
	for id, v := range running {
		if v.Config.TapDevice != "" {
			wantTaps[v.Config.TapDevice] = id
		}
	}
	if taps, err := network.ListTaps(); err != nil {
		add("host_unreadable", "links", "listing taps: %v", err)
	} else {
		have := make(map[string]bool, len(taps))
		for _, t := range taps {
			have[t] = true
			if _, ok := wantTaps[t]; !ok && !busy[strings.TrimPrefix(t, "tap")] {
				add("tap_orphan", t, "tap device belongs to no running VM")
			}
		}
		for t, id := range wantTaps {
			if !have[t] {
				add("tap_missing", id, "running VM's tap %s does not exist", t)
			}
		}
	}

	// Bridges and address leases.
	nets := m.netmgr.List()
	wantBridges := make(map[string]string, len(nets))
	for _, n := range nets {
		wantBridges[n.Bridge] = n.Name
	}
	if bridges, err := network.ListBridges(); err != nil {
		add("host_unreadable", "links", "listing bridges: %v", err)
	} else {
		have := make(map[string]bool, len(bridges))
		for _, b := range bridges {
			have[b] = true
			if _, ok := wantBridges[b]; !ok {
				add("bridge_orphan", b, "bridge belongs to no network")
			}
		}
		for b, name := range wantBridges {
			if !have[b] {
				add("bridge_missing", name, "network's bridge %s does not exist", b)
			}
		}
	}
	for netName, ids := range m.netmgr.Leases() {
		for _, id := range ids {
			if !owned(id) {
				add("lease_orphan", id, "holds an address on network %s but no such VM exists", netName)
			}
		}
	}
	if st := m.netmgr.Rules(); !st.At.IsZero() && !st.OK {
		add("ruleset_failed", "nftables", "the last ruleset install failed at %s: the policy in force may not be the declared one: %s",
			st.At.Format("2006-01-02T15:04:05Z07:00"), firstLine(st.Err))
	}

	// Jail dirs and cgroups: only a running VM has them.
	for _, id := range jailer.InstanceIDs(m.jailerCfg) {
		if _, ok := running[id]; !ok && !busy[id] {
			add("jail_orphan", id, "jail dir %s belongs to no running VM", jailer.InstanceDir(m.jailerCfg, id))
		}
	}
	for _, id := range jailer.CgroupIDs(m.jailerCfg) {
		if _, ok := running[id]; !ok && !busy[id] {
			add("cgroup_orphan", id, "cgroup belongs to no running VM")
		}
	}

	// Disks, logs and snapshot dirs in the store.
	if entries, err := os.ReadDir(m.instancesDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			for _, ext := range []string{".ext4", ".log"} {
				id, ok := strings.CutSuffix(name, ext)
				if ok && jailer.IsVMID(id) && !owned(id) {
					kind := "clone_orphan"
					if ext == ".log" {
						kind = "log_orphan"
					}
					add(kind, filepath.Join(m.instancesDir, name), "no VM %s owns it", id)
				}
			}
		}
	}
	for id, v := range vms {
		if v.State == types.VMStateCreating || v.Config.Rootfs == "" {
			continue
		}
		if _, err := os.Stat(v.Config.Rootfs); err != nil {
			add("disk_missing", id, "VM's disk %s is gone", v.Config.Rootfs)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(m.instancesDir, "snapshots")); err == nil {
		for _, e := range entries {
			if e.IsDir() && jailer.IsVMID(e.Name()) && !snaps[e.Name()] {
				add("snapshot_dir_orphan", filepath.Join(m.instancesDir, "snapshots", e.Name()), "no snapshot record owns it")
			}
		}
	}

	for _, vol := range claims {
		if !owned(vol.AttachedTo) {
			add("volume_claim_orphan", vol.Name, "claimed by vm %s, which does not exist", vol.AttachedTo)
		}
	}

	sort.Slice(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Object < b.Object
	})
	rep.Clean = len(rep.Findings) == 0
	return rep
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
