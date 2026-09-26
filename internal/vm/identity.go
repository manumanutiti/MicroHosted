package vm

import (
	"fmt"
	"log"

	"microhosted/internal/jailer"
	"microhosted/internal/labels"
	"microhosted/pkg/types"
)

// ErrInvalid is a request the manager refuses on its own terms — a malformed
// name or label, an impossible VM shape — whatever the host's state. Maps to
// 400 in the API. It is the labels package's sentinel, so a bad label is the
// same error whichever object it was written on.
var ErrInvalid = labels.ErrInvalid

const (
	// maxVCPUs is Firecracker's own ceiling for a microVM.
	maxVCPUs = 32
	// minMemMB is below what any guest here boots in (the Alpine image idles
	// at ~20 MB); smaller asks are a typo for a size, not a tiny VM.
	minMemMB = 32
)

// ValidateName checks a VM name: empty (no name) or a DNS label of up to 63
// characters that does not have the shape of a VM ID — a name "1a2b3c4d" could
// be another VM's ID, and a reference to it would be ambiguous.
func ValidateName(name string) error {
	if name == "" {
		return nil
	}
	if err := labels.ValidateName("name", name); err != nil {
		return err
	}
	if jailer.IsVMID(name) {
		return fmt.Errorf("%w: name %q has the shape of a VM ID (8 hex characters)", ErrInvalid, name)
	}
	return nil
}

// ValidateLabels checks a VM's label set (see labels.Validate).
func ValidateLabels(l map[string]string) error {
	return labels.Validate(l)
}

// validateShape checks the resources a create asks for. Zero means "the
// template's"; anything else must be a VM Firecracker can run. Whether the
// host has room for it is admission's call (see admit), not this one.
func validateShape(vcpus, memMB, diskMB int64) error {
	if vcpus < 0 || vcpus > maxVCPUs {
		return fmt.Errorf("%w: vcpus %d: between 1 and %d", ErrInvalid, vcpus, maxVCPUs)
	}
	if memMB < 0 || (memMB > 0 && memMB < minMemMB) {
		return fmt.Errorf("%w: mem_mb %d: at least %d", ErrInvalid, memMB, minMemMB)
	}
	if diskMB < 0 {
		return fmt.Errorf("%w: disk_mb %d is negative", ErrInvalid, diskMB)
	}
	return nil
}

// reserveNameLocked claims name for id; the caller holds m.mu. The claim is
// taken before the VM's record is first written, so two concurrent creates
// cannot both get it, and releaseName drops it when the VM goes away
// (Destroy, undoCreate).
func (m *Manager) reserveNameLocked(name, id string) error {
	if name == "" {
		return nil
	}
	if m.names == nil {
		m.names = make(map[string]string)
	}
	if owner, ok := m.names[name]; ok && owner != id {
		return fmt.Errorf("%w: the name %q is taken by vm %s", ErrConflict, name, owner)
	}
	m.names[name] = id
	return nil
}

func (m *Manager) reserveName(name, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reserveNameLocked(name, id)
}

func (m *Manager) releaseName(name, id string) {
	if name == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.names[name] == id {
		delete(m.names, name)
	}
}

// adoptName registers the name of a VM Reconcile keeps. Two records with the
// same name can only come from a store written by hand or by another build;
// the first one keeps it and the other loses its name, loudly, rather than
// refusing to start the daemon over an alias.
func (m *Manager) adoptName(rec *types.VM) {
	if rec.Config.Name == "" {
		return
	}
	if err := m.reserveName(rec.Config.Name, rec.Config.ID); err != nil {
		log.Printf("reconcile: vm %s: dropping its name: %v", rec.Config.ID, err)
		rec.Config.Name = ""
		if err := m.store.SaveVM(rec); err != nil {
			log.Printf("reconcile: vm %s: persisting the dropped name: %v", rec.Config.ID, err)
		}
	}
}

// SetLabels applies a merge patch to a VM's labels: a key with a value sets
// it, a key with nil removes it. The result is validated whole and persisted
// before it is reported; on any failure the VM keeps the labels it had.
func (m *Manager) SetLabels(id string, patch map[string]*string) (*types.VM, error) {
	if err := labels.ValidatePatch(patch); err != nil {
		return nil, err
	}

	m.mu.Lock()
	record, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	prev := record.Config.Labels
	next, err := labels.Patch(prev, patch)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	// Replaced, never written into: copies handed out earlier share prev.
	record.Config.Labels = next
	cp := *record
	m.mu.Unlock()

	if err := m.store.SaveVM(&cp); err != nil {
		m.mu.Lock()
		record.Config.Labels = prev
		m.mu.Unlock()
		return nil, fmt.Errorf("persisting vm %s: %w", id, err)
	}
	return &cp, nil
}
