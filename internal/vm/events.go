package vm

import (
	"microhosted/internal/events"
	"microhosted/pkg/types"
)

// SetEvents connects the manager to the bus its lifecycle events go to. Call
// once, before Reconcile, so what startup finds (VMs that died while the
// daemon was down) is published too. Without it events are dropped.
func (m *Manager) SetEvents(b *events.Bus) {
	m.events = b
}

// emit publishes a vm.* event describing rec as it is now. A quarantined VM is
// reported on the network it served on, so a subscriber filtering by network
// still sees what became of its VMs. Takes m.mu to read rec: never call it
// holding the lock.
func (m *Manager) emit(typ string, rec *types.VM, reason string, data map[string]string) {
	if m.events == nil {
		return
	}
	cfg := m.snapshotVM(rec).Config
	network := cfg.NetworkName
	if network == "" {
		network = cfg.QuarantinedFrom
	}
	m.events.Publish(types.Event{
		Type:    typ,
		VM:      cfg.ID,
		Name:    cfg.Name,
		Labels:  cfg.Labels, // copy-on-write: never written into, safe to share
		Network: network,
		Reason:  reason,
		Data:    data,
	})
}
