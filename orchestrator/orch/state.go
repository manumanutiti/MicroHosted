package orch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Failure is why one of a function's VMs failed, captured before the VM was
// destroyed (docs/orchestrator.md §8). Output and Console come from the guest:
// untrusted, stored and shown as data.
type Failure struct {
	At       string `json:"at"`
	Function string `json:"function"`
	Mode     string `json:"mode"`
	VM       string `json:"vm,omitempty"`
	VMID     string `json:"vm_id,omitempty"`
	Image    string `json:"image"`
	// Cause: exit N, timeout, health, died, create, … with the detail.
	Cause string `json:"cause"`
	Exit  *int   `json:"exit,omitempty"`
	// AfterMS is how long after the VM was created it failed: "fails right
	// after starting" reads as a small number.
	AfterMS int64 `json:"after_ms"`
	// Output: the end of the command's (or the service's) stdout+stderr.
	Output string `json:"output,omitempty"`
	// Console: the end of the VM's serial console — kernel, init and
	// whatever the guest printed there.
	Console string `json:"console,omitempty"`
}

// Bounds on what failure records keep.
const (
	// FailuresKept per function: the newest ones.
	FailuresKept = 5
	// ConsoleKept: how much of the console tail a record keeps.
	ConsoleKept = 16 << 10
)

// FunctionState is what the orchestrator keeps about one function across its
// own restarts.
type FunctionState struct {
	Failures []Failure `json:"failures,omitempty"`
	// Degraded is set by a running orchestrator that stopped retrying the
	// function; a new run starts it over.
	Degraded   bool   `json:"degraded,omitempty"`
	DegradedAt string `json:"degraded_at,omitempty"`
	// Held: a running orchestrator put the function back on its previous
	// version after its update failed; applying the spec again retries.
	Held       bool   `json:"held,omitempty"`
	HeldReason string `json:"held_reason,omitempty"`
	HeldAt     string `json:"held_at,omitempty"`
}

// State is the orchestrator's small persistent state: one JSON file per
// function in a private directory, each rewritten atomically (write + rename).
// It holds no objects — the engine is the source of truth for what exists.
type State struct {
	dir string
	mu  sync.Mutex
}

// DefaultStateDir is $XDG_STATE_HOME/mh-orchestrator, or
// ~/.local/state/mh-orchestrator.
func DefaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "mh-orchestrator")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "mh-orchestrator-state")
	}
	return filepath.Join(home, ".local", "state", "mh-orchestrator")
}

// OpenState opens (creating it) a state directory. It is private: console
// tails may carry whatever a guest printed, secrets included.
func OpenState(dir string) (*State, error) {
	if err := os.MkdirAll(filepath.Join(dir, "functions"), 0o700); err != nil {
		return nil, err
	}
	for _, d := range []string{dir, filepath.Join(dir, "functions")} {
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &State{dir: dir}, nil
}

func (s *State) path(fn string) string {
	return filepath.Join(s.dir, "functions", fn+".json")
}

// Get returns a function's state; a missing or unreadable file is empty.
func (s *State) Get(fn string) FunctionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(fn)
}

func (s *State) get(fn string) FunctionState {
	var st FunctionState
	if data, err := os.ReadFile(s.path(fn)); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

// update applies change to a function's state and writes it back.
func (s *State) update(fn string, change func(*FunctionState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.get(fn)
	change(&st)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "functions"), "."+fn+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path(fn))
}

// RecordFailure appends a failure, keeping the newest FailuresKept.
func (s *State) RecordFailure(f Failure) error {
	return s.update(f.Function, func(st *FunctionState) {
		st.Failures = append(st.Failures, f)
		if n := len(st.Failures); n > FailuresKept {
			st.Failures = st.Failures[n-FailuresKept:]
		}
	})
}

// SetDegraded marks or clears a function as degraded.
func (s *State) SetDegraded(fn string, degraded bool) error {
	return s.update(fn, func(st *FunctionState) {
		st.Degraded = degraded
		st.DegradedAt = ""
		if degraded {
			st.DegradedAt = time.Now().UTC().Format(time.RFC3339)
		}
	})
}

// SetHeld marks a function as held (reason non-empty) or clears it.
func (s *State) SetHeld(fn, reason string) error {
	return s.update(fn, func(st *FunctionState) {
		st.Held, st.HeldReason, st.HeldAt = reason != "", reason, ""
		if st.Held {
			st.HeldAt = time.Now().UTC().Format(time.RFC3339)
		}
	})
}

// Remove forgets a function removed from the spec.
func (s *State) Remove(fn string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(fn)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Prune removes the state of functions not in keep (no residue: a removed
// function leaves nothing behind) and clears every degraded and held flag — a
// new run starts every function from the spec with its attempts again.
func (s *State) Prune(keep map[string]bool) error {
	entries, err := os.ReadDir(filepath.Join(s.dir, "functions"))
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		fn, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !keep[fn] {
			if err := os.Remove(s.path(fn)); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if st := s.Get(fn); st.Degraded || st.Held {
			if err := s.update(fn, func(st *FunctionState) {
				st.Degraded, st.DegradedAt = false, ""
				st.Held, st.HeldReason, st.HeldAt = false, "", ""
			}); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("pruning orchestrator state: %w", err)
	}
	return nil
}

// Printable makes guest text safe for a terminal: control characters other
// than newline and tab — escape sequences that could rewrite the operator's
// screen — are shown escaped, and serial-console CRLFs become newlines.
func Printable(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			fmt.Fprintf(&b, "\\x%02x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
