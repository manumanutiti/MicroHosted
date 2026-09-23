package vm

import (
	"fmt"
	"log"
	"maps"

	"microhosted/internal/network"
	"microhosted/pkg/types"
)

// The label Quarantine sets, so an orchestrator can tell by selection (GET
// /v1/vms?label=lease=quarantined) which VMs it already pulled out of service.
const (
	LeaseLabel       = "lease"
	LeaseQuarantined = "quarantined"
)

// Quarantine cuts a VM off its network in place, without stopping it: its TAP
// leaves the bridge (up, going nowhere — what a quarantined fork gets), its IP
// reservation goes back to the network, and it is marked Quarantine with the
// label lease=quarantined. The guest keeps running, still believing it has its
// address, and stays reachable over vsock (exec, files) for whoever
// investigates it. Its name and the rest of its labels are kept.
//
// It is the first half of a lease: the replacement claims the freed IP only
// after this returns (ClaimVM refuses an address still held), so the order
// quarantine → claim is mandatory.
//
// A stopped VM can be quarantined too: it has no TAP to detach, and Start and
// Restore bring it back bridge-less (recreateTap). There is no way back — a
// quarantined VM returns to service only as a new VM.
//
// Fail-closed ordering: the TAP leaves the bridge first, then the record is
// persisted, then the IP released. If persisting fails, the TAP is put back
// and nothing has changed. If the daemon dies between the detach and the
// write, the unacknowledged operation is undone at startup: Reconcile
// re-enslaves the TAP of every networked VM it adopts.
func (m *Manager) Quarantine(id string) (*types.VM, error) {
	if err := m.begin(id, "quarantine"); err != nil {
		return nil, err
	}
	defer m.end(id)
	return m.quarantine(id)
}

// quarantine is Quarantine for a caller that already holds the VM's busy mark.
func (m *Manager) quarantine(id string) (*types.VM, error) {
	m.mu.Lock()
	record, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	prev := record.Config
	state := record.State
	m.mu.Unlock()
	if prev.Quarantine {
		return nil, fmt.Errorf("%w: vm %s is already quarantined", ErrVMState, id)
	}

	labels := maps.Clone(prev.Labels)
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[LeaseLabel] = LeaseQuarantined
	if err := ValidateLabels(labels); err != nil {
		return nil, err
	}

	// Only a running (or paused) VM holds a TAP; a stopped one released it.
	detached := false
	if prev.TapDevice != "" && state != types.VMStateStopped {
		if err := network.DetachTap(prev.TapDevice); err != nil {
			return nil, fmt.Errorf("quarantining vm %s: %w", id, err)
		}
		detached = true
	}

	m.mu.Lock()
	record.Config.Quarantine = true
	record.Config.QuarantinedFrom = prev.NetworkName
	record.Config.NetworkName = ""
	record.Config.Bridge = ""
	// Replaced, never written into: copies handed out earlier share it.
	record.Config.Labels = labels
	cp := *record
	m.mu.Unlock()

	if err := m.store.SaveVM(&cp); err != nil {
		m.mu.Lock()
		record.Config.Quarantine = false
		record.Config.QuarantinedFrom = prev.QuarantinedFrom
		record.Config.NetworkName = prev.NetworkName
		record.Config.Bridge = prev.Bridge
		record.Config.Labels = prev.Labels
		m.mu.Unlock()
		err = fmt.Errorf("persisting quarantined vm %s: %w", id, err)
		if detached {
			if rerr := network.EnslaveTap(prev.TapDevice, prev.Bridge, !m.netmgr.Intra(prev.NetworkName)); rerr != nil {
				// The VM is off its network while its record says it is on
				// it; the next daemon start re-enslaves it (Reconcile).
				return nil, fmt.Errorf("%w; putting its tap back on %s also failed, it stays cut off until the daemon restarts: %v", err, prev.Bridge, rerr)
			}
		}
		return nil, err
	}

	if prev.NetworkName != "" {
		m.netmgr.DetachVM(prev.NetworkName, id)
	}
	log.Printf("vm %s quarantined (was %s on network %q)", id, prev.GuestIP, prev.NetworkName)
	m.emit(types.EventVMQuarantined, &cp, "", nil)
	return &cp, nil
}
