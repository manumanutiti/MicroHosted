package orch

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"microhosted/orchestrator/spec"
	"microhosted/pkg/types"
)

// Action is one change the plan makes, in order.
type Action struct {
	Verb   string // create, update, destroy, delete
	Object string // "network web", "function api", "vm api-3"
	Why    string
	run    func(ctx context.Context) error
	// function is the persistent function a create action starts.
	function string
	kind     actionKind
}

// actionKind tells a roll-out (rollout.go) which actions it runs as planned
// and which it handles itself, one function at a time.
type actionKind int

const (
	kindNetwork   actionKind = iota // create or update a network: run as planned
	kindNetDelete                   // delete a network: run last
	kindPrune                       // destroy a VM of a removed function
	kindLabels                      // patch a VM's labels in place
	kindFunction                    // (re)create a persistent function: the roll-out's job
	kindLeftover                    // a cycle VM outside a cycle: only without a running orchestrator
)

// Rollout returns the actions a running orchestrator would carry out as
// planned when this spec is applied to it: what changes a function's VM is
// rolled out one function at a time instead, and cycle VMs are its own.
func (p *Plan) Rollout() []Action {
	var out []Action
	for _, a := range p.Actions {
		if a.kind != kindLeftover {
			out = append(out, a)
		}
	}
	return out
}

// ApplyError is the action an apply stopped at.
type ApplyError struct {
	Action Action
	Err    error
}

func (e *ApplyError) Error() string {
	return fmt.Sprintf("%s %s: %v", e.Action.Verb, e.Action.Object, e.Err)
}
func (e *ApplyError) Unwrap() error { return e.Err }

func (a Action) String() string {
	s := fmt.Sprintf("%-8s %s", a.Verb, a.Object)
	if a.Why != "" {
		s += "  (" + a.Why + ")"
	}
	return s
}

// Plan is what an apply would change, and the worst case it has to fit.
type Plan struct {
	Actions []Action
	// Kept lists what the plan deliberately leaves alone (quarantined VMs).
	Kept      []string
	PeakVMs   int
	PeakMemMB int64
}

// Plan compares the spec with what the engine holds and returns the changes
// that converge them. It changes nothing. It fails — before any change — when
// the spec cannot be applied: a name taken by an object the orchestrator does
// not own, an overlapping subnet, an image missing from the store, a worst
// case over the budget.
func (o *Orchestrator) Plan(ctx context.Context) (*Plan, error) {
	return o.planFor(ctx, o.spec)
}

// planFor plans spec s, which need not be the one the orchestrator holds: a
// reload plans the new spec before adopting it.
func (o *Orchestrator) planFor(ctx context.Context, s *spec.Spec) (*Plan, error) {
	p := &Plan{}
	var errs []error

	if err := o.budget(ctx, s, p); err != nil {
		errs = append(errs, err)
	}

	// Networks.
	all, err := o.eng.ListNetworks(ctx, nil)
	if err != nil {
		return nil, err
	}
	existing := map[string]types.NetworkResponse{}
	for _, n := range all {
		existing[n.Name] = n
	}
	var netNames []string
	for name := range s.Networks {
		netNames = append(netNames, name)
	}
	sort.Strings(netNames)
	for _, name := range netNames {
		want := s.Networks[name]
		have, ok := existing[name]
		if ok && have.Labels[LabelManagedBy] != Owner {
			errs = append(errs, fmt.Errorf("networks.%s: a network with that name exists and is not the orchestrator's: rename one of them", name))
			continue
		}
		if ok && !o.mine(have.Labels, true) {
			errs = append(errs, fmt.Errorf("networks.%s: the network belongs to project %s: network names are shared by the whole host, rename one of them", name, have.Labels[LabelProject]))
			continue
		}
		if !ok {
			wp := netip.MustParsePrefix(want.Subnet)
			for _, n := range all {
				if hp, err := netip.ParsePrefix(n.Subnet); err == nil && hp.Overlaps(wp) {
					errs = append(errs, fmt.Errorf("networks.%s: subnet %s overlaps the engine's network %s (%s)", name, want.Subnet, n.Name, n.Subnet))
				}
			}
			req := createNetworkRequest(o.project, name, want)
			p.Actions = append(p.Actions, Action{Verb: "create", Object: "network " + name, Why: want.Subnet, kind: kindNetwork,
				run: func(ctx context.Context) error { return o.eng.CreateNetwork(ctx, req) }})
			continue
		}
		if have.Subnet != want.Subnet {
			errs = append(errs, fmt.Errorf("networks.%s: subnet %s → %s would recreate the network and every VM on it; not supported yet", name, have.Subnet, want.Subnet))
			continue
		}
		p.Actions = append(p.Actions, networkUpdates(o.eng, name, have, want)...)
		if have.Labels[LabelProject] != o.project {
			// Made before projects: this project adopts it.
			patch := map[string]*string{LabelProject: &o.project}
			p.Actions = append(p.Actions, Action{Verb: "update", Object: "network " + name, Why: "adopted by project " + o.project, kind: kindNetwork,
				run: func(ctx context.Context) error { return o.eng.PatchNetworkLabels(ctx, name, patch) }})
		}
	}

	// VMs.
	vms, err := o.myVMs(ctx, s)
	if err != nil {
		return nil, err
	}
	o.observeGenerations(vms)
	serving, quarantined := byFunction(vms)

	for _, v := range quarantined {
		p.Kept = append(p.Kept, fmt.Sprintf("vm %s (%s): quarantined, function %s — evidence is never removed by an apply; delete it with mh rm", v.Name, v.ID, v.Labels[LabelFunction]))
	}

	// Functions no longer in the spec.
	var gone []string
	for fn := range serving {
		if _, ok := s.Functions[fn]; !ok {
			gone = append(gone, fn)
		}
	}
	sort.Strings(gone)
	for _, fn := range gone {
		for _, v := range serving[fn] {
			a := o.destroyAction(v, "function "+fn+" removed from the spec")
			a.kind = kindPrune
			p.Actions = append(p.Actions, a)
		}
	}

	for _, name := range s.FunctionOrder {
		f := s.Functions[name]
		have := serving[name]
		if f.Lifecycle.Mode != spec.ModePersistent {
			// A cycle's VM outside a running cycle is a leftover: apply
			// holds the lock, so no cycle is running.
			for _, v := range have {
				a := o.destroyAction(v, "leftover of an interrupted cycle")
				a.kind = kindLeftover
				p.Actions = append(p.Actions, a)
			}
			continue
		}
		p.Actions = append(p.Actions, o.persistentActions(name, f, have)...)
	}

	// Owned networks no longer in the spec go last, once their VMs are gone.
	for _, n := range all {
		if _, ok := s.Networks[n.Name]; ok || !o.mine(n.Labels, false) {
			continue
		}
		name := n.Name
		p.Actions = append(p.Actions, Action{Verb: "delete", Object: "network " + name, Why: "removed from the spec", kind: kindNetDelete,
			run: func(ctx context.Context) error { return o.eng.DeleteNetwork(ctx, name) }})
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return p, nil
}

// budget computes the spec's worst case and refuses it over the budget
// (docs/orchestrator.md §11): every persistent function's VM, plus one VM per
// worker for cycles. It also checks that every image is in the store.
func (o *Orchestrator) budget(ctx context.Context, s *spec.Spec, p *Plan) error {
	// Every image resolved and its defaults applied first: a function's
	// command is part of what its VM is born with (SpecHash).
	errs := o.resolveDefaults(ctx, s)
	failed := len(errs) > 0
	cycles := 0
	var cycleMem int64
	for _, name := range s.FunctionOrder {
		f := s.Functions[name]
		mem, err := o.memOf(ctx, f)
		if err != nil {
			if !failed {
				errs = append(errs, fmt.Errorf("functions.%s: %w", name, err))
			}
			continue
		}
		if f.Lifecycle.Mode == spec.ModePersistent {
			p.PeakVMs++
			p.PeakMemMB += mem
		} else {
			cycles++
			cycleMem = max(cycleMem, mem)
		}
	}
	w := min(s.Budget.Workers, cycles)
	p.PeakVMs += w
	p.PeakMemMB += int64(w) * cycleMem
	if p.PeakVMs > s.Budget.MaxVMs {
		errs = append(errs, fmt.Errorf("budget: worst case is %d VMs, budget.max_vms is %d", p.PeakVMs, s.Budget.MaxVMs))
	}
	if p.PeakMemMB > s.Budget.MaxMemMB {
		errs = append(errs, fmt.Errorf("budget: worst case is %d MB, budget.max_mem_mb is %d", p.PeakMemMB, s.Budget.MaxMemMB))
	}
	return errors.Join(errs...)
}

func (o *Orchestrator) destroyAction(v types.VMResponse, why string) Action {
	id := v.ID
	return Action{Verb: "destroy", Object: "vm " + v.Name, Why: why, kind: kindFunction,
		run: func(context.Context) error { return o.destroy(id, why) }}
}

// persistentActions converges one persistent function: exactly one running VM
// born from the current spec. Anything else of the function is destroyed —
// a VM that died, one from an older spec, a duplicate.
func (o *Orchestrator) persistentActions(name string, f *spec.Function, have []types.VMResponse) []Action {
	hash := SpecHash(f)
	var keep *types.VMResponse
	for i := range have {
		v := &have[i]
		if v.State == types.VMStateRunning && v.Labels[LabelSpec] == hash && (keep == nil || v.CreatedAt > keep.CreatedAt) {
			keep = v
		}
	}
	var acts []Action
	for _, v := range have {
		if keep != nil && v.ID == keep.ID {
			continue
		}
		why := "duplicate"
		switch {
		case v.State != types.VMStateRunning:
			why = "not running (" + string(v.State) + ")"
			if v.LastExit != nil {
				why = "died: " + v.LastExit.Reason
			}
			// A death is a failure: keep why before the VM (and its
			// console log) goes.
			a := o.destroyAction(v, why)
			destroy, dead := a.run, v
			a.run = func(ctx context.Context) error {
				born, _ := time.Parse(time.RFC3339, dead.CreatedAt)
				o.recordFailure(name, f, &dead, born, why, nil, "")
				return destroy(ctx)
			}
			acts = append(acts, a)
			continue
		case v.Labels[LabelSpec] != hash:
			why = "spec changed"
		}
		acts = append(acts, o.destroyAction(v, why))
	}
	if keep == nil {
		acts = append(acts, Action{Verb: "create", Object: "function " + name, Why: createWhy(f), function: name, kind: kindFunction,
			run: func(ctx context.Context) error {
				vm, err := o.startPersistent(ctx, name, f)
				if err != nil {
					return err
				}
				o.log.Printf("function %s: serving on vm %s (%s) %s", name, vm.Name, vm.ID, vm.GuestIP)
				return nil
			}})
		return acts
	}
	// Labels the spec sets but the VM lacks (or has with another value), or
	// the VM carries but the spec no longer sets: patched in place.
	patch := map[string]*string{}
	want := functionLabels(o.project, name, f, 0)
	for k, v := range want {
		if k == LabelGeneration {
			continue
		}
		if keep.Labels[k] != v {
			patch[k] = &v
		}
	}
	for k := range keep.Labels {
		if _, ok := want[k]; !ok {
			patch[k] = nil
		}
	}
	if len(patch) > 0 {
		id := keep.ID
		acts = append(acts, Action{Verb: "update", Object: "vm " + keep.Name, Why: "labels", kind: kindLabels,
			run: func(ctx context.Context) error { return o.eng.PatchVMLabels(ctx, id, patch) }})
	}
	return acts
}

func toEgress(rules []spec.EgressRule) []types.EgressRule {
	var out []types.EgressRule
	for _, r := range rules {
		out = append(out, types.EgressRule{Iface: r.Iface, IP: r.IP, Protocol: r.Protocol, Port: r.Port})
	}
	return out
}

func toIngress(rules []spec.IngressRule) []types.IngressRule {
	var out []types.IngressRule
	for _, r := range rules {
		out = append(out, types.IngressRule{Iface: r.Iface, SrcIP: r.SrcIP, Protocol: r.Protocol, Port: r.Port, ToIP: r.ToIP})
	}
	return out
}

func createNetworkRequest(project, name string, n *spec.Network) types.CreateNetworkRequest {
	return types.CreateNetworkRequest{
		Name:           name,
		Labels:         map[string]string{LabelManagedBy: Owner, LabelProject: project},
		Subnet:         n.Subnet,
		Egress:         n.Egress,
		EgressIface:    n.EgressIface,
		EgressPrivate:  n.EgressPrivate,
		AllowedEgress:  toEgress(n.AllowedEgress),
		AllowedIngress: toIngress(n.AllowedIngress),
		Intra:          n.Intra,
	}
}

// networkUpdates changes a live owned network's policy in place where it
// differs from the spec.
func networkUpdates(eng Engine, name string, have types.NetworkResponse, want *spec.Network) []Action {
	var acts []Action
	wantEgress := toEgress(want.AllowedEgress)
	if have.Egress != want.Egress || have.EgressIface != want.EgressIface || have.EgressPrivate != want.EgressPrivate || !sameRules(have.AllowedEgress, wantEgress) {
		req := types.UpdateNetworkEgressRequest{Egress: want.Egress, EgressIface: want.EgressIface, EgressPrivate: want.EgressPrivate, AllowedEgress: wantEgress}
		acts = append(acts, Action{Verb: "update", Object: "network " + name, Why: "egress", kind: kindNetwork,
			run: func(ctx context.Context) error { return eng.SetEgress(ctx, name, req) }})
	}
	wantIngress := toIngress(want.AllowedIngress)
	if !sameRules(have.AllowedIngress, wantIngress) {
		acts = append(acts, Action{Verb: "update", Object: "network " + name, Why: "ingress", kind: kindNetwork,
			run: func(ctx context.Context) error { return eng.SetIngress(ctx, name, wantIngress) }})
	}
	if have.Intra != want.Intra {
		intra := want.Intra
		acts = append(acts, Action{Verb: "update", Object: "network " + name, Why: fmt.Sprintf("intra %v", intra), kind: kindNetwork,
			run: func(ctx context.Context) error { return eng.SetIntra(ctx, name, intra) }})
	}
	return acts
}

// sameRules compares two rule lists as sets of rules, in any order.
func sameRules[T any](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
	for _, x := range a {
		i := slices.IndexFunc(b, func(y T) bool { return reflect.DeepEqual(x, y) })
		for i >= 0 && used[i] {
			j := slices.IndexFunc(b[i+1:], func(y T) bool { return reflect.DeepEqual(x, y) })
			if j < 0 {
				i = -1
				break
			}
			i += 1 + j
		}
		if i < 0 {
			return false
		}
		used[i] = true
	}
	return true
}

// Apply runs a plan's actions in order and stops at the first failure: what
// is done stays done, and the next apply plans from there.
func (o *Orchestrator) Apply(ctx context.Context, p *Plan) error {
	for _, a := range p.Actions {
		o.log.Printf("%s", a)
		if err := a.run(ctx); err != nil {
			return &ApplyError{Action: a, Err: err}
		}
	}
	return nil
}

// createWhy says what a function's new VM runs, and what of it the image
// declared.
func createWhy(f *spec.Function) string {
	why := f.Lifecycle.Mode + ", " + f.Image
	if len(f.FromImage) > 0 {
		why += ", " + strings.Join(f.FromImage, " and ") + " from the image"
	}
	return why
}
