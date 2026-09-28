package orch

import (
	"context"
	"strings"

	"microhosted/orchestrator/spec"
)

// rollout makes ns the desired state of a running orchestrator
// (docs/orchestrator.md §13). A spec that does not plan — invalid against the
// engine, over budget, colliding with objects the orchestrator does not own —
// is refused whole and the previous one keeps running. Otherwise:
//
//   - removed functions stop and their VMs go; networks are created and
//     updated, and labels patched, as planned;
//   - changed functions are updated one at a time, in the spec's order, each
//     verified before the next: a persistent function by a fresh VM passing
//     its checks, a cycle function by one cycle run at once;
//   - at the first failure that function goes back to its previous version
//     — a fresh VM from it, never the failed one reconnected — and is held,
//     and the roll-out stops: functions not reached keep their previous
//     version. Applying again retries from there.
//
// It runs in the supervisor's goroutine, so no supervision races it.
func (o *Orchestrator) rollout(ctx context.Context, ns *spec.Spec, state map[string]*persistentState, cs *cycleSet) {
	p, err := o.planFor(ctx, ns)
	if err != nil {
		o.log.Printf("reload refused, the previous spec keeps running: %v", err)
		return
	}
	old := o.spec
	o.log.Printf("reload: rolling out the new spec")
	if ns.Budget.Workers != old.Budget.Workers {
		o.log.Printf("reload: budget.workers %d → %d takes effect when the orchestrator restarts", old.Budget.Workers, ns.Budget.Workers)
	}
	o.mu.Lock()
	o.spec = ns
	o.mu.Unlock()

	// Removed functions stop first, so no cycle of theirs starts a VM
	// between the pruning below and their end.
	for _, name := range old.FunctionOrder {
		if ns.Functions[name] != nil {
			continue
		}
		cs.stop(name)
		o.setFn(name, nil)
		delete(state, name)
		if o.state != nil {
			o.state.Remove(name)
		}
		o.log.Printf("function %s: removed", name)
	}

	var deletes []Action
	for _, a := range p.Actions {
		switch a.kind {
		case kindNetwork, kindPrune, kindLabels:
			o.log.Printf("%s", a)
			if err := a.run(ctx); err != nil {
				// A function that needs it fails its update and goes back.
				o.log.Printf("reload: %s %s: %v", a.Verb, a.Object, err)
			}
		case kindNetDelete:
			deletes = append(deletes, a)
		}
	}

	for i, name := range ns.FunctionOrder {
		if ctx.Err() != nil {
			return
		}
		nf, cur := ns.Functions[name], o.fn(name)
		switch {
		case cur == nil:
			o.setFn(name, nf)
			if nf.Lifecycle.Mode == spec.ModePersistent {
				state[name] = &persistentState{} // the supervisor starts it
			} else {
				cs.start(name)
			}
			o.log.Printf("function %s: added", name)

		case SpecHash(cur) == SpecHash(nf):
			// Same VM contents: health, labels or schedule changed, if
			// anything. Nothing to verify.
			o.setFn(name, nf)
			o.unhold(name)
			if cur.Lifecycle != nf.Lifecycle && nf.Lifecycle.Mode != spec.ModePersistent {
				cs.stop(name)
				cs.start(name)
			}

		default:
			if !o.update(ctx, name, cur, nf, state, cs) {
				var rest []string
				for _, n := range ns.FunctionOrder[i+1:] {
					if c := o.fn(n); c == nil || SpecHash(c) != SpecHash(ns.Functions[n]) {
						rest = append(rest, n)
					}
				}
				msg := "reload: roll-out halted at " + name
				if len(rest) > 0 {
					msg += "; not rolled out: " + strings.Join(rest, ", ")
				}
				o.log.Printf("%s — fix the spec and apply again", msg)
				return
			}
		}
	}
	for _, a := range deletes {
		o.log.Printf("%s", a)
		if err := a.run(ctx); err != nil {
			o.log.Printf("reload: %s %s: %v", a.Verb, a.Object, err)
		}
	}
	o.log.Printf("reload: rolled out")
}

// update moves one function from cur to nf and verifies it; false means it
// went back to cur and is held.
func (o *Orchestrator) update(ctx context.Context, name string, cur, nf *spec.Function, state map[string]*persistentState, cs *cycleSet) bool {
	if cur.Lifecycle.Mode != nf.Lifecycle.Mode {
		// Another kind of function under the same name: the old one goes,
		// the new one starts. Nothing to go back to.
		cs.stop(name)
		o.destroyServing(ctx, name, "mode changed")
		delete(state, name)
		o.setFn(name, nf)
		if nf.Lifecycle.Mode == spec.ModePersistent {
			state[name] = &persistentState{}
		} else {
			cs.start(name)
		}
		o.log.Printf("function %s: now %s", name, nf.Lifecycle.Mode)
		return true
	}

	if nf.Lifecycle.Mode == spec.ModePersistent {
		// About a second without the function: the engine never lets two
		// VMs serve one address.
		o.destroyServing(ctx, name, "update")
		o.setFn(name, nf)
		if st := state[name]; st != nil {
			*st = persistentState{}
		}
		cs.workers <- struct{}{}
		vm, err := o.startPersistent(ctx, name, nf)
		<-cs.workers
		if err == nil {
			o.unhold(name)
			o.log.Printf("function %s: updated, serving on vm %s (%s)", name, vm.Name, vm.ID)
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		o.log.Printf("function %s: the new version failed (%v): going back to the previous one", name, err)
		o.setFn(name, cur)
		cs.workers <- struct{}{}
		vm, err2 := o.startPersistent(ctx, name, cur)
		<-cs.workers
		if err2 != nil {
			o.log.Printf("function %s: the previous version failed to start too (%v): the supervisor keeps retrying it", name, err2)
		} else {
			o.log.Printf("function %s: back on its previous version, vm %s (%s)", name, vm.Name, vm.ID)
		}
		o.hold(name, err.Error())
		return false
	}

	// A cycle function: its next cycles run the new version once one cycle
	// of it, run now, succeeds.
	cs.stop(name)
	defer cs.start(name)
	o.setFn(name, nf)
	cs.workers <- struct{}{}
	r := o.cycle(ctx, name, nf)
	<-cs.workers
	o.report(r)
	switch {
	case r.OK:
		o.unhold(name)
		o.log.Printf("function %s: updated, verified by one cycle", name)
		return true
	case ctx.Err() != nil:
		return false
	case r.Skipped:
		// The host was full: nothing learnt about the new version, which
		// stays; its scheduled cycles report how it does.
		o.log.Printf("function %s: updated, not verified (%s)", name, r.Cause)
		return true
	}
	o.log.Printf("function %s: the new version failed its verification cycle (%s): going back to the previous one", name, r.Cause)
	o.setFn(name, cur)
	o.hold(name, r.Cause)
	return false
}

// destroyServing destroys a function's VMs that are not quarantined.
func (o *Orchestrator) destroyServing(ctx context.Context, name, why string) {
	vms, err := o.myVMs(ctx, o.desired())
	if err != nil {
		o.log.Printf("function %s: listing its VMs: %v", name, err)
		return
	}
	for _, v := range vms {
		if v.Labels[LabelFunction] == name && !v.Quarantine {
			o.destroy(v.ID, why)
		}
	}
}

// hold marks a function as held on its previous version after a failed
// update, for status; a later successful update (or a new run) clears it.
func (o *Orchestrator) hold(name, reason string) {
	if o.state != nil {
		if err := o.state.SetHeld(name, reason); err != nil {
			o.log.Printf("function %s: recording held: %v", name, err)
		}
	}
}

func (o *Orchestrator) unhold(name string) {
	if o.state != nil && o.state.Get(name).Held {
		if err := o.state.SetHeld(name, ""); err != nil {
			o.log.Printf("function %s: clearing held: %v", name, err)
		}
	}
}
