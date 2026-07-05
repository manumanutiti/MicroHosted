package vm

import (
	"microhosted/internal/storage"
	"microhosted/pkg/types"
)

// Observability accessors: the manager-owned facts the /v1/system and
// /v1/health endpoints report. The API layer composes these with host metrics
// (internal/hostinfo) — the manager only surfaces what it already knows, it
// doesn't measure the host.

// Facts are the immutable configuration facts the manager was built with,
// plus the firecracker capability probed at startup. No lock needed: none of
// this changes after NewManager.
type Facts struct {
	StoreDir       string
	ChrootBase     string
	FirecrackerBin string
	JailerBin      string
	// NetworkOverridesOK: firecracker >= 1.12, simultaneous forks possible.
	NetworkOverridesOK bool
}

// Facts returns the manager's configuration facts.
func (m *Manager) Facts() Facts {
	return Facts{
		StoreDir:           m.instancesDir,
		ChrootBase:         m.jailerCfg.ChrootBaseDir,
		FirecrackerBin:     m.jailerCfg.ExecFile,
		JailerBin:          m.jailerCfg.JailerBinary,
		NetworkOverridesOK: m.netOverridesOK,
	}
}

// FleetStats snapshots everything the manager tracks, under one lock
// acquisition: VM counts by state, the aggregate vCPU/RAM promised to the
// running ones (overcommit visibility — judge against the host's totals),
// snapshot and volume counts. Networks are the network manager's to count.
func (m *Manager) FleetStats() types.FleetInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	info := types.FleetInfo{Templates: len(m.catalog.List())}
	info.VMs.Total = len(m.vms)
	for _, v := range m.vms {
		switch v.State {
		case types.VMStateRunning:
			info.VMs.Running++
			info.Allocated.VCPUs += v.Config.VCPUs
			info.Allocated.MemMB += v.Config.MemMB
		case types.VMStateStopped:
			info.VMs.Stopped++
		}
	}
	info.Snapshots = len(m.snaps)
	info.Volumes.Total = len(m.vols)
	for _, vol := range m.vols {
		if vol.AttachedTo != "" {
			info.Volumes.Attached++
		}
	}
	return info
}

// CheckDB probes the persistence layer for the health endpoint.
func (m *Manager) CheckDB() error {
	return m.store.Ping()
}

// StoreIsCoW reports whether the instances store supports reflink clones —
// re-probed on each call (cheap) rather than cached, so a store remounted
// under a live daemon is reported truthfully.
func (m *Manager) StoreIsCoW() bool {
	return storage.SupportsReflink(m.instancesDir)
}
