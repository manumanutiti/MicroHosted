package vm

import (
	"errors"
	"fmt"
	"log"
	"maps"
	"regexp"
	"strings"

	"microhosted/internal/jailer"
	"microhosted/pkg/types"
)

// ErrInvalid is a request the manager refuses on its own terms — a malformed
// name or label, an impossible VM shape — whatever the host's state. Maps to
// 400 in the API.
var ErrInvalid = errors.New("invalid request")

const (
	// maxVCPUs is Firecracker's own ceiling for a microVM.
	maxVCPUs = 32
	// minMemMB is below what any guest here boots in (the Alpine image idles
	// at ~20 MB); smaller asks are a typo for a size, not a tiny VM.
	minMemMB = 32

	maxLabels      = 32
	maxLabelKey    = 63
	maxLabelValue  = 63
	maxNameLength  = 63
	labelSelectSep = "="
)

var (
	// A DNS label: what fits in a hostname, a metric label and a file name
	// without quoting.
	nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	// Keys may carry a prefix ("ot.plant/sensor"), as in Kubernetes.
	labelKeyRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]*[a-z0-9])?$`)
	labelValueRE = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?)?$`)
)

// ValidateName checks a VM name: empty (no name) or a DNS label of up to 63
// characters that does not have the shape of a VM ID — a name "1a2b3c4d" could
// be another VM's ID, and a reference to it would be ambiguous.
func ValidateName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > maxNameLength || !nameRE.MatchString(name) {
		return fmt.Errorf("%w: name %q: lowercase letters, digits and '-', starting and ending with a letter or digit, at most %d characters", ErrInvalid, name, maxNameLength)
	}
	if jailer.IsVMID(name) {
		return fmt.Errorf("%w: name %q has the shape of a VM ID (8 hex characters)", ErrInvalid, name)
	}
	return nil
}

// ValidateLabels checks a label set. A label written wrong is refused, never
// dropped: an orchestrator that selects on it would silently lose the VM.
func ValidateLabels(labels map[string]string) error {
	if len(labels) > maxLabels {
		return fmt.Errorf("%w: %d labels, at most %d", ErrInvalid, len(labels), maxLabels)
	}
	for k, v := range labels {
		if err := validateLabelKey(k); err != nil {
			return err
		}
		if len(v) > maxLabelValue || !labelValueRE.MatchString(v) {
			return fmt.Errorf("%w: label %s: value %q: letters, digits, '.', '_' and '-', starting and ending with a letter or digit, at most %d characters", ErrInvalid, k, v, maxLabelValue)
		}
	}
	return nil
}

func validateLabelKey(k string) error {
	if len(k) > maxLabelKey || !labelKeyRE.MatchString(k) {
		return fmt.Errorf("%w: label key %q: lowercase letters, digits, '.', '_', '-' and '/', starting and ending with a letter or digit, at most %d characters", ErrInvalid, k, maxLabelKey)
	}
	return nil
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
	for k := range patch {
		if err := validateLabelKey(k); err != nil {
			return nil, err
		}
	}

	m.mu.Lock()
	record, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	prev := record.Config.Labels
	next := maps.Clone(prev)
	if next == nil {
		next = make(map[string]string)
	}
	for k, v := range patch {
		if v == nil {
			delete(next, k)
		} else {
			next[k] = *v
		}
	}
	if err := ValidateLabels(next); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if len(next) == 0 {
		next = nil
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

// LabelSelector is a set of key=value requirements, all of which must hold.
type LabelSelector map[string]string

// ParseLabelSelector reads "k=v" terms (one per element, or comma-separated
// within one) into a selector. An empty input selects everything.
func ParseLabelSelector(terms []string) (LabelSelector, error) {
	sel := make(LabelSelector)
	for _, t := range terms {
		for _, term := range strings.Split(t, ",") {
			if term == "" {
				continue
			}
			k, v, ok := strings.Cut(term, labelSelectSep)
			if !ok {
				return nil, fmt.Errorf("%w: label selector %q: want key=value", ErrInvalid, term)
			}
			if err := ValidateLabels(map[string]string{k: v}); err != nil {
				return nil, err
			}
			if prev, dup := sel[k]; dup && prev != v {
				return nil, fmt.Errorf("%w: label selector asks for %s=%s and %s=%s at once", ErrInvalid, k, prev, k, v)
			}
			sel[k] = v
		}
	}
	return sel, nil
}

// Matches reports whether labels satisfy every requirement of s.
func (s LabelSelector) Matches(labels map[string]string) bool {
	for k, v := range s {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}
