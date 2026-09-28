package orch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"microhosted/orchestrator/spec"
	"microhosted/pkg/types"
)

// Instance is one of a function's VMs as the engine reports it.
type Instance struct {
	Name, ID, State, IP string
	Age                 time.Duration
	Quarantined         bool
	// Health: persistent functions only — "healthy", or why not.
	Health string
}

// FunctionStatus is a function of the spec and what the engine holds for it.
type FunctionStatus struct {
	Name, Mode string
	Instances  []Instance
	// From the orchestrator's state: set by a running orchestrator.
	Degraded    bool
	Held        string // why, when held on its previous version
	LastFailure *Failure
	Failures    int
}

// Status reports every function of the spec and, separately, what the
// orchestrator owns that the spec does not declare (should be nothing).
func (o *Orchestrator) Status(ctx context.Context) ([]FunctionStatus, []string, error) {
	vms, err := o.eng.ListVMs(ctx, owned())
	if err != nil {
		return nil, nil, err
	}
	nets, err := o.eng.ListNetworks(ctx, owned())
	if err != nil {
		return nil, nil, err
	}
	byFn := map[string][]types.VMResponse{}
	for _, v := range vms {
		byFn[v.Labels[LabelFunction]] = append(byFn[v.Labels[LabelFunction]], v)
	}
	var out []FunctionStatus
	for _, name := range o.spec.FunctionOrder {
		f := o.spec.Functions[name]
		fs := FunctionStatus{Name: name, Mode: f.Lifecycle.Mode}
		if o.state != nil {
			st := o.state.Get(name)
			fs.Degraded, fs.Failures = st.Degraded, len(st.Failures)
			if st.Held {
				fs.Held = st.HeldReason
			}
			if n := len(st.Failures); n > 0 {
				fs.LastFailure = &st.Failures[n-1]
			}
		}
		for _, v := range byFn[name] {
			in := Instance{Name: v.Name, ID: v.ID, State: string(v.State), IP: v.GuestIP, Quarantined: v.Quarantine}
			if t, err := time.Parse(time.RFC3339, v.CreatedAt); err == nil {
				in.Age = time.Since(t).Round(time.Second)
			}
			if f.Lifecycle.Mode == spec.ModePersistent && v.State == types.VMStateRunning && !v.Quarantine {
				in.Health = o.probe(ctx, v.ID, f)
			}
			fs.Instances = append(fs.Instances, in)
		}
		out = append(out, fs)
		delete(byFn, name)
	}
	var orphans []string
	for fn, vs := range byFn {
		for _, v := range vs {
			orphans = append(orphans, fmt.Sprintf("vm %s (%s), function %q not in the spec", v.Name, v.ID, fn))
		}
	}
	for _, n := range nets {
		if _, ok := o.spec.Networks[n.Name]; !ok {
			orphans = append(orphans, fmt.Sprintf("network %s, not in the spec", n.Name))
		}
	}
	sort.Strings(orphans)
	return out, orphans, nil
}

// probe runs a persistent instance's check once, for status.
func (o *Orchestrator) probe(ctx context.Context, id string, f *spec.Function) string {
	switch {
	case f.Health != nil:
		if err := o.checkHealth(ctx, id, f.Health); err != nil {
			return "unhealthy: " + err.Error()
		}
	case f.Command != "":
		if ok, out := o.commandAlive(ctx, id); !ok {
			return "command not running: " + strings.TrimSpace(out)
		}
	default:
		if _, err := o.eng.Exec(ctx, id, "true", 5*time.Second); err != nil {
			return "agent not answering: " + err.Error()
		}
	}
	return "healthy"
}

// Down removes everything the orchestrator owns except quarantined VMs, which
// are evidence and stay until an operator deletes them. It returns what it
// kept.
func (o *Orchestrator) Down(ctx context.Context) ([]string, error) {
	vms, err := o.eng.ListVMs(ctx, owned())
	if err != nil {
		return nil, err
	}
	var kept []string
	var errs []error
	for _, v := range vms {
		if v.Quarantine {
			kept = append(kept, fmt.Sprintf("vm %s (%s): quarantined", v.Name, v.ID))
			continue
		}
		o.log.Printf("destroy vm %s (%s)", v.Name, v.ID)
		if err := o.destroy(v.ID, "down"); err != nil {
			errs = append(errs, err)
		}
	}
	nets, err := o.eng.ListNetworks(ctx, owned())
	if err != nil {
		return kept, errors.Join(append(errs, err)...)
	}
	for _, n := range nets {
		o.log.Printf("delete network %s", n.Name)
		if err := o.eng.DeleteNetwork(ctx, n.Name); err != nil {
			errs = append(errs, fmt.Errorf("network %s: %w", n.Name, err))
		}
	}
	return kept, errors.Join(errs...)
}
