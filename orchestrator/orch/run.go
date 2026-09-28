package orch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"

	"microhosted/orchestrator/engine"
	"microhosted/orchestrator/spec"
	"microhosted/pkg/types"
)

// Supervision of persistent functions.
var (
	superviseEvery = 2 * time.Second
	backoffMin     = 5 * time.Second
	backoffMax     = 5 * time.Minute
)

// degradedAfter is how many failed starts in a row stop the retries of a
// persistent function until the orchestrator is restarted.
const degradedAfter = 5

// Result is one finished cycle, written as a JSON line to the result sink.
type Result struct {
	Time       string `json:"time"`
	Function   string `json:"function"`
	Mode       string `json:"mode"`
	VM         string `json:"vm,omitempty"`
	OK         bool   `json:"ok"`
	Skipped    bool   `json:"skipped,omitempty"`
	Cause      string `json:"cause,omitempty"` // why it failed or was skipped
	Exit       *int   `json:"exit,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Output     string `json:"output,omitempty"`
}

// Run applies the spec once and then keeps it: every transaction and window
// function runs its cycles, every persistent function is watched and
// replaced when it dies or fails its health check. A spec received on reload
// becomes the new desired state, rolled out one function at a time
// (rollout.go). Run returns when ctx ends, after destroying the VMs of cycles
// still in flight.
func (o *Orchestrator) Run(ctx context.Context, reload <-chan *spec.Spec) error {
	p, err := o.Plan(ctx)
	if err != nil {
		return err
	}
	if o.state != nil {
		keep := map[string]bool{}
		for _, name := range o.spec.FunctionOrder {
			keep[name] = true
		}
		if err := o.state.Prune(keep); err != nil {
			o.log.Printf("%v", err)
		}
	}
	for _, k := range p.Kept {
		o.log.Printf("kept: %s", k)
	}
	state := map[string]*persistentState{}
	for _, name := range o.spec.FunctionOrder {
		if o.spec.Functions[name].Lifecycle.Mode == spec.ModePersistent {
			state[name] = &persistentState{}
		}
	}
	if err := o.Apply(ctx, p); err != nil {
		// What failed is a function (or a network it needs): the supervisor
		// retries with back-off, and this failure is the first of the count.
		o.log.Printf("initial apply: %v", err)
		var ae *ApplyError
		if errors.As(err, &ae) && state[ae.Action.function] != nil {
			state[ae.Action.function].failedStart(o, ae.Action.function)
		}
	}

	cs := &cycleSet{o: o, ctx: ctx, workers: make(chan struct{}, o.spec.Budget.Workers), running: map[string]chan struct{}{}}
	for _, name := range o.spec.FunctionOrder {
		cs.start(name)
	}
	o.supervise(ctx, state, cs, reload)
	cs.wg.Wait()
	return nil
}

// cycleSet runs one cycle goroutine per transaction or window function. A
// roll-out stops a function's cycles while it verifies a new version and
// starts them again after.
type cycleSet struct {
	o       *Orchestrator
	ctx     context.Context // the run's: ending it interrupts cycles in flight
	workers chan struct{}
	mu      sync.Mutex
	running map[string]chan struct{} // function → its stop channel
	stopped map[string]chan struct{} // function → closed when its goroutine ended
	wg      sync.WaitGroup
}

// start runs a function's cycles, if it has cycles and they are not running.
func (c *cycleSet) start(name string) {
	f := c.o.fn(name)
	if f == nil || f.Lifecycle.Mode == spec.ModePersistent {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[name] != nil {
		return
	}
	if c.stopped == nil {
		c.stopped = map[string]chan struct{}{}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	c.running[name], c.stopped[name] = stop, done
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(done)
		c.o.cycles(c.ctx, name, f.Lifecycle.Every.D(), c.workers, stop)
	}()
}

// stop ends a function's cycles after the one in flight, if any, finishes.
func (c *cycleSet) stop(name string) {
	c.mu.Lock()
	stop, done := c.running[name], c.stopped[name]
	delete(c.running, name)
	c.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// phase spreads functions with the same interval over it, deterministically.
func phase(name string, every time.Duration) time.Duration {
	h := fnv.New64a()
	h.Write([]byte(name))
	return time.Duration(h.Sum64() % uint64(every))
}

// cycles runs a function's cycles every interval, each with the version the
// function runs at that moment. One at a time: a tick that arrives while a
// cycle runs (or waits for a worker) is dropped — coalesced. Closing stop ends
// it between cycles; ending ctx interrupts the cycle in flight.
func (o *Orchestrator) cycles(ctx context.Context, name string, every time.Duration, workers chan struct{}, stop <-chan struct{}) {
	first := time.NewTimer(phase(name, every))
	select {
	case <-first.C:
	case <-stop:
		first.Stop()
		return
	case <-ctx.Done():
		first.Stop()
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case workers <- struct{}{}:
		case <-stop:
			return
		case <-ctx.Done():
			return
		}
		f := o.fn(name)
		if f == nil {
			<-workers
			return
		}
		r := o.cycle(ctx, name, f)
		<-workers
		o.report(r)
		select {
		case <-t.C:
		case <-stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

// cycle runs one transaction or window cycle on a fresh VM and destroys it.
func (o *Orchestrator) cycle(ctx context.Context, name string, f *spec.Function) Result {
	start := time.Now()
	r := Result{Time: start.UTC().Format(time.RFC3339), Function: name, Mode: f.Lifecycle.Mode}
	var vm *types.VMResponse
	done := func() Result {
		if !r.OK && ctx.Err() != nil {
			// Cut short by the orchestrator stopping: says nothing about
			// the function.
			r.Skipped, r.Cause = true, "interrupted: orchestrator stopping"
		}
		if !r.OK && !r.Skipped {
			// Before the deferred destroy: the console is read from the
			// engine while the VM still exists.
			o.recordFailure(name, f, vm, start, r.Cause, r.Exit, r.Output)
		}
		r.DurationMS = time.Since(start).Milliseconds()
		r.Output = tail(r.Output)
		return r
	}

	// A transaction's timeout covers the whole cycle, boot included. A
	// window's boot is bounded separately: its duration starts once the
	// command runs.
	var deadline time.Time
	if f.Lifecycle.Mode == spec.ModeTransaction {
		deadline = start.Add(f.Lifecycle.Timeout.D())
	} else {
		deadline = start.Add(readyTimeout + f.Lifecycle.Duration.D())
	}
	// The HTTP calls get a grace past the deadline, so the engine's own
	// answer at the deadline (a 504) arrives instead of a cancelled request.
	cctx, cancel := context.WithDeadline(ctx, deadline.Add(5*time.Second))
	defer cancel()

	var err error
	vm, err = o.createVM(cctx, name, f)
	if err != nil {
		vm = nil
		switch engine.StatusOf(err) {
		case http.StatusServiceUnavailable, http.StatusTooManyRequests:
			// The host (or this orchestrator's quota) is full right now:
			// not the function's failure.
			r.Skipped, r.Cause = true, "engine full: "+err.Error()
		default:
			r.Cause = "create: " + err.Error()
		}
		return done()
	}
	r.VM = vm.Name
	defer o.destroy(vm.ID, "end of cycle")

	readyBy := deadline
	if f.Lifecycle.Mode == spec.ModeWindow {
		readyBy = start.Add(readyTimeout)
	}
	if err := o.eng.WaitReady(cctx, vm.ID, time.Until(readyBy)); err != nil {
		r.Cause = "guest agent: " + err.Error()
		return done()
	}

	if f.Lifecycle.Mode == spec.ModeTransaction {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			r.Cause = fmt.Sprintf("timeout: booting took the whole %s", f.Lifecycle.Timeout.D())
			return done()
		}
		res, err := o.eng.Exec(cctx, vm.ID, f.Command, remaining)
		if err != nil {
			if engine.StatusOf(err) == http.StatusGatewayTimeout {
				r.Cause = fmt.Sprintf("timeout: still running after %s", f.Lifecycle.Timeout.D())
			} else {
				r.Cause = "exec: " + err.Error()
			}
			return done()
		}
		code := res.ExitCode
		r.Exit, r.Output = &code, res.Output
		r.OK = code == 0
		if !r.OK {
			r.Cause = fmt.Sprintf("exit %d", code)
		}
		return done()
	}

	// Window: the command runs detached for the duration; ending before it
	// is a failure.
	if err := o.launch(cctx, vm.ID, f.Command); err != nil {
		r.Cause = err.Error()
		return done()
	}
	end := time.Now().Add(f.Lifecycle.Duration.D())
	for time.Now().Before(end) {
		if err := sleep(ctx, min(pollEvery, time.Until(end))); err != nil {
			return done()
		}
		if ok, out := o.commandAlive(cctx, vm.ID); !ok {
			r.Cause = fmt.Sprintf("command ended after %s, before the %s window", time.Since(end.Add(-f.Lifecycle.Duration.D())).Round(time.Second), f.Lifecycle.Duration.D())
			r.Output = out
			return done()
		}
	}
	r.OK = true
	r.Output = o.commandOutput(cctx, vm.ID)
	return done()
}

// report writes a cycle's result to the sink and a line to the log.
func (o *Orchestrator) report(r Result) {
	if b, err := json.Marshal(r); err == nil && o.out != nil {
		fmt.Fprintf(o.out, "%s\n", b)
	}
	status := "ok"
	switch {
	case r.Skipped:
		status = "skipped: " + r.Cause
	case !r.OK:
		status = "FAILED: " + r.Cause
	}
	o.log.Printf("%s %s %s: %s in %s", r.Mode, r.Function, r.VM, status, time.Duration(r.DurationMS)*time.Millisecond)
}

// persistentState is the supervisor's memory of one persistent function.
type persistentState struct {
	healthFailures int       // consecutive failed checks of the running VM
	startFailures  int       // consecutive failed starts
	nextStart      time.Time // back-off
	lastCheck      time.Time
	degraded       bool
}

// supervise keeps every persistent function served until ctx ends, and
// rolls out each spec received on reload. Both happen in this one goroutine,
// so a roll-out never races the supervision of the function it updates.
func (o *Orchestrator) supervise(ctx context.Context, state map[string]*persistentState, cs *cycleSet, reload <-chan *spec.Spec) {
	workers := cs.workers
	t := time.NewTicker(superviseEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ns := <-reload:
			o.rollout(ctx, ns, state, cs)
			continue
		case <-t.C:
		}
		if len(state) == 0 {
			continue
		}
		vms, err := o.myVMs(ctx, o.desired())
		if err != nil {
			o.log.Printf("supervise: listing VMs: %v", err)
			continue
		}
		o.observeGenerations(vms)
		serving, _ := byFunction(vms)
		for _, name := range o.spec.FunctionOrder {
			if st := state[name]; st != nil && ctx.Err() == nil {
				if f := o.fn(name); f != nil {
					o.superviseOne(ctx, name, f, serving[name], st, workers)
				}
			}
		}
	}
}

func (o *Orchestrator) superviseOne(ctx context.Context, name string, f *spec.Function, have []types.VMResponse, st *persistentState, workers chan struct{}) {
	if st.degraded {
		return
	}
	hash := SpecHash(f)
	var cur *types.VMResponse
	for i := range have {
		v := &have[i]
		switch {
		case v.State == types.VMStateRunning && v.Labels[LabelSpec] == hash && cur == nil:
			cur = v
		default:
			why := "duplicate"
			if v.State != types.VMStateRunning {
				why = "not running (" + string(v.State) + ")"
				if v.LastExit != nil {
					why = "died: " + v.LastExit.Reason
				}
				born, _ := time.Parse(time.RFC3339, v.CreatedAt)
				o.recordFailure(name, f, v, born, why, nil, "")
			}
			o.log.Printf("function %s: vm %s %s: destroying", name, v.Name, why)
			o.destroy(v.ID, why)
		}
	}

	if cur != nil {
		if r := f.Lifecycle.Recycle.D(); r > 0 {
			if created, err := time.Parse(time.RFC3339, cur.CreatedAt); err == nil && time.Since(created) > r {
				o.log.Printf("function %s: vm %s is older than recycle %s: replacing", name, cur.Name, r)
				o.destroy(cur.ID, "recycle")
				cur = nil
			}
		}
	}

	if cur != nil {
		every, check := 10*time.Second, func() error { return nil }
		switch {
		case f.Health != nil:
			every = f.Health.Every.D()
			check = func() error { return o.checkHealth(ctx, cur.ID, f.Health) }
		case f.Command != "":
			check = func() error {
				if ok, out := o.commandAlive(ctx, cur.ID); !ok {
					return fmt.Errorf("command no longer running: %s", strings.TrimSpace(out))
				}
				return nil
			}
		}
		if time.Since(st.lastCheck) < every {
			return
		}
		st.lastCheck = time.Now()
		if err := check(); err != nil {
			st.healthFailures++
			threshold := 1
			if f.Health != nil {
				threshold = f.Health.Failures
			}
			o.log.Printf("function %s: vm %s unhealthy (%d/%d): %v", name, cur.Name, st.healthFailures, threshold, err)
			if st.healthFailures < threshold {
				return
			}
			var out string
			if f.Command != "" {
				out = o.commandOutput(ctx, cur.ID)
			}
			born, _ := time.Parse(time.RFC3339, cur.CreatedAt)
			o.recordFailure(name, f, cur, born, fmt.Sprintf("unhealthy: %d failed checks in a row, last: %v", st.healthFailures, err), nil, out)
			o.destroy(cur.ID, "unhealthy")
		} else {
			st.healthFailures = 0
			return
		}
	}

	// Not served: start a fresh VM, within the back-off and a worker.
	if time.Now().Before(st.nextStart) {
		return
	}
	if len(have) == 0 && st.startFailures == 0 {
		o.log.Printf("function %s: no VM serving it (destroyed or quarantined from outside?): starting one", name)
	}
	select {
	case workers <- struct{}{}:
	case <-ctx.Done():
		return
	}
	vm, err := o.startPersistent(ctx, name, f)
	<-workers
	st.healthFailures, st.lastCheck = 0, time.Now()
	if err != nil {
		if ctx.Err() != nil {
			return // stopping: not the function's failure
		}
		o.log.Printf("function %s: start failed (%d in a row): %v", name, st.startFailures+1, err)
		st.failedStart(o, name)
		return
	}
	st.startFailures = 0
	o.log.Printf("function %s: serving on vm %s (%s) %s", name, vm.Name, vm.ID, vm.GuestIP)
}

// failedStart counts a failed start: back-off, and degraded at the limit.
func (st *persistentState) failedStart(o *Orchestrator, name string) {
	st.startFailures++
	if st.startFailures >= degradedAfter {
		st.degraded = true
		if o.state != nil {
			if err := o.state.SetDegraded(name, true); err != nil {
				o.log.Printf("function %s: recording degraded: %v", name, err)
			}
		}
		o.log.Printf("function %s: DEGRADED after %d failed starts: no more attempts until the orchestrator restarts", name, st.startFailures)
		return
	}
	backoff := min(backoffMin<<(st.startFailures-1), backoffMax)
	st.nextStart = time.Now().Add(backoff)
	o.log.Printf("function %s: next attempt in %s", name, backoff)
}
