// Package labels validates, patches and selects on the key=value metadata that
// VMs and networks carry. One set of rules for both: an orchestrator that marks
// what it owns (managed-by=…) must be able to write the same mark on every
// kind of object and select on it the same way.
package labels

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
)

// ErrInvalid is a request refused on its own terms — a malformed name or
// label, an impossible shape — whatever the host's state. The API maps it
// to 400.
var ErrInvalid = errors.New("invalid request")

const (
	// ManagedBy is the owner label: which consumer (an orchestrator, an
	// operator's tooling) an object belongs to. The engine counts per-consumer
	// quotas by it, and an orchestrator only touches objects carrying its own
	// value, so it is set when the object is created and never changed by a
	// patch: relabelling would slip a VM out of its quota, or hand one
	// consumer's objects to another.
	ManagedBy = "managed-by"

	// MaxLabels is how many labels one object may carry.
	MaxLabels     = 32
	maxLabelKey   = 63
	maxLabelValue = 63
	maxNameLength = 63
	selectSep     = "="
)

var (
	// A DNS label: what fits in a hostname, a metric label and a file name
	// without quoting.
	nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	// Keys may carry a prefix ("ot.plant/sensor"), as in Kubernetes.
	keyRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]*[a-z0-9])?$`)
	valueRE = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?)?$`)
)

// ValidateName checks that name is a DNS label of up to 63 characters. what
// names the object in the error ("name", "network name").
func ValidateName(what, name string) error {
	if len(name) > maxNameLength || !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %s %q: lowercase letters, digits and '-', starting and ending with a letter or digit, at most %d characters", ErrInvalid, what, name, maxNameLength)
	}
	return nil
}

// Validate checks a label set. A label written wrong is refused, never
// dropped: an orchestrator that selects on it would silently lose the object.
func Validate(labels map[string]string) error {
	if len(labels) > MaxLabels {
		return fmt.Errorf("%w: %d labels, at most %d", ErrInvalid, len(labels), MaxLabels)
	}
	for k, v := range labels {
		if err := ValidateKey(k); err != nil {
			return err
		}
		if len(v) > maxLabelValue || !valueRE.MatchString(v) {
			return fmt.Errorf("%w: label %s: value %q: letters, digits, '.', '_' and '-', starting and ending with a letter or digit, at most %d characters", ErrInvalid, k, v, maxLabelValue)
		}
	}
	return nil
}

// ValidateKey checks one label key.
func ValidateKey(k string) error {
	if len(k) > maxLabelKey || !keyRE.MatchString(k) {
		return fmt.Errorf("%w: label key %q: lowercase letters, digits, '.', '_', '-' and '/', starting and ending with a letter or digit, at most %d characters", ErrInvalid, k, maxLabelKey)
	}
	return nil
}

// Patch applies a merge patch to prev — a key with a value sets it, a key with
// nil removes it — and returns the validated result, nil when empty. prev is
// never written into: copies handed out earlier may share it.
func Patch(prev map[string]string, patch map[string]*string) (map[string]string, error) {
	if err := ValidatePatch(patch); err != nil {
		return nil, err
	}
	if v, ok := patch[ManagedBy]; ok {
		cur, had := prev[ManagedBy]
		if v == nil && had || v != nil && (!had || cur != *v) {
			return nil, fmt.Errorf("%w: label %s is fixed when the object is created (it decides ownership and quota)", ErrInvalid, ManagedBy)
		}
	}
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
	if err := Validate(next); err != nil {
		return nil, err
	}
	if len(next) == 0 {
		return nil, nil
	}
	return next, nil
}

// ValidatePatch checks what can be checked of a patch without the object it
// applies to — its keys and the values it sets — so a malformed request is
// refused before looking the object up.
func ValidatePatch(patch map[string]*string) error {
	for k, v := range patch {
		if err := ValidateKey(k); err != nil {
			return err
		}
		if v != nil {
			if err := Validate(map[string]string{k: *v}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Selector is a set of key=value requirements, all of which must hold.
type Selector map[string]string

// ParseSelector reads "k=v" terms (one per element, or comma-separated within
// one) into a selector. An empty input selects everything.
func ParseSelector(terms []string) (Selector, error) {
	sel := make(Selector)
	for _, t := range terms {
		for _, term := range strings.Split(t, ",") {
			if term == "" {
				continue
			}
			k, v, ok := strings.Cut(term, selectSep)
			if !ok {
				return nil, fmt.Errorf("%w: label selector %q: want key=value", ErrInvalid, term)
			}
			if err := Validate(map[string]string{k: v}); err != nil {
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
func (s Selector) Matches(labels map[string]string) bool {
	for k, v := range s {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}
